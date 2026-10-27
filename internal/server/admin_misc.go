package server

import (
	"context"
	"fmt"
	"html/template"
	"io/fs"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"wpgo/internal/media"
	"wpgo/internal/store"
)

// updateCore is update-core.php. WordPress Go ships as one binary, so updates mean replacing it.
func (s *Server) updateCore(c *gin.Context) {
	if !currentUser(c).Can("update_core", nil) {
		c.String(http.StatusForbidden, "Sorry, you are not allowed to update this site.")
		return
	}
	s.adminPage(c, "update-core", "WordPress Go Updates", "update-core-php", "admin/update-core", map[string]any{
		"Version": Version, "Checked": time.Now().In(s.loc()).Format("January 2, 2006 at 3:04 pm"), "Plugins": s.installedPlugins(c), "Themes": s.installedThemes()})
}

// about is about.php: what is new in WordPress Go.
func (s *Server) about(c *gin.Context) {
	s.adminPage(c, "about", "About WordPress Go", "about-php", "admin/about", map[string]any{"Version": Version, "Go": runtime.Version(), "Golementor": s.golementorOn()})
}

// tools is tools.php.
func (s *Server) tools(c *gin.Context) {
	s.adminPage(c, "tools", "Tools", "tools-php", "admin/tools", nil)
}

// optionsPrivacy is options-privacy.php: the privacy policy page setting.
func (s *Server) optionsPrivacy(c *gin.Context) {
	u := currentUser(c)
	if !u.Can("manage_options", nil) {
		c.String(http.StatusForbidden, "Sorry, you are not allowed to manage privacy options on this site.")
		return
	}
	if c.Query("tab") == "policyguide" { // the Policy Guide tab (privacy-policy-guide.php, admin_tools2.go)
		s.privacyPolicyGuide(c)
		return
	}
	if c.Request.Method == http.MethodPost {
		if !s.checkNonce(c, "set-privacy-page") {
			s.denyNonce(c)
			return
		}
		id, _ := strconv.Atoi(c.PostForm("page_for_privacy_policy"))
		if c.PostForm("action") == "create-privacy-page" {
			p := &store.Post{Type: "page", Status: "draft", AuthorID: u.ID, Title: "Privacy Policy", Content: privacyPolicyContent}
			if err := s.store.InsertPost(c, p); err == nil {
				id = int(p.ID)
			}
		}
		_ = s.settings.Set(c, map[string]any{"wp_page_for_privacy_policy": id})
		s.purgeAll(c)
		c.Redirect(http.StatusFound, "/wp-admin/options-privacy?settings-updated=true")
		return
	}
	pages, _, _ := s.store.QueryPosts(c, store.PostQuery{Types: []string{"page"}, Statuses: []string{"publish", "draft"}, PerPage: 200, Page: 1, OrderBy: "title", Order: "ASC"})
	current := int64(s.settings.Int("wp_page_for_privacy_policy"))
	var cur *store.Post
	for _, p := range pages {
		if p.ID == current {
			cur = p
		}
	}
	d := map[string]any{"Pages": pages, "Current": current, "CurrentPost": cur, "Nonce": s.nonce(c, "set-privacy-page")}
	if cur != nil {
		d["EditURL"] = "/wp-admin/post?post=" + itoa64(cur.ID) + "&action=edit"
		d["ViewURL"] = s.links(c).Permalink(cur)
	}
	s.adminPage(c, "options-privacy", "Privacy", "options-privacy-php privacy-settings", "admin/options-privacy", d)
}

// themeInstall is theme-install.php: a small theme directory (bundled themes plus coming-soon listings).
func (s *Server) themeInstall(c *gin.Context) {
	if !currentUser(c).Can("install_themes", nil) {
		c.String(http.StatusForbidden, "Sorry, you are not allowed to install themes on this site.")
		return
	}
	active := s.activeTheme()
	type card struct {
		Name, Author, Screenshot, Slug string
		Installed, Active, Demo        bool
		Description, Initials, Color   string
		ActivateURL, PreviewURL        string
		Custom                         bool
	}
	browse := c.DefaultQuery("browse", "all")
	search := strings.ToLower(strings.TrimSpace(c.Query("search")))
	match := func(cd card) bool {
		return search == "" || strings.Contains(strings.ToLower(cd.Name+" "+cd.Author+" "+cd.Description), search)
	}
	var cards, demos []card
	for _, t := range s.installedThemes() {
		cd := card{Name: t.Name, Author: t.Author, Screenshot: t.ScreenshotSrc(), Slug: t.Slug, Installed: true, Active: t.Slug == active.Slug, Description: t.Description,
			ActivateURL: "/wp-admin/themes?action=activate&stylesheet=" + t.Slug + "&_wpnonce=" + s.nonce(c, "switch-theme_"+t.Slug), PreviewURL: "/?theme=" + t.Slug, Custom: t.Custom}
		if !match(cd) {
			continue
		}
		if cd.Active {
			cards = append([]card{cd}, cards...)
		} else {
			cards = append(cards, cd)
		}
	}
	for _, d := range []card{
		{Name: "Gopher Magazine", Color: "blue", Description: "A news and magazine layout with a bold front page grid."},
		{Name: "Burrow Portfolio", Color: "green", Description: "Full-bleed galleries and case studies for creative work."},
		{Name: "Channel Shop", Color: "orange", Description: "A storefront theme built around product grids and a mini cart."},
		{Name: "Goroutine Docs", Color: "purple", Description: "Documentation theme with a sidebar table of contents and search."},
	} {
		d.Author, d.Demo = "WordPress Go fans", true
		d.Slug = strings.ToLower(strings.ReplaceAll(d.Name, " ", "-"))
		for _, w := range strings.Fields(d.Name) {
			d.Initials += w[:1]
		}
		if match(d) {
			demos = append(demos, d)
		}
	}
	switch browse {
	case "installed":
		demos = nil
	case "demos":
		cards = nil
	default:
		browse = "all"
	}
	data := map[string]any{"Cards": cards, "Demos": demos, "Browse": browse, "Search": c.Query("search"), "Count": len(cards) + len(demos), "Golementor": s.golementorOn()}
	s.adminPage(c, "themes", "Add Themes", "theme-install-php", "admin/theme-install", data)
}

// Site Health --------------------------------------------------------------------

type healthTest struct {
	Label, Status, Badge, Description string // status: good, recommended, critical
	Action                            template.HTML
	Slug                              string // accordion id suffix (health-check-accordion-block-<slug>)
}

type healthSection struct {
	Title  string
	Fields [][2]string
}

// healthSlug turns a label into the id WordPress gives a test or Info section.
func healthSlug(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

func (s *Server) siteHealth(c *gin.Context) {
	if !currentUser(c).Can("manage_options", nil) {
		c.String(http.StatusForbidden, "Sorry, you are not allowed to access the debug data.")
		return
	}
	ctx := c.Request.Context()
	var tests []healthTest
	add := func(t healthTest) { tests = append(tests, t) }

	add(healthTest{Label: "Your site is running the current version of WordPress Go (" + Version + ")", Status: "good", Badge: "Performance",
		Description: "WordPress Go is a single compiled binary; updating means replacing it and restarting."})
	add(healthTest{Label: "Your site is running on " + runtime.Version(), Status: "good", Badge: "Performance",
		Description: "The Go runtime is compiled into the binary. There is no interpreter to keep up to date."})
	var pgVersion string
	_ = s.store.Pool.QueryRow(ctx, `SHOW server_version`).Scan(&pgVersion)
	pgMajor, _ := strconv.Atoi(strings.SplitN(pgVersion, ".", 2)[0])
	if pgMajor >= 15 {
		add(healthTest{Label: "PostgreSQL " + pgVersion + " is supported", Status: "good", Badge: "Performance", Description: "The database server is a recent, supported release."})
	} else {
		add(healthTest{Label: "PostgreSQL " + pgVersion + " is old", Status: "recommended", Badge: "Performance", Description: "PostgreSQL 15 or newer is recommended."})
	}
	if strings.HasPrefix(s.links(c).Home, "https://") {
		add(healthTest{Label: "Your website is using an active HTTPS connection", Status: "good", Badge: "Security", Description: "An HTTPS connection helps keep your site and its visitors safe."})
	} else {
		add(healthTest{Label: "Your website does not use HTTPS", Status: "recommended", Badge: "Security",
			Description: "HTTPS is already supported by most hosts and makes your site more secure. Put WordPress Go behind a TLS-terminating proxy and set WPGO_SITE_URL to an https:// address."})
	}
	if f, err := os.CreateTemp(s.cfg.UploadsDir, ".health-*"); err == nil {
		f.Close()
		os.Remove(f.Name())
		add(healthTest{Label: "The uploads directory is writable", Status: "good", Badge: "Performance", Description: "Media files can be uploaded."})
	} else {
		add(healthTest{Label: "The uploads directory is not writable", Status: "critical", Badge: "Performance", Description: "Uploads will fail: " + err.Error()})
	}
	if s.settings.Bool("wpgo_cache_enabled") {
		st := s.currentCache().Stats()
		add(healthTest{Label: "Page cache is enabled", Status: "good", Badge: "Performance",
			Description: fmt.Sprintf("WPGo Cache serves pages from memory (%d cached, %.0f%% hit ratio since start).", st.Entries, st.HitRatio*100)})
	} else {
		add(healthTest{Label: "Page cache is not enabled", Status: "recommended", Badge: "Performance",
			Description: "Page cache can improve your site performance by caching pages in memory.", Action: `<a href="/wp-admin/admin?page=wpgo-cache-settings">Turn on WPGo Cache</a>`})
	}
	if s.plugins != nil && s.plugins.initErr == nil {
		add(healthTest{Label: "The plugin sandbox is running", Status: "good", Badge: "Security", Description: "Plugins run as WebAssembly with only the permissions you granted."})
	} else {
		add(healthTest{Label: "The plugin sandbox could not start", Status: "critical", Badge: "Security", Description: "Wasm plugins are disabled."})
	}
	if s.settings.String("timezone_string") != "" {
		add(healthTest{Label: "Your site uses a named time zone", Status: "good", Badge: "Performance", Description: "Dates follow daylight saving changes automatically."})
	} else {
		add(healthTest{Label: "Your site uses a fixed UTC offset", Status: "recommended", Badge: "Performance",
			Description: "Choose a city-based time zone so dates follow daylight saving time.", Action: `<a href="/wp-admin/options-general">General Settings</a>`})
	}
	if !s.settings.Bool("blog_public") {
		add(healthTest{Label: "Search engines are discouraged from indexing this site", Status: "recommended", Badge: "Privacy",
			Description: "Your site is hidden from search engines.", Action: `<a href="/wp-admin/options-reading">Reading Settings</a>`})
	}
	var inactivePlugins int
	_ = s.store.Pool.QueryRow(ctx, `SELECT count(*) FROM plugins WHERE status = 'inactive'`).Scan(&inactivePlugins)
	if inactivePlugins > 0 {
		add(healthTest{Label: "You should remove inactive plugins", Status: "recommended", Badge: "Security",
			Description: fmt.Sprintf("Your site has %d inactive plugin(s). Inactive plugins are not run, but removing what you don't use keeps things tidy.", inactivePlugins),
			Action:      `<a href="/wp-admin/plugins?plugin_status=inactive">Manage inactive plugins</a>`})
	}
	if s.settings.Int("wp_page_for_privacy_policy") == 0 {
		add(healthTest{Label: "No privacy policy page is set", Status: "recommended", Badge: "Privacy", Action: `<a href="/wp-admin/options-privacy">Privacy Settings</a>`})
	}

	var byStatus = map[string][]healthTest{}
	for i, t := range tests {
		t.Slug = healthSlug(t.Label)
		if t.Slug == "" {
			t.Slug = "test-" + strconv.Itoa(i)
		}
		st := t.Status
		if st != "good" && st != "recommended" {
			st = "critical"
		}
		byStatus[st] = append(byStatus[st], t)
	}
	good, rec, crit := len(byStatus["good"]), len(byStatus["recommended"]), len(byStatus["critical"])
	// site-health.js recalculateProgression().
	total := float64(good) + float64(rec) + 1.5*float64(crit)
	failed := 0.5*float64(rec) + 1.5*float64(crit)
	val := 100
	if total > 0 {
		val = 100 - int(math.Ceil(failed/total*100))
	}
	val = max(0, min(100, val))
	progressClass, progressLabel := "orange", "Should be improved"
	if val >= 80 && crit == 0 {
		progressClass, progressLabel = "green", "Good"
	}
	type infoSection struct {
		healthSection
		Slug string
	}
	var info []infoSection
	var copyText strings.Builder
	copyText.WriteString("`\n")
	for _, sec := range s.healthInfo(ctx) {
		info = append(info, infoSection{sec, healthSlug(sec.Title)})
		copyText.WriteString("### " + healthSlug(sec.Title) + " ###\n\n")
		for _, f := range sec.Fields {
			copyText.WriteString(f[0] + ": " + f[1] + "\n")
		}
		copyText.WriteString("\n")
	}
	copyText.WriteString("`")
	d := map[string]any{"Critical": byStatus["critical"], "Recommended": byStatus["recommended"], "Good": byStatus["good"],
		"GoodCount": good, "RecommendedCount": rec, "CriticalCount": crit,
		"ProgressClass": progressClass, "ProgressLabel": progressLabel, "DashOffset": fmt.Sprintf("%.3f", float64(100-val)/100*math.Pi*2*90),
		"Tab": c.DefaultQuery("tab", "status"), "Info": info, "CopyText": copyText.String()}
	s.adminPage(c, "site-health", "Site Health", "site-health-php", "admin/site-health", d)
}

func (s *Server) healthInfo(ctx context.Context) []healthSection {
	var dbSize, pgVersion string
	_ = s.store.Pool.QueryRow(ctx, `SELECT pg_size_pretty(pg_database_size(current_database())), version()`).Scan(&dbSize, &pgVersion)
	var posts, users, comments, attachments int
	_ = s.store.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM posts), (SELECT count(*) FROM users), (SELECT count(*) FROM comments), (SELECT count(*) FROM media)`).Scan(&posts, &users, &comments, &attachments)
	var uploadBytes int64
	_ = filepath.WalkDir(s.cfg.UploadsDir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if fi, err := d.Info(); err == nil {
				uploadBytes += fi.Size()
			}
		}
		return nil
	})
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	t := s.activeTheme()
	var pluginList []string
	if s.plugins != nil && s.plugins.rt != nil {
		for _, p := range s.plugins.rt.Plugins() {
			pluginList = append(pluginList, p.Manifest().Name+" "+p.Manifest().Version)
		}
	}
	host, _ := os.Hostname()
	return []healthSection{
		{"WordPress Go", [][2]string{{"Version", Version}, {"Site URL", s.settings.String("siteurl")}, {"Home URL", s.settings.String("home")},
			{"Permalink structure", defaultStr(s.settings.String("permalink_structure"), "No permalink structure set")},
			{"Is this site discouraging search engines?", map[bool]string{true: "No", false: "Yes"}[s.settings.Bool("blog_public")]},
			{"Default comment status", s.settings.String("default_comment_status")}, {"Environment type", "production"},
			{"User count", strconv.Itoa(users)}, {"Content items", strconv.Itoa(posts)}, {"Comments", strconv.Itoa(comments)}}},
		{"Active Theme", [][2]string{{"Name", t.Name + " (" + t.Slug + ")"}, {"Version", t.Version}, {"Author", t.Author}, {"Widget areas", strconv.Itoa(len(t.WidgetAreas))}}},
		{"Plugins", [][2]string{{"Built-in", "Classic Editor 1.6.7, WPGo Cache 1.0.0"}, {"Active Wasm plugins", defaultStr(strings.Join(pluginList, ", "), "None")}}},
		{"Media Handling", [][2]string{{"Image library", "Go image (golang.org/x/image) – JPEG, PNG, GIF, WebP decode"}, {"Max upload size", media.HumanSize(maxUploadBytes)},
			{"Attachments", strconv.Itoa(attachments)}, {"Uploads size", media.HumanSize(uploadBytes)}}},
		{"Server", [][2]string{{"Server architecture", runtime.GOOS + " " + runtime.GOARCH}, {"Host name", host}, {"Go version", runtime.Version()},
			{"CPUs", strconv.Itoa(runtime.NumCPU())}, {"Goroutines", strconv.Itoa(runtime.NumGoroutine())},
			{"Memory in use", media.HumanSize(int64(ms.Alloc))}, {"Web server", "net/http (built in)"}}},
		{"Database", [][2]string{{"Extension", "pgx/v5"}, {"Server version", pgVersion}, {"Database size", dbSize}}},
		{"Scheduled Events", s.healthEvents(ctx)},
		{"Mail", [][2]string{{"Transport", map[bool]string{true: "SMTP " + s.cfg.SMTPHost + ":" + s.cfg.SMTPPort, false: "Development: messages are saved as .eml files"}[s.cfg.SMTPHost != ""]}, {"From", s.mailer().From}}},
		{"Filesystem", [][2]string{{"Uploads directory", s.cfg.UploadsDir}, {"Web assets", map[bool]string{true: "embedded in the binary", false: s.cfg.WebDir}[s.cfg.WebDir == ""]}}},
	}
}

// healthEvents lists the job runner's recurring events (like WP Crontrol) and the queue.
func (s *Server) healthEvents(ctx context.Context) [][2]string {
	var out [][2]string
	rows, err := s.store.Pool.Query(ctx, `SELECT name, recurrence, run_at, status, last_error FROM jobs WHERE recurrence <> '' ORDER BY name`)
	if err == nil {
		for rows.Next() {
			var name, rec, status, lastErr string
			var at time.Time
			if rows.Scan(&name, &rec, &at, &status, &lastErr) == nil {
				v := rec + ", next run " + at.In(s.loc()).Format("2006-01-02 15:04")
				if lastErr != "" {
					v += " (last error: " + lastErr + ")"
				}
				out = append(out, [2]string{name, v})
			}
		}
		rows.Close()
	}
	var queued, failed int
	_ = s.store.Pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE status = 'queued' AND recurrence = ''), count(*) FILTER (WHERE status = 'failed') FROM jobs`).Scan(&queued, &failed)
	out = append(out, [2]string{"Queued one-off jobs", strconv.Itoa(queued)}, [2]string{"Failed jobs", strconv.Itoa(failed)})
	return out
}
