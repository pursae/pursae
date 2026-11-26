// Package sipay is the Sipay hosted payment page provider (purchase/link): the server sends the invoice
// and gets a payment link, the customer types the card on Sipay's page, and Sipay sends the browser back
// to the invoice's return_url or cancel_url. The outcome is never read from that landing: Complete asks
// Sipay's checkstatus API, server to server, for the invoice.
//
// Protocol: https://apidocs.sipay.com.tr/non-secure-and-3d-payment-with-sipay-26902318e0 (purchase/link),
// https://apidocs.sipay.com.tr/checkstatus-25727396e0, https://apidocs.sipay.com.tr/refund-26908125e0,
// https://apidocs.sipay.com.tr/hash-validation-2106880m0. Token, hash keys, refund and status shapes
// follow better-payment (MIT).
package sipay

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"wpgo/internal/pay"
)

const (
	liveOrigin = "https://app.sipay.com.tr"
	testOrigin = "https://provisioning.sipay.com.tr"

	pathToken  = "/ccpayment/api/token"
	pathLink   = "/ccpayment/purchase/link"
	pathStatus = "/ccpayment/api/checkstatus"
	pathRefund = "/ccpayment/api/refund"

	// successCode is Sipay's application status_code for a processed request.
	successCode = "100"
	// tokenLifetime: tokens are valid for two hours; a new one is asked for well before that.
	tokenLifetime = 100 * time.Minute
)

// Specs lists the Sipay providers. QNBpay (the same API on QNBpay's hosts) is not listed: its hosted
// payment page could not be confirmed from a citable source.
func Specs() []pay.Spec { return []pay.Spec{Spec()} }

// Spec describes the Sipay provider.
func Spec() pay.Spec {
	return pay.Spec{
		ID:    "sipay",
		Title: "Sipay",
		Description: "Card payments on Sipay's secure payment page (purchase link). The result is " +
			"confirmed with Sipay's status API.",
		Region:     pay.RegionTR,
		Kind:       pay.KindPSP,
		Features:   []pay.Feature{pay.Feature3DS, pay.FeatureInstallments, pay.FeatureRefunds, pay.FeatureQuery},
		Currencies: []string{"TRY", "USD", "EUR"},
		Fields: []pay.Field{
			{ID: "merchant_key", Title: "Merchant key", Type: "password", Secret: true, Required: true,
				Desc: "From the Sipay merchant panel: Settings › Integration & API."},
			{ID: "app_id", Title: "App ID", Type: "text", Required: true,
				Desc: "App Key / App ID from the same page."},
			{ID: "app_secret", Title: "App secret", Type: "password", Secret: true, Required: true,
				Desc: "App Secret from the same page. It signs refund and status requests."},
		},
		DocsURL: "https://apidocs.sipay.com.tr/",
		New:     newProvider,
	}
}

type provider struct {
	cfg                           pay.Config
	merchantKey, appID, appSecret string

	mu      sync.Mutex
	token   string
	expires time.Time
}

func newProvider(cfg pay.Config) (pay.Provider, error) {
	if err := cfg.Require("merchant_key", "app_id", "app_secret"); err != nil {
		return nil, err
	}
	return &provider{
		cfg:         cfg,
		merchantKey: cfg.Value("merchant_key"),
		appID:       cfg.Value("app_id"),
		appSecret:   cfg.Value("app_secret"),
	}, nil
}

func (p *provider) origin() string { return p.cfg.Origin(liveOrigin, testOrigin) }

// --- hash keys -------------------------------------------------------------------------------------

// aesKey is the AES-256 key of a hash key: the first 32 hex characters of
// SHA-256(hex(SHA-1(app_secret)) + salt), used as ASCII bytes.
func aesKey(appSecret, salt string) []byte {
	s1 := sha1.Sum([]byte(appSecret))
	s2 := sha256.Sum256([]byte(hex.EncodeToString(s1[:]) + salt))
	return []byte(hex.EncodeToString(s2[:])[:32])
}

// newHashKey encrypts the "|"-joined fields with AES-256-CBC under a random IV and salt and writes
// "iv:salt:base64" with "/" as "__", Sipay's hash_key format.
func newHashKey(appSecret string, fields ...string) (string, error) {
	var rnd [10]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return "", err
	}
	iv, salt := hex.EncodeToString(rnd[:8]), hex.EncodeToString(rnd[8:])
	return encryptHashKey(appSecret, iv, salt, strings.Join(fields, "|"))
}

func encryptHashKey(appSecret, iv, salt, plain string) (string, error) {
	block, err := aes.NewCipher(aesKey(appSecret, salt))
	if err != nil {
		return "", err
	}
	pad := aes.BlockSize - len(plain)%aes.BlockSize
	data := append([]byte(plain), bytes.Repeat([]byte{byte(pad)}, pad)...)
	cipher.NewCBCEncrypter(block, []byte(iv)).CryptBlocks(data, data)
	out := iv + ":" + salt + ":" + base64.StdEncoding.EncodeToString(data)
	return strings.ReplaceAll(out, "/", "__"), nil
}

// --- transport -------------------------------------------------------------------------------------

// text reads a JSON string or number as text.
func text(r json.RawMessage) string {
	var s string
	if json.Unmarshal(r, &s) == nil {
		return strings.TrimSpace(s)
	}
	var n json.Number
	if json.Unmarshal(r, &n) == nil {
		return n.String()
	}
	return ""
}

func (p *provider) getToken(ctx context.Context, renew bool) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !renew && p.token != "" && time.Now().Before(p.expires) {
		return p.token, nil
	}
	body, _ := json.Marshal(map[string]string{"app_id": p.appID, "app_secret": p.appSecret})
	status, raw, err := pay.PostBody(ctx, p.cfg.HTTP, p.origin()+pathToken, "application/json", body, jsonAccept)
	if err != nil {
		// Asking for a token changes nothing at Sipay: an unknown outcome is a plain failure here.
		return "", fmt.Errorf("sipay: token request failed: %v", err)
	}
	var res struct {
		StatusCode        json.RawMessage `json:"status_code"`
		StatusDescription string          `json:"status_description"`
		Data              struct {
			Token string `json:"token"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &res) != nil {
		return "", pay.Errorf(pay.CodeProviderDown, strconv.Itoa(status), "unreadable token response")
	}
	if res.Data.Token == "" {
		return "", &pay.Error{Code: pay.CodeConfiguration, ProviderCode: text(res.StatusCode), ProviderMessage: res.StatusDescription}
	}
	p.token, p.expires = res.Data.Token, time.Now().Add(tokenLifetime)
	return p.token, nil
}

var jsonAccept = http.Header{"Accept": {"application/json"}}

// unauthenticated reports Sipay's answer to an expired token (status_code 101); such a request was not
// processed, so it is sent once more with a new token.
func unauthenticated(status int, body []byte) bool {
	if status == http.StatusUnauthorized {
		return true
	}
	var res struct {
		StatusCode json.RawMessage `json:"status_code"`
	}
	return json.Unmarshal(body, &res) == nil && text(res.StatusCode) == "101"
}

// call posts JSON with the Bearer token.
func (p *provider) call(ctx context.Context, path string, payload any) (int, []byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, nil, err
	}
	send := func(renew bool) (int, []byte, error) {
		tok, err := p.getToken(ctx, renew)
		if err != nil {
			return 0, nil, err
		}
		h := http.Header{"Accept": {"application/json"}, "Authorization": {"Bearer " + tok}}
		return pay.PostBody(ctx, p.cfg.HTTP, p.origin()+path, "application/json", body, h)
	}
	status, raw, err := send(false)
	if err == nil && unauthenticated(status, raw) {
		status, raw, err = send(true)
	}
	return status, raw, err
}

// --- start -----------------------------------------------------------------------------------------

func currency(iso string) (string, bool) {
	switch c := strings.ToUpper(iso); c {
	case "", "TRY", "TL":
		return "TRY", true
	case "USD", "EUR":
		return c, true
	}
	return "", false
}

func lang(locale string) string {
	if strings.HasPrefix(strings.ToLower(locale), "en") {
		return "en"
	}
	return "tr"
}

type item struct {
	Name        string `json:"name"`
	Price       string `json:"price"`
	Quantity    int    `json:"quantity"`
	Description string `json:"description"`
}

type invoice struct {
	InvoiceID          string `json:"invoice_id"`
	InvoiceDescription string `json:"invoice_description"`
	Total              string `json:"total"`
	ReturnURL          string `json:"return_url"`
	CancelURL          string `json:"cancel_url"`
	Items              []item `json:"items"`
	BillAddress1       string `json:"bill_address1"`
	BillAddress2       string `json:"bill_address2"`
	BillCity           string `json:"bill_city"`
	BillPostcode       string `json:"bill_postcode"`
	BillState          string `json:"bill_state"`
	BillCountry        string `json:"bill_country"`
	BillEmail          string `json:"bill_email"`
	BillPhone          string `json:"bill_phone"`
	ResponseMethod     string `json:"response_method"`
}

type linkRequest struct {
	MerchantKey    string `json:"merchant_key"`
	Invoice        string `json:"invoice"`
	CurrencyCode   string `json:"currency_code"`
	Name           string `json:"name"`
	Surname        string `json:"surname"`
	BillAddress1   string `json:"bill_address1"`
	BillAddress2   string `json:"bill_address2,omitempty"`
	BillCity       string `json:"bill_city,omitempty"`
	BillPostcode   string `json:"bill_postcode,omitempty"`
	BillState      string `json:"bill_state,omitempty"`
	BillCountry    string `json:"bill_country,omitempty"`
	BillEmail      string `json:"bill_email"`
	BillPhone      string `json:"bill_phone"`
	MaxInstallment int    `json:"max_installment,omitempty"`
	OrderType      int    `json:"order_type"`
	AppLang        string `json:"app_lang"`
}

func orDash(s string) string {
	if s = strings.TrimSpace(s); s == "" {
		return "-"
	}
	return s
}

func (p *provider) Start(ctx context.Context, r pay.StartRequest) (*pay.StartResult, error) {
	cur, ok := currency(r.Currency)
	if !ok {
		return nil, pay.Errorf(pay.CodeInvalidRequest, "", "currency %s is not supported by Sipay", r.Currency)
	}
	if r.Amount <= 0 || len(r.Items) == 0 {
		return nil, pay.Errorf(pay.CodeInvalidRequest, "", "empty payment")
	}
	desc := r.Description
	if desc == "" {
		desc = "Order " + r.OrderNumber
	}
	items := make([]item, 0, len(r.Items))
	for _, it := range r.Items {
		d := it.Category
		if d == "" {
			d = it.Name
		}
		items = append(items, item{Name: it.Name, Price: pay.FormatMinor(it.UnitPrice, cur), Quantity: it.Quantity, Description: d})
	}
	first, last := r.Customer.FirstName, r.Customer.LastName
	if first == "" && last == "" {
		first, last, _ = strings.Cut(strings.TrimSpace(r.Billing.Name), " ")
	}
	cancel := r.FailURL
	if cancel == "" {
		cancel = r.ReturnURL
	}
	b := r.Billing
	inv := invoice{
		InvoiceID: r.Ref, InvoiceDescription: desc, Total: pay.FormatMinor(r.Amount, cur),
		ReturnURL: r.ReturnURL, CancelURL: cancel, Items: items,
		BillAddress1: orDash(b.Line1), BillAddress2: b.Line2, BillCity: b.City, BillPostcode: b.Postcode,
		BillState: b.State, BillCountry: b.Country, BillEmail: r.Customer.Email, BillPhone: r.Customer.Phone,
		ResponseMethod: "POST",
	}
	invJSON, err := json.Marshal(inv)
	if err != nil {
		return nil, err
	}
	req := linkRequest{
		MerchantKey: p.merchantKey, Invoice: string(invJSON), CurrencyCode: cur,
		Name: orDash(first), Surname: orDash(last),
		BillAddress1: orDash(b.Line1), BillAddress2: b.Line2, BillCity: b.City, BillPostcode: b.Postcode,
		BillState: b.State, BillCountry: b.Country, BillEmail: r.Customer.Email, BillPhone: r.Customer.Phone,
		OrderType: 0, AppLang: lang(r.Locale),
	}
	if r.MaxInstallment > 0 {
		req.MaxInstallment = min(r.MaxInstallment, 12)
	}
	status, body, err := p.call(ctx, pathLink, req)
	if err != nil {
		return nil, err
	}
	var res struct {
		StatusCode        json.RawMessage `json:"status_code"`
		StatusDescription string          `json:"status_description"`
		Message           string          `json:"message"`
		Link              string          `json:"link"`
		OrderID           json.RawMessage `json:"order_id"`
	}
	if json.Unmarshal(body, &res) != nil {
		return nil, pay.Errorf(pay.CodeProviderDown, strconv.Itoa(status), "unreadable purchase/link response")
	}
	code := text(res.StatusCode)
	if code != successCode || res.Link == "" {
		msg := res.StatusDescription
		if msg == "" {
			msg = res.Message
		}
		return nil, &pay.Error{Code: mapCode(code), ProviderCode: code, ProviderMessage: msg}
	}
	if !p.onOrigin(res.Link) {
		return nil, pay.Errorf(pay.CodeProviderDown, "", "payment link on an unexpected host")
	}
	return &pay.StartResult{Redirect: res.Link, ProviderRef: text(res.OrderID)}, nil
}

// onOrigin keeps the browser on Sipay's own host: the link must be under the API origin in use.
func (p *provider) onOrigin(link string) bool {
	u, err := url.Parse(link)
	o, err2 := url.Parse(p.origin())
	return err == nil && err2 == nil && u.Scheme == o.Scheme && u.Host == o.Host && u.User == nil
}

// --- callbacks, status -----------------------------------------------------------------------------

func (p *provider) CallbackRef(in pay.CallbackInput) string {
	if v := in.Form.Get("invoice_id"); v != "" {
		return v
	}
	return in.Query.Get("invoice_id")
}

// Complete asks checkstatus for the attempt's invoice: what the browser brings back (status,
// hash_key, amount) is not used as proof, because the content of the hash_key Sipay adds to a payment
// page's return is not documented.
func (p *provider) Complete(ctx context.Context, in pay.CallbackInput, a pay.Attempt) (*pay.Result, error) {
	if ref := p.CallbackRef(in); ref != "" && ref != a.Ref {
		return nil, errors.New("sipay: callback is about another payment")
	}
	res, err := p.Query(ctx, a)
	if err != nil {
		if errors.Is(err, pay.ErrUnknownOutcome) {
			return &pay.Result{Status: pay.StatusPending}, nil
		}
		return nil, err
	}
	return res, nil
}

type statusResponse struct {
	StatusCode            json.RawMessage `json:"status_code"`
	StatusDescription     string          `json:"status_description"`
	TransactionStatus     string          `json:"transaction_status"`
	OrderID               json.RawMessage `json:"order_id"`
	TransactionID         json.RawMessage `json:"transaction_id"`
	Message               string          `json:"message"`
	Reason                string          `json:"reason"`
	InvoiceID             string          `json:"invoice_id"`
	TotalRefundedAmount   json.RawMessage `json:"total_refunded_amount"`
	TransactionAmount     json.RawMessage `json:"transaction_amount"`
	ProductPrice          json.RawMessage `json:"product_price"`
	Installment           json.RawMessage `json:"installment"`
	OriginalBankErrorCode *string         `json:"original_bank_error_code"`
	OriginalBankErrorDesc *string         `json:"original_bank_error_description"`
	BankStatusDescription string          `json:"bank_status_description"`
}

func (p *provider) Query(ctx context.Context, a pay.Attempt) (*pay.Result, error) {
	hk, err := newHashKey(p.appSecret, a.Ref, p.merchantKey)
	if err != nil {
		return nil, err
	}
	status, body, err := p.call(ctx, pathStatus, map[string]any{
		"merchant_key":           p.merchantKey,
		"invoice_id":             a.Ref,
		"include_pending_status": true,
		"show_refund_info":       "Y",
		"hash_key":               hk,
		"app_lang":               "en",
	})
	if err != nil {
		return nil, err
	}
	var res statusResponse
	if json.Unmarshal(body, &res) != nil {
		return nil, pay.Errorf(pay.CodeProviderDown, strconv.Itoa(status), "unreadable checkstatus response")
	}
	if res.InvoiceID != "" && res.InvoiceID != a.Ref {
		return nil, errors.New("sipay: status answer is about another invoice")
	}
	code := text(res.StatusCode)
	out := &pay.Result{TxnID: text(res.OrderID), ProviderCode: code, ProviderMessage: firstNonEmpty(res.StatusDescription, res.Message, res.Reason)}
	switch strings.ToLower(strings.TrimSpace(res.TransactionStatus)) {
	case "completed", "partial refund", "partially_refund":
		if code != successCode {
			return nil, pay.Errorf(pay.CodeProviderDown, code, "completed status without success code")
		}
		cur := a.Currency
		amt := text(res.TransactionAmount)
		if amt == "" {
			amt = text(res.ProductPrice)
		}
		paid, err := pay.ParseMinor(amt, cur)
		if err != nil {
			return nil, err
		}
		out.Status, out.PaidAmount = pay.StatusSuccess, paid
		out.ProviderCode, out.ProviderMessage = "", ""
		if n, err := strconv.Atoi(text(res.Installment)); err == nil && n > 1 && n <= 36 {
			out.Installment = n
		}
	case "refunded", "refund", "cancelled", "canceled", "void":
		out.Status = pay.StatusCancelled
	case "failed", "fail":
		if code == "32" { // no request for this invoice id: the customer has not paid (yet)
			out.Status = pay.StatusPending
			break
		}
		out.Status = pay.StatusFailure
		bank := ""
		if res.OriginalBankErrorCode != nil {
			bank = strings.TrimSpace(*res.OriginalBankErrorCode)
		}
		if bank != "" {
			out.ProviderCode = bank
			out.Code = bankCode(bank)
			if res.OriginalBankErrorDesc != nil && *res.OriginalBankErrorDesc != "" {
				out.ProviderMessage = *res.OriginalBankErrorDesc
			}
		} else {
			out.Code = mapCode(code)
		}
	case "":
		if code != successCode && code != "69" {
			return nil, &pay.Error{Code: mapCode(code), ProviderCode: code, ProviderMessage: out.ProviderMessage}
		}
		out.Status = pay.StatusPending
	default: // Pending, Pre-Authorization (not captured) and anything new
		out.Status = pay.StatusPending
	}
	return out, nil
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}

// --- refund ----------------------------------------------------------------------------------------

// Refund uses Sipay's refund API, which voids a same-day payment by itself, so Full needs no cancel.
func (p *provider) Refund(ctx context.Context, r pay.RefundRequest) (*pay.Result, error) {
	if r.Amount <= 0 {
		return nil, pay.Errorf(pay.CodeInvalidRequest, "", "refund amount must be positive")
	}
	amount := pay.FormatMinor(r.Amount, r.Attempt.Currency)
	hk, err := newHashKey(p.appSecret, amount, r.Attempt.Ref, p.merchantKey)
	if err != nil {
		return nil, err
	}
	status, body, err := p.call(ctx, pathRefund, map[string]string{
		"invoice_id":   r.Attempt.Ref,
		"amount":       amount,
		"merchant_key": p.merchantKey,
		"hash_key":     hk,
		"app_lang":     "en",
	})
	if err != nil {
		return nil, err
	}
	var res struct {
		StatusCode        json.RawMessage `json:"status_code"`
		StatusDescription string          `json:"status_description"`
		Message           string          `json:"message"`
		RefNo             json.RawMessage `json:"ref_no"`
		RefNumber         json.RawMessage `json:"ref_number"`
	}
	if json.Unmarshal(body, &res) != nil {
		return nil, fmt.Errorf("%w: unreadable refund response (HTTP %d)", pay.ErrUnknownOutcome, status)
	}
	code := text(res.StatusCode)
	if code != successCode {
		return nil, &pay.Error{Code: mapCode(code), ProviderCode: code, ProviderMessage: firstNonEmpty(res.StatusDescription, res.Message)}
	}
	return &pay.Result{Status: pay.StatusSuccess, PaidAmount: r.Amount, TxnID: firstNonEmpty(text(res.RefNo), text(res.RefNumber))}, nil
}

// --- error codes -----------------------------------------------------------------------------------

// mapCode maps Sipay's status_code (better-payment's table).
func mapCode(c string) pay.Code {
	switch c {
	case "1", "12", "13", "32", "33", "42", "85", "110":
		return pay.CodeInvalidRequest
	case "3", "117":
		return pay.CodeDuplicate
	case "14", "30", "34", "35", "36", "37", "38", "39", "43", "68", "90", "101", "102":
		return pay.CodeConfiguration
	case "31":
		return pay.CodeNotFound
	case "41", "60", "76", "77", "105":
		return pay.CodeCardDeclined
	case "44", "45", "46", "47", "48":
		return pay.CodeLimitExceeded
	case "108":
		return pay.CodeInvalidCard
	case "119":
		return pay.CodeFraudSuspected
	}
	return pay.CodeUnknown
}

// bankCode maps the bank's ISO 8583 response code (original_bank_error_code).
func bankCode(c string) pay.Code {
	switch c {
	case "51":
		return pay.CodeInsufficientFunds
	case "54", "33":
		return pay.CodeExpiredCard
	case "14", "15", "82":
		return pay.CodeInvalidCard
	case "41", "43":
		return pay.CodeStolenOrLost
	case "59", "34":
		return pay.CodeFraudSuspected
	case "61", "65":
		return pay.CodeLimitExceeded
	case "05", "57", "62", "04", "07":
		return pay.CodeCardDeclined
	case "91", "96":
		return pay.CodeProviderDown
	}
	return pay.CodeUnknown
}
