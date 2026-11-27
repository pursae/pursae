package garanti

import (
	"context"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"wpgo/internal/pay"
)

// The public test terminal of dev.garantibbva.com.tr, as in the reference's fixtures.
const (
	testMerchant = "7000679"
	testTerminal = "30691297"
	testPassword = "123qweASD/"
	testStoreKey = "12345678"
	// SHA1("123qweASD/" + "030691297"), from the reference fixtures (computed there with Python).
	testSecurityData = "BAF0BF326B0261A4288A7273F18674FF35E9826F"
)

func newTest(t *testing.T, base string, client *http.Client) pay.Provider {
	t.Helper()
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Second}
	}
	p, err := Spec().New(pay.Config{
		Values: map[string]string{
			"merchant_id": testMerchant, "terminal_id": testTerminal,
			"provision_password": testPassword, "store_key": testStoreKey,
			"refund_password": "fake-refund-pass",
		},
		Test: true, HTTP: client, BaseURL: base,
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSecurityData(t *testing.T) {
	if got := securityData(testPassword, testTerminal); got != testSecurityData {
		t.Fatalf("securityData = %s", got)
	}
}

func TestStartFormFields(t *testing.T) {
	p := newTest(t, "", nil)
	res, err := p.Start(context.Background(), pay.StartRequest{
		Ref: "ORDERGP1234", Amount: 120, Currency: "TRY",
		ReturnURL: "https://shop.example/ok", FailURL: "https://shop.example/fail",
		Customer: pay.Customer{IP: "198.51.100.7", Email: "a@example.com"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Form == nil || res.Form.Action != testOrigin+gatePath {
		t.Fatalf("form = %+v", res.Form)
	}
	f := map[string]string{}
	var names []string
	for _, kv := range res.Form.Fields {
		f[kv[0]] = kv[1]
		names = append(names, kv[0])
	}
	want := map[string]string{
		"secure3dsecuritylevel": "CUSTOM_PAY", "mode": "TEST", "apiversion": "512",
		"terminalprovuserid": "PROVAUT", "terminaluserid": "PROVAUT", "terminalmerchantid": testMerchant,
		"terminalid": testTerminal, "txntype": "gpdatarequest", "txnsubtype": "sales", "txnamount": "120",
		"txncurrencycode": "949", "txninstallmentcount": "", "orderid": "ORDERGP1234",
		"successurl": "https://shop.example/ok", "errorurl": "https://shop.example/fail", "garantipay": "Y",
	}
	for k, v := range want {
		if f[k] != v {
			t.Errorf("%s = %q, want %q", k, f[k], v)
		}
	}
	allowed := "secure3dsecuritylevel mode apiversion terminalprovuserid terminaluserid terminalmerchantid " +
		"terminalid txntype txnsubtype txnamount txncurrencycode txninstallmentcount orderid successurl errorurl " +
		"customeripaddress customeremailaddress garantipay bnsuseflag fbbuseflag chequeuseflag mileuseflag " +
		"companyname lang txntimestamp secure3dhash"
	if strings.Join(names, " ") != allowed {
		t.Errorf("field set = %s", strings.Join(names, " "))
	}
	for _, n := range names {
		if strings.Contains(n, "card") || strings.Contains(n, "cvv") || n == "ccno" || n == "cvc" || n == "expDate" {
			t.Errorf("card field %s in the form", n)
		}
	}
	// Independent computation of secure3dhash.
	h := sha512.Sum512([]byte(testTerminal + "ORDERGP1234" + "120" + "949" + "https://shop.example/ok" +
		"https://shop.example/fail" + "gpdatarequest" + "" + testStoreKey + testSecurityData))
	if f["secure3dhash"] != strings.ToUpper(hex.EncodeToString(h[:])) {
		t.Errorf("secure3dhash = %s", f["secure3dhash"])
	}

	res, _ = p.Start(context.Background(), pay.StartRequest{Ref: "ORDERGP1235", Amount: 120, Currency: "TRY",
		Installment: 3, ReturnURL: "https://shop.example/ok"})
	for _, kv := range res.Form.Fields {
		if kv[0] == "txninstallmentcount" && kv[1] != "3" {
			t.Errorf("installment = %q", kv[1])
		}
	}
}

// approvedCallback is the reference's 3D_PAY fixture (hash over hashparams with store key 12345678),
// with the order id of an attempt.
func approvedCallback() url.Values {
	v := url.Values{}
	for k, s := range map[string]string{
		"clientid": testTerminal, "oid": "ORDERPAY", "orderid": "ORDERPAY", "terminalid": testTerminal,
		"terminalmerchantid": testMerchant, "authcode": "123456", "procreturncode": "00", "response": "Approved",
		"mdstatus": "1", "cavv": "CAVVVALUE=", "eci": "02", "md": "MDVALUE123", "rnd": "RNDVALUE",
		"txnamount": "120", "txncurrencycode": "949",
		"hashparams":    "clientid:oid:authcode:procreturncode:response:mdstatus:cavv:eci:md:rnd:",
		"hashparamsval": "30691297ORDERPAY12345600Approved1CAVVVALUE=02MDVALUE123RNDVALUE",
		"hash":          "BE6C74AF86901D3689C4A088CECF3597D8D6C696EC0E08652EEAF3A031372AC6F778553D53AC4BA8729D081C0A74B230B84379BC0682993DF3BC247D8D2E2F89",
	} {
		v.Set(k, s)
	}
	return v
}

// sign recomputes the callback hash independently after a test changed fields.
func sign(v url.Values) {
	var s strings.Builder
	for _, n := range strings.Split(v.Get("hashparams"), ":") {
		if n != "" {
			s.WriteString(v.Get(n))
		}
	}
	v.Set("hashparamsval", s.String())
	h := sha512.Sum512([]byte(s.String() + testStoreKey))
	v.Set("hash", strings.ToUpper(hex.EncodeToString(h[:])))
}

func inquiryXML(status, amount string) string {
	return `<GVPSResponse><Order><OrderID>ORDERPAY</OrderID><OrderInqResult><ChargeType>S</ChargeType>` +
		`<AuthAmount>` + amount + `</AuthAmount><Status>` + status + `</Status><RetrefNum>628408842186</RetrefNum>` +
		`<InstallmentCnt>0</InstallmentCnt></OrderInqResult></Order>` +
		`<Transaction><Response><Source>GVPS</Source><Code>00</Code><Message>Approved</Message></Response></Transaction></GVPSResponse>`
}

type bank struct {
	srv   *httptest.Server
	types []string
	reply func(txnType string) string
}

func newBank(t *testing.T, reply func(string) string) *bank {
	b := &bank{reply: reply}
	b.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s := string(body)
		typ := s[strings.Index(s, "<Type>")+6 : strings.Index(s, "</Type>")]
		b.types = append(b.types, typ)
		if r.URL.Path != apiPath || !strings.Contains(s, "<HashData>") || strings.Contains(s, "<Card>") {
			t.Errorf("bad request %s %s", r.URL.Path, s)
		}
		io.WriteString(w, b.reply(typ))
	}))
	t.Cleanup(b.srv.Close)
	return b
}

func TestCompleteApproved(t *testing.T) {
	b := newBank(t, func(string) string { return inquiryXML("APPROVED", "120") })
	p := newTest(t, b.srv.URL, nil)
	a := pay.Attempt{Ref: "ORDERPAY", Amount: 120, Currency: "TRY"}
	in := pay.CallbackInput{Kind: pay.CallbackReturn, Form: approvedCallback()}
	if got := p.CallbackRef(in); got != "ORDERPAY" {
		t.Fatalf("CallbackRef = %q", got)
	}
	res, err := p.Complete(context.Background(), in, a)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != pay.StatusSuccess || res.PaidAmount != 120 || res.TxnID != "628408842186" {
		t.Fatalf("result = %+v", res)
	}
	if len(b.types) != 1 || b.types[0] != "orderinq" {
		t.Fatalf("requests = %v", b.types)
	}
}

func TestCompleteBankDoesNotConfirm(t *testing.T) {
	b := newBank(t, func(string) string { return inquiryXML("DECLINED", "120") })
	p := newTest(t, b.srv.URL, nil)
	_, err := p.Complete(context.Background(), pay.CallbackInput{Form: approvedCallback()},
		pay.Attempt{Ref: "ORDERPAY", Amount: 120, Currency: "TRY"})
	if err == nil {
		t.Fatal("accepted a payment the bank does not confirm")
	}
}

func TestCompletePaidAmountFromBank(t *testing.T) {
	// A callback whose txnamount was changed: not hashed, and the amount comes from the bank.
	b := newBank(t, func(string) string { return inquiryXML("APPROVED", "100") })
	p := newTest(t, b.srv.URL, nil)
	v := approvedCallback()
	v.Set("txnamount", "99999")
	res, err := p.Complete(context.Background(), pay.CallbackInput{Form: v},
		pay.Attempt{Ref: "ORDERPAY", Amount: 120, Currency: "TRY"})
	if err != nil || res.PaidAmount != 100 {
		t.Fatalf("res = %+v err = %v", res, err)
	}
}

func TestCompleteDeclined(t *testing.T) {
	p := newTest(t, "http://127.0.0.1:1", nil)
	v := approvedCallback()
	v.Set("procreturncode", "51")
	v.Set("response", "Declined")
	v.Set("authcode", "")
	sign(v)
	res, err := p.Complete(context.Background(), pay.CallbackInput{Form: v}, pay.Attempt{Ref: "ORDERPAY", Amount: 120})
	if err != nil || res.Status != pay.StatusFailure || res.Code != pay.CodeInsufficientFunds || res.ProviderCode != "51" {
		t.Fatalf("res = %+v err = %v", res, err)
	}
}

func TestCompleteUnsignedTimeout(t *testing.T) {
	p := newTest(t, "http://127.0.0.1:1", nil)
	v := url.Values{"orderid": {"ORDERGP"}, "oid": {"ORDERGP"}, "procreturncode": {"92"}, "response": {"Error"},
		"mdstatus": {"7"}, "terminalid": {testTerminal}}
	res, err := p.Complete(context.Background(), pay.CallbackInput{Form: v}, pay.Attempt{Ref: "ORDERGP"})
	if err != nil || res.Status != pay.StatusFailure || res.ProviderCode != "92" {
		t.Fatalf("res = %+v err = %v", res, err)
	}
	v.Set("procreturncode", "00")
	v.Set("response", "Approved")
	if _, err := p.Complete(context.Background(), pay.CallbackInput{Form: v}, pay.Attempt{Ref: "ORDERGP"}); !errors.Is(err, pay.ErrInvalidSignature) {
		t.Fatalf("unsigned approval: err = %v", err)
	}
}

func TestCompleteRejects(t *testing.T) {
	p := newTest(t, "http://127.0.0.1:1", nil)
	a := pay.Attempt{Ref: "ORDERPAY", Amount: 120}

	tampered := approvedCallback()
	tampered.Set("authcode", "999999") // hashed field
	if _, err := p.Complete(context.Background(), pay.CallbackInput{Form: tampered}, a); !errors.Is(err, pay.ErrInvalidSignature) {
		t.Errorf("tampered field: %v", err)
	}

	otherOrder := approvedCallback()
	otherOrder.Set("oid", "ORDEROTHER")
	otherOrder.Set("orderid", "ORDEROTHER")
	if _, err := p.Complete(context.Background(), pay.CallbackInput{Form: otherOrder}, pay.Attempt{Ref: "ORDEROTHER"}); !errors.Is(err, pay.ErrInvalidSignature) {
		t.Errorf("tampered order id: %v", err)
	}

	missing := approvedCallback()
	missing.Del("hash")
	if _, err := p.Complete(context.Background(), pay.CallbackInput{Form: missing}, a); err == nil {
		t.Error("missing hash accepted")
	}

	if _, err := p.Complete(context.Background(), pay.CallbackInput{Form: approvedCallback()}, pay.Attempt{Ref: "ORDERX"}); err == nil {
		t.Error("callback for another attempt accepted")
	}

	noMD := approvedCallback()
	noMD.Set("hashparams", "clientid:oid:authcode:procreturncode:response:")
	sign(noMD)
	if _, err := p.Complete(context.Background(), pay.CallbackInput{Form: noMD}, a); err == nil {
		t.Error("hashparams without mdstatus accepted")
	}
}

func TestQuery(t *testing.T) {
	for status, want := range map[string]pay.Status{"APPROVED": pay.StatusSuccess, "DECLINED": pay.StatusFailure,
		"VOID": pay.StatusCancelled, "REFUND": pay.StatusCancelled, "WAITING": pay.StatusPending} {
		b := newBank(t, func(string) string { return inquiryXML(status, "120") })
		res, err := newTest(t, b.srv.URL, nil).Query(context.Background(), pay.Attempt{Ref: "ORDERPAY", Currency: "TRY"})
		if err != nil || res.Status != want {
			t.Errorf("%s: res = %+v err = %v", status, res, err)
		}
	}
}

func txnXML(code, reason string) string {
	return `<GVPSResponse><Transaction><Response><Code>` + code + `</Code><ReasonCode>` + reason +
		`</ReasonCode></Response><RetrefNum>REF1</RetrefNum></Transaction></GVPSResponse>`
}

func TestRefund(t *testing.T) {
	a := pay.Attempt{Ref: "ORDERPAY", Amount: 120, PaidAmount: 120, Currency: "TRY"}

	b := newBank(t, func(string) string { return txnXML("00", "") })
	res, err := newTest(t, b.srv.URL, nil).Refund(context.Background(), pay.RefundRequest{Attempt: a, Amount: 120, Full: true})
	if err != nil || res.Status != pay.StatusSuccess || strings.Join(b.types, ",") != "void" {
		t.Fatalf("void: %+v %v %v", res, err, b.types)
	}

	b = newBank(t, func(typ string) string {
		if typ == "void" {
			return txnXML("99", "")
		}
		return txnXML("00", "")
	})
	res, err = newTest(t, b.srv.URL, nil).Refund(context.Background(), pay.RefundRequest{Attempt: a, Amount: 120, Full: true})
	if err != nil || res.Status != pay.StatusSuccess || strings.Join(b.types, ",") != "void,refund" {
		t.Fatalf("void then refund: %+v %v %v", res, err, b.types)
	}

	b = newBank(t, func(string) string { return txnXML("00", "") })
	if _, err := newTest(t, b.srv.URL, nil).Refund(context.Background(), pay.RefundRequest{Attempt: a, Amount: 50}); err != nil ||
		strings.Join(b.types, ",") != "refund" {
		t.Fatalf("partial: %v %v", err, b.types)
	}

	b = newBank(t, func(string) string { return txnXML("99", "05") })
	_, err = newTest(t, b.srv.URL, nil).Refund(context.Background(), pay.RefundRequest{Attempt: a, Amount: 50})
	var pe *pay.Error
	if !errors.As(err, &pe) || pe.Code != pay.CodeCardDeclined {
		t.Fatalf("declined refund: %v", err)
	}
}

func TestRefundTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
	}))
	defer srv.Close()
	p := newTest(t, srv.URL, &http.Client{Timeout: 50 * time.Millisecond})
	_, err := p.Refund(context.Background(), pay.RefundRequest{Attempt: pay.Attempt{Ref: "ORDERPAY", Currency: "TRY"}, Amount: 120, Full: true})
	if !errors.Is(err, pay.ErrUnknownOutcome) {
		t.Fatalf("err = %v", err)
	}
}
