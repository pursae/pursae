//go:build paydemo

package demo

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"wpgo/internal/pay"
)

var hiddenRe = regexp.MustCompile(`<input type="hidden" name="([^"]+)" value="([^"]*)">`)

// click opens the demo page and presses one of its buttons; it returns where the browser is sent back
// to and the fields it posts.
func click(t *testing.T, page, tok, status string) (string, url.Values) {
	t.Helper()
	resp, err := http.Get(page + "?t=" + tok)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	for _, want := range []string{"Approve payment", "Decline (insufficient funds)", "Cancel"} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("demo page lacks %q", want)
		}
	}
	if regexp.MustCompile(`(?i)card|cvv|cvc|expir`).Match(b) && !strings.Contains(string(b), "no card is asked for") {
		t.Fatal("the demo page mentions card data")
	}
	resp, err = http.PostForm(strings.TrimSuffix(page, "/pay")+"/decide", url.Values{"t": {tok}, "status": {status}})
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	m := regexp.MustCompile(`action="([^"]+)"`).FindSubmatch(b)
	if m == nil {
		t.Fatalf("no form back to the store: %s", b)
	}
	v := url.Values{}
	for _, f := range hiddenRe.FindAllSubmatch(b, -1) {
		v.Set(string(f[1]), strings.ReplaceAll(string(f[2]), "&amp;", "&"))
	}
	return strings.ReplaceAll(string(m[1]), "&amp;", "&"), v
}

func TestDemoFlow(t *testing.T) {
	sp := Spec()
	if _, err := sp.New(pay.Config{Values: map[string]string{}}); err == nil {
		t.Fatal("built without a secret")
	}
	ctx := context.Background()
	p, err := sp.New(pay.Config{Values: map[string]string{"secret": "demo-secret-fake", "flow": "form"}})
	if err != nil {
		t.Fatal(err)
	}
	req := pay.StartRequest{Ref: "DEMOref0001", Amount: 12345, Currency: "TRY", Installment: 3,
		ReturnURL: "https://shop.example/r?a=1", FailURL: "https://shop.example/f?a=1"}
	res, err := p.Start(ctx, req)
	if err != nil || res.Form == nil || !strings.HasPrefix(res.Form.Action, "http://127.0.0.1:") {
		t.Fatalf("start: %+v %v", res, err)
	}
	tok := res.Form.Fields[0][1]
	a := pay.Attempt{Ref: req.Ref, Amount: req.Amount, Currency: "TRY", ProviderRef: res.ProviderRef}

	if q, _ := p.Query(ctx, a); q.Status != pay.StatusPending {
		t.Errorf("query before approval: %+v", q)
	}
	back, v := click(t, res.Form.Action, tok, "approved")
	if back != req.ReturnURL || v.Get("installment") != "3" || v.Get("amount") != "12345" {
		t.Errorf("approve: %s %v", back, v)
	}
	r, err := p.Complete(ctx, pay.CallbackInput{Kind: pay.CallbackReturn, Method: "POST", Form: v}, a)
	if err != nil || r.Status != pay.StatusSuccess || r.PaidAmount != 12345 || r.Currency != "TRY" || r.Installment != 3 {
		t.Errorf("complete: %+v %v", r, err)
	}
	if q, _ := p.Query(ctx, a); q.Status != pay.StatusSuccess || q.PaidAmount != 12345 {
		t.Errorf("query after approval: %+v", q)
	}

	// Tampered amount, another attempt, another secret: refused.
	bad := url.Values{}
	for k := range v {
		bad.Set(k, v.Get(k))
	}
	bad.Set("amount", "1")
	if _, err := p.Complete(ctx, pay.CallbackInput{Form: bad}, a); err == nil {
		t.Error("a tampered amount was accepted")
	}
	if _, err := p.Complete(ctx, pay.CallbackInput{Form: v}, pay.Attempt{Ref: "OTHERref0001"}); err == nil {
		t.Error("a callback of another attempt was accepted")
	}
	other, _ := sp.New(pay.Config{Values: map[string]string{"secret": "another-fake"}})
	if _, err := other.Complete(ctx, pay.CallbackInput{Form: v}, a); err == nil {
		t.Error("a signature with another secret was accepted")
	}

	// Decline and cancel go to the fail URL.
	res2, _ := p.Start(ctx, pay.StartRequest{Ref: "DEMOref0002", Amount: 500, Currency: "TRY", ReturnURL: "https://shop.example/r", FailURL: "https://shop.example/f"})
	back, v = click(t, res2.Form.Action, res2.Form.Fields[0][1], "declined")
	r, err = p.Complete(ctx, pay.CallbackInput{Form: v}, pay.Attempt{Ref: "DEMOref0002"})
	if back != "https://shop.example/f" || err != nil || r.Status != pay.StatusFailure || r.Code != pay.CodeInsufficientFunds {
		t.Errorf("decline: %s %+v %v", back, r, err)
	}
	if rf, err := p.Refund(ctx, pay.RefundRequest{Attempt: a, Amount: 100, RefundRef: "rf1"}); err != nil || rf.Status != pay.StatusSuccess || len(Refunds(a.Ref)) != 1 {
		t.Errorf("refund: %+v %v", rf, err)
	}

	// The other flows.
	for flow, ok := range map[string]func(*pay.StartResult) bool{
		"embed": func(r *pay.StartResult) bool {
			return r.Embed != nil && strings.HasPrefix(r.Embed.FrameURL, "http://127.0.0.1:")
		},
		"redirect": func(r *pay.StartResult) bool { return strings.HasPrefix(r.Redirect, "http://127.0.0.1:") },
	} {
		pf, _ := sp.New(pay.Config{Values: map[string]string{"secret": "demo-secret-fake", "flow": flow}})
		r, err := pf.Start(ctx, pay.StartRequest{Ref: "DEMOflow" + flow, Amount: 1, Currency: "TRY", ReturnURL: "https://shop.example/r"})
		if err != nil || !ok(r) {
			t.Errorf("%s: %+v %v", flow, r, err)
		}
	}
}
