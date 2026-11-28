package server

import (
	"bytes"
	"context"
	"math"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"wpgo/internal/cache"
)

// The front end of replaced and re-cropped media (media_replace.go): uploads requests, and the
// markup of every site page, whoever rendered it (theme, blocks, Web Builder, Commerce, plugins).

// uploadCacheControl is the Cache-Control of an uploads response: immutable, except for a file that
// has a version when the request does not name it: a link that bypasses uploadURL (a hard-coded
// URL, another site) revalidates, so it never shows an older file for a year.
func (s *Server) uploadCacheControl(c *gin.Context) string {
	rel := strings.TrimPrefix(path.Clean(c.Request.URL.Path), "/wp-content/uploads/")
	if strings.HasPrefix(rel, "wp-personal-data-exports/") {
		// A personal-data export is one person's data behind an unguessable name: no shared cache keeps it (#25).
		return "private, no-store"
	}
	if cc, ok := s.wbKitCacheControl(c, rel); ok { // design kit files (webbuilder_kits.go)
		return cc
	}
	if v := s.mediaIdx.version(rel); v != "" && c.Query("v") != v {
		return "public, no-cache"
	}
	return "public, max-age=31536000, immutable"
}

// serveMovedUpload redirects a request for a path a replaced attachment no longer has to the file
// that took its place. It reports whether it answered.
func (s *Server) serveMovedUpload(c *gin.Context) bool {
	rel := strings.TrimPrefix(path.Clean(c.Request.URL.Path), "/wp-content/uploads/")
	local := s.gocdnLocalPath(rel)
	if local == "" {
		return false
	}
	if _, err := os.Stat(local); err == nil {
		return false
	}
	target := ""
	if fi, ok := s.mediaIdx.lookup(rel); ok && fi.Moved != "" {
		target = fi.Moved
	} else if t, ok := s.store.MovedFileTarget(c, rel); ok { // another instance replaced it just now
		target = t
	}
	if target == "" || target == rel {
		return false
	}
	c.Header("Cache-Control", "public, max-age=86400")
	c.Redirect(http.StatusMovedPermanently, s.uploadURL(c, target))
	return true
}

var (
	mediaStyleRe    = regexp.MustCompile(`(?i)\sstyle=("([^"]*)"|'([^']*)')`)
	mediaUploadsMrk = []byte("/wp-content/uploads/")
)

// mediaRefreshHTML brings a site page up to date with the media index: <img> tags of an attachment
// with a focal point get object-position (unless they position themselves), tags whose width and
// height no longer match a replaced file get the height of its new aspect ratio, uploads URLs of
// moved paths point at their successors and the URLs of versioned files carry ?v=<version>.
func (s *Server) mediaRefreshHTML(c *gin.Context, body []byte) []byte {
	if s.mediaIdx.empty() || !bytes.Contains(body, mediaUploadsMrk) {
		return body
	}
	hosts := s.gocdnHosts(c)
	body = mediaImgTags(body, func(tag string) string { return s.mediaFixImgTag(tag, hosts) })
	return s.mediaVersionURLs(body, hosts)
}

// mediaImgTags replaces every <img> tag of body with fix(tag): what the regexp (?is)<img\s[^>]*>
// with ReplaceAllFunc does (the tests keep it as the oracle), found with byte searches instead, and
// copying body only once fix changes a tag. Most pages have nothing to change, and the regexp copied
// the whole page regardless. body itself is returned when nothing changed.
func mediaImgTags(body []byte, fix func(tag string) string) []byte {
	var out []byte // nil until a tag changes
	last := 0      // body[:last] is in out (or unchanged)
	for i := 0; ; {
		j := bytes.IndexByte(body[i:], '<')
		if j < 0 {
			break
		}
		start := i + j
		if start+4 >= len(body) || !isImgTagStart(body[start+1:start+5]) {
			i = start + 1
			continue
		}
		k := bytes.IndexByte(body[start+5:], '>')
		if k < 0 {
			break // no '>' after this tag: no later one can match either
		}
		end := start + 5 + k + 1
		tag := string(body[start:end])
		if fixed := fix(tag); fixed != tag {
			if out == nil {
				out = make([]byte, 0, len(body)+len(body)/64)
			}
			out = append(out, body[last:start]...)
			out = append(out, fixed...)
			last = end
		}
		i = end
	}
	if out == nil {
		return body
	}
	return append(out, body[last:]...)
}

// isImgTagStart reports whether b (4 bytes) is "img" in any case and a whitespace byte, as \s is
// for RE2: [\t\n\f\r ].
func isImgTagStart(b []byte) bool {
	if b[0]|0x20 != 'i' || b[1]|0x20 != 'm' || b[2]|0x20 != 'g' {
		return false
	}
	switch b[3] {
	case ' ', '\t', '\n', '\f', '\r':
		return true
	}
	return false
}

// mediaRelOf is the uploads path of a URL of this site ("" when it is not one).
func mediaRelOf(u string, hosts map[string]bool) string {
	i := strings.Index(u, "/wp-content/uploads/")
	if i < 0 {
		return ""
	}
	if pre := u[:i]; pre != "" {
		pre = strings.TrimPrefix(strings.TrimPrefix(pre, "https:"), "http:")
		host, ok := strings.CutPrefix(pre, "//")
		if !ok || !hosts[strings.ToLower(host)] {
			return ""
		}
	}
	rel := u[i+len("/wp-content/uploads/"):]
	if j := strings.IndexAny(rel, "?#"); j >= 0 {
		rel = rel[:j]
	}
	if un, err := url.PathUnescape(rel); err == nil {
		rel = un
	}
	return rel
}

// mediaFixImgTag applies the focal point and the new aspect ratio to one <img> tag.
func (s *Server) mediaFixImgTag(tag string, hosts map[string]bool) string {
	m := imgSrcRe.FindStringSubmatch(tag)
	if m == nil {
		return tag
	}
	fi, ok := s.mediaIdx.lookup(mediaRelOf(m[1], hosts))
	if !ok {
		return tag
	}
	w, h := fi.W, fi.H
	if fi.Moved != "" {
		t, _ := s.mediaIdx.lookup(fi.Moved)
		w, h = t.W, t.H
	}
	style := ""
	if sm := mediaStyleRe.FindStringSubmatch(tag); sm != nil {
		style = strings.ToLower(sm[2] + sm[3])
	}
	if !strings.Contains(style, "object-fit") {
		tag = mediaFixDims(tag, w, h)
	}
	if f := fi.Focal; f != nil && !f.Centered() && !strings.Contains(style, "object-position") {
		decl := "object-position:" + f.CSS() + ";"
		if sm := mediaStyleRe.FindStringSubmatchIndex(tag); sm != nil {
			// Inside the quotes, before the tag's own declarations.
			at := sm[4] // the start of the double-quoted value
			if at < 0 {
				at = sm[6] // single-quoted
			}
			tag = tag[:at] + decl + tag[at:]
		} else {
			tag = strings.Replace(tag, "<img", `<img style="`+decl+`"`, 1)
		}
	}
	return tag
}

// mediaFixDims gives a tag whose width and height no longer match a w×h file's aspect ratio the
// height of that ratio (the width stays: it may be a scaled-down display size). An image that crops
// itself with object-fit sets its own shape; the callers leave it alone.
func mediaFixDims(tag string, w, h int) string {
	if w <= 0 || h <= 0 {
		return tag
	}
	mw, mh := imgWidthRe.FindStringSubmatchIndex(tag), imgHeightRe.FindStringSubmatchIndex(tag)
	if mw == nil || mh == nil {
		return tag
	}
	aw, _ := strconv.Atoi(tag[mw[2]:mw[3]])
	ah, _ := strconv.Atoi(tag[mh[2]:mh[3]])
	if aw <= 0 || ah <= 0 || imageMatchesRatio(aw, ah, w, h) {
		return tag
	}
	nh := max(1, int(math.Round(float64(aw)*float64(h)/float64(w))))
	return tag[:mh[2]] + strconv.Itoa(nh) + tag[mh[3]:]
}

// mediaContentImg is filterContentTags' first step for an <img> of attachment id: a src that is a
// path the attachment no longer has (its file was replaced) points at its successor, with the
// successor's aspect ratio, so srcset and the size attributes are worked out for the current files.
func (s *Server) mediaContentImg(img string, id int64) string {
	m := imgSrcRe.FindStringSubmatchIndex(img)
	if m == nil {
		return img
	}
	src := img[m[2]:m[3]]
	i := strings.Index(src, "/wp-content/uploads/")
	if i < 0 {
		return img
	}
	rel := src[i+len("/wp-content/uploads/"):]
	if j := strings.IndexAny(rel, "?#"); j >= 0 {
		rel = rel[:j]
	}
	fi, ok := s.mediaIdx.lookup(rel)
	if !ok || fi.ID != id || fi.Moved == "" {
		return img
	}
	t, _ := s.mediaIdx.lookup(fi.Moved)
	img = img[:m[2]] + src[:i] + "/wp-content/uploads/" + fi.Moved + img[m[3]:]
	if sm := mediaStyleRe.FindStringSubmatch(img); sm != nil && strings.Contains(strings.ToLower(sm[2]+sm[3]), "object-fit") {
		return img
	}
	return mediaFixDims(img, t.W, t.H)
}

// mediaVersionURLs rewrites the uploads URLs of the index's paths: a moved path becomes its
// successor, a versioned file gets ?v=<version> (an existing ?v= is updated; other query strings,
// e.g. image service parameters, are left as they are).
func (s *Server) mediaVersionURLs(body []byte, hosts map[string]bool) []byte {
	matches := gocdnUploadsURL.FindAllSubmatchIndex(body, -1)
	if len(matches) == 0 {
		return body
	}
	var b bytes.Buffer
	last := 0
	for _, m := range matches {
		if !gocdnBoundary(body, m[0]) {
			continue
		}
		if m[2] >= 0 && !hosts[strings.ToLower(string(body[m[2]:m[3]]))] {
			continue
		}
		raw := string(body[m[4]:m[5]])
		rel := raw
		if un, err := url.PathUnescape(raw); err == nil {
			rel = un
		}
		fi, ok := s.mediaIdx.lookup(rel)
		if !ok {
			continue
		}
		newRel := raw
		if fi.Moved != "" {
			newRel = fi.Moved
		}
		ver := s.mediaIdx.version(path.Clean(strings.TrimPrefix(newRel, "/")))
		end := m[5]
		suffix := ""
		if end < len(body) && body[end] == '?' {
			if n := mediaVersionQueryLen(body[end:]); n > 0 {
				end += n // our own ?v=…: replaced by the current one
				if ver != "" {
					suffix = "?v=" + ver
				}
			} else if fi.Moved == "" {
				continue // another query string: nothing to change
			}
		} else if ver != "" {
			suffix = "?v=" + ver
		}
		if newRel == raw && end == m[5] && suffix == "" {
			continue
		}
		b.Write(body[last:m[4]])
		b.WriteString(newRel)
		b.WriteString(suffix)
		last = end
	}
	if last == 0 {
		return body
	}
	b.Write(body[last:])
	return b.Bytes()
}

// mediaVersionQueryLen is the length of a "?v=<version>" query that is the whole query string at
// the start of rest (0 when rest does not start with one).
func mediaVersionQueryLen(rest []byte) int {
	if !bytes.HasPrefix(rest, []byte("?v=")) {
		return 0
	}
	n := 3
	for n < len(rest) && (rest[n] >= '0' && rest[n] <= '9' || rest[n] >= 'a' && rest[n] <= 'z') {
		n++
	}
	if n == 3 {
		return 0
	}
	if n < len(rest) {
		switch rest[n] {
		case '"', '\'', ' ', '\t', '\n', '\r', '<', '>', ')', ',', '\\', '#':
		default:
			return 0 // more parameters follow
		}
	}
	return n
}

// announceMediaChange makes the other instances reload the media index when this one has no page
// cache to announce the purge (with Pursae Cache on, purgeAll announces it).
func (s *Server) announceMediaChange(ctx context.Context) {
	if s.currentCache() == nil {
		s.announcePurge(cache.Purge{All: true})
	}
}
