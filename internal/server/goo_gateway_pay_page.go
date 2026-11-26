package server

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"

	"wpgo/internal/pay"
)

// The ?pay=go page of the hosted-payment bridge: the step between the checkout and the provider for a
// StartResult that is a Form (a bank's 3-D Secure page, auto-posted) or an Embed (a provider iframe
// where the customer types the card). It is rendered inside the store's theme like the checkout, so
// the customer stays on a page of the shop, and its response's Content-Security-Policy is the site's
// own policy plus the provider's listed hosts, nothing else.

// gooPayLoopbackHost is the only host an http URL of a provider may have, and only when
// gooPayAllowLoopbackHTTP is set (the development-only demo provider, build tag paydemo).
const gooPayLoopbackHost = "127.0.0.1"

// gooPayLoopbackOK reports whether u is an http URL on the loopback address and hosts list
// "http://127.0.0.1" (any port), in a build that allows it.
func gooPayLoopbackOK(u *url.URL, hosts []string) bool {
	if !gooPayAllowLoopbackHTTP || u.Scheme != "http" || u.Hostname() != gooPayLoopbackHost {
		return false
	}
	for _, h := range hosts {
		if strings.EqualFold(strings.TrimRight(h, "/"), "http://"+gooPayLoopbackHost) {
			return true
		}
	}
	return false
}

// gooPayHostOK reports whether raw is an https URL whose origin is one of hosts.
func gooPayHostOK(raw string, hosts []string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil {
		return false
	}
	if gooPayLoopbackOK(u, hosts) {
		return true
	}
	if u.Scheme != "https" {
		return false
	}
	origin := "https://" + strings.ToLower(u.Host)
	for _, h := range hosts {
		if strings.EqualFold(strings.TrimRight(h, "/"), origin) {
			return true
		}
	}
	return false
}

// gooPayOrigin is the CSP source of an accepted provider URL: its origin, with the port. Spec hosts
// are added as they are; this adds the loopback origin of the demo provider, whose port is chosen
// when it starts.
func gooPayOrigin(raw string) []string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil
	}
	return []string{u.Scheme + "://" + strings.ToLower(u.Host)}
}

// checkStart refuses a StartResult that is not exactly one https target on the spec's hosts.
func (g *gooPayGateway) checkStart(r *pay.StartResult) error {
	n := 0
	if r.Redirect != "" {
		n++
		u, err := url.Parse(r.Redirect)
		if err != nil || u.Host == "" || (u.Scheme != "https" && !gooPayLoopbackOK(u, g.spec.Hosts.Form)) {
			return errors.New("the provider's redirect is not an https URL")
		}
	}
	if r.Form != nil {
		n++
		if !gooPayHostOK(r.Form.Action, g.spec.Hosts.Form) {
			return errors.New("the provider's form posts to a host that is not listed")
		}
	}
	if r.Embed != nil {
		n++
		if !gooPayHostOK(r.Embed.FrameURL, g.spec.Hosts.Frame) {
			return errors.New("the provider's iframe is on a host that is not listed")
		}
		for _, sc := range r.Embed.Scripts {
			if !gooPayHostOK(sc, g.spec.Hosts.Script) {
				return errors.New("a provider script is on a host that is not listed")
			}
		}
		if strings.Contains(strings.ToLower(r.Embed.Init), "</script") {
			return errors.New("the provider's init script is not valid")
		}
	}
	if n != 1 {
		return errors.New("the provider did not return exactly one way to pay")
	}
	return nil
}

// gooPayTf translates a bridge sentence with one %s and fills it in (plain text, escape the result).
func (s *Server) gooPayTf(ctx context.Context, format, arg string) string {
	return strings.Replace(s.gooPayT(ctx, format), "%s", arg, 1)
}

// gooPayGoPage renders the ?pay=go page of an attempt inside the theme: the order, the amount and the
// payment method, then the auto-submitted form to the provider or the provider's iframe. Only the
// spec's hosts are added to the site's CSP for this response.
func (s *Server) gooPayGoPage(c *gin.Context, g *gooPayGateway, a *gooPayAttempt, o *gooOrder) {
	ctx := c.Request.Context()
	var st gooPayStart
	if len(a.Start) == 0 || json.Unmarshal(a.Start, &st) != nil {
		s.gooPayBadRequest(c)
		return
	}
	res := &pay.StartResult{Form: st.Form, Embed: st.Embed}
	if res.Form == nil && res.Embed == nil {
		s.gooPayBadRequest(c)
		return
	}
	if err := g.checkStart(res); err != nil {
		log.Printf("goocommerce: %s: attempt %s: %v", g.ID(), a.Ref, err)
		s.gooPayBadRequest(c)
		return
	}
	name := g.spec.Title
	if h := s.gooGateway(g.ID()); h != nil {
		if t := strings.TrimSpace(h.Title()); t != "" {
			name = t
		}
	}
	nonce := cspNonceAttr()

	// The provider block (form fields, iframe, scripts) is kept out of the store's text translation:
	// it is the provider's data, not store markup.
	var prov strings.Builder
	kind := "embed"
	if f := st.Form; f != nil {
		kind = "form"
		cspAllow(c, "form-action", g.spec.Hosts.Form...)
		cspAllow(c, "form-action", gooPayOrigin(f.Action)...)
		prov.WriteString(`<form id="goo-pay-form" class="goo-pay-form" method="post" action="` + gooEsc(f.Action) + `">`)
		for _, kv := range f.Fields {
			prov.WriteString(`<input type="hidden" name="` + gooEsc(kv[0]) + `" value="` + gooEsc(kv[1]) + `">`)
		}
	} else {
		e := st.Embed
		cspAllow(c, "frame-src", g.spec.Hosts.Frame...)
		cspAllow(c, "frame-src", gooPayOrigin(e.FrameURL)...)
		cspAllow(c, "script-src", g.spec.Hosts.Script...)
		prov.WriteString(`<iframe id="` + pay.FrameID + `" src="` + gooEsc(e.FrameURL) + `" class="goo-pay-frame" title="` +
			gooEsc(s.gooPayT(ctx, "Secure payment form")) + `" allow="payment"></iframe>`)
		for _, sc := range e.Scripts {
			cspAllow(c, "script-src", gooPayOrigin(sc)...)
			prov.WriteString(`<script` + nonce + ` src="` + gooEsc(sc) + `"></script>`)
		}
		if e.Init != "" {
			prov.WriteString(`<script` + nonce + `>` + e.Init + `</script>`)
		}
	}

	var b strings.Builder
	b.WriteString(`<div class="woocommerce goo-pay-page goo-pay-page--` + kind + `">`)
	b.WriteString(`<ul class="woocommerce-order-overview goo-pay-summary">`)
	b.WriteString(`<li class="goo-pay-summary__order">` + gooEsc(s.gooPayT(ctx, "Order number:")) + ` <strong>` + gooEsc(o.Number()) + `</strong></li>`)
	b.WriteString(`<li class="goo-pay-summary__amount">` + gooEsc(s.gooPayT(ctx, "Amount to pay:")) + ` <strong>` + string(s.gooPriceIn(o.Total, o.Currency)) + `</strong></li>`)
	b.WriteString(`<li class="goo-pay-summary__method">` + gooEsc(s.gooPayT(ctx, "Payment method:")) + ` <strong><img class="goo-gateway-icon" src="` +
		gooEsc(s.gooGatewayLogoURL(ctx, g.ID())) + `" alt="" height="28"> ` + gooEsc(name) + `</strong></li></ul>`)
	if kind == "form" {
		b.WriteString(`<p class="goo-pay-redirect">` + gooEsc(s.gooPayTf(ctx, "You are being redirected to %s's secure payment page…", name)) + `</p>`)
		b.WriteString(s.gooKeep(c, prov.String()))
		b.WriteString(`<noscript><p class="goo-pay-noscript">` + gooEsc(s.gooPayTf(ctx, "JavaScript is turned off in your browser: press “Continue to %s” to go on.", name)) + `</p></noscript>`)
		b.WriteString(`<p><button type="submit" class="button alt goo-pay-continue">` + gooEsc(s.gooPayTf(ctx, "Continue to %s", name)) + `</button></p>`)
		b.WriteString(`</form>`)
		b.WriteString(`<script` + nonce + `>document.getElementById("goo-pay-form").submit();</script>`)
	} else {
		b.WriteString(`<div class="goo-pay-frame-wrap">` + s.gooKeep(c, prov.String()) + `</div>`)
		b.WriteString(`<p class="goo-pay-secure">` + gooEsc(s.gooPayTf(ctx, "Your card details are entered on %s's secure payment form and never reach this store.", name)) + `</p>`)
		b.WriteString(`<p class="goo-pay-cancel"><a href="` + gooEsc(s.gooOrderPayURL(ctx, o)) + `">` + gooEsc(s.gooPayT(ctx, "Cancel and choose another payment method")) + `</a></p>`)
	}
	b.WriteString(`</div>`)

	// The page URL carries the order key: no referrer to the provider or to anything the theme loads,
	// never stored by a cache (HandleAPI sent gooNoStore; the private marker keeps Pursae Cache out
	// even if a theme hook rewrites Cache-Control).
	c.Header("Referrer-Policy", "no-referrer")
	c.Header("X-WPGo-Cache-Control", "no-store")
	c.Header("X-Robots-Tag", "noindex, nofollow")
	post := s.gooSyntheticPost(ctx, 0, s.gooPayT(ctx, "Complete your payment"), "", nil)
	s.gooRenderSynthetic(c, post, b.String(), 1)
}
