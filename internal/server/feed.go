package server

import (
	"context"
	"encoding/xml"
	"fmt"
	"html"
	"net/http"
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
func (s *Server) feedTitle(q *siteQuery) string {
	site := html.EscapeString(s.settings.String("blogname"))
	var title string
	switch q.Kind {
	case "search":
		title = "Search Results for &#8220;" + html.EscapeString(q.Search) + "&#8221;"
	case "category", "tag", "author", "date":
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
	pq := store.PostQuery{Types: []string{"post"}, Statuses: []string{"publish"}, PerPage: s.settings.Int("posts_per_rss"), Loc: s.loc()}
	switch q.Kind {
	case "category", "tag":
		pq.TermIDs = []int64{q.Term.ID}
	case "author":
		pq.AuthorID = q.Author.ID
	case "date":
		pq.Year, pq.Month, pq.Day = q.Year, q.Month, q.Day
	case "search":
		if q.Search == "" {
			return nil
		}
		pq.Search = q.Search
		pq.Types = []string{"post", "page"}
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
	return s.theContent(ctx, c, p, true, lb)
}

func (s *Server) feedExcerpt(p *store.Post) string {
	if p.Password != "" {
		return "There is no excerpt because this is a protected post."
	}
	if p.Excerpt != "" {
		return p.Excerpt
	}
	return wp.TrimWords(stripBlocks(p.Content), 55, " [&#8230;]")
}

func (s *Server) postAuthorName(ctx context.Context, p *store.Post) (string, string) {
	author, _ := s.store.UserByID(ctx, p.AuthorID)
	if author == nil {
		return "", ""
	}
	return author.DisplayName, author.URL
}

// rssFeed is feed-rss2.php.
func (s *Server) rssFeed(ctx context.Context, c *gin.Context, q *siteQuery, lb *links.Builder) string {
	posts := s.feedPosts(ctx, q)
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
	fmt.Fprintf(&sb, "\t<title>%s</title>\n", s.feedTitle(q))
	fmt.Fprintf(&sb, "\t<atom:link href=\"%s\" rel=\"self\" type=\"application/rss+xml\" />\n", html.EscapeString(lb.HomeURL(c.Request.URL.RequestURI())))
	fmt.Fprintf(&sb, "\t<link>%s</link>\n\t<description>%s</description>\n\t<lastBuildDate>%s</lastBuildDate>\n\t<language>en-US</language>\n",
		html.EscapeString(lb.HomeURL("/")), html.EscapeString(s.settings.String("blogdescription")), feedBuildDate(posts).Format(rssTime))
	sb.WriteString("\t<sy:updatePeriod>\n\thourly\t</sy:updatePeriod>\n\t<sy:updateFrequency>\n\t1\t</sy:updateFrequency>\n\t<generator>https://wordpress.org/?v=wpgo-" + Version + "</generator>\n")
	for _, p := range posts {
		name, _ := s.postAuthorName(ctx, p)
		cats, _ := s.store.TermsForPosts(ctx, []int64{p.ID}, "category")
		tags, _ := s.store.TermsForPosts(ctx, []int64{p.ID}, "post_tag")
		link := lb.Permalink(p)
		fmt.Fprintf(&sb, "\t<item>\n\t\t<title>%s</title>\n\t\t<link>%s</link>\n\t\t<dc:creator>%s</dc:creator>\n\t\t<pubDate>%s</pubDate>\n",
			html.EscapeString(p.Title), html.EscapeString(link), cdata(name), p.Date().UTC().Format(rssTime))
		for _, t := range append(cats[p.ID], tags[p.ID]...) {
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

// atomFeed is feed-atom.php.
func (s *Server) atomFeed(ctx context.Context, c *gin.Context, q *siteQuery, lb *links.Builder) string {
	posts := s.feedPosts(ctx, q)
	home := lb.HomeURL("/")
	var sb strings.Builder
	sb.WriteString(xml.Header)
	fmt.Fprintf(&sb, `<feed
	xmlns="http://www.w3.org/2005/Atom"
	xmlns:thr="http://purl.org/syndication/thread/1.0"
	xml:lang="en-US"
	xml:base="%s/wp-atom.php"
	>
	<title type="text">%s</title>
	<subtitle type="text">%s</subtitle>

	<updated>%s</updated>

	<link rel="alternate" type="text/html" href="%s" />
	<id>%s</id>
	<link rel="self" type="application/atom+xml" href="%s" />

	<generator uri="https://wordpress.org/" version="wpgo-%s">WordPress</generator>
`, html.EscapeString(strings.TrimSuffix(home, "/")), s.feedTitle(q), html.EscapeString(s.settings.String("blogdescription")), feedBuildDate(posts).Format(atomTime),
		html.EscapeString(home), html.EscapeString(lb.FeedLink("atom")), html.EscapeString(lb.HomeURL(c.Request.URL.RequestURI())), Version)
	for _, p := range posts {
		name, authorURL := s.postAuthorName(ctx, p)
		link := lb.Permalink(p)
		fmt.Fprintf(&sb, "\t<entry>\n\t\t<author>\n\t\t\t<name>%s</name>\n", html.EscapeString(name))
		if authorURL != "" {
			fmt.Fprintf(&sb, "\t\t\t<uri>%s</uri>\n", html.EscapeString(authorURL))
		}
		fmt.Fprintf(&sb, "\t\t</author>\n\n\t\t<title type=\"html\">%s</title>\n\t\t<link rel=\"alternate\" type=\"text/html\" href=\"%s\" />\n\t\t<id>%s</id>\n\t\t<updated>%s</updated>\n\t\t<published>%s</published>\n",
			cdata(p.Title), html.EscapeString(link), html.EscapeString(lb.HomeURL("/?p="+itoa64(p.ID))), p.ModifiedAt.UTC().Format(atomTime), p.Date().UTC().Format(atomTime))
		cats, _ := s.store.TermsForPosts(ctx, []int64{p.ID}, "category")
		tags, _ := s.store.TermsForPosts(ctx, []int64{p.ID}, "post_tag")
		for _, t := range append(cats[p.ID], tags[p.ID]...) {
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
func (s *Server) feedComments(ctx context.Context, q *siteQuery) []*store.Comment {
	limit := s.settings.Int("posts_per_rss")
	if q.Post != nil {
		comments, _ := s.store.ApprovedComments(ctx, q.Post.ID, "desc", "")
		if limit > 0 && len(comments) > limit {
			comments = comments[:limit]
		}
		return comments
	}
	comments, _ := s.store.RecentComments(ctx, limit, false)
	return comments
}

// commentsFeedTitle is the <title> of feed-rss2-comments.php / feed-atom-comments.php.
func (s *Server) commentsFeedTitle(q *siteQuery) string {
	switch {
	case q.Post != nil:
		return "Comments on: " + html.EscapeString(q.Post.Title)
	case q.Kind == "search":
		return "Comments for " + html.EscapeString(s.settings.String("blogname")) + " searching on " + html.EscapeString(q.Search)
	}
	return "Comments for " + s.feedTitle(q)
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

// rssCommentsFeed is feed-rss2-comments.php.
func (s *Server) rssCommentsFeed(ctx context.Context, c *gin.Context, q *siteQuery, lb *links.Builder) string {
	comments := s.feedComments(ctx, q)
	link := lb.HomeURL("/")
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
		s.commentsFeedTitle(q), html.EscapeString(lb.HomeURL(c.Request.URL.RequestURI())), html.EscapeString(link), html.EscapeString(s.settings.String("blogdescription")), built.Format(rssTime))
	sb.WriteString("\t<sy:updatePeriod>\n\thourly\t</sy:updatePeriod>\n\t<sy:updateFrequency>\n\t1\t</sy:updateFrequency>\n\t<generator>https://wordpress.org/?v=wpgo-" + Version + "</generator>\n")
	for _, cm := range comments {
		p := q.Post
		if p == nil {
			var err error
			if p, err = s.store.GetPost(ctx, cm.PostID); err != nil {
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
	comments := s.feedComments(ctx, q)
	built := time.Now().UTC()
	if len(comments) > 0 {
		built = comments[0].CreatedAt.UTC()
	}
	var sb strings.Builder
	sb.WriteString(xml.Header)
	fmt.Fprintf(&sb, "<feed\n\txmlns=\"http://www.w3.org/2005/Atom\"\n\txmlns:thr=\"http://purl.org/syndication/thread/1.0\"\n\txml:lang=\"en-US\"\n\t>\n\t<title type=\"text\">%s</title>\n\t<subtitle type=\"text\">%s</subtitle>\n\n\t<updated>%s</updated>\n",
		s.commentsFeedTitle(q), html.EscapeString(s.settings.String("blogdescription")), built.Format(atomTime))
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
			html.EscapeString(lb.HomeURL("/")), html.EscapeString(self), html.EscapeString(self))
	}
	for _, cm := range comments {
		p := q.Post
		if p == nil {
			var err error
			if p, err = s.store.GetPost(ctx, cm.PostID); err != nil {
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
		sb.WriteString("Disallow: /wp-admin/\nAllow: /wp-admin/admin-ajax\n\nSitemap: " + lb.HomeURL("/wp-sitemap.xml") + "\n")
	} else {
		sb.WriteString("Disallow: /\n")
	}
	c.Data(http.StatusOK, "text/plain; charset=utf-8", []byte(sb.String()))
}

// sitemapIndex is /wp-sitemap.xml (core sitemaps): posts, pages, taxonomies and users.
func (s *Server) sitemapIndex(c *gin.Context) {
	if !s.settings.Bool("blog_public") {
		c.Status(http.StatusNotFound)
		return
	}
	lb := s.links(c)
	var sb strings.Builder
	sb.WriteString(xml.Header + `<sitemapindex xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">`)
	for _, part := range []string{"posts-post-1", "posts-page-1", "taxonomies-category-1", "taxonomies-post_tag-1", "users-1"} {
		fmt.Fprintf(&sb, "<sitemap><loc>%s</loc></sitemap>", html.EscapeString(lb.HomeURL("/wp-sitemap-"+part+".xml")))
	}
	sb.WriteString("</sitemapindex>\n")
	c.Data(http.StatusOK, "application/xml; charset=UTF-8", []byte(sb.String()))
}

func (s *Server) sitemapPart(c *gin.Context, part string) {
	if !s.settings.Bool("blog_public") {
		c.Status(http.StatusNotFound)
		return
	}
	ctx := c.Request.Context()
	lb := s.links(ctx)
	idx := strings.LastIndex(part, "-")
	if idx < 0 {
		c.Status(http.StatusNotFound)
		return
	}
	page, _ := strconv.Atoi(part[idx+1:])
	if page < 1 {
		page = 1
	}
	kind := part[:idx]
	var locs []string
	switch kind {
	case "posts-post", "posts-page":
		typ := strings.TrimPrefix(kind, "posts-")
		posts, _, _ := s.store.QueryPosts(ctx, store.PostQuery{Types: []string{typ}, Statuses: []string{"publish"}, PerPage: 2000, Page: page, OrderBy: "id", Order: "asc"})
		for _, p := range posts {
			if p.Password == "" {
				locs = append(locs, lb.Permalink(p))
			}
		}
	case "taxonomies-category", "taxonomies-post_tag":
		terms, _, _ := s.store.QueryTerms(ctx, store.TermQuery{Taxonomy: strings.TrimPrefix(kind, "taxonomies-"), HideEmpty: true, PerPage: 2000, Page: page})
		for _, t := range terms {
			locs = append(locs, lb.TermLink(t))
		}
	case "users":
		rows, err := s.store.Pool.Query(ctx, `SELECT DISTINCT u.id, u.nicename FROM users u JOIN posts p ON p.author_id = u.id
			WHERE p.type = 'post' AND p.status = 'publish' ORDER BY u.id`)
		if err == nil {
			for rows.Next() {
				var id int64
				var nicename string
				if rows.Scan(&id, &nicename) == nil {
					locs = append(locs, lb.AuthorLink(id, nicename))
				}
			}
			rows.Close()
		}
	default:
		c.Status(http.StatusNotFound)
		return
	}
	var sb strings.Builder
	sb.WriteString(xml.Header + `<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">`)
	for _, l := range locs {
		fmt.Fprintf(&sb, "<url><loc>%s</loc></url>", html.EscapeString(l))
	}
	sb.WriteString("</urlset>\n")
	c.Data(http.StatusOK, "application/xml; charset=UTF-8", []byte(sb.String()))
}
