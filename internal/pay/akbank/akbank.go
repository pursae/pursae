// Package akbank is Akbank's JSON virtual POS ("Akbank POS", apiportal.akbank.com) in its hosted
// model, 3D_PAY_HOSTING: the store posts a signed form without card fields to Akbank's payment page,
// the customer types the card there, and Akbank posts the signed result back to okUrl / failUrl.
//
// The result callback's hash does not cover the amount, so a successful callback is confirmed with an
// order history request (txnCode 1010) made here, which also gives the charged amount.
//
// Protocol: better-payment (MIT) for the API, signatures and txn codes; mews/pos (MIT,
// github.com/mewebstudio/pos, AkbankPos) for the 3D_PAY_HOSTING form, its endpoint and the callback
// hash check.
package akbank

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha512"
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
	liveAPI     = "https://api.akbank.com/api/v1/payment/virtualpos"
	testAPI     = "https://apipre.akbank.com/api/v1/payment/virtualpos"
	liveGateway = "https://virtualpospaymentgateway.akbank.com"
	testGateway = "https://virtualpospaymentgatewaypre.akbank.com"
	hostingPath = "/payhosting"

	apiVersion  = "1.00"
	successCode = "VPS-0000"

	txnSecureSale = "3000"
	txnSale       = "1000"
	txnRefund     = "1002"
	txnVoid       = "1003"
	txnHistory    = "1010"
)

// Spec describes the Akbank provider.
func Spec() pay.Spec {
	return pay.Spec{
		ID:    "akbank",
		Title: "Akbank",
		Description: "Card payments on Akbank's secure payment page (Akbank POS, 3D Pay Hosting). " +
			"The terminal must be enabled for the 3D_PAY_HOSTING payment model.",
		Region:     pay.RegionTR,
		Kind:       pay.KindBank,
		Features:   []pay.Feature{pay.Feature3DS, pay.FeatureInstallments, pay.FeatureRefunds, pay.FeatureQuery},
		Currencies: []string{"TRY", "USD", "EUR", "GBP", "JPY", "RUB"},
		Fields: []pay.Field{
			{ID: "merchant_safe_id", Title: "Merchant safe ID", Type: "text", Required: true,
				Desc: "Üye İşyeri Güvenli No, from the Akbank POS panel."},
			{ID: "terminal_safe_id", Title: "Terminal safe ID", Type: "text", Required: true,
				Desc: "Terminal Güvenli No, from the Akbank POS panel."},
			{ID: "secret_key", Title: "Secret key", Type: "password", Secret: true, Required: true,
				Desc: "Gizli Anahtar, from the Akbank POS panel. Signs every request and the payment results."},
		},
		Hosts:   pay.Hosts{Form: []string{liveGateway, testGateway}},
		DocsURL: "https://apiportal.akbank.com",
		New:     newProvider,
	}
}

type provider struct {
	cfg                  pay.Config
	merchantID, terminal string
	secret               string
}

func newProvider(cfg pay.Config) (pay.Provider, error) {
	if err := cfg.Require("merchant_safe_id", "terminal_safe_id", "secret_key"); err != nil {
		return nil, err
	}
	return &provider{
		cfg:        cfg,
		merchantID: cfg.Value("merchant_safe_id"),
		terminal:   cfg.Value("terminal_safe_id"),
		secret:     cfg.Value("secret_key"),
	}, nil
}

// sign is Akbank's signature: Base64(HMAC-SHA512(key=secret key, msg)).
func sign(secret, msg string) string {
	m := hmac.New(sha512.New, []byte(secret))
	m.Write([]byte(msg))
	return base64.StdEncoding.EncodeToString(m.Sum(nil))
}

// formHashFields are the fields of the payment form hash, in this order; absent ones count as "".
// The card fields of the merchant-side models are part of the formula and stay empty here.
var formHashFields = []string{
	"paymentModel", "txnCode", "merchantSafeId", "terminalSafeId", "orderId", "lang", "amount",
	"ccbRewardAmount", "pcbRewardAmount", "xcbRewardAmount", "currencyCode", "installCount", "okUrl",
	"failUrl", "emailAddress", "subMerchantId", "creditCard", "expiredDate", "cvv", "randomNumber",
	"requestDateTime", "b2bIdentityNumber",
}

func formHash(secret string, fields map[string]string) string {
	var b strings.Builder
	for _, k := range formHashFields {
		b.WriteString(fields[k])
	}
	return sign(secret, b.String())
}

// randomNumber is the 128 hex character nonce every request carries.
func randomNumber() (string, error) {
	b := make([]byte, 64)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return strings.ToUpper(hex.EncodeToString(b)), nil
}

// istanbul is Türkiye's time (UTC+3 all year), the zone of requestDateTime.
var istanbul = time.FixedZone("TRT", 3*60*60)

func requestDateTime() string { return time.Now().In(istanbul).Format("2006-01-02T15:04:05") + ".000" }

func (p *provider) gateway() string {
	if p.cfg.Test {
		return testGateway
	}
	return liveGateway
}

func (p *provider) Start(ctx context.Context, r pay.StartRequest) (*pay.StartResult, error) {
	cur := pay.NumericCurrency(r.Currency)
	if !supported(r.Currency) || cur == "" {
		return nil, pay.Errorf(pay.CodeInvalidRequest, "", "currency %s is not supported by Akbank", r.Currency)
	}
	if r.Amount <= 0 {
		return nil, pay.Errorf(pay.CodeInvalidRequest, "", "empty payment")
	}
	rnd, err := randomNumber()
	if err != nil {
		return nil, err
	}
	lang := "TR"
	if strings.HasPrefix(strings.ToLower(r.Locale), "en") {
		lang = "EN"
	}
	inst := "1"
	if r.Installment > 1 {
		inst = strconv.Itoa(r.Installment)
	}
	failURL := r.FailURL
	if failURL == "" {
		failURL = r.ReturnURL
	}
	order := [][2]string{
		{"paymentModel", "3D_PAY_HOSTING"},
		{"txnCode", txnSecureSale},
		{"merchantSafeId", p.merchantID},
		{"terminalSafeId", p.terminal},
		{"orderId", r.Ref},
		{"lang", lang},
		{"amount", pay.FormatMinor(r.Amount, r.Currency)},
		{"currencyCode", cur},
		{"installCount", inst},
		{"okUrl", r.ReturnURL},
		{"failUrl", failURL},
		{"randomNumber", rnd},
		{"requestDateTime", requestDateTime()},
	}
	m := make(map[string]string, len(order))
	for _, kv := range order {
		m[kv[0]] = kv[1]
	}
	fields := append(order, [2]string{"hash", formHash(p.secret, m)})
	return &pay.StartResult{
		Form:        &pay.Form{Action: p.gateway() + hostingPath, Fields: fields},
		ProviderRef: r.Ref,
	}, nil
}

func supported(cur string) bool {
	switch strings.ToUpper(cur) {
	case "TRY", "USD", "EUR", "GBP", "JPY", "RUB":
		return true
	}
	return false
}

func (p *provider) CallbackRef(in pay.CallbackInput) string { return in.Form.Get("orderId") }

// verifyCallback checks the callback hash: Base64(HMAC-SHA512(values of the fields named in
// hashParams, '+'-separated, concatenated)). Every name in need must be among the signed fields.
func (p *provider) verifyCallback(f map[string][]string, need ...string) bool {
	get := func(k string) string {
		if v := f[k]; len(v) > 0 {
			return v[0]
		}
		return ""
	}
	hash, params := get("hash"), get("hashParams")
	if hash == "" || params == "" {
		return false
	}
	signed := map[string]bool{}
	var b strings.Builder
	for _, k := range strings.Split(params, "+") {
		if k == "" {
			continue
		}
		signed[k] = true
		b.WriteString(get(k))
	}
	for _, k := range need {
		if !signed[k] {
			return false
		}
	}
	return pay.Equal(sign(p.secret, b.String()), hash)
}

func (p *provider) Complete(ctx context.Context, in pay.CallbackInput, a pay.Attempt) (*pay.Result, error) {
	f := in.Form
	if !p.verifyCallback(f, "responseCode", "orderId") {
		return nil, pay.ErrInvalidSignature
	}
	if f.Get("orderId") != a.Ref {
		return nil, errors.New("akbank: callback is about another payment")
	}
	code := f.Get("responseCode")
	if code != successCode {
		host := f.Get("hostResponseCode")
		msg := f.Get("hostMessage")
		if msg == "" {
			msg = f.Get("responseMessage")
		}
		pc := code
		if host != "" {
			pc = code + "/" + host
		}
		return &pay.Result{Status: pay.StatusFailure, Code: failureCode(code, host), ProviderCode: pc, ProviderMessage: msg}, nil
	}
	// An approval must also be signed for this terminal.
	if !p.verifyCallback(f, "responseCode", "orderId", "merchantSafeId", "terminalSafeId") ||
		f.Get("merchantSafeId") != p.merchantID || f.Get("terminalSafeId") != p.terminal {
		return nil, pay.ErrInvalidSignature
	}
	// The hash does not cover the amount: Akbank's own record of the order decides.
	res, err := p.Query(ctx, a)
	if err != nil {
		return nil, err
	}
	if res.Status != pay.StatusSuccess {
		return nil, fmt.Errorf("akbank: callback approval not confirmed by order history (%s)", res.Status)
	}
	if res.TxnID == "" {
		res.TxnID = f.Get("rrn")
	}
	return res, nil
}

type terminal struct {
	MerchantSafeID string `json:"merchantSafeId"`
	TerminalSafeID string `json:"terminalSafeId"`
}

type orderRef struct {
	OrderID string `json:"orderId"`
}

type amount struct {
	Amount       string `json:"amount"`
	CurrencyCode int    `json:"currencyCode"`
}

type request struct {
	Version         string   `json:"version"`
	TxnCode         string   `json:"txnCode"`
	RequestDateTime string   `json:"requestDateTime"`
	RandomNumber    string   `json:"randomNumber"`
	Terminal        terminal `json:"terminal"`
	Order           orderRef `json:"order"`
	Transaction     *amount  `json:"transaction,omitempty"`
}

type txnDetail struct {
	TxnCode         string          `json:"txnCode"`
	ResponseCode    string          `json:"responseCode"`
	ResponseMessage string          `json:"responseMessage"`
	TxnStatus       string          `json:"txnStatus"`
	OrderID         string          `json:"orderId"`
	Amount          json.RawMessage `json:"amount"`
	CurrencyCode    json.RawMessage `json:"currencyCode"`
	InstallCount    json.RawMessage `json:"installCount"`
	RRN             string          `json:"rrn"`
	AuthCode        string          `json:"authCode"`
}

type response struct {
	TxnCode          string          `json:"txnCode"`
	ResponseCode     string          `json:"responseCode"`
	ResponseMessage  string          `json:"responseMessage"`
	HostResponseCode string          `json:"hostResponseCode"`
	HostMessage      string          `json:"hostMessage"`
	Order            *orderRef       `json:"order"`
	Transaction      *txnDetail      `json:"transaction"`
	TxnDetailList    []txnDetail     `json:"txnDetailList"`
	Code             json.RawMessage `json:"code"`
	Message          string          `json:"message"`
}

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

// process posts a request to /transaction/process, signed in the auth-hash header.
func (p *provider) process(ctx context.Context, txnCode, orderID string, amt *amount) (*response, int, error) {
	rnd, err := randomNumber()
	if err != nil {
		return nil, 0, err
	}
	body, err := json.Marshal(request{
		Version: apiVersion, TxnCode: txnCode, RequestDateTime: requestDateTime(), RandomNumber: rnd,
		Terminal: terminal{MerchantSafeID: p.merchantID, TerminalSafeID: p.terminal},
		Order:    orderRef{OrderID: orderID}, Transaction: amt,
	})
	if err != nil {
		return nil, 0, err
	}
	h := http.Header{"auth-hash": {sign(p.secret, string(body))}}
	status, raw, err := pay.PostBody(ctx, p.cfg.HTTP, p.cfg.Origin(liveAPI, testAPI)+"/transaction/process", "application/json", body, h)
	if err != nil {
		return nil, status, err
	}
	var res response
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, status, fmt.Errorf("akbank: unreadable response (HTTP %d)", status)
	}
	return &res, status, nil
}

func (r *response) err() *pay.Error {
	code := r.ResponseCode
	if code == "" {
		code = text(r.Code)
	}
	msg := r.HostMessage
	if msg == "" {
		msg = r.ResponseMessage
	}
	if msg == "" {
		msg = r.Message
	}
	return &pay.Error{Code: failureCode(code, r.HostResponseCode), ProviderCode: code, ProviderMessage: msg}
}

// paymentTxn reports whether a txn code is a sale (3D or not).
func paymentTxn(c string) bool { return c == txnSale || c == txnSecureSale }

func (p *provider) Query(ctx context.Context, a pay.Attempt) (*pay.Result, error) {
	res, status, err := p.process(ctx, txnHistory, a.Ref, nil)
	if err != nil {
		return nil, err
	}
	if status >= 500 {
		return nil, pay.Errorf(pay.CodeProviderDown, strconv.Itoa(status), "order history failed")
	}
	if res.ResponseCode != successCode {
		return nil, res.err()
	}
	var sale *txnDetail
	for i := range res.TxnDetailList {
		tx := &res.TxnDetailList[i]
		if tx.OrderID != "" && tx.OrderID != a.Ref {
			continue
		}
		if tx.TxnCode != "" && !paymentTxn(tx.TxnCode) {
			continue
		}
		if sale == nil || tx.ResponseCode == successCode {
			sale = tx
		}
	}
	if sale == nil {
		return &pay.Result{Status: pay.StatusPending}, nil
	}
	if sale.ResponseCode != "" && sale.ResponseCode != successCode {
		return &pay.Result{Status: pay.StatusFailure, Code: failureCode(sale.ResponseCode, ""),
			ProviderCode: sale.ResponseCode, ProviderMessage: sale.ResponseMessage}, nil
	}
	cur := pay.CurrencyFromNumeric(text(sale.CurrencyCode))
	paid, err := pay.ParseMinor(text(sale.Amount), cur)
	if err != nil {
		return nil, err
	}
	out := &pay.Result{PaidAmount: paid, Currency: cur, TxnID: sale.RRN}
	if n, err := strconv.Atoi(text(sale.InstallCount)); err == nil && n > 1 {
		out.Installment = n
	}
	switch sale.TxnStatus {
	case "N":
		out.Status = pay.StatusSuccess
	case "V", "R":
		out.Status = pay.StatusCancelled
	case "S":
		out.Status = pay.StatusFailure
		out.Code = pay.CodeUnknown
	default:
		if sale.ResponseCode != successCode {
			return &pay.Result{Status: pay.StatusPending}, nil
		}
		out.Status = pay.StatusSuccess
	}
	return out, nil
}

func (p *provider) Refund(ctx context.Context, r pay.RefundRequest) (*pay.Result, error) {
	if r.Amount <= 0 {
		return nil, pay.Errorf(pay.CodeInvalidRequest, "", "refund amount must be positive")
	}
	if r.Full {
		res, err := p.call(ctx, txnVoid, r.Attempt.Ref, nil)
		if err != nil {
			return nil, err
		}
		if res.ResponseCode == successCode {
			return &pay.Result{Status: pay.StatusSuccess, PaidAmount: r.Amount, TxnID: rrn(res)}, nil
		}
		// A void is refused once the day is settled: refund the amount instead.
	}
	cur := pay.NumericCurrency(r.Attempt.Currency)
	n, _ := strconv.Atoi(cur)
	if n == 0 {
		return nil, pay.Errorf(pay.CodeInvalidRequest, "", "currency %s is not supported by Akbank", r.Attempt.Currency)
	}
	res, err := p.call(ctx, txnRefund, r.Attempt.Ref, &amount{Amount: pay.FormatMinor(r.Amount, r.Attempt.Currency), CurrencyCode: n})
	if err != nil {
		return nil, err
	}
	if res.ResponseCode != successCode {
		return nil, res.err()
	}
	return &pay.Result{Status: pay.StatusSuccess, PaidAmount: r.Amount, TxnID: rrn(res)}, nil
}

// call sends a money-moving request: any failure after it may have reached Akbank, or an unreadable
// answer, is ErrUnknownOutcome so it is never retried blindly.
func (p *provider) call(ctx context.Context, txnCode, orderID string, amt *amount) (*response, error) {
	res, status, err := p.process(ctx, txnCode, orderID, amt)
	if err != nil {
		if errors.Is(err, pay.ErrUnknownOutcome) || status != 0 {
			return nil, fmt.Errorf("%w: %v", pay.ErrUnknownOutcome, err)
		}
		return nil, err
	}
	if status >= 500 {
		return nil, fmt.Errorf("%w: HTTP %d", pay.ErrUnknownOutcome, status)
	}
	return res, nil
}

func rrn(r *response) string {
	if r.Transaction != nil {
		if r.Transaction.RRN != "" {
			return r.Transaction.RRN
		}
		return r.Transaction.AuthCode
	}
	return ""
}

// failureCode maps the ISO 8583 host response code when there is one, else Akbank's VPS code.
func failureCode(vps, host string) pay.Code {
	switch host {
	case "05", "57", "62", "01", "02":
		return pay.CodeCardDeclined
	case "51":
		return pay.CodeInsufficientFunds
	case "54", "33":
		return pay.CodeExpiredCard
	case "14", "15", "82":
		return pay.CodeInvalidCard
	case "61", "65":
		return pay.CodeLimitExceeded
	case "41", "43":
		return pay.CodeStolenOrLost
	case "59":
		return pay.CodeFraudSuspected
	case "83":
		return pay.CodeInstallment
	}
	switch vps {
	case "VPS-1279": // 3DS server verification failed
		return pay.Code3DSFailed
	}
	return pay.CodeUnknown
}
