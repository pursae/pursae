package server

import (
	"html"
	"html/template"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"

	"wpgo/internal/wp"
)

// Release notes: the core release notes the Hub index carries (hubCore.Notes) are the version's
// CHANGELOG.md section that tools/release.sh publishes, in Markdown. This is a small renderer for
// the subset that file uses: paragraphs, "#".."######" headings, "-" / "*" bullet lists whose items
// wrap onto indented continuation lines, fenced code, `inline code`, **bold** and [text](https://…)
// links. Every other character is text: entities in the source are decoded once and the text is
// escaped exactly once, so "editor's" shows as editor's, never as &#39; or &amp;#39;. The output
// goes through KsesPost as well.

// notesBlock is one block of the notes: a paragraph, a heading, a bullet list or fenced code.
type notesBlock struct {
	Kind  byte // 'p' paragraph, 'h' heading, 'l' list, 'c' code
	Level int  // heading level
	Text  string
	Items []string
}

// parseReleaseNotes splits Markdown notes into blocks. Lines of a paragraph or of a list item are
// joined with a space.
func parseReleaseNotes(md string) []notesBlock {
	md = strings.ReplaceAll(strings.ReplaceAll(md, "\r\n", "\n"), "\r", "\n")
	var (
		out      []notesBlock
		para     []string
		list     *notesBlock
		code     []string
		afterGap bool // a blank line since the last line of the list
	)
	flushPara := func() {
		if len(para) > 0 {
			out = append(out, notesBlock{Kind: 'p', Text: strings.Join(para, " ")})
			para = nil
		}
	}
	flushList := func() {
		if list != nil {
			out = append(out, *list)
			list = nil
		}
	}
	codeFence := 0
	for _, line := range strings.Split(md, "\n") {
		if code != nil {
			// A fence closes the block only when it is backticks alone, at least as long as the opening one.
			fence := strings.TrimSpace(line)
			if len(fence) >= codeFence && strings.Trim(fence, "`") == "" {
				out = append(out, notesBlock{Kind: 'c', Text: strings.Join(code, "\n")})
				code = nil
				continue
			}
			code = append(code, line)
			continue
		}
		trimmed := strings.TrimSpace(line)
		indent := len(line) - len(strings.TrimLeft(line, " \t"))
		switch {
		case trimmed == "":
			flushPara()
			afterGap = true
		case strings.HasPrefix(trimmed, "```"):
			flushPara()
			flushList()
			codeFence = len(trimmed) - len(strings.TrimLeft(trimmed, "`"))
			code = []string{}
		case indent < 4 && notesHeadingLevel(trimmed) > 0:
			flushPara()
			flushList()
			lvl := notesHeadingLevel(trimmed)
			title := trimmed[lvl:]
			if before := strings.TrimRight(title, "#"); before != title &&
				(strings.HasSuffix(before, " ") || strings.HasSuffix(before, "\t")) {
				title = before
			}
			out = append(out, notesBlock{Kind: 'h', Level: lvl, Text: strings.TrimSpace(title)})
		case notesBullet(trimmed) && (list != nil || indent < 4):
			flushPara()
			if list == nil {
				list = &notesBlock{Kind: 'l'}
			}
			list.Items = append(list.Items, strings.TrimSpace(trimmed[2:]))
			afterGap = false
		case list != nil && (indent > 0 || !afterGap):
			// A continuation line of the last item: indented, or lazily right below it.
			list.Items[len(list.Items)-1] += " " + trimmed
			afterGap = false
		default:
			flushList()
			para = append(para, trimmed)
		}
	}
	if code != nil {
		out = append(out, notesBlock{Kind: 'c', Text: strings.Join(code, "\n")})
	}
	flushPara()
	flushList()
	return out
}

// notesHeadingLevel is the level of an ATX heading line ("### Added"), 0 for any other line.
func notesHeadingLevel(s string) int {
	n := 0
	for n < len(s) && s[n] == '#' {
		n++
	}
	if n == 0 || n > 6 || (n < len(s) && s[n] != ' ' && s[n] != '\t') {
		return 0
	}
	return n
}

func notesBullet(s string) bool {
	return len(s) >= 2 && (s[0] == '-' || s[0] == '*' || s[0] == '+') && (s[1] == ' ' || s[1] == '\t')
}

// notesInline is one inline piece of a paragraph, heading or item: plain text, `code`, **bold**
// or a link. Bold and link text are plain text (with any backticks inside shown as code).
type notesInline struct {
	Kind byte // 't' text, 'c' code, 'b' bold, 'a' link
	Text string
	Href string // links: an absolute http(s) URL
}

// parseNotesInline splits inline Markdown into pieces. Entities in text are decoded here (once);
// code spans keep their content as written.
func parseNotesInline(s string) []notesInline {
	var out []notesInline
	var text strings.Builder
	flush := func() {
		if text.Len() > 0 {
			out = append(out, notesInline{Kind: 't', Text: html.UnescapeString(text.String())})
			text.Reset()
		}
	}
	for i := 0; i < len(s); {
		switch {
		case s[i] == '\\' && i+1 < len(s) && strings.IndexByte("\\`*_[]()#+-.!<>", s[i+1]) >= 0:
			text.WriteByte(s[i+1])
			i += 2
			continue
		case s[i] == '`':
			n := 0
			for i+n < len(s) && s[i+n] == '`' {
				n++
			}
			fence := s[i : i+n]
			if end := notesCodeSpanEnd(s[i+n:], n); end >= 0 {
				flush()
				inner := s[i+n : i+n+end]
				if t := strings.TrimSpace(inner); t != "" {
					inner = t
				}
				out = append(out, notesInline{Kind: 'c', Text: inner})
				i += n + end + n
				continue
			}
			text.WriteString(fence)
			i += n
			continue
		case strings.HasPrefix(s[i:], "**") || strings.HasPrefix(s[i:], "__"):
			mark := s[i : i+2]
			if end := strings.Index(s[i+2:], mark); end > 0 {
				flush()
				out = append(out, notesInline{Kind: 'b', Text: html.UnescapeString(s[i+2 : i+2+end])})
				i += 2 + end + 2
				continue
			}
		case s[i] == '[':
			if label, href, n, ok := notesLink(s[i:]); ok {
				flush()
				label = html.UnescapeString(label)
				if u := notesSafeURL(href); u != "" {
					out = append(out, notesInline{Kind: 'a', Text: label, Href: u})
				} else {
					out = append(out, notesInline{Kind: 't', Text: label}) // a relative or unsafe link keeps its text only
				}
				i += n
				continue
			}
		}
		text.WriteByte(s[i])
		i++
	}
	flush()
	return mergeNotesText(out)
}

// notesCodeSpanEnd finds the next complete backtick run matching the opening delimiter.
func notesCodeSpanEnd(s string, width int) int {
	for i := 0; i < len(s); {
		next := strings.IndexByte(s[i:], '`')
		if next < 0 {
			break
		}
		start := i + next
		i = start + 1
		for i < len(s) && s[i] == '`' {
			i++
		}
		if i-start == width {
			return start
		}
	}
	return -1
}

// mergeNotesText joins neighbouring text pieces.
func mergeNotesText(in []notesInline) []notesInline {
	var out []notesInline
	for _, p := range in {
		if p.Kind == 't' && len(out) > 0 && out[len(out)-1].Kind == 't' {
			out[len(out)-1].Text += p.Text
			continue
		}
		out = append(out, p)
	}
	return out
}

// notesLink parses "[label](href)" at the start of s: the label, the destination and the length.
func notesLink(s string) (label, href string, n int, ok bool) {
	depth := 0
	for i := 1; i < len(s); i++ {
		switch s[i] {
		case '[':
			depth++
		case ']':
			if depth > 0 {
				depth--
				continue
			}
			if i+1 >= len(s) || s[i+1] != '(' {
				return "", "", 0, false
			}
			end, open := -1, 0 // the ")" that closes the destination; parentheses inside balance
			var titleQuote byte
			for j := i + 2; j < len(s) && end < 0; j++ {
				ch := s[j]
				if titleQuote != 0 {
					switch ch {
					case '\\':
						j++
					case titleQuote:
						titleQuote = 0
					}
					continue
				}
				if open == 0 && j > i+2 && (ch == '\'' || ch == '"') && (s[j-1] == ' ' || s[j-1] == '\t') {
					titleQuote = ch
					continue
				}
				switch ch {
				case '(':
					open++
				case ')':
					if open == 0 {
						end = j - (i + 2)
					}
					open--
				}
			}
			if end < 0 {
				return "", "", 0, false
			}
			dest := strings.TrimSpace(s[i+2 : i+2+end])
			if sp := strings.IndexAny(dest, " \t"); sp >= 0 { // [x](url "title")
				dest = dest[:sp]
			}
			return s[1:i], strings.Trim(dest, "<>"), i + 2 + end + 1, true
		}
	}
	return "", "", 0, false
}

// notesSafeURL returns an absolute http(s) URL as is, "" for any other destination (javascript:,
// data:, relative paths).
func notesSafeURL(raw string) string {
	raw = strings.TrimSpace(html.UnescapeString(raw))
	if strings.ContainsAny(raw, " \t\r\n\x00") {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return ""
	}
	return u.String()
}

// notesEscape escapes text for an element's content: &, < and >, nothing else, so the result has
// every character escaped exactly once.
func notesEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// renderNotesInline writes inline pieces as HTML.
func renderNotesInline(b *strings.Builder, parts []notesInline) {
	for _, p := range parts {
		switch p.Kind {
		case 'c':
			b.WriteString("<code>" + notesEscape(p.Text) + "</code>")
		case 'b':
			b.WriteString("<strong>")
			renderNotesInline(b, codeOnly(p.Text))
			b.WriteString("</strong>")
		case 'a':
			b.WriteString(`<a href="` + html.EscapeString(p.Href) + `" target="_blank" rel="noopener noreferrer">`)
			renderNotesInline(b, codeOnly(p.Text))
			b.WriteString("</a>")
		default:
			b.WriteString(notesEscape(p.Text))
		}
	}
}

// codeOnly splits bold or link text into text and code pieces (no nested links or bold).
func codeOnly(s string) []notesInline {
	var out []notesInline
	for s != "" {
		i := strings.IndexByte(s, '`')
		if i < 0 {
			break
		}
		j := strings.IndexByte(s[i+1:], '`')
		if j < 0 {
			break
		}
		if i > 0 {
			out = append(out, notesInline{Kind: 't', Text: s[:i]})
		}
		out = append(out, notesInline{Kind: 'c', Text: s[i+1 : i+1+j]})
		s = s[i+1+j+1:]
	}
	if s != "" {
		out = append(out, notesInline{Kind: 't', Text: s})
	}
	return out
}

// notesInlineHTML renders one line of inline Markdown.
func notesInlineHTML(s string) string {
	var b strings.Builder
	renderNotesInline(&b, parseNotesInline(s))
	return b.String()
}

// renderReleaseNotes renders the full notes as sanitised HTML. Headings start at <h3> (the notes sit
// under the screen's <h1>/<h2>): "#", "##" and "###" are <h3>, "####" and deeper <h4>.
//
// A Keep a Changelog section heading ("Added", "Fixed", "Security"...) carries its colour (the
// Dashboard digest's), and its items are cards in that colour: an item that starts with a bold title
// is a closed <details> whose summary is the title, so the screen lists the titles and a click shows
// the rest.
func renderReleaseNotes(md string) template.HTML {
	var b strings.Builder
	cls := "other" // the section of the lists that follow
	for _, bl := range parseReleaseNotes(md) {
		switch bl.Kind {
		case 'h':
			if bl.Level >= 4 {
				b.WriteString("<h4>" + notesInlineHTML(bl.Text) + "</h4>\n")
				continue
			}
			cls = notesSectionClass(bl.Text)
			b.WriteString(`<h3 class="pursae-notes-section is-` + cls + `"><span class="pursae-news-label">` + notesInlineHTML(bl.Text) + "</span></h3>\n")
		case 'l':
			b.WriteString(`<ul class="pursae-notes-cards is-` + cls + `">` + "\n")
			for _, it := range bl.Items {
				b.WriteString("<li>" + notesCardHTML(it) + "</li>\n")
			}
			b.WriteString("</ul>\n")
		case 'c':
			b.WriteString("<pre><code>" + notesEscape(bl.Text) + "</code></pre>\n")
		default:
			b.WriteString("<p>" + notesInlineHTML(bl.Text) + "</p>\n")
		}
	}
	return template.HTML(wp.KsesPost(b.String())) //nolint:gosec // built from escaped text above and sanitised by KsesPost
}

// notesCardHTML renders one list item: "**Title.** details" as a closed <details> card with the title
// as its summary; an item without a leading bold title, or with nothing after it, as plain text.
func notesCardHTML(item string) string {
	parts := parseNotesInline(item)
	for len(parts) > 0 && parts[0].Kind == 't' && strings.TrimSpace(parts[0].Text) == "" {
		parts = parts[1:]
	}
	if len(parts) < 2 || parts[0].Kind != 'b' {
		return notesInlineHTML(item)
	}
	rest := parts[1:]
	if rest[0].Kind == 't' {
		rest = append([]notesInline{{Kind: 't', Text: strings.TrimLeft(rest[0].Text, " \t")}}, rest[1:]...)
	}
	var title, body strings.Builder
	renderNotesInline(&title, parts[:1])
	renderNotesInline(&body, rest)
	if strings.TrimSpace(body.String()) == "" {
		return title.String()
	}
	return `<details class="pursae-note"><summary>` + title.String() + `</summary><div class="pursae-note-body">` + body.String() + "</div></details>"
}

// notesDigest is the short form of the notes for the Dashboard: the summary (the first paragraph
// before any heading) and a few items, each with its section ("Added", "Security"...).
type notesDigest struct {
	Summary template.HTML
	Groups  []notesDigestGroup
	More    int // items left out of the digest
}

type notesDigestGroup struct {
	Label, Class string // Class: added, changed, fixed, security, deprecated, removed or other
	Items        []template.HTML
}

// Digest limits.
const (
	notesDigestItems   = 4
	notesSummaryRunes  = 280
	notesDigestItemLen = 150
)

// releaseNotesDigest builds the Dashboard digest: up to max items, taken round-robin from the
// sections (the first item of each, then the second...) so every section shows, and listed in the
// order of the notes. Each item is cut at a sentence end, or else at a word with an ellipsis.
func releaseNotesDigest(md string, max int) *notesDigest {
	type section struct {
		label string
		items []string
	}
	var (
		secs    []section
		summary string
		total   int
	)
	cur := -1
	hasSectionHeading := false
	for _, bl := range parseReleaseNotes(md) {
		switch bl.Kind {
		case 'h':
			if bl.Level >= 4 && hasSectionHeading {
				continue // full release notes also keep subheadings within their parent section
			}
			hasSectionHeading = bl.Level < 4
			secs = append(secs, section{label: bl.Text})
			cur = len(secs) - 1
		case 'p':
			if summary == "" && cur < 0 {
				summary = bl.Text
			}
		case 'l':
			if cur < 0 {
				secs = append(secs, section{})
				cur = len(secs) - 1
			}
			secs[cur].items = append(secs[cur].items, bl.Items...)
			total += len(bl.Items)
		}
	}
	d := &notesDigest{}
	if summary != "" {
		d.Summary = template.HTML(wp.KsesPost(trimNotesInline(parseNotesInline(summary), notesSummaryRunes))) //nolint:gosec // escaped and sanitised
	}
	picked := make([]int, len(secs))
	for n, round := 0, 0; n < max; round++ {
		added := false
		for i := range secs {
			if n < max && round < len(secs[i].items) {
				picked[i]++
				n++
				added = true
			}
		}
		if !added {
			break
		}
	}
	shown := 0
	for i, sec := range secs {
		if picked[i] == 0 {
			continue
		}
		g := notesDigestGroup{Label: sec.label, Class: notesSectionClass(sec.label)}
		for _, it := range sec.items[:picked[i]] {
			g.Items = append(g.Items, template.HTML(wp.KsesPost(trimNotesInline(parseNotesInline(it), notesDigestItemLen)))) //nolint:gosec // escaped and sanitised
		}
		shown += picked[i]
		d.Groups = append(d.Groups, g)
	}
	d.More = total - shown
	if d.Summary == "" && len(d.Groups) == 0 {
		return nil
	}
	return d
}

// notesStopWords are not left at the end of a cut item.
var notesStopWords = map[string]bool{"a": true, "an": true, "and": true, "the": true, "of": true, "to": true, "in": true,
	"on": true, "or": true, "for": true, "with": true, "at": true, "by": true, "as": true, "from": true, "into": true}

// notesSectionClass maps a Keep a Changelog section to its label colour.
func notesSectionClass(label string) string {
	switch l := strings.ToLower(strings.TrimSpace(label)); l {
	case "added", "changed", "fixed", "security", "deprecated", "removed":
		return l
	}
	return "other"
}

// trimNotesInline renders inline pieces as HTML, cut to about limit characters of text: at the end
// of the first sentence when it fits (no ellipsis), else after the last whole word that fits,
// followed by "…". Code spans, bold and links are never cut in the middle.
func trimNotesInline(parts []notesInline, limit int) string {
	plainLen := 0
	for _, p := range parts {
		plainLen += utf8.RuneCountInString(p.Text)
	}
	render := func(ps []notesInline) string {
		var b strings.Builder
		renderNotesInline(&b, ps)
		return b.String()
	}
	if plainLen <= limit {
		return render(parts)
	}
	// cutAt returns the pieces up to byte offset off of text piece i.
	cutAt := func(i, off int) []notesInline {
		out := append([]notesInline{}, parts[:i]...)
		return append(out, notesInline{Kind: 't', Text: parts[i].Text[:off]})
	}
	// The end of the first sentence within the limit.
	pos := 0
	for i, p := range parts {
		if p.Kind == 't' {
			r := 0
			for off, c := range p.Text {
				if pos+r+1 > limit {
					break
				}
				if (c == '.' || c == '!' || c == '?') && pos+r >= 40 {
					// A sentence ends at ". " before a word that is not lower case ("e.g. the"
					// goes on).
					if next := p.Text[off+1:]; len(next) >= 2 && next[0] == ' ' {
						if r, _ := utf8.DecodeRuneInString(next[1:]); !unicode.IsLower(r) {
							return render(cutAt(i, off+1))
						}
					} else if next == " " && i+1 < len(parts) {
						return render(cutAt(i, off+1)) // ". `code`…"
					}
				}
				r++
			}
		}
		pos += utf8.RuneCountInString(p.Text)
		if pos >= limit {
			break
		}
	}
	// Else the last word boundary within the limit (an atomic piece that does not fit ends it).
	pos = 0
	bestPiece, bestOff := -1, 0
	for i, p := range parts {
		n := utf8.RuneCountInString(p.Text)
		if p.Kind != 't' {
			if pos+n > limit {
				break
			}
			pos += n
			bestPiece, bestOff = i+1, -1 // after this whole piece
			continue
		}
		r := 0
		for off, c := range p.Text {
			if pos+r > limit {
				break
			}
			if unicode.IsSpace(c) {
				bestPiece, bestOff = i, off
			}
			r++
		}
		if pos+n > limit {
			break
		}
		pos += n
	}
	var kept []notesInline
	switch {
	case bestPiece < 0:
		kept = []notesInline{parts[0]}
		if kept[0].Kind == 't' {
			// Keep the first word intact even when it exceeds the limit.
			if off := strings.IndexFunc(kept[0].Text, unicode.IsSpace); off > 0 {
				kept[0].Text = kept[0].Text[:off]
			}
		}
	case bestOff < 0:
		kept = parts[:bestPiece]
	default:
		kept = cutAt(bestPiece, bestOff)
	}
	// No dangling punctuation, space or little word ("and a…") before the ellipsis.
	if n := len(kept); n > 0 && kept[n-1].Kind == 't' {
		trim := func(t string) string {
			return strings.TrimRightFunc(t, func(r rune) bool { return unicode.IsSpace(r) || strings.ContainsRune(",;:([{-–—", r) })
		}
		t := trim(kept[n-1].Text)
		for range 3 {
			i := strings.LastIndexByte(t, ' ')
			if i <= 0 || !notesStopWords[strings.ToLower(t[i+1:])] {
				break
			}
			t = trim(t[:i])
		}
		kept[n-1].Text = t
	}
	return render(kept) + "…"
}
