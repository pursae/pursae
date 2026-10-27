package server

import (
	"context"
	"fmt"
	"html"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"wpgo/internal/blocks"
	"wpgo/internal/links"
	"wpgo/internal/store"
	"wpgo/internal/wp"
)

// renderCtx carries per-request state that dynamic block renderers need.
type renderCtx struct {
	s      *Server
	lb     *links.Builder
	search string
	ids    map[string]int
	depth  int // recursion guard for core/block

	// Request state the WordPress render callbacks read from globals.
	c           *gin.Context // nil outside a request
	user        *store.User  // is_user_logged_in()
	q           *siteQuery   // the main query (get_query_var, get_queried_object_id)
	postID      int64        // the post whose content is rendering (block context postId)
	feed        bool         // is_feed(): no lazy loading, feed URLs
	reqURI      string       // REQUEST_URI for the loginout block's redirect
	logoutNonce string       // wp_logout_url() nonce of the current user

	// Loading optimisation state (wp_increase_content_media_count, wp_maybe_add_fetchpriority_high_attr).
	mainLoop     bool // in_the_loop() && is_main_query()
	mediaCount   int
	priorityDone bool
}

type renderKey struct{}

func withRender(ctx context.Context, rc *renderCtx) context.Context {
	return context.WithValue(ctx, renderKey{}, rc)
}

func renderFrom(ctx context.Context) *renderCtx {
	rc, _ := ctx.Value(renderKey{}).(*renderCtx)
	return rc
}

// uniqueID is wp_unique_id().
func (rc *renderCtx) uniqueID(prefix string) string {
	if rc.ids == nil {
		rc.ids = map[string]int{}
	}
	rc.ids[prefix]++
	return prefix + strconv.Itoa(rc.ids[prefix])
}

func attrBool(b *blocks.Block, key string, def bool) bool {
	if v, ok := b.Attrs[key].(bool); ok {
		return v
	}
	return def
}

func attrInt(b *blocks.Block, key string, def int) int {
	if v, ok := b.Attrs[key].(float64); ok {
		return int(v)
	}
	return def
}

func attrStr(b *blocks.Block, key, def string) string {
	if v, ok := b.Attrs[key].(string); ok {
		return v
	}
	return def
}

// wrapperClass is get_block_wrapper_attributes(): wp-block-{name} plus className and align attributes.
func wrapperClass(b *blocks.Block, extra string) string {
	name := strings.TrimPrefix(b.Name(), "core/")
	classes := []string{}
	if extra != "" {
		classes = append(classes, extra)
	}
	classes = append(classes, "wp-block-"+name)
	if a := attrStr(b, "align", ""); a != "" {
		classes = append(classes, "align"+a)
	}
	if cn := attrStr(b, "className", ""); cn != "" {
		classes = append(classes, cn)
	}
	return `class="` + html.EscapeString(strings.Join(classes, " ")) + `"`
}

func registerCoreBlocks(s *Server) {
	blocks.RegisterDynamic("core/heading", func(_ context.Context, _ *blocks.Block, content string) string {
		return blocks.AddClass(content, "wp-block-heading", "h1", "h2", "h3", "h4", "h5", "h6")
	})

	// Layout support (wp_render_layout_support_flag) for the flow and flex layouts used by
	// classic themes; constrained layouts get their class names.
	layout := func(name string) blocks.RenderFunc {
		return func(_ context.Context, b *blocks.Block, content string) string {
			typ := "flow"
			if l, ok := b.Attrs["layout"].(map[string]any); ok {
				if t, ok := l["type"].(string); ok && t != "default" {
					typ = t
				} else if inherit, ok := l["inherit"].(bool); ok && inherit {
					typ = "constrained"
				}
			}
			if name == "columns" || name == "buttons" || name == "social-links" {
				typ = "flex"
			}
			return blocks.AddClass(content, "is-layout-"+typ+" wp-block-"+name+"-is-layout-"+typ)
		}
	}
	for _, n := range []string{"group", "columns", "column", "buttons", "social-links", "quote", "details"} {
		blocks.RegisterDynamic("core/"+n, layout(n))
	}

	blocks.RegisterDynamic("core/search", renderSearchBlock)
	blocks.RegisterDynamic("core/latest-posts", renderLatestPosts)
	blocks.RegisterDynamic("core/latest-comments", renderLatestComments)
	blocks.RegisterDynamic("core/archives", renderArchives)
	blocks.RegisterDynamic("core/categories", renderCategories)
	blocks.RegisterDynamic("core/block", renderSyncedPattern)
	blocks.RegisterDynamic("core/shortcode", func(_ context.Context, _ *blocks.Block, content string) string {
		return wp.Autop(content)
	})
	blocks.RegisterDynamic("core/more", func(_ context.Context, _ *blocks.Block, content string) string {
		return content
	})
	// <!--nextpage--> stays in the content; theContent splits on it (setup_postdata()).
	blocks.RegisterDynamic("core/nextpage", func(_ context.Context, _ *blocks.Block, content string) string {
		return content
	})
	registerSiteBlocks(s)
}

func renderSearchBlock(ctx context.Context, b *blocks.Block, _ string) string {
	rc := renderFrom(ctx)
	if rc == nil {
		return ""
	}
	label := attrStr(b, "label", "Search")
	if label == "" {
		label = "Search"
	}
	buttonText := attrStr(b, "buttonText", "Search")
	position := attrStr(b, "buttonPosition", "button-outside")
	useIcon := attrBool(b, "buttonUseIcon", false)
	showLabel := attrBool(b, "showLabel", true)
	placeholder := attrStr(b, "placeholder", "")
	id := rc.uniqueID("wp-block-search__input-")

	var classes []string
	switch position {
	case "button-inside":
		classes = append(classes, "wp-block-search__button-inside")
	case "button-outside":
		classes = append(classes, "wp-block-search__button-outside")
	case "no-button":
		classes = append(classes, "wp-block-search__no-button")
	case "button-only":
		classes = append(classes, "wp-block-search__button-only wp-block-search__searchfield-hidden")
	}
	if position != "no-button" {
		if useIcon {
			classes = append(classes, "wp-block-search__icon-button")
		} else {
			classes = append(classes, "wp-block-search__text-button")
		}
	}
	labelClass := "wp-block-search__label"
	if !showLabel {
		labelClass += " screen-reader-text"
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, `<form role="search" method="get" action="%s" %s><label class="%s" for="%s">%s</label><div class="wp-block-search__inside-wrapper">`,
		html.EscapeString(rc.lb.HomeURL("/")), wrapperClass(b, strings.Join(classes, " ")), labelClass, id, html.EscapeString(label))
	fmt.Fprintf(&sb, `<input class="wp-block-search__input" id="%s" placeholder="%s" value="%s" type="search" name="s" required />`,
		id, html.EscapeString(placeholder), html.EscapeString(rc.search))
	if position != "no-button" {
		inner := html.EscapeString(buttonText)
		btnClass := "wp-block-search__button"
		if useIcon {
			btnClass += " has-icon"
			inner = `<svg class="search-icon" viewBox="0 0 24 24" width="24" height="24"><path d="M13 5c-3.3 0-6 2.7-6 6 0 1.4.5 2.7 1.3 3.7l-3.8 3.8 1.1 1.1 3.8-3.8c1 .8 2.3 1.3 3.7 1.3 3.3 0 6-2.7 6-6S16.3 5 13 5zm0 10.5c-2.5 0-4.5-2-4.5-4.5s2-4.5 4.5-4.5 4.5 2 4.5 4.5-2 4.5-4.5 4.5z"></path></svg>`
		}
		fmt.Fprintf(&sb, `<button aria-label="%s" class="%s wp-element-button" type="submit">%s</button>`, html.EscapeString(buttonText), btnClass, inner)
	}
	sb.WriteString(`</div></form>`)
	return sb.String()
}

func renderLatestPosts(ctx context.Context, b *blocks.Block, _ string) string {
	rc := renderFrom(ctx)
	if rc == nil {
		return ""
	}
	q := store.PostQuery{Types: []string{"post"}, Statuses: []string{"publish"}, PerPage: attrInt(b, "postsToShow", 5),
		OrderBy: attrStr(b, "orderBy", "date"), Order: attrStr(b, "order", "desc")}
	if cats, ok := b.Attrs["categories"].([]any); ok {
		for _, c := range cats {
			if m, ok := c.(map[string]any); ok {
				if id, ok := m["id"].(float64); ok {
					q.TermIDs = append(q.TermIDs, int64(id))
				}
			}
		}
	}
	if a, ok := b.Attrs["selectedAuthor"].(float64); ok {
		q.AuthorID = int64(a)
	}
	posts, _, err := rc.s.store.QueryPosts(ctx, q)
	if err != nil {
		return ""
	}
	loc := rc.s.loc()
	var items strings.Builder
	for _, p := range posts {
		title := p.Title
		if strings.TrimSpace(title) == "" {
			title = "(no title)"
		}
		link := rc.lb.Permalink(p)
		items.WriteString("<li>")
		fmt.Fprintf(&items, `<a class="wp-block-latest-posts__post-title" href="%s">%s</a>`, html.EscapeString(link), wp.Texturize(wp.EscText(title)))
		if attrBool(b, "displayAuthor", false) {
			if u, err := rc.s.store.UserByID(ctx, p.AuthorID); err == nil {
				fmt.Fprintf(&items, `<div class="wp-block-latest-posts__post-author">by %s</div>`, html.EscapeString(u.DisplayName))
			}
		}
		if attrBool(b, "displayPostDate", false) {
			t := p.Date().In(loc)
			fmt.Fprintf(&items, `<time datetime="%s" class="wp-block-latest-posts__post-date">%s</time>`,
				t.Format(time.RFC3339), wp.FormatDate(t, rc.s.settings.String("date_format")))
		}
		if attrBool(b, "displayPostContent", false) {
			if attrStr(b, "displayPostContentRadio", "excerpt") == "excerpt" {
				ex := p.Excerpt
				if ex == "" {
					ex = wp.TrimWords(stripBlocks(p.Content), attrInt(b, "excerptLength", 55),
						` … <a class="wp-block-latest-posts__read-more" href="`+html.EscapeString(link)+`" rel="noopener">Read more<span class="screen-reader-text">: `+html.EscapeString(title)+`</span></a>`)
				}
				fmt.Fprintf(&items, `<div class="wp-block-latest-posts__post-excerpt">%s</div>`, ex)
			} else if rc.depth < 3 {
				rc.depth++
				fmt.Fprintf(&items, `<div class="wp-block-latest-posts__post-full-content">%s</div>`, blocks.Render(ctx, p.Content))
				rc.depth--
			}
		}
		items.WriteString("</li>\n")
	}
	classes := []string{"wp-block-latest-posts__list"}
	if l, ok := b.Attrs["postLayout"].(string); ok && l == "grid" {
		classes = append(classes, "is-grid", "columns-"+strconv.Itoa(attrInt(b, "columns", 3)))
	}
	if attrBool(b, "displayPostDate", false) {
		classes = append(classes, "has-dates")
	}
	if attrBool(b, "displayAuthor", false) {
		classes = append(classes, "has-author")
	}
	return fmt.Sprintf(`<ul %s>%s</ul>`, wrapperClass(b, strings.Join(classes, " ")), items.String())
}

func renderLatestComments(ctx context.Context, b *blocks.Block, _ string) string {
	rc := renderFrom(ctx)
	if rc == nil {
		return ""
	}
	display := "excerpt"
	if v, ok := b.Attrs["displayExcerpt"].(bool); ok {
		if !v {
			display = "none"
		}
	} else {
		display = attrStr(b, "displayContent", "excerpt")
	}
	avatar := attrBool(b, "displayAvatar", true)
	showDate := attrBool(b, "displayDate", true)
	comments, err := rc.s.store.RecentComments(ctx, attrInt(b, "commentsToShow", 5), false)
	if err != nil {
		return ""
	}
	loc := rc.s.loc()
	var items strings.Builder
	for _, cm := range comments {
		p, err := rc.s.store.GetPost(ctx, cm.PostID)
		if err != nil {
			continue
		}
		items.WriteString(`<li class="wp-block-latest-comments__comment">`)
		if avatar {
			fmt.Fprintf(&items, `<img alt='' src='%s' srcset='%s 2x' class='avatar avatar-48 photo wp-block-latest-comments__comment-avatar' height='48' width='48' />`,
				html.EscapeString(rc.s.avatarFor(ctx, cm.AuthorEmail, 48)), html.EscapeString(rc.s.avatarFor(ctx, cm.AuthorEmail, 96)))
		}
		items.WriteString(`<article><footer class="wp-block-latest-comments__comment-meta">`)
		name := cm.AuthorName
		if name == "" {
			name = "Anonymous"
		}
		authorURL := cm.AuthorURL
		if authorURL == "" && cm.UserID != 0 {
			if u, err := rc.s.store.UserByID(ctx, cm.UserID); err == nil {
				authorURL = rc.lb.AuthorLink(u.ID, u.Nicename)
			}
		}
		if authorURL != "" {
			fmt.Fprintf(&items, `<a class="wp-block-latest-comments__comment-author" href="%s">%s</a>`, html.EscapeString(authorURL), html.EscapeString(name))
		} else {
			fmt.Fprintf(&items, `<span class="wp-block-latest-comments__comment-author">%s</span>`, html.EscapeString(name))
		}
		title := p.Title
		if strings.TrimSpace(title) == "" {
			title = "(no title)"
		}
		fmt.Fprintf(&items, ` on <a class="wp-block-latest-comments__comment-link" href="%s">%s</a>`,
			html.EscapeString(rc.lb.Permalink(p)+"#comment-"+itoa64(cm.ID)), wp.Texturize(wp.EscText(title)))
		if showDate {
			t := cm.CreatedAt.In(loc)
			fmt.Fprintf(&items, `<time datetime="%s" class="wp-block-latest-comments__comment-date">%s</time>`, t.Format(time.RFC3339), wp.FormatDate(t, rc.s.settings.String("date_format")))
		}
		items.WriteString(`</footer>`)
		switch display {
		case "full":
			fmt.Fprintf(&items, `<div class="wp-block-latest-comments__comment-excerpt">%s</div>`, wp.Autop(commentText(cm.Content)))
		case "excerpt":
			fmt.Fprintf(&items, `<div class="wp-block-latest-comments__comment-excerpt">%s</div>`, wp.Autop(wp.TrimWords(cm.Content, 20, "&hellip;")))
		}
		items.WriteString(`</article></li>`)
	}
	var classes []string
	if avatar {
		classes = append(classes, "has-avatars")
	}
	if showDate {
		classes = append(classes, "has-dates")
	}
	if display != "none" {
		classes = append(classes, "has-excerpts")
	}
	if len(comments) == 0 {
		classes = append(classes, "no-comments")
		return fmt.Sprintf(`<div %s>No comments to show.</div>`, wrapperClass(b, strings.Join(classes, " ")))
	}
	return fmt.Sprintf(`<ol %s>%s</ol>`, wrapperClass(b, strings.Join(classes, " ")), items.String())
}

func renderArchives(ctx context.Context, b *blocks.Block, _ string) string {
	rc := renderFrom(ctx)
	if rc == nil {
		return ""
	}
	months, err := rc.s.store.MonthlyArchives(ctx, rc.s.loc())
	if err != nil {
		return ""
	}
	showCount := attrBool(b, "showPostCounts", false)
	if attrBool(b, "displayAsDropdown", false) {
		id := rc.uniqueID("wp-block-archives-")
		labelClass := "wp-block-archives__label"
		if !attrBool(b, "showLabel", true) {
			labelClass += " screen-reader-text"
		}
		var opts strings.Builder
		for _, m := range months {
			text := time.Month(m.Month).String() + " " + strconv.Itoa(m.Year)
			if showCount {
				text += fmt.Sprintf(" (%d)", m.Count)
			}
			fmt.Fprintf(&opts, "\t<option value='%s'> %s </option>\n", html.EscapeString(rc.lb.MonthLink(m.Year, m.Month)), text)
		}
		return fmt.Sprintf(`<div %s><label for="%s" class="%s">Archives</label>
		<select id="%s" name="archive-dropdown" onchange="document.location.href=this.options[this.selectedIndex].value;">
		<option value="">Select Month</option>%s</select></div>`, wrapperClass(b, "wp-block-archives-dropdown"), id, labelClass, id, opts.String())
	}
	if len(months) == 0 {
		return fmt.Sprintf(`<div %s>No archives to show.</div>`, wrapperClass(b, "wp-block-archives-list"))
	}
	var items strings.Builder
	for _, m := range months {
		text := time.Month(m.Month).String() + " " + strconv.Itoa(m.Year)
		fmt.Fprintf(&items, "\t<li><a href='%s'>%s</a>", html.EscapeString(rc.lb.MonthLink(m.Year, m.Month)), text)
		if showCount {
			fmt.Fprintf(&items, "&nbsp;(%d)", m.Count)
		}
		items.WriteString("</li>\n")
	}
	return fmt.Sprintf(`<ul %s>%s</ul>`, wrapperClass(b, "wp-block-archives-list"), items.String())
}

func renderCategories(ctx context.Context, b *blocks.Block, _ string) string {
	rc := renderFrom(ctx)
	if rc == nil {
		return ""
	}
	tax := attrStr(b, "taxonomy", "category")
	hideEmpty := !attrBool(b, "showEmpty", false)
	hier := attrBool(b, "showHierarchy", false)
	showCount := attrBool(b, "showPostCounts", false)
	terms, err := rc.s.store.HierarchicalTerms(ctx, tax, hideEmpty)
	if err != nil {
		return ""
	}
	if attrBool(b, "showOnlyTopLevel", false) {
		var top []*store.Term
		for _, t := range terms {
			if t.ParentID == 0 {
				top = append(top, t)
			}
		}
		terms = top
	}
	if !hier {
		// Flat list ordered by name, like wp_list_categories( hierarchical => false ).
		for i := 1; i < len(terms); i++ {
			for j := i; j > 0 && strings.ToLower(terms[j].Name) < strings.ToLower(terms[j-1].Name); j-- {
				terms[j], terms[j-1] = terms[j-1], terms[j]
			}
		}
	}
	if attrBool(b, "displayAsDropdown", false) {
		id := "wp-block-categories-" + strconv.Itoa(len(rc.ids)+1)
		var opts strings.Builder
		opts.WriteString(`<option value='-1'>Select Category</option>`)
		for _, t := range terms {
			text := t.Name
			if showCount {
				text += fmt.Sprintf("&nbsp;&nbsp;(%d)", t.Count)
			}
			fmt.Fprintf(&opts, `<option class="level-%d" value="%s">%s%s</option>`, t.Depth, html.EscapeString(t.Slug), strings.Repeat("&nbsp;&nbsp;&nbsp;", t.Depth*boolInt(hier)), text)
		}
		return fmt.Sprintf(`<div %s><label class="wp-block-categories__label" for="%s">Categories</label><select name='category_name' id='%s' class='postform'>%s</select></div>`,
			wrapperClass(b, "wp-block-categories-dropdown wp-block-categories-taxonomy-"+tax), id, id, opts.String())
	}
	var items strings.Builder
	if len(terms) == 0 {
		items.WriteString(`<li class="cat-item-none">No categories</li>`)
	}
	var write func(parent int64, list []*store.Term)
	write = func(parent int64, list []*store.Term) {
		for _, t := range list {
			if hier && t.ParentID != parent {
				continue
			}
			fmt.Fprintf(&items, `<li class="cat-item cat-item-%d"><a href="%s">%s</a>`, t.ID, html.EscapeString(rc.lb.TermLink(t)), html.EscapeString(t.Name))
			if showCount {
				fmt.Fprintf(&items, " (%d)", t.Count)
			}
			if hier {
				var kids []*store.Term
				for _, k := range terms {
					if k.ParentID == t.ID {
						kids = append(kids, k)
					}
				}
				if len(kids) > 0 {
					items.WriteString("\n<ul class='children'>\n")
					write(t.ID, kids)
					items.WriteString("</ul>\n")
				}
			}
			items.WriteString("\n</li>\n")
		}
	}
	if hier {
		var roots []*store.Term
		ids := map[int64]bool{}
		for _, t := range terms {
			ids[t.ID] = true
		}
		for _, t := range terms {
			if !ids[t.ParentID] {
				t.ParentID = 0
				roots = append(roots, t)
			}
		}
		write(0, roots)
	} else {
		write(0, terms)
	}
	return fmt.Sprintf(`<ul %s>%s</ul>`, wrapperClass(b, "wp-block-categories-list wp-block-categories-taxonomy-"+tax), items.String())
}

// renderSyncedPattern renders a reusable block (wp_block post) with a recursion guard.
func renderSyncedPattern(ctx context.Context, b *blocks.Block, _ string) string {
	rc := renderFrom(ctx)
	ref := attrInt(b, "ref", 0)
	if rc == nil || ref == 0 || rc.depth > 5 {
		return ""
	}
	p, err := rc.s.store.GetPost(ctx, int64(ref))
	if err != nil || p.Type != "wp_block" || p.Status != "publish" {
		return ""
	}
	rc.depth++
	defer func() { rc.depth-- }()
	return blocks.Render(ctx, p.Content)
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// stripBlocks removes block delimiters, leaving the HTML (excerpt generation).
func stripBlocks(content string) string {
	var sb strings.Builder
	for _, b := range blocks.Parse(content) {
		sb.WriteString(blocks.RenderBlock(context.Background(), b))
	}
	return sb.String()
}
