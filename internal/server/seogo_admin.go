package server

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"html"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"wpgo/internal/store"
	"wpgo/internal/wp"
)

// The Pursae SEO admin screens, all under /wp-admin/seogo?page=…: Dashboard (with the modules),
// Setup Wizard, General Settings, Search Appearance, Social, Schema, Sitemaps, Redirections,
// 404 Monitor, Link Counter, Bulk Editor, Tools and Role Manager.

func (s *Server) registerSEOGoAdmin(admin *gin.RouterGroup) {
	admin.GET("/seogo", s.seogoOnly(), s.seogoAdmin)
	admin.POST("/seogo", s.seogoOnly(), s.seogoAdmin)
}

type seogoPage struct {
	Page, Tab string
	Title     string
	Opts      *seogoOptions
	Nonce     string
	Modules   []seogoModuleRow
	Data      map[string]any
	Types     []seogoTypeRow
	Caps      []struct{ Key, Label string }
	Roles     []seogoRoleRow
	Home      string
	Vars      [][2]string
	PageTypes []string
	ArtTypes  []string
	Step      int
}

type seogoModuleRow struct {
	Key, Name, Desc, Icon string
	On                    bool
}

type seogoTypeRow struct {
	Key, Label string
	Opts       seogoTypeOpts
	Archive    bool // no schema defaults, maybe "disabled"
	Special    bool // search, 404, home: no noindex/sitemap switch
}

type seogoRoleRow struct {
	Slug, Name string
	Caps       map[string]bool
}

var seogoPageCaps = map[string]string{"": "seogo_general", "wizard": "seogo_general", "general": "seogo_general", "titles": "seogo_titles",
	"social": "seogo_titles", "schema": "seogo_titles", "sitemap": "seogo_sitemap", "redirections": "seogo_redirections",
	"404": "seogo_404_monitor", "links": "seogo_link_builder", "bulk": "seogo_titles", "tools": "seogo_general", "roles": "seogo_role_manager",
	"search-console": "seogo_general"}

var seogoPageTitles = map[string]string{"": "Dashboard", "wizard": "Setup Wizard", "general": "General Settings", "titles": "Search Appearance",
	"social": "Social", "schema": "Schema", "sitemap": "Sitemaps", "redirections": "Redirections", "404": "404 Monitor",
	"links": "Link Counter", "bulk": "Bulk Editor", "tools": "Tools", "roles": "Role Manager",
	"search-console": "Search Console"}

func (s *Server) seogoAdmin(c *gin.Context) {
	page := c.Query("page")
	cp, ok := seogoPageCaps[page]
	if !ok || !s.seogoCan(currentUser(c), cp) {
		c.String(http.StatusForbidden, "Sorry, you are not allowed to access this page.")
		return
	}
	if c.Request.Method == http.MethodPost {
		if !s.checkNonce(c, "seogo-"+page) {
			s.denyNonce(c)
			return
		}
		msg, err := s.seogoAdminPost(c, page)
		if err != nil {
			s.seogoBack(c, page, "error", err.Error())
			return
		}
		if msg == "" { // the handler answered (downloads)
			return
		}
		if page == "wizard" {
			return
		}
		s.seogoBack(c, page, "updated", msg)
		return
	}
	if page == "redirections" && c.Query("action") == "export" {
		s.seogoExportRedirects(c)
		return
	}
	if page == "search-console" && s.seogoGSCGet(c) { // connect and the OAuth callback (seogo_gsc.go)
		return
	}
	if page == "tools" && c.Query("action") == "export" {
		s.seogoExportSettings(c)
		return
	}
	d := &seogoPage{Page: page, Tab: c.Query("tab"), Title: seogoPageTitles[page], Opts: s.seogoOpts(), Nonce: s.nonce(c, "seogo-"+page),
		Data: map[string]any{}, Caps: seogoCaps, Home: s.links(c).HomeURL("/"), Vars: seogoVarList, PageTypes: seogoPageTypes, ArtTypes: seogoArtTypes}
	ctx := c.Request.Context()
	switch page {
	case "":
		for _, m := range seogoModules {
			d.Modules = append(d.Modules, seogoModuleRow{Key: m.Key, Name: m.Name, Desc: m.Desc, Icon: m.Icon, On: d.Opts.Modules[m.Key]})
		}
		d.Data = s.seogoDashboardStats(ctx)
	case "wizard":
		d.Step, _ = strconv.Atoi(c.DefaultQuery("step", "1"))
		if d.Step < 1 || d.Step > 5 {
			d.Step = 1
		}
		d.Data["Users"] = s.seogoAdmins(ctx)
		d.Data["Detected"] = s.seogoDetectImport(ctx)
	case "titles":
		d.Types = s.seogoTypeRows()
	case "general":
		if d.Tab == "toc" {
			type opt struct {
				Value, Label string
				On           bool
			}
			var lv, ty []opt
			for n := 1; n <= 6; n++ {
				lv = append(lv, opt{strconv.Itoa(n), "H" + strconv.Itoa(n), slices.Contains(d.Opts.seogoTOCDefaults().Levels, n)})
			}
			for _, t := range s.sitemapPostTypes(ctx) {
				ty = append(ty, opt{t, t, slices.Contains(d.Opts.TOCAutoTypes, t)})
			}
			d.Data["TOCLevels"], d.Data["TOCTypes"] = lv, ty
		}
	case "schema":
		d.Data["Users"] = s.seogoAdmins(ctx)
	case "sitemap":
		var links []string
		for _, t := range s.seogoSitemapTypes(ctx) {
			links = append(links, s.links(c).HomeURL("/"+t+"-sitemap.xml"))
		}
		d.Data["Index"] = s.links(c).HomeURL("/sitemap_index.xml")
		d.Data["Links"] = links
		d.Data["Public"] = s.settings.Bool("blog_public")
	case "redirections":
		rows, _ := s.seogoRedirectRows(ctx, false)
		if f := c.Query("status"); f != "" {
			var keep []*seogoRedirectRow
			for _, r := range rows {
				if r.Status == f {
					keep = append(keep, r)
				}
			}
			rows = keep
		}
		d.Data["Rows"] = rows
		d.Data["Filter"] = c.Query("status")
		edit := &seogoRedirectRow{Code: 301, MatchType: "exact", Status: "active", Source: c.Query("source")}
		if id, _ := strconv.ParseInt(c.Query("edit"), 10, 64); id > 0 {
			for _, r := range rows {
				if r.ID == id {
					edit = r
				}
			}
		}
		d.Data["Edit"] = edit
	case "404":
		d.Data["Rows"] = s.seogo404Rows(ctx, c.Query("s"))
		d.Data["Search"] = c.Query("s")
	case "links":
		rows := s.seogoLinkCounts(ctx)
		if c.Query("filter") == "orphaned" {
			var keep []seogoLinkRow
			for _, r := range rows {
				if r.Inbound == 0 {
					keep = append(keep, r)
				}
			}
			rows = keep
		}
		d.Data["Rows"] = rows
		d.Data["Filter"] = c.Query("filter")
	case "bulk":
		types := s.sitemapPostTypes(ctx)
		typ := c.DefaultQuery("post_type", "post")
		if !slices.Contains(types, typ) {
			typ = "post"
		}
		search := strings.TrimSpace(c.Query("s"))
		page, _ := strconv.Atoi(c.Query("paged"))
		page = max(page, 1)
		const per = 50
		rows, total := s.seogoBulkRows(ctx, typ, search, per, page)
		var links []map[string]string
		for _, t := range types {
			label := strings.ToUpper(t[:1]) + t[1:] + "s"
			switch t {
			case "post":
				label = "Posts"
			case "page":
				label = "Pages"
			}
			links = append(links, map[string]string{"Slug": t, "Label": label})
		}
		q := url.Values{"page": {"bulk"}, "post_type": {typ}}
		if search != "" {
			q.Set("s", search)
		}
		d.Data["Type"], d.Data["Rows"], d.Data["TypeLinks"], d.Data["Search"], d.Data["Paged"] = typ, rows, links, search, page
		d.Data["Pagination"] = newPagination(total, per, page, url.URL{Path: "/wp-admin/seogo", RawQuery: q.Encode()})
	case "tools":
		d.Data["Detected"] = s.seogoDetectImport(ctx)
		d.Data["RobotsDefault"] = s.seogoRobotsDefault(ctx)
		d.Data["IndexNow"] = s.seogoIndexNowLog()
	case "roles":
		d.Roles = s.seogoRoleRows(ctx)
	case "search-console":
		s.seogoGSCView(ctx, d)
	}
	if page == "wizard" {
		s.seogoRenderWizard(c, d)
		return
	}
	screen := "seogo"
	if page != "" {
		screen = "seogo?page=" + page
	}
	s.enqueueMedia(c, 0)
	s.adminPage(c, screen, d.Title+" ‹ Pursae SEO", "toplevel_page_seogo seogo-admin", "admin/seogo", d, seogoFlash(c)...)
}

func (s *Server) seogoBack(c *gin.Context, page, kind, msg string) {
	q := url.Values{}
	if page != "" {
		q.Set("page", page)
	}
	if t := c.PostForm("tab"); t != "" {
		q.Set("tab", t)
	}
	if t := c.PostForm("post_type"); t != "" && page == "bulk" {
		q.Set("post_type", t)
		for _, k := range []string{"s", "paged"} {
			if v := c.PostForm(k); v != "" {
				q.Set(k, v)
			}
		}
	}
	q.Set("seogo-"+kind, msg)
	c.Redirect(http.StatusFound, "/wp-admin/seogo?"+q.Encode())
}

// seogoFlash shows the message a POST redirected back with.
func seogoFlash(c *gin.Context) []notice {
	var out []notice
	if m := c.Query("seogo-updated"); m != "" {
		out = append(out, notice{Type: "success", Message: template.HTML(template.HTMLEscapeString(m)), Dismissible: true})
	}
	if m := c.Query("seogo-error"); m != "" {
		out = append(out, notice{Type: "error", Message: template.HTML(template.HTMLEscapeString(m)), Dismissible: true})
	}
	return out
}

func (s *Server) seogoTypeRows() []seogoTypeRow {
	o := s.seogoOpts()
	rows := []seogoTypeRow{
		{Key: "home", Label: "Homepage", Special: true},
		{Key: "post", Label: "Posts"}, {Key: "page", Label: "Pages"},
		{Key: "category", Label: "Categories", Archive: true}, {Key: "post_tag", Label: "Tags", Archive: true},
		{Key: "author", Label: "Author archives", Archive: true}, {Key: "date", Label: "Date archives", Archive: true},
		{Key: "search", Label: "Search results", Special: true}, {Key: "404", Label: "404 pages", Special: true},
	}
	if s.gooOn() { // Pursae Commerce
		rows = append(rows, seogoTypeRow{Key: "product", Label: "Products"},
			seogoTypeRow{Key: "product_cat", Label: "Product categories", Archive: true}, seogoTypeRow{Key: "product_tag", Label: "Product tags", Archive: true})
	}
	// Public Pursae Custom Fields post types: their entries and, with an archive, the archive.
	acfgoRegistry.RLock()
	var cpts []string
	for slug := range acfgoRegistry.types {
		cpts = append(cpts, slug)
	}
	acfgoRegistry.RUnlock()
	sort.Strings(cpts)
	for _, slug := range cpts {
		pt := siteCPT(slug)
		if pt == nil {
			continue
		}
		l := labelsFor(slug)
		rows = append(rows, seogoTypeRow{Key: slug, Label: l.Name})
		if pt.HasArchive {
			rows = append(rows, seogoTypeRow{Key: "pta_" + slug, Label: l.Name + " archive", Archive: true})
		}
	}
	for i := range rows {
		rows[i].Opts = o.seogoType(rows[i].Key)
	}
	return rows
}

func (s *Server) seogoAdmins(ctx context.Context) []*store.User {
	rows, err := s.store.Pool.Query(ctx, `SELECT u.id FROM users u JOIN user_roles ur ON ur.user_id = u.id JOIN roles r ON r.id = ur.role_id
		WHERE r.slug IN ('administrator','editor','author') GROUP BY u.id ORDER BY u.id`)
	if err != nil {
		return nil
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	var out []*store.User
	for _, id := range ids {
		if u, err := s.store.UserByID(ctx, id); err == nil {
			out = append(out, u)
		}
	}
	return out
}

func (s *Server) seogoRoleRows(ctx context.Context) []seogoRoleRow {
	rows, err := s.store.Pool.Query(ctx, `SELECT slug, name FROM roles WHERE slug <> 'administrator' ORDER BY id`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	o := s.seogoOpts()
	var out []seogoRoleRow
	for rows.Next() {
		var r seogoRoleRow
		if rows.Scan(&r.Slug, &r.Name) != nil {
			continue
		}
		r.Caps = map[string]bool{}
		for _, cp := range o.Roles[r.Slug] {
			r.Caps[cp] = true
		}
		out = append(out, r)
	}
	return out
}

// --- dashboard ---------------------------------------------------------------------------------

func (s *Server) seogoDashboardStats(ctx context.Context) map[string]any {
	out := map[string]any{}
	type bucket struct{ Good, OK, Bad, NA int }
	seo, read := bucket{}, bucket{}
	rows, err := s.store.Pool.Query(ctx, `SELECT coalesce(meta->>'_seogo_seo_score', meta->>'_yoast_wpseo_linkdex', ''), coalesce(meta->>'_seogo_read_score', meta->>'_yoast_wpseo_content_score', ''),
		coalesce(meta->>'_seogo_focus', meta->>'_yoast_wpseo_focuskw', meta->>'rank_math_focus_keyword', '')
		FROM posts WHERE type = ANY($1) AND status = 'publish'`, s.sitemapPostTypes(ctx))
	total := 0
	if err == nil {
		for rows.Next() {
			var sv, rv, fk string
			if rows.Scan(&sv, &rv, &fk) != nil {
				continue
			}
			total++
			add := func(b *bucket, v string, na bool) {
				n, err := strconv.Atoi(v)
				switch {
				case na || err != nil || v == "":
					b.NA++
				case n >= 71:
					b.Good++
				case n >= 41:
					b.OK++
				default:
					b.Bad++
				}
			}
			add(&seo, sv, fk == "")
			add(&read, rv, false)
		}
		rows.Close()
	}
	out["Total"], out["SEO"], out["Read"] = total, seo, read
	var redirects, r404, cornerstone int
	_ = s.store.Pool.QueryRow(ctx, `SELECT count(*) FROM redirects`).Scan(&redirects)
	_ = s.store.Pool.QueryRow(ctx, `SELECT count(*) FROM seogo_404`).Scan(&r404)
	_ = s.store.Pool.QueryRow(ctx, `SELECT count(*) FROM posts WHERE status='publish' AND type = ANY($1)
		AND coalesce(meta->>'_seogo_cornerstone', meta->>'_yoast_wpseo_is_cornerstone', '') = '1'`, s.sitemapPostTypes(ctx)).Scan(&cornerstone)
	out["Redirects"], out["NotFound"], out["Cornerstone"] = redirects, r404, cornerstone
	out["Orphans"] = s.seogoOrphanCount(ctx)
	var problems []string
	if !s.settings.Bool("blog_public") {
		problems = append(problems, "Search engines are discouraged from indexing this site (Settings › Reading). Nothing will be indexed.")
	}
	if !s.links(ctx).Pretty() {
		problems = append(problems, "Plain permalinks are in use. Pretty permalinks (Settings › Permalinks) make better URLs and enable the keyphrase-in-slug check.")
	}
	if s.settings.String("blogdescription") == "Just another Pursae site" {
		problems = append(problems, "You still have the default tagline. Change it under Settings › General.")
	}
	out["Problems"] = problems
	return out
}

// --- 404 log, link counter, bulk editor --------------------------------------------------------

type seogo404Row struct {
	ID        int64
	URI       string
	Hits      int64
	Referer   string
	UserAgent string
	LastSeen  time.Time
}

func (s *Server) seogo404Rows(ctx context.Context, search string) []seogo404Row {
	q := `SELECT id, uri, hits, referer, user_agent, last_seen FROM seogo_404`
	args := []any{}
	if search != "" {
		q += ` WHERE uri ILIKE $1`
		args = append(args, "%"+search+"%")
	}
	rows, err := s.store.Pool.Query(ctx, q+` ORDER BY last_seen DESC LIMIT 500`, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []seogo404Row
	for rows.Next() {
		var r seogo404Row
		if rows.Scan(&r.ID, &r.URI, &r.Hits, &r.Referer, &r.UserAgent, &r.LastSeen) == nil {
			out = append(out, r)
		}
	}
	return out
}

type seogoLinkRow struct {
	ID                          int64
	Title, URL, Type, EditURL   string
	Internal, External, Inbound int
	Cornerstone                 bool
}

// seogoLinkCounts counts the links of every published post of the sitemap post types (Yoast's text
// link counter). Posts are read in pages so memory stays flat on big sites; only the anchors of
// each post are kept, not its content.
func (s *Server) seogoLinkCounts(ctx context.Context) []seogoLinkRow {
	lb := s.links(ctx)
	home := strings.TrimSuffix(lb.HomeURL("/"), "/")
	byPath := map[string]int{}
	var rows []seogoLinkRow
	var hrefs [][]string
	types := s.sitemapPostTypes(ctx)
	const per = 500
	for page := 1; ; page++ {
		posts, _, err := s.store.QueryPosts(ctx, store.PostQuery{Types: types, Statuses: []string{"publish"}, PerPage: per, Page: page, OrderBy: "id", Order: "asc"})
		if err != nil || len(posts) == 0 {
			break
		}
		for _, p := range posts {
			i := len(rows)
			u := lb.Permalink(p)
			rows = append(rows, seogoLinkRow{ID: p.ID, Title: p.Title, URL: u, Type: p.Type, EditURL: "/wp-admin/post?post=" + itoa64(p.ID) + "&action=edit",
				Cornerstone: seogoMeta(p.Meta, "cornerstone") == "1"})
			byPath["/"+normPath(u)] = i
			byPath["/?p="+itoa64(p.ID)] = i
			byPath["/?page_id="+itoa64(p.ID)] = i
			var hs []string
			for _, m := range anchorRe.FindAllStringSubmatch(p.Content, -1) {
				hs = append(hs, m[1])
			}
			hrefs = append(hrefs, hs)
		}
		if len(posts) < per {
			break
		}
	}
	front := int64(0)
	if s.settings.String("show_on_front") == "page" {
		front = int64(s.settings.Int("page_on_front"))
	}
	for i := range rows {
		seen := map[int]bool{}
		for _, raw := range hrefs[i] {
			href := strings.TrimSpace(raw)
			if href == "" || strings.HasPrefix(href, "#") || strings.HasPrefix(href, "mailto:") {
				continue
			}
			if strings.Contains(href, "://") && !strings.HasPrefix(href, home) {
				rows[i].External++
				continue
			}
			rows[i].Internal++
			path := strings.TrimPrefix(href, home)
			if j := strings.Index(path, "#"); j >= 0 {
				path = path[:j]
			}
			key := "/" + normPath(path)
			if strings.HasPrefix(path, "/?") {
				key = path
			}
			if j, ok := byPath[key]; ok && j != i && !seen[j] {
				seen[j] = true
				rows[j].Inbound++
			}
		}
	}
	if front > 0 { // the front page is linked from every page through the site title
		for i := range rows {
			if rows[i].ID == front {
				rows[i].Inbound = max(rows[i].Inbound, 1)
			}
		}
	}
	return rows
}

// The link index (every post's link counts) is cached for a few minutes and dropped whenever a post
// is saved, so the Dashboard's orphan count and the posts list's incoming links do not rescan every
// post on each request.
var seogoLinkCache = struct {
	sync.Mutex
	m map[*Server]seogoLinkCacheEntry
}{m: map[*Server]seogoLinkCacheEntry{}}

type seogoLinkCacheEntry struct {
	rows map[int64]seogoLinkRow
	at   time.Time
}

// seogoLinkIndex is seogoLinkCounts by post ID, cached.
func (s *Server) seogoLinkIndex(ctx context.Context) map[int64]seogoLinkRow {
	seogoLinkCache.Lock()
	if e, ok := seogoLinkCache.m[s]; ok && time.Since(e.at) < 5*time.Minute {
		seogoLinkCache.Unlock()
		return e.rows
	}
	seogoLinkCache.Unlock()
	rows := map[int64]seogoLinkRow{}
	for _, r := range s.seogoLinkCounts(ctx) {
		rows[r.ID] = r
	}
	seogoLinkCache.Lock()
	seogoLinkCache.m[s] = seogoLinkCacheEntry{rows, time.Now()}
	seogoLinkCache.Unlock()
	return rows
}

func (s *Server) seogoOrphanCount(ctx context.Context) int {
	n := 0
	for _, r := range s.seogoLinkIndex(ctx) {
		if r.Inbound == 0 {
			n++
		}
	}
	return n
}

func (s *Server) seogoOrphanCacheDrop() {
	seogoLinkCache.Lock()
	delete(seogoLinkCache.m, s)
	seogoLinkCache.Unlock()
}

type seogoBulkRow struct {
	ID                                int64
	Title, URL, Status                string
	SEOTitle, Desc, Focus             string
	TitlePlaceholder, DescPlaceholder string
	SEOScore, ReadScore               string
	Noindex                           bool
}

// seogoBulkRows is one page of the Bulk Editor list (with the total for the pager).
func (s *Server) seogoBulkRows(ctx context.Context, typ, search string, per, page int) ([]seogoBulkRow, int) {
	posts, total, _ := s.store.QueryPosts(ctx, store.PostQuery{Types: []string{typ}, Statuses: []string{"publish", "draft", "future", "pending", "private"}, Search: search, PerPage: per, Page: page, OrderBy: "date", Order: "desc"})
	lb := s.links(ctx)
	var out []seogoBulkRow
	for _, p := range posts {
		sc := &seogoCtx{s: s, ctx: ctx, lb: lb, Kind: s.seogoQueryForPost(ctx, p).Kind, Post: p}
		r := seogoBulkRow{ID: p.ID, Title: p.Title, URL: lb.Permalink(p), Status: p.Status, SEOTitle: seogoMeta(p.Meta, "title"),
			Desc: seogoMeta(p.Meta, "description"), Focus: seogoMeta(p.Meta, "focus"), Noindex: seogoMeta(p.Meta, "noindex") == "noindex"}
		r.TitlePlaceholder = sc.replace(s.seogoOpts().Types[typ].Title)
		r.DescPlaceholder = sc.excerpt(false)
		r.SEOScore, _ = seogoRaw(p.Meta, seogoKey("seo_score"))
		r.ReadScore, _ = seogoRaw(p.Meta, seogoKey("read_score"))
		if r.SEOScore == "" {
			r.SEOScore, _ = seogoRaw(p.Meta, "_yoast_wpseo_linkdex")
		}
		out = append(out, r)
	}
	return out, total
}

// --- import from Yoast SEO / Rank Math ---------------------------------------------------------

type seogoDetected struct {
	Yoast, RankMath int
}

func (s *Server) seogoDetectImport(ctx context.Context) seogoDetected {
	var d seogoDetected
	_ = s.store.Pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE EXISTS (SELECT 1 FROM jsonb_object_keys(meta) k WHERE k LIKE '\_yoast\_wpseo\_%')),
		count(*) FILTER (WHERE EXISTS (SELECT 1 FROM jsonb_object_keys(meta) k WHERE k LIKE 'rank\_math\_%')) FROM posts`).Scan(&d.Yoast, &d.RankMath)
	var ty, tr int
	_ = s.store.Pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE EXISTS (SELECT 1 FROM jsonb_object_keys(meta) k WHERE k LIKE '\_yoast\_wpseo\_%')),
		count(*) FILTER (WHERE EXISTS (SELECT 1 FROM jsonb_object_keys(meta) k WHERE k LIKE 'rank\_math\_%')) FROM terms`).Scan(&ty, &tr)
	d.Yoast += ty
	d.RankMath += tr
	return d
}

// seogoImportAll converts every post's and term's Yoast / Rank Math meta into Pursae SEO's keys.
func (s *Server) seogoImportAll(ctx context.Context) (int, error) {
	n := 0
	rows, err := s.store.Pool.Query(ctx, `SELECT id, meta FROM posts WHERE meta::text LIKE '%yoast_wpseo%' OR meta::text LIKE '%rank_math_%'`)
	if err != nil {
		return 0, err
	}
	type item struct {
		id   int64
		meta map[string]any
	}
	var list []item
	for rows.Next() {
		var it item
		var raw []byte
		if rows.Scan(&it.id, &raw) == nil && json.Unmarshal(raw, &it.meta) == nil {
			list = append(list, it)
		}
	}
	rows.Close()
	for _, it := range list {
		if seogoImportMeta(it.meta) > 0 {
			if _, err := s.store.Pool.Exec(ctx, `UPDATE posts SET meta = $2 WHERE id = $1`, it.id, seogoJSON(it.meta)); err == nil {
				n++
			}
		}
	}
	trows, err := s.store.Pool.Query(ctx, `SELECT id, meta FROM terms WHERE meta::text LIKE '%yoast_wpseo%' OR meta::text LIKE '%rank_math_%'`)
	if err == nil {
		list = list[:0]
		for trows.Next() {
			var it item
			var raw []byte
			if trows.Scan(&it.id, &raw) == nil && json.Unmarshal(raw, &it.meta) == nil {
				list = append(list, it)
			}
		}
		trows.Close()
		for _, it := range list {
			if seogoImportMeta(it.meta) > 0 {
				if _, err := s.store.Pool.Exec(ctx, `UPDATE terms SET meta = $2 WHERE id = $1`, it.id, seogoJSON(it.meta)); err == nil {
					n++
				}
			}
		}
	}
	s.purgeAll(ctx)
	return n, nil
}

// --- POST handlers -----------------------------------------------------------------------------

func seogoFormBool(c *gin.Context, key string) bool {
	v := c.PostForm(key)
	return v == "1" || v == "on" || v == "true"
}

func (s *Server) seogoAdminPost(c *gin.Context, page string) (string, error) {
	ctx := c.Request.Context()
	o := s.seogoOpts().clone()
	save := func(msg string) (string, error) {
		if err := s.seogoSave(ctx, o); err != nil {
			return "", err
		}
		return msg, nil
	}
	switch page {
	case "search-console":
		return s.seogoGSCPost(c)
	case "":
		if key := c.PostForm("module"); key != "" {
			found := false
			for _, m := range seogoModules {
				found = found || m.Key == key
			}
			if !found {
				return "", fmt.Errorf("unknown module")
			}
			o.Modules[key] = c.PostForm("state") == "on"
			return save("Module " + map[bool]string{true: "activated", false: "deactivated"}[o.Modules[key]] + ".")
		}
		return "Nothing changed.", nil
	case "wizard":
		return s.seogoWizardPost(c, o)
	case "general":
		switch c.PostForm("tab") {
		case "", "links":
			o.Separator = strings.TrimSpace(c.PostForm("separator"))
			if o.Separator == "" {
				o.Separator = "-"
			}
			o.NoindexEmptyTerms = seogoFormBool(c, "noindex_empty_terms")
			o.NoindexPaged = seogoFormBool(c, "noindex_paged")
			o.RobotsDefaults = strings.TrimSpace(c.PostForm("robots_defaults"))
			o.AutoRedirect = seogoFormBool(c, "auto_redirect")
			o.Redirect404 = strings.TrimSpace(c.PostForm("redirect_404"))
			o.MonitorLimit, _ = strconv.Atoi(c.PostForm("monitor_limit"))
			o.MonitorIgnore = c.PostForm("monitor_ignore")
		case "webmaster":
			for _, k := range []string{"google", "bing", "baidu", "yandex", "pinterest", "naver"} {
				o.Verify[k] = seogoVerifyCode(strings.TrimSpace(c.PostForm("verify_" + k)))
			}
		case "breadcrumbs":
			o.BCSep, o.BCHome, o.BCPrefix = c.PostForm("bc_sep"), c.PostForm("bc_home"), c.PostForm("bc_prefix")
			o.BCArchive, o.BCSearch, o.BC404 = c.PostForm("bc_archive"), c.PostForm("bc_search"), c.PostForm("bc_404")
			o.BCBoldLast, o.BCShowBlog, o.BCHideTitle = seogoFormBool(c, "bc_bold_last"), seogoFormBool(c, "bc_show_blog"), seogoFormBool(c, "bc_hide_title")
		case "toc":
			o.TOCTitle = strings.TrimSpace(c.PostForm("toc_title"))
			o.TOCLevels = nil
			for _, v := range c.PostFormArray("toc_levels") {
				if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= 6 && !slices.Contains(o.TOCLevels, n) {
					o.TOCLevels = append(o.TOCLevels, n)
				}
			}
			slices.Sort(o.TOCLevels)
			o.TOCNumbered, o.TOCCollapsible, o.TOCAuto = seogoFormBool(c, "toc_numbered"), seogoFormBool(c, "toc_collapsible"), seogoFormBool(c, "toc_auto")
			o.TOCAutoTypes = nil
			for _, v := range c.PostFormArray("toc_auto_types") {
				if slices.Contains(s.sitemapPostTypes(c.Request.Context()), v) && !slices.Contains(o.TOCAutoTypes, v) {
					o.TOCAutoTypes = append(o.TOCAutoTypes, v)
				}
			}
			o.TOCMin, _ = strconv.Atoi(c.PostForm("toc_min"))
			o.TOCMin = min(max(o.TOCMin, 1), 20)
		case "rss":
			o.RSSBefore, o.RSSAfter = c.PostForm("rss_before"), c.PostForm("rss_after")
			if !currentUser(c).Can("unfiltered_html", nil) { // feed items carry them as HTML
				o.RSSBefore, o.RSSAfter = wp.KsesPost(o.RSSBefore), wp.KsesPost(o.RSSAfter)
			}
		case "images":
			o.ImgAlt, o.ImgTitle = c.PostForm("img_alt"), c.PostForm("img_title")
		}
		return save("Settings saved.")
	case "titles":
		for _, r := range s.seogoTypeRows() {
			t := o.Types[r.Key]
			t.Title = c.PostForm("title_" + r.Key)
			t.Description = c.PostForm("desc_" + r.Key)
			if !r.Special {
				t.Noindex = seogoFormBool(c, "noindex_"+r.Key)
				t.Sitemap = !seogoFormBool(c, "nositemap_"+r.Key)
			} else if r.Key != "home" {
				t.Noindex = true
			}
			if r.Key == "author" || r.Key == "date" {
				t.Disabled = seogoFormBool(c, "disabled_"+r.Key)
			}
			if r.Key == "post" || r.Key == "page" {
				t.SchemaPage, t.SchemaArt = c.PostForm("schema_page_"+r.Key), c.PostForm("schema_art_"+r.Key)
			}
			o.Types[r.Key] = t
		}
		return save("Search Appearance saved.")
	case "social":
		o.OpenGraph, o.TwitterCards = seogoFormBool(c, "opengraph"), seogoFormBool(c, "twitter_cards")
		o.TwitterCard = map[bool]string{true: "summary", false: "summary_large_image"}[c.PostForm("twitter_card") == "summary"]
		o.DefaultImage, o.HomeOGImage = strings.TrimSpace(c.PostForm("default_image")), strings.TrimSpace(c.PostForm("home_og_image"))
		o.FacebookURL, o.FacebookAppID = strings.TrimSpace(c.PostForm("facebook_url")), strings.TrimSpace(c.PostForm("facebook_app_id"))
		o.TwitterUser = strings.TrimPrefix(strings.TrimSpace(c.PostForm("twitter_user")), "@")
		o.Profiles = lines(c.PostForm("profiles"))
		return save("Social settings saved.")
	case "schema":
		o.KGType = map[bool]string{true: "person", false: "organization"}[c.PostForm("kg_type") == "person"]
		o.KGName, o.KGAltName, o.KGLogo = strings.TrimSpace(c.PostForm("kg_name")), strings.TrimSpace(c.PostForm("kg_alt_name")), strings.TrimSpace(c.PostForm("kg_logo"))
		o.KGPersonID, _ = strconv.ParseInt(c.PostForm("kg_person"), 10, 64)
		l := &o.Local
		l.Type, l.Name, l.Street, l.City = c.PostForm("local_type"), c.PostForm("local_name"), c.PostForm("local_street"), c.PostForm("local_city")
		l.Region, l.Postal, l.Country, l.Phone = c.PostForm("local_region"), c.PostForm("local_postal"), c.PostForm("local_country"), c.PostForm("local_phone")
		l.Email, l.PriceRange, l.Lat, l.Lng, l.Hours = c.PostForm("local_email"), c.PostForm("local_price"), c.PostForm("local_lat"), c.PostForm("local_lng"), c.PostForm("local_hours")
		return save("Schema settings saved.")
	case "sitemap":
		o.SitemapPerPage, _ = strconv.Atoi(c.PostForm("sitemap_per_page"))
		o.SitemapImages = seogoFormBool(c, "sitemap_images")
		o.SitemapExclude, o.SitemapExcludeT = c.PostForm("sitemap_exclude"), c.PostForm("sitemap_exclude_terms")
		o.SitemapNewsTypes = c.PostForm("sitemap_news_name")
		return save("Sitemap settings saved.")
	case "redirections":
		return s.seogoRedirectionsPost(c)
	case "404":
		switch c.PostForm("action") {
		case "clear":
			_, err := s.store.Pool.Exec(ctx, `DELETE FROM seogo_404`)
			return "The 404 log was cleared.", err
		case "delete":
			ids := c.PostFormArray("ids[]")
			if len(ids) == 0 {
				return "No items selected.", nil
			}
			var nums []int64
			for _, v := range ids {
				if n, err := strconv.ParseInt(v, 10, 64); err == nil {
					nums = append(nums, n)
				}
			}
			_, err := s.store.Pool.Exec(ctx, `DELETE FROM seogo_404 WHERE id = ANY($1)`, nums)
			return fmt.Sprintf("%d log entries deleted.", len(nums)), err
		}
		return "Nothing changed.", nil
	case "bulk":
		n := 0
		for _, raw := range c.PostFormArray("ids[]") {
			id, _ := strconv.ParseInt(raw, 10, 64)
			p, err := s.store.GetPost(ctx, id)
			if err != nil || !currentUser(c).Can("edit_post", p) {
				continue
			}
			if p.Meta == nil {
				p.Meta = map[string]any{}
			}
			before := string(seogoJSON(p.Meta))
			seogoSetMeta(p.Meta, "title", c.PostForm("title_"+raw))
			seogoSetMeta(p.Meta, "description", c.PostForm("desc_"+raw))
			seogoSetMeta(p.Meta, "focus", c.PostForm("focus_"+raw))
			if string(seogoJSON(p.Meta)) != before {
				s.seogoStoreScores(ctx, p)
				if _, err := s.store.Pool.Exec(ctx, `UPDATE posts SET meta = $2 WHERE id = $1`, p.ID, seogoJSON(p.Meta)); err == nil {
					n++
				}
			}
		}
		s.purgeAll(ctx)
		return fmt.Sprintf("%d item(s) updated.", n), nil
	case "tools":
		return s.seogoToolsPost(c, o)
	case "roles":
		o.Roles = map[string][]string{}
		for _, r := range s.seogoRoleRows(ctx) {
			for _, cp := range seogoCaps {
				if seogoFormBool(c, "cap_"+r.Slug+"_"+cp.Key) {
					o.Roles[r.Slug] = append(o.Roles[r.Slug], cp.Key)
				}
			}
		}
		return save("Role Manager saved.")
	}
	return "", fmt.Errorf("unknown screen")
}

func (s *Server) seogoRedirectionsPost(c *gin.Context) (string, error) {
	ctx := c.Request.Context()
	switch c.PostForm("action") {
	case "save":
		r := &seogoRedirectRow{Source: strings.TrimSpace(c.PostForm("source")), Target: strings.TrimSpace(c.PostForm("target")),
			MatchType: c.PostForm("match_type"), Status: c.PostForm("status"), Category: strings.TrimSpace(c.PostForm("category"))}
		r.ID, _ = strconv.ParseInt(c.PostForm("id"), 10, 64)
		r.Code, _ = strconv.Atoi(c.PostForm("code"))
		switch r.Code {
		case 301, 302, 307, 410, 451:
		default:
			return "", fmt.Errorf("unsupported redirect type")
		}
		switch r.MatchType {
		case "exact", "contains", "start", "end", "regex":
		default:
			r.MatchType = "exact"
		}
		if r.Status != "inactive" {
			r.Status = "active"
		}
		if err := s.seogoAddRedirect(ctx, r); err != nil {
			return "", err
		}
		return "Redirection saved.", nil
	case "delete", "activate", "deactivate":
		var nums []int64
		for _, v := range c.PostFormArray("ids[]") {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				nums = append(nums, n)
			}
		}
		if len(nums) == 0 {
			return "No redirections selected.", nil
		}
		var err error
		switch c.PostForm("action") {
		case "delete":
			_, err = s.store.Pool.Exec(ctx, `DELETE FROM redirects WHERE id = ANY($1)`, nums)
		case "activate":
			_, err = s.store.Pool.Exec(ctx, `UPDATE redirects SET status = 'active' WHERE id = ANY($1)`, nums)
		default:
			_, err = s.store.Pool.Exec(ctx, `UPDATE redirects SET status = 'inactive' WHERE id = ANY($1)`, nums)
		}
		s.seogoRedirectsChanged()
		return fmt.Sprintf("%d redirection(s) updated.", len(nums)), err
	case "import":
		f, _, err := c.Request.FormFile("file")
		var data []byte
		if err == nil {
			data, _ = io.ReadAll(io.LimitReader(f, 5<<20))
			f.Close()
		} else {
			data = []byte(c.PostForm("csv"))
		}
		n, err := s.seogoImportRedirects(ctx, data)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%d redirection(s) imported.", n), nil
	}
	return "Nothing changed.", nil
}

// seogoImportRedirects reads CSV lines "source,target[,code[,match type]]" (Rank Math / Redirection
// exports and Yoast's plain list use the same columns); a header line is skipped.
func (s *Server) seogoImportRedirects(ctx context.Context, data []byte) (int, error) {
	r := csv.NewReader(strings.NewReader(string(data)))
	r.FieldsPerRecord = -1
	r.TrimLeadingSpace = true
	recs, err := r.ReadAll()
	if err != nil {
		return 0, fmt.Errorf("the CSV could not be read: %v", err)
	}
	n := 0
	for i, rec := range recs {
		if len(rec) < 2 || (i == 0 && strings.EqualFold(strings.TrimSpace(rec[0]), "source")) {
			continue
		}
		row := &seogoRedirectRow{Source: rec[0], Target: rec[1], Code: 301, MatchType: "exact", Origin: "import"}
		if len(rec) > 2 {
			if code, err := strconv.Atoi(strings.TrimSpace(rec[2])); err == nil {
				row.Code = code
			}
		}
		if len(rec) > 3 {
			switch m := strings.ToLower(strings.TrimSpace(rec[3])); m {
			case "regex", "contains", "start", "end", "exact":
				row.MatchType = m
			}
		}
		if s.seogoAddRedirect(ctx, row) == nil {
			n++
		}
	}
	return n, nil
}

func (s *Server) seogoExportRedirects(c *gin.Context) {
	if !s.checkNonce(c, "seogo-redirections") {
		s.denyNonce(c)
		return
	}
	rows, _ := s.seogoRedirectRows(c.Request.Context(), false)
	var sb strings.Builder
	w := csv.NewWriter(&sb)
	_ = w.Write([]string{"source", "target", "code", "match_type", "status", "hits"})
	for _, r := range rows {
		_ = w.Write([]string{r.Source, r.Target, strconv.Itoa(r.Code), r.MatchType, r.Status, strconv.FormatInt(r.Hits, 10)})
	}
	w.Flush()
	c.Header("Content-Disposition", `attachment; filename="seogo-redirections.csv"`)
	c.Data(http.StatusOK, "text/csv; charset=utf-8", []byte(sb.String()))
}

func (s *Server) seogoExportSettings(c *gin.Context) {
	if !s.checkNonce(c, "seogo-tools") {
		s.denyNonce(c)
		return
	}
	o := s.seogoOpts().clone()
	b, _ := json.MarshalIndent(o, "", "  ")
	c.Header("Content-Disposition", `attachment; filename="seogo-settings.json"`)
	c.Data(http.StatusOK, "application/json", b)
}

func (s *Server) seogoToolsPost(c *gin.Context, o *seogoOptions) (string, error) {
	ctx := c.Request.Context()
	switch c.PostForm("action") {
	case "import_meta":
		n, err := s.seogoImportAll(ctx)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("SEO data of %d item(s) was imported from Yoast SEO and Rank Math.", n), nil
	case "robots":
		o.RobotsTxt = strings.ReplaceAll(c.PostForm("robots_txt"), "\r\n", "\n")
		if err := s.seogoSave(ctx, o); err != nil {
			return "", err
		}
		return "robots.txt saved.", nil
	case "import_settings":
		raw := c.PostForm("settings_json")
		if f, _, err := c.Request.FormFile("file"); err == nil {
			b, _ := io.ReadAll(io.LimitReader(f, 1<<20))
			f.Close()
			raw = string(b)
		}
		in, err := seogoImportOptions(o, raw)
		if err != nil {
			return "", err
		}
		if err := s.seogoSave(ctx, in); err != nil {
			return "", err
		}
		return "Settings imported.", nil
	case "indexnow":
		if !o.Modules["indexnow"] {
			return "", fmt.Errorf("enable the Instant Indexing module first")
		}
		urls := lines(c.PostForm("urls"))
		if len(urls) == 0 {
			return "", fmt.Errorf("enter at least one URL")
		}
		s.seogoIndexNow(ctx, urls)
		return fmt.Sprintf("%d URL(s) submitted to IndexNow.", len(urls)), nil
	case "indexnow_key":
		o.IndexNowKey = ""
		if err := s.seogoSave(ctx, o); err != nil {
			return "", err
		}
		if err := s.seogoActivate(ctx); err != nil {
			return "", err
		}
		return "A new IndexNow key was generated.", nil
	case "recalc":
		// Every sitemap post type, in batches ordered by id (keyset) so nothing is skipped or cut off.
		types := s.sitemapPostTypes(ctx)
		done, after := 0, int64(0)
		for {
			rows, err := s.store.Pool.Query(ctx, `SELECT id FROM posts WHERE type = ANY($1) AND id > $2 AND status IN ('publish','draft','future','pending','private') ORDER BY id LIMIT 200`, types, after)
			if err != nil {
				return "", err
			}
			var ids []int64
			for rows.Next() {
				var id int64
				if rows.Scan(&id) == nil {
					ids = append(ids, id)
				}
			}
			rows.Close()
			if len(ids) == 0 {
				break
			}
			after = ids[len(ids)-1]
			posts, _, _ := s.store.QueryPosts(ctx, store.PostQuery{IDs: ids, Types: types, Statuses: []string{"publish", "draft", "future", "pending", "private"}, PerPage: -1, OrderBy: "id", Order: "asc"})
			for _, p := range posts {
				if p.Meta == nil {
					p.Meta = map[string]any{}
				}
				s.seogoStoreScores(ctx, p)
				_, _ = s.store.Pool.Exec(ctx, `UPDATE posts SET meta = $2 WHERE id = $1`, p.ID, seogoJSON(p.Meta))
				done++
			}
		}
		return fmt.Sprintf("Scores of %d item(s) recalculated.", done), nil
	case "image_alt":
		n, err := s.seogoFillImageAlts(ctx, o)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%d image(s) in the Media Library got an alt text.", n), nil
	}
	return "Nothing changed.", nil
}

// --- setup wizard ------------------------------------------------------------------------------

func (s *Server) seogoRenderWizard(c *gin.Context, d *seogoPage) {
	s.enqueueMedia(c, 0)
	s.adminPage(c, "seogo", "Setup Wizard ‹ Pursae SEO", "toplevel_page_seogo seogo-admin seogo-wizard-page", "admin/seogo", d, seogoFlash(c)...)
}

func (s *Server) seogoWizardPost(c *gin.Context, o *seogoOptions) (string, error) {
	ctx := c.Request.Context()
	step, _ := strconv.Atoi(c.PostForm("step"))
	switch step {
	case 1: // site representation
		o.KGType = map[bool]string{true: "person", false: "organization"}[c.PostForm("kg_type") == "person"]
		o.KGName = strings.TrimSpace(c.PostForm("kg_name"))
		o.KGLogo = strings.TrimSpace(c.PostForm("kg_logo"))
		o.KGPersonID, _ = strconv.ParseInt(c.PostForm("kg_person"), 10, 64)
		o.Separator = defaultStr(strings.TrimSpace(c.PostForm("separator")), o.Separator)
	case 2: // social profiles
		o.FacebookURL = strings.TrimSpace(c.PostForm("facebook_url"))
		o.TwitterUser = strings.TrimPrefix(strings.TrimSpace(c.PostForm("twitter_user")), "@")
		o.Profiles = lines(c.PostForm("profiles"))
	case 3: // search engine visibility, sitemaps, modules
		o.Modules["sitemap"] = seogoFormBool(c, "sitemap")
		o.Modules["redirections"] = seogoFormBool(c, "redirections")
		o.Modules["monitor"] = seogoFormBool(c, "monitor")
		o.Modules["breadcrumbs"] = seogoFormBool(c, "breadcrumbs")
		o.Modules["indexnow"] = seogoFormBool(c, "indexnow")
		o.Types["author"] = func(t seogoTypeOpts) seogoTypeOpts { t.Disabled = seogoFormBool(c, "single_author"); return t }(o.Types["author"])
		o.NoindexEmptyTerms = seogoFormBool(c, "noindex_empty_terms")
	case 4: // import
		if seogoFormBool(c, "import") {
			if _, err := s.seogoImportAll(ctx); err != nil {
				return "", err
			}
		}
	case 5:
		o.WizardDone = true
	}
	if step == 4 {
		o.WizardDone = true
	}
	if err := s.seogoSave(ctx, o); err != nil {
		return "", err
	}
	next := step + 1
	if c.PostForm("skip") == "1" || step >= 5 {
		o.WizardDone = true
		_ = s.seogoSave(ctx, o)
		c.Redirect(http.StatusFound, "/wp-admin/seogo?seogo-updated="+url.QueryEscape("Pursae SEO is set up."))
		return "ok", nil
	}
	c.Redirect(http.StatusFound, "/wp-admin/seogo?page=wizard&step="+strconv.Itoa(next))
	return "ok", nil
}

// --- Image SEO ---------------------------------------------------------------------------------

var imgNoAltRe = regexp.MustCompile(`(?i)<img\s[^>]*>`)

// seogoImageSEO fills empty alt (and title) attributes of images in rendered content.
func (s *Server) seogoImageSEO(ctx context.Context, content string) string {
	if !s.seogoModule("image_seo") || !strings.Contains(content, "<img") {
		return content
	}
	o := s.seogoOpts()
	rc := renderFrom(ctx)
	sc := &seogoCtx{s: s, ctx: ctx, lb: s.links(ctx)}
	if rc != nil && rc.q != nil {
		sc = s.seogoCtxFor(ctx, rc.q)
	}
	return imgNoAltRe.ReplaceAllStringFunc(content, func(tag string) string {
		src := ""
		if m := seogoImgSrcRe.FindStringSubmatch(tag); m != nil {
			src = m[1]
		}
		if o.ImgAlt != "" {
			if m := altRe.FindStringSubmatch(tag); m == nil || strings.TrimSpace(m[1]) == "" {
				alt := template.HTMLEscapeString(sc.imageText(o.ImgAlt, src))
				if m == nil {
					tag = strings.Replace(tag, "<img", `<img alt="`+alt+`"`, 1)
				} else {
					tag = strings.Replace(tag, m[0], ` alt="`+alt+`"`, 1)
				}
			}
		}
		if o.ImgTitle != "" && !strings.Contains(strings.ToLower(tag), " title=") {
			tag = strings.Replace(tag, "<img", `<img title="`+template.HTMLEscapeString(sc.imageText(o.ImgTitle, src))+`"`, 1)
		}
		return tag
	})
}

// seogoFillImageAlts writes alt texts for Media Library images that have none (Tools).
func (s *Server) seogoFillImageAlts(ctx context.Context, o *seogoOptions) (int, error) {
	rows, err := s.store.Pool.Query(ctx, `SELECT id FROM media WHERE mime_type LIKE 'image/%' AND alt_text = ''`)
	if err != nil {
		return 0, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	sc := &seogoCtx{s: s, ctx: ctx, lb: s.links(ctx)}
	tpl := o.ImgAlt
	if tpl == "" {
		tpl = "%filename%"
	}
	n := 0
	for _, id := range ids {
		a, err := s.store.GetAttachment(ctx, id)
		if err != nil {
			continue
		}
		a.Alt = sc.imageText(tpl, a.FilePath)
		if a.Alt != "" && s.store.UpdateAttachment(ctx, a) == nil {
			n++
		}
	}
	return n, nil
}

// seogoImportOptions merges an imported settings JSON over the current options: keys the file
// leaves out keep their values, and only a JSON object is accepted.
func seogoImportOptions(cur *seogoOptions, raw string) (*seogoOptions, error) {
	if !strings.HasPrefix(strings.TrimSpace(raw), "{") {
		return nil, fmt.Errorf("the settings file is not valid JSON")
	}
	in := cur.clone()
	if err := json.Unmarshal([]byte(raw), in); err != nil {
		return nil, fmt.Errorf("the settings file is not valid JSON")
	}
	if in.IndexNowKey == "" {
		in.IndexNowKey = cur.IndexNowKey
	}
	if in.Types == nil {
		in.Types = cur.clone().Types
	}
	if in.Modules == nil {
		in.Modules = cur.clone().Modules
	}
	if in.Verify == nil {
		in.Verify = map[string]string{}
	}
	if in.Roles == nil {
		in.Roles = cur.clone().Roles
	}
	return in, nil
}

// --- posts list: score columns and filters -----------------------------------------------------

// seogoScoreBand maps a stored 0-100 score to its traffic light (the thresholds of the dashboard).
func seogoScoreBand(v string, na bool) (class, label string) {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	switch {
	case na || err != nil || strings.TrimSpace(v) == "":
		return "na", "Not available"
	case n >= 71:
		return "good", "Good"
	case n >= 41:
		return "ok", "OK"
	default:
		return "bad", "Needs improvement"
	}
}

// seogoListFilters are the filter views of the posts list: query value, label, SQL condition on posts.
var seogoListFilters = []struct{ Key, Label, Cond string }{
	{"nofocus", "No focus keyphrase", `coalesce(meta->>'_seogo_focus', meta->>'_yoast_wpseo_focuskw', meta->>'rank_math_focus_keyword', '') = ''`},
	{"seo_bad", "SEO: Needs improvement", `coalesce(meta->>'_seogo_seo_score', meta->>'_yoast_wpseo_linkdex', '') ~ '^[0-9]+$' AND coalesce(meta->>'_seogo_seo_score', meta->>'_yoast_wpseo_linkdex')::int < 41 AND coalesce(meta->>'_seogo_focus', meta->>'_yoast_wpseo_focuskw', meta->>'rank_math_focus_keyword', '') <> ''`},
	{"seo_good", "SEO: Good", `coalesce(meta->>'_seogo_seo_score', meta->>'_yoast_wpseo_linkdex', '') ~ '^[0-9]+$' AND coalesce(meta->>'_seogo_seo_score', meta->>'_yoast_wpseo_linkdex')::int >= 71`},
	{"read_bad", "Readability: Needs improvement", `coalesce(meta->>'_seogo_read_score', meta->>'_yoast_wpseo_content_score', '') ~ '^[0-9]+$' AND coalesce(meta->>'_seogo_read_score', meta->>'_yoast_wpseo_content_score')::int < 41`},
	{"noindex", "Set to noindex", `coalesce(meta->>'_seogo_noindex', meta->>'_yoast_wpseo_meta-robots-noindex', '') IN ('1','2')`},
}

// seogoListFilter narrows the posts list query by the seo_filter parameter and returns the active key.
func (s *Server) seogoListFilter(c *gin.Context, typ string, q *store.PostQuery) string {
	key := c.Query("seo_filter")
	if key == "" || !s.seogoOn() || !s.seogoCan(currentUser(c), "seogo_onpage_general") {
		return ""
	}
	for _, f := range seogoListFilters {
		if f.Key != key {
			continue
		}
		ids := []int64{-1}
		rows, err := s.store.Pool.Query(c.Request.Context(), `SELECT id FROM posts WHERE type = $1 AND `+f.Cond, typ)
		if err == nil {
			for rows.Next() {
				var id int64
				if rows.Scan(&id) == nil {
					ids = append(ids, id)
				}
			}
			rows.Close()
		}
		q.IDs = ids
		return key
	}
	return ""
}

// seogoPostColumns adds the SEO and Readability score columns to the posts list and the filter views.
func (s *Server) seogoPostColumns(c *gin.Context, d *postsListData, posts []*store.Post, typ, base, active string) {
	if !s.seogoOn() || len(posts) == 0 || !s.seogoCan(currentUser(c), "seogo_onpage_general") || !slices.Contains(s.sitemapPostTypes(c.Request.Context()), typ) {
		return
	}
	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}
	for _, f := range seogoListFilters {
		var n int
		_ = s.store.Pool.QueryRow(c.Request.Context(), `SELECT count(*) FROM posts WHERE type = $1 AND status <> 'trash' AND `+f.Cond, typ).Scan(&n)
		if n > 0 || active == f.Key {
			d.Statuses = append(d.Statuses, statusLink{"seo_" + f.Key, f.Label, base + sep + "seo_filter=" + f.Key, n, active == f.Key})
			if active == f.Key {
				for i := range d.Statuses {
					if d.Statuses[i].Key != "seo_"+f.Key {
						d.Statuses[i].Current = false
					}
				}
			}
		}
	}
	d.PluginHeads = append(d.PluginHeads,
		template.HTML(`<th scope="col" id="seogo-details" class="manage-column column-seogo-details" style="width:220px"><span class="seogo-col-title" title="Pursae SEO">SEO Details</span></th>`))
	d.ColCount++
	o := s.seogoOpts()
	var inbound map[int64]seogoLinkRow
	if o.Modules["links"] {
		inbound = s.seogoLinkIndex(c.Request.Context())
	}
	home := strings.TrimSuffix(s.links(c).HomeURL("/"), "/")
	for i := range d.Rows {
		if i >= len(posts) {
			break
		}
		p := posts[i]
		d.Rows[i].PluginCols = append(d.Rows[i].PluginCols, template.HTML(`<td class="seogo-details column-seogo-details" data-colname="SEO Details">`+
			s.seogoDetailsCell(p, o, home, inbound)+`</td>`))
	}
}

// seogoDetailsCell is the SEO Details cell of one post (Rank Math's list column): the score badge,
// the focus keyphrase, the schema type, readability and the link counts.
func (s *Server) seogoDetailsCell(p *store.Post, o *seogoOptions, home string, inbound map[int64]seogoLinkRow) string {
	m := p.Meta
	sv, _ := seogoRaw(m, "_seogo_seo_score")
	if sv == "" {
		sv, _ = seogoRaw(m, "_yoast_wpseo_linkdex")
	}
	rv, _ := seogoRaw(m, "_seogo_read_score")
	if rv == "" {
		rv, _ = seogoRaw(m, "_yoast_wpseo_content_score")
	}
	fk := seogoMeta(m, "focus")
	if fk == "" {
		fk, _ = seogoRaw(m, "_yoast_wpseo_focuskw")
	}
	if list := seogoFocusList(fk); len(list) > 0 {
		fk = list[0]
	}
	fk = strings.TrimSpace(fk)
	colors := map[string]string{"good": "#00a32a", "ok": "#dba617", "bad": "#d63638", "na": "#a7aaad"}
	sc, sl := seogoScoreBand(sv, fk == "")
	badge := "N/A"
	if sc != "na" {
		badge = strings.TrimSpace(sv) + " / 100"
	}
	var b strings.Builder
	b.WriteString(`<span class="seogo-score-badge ` + sc + `" title="SEO: ` + sl + `" style="display:inline-block;padding:2px 8px;border-radius:3px;font-weight:600;color:#fff;background:` + colors[sc] + `">` + badge + `</span>`)
	b.WriteString(`<span class="screen-reader-text">SEO: ` + sl + `</span>`)
	row := func(label, value string) {
		b.WriteString(`<div class="seogo-detail" style="margin-top:4px"><strong>` + label + `:</strong> ` + value + `</div>`)
	}
	if fk != "" {
		row("Keyword", html.EscapeString(fk))
	} else {
		row("Keyword", `<span style="color:#a7aaad">Not set</span>`)
	}
	if o.Modules["schema"] {
		row("Schema", html.EscapeString(seogoPostSchemaLabel(p, o)))
	}
	if o.Modules["analysis"] {
		rc, rl := seogoScoreBand(rv, false)
		row("Readability", `<span style="display:inline-block;width:10px;height:10px;border-radius:50%;background:`+colors[rc]+`"></span> `+rl)
	}
	if o.Modules["links"] {
		in, ex := seogoCountLinks(p.Content, home)
		inb := "–"
		if r, ok := inbound[p.ID]; ok {
			inb = strconv.Itoa(r.Inbound)
		}
		row("Links", `<span style="white-space:nowrap"><span title="Internal links">&#128279; `+strconv.Itoa(in)+`</span> <span style="color:#c3c4c7">|</span> `+
			`<span title="External links">&#8599; `+strconv.Itoa(ex)+`</span> <span style="color:#c3c4c7">|</span> `+
			`<span title="Incoming links">&#8601; `+inb+`</span></span>`)
	}
	return b.String()
}

// seogoPostSchemaLabel is the schema type a post publishes: its Article type, else a custom page
// type, else "Off" (the WebPage node every page has is not counted, as in Rank Math).
func seogoPostSchemaLabel(p *store.Post, o *seogoOptions) string {
	art := o.Types[p.Type].SchemaArt
	if v := seogoMeta(p.Meta, "schema_article"); v != "" {
		art = v
	}
	var parts []string
	if art != "" && art != "None" {
		parts = append(parts, art)
	} else if v := seogoMeta(p.Meta, "schema_page"); v != "" && v != "WebPage" {
		parts = append(parts, v)
	}
	if strings.Contains(p.Content, "<!-- wp:seogo/faq") || strings.Contains(p.Content, "<!-- wp:yoast/faq-block") || strings.Contains(p.Content, "<!-- wp:rank-math/faq-block") {
		parts = append(parts, "FAQ")
	}
	if strings.Contains(p.Content, "<!-- wp:seogo/howto") || strings.Contains(p.Content, "<!-- wp:yoast/how-to-block") || strings.Contains(p.Content, "<!-- wp:rank-math/howto-block") {
		parts = append(parts, "HowTo")
	}
	if seogoMeta(p.Meta, "schema_custom") != "" {
		parts = append(parts, "Custom")
	}
	if len(parts) == 0 {
		return "Off"
	}
	return strings.Join(parts, ", ")
}

// seogoCountLinks counts the internal and external links of one content (as seogoLinkCounts does).
func seogoCountLinks(content, home string) (internal, external int) {
	for _, m := range anchorRe.FindAllStringSubmatch(content, -1) {
		href := strings.TrimSpace(m[1])
		if href == "" || strings.HasPrefix(href, "#") || strings.HasPrefix(href, "mailto:") || strings.HasPrefix(href, "tel:") {
			continue
		}
		if strings.Contains(href, "://") && !strings.HasPrefix(href, home) {
			external++
		} else {
			internal++
		}
	}
	return
}
