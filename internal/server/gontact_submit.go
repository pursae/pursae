package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"net/mail"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"wpgo/internal/store"
)

// Submissions: WPCF7_Submission — posted data, validation, acceptance, spam checks, file uploads
// and the mails — plus the REST endpoints the front-end script talks to.

type gfInvalid struct{ name, reason, idref string }

type gfUpload struct {
	name, contentType string
	data              []byte
}

type gfSubmission struct {
	s        *Server
	c        *gin.Context
	ctx      context.Context
	form     *gfForm
	tags     []*gfTag
	user     *store.User
	unitTag  string
	status   string
	message  string
	invalid  []gfInvalid
	raw      map[string][]string // values as posted (before pipes), by field name
	posted   map[string][]string // values after pipes: what mail-tags print
	files    map[string][]gfUpload
	consent  map[string]string
	spamLog  []map[string]string
	skipMail bool
	hidden   map[string]bool // fields inside conditional groups that are hidden
	entryID  int64           // the stored gontact_entries row (0: not stored)
	limited  bool            // refused by the per-address rate limit
	uploaded int64           // bytes of all uploads so far

	remoteIP, userAgent, pageURL string
	containerPost                int64
	timestamp                    time.Time
}

func (sub *gfSubmission) is(status string) bool { return sub.status == status }

// invalidate is WPCF7_Validation::invalidate(); the first reason of a field wins.
func (sub *gfSubmission) invalidate(t *gfTag, key string) {
	for _, inv := range sub.invalid {
		if inv.name == t.Name {
			return
		}
	}
	sub.invalid = append(sub.invalid, gfInvalid{name: t.Name, reason: sub.form.message(key), idref: t.idOption()})
}

// gontactSubmit runs a submission for form f with the request's form data.
func (s *Server) gontactSubmit(c *gin.Context, f *gfForm, user *store.User, nonceOK bool) *gfSubmission {
	sub := &gfSubmission{s: s, c: c, ctx: c.Request.Context(), form: f, user: user, status: "init",
		raw: map[string][]string{}, posted: map[string][]string{}, files: map[string][]gfUpload{}, consent: map[string]string{},
		unitTag: gfUnitTagClean(c.PostForm("_wpcf7_unit_tag")), timestamp: time.Now()}
	sub.skipMail = f.demoMode() || f.isTrue("skip_mail")
	if f.isTrue("subscribers_only") && user == nil {
		sub.status, sub.message = "error", "This contact form is available only for logged in users."
		return sub
	}
	sub.remoteIP, sub.userAgent = clientIP(c), c.GetHeader("User-Agent")
	if !gontactRateOK(c, "feedback", gontactFeedbackPerMin) {
		sub.limited = true
		sub.status, sub.message = "error", "Too many submissions from your address. Please wait a minute and try again."
		return sub
	}
	sub.pageURL = c.GetHeader("Referer")
	if c.Request.URL.Path != "" && !strings.HasPrefix(c.Request.URL.Path, "/wp-json") {
		sub.pageURL = s.links(c).HomeURL(c.Request.URL.RequestURI())
	}
	sub.containerPost, _ = strconv.ParseInt(c.PostForm("_wpcf7_container_post"), 10, 64)
	_, sub.tags = gfScan(f.Form, nil)
	sub.setupPostedData()
	sub.applyConditions()

	valid := sub.validate()
	sub.pluginValidate()
	if !valid || len(sub.invalid) > 0 {
		sub.status, sub.message = "validation_failed", f.message("validation_error")
	}
	if sub.is("init") && !sub.accepted() {
		sub.status, sub.message = "acceptance_missing", f.message("accept_terms")
	}
	if sub.is("init") && sub.spam(nonceOK) {
		sub.status, sub.message = "spam", f.message("spam")
		slog.Info("gontact form: spam", "form", f.ID, "log", sub.spamLog)
	}
	if sub.is("init") && !sub.beforeSendMail() {
		return sub
	}
	if sub.is("init") {
		sub.storeEntry()
		if sub.mail() {
			sub.queueWebhooks()
			sub.status, sub.message = "mail_sent", f.message("mail_sent_ok")
			s.gontactCountSerial(sub.ctx, f)
			if s.plugins != nil && s.plugins.hooks.HasAction("wpcf7_mail_sent") {
				s.plugins.hooks.DoAction(sub.ctx, "wpcf7_mail_sent", map[string]any{"contact_form_id": f.ID, "title": f.Title, "posted_data": sub.flatPosted()})
			}
		} else {
			sub.status, sub.message = "mail_failed", f.message("mail_sent_ng")
			s.gontactEntryMailStatus(sub.ctx, sub.entryID, "failed")
		}
	}
	return sub
}

func gfUnitTagClean(v string) string {
	if gfUnitTagRe.MatchString(v) {
		return v
	}
	return ""
}

func (sub *gfSubmission) flatPosted() map[string]string {
	out := map[string]string{}
	for k, v := range sub.posted {
		out[k] = strings.Join(v, ", ")
	}
	return out
}

var gfNullRe = regexp.MustCompile("\x00")

// formValues returns the values posted under name or name[] (PHP's array syntax).
func (sub *gfSubmission) formValues(name string) []string {
	vals := sub.c.PostFormArray(name)
	if len(vals) == 0 {
		vals = sub.c.PostFormArray(name + "[]")
	}
	out := make([]string, 0, len(vals))
	for _, v := range vals {
		if !utf8.ValidString(v) {
			v = strings.ToValidUTF8(v, "")
		}
		out = append(out, strings.ReplaceAll(gfNullRe.ReplaceAllString(v, ""), "\r\n", "\n"))
	}
	return out
}

// setupPostedData is setup_posted_data(): every named field, pipes resolved, free text merged.
func (sub *gfSubmission) setupPostedData() {
	for _, t := range sub.tags {
		if t.Name == "" || t.BaseType == "file" || t.BaseType == "count" || t.BaseType == "captchac" {
			continue
		}
		vals := sub.formValues(t.Name)
		if t.BaseType == "checkbox" || t.BaseType == "radio" {
			if t.has("free_text") && len(t.Values) > 0 {
				last := t.Values[len(t.Values)-1]
				ft := "_wpcf7_free_text_" + t.Name
				sub.raw[ft] = sub.formValues(ft)
				for i, v := range vals {
					if v == last {
						vals[i] = strings.TrimSpace(last + " " + first(sub.raw[ft]))
					}
				}
			}
		}
		if t.BaseType == "text" || t.BaseType == "email" || t.BaseType == "url" || t.BaseType == "tel" || t.BaseType == "quiz" || t.BaseType == "captchar" {
			for i := range vals {
				vals[i] = strings.TrimSpace(strings.ReplaceAll(vals[i], "\n", " "))
			}
		}
		if t.BaseType == "url" {
			for i, v := range vals {
				if v != "" && !gfSchemeRe.MatchString(v) && strings.Contains(v, ".") {
					vals[i] = "http://" + strings.TrimLeft(v, "/")
				}
			}
		}
		sub.raw[t.Name] = vals
		piped := make([]string, len(vals))
		for i, v := range vals {
			piped[i] = v
			if (t.BaseType == "select" || t.BaseType == "checkbox" || t.BaseType == "radio") && t.hasPipes() {
				piped[i] = t.pipeAfter(v)
			}
		}
		sub.posted[t.Name] = piped
	}
}

var (
	gfTelRe  = regexp.MustCompile(`^[+]?[0-9() -]*[0-9][0-9() -]*$`)
	gfDateRe = regexp.MustCompile(`^[0-9]{4,}-[0-9]{2}-[0-9]{2}$`)
	gfNumRe  = regexp.MustCompile(`^[-]?(?:[0-9]+|[0-9]*[.][0-9]+)(?:[eE][+-]?[0-9]+)?$`)
	gfURLRe  = regexp.MustCompile(`(?i)^[a-z][a-z0-9.+-]*:[^\s]+$`)
)

// gfIsEmail is wpcf7_is_email(): one address with a dotted domain.
func gfIsEmail(v string) bool {
	a, err := mail.ParseAddress(v)
	if err != nil || a.Address != v {
		return false
	}
	at := strings.LastIndex(v, "@")
	return at > 0 && strings.Contains(v[at+1:], ".") && !strings.HasSuffix(v, ".")
}

// validate runs every field rule (the SWV schema and the wpcf7_validate_* filters).
func (sub *gfSubmission) validate() bool {
	acceptAsValidation := sub.form.isTrue("acceptance_as_validation")
	for _, t := range sub.tags {
		if t.Name == "" || sub.hidden[t.Name] {
			continue
		}
		vals := sub.raw[t.Name]
		v := strings.Join(vals, "")
		empty := strings.TrimSpace(v) == ""
		switch t.BaseType {
		case "text", "email", "url", "tel", "textarea":
			if t.required() && empty {
				sub.invalidate(t, "invalid_required")
				continue
			}
			if empty {
				continue
			}
			switch t.BaseType {
			case "email":
				if !gfIsEmail(v) {
					sub.invalidate(t, "invalid_email")
				}
			case "url":
				if !gfURLRe.MatchString(v) {
					sub.invalidate(t, "invalid_url")
				}
			case "tel":
				if !gfTelRe.MatchString(v) {
					sub.invalidate(t, "invalid_tel")
				}
			}
			def := "400"
			if t.BaseType == "textarea" {
				def = "2000"
			}
			n := utf8.RuneCountInString(v)
			if max := gfAtoi(t.maxlengthOption(def)); max > 0 && n > max {
				sub.invalidate(t, "invalid_too_long")
			}
			if min := gfAtoi(t.minlengthOption()); min > 0 && n < min {
				sub.invalidate(t, "invalid_too_short")
			}
		case "number", "range":
			if t.required() && empty {
				sub.invalidate(t, "invalid_required")
				continue
			}
			if empty {
				continue
			}
			if !gfNumRe.MatchString(v) {
				sub.invalidate(t, "invalid_number")
				continue
			}
			x, _ := strconv.ParseFloat(v, 64)
			if min := t.option1("min", "signed_num"); min != "" {
				if m, _ := strconv.ParseFloat(min, 64); x < m {
					sub.invalidate(t, "number_too_small")
				}
			}
			if max := t.option1("max", "signed_num"); max != "" {
				if m, _ := strconv.ParseFloat(max, 64); x > m {
					sub.invalidate(t, "number_too_large")
				}
			}
		case "date":
			if t.required() && empty {
				sub.invalidate(t, "invalid_required")
				continue
			}
			if empty {
				continue
			}
			if _, err := time.Parse("2006-01-02", v); err != nil || !gfDateRe.MatchString(v) {
				sub.invalidate(t, "invalid_date")
				continue
			}
			r := &gfRender{s: sub.s}
			if min := t.dateOption("min", r.today); min != "" && v < min {
				sub.invalidate(t, "date_too_early")
			}
			if max := t.dateOption("max", r.today); max != "" && v > max {
				sub.invalidate(t, "date_too_late")
			}
		case "select", "checkbox", "radio":
			if (t.required() || t.BaseType == "radio") && empty {
				sub.invalidate(t, "invalid_required")
				continue
			}
			// Only the offered choices (and the free text of the last one) are accepted.
			for _, x := range vals {
				if x == "" || gfContains(t.Values, x) {
					continue
				}
				if t.has("free_text") && len(t.Values) > 0 && strings.HasPrefix(x, t.Values[len(t.Values)-1]) {
					continue
				}
				sub.invalidate(t, "validation_error")
			}
			if t.BaseType != "checkbox" || t.has("exclusive") {
				if len(vals) > 1 {
					sub.invalidate(t, "validation_error")
				}
			}
		case "quiz":
			ans := gfCanonicalize(v)
			expected := first(sub.c.PostFormArray("_wpcf7_quiz_answer_" + t.Name))
			if !hmac.Equal([]byte(expected), []byte(sub.s.gontactQuizHash(ans))) {
				sub.invalidate(t, "quiz_answer_not_correct")
			}
		case "captchar":
			// wpcf7_captcha_validation_filter(): the challenge is used up, right or wrong.
			prefix := first(sub.c.PostFormArray("_wpcf7_captcha_challenge_" + t.Name))
			if prefix == "" || !sub.s.gontactCaptchaCheck(sub.ctx, prefix, v) {
				sub.invalidate(t, "captcha_not_match")
			}
		case "acceptance":
			if acceptAsValidation && !t.has("optional") {
				on := v != ""
				if t.has("invert") == on {
					sub.invalidate(t, "accept_terms")
				}
			}
		case "file":
			sub.validateFile(t)
		}
	}
	return len(sub.invalid) == 0
}

// gfCanonicalize is wpcf7_canonicalize() with strip_separators: lower case, no blanks or dashes.
func gfCanonicalize(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	return gfTelStripRe.ReplaceAllString(v, "")
}

// gontactQuizHash is wp_hash( $answer, 'wpcf7_quiz' ).
func (s *Server) gontactQuizHash(answer string) string {
	m := hmac.New(sha256.New, []byte(s.settings.String(gontactSecretKey)+"|wpcf7_quiz"))
	m.Write([]byte(gfCanonicalize(answer)))
	return hex.EncodeToString(m.Sum(nil))[:32]
}

// validateFile checks an upload against required, limit: and filetypes: and keeps it for the mail.
func (sub *gfSubmission) validateFile(t *gfTag) {
	var headers []*multipart.FileHeader
	if mf := sub.c.Request.MultipartForm; mf != nil {
		headers = append(mf.File[t.Name], mf.File[t.Name+"[]"]...)
	}
	if len(headers) == 0 {
		if t.required() {
			sub.invalidate(t, "invalid_required")
		}
		return
	}
	types := gfFileTypes(t)
	limit := t.limitOption()
	var total int64
	for _, fh := range headers {
		name := gfSanitizeFileName(fh.Filename)
		ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(name), "."))
		ctype := mime.TypeByExtension("." + ext)
		ok := false
		for _, ft := range types {
			if ft == ext || ft == ctype || strings.HasSuffix(ft, "/*") && strings.HasPrefix(ctype, strings.TrimSuffix(ft, "*")) {
				ok = true
				break
			}
		}
		if !ok || ext == "" {
			sub.invalidate(t, "upload_file_type_invalid")
			return
		}
		total += fh.Size
		sub.uploaded += fh.Size
		if total > limit || sub.uploaded > gfUploadTotalMax {
			sub.invalidate(t, "upload_file_too_large")
			return
		}
		file, err := fh.Open()
		if err != nil {
			sub.invalidate(t, "upload_failed")
			return
		}
		data, err := io.ReadAll(io.LimitReader(file, limit+1))
		file.Close()
		if err != nil {
			sub.invalidate(t, "upload_failed_php_error")
			return
		}
		if int64(len(data)) > limit {
			sub.invalidate(t, "upload_file_too_large")
			return
		}
		if !gfSniffMatches(ext, data) {
			sub.invalidate(t, "upload_file_type_invalid")
			return
		}
		if ctype == "" {
			ctype = "application/octet-stream"
		}
		sub.files[t.Name] = append(sub.files[t.Name], gfUpload{name: gfAntiscript(name), contentType: ctype, data: data})
	}
	names := make([]string, 0, len(sub.files[t.Name]))
	for _, u := range sub.files[t.Name] {
		names = append(names, u.name)
	}
	sub.raw[t.Name], sub.posted[t.Name] = names, names
}

// gfSniffTypes lists, per extension, the content types http.DetectContentType may report for a
// genuine file (the first 512 bytes decide). Extensions not listed are not sniffed.
var gfSniffTypes = map[string][]string{
	"jpg": {"image/jpeg"}, "jpeg": {"image/jpeg"}, "jpe": {"image/jpeg"},
	"png": {"image/png"}, "gif": {"image/gif"}, "webp": {"image/webp"}, "bmp": {"image/bmp"},
	"pdf": {"application/pdf"},
	"zip": {"application/zip"}, "docx": {"application/zip"}, "xlsx": {"application/zip"}, "pptx": {"application/zip"},
	"odt": {"application/zip"}, "ods": {"application/zip"}, "odp": {"application/zip"},
	"mp3": {"audio/mpeg"},
	"txt": {"text/plain"}, "csv": {"text/plain"},
}

// gfSniffMatches reports whether the content of an upload fits its extension.
func gfSniffMatches(ext string, data []byte) bool {
	want, ok := gfSniffTypes[ext]
	if !ok {
		return true
	}
	head := data
	if len(head) > 512 {
		head = head[:512]
	}
	got := http.DetectContentType(head)
	if i := strings.Index(got, ";"); i >= 0 {
		got = got[:i]
	}
	if ext == "mp3" && got == "application/octet-stream" { // ID3-less frames are not always recognised
		return true
	}
	if (ext == "txt" || ext == "csv") && len(data) == 0 {
		return true
	}
	return gfContains(want, got)
}

// gfUploadTotalMax caps all uploads of one submission: they are held in memory and queued with the mail.
const gfUploadTotalMax = 25 << 20

// gfSanitizeFileName is sanitize_file_name() for uploads: no paths, no odd characters.
func gfSanitizeFileName(name string) string {
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	name = gfFileSpecialRe.ReplaceAllString(name, "")
	name = gfFileDashRe.ReplaceAllString(name, "-")
	name = strings.Trim(name, ".-_")
	if name == "" {
		name = "unnamed-file"
	}
	return name
}

// gfAntiscript is wpcf7_antiscript_file_name(): script extensions get an underscore.
func gfAntiscript(name string) string {
	ext := strings.ToLower(filepath.Ext(name))
	if gfBadExtRe.MatchString(ext) {
		return name + "_.txt"
	}
	return name
}

// accepted is the wpcf7_acceptance filter.
func (sub *gfSubmission) accepted() bool {
	ok := true
	for _, t := range sub.tags {
		if t.BaseType != "acceptance" || t.Name == "" || sub.hidden[t.Name] {
			continue
		}
		on := strings.Join(sub.raw[t.Name], "") != ""
		content := t.Content
		if content == "" {
			content = first(t.Values)
		}
		if on && strings.TrimSpace(content) != "" {
			sub.consent[t.Name] = strings.TrimSpace(content)
		}
		if t.has("optional") || sub.form.isTrue("acceptance_as_validation") {
			continue
		}
		if t.has("invert") == on {
			ok = false
		}
	}
	return ok
}

// spam is WPCF7_Submission::spam() with the disallowed list, reCAPTCHA and Turnstile checks.
func (sub *gfSubmission) spam(nonceOK bool) bool {
	if sub.form.isTrue("subscribers_only") && sub.user != nil {
		return false
	}
	spam := false
	log := func(agent, reason string) {
		spam = true
		sub.spamLog = append(sub.spamLog, map[string]string{"agent": agent, "reason": reason})
	}
	if utf8.RuneCountInString(sub.userAgent) < 2 {
		log("wpcf7", "User-Agent string is unnaturally short.")
	}
	if !nonceOK {
		log("wpcf7", "Submitted nonce is invalid.")
	}
	if sub.c.PostForm(gontactHoneypotField) != "" {
		log("honeypot", "The hidden honeypot field was filled in.")
	}
	if present, valid, age := sub.s.gontactCheckTimestamp(sub.c.PostForm(gontactTimestampField), sub.timestamp); present {
		if !valid || age < 0 {
			log("timestamp", "The signed form timestamp is invalid.")
		} else if age < int64(gontactMinFillSeconds) {
			log("timestamp", "The form was submitted faster than a person can fill it.")
		}
	}
	if word := sub.s.gontactDisallowed(sub); word != "" {
		log("disallowed_list", "Disallowed words ("+word+") are used.")
	}
	if secret := sub.s.settings.String("gontact_recaptcha_secret"); secret != "" && sub.s.settings.String("gontact_recaptcha_sitekey") != "" {
		token := sub.c.PostForm("_wpcf7_recaptcha_response")
		if token == "" && sub.s.gooikeOn() {
			// Pursae Cookie holds reCAPTCHA back until the visitor consents; the other checks still apply.
		} else if token == "" {
			log("recaptcha", "reCAPTCHA response token is empty.")
		} else if score, ok := sub.s.verifyCaptcha(sub.ctx, captchaRecaptchaVerify, secret, token, sub.remoteIP); !ok || score < 0.5 {
			log("recaptcha", "reCAPTCHA score ("+strconv.FormatFloat(score, 'f', 2, 64)+") is lower than the threshold (0.50).")
		}
	}
	if secret := sub.s.settings.String("gontact_turnstile_secret"); secret != "" && sub.s.settings.String("gontact_turnstile_sitekey") != "" {
		token := sub.c.PostForm("_wpcf7_turnstile_response")
		if token == "" {
			log("turnstile", "Turnstile token is empty.")
		} else if _, ok := sub.s.verifyCaptcha(sub.ctx, captchaTurnstileVerify, secret, token, sub.remoteIP); !ok {
			log("turnstile", "Turnstile validation failed.")
		}
	}
	if sub.s.plugins != nil && sub.s.plugins.hooks.HasFilter("wpcf7_spam") {
		if v, ok := sub.s.applyFilters(sub.ctx, "wpcf7_spam", spam, map[string]any{"contact_form_id": sub.form.ID, "posted_data": sub.flatPosted(),
			"remote_ip": sub.remoteIP, "user_agent": sub.userAgent}).(bool); ok {
			spam = v
		}
	}
	return spam
}

// gontactDisallowed is wpcf7_disallowed_list(): the Discussion screen's disallowed keys against the
// posted values, the IP and the user agent. It returns the matching word.
func (s *Server) gontactDisallowed(sub *gfSubmission) string {
	keys := s.settings.String("disallowed_keys")
	if strings.TrimSpace(keys) == "" {
		return ""
	}
	var parts []string
	for _, v := range sub.posted {
		parts = append(parts, v...)
	}
	parts = append(parts, sub.remoteIP, sub.userAgent)
	hay := strings.ToLower(strings.Join(parts, "\n"))
	for _, k := range strings.Split(keys, "\n") {
		if k = strings.ToLower(strings.TrimSpace(k)); k != "" && strings.Contains(hay, k) {
			return k
		}
	}
	return ""
}

// gontactCountSerial keeps the [_serial_number] counter of a form.
func (s *Server) gontactCountSerial(ctx context.Context, f *gfForm) {
	_, _ = s.store.Pool.Exec(ctx, `UPDATE posts SET meta = jsonb_set(meta, '{_gontact_serial}', to_jsonb(coalesce((meta->>'_gontact_serial')::int, 0) + 1)) WHERE id = $1`, f.ID)
}

// postedDataHash is get_posted_data_hash().
func (sub *gfSubmission) postedDataHash() string {
	raw, _ := json.Marshal(sub.posted)
	m := hmac.New(sha256.New, []byte(sub.s.settings.String(gontactSecretKey)+"|posted_data"))
	m.Write(raw)
	m.Write([]byte(sub.remoteIP + "|" + sub.userAgent))
	return hex.EncodeToString(m.Sum(nil))[:32]
}

// result is get_result() plus the REST response properties of create_feedback().
func (sub *gfSubmission) result() map[string]any {
	out := map[string]any{"contact_form_id": sub.form.ID, "status": sub.status, "message": sub.message}
	switch sub.status {
	case "init", "validation_failed", "acceptance_missing", "spam", "error":
		out["posted_data_hash"] = ""
	default:
		out["posted_data_hash"] = sub.postedDataHash()
	}
	if sub.form.demoMode() {
		out["demo_mode"] = true
	}
	if sub.status == "error" {
		delete(out, "posted_data_hash")
	}
	invalid := []map[string]any{}
	for _, inv := range sub.invalid {
		var idref any
		if inv.idref != "" {
			idref = inv.idref
		}
		invalid = append(invalid, map[string]any{"field": inv.name, "message": inv.reason, "idref": idref,
			"error_id": gfNonIDRe.ReplaceAllString(sub.unitTag+"-ve-"+inv.name, "")})
	}
	out["into"] = "#" + sub.unitTag
	out["invalid_fields"] = invalid
	if q := sub.s.gontactQuizRefill(sub.tags); len(q) > 0 {
		out["quiz"] = q
	}
	ctx := sub.ctx
	if sub.c != nil {
		ctx = sub.c // carries the client network for the per-network CAPTCHA limit
	}
	if cp := sub.s.gontactCaptchaRefill(ctx, sub.tags); len(cp) > 0 {
		out["captcha"] = cp
	}
	return out
}

// gontactQuizRefill is wpcf7_quiz_ajax_refill(): a fresh question and answer hash per quiz.
func (s *Server) gontactQuizRefill(tags []*gfTag) map[string]any {
	out := map[string]any{}
	for _, t := range tags {
		if t.BaseType == "quiz" && t.Name != "" {
			q, a := s.gontactQuizPick(t)
			out[t.Name] = []string{q, s.gontactQuizHash(a)}
		}
	}
	return out
}

// REST ------------------------------------------------------------------------------

func (s *Server) registerGontactREST() {
	api := s.rest
	const ns = gontactNamespace
	api.register(ns, "/"+ns+"/contact-forms/(?P<id>[\\d]+)/feedback", map[string]restHandler{http.MethodPost: s.gontactFeedback}, nil)
	api.register(ns, "/"+ns+"/contact-forms/(?P<id>[\\d]+)/feedback/schema", map[string]restHandler{http.MethodGet: s.gontactSchemaREST}, nil)
	api.register(ns, "/"+ns+"/contact-forms/(?P<id>[\\d]+)/refill", map[string]restHandler{http.MethodGet: s.gontactRefillREST}, nil)
	api.register(ns, "/"+ns+"/contact-forms", map[string]restHandler{http.MethodGet: s.gontactListREST, http.MethodPost: s.gontactCreateREST}, nil)
	api.register(ns, "/"+ns+"/contact-forms/(?P<id>[\\d]+)", map[string]restHandler{
		http.MethodGet: s.gontactGetREST, http.MethodPost: s.gontactUpdateREST, http.MethodDelete: s.gontactDeleteREST}, nil)
}

func (s *Server) gontactRESTForm(c *gin.Context, r *restRequest) *gfForm {
	f, err := s.gontactForm(c, r.ID())
	if err != nil {
		restErr(c, http.StatusNotFound, "wpcf7_not_found", "The requested contact form was not found.")
		return nil
	}
	return f
}

// gontactFeedback is POST /contact-form-7/v1/contact-forms/{id}/feedback.
func (s *Server) gontactFeedback(c *gin.Context, r *restRequest) {
	if !strings.HasPrefix(c.ContentType(), "multipart/form-data") {
		restErr(c, http.StatusUnsupportedMediaType, "wpcf7_unsupported_media_type", "The request payload format is not supported.")
		return
	}
	f := s.gontactRESTForm(c, r)
	if f == nil {
		return
	}
	// The body is read before anything is checked: bound it by what the form can accept (its file
	// fields' limit: options plus room for the text fields), so an anonymous post cannot stream
	// gigabytes into temporary files.
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, gfBodyLimit(f))
	if gfUnitTagClean(c.PostForm("_wpcf7_unit_tag")) == "" {
		restErr(c, http.StatusBadRequest, "wpcf7_unit_tag_not_found", "There is no valid unit tag.")
		return
	}
	// Cookie users authenticate through the form's _wpnonce (rest_cookie_check_errors), so a
	// logged-in visitor without a valid nonce is a visitor here, exactly as in WordPress.
	sub := s.gontactSubmit(c, f, restCurrent(c), true)
	if sub.limited {
		c.Header("Retry-After", "60")
		restErr(c, http.StatusTooManyRequests, "wpcf7_rate_limited", sub.message)
		return
	}
	restJSON(c, http.StatusOK, sub.result())
}

// gfBodyLimit is the largest request body a form's submission needs: every file field at its limit:
// plus 2 MiB for the text fields and the multipart framing.
func gfBodyLimit(f *gfForm) int64 {
	n := int64(2 << 20)
	_, tags := gfScan(f.Form, nil)
	for _, t := range tags {
		if t.BaseType == "file" && t.Name != "" {
			n += t.limitOption() + 4<<10
		}
	}
	return n
}

// gontactFallbackPost handles a form posted without JavaScript to the page that shows it
// (wpcf7_control_init()): the submission runs and the page renders with its result. It reports
// whether the request was such a submission (the site then renders the page as for a GET).
func (s *Server) gontactFallbackPost(c *gin.Context) bool {
	if c.Request.Method != http.MethodPost || !s.gontactOn() || c.PostForm("_wpcf7") == "" {
		return false
	}
	id, _ := strconv.ParseInt(c.PostForm("_wpcf7"), 10, 64)
	f, err := s.gontactForm(c, id)
	if err != nil || gfUnitTagClean(c.PostForm("_wpcf7_unit_tag")) == "" {
		return false
	}
	u := currentUser(c)
	nonceOK := u == nil || s.checkNonceValue(c, c.PostForm("_wpnonce"), "wp_rest")
	c.Set("gontact.posted", s.gontactSubmit(c, f, u, nonceOK))
	c.Header("Cache-Control", "no-cache, must-revalidate, max-age=0, no-store, private")
	return true
}

// gontactSchema is WPCF7_SWV_Schema::to_array() for the client-side validation.
func (s *Server) gontactSchema(f *gfForm) map[string]any {
	_, tags := gfScan(f.Form, nil)
	rules := []map[string]any{}
	add := func(rule, field, key string, extra ...any) {
		r := map[string]any{"rule": rule, "field": field, "error": f.message(key)}
		for i := 0; i+1 < len(extra); i += 2 {
			r[extra[i].(string)] = extra[i+1]
		}
		rules = append(rules, r)
	}
	rt := &gfRender{s: s}
	for _, t := range tags {
		if t.Name == "" {
			continue
		}
		switch t.BaseType {
		case "text", "email", "url", "tel", "textarea":
			if t.required() {
				add("required", t.Name, "invalid_required")
			}
			if t.BaseType != "text" && t.BaseType != "textarea" {
				add(t.BaseType, t.Name, "invalid_"+t.BaseType)
			}
			def := "400"
			if t.BaseType == "textarea" {
				def = "2000"
			}
			if min := gfAtoi(t.minlengthOption()); min > 0 {
				add("minlength", t.Name, "invalid_too_short", "threshold", min)
			}
			if max := gfAtoi(t.maxlengthOption(def)); max > 0 {
				add("maxlength", t.Name, "invalid_too_long", "threshold", max)
			}
		case "number", "range":
			if t.required() {
				add("required", t.Name, "invalid_required")
			}
			add("number", t.Name, "invalid_number")
			if min := t.option1("min", "signed_num"); min != "" {
				v, _ := strconv.ParseFloat(min, 64)
				add("minnumber", t.Name, "number_too_small", "threshold", v)
			}
			if max := t.option1("max", "signed_num"); max != "" {
				v, _ := strconv.ParseFloat(max, 64)
				add("maxnumber", t.Name, "number_too_large", "threshold", v)
			}
		case "date":
			if t.required() {
				add("required", t.Name, "invalid_required")
			}
			add("date", t.Name, "invalid_date")
			if min := t.dateOption("min", rt.today); min != "" {
				add("mindate", t.Name, "date_too_early", "threshold", min)
			}
			if max := t.dateOption("max", rt.today); max != "" {
				add("maxdate", t.Name, "date_too_late", "threshold", max)
			}
		case "select", "checkbox", "radio":
			if t.required() || t.BaseType == "radio" {
				add("required", t.Name, "invalid_required")
			}
		case "file":
			if t.required() {
				add("requiredfile", t.Name, "invalid_required")
			}
			add("file", t.Name, "upload_file_type_invalid", "accept", strings.Split(gfAcceptTypes(t), ","))
			add("maxfilesize", t.Name, "upload_file_too_large", "threshold", t.limitOption())
		}
	}
	return map[string]any{"version": "Contact Form 7 SWV Schema 2024-10", "locale": f.Locale, "rules": rules}
}

func (s *Server) gontactSchemaREST(c *gin.Context, r *restRequest) {
	if f := s.gontactRESTForm(c, r); f != nil {
		restJSON(c, http.StatusOK, s.gontactSchema(f))
	}
}

func (s *Server) gontactRefillREST(c *gin.Context, r *restRequest) {
	f := s.gontactRESTForm(c, r)
	if f == nil {
		return
	}
	if !gontactRateOK(c, "refill", gontactRefillPerMin) {
		c.Header("Retry-After", "60")
		restErr(c, http.StatusTooManyRequests, "wpcf7_rate_limited", "Too many requests. Please wait a minute and try again.")
		return
	}
	_, tags := gfScan(f.Form, nil)
	out := map[string]any{}
	if q := s.gontactQuizRefill(tags); len(q) > 0 {
		out["quiz"] = q
	}
	if cp := s.gontactCaptchaRefill(c, tags); len(cp) > 0 {
		out["captcha"] = cp
	}
	c.Header("Cache-Control", "no-cache, must-revalidate, max-age=0, no-store, private")
	restJSON(c, http.StatusOK, out)
}

// The contact form CRUD endpoints (WPCF7_REST_Controller) ------------------------------

func (s *Server) gontactRESTProps(f *gfForm) map[string]any {
	_, tags := gfScan(f.Form, nil)
	fields := []map[string]any{}
	for _, t := range tags {
		fields = append(fields, map[string]any{"type": t.Type, "basetype": t.BaseType, "name": t.Name, "options": t.Options,
			"raw_values": t.Values, "labels": t.Labels, "values": t.Values, "content": t.Content})
	}
	msgs := map[string]string{}
	for _, m := range gfMessageDefs {
		msgs[m.Key] = f.message(m.Key)
	}
	mailJSON := func(m gfMail, active bool) map[string]any { return gfMailMeta(m, active) }
	var settings []map[string]string
	for _, line := range strings.Split(f.Additional, "\n") {
		if m := gfSettingRe.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			settings = append(settings, map[string]string{"name": m[1], "value": strings.TrimSpace(m[2])})
		}
	}
	return map[string]any{"id": f.ID, "slug": "contact-form-" + itoa64(f.ID), "title": f.Title, "locale": f.Locale, "hash": f.Hash,
		"properties": map[string]any{"form": map[string]any{"content": f.Form, "fields": fields}, "mail": mailJSON(f.Mail, false),
			"mail_2": mailJSON(f.Mail2, true), "messages": msgs,
			"additional_settings": map[string]any{"content": f.Additional, "settings": settings}}}
}

func (s *Server) gontactListREST(c *gin.Context, r *restRequest) {
	if u := restCurrent(c); u == nil || !u.Can("edit_posts", nil) {
		restErr(c, http.StatusUnauthorized, "wpcf7_forbidden", "You are not allowed to access contact forms.")
		return
	}
	forms, _, err := s.gontactForms(c, c.Query("search"), "", "", 1, -1)
	if err != nil {
		restErr(c, http.StatusInternalServerError, "wpcf7_cannot_list", err.Error())
		return
	}
	out := []map[string]any{}
	for _, f := range forms {
		out = append(out, map[string]any{"id": f.ID, "slug": "contact-form-" + itoa64(f.ID), "title": f.Title, "locale": f.Locale, "hash": f.Hash})
	}
	restJSON(c, http.StatusOK, out)
}

func (s *Server) gontactGetREST(c *gin.Context, r *restRequest) {
	if u := restCurrent(c); u == nil || !u.Can("publish_pages", nil) {
		restErr(c, http.StatusUnauthorized, "wpcf7_forbidden", "You are not allowed to access the requested contact form.")
		return
	}
	if f := s.gontactRESTForm(c, r); f != nil {
		restJSON(c, http.StatusOK, s.gontactRESTProps(f))
	}
}

// gontactApplyREST copies title, locale, form, mail, mail_2, messages and additional_settings
// from a REST body onto a form.
func (s *Server) gontactApplyREST(c *gin.Context, r *restRequest, f *gfForm) {
	if v, ok := r.Str(c, "title"); ok {
		f.Title = v
	}
	if v, ok := r.Str(c, "locale"); ok && v != "" {
		f.Locale = v
	}
	if v, ok := r.Str(c, "form"); ok {
		f.Form = v
	}
	if v, ok := r.Body["mail"].(map[string]any); ok {
		f.Mail = gfMailFrom(v)
	}
	if v, ok := r.Body["mail_2"].(map[string]any); ok {
		f.Mail2 = gfMailFrom(v)
	}
	if v, ok := r.Body["messages"].(map[string]any); ok {
		for k, m := range v {
			if str, ok := m.(string); ok {
				f.Messages[k] = str
			}
		}
	}
	if v, ok := r.Str(c, "additional_settings"); ok {
		f.Additional = v
	}
}

func (s *Server) gontactCreateREST(c *gin.Context, r *restRequest) {
	u := restCurrent(c)
	if u == nil || !u.Can("publish_pages", nil) {
		restErr(c, http.StatusUnauthorized, "wpcf7_forbidden", "You are not allowed to create a contact form.")
		return
	}
	if _, ok := r.Params["id"]; ok {
		restErr(c, http.StatusBadRequest, "wpcf7_post_exists", "Cannot create existing contact form.")
		return
	}
	f := s.gontactDefaultForm()
	f.AuthorID = u.ID
	s.gontactApplyREST(c, r, f)
	if err := s.gontactSave(c, f, u.ID); err != nil {
		restErr(c, http.StatusInternalServerError, "wpcf7_cannot_save", "There was an error saving the contact form.")
		return
	}
	restJSON(c, http.StatusOK, s.gontactRESTProps(f))
}

func (s *Server) gontactUpdateREST(c *gin.Context, r *restRequest) {
	u := restCurrent(c)
	if u == nil || !u.Can("publish_pages", nil) {
		restErr(c, http.StatusUnauthorized, "wpcf7_forbidden", "You are not allowed to edit this contact form.")
		return
	}
	f := s.gontactRESTForm(c, r)
	if f == nil {
		return
	}
	s.gontactApplyREST(c, r, f)
	if err := s.gontactSave(c, f, u.ID); err != nil {
		restErr(c, http.StatusInternalServerError, "wpcf7_cannot_save", "There was an error saving the contact form.")
		return
	}
	restJSON(c, http.StatusOK, s.gontactRESTProps(f))
}

func (s *Server) gontactDeleteREST(c *gin.Context, r *restRequest) {
	if u := restCurrent(c); u == nil || !u.Can("publish_pages", nil) {
		restErr(c, http.StatusUnauthorized, "wpcf7_forbidden", "You are not allowed to delete this contact form.")
		return
	}
	f := s.gontactRESTForm(c, r)
	if f == nil {
		return
	}
	if err := s.store.DeletePost(c, f.ID); err != nil {
		restErr(c, http.StatusInternalServerError, "wpcf7_cannot_delete", "There was an error deleting the contact form.")
		return
	}
	restJSON(c, http.StatusOK, map[string]any{"deleted": true})
}

// Compiled once (they used to be compiled on every call).
var (
	gfUnitTagRe     = regexp.MustCompile(`^wpcf7-f[0-9]+(-p[0-9]+)?-o[0-9]+$`)
	gfSchemeRe      = regexp.MustCompile(`(?i)^[a-z][a-z0-9.+-]*:`)
	gfTelStripRe    = regexp.MustCompile(`[\s\x{00a0}\x{200b}-\x{200d}\x{2060}\x{feff}\x{3000}\-\x{2010}-\x{2015}]+`)
	gfFileSpecialRe = regexp.MustCompile(`[?\[\]/\\=<>:;,'"&$#*()|~` + "`" + `!{}%+’«»”“\x00-\x1f]+`)
	gfFileDashRe    = regexp.MustCompile(`[\s-]+`)
	gfBadExtRe      = regexp.MustCompile(`^\.(php|phtml|pht|phar|pl|py|cgi|asp|aspx|js|sh|exe|bat|html?|svg)\d*$`)
)
