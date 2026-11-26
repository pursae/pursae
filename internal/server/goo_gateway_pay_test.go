package server

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"wpgo/internal/pay"
)

// gooFakePay is a provider for the bridge's tests. A callback is "signed" when it carries sig=ok; its
// status, amount and currency come from the callback itself (status, amount, cur).
type gooFakePay struct {
	mu        sync.Mutex
	mode      string // Start: redirect, form, embed, badhost
	lastStart pay.StartRequest
	query     *pay.Result
	queryErr  error
	refundErr error
	refunds   []pay.RefundRequest
	inflight  atomic.Int32
	maxIn     atomic.Int32
}

func (f *gooFakePay) Start(ctx context.Context, r pay.StartRequest) (*pay.StartResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastStart = r
	switch f.mode {
	case "form":
		return &pay.StartResult{ProviderRef: "tok_" + r.Ref, Form: &pay.Form{Action: "https://pay.fake.test/pay",
			Fields: [][2]string{{"merchant_oid", r.Ref}, {"note", `"><script>alert(1)</script>`}}}}, nil
	case "embed":
		return &pay.StartResult{Embed: &pay.Embed{FrameURL: "https://frame.fake.test/f?t=1", Scripts: []string{"https://js.fake.test/sdk.js"},
			Init: `iFrameResize({}, "#goo-pay-frame");`}}, nil
	case "badhost":
		return &pay.StartResult{Form: &pay.Form{Action: "https://evil.example/pay"}}, nil
	case "reject":
		return nil, pay.Errorf(pay.CodeConfiguration, "E1", "merchant disabled")
	}
	return &pay.StartResult{Redirect: "https://pay.fake.test/r/" + r.Ref}, nil
}

func gooFakeVal(in pay.CallbackInput, k string) string {
	if v := in.Form.Get(k); v != "" {
		return v
	}
	return in.Query.Get(k)
}

func (f *gooFakePay) CallbackRef(in pay.CallbackInput) string { return gooFakeVal(in, "ref") }

func (f *gooFakePay) Complete(ctx context.Context, in pay.CallbackInput, a pay.Attempt) (*pay.Result, error) {
	n := f.inflight.Add(1)
	defer f.inflight.Add(-1)
	for {
		m := f.maxIn.Load()
		if n <= m || f.maxIn.CompareAndSwap(m, n) {
			break
		}
	}
	time.Sleep(5 * time.Millisecond)
	if gooFakeVal(in, "sig") != "ok" {
		return nil, pay.ErrInvalidSignature
	}
	if r := gooFakeVal(in, "ref"); r != "" && r != a.Ref {
		return nil, errors.New("not this attempt")
	}
	res := &pay.Result{Status: pay.Status(gooFakeVal(in, "status")), TxnID: "txn_" + a.Ref, Currency: gooFakeVal(in, "cur"),
		Ack: &pay.Ack{Body: "FAKE-OK"}}
	res.PaidAmount, _ = strconv.ParseInt(gooFakeVal(in, "amount"), 10, 64)
	res.Installment, _ = strconv.Atoi(gooFakeVal(in, "inst"))
	if res.Status == pay.StatusFailure {
		res.Code, res.ProviderCode = pay.CodeCardDeclined, "51"
	}
	return res, nil
}

func (f *gooFakePay) Query(ctx context.Context, a pay.Attempt) (*pay.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.queryErr != nil {
		return nil, f.queryErr
	}
	if f.query == nil {
		return &pay.Result{Status: pay.StatusPending}, nil
	}
	r := *f.query
	r.TxnID = "txn_" + a.Ref
	return &r, nil
}

func (f *gooFakePay) Refund(ctx context.Context, r pay.RefundRequest) (*pay.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refunds = append(f.refunds, r)
	if f.refundErr != nil {
		return nil, f.refundErr
	}
	return &pay.Result{Status: pay.StatusSuccess, TxnID: "rf_" + r.RefundRef, PaidAmount: r.Amount}, nil
}

func gooFakePaySpec(f *gooFakePay) pay.Spec {
	return pay.Spec{ID: "fakepay", Title: "FakePay", Description: "A provider for tests.", Region: pay.RegionTR, Kind: pay.KindPSP,
		Features: []pay.Feature{pay.Feature3DS, pay.FeatureInstallments, pay.FeatureRefunds, pay.FeatureQuery},
		Fields: []pay.Field{
			{ID: "merchant_id", Title: "Merchant ID", Type: "text", Required: true},
			{ID: "secret_key", Title: "Secret key", Type: "text", Secret: true, Required: true},
		},
		Hosts: pay.Hosts{Form: []string{"https://pay.fake.test"}, Frame: []string{"https://frame.fake.test"}, Script: []string{"https://js.fake.test"}},
		New: func(c pay.Config) (pay.Provider, error) {
			if err := c.Require("merchant_id", "secret_key"); err != nil {
				return nil, err
			}
			return f, nil
		}}
}

const gooFakePayID = "goo_fakepay"

func gooFakePayRegister(t *testing.T, srv *Server) (*gooFakePay, *gooPayGateway) {
	t.Helper()
	f := &gooFakePay{}
	g := &gooPayGateway{spec: gooFakePaySpec(f)}
	gooRegisterGateway(g)
	t.Cleanup(func() { delete(gooGatewayRegistry, g.ID()) })
	gooFakePaySettings(t, srv, map[string]any{"enabled": "yes", "testmode": "yes", "merchant_id": "m-test", "secret_key": "fake-secret-not-real"})
	return f, g
}

func gooFakePaySettings(t *testing.T, srv *Server, st map[string]any) {
	t.Helper()
	if err := srv.gooSetOpts(context.Background(), map[string]any{gooFakePayID + "_settings": st}); err != nil {
		t.Fatal(err)
	}
}

// gooFakePayCheckout places an order with FakePay through the checkout.
func gooFakePayCheckout(t *testing.T, srv *Server, name string, price float64) (*gooOrder, *gooPaymentResult) {
	t.Helper()
	ctx := context.Background()
	p := gooMakeSimple(t, srv, name, price, func(p *gooProduct) { p.Virtual = true })
	c := gooTestCtx()
	if _, err := srv.gooCartFor(c).Add(ctx, p.ID, 2, 0, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	form := map[string][]string{}
	for k, v := range gxCheckoutForm {
		form[k] = v
	}
	form["payment_method"] = []string{gooFakePayID}
	res, o, _ := srv.gooProcessCheckout(c, form)
	if o == nil {
		t.Fatalf("checkout: no order (%+v)", res)
	}
	o, _ = srv.gooGetOrder(ctx, o.ID)
	return o, res
}

func gooFakePayAttempts(t *testing.T, srv *Server, orderID int64) []*gooPayAttempt {
	t.Helper()
	rows, err := srv.store.Pool.Query(context.Background(), `SELECT `+gooPayAttemptCols+` FROM goo_payment_attempts WHERE order_id = $1 ORDER BY id`, orderID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []*gooPayAttempt
	for rows.Next() {
		a, err := gooScanPayAttempt(rows)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, a)
	}
	return out
}

func gooFakePayNotes(t *testing.T, srv *Server, orderID int64, like string) int {
	t.Helper()
	var n int
	if err := srv.store.Pool.QueryRow(context.Background(), `SELECT count(*) FROM goo_order_notes WHERE order_id = $1 AND content LIKE '%' || $2 || '%'`,
		orderID, like).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestGooPayBridge(t *testing.T) {
	base, admin, srv := gooSite(t)
	ctx := context.Background()
	fake, g := gooFakePayRegister(t, srv)
	guest := gooGuestClient()
	cb := base + "/wc-api/" + gooFakePayID + "/"
	notify := func(v url.Values) (int, string) {
		t.Helper()
		code, body, _ := post(t, guest, cb+"?pay=notify", v)
		return code, body
	}
	order := func(id int64) *gooOrder {
		t.Helper()
		o, err := srv.gooGetOrder(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return o
	}

	// Settings: the secret is never printed back; the notification URL is shown.
	if _, page, _ := get(t, admin, base+"/wp-admin/goocommerce?page=settings&tab=checkout&section="+gooFakePayID); strings.Contains(page, "fake-secret-not-real") {
		t.Error("the settings page shows a saved secret")
	} else if !strings.Contains(page, "/wc-api/goo_fakepay/?pay=notify") {
		t.Error("the settings page does not show the notification URL")
	}
	if p := g.SetupProblem(srv); p != "" {
		t.Fatalf("configured gateway has a setup problem: %s", p)
	}

	// Start → redirect: the order waits, the attempt holds the amount; the basket adds up.
	o, res := gooFakePayCheckout(t, srv, "Pay Lamp", 12.5)
	if res == nil || res.Result != "success" || !strings.HasPrefix(res.Redirect, "https://pay.fake.test/r/") || o.Status != "pending" {
		t.Fatalf("redirect start: %+v, status %s", res, o.Status)
	}
	as := gooFakePayAttempts(t, srv, o.ID)
	if len(as) != 1 || as[0].Amount != 2500 || as[0].Currency != "USD" || as[0].Status != "started" || !as[0].Test || len(as[0].Ref) != 20 {
		t.Fatalf("attempt: %+v", as)
	}
	a := as[0]
	st := fake.lastStart
	var sum int64
	for _, it := range st.Items {
		sum += int64(it.Quantity) * it.UnitPrice
	}
	if sum != st.Amount || st.Ref != a.Ref || strings.Contains(st.NotifyURL, a.Ref) || !strings.Contains(st.ReturnURL, "a="+a.Ref) ||
		st.MaxInstallment != 12 || st.Installment != 0 {
		t.Fatalf("start request: %+v", st)
	}

	// Callbacks that do not verify, or name no attempt of this gateway, change nothing.
	if code, body := notify(url.Values{"ref": {a.Ref}, "sig": {"forged"}, "status": {"success"}, "amount": {"2500"}}); code != http.StatusBadRequest {
		t.Errorf("bad signature: %d %s", code, body)
	}
	if code, body := notify(url.Values{"ref": {"UnknownRef1234567890"}, "sig": {"ok"}, "status": {"success"}, "amount": {"2500"}}); code != http.StatusBadRequest || body != "-1" {
		t.Errorf("unknown ref: %d %s", code, body)
	}
	if order(o.ID).Status != "pending" {
		t.Fatal("a refused callback changed the order")
	}

	// Success, sent twice and concurrently: paid once, one note, the provider's ack every time.
	good := url.Values{"ref": {a.Ref}, "sig": {"ok"}, "status": {"success"}, "amount": {"2500"}, "cur": {"USD"}}
	var wg sync.WaitGroup
	var bad atomic.Int32
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := guest.PostForm(cb+"?pay=notify", good)
			if err != nil || resp.StatusCode != http.StatusOK {
				bad.Add(1)
			}
			if resp != nil {
				resp.Body.Close()
			}
		}()
	}
	wg.Wait()
	if bad.Load() != 0 {
		t.Errorf("%d concurrent notifications failed", bad.Load())
	}
	if code, body := notify(good); code != http.StatusOK || body != "FAKE-OK" {
		t.Errorf("repeated notify: %d %q", code, body)
	}
	if fake.maxIn.Load() != 1 {
		t.Errorf("Complete ran %d times at once for one attempt", fake.maxIn.Load())
	}
	o = order(o.ID)
	if !o.IsPaid() || o.TransactionID != "txn_"+a.Ref || gxaStr(o.Meta, "_goo_pay_ref") != a.Ref {
		t.Fatalf("after notify: %s %q", o.Status, o.TransactionID)
	}
	if n := gooFakePayNotes(t, srv, o.ID, "payment complete"); n != 1 {
		t.Errorf("payment complete notes: %d", n)
	}
	if as := gooFakePayAttempts(t, srv, o.ID); as[0].Status != "paid" || as[0].Paid != 2500 || as[0].Start != nil {
		t.Errorf("paid attempt: %+v", as[0])
	}

	// Refunds: partial, then the rest; the attempt keeps the refunded total.
	if _, err := srv.gooCreateRefund(ctx, o, 5, "Scratch", nil, false, true, 1); err != nil {
		t.Fatalf("partial refund: %v", err)
	}
	if r := fake.refunds[len(fake.refunds)-1]; r.Amount != 500 || r.Full || r.Attempt.Ref != a.Ref || r.RefundRef == "" {
		t.Errorf("partial refund request: %+v", r)
	}
	fake.refundErr = pay.ErrUnknownOutcome
	if _, err := srv.gooCreateRefund(ctx, order(o.ID), 5, "", nil, false, true, 1); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Errorf("unknown outcome refund: %v", err)
	}
	fake.refundErr = nil
	if as := gooFakePayAttempts(t, srv, o.ID); as[0].Refunded != 500 {
		t.Errorf("refunded total: %d", as[0].Refunded)
	}
	o2, _ := gooFakePayCheckout(t, srv, "Pay Rug", 10)
	a2 := gooFakePayAttempts(t, srv, o2.ID)[0]
	notify(url.Values{"ref": {a2.Ref}, "sig": {"ok"}, "status": {"success"}, "amount": {"2000"}})
	if _, err := srv.gooCreateRefund(ctx, order(o2.ID), 20, "", nil, false, true, 1); err != nil {
		t.Fatalf("full refund: %v", err)
	}
	if r := fake.refunds[len(fake.refunds)-1]; r.Amount != 2000 || !r.Full {
		t.Errorf("full refund request: %+v", r)
	}

	// Underpaid and wrong currency: held for review, never paid.
	for name, v := range map[string]url.Values{"underpaid": {"amount": {"1999"}}, "currency": {"amount": {"2000"}, "cur": {"EUR"}}} {
		ou, _ := gooFakePayCheckout(t, srv, "Pay "+name, 10)
		au := gooFakePayAttempts(t, srv, ou.ID)[0]
		v.Set("ref", au.Ref)
		v.Set("sig", "ok")
		v.Set("status", "success")
		notify(v)
		notify(v)
		if got := order(ou.ID); got.Status != "on-hold" || got.IsPaid() {
			t.Errorf("%s: status %s", name, got.Status)
		}
		if n := gooFakePayNotes(t, srv, ou.ID, "was not accepted"); n != 1 {
			t.Errorf("%s: %d review notes", name, n)
		}
	}

	// The order total changed after the payment was started.
	oc, _ := gooFakePayCheckout(t, srv, "Pay Chair", 10)
	ac := gooFakePayAttempts(t, srv, oc.ID)[0]
	if _, err := srv.store.Pool.Exec(ctx, `UPDATE goo_orders SET total = 30 WHERE id = $1`, oc.ID); err != nil {
		t.Fatal(err)
	}
	notify(url.Values{"ref": {ac.Ref}, "sig": {"ok"}, "status": {"success"}, "amount": {"2000"}})
	if got := order(oc.ID); got.Status != "on-hold" {
		t.Errorf("changed total: status %s", got.Status)
	}

	// Two attempts: a late failure of the older one does not fail the order; the newest one does.
	ob, _ := gooFakePayCheckout(t, srv, "Pay Bench", 10)
	c := gooTestCtx()
	if r, err := g.ProcessPayment(srv, c, order(ob.ID)); err != nil || r.Result != "success" {
		t.Fatalf("second attempt: %v", err)
	}
	ab := gooFakePayAttempts(t, srv, ob.ID)
	if len(ab) != 2 {
		t.Fatalf("attempts: %d", len(ab))
	}
	notify(url.Values{"ref": {ab[0].Ref}, "sig": {"ok"}, "status": {"failure"}})
	if got := order(ob.ID); got.Status != "pending" {
		t.Errorf("old attempt's failure moved the order to %s", got.Status)
	}
	// A browser callback without the order key (or with a wrong one) is refused before the provider
	// sees it: the attempt reference alone must not move an order or reveal its key.
	for _, k := range []string{"", "&k=wrong"} {
		code, body, loc := post(t, guest, cb+"?pay=fail&a="+ab[1].Ref+k, url.Values{"sig": {"ok"}, "status": {"failure"}})
		if code != http.StatusBadRequest || body != "-1" || loc != "" {
			t.Fatalf("fail return without the order key: %d %q %s", code, body, loc)
		}
	}
	if got := order(ob.ID); got.Status != "pending" {
		t.Errorf("a callback without the order key moved the order to %s", got.Status)
	}
	// A browser callback the provider refuses changes nothing and lands on the result page.
	if code, _, loc := post(t, guest, cb+"?pay=fail&a="+ab[1].Ref+"&k="+url.QueryEscape(ob.OrderKey), url.Values{"sig": {"bad"}, "status": {"failure"}}); code != http.StatusSeeOther || !strings.Contains(loc, "pay=done") {
		t.Fatalf("refused browser callback: %d %s", code, loc)
	}
	if got := order(ob.ID); got.Status != "pending" {
		t.Errorf("a refused browser callback moved the order to %s", got.Status)
	}
	// The newest fails on the browser's fail URL: back to Pay for order with the reason.
	code, _, loc := post(t, guest, cb+"?pay=fail&a="+ab[1].Ref+"&k="+url.QueryEscape(ob.OrderKey), url.Values{"sig": {"ok"}, "status": {"failure"}})
	if code != http.StatusSeeOther || !strings.Contains(loc, "pay=done") {
		t.Fatalf("fail return: %d %s", code, loc)
	}
	if got := order(ob.ID); got.Status != "failed" {
		t.Errorf("newest attempt's failure: status %s", got.Status)
	}
	if code, _, loc := get(t, guest, loc); code != http.StatusFound || !strings.Contains(loc, "pay_for_order") {
		t.Errorf("done after failure: %d %s", code, loc)
	}

	// A browser return without proof: the bridge asks the provider once.
	op, _ := gooFakePayCheckout(t, srv, "Pay Stool", 10)
	ap := gooFakePayAttempts(t, srv, op.ID)[0]
	retURL := cb + "?pay=return&a=" + ap.Ref + "&k=" + url.QueryEscape(op.OrderKey)
	code, _, loc = get(t, guest, retURL+"&sig=ok&status=pending")
	if code != http.StatusSeeOther || order(op.ID).Status != "pending" {
		t.Fatalf("pending return: %d", code)
	}
	if _, _, l := get(t, guest, loc); !strings.Contains(l, "order-received") {
		t.Errorf("pending done: %s", l)
	}
	fake.mu.Lock()
	fake.query = &pay.Result{Status: pay.StatusSuccess, PaidAmount: 2000, Currency: "USD"}
	fake.mu.Unlock()
	get(t, guest, retURL+"&sig=ok&status=pending")
	if got := order(op.ID); !got.IsPaid() {
		t.Errorf("query success: status %s", got.Status)
	}
	fake.mu.Lock()
	fake.query = nil
	fake.mu.Unlock()

	// Form and iframe pages: escaped values, only the spec's hosts in the CSP, the key checked.
	fake.mode = "form"
	of, rf := gooFakePayCheckout(t, srv, "Pay Vase", 10)
	if rf == nil || !strings.Contains(rf.Redirect, "pay=go") {
		t.Fatalf("form start: %+v", rf)
	}
	resp, err := guest.Get(rf.Redirect)
	if err != nil {
		t.Fatal(err)
	}
	page := gooReadBody(resp)
	csp := resp.Header.Get("Content-Security-Policy")
	if resp.StatusCode != http.StatusOK || !strings.Contains(page, `action="https://pay.fake.test/pay"`) || strings.Contains(page, `"><script>alert`) ||
		!strings.Contains(page, "&lt;script&gt;") || !strings.Contains(csp, "https://pay.fake.test") || strings.Contains(csp, "evil.example") {
		t.Errorf("form page %d: csp %q\n%s", resp.StatusCode, csp, page)
	}
	af := gooFakePayAttempts(t, srv, of.ID)[0]
	if code, body, _ := get(t, guest, cb+"?pay=go&a="+af.Ref+"&k=wc_order_forged"); code != http.StatusBadRequest || body != "-1" {
		t.Errorf("go with a wrong key: %d", code)
	}
	fake.mode = "embed"
	_, re := gooFakePayCheckout(t, srv, "Pay Bowl", 10)
	resp, err = guest.Get(re.Redirect)
	if err != nil {
		t.Fatal(err)
	}
	page = gooReadBody(resp)
	csp = resp.Header.Get("Content-Security-Policy")
	if !strings.Contains(page, `<iframe id="goo-pay-frame" src="https://frame.fake.test/f?t=1"`) || !strings.Contains(csp, "https://js.fake.test") ||
		!strings.Contains(page, `src="https://js.fake.test/sdk.js"`) || !strings.Contains(page, "iFrameResize") {
		t.Errorf("embed page: csp %q\n%s", csp, page)
	}
	fake.mode = "badhost"
	ox, rx := gooFakePayCheckout(t, srv, "Pay Cup", 10)
	if rx != nil && rx.Result == "success" {
		t.Errorf("a form on an unlisted host was accepted: %+v", rx)
	}
	if ax := gooFakePayAttempts(t, srv, ox.ID); len(ax) != 1 || ax[0].Status != "failed" {
		t.Errorf("refused start attempt: %+v", ax)
	}
	fake.mode = "redirect"

	// Configured but switched off: callbacks of payments in flight still arrive. Unconfigured and
	// off: the route answers like an unknown gateway.
	ol, _ := gooFakePayCheckout(t, srv, "Pay Mug", 10)
	al := gooFakePayAttempts(t, srv, ol.ID)[0]
	gooFakePaySettings(t, srv, map[string]any{"enabled": "no", "testmode": "yes", "merchant_id": "m-test", "secret_key": "fake-secret-not-real"})
	if code, body := notify(url.Values{"ref": {al.Ref}, "sig": {"ok"}, "status": {"pending"}}); code != http.StatusOK || body != "FAKE-OK" {
		t.Errorf("configured, disabled: %d %q", code, body)
	}
	gooFakePaySettings(t, srv, map[string]any{"enabled": "no", "testmode": "yes"})
	if code, body := notify(url.Values{"ref": {al.Ref}, "sig": {"ok"}, "status": {"success"}, "amount": {"2000"}}); code != http.StatusBadRequest || body != "-1" {
		t.Errorf("unconfigured, disabled: %d %q", code, body)
	}
	if order(ol.ID).IsPaid() {
		t.Error("an unconfigured gateway's callback paid an order")
	}
}

func gooReadBody(resp *http.Response) string {
	defer resp.Body.Close()
	var b strings.Builder
	buf := make([]byte, 32<<10)
	for {
		n, err := resp.Body.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			return b.String()
		}
	}
}

func TestGooPayStartRejected(t *testing.T) {
	_, _, srv := gooSite(t)
	fake, _ := gooFakePayRegister(t, srv)
	fake.mode = "reject"
	o, res := gooFakePayCheckout(t, srv, "Pay Kettle", 3)
	if res != nil && res.Result == "success" {
		t.Fatalf("a rejected start succeeded: %+v", res)
	}
	as := gooFakePayAttempts(t, srv, o.ID)
	if len(as) != 1 || as[0].Status != "failed" || as[0].Code != string(pay.CodeConfiguration) {
		t.Errorf("attempt: %+v", as)
	}
}

func TestGooPayItemsExact(t *testing.T) {
	_, _, srv := gooSite(t)
	ctx := context.Background()
	o := &gooOrder{ID: 7, Currency: "TRY", Items: []*gooOrderItem{
		{Type: "line_item", Name: "A", Quantity: 3, Total: 10, TotalTax: 0},
		{Type: "shipping", Name: "Cargo", Total: 4.99, TotalTax: 0},
		{Type: "fee", Name: "Gift wrap", Total: 1, TotalTax: 0.2},
	}}
	items := srv.gooPayItems(ctx, o, 1619)
	var sum int64
	for _, it := range items {
		sum += int64(it.Quantity) * it.UnitPrice
	}
	if sum != 1619 || len(items) != 3 || items[0].Quantity != 1 {
		t.Errorf("items %+v sum %d", items, sum)
	}
	if items := srv.gooPayItems(ctx, o, 2000); len(items) != 1 || items[0].UnitPrice != 2000 {
		t.Errorf("fallback: %+v", items)
	}
}

// gooRouteProbe is a gateway with a callback handler and a setup state the routing test sets.
type gooRouteProbe struct {
	gooTestGateway
	problem string
}

func (g *gooRouteProbe) ID() string                          { return "goo_probe" }
func (g *gooRouteProbe) SetupProblem(s *Server) string       { return g.problem }
func (g *gooRouteProbe) HandleAPI(s *Server, c *gin.Context) { c.String(http.StatusOK, "hit") }

func TestGooWCAPIRouteNeedsConfiguredGateway(t *testing.T) {
	base, _, srv := gooSite(t)
	probe := &gooRouteProbe{problem: "The key is missing."}
	gooRegisterGateway(probe)
	t.Cleanup(func() { delete(gooGatewayRegistry, "goo_probe") })
	guest := gooGuestClient()
	set := func(enabled string) {
		if err := srv.gooSetOpts(context.Background(), map[string]any{"goo_probe_settings": map[string]any{"enabled": enabled}}); err != nil {
			t.Fatal(err)
		}
	}
	set("no")
	if code, body, _ := get(t, guest, base+"/wc-api/goo_probe/"); code != http.StatusBadRequest || body != "-1" {
		t.Errorf("unconfigured, disabled: %d %q", code, body)
	}
	probe.problem = ""
	if code, body, _ := get(t, guest, base+"/wc-api/goo_probe/"); code != http.StatusOK || body != "hit" {
		t.Errorf("configured, disabled: %d %q", code, body)
	}
	probe.problem = "The key is missing."
	set("yes")
	if code, body, _ := get(t, guest, base+"/?wc-api=goo_probe"); code != http.StatusOK || body != "hit" {
		t.Errorf("enabled: %d %q", code, body)
	}
}

func TestGooPayParsePlan(t *testing.T) {
	got := gooPayParsePlan("6:5.9, 2:0\n3:2.5;x:1, 4, 1:0, 13:1, 5:-1, 7:101, 3:9, 9:abc, 12:11")
	want := []gooPayPlanEntry{{2, 0}, {3, 2.5}, {6, 5.9}, {12, 11}}
	if len(got) != len(want) {
		t.Fatalf("plan: %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("plan: %+v", got)
		}
	}
	if gooPayParsePlan("") != nil {
		t.Error("an empty plan has entries")
	}
}

const gooFakeBankID = "goo_fakebank"

// gooFakeBankRegister adds a bank POS gateway with store-chosen installments, in a TRY store.
func gooFakeBankRegister(t *testing.T, srv *Server, plan, minimum string) (*gooFakePay, *gooPayGateway) {
	t.Helper()
	f := &gooFakePay{}
	sp := gooFakePaySpec(f)
	sp.ID, sp.Title, sp.Kind = "fakebank", "FakeBank", pay.KindBank
	g := &gooPayGateway{spec: sp}
	gooRegisterGateway(g)
	t.Cleanup(func() { delete(gooGatewayRegistry, g.ID()) })
	ctx := context.Background()
	if err := srv.gooSetOpts(ctx, map[string]any{gooFakeBankID + "_settings": map[string]any{"enabled": "yes", "testmode": "yes", "merchant_id": "m-test",
		"secret_key": "fake-secret-not-real", "installments": "yes", "installment_plan": plan, "installment_min": minimum}, "currency": "TRY"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.gooSetOpts(ctx, map[string]any{"currency": "USD"}) })
	return f, g
}

// gooFakeBankCart is a fresh customer with 2 × 100.00 in the cart.
func gooFakeBankCart(t *testing.T, srv *Server, name string) *gin.Context {
	t.Helper()
	p := gooMakeSimple(t, srv, name, 100, func(p *gooProduct) { p.Virtual = true })
	c := gooTestCtx()
	if _, err := srv.gooCartFor(c).Add(context.Background(), p.ID, 2, 0, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	return c
}

// gooFakeBankForm is the checkout form choosing a gateway and an installment count; submit adds the
// checkout nonce field, which marks the order submission (as opposed to an order review refresh).
func gooFakeBankForm(gateway string, n int, submit bool) url.Values {
	form := url.Values{}
	for k, v := range gxCheckoutForm {
		form[k] = v
	}
	form.Set("payment_method", gateway)
	form.Set(gateway+"_installment", strconv.Itoa(n))
	if submit {
		form.Set("woocommerce-process-checkout-nonce", "x")
	}
	return form
}

// gooFakeBankReview recalculates the cart as the order review refresh does.
func gooFakeBankReview(srv *Server, c *gin.Context, form url.Values) *gooCart {
	c.Request.PostForm = form
	cart := srv.gooCartFor(c)
	srv.gooSessionFor(c).Data.ChosenPayment = form.Get("payment_method")
	cart.Calculate()
	return cart
}

// gooFakeBankSubmit places the order as the checkout's AJAX post does (the form is the request's).
func gooFakeBankSubmit(srv *Server, c *gin.Context, form url.Values) (*gooPaymentResult, *gooOrder, []string) {
	c.Request.PostForm = form
	return srv.gooProcessCheckout(c, form)
}

func gooFakeBankFee(cart *gooCart) *gooFee {
	for _, f := range cart.Fees {
		if _, _, ok := gooPayInstFeeOf(f.ID); ok {
			return f
		}
	}
	return nil
}

func TestGooPayInstallments(t *testing.T) {
	_, _, srv := gooSite(t)
	ctx := context.Background()
	bank, g := gooFakeBankRegister(t, srv, "2:0, 3:10, 6:abc", "")
	psp, _ := gooFakePayRegister(t, srv)

	// The settings offer the plan for the bank gateway only.
	var ids []string
	for _, f := range g.FormFields() {
		ids = append(ids, f.ID)
	}
	if !strings.Contains(strings.Join(ids, ","), "installment_plan") || strings.Contains(strings.Join(ids, ","), "max_installment") {
		t.Errorf("bank settings: %v", ids)
	}

	// Choosing 3 installments adds the 10% surcharge to the cart; the select shows per-month amounts.
	c := gooFakeBankCart(t, srv, "Inst Lamp")
	cart := gooFakeBankReview(srv, c, gooFakeBankForm(gooFakeBankID, 3, false))
	if f := gooFakeBankFee(cart); f == nil || f.Total != 20 || cart.Total != 220 || !strings.Contains(f.Name, "3") {
		t.Fatalf("fee: %+v, total %v", f, cart.Total)
	}
	fields := g.PaymentFields(srv, c)
	if !strings.Contains(fields, `name="goo_fakebank_installment"`) || !strings.Contains(fields, "Single payment") ||
		!strings.Contains(fields, "3 × ") || !strings.Contains(fields, `value="3" selected`) || strings.Contains(fields, `value="6"`) {
		t.Errorf("payment fields: %s", fields)
	}
	// Back to single payment: the fee is gone.
	if cart = gooFakeBankReview(srv, c, gooFakeBankForm(gooFakeBankID, 0, false)); gooFakeBankFee(cart) != nil || cart.Total != 200 {
		t.Errorf("single payment: total %v", cart.Total)
	}

	// The order carries the fee; Start receives the count and the amount with the surcharge.
	gooFakeBankReview(srv, c, gooFakeBankForm(gooFakeBankID, 3, false))
	res, o, errs := gooFakeBankSubmit(srv, c, gooFakeBankForm(gooFakeBankID, 3, true))
	if o == nil || len(errs) > 0 || res == nil || res.Result != "success" {
		t.Fatalf("checkout: %+v %v", res, errs)
	}
	o, _ = srv.gooGetOrder(ctx, o.ID)
	if o.Total != 220 || len(o.ItemsOf("fee")) != 1 || !strings.Contains(o.ItemsOf("fee")[0].Name, "Installment surcharge") {
		t.Fatalf("order: total %v, fees %+v", o.Total, o.ItemsOf("fee"))
	}
	if st := bank.lastStart; st.Installment != 3 || st.Amount != 22000 || st.Currency != "TRY" {
		t.Errorf("start: installment %d amount %d %s", st.Installment, st.Amount, st.Currency)
	}
	if as := gooFakePayAttempts(t, srv, o.ID); len(as) != 1 || as[0].Installment != 3 {
		t.Errorf("attempt: %+v", as)
	}
	if v, _ := o.Meta["_goo_pay_installment"].(float64); v != 3 {
		t.Errorf("order meta installment: %v", o.Meta["_goo_pay_installment"])
	}

	// A surcharge the customer was not shown (the review was not refreshed) refuses the submission.
	c = gooFakeBankCart(t, srv, "Inst Rug")
	gooFakeBankReview(srv, c, gooFakeBankForm(gooFakeBankID, 0, false))
	if _, o, errs := gooFakeBankSubmit(srv, c, gooFakeBankForm(gooFakeBankID, 3, true)); o != nil || len(errs) == 0 || !strings.Contains(errs[0], "surcharge") {
		t.Fatalf("unseen surcharge: %v %v", o, errs)
	}

	// A posted count that is not in the plan is a single payment, without a fee.
	c = gooFakeBankCart(t, srv, "Inst Vase")
	gooFakeBankReview(srv, c, gooFakeBankForm(gooFakeBankID, 5, false))
	_, o, errs = gooFakeBankSubmit(srv, c, gooFakeBankForm(gooFakeBankID, 5, true))
	if o == nil || len(errs) > 0 {
		t.Fatalf("checkout 5: %v", errs)
	}
	o, _ = srv.gooGetOrder(ctx, o.ID)
	if o.Total != 200 || len(o.ItemsOf("fee")) != 0 || bank.lastStart.Installment != 0 {
		t.Errorf("count not in plan: total %v installment %d", o.Total, bank.lastStart.Installment)
	}

	// Pay for order: only 0-rate entries are accepted without a surcharge line on the order.
	pc := gooTestCtx()
	pc.Request.PostForm = url.Values{"goo_fakebank_installment": {"2"}}
	if n := g.orderInstallment(srv, pc, o); n != 2 {
		t.Errorf("pay for order, rate 0: %d", n)
	}
	pc.Request.PostForm = url.Values{"goo_fakebank_installment": {"3"}}
	if n := g.orderInstallment(srv, pc, o); n != 0 {
		t.Errorf("pay for order, rate 10 without the fee line: %d", n)
	}

	// Installments are offered in TRY only, and from the minimum on.
	if g.installmentPlan(srv, "USD", 1000) != nil {
		t.Error("installments offered in USD")
	}
	gooFakeBankRegister(t, srv, "2:0, 3:10", "500")
	if g.installmentPlan(srv, "TRY", 200) != nil || len(g.installmentPlan(srv, "TRY", 500)) != 2 {
		t.Error("the minimum is not applied")
	}

	// A PSP is unaffected: no fee, the count is chosen on its page.
	c = gooFakeBankCart(t, srv, "Inst Cup")
	cart = gooFakeBankReview(srv, c, gooFakeBankForm(gooFakePayID, 3, false))
	if gooFakeBankFee(cart) != nil || cart.Total != 200 {
		t.Errorf("psp fee: total %v", cart.Total)
	}
	if _, o, errs := gooFakeBankSubmit(srv, c, gooFakeBankForm(gooFakePayID, 3, true)); o == nil || len(errs) > 0 ||
		psp.lastStart.Installment != 0 || psp.lastStart.MaxInstallment != 12 || psp.lastStart.Amount != 20000 {
		t.Errorf("psp start: %+v %v", psp.lastStart, errs)
	}
}

func TestGooPayCheckStatusAction(t *testing.T) {
	base, admin, srv := gooSite(t)
	ctx := context.Background()
	fake, _ := gooFakePayRegister(t, srv)
	o, _ := gooFakePayCheckout(t, srv, "Check Lamp", 12.5)
	a := gooFakePayAttempts(t, srv, o.ID)[0]
	edit := base + "/wp-admin/goocommerce?page=orders&action=edit&id=" + itoa64(o.ID)
	_, body, _ := get(t, admin, edit)
	if !strings.Contains(body, "Check payment status with FakePay") || !strings.Contains(body, a.Ref) {
		t.Fatal("the order screen shows no status check or attempt")
	}
	nonce := formNonce(t, body)
	run := func(c *http.Client, nonce string) {
		t.Helper()
		post(t, c, edit, url.Values{"_wpnonce": {nonce}, "wc_order_action": {gooPayCheckAction}})
	}
	status := func() string {
		t.Helper()
		got, err := srv.gooGetOrder(ctx, o.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got.Status
	}

	// Refused without the nonce or the capability.
	fake.query = &pay.Result{Status: pay.StatusSuccess, PaidAmount: 2500, Currency: "USD"}
	run(admin, "bad")
	asCreateUser(t, srv, "paysub", "subscriber", asStrongPass)
	sub := asNewClient()
	if code, _, _ := asLogin(t, sub, base, "/wp-login", "paysub", asStrongPass); code != http.StatusFound {
		t.Fatalf("login: %d", code)
	}
	run(sub, nonce)
	if status() != "pending" {
		t.Fatal("the status check ran without the nonce or the capability")
	}

	// The provider cannot be reached: nothing changes.
	fake.queryErr = errors.New("dial tcp: timeout")
	run(admin, nonce)
	if status() != "pending" || gooFakePayAttempts(t, srv, o.ID)[0].Status != "started" {
		t.Fatal("a failed query changed the order")
	}
	fake.queryErr = nil

	// Still waiting, then paid.
	fake.query = nil
	if msg, err := srv.gooPayCheckStatus(ctx, o, ""); err != nil || !strings.Contains(msg, "Still waiting") {
		t.Errorf("pending: %q %v", msg, err)
	}
	fake.query = &pay.Result{Status: pay.StatusSuccess, PaidAmount: 2500, Currency: "USD"}
	run(admin, nonce)
	got, _ := srv.gooGetOrder(ctx, o.ID)
	if !got.IsPaid() || gooFakePayAttempts(t, srv, o.ID)[0].Status != "paid" {
		t.Fatalf("after the check: %s", got.Status)
	}
	if msg, err := srv.gooPayCheckStatus(ctx, got, ""); err != nil || !strings.Contains(msg, "confirms") {
		t.Errorf("paid again: %q %v", msg, err)
	}
}
