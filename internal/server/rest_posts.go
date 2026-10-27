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
	return "post"
}

func restBaseFor(typ string) string {
	switch typ {
	case "page":
		return "pages"
	case "wp_block":
		return "blocks"
	}
	return "posts"
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
	if !protected || u.Can("edit_post", p) {
		rendered = blocks.Render(withRender(c.Request.Context(), rc), p.Content)
		if !blocks.HasBlocks(p.Content) {
			rendered = wp.Autop(p.Content)
		}
	}
	excerpt := p.Excerpt
	if excerpt == "" {
		excerpt = wp.TrimWords(stripBlocks(p.Content), 55, " [&hellip;]")
	}
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
		"meta":           map[string]any{"footnotes": ""},
		"class_list":     []string{"post-" + itoa64(p.ID), p.Type, "type-" + p.Type, "status-" + p.Status, "hentry"},
	}
	if p.Type == "post" {
		out["sticky"] = p.Sticky
		out["format"] = p.Format
		cats, _ := s.store.TermsForPosts(c, []int64{p.ID}, "category")
		tags, _ := s.store.TermsForPosts(c, []int64{p.ID}, "post_tag")
		out["categories"] = termIDs(cats[p.ID])
		out["tags"] = termIDs(tags[p.ID])
	}
	if p.Type == "page" {
		out["parent"] = p.ParentID
		out["menu_order"] = p.MenuOrder
	}
	if p.Type == "wp_block" {
		out["wp_pattern_sync_status"] = metaString(p.Meta, "wp_pattern_sync_status")
		out["wp_pattern_category"] = []int64{}
	}
	if ctxName == "edit" {
		out["guid"].(map[string]any)["raw"] = home + "/?p=" + itoa64(p.ID)
		out["title"].(map[string]any)["raw"] = title
		out["content"].(map[string]any)["raw"] = p.Content
		out["content"].(map[string]any)["block_version"] = boolInt(blocks.HasBlocks(p.Content))
		out["excerpt"].(map[string]any)["raw"] = p.Excerpt
		out["password"] = p.Password
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
	revs, _ := s.store.Revisions(c, p.ID)
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
	out["_links"] = restLinks(self, home+"/wp-json/wp/v2/"+base, extra)
	return out
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
		base := base
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
			http.MethodGet: func(c *gin.Context, r *restRequest) { api.listAutosaves(c, r, typ) },
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

// readable reports whether the current REST user may see a post (published posts are public).
func (api *restAPI) readable(c *gin.Context, p *store.Post) bool {
	if p.Status == "publish" && p.Password == "" {
		return true
	}
	u := restCurrent(c)
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

// embedPost adds the _embedded author, featured media and terms (?_embed).
func (api *restAPI) embedPost(c *gin.Context, p *store.Post, item map[string]any) map[string]any {
	emb := map[string]any{}
	if u, err := api.s.store.UserByID(c, p.AuthorID); err == nil {
		emb["author"] = []any{api.s.restUser(c, u, "embed")}
	}
	if p.FeaturedMediaID != 0 {
		if a, err := api.s.store.GetAttachment(c, p.FeaturedMediaID); err == nil {
			emb["wp:featuredmedia"] = []any{api.s.restMedia(c, a, "embed")}
		}
	}
	if p.Type == "post" {
		cats, _ := api.s.store.TermsForPosts(c, []int64{p.ID}, "category")
		tags, _ := api.s.store.TermsForPosts(c, []int64{p.ID}, "post_tag")
		var cl, tl []any
		for _, t := range cats[p.ID] {
			cl = append(cl, api.s.restTerm(c, t, "embed"))
		}
		for _, t := range tags[p.ID] {
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
	item["_embedded"] = emb
	return item
}

func (api *restAPI) listPosts(c *gin.Context, r *restRequest, typ string) {
	s := api.s
	u := restCurrent(c)
	page, per := pageArgs(c)
	q := store.PostQuery{Types: []string{typ}, Page: page, PerPage: per, Search: c.Query("search"),
		OrderBy: c.DefaultQuery("orderby", "date"), Order: c.DefaultQuery("order", "desc"), Loc: s.loc()}
	if q.OrderBy == "include" || q.OrderBy == "relevance" {
		q.OrderBy = "date"
	}
	statuses := strings.Split(c.DefaultQuery("status", "publish"), ",")
	for _, st := range statuses {
		if st != "publish" && (u == nil || !u.Can("edit_"+restBaseFor(typ), nil)) {
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
	if ids, ok := r.IDs(c, "author"); ok && len(ids) > 0 {
		q.AuthorID = ids[0]
	}
	if p, ok := c.GetQuery("parent"); ok {
		id, _ := strconv.ParseInt(p, 10, 64)
		q.ParentID = &id
	}
	var termFilter []int64
	if ids, ok := r.IDs(c, "categories"); ok {
		termFilter = append(termFilter, ids...)
	}
	if ids, ok := r.IDs(c, "tags"); ok {
		termFilter = append(termFilter, ids...)
	}
	if len(termFilter) > 0 {
		q.TermIDs = termFilter
	}
	if slug := c.Query("slug"); slug != "" {
		p, err := s.store.PostBySlug(c, typ, slug)
		if err != nil {
			paginate(c, 0, per, page)
			restJSON(c, http.StatusOK, []any{})
			return
		}
		q.IDs = []int64{p.ID}
	}
	if st, ok := c.GetQuery("sticky"); ok {
		var stickyIDs []int64
		posts, _, _ := s.store.QueryPosts(c, store.PostQuery{Types: []string{"post"}, Statuses: []string{"publish"}, PerPage: -1})
		for _, p := range posts {
			if p.Sticky == (st == "true" || st == "1") {
				stickyIDs = append(stickyIDs, p.ID)
			}
		}
		if len(stickyIDs) == 0 {
			stickyIDs = []int64{-1}
		}
		q.IDs = stickyIDs
	}
	posts, total, err := s.store.QueryPosts(c, q)
	if err != nil {
		restErr(c, http.StatusInternalServerError, "rest_query_error", err.Error())
		return
	}
	ctxName := restContext(c)
	out := []map[string]any{}
	_, embed := c.GetQuery("_embed")
	for _, p := range posts {
		if p.Status != "publish" && !u.Can("read_post", p) {
			continue
		}
		item := s.restPost(c, p, ctxName)
		if embed {
			item = api.embedPost(c, p, item)
		}
		out = append(out, item)
	}
	paginate(c, total, per, page)
	restJSON(c, http.StatusOK, out)
}

// savePost creates or updates a post (POST /posts, PUT /posts/{id}).
func (api *restAPI) savePost(c *gin.Context, r *restRequest, typ string, id int64) {
	s := api.s
	u := restCurrent(c)
	if u == nil {
		restErr(c, http.StatusUnauthorized, "rest_cannot_create", "Sorry, you are not allowed to create posts as this user.")
		return
	}
	plural := restBaseFor(typ)
	if typ == "wp_block" {
		plural = "posts"
	}
	var p *store.Post
	isNew := id == 0
	if isNew {
		if !u.Can("edit_"+plural, nil) {
			restErr(c, http.StatusForbidden, "rest_cannot_create", "Sorry, you are not allowed to create posts as this user.")
			return
		}
		p = &store.Post{Type: typ, AuthorID: u.ID, Status: "draft", CommentStatus: s.settings.String("default_comment_status"), PingStatus: s.settings.String("default_ping_status")}
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
		p.Format = v
	}
	if v, ok := r.Bool(c, "sticky"); ok && typ == "post" {
		p.Sticky = v
	}
	if v, ok := r.Int(c, "featured_media"); ok {
		p.FeaturedMediaID = v
	}
	if v, ok := r.Int(c, "parent"); ok && typ == "page" {
		p.ParentID = v
	}
	if v, ok := r.Int(c, "menu_order"); ok {
		p.MenuOrder = int(v)
	}
	if v, ok := r.Int(c, "author"); ok && v != p.AuthorID {
		if !u.Can("edit_others_"+plural, nil) {
			restErr(c, http.StatusForbidden, "rest_cannot_edit_others", "Sorry, you are not allowed to update posts as this user.")
			return
		}
		p.AuthorID = v
	}
	if v, ok := r.Str(c, "wp_pattern_sync_status"); ok && typ == "wp_block" {
		p.Meta["wp_pattern_sync_status"] = v
	}
	if v, ok := r.Str(c, "date"); ok && v != "" {
		if t, err := time.ParseInLocation("2006-01-02T15:04:05", v, s.loc()); err == nil {
			p.PublishedAt = &t
		}
	} else if v, ok := r.Str(c, "date_gmt"); ok && v != "" {
		if t, err := time.ParseInLocation("2006-01-02T15:04:05", v, time.UTC); err == nil {
			p.PublishedAt = &t
		}
	} else if ok {
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
	if typ == "wp_block" && p.Status == "draft" && isNew {
		p.Status = "publish"
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

// afterPostSave runs the save_post-time side effects (cache purge, hooks).
func (s *Server) afterPostSave(ctx context.Context, p *store.Post) {
	s.autoAddPage(ctx, p)
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
		if err := s.store.DeletePost(c, p.ID); err != nil {
			restErr(c, http.StatusInternalServerError, "rest_cannot_delete", "The post cannot be deleted.")
			return
		}
		restJSON(c, http.StatusOK, map[string]any{"deleted": true, "previous": prev})
		return
	}
	if p.Status == "trash" {
		restErr(c, http.StatusGone, "rest_already_trashed", "The post has already been deleted.")
		return
	}
	_ = s.store.TrashPost(c, p.ID)
	p, _ = s.store.GetPost(c, p.ID)
	s.afterPostSave(c, p)
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
		"excerpt": map[string]any{"rendered": wp.Autop(rv.Excerpt), "raw": rv.Excerpt},
		"meta":    map[string]any{"footnotes": ""}, "preview_link": preview,
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
	if err != nil || !u.Can("edit_post", p) {
		restErr(c, http.StatusForbidden, "rest_cannot_read", "Sorry, you are not allowed to view autosaves of this post.")
		return
	}
	revs, _ := s.revisions(c, p.ID, true, 0)
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
	if err != nil || !u.Can("edit_post", p) {
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
	if !u.Can("unfiltered_html", nil) {
		content = wp.KsesPost(content)
	}
	draftLike := p.Status == "draft" || p.Status == "auto-draft" || p.Status == "pending"
	if draftLike && p.AuthorID == u.ID {
		p.Title, p.Content, p.Excerpt = title, content, excerpt
		if p.Status == "auto-draft" {
			p.Status = "draft"
		}
		if err := s.store.UpdatePost(c, p, u.ID); err != nil {
			restErr(c, http.StatusInternalServerError, "db_update_error", "Could not update post in the database.")
			return
		}
		p, _ = s.store.GetPost(c, p.ID)
		rv := revision{ID: p.ID, PostID: p.ID, AuthorID: u.ID, Title: p.Title, Excerpt: p.Excerpt, Content: p.Content, IsAutosave: true, CreatedAt: p.ModifiedAt}
		out := s.restRevision(c, rv, p)
		out["id"] = p.ID
		restJSON(c, http.StatusOK, out)
		return
	}
	var rv revision
	err = s.store.Pool.QueryRow(c, `WITH del AS (DELETE FROM post_revisions WHERE post_id = $1 AND author_id = $2 AND is_autosave)
		INSERT INTO post_revisions(post_id, author_id, title, excerpt, content, is_autosave) VALUES ($1,$2,$3,$4,$5,true)
		RETURNING id, created_at`, p.ID, u.ID, title, excerpt, content).Scan(&rv.ID, &rv.CreatedAt)
	if err != nil {
		restErr(c, http.StatusInternalServerError, "db_insert_error", "Could not save the autosave.")
		return
	}
	rv.PostID, rv.AuthorID, rv.Title, rv.Excerpt, rv.Content, rv.IsAutosave = p.ID, u.ID, title, excerpt, content, true
	restJSON(c, http.StatusOK, s.restRevision(c, rv, p))
}

func (api *restAPI) listRevisions(c *gin.Context, r *restRequest, typ string) {
	s := api.s
	u := restCurrent(c)
	p, err := s.store.GetPost(c, r.ID())
	if err != nil || !u.Can("edit_post", p) {
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
		WHERE id = $1 AND post_id = $2 AND NOT is_autosave`, r.ID(), p.ID).Scan(&rv.ID, &rv.PostID, &rv.AuthorID, &rv.Title, &rv.Excerpt, &rv.Content, &rv.IsAutosave, &rv.CreatedAt)
	if err != nil {
		restErr(c, http.StatusNotFound, "rest_post_invalid_id", "Invalid revision ID.")
		return nil, revision{}, false
	}
	return p, rv, true
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
