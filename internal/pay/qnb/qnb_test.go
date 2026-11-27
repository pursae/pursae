package qnb

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"wpgo/internal/pay"
)

const (
	tMerchant = "000000000000001"
	tUser     = "FAKE_API_USER"
	tPass     = "fake-api-pass"
	tKey      = "FAKEMERCHANTPASS"
	tRef      = "REF12345678"
)

func newP(t *testing.T, base string) *provider {
	t.Helper()
	pr, err := Spec().New(pay.Config{
		Values: map[string]string{"merchant_id": tMerchant, "user_code": tUser, "user_pass": tPass, "merchant_pass": tKey},
		Test:   true, HTTP: &http.Client{Timeout: 500 * time.Millisecond}, BaseURL: base,
	})
	if err != nil {
		t.Fatal(err)
	}
	return pr.(*provider)
}

func TestSpecRequiresFields(t *testing.T) {
	if _, err := Spec().New(pay.Config{Values: map[string]string{"merchant_id": "1"}}); err == nil {
		t.Fatal("incomplete settings accepted")
	}
}

// Known vectors: better-payment tests/fixtures/qnb.ts (Python hashlib) and mews/pos
// tests/Unit/Crypt/PayForPosCryptTest.php (the 3DHost callback).
func TestHashReferenceVectors(t *testing.T) {
	if got := FormHash("ORDER3D", "1.20", "https://example.com/callback", "https://example.com/callback",
		"Auth", "0", "0123456789abcdef0123", "12345678"); got != "Zf29qjUJV9sxtrOZspFcjvecWxc=" {
		t.Fatalf("better-payment form hash: %s", got)
	}
	if got := FormHash("2020110828BC", "100.01", "http://localhost/finansbank-payfor/3d/response.php",
		"http://localhost/finansbank-payfor/3d/response.php", "Auth", "0", "0.43625700 1604831630",
		"12345678"); got != "zmSUxYPhmCj7QOzqpk/28LuE1Oc=" {
		t.Fatalf("mews form hash: %s", got)
	}
	if got := ResponseHash("085300000009597", "12345678", "ORDERPAY", "S59045", "00", "1",
		"PF639273336051392940", "QNB_API_KULLANICI"); got != "3Yllopti5IiA/HlqGAZkwGQw7Oo=" {
		t.Fatalf("better-payment 3DPay response hash: %s", got)
	}
	if got := ResponseHash("085300000009704", "12345678", "2022103121CA", "S28031", "00", "1",
		"PF638028543970517651", "QNB_API_KULLANICI_3DPAY"); got != "R3sOuVFWDP8nvu9mtVtvLxwlqow=" {
		t.Fatalf("mews 3DHost response hash: %s", got)
	}
}

func sha(s string) string {
	h := sha1.Sum([]byte(s))
	return base64.StdEncoding.EncodeToString(h[:])
}

func TestHashIndependent(t *testing.T) {
	if FormHash("O", "1.00", "ok", "fail", "Auth", "3", "r", "k") != sha("5O1.00okfailAuth3rk") {
		t.Fatal("form hash")
	}
	if ResponseHash("m", "k", "O", "A", "00", "1", "R", "u") != sha("mkOA001Ru") {
		t.Fatal("response hash")
	}
}

func formMap(f *pay.Form) map[string]string {
	m := map[string]string{}
	for _, kv := range f.Fields {
		m[kv[0]] = kv[1]
	}
	return m
}

func TestStart(t *testing.T) {
	p := newP(t, "")
	res, err := p.Start(context.Background(), pay.StartRequest{Ref: tRef, Amount: 34990, Currency: "TRY",
		Installment: 3, ReturnURL: "https://shop.test/ok", FailURL: "https://shop.test/fail", Locale: "en"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Form == nil || res.Form.Action != testOrigin+"/Gateway/3DHost.aspx" {
		t.Fatalf("action %+v", res.Form)
	}
	var names []string
	for _, kv := range res.Form.Fields {
		names = append(names, kv[0])
	}
	want := "MbrId MerchantID UserCode OrderId Lang SecureType TxnType PurchAmount InstallmentCount Currency OkUrl FailUrl Rnd Hash"
	if strings.Join(names, " ") != want {
		t.Fatalf("fields %v", names)
	}
	m := formMap(res.Form)
	for _, card := range []string{"Pan", "Cvv2", "Expiry", "CardHolderName", "CardType", "UserPass"} {
		if _, ok := m[card]; ok {
			t.Fatalf("form carries %s", card)
		}
	}
	exp := map[string]string{"MbrId": "5", "MerchantID": tMerchant, "UserCode": tUser, "OrderId": tRef,
		"Lang": "EN", "SecureType": "3DHost", "TxnType": "Auth", "PurchAmount": "349.90",
		"InstallmentCount": "3", "Currency": "949", "OkUrl": "https://shop.test/ok", "FailUrl": "https://shop.test/fail"}
	for k, v := range exp {
		if m[k] != v {
			t.Errorf("%s = %q, want %q", k, m[k], v)
		}
	}
	if m["Rnd"] == "" || m["Hash"] != sha("5"+tRef+"349.90https://shop.test/okhttps://shop.test/failAuth3"+m["Rnd"]+tKey) {
		t.Fatal("hash")
	}

	single, _ := p.Start(context.Background(), pay.StartRequest{Ref: tRef, Amount: 100, Currency: "USD",
		ReturnURL: "https://shop.test/ok"})
	sm := formMap(single.Form)
	if sm["InstallmentCount"] != "0" || sm["Currency"] != "840" || sm["FailUrl"] != "https://shop.test/ok" || sm["Lang"] != "TR" {
		t.Fatalf("single %v", sm)
	}
	if _, err := p.Start(context.Background(), pay.StartRequest{Ref: tRef, Amount: 1, Currency: "JPY"}); err == nil {
		t.Fatal("JPY accepted")
	}
	for _, h := range Spec().Hosts.Form {
		if h == liveOrigin {
			return
		}
	}
	t.Fatal("live origin not in Hosts.Form")
}

// fakeBank answers PayforRequests by TxnType.
type fakeBank struct {
	mu      sync.Mutex
	answers map[string]string
	got     []string
	delay   time.Duration
}

func (b *fakeBank) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	s := string(body)
	b.mu.Lock()
	b.got = append(b.got, s)
	b.mu.Unlock()
	if b.delay > 0 {
		time.Sleep(b.delay)
	}
	for txn, ans := range b.answers {
		if strings.Contains(s, "<TxnType>"+txn+"</TxnType>") {
			io.WriteString(w, `<?xml version="1.0" encoding="utf-8"?><PayforResponse>`+ans+`</PayforResponse>`)
			return
		}
	}
	http.Error(w, "no", 500)
}

func server(t *testing.T, b *fakeBank) string {
	s := httptest.NewServer(b)
	t.Cleanup(s.Close)
	return s.URL
}

func inquiry(over string) string {
	if over != "" {
		return over
	}
	return `<OrderId>` + tRef + `</OrderId><TxnResult>Success</TxnResult><ProcReturnCode>00</ProcReturnCode>` +
		`<HostRefNum>628416660077</HostRefNum><TxnStatus>Y</TxnStatus><IsVoided>false</IsVoided>` +
		`<IsRefunded>false</IsRefunded><PurchAmount>349.90</PurchAmount><Currency>949</Currency>` +
		`<InstallmentCount>3</InstallmentCount><TxnType>Auth</TxnType>`
}

func callback(over map[string]string) url.Values {
	f := map[string]string{"OrderId": tRef, "MerchantID": tMerchant, "SecureType": "3DHost", "3DStatus": "1",
		"ProcReturnCode": "00", "AuthCode": "S28031", "PurchAmount": "349.90", "Currency": "949",
		"ResponseRnd": "PF638028543970517651", "ErrMsg": "Onaylandı"}
	for k, v := range over {
		f[k] = v
	}
	f["ResponseHash"] = ResponseHash(tMerchant, tKey, f["OrderId"], f["AuthCode"], f["ProcReturnCode"],
		f["3DStatus"], f["ResponseRnd"], tUser)
	v := url.Values{}
	for k, x := range f {
		v.Set(k, x)
	}
	return v
}

var att = pay.Attempt{Ref: tRef, Amount: 34990, Currency: "TRY", Installment: 3}

func complete(p *provider, form url.Values, a pay.Attempt) (*pay.Result, error) {
	return p.Complete(context.Background(), pay.CallbackInput{Kind: pay.CallbackReturn, Method: "POST", Form: form}, a)
}

func TestCallbackRef(t *testing.T) {
	if got := newP(t, "").CallbackRef(pay.CallbackInput{Form: callback(nil)}); got != tRef {
		t.Fatal(got)
	}
}

func TestCompleteApproved(t *testing.T) {
	b := &fakeBank{answers: map[string]string{"OrderInquiry": inquiry("")}}
	p := newP(t, server(t, b))
	r, err := complete(p, callback(nil), att)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != pay.StatusSuccess || r.PaidAmount != 34990 || r.Currency != "TRY" ||
		r.TxnID != "628416660077" || r.Installment != 3 {
		t.Fatalf("%+v", r)
	}
	req := b.got[0]
	for _, s := range []string{"<OrgOrderId>" + tRef + "</OrgOrderId>", "<SecureType>Inquiry</SecureType>",
		"<UserPass>" + tPass + "</UserPass>", "<MbrId>5</MbrId>", "<MerchantId>" + tMerchant + "</MerchantId>"} {
		if !strings.Contains(req, s) {
			t.Fatalf("inquiry lacks %s: %s", s, req)
		}
	}
}

func TestCompleteDeclined(t *testing.T) {
	p := newP(t, "http://127.0.0.1:1")
	for code, want := range map[string]pay.Code{"51": pay.CodeInsufficientFunds, "54": pay.CodeExpiredCard,
		"05": pay.CodeCardDeclined, "V034": pay.Code3DSFailed, "XX": pay.CodeUnknown} {
		r, err := complete(p, callback(map[string]string{"ProcReturnCode": code, "AuthCode": ""}), att)
		if err != nil || r.Status != pay.StatusFailure || r.Code != want || r.ProviderCode != code {
			t.Fatalf("%s: %+v %v", code, r, err)
		}
	}
	r, err := complete(p, callback(map[string]string{"3DStatus": "0", "ProcReturnCode": "V034"}), att)
	if err != nil || r.Status != pay.StatusFailure || r.Code != pay.Code3DSFailed {
		t.Fatalf("3d: %+v %v", r, err)
	}
}

func TestCompleteRejections(t *testing.T) {
	p := newP(t, "http://127.0.0.1:1")
	missing := callback(nil)
	missing.Del("ResponseHash")
	if _, err := complete(p, missing, att); !errors.Is(err, pay.ErrInvalidSignature) {
		t.Fatalf("missing hash: %v", err)
	}
	tamperedCode := callback(map[string]string{"ProcReturnCode": "51"})
	tamperedCode.Set("ProcReturnCode", "00")
	if _, err := complete(p, tamperedCode, att); !errors.Is(err, pay.ErrInvalidSignature) {
		t.Fatalf("tampered code: %v", err)
	}
	tamperedID := callback(nil)
	tamperedID.Set("OrderId", "REF99999999")
	if _, err := complete(p, tamperedID, pay.Attempt{Ref: "REF99999999", Amount: 34990, Currency: "TRY"}); !errors.Is(err, pay.ErrInvalidSignature) {
		t.Fatalf("tampered order id: %v", err)
	}
	other := callback(map[string]string{"OrderId": "REF00000000"})
	if _, err := complete(p, other, att); err == nil {
		t.Fatal("order id of another attempt accepted")
	}
	wrongMerchant := callback(map[string]string{"MerchantID": "999"})
	if _, err := complete(p, wrongMerchant, att); err == nil {
		t.Fatal("other merchant accepted")
	}
}

// The amount is not in the ResponseHash: a tampered PurchAmount in the callback is ignored and the
// paid amount comes from the bank's inquiry, which the bridge compares with the attempt.
func TestCompleteTamperedAmount(t *testing.T) {
	b := &fakeBank{answers: map[string]string{"OrderInquiry": strings.Replace(inquiry(""),
		"<PurchAmount>349.90</PurchAmount>", "<PurchAmount>1.00</PurchAmount>", 1)}}
	p := newP(t, server(t, b))
	form := callback(nil)
	form.Set("PurchAmount", "349.90")
	r, err := complete(p, form, att)
	if err != nil || r.PaidAmount != 100 {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestCompleteBankDisagrees(t *testing.T) {
	for name, inq := range map[string]string{
		"voided":  strings.Replace(inquiry(""), "<IsVoided>false", "<IsVoided>true", 1),
		"failed":  `<OrderId>` + tRef + `</OrderId><ProcReturnCode>00</ProcReturnCode><TxnResult>Failed</TxnResult><TxnStatus>N</TxnStatus>`,
		"other":   strings.Replace(inquiry(""), tRef, "REF00000000", 1),
		"unknown": `<ProcReturnCode>V013</ProcReturnCode><ErrMsg>Seçili İşlem Bulunamadı!</ErrMsg>`,
	} {
		b := &fakeBank{answers: map[string]string{"OrderInquiry": inq}}
		if _, err := complete(newP(t, server(t, b)), callback(nil), att); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
}

func TestQuery(t *testing.T) {
	cases := map[string]pay.Status{
		inquiry(""): pay.StatusSuccess,
		strings.Replace(inquiry(""), "<IsRefunded>false", "<IsRefunded>true", 1): pay.StatusCancelled,
		`<ProcReturnCode>V013</ProcReturnCode>`:                                  pay.StatusFailure,
	}
	for inq, want := range cases {
		b := &fakeBank{answers: map[string]string{"OrderInquiry": inq}}
		r, err := newP(t, server(t, b)).Query(context.Background(), att)
		if err != nil || r.Status != want {
			t.Fatalf("%s: %+v %v", inq, r, err)
		}
		if want == pay.StatusSuccess && r.PaidAmount != 34990 {
			t.Fatalf("paid %d", r.PaidAmount)
		}
	}
}

func refundReq(full bool, amount int64) pay.RefundRequest {
	return pay.RefundRequest{Attempt: pay.Attempt{Ref: tRef, Amount: 34990, Currency: "TRY", PaidAmount: 34990},
		Amount: amount, Full: full, RefundRef: "RF1"}
}

const ok = `<ProcReturnCode>00</ProcReturnCode><HostRefNum>230423103898</HostRefNum><AuthCode>S05229</AuthCode>`

func TestRefundVoid(t *testing.T) {
	b := &fakeBank{answers: map[string]string{"Void": ok}}
	r, err := newP(t, server(t, b)).Refund(context.Background(), refundReq(true, 34990))
	if err != nil || r.Status != pay.StatusSuccess || r.TxnID != "230423103898" || len(b.got) != 1 {
		t.Fatalf("%+v %v", r, err)
	}
	if !strings.Contains(b.got[0], "<OrgOrderId>"+tRef+"</OrgOrderId>") {
		t.Fatal(b.got[0])
	}
}

func TestRefundVoidThenRefund(t *testing.T) {
	b := &fakeBank{answers: map[string]string{"Void": `<ProcReturnCode>V014</ProcReturnCode>`, "Refund": ok}}
	r, err := newP(t, server(t, b)).Refund(context.Background(), refundReq(true, 34990))
	if err != nil || r.Status != pay.StatusSuccess || len(b.got) != 2 || !strings.Contains(b.got[1], "<PurchAmount>349.90</PurchAmount>") {
		t.Fatalf("%+v %v %v", r, err, b.got)
	}
}

func TestRefundPartial(t *testing.T) {
	b := &fakeBank{answers: map[string]string{"Refund": ok, "Void": ok}}
	r, err := newP(t, server(t, b)).Refund(context.Background(), refundReq(false, 1000))
	if err != nil || r.Status != pay.StatusSuccess || r.PaidAmount != 1000 || len(b.got) != 1 ||
		!strings.Contains(b.got[0], "<TxnType>Refund</TxnType>") || !strings.Contains(b.got[0], "<PurchAmount>10.00</PurchAmount>") {
		t.Fatalf("%+v %v %v", r, err, b.got)
	}
	b2 := &fakeBank{answers: map[string]string{"Refund": `<ProcReturnCode>05</ProcReturnCode><ErrMsg>Red</ErrMsg>`}}
	r, err = newP(t, server(t, b2)).Refund(context.Background(), refundReq(false, 1000))
	if err != nil || r.Status != pay.StatusFailure || r.Code != pay.CodeCardDeclined {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestRefundTimeout(t *testing.T) {
	b := &fakeBank{answers: map[string]string{"Refund": ok}, delay: 2 * time.Second}
	_, err := newP(t, server(t, b)).Refund(context.Background(), refundReq(false, 1000))
	if !errors.Is(err, pay.ErrUnknownOutcome) {
		t.Fatalf("%v", err)
	}
	b2 := &fakeBank{answers: map[string]string{"Refund": "<oops"}}
	_, err = newP(t, server(t, b2)).Refund(context.Background(), refundReq(false, 1000))
	if !errors.Is(err, pay.ErrUnknownOutcome) {
		t.Fatalf("garbled: %v", err)
	}
}
