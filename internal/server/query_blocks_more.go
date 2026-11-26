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

// The rest of the block theme set, rendered server-side: core/navigation (+ navigation-link,
// navigation-submenu, home-link), site-logo, post-author (+ name, biography), post-terms,
// post-navigation-link, post-comments-count/-link/-form and the comments family (comments,
// comments-title, comment-template, comment-author-name, comment-content, comment-date,
// comment-reply-link, comment-edit-link, comments-pagination + previous/next/numbers).
// Sources: wp-includes/blocks/navigation*.php, site-logo.php, post-author*.php, post-terms.php,
// post-navigation-link.php, comments*.php, comment-*.php.

type commentState struct {
	post          *store.Post
	tree          []*store.Comment
	count         int
	page, maxPage int
	depth         int
	cm            *store.Comment // the comment a comment-* block renders
}

type commentStateKey struct{}

func commentsFrom(ctx context.Context) *commentState {
	cs, _ := ctx.Value(commentStateKey{}).(*commentState)
	return cs
}

func registerMoreQueryBlocks(s *Server) {
	blocks.RegisterContainer("core/navigation", renderNavigationBlock)
	blocks.RegisterDynamic("core/navigation-link", func(ctx context.Context, b *blocks.Block, _ string) string {
		return navItem(ctx, b, "")
	})
	blocks.RegisterContainer("core/navigation-submenu", func(ctx context.Context, b *blocks.Block, _ string) string {
		return navItem(ctx, b, strings.TrimSpace(blocks.RenderInner(ctx, b)))
	})
	blocks.RegisterDynamic("core/home-link", func(ctx context.Context, b *blocks.Block, _ string) string {
		rc := renderFrom(ctx)
		if rc == nil {
			return ""
		}
		label := attrStr(b, "label", "Home")
		cls, aria := "wp-block-navigation-item", ""
		if rc.q != nil && (rc.q.Kind == "home" || rc.q.Kind == "front-page") && rc.q.Paged < 2 {
			cls, aria = cls+" current-menu-item", ` aria-current="page"`
		}
		return `<li class="` + cls + ` wp-block-home-link"><a class="wp-block-home-link__content wp-block-navigation-item__content" href="` +
			html.EscapeString(rc.lb.Front()) + `" rel="home"` + aria + `>` + wp.KsesPost(label) + `</a></li>`
	})

	blocks.RegisterDynamic("core/site-logo", func(ctx context.Context, b *blocks.Block, _ string) string {
		rc := renderFrom(ctx)
		if rc == nil {
			return ""
		}
		// The site logo is the active theme's custom_logo mod, else the site_logo setting (REST /settings).
		id := int64(0)
		if t := rc.s.activeTheme(); t != nil {
			id = int64(modInt(rc.s.themeMods(t.Slug), "custom_logo", 0))
		}
		if id == 0 {
			id = int64(rc.s.settings.Int("site_logo"))
		}
		if id == 0 {
			return ""
		}
		a, err := rc.s.store.GetAttachment(ctx, id)
		if err != nil {
			return ""
		}
		img := *a
		if img.Alt == "" {
			img.Alt = rc.s.settings.String("blogname")
		}
		tag := string(rc.s.attachmentImage(ctx, &img, "full", "custom-logo"))
		if w := attrInt(b, "width", 0); w > 0 && a.Width > 0 {
			h := a.Height * w / a.Width
			tag = strings.Replace(tag, fmt.Sprintf(`width="%d" height="%d"`, a.Width, a.Height), fmt.Sprintf(`width="%d" height="%d"`, w, h), 1)
		}
		if attrBool(b, "isLink", true) {
			target := ""
			if attrStr(b, "linkTarget", "_self") == "_blank" {
				target = ` target="_blank"`
			}
			tag = `<a href="` + html.EscapeString(rc.lb.Front()) + `" class="custom-logo-link" rel="home"` + target + `>` + tag + `</a>`
		}
		return `<div ` + wrapperClass(b, "") + `>` + tag + `</div>`
	})

	blocks.RegisterDynamic("core/post-author", func(ctx context.Context, b *blocks.Block, _ string) string {
		rc, u := loopAuthor(ctx)
		if u == nil {
			return ""
		}
		var sb strings.Builder
		sb.WriteString(`<div ` + wrapperClass(b, "") + `>`)
		if attrBool(b, "showAvatar", true) {
			size := attrInt(b, "avatarSize", 48)
			fmt.Fprintf(&sb, `<div class="wp-block-post-author__avatar"><img alt='' src='%s' class='avatar avatar-%d photo' height='%d' width='%d' loading='lazy' decoding='async'/></div>`,
				html.EscapeString(rc.s.avatarFor(ctx, u.Email, size)), size, size, size)
		}
		sb.WriteString(`<div class="wp-block-post-author__content">`)
		if by := attrStr(b, "byline", ""); by != "" {
			sb.WriteString(`<p class="wp-block-post-author__byline">` + wp.KsesPost(by) + `</p>`)
		}
		name := html.EscapeString(u.DisplayName)
		if attrBool(b, "isLink", false) {
			name = `<a href="` + html.EscapeString(rc.lb.AuthorLink(u.ID, u.Nicename)) + `" target="` + html.EscapeString(attrStr(b, "linkTarget", "_self")) + `">` + name + `</a>`
		}
		sb.WriteString(`<p class="wp-block-post-author__name">` + name + `</p>`)
		if attrBool(b, "showBio", false) && u.Profile["description"] != "" {
			sb.WriteString(`<p class="wp-block-post-author__bio">` + wp.KsesData(u.Profile["description"]) + `</p>`)
		}
		sb.WriteString(`</div></div>`)
		return sb.String()
	})
	blocks.RegisterDynamic("core/post-author-name", func(ctx context.Context, b *blocks.Block, _ string) string {
		rc, u := loopAuthor(ctx)
		if u == nil {
			return ""
		}
		name := html.EscapeString(u.DisplayName)
		if attrBool(b, "isLink", false) {
			name = `<a href="` + html.EscapeString(rc.lb.AuthorLink(u.ID, u.Nicename)) + `" target="` + html.EscapeString(attrStr(b, "linkTarget", "_self")) + `" class="wp-block-post-author-name__link">` + name + `</a>`
		}
		return `<div ` + wrapperClass(b, "") + `>` + name + `</div>`
	})
	blocks.RegisterDynamic("core/post-author-biography", func(ctx context.Context, b *blocks.Block, _ string) string {
		_, u := loopAuthor(ctx)
		if u == nil || u.Profile["description"] == "" {
			return ""
		}
		return `<div ` + wrapperClass(b, "") + `>` + wp.KsesData(u.Profile["description"]) + `</div>`
	})

	blocks.RegisterDynamic("core/post-terms", func(ctx context.Context, b *blocks.Block, _ string) string {
		rc, p := loopPost(ctx)
		tax := attrStr(b, "term", "")
		if p == nil || tax == "" {
			return ""
		}
		byPost, err := rc.s.store.TermsForPosts(ctx, []int64{p.ID}, tax)
		if err != nil || len(byPost[p.ID]) == 0 {
			return ""
		}
		sep := attrStr(b, "separator", ", ")
		var links []string
		for _, t := range byPost[p.ID] {
			links = append(links, `<a href="`+html.EscapeString(rc.lb.TermLink(t))+`" rel="tag">`+html.EscapeString(t.Name)+`</a>`)
		}
		out := strings.Join(links, `<span class="wp-block-post-terms__separator">`+html.EscapeString(sep)+`</span>`)
		if pre := attrStr(b, "prefix", ""); pre != "" {
			out = `<span class="wp-block-post-terms__prefix">` + wp.KsesPost(pre) + `</span>` + out
		}
		if suf := attrStr(b, "suffix", ""); suf != "" {
			out += `<span class="wp-block-post-terms__suffix">` + wp.KsesPost(suf) + `</span>`
		}
		return `<div ` + wrapperClass(b, "taxonomy-"+tax) + `>` + out + `</div>`
	})

	blocks.RegisterDynamic("core/post-navigation-link", func(ctx context.Context, b *blocks.Block, _ string) string {
		rc, p := loopPost(ctx)
		if p == nil {
			return ""
		}
		prev := attrStr(b, "type", "next") == "previous"
		adj, err := rc.s.store.Adjacent(ctx, p, prev)
		if err != nil || adj == nil {
			return ""
		}
		label := attrStr(b, "label", "")
		if label == "" {
			label = "Next"
			if prev {
				label = "Previous"
			}
		}
		text := wp.KsesPost(label)
		if attrBool(b, "showTitle", false) {
			if attrBool(b, "linkLabel", false) {
				text += `: ` + html.EscapeString(adj.Title)
			} else {
				text = html.EscapeString(adj.Title)
				if attrStr(b, "label", "") != "" {
					text = wp.KsesPost(label) + `: ` + text
				}
			}
		}
		rel, kind := "next", "next"
		if prev {
			rel, kind = "prev", "previous"
		}
		return `<div ` + wrapperClass(b, "post-navigation-link-"+kind) + `><a href="` + html.EscapeString(rc.lb.Permalink(adj)) + `" rel="` + rel + `">` + text + `</a></div>`
	})

	// Comments -------------------------------------------------------------------------------------
	blocks.RegisterContainer("core/comments", func(ctx context.Context, b *blocks.Block, _ string) string {
		rc, p := loopPost(ctx)
		if p == nil || rc.c == nil || rc.s.passwordRequired(rc.c, p) || rc.s.gmemHidesComments(ctx, rc.user, p.ID) {
			return ""
		}
		// Legacy mode (and a block without inner blocks) prints the theme's comments template.
		if attrBool(b, "legacy", false) || len(b.InnerBlocks) == 0 {
			cv := rc.s.commentsView(rc.c, p, rc.lb, rc.user)
			if cv == nil || cv.Count == 0 && !cv.Open {
				return ""
			}
			var sb strings.Builder
			sb.WriteString(`<div ` + wrapperClass(b, "") + ` id="comments">`)
			if cv.Count > 0 {
				sb.WriteString(`<h2 class="comments-title">` + string(cv.Title) + `</h2><ol class="comment-list">` + string(cv.List) + `</ol>` + string(cv.Nav))
			}
			if cv.ClosedNotice {
				sb.WriteString(`<p class="no-comments">Comments are closed.</p>`)
			}
			sb.WriteString(string(cv.Form) + `</div>`)
			return sb.String()
		}
		cs := rc.s.commentBlockState(rc, p)
		if cs.count == 0 && !rc.s.commentsOpen(p) {
			return ""
		}
		return `<div ` + wrapperClass(b, "") + ` id="comments">` + blocks.RenderInner(context.WithValue(ctx, commentStateKey{}, cs), b) + `</div>`
	})
	blocks.RegisterDynamic("core/comments-title", func(ctx context.Context, b *blocks.Block, _ string) string {
		cs := commentsFrom(ctx)
		if cs == nil || cs.count == 0 {
			return ""
		}
		level := attrInt(b, "level", 2)
		if level < 1 || level > 6 {
			level = 2
		}
		showCount, showTitle := attrBool(b, "showCommentsCount", true), attrBool(b, "showPostTitle", true)
		title := `&#8220;` + wp.Texturize(wp.EscText(theTitle(cs.post))) + `&#8221;`
		var text string
		switch {
		case showCount && showTitle && cs.count == 1:
			text = "One response to " + title
		case showCount && showTitle:
			text = formatNumber(cs.count) + " responses to " + title
		case showCount && cs.count == 1:
			text = "One response"
		case showCount:
			text = formatNumber(cs.count) + " responses"
		case showTitle && cs.count == 1:
			text = "Response to " + title
		case showTitle:
			text = "Responses to " + title
		case cs.count == 1:
			text = "Response"
		default:
			text = "Responses"
		}
		return fmt.Sprintf(`<h%d id="comments" %s>%s</h%d>`, level, wrapperClass(b, ""), text, level)
	})
	blocks.RegisterContainer("core/comment-template", func(ctx context.Context, b *blocks.Block, _ string) string {
		cs := commentsFrom(ctx)
		if cs == nil || len(cs.tree) == 0 {
			return ""
		}
		var walk func(list []*store.Comment) string
		walk = func(list []*store.Comment) string {
			var sb strings.Builder
			for _, cm := range list {
				item := *cs
				item.cm = cm
				classes := "comment depth-" + strconv.Itoa(cm.Depth)
				if cm.UserID != 0 && cm.UserID == cs.post.AuthorID {
					classes += " byuser bypostauthor"
				} else if cm.UserID != 0 {
					classes += " byuser"
				}
				sb.WriteString(`<li id="comment-` + itoa64(cm.ID) + `" class="` + classes + `">` + blocks.RenderInner(context.WithValue(ctx, commentStateKey{}, &item), b))
				if len(cm.Children) > 0 {
					sb.WriteString(`<ol>` + walk(cm.Children) + `</ol>`)
				}
				sb.WriteString(`</li>`)
			}
			return sb.String()
		}
		return `<ol ` + wrapperClass(b, "") + `>` + walk(cs.tree) + `</ol>`
	})
	blocks.RegisterDynamic("core/comment-author-name", func(ctx context.Context, b *blocks.Block, _ string) string {
		cs := commentsFrom(ctx)
		if cs == nil || cs.cm == nil {
			return ""
		}
		name := html.EscapeString(defaultStr(cs.cm.AuthorName, "Anonymous"))
		if link := commentAuthorURL(cs.cm.AuthorURL); link != "" && attrBool(b, "isLink", true) {
			name = `<a rel="external nofollow ugc" href="` + html.EscapeString(link) + `" target="` + html.EscapeString(attrStr(b, "linkTarget", "_self")) + `">` + name + `</a>`
		}
		return `<div ` + wrapperClass(b, "") + `>` + name + `</div>`
	})
	blocks.RegisterDynamic("core/comment-content", func(ctx context.Context, b *blocks.Block, _ string) string {
		rc, cs := renderFrom(ctx), commentsFrom(ctx)
		if rc == nil || cs == nil || cs.cm == nil {
			return ""
		}
		out := rc.s.convertSmilies(rc.lb, commentText(cs.cm.Content))
		if cs.cm.Status == "pending" {
			out = `<p><em class="comment-awaiting-moderation">Your comment is awaiting moderation.</em></p>` + out
		}
		return `<div ` + wrapperClass(b, "") + `>` + out + `</div>`
	})
	blocks.RegisterDynamic("core/comment-date", func(ctx context.Context, b *blocks.Block, _ string) string {
		rc, cs := renderFrom(ctx), commentsFrom(ctx)
		if rc == nil || cs == nil || cs.cm == nil {
			return ""
		}
		t := cs.cm.CreatedAt.In(rc.s.loc())
		format := attrStr(b, "format", "")
		if format == "" {
			format = rc.s.settings.String("date_format")
		}
		out := `<time datetime="` + t.Format(time.RFC3339) + `">` + html.EscapeString(wp.FormatDate(t, format)) + `</time>`
		if attrBool(b, "isLink", true) {
			out = `<a href="` + html.EscapeString(rc.s.commentLink(cs.post, rc.lb, cs.page, 0, cs.cm.ID)) + `">` + out + `</a>`
		}
		return `<div ` + wrapperClass(b, "") + `>` + out + `</div>`
	})
	blocks.RegisterDynamic("core/comment-reply-link", func(ctx context.Context, b *blocks.Block, _ string) string {
		rc, cs := renderFrom(ctx), commentsFrom(ctx)
		if rc == nil || cs == nil || cs.cm == nil || !rc.s.commentsOpen(cs.post) || cs.cm.Status != "approved" || cs.depth <= 1 || cs.cm.Depth >= cs.depth {
			return ""
		}
		link := rc.lb.Permalink(cs.post)
		link += previewSep(link) + "replytocom=" + itoa64(cs.cm.ID) + "#respond"
		name := html.EscapeString(defaultStr(cs.cm.AuthorName, "Anonymous"))
		return fmt.Sprintf(`<div %s><a rel="nofollow" class="comment-reply-link" href="%s" data-commentid="%d" data-postid="%d" data-belowelement="comment-%d" data-respondelement="respond" data-replyto="Reply to %s" aria-label="Reply to %s">Reply</a></div>`,
			wrapperClass(b, ""), html.EscapeString(link), cs.cm.ID, cs.post.ID, cs.cm.ID, name, name)
	})
	blocks.RegisterDynamic("core/comment-edit-link", func(ctx context.Context, b *blocks.Block, _ string) string {
		rc, cs := renderFrom(ctx), commentsFrom(ctx)
		if rc == nil || cs == nil || cs.cm == nil || !rc.user.Can("moderate_comments", nil) {
			return ""
		}
		return fmt.Sprintf(`<div %s><a href="/wp-admin/comment?action=editcomment&amp;c=%d" target="%s">Edit</a></div>`,
			wrapperClass(b, ""), cs.cm.ID, html.EscapeString(attrStr(b, "linkTarget", "_self")))
	})
	blocks.RegisterDynamic("core/comments-pagination", func(ctx context.Context, b *blocks.Block, content string) string {
		if cs := commentsFrom(ctx); cs == nil || cs.maxPage < 2 || strings.TrimSpace(content) == "" {
			return ""
		}
		return `<div ` + wrapperClass(b, "") + `>` + content + `</div>`
	})
	blocks.RegisterDynamic("core/comments-pagination-previous", func(ctx context.Context, b *blocks.Block, _ string) string {
		rc, cs := renderFrom(ctx), commentsFrom(ctx)
		if rc == nil || cs == nil || cs.page <= 1 {
			return ""
		}
		return `<a href="` + html.EscapeString(rc.s.commentPageURL(rc, cs, cs.page-1)) + `" ` + wrapperClass(b, "") + `>` + html.EscapeString(attrStr(b, "label", "Older Comments")) + `</a>`
	})
	blocks.RegisterDynamic("core/comments-pagination-next", func(ctx context.Context, b *blocks.Block, _ string) string {
		rc, cs := renderFrom(ctx), commentsFrom(ctx)
		if rc == nil || cs == nil || cs.page >= cs.maxPage {
			return ""
		}
		return `<a href="` + html.EscapeString(rc.s.commentPageURL(rc, cs, cs.page+1)) + `" ` + wrapperClass(b, "") + `>` + html.EscapeString(attrStr(b, "label", "Newer Comments")) + `</a>`
	})
	blocks.RegisterDynamic("core/comments-pagination-numbers", func(ctx context.Context, b *blocks.Block, _ string) string {
		rc, cs := renderFrom(ctx), commentsFrom(ctx)
		if rc == nil || cs == nil || cs.maxPage < 2 {
			return ""
		}
		var sb strings.Builder
		for i := 1; i <= cs.maxPage; i++ {
			if i == cs.page {
				fmt.Fprintf(&sb, `<span aria-current="page" class="page-numbers current">%d</span>`, i)
			} else {
				fmt.Fprintf(&sb, `<a class="page-numbers" href="%s">%d</a>`, html.EscapeString(rc.s.commentPageURL(rc, cs, i)), i)
			}
		}
		return `<div ` + wrapperClass(b, "") + `>` + sb.String() + `</div>`
	})
	blocks.RegisterDynamic("core/post-comments-form", func(ctx context.Context, b *blocks.Block, _ string) string {
		rc, p := loopPost(ctx)
		if p == nil || rc.c == nil || !rc.s.commentsOpen(p) || rc.s.passwordRequired(rc.c, p) {
			return ""
		}
		form := strings.Replace(string(rc.s.commentForm(rc.c, p, rc.lb, rc.user)), `class="submit"`, `class="submit wp-block-button__link wp-element-button"`, 1)
		return `<div ` + wrapperClass(b, "comment-respond") + `>` + form + `</div>`
	})
	blocks.RegisterDynamic("core/post-comments-count", func(ctx context.Context, b *blocks.Block, _ string) string {
		rc, p := loopPost(ctx)
		if p == nil {
			return ""
		}
		return `<div ` + wrapperClass(b, "") + `>` + strconv.Itoa(rc.s.approvedCommentCount(ctx, p.ID)) + `</div>`
	})
	blocks.RegisterDynamic("core/post-comments-link", func(ctx context.Context, b *blocks.Block, _ string) string {
		rc, p := loopPost(ctx)
		if p == nil || !rc.s.commentsOpen(p) {
			return ""
		}
		n := rc.s.approvedCommentCount(ctx, p.ID)
		text := "No comments"
		if n == 1 {
			text = "1 comment"
		} else if n > 1 {
			text = formatNumber(n) + " comments"
		}
		return `<div ` + wrapperClass(b, "") + `><a href="` + html.EscapeString(rc.lb.Permalink(p)) + `#comments">` + text + `</a></div>`
	})
}

func (s *Server) approvedCommentCount(ctx context.Context, postID int64) int {
	list, _ := s.store.ApprovedComments(ctx, postID, "asc", "")
	return len(list)
}

// commentBlockState loads the comments of a post the way comments.php does (commentsView): threaded
// to thread_comments_depth, paged by comments_per_page, top level reversed for comment_order desc.
func (s *Server) commentBlockState(rc *renderCtx, p *store.Post) *commentState {
	cs := &commentState{post: p, depth: 1}
	pendingEmail := ""
	cpage := 0
	if rc.c != nil {
		if e, err := rc.c.Cookie("comment_author_email"); err == nil {
			pendingEmail = e
		}
		if rc.q != nil {
			cpage = rc.q.CPage
		} else {
			cpage, _ = strconv.Atoi(rc.c.Query("cpage"))
		}
	}
	if rc.user != nil {
		pendingEmail = rc.user.Email
	}
	ctx := context.Background()
	if rc.c != nil {
		ctx = rc.c.Request.Context()
	}
	comments, _ := s.store.ApprovedComments(ctx, p.ID, "asc", pendingEmail)
	cs.count = len(comments)
	if s.settings.Bool("thread_comments") {
		cs.depth = s.settings.Int("thread_comments_depth")
	}
	tree := store.CommentTree(comments, cs.depth)
	perPage, maxPage, page := s.commentPaging(len(tree), cpage)
	cs.page, cs.maxPage = page, maxPage
	if perPage > 0 {
		start, end := min((page-1)*perPage, len(tree)), min(page*perPage, len(tree))
		tree = tree[start:end]
	}
	if s.settings.String("comment_order") == "desc" {
		for i, j := 0, len(tree)-1; i < j; i, j = i+1, j-1 {
			tree[i], tree[j] = tree[j], tree[i]
		}
	}
	cs.tree = tree
	return cs
}

func (s *Server) commentPageURL(rc *renderCtx, cs *commentState, n int) string {
	return rc.lb.CommentPageLink(cs.post, n, cs.maxPage, s.settings.String("default_comments_page") == "newest") + "#comments"
}

// loopAuthor is the author a post-author* block renders: the loop post's author, else the queried author.
func loopAuthor(ctx context.Context) (*renderCtx, *store.User) {
	rc, p := loopPost(ctx)
	if rc == nil {
		return nil, nil
	}
	if p == nil {
		if rc.q != nil && rc.q.Author != nil {
			return rc, rc.q.Author
		}
		return rc, nil
	}
	u, err := rc.s.store.UserByID(ctx, p.AuthorID)
	if err != nil {
		return rc, nil
	}
	return rc, u
}

// navItem is core/navigation-link, or core/navigation-submenu when sub holds the rendered children.
func navItem(ctx context.Context, b *blocks.Block, sub string) string {
	rc := renderFrom(ctx)
	if rc == nil {
		return ""
	}
	label := attrStr(b, "label", "")
	link := attrStr(b, "url", "")
	// A link to a post or term follows its current permalink (the stored url goes stale on a slug change).
	if id, ok := b.Attrs["id"].(float64); ok && id > 0 {
		switch attrStr(b, "kind", "") {
		case "post-type":
			p, err := rc.s.store.GetPost(ctx, int64(id))
			if err != nil || p.Status != "publish" {
				return "" // WordPress drops links to unpublished posts
			}
			link = rc.lb.Permalink(p)
			if label == "" {
				label = html.EscapeString(p.Title)
			}
		case "taxonomy":
			if t, err := rc.s.store.GetTerm(ctx, int64(id)); err == nil {
				link = rc.lb.TermLink(t)
			}
		}
	}
	cls, aria := "wp-block-navigation-item", ""
	if rc.c != nil && link != "" && navSamePath(link, rc.c.Request.URL.Path) {
		cls, aria = cls+" current-menu-item", ` aria-current="page"`
	}
	if cn := attrStr(b, "className", ""); cn != "" {
		cls += " " + html.EscapeString(strings.Join(blocks.SafeClassTokens(cn), " "))
	}
	attrs := ` class="wp-block-navigation-item__content"`
	if link != "" {
		attrs += ` href="` + wp.EscURL(link) + `"`
	}
	if attrBool(b, "opensInNewTab", false) {
		attrs += ` target="_blank"`
	}
	if rel := attrStr(b, "rel", ""); rel != "" {
		attrs += ` rel="` + html.EscapeString(rel) + `"`
	}
	if title := attrStr(b, "title", ""); title != "" {
		attrs += ` title="` + html.EscapeString(title) + `"`
	}
	a := `<a` + attrs + aria + `><span class="wp-block-navigation-item__label">` + wp.KsesPost(label) + `</span></a>`
	if sub == "" && b.Name() != "core/navigation-submenu" {
		return `<li class="` + cls + ` wp-block-navigation-link">` + a + `</li>`
	}
	return `<li class="` + cls + ` has-child open-on-hover-click wp-block-navigation-submenu">` + a +
		`<button aria-label="` + html.EscapeString(wp.StripTags(label)) + ` submenu" class="wp-block-navigation__submenu-icon wp-block-navigation-submenu__toggle" aria-expanded="false">` + tt5SubmenuIcon +
		`</button><ul class="wp-block-navigation__submenu-container wp-block-navigation-submenu">` + sub + `</ul></li>`
}

func navSamePath(link, path string) bool {
	if i := strings.Index(link, "://"); i >= 0 {
		rest := link[i+3:]
		if j := strings.IndexByte(rest, '/'); j >= 0 {
			link = rest[j:]
		} else {
			link = "/"
		}
	}
	if i := strings.IndexAny(link, "?#"); i >= 0 {
		link = link[:i]
	}
	return strings.TrimSuffix(link, "/") == strings.TrimSuffix(path, "/")
}

// renderNavigationBlock is render_block_core_navigation(): the block's inner links, else the menu the
// "ref" names (a nav menu id here; there is no wp_navigation post type), else the menu assigned to the
// theme's first location, else the published top-level pages (the page-list fallback).
func renderNavigationBlock(ctx context.Context, b *blocks.Block, _ string) string {
	rc := renderFrom(ctx)
	if rc == nil {
		return ""
	}
	items := blocks.RenderInner(ctx, b)
	if strings.TrimSpace(items) == "" {
		var menu []*store.MenuItem
		if ref := attrInt(b, "ref", 0); ref > 0 {
			menu, _ = rc.s.store.MenuTree(ctx, int64(ref))
		}
		if len(menu) == 0 {
			if t := rc.s.activeTheme(); t != nil {
				for _, loc := range t.MenuLocations {
					if menu, _ = rc.s.store.MenuAt(ctx, loc.Slug); len(menu) > 0 {
						break
					}
				}
			}
		}
		if len(menu) == 0 {
			menu, _ = rc.s.store.MenuAt(ctx, "primary")
		}
		if len(menu) > 0 {
			rc.s.resolveMenu(ctx, menu, rc.lb, rc.q)
			items = tt5NavItems(menu)
		} else {
			items = tt5PageListItems(renderPageListBlock(ctx, pageListFallbackBlock(), ""))
		}
	}
	if strings.TrimSpace(items) == "" {
		return ""
	}
	classes := "wp-block-navigation"
	if j := attrStr(b, "itemsJustification", ""); j != "" {
		classes += " items-justified-" + html.EscapeString(j)
	}
	if attrStr(b, "orientation", "horizontal") == "vertical" {
		classes += " is-vertical"
	}
	if cn := attrStr(b, "className", ""); cn != "" {
		classes += " " + html.EscapeString(strings.Join(blocks.SafeClassTokens(cn), " "))
	}
	label := attrStr(b, "ariaLabel", "Menu")
	list := `<ul class="wp-block-navigation__container ` + classes + `">` + items + `</ul>`
	if attrStr(b, "overlayMenu", "mobile") == "never" {
		return `<nav class="` + classes + `" aria-label="` + html.EscapeString(label) + `">` + list + `</nav>`
	}
	id := rc.uniqueID("modal-")
	return `<nav class="is-responsive ` + classes + `" aria-label="` + html.EscapeString(label) + `">` +
		`<button aria-haspopup="dialog" aria-label="Open menu" class="wp-block-navigation__responsive-container-open"><svg width="24" height="24" xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" aria-hidden="true" focusable="false"><path d="M4 7.5h16v1.5H4z"></path><path d="M4 15h16v1.5H4z"></path></svg></button>` +
		`<div class="wp-block-navigation__responsive-container" id="` + id + `"><div class="wp-block-navigation__responsive-close" tabindex="-1"><div class="wp-block-navigation__responsive-dialog">` +
		`<button aria-label="Close menu" class="wp-block-navigation__responsive-container-close"><svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" width="24" height="24" aria-hidden="true" focusable="false"><path d="m13.06 12 6.47-6.47-1.06-1.06L12 10.94 5.53 4.47 4.47 5.53 10.94 12l-6.47 6.47 1.06 1.06L12 13.06l6.47 6.47 1.06-1.06L13.06 12Z"></path></svg></button>` +
		`<div class="wp-block-navigation__responsive-container-content" id="` + id + `-content">` + list + `</div></div></div></div></nav>`
}

func pageListFallbackBlock() *blocks.Block {
	name := "core/page-list"
	return &blocks.Block{BlockName: &name, Attrs: map[string]any{}}
}
