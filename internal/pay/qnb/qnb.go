// Package qnb is the QNB Türkiye (Finansbank) virtual POS on the PayFor gateway, in its 3D Host mode
// (SecureType "3DHost"): the customer's browser posts a signed form without card fields to the bank,
// the bank shows its own card page, authenticates the card with 3D Secure, charges it and posts the
// result back to OkUrl / FailUrl. No field of this package holds card data.
//
// Sources: better-payment (MIT) src/providers/qnb for the XML API (OrderInquiry, Void, Refund), the
// form hash and the ResponseHash; mews/pos (MIT) for the 3DHost gate and its field set:
// https://github.com/mewebstudio/pos/blob/master/src/DataMapper/Request/Mapper/PayForPosRequestDataMapper.php,
// https://github.com/mewebstudio/pos/blob/master/src/Crypt/PayForPosCrypt.php,
// https://github.com/mewebstudio/pos/blob/master/config/pos_production.php.
package qnb

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"wpgo/internal/pay"
)

const (
	liveOrigin = "https://vpos.qnb.com.tr"
	testOrigin = "https://vpostest.qnb.com.tr"

	gatePath = "/Gateway/3DHost.aspx"  // 3D Host gate: the bank's card page
	apiPath  = "/Gateway/XMLGate.aspx" // XML API (PayforRequest)

	// mbrID is the institution code of QNB on the PayFor platform.
	mbrID = "5"
)

// Spec describes the QNB provider.
func Spec() pay.Spec {
	return pay.Spec{
		ID:    "qnb",
		Title: "QNB",
		Description: "QNB virtual POS (PayFor, 3D Host): the customer types the card on QNB's own " +
			"3D Secure payment page.",
		Region:     pay.RegionTR,
		Kind:       pay.KindBank,
		Features:   []pay.Feature{pay.Feature3DS, pay.FeatureInstallments, pay.FeatureRefunds, pay.FeatureQuery},
		Currencies: []string{"TRY", "USD", "EUR"},
		Fields: []pay.Field{
			{ID: "merchant_id", Title: "Merchant ID", Type: "text", Required: true,
				Desc: "Üye işyeri numarası (MerchantId), from QNB."},
			{ID: "user_code", Title: "API user code", Type: "text", Required: true,
				Desc: "UserCode of the virtual POS API user."},
			{ID: "user_pass", Title: "API user password", Type: "password", Secret: true, Required: true,
				Desc: "UserPass of the API user, used for inquiries and refunds; never sent to the browser."},
			{ID: "merchant_pass", Title: "3D Secure key", Type: "password", Secret: true, Required: true,
				Desc: "MerchantPass (üye işyeri 3D anahtarı), used only to sign and verify; never sent."},
		},
		Hosts:   pay.Hosts{Form: []string{liveOrigin, testOrigin}},
		DocsURL: "https://github.com/mewebstudio/pos",
		New:     newProvider,
	}
}

type provider struct {
	cfg          pay.Config
	merchantID   string
	userCode     string
	userPass     string
	merchantPass string
	origin       string
}

func newProvider(c pay.Config) (pay.Provider, error) {
	if err := c.Require("merchant_id", "user_code", "user_pass", "merchant_pass"); err != nil {
		return nil, err
	}
	return &provider{
		cfg:        c,
		merchantID: c.Value("merchant_id"), userCode: c.Value("user_code"),
		userPass: c.Value("user_pass"), merchantPass: c.Value("merchant_pass"),
		origin: c.Origin(liveOrigin, testOrigin),
	}, nil
}

// sha1Base64 is PayFor's hash: Base64(SHA1(s)).
func sha1Base64(s string) string {
	h := sha1.Sum([]byte(s))
	return base64.StdEncoding.EncodeToString(h[:])
}

// FormHash is the hash of the 3D form:
// Base64(SHA1(MbrId + OrderId + PurchAmount + OkUrl + FailUrl + TxnType + InstallmentCount + Rnd + MerchantPass)).
// 3DHost uses the same formula as 3DModel / 3DPay; there is no card field in it.
func FormHash(orderID, amount, okURL, failURL, txnType, installment, rnd, merchantPass string) string {
	return sha1Base64(mbrID + orderID + amount + okURL + failURL + txnType + installment + rnd + merchantPass)
}

// ResponseHash is the hash of the bank's POST to OkUrl / FailUrl:
// Base64(SHA1(MerchantID + MerchantPass + OrderId + AuthCode + ProcReturnCode + 3DStatus + ResponseRnd + UserCode)),
// with the merchant id and user code from the settings.
func ResponseHash(merchantID, merchantPass, orderID, authCode, procReturnCode, status3D, responseRnd, userCode string) string {
	return sha1Base64(merchantID + merchantPass + orderID + authCode + procReturnCode + status3D + responseRnd + userCode)
}

func lang(locale string) string {
	if strings.HasPrefix(strings.ToLower(locale), "en") {
		return "EN"
	}
	return "TR"
}

func currencyCode(iso string) string {
	switch strings.ToUpper(iso) {
	case "TRY", "USD", "EUR":
		return pay.NumericCurrency(iso)
	}
	return ""
}

// installmentCount is InstallmentCount: "0" for a single payment. It is part of the form hash, so it
// is always sent.
func installmentCount(n int) string {
	if n > 1 {
		return strconv.Itoa(n)
	}
	return "0"
}

// Start builds the 3D Host form: the field set of mews/pos create3DFormData without a card.
func (p *provider) Start(ctx context.Context, r pay.StartRequest) (*pay.StartResult, error) {
	cur := currencyCode(r.Currency)
	if cur == "" {
		return nil, pay.Errorf(pay.CodeInvalidRequest, "", "currency %s is not supported", r.Currency)
	}
	if r.Amount <= 0 {
		return nil, pay.Errorf(pay.CodeInvalidRequest, "", "amount must be positive")
	}
	failURL := r.FailURL
	if failURL == "" {
		failURL = r.ReturnURL
	}
	b := make([]byte, 10)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	rnd := hex.EncodeToString(b)
	amount := pay.FormatMinor(r.Amount, r.Currency)
	inst := installmentCount(r.Installment)
	fields := [][2]string{
		{"MbrId", mbrID},
		{"MerchantID", p.merchantID},
		{"UserCode", p.userCode},
		{"OrderId", r.Ref},
		{"Lang", lang(r.Locale)},
		{"SecureType", "3DHost"},
		{"TxnType", "Auth"},
		{"PurchAmount", amount},
		{"InstallmentCount", inst},
		{"Currency", cur},
		{"OkUrl", r.ReturnURL},
		{"FailUrl", failURL},
		{"Rnd", rnd},
		{"Hash", FormHash(r.Ref, amount, r.ReturnURL, failURL, "Auth", inst, rnd, p.merchantPass)},
	}
	return &pay.StartResult{Form: &pay.Form{Action: p.origin + gatePath, Fields: fields}}, nil
}

// CallbackRef reads the order id the bank posts back.
func (p *provider) CallbackRef(in pay.CallbackInput) string {
	return strings.TrimSpace(in.Form.Get("OrderId"))
}

// Complete verifies the bank's POST to OkUrl / FailUrl.
//
// The ResponseHash covers OrderId, AuthCode, ProcReturnCode and 3DStatus but not the amount, and joins
// its values without a separator, so a valid hash alone does not pin each value. As better-payment
// does for 3DPay (the bank has charged the card in both modes), an approved callback is confirmed
// with OrderInquiry, whose answer gives the amount and the transaction reference.
func (p *provider) Complete(ctx context.Context, in pay.CallbackInput, a pay.Attempt) (*pay.Result, error) {
	f := func(k string) string { return in.Form.Get(k) }
	if f("ResponseHash") == "" || f("ResponseRnd") == "" || f("OrderId") == "" {
		return nil, pay.ErrInvalidSignature
	}
	want := ResponseHash(p.merchantID, p.merchantPass, f("OrderId"), f("AuthCode"), f("ProcReturnCode"),
		f("3DStatus"), f("ResponseRnd"), p.userCode)
	if !pay.Equal(want, f("ResponseHash")) {
		return nil, pay.ErrInvalidSignature
	}
	if m := in.Form.Get("MerchantID"); m != "" && m != p.merchantID {
		return nil, errors.New("pay: QNB callback names another merchant")
	}
	if f("OrderId") != a.Ref {
		return nil, errors.New("pay: QNB callback is about another order")
	}
	code := f("ProcReturnCode")
	if f("3DStatus") != "1" {
		return &pay.Result{Status: pay.StatusFailure, Code: pay.Code3DSFailed,
			ProviderCode: code, ProviderMessage: f("ErrMsg")}, nil
	}
	if code != "00" {
		return &pay.Result{Status: pay.StatusFailure, Code: mapCode(code),
			ProviderCode: code, ProviderMessage: f("ErrMsg")}, nil
	}

	inq, err := p.orderInquiry(ctx, a.Ref, a.Currency)
	if err != nil {
		return nil, err
	}
	if !capturedSale(inq, a.Ref) {
		return nil, errors.New("pay: QNB callback does not match the order at the bank")
	}
	cur := pay.CurrencyFromNumeric(inq["Currency"])
	if cur == "" {
		cur = a.Currency
	}
	paid, err := pay.ParseMinor(inq["PurchAmount"], cur)
	if err != nil {
		return nil, err
	}
	txn := inq["HostRefNum"]
	if txn == "" {
		txn = f("AuthCode")
	}
	return &pay.Result{Status: pay.StatusSuccess, TxnID: txn, PaidAmount: paid,
		Currency: pay.CurrencyFromNumeric(inq["Currency"]), Installment: installments(inq["InstallmentCount"]),
		ProviderCode: code, ProviderMessage: "AuthCode " + f("AuthCode")}, nil
}

// Query asks the bank for the order (OrderInquiry).
func (p *provider) Query(ctx context.Context, a pay.Attempt) (*pay.Result, error) {
	inq, err := p.orderInquiry(ctx, a.Ref, a.Currency)
	if err != nil {
		return nil, err
	}
	code := inq["ProcReturnCode"]
	r := &pay.Result{TxnID: inq["HostRefNum"], ProviderCode: code, Currency: pay.CurrencyFromNumeric(inq["Currency"])}
	if code != "00" {
		r.Status, r.Code, r.ProviderMessage = pay.StatusFailure, mapCode(code), inq["ErrMsg"]
		return r, nil
	}
	if id := orderOf(inq); id != "" && id != a.Ref {
		return nil, errors.New("pay: QNB inquiry answered about another order")
	}
	r.Status = orderStatus(inq)
	switch r.Status {
	case pay.StatusSuccess:
		cur := r.Currency
		if cur == "" {
			cur = a.Currency
		}
		if n, err := pay.ParseMinor(inq["PurchAmount"], cur); err == nil {
			r.PaidAmount = n
		}
		r.Installment = installments(inq["InstallmentCount"])
	case pay.StatusFailure:
		r.Code, r.ProviderMessage = pay.CodeCardDeclined, inq["ErrMsg"]
	}
	return r, nil
}

// Refund voids a full refund (same-day cancel) and falls back to Refund when the bank declines the
// Void (the sale has settled); a partial refund is a Refund. better-payment offers Void (cancel) and
// Refund separately; the fallback is this package's.
func (p *provider) Refund(ctx context.Context, r pay.RefundRequest) (*pay.Result, error) {
	cur := currencyCode(r.Attempt.Currency)
	if cur == "" {
		return nil, pay.Errorf(pay.CodeInvalidRequest, "", "currency %s is not supported", r.Attempt.Currency)
	}
	ref := r.Attempt.Ref
	if r.Full {
		res, err := p.api(ctx, [][2]string{
			{"OrgOrderId", ref}, {"SecureType", "NonSecure"}, {"TxnType", "Void"}, {"Currency", cur},
		})
		if err != nil {
			return nil, refundErr(err)
		}
		if res["ProcReturnCode"] == "00" {
			return &pay.Result{Status: pay.StatusSuccess, TxnID: res["HostRefNum"], PaidAmount: r.Amount,
				Currency: r.Attempt.Currency, ProviderCode: "00", ProviderMessage: "void"}, nil
		}
	}
	res, err := p.api(ctx, [][2]string{
		{"OrgOrderId", ref}, {"SecureType", "NonSecure"}, {"TxnType", "Refund"},
		{"PurchAmount", pay.FormatMinor(r.Amount, r.Attempt.Currency)}, {"Currency", cur},
	})
	if err != nil {
		return nil, refundErr(err)
	}
	if code := res["ProcReturnCode"]; code != "00" {
		return &pay.Result{Status: pay.StatusFailure, Code: mapCode(code), ProviderCode: code,
			ProviderMessage: res["ErrMsg"]}, nil
	}
	return &pay.Result{Status: pay.StatusSuccess, TxnID: res["HostRefNum"], PaidAmount: r.Amount,
		Currency: r.Attempt.Currency, ProviderCode: "00", ProviderMessage: "refund"}, nil
}

// refundErr turns any failure after the request may have reached the bank (unreadable answer, HTTP
// error) into ErrUnknownOutcome; a refund is never retried blindly.
func refundErr(err error) error {
	if errors.Is(err, pay.ErrUnknownOutcome) || errors.Is(err, errNotSent) {
		return err
	}
	return fmt.Errorf("%w: %v", pay.ErrUnknownOutcome, err)
}

var errNotSent = errors.New("pay: QNB request not sent")

// orderInquiry is better-payment's OrderInquiry, with the attempt's currency.
func (p *provider) orderInquiry(ctx context.Context, ref, currency string) (map[string]string, error) {
	cur := currencyCode(currency)
	if cur == "" {
		cur = "949"
	}
	return p.api(ctx, [][2]string{
		{"OrgOrderId", ref}, {"SecureType", "Inquiry"}, {"TxnType", "OrderInquiry"}, {"Currency", cur},
	})
}

// orderOf is the order id an inquiry answer names, "" when it names none.
func orderOf(inq map[string]string) string {
	if v := inq["OrgOrderId"]; v != "" {
		return v
	}
	return inq["OrderId"]
}

// orderStatus maps an OrderInquiry answer: TxnStatus Y with TxnResult Success is a sale, V or
// IsVoided / IsRefunded a cancelled or refunded one (better-payment mapQnbOrderStatus).
func orderStatus(inq map[string]string) pay.Status {
	switch {
	case inq["IsVoided"] == "true", inq["IsRefunded"] == "true", inq["TxnStatus"] == "V":
		return pay.StatusCancelled
	case inq["TxnStatus"] == "Y" && inq["TxnResult"] == "Success":
		return pay.StatusSuccess
	case inq["TxnResult"] == "Failed":
		return pay.StatusFailure
	}
	return pay.StatusPending
}

// capturedSale is an approved, not cancelled sale of order ref.
func capturedSale(inq map[string]string, ref string) bool {
	if inq["ProcReturnCode"] != "00" || orderStatus(inq) != pay.StatusSuccess {
		return false
	}
	id := orderOf(inq)
	return id == "" || id == ref
}

func installments(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 1 {
		return 0
	}
	return n
}

// api sends a PayforRequest with the API user's credentials and parses the PayforResponse.
func (p *provider) api(ctx context.Context, fields [][2]string) (map[string]string, error) {
	all := append([][2]string{
		{"MbrId", mbrID}, {"MerchantId", p.merchantID}, {"UserCode", p.userCode}, {"UserPass", p.userPass},
	}, fields...)
	all = append(all, [2]string{"Lang", "TR"})
	var b bytes.Buffer
	b.WriteString(`<?xml version="1.0" encoding="utf-8"?><PayforRequest>`)
	for _, kv := range all {
		b.WriteString("<" + kv[0] + ">")
		_ = xml.EscapeText(&b, []byte(kv[1]))
		b.WriteString("</" + kv[0] + ">")
	}
	b.WriteString("</PayforRequest>")
	status, out, err := pay.PostBody(ctx, p.cfg.HTTP, p.origin+apiPath, "application/xml", b.Bytes(), nil)
	if err != nil {
		if errors.Is(err, pay.ErrUnknownOutcome) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %v", errNotSent, err)
	}
	if status < 200 || status > 299 {
		return nil, fmt.Errorf("pay: QNB API answered HTTP %d", status)
	}
	return parseResponse(out)
}

// parseResponse reads the child elements of a PayforResponse into a map (first value of each name,
// trimmed). encoding/xml resolves no external entities.
func parseResponse(body []byte) (map[string]string, error) {
	var raw struct {
		XMLName xml.Name `xml:"PayforResponse"`
		Items   []struct {
			XMLName xml.Name
			Value   string `xml:",chardata"`
		} `xml:",any"`
	}
	d := xml.NewDecoder(bytes.NewReader(body))
	d.CharsetReader = func(_ string, in io.Reader) (io.Reader, error) { return in, nil }
	if err := d.Decode(&raw); err != nil {
		return nil, errors.New("pay: QNB returned an unexpected response")
	}
	m := make(map[string]string, len(raw.Items))
	for _, it := range raw.Items {
		if _, ok := m[it.XMLName.Local]; !ok {
			m[it.XMLName.Local] = strings.TrimSpace(it.Value)
		}
	}
	return m, nil
}

// mapCode maps PayFor return codes: ISO 8583 codes, plus better-payment's QNB additions.
func mapCode(c string) pay.Code {
	switch strings.TrimSpace(c) {
	case "01", "02", "05", "12", "57", "62":
		return pay.CodeCardDeclined
	case "03", "58":
		return pay.CodeConfiguration
	case "04", "07", "59":
		return pay.CodeFraudSuspected
	case "41", "43":
		return pay.CodeStolenOrLost
	case "13", "V013", "V014":
		return pay.CodeInvalidRequest
	case "14", "15", "82":
		return pay.CodeInvalidCard
	case "33", "54":
		return pay.CodeExpiredCard
	case "51":
		return pay.CodeInsufficientFunds
	case "61", "65":
		return pay.CodeLimitExceeded
	case "91", "96", "M030":
		return pay.CodeProviderDown
	case "94":
		return pay.CodeDuplicate
	case "V034":
		return pay.Code3DSFailed
	}
	return pay.CodeUnknown
}
