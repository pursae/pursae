package paypal

import (
	"context"
	"encoding/json"
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

// fakePayPal plays PayPal's REST API. Handlers are keyed by "METHOD path".
type fakePayPal struct {
	t        *testing.T
	mu       sync.Mutex
	tokens   int
	tokenTTL int
	calls    []recorded
	handlers map[string]func(w http.ResponseWriter, r *http.Request, body []byte)
	// reject401 makes the next n API calls answer 401.
	reject401 int
}

type recorded struct {
	Method, Path string
	Header       http.Header
	Body         []byte
}

func newFake(t *testing.T) (*fakePayPal, *httptest.Server) {
	f := &fakePayPal{t: t, tokenTTL: 32400, handlers: map[string]func(http.ResponseWriter, *http.Request, []byte){}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		if r.URL.Path == "/v1/oauth2/token" {
			f.tokens++
			n := f.tokens
			f.mu.Unlock()
			user, pass, ok := r.BasicAuth()
			if !ok || user != "test-client" || pass != "test-secret" || string(body) != "grant_type=client_credentials" {
				w.WriteHeader(401)
				io.WriteString(w, `{"error":"invalid_client"}`)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"access_token": "tok" + string(rune('0'+n)), "expires_in": f.tokenTTL})
			return
		}
		f.calls = append(f.calls, recorded{r.Method, r.URL.Path, r.Header.Clone(), body})
		if f.reject401 > 0 {
			f.reject401--
			f.mu.Unlock()
			w.WriteHeader(401)
			return
		}
		h := f.handlers[r.Method+" "+r.URL.Path]
		f.mu.Unlock()
		if h == nil {
			t.Errorf("unexpected call %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
			return
		}
		h(w, r, body)
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakePayPal) on(key string, status int, body string) {
	f.handlers[key] = func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, body)
	}
}

func (f *fakePayPal) called(key string) int {
	n := 0
	for _, c := range f.calls {
		if c.Method+" "+c.Path == key {
			n++
		}
	}
	return n
}

func newP(t *testing.T, srv *httptest.Server, webhook string) *provider {
	pr, err := Spec().New(pay.Config{
		Values:  map[string]string{"client_id": "test-client", "client_secret": "test-secret", "webhook_id": webhook},
		HTTP:    &http.Client{Timeout: 2 * time.Second},
		BaseURL: srv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	return pr.(*provider)
}

const ref = "REF12345abc"
const orderID = "5O190127TN364715T"

func startReq() pay.StartRequest {
	return pay.StartRequest{
		Ref: ref, OrderNumber: "1042", Amount: 3499, Currency: "USD", Description: "Order #1042 - Test store",
		Items:     []pay.Item{{ID: "sku1", Name: "Mug", Quantity: 2, UnitPrice: 1250}, {ID: "shipping", Name: "Shipping", Quantity: 1, UnitPrice: 999, Virtual: true}},
		Shipping:  pay.Address{Name: "Ada Test", Line1: "1 Main St", City: "Springfield", State: "IL", Postcode: "62701", Country: "US"},
		ReturnURL: "https://store.test/pay/return", FailURL: "https://store.test/pay/fail", Locale: "en",
	}
}

const createdOrder = `{"id":"` + orderID + `","status":"PAYER_ACTION_REQUIRED","links":[
 {"href":"https://api-m.paypal.com/v2/checkout/orders/` + orderID + `","rel":"self"},
 {"href":"https://www.paypal.com/checkoutnow?token=` + orderID + `","rel":"payer-action"}]}`

func TestStart(t *testing.T) {
	f, srv := newFake(t)
	f.on("POST /v2/checkout/orders", 200, createdOrder)
	p := newP(t, srv, "")
	res, err := p.Start(context.Background(), startReq())
	if err != nil {
		t.Fatal(err)
	}
	if res.Redirect != "https://www.paypal.com/checkoutnow?token="+orderID || res.ProviderRef != orderID {
		t.Fatalf("result %+v", res)
	}
	c := f.calls[0]
	if c.Header.Get("Authorization") != "Bearer tok1" || c.Header.Get("PayPal-Request-Id") != ref {
		t.Fatalf("headers %v", c.Header)
	}
	want := `{"intent":"CAPTURE","purchase_units":[{"reference_id":"REF12345abc","custom_id":"REF12345abc","invoice_id":"REF12345abc","description":"Order #1042 - Test store","amount":{"currency_code":"USD","value":"34.99","breakdown":{"item_total":{"currency_code":"USD","value":"34.99"}}},"items":[{"name":"Mug","quantity":"2","unit_amount":{"currency_code":"USD","value":"12.50"},"sku":"sku1","category":"PHYSICAL_GOODS"},{"name":"Shipping","quantity":"1","unit_amount":{"currency_code":"USD","value":"9.99"},"sku":"shipping","category":"DIGITAL_GOODS"}],"shipping":{"name":{"full_name":"Ada Test"},"address":{"address_line_1":"1 Main St","admin_area_2":"Springfield","admin_area_1":"IL","postal_code":"62701","country_code":"US"}}}],"payment_source":{"paypal":{"experience_context":{"return_url":"https://store.test/pay/return","cancel_url":"https://store.test/pay/fail","user_action":"PAY_NOW","shipping_preference":"SET_PROVIDED_ADDRESS","locale":"en-US"}}}}`
	if string(c.Body) != want {
		t.Fatalf("body\n got %s\nwant %s", c.Body, want)
	}
}

func TestStartZeroDecimalNoBreakdownApproveLink(t *testing.T) {
	f, srv := newFake(t)
	f.on("POST /v2/checkout/orders", 201, `{"id":"`+orderID+`","links":[{"href":"https://www.sandbox.paypal.com/checkoutnow?token=x","rel":"approve"}]}`)
	p := newP(t, srv, "")
	r := startReq()
	r.Currency, r.Amount = "JPY", 5000
	r.Items = []pay.Item{{Name: "Download", Quantity: 1, UnitPrice: 4999, Virtual: true}} // does not sum
	res, err := p.Start(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Redirect, "sandbox.paypal.com") {
		t.Fatal(res.Redirect)
	}
	body := string(f.calls[0].Body)
	if !strings.Contains(body, `"amount":{"currency_code":"JPY","value":"5000"}`) || strings.Contains(body, "items") || strings.Contains(body, "breakdown") {
		t.Fatal(body)
	}
	if !strings.Contains(body, `"shipping_preference":"NO_SHIPPING"`) {
		t.Fatal(body)
	}
}

func TestStartRefusesForeignRedirect(t *testing.T) {
	for _, href := range []string{"https://evil.example/checkout", "http://www.paypal.com/x", "https://paypal.com.evil.example/"} {
		f, srv := newFake(t)
		f.on("POST /v2/checkout/orders", 200, `{"id":"`+orderID+`","links":[{"href":"`+href+`","rel":"payer-action"}]}`)
		if _, err := newP(t, srv, "").Start(context.Background(), startReq()); err == nil {
			t.Fatalf("%s accepted", href)
		}
	}
}

func TestStartError(t *testing.T) {
	f, srv := newFake(t)
	f.on("POST /v2/checkout/orders", 422, `{"name":"UNPROCESSABLE_ENTITY","details":[{"issue":"CURRENCY_NOT_SUPPORTED"}]}`)
	_, err := newP(t, srv, "").Start(context.Background(), startReq())
	var pe *pay.Error
	if !errors.As(err, &pe) || pe.Code != pay.CodeConfiguration || pe.ProviderCode != "CURRENCY_NOT_SUPPORTED" {
		t.Fatal(err)
	}
}

func TestTokenCacheRefreshExpiry(t *testing.T) {
	f, srv := newFake(t)
	f.on("GET /v2/checkout/orders/"+orderID, 200, completedOrder(ref, "COMPLETED"))
	p := newP(t, srv, "")
	a := attempt()
	for i := 0; i < 2; i++ {
		if _, err := p.Query(context.Background(), a); err != nil {
			t.Fatal(err)
		}
	}
	if f.tokens != 1 {
		t.Fatalf("tokens %d", f.tokens)
	}
	f.reject401 = 1
	if _, err := p.Query(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if f.tokens != 2 || f.calls[len(f.calls)-1].Header.Get("Authorization") != "Bearer tok2" {
		t.Fatalf("tokens %d", f.tokens)
	}
	p.now = func() time.Time { return time.Now().Add(9 * time.Hour) }
	if _, err := p.Query(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if f.tokens != 3 {
		t.Fatalf("expired token reused, tokens %d", f.tokens)
	}
}

func TestBadCredentials(t *testing.T) {
	_, srv := newFake(t)
	pr, _ := Spec().New(pay.Config{Values: map[string]string{"client_id": "x", "client_secret": "y"}, HTTP: http.DefaultClient, BaseURL: srv.URL})
	_, err := pr.Query(context.Background(), attempt())
	var pe *pay.Error
	if !errors.As(err, &pe) || pe.Code != pay.CodeConfiguration {
		t.Fatal(err)
	}
}

func attempt() pay.Attempt {
	return pay.Attempt{Ref: ref, ProviderRef: orderID, Amount: 3499, Currency: "USD"}
}

func completedOrder(custom, capStatus string) string {
	return `{"id":"` + orderID + `","status":"COMPLETED","purchase_units":[{"reference_id":"` + custom + `","custom_id":"` + custom + `",
	 "payments":{"captures":[{"id":"3C679366HH908993F","status":"` + capStatus + `","amount":{"currency_code":"USD","value":"34.99"},"custom_id":"` + custom + `","status_details":{"reason":"PENDING_REVIEW"}}]}}]}`
}

func ret(token string) pay.CallbackInput {
	return pay.CallbackInput{Kind: pay.CallbackReturn, Method: "GET", Query: url.Values{"token": {token}, "PayerID": {"P1"}}}
}

func TestCompleteReturnSuccess(t *testing.T) {
	f, srv := newFake(t)
	f.on("POST /v2/checkout/orders/"+orderID+"/capture", 201, completedOrder(ref, "COMPLETED"))
	r, err := newP(t, srv, "").Complete(context.Background(), ret(orderID), attempt())
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != pay.StatusSuccess || r.PaidAmount != 3499 || r.Currency != "USD" || r.TxnID != "3C679366HH908993F" {
		t.Fatalf("%+v", r)
	}
	if f.calls[0].Header.Get("PayPal-Request-Id") != "cap"+ref {
		t.Fatal(f.calls[0].Header)
	}
}

func TestCompleteReturnTokenMismatch(t *testing.T) {
	f, srv := newFake(t)
	for _, tok := range []string{"OTHER", ""} {
		if _, err := newP(t, srv, "").Complete(context.Background(), ret(tok), attempt()); err == nil {
			t.Fatal("accepted")
		}
	}
	if len(f.calls) != 0 {
		t.Fatal("capture called")
	}
}

func TestCompleteReturnWrongRef(t *testing.T) {
	f, srv := newFake(t)
	f.on("POST /v2/checkout/orders/"+orderID+"/capture", 201, completedOrder("OTHERREF1", "COMPLETED"))
	if _, err := newP(t, srv, "").Complete(context.Background(), ret(orderID), attempt()); err == nil {
		t.Fatal("accepted")
	}
}

func TestCompleteAlreadyCaptured(t *testing.T) {
	f, srv := newFake(t)
	f.on("POST /v2/checkout/orders/"+orderID+"/capture", 422, `{"name":"UNPROCESSABLE_ENTITY","details":[{"issue":"ORDER_ALREADY_CAPTURED"}]}`)
	f.on("GET /v2/checkout/orders/"+orderID, 200, completedOrder(ref, "COMPLETED"))
	r, err := newP(t, srv, "").Complete(context.Background(), ret(orderID), attempt())
	if err != nil || r.Status != pay.StatusSuccess || f.called("GET /v2/checkout/orders/"+orderID) != 1 {
		t.Fatal(r, err)
	}
}

func TestCompletePending(t *testing.T) {
	f, srv := newFake(t)
	f.on("POST /v2/checkout/orders/"+orderID+"/capture", 201, completedOrder(ref, "PENDING"))
	r, err := newP(t, srv, "").Complete(context.Background(), ret(orderID), attempt())
	if err != nil || r.Status != pay.StatusPending || r.ProviderMessage != "PENDING_REVIEW" {
		t.Fatal(r, err)
	}
}

func TestCompleteDeclined(t *testing.T) {
	f, srv := newFake(t)
	f.on("POST /v2/checkout/orders/"+orderID+"/capture", 422, `{"name":"UNPROCESSABLE_ENTITY","details":[{"issue":"INSTRUMENT_DECLINED","description":"declined"}],"debug_id":"d1"}`)
	r, err := newP(t, srv, "").Complete(context.Background(), ret(orderID), attempt())
	if err != nil || r.Status != pay.StatusFailure || r.Code != pay.CodeCardDeclined || r.ProviderCode != "INSTRUMENT_DECLINED" {
		t.Fatal(r, err)
	}
	f.on("POST /v2/checkout/orders/"+orderID+"/capture", 201, completedOrder(ref, "DECLINED"))
	r, err = newP(t, srv, "").Complete(context.Background(), ret(orderID), attempt())
	if err != nil || r.Status != pay.StatusFailure || r.Code != pay.CodeCardDeclined {
		t.Fatal(r, err)
	}
}

func TestCompleteFail(t *testing.T) {
	f, srv := newFake(t)
	r, err := newP(t, srv, "").Complete(context.Background(), pay.CallbackInput{Kind: pay.CallbackFail}, attempt())
	if err != nil || r.Status != pay.StatusFailure || r.Code != pay.CodeCancelledByUser || len(f.calls) != 0 {
		t.Fatal(r, err)
	}
}

const captureEvent = `{"id":"WH-1","event_type":"PAYMENT.CAPTURE.COMPLETED",  "resource":{"id":"3C679366HH908993F","status":"COMPLETED","amount":{"currency_code":"USD","value":"34.99"},"custom_id":"` + ref + `"}}`

func notifyIn(body string) pay.CallbackInput {
	h := http.Header{}
	h.Set("Paypal-Auth-Algo", "SHA256withRSA")
	h.Set("Paypal-Cert-Url", "https://api-m.paypal.com/v1/notifications/certs/CERT-1")
	h.Set("Paypal-Transmission-Id", "tid")
	h.Set("Paypal-Transmission-Sig", "sig")
	h.Set("Paypal-Transmission-Time", "2026-11-27T10:00:00Z")
	return pay.CallbackInput{Kind: pay.CallbackNotify, Method: "POST", Header: h, Body: []byte(body)}
}

func TestNotify(t *testing.T) {
	f, srv := newFake(t)
	f.on("POST /v1/notifications/verify-webhook-signature", 200, `{"verification_status":"SUCCESS"}`)
	p := newP(t, srv, "WH-ID-1")
	in := notifyIn(captureEvent)
	if got := p.CallbackRef(in); got != ref {
		t.Fatal(got)
	}
	r, err := p.Complete(context.Background(), in, attempt())
	if err != nil || r.Status != pay.StatusSuccess || r.PaidAmount != 3499 || r.Ack == nil || r.Ack.Status != 200 || r.Ack.Body != "" {
		t.Fatal(r, err)
	}
	want := `{"auth_algo":"SHA256withRSA","cert_url":"https://api-m.paypal.com/v1/notifications/certs/CERT-1","transmission_id":"tid","transmission_sig":"sig","transmission_time":"2026-11-27T10:00:00Z","webhook_id":"WH-ID-1","webhook_event":` + captureEvent + `}`
	if string(f.calls[0].Body) != want {
		t.Fatalf("verify body\n got %s\nwant %s", f.calls[0].Body, want)
	}
}

func TestNotifyFailureAndNoWebhook(t *testing.T) {
	f, srv := newFake(t)
	f.on("POST /v1/notifications/verify-webhook-signature", 200, `{"verification_status":"FAILURE"}`)
	if _, err := newP(t, srv, "WH-ID-1").Complete(context.Background(), notifyIn(captureEvent), attempt()); !errors.Is(err, pay.ErrInvalidSignature) {
		t.Fatal(err)
	}
	n := len(f.calls)
	if _, err := newP(t, srv, "").Complete(context.Background(), notifyIn(captureEvent), attempt()); err == nil || len(f.calls) != n {
		t.Fatal("unverified notification processed", err)
	}
}

func TestNotifyOrderApprovedCaptures(t *testing.T) {
	f, srv := newFake(t)
	f.on("POST /v1/notifications/verify-webhook-signature", 200, `{"verification_status":"SUCCESS"}`)
	f.on("POST /v2/checkout/orders/"+orderID+"/capture", 201, completedOrder(ref, "COMPLETED"))
	ev := `{"event_type":"CHECKOUT.ORDER.APPROVED","resource":{"id":"` + orderID + `","status":"APPROVED","purchase_units":[{"reference_id":"` + ref + `","custom_id":"` + ref + `"}]}}`
	p := newP(t, srv, "WH-ID-1")
	if p.CallbackRef(notifyIn(ev)) != ref {
		t.Fatal("ref")
	}
	r, err := p.Complete(context.Background(), notifyIn(ev), attempt())
	if err != nil || r.Status != pay.StatusSuccess || r.Ack == nil {
		t.Fatal(r, err)
	}
}

func TestQuery(t *testing.T) {
	cases := []struct {
		body string
		want pay.Status
	}{
		{completedOrder(ref, "COMPLETED"), pay.StatusSuccess},
		{completedOrder(ref, "REFUNDED"), pay.StatusCancelled},
		{`{"id":"` + orderID + `","status":"APPROVED","purchase_units":[{"custom_id":"` + ref + `"}]}`, pay.StatusPending},
		{`{"id":"` + orderID + `","status":"PAYER_ACTION_REQUIRED","purchase_units":[{"custom_id":"` + ref + `"}]}`, pay.StatusPending},
		{`{"id":"` + orderID + `","status":"VOIDED","purchase_units":[{"custom_id":"` + ref + `"}]}`, pay.StatusCancelled},
	}
	for _, c := range cases {
		f, srv := newFake(t)
		f.on("GET /v2/checkout/orders/"+orderID, 200, c.body)
		r, err := newP(t, srv, "").Query(context.Background(), attempt())
		if err != nil || r.Status != c.want {
			t.Fatalf("%s: %+v %v", c.want, r, err)
		}
	}
}

func refundReq(full bool) pay.RefundRequest {
	a := attempt()
	a.TxnID, a.PaidAmount = "3C679366HH908993F", 3499
	return pay.RefundRequest{Attempt: a, Amount: 1000, Full: full, RefundRef: "RF1", Reason: "Damaged"}
}

func TestRefund(t *testing.T) {
	path := "POST /v2/payments/captures/3C679366HH908993F/refund"
	f, srv := newFake(t)
	f.on(path, 201, `{"id":"1JU08902781691411","status":"COMPLETED","amount":{"currency_code":"USD","value":"10.00"}}`)
	p := newP(t, srv, "")
	r, err := p.Refund(context.Background(), refundReq(false))
	if err != nil || r.Status != pay.StatusSuccess || r.PaidAmount != 1000 {
		t.Fatal(r, err)
	}
	if string(f.calls[0].Body) != `{"amount":{"currency_code":"USD","value":"10.00"},"note_to_payer":"Damaged"}` || f.calls[0].Header.Get("PayPal-Request-Id") != "RF1" {
		t.Fatal(string(f.calls[0].Body))
	}
	if _, err := p.Refund(context.Background(), refundReq(true)); err != nil {
		t.Fatal(err)
	}
	if string(f.calls[1].Body) != `{"note_to_payer":"Damaged"}` {
		t.Fatal(string(f.calls[1].Body))
	}
	f.on(path, 201, `{"id":"1J","status":"PENDING","status_details":{"reason":"ECHECK"}}`)
	if r, err := p.Refund(context.Background(), refundReq(false)); err != nil || r.Status != pay.StatusPending {
		t.Fatal(r, err)
	}
}

func TestRefundTimeout(t *testing.T) {
	f, srv := newFake(t)
	f.handlers["POST /v2/payments/captures/3C679366HH908993F/refund"] = func(w http.ResponseWriter, r *http.Request, _ []byte) {
		time.Sleep(300 * time.Millisecond)
	}
	p := newP(t, srv, "")
	p.cfg.HTTP = &http.Client{Timeout: 100 * time.Millisecond}
	p.token, p.expires = "tok0", time.Now().Add(time.Hour)
	if _, err := p.Refund(context.Background(), refundReq(false)); !errors.Is(err, pay.ErrUnknownOutcome) {
		t.Fatal(err)
	}
}

// onSeq answers successive calls to key with the given status/body pairs, the last one repeating.
func (f *fakePayPal) onSeq(key string, replies ...[2]any) {
	n := 0
	f.handlers[key] = func(w http.ResponseWriter, r *http.Request, _ []byte) {
		rep := replies[min(n, len(replies)-1)]
		n++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(rep[0].(int))
		io.WriteString(w, rep[1].(string))
	}
}

func TestStartRetriesWithoutRejectedAddress(t *testing.T) {
	for _, rejected := range []string{
		`{"name":"UNPROCESSABLE_ENTITY","details":[{"issue":"SHIPPING_ADDRESS_INVALID"}]}`,
		`{"name":"UNPROCESSABLE_ENTITY","details":[{"issue":"POSTAL_CODE_REQUIRED","field":"/purchase_units/@reference_id=='REF12345abc'/shipping/address/postal_code"}]}`,
		`{"name":"UNPROCESSABLE_ENTITY","details":[{"issue":"CITY_REQUIRED"}]}`,
		`{"name":"INVALID_REQUEST","details":[{"issue":"INVALID_STRING_LENGTH","field":"/purchase_units/0/shipping/address/admin_area_1"}]}`,
	} {
		f, srv := newFake(t)
		status := 422
		if strings.Contains(rejected, "INVALID_REQUEST") {
			status = 400
		}
		f.onSeq("POST /v2/checkout/orders", [2]any{status, rejected}, [2]any{201, createdOrder})
		res, err := newP(t, srv, "").Start(context.Background(), startReq())
		if err != nil {
			t.Fatalf("%s: %v", rejected, err)
		}
		if res.Redirect != "https://www.paypal.com/checkoutnow?token="+orderID || f.called("POST /v2/checkout/orders") != 2 {
			t.Fatalf("%s: %+v", rejected, res)
		}
		first, retry := f.calls[0], f.calls[1]
		if first.Header.Get("PayPal-Request-Id") != ref || retry.Header.Get("PayPal-Request-Id") != ref+"-noaddr" {
			t.Fatal(first.Header.Get("PayPal-Request-Id"), retry.Header.Get("PayPal-Request-Id"))
		}
		body := string(retry.Body)
		if strings.Contains(body, `"shipping":`) || !strings.Contains(body, `"shipping_preference":"GET_FROM_FILE"`) ||
			!strings.Contains(body, `"reference_id":"REF12345abc","custom_id":"REF12345abc","invoice_id":"REF12345abc"`) {
			t.Fatal(body)
		}
	}
}

func TestStartNoRetryForOtherIssues(t *testing.T) {
	f, srv := newFake(t)
	f.onSeq("POST /v2/checkout/orders",
		[2]any{422, `{"name":"UNPROCESSABLE_ENTITY","details":[{"issue":"PAYEE_ACCOUNT_RESTRICTED"}]}`},
		[2]any{201, createdOrder})
	_, err := newP(t, srv, "").Start(context.Background(), startReq())
	var pe *pay.Error
	if !errors.As(err, &pe) || pe.ProviderCode != "PAYEE_ACCOUNT_RESTRICTED" || f.called("POST /v2/checkout/orders") != 1 {
		t.Fatal(err, f.called("POST /v2/checkout/orders"))
	}
}

func TestStartNoRetryWithoutProvidedAddress(t *testing.T) {
	f, srv := newFake(t)
	f.onSeq("POST /v2/checkout/orders", [2]any{422, `{"name":"UNPROCESSABLE_ENTITY","details":[{"issue":"SHIPPING_ADDRESS_INVALID"}]}`})
	r := startReq()
	r.Shipping = pay.Address{}
	if _, err := newP(t, srv, "").Start(context.Background(), r); err == nil || f.called("POST /v2/checkout/orders") != 1 {
		t.Fatal(err)
	}
}

func TestStartRetryFailureReturnsError(t *testing.T) {
	f, srv := newFake(t)
	f.onSeq("POST /v2/checkout/orders",
		[2]any{422, `{"name":"UNPROCESSABLE_ENTITY","details":[{"issue":"SHIPPING_ADDRESS_INVALID"}]}`},
		[2]any{422, `{"name":"UNPROCESSABLE_ENTITY","details":[{"issue":"CURRENCY_NOT_SUPPORTED"}]}`})
	_, err := newP(t, srv, "").Start(context.Background(), startReq())
	var pe *pay.Error
	if !errors.As(err, &pe) || pe.ProviderCode != "CURRENCY_NOT_SUPPORTED" || f.called("POST /v2/checkout/orders") != 2 {
		t.Fatal(err)
	}
}

func TestCaptureAccepted(t *testing.T) {
	f, srv := newFake(t)
	f.on("POST /v2/checkout/orders/"+orderID+"/capture", 202, `{"id":"`+orderID+`","status":"APPROVED"}`)
	r, err := newP(t, srv, "").Complete(context.Background(), ret(orderID), attempt())
	if err != nil || r.Status != pay.StatusPending {
		t.Fatal(r, err)
	}
}

func TestNotifyCapturePendingAndDeclined(t *testing.T) {
	f, srv := newFake(t)
	f.on("POST /v1/notifications/verify-webhook-signature", 200, `{"verification_status":"SUCCESS"}`)
	p := newP(t, srv, "WH-ID-1")
	pending := `{"event_type":"PAYMENT.CAPTURE.PENDING","resource":{"id":"C1","status":"PENDING","status_details":{"reason":"PENDING_REVIEW"},"custom_id":"` + ref + `"}}`
	r, err := p.Complete(context.Background(), notifyIn(pending), attempt())
	if err != nil || r.Status != pay.StatusPending || r.ProviderMessage != "PENDING_REVIEW" || r.Ack == nil {
		t.Fatal(r, err)
	}
	declined := `{"event_type":"PAYMENT.CAPTURE.DECLINED","resource":{"id":"C1","status":"DECLINED","custom_id":"` + ref + `"}}`
	r, err = p.Complete(context.Background(), notifyIn(declined), attempt())
	if err != nil || r.Status != pay.StatusFailure || r.Code != pay.CodeCardDeclined {
		t.Fatal(r, err)
	}
	other := `{"event_type":"PAYMENT.CAPTURE.PENDING","resource":{"id":"C1","status":"PENDING","custom_id":"OTHERREF1"}}`
	if _, err := p.Complete(context.Background(), notifyIn(other), attempt()); err == nil {
		t.Fatal("foreign capture accepted")
	}
}
