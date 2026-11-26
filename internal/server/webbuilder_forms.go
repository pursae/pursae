package server

import (
	"context"
	"crypto/hmac"
	"encoding/json"
	"errors"
	"html"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"wpgo/internal/i18n"
	"wpgo/internal/mail"
	"wpgo/internal/store"
	"wpgo/internal/webbuilder"
)

// Pursae Web Builder: the public form endpoint POST web-builder/v2/forms/{post}/{section}
// (docs/developer/web-builder-contract.md §3.5). A `form` section of a published builder page posts
// here, with fetch and Accept: application/json from the site runtime (→ JSON) or as a plain form
// post without JavaScript (→ 303 back to the page).
//
// Nothing in the request is trusted: the fields come from the stored doc (webbuilder.FindForm), the
// values are validated against that schema and every other field is dropped. A filled honeypot, a
// too-fast wb_t, or a disallowed word (Settings › Discussion) is spam: the visitor sees
// "sent" and nothing is mailed. Each address gets wbFormLimit attempts per wbFormWindow. When
// Pursae Contact Form is active with reCAPTCHA or Turnstile keys, the matching token is required.
// The message goes to admin_email through the core mailer (the wp_mail job).
//
// POST requires a same-origin request and wb_nonce. Logged-in visitors get a session nonce
// (wbFormToken); everyone else gets a signed token of the post and section. A cookie without
// X-WP-Nonce is still a visitor to the REST API (restAuthUser).

const (
	wbFormLimit   = 10                                  // attempts per address and window
	wbFormWindow  = 10 * time.Minute                    // fixed window of the per-address limit
	wbFormSentMsg = "Thank you, your message was sent." // default texts; shown translated (wbFormMessage)
	wbFormBadMsg  = "Please check the highlighted fields."
	wbFormFailMsg = "Sorry, the message could not be sent."
	wbFormRateMsg = "Too many messages from this address. Please try again later."
	wbFormTitle   = 200 // runes of the mail subject's title part
)

// registerWebBuilderForms adds the public form route (the handler checks wbOn on every request).
func (s *Server) registerWebBuilderForms() {
	s.rest.register(webbuilder.RESTNamespace, `/web-builder/v2/forms/(?P<id>[\d]+)/(?P<section>[A-Za-z0-9_-]{1,64})`,
		map[string]restHandler{http.MethodPost: s.wbRESTForm}, nil)
}

// --- rate limit --------------------------------------------------------------------------------

// wbFormLimiter is a fixed-window counter per address, one per Server.
type wbFormLimiter struct {
	mu    sync.Mutex
	hits  map[string]*wbFormHits
	swept time.Time
}

type wbFormHits struct {
	start time.Time
	n     int
}

var wbFormLimiters sync.Map // *Server → *wbFormLimiter

func (s *Server) wbFormLimiter() *wbFormLimiter {
	if v, ok := wbFormLimiters.Load(s); ok {
		return v.(*wbFormLimiter)
	}
	v, _ := wbFormLimiters.LoadOrStore(s, &wbFormLimiter{hits: map[string]*wbFormHits{}})
	return v.(*wbFormLimiter)
}

// allow counts one attempt of key; when the limit is spent it returns false and the time until the
// window ends.
func (l *wbFormLimiter) allow(key string, now time.Time) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.swept) > wbFormWindow {
		for k, h := range l.hits {
			if now.Sub(h.start) >= wbFormWindow {
				delete(l.hits, k)
			}
		}
		l.swept = now
	}
	h := l.hits[key]
	if h == nil || now.Sub(h.start) >= wbFormWindow {
		h = &wbFormHits{start: now}
		l.hits[key] = h
	}
	h.n++
	if h.n > wbFormLimit {
		return false, h.start.Add(wbFormWindow).Sub(now)
	}
	return true, 0
}

// wbFormIPKey is the limiter key of an address: IPv4 as is, IPv6 by its /64 (one host's range).
func wbFormIPKey(addr string) string {
	ip := net.ParseIP(strings.TrimSpace(addr))
	if ip == nil {
		return addr
	}
	if v4 := ip.To4(); v4 != nil {
		return v4.String()
	}
	return ip.Mask(net.CIDRMask(64, 128)).String()
}

// --- handler -----------------------------------------------------------------------------------

// wbFormOutcome is how a submit ended.
type wbFormOutcome int

const (
	wbFormSent wbFormOutcome = iota
	wbFormInvalid
	wbFormFailed
)

// wbRESTForm handles POST web-builder/v2/forms/{id}/{section}.
func (s *Server) wbRESTForm(c *gin.Context, r *restRequest) {
	if !s.wbOn() {
		restErr(c, http.StatusNotFound, "rest_no_route", "No route was found matching the URL and request method.")
		return
	}
	c.Header("Cache-Control", "no-store")
	wantJSON := strings.Contains(strings.ToLower(c.GetHeader("Accept")), "application/json")

	// Count the attempt before any lookup, so probing for forms costs the same as posting to one.
	if ok, wait := s.wbFormLimiter().allow(wbFormIPKey(clientIP(c)), time.Now()); !ok {
		secs := int((wait + time.Second - 1) / time.Second)
		if secs < 1 {
			secs = 1
		}
		c.Header("Retry-After", strconv.Itoa(secs))
		restErr(c, http.StatusTooManyRequests, webbuilder.ErrRateLimited, s.frontT(c.Request.Context(), wbFormRateMsg),
			map[string]any{"retryAfter": secs})
		return
	}

	p, schema, ok := s.wbFormTarget(c, r)
	if !ok {
		return
	}
	values, ok := wbFormValues(c)
	if !ok {
		return
	}
	finish := func(o wbFormOutcome, fields map[string]string) {
		s.wbFormRespond(c, p, schema, wantJSON, o, fields)
	}

	if !s.wbFormOriginOK(c) || !s.wbFormTokenOK(c, p.ID, schema.SectionID, values[webbuilder.FormNonce]) {
		restErr(c, http.StatusForbidden, webbuilder.ErrInvalidNonce, "The form token is missing or has expired.")
		return
	}
	if wbFormSpam(values) || s.wbFormDisallowed(values, clientIP(c), c.Request.UserAgent()) {
		finish(wbFormSent, nil) // spam looks like a success and is not mailed
		return
	}
	clean, errs := schema.Validate(values)
	if len(errs) > 0 {
		finish(wbFormInvalid, schema.InputErrors(errs))
		return
	}
	if reason := s.wbFormCaptcha(c.Request.Context(), values, clientIP(c)); reason != "" {
		finish(wbFormInvalid, map[string]string{"captcha": reason})
		return
	}
	ctx := c.Request.Context()
	link := s.wbFormPageURL(c, p)
	// Keep the message first: a mail that cannot be queued must not lose it.
	var stored *store.WebBuilderSubmission
	if s.wbFormStoreOn() {
		stored = &store.WebBuilderSubmission{PostID: p.ID, SectionID: schema.SectionID, FormTitle: wbFormTitleOf(p, schema),
			PageURL: link, Fields: clean, IP: clientIP(c), EmailFields: wbFormEmailFields(schema)}
		if u := currentUser(c); u != nil {
			stored.UserID = u.ID
		}
		if err := s.store.WebBuilder().AddSubmission(ctx, stored); err != nil {
			slog.Error("web builder form: store", "post", p.ID, "section", schema.SectionID, "err", err)
			stored = nil
		}
	}
	if err := s.wbFormMail(ctx, p, link, schema, clean, clientIP(c)); err != nil {
		slog.Error("web builder form: mail", "post", p.ID, "section", schema.SectionID, "err", err)
		if stored == nil {
			finish(wbFormFailed, nil)
			return
		}
		// Stored but not mailed: the visitor's message is safe in Settings › Web Builder.
		finish(wbFormSent, nil)
		return
	}
	if stored != nil {
		_ = s.store.WebBuilder().SetSubmissionMailed(ctx, stored.ID, true)
	}
	finish(wbFormSent, nil)
}

// wbFormEmailFields are the names of the form's e-mail fields: type email (a section form's email,
// a code form's type="email" input or the input bound to email) and email, which the notification
// answers to (Reply-To). Personal data export and erasure match the address on them only.
func wbFormEmailFields(schema webbuilder.FormSchema) []string {
	out := []string{}
	for _, f := range schema.Fields {
		if (f.Type == "email" || f.Name == "email") && !slices.Contains(out, f.Name) {
			out = append(out, f.Name)
		}
	}
	return out
}

// wbFormTarget loads the published page and the stored schema of the {section} form. Anything that
// is not a visible form section of a published, non-detached builder page or of a published
// Theme Builder template (contract §11.9) answers 404.
func (s *Server) wbFormTarget(c *gin.Context, r *restRequest) (*store.Post, webbuilder.FormSchema, bool) {
	notFound := func() (*store.Post, webbuilder.FormSchema, bool) {
		restErr(c, http.StatusNotFound, webbuilder.ErrNotFound, "Invalid form.")
		return nil, webbuilder.FormSchema{}, false
	}
	p, err := s.store.GetPost(c, r.ID())
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return notFound()
		}
		restErr(c, http.StatusInternalServerError, "db_error", "Could not read the post.")
		return nil, webbuilder.FormSchema{}, false
	}
	if !s.wbEnabledType(p.Type) && !wbIsTemplate(p) {
		restErr(c, http.StatusNotFound, webbuilder.ErrNotBuilder, "Pursae Web Builder is not enabled for this post type.")
		return nil, webbuilder.FormSchema{}, false
	}
	if p.Status != "publish" || s.passwordRequired(c, p) {
		return notFound()
	}
	row, err := s.store.WebBuilder().Doc(c, p.ID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return notFound()
		}
		restErr(c, http.StatusInternalServerError, "db_error", "Could not read the page.")
		return nil, webbuilder.FormSchema{}, false
	}
	// A detached page (content edited outside the builder, §6.3) shows the theme fallback: its
	// forms are not on the page any more. Templates have no core editor and never detach.
	if !wbIsTemplate(p) && (row.Detached || (row.ContentHash != "" && row.ContentHash != store.WebBuilderContentHash(p.Content))) {
		return notFound()
	}
	var doc webbuilder.DocState
	if err := json.Unmarshal(row.Doc, &doc); err != nil {
		slog.Warn("web builder form: stored doc", "post", p.ID, "err", err)
		return notFound()
	}
	schema, ok := webbuilder.FindForm(&doc, r.Params["section"])
	if !ok {
		return notFound()
	}
	return p, schema, true
}

// wbFormValues reads the posted fields: application/x-www-form-urlencoded or multipart/form-data
// (files are ignored), at most webbuilder.MaxFormBodyBytes. The first value of each name wins.
func wbFormValues(c *gin.Context) (map[string]string, bool) {
	limit := int64(webbuilder.MaxFormBodyBytes)
	if c.Request.ContentLength > limit {
		restErr(c, http.StatusRequestEntityTooLarge, webbuilder.ErrTooLarge, "The request body is too large.", map[string]any{"limit": limit})
		return nil, false
	}
	mt, _, _ := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if mt != "application/x-www-form-urlencoded" && mt != "multipart/form-data" {
		restErr(c, http.StatusUnsupportedMediaType, "rest_invalid_param", "Send the form as application/x-www-form-urlencoded or multipart/form-data.")
		return nil, false
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, limit)
	var form url.Values
	if mt == "multipart/form-data" {
		if err := c.Request.ParseMultipartForm(limit); err != nil {
			return wbFormBodyErr(c, err)
		}
		if c.Request.MultipartForm != nil {
			_ = c.Request.MultipartForm.RemoveAll()
			form = c.Request.MultipartForm.Value
		}
	} else {
		if err := c.Request.ParseForm(); err != nil {
			return wbFormBodyErr(c, err)
		}
		form = c.Request.PostForm
	}
	out := make(map[string]string, len(form))
	for k, v := range form {
		if len(v) > 0 {
			out[k] = v[0]
		}
	}
	return out, true
}

func wbFormBodyErr(c *gin.Context, err error) (map[string]string, bool) {
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		restErr(c, http.StatusRequestEntityTooLarge, webbuilder.ErrTooLarge, "The request body is too large.",
			map[string]any{"limit": webbuilder.MaxFormBodyBytes})
	} else {
		restErr(c, http.StatusBadRequest, "rest_invalid_param", "The form data could not be read.")
	}
	return nil, false
}

// wbFormSpam: a filled honeypot, or a timing the runtime measured below webbuilder.FormMinMillis.
// A missing timing (a plain post without JavaScript) is not spam by itself.
func wbFormSpam(values map[string]string) bool {
	if strings.TrimSpace(values[webbuilder.FormHoneypot]) != "" {
		return true
	}
	if t := strings.TrimSpace(values[webbuilder.FormTiming]); t != "" {
		ms, err := strconv.ParseInt(t, 10, 64)
		if err != nil || ms < webbuilder.FormMinMillis {
			return true
		}
	}
	return false
}

// wbFormSignMsg is the signed-token message for an anonymous form post.
func wbFormSignMsg(postID int64, section string) string {
	return "wb-form|" + strconv.FormatInt(postID, 10) + "|" + section
}

// wbFormNonceAction is the session nonce action for a logged-in form post.
func wbFormNonceAction(postID int64, section string) string {
	return "wb-form:" + strconv.FormatInt(postID, 10) + ":" + section
}

// wbFormToken is the hidden wb_nonce value for this visitor: a session nonce when someone is
// logged in, otherwise a stable signature of the post and section (safe to cache).
func (s *Server) wbFormToken(c *gin.Context, postID int64, section string) string {
	if currentUser(c) != nil {
		return "n." + s.nonce(c, wbFormNonceAction(postID, section))
	}
	return "s." + s.auth.Sign(wbFormSignMsg(postID, section))
}

// wbFormTokenOK reports whether tok was issued for this visitor and this form.
func (s *Server) wbFormTokenOK(c *gin.Context, postID int64, section, tok string) bool {
	kind, val, ok := strings.Cut(tok, ".")
	if !ok || val == "" {
		return false
	}
	switch kind {
	case "s":
		// A visitor who logged in after the page loaded still holds the anonymous token.
		return hmac.Equal([]byte(s.auth.Sign(wbFormSignMsg(postID, section))), []byte(val))
	case "n":
		return s.checkNonceValue(c, val, wbFormNonceAction(postID, section))
	default:
		return false
	}
}

// wbFormOriginOK requires the POST to come from this site. A cross-site form carries the other
// site's Origin; a same-site post carries this site's Origin or Referer.
func (s *Server) wbFormOriginOK(c *gin.Context) bool {
	home := s.links(c).HomeURL("/")
	hu, err := url.Parse(home)
	if err != nil || hu.Host == "" {
		return false
	}
	match := func(raw string) bool {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			return false
		}
		return strings.EqualFold(u.Scheme, hu.Scheme) && strings.EqualFold(u.Host, hu.Host)
	}
	if o := strings.TrimSpace(c.GetHeader("Origin")); o != "" {
		return match(o)
	}
	if r := strings.TrimSpace(c.GetHeader("Referer")); r != "" {
		return match(r)
	}
	return false
}

// wbFormNonces appends a hidden wb_nonce to every builder form. The value is bound to the visitor
// (wbFormToken) and to the post and section in the form's action.
func (s *Server) wbFormNonces(c *gin.Context, h string) string {
	if !strings.Contains(h, "data-wb-form") {
		return h
	}
	return wbFormBlockRE.ReplaceAllStringFunc(h, func(m string) string {
		g := wbFormBlockRE.FindStringSubmatch(m)
		if len(g) != 4 || strings.Contains(g[2], `name="`+webbuilder.FormNonce+`"`) {
			return m
		}
		am := wbFormActionRE.FindStringSubmatch(g[1])
		if am == nil {
			return m
		}
		postID, err := strconv.ParseInt(am[1], 10, 64)
		if err != nil {
			return m
		}
		field := `<input type="hidden" name="` + webbuilder.FormNonce + `" value="` + html.EscapeString(s.wbFormToken(c, postID, am[2])) + `">`
		return g[1] + g[2] + field + g[3]
	})
}

// wbFormDisallowed checks the Discussion screen's disallowed keys (the comment blocklist, which
// Pursae Contact Form also applies) against the values, the address and the user agent.
func (s *Server) wbFormDisallowed(values map[string]string, ip, ua string) bool {
	keys := s.settings.String("disallowed_keys")
	if strings.TrimSpace(keys) == "" {
		return false
	}
	parts := make([]string, 0, len(webbuilder.FormFields)+2)
	for _, f := range webbuilder.FormFields {
		parts = append(parts, values[f])
	}
	hay := strings.ToLower(strings.Join(append(parts, ip, ua), "\n"))
	for _, k := range strings.Split(keys, "\n") {
		if k = strings.ToLower(strings.TrimSpace(k)); k != "" && strings.Contains(hay, k) {
			return true
		}
	}
	return false
}

// wbFormCaptcha verifies the captcha Pursae Contact Form is configured with (its keys, its
// verifier). It returns "" when there is nothing to check or the token is valid, else
// webbuilder.FormRequired (no token) or webbuilder.FormInvalid.
func (s *Server) wbFormCaptcha(ctx context.Context, values map[string]string, ip string) string {
	if !s.gontactOn() {
		return ""
	}
	type check struct{ secret, site, field, endpoint string }
	for _, k := range []check{
		{"gontact_turnstile_secret", "gontact_turnstile_sitekey", "cf-turnstile-response", "https://challenges.cloudflare.com/turnstile/v0/siteverify"},
		{"gontact_recaptcha_secret", "gontact_recaptcha_sitekey", "g-recaptcha-response", "https://www.google.com/recaptcha/api/siteverify"},
	} {
		secret := s.revealSecret(s.settings.String(k.secret)) // stored sealed (sealSecret)
		if secret == "" || s.settings.String(k.site) == "" {
			continue
		}
		token := strings.TrimSpace(values[k.field])
		if token == "" {
			return webbuilder.FormRequired
		}
		score, ok := s.verifyCaptcha(ctx, k.endpoint, secret, token, ip)
		if !ok || score < 0.5 { // Pursae Contact Form's reCAPTCHA threshold; Turnstile scores 1
			return webbuilder.FormInvalid
		}
	}
	return ""
}

// wbFormTitleOf is the title of a form: its section title, else the page title, else "Form".
func wbFormTitleOf(p *store.Post, schema webbuilder.FormSchema) string {
	title := wbFormLine(schema.Title)
	if title == "" {
		title = wbFormLine(p.Title)
	}
	if title == "" {
		title = "Form"
	}
	if utf8.RuneCountInString(title) > wbFormTitle {
		title = string([]rune(title)[:wbFormTitle]) + "…"
	}
	return title
}

// wbFormRecipients are the To and Cc addresses of a form: its own (section content), else the site
// defaults of Settings › Web Builder, else admin_email. The form's own addresses come from the page
// document, which an author can write: unless the page was last published by a user who may change
// site settings or post unfiltered HTML, they are limited to the site's own addresses
// (wbFormSiteAddress), so a page cannot turn the form into a mail relay.
func (s *Server) wbFormRecipients(ctx context.Context, p *store.Post, schema webbuilder.FormSchema) (to, cc []string) {
	trusted := s.wbFormPublisherTrusted(ctx, p)
	own := func(list string) []string {
		out := webbuilder.FormAddresses(list)
		if trusted {
			return out
		}
		return slices.DeleteFunc(out, func(a string) bool { return !s.wbFormSiteAddress(ctx, a) })
	}
	to = own(schema.To)
	if len(to) == 0 {
		to = webbuilder.FormAddresses(s.settings.String(wbFormToOption))
	}
	if len(to) == 0 {
		to = webbuilder.FormAddresses(s.settings.String("admin_email"))
	}
	cc = own(schema.Cc)
	if schema.Cc == "" {
		cc = webbuilder.FormAddresses(s.settings.String(wbFormCcOption))
	}
	return to, cc
}

// wbFormPublisherTrusted: the stored doc of p was published by a user with manage_options or
// unfiltered_html.
func (s *Server) wbFormPublisherTrusted(ctx context.Context, p *store.Post) bool {
	if p == nil {
		return false
	}
	row, err := s.store.WebBuilder().Doc(ctx, p.ID)
	if err != nil || row.SnapshotUser == 0 {
		return false
	}
	u, err := s.store.UserByID(ctx, row.SnapshotUser)
	return err == nil && u != nil && (u.Can("manage_options", nil) || u.Can("unfiltered_html", nil))
}

// wbFormSiteAddress: addr is admin_email, one of the site's default form recipients or the address
// of a user who can write posts.
func (s *Server) wbFormSiteAddress(ctx context.Context, addr string) bool {
	for _, opt := range []string{"admin_email", wbFormToOption, wbFormCcOption} {
		for _, a := range webbuilder.FormAddresses(s.settings.String(opt)) {
			if strings.EqualFold(a, addr) {
				return true
			}
		}
	}
	u, err := s.store.UserByLoginOrEmail(ctx, addr)
	return err == nil && u != nil && strings.EqualFold(u.Email, addr) && u.Can("edit_posts", nil)
}

// wbFormMessage is the text shown after a submit: the form's own, else the site's (Settings › Web
// Builder), else the default translated with the request language.
func (s *Server) wbFormMessage(ctx context.Context, custom, option, def string) string {
	if m := wbFormLine(custom); m != "" {
		return m
	}
	if m := wbFormLine(s.settings.String(option)); m != "" {
		return m
	}
	return s.frontT(ctx, def)
}

// wbFormMail queues the message to the form's recipients (the wp_mail job of the core mailer). link
// is the page the form was sent from (wbFormPageURL). The mail text is in the site language.
func (s *Server) wbFormMail(ctx context.Context, p *store.Post, link string, schema webbuilder.FormSchema, clean map[string]string, ip string) error {
	to, cc := s.wbFormRecipients(ctx, p, schema)
	if len(to) == 0 {
		return errors.New("no recipient")
	}
	blog := wbFormLine(s.settings.String("blogname"))
	title := wbFormTitleOf(p, schema)
	t := func(msgid string) string {
		if s.lang == nil {
			return msgid
		}
		return s.lang.Catalog(s.siteLocale()).T(msgid)
	}

	var b strings.Builder
	b.WriteString(i18n.Sprintf(t(`New message from the form "%s" on %s`), title, defaultStr(wbFormLine(p.Title), t("(no title)"))) + "\n")
	b.WriteString(link + "\n\n")
	for _, f := range schema.Fields {
		v, ok := clean[f.Name]
		if !ok {
			continue
		}
		label := defaultStr(wbFormLine(f.Label), f.Name)
		if f.Type == "textarea" {
			b.WriteString(label + ":\n" + v + "\n\n")
		} else {
			b.WriteString(label + ": " + v + "\n")
		}
	}
	b.WriteString("\n--\n" + i18n.Sprintf(t("Sent by Pursae Web Builder from %s"), defaultStr(blog, s.links(ctx).HomeURL("/"))) + "\n")
	if ip != "" {
		b.WriteString(t("IP address") + ": " + ip + "\n")
	}

	subject := title
	if blog != "" {
		subject = "[" + blog + "] " + title
	}
	msg := mail.Message{To: to, Cc: cc, Subject: subject, Body: b.String(), ReplyTo: clean["email"]}
	s.mailOnConfiguredHome(ctx, &msg)
	if _, err := s.mailer().Build(msg); err != nil {
		return err
	}
	return s.queueJob(context.WithoutCancel(ctx), "wp_mail", msg, time.Time{})
}

// wbFormLine is v on one line: control characters and line breaks become spaces (header safety).
func wbFormLine(v string) string {
	v = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == 0x2028 || r == 0x2029 {
			return ' '
		}
		return r
	}, v)
	return strings.Join(strings.Fields(v), " ")
}

// wbFormRespond answers JSON for the runtime and 303 back to the page (or to the form's redirect
// URL after a sent form) for a plain form post.
func (s *Server) wbFormRespond(c *gin.Context, p *store.Post, schema webbuilder.FormSchema, wantJSON bool, o wbFormOutcome, fields map[string]string) {
	ctx := c.Request.Context()
	redirect := ""
	if o == wbFormSent {
		redirect = s.wbFormRedirectURL(ctx, defaultStr(schema.Redirect, s.settings.String(wbFormRedirectOption)))
	}
	if wantJSON {
		switch o {
		case wbFormInvalid:
			restErr(c, http.StatusUnprocessableEntity, webbuilder.ErrFormInvalid, s.frontT(ctx, wbFormBadMsg), map[string]any{"fields": fields})
		case wbFormFailed:
			restErr(c, http.StatusInternalServerError, "wb_form_failed", s.wbFormMessage(ctx, schema.Failure, wbFormFailureOption, wbFormFailMsg))
		default:
			c.JSON(http.StatusOK, webbuilder.FormResponse{OK: true, Message: s.wbFormMessage(ctx, schema.Success, wbFormSuccessOption, wbFormSentMsg), Redirect: redirect})
		}
		return
	}
	if redirect != "" {
		c.Redirect(http.StatusSeeOther, redirect)
		return
	}
	state := "sent"
	if o != wbFormSent {
		state = "error"
	}
	c.Redirect(http.StatusSeeOther, wbFormBackURL(s.wbFormPageURL(c, p), state, schema.SectionID))
}

// wbFormRedirectURL turns a configured redirect into an absolute URL of this site, or "" when it is
// empty or points elsewhere (a path on this site, or an http(s) URL of the home host).
func (s *Server) wbFormRedirectURL(ctx context.Context, raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	home := s.links(ctx).HomeURL("/")
	hu, err := url.Parse(home)
	if err != nil || hu.Host == "" {
		return ""
	}
	if strings.HasPrefix(raw, "/") {
		path := safeRedirect(raw, "")
		if path == "" {
			return ""
		}
		return hu.Scheme + "://" + hu.Host + path
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || !strings.EqualFold(u.Host, hu.Host) || hasControlChar(raw) {
		return ""
	}
	return u.String()
}

// wbFormPageURL is the page a form was sent from: the builder page itself, or for a form inside a
// Theme Builder template (shown on many pages) the same-origin Referer, else the home page.
func (s *Server) wbFormPageURL(c *gin.Context, p *store.Post) string {
	lb := s.links(c)
	if !wbIsTemplate(p) {
		return lb.Permalink(p)
	}
	home := lb.HomeURL("/")
	ref, err := url.Parse(c.GetHeader("Referer"))
	if err != nil || (ref.Scheme != "http" && ref.Scheme != "https") || ref.Host == "" || ref.User != nil {
		return home
	}
	// Same origin means the home URL's host (local-origin aware through s.links), never the
	// request's Host header, which the client controls.
	hu, err := url.Parse(home)
	if err != nil || hu.Host == "" || !strings.EqualFold(ref.Host, hu.Host) {
		return home
	}
	path := ref.RequestURI()
	if safeRedirect(path, "") == "" { // a path on this site, nothing a browser would read as another host
		return home
	}
	return hu.Scheme + "://" + hu.Host + path
}

// wbFormBackURL is the page with ?wb_form=<state> and #<section>.
func wbFormBackURL(link, state, section string) string {
	u, err := url.Parse(link)
	if err != nil {
		return link
	}
	q := u.Query()
	q.Set(webbuilder.FormSentQuery, state)
	u.RawQuery = q.Encode()
	u.Fragment = section
	return u.String()
}
