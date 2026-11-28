package server

import (
	"context"
	"fmt"
	"html"
	"strconv"
	"strings"
	"time"

	"wpgo/internal/blocks"
	"wpgo/internal/store"
	"wpgo/internal/wp"
)

// Server-rendered Query Loop family and the post/site blocks of the block theme set: core/query,
// post-template, query-pagination (+ next/previous/numbers), query-no-results, post-title,
// post-excerpt, post-date, post-featured-image, post-content, read-more, site-title and site-tagline
// (wp-includes/blocks/query.php, post-template.php, query-pagination*.php, post-*.php, site-*.php).

type queryState struct {
	id         int
	attrs      map[string]any
	inherit    bool
	page       int
	totalPages int
	total      int
	ran        bool
}

type queryStateKey struct{}

func queryFrom(ctx context.Context) *queryState {
	q, _ := ctx.Value(queryStateKey{}).(*queryState)
	return q
}

func registerQueryBlocks(s *Server) {
	blocks.RegisterContainer("core/query", renderQueryBlock)
	blocks.RegisterContainer("core/post-template", renderPostTemplateBlock)
	blocks.RegisterContainer("core/query-no-results", func(ctx context.Context, b *blocks.Block, _ string) string {
		if q := queryFrom(ctx); q != nil && q.ran && q.total == 0 {
			return blocks.RenderInner(ctx, b)
		}
		return ""
	})
	blocks.RegisterDynamic("core/query-pagination", func(ctx context.Context, b *blocks.Block, content string) string {
		if q := queryFrom(ctx); q == nil || q.totalPages < 2 {
			return ""
		}
		return `<nav ` + wrapperClass(b, "") + ` aria-label="Pagination">` + content + `</nav>`
	})
	blocks.RegisterDynamic("core/query-pagination-next", func(ctx context.Context, b *blocks.Block, _ string) string {
		q, rc := queryFrom(ctx), renderFrom(ctx)
		if q == nil || rc == nil || q.page >= q.totalPages {
			return ""
		}
		label := attrStr(b, "label", "Next Page")
		return `<a href="` + html.EscapeString(queryPageURL(rc, q, q.page+1)) + `" ` + wrapperClass(b, "") + `>` + html.EscapeString(label) + ` <span class="wp-block-query-pagination-next-arrow is-arrow-arrow" aria-hidden="true">&rarr;</span></a>`
	})
	blocks.RegisterDynamic("core/query-pagination-previous", func(ctx context.Context, b *blocks.Block, _ string) string {
		q, rc := queryFrom(ctx), renderFrom(ctx)
		if q == nil || rc == nil || q.page <= 1 {
			return ""
		}
		label := attrStr(b, "label", "Previous Page")
		return `<a href="` + html.EscapeString(queryPageURL(rc, q, q.page-1)) + `" ` + wrapperClass(b, "") + `><span class="wp-block-query-pagination-previous-arrow is-arrow-arrow" aria-hidden="true">&larr;</span> ` + html.EscapeString(label) + `</a>`
	})
	blocks.RegisterDynamic("core/query-pagination-numbers", func(ctx context.Context, b *blocks.Block, _ string) string {
		q, rc := queryFrom(ctx), renderFrom(ctx)
		if q == nil || rc == nil || q.totalPages < 2 {
			return ""
		}
		var sb strings.Builder
		for i := 1; i <= q.totalPages; i++ {
			if i == q.page {
				fmt.Fprintf(&sb, `<span aria-current="page" class="page-numbers current">%d</span>`, i)
			} else {
				fmt.Fprintf(&sb, `<a class="page-numbers" href="%s">%d</a>`, html.EscapeString(queryPageURL(rc, q, i)), i)
			}
		}
		return `<div ` + wrapperClass(b, "") + `>` + sb.String() + `</div>`
	})

	blocks.RegisterDynamic("core/post-title", func(ctx context.Context, b *blocks.Block, _ string) string {
		rc, p := loopPost(ctx)
		if p == nil {
			return ""
		}
		title := p.Title
		if title == "" {
			return ""
		}
		level := attrInt(b, "level", 2)
		tag := "h" + strconv.Itoa(level)
		if level == 0 {
			tag = "p"
		} else if level < 1 || level > 6 {
			tag = "h2"
		}
		out := wp.Texturize(wp.EscText(title))
		if attrBool(b, "isLink", false) {
			rel := ""
			if r := attrStr(b, "rel", ""); r != "" {
				rel = ` rel="` + html.EscapeString(r) + `"`
			}
			target := ""
			if attrStr(b, "linkTarget", "_self") == "_blank" {
				target = ` target="_blank"`
			}
			out = `<a href="` + html.EscapeString(rc.lb.Permalink(p)) + `"` + target + rel + `>` + out + `</a>`
		}
		return `<` + tag + ` ` + wrapperClass(b, "") + `>` + out + `</` + tag + `>`
	})
	blocks.RegisterDynamic("core/post-excerpt", func(ctx context.Context, b *blocks.Block, _ string) string {
		rc, p := loopPost(ctx)
		if p == nil {
			return ""
		}
		if p.Password != "" && (rc.c == nil || rc.s.passwordRequired(rc.c, p)) {
			return ""
		}
		more := attrStr(b, "moreText", "")
		link := html.EscapeString(rc.lb.Permalink(p))
		ex := wp.KsesPost(p.Excerpt)
		if ex == "" {
			ex = wp.TrimWords(stripBlocks(p.Content), attrInt(b, "excerptLength", 55), " …")
		}
		out := `<p class="wp-block-post-excerpt__excerpt">` + ex
		if more != "" && attrBool(b, "showMoreOnNewLine", true) {
			out += `</p><p class="wp-block-post-excerpt__more-text"><a class="wp-block-post-excerpt__more-link" href="` + link + `">` + html.EscapeString(more) + `</a></p>`
		} else if more != "" {
			out += ` <a class="wp-block-post-excerpt__more-link" href="` + link + `">` + html.EscapeString(more) + `</a></p>`
		} else {
			out += `</p>`
		}
		return `<div ` + wrapperClass(b, "") + `>` + out + `</div>`
	})
	blocks.RegisterDynamic("core/post-date", func(ctx context.Context, b *blocks.Block, _ string) string {
		rc, p := loopPost(ctx)
		if p == nil {
			return ""
		}
		var t time.Time = p.Date().In(rc.s.loc())
		if attrStr(b, "displayType", "date") == "modified" {
			t = p.ModifiedAt.In(rc.s.loc())
		}
		format := attrStr(b, "format", "")
		if format == "" {
			format = rc.s.settings.String("date_format")
		}
		out := `<time datetime="` + t.Format(time.RFC3339) + `">` + html.EscapeString(wp.FormatDate(t, format)) + `</time>`
		if attrBool(b, "isLink", false) {
			out = `<a href="` + html.EscapeString(rc.lb.Permalink(p)) + `">` + out + `</a>`
		}
		return `<div ` + wrapperClass(b, "") + `>` + out + `</div>`
	})
	blocks.RegisterDynamic("core/post-featured-image", func(ctx context.Context, b *blocks.Block, _ string) string {
		rc, p := loopPost(ctx)
		if p == nil || p.FeaturedMediaID == 0 {
			return ""
		}
		a, err := rc.s.store.GetAttachment(ctx, p.FeaturedMediaID)
		if err != nil {
			return ""
		}
		size := attrStr(b, "sizeSlug", "post-thumbnail")
		src, w, h := rc.s.uploadURL(ctx, a.FilePath), a.Width, a.Height
		if v := a.Variant(size); v != nil {
			src, w, h = rc.s.uploadURL(ctx, v.FilePath), v.Width, v.Height
		}
		img := fmt.Sprintf(`<img src="%s" alt="%s" width="%d" height="%d" class="attachment-%s size-%s" loading="lazy" decoding="async" />`,
			html.EscapeString(src), html.EscapeString(a.Alt), w, h, html.EscapeString(size), html.EscapeString(size))
		if attrBool(b, "isLink", false) {
			img = `<a href="` + html.EscapeString(rc.lb.Permalink(p)) + `">` + img + `</a>`
		}
		return `<figure ` + wrapperClass(b, "") + `>` + img + `</figure>`
	})
	blocks.RegisterDynamic("core/post-content", func(ctx context.Context, b *blocks.Block, _ string) string {
		rc, p := loopPost(ctx)
		// render_block_core_post_content(): a post whose content is already rendering ($seen_ids)
		// renders empty.
		if p == nil || rc.depth >= 3 || rc.seenPosts[p.ID] {
			return ""
		}
		if p.Password != "" && (rc.c == nil || rc.s.passwordRequired(rc.c, p)) {
			return ""
		}
		// Memberships: the inner post's own plans apply, as on its page (its the_content is bypassed).
		if msg, hidden := rc.s.gmemNestedRestricted(ctx, rc.user, p.ID, false); hidden {
			return `<div ` + wrapperClass(b, "entry-content") + `>` + msg + `</div>`
		}
		if !rc.enterNested() {
			return ""
		}
		if rc.seenPosts == nil {
			rc.seenPosts = map[int64]bool{}
		}
		rc.seenPosts[p.ID] = true
		rc.depth++
		defer func() {
			rc.depth--
			delete(rc.seenPosts, p.ID)
		}()
		return `<div ` + wrapperClass(b, "entry-content") + `>` + blocks.Render(ctx, rc.s.gmemNestedShortcodes(ctx, rc.user, rc.s.cspTrustPostContent(ctx, p))) + `</div>`
	})
	blocks.RegisterDynamic("core/read-more", func(ctx context.Context, b *blocks.Block, _ string) string {
		rc, p := loopPost(ctx)
		if p == nil {
			return ""
		}
		text := attrStr(b, "content", "Read more")
		return `<a ` + wrapperClass(b, "") + ` href="` + html.EscapeString(rc.lb.Permalink(p)) + `" target="` + html.EscapeString(attrStr(b, "linkTarget", "_self")) + `">` + wp.KsesPost(text) + `</a>`
	})
	blocks.RegisterDynamic("core/site-title", func(ctx context.Context, b *blocks.Block, _ string) string {
		rc := renderFrom(ctx)
		if rc == nil {
			return ""
		}
		name := rc.s.settings.String("blogname")
		if name == "" {
			return ""
		}
		level := attrInt(b, "level", 1)
		tag := "h" + strconv.Itoa(level)
		if level == 0 {
			tag = "p"
		} else if level < 1 || level > 6 {
			tag = "h1"
		}
		out := html.EscapeString(name)
		if attrBool(b, "isLink", true) {
			target := ""
			if attrStr(b, "linkTarget", "_self") == "_blank" {
				target = ` target="_blank" rel="noopener noreferrer"`
			}
			out = `<a href="` + html.EscapeString(rc.lb.Front()) + `" rel="home"` + target + `>` + out + `</a>`
		}
		return `<` + tag + ` ` + wrapperClass(b, "") + `>` + out + `</` + tag + `>`
	})
	blocks.RegisterDynamic("core/site-tagline", func(ctx context.Context, b *blocks.Block, _ string) string {
		rc := renderFrom(ctx)
		if rc == nil {
			return ""
		}
		tag := rc.s.settings.String("blogdescription")
		if tag == "" {
			return ""
		}
		return `<p ` + wrapperClass(b, "") + `>` + html.EscapeString(tag) + `</p>`
	})
}

// loopPost is the post a post-* block renders: the current Query Loop item, else the post whose
// content is rendering, else the queried post.
func loopPost(ctx context.Context) (*renderCtx, *store.Post) {
	rc := renderFrom(ctx)
	if rc == nil {
		return nil, nil
	}
	id := rc.postID
	if id == 0 && rc.q != nil && rc.q.Post != nil {
		return rc, rc.q.Post
	}
	if id == 0 {
		return rc, nil
	}
	if rc.q != nil && rc.q.Post != nil && rc.q.Post.ID == id {
		return rc, rc.q.Post
	}
	p, err := rc.s.store.GetPost(ctx, id)
	if err != nil || p.Status != "publish" && (rc.user == nil || !rc.user.Can("read_post", p)) {
		return rc, nil
	}
	return rc, p
}

func queryPageURL(rc *renderCtx, q *queryState, n int) string {
	if rc.c == nil {
		return "#"
	}
	u := *rc.c.Request.URL
	if q.inherit {
		return rc.lb.Paged(rc.lb.HomeURL(u.Path), n)
	}
	v := u.Query()
	key := "query-" + strconv.Itoa(q.id) + "-page"
	if n <= 1 {
		v.Del(key)
	} else {
		v.Set(key, strconv.Itoa(n))
	}
	u.RawQuery = v.Encode()
	return u.String()
}

func renderQueryBlock(ctx context.Context, b *blocks.Block, _ string) string {
	rc := renderFrom(ctx)
	if rc == nil {
		return ""
	}
	attrs, _ := b.Attrs["query"].(map[string]any)
	if attrs == nil {
		attrs = map[string]any{}
	}
	st := &queryState{id: attrInt(b, "queryId", 0), attrs: attrs, page: 1}
	st.inherit, _ = attrs["inherit"].(bool)
	if st.inherit && rc.q != nil {
		st.page = max(rc.q.Paged, 1)
	} else if rc.c != nil {
		if n, _ := strconv.Atoi(rc.c.Query("query-" + strconv.Itoa(st.id) + "-page")); n > 1 {
			st.page = n
		}
	}
	inner := blocks.RenderInner(context.WithValue(ctx, queryStateKey{}, st), b)
	tag := attrStr(b, "tagName", "div")
	switch tag {
	case "div", "main", "section", "aside", "article":
	default:
		tag = "div"
	}
	return `<` + tag + ` ` + wrapperClass(b, "") + `>` + inner + `</` + tag + `>`
}

func qInts(v any) []int64 {
	arr, _ := v.([]any)
	var out []int64
	for _, e := range arr {
		switch n := e.(type) {
		case float64:
			out = append(out, int64(n))
		case string:
			if i, err := strconv.ParseInt(n, 10, 64); err == nil {
				out = append(out, i)
			}
		}
	}
	return out
}

func renderPostTemplateBlock(ctx context.Context, b *blocks.Block, _ string) string {
	rc, st := renderFrom(ctx), queryFrom(ctx)
	if rc == nil || st == nil || rc.depth >= 3 {
		return ""
	}
	var posts []*store.Post
	var err error
	a := st.attrs
	if st.inherit && rc.q != nil && rc.q.Kind != "" && rc.q.Post == nil {
		var total, per int
		posts, total, per, err = rc.s.mainPosts(ctx, rc.q, rc.user)
		if per > 0 {
			st.totalPages = (total + per - 1) / per
		}
		st.total = total
	} else {
		pt, _ := a["postType"].(string)
		if pt == "" {
			pt = "post"
		}
		per := 10
		if n, ok := a["perPage"].(float64); ok && n > 0 {
			per = int(n)
		} else if n := rc.s.settings.Int("posts_per_page"); n > 0 {
			per = n
		}
		per = min(per, 100)
		q := store.PostQuery{Types: []string{pt}, Statuses: []string{"publish"}, PerPage: per, Page: st.page, Loc: rc.s.loc(),
			OrderBy: "date", Order: "desc"}
		if o, _ := a["order"].(string); o != "" {
			q.Order = o
		}
		if o, _ := a["orderBy"].(string); o != "" {
			q.OrderBy = o
		}
		if tq, ok := a["taxQuery"].(map[string]any); ok {
			for _, ids := range tq {
				q.TermIDs = append(q.TermIDs, qInts(ids)...)
			}
		}
		if ids := qInts([]any{a["author"]}); len(ids) == 1 && ids[0] > 0 {
			q.AuthorID = ids[0]
		}
		if s, _ := a["search"].(string); s != "" {
			q.Search = s
		}
		q.ExcludeID = append(qInts(a["exclude"]), rc.s.gooListingExclude(ctx, pt)...)
		if sv, _ := a["sticky"].(string); sv == "only" {
			t := true
			q.Sticky = &t
		} else if sv == "exclude" {
			f := false
			q.Sticky = &f
		}
		var total int
		posts, total, err = rc.s.store.QueryPosts(ctx, q)
		st.total = total
		st.totalPages = (total + per - 1) / per
		if mp := attrIntMap(a, "pages"); mp > 0 && st.totalPages > mp {
			st.totalPages = mp
		}
	}
	st.ran = true
	if err != nil || len(posts) == 0 {
		return ""
	}
	prev := rc.postID
	defer func() { rc.postID = prev }()
	rc.depth++
	defer func() { rc.depth-- }()
	var sb strings.Builder
	for _, p := range posts {
		rc.postID = p.ID
		inner := blocks.RenderInner(ctx, b)
		sb.WriteString(`<li class="wp-block-post post-` + strconv.FormatInt(p.ID, 10) + ` ` + html.EscapeString(p.Type) + ` type-` + html.EscapeString(p.Type) + ` status-publish">` + inner + `</li>`)
	}
	return `<ul ` + wrapperClass(b, "") + `>` + sb.String() + `</ul>`
}

func attrIntMap(m map[string]any, key string) int {
	if v, ok := m[key].(float64); ok {
		return int(v)
	}
	return 0
}
