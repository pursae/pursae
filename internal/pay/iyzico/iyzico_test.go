package iyzico

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
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
	testAPIKey    = "sandbox-fake-api-key"
	testSecretKey = "sandbox-fake-secret-key"
	testRnd       = "1700000000000abcdef"
)

type call struct {
	path   string
	header http.Header
	body   []byte
}

// server answers each request with handler(path, body) and records the calls.
func newTestProvider(t *testing.T, handler func(path string, body map[string]any) any) (*provider, *[]call) {
	t.Helper()
	var calls []call
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		calls = append(calls, call{r.URL.Path, r.Header.Clone(), b})
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		w.Header().Set("Content-Type", "application/json")
		out := handler(r.URL.Path, m)
		if s, ok := out.(string); ok {
			io.WriteString(w, s)
			return
		}
		json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(srv.Close)
	pv, err := Spec().New(pay.Config{
		Values:  map[string]string{"api_key": testAPIKey, "secret_key": testSecretKey},
		Test:    true,
		HTTP:    &http.Client{Timeout: 2 * time.Second},
		BaseURL: srv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	p := pv.(*provider)
	p.rnd = func() string { return testRnd }
	return p, &calls
}

func hmacHex(key, msg string) string {
	m := hmac.New(sha256.New, []byte(key))
	m.Write([]byte(msg))
	return hex.EncodeToString(m.Sum(nil))
}

func TestAuthorizationKnownValue(t *testing.T) {
	body := []byte(`{"locale":"tr"}`)
	path := "/payment/cancel"
	sig := hmacHex(testSecretKey, testRnd+path+string(body))
	want := "IYZWSv2 " + base64.StdEncoding.EncodeToString([]byte("apiKey:"+testAPIKey+"&randomKey:"+testRnd+"&signature:"+sig))
	if got := authorization(testAPIKey, testSecretKey, testRnd, path, body); got != want {
		t.Fatalf("got %s want %s", got, want)
	}
	// Fixed vector computed with openssl, independent of this package.
	const fixed = "IYZWSv2 YXBpS2V5OnNhbmRib3gtZmFrZS1hcGkta2V5JnJhbmRvbUtleToxNzAwMDAwMDAwMDAwYWJjZGVmJnNpZ25hdHVyZToyYTYwNWRkMDRmYzFiZDhiZTMzYjhjNGFjZTc0NDU2N2ZkMWE0MzA4ZjZiMTZlOGVhY2IwNWYxZWIwMWFlNGUw"
	if want != fixed {
		t.Fatal("fixed vector")
	}
}

func startRequest() pay.StartRequest {
	addr := pay.Address{Name: "Ada Yılmaz", Line1: "Atatürk Cd. 1", Line2: "D:2", City: "İstanbul", Postcode: "34000", Country: "TR"}
	return pay.StartRequest{
		Ref: "REF12345678", OrderNumber: "1042", Amount: 12050, Currency: "TRY", MaxInstallment: 1,
		Customer: pay.Customer{ID: "7", Email: "ada@example.com", FirstName: "Ada", LastName: "Yılmaz", Phone: "+905550000000", IP: "203.0.113.5"},
		Billing:  addr, Shipping: addr,
		Items: []pay.Item{
			{ID: "sku1", Name: "Mug", Quantity: 2, UnitPrice: 5000},
			{ID: "shipping", Name: "Shipping", Quantity: 1, UnitPrice: 2050, Virtual: true},
		},
		ReturnURL: "https://shop.example/pay/return?a=1",
		Locale:    "en",
	}
}

func initOK(conv, token string) map[string]any {
	return map[string]any{
		"status": "success", "conversationId": conv, "token": token,
		"paymentPageUrl": "https://sandbox-cpp.iyzipay.com?token=" + token,
		"signature":      hmacHex(testSecretKey, conv+":"+token),
	}
}

func TestStart(t *testing.T) {
	p, calls := newTestProvider(t, func(path string, b map[string]any) any { return initOK("REF12345678", "tok-1") })
	res, err := p.Start(context.Background(), startRequest())
	if err != nil {
		t.Fatal(err)
	}
	if res.Redirect != "https://sandbox-cpp.iyzipay.com?token=tok-1" || res.ProviderRef != "tok-1" || res.Form != nil || res.Embed != nil {
		t.Fatalf("result %+v", res)
	}
	c := (*calls)[0]
	if c.path != pathInitialize {
		t.Fatal(c.path)
	}
	want := `{"locale":"en","conversationId":"REF12345678","price":"120.50","paidPrice":"120.50","currency":"TRY","basketId":"REF12345678","paymentGroup":"PRODUCT","callbackUrl":"https://shop.example/pay/return?a=1","enabledInstallments":[1],` +
		`"buyer":{"id":"7","name":"Ada","surname":"Yılmaz","gsmNumber":"+905550000000","email":"ada@example.com","identityNumber":"11111111111","registrationAddress":"Atatürk Cd. 1 D:2","ip":"203.0.113.5","city":"İstanbul","country":"Turkey","zipCode":"34000"},` +
		`"shippingAddress":{"contactName":"Ada Yılmaz","city":"İstanbul","country":"Turkey","address":"Atatürk Cd. 1 D:2","zipCode":"34000"},` +
		`"billingAddress":{"contactName":"Ada Yılmaz","city":"İstanbul","country":"Turkey","address":"Atatürk Cd. 1 D:2","zipCode":"34000"},` +
		`"basketItems":[{"id":"sku1","name":"Mug","category1":"General","itemType":"PHYSICAL","price":"100.00"},{"id":"shipping","name":"Shipping","category1":"General","itemType":"VIRTUAL","price":"20.50"}]}`
	if string(c.body) != want {
		t.Fatalf("body\n got %s\nwant %s", c.body, want)
	}
	if c.header.Get("x-iyzi-rnd") != testRnd || c.header.Get("Content-Type") != "application/json" {
		t.Fatalf("headers %v", c.header)
	}
	if c.header.Get("Authorization") != authorization(testAPIKey, testSecretKey, testRnd, pathInitialize, c.body) {
		t.Fatal("authorization header")
	}
}

func TestStartInstallmentsDefault(t *testing.T) {
	p, calls := newTestProvider(t, func(string, map[string]any) any { return initOK("REF12345678", "t") })
	r := startRequest()
	r.MaxInstallment = 0
	if _, err := p.Start(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string((*calls)[0].body), `"enabledInstallments":[1,2,3,6,9,12]`) {
		t.Fatal(string((*calls)[0].body))
	}
}

func TestStartNoPageURL(t *testing.T) {
	p, _ := newTestProvider(t, func(string, map[string]any) any {
		m := initOK("REF12345678", "t")
		delete(m, "paymentPageUrl")
		m["checkoutFormContent"] = "<script>x</script>"
		return m
	})
	if _, err := p.Start(context.Background(), startRequest()); err == nil {
		t.Fatal("want error")
	}
}

func TestStartBadSignature(t *testing.T) {
	p, _ := newTestProvider(t, func(string, map[string]any) any {
		m := initOK("REF12345678", "t")
		m["signature"] = hmacHex("other", "x")
		return m
	})
	if _, err := p.Start(context.Background(), startRequest()); !errors.Is(err, pay.ErrInvalidSignature) {
		t.Fatal(err)
	}
}

func TestStartAPIError(t *testing.T) {
	p, _ := newTestProvider(t, func(string, map[string]any) any {
		return map[string]any{"status": "failure", "errorCode": "10051", "errorMessage": "Kart limiti yetersiz"}
	})
	_, err := p.Start(context.Background(), startRequest())
	var pe *pay.Error
	if !errors.As(err, &pe) || pe.Code != pay.CodeInsufficientFunds || pe.ProviderCode != "10051" {
		t.Fatal(err)
	}
}

func attempt() pay.Attempt {
	return pay.Attempt{Ref: "REF12345678", ProviderRef: "tok-1", Amount: 12050, Currency: "TRY"}
}

// retrieveJSON builds a signed retrieve response; amounts are raw JSON numbers.
func retrieveJSON(status, paymentStatus, paidPrice, price, fraud string) string {
	sigP := strings.TrimSuffix(strings.TrimRight(paidPrice, "0"), ".")
	sigQ := strings.TrimSuffix(strings.TrimRight(price, "0"), ".")
	sig := hmacHex(testSecretKey, strings.Join([]string{paymentStatus, "pid-9", "TRY", "REF12345678", "REF12345678", sigP, sigQ, "tok-1"}, ":"))
	return `{"status":"` + status + `","paymentStatus":"` + paymentStatus + `","paymentId":"pid-9","currency":"TRY","basketId":"REF12345678","conversationId":"REF12345678",` +
		`"paidPrice":` + paidPrice + `,"price":` + price + `,"token":"tok-1","installment":3,"fraudStatus":` + fraud +
		`,"errorCode":"10005","errorMessage":"Do not honour","signature":"` + sig + `"}`
}

func callback(token string) pay.CallbackInput {
	return pay.CallbackInput{Kind: pay.CallbackReturn, Method: "POST", Form: url.Values{"token": {token}}}
}

func TestCompleteSuccess(t *testing.T) {
	p, calls := newTestProvider(t, func(string, map[string]any) any { return retrieveJSON("success", "SUCCESS", "123.40", "120.50", "1") })
	res, err := p.Complete(context.Background(), callback("tok-1"), attempt())
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != pay.StatusSuccess || res.PaidAmount != 12340 || res.Currency != "TRY" || res.TxnID != "pid-9" || res.Installment != 3 {
		t.Fatalf("%+v", res)
	}
	if (*calls)[0].path != pathRetrieve || string((*calls)[0].body) != `{"locale":"tr","conversationId":"REF12345678","token":"tok-1"}` {
		t.Fatalf("%s %s", (*calls)[0].path, (*calls)[0].body)
	}
}

func TestCompleteTokenMismatch(t *testing.T) {
	p, calls := newTestProvider(t, func(string, map[string]any) any { return retrieveJSON("success", "SUCCESS", "120.5", "120.5", "1") })
	if _, err := p.Complete(context.Background(), callback("tok-2"), attempt()); err == nil {
		t.Fatal("want error")
	}
	if _, err := p.Complete(context.Background(), callback(""), attempt()); err == nil {
		t.Fatal("want error")
	}
	if len(*calls) != 0 {
		t.Fatal("no HTTP call expected")
	}
}

func TestCompleteConversationMismatch(t *testing.T) {
	p, _ := newTestProvider(t, func(string, map[string]any) any { return retrieveJSON("success", "SUCCESS", "120.5", "120.5", "1") })
	a := attempt()
	a.Ref = "OTHER1234567"
	if _, err := p.Complete(context.Background(), callback("tok-1"), a); err == nil {
		t.Fatal("want error")
	}
}

func TestCompleteTamperedSignature(t *testing.T) {
	p, _ := newTestProvider(t, func(string, map[string]any) any {
		return strings.Replace(retrieveJSON("success", "SUCCESS", "1.0", "120.5", "1"), `"paidPrice":1.0`, `"paidPrice":120.5`, 1)
	})
	if _, err := p.Complete(context.Background(), callback("tok-1"), attempt()); !errors.Is(err, pay.ErrInvalidSignature) {
		t.Fatal(err)
	}
}

func TestCompleteFailureAndPending(t *testing.T) {
	cases := []struct {
		body string
		want pay.Status
		code pay.Code
	}{
		{retrieveJSON("success", "FAILURE", "120.5", "120.5", "1"), pay.StatusFailure, pay.CodeCardDeclined},
		{retrieveJSON("success", "INIT_THREEDS", "120.5", "120.5", "1"), pay.StatusPending, ""},
		{retrieveJSON("success", "PENDING_CREDIT", "120.5", "120.5", "1"), pay.StatusPending, ""},
		{retrieveJSON("success", "SUCCESS", "120.5", "120.5", "0"), pay.StatusPending, ""},
		{retrieveJSON("success", "SUCCESS", "120.5", "120.5", "-1"), pay.StatusFailure, pay.CodeFraudSuspected},
		{`{"status":"failure","errorCode":"5","errorMessage":"invalid token"}`, pay.StatusFailure, pay.CodeUnknown},
	}
	for i, c := range cases {
		p, _ := newTestProvider(t, func(string, map[string]any) any { return c.body })
		res, err := p.Complete(context.Background(), callback("tok-1"), attempt())
		if err != nil {
			t.Fatalf("%d: %v", i, err)
		}
		if res.Status != c.want || res.Code != c.code || res.PaidAmount != 0 {
			t.Fatalf("%d: %+v", i, res)
		}
	}
}

func TestQuery(t *testing.T) {
	p, calls := newTestProvider(t, func(string, map[string]any) any { return retrieveJSON("success", "SUCCESS", "120.5", "120.5", "1") })
	res, err := p.Query(context.Background(), attempt())
	if err != nil || res.Status != pay.StatusSuccess || res.PaidAmount != 12050 {
		t.Fatal(res, err)
	}
	if (*calls)[0].path != pathRetrieve {
		t.Fatal((*calls)[0].path)
	}
}

func paidAttempt(created time.Time) pay.Attempt {
	a := attempt()
	a.TxnID, a.PaidAmount, a.CreatedAt = "pid-9", 12050, created
	return a
}

func TestRefundCancelSameDay(t *testing.T) {
	p, calls := newTestProvider(t, func(string, map[string]any) any {
		return map[string]any{"status": "success", "conversationId": "RF1", "paymentId": "pid-9", "price": 120.5, "currency": "TRY"}
	})
	now := time.Date(2026, 11, 27, 15, 0, 0, 0, time.UTC)
	p.now = func() time.Time { return now }
	res, err := p.Refund(context.Background(), pay.RefundRequest{Attempt: paidAttempt(now.Add(-2 * time.Hour)), Amount: 12050, Full: true, RefundRef: "RF1"})
	if err != nil || res.Status != pay.StatusSuccess || res.PaidAmount != 12050 {
		t.Fatal(res, err)
	}
	c := (*calls)[0]
	if len(*calls) != 1 || c.path != pathCancel || string(c.body) != `{"locale":"tr","conversationId":"RF1","paymentId":"pid-9"}` {
		t.Fatalf("%s %s", c.path, c.body)
	}
}

func TestRefundAmount(t *testing.T) {
	p, calls := newTestProvider(t, func(string, map[string]any) any {
		return map[string]any{"status": "success", "conversationId": "RF2", "paymentId": "pid-9", "price": 50, "currency": "TRY"}
	})
	now := time.Date(2026, 11, 27, 15, 0, 0, 0, time.UTC)
	p.now = func() time.Time { return now }
	// Partial on the same day, and full on a later day: both are refunds.
	for _, r := range []pay.RefundRequest{
		{Attempt: paidAttempt(now), Amount: 5000, RefundRef: "RF2"},
		{Attempt: paidAttempt(now.Add(-48 * time.Hour)), Amount: 5000, Full: true, RefundRef: "RF2"},
	} {
		*calls = nil
		res, err := p.Refund(context.Background(), r)
		if err != nil || res.PaidAmount != 5000 {
			t.Fatal(res, err)
		}
		c := (*calls)[0]
		if len(*calls) != 1 || c.path != pathRefund || string(c.body) != `{"locale":"tr","conversationId":"RF2","paymentId":"pid-9","price":"50.00","currency":"TRY"}` {
			t.Fatalf("%s %s", c.path, c.body)
		}
	}
}

func TestRefundCancelRefusedFallsBack(t *testing.T) {
	p, calls := newTestProvider(t, func(path string, _ map[string]any) any {
		if path == pathCancel {
			return map[string]any{"status": "failure", "errorCode": "10223"}
		}
		return map[string]any{"status": "success", "conversationId": "RF3", "paymentId": "pid-9", "price": 120.5}
	})
	res, err := p.Refund(context.Background(), pay.RefundRequest{Attempt: paidAttempt(time.Now()), Amount: 12050, Full: true, RefundRef: "RF3"})
	if err != nil || res.Status != pay.StatusSuccess || len(*calls) != 2 || (*calls)[1].path != pathRefund {
		t.Fatal(res, err)
	}
}

func TestRefundTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { time.Sleep(300 * time.Millisecond) }))
	defer srv.Close()
	pv, _ := Spec().New(pay.Config{
		Values: map[string]string{"api_key": testAPIKey, "secret_key": testSecretKey},
		HTTP:   &http.Client{Timeout: 50 * time.Millisecond}, BaseURL: srv.URL,
	})
	_, err := pv.Refund(context.Background(), pay.RefundRequest{Attempt: paidAttempt(time.Now().Add(-72 * time.Hour)), Amount: 100, RefundRef: "RF4"})
	if !errors.Is(err, pay.ErrUnknownOutcome) {
		t.Fatal(err)
	}
}

func TestRefundError(t *testing.T) {
	p, _ := newTestProvider(t, func(string, map[string]any) any {
		return map[string]any{"status": "failure", "errorCode": "10232", "errorMessage": "invalid amount"}
	})
	_, err := p.Refund(context.Background(), pay.RefundRequest{Attempt: paidAttempt(time.Now().Add(-72 * time.Hour)), Amount: 100, RefundRef: "RF5"})
	var pe *pay.Error
	if !errors.As(err, &pe) || pe.Code != pay.CodeInvalidRequest {
		t.Fatal(err)
	}
}

func TestNewRequiresKeys(t *testing.T) {
	if _, err := Spec().New(pay.Config{Values: map[string]string{"api_key": "x"}, HTTP: http.DefaultClient}); err == nil {
		t.Fatal("want error")
	}
}
