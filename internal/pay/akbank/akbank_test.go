package akbank

import (
	"context"
	"crypto/hmac"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
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

const (
	fakeMerchant = "FAKEMERCHANTSAFEID00000000000001"
	fakeTerminal = "FAKETERMINALSAFEID00000000000001"
	fakeSecret   = "fake-secret-key"
)

func hmacB64(key, msg string) string {
	m := hmac.New(sha512.New, []byte(key))
	m.Write([]byte(msg))
	return base64.StdEncoding.EncodeToString(m.Sum(nil))
}

// Known vectors from mews/pos (MIT) AkbankPosCryptTest, with its test account.
func TestKnownVectors(t *testing.T) {
	const key = "3230323330393034313735303032363031353172675f357637355f3273387373745f7233725f73323333383737335f323272383774767276327672323531355f"
	body := `{"terminal":{"merchantSafeId":"2023090417500272654BD9A49CF07574","terminalSafeId":"2023090417500284633D137A249DBBEB"},"version":"1.00","txnCode":"1010","requestDateTime":"2024-04-20T13:48:02.000","randomNumber":"59EED19EC4FA761B8D147F5175C915EFD69D193ED96114F9690505EDB02FF5FD3CB161A15FD5EFFB294177291DC27B2A9F58FB1DA6F2F4617762AF180A023A33","order":{"orderId":"2024042053E2"}}`
	if got := sign(key, body); got != "D07V9wUXfyPbn+L8jqd9plMfJKT3gSy22Z+yruzWsvuJBkQ63fz+wcWjTF2XurVjYtnR7wLGYTJYg0xjDCYAHA==" {
		t.Errorf("auth-hash %s", got)
	}
	host := map[string]string{
		"paymentModel": "MODEL_3D_HOST", "txnCode": "3000", "merchantSafeId": "2023090417500272654BD9A49CF07574",
		"terminalSafeId": "2023090417500284633D137A249DBBEB", "orderId": "20240404A4B0", "lang": "TR", "amount": "1.01",
		"ccbRewardAmount": "1.00", "pcbRewardAmount": "1.00", "xcbRewardAmount": "1.00", "currencyCode": "949",
		"installCount": "1", "okUrl": "http://localhost/akbankpos/3d/response.php",
		"failUrl": "http://localhost/akbankpos/3d/response.php", "emailAddress": "test@test.com",
		"randomNumber":    "AEDDD8688E11A3DC588DAB2ED59B2F64D45E798761CEFF17F4DB47581072697890180C4195986250F89C2C67A04A3B96F0AC66AE99B49BB7BEE618FBD621C4CD",
		"requestDateTime": "2024-04-04T21:11:41.000",
	}
	if got := formHash(key, host); got != "36uUlZFNi+Fs928RPt9CUfKfq+2hTWKK6idWFXjwnO47Qz4pVk6bliSUY+4RGw8Fzcqz+MUWQQ2Grz2zk4l9og==" {
		t.Errorf("3d host form hash %s", got)
	}
	p := &provider{secret: key, merchantID: "2023090417500272654BD9A49CF07574", terminal: "2023090417500284633D137A249DBBEB"}
	ok := url.Values{}
	for k, v := range map[string]string{
		"txnCode": "1000", "responseCode": "VPS-0000", "responseMessage": "BAŞARILI", "hostResponseCode": "00",
		"hostMessage": "000 ONAY KODU XXXXXX", "txnDateTime": "2024-04-18T20:56:28.000",
		"merchantSafeId": "2023090417500272654BD9A49CF07574", "terminalSafeId": "2023090417500284633D137A249DBBEB",
		"cardHolderName": "TD**", "orderId": "2024041898FD", "authCode": "306460", "rrn": "411024360239",
		"batchNumber": "43", "stan": "90", "additionalInstallCount": "0", "deferingMonth": "2",
		"ccbEarnedRewardAmount": "0.01", "ccbBalanceRewardAmount": "215.66", "ccbRewardDesc": "CHIP PARA",
		"pcbEarnedRewardAmount": "0.00", "pcbBalanceRewardAmount": "0.00", "pcbRewardDesc": "",
		"xcbEarnedRewardAmount": "0.00", "xcbBalanceRewardAmount": "0.00", "xcbRewardDesc": "",
		"hashParams": "txnCode+responseCode+responseMessage+hostResponseCode+hostMessage+txnDateTime+merchantSafeId+terminalSafeId+orderId+cardHolderName+authCode+rrn+batchNumber+stan+additionalInstallCount+deferingMonth+ccbEarnedRewardAmount+ccbBalanceRewardAmount+ccbRewardDesc+pcbEarnedRewardAmount+pcbBalanceRewardAmount+xcbEarnedRewardAmount+xcbBalanceRewardAmount",
		"hash":       "o8cVLBkljHc+1Icpoa35agcd7UPvD57gFY+bp7MQYznHnOr3f71vvsJwnbIQ2hGGVJj1nAdkZrh/lcVCdtMA+g==",
	} {
		ok.Set(k, v)
	}
	if !p.verifyCallback(ok, "responseCode", "orderId", "merchantSafeId", "terminalSafeId") {
		t.Error("3d host success callback vector")
	}
	fail := url.Values{"txnCode": {""}, "responseCode": {"VPS-1279"}, "responseMessage": {"BKM 3DS Server Doğrulama Başarısız"},
		"hostResponseCode": {""}, "hostMessage": {""}, "merchantSafeId": {""}, "terminalSafeId": {""}, "orderId": {"20240418452F"},
		"hashParams": {"responseCode+responseMessage+orderId"},
		"hash":       {"iW4wwqGV9h1ydKhpvhQ9Qeq2pOW+/HK0OB1AXoUlt/WlajbphFswh4Jy6BxTl4RAb2OICUnw+gy3UBPudHl5YA=="}}
	res, err := p.Complete(context.Background(), pay.CallbackInput{Kind: pay.CallbackFail, Form: fail}, pay.Attempt{Ref: "20240418452F"})
	if err != nil || res.Status != pay.StatusFailure || res.Code != pay.Code3DSFailed || res.ProviderCode != "VPS-1279" {
		t.Errorf("3d host failure vector: %+v %v", res, err)
	}
}

type bank struct {
	t       *testing.T
	reqs    []map[string]any
	replies map[string]string // txnCode → JSON reply
	status  int
}

func (b *bank) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	if r.URL.Path != "/transaction/process" || r.Header.Get("Content-Type") != "application/json" {
		b.t.Errorf("request %s %s", r.URL.Path, r.Header.Get("Content-Type"))
	}
	if r.Header.Get("auth-hash") != hmacB64(fakeSecret, string(body)) {
		b.t.Errorf("auth-hash mismatch")
	}
	var m map[string]any
	json.Unmarshal(body, &m)
	b.reqs = append(b.reqs, m)
	if b.status != 0 {
		w.WriteHeader(b.status)
	}
	w.Write([]byte(b.replies[m["txnCode"].(string)]))
}

func newTest(t *testing.T, replies map[string]string) (*provider, *bank) {
	t.Helper()
	b := &bank{t: t, replies: replies}
	srv := httptest.NewServer(b)
	t.Cleanup(srv.Close)
	p, err := Spec().New(pay.Config{Values: map[string]string{"merchant_safe_id": fakeMerchant, "terminal_safe_id": fakeTerminal,
		"secret_key": fakeSecret}, Test: true, HTTP: srv.Client(), BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	return p.(*provider), b
}

func startReq() pay.StartRequest {
	return pay.StartRequest{Ref: "ABCD1234efgh", Amount: 34990, Currency: "TRY", Locale: "en",
		Customer:  pay.Customer{Email: "buyer@example.com", IP: "203.0.113.7"},
		Items:     []pay.Item{{Name: "Kitap", Quantity: 1, UnitPrice: 34990}},
		ReturnURL: "https://shop.example/ok", FailURL: "https://shop.example/fail"}
}

var cardFields = []string{"creditcard", "cardnumber", "cvv", "cvv2", "expiredate", "expireddate", "pan", "expiry", "cardholdername"}

func TestStart(t *testing.T) {
	p, b := newTest(t, nil)
	res, err := p.Start(context.Background(), startReq())
	if err != nil {
		t.Fatal(err)
	}
	if len(b.reqs) != 0 {
		t.Error("Start made a request")
	}
	f := res.Form
	if f == nil || f.Action != "https://virtualpospaymentgatewaypre.akbank.com/payhosting" || res.ProviderRef != "ABCD1234efgh" {
		t.Fatalf("result %+v", res)
	}
	names := []string{}
	got := map[string]string{}
	for _, kv := range f.Fields {
		names = append(names, kv[0])
		got[kv[0]] = kv[1]
		for _, c := range cardFields {
			if strings.ToLower(kv[0]) == c {
				t.Errorf("card field %s", kv[0])
			}
		}
	}
	want := "paymentModel txnCode merchantSafeId terminalSafeId orderId lang amount currencyCode installCount okUrl failUrl randomNumber requestDateTime hash"
	if strings.Join(names, " ") != want {
		t.Errorf("fields %v", names)
	}
	for k, v := range map[string]string{"paymentModel": "3D_PAY_HOSTING", "txnCode": "3000", "merchantSafeId": fakeMerchant,
		"terminalSafeId": fakeTerminal, "orderId": "ABCD1234efgh", "lang": "EN", "amount": "349.90", "currencyCode": "949",
		"installCount": "1", "okUrl": "https://shop.example/ok", "failUrl": "https://shop.example/fail"} {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	if len(got["randomNumber"]) != 128 || strings.ToUpper(got["randomNumber"]) != got["randomNumber"] {
		t.Errorf("randomNumber %q", got["randomNumber"])
	}
	if _, err := time.Parse("2006-01-02T15:04:05.000", got["requestDateTime"]); err != nil {
		t.Errorf("requestDateTime %q", got["requestDateTime"])
	}
	// Independent hash: the documented field order, card and reward fields empty.
	msg := "3D_PAY_HOSTING" + "3000" + fakeMerchant + fakeTerminal + "ABCD1234efgh" + "EN" + "349.90" + "" + "" + "" + "949" + "1" +
		"https://shop.example/ok" + "https://shop.example/fail" + "" + "" + "" + "" + "" + got["randomNumber"] + got["requestDateTime"] + ""
	if got["hash"] != hmacB64(fakeSecret, msg) {
		t.Error("form hash")
	}

	r := startReq()
	r.Installment, r.Locale, r.FailURL = 3, "tr", ""
	p.cfg.Test = false
	res, _ = p.Start(context.Background(), r)
	got = map[string]string{}
	for _, kv := range res.Form.Fields {
		got[kv[0]] = kv[1]
	}
	if got["installCount"] != "3" || got["lang"] != "TR" || got["failUrl"] != "https://shop.example/ok" ||
		res.Form.Action != "https://virtualpospaymentgateway.akbank.com/payhosting" {
		t.Errorf("live/installment %v %s", got, res.Form.Action)
	}
	r.Currency = "CHF"
	var pe *pay.Error
	if _, err := p.Start(context.Background(), r); !errors.As(err, &pe) {
		t.Errorf("CHF accepted: %v", err)
	}
}

// callback builds a signed okUrl post.
func callback(over map[string]string, params string) url.Values {
	f := map[string]string{"txnCode": "3000", "responseCode": "VPS-0000", "responseMessage": "BAŞARILI",
		"hostResponseCode": "00", "hostMessage": "000 ONAY KODU", "merchantSafeId": fakeMerchant,
		"terminalSafeId": fakeTerminal, "orderId": "ABCD1234efgh", "authCode": "123456", "rrn": "411024360239"}
	for k, v := range over {
		f[k] = v
	}
	if params == "" {
		params = "txnCode+responseCode+responseMessage+hostResponseCode+hostMessage+merchantSafeId+terminalSafeId+orderId+authCode+rrn"
	}
	var b strings.Builder
	for _, k := range strings.Split(params, "+") {
		b.WriteString(f[k])
	}
	v := url.Values{}
	for k, x := range f {
		v.Set(k, x)
	}
	v.Set("hashParams", params)
	v.Set("hash", hmacB64(fakeSecret, b.String()))
	return v
}

func history(status, amount string) string {
	return `{"txnCode":"1010","responseCode":"VPS-0000","txnDetailList":[{"txnCode":"3000","responseCode":"VPS-0000","txnStatus":"` +
		status + `","orderId":"ABCD1234efgh","amount":` + amount + `,"currencyCode":949,"installCount":1,"rrn":"411024360239"}]}`
}

func TestComplete(t *testing.T) {
	ctx := context.Background()
	a := pay.Attempt{Ref: "ABCD1234efgh", Amount: 34990, Currency: "TRY"}
	in := func(f url.Values) pay.CallbackInput {
		return pay.CallbackInput{Kind: pay.CallbackReturn, Method: "POST", Form: f}
	}

	p, b := newTest(t, map[string]string{"1010": history("N", "349.90")})
	if p.CallbackRef(in(callback(nil, ""))) != a.Ref {
		t.Error("CallbackRef")
	}
	res, err := p.Complete(ctx, in(callback(nil, "")), a)
	if err != nil || res.Status != pay.StatusSuccess || res.PaidAmount != 34990 || res.Currency != "TRY" || res.TxnID != "411024360239" {
		t.Fatalf("approved: %+v %v", res, err)
	}
	if len(b.reqs) != 1 || b.reqs[0]["order"].(map[string]any)["orderId"] != a.Ref {
		t.Errorf("history request %v", b.reqs)
	}

	// Akbank's record says a smaller amount: that is what is reported, the bridge refuses it.
	p, _ = newTest(t, map[string]string{"1010": history("N", "1.00")})
	if res, err := p.Complete(ctx, in(callback(nil, "")), a); err != nil || res.PaidAmount != 100 {
		t.Errorf("amount from history: %+v %v", res, err)
	}

	// Declined, signed.
	p, b = newTest(t, nil)
	res, err = p.Complete(ctx, in(callback(map[string]string{"responseCode": "VPS-1001", "hostResponseCode": "51",
		"hostMessage": "YETERSIZ BAKIYE"}, "")), a)
	if err != nil || res.Status != pay.StatusFailure || res.Code != pay.CodeInsufficientFunds || res.ProviderCode != "VPS-1001/51" ||
		res.ProviderMessage != "YETERSIZ BAKIYE" || len(b.reqs) != 0 {
		t.Errorf("declined: %+v %v", res, err)
	}

	p, _ = newTest(t, map[string]string{"1010": history("N", "349.90")})
	tampered := callback(nil, "")
	tampered.Set("orderId", "OTHER1234567")
	missing := callback(nil, "")
	missing.Del("hash")
	noTerm := callback(nil, "responseCode+orderId")
	wrongTerm := callback(map[string]string{"terminalSafeId": "SOMEOTHERTERMINAL"}, "")
	unsigned := callback(nil, "")
	unsigned.Set("hashParams", "txnCode")
	for name, f := range map[string]url.Values{"tampered order": tampered, "missing hash": missing,
		"approval without terminal": noTerm, "other terminal": wrongTerm, "orderId not signed": unsigned} {
		if _, err := p.Complete(ctx, in(f), a); !errors.Is(err, pay.ErrInvalidSignature) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := p.Complete(ctx, in(callback(map[string]string{"orderId": "OTHER1234567"}, "")), a); err == nil {
		t.Error("other attempt accepted")
	}

	// Callback approved but Akbank's record disagrees.
	for _, reply := range []string{history("V", "349.90"), `{"responseCode":"VPS-0000","txnDetailList":[]}`,
		`{"responseCode":"VPS-0000","txnDetailList":[{"txnCode":"3000","responseCode":"VPS-1001","orderId":"ABCD1234efgh"}]}`} {
		p, _ = newTest(t, map[string]string{"1010": reply})
		if _, err := p.Complete(ctx, in(callback(nil, "")), a); err == nil {
			t.Errorf("unconfirmed approval accepted: %s", reply)
		}
	}
}

func TestQuery(t *testing.T) {
	ctx := context.Background()
	a := pay.Attempt{Ref: "ABCD1234efgh"}
	p, b := newTest(t, map[string]string{"1010": `{"responseCode":"VPS-0000","txnDetailList":[{"txnCode":"3000","responseCode":"VPS-0000","txnStatus":"N","orderId":"ABCD1234efgh","amount":"100.50","currencyCode":"840","installCount":3,"rrn":"R1"}]}`})
	res, err := p.Query(ctx, a)
	if err != nil || res.Status != pay.StatusSuccess || res.PaidAmount != 10050 || res.Currency != "USD" || res.Installment != 3 || res.TxnID != "R1" {
		t.Fatalf("%+v %v", res, err)
	}
	req := b.reqs[0]
	if req["txnCode"] != "1010" || req["version"] != "1.00" || req["terminal"].(map[string]any)["merchantSafeId"] != fakeMerchant ||
		len(req["randomNumber"].(string)) != 128 || req["transaction"] != nil {
		t.Errorf("request %v", req)
	}
	p, _ = newTest(t, map[string]string{"1010": history("R", "349.90")})
	if res, err := p.Query(ctx, a); err != nil || res.Status != pay.StatusCancelled {
		t.Errorf("refunded: %+v %v", res, err)
	}
	p, _ = newTest(t, map[string]string{"1010": `{"responseCode":"VPS-0000","txnDetailList":[]}`})
	if res, err := p.Query(ctx, a); err != nil || res.Status != pay.StatusPending {
		t.Errorf("none: %+v %v", res, err)
	}
	p, _ = newTest(t, map[string]string{"1010": `{"responseCode":"VPS-1005","responseMessage":"Hatali istek"}`})
	var pe *pay.Error
	if _, err := p.Query(ctx, a); !errors.As(err, &pe) || pe.ProviderCode != "VPS-1005" {
		t.Errorf("error: %v", err)
	}
}

func refundReq(full bool, amount int64) pay.RefundRequest {
	return pay.RefundRequest{Attempt: pay.Attempt{Ref: "ABCD1234efgh", Currency: "TRY", PaidAmount: 34990}, Amount: amount, Full: full, RefundRef: "RF1"}
}

func TestRefund(t *testing.T) {
	ctx := context.Background()
	ok := `{"responseCode":"VPS-0000","transaction":{"rrn":"RRN1","authCode":"A1"}}`
	no := `{"responseCode":"VPS-1060","responseMessage":"Iptal edilemez"}`

	p, b := newTest(t, map[string]string{"1003": ok})
	res, err := p.Refund(ctx, refundReq(true, 34990))
	if err != nil || res.Status != pay.StatusSuccess || res.PaidAmount != 34990 || res.TxnID != "RRN1" || len(b.reqs) != 1 ||
		b.reqs[0]["txnCode"] != "1003" || b.reqs[0]["transaction"] != nil {
		t.Fatalf("void: %+v %v %v", res, err, b.reqs)
	}

	p, b = newTest(t, map[string]string{"1003": no, "1002": ok})
	res, err = p.Refund(ctx, refundReq(true, 34990))
	if err != nil || res.Status != pay.StatusSuccess || len(b.reqs) != 2 || b.reqs[1]["txnCode"] != "1002" {
		t.Fatalf("void then refund: %+v %v %v", res, err, b.reqs)
	}
	tx := b.reqs[1]["transaction"].(map[string]any)
	if tx["amount"] != "349.90" || tx["currencyCode"] != float64(949) {
		t.Errorf("refund transaction %v", tx)
	}

	p, b = newTest(t, map[string]string{"1002": ok})
	if res, err := p.Refund(ctx, refundReq(false, 1000)); err != nil || res.PaidAmount != 1000 || len(b.reqs) != 1 ||
		b.reqs[0]["txnCode"] != "1002" || b.reqs[0]["transaction"].(map[string]any)["amount"] != "10.00" {
		t.Errorf("partial: %+v %v %v", res, err, b.reqs)
	}

	p, _ = newTest(t, map[string]string{"1002": no})
	var pe *pay.Error
	if _, err := p.Refund(ctx, refundReq(false, 1000)); !errors.As(err, &pe) || pe.ProviderCode != "VPS-1060" {
		t.Errorf("rejected: %v", err)
	}

	p, b = newTest(t, map[string]string{"1002": "<html>"})
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
	p, _ := Spec().New(pay.Config{Values: map[string]string{"merchant_safe_id": fakeMerchant, "terminal_safe_id": fakeTerminal,
		"secret_key": fakeSecret}, HTTP: cl, BaseURL: srv.URL})
	for _, full := range []bool{true, false} {
		if _, err := p.Refund(context.Background(), refundReq(full, 34990)); !errors.Is(err, pay.ErrUnknownOutcome) {
			t.Errorf("full=%v: %v", full, err)
		}
	}
}

func TestNewRequires(t *testing.T) {
	if _, err := Spec().New(pay.Config{Values: map[string]string{"merchant_safe_id": fakeMerchant, "terminal_safe_id": fakeTerminal}}); err == nil {
		t.Error("missing secret accepted")
	}
}
