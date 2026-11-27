package server

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"wpgo/internal/pay"
)

// gooPayCSPDirectives is a policy as directive → sources.
func gooPayCSPDirectives(policy string) map[string][]string {
	out := map[string][]string{}
	for _, part := range strings.Split(policy, ";") {
		if f := strings.Fields(part); len(f) > 0 {
			out[strings.ToLower(f[0])] = f[1:]
		}
	}
	return out
}

// The ?pay=go page is a page of the shop: the theme's header and footer around the order summary and
// the provider's form or iframe; its CSP is the site's own plus the provider's hosts.
func TestGooPayGoPageThemed(t *testing.T) {
	base, _, srv := gooSite(t)
	fake, _ := gooFakePayRegister(t, srv)
	guest := gooGuestClient()

	home, err := guest.Get(base + "/")
	if err != nil {
		t.Fatal(err)
	}
	homePage := gooReadBody(home)
	siteCSP := gooPayCSPDirectives(home.Header.Get("Content-Security-Policy"))
	if len(siteCSP) == 0 {
		t.Fatal("the store front sends no Content-Security-Policy")
	}
	// Theme markers: what a normal page of this site has around its content.
	var markers []string
	for _, m := range []string{"<header", "<footer", `class="site-header`, `class="site-footer`, `class="wp-site-blocks"`} {
		if strings.Contains(homePage, m) {
			markers = append(markers, m)
		}
	}
	if len(markers) == 0 {
		t.Fatal("no theme marker found on the home page")
	}

	check := func(what string, resp *http.Response, want ...string) string {
		t.Helper()
		page := gooReadBody(resp)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: %d\n%s", what, resp.StatusCode, page)
		}
		gooMustContain(t, what, page, append(markers, want...)...)
		for _, h := range []string{"Referrer-Policy", "Cache-Control"} {
			if v := resp.Header.Get(h); (h == "Referrer-Policy" && v != "no-referrer") || (h == "Cache-Control" && !strings.Contains(v, "no-store")) {
				t.Errorf("%s: %s %q", what, h, v)
			}
		}
		csp := gooPayCSPDirectives(resp.Header.Get("Content-Security-Policy"))
		for d, srcs := range siteCSP {
			got, ok := csp[d]
			if !ok {
				t.Errorf("%s: the site's directive %s is missing", what, d)
				continue
			}
			for _, s := range srcs {
				if !cspHas(got, s) {
					t.Errorf("%s: %s lost %s", what, d, s)
				}
			}
			for _, s := range got {
				if (s == "'unsafe-inline'" || s == "*" || strings.Contains(s, "*.")) && !cspHas(srcs, s) {
					t.Errorf("%s: %s gained %s", what, d, s)
				}
			}
		}
		if xfo := resp.Header.Get("X-Frame-Options"); xfo != home.Header.Get("X-Frame-Options") {
			t.Errorf("%s: X-Frame-Options %q, the site sends %q", what, xfo, home.Header.Get("X-Frame-Options"))
		}
		if !cspHas(csp["frame-ancestors"], "'self'") && !cspHas(csp["frame-ancestors"], "'none'") && resp.Header.Get("X-Frame-Options") == "" {
			t.Errorf("%s: the page can be framed by other sites", what)
		}
		return strings.Join(csp["frame-src"], " ") + " | " + strings.Join(csp["script-src"], " ") + " | " + strings.Join(csp["form-action"], " ")
	}

	// Embed: the iframe, the provider's script with the nonce, the cancel link back to Pay for order.
	fake.mode = "embed"
	oe, re := gooFakePayCheckout(t, srv, "Page Bowl", 10)
	resp, err := guest.Get(re.Redirect)
	if err != nil {
		t.Fatal(err)
	}
	csp := check("embed", resp, `<iframe id="goo-pay-frame" src="https://frame.fake.test/f?t=1"`, `allow="payment"`, `title="Secure payment form"`,
		`src="https://js.fake.test/sdk.js"`, "iFrameResize", "Complete your payment", "<strong>"+oe.Number()+"</strong>", "Order number:", "images/payment-gateways/",
		"Cancel and choose another payment method", "pay_for_order=true&amp;key="+oe.OrderKey, "never reach this store")
	// frame-src may already cover the frame host (https:); the script host is always named.
	if !(strings.Contains(csp, "https://frame.fake.test") || strings.HasPrefix(csp, "'self' https: |")) || !strings.Contains(csp, "https://js.fake.test") || strings.Contains(csp, "pay.fake.test") {
		t.Errorf("embed CSP: %s", csp)
	}

	// Form: the auto-submitted form, a visible Continue button and a <noscript> hint.
	fake.mode = "form"
	_, rf := gooFakePayCheckout(t, srv, "Page Vase", 10)
	resp, err = guest.Get(rf.Redirect)
	if err != nil {
		t.Fatal(err)
	}
	csp = check("form", resp, `action="https://pay.fake.test/pay"`, `name="merchant_oid"`, "&lt;script&gt;", "Continue to FakePay", "<noscript>",
		"redirected to FakePay&#39;s secure payment page", `document.getElementById("goo-pay-form").submit()`)
	if !strings.Contains(csp, "https://pay.fake.test") || strings.Contains(csp, "frame.fake.test") {
		t.Errorf("form CSP: %s", csp)
	}

	// Wrong or missing key, unknown attempt: 400.
	cb := base + "/wc-api/" + gooFakePayID + "/?pay=go"
	u, _ := url.Parse(rf.Redirect)
	ref := u.Query().Get("a")
	for _, bad := range []string{cb + "&a=" + ref + "&k=wc_order_wrong", cb + "&a=" + ref, cb, cb + "&a=NOPE0000NOPE0000&k=" + u.Query().Get("k")} {
		if code, body, _ := get(t, guest, bad); code != http.StatusBadRequest || body != "-1" {
			t.Errorf("%s: %d %q", bad, code, body)
		}
	}
}

// A stored start that names an unlisted host is refused when the page renders, not only at Start.
func TestGooPayGoPageRefusesUnlistedHost(t *testing.T) {
	_, _, srv := gooSite(t)
	fake, g := gooFakePayRegister(t, srv)
	for _, r := range []*pay.StartResult{
		{Form: &pay.Form{Action: "https://evil.example/pay"}},
		{Form: &pay.Form{Action: "http://pay.fake.test/pay"}},
		{Embed: &pay.Embed{FrameURL: "https://frame.fake.test.evil.example/f"}},
		{Embed: &pay.Embed{FrameURL: "https://frame.fake.test/f", Scripts: []string{"https://cdn.evil.example/x.js"}}},
		{Embed: &pay.Embed{FrameURL: "https://frame.fake.test/f", Init: "x</script><script>alert(1)"}},
		{Redirect: "http://pay.fake.test/r"},
	} {
		if err := g.checkStart(r); err == nil {
			t.Errorf("accepted %+v %+v %q", r.Form, r.Embed, r.Redirect)
		}
	}
	_ = fake
	// Loopback http: only in the paydemo build, and only for a spec that lists http://127.0.0.1.
	if g.checkStart(&pay.StartResult{Form: &pay.Form{Action: "http://127.0.0.1:8080/pay"}}) == nil {
		t.Error("loopback http accepted for a spec that does not list it")
	}
	demo := []string{"http://127.0.0.1"}
	if got := gooPayHostOK("http://127.0.0.1:9/pay", demo); got != gooPayAllowLoopbackHTTP {
		t.Errorf("loopback http with the demo host listed: %v, build allows %v", got, gooPayAllowLoopbackHTTP)
	}
	for _, raw := range []string{"http://localhost:9/pay", "http://127.0.0.2:9/pay", "http://u@127.0.0.1:9/pay", "http://10.0.0.1/pay"} {
		if gooPayHostOK(raw, demo) {
			t.Errorf("%s accepted", raw)
		}
	}
}

// The page speaks the store's language.
func TestGooPayGoPageTurkish(t *testing.T) {
	_, _, srv := gooSite(t)
	ctx := context.Background()
	fake, _ := gooFakePayRegister(t, srv)
	if err := srv.settings.Set(ctx, map[string]any{"WPLANG": "tr_TR"}); err != nil {
		t.Fatal(err)
	}
	srv.lang.Reset()
	guest := gooGuestClient()
	fake.mode = "embed"
	_, re := gooFakePayCheckout(t, srv, "Sayfa Kase", 10)
	_, page, _ := get(t, guest, re.Redirect)
	gooMustContain(t, "Turkish embed page", page, "Ödemenizi tamamlayın", "Sipariş numarası:", "Ödenecek tutar:",
		"İptal et ve başka bir ödeme yöntemi seç", `title="Güvenli ödeme formu"`)
	fake.mode = "form"
	_, rf := gooFakePayCheckout(t, srv, "Sayfa Vazo", 10)
	_, page, _ = get(t, guest, rf.Redirect)
	gooMustContain(t, "Turkish form page", page, "FakePay ile devam et", "güvenli ödeme sayfasına yönlendiriliyorsunuz")
}
