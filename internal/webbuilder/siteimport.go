package webbuilder

import (
	"fmt"
	"path"
	"slices"
	"sort"
	"strings"

	"golang.org/x/net/html"
)

// ConvertSite imports a whole static site (a folder of HTML pages whose files are a design kit) as
// code sections: every page through ConvertCode with Split, the pages' copies of the header and of
// the footer clustered into as few templates as reproduce them (ClusterParts: copies that differ
// only in the current page's menu link are one template with an ActiveRule), links between the
// pages mapped to their site URLs. Creating the pages and templates is the caller's
// (internal/server/webbuilder_siteimport.go): it plans the site's URLs first (PlanSite), creates
// the pages, then converts with their real URLs.

// SitePage is one HTML file of the site.
type SitePage struct {
	// Path is the file's path in the folder (= in the kit): "index.html", "hizmet/3.html".
	Path string
	HTML string
}

// SitePlanPage is where a file goes.
type SitePlanPage struct {
	Path string `json:"path"`
	// Slug and Parent: the page's slug and the folder key of its parent ("" at the top; folders are
	// "dir:hizmet"; a folder's own page is the page of "hizmet.html" or "hizmet/index.html").
	Slug   string `json:"slug"`
	Parent string `json:"parent,omitempty"`
	// Front: index.html at the top (the front page); NotFound: 404.html (the 404 template).
	Front    bool `json:"front,omitempty"`
	NotFound bool `json:"notFound,omitempty"`
}

// SitePlanFolder is a folder that needs a page of its own as the parent of its pages.
type SitePlanFolder struct {
	Key    string `json:"key"` // "dir:hizmet", "dir:a/b"
	Slug   string `json:"slug"`
	Parent string `json:"parent,omitempty"`
	// Page: the file that is the folder's page ("hizmet.html", "hizmet/index.html"); "" = none, the
	// caller creates a placeholder parent (a draft, so the folder's own URL stays a 404 as in the
	// design).
	Page string `json:"page,omitempty"`
}

// SitePlan is the page tree of a site.
type SitePlan struct {
	Pages   []SitePlanPage   `json:"pages"`
	Folders []SitePlanFolder `json:"folders"`
}

// URLs is the site path each planned page gets once published ("/" for the front page,
// "/hizmet/3/", "/kurumsal/"): the page's slug under the URL of its parent folder (a folder with a
// page of its own takes that page's URL, a placeholder folder its slug). The 404 page has none.
func (p SitePlan) URLs() map[string]string {
	pages := map[string]SitePlanPage{}
	for _, pg := range p.Pages {
		pages[pg.Path] = pg
	}
	folders := map[string]SitePlanFolder{}
	for _, f := range p.Folders {
		folders[f.Key] = f
	}
	var folderURL, pageURL func(key string, depth int) string
	under := func(parent string, depth int) string {
		if parent == "" {
			return "/"
		}
		return folderURL(parent, depth+1)
	}
	folderURL = func(key string, depth int) string {
		f, ok := folders[key]
		if !ok || depth > 32 {
			return "/"
		}
		if f.Page != "" {
			return pageURL(f.Page, depth+1)
		}
		return under(f.Parent, depth) + f.Slug + "/"
	}
	pageURL = func(path string, depth int) string {
		pg, ok := pages[path]
		if !ok || depth > 32 {
			return "/"
		}
		if pg.Front {
			return "/"
		}
		return under(pg.Parent, depth) + pg.Slug + "/"
	}
	out := map[string]string{}
	for _, pg := range p.Pages {
		if !pg.NotFound {
			out[pg.Path] = pageURL(pg.Path, 0)
		}
	}
	return out
}

// IsSitePage reports whether a file is one of the site's pages.
func IsSitePage(p string) bool {
	ext := strings.ToLower(path.Ext(p))
	return ext == ".html" || ext == ".htm"
}

// PlanSite plans the pages of a folder: index.html at the top is the front page, 404.html the 404
// template; a/b.html is page b under the page of folder a; a/index.html is folder a's page.
func PlanSite(files []string) SitePlan {
	var plan SitePlan
	files = slices.Clone(files)
	sort.Strings(files)
	have := map[string]string{}
	for _, f := range files {
		ext := path.Ext(f)
		name := strings.TrimSuffix(path.Base(f), ext)
		lowerExt := strings.ToLower(ext)
		key := strings.TrimSuffix(f, ext) + lowerExt
		have[f] = f
		if have[key] == "" {
			have[key] = f
		}
		if strings.EqualFold(name, "index") {
			key = strings.TrimSuffix(f, name+ext) + "index" + lowerExt
			if have[key] == "" {
				have[key] = f
			}
		}
	}
	folderPage := func(dir string) string {
		for _, c := range []string{dir + ".html", dir + ".htm", dir + "/index.html", dir + "/index.htm"} {
			if page := have[c]; page != "" {
				return page
			}
		}
		return ""
	}
	folders := map[string]bool{}
	var addFolder func(dir string)
	addFolder = func(dir string) {
		if dir == "" || dir == "." || folders[dir] {
			return
		}
		folders[dir] = true
		parent := path.Dir(dir)
		if parent == "." {
			parent = ""
		}
		addFolder(parent)
		f := SitePlanFolder{Key: "dir:" + dir, Slug: siteSlug(path.Base(dir)), Page: folderPage(dir)}
		if parent != "" {
			f.Parent = "dir:" + parent
		}
		plan.Folders = append(plan.Folders, f)
	}
	for _, f := range files {
		if !IsSitePage(f) {
			continue
		}
		dir, base := path.Dir(f), path.Base(f)
		if dir == "." {
			dir = ""
		}
		name := strings.TrimSuffix(base, path.Ext(base))
		p := SitePlanPage{Path: f}
		switch {
		case dir == "" && strings.EqualFold(name, "index"):
			p.Front, p.Slug = true, "home"
		case dir == "" && name == "404":
			p.NotFound = true
		case strings.EqualFold(name, "index"):
			// a folder's own page: the folder's slug, under the folder's parent
			p.Slug = siteSlug(path.Base(dir))
			if pd := path.Dir(dir); pd != "." {
				addFolder(pd)
				p.Parent = "dir:" + pd
			}
		default:
			p.Slug = siteSlug(name)
			if dir != "" {
				addFolder(dir)
				p.Parent = "dir:" + dir
			}
		}
		plan.Pages = append(plan.Pages, p)
	}
	return plan
}

// siteSlug is a file or folder name as a slug: lowercase letters, digits and dashes.
func siteSlug(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		case r > 127:
			b.WriteRune(r) // the site sanitizes (and keeps) other letters
			dash = false
		default:
			if !dash && b.Len() > 0 {
				b.WriteByte('-')
				dash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// SiteImportOptions configures ConvertSite.
type SiteImportOptions struct {
	KitURL   string
	KitFiles map[string]bool
	// URLs: each page file → its site path ("/", "/hakkimizda/", "/hizmet/3/"); the 404 page has
	// none. Every page link of the design is mapped through it.
	URLs map[string]string
	// NoSplit keeps every page's header and footer in the page (no header or footer templates).
	NoSplit bool
	// OldHosts, KeepOldLinks, SiteURL: the design's original site (CodeImportOptions, oldsite.go).
	OldHosts     []string
	KeepOldLinks bool
	SiteURL      string
	// Remap: files outside the kit with a URL of their own (CodeImportOptions.Remap).
	Remap map[string]string
}

// SitePageImport is one converted page.
type SitePageImport struct {
	Path string      `json:"path"`
	URL  string      `json:"url,omitempty"`
	Code *CodeImport `json:"-"`
	// Header and Footer: the index of the page's template in SiteImport.Headers / Footers (-1: none).
	Header int `json:"header"`
	Footer int `json:"footer"`
}

// SiteTemplate is a header or footer template: its sections (in order), the head markup they need,
// the rule that marks the current page's link, and the pages it serves (indexes into Pages).
type SiteTemplate struct {
	Sections []CodePart  `json:"-"`
	Head     string      `json:"-"`
	Lang     string      `json:"-"`
	Attrs    [2]string   `json:"-"` // html and body attributes
	Rule     *ActiveRule `json:"-"`
	Pages    []int       `json:"pages"`
}

// SiteImport is the result of ConvertSite.
type SiteImport struct {
	Pages    []SitePageImport `json:"pages"`
	Headers  []SiteTemplate   `json:"headers"`
	Footers  []SiteTemplate   `json:"footers"`
	Warnings []string         `json:"warnings,omitempty"`
	Unmapped []string         `json:"unmappedLinks,omitempty"`
	Missing  []string         `json:"missingFiles,omitempty"`
	// The design's original site (oldsite.go): the URLs of OldHosts the pages held (as written),
	// the pages whose forms posted there ("blog/index.html: 2"), how many script URLs now point at
	// this site, and the third-party tracking the pages load.
	OldURLs      []string `json:"oldUrls,omitempty"`
	OldForms     []string `json:"oldForms,omitempty"`
	OldInScripts int      `json:"oldInScripts,omitempty"`
	Trackers     []string `json:"trackers,omitempty"`
}

// ConvertSite converts the site's pages (see the file comment).
func ConvertSite(pages []SitePage, o SiteImportOptions) (*SiteImport, error) {
	if len(pages) == 0 {
		return nil, fmt.Errorf("no pages")
	}
	links := map[string]string{}
	for p, u := range o.URLs {
		if u == "" {
			continue
		}
		links[p] = u
		dir, base := path.Dir(p), path.Base(p)
		if strings.EqualFold(strings.TrimSuffix(base, path.Ext(base)), "index") {
			// links to the folder itself ("hizmet/", "./", "/")
			if dir == "." {
				dir = ""
			}
			links[dir] = u
		}
	}
	out := &SiteImport{}
	unmapped, missing := map[string]bool{}, map[string]bool{}
	oldURLs, trackers := map[string]bool{}, map[string]bool{}
	var hs, fs []ActiveSample
	var hIdx, fIdx []int
	ids := make([]map[string]bool, len(pages))
	for i, p := range pages {
		ids[i] = volatileIDs(p.HTML)
	}
	pageOpts := func(i int) CodeImportOptions {
		base := path.Dir(pages[i].Path)
		if base == "." {
			base = ""
		}
		return CodeImportOptions{KitURL: o.KitURL, KitFiles: o.KitFiles, Base: base, LinkMap: links, Split: !o.NoSplit,
			Remap: o.Remap, OldHosts: o.OldHosts, KeepOldLinks: o.KeepOldLinks, SiteURL: o.SiteURL}
	}
	var footers map[int][]int
	if !o.NoSplit {
		footers = siteFooterHints(pages, ids, pageOpts)
	}
	for i, p := range pages {
		po := pageOpts(i)
		po.FooterAt = footers[i]
		res, err := ConvertCode(p.HTML, po)
		if err != nil {
			return nil, fmt.Errorf("%s: %v", p.Path, err)
		}
		for _, u := range res.OldURLs {
			oldURLs[u] = true
		}
		for _, t := range res.Trackers {
			trackers[t] = true
		}
		if res.OldForms > 0 {
			out.OldForms = append(out.OldForms, fmt.Sprintf("%s: %d", p.Path, res.OldForms))
		}
		out.OldInScripts += res.OldInScripts
		for _, u := range res.Unmapped {
			unmapped[p.Path+": "+u] = true
		}
		for _, m := range res.Missing {
			missing[m] = true
		}
		for _, w := range res.Warnings {
			out.Warnings = append(out.Warnings, p.Path+": "+w)
		}
		out.Pages = append(out.Pages, SitePageImport{Path: p.Path, URL: o.URLs[p.Path], Code: res, Header: -1, Footer: -1})
		if res.Header != nil {
			var parts []string
			for _, b := range res.HeaderBefore {
				parts = append(parts, b.Wrap+"\x05"+b.HTML)
			}
			parts = append(parts, res.Header.Wrap+"\x05"+res.Header.HTML)
			hs = append(hs, ActiveSample{Path: o.URLs[p.Path], Parts: parts, Volatile: ids[i]})
			hIdx = append(hIdx, i)
		}
		// A footer the design put inside main stays on its page (FooterInPage): no template serves
		// that page, or it would show the footer twice.
		if res.Footer != nil && !res.FooterInPage {
			parts := []string{res.Footer.Wrap + "\x05" + res.Footer.HTML}
			for _, a := range res.FooterAfter {
				parts = append(parts, a.Wrap+"\x05"+a.HTML)
			}
			fs = append(fs, ActiveSample{Path: o.URLs[p.Path], Parts: parts, Volatile: ids[i]})
			fIdx = append(fIdx, i)
		}
	}
	for u := range unmapped {
		out.Unmapped = append(out.Unmapped, u)
	}
	for m := range missing {
		out.Missing = append(out.Missing, m)
	}
	sort.Strings(out.Unmapped)
	sort.Strings(out.Missing)
	for u := range oldURLs {
		out.OldURLs = append(out.OldURLs, u)
	}
	for t := range trackers {
		out.Trackers = append(out.Trackers, t)
	}
	sort.Strings(out.OldURLs)
	sort.Strings(out.Trackers)
	out.Headers = siteTemplates(out, hs, hIdx, true)
	out.Footers = siteTemplates(out, fs, fIdx, false)
	return out, nil
}

// siteTemplates clusters the pages' copies of a part into templates.
func siteTemplates(out *SiteImport, samples []ActiveSample, idx []int, header bool) []SiteTemplate {
	if len(samples) == 0 {
		return nil
	}
	// The wrap of each part travels with its markup ("wrap\x05html") so copies in other wrappers
	// are other parts; ClusterParts sees only the markup.
	split := func(s string) (string, string) {
		w, h, _ := strings.Cut(s, "\x05")
		return w, h
	}
	wraps := make([][]string, len(samples))
	plain := make([]ActiveSample, len(samples))
	for i, s := range samples {
		plain[i].Path, plain[i].Volatile = s.Path, s.Volatile
		for _, p := range s.Parts {
			w, h := split(p)
			wraps[i] = append(wraps[i], w)
			plain[i].Parts = append(plain[i].Parts, h)
		}
	}
	// Copies in different wrappers never share a template.
	byWrap := map[string][]int{}
	var order []string
	for i := range plain {
		k := strings.Join(wraps[i], "\x06")
		if _, ok := byWrap[k]; !ok {
			order = append(order, k)
		}
		byWrap[k] = append(byWrap[k], i)
	}
	var list []SiteTemplate
	for _, k := range order {
		members := byWrap[k]
		sub := make([]ActiveSample, len(members))
		for j, m := range members {
			sub[j] = plain[m]
		}
		for _, cl := range ClusterParts(sub) {
			first := members[cl.Members[0]]
			res := out.Pages[idx[first]].Code
			// The template's head: what every page it serves has in its head (a page's own
			// structured data, a stylesheet with its own cache-buster stay in that page's head).
			heads := make([]string, 0, len(cl.Members))
			for _, m := range cl.Members {
				heads = append(heads, out.Pages[idx[members[m]]].Code.Head)
			}
			// Its <html> and <body> attributes: those every page it serves has (a page's own
			// body classes — WordPress's page-id-10, home — stay that page's).
			var hattrs, battrs []string
			for _, m := range cl.Members {
				pc := out.Pages[idx[members[m]]].Code
				hattrs, battrs = append(hattrs, pc.HTMLAttrs), append(battrs, pc.BodyAttrs)
			}
			t := SiteTemplate{Head: CommonHead(heads), Lang: res.Lang, Attrs: [2]string{CommonAttrs(hattrs), CommonAttrs(battrs)}, Rule: cl.Rule}
			names := sitePartNames(res, header)
			for j, h := range cl.Parts {
				name := "Part"
				if j < len(names) {
					name = names[j]
				}
				t.Sections = append(t.Sections, CodePart{Name: name, HTML: h, Wrap: wraps[first][j]})
			}
			for _, m := range cl.Members {
				t.Pages = append(t.Pages, idx[members[m]])
			}
			list = append(list, t)
		}
	}
	sort.SliceStable(list, func(i, j int) bool { return len(list[i].Pages) > len(list[j].Pages) })
	for ti, t := range list {
		for _, pi := range t.Pages {
			if header {
				out.Pages[pi].Header = ti
			} else {
				out.Pages[pi].Footer = ti
			}
		}
	}
	return list
}

// CommonAttrs is the attributes (an opening tag's, `class="a b" data-x="1"`) that every one of list
// has: an attribute with the same value in all, and the classes all of them have.
func CommonAttrs(list []string) string {
	if len(list) == 0 {
		return ""
	}
	if len(list) == 1 {
		return list[0]
	}
	parse := func(s string) []html.Attribute {
		nodes, err := parseFragment("<div " + s + "></div>")
		if err != nil || len(nodes) == 0 {
			return nil
		}
		return nodes[0].Attr
	}
	first := parse(list[0])
	rest := make([][]html.Attribute, len(list)-1)
	for i, s := range list[1:] {
		rest[i] = parse(s)
	}
	get := func(attrs []html.Attribute, k string) (string, bool) {
		for _, a := range attrs {
			if a.Key == k && a.Namespace == "" {
				return a.Val, true
			}
		}
		return "", false
	}
	var b strings.Builder
	for _, a := range first {
		if a.Namespace != "" {
			continue
		}
		val := a.Val
		if a.Key == "class" {
			var keep []string
			for _, c := range strings.Fields(a.Val) {
				all := true
				for _, r := range rest {
					v, _ := get(r, "class")
					if !slices.Contains(strings.Fields(v), c) {
						all = false
						break
					}
				}
				if all {
					keep = append(keep, c)
				}
			}
			if len(keep) == 0 {
				continue
			}
			val = strings.Join(keep, " ")
		} else {
			all := true
			for _, r := range rest {
				if v, ok := get(r, a.Key); !ok || v != a.Val {
					all = false
					break
				}
			}
			if !all {
				continue
			}
		}
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(a.Key + `="` + html.EscapeString(val) + `"`)
	}
	return b.String()
}

// CommonHead is the head markup every one of heads has: the first head's top-level elements (and
// the text between them) that each other head holds as written, in order.
func CommonHead(heads []string) string {
	if len(heads) == 0 {
		return ""
	}
	if len(heads) == 1 {
		return heads[0]
	}
	other := make([][]string, 0, len(heads)-1)
	for _, h := range heads[1:] {
		other = append(other, headElements(h))
	}
	var keep []string
	for _, e := range headElements(heads[0]) {
		all := true
		for _, elements := range other {
			if !slices.Contains(elements, e) {
				all = false
				break
			}
		}
		if all {
			keep = append(keep, e)
		}
	}
	return strings.Join(keep, "\n")
}

// headElements splits head markup (one top-level element per line, as ConvertCode writes it; an
// element over several lines, an inline style or script, is kept whole) into its elements.
func headElements(head string) []string {
	lines := strings.Split(head, "\n")
	// The head markup is one top-level element per line (ConvertCode's head); an element over
	// several lines (an inline style) is kept whole.
	var elems []string
	var cur strings.Builder
	depth := 0
	for _, l := range lines {
		if cur.Len() > 0 {
			cur.WriteByte('\n')
		}
		cur.WriteString(l)
		depth += strings.Count(l, "<style") + strings.Count(l, "<script") + strings.Count(l, "<noscript") -
			strings.Count(l, "</style>") - strings.Count(l, "</script>") - strings.Count(l, "</noscript>")
		if depth <= 0 {
			elems = append(elems, cur.String())
			cur.Reset()
			depth = 0
		}
	}
	if cur.Len() > 0 {
		elems = append(elems, cur.String())
	}
	return elems
}

// Uncovered lists the pages (indexes into Pages) that no header (header true) or footer template
// serves: the design has none there (a bare 404 page, a landing page without a footer) or keeps it
// in the page (FooterInPage). The whole-site template must exclude them, or the theme draws the
// design's header or footer where the design has none.
func (s *SiteImport) Uncovered(header bool) []int {
	var out []int
	for i, p := range s.Pages {
		if (header && p.Header < 0) || (!header && p.Footer < 0) {
			out = append(out, i)
		}
	}
	return out
}

func sitePartNames(res *CodeImport, header bool) []string {
	var out []string
	if header {
		for _, b := range res.HeaderBefore {
			out = append(out, b.Name)
		}
		return append(out, "Header")
	}
	out = append(out, "Footer")
	for _, a := range res.FooterAfter {
		out = append(out, a.Name)
	}
	return out
}
