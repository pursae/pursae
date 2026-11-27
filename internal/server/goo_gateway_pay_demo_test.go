//go:build paydemo

package server

import (
	"context"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

var gooDemoHidden = regexp.MustCompile(`<input type="hidden" name="([^"]+)" value="([^"]*)">`)

// gooDemoForm is the action and hidden fields of the form with the given id ("" = the first form).
func gooDemoForm(t *testing.T, page, id string) (string, url.Values) {
	t.Helper()
	i := strings.Index(page, "<form")
	if id != "" {
		j := strings.Index(page, `id="`+id+`"`)
		i = strings.LastIndex(page[:max(j, 0)+1], "<form")
		if j < 0 {
			i = -1
		}
	}
	if i < 0 {
		t.Fatalf("no form %q in\n%s", id, page)
	}
	end := strings.Index(page[i:], "</form>")
	f := page[i : i+end]
	m := regexp.MustCompile(`action="([^"]+)"`).FindStringSubmatch(f)
	v := url.Values{}
	for _, h := range gooDemoHidden.FindAllStringSubmatch(f, -1) {
		v.Set(html.UnescapeString(h[1]), html.UnescapeString(h[2]))
	}
	return html.UnescapeString(m[1]), v
}

// The whole hosted flow with the demo bank: checkout → the store's payment page → the demo page →
// Approve → the signed POST to the return URL → the order is paid.
func TestGooPayDemoRoundTrip(t *testing.T) {
	if !gooPayAllowLoopbackHTTP {
		t.Fatal("the paydemo build does not allow the loopback demo page")
	}
	base, _, srv := gooSite(t)
	ctx := context.Background()
	if srv.gooGateway("goo_demo") == nil {
		t.Fatal("the demo bank is not registered in a paydemo build")
	}
	for _, flow := range []string{"form", "embed"} {
		if err := srv.gooSetOpts(ctx, map[string]any{"goo_demo_settings": map[string]any{"enabled": "yes", "testmode": "yes",
			"secret": "demo-secret-fake", "flow": flow}}); err != nil {
			t.Fatal(err)
		}
		p := gooMakeSimple(t, srv, "Demo "+flow, 21.5, func(p *gooProduct) { p.Virtual = true })
		c := gooTestCtx()
		if _, err := srv.gooCartFor(c).Add(ctx, p.ID, 1, 0, nil, nil, nil); err != nil {
			t.Fatal(err)
		}
		form := map[string][]string{}
		for k, v := range gxCheckoutForm {
			form[k] = v
		}
		form["payment_method"] = []string{"goo_demo"}
		res, o, _ := srv.gooProcessCheckout(c, form)
		if o == nil || res == nil || res.Result != "success" || !strings.Contains(res.Redirect, "pay=go") {
			t.Fatalf("%s: checkout: %+v", flow, res)
		}
		guest := gooGuestClient()
		resp, err := guest.Get(res.Redirect)
		if err != nil {
			t.Fatal(err)
		}
		page := gooReadBody(resp)
		csp := resp.Header.Get("Content-Security-Policy")
		var demoPage string
		if flow == "form" {
			action, v := gooDemoForm(t, page, "goo-pay-form")
			if !strings.HasPrefix(action, "http://127.0.0.1:") || !strings.Contains(csp, strings.TrimSuffix(action, "/pay")) {
				t.Fatalf("form: action %s, csp %q", action, csp)
			}
			demoPage = action + "?t=" + url.QueryEscape(v.Get("t"))
		} else {
			m := regexp.MustCompile(`<iframe id="goo-pay-frame" src="([^"]+)"`).FindStringSubmatch(page)
			if m == nil {
				t.Fatalf("embed: no iframe\n%s", page)
			}
			demoPage = html.UnescapeString(m[1])
			u, _ := url.Parse(demoPage)
			if !strings.Contains(csp, "http://"+u.Host) {
				t.Fatalf("embed: csp %q lacks %s", csp, u.Host)
			}
		}
		code, dp, _ := get(t, guest, demoPage)
		if code != http.StatusOK || !strings.Contains(dp, "Approve payment") {
			t.Fatalf("%s: demo page %d\n%s", flow, code, dp)
		}
		_, dv := gooDemoForm(t, dp, "")
		dv.Set("status", "approved")
		u, _ := url.Parse(demoPage)
		code, back, _ := post(t, guest, "http://"+u.Host+"/decide", dv)
		if code != http.StatusOK {
			t.Fatalf("%s: decide %d", flow, code)
		}
		ret, rv := gooDemoForm(t, back, "f")
		if !strings.HasPrefix(ret, base+"/wc-api/goo_demo/") || !strings.Contains(ret, "pay=return") {
			t.Fatalf("%s: back to %s", flow, ret)
		}
		if code, _, _ := post(t, guest, ret, rv); code >= 400 {
			t.Fatalf("%s: return %d", flow, code)
		}
		got, _ := srv.gooGetOrder(ctx, o.ID)
		if !got.IsPaid() {
			t.Errorf("%s: order %d is %s after an approved demo payment", flow, o.ID, got.Status)
		}
	}
}
