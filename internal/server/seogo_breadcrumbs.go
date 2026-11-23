package server

import (
	"context"
	"html"
	"strconv"
	"strings"

	"wpgo/internal/blocks"
	"wpgo/internal/store"
)

// Breadcrumbs: the trail of a context (also BreadcrumbList in the schema graph), rendered by the
// [seogo_breadcrumb] shortcode (and Yoast's [wpseo_breadcrumb] / Rank Math's [rank_math_breadcrumb]),
// the seogo/breadcrumbs block (and yoast-seo/breadcrumbs), and optionally above singular content.

type seogoCrumb struct {
	Name, URL string
}

func (sc *seogoCtx) crumbName(metaTitle, fallback string) string {
	if metaTitle != "" {
		return metaTitle
	}
	return fallback
}

// breadcrumbs builds the trail: Home › (blog page) › category ancestors › post, or the archive.
func (sc *seogoCtx) breadcrumbs() []seogoCrumb {
	o := sc.s.seogoOpts()
	lb := sc.lb
	tr := func(v string) string { return sc.s.seogoTr(sc.ctx, v) }
	home := tr(o.BCHome)
	if home == "" {
		home = "Home"
	}
	trail := []seogoCrumb{{Name: home, URL: sc.homeURL()}}
	blog := func() {
		if !o.BCShowBlog || sc.s.settings.String("show_on_front") != "page" {
			return
		}
		if id := int64(sc.s.settings.Int("page_for_posts")); id > 0 {
			if p, err := sc.s.store.GetPost(sc.ctx, id); err == nil && p.Status == "publish" {
				trail = append(trail, seogoCrumb{Name: sc.crumbName(seogoMeta(p.Meta, "bc_title"), p.Title), URL: lb.Permalink(p)})
			}
		}
	}
	termLink := func(t *store.Term) string {
		if sc.s.sitemapGooTaxonomy(t.Taxonomy) {
			return sc.s.gooTermLink(sc.ctx, t)
		}
		return lb.TermLink(t)
	}
	termChain := func(id int64) {
		var chain []seogoCrumb
		seen := map[int64]bool{}
		for id > 0 && !seen[id] {
			seen[id] = true
			t, err := sc.s.store.GetTerm(gooPllUnfiltered(sc.ctx), id)
			if err != nil {
				break
			}
			chain = append([]seogoCrumb{{Name: sc.crumbName(seogoMeta(t.Meta, "bc_title"), t.Name), URL: termLink(t)}}, chain...)
			id = t.ParentID
		}
		trail = append(trail, chain...)
	}
	// shop is the Pursae Commerce shop crumb (WooCommerce's breadcrumb puts it before products and
	// product terms when the shop page is set).
	shop := func() {
		id := sc.s.gooPageIDIn(sc.ctx, "shop")
		if id <= 0 {
			return
		}
		if p, err := sc.s.store.GetPost(gooPllUnfiltered(sc.ctx), id); err == nil && p.Status == "publish" {
			trail = append(trail, seogoCrumb{Name: sc.crumbName(seogoMeta(p.Meta, "bc_title"), p.Title), URL: sc.s.gooShopURL(sc.ctx)})
		}
	}
	switch {
	case sc.Store == "shop" && sc.Post != nil:
		trail = append(trail, seogoCrumb{Name: sc.crumbName(seogoMeta(sc.Post.Meta, "bc_title"), sc.Post.Title), URL: sc.url()})
		return trail
	case sc.Store == "product" && sc.Post != nil:
		shop()
		if m, _ := sc.s.store.TermsForPosts(gooPllUnfiltered(sc.ctx), []int64{sc.Post.ID}, "product_cat"); len(m[sc.Post.ID]) > 0 {
			cats := m[sc.Post.ID]
			pick := cats[0]
			if id, _ := strconv.ParseInt(seogoMeta(sc.Post.Meta, "primary_category"), 10, 64); id > 0 {
				for _, t := range cats {
					if t.ID == id {
						pick = t
					}
				}
			}
			termChain(pick.ID)
		}
		trail = append(trail, seogoCrumb{Name: sc.crumbName(seogoMeta(sc.Post.Meta, "bc_title"), sc.Post.Title), URL: sc.url()})
		return trail
	case sc.Kind == "taxonomy" && sc.Term != nil:
		if sc.storeArchive() {
			shop()
		}
		termChain(sc.Term.ID)
		return trail
	case sc.Kind == "post-type-archive" && sc.PostType != "":
		trail = append(trail, seogoCrumb{Name: seogoTypeLabels(sc.PostType).Name, URL: sc.url()})
		return trail
	}
	switch sc.Kind {
	case "front-page":
		return trail
	case "home":
		if sc.Post != nil {
			trail = append(trail, seogoCrumb{Name: sc.crumbName(seogoMeta(sc.Post.Meta, "bc_title"), sc.Post.Title), URL: sc.url()})
		}
	case "single", "page":
		p := sc.Post
		if p.Type == "post" {
			blog()
			if t := sc.primaryCategory(); t != nil {
				termChain(t.ID)
			}
		} else if p.Type == "page" {
			var chain []seogoCrumb
			seen := map[int64]bool{p.ID: true}
			for id := p.ParentID; id > 0 && !seen[id]; {
				seen[id] = true
				parent, err := sc.s.store.GetPost(sc.ctx, id)
				if err != nil {
					break
				}
				chain = append([]seogoCrumb{{Name: sc.crumbName(seogoMeta(parent.Meta, "bc_title"), parent.Title), URL: lb.Permalink(parent)}}, chain...)
				id = parent.ParentID
			}
			trail = append(trail, chain...)
		}
		trail = append(trail, seogoCrumb{Name: sc.crumbName(seogoMeta(p.Meta, "bc_title"), p.Title), URL: lb.Permalink(p)})
	case "category", "tag":
		blog()
		prefix := strings.TrimSpace(tr(o.BCArchive))
		if sc.Term.Taxonomy == "category" {
			termChain(sc.Term.ID)
		} else {
			trail = append(trail, seogoCrumb{Name: sc.crumbName(seogoMeta(sc.Term.Meta, "bc_title"), sc.Term.Name), URL: lb.TermLink(sc.Term)})
		}
		if prefix != "" {
			trail[len(trail)-1].Name = prefix + " " + trail[len(trail)-1].Name
		}
	case "author":
		if a := sc.author(); a != nil {
			name := a.DisplayName
			if p := strings.TrimSpace(tr(o.BCArchive)); p != "" {
				name = p + " " + name
			}
			trail = append(trail, seogoCrumb{Name: name, URL: sc.url()})
		}
	case "date":
		name := sc.archiveDate()
		if p := strings.TrimSpace(tr(o.BCArchive)); p != "" {
			name = p + " " + name
		}
		trail = append(trail, seogoCrumb{Name: name, URL: sc.url()})
	case "search":
		trail = append(trail, seogoCrumb{Name: strings.TrimSpace(tr(o.BCSearch) + " " + sc.Search)})
	case "404":
		trail = append(trail, seogoCrumb{Name: tr(o.BC404)})
	}
	return trail
}

// breadcrumbHTML renders the trail like Yoast (span#breadcrumbs) with Rank Math's class names.
func (sc *seogoCtx) breadcrumbHTML() string {
	o := sc.s.seogoOpts()
	trail := sc.breadcrumbs()
	if len(trail) < 2 && sc.Kind != "front-page" {
		return ""
	}
	sep := o.BCSep
	if sep == "" {
		sep = "»"
	}
	esc := html.EscapeString
	var parts []string
	for i, c := range trail {
		last := i == len(trail)-1
		switch {
		case last && o.BCHideTitle && len(trail) > 1:
			continue
		case last:
			name := esc(c.Name)
			if o.BCBoldLast {
				name = "<strong>" + name + "</strong>"
			}
			parts = append(parts, `<span class="last breadcrumb_last" aria-current="page">`+name+`</span>`)
		case c.URL != "":
			parts = append(parts, `<span><a href="`+esc(c.URL)+`">`+esc(c.Name)+`</a></span>`)
		default:
			parts = append(parts, `<span>`+esc(c.Name)+`</span>`)
		}
	}
	prefix := ""
	if o.BCPrefix != "" {
		prefix = `<span class="label">` + esc(sc.s.seogoTr(sc.ctx, o.BCPrefix)) + `</span> `
	}
	return `<nav aria-label="breadcrumbs" class="seogo-breadcrumb rank-math-breadcrumb"><p id="breadcrumbs">` + prefix +
		strings.Join(parts, ` <span class="separator"> `+esc(sep)+` </span> `) + `</p></nav>`
}

// seogoBreadcrumbsFor is the trail of the request being rendered (shortcode and block).
func (s *Server) seogoBreadcrumbsFor(ctx context.Context) string {
	if !s.seogoModule("breadcrumbs") {
		return ""
	}
	rc := renderFrom(ctx)
	if rc == nil || rc.q == nil {
		return ""
	}
	return s.seogoCtxFor(ctx, rc.q).breadcrumbHTML()
}

var seogoBreadcrumbTags = []string{"seogo_breadcrumb", "wpseo_breadcrumb", "rank_math_breadcrumb"}

// seogoContent is Pursae SEO's the_content filter: Image SEO, tables of contents and the breadcrumb shortcodes.
func (s *Server) seogoContent(ctx context.Context, content string) string {
	if !s.seogoOn() {
		return content
	}
	content = s.seogoImageSEO(ctx, content)
	content = s.seogoTOCFor(ctx, content)
	if !strings.Contains(content, "_breadcrumb") {
		return content
	}
	for _, tag := range seogoBreadcrumbTags {
		for _, form := range []string{"[" + tag + "]", "[" + tag + " /]", "[" + tag + "/]"} {
			if strings.Contains(content, form) {
				content = strings.ReplaceAll(content, form, s.seogoBreadcrumbsFor(ctx))
			}
		}
	}
	return content
}

// seogoRegisterBlocks registers the server renderers of Pursae SEO's blocks. They return the saved
// markup untouched while the plugin is off, so the content looks the same either way. The block
// registry is shared by every Server in the process: the renderer uses the server of the render
// context (a theme-preview sandbox has its own), falling back to the one that registered it.
func (s *Server) seogoRegisterBlocks() {
	bc := func(ctx context.Context, _ *blocks.Block, content string) string {
		s := s
		if rc := renderFrom(ctx); rc != nil && rc.s != nil {
			s = rc.s
		}
		if !s.seogoOn() {
			return content
		}
		return s.seogoBreadcrumbsFor(ctx)
	}
	blocks.RegisterDynamic("seogo/breadcrumbs", bc)
	blocks.RegisterDynamic("yoast-seo/breadcrumbs", bc)
	s.seogoRegisterTOCBlock()
}
