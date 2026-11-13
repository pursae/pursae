package wp

import (
	"html"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

var accentMap = map[rune]string{
	'ı': "i", 'İ': "I", 'ş': "s", 'Ş': "S", 'ğ': "g", 'Ğ': "G", 'ç': "c", 'Ç': "C", 'ö': "o", 'Ö': "O",
	'ü': "u", 'Ü': "U", 'ß': "ss", 'æ': "ae", 'Æ': "AE", 'ø': "o", 'Ø': "O", 'œ': "oe", 'Œ': "OE",
	'đ': "d", 'Đ': "D", 'ł': "l", 'Ł': "L", 'þ': "th", 'Þ': "TH", 'ð': "d", 'Ð': "D",
}

// RemoveAccents is remove_accents(): maps accented Latin letters to ASCII. Only marks on Latin letters
// are dropped; other scripts keep theirs (a Devanagari vowel sign or virama is part of the letter).
func RemoveAccents(s string) string {
	var sb strings.Builder
	var base rune
	for _, r := range norm.NFD.String(s) {
		if rep, ok := accentMap[r]; ok {
			sb.WriteString(rep)
			base = 'a'
			continue
		}
		if unicode.Is(unicode.Mn, r) {
			if unicode.Is(unicode.Latin, base) {
				continue
			}
		} else {
			base = r
		}
		sb.WriteRune(r)
	}
	return norm.NFC.String(sb.String())
}

var (
	// Quoted attribute values may contain '>'. The last alternative keeps the old behaviour for
	// malformed tags (an unclosed quote), so they are still stripped instead of passing through.
	tagRe        = regexp.MustCompile(`(?s)<!--.*?-->|<(?:"[^"]*"|'[^']*'|[^'">])*>|<[^>]*>`)
	rawContentRe = regexp.MustCompile(`(?is)<script\b[^>]*>.*?</script\s*>|<style\b[^>]*>.*?</style\s*>`)
	entityRe     = regexp.MustCompile(`&[^;\s]+;`)
	dashesRe     = regexp.MustCompile(`-+`)
	pctOctetRe   = regexp.MustCompile(`%[0-9A-Fa-f]{2}`)
	slugReplace  = strings.NewReplacer(".", "-", "/", "-", " ", "-", "\u00a0", "-", "–", "-", "—", "-")
)

// SanitizeTitle is sanitize_title_with_dashes(): a lowercase, dash-separated slug. Latin accents become
// ASCII; letters, digits and combining marks of other scripts are kept (Cyrillic, Greek, Arabic, CJK …).
// WordPress stores those as lowercase %xx octets; Pursae stores the decoded UTF-8, which is what the
// router sees after the request path is unescaped, and the link builders escape it again. Existing %xx
// sequences (WXR post_name, category_nicename) are decoded first.
func SanitizeTitle(title string) string {
	s := StripTags(title)
	s = entityRe.ReplaceAllString(s, "")
	if pctOctetRe.MatchString(s) {
		if dec, err := url.PathUnescape(s); err == nil && utf8.ValidString(dec) {
			s = dec
		}
	}
	s = RemoveAccents(s)
	s = strings.ToLower(s)
	s = slugReplace.Replace(s)
	s = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '-':
			return r
		case r < 0x80:
			return -1
		case unicode.IsLetter(r), unicode.IsNumber(r), unicode.In(r, unicode.Mn, unicode.Mc):
			return r
		}
		return -1
	}, s)
	s = dashesRe.ReplaceAllString(s, "-")
	return strings.Trim(s, "-")
}

// StripTags is wp_strip_all_tags() without whitespace collapsing.
func StripTags(s string) string {
	s = rawContentRe.ReplaceAllString(s, "")
	return tagRe.ReplaceAllString(s, "")
}

// TrimWords is wp_trim_words(): the first n words of the text content followed by more.
func TrimWords(text string, n int, more string) string {
	if n < 0 {
		n = 0
	}
	text = html.UnescapeString(StripTags(text))
	words := strings.Fields(text)
	if len(words) <= n {
		return html.EscapeString(strings.Join(words, " "))
	}
	return html.EscapeString(strings.Join(words[:n], " ")) + more
}

// moreTagRe is the <!--more--> tag with its optional custom link text.
var moreTagRe = regexp.MustCompile(`<!--more(.*?)?-->`)

// SplitMore splits post content at the <!--more--> tag. teaser is the part before
// it, or the whole content when there is no tag; hasMore reports whether it was found.
func SplitMore(content string) (teaser string, rest string, moreText string, hasMore bool) {
	if !strings.Contains(content, "<!--more") {
		return content, "", "", false
	}
	loc := moreTagRe.FindStringSubmatchIndex(content)
	if loc == nil {
		return content, "", "", false
	}
	moreText = strings.TrimSpace(content[loc[2]:loc[3]])
	return content[:loc[0]], content[loc[1]:], moreText, true
}

var (
	preBlockRe   = regexp.MustCompile(`(?is)<pre[^>]*>.*?</pre>`)
	blockElems   = `(?:table|thead|tfoot|caption|col|colgroup|tbody|tr|td|th|div|dl|dd|dt|ul|ol|li|pre|form|map|area|blockquote|address|math|style|p|h[1-6]|hr|fieldset|legend|section|article|aside|hgroup|header|footer|nav|figure|figcaption|details|menu|summary)`
	openBlockRe  = regexp.MustCompile(`(?i)(<` + blockElems + `[\s/>])`)
	closeBlockRe = regexp.MustCompile(`(?i)(</` + blockElems + `>)`)
	pEmptyRe     = regexp.MustCompile(`<p>\s*</p>`)
	pAroundOpen  = regexp.MustCompile(`(?i)<p>\s*(</?` + blockElems + `[^>]*>)`)
	pAroundClose = regexp.MustCompile(`(?i)(</?` + blockElems + `[^>]*>)\s*</p>`)
	brAfterBlock = regexp.MustCompile(`(?i)(</?` + blockElems + `[^>]*>)\s*<br />`)
	brBeforeBlk  = regexp.MustCompile(`(?i)<br />(\s*</?(?:p|li|div|dl|dd|dt|th|pre|td|ul|ol)[^>]*>)`)
	multiNL      = regexp.MustCompile(`\n\s*\n`)
)

// Autop is a compact port of wpautop(): double line breaks become paragraphs,
// single ones become <br />, block-level elements are left unwrapped.
func Autop(text string) string {
	if strings.TrimSpace(text) == "" {
		return ""
	}
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n") + "\n"

	// Keep <pre> contents untouched.
	var pres []string
	text = preBlockRe.ReplaceAllStringFunc(text, func(m string) string {
		pres = append(pres, m)
		return "<pre wp-pre-tag-" + strconv.Itoa(len(pres)-1) + "></pre>"
	})

	text = openBlockRe.ReplaceAllString(text, "\n\n$1")
	text = closeBlockRe.ReplaceAllString(text, "$1\n\n")
	// wp_replace_in_html_tags( $text, array( "\n" => ' <!-- wpnl --> ' ) ): a newline inside a tag
	// or comment neither splits a paragraph nor becomes a <br />.
	text = protectTagNewlines(text)
	if strings.Contains(text, "<option") {
		text = optionBeforeRe.ReplaceAllString(text, "<option")
		text = optionAfterRe.ReplaceAllString(text, "</option>")
	}
	text = multiNL.ReplaceAllString(text, "\n\n")

	var sb strings.Builder
	for _, chunk := range strings.Split(text, "\n\n") {
		chunk = strings.TrimSpace(chunk)
		if chunk == "" {
			continue
		}
		sb.WriteString("<p>" + chunk + "</p>\n")
	}
	out := sb.String()
	out = pEmptyRe.ReplaceAllString(out, "")
	out = pAroundOpen.ReplaceAllString(out, "$1")
	out = pAroundClose.ReplaceAllString(out, "$1")
	// Single newlines inside paragraphs become line breaks, except inside <script>, <style>, <svg>
	// and <math> (_autop_newline_preservation_helper()).
	out = autopPreserveRe.ReplaceAllStringFunc(out, func(m string) string { return strings.ReplaceAll(m, "\n", autopNL) })
	out = regexp.MustCompile(`(?s)<p>(.*?)</p>`).ReplaceAllStringFunc(out, func(p string) string {
		inner := p[3 : len(p)-4]
		return "<p>" + strings.ReplaceAll(strings.TrimSpace(inner), "\n", "<br />\n") + "</p>"
	})
	out = brAfterBlock.ReplaceAllString(out, "$1")
	out = brBeforeBlk.ReplaceAllString(out, "$1")

	for i, pre := range pres {
		out = strings.Replace(out, "<pre wp-pre-tag-"+strconv.Itoa(i)+"></pre>", pre, 1)
	}
	return strings.ReplaceAll(out, autopNL, "\n")
}

// autopNL stands in for a newline that Autop must keep as it is.
const autopNL = "\x1ewpnl\x1e"

var (
	optionBeforeRe  = regexp.MustCompile(`\s*<option`)
	optionAfterRe   = regexp.MustCompile(`</option>\s*`)
	autopPreserveRe = regexp.MustCompile(`(?is)<(?:script|style|svg|math)\b.*?</(?:script|style|svg|math)>`)
)

// protectTagNewlines swaps the newlines inside tags and comments for autopNL. Quoted attribute
// values are skipped as a browser does (htmlElementEnd), so a ">" inside one does not end the tag.
func protectTagNewlines(text string) string {
	if !strings.Contains(text, "<") {
		return text
	}
	var b strings.Builder
	b.Grow(len(text))
	for i := 0; i < len(text); {
		lt := strings.IndexByte(text[i:], '<')
		if lt < 0 {
			b.WriteString(text[i:])
			break
		}
		b.WriteString(text[i : i+lt])
		i += lt
		end, kind := htmlElementEnd(text, i)
		if kind == elemText {
			b.WriteByte('<')
			i++
			continue
		}
		b.WriteString(strings.ReplaceAll(text[i:end], "\n", autopNL))
		i = end
	}
	return b.String()
}

// EscText escapes <, > and & but leaves quotes alone so Texturize can still curl them.
func EscText(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// Texturize is a small subset of wptexturize(): curly quotes, dashes and ellipses in plain text.
func Texturize(s string) string {
	if strings.ContainsAny(s, "<>") {
		return s
	}
	r := strings.NewReplacer(" -- ", " &#8212; ", "---", "&#8212;", " - ", " &#8211; ", "--", "&#8211;", "...", "&#8230;")
	s = r.Replace(s)
	s = regexp.MustCompile(`(^|[\s(\[{])'`).ReplaceAllString(s, "$1&#8216;")
	s = regexp.MustCompile(`'`).ReplaceAllString(s, "&#8217;")
	s = regexp.MustCompile(`(^|[\s(\[{])"`).ReplaceAllString(s, "$1&#8220;")
	s = regexp.MustCompile(`"`).ReplaceAllString(s, "&#8221;")
	return s
}
