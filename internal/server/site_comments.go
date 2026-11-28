package server

import (
	"context"
	"crypto/hmac"
	"fmt"
	"html"
	"html/template"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"wpgo/internal/i18n"
	"wpgo/internal/links"
	"wpgo/internal/store"
	"wpgo/internal/wp"
)

type commentsView struct {
	Title        template.HTML
	List         template.HTML
	Count        int
	Open         bool
	ClosedNotice bool
	Form         template.HTML
	// Nav is the_comments_navigation() when comments are paged (page_comments); themes print it
	// before and after the list.
	Nav template.HTML
	// Page and MaxPage are cpage and get_comment_pages_count().
	Page, MaxPage int
}

// commentText is comment_text(): kses-filtered, texturized and auto-paragraphed. Comments from users
// without unfiltered_html are already limited to wp.KsesData when saved; the post policy here (with
// safecss) is the safety net for rows saved before that and for HTML written with unfiltered_html.
func commentText(content string) string {
	return wp.Autop(wp.Texturize(wp.RelUGC(wp.KsesPost(content)))) // pre_comment_content: wp_rel_ugc
}

// commentsOpen is comments_open() with _close_comments_for_old_post(): the close_comments_for_old_posts
// setting closes comments on posts older than close_comments_days_old days.
func (s *Server) commentsOpen(p *store.Post) bool {
	if p == nil || p.CommentStatus != "open" {
		return false
	}
	if !s.settings.Bool("close_comments_for_old_posts") || p.Type != "post" {
		return true
	}
	days := s.settings.Int("close_comments_days_old")
	if days <= 0 || p.PublishedAt == nil {
		return true
	}
	return time.Since(*p.PublishedAt) <= time.Duration(days)*24*time.Hour
}

// commentPaging is the comments_template() paging arithmetic: per page (0 when page_comments is off),
// the number of pages for topLevel threads and the page to show for a cpage of 0 (default_comments_page).
func (s *Server) commentPaging(topLevel, cpage int) (perPage, maxPage, page int) {
	if !s.settings.Bool("page_comments") {
		return 0, 1, 1
	}
	perPage = s.settings.Int("comments_per_page")
	if perPage < 1 {
		perPage = 50
	}
	maxPage = (topLevel + perPage - 1) / perPage
	if maxPage < 1 {
		maxPage = 1
	}
	page = cpage
	if page == 0 {
		page = 1
		if s.settings.String("default_comments_page") == "newest" {
			page = maxPage
		}
	}
	if page > maxPage {
		page = maxPage
	}
	return perPage, maxPage, page
}

// commentsView renders comments.php: the comment list and comment_form().
func (s *Server) commentsView(c *gin.Context, p *store.Post, lb *links.Builder, u *store.User) *commentsView {
	if s.passwordRequired(c, p) || s.gmemHidesComments(c.Request.Context(), u, p.ID) {
		return nil
	}
	cv := &commentsView{Open: s.commentsOpen(p)}
	cpage := 0
	if q := siteQueryFrom(c); q != nil {
		cpage = q.CPage
	} else {
		cpage, _ = strconv.Atoi(c.Query("cpage"))
	}
	pendingEmail, _ := c.Cookie("comment_author_email")
	if u != nil {
		pendingEmail = u.Email
	}
	if pendingEmail, err := url.QueryUnescape(pendingEmail); err == nil {
		// Comments are fetched oldest first; comment_order reverses the top level of the shown page (wp_list_comments reverse_top_level).
		comments, _ := s.store.ApprovedComments(c, p.ID, "asc", pendingEmail)
		comments = s.visiblePendingComments(c, comments, u)
		cv.Count = len(comments)
		depth := 1
		if s.settings.Bool("thread_comments") {
			depth = s.settings.Int("thread_comments_depth")
		}
		tree := store.CommentTree(comments, depth)
		perPage, maxPage, page := s.commentPaging(len(tree), cpage)
		cv.Page, cv.MaxPage = page, maxPage
		if perPage > 0 {
			start := (page - 1) * perPage
			end := start + perPage
			if start > len(tree) {
				start = len(tree)
			}
			if end > len(tree) {
				end = len(tree)
			}
			tree = tree[start:end]
		}
		if s.settings.String("comment_order") == "desc" {
			for i, j := 0, len(tree)-1; i < j; i, j = i+1, j-1 {
				tree[i], tree[j] = tree[j], tree[i]
			}
		}
		if cv.Count > 0 {
			title := theTitle(p)
			if cv.Count == 1 {
				cv.Title = template.HTML("One thought on &ldquo;" + title + "&rdquo;")
			} else {
				cv.Title = template.HTML(formatNumber(cv.Count) + " thoughts on &ldquo;" + title + "&rdquo;")
			}
			var sb strings.Builder
			counter := 0
			listPage := 0
			if perPage > 0 {
				listPage = page
			}
			ctx := withViewMemo(c.Request.Context())
			emails := make([]string, len(comments))
			for i, cm := range comments {
				emails[i] = cm.AuthorEmail
			}
			s.prefetchAvatars(ctx, emails)
			s.listComments(ctx, &sb, tree, p, lb, u, depth, &counter, listPage, s.pllUCOn(c))
			cv.List = template.HTML(sb.String())
			cv.Nav = s.commentsNavigation(p, lb, page, maxPage)
		}
	}
	cv.ClosedNotice = !cv.Open && cv.Count > 0
	if cv.Open {
		cv.Form = s.commentForm(c, p, lb, u)
	}
	return cv
}

// commentsNavigation is the_comments_navigation(): Older/Newer comments links when page_comments is on.
func (s *Server) commentsNavigation(p *store.Post, lb *links.Builder, page, maxPage int) template.HTML {
	if maxPage < 2 || !s.settings.Bool("page_comments") {
		return ""
	}
	newest := s.settings.String("default_comments_page") == "newest"
	var nav string
	if page > 1 {
		nav += `<div class="nav-previous"><a href="` + html.EscapeString(lb.CommentPageLink(p, page-1, maxPage, newest)+"#comments") + `" >Older comments</a></div>`
	}
	if page < maxPage {
		nav += `<div class="nav-next"><a href="` + html.EscapeString(lb.CommentPageLink(p, page+1, maxPage, newest)+"#comments") + `" >Newer comments</a></div>`
	}
	if nav == "" {
		return ""
	}
	return template.HTML(`
	<nav class="navigation comments-navigation" aria-label="Comments">
		<h2 class="screen-reader-text">Comments navigation</h2>
		<div class="nav-links">` + nav + `</div>
	</nav>`)
}

// commentLink is get_comment_link(): with paged comments the link carries the comment page (comment-page-N).
func (s *Server) commentLink(p *store.Post, lb *links.Builder, cpage, maxPage int, id int64) string {
	link := lb.Permalink(p)
	if s.settings.Bool("page_comments") && cpage > 0 {
		// Only with "oldest" as the default page is page 1 the bare permalink; with "newest" the bare
		// permalink shows the last page, so every page (1 included) needs its comment-page-N.
		link = lb.CommentPageLink(p, cpage, 0, s.settings.String("default_comments_page") == "newest")
	}
	return link + "#comment-" + itoa64(id)
}

// pageOfComment is get_page_of_comment(): the comment page a (top-level or nested) comment shows on.
func (s *Server) pageOfComment(ctx context.Context, cm *store.Comment) int {
	if !s.settings.Bool("page_comments") {
		return 0
	}
	perPage := s.settings.Int("comments_per_page")
	if perPage < 1 {
		perPage = 50
	}
	comments, err := s.store.ApprovedComments(ctx, cm.PostID, "asc", "")
	if err != nil {
		return 1
	}
	depth := 1
	if s.settings.Bool("thread_comments") {
		depth = s.settings.Int("thread_comments_depth")
	}
	// Replies live on their top-level ancestor's page.
	byID := map[int64]*store.Comment{}
	for _, c := range comments {
		byID[c.ID] = c
	}
	root := cm
	if depth > 1 {
		for root.ParentID != 0 {
			parent, ok := byID[root.ParentID]
			if !ok {
				break
			}
			root = parent
		}
	}
	tree := store.CommentTree(comments, depth)
	for i, t := range tree {
		if t.ID == root.ID {
			return i/perPage + 1
		}
	}
	// A comment that is not in the approved list yet (pending) comes after every older top-level one.
	return (len(tree) + 1 + perPage - 1) / perPage
}

// listComments is wp_list_comments() with the html5 comment markup (avatar size 42).
// cpage is the comment page being shown (0 when comments are not paged).
// uc marks each comment body as user content (Pursae Multi Language, golylang_uc.go).
func (s *Server) listComments(ctx context.Context, sb *strings.Builder, list []*store.Comment, p *store.Post, lb *links.Builder, u *store.User, maxDepth int, counter *int, cpage int, uc bool) {
	loc := s.loc()
	permalink := lb.Permalink(p)
	for _, cm := range list {
		parity := "even"
		if *counter%2 == 1 {
			parity = "odd"
		}
		*counter++
		classes := []string{"comment", parity}
		if cm.Depth == 1 {
			classes = append(classes, "thread-"+parity)
		}
		classes = append(classes, "depth-"+strconv.Itoa(cm.Depth))
		if len(cm.Children) > 0 {
			classes = append(classes, "parent")
		}
		if cm.UserID != 0 && cm.UserID == p.AuthorID {
			classes = append(classes, "byuser", "bypostauthor")
		} else if cm.UserID != 0 {
			classes = append(classes, "byuser")
		}
		name := cm.AuthorName
		if name == "" {
			name = "Anonymous"
		}
		author := html.EscapeString(name)
		if uc { // commenter names are user content (Pursae Multi Language)
			author = i18n.UCOpen + author + i18n.UCClose
		}
		// Stored URLs are re-checked here: rows written before the check, or by other paths, must not become script links.
		if link := commentAuthorURL(cm.AuthorURL); link != "" {
			author = fmt.Sprintf(`<a href="%s" class="url" rel="ugc external nofollow">%s</a>`, html.EscapeString(link), author)
		}
		t := cm.CreatedAt.In(loc)
		fmt.Fprintf(sb, `<li id="comment-%d" class="%s">
				<article id="div-comment-%d" class="comment-body">
					<footer class="comment-meta">
						<div class="comment-author vcard">
							%s						<b class="fn">%s</b> <span class="says">says:</span>					</div><!-- .comment-author -->

						<div class="comment-metadata">
							<a href="%s#comment-%d"><time datetime="%s">%s at %s</time></a>`,
			cm.ID, strings.Join(classes, " "), cm.ID, s.avatarImg(ctx, cm.AuthorEmail, 42), author,
			html.EscapeString(strings.TrimSuffix(s.commentLink(p, lb, cpage, 0, cm.ID), "#comment-"+itoa64(cm.ID))), cm.ID, t.Format(time.RFC3339), wp.FormatDate(t, s.settings.String("date_format")), wp.FormatDate(t, s.settings.String("time_format")))
		if u.Can("moderate_comments", nil) {
			fmt.Fprintf(sb, ` <span class="edit-link"><a class="comment-edit-link" href="/wp-admin/comment?action=editcomment&amp;c=%d">Edit</a></span>`, cm.ID)
		}
		sb.WriteString("						</div><!-- .comment-metadata -->\n\n")
		if cm.Status == "pending" {
			sb.WriteString("											<em class=\"comment-awaiting-moderation\">Your comment is awaiting moderation.</em>\n")
		}
		sb.WriteString("									</footer><!-- .comment-meta -->\n\n				<div class=\"comment-content\">\n")
		if uc {
			sb.WriteString(i18n.UCOpen + s.convertSmilies(lb, commentText(cm.Content)) + i18n.UCClose)
		} else {
			sb.WriteString(s.convertSmilies(lb, commentText(cm.Content)))
		}
		sb.WriteString("				</div><!-- .comment-content -->\n\n")
		if s.commentsOpen(p) && cm.Status == "approved" && maxDepth > 1 && cm.Depth < maxDepth {
			replyURL := permalink + previewSep(permalink) + "replytocom=" + itoa64(cm.ID) + "#respond"
			fmt.Fprintf(sb, `<div class="reply"><a rel="nofollow" class="comment-reply-link" href="%s" data-commentid="%d" data-postid="%d" data-belowelement="div-comment-%d" data-respondelement="respond" data-replyto="Reply to %s" aria-label="Reply to %s">Reply</a></div>`,
				html.EscapeString(replyURL), cm.ID, p.ID, cm.ID, html.EscapeString(name), html.EscapeString(name))
		}
		sb.WriteString("			</article><!-- .comment-body -->\n")
		if len(cm.Children) > 0 {
			sb.WriteString("<ol class=\"children\">\n")
			s.listComments(ctx, sb, cm.Children, p, lb, u, maxDepth, counter, cpage, uc)
			sb.WriteString("</ol><!-- .children -->\n")
		}
		sb.WriteString("</li><!-- #comment-## -->\n")
	}
}

// commentForm is comment_form() with html5 markup and an h2 reply title.
func (s *Server) commentForm(c *gin.Context, p *store.Post, lb *links.Builder, u *store.User) template.HTML {
	esc := html.EscapeString
	permalink := lb.Permalink(p)
	replyTo, _ := strconv.ParseInt(c.Query("replytocom"), 10, 64)
	var sb strings.Builder
	sb.WriteString(`<div id="respond" class="comment-respond">` + "\n")
	title := s.frontE(c, "Leave a Reply")
	if replyTo > 0 {
		// _get_comment_reply_id(): only an approved comment of this post can be replied to; a pending, spam
		// or trashed one must not put its author's name on the page.
		if parent, err := s.store.GetComment(c, replyTo); err == nil && parent.PostID == p.ID && parent.Status == "approved" {
			title = s.frontEf(c, "Reply to %s", `<a href="#comment-`+itoa64(parent.ID)+`">`+esc(defaultStr(parent.AuthorName, "Anonymous"))+`</a>`)
		} else {
			replyTo = 0
		}
	}
	cancelStyle := ` style="display:none;"`
	if replyTo > 0 {
		cancelStyle = ""
	}
	fmt.Fprintf(&sb, `<h2 id="reply-title" class="comment-reply-title">%s <small><a rel="nofollow" id="cancel-comment-reply-link" href="%s#respond"%s>%s</a></small></h2>`, title, esc(permalink), cancelStyle, s.frontE(c, "Cancel reply"))

	if s.settings.Bool("comment_registration") && u == nil {
		// __() with markup, like comment_form(): the link is the translation's own.
		sb.WriteString(`<p class="must-log-in">` + i18n.Sprintf(s.frontT(c, `You must be <a href="%s">logged in</a> to post a comment.`), esc(s.loginPath()+"?redirect_to="+url.QueryEscape(permalink))) + `</p>`)
		sb.WriteString("\t</div><!-- #respond -->\n")
		return template.HTML(sb.String())
	}
	sb.WriteString(`<form action="/wp-comments-post" method="post" id="commentform" class="comment-form">`)
	required := s.settings.Bool("require_name_email")
	req := func(on bool) string {
		if on {
			return ` <span class="required">*</span>`
		}
		return ""
	}
	requiredMsg := `<span class="required-field-message">` + s.frontEf(c, "Required fields are marked %s", `<span class="required">*</span>`) + `</span>`
	if u != nil {
		fmt.Fprintf(&sb, `<p class="logged-in-as"><a href="/wp-admin/profile" aria-label="%s">%s</a>. <a href="%s?action=logout&amp;_wpnonce=%s">%s</a> %s</p>`,
			s.frontEf(c, "Logged in as %s. Edit your profile.", esc(u.DisplayName)), s.frontEf(c, "Logged in as %s", esc(u.DisplayName)),
			s.loginPath(), s.nonce(c, "log-out"), s.frontE(c, "Log out?"), requiredMsg)
	} else {
		sb.WriteString(`<p class="comment-notes"><span id="email-notes">` + s.frontE(c, "Your email address will not be published.") + `</span> ` + requiredMsg + `</p>`)
	}
	sb.WriteString(`<p class="comment-form-comment"><label for="comment">` + s.frontE(c, "Comment") + ` <span class="required">*</span></label> <textarea id="comment" name="comment" cols="45" rows="8" maxlength="65525" required></textarea></p>`)
	if u == nil {
		author, _ := c.Cookie("comment_author")
		email, _ := c.Cookie("comment_author_email")
		site, _ := c.Cookie("comment_author_url")
		author, _ = url.QueryUnescape(author)
		email, _ = url.QueryUnescape(email)
		site, _ = url.QueryUnescape(site)
		reqAttr := ""
		if required {
			reqAttr = " required"
		}
		fmt.Fprintf(&sb, `<p class="comment-form-author"><label for="author">%s%s</label> <input id="author" name="author" type="text" value="%s" size="30" maxlength="245" autocomplete="name"%s /></p>`, s.frontE(c, "Name"), req(required), esc(author), reqAttr)
		fmt.Fprintf(&sb, "\n"+`<p class="comment-form-email"><label for="email">%s%s</label> <input id="email" name="email" type="email" value="%s" size="30" maxlength="100" aria-describedby="email-notes" autocomplete="email"%s /></p>`, s.frontE(c, "Email"), req(required), esc(email), reqAttr)
		fmt.Fprintf(&sb, "\n"+`<p class="comment-form-url"><label for="url">%s</label> <input id="url" name="url" type="url" value="%s" size="30" maxlength="200" autocomplete="url" /></p>`, s.frontE(c, "Website"), esc(site))
		if s.settings.Bool("show_comments_cookies_opt_in") {
			checked := ""
			if email != "" {
				checked = ` checked="checked"`
			}
			fmt.Fprintf(&sb, "\n"+`<p class="comment-form-cookies-consent"><input id="wp-comment-cookies-consent" name="wp-comment-cookies-consent" type="checkbox" value="yes"%s /> <label for="wp-comment-cookies-consent">%s</label></p>`, checked, s.frontE(c, "Save my name, email, and website in this browser for the next time I comment."))
		}
	}
	if u == nil {
		// Spam traps: a field humans never see (bots fill every input) and a signed timestamp (bots post at once).
		sb.WriteString("\n" + `<p class="wpgo-hp" style="display:none !important" aria-hidden="true"><label for="wpgo_hp">Leave this field empty</label><input type="text" name="` + commentHoneypot + `" id="wpgo_hp" value="" tabindex="-1" autocomplete="off" /></p>`)
		sb.WriteString(`<input type="hidden" name="` + commentTimeField + `" value="` + s.commentTimeToken(p.ID, time.Now()) + `" />`)
	}
	if u.Can("unfiltered_html", nil) {
		// wp_comment_form_unfiltered_html_nonce(): only a comment sent from this form keeps raw HTML.
		sb.WriteString(`<input type="hidden" id="_wp_unfiltered_html_comment" name="_wp_unfiltered_html_comment" value="` + s.nonce(c, commentUnfilteredAction(p.ID)) + `" />`)
	}
	fmt.Fprintf(&sb, "\n"+`<p class="form-submit"><input name="submit" type="submit" id="submit" class="submit" value="%s" /> <input type='hidden' name='comment_post_ID' value='%d' id='comment_post_ID' />
<input type='hidden' name='comment_parent' id='comment_parent' value='%d' />
</p></form>	</div><!-- #respond -->
	`, s.frontE(c, "Post Comment"), p.ID, replyTo)
	return template.HTML(sb.String())
}

// commentUnfilteredAction is the nonce action of the comment form's _wp_unfiltered_html_comment field.
func commentUnfilteredAction(postID int64) string {
	return "unfiltered-html-comment_" + strconv.FormatInt(postID, 10)
}

// linkRe finds the links of a comment or forum post for the comment_max_links rule. Every anchor with
// an href counts once, whatever its URL (relative, mailto:, www.…, as WordPress counts "<a "), and its
// quoted href value is consumed whole so the URL inside it is not counted again; a bare http(s):// URL
// counts too. The old pattern matched both "<a " and the URL of every anchor, holding a comment with
// one link when the limit was 2.
var linkRe = regexp.MustCompile(`(?i)<a\s[^>]*?href\s*=\s*(?:"[^"]*"?|'[^']*'?|[^\s>]*)|https?://`)

// commentLinkCount is the number of links in s (see linkRe).
func commentLinkCount(s string) int { return len(linkRe.FindAllString(s, -1)) }

// commentPost is wp-comments-post.php: validation, moderation rules and the redirect back to the post.
// userURL is the pre_user_url filter (esc_url_raw) for a user's profile URL. The profile URL is copied
// into the user's comments and linked from the front end, so it follows the comment author rule: an
// http(s) URL with a host, or "".
func userURL(raw string) string { return commentAuthorURL(raw) }

// commentAuthorURL is esc_url_raw() for a comment author's URL, which is shown as a link: only http and
// https URLs with a host are kept. Like the comment form, a value without a scheme gets "http://";
// "javascript:alert(1)" becomes "http://javascript:alert(1)", which does not parse, so it is dropped.
// Control characters (browsers delete tab and newline from a URL) and every other scheme give "".
func commentAuthorURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || hasControlChar(raw) {
		return ""
	}
	if l := strings.ToLower(raw); !strings.HasPrefix(l, "http://") && !strings.HasPrefix(l, "https://") {
		// "example.com:8080/x" is a host and port; "javascript:x", "mailto:a@b" and "ftp://h" carry another scheme.
		if i := strings.Index(raw, ":"); i > 0 && schemeLike(raw[:i]) && (strings.HasPrefix(raw[i+1:], "//") || !startsWithPort(raw[i+1:])) {
			return ""
		}
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || strings.ContainsAny(u.Host, " ") {
		return ""
	}
	return raw
}

// schemeLike reports whether s has the shape of a URL scheme (letters, digits, + - .; starting with a letter).
func schemeLike(s string) bool {
	for i, r := range s {
		letter := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z'
		if i == 0 && !letter || !letter && (r < '0' || r > '9') && r != '+' && r != '-' && r != '.' {
			return false
		}
	}
	return s != ""
}

// startsWithPort reports whether s begins with digits that end the authority (end, "/", "?" or "#").
func startsWithPort(s string) bool {
	n := 0
	for n < len(s) && s[n] >= '0' && s[n] <= '9' {
		n++
	}
	return n > 0 && (n == len(s) || s[n] == '/' || s[n] == '?' || s[n] == '#')
}

// commentFlooded is wp_check_comment_flood(): fifteen seconds between comments from the same visitor
// (IP, e-mail or user), unless they moderate. Used by the comment form and REST.
func (s *Server) commentFlooded(ctx context.Context, cm *store.Comment, u *store.User) bool {
	if u.Can("manage_options", nil) || u.Can("moderate_comments", nil) {
		return false
	}
	var uid int64
	if u != nil {
		uid = u.ID
	}
	last, ok, _ := s.store.LastCommentAt(ctx, cm.AuthorIP, cm.AuthorEmail, uid)
	return ok && time.Since(last) < commentFloodInterval
}

func (s *Server) commentPost(c *gin.Context) {
	ctx := c.Request.Context()
	u := currentUser(c)
	// The form has no nonce (as in WordPress): a signed-in visitor's cookie must not post for them from
	// another site. Anonymous comments carry no identity, so they are left as they are.
	if u != nil && s.crossSitePost(c) {
		s.commentDie(c, http.StatusForbidden, "Sorry, you are not allowed to post a comment from another site.")
		return
	}
	postID, _ := strconv.ParseInt(c.PostForm("comment_post_ID"), 10, 64)
	p, err := s.store.GetPost(ctx, postID)
	if err != nil || (p.Status != "publish" && p.Status != "private") || (p.Status == "private" && !u.Can("read_post", p)) {
		// A private post answers like a missing one to visitors who cannot read it.
		s.commentDie(c, http.StatusNotFound, "Sorry, comments are closed for this item.")
		return
	}
	if !s.commentsOpen(p) || (!u.Can("edit_post", p) && s.gmemHidesPost(ctx, u, p.ID)) { // Memberships: closed to non-members
		s.commentDie(c, http.StatusForbidden, "Sorry, comments are closed for this item.")
		return
	}
	if p.Password != "" && s.passwordRequired(c, p) {
		s.commentDie(c, http.StatusForbidden, "Sorry, this post is password protected.")
		return
	}
	if s.settings.Bool("comment_registration") && u == nil {
		s.commentDie(c, http.StatusForbidden, "Sorry, you must be logged in to comment.")
		return
	}
	cm := &store.Comment{PostID: p.ID, Content: strings.TrimSpace(c.PostForm("comment")),
		AuthorIP: clientIP(c), UserAgent: c.Request.UserAgent()}
	cm.ParentID, _ = strconv.ParseInt(c.PostForm("comment_parent"), 10, 64)
	if cm.ParentID > 0 {
		if parent, err := s.store.GetComment(ctx, cm.ParentID); err != nil || parent.PostID != p.ID || parent.Status != "approved" {
			cm.ParentID = 0
		}
	}
	if u != nil {
		cm.UserID, cm.AuthorName, cm.AuthorEmail, cm.AuthorURL = u.ID, u.DisplayName, u.Email, commentAuthorURL(u.URL)
	} else {
		cm.AuthorName = strings.TrimSpace(c.PostForm("author"))
		cm.AuthorEmail = strings.TrimSpace(c.PostForm("email"))
		cm.AuthorURL = commentAuthorURL(c.PostForm("url"))
		if s.settings.Bool("require_name_email") {
			if cm.AuthorName == "" || cm.AuthorEmail == "" {
				s.commentDie(c, http.StatusBadRequest, "<strong>Error:</strong> Please fill the required fields.")
				return
			}
			if !validEmail(cm.AuthorEmail) {
				s.commentDie(c, http.StatusBadRequest, "<strong>Error:</strong> Please enter a valid email address.")
				return
			}
		}
	}
	if u == nil && !s.commentTrapsPassed(c, p.ID) {
		s.commentDie(c, http.StatusForbidden, "<strong>Error:</strong> Your comment looks like spam.")
		return
	}
	// preprocess_comment: a plugin may rewrite the author fields and the text before validation.
	if s.plugins != nil && s.plugins.hooks.HasFilter("preprocess_comment") {
		if m, ok := s.applyFilters(ctx, "preprocess_comment", commentHookArgs(cm)).(map[string]any); ok {
			str := func(k string, dst *string) {
				if v, ok := m[k].(string); ok {
					*dst = v
				}
			}
			str("author", &cm.AuthorName)
			str("email", &cm.AuthorEmail)
			str("url", &cm.AuthorURL)
			str("content", &cm.Content)
			cm.AuthorURL = commentAuthorURL(cm.AuthorURL)
			cm.Content = strings.TrimSpace(cm.Content)
		}
	}
	if cm.Content == "" {
		s.commentDie(c, http.StatusBadRequest, "<strong>Error:</strong> Please type your comment text.")
		return
	}
	if len(cm.Content) > 65525 {
		s.commentDie(c, http.StatusRequestEntityTooLarge, "<strong>Error:</strong> Your comment is too long.")
		return
	}
	// wp-comments-post.php keeps raw HTML (kses_remove_filters) only for an unfiltered_html user whose
	// form carried a valid _wp_unfiltered_html_comment nonce; any other post is filtered like everyone's.
	if !u.Can("unfiltered_html", nil) || !s.checkNonceValue(c, c.PostForm("_wp_unfiltered_html_comment"), commentUnfilteredAction(p.ID)) {
		cm.Content = wp.KsesData(cm.Content) // pre_comment_content: wp_filter_kses ($allowedtags)
	}
	if dup, _ := s.store.IsDuplicateComment(ctx, cm); dup {
		s.commentDie(c, http.StatusConflict, "Duplicate comment detected; it looks as though you&#8217;ve already said that!")
		return
	}
	if s.commentFlooded(ctx, cm, u) {
		s.commentDie(c, http.StatusTooManyRequests, "You are posting comments too quickly. Slow down.")
		return
	}
	cm.Status = s.commentStatus(c, cm, u)
	// A visitor form without the signed timestamp (a custom theme form, or a bot posting straight to
	// wp-comments-post) is not refused, but it never skips moderation.
	if u == nil && c.PostForm(commentTimeField) == "" && cm.Status == "approved" {
		cm.Status = "pending"
	}
	// pre_comment_approved: a plugin (an antispam service) may turn the verdict into approved, hold, spam or trash.
	if s.plugins != nil && s.plugins.hooks.HasFilter("pre_comment_approved") {
		if v, ok := s.applyFilters(ctx, "pre_comment_approved", cm.Status, commentHookArgs(cm)).(string); ok {
			switch v {
			case "approved", "pending", "spam", "trash":
				cm.Status = v
			case "1":
				cm.Status = "approved"
			case "0":
				cm.Status = "pending"
			}
		}
	}
	if err := s.store.InsertComment(ctx, cm); err != nil {
		s.commentDie(c, http.StatusInternalServerError, "Could not save the comment.")
		return
	}
	if s.plugins != nil {
		s.plugins.hooks.DoAction(ctx, "comment_post", cm.ID, cm.Status, commentHookArgs(cm))
	}
	if cm.Status == "approved" {
		s.purgeTags("post:"+itoa64(cm.PostID), "feed", "widgets")
	}
	s.notifyComment(c, cm)

	if u == nil {
		maxAge := -1
		if c.PostForm("wp-comment-cookies-consent") == "yes" {
			maxAge = 30000000
		}
		for k, v := range map[string]string{"comment_author": cm.AuthorName, "comment_author_email": cm.AuthorEmail, "comment_author_url": cm.AuthorURL} {
			http.SetCookie(c.Writer, &http.Cookie{Name: k, Value: url.QueryEscape(v), Path: "/", MaxAge: maxAge, SameSite: http.SameSiteLaxMode})
		}
		// Without consent, keep the email for this browser session so the author still sees the pending comment.
		if maxAge < 0 && cm.Status == "pending" {
			http.SetCookie(c.Writer, &http.Cookie{Name: "comment_author_email", Value: url.QueryEscape(cm.AuthorEmail), Path: "/", SameSite: http.SameSiteLaxMode})
		}
		if cm.Status == "pending" {
			s.rememberPendingComment(c, cm.ID, maxAge)
		}
	}
	lb := s.links(ctx)
	c.Redirect(http.StatusFound, s.commentLink(p, lb, s.pageOfComment(ctx, cm), 0, cm.ID))
}

// commentFloodInterval is wp_throttle_comment_flood()'s limit.
const commentFloodInterval = 15 * time.Second

// commentStatus is wp_allow_comment(): disallowed keys go to the trash, moderation rules hold, everything else is approved.
func (s *Server) commentStatus(c *gin.Context, cm *store.Comment, u *store.User) string {
	if u.Can("moderate_comments", nil) {
		return "approved"
	}
	// "The author and the admins get respect": the post's own author is not moderated on it.
	if u != nil && u.ID != 0 {
		if p, err := s.store.GetPost(c.Request.Context(), cm.PostID); err == nil && p.AuthorID == u.ID {
			return "approved"
		}
	}
	haystack := strings.ToLower(strings.Join([]string{cm.AuthorName, cm.AuthorEmail, cm.AuthorURL, cm.Content, cm.AuthorIP, cm.UserAgent}, "\n"))
	matches := func(keys string) bool {
		for _, k := range strings.Split(keys, "\n") {
			k = strings.ToLower(strings.TrimSpace(k))
			if k != "" && strings.Contains(haystack, k) {
				return true
			}
		}
		return false
	}
	if matches(s.settings.String("disallowed_keys")) {
		return "trash"
	}
	if s.settings.Bool("comment_moderation") {
		return "pending"
	}
	if max := s.settings.Int("comment_max_links"); max > 0 && commentLinkCount(cm.Content) >= max {
		return "pending"
	}
	if matches(s.settings.String("moderation_keys")) {
		return "pending"
	}
	if s.settings.Bool("comment_previously_approved") {
		if ok, _ := s.store.HasApprovedComment(c, cm.AuthorName, cm.AuthorEmail); !ok {
			return "pending"
		}
	}
	return "approved"
}

func (s *Server) commentDie(c *gin.Context, status int, msg string) {
	c.Header("Content-Type", "text/html; charset=utf-8")
	c.String(status, `<!DOCTYPE html><html lang="en-US"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>Comment Submission Failure</title>`+
		`<link rel="stylesheet" href="/wp-admin/assets/css/wp/install.min.css"></head><body id="error-page"><div class="wp-die-message"><p>`+msg+
		`</p></div><p>`+historyBackLink("&laquo; Back")+`</p></body></html>`)
}

// Comment spam traps. The honeypot input is hidden with CSS; the time field is "<unix>.<hmac>" signed
// for the post, so it cannot be forged and carries no expiry (a cached page keeps working).
const (
	commentHoneypot  = "wpgo_website"
	commentTimeField = "wpgo_ct"
	commentMinAge    = 2 * time.Second
)

func (s *Server) commentTimeToken(postID int64, at time.Time) string {
	ts := strconv.FormatInt(at.Unix(), 10)
	return ts + "." + s.asHash("comment-time", ts+"|"+itoa64(postID))
}

// commentTrapsPassed is false when the honeypot is filled, the timestamp is forged, or the form was
// submitted faster than a person can. A form without the timestamp (a custom theme form) passes, but
// commentPost holds it for moderation.
func (s *Server) commentTrapsPassed(c *gin.Context, postID int64) bool {
	if c.PostForm(commentHoneypot) != "" {
		return false
	}
	tok := c.PostForm(commentTimeField)
	if tok == "" {
		return true
	}
	ts, sig, ok := strings.Cut(tok, ".")
	n, err := strconv.ParseInt(ts, 10, 64)
	if !ok || err != nil || !hmac.Equal([]byte(sig), []byte(s.asHash("comment-time", ts+"|"+itoa64(postID)))) {
		return false
	}
	return time.Since(time.Unix(n, 0)) >= commentMinAge
}

// commentHookArgs is the comment as plugins see it.
func commentHookArgs(cm *store.Comment) map[string]any {
	return map[string]any{"post_id": cm.PostID, "parent": cm.ParentID, "user_id": cm.UserID, "author": cm.AuthorName,
		"email": cm.AuthorEmail, "url": cm.AuthorURL, "content": cm.Content, "ip": cm.AuthorIP, "user_agent": cm.UserAgent}
}
