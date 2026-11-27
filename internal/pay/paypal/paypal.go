// Package paypal is the PayPal provider: PayPal Checkout by redirect on the REST API (Orders v2,
// intent CAPTURE). The store creates an order, sends the buyer to PayPal's approval page and captures
// the order server-side when the buyer comes back (or when the CHECKOUT.ORDER.APPROVED webhook
// arrives). No card fields, no JS SDK.
//
// Checked against these sources on 2026-11-27:
//   - OAuth 2.0 token: https://developer.paypal.com/api/rest/authentication/
//   - Orders v2 (create, capture, show): github.com/paypal/paypal-rest-api-specifications
//     openapi/checkout_orders_v2.json
//   - Payments v2 (refund): same repository, openapi/payments_payment_v2.json
//   - Webhook signature verification: same repository, openapi/notifications_webhooks_v1.json
//   - Webhook event names: https://developer.paypal.com/api/rest/webhooks/event-names/
//   - Currencies: https://developer.paypal.com/api/rest/reference/currency-codes/
//   - Error issues: https://developer.paypal.com/api/rest/reference/orders/v2/errors/
package paypal

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"wpgo/internal/pay"
)

const (
	liveOrigin = "https://api-m.paypal.com"
	testOrigin = "https://api-m.sandbox.paypal.com"
	// tokenMargin renews the access token this long before PayPal's expiry.
	tokenMargin = 5 * time.Minute
)

// Spec is the PayPal provider.
func Spec() pay.Spec {
	return pay.Spec{
		ID:          "paypal",
		Title:       "PayPal",
		Description: "PayPal Checkout: the customer pays with their PayPal account or a card on PayPal's page.",
		Region:      pay.RegionGlobal,
		Kind:        pay.KindWallet,
		Features:    []pay.Feature{pay.FeatureRefunds, pay.FeatureQuery},
		// PayPal's REST currency list. BRL, CNY and MYR work only for PayPal accounts in Brazil,
		// China and Malaysia. HUF, JPY and TWD take no decimals (pay.Decimals is 0 for them).
		Currencies: []string{"AUD", "BRL", "CAD", "CNY", "CZK", "DKK", "EUR", "HKD", "HUF", "ILS", "JPY",
			"MYR", "MXN", "TWD", "NZD", "NOK", "PHP", "PLN", "GBP", "RUB", "SGD", "SEK", "CHF", "THB", "USD"},
		Fields: []pay.Field{
			{ID: "client_id", Title: "Client ID", Type: "text", Required: true,
				Desc: "The REST app's client ID from the PayPal developer dashboard (Apps & Credentials)."},
			{ID: "client_secret", Title: "Client secret", Type: "password", Secret: true, Required: true,
				Desc: "The REST app's secret."},
			{ID: "webhook_id", Title: "Webhook ID", Type: "text",
				Desc: "Create a webhook in the PayPal developer dashboard for the notification URL shown here with the events PAYMENT.CAPTURE.COMPLETED, PAYMENT.CAPTURE.DECLINED, PAYMENT.CAPTURE.PENDING and CHECKOUT.ORDER.APPROVED, and paste its ID. Without it, PayPal notifications are refused and payments are confirmed only when the customer returns."},
		},
		DocsURL: "https://developer.paypal.com/docs/api/orders/v2/",
		New:     newProvider,
	}
}

type provider struct {
	cfg       pay.Config
	origin    string
	clientID  string
	secret    string
	webhookID string

	mu      sync.Mutex
	token   string
	expires time.Time
	now     func() time.Time
}

func newProvider(cfg pay.Config) (pay.Provider, error) {
	if err := cfg.Require("client_id", "client_secret"); err != nil {
		return nil, err
	}
	return &provider{
		cfg:       cfg,
		origin:    cfg.Origin(liveOrigin, testOrigin),
		clientID:  cfg.Value("client_id"),
		secret:    cfg.Value("client_secret"),
		webhookID: cfg.Value("webhook_id"),
		now:       time.Now,
	}, nil
}

// --- HTTP ----------------------------------------------------------------------------------------

// accessToken returns the cached OAuth token, fetching a new one when it is missing or near expiry.
func (p *provider) accessToken(ctx context.Context, fresh bool) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !fresh && p.token != "" && p.now().Before(p.expires) {
		return p.token, nil
	}
	p.token = ""
	req, err := http.NewRequest(http.MethodPost, p.origin+"/v1/oauth2/token",
		strings.NewReader(url.Values{"grant_type": {"client_credentials"}}.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(p.clientID+":"+p.secret)))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	status, body, err := pay.Do(ctx, p.cfg.HTTP, req)
	if err != nil {
		// Nothing is charged by a token request, so an unknown outcome is just a failure here.
		if errors.Is(err, pay.ErrUnknownOutcome) {
			return "", pay.Errorf(pay.CodeProviderDown, "", "PayPal token request failed")
		}
		return "", err
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return "", pay.Errorf(pay.CodeConfiguration, "invalid_client", "PayPal rejected the client ID or secret")
	}
	if status >= 500 {
		return "", pay.Errorf(pay.CodeProviderDown, fmt.Sprint(status), "PayPal token request failed")
	}
	var t struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
		Error       string `json:"error"`
	}
	if err := json.Unmarshal(body, &t); err != nil || status != http.StatusOK || t.AccessToken == "" {
		return "", pay.Errorf(pay.CodeConfiguration, t.Error, "PayPal token request failed (HTTP %d)", status)
	}
	life := time.Duration(t.ExpiresIn)*time.Second - tokenMargin
	if life < 0 {
		life = 0
	}
	p.token, p.expires = t.AccessToken, p.now().Add(life)
	return p.token, nil
}

// call sends one REST request with the bearer token; on 401 it fetches a fresh token and tries once
// more (POSTs carry a PayPal-Request-Id, so the second try is idempotent).
func (p *provider) call(ctx context.Context, method, path string, body []byte, requestID string) (int, []byte, error) {
	fresh := false
	for try := 0; ; try++ {
		tok, err := p.accessToken(ctx, fresh)
		if err != nil {
			return 0, nil, err
		}
		var rd *strings.Reader
		if body != nil {
			rd = strings.NewReader(string(body))
		} else {
			rd = strings.NewReader("")
		}
		req, err := http.NewRequest(method, p.origin+path, rd)
		if err != nil {
			return 0, nil, err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Prefer", "return=representation")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if requestID != "" {
			req.Header.Set("PayPal-Request-Id", requestID)
		}
		status, res, err := pay.Do(ctx, p.cfg.HTTP, req)
		if err == nil && status == http.StatusUnauthorized && try == 0 {
			fresh = true
			continue
		}
		if err == nil && status == http.StatusUnauthorized {
			return status, res, pay.Errorf(pay.CodeConfiguration, "AUTHENTICATION_FAILURE", "PayPal rejected the access token")
		}
		return status, res, err
	}
}

// apiError is PayPal's error body.
type apiError struct {
	Name    string `json:"name"`
	Message string `json:"message"`
	DebugID string `json:"debug_id"`
	Details []struct {
		Issue       string `json:"issue"`
		Field       string `json:"field"`
		Description string `json:"description"`
	} `json:"details"`
}

// issue is the most specific error name: the first detail's issue, else the error name.
func (e apiError) issue() string {
	if len(e.Details) > 0 && e.Details[0].Issue != "" {
		return e.Details[0].Issue
	}
	return e.Name
}

func parseError(status int, body []byte) *pay.Error {
	var e apiError
	_ = json.Unmarshal(body, &e)
	issue := e.issue()
	msg := e.Message
	if len(e.Details) > 0 && e.Details[0].Description != "" {
		msg = e.Details[0].Description
	}
	if e.DebugID != "" {
		msg = strings.TrimSpace(msg + " (debug_id " + e.DebugID + ")")
	}
	code := mapIssue(issue)
	if code == pay.CodeUnknown {
		switch {
		case status >= 500:
			code = pay.CodeProviderDown
		case status == http.StatusUnauthorized || status == http.StatusForbidden:
			code = pay.CodeConfiguration
		case status == http.StatusNotFound:
			code = pay.CodeNotFound
		}
	}
	if issue == "" {
		issue = fmt.Sprint(status)
	}
	return &pay.Error{Code: code, ProviderCode: issue, ProviderMessage: msg}
}

// addressIssues are the Orders v2 create-order issues that name the shipping address itself
// (https://developer.paypal.com/api/rest/reference/orders/v2/errors/).
var addressIssues = map[string]bool{
	"SHIPPING_ADDRESS_INVALID": true,
	"CITY_REQUIRED":            true,
	"POSTAL_CODE_REQUIRED":     true,
}

// addressRejected reports whether PayPal refused an order because of the shipping address we provided:
// an address issue, or any client error whose field points into purchase_units/…/shipping.
func addressRejected(status int, body []byte) bool {
	if status != http.StatusBadRequest && status != http.StatusUnprocessableEntity {
		return false
	}
	var e apiError
	if json.Unmarshal(body, &e) != nil {
		return false
	}
	for _, d := range e.Details {
		if addressIssues[d.Issue] || strings.Contains(d.Field, "/shipping/") {
			return true
		}
	}
	return false
}

// mapIssue maps PayPal error names and issues to pay codes.
func mapIssue(issue string) pay.Code {
	switch issue {
	case "INSTRUMENT_DECLINED", "TRANSACTION_REFUSED", "CARD_CLOSED", "DECLINED":
		return pay.CodeCardDeclined
	case "PAYER_CANNOT_PAY", "PAYER_ACCOUNT_RESTRICTED", "PAYER_ACCOUNT_LOCKED_OR_CLOSED":
		return pay.CodeCardDeclined
	case "MAX_NUMBER_OF_PAYMENT_ATTEMPTS_EXCEEDED", "TRANSACTION_LIMIT_EXCEEDED", "TRANSACTION_RECEIVING_LIMIT_EXCEEDED":
		return pay.CodeLimitExceeded
	case "COMPLIANCE_VIOLATION", "REDIRECT_PAYER_FOR_ALTERNATE_FUNDING":
		return pay.CodeFraudSuspected
	case "PAYER_ACTION_REQUIRED", "ORDER_NOT_APPROVED":
		return pay.CodeInvalidRequest
	case "PAYEE_ACCOUNT_RESTRICTED", "PAYEE_ACCOUNT_LOCKED_OR_CLOSED", "PAYEE_ACCOUNT_INVALID",
		"PAYEE_NOT_ENABLED_FOR_CARD_PROCESSING", "NOT_ENABLED_FOR_CARD_PROCESSING", "PERMISSION_DENIED", "NOT_AUTHORIZED",
		"AUTHENTICATION_FAILURE", "CURRENCY_NOT_SUPPORTED", "CURRENCY_NOT_SUPPORTED_FOR_RECEIVER",
		"CURRENCY_NOT_SUPPORTED_FOR_COUNTRY", "INVALID_CURRENCY_CODE", "PAYEE_BLOCKED_TRANSACTION":
		return pay.CodeConfiguration
	case "DUPLICATE_INVOICE_ID", "DUPLICATE_REQUEST_ID", "ORDER_ALREADY_CAPTURED", "CAPTURE_FULLY_REFUNDED":
		return pay.CodeDuplicate
	case "RESOURCE_NOT_FOUND", "INVALID_RESOURCE_ID":
		return pay.CodeNotFound
	case "ORDER_EXPIRED", "ORDER_CANNOT_BE_SAVED":
		return pay.CodeTimeout
	case "INVALID_REQUEST", "UNPROCESSABLE_ENTITY", "INVALID_PARAMETER_VALUE", "MISSING_REQUIRED_PARAMETER",
		"INVALID_STRING_LENGTH", "DECIMAL_PRECISION", "AMOUNT_MISMATCH", "ITEM_TOTAL_MISMATCH",
		"REFUND_AMOUNT_EXCEEDED", "REFUND_TIME_LIMIT_EXCEEDED", "PARTIAL_REFUND_NOT_ALLOWED", "MALFORMED_REQUEST":
		return pay.CodeInvalidRequest
	case "INTERNAL_SERVER_ERROR", "INTERNAL_SERVICE_ERROR", "SERVICE_UNAVAILABLE":
		return pay.CodeProviderDown
	}
	return pay.CodeUnknown
}

// --- Start ---------------------------------------------------------------------------------------

type money struct {
	CurrencyCode string `json:"currency_code"`
	Value        string `json:"value"`
}

type orderItem struct {
	Name       string `json:"name"`
	Quantity   string `json:"quantity"`
	UnitAmount money  `json:"unit_amount"`
	SKU        string `json:"sku,omitempty"`
	Category   string `json:"category,omitempty"`
}

type amountWithBreakdown struct {
	money
	Breakdown *struct {
		ItemTotal money `json:"item_total"`
	} `json:"breakdown,omitempty"`
}

type shippingAddress struct {
	Line1       string `json:"address_line_1,omitempty"`
	Line2       string `json:"address_line_2,omitempty"`
	AdminArea2  string `json:"admin_area_2,omitempty"`
	AdminArea1  string `json:"admin_area_1,omitempty"`
	PostalCode  string `json:"postal_code,omitempty"`
	CountryCode string `json:"country_code"`
}

type shipping struct {
	Name *struct {
		FullName string `json:"full_name"`
	} `json:"name,omitempty"`
	Address shippingAddress `json:"address"`
}

type purchaseUnitReq struct {
	ReferenceID string              `json:"reference_id"`
	CustomID    string              `json:"custom_id"`
	InvoiceID   string              `json:"invoice_id"`
	Description string              `json:"description,omitempty"`
	Amount      amountWithBreakdown `json:"amount"`
	Items       []orderItem         `json:"items,omitempty"`
	Shipping    *shipping           `json:"shipping,omitempty"`
}

type experienceContext struct {
	ReturnURL          string `json:"return_url"`
	CancelURL          string `json:"cancel_url"`
	UserAction         string `json:"user_action"`
	ShippingPreference string `json:"shipping_preference"`
	Locale             string `json:"locale,omitempty"`
}

type orderReq struct {
	Intent        string            `json:"intent"`
	PurchaseUnits []purchaseUnitReq `json:"purchase_units"`
	PaymentSource struct {
		PayPal struct {
			ExperienceContext experienceContext `json:"experience_context"`
		} `json:"paypal"`
	} `json:"payment_source"`
}

// addressRetrySuffix marks the PayPal-Request-Id of the create-order retry without the address.
const addressRetrySuffix = "-noaddr"

func (p *provider) Start(ctx context.Context, r pay.StartRequest) (*pay.StartResult, error) {
	cur := strings.ToUpper(r.Currency)
	pu := purchaseUnitReq{
		ReferenceID: r.Ref,
		CustomID:    r.Ref,
		InvoiceID:   r.Ref,
		Description: pay.Trunc(r.Description, 127),
		Amount:      amountWithBreakdown{money: money{cur, pay.FormatMinor(r.Amount, cur)}},
	}
	if items, ok := breakdown(r.Items, r.Amount, cur); ok {
		pu.Items = items
		pu.Amount.Breakdown = &struct {
			ItemTotal money `json:"item_total"`
		}{money{cur, pay.FormatMinor(r.Amount, cur)}}
	}
	pref := "GET_FROM_FILE"
	switch {
	case allVirtual(r.Items):
		pref = "NO_SHIPPING"
	case r.Shipping.Line1 != "" && len(r.Shipping.Country) == 2:
		pref = "SET_PROVIDED_ADDRESS"
		s := &shipping{Address: shippingAddress{
			Line1:       pay.Trunc(r.Shipping.Line1, 300),
			Line2:       pay.Trunc(r.Shipping.Line2, 300),
			AdminArea2:  pay.Trunc(r.Shipping.City, 120),
			AdminArea1:  pay.Trunc(r.Shipping.State, 300),
			PostalCode:  pay.Trunc(r.Shipping.Postcode, 60),
			CountryCode: strings.ToUpper(r.Shipping.Country),
		}}
		if r.Shipping.Name != "" {
			s.Name = &struct {
				FullName string `json:"full_name"`
			}{pay.Trunc(r.Shipping.Name, 300)}
		}
		pu.Shipping = s
	}
	var o orderReq
	o.Intent = "CAPTURE"
	o.PurchaseUnits = []purchaseUnitReq{pu}
	o.PaymentSource.PayPal.ExperienceContext = experienceContext{
		ReturnURL:          r.ReturnURL,
		CancelURL:          r.FailURL,
		UserAction:         "PAY_NOW",
		ShippingPreference: pref,
		Locale:             locale(r.Locale),
	}
	body, err := json.Marshal(o)
	if err != nil {
		return nil, err
	}
	status, res, err := p.call(ctx, http.MethodPost, "/v2/checkout/orders", body, r.Ref)
	if err != nil {
		return nil, err
	}
	if pu.Shipping != nil && addressRejected(status, res) {
		// A store address PayPal will not accept must not cost the sale: try once more and let the
		// buyer pick an address from their PayPal account. The first request id is bound to the
		// rejected payload, so the retry needs its own; the order refs stay r.Ref.
		o.PurchaseUnits[0].Shipping = nil
		o.PaymentSource.PayPal.ExperienceContext.ShippingPreference = "GET_FROM_FILE"
		if body, err = json.Marshal(o); err != nil {
			return nil, err
		}
		status, res, err = p.call(ctx, http.MethodPost, "/v2/checkout/orders", body, r.Ref+addressRetrySuffix)
		if err != nil {
			return nil, err
		}
	}
	if status != http.StatusOK && status != http.StatusCreated {
		return nil, parseError(status, res)
	}
	var out order
	if err := json.Unmarshal(res, &out); err != nil || out.ID == "" {
		return nil, pay.Errorf(pay.CodeUnknown, "", "unreadable PayPal order response")
	}
	link := out.link("payer-action")
	if link == "" {
		link = out.link("approve")
	}
	if !paypalURL(link) {
		return nil, pay.Errorf(pay.CodeUnknown, "", "PayPal returned no usable approval link")
	}
	return &pay.StartResult{Redirect: link, ProviderRef: out.ID}, nil
}

// breakdown is the item list, or false when it does not sum exactly to the amount (PayPal rejects an
// order whose item_total differs from the items).
func breakdown(in []pay.Item, amount int64, cur string) ([]orderItem, bool) {
	if len(in) == 0 {
		return nil, false
	}
	var sum int64
	out := make([]orderItem, 0, len(in))
	for _, it := range in {
		if it.Quantity <= 0 || it.UnitPrice < 0 || strings.TrimSpace(it.Name) == "" {
			return nil, false
		}
		sum += int64(it.Quantity) * it.UnitPrice
		cat := "PHYSICAL_GOODS"
		if it.Virtual {
			cat = "DIGITAL_GOODS"
		}
		out = append(out, orderItem{
			Name:       pay.Trunc(it.Name, 127),
			Quantity:   fmt.Sprint(it.Quantity),
			UnitAmount: money{cur, pay.FormatMinor(it.UnitPrice, cur)},
			SKU:        pay.Trunc(it.ID, 127),
			Category:   cat,
		})
	}
	return out, sum == amount
}

func allVirtual(items []pay.Item) bool {
	for _, it := range items {
		if !it.Virtual {
			return false
		}
	}
	return len(items) > 0
}

func locale(l string) string {
	switch strings.ToLower(l) {
	case "tr":
		return "tr-TR"
	case "en":
		return "en-US"
	}
	return ""
}

// paypalURL accepts only https links on paypal.com or one of its subdomains.
func paypalURL(s string) bool {
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "https" || u.User != nil {
		return false
	}
	h := strings.ToLower(u.Hostname())
	return h == "paypal.com" || strings.HasSuffix(h, ".paypal.com")
}

// --- Orders --------------------------------------------------------------------------------------

type capture struct {
	ID            string `json:"id"`
	Status        string `json:"status"`
	Amount        money  `json:"amount"`
	CustomID      string `json:"custom_id"`
	InvoiceID     string `json:"invoice_id"`
	StatusDetails struct {
		Reason string `json:"reason"`
	} `json:"status_details"`
}

type order struct {
	ID            string `json:"id"`
	Status        string `json:"status"`
	PurchaseUnits []struct {
		ReferenceID string `json:"reference_id"`
		CustomID    string `json:"custom_id"`
		Payments    struct {
			Captures []capture `json:"captures"`
		} `json:"payments"`
	} `json:"purchase_units"`
	Links []struct {
		Href string `json:"href"`
		Rel  string `json:"rel"`
	} `json:"links"`
}

func (o *order) link(rel string) string {
	for _, l := range o.Links {
		if l.Rel == rel {
			return l.Href
		}
	}
	return ""
}

// unitRef is the attempt reference the order's purchase unit carries.
func (o *order) unitRef() string {
	if len(o.PurchaseUnits) == 0 {
		return ""
	}
	if o.PurchaseUnits[0].CustomID != "" {
		return o.PurchaseUnits[0].CustomID
	}
	return o.PurchaseUnits[0].ReferenceID
}

func (o *order) capture() *capture {
	if len(o.PurchaseUnits) == 0 || len(o.PurchaseUnits[0].Payments.Captures) == 0 {
		return nil
	}
	return &o.PurchaseUnits[0].Payments.Captures[0]
}

func (p *provider) getOrder(ctx context.Context, id string) (*order, error) {
	status, res, err := p.call(ctx, http.MethodGet, "/v2/checkout/orders/"+url.PathEscape(id), nil, "")
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, parseError(status, res)
	}
	var o order
	if err := json.Unmarshal(res, &o); err != nil || o.ID == "" {
		return nil, errors.New("paypal: unreadable order response")
	}
	return &o, nil
}

// captureOrder captures the order, or reads it when it was captured already, and reports the outcome.
func (p *provider) captureOrder(ctx context.Context, a pay.Attempt) (*pay.Result, error) {
	path := "/v2/checkout/orders/" + url.PathEscape(a.ProviderRef) + "/capture"
	status, res, err := p.call(ctx, http.MethodPost, path, []byte("{}"), "cap"+a.Ref)
	if err != nil {
		return nil, err
	}
	var o *order
	switch {
	case status == http.StatusOK || status == http.StatusCreated:
		o = &order{}
		if err := json.Unmarshal(res, o); err != nil || o.ID == "" {
			return nil, errors.New("paypal: unreadable capture response")
		}
	case status == http.StatusAccepted:
		// 202: PayPal accepted the capture and finishes it asynchronously; the outcome comes from a
		// webhook or a later Query.
		return &pay.Result{Status: pay.StatusPending, ProviderCode: "202"}, nil
	case status == http.StatusUnprocessableEntity:
		e := parseError(status, res)
		switch e.ProviderCode {
		case "ORDER_ALREADY_CAPTURED":
			if o, err = p.getOrder(ctx, a.ProviderRef); err != nil {
				return nil, err
			}
		case "ORDER_NOT_APPROVED", "PAYER_ACTION_REQUIRED":
			return &pay.Result{Status: pay.StatusPending, ProviderCode: e.ProviderCode, ProviderMessage: e.ProviderMessage}, nil
		default:
			return &pay.Result{Status: pay.StatusFailure, Code: e.Code, ProviderCode: e.ProviderCode, ProviderMessage: e.ProviderMessage}, nil
		}
	default:
		e := parseError(status, res)
		if status >= 500 {
			return nil, e
		}
		return &pay.Result{Status: pay.StatusFailure, Code: e.Code, ProviderCode: e.ProviderCode, ProviderMessage: e.ProviderMessage}, nil
	}
	if o.ID != a.ProviderRef {
		return nil, errors.New("paypal: order id does not match the attempt")
	}
	return orderResult(o, a, false)
}

// orderResult reads an order's outcome. query: report refunds and voids as cancelled.
func orderResult(o *order, a pay.Attempt, query bool) (*pay.Result, error) {
	if !pay.Equal(o.unitRef(), a.Ref) {
		return nil, errors.New("paypal: order is not about this attempt")
	}
	c := o.capture()
	switch {
	case o.Status == "VOIDED":
		return &pay.Result{Status: pay.StatusCancelled, ProviderCode: o.Status}, nil
	case o.Status == "COMPLETED" && c != nil:
		return captureResult(c, query)
	case o.Status == "COMPLETED":
		return &pay.Result{Status: pay.StatusPending, ProviderCode: o.Status}, nil
	}
	// CREATED, SAVED, APPROVED, PAYER_ACTION_REQUIRED: the buyer has not paid yet.
	return &pay.Result{Status: pay.StatusPending, ProviderCode: o.Status}, nil
}

func captureResult(c *capture, query bool) (*pay.Result, error) {
	switch c.Status {
	case "COMPLETED", "PARTIALLY_REFUNDED":
		amt, err := pay.ParseMinor(c.Amount.Value, c.Amount.CurrencyCode)
		if err != nil {
			return nil, err
		}
		return &pay.Result{Status: pay.StatusSuccess, TxnID: c.ID, PaidAmount: amt,
			Currency: strings.ToUpper(c.Amount.CurrencyCode), ProviderCode: c.Status}, nil
	case "PENDING":
		return &pay.Result{Status: pay.StatusPending, TxnID: c.ID, ProviderCode: c.Status,
			ProviderMessage: c.StatusDetails.Reason}, nil
	case "REFUNDED":
		if query {
			return &pay.Result{Status: pay.StatusCancelled, TxnID: c.ID, ProviderCode: c.Status}, nil
		}
	case "DECLINED", "FAILED":
		return &pay.Result{Status: pay.StatusFailure, TxnID: c.ID, Code: pay.CodeCardDeclined,
			ProviderCode: c.Status, ProviderMessage: c.StatusDetails.Reason}, nil
	}
	return &pay.Result{Status: pay.StatusFailure, TxnID: c.ID, Code: pay.CodeUnknown, ProviderCode: c.Status,
		ProviderMessage: c.StatusDetails.Reason}, nil
}

// --- Callbacks -----------------------------------------------------------------------------------

type webhookEvent struct {
	EventType string          `json:"event_type"`
	Resource  json.RawMessage `json:"resource"`
}

func (p *provider) CallbackRef(in pay.CallbackInput) string {
	if in.Kind != pay.CallbackNotify {
		return ""
	}
	var ev webhookEvent
	if json.Unmarshal(in.Body, &ev) != nil {
		return ""
	}
	if strings.HasPrefix(ev.EventType, "PAYMENT.CAPTURE.") {
		var c capture
		if json.Unmarshal(ev.Resource, &c) == nil {
			return c.CustomID
		}
		return ""
	}
	if ev.EventType == "CHECKOUT.ORDER.APPROVED" {
		var o order
		if json.Unmarshal(ev.Resource, &o) == nil {
			return o.unitRef()
		}
	}
	return ""
}

func (p *provider) Complete(ctx context.Context, in pay.CallbackInput, a pay.Attempt) (*pay.Result, error) {
	switch in.Kind {
	case pay.CallbackFail:
		return &pay.Result{Status: pay.StatusFailure, Code: pay.CodeCancelledByUser}, nil
	case pay.CallbackReturn:
		if !pay.Equal(in.Query.Get("token"), a.ProviderRef) {
			return nil, errors.New("paypal: return token does not match the attempt")
		}
		return p.captureOrder(ctx, a)
	case pay.CallbackNotify:
		return p.notify(ctx, in, a)
	}
	return nil, fmt.Errorf("paypal: unknown callback kind %q", in.Kind)
}

func (p *provider) notify(ctx context.Context, in pay.CallbackInput, a pay.Attempt) (*pay.Result, error) {
	if p.webhookID == "" {
		return nil, errors.New("paypal: webhook_id is not configured, notification refused")
	}
	if !json.Valid(in.Body) {
		return nil, errors.New("paypal: notification body is not JSON")
	}
	if err := p.verifyWebhook(ctx, in); err != nil {
		return nil, err
	}
	var ev webhookEvent
	if err := json.Unmarshal(in.Body, &ev); err != nil {
		return nil, err
	}
	ack := &pay.Ack{Status: http.StatusOK}
	var r *pay.Result
	switch ev.EventType {
	case "PAYMENT.CAPTURE.COMPLETED", "PAYMENT.CAPTURE.PENDING", "PAYMENT.CAPTURE.DENIED", "PAYMENT.CAPTURE.DECLINED":
		var c capture
		if err := json.Unmarshal(ev.Resource, &c); err != nil {
			return nil, err
		}
		if !pay.Equal(c.CustomID, a.Ref) {
			return nil, errors.New("paypal: capture is not about this attempt")
		}
		if ev.EventType == "PAYMENT.CAPTURE.PENDING" {
			r = &pay.Result{Status: pay.StatusPending, TxnID: c.ID, ProviderCode: c.Status,
				ProviderMessage: c.StatusDetails.Reason}
		} else if ev.EventType == "PAYMENT.CAPTURE.COMPLETED" {
			if c.Status != "COMPLETED" {
				r = &pay.Result{Status: pay.StatusPending, TxnID: c.ID, ProviderCode: c.Status}
				break
			}
			var err error
			if r, err = captureResult(&c, false); err != nil {
				return nil, err
			}
		} else {
			r = &pay.Result{Status: pay.StatusFailure, Code: pay.CodeCardDeclined, TxnID: c.ID,
				ProviderCode: ev.EventType, ProviderMessage: c.StatusDetails.Reason}
		}
	case "CHECKOUT.ORDER.APPROVED":
		var o order
		if err := json.Unmarshal(ev.Resource, &o); err != nil {
			return nil, err
		}
		if !pay.Equal(o.ID, a.ProviderRef) || !pay.Equal(o.unitRef(), a.Ref) {
			return nil, errors.New("paypal: order is not about this attempt")
		}
		var err error
		if r, err = p.captureOrder(ctx, a); err != nil {
			return nil, err
		}
	default:
		r = &pay.Result{Status: pay.StatusPending, ProviderCode: ev.EventType}
	}
	r.Ack = ack
	return r, nil
}

// verifyWebhook asks PayPal whether the notification is genuine. The raw body goes into webhook_event
// byte for byte: PayPal verifies the signature over the body as it sent it, and re-marshalling could
// change it.
func (p *provider) verifyWebhook(ctx context.Context, in pay.CallbackInput) error {
	h := func(k string) string { return in.Header.Get(k) }
	head, err := json.Marshal(map[string]string{
		"auth_algo":         h("Paypal-Auth-Algo"),
		"cert_url":          h("Paypal-Cert-Url"),
		"transmission_id":   h("Paypal-Transmission-Id"),
		"transmission_sig":  h("Paypal-Transmission-Sig"),
		"transmission_time": h("Paypal-Transmission-Time"),
		"webhook_id":        p.webhookID,
	})
	if err != nil {
		return err
	}
	body := make([]byte, 0, len(head)+len(in.Body)+20)
	body = append(body, head[:len(head)-1]...)
	body = append(body, `,"webhook_event":`...)
	body = append(body, in.Body...)
	body = append(body, '}')
	status, res, err := p.call(ctx, http.MethodPost, "/v1/notifications/verify-webhook-signature", body, "")
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("%w: verification request failed (%s)", pay.ErrInvalidSignature, parseError(status, res).ProviderCode)
	}
	var v struct {
		VerificationStatus string `json:"verification_status"`
	}
	if json.Unmarshal(res, &v) != nil || v.VerificationStatus != "SUCCESS" {
		return pay.ErrInvalidSignature
	}
	return nil
}

// --- Query and refund ----------------------------------------------------------------------------

func (p *provider) Query(ctx context.Context, a pay.Attempt) (*pay.Result, error) {
	if a.ProviderRef == "" {
		return nil, pay.Errorf(pay.CodeNotFound, "", "no PayPal order id for this attempt")
	}
	o, err := p.getOrder(ctx, a.ProviderRef)
	if err != nil {
		return nil, err
	}
	return orderResult(o, a, true)
}

func (p *provider) Refund(ctx context.Context, r pay.RefundRequest) (*pay.Result, error) {
	if r.Attempt.TxnID == "" {
		return nil, pay.Errorf(pay.CodeNotFound, "", "no PayPal capture id for this payment")
	}
	req := map[string]any{}
	if !r.Full {
		cur := strings.ToUpper(r.Attempt.Currency)
		req["amount"] = money{cur, pay.FormatMinor(r.Amount, cur)}
	}
	if r.Reason != "" {
		req["note_to_payer"] = pay.Trunc(r.Reason, 255)
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	path := "/v2/payments/captures/" + url.PathEscape(r.Attempt.TxnID) + "/refund"
	status, res, err := p.call(ctx, http.MethodPost, path, body, r.RefundRef)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK && status != http.StatusCreated {
		if status >= 500 {
			return nil, fmt.Errorf("%w: PayPal answered HTTP %d", pay.ErrUnknownOutcome, status)
		}
		e := parseError(status, res)
		return &pay.Result{Status: pay.StatusFailure, Code: e.Code, ProviderCode: e.ProviderCode, ProviderMessage: e.ProviderMessage}, nil
	}
	var out struct {
		ID            string `json:"id"`
		Status        string `json:"status"`
		Amount        money  `json:"amount"`
		StatusDetails struct {
			Reason string `json:"reason"`
		} `json:"status_details"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return nil, fmt.Errorf("%w: unreadable refund response", pay.ErrUnknownOutcome)
	}
	switch out.Status {
	case "COMPLETED":
		amt := r.Amount
		if out.Amount.Value != "" {
			if amt, err = pay.ParseMinor(out.Amount.Value, out.Amount.CurrencyCode); err != nil {
				return nil, err
			}
		}
		return &pay.Result{Status: pay.StatusSuccess, TxnID: out.ID, PaidAmount: amt,
			Currency: strings.ToUpper(out.Amount.CurrencyCode), ProviderCode: out.Status}, nil
	case "PENDING":
		return &pay.Result{Status: pay.StatusPending, TxnID: out.ID, ProviderCode: out.Status,
			ProviderMessage: out.StatusDetails.Reason}, nil
	case "CANCELLED", "FAILED":
		return &pay.Result{Status: pay.StatusFailure, Code: pay.CodeUnknown, TxnID: out.ID,
			ProviderCode: out.Status, ProviderMessage: out.StatusDetails.Reason}, nil
	}
	return nil, fmt.Errorf("%w: refund status %q", pay.ErrUnknownOutcome, out.Status)
}
