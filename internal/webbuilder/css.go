package webbuilder

import (
	"errors"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"
)

// CSS sanitizer (contract §8.3). The editor sends page CSS (Tailwind utilities plus section custom
// CSS) and inline style attributes; both are untrusted. The sanitizer keeps the text of every rule
// and declaration it allows (comments removed) and drops the rest:
//
//   - at-rules outside CSSAllowedAtRules (@import, @charset, @namespace, …);
//   - declarations that, after CSS unescaping and whitespace removal, contain one of
//     CSSForbiddenTokens (expression(, behavior, -moz-binding, javascript:, vbscript:, …);
//   - declarations with a url() (or an image-set()/image()/src() string) that is not http, https,
//     relative, a same-document "#id" or a data:image/* URI of at most MaxDataURIBytes;
//   - @font-face src URLs outside CSSFontFacePrefix.
//
// "</style" anywhere is an error (ErrCSSStyleTag), never silently fixed, because it can only come
// from a forged request.

// ErrCSSStyleTag is returned when CSS contains "</style" (422 bad_css).
var ErrCSSStyleTag = errors.New("webbuilder: css contains </style")

// ErrCSSTooLarge is returned when the CSS is above MaxCSSBytes (413).
var ErrCSSTooLarge = errors.New("webbuilder: css too large")

// CSSOptions tune SanitizeCSS.
type CSSOptions struct {
	// SiteOrigin ("https://example.com") lets absolute @font-face URLs on the site itself through;
	// relative ones are always accepted.
	SiteOrigin string
}

const cssMaxNesting = 24

// SanitizeCSS cleans a stylesheet. changed reports whether anything besides comments and
// whitespace between rules was removed.
func SanitizeCSS(css string, opts CSSOptions) (out string, changed bool, err error) {
	if len(css) > MaxCSSBytes {
		return "", false, ErrCSSTooLarge
	}
	if hasStyleClose(css) {
		return "", false, ErrCSSStyleTag
	}
	css = strings.ToValidUTF8(css, "�")
	src, _ := stripCSSComments(css)
	c := &cssCleaner{opts: opts}
	out = c.ruleList(src, 0, ctxStylesheet)
	return out, c.changed, nil
}

// SanitizeStyleAttr cleans the value of a style attribute (a declaration list). It never fails:
// "</style" in an attribute is harmless once the attribute is escaped, the declaration is dropped
// all the same.
func SanitizeStyleAttr(style string) (string, bool) {
	style = strings.ToValidUTF8(style, "�")
	src, _ := stripCSSComments(style)
	c := &cssCleaner{}
	var parts []string
	for _, it := range splitCSSItems(src) {
		if it.block || it.stray {
			c.changed = true
			continue
		}
		d := strings.TrimSpace(it.text)
		if d == "" {
			continue
		}
		if !c.declOK(d, false) {
			c.changed = true
			continue
		}
		parts = append(parts, d)
	}
	return strings.Join(parts, ";"), c.changed
}

// CSSValueOK reports whether v is safe as the value of one CSS property (doc fields such as
// ElStyle.Border, ElStyle.BgImage or a color). React sets these through the CSSOM, but the same
// rules as for stylesheets apply so a forged doc never stores a hostile value.
func CSSValueOK(v string) bool {
	if v == "" {
		return true
	}
	if hasStyleClose(v) {
		return false
	}
	items := splitCSSItems("x:" + v)
	if len(items) != 1 || items[0].block || items[0].stray {
		return false
	}
	c := &cssCleaner{}
	return c.declOK(items[0].text, false)
}

// BorderSidesOK accepts the per-side form of ElStyle.Border the editor reads (extraCss):
// "border-top:1px solid #ccc;border-bottom:2px dashed red", each side a safe CSS value.
func BorderSidesOK(v string) bool {
	if hasStyleClose(v) || !strings.Contains(v, ":") {
		return false
	}
	for _, part := range strings.Split(v, ";") {
		k, val, ok := strings.Cut(part, ":")
		if !ok {
			return false
		}
		switch strings.TrimSpace(k) {
		case "border-top", "border-right", "border-bottom", "border-left":
		default:
			return false
		}
		if val = strings.TrimSpace(val); val == "" || !CSSValueOK(val) {
			return false
		}
	}
	return true
}

func hasStyleClose(s string) bool {
	return strings.Contains(strings.ToLower(s), "</style")
}

type cssCtx int

const (
	ctxStylesheet cssCtx = iota // rule list (top level, @media, @supports, …)
	ctxDecls                    // declarations plus nested rules (CSS nesting)
	ctxKeyframes                // keyframe selectors, each with declarations
	ctxFontFace                 // declarations only, src restricted
	ctxDeclsOnly                // declarations only (@property)
)

type cssCleaner struct {
	opts    CSSOptions
	changed bool
}

func (c *cssCleaner) ruleList(src string, depth int, ctx cssCtx) string {
	if depth > cssMaxNesting {
		c.changed = true
		return ""
	}
	var b strings.Builder
	for _, it := range splitCSSItems(src) {
		text := strings.TrimSpace(it.text)
		if it.stray {
			c.changed = true
			continue
		}
		if !it.block {
			if text == "" {
				continue
			}
			if text[0] == '@' {
				// Statement at-rules: only "@layer a, b;" survives.
				name, _ := atRuleName(text)
				if name == "layer" && !strings.ContainsAny(text, "(\\") && c.preludeOK(text) {
					b.WriteString(text)
					b.WriteByte(';')
					continue
				}
				c.changed = true
				continue
			}
			switch ctx {
			case ctxDecls, ctxFontFace, ctxDeclsOnly:
				if c.declOK(text, ctx == ctxFontFace) {
					b.WriteString(text)
					b.WriteByte(';')
				} else {
					c.changed = true
				}
			default:
				c.changed = true // stray text in a rule list
			}
			continue
		}
		prelude := strings.TrimSpace(it.text)
		body := it.body
		if prelude == "" {
			c.changed = true
			continue
		}
		if prelude[0] == '@' {
			name, _ := atRuleName(prelude)
			if !c.atRuleAllowed(name) || !c.preludeOK(prelude) || ctx == ctxFontFace || ctx == ctxDeclsOnly || ctx == ctxKeyframes {
				c.changed = true
				continue
			}
			var inner string
			switch name {
			case "keyframes", "-webkit-keyframes":
				inner = c.ruleList(body, depth+1, ctxKeyframes)
			case "font-face":
				inner = c.ruleList(body, depth+1, ctxFontFace)
			case "property":
				inner = c.ruleList(body, depth+1, ctxDeclsOnly)
			default: // media, container, supports, layer
				if ctx == ctxDecls {
					inner = c.ruleList(body, depth+1, ctxDecls)
				} else {
					inner = c.ruleList(body, depth+1, ctxStylesheet)
				}
			}
			b.WriteString(prelude)
			b.WriteByte('{')
			b.WriteString(inner)
			b.WriteByte('}')
			continue
		}
		if ctx == ctxFontFace || ctx == ctxDeclsOnly {
			c.changed = true
			continue
		}
		if !c.preludeOK(prelude) {
			c.changed = true
			continue
		}
		inner := ""
		if ctx == ctxKeyframes {
			inner = c.ruleList(body, depth+1, ctxDeclsOnly)
		} else {
			inner = c.ruleList(body, depth+1, ctxDecls)
		}
		b.WriteString(prelude)
		b.WriteByte('{')
		b.WriteString(inner)
		b.WriteByte('}')
	}
	return b.String()
}

func (c *cssCleaner) atRuleAllowed(name string) bool {
	if name == "-webkit-keyframes" {
		return true
	}
	for _, a := range CSSAllowedAtRules {
		if a == name {
			return true
		}
	}
	return false
}

// preludeOK checks a selector or an at-rule prelude: no forbidden tokens, no URLs.
func (c *cssCleaner) preludeOK(p string) bool {
	if !cssBalanced(p) {
		return false
	}
	n := cssNormalize(p)
	for _, t := range CSSForbiddenTokens {
		if strings.Contains(n, t) {
			return false
		}
	}
	return !strings.Contains(n, "url(") && !strings.Contains(n, "image-set(")
}

var cssPropName = func(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	if strings.HasPrefix(s, "--") {
		for _, r := range s[2:] {
			if r == ':' || r == ';' || r == '{' || r == '}' || r == '\\' || r <= ' ' {
				return false
			}
		}
		return len(s) > 2
	}
	t := strings.TrimLeft(s, "-")
	if len(s)-len(t) > 1 || t == "" {
		return false
	}
	for i, r := range t {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_':
		case (r >= '0' && r <= '9') || r == '-':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// declOK checks one "name: value" declaration (without the trailing semicolon).
func (c *cssCleaner) declOK(decl string, fontFace bool) bool {
	colon := strings.IndexByte(decl, ':')
	if colon <= 0 {
		return false
	}
	name := strings.TrimSpace(decl[:colon])
	if !cssPropName(name) {
		return false
	}
	if !cssBalanced(decl) {
		return false
	}
	n := cssNormalize(decl)
	for _, t := range CSSForbiddenTokens {
		if strings.Contains(n, t) {
			return false
		}
	}
	lname := strings.ToLower(cssUnescape(name))
	urls, ok := cssURLs(decl[colon+1:])
	if !ok {
		return false
	}
	for _, u := range urls {
		if fontFace && lname == "src" {
			if !c.fontURLOK(u) {
				return false
			}
			continue
		}
		if !cssURLOK(u) {
			return false
		}
	}
	return true
}

// cssBalanced: strings closed, (), [] and {} balanced and properly nested, so a kept declaration
// re-splits to itself.
func cssBalanced(s string) bool {
	var stack []byte
	for i := 0; i < len(s); i++ {
		switch ch := s[i]; ch {
		case '\\':
			if i+1 >= len(s) {
				return false // a trailing escape would swallow the ";" we append
			}
			i++
		case '"', '\'':
			_, end, closed := readCSSString(s, i)
			if !closed {
				return false
			}
			i = end - 1
		case '(', '[', '{':
			stack = append(stack, ch)
		case ')', ']', '}':
			want := map[byte]byte{')': '(', ']': '[', '}': '{'}[ch]
			if len(stack) == 0 || stack[len(stack)-1] != want {
				return false
			}
			stack = stack[:len(stack)-1]
		}
	}
	return len(stack) == 0
}

// cssURLs returns the URLs a declaration value references: url(...) arguments and, when the value
// uses image-set()/image()/src()/cross-fade(), every quoted string. ok=false on unbalanced input.
// v is the raw (still escaped) value and is tokenized the way a browser does: an escape is part of
// an identifier and never opens or closes a string, and an escaped function name ("u\72l(") is
// still url(.
func cssURLs(v string) (urls []string, ok bool) {
	lv := stripSpace(strings.ToLower(cssUnescape(v)))
	quotedAreURLs := false
	for _, fn := range []string{"image-set(", "image(", "src(", "cross-fade(", "element("} {
		if strings.Contains(lv, fn) {
			quotedAreURLs = true
		}
	}
	i := 0
	for i < len(v) {
		ch := v[i]
		switch {
		case ch == '"' || ch == '\'':
			s, end, closed := readCSSString(v, i)
			if !closed {
				return nil, false
			}
			if quotedAreURLs {
				urls = append(urls, s)
			}
			i = end
		case ch == '\\' || cssNameChar(ch):
			end := cssIdentEnd(v, i)
			if end >= len(v) || v[end] != '(' || !strings.EqualFold(cssUnescape(v[i:end]), "url") {
				i = end
				continue
			}
			j := end + 1
			for j < len(v) && isCSSSpace(v[j]) {
				j++
			}
			if j < len(v) && (v[j] == '"' || v[j] == '\'') {
				s, end, closed := readCSSString(v, j)
				if !closed {
					return nil, false
				}
				urls = append(urls, s)
				j = end
				for j < len(v) && isCSSSpace(v[j]) {
					j++
				}
				if j >= len(v) || v[j] != ')' {
					return nil, false
				}
				i = j + 1
				continue
			}
			k := j
			for k < len(v) && v[k] != ')' {
				if v[k] == '\\' {
					k++
				}
				k++
			}
			if k >= len(v) {
				return nil, false
			}
			urls = append(urls, strings.TrimSpace(cssUnescape(v[j:k])))
			i = k + 1
		default:
			i++
		}
	}
	return urls, true
}

// cssNameChar: a byte that continues a CSS identifier (non-ASCII bytes included).
func cssNameChar(b byte) bool {
	return b == '-' || b == '_' || (b >= '0' && b <= '9') || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || b >= 0x80
}

// cssIdentEnd returns the index after the identifier (name characters and escapes) starting at i.
func cssIdentEnd(v string, i int) int {
	for i < len(v) {
		switch {
		case v[i] == '\\':
			i++
			if i >= len(v) {
				return i
			}
			if isHex(v[i]) {
				j := i
				for j < len(v) && j-i < 6 && isHex(v[j]) {
					j++
				}
				i = j
				if i < len(v) && isCSSSpace(v[i]) {
					if v[i] == '\r' && i+1 < len(v) && v[i+1] == '\n' {
						i++
					}
					i++
				}
				continue
			}
			i++
		case cssNameChar(v[i]):
			i++
		default:
			return i
		}
	}
	return i
}

// cssURLOK: http, https, relative, "#id" or data:image/* within MaxDataURIBytes.
func cssURLOK(u string) bool {
	u = strings.TrimSpace(u)
	if u == "" {
		return false
	}
	if strings.HasPrefix(u, "#") {
		return true
	}
	return ImageURLOK(u, true)
}

func (c *cssCleaner) fontURLOK(u string) bool {
	u = strings.TrimSpace(u)
	probe := stripURLControl(u)
	if strings.HasPrefix(probe, CSSFontFacePrefix) && !strings.Contains(probe, "..") {
		return true
	}
	if c.opts.SiteOrigin == "" {
		return false
	}
	pu, err := url.Parse(probe)
	if err != nil || pu.Scheme == "" {
		return false
	}
	origin := strings.TrimRight(c.opts.SiteOrigin, "/")
	if !strings.EqualFold(pu.Scheme+"://"+pu.Host, origin) {
		return false
	}
	return strings.HasPrefix(pu.Path, CSSFontFacePrefix) && !strings.Contains(pu.Path, "..")
}

// cssNormalize unescapes, lower-cases and removes whitespace, so "java\73 cript :" and
// "expression (" are caught.
func cssNormalize(s string) string {
	return stripSpace(strings.ToLower(cssUnescape(s)))
}

func stripSpace(s string) string {
	return strings.Map(func(r rune) rune {
		if r <= ' ' || r == 0x7f || (r >= 0x80 && r <= 0xa0) || r == 0xfeff || r == 0x200b {
			return -1
		}
		return r
	}, s)
}

func isCSSSpace(b byte) bool { return b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == '\f' }

func isHex(b byte) bool {
	return (b >= '0' && b <= '9') || (b >= 'a' && b <= 'f') || (b >= 'A' && b <= 'F')
}

// cssUnescape resolves CSS escapes: "\" + 1-6 hex digits (+ one whitespace), "\" + newline
// (removed) and "\" + any other character (that character).
func cssUnescape(s string) string {
	if strings.IndexByte(s, '\\') < 0 && strings.IndexByte(s, 0) < 0 {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		ch := s[i]
		if ch == 0 {
			b.WriteRune(utf8.RuneError)
			i++
			continue
		}
		if ch != '\\' {
			b.WriteByte(ch)
			i++
			continue
		}
		i++
		if i >= len(s) {
			break
		}
		if s[i] == '\n' || s[i] == '\f' {
			i++
			continue
		}
		if s[i] == '\r' {
			i++
			if i < len(s) && s[i] == '\n' {
				i++
			}
			continue
		}
		if isHex(s[i]) {
			j := i
			for j < len(s) && j-i < 6 && isHex(s[j]) {
				j++
			}
			n, _ := strconv.ParseUint(s[i:j], 16, 32)
			r := rune(n)
			if r == 0 || r > utf8.MaxRune || (r >= 0xD800 && r <= 0xDFFF) {
				r = utf8.RuneError
			}
			b.WriteRune(r)
			i = j
			if i < len(s) && isCSSSpace(s[i]) {
				if s[i] == '\r' && i+1 < len(s) && s[i+1] == '\n' {
					i++
				}
				i++
			}
			continue
		}
		_, size := utf8.DecodeRuneInString(s[i:])
		b.WriteString(s[i : i+size])
		i += size
	}
	return b.String()
}

// readCSSString reads a quoted string starting at s[i] (the quote) and returns its unescaped
// content and the index after the closing quote.
func readCSSString(s string, i int) (string, int, bool) {
	q := s[i]
	j := i + 1
	for j < len(s) {
		switch s[j] {
		case '\\':
			j += 2
			continue
		case q:
			return cssUnescape(s[i+1 : j]), j + 1, true
		case '\n':
			return "", j, false
		}
		j++
	}
	return "", len(s), false
}

// stripCSSComments removes /* … */ outside strings; an unterminated comment runs to the end.
func stripCSSComments(s string) (string, bool) {
	if !strings.Contains(s, "/*") {
		return s, false
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		ch := s[i]
		if ch == '"' || ch == '\'' {
			j := i + 1
			for j < len(s) && s[j] != ch && s[j] != '\n' {
				if s[j] == '\\' {
					j++
				}
				j++
			}
			if j < len(s) {
				j++
			}
			if j > len(s) {
				j = len(s)
			}
			b.WriteString(s[i:j])
			i = j
			continue
		}
		if ch == '\\' && i+1 < len(s) {
			b.WriteString(s[i : i+2])
			i += 2
			continue
		}
		if ch == '/' && i+1 < len(s) && s[i+1] == '*' {
			end := strings.Index(s[i+2:], "*/")
			if end < 0 {
				return b.String(), true
			}
			b.WriteByte(' ')
			i += 2 + end + 2
			continue
		}
		b.WriteByte(ch)
		i++
	}
	return b.String(), true
}

type cssItem struct {
	text  string // statement / declaration text, or the prelude of a block
	body  string // block content (block items only)
	block bool
	stray bool // text before an unmatched "}" (dropped)
}

// splitCSSItems splits a rule list or declaration block into statements ("…;") and blocks
// ("prelude { … }"), honouring strings, escapes and parentheses. Unbalanced input never panics:
// a stray "}" is skipped and an unclosed block ends at the end of the input.
func splitCSSItems(s string) []cssItem {
	var items []cssItem
	start := 0
	paren := 0
	for i := 0; i < len(s); i++ {
		switch ch := s[i]; ch {
		case '\\':
			i++
		case '"', '\'':
			j := i + 1
			for j < len(s) && s[j] != ch && s[j] != '\n' {
				if s[j] == '\\' {
					j++
				}
				j++
			}
			i = j
		case '(':
			paren++
		case ')':
			if paren > 0 {
				paren--
			}
		case ';':
			if paren == 0 {
				items = append(items, cssItem{text: s[start:i]})
				start = i + 1
			}
		case '}':
			// Stray closing brace at this level: drop what came before it.
			items = append(items, cssItem{text: s[start:i], stray: true})
			start = i + 1
			paren = 0
		case '{':
			prelude := s[start:i]
			end, bodyEnd := matchBrace(s, i)
			items = append(items, cssItem{text: prelude, body: s[i+1 : bodyEnd], block: true})
			i = end
			start = end + 1
			paren = 0
		}
	}
	if start < len(s) {
		items = append(items, cssItem{text: s[start:]})
	}
	return items
}

// matchBrace returns the index of the "}" matching s[open] ("{") and the end of the body; for an
// unclosed block both are len(s)-1 / len(s).
func matchBrace(s string, open int) (end, bodyEnd int) {
	depth := 0
	for i := open; i < len(s); i++ {
		switch ch := s[i]; ch {
		case '\\':
			i++
		case '"', '\'':
			j := i + 1
			for j < len(s) && s[j] != ch && s[j] != '\n' {
				if s[j] == '\\' {
					j++
				}
				j++
			}
			i = j
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i, i
			}
		}
	}
	return len(s) - 1, len(s)
}

func atRuleName(s string) (string, string) {
	s = strings.TrimPrefix(s, "@")
	i := 0
	for i < len(s) && (s[i] == '-' || s[i] == '_' || (s[i] >= 'a' && s[i] <= 'z') || (s[i] >= 'A' && s[i] <= 'Z') || (s[i] >= '0' && s[i] <= '9')) {
		i++
	}
	return strings.ToLower(s[:i]), s[i:]
}
