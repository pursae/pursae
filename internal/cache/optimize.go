package cache

import (
	"html"
	"path"
	"strings"
)

// segment is a slice of the document: either ordinary markup ("normal") or a block
// whose content must not be touched (a comment or a <script>, <style>, <pre> or
// <textarea> element including its tags).
type segment struct {
	start, end int
	raw        string // "" for normal markup, else "!--", "script", "style", "pre", "textarea"
}

var rawElements = []string{"script", "style", "pre", "textarea"}

// Optimize applies the enabled optimizations of o to an HTML document. It works on
// the full body with a small hand-written scanner that never rewrites the content of
// comments, <script>, <style>, <pre> and <textarea>; when markup cannot be
// understood (e.g. an unterminated tag) it is left unchanged.
func Optimize(doc []byte, o OptimizeConfig) []byte {
	if !o.enabled() || len(doc) == 0 {
		return doc
	}
	src := string(doc)
	lc := asciiLower(src)
	segs := splitSegments(src, lc)

	var out strings.Builder
	out.Grow(len(src) + 256)
	imgs := 0
	inBody := false
	headDone := len(o.Preload) == 0
	for i, sg := range segs {
		text := src[sg.start:sg.end]
		ltext := lc[sg.start:sg.end]
		if sg.raw == "!--" && o.CollapseWhitespace && removableComment(text) {
			continue
		}
		if sg.raw != "" {
			if sg.raw == "script" && o.DeferScripts && inBody {
				text = deferScript(text, lc, o.DeferAllowlist)
			}
			out.WriteString(text)
			continue
		}
		if !headDone {
			if at := findTag(ltext, "head", 0); at >= 0 {
				if end := tagEnd(text, at); end >= 0 {
					text = text[:end+1] + preloadLinks(o.Preload, src) + text[end+1:]
					ltext = asciiLower(text)
					headDone = true
				}
			}
		}
		if !inBody && findTag(ltext, "body", 0) >= 0 {
			inBody = true
		}
		if o.LazyImages {
			text = lazyImages(text, &imgs, o.LazySkipFirst)
		}
		if o.CollapseWhitespace {
			prevTag := i > 0
			nextTag := i < len(segs)-1
			text = collapseWhitespace(text, prevTag, nextTag)
		}
		out.WriteString(text)
	}
	return []byte(out.String())
}

// splitSegments partitions src into normal and protected segments. lc is the
// ASCII-lowercased src (same length).
func splitSegments(src, lc string) []segment {
	var segs []segment
	normalStart := 0
	i := 0
	for i < len(lc) {
		j := strings.IndexByte(lc[i:], '<')
		if j < 0 {
			break
		}
		i += j
		raw, end := "", -1
		if strings.HasPrefix(lc[i:], "<!--") {
			raw = "!--"
			if k := strings.Index(lc[i+4:], "-->"); k >= 0 {
				end = i + 4 + k + 3
			} else {
				end = len(lc)
			}
		} else {
			for _, name := range rawElements {
				if isTagAt(lc, i, name) {
					raw = name
					open := tagEnd(src, i)
					if open < 0 {
						end = len(lc)
						break
					}
					closeTag := "</" + name
					k := open + 1
					for {
						m := strings.Index(lc[k:], closeTag)
						if m < 0 {
							end = len(lc)
							break
						}
						k += m
						after := k + len(closeTag)
						if after >= len(lc) || lc[after] == '>' || isSpace(lc[after]) || lc[after] == '/' {
							if gt := strings.IndexByte(lc[k:], '>'); gt >= 0 {
								end = k + gt + 1
							} else {
								end = len(lc)
							}
							break
						}
						k = after
					}
					break
				}
			}
		}
		if raw == "" {
			i++
			continue
		}
		if i > normalStart {
			segs = append(segs, segment{start: normalStart, end: i})
		}
		segs = append(segs, segment{start: i, end: end, raw: raw})
		i, normalStart = end, end
	}
	if normalStart < len(src) {
		segs = append(segs, segment{start: normalStart, end: len(src)})
	}
	return segs
}

// isTagAt reports whether an opening tag <name starts at lc[i].
func isTagAt(lc string, i int, name string) bool {
	if !strings.HasPrefix(lc[i+1:], name) {
		return false
	}
	k := i + 1 + len(name)
	return k >= len(lc) || lc[k] == '>' || lc[k] == '/' || isSpace(lc[k])
}

// findTag returns the index of the first opening tag <name in lc at or after from.
func findTag(lc, name string, from int) int {
	for from < len(lc) {
		j := strings.IndexByte(lc[from:], '<')
		if j < 0 {
			return -1
		}
		at := from + j
		if isTagAt(lc, at, name) {
			return at
		}
		end := tagEnd(lc, at)
		if end < 0 {
			from = at + 1 // a stray "<" in text (1 < 2): skip it, not the rest of the document
			continue
		}
		from = end + 1
	}
	return -1
}

// tagEnd returns the index of the '>' closing the tag that starts at s[i], skipping
// quoted attribute values. It returns -1 when the tag is unterminated.
func tagEnd(s string, i int) int {
	var last byte // last non-space byte outside quotes
	for j := i + 1; j < len(s); j++ {
		c := s[j]
		switch {
		case (c == '"' || c == '\'') && last == '=':
			k := strings.IndexByte(s[j+1:], c)
			if k < 0 {
				return -1
			}
			j += k + 1
			last = c
		case c == '>':
			return j
		case c == '<' && j > i+1:
			return -1 // malformed: a new tag starts before this one closed
		case !isSpace(c):
			last = c
		}
	}
	return -1
}

// parseAttrs returns the lowercased attribute names of tag (from '<' to '>') mapped
// to their unescaped values.
func parseAttrs(tag string) map[string]string {
	attrs := map[string]string{}
	j := 1
	for j < len(tag) && !isSpace(tag[j]) && tag[j] != '>' && tag[j] != '/' {
		j++ // tag name
	}
	for j < len(tag) {
		for j < len(tag) && (isSpace(tag[j]) || tag[j] == '/') {
			j++
		}
		if j >= len(tag) || tag[j] == '>' {
			break
		}
		ns := j
		for j < len(tag) && !isSpace(tag[j]) && tag[j] != '=' && tag[j] != '>' && !(tag[j] == '/' && j+1 < len(tag) && tag[j+1] == '>') {
			j++
		}
		name := asciiLower(tag[ns:j])
		for j < len(tag) && isSpace(tag[j]) {
			j++
		}
		val := ""
		if j < len(tag) && tag[j] == '=' {
			j++
			for j < len(tag) && isSpace(tag[j]) {
				j++
			}
			if j < len(tag) && (tag[j] == '"' || tag[j] == '\'') {
				q := tag[j]
				k := strings.IndexByte(tag[j+1:], q)
				if k < 0 {
					k = len(tag) - j - 1
				}
				val = tag[j+1 : j+1+k]
				j += k + 2
			} else {
				vs := j
				for j < len(tag) && !isSpace(tag[j]) && tag[j] != '>' {
					j++
				}
				val = tag[vs:j]
			}
		}
		if name != "" {
			if _, dup := attrs[name]; !dup {
				attrs[name] = html.UnescapeString(val)
			}
		}
	}
	return attrs
}

// insertAttrs inserts extra (which starts with a space) before the end of the tag
// tag[:gt+1], keeping a self-closing "/>" intact.
func insertAttrs(tag string, gt int, extra string) string {
	k := gt
	if k > 0 && tag[k-1] == '/' && k > 1 && (isSpace(tag[k-2]) || tag[k-2] == '"' || tag[k-2] == '\'') {
		k--
	}
	for k > 0 && isSpace(tag[k-1]) {
		k--
	}
	return tag[:k] + extra + tag[k:]
}

// lazyImages adds loading="lazy" decoding="async" to <img> tags (loading="lazy" to <iframe>
// tags) lacking a loading attribute. The first skip images of the document stay eager, and the
// very first one gets fetchpriority="high": it is most likely the LCP image.
func lazyImages(text string, count *int, skip int) string {
	lc := asciiLower(text)
	var b strings.Builder
	last, from := 0, 0
	for {
		at, name := findTag(lc, "img", from), "img"
		if fr := findTag(lc, "iframe", from); fr >= 0 && (at < 0 || fr < at) {
			at, name = fr, "iframe"
		}
		if at < 0 {
			break
		}
		gt := tagEnd(text, at)
		if gt < 0 {
			break
		}
		from = gt + 1
		tag := text[at : gt+1]
		attrs := parseAttrs(tag)
		extra := ""
		if name == "img" {
			*count++
			if *count <= skip {
				_, lazy := attrs["loading"]
				if _, ok := attrs["fetchpriority"]; *count == 1 && !ok && !lazy {
					extra = ` fetchpriority="high"`
				}
			}
		}
		if _, ok := attrs["loading"]; !ok && extra == "" && (name == "iframe" || *count > skip) {
			extra = ` loading="lazy"`
			if _, ok := attrs["decoding"]; !ok && name == "img" {
				extra += ` decoding="async"`
			}
		}
		if extra == "" {
			continue
		}
		b.WriteString(text[last:at])
		b.WriteString(insertAttrs(tag, len(tag)-1, extra))
		last = gt + 1
	}
	if last == 0 {
		return text
	}
	b.WriteString(text[last:])
	return b.String()
}

// deferScript adds defer to a classic external <script> whose handle is allowlisted.
// seg is the whole script element; docLC the lowercased document, used to refuse
// handles that have a dependent inline "<handle>-js-after" script.
func deferScript(seg, docLC string, allow []string) string {
	gt := tagEnd(seg, 0)
	if gt < 0 {
		return seg
	}
	open := seg[:gt+1]
	attrs := parseAttrs(open)
	src, ok := attrs["src"]
	if !ok || src == "" {
		return seg
	}
	for _, a := range []string{"async", "defer", "nomodule", "data-no-defer"} {
		if _, ok := attrs[a]; ok {
			return seg
		}
	}
	switch strings.ToLower(strings.TrimSpace(attrs["type"])) {
	case "", "text/javascript", "application/javascript":
	default:
		return seg // modules, JSON, templates
	}
	handle := matchHandle(attrs["id"], src, allow)
	if handle == "" {
		return seg
	}
	if strings.Contains(docLC, asciiLower(handle)+"-js-after") {
		return seg // an inline script runs right after it and expects it loaded
	}
	return insertAttrs(open, gt, " defer") + seg[gt+1:]
}

func matchHandle(id, src string, allow []string) string {
	file := src
	if i := strings.IndexAny(file, "?#"); i >= 0 {
		file = file[:i]
	}
	file = path.Base(file)
	file = strings.TrimSuffix(strings.TrimSuffix(file, ".js"), ".min")
	for _, h := range allow {
		if h == "" {
			continue
		}
		if id == h || id == h+"-js" || file == h || strings.ContainsAny(h, "/.") && strings.Contains(src, h) {
			return h
		}
	}
	return ""
}

// collapseWhitespace collapses runs of whitespace between tags into a single space.
// prevTag/nextTag tell whether the segment is adjacent to a protected element (which
// always starts with '<' and ends with '>').
func collapseWhitespace(text string, prevTag, nextTag bool) string {
	var b strings.Builder
	b.Grow(len(text))
	i := 0
	// Leading run after a protected element.
	if prevTag {
		n := countSpace(text, 0)
		if n > 0 && (n == len(text) && nextTag || n < len(text) && text[n] == '<') {
			b.WriteByte(' ')
			i = n
			if n == len(text) {
				return b.String()
			}
		}
	}
	for i < len(text) {
		c := text[i]
		b.WriteByte(c)
		i++
		if c != '>' {
			continue
		}
		n := countSpace(text, i)
		if n == 0 {
			continue
		}
		if i+n < len(text) && text[i+n] == '<' || i+n == len(text) && nextTag {
			b.WriteByte(' ')
			i += n
		}
	}
	return b.String()
}

// removableComment reports whether an HTML comment can go when minifying: everything except
// conditional comments and ones marked to stay (<!--noptimize-->, <!--wpgo-keep ...-->).
func removableComment(c string) bool {
	lc := asciiLower(c)
	for _, keep := range []string{"<!--[if", "<![endif]", "<!--<![endif]", "noptimize", "wpgo-keep", "google_ad"} {
		if strings.Contains(lc, keep) {
			return false
		}
	}
	return true
}

func countSpace(s string, i int) int {
	n := 0
	for i+n < len(s) && isSpace(s[i+n]) {
		n++
	}
	return n
}

// preloadLinks renders <link rel="preload"> tags, skipping assets already preloaded.
func preloadLinks(assets []PreloadAsset, doc string) string {
	var b strings.Builder
	lcDoc := asciiLower(doc)
	for _, a := range assets {
		if a.Href == "" || (a.IfDocContains != "" && !strings.Contains(doc, a.IfDocContains)) {
			continue
		}
		href := html.EscapeString(a.Href)
		if strings.Contains(lcDoc, `rel="preload"`) && strings.Contains(doc, `href="`+href+`"`) {
			continue
		}
		b.WriteString(`<link rel="preload" href="`)
		b.WriteString(href)
		b.WriteByte('"')
		if a.As != "" {
			b.WriteString(` as="` + html.EscapeString(a.As) + `"`)
		}
		if a.Type != "" {
			b.WriteString(` type="` + html.EscapeString(a.Type) + `"`)
		}
		if a.CrossOrigin {
			b.WriteString(" crossorigin")
		}
		b.WriteByte('>')
	}
	return b.String()
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' }

// asciiLower lowercases ASCII letters only, so byte offsets match the input.
func asciiLower(s string) string {
	for i := 0; i < len(s); i++ {
		if c := s[i]; 'A' <= c && c <= 'Z' {
			b := []byte(s)
			for j := i; j < len(b); j++ {
				if 'A' <= b[j] && b[j] <= 'Z' {
					b[j] += 'a' - 'A'
				}
			}
			return string(b)
		}
	}
	return s
}
