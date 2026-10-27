package server

import (
	"context"
	"fmt"
	"html"
	"path"
	"regexp"
	"strconv"
	"strings"

	"wpgo/internal/store"
)

// Responsive images and loading optimisation for post content: wp_filter_content_tags() and the
// wp_img_tag_add_* helpers of media.php.

const (
	// omitLoadingAttrThreshold is wp_omit_loading_attr_threshold(): the first images of the main loop
	// are assumed to be in the viewport and are not lazy-loaded.
	omitLoadingAttrThreshold = 3
	// minPriorityImgPixels is wp_min_priority_img_pixels: images at least this large (width × height)
	// get fetchpriority="high" when they are the first in the viewport.
	minPriorityImgPixels = 50000
	// maxSrcsetImageWidth is max_srcset_image_width: wider sub-sizes stay out of srcset unless they are the src.
	maxSrcsetImageWidth = 2048
)

var (
	contentImgRe     = regexp.MustCompile(`(?is)<img\s[^>]+>`)
	imgClassIDRe     = regexp.MustCompile(`(?i)wp-image-([0-9]+)`)
	imgSrcRe         = regexp.MustCompile(`(?i)\ssrc=["']([^"']+)["']`)
	imgWidthRe       = regexp.MustCompile(`(?i)\swidth=["']([0-9]+)["']`)
	imgHeightRe      = regexp.MustCompile(`(?i)\sheight=["']([0-9]+)["']`)
	imgLoadingRe     = regexp.MustCompile(`(?i)\sloading=["']([^"']*)["']`)
	imgPriorityRe    = regexp.MustCompile(`(?i)\sfetchpriority=["']([^"']*)["']`)
	imgDecodingRe    = regexp.MustCompile(`(?i)\sdecoding=["']([^"']*)["']`)
	imgSizesRe       = regexp.MustCompile(`(?i)\ssizes=["']([^"']*)["']`)
	imgEndRe         = regexp.MustCompile(`^<img\s(.+?)[/\s]*>$`)
	sizesValidAutoRe = regexp.MustCompile(`(?i)^auto\s*(,|$)`)
)

// filterContentTags is wp_filter_content_tags() for <img> tags: width/height, srcset/sizes for images
// from the media library (class wp-image-N) and the loading optimisation attributes.
func (s *Server) filterContentTags(ctx context.Context, content string) string {
	if !strings.Contains(content, "<img") {
		return content
	}
	rc := renderFrom(ctx)
	attachments := map[int64]*store.Attachment{}
	return contentImgRe.ReplaceAllStringFunc(content, func(img string) string {
		var a *store.Attachment
		if m := imgClassIDRe.FindStringSubmatch(img); m != nil {
			id, _ := strconv.ParseInt(m[1], 10, 64)
			if id > 0 {
				if cached, ok := attachments[id]; ok {
					a = cached
				} else if got, err := s.store.GetAttachment(ctx, id); err == nil {
					a = got
					attachments[id] = a
				}
			}
		}
		if a != nil && !strings.Contains(img, " width=") && !strings.Contains(img, " height=") {
			img = s.imgAddWidthHeight(ctx, img, a)
		}
		if a != nil && !strings.Contains(img, " srcset=") {
			img = s.imgAddSrcsetSizes(ctx, img, a)
		}
		img = imgAddLoadingAttrs(rc, img)
		return imgAddAutoSizes(img)
	})
}

func imgSrc(img string) string {
	m := imgSrcRe.FindStringSubmatch(img)
	if m == nil {
		return ""
	}
	return strings.SplitN(m[1], "?", 2)[0]
}

// imageSrcDimensions is wp_image_src_get_dimensions(): the size whose file name the src points at.
func imageSrcDimensions(src string, a *store.Attachment) (int, int, bool) {
	base := path.Base(src)
	if base == path.Base(a.FilePath) {
		return a.Width, a.Height, a.Width > 0
	}
	for _, v := range a.Variants {
		if base == path.Base(v.FilePath) {
			return v.Width, v.Height, true
		}
	}
	return 0, 0, false
}

// imgAddWidthHeight is wp_img_tag_add_width_and_height_attr().
func (s *Server) imgAddWidthHeight(_ context.Context, img string, a *store.Attachment) string {
	src := imgSrc(img)
	if src == "" {
		return img
	}
	w, h, ok := imageSrcDimensions(src, a)
	if !ok {
		return img
	}
	return strings.Replace(img, "<img", fmt.Sprintf(`<img width="%d" height="%d"`, w, h), 1)
}

// imageMatchesRatio is wp_image_matches_ratio(): the two sizes have the same aspect ratio within 1px.
func imageMatchesRatio(sw, sh, tw, th int) bool {
	if sw <= 0 || sh <= 0 || tw <= 0 || th <= 0 {
		return false
	}
	// Scale the target down to the source width and compare heights.
	var ew, eh int
	if sw > tw {
		ew, eh = constrainDimensions(sw, sh, tw, 0)
	} else {
		ew, eh = constrainDimensions(tw, th, sw, 0)
		tw, th, sw, sh = sw, sh, tw, th
	}
	return abs(ew-tw) <= 1 && abs(eh-th) <= 1
}

// constrainDimensions is wp_constrain_dimensions() for a width-only constraint.
func constrainDimensions(w, h, maxW, maxH int) (int, int) {
	if w == 0 || h == 0 {
		return 0, 0
	}
	ratio := float64(maxW) / float64(w)
	if maxH > 0 {
		if r := float64(maxH) / float64(h); r < ratio {
			ratio = r
		}
	}
	nw := int(float64(w)*ratio + 0.5)
	nh := int(float64(h)*ratio + 0.5)
	return nw, nh
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// imageSrcset is wp_calculate_image_srcset(): every sub-size (and the original) sharing the aspect
// ratio of the displayed size, the src first; "" when there are fewer than two candidates.
func (s *Server) imageSrcset(ctx context.Context, src string, w, h int, a *store.Attachment) string {
	dir := path.Dir(a.FilePath)
	if dir == "." {
		dir = ""
	} else {
		dir += "/"
	}
	type cand struct {
		url   string
		width int
	}
	var sources []cand
	seen := map[int]bool{}
	srcMatched := false
	add := func(file string, width, height int) {
		isSrc := false
		if !srcMatched && strings.Contains(src, dir+path.Base(file)) {
			srcMatched, isSrc = true, true
		}
		if width > maxSrcsetImageWidth && !isSrc {
			return
		}
		if !imageMatchesRatio(w, h, width, height) || seen[width] {
			return
		}
		seen[width] = true
		c := cand{s.uploadURL(ctx, file), width}
		if isSrc {
			sources = append([]cand{c}, sources...)
		} else {
			sources = append(sources, c)
		}
	}
	for _, v := range a.Variants {
		add(v.FilePath, v.Width, v.Height)
	}
	add(a.FilePath, a.Width, a.Height)
	if !srcMatched || len(sources) < 2 {
		return ""
	}
	parts := make([]string, len(sources))
	for i, c := range sources {
		parts[i] = fmt.Sprintf("%s %dw", c.url, c.width)
	}
	return strings.Join(parts, ", ")
}

// imageSizes is wp_calculate_image_sizes().
func imageSizes(w int) string {
	return fmt.Sprintf("(max-width: %dpx) 100vw, %dpx", w, w)
}

// imgAddSrcsetSizes is wp_img_tag_add_srcset_and_sizes_attr() / wp_image_add_srcset_and_sizes().
func (s *Server) imgAddSrcsetSizes(ctx context.Context, img string, a *store.Attachment) string {
	if len(a.Variants) == 0 {
		return img
	}
	src := imgSrc(img)
	if src == "" {
		return img
	}
	var w, h int
	if mw, mh := imgWidthRe.FindStringSubmatch(img), imgHeightRe.FindStringSubmatch(img); mw != nil && mh != nil {
		w, _ = strconv.Atoi(mw[1])
		h, _ = strconv.Atoi(mh[1])
	}
	if w == 0 || h == 0 {
		var ok bool
		if w, h, ok = imageSrcDimensions(src, a); !ok {
			return img
		}
	}
	srcset := s.imageSrcset(ctx, src, w, h, a)
	if srcset == "" {
		return img
	}
	attr := ` srcset="` + html.EscapeString(srcset) + `"`
	if !strings.Contains(img, " sizes=") {
		attr += ` sizes="` + html.EscapeString(imageSizes(w)) + `"`
	}
	if m := imgEndRe.FindStringSubmatch(img); m != nil {
		return "<img " + m[1] + attr + " />"
	}
	return img
}

// imgAddLoadingAttrs is wp_img_tag_add_loading_optimization_attrs(): decoding="async" for every image,
// loading="lazy" for images outside the first screenful and fetchpriority="high" for the first large
// one, following wp_get_loading_optimization_attributes().
func imgAddLoadingAttrs(rc *renderCtx, img string) string {
	if !strings.Contains(img, ` src="`) {
		return img
	}
	loadingVal, priorityVal, decodingVal := "", "", ""
	if m := imgLoadingRe.FindStringSubmatch(img); m != nil {
		loadingVal = m[1]
	}
	if m := imgPriorityRe.FindStringSubmatch(img); m != nil {
		priorityVal = m[1]
	}
	if m := imgDecodingRe.FindStringSubmatch(img); m != nil {
		decodingVal = m[1]
	}
	var w, h int
	if mw, mh := imgWidthRe.FindStringSubmatch(img), imgHeightRe.FindStringSubmatch(img); mw != nil && mh != nil {
		w, _ = strconv.Atoi(mw[1])
		h, _ = strconv.Atoi(mh[1])
	}
	loading, priority := "", ""
	if w > 0 && h > 0 {
		var inViewport *bool
		t, f := true, false
		if loadingVal != "" {
			if loadingVal == "lazy" {
				inViewport = &f
			} else {
				inViewport = &t
			}
		}
		if priorityVal == "high" {
			inViewport = &t
		}
		increase := false
		if inViewport == nil && rc != nil && rc.mainLoop {
			increase = true
			if rc.mediaCount < omitLoadingAttrThreshold {
				inViewport = &t
			} else {
				inViewport = &f
			}
		}
		if inViewport != nil && *inViewport {
			// wp_maybe_add_fetchpriority_high_attr()
			if rc != nil && priorityVal == "" && loadingVal != "lazy" && !rc.priorityDone && w*h >= minPriorityImgPixels {
				priority = "high"
				rc.priorityDone = true
			}
		} else if rc == nil || !rc.feed { // wp_lazy_loading_enabled() is off in feeds
			loading = "lazy"
		}
		if increase {
			rc.mediaCount++
		}
	}
	if loading != "" && loadingVal == "" {
		img = strings.Replace(img, "<img", `<img loading="`+loading+`"`, 1)
	}
	if priority != "" && priorityVal == "" {
		img = strings.Replace(img, "<img", `<img fetchpriority="`+priority+`"`, 1)
	}
	if decodingVal == "" {
		img = strings.Replace(img, "<img", `<img decoding="async"`, 1)
	}
	return img
}

// imgAddAutoSizes is wp_img_tag_add_auto_sizes(): lazy-loaded responsive images get sizes="auto, …".
func imgAddAutoSizes(img string) string {
	m := imgLoadingRe.FindStringSubmatch(img)
	if m == nil || m[1] != "lazy" {
		return img
	}
	sm := imgSizesRe.FindStringSubmatchIndex(img)
	if sm == nil {
		return img
	}
	sizes := img[sm[2]:sm[3]]
	if sizesValidAutoRe.MatchString(sizes) {
		return img
	}
	return img[:sm[2]] + "auto, " + img[sm[2]:]
}
