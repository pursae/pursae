package paytr

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"wpgo/internal/pay"
)

const (
	fakeID   = "123456"
	fakeKey  = "fake-merchant-key"
	fakeSalt = "fake-merchant-salt"
)

func hmacB64(key, msg string) string {
	m := hmac.New(sha256.New, []byte(key))
	m.Write([]byte(msg))
	return base64.StdEncoding.EncodeToString(m.Sum(nil))
}

type capture struct {
	path string
	form url.Values
}

func newTest(t *testing.T, reply string, extra map[string]string) (pay.Provider, *capture) {
	t.Helper()
	c := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.path = r.URL.Path
		if r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
			t.Errorf("content type %q", r.Header.Get("Content-Type"))
		}
		r.ParseForm()
		c.form = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(reply))
	}))
	t.Cleanup(srv.Close)
	vals := map[string]string{"merchant_id": fakeID, "merchant_key": fakeKey, "merchant_salt": fakeSalt}
	for k, v := range extra {
		vals[k] = v
	}
	p, err := Spec().New(pay.Config{Values: vals, Test: true, HTTP: srv.Client(), BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	return p, c
}

// Known vectors from better-payment's PayTR tests (mews/pos test credentials).
func TestKnownVectors(t *testing.T) {
	p := &provider{merchantKey: "wWwU8buJp6jo1r25", salt: "YEUaNcdHXqyt7hjt"}
	s := p.salt
	if got := p.sign("123456" + "127.0.0.1" + "order-123" + "test@example.com" + "10050" + "W1siUHJvZHVjdCIsIjEwLjUwIiwxXV0=" + "0" + "0" + "TL" + "1" + s); got != "O5CaLankrigiiPDh3G75lLWdRX6wrEXGbLRjt37tuzY=" {
		t.Errorf("iframe token %s", got)
	}
	if got := p.sign("123456" + "order-789" + "5000" + s); got != "7ZqQEds0nem2gCqpLltwuNlmV9f7KHVlc73qeiNjwMg=" {
		t.Errorf("refund token %s", got)
	}
	if got := p.sign("123456" + "order-999" + s); got != "mIFJNpzMuo/gd9pcPNBinujmMVlEkR3DHZ6stLoQo/s=" {
		t.Errorf("status token %s", got)
	}
	in := pay.CallbackInput{Kind: pay.CallbackNotify, Form: url.Values{
		"merchant_oid": {"202606234E4E"}, "status": {"success"}, "total_amount": {"1001"},
		"hash": {"ZDVOQUw4aDJhNR5dWYBC5bD95bLtSOtj9DzzSQ9sUHs="}}}
	res, err := p.Complete(context.Background(), in, pay.Attempt{Ref: "202606234E4E"})
	if err != nil || res.Status != pay.StatusSuccess || res.PaidAmount != 1001 {
		t.Fatalf("callback vector: %+v %v", res, err)
	}
	b, _ := basket([]pay.Item{{Name: "Product", UnitPrice: 1050, Quantity: 1}}, "TRY")
	if b != "W1siUHJvZHVjdCIsIjEwLjUwIiwxXV0=" {
		t.Errorf("basket %s", b)
	}
}

func startReq() pay.StartRequest {
	return pay.StartRequest{
		Ref: "ABCD1234efgh", Amount: 34990, Currency: "TRY", MaxInstallment: 0,
		Customer: pay.Customer{Email: "buyer@example.com", FirstName: "Ayşe", LastName: "Yılmaz", Phone: "05550000000", IP: "203.0.113.7"},
		Billing:  pay.Address{Line1: "Test Sk. 1", City: "İstanbul", Country: "TR"},
		Items: []pay.Item{
			{Name: "Kitap <özel>", Quantity: 2, UnitPrice: 15000},
			{Name: "Kargo", Quantity: 1, UnitPrice: 4990},
		},
		ReturnURL: "https://shop.example/ok", FailURL: "https://shop.example/fail",
		Locale: "en", ExpiresIn: 45 * time.Minute,
	}
}

func TestStart(t *testing.T) {
	p, c := newTest(t, `{"status":"success","token":"abc123TOKEN"}`, map[string]string{"dark": "yes"})
	res, err := p.Start(context.Background(), startReq())
	if err != nil {
		t.Fatal(err)
	}
	if c.path != "/odeme/api/get-token" {
		t.Errorf("path %s", c.path)
	}
	wantBasket := base64.StdEncoding.EncodeToString([]byte(`[["Kitap <özel>","150.00",2],["Kargo","49.90",1]]`))
	want := map[string]string{
		"merchant_id": fakeID, "user_ip": "203.0.113.7", "merchant_oid": "ABCD1234efgh",
		"email": "buyer@example.com", "payment_amount": "34990", "user_basket": wantBasket,
		"debug_on": "1", "no_installment": "0", "max_installment": "0", "user_name": "Ayşe Yılmaz",
		"user_address": "Test Sk. 1 İstanbul TR", "user_phone": "05550000000",
		"merchant_ok_url": "https://shop.example/ok", "merchant_fail_url": "https://shop.example/fail",
		"timeout_limit": "45", "currency": "TL", "test_mode": "1", "lang": "en",
		"iframe_v2": "1", "iframe_v2_dark": "1",
	}
	want["paytr_token"] = hmacB64(fakeKey, fakeID+"203.0.113.7"+"ABCD1234efgh"+"buyer@example.com"+"34990"+wantBasket+"0"+"0"+"TL"+"1"+fakeSalt)
	for k, v := range want {
		if got := c.form.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	if len(c.form) != len(want) {
		t.Errorf("sent %d fields, want %d: %v", len(c.form), len(want), c.form)
	}
	for _, k := range []string{"merchant_key", "merchant_salt"} {
		if c.form.Has(k) {
			t.Errorf("%s sent", k)
		}
	}
	e := res.Embed
	if e == nil || e.FrameURL != "https://www.paytr.com/odeme/guvenli/abc123TOKEN" || res.ProviderRef != "abc123TOKEN" ||
		len(e.Scripts) != 1 || e.Scripts[0] != "https://www.paytr.com/js/iframeResizer.min.js?v2" ||
		e.Init != "iFrameResize({}, '#goo-pay-frame');" {
		t.Errorf("result %+v %+v", res, e)
	}
}

func TestStartInstallmentsAndDefaults(t *testing.T) {
	for _, tc := range []struct {
		max         int
		no, maxInst string
	}{{1, "1", "0"}, {0, "0", "0"}, {6, "0", "6"}, {20, "0", "12"}} {
		p, c := newTest(t, `{"status":"success","token":"t"}`, nil)
		r := startReq()
		r.MaxInstallment, r.ExpiresIn, r.Locale, r.FailURL = tc.max, 0, "tr", ""
		if _, err := p.Start(context.Background(), r); err != nil {
			t.Fatal(err)
		}
		if c.form.Get("no_installment") != tc.no || c.form.Get("max_installment") != tc.maxInst {
			t.Errorf("max %d: %s/%s", tc.max, c.form.Get("no_installment"), c.form.Get("max_installment"))
		}
		if c.form.Get("timeout_limit") != "30" || c.form.Get("lang") != "tr" || c.form.Has("iframe_v2_dark") ||
			c.form.Get("merchant_fail_url") != "https://shop.example/ok" {
			t.Errorf("defaults %v", c.form)
		}
	}
}

func TestStartErrors(t *testing.T) {
	p, _ := newTest(t, `{"status":"failed","reason":"Zorunlu alan degeri gecersiz: merchant_id"}`, nil)
	_, err := p.Start(context.Background(), startReq())
	var pe *pay.Error
	if !errors.As(err, &pe) || pe.ProviderMessage != "Zorunlu alan degeri gecersiz: merchant_id" {
		t.Fatalf("err %v", err)
	}
	r := startReq()
	r.Currency = "JPY"
	if _, err := p.Start(context.Background(), r); !errors.As(err, &pe) {
		t.Errorf("JPY accepted: %v", err)
	}
	p, _ = newTest(t, `{"status":"success","token":"../evil"}`, nil)
	if _, err := p.Start(context.Background(), startReq()); err == nil {
		t.Error("unsafe token accepted")
	}
}

func notify(f map[string]string) pay.CallbackInput {
	v := url.Values{}
	for k, x := range f {
		v.Set(k, x)
	}
	return pay.CallbackInput{Kind: pay.CallbackNotify, Method: "POST", Form: v}
}

func signed(oid, status, total string) map[string]string {
	return map[string]string{"merchant_oid": oid, "status": status, "total_amount": total,
		"hash": hmacB64(fakeKey, oid+fakeSalt+status+total)}
}

func TestComplete(t *testing.T) {
	p, _ := newTest(t, "", nil)
	ctx := context.Background()
	a := pay.Attempt{Ref: "ABCD1234efgh", Amount: 34990}

	f := signed(a.Ref, "success", "36500")
	f["installment_count"], f["currency"], f["payment_amount"] = "3", "USD", "1"
	if p.CallbackRef(notify(f)) != a.Ref {
		t.Error("CallbackRef")
	}
	res, err := p.Complete(ctx, notify(f), a)
	if err != nil || res.Status != pay.StatusSuccess || res.PaidAmount != 36500 || res.Currency != "" || res.Installment != 3 ||
		res.Ack == nil || res.Ack.Body != "OK" || res.Ack.ContentType != "text/plain" {
		t.Fatalf("success: %+v %v", res, err)
	}

	f = signed(a.Ref, "failed", "0")
	f["failed_reason_code"], f["failed_reason_msg"] = "6", "Odeme zaman asimina ugradi"
	res, err = p.Complete(ctx, notify(f), a)
	if err != nil || res.Status != pay.StatusFailure || res.Code != pay.CodeCancelledByUser ||
		res.ProviderCode != "6" || res.ProviderMessage != "Odeme zaman asimina ugradi" || res.Ack.Body != "OK" {
		t.Fatalf("failed: %+v %v", res, err)
	}

	bad := []map[string]string{}
	f = signed(a.Ref, "failed", "34990")
	f["status"] = "success"
	bad = append(bad, f)
	f = signed(a.Ref, "success", "100")
	f["total_amount"] = "34990"
	bad = append(bad, f)
	f = signed(a.Ref, "success", "34990")
	delete(f, "hash")
	bad = append(bad, f)
	for i, f := range bad {
		if _, err := p.Complete(ctx, notify(f), a); !errors.Is(err, pay.ErrInvalidSignature) {
			t.Errorf("case %d: %v", i, err)
		}
	}
	if _, err := p.Complete(ctx, notify(signed("OTHER12345", "success", "34990")), a); err == nil {
		t.Error("other merchant_oid accepted")
	}
	for _, total := range []string{"349.90", "-1", "1e5", " 34990"} {
		if _, err := p.Complete(ctx, notify(signed(a.Ref, "success", total)), a); err == nil {
			t.Errorf("total %q accepted", total)
		}
	}
	for _, k := range []pay.CallbackKind{pay.CallbackReturn, pay.CallbackFail} {
		in := notify(signed(a.Ref, "success", "34990"))
		in.Kind = k
		res, err := p.Complete(ctx, in, a)
		if err != nil || res.Status != pay.StatusPending {
			t.Errorf("%s: %+v %v", k, res, err)
		}
	}
}

func TestQuery(t *testing.T) {
	ctx := context.Background()
	a := pay.Attempt{Ref: "ABCD1234efgh"}
	p, c := newTest(t, `{"status":"success","payment_amount":"349,9","payment_total":"365.00","currency":"TL","taksit":"3","returns":[]}`, nil)
	res, err := p.Query(ctx, a)
	if err != nil || res.Status != pay.StatusSuccess || res.PaidAmount != 36500 || res.Currency != "TRY" || res.Installment != 3 {
		t.Fatalf("%+v %v", res, err)
	}
	if c.path != "/odeme/durum-sorgu" || c.form.Get("merchant_id") != fakeID || c.form.Get("merchant_oid") != a.Ref ||
		c.form.Get("paytr_token") != hmacB64(fakeKey, fakeID+a.Ref+fakeSalt) || len(c.form) != 3 {
		t.Errorf("request %s %v", c.path, c.form)
	}

	p, _ = newTest(t, `{"status":"success","payment_amount":"10.01","payment_total":"10.01","currency":"TL","returns":[{"return_amount":"5.00"},{"return_amount":"5,01"}]}`, nil)
	if res, err := p.Query(ctx, a); err != nil || res.Status != pay.StatusCancelled {
		t.Errorf("refunded: %+v %v", res, err)
	}
	p, _ = newTest(t, `{"status":"success","payment_total":"10.01","currency":"TL","returns":[{"return_amount":"5.00"}]}`, nil)
	if res, err := p.Query(ctx, a); err != nil || res.Status != pay.StatusSuccess {
		t.Errorf("partial: %+v %v", res, err)
	}
	p, _ = newTest(t, `{"status":"error","err_no":"004","err_msg":"merchant_oid ile basarili odeme bulunamadi"}`, nil)
	if res, err := p.Query(ctx, a); err != nil || res.Status != pay.StatusPending {
		t.Errorf("004: %+v %v", res, err)
	}
	p, _ = newTest(t, `{"status":"error","err_no":"001","err_msg":"x"}`, nil)
	var pe *pay.Error
	if _, err := p.Query(ctx, a); !errors.As(err, &pe) || pe.ProviderCode != "001" {
		t.Errorf("error: %v", err)
	}
}

func TestRefund(t *testing.T) {
	ctx := context.Background()
	r := pay.RefundRequest{Attempt: pay.Attempt{Ref: "ABCD1234efgh", Currency: "TRY"}, Amount: 5000, RefundRef: "RF1234"}
	p, c := newTest(t, `{"status":"success","is_test":1,"merchant_oid":"ABCD1234efgh","return_amount":"50.00","reference_no":"RF1234"}`, nil)
	res, err := p.Refund(ctx, r)
	if err != nil || res.Status != pay.StatusSuccess || res.PaidAmount != 5000 || res.TxnID != "RF1234" {
		t.Fatalf("%+v %v", res, err)
	}
	if c.path != "/odeme/iade" || c.form.Get("return_amount") != "50.00" || c.form.Get("reference_no") != "RF1234" ||
		c.form.Get("paytr_token") != hmacB64(fakeKey, fakeID+"ABCD1234efgh"+"50.00"+fakeSalt) || len(c.form) != 5 {
		t.Errorf("request %s %v", c.path, c.form)
	}
	p, _ = newTest(t, `{"status":"error","err_no":"006","err_msg":"Toplam iade tutari odeme tutarindan fazla olamaz"}`, nil)
	var pe *pay.Error
	if _, err := p.Refund(ctx, r); !errors.As(err, &pe) || pe.Code != pay.CodeInvalidRequest {
		t.Errorf("006: %v", err)
	}
}

func TestRefundTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
	}))
	defer srv.Close()
	cl := srv.Client()
	cl.Timeout = 50 * time.Millisecond
	p, _ := Spec().New(pay.Config{Values: map[string]string{"merchant_id": fakeID, "merchant_key": fakeKey, "merchant_salt": fakeSalt},
		HTTP: cl, BaseURL: srv.URL})
	_, err := p.Refund(context.Background(), pay.RefundRequest{Attempt: pay.Attempt{Ref: "ABCD1234efgh", Currency: "TRY"}, Amount: 100})
	if !errors.Is(err, pay.ErrUnknownOutcome) {
		t.Fatalf("err %v", err)
	}
}

func TestNewRequires(t *testing.T) {
	if _, err := Spec().New(pay.Config{Values: map[string]string{"merchant_id": fakeID, "merchant_key": fakeKey}}); err == nil {
		t.Error("missing salt accepted")
	}
}
