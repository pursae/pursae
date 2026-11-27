// Package iyzico is the iyzico provider of Pursae Commerce: the hosted Checkout Form. Start initializes
// a form and sends the customer to iyzico's payment page; iyzico posts a token back to the return URL
// and Complete retrieves the result server-side with that token, checking iyzico's response signature.
//
// Protocol: iyzico API documentation (docs.iyzico.com: Checkout Form initialize / retrieve, IYZWSv2
// authorization, response signature validation, refund and cancel), with better-payment's iyzico
// provider as the reference for request shapes.
package iyzico

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"wpgo/internal/pay"
)

const (
	liveOrigin = "https://api.iyzipay.com"
	testOrigin = "https://sandbox-api.iyzipay.com"

	pathInitialize = "/payment/iyzipos/checkoutform/initialize/auth/ecom"
	pathRetrieve   = "/payment/iyzipos/checkoutform/auth/ecom/detail"
	pathCancel     = "/payment/cancel"
	pathRefund     = "/v2/payment/refund"

	// defaultIdentityNumber is the placeholder TCKN iyzico's own e-commerce plugins send when the
	// store does not collect one: iyzico requires buyer.identityNumber but accepts this value.
	defaultIdentityNumber = "11111111111"
)

// Spec describes the iyzico provider.
func Spec() pay.Spec {
	return pay.Spec{
		ID:          "iyzico",
		Title:       "iyzico",
		Description: "Card payments and installments on iyzico's hosted payment page.",
		Region:      pay.RegionTR,
		Kind:        pay.KindPSP,
		Features:    []pay.Feature{pay.Feature3DS, pay.FeatureInstallments, pay.FeatureRefunds, pay.FeatureQuery},
		Currencies:  []string{"TRY", "USD", "EUR", "GBP", "NOK", "CHF"},
		Fields: []pay.Field{
			{ID: "api_key", Title: "API key", Type: "text", Required: true},
			{ID: "secret_key", Title: "Secret key", Type: "password", Secret: true, Required: true},
		},
		DocsURL: "https://docs.iyzico.com/en/payment-methods/checkoutform",
		New:     newProvider,
	}
}

type provider struct {
	apiKey, secretKey string
	origin            string
	http              *http.Client
	// rnd makes the x-iyzi-rnd value; now is the clock for the same-day cancel rule. Tests replace both.
	rnd func() string
	now func() time.Time
}

func newProvider(c pay.Config) (pay.Provider, error) {
	if err := c.Require("api_key", "secret_key"); err != nil {
		return nil, err
	}
	return &provider{
		apiKey:    c.Value("api_key"),
		secretKey: c.Value("secret_key"),
		origin:    c.Origin(liveOrigin, testOrigin),
		http:      c.HTTP,
		rnd:       randomKey,
		now:       time.Now,
	}, nil
}

// randomKey is the per-request random string of IYZWSv2: a millisecond timestamp and random hex, the
// same shape as iyzico's SDKs.
func randomKey() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return strconv.FormatInt(time.Now().UnixMilli(), 10) + hex.EncodeToString(b)
}

// authorization is the IYZWSv2 Authorization header: Base64 of "apiKey:…&randomKey:…&signature:…",
// where signature is the hex HMAC-SHA256 (secret key) of randomKey + URI path + exact JSON body.
func authorization(apiKey, secretKey, rnd, path string, body []byte) string {
	sig := hex.EncodeToString(pay.HMACSHA256([]byte(secretKey), []byte(rnd+path+string(body))))
	params := "apiKey:" + apiKey + "&randomKey:" + rnd + "&signature:" + sig
	return "IYZWSv2 " + base64.StdEncoding.EncodeToString([]byte(params))
}

// responseSignature is iyzico's response signature: hex HMAC-SHA256 (secret key) of the documented
// fields joined with ":".
func responseSignature(secretKey string, fields ...string) string {
	return hex.EncodeToString(pay.HMACSHA256([]byte(secretKey), []byte(strings.Join(fields, ":"))))
}

// apiResponse is the common part of every iyzico response.
type apiResponse struct {
	Status         string `json:"status"`
	ErrorCode      string `json:"errorCode"`
	ErrorMessage   string `json:"errorMessage"`
	ConversationID string `json:"conversationId"`
	Signature      string `json:"signature"`
}

// decimal reads an iyzico amount, sent as a JSON number (or, defensively, a string), keeping its text
// so the response signature is computed over what iyzico sent.
type decimal string

func (d *decimal) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "null" {
		*d = ""
		return nil
	}
	if strings.HasPrefix(s, `"`) {
		var v string
		if err := json.Unmarshal(b, &v); err != nil {
			return err
		}
		*d = decimal(v)
		return nil
	}
	*d = decimal(s)
	return nil
}

// sigText is the amount as the signature takes it: trailing fractional zeros removed ("50.00" → "50",
// "10.50" → "10.5").
func (d decimal) sigText() string {
	s := string(d)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(s, "0")
		s = strings.TrimSuffix(s, ".")
	}
	return s
}

// post sends a signed JSON request and decodes the response into out. An undecodable answer is an error;
// iyzico answers failures with a JSON body (sometimes with a 4xx status), which out receives.
func (p *provider) post(ctx context.Context, path string, req, out any) error {
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	rnd := p.rnd()
	h := http.Header{}
	h.Set("Accept", "application/json")
	h.Set("Authorization", authorization(p.apiKey, p.secretKey, rnd, path, body))
	h.Set("x-iyzi-rnd", rnd)
	status, resp, err := pay.PostBody(ctx, p.http, p.origin+path, "application/json", body, h)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(resp, out); err != nil {
		if status >= 500 {
			return pay.Errorf(pay.CodeProviderDown, strconv.Itoa(status), "iyzico HTTP %d", status)
		}
		return fmt.Errorf("iyzico: unreadable response (HTTP %d)", status)
	}
	return nil
}

func providerError(r apiResponse) *pay.Error {
	return &pay.Error{Code: mapCode(r.ErrorCode), ProviderCode: r.ErrorCode, ProviderMessage: r.ErrorMessage}
}

// Request bodies. Amounts are decimal text, as iyzico's SDKs send them.

type initRequest struct {
	Locale              string       `json:"locale"`
	ConversationID      string       `json:"conversationId"`
	Price               string       `json:"price"`
	PaidPrice           string       `json:"paidPrice"`
	Currency            string       `json:"currency"`
	BasketID            string       `json:"basketId"`
	PaymentGroup        string       `json:"paymentGroup"`
	CallbackURL         string       `json:"callbackUrl"`
	EnabledInstallments []int        `json:"enabledInstallments"`
	Buyer               buyer        `json:"buyer"`
	ShippingAddress     address      `json:"shippingAddress"`
	BillingAddress      address      `json:"billingAddress"`
	BasketItems         []basketItem `json:"basketItems"`
}

type buyer struct {
	ID                  string `json:"id"`
	Name                string `json:"name"`
	Surname             string `json:"surname"`
	GSMNumber           string `json:"gsmNumber,omitempty"`
	Email               string `json:"email"`
	IdentityNumber      string `json:"identityNumber"`
	RegistrationAddress string `json:"registrationAddress"`
	IP                  string `json:"ip,omitempty"`
	City                string `json:"city"`
	Country             string `json:"country"`
	ZipCode             string `json:"zipCode,omitempty"`
}

type address struct {
	ContactName string `json:"contactName"`
	City        string `json:"city"`
	Country     string `json:"country"`
	Address     string `json:"address"`
	ZipCode     string `json:"zipCode,omitempty"`
}

type basketItem struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Category1 string `json:"category1"`
	ItemType  string `json:"itemType"`
	Price     string `json:"price"`
}

type initResponse struct {
	apiResponse
	Token          string `json:"token"`
	PaymentPageURL string `json:"paymentPageUrl"`
}

func locale(l string) string {
	if strings.EqualFold(l, "en") {
		return "en"
	}
	return "tr"
}

// countryName turns an ISO code into the English country name iyzico's examples and plugins use for
// Türkiye; other codes are sent as given.
func countryName(code string) string {
	switch strings.ToUpper(code) {
	case "", "TR":
		return "Turkey"
	}
	return code
}

func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

func mapAddress(a pay.Address) address {
	return address{
		ContactName: orDefault(a.Name, "-"),
		City:        orDefault(a.City, orDefault(a.State, "-")),
		Country:     countryName(a.Country),
		Address:     orDefault(strings.TrimSpace(a.Line1+" "+a.Line2), "-"),
		ZipCode:     a.Postcode,
	}
}

// installments is the list the hosted page may offer: 1 a single payment, 0 iyzico's usual plans.
func installments(max int) []int {
	all := []int{1, 2, 3, 6, 9, 12}
	if max <= 0 {
		return all
	}
	var out []int
	for _, n := range all {
		if n <= max {
			out = append(out, n)
		}
	}
	return out
}

func (p *provider) Start(ctx context.Context, r pay.StartRequest) (*pay.StartResult, error) {
	var sum int64
	var items []basketItem
	for i, it := range r.Items {
		line := int64(it.Quantity) * it.UnitPrice
		sum += line
		if line <= 0 {
			continue // iyzico rejects items priced 0; they do not change the total
		}
		typ := "PHYSICAL"
		if it.Virtual {
			typ = "VIRTUAL"
		}
		items = append(items, basketItem{
			ID:        orDefault(it.ID, strconv.Itoa(i+1)),
			Name:      orDefault(it.Name, "Item"),
			Category1: orDefault(it.Category, "General"),
			ItemType:  typ,
			Price:     pay.FormatMinor(line, r.Currency),
		})
	}
	if len(items) == 0 {
		return nil, pay.Errorf(pay.CodeInvalidRequest, "", "empty basket")
	}
	c := r.Customer
	first, last := c.FirstName, c.LastName
	if first == "" && last == "" {
		first, last, _ = strings.Cut(strings.TrimSpace(r.Billing.Name), " ")
	}
	buyerID := c.ID
	if buyerID == "" {
		buyerID = "guest-" + r.Ref
	}
	req := initRequest{
		Locale:              locale(r.Locale),
		ConversationID:      r.Ref,
		Price:               pay.FormatMinor(sum, r.Currency),
		PaidPrice:           pay.FormatMinor(r.Amount, r.Currency),
		Currency:            r.Currency,
		BasketID:            r.Ref,
		PaymentGroup:        "PRODUCT",
		CallbackURL:         r.ReturnURL,
		EnabledInstallments: installments(r.MaxInstallment),
		Buyer: buyer{
			ID:                  buyerID,
			Name:                orDefault(first, "-"),
			Surname:             orDefault(last, "-"),
			GSMNumber:           c.Phone,
			Email:               c.Email,
			IdentityNumber:      defaultIdentityNumber,
			RegistrationAddress: orDefault(strings.TrimSpace(r.Billing.Line1+" "+r.Billing.Line2), "-"),
			IP:                  c.IP,
			City:                orDefault(r.Billing.City, orDefault(r.Billing.State, "-")),
			Country:             countryName(r.Billing.Country),
			ZipCode:             r.Billing.Postcode,
		},
		ShippingAddress: mapAddress(r.Shipping),
		BillingAddress:  mapAddress(r.Billing),
		BasketItems:     items,
	}
	var res initResponse
	if err := p.post(ctx, pathInitialize, req, &res); err != nil {
		return nil, err
	}
	if res.Status != "success" {
		return nil, providerError(res.apiResponse)
	}
	if res.Token == "" || res.PaymentPageURL == "" || !strings.HasPrefix(res.PaymentPageURL, "https://") {
		return nil, errors.New("iyzico: initialize response has no payment page URL")
	}
	if !pay.Equal(res.Signature, responseSignature(p.secretKey, res.ConversationID, res.Token)) {
		return nil, fmt.Errorf("iyzico: initialize response: %w", pay.ErrInvalidSignature)
	}
	if res.ConversationID != r.Ref {
		return nil, errors.New("iyzico: initialize response is for another conversation")
	}
	return &pay.StartResult{Redirect: res.PaymentPageURL, ProviderRef: res.Token}, nil
}

// CallbackRef returns "": iyzico posts only the token to the callback URL, which names no attempt
// reference. The bridge finds the attempt from its own routing parameter in the URL, and Complete
// checks the token against the attempt.
func (p *provider) CallbackRef(in pay.CallbackInput) string { return "" }

type retrieveRequest struct {
	Locale         string `json:"locale"`
	ConversationID string `json:"conversationId"`
	Token          string `json:"token"`
}

type retrieveResponse struct {
	apiResponse
	PaymentStatus string  `json:"paymentStatus"`
	PaymentID     string  `json:"paymentId"`
	Price         decimal `json:"price"`
	PaidPrice     decimal `json:"paidPrice"`
	Currency      string  `json:"currency"`
	BasketID      string  `json:"basketId"`
	Installment   int     `json:"installment"`
	FraudStatus   *int    `json:"fraudStatus"`
	Token         string  `json:"token"`
}

func (p *provider) Complete(ctx context.Context, in pay.CallbackInput, a pay.Attempt) (*pay.Result, error) {
	token := in.Form.Get("token")
	if token == "" {
		token = in.Query.Get("token")
	}
	if !pay.Equal(token, a.ProviderRef) {
		return nil, errors.New("iyzico: callback token does not match the attempt")
	}
	return p.retrieve(ctx, a)
}

func (p *provider) Query(ctx context.Context, a pay.Attempt) (*pay.Result, error) {
	if a.ProviderRef == "" {
		return nil, errors.New("iyzico: attempt has no token")
	}
	return p.retrieve(ctx, a)
}

// retrieve asks iyzico for the Checkout Form result of the attempt's token: the server-side proof.
func (p *provider) retrieve(ctx context.Context, a pay.Attempt) (*pay.Result, error) {
	var res retrieveResponse
	err := p.post(ctx, pathRetrieve, retrieveRequest{Locale: "tr", ConversationID: a.Ref, Token: a.ProviderRef}, &res)
	if err != nil {
		return nil, err
	}
	if res.Status != "success" {
		e := providerError(res.apiResponse)
		return &pay.Result{Status: pay.StatusFailure, Code: e.Code, ProviderCode: e.ProviderCode, ProviderMessage: e.ProviderMessage}, nil
	}
	want := responseSignature(p.secretKey, res.PaymentStatus, res.PaymentID, res.Currency, res.BasketID,
		res.ConversationID, res.PaidPrice.sigText(), res.Price.sigText(), res.Token)
	if !pay.Equal(res.Signature, want) {
		return nil, fmt.Errorf("iyzico: retrieve response: %w", pay.ErrInvalidSignature)
	}
	if res.Token != a.ProviderRef || res.ConversationID != a.Ref || res.BasketID != a.Ref {
		return nil, errors.New("iyzico: retrieve response is not about this attempt")
	}
	out := &pay.Result{TxnID: res.PaymentID, Currency: res.Currency, Installment: res.Installment}
	switch strings.ToUpper(res.PaymentStatus) {
	case "SUCCESS":
		// fraudStatus 1 approved, 0 under review (iyzico notifies later), -1 rejected.
		switch {
		case res.FraudStatus != nil && *res.FraudStatus == -1:
			out.Status, out.Code, out.ProviderCode = pay.StatusFailure, pay.CodeFraudSuspected, "fraudStatus=-1"
		case res.FraudStatus != nil && *res.FraudStatus == 0:
			out.Status = pay.StatusPending
		default:
			amt, err := pay.ParseMinor(string(res.PaidPrice), res.Currency)
			if err != nil {
				return nil, fmt.Errorf("iyzico: paidPrice: %w", err)
			}
			out.Status, out.PaidAmount = pay.StatusSuccess, amt
		}
	case "FAILURE":
		out.Status = pay.StatusFailure
		out.Code = mapCode(res.ErrorCode)
		out.ProviderCode, out.ProviderMessage = res.ErrorCode, res.ErrorMessage
	default: // INIT_THREEDS, CALLBACK_THREEDS, PENDING_*, WAITING, or the form not submitted yet
		out.Status = pay.StatusPending
	}
	return out, nil
}

type cancelRequest struct {
	Locale         string `json:"locale"`
	ConversationID string `json:"conversationId"`
	PaymentID      string `json:"paymentId"`
}

type refundRequest struct {
	Locale         string `json:"locale"`
	ConversationID string `json:"conversationId"`
	PaymentID      string `json:"paymentId"`
	Price          string `json:"price"`
	Currency       string `json:"currency"`
}

type moneyResponse struct {
	apiResponse
	PaymentID string  `json:"paymentId"`
	Price     decimal `json:"price"`
	Currency  string  `json:"currency"`
}

// istanbul is iyzico's business day: a cancel is possible before the bank's end-of-day.
var istanbul = time.FixedZone("TRT", 3*60*60)

func sameDay(a, b time.Time) bool {
	ay, am, ad := a.In(istanbul).Date()
	by, bm, bd := b.In(istanbul).Date()
	return ay == by && am == bm && ad == bd
}

// Refund cancels a whole payment on the day it was made (no statement entry for the customer) and
// otherwise refunds an amount of it with the amount-based refund (v2, by paymentId). When iyzico
// refuses the cancel (the bank closed the day already), it falls back to a refund of the full amount.
func (p *provider) Refund(ctx context.Context, r pay.RefundRequest) (*pay.Result, error) {
	a := r.Attempt
	if a.TxnID == "" {
		return nil, errors.New("iyzico: attempt has no payment id")
	}
	if r.Amount <= 0 {
		return nil, pay.Errorf(pay.CodeInvalidRequest, "", "refund amount must be positive")
	}
	if r.Full && sameDay(a.CreatedAt, p.now()) {
		var res moneyResponse
		err := p.post(ctx, pathCancel, cancelRequest{Locale: "tr", ConversationID: r.RefundRef, PaymentID: a.TxnID}, &res)
		if err != nil {
			return nil, err
		}
		if res.Status == "success" {
			return p.moneyResult(res, r)
		}
	}
	var res moneyResponse
	err := p.post(ctx, pathRefund, refundRequest{
		Locale:         "tr",
		ConversationID: r.RefundRef,
		PaymentID:      a.TxnID,
		Price:          pay.FormatMinor(r.Amount, a.Currency),
		Currency:       a.Currency,
	}, &res)
	if err != nil {
		return nil, err
	}
	if res.Status != "success" {
		return nil, providerError(res.apiResponse)
	}
	return p.moneyResult(res, r)
}

func (p *provider) moneyResult(res moneyResponse, r pay.RefundRequest) (*pay.Result, error) {
	if res.ConversationID != "" && res.ConversationID != r.RefundRef {
		return nil, errors.New("iyzico: refund response is for another conversation")
	}
	out := &pay.Result{Status: pay.StatusSuccess, TxnID: orDefault(res.PaymentID, r.Attempt.TxnID), Currency: res.Currency, PaidAmount: r.Amount}
	if res.Price != "" {
		cur := orDefault(res.Currency, r.Attempt.Currency)
		if amt, err := pay.ParseMinor(string(res.Price), cur); err == nil {
			out.PaidAmount = amt
		}
	}
	return out, nil
}
