// Package garanti is Garanti BBVA's virtual POS (GVP) as a hosted payment: GarantiPay.
//
// The customer is sent to Garanti's GarantiPay page (secure3dsecuritylevel CUSTOM_PAY, txntype
// gpdatarequest) and pays there with the Garanti BBVA Mobile or BonusFlaş app; no card field is ever
// sent from the store. Garanti charges the payment itself and posts the result back to successurl /
// errorurl. The callback's hash is checked, and the payment is proven by an order inquiry (orderinq)
// on the XML API, whose answer gives the amount actually charged.
//
// Garanti's other models are not offered: 3D and 3D_PAY post the card fields from the store's page,
// and the common payment page (OOS_PAY / 3D_OOS_PAY) could not be confirmed as a card-free form.
package garanti

import (
	"bytes"
	"context"
	"crypto/sha1" //nolint:gosec // G505: SHA-1 is what the bank protocol signs with, not our choice
	"crypto/sha512"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"hash"
	"strconv"
	"strings"
	"time"

	"wpgo/internal/pay"
)

const (
	liveOrigin = "https://sanalposprov.garanti.com.tr"
	testOrigin = "https://sanalposprovtest.garantibbva.com.tr"

	apiPath  = "/VPServlet"          // XML API (GVPSRequest)
	gatePath = "/servlet/gt3dengine" // browser form target

	apiVersion = "512"
	provUser   = "PROVAUT" // provision user: sales, inquiries
	refundUser = "PROVRFN" // cancel and refund user
)

// Spec describes the provider.
func Spec() pay.Spec {
	return pay.Spec{
		ID:    "garanti",
		Title: "GarantiPay (Garanti BBVA)",
		Description: "GarantiPay on Garanti BBVA's own page: the customer pays with the Garanti BBVA " +
			"Mobile or BonusFlaş app; no card details are entered in the store.",
		Region:     pay.RegionTR,
		Kind:       pay.KindBank,
		Features:   []pay.Feature{pay.Feature3DS, pay.FeatureRefunds, pay.FeatureQuery},
		Currencies: []string{"TRY", "USD", "EUR", "GBP", "JPY"},
		Fields: []pay.Field{
			{ID: "merchant_id", Title: "Merchant ID", Type: "text", Required: true,
				Desc: "Üye işyeri numarası, from the Garanti BBVA virtual POS panel."},
			{ID: "terminal_id", Title: "Terminal ID", Type: "text", Required: true,
				Desc: "Terminal numarası, from the same panel."},
			{ID: "provision_password", Title: "Provision password", Type: "password", Secret: true, Required: true,
				Desc: "Password of the PROVAUT user."},
			{ID: "store_key", Title: "3D store key", Type: "password", Secret: true, Required: true,
				Desc: "The 3D Secure store key (storekey) set in the virtual POS panel."},
			{ID: "refund_password", Title: "Refund password", Type: "password", Secret: true,
				Desc: "Password of the PROVRFN (cancel / refund) user. Empty: the provision password."},
			{ID: "company_name", Title: "Company name", Type: "text",
				Desc: "Shown to the customer on the GarantiPay page."},
		},
		Hosts:   pay.Hosts{Form: []string{liveOrigin, testOrigin}},
		DocsURL: "https://dev.garantibbva.com.tr/en/solutions",
		New:     newProvider,
	}
}

type provider struct {
	cfg                                pay.Config
	merchantID, terminalID             string
	password, storeKey, refundPassword string
	companyName                        string
}

func newProvider(cfg pay.Config) (pay.Provider, error) {
	if err := cfg.Require("merchant_id", "terminal_id", "provision_password", "store_key"); err != nil {
		return nil, err
	}
	p := &provider{
		cfg:            cfg,
		merchantID:     cfg.Value("merchant_id"),
		terminalID:     cfg.Value("terminal_id"),
		password:       cfg.Value("provision_password"),
		storeKey:       cfg.Value("store_key"),
		refundPassword: cfg.Value("refund_password"),
		companyName:    cfg.Value("company_name"),
	}
	if p.refundPassword == "" {
		p.refundPassword = p.password
	}
	return p, nil
}

func (p *provider) origin() string { return p.cfg.Origin(liveOrigin, testOrigin) }

func (p *provider) mode() string {
	if p.cfg.Test {
		return "TEST"
	}
	return "PROD"
}

// iso88599 encodes s as ISO-8859-9 (Turkish), the encoding Garanti hashes. Runes it cannot
// represent become '?', so a hash over them never silently matches another text.
func iso88599(s string) []byte {
	b := make([]byte, 0, len(s))
	for _, r := range s {
		switch r {
		case 'Ğ':
			b = append(b, 0xD0)
		case 'İ':
			b = append(b, 0xDD)
		case 'Ş':
			b = append(b, 0xDE)
		case 'ğ':
			b = append(b, 0xF0)
		case 'ı':
			b = append(b, 0xFD)
		case 'ş':
			b = append(b, 0xFE)
		case 0xD0, 0xDD, 0xDE, 0xF0, 0xFD, 0xFE:
			b = append(b, '?') // Latin-1 letters whose slots ISO-8859-9 gives to Turkish ones
		default:
			if r < 0x100 {
				b = append(b, byte(r))
			} else {
				b = append(b, '?')
			}
		}
	}
	return b
}

func upperHash(h hash.Hash, s string) string {
	h.Write(iso88599(s))
	return strings.ToUpper(hex.EncodeToString(h.Sum(nil)))
}

// securityData is SHA1(password + terminal id left-padded with zeros to 9 digits).
func securityData(password, terminalID string) string {
	if n := len(terminalID); n < 9 {
		terminalID = strings.Repeat("0", 9-n) + terminalID
	}
	return upperHash(sha1.New(), password+terminalID) //nolint:gosec // G401: SecurityData is SHA-1 by the Garanti protocol
}

// formHash is secure3dhash: SHA512(terminalid + orderid + txnamount + txncurrencycode + successurl +
// errorurl + txntype + txninstallmentcount + storekey + SecurityData).
func formHash(terminalID, orderID, amount, currency, successURL, errorURL, txnType, installment, storeKey, secData string) string {
	return upperHash(sha512.New(), terminalID+orderID+amount+currency+successURL+errorURL+txnType+installment+storeKey+secData)
}

// requestHash is the XML API's HashData: SHA512(OrderID + TerminalID + card number (always empty
// here) + Amount + CurrencyCode + SecurityData).
func requestHash(orderID, terminalID, amount, currency, secData string) string {
	return upperHash(sha512.New(), orderID+terminalID+amount+currency+secData)
}

// verifyCallback checks the callback hash: the values of the fields named in hashparams
// (":"-separated) joined without a separator, plus the store key, hashed with SHA-512 (API version
// 512) or SHA-1 (older terminals). hashparams must cover the order id and mdstatus, and a
// hashparamsval sent along must equal the rebuilt value.
func verifyCallback(get func(string) string, storeKey string) bool {
	sent, params := get("hash"), get("hashparams")
	if sent == "" || params == "" {
		return false
	}
	var names []string
	for _, n := range strings.Split(params, ":") {
		if n != "" {
			names = append(names, n)
		}
	}
	hasOrder, hasMD := false, false
	var val strings.Builder
	for _, n := range names {
		hasOrder = hasOrder || n == "oid" || n == "orderid"
		hasMD = hasMD || n == "mdstatus"
		val.WriteString(get(n))
	}
	if !hasOrder || !hasMD {
		return false
	}
	if pv := get("hashparamsval"); pv != "" && pv != val.String() {
		return false
	}
	sent = strings.ToUpper(sent)
	return pay.Equal(upperHash(sha512.New(), val.String()+storeKey), sent) ||
		pay.Equal(upperHash(sha1.New(), val.String()+storeKey), sent) //nolint:gosec // G401: older terminals sign the callback with SHA-1
}

func (p *provider) Start(ctx context.Context, r pay.StartRequest) (*pay.StartResult, error) {
	cur := pay.NumericCurrency(r.Currency)
	if cur == "" {
		return nil, pay.Errorf(pay.CodeInvalidRequest, "", "currency %s is not supported", r.Currency)
	}
	if r.Amount <= 0 {
		return nil, pay.Errorf(pay.CodeInvalidRequest, "", "amount must be positive")
	}
	amount := strconv.FormatInt(r.Amount, 10)
	inst := ""
	if r.Installment > 1 {
		inst = strconv.Itoa(r.Installment)
	}
	errorURL := r.FailURL
	if errorURL == "" {
		errorURL = r.ReturnURL
	}
	lang := "tr"
	if strings.HasPrefix(strings.ToLower(r.Locale), "en") {
		lang = "en"
	}
	const txnType = "gpdatarequest"
	hash := formHash(p.terminalID, r.Ref, amount, cur, r.ReturnURL, errorURL, txnType, inst, p.storeKey,
		securityData(p.password, p.terminalID))
	fields := [][2]string{
		{"secure3dsecuritylevel", "CUSTOM_PAY"},
		{"mode", p.mode()},
		{"apiversion", apiVersion},
		{"terminalprovuserid", provUser},
		{"terminaluserid", provUser},
		{"terminalmerchantid", p.merchantID},
		{"terminalid", p.terminalID},
		{"txntype", txnType},
		{"txnsubtype", "sales"},
		{"txnamount", amount},
		{"txncurrencycode", cur},
		{"txninstallmentcount", inst},
		{"orderid", r.Ref},
		{"successurl", r.ReturnURL},
		{"errorurl", errorURL},
		{"customeripaddress", r.Customer.IP},
		{"customeremailaddress", r.Customer.Email},
		{"garantipay", "Y"},
		// Let the customer use Bonus, partner (FBB), cheque and mile points.
		{"bnsuseflag", "Y"},
		{"fbbuseflag", "Y"},
		{"chequeuseflag", "Y"},
		{"mileuseflag", "Y"},
		{"companyname", p.companyName},
		{"lang", lang},
		{"txntimestamp", time.Now().UTC().Format("2006-01-02T15:04:05Z")},
		{"secure3dhash", hash},
	}
	return &pay.StartResult{Form: &pay.Form{Action: p.origin() + gatePath, Fields: fields}}, nil
}

func (p *provider) CallbackRef(in pay.CallbackInput) string {
	if v := in.Form.Get("orderid"); v != "" {
		return v
	}
	return in.Form.Get("oid")
}

func (p *provider) Complete(ctx context.Context, in pay.CallbackInput, a pay.Attempt) (*pay.Result, error) {
	get := in.Form.Get
	orderID := get("orderid")
	if orderID == "" {
		orderID = get("oid")
	}
	if oid := get("oid"); oid != "" && get("orderid") != "" && oid != get("orderid") {
		return nil, errors.New("garanti: callback names two orders")
	}
	if orderID != a.Ref {
		return nil, errors.New("garanti: callback is about another order")
	}
	code, approved := get("procreturncode"), get("procreturncode") == "00" || get("response") == "Approved"

	// Garanti posts some failures unsigned (a timed-out GarantiPay page). A failure that claims
	// nothing is reported with its code; an unsigned callback claiming approval is rejected.
	if get("hash") == "" && get("hashparams") == "" {
		// Only on the browser's own URLs, which the bridge routes with the order key: an unsigned
		// request on the notification URL could otherwise fail an order knowing just the reference.
		if approved || in.Kind == pay.CallbackNotify {
			return nil, pay.ErrInvalidSignature
		}
		return failure(code, get("errmsg")), nil
	}
	if !verifyCallback(get, p.storeKey) {
		return nil, pay.ErrInvalidSignature
	}
	if get("terminalid") != p.terminalID ||
		(get("terminalmerchantid") != "" && get("terminalmerchantid") != p.merchantID) {
		return nil, errors.New("garanti: callback is about another terminal")
	}
	if code != "00" {
		msg := get("errmsg")
		if msg == "" {
			msg = get("mderrormessage")
		}
		return failure(code, msg), nil
	}

	// The bank already charged; its own record is the proof and gives the amount.
	res, err := p.inquire(ctx, a.Ref, a.Currency)
	if err != nil {
		return nil, err
	}
	o := res.Order.Inq
	if res.Transaction.Response.Code != "00" || o.Status != "APPROVED" || o.ChargeType != "S" {
		return nil, errors.New("garanti: the bank does not confirm the payment")
	}
	paid, err := strconv.ParseInt(strings.TrimSpace(o.AuthAmount), 10, 64)
	if err != nil || paid <= 0 {
		return nil, errors.New("garanti: the bank's order record has no amount")
	}
	inst, _ := strconv.Atoi(strings.TrimSpace(o.InstallmentCnt))
	return &pay.Result{Status: pay.StatusSuccess, TxnID: o.RetrefNum, PaidAmount: paid, Installment: inst}, nil
}

func (p *provider) Query(ctx context.Context, a pay.Attempt) (*pay.Result, error) {
	res, err := p.inquire(ctx, a.Ref, a.Currency)
	if err != nil {
		return nil, err
	}
	if res.Transaction.Response.Code != "00" {
		r := res.Transaction.Response
		return nil, &pay.Error{Code: mapCode(r.code()), ProviderCode: r.code(), ProviderMessage: r.message()}
	}
	o := res.Order.Inq
	switch o.Status {
	case "APPROVED":
		paid, _ := strconv.ParseInt(strings.TrimSpace(o.AuthAmount), 10, 64)
		inst, _ := strconv.Atoi(strings.TrimSpace(o.InstallmentCnt))
		return &pay.Result{Status: pay.StatusSuccess, TxnID: o.RetrefNum, PaidAmount: paid, Installment: inst}, nil
	case "DECLINED":
		c := o.ReasonCode
		if c == "" {
			c = o.Code
		}
		return &pay.Result{Status: pay.StatusFailure, Code: mapCode(c), ProviderCode: c, ProviderMessage: o.SysErrMsg}, nil
	case "VOID", "REFUND":
		// REFUND: AuthAmount is what remains after refunds.
		return &pay.Result{Status: pay.StatusCancelled, TxnID: o.RetrefNum}, nil
	}
	return &pay.Result{Status: pay.StatusPending}, nil
}

// Refund voids a full refund (same-day cancel) and falls back to a refund when the bank refuses the
// void (the sale has settled); a partial amount is always a refund.
func (p *provider) Refund(ctx context.Context, r pay.RefundRequest) (*pay.Result, error) {
	a := r.Attempt
	cur := pay.NumericCurrency(a.Currency)
	if cur == "" {
		return nil, pay.Errorf(pay.CodeInvalidRequest, "", "currency %s is not supported", a.Currency)
	}
	if r.Amount <= 0 {
		return nil, pay.Errorf(pay.CodeInvalidRequest, "", "amount must be positive")
	}
	amount := strconv.FormatInt(r.Amount, 10)
	if r.Full {
		res, err := p.api(ctx, a.Ref, amount, cur, "void", true)
		if err != nil {
			return nil, err
		}
		if res.Transaction.Response.Code == "00" {
			return &pay.Result{Status: pay.StatusSuccess, TxnID: res.Transaction.RetrefNum, PaidAmount: r.Amount}, nil
		}
	}
	res, err := p.api(ctx, a.Ref, amount, cur, "refund", true)
	if err != nil {
		return nil, err
	}
	if rs := res.Transaction.Response; rs.Code != "00" {
		return nil, &pay.Error{Code: mapCode(rs.code()), ProviderCode: rs.code(), ProviderMessage: rs.message()}
	}
	return &pay.Result{Status: pay.StatusSuccess, TxnID: res.Transaction.RetrefNum, PaidAmount: r.Amount}, nil
}

// inquire is the order inquiry (orderinq). Its HashData needs an amount and currency, which the
// bank does not check against the order.
func (p *provider) inquire(ctx context.Context, orderID, currency string) (*gvpsResponse, error) {
	cur := pay.NumericCurrency(currency)
	if cur == "" {
		cur = "949"
	}
	return p.api(ctx, orderID, "1", cur, "orderinq", false)
}

type gvpsResponse struct {
	XMLName xml.Name `xml:"GVPSResponse"`
	Order   struct {
		Inq struct {
			Status         string `xml:"Status"`
			ChargeType     string `xml:"ChargeType"`
			AuthAmount     string `xml:"AuthAmount"`
			InstallmentCnt string `xml:"InstallmentCnt"`
			RetrefNum      string `xml:"RetrefNum"`
			Code           string `xml:"Code"`
			ReasonCode     string `xml:"ReasonCode"`
			SysErrMsg      string `xml:"SysErrMsg"`
		} `xml:"OrderInqResult"`
	} `xml:"Order"`
	Transaction struct {
		Response  gvpsStatus `xml:"Response"`
		RetrefNum string     `xml:"RetrefNum"`
		AuthCode  string     `xml:"AuthCode"`
	} `xml:"Transaction"`
}

type gvpsStatus struct {
	Code       string `xml:"Code"`
	ReasonCode string `xml:"ReasonCode"`
	Message    string `xml:"Message"`
	ErrorMsg   string `xml:"ErrorMsg"`
	SysErrMsg  string `xml:"SysErrMsg"`
}

// code is ReasonCode (ISO 8583 for bank declines), else Code.
func (s gvpsStatus) code() string {
	if s.ReasonCode != "" {
		return s.ReasonCode
	}
	return s.Code
}

func (s gvpsStatus) message() string {
	for _, m := range []string{s.ErrorMsg, s.SysErrMsg, s.Message} {
		if m != "" {
			return m
		}
	}
	return ""
}

func esc(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// api sends a GVPSRequest signed with HashData. Void and refund use the PROVRFN user.
func (p *provider) api(ctx context.Context, orderID, amount, cur, txnType string, refund bool) (*gvpsResponse, error) {
	user, password := provUser, p.password
	if refund {
		user, password = refundUser, p.refundPassword
	}
	hashData := requestHash(orderID, p.terminalID, amount, cur, securityData(password, p.terminalID))
	extra := `<InstallmentCnt></InstallmentCnt><CardholderPresentCode>0</CardholderPresentCode><MotoInd>N</MotoInd>`
	if txnType == "orderinq" {
		extra = `<ListPageNum>0</ListPageNum><CardholderPresentCode></CardholderPresentCode><MotoInd>N</MotoInd>`
	}
	body := `<?xml version="1.0" encoding="UTF-8"?><GVPSRequest>` +
		`<Mode>` + p.mode() + `</Mode><Version>` + apiVersion + `</Version>` +
		`<Terminal><ProvUserID>` + user + `</ProvUserID><HashData>` + hashData + `</HashData><UserID>` + user +
		`</UserID><ID>` + esc(p.terminalID) + `</ID><MerchantID>` + esc(p.merchantID) + `</MerchantID></Terminal>` +
		`<Customer><IPAddress>127.0.0.1</IPAddress></Customer>` +
		`<Order><OrderID>` + esc(orderID) + `</OrderID></Order>` +
		`<Transaction><Type>` + txnType + `</Type>` + extra +
		`<Amount>` + amount + `</Amount><CurrencyCode>` + cur + `</CurrencyCode></Transaction>` +
		`</GVPSRequest>`
	status, out, err := pay.PostBody(ctx, p.cfg.HTTP, p.origin()+apiPath, "application/xml", []byte(body), nil)
	if err != nil {
		return nil, err
	}
	var res gvpsResponse
	if err := xml.Unmarshal(out, &res); err != nil {
		if refund {
			// The request reached the bank; an unreadable answer leaves the refund's outcome open.
			return nil, fmt.Errorf("%w: garanti returned HTTP %d without a GVPSResponse", pay.ErrUnknownOutcome, status)
		}
		return nil, fmt.Errorf("garanti: HTTP %d without a GVPSResponse", status)
	}
	return &res, nil
}

func failure(code, msg string) *pay.Result {
	return &pay.Result{Status: pay.StatusFailure, Code: mapCode(code), ProviderCode: code, ProviderMessage: msg}
}

// mapCode maps ISO 8583 response codes (the reference's table) to pay codes.
func mapCode(c string) pay.Code {
	switch c {
	case "01", "02", "05", "12", "57", "62":
		return pay.CodeCardDeclined
	case "03", "58":
		return pay.CodeConfiguration
	case "04", "07", "59":
		return pay.CodeFraudSuspected
	case "41", "43":
		return pay.CodeStolenOrLost
	case "13":
		return pay.CodeInvalidRequest
	case "14", "15", "82":
		return pay.CodeInvalidCard
	case "33", "54":
		return pay.CodeExpiredCard
	case "51":
		return pay.CodeInsufficientFunds
	case "61", "65":
		return pay.CodeLimitExceeded
	case "91", "96":
		return pay.CodeProviderDown
	case "94":
		return pay.CodeDuplicate
	}
	return pay.CodeUnknown
}
