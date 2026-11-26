package server

import (
	"context"
	"encoding/xml"
	"fmt"
	"html"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"wpgo/internal/links"
	"wpgo/internal/store"
	"wpgo/internal/wp"
)

func cdata(s string) string {
	return "<![CDATA[" + strings.ReplaceAll(s, "]]>", "]]]]><![CDATA[>") + "]]>"
}

const (
	atomTime = "2006-01-02T15:04:05Z" // Y-m-d\TH:i:s\Z
	rssTime  = time.RFC1123Z          // D, d M Y H:i:s +0000
)

// feedTitle is get_wp_title_rss() (wp_get_document_title() on a feed request): the archive title and
// the site name joined by an en dash; the main feed is the site name alone.
func (s *Server) feedTitle(ctx context.Context, q *siteQuery) string {
	site := html.EscapeString(s.feedBlogInfo(ctx, "blogname"))
	var title string
	switch q.Kind {
	case "search":
		title = "Search Results for &#8220;" + html.EscapeString(q.Search) + "&#8221;"
	case "category", "tag", "taxonomy", "author", "date":
		_, title = q.archiveTitle()
	case "home":
		if q.PostsPage != nil {
			title = html.EscapeString(q.PostsPage.Title)
		}
	}
	if q.Paged > 1 {
		title += " &#8211; Page " + strconv.Itoa(q.Paged)
	}
	if title == "" {
		return site
	}
	return title + " &#8211; " + site
}

// feed serves the post and comment feeds: feed-rss2.php, feed-atom.php, feed-rss2-comments.php and
// feed-atom-comments.php. A feed under a post's permalink lists that post's comments.
func (s *Server) feed(c *gin.Context, q *siteQuery) {
	s.cacheTags(c, q)
	// The request memo lets the item loop share one query per kind (authors, terms, posts) with the content
	// renderer; installed after cacheTags, so the feed keeps the blunt "widgets" tag.
	c.Request = c.Request.WithContext(withViewMemo(c.Request.Context()))
	ctx := c.Request.Context()
	lb := s.links(ctx)
	rc := &renderCtx{s: s, lb: lb, c: c, user: currentUser(c), q: q, feed: true, mainLoop: true, reqURI: c.Request.URL.RequestURI()}
	ctx = withRender(ctx, rc)
	atom := q.Feed == "atom" || q.Feed == "comments-atom"
	comments := q.Feed == "comments" || q.Feed == "comments-atom" || q.Post != nil

	var body string
	switch {
	case comments && atom:
		body = s.atomCommentsFeed(ctx, c, q, lb)
	case comments:
		body = s.rssCommentsFeed(ctx, c, q, lb)
	case atom:
		body = s.atomFeed(ctx, c, q, lb)
	default:
		body = s.rssFeed(ctx, c, q, lb)
	}
	ctype := "application/rss+xml; charset=UTF-8"
	if atom {
		ctype = "application/atom+xml; charset=UTF-8"
	}
	c.Data(http.StatusOK, ctype, []byte(body))
}

// feedPosts runs the feed's main query: the archive filters apply, posts_per_rss limits it.
func (s *Server) feedPosts(ctx context.Context, q *siteQuery) []*store.Post {
	pq := store.PostQuery{Types: []string{"post"}, Statuses: []string{"publish"}, PerPage: s.settings.Int("posts_per_rss"), Loc: s.loc(), NoTotal: true}
	pq.ExcludeID = s.gooListingExclude(ctx, "post") // Goocommerce Memberships
	switch q.Kind {
	case "category":
		pq.TermIDs = s.categoryWithDescendants(ctx, q.Term.ID)
	case "tag":
		pq.TermIDs = []int64{q.Term.ID}
	case "taxonomy":
		s.taxonomyQuery(ctx, q.Term, &pq)
	case "author":
		pq.AuthorID = q.Author.ID
	case "date":
		pq.Year, pq.Month, pq.Day = q.Year, q.Month, q.Day
	case "post-type-archive":
		pq.Types = []string{q.PostType}
	case "search":
		if q.Search == "" {
			return nil
		}
		pq.Search = q.Search
		pq.OrderBy = "relevance"
		pq.Types = siteSearchTypes()
		pq.ExcludePassworded = true // feeds are anonymous (WP_Query::parse_search())
	}
	posts, _, _ := s.store.QueryPosts(ctx, pq)
	return posts
}

// feedBuildDate is get_feed_build_date(): the newest modification among the feed's posts, or now.
func feedBuildDate(posts []*store.Post) time.Time {
	t := time.Time{}
	for _, p := range posts {
		if p.ModifiedAt.After(t) {
			t = p.ModifiedAt
		}
	}
	if t.IsZero() {
		t = time.Now()
	}
	return t.UTC()
}

// feedContent is get_the_content_feed(): the full content (the <!--more--> teaser is not cut in feeds),
// its images with srcset but without lazy loading.
func (s *Server) feedContent(ctx context.Context, c *gin.Context, p *store.Post, lb *links.Builder) string {
	if p.Password != "" {
		return "There is no excerpt because this is a protected post."
	}
	return s.seogoFeedContent(ctx, p, s.theContent(ctx, c, p, true, lb)) // Pursae SEO RSS before/after
}

func (s *Server) feedExcerpt(p *store.Post) string {
	if p.Password != "" {
		return "There is no excerpt because this is a protected post."
	}
	if p.Excerpt != "" {
		return wp.KsesPost(p.Excerpt) // stored excerpts may predate save-time filtering
	}
	return wp.TrimWords(stripBlocks(p.Content), 55, " [&#8230;]")
}

func (s *Server) postAuthorName(ctx context.Context, p *store.Post) (string, string) {
	author := s.displayUser(ctx, p.AuthorID) // display fields through the request memo
	if author == nil {
		return "", ""
	}
	return author.DisplayName, author.URL
}

// rssFeed is feed-rss2.php.
func (s *Server) rssFeed(ctx context.Context, c *gin.Context, q *siteQuery, lb *links.Builder) string {
	posts := s.feedPosts(ctx, q)
	s.prefetchPosts(ctx, posts)
	var sb strings.Builder
	sb.WriteString(xml.Header)
	sb.WriteString(`<rss version="2.0"
	xmlns:content="http://purl.org/rss/1.0/modules/content/"
	xmlns:wfw="http://wellformedweb.org/CommentAPI/"
	xmlns:dc="http://purl.org/dc/elements/1.1/"
	xmlns:atom="http://www.w3.org/2005/Atom"
	xmlns:sy="http://purl.org/rss/1.0/modules/syndication/"
	xmlns:slash="http://purl.org/rss/1.0/modules/slash/"
	>

<channel>
`)
	fmt.Fprintf(&sb, "\t<title>%s</title>\n", s.feedTitle(ctx, q))
	fmt.Fprintf(&sb, "\t<atom:link href=\"%s\" rel=\"self\" type=\"application/rss+xml\" />\n", html.EscapeString(lb.HomeURL(c.Request.URL.RequestURI())))
	fmt.Fprintf(&sb, "\t<link>%s</link>\n\t<description>%s</description>\n\t<lastBuildDate>%s</lastBuildDate>\n\t<language>%s</language>\n",
		html.EscapeString(lb.Front()), html.EscapeString(s.feedBlogInfo(ctx, "blogdescription")), feedBuildDate(posts).Format(rssTime), html.EscapeString(s.feedLanguage(ctx)))
	sb.WriteString("\t<sy:updatePeriod>\n\thourly\t</sy:updatePeriod>\n\t<sy:updateFrequency>\n\t1\t</sy:updateFrequency>\n\t<generator>https://wordpress.org/?v=wpgo-" + s.publicVersion() + "</generator>\n")
	for _, p := range posts {
		name, _ := s.postAuthorName(ctx, p)
		cats, tags := s.postTerms(ctx, p.ID, "category"), s.postTerms(ctx, p.ID, "post_tag")
		link := lb.Permalink(p)
		fmt.Fprintf(&sb, "\t<item>\n\t\t<title>%s</title>\n\t\t<link>%s</link>\n\t\t<dc:creator>%s</dc:creator>\n\t\t<pubDate>%s</pubDate>\n",
			html.EscapeString(p.Title), html.EscapeString(link), cdata(name), p.Date().UTC().Format(rssTime))
		for _, t := range append(cats, tags...) {
			fmt.Fprintf(&sb, "\t\t<category>%s</category>\n", cdata(t.Name))
		}
		fmt.Fprintf(&sb, "\t\t<guid isPermaLink=\"false\">%s</guid>\n", html.EscapeString(lb.HomeURL("/?p="+itoa64(p.ID))))
		if s.settings.Bool("rss_use_excerpt") {
			fmt.Fprintf(&sb, "\t\t<description>%s</description>\n", cdata(s.feedExcerpt(p)))
		} else {
			fmt.Fprintf(&sb, "\t\t<description>%s</description>\n\t\t<content:encoded>%s</content:encoded>\n", cdata(s.feedExcerpt(p)), cdata(s.feedContent(ctx, c, p, lb)))
		}
		if p.CommentCount > 0 || s.commentsOpen(p) {
			hash := "#respond"
			if p.CommentCount > 0 {
				hash = "#comments"
			}
			fmt.Fprintf(&sb, "\t\t<comments>%s</comments>\n\t\t<wfw:commentRss>%s</wfw:commentRss>\n\t\t<slash:comments>%d</slash:comments>\n",
				html.EscapeString(link+hash), html.EscapeString(lb.PostCommentsFeed(p)), p.CommentCount)
		}
		sb.WriteString("\t</item>\n")
	}
	sb.WriteString("\t</channel>\n</rss>\n")
	return sb.String()
}

// titleRSS is get_the_title_rss(): tags stripped, then HTML-escaped once, like an RSS <title>
// text node (html.EscapeString, no Unescape). Atom entry titles are type="html" inside CDATA,
// which a feed reader HTML-decodes. Unescaping before that single escape turns a stored
// &lt;img onerror&gt; back into a tag on the reader's decode.
func titleRSS(title string) string {
	return html.EscapeString(wp.StripTags(title))
}

// atomFeed is feed-atom.php.
func (s *Server) atomFeed(ctx context.Context, c *gin.Context, q *siteQuery, lb *links.Builder) string {
	posts := s.feedPosts(ctx, q)
	s.prefetchPosts(ctx, posts)
	home := lb.Front() // the language home; xml:base keeps the unfiltered root (bloginfo_rss('url'))
	var sb strings.Builder
	sb.WriteString(xml.Header)
	fmt.Fprintf(&sb, `<feed
	xmlns="http://www.w3.org/2005/Atom"
	xmlns:thr="http://purl.org/syndication/thread/1.0"
	xml:lang="%s"
	xml:base="%s/wp-atom.php"
	>
	<title type="text">%s</title>
	<subtitle type="text">%s</subtitle>

	<updated>%s</updated>

	<link rel="alternate" type="text/html" href="%s" />
	<id>%s</id>
	<link rel="self" type="application/atom+xml" href="%s" />

	<generator uri="https://wordpress.org/" version="wpgo-%s">WordPress</generator>
`, html.EscapeString(s.feedLanguage(ctx)), html.EscapeString(strings.TrimSuffix(lb.HomeURL("/"), "/")), s.feedTitle(ctx, q), html.EscapeString(s.feedBlogInfo(ctx, "blogdescription")), feedBuildDate(posts).Format(atomTime),
		html.EscapeString(home), html.EscapeString(lb.FeedLink("atom")), html.EscapeString(lb.HomeURL(c.Request.URL.RequestURI())), s.publicVersion())
	for _, p := range posts {
		name, authorURL := s.postAuthorName(ctx, p)
		link := lb.Permalink(p)
		fmt.Fprintf(&sb, "\t<entry>\n\t\t<author>\n\t\t\t<name>%s</name>\n", html.EscapeString(name))
		if authorURL != "" {
			fmt.Fprintf(&sb, "\t\t\t<uri>%s</uri>\n", html.EscapeString(authorURL))
		}
		fmt.Fprintf(&sb, "\t\t</author>\n\n\t\t<title type=\"html\">%s</title>\n\t\t<link rel=\"alternate\" type=\"text/html\" href=\"%s\" />\n\t\t<id>%s</id>\n\t\t<updated>%s</updated>\n\t\t<published>%s</published>\n",
			cdata(titleRSS(p.Title)), html.EscapeString(link), html.EscapeString(lb.HomeURL("/?p="+itoa64(p.ID))), p.ModifiedAt.UTC().Format(atomTime), p.Date().UTC().Format(atomTime))
		cats, tags := s.postTerms(ctx, p.ID, "category"), s.postTerms(ctx, p.ID, "post_tag")
		for _, t := range append(cats, tags...) {
			fmt.Fprintf(&sb, "\t\t<category scheme=\"%s\" term=\"%s\" />\n", html.EscapeString(home), html.EscapeString(t.Name))
		}
		fmt.Fprintf(&sb, "\t\t<summary type=\"html\">%s</summary>\n", cdata(s.feedExcerpt(p)))
		if !s.settings.Bool("rss_use_excerpt") && p.Password == "" {
			fmt.Fprintf(&sb, "\t\t<content type=\"html\" xml:base=\"%s\">%s</content>\n", html.EscapeString(link), cdata(s.feedContent(ctx, c, p, lb)))
		}
		fmt.Fprintf(&sb, "\t\t<link rel=\"replies\" type=\"text/html\" href=\"%s#comments\" thr:count=\"%d\" />\n\t\t<link rel=\"replies\" type=\"application/atom+xml\" href=\"%s\" thr:count=\"%d\" />\n\t\t<thr:total>%d</thr:total>\n\t</entry>\n",
			html.EscapeString(link), p.CommentCount, html.EscapeString(lb.PostCommentsFeedFormat(p, "atom")), p.CommentCount, p.CommentCount)
	}
	sb.WriteString("</feed>\n")
	return sb.String()
}

// feedComments is the comment feed query: a post's approved comments, or the newest approved
// comments across published posts, newest first and limited by posts_per_rss.
func (s *Server) feedComments(ctx context.Context, c *gin.Context, q *siteQuery) []*store.Comment {
	limit := s.settings.Int("posts_per_rss")
	u := currentUser(c)
	if q.Post != nil {
		if s.gmemHidesComments(ctx, u, q.Post.ID) { // Memberships: restricted content's discussion
			return nil
		}
		comments, _ := s.store.ApprovedComments(ctx, q.Post.ID, "desc", "")
		if limit > 0 && len(comments) > limit {
			comments = comments[:limit]
		}
		return comments
	}
	comments, _ := s.store.RecentComments(ctx, limit, false)
	if hidden := s.gmemCommentHiddenPosts(ctx, u); len(hidden) > 0 {
		comments = slices.DeleteFunc(comments, func(cm *store.Comment) bool { return slices.Contains(hidden, cm.PostID) })
	}
	return comments
}

// commentsFeedTitle is the <title> of feed-rss2-comments.php / feed-atom-comments.php.
func (s *Server) commentsFeedTitle(ctx context.Context, q *siteQuery) string {
	switch {
	case q.Post != nil:
		return "Comments on: " + html.EscapeString(q.Post.Title)
	case q.Kind == "search":
		return "Comments for " + html.EscapeString(s.feedBlogInfo(ctx, "blogname")) + " searching on " + html.EscapeString(q.Search)
	}
	return "Comments for " + s.feedTitle(ctx, q)
}

// commentFeedText is comment_text_rss(): the plain comment text, HTML-escaped.
func commentFeedText(cm *store.Comment) string {
	return html.EscapeString(wp.StripTags(wp.KsesPost(cm.Content)))
}

// protectedComment reports whether a comment's post hides its comments behind a password.
func (s *Server) protectedComment(c *gin.Context, p *store.Post) bool {
	return p.Password != "" && s.passwordRequired(c, p)
}

const protectedCommentsText = "Protected Comments: Please enter your password to view comments."

// feedCommentPosts loads the posts of a site-wide comment feed's comments in one query (none for a
// single post's feed, which shows q.Post).
func (s *Server) feedCommentPosts(ctx context.Context, q *siteQuery, comments []*store.Comment) map[int64]*store.Post {
	if q.Post != nil || len(comments) == 0 {
		return nil
	}
	ids := make([]int64, len(comments))
	for i, cm := range comments {
		ids[i] = cm.PostID
	}
	posts, _ := s.store.PostsByIDs(ctx, ids)
	return posts
}

// rssCommentsFeed is feed-rss2-comments.php.
func (s *Server) rssCommentsFeed(ctx context.Context, c *gin.Context, q *siteQuery, lb *links.Builder) string {
	comments := s.feedComments(ctx, c, q)
	posts := s.feedCommentPosts(ctx, q, comments)
	link := lb.Front()
	if q.Post != nil {
		link = lb.Permalink(q.Post)
	}
	built := time.Now().UTC()
	if len(comments) > 0 {
		built = comments[0].CreatedAt.UTC()
	}
	var sb strings.Builder
	sb.WriteString(xml.Header)
	sb.WriteString(`<rss version="2.0"
	xmlns:content="http://purl.org/rss/1.0/modules/content/"
	xmlns:dc="http://purl.org/dc/elements/1.1/"
	xmlns:atom="http://www.w3.org/2005/Atom"
	xmlns:sy="http://purl.org/rss/1.0/modules/syndication/"
	>
<channel>
`)
	fmt.Fprintf(&sb, "\t<title>%s</title>\n\t<atom:link href=\"%s\" rel=\"self\" type=\"application/rss+xml\" />\n\t<link>%s</link>\n\t<description>%s</description>\n\t<lastBuildDate>%s</lastBuildDate>\n",
		s.commentsFeedTitle(ctx, q), html.EscapeString(lb.HomeURL(c.Request.URL.RequestURI())), html.EscapeString(link), html.EscapeString(s.feedBlogInfo(ctx, "blogdescription")), built.Format(rssTime))
	sb.WriteString("\t<sy:updatePeriod>\n\thourly\t</sy:updatePeriod>\n\t<sy:updateFrequency>\n\t1\t</sy:updateFrequency>\n\t<generator>https://wordpress.org/?v=wpgo-" + s.publicVersion() + "</generator>\n")
	for _, cm := range comments {
		p := q.Post
		if p == nil {
			if p = posts[cm.PostID]; p == nil {
				continue
			}
		}
		author := defaultStr(cm.AuthorName, "Anonymous")
		title := "By: " + html.EscapeString(author)
		if q.Post == nil {
			title = "Comment on " + html.EscapeString(p.Title) + " by " + html.EscapeString(author)
		}
		commentLink := lb.Permalink(p) + "#comment-" + itoa64(cm.ID)
		fmt.Fprintf(&sb, "\t<item>\n\t\t<title>%s</title>\n\t\t<link>%s</link>\n\t\t<dc:creator>%s</dc:creator>\n\t\t<pubDate>%s</pubDate>\n\t\t<guid isPermaLink=\"false\">%s</guid>\n",
			title, html.EscapeString(commentLink), cdata(author), cm.CreatedAt.UTC().Format(rssTime), html.EscapeString(commentLink))
		if s.protectedComment(c, p) {
			fmt.Fprintf(&sb, "\t\t<description>%s</description>\n\t\t<content:encoded>%s</content:encoded>\n", protectedCommentsText, cdata(s.passwordForm(p, lb)))
		} else {
			fmt.Fprintf(&sb, "\t\t<description>%s</description>\n\t\t<content:encoded>%s</content:encoded>\n", commentFeedText(cm), cdata(s.convertSmilies(lb, commentText(cm.Content))))
		}
		sb.WriteString("\t</item>\n")
	}
	sb.WriteString("</channel>\n</rss>\n")
	return sb.String()
}

// atomCommentsFeed is feed-atom-comments.php.
func (s *Server) atomCommentsFeed(ctx context.Context, c *gin.Context, q *siteQuery, lb *links.Builder) string {
	comments := s.feedComments(ctx, c, q)
	posts := s.feedCommentPosts(ctx, q, comments)
	built := time.Now().UTC()
	if len(comments) > 0 {
		built = comments[0].CreatedAt.UTC()
	}
	var sb strings.Builder
	sb.WriteString(xml.Header)
	fmt.Fprintf(&sb, "<feed\n\txmlns=\"http://www.w3.org/2005/Atom\"\n\txmlns:thr=\"http://purl.org/syndication/thread/1.0\"\n\txml:lang=\"%s\"\n\t>\n\t<title type=\"text\">%s</title>\n\t<subtitle type=\"text\">%s</subtitle>\n\n\t<updated>%s</updated>\n",
		html.EscapeString(s.feedLanguage(ctx)), s.commentsFeedTitle(ctx, q), html.EscapeString(s.feedBlogInfo(ctx, "blogdescription")), built.Format(atomTime))
	switch {
	case q.Post != nil:
		self := lb.PostCommentsFeedFormat(q.Post, "atom")
		fmt.Fprintf(&sb, "\t<link rel=\"alternate\" type=\"text/html\" href=\"%s#comments\" />\n\t<link rel=\"self\" type=\"application/atom+xml\" href=\"%s\" />\n\t<id>%s</id>\n",
			html.EscapeString(lb.Permalink(q.Post)), html.EscapeString(self), html.EscapeString(self))
	case q.Kind == "search":
		self := lb.HomeURL(c.Request.URL.RequestURI())
		fmt.Fprintf(&sb, "\t<link rel=\"alternate\" type=\"text/html\" href=\"%s\" />\n\t<link rel=\"self\" type=\"application/atom+xml\" href=\"%s\" />\n\t<id>%s</id>\n",
			html.EscapeString(lb.SearchLink(q.Search)), html.EscapeString(self), html.EscapeString(self))
	default:
		self := lb.CommentsFeedLink("atom")
		fmt.Fprintf(&sb, "\t<link rel=\"alternate\" type=\"text/html\" href=\"%s\" />\n\t<link rel=\"self\" type=\"application/atom+xml\" href=\"%s\" />\n\t<id>%s</id>\n",
			html.EscapeString(lb.Front()), html.EscapeString(self), html.EscapeString(self))
	}
	for _, cm := range comments {
		p := q.Post
		if p == nil {
			if p = posts[cm.PostID]; p == nil {
				continue
			}
		}
		author := defaultStr(cm.AuthorName, "Anonymous")
		title := "By: " + html.EscapeString(author)
		if q.Post == nil {
			title = "Comment on " + html.EscapeString(p.Title) + " by " + html.EscapeString(author)
		}
		commentLink := lb.Permalink(p) + "#comment-" + itoa64(cm.ID)
		when := cm.CreatedAt.UTC().Format(atomTime)
		fmt.Fprintf(&sb, "\t<entry>\n\t\t<title>%s</title>\n\t\t<link rel=\"alternate\" href=\"%s\" type=\"text/html\" />\n\t\t<author>\n\t\t\t<name>%s</name>\n",
			title, html.EscapeString(commentLink), html.EscapeString(author))
		if cm.AuthorURL != "" {
			fmt.Fprintf(&sb, "\t\t\t<uri>%s</uri>\n", html.EscapeString(cm.AuthorURL))
		}
		fmt.Fprintf(&sb, "\t\t</author>\n\t\t<id>%s</id>\n\t\t<updated>%s</updated>\n\t\t<published>%s</published>\n", html.EscapeString(commentLink), when, when)
		if s.protectedComment(c, p) {
			fmt.Fprintf(&sb, "\t\t<content type=\"html\" xml:base=\"%s\">%s</content>\n", html.EscapeString(commentLink), cdata(protectedCommentsText))
		} else {
			fmt.Fprintf(&sb, "\t\t<content type=\"html\" xml:base=\"%s\">%s</content>\n", html.EscapeString(commentLink), cdata(s.convertSmilies(lb, commentText(cm.Content))))
		}
		fmt.Fprintf(&sb, "\t\t<thr:in-reply-to ref=\"%s\" href=\"%s\" type=\"text/html\" />\n\t</entry>\n",
			html.EscapeString(lb.HomeURL("/?p="+itoa64(p.ID))), html.EscapeString(lb.Permalink(p)))
	}
	sb.WriteString("</feed>\n")
	return sb.String()
}

// passwordForm is get_the_password_form().
func (s *Server) passwordForm(p *store.Post, lb *links.Builder) string {
	return fmt.Sprintf(`<form action="/wp-login?action=postpass" class="post-password-form" method="post"><input type="hidden" name="redirect_to" value="%s" /></p>
<p>This content is password-protected. To view it please enter your password below.</p>
<p><label for="pwbox-%d">Password: <input name="post_password" id="pwbox-%d" type="password" spellcheck="false" required size="20" /></label> <input type="submit" name="Submit" value="Enter" /></p></form>
`, html.EscapeString(lb.Permalink(p)), p.ID, p.ID)
}

func (s *Server) robots(c *gin.Context) {
	lb := s.links(c)
	var sb strings.Builder
	sb.WriteString("User-agent: *\n")
	if s.settings.Bool("blog_public") {
		sm := lb.HomeURL("/wp-sitemap.xml")
		if s.golylangOn() && s.pllOpts().ForceLang >= 2 { // a host per language: this host's index
			sm = s.golylangURL(c, sm)
		}
		sb.WriteString("Disallow: /wp-admin/\nAllow: /wp-admin/admin-ajax\n\nSitemap: " + sm + "\n")
	} else {
		sb.WriteString("Disallow: /\n")
	}
	c.Data(http.StatusOK, "text/plain; charset=utf-8", []byte(sb.String()))
}
