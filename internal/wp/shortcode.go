package wp

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Shortcodes follow WordPress' shortcode syntax (get_shortcode_regex()):
//
//	[tag]                    self-closing, no attributes
//	[tag attr="v" k='v' n=3 flag]
//	[tag]content[/tag]       enclosing
//	[tag /]                  explicitly self-closing
//	[[tag]]                  escaped: rendered as the literal text [tag]
//
// Only registered tags are recognised; anything else is left untouched, as in
// WordPress. Enclosing shortcodes do not nest the same tag inside themselves.

// ShortcodeFunc renders one shortcode. attrs holds the named attributes plus
// positional ones under "0", "1", … content is the enclosed text (empty for
// self-closing tags). The return value replaces the whole shortcode.
type ShortcodeFunc func(tag string, attrs map[string]string, content string) string

// ValidShortcodeTag reports whether tag is an acceptable shortcode name:
// 1–64 lowercase letters, digits, underscores and dashes.
func ValidShortcodeTag(tag string) bool {
	return shortcodeTagRe.MatchString(tag)
}

var shortcodeTagRe = regexp.MustCompile(`^[a-z0-9_-]{1,64}$`)

// DoShortcode expands every occurrence of the given tags in content with fn.
// It mirrors do_shortcode(): escaped shortcodes ([[tag]]) lose one pair of
// brackets, unknown tags are left alone, and a <p> that holds nothing but a
// shortcode is removed first (shortcode_unautop()).
func DoShortcode(content string, tags []string, fn ShortcodeFunc) string {
	if len(tags) == 0 || !strings.Contains(content, "[") {
		return content
	}
	re := shortcodeRegex(tags)
	if re == nil {
		return content
	}
	content = unautopShortcodes(content, re)
	return re.ReplaceAllStringFunc(content, func(m string) string {
		sub := re.FindStringSubmatch(m)
		// sub: 1 = leading "[", 2 = tag, 3 = attrs, 4 = self-closing "/", 5 = content, 6 = trailing "]"
		if sub[1] == "[" && sub[6] == "]" {
			return m[1 : len(m)-1] // escaped
		}
		attrs := ParseShortcodeAttrs(sub[3])
		return sub[1] + fn(sub[2], attrs, sub[5]) + sub[6]
	})
}

// shortcodeRegex builds the WordPress shortcode regex for the given tags.
func shortcodeRegex(tags []string) *regexp.Regexp {
	names := make([]string, 0, len(tags))
	for _, t := range tags {
		if ValidShortcodeTag(t) {
			names = append(names, regexp.QuoteMeta(t))
		}
	}
	if len(names) == 0 {
		return nil
	}
	sort.Slice(names, func(i, j int) bool { return len(names[i]) > len(names[j]) }) // longest first, like WordPress
	alt := strings.Join(names, "|")
	// Groups: 1 escape "[", 2 tag, 3 attributes, 4 self-closing "/", 5 content, 6 escape "]".
	// The attribute group must start with whitespace, so [boxes] never matches the tag "box"
	// (WordPress uses a lookahead for that). Go's RE2 has no backreferences, so the closing tag
	// is any registered tag; the content group stops at the first closing shortcode, which also
	// keeps the same tag from nesting.
	pattern := `(\[?)\[(` + alt + `)((?:\s(?:[^\]\/]|\/[^\]])*)?)(?:(\/)\]|\](?:((?:[^\[]|\[[^\/])*?)\[\/(?:` + alt + `)\])?)(\]?)`
	return regexp.MustCompile(pattern)
}

// unautopShortcodes removes the <p>…</p> wpautop() wraps around a lone shortcode.
func unautopShortcodes(content string, re *regexp.Regexp) string {
	if !strings.Contains(content, "<p>") {
		return content
	}
	p := regexp.MustCompile(`(?s)<p>\s*(\[[^\]]+\](?:[^\[]*\[/[^\]]+\])?)\s*</p>`)
	return p.ReplaceAllStringFunc(content, func(m string) string {
		inner := p.FindStringSubmatch(m)[1]
		if loc := re.FindStringIndex(inner); loc != nil && loc[0] == 0 && loc[1] == len(inner) {
			return inner
		}
		return m
	})
}

var shortcodeAttrRe = regexp.MustCompile(`(\w+)\s*=\s*"([^"]*)"|(\w+)\s*=\s*'([^']*)'|(\w+)\s*=\s*([^\s'"]+)|"([^"]*)"|'([^']*)'|(\S+)`)

// ParseShortcodeAttrs is shortcode_parse_atts(): named attributes keep their
// (lower-cased) name, bare words and quoted strings become positional "0",
// "1", … values.
func ParseShortcodeAttrs(text string) map[string]string {
	out := map[string]string{}
	text = strings.TrimSpace(text)
	if text == "" {
		return out
	}
	pos := 0
	for _, m := range shortcodeAttrRe.FindAllStringSubmatch(text, -1) {
		switch {
		case m[1] != "":
			out[strings.ToLower(m[1])] = m[2]
		case m[3] != "":
			out[strings.ToLower(m[3])] = m[4]
		case m[5] != "":
			out[strings.ToLower(m[5])] = m[6]
		case m[7] != "":
			out[strconv.Itoa(pos)] = m[7]
			pos++
		case m[8] != "":
			out[strconv.Itoa(pos)] = m[8]
			pos++
		default:
			out[strconv.Itoa(pos)] = m[9]
			pos++
		}
	}
	return out
}

// HasShortcode reports whether content contains a shortcode with tag.
func HasShortcode(content, tag string) bool {
	if !strings.Contains(content, "["+tag) {
		return false
	}
	re := shortcodeRegex([]string{tag})
	return re != nil && re.MatchString(content)
}
