package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"html/template"
	"net/http"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"

	"wpgo/internal/store"
)

// Pursae SEO is a native plugin like Classic Editor, Pursae Cache and Pursae Web Builder: the union of Yoast SEO
// (Premium) and Rank Math (Pro) in one WordPress-style UI. It is off on a fresh install; while it is
// off its routes answer 404 and the site, the editors and the admin menu are exactly as without it.
// The switch is the option native_seogo; every setting lives in the one JSON option seogo_options.
// Post meta uses _seogo_* keys and falls back to the _yoast_wpseo_* and rank_math_* keys an import
// from WordPress brings along (seogo_meta.go).

const (
	seogoSlug       = "seo-go"
	seogoSetting    = "native_seogo"
	seogoOptionsKey = "seogo_options"
)

// seogoOn reports whether the Pursae SEO plugin is active: one cached option read.
func (s *Server) seogoOn() bool {
	return s.settings.Bool(seogoSetting)
}

// seogoOnly is the middleware of Pursae SEO's own routes: 404 while the plugin is off.
func (s *Server) seogoOnly() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !s.seogoOn() {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		c.Next()
	}
}

// seogoTypeOpts are the Search Appearance settings of one post type, taxonomy or archive.
type seogoTypeOpts struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	Noindex     bool   `json:"noindex"`
	Disabled    bool   `json:"disabled,omitempty"` // author and date archives: redirect to the home page
	SchemaPage  string `json:"schema_page,omitempty"`
	SchemaArt   string `json:"schema_article,omitempty"`
	Sitemap     bool   `json:"sitemap"`
}

// seogoOptions is every Pursae SEO setting (seogo_options).
type seogoOptions struct {
	WizardDone bool            `json:"wizard_done"`
	Modules    map[string]bool `json:"modules"`

	Separator         string                   `json:"separator"`
	Types             map[string]seogoTypeOpts `json:"types"` // post, page, category, post_tag, author, date, search, 404, home
	NoindexEmptyTerms bool                     `json:"noindex_empty_terms"`
	NoindexPaged      bool                     `json:"noindex_paged"`
	RobotsDefaults    string                   `json:"robots_defaults"` // max-snippet / max-image-preview / max-video-preview
	HomeOGImage       string                   `json:"home_og_image"`

	// Site representation (Knowledge Graph)
	KGType        string   `json:"kg_type"` // organization, person
	KGName        string   `json:"kg_name"`
	KGAltName     string   `json:"kg_alt_name"`
	KGLogo        string   `json:"kg_logo"`
	KGPersonID    int64    `json:"kg_person"`
	Profiles      []string `json:"profiles"` // sameAs URLs
	TwitterUser   string   `json:"twitter_user"`
	FacebookURL   string   `json:"facebook_url"`
	FacebookAppID string   `json:"facebook_app_id"`

	// Social
	OpenGraph    bool   `json:"opengraph"`
	TwitterCards bool   `json:"twitter_cards"`
	TwitterCard  string `json:"twitter_card"` // summary_large_image, summary
	DefaultImage string `json:"default_image"`

	// Webmaster tools
	Verify map[string]string `json:"verify"` // google, bing, baidu, yandex, pinterest, naver

	// RSS
	RSSBefore string `json:"rss_before"`
	RSSAfter  string `json:"rss_after"`

	// Breadcrumbs
	BCSep       string `json:"bc_sep"`
	BCHome      string `json:"bc_home"`
	BCPrefix    string `json:"bc_prefix"`
	BCArchive   string `json:"bc_archive"`
	BCSearch    string `json:"bc_search"`
	BC404       string `json:"bc_404"`
	BCBoldLast  bool   `json:"bc_bold_last"`
	BCShowBlog  bool   `json:"bc_show_blog"`
	BCHideTitle bool   `json:"bc_hide_title"`

	// Table of Contents
	TOCTitle       string   `json:"toc_title"`
	TOCLevels      []int    `json:"toc_levels"`
	TOCNumbered    bool     `json:"toc_numbered"`
	TOCCollapsible bool     `json:"toc_collapsible"`
	TOCAuto        bool     `json:"toc_auto"`       // insert one before the first heading of singular content
	TOCAutoTypes   []string `json:"toc_auto_types"` // post types of the automatic insert
	TOCMin         int      `json:"toc_min"`        // headings the automatic insert needs

	// Sitemaps
	SitemapPerPage   int    `json:"sitemap_per_page"`
	SitemapImages    bool   `json:"sitemap_images"`
	SitemapExclude   string `json:"sitemap_exclude"`       // post IDs
	SitemapExcludeT  string `json:"sitemap_exclude_terms"` // term IDs
	SitemapNewsTypes string `json:"sitemap_news_name"`     // publication name of the news sitemap

	// robots.txt (empty: the generated default)
	RobotsTxt string `json:"robots_txt"`

	// Redirections and 404 monitor
	AutoRedirect  bool   `json:"auto_redirect"`
	Redirect404   string `json:"redirect_404"` // "" or a URL every 404 goes to (Rank Math "Redirect all 404s")
	MonitorLimit  int    `json:"monitor_limit"`
	MonitorIgnore string `json:"monitor_ignore"`

	// Image SEO
	ImgAlt   string `json:"img_alt"`   // template for missing alt, e.g. %filename%
	ImgTitle string `json:"img_title"` // template for missing title

	// IndexNow
	IndexNowKey string `json:"indexnow_key"`

	// Local SEO
	Local seogoLocal `json:"local"`

	// Role manager: role slug → granted Pursae SEO capabilities (administrators have them all)
	Roles map[string][]string `json:"roles"`
}

type seogoLocal struct {
	Type       string `json:"type"` // LocalBusiness subtype
	Name       string `json:"name"`
	Street     string `json:"street"`
	City       string `json:"city"`
	Region     string `json:"region"`
	Postal     string `json:"postal"`
	Country    string `json:"country"`
	Phone      string `json:"phone"`
	Email      string `json:"email"`
	PriceRange string `json:"price_range"`
	Lat        string `json:"lat"`
	Lng        string `json:"lng"`
	Hours      string `json:"hours"` // one "Mo-Fr 09:00-17:00" spec per line
}

// seogoModules are the switchable parts (Rank Math's Modules screen).
var seogoModules = []struct{ Key, Name, Desc, Icon string }{
	{"analysis", "Content Analysis", "SEO and readability analysis with traffic lights in both editors.", "dashicons-chart-bar"},
	{"schema", "Schema (Structured Data)", "The JSON-LD graph: WebSite, Organization or Person, WebPage, Article, BreadcrumbList, FAQ and HowTo blocks, custom JSON-LD.", "dashicons-editor-code"},
	{"sitemap", "XML Sitemaps", "sitemap_index.xml with one sitemap per post type, taxonomy and authors, image entries; replaces wp-sitemap.xml.", "dashicons-networking"},
	{"breadcrumbs", "Breadcrumbs", "Breadcrumb trail as a shortcode, a block and a template function, with BreadcrumbList schema.", "dashicons-menu"},
	{"toc", "Table of Contents", "A Table of Contents block that lists the headings of a post with jump links, optionally inserted automatically.", "dashicons-list-view"},
	{"redirections", "Redirections", "301, 302, 307, 410 and 451 redirects with exact, partial and regex matching; automatic redirects on slug changes.", "dashicons-randomize"},
	{"monitor", "404 Monitor", "Logs URLs that end in a 404 so they can be redirected.", "dashicons-dismiss"},
	{"links", "Link Counter", "Internal and external link counts, orphaned content and internal linking suggestions.", "dashicons-admin-links"},
	{"image_seo", "Image SEO", "Fills missing alt and title attributes of images from templates.", "dashicons-format-image"},
	{"local", "Local SEO", "LocalBusiness schema with address, opening hours and geo coordinates.", "dashicons-location"},
	{"news_sitemap", "News Sitemap", "A Google News sitemap of the posts of the last two days.", "dashicons-media-document"},
	{"video_sitemap", "Video Sitemap", "A video sitemap of posts that embed YouTube or Vimeo videos.", "dashicons-video-alt3"},
	{"indexnow", "Instant Indexing (IndexNow)", "Tells Bing, Yandex and other IndexNow engines when content is published or updated.", "dashicons-performance"},
	{"role_manager", "Role Manager", "Choose which roles may use which Pursae SEO features.", "dashicons-admin-users"},
}

// seogoCaps are the capabilities of the role manager (Rank Math's rank_math_* capabilities).
var seogoCaps = []struct{ Key, Label string }{
	{"seogo_titles", "Search Appearance"},
	{"seogo_general", "General Settings"},
	{"seogo_sitemap", "Sitemaps"},
	{"seogo_404_monitor", "404 Monitor"},
	{"seogo_redirections", "Redirections"},
	{"seogo_link_builder", "Link Builder"},
	{"seogo_role_manager", "Role Manager"},
	{"seogo_onpage_analysis", "On-Page Analysis"},
	{"seogo_onpage_general", "On-Page General Settings"},
	{"seogo_onpage_advanced", "On-Page Advanced Settings"},
	{"seogo_onpage_snippet", "On-Page Schema Settings"},
	{"seogo_onpage_social", "On-Page Social Settings"},
}

func seogoDefaults() *seogoOptions {
	return &seogoOptions{
		Modules: map[string]bool{"analysis": true, "schema": true, "sitemap": true, "breadcrumbs": true, "toc": true, "redirections": true,
			"monitor": true, "links": true, "image_seo": false, "local": false, "news_sitemap": false, "video_sitemap": false,
			"indexnow": false, "role_manager": true},
		Separator: "-",
		Types: map[string]seogoTypeOpts{
			"post":     {Title: "%title% %page% %sep% %sitename%", Description: "%excerpt%", Sitemap: true, SchemaPage: "WebPage", SchemaArt: "Article"},
			"page":     {Title: "%title% %page% %sep% %sitename%", Description: "%excerpt%", Sitemap: true, SchemaPage: "WebPage", SchemaArt: "None"},
			"category": {Title: "%term_title% Archives %page% %sep% %sitename%", Description: "%term_description%", Sitemap: true},
			"post_tag": {Title: "%term_title% Archives %page% %sep% %sitename%", Description: "%term_description%", Sitemap: true},
			"author":   {Title: "%name%, Author at %sitename% %page%", Sitemap: true},
			"date":     {Title: "%date% %page% %sep% %sitename%", Noindex: true},
			"search":   {Title: "You searched for %searchphrase% %page% %sep% %sitename%", Noindex: true},
			"404":      {Title: "Page not found %sep% %sitename%", Noindex: true},
			"home":     {Title: "%sitename% %page% %sep% %sitedesc%"},
			// Pursae Commerce (Goocommerce publishes its own Product JSON-LD, so no schema type here)
			"product":     {Title: "%title% %page% %sep% %sitename%", Description: "%excerpt%", Sitemap: true, SchemaPage: "WebPage", SchemaArt: "None"},
			"product_cat": {Title: "%term_title% Archives %page% %sep% %sitename%", Description: "%term_description%", Sitemap: true},
			"product_tag": {Title: "%term_title% Archives %page% %sep% %sitename%", Description: "%term_description%", Sitemap: true},
		},
		NoindexEmptyTerms: true,
		RobotsDefaults:    "max-snippet:-1, max-image-preview:large, max-video-preview:-1",
		KGType:            "organization",
		OpenGraph:         true, TwitterCards: true, TwitterCard: "summary_large_image",
		Verify:   map[string]string{},
		RSSAfter: "The post %postlink% appeared first on %bloglink%.",
		BCSep:    "»", BCHome: "Home", BCArchive: "Archives for", BCSearch: "You searched for", BC404: "Error 404: Page not found",
		BCShowBlog: true,
		TOCTitle:   "Table of Contents", TOCLevels: []int{2, 3, 4}, TOCNumbered: true, TOCAutoTypes: []string{"post"}, TOCMin: 3,
		SitemapPerPage: 1000, SitemapImages: true,
		AutoRedirect: true, MonitorLimit: 1000,
		ImgAlt: "%filename%", ImgTitle: "%filename%",
		Roles: map[string][]string{
			"editor": {"seogo_titles", "seogo_redirections", "seogo_404_monitor", "seogo_link_builder", "seogo_onpage_analysis",
				"seogo_onpage_general", "seogo_onpage_advanced", "seogo_onpage_snippet", "seogo_onpage_social"},
			"author":      {"seogo_onpage_analysis", "seogo_onpage_general", "seogo_onpage_snippet", "seogo_onpage_social"},
			"contributor": {"seogo_onpage_analysis", "seogo_onpage_general"},
		},
	}
}

// seogoRuntime is the per-server state: the parsed options, cached until the option changes.
type seogoRuntime struct {
	mu   sync.Mutex
	raw  string
	opts *seogoOptions
}

var seogoState sync.Map // *Server → *seogoRuntime

func (s *Server) seogoRT() *seogoRuntime {
	if v, ok := seogoState.Load(s); ok {
		return v.(*seogoRuntime)
	}
	v, _ := seogoState.LoadOrStore(s, &seogoRuntime{})
	return v.(*seogoRuntime)
}

// seogoOpts returns the options merged over the defaults; callers must not modify the result.
func (s *Server) seogoOpts() *seogoOptions {
	raw, _ := json.Marshal(s.settings.Get(seogoOptionsKey))
	rt := s.seogoRT()
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.opts != nil && rt.raw == string(raw) {
		return rt.opts
	}
	o := seogoDefaults()
	if string(raw) != "null" {
		_ = json.Unmarshal(raw, o)
	}
	def := seogoDefaults()
	// A stored JSON null (a partial import, a REST patch) wipes a default map; never write into nil.
	if o.Types == nil {
		o.Types = map[string]seogoTypeOpts{}
	}
	if o.Modules == nil {
		o.Modules = map[string]bool{}
	}
	if o.Roles == nil {
		o.Roles = map[string][]string{}
	}
	for k, v := range def.Types { // types added later keep their defaults
		if _, ok := o.Types[k]; !ok {
			o.Types[k] = v
		}
	}
	for k, v := range def.Modules {
		if _, ok := o.Modules[k]; !ok {
			o.Modules[k] = v
		}
	}
	if o.Verify == nil {
		o.Verify = map[string]string{}
	}
	if o.SitemapPerPage <= 0 || o.SitemapPerPage > 50000 {
		o.SitemapPerPage = 1000
	}
	rt.raw, rt.opts = string(raw), o
	return o
}

// seogoType is the Search Appearance entry of a key; a type without one (a custom post type, a
// product attribute) is indexed and in the sitemaps.
func (o *seogoOptions) seogoType(key string) seogoTypeOpts {
	if t, ok := o.Types[key]; ok {
		return t
	}
	return seogoTypeOpts{Sitemap: true}
}

// seogoClone is a deep copy of the options for editing.
func (o *seogoOptions) clone() *seogoOptions {
	b, _ := json.Marshal(o)
	out := &seogoOptions{}
	_ = json.Unmarshal(b, out)
	return out
}

func (s *Server) seogoSave(ctx context.Context, o *seogoOptions) error {
	b, _ := json.Marshal(o)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	if err := s.settings.Set(ctx, map[string]any{seogoOptionsKey: m}); err != nil {
		return err
	}
	s.purgeAll(ctx)
	return nil
}

// seogoModule reports whether the plugin is on and one of its modules enabled.
func (s *Server) seogoModule(key string) bool {
	return s.seogoOn() && s.seogoOpts().Modules[key]
}

// seogoCan is the role manager check: administrators (manage_options) may do everything.
func (s *Server) seogoCan(u *store.User, cap string) bool {
	if u == nil {
		return false
	}
	if u.Can("manage_options", nil) {
		return true
	}
	if !s.seogoOpts().Modules["role_manager"] {
		// Without the role manager the editors keep the on-page box, everything else is for admins.
		return strings.HasPrefix(cap, "seogo_onpage_") && u.Can("edit_posts", nil)
	}
	for _, r := range u.Roles {
		for _, granted := range s.seogoOpts().Roles[r] {
			if granted == cap {
				return true
			}
		}
	}
	return false
}

// seogoActivate switches the plugin on; the first activation creates the IndexNow key.
func (s *Server) seogoActivate(ctx context.Context) error {
	o := s.seogoOpts().clone()
	if o.IndexNowKey == "" {
		b := make([]byte, 16)
		_, _ = rand.Read(b)
		o.IndexNowKey = hex.EncodeToString(b)
		if err := s.seogoSave(ctx, o); err != nil {
			return err
		}
	}
	if err := s.settings.Set(ctx, map[string]any{seogoSetting: true}); err != nil {
		return err
	}
	s.purgeAll(ctx)
	return nil
}

// seogoDeactivate switches the plugin off. Settings, meta, redirects and the 404 log stay.
func (s *Server) seogoDeactivate(ctx context.Context) error {
	if err := s.settings.Set(ctx, map[string]any{seogoSetting: false}); err != nil {
		return err
	}
	s.purgeAll(ctx)
	return nil
}

// seogoPluginRow is Pursae SEO's row on the Plugins screen.
func (s *Server) seogoPluginRow() pluginRow {
	r := pluginRow{Slug: seogoSlug, Name: "Pursae SEO", Version: builtinPluginVersion, Like: "Rank Math", Native: true, Active: s.seogoOn(),
		Description: "Search engine optimization in one place: SEO and readability analysis in both editors, titles and meta, social previews, the schema graph, XML sitemaps, breadcrumbs, redirections, a 404 monitor, internal links and more. While it is off, none of it is part of the site.",
		Author:      "Pursae"}
	if r.Active {
		r.SettingsURL = "/wp-admin/seogo"
	}
	return r
}

// seogoPluginAction is the Activate / Deactivate link of the Plugins screen.
func (s *Server) seogoPluginAction(c *gin.Context, action string) {
	var err error
	if action == "activate" {
		err = s.seogoActivate(c)
	} else {
		err = s.seogoDeactivate(c)
	}
	if err != nil {
		c.Redirect(http.StatusFound, "/wp-admin/plugins?error="+template.URLQueryEscaper(err.Error()))
		return
	}
	// The first activation asks whether to start the setup wizard (plugin_setup.go).
	c.Redirect(http.StatusFound, "/wp-admin/plugins?"+action+"=true")
}

// seogoMenuDef is the Pursae SEO top-level menu (after Settings, like Yoast SEO and Rank Math).
func (s *Server) seogoMenuDef() menuDef {
	sub := []subDef{
		{slug: "seogo", title: "Dashboard", cap: "seogo_general"},
		{slug: "seogo?page=general", title: "General Settings", cap: "seogo_general"},
		{slug: "seogo?page=titles", title: "Search Appearance", cap: "seogo_titles"},
		{slug: "seogo?page=social", title: "Social", cap: "seogo_titles"},
		{slug: "seogo?page=schema", title: "Schema", cap: "seogo_titles"},
		{slug: "seogo?page=sitemap", title: "Sitemaps", cap: "seogo_sitemap"},
		{slug: "seogo?page=redirections", title: "Redirections", cap: "seogo_redirections"},
		{slug: "seogo?page=404", title: "404 Monitor", cap: "seogo_404_monitor"},
		{slug: "seogo?page=links", title: "Link Counter", cap: "seogo_link_builder"},
		{slug: "seogo?page=bulk", title: "Bulk Editor", cap: "seogo_titles"},
		{slug: "seogo?page=search-console", title: "Search Console", cap: "seogo_general"},
		{slug: "seogo?page=tools", title: "Tools", cap: "seogo_general"},
		{slug: "seogo?page=roles", title: "Role Manager", cap: "seogo_role_manager"},
	}
	return menuDef{slug: "seogo", title: "Pursae SEO", cap: "seogo_onpage_general", id: "toplevel_page_seogo", icon: "dashicons-chart-line", sub: sub}
}

// seogoMenu adds the Pursae SEO menu to a built admin menu (adminPage), only while the plugin is on.
func (s *Server) seogoMenu(menu []*menuTop, u *store.User, screen string) []*menuTop {
	if !s.seogoOn() || u == nil {
		return menu
	}
	d := s.seogoMenuDef()
	mods := s.seogoOpts().Modules
	need := map[string]string{"seogo?page=sitemap": "sitemap", "seogo?page=redirections": "redirections", "seogo?page=404": "monitor",
		"seogo?page=links": "links", "seogo?page=roles": "role_manager", "seogo?page=schema": "schema"}
	top := &menuTop{Slug: d.slug, Title: template.HTML(d.title), ID: d.id, Icon: d.icon}
	for _, sd := range d.sub {
		if m, ok := need[sd.slug]; ok && !mods[m] {
			continue
		}
		if !s.seogoCan(u, sd.cap) {
			continue
		}
		ms := &menuSub{Slug: sd.slug, Title: template.HTML(sd.title), First: len(top.Sub) == 0, Current: sd.slug == screen}
		top.Current = top.Current || ms.Current
		top.Sub = append(top.Sub, ms)
	}
	if len(top.Sub) == 0 {
		return menu
	}
	top.Slug = top.Sub[0].Slug
	// After Settings, inside its group: Settings is no longer the last item of the group.
	at := len(menu)
	for i, m := range menu {
		if m.ID == "menu-settings" {
			at = i + 1
			m.Last = false
		}
	}
	top.Last = at == len(menu) || menu[at].Separator
	out := append([]*menuTop{}, menu[:at]...)
	out = append(out, top)
	return append(out, menu[at:]...)
}
