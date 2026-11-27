package server

import (
	"context"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"wpgo/internal/store"
	"wpgo/internal/wp"
)

// Goocommerce › Settings (page=settings&tab=…&section=…, WooCommerce's admin.php?page=wc-settings): the tab
// bar from gooSettingsTabs(), section links, a generic renderer / saver of the declared fields
// (WC_Admin_Settings::output_fields / save_fields) and the special sections: tax rates, shipping zones,
// methods and classes, payment gateways, e-mails, REST API keys and webhooks.

// gooSettingsData is the view of every settings screen; Kind picks the body.
type gooSettingsData struct {
	Tabs     []gooOpt
	Tab      string
	Sections []gooView
	Section  string
	Kind     string // fields, taxrates, zones, zone, classes, gateways, emails, keys, key, webhooks, webhook
	Title    string
	Intro    template.HTML
	Body     template.HTML // rendered fields
	Nonce    string
	Action   string
	HideSave bool
	SaveText string
	BackURL  string
	BackText string

	Tax      *gooTaxGrid
	Zones    []*gooZoneRow
	Zone     *gooZoneEdit
	Classes  []gooClassRow
	Gateways []gooGatewayRow
	Emails   []gooEmailRow
	Keys     []gooKeyRow
	Key      *gooKeyEdit
	Hooks    []gooHookRow
	Hook     *gooHookEdit
}

func (s *Server) gooAdminSettings(c *gin.Context) {
	tabs := gooSettingsTabs()
	tab := c.DefaultQuery("tab", "general")
	var cur *gooTab
	for i := range tabs {
		if tabs[i].ID == tab {
			cur = &tabs[i]
		}
	}
	if cur == nil {
		cur = &tabs[0]
	}
	section := c.Query("section")
	if c.Request.Method == http.MethodPost {
		if !s.checkNonce(c, gooNonce) {
			s.gooDeny(c)
			return
		}
		s.gooSettingsPost(c, cur, section)
		return
	}
	d := s.gooSettingsShell(c, tabs, cur, section)
	s.gooSettingsBody(c, d, cur, section)
	if c.Writer.Written() || c.IsAborted() { // a CSV download
		return
	}
	if d.Kind == "" {
		d.Kind = "fields"
	}
	s.gooStoreRender(c, "goocommerce?page=settings", "wc-settings", gooPlainTitle(cur.Title), "admin/goo-settings", d)
}

// gooPlainTitle turns "Accounts &amp; Privacy" into the page title text.
func gooPlainTitle(v string) string { return strings.ReplaceAll(v, "&amp;", "&") }

func (s *Server) gooSettingsShell(c *gin.Context, tabs []gooTab, cur *gooTab, section string) *gooSettingsData {
	d := &gooSettingsData{Tab: cur.ID, Section: section, Nonce: s.nonce(c, gooNonce), Title: gooPlainTitle(cur.Title), SaveText: "Save changes"}
	d.Action = gooAdminURL("settings", "tab", cur.ID, "section", section)
	for _, t := range tabs {
		d.Tabs = append(d.Tabs, gooOpt{t.ID, t.Title})
	}
	link := func(sec, label string) {
		d.Sections = append(d.Sections, gooView{Label: label, URL: gooAdminURL("settings", "tab", cur.ID, "section", sec), Current: sec == section})
	}
	switch cur.ID {
	case "tax":
		link("", "Tax options")
		for _, cl := range s.gooTaxClasses() {
			link(gooTaxSection(cl.Value), cl.Label+" rates")
		}
	case "checkout", "email":
		// No section links: the gateways / e-mails list links to each one.
	default:
		if len(cur.Sections) > 1 {
			for _, sec := range cur.Sections {
				link(sec.ID, gooPlainTitle(sec.Title))
			}
		}
	}
	return d
}

// gooTaxSection is a tax class's section id: "standard" for the standard class.
func gooTaxSection(class string) string {
	if class == "" {
		return "standard"
	}
	return class
}

// gooSectionFields are the declared fields of a tab's section ("" = the first section).
func gooSectionFields(t *gooTab, section string) []gooField {
	for _, sec := range t.Sections {
		if sec.ID == section {
			return sec.Fields
		}
	}
	if section == "" && len(t.Sections) > 0 {
		return t.Sections[0].Fields
	}
	return nil
}

func (s *Server) gooSettingsBody(c *gin.Context, d *gooSettingsData, t *gooTab, section string) {
	ctx := c.Request.Context()
	switch t.ID {
	case "tax":
		if section != "" {
			s.gooTaxRatesView(c, d, section)
			return
		}
	case "shipping":
		switch {
		case c.Query("instance_id") != "":
			s.gooShippingMethodView(c, d)
			return
		case c.Query("zone_id") != "":
			s.gooZoneView(c, d)
			return
		case section == "":
			s.gooZonesView(c, d)
			return
		case section == "classes":
			s.gooClassesView(c, d)
			return
		}
	case "checkout":
		if section == "" {
			s.gooGatewaysView(c, d)
		} else {
			s.gooGatewayView(c, d, section)
		}
		return
	case "email":
		if section == "" {
			s.gooEmailsView(c, d)
			d.Body = s.gooFieldsHTML(ctx, gooSectionFields(t, ""), gooOptName, s.gooOptValue, nil)
		} else {
			s.gooEmailView(c, d, section)
		}
		return
	case "advanced":
		switch section {
		case "keys":
			s.gooKeysView(c, d)
			return
		case "webhooks":
			s.gooWebhooksView(c, d)
			return
		}
	}
	d.Kind = "fields"
	d.Body = s.gooFieldsHTML(ctx, gooSectionFields(t, section), gooOptName, s.gooOptValue, nil)
}

func (s *Server) gooSettingsPost(c *gin.Context, t *gooTab, section string) {
	ctx := c.Request.Context()
	back := gooAdminURL("settings", "tab", t.ID, "section", section)
	saved := "Your settings have been saved."
	switch t.ID {
	case "tax":
		if section != "" {
			msg, errMsg := s.gooTaxRatesPost(c, section)
			if msg == "" && errMsg == "" {
				return // a CSV export was written
			}
			gooRedirect(c, back, msg, errMsg)
			return
		}
	case "shipping":
		switch {
		case c.Query("instance_id") != "":
			to, msg, errMsg := s.gooShippingMethodPost(c)
			gooRedirect(c, to, msg, errMsg)
			return
		case c.Query("zone_id") != "":
			to, msg, errMsg := s.gooZonePost(c)
			gooRedirect(c, to, msg, errMsg)
			return
		case section == "":
			msg, errMsg := s.gooZonesPost(c)
			gooRedirect(c, back, msg, errMsg)
			return
		case section == "classes":
			msg, errMsg := s.gooClassesPost(c)
			gooRedirect(c, back, msg, errMsg)
			return
		}
	case "checkout":
		if section == "" {
			if errMsg := s.gooGatewaysPost(c); errMsg != "" {
				gooRedirect(c, back, saved, errMsg)
				return
			}
		} else if err := s.gooGatewayPost(c, section); err != nil {
			gooRedirect(c, back, "", err.Error())
			return
		}
		gooRedirect(c, back, saved, "")
		return
	case "email":
		if section != "" {
			if err := s.gooEmailPost(c, section); err != nil {
				gooRedirect(c, back, "", err.Error())
				return
			}
			gooRedirect(c, back, saved, "")
			return
		}
	case "advanced":
		switch section {
		case "keys":
			s.gooKeysPost(c)
			return
		case "webhooks":
			s.gooWebhooksPost(c)
			return
		}
	}
	vals := s.gooReadFields(c, gooSectionFields(t, section), gooOptName)
	if err := s.gooSetOpts(ctx, vals); err != nil {
		gooRedirect(c, back, "", err.Error())
		return
	}
	s.purgeAll(ctx)
	gooRedirect(c, back, saved, "")
}

// Field rendering ---------------------------------------------------------------------------------------------

func gooOptName(id string) string { return "woocommerce_" + id }

// gooOptValue is a store option's current value for the renderer.
func (s *Server) gooOptValue(f gooField) any { return s.gooOpt(f.ID) }

func gooValStr(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case bool:
		if x {
			return "yes"
		}
		return "no"
	}
	return ""
}

func gooValBool(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case string:
		return x == "yes" || x == "1" || x == "true" || x == "on"
	case float64:
		return x != 0
	case int:
		return x != 0
	}
	return false
}

func gooValList(v any) []string {
	switch x := v.(type) {
	case []string:
		return x
	case []any:
		var out []string
		for _, e := range x {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case string:
		if x == "" {
			return nil
		}
		return strings.Split(x, ",")
	}
	return nil
}

func gooAttr(v string) string { return template.HTMLEscapeString(v) }

// gooFieldsHTML is WC_Admin_Settings::output_fields(): form-table rows for the fields. nameOf maps a
// field id to the input name, valueOf reads the current value; custom renders types it knows
// (bacs_accounts…) and returns "" for the rest.
func (s *Server) gooFieldsHTML(ctx context.Context, fields []gooField, nameOf func(string) string, valueOf func(gooField) any,
	custom func(f gooField, name string) template.HTML) template.HTML {
	var b strings.Builder
	open := false // a form-table is open
	inGroup := false
	ensureTable := func() {
		if !open {
			b.WriteString(`<table class="form-table">`)
			open = true
		}
	}
	for _, f := range fields {
		name := nameOf(f.ID)
		tip := ""
		if f.Tip != "" {
			tip = ` <span class="woocommerce-help-tip" tabindex="0" aria-label="` + gooAttr(f.Tip) + `" title="` + gooAttr(f.Tip) + `"></span>`
		}
		desc := ""
		if f.Desc != "" && f.Type != "checkbox" && f.Type != "title" {
			desc = `<p class="description">` + f.Desc + `</p>`
		}
		row := func(input string) {
			ensureTable()
			cls := ""
			if f.Show != "" {
				cls = ` class="goo-show-if" data-show-if="` + gooAttr(nameOf(f.Show)) + `"`
			}
			fmt.Fprintf(&b, `<tr%s><th scope="row" class="titledesc"><label for="%s">%s%s</label></th><td class="forminp forminp-%s">%s%s</td></tr>`,
				cls, gooAttr(name), f.Title, tip, gooAttr(f.Type), input, desc)
		}
		if custom != nil {
			if h := custom(f, name); h != "" {
				row(string(h))
				continue
			}
		}
		val := valueOf(f)
		attrs := ""
		if f.Holder != "" {
			attrs += ` placeholder="` + gooAttr(f.Holder) + `"`
		}
		if f.Min != "" {
			attrs += ` min="` + gooAttr(f.Min) + `"`
		}
		if f.Step != "" {
			attrs += ` step="` + gooAttr(f.Step) + `"`
		}
		switch f.Type {
		case "title":
			if open {
				b.WriteString(`</table>`)
				open = false
			}
			if f.Title != "" {
				fmt.Fprintf(&b, `<h2>%s</h2>`, f.Title)
			}
			if f.Desc != "" {
				fmt.Fprintf(&b, `<div id="%s-description"><p>%s</p></div>`, gooAttr(f.ID), f.Desc)
			}
			b.WriteString(`<table class="form-table">`)
			open = true
		case "sectionend":
			if open {
				b.WriteString(`</table>`)
				open = false
			}
		case "text", "email", "password", "number", "price", "color", "image", "url":
			typ := f.Type
			cls := f.Class
			switch f.Type {
			case "price":
				typ, cls = "text", strings.TrimSpace(cls+" wc_input_price")
			case "color":
				typ, cls = "text", strings.TrimSpace(cls+" colorpick")
			case "image":
				typ = "number"
			}
			input := fmt.Sprintf(`<input name="%s" id="%s" type="%s" style="" value="%s" class="%s"%s>`, gooAttr(name), gooAttr(name), typ, gooAttr(gooValStr(val)), gooAttr(cls), attrs)
			if f.Type == "color" {
				input = `<span class="colorpickpreview" style="background:` + gooAttr(gooValStr(val)) + `">&nbsp;</span>` + input
			}
			row(input)
		case "textarea":
			row(fmt.Sprintf(`<textarea name="%s" id="%s" style="width:400px;height:75px" class="%s"%s>%s</textarea>`, gooAttr(name), gooAttr(name), gooAttr(f.Class), attrs, gooAttr(gooValStr(val))))
		case "select", "single_select_page", "single_select_country":
			var o strings.Builder
			cur := gooValStr(val)
			switch f.Type {
			case "single_select_page":
				cur = strconv.Itoa(int(gooNum(val)))
				o.WriteString(`<option value="">Select a page…</option>`)
				for _, p := range s.gooPagesList(ctx) {
					sel := ""
					if p.Value == cur {
						sel = ` selected="selected"`
					}
					fmt.Fprintf(&o, `<option value="%s"%s>%s</option>`, p.Value, sel, gooAttr(p.Label))
				}
			case "single_select_country":
				o.WriteString(gooCountryStateOptions(cur))
			default:
				for _, op := range f.Options {
					sel := ""
					if op.Value == cur {
						sel = ` selected="selected"`
					}
					fmt.Fprintf(&o, `<option value="%s"%s>%s</option>`, gooAttr(op.Value), sel, op.Label)
				}
			}
			cls := strings.TrimSpace(f.Class + " wc-enhanced-select")
			if f.Type == "single_select_page" {
				cls = "wc-page-search"
			}
			row(fmt.Sprintf(`<select name="%s" id="%s" style="" class="%s">%s</select>`, gooAttr(name), gooAttr(name), gooAttr(cls), o.String()))
		case "multiselect", "multi_select_countries":
			sel := map[string]bool{}
			for _, v := range gooValList(val) {
				sel[v] = true
			}
			opts := f.Options
			if f.Type == "multi_select_countries" {
				opts = nil
				for _, cn := range gooCountryOrder {
					opts = append(opts, gooOpt{cn.Code, cn.Name})
				}
			}
			var o strings.Builder
			for _, op := range opts {
				attr := ""
				if sel[op.Value] {
					attr = ` selected="selected"`
				}
				fmt.Fprintf(&o, `<option value="%s"%s>%s</option>`, gooAttr(op.Value), attr, gooAttr(op.Label))
			}
			extra := ""
			if f.Type == "multi_select_countries" {
				extra = `<br><a class="select_all button" href="#">Select all</a> <a class="select_none button" href="#">Select none</a>`
			}
			row(fmt.Sprintf(`<input type="hidden" name="%s[]" value=""><select multiple="multiple" name="%s[]" id="%s" style="width:350px" data-placeholder="Choose countries / regions…" class="wc-enhanced-select %s" size="8">%s</select>%s`,
				gooAttr(name), gooAttr(name), gooAttr(name), gooAttr(f.Class), o.String(), extra))
		case "radio":
			var o strings.Builder
			cur := gooValStr(val)
			o.WriteString(`<fieldset><ul>`)
			for _, op := range f.Options {
				chk := ""
				if op.Value == cur {
					chk = ` checked="checked"`
				}
				fmt.Fprintf(&o, `<li><label><input name="%s" value="%s" type="radio" class="%s"%s> %s</label></li>`, gooAttr(name), gooAttr(op.Value), gooAttr(f.Class), chk, op.Label)
			}
			o.WriteString(`</ul></fieldset>`)
			row(o.String())
		case "checkbox":
			chk := ""
			if gooValBool(val) {
				chk = ` checked="checked"`
			}
			show := ""
			if f.Show != "" {
				show = ` class="goo-show-if" data-show-if="` + gooAttr(nameOf(f.Show)) + `"`
			}
			box := fmt.Sprintf(`<label for="%s"><input name="%s" id="%s" type="checkbox" class="%s" value="1"%s> %s</label>%s`,
				gooAttr(name), gooAttr(name), gooAttr(name), gooAttr(f.Class), chk, f.Desc, tip)
			if !inGroup {
				ensureTable()
				rowCls := ""
				if f.Show != "" && f.Group == "start" {
					rowCls = ` class="goo-show-if" data-show-if="` + gooAttr(nameOf(f.Show)) + `"`
				}
				fmt.Fprintf(&b, `<tr%s><th scope="row" class="titledesc">%s</th><td class="forminp forminp-checkbox"><fieldset>`, rowCls, f.Title)
				if f.Group == "start" {
					inGroup = true
				}
			}
			if f.Show != "" && !(f.Group == "start") {
				fmt.Fprintf(&b, `<div%s>%s</div>`, show, box)
			} else {
				fmt.Fprintf(&b, `<div>%s</div>`, box)
			}
			if f.Group == "end" || !inGroup {
				b.WriteString(`</fieldset></td></tr>`)
				inGroup = false
			}
		default:
			row(fmt.Sprintf(`<input name="%s" id="%s" type="text" value="%s"%s>`, gooAttr(name), gooAttr(name), gooAttr(gooValStr(val)), attrs))
		}
	}
	if inGroup {
		b.WriteString(`</fieldset></td></tr>`)
	}
	if open {
		b.WriteString(`</table>`)
	}
	return template.HTML(b.String())
}

func gooNum(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int:
		return float64(x)
	case int64:
		return float64(x)
	case string:
		f, _ := strconv.ParseFloat(strings.TrimSpace(x), 64)
		return f
	}
	return 0
}

// gooCountryStateOptions is WC_Countries::country_dropdown_options(): states as optgroups.
func gooCountryStateOptions(cur string) string {
	var b strings.Builder
	for _, cn := range gooCountryOrder {
		if states := gooStates[cn.Code]; len(states) > 0 {
			fmt.Fprintf(&b, `<optgroup label="%s">`, gooAttr(cn.Name))
			for _, st := range states {
				v := cn.Code + ":" + st.Code
				sel := ""
				if v == cur {
					sel = ` selected="selected"`
				}
				fmt.Fprintf(&b, `<option value="%s"%s>%s &mdash; %s</option>`, gooAttr(v), sel, gooAttr(cn.Name), gooAttr(st.Name))
			}
			b.WriteString(`</optgroup>`)
			continue
		}
		sel := ""
		if cn.Code == cur {
			sel = ` selected="selected"`
		}
		fmt.Fprintf(&b, `<option value="%s"%s>%s</option>`, gooAttr(cn.Code), sel, gooAttr(cn.Name))
	}
	return b.String()
}

// gooPagesList are the published pages for single_select_page.
func (s *Server) gooPagesList(ctx context.Context) []gooOpt {
	rows, err := s.store.Pool.Query(ctx, `SELECT id, title FROM posts WHERE type = 'page' AND status IN ('publish','private','draft') ORDER BY title, id LIMIT 500`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []gooOpt
	for rows.Next() {
		var id int64
		var t string
		if rows.Scan(&id, &t) == nil {
			out = append(out, gooOpt{itoa64(id), t + " (ID: " + itoa64(id) + ")"})
		}
	}
	return out
}

// gooReadFields is WC_Admin_Settings::save_fields(): the posted values of the fields, typed for
// gooSetOpts (numbers float64, checkboxes bool, lists []string, the rest strings).
func (s *Server) gooReadFields(c *gin.Context, fields []gooField, nameOf func(string) string) map[string]any {
	out := map[string]any{}
	for _, f := range fields {
		name := nameOf(f.ID)
		switch f.Type {
		case "title", "sectionend":
			continue
		case "checkbox":
			out[f.ID] = c.PostForm(name) != ""
		case "multiselect", "multi_select_countries":
			list := []string{}
			for _, v := range c.PostFormArray(name + "[]") {
				if v != "" {
					list = append(list, v)
				}
			}
			out[f.ID] = list
		case "number", "single_select_page", "image":
			raw := strings.TrimSpace(c.PostForm(name))
			if raw == "" {
				out[f.ID] = gooNum(f.Default)
				continue
			}
			n, err := strconv.ParseFloat(raw, 64)
			if err != nil {
				n = gooNum(f.Default)
			}
			out[f.ID] = n
		case "textarea":
			out[f.ID] = gooKsesTextarea(currentUser(c), strings.ReplaceAll(c.PostForm(name), "\r\n", "\n"))
		default:
			if _, ok := c.GetPostForm(name); !ok {
				continue
			}
			out[f.ID] = strings.TrimSpace(c.PostForm(name))
		}
	}
	return out
}

// gooReadMapFields reads fields saved as a settings map (gateways, shipping methods, e-mails): checkboxes
// "yes"/"no", lists []string, everything else a string — like WC_Settings_API.
func (s *Server) gooReadMapFields(c *gin.Context, fields []gooField, nameOf func(string) string, into map[string]any) {
	for _, f := range fields {
		name := nameOf(f.ID)
		switch f.Type {
		case "title", "sectionend":
		case "checkbox":
			if c.PostForm(name) != "" {
				into[f.ID] = "yes"
			} else {
				into[f.ID] = "no"
			}
		case "multiselect", "multi_select_countries":
			list := []string{}
			for _, v := range c.PostFormArray(name + "[]") {
				if v != "" {
					list = append(list, v)
				}
			}
			into[f.ID] = list
		default:
			if v, ok := c.GetPostForm(name); ok {
				into[f.ID] = strings.TrimSpace(strings.ReplaceAll(v, "\r\n", "\n"))
			}
		}
	}
}

// gooMapValue reads a settings map value with the field's default.
func gooMapValue(m map[string]any) func(gooField) any {
	return func(f gooField) any {
		if v, ok := m[f.ID]; ok {
			return v
		}
		return f.Default
	}
}

// gooKsesTextarea is WC_Admin_Settings' wp_kses_post() for a textarea setting: the store prints
// these messages unescaped on public pages, so markup from a user without unfiltered_html (a shop
// manager) is filtered like post content. Plain text is kept byte for byte.
func gooKsesTextarea(u *store.User, v string) string {
	if !strings.Contains(v, "<") || u.Can("unfiltered_html", nil) {
		return v
	}
	return wp.KsesPost(v)
}
