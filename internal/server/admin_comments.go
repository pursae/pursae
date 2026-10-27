package server

import (
	"bytes"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"wpgo/internal/links"
	"wpgo/internal/store"
	"wpgo/internal/wp"
)

type commentRow struct {
	ID, PostID                        int64
	Status, Class                     string
	Avatar                            template.HTML
	AuthorName, AuthorURL, AuthorHost string
	AuthorEmail, AuthorIP             string
	Content                           template.HTML
	RawContent                        string // the hidden Quick Edit textarea
	StatusFlag                        string // comment_approved: 1, 0, spam, trash
	ParentNote                        template.HTML
	PostTitle, PostEditURL, PostURL   string
	PostCount                         int
	Date                              string
	Permalink                         string
	Actions                           template.HTML
	CanEdit                           bool
}

type commentsListData struct {
	Rows        []commentRow
	Status      string
	Counts      map[string]int
	All         int
	Search      string
	Pagination  pagination
	BulkNonce   string
	ReplyNonce  string
	PostID      int64
	PostTitle   string
	SortLinks   map[string]sortLink
	Type        string // comment_type filter
	Hidden      map[string]bool
	Mode        string
	ColCount    int
	Total       int
	PerPage     int
	Page        int
	CanModerate bool
}

// commentListColumns are the Screen Options column toggles of edit-comments.php.
var commentListColumns = []screenColumn{{"author", "Author"}, {"response", "In response to"}, {"date", "Submitted on"}}

// commentsScreen is edit-comments.php.
func (s *Server) commentsScreen(c *gin.Context) {
	u := currentUser(c)
	if !u.Can("edit_posts", nil) {
		c.String(http.StatusForbidden, "Sorry, you are not allowed to edit comments.")
		return
	}
	if c.Request.Method == http.MethodPost {
		if !s.setScreenOption(c) {
			c.Redirect(http.StatusFound, "/wp-admin/edit-comments")
		}
		return
	}
	if a := c.Query("action"); a != "" && a != "-1" || c.Query("action2") != "" && c.Query("action2") != "-1" {
		s.commentsBulk(c)
		return
	}
	ctx := c.Request.Context()
	page, _ := strconv.Atoi(c.Query("paged"))
	perPage := s.userOptionInt(u, "edit_comments_per_page", 20)
	q := store.CommentQuery{Status: c.DefaultQuery("comment_status", "all"), Search: strings.TrimSpace(c.Query("s")), Page: page, PerPage: perPage,
		OrderBy: map[string]string{"comment_author": "author", "comment_post_ID": "post", "author": "author", "post": "post"}[c.Query("orderby")], Order: c.Query("order"), Type: c.Query("comment_type")}
	if q.Status == "mine" {
		q.Status, q.UserID = "all", u.ID
	}
	d := commentsListData{Status: c.DefaultQuery("comment_status", "all"), Search: q.Search, BulkNonce: s.nonce(c, "bulk-comments"), Type: q.Type,
		ReplyNonce: s.nonce(c, "replyto-comment"), Hidden: s.hiddenColumns(u, "edit-comments"), Mode: s.listMode(c), PerPage: perPage, Page: max(page, 1),
		CanModerate: u.Can("moderate_comments", nil),
		SortLinks:   sortLinks(c, map[string]bool{"comment_author": false, "comment_post_ID": false, "comment_date": true}, "comment_date", "desc")}
	d.ColCount = 2
	for _, col := range commentListColumns {
		if !d.Hidden[col.ID] {
			d.ColCount++
		}
	}
	if pid, _ := strconv.ParseInt(c.Query("p"), 10, 64); pid > 0 {
		q.PostID, d.PostID = pid, pid
		if p, err := s.store.GetPost(ctx, pid); err == nil {
			d.PostTitle = p.Title
		}
	}
	if !u.Can("moderate_comments", nil) {
		q.UserID = 0 // authors see comments on their own posts only; simplified: list all approved
		if q.Status == "all" {
			q.Status = "approved"
		}
	}
	list, total, err := s.store.QueryComments(ctx, q)
	if err != nil {
		c.String(http.StatusInternalServerError, err.Error())
		return
	}
	d.Total = total
	d.Counts, _ = s.store.CommentCounts(ctx)
	d.All = d.Counts["pending"] + d.Counts["approved"]
	posts := map[int64]*store.Post{}
	for i, cm := range list {
		d.Rows = append(d.Rows, s.commentRow(c, cm, posts, i%2 == 1))
	}
	d.Pagination = newPagination(total, perPage, max(page, 1), *c.Request.URL)
	var notices []notice
	for _, k := range []struct{ key, one, many string }{
		{"approved", "%d comment approved.", "%d comments approved."}, {"unapproved", "%d comment unapproved.", "%d comments unapproved."},
		{"spammed", "%d comment marked as spam.", "%d comments marked as spam."}, {"unspammed", "%d comment restored from the spam.", "%d comments restored from the spam."},
		{"trashed", "%d comment moved to the Trash.", "%d comments moved to the Trash."}, {"untrashed", "%d comment restored from the Trash.", "%d comments restored from the Trash."},
		{"deleted", "%d comment permanently deleted.", "%d comments permanently deleted."}, {"updated", "Comment updated.", "Comment updated."},
	} {
		if n, _ := strconv.Atoi(c.Query(k.key)); n > 0 {
			msg := k.one
			if n > 1 {
				msg = k.many
			}
			if strings.Contains(msg, "%d") {
				msg = fmt.Sprintf(msg, n)
			}
			notices = append(notices, notice{Type: "success", Message: template.HTML(msg), Dismissible: true})
		}
	}
	s.setScreenOptions(c, screenOptions{Screen: "edit-comments", Columns: commentListColumns, Hidden: d.Hidden,
		PerPageOption: "edit_comments_per_page", PerPage: perPage, ViewMode: d.Mode})
	a := s.pageAssets(c)
	a.Script("admin-comments").Style("editor-buttons")
	// wp_comment_reply() builds the reply box with wp_editor( '', 'replycontent', { tinymce: false, quicktags: ... } ).
	a.AddInline("quicktags", `quicktags({id:"replycontent",buttons:"strong,em,link,block,del,ins,img,ul,ol,li,code,close"});`)
	s.adminPage(c, "edit-comments", "Comments", "edit-comments-php", "admin/comments-list", d, notices...)
}

// commentRow builds one row of the comments table (WP_Comments_List_Table::single_row()).
func (s *Server) commentRow(c *gin.Context, cm *store.Comment, posts map[int64]*store.Post, odd bool) commentRow {
	ctx := c.Request.Context()
	u := currentUser(c)
	lb := s.links(ctx)
	loc := s.loc()
	p := posts[cm.PostID]
	if p == nil {
		p, _ = s.store.GetPost(ctx, cm.PostID)
		posts[cm.PostID] = p
	}
	r := commentRow{ID: cm.ID, PostID: cm.PostID, Status: cm.Status, AuthorName: defaultStr(cm.AuthorName, "Anonymous"), AuthorURL: cm.AuthorURL,
		AuthorEmail: cm.AuthorEmail, AuthorIP: cm.AuthorIP, Content: template.HTML(wp.Autop(commentText(cm.Content))), RawContent: cm.Content,
		Date:       wp.FormatDate(cm.CreatedAt.In(loc), "Y/m/d") + " at " + wp.FormatDate(cm.CreatedAt.In(loc), "g:i a"),
		Avatar:     template.HTML(s.avatarImg(ctx, cm.AuthorEmail, 32)),
		StatusFlag: map[string]string{"approved": "1", "pending": "0", "spam": "spam", "trash": "trash"}[cm.Status],
		CanEdit:    u.Can("moderate_comments", nil)}
	if pu, err := url.Parse(cm.AuthorURL); err == nil && pu.Host != "" {
		r.AuthorHost = strings.TrimPrefix(pu.Host, "www.")
	}
	parity := "even"
	if odd {
		parity = "odd alt"
	}
	r.Class = fmt.Sprintf("comment %s thread-%s depth-1 %s", parity, strings.Fields(parity)[0], map[string]string{"pending": "unapproved", "approved": "approved", "spam": "spam", "trash": "trash"}[cm.Status])
	if p != nil {
		r.PostTitle, r.PostEditURL, r.PostURL, r.PostCount = defaultStr(p.Title, "(no title)"), links.EditLink(p.ID), lb.Permalink(p), p.CommentCount
		r.Permalink = r.PostURL + "#comment-" + itoa64(cm.ID)
	}
	if cm.ParentID > 0 {
		if parent, err := s.store.GetComment(ctx, cm.ParentID); err == nil {
			r.ParentNote = template.HTML(fmt.Sprintf(`In reply to <a href="%s#comment-%d">%s</a>.`, template.HTMLEscapeString(r.PostURL), parent.ID, template.HTMLEscapeString(parent.AuthorName)))
		}
	}
	if r.CanEdit {
		r.Actions = s.commentActions(c, cm)
	}
	return r
}

// commentRowHTML renders one comment row for the admin-ajax responses that insert it into the list.
func (s *Server) commentRowHTML(c *gin.Context, cm *store.Comment) string {
	d := commentsListData{Hidden: s.hiddenColumns(currentUser(c), "edit-comments"), Mode: s.listMode(c)}
	var buf bytes.Buffer
	if err := s.tmpl.get(false).ExecuteTemplate(&buf, "comment-row", map[string]any{"R": s.commentRow(c, cm, map[int64]*store.Post{}, false), "D": d}); err != nil {
		return err.Error()
	}
	return buf.String()
}

// commentActions are the row actions of WP_Comments_List_Table::handle_row_actions(): the links carry
// the data-wp-lists instructions wp-lists.js turns into dim-comment / delete-comment ajax calls.
func (s *Server) commentActions(c *gin.Context, cm *store.Comment) template.HTML {
	id := itoa64(cm.ID)
	approve := s.nonce(c, "approve-comment_"+id)
	del := s.nonce(c, "delete-comment_"+id)
	link := func(action, nonce string) string {
		return template.HTMLEscapeString("/wp-admin/comment?c=" + id + "&action=" + action + "&_wpnonce=" + nonce + "&_wp_http_referer=" + url.QueryEscape(c.Request.URL.RequestURI()))
	}
	status := map[string]string{"approved": "approved", "pending": "unapproved", "spam": "spam", "trash": "trash"}[cm.Status]
	view := c.Query("comment_status")
	type action struct{ key, html string }
	var actions []action
	add := func(key, html string) { actions = append(actions, action{key, html}) }
	a := func(href, lists, class, aria, label string) string {
		return fmt.Sprintf(`<a href="%s" data-wp-lists="%s" class="%s" aria-label="%s">%s</a>`, href, lists, class, aria, label)
	}
	if view != "" && view != "all" {
		if status == "approved" {
			add("unapprove", a(link("unapprovecomment", approve), "delete:the-comment-list:comment-"+id+":e7e7d3:action=dim-comment&amp;new=unapproved", "vim-u vim-destructive aria-button-if-js", "Unapprove this comment", "Unapprove"))
		} else if status == "unapproved" {
			add("approve", a(link("approvecomment", approve), "delete:the-comment-list:comment-"+id+":e7e7d3:action=dim-comment&amp;new=approved", "vim-a vim-destructive aria-button-if-js", "Approve this comment", "Approve"))
		}
	} else {
		add("approve", a(link("approvecomment", approve), "dim:the-comment-list:comment-"+id+":unapproved:e7e7d3:e7e7d3:new=approved", "vim-a aria-button-if-js", "Approve this comment", "Approve"))
		add("unapprove", a(link("unapprovecomment", approve), "dim:the-comment-list:comment-"+id+":unapproved:e7e7d3:e7e7d3:new=unapproved", "vim-u aria-button-if-js", "Unapprove this comment", "Unapprove"))
	}
	if status != "spam" && status != "trash" {
		btn := func(action, class, aria, label string) string {
			return fmt.Sprintf(`<button type="button" data-comment-id="%s" data-post-id="%d" data-action="%s" class="%s button-link" aria-expanded="false" aria-label="%s">%s</button>`, id, cm.PostID, action, class, aria, label)
		}
		add("reply hide-if-no-js", btn("replyto", "vim-r comment-inline", "Reply to this comment", "Reply"))
		add("quickedit hide-if-no-js", btn("edit", "vim-q comment-inline", "Quick edit this comment inline", "Quick&nbsp;Edit"))
		add("edit", fmt.Sprintf(`<a href="/wp-admin/comment?action=editcomment&amp;c=%s" aria-label="Edit this comment">Edit</a>`, id))
	}
	if status != "spam" {
		add("spam", a(link("spamcomment", del), "delete:the-comment-list:comment-"+id+"::spam=1", "vim-s vim-destructive aria-button-if-js", "Mark this comment as spam", "Spam"))
	} else {
		add("unspam", a(link("unspamcomment", del), "delete:the-comment-list:comment-"+id+":66cc66:unspam=1", "vim-z vim-destructive aria-button-if-js", "Restore this comment from the spam", "Not Spam"))
	}
	if status == "trash" {
		add("untrash", a(link("untrashcomment", del), "delete:the-comment-list:comment-"+id+":66cc66:untrash=1", "vim-z vim-destructive aria-button-if-js", "Restore this comment from the Trash", "Restore"))
	}
	if status == "spam" || status == "trash" {
		add("delete", a(link("deletecomment", del), "delete:the-comment-list:comment-"+id+"::delete=1", "delete vim-d vim-destructive aria-button-if-js", "Delete this comment permanently", "Delete Permanently"))
	} else {
		add("trash", a(link("trashcomment", del), "delete:the-comment-list:comment-"+id+"::trash=1", "delete vim-d vim-destructive aria-button-if-js", "Move this comment to the Trash", "Trash"))
	}
	class := "row-actions"
	if s.listMode(c) == "excerpt" {
		class = "row-actions visible"
	}
	var b strings.Builder
	b.WriteString(`<div class="` + class + `">`)
	for i, act := range actions {
		sep := " | "
		if i == 0 || (i == 1 && strings.HasPrefix(act.key, "unapprove")) {
			sep = ""
		}
		key := act.key
		if (key == "untrash" || key == "unspam") && cm.Meta["_wp_trash_meta_status"] != nil {
			if cm.Meta["_wp_trash_meta_status"] == "approved" {
				key += " approve"
			} else {
				key += " unapprove"
			}
		}
		b.WriteString(`<span class='` + key + `'>` + sep + act.html + `</span>`)
	}
	b.WriteString(`</div>`)
	return template.HTML(b.String())
}

// ajaxResponse is one <response> of WP_Ajax_Response, the XML admin-ajax answers wp-lists.js and
// edit-comments.js parse (wpAjax.parseAjaxResponse()).
type ajaxResponse struct {
	What, Action string
	ID           int64
	OldID        int64 // printed when >0 (a row replaced in place)
	Position     string
	Data         string
	Error        string // a wp_error instead of response_data
	Supplemental [][2]string
}

func ajaxCData(v string) string {
	return "<![CDATA[" + strings.ReplaceAll(v, "]]>", "]]]]><![CDATA[>") + "]]>"
}

// sendAjaxResponse is WP_Ajax_Response::send().
func sendAjaxResponse(c *gin.Context, items ...ajaxResponse) {
	var b strings.Builder
	b.WriteString("<?xml version='1.0' encoding='utf-8' standalone='yes'?><wp_ajax>")
	for _, r := range items {
		fmt.Fprintf(&b, "<response action='%s_%d'><%s id='%d' ", r.Action, r.ID, r.What, r.ID)
		if r.OldID > 0 {
			fmt.Fprintf(&b, "old_id='%d' ", r.OldID)
		}
		fmt.Fprintf(&b, "position='%s'>", defaultStr(r.Position, "1"))
		if r.Error != "" {
			b.WriteString("<wp_error code='1'>" + ajaxCData(r.Error) + "</wp_error>")
		} else {
			b.WriteString("<response_data>" + ajaxCData(r.Data) + "</response_data>")
		}
		if len(r.Supplemental) > 0 {
			b.WriteString("<supplemental>")
			for _, kv := range r.Supplemental {
				b.WriteString("<" + kv[0] + ">" + ajaxCData(kv[1]) + "</" + kv[0] + ">")
			}
			b.WriteString("</supplemental>")
		}
		fmt.Fprintf(&b, "</%s></response>", r.What)
	}
	b.WriteString("</wp_ajax>")
	c.Data(http.StatusOK, "text/xml; charset=utf-8", []byte(b.String()))
}

// commentCountSupplemental is the moderation count edit-comments.js refreshes the menu bubble with.
func (s *Server) commentCountSupplemental(c *gin.Context) [][2]string {
	counts, _ := s.store.CommentCounts(c)
	n := counts["pending"]
	plural := "%s Comments in moderation"
	if n == 1 {
		plural = "%s Comment in moderation"
	}
	return [][2]string{{"in_moderation", strconv.Itoa(n)}, {"i18n_comments_text", fmt.Sprintf("%d Comments", counts["approved"]+n)},
		{"i18n_moderation_text", fmt.Sprintf(plural, strconv.Itoa(n))}}
}

// replyToCommentAjax is wp_ajax_replyto_comment(): the inline Reply box on edit-comments.php and the
// dashboard adds an approved reply by the current user; replying to a pending comment approves it.
func (s *Server) replyToCommentAjax(c *gin.Context) {
	ctx := c.Request.Context()
	u := currentUser(c)
	if !s.checkNonceValue(c, c.PostForm("_ajax_nonce-replyto-comment"), "replyto-comment") {
		c.String(http.StatusForbidden, "-1")
		return
	}
	pid, _ := strconv.ParseInt(c.PostForm("comment_post_ID"), 10, 64)
	p, err := s.store.GetPost(ctx, pid)
	if err != nil {
		c.String(http.StatusOK, "-1")
		return
	}
	if !u.Can("edit_post", p) {
		c.String(http.StatusOK, "-1")
		return
	}
	if p.Status == "trash" || p.Status == "draft" || p.Status == "pending" || p.Status == "auto-draft" {
		c.String(http.StatusOK, "You cannot reply to a comment on a draft post.")
		return
	}
	content := strings.TrimSpace(c.PostForm("content"))
	if content == "" {
		c.String(http.StatusOK, "Please type your comment text.")
		return
	}
	if !u.Can("unfiltered_html", nil) {
		content = wp.KsesPost(content)
	}
	parentID, _ := strconv.ParseInt(c.PostForm("comment_ID"), 10, 64)
	var parentApproved *store.Comment
	if c.PostForm("approve_parent") != "" && parentID > 0 {
		if parent, err := s.store.GetComment(ctx, parentID); err == nil && parent.Status == "pending" && parent.PostID == pid {
			if u.Can("moderate_comments", nil) && s.store.SetCommentStatus(ctx, parent.ID, "approved") == nil {
				parentApproved = parent
			}
		}
	}
	cm := &store.Comment{PostID: pid, ParentID: parentID, UserID: u.ID, AuthorName: u.DisplayName, AuthorEmail: u.Email, AuthorURL: u.URL,
		AuthorIP: clientIP(c), UserAgent: c.Request.UserAgent(), Content: content, Status: "approved"}
	if !u.Can("moderate_comments", nil) {
		cm.Status = "pending"
	}
	if err := s.store.InsertComment(ctx, cm); err != nil {
		c.String(http.StatusOK, err.Error())
		return
	}
	s.purgeTags("post:"+itoa64(pid), "feed", "widgets")
	cm, _ = s.store.GetComment(ctx, cm.ID)
	position := "-1"
	if n, _ := strconv.Atoi(c.PostForm("position")); n != 0 {
		position = strconv.Itoa(n)
	}
	resp := ajaxResponse{What: "comment", Action: "replyto-comment", ID: cm.ID, Position: position, Data: s.commentRowHTML(c, cm), Supplemental: s.commentCountSupplemental(c)}
	if parentApproved != nil {
		resp.Supplemental = append(resp.Supplemental, [2]string{"parent_approved", itoa64(parentApproved.ID)}, [2]string{"parent_post_id", itoa64(parentApproved.PostID)})
	}
	sendAjaxResponse(c, resp)
}

// editCommentAjax is wp_ajax_edit_comment(): Quick Edit on a comment row.
func (s *Server) editCommentAjax(c *gin.Context) {
	ctx := c.Request.Context()
	u := currentUser(c)
	if !s.checkNonceValue(c, c.PostForm("_ajax_nonce-replyto-comment"), "replyto-comment") {
		c.String(http.StatusForbidden, "-1")
		return
	}
	id, _ := strconv.ParseInt(c.PostForm("comment_ID"), 10, 64)
	cm, err := s.store.GetComment(ctx, id)
	if err != nil || !u.Can("moderate_comments", nil) {
		c.String(http.StatusOK, "-1")
		return
	}
	if c.PostForm("content") == "" {
		c.String(http.StatusOK, "Please type your comment text.")
		return
	}
	cm.AuthorName = strings.TrimSpace(c.PostForm("newcomment_author"))
	cm.AuthorEmail = strings.TrimSpace(c.PostForm("newcomment_author_email"))
	cm.AuthorURL = strings.TrimSpace(c.PostForm("newcomment_author_url"))
	cm.Content = c.PostForm("content")
	if !u.Can("unfiltered_html", nil) {
		cm.Content = wp.KsesPost(cm.Content)
	}
	switch c.PostForm("status") {
	case "1", "approve", "approved":
		cm.Status = "approved"
	case "0", "hold", "pending":
		cm.Status = "pending"
	case "spam":
		cm.Status = "spam"
	}
	if err := s.store.UpdateComment(ctx, cm); err != nil {
		c.String(http.StatusOK, err.Error())
		return
	}
	s.purgeTags("post:"+itoa64(cm.PostID), "feed", "widgets")
	cm, _ = s.store.GetComment(ctx, cm.ID)
	position := "-1"
	if n, _ := strconv.Atoi(c.PostForm("position")); n != 0 {
		position = strconv.Itoa(n)
	}
	sendAjaxResponse(c, ajaxResponse{What: "edit_comment", Action: "edit-comment", ID: cm.ID, Position: position, Data: s.commentRowHTML(c, cm)})
}

// dimCommentAjax is wp_ajax_dim_comment(): the Approve / Unapprove row actions through wp-lists.js.
func (s *Server) dimCommentAjax(c *gin.Context) {
	ctx := c.Request.Context()
	id, _ := strconv.ParseInt(c.PostForm("id"), 10, 64)
	cm, err := s.store.GetComment(ctx, id)
	if err != nil {
		sendAjaxResponse(c, ajaxResponse{What: "comment", Action: "dim-comment", ID: id, Error: fmt.Sprintf("Comment %d does not exist", id)})
		return
	}
	if !currentUser(c).Can("moderate_comments", nil) {
		c.String(http.StatusOK, "-1")
		return
	}
	current := map[string]string{"approved": "approved", "pending": "unapproved", "spam": "spam", "trash": "trash"}[cm.Status]
	if n := c.PostForm("new"); n != "" && n == current {
		c.String(http.StatusOK, strconv.FormatInt(time.Now().Unix(), 10))
		return
	}
	if !s.checkNonceValue(c, c.PostForm("_ajax_nonce"), "approve-comment_"+itoa64(id)) {
		c.String(http.StatusForbidden, "-1")
		return
	}
	action := "unapprove"
	if cm.Status == "pending" || cm.Status == "spam" {
		action = "approve"
	}
	if err := s.applyCommentAction(c, id, action); err != nil {
		sendAjaxResponse(c, ajaxResponse{What: "comment", Action: "dim-comment", ID: id, Error: err.Error()})
		return
	}
	s.deleteCommentResponse(c, id, "dim-comment")
}

// deleteCommentAjax is wp_ajax_delete_comment(): Spam, Trash, Restore, Not Spam and Delete
// Permanently row actions through wp-lists.js.
func (s *Server) deleteCommentAjax(c *gin.Context) {
	ctx := c.Request.Context()
	id, _ := strconv.ParseInt(c.PostForm("id"), 10, 64)
	cm, err := s.store.GetComment(ctx, id)
	if err != nil {
		c.String(http.StatusOK, strconv.FormatInt(time.Now().Unix(), 10))
		return
	}
	if !currentUser(c).Can("moderate_comments", nil) {
		c.String(http.StatusOK, "-1")
		return
	}
	if !s.checkNonceValue(c, c.PostForm("_ajax_nonce"), "delete-comment_"+itoa64(id)) {
		c.String(http.StatusForbidden, "-1")
		return
	}
	var action string
	switch {
	case c.PostForm("trash") == "1":
		if cm.Status == "trash" {
			c.String(http.StatusOK, strconv.FormatInt(time.Now().Unix(), 10))
			return
		}
		action = "trash"
	case c.PostForm("untrash") == "1":
		if cm.Status != "trash" {
			c.String(http.StatusOK, strconv.FormatInt(time.Now().Unix(), 10))
			return
		}
		action = "untrash"
	case c.PostForm("spam") == "1":
		if cm.Status == "spam" {
			c.String(http.StatusOK, strconv.FormatInt(time.Now().Unix(), 10))
			return
		}
		action = "spam"
	case c.PostForm("unspam") == "1":
		if cm.Status != "spam" {
			c.String(http.StatusOK, strconv.FormatInt(time.Now().Unix(), 10))
			return
		}
		action = "unspam"
	case c.PostForm("delete") == "1":
		action = "delete"
	default:
		c.String(http.StatusOK, "-1")
		return
	}
	if err := s.applyCommentAction(c, id, action); err != nil {
		c.String(http.StatusOK, "0")
		return
	}
	s.deleteCommentResponse(c, id, "delete-comment")
}

// deleteCommentResponse is _wp_ajax_delete_comment_response(): the status and counts the list
// needs after a row action.
func (s *Server) deleteCommentResponse(c *gin.Context, id int64, action string) {
	sup := [][2]string{{"status", ""}, {"postId", ""}, {"time", strconv.FormatInt(time.Now().Unix(), 10)}}
	if cm, err := s.store.GetComment(c, id); err == nil {
		sup[0][1] = map[string]string{"approved": "1", "pending": "0", "spam": "spam", "trash": "trash"}[cm.Status]
		sup[1][1] = itoa64(cm.PostID)
		if cm.Status == "approved" {
			if p, err := s.store.GetPost(c, cm.PostID); err == nil {
				sup = append(sup, [2]string{"comment_link", s.links(c).Permalink(p) + "#comment-" + itoa64(cm.ID)})
			}
		}
	}
	sendAjaxResponse(c, ajaxResponse{What: "comment", Action: action, ID: id, Supplemental: append(sup, s.commentCountSupplemental(c)...)})
}

func (s *Server) commentsBulk(c *gin.Context) {
	if !currentUser(c).Can("moderate_comments", nil) {
		c.String(http.StatusForbidden, "Sorry, you are not allowed to moderate comments.")
		return
	}
	if !s.checkNonce(c, "bulk-comments") {
		s.denyNonce(c)
		return
	}
	action := c.Query("action")
	if action == "" || action == "-1" {
		action = c.Query("action2")
	}
	n := 0
	for _, raw := range c.QueryArray("delete_comments[]") {
		id, _ := strconv.ParseInt(raw, 10, 64)
		if s.applyCommentAction(c, id, action) == nil {
			n++
		}
	}
	key := map[string]string{"approve": "approved", "unapprove": "unapproved", "spam": "spammed", "unspam": "unspammed",
		"trash": "trashed", "untrash": "untrashed", "delete": "deleted"}[action]
	c.Redirect(http.StatusFound, "/wp-admin/edit-comments?comment_status="+url.QueryEscape(c.DefaultQuery("comment_status", "all"))+"&"+key+"="+strconv.Itoa(n))
}

// applyCommentAction changes one comment and purges the cached pages that show it.
func (s *Server) applyCommentAction(c *gin.Context, id int64, action string) error {
	ctx := c.Request.Context()
	cm, err := s.store.GetComment(ctx, id)
	if err != nil {
		return err
	}
	switch action {
	case "approve":
		err = s.store.SetCommentStatus(ctx, id, "approved")
	case "unapprove":
		err = s.store.SetCommentStatus(ctx, id, "pending")
	case "spam":
		err = s.store.SetCommentStatus(ctx, id, "spam")
	case "trash":
		err = s.store.SetCommentStatus(ctx, id, "trash")
	case "unspam", "untrash":
		err = s.store.SetCommentStatus(ctx, id, "restore")
	case "delete":
		err = s.store.DeleteComment(ctx, id)
	default:
		return fmt.Errorf("unknown action")
	}
	if err == nil {
		s.purgeTags("post:"+itoa64(cm.PostID), "feed", "widgets")
	}
	return err
}

type commentEditData struct {
	C         *store.Comment
	Nonce     string
	PostTitle string
	PostURL   string
	DateLabel string
	DateInput string
	TimeInput string
	DeleteURL string
}

// commentScreen is comment.php: single-comment actions and the edit form.
func (s *Server) commentScreen(c *gin.Context) {
	u := currentUser(c)
	if !u.Can("moderate_comments", nil) {
		c.String(http.StatusForbidden, "Sorry, you are not allowed to edit comments on this post.")
		return
	}
	id, _ := strconv.ParseInt(defaultStr(c.Query("c"), c.PostForm("comment_ID")), 10, 64)
	cm, err := s.store.GetComment(c, id)
	if err != nil {
		c.String(http.StatusNotFound, "Invalid comment ID.")
		return
	}
	action := defaultStr(c.Query("action"), c.PostForm("action"))
	back := c.Query("_wp_http_referer")
	if back == "" || !strings.HasPrefix(back, "/wp-admin/") {
		back = "/wp-admin/edit-comments"
	}
	flag := func(key string) string {
		sep := "?"
		if strings.Contains(back, "?") {
			sep = "&"
		}
		return back + sep + key + "=1"
	}
	switch action {
	case "approvecomment", "unapprovecomment":
		if !s.checkNonce(c, "approve-comment_"+itoa64(id)) {
			s.denyNonce(c)
			return
		}
		a := strings.TrimSuffix(action, "comment")
		_ = s.applyCommentAction(c, id, a)
		c.Redirect(http.StatusFound, flag(map[string]string{"approve": "approved", "unapprove": "unapproved"}[a]))
	case "spamcomment", "unspamcomment", "trashcomment", "untrashcomment", "deletecomment":
		if !s.checkNonce(c, "delete-comment_"+itoa64(id)) {
			s.denyNonce(c)
			return
		}
		a := strings.TrimSuffix(action, "comment")
		_ = s.applyCommentAction(c, id, a)
		c.Redirect(http.StatusFound, flag(map[string]string{"spam": "spammed", "unspam": "unspammed", "trash": "trashed", "untrash": "untrashed", "delete": "deleted"}[a]))
	case "editedcomment":
		if !s.checkNonce(c, "update-comment_"+itoa64(id)) {
			s.denyNonce(c)
			return
		}
		cm.AuthorName = strings.TrimSpace(c.PostForm("newcomment_author"))
		cm.AuthorEmail = strings.TrimSpace(c.PostForm("newcomment_author_email"))
		cm.AuthorURL = strings.TrimSpace(c.PostForm("newcomment_author_url"))
		cm.Content = c.PostForm("content")
		if !u.Can("unfiltered_html", nil) {
			cm.Content = wp.KsesPost(cm.Content)
		}
		if st := c.PostForm("comment_status"); st == "approved" || st == "pending" || st == "spam" {
			cm.Status = st
		}
		if d, t := c.PostForm("comment_date"), c.PostForm("comment_time"); d != "" {
			if when, err := time.ParseInLocation("2006-01-02 15:04", d+" "+defaultStr(t, "00:00"), s.loc()); err == nil {
				cm.CreatedAt = when
			}
		}
		if err := s.store.UpdateComment(c, cm); err != nil {
			c.String(http.StatusInternalServerError, err.Error())
			return
		}
		s.purgeTags("post:"+itoa64(cm.PostID), "feed", "widgets")
		c.Redirect(http.StatusFound, "/wp-admin/edit-comments?updated=1")
	default: // editcomment
		d := commentEditData{C: cm, Nonce: s.nonce(c, "update-comment_"+itoa64(id)),
			DeleteURL: "/wp-admin/comment?c=" + itoa64(id) + "&action=trashcomment&_wpnonce=" + s.nonce(c, "delete-comment_"+itoa64(id))}
		if p, err := s.store.GetPost(c, cm.PostID); err == nil {
			d.PostTitle, d.PostURL = defaultStr(p.Title, "(no title)"), s.links(c).Permalink(p)
		}
		t := cm.CreatedAt.In(s.loc())
		d.DateLabel = "Submitted on: <b>" + wp.FormatDate(t, "M j, Y") + " at " + wp.FormatDate(t, "H:i") + "</b>"
		d.DateInput, d.TimeInput = t.Format("2006-01-02"), t.Format("15:04")
		s.adminPage(c, "edit-comments", "Edit Comment", "comment-php", "admin/comment-edit", d)
	}
}
