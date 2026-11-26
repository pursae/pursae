// Package vakifbank is VakıfBank's common payment page (PayFlex "Ortak Ödeme" Common Payment v4): the
// server registers the transaction (RegisterTransaction) and receives a payment token, the customer
// types the card on the bank's page (CommonPaymentUrl?Ptkn=<token>), and the browser comes back to
// SuccessUrl / FailUrl with TransactionId and Ptkn (plus RC / MSG on failure). The return is
// informational only and unsigned, so the outcome is read server-side (VposTransaction).
//
// The merchant's TransactionId is the attempt Ref: Ortak Ödeme passes it on to the virtual POS as the
// sale's TransactionId, which makes it both the key of the result query and the reference of later
// cancels and refunds (VPOS 7/24 service, Vposreq.aspx, ReferenceTransactionId).
//
// Sources, checked 2026-11-27 against the official material linked from
// https://sanalpos.innova.com.tr/vakifbankasi/ (Innova PayFlex, VakıfBank's virtual POS vendor):
//   - PHP_v4.rar / ASP_v4.rar (https://sanalpos.innova.com.tr/vakifbankasi/Dosyalar/), samples
//     ORTAK_ODEME/SampleCommonPaymentRegistration.* and SampleResultPage.*, POS/satis.php;
//   - "PayFlex Common Payment ISD v4.0" (Ortak Ödeme Entegrasyon Dokümanı, in both archives);
//   - "PayFlex Sanal Pos ISD v4.0" (VPOS XML İşlem Tanımları, Cancel and Refund).
//
// The official material names no live hosts and its samples use Innova's test host; the hosts below
// come from mews/pos (MIT, github.com/mewebstudio/pos, PayFlexCPV4Pos).
package vakifbank

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"wpgo/internal/pay"
)

const (
	liveCP   = "https://cpweb.vakifbank.com.tr"
	testCP   = "https://cptest.vakifbank.com.tr"
	cpPath   = "/CommonPayment/api"
	liveVPOS = "https://onlineodeme.vakifbank.com.tr:4443"
	testVPOS = "https://onlineodemetest.vakifbank.com.tr:4443"
	vposPath = "/VposService/v3/Vposreq.aspx"

	successCode = "0000"
)

// Spec describes the VakıfBank provider.
func Spec() pay.Spec {
	return pay.Spec{
		ID:    "vakifbank",
		Title: "VakıfBank",
		Description: "Card payments on VakıfBank's common payment page (PayFlex Ortak Ödeme). " +
			"The terminal must be enabled for common payment.",
		Region:     pay.RegionTR,
		Kind:       pay.KindBank,
		Features:   []pay.Feature{pay.Feature3DS, pay.FeatureInstallments, pay.FeatureRefunds},
		Currencies: []string{"TRY", "USD", "EUR", "GBP"},
		Fields: []pay.Field{
			{ID: "merchant_id", Title: "Merchant ID", Type: "text", Required: true,
				Desc: "Üye İşyeri No (HostMerchantId), from VakıfBank."},
			{ID: "password", Title: "Merchant password", Type: "password", Secret: true, Required: true,
				Desc: "Üye İşyeri Şifresi, set in the VakıfBank virtual POS panel."},
			{ID: "terminal_no", Title: "Terminal number", Type: "text", Required: true,
				Desc: "Terminal No (HostTerminalId), for example VP000579."},
		},
		DocsURL: "https://sanalpos.innova.com.tr/vakifbankasi/",
		New:     newProvider,
	}
}

type provider struct {
	cfg                pay.Config
	merchant, terminal string
	password           string
}

func newProvider(cfg pay.Config) (pay.Provider, error) {
	if err := cfg.Require("merchant_id", "password", "terminal_no"); err != nil {
		return nil, err
	}
	return &provider{cfg: cfg, merchant: cfg.Value("merchant_id"), password: cfg.Value("password"), terminal: cfg.Value("terminal_no")}, nil
}

func (p *provider) cp(op string) string { return p.cfg.Origin(liveCP, testCP) + cpPath + "/" + op }

func (p *provider) vpos() string { return p.cfg.Origin(liveVPOS, testVPOS) + vposPath }

func supported(cur string) bool {
	switch strings.ToUpper(cur) {
	// Amount must be "xxx.xx" (the guide's 5014 note), which a zero-decimal currency would not give.
	case "TRY", "USD", "EUR", "GBP":
		return true
	}
	return false
}

// leaves reads the text of every leaf element of an XML answer by name (first occurrence wins).
func leaves(body []byte) (map[string]string, error) {
	d := xml.NewDecoder(bytes.NewReader(body))
	d.CharsetReader = func(_ string, r io.Reader) (io.Reader, error) { return r, nil }
	out := map[string]string{}
	var name string
	var text strings.Builder
	root := false
	for {
		tok, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			root = true
			name = t.Name.Local
			text.Reset()
		case xml.CharData:
			text.Write(t)
		case xml.EndElement:
			if name == t.Name.Local {
				if _, ok := out[name]; !ok {
					out[name] = strings.TrimSpace(text.String())
				}
			}
			name = ""
		}
	}
	if !root {
		return nil, errors.New("vakifbank: empty response")
	}
	return out, nil
}

func (p *provider) post(ctx context.Context, endpoint string, form url.Values) (map[string]string, int, error) {
	status, body, err := pay.PostForm(ctx, p.cfg.HTTP, endpoint, form, http.Header{"Accept": {"application/xml"}})
	if err != nil {
		return nil, status, err
	}
	f, err := leaves(body)
	if err != nil {
		return nil, status, fmt.Errorf("vakifbank: unreadable response (HTTP %d)", status)
	}
	return f, status, nil
}

func (p *provider) Start(ctx context.Context, r pay.StartRequest) (*pay.StartResult, error) {
	code := pay.NumericCurrency(r.Currency)
	if !supported(r.Currency) || code == "" {
		return nil, pay.Errorf(pay.CodeInvalidRequest, "", "currency %s is not supported by VakıfBank", r.Currency)
	}
	if r.Amount <= 0 {
		return nil, pay.Errorf(pay.CodeInvalidRequest, "", "empty payment")
	}
	amount := pay.FormatMinor(r.Amount, r.Currency)
	lang := "tr-TR"
	if strings.HasPrefix(strings.ToLower(r.Locale), "en") {
		lang = "en-US"
	}
	failURL := r.FailURL
	if failURL == "" {
		failURL = r.ReturnURL
	}
	form := url.Values{
		"HostMerchantId":       {p.merchant},
		"MerchantPassword":     {p.password},
		"HostTerminalId":       {p.terminal},
		"TransactionType":      {"Sale"},
		"AmountCode":           {code},
		"Amount":               {amount},
		"TransactionId":        {r.Ref},
		"OrderID":              {r.Ref},
		"IsSecure":             {"true"},
		"AllowNotEnrolledCard": {"false"},
		"SuccessUrl":           {r.ReturnURL},
		"FailUrl":              {failURL},
		"RequestLanguage":      {lang},
	}
	if r.Installment > 1 {
		form.Set("InstallmentCount", strconv.Itoa(r.Installment))
	}
	f, status, err := p.post(ctx, p.cp("RegisterTransaction"), form)
	if err != nil {
		return nil, err
	}
	if e := f["ErrorCode"]; e != "" {
		return nil, &pay.Error{Code: pay.CodeInvalidRequest, ProviderCode: e, ProviderMessage: f["ResponseMessage"]}
	}
	token, page := f["PaymentToken"], f["CommonPaymentUrl"]
	if !safeToken(token) || !bankURL(page) {
		return nil, pay.Errorf(pay.CodeProviderDown, strconv.Itoa(status), "unexpected RegisterTransaction response")
	}
	return &pay.StartResult{Redirect: page + "?Ptkn=" + url.QueryEscape(token), ProviderRef: token}, nil
}

func safeToken(t string) bool {
	if t == "" || len(t) > 128 {
		return false
	}
	for _, c := range t {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

// bankURL keeps the redirect on VakıfBank's own HTTPS hosts.
func bankURL(s string) bool {
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "https" || u.RawQuery != "" || u.User != nil {
		return false
	}
	h := u.Hostname()
	return h == "vakifbank.com.tr" || strings.HasSuffix(h, ".vakifbank.com.tr")
}

// CallbackRef is "": the return carries only VakıfBank's token and transaction id, and the bridge
// routes it by its own URL.
func (p *provider) CallbackRef(pay.CallbackInput) string { return "" }

// param reads a return field; the guide shows a GET query string, any form body is read too.
func param(in pay.CallbackInput, keys ...string) string {
	for _, k := range keys {
		if v := in.Form.Get(k); v != "" {
			return v
		}
		if v := in.Query.Get(k); v != "" {
			return v
		}
	}
	return ""
}

// lookup asks Ortak Ödeme's query API for the attempt (TransactionId is the attempt Ref).
func (p *provider) lookup(ctx context.Context, a pay.Attempt) (map[string]string, error) {
	form := url.Values{"HostMerchantId": {p.merchant}, "Password": {p.password}, "TransactionId": {a.Ref}}
	if a.ProviderRef != "" {
		form.Set("PaymentToken", a.ProviderRef)
	}
	f, status, err := p.post(ctx, p.cp("VposTransaction"), form)
	if err != nil {
		return nil, err
	}
	if status >= 500 {
		return nil, pay.Errorf(pay.CodeProviderDown, strconv.Itoa(status), "VposTransaction failed")
	}
	return f, nil
}

// notFound is the query's ErrorCode for a transaction that never reached the virtual POS.
const notFound = "5003"

func (p *provider) Complete(ctx context.Context, in pay.CallbackInput, a pay.Attempt) (*pay.Result, error) {
	token := param(in, "Ptkn", "PaymentToken")
	if token == "" || !pay.Equal(token, a.ProviderRef) {
		return nil, errors.New("vakifbank: callback is about another payment")
	}
	if txn := param(in, "TransactionId"); txn != "" && txn != a.Ref {
		return nil, errors.New("vakifbank: callback is about another transaction")
	}
	f, err := p.lookup(ctx, a)
	if err != nil {
		return nil, err
	}
	if e := f["ErrorCode"]; e == notFound {
		// The customer was sent back without a virtual POS transaction: nothing was charged.
		rc := param(in, "RC", "Rc")
		if rc == "" {
			rc = e
		}
		return &pay.Result{Status: pay.StatusFailure, Code: failureCode(rc), ProviderCode: rc, ProviderMessage: param(in, "MSG", "Message")}, nil
	}
	return p.outcome(f, a)
}

// Query reads the attempt's state from the same query API; a transaction that has not reached the
// virtual POS yet is still pending.
func (p *provider) Query(ctx context.Context, a pay.Attempt) (*pay.Result, error) {
	f, err := p.lookup(ctx, a)
	if err != nil {
		return nil, err
	}
	if f["ErrorCode"] == notFound {
		return &pay.Result{Status: pay.StatusPending}, nil
	}
	return p.outcome(f, a)
}

// outcome turns a VposQueryResponse into a result. Only Rc "0000" is approved; the answer must name
// this attempt's TransactionId and carry the charged amount and currency.
func (p *provider) outcome(f map[string]string, a pay.Attempt) (*pay.Result, error) {
	if e := f["ErrorCode"]; e != "" {
		return nil, &pay.Error{Code: pay.CodeUnknown, ProviderCode: e, ProviderMessage: f["Message"]}
	}
	if pt := f["PaymentToken"]; pt != "" && !pay.Equal(pt, a.ProviderRef) {
		return nil, errors.New("vakifbank: result is about another payment")
	}
	if f["TransactionId"] != a.Ref {
		return nil, errors.New("vakifbank: result is about another transaction")
	}
	rc := f["Rc"]
	if rc == "" {
		return nil, errors.New("vakifbank: result without Rc")
	}
	if rc != successCode {
		return &pay.Result{Status: pay.StatusFailure, Code: failureCode(rc), ProviderCode: rc, ProviderMessage: f["Message"]}, nil
	}
	if o := f["OrderID"]; o != "" && o != a.Ref {
		return nil, errors.New("vakifbank: result is about another order")
	}
	if m := f["HostMerchantId"]; m != "" && m != p.merchant {
		return nil, errors.New("vakifbank: result is about another merchant")
	}
	res := &pay.Result{Status: pay.StatusSuccess, TxnID: a.Ref}
	// The guide's short success answer carries no amount. The bank did approve this transaction, so
	// this is a success without a confirmed amount: the bridge then holds the order for the merchant
	// to review instead of leaving a charged customer on an unpaid order.
	if f["Amount"] != "" || f["AmountCode"] != "" {
		cur := pay.CurrencyFromNumeric(f["AmountCode"])
		if cur == "" {
			return nil, fmt.Errorf("vakifbank: unknown currency code %q", f["AmountCode"])
		}
		paid, err := pay.ParseMinor(f["Amount"], cur)
		if err != nil {
			return nil, err
		}
		res.PaidAmount, res.Currency = paid, cur
	}
	if n, err := strconv.Atoi(f["InstallmentCount"]); err == nil && n > 1 {
		res.Installment = n
	}
	return res, nil
}

// vposRequest is the VPOS 7/24 body: prmstr=<VposRequest>…</VposRequest>. TransactionDeviceSource
// (0: e-commerce) and Location (1: internet) are listed as required for every request.
func (p *provider) vposRequest(fields [][2]string) url.Values {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="utf-8"?><VposRequest>`)
	all := append([][2]string{{"MerchantId", p.merchant}, {"Password", p.password}, {"TerminalNo", p.terminal},
		{"TransactionDeviceSource", "0"}, {"Location", "1"}}, fields...)
	for _, kv := range all {
		if kv[0] == "" {
			continue
		}
		b.WriteString("<" + kv[0] + ">")
		xml.EscapeText(&b, []byte(kv[1]))
		b.WriteString("</" + kv[0] + ">")
	}
	b.WriteString("</VposRequest>")
	return url.Values{"prmstr": {b.String()}}
}

// money sends a cancel or refund. Anything but a readable answer is ErrUnknownOutcome: the request
// may have been processed.
func (p *provider) money(ctx context.Context, fields [][2]string) (map[string]string, error) {
	f, status, err := p.post(ctx, p.vpos(), p.vposRequest(fields))
	if err != nil {
		if errors.Is(err, pay.ErrUnknownOutcome) || status != 0 {
			return nil, fmt.Errorf("%w: %v", pay.ErrUnknownOutcome, err)
		}
		return nil, err
	}
	if status >= 500 || f["ResultCode"] == "" {
		return nil, fmt.Errorf("%w: unexpected VPOS response (HTTP %d)", pay.ErrUnknownOutcome, status)
	}
	return f, nil
}

func (p *provider) Refund(ctx context.Context, r pay.RefundRequest) (*pay.Result, error) {
	if r.Amount <= 0 {
		return nil, pay.Errorf(pay.CodeInvalidRequest, "", "refund amount must be positive")
	}
	ref := r.Attempt.TxnID
	if ref == "" {
		return nil, pay.Errorf(pay.CodeInvalidRequest, "", "the payment has no VakıfBank transaction id")
	}
	// Each cancel or refund needs its own unique TransactionId (VPOS makes one up when none is sent);
	// the cancel tried first gets a suffix so the refund that may follow it does not reuse the id.
	own := func(suffix string) [2]string { return [2]string{"TransactionId", r.RefundRef + suffix} }
	if r.RefundRef == "" {
		own = func(string) [2]string { return [2]string{} }
	}
	if r.Full {
		f, err := p.money(ctx, [][2]string{{"TransactionType", "Cancel"}, own("C"), {"ReferenceTransactionId", ref}})
		if err != nil {
			return nil, err
		}
		if f["ResultCode"] == successCode {
			return &pay.Result{Status: pay.StatusSuccess, PaidAmount: r.Amount, TxnID: f["TransactionId"]}, nil
		}
		// A cancel is refused after end of day: refund the amount instead.
	}
	f, err := p.money(ctx, [][2]string{
		{"TransactionType", "Refund"},
		own(""),
		{"ReferenceTransactionId", ref},
		{"CurrencyAmount", pay.FormatMinor(r.Amount, r.Attempt.Currency)},
		{"CurrencyCode", pay.NumericCurrency(r.Attempt.Currency)},
	})
	if err != nil {
		return nil, err
	}
	if f["ResultCode"] != successCode {
		return nil, &pay.Error{Code: failureCode(f["ResultCode"]), ProviderCode: f["ResultCode"], ProviderMessage: f["ResultDetail"]}
	}
	return &pay.Result{Status: pay.StatusSuccess, PaidAmount: r.Amount, TxnID: f["TransactionId"]}, nil
}

// failureCode maps VakıfBank result codes: the ISO 8583 response code, which the query and the
// return show without padding ("51") and VPOS with two leading zeros ("0051").
func failureCode(c string) pay.Code {
	if len(c) < 4 {
		c = strings.Repeat("0", 4-len(c)) + c
	}
	switch c {
	case "0001", "0005", "0057", "0062":
		return pay.CodeCardDeclined
	case "0051":
		return pay.CodeInsufficientFunds
	case "0054":
		return pay.CodeExpiredCard
	case "9093":
		return pay.CodeProviderDown
	}
	return pay.CodeUnknown
}
