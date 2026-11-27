package server

import (
	"context"
	"errors"
	"fmt"
	"html"
	"html/template"
	"regexp"
	"sort"
	"strings"

	"wpgo/internal/pay/all"

	"github.com/gin-gonic/gin"
)

// Settings › Payments (tab=checkout): the gateways table (drag order saved to gateway_order, enabled
// toggles) and each gateway's settings (section=<id>) from its FormFields, saved as "<id>_settings".
// Settings › Emails (tab=email): the e-mails list and each e-mail's settings (section=<id>) saved as
// "email_<id>_settings", plus the sender / template options.

type gooGatewayRow struct {
	ID, Title, MethodTitle, Desc string
	Enabled                      bool
	URL                          string
}

// gooGatewayFeatureLabels are the feature labels of a method card ("query" is internal and not shown).
var gooGatewayFeatureLabels = []gooOpt{{"3ds", "3D Secure"}, {"installments", "Installments"}, {"refunds", "Refunds"},
	{"subscriptions", "Subscriptions"}, {"saved_cards", "Saved cards"}}

// gooGatewayStatus is the one status badge of a method: its CSS class, label and the setup problem, if any.
func (s *Server) gooGatewayStatus(g gooGatewayDef, enabled bool) (class, label, problem string) {
	problem = s.gooGatewaySetupProblem(g)
	switch {
	case !enabled:
		return "disabled", "Disabled", ""
	case problem != "":
		return "setup", "Setup incomplete", problem
	case s.gooGatewayTestMode(g.ID()):
		return "test", "Test mode", ""
	}
	return "live", "Live", ""
}

// gooGatewaysDefaultRegion is the tab Settings › Payments opens on: Türkiye for a Turkish store.
func (s *Server) gooGatewaysDefaultRegion() string {
	if strings.EqualFold(s.gooBaseCountry(), "TR") || strings.EqualFold(s.gooCurrency(), "TRY") {
		return gooRegionTR
	}
	return gooRegionGlobal
}

// gooGatewayOrderRank is the fixed place of a method inside its group: the built-in ones first, then the
// hosted-payment providers in the order of all.Specs() (PayTR, iyzico, Sipay, the banks, PayPal).
func gooGatewayOrderRank() map[string]int {
	rank := map[string]int{"bacs": 1, "cheque": 2, "cod": 3, "goo_test": 4, "goo_stripe": 5}
	for i, sp := range all.Specs() {
		rank["goo_"+sp.ID] = 100 + i
	}
	return rank
}

// gooGatewayCardOrder sorts the cards of one group: enabled methods first, then the fixed rank, then the id.
func gooGatewayCardOrder(gws []*gooGatewayHandle) []*gooGatewayHandle {
	rank := gooGatewayOrderRank()
	at := func(id string) int {
		if r, ok := rank[id]; ok {
			return r
		}
		return 10000
	}
	out := append([]*gooGatewayHandle(nil), gws...)
	sort.SliceStable(out, func(i, j int) bool {
		ei, ej := out[i].Enabled(), out[j].Enabled()
		if ei != ej {
			return ei
		}
		ri, rj := at(out[i].ID()), at(out[j].ID())
		if ri != rj {
			return ri < rj
		}
		return out[i].ID() < out[j].ID()
	})
	return out
}

// gooGatewayLogoBox is the fixed-size white box a method logo sits in, so wide wordmarks and square marks
// both fit without stretching. size is "", "sm" or "lg". alt is empty where the name is printed next to it.
func (s *Server) gooGatewayLogoBox(ctx context.Context, id, size, alt string) string {
	cls := "goo-gw-logobox"
	if size != "" {
		cls += " goo-gw-logobox-" + size
	}
	return `<span class="` + cls + `"><img class="goo-gw-logo" src="` + gooAttr(s.gooGatewayLogoURL(ctx, id)) + `" alt="` + gooAttr(alt) + `" loading="lazy"></span>`
}

// gooGatewayStatusHTML is the status label: a coloured dot and the text (colour is never the only signal).
func gooGatewayStatusHTML(class, label, problem string) string {
	t := ""
	if problem != "" {
		t = ` title="` + gooAttr(problem) + `"`
	}
	return `<span class="goo-gw-badge goo-gw-badge-` + class + `"` + t + `><span class="goo-gw-dot" aria-hidden="true"></span>` + label + `</span>`
}

// gooGatewayIsBeta reports whether a method still carries the Beta chip.
func gooGatewayIsBeta(info gooGatewayInfo) bool {
	return !info.Verified && (info.Region == gooRegionTR || info.Region == gooRegionGlobal)
}

// gooGatewayCardHTML is one method card: logo, status and toggle on top, name and description, then the
// feature chips and the button.
func (s *Server) gooGatewayCardHTML(ctx context.Context, g *gooGatewayHandle) string {
	info := gooGatewayInfoOf(g)
	enabled := g.Enabled()
	class, label, problem := s.gooGatewayStatus(g, enabled)
	url := gooAdminURL("settings", "tab", "checkout", "section", g.ID())
	name := g.Title()
	if g.Title() != g.MethodTitle() {
		name += " " + g.MethodTitle()
	}
	cls := "goo-gw-card goo-gw-" + class
	if enabled {
		cls += " goo-gw-on"
	}
	var b strings.Builder
	b.WriteString(`<article class="` + cls + `" data-gateway_id="` + gooAttr(g.ID()) + `" data-name="` + gooAttr(name) + `">`)
	b.WriteString(`<div class="goo-gw-bar">` + s.gooGatewayLogoBox(ctx, g.ID(), "", "") + `<div class="goo-gw-side">` + gooGatewayStatusHTML(class, label, problem))
	b.WriteString(`<label class="goo-toggle"><input type="checkbox" name="gateway_enabled[` + gooAttr(g.ID()) + `]" value="1"`)
	if enabled {
		b.WriteString(` checked="checked"`)
	}
	b.WriteString(` aria-label="Enable ` + gooAttr(g.Title()) + `"><span class="woocommerce-input-toggle woocommerce-input-toggle--` + map[bool]string{true: "enabled", false: "disabled"}[enabled] + `">`)
	b.WriteString(map[bool]string{true: "Yes", false: "No"}[enabled] + `</span></label></div></div>`)
	b.WriteString(`<div class="goo-gw-ident"><h4 class="goo-gw-name"><a href="` + gooAttr(url) + `" class="wc-payment-gateway-method-title">` + gooEsc(g.Title()) + `</a>`)
	if g.Title() != g.MethodTitle() {
		b.WriteString(`<span class="wc-payment-gateway-method-name">&nbsp;&ndash; ` + gooEsc(g.MethodTitle()) + `</span>`)
	}
	b.WriteString(`</h4>`)
	if d := g.MethodDescription(); d != "" {
		b.WriteString(`<p class="goo-gw-desc" title="` + gooAttr(d) + `">` + gooEsc(d) + `</p>`)
	}
	if problem != "" {
		b.WriteString(`<p class="goo-gw-problem">` + gooEsc(problem) + `</p>`)
	}
	b.WriteString(`</div><div class="goo-gw-foot"><div class="goo-gw-chips">`)
	if gooGatewayIsBeta(info) {
		b.WriteString(`<span class="goo-gw-chip goo-gw-badge-beta">Beta</span>`)
	}
	for _, f := range gooGatewayFeatureLabels {
		if containsStr(info.Features, f.Value) {
			b.WriteString(`<span class="goo-gw-chip goo-gw-feature">` + f.Label + `</span>`)
		}
	}
	b.WriteString(`</div>`)
	if enabled && problem == "" {
		b.WriteString(`<a class="button goo-gw-action" href="` + gooAttr(url) + `">Manage</a>`)
	} else {
		b.WriteString(`<a class="button button-primary goo-gw-action" href="` + gooAttr(url) + `">Set up</a>`)
	}
	b.WriteString(`</div></article>`)
	return b.String()
}

// gooGatewaysPanelHTML is Settings › Payments: the summary, the region tabs with their method cards and a
// filter, then the checkout order of the enabled methods.
func (s *Server) gooGatewaysPanelHTML(ctx context.Context) string {
	gws := s.gooGatewaysOrdered()
	type section struct {
		title string
		gws   []*gooGatewayHandle
	}
	group := func(match func(gooGatewayInfo) bool) []*gooGatewayHandle {
		var out []*gooGatewayHandle
		for _, g := range gws {
			if match(gooGatewayInfoOf(g)) {
				out = append(out, g)
			}
		}
		return gooGatewayCardOrder(out)
	}
	regions := []struct {
		id, title string
		sections  []section
	}{
		{gooRegionTR, "T\u00fcrkiye", []section{
			{"Payment providers", group(func(i gooGatewayInfo) bool { return i.Region == gooRegionTR && i.Kind != "bank" })},
			{"Bank virtual POS", group(func(i gooGatewayInfo) bool { return i.Region == gooRegionTR && i.Kind == "bank" })}}},
		{gooRegionGlobal, "Worldwide", []section{{"", group(func(i gooGatewayInfo) bool { return i.Region == gooRegionGlobal })}}},
		{gooRegionOffline, "Offline", []section{{"", group(func(i gooGatewayInfo) bool { return i.Region == gooRegionOffline })},
			{"Testing", group(func(i gooGatewayInfo) bool { return i.Region == gooRegionTest })}}},
	}
	def := s.gooGatewaysDefaultRegion()
	total := 0
	for _, g := range gws {
		if g.Enabled() {
			total++
		}
	}
	var b strings.Builder
	b.WriteString(`<input type="hidden" name="gateway_toggles" value="1"><div class="wc_gateways goo-gw" data-default="` + def + `">`)
	b.WriteString(`<p class="goo-gw-summary" aria-live="polite">` + gooGatewaySummaryHTML(total) + `</p>`)
	b.WriteString(`<div class="goo-gw-tabs" role="tablist" aria-label="Payment method regions">`)
	for _, r := range regions {
		on, n := 0, 0
		for _, sec := range r.sections {
			for _, g := range sec.gws {
				n++
				if g.Enabled() {
					on++
				}
			}
		}
		b.WriteString(`<button type="button" role="tab" class="goo-gw-tab" id="goo-gw-tab-` + r.id + `" aria-controls="goo-gw-panel-` + r.id + `" data-region="` + r.id + `" aria-selected="` + map[bool]string{true: "true", false: "false"}[r.id == def] + `">` +
			`<span class="goo-gw-tab-label">` + r.title + `</span> <span class="goo-gw-count" data-count="` + fmt.Sprint(on) + `/` + fmt.Sprint(n) + `">` + fmt.Sprint(on) + `/` + fmt.Sprint(n) + `</span></button>`)
	}
	b.WriteString(`</div>`)
	b.WriteString(`<div class="goo-gw-filterbar"><input type="search" class="goo-gw-filter" placeholder="Filter payment methods&hellip;" aria-label="Filter payment methods" autocomplete="off"></div>`)
	for _, r := range regions {
		b.WriteString(`<section class="goo-gw-region" role="tabpanel" id="goo-gw-panel-` + r.id + `" aria-labelledby="goo-gw-tab-` + r.id + `" data-region="` + r.id + `"><h2 class="goo-gw-region-title">` + r.title + `</h2>`)
		n := 0
		for _, sec := range r.sections {
			if len(sec.gws) == 0 {
				continue
			}
			n += len(sec.gws)
			b.WriteString(`<div class="goo-gw-sec">`)
			if sec.title != "" {
				b.WriteString(`<h3 class="goo-gw-section-title">` + sec.title + `</h3>`)
			}
			b.WriteString(`<div class="goo-gw-grid">`)
			for _, g := range sec.gws {
				b.WriteString(s.gooGatewayCardHTML(ctx, g))
			}
			b.WriteString(`</div></div>`)
		}
		if n == 0 {
			b.WriteString(`<p class="goo-gw-empty">No payment methods in this region yet.</p>`)
		}
		b.WriteString(`<p class="goo-gw-nomatch goo-gw-empty" hidden>No payment method matches</p></section>`)
	}
	b.WriteString(`<h2 class="goo-gw-order-title">Checkout order</h2><p class="description">Enabled methods in the order shoppers see them. Drag a row or use the arrows; a method you just switched on appears here after you save.</p><ul class="goo-gw-order">`)
	enabled := 0
	for _, g := range gws {
		if !g.Enabled() {
			continue
		}
		enabled++
		class, label, problem := s.gooGatewayStatus(g, true)
		b.WriteString(`<li data-gateway_id="` + gooAttr(g.ID()) + `" draggable="true"><div class="wc-item-reorder-nav"><button type="button" class="wc-move-up" aria-label="Move ` + gooAttr(g.Title()) + ` up">&#9650;</button><button type="button" class="wc-move-down" aria-label="Move ` + gooAttr(g.Title()) + ` down">&#9660;</button></div>` +
			s.gooGatewayLogoBox(ctx, g.ID(), "sm", "") + `<span class="goo-gw-order-name">` + gooEsc(g.Title()) + `</span>` + gooGatewayStatusHTML(class, label, problem) +
			`<input type="hidden" name="gateway_order[]" value="` + gooAttr(g.ID()) + `"></li>`)
	}
	b.WriteString(`<li class="goo-gw-empty goo-gw-order-empty"` + map[bool]string{true: ` hidden`, false: ``}[enabled > 0] + `>No payment method is enabled.</li>`)
	b.WriteString(`</ul></div>`)
	return b.String()
}

// gooGatewaySummaryHTML is the one-line count at the top of Settings › Payments.
func gooGatewaySummaryHTML(n int) string {
	switch n {
	case 0:
		return `<strong>0</strong> payment methods enabled <span class="goo-gw-hint">Switch a method on below to start accepting payments.</span>`
	case 1:
		return `<strong>1</strong> payment method enabled`
	}
	return `<strong>` + fmt.Sprint(n) + `</strong> payment methods enabled`
}

func (s *Server) gooGatewaysView(c *gin.Context, d *gooSettingsData) {
	d.Kind, d.Title = "gateways", "Payment methods"
	d.Intro = `<p>Payment methods are grouped by region. Enable the ones you accept; the enabled methods can be sorted under Checkout order.</p>`
	d.Body = template.HTML(s.gooGatewaysPanelHTML(c.Request.Context()))
	for _, g := range s.gooGatewaysOrdered() {
		d.Gateways = append(d.Gateways, gooGatewayRow{ID: g.ID(), Title: g.Title(), MethodTitle: g.MethodTitle(), Desc: g.MethodDescription(),
			Enabled: g.Enabled(), URL: gooAdminURL("settings", "tab", "checkout", "section", g.ID())})
	}
}

// gooGatewaysPost saves the checkout order and the enabled toggles. It returns an error message for the
// admin when a method was left disabled because its setup is incomplete.
func (s *Server) gooGatewaysPost(c *gin.Context) (errMsg string) {
	ctx := c.Request.Context()
	order := []string{}
	for _, id := range c.PostFormArray("gateway_order[]") {
		if gooGatewayRegistry[id] != nil && !containsStr(order, id) {
			order = append(order, id)
		}
	}
	vals := map[string]any{}
	if len(order) > 0 { // methods that were not posted (disabled ones) keep their saved place after the posted ones
		for _, g := range s.gooGatewaysOrdered() {
			if !containsStr(order, g.ID()) {
				order = append(order, g.ID())
			}
		}
		vals["gateway_order"] = order
	}
	if c.PostForm("gateway_toggles") != "" {
		var refused []string
		for _, g := range s.gooGatewaysOrdered() {
			st := s.gooOptMap(g.ID() + "_settings")
			en := "no"
			if c.PostForm("gateway_enabled["+g.ID()+"]") != "" {
				en = "yes"
				if s.gooGatewaySetupProblem(g) != "" {
					en = "no"
					if gooValStr(st["enabled"]) != "yes" { // already enabled stays as it is
						refused = append(refused, g.MethodTitle())
					} else {
						en = "yes"
					}
				}
			}
			if gooValStr(st["enabled"]) != en {
				cp := map[string]any{}
				for k, v := range st {
					cp[k] = v
				}
				cp["enabled"] = en
				vals[g.ID()+"_settings"] = cp
			}
		}
		if len(refused) > 0 {
			errMsg = "Not enabled, setup is incomplete: " + strings.Join(refused, ", ") + "."
		}
	}
	if len(vals) > 0 {
		_ = s.gooSetOpts(ctx, vals)
	}
	return errMsg
}

// gooBACSAccountsHTML is the BACS "Account details" table (wc_bacs_accounts).
func (s *Server) gooBACSAccountsHTML(v any) template.HTML {
	list, _ := v.([]any)
	var b strings.Builder
	b.WriteString(`<table class="widefat wc_input_table sortable" cellspacing="0"><thead><tr><th class="sort">&nbsp;</th>
		<th>Account name</th><th>Account number</th><th>Bank name</th><th>Sort code</th><th>IBAN</th><th>BIC / Swift</th></tr></thead><tbody class="accounts">`)
	keys := []string{"account_name", "account_number", "bank_name", "sort_code", "iban", "bic"}
	row := func(m map[string]any) {
		b.WriteString(`<tr class="account"><td class="sort"></td>`)
		for _, k := range keys {
			v, _ := m[k].(string)
			fmt.Fprintf(&b, `<td><input type="text" value="%s" name="bacs_%s[]"></td>`, gooAttr(v), k)
		}
		b.WriteString(`</tr>`)
	}
	for _, a := range list {
		if m, ok := a.(map[string]any); ok {
			row(m)
		}
	}
	row(map[string]any{}) // a blank row to add an account
	b.WriteString(`</tbody><tfoot><tr><th colspan="7"><a href="#" class="add button">+ Add account</a> <a href="#" class="remove_rows button">Remove selected account(s)</a></th></tr></tfoot></table>`)
	return template.HTML(b.String())
}

// gooCODMethodOptions are the shipping methods COD can be limited to: method types and their instances.
func (s *Server) gooCODMethodOptions(ctx context.Context) []gooOpt {
	var out []gooOpt
	zones, _ := s.gooZones(ctx, true)
	for _, def := range s.gooShippingMethodOrder() {
		out = append(out, gooOpt{def.ID, def.Title})
		for _, z := range zones {
			for _, m := range z.Methods {
				if m.MethodID == def.ID {
					out = append(out, gooOpt{def.ID + ":" + itoa64(m.InstanceID), fmt.Sprintf("%s – %s (#%d)", z.Name, m.Title(), m.InstanceID)})
				}
			}
		}
	}
	return out
}

func (s *Server) gooGatewayFields(ctx context.Context, g gooGatewayDef) []gooField {
	fields := g.FormFields()
	for i := range fields {
		if g.ID() == "cod" && fields[i].ID == "enable_for_methods" {
			fields[i].Options = s.gooCODMethodOptions(ctx)
		}
	}
	if p, ok := g.(gooGatewayFieldsPatcher); ok {
		fields = p.PatchFields(s, ctx, fields)
	}
	return fields
}

func (s *Server) gooGatewayView(c *gin.Context, d *gooSettingsData, id string) {
	ctx := c.Request.Context()
	d.Kind = "fields"
	g := gooGatewayRegistry[id]
	if g == nil {
		d.Body, d.HideSave = `<p>Invalid payment method.</p>`, true
		return
	}
	d.Title = g.MethodTitle()
	d.BackURL, d.BackText = gooAdminURL("settings", "tab", "checkout"), "Payment methods"
	d.Intro = template.HTML(s.gooGatewayHeaderHTML(ctx, s.gooGateway(id)))
	prefix := "woocommerce_" + id + "_"
	values := s.gooGatewaySettings(id)
	fields := s.gooGatewayFields(ctx, g)
	// Secrets (API keys) are never written back into the page: a saved one shows as a placeholder.
	shown := map[string]any{}
	for k, v := range values {
		shown[k] = v
	}
	for i, f := range fields {
		if f.Type == "password" && gooValStr(values[f.ID]) != "" {
			shown[f.ID] = ""
			fields[i].Holder = "Saved – leave empty to keep"
		}
	}
	values = shown
	body := s.gooFieldsHTML(ctx, fields, func(f string) string { return prefix + f }, gooMapValue(values),
		func(f gooField, name string) template.HTML {
			if f.Type == "bacs_accounts" {
				return s.gooBACSAccountsHTML(values["accounts"])
			}
			return ""
		})
	d.Body = gooGatewayNotifyCopyable(body)
}

// gooGatewayHeaderHTML is the top of a method's settings page: the big logo, the name with its status and
// Beta chip, the description, the documentation link and the notices.
func (s *Server) gooGatewayHeaderHTML(ctx context.Context, g *gooGatewayHandle) string {
	info := gooGatewayInfoOf(g)
	class, label, _ := s.gooGatewayStatus(g, g.Enabled())
	problem := s.gooGatewaySetupProblem(g)
	var b strings.Builder
	b.WriteString(`<div class="goo-gw-hero">` + s.gooGatewayLogoBox(ctx, g.ID(), "lg", "") + `<div class="goo-gw-hero-text"><h2>` + gooEsc(g.MethodTitle()) + `</h2>`)
	b.WriteString(`<p class="goo-gw-hero-badges">` + gooGatewayStatusHTML(class, label, ""))
	if gooGatewayIsBeta(info) {
		b.WriteString(`<span class="goo-gw-chip goo-gw-badge-beta">Beta</span>`)
	}
	b.WriteString(`</p>`)
	if d := g.MethodDescription(); d != "" {
		b.WriteString(`<p class="goo-gw-hero-desc">` + gooEsc(d) + `</p>`)
	}
	if info.DocsURL != "" {
		b.WriteString(`<p><a class="goo-gw-docs" href="` + gooAttr(info.DocsURL) + `" target="_blank" rel="noopener noreferrer">Documentation</a></p>`)
	}
	b.WriteString(`</div></div>`)
	if gooGatewayIsBeta(info) {
		b.WriteString(`<div class="notice notice-info inline goo-gw-notice"><p>This method has not yet been verified against a live merchant account. Test it in test mode before going live.</p></div>`)
	}
	if problem != "" {
		b.WriteString(`<div class="notice notice-warning inline goo-gw-notice"><p><strong>Setup incomplete:</strong> ` + gooEsc(problem) + `</p></div>`)
	}
	return b.String()
}

var gooNotifyRow = regexp.MustCompile(`(?s)<div id="notify_url-description"><p>(.*?)<code>(.*?)</code>(.*?)</p></div>`)

// gooGatewayNotifyCopyable turns the notification URL description of a hosted-payment method into a
// read-only box with a Copy button (store.js handles the click). The input has no name: it is not saved.
func gooGatewayNotifyCopyable(body template.HTML) template.HTML {
	out := gooNotifyRow.ReplaceAllStringFunc(string(body), func(m string) string {
		p := gooNotifyRow.FindStringSubmatch(m)
		return `<div id="notify_url-description" class="goo-gw-notify"><p>` + p[1] + strings.TrimSpace(p[3]) + `</p><div class="goo-gw-copyrow">` +
			`<input type="text" readonly class="goo-gw-copyinput" value="` + gooAttr(html.UnescapeString(p[2])) + `" aria-label="Notification URL">` +
			`<button type="button" class="button goo-gw-copy">Copy</button></div></div>`
	})
	return template.HTML(out)
}

func (s *Server) gooGatewayPost(c *gin.Context, id string) error {
	ctx := c.Request.Context()
	g := gooGatewayRegistry[id]
	if g == nil {
		return errors.New("Invalid payment method.")
	}
	st := map[string]any{}
	for k, v := range s.gooOptMap(id + "_settings") {
		st[k] = v
	}
	prefix := "woocommerce_" + id + "_"
	var plain []gooField
	for _, f := range s.gooGatewayFields(ctx, g) {
		if f.Type == "bacs_accounts" {
			names := c.PostFormArray("bacs_account_name[]")
			numbers := c.PostFormArray("bacs_account_number[]")
			banks := c.PostFormArray("bacs_bank_name[]")
			sorts := c.PostFormArray("bacs_sort_code[]")
			ibans := c.PostFormArray("bacs_iban[]")
			bics := c.PostFormArray("bacs_bic[]")
			at := func(l []string, i int) string {
				if i < len(l) {
					return strings.TrimSpace(l[i])
				}
				return ""
			}
			accounts := []any{}
			for i := range names {
				a := map[string]any{"account_name": at(names, i), "account_number": at(numbers, i), "bank_name": at(banks, i),
					"sort_code": at(sorts, i), "iban": at(ibans, i), "bic": at(bics, i)}
				empty := true
				for _, v := range a {
					if v != "" {
						empty = false
					}
				}
				if !empty {
					accounts = append(accounts, a)
				}
			}
			st["accounts"] = accounts
			continue
		}
		plain = append(plain, f)
	}
	secrets := map[string]any{}
	for _, f := range plain {
		if f.Type == "password" {
			secrets[f.ID] = st[f.ID]
		}
	}
	s.gooReadMapFields(c, plain, func(f string) string { return prefix + f }, st)
	for k, old := range secrets { // an empty secret field keeps the saved secret (it is not shown in the form)
		if gooValStr(st[k]) == "" && gooValStr(old) != "" {
			st[k] = old
		}
	}
	// A bad installment plan or minimum keeps its saved value; the rest of the form is saved.
	instErr := s.gooPayCheckInstallmentSettings(g, st, s.gooOptMap(id+"_settings"))
	wasOn := gooValStr(s.gooOptMap(id + "_settings")["enabled"]) == "yes"
	if err := s.gooSetOpts(ctx, map[string]any{id + "_settings": st}); err != nil {
		return err
	}
	// A method cannot be enabled while its setup is incomplete (checked after saving: the credentials may
	// have been entered in this same form).
	if gooValStr(st["enabled"]) == "yes" && !wasOn {
		if problem := s.gooGatewaySetupProblem(g); problem != "" {
			st["enabled"] = "no"
			if err := s.gooSetOpts(ctx, map[string]any{id + "_settings": st}); err != nil {
				return err
			}
			return errors.New(s.gooL10nf(ctx, "Settings saved, but %s was not enabled: %s", g.MethodTitle(), problem))
		}
	}
	if instErr != nil {
		return errors.New(s.gooL10nf(ctx, "Settings saved, except: %s", instErr.Error()))
	}
	return nil
}

// E-mails ------------------------------------------------------------------------------------------------------

type gooEmailRow struct {
	ID, Title, Desc, Type, Recipient, URL string
	Enabled, Manual                       bool
}

var gooEmailTypes = []gooOpt{{"plain", "Plain text"}, {"html", "HTML"}, {"multipart", "Multipart"}}

func (s *Server) gooVisibleEmails() []*gooEmailDef {
	var out []*gooEmailDef
	for _, d := range gooEmailDefs {
		if d.Module == "" || s.gooExtOn(d.Module) {
			out = append(out, d)
		}
	}
	return out
}

func (s *Server) gooEmailsView(c *gin.Context, d *gooSettingsData) {
	d.Kind, d.Title = "emails", "Emails"
	d.Intro = `<p>Email notifications sent from Pursae Commerce are listed below. Click on an email to configure it.</p>`
	for _, e := range s.gooVisibleEmails() {
		st := s.gooEmailSettings(e.ID)
		r := gooEmailRow{ID: e.ID, Title: e.Title, Desc: e.Description, Enabled: st["enabled"] == "yes", Manual: e.Manual,
			URL: gooAdminURL("settings", "tab", "email", "section", e.ID)}
		for _, t := range gooEmailTypes {
			if t.Value == st["email_type"] {
				r.Type = t.Label
			}
		}
		if e.Customer {
			r.Recipient = "Customer"
		} else {
			r.Recipient = st["recipient"]
		}
		d.Emails = append(d.Emails, r)
	}
}

func gooEmailFields(e *gooEmailDef) []gooField {
	ph := "Available placeholders: <code>{site_title}</code>, <code>{site_address}</code>, <code>{site_url}</code>, <code>{store_email}</code>, <code>{order_date}</code>, <code>{order_number}</code>"
	fields := []gooField{{ID: "email_options", Type: "title"}}
	if !e.Manual {
		fields = append(fields, gooField{ID: "enabled", Title: "Enable/Disable", Type: "checkbox", Default: "yes", Desc: "Enable this email notification"})
	}
	if !e.Customer {
		fields = append(fields, gooField{ID: "recipient", Title: "Recipient(s)", Type: "text", Holder: "", Desc: "Enter recipients (comma separated) for this email."})
	}
	fields = append(fields,
		gooField{ID: "subject", Title: "Subject", Type: "text", Holder: e.Subject, Desc: ph, Class: "large-text"},
		gooField{ID: "heading", Title: "Email heading", Type: "text", Holder: e.Heading, Desc: ph, Class: "large-text"},
		gooField{ID: "additional_content", Title: "Additional content", Type: "textarea", Holder: "N/A", Desc: "Text to appear below the main email content. " + ph},
		gooField{ID: "email_type", Title: "Email type", Type: "select", Default: "html", Options: gooEmailTypes, Desc: "Choose which format of email to send.", Class: "email_type"},
		gooField{ID: "email_options_end", Type: "sectionend"})
	return fields
}

func (s *Server) gooEmailView(c *gin.Context, d *gooSettingsData, id string) {
	d.Kind = "fields"
	e := gooEmailDefByID(id)
	if e == nil {
		d.Body, d.HideSave = `<p>Invalid email.</p>`, true
		return
	}
	d.Title = e.Title
	d.BackURL, d.BackText = gooAdminURL("settings", "tab", "email"), "Emails"
	d.Intro = template.HTML(`<h2>` + gooEsc(e.Title) + `</h2><p>` + gooEsc(e.Description) + `</p>`)
	st := s.gooEmailSettings(id)
	vals := map[string]any{}
	for k, v := range st {
		vals[k] = v
	}
	prefix := "woocommerce_" + id + "_"
	d.Body = s.gooFieldsHTML(c.Request.Context(), gooEmailFields(e), func(f string) string { return prefix + f }, gooMapValue(vals), nil)
}

func (s *Server) gooEmailPost(c *gin.Context, id string) error {
	e := gooEmailDefByID(id)
	if e == nil {
		return errors.New("Invalid email.")
	}
	st := map[string]any{}
	for k, v := range s.gooOptMap("email_" + id + "_settings") {
		st[k] = v
	}
	prefix := "woocommerce_" + id + "_"
	s.gooReadMapFields(c, gooEmailFields(e), func(f string) string { return prefix + f }, st)
	return s.gooSetOpts(c.Request.Context(), map[string]any{"email_" + id + "_settings": st})
}
