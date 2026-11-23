package server

// Pursae SEO: Table of Contents. The seogo/toc block (and, optionally, an automatic insert before the
// first heading of singular content) lists the headings of the post with jump links. The block
// renders a marker; seogoContent replaces it once the whole content is rendered, giving each listed
// heading an id when it has none.

import (
	"context"
	"encoding/json"
	"html"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"wpgo/internal/blocks"
)

const seogoTOCMarker = "<!--seogo-toc:"

// seogoTOCOpts are the settings of one table of contents (block attributes over the defaults).
type seogoTOCOpts struct {
	Title       string `json:"title"`
	Levels      []int  `json:"levels"`
	Numbered    bool   `json:"numbered"`
	Collapsible bool   `json:"collapsible"`
	Collapsed   bool   `json:"collapsed"`
}

var (
	seogoHeadingRe = regexp.MustCompile(`(?is)<h([1-6])(\s[^>]*)?>(.*?)</h([1-6])>`)
	seogoIDAttrRe  = regexp.MustCompile(`(?i)\sid\s*=\s*["']([^"']*)["']`)
	seogoTagRe     = regexp.MustCompile(`(?s)<[^>]*>`)
	seogoTOCMarkRe = regexp.MustCompile(`<!--seogo-toc:([^>]*?)-->`)
)

// seogoTOCDefaults are the panel's defaults (General Settings › Table of Contents).
func (o *seogoOptions) seogoTOCDefaults() seogoTOCOpts {
	t := seogoTOCOpts{Title: o.TOCTitle, Levels: o.TOCLevels, Numbered: o.TOCNumbered, Collapsible: o.TOCCollapsible}
	if len(t.Levels) == 0 {
		t.Levels = []int{2, 3, 4}
	}
	return t
}

// seogoTOCBlockOpts merges a block's attributes over the defaults.
func seogoTOCBlockOpts(def seogoTOCOpts, attrs map[string]any) seogoTOCOpts {
	t := def
	if v, ok := attrs["title"].(string); ok && strings.TrimSpace(v) != "" {
		t.Title = v
	}
	if v, ok := attrs["levels"].([]any); ok && len(v) > 0 {
		t.Levels = nil
		for _, x := range v {
			if f, ok := x.(float64); ok && f >= 1 && f <= 6 {
				t.Levels = append(t.Levels, int(f))
			}
		}
		if len(t.Levels) == 0 {
			t.Levels = def.Levels
		}
	}
	if v, ok := attrs["listStyle"].(string); ok && v != "" {
		t.Numbered = v == "ol"
	}
	if v, ok := attrs["collapsible"].(bool); ok {
		t.Collapsible = v
	}
	if v, ok := attrs["collapsed"].(bool); ok {
		t.Collapsed = v
	}
	return t
}

func seogoTOCMarkerFor(t seogoTOCOpts) string {
	b, _ := json.Marshal(t)
	return seogoTOCMarker + url.QueryEscape(string(b)) + "-->"
}

// seogoRegisterTOCBlock registers the seogo/toc renderer: a marker filled in by seogoContent.
func (s *Server) seogoRegisterTOCBlock() {
	blocks.RegisterDynamic("seogo/toc", func(ctx context.Context, b *blocks.Block, _ string) string {
		s := s
		if rc := renderFrom(ctx); rc != nil && rc.s != nil {
			s = rc.s
		}
		if !s.seogoOn() || !s.seogoModule("toc") {
			return ""
		}
		return seogoTOCMarkerFor(seogoTOCBlockOpts(s.seogoOpts().seogoTOCDefaults(), b.Attrs))
	})
}

type seogoTOCItem struct {
	Level    int
	ID, Text string
}

// seogoTOCContent fills the table of contents markers of a rendered content, and inserts one
// before the first listed heading when auto is set and the content has none. Headings without an
// id get one, so the links have a target.
func seogoTOCContent(content string, def seogoTOCOpts, auto bool, minHeadings int) string {
	has := strings.Contains(content, seogoTOCMarker)
	if !has && !auto {
		return content
	}
	// Collect the levels any table of contents lists.
	tocs := []seogoTOCOpts{}
	for _, m := range seogoTOCMarkRe.FindAllStringSubmatch(content, -1) {
		t := def
		if raw, err := url.QueryUnescape(m[1]); err == nil {
			_ = json.Unmarshal([]byte(raw), &t)
		}
		tocs = append(tocs, t)
	}
	if !has {
		tocs = append(tocs, def)
	}
	want := map[int]bool{}
	for _, t := range tocs {
		for _, l := range t.Levels {
			want[l] = true
		}
	}
	// Give the headings ids and collect them.
	used := map[string]bool{}
	for _, m := range seogoIDAttrRe.FindAllStringSubmatch(content, -1) {
		used[m[1]] = true
	}
	var items []seogoTOCItem
	firstAt := -1
	var sb strings.Builder
	last := 0
	for _, loc := range seogoHeadingRe.FindAllStringSubmatchIndex(content, -1) {
		open, closeLvl := content[loc[2]:loc[3]], content[loc[8]:loc[9]]
		if open != closeLvl {
			continue
		}
		lvl, _ := strconv.Atoi(open)
		if !want[lvl] {
			continue
		}
		attrs := ""
		if loc[4] >= 0 {
			attrs = content[loc[4]:loc[5]]
		}
		if strings.Contains(attrs, "seogo-toc-exclude") {
			continue
		}
		text := strings.TrimSpace(html.UnescapeString(seogoTagRe.ReplaceAllString(content[loc[6]:loc[7]], "")))
		if text == "" {
			continue
		}
		id := ""
		if m := seogoIDAttrRe.FindStringSubmatch(attrs); m != nil {
			id = html.UnescapeString(m[1])
		} else {
			id = seogoTOCSlug(text)
			if used[id] {
				for n := 2; ; n++ {
					if c := id + "-" + strconv.Itoa(n); !used[c] {
						id = c
						break
					}
				}
			}
			used[id] = true
			sb.WriteString(content[last:loc[0]])
			sb.WriteString(`<h` + open + attrs + ` id="` + html.EscapeString(id) + `">`)
			last = loc[0] + 3 + len(attrs) + 1 // "<hN" + attrs + ">"
		}
		if firstAt < 0 {
			firstAt = loc[0]
		}
		items = append(items, seogoTOCItem{lvl, id, text})
	}
	sb.WriteString(content[last:])
	out := sb.String()
	if !has {
		if len(items) < max(minHeadings, 1) || firstAt < 0 {
			return out
		}
		// The first heading is at the same place in out: only later headings or the heading itself changed.
		out = out[:firstAt] + seogoTOCMarkerFor(def) + out[firstAt:]
	}
	i := 0
	return seogoTOCMarkRe.ReplaceAllStringFunc(out, func(string) string {
		t := tocs[min(i, len(tocs)-1)]
		i++
		var list []seogoTOCItem
		for _, it := range items {
			if slices.Contains(t.Levels, it.Level) {
				list = append(list, it)
			}
		}
		return seogoTOCHTML(t, list)
	})
}

// seogoTOCHTML renders one table of contents as a nested list.
func seogoTOCHTML(t seogoTOCOpts, items []seogoTOCItem) string {
	if len(items) == 0 {
		return ""
	}
	tag := "ul"
	if t.Numbered {
		tag = "ol"
	}
	base := 6
	for _, it := range items {
		base = min(base, it.Level)
	}
	var b strings.Builder
	b.WriteString("<" + tag + ` class="seogo-toc-list">`)
	cur := base
	for i, it := range items {
		lvl := max(it.Level, base)
		switch {
		case i == 0:
			b.WriteString("<li>")
		case lvl > cur:
			for ; cur < lvl; cur++ {
				b.WriteString("<" + tag + "><li>")
			}
		default:
			b.WriteString("</li>")
			for ; cur > lvl; cur-- {
				b.WriteString("</" + tag + "></li>")
			}
			b.WriteString("<li>")
		}
		cur = lvl
		b.WriteString(`<a href="#` + html.EscapeString(url.PathEscape(it.ID)) + `">` + html.EscapeString(it.Text) + `</a>`)
	}
	b.WriteString("</li>")
	for ; cur > base; cur-- {
		b.WriteString("</" + tag + "></li>")
	}
	b.WriteString("</" + tag + ">")
	title := strings.TrimSpace(t.Title)
	if title == "" {
		title = "Table of Contents"
	}
	et := html.EscapeString(title)
	if t.Collapsible {
		open := " open"
		if t.Collapsed {
			open = ""
		}
		return `<nav class="seogo-toc wp-block-seogo-toc" aria-label="` + et + `"><details` + open + `><summary class="seogo-toc-title">` + et + `</summary>` + b.String() + `</details></nav>`
	}
	return `<nav class="seogo-toc wp-block-seogo-toc" aria-label="` + et + `"><p class="seogo-toc-title"><strong>` + et + `</strong></p>` + b.String() + `</nav>`
}

// seogoTOCSlug is the anchor of a heading: lower-case letters and digits (any script) joined by "-".
func seogoTOCSlug(text string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(text) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
		if b.Len() > 80 {
			break
		}
	}
	id := strings.Trim(b.String(), "-")
	if id == "" || unicode.IsDigit([]rune(id)[0]) {
		id = "h-" + id
	}
	return strings.TrimSuffix(id, "-")
}

// seogoTOCFor applies the tables of contents to a singular post's rendered content (seogoContent).
func (s *Server) seogoTOCFor(ctx context.Context, content string) string {
	if !s.seogoModule("toc") {
		return content
	}
	o := s.seogoOpts()
	auto := false
	if o.TOCAuto {
		if rc := renderFrom(ctx); rc != nil && rc.q != nil && rc.q.Post != nil && rc.postID == rc.q.Post.ID && !rc.feed {
			k := rc.q.Kind
			auto = (k == "single" || k == "page" || k == "front-page") && slices.Contains(o.TOCAutoTypes, rc.q.Post.Type)
		}
	}
	return seogoTOCContent(content, o.seogoTOCDefaults(), auto, o.TOCMin)
}
