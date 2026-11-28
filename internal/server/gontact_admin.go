package server

import (
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"net/mail"
	"net/url"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

// Admin screens of Gontact Form: Contact › Contact Forms (list), Add Contact Form and the editor,
// and Integration. URLs are CF7's: admin?page=wpcf7, admin?page=wpcf7&post=ID&action=edit,
// admin?page=wpcf7-new, admin?page=wpcf7-integration.

// gontactAdmin dispatches the admin?page=wpcf7… screens; ok is false for other pages.
func (s *Server) gontactAdmin(c *gin.Context) bool {
	page := c.Query("page")
	if page != "wpcf7" && page != "wpcf7-new" && page != "wpcf7-integration" && page != gontactEntriesPage {
		return false
	}
	u := currentUser(c)
	if !s.gontactOn() || !u.Can("edit_posts", nil) {
		c.String(http.StatusForbidden, "Sorry, you are not allowed to access this page.")
		return true
	}
	switch {
	case page == gontactEntriesPage:
		s.gontactEntriesScreen(c)
	case page == "wpcf7-integration":
		s.gontactIntegration(c)
	case page == "wpcf7-new":
		if !u.Can("publish_pages", nil) {
			c.String(http.StatusForbidden, "Sorry, you are not allowed to access this page.")
			return true
		}
		if c.Request.Method == http.MethodPost {
			s.gontactSaveScreen(c, nil)
			return true
		}
		f := s.gontactDefaultForm()
		f.AuthorID = u.ID
		s.gontactEditor(c, f, true)
	default:
		s.gontactFormsScreen(c)
	}
	return true
}

func (s *Server) gontactFormsScreen(c *gin.Context) {
	u := currentUser(c)
	action := c.Query("action")
	if c.Request.Method == http.MethodPost {
		action = c.PostForm("action")
		if action == "-1" || action == "" {
			action = c.PostForm("action2")
		}
	}
	id, _ := strconv.ParseInt(c.DefaultQuery("post", c.PostForm("post_ID")), 10, 64)
	switch action {
	case "save":
		f, err := s.gontactForm(c, id)
		if err != nil || !u.Can("publish_pages", nil) {
			c.String(http.StatusForbidden, "You are not allowed to edit this item.")
			return
		}
		s.gontactSaveScreen(c, f)
		return
	case "copy":
		f, err := s.gontactForm(c, id)
		if err != nil || !u.Can("publish_pages", nil) || !s.checkNonce(c, "wpcf7-copy-contact-form_"+itoa64(id)) {
			s.denyNonce(c)
			return
		}
		cp := *f
		cp.ID, cp.Hash, cp.AuthorID = 0, "", u.ID
		cp.Title = f.Title + "_copy"
		cp.Messages = map[string]string{}
		for k, v := range f.Messages {
			cp.Messages[k] = v
		}
		if err := s.gontactSave(c, &cp, u.ID); err != nil {
			c.Redirect(http.StatusFound, gontactAdminURL("wpcf7", url.Values{"message": {"failed"}}))
			return
		}
		c.Redirect(http.StatusFound, gontactAdminURL("wpcf7", url.Values{"post": {itoa64(cp.ID)}, "action": {"edit"}, "message": {"created"}}))
		return
	case "delete":
		var ids []int64
		nonceOK := false
		if c.Request.Method == http.MethodPost && len(c.PostFormArray("post[]")) > 0 {
			for _, v := range c.PostFormArray("post[]") {
				if n, err := strconv.ParseInt(v, 10, 64); err == nil {
					ids = append(ids, n)
				}
			}
			nonceOK = s.checkNonce(c, "bulk-posts")
		} else {
			ids = []int64{id}
			nonceOK = s.checkNonce(c, "wpcf7-delete-contact-form_"+itoa64(id))
		}
		if !nonceOK || !u.Can("publish_pages", nil) {
			s.denyNonce(c)
			return
		}
		for _, pid := range ids {
			if _, err := s.gontactForm(c, pid); err == nil {
				_ = s.store.DeletePost(c, pid)
			}
		}
		s.purgeAll(c)
		c.Redirect(http.StatusFound, gontactAdminURL("wpcf7", url.Values{"message": {"deleted"}}))
		return
	case "edit":
		f, err := s.gontactForm(c, id)
		if err != nil {
			c.String(http.StatusNotFound, "You attempted to edit an item that does not exist. Perhaps it was deleted?")
			return
		}
		// The editor shows the form's mail and webhook settings (webhook_secret among them): it needs the
		// capability that saving needs, like CF7's wpcf7_edit_contact_form (publish_pages).
		if !u.Can("publish_pages", nil) {
			c.String(http.StatusForbidden, "You are not allowed to edit this item.")
			return
		}
		s.gontactEditor(c, f, false)
		return
	}
	s.gontactList(c)
}

// List table -------------------------------------------------------------------------

type gfListRow struct {
	ID                                      int64
	Title, Shortcode, Author, Date, ISODate string
	EditURL, CopyURL, DeleteURL             string
	CanEdit                                 bool
}

type gfListData struct {
	Rows       []gfListRow
	Search     string
	CanCreate  bool
	BulkNonce  string
	Pagination pagination
	Orderby    string
	Order      string
	TitleSort  sortLinkData
	DateSort   sortLinkData
	ItemsLabel string
	Total      int
	ColCount   int
}

type sortLinkData struct{ URL, Class, AriaSort string }

func (s *Server) gontactList(c *gin.Context) {
	u := currentUser(c)
	search := strings.TrimSpace(c.Query("s"))
	orderby, order := c.DefaultQuery("orderby", "title"), strings.ToLower(c.DefaultQuery("order", "asc"))
	if order != "desc" {
		order = "asc"
	}
	page, _ := strconv.Atoi(c.DefaultQuery("paged", "1"))
	if page < 1 {
		page = 1
	}
	const perPage = 20
	forms, total, _ := s.gontactForms(c, search, orderby, order, page, perPage)
	d := gfListData{Search: search, CanCreate: u.Can("publish_pages", nil), BulkNonce: s.nonce(c, "bulk-posts"), Orderby: orderby, Order: order, Total: total, ColCount: 5}
	base := url.URL{Path: "/wp-admin/admin", RawQuery: url.Values{"page": {"wpcf7"}, "s": {search}, "orderby": {orderby}, "order": {order}}.Encode()}
	d.Pagination = newPagination(total, perPage, page, base)
	sortLink := func(col string) sortLinkData {
		cur := orderby == col
		next := "asc"
		if cur && order == "asc" {
			next = "desc"
		}
		l := sortLinkData{URL: gontactAdminURL("wpcf7", url.Values{"orderby": {col}, "order": {next}, "s": {search}})}
		if cur {
			l.Class, l.AriaSort = "sorted "+order, map[string]string{"asc": "ascending", "desc": "descending"}[order]
		} else {
			l.Class = "sortable desc"
		}
		return l
	}
	d.TitleSort, d.DateSort = sortLink("title"), sortLink("date")
	names := map[int64]string{}
	for _, f := range forms {
		row := gfListRow{ID: f.ID, Title: f.Title, Shortcode: f.Shortcode(), CanEdit: u.Can("publish_pages", nil),
			Date: f.Date.In(s.loc()).Format("2006/01/02 at 3:04 pm"), ISODate: f.Date.Format("2006-01-02T15:04:05Z07:00")}
		row.Date = f.Date.In(s.loc()).Format("2006/01/02") + " at " + f.Date.In(s.loc()).Format("3:04 pm")
		if n, ok := names[f.AuthorID]; ok {
			row.Author = n
		} else if a, err := s.store.UserByID(c, f.AuthorID); err == nil {
			names[f.AuthorID], row.Author = a.DisplayName, a.DisplayName
		}
		id := itoa64(f.ID)
		row.EditURL = gontactAdminURL("wpcf7", url.Values{"post": {id}, "action": {"edit"}})
		row.CopyURL = gontactAdminURL("wpcf7", url.Values{"post": {id}, "action": {"copy"}, "_wpnonce": {s.nonce(c, "wpcf7-copy-contact-form_"+id)}})
		row.DeleteURL = gontactAdminURL("wpcf7", url.Values{"post": {id}, "action": {"delete"}, "_wpnonce": {s.nonce(c, "wpcf7-delete-contact-form_"+id)}})
		d.Rows = append(d.Rows, row)
	}
	s.adminPage(c, "admin?page=wpcf7", "Contact Forms", "toplevel_page_wpcf7", "admin/gontact-list", d, s.gontactNotices(c)...)
}

func (s *Server) gontactNotices(c *gin.Context) []notice {
	var extra []notice
	if c.Query("page") == "wpcf7" {
		if n := s.gontactFailedMails(c); n > 0 {
			extra = append(extra, notice{Type: "warning", Message: template.HTML(fmt.Sprintf("%d form submission(s) in the last 7 days could not be delivered by e-mail. See <a href=\"%s\">Entries</a>.", n, gontactAdminURL(gontactEntriesPage, url.Values{"mail_status": {"failed"}})))})
		}
	}
	msg := map[string]string{"created": "Contact form created.", "saved": "Contact form saved.", "deleted": "Contact form deleted.",
		"failed": "There was an error saving the contact form.", "integration": "Settings saved."}[c.Query("message")]
	if msg == "" {
		return extra
	}
	typ := "success"
	if c.Query("message") == "failed" {
		typ = "error"
	}
	return append(extra, notice{Type: typ, Message: template.HTML(template.HTMLEscapeString(msg)), Dismissible: true})
}

// Editor -------------------------------------------------------------------------------

type gfMessageRow struct{ Key, Description, Value string }

type gfEditorData struct {
	Form          *gfForm
	New           bool
	Action        string
	Nonce         string
	CopyURL       string
	DeleteURL     string
	Shortcode     string
	OldShortcode  string
	MailTags      []string
	Messages      []gfMessageRow
	Warnings      map[string][]string // panel → configuration errors
	WarningCount  int
	ActiveTab     string
	SettingsCount int
	TagGenerators []gfTagGen
}

type gfTagGen struct{ Type, Label string }

var gfTagGenerators = []gfTagGen{{"text", "text"}, {"email", "email"}, {"url", "URL"}, {"tel", "tel"}, {"number", "number"},
	{"date", "date"}, {"textarea", "text area"}, {"select", "drop-down menu"}, {"checkbox", "checkboxes"}, {"radio", "radio buttons"},
	{"acceptance", "acceptance"}, {"quiz", "quiz"},
	{"captchac", "CAPTCHA (image)"}, {"captchar", "CAPTCHA (answer)"}, {"file", "file"}, {"submit", "submit"}}

func (s *Server) gontactEditor(c *gin.Context, f *gfForm, isNew bool) {
	d := gfEditorData{Form: f, New: isNew, ActiveTab: c.Query("active-tab"), TagGenerators: gfTagGenerators}
	if isNew {
		d.Action = gontactAdminURL("wpcf7-new", nil)
		d.Nonce = s.nonce(c, "wpcf7-save-contact-form_-1")
	} else {
		id := itoa64(f.ID)
		d.Action = gontactAdminURL("wpcf7", url.Values{"post": {id}})
		d.Nonce = s.nonce(c, "wpcf7-save-contact-form_"+id)
		d.CopyURL = gontactAdminURL("wpcf7", url.Values{"post": {id}, "action": {"copy"}, "_wpnonce": {s.nonce(c, "wpcf7-copy-contact-form_"+id)}})
		d.DeleteURL = gontactAdminURL("wpcf7", url.Values{"post": {id}, "action": {"delete"}, "_wpnonce": {s.nonce(c, "wpcf7-delete-contact-form_"+id)}})
		d.Shortcode = f.Shortcode()
		d.OldShortcode = `[contact-form-7 id="` + id + `" title="` + strings.ReplaceAll(f.Title, `"`, "") + `"]`
	}
	_, tags := gfScan(f.Form, nil)
	for _, t := range tags {
		if t.Name != "" && t.BaseType != "count" && t.BaseType != "captchac" && t.BaseType != "captchar" && !gfContains(d.MailTags, "["+t.Name+"]") {
			d.MailTags = append(d.MailTags, "["+t.Name+"]")
		}
	}
	for _, m := range gfMessageDefs {
		d.Messages = append(d.Messages, gfMessageRow{Key: m.Key, Description: m.Description, Value: f.message(m.Key)})
	}
	for _, line := range strings.Split(f.Additional, "\n") {
		if gfSettingRe.MatchString(strings.TrimSpace(line)) {
			d.SettingsCount++
		}
	}
	if !isNew {
		d.Warnings = s.gontactValidateConfig(f)
		for _, w := range d.Warnings {
			d.WarningCount += len(w)
		}
	}
	title := "Edit Contact Form"
	screen := "admin?page=wpcf7"
	if isNew {
		title, screen = "Add Contact Form", "admin?page=wpcf7-new"
	}
	s.adminPage(c, screen, title, "contact_page_wpcf7-new toplevel_page_wpcf7", "admin/gontact-edit", d, s.gontactNotices(c)...)
}

// gontactSaveScreen saves the editor's POST (wpcf7_save_contact_form()); f is nil for a new form.
func (s *Server) gontactSaveScreen(c *gin.Context, f *gfForm) {
	u := currentUser(c)
	nonceID := "-1"
	if f != nil {
		nonceID = itoa64(f.ID)
	}
	if !s.checkNonce(c, "wpcf7-save-contact-form_"+nonceID) {
		s.denyNonce(c)
		return
	}
	isNew := f == nil
	if isNew {
		f = s.gontactDefaultForm()
		f.AuthorID = u.ID
	}
	if c.PostForm("wpcf7-copy") != "" && !isNew {
		c.Redirect(http.StatusFound, gontactAdminURL("wpcf7", url.Values{"post": {itoa64(f.ID)}, "action": {"copy"}, "_wpnonce": {s.nonce(c, "wpcf7-copy-contact-form_"+itoa64(f.ID))}}))
		return
	}
	f.Title = strings.TrimSpace(c.PostForm("post_title"))
	if loc := c.PostForm("wpcf7-locale"); loc != "" {
		f.Locale = loc
	}
	f.Form = strings.ReplaceAll(c.PostForm("wpcf7-form"), "\r\n", "\n")
	readMail := func(prefix string) gfMail {
		g := func(k string) string { return strings.ReplaceAll(c.PostForm(prefix+"["+k+"]"), "\r\n", "\n") }
		return gfMail{Active: g("active") != "", Subject: g("subject"), Sender: g("sender"), Recipient: g("recipient"), Body: g("body"),
			AdditionalHeaders: g("additional_headers"), Attachments: g("attachments"), UseHTML: g("use_html") != "", ExcludeBlank: g("exclude_blank") != ""}
	}
	f.Mail, f.Mail2 = readMail("wpcf7-mail"), readMail("wpcf7-mail-2")
	f.Mail.Active = true
	for _, m := range gfMessageDefs {
		if v, ok := c.GetPostForm("wpcf7-messages[" + m.Key + "]"); ok {
			f.Messages[m.Key] = v
		}
	}
	f.Additional = strings.ReplaceAll(c.PostForm("wpcf7-additional-settings"), "\r\n", "\n")
	if err := s.gontactSave(c, f, u.ID); err != nil {
		c.Redirect(http.StatusFound, gontactAdminURL("wpcf7", url.Values{"message": {"failed"}}))
		return
	}
	s.purgeAll(c)
	msg := "saved"
	if isNew {
		msg = "created"
	}
	q := url.Values{"post": {itoa64(f.ID)}, "action": {"edit"}, "message": {msg}}
	if tab := c.PostForm("active-tab"); tab != "" {
		q.Set("active-tab", tab)
	}
	c.Redirect(http.StatusFound, gontactAdminURL("wpcf7", q))
}

// gontactValidateConfig is a subset of WPCF7_ConfigValidator: the mistakes that keep mail from arriving.
func (s *Server) gontactValidateConfig(f *gfForm) map[string][]string {
	out := map[string][]string{}
	_, tags := gfScan(f.Form, nil)
	names := map[string]bool{}
	files := map[string]bool{}
	for _, t := range tags {
		names[t.Name] = true
		if t.BaseType == "file" {
			files[t.Name] = true
		}
	}
	check := func(panel string, m gfMail) {
		for _, field := range []struct{ label, v string }{{"To", m.Recipient}, {"From", m.Sender}, {"Subject", m.Subject},
			{"Additional headers", m.AdditionalHeaders}, {"Message body", m.Body}} {
			for _, g := range gfMailTagRe.FindAllStringSubmatch(field.v, -1) {
				n := strings.TrimPrefix(strings.TrimPrefix(g[2], "_raw_"), "_format_")
				if g[1] == "[" || strings.HasPrefix(n, "_") || names[n] {
					continue
				}
				out[panel] = append(out[panel], field.label+": there is an invalid mail-tag ["+g[2]+"].")
			}
		}
		if strings.TrimSpace(m.Recipient) == "" {
			out[panel] = append(out[panel], "To: this field is required.")
		} else if !strings.Contains(m.Recipient, "[") {
			for _, a := range strings.Split(m.Recipient, ",") {
				if _, err := mail.ParseAddress(strings.TrimSpace(a)); err != nil {
					out[panel] = append(out[panel], "To: invalid mailbox syntax is used.")
					break
				}
			}
		}
		if strings.TrimSpace(m.Sender) == "" {
			out[panel] = append(out[panel], "From: this field is required.")
		} else if !strings.Contains(strings.Split(m.Sender, "<")[len(strings.Split(m.Sender, "<"))-1], "[") {
			sender := strings.Replace(m.Sender, "[_site_title]", s.settings.String("blogname"), -1)
			if _, err := mail.ParseAddress(sender); err != nil {
				out[panel] = append(out[panel], "From: invalid mailbox syntax is used.")
			}
		}
		for _, h := range strings.Split(m.AdditionalHeaders, "\n") {
			if h = strings.TrimSpace(h); h != "" && !strings.Contains(h, ":") {
				out[panel] = append(out[panel], "Additional headers: invalid mail header is used.")
			}
		}
		for _, line := range strings.Split(m.Attachments, "\n") {
			for _, g := range gfMailTagRe.FindAllStringSubmatch(line, -1) {
				if !files[g[2]] {
					out[panel] = append(out[panel], "File attachments: ["+g[2]+"] is not a file upload field.")
				}
			}
		}
	}
	check("mail", f.Mail)
	if f.Mail2.Active {
		check("mail_2", f.Mail2)
	}
	for _, line := range strings.Split(f.Additional, "\n") {
		if line = strings.TrimSpace(line); line != "" && gfSettingRe.FindStringSubmatch(line) == nil {
			out["additional_settings"] = append(out["additional_settings"], "Invalid setting syntax: "+line)
		}
	}
	return out
}

// Integration --------------------------------------------------------------------------

type gfIntegrationData struct {
	Nonce                            string
	RecaptchaSite, RecaptchaSecret   string
	TurnstileSite, TurnstileSecret   string
	RecaptchaActive, TurnstileActive bool
	Service                          string
	CanManage                        bool
}

func (s *Server) gontactIntegration(c *gin.Context) {
	u := currentUser(c)
	if !u.Can("manage_options", nil) {
		c.String(http.StatusForbidden, "Sorry, you are not allowed to access this page.")
		return
	}
	if c.Request.Method == http.MethodPost {
		if !s.checkNonce(c, "wpcf7-integration") {
			s.denyNonce(c)
			return
		}
		vals := map[string]any{}
		switch c.PostForm("service") {
		case "recaptcha":
			vals["gontact_recaptcha_sitekey"], vals["gontact_recaptcha_secret"] = strings.TrimSpace(c.PostForm("sitekey")), s.sealSecret(strings.TrimSpace(c.PostForm("secret")))
			if c.PostForm("reset") != "" {
				vals["gontact_recaptcha_sitekey"], vals["gontact_recaptcha_secret"] = "", ""
			}
		case "turnstile":
			vals["gontact_turnstile_sitekey"], vals["gontact_turnstile_secret"] = strings.TrimSpace(c.PostForm("sitekey")), s.sealSecret(strings.TrimSpace(c.PostForm("secret")))
			if c.PostForm("reset") != "" {
				vals["gontact_turnstile_sitekey"], vals["gontact_turnstile_secret"] = "", ""
			}
		}
		if len(vals) > 0 {
			_ = s.settings.Set(c, vals)
			s.purgeAll(c)
		}
		c.Redirect(http.StatusFound, gontactAdminURL("wpcf7-integration", url.Values{"message": {"integration"}}))
		return
	}
	d := gfIntegrationData{Nonce: s.nonce(c, "wpcf7-integration"), Service: c.Query("service"), CanManage: true,
		RecaptchaSite: s.settings.String("gontact_recaptcha_sitekey"), TurnstileSite: s.settings.String("gontact_turnstile_sitekey")}
	d.RecaptchaActive = d.RecaptchaSite != "" && s.settings.String("gontact_recaptcha_secret") != ""
	d.TurnstileActive = d.TurnstileSite != "" && s.settings.String("gontact_turnstile_secret") != ""
	if d.RecaptchaActive {
		d.RecaptchaSecret = gfMask(s.revealSecret(s.settings.String("gontact_recaptcha_secret")))
	}
	if d.TurnstileActive {
		d.TurnstileSecret = gfMask(s.revealSecret(s.settings.String("gontact_turnstile_secret")))
	}
	s.adminPage(c, "admin?page=wpcf7-integration", "Integration with External API", "contact_page_wpcf7-integration", "admin/gontact-integration", d, s.gontactNotices(c)...)
}

// gfMask shows a secret as CF7 does: the first and last four characters.
func gfMask(v string) string {
	if len(v) <= 8 {
		return strings.Repeat("*", len(v))
	}
	return v[:4] + strings.Repeat("*", len(v)-8) + v[len(v)-4:]
}

// Gotenberg block ----------------------------------------------------------------------

// gontactEditorAssets registers the contact-form-7/contact-form-selector block in the block editor
// while the plugin is on.
func (s *Server) gontactEditorAssets(c *gin.Context, a *assets) {
	if !s.gontactOn() {
		return
	}
	forms, _, _ := s.gontactForms(c, "", "title", "asc", 1, -1)
	type item struct {
		ID    int64  `json:"id"`
		Hash  string `json:"hash"`
		Title string `json:"title"`
	}
	list := []item{}
	for _, f := range forms {
		list = append(list, item{f.ID, f.ShortHash(), f.Title})
	}
	raw, _ := json.Marshal(map[string]any{"contactForms": list, "adminURL": "/wp-admin/admin?page=wpcf7"})
	addBodyHTML(c, template.HTML(`<script`+cspNonceAttr()+`>window.wpcf7 = Object.assign(window.wpcf7 || {}, `+string(raw)+`);</script>`+
		`<script`+cspNonceAttr()+` src="`+gontactAssets+`/block.js?ver=`+gontactVersion+`" defer></script>`))
}
