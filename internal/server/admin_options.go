package server

import (
	_ "embed"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"wpgo/internal/settings"
	"wpgo/internal/store"
	"wpgo/internal/wp"
)

//go:embed defaults/timezones.txt
var timezoneList string

// optionPages whitelists the settings each screen may write (register_setting()).
var optionPages = map[string][]string{
	"general": {"blogname", "blogdescription", "siteurl", "home", "admin_email", "users_can_register", "default_role",
		"timezone_string", "gmt_offset", "date_format", "time_format", "start_of_week"},
	"writing": {"default_category", "default_post_format", "classic-editor-replace", "classic-editor-allow-users"},
	"reading": {"show_on_front", "page_on_front", "page_for_posts", "posts_per_page", "posts_per_rss", "rss_use_excerpt", "blog_public"},
	"discussion": {"default_pingback_flag", "default_ping_status", "default_comment_status", "require_name_email", "comment_registration",
		"close_comments_for_old_posts", "close_comments_days_old", "show_comments_cookies_opt_in", "thread_comments", "thread_comments_depth",
		"page_comments", "comments_per_page", "default_comments_page", "comment_order", "comments_notify", "moderation_notify",
		"comment_moderation", "comment_previously_approved", "comment_max_links", "moderation_keys", "disallowed_keys",
		"show_avatars", "avatar_rating", "avatar_default"},
	"media": {"thumbnail_size_w", "thumbnail_size_h", "thumbnail_crop", "medium_size_w", "medium_size_h", "large_size_w", "large_size_h",
		"uploads_use_yearmonth_folders"},
	"permalink": {"permalink_structure", "category_base", "tag_base"},
}

var optionTitles = map[string]string{
	"general": "General Settings", "writing": "Writing Settings", "reading": "Reading Settings",
	"discussion": "Discussion Settings", "media": "Media Settings", "permalink": "Permalink Settings",
}

type choice struct {
	Value, Label, Example string
	Checked               bool
}

type optionsData struct {
	Page                     string
	Nonce                    string
	Timezones                []string
	TZ                       string // timezone_string, or "UTC+3" style for manual offsets
	DateFormats, TimeFormats []choice
	DateCustom, TimeCustom   bool
	Weekdays                 []string
	Categories               []*store.Term
	Pages                    []*store.Post
	Formats                  []string
	Permalinks               []choice
	PermCustom               bool
	Home                     string
	Now                      time.Time
	Roles                    []choice
	Depths                   []int
}

func (s *Server) optionsScreen(page string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !currentUser(c).Can("manage_options", nil) {
			c.String(http.StatusForbidden, "Sorry, you are not allowed to manage options for this site.")
			return
		}
		if slug := c.Query("page"); page == "general" && slug != "" && s.pluginSettingsScreen(c, slug) {
			return // Settings › <plugin> (plugins_host.go)
		}
		ctx := c.Request.Context()
		d := optionsData{Page: page, Nonce: s.nonce(c, page+"-options"), Home: s.links(ctx).HomeURL("/"), Now: time.Now()}
		loc := s.loc()
		switch page {
		case "general":
			d.Timezones = strings.Fields(timezoneList)
			d.TZ = s.settings.String("timezone_string")
			if d.TZ == "" {
				off := s.settings.Float("gmt_offset")
				d.TZ = "UTC"
				if off > 0 {
					d.TZ = "UTC+" + strconv.FormatFloat(off, 'f', -1, 64)
				} else if off < 0 {
					d.TZ = "UTC" + strconv.FormatFloat(off, 'f', -1, 64)
				}
			}
			df, tf := s.settings.String("date_format"), s.settings.String("time_format")
			now := time.Now().In(loc)
			d.DateCustom, d.TimeCustom = true, true
			for _, f := range []string{"F j, Y", "Y-m-d", "m/d/Y", "d/m/Y"} {
				d.DateFormats = append(d.DateFormats, choice{Value: f, Label: wp.FormatDate(now, f), Checked: f == df})
				if f == df {
					d.DateCustom = false
				}
			}
			for _, f := range []string{"g:i a", "g:i A", "H:i"} {
				d.TimeFormats = append(d.TimeFormats, choice{Value: f, Label: wp.FormatDate(now, f), Checked: f == tf})
				if f == tf {
					d.TimeCustom = false
				}
			}
			d.Weekdays = []string{"Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"}
			for _, r := range [][2]string{{"subscriber", "Subscriber"}, {"contributor", "Contributor"}, {"author", "Author"}, {"editor", "Editor"}, {"administrator", "Administrator"}} {
				d.Roles = append(d.Roles, choice{Value: r[0], Label: r[1], Checked: r[0] == s.settings.String("default_role")})
			}
		case "writing":
			d.Categories, _ = s.store.HierarchicalTerms(ctx, "category", false)
			d.Formats = []string{"0", "aside", "chat", "gallery", "link", "image", "quote", "status", "video", "audio"}
		case "reading":
			d.Pages, _, _ = s.store.QueryPosts(ctx, store.PostQuery{Types: []string{"page"}, Statuses: []string{"publish"}, OrderBy: "title", Order: "asc", PerPage: -1})
		case "discussion":
			for i := 2; i <= 10; i++ {
				d.Depths = append(d.Depths, i)
			}
		case "permalink":
			cur := s.settings.String("permalink_structure")
			sample := &store.Post{ID: 123, Type: "post", Slug: "sample-post", Status: "publish", PublishedAt: &d.Now}
			lb := s.links(ctx)
			d.PermCustom = true
			for _, p := range [][2]string{
				{"", "Plain"},
				{"/%year%/%monthnum%/%day%/%postname%/", "Day and name"},
				{"/%year%/%monthnum%/%postname%/", "Month and name"},
				{"/archives/%post_id%", "Numeric"},
				{"/%postname%/", "Post name"},
			} {
				lb.Structure = p[0]
				d.Permalinks = append(d.Permalinks, choice{Value: p[0], Label: p[1], Example: lb.Permalink(sample), Checked: p[0] == cur})
				if p[0] == cur {
					d.PermCustom = false
				}
			}
		}
		title := optionTitles[page]
		s.adminPage(c, "options-"+page, title, "options-"+page+"-php", "admin/options", d)
	}
}

// optionsSave is options.php: validates the nonce, writes only whitelisted keys and redirects back.
func (s *Server) optionsSave(c *gin.Context) {
	if !currentUser(c).Can("manage_options", nil) {
		c.String(http.StatusForbidden, "Sorry, you are not allowed to manage options for this site.")
		return
	}
	page := c.PostForm("option_page")
	if strings.HasPrefix(page, pluginSettingsSlug) {
		s.pluginSettingsSave(c, page) // plugin settings page (plugins_host.go)
		return
	}
	keys, ok := optionPages[page]
	if !ok {
		c.String(http.StatusBadRequest, "Error: The options page was not found in the allowed options list.")
		return
	}
	if !s.checkNonce(c, page+"-options") {
		s.denyNonce(c)
		return
	}
	values := map[string]any{}
	for _, k := range keys {
		raw, present := c.GetPostForm(k)
		def := settings.Defaults[k]
		switch def.(type) {
		case bool:
			values[k] = present && raw != "" && raw != "0"
		case int:
			if !present {
				continue
			}
			n, err := strconv.Atoi(strings.TrimSpace(raw))
			if err != nil {
				continue
			}
			if n < 0 {
				n = 0
			}
			values[k] = n
		case float64:
			if !present {
				continue
			}
			f, _ := strconv.ParseFloat(raw, 64)
			values[k] = f
		default:
			if !present {
				continue
			}
			values[k] = strings.TrimSpace(raw)
		}
	}

	switch page {
	case "general":
		// "Custom" radio buttons carry their value in a separate text field.
		if c.PostForm("date_format") == "\\c\\u\\s\\t\\o\\m" {
			values["date_format"] = strings.TrimSpace(c.PostForm("date_format_custom"))
		}
		if c.PostForm("time_format") == "\\c\\u\\s\\t\\o\\m" {
			values["time_format"] = strings.TrimSpace(c.PostForm("time_format_custom"))
		}
		// A manual UTC offset is stored as gmt_offset with an empty timezone_string.
		if tz := c.PostForm("timezone_string"); strings.HasPrefix(tz, "UTC") {
			off, _ := strconv.ParseFloat(strings.TrimPrefix(strings.TrimPrefix(tz, "UTC"), "+"), 64)
			values["timezone_string"] = ""
			values["gmt_offset"] = off
		} else if _, err := time.LoadLocation(tz); err != nil {
			delete(values, "timezone_string")
		}
		for _, k := range []string{"siteurl", "home"} {
			if v, ok := values[k].(string); ok {
				u, err := url.Parse(v)
				if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
					delete(values, k)
					continue
				}
				values[k] = strings.TrimRight(v, "/")
			}
		}
		if v, ok := values["admin_email"].(string); ok && !strings.Contains(v, "@") {
			delete(values, "admin_email")
		}
		if v, ok := values["default_role"].(string); ok && v == "administrator" {
			// WordPress allows it but warns; keep the choice.
			values["default_role"] = v
		}
	case "discussion":
		for _, k := range []string{"default_ping_status", "default_comment_status"} {
			values[k] = map[bool]string{true: "open", false: "closed"}[c.PostForm(k) == "open"]
		}
	case "reading":
		if values["show_on_front"] != "page" {
			values["show_on_front"] = "posts"
		}
		if v, ok := values["posts_per_page"].(int); ok && v < 1 {
			values["posts_per_page"] = 1
		}
		// The checkbox means "discourage", so blog_public is its inverse.
		values["blog_public"] = c.PostForm("blog_public") != "0"
	case "permalink":
		sel := c.PostForm("selection")
		if sel == "custom" {
			sel = strings.TrimSpace(c.PostForm("permalink_structure"))
		}
		if sel != "" {
			if !strings.HasPrefix(sel, "/") {
				sel = "/" + sel
			}
			if !strings.Contains(sel, "%") {
				sel = ""
			}
		}
		values["permalink_structure"] = sel
		values["category_base"] = strings.Trim(c.PostForm("category_base"), "/ ")
		values["tag_base"] = strings.Trim(c.PostForm("tag_base"), "/ ")
	}

	if err := s.settings.Set(c, values); err != nil {
		c.String(http.StatusInternalServerError, err.Error())
		return
	}
	s.purgeAll(c)
	target := "/wp-admin/options-" + page + "?settings-updated=true"
	c.Redirect(http.StatusFound, target)
}
