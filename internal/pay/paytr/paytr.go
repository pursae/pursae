// Package paytr is the PayTR iFrame API (iframe_v2) provider: the server asks PayTR for a token, the
// customer types the card inside PayTR's iframe, and PayTR reports the outcome with a signed
// server-to-server notification to the URL saved in the PayTR merchant panel.
//
// Protocol: https://dev.paytr.com/iframe-api, https://dev.paytr.com/durum-sorgu,
// https://dev.paytr.com/iade-api. Field names and signatures follow better-payment (MIT).
package paytr

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"wpgo/internal/pay"
)

const (
	liveOrigin = "https://www.paytr.com"
	// scriptURL is the iframeResizer build PayTR serves for the iframe_v2 page.
	scriptURL = liveOrigin + "/js/iframeResizer.min.js?v2"
	initJS    = "iFrameResize({}, '#" + pay.FrameID + "');"
)

// Spec describes the PayTR provider.
func Spec() pay.Spec {
	return pay.Spec{
		ID:    "paytr",
		Title: "PayTR",
		Description: "Card payments in PayTR's secure iframe (iFrame API). Enter the notification URL " +
			"shown here in the PayTR panel under Support & Setup › Settings › Notification URL.",
		Region:     pay.RegionTR,
		Kind:       pay.KindPSP,
		Features:   []pay.Feature{pay.Feature3DS, pay.FeatureInstallments, pay.FeatureRefunds, pay.FeatureQuery},
		Currencies: []string{"TRY", "USD", "EUR", "GBP", "RUB"},
		Fields: []pay.Field{
			{ID: "merchant_id", Title: "Merchant ID", Type: "text", Required: true,
				Desc: "Mağaza No, from the PayTR panel: Support & Setup › Integration Information."},
			{ID: "merchant_key", Title: "Merchant key", Type: "password", Secret: true, Required: true,
				Desc: "Mağaza Parola, from the same page."},
			{ID: "merchant_salt", Title: "Merchant salt", Type: "password", Secret: true, Required: true,
				Desc: "Mağaza Gizli Anahtar, from the same page. PayTR sends payment results to the " +
					"notification URL saved in its panel, not to one sent with the payment: enter the " +
					"notification URL shown in these settings there."},
			{ID: "dark", Title: "Dark payment form", Type: "checkbox", Default: "no",
				Desc: "Show PayTR's payment form in its dark theme."},
		},
		Hosts: pay.Hosts{
			Frame:  []string{liveOrigin},
			Script: []string{liveOrigin},
		},
		DocsURL: "https://dev.paytr.com/iframe-api",
		New:     newProvider,
	}
}

type provider struct {
	cfg               pay.Config
	merchantID        string
	merchantKey, salt string
	dark              bool
}

func newProvider(cfg pay.Config) (pay.Provider, error) {
	if err := cfg.Require("merchant_id", "merchant_key", "merchant_salt"); err != nil {
		return nil, err
	}
	return &provider{
		cfg:         cfg,
		merchantID:  cfg.Value("merchant_id"),
		merchantKey: cfg.Value("merchant_key"),
		salt:        cfg.Value("merchant_salt"),
		dark:        cfg.Bool("dark"),
	}, nil
}

// sign is PayTR's signature: Base64(HMAC-SHA256(key=merchant_key, msg)), msg ending with the salt.
func (p *provider) sign(msg string) string { return pay.HMACSHA256Base64(p.merchantKey, msg) }

func (p *provider) api(path string) string { return p.cfg.Origin(liveOrigin, liveOrigin) + path }

// currency maps an ISO code to PayTR's name for it.
func currency(iso string) (string, bool) {
	switch strings.ToUpper(iso) {
	case "TRY", "":
		return "TL", true
	case "USD", "EUR", "GBP", "RUB":
		return strings.ToUpper(iso), true
	}
	return "", false
}

func isoCurrency(paytr string) string {
	switch c := strings.ToUpper(strings.TrimSpace(paytr)); c {
	case "TL", "TRY":
		return "TRY"
	case "USD", "EUR", "GBP", "RUB":
		return c
	}
	return ""
}

func flag(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// basket is user_basket: Base64 of [[name, unit price as decimal text, quantity], …].
func basket(items []pay.Item, cur string) (string, error) {
	rows := make([][]any, 0, len(items))
	for _, it := range items {
		rows = append(rows, []any{it.Name, pay.FormatMinor(it.UnitPrice, cur), it.Quantity})
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(rows); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(bytes.TrimRight(buf.Bytes(), "\n")), nil
}

// installments returns no_installment and max_installment: 1 is a single payment, 0 PayTR's limit,
// otherwise the largest count offered (PayTR accepts 2–12).
func installments(max int) (string, string) {
	switch {
	case max == 1:
		return "1", "0"
	case max <= 0:
		return "0", "0"
	case max > 12:
		max = 12
	}
	return "0", strconv.Itoa(max)
}

func (p *provider) Start(ctx context.Context, r pay.StartRequest) (*pay.StartResult, error) {
	cur, ok := currency(r.Currency)
	if !ok {
		return nil, pay.Errorf(pay.CodeInvalidRequest, "", "currency %s is not supported by PayTR", r.Currency)
	}
	if r.Amount <= 0 || len(r.Items) == 0 {
		return nil, pay.Errorf(pay.CodeInvalidRequest, "", "empty payment")
	}
	userBasket, err := basket(r.Items, r.Currency)
	if err != nil {
		return nil, err
	}
	noInst, maxInst := installments(r.MaxInstallment)
	amount := strconv.FormatInt(r.Amount, 10)
	email := pay.Trunc(r.Customer.Email, 100)
	test := flag(p.cfg.Test)
	timeout := 30
	if r.ExpiresIn > 0 {
		timeout = int((r.ExpiresIn + time.Minute - 1) / time.Minute)
	}
	lang := "tr"
	if strings.HasPrefix(strings.ToLower(r.Locale), "en") {
		lang = "en"
	}
	name := strings.TrimSpace(r.Customer.FirstName + " " + r.Customer.LastName)
	if name == "" {
		name = r.Billing.Name
	}
	addr := strings.Join(nonEmpty(r.Billing.Line1, r.Billing.Line2, r.Billing.Postcode, r.Billing.City, r.Billing.State, r.Billing.Country), " ")

	token := p.sign(p.merchantID + r.Customer.IP + r.Ref + email + amount + userBasket + noInst + maxInst + cur + test + p.salt)
	form := url.Values{
		"merchant_id":       {p.merchantID},
		"user_ip":           {r.Customer.IP},
		"merchant_oid":      {r.Ref},
		"email":             {email},
		"payment_amount":    {amount},
		"paytr_token":       {token},
		"user_basket":       {userBasket},
		"debug_on":          {test},
		"no_installment":    {noInst},
		"max_installment":   {maxInst},
		"user_name":         {pay.Trunc(name, 60)},
		"user_address":      {pay.Trunc(addr, 400)},
		"user_phone":        {pay.Trunc(r.Customer.Phone, 20)},
		"merchant_ok_url":   {r.ReturnURL},
		"merchant_fail_url": {r.FailURL},
		"timeout_limit":     {strconv.Itoa(timeout)},
		"currency":          {cur},
		"test_mode":         {test},
		"lang":              {lang},
		"iframe_v2":         {"1"},
	}
	if form.Get("merchant_fail_url") == "" {
		form.Set("merchant_fail_url", r.ReturnURL)
	}
	if p.dark {
		form.Set("iframe_v2_dark", "1")
	}
	status, body, err := pay.PostForm(ctx, p.cfg.HTTP, p.api("/odeme/api/get-token"), form, nil)
	if err != nil {
		return nil, err
	}
	var res struct {
		Status string `json:"status"`
		Token  string `json:"token"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return nil, pay.Errorf(pay.CodeProviderDown, strconv.Itoa(status), "unreadable get-token response")
	}
	if res.Status != "success" || res.Token == "" {
		return nil, &pay.Error{Code: pay.CodeInvalidRequest, ProviderCode: res.Status, ProviderMessage: res.Reason}
	}
	if !safeToken(res.Token) {
		return nil, pay.Errorf(pay.CodeProviderDown, "", "unexpected token format")
	}
	return &pay.StartResult{
		Embed: &pay.Embed{
			FrameURL: liveOrigin + "/odeme/guvenli/" + res.Token,
			Scripts:  []string{scriptURL},
			Init:     initJS,
		},
		ProviderRef: res.Token,
	}, nil
}

func nonEmpty(s ...string) []string {
	var out []string
	for _, v := range s {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// safeToken keeps the token, which becomes a path segment of the iframe URL, to plain characters.
func safeToken(t string) bool {
	for _, c := range t {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			return false
		}
	}
	return len(t) <= 512
}

func (p *provider) CallbackRef(in pay.CallbackInput) string {
	return in.Form.Get("merchant_oid")
}

func (p *provider) Complete(ctx context.Context, in pay.CallbackInput, a pay.Attempt) (*pay.Result, error) {
	if in.Kind != pay.CallbackNotify {
		// Browser landings carry no proof; the signed notification decides.
		return &pay.Result{Status: pay.StatusPending}, nil
	}
	f := in.Form
	oid, status, total, hash := f.Get("merchant_oid"), f.Get("status"), f.Get("total_amount"), f.Get("hash")
	if oid == "" || status == "" || total == "" || hash == "" {
		return nil, pay.ErrInvalidSignature
	}
	if !pay.Equal(p.sign(oid+p.salt+status+total), hash) {
		return nil, pay.ErrInvalidSignature
	}
	if oid != a.Ref {
		return nil, errors.New("paytr: notification is about another payment")
	}
	ack := &pay.Ack{Body: "OK", ContentType: "text/plain"}
	switch status {
	case "success":
		paid, err := strictInt(total)
		if err != nil {
			return nil, err
		}
		res := &pay.Result{Status: pay.StatusSuccess, PaidAmount: paid, Ack: ack}
		// installment_count is not covered by the hash: informational only.
		if n, err := strconv.Atoi(f.Get("installment_count")); err == nil && n >= 0 && n <= 36 {
			res.Installment = n
		}
		return res, nil
	case "failed":
		code := f.Get("failed_reason_code")
		return &pay.Result{
			Status:          pay.StatusFailure,
			Code:            failureCode(code),
			ProviderCode:    code,
			ProviderMessage: f.Get("failed_reason_msg"),
			Ack:             ack,
		}, nil
	}
	return nil, fmt.Errorf("paytr: unknown notification status %q", status)
}

// strictInt parses a kuruş amount: digits only, so a malformed value is never read as another amount.
func strictInt(s string) (int64, error) {
	if s == "" || len(s) > 15 {
		return 0, fmt.Errorf("paytr: bad amount %q", s)
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("paytr: bad amount %q", s)
		}
	}
	return strconv.ParseInt(s, 10, 64)
}

// failureCode maps failed_reason_code (iFrame API step 2).
func failureCode(c string) pay.Code {
	switch c {
	case "1", "2": // phone not entered / wrong SMS code during authentication
		return pay.Code3DSFailed
	case "3", "11": // security check not approved / fraud warning
		return pay.CodeFraudSuspected
	case "6": // customer left the page or the time limit passed
		return pay.CodeCancelledByUser
	case "8": // installments not allowed for this card
		return pay.CodeInstallment
	case "9", "10": // store not allowed this card / 3D Secure required
		return pay.CodeCardDeclined
	case "99": // integration error
		return pay.CodeConfiguration
	}
	return pay.CodeUnknown // 0: free text in failed_reason_msg
}

// apiError maps err_no of the status and refund APIs.
func apiError(no, msg string) *pay.Error {
	code := pay.CodeUnknown
	switch no {
	case "004":
		code = pay.CodeNotFound
	case "006":
		code = pay.CodeInvalidRequest
	}
	return &pay.Error{Code: code, ProviderCode: no, ProviderMessage: msg}
}

type queryResponse struct {
	Status        string          `json:"status"`
	PaymentAmount json.RawMessage `json:"payment_amount"`
	PaymentTotal  json.RawMessage `json:"payment_total"`
	Currency      string          `json:"currency"`
	Taksit        json.RawMessage `json:"taksit"`
	Returns       json.RawMessage `json:"returns"`
	ErrNo         json.RawMessage `json:"err_no"`
	ErrMsg        string          `json:"err_msg"`
}

// text reads a JSON string or number as text.
func text(r json.RawMessage) string {
	var s string
	if json.Unmarshal(r, &s) == nil {
		return s
	}
	var n json.Number
	if json.Unmarshal(r, &n) == nil {
		return n.String()
	}
	return ""
}

func (p *provider) Query(ctx context.Context, a pay.Attempt) (*pay.Result, error) {
	form := url.Values{
		"merchant_id":  {p.merchantID},
		"merchant_oid": {a.Ref},
		"paytr_token":  {p.sign(p.merchantID + a.Ref + p.salt)},
	}
	status, body, err := pay.PostForm(ctx, p.cfg.HTTP, p.api("/odeme/durum-sorgu"), form, nil)
	if err != nil {
		return nil, err
	}
	var res queryResponse
	if err := json.Unmarshal(body, &res); err != nil {
		return nil, pay.Errorf(pay.CodeProviderDown, strconv.Itoa(status), "unreadable status response")
	}
	if res.Status != "success" {
		no := text(res.ErrNo)
		if no == "004" {
			// No successful payment with this merchant_oid: not paid (yet); the API does not say
			// whether the customer failed or is still on the form.
			return &pay.Result{Status: pay.StatusPending, ProviderCode: no, ProviderMessage: res.ErrMsg}, nil
		}
		return nil, apiError(no, res.ErrMsg)
	}
	cur := isoCurrency(res.Currency)
	paidText := text(res.PaymentTotal)
	if paidText == "" {
		paidText = text(res.PaymentAmount)
	}
	paid, err := pay.ParseMinor(paidText, cur)
	if err != nil {
		return nil, err
	}
	out := &pay.Result{Status: pay.StatusSuccess, PaidAmount: paid, Currency: cur}
	if n, err := strconv.Atoi(text(res.Taksit)); err == nil && n > 1 {
		out.Installment = n
	}
	var returns []struct {
		ReturnAmount json.RawMessage `json:"return_amount"`
	}
	if len(res.Returns) > 0 && json.Unmarshal(res.Returns, &returns) == nil && len(returns) > 0 {
		var refunded int64
		for _, r := range returns {
			v, err := pay.ParseMinor(text(r.ReturnAmount), cur)
			if err != nil {
				return nil, err
			}
			refunded += v
		}
		if paid > 0 && refunded >= paid {
			out.Status = pay.StatusCancelled
		}
	}
	return out, nil
}

func (p *provider) Refund(ctx context.Context, r pay.RefundRequest) (*pay.Result, error) {
	if r.Amount <= 0 {
		return nil, pay.Errorf(pay.CodeInvalidRequest, "", "refund amount must be positive")
	}
	amount := pay.FormatMinor(r.Amount, r.Attempt.Currency)
	form := url.Values{
		"merchant_id":   {p.merchantID},
		"merchant_oid":  {r.Attempt.Ref},
		"return_amount": {amount},
		"paytr_token":   {p.sign(p.merchantID + r.Attempt.Ref + amount + p.salt)},
	}
	if r.RefundRef != "" {
		form.Set("reference_no", pay.Trunc(r.RefundRef, 64))
	}
	status, body, err := pay.PostForm(ctx, p.cfg.HTTP, p.api("/odeme/iade"), form, nil)
	if err != nil {
		return nil, err
	}
	var res struct {
		Status      string          `json:"status"`
		ReferenceNo string          `json:"reference_no"`
		ErrNo       json.RawMessage `json:"err_no"`
		ErrMsg      string          `json:"err_msg"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		// The refund may have been processed: do not let the caller retry blindly.
		return nil, fmt.Errorf("%w: unreadable refund response (HTTP %d)", pay.ErrUnknownOutcome, status)
	}
	if res.Status != "success" {
		return nil, apiError(text(res.ErrNo), res.ErrMsg)
	}
	return &pay.Result{Status: pay.StatusSuccess, PaidAmount: r.Amount, TxnID: res.ReferenceNo}, nil
}
