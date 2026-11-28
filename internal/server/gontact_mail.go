package server

import (
	"html"
	"log/slog"
	"mime"
	"net/mail"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	wpmail "wpgo/internal/mail"
	"wpgo/internal/wp"
)

// Mail: WPCF7_Mail and WPCF7_MailTaggedText — mail-tags ([your-name], [_raw_…], [_format_…],
// special tags) replaced in the Mail / Mail (2) templates, then handed to the core mail queue.

var gfMailTagRe = regexp.MustCompile(`(\[?)\[[\t ]*([a-zA-Z_][0-9a-zA-Z:._-]*)((?:[\t ]+"[^"]*"|[\t ]+'[^']*')*)[\t ]*\](\]?)`)

// mailTags replaces the mail-tags of text. With htmlMode values are escaped; replaced collects the
// values that replaced tags (for exclude_blank).
func (sub *gfSubmission) mailTags(text string, htmlMode bool, replaced *[]string) string {
	return gfMailTagRe.ReplaceAllStringFunc(text, func(m string) string {
		g := gfMailTagRe.FindStringSubmatch(m)
		if g[1] == "[" && g[4] == "]" {
			return m[1 : len(m)-1]
		}
		v, ok := sub.mailTag(g[2], gfValRe.FindAllString(g[3], -1))
		if !ok {
			return m
		}
		if replaced != nil {
			*replaced = append(*replaced, v)
		}
		if htmlMode {
			v = strings.ReplaceAll(html.EscapeString(v), "\n", "<br />\n")
		}
		return g[1] + v + g[4]
	})
}

// mailTag resolves one mail-tag; ok is false for unknown tags, which stay as they are.
func (sub *gfSubmission) mailTag(name string, values []string) (string, bool) {
	for i, v := range values {
		values[i] = v[1 : len(v)-1]
	}
	raw := false
	if n, ok := strings.CutPrefix(name, "_raw_"); ok {
		name, raw = n, true
	}
	format := ""
	if n, ok := strings.CutPrefix(name, "_format_"); ok {
		name = n
		if len(values) > 0 {
			format = values[0]
		}
	}
	if t := sub.tag(name); t != nil {
		if t.BaseType == "acceptance" {
			if strings.Join(sub.raw[name], "") != "" {
				return "Consented", true
			}
			return "Not consented", true
		}
		vals := sub.posted[name]
		if raw {
			vals = sub.raw[name]
		}
		out := strings.Join(vals, ", ")
		if format != "" && t.BaseType == "date" {
			if d, err := time.Parse("2006-01-02", out); err == nil {
				out = wp.FormatDate(d, format)
			}
		}
		return out, true
	}
	if strings.HasPrefix(name, "_") {
		return sub.specialMailTag(name)
	}
	return "", false
}

func (sub *gfSubmission) tag(name string) *gfTag {
	for _, t := range sub.tags {
		if t.Name == name && t.Name != "" && t.BaseType != "count" {
			return t
		}
	}
	return nil
}

// specialMailTag covers wpcf7_special_mail_tag(), the post, site and user related tags.
func (sub *gfSubmission) specialMailTag(name string) (string, bool) {
	s := sub.s
	switch name {
	case "_remote_ip":
		return sub.remoteIP, true
	case "_user_agent":
		return sub.userAgent, true
	case "_url":
		return sub.pageURL, true
	case "_date":
		return wp.FormatDate(sub.timestamp.In(s.loc()), s.settings.String("date_format")), true
	case "_time":
		return wp.FormatDate(sub.timestamp.In(s.loc()), s.settings.String("time_format")), true
	case "_invalid_fields":
		return strconv.Itoa(len(sub.invalid)), true
	case "_contact_form_title":
		return sub.form.Title, true
	case "_serial_number":
		var n int
		_ = s.store.Pool.QueryRow(sub.ctx, `SELECT coalesce((meta->>'_gontact_serial')::int, 0) FROM posts WHERE id = $1`, sub.form.ID).Scan(&n)
		return strconv.Itoa(n + 1), true
	case "_site_title":
		return s.settings.String("blogname"), true
	case "_site_description":
		return s.settings.String("blogdescription"), true
	case "_site_url":
		return s.links(sub.ctx).HomeURL("/"), true
	case "_site_domain":
		return hostOf(s.settings.String("home")), true
	case "_site_admin_email":
		return s.settings.String("admin_email"), true
	}
	if strings.HasPrefix(name, "_post_") {
		if sub.containerPost <= 0 {
			return "", true
		}
		p, err := s.store.GetPost(sub.ctx, sub.containerPost)
		if err != nil {
			return "", true
		}
		switch name {
		case "_post_id":
			return itoa64(p.ID), true
		case "_post_name":
			return p.Slug, true
		case "_post_title":
			return p.Title, true
		case "_post_url":
			return s.links(sub.ctx).Permalink(p), true
		case "_post_author", "_post_author_email":
			a, err := s.store.UserByID(sub.ctx, p.AuthorID)
			if err != nil {
				return "", true
			}
			if name == "_post_author" {
				return a.DisplayName, true
			}
			return a.Email, true
		}
		return "", true
	}
	if strings.HasPrefix(name, "_user_") {
		if sub.user == nil {
			return "", true
		}
		switch name {
		case "_user_login", "_user_email", "_user_url", "_user_display_name", "_user_nickname", "_user_first_name", "_user_last_name":
			return gfUserProp(sub.user, strings.TrimPrefix(name, "_")), true
		}
	}
	return "", false
}

// composeBody is the mail body with exclude_blank and the HTML template.
func (sub *gfSubmission) composeBody(m gfMail, subject string) (text, htmlBody string) {
	lines := strings.Split(strings.ReplaceAll(m.Body, "\r\n", "\n"), "\n")
	var plain, rich []string
	for _, line := range lines {
		var replaced []string
		p := sub.mailTags(line, false, &replaced)
		if m.ExcludeBlank && len(replaced) > 0 && strings.TrimSpace(strings.Join(replaced, "")) == "" {
			continue
		}
		plain = append(plain, p)
		if m.UseHTML {
			rich = append(rich, sub.mailTags(line, true, nil))
		}
	}
	text = strings.Join(plain, "\n")
	if m.UseHTML {
		htmlBody = "<!doctype html>\n<html xmlns=\"http://www.w3.org/1999/xhtml\" lang=\"" + strings.ReplaceAll(sub.form.Locale, "_", "-") + "\">\n<head>\n<title>" +
			html.EscapeString(subject) + "</title>\n</head>\n<body>\n" + wp.Autop(strings.Join(rich, "\n")) + "</body>\n</html>\n"
		text = wp.StripTags(text)
	}
	return text, htmlBody
}

// gontactVisitorReplyKind is the Message.Kind of a form mail whose Reply-To comes from a posted
// field (Mail's default "Reply-To: [your-email]"): the address the visitor typed. Privacy requests
// match such a message by its Reply-To; a fixed Reply-To (Mail (2)'s [_site_admin_email], a staff
// address) does not mark the mail as being about that address.
const gontactVisitorReplyKind = "gontact_visitor_reply"

// gfHasFieldTag reports whether a template holds a mail-tag of a posted field (not a special
// [_...] tag and not an escaped [[tag]]).
func gfHasFieldTag(v string) bool {
	for _, m := range gfMailTagRe.FindAllStringSubmatch(v, -1) {
		if m[1] == "[" && m[4] == "]" {
			continue
		}
		if !strings.HasPrefix(m[2], "_") {
			return true
		}
	}
	return false
}

// gfAddressList splits a comma list of addresses, keeping only valid ones.
func gfAddressList(v string) []string {
	var out []string
	for _, part := range strings.Split(v, ",") {
		if part = strings.TrimSpace(part); part == "" {
			continue
		}
		if a, err := mail.ParseAddress(part); err == nil {
			out = append(out, a.String())
		}
	}
	return out
}

// composeMail builds the message of one template; ok is false when it cannot be sent.
func (sub *gfSubmission) composeMail(m gfMail) (wpmail.Message, bool) {
	oneLine := func(v string) string {
		return strings.TrimSpace(strings.NewReplacer("\r", " ", "\n", " ").Replace(sub.mailTags(v, false, nil)))
	}
	subject := oneLine(m.Subject)
	msg := wpmail.Message{Subject: subject, To: gfAddressList(oneLine(m.Recipient))}
	if from := oneLine(m.Sender); from != "" {
		if a, err := mail.ParseAddress(from); err == nil {
			msg.From = a.String()
		}
	}
	// Split the template first and put each line on one line: a multi-line value ([your-message])
	// must not start header lines of its own (a visitor-chosen Bcc).
	for _, h := range strings.Split(strings.ReplaceAll(m.AdditionalHeaders, "\r", ""), "\n") {
		k, v, ok := strings.Cut(oneLine(h), ":")
		if !ok {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "reply-to":
			if list := gfAddressList(v); len(list) > 0 {
				msg.ReplyTo = list[0]
				if _, raw, _ := strings.Cut(h, ":"); gfHasFieldTag(raw) {
					msg.Kind = gontactVisitorReplyKind
				}
			}
		case "cc":
			msg.Cc = append(msg.Cc, gfAddressList(v)...)
		case "bcc":
			msg.Bcc = append(msg.Bcc, gfAddressList(v)...)
		}
	}
	msg.Body, msg.HTML = sub.composeBody(m, subject)
	msg.Attachments = sub.attachments(m.Attachments)
	return msg, len(msg.To) > 0
}

// attachments resolves the File attachments field: [file-tag] mail-tags attach the uploads, other
// lines are files relative to wp-content (only inside the uploads directory).
func (sub *gfSubmission) attachments(field string) []wpmail.Attachment {
	var out []wpmail.Attachment
	for _, line := range strings.Split(field, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		for _, m := range gfMailTagRe.FindAllStringSubmatch(line, -1) {
			for _, u := range sub.files[m[2]] {
				out = append(out, wpmail.Attachment{Name: u.name, ContentType: u.contentType, Data: u.data})
			}
		}
		rest := strings.TrimSpace(gfMailTagRe.ReplaceAllString(line, ""))
		if rel, ok := strings.CutPrefix(strings.TrimPrefix(rest, "/"), "uploads/"); ok && rest != "" {
			base := filepath.Clean(sub.s.cfg.UploadsDir)
			p := filepath.Join(base, filepath.Clean("/"+rel))
			if st, err := os.Stat(sub.s.gocdnFetchBack(sub.ctx, p)); err == nil && !st.IsDir() && st.Size() < 20<<20 && strings.HasPrefix(p, base+string(filepath.Separator)) {
				if data, err := os.ReadFile(p); err == nil {
					ct := mime.TypeByExtension(filepath.Ext(p))
					out = append(out, wpmail.Attachment{Name: filepath.Base(p), ContentType: ct, Data: data})
				}
			}
		}
	}
	return out
}

// mail is WPCF7_Submission::mail(): Mail, then Mail (2) when active. Messages are queued for the
// job runner like every wp_mail() of WordPress Go.
func (sub *gfSubmission) mail() bool {
	if sub.skipMail {
		return true
	}
	// Capped against mail floods (notifyMailAllowed): the entry is kept, marked as not mailed, and the
	// visitor gets the usual answer.
	if !sub.s.notifyMailAllowed("contact", sub.remoteIP) {
		if sub.entryID > 0 {
			sub.s.gontactEntryMailStatus(sub.ctx, sub.entryID, "skipped")
		}
		return true
	}
	msg, ok := sub.composeMail(sub.form.Mail)
	if !ok {
		return false
	}
	sub.s.mailOnConfiguredHome(sub.ctx, &msg)
	mailer := sub.s.mailer()
	if _, err := mailer.Build(msg); err != nil {
		slog.Warn("gontact form: mail", "form", sub.form.ID, "err", err)
		return false
	}
	if err := sub.s.queueJob(sub.ctx, "wp_mail", gontactMailJob{Message: msg, Entry: sub.entryID}, time.Time{}); err != nil {
		slog.Error("gontact form: queue mail", "err", err)
		return false
	}
	if sub.form.Mail2.Active {
		if m2, ok := sub.composeMail(sub.form.Mail2); ok {
			sub.s.mailOnConfiguredHome(sub.ctx, &m2)
			if _, err := mailer.Build(m2); err != nil {
				slog.Warn("gontact form: mail (2)", "form", sub.form.ID, "err", err)
			} else if err := sub.s.queueJob(sub.ctx, "wp_mail", m2, time.Time{}); err != nil {
				slog.Warn("gontact form: queue mail (2)", "form", sub.form.ID, "err", err)
			}
		}
	}
	return true
}
