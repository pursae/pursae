package server

import (
	"context"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
)

// Forum URLs (default base "forums"):
//
//	/forums/                       index            /forums/f/<slug>/[page/N/]       forum
//	/forums/f/<slug>/new/          new-topic        /forums/t/<id>/<slug>/[page/N/]  topic (wrong/missing slug → 301)
//	/forums/r/<id>/                reply → 302 to the topic page #post-<id>
//	/forums/edit/<id>/             edit             /forums/search/?q=…[page/N/]    search
//	/forums/u/<nicename>/[page/N/] profile          /forums/login/ /forums/register/ /forums/verify/?u=&k= /forums/account/
//	/forums/members/[page/N/]?q=&sort=  members (WP7; 404 while forumSettings.ShowMembers is off)
//	/forums/new/                   new-topic with the forum picker (v0.2.5)
//	/forums/topics/[page/N/]       every topic (?sort=&tag=)          /forums/tag/<slug>/[page/N/]  a tag's topics
//	/forums/tags/                  the tag list                       /forums/unsubscribe/?t=&u=&k= a reply email's unsubscribe link
//
// Topic lists (index, forum, topics, tag) take ?sort=latest|popular|unanswered|solved and ?tag=<slug>
// (forum and topics views); both are part of the page cache key while the plugin is on.
//
// Forum URLs always end in a slash, whatever the permalink structure; they work with plain
// permalinks too (site() hands every path to the intercept).

// forumSlugRe is a slug as a path segment (decoded): lowercase letters of any script, digits, -, _,
// %, and . (nicenames may contain dots and @).
var (
	forumSlugRe     = regexp.MustCompile(`^[\p{L}\p{Mn}\p{Mc}\p{N}_%-]+$`)
	forumNicenameRe = regexp.MustCompile(`^[\p{Ll}\p{Lo}\p{Lm}\p{Mn}\p{Mc}\p{N}_%.@-]+$`)
)

// The length limits (200 / 100 runes) are checked outside the regexps: a counted {1,200} repeat over
// these Unicode classes compiles to a huge program (~8 MB of heap retained per process).
func forumValidSlug(s string) bool {
	return utf8.RuneCountInString(s) <= 200 && s == strings.ToLower(s) && forumSlugRe.MatchString(s)
}

func forumValidNicename(s string) bool {
	return utf8.RuneCountInString(s) <= 100 && forumNicenameRe.MatchString(s)
}

// forumParsePath maps a site path (golylang-stripped, no slashes at either end) to a forum route.
func forumParsePath(base, path string) (forumRoute, bool) {
	base = strings.Trim(base, "/")
	path = strings.Trim(path, "/")
	if base == "" || (path != base && !strings.HasPrefix(path, base+"/")) {
		return forumRoute{}, false
	}
	rest := strings.Trim(strings.TrimPrefix(path, base), "/")
	var segs []string
	if rest != "" {
		segs = strings.Split(rest, "/")
	}
	for _, s := range segs {
		if s == "" {
			return forumRoute{}, false // "//" inside the path
		}
	}
	rt := forumRoute{Page: 1}
	paged := false
	// A trailing page/N.
	if n := len(segs); n >= 2 && segs[n-2] == "page" {
		paged = true
		p, err := strconv.Atoi(segs[n-1])
		if err != nil || p < 1 || p > 100000 || segs[n-1] != strconv.Itoa(p) {
			return forumRoute{}, false
		}
		rt.Page = p
		segs = segs[:n-2]
		if len(segs) == 0 {
			return forumRoute{}, false // the index has no pages
		}
	}
	id := func(s string) (int64, bool) {
		v, err := strconv.ParseInt(s, 10, 64)
		return v, err == nil && v > 0 && s == strconv.FormatInt(v, 10)
	}
	switch {
	case len(segs) == 0:
		rt.View = forumViewIndex
	case segs[0] == "f" && len(segs) == 2 && forumValidSlug(segs[1]):
		rt.View, rt.Slug = forumViewForum, segs[1]
	case segs[0] == "f" && len(segs) == 3 && segs[2] == "new" && forumValidSlug(segs[1]) && !paged:
		rt.View, rt.Slug = forumViewNewTopic, segs[1]
	case segs[0] == "t" && (len(segs) == 2 || len(segs) == 3):
		v, ok := id(segs[1])
		if !ok {
			return forumRoute{}, false
		}
		rt.View, rt.ID = forumViewTopic, v
		if len(segs) == 3 {
			if !forumValidSlug(segs[2]) {
				return forumRoute{}, false
			}
			rt.Slug = segs[2]
		}
	case segs[0] == "r" && len(segs) == 2 && !paged:
		v, ok := id(segs[1])
		if !ok {
			return forumRoute{}, false
		}
		rt.View, rt.ID = forumViewReply, v
	case segs[0] == "edit" && len(segs) == 2 && !paged:
		v, ok := id(segs[1])
		if !ok {
			return forumRoute{}, false
		}
		rt.View, rt.ID = forumViewEdit, v
	case segs[0] == "search" && len(segs) == 1:
		rt.View = forumViewSearch
	case segs[0] == "u" && len(segs) == 2 && forumValidNicename(segs[1]):
		rt.View, rt.Slug = forumViewProfile, segs[1]
	case len(segs) == 1 && segs[0] == "members":
		rt.View = forumViewMembers
	case len(segs) == 1 && segs[0] == "new" && !paged:
		rt.View = forumViewNewTopic // v0.2.5: the forum picker (Slug "")
	case len(segs) == 1 && segs[0] == "topics":
		rt.View = forumViewTopics
	case len(segs) == 2 && segs[0] == "tag" && forumValidSlug(segs[1]):
		rt.View, rt.Slug = forumViewTag, segs[1]
	case len(segs) == 1 && !paged && (segs[0] == "tags" || segs[0] == "unsubscribe"):
		rt.View = segs[0] // forumViewTags, forumViewUnsubscribe
	case len(segs) == 1 && !paged && (segs[0] == "login" || segs[0] == "register" || segs[0] == "verify" || segs[0] == "account"):
		rt.View = segs[0] // forumViewLogin, forumViewRegister, forumViewVerify, forumViewAccount
	default:
		return forumRoute{}, false
	}
	return rt, true
}

// forumBase is the forum's first path segment ("forums").
func (s *Server) forumBase() string { return s.forumLoad().Base }

// forumURL is the absolute URL of /<base>/<parts…>/ (with the request's language under Pursae Multi
// Language). Parts are path segments, escaped here.
func (s *Server) forumURL(ctx context.Context, parts ...string) string {
	p := "/" + s.forumBase() + "/"
	for _, x := range parts {
		x = strings.Trim(x, "/")
		if x == "" {
			continue
		}
		for _, seg := range strings.Split(x, "/") {
			p += url.PathEscape(seg) + "/"
		}
	}
	lb := s.links(ctx)
	u := lb.HomeURL(p)
	if lb.LinkFilter != nil {
		u = lb.LinkFilter("home", nil, u) // Golylang: the language prefix
	}
	return u
}

// forumPagedParts appends page/N to parts when page > 1.
func forumPagedParts(page int, parts ...string) []string {
	if page > 1 {
		parts = append(parts, "page", strconv.Itoa(page))
	}
	return parts
}

// forumForumURL is a forum's (or category's) URL.
func (s *Server) forumForumURL(ctx context.Context, f *forumForum) string {
	if f == nil {
		return s.forumURL(ctx)
	}
	return s.forumURL(ctx, "f", f.Slug)
}

// forumTopicURL is a topic's URL, on page page of its replies.
func (s *Server) forumTopicURL(ctx context.Context, t *forumTopic, page int) string {
	if t == nil {
		return s.forumURL(ctx)
	}
	parts := []string{"t", strconv.FormatInt(t.ID, 10)}
	if t.Slug != "" {
		parts = append(parts, t.Slug)
	}
	return s.forumURL(ctx, forumPagedParts(page, parts...)...)
}

// forumReplyURL is a reply's short URL (it redirects to the topic page holding the reply).
func (s *Server) forumReplyURL(ctx context.Context, r *forumReply) string {
	if r == nil {
		return s.forumURL(ctx)
	}
	return s.forumURL(ctx, "r", strconv.FormatInt(r.ID, 10))
}

// forumProfileURL is a member's forum profile.
func (s *Server) forumProfileURL(ctx context.Context, nicename string) string {
	if nicename == "" {
		return s.forumURL(ctx)
	}
	return s.forumURL(ctx, "u", nicename)
}

// forumLocalPath is the path (and query) of an absolute URL of this site, for redirect_to values
// that safeRedirect accepts.
func forumLocalPath(abs string) string {
	u, err := url.Parse(abs)
	if err != nil || u.Path == "" {
		return "/"
	}
	p := u.EscapedPath()
	if u.RawQuery != "" {
		p += "?" + u.RawQuery
	}
	return p
}

// forumURLsFor are the links every forum view and the theme functions use. Login and Register carry
// ?redirect=<this page> when the visitor is on another forum page; Logout is core's log-out link
// with its nonce and redirect_to the forum index; LostPassword is lostPasswordURL (never a hidden login
// address); GoogleStart / GoogleLink are the gin routes under /forum-auth/.
func (s *Server) forumURLsFor(c *gin.Context) forumURLs {
	ctx := c.Request.Context()
	index := s.forumURL(ctx)
	st := s.forumLoad()
	u := forumURLs{Index: index, Search: s.forumURL(ctx, "search"), Login: s.forumURL(ctx, "login"),
		Register: s.forumURL(ctx, "register"), Account: s.forumURL(ctx, "account"),
		LostPassword: s.lostPasswordURL(ctx),
		Topics:       s.forumURL(ctx, "topics"), Tags: s.forumURL(ctx, "tags"), NewAny: s.forumURL(ctx, "new"), Rules: st.RulesURL}
	here := ""
	if c.Request.Method == "GET" {
		here = c.Request.URL.Path
		if rt, ok := forumParsePath(s.forumBase(), s.golylangSitePath(c, strings.Trim(here, "/"))); ok &&
			(rt.View == forumViewLogin || rt.View == forumViewRegister || rt.View == forumViewVerify) {
			here = ""
		}
	}
	if here != "" && safeRedirect(here, "") != "" {
		u.Login += "?redirect=" + url.QueryEscape(here)
		u.Register += "?redirect=" + url.QueryEscape(here)
	}
	if currentUser(c) != nil {
		u.Logout = s.loginPath() + "?action=logout&_wpnonce=" + url.QueryEscape(s.nonce(c, "log-out")) +
			"&redirect_to=" + url.QueryEscape(forumLocalPath(index))
	}
	if s.forumGoogleReady() {
		back := forumLocalPath(index)
		if here != "" {
			back = here
		}
		u.GoogleStart = forumAuthPrefix + "/google?redirect=" + url.QueryEscape(back)
		u.GoogleLink = forumAuthPrefix + "/google?link=1&redirect=" + url.QueryEscape(forumLocalPath(u.Account))
	}
	return u
}

// Pursae Multi Language --------------------------------------------------------------------------

// forumPllTranslationURL is the URL of the forum view being rendered in language l, for the language
// switcher and the hreflang links (pllTranslationURL asks it first). Forum content is shared by every
// language, so it is the same forum path under l's prefix. ok is false off the forum. has is false
// on private views (login, search, a member search, …): the switcher still links them, the hreflang
// list leaves them out.
func (s *Server) forumPllTranslationURL(c *gin.Context, q *siteQuery, l *pllLanguage) (u string, has, ok bool) {
	if c == nil || c.Request == nil || q == nil || q.Forum == "" || l == nil {
		return "", false, false
	}
	ctx := c.Request.Context()
	lb := s.links(ctx)
	src, has := q.ForumURL, true
	if src == "" {
		// A private view: the path being viewed (a theme 404 never gets here: q.Forum is empty).
		src, has = lb.Home+c.Request.URL.EscapedPath(), false
		if v := strings.TrimSpace(c.Query("q")); v != "" && q.Forum == forumViewSearch {
			src += "?q=" + url.QueryEscape(v)
		} else if v != "" && q.Forum == forumViewMembers {
			src += forumMembersQuery(v, forumMemberFrontSort(c.Query("sort"))) // a member search keeps its sort
		}
	} else if q.Paged > 1 {
		src = strings.TrimSuffix(src, "/") + "/page/" + strconv.Itoa(q.Paged) + "/"
	}
	return s.pllConvert(ctx, lb.Home, src, l, "home", s.pllOpts()), has, true
}

// forumPllXDefault is the hreflang="x-default" URL of a public forum view under Pursae Multi
// Language: the view in the default language. "" off the forum, on private and paged views, and
// with fewer than two languages.
func (s *Server) forumPllXDefault(c *gin.Context, q *siteQuery) string {
	if c == nil || q == nil || q.Forum == "" || q.ForumURL == "" || q.Paged > 1 || q.Preview || !s.golylangOn() {
		return ""
	}
	if r := pllFrom(c); r == nil || !r.Front || r.Lang == nil {
		return ""
	}
	ctx := c.Request.Context()
	def := s.pllDefault(ctx)
	if def == nil || len(s.pllActiveLanguages(ctx)) < 2 {
		return ""
	}
	u, has, ok := s.forumPllTranslationURL(c, q, def)
	if !ok || !has {
		return ""
	}
	return u
}
