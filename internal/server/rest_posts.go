package server

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"wpgo/internal/blocks"
	"wpgo/internal/links"
	"wpgo/internal/store"
	"wpgo/internal/wp"
)

// restTypeFor maps a rest_base to a post type.
func restTypeFor(base string) string {
	switch base {
	case "pages":
		return "page"
	case "blocks":
		return "wp_block"
	}
	if typ := acfgoRESTTypeFor(base); typ != "" {
		return typ
	}
	return "post"
}

func restBaseFor(typ string) string {
	switch typ {
	case "page":
		return "pages"
	case "wp_block":
		return "blocks"
	}
	if base := acfgoRESTBaseFor(typ); base != "" {
		return base
	}
	return "posts"
}

// restCapPlural is the plural of the edit_/delete_ capabilities of a REST post type: wp_block maps
// to the post ones and an ACF-registered type uses its own key ("book" -> edit_books).
func restCapPlural(typ string) string {
	switch typ {
	case "wp_block":
		return "posts"
	case "post", "page":
		return restBaseFor(typ)
	}
	if acfgoRESTBaseFor(typ) != "" {
		return typ + "s"
	}
	return restBaseFor(typ)
}

// restCreateCap is the post type's create_posts capability. register_post_type( 'wp_block' ) maps
// it to publish_posts ("You need to be able to publish posts, in order to create blocks"), so a
// Contributor may read and use patterns but not create them; other types create with edit_posts.
func restCreateCap(typ string) string {
	if typ == "wp_block" {
		return "publish_posts"
	}
	return "edit_" + restCapPlural(typ)
}

func gmt(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05") }

// restPost is WP_REST_Posts_Controller::prepare_item_for_response().
func (s *Server) restPost(c *gin.Context, p *store.Post, ctxName string) map[string]any {
	u := restCurrent(c)
	lb := s.links(c)
	loc := s.loc()
	base := restBaseFor(p.Type)
	home := lb.Home
	self := home + "/wp-json/wp/v2/" + base + "/" + itoa64(p.ID)

	rc := &renderCtx{s: s, lb: lb}
	rendered := ""
	protected := p.Password != ""
	canEdit := u.Can("edit_post", p)
	if !protected || canEdit {
		rendered = blocks.Render(withRender(c.Request.Context(), rc), p.Content)
		if !blocks.HasBlocks(p.Content) {
			rendered = wp.Autop(p.Content)
		}
	}
	excerpt := wp.KsesPost(p.Excerpt) // rendered: excerpts stored before excerpt_save_pre sanitizing
	if excerpt == "" {
		excerpt = wp.TrimWords(stripBlocks(p.Content), 55, " [&hellip;]")
	}
	if protected && !canEdit {
		excerpt = "" // the first words of a password-protected post are as private as the rest of it
	}
	restricted := false // Goocommerce Memberships hid (part of) the content from this caller
	if !canEdit {
		r0, e0 := rendered, excerpt
		rendered, excerpt = s.gooRESTRestrict(c.Request.Context(), u, p, rendered, excerpt) // Goocommerce Memberships
		restricted = rendered != r0 || excerpt != e0
	}
	// Password-protected posts must not leak the note text or custom fields to callers
	// who cannot edit the post (public view, or an edit-context collection of someone else's post).
	hideProtected := protected && !canEdit
	// Members-only posts keep their custom fields and footnotes from non-members like their body.
	hideFields := hideProtected || (!canEdit && s.gmemHidesPost(c.Request.Context(), u, p.ID))
	date := p.Date().In(loc)
	var dateGMT any = gmt(p.Date())
	if p.PublishedAt == nil && (p.Status == "draft" || p.Status == "auto-draft" || p.Status == "pending") {
		dateGMT = nil
	}
	link := lb.Permalink(p)
	title := p.Title
	if p.Status == "auto-draft" && title == "Auto Draft" {
		title = ""
	}
	out := map[string]any{
		"id":             p.ID,
		"date":           date.Format("2006-01-02T15:04:05"),
		"date_gmt":       dateGMT,
		"guid":           map[string]any{"rendered": home + "/?p=" + itoa64(p.ID)},
		"modified":       p.ModifiedAt.In(loc).Format("2006-01-02T15:04:05"),
		"modified_gmt":   gmt(p.ModifiedAt),
		"slug":           p.Slug,
		"status":         p.Status,
		"type":           p.Type,
		"link":           link,
		"title":          map[string]any{"rendered": wp.Texturize(wp.EscText(title))},
		"content":        map[string]any{"rendered": rendered, "protected": protected},
		"excerpt":        map[string]any{"rendered": wp.Autop(excerpt), "protected": protected},
		"author":         p.AuthorID,
		"featured_media": p.FeaturedMediaID,
		"comment_status": p.CommentStatus,
		"ping_status":    p.PingStatus,
		"template":       "",
		"meta":           map[string]any{"footnotes": footnotesForREST(p, hideFields)},
		"class_list":     []string{"post-" + itoa64(p.ID), p.Type, "type-" + p.Type, "status-" + p.Status, "hentry"},
	}
	if p.Type == "post" {
		out["sticky"] = p.Sticky
		out["format"] = store.CanonicalPostFormat(p.Format)
		ctx := c.Request.Context() // request memo when a list prefetched its posts (listPosts)
		out["categories"] = termIDs(s.postTerms(ctx, p.ID, "category"))
		out["tags"] = termIDs(s.postTerms(ctx, p.ID, "post_tag"))
	}
	if p.Type == "page" {
		out["parent"] = p.ParentID
		out["menu_order"] = p.MenuOrder
		if tpl, _ := p.Meta["_wp_page_template"].(string); tpl != "default" {
			out["template"] = tpl
		}
	}
	if p.Type == "wp_block" {
		// The sync status is the registered meta key the editor writes (register_post_meta( 'wp_block',
		// 'wp_pattern_sync_status' )), answered only as the top-level field, never inside meta
		// (WP_REST_Blocks_Controller::filter_response_by_context()).
		out["wp_pattern_sync_status"] = metaString(p.Meta, "wp_pattern_sync_status")
		out["wp_pattern_category"] = termIDs(s.postTerms(c.Request.Context(), p.ID, "wp_pattern_category"))
	}
	s.golylangRESTFields(c, pllPost, p.ID, p.Type, out) // Golylang: lang and translations
	if ctxName == "edit" {
		out["guid"].(map[string]any)["raw"] = home + "/?p=" + itoa64(p.ID)
		out["title"].(map[string]any)["raw"] = title
		// A collection in the edit context (any edit_posts caller) also lists items the caller
		// cannot edit: like WordPress they keep their raw fields, except the post password and
		// the raw text of a password-protected or members-only post. That holds for patterns
		// (wp_block) too: the inserter builds other users' public patterns from content.raw, but a
		// protected one stays readable only to those who can edit it.
		hideRaw := !canEdit && (protected || restricted)
		if !hideRaw {
			out["content"].(map[string]any)["raw"] = p.Content
			out["excerpt"].(map[string]any)["raw"] = p.Excerpt
		}
		out["content"].(map[string]any)["block_version"] = boolInt(blocks.HasBlocks(p.Content))
		if canEdit {
			out["password"] = p.Password
		}
		out["permalink_template"] = lb.PermalinkTemplate(p)
		gen := wp.SanitizeTitle(title)
		if gen == "" {
			gen = itoa64(p.ID)
		}
		out["generated_slug"] = gen
	} else if protected {
		delete(out, "password")
	}

	// Links decide which panels and buttons the editor shows.
	extra := map[string]any{
		"about":         []map[string]any{{"href": home + "/wp-json/wp/v2/types/" + p.Type}},
		"author":        []map[string]any{{"embeddable": true, "href": home + "/wp-json/wp/v2/users/" + itoa64(p.AuthorID)}},
		"replies":       []map[string]any{{"embeddable": true, "href": home + "/wp-json/wp/v2/comments?post=" + itoa64(p.ID)}},
		"wp:attachment": []map[string]any{{"href": home + "/wp-json/wp/v2/media?parent=" + itoa64(p.ID)}},
	}
	revs, ok := restRevisionCount(c.Request.Context(), p.ID)
	if !ok {
		revs, _ = s.store.Revisions(c, p.ID)
	}
	extra["version-history"] = []map[string]any{{"count": revs, "href": self + "/revisions"}}
	if p.FeaturedMediaID != 0 {
		extra["wp:featuredmedia"] = []map[string]any{{"embeddable": true, "href": home + "/wp-json/wp/v2/media/" + itoa64(p.FeaturedMediaID)}}
	}
	if p.Type == "post" {
		extra["wp:term"] = []map[string]any{
			{"taxonomy": "category", "embeddable": true, "href": home + "/wp-json/wp/v2/categories?post=" + itoa64(p.ID)},
			{"taxonomy": "post_tag", "embeddable": true, "href": home + "/wp-json/wp/v2/tags?post=" + itoa64(p.ID)},
		}
	}
	if p.Type == "wp_block" {
		extra["wp:term"] = []map[string]any{
			{"taxonomy": "wp_pattern_category", "embeddable": true, "href": home + "/wp-json/wp/v2/wp_pattern_category?post=" + itoa64(p.ID)},
		}
	}
	if ctxName == "edit" && u != nil {
		plural := "posts"
		if p.Type == "page" {
			plural = "pages"
		}
		if u.Can("publish_"+plural, nil) {
			extra["wp:action-publish"] = []map[string]any{{"href": self}}
		}
		if u.Can("unfiltered_html", nil) {
			extra["wp:action-unfiltered-html"] = []map[string]any{{"href": self}}
		}
		if p.Type == "post" && u.Can("edit_others_posts", nil) && u.Can("publish_posts", nil) {
			extra["wp:action-sticky"] = []map[string]any{{"href": self}}
		}
		if u.Can("edit_others_"+plural, nil) {
			extra["wp:action-assign-author"] = []map[string]any{{"href": self}}
		}
		if p.Type == "wp_block" && u.Can("edit_posts", nil) {
			// A flat taxonomy: creating a term needs assign_terms, like assigning one (get_available_actions()).
			extra["wp:action-create-wp_pattern_category"] = []map[string]any{{"href": self}}
			extra["wp:action-assign-wp_pattern_category"] = []map[string]any{{"href": self}}
		}
		if p.Type == "post" {
			if u.Can("manage_categories", nil) {
				extra["wp:action-create-categories"] = []map[string]any{{"href": self}}
			}
			if u.Can("edit_posts", nil) {
				extra["wp:action-assign-categories"] = []map[string]any{{"href": self}}
				extra["wp:action-create-tags"] = []map[string]any{{"href": self}}
				extra["wp:action-assign-tags"] = []map[string]any{{"href": self}}
			}
		}
	}
	s.acfgoRESTPost(c, p, out) // ACF-Go: the "acf" field
	if hideFields {
		delete(out, "acf")
	}
	out["_links"] = restLinks(self, home+"/wp-json/wp/v2/"+base, extra)
	return out
}

// footnotesForREST is the registered footnotes meta. Password-protected posts
// expose it only to callers who can edit the post.
func footnotesForREST(p *store.Post, hide bool) string {
	if hide {
		return ""
	}
	return metaString(p.Meta, "footnotes")
}

func metaString(m map[string]any, k string) string {
	if v, ok := m[k].(string); ok {
		return v
	}
	return ""
}

func termIDs(ts []*store.Term) []int64 {
	out := []int64{}
	for _, t := range ts {
		out = append(out, t.ID)
	}
	return out
}

func (api *restAPI) registerPosts() {
	for _, base := range []string{"posts", "pages", "blocks"} {
		typ := restTypeFor(base)
		api.register("wp/v2", "/wp/v2/"+base, map[string]restHandler{
			http.MethodGet:  func(c *gin.Context, r *restRequest) { api.listPosts(c, r, typ) },
			http.MethodPost: func(c *gin.Context, r *restRequest) { api.savePost(c, r, typ, 0) },
		}, nil)
		api.register("wp/v2", "/wp/v2/"+base+`/(?P<id>[\d]+)`, map[string]restHandler{
			http.MethodGet:    func(c *gin.Context, r *restRequest) { api.getPost(c, r, typ) },
			http.MethodPost:   func(c *gin.Context, r *restRequest) { api.savePost(c, r, typ, r.ID()) },
			http.MethodPut:    func(c *gin.Context, r *restRequest) { api.savePost(c, r, typ, r.ID()) },
			http.MethodDelete: func(c *gin.Context, r *restRequest) { api.deletePost(c, r, typ) },
		}, nil)
		api.register("wp/v2", "/wp/v2/"+base+`/(?P<id>[\d]+)/autosaves`, map[string]restHandler{
			http.MethodGet:  func(c *gin.Context, r *restRequest) { api.listAutosaves(c, r, typ) },
			http.MethodPost: func(c *gin.Context, r *restRequest) { api.createAutosave(c, r, typ) },
		}, nil)
		api.register("wp/v2", "/wp/v2/"+base+`/(?P<parent>[\d]+)/autosaves/(?P<id>[\d]+)`, map[string]restHandler{
			http.MethodGet: func(c *gin.Context, r *restRequest) { api.getAutosave(c, r, typ) },
		}, nil)
		api.register("wp/v2", "/wp/v2/"+base+`/(?P<id>[\d]+)/revisions`, map[string]restHandler{
			http.MethodGet: func(c *gin.Context, r *restRequest) { api.listRevisions(c, r, typ) },
		}, nil)
		api.register("wp/v2", "/wp/v2/"+base+`/(?P<parent>[\d]+)/revisions/(?P<id>[\d]+)`, map[string]restHandler{
			http.MethodGet:    func(c *gin.Context, r *restRequest) { api.getRevision(c, r, typ) },
			http.MethodDelete: func(c *gin.Context, r *restRequest) { api.deleteRevision(c, r, typ) },
		}, nil)
	}
}

// restCanReadBlocks: wp_block maps read_post to edit_posts (WP_REST_Blocks_Controller::check_read_permission()),
// so synced patterns are not public even when published.
func restCanReadBlocks(u *store.User) bool {
	return u != nil && u.Can("edit_posts", nil)
}

// readable reports whether the current REST user may see a post (published posts are public).
func (api *restAPI) readable(c *gin.Context, p *store.Post) bool {
	u := restCurrent(c)
	if p.Type == "wp_block" && !restCanReadBlocks(u) {
		return false
	}
	if p.Status == "publish" && p.Password == "" {
		return true
	}
	if p.Status == "publish" {
		return true
	}
	return u != nil && u.Can("read_post", p)
}

func (api *restAPI) getPost(c *gin.Context, r *restRequest, typ string) {
	p, err := api.s.store.GetPost(c, r.ID())
	if err != nil || p.Type != typ {
		restErr(c, http.StatusNotFound, "rest_post_invalid_id", "Invalid post ID.")
		return
	}
	if !api.readable(c, p) {
		restErr(c, http.StatusUnauthorized, "rest_forbidden", "Sorry, you are not allowed to do that.")
		return
	}
	ctxName := restContext(c)
	if ctxName == "edit" && !restCurrent(c).Can("edit_post", p) {
		restErr(c, http.StatusForbidden, "rest_forbidden_context", "Sorry, you are not allowed to edit this post.")
		return
	}
	item := api.s.restPost(c, p, ctxName)
	if _, ok := c.GetQuery("_embed"); ok {
		item = api.embedPost(c, p, item)
	}
	restJSON(c, http.StatusOK, item)
}

// restRevisionCountsKey carries a list page's revision counts (store.RevisionCounts) to restPost.
type restRevisionCountsKey struct{}

// restRevisionCount is the prefetched revision count of a post; ok is false without a prefetch.
func restRevisionCount(ctx context.Context, postID int64) (int, bool) {
	m, ok := ctx.Value(restRevisionCountsKey{}).(map[int64]int)
	if !ok {
		return 0, false
	}
	return m[postID], true
}

// embedPost adds the _embedded author, featured media and terms (?_embed).
// The embed views read only display fields, so they go through the request memo (site_memo.go),
// which listPosts fills for the whole page in a few queries.
func (api *restAPI) embedPost(c *gin.Context, p *store.Post, item map[string]any) map[string]any {
	ctx := c.Request.Context()
	emb := map[string]any{}
	if u := api.s.displayUser(ctx, p.AuthorID); u != nil {
		emb["author"] = []any{api.s.restUser(c, u, "embed")}
	}
	if p.FeaturedMediaID != 0 {
		// The same rule as /wp/v2/media/<id>: an attachment of an unpublished post stays hidden
		// from those who do not browse the library, whichever post names it as its image.
		if a, err := api.s.attachment(ctx, p.FeaturedMediaID); err == nil &&
			(a.PostID == p.ID && p.Status == "publish" && p.Password == "" && !api.s.gmemHidesPost(ctx, restCurrent(c), p.ID) || api.mediaReadable(c, a)) {
			emb["wp:featuredmedia"] = []any{api.s.restMedia(c, a, "embed")}
		}
	}
	if p.Type == "post" {
		var cl, tl []any
		for _, t := range api.s.postTerms(ctx, p.ID, "category") {
			cl = append(cl, api.s.restTerm(c, t, "embed"))
		}
		for _, t := range api.s.postTerms(ctx, p.ID, "post_tag") {
			tl = append(tl, api.s.restTerm(c, t, "embed"))
		}
		if cl == nil {
			cl = []any{}
		}
		if tl == nil {
			tl = []any{}
		}
		emb["wp:term"] = []any{cl, tl}
	}
	if p.Type == "wp_block" {
		pc := []any{}
		for _, t := range api.s.postTerms(ctx, p.ID, "wp_pattern_category") {
			pc = append(pc, api.s.restTerm(c, t, "embed"))
		}
		emb["wp:term"] = []any{pc}
	}
	item["_embedded"] = emb
	return item
}

func (api *restAPI) listPosts(c *gin.Context, r *restRequest, typ string) {
	s := api.s
	u := restCurrent(c)
	page, per, ok := listArgs(c, restPostOrderBy)
	if !ok {
		return
	}
	q := store.PostQuery{Types: []string{typ}, Page: page, PerPage: per, Search: c.Query("search"),
		OrderBy: c.DefaultQuery("orderby", "date"), Order: c.DefaultQuery("order", "desc"), Loc: s.loc()}
	if q.OrderBy == "relevance" && q.Search == "" || q.OrderBy == "include" {
		q.OrderBy = "date"
	}
	plural := restCapPlural(typ)
	ctxName := restContext(c)
	// WP_REST_Posts_Controller::get_items_permissions_check(): the edit context carries
	// passwords and raw content, so it needs edit_{type}s.
	if ctxName == "edit" && (u == nil || !u.Can("edit_"+plural, nil)) {
		forbidden(c, "rest_forbidden_context", "Sorry, you are not allowed to edit posts in this post type.")
		return
	}
	if typ == "wp_block" && !restCanReadBlocks(u) {
		paginate(c, 0, per, page) // every item fails check_read_permission()
		restJSON(c, http.StatusOK, []any{})
		return
	}
	q.ExcludePassworded = q.Search != "" && u == nil // WP_Query::parse_search() for logged-out requests
	statuses := strings.Split(c.DefaultQuery("status", "publish"), ",")
	for _, st := range statuses {
		if st != "publish" && (u == nil || !u.Can("edit_"+plural, nil)) {
			restErr(c, http.StatusBadRequest, "rest_invalid_param", "Invalid parameter(s): status", map[string]any{"params": map[string]string{"status": "Status is forbidden."}})
			return
		}
	}
	if len(statuses) == 1 && statuses[0] == "any" {
		statuses = []string{"publish", "future", "draft", "pending", "private"}
	}
	q.Statuses = statuses
	if ids, ok := r.IDs(c, "include"); ok {
		q.IDs = ids
	}
	if ids, ok := r.IDs(c, "exclude"); ok {
		q.ExcludeID = ids
	}
	q.ExcludeID = append(q.ExcludeID, s.gooListingExclude(withPluginUser(c.Request.Context(), u), "post")...) // Goocommerce Memberships
	if !restCollectionFilters(c, r, s.loc(), &q) {
		return
	}
	if typ == "wp_block" { // the wp_pattern_category taxonomy's collection parameters
		if ids, ok := r.IDs(c, "wp_pattern_category"); ok && len(ids) > 0 {
			q.TermClauses = append(q.TermClauses, store.TermClause{IDs: ids})
		}
		if ids, ok := r.IDs(c, "wp_pattern_category_exclude"); ok && len(ids) > 0 {
			q.TermClauses = append(q.TermClauses, store.TermClause{IDs: ids, Exclude: true})
		}
	}
	// Statuses beyond publish: callers who cannot edit others' posts see only what they may read
	// (published/private posts and their own), filtered in SQL so X-WP-Total matches the items.
	if u != nil && !u.Can("edit_others_"+plural, nil) {
		for _, st := range q.Statuses {
			if st != "publish" {
				q.ReadableBy, q.ReadablePrivate = u.ID, u.Can("read_private_"+plural, nil)
				break
			}
		}
	}
	if st, ok := c.GetQuery("sticky"); ok && typ == "post" {
		v := st == "true" || st == "1"
		q.Sticky = &v
	}
	posts, total, err := s.store.QueryPosts(c, q)
	if err != nil {
		restErr(c, http.StatusInternalServerError, "rest_query_error", err.Error())
		return
	}
	// Terms, authors, featured images and revision counts for the whole page in one query each,
	// instead of several per post in restPost/embedPost.
	ctx := withViewMemo(c.Request.Context())
	s.prefetchPosts(ctx, posts)
	ids := make([]int64, 0, len(posts))
	for _, p := range posts {
		ids = append(ids, p.ID)
	}
	if revs, err := s.store.RevisionCounts(ctx, ids); err == nil {
		ctx = context.WithValue(ctx, restRevisionCountsKey{}, revs)
	}
	c.Request = c.Request.WithContext(ctx)
	out := []map[string]any{}
	_, embed := c.GetQuery("_embed")
	for _, p := range posts {
		if p.Status != "publish" && !u.Can("read_post", p) {
			continue
		}
		item := s.restPost(c, p, ctxName) // restPost hides passwords and protected raw text from non-editors
		if embed {
			item = api.embedPost(c, p, item)
		}
		out = append(out, item)
	}
	paginate(c, total, per, page)
	restJSON(c, http.StatusOK, out)
}

// restCollectionFilters reads the WP_REST_Posts_Controller collection parameters that map to SQL
// (author, author_exclude, parent, parent_exclude, slug, categories/tags with *_exclude and
// tax_relation, after/before, modified_after/modified_before, offset) into q. It answers
// 400 rest_invalid_param and returns false for a malformed value.
func restCollectionFilters(c *gin.Context, r *restRequest, loc *time.Location, q *store.PostQuery) bool {
	invalid := func(param, msg string) bool {
		restErr(c, http.StatusBadRequest, "rest_invalid_param", "Invalid parameter(s): "+param,
			map[string]any{"params": map[string]string{param: msg}})
		return false
	}
	if ids, ok := r.IDs(c, "author"); ok && len(ids) > 0 {
		q.AuthorIDs = ids
	}
	if ids, ok := r.IDs(c, "author_exclude"); ok && len(ids) > 0 {
		q.ExcludeAuthorIDs = ids
	}
	if ids, ok := r.IDs(c, "parent"); ok && len(ids) > 0 {
		q.ParentIDs = ids
	}
	if ids, ok := r.IDs(c, "parent_exclude"); ok && len(ids) > 0 {
		q.ExcludeParentIDs = ids
	}
	slugs := c.QueryArray("slug[]")
	if len(slugs) == 0 {
		if v := c.Query("slug"); v != "" {
			slugs = strings.Split(v, ",")
		}
	}
	for _, sl := range slugs {
		if sl = strings.TrimSpace(sl); sl != "" {
			q.Slugs = append(q.Slugs, wp.SanitizeTitle(sl))
		}
	}
	for _, tax := range []string{"categories", "tags"} {
		if ids, ok := r.IDs(c, tax); ok && len(ids) > 0 {
			q.TermClauses = append(q.TermClauses, store.TermClause{IDs: ids})
		}
		if ids, ok := r.IDs(c, tax+"_exclude"); ok && len(ids) > 0 {
			q.TermClauses = append(q.TermClauses, store.TermClause{IDs: ids, Exclude: true})
		}
	}
	switch rel := c.Query("tax_relation"); rel {
	case "", "AND":
	case "OR":
		q.TermRelationOR = true
	default:
		return invalid("tax_relation", "tax_relation is not one of AND and OR.")
	}
	for _, d := range []struct {
		param string
		dst   **time.Time
	}{{"after", &q.After}, {"before", &q.Before}, {"modified_after", &q.ModifiedAfter}, {"modified_before", &q.ModifiedBefore}} {
		v, ok := c.GetQuery(d.param)
		if !ok || v == "" {
			continue
		}
		t, ok := restParseDate(v, loc)
		if !ok {
			return invalid(d.param, "Invalid date.")
		}
		*d.dst = &t
	}
	if v, ok := c.GetQuery("offset"); ok && v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return invalid("offset", "offset is not of type integer.")
		}
		q.Offset = &n
	}
	return true
}

// savePost creates or updates a post (POST /posts, PUT /posts/{id}).
func (api *restAPI) savePost(c *gin.Context, r *restRequest, typ string, id int64) {
	s := api.s
	u := restCurrent(c)
	if u == nil {
		restErr(c, http.StatusUnauthorized, "rest_cannot_create", "Sorry, you are not allowed to create posts as this user.")
		return
	}
	plural := restCapPlural(typ)
	var p *store.Post
	isNew := id == 0
	if isNew {
		if !u.Can("edit_"+plural, nil) || !u.Can(restCreateCap(typ), nil) {
			restErr(c, http.StatusForbidden, "rest_cannot_create", "Sorry, you are not allowed to create posts as this user.")
			return
		}
		p = &store.Post{Type: typ, AuthorID: u.ID, Status: "draft", Meta: map[string]any{}, CommentStatus: s.settings.String("default_comment_status"), PingStatus: s.settings.String("default_ping_status")}
	} else {
		var err error
		p, err = s.store.GetPost(c, id)
		if err != nil || p.Type != typ {
			restErr(c, http.StatusNotFound, "rest_post_invalid_id", "Invalid post ID.")
			return
		}
		if !u.Can("edit_post", p) {
			restErr(c, http.StatusForbidden, "rest_cannot_edit", "Sorry, you are not allowed to edit this post.")
			return
		}
	}
	wasPublished := p.Status == "publish" || p.Status == "private" || p.Status == "future"

	if v, ok := r.Str(c, "title"); ok {
		p.Title = v
	}
	if v, ok := r.Str(c, "content"); ok {
		if !u.Can("unfiltered_html", nil) {
			v = wp.KsesPost(v)
		}
		p.Content = v
	}
	if v, ok := r.Str(c, "excerpt"); ok {
		if !u.Can("unfiltered_html", nil) {
			v = wp.KsesPost(v) // excerpt_save_pre
		}
		p.Excerpt = v
	}
	if v, ok := r.Str(c, "slug"); ok {
		p.Slug = v
	}
	if v, ok := r.Str(c, "password"); ok {
		p.Password = v
	}
	if v, ok := r.Str(c, "comment_status"); ok && (v == "open" || v == "closed") {
		p.CommentStatus = v
	}
	if v, ok := r.Str(c, "ping_status"); ok && (v == "open" || v == "closed") {
		p.PingStatus = v
	}
	if v, ok := r.Str(c, "format"); ok && v != "" {
		if store.CanonicalPostFormat(v) != v {
			restErr(c, http.StatusBadRequest, "rest_invalid_param", "Invalid parameter(s): format")
			return
		}
		p.Format = v
	}
	if v, ok := r.Bool(c, "sticky"); ok && typ == "post" {
		// WP_REST_Posts_Controller: making a post sticky needs edit_others_posts or publish_posts.
		if v && !u.Can("edit_others_posts", nil) && !u.Can("publish_posts", nil) {
			restErr(c, http.StatusForbidden, "rest_cannot_assign_sticky", "Sorry, you are not allowed to make posts sticky.")
			return
		}
		p.Sticky = v
	}
	// A missing user, attachment or parent is the client's mistake: WordPress answers 400 with its
	// own code, while the foreign key would otherwise fail the save with a 500.
	if v, ok := r.Int(c, "featured_media"); ok {
		if v != 0 {
			if _, err := s.store.GetAttachment(c, v); err != nil {
				restErr(c, http.StatusBadRequest, "rest_invalid_featured_media", "Invalid featured media ID.")
				return
			}
		}
		p.FeaturedMediaID = v
	}
	if v, ok := r.Int(c, "parent"); ok && typ == "page" {
		if v != 0 {
			if _, err := s.store.GetPost(c, v); err != nil {
				restErr(c, http.StatusBadRequest, "rest_post_invalid_id", "Invalid post parent ID.")
				return
			}
		}
		p.ParentID = v
	}
	if v, ok := r.Int(c, "menu_order"); ok {
		p.MenuOrder = int(v)
	}
	if v, ok := r.Str(c, "template"); ok && typ == "page" {
		// WP_REST_Posts_Controller::check_template(): "" is the default template; otherwise one of the
		// theme's page templates, or the page's current template even when the theme does not list it.
		cur, _ := p.Meta["_wp_page_template"].(string)
		if v != "" && v != "default" && v != cur && !s.isPageTemplate(v) {
			restErr(c, http.StatusBadRequest, "rest_invalid_param", "Invalid parameter(s): template")
			return
		}
		setPageTemplate(p, v)
	}
	if v, ok := r.Int(c, "author"); ok && v != p.AuthorID {
		if !u.Can("edit_others_"+plural, nil) {
			restErr(c, http.StatusForbidden, "rest_cannot_edit_others", "Sorry, you are not allowed to update posts as this user.")
			return
		}
		if _, err := s.store.UserByID(c, v); err != nil {
			restErr(c, http.StatusBadRequest, "rest_invalid_author", "Invalid author ID.")
			return
		}
		p.AuthorID = v
	}
	if p.Meta == nil {
		p.Meta = map[string]any{}
	}
	if typ == "wp_block" && !restPatternFields(c, r, u, p) {
		return
	}
	// Registered REST meta: only "footnotes" (a string), where the block editor keeps the
	// core/footnotes texts. Other keys are ignored, as WordPress ignores unregistered meta.
	if meta, ok := r.Body["meta"].(map[string]any); ok {
		if v, ok := meta["footnotes"].(string); ok {
			p.Meta["footnotes"] = sanitizePostMeta(u, "footnotes", v)
		}
	}
	dv, dok := r.Str(c, "date")
	gv, gok := r.Str(c, "date_gmt")
	if dok && dv != "" {
		t, ok := restParseDate(dv, s.loc())
		if !ok {
			restErr(c, http.StatusBadRequest, "rest_invalid_param", "Invalid parameter(s): date")
			return
		}
		p.PublishedAt = &t
	} else if gok && gv != "" {
		t, ok := restParseDate(gv, time.UTC)
		if !ok {
			restErr(c, http.StatusBadRequest, "rest_invalid_param", "Invalid parameter(s): date_gmt")
			return
		}
		p.PublishedAt = &t
	} else if dok || gok {
		// date sent as null: a floating date ("Immediately") for unpublished posts.
		if !wasPublished {
			p.PublishedAt = nil
		}
	}
	if v, ok := r.Str(c, "status"); ok && v != "" {
		switch v {
		case "publish", "future", "private":
			if !u.Can("publish_post", p) {
				v = "pending"
			}
		case "draft", "pending":
		default:
			restErr(c, http.StatusBadRequest, "rest_invalid_param", "Invalid parameter(s): status")
			return
		}
		p.Status = v
	}
	if p.Status == "auto-draft" {
		p.Status = "draft"
	}
	if typ == "wp_block" && p.Status == "draft" && isNew && u.Can("publish_posts", nil) {
		p.Status = "publish"
	}

	if !s.acfgoRESTValidate(c, r.Body, p) { // ACF-Go: the "acf" object
		return
	}
	var err error
	if isNew {
		err = s.store.InsertPost(c, p)
	} else {
		err = s.store.UpdatePost(c, p, u.ID)
	}
	if err != nil {
		restErr(c, http.StatusInternalServerError, "db_update_error", "Could not update post in the database.")
		return
	}
	s.acfgoRESTSave(c, r.Body, p) // ACF-Go
	if ids, ok := r.IDs(c, "wp_pattern_category"); ok && typ == "wp_block" {
		_ = s.store.SetPostTerms(c, p.ID, "wp_pattern_category", ids)
	}
	if typ == "post" {
		if ids, ok := r.IDs(c, "categories"); ok {
			if len(ids) == 0 {
				ids = []int64{int64(s.settings.Int("default_category"))}
			}
			_ = s.store.SetPostTerms(c, p.ID, "category", ids)
		} else if isNew {
			_ = s.store.SetPostTerms(c, p.ID, "category", []int64{int64(s.settings.Int("default_category"))})
		}
		if ids, ok := r.IDs(c, "tags"); ok {
			_ = s.store.SetPostTerms(c, p.ID, "post_tag", ids)
		}
	}
	// Drop the autosave once the post itself is saved.
	_, _ = s.store.Pool.Exec(c, `DELETE FROM post_revisions WHERE post_id = $1 AND is_autosave AND author_id = $2`, p.ID, u.ID)
	s.afterPostSave(c, p)
	p, _ = s.store.GetPost(c, p.ID)
	status := http.StatusOK
	if isNew {
		status = http.StatusCreated
	}
	restJSON(c, status, s.restPost(c, p, "edit"))
}

// restPatternFields reads the pattern-only fields of a wp_block save: the sync status, sent as the
// wp_pattern_sync_status field or as meta.wp_pattern_sync_status (what the editor and the JSON import
// send), and checks that the caller may assign the wp_pattern_category terms it sends
// (check_assign_terms_permission(): assign_terms is edit_posts). It answers the error and returns false
// for a bad request.
//
// meta.wp_pattern_sync_status follows WP_REST_Meta_Fields: null deletes the meta (the pattern becomes
// synced) and any value outside the enum, "" included, is "Invalid parameter(s): meta". The top-level
// field also takes "" for synced, a Pursae convenience (WordPress has no writable top-level field).
func restPatternFields(c *gin.Context, r *restRequest, u *store.User, p *store.Post) bool {
	if sync, has := r.Str(c, "wp_pattern_sync_status"); has {
		switch sync {
		case "": // fully synced, the default
			delete(p.Meta, "wp_pattern_sync_status")
		case "partial", "unsynced":
			p.Meta["wp_pattern_sync_status"] = sync
		default:
			restErr(c, http.StatusBadRequest, "rest_invalid_param", "Invalid parameter(s): wp_pattern_sync_status",
				map[string]any{"params": map[string]string{"wp_pattern_sync_status": "wp_pattern_sync_status is not one of partial and unsynced."}})
			return false
		}
	}
	if meta, ok := r.Body["meta"].(map[string]any); ok {
		if v, sent := meta["wp_pattern_sync_status"]; sent {
			switch sv, _ := v.(string); {
			case v == nil:
				delete(p.Meta, "wp_pattern_sync_status")
			case sv == "partial" || sv == "unsynced":
				p.Meta["wp_pattern_sync_status"] = sv
			default:
				restErr(c, http.StatusBadRequest, "rest_invalid_param", "Invalid parameter(s): meta",
					map[string]any{"params": map[string]string{"meta": "meta.wp_pattern_sync_status is not one of partial and unsynced."}})
				return false
			}
		}
	}
	if _, ok := r.IDs(c, "wp_pattern_category"); ok && !u.Can("edit_posts", nil) {
		restErr(c, http.StatusForbidden, "rest_cannot_assign_term", "Sorry, you are not allowed to assign the provided terms.")
		return false
	}
	return true
}

// afterPostSave runs the save_post-time side effects (cache purge, hooks).
func (s *Server) afterPostSave(ctx context.Context, p *store.Post) {
	s.autoAddPage(ctx, p)
	s.golylangSaved(ctx, p)  // Golylang: language of new posts, REST lang/translations, synchronization
	s.seogoAfterSave(ctx, p) // Pursae SEO: slug-change redirect, IndexNow
	// Pursae Web Builder: detach the doc when posts.content was edited outside the builder.
	s.wbAfterPostSave(ctx, p)
	s.pluginSavePost(ctx, p) // Wasm plugins: save_post_<type>, save_post (plugins_host.go)
	if s.onPostSaved != nil {
		s.onPostSaved(ctx, p)
	}
}

func (api *restAPI) deletePost(c *gin.Context, r *restRequest, typ string) {
	s := api.s
	u := restCurrent(c)
	p, err := s.store.GetPost(c, r.ID())
	if err != nil || p.Type != typ {
		restErr(c, http.StatusNotFound, "rest_post_invalid_id", "Invalid post ID.")
		return
	}
	if !u.Can("delete_post", p) {
		restErr(c, http.StatusForbidden, "rest_cannot_delete", "Sorry, you are not allowed to delete this post.")
		return
	}
	prev := s.restPost(c, p, "edit")
	if force, _ := r.Bool(c, "force"); force || typ == "wp_block" {
		// Purge while the post and its terms still exist (the admin delete paths do the same).
		s.afterPostSave(c, p)
		s.pluginPostAction(c, "before_delete_post", p)
		if err := s.store.DeletePost(c, p.ID); err != nil {
			restErr(c, http.StatusInternalServerError, "rest_cannot_delete", "The post cannot be deleted.")
			return
		}
		s.pluginPostMoved(c, "delete", p)
		s.purgePostAgain(c, p)
		s.wbAfterStatusChange(c, p)
		restJSON(c, http.StatusOK, map[string]any{"deleted": true, "previous": prev})
		return
	}
	if p.Status == "trash" {
		restErr(c, http.StatusGone, "rest_already_trashed", "The post has already been deleted.")
		return
	}
	if s.store.TrashPost(c, p.ID) == nil {
		s.pluginPostMoved(c, "trash", p) // Wasm plugins: transition_post_status, trashed_post
	}
	p, _ = s.store.GetPost(c, p.ID)
	s.afterPostSave(c, p)
	s.wbAfterStatusChange(c, p)
	restJSON(c, http.StatusOK, s.restPost(c, p, "edit"))
}

type revision struct {
	ID         int64
	PostID     int64
	AuthorID   int64
	Title      string
	Excerpt    string
	Content    string
	IsAutosave bool
	CreatedAt  time.Time
}

func (s *Server) revisions(c *gin.Context, postID int64, autosave bool, authorID int64) ([]revision, error) {
	q := `SELECT id, post_id, coalesce(author_id,0), title, excerpt, content, is_autosave, created_at FROM post_revisions
		WHERE post_id = $1 AND is_autosave = $2`
	args := []any{postID, autosave}
	if authorID != 0 {
		q += ` AND author_id = $3`
		args = append(args, authorID)
	}
	rows, err := s.store.Pool.Query(c, q+` ORDER BY created_at DESC, id DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []revision
	for rows.Next() {
		var rv revision
		if err := rows.Scan(&rv.ID, &rv.PostID, &rv.AuthorID, &rv.Title, &rv.Excerpt, &rv.Content, &rv.IsAutosave, &rv.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, rv)
	}
	return out, rows.Err()
}

func (s *Server) restRevision(c *gin.Context, rv revision, p *store.Post) map[string]any {
	home := s.links(c).Home
	base := restBaseFor(p.Type)
	loc := s.loc()
	sub := "revisions"
	if rv.IsAutosave {
		sub = "autosaves"
	}
	preview := s.links(c).Permalink(p)
	preview += previewSep(preview) + "preview_id=" + itoa64(p.ID) + "&preview_nonce=" + s.nonce(c, "post_preview_"+itoa64(p.ID))
	if rv.IsAutosave {
		preview += "&preview=true"
	}
	return map[string]any{
		"id": rv.ID, "author": rv.AuthorID, "parent": p.ID, "slug": itoa64(p.ID) + "-" + sub[:len(sub)-1] + "-v1",
		"date": rv.CreatedAt.In(loc).Format("2006-01-02T15:04:05"), "date_gmt": gmt(rv.CreatedAt),
		"modified": rv.CreatedAt.In(loc).Format("2006-01-02T15:04:05"), "modified_gmt": gmt(rv.CreatedAt),
		"guid":    map[string]any{"rendered": home + "/?p=" + itoa64(rv.ID), "raw": home + "/?p=" + itoa64(rv.ID)},
		"title":   map[string]any{"rendered": wp.EscText(rv.Title), "raw": rv.Title},
		"content": map[string]any{"rendered": blocks.Render(c.Request.Context(), rv.Content), "raw": rv.Content},
		"excerpt": map[string]any{"rendered": wp.Autop(wp.KsesPost(rv.Excerpt)), "raw": rv.Excerpt},
		"meta":    map[string]any{"footnotes": metaString(p.Meta, "footnotes")}, "preview_link": preview,
		"_links": map[string]any{"parent": []map[string]any{{"href": home + "/wp-json/wp/v2/" + base + "/" + itoa64(p.ID)}}},
	}
}

func (api *restAPI) listAutosaves(c *gin.Context, r *restRequest, typ string) {
	s := api.s
	u := restCurrent(c)
	pid := r.ID()
	if r.Params["parent"] != "" {
		pid, _ = strconv.ParseInt(r.Params["parent"], 10, 64)
	}
	p, err := s.store.GetPost(c, pid)
	if err != nil || p.Type != typ { // /wp/v2/posts/<id> of a product, a contact form, … is not a post
		restErr(c, http.StatusNotFound, "rest_post_invalid_parent", "Invalid post parent ID.")
		return
	}
	if u == nil || !u.Can("edit_post", p) {
		restErr(c, http.StatusForbidden, "rest_cannot_read", "Sorry, you are not allowed to view autosaves of this post.")
		return
	}
	// An author sees only the autosave they wrote. edit_others_* is required to list someone else's.
	authorID := u.ID
	if u.Can("edit_others_"+restCapPlural(p.Type), nil) {
		authorID = 0
	}
	revs, _ := s.revisions(c, p.ID, true, authorID)
	out := []map[string]any{}
	for _, rv := range revs {
		out = append(out, s.restRevision(c, rv, p))
	}
	restJSON(c, http.StatusOK, out)
}

// createAutosave is WP_REST_Autosaves_Controller::create_item(): for drafts edited by
// their author the post itself is updated; otherwise an autosave revision is stored.
func (api *restAPI) createAutosave(c *gin.Context, r *restRequest, typ string) {
	s := api.s
	u := restCurrent(c)
	p, err := s.store.GetPost(c, r.ID())
	if err != nil || p.Type != typ {
		restErr(c, http.StatusNotFound, "rest_post_invalid_id", "Invalid post ID.")
		return
	}
	if !u.Can("edit_post", p) {
		restErr(c, http.StatusForbidden, "rest_cannot_edit", "Sorry, you are not allowed to edit this post.")
		return
	}
	title, _ := r.Str(c, "title")
	content, _ := r.Str(c, "content")
	excerpt, _ := r.Str(c, "excerpt")
	if _, ok := r.get(c, "title"); !ok {
		title = p.Title
	}
	if _, ok := r.get(c, "content"); !ok {
		content = p.Content
	}
	if _, ok := r.get(c, "excerpt"); !ok {
		excerpt = p.Excerpt
	}
	rv, wrotePost, err := s.saveAutosave(c, p, u, title, content, excerpt)
	if err != nil {
		if wrotePost {
			restErr(c, http.StatusInternalServerError, "db_update_error", "Could not update post in the database.")
		} else {
			restErr(c, http.StatusInternalServerError, "db_insert_error", "Could not save the autosave.")
		}
		return
	}
	if wrotePost {
		out := s.restRevision(c, rv, p)
		out["id"] = p.ID
		restJSON(c, http.StatusOK, out)
		return
	}
	restJSON(c, http.StatusOK, s.restRevision(c, rv, p))
}

// saveAutosave stores an autosave the way wp_create_post_autosave() / post_preview() do: a draft or
// pending post edited by its author is updated in place (title, content and excerpt only; the status
// stays), any other post gets the user's single autosave revision and is left untouched, so nothing
// goes live. Content and excerpt are kses'd for users without unfiltered_html. wrotePost reports
// which of the two happened; on success with wrotePost the returned revision mirrors the post.
func (s *Server) saveAutosave(c *gin.Context, p *store.Post, u *store.User, title, content, excerpt string) (rv revision, wrotePost bool, err error) {
	if !u.Can("unfiltered_html", nil) {
		content = wp.KsesPost(content)
		excerpt = wp.KsesPost(excerpt)
	}
	draftLike := p.Status == "draft" || p.Status == "auto-draft" || p.Status == "pending"
	if draftLike && p.AuthorID == u.ID {
		p.Title, p.Content, p.Excerpt = title, content, excerpt
		if p.Status == "auto-draft" {
			p.Status = "draft"
		}
		if err := s.store.UpdatePost(c, p, u.ID); err != nil {
			return rv, true, err
		}
		// Pursae Web Builder drift: this autosave writes posts.content. Only the drift check: the
		// post stays a draft or pending, so no loop or template a visitor sees changes (purging
		// wb-loop on every autosave would empty most of the page cache once a minute).
		_, _ = s.wbDetectDrift(c, p.ID)
		if fresh, err := s.store.GetPost(c, p.ID); err == nil {
			*p = *fresh
		}
		return revision{ID: p.ID, PostID: p.ID, AuthorID: u.ID, Title: p.Title, Excerpt: p.Excerpt, Content: p.Content, IsAutosave: true, CreatedAt: p.ModifiedAt}, true, nil
	}
	err = s.store.Pool.QueryRow(c, `WITH del AS (DELETE FROM post_revisions WHERE post_id = $1 AND author_id = $2 AND is_autosave)
		INSERT INTO post_revisions(post_id, author_id, title, excerpt, content, is_autosave) VALUES ($1,$2,$3,$4,$5,true)
		RETURNING id, created_at`, p.ID, u.ID, title, excerpt, content).Scan(&rv.ID, &rv.CreatedAt)
	if err != nil {
		return rv, false, err
	}
	rv.PostID, rv.AuthorID, rv.Title, rv.Excerpt, rv.Content, rv.IsAutosave = p.ID, u.ID, title, excerpt, content, true
	return rv, false, nil
}

func (api *restAPI) listRevisions(c *gin.Context, r *restRequest, typ string) {
	s := api.s
	u := restCurrent(c)
	p, err := s.store.GetPost(c, r.ID())
	if err != nil || p.Type != typ {
		restErr(c, http.StatusNotFound, "rest_post_invalid_parent", "Invalid post parent ID.")
		return
	}
	if !u.Can("edit_post", p) {
		restErr(c, http.StatusForbidden, "rest_cannot_read", "Sorry, you are not allowed to view revisions of this post.")
		return
	}
	revs, _ := s.revisions(c, p.ID, false, 0)
	out := []map[string]any{}
	for _, rv := range revs {
		out = append(out, s.restRevision(c, rv, p))
	}
	paginate(c, len(out), 100, 1)
	restJSON(c, http.StatusOK, out)
}

// loadRevision is WP_REST_Revisions_Controller::get_revision() + get_parent().
func (api *restAPI) loadRevision(c *gin.Context, r *restRequest, typ string) (*store.Post, revision, bool) {
	return api.loadRevisionKind(c, r, typ, false)
}

// loadRevisionKind loads /{parent}/revisions/{id} (autosave false) or /{parent}/autosaves/{id}.
func (api *restAPI) loadRevisionKind(c *gin.Context, r *restRequest, typ string, autosave bool) (*store.Post, revision, bool) {
	s := api.s
	u := restCurrent(c)
	parentID, _ := strconv.ParseInt(r.Params["parent"], 10, 64)
	p, err := s.store.GetPost(c, parentID)
	if err != nil || p.Type != typ {
		restErr(c, http.StatusNotFound, "rest_post_invalid_parent", "Invalid post parent ID.")
		return nil, revision{}, false
	}
	if u == nil || !u.Can("edit_post", p) {
		forbidden(c, "rest_cannot_read", "Sorry, you are not allowed to view revisions of this post.")
		return nil, revision{}, false
	}
	var rv revision
	err = s.store.Pool.QueryRow(c, `SELECT id, post_id, coalesce(author_id,0), title, excerpt, content, is_autosave, created_at FROM post_revisions
		WHERE id = $1 AND post_id = $2 AND is_autosave = $3`, r.ID(), p.ID, autosave).Scan(&rv.ID, &rv.PostID, &rv.AuthorID, &rv.Title, &rv.Excerpt, &rv.Content, &rv.IsAutosave, &rv.CreatedAt)
	if err != nil {
		restErr(c, http.StatusNotFound, "rest_post_invalid_id", "Invalid revision ID.")
		return nil, revision{}, false
	}
	return p, rv, true
}

// getAutosave is WP_REST_Autosaves_Controller::get_item(): one autosave of the parent post.
func (api *restAPI) getAutosave(c *gin.Context, r *restRequest, typ string) {
	p, rv, ok := api.loadRevisionKind(c, r, typ, true)
	if !ok {
		return
	}
	u := restCurrent(c)
	if u == nil || (rv.AuthorID != u.ID && !u.Can("edit_others_"+restCapPlural(p.Type), nil)) {
		restErr(c, http.StatusForbidden, "rest_cannot_read", "Sorry, you are not allowed to view this autosave.")
		return
	}
	restJSON(c, http.StatusOK, api.s.restRevision(c, rv, p))
}

func (api *restAPI) getRevision(c *gin.Context, r *restRequest, typ string) {
	p, rv, ok := api.loadRevision(c, r, typ)
	if !ok {
		return
	}
	restJSON(c, http.StatusOK, api.s.restRevision(c, rv, p))
}

// deleteRevision is delete_item(): revisions cannot be trashed, force=true is required.
func (api *restAPI) deleteRevision(c *gin.Context, r *restRequest, typ string) {
	s := api.s
	p, rv, ok := api.loadRevision(c, r, typ)
	if !ok {
		return
	}
	if !restCurrent(c).Can("delete_post", p) {
		restErr(c, http.StatusForbidden, "rest_cannot_delete", "Sorry, you are not allowed to delete this revision.")
		return
	}
	if force, _ := r.Bool(c, "force"); !force {
		restErr(c, http.StatusNotImplemented, "rest_trash_not_supported", "Revisions do not support trashing. Set 'force=true' to delete.")
		return
	}
	prev := s.restRevision(c, rv, p)
	if _, err := s.store.Pool.Exec(c, `DELETE FROM post_revisions WHERE id = $1`, rv.ID); err != nil {
		restErr(c, http.StatusInternalServerError, "rest_cannot_delete", "The post cannot be deleted.")
		return
	}
	restJSON(c, http.StatusOK, map[string]any{"deleted": true, "previous": prev})
}

var _ = errors.New
var _ = links.EditLink
