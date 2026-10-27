// Package links builds public URLs the way WordPress' link-template.php does,
// honouring the permalink structure, and parses permalink structures into matchers.
package links

import (
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"wpgo/internal/store"
)

// Builder turns posts, terms and authors into URLs. Lookup functions supply data
// it does not hold itself (page ancestors, a post's first category, author nicename).
type Builder struct {
	Home      string // home URL without trailing slash
	Structure string // permalink_structure; "" means plain links
	CatBase   string
	TagBase   string
	Loc       *time.Location

	Ancestors     func(p *store.Post) []*store.Post
	FirstCategory func(p *store.Post) *store.Term
	CategoryPath  func(t *store.Term) string // parent/child slug path
	AuthorSlug    func(id int64) string
}

func (b *Builder) Pretty() bool { return b.Structure != "" }

// HomeURL is home_url($path).
func (b *Builder) HomeURL(path string) string {
	if path == "" {
		path = "/"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return b.Home + path
}

func (b *Builder) trailing(path string) string {
	// user_trailingslashit(): follow the structure's trailing slash.
	if strings.HasSuffix(b.Structure, "/") {
		return strings.TrimSuffix(path, "/") + "/"
	}
	return strings.TrimSuffix(path, "/")
}

// Permalink is get_permalink().
func (b *Builder) Permalink(p *store.Post) string {
	switch p.Type {
	case "page":
		return b.pageLink(p)
	case "post":
		return b.postLink(p)
	}
	return b.HomeURL("/?p=" + strconv.FormatInt(p.ID, 10))
}

func draftLike(p *store.Post) bool {
	return p.Status == "draft" || p.Status == "pending" || p.Status == "auto-draft" || p.Status == "future" || p.Slug == ""
}

func (b *Builder) pageLink(p *store.Post) string {
	if !b.Pretty() || draftLike(p) {
		return b.HomeURL("/?page_id=" + strconv.FormatInt(p.ID, 10))
	}
	var parts []string
	if b.Ancestors != nil {
		for _, a := range b.Ancestors(p) {
			parts = append(parts, a.Slug)
		}
	}
	parts = append(parts, p.Slug)
	return b.HomeURL(b.trailing("/" + strings.Join(parts, "/")))
}

func (b *Builder) postLink(p *store.Post) string {
	if !b.Pretty() || draftLike(p) {
		return b.HomeURL("/?p=" + strconv.FormatInt(p.ID, 10))
	}
	t := p.Date().In(b.loc())
	r := strings.NewReplacer(
		"%year%", strconv.Itoa(t.Year()),
		"%monthnum%", fmt.Sprintf("%02d", int(t.Month())),
		"%day%", fmt.Sprintf("%02d", t.Day()),
		"%hour%", fmt.Sprintf("%02d", t.Hour()),
		"%minute%", fmt.Sprintf("%02d", t.Minute()),
		"%second%", fmt.Sprintf("%02d", t.Second()),
		"%post_id%", strconv.FormatInt(p.ID, 10),
		"%postname%", p.Slug,
		"%category%", b.postCategoryPath(p),
		"%author%", b.authorSlug(p.AuthorID),
	)
	return b.HomeURL(r.Replace(b.Structure))
}

// PermalinkTemplate is the structure with %postname% left in place (REST permalink_template).
func (b *Builder) PermalinkTemplate(p *store.Post) string {
	if !b.Pretty() {
		return b.Permalink(p)
	}
	if p.Type == "page" {
		return b.HomeURL(b.trailing("/%pagename%"))
	}
	q := *p
	q.Slug = "%postname%"
	if q.Status != "publish" && q.Status != "private" {
		q.Status = "publish"
	}
	return b.postLink(&q)
}

func (b *Builder) loc() *time.Location {
	if b.Loc == nil {
		return time.UTC
	}
	return b.Loc
}

func (b *Builder) postCategoryPath(p *store.Post) string {
	if b.FirstCategory == nil {
		return "uncategorized"
	}
	t := b.FirstCategory(p)
	if t == nil {
		return "uncategorized"
	}
	if b.CategoryPath != nil {
		return b.CategoryPath(t)
	}
	return t.Slug
}

func (b *Builder) authorSlug(id int64) string {
	if b.AuthorSlug == nil {
		return ""
	}
	return b.AuthorSlug(id)
}

// TermLink is get_term_link().
func (b *Builder) TermLink(t *store.Term) string {
	switch t.Taxonomy {
	case "category":
		if !b.Pretty() {
			return b.HomeURL("/?cat=" + strconv.FormatInt(t.ID, 10))
		}
		base := b.CatBase
		if base == "" {
			base = "category"
		}
		path := t.Slug
		if b.CategoryPath != nil {
			path = b.CategoryPath(t)
		}
		return b.HomeURL(b.trailing("/" + strings.Trim(base, "/") + "/" + path))
	case "post_tag":
		if !b.Pretty() {
			return b.HomeURL("/?tag=" + t.Slug)
		}
		base := b.TagBase
		if base == "" {
			base = "tag"
		}
		return b.HomeURL(b.trailing("/" + strings.Trim(base, "/") + "/" + t.Slug))
	case "post_format":
		if !b.Pretty() {
			return b.HomeURL("/?post_format=" + strings.TrimPrefix(t.Slug, "post-format-"))
		}
		return b.HomeURL(b.trailing("/type/" + strings.TrimPrefix(t.Slug, "post-format-")))
	}
	return b.HomeURL("/?taxonomy=" + url.QueryEscape(t.Taxonomy) + "&term=" + url.QueryEscape(t.Slug))
}

// AuthorLink is get_author_posts_url().
func (b *Builder) AuthorLink(id int64, nicename string) string {
	if !b.Pretty() {
		return b.HomeURL("/?author=" + strconv.FormatInt(id, 10))
	}
	return b.HomeURL(b.trailing("/author/" + nicename))
}

func (b *Builder) YearLink(y int) string {
	if !b.Pretty() {
		return b.HomeURL("/?m=" + strconv.Itoa(y))
	}
	return b.HomeURL(b.trailing(fmt.Sprintf("/%d", y)))
}

func (b *Builder) MonthLink(y, m int) string {
	if !b.Pretty() {
		return b.HomeURL(fmt.Sprintf("/?m=%d%02d", y, m))
	}
	return b.HomeURL(b.trailing(fmt.Sprintf("/%d/%02d", y, m)))
}

func (b *Builder) DayLink(y, m, d int) string {
	if !b.Pretty() {
		return b.HomeURL(fmt.Sprintf("/?m=%d%02d%02d", y, m, d))
	}
	return b.HomeURL(b.trailing(fmt.Sprintf("/%d/%02d/%02d", y, m, d)))
}

// FeedLink is get_feed_link(): "" for posts, "comments_" prefix handled by the caller.
func (b *Builder) FeedLink(feed string) string {
	if !b.Pretty() {
		if feed == "" || feed == "rss2" {
			return b.HomeURL("/?feed=rss2")
		}
		return b.HomeURL("/?feed=" + feed)
	}
	switch feed {
	case "", "rss2":
		return b.HomeURL(b.trailing("/feed"))
	case "comments_rss2":
		return b.HomeURL(b.trailing("/comments/feed"))
	}
	return b.HomeURL(b.trailing("/feed/" + feed))
}

// PostCommentsFeed is get_post_comments_feed_link().
func (b *Builder) PostCommentsFeed(p *store.Post) string {
	if !b.Pretty() || draftLike(p) {
		return b.HomeURL("/?feed=rss2&p=" + strconv.FormatInt(p.ID, 10))
	}
	return strings.TrimSuffix(b.Permalink(p), "/") + b.trailing("/feed")
}

// Trailing is user_trailingslashit(): the path gets the trailing slash of the permalink structure.
func (b *Builder) Trailing(path string) string { return b.trailing(path) }

// PostCommentsFeedFormat is get_post_comments_feed_link( $id, $feed ) for a feed format such as "atom".
func (b *Builder) PostCommentsFeedFormat(p *store.Post, feed string) string {
	if feed == "" || feed == "rss2" {
		return b.PostCommentsFeed(p)
	}
	if !b.Pretty() || draftLike(p) {
		return b.HomeURL("/?feed=" + feed + "&p=" + strconv.FormatInt(p.ID, 10))
	}
	return strings.TrimSuffix(b.Permalink(p), "/") + b.trailing("/feed/"+feed)
}

// CommentsFeedLink is get_feed_link('comments_' . $feed): the site comments feed in a format.
func (b *Builder) CommentsFeedLink(feed string) string {
	if feed == "" || feed == "rss2" {
		return b.FeedLink("comments_rss2")
	}
	if !b.Pretty() {
		return b.HomeURL("/?feed=comments-" + feed)
	}
	return b.HomeURL(b.trailing("/comments/feed/" + feed))
}

// CommentPageLink is get_comments_pagenum_link() without the #comments fragment: the URL of a page of
// a post's comments. With newest-first ordering the last page is the permalink itself.
func (b *Builder) CommentPageLink(p *store.Post, page, maxPage int, newestFirst bool) string {
	result := b.Permalink(p)
	paged := false
	if newestFirst {
		paged = page != maxPage
	} else {
		paged = page > 1
	}
	if !paged {
		return result
	}
	if !b.Pretty() || draftLike(p) {
		return b.addQueryArg(result, "cpage", strconv.Itoa(page))
	}
	return strings.TrimSuffix(result, "/") + b.trailing("/comment-page-"+strconv.Itoa(page))
}

// PostPageLink is _wp_link_page(): the URL of page n of a <!--nextpage--> split post.
func (b *Builder) PostPageLink(p *store.Post, n int) string {
	link := b.Permalink(p)
	if n <= 1 {
		return link
	}
	if !b.Pretty() || draftLike(p) {
		return b.addQueryArg(link, "page", strconv.Itoa(n))
	}
	return strings.TrimSuffix(link, "/") + b.trailing("/"+strconv.Itoa(n))
}

func (b *Builder) addQueryArg(link, key, value string) string {
	u, err := url.Parse(link)
	if err != nil {
		return link
	}
	q := u.Query()
	q.Set(key, value)
	u.RawQuery = q.Encode()
	return u.String()
}

// Paged appends a page number to an archive base URL (get_pagenum_link()).
func (b *Builder) Paged(base string, n int) string {
	u, err := url.Parse(base)
	if err != nil {
		return base
	}
	if !b.Pretty() {
		q := u.Query()
		if n <= 1 {
			q.Del("paged")
		} else {
			q.Set("paged", strconv.Itoa(n))
		}
		u.RawQuery = q.Encode()
		s := u.String()
		if u.Path == "" {
			s = strings.Replace(s, "?", "/?", 1)
		}
		return s
	}
	u.Path = regexp.MustCompile(`/page/\d+/?$`).ReplaceAllString(u.Path, "/")
	if n > 1 {
		u.Path = strings.TrimSuffix(u.Path, "/") + b.trailing("/page/"+strconv.Itoa(n))
	}
	if u.Path == "" {
		u.Path = "/"
	}
	return u.String()
}

// SearchLink is get_search_link().
func (b *Builder) SearchLink(q string) string {
	return b.HomeURL("/?s=" + url.QueryEscape(q))
}

// EditLink is get_edit_post_link() for the admin.
func EditLink(id int64) string {
	return "/wp-admin/post?post=" + strconv.FormatInt(id, 10) + "&action=edit"
}

// Structure matching -----------------------------------------------------------

// Matcher matches request paths against a post permalink structure.
type Matcher struct {
	re     *regexp.Regexp
	fields []string
}

var tagPatterns = map[string]string{
	"%year%":     `(\d{4})`,
	"%monthnum%": `(\d{1,2})`,
	"%day%":      `(\d{1,2})`,
	"%hour%":     `(\d{1,2})`,
	"%minute%":   `(\d{1,2})`,
	"%second%":   `(\d{1,2})`,
	"%post_id%":  `(\d+)`,
	"%postname%": `([^/]+)`,
	"%category%": `(.+?)`,
	"%author%":   `([^/]+)`,
}

var tagRe = regexp.MustCompile(`%[a-z_]+%`)

// NewMatcher compiles a permalink structure such as /%year%/%monthnum%/%postname%/.
func NewMatcher(structure string) *Matcher {
	if structure == "" {
		return nil
	}
	var fields []string
	pattern := regexp.QuoteMeta(strings.Trim(structure, "/"))
	pattern = tagRe.ReplaceAllStringFunc(pattern, func(tag string) string {
		p, ok := tagPatterns[tag]
		if !ok {
			return regexp.QuoteMeta(tag)
		}
		fields = append(fields, strings.Trim(tag, "%"))
		return p
	})
	return &Matcher{re: regexp.MustCompile(`^` + pattern + `$`), fields: fields}
}

// Match returns the structure tag values for a path without leading/trailing slashes.
func (m *Matcher) Match(path string) (map[string]string, bool) {
	if m == nil {
		return nil, false
	}
	sub := m.re.FindStringSubmatch(path)
	if sub == nil {
		return nil, false
	}
	out := map[string]string{}
	for i, f := range m.fields {
		out[f] = sub[i+1]
	}
	return out, true
}
