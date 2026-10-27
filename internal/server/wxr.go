package server

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"wpgo/internal/media"
	"wpgo/internal/store"
	"wpgo/internal/wp"
)

// WordPress eXtended RSS 1.2: export.php (export_wp()) and the WordPress Importer.

func xmlEsc(v string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(v))
	return b.String()
}

// exportData fills the export.php filter controls.
type exportData struct {
	Categories                        []*store.Term
	Authors                           []*store.User
	PostMonths, PageMonths, AttMonths []monthOption
	Statuses                          []choice
}

// exportMonths is export_date_options(): the months that have items of the post type (or media), newest first.
func (s *Server) exportMonths(ctx context.Context, typ string) []monthOption {
	loc := s.loc()
	q := `SELECT DISTINCT to_char(coalesce(published_at, created_at) AT TIME ZONE $2, 'YYYY-MM') FROM posts WHERE type = $1 AND status <> 'auto-draft' ORDER BY 1 DESC`
	if typ == "attachment" {
		q = `SELECT DISTINCT to_char(created_at AT TIME ZONE $2, 'YYYY-MM') FROM media WHERE $1 = 'attachment' ORDER BY 1 DESC`
	}
	rows, err := s.store.Pool.Query(ctx, q, typ, loc.String())
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []monthOption
	for rows.Next() {
		var ym string
		if rows.Scan(&ym) != nil {
			continue
		}
		if t, err := time.Parse("2006-01", ym); err == nil {
			out = append(out, monthOption{Value: ym, Label: t.Format("January 2006")})
		}
	}
	return out
}

// exportFilters are export.php's optional filters for one content type.
type exportFilters struct {
	Category int64
	Author   int64
	Start    string // YYYY-MM
	End      string // YYYY-MM
	Status   string
}

// keep reports whether a post date falls inside the YYYY-MM range ('post_date >= start AND post_date < end + 1 month').
func (f exportFilters) keep(t time.Time, loc *time.Location) bool {
	if f.Start != "" && f.Start != "0" {
		if st, err := time.ParseInLocation("2006-01", f.Start, loc); err == nil && t.In(loc).Before(st) {
			return false
		}
	}
	if f.End != "" && f.End != "0" {
		if en, err := time.ParseInLocation("2006-01", f.End, loc); err == nil && !t.In(loc).Before(en.AddDate(0, 1, 0)) {
			return false
		}
	}
	return true
}

// exportScreen is export.php: the form, and the WXR download when "download" is set.
func (s *Server) exportScreen(c *gin.Context) {
	u := currentUser(c)
	if !u.Can("export", nil) {
		c.String(http.StatusForbidden, "Sorry, you are not allowed to export the content of this site.")
		return
	}
	ctx := c.Request.Context()
	if c.Query("download") == "" {
		d := exportData{PostMonths: s.exportMonths(ctx, "post"), PageMonths: s.exportMonths(ctx, "page"), AttMonths: s.exportMonths(ctx, "attachment")}
		d.Categories, _ = s.store.HierarchicalTerms(ctx, "category", false)
		users, _, _ := s.store.QueryUsers(ctx, store.UserQuery{PerPage: 10000})
		var authorIDs []int64
		rows, err := s.store.Pool.Query(ctx, `SELECT DISTINCT author_id FROM posts WHERE author_id IS NOT NULL AND type IN ('post','page') AND status <> 'auto-draft'`)
		if err == nil {
			for rows.Next() {
				var id int64
				if rows.Scan(&id) == nil {
					authorIDs = append(authorIDs, id)
				}
			}
			rows.Close()
		}
		for _, x := range users {
			for _, id := range authorIDs {
				if x.ID == id {
					d.Authors = append(d.Authors, x)
				}
			}
		}
		for _, st := range [][2]string{{"publish", "Published"}, {"future", "Scheduled"}, {"draft", "Draft"}, {"pending", "Pending"}, {"private", "Private"}, {"trash", "Trash"}} {
			d.Statuses = append(d.Statuses, choice{Value: st[0], Label: st[1]})
		}
		a := s.pageAssets(c)
		a.Script("jquery")
		a.AddInline("jquery", `jQuery( function($) {
	var form = $('#export-filters'),
		filters = form.find('.export-filters');
	filters.hide();
	form.find('input:radio').on( 'change', function() {
		filters.hide();
		$('#' + $(this).val() + '-filters').show();
	});
});`)
		s.adminPage(c, "export", "Export", "export-php", "admin/export", d)
		return
	}
	what := c.DefaultQuery("content", "all")
	var types []string
	var f exportFilters
	switch what {
	case "post", "posts":
		what = "post"
		types = []string{"post"}
		f.Category, _ = strconv.ParseInt(c.Query("cat"), 10, 64)
		f.Author, _ = strconv.ParseInt(c.Query("post_author"), 10, 64)
		f.Start, f.End, f.Status = c.Query("post_start_date"), c.Query("post_end_date"), c.Query("post_status")
	case "page", "pages":
		what = "page"
		types = []string{"page"}
		f.Author, _ = strconv.ParseInt(c.Query("page_author"), 10, 64)
		f.Start, f.End, f.Status = c.Query("page_start_date"), c.Query("page_end_date"), c.Query("page_status")
	case "attachment":
		f.Start, f.End = c.Query("attachment_start_date"), c.Query("attachment_end_date")
	default:
		what = "all"
		types = []string{"post", "page", "wp_block"}
	}
	if f.Status == "0" {
		f.Status = ""
	}
	lb := s.links(c)
	loc := s.loc()
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8" ?>
<!-- This is a WordPress eXtended RSS file generated by WordPress Go as an export of your site. -->
<!-- It contains information about your site's posts, pages, comments, categories, and other content. -->
<!-- You may use this file to transfer that content from one site to another. -->
<!-- To import this information into a WordPress site follow these steps: -->
<!-- 1. Log in to that site as an administrator. -->
<!-- 2. Go to Tools: Import in the WordPress admin panel. -->
<!-- 3. Install the "WordPress" importer from the list. -->
<!-- 4. Activate & Run Importer. -->
<!-- 5. Upload this file using the form provided on that page. -->
<!-- generator="WordPress Go/` + Version + `" created="` + time.Now().UTC().Format("2006-01-02 15:04") + `" -->
<rss version="2.0"
	xmlns:excerpt="http://wordpress.org/export/1.2/excerpt/"
	xmlns:content="http://purl.org/rss/1.0/modules/content/"
	xmlns:wfw="http://wellformedweb.org/CommentAPI/"
	xmlns:dc="http://purl.org/dc/elements/1.1/"
	xmlns:wp="http://wordpress.org/export/1.2/"
>
<channel>
`)
	fmt.Fprintf(&b, "\t<title>%s</title>\n\t<link>%s</link>\n\t<description>%s</description>\n\t<pubDate>%s</pubDate>\n\t<language>en-US</language>\n\t<wp:wxr_version>1.2</wp:wxr_version>\n\t<wp:base_site_url>%s</wp:base_site_url>\n\t<wp:base_blog_url>%s</wp:base_blog_url>\n\n",
		xmlEsc(s.settings.String("blogname")), xmlEsc(lb.Home), xmlEsc(s.settings.String("blogdescription")), time.Now().UTC().Format(time.RFC1123Z), xmlEsc(lb.Home), xmlEsc(lb.Home))

	users, _, _ := s.store.QueryUsers(ctx, store.UserQuery{PerPage: 10000})
	logins := map[int64]string{}
	for _, x := range users {
		logins[x.ID] = x.Login
		fmt.Fprintf(&b, "\t<wp:author><wp:author_id>%d</wp:author_id><wp:author_login>%s</wp:author_login><wp:author_email>%s</wp:author_email><wp:author_display_name>%s</wp:author_display_name><wp:author_first_name>%s</wp:author_first_name><wp:author_last_name>%s</wp:author_last_name></wp:author>\n",
			x.ID, cdata(x.Login), cdata(x.Email), cdata(x.DisplayName), cdata(x.Profile["first_name"]), cdata(x.Profile["last_name"]))
	}
	b.WriteString("\n")
	termSlugs := map[int64]string{}
	if what == "all" {
		// export.php lists every term; a filtered export lists only the terms of the exported posts (below).
		for _, tax := range []string{"category", "post_tag"} {
			terms, _, _ := s.store.QueryTerms(ctx, store.TermQuery{Taxonomy: tax, PerPage: 100000})
			for _, t := range terms {
				termSlugs[t.ID] = t.Slug
			}
			for _, t := range terms {
				if tax == "category" {
					fmt.Fprintf(&b, "\t<wp:category><wp:term_id>%d</wp:term_id><wp:category_nicename>%s</wp:category_nicename><wp:category_parent>%s</wp:category_parent><wp:cat_name>%s</wp:cat_name>%s</wp:category>\n",
						t.ID, cdata(t.Slug), cdata(termSlugs[t.ParentID]), cdata(t.Name), optDesc("wp:category_description", t.Description))
				} else {
					fmt.Fprintf(&b, "\t<wp:tag><wp:term_id>%d</wp:term_id><wp:tag_slug>%s</wp:tag_slug><wp:tag_name>%s</wp:tag_name>%s</wp:tag>\n",
						t.ID, cdata(t.Slug), cdata(t.Name), optDesc("wp:tag_description", t.Description))
				}
			}
		}
	}
	var menus []*store.Menu
	if what == "all" {
		// wxr_nav_menu_terms(): navigation menus are nav_menu terms.
		menus, _ = s.store.Menus(ctx)
		for _, m := range menus {
			fmt.Fprintf(&b, "\t<wp:term><wp:term_id>%d</wp:term_id><wp:term_taxonomy>%s</wp:term_taxonomy><wp:term_slug>%s</wp:term_slug><wp:term_parent>%s</wp:term_parent><wp:term_name>%s</wp:term_name></wp:term>\n",
				m.ID, cdata("nav_menu"), cdata(m.Slug), cdata(""), cdata(m.Name))
		}
	}
	b.WriteString("\n\t<generator>https://wordpress.org/?v=" + Version + "</generator>\n\n")

	wpDate := func(t time.Time) string { return t.In(loc).Format("2006-01-02 15:04:05") }
	postmeta := func(key, value string) string {
		return "\t\t<wp:postmeta>\n\t\t\t<wp:meta_key>" + cdata(key) + "</wp:meta_key>\n\t\t\t<wp:meta_value>" + cdata(value) + "</wp:meta_value>\n\t\t</wp:postmeta>\n"
	}
	if len(types) > 0 {
		q := store.PostQuery{Types: types, Statuses: []string{"publish", "draft", "pending", "private", "future", "trash"}, PerPage: -1, Page: 1, OrderBy: "id", Order: "ASC"}
		if f.Status != "" {
			q.Statuses = []string{f.Status}
		}
		if f.Author > 0 {
			q.AuthorID = f.Author
		}
		if f.Category > 0 {
			q.TermIDs = []int64{f.Category}
		}
		posts, _, _ := s.store.QueryPosts(ctx, q)
		var kept []*store.Post
		var ids []int64
		for _, p := range posts {
			if !f.keep(p.Date(), loc) {
				continue
			}
			kept = append(kept, p)
			ids = append(ids, p.ID)
		}
		posts = kept
		cats, _ := s.store.TermsForPosts(ctx, ids, "category")
		tags, _ := s.store.TermsForPosts(ctx, ids, "post_tag")
		if what != "all" {
			// wxr_cat_name() etc. for the terms of a filtered export only.
			seen := map[int64]bool{}
			for _, p := range posts {
				for _, t := range cats[p.ID] {
					if seen[t.ID] {
						continue
					}
					seen[t.ID] = true
					parent := ""
					if t.ParentID != 0 {
						if pt, err := s.store.GetTerm(ctx, t.ParentID); err == nil {
							parent = pt.Slug
						}
					}
					fmt.Fprintf(&b, "\t<wp:category><wp:term_id>%d</wp:term_id><wp:category_nicename>%s</wp:category_nicename><wp:category_parent>%s</wp:category_parent><wp:cat_name>%s</wp:cat_name>%s</wp:category>\n",
						t.ID, cdata(t.Slug), cdata(parent), cdata(t.Name), optDesc("wp:category_description", t.Description))
				}
				for _, t := range tags[p.ID] {
					if seen[t.ID] {
						continue
					}
					seen[t.ID] = true
					fmt.Fprintf(&b, "\t<wp:tag><wp:term_id>%d</wp:term_id><wp:tag_slug>%s</wp:tag_slug><wp:tag_name>%s</wp:tag_name>%s</wp:tag>\n",
						t.ID, cdata(t.Slug), cdata(t.Name), optDesc("wp:tag_description", t.Description))
				}
			}
		}
		for _, p := range posts {
			b.WriteString("\t<item>\n")
			fmt.Fprintf(&b, "\t\t<title>%s</title>\n\t\t<link>%s</link>\n\t\t<pubDate>%s</pubDate>\n\t\t<dc:creator>%s</dc:creator>\n\t\t<guid isPermaLink=\"false\">%s</guid>\n\t\t<description></description>\n",
				cdata(p.Title), xmlEsc(lb.Permalink(p)), p.Date().UTC().Format(time.RFC1123Z), cdata(logins[p.AuthorID]), xmlEsc(lb.Home+"/?p="+itoa64(p.ID)))
			fmt.Fprintf(&b, "\t\t<content:encoded>%s</content:encoded>\n\t\t<excerpt:encoded>%s</excerpt:encoded>\n", cdata(p.Content), cdata(p.Excerpt))
			gmt := "0000-00-00 00:00:00"
			if p.PublishedAt != nil {
				gmt = p.PublishedAt.UTC().Format("2006-01-02 15:04:05")
			}
			fmt.Fprintf(&b, "\t\t<wp:post_id>%d</wp:post_id>\n\t\t<wp:post_date>%s</wp:post_date>\n\t\t<wp:post_date_gmt>%s</wp:post_date_gmt>\n\t\t<wp:post_modified>%s</wp:post_modified>\n\t\t<wp:post_modified_gmt>%s</wp:post_modified_gmt>\n",
				p.ID, cdata(wpDate(p.Date())), cdata(gmt), cdata(wpDate(p.ModifiedAt)), cdata(p.ModifiedAt.UTC().Format("2006-01-02 15:04:05")))
			fmt.Fprintf(&b, "\t\t<wp:comment_status>%s</wp:comment_status>\n\t\t<wp:ping_status>%s</wp:ping_status>\n\t\t<wp:post_name>%s</wp:post_name>\n\t\t<wp:status>%s</wp:status>\n\t\t<wp:post_parent>%d</wp:post_parent>\n\t\t<wp:menu_order>%d</wp:menu_order>\n\t\t<wp:post_type>%s</wp:post_type>\n\t\t<wp:post_password>%s</wp:post_password>\n\t\t<wp:is_sticky>%d</wp:is_sticky>\n",
				cdata(p.CommentStatus), cdata(p.PingStatus), cdata(p.Slug), cdata(p.Status), p.ParentID, p.MenuOrder, cdata(p.Type), cdata(p.Password), map[bool]int{true: 1}[p.Sticky])
			for _, t := range cats[p.ID] {
				fmt.Fprintf(&b, "\t\t<category domain=\"category\" nicename=\"%s\">%s</category>\n", xmlEsc(t.Slug), cdata(t.Name))
			}
			for _, t := range tags[p.ID] {
				fmt.Fprintf(&b, "\t\t<category domain=\"post_tag\" nicename=\"%s\">%s</category>\n", xmlEsc(t.Slug), cdata(t.Name))
			}
			if p.Format != "" && p.Format != "standard" {
				fmt.Fprintf(&b, "\t\t<category domain=\"post_format\" nicename=\"post-format-%s\">%s</category>\n", xmlEsc(p.Format), cdata(strings.ToUpper(p.Format[:1])+p.Format[1:]))
			}
			if p.FeaturedMediaID > 0 {
				b.WriteString(postmeta("_thumbnail_id", itoa64(p.FeaturedMediaID)))
			}
			// wxr_post_meta(): every meta row except _edit_lock.
			keys := make([]string, 0, len(p.Meta))
			for k := range p.Meta {
				if k != "_edit_lock" && k != "_edit_last" {
					keys = append(keys, k)
				}
			}
			sort.Strings(keys)
			for _, k := range keys {
				if v, ok := scalarMeta(p.Meta[k]); ok {
					b.WriteString(postmeta(k, v))
				}
			}
			comments, _, _ := s.store.QueryComments(ctx, store.CommentQuery{PostID: p.ID, Status: "approved", PerPage: 100000, Order: "asc"})
			for _, cm := range comments {
				fmt.Fprintf(&b, "\t\t<wp:comment>\n\t\t\t<wp:comment_id>%d</wp:comment_id>\n\t\t\t<wp:comment_author>%s</wp:comment_author>\n\t\t\t<wp:comment_author_email>%s</wp:comment_author_email>\n\t\t\t<wp:comment_author_url>%s</wp:comment_author_url>\n\t\t\t<wp:comment_author_IP>%s</wp:comment_author_IP>\n\t\t\t<wp:comment_date>%s</wp:comment_date>\n\t\t\t<wp:comment_date_gmt>%s</wp:comment_date_gmt>\n\t\t\t<wp:comment_content>%s</wp:comment_content>\n\t\t\t<wp:comment_approved>%s</wp:comment_approved>\n\t\t\t<wp:comment_type>%s</wp:comment_type>\n\t\t\t<wp:comment_parent>%d</wp:comment_parent>\n\t\t\t<wp:comment_user_id>%d</wp:comment_user_id>\n\t\t</wp:comment>\n",
					cm.ID, cdata(cm.AuthorName), cdata(cm.AuthorEmail), cdata(cm.AuthorURL), cdata(cm.AuthorIP), cdata(wpDate(cm.CreatedAt)), cdata(cm.CreatedAt.UTC().Format("2006-01-02 15:04:05")),
					cdata(cm.Content), cdata("1"), cdata(cm.Type), cm.ParentID, cm.UserID)
			}
			b.WriteString("\t</item>\n")
		}
	}
	if what == "all" || what == "attachment" {
		atts, _, _ := s.store.QueryAttachments(ctx, store.AttachmentQuery{Page: 1, PerPage: 100000})
		for _, a := range atts {
			if !f.keep(a.CreatedAt, loc) {
				continue
			}
			fileURL := s.uploadURL(ctx, a.FilePath)
			fmt.Fprintf(&b, "\t<item>\n\t\t<title>%s</title>\n\t\t<link>%s</link>\n\t\t<pubDate>%s</pubDate>\n\t\t<dc:creator>%s</dc:creator>\n\t\t<guid isPermaLink=\"false\">%s</guid>\n\t\t<description></description>\n\t\t<content:encoded>%s</content:encoded>\n\t\t<excerpt:encoded>%s</excerpt:encoded>\n\t\t<wp:post_id>%d</wp:post_id>\n\t\t<wp:post_date>%s</wp:post_date>\n\t\t<wp:post_date_gmt>%s</wp:post_date_gmt>\n\t\t<wp:post_modified>%s</wp:post_modified>\n\t\t<wp:post_modified_gmt>%s</wp:post_modified_gmt>\n\t\t<wp:comment_status>open</wp:comment_status>\n\t\t<wp:ping_status>closed</wp:ping_status>\n\t\t<wp:post_name>%s</wp:post_name>\n\t\t<wp:status>%s</wp:status>\n\t\t<wp:post_parent>%d</wp:post_parent>\n\t\t<wp:menu_order>%d</wp:menu_order>\n\t\t<wp:post_type>%s</wp:post_type>\n\t\t<wp:post_password></wp:post_password>\n\t\t<wp:is_sticky>0</wp:is_sticky>\n\t\t<wp:attachment_url>%s</wp:attachment_url>\n%s\t</item>\n",
				cdata(a.Title), xmlEsc(fileURL), a.CreatedAt.UTC().Format(time.RFC1123Z), cdata(logins[a.UploaderID]), xmlEsc(fileURL), cdata(a.Description), cdata(a.Caption),
				a.ID, cdata(wpDate(a.CreatedAt)), cdata(a.CreatedAt.UTC().Format("2006-01-02 15:04:05")), cdata(wpDate(a.UpdatedAt)), cdata(a.UpdatedAt.UTC().Format("2006-01-02 15:04:05")),
				cdata(wp.SanitizeTitle(a.Title)), cdata("inherit"), a.PostID, a.MenuOrder, cdata("attachment"), cdata(fileURL), postmeta("_wp_attachment_image_alt", a.Alt))
		}
	}
	if what == "all" {
		// Menu items are nav_menu_item posts in WordPress; their ids are offset so they never collide with post ids.
		for _, m := range menus {
			items, _ := s.store.MenuItems(ctx, m.ID)
			for _, it := range items {
				id := wxrMenuItemBase + it.ID
				parent := int64(0)
				if it.ParentID != 0 {
					parent = wxrMenuItemBase + it.ParentID
				}
				now := time.Now()
				fmt.Fprintf(&b, "\t<item>\n\t\t<title>%s</title>\n\t\t<link>%s</link>\n\t\t<pubDate>%s</pubDate>\n\t\t<dc:creator>%s</dc:creator>\n\t\t<guid isPermaLink=\"false\">%s</guid>\n\t\t<description></description>\n\t\t<content:encoded>%s</content:encoded>\n\t\t<excerpt:encoded>%s</excerpt:encoded>\n\t\t<wp:post_id>%d</wp:post_id>\n\t\t<wp:post_date>%s</wp:post_date>\n\t\t<wp:post_date_gmt>%s</wp:post_date_gmt>\n\t\t<wp:post_modified>%s</wp:post_modified>\n\t\t<wp:post_modified_gmt>%s</wp:post_modified_gmt>\n\t\t<wp:comment_status>closed</wp:comment_status>\n\t\t<wp:ping_status>closed</wp:ping_status>\n\t\t<wp:post_name>%d</wp:post_name>\n\t\t<wp:status>publish</wp:status>\n\t\t<wp:post_parent>0</wp:post_parent>\n\t\t<wp:menu_order>%d</wp:menu_order>\n\t\t<wp:post_type>nav_menu_item</wp:post_type>\n\t\t<wp:post_password></wp:post_password>\n\t\t<wp:is_sticky>0</wp:is_sticky>\n\t\t<category domain=\"nav_menu\" nicename=\"%s\">%s</category>\n",
					cdata(it.Label), xmlEsc(lb.Home+"/?p="+itoa64(id)), now.UTC().Format(time.RFC1123Z), cdata(logins[u.ID]), xmlEsc(lb.Home+"/?p="+itoa64(id)), cdata(it.Description), cdata(it.AttrTitle),
					id, cdata(wpDate(now)), cdata(now.UTC().Format("2006-01-02 15:04:05")), cdata(wpDate(now)), cdata(now.UTC().Format("2006-01-02 15:04:05")), id, it.Order, xmlEsc(m.Slug), cdata(m.Name))
				b.WriteString(postmeta("_menu_item_type", it.Kind))
				b.WriteString(postmeta("_menu_item_menu_item_parent", itoa64(parent)))
				b.WriteString(postmeta("_menu_item_object_id", itoa64(it.TargetID)))
				b.WriteString(postmeta("_menu_item_object", it.Object))
				b.WriteString(postmeta("_menu_item_target", it.Target))
				b.WriteString(postmeta("_menu_item_classes", phpSerializeStrings(strings.Fields(it.Classes))))
				b.WriteString(postmeta("_menu_item_xfn", it.XFN))
				b.WriteString(postmeta("_menu_item_url", it.URL))
				b.WriteString("\t</item>\n")
			}
		}
	}
	b.WriteString("</channel>\n</rss>\n")
	name := wp.SanitizeTitle(s.settings.String("blogname"))
	if name == "" {
		name = "wordpress-go"
	}
	c.Header("Content-Description", "File Transfer")
	c.Header("Content-Disposition", "attachment; filename="+name+".WordPress."+time.Now().Format("2006-01-02")+".xml")
	c.Data(http.StatusOK, "text/xml; charset=UTF-8", []byte(b.String()))
}

// wxrMenuItemBase offsets menu item ids in exports: in WordPress menu items are posts with their own ids;
// here they live in their own table, so the export keeps them apart from post ids.
const wxrMenuItemBase int64 = 1_000_000_000

func scalarMeta(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64), true
	case bool:
		if t {
			return "1", true
		}
		return "", true
	case nil:
		return "", true
	}
	return "", false
}

func optDesc(tag, v string) string {
	if v == "" {
		return ""
	}
	return "<" + tag + ">" + cdata(v) + "</" + tag + ">"
}

// phpSerializeStrings is serialize() of a PHP list of strings, the way _menu_item_classes is stored.
func phpSerializeStrings(list []string) string {
	if len(list) == 0 {
		list = []string{""}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "a:%d:{", len(list))
	for i, v := range list {
		fmt.Fprintf(&b, "i:%d;s:%d:\"%s\";", i, len(v), v)
	}
	b.WriteString("}")
	return b.String()
}

// phpUnserializeStrings reads the string values of a serialized PHP array (a:N:{i:0;s:L:"v";...}).
// A plain (unserialized) value is returned as is.
func phpUnserializeStrings(v string) []string {
	if !strings.HasPrefix(v, "a:") {
		if v == "" {
			return nil
		}
		return strings.Fields(v)
	}
	var out []string
	for {
		i := strings.Index(v, "s:")
		if i < 0 {
			break
		}
		rest := v[i+2:]
		j := strings.Index(rest, ":\"")
		if j < 0 {
			break
		}
		n, err := strconv.Atoi(rest[:j])
		if err != nil || n < 0 || j+2+n > len(rest) {
			break
		}
		s := rest[j+2 : j+2+n]
		if s != "" {
			out = append(out, s)
		}
		v = rest[j+2+n:]
	}
	return out
}

// WXR import ------------------------------------------------------------------------

type wxrDoc struct {
	Channel struct {
		Authors []struct {
			Login       string `xml:"author_login"`
			Email       string `xml:"author_email"`
			DisplayName string `xml:"author_display_name"`
		} `xml:"author"`
		Categories []struct {
			TermID int64  `xml:"term_id"`
			Slug   string `xml:"category_nicename"`
			Parent string `xml:"category_parent"`
			Name   string `xml:"cat_name"`
			Desc   string `xml:"category_description"`
		} `xml:"category"`
		Tags []struct {
			TermID int64  `xml:"term_id"`
			Slug   string `xml:"tag_slug"`
			Name   string `xml:"tag_name"`
		} `xml:"tag"`
		Terms []struct {
			TermID   int64  `xml:"term_id"`
			Taxonomy string `xml:"term_taxonomy"`
			Slug     string `xml:"term_slug"`
			Name     string `xml:"term_name"`
		} `xml:"term"`
		Items []wxrItem `xml:"item"`
	} `xml:"channel"`
}

type wxrItem struct {
	Title         string `xml:"title"`
	Creator       string `xml:"creator"`
	Content       string `xml:"http://purl.org/rss/1.0/modules/content/ encoded"`
	Excerpt       string `xml:"http://wordpress.org/export/1.2/excerpt/ encoded"`
	PostID        int64  `xml:"post_id"`
	PostDate      string `xml:"post_date"`
	CommentStatus string `xml:"comment_status"`
	PingStatus    string `xml:"ping_status"`
	Name          string `xml:"post_name"`
	Status        string `xml:"status"`
	Parent        int64  `xml:"post_parent"`
	MenuOrder     int    `xml:"menu_order"`
	Type          string `xml:"post_type"`
	Password      string `xml:"post_password"`
	Sticky        int    `xml:"is_sticky"`
	AttachmentURL string `xml:"attachment_url"`
	Categories    []struct {
		Domain   string `xml:"domain,attr"`
		Nicename string `xml:"nicename,attr"`
		Name     string `xml:",chardata"`
	} `xml:"category"`
	Meta []struct {
		Key   string `xml:"meta_key"`
		Value string `xml:"meta_value"`
	} `xml:"postmeta"`
	Comments []struct {
		ID       int64  `xml:"comment_id"`
		Author   string `xml:"comment_author"`
		Email    string `xml:"comment_author_email"`
		URL      string `xml:"comment_author_url"`
		IP       string `xml:"comment_author_IP"`
		Date     string `xml:"comment_date"`
		Content  string `xml:"comment_content"`
		Approved string `xml:"comment_approved"`
		Type     string `xml:"comment_type"`
		Parent   int64  `xml:"comment_parent"`
	} `xml:"comment"`
}

func (it *wxrItem) meta(key string) string {
	for _, m := range it.Meta {
		if m.Key == key {
			return m.Value
		}
	}
	return ""
}

// importScreen is import.php with the WordPress importer built in.
func (s *Server) importScreen(c *gin.Context) {
	u := currentUser(c)
	if !u.Can("import", nil) {
		c.String(http.StatusForbidden, "Sorry, you are not allowed to import content into this site.")
		return
	}
	d := map[string]any{"Nonce": s.nonce(c, "import-upload")}
	if c.Request.Method == http.MethodPost {
		if !s.checkNonce(c, "import-upload") {
			s.denyNonce(c)
			return
		}
		f, _, err := c.Request.FormFile("import")
		if err != nil {
			d["Error"] = "No file was uploaded."
		} else {
			data, _ := io.ReadAll(io.LimitReader(f, 256<<20))
			f.Close()
			report, err := s.importWXR(c, u, data)
			if err != nil {
				d["Error"] = "Sorry, there has been an error. " + err.Error()
			} else {
				d["Report"] = report
				s.purgeAll(c)
			}
		}
	}
	s.adminPage(c, "import", "Import", "import-php", "admin/import", d)
}

type importReport struct {
	Posts, Pages, Blocks, Skipped, Terms, Comments, Attachments, MenuItems int
	Log                                                                    []string
}

func (s *Server) importWXR(c *gin.Context, u *store.User, data []byte) (*importReport, error) {
	ctx := c.Request.Context()
	var doc wxrDoc
	dec := xml.NewDecoder(strings.NewReader(string(data)))
	dec.Strict = false
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("this does not appear to be a WXR file: %v", err)
	}
	rep := &importReport{}
	authors := map[string]int64{}
	for _, a := range doc.Channel.Authors {
		if ex, err := s.store.UserByLoginOrEmail(ctx, a.Login); err == nil {
			authors[a.Login] = ex.ID
		} else {
			authors[a.Login] = u.ID
		}
	}
	termID := map[string]int64{}
	oldTerms := map[int64]int64{} // processed_terms: term ids in the file -> ours
	ensureTerm := func(tax, slug, name string, parent int64) int64 {
		key := tax + ":" + slug
		if id, ok := termID[key]; ok {
			return id
		}
		if t, err := s.store.TermBySlug(ctx, tax, slug); err == nil {
			termID[key] = t.ID
			return t.ID
		}
		t := &store.Term{Taxonomy: tax, Slug: slug, Name: defaultStr(name, slug), ParentID: parent}
		if err := s.store.InsertTerm(ctx, t); err != nil {
			return 0
		}
		rep.Terms++
		termID[key] = t.ID
		return t.ID
	}
	for _, cat := range doc.Channel.Categories {
		var parent int64
		if cat.Parent != "" {
			parent = ensureTerm("category", cat.Parent, cat.Parent, 0)
		}
		if id := ensureTerm("category", cat.Slug, cat.Name, parent); cat.TermID != 0 {
			oldTerms[cat.TermID] = id
		}
	}
	for _, t := range doc.Channel.Tags {
		if id := ensureTerm("post_tag", t.Slug, t.Name, 0); t.TermID != 0 {
			oldTerms[t.TermID] = id
		}
	}
	// Navigation menus (nav_menu terms): found by slug, created when missing.
	menuBySlug := map[string]int64{}
	ensureMenu := func(slug, name string) int64 {
		if id, ok := menuBySlug[slug]; ok {
			return id
		}
		if all, err := s.store.Menus(ctx); err == nil {
			for _, m := range all {
				menuBySlug[m.Slug] = m.ID
			}
		}
		if id, ok := menuBySlug[slug]; ok {
			return id
		}
		id, err := s.store.CreateMenu(ctx, defaultStr(name, slug))
		if err != nil {
			return 0
		}
		menuBySlug[slug] = id
		return id
	}
	for _, t := range doc.Channel.Terms {
		if t.Taxonomy == "nav_menu" && t.Slug != "" {
			ensureMenu(t.Slug, t.Name)
		}
	}
	idMap := map[int64]int64{}
	loc := s.loc()
	urlMap := map[string]string{}    // attachment URLs in the file -> ours (backfill_attachment_urls)
	featured := map[int64]int64{}    // our post id -> _thumbnail_id in the file
	postParents := map[int64]int64{} // our post id -> parent id in the file (backfill_parents)
	attParents := map[int64]int64{}  // our attachment id -> parent post id in the file
	var imported []int64             // our post ids, for the URL backfill
	type menuItemRef struct {        // menu items are processed after posts and terms (process_menu_item)
		item *wxrItem
		menu int64
	}
	var menuItems []menuItemRef
	for i := range doc.Channel.Items {
		it := &doc.Channel.Items[i]
		switch it.Type {
		case "post", "page", "wp_block":
		case "attachment":
			if it.Status == "trash" {
				rep.Skipped++
				continue
			}
			a, err := s.sideloadURL(ctx, it.AttachmentURL, authors[it.Creator], 0)
			if err != nil {
				rep.Log = append(rep.Log, fmt.Sprintf("Failed to import Media “%s”: %v", it.Title, err))
				rep.Skipped++
				continue
			}
			if it.Title != "" {
				a.Title = it.Title
				a.Slug = wp.SanitizeTitle(defaultStr(it.Name, it.Title))
			}
			a.Description, a.Caption, a.Alt = it.Content, it.Excerpt, it.meta("_wp_attachment_image_alt")
			if a.UploaderID == 0 {
				a.UploaderID = u.ID
			}
			_ = s.store.UpdateAttachment(ctx, a)
			if t, err := time.ParseInLocation("2006-01-02 15:04:05", it.PostDate, loc); err == nil {
				_, _ = s.store.Pool.Exec(ctx, `UPDATE media SET created_at = $2, uploader_id = $3 WHERE id = $1`, a.ID, t, nullable64(a.UploaderID))
			}
			idMap[it.PostID] = a.ID
			urlMap[it.AttachmentURL] = s.uploadURL(ctx, a.FilePath)
			if it.Parent != 0 {
				attParents[a.ID] = it.Parent
			}
			rep.Attachments++
			continue
		case "nav_menu_item":
			slug := ""
			for _, cat := range it.Categories {
				if cat.Domain == "nav_menu" {
					slug = cat.Nicename
				}
			}
			if slug == "" {
				rep.Log = append(rep.Log, "Menu item skipped due to missing menu slug")
				rep.Skipped++
				continue
			}
			menuID := ensureMenu(slug, slug)
			if menuID == 0 {
				rep.Log = append(rep.Log, fmt.Sprintf("Menu item skipped due to invalid menu slug: %s", slug))
				rep.Skipped++
				continue
			}
			menuItems = append(menuItems, menuItemRef{item: it, menu: menuID})
			continue
		default:
			rep.Skipped++
			continue
		}
		if it.Status == "auto-draft" || it.Status == "trash" || it.Status == "inherit" {
			rep.Skipped++
			continue
		}
		if ex, err := s.store.PostBySlug(ctx, it.Type, it.Name); err == nil && it.Name != "" && ex.Title == it.Title {
			rep.Log = append(rep.Log, fmt.Sprintf("%s “%s” already exists.", wxrTypeLabel(it.Type), it.Title))
			idMap[it.PostID] = ex.ID
			rep.Skipped++
			continue
		}
		p := &store.Post{Type: it.Type, AuthorID: authors[it.Creator], Title: it.Title, Content: it.Content, Excerpt: it.Excerpt,
			Status: defaultStr(it.Status, "draft"), Slug: it.Name, MenuOrder: it.MenuOrder, Password: it.Password,
			CommentStatus: defaultStr(it.CommentStatus, "open"), PingStatus: defaultStr(it.PingStatus, "open"), Sticky: it.Sticky == 1, Format: "standard",
			Meta: map[string]any{}}
		if p.AuthorID == 0 {
			p.AuthorID = u.ID
		}
		if t, err := time.ParseInLocation("2006-01-02 15:04:05", it.PostDate, loc); err == nil {
			p.PublishedAt = &t
		}
		if parent, ok := idMap[it.Parent]; ok {
			p.ParentID = parent
		}
		var catIDs, tagIDs []int64
		for _, cat := range it.Categories {
			switch cat.Domain {
			case "category":
				catIDs = append(catIDs, ensureTerm("category", cat.Nicename, cat.Name, 0))
			case "post_tag":
				tagIDs = append(tagIDs, ensureTerm("post_tag", cat.Nicename, cat.Name, 0))
			case "post_format":
				p.Format = strings.TrimPrefix(cat.Nicename, "post-format-")
			}
		}
		// Post meta: everything but the editor locks; _thumbnail_id is remapped once the media is in.
		var thumb int64
		for _, m := range it.Meta {
			switch m.Key {
			case "_edit_lock", "_edit_last":
			case "_thumbnail_id":
				thumb, _ = strconv.ParseInt(m.Value, 10, 64)
			default:
				p.Meta[m.Key] = m.Value
			}
		}
		if err := s.store.InsertPost(ctx, p); err != nil {
			rep.Log = append(rep.Log, fmt.Sprintf("Failed to import “%s”: %v", it.Title, err))
			continue
		}
		idMap[it.PostID] = p.ID
		imported = append(imported, p.ID)
		if thumb != 0 {
			featured[p.ID] = thumb
		}
		if it.Parent != 0 && p.ParentID == 0 {
			postParents[p.ID] = it.Parent
		}
		switch it.Type {
		case "post":
			if len(catIDs) == 0 {
				catIDs = []int64{int64(s.settings.Int("default_category"))}
			}
			_ = s.store.SetPostTerms(ctx, p.ID, "category", catIDs)
			_ = s.store.SetPostTerms(ctx, p.ID, "post_tag", tagIDs)
			rep.Posts++
		case "page":
			rep.Pages++
		default:
			rep.Blocks++
		}
		commentIDs := map[int64]int64{}
		for _, cm := range it.Comments {
			nc := &store.Comment{PostID: p.ID, AuthorName: cm.Author, AuthorEmail: cm.Email, AuthorURL: cm.URL, AuthorIP: cm.IP,
				Content: cm.Content, Type: defaultStr(cm.Type, "comment"), Status: "approved", ParentID: commentIDs[cm.Parent]}
			if cm.Approved != "1" {
				nc.Status = "pending"
			}
			if cm.Approved == "spam" {
				nc.Status = "spam"
			}
			if err := s.store.InsertComment(ctx, nc); err == nil {
				commentIDs[cm.ID] = nc.ID
				rep.Comments++
				if t, err := time.ParseInLocation("2006-01-02 15:04:05", cm.Date, loc); err == nil {
					_, _ = s.store.Pool.Exec(ctx, `UPDATE comments SET created_at = $2 WHERE id = $1`, nc.ID, t)
				}
			}
		}
	}
	// backfill_parents(): parents that were imported after their children.
	for id, oldParent := range postParents {
		if parent, ok := idMap[oldParent]; ok {
			_, _ = s.store.Pool.Exec(ctx, `UPDATE posts SET parent_id = $2 WHERE id = $1`, id, parent)
		}
	}
	for id, oldParent := range attParents {
		if parent, ok := idMap[oldParent]; ok {
			_, _ = s.store.Pool.Exec(ctx, `UPDATE media SET post_id = $2 WHERE id = $1`, id, parent)
		}
	}
	// remap_featured_images()
	for id, oldThumb := range featured {
		if att, ok := idMap[oldThumb]; ok {
			_, _ = s.store.Pool.Exec(ctx, `UPDATE posts SET featured_media_id = $2 WHERE id = $1`, id, att)
		}
	}
	// backfill_attachment_urls(): links to the old site's uploads point at the imported files, longest URLs first.
	if len(urlMap) > 0 {
		var pairs []string
		for from, to := range urlMap {
			pairs = append(pairs, from, to)
			if ext := path.Ext(from); ext != "" && path.Ext(to) != "" {
				// Image sizes: photo-300x200.jpg follows photo.jpg.
				pairs = append(pairs, strings.TrimSuffix(from, ext), strings.TrimSuffix(to, path.Ext(to)))
			}
		}
		type pair struct{ from, to string }
		var ordered []pair
		for i := 0; i+1 < len(pairs); i += 2 {
			ordered = append(ordered, pair{pairs[i], pairs[i+1]})
		}
		sort.Slice(ordered, func(i, j int) bool { return len(ordered[i].from) > len(ordered[j].from) })
		var oldnew []string
		for _, p := range ordered {
			oldnew = append(oldnew, p.from, p.to)
		}
		rep := strings.NewReplacer(oldnew...)
		for _, id := range imported {
			p, err := s.store.GetPost(ctx, id)
			if err != nil {
				continue
			}
			if content := rep.Replace(p.Content); content != p.Content {
				p.Content = content
				_ = s.store.UpdatePost(ctx, p, u.ID)
			}
		}
	}
	// process_menu_item(): items whose object was not imported are skipped (custom links always import).
	menuItemIDs := map[int64]int64{}
	menuOrphans := map[int64]int64{}
	for _, ref := range menuItems {
		it := ref.item
		kind := defaultStr(it.meta("_menu_item_type"), "custom")
		objectID, _ := strconv.ParseInt(it.meta("_menu_item_object_id"), 10, 64)
		switch kind {
		case "taxonomy":
			if id, ok := oldTerms[objectID]; ok {
				objectID = id
			} else {
				rep.Log = append(rep.Log, fmt.Sprintf("Menu item “%s” skipped due to missing taxonomy term.", it.Title))
				rep.Skipped++
				continue
			}
		case "post_type":
			if id, ok := idMap[objectID]; ok {
				objectID = id
			} else {
				rep.Log = append(rep.Log, fmt.Sprintf("Menu item “%s” skipped due to missing post.", it.Title))
				rep.Skipped++
				continue
			}
		case "custom":
			objectID = 0
		default:
			rep.Log = append(rep.Log, fmt.Sprintf("Menu item “%s” skipped due to invalid menu item type.", it.Title))
			rep.Skipped++
			continue
		}
		parentOld, _ := strconv.ParseInt(it.meta("_menu_item_menu_item_parent"), 10, 64)
		mi := &store.EditItem{MenuID: ref.menu, Kind: kind, Object: it.meta("_menu_item_object"), TargetID: objectID,
			URL: it.meta("_menu_item_url"), Label: it.Title, Target: it.meta("_menu_item_target"),
			Classes: strings.Join(phpUnserializeStrings(it.meta("_menu_item_classes")), " "), XFN: it.meta("_menu_item_xfn"),
			AttrTitle: it.Excerpt, Description: it.Content, Order: it.MenuOrder, Status: "publish"}
		if parentOld != 0 {
			if pid, ok := menuItemIDs[parentOld]; ok {
				mi.ParentID = pid
			}
		}
		if err := s.store.AddMenuItem(ctx, mi); err != nil {
			rep.Log = append(rep.Log, fmt.Sprintf("Failed to import menu item “%s”: %v", it.Title, err))
			continue
		}
		menuItemIDs[it.PostID] = mi.ID
		if parentOld != 0 && mi.ParentID == 0 {
			menuOrphans[mi.ID] = parentOld
		}
		rep.MenuItems++
	}
	for id, parentOld := range menuOrphans {
		if pid, ok := menuItemIDs[parentOld]; ok {
			_, _ = s.store.Pool.Exec(ctx, `UPDATE menu_items SET parent_id = $2 WHERE id = $1`, id, pid)
		}
	}
	var all []int64
	for _, id := range termID {
		all = append(all, id)
	}
	_ = s.store.RecountTerms(ctx, all)
	return rep, nil
}

func wxrTypeLabel(typ string) string {
	switch typ {
	case "wp_block":
		return "Pattern"
	}
	return ucfirstStr(typ)
}

func ucfirstStr(v string) string {
	if v == "" {
		return v
	}
	return strings.ToUpper(v[:1]) + v[1:]
}

// Remote files -------------------------------------------------------------------------

// wxrFetchTransport, when set, carries attachment downloads (tests point it at an httptest server).
// nil = a transport that only connects to public addresses.
var wxrFetchTransport http.RoundTripper

const wxrFetchTimeout = 30 * time.Second

// wxrPublicIP refuses loopback, private, link-local, multicast and unspecified addresses.
func wxrPublicIP(ip net.IP) bool {
	return ip != nil && !ip.IsLoopback() && !ip.IsPrivate() && !ip.IsLinkLocalUnicast() && !ip.IsLinkLocalMulticast() &&
		!ip.IsMulticast() && !ip.IsUnspecified() && !ip.IsInterfaceLocalMulticast()
}

// wxrCheckURL allows only http(s) URLs without credentials.
func wxrCheckURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return nil, errors.New("Invalid URL.")
	}
	return u, nil
}

// wxrDial resolves the host and connects only to a public address (the dialed address is the checked one).
func wxrDial(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	d := net.Dialer{Timeout: 10 * time.Second}
	for _, ip := range ips {
		if !wxrPublicIP(ip.IP) {
			return nil, fmt.Errorf("Remote file %q resolves to a private address.", host)
		}
	}
	var last error
	for _, ip := range ips {
		conn, err := d.DialContext(ctx, network, net.JoinHostPort(ip.IP.String(), port))
		if err == nil {
			return conn, nil
		}
		last = err
	}
	if last == nil {
		last = errors.New("could not resolve host")
	}
	return nil, last
}

// wxrFetch downloads one remote file (http(s) only, public hosts only, at most 5 redirects, 30 s).
func wxrFetch(ctx context.Context, raw string) (*http.Response, error) {
	u, err := wxrCheckURL(raw)
	if err != nil {
		return nil, err
	}
	rt := wxrFetchTransport
	if rt == nil {
		rt = &http.Transport{DialContext: wxrDial, Proxy: nil, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: wxrFetchTimeout}
	}
	client := &http.Client{Transport: rt, Timeout: wxrFetchTimeout, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("Too many redirects.")
		}
		_, err := wxrCheckURL(req.URL.String())
		return err
	}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "WordPress Go/"+Version)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("Remote server returned the following unexpected result: %s", resp.Status)
	}
	return resp, nil
}

// sideloadURL is media_sideload_image() / the importer's fetch_remote_file(): the file is downloaded
// into the uploads directory, image sizes are generated and the attachment is inserted.
func (s *Server) sideloadURL(ctx context.Context, rawURL string, uploaderID, postID int64) (*store.Attachment, error) {
	if strings.TrimSpace(rawURL) == "" {
		return nil, errors.New("Invalid file URL.")
	}
	resp, err := wxrFetch(ctx, rawURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	u, _ := url.Parse(rawURL)
	name := path.Base(u.Path)
	if name == "" || name == "/" || name == "." {
		return nil, errors.New("Invalid file name.")
	}
	st, err := media.Save(s.cfg.UploadsDir, s.settings.Bool("uploads_use_yearmonth_folders"), time.Now(), name, io.LimitReader(resp.Body, maxUploadBytes+1))
	if err != nil {
		return nil, err
	}
	if st.Size > maxUploadBytes {
		_ = os.Remove(filepath.Join(s.cfg.UploadsDir, filepath.FromSlash(st.RelPath)))
		return nil, errors.New("Remote file is too large, limit is " + media.HumanSize(maxUploadBytes) + ".")
	}
	if st.Size == 0 {
		_ = os.Remove(filepath.Join(s.cfg.UploadsDir, filepath.FromSlash(st.RelPath)))
		return nil, errors.New("Zero size file downloaded.")
	}
	title := strings.TrimSuffix(filepath.Base(st.Original), filepath.Ext(st.Original))
	a := &store.Attachment{UploaderID: uploaderID, PostID: postID, FilePath: st.RelPath, Mime: st.Mime, FileSize: st.Size,
		Title: title, Slug: wp.SanitizeTitle(title), Meta: map[string]any{"filesize": st.Size}}
	if strings.HasPrefix(st.Mime, "image/") {
		if info, err := media.ProcessImage(s.cfg.UploadsDir, st.RelPath, s.imageSizes()); err == nil {
			a.Width, a.Height = info.Width, info.Height
			a.FilePath = info.RelPath
			a.FileSize = info.FileSize
			a.Meta["filesize"] = info.FileSize
			if info.OriginalFile != "" {
				a.Meta["original_image"] = info.OriginalFile
			}
			dir := path.Dir(info.RelPath)
			for _, v := range info.Variants {
				a.Variants = append(a.Variants, store.AttachmentVariant{Name: v.Name, FilePath: path.Join(dir, v.File),
					Width: v.Width, Height: v.Height, Mime: v.Mime, FileSize: v.Size})
			}
		}
	}
	if err := s.store.InsertAttachment(ctx, a); err != nil {
		return nil, err
	}
	return a, nil
}
