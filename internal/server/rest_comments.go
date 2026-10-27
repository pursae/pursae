package server

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"wpgo/internal/store"
	"wpgo/internal/wp"
)

// WP_REST_Comments_Controller: /wp/v2/comments.

func (api *restAPI) registerComments() {
	api.register("wp/v2", "/wp/v2/comments", map[string]restHandler{
		http.MethodGet:  api.listComments,
		http.MethodPost: api.createComment,
	}, nil)
	api.register("wp/v2", `/wp/v2/comments/(?P<id>[\d]+)`, map[string]restHandler{
		http.MethodGet:    api.getComment,
		http.MethodPost:   api.updateComment,
		http.MethodPut:    api.updateComment,
		http.MethodDelete: api.deleteComment,
	}, nil)
}

// restCommentStatus maps the stored status to the API's (approved, hold, spam, trash).
func restCommentStatus(st string) string {
	if st == "pending" {
		return "hold"
	}
	return st
}

// storeCommentStatus accepts the API's status spellings (approve, approved, 1, hold, 0, spam, trash).
func storeCommentStatus(v string) (string, bool) {
	switch v {
	case "approve", "approved", "1":
		return "approved", true
	case "hold", "0":
		return "pending", true
	case "spam", "trash":
		return v, true
	}
	return "", false
}

// restComment is prepare_item_for_response(): view fields, plus author_email/author_ip/author_user_agent and
// content.raw in the edit context.
func (s *Server) restComment(c *gin.Context, cm *store.Comment, ctxName string) map[string]any {
	home := s.links(c).Home
	loc := s.loc()
	link := ""
	var p *store.Post
	if pp, err := s.store.GetPost(c, cm.PostID); err == nil {
		p = pp
		link = s.links(c).Permalink(p) + "#comment-" + itoa64(cm.ID)
	}
	self := home + "/wp-json/wp/v2/comments/" + itoa64(cm.ID)
	extra := map[string]any{}
	if cm.UserID != 0 {
		extra["author"] = []map[string]any{{"embeddable": true, "href": home + "/wp-json/wp/v2/users/" + itoa64(cm.UserID)}}
	}
	if p != nil {
		extra["up"] = []map[string]any{{"embeddable": true, "post_type": p.Type, "href": home + "/wp-json/wp/v2/" + restBaseFor(p.Type) + "/" + itoa64(p.ID)}}
	}
	if cm.ParentID != 0 {
		extra["in-reply-to"] = []map[string]any{{"embeddable": true, "href": home + "/wp-json/wp/v2/comments/" + itoa64(cm.ParentID)}}
	}
	extra["children"] = []map[string]any{{"embeddable": true, "href": home + "/wp-json/wp/v2/comments?parent=" + itoa64(cm.ID)}}
	content := map[string]any{"rendered": commentText(cm.Content)}
	if ctxName == "edit" {
		content["raw"] = cm.Content
	}
	out := map[string]any{
		"id": cm.ID, "post": cm.PostID, "parent": cm.ParentID, "author": cm.UserID, "author_name": cm.AuthorName,
		"author_url": cm.AuthorURL, "date": cm.CreatedAt.In(loc).Format("2006-01-02T15:04:05"), "date_gmt": gmt(cm.CreatedAt),
		"content": content, "link": link, "status": restCommentStatus(cm.Status), "type": cm.Type,
		"author_avatar_urls": map[string]string{"24": s.avatarURL(c, cm.AuthorEmail, 24), "48": s.avatarURL(c, cm.AuthorEmail, 48), "96": s.avatarURL(c, cm.AuthorEmail, 96)},
		"meta":               []any{},
		"_links":             restLinks(self, home+"/wp-json/wp/v2/comments", extra),
	}
	if ctxName == "edit" {
		out["author_email"] = cm.AuthorEmail
		out["author_ip"] = cm.AuthorIP
		out["author_user_agent"] = cm.UserAgent
	}
	if ctxName == "embed" {
		return map[string]any{"id": cm.ID, "parent": cm.ParentID, "author": cm.UserID, "author_name": cm.AuthorName, "author_url": cm.AuthorURL,
			"date": out["date"], "content": map[string]any{"rendered": commentText(cm.Content)}, "link": link, "type": cm.Type,
			"author_avatar_urls": out["author_avatar_urls"], "_links": out["_links"]}
	}
	return out
}

func canModerate(u *store.User) bool { return u != nil && u.Can("moderate_comments", nil) }

// forbidden answers with 401 for visitors and 403 for logged-in users, like rest_authorization_required_code().
func forbidden(c *gin.Context, code, msg string) {
	status := http.StatusForbidden
	if restCurrent(c) == nil {
		status = http.StatusUnauthorized
	}
	restErr(c, status, code, msg)
}

// postReadableWithPassword is check_read_post_permission(): published posts, protected ones with the right
// password (or edit rights), the rest only for users who may read them.
func (api *restAPI) postReadableWithPassword(c *gin.Context, p *store.Post, password string) bool {
	u := restCurrent(c)
	if p.Password != "" {
		if password != "" && password == p.Password {
			return p.Status == "publish" || (u != nil && u.Can("read_post", p))
		}
		return u != nil && u.Can("edit_post", p)
	}
	return api.readable(c, p)
}

func (api *restAPI) listComments(c *gin.Context, r *restRequest) {
	s := api.s
	u := restCurrent(c)
	ctxName := restContext(c)
	if ctxName == "edit" && !canModerate(u) {
		forbidden(c, "rest_forbidden_context", "Sorry, you are not allowed to edit comments.")
		return
	}
	status := c.DefaultQuery("status", "approve")
	if !canModerate(u) {
		if status != "approve" && status != "approved" {
			forbidden(c, "rest_forbidden_param", "Query parameter not permitted: status")
			return
		}
		for _, p := range []string{"author_email", "author_exclude", "author"} {
			if _, ok := c.GetQuery(p); ok && !(u != nil && u.Can("edit_posts", nil)) {
				forbidden(c, "rest_forbidden_param", "Query parameter not permitted: "+p)
				return
			}
		}
	}
	page, per := pageArgs(c)
	q := store.CommentQuery{Search: c.Query("search"), Order: c.DefaultQuery("order", "desc"), OrderBy: c.DefaultQuery("orderby", "date_gmt"), PerPage: 100000}
	switch status {
	case "approve", "approved":
		q.Status = "approved"
	case "hold":
		q.Status = "pending"
	case "all", "any":
		q.Status = "all"
	default:
		q.Status = status
	}
	if q.OrderBy == "date_gmt" || q.OrderBy == "date" {
		q.OrderBy = "date"
	}
	postIDs, _ := r.IDs(c, "post")
	for _, pid := range postIDs {
		p, err := s.store.GetPost(c, pid)
		if err != nil || !api.postReadableWithPassword(c, p, c.Query("password")) {
			forbidden(c, "rest_cannot_read_post", "Sorry, you are not allowed to read the post for this comment.")
			return
		}
	}
	if len(postIDs) == 1 {
		q.PostID = postIDs[0]
	}
	if a := c.Query("author"); a != "" && !strings.Contains(a, ",") {
		q.UserID, _ = strconv.ParseInt(a, 10, 64)
	}
	list, _, err := s.store.QueryComments(c, q)
	if err != nil {
		restErr(c, http.StatusInternalServerError, "rest_query_error", err.Error())
		return
	}
	include, hasInclude := r.IDs(c, "include")
	exclude, _ := r.IDs(c, "exclude")
	authors, _ := r.IDs(c, "author")
	authorsEx, _ := r.IDs(c, "author_exclude")
	parents, hasParent := r.IDs(c, "parent")
	parentsEx, _ := r.IDs(c, "parent_exclude")
	inSet := func(ids []int64, id int64) bool {
		for _, x := range ids {
			if x == id {
				return true
			}
		}
		return false
	}
	var filtered []*store.Comment
	for _, cm := range list {
		if len(postIDs) > 1 && !inSet(postIDs, cm.PostID) {
			continue
		}
		if hasInclude && !inSet(include, cm.ID) || inSet(exclude, cm.ID) {
			continue
		}
		if len(authors) > 0 && !inSet(authors, cm.UserID) || inSet(authorsEx, cm.UserID) {
			continue
		}
		if hasParent && !inSet(parents, cm.ParentID) || inSet(parentsEx, cm.ParentID) {
			continue
		}
		if ae := c.Query("author_email"); ae != "" && !strings.EqualFold(ae, cm.AuthorEmail) {
			continue
		}
		if t := c.Query("type"); t != "" && t != cm.Type {
			continue
		}
		if len(postIDs) == 0 && u == nil {
			// Visitors only see comments of posts they may read.
			if p, err := s.store.GetPost(c, cm.PostID); err != nil || !api.postReadableWithPassword(c, p, "") {
				continue
			}
		}
		filtered = append(filtered, cm)
	}
	offset, _ := strconv.Atoi(c.Query("offset"))
	if offset <= 0 {
		offset = (page - 1) * per
	}
	out := []map[string]any{}
	for i, cm := range filtered {
		if i < offset || i >= offset+per {
			continue
		}
		out = append(out, s.restComment(c, cm, ctxName))
	}
	paginate(c, len(filtered), per, page)
	restJSON(c, http.StatusOK, out)
}

// loadComment reads the comment of a single-item route and checks that the caller may see it.
func (api *restAPI) loadComment(c *gin.Context, r *restRequest) (*store.Comment, *store.Post, bool) {
	s := api.s
	cm, err := s.store.GetComment(c, r.ID())
	if err != nil {
		restErr(c, http.StatusNotFound, "rest_comment_invalid_id", "Invalid comment ID.")
		return nil, nil, false
	}
	p, err := s.store.GetPost(c, cm.PostID)
	if err != nil {
		restErr(c, http.StatusNotFound, "rest_comment_invalid_id", "Invalid comment ID.")
		return nil, nil, false
	}
	return cm, p, true
}

func (api *restAPI) getComment(c *gin.Context, r *restRequest) {
	u := restCurrent(c)
	cm, p, ok := api.loadComment(c, r)
	if !ok {
		return
	}
	if !canModerate(u) {
		if !api.postReadableWithPassword(c, p, c.Query("password")) {
			forbidden(c, "rest_cannot_read_post", "Sorry, you are not allowed to read the post for this comment.")
			return
		}
		if cm.Status != "approved" && !(u != nil && cm.UserID == u.ID) {
			forbidden(c, "rest_cannot_read", "Sorry, you are not allowed to read this comment.")
			return
		}
	}
	ctxName := restContext(c)
	if ctxName == "edit" && !(u != nil && u.Can("edit_post", p)) {
		forbidden(c, "rest_forbidden_context", "Sorry, you are not allowed to edit this comment.")
		return
	}
	restJSON(c, http.StatusOK, api.s.restComment(c, cm, ctxName))
}

// createComment is create_item(). Anonymous comments require the rest_allow_anonymous_comments filter in
// WordPress, which is off by default: visitors get rest_comment_login_required.
func (api *restAPI) createComment(c *gin.Context, r *restRequest) {
	s := api.s
	u := restCurrent(c)
	if u == nil {
		restErr(c, http.StatusUnauthorized, "rest_comment_login_required", "Sorry, you must be logged in to comment.")
		return
	}
	if id, _ := r.Int(c, "id"); id != 0 {
		restErr(c, http.StatusBadRequest, "rest_comment_exists", "Cannot create existing comment.")
		return
	}
	if t, ok := r.Str(c, "type"); ok && t != "" && t != "comment" {
		restErr(c, http.StatusBadRequest, "rest_invalid_comment_type", "Cannot create a comment with that type.")
		return
	}
	postID, _ := r.Int(c, "post")
	if postID == 0 {
		restErr(c, http.StatusForbidden, "rest_comment_invalid_post_id", "Sorry, you are not allowed to create this comment without a post.")
		return
	}
	p, err := s.store.GetPost(c, postID)
	if err != nil {
		restErr(c, http.StatusForbidden, "rest_comment_invalid_post_id", "Sorry, you are not allowed to create this comment without a post.")
		return
	}
	if author, ok := r.Int(c, "author"); ok && author != u.ID && !canModerate(u) {
		restErr(c, http.StatusForbidden, "rest_comment_invalid_author", "Sorry, you are not allowed to edit 'author' for comments.")
		return
	}
	if _, ok := r.Str(c, "status"); ok && !canModerate(u) {
		restErr(c, http.StatusForbidden, "rest_comment_invalid_status", "Sorry, you are not allowed to assign the provided status.")
		return
	}
	password, _ := r.Str(c, "password")
	if !api.postReadableWithPassword(c, p, password) {
		restErr(c, http.StatusForbidden, "rest_cannot_read_post", "Sorry, you are not allowed to read the post for this comment.")
		return
	}
	if p.Status == "draft" || p.Status == "auto-draft" || p.Status == "pending" {
		restErr(c, http.StatusForbidden, "rest_comment_draft_post", "Sorry, you cannot create a comment on this post.")
		return
	}
	if p.Status == "trash" {
		restErr(c, http.StatusForbidden, "rest_comment_trash_post", "Sorry, you cannot create a comment on this post.")
		return
	}
	if p.CommentStatus != "open" {
		restErr(c, http.StatusForbidden, "rest_comment_closed", "Sorry, comments are closed for this item.")
		return
	}
	cm := &store.Comment{PostID: p.ID, UserID: u.ID, AuthorName: u.DisplayName, AuthorEmail: u.Email, AuthorURL: u.URL,
		AuthorIP: clientIP(c), UserAgent: c.Request.UserAgent(), Type: "comment"}
	if author, ok := r.Int(c, "author"); ok && author != u.ID {
		if au, err := s.store.UserByID(c, author); err == nil {
			cm.UserID, cm.AuthorName, cm.AuthorEmail, cm.AuthorURL = au.ID, au.DisplayName, au.Email, au.URL
		} else {
			restErr(c, http.StatusBadRequest, "rest_comment_author_invalid", "Invalid comment author ID.")
			return
		}
	}
	if !api.applyCommentFields(c, r, cm, u, true) {
		return
	}
	if strings.TrimSpace(cm.Content) == "" {
		restErr(c, http.StatusBadRequest, "rest_comment_content_invalid", "Invalid comment content.")
		return
	}
	if dup, _ := s.store.IsDuplicateComment(c, cm); dup {
		restErr(c, http.StatusConflict, "comment_duplicate", "Duplicate comment detected; it looks as though you&#8217;ve already said that!")
		return
	}
	if st, ok := r.Str(c, "status"); ok {
		if mapped, ok := storeCommentStatus(st); ok {
			cm.Status = mapped
		}
	}
	if cm.Status == "" {
		cm.Status = s.commentStatus(c, cm, u)
	}
	if err := s.store.InsertComment(c, cm); err != nil {
		restErr(c, http.StatusInternalServerError, "rest_comment_failed_create", "Creating comment failed.")
		return
	}
	if date, ok := commentDateParam(c, r, s); ok {
		_, _ = s.store.Pool.Exec(c, `UPDATE comments SET created_at = $2 WHERE id = $1`, cm.ID, date)
		cm.CreatedAt = date
	}
	s.afterPostSave(c, p)
	c.Header("Location", s.links(c).HomeURL("/wp-json/wp/v2/comments/"+itoa64(cm.ID)))
	restJSON(c, http.StatusCreated, s.restComment(c, cm, "edit"))
}

// applyCommentFields copies the editable fields of a request onto a comment.
func (api *restAPI) applyCommentFields(c *gin.Context, r *restRequest, cm *store.Comment, u *store.User, create bool) bool {
	s := api.s
	if v, ok := r.Str(c, "content"); ok {
		if u.Can("unfiltered_html", nil) {
			cm.Content = v
		} else {
			cm.Content = wp.KsesPost(v)
		}
	}
	if v, ok := r.Str(c, "author_name"); ok {
		cm.AuthorName = v
	}
	if v, ok := r.Str(c, "author_email"); ok {
		if v != "" && !validEmail(v) {
			restErr(c, http.StatusBadRequest, "rest_invalid_param", "Invalid parameter(s): author_email")
			return false
		}
		cm.AuthorEmail = v
	}
	if v, ok := r.Str(c, "author_url"); ok {
		cm.AuthorURL = v
	}
	if canModerate(u) {
		if v, ok := r.Str(c, "author_ip"); ok {
			cm.AuthorIP = v
		}
		if v, ok := r.Str(c, "author_user_agent"); ok {
			cm.UserAgent = v
		}
	}
	if v, ok := r.Int(c, "parent"); ok {
		if v != 0 {
			parent, err := s.store.GetComment(c, v)
			if err != nil || parent.PostID != cm.PostID {
				restErr(c, http.StatusBadRequest, "rest_comment_invalid_parent", "Invalid comment parent ID.")
				return false
			}
		}
		cm.ParentID = v
	}
	return true
}

// commentDateParam reads date / date_gmt (Y-m-d\TH:i:s).
func commentDateParam(c *gin.Context, r *restRequest, s *Server) (time.Time, bool) {
	if v, ok := r.Str(c, "date_gmt"); ok && v != "" {
		if t, err := time.Parse("2006-01-02T15:04:05", v); err == nil {
			return t, true
		}
	}
	if v, ok := r.Str(c, "date"); ok && v != "" {
		if t, err := time.ParseInLocation("2006-01-02T15:04:05", v, s.loc()); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func (api *restAPI) updateComment(c *gin.Context, r *restRequest) {
	s := api.s
	u := restCurrent(c)
	cm, p, ok := api.loadComment(c, r)
	if !ok {
		return
	}
	if !(u != nil && u.Can("edit_post", p)) {
		forbidden(c, "rest_cannot_edit", "Sorry, you are not allowed to edit this comment.")
		return
	}
	if t, ok := r.Str(c, "type"); ok && t != cm.Type {
		restErr(c, http.StatusBadRequest, "rest_comment_invalid_type", "Sorry, you are not allowed to change the comment type.")
		return
	}
	if pid, ok := r.Int(c, "post"); ok && pid != cm.PostID {
		np, err := s.store.GetPost(c, pid)
		if err != nil {
			restErr(c, http.StatusForbidden, "rest_comment_invalid_post_id", "Invalid post ID.")
			return
		}
		cm.PostID = np.ID
		p = np
	}
	if author, ok := r.Int(c, "author"); ok && author != cm.UserID {
		if !canModerate(u) {
			restErr(c, http.StatusForbidden, "rest_comment_invalid_author", "Sorry, you are not allowed to edit 'author' for comments.")
			return
		}
		cm.UserID = author
	}
	if !api.applyCommentFields(c, r, cm, u, false) {
		return
	}
	if strings.TrimSpace(cm.Content) == "" {
		restErr(c, http.StatusBadRequest, "rest_comment_content_invalid", "Invalid comment content.")
		return
	}
	if date, ok := commentDateParam(c, r, s); ok {
		cm.CreatedAt = date
	}
	if st, ok := r.Str(c, "status"); ok {
		mapped, valid := storeCommentStatus(st)
		if !valid {
			restErr(c, http.StatusBadRequest, "rest_invalid_param", "Invalid parameter(s): status")
			return
		}
		if mapped != cm.Status {
			if !canModerate(u) {
				restErr(c, http.StatusForbidden, "rest_comment_invalid_status", "Sorry, you are not allowed to assign the provided status.")
				return
			}
			if err := s.store.SetCommentStatus(c, cm.ID, mapped); err != nil {
				restErr(c, http.StatusInternalServerError, "rest_comment_failed_edit", "Updating comment status failed.")
				return
			}
			cm.Status = mapped
		}
	}
	if _, err := s.store.Pool.Exec(c, `UPDATE comments SET post_id = $2, parent_id = $3, user_id = $4, author_name = $5, author_email = $6, author_url = $7,
		author_ip = $8, user_agent = $9, content = $10, created_at = $11 WHERE id = $1`, cm.ID, cm.PostID, nullable64(cm.ParentID), nullable64(cm.UserID),
		cm.AuthorName, cm.AuthorEmail, cm.AuthorURL, cm.AuthorIP, cm.UserAgent, cm.Content, cm.CreatedAt); err != nil {
		restErr(c, http.StatusInternalServerError, "rest_comment_failed_edit", "Updating comment failed.")
		return
	}
	_ = s.store.RecountComments(c, cm.PostID)
	cm, _ = s.store.GetComment(c, cm.ID)
	s.afterPostSave(c, p)
	restJSON(c, http.StatusOK, s.restComment(c, cm, "edit"))
}

func (api *restAPI) deleteComment(c *gin.Context, r *restRequest) {
	s := api.s
	u := restCurrent(c)
	cm, p, ok := api.loadComment(c, r)
	if !ok {
		return
	}
	if !(u != nil && u.Can("edit_post", p)) {
		forbidden(c, "rest_cannot_delete", "Sorry, you are not allowed to delete this comment.")
		return
	}
	force, _ := r.Bool(c, "force")
	prev := s.restComment(c, cm, "edit")
	if force {
		if err := s.store.DeleteComment(c, cm.ID); err != nil {
			restErr(c, http.StatusInternalServerError, "rest_cannot_delete", "The comment cannot be deleted.")
			return
		}
		s.afterPostSave(c, p)
		restJSON(c, http.StatusOK, map[string]any{"deleted": true, "previous": prev})
		return
	}
	if cm.Status == "trash" {
		restErr(c, http.StatusGone, "rest_already_trashed", "The comment has already been trashed.")
		return
	}
	if err := s.store.SetCommentStatus(c, cm.ID, "trash"); err != nil {
		restErr(c, http.StatusInternalServerError, "rest_cannot_delete", "The comment cannot be deleted.")
		return
	}
	cm, _ = s.store.GetComment(c, cm.ID)
	s.afterPostSave(c, p)
	restJSON(c, http.StatusOK, s.restComment(c, cm, "edit"))
}
