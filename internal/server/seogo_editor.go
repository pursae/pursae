package server

import (
	"bytes"
	"context"
	"encoding/json"
	"html"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"wpgo/internal/store"
)

// The Pursae SEO box of both editors: a meta box in the classic editor and a sidebar (plus a
// document settings panel) in Gotenberg, both built from the same markup (seogo/metabox) and the
// same script (/wp-admin/js/seogo-editor.js). Analysis, snippet preview values and saving go
// through the seogo/v1 REST routes registered here, together with the get_head endpoints.

// seogoEditFields are the per-post fields the box edits, with the capability each needs.
var seogoEditFields = map[string]string{
	"focus": "seogo_onpage_general", "title": "seogo_onpage_general", "description": "seogo_onpage_general",
	"cornerstone": "seogo_onpage_general",
	"canonical":   "seogo_onpage_advanced", "noindex": "seogo_onpage_advanced", "nofollow": "seogo_onpage_advanced",
	"robots_adv": "seogo_onpage_advanced", "max_snippet": "seogo_onpage_advanced", "max_image_preview": "seogo_onpage_advanced",
	"max_video_preview": "seogo_onpage_advanced", "bc_title": "seogo_onpage_advanced", "primary_category": "seogo_onpage_advanced",
	"og_title": "seogo_onpage_social", "og_description": "seogo_onpage_social", "og_image": "seogo_onpage_social",
	"tw_title": "seogo_onpage_social", "tw_description": "seogo_onpage_social", "tw_image": "seogo_onpage_social",
	"schema_page": "seogo_onpage_snippet", "schema_article": "seogo_onpage_snippet", "schema_custom": "seogo_onpage_snippet",
}

var seogoProductFields = []string{"price", "currency", "sku", "brand", "availability", "rating", "review_count"}

// seogoVarList is the "Insert variable" list of the title and description fields.
var seogoVarList = [][2]string{
	{"%title%", "Title"}, {"%sitename%", "Site title"}, {"%sitedesc%", "Tagline"}, {"%sep%", "Separator"}, {"%excerpt%", "Excerpt"},
	{"%primary_category%", "Primary category"}, {"%category%", "Categories"}, {"%tag%", "Tags"}, {"%name%", "Author name"},
	{"%date%", "Date"}, {"%modified%", "Modified date"}, {"%currentyear%", "Current year"}, {"%currentmonth%", "Current month"},
	{"%page%", "Page number"}, {"%focuskw%", "Focus keyphrase"}, {"%pt_single%", "Post type (singular)"}, {"%parent_title%", "Parent title"},
	{"%term_title%", "Term title"}, {"%term_description%", "Term description"}, {"%searchphrase%", "Search phrase"},
}

type seogoBoxData struct {
	PostID      int64
	Type        string
	Mode        string // classic, block
	Fields      map[string]string
	Product     map[string]string
	TitleTpl    string
	DescTpl     string
	Vars        [][2]string
	Can         map[string]bool
	Modules     map[string]bool
	Nonce       string
	SiteName    string
	HomeHost    string
	Categories  []*store.Term
	PageTypes   []string
	ArtTypes    []string
	DefaultPage string
	DefaultArt  string
	Term        bool
}

var seogoPageTypes = []string{"WebPage", "ItemPage", "AboutPage", "FAQPage", "QAPage", "ProfilePage", "ContactPage", "MedicalWebPage",
	"CollectionPage", "CheckoutPage", "RealEstateListing", "SearchResultsPage"}
var seogoArtTypes = []string{"None", "Article", "BlogPosting", "SocialMediaPosting", "NewsArticle", "AdvertiserContentArticle",
	"SatiricalArticle", "ScholarlyArticle", "TechArticle", "Report", "Product"}

func (s *Server) seogoBoxFor(c *gin.Context, meta map[string]any, typ string) *seogoBoxData {
	u := currentUser(c)
	o := s.seogoOpts()
	d := &seogoBoxData{Type: typ, Fields: map[string]string{}, Product: map[string]string{}, Vars: seogoVarList, Can: map[string]bool{},
		Modules: o.Modules, Nonce: s.nonce(c, "wp_rest"), SiteName: s.settings.String("blogname"), PageTypes: seogoPageTypes, ArtTypes: seogoArtTypes}
	for f := range seogoEditFields {
		d.Fields[f] = seogoMeta(meta, f)
	}
	for _, f := range seogoProductFields {
		d.Product[f], _ = seogoRaw(meta, "_seogo_product_"+f)
	}
	for _, cp := range seogoCaps {
		d.Can[cp.Key] = s.seogoCan(u, cp.Key)
	}
	to := o.Types[typ]
	d.TitleTpl, d.DescTpl, d.DefaultPage, d.DefaultArt = to.Title, to.Description, to.SchemaPage, to.SchemaArt
	if hu, err := url.Parse(s.links(c).HomeURL("/")); err == nil {
		d.HomeHost = hu.Host
	}
	return d
}

func (s *Server) seogoRenderPartial(name string, data any) template.HTML {
	var buf bytes.Buffer
	if err := s.tmpl.get(false).ExecuteTemplate(&buf, name, data); err != nil {
		return template.HTML("<!-- seogo: " + html.EscapeString(err.Error()) + " -->")
	}
	return template.HTML(buf.String())
}

// seogoEditor adds the Pursae SEO box to the classic editor or Gotenberg (renderEdit).
func (s *Server) seogoEditor(c *gin.Context, p *store.Post, block bool) {
	if !s.seogoOn() || p == nil || (p.Type != "post" && p.Type != "page" && p.Type != "product" && siteCPT(p.Type) == nil) || !s.seogoCan(currentUser(c), "seogo_onpage_general") {
		return
	}
	d := s.seogoBoxFor(c, p.Meta, p.Type)
	d.PostID = p.ID
	d.Mode = map[bool]string{true: "block", false: "classic"}[block]
	if p.Type == "post" {
		d.Categories, _ = s.store.HierarchicalTerms(c, "category", false)
	}
	box := s.seogoRenderPartial("seogo/metabox", d)
	cfg, _ := json.Marshal(map[string]any{"postId": p.ID, "mode": d.Mode, "nonce": d.Nonce, "root": "/wp-json/", "home": s.links(c).HomeURL("/"),
		"titleTpl": d.TitleTpl, "descTpl": d.DescTpl, "analysis": d.Modules["analysis"], "type": p.Type})
	a := s.pageAssets(c)
	a.Script("wp-api-fetch")
	blocks := ""
	if block {
		a.Script("wp-plugins")
		blocks = `<script` + cspNonceAttr() + ` src="/wp-admin/js/seogo-blocks.js?ver=1.1.0" defer></script>`
	}
	s.enqueueMedia(c, p.ID)
	addBodyHTML(c, template.HTML(`<link rel="stylesheet" id="seogo-editor-css" href="/wp-admin/css/seogo.css?ver=1.0.0" media="all" />`+
		`<div id="seogo-box-source" hidden>`)+box+template.HTML(`</div><script`+cspNonceAttr()+`>window.seogoEditor = `+string(cfg)+`;</script>`+
		`<script`+cspNonceAttr()+` src="/wp-admin/js/seogo-admin.js?ver=1.0.0" defer></script>`+blocks+
		`<script`+cspNonceAttr()+` src="/wp-admin/js/seogo-editor.js?ver=1.0.0" defer></script>`))
}

// seogoReadForm applies the box's seogo[...] fields to a post's meta (classic editor save).
func (s *Server) seogoReadForm(c *gin.Context, meta map[string]any) bool {
	if _, ok := c.GetPostForm("seogo_present"); !ok {
		return false
	}
	u := currentUser(c)
	for f, cp := range seogoEditFields {
		if !s.seogoCan(u, cp) {
			continue
		}
		v, ok := c.GetPostForm("seogo[" + f + "]")
		if f == "cornerstone" || f == "nofollow" {
			v = map[bool]string{true: "1", false: ""}[v == "1" || v == "on"]
			ok = true
		}
		if f == "robots_adv" {
			v, ok = strings.Join(c.PostFormArray("seogo[robots_adv][]"), ","), true
		}
		if !ok {
			continue
		}
		seogoSetMeta(meta, f, v)
	}
	if s.seogoCan(u, "seogo_onpage_snippet") {
		for _, f := range seogoProductFields {
			if v, ok := c.GetPostForm("seogo_product[" + f + "]"); ok {
				if v = strings.TrimSpace(v); v == "" {
					delete(meta, "_seogo_product_"+f)
				} else {
					meta["_seogo_product_"+f] = v
				}
			}
		}
	}
	return true
}

// seogoClassicSave is called by the classic editor's save before the post is written.
func (s *Server) seogoClassicSave(c *gin.Context, p *store.Post) {
	if !s.seogoOn() || p == nil {
		return
	}
	if p.Meta == nil {
		p.Meta = map[string]any{}
	}
	if s.seogoReadForm(c, p.Meta) {
		s.seogoStoreScores(c, p)
	}
}

// seogoRescoreSaved recomputes the stored scores of a post its save has just written (the title,
// slug and status a save fills in are known only then) when the save carried the Pursae SEO box.
func (s *Server) seogoRescoreSaved(c *gin.Context, p *store.Post) {
	if _, ok := c.GetPostForm("seogo_present"); !ok || !s.seogoOn() || p == nil || p.ID == 0 {
		return
	}
	if p.Meta == nil {
		p.Meta = map[string]any{}
	}
	s.seogoStoreScores(c, p)
	scores := map[string]any{}
	for _, k := range []string{"seo_score", "read_score", "rm_score"} {
		scores[seogoKey(k)] = p.Meta[seogoKey(k)]
	}
	_, _ = s.store.Pool.Exec(c, `UPDATE posts SET meta = coalesce(meta, '{}'::jsonb) || $2::jsonb WHERE id = $1`, p.ID, scores)
}

// seogoStoreScores keeps the current scores in meta for the dashboard and the bulk editor.
func (s *Server) seogoStoreScores(ctx context.Context, p *store.Post) {
	sc := &seogoCtx{s: s, ctx: ctx, lb: s.links(ctx), Kind: s.seogoQueryForPost(p).Kind, Post: p}
	in := s.seogoAnalysisInputFor(ctx, sc)
	seo, read := seogoAnalyzeSEO(in), seogoAnalyzeReadability(in)
	p.Meta[seogoKey("seo_score")] = strconv.Itoa(seo.Score)
	p.Meta[seogoKey("read_score")] = strconv.Itoa(read.Score)
	p.Meta[seogoKey("rm_score")] = strconv.Itoa(seo.Points)
}

func (s *Server) seogoAnalysisInputFor(ctx context.Context, sc *seogoCtx) seogoAnalysisInput {
	p := sc.Post
	in := seogoAnalysisInput{SEOTitle: sc.title(), PostTitle: p.Title, Description: sc.description(), Slug: p.Slug, Content: p.Content,
		Keywords: seogoFocusList(seogoMeta(p.Meta, "focus")), Cornerstone: seogoMeta(p.Meta, "cornerstone") == "1"}
	in.Locale = s.seogoPostLocale(ctx, p)
	if sc.Focus != "" {
		in.Keywords = seogoFocusList(sc.Focus)
	}
	if hu, err := url.Parse(sc.lb.HomeURL("/")); err == nil {
		in.SiteHost = hu.Host
	}
	if p.Status != "auto-draft" && p.Slug != "" {
		in.URL = s.seogoPostLoc(ctx, sc.lb, p)
	}
	if len(in.Keywords) > 0 {
		rows, err := s.store.Pool.Query(ctx, `SELECT title FROM posts WHERE id <> $1 AND type IN ('post','page') AND status IN ('publish','future','draft','pending','private')
			AND lower(trim(split_part(coalesce(meta->>'_seogo_focus', meta->>'_yoast_wpseo_focuskw', meta->>'rank_math_focus_keyword', ''), ',', 1))) = lower($2) LIMIT 10`, p.ID, in.Keywords[0])
		if err == nil {
			for rows.Next() {
				var t string
				if rows.Scan(&t) == nil {
					in.UsedBy = append(in.UsedBy, t)
				}
			}
			rows.Close()
		}
	}
	return in
}

// --- REST --------------------------------------------------------------------------------------

func (s *Server) registerSEOGoREST() {
	if s.sandbox == nil { // the block registry is process-wide; a sandbox leaves it to the site
		s.seogoRegisterBlocks()
	}
	api := s.rest
	const ns = "seogo/v1"
	guard := func(cp string, h restHandler) restHandler {
		return func(c *gin.Context, r *restRequest) {
			if !s.seogoOn() {
				restErr(c, http.StatusNotFound, "rest_no_route", "No route was found matching the URL and request method.")
				return
			}
			if cp != "" && !s.seogoCan(restCurrent(c), cp) {
				restErr(c, http.StatusForbidden, "rest_forbidden", "Sorry, you are not allowed to do that.")
				return
			}
			h(c, r)
		}
	}
	api.register(ns, "/seogo/v1/analyze", map[string]restHandler{http.MethodPost: guard("seogo_onpage_general", s.seogoRESTAnalyze)}, nil)
	api.register(ns, "/seogo/v1/meta/(?P<id>[\\d]+)", map[string]restHandler{
		http.MethodGet:  guard("seogo_onpage_general", s.seogoRESTMeta),
		http.MethodPost: guard("seogo_onpage_general", s.seogoRESTMeta),
	}, nil)
	api.register(ns, "/seogo/v1/links/suggest", map[string]restHandler{http.MethodGet: guard("seogo_onpage_general", s.seogoRESTSuggest)}, nil)
	api.register(ns, "/seogo/v1/get_head", map[string]restHandler{http.MethodGet: guard("", s.seogoRESTHead("seogo"))}, nil)
	api.register("yoast/v1", "/yoast/v1/get_head", map[string]restHandler{http.MethodGet: guard("", s.seogoRESTHead("yoast"))}, nil)
	api.register("rankmath/v1", "/rankmath/v1/getHead", map[string]restHandler{http.MethodGet: guard("", s.seogoRESTHead("rankmath"))}, nil)
}

func bodyString(r *restRequest, c *gin.Context, key string) string {
	v, _ := r.Str(c, key)
	return v
}

// seogoRESTAnalyze analyzes unsaved editor values: the resolved snippet and both analyses.
func (s *Server) seogoRESTAnalyze(c *gin.Context, r *restRequest) {
	ctx := c.Request.Context()
	id, _ := r.Int(c, "post_id")
	p := &store.Post{Type: "post", Status: "draft", Meta: map[string]any{}}
	if id > 0 {
		got, err := s.store.GetPost(ctx, id)
		if err != nil || !restCurrent(c).Can("edit_post", got) {
			restErr(c, http.StatusForbidden, "rest_forbidden", "Sorry, you are not allowed to edit this post.")
			return
		}
		cp := *got
		cp.Meta = map[string]any{}
		for k, v := range got.Meta {
			cp.Meta[k] = v
		}
		p = &cp
	}
	if v, ok := r.Str(c, "title"); ok {
		p.Title = v
	}
	if v, ok := r.Str(c, "content"); ok {
		p.Content = v
	}
	if v, ok := r.Str(c, "excerpt"); ok {
		p.Excerpt = v
	}
	if v, ok := r.Str(c, "slug"); ok && v != "" {
		p.Slug = v
	}
	if p.Slug == "" && p.Title != "" {
		p.Slug = sanitizeSlug(p.Title)
	}
	if fields, ok := r.Body["fields"].(map[string]any); ok {
		for f := range seogoEditFields {
			if v, ok := fields[f].(string); ok {
				p.Meta[seogoKey(f)] = v
			}
		}
	}
	sc := &seogoCtx{s: s, ctx: ctx, lb: s.links(ctx), Kind: s.seogoQueryForPost(p).Kind, Post: p}
	in := s.seogoAnalysisInputFor(ctx, sc)
	if p.Status == "auto-draft" || in.URL == "" {
		cp := *p
		cp.Status = "publish"
		in.URL = sc.lb.Permalink(&cp)
	}
	if p.Meta[seogoKey("cornerstone")] == "1" {
		in.Cornerstone = true
	}
	out := map[string]any{"title": in.SEOTitle, "description": in.Description, "url": in.URL, "keywords": in.Keywords}
	if in.Description == "" {
		out["description_fallback"] = sc.excerpt(false)
	}
	if s.seogoOpts().Modules["analysis"] {
		out["seo"] = seogoAnalyzeSEO(in)
		out["readability"] = seogoAnalyzeReadability(in)
	}
	out["og_image"] = ""
	if img := sc.image("og_image"); img != nil {
		out["og_image"] = img.URL
	}
	restJSON(c, http.StatusOK, out)
}

func sanitizeSlug(t string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(t) {
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

// seogoRESTMeta reads or writes the SEO fields of a post (the Gotenberg sidebar saves with it).
func (s *Server) seogoRESTMeta(c *gin.Context, r *restRequest) {
	ctx := c.Request.Context()
	u := restCurrent(c)
	p, err := s.store.GetPost(ctx, r.ID())
	if err != nil || !u.Can("edit_post", p) {
		restErr(c, http.StatusForbidden, "rest_forbidden", "Sorry, you are not allowed to edit this post.")
		return
	}
	if r.Method == http.MethodPost {
		if p.Meta == nil {
			p.Meta = map[string]any{}
		}
		fields, _ := r.Body["fields"].(map[string]any)
		for f, cp := range seogoEditFields {
			v, ok := fields[f]
			if !ok || !s.seogoCan(u, cp) {
				continue
			}
			switch t := v.(type) {
			case string:
				seogoSetMeta(p.Meta, f, t)
			case bool:
				seogoSetMeta(p.Meta, f, map[bool]string{true: "1", false: ""}[t])
			case []any:
				var parts []string
				for _, x := range t {
					if sx, ok := x.(string); ok {
						parts = append(parts, sx)
					}
				}
				seogoSetMeta(p.Meta, f, strings.Join(parts, ","))
			}
		}
		if prod, ok := r.Body["product"].(map[string]any); ok && s.seogoCan(u, "seogo_onpage_snippet") {
			for _, f := range seogoProductFields {
				if v, ok := prod[f].(string); ok {
					if v = strings.TrimSpace(v); v == "" {
						delete(p.Meta, "_seogo_product_"+f)
					} else {
						p.Meta["_seogo_product_"+f] = v
					}
				}
			}
		}
		s.seogoStoreScores(ctx, p)
		if _, err := s.store.Pool.Exec(ctx, `UPDATE posts SET meta = $2 WHERE id = $1`, p.ID, seogoJSON(p.Meta)); err != nil {
			restErr(c, http.StatusInternalServerError, "db_update_error", "Could not update the SEO data.")
			return
		}
		s.purgeTags("post:" + itoa64(p.ID))
	}
	out := map[string]any{"id": p.ID, "fields": map[string]string{}, "product": map[string]string{}}
	for f := range seogoEditFields {
		out["fields"].(map[string]string)[f] = seogoMeta(p.Meta, f)
	}
	for _, f := range seogoProductFields {
		out["product"].(map[string]string)[f], _ = seogoRaw(p.Meta, "_seogo_product_"+f)
	}
	for _, f := range []string{"seo_score", "read_score", "rm_score"} {
		out[f], _ = seogoRaw(p.Meta, seogoKey(f))
	}
	restJSON(c, http.StatusOK, out)
}

func seogoJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// seogoRESTSuggest is the internal linking suggestions list: published posts that share the most
// significant words of the text being written (Yoast Premium's "Internal linking suggestions").
func (s *Server) seogoRESTSuggest(c *gin.Context, r *restRequest) {
	ctx := c.Request.Context()
	id, _ := r.Int(c, "post_id")
	text := bodyString(r, c, "q")
	restJSON(c, http.StatusOK, s.seogoSuggestions(ctx, id, text, 10))
}

type seogoSuggestion struct {
	ID          int64  `json:"id"`
	Title       string `json:"title"`
	URL         string `json:"url"`
	Cornerstone bool   `json:"cornerstone"`
	Linked      bool   `json:"linked"`
}

func (s *Server) seogoSuggestions(ctx context.Context, postID int64, text string, limit int) []seogoSuggestion {
	freq := map[string]int{}
	for _, w := range words(plainText(text)) {
		if len(w) > 3 && !functionWords[w] {
			freq[w]++
		}
	}
	type kv struct {
		w string
		n int
	}
	var top []kv
	for w, n := range freq {
		top = append(top, kv{w, n})
	}
	for i := 1; i < len(top); i++ {
		for j := i; j > 0 && (top[j].n > top[j-1].n || (top[j].n == top[j-1].n && top[j].w < top[j-1].w)); j-- {
			top[j], top[j-1] = top[j-1], top[j]
		}
	}
	if len(top) > 12 {
		top = top[:12]
	}
	out := []seogoSuggestion{}
	if len(top) == 0 {
		return out
	}
	var terms []string
	for _, t := range top {
		terms = append(terms, strings.ReplaceAll(t.w, "'", ""))
	}
	rows, err := s.store.Pool.Query(ctx, `SELECT id FROM posts WHERE status = 'publish' AND type IN ('post','page') AND id <> $1 AND password = ''
		AND search @@ to_tsquery('simple', `+store.FoldedArgSQL("$2")+`)
		ORDER BY (coalesce(meta->>'_seogo_cornerstone', meta->>'_yoast_wpseo_is_cornerstone', '') = '1') DESC,
			ts_rank(search, to_tsquery('simple', `+store.FoldedArgSQL("$2")+`)) DESC LIMIT $3`, postID, strings.Join(terms, " | "), limit) // folded like posts.search (0105)
	if err != nil {
		return out
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	lb := s.links(ctx)
	for _, id := range ids {
		p, err := s.store.GetPost(ctx, id)
		if err != nil {
			continue
		}
		u := lb.Permalink(p)
		out = append(out, seogoSuggestion{ID: p.ID, Title: p.Title, URL: u, Cornerstone: seogoMeta(p.Meta, "cornerstone") == "1",
			Linked: strings.Contains(text, u) || strings.Contains(text, "/?p="+itoa64(p.ID))})
	}
	return out
}

// seogoRESTHead is Yoast's /yoast/v1/get_head (html + json + status), Rank Math's
// /rankmath/v1/getHead (success + head) and Pursae SEO's own (Yoast's shape).
func (s *Server) seogoRESTHead(flavor string) restHandler {
	return func(c *gin.Context, r *restRequest) {
		raw := c.Query("url")
		u, err := url.Parse(raw)
		if raw == "" || err != nil {
			restErr(c, http.StatusBadRequest, "rest_invalid_param", "Invalid parameter(s): url")
			return
		}
		lb := s.links(c)
		home, _ := url.Parse(lb.HomeURL("/"))
		if u.Host != "" && home != nil && !strings.EqualFold(u.Host, home.Host) {
			restErr(c, http.StatusBadRequest, "rest_invalid_param", "The URL does not belong to this site.")
			return
		}
		req := c.Request.Clone(c.Request.Context())
		req.URL = &url.URL{Path: "/" + strings.TrimLeft(u.Path, "/"), RawQuery: u.RawQuery}
		c2 := c.Copy()
		c2.Request = req
		path := strings.Trim(u.Path, "/")
		q, err := s.resolve(c2, path)
		status := http.StatusOK
		if err != nil || q == nil || strings.HasPrefix(q.Kind, "redirect:") {
			q, status = &siteQuery{Kind: "404"}, http.StatusNotFound
		}
		if q.Post != nil && (q.Post.Status != "publish" || q.Post.Password != "" || s.gooSingleAccess(c2, q.Post.ID, q.Post.Type) != "") {
			q, status = &siteQuery{Kind: "404"}, http.StatusNotFound
		}
		sc := s.seogoCtxFor(c2.Request.Context(), q)
		sc.lb = lb
		title := sc.title()
		head := "<title>" + html.EscapeString(title) + "</title>\n" + s.seogoHeadFor(sc, false)
		if flavor == "rankmath" {
			restJSON(c, http.StatusOK, map[string]any{"success": true, "head": head})
			return
		}
		restJSON(c, http.StatusOK, map[string]any{"html": head, "json": s.seogoHeadJSON(sc, title), "status": status})
	}
}

// seogoHeadJSON is the json half of get_head (Yoast's yoast_head_json).
func (s *Server) seogoHeadJSON(sc *seogoCtx, title string) map[string]any {
	o := s.seogoOpts()
	desc := sc.description()
	robots := sc.robots(false)
	rm := map[string]string{"index": robots[0], "follow": robots[1]}
	for _, d := range robots[2:] {
		if k, v, ok := strings.Cut(d, ":"); ok {
			rm[k] = k + ":" + v
		} else {
			rm[d] = d
		}
	}
	out := map[string]any{"title": title, "robots": rm, "og_locale": s.seogoLocale(sc.ctx), "og_title": title, "og_site_name": s.seogoSiteName(sc.ctx),
		"twitter_card": o.TwitterCard}
	if desc != "" {
		out["description"], out["og_description"] = desc, desc
	}
	if can := sc.canonical(); can != "" {
		out["canonical"], out["og_url"] = can, can
	}
	out["og_type"] = "website"
	if sc.singular() && sc.Post.Type == "post" && sc.Kind == "single" {
		out["og_type"] = "article"
		if sc.Post.PublishedAt != nil {
			out["article_published_time"] = sc.Post.PublishedAt.UTC().Format("2006-01-02T15:04:05+00:00")
		}
		out["article_modified_time"] = sc.Post.ModifiedAt.UTC().Format("2006-01-02T15:04:05+00:00")
		if a := sc.author(); a != nil {
			out["author"] = a.DisplayName
			out["twitter_misc"] = map[string]string{"Written by": a.DisplayName, "Est. reading time": strconv.Itoa(readingTime(plainText(sc.Post.Content))) + " minutes"}
		}
	}
	if img := sc.image("og_image"); img != nil {
		im := map[string]any{"url": img.URL}
		if img.Width > 0 {
			im["width"], im["height"] = img.Width, img.Height
		}
		if img.Mime != "" {
			im["type"] = img.Mime
		}
		out["og_image"] = []any{im}
	}
	if o.Modules["schema"] {
		out["schema"] = map[string]any{"@context": "https://schema.org", "@graph": s.seogoGraph(sc, title, desc)}
	}
	return out
}

// --- term edit screen --------------------------------------------------------------------------

// seogoTermBox adds the SEO fields to the Edit Category / Edit Tag screen.
func (s *Server) seogoTermBox(c *gin.Context, t *store.Term) {
	if !s.seogoOn() || t == nil || !s.seogoCan(currentUser(c), "seogo_onpage_general") {
		return
	}
	tax := t.Taxonomy
	d := s.seogoBoxFor(c, t.Meta, tax)
	d.Term = true
	d.Mode = "term"
	addBodyHTML(c, template.HTML(`<link rel="stylesheet" href="/wp-admin/css/seogo.css?ver=1.0.0" media="all" /><div id="seogo-box-source" hidden>`)+
		s.seogoRenderPartial("seogo/termbox", d)+template.HTML(`</div><script`+cspNonceAttr()+`>(function(){var src=document.getElementById('seogo-box-source'),f=document.getElementById('edittag');if(src&&f){var t=f.querySelector('.form-table')||f;t.parentNode.insertBefore(src.firstElementChild,t.nextSibling);}})();</script>`))
}

// seogoTermSave stores the term SEO fields before the term is written.
func (s *Server) seogoTermSave(c *gin.Context, t *store.Term) {
	if !s.seogoOn() || t == nil {
		return
	}
	if t.Meta == nil {
		t.Meta = map[string]any{}
	}
	s.seogoReadForm(c, t.Meta)
}
