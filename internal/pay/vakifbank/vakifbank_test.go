package vakifbank

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"wpgo/internal/pay"
)

const (
	fakeMerchant = "000000000999999"
	fakePassword = "fake-password"
	fakeTerminal = "VP999999"
	token        = "b35a56bf37334872a945afc0016d5887"
	ref          = "ABCD1234efgh" // attempt Ref, sent as TransactionId and OrderID
	txnID        = ref
)

type call struct {
	path string
	form url.Values
}

type bank struct {
	calls   []call
	replies map[string]string // path → XML
	status  int
}

func (b *bank) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	b.calls = append(b.calls, call{r.URL.Path, r.PostForm})
	if b.status != 0 {
		w.WriteHeader(b.status)
	}
	w.Write([]byte(b.replies[r.URL.Path]))
}

const (
	registerPath = "/CommonPayment/api/RegisterTransaction"
	resultPath   = "/CommonPayment/api/VposTransaction"
	vposPathT    = "/VposService/v3/Vposreq.aspx"
)

func newTest(t *testing.T, replies map[string]string) (pay.Provider, *bank) {
	t.Helper()
	b := &bank{replies: replies}
	srv := httptest.NewServer(b)
	t.Cleanup(srv.Close)
	p, err := Spec().New(pay.Config{Values: map[string]string{"merchant_id": fakeMerchant, "password": fakePassword,
		"terminal_no": fakeTerminal}, Test: true, HTTP: srv.Client(), BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	return p, b
}

func startReq() pay.StartRequest {
	return pay.StartRequest{Ref: "ABCD1234efgh", Amount: 34990, Currency: "TRY", Locale: "en",
		Customer:  pay.Customer{Email: "buyer@example.com", IP: "203.0.113.7"},
		Items:     []pay.Item{{Name: "Kitap", Quantity: 1, UnitPrice: 34990}},
		ReturnURL: "https://shop.example/ok", FailURL: "https://shop.example/fail"}
}

// Shape of the guide's RegisterResponse example.
const registered = `<RegisterResponse xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xmlns:xsd="http://www.w3.org/2001/XMLSchema"><CommonPaymentUrl>https://cptest.vakifbank.com.tr/CommonPayment/SecurePayment</CommonPaymentUrl><PaymentToken>` + token + `</PaymentToken></RegisterResponse>`

var cardFields = []string{"pan", "cvv", "expiremonth", "expireyear", "expiry", "brandnumber", "cardholdersname", "creditcard", "cardnumber"}

func TestStart(t *testing.T) {
	p, b := newTest(t, map[string]string{registerPath: registered})
	res, err := p.Start(context.Background(), startReq())
	if err != nil {
		t.Fatal(err)
	}
	if res.Redirect != "https://cptest.vakifbank.com.tr/CommonPayment/SecurePayment?Ptkn="+token || res.ProviderRef != token || res.Form != nil {
		t.Errorf("result %+v", res)
	}
	if len(b.calls) != 1 || b.calls[0].path != registerPath {
		t.Fatalf("calls %+v", b.calls)
	}
	f := b.calls[0].form
	want := map[string]string{"HostMerchantId": fakeMerchant, "MerchantPassword": fakePassword, "HostTerminalId": fakeTerminal,
		"TransactionType": "Sale", "AmountCode": "949", "Amount": "349.90", "TransactionId": ref, "OrderID": ref, "IsSecure": "true",
		"AllowNotEnrolledCard": "false", "SuccessUrl": "https://shop.example/ok", "FailUrl": "https://shop.example/fail",
		"RequestLanguage": "en-US"}
	for k, v := range want {
		if !f.Has(k) || f.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, f.Get(k), v)
		}
	}
	if len(f) != len(want) {
		t.Errorf("sent %d fields, want %d: %v", len(f), len(want), f)
	}
	for k := range f {
		for _, c := range cardFields {
			if strings.ToLower(k) == c {
				t.Errorf("card field %s", k)
			}
		}
	}

	r := startReq()
	r.Installment, r.Locale, r.FailURL = 3, "tr", ""
	p.Start(context.Background(), r)
	f = b.calls[1].form
	if f.Get("InstallmentCount") != "3" || f.Get("RequestLanguage") != "tr-TR" || f.Get("FailUrl") != "https://shop.example/ok" {
		t.Errorf("installment/defaults %v", f)
	}
}

func TestStartErrors(t *testing.T) {
	var pe *pay.Error
	p, _ := newTest(t, map[string]string{registerPath: `<RegisterResponse><ErrorCode>5007</ErrorCode></RegisterResponse>`})
	if _, err := p.Start(context.Background(), startReq()); !errors.As(err, &pe) || pe.ProviderCode != "5007" {
		t.Errorf("register error: %v", err)
	}
	for _, reply := range []string{
		`<R><CommonPaymentUrl>https://evil.example/pay</CommonPaymentUrl><PaymentToken>abc</PaymentToken></R>`,
		`<R><CommonPaymentUrl>http://cptest.vakifbank.com.tr/x</CommonPaymentUrl><PaymentToken>abc</PaymentToken></R>`,
		`<R><CommonPaymentUrl>https://cptest.vakifbank.com.tr/x</CommonPaymentUrl><PaymentToken>a&amp;b=c</PaymentToken></R>`,
		`not xml`,
	} {
		p, _ := newTest(t, map[string]string{registerPath: reply})
		if _, err := p.Start(context.Background(), startReq()); err == nil {
			t.Errorf("accepted %s", reply)
		}
	}
	r := startReq()
	r.Currency = "CHF"
	if _, err := p.Start(context.Background(), r); !errors.As(err, &pe) {
		t.Errorf("CHF accepted: %v", err)
	}
}

func result(over map[string]string) string {
	f := map[string]string{"Rc": "0000", "AuthCode": "735879", "Message": "", "TransactionId": txnID,
		"PaymentToken": token, "MaskedPan": "49384601****4205", "HostMerchantId": fakeMerchant, "AmountCode": "949",
		"Amount": "349,90", "TransactionType": "Sale", "InstallmentCount": "", "ErrorCode": ""}
	for k, v := range over {
		f[k] = v
	}
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="utf-8"?><VposQueryResponse>`)
	for k, v := range f {
		if v == "" {
			continue
		} else {
			b.WriteString("<" + k + ">" + v + "</" + k + ">")
		}
	}
	b.WriteString(`</VposQueryResponse>`)
	return b.String()
}

// back is the guide's return: GET with TransactionId and Ptkn in the query.
func back(tok, txn string) pay.CallbackInput {
	return pay.CallbackInput{Kind: pay.CallbackReturn, Method: "GET",
		Query: url.Values{"TransactionId": {txn}, "Ptkn": {tok}}}
}

func TestComplete(t *testing.T) {
	ctx := context.Background()
	a := pay.Attempt{Ref: ref, ProviderRef: token, Amount: 34990, Currency: "TRY"}

	p, b := newTest(t, map[string]string{resultPath: result(map[string]string{"InstallmentCount": "3"})})
	if p.CallbackRef(back(token, txnID)) != "" {
		t.Error("CallbackRef")
	}
	res, err := p.Complete(ctx, back(token, txnID), a)
	if err != nil || res.Status != pay.StatusSuccess || res.PaidAmount != 34990 || res.Currency != "TRY" || res.TxnID != txnID || res.Installment != 3 {
		t.Fatalf("approved: %+v %v", res, err)
	}
	f := b.calls[0].form
	if b.calls[0].path != resultPath || f.Get("HostMerchantId") != fakeMerchant || f.Get("Password") != fakePassword ||
		f.Get("TransactionId") != txnID || f.Get("PaymentToken") != token || len(f) != 4 {
		t.Errorf("result request %+v", b.calls[0])
	}

	// A form post with the sample page's field names works too.
	in := pay.CallbackInput{Kind: pay.CallbackReturn, Method: "POST", Form: url.Values{"PaymentToken": {token}, "TransactionId": {txnID}}}
	if res, err := p.Complete(ctx, in, a); err != nil || res.Status != pay.StatusSuccess {
		t.Errorf("POST return: %+v %v", res, err)
	}

	// Declined, as in the guide's examples (unpadded Rc).
	p, _ = newTest(t, map[string]string{resultPath: result(map[string]string{"Rc": "51", "Message": "Bakiye Yetersiz", "AmountCode": "", "Amount": ""})})
	res, err = p.Complete(ctx, back(token, txnID), a)
	if err != nil || res.Status != pay.StatusFailure || res.Code != pay.CodeInsufficientFunds || res.ProviderCode != "51" || res.ProviderMessage != "Bakiye Yetersiz" {
		t.Errorf("declined: %+v %v", res, err)
	}

	// The browser's own claims are ignored: a "0000" return the bank does not confirm fails.
	p, _ = newTest(t, map[string]string{resultPath: result(map[string]string{"Rc": "0057"})})
	in = back(token, txnID)
	in.Query.Set("RC", "0000")
	if res, err := p.Complete(ctx, in, a); err != nil || res.Status != pay.StatusFailure || res.Code != pay.CodeCardDeclined {
		t.Errorf("unconfirmed: %+v %v", res, err)
	}

	// Never reached the virtual POS: the query says 5003, the return's RC / MSG explain.
	p, _ = newTest(t, map[string]string{resultPath: `<RegisterResponse><ErrorCode>5003</ErrorCode></RegisterResponse>`})
	in = back(token, txnID)
	in.Query.Set("RC", "6969")
	in.Query.Set("MSG", "ACS")
	if res, err := p.Complete(ctx, in, a); err != nil || res.Status != pay.StatusFailure || res.ProviderCode != "6969" {
		t.Errorf("not sent: %+v %v", res, err)
	}
	// Any other query error is not an outcome.
	p, _ = newTest(t, map[string]string{resultPath: `<RegisterResponse><ErrorCode>5002</ErrorCode></RegisterResponse>`})
	if _, err := p.Complete(ctx, back(token, txnID), a); err == nil {
		t.Error("query error accepted")
	}

	// Tampered amount is reported as the bank's amount (the bridge refuses a lower one).
	p, _ = newTest(t, map[string]string{resultPath: result(map[string]string{"Amount": "1,00"})})
	if res, err := p.Complete(ctx, back(token, txnID), a); err != nil || res.PaidAmount != 100 {
		t.Errorf("amount: %+v %v", res, err)
	}

	for name, over := range map[string]map[string]string{
		"other order":    {"OrderID": "OTHER1234567"},
		"other txn":      {"TransactionId": "OTHER1234567"},
		"no txn":         {"TransactionId": ""},
		"no amount":      {"Amount": ""},
		"no currency":    {"AmountCode": ""},
		"no rc":          {"Rc": ""},
		"other token":    {"PaymentToken": "ffffffffffffffffffffffffffffffff"},
		"other merchant": {"HostMerchantId": "000000000111111"},
		"bad amount":     {"Amount": "1.2.3"},
		"bad currency":   {"AmountCode": "999"},
	} {
		p, _ := newTest(t, map[string]string{resultPath: result(over)})
		if _, err := p.Complete(ctx, back(token, txnID), a); err == nil {
			t.Errorf("%s accepted", name)
		}
	}

	// Token (and transaction id) in the return must be this attempt's.
	p, b = newTest(t, map[string]string{resultPath: result(nil)})
	for _, in := range []pay.CallbackInput{back("", txnID), back("ffffffffffffffffffffffffffffffff", txnID), back(token, "OTHER1234567")} {
		if _, err := p.Complete(ctx, in, a); err == nil {
			t.Errorf("return %v accepted", in.Query)
		}
	}
	if len(b.calls) != 0 {
		t.Error("looked up a foreign return")
	}
	// The return without a TransactionId is looked up by the attempt's own.
	if res, err := p.Complete(ctx, pay.CallbackInput{Query: url.Values{"Ptkn": {token}}}, a); err != nil || res.Status != pay.StatusSuccess {
		t.Errorf("token only: %+v %v", res, err)
	}
}

func TestQuery(t *testing.T) {
	ctx := context.Background()
	a := pay.Attempt{Ref: ref, ProviderRef: token}
	p, b := newTest(t, map[string]string{resultPath: result(nil)})
	if res, err := p.Query(ctx, a); err != nil || res.Status != pay.StatusSuccess || res.PaidAmount != 34990 {
		t.Errorf("paid: %+v %v", res, err)
	}
	if f := b.calls[0].form; f.Get("TransactionId") != ref || f.Get("PaymentToken") != token || f.Get("Password") != fakePassword {
		t.Errorf("query request %v", f)
	}
	p, _ = newTest(t, map[string]string{resultPath: `<RegisterResponse><ErrorCode>5003</ErrorCode></RegisterResponse>`})
	if res, err := p.Query(ctx, a); err != nil || res.Status != pay.StatusPending {
		t.Errorf("not yet: %+v %v", res, err)
	}
}

func vposReply(code string) string {
	return `<?xml version="1.0" encoding="utf-8"?><VposResponse><MerchantId>` + fakeMerchant + `</MerchantId><TransactionId>R` + code +
		`</TransactionId><ResultCode>` + code + `</ResultCode><ResultDetail>detail</ResultDetail></VposResponse>`
}

func refundReq(full bool, amount int64) pay.RefundRequest {
	return pay.RefundRequest{Attempt: pay.Attempt{Ref: "ABCD1234efgh", Currency: "TRY", TxnID: txnID, PaidAmount: 34990},
		Amount: amount, Full: full, RefundRef: "RF1"}
}

func xmlOf(t *testing.T, c call) map[string]string {
	t.Helper()
	if c.path != vposPathT {
		t.Errorf("path %s", c.path)
	}
	m, err := leaves([]byte(c.form.Get("prmstr")))
	if err != nil || !strings.Contains(c.form.Get("prmstr"), "<VposRequest>") {
		t.Fatalf("prmstr %q", c.form.Get("prmstr"))
	}
	return m
}

func TestRefund(t *testing.T) {
	ctx := context.Background()
	p, b := newTest(t, map[string]string{vposPathT: vposReply("0000")})
	res, err := p.Refund(ctx, refundReq(true, 34990))
	if err != nil || res.Status != pay.StatusSuccess || res.PaidAmount != 34990 || len(b.calls) != 1 {
		t.Fatalf("cancel: %+v %v", res, err)
	}
	m := xmlOf(t, b.calls[0])
	if m["TransactionType"] != "Cancel" || m["ReferenceTransactionId"] != txnID || m["MerchantId"] != fakeMerchant ||
		m["Password"] != fakePassword || m["TerminalNo"] != fakeTerminal || m["TransactionId"] != "RF1C" ||
		m["TransactionDeviceSource"] != "0" || m["Location"] != "1" || len(m) != 8 {
		t.Errorf("cancel request %v", m)
	}

	// Cancel refused, then refund.
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Write([]byte(vposReply("1059")))
			return
		}
		r.ParseForm()
		m, _ := leaves([]byte(r.PostForm.Get("prmstr")))
		if m["TransactionType"] != "Refund" || m["CurrencyAmount"] != "349.90" || m["CurrencyCode"] != "949" ||
			m["ReferenceTransactionId"] != txnID || m["TransactionId"] != "RF1" {
			t.Errorf("refund request %v", m)
		}
		w.Write([]byte(vposReply("0000")))
	}))
	defer srv.Close()
	p2, _ := Spec().New(pay.Config{Values: map[string]string{"merchant_id": fakeMerchant, "password": fakePassword, "terminal_no": fakeTerminal},
		HTTP: srv.Client(), BaseURL: srv.URL})
	if res, err := p2.Refund(ctx, refundReq(true, 34990)); err != nil || res.Status != pay.StatusSuccess || calls != 2 {
		t.Errorf("cancel then refund: %+v %v %d", res, err, calls)
	}

	p, b = newTest(t, map[string]string{vposPathT: vposReply("0000")})
	if res, err := p.Refund(ctx, refundReq(false, 1000)); err != nil || res.PaidAmount != 1000 {
		t.Errorf("partial: %+v %v", res, err)
	}
	if m := xmlOf(t, b.calls[0]); m["TransactionType"] != "Refund" || m["CurrencyAmount"] != "10.00" {
		t.Errorf("partial request %v", m)
	}

	p, _ = newTest(t, map[string]string{vposPathT: vposReply("0051")})
	var pe *pay.Error
	if _, err := p.Refund(ctx, refundReq(false, 1000)); !errors.As(err, &pe) || pe.ProviderCode != "0051" {
		t.Errorf("rejected: %v", err)
	}
	r := refundReq(false, 1000)
	r.RefundRef = ""
	p, b = newTest(t, map[string]string{vposPathT: vposReply("0000")})
	if _, err := p.Refund(ctx, r); err != nil || strings.Contains(b.calls[0].form.Get("prmstr"), "<TransactionId>") {
		t.Errorf("no refund ref: %v %v", err, b.calls)
	}
	r.Attempt.TxnID = ""
	if _, err := p.Refund(ctx, r); !errors.As(err, &pe) {
		t.Errorf("no txn id: %v", err)
	}
	p, b = newTest(t, map[string]string{vposPathT: "<html>oops</html>"})
	b.status = 502
	if _, err := p.Refund(ctx, refundReq(false, 1000)); !errors.Is(err, pay.ErrUnknownOutcome) {
		t.Errorf("502: %v", err)
	}
}

func TestRefundTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
	}))
	defer srv.Close()
	cl := srv.Client()
	cl.Timeout = 50 * time.Millisecond
	p, _ := Spec().New(pay.Config{Values: map[string]string{"merchant_id": fakeMerchant, "password": fakePassword, "terminal_no": fakeTerminal},
		HTTP: cl, BaseURL: srv.URL})
	for _, full := range []bool{true, false} {
		if _, err := p.Refund(context.Background(), refundReq(full, 34990)); !errors.Is(err, pay.ErrUnknownOutcome) {
			t.Errorf("full=%v: %v", full, err)
		}
	}
}

func TestNewRequires(t *testing.T) {
	if _, err := Spec().New(pay.Config{Values: map[string]string{"merchant_id": fakeMerchant, "terminal_no": fakeTerminal}}); err == nil {
		t.Error("missing password accepted")
	}
}
