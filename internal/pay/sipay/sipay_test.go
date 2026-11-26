package sipay

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
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
	fakeSecret = "test-app-secret"
	fakeKey    = "test-merchant-key"
	fakeAppID  = "test-app-id"
)

// refDecrypt is Sipay's "Hash Validation" sample written independently of the package: replace "__"
// with "/", split iv:salt:data, key = first 32 hex chars of sha256(hex(sha1(secret)) + salt).
func refDecrypt(t *testing.T, secret, hashKey string) string {
	t.Helper()
	parts := strings.Split(strings.ReplaceAll(hashKey, "__", "/"), ":")
	if len(parts) != 3 {
		t.Fatalf("hash key %q", hashKey)
	}
	h1 := sha1.Sum([]byte(secret))
	h2 := sha256.Sum256([]byte(hex.EncodeToString(h1[:]) + parts[1]))
	block, err := aes.NewCipher([]byte(hex.EncodeToString(h2[:])[:32]))
	if err != nil {
		t.Fatal(err)
	}
	data, err := base64.StdEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	cipher.NewCBCDecrypter(block, []byte(parts[0])).CryptBlocks(data, data)
	return string(data[:len(data)-int(data[len(data)-1])])
}

// Vector from better-payment's fixtures (made with Node's crypto as Sipay's sample does).
func TestHashKeyVectors(t *testing.T) {
	const ref = "0123456789abcdef:a1b2:J3v+eSuMITtIyU__TQQ8KEbj4eQKOOT7Nkz0td1TvB8uxn6qe0Fs5Rn__ci6aMA6PYfziJi996pEIdoergEUcQfg=="
	const plain = "1|10.00|BP3D1791714553140|VP17917145548423635|TRY"
	if got := refDecrypt(t, fakeSecret, ref); got != plain {
		t.Fatalf("reference decrypt %q", got)
	}
	got, err := encryptHashKey(fakeSecret, "0123456789abcdef", "a1b2", plain)
	if err != nil || got != ref {
		t.Fatalf("encrypt %q %v", got, err)
	}
	a, _ := newHashKey(fakeSecret, "10.00", "INV1", fakeKey)
	b, _ := newHashKey(fakeSecret, "10.00", "INV1", fakeKey)
	if a == b || strings.Contains(a, "/") {
		t.Errorf("hash keys %q %q", a, b)
	}
	if got := refDecrypt(t, fakeSecret, a); got != "10.00|INV1|"+fakeKey {
		t.Errorf("decrypt %q", got)
	}
}

type call struct {
	path string
	auth string
	body map[string]any
}

type fake struct {
	mu      sync.Mutex
	calls   []call
	replies map[string][]string // path → replies in order (last repeats)
	delay   time.Duration
}

func (f *fake) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("content type %q", r.Header.Get("Content-Type"))
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.calls = append(f.calls, call{r.URL.Path, r.Header.Get("Authorization"), body})
		rs := f.replies[r.URL.Path]
		reply := `{}`
		if len(rs) > 0 {
			reply = rs[0]
			if len(rs) > 1 {
				f.replies[r.URL.Path] = rs[1:]
			}
		}
		f.mu.Unlock()
		if f.delay > 0 && r.URL.Path != pathToken {
			time.Sleep(f.delay)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(reply))
	}
}

func (f *fake) last(path string) call {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.calls) - 1; i >= 0; i-- {
		if f.calls[i].path == path {
			return f.calls[i]
		}
	}
	return call{}
}

const tokenOK = `{"status_code":100,"data":{"token":"tok1","is_3d":1}}`

func newTest(t *testing.T, replies map[string][]string) (pay.Provider, *fake, *httptest.Server) {
	t.Helper()
	f := &fake{replies: map[string][]string{pathToken: {tokenOK}}}
	for k, v := range replies {
		f.replies[k] = v
	}
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	p, err := Spec().New(pay.Config{
		Values: map[string]string{"merchant_key": fakeKey, "app_id": fakeAppID, "app_secret": fakeSecret},
		Test:   true, HTTP: srv.Client(), BaseURL: srv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	return p, f, srv
}

func startReq() pay.StartRequest {
	return pay.StartRequest{
		Ref: "ABCD1234efgh", OrderNumber: "1042", Amount: 34990, Currency: "TRY", MaxInstallment: 6,
		Description: "Order #1042",
		Customer:    pay.Customer{Email: "buyer@example.com", FirstName: "Ayşe", LastName: "Yılmaz", Phone: "05550000000", IP: "203.0.113.7"},
		Billing:     pay.Address{Line1: "Test Sk. 1", City: "İstanbul", Postcode: "34000", Country: "TR"},
		Items: []pay.Item{
			{Name: "Kitap <özel>", Category: "Books", Quantity: 2, UnitPrice: 15000},
			{Name: "Kargo", Quantity: 1, UnitPrice: 4990},
		},
		ReturnURL: "https://shop.example/ok", FailURL: "https://shop.example/fail", Locale: "en",
	}
}

func TestStart(t *testing.T) {
	p, f, srv := newTest(t, nil)
	f.replies[pathLink] = []string{`{"status":true,"status_code":100,"success_message":"Success !","link":"` + srv.URL + `/ccpayment/makepayment/VP1","order_id":"VP1"}`}
	res, err := p.Start(context.Background(), startReq())
	if err != nil {
		t.Fatal(err)
	}
	if res.Redirect != srv.URL+"/ccpayment/makepayment/VP1" || res.ProviderRef != "VP1" || res.Form != nil || res.Embed != nil {
		t.Errorf("result %+v", res)
	}
	tok := f.last(pathToken)
	if tok.body["app_id"] != fakeAppID || tok.body["app_secret"] != fakeSecret {
		t.Errorf("token body %v", tok.body)
	}
	c := f.last(pathLink)
	if c.auth != "Bearer tok1" {
		t.Errorf("auth %q", c.auth)
	}
	want := map[string]any{
		"merchant_key": fakeKey, "currency_code": "TRY", "name": "Ayşe", "surname": "Yılmaz",
		"bill_address1": "Test Sk. 1", "bill_city": "İstanbul", "bill_postcode": "34000", "bill_country": "TR",
		"bill_email": "buyer@example.com", "bill_phone": "05550000000", "max_installment": float64(6),
		"order_type": float64(0), "app_lang": "en",
	}
	for k, v := range want {
		if c.body[k] != v {
			t.Errorf("%s = %v, want %v", k, c.body[k], v)
		}
	}
	var inv map[string]any
	if err := json.Unmarshal([]byte(c.body["invoice"].(string)), &inv); err != nil {
		t.Fatal(err)
	}
	if inv["invoice_id"] != "ABCD1234efgh" || inv["total"] != "349.90" || inv["return_url"] != "https://shop.example/ok" ||
		inv["cancel_url"] != "https://shop.example/fail" || inv["invoice_description"] != "Order #1042" || inv["response_method"] != "POST" {
		t.Errorf("invoice %v", inv)
	}
	items, _ := json.Marshal(inv["items"])
	if string(items) != `[{"description":"Books","name":"Kitap \u003cözel\u003e","price":"150.00","quantity":2},{"description":"Kargo","name":"Kargo","price":"49.90","quantity":1}]` {
		t.Errorf("items %s", items)
	}
	raw, _ := json.Marshal(c.body)
	for _, bad := range []string{"cc_no", "cc_holder_name", "cvv", "expiry_month", "expiry_year", "card"} {
		if strings.Contains(string(raw), bad) {
			t.Errorf("card field %s in request", bad)
		}
	}
}

func TestStartRejected(t *testing.T) {
	p, f, _ := newTest(t, nil)
	f.replies[pathLink] = []string{`{"status_code":14,"status_description":"Merchant not found","link":null,"order_id":0}`}
	_, err := p.Start(context.Background(), startReq())
	var pe *pay.Error
	if !errors.As(err, &pe) || pe.Code != pay.CodeConfiguration || pe.ProviderCode != "14" {
		t.Fatalf("err %v", err)
	}
	// A link on a foreign host is refused.
	f.replies[pathLink] = []string{`{"status_code":100,"link":"https://evil.example/pay","order_id":"VP1"}`}
	if _, err := p.Start(context.Background(), startReq()); err == nil {
		t.Fatal("foreign link accepted")
	}
}

func TestTokenRenewedOnUnauthenticated(t *testing.T) {
	p, f, srv := newTest(t, nil)
	f.replies[pathToken] = []string{tokenOK, `{"status_code":100,"data":{"token":"tok2"}}`}
	f.replies[pathLink] = []string{`{"status_code":101,"status_description":"Unauthenticated."}`,
		`{"status_code":100,"link":"` + srv.URL + `/ccpayment/makepayment/VP1","order_id":"VP1"}`}
	if _, err := p.Start(context.Background(), startReq()); err != nil {
		t.Fatal(err)
	}
	if c := f.last(pathLink); c.auth != "Bearer tok2" {
		t.Errorf("auth %q", c.auth)
	}
}

func statusReply(status, code, invoice, amount string) string {
	return `{"status_code":` + code + `,"status_description":"x","transaction_status":"` + status + `","order_id":"VP9","invoice_id":"` + invoice +
		`","transaction_amount":` + amount + `,"installment":3,"original_bank_error_code":null}`
}

func attempt() pay.Attempt { return pay.Attempt{Ref: "ABCD1234efgh", Amount: 34990, Currency: "TRY"} }

func TestCompleteUsesStatusAPI(t *testing.T) {
	p, f, _ := newTest(t, map[string][]string{pathStatus: {statusReply("Completed", "100", "ABCD1234efgh", "349.9")}})
	// The landing claims a different amount and a failure; only checkstatus counts.
	in := pay.CallbackInput{Kind: pay.CallbackReturn, Form: url.Values{"invoice_id": {"ABCD1234efgh"}, "amount": {"1.00"}, "status_code": {"41"}}}
	if p.CallbackRef(in) != "ABCD1234efgh" {
		t.Fatal("callback ref")
	}
	res, err := p.Complete(context.Background(), in, attempt())
	if err != nil || res.Status != pay.StatusSuccess || res.PaidAmount != 34990 || res.TxnID != "VP9" || res.Installment != 3 {
		t.Fatalf("%+v %v", res, err)
	}
	c := f.last(pathStatus)
	if c.body["invoice_id"] != "ABCD1234efgh" || c.body["merchant_key"] != fakeKey || c.body["include_pending_status"] != true {
		t.Errorf("status body %v", c.body)
	}
	if got := refDecrypt(t, fakeSecret, c.body["hash_key"].(string)); got != "ABCD1234efgh|"+fakeKey {
		t.Errorf("status hash %q", got)
	}
}

func TestCompleteOutcomes(t *testing.T) {
	cases := []struct {
		name, reply string
		status      pay.Status
		code        pay.Code
		wantErr     bool
	}{
		{"pending", statusReply("Pending", "69", "ABCD1234efgh", "349.9"), pay.StatusPending, "", false},
		{"not found yet", statusReply("Failed", "32", "", "0"), pay.StatusPending, "", false},
		{"failed", `{"status_code":41,"transaction_status":"Failed","invoice_id":"ABCD1234efgh"}`, pay.StatusFailure, pay.CodeCardDeclined, false},
		{"bank decline", `{"status_code":41,"transaction_status":"Failed","invoice_id":"ABCD1234efgh","original_bank_error_code":"51","original_bank_error_description":"Yetersiz bakiye"}`, pay.StatusFailure, pay.CodeInsufficientFunds, false},
		{"refunded", statusReply("Refunded", "100", "ABCD1234efgh", "349.9"), pay.StatusCancelled, "", false},
		{"other invoice", statusReply("Completed", "100", "OTHER0000000", "349.9"), "", "", true},
		{"bad amount", statusReply("Completed", "100", "ABCD1234efgh", `"3,49,9"`), "", "", true},
		{"invalid hash", `{"status_code":68,"transaction_status":"","message":"Invalid hash key"}`, "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, _, _ := newTest(t, map[string][]string{pathStatus: {tc.reply}})
			res, err := p.Complete(context.Background(), pay.CallbackInput{Kind: pay.CallbackFail, Form: url.Values{"invoice_id": {"ABCD1234efgh"}}}, attempt())
			if tc.wantErr {
				if err == nil {
					t.Fatalf("no error: %+v", res)
				}
				return
			}
			if err != nil || res.Status != tc.status || res.Code != tc.code {
				t.Fatalf("%+v %v", res, err)
			}
		})
	}
}

func TestCompleteOtherInvoice(t *testing.T) {
	p, f, _ := newTest(t, nil)
	_, err := p.Complete(context.Background(), pay.CallbackInput{Kind: pay.CallbackReturn, Form: url.Values{"invoice_id": {"OTHER0000000"}}}, attempt())
	if err == nil {
		t.Fatal("accepted callback for another invoice")
	}
	if f.last(pathStatus).path != "" {
		t.Error("status called")
	}
}

func TestCompleteTimeoutIsPending(t *testing.T) {
	p, f, srv := newTest(t, map[string][]string{pathStatus: {statusReply("Completed", "100", "ABCD1234efgh", "349.9")}})
	f.delay = 200 * time.Millisecond
	p.(*provider).cfg.HTTP = &http.Client{Timeout: 50 * time.Millisecond, Transport: srv.Client().Transport}
	res, err := p.Complete(context.Background(), pay.CallbackInput{Kind: pay.CallbackReturn}, attempt())
	if err != nil || res.Status != pay.StatusPending {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestQuery(t *testing.T) {
	p, _, _ := newTest(t, map[string][]string{pathStatus: {statusReply("Partial Refund", "100", "ABCD1234efgh", "5")}})
	res, err := p.Query(context.Background(), attempt())
	if err != nil || res.Status != pay.StatusSuccess || res.PaidAmount != 500 {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestRefund(t *testing.T) {
	p, f, _ := newTest(t, map[string][]string{pathRefund: {`{"status_code":100,"status_description":"Refund completed","ref_no":"R123"}`}})
	a := attempt()
	res, err := p.Refund(context.Background(), pay.RefundRequest{Attempt: a, Amount: 10050, RefundRef: "RF1"})
	if err != nil || res.Status != pay.StatusSuccess || res.PaidAmount != 10050 || res.TxnID != "R123" {
		t.Fatalf("%+v %v", res, err)
	}
	c := f.last(pathRefund)
	if c.body["invoice_id"] != a.Ref || c.body["amount"] != "100.50" || c.body["merchant_key"] != fakeKey {
		t.Errorf("refund body %v", c.body)
	}
	if got := refDecrypt(t, fakeSecret, c.body["hash_key"].(string)); got != "100.50|ABCD1234efgh|"+fakeKey {
		t.Errorf("refund hash %q", got)
	}
	if _, ok := c.body["app_secret"]; ok {
		t.Error("app_secret sent with refund")
	}
}

func TestRefundRejected(t *testing.T) {
	p, _, _ := newTest(t, map[string][]string{pathRefund: {`{"status_code":110,"status_description":"Partial refund not allowed"}`}})
	_, err := p.Refund(context.Background(), pay.RefundRequest{Attempt: attempt(), Amount: 100})
	var pe *pay.Error
	if !errors.As(err, &pe) || pe.Code != pay.CodeInvalidRequest {
		t.Fatalf("err %v", err)
	}
}

func TestRefundUnknownOutcome(t *testing.T) {
	p, f, srv := newTest(t, map[string][]string{pathRefund: {`<html>gateway timeout</html>`}})
	if _, err := p.Refund(context.Background(), pay.RefundRequest{Attempt: attempt(), Amount: 100}); !errors.Is(err, pay.ErrUnknownOutcome) {
		t.Fatalf("unreadable: %v", err)
	}
	f.delay = 200 * time.Millisecond
	p.(*provider).cfg.HTTP = &http.Client{Timeout: 50 * time.Millisecond, Transport: srv.Client().Transport}
	if _, err := p.Refund(context.Background(), pay.RefundRequest{Attempt: attempt(), Amount: 100}); !errors.Is(err, pay.ErrUnknownOutcome) {
		t.Fatalf("timeout: %v", err)
	}
}

func TestSpecs(t *testing.T) {
	s := Specs()
	if len(s) != 1 || s[0].ID != "sipay" || s[0].Region != pay.RegionTR || s[0].Kind != pay.KindPSP {
		t.Fatalf("%+v", s)
	}
	if _, err := s[0].New(pay.Config{Values: map[string]string{"merchant_key": "x"}}); err == nil {
		t.Error("incomplete settings accepted")
	}
}
