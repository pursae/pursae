package server

import (
	"net/url"
	"path"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

// Canonical URLs: wp_old_slug_redirect() for renamed posts and redirect_canonical() for query-var and
// trailing-slash variants of pretty permalinks.

// oldSlugRedirect is wp_old_slug_redirect(): a 404 whose last path segment is a slug a published post
// used before resolves to that post's current permalink. Query vars (?name=, %year%/%monthnum%/%day%) narrow it.
func (s *Server) oldSlugRedirect(c *gin.Context, requestPath string) string {
	ctx := c.Request.Context()
	v := c.Request.URL.Query()
	slug := v.Get("name")
	if slug == "" {
		slug = v.Get("pagename")
	}
	year, month, day := 0, 0, 0
	if slug == "" {
		clean := strings.Trim(requestPath, "/")
		if clean == "" {
			return ""
		}
		parts := strings.Split(clean, "/")
		slug = parts[len(parts)-1]
		// A date prefix from a %year%/%monthnum%/%day%/%postname% structure must match the post's date.
		if m := datePrefix(parts[:len(parts)-1]); m != nil {
			year, month, day = m[0], m[1], m[2]
		}
	}
	if slug == "" || strings.ContainsAny(slug, ".") {
		return ""
	}
	p, err := s.store.PostByOldSlug(ctx, slug, year, month, day, s.loc())
	if err != nil {
		return ""
	}
	target := s.links(ctx).Permalink(p)
	if page, _ := strconv.Atoi(v.Get("page")); page > 1 {
		target = s.links(ctx).PostPageLink(p, page)
	}
	return target
}

// datePrefix reads a leading year, month and day out of path segments such as 2026/10/27.
func datePrefix(parts []string) []int {
	if len(parts) == 0 {
		return nil
	}
	out := []int{0, 0, 0}
	for i, part := range parts {
		if i > 2 {
			break
		}
		n, err := strconv.Atoi(part)
		if err != nil {
			break
		}
		if i == 0 && len(part) != 4 {
			break
		}
		out[i] = n
	}
	if out[0] == 0 {
		return nil
	}
	return out
}

// canonicalRedirect is redirect_canonical() limited to paths: with pretty permalinks a request that
// reached its content through query vars (?p=, ?page_id=, ?cat=, ?tag=, ?author=, ?m=, ?feed=, ?paged=,
// ?page=, ?cpage=) or with the wrong trailing slash is sent to the canonical URL. "" means no redirect.
func (s *Server) canonicalRedirect(c *gin.Context, q *siteQuery, requestPath string) string {
	if q == nil || q.Preview || q.Kind == "404" || strings.HasPrefix(q.Kind, "redirect:") {
		return ""
	}
	ctx := c.Request.Context()
	lb := s.links(ctx)
	if !lb.Pretty() {
		return ""
	}
	v := c.Request.URL.Query()
	if v.Has("rest_route") || v.Has("s") || v.Has("e_search_props") || v.Has("replytocom") || v.Has("customize_changeset_uuid") {
		return ""
	}
	consumed := []string{"p", "page_id", "cat", "tag", "author", "m", "feed", "paged", "page", "cpage", "name", "pagename", "withcomments"}
	usedQueryVars := false
	for _, k := range consumed {
		if v.Has(k) {
			usedQueryVars = true
			break
		}
	}

	var base string
	switch q.Kind {
	case "single", "page":
		base = lb.Permalink(q.Post)
	case "front-page":
		base = lb.HomeURL("/")
	case "home":
		base = lb.HomeURL("/")
		if q.PostsPage != nil {
			base = lb.Permalink(q.PostsPage)
		}
	case "category", "tag":
		base = lb.TermLink(q.Term)
	case "author":
		base = lb.AuthorLink(q.Author.ID, q.Author.Nicename)
	case "date":
		switch {
		case q.Day != 0:
			base = lb.DayLink(q.Year, q.Month, q.Day)
		case q.Month != 0:
			base = lb.MonthLink(q.Year, q.Month)
		default:
			base = lb.YearLink(q.Year)
		}
	default:
		return ""
	}

	target := base
	switch {
	case q.Feed != "":
		feed := "rss2"
		if q.Feed == "atom" || q.Feed == "comments-atom" {
			feed = "atom"
		}
		switch {
		case q.Post != nil:
			target = lb.PostCommentsFeedFormat(q.Post, feed)
		case q.Feed == "comments" || q.Feed == "comments-atom":
			target = lb.CommentsFeedLink(feed)
		case q.Kind == "home" && q.PostsPage == nil:
			if feed == "atom" {
				target = lb.FeedLink("atom")
			} else {
				target = lb.FeedLink("")
			}
		default:
			target = strings.TrimSuffix(base, "/") + lb.Trailing("/feed")
			if feed == "atom" {
				target = strings.TrimSuffix(base, "/") + lb.Trailing("/feed/atom")
			}
		}
	case q.Post != nil && q.CPage > 0:
		// Every cpage gets the comments_pagination_base path, page 1 included.
		target = strings.TrimSuffix(base, "/") + lb.Trailing("/comment-page-"+strconv.Itoa(q.CPage))
	case q.Post != nil && q.Page > 1:
		target = lb.PostPageLink(q.Post, q.Page)
	case q.Paged > 1:
		target = lb.Paged(base, q.Paged)
	}

	// Only the path and the consumed query vars take part; anything else is kept as is.
	tu, err := url.Parse(target)
	if err != nil {
		return ""
	}
	rest := url.Values{}
	for k, vals := range v {
		keep := true
		for _, ck := range consumed {
			if k == ck {
				keep = false
				break
			}
		}
		if keep {
			rest[k] = vals
		}
	}
	reqPath := c.Request.URL.Path
	if reqPath == "" {
		reqPath = "/"
	}
	if !usedQueryVars && path.Clean(reqPath) == path.Clean(tu.Path) && strings.HasSuffix(reqPath, "/") == strings.HasSuffix(tu.Path, "/") {
		return ""
	}
	if !usedQueryVars && reqPath == "/" {
		return ""
	}
	out := tu.Path
	if out == "" {
		out = "/"
	}
	if len(rest) > 0 {
		out += "?" + rest.Encode()
	}
	full := lb.HomeURL(out)
	if full == lb.HomeURL(c.Request.URL.RequestURI()) {
		return ""
	}
	return full
}
