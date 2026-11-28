package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"wpgo/internal/store"
	"wpgo/internal/webbuilder"
)

// Pursae Web Builder REST API, namespace web-builder/v2 (docs/developer/web-builder-contract.md §3):
// documents (load, autosave, edit lock, publish), revisions, the site's saved components, the
// user's editor preferences and the local Google Fonts stylesheet. The public form endpoint lives
// in webbuilder_forms.go.
//
// Authentication is the core one (restAuthUser: cookie + X-WP-Nonce, or an Application Password).
// Every route answers 404 rest_no_route while the plugin is off, and every route but fonts/css2
// needs the X-WB-Api handshake header. The snapshot and CSS a publish carries come from the browser
// and are untrusted: the doc is validated and normalized by internal/webbuilder, the snapshot and
// CSS are sanitized there, and posts.content is generated in Go from the doc.

const (
	wbRevisionsPerPage = webbuilder.MaxRevisionsListed
	wbNameMax          = 200 // saved component name, runes
	wbTitleMax         = 1000
	wbSlugMax          = 200
	wbDescriptionMax   = 5000
	wbFontsPerRequest  = 12
	wbFontsWait        = 20 * time.Second // how long the editor's stylesheet waits for a first download
	wbFontsDiskCap     = 256 << 20        // no new download once the localized fonts take this much space
	wbFontsDiskTTL     = 60 * time.Second // how long a measured disk usage is trusted
	wbPublishLockClass = int32(0x5742)    // pg_advisory_xact_lock(class, post id): one publish per post at a time
	wbFontsCacheMaxAge = "private, max-age=3600"
)

// wbREST wraps a web-builder/v2 handler: 404 while the plugin is off and, unless exempt, the
// X-WB-Api handshake (409 wb_api_mismatch, data.reload).
func (s *Server) wbREST(h restHandler, handshake bool) restHandler {
	return func(c *gin.Context, r *restRequest) {
		if !s.wbOn() {
			restErr(c, http.StatusNotFound, "rest_no_route", "No route was found matching the URL and request method.")
			return
		}
		if handshake && strings.TrimSpace(c.GetHeader(webbuilder.APIHeader)) != strconv.Itoa(webbuilder.APIVersion) {
			restErr(c, http.StatusConflict, webbuilder.ErrAPIMismatch, "The editor is out of date. Reload it to keep working.",
				map[string]any{"reload": true, "apiVersion": webbuilder.APIVersion})
			return
		}
		h(c, r)
	}
}

// registerWebBuilderREST adds the web-builder/v2 routes (the handlers check wbOn on every request,
// so switching the plugin needs no restart).
func (s *Server) registerWebBuilderREST() {
	const ns = webbuilder.RESTNamespace
	const doc = `/web-builder/v2/documents/(?P<id>[\d]+)`
	reg := func(pattern string, methods map[string]restHandler, handshake bool) {
		wrapped := make(map[string]restHandler, len(methods))
		for m, h := range methods {
			wrapped[m] = s.wbREST(h, handshake)
		}
		s.rest.register(ns, pattern, wrapped, nil)
	}
	reg(doc, map[string]restHandler{http.MethodGet: s.wbRESTDocument}, true)
	reg(doc+`/autosave`, map[string]restHandler{http.MethodPut: s.wbRESTAutosave, http.MethodDelete: s.wbRESTAutosaveDelete}, true)
	reg(doc+`/lock`, map[string]restHandler{http.MethodPost: s.wbRESTLock}, true)
	reg(doc+`/publish`, map[string]restHandler{http.MethodPost: s.wbRESTPublish}, true)
	reg(doc+`/languages`, map[string]restHandler{http.MethodGet: s.wbRESTLanguages}, true)
	reg(doc+`/plugins`, map[string]restHandler{http.MethodGet: s.wbRESTPlugins}, true)
	reg(doc+`/translations`, map[string]restHandler{http.MethodPost: s.wbRESTTranslate}, true)
	reg(doc+`/revisions`, map[string]restHandler{http.MethodGet: s.wbRESTRevisions}, true)
	reg(doc+`/revisions/(?P<rid>[\d]+)`, map[string]restHandler{http.MethodGet: s.wbRESTRevision}, true)
	reg(doc+`/revisions/(?P<rid>[\d]+)/restore`, map[string]restHandler{http.MethodPost: s.wbRESTRestore}, true)
	reg(`/web-builder/v2/library`, map[string]restHandler{http.MethodGet: s.wbRESTLibrary, http.MethodPost: s.wbRESTLibraryCreate}, true)
	reg(`/web-builder/v2/library/(?P<id>[\d]+)`, map[string]restHandler{http.MethodDelete: s.wbRESTLibraryDelete}, true)
	reg(`/web-builder/v2/prefs`, map[string]restHandler{http.MethodGet: s.wbRESTPrefs, http.MethodPut: s.wbRESTPrefsSave}, true)
	reg(`/web-builder/v2/fonts/css2`, map[string]restHandler{http.MethodGet: s.wbRESTFontsCSS}, false) // a stylesheet: nonce in ?_wpnonce=
	reg(`/web-builder/v2/site-settings`, map[string]restHandler{http.MethodGet: s.wbRESTSiteSettings, http.MethodPut: s.wbRESTSiteSettingsSave}, true)
	reg(`/web-builder/v2/outdated`, map[string]restHandler{http.MethodGet: s.wbRESTOutdated}, true)
	// Widgets of Wasm plugins (plugins_ui.go)
	reg(`/web-builder/v2/plugin-widgets`, map[string]restHandler{http.MethodGet: s.wbPluginWidgets}, true)
	reg(`/web-builder/v2/plugin-widgets/render`, map[string]restHandler{http.MethodPost: s.wbPluginWidgetRender}, true)
	// Theme Builder (webbuilder_templates_rest.go)
	s.wbRegisterTemplateREST(reg)
	// Theme library (webbuilder_themes.go)
	s.wbRegisterThemeREST(reg)
	// Design kits for code sections (webbuilder_kits.go)
	s.wbRegisterKitREST(reg)
	// A user's script trust of a kit (webbuilder_trust.go)
	reg(wbKitTrustRoute, map[string]restHandler{http.MethodPut: s.wbRESTKitTrust}, true)
}

// --- shared helpers --------------------------------------------------------------------------------

func wbTime(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// wbForbidden answers 401 for a logged-out request and 403 otherwise (core rest_forbidden).
func wbForbidden(c *gin.Context, u *store.User) {
	if u == nil {
		restErr(c, http.StatusUnauthorized, webbuilder.ErrForbidden, "Sorry, you are not allowed to do that.")
		return
	}
	restErr(c, http.StatusForbidden, webbuilder.ErrForbidden, "Sorry, you are not allowed to do that.")
}

// wbUser is the request's user when it has cap (edit_posts and the like), else it answers 401/403.
func wbUser(c *gin.Context, cap string) *store.User {
	u := restCurrent(c)
	if u == nil || !u.Can(cap, nil) {
		wbForbidden(c, u)
		return nil
	}
	return u
}

// wbPost loads the {id} post for a document route: logged in, a builder post type (or a Theme
// Builder template), not trashed and editable by the user (wbEditable: edit_post, or
// edit_theme_options for a template). It answers the error itself and returns nil on failure.
func (s *Server) wbPost(c *gin.Context, r *restRequest) (*store.User, *store.Post) {
	u := restCurrent(c)
	if u == nil {
		wbForbidden(c, nil)
		return nil, nil
	}
	p, err := s.store.GetPost(c, r.ID())
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			restErr(c, http.StatusNotFound, webbuilder.ErrNotFound, "Invalid post ID.")
		} else {
			restErr(c, http.StatusInternalServerError, "db_error", "Could not read the post.")
		}
		return nil, nil
	}
	if !s.wbEnabledType(p.Type) && !wbIsTemplate(p) {
		restErr(c, http.StatusNotFound, webbuilder.ErrNotBuilder, "Pursae Web Builder is not enabled for this post type.")
		return nil, nil
	}
	if p.Status == "trash" {
		restErr(c, http.StatusNotFound, webbuilder.ErrNotFound, "Invalid post ID.")
		return nil, nil
	}
	if !s.wbEditable(u, p) {
		wbForbidden(c, u)
		return nil, nil
	}
	return u, p
}

// wbBody is the raw request body, at most limit bytes (413 wb_too_large, data.limit). The REST
// dispatcher has already decoded a JSON body into r.Body; it is re-encoded (without HTML escaping)
// so the doc parser sees one JSON object either way.
func wbBody(c *gin.Context, r *restRequest, limit int) ([]byte, bool) {
	tooLarge := func() ([]byte, bool) {
		restErr(c, http.StatusRequestEntityTooLarge, webbuilder.ErrTooLarge, "The request body is too large.", map[string]any{"limit": limit})
		return nil, false
	}
	if c.Request.ContentLength > int64(limit) {
		return tooLarge()
	}
	var raw []byte
	if len(r.Body) > 0 {
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(r.Body); err != nil {
			restErr(c, http.StatusBadRequest, "rest_invalid_json", "Invalid JSON body passed.")
			return nil, false
		}
		raw = bytes.TrimSpace(buf.Bytes())
	} else if c.Request.Body != nil {
		b, err := io.ReadAll(io.LimitReader(c.Request.Body, int64(limit)+1))
		if err != nil {
			restErr(c, http.StatusBadRequest, "rest_invalid_json", "Invalid JSON body passed.")
			return nil, false
		}
		raw = bytes.TrimSpace(b)
	}
	if len(raw) > limit {
		return tooLarge()
	}
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	return raw, true
}

// wbDecode decodes the body into v; 400 rest_invalid_json on failure.
func wbDecode(c *gin.Context, raw []byte, v any) bool {
	if err := json.Unmarshal(raw, v); err != nil {
		restErr(c, http.StatusBadRequest, "rest_invalid_json", "Invalid JSON body passed.")
		return false
	}
	return true
}

func wbInvalid(c *gin.Context, errs []webbuilder.ValidationError) {
	msg := "The page could not be saved: it is not valid."
	if len(errs) > 0 && errs[0].Message != "" {
		msg = errs[0].Message
	}
	restErr(c, http.StatusUnprocessableEntity, webbuilder.ErrInvalidDoc, msg, map[string]any{"errors": errs})
}

func wbInvalid1(c *gin.Context, path, code, msg string) {
	wbInvalid(c, []webbuilder.ValidationError{{Path: path, Code: code, Message: msg}})
}

// wbUnfiltered: u may keep the privileged parts of a doc (html and code sections, content.html,
// custom CSS) in this request: unfiltered_html, and with an Application Password the themes scope
// as well (restScopeSiteCode).
func wbUnfiltered(c *gin.Context, u *store.User) bool {
	return u != nil && u.Can("unfiltered_html", nil) && (c == nil || restScopeSiteCode(c))
}

// wbParseDoc validates and normalizes a doc for u. Without unfiltered_html the privileged parts
// (html and code sections, content.html, style.css) are those of the stored docs, by section id
// (webbuilder.DocOptions.Stored; see wbStoredDocs), and dropped where nothing is stored. It answers
// 413 / 422 itself.
func wbParseDoc(c *gin.Context, raw json.RawMessage, u *store.User, stored ...json.RawMessage) (*webbuilder.DocResult, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		wbInvalid1(c, "/doc", webbuilder.VErrRequired, "The doc is required.")
		return nil, false
	}
	res, err := webbuilder.ParseDoc(raw, webbuilder.DocOptions{UnfilteredHTML: wbUnfiltered(c, u), PathPrefix: "/doc", Stored: stored})
	switch {
	case errors.Is(err, webbuilder.ErrDocTooLarge):
		restErr(c, http.StatusRequestEntityTooLarge, webbuilder.ErrTooLarge, "The page is too large.", map[string]any{"limit": webbuilder.MaxDocBytes})
		return nil, false
	case err != nil:
		wbInvalid1(c, "/doc", webbuilder.VErrRequired, "The doc is not a valid page document.")
		return nil, false
	case !res.Valid():
		wbInvalid(c, res.Errors)
		return nil, false
	}
	return res, true
}

// wbStoredDocs are the docs whose privileged parts a user without unfiltered_html keeps when saving
// post id (wbParseDoc): first the published doc (row, read when nil), then the user's own draft.
// Neither holds privileged parts that an unfiltered_html author did not write: publishes and
// autosaves of other users keep them only from these, and a restored revision was such a publish.
// nil for users with unfiltered_html.
func (s *Server) wbStoredDocs(c *gin.Context, id int64, u *store.User, row *store.WebBuilderDoc) []json.RawMessage {
	if u == nil || wbUnfiltered(c, u) {
		return nil
	}
	ctx := c.Request.Context()
	wb := s.store.WebBuilder()
	var out []json.RawMessage
	if row == nil {
		row, _ = wb.Doc(ctx, id)
	}
	if row != nil && len(row.Doc) > 0 {
		out = append(out, row.Doc)
	}
	if dr, err := wb.Draft(ctx, id, u.ID); err == nil && len(dr.Doc) > 0 {
		out = append(out, dr.Doc)
	}
	return out
}

// wbPage is a normalized PageSettings plus its JSON: the client's object with the four known
// fields replaced by their normalized values (fields a newer editor adds are kept).
func wbPage(raw json.RawMessage) (webbuilder.PageSettings, json.RawMessage) {
	var ps webbuilder.PageSettings
	obj := map[string]any{}
	if len(raw) > 0 && string(raw) != "null" {
		_ = json.Unmarshal(raw, &obj)
		_ = json.Unmarshal(raw, &ps)
	}
	if obj == nil {
		obj = map[string]any{}
	}
	ps.Title = wbClip(strings.TrimSpace(ps.Title), wbTitleMax)
	ps.Slug = wbClip(strings.TrimSpace(ps.Slug), wbSlugMax)
	ps.Description = wbClip(strings.TrimSpace(ps.Description), wbDescriptionMax)
	if _, ok := obj["indexable"]; !ok {
		ps.Indexable = true // a page is indexable unless it says otherwise
	}
	return ps, wbPageMerge(obj, ps)
}

// wbPageLoadedKey is the page object key the editor's boot page carries (wbDocumentFor) and the
// editor sends back unchanged: the post's description and indexable flag as that session loaded
// them, and the rev it loaded. It is never stored with a published doc or a revision; the user's
// own draft keeps it (autosave) so a restored draft is compared with its session's values.
const wbPageLoadedKey = "_loaded"

type wbPageLoaded struct {
	Rev         int    `json:"rev"`
	Description string `json:"description"`
	Indexable   bool   `json:"indexable"`
}

// wbPageChanged reports whether the posted page settings change the description and the indexable
// flag the editor last knew: the values its session loaded (the page's _loaded key) while it has
// not published since (_loaded.rev = baseRev), else the last published page object (row.Page, the
// session's own publish). Without either, or a page object that lacks the key, the field counts
// as changed. Pursae SEO edits made after the editor loaded the page then survive a publish that
// did not touch the field.
func wbPageChanged(row *store.WebBuilderDoc, body json.RawMessage, baseRev int, ps webbuilder.PageSettings) (desc, indexable bool) {
	var obj map[string]json.RawMessage
	_ = json.Unmarshal(body, &obj)
	if raw, ok := obj[wbPageLoadedKey]; ok {
		var l wbPageLoaded
		if json.Unmarshal(raw, &l) == nil && l.Rev == baseRev {
			return wbClip(strings.TrimSpace(l.Description), wbDescriptionMax) != ps.Description, l.Indexable != ps.Indexable
		}
	}
	if row == nil {
		return true, true
	}
	obj = nil
	if len(row.Page) == 0 || json.Unmarshal(row.Page, &obj) != nil {
		return true, true
	}
	prev, _ := wbPage(row.Page)
	_, hasDesc := obj["description"]
	_, hasIndex := obj["indexable"]
	return !hasDesc || prev.Description != ps.Description, !hasIndex || prev.Indexable != ps.Indexable
}

// wbPageMissing reports a request without page settings (absent or null).
func wbPageMissing(raw json.RawMessage) bool {
	t := strings.TrimSpace(string(raw))
	return t == "" || t == "null"
}

func wbPageMerge(obj map[string]any, ps webbuilder.PageSettings) json.RawMessage {
	delete(obj, wbPageLoadedKey) // boot-only; autosave alone puts it back (the draft keeps it)
	obj["title"], obj["slug"], obj["description"], obj["indexable"] = ps.Title, ps.Slug, ps.Description, ps.Indexable
	b, _ := json.Marshal(obj)
	return b
}

// wbClip shortens s to at most n runes.
func wbClip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n])
}

// wbUserRefs resolves user ids to {id, name}, once per id per request.
type wbUserRefs struct {
	s     *Server
	ctx   context.Context
	cache map[int64]webbuilder.UserRef
}

func (s *Server) wbUserRefs(ctx context.Context) *wbUserRefs {
	return &wbUserRefs{s: s, ctx: ctx, cache: map[int64]webbuilder.UserRef{}}
}

func (r *wbUserRefs) get(id int64) webbuilder.UserRef {
	if ref, ok := r.cache[id]; ok {
		return ref
	}
	ref := webbuilder.UserRef{ID: id}
	if id > 0 {
		if u, err := r.s.store.UserByID(r.ctx, id); err == nil {
			ref.Name = wbUserName(u)
		}
	}
	r.cache[id] = ref
	return ref
}

// --- documents -------------------------------------------------------------------------------------

// wbRESTDocument is GET documents/{id}: the doc, the user's newer draft and another user's lock.
func (s *Server) wbRESTDocument(c *gin.Context, r *restRequest) {
	u, p := s.wbPost(c, r)
	if p == nil {
		return
	}
	d, err := s.wbDocumentFor(c, p, u)
	if err != nil {
		restErr(c, http.StatusInternalServerError, "db_error", "Could not read the page.")
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, d)
}

type wbAutosaveBody struct {
	Doc     json.RawMessage `json:"doc"`
	Page    json.RawMessage `json:"page"`
	BaseRev int             `json:"baseRev"`
}

// wbRESTAutosave is PUT documents/{id}/autosave: the user's draft row (the live page is untouched).
func (s *Server) wbRESTAutosave(c *gin.Context, r *restRequest) {
	u, p := s.wbPost(c, r)
	if p == nil {
		return
	}
	raw, ok := wbBody(c, r, webbuilder.MaxDocBytes)
	if !ok {
		return
	}
	var body wbAutosaveBody
	if !wbDecode(c, raw, &body) {
		return
	}
	res, ok := wbParseDoc(c, body.Doc, u, s.wbStoredDocs(c, p.ID, u, nil)...)
	if !ok {
		return
	}
	_, pageJSON := wbPage(body.Page)
	if l, ok := wbPageLoadedOf(body.Page); ok {
		// The draft keeps what its session loaded, so a restored draft is compared with that on
		// publish (wbPageChanged), not with the post as it is when the editor reopens.
		l.Description = wbClip(strings.TrimSpace(l.Description), wbDescriptionMax)
		pageJSON = wbPageStamp(pageJSON, l)
	}
	wb := s.store.WebBuilder()
	d := &store.WebBuilderDraft{PostID: p.ID, UserID: u.ID, Doc: res.JSON, Page: pageJSON, BaseRev: body.BaseRev}
	if err := wb.SaveDraft(c, d); err != nil {
		restErr(c, http.StatusInternalServerError, "db_error", "Could not save the draft.")
		return
	}
	_, _ = s.wbTakeLock(c, p.ID, u.ID, false) // autosave refreshes the caller's own (or a free) lock
	rev, _ := wb.DocRev(c, p.ID)
	c.JSON(http.StatusOK, webbuilder.AutosaveResponse{SavedAt: wbTime(d.UpdatedAt), Rev: rev})
}

// wbRESTAutosaveDelete is DELETE documents/{id}/autosave.
func (s *Server) wbRESTAutosaveDelete(c *gin.Context, r *restRequest) {
	u, p := s.wbPost(c, r)
	if p == nil {
		return
	}
	if err := s.store.WebBuilder().DeleteDraft(c, p.ID, u.ID); err != nil {
		restErr(c, http.StatusInternalServerError, "db_error", "Could not delete the draft.")
		return
	}
	c.JSON(http.StatusOK, map[string]any{"deleted": true})
}

// wbRESTLock is POST documents/{id}/lock: take a free or own lock, or with take another user's.
func (s *Server) wbRESTLock(c *gin.Context, r *restRequest) {
	u, p := s.wbPost(c, r)
	if p == nil {
		return
	}
	raw, ok := wbBody(c, r, 4<<10)
	if !ok {
		return
	}
	var body webbuilder.LockRequest
	if !wbDecode(c, raw, &body) {
		return
	}
	held, err := s.wbTakeLock(c, p.ID, u.ID, body.Take)
	if err != nil {
		restErr(c, http.StatusInternalServerError, "db_error", "Could not lock the page.")
		return
	}
	ttl := time.Duration(webbuilder.LockTTLSeconds) * time.Second
	if held {
		c.JSON(http.StatusOK, webbuilder.LockResponse{Lock: nil, Expires: wbTime(time.Now().Add(ttl))})
		return
	}
	fresh, err := s.store.GetPost(c, p.ID)
	if err != nil {
		restErr(c, http.StatusInternalServerError, "db_error", "Could not read the post.")
		return
	}
	resp := webbuilder.LockResponse{Lock: s.wbLockHolder(c, fresh, u.ID), Expires: wbTime(time.Now().Add(ttl))}
	if resp.Lock != nil {
		if since, err := time.Parse(time.RFC3339, resp.Lock.Since); err == nil {
			resp.Expires = wbTime(since.Add(ttl))
		}
	}
	c.JSON(http.StatusOK, resp)
}

type wbPublishBody struct {
	BaseRev    int                     `json:"baseRev"`
	Status     string                  `json:"status"`
	Date       string                  `json:"date"`
	Reattach   bool                    `json:"reattach"`
	Doc        json.RawMessage         `json:"doc"`
	Page       json.RawMessage         `json:"page"`
	Render     webbuilder.RenderResult `json:"render"`
	UtilityCSS string                  `json:"utilityCss"`
	// ServerRender asks the server to draw the snapshot itself (webbuilder_ssr.go) instead of taking
	// render from the body: how Pursae MCP and other clients without the editor publish.
	ServerRender bool `json:"serverRender"`
}

// wbSnapshot is the sanitized part of a publish.
type wbSnapshot struct {
	html, css string
	features  []string
	stripped  []string
}

// wbSanitizeRender cleans the snapshot and CSS of a publish (contract steps 4 and 5). Without
// unfiltered_html the section CSS (render.css) is dropped and srcdoc embeds are removed by the
// snapshot sanitizer. It answers 413 / 422 itself.
func (s *Server) wbSanitizeRender(c *gin.Context, p *store.Post, body *wbPublishBody, unfiltered bool, stripped []string) (*wbSnapshot, bool) {
	out := &wbSnapshot{stripped: append([]string{}, stripped...)}
	add := func(what string) {
		for _, x := range out.stripped {
			if x == what {
				return
			}
		}
		out.stripped = append(out.stripped, what)
	}
	rr := &body.Render
	if rr.Renderer != webbuilder.RendererVersion {
		wbInvalid1(c, "/render/renderer", webbuilder.VErrRenderer, "The page was rendered by an editor of another version. Reload the editor.")
		return nil, false
	}
	lb := s.links(c)
	html, changed, err := webbuilder.SanitizeSnapshot(rr.HTML, webbuilder.SnapshotOptions{
		FormActionPrefix: lb.HomeURL("/wp-json/" + webbuilder.RESTNamespace + "/forms/" + itoa64(p.ID) + "/"),
		EmbedHTML:        unfiltered,
		SearchAction:     lb.HomeURL("/"),
	})
	switch {
	case errors.Is(err, webbuilder.ErrSnapshotTooLarge):
		wbInvalid1(c, "/render/html", webbuilder.VErrTooLong, "The rendered page is too large.")
		return nil, false
	case err != nil:
		wbInvalid1(c, "/render/html", webbuilder.VErrEnum, "The rendered page must be exactly one wb-page root element.")
		return nil, false
	}
	out.html = html
	if changed {
		add(webbuilder.StrippedMarkup)
	}
	sectionCSS, utilityCSS := rr.CSS, body.UtilityCSS
	if !unfiltered && strings.TrimSpace(sectionCSS) != "" {
		sectionCSS = ""
		add(webbuilder.StrippedCustomCSS)
	}
	// The editor never sends utilityCss (it is empty since utility classes were dropped); a forged one
	// is a page-wide stylesheet, which is custom CSS and needs unfiltered_html like render.css.
	if !unfiltered && strings.TrimSpace(utilityCSS) != "" {
		utilityCSS = ""
		add(webbuilder.StrippedCustomCSS)
	}
	if len(utilityCSS)+len(sectionCSS) > webbuilder.MaxCSSBytes {
		restErr(c, http.StatusRequestEntityTooLarge, webbuilder.ErrTooLarge, "The page CSS is too large.", map[string]any{"limit": webbuilder.MaxCSSBytes})
		return nil, false
	}
	opts := webbuilder.CSSOptions{SiteOrigin: strings.TrimRight(lb.HomeURL("/"), "/")}
	var parts []string
	for _, x := range []struct{ path, css string }{{"/utilityCss", utilityCSS}, {"/render/css", sectionCSS}} {
		if strings.TrimSpace(x.css) == "" {
			continue
		}
		clean, changed, err := webbuilder.SanitizeCSS(x.css, opts)
		switch {
		case errors.Is(err, webbuilder.ErrCSSTooLarge):
			restErr(c, http.StatusRequestEntityTooLarge, webbuilder.ErrTooLarge, "The page CSS is too large.", map[string]any{"limit": webbuilder.MaxCSSBytes})
			return nil, false
		case err != nil:
			wbInvalid1(c, x.path, webbuilder.VErrCSS, "The page CSS is not valid.")
			return nil, false
		}
		if changed {
			add(webbuilder.StrippedCSS)
		}
		if clean = strings.TrimSpace(clean); clean != "" {
			parts = append(parts, clean)
		}
	}
	out.css = strings.Join(parts, "\n")
	out.features = wbFeatures(rr.Features)
	return out, true
}

// wbFeatures keeps the known runtime features, sorted and unique.
func wbFeatures(in []webbuilder.Feature) []string {
	known := map[webbuilder.Feature]bool{webbuilder.FeatureMenu: true, webbuilder.FeatureAcc: true, webbuilder.FeatureTabs: true,
		webbuilder.FeatureDismiss: true, webbuilder.FeatureForm: true, webbuilder.FeatureMotion: true, webbuilder.FeatureEmbed: true}
	seen := map[string]bool{}
	out := []string{}
	for _, f := range in {
		if known[f] && !seen[string(f)] {
			seen[string(f)] = true
			out = append(out, string(f))
		}
	}
	sort.Strings(out)
	return out
}

// wbRevConflict answers 409 wb_rev_conflict with the stored state.
func (s *Server) wbRevConflict(c *gin.Context, row *store.WebBuilderDoc) {
	data := map[string]any{"rev": 0, "modified": "", "modifiedBy": nil}
	if row != nil {
		data["rev"], data["modified"] = row.Rev, wbTime(row.UpdatedAt)
		if row.SnapshotUser > 0 {
			data["modifiedBy"] = s.wbUserRefs(c).get(row.SnapshotUser)
		}
	}
	restErr(c, http.StatusConflict, webbuilder.ErrRevConflict, "This page was published by someone else after you started editing.", data)
}

// wbRESTPublish is POST documents/{id}/publish (contract §3.1, steps 1–8).
func (s *Server) wbRESTPublish(c *gin.Context, r *restRequest) {
	u, p := s.wbPost(c, r)
	if p == nil {
		return
	}
	if lock := s.wbLockHolder(c, p, u.ID); lock != nil {
		restErr(c, http.StatusConflict, webbuilder.ErrLocked, lock.User.Name+" is editing this page.", map[string]any{"lock": lock})
		return
	}
	raw, ok := wbBody(c, r, webbuilder.MaxDocBytes)
	if !ok {
		return
	}
	var body wbPublishBody
	if !wbDecode(c, raw, &body) {
		return
	}
	status := body.Status
	// Scheduled publish: status future with the date the page goes live. The snapshot is stored now;
	// the core's publish_future_post cron (publishDuePosts) flips the status and purges the cache.
	var when *time.Time
	if status == webbuilder.StatusFuture {
		t, err := time.Parse(time.RFC3339, strings.TrimSpace(body.Date))
		if err != nil || wbIsTemplate(p) {
			restErr(c, http.StatusBadRequest, "rest_invalid_param", "Invalid parameter(s): date")
			return
		}
		when = &t
		if !t.After(time.Now()) {
			status = webbuilder.StatusPublish // the store would do the same; say so in the answer
		}
	}
	switch {
	case wbIsTemplate(p):
		// A template is active (publish) or disabled (draft); wbEditable already required
		// edit_theme_options, so there is no pending path.
		if !wbTemplateStatusOK(status) {
			wbBadTemplate(c, "A template can only be published or saved as a draft.")
			return
		}
	case status == webbuilder.StatusPublish || status == webbuilder.StatusPrivate || status == webbuilder.StatusFuture:
		if !u.Can("publish_post", p) {
			status, when = webbuilder.StatusPending, nil
		}
	case status == webbuilder.StatusDraft || status == webbuilder.StatusPending:
	default:
		restErr(c, http.StatusBadRequest, "rest_invalid_param", "Invalid parameter(s): status")
		return
	}
	wb := s.store.WebBuilder()
	// Cheap early rev check; the authoritative one runs under the publish lock below.
	row, err := wb.Doc(c, p.ID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		restErr(c, http.StatusInternalServerError, "db_error", "Could not read the page.")
		return
	}
	if err != nil {
		row = nil
	}
	if (row == nil && body.BaseRev != 0) || (row != nil && row.Rev != body.BaseRev) {
		s.wbRevConflict(c, row)
		return
	}
	if row != nil && row.Detached && !body.Reattach {
		restErr(c, http.StatusConflict, webbuilder.ErrDetached, "This page was edited outside the builder.")
		return
	}
	unfiltered := wbUnfiltered(c, u)
	res, ok := wbParseDoc(c, body.Doc, u, s.wbStoredDocs(c, p.ID, u, row)...)
	if !ok {
		return
	}
	// trusted: the snapshot is sanitized as an unfiltered_html author's. Without unfiltered_html a
	// doc that kept stored privileged parts (res.Kept: an administrator's html or code sections or
	// custom CSS) is drawn on the server from the parsed doc, never from the request's render: its
	// privileged parts are the stored ones and everything else is normalized, so the snapshot is
	// the one its authors would publish. Without the server renderer the request's render is
	// sanitized as usual (the html sections' frames and the custom CSS are then missing until an
	// unfiltered_html user publishes; code sections are filled below either way).
	trusted := unfiltered
	switch {
	case body.ServerRender:
		if !s.wbServerRenderInto(c, p, res, &body) {
			return
		}
		trusted = unfiltered || res.Kept
	case !unfiltered && res.Kept:
		if rr, err := s.wbServerRender(c, p, res); err == nil {
			body.Render, body.UtilityCSS, trusted = *rr, "", true
		} else {
			log.Printf("web builder: publish %d: server render: %v", p.ID, err)
		}
	}
	snap, ok := s.wbSanitizeRender(c, p, &body, trusted, res.Stripped)
	if !ok {
		return
	}
	// Code sections (privileged): their own markup replaces the placeholders the renderer drew
	// (internal/webbuilder/code.go). The markup is res's: the publisher's own with unfiltered_html,
	// else the stored sections' (wbParseDoc drops every other code section), never the request's.
	if (unfiltered || res.Kept) && webbuilder.HasCode(res.JSON) {
		list, only := webbuilder.CodeSections(res.JSON)
		s.wbLocalBreakpoints(list)
		snap.html = webbuilder.FillCodeWith(snap.html, list, only, webbuilder.CodeFillOptions{
			FormActionPrefix: s.links(c).HomeURL("/wp-json/" + webbuilder.RESTNamespace + "/forms/" + itoa64(p.ID) + "/")})
	}
	ps, _ := wbPage(body.Page)
	needs := webbuilder.DocFonts(&res.Doc)
	fonts, _ := json.Marshal(needs)
	s.wbLocalizeFonts(needs) // before the transaction: the page's fonts must exist locally for visitors
	content := webbuilder.FallbackHTML(&res.Doc)

	tx, err := s.store.Pool.Begin(c)
	if err != nil {
		restErr(c, http.StatusInternalServerError, "db_error", "Could not publish the page.")
		return
	}
	// Every query until Commit runs on tx: the post, its revision, the doc and the draft commit or roll
	// back together, and the handler never waits for a second pooled connection while holding this one.
	// Answers that look up users (lock holder, rev conflict) roll back first for the same reason.
	rollback := func() { _ = tx.Rollback(context.WithoutCancel(c)) }
	defer rollback()
	if _, err := tx.Exec(c, `SELECT pg_advisory_xact_lock($1, $2)`, wbPublishLockClass, int32(p.ID)); err != nil {
		restErr(c, http.StatusInternalServerError, "db_error", "Could not publish the page.")
		return
	}
	wbtx := store.WebBuilderIn(tx)
	row, err = wbtx.DocForUpdate(c, p.ID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		restErr(c, http.StatusInternalServerError, "db_error", "Could not read the page.")
		return
	}
	if err != nil {
		row = nil
	}
	if (row == nil && body.BaseRev != 0) || (row != nil && row.Rev != body.BaseRev) {
		rollback()
		s.wbRevConflict(c, row)
		return
	}
	if row != nil && row.Detached && !body.Reattach {
		restErr(c, http.StatusConflict, webbuilder.ErrDetached, "This page was edited outside the builder.")
		return
	}
	// The post as it is now (another save may have changed its meta since the request started).
	fresh, err := store.GetPostTx(c, tx, p.ID)
	if err != nil {
		restErr(c, http.StatusInternalServerError, "db_error", "Could not read the post.")
		return
	}
	if wbLockedByOther(fresh, u.ID) {
		rollback()
		lock := s.wbLockHolder(c, fresh, u.ID)
		restErr(c, http.StatusConflict, webbuilder.ErrLocked, lock.User.Name+" is editing this page.", map[string]any{"lock": lock})
		return
	}
	pageMissing := wbPageMissing(body.Page)
	if pageMissing {
		// No page settings in the request (a REST client other than the editor): the post keeps
		// its title, slug, excerpt, SEO description and noindex instead of losing them.
		ps = webbuilder.PageSettings{Title: fresh.Title, Description: fresh.Excerpt, Indexable: seogoMeta(fresh.Meta, "noindex") != "noindex"}
	}
	prevRevision, err := wbtx.LatestPostRevisionID(c, p.ID)
	if err != nil {
		restErr(c, http.StatusInternalServerError, "db_error", "Could not publish the page.")
		return
	}
	// Step 6: PageSettings → the post; step 7: posts.content is the fallback generated from the doc.
	fresh.Title = ps.Title
	if ps.Slug != "" {
		fresh.Slug = ps.Slug
	}
	// The description and noindex are shared with Pursae SEO (its metabox and the editor's Page
	// tools › SEO tab write them straight to the post meta): publish writes them only when the
	// builder's own page settings changed them since the last publish, so a stale value the editor
	// loaded never overwrites what Pursae SEO saved in the meantime.
	descChanged, indexChanged := wbPageChanged(row, body.Page, body.BaseRev, ps)
	if pageMissing || descChanged {
		fresh.Excerpt = ps.Description
	}
	fresh.Content = content
	switch {
	case when != nil:
		fresh.PublishedAt = when
	case status == webbuilder.StatusPublish && fresh.Status == webbuilder.StatusFuture:
		fresh.PublishedAt = nil // "publish now" of a scheduled page: the store dates it now
	}
	fresh.Status = status
	if fresh.Meta == nil {
		fresh.Meta = map[string]any{}
	}
	fresh.Meta[webbuilder.PostMarkerMeta] = "1"
	fresh.Meta[webbuilder.EditLockMeta] = strconv.FormatInt(time.Now().Unix(), 10) + ":" + itoa64(u.ID)
	// The Pursae SEO fields keep their own capabilities (seogoEditFields): the builder's page
	// settings write the meta description only with seogo_onpage_general and noindex only with
	// seogo_onpage_advanced, as the SEO box and seogo/v1/meta do (an author must not noindex a
	// page through the builder when the Role Manager does not let them).
	if !pageMissing && descChanged && s.seogoCan(u, seogoEditFields["description"]) {
		fresh.Meta[seogoKey("description")] = ps.Description
	}
	if !pageMissing && indexChanged && s.seogoCan(u, seogoEditFields["noindex"]) {
		if !ps.Indexable {
			fresh.Meta[seogoKey("noindex")] = "noindex"
		} else if seogoMeta(fresh.Meta, "noindex") == "noindex" {
			fresh.Meta[seogoKey("noindex")] = "" // back to the site default
		}
	} else if !pageMissing && indexChanged {
		ps.Indexable = seogoMeta(fresh.Meta, "noindex") != "noindex" // the stored page keeps what the post has
	}
	if err := store.UpdatePostTx(c, tx, fresh, u.ID); err != nil {
		restErr(c, http.StatusInternalServerError, "db_update_error", "Could not update post in the database.")
		return
	}
	revisionID, err := wbtx.LatestPostRevisionID(c, p.ID)
	if err == nil && revisionID == prevRevision {
		// Same title, excerpt and content (a style-only change): the core update wrote no revision,
		// but every builder publish gets one.
		err = tx.QueryRow(c, `INSERT INTO post_revisions(post_id, author_id, title, excerpt, content) VALUES ($1, $2, $3, $4, $5) RETURNING id`,
			p.ID, u.ID, fresh.Title, fresh.Excerpt, fresh.Content).Scan(&revisionID)
	}
	if err != nil {
		restErr(c, http.StatusInternalServerError, "db_error", "Could not record the revision.")
		return
	}
	pageJSON := wbPageMerge(wbPageObject(body.Page), webbuilder.PageSettings{Title: fresh.Title, Slug: fresh.Slug, Description: ps.Description, Indexable: ps.Indexable})
	d := &store.WebBuilderDoc{PostID: p.ID, Doc: res.JSON, Page: pageJSON, HTML: snap.html, CSS: snap.css, Fonts: fonts,
		Features: snap.features, Renderer: webbuilder.RendererVersion, ContentHash: store.WebBuilderContentHash(fresh.Content),
		SnapshotUser: u.ID, SnapshotUnfiltered: unfiltered || res.Kept}
	if err := wbtx.PublishDoc(c, d, body.BaseRev); err != nil {
		var rc *store.RevConflictError
		if errors.As(err, &rc) {
			rollback()
			s.wbRevConflict(c, &store.WebBuilderDoc{Rev: rc.Rev, UpdatedAt: rc.UpdatedAt, SnapshotUser: rc.SnapshotUser})
			return
		}
		restErr(c, http.StatusInternalServerError, "db_error", "Could not publish the page.")
		return
	}
	if err := wbtx.AddRevision(c, &store.WebBuilderRevision{RevisionID: revisionID, PostID: p.ID, Rev: d.Rev, Doc: res.JSON,
		Page: pageJSON, Renderer: webbuilder.RendererVersion}); err != nil {
		restErr(c, http.StatusInternalServerError, "db_error", "Could not record the revision.")
		return
	}
	// The core cap (PURSAE_POST_REVISIONS) also covers the revision a style-only publish inserts above.
	if err := store.PruneRevisionsTx(c, tx, p.ID); err != nil {
		restErr(c, http.StatusInternalServerError, "db_error", "Could not prune the old revisions.")
		return
	}
	if _, err := wbtx.PruneRevisions(c, p.ID, s.wbRevisionsKeep()); err != nil {
		restErr(c, http.StatusInternalServerError, "db_error", "Could not prune the old revisions.")
		return
	}
	if err := wbtx.DeleteDraft(c, p.ID, u.ID); err != nil {
		restErr(c, http.StatusInternalServerError, "db_error", "Could not publish the page.")
		return
	}
	if err := tx.Commit(c); err != nil {
		restErr(c, http.StatusInternalServerError, "db_error", "Could not publish the page.")
		return
	}
	if after, err := s.store.GetPost(c, p.ID); err == nil {
		fresh = after
	}
	// save_post side effects, Pursae Cache purge of the post included; for a template
	// wbAfterPostSave calls wbTemplatesChanged (index generation + full purge).
	s.afterPostSave(c, fresh)
	permalink := s.links(c).Permalink(fresh)
	if wbIsTemplate(fresh) {
		// A template has no front URL of its own (/?p=<id> is a 404): like links.permalink in the
		// editor (contract §11.7), the permalink is the template's preview URL.
		if t, err := s.store.WebBuilder().Template(c, fresh.ID); err == nil {
			info, _ := s.wbTemplatePreviewInfo(c, t)
			permalink = info.URL
		}
	}
	c.JSON(http.StatusOK, webbuilder.PublishResponse{Rev: d.Rev, Status: fresh.Status, Date: wbPostDate(fresh), Permalink: permalink,
		RevisionID: revisionID, Modified: wbTime(fresh.ModifiedAt), Stripped: snap.stripped})
}

// wbPageObject is the client's page object ({} when it is not one).
func wbPageObject(raw json.RawMessage) map[string]any {
	obj := map[string]any{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &obj)
	}
	if obj == nil {
		obj = map[string]any{}
	}
	return obj
}

// --- revisions -------------------------------------------------------------------------------------

// wbRESTRevisions is GET documents/{id}/revisions?page=N (newest first, 50 per page).
func (s *Server) wbRESTRevisions(c *gin.Context, r *restRequest) {
	_, p := s.wbPost(c, r)
	if p == nil {
		return
	}
	page := 1
	if n, err := strconv.Atoi(c.Query("page")); err == nil && n > 0 {
		page = n
	}
	wb := s.store.WebBuilder()
	total, err := wb.CountRevisions(c, p.ID)
	if err != nil {
		restErr(c, http.StatusInternalServerError, "db_error", "Could not read the revisions.")
		return
	}
	list, err := wb.Revisions(c, p.ID, wbRevisionsPerPage, (page-1)*wbRevisionsPerPage)
	if err != nil {
		restErr(c, http.StatusInternalServerError, "db_error", "Could not read the revisions.")
		return
	}
	refs := s.wbUserRefs(c)
	out := make([]webbuilder.RevisionSummary, 0, len(list))
	for _, x := range list {
		out = append(out, webbuilder.RevisionSummary{ID: x.RevisionID, Rev: x.Rev, Author: refs.get(x.AuthorID),
			Date: wbTime(x.Date), Title: x.Title, Renderer: x.Renderer})
	}
	pages := (total + wbRevisionsPerPage - 1) / wbRevisionsPerPage
	if pages < 1 {
		pages = 1
	}
	c.Header("X-WP-Total", strconv.Itoa(total))
	c.Header("X-WP-TotalPages", strconv.Itoa(pages))
	c.JSON(http.StatusOK, out)
}

// wbRevision loads the {rid} builder revision of the {id} post.
func (s *Server) wbRevision(c *gin.Context, r *restRequest) (*store.User, *store.Post, *store.WebBuilderRevision) {
	u, p := s.wbPost(c, r)
	if p == nil {
		return nil, nil, nil
	}
	rid, _ := strconv.ParseInt(r.Params["rid"], 10, 64)
	rev, err := s.store.WebBuilder().Revision(c, p.ID, rid)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			restErr(c, http.StatusNotFound, webbuilder.ErrNotFound, "Invalid revision ID.")
		} else {
			restErr(c, http.StatusInternalServerError, "db_error", "Could not read the revision.")
		}
		return nil, nil, nil
	}
	return u, p, rev
}

type wbRevisionJSON struct {
	ID   int64           `json:"id"`
	Rev  int             `json:"rev"`
	Doc  json.RawMessage `json:"doc"`
	Page json.RawMessage `json:"page"`
}

// wbRESTRevision is GET documents/{id}/revisions/{rid}.
func (s *Server) wbRESTRevision(c *gin.Context, r *restRequest) {
	_, _, rev := s.wbRevision(c, r)
	if rev == nil {
		return
	}
	c.JSON(http.StatusOK, wbRevisionJSON{ID: rev.RevisionID, Rev: rev.Rev, Doc: webbuilder.TagCodeDoc(rev.Doc), Page: wbPageJSON(rev.Page, json.RawMessage("{}"))})
}

// wbRESTRestore is POST documents/{id}/revisions/{rid}/restore: the revision becomes the caller's
// draft (re-normalized for the caller's capabilities); the editor loads it and publishes.
func (s *Server) wbRESTRestore(c *gin.Context, r *restRequest) {
	u, p, rev := s.wbRevision(c, r)
	if rev == nil {
		return
	}
	raw, ok := wbBody(c, r, 4<<10)
	if !ok {
		return
	}
	var body webbuilder.RestoreRequest
	if !wbDecode(c, raw, &body) {
		return
	}
	// The revision was a publish: its privileged parts are an unfiltered_html author's (or kept from
	// one), so they stay in the restored draft.
	res, ok := wbParseDoc(c, rev.Doc, u, append([]json.RawMessage{rev.Doc}, s.wbStoredDocs(c, p.ID, u, nil)...)...)
	if !ok {
		return
	}
	_, pageJSON := wbPage(rev.Page)
	d := &store.WebBuilderDraft{PostID: p.ID, UserID: u.ID, Doc: res.JSON, Page: pageJSON, BaseRev: body.BaseRev}
	if err := s.store.WebBuilder().SaveDraft(c, d); err != nil {
		restErr(c, http.StatusInternalServerError, "db_error", "Could not save the draft.")
		return
	}
	c.JSON(http.StatusOK, wbDraft{Doc: webbuilder.TagCodeDoc(d.Doc), Page: d.Page, BaseRev: d.BaseRev, SavedAt: wbTime(d.UpdatedAt)})
}

// --- library ---------------------------------------------------------------------------------------

type wbLibraryJSON struct {
	ID      string             `json:"id"`
	Name    string             `json:"name"`
	Section json.RawMessage    `json:"section"`
	Author  webbuilder.UserRef `json:"author"`
	Created string             `json:"created"`
}

func wbLibraryOut(it *store.WebBuilderLibraryItem, refs *wbUserRefs) wbLibraryJSON {
	return wbLibraryJSON{ID: itoa64(it.ID), Name: it.Name, Section: it.Data, Author: refs.get(it.AuthorID), Created: wbTime(it.CreatedAt)}
}

// wbRESTLibrary is GET library: saved components, newest first. A user without edit_others_posts
// sees only the components they saved.
func (s *Server) wbRESTLibrary(c *gin.Context, _ *restRequest) {
	u := wbUser(c, "edit_posts")
	if u == nil {
		return
	}
	items, err := s.store.WebBuilder().LibraryItems(c)
	if err != nil {
		restErr(c, http.StatusInternalServerError, "db_error", "Could not read the library.")
		return
	}
	if !u.Can("edit_others_posts", nil) {
		own := make([]*store.WebBuilderLibraryItem, 0, len(items))
		for _, it := range items {
			if it.AuthorID == u.ID {
				own = append(own, it)
			}
		}
		items = own
	}
	refs := s.wbUserRefs(c)
	out := make([]wbLibraryJSON, 0, len(items))
	privileged := map[int64]bool{}
	for _, it := range items {
		// A component saved by a user without unfiltered_html before form recipients were privileged
		// may still name its own: they are not handed to whoever inserts it.
		ok, seen := privileged[it.AuthorID]
		if !seen {
			au, err := s.store.UserByID(c, it.AuthorID)
			ok = err == nil && au != nil && au.Can("unfiltered_html", nil)
			privileged[it.AuthorID] = ok
		}
		if !ok {
			cp := *it
			cp.Data = webbuilder.StripFormRecipients(it.Data)
			it = &cp
		}
		out = append(out, wbLibraryOut(it, refs))
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, out)
}

type wbLibraryBody struct {
	Name    string          `json:"name"`
	Section json.RawMessage `json:"section"`
}

// wbRESTLibraryCreate is POST library: a saved component, validated like a doc section.
func (s *Server) wbRESTLibraryCreate(c *gin.Context, r *restRequest) {
	u := wbUser(c, "edit_posts")
	if u == nil {
		return
	}
	raw, ok := wbBody(c, r, webbuilder.MaxDocBytes)
	if !ok {
		return
	}
	var body wbLibraryBody
	if !wbDecode(c, raw, &body) {
		return
	}
	name := strings.TrimSpace(body.Name)
	switch {
	case name == "":
		wbInvalid1(c, "/name", webbuilder.VErrRequired, "The component needs a name.")
		return
	case utf8.RuneCountInString(name) > wbNameMax:
		wbInvalid1(c, "/name", webbuilder.VErrTooLong, "The component name is too long.")
		return
	case len(body.Section) == 0 || string(body.Section) == "null":
		wbInvalid1(c, "/section", webbuilder.VErrRequired, "The component needs a section.")
		return
	}
	res, err := webbuilder.ParseSection(body.Section, webbuilder.DocOptions{UnfilteredHTML: wbUnfiltered(c, u), PathPrefix: "/section"})
	switch {
	case errors.Is(err, webbuilder.ErrDocTooLarge):
		restErr(c, http.StatusRequestEntityTooLarge, webbuilder.ErrTooLarge, "The component is too large.", map[string]any{"limit": webbuilder.MaxDocBytes})
		return
	case err != nil:
		wbInvalid1(c, "/section", webbuilder.VErrRequired, "The component is not a valid section.")
		return
	case !res.Valid():
		wbInvalid(c, res.Errors)
		return
	case res.Section == nil:
		restErr(c, http.StatusForbidden, webbuilder.ErrForbidden, "Sorry, you are not allowed to save HTML sections.")
		return
	}
	it := &store.WebBuilderLibraryItem{Name: name, Data: res.JSON, AuthorID: u.ID}
	if err := s.store.WebBuilder().AddLibraryItem(c, it, webbuilder.MaxLibraryItems); err != nil {
		if errors.Is(err, store.ErrLibraryFull) {
			restErr(c, http.StatusConflict, "wb_library_full", "The library is full. Delete a saved component first.", map[string]any{"limit": webbuilder.MaxLibraryItems})
			return
		}
		restErr(c, http.StatusInternalServerError, "db_error", "Could not save the component.")
		return
	}
	c.JSON(http.StatusCreated, wbLibraryOut(it, s.wbUserRefs(c)))
}

// wbRESTLibraryDelete is DELETE library/{id}: the author, or a user with edit_others_posts.
func (s *Server) wbRESTLibraryDelete(c *gin.Context, r *restRequest) {
	u := wbUser(c, "edit_posts")
	if u == nil {
		return
	}
	wb := s.store.WebBuilder()
	it, err := wb.LibraryItem(c, r.ID())
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			restErr(c, http.StatusNotFound, webbuilder.ErrNotFound, "Invalid component ID.")
		} else {
			restErr(c, http.StatusInternalServerError, "db_error", "Could not read the library.")
		}
		return
	}
	if it.AuthorID != u.ID && !u.Can("edit_others_posts", nil) {
		wbForbidden(c, u)
		return
	}
	if err := wb.DeleteLibraryItem(c, it.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
		restErr(c, http.StatusInternalServerError, "db_error", "Could not delete the component.")
		return
	}
	c.JSON(http.StatusOK, map[string]any{"deleted": true})
}

// --- prefs -----------------------------------------------------------------------------------------

var wbPrefKeyRe = regexp.MustCompile(webbuilder.PrefKeyPattern)

// wbRESTPrefs is GET prefs: the user's editor preferences.
func (s *Server) wbRESTPrefs(c *gin.Context, _ *restRequest) {
	u := wbUser(c, "edit_posts")
	if u == nil {
		return
	}
	var raw []byte
	err := s.store.Pool.QueryRow(c, `SELECT coalesce(meta->$2, '{}'::jsonb) FROM users WHERE id = $1`, u.ID, webbuilder.UserPrefsMeta).Scan(&raw)
	if err != nil {
		restErr(c, http.StatusInternalServerError, "db_error", "Could not read the preferences.")
		return
	}
	c.Header("Cache-Control", "no-store")
	c.Data(http.StatusOK, "application/json; charset=utf-8", wbPrefsObject(raw))
}

// wbPrefsObject is raw when it is a JSON object, else {}.
func wbPrefsObject(raw []byte) []byte {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil || m == nil {
		return []byte("{}")
	}
	return raw
}

// wbRESTPrefsSave is PUT prefs: the body is merged into the stored preferences; null deletes a key.
func (s *Server) wbRESTPrefsSave(c *gin.Context, r *restRequest) {
	u := wbUser(c, "edit_posts")
	if u == nil {
		return
	}
	raw, ok := wbBody(c, r, 4*webbuilder.MaxPrefsBytes)
	if !ok {
		return
	}
	var patch map[string]json.RawMessage
	if err := json.Unmarshal(raw, &patch); err != nil || patch == nil {
		restErr(c, http.StatusBadRequest, "rest_invalid_json", "The preferences must be a JSON object.")
		return
	}
	for k := range patch {
		if !wbPrefKeyRe.MatchString(k) {
			restErr(c, http.StatusBadRequest, "rest_invalid_param", "Invalid preference key: "+wbClip(k, 64), map[string]any{"key": wbClip(k, 64)})
			return
		}
	}
	var out []byte
	tooLarge := errors.New("too large")
	err := pgx.BeginFunc(c, s.store.Pool, func(tx pgx.Tx) error {
		var cur []byte
		if err := tx.QueryRow(c, `SELECT coalesce(meta->$2, '{}'::jsonb) FROM users WHERE id = $1 FOR UPDATE`, u.ID, webbuilder.UserPrefsMeta).Scan(&cur); err != nil {
			return err
		}
		m := map[string]json.RawMessage{}
		_ = json.Unmarshal(wbPrefsObject(cur), &m)
		for k, v := range patch {
			if string(bytes.TrimSpace(v)) == "null" {
				delete(m, k)
			} else {
				m[k] = v
			}
		}
		b, err := json.Marshal(m)
		if err != nil {
			return err
		}
		if len(b) > webbuilder.MaxPrefsBytes {
			return tooLarge
		}
		out = b
		_, err = tx.Exec(c, `UPDATE users SET meta = coalesce(meta, '{}'::jsonb) || jsonb_build_object($2::text, $3::jsonb) WHERE id = $1`,
			u.ID, webbuilder.UserPrefsMeta, string(b))
		return err
	})
	if errors.Is(err, tooLarge) {
		restErr(c, http.StatusRequestEntityTooLarge, webbuilder.ErrTooLarge, "The preferences are too large.", map[string]any{"limit": webbuilder.MaxPrefsBytes})
		return
	}
	if err != nil {
		restErr(c, http.StatusInternalServerError, "db_error", "Could not save the preferences.")
		return
	}
	c.Data(http.StatusOK, "application/json; charset=utf-8", out)
}

// --- fonts -----------------------------------------------------------------------------------------

// wbFontRequest is one family= of a Google Fonts css2 query, resolved against the builder's font
// library: weights snapped to the ones the family ships (contract §3.3 weight allowlist).
type wbFontRequest struct {
	family string
	normal map[int]bool
	italic map[int]bool
}

// wbParseCSS2Family parses "Inter:wght@400;700" / "Inter:ital,wght@0,400;1,700" / "Inter". ok is
// false for a family outside the builder's font library.
func wbParseCSS2Family(v string) (wbFontRequest, bool) {
	name, axes, _ := strings.Cut(v, ":")
	name = strings.TrimSpace(strings.ReplaceAll(name, "+", " "))
	f, ok := webbuilder.LookupFont(name)
	if !ok {
		return wbFontRequest{}, false
	}
	req := wbFontRequest{family: f.Family, normal: map[int]bool{}, italic: map[int]bool{}}
	snap := func(list []int, w int) (int, bool) {
		if len(list) == 0 {
			return 0, false
		}
		best := list[0]
		for _, x := range list {
			if wbAbs(x-w) < wbAbs(best-w) {
				best = x
			}
		}
		return best, true
	}
	addW := func(italic bool, w int) {
		if w < 100 || w > 900 {
			return
		}
		if italic {
			if x, ok := snap(f.Italics, w); ok {
				req.italic[x] = true
			}
			return
		}
		if x, ok := snap(f.Weights, w); ok {
			req.normal[x] = true
		}
	}
	tags, tuples, _ := strings.Cut(axes, "@")
	names := strings.Split(tags, ",")
	italIdx, wghtIdx := -1, -1
	for i, n := range names {
		switch strings.TrimSpace(n) {
		case "ital":
			italIdx = i
		case "wght":
			wghtIdx = i
		}
	}
	if axes != "" && tuples != "" {
		for _, t := range strings.Split(tuples, ";") {
			vals := strings.Split(t, ",")
			if len(vals) != len(names) {
				continue
			}
			italic := italIdx >= 0 && strings.TrimSpace(vals[italIdx]) == "1"
			w := 400
			if wghtIdx >= 0 {
				// "100..900" ranges (variable fonts) ask for every weight in the range.
				lo, hi, isRange := strings.Cut(strings.TrimSpace(vals[wghtIdx]), "..")
				a, err1 := strconv.Atoi(lo)
				if err1 != nil {
					continue
				}
				if isRange {
					b, err2 := strconv.Atoi(hi)
					if err2 != nil {
						continue
					}
					// Only 100..900 can name a weight: clamp first so a huge bound cannot spin the loop.
					a, b = max(a, 100), min(b, 900)
					for x := (a + 99) / 100 * 100; x <= b; x += 100 {
						addW(italic, x)
					}
					continue
				}
				w = a
			}
			addW(italic, w)
		}
	}
	if len(req.normal) == 0 && len(req.italic) == 0 {
		addW(false, 400)
	}
	return req, true
}

func wbAbs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

var (
	wbFaceBlockRe = regexp.MustCompile(`(?s)(/\*[^*]*\*/\s*)?@font-face\s*\{[^}]*\}`)
	wbFaceURLRe   = regexp.MustCompile(`url\(\s*['"]?\.\./fonts/([A-Za-z0-9._-]+)['"]?\s*\)`)
	wbFaceWRe     = regexp.MustCompile(`font-weight\s*:\s*([0-9]+|normal|bold)(?:\s+([0-9]+))?`)
)

// wbFontFaces filters a localized family stylesheet (gfonts.go, every weight) down to the
// requested faces and points their src at fontsURL (absolute, ends with "/").
func wbFontFaces(css string, req wbFontRequest, fontsURL string) string {
	var sb strings.Builder
	for _, m := range wbFaceBlockRe.FindAllString(css, -1) {
		italic := gfontFaceStyleRe.MatchString(m) && gfontFaceStyleRe.FindStringSubmatch(m)[1] == "italic"
		want := req.normal
		if italic {
			want = req.italic
		}
		lo, hi := 400, 400
		if w := wbFaceWRe.FindStringSubmatch(m); w != nil {
			switch w[1] {
			case "normal":
			case "bold":
				lo, hi = 700, 700
			default:
				lo, _ = strconv.Atoi(w[1])
				hi = lo
				if w[2] != "" {
					hi, _ = strconv.Atoi(w[2])
				}
			}
		}
		keep := false
		for w := range want {
			if w >= lo && w <= hi {
				keep = true
				break
			}
		}
		if !keep {
			continue
		}
		sb.WriteString(wbFaceURLRe.ReplaceAllString(m, "url("+fontsURL+"$1)"))
		sb.WriteString("\n")
	}
	return sb.String()
}

// wbFontsDisk measures the localized fonts once per wbFontsDiskTTL.
var wbFontsDisk struct {
	sync.Mutex
	dir  string
	at   time.Time
	size int64
}

func (s *Server) wbFontsDiskUsage() int64 {
	dir := filepath.Join(s.cfg.UploadsDir, filepath.FromSlash(gfontsBase))
	wbFontsDisk.Lock()
	defer wbFontsDisk.Unlock()
	if wbFontsDisk.dir == dir && time.Since(wbFontsDisk.at) < wbFontsDiskTTL {
		return wbFontsDisk.size
	}
	var n int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if info, err := d.Info(); err == nil {
				n += info.Size()
			}
		}
		return nil
	})
	wbFontsDisk.dir, wbFontsDisk.at, wbFontsDisk.size = dir, time.Now(), n
	return n
}

// wbLocalizeFonts downloads the library fonts a page uses (once, by the localizer in gfonts.go) when
// an editor publishes it. The editor ships some families itself (@fontsource), so it never asks
// fonts/css2 for them, yet visitors only get fonts that are already local: a visitor's request never
// starts a download. A slow or failed download does not fail the publish; the page is linked to
// the font as soon as the background download finishes (the next cache purge or view).
func (s *Server) wbLocalizeFonts(needs []webbuilder.FontNeed) {
	var wg sync.WaitGroup
	seen := map[string]bool{}
	for _, n := range needs {
		f, ok := webbuilder.LookupFont(n.Family)
		if !ok || seen[f.Family] {
			continue
		}
		seen[f.Family] = true
		if _, err := os.Stat(filepath.Join(s.gfontsDir("css"), gfontKey(f.Family)+".css")); err == nil {
			continue
		}
		if s.wbFontsDiskUsage() >= wbFontsDiskCap {
			return
		}
		wg.Add(1)
		family := f.Family
		safeGo("webbuilder_rest.wbLocalizeFonts", func() {
			defer wg.Done()
			s.gfontEnsure(family, wbFontsWait)
		})
	}
	wg.Wait()
}

// wbRESTFontsCSS is GET fonts/css2?family=…: the Google Fonts css2 API served from this site. The
// families are downloaded (once, all weights, by the localizer in gfonts.go) only here, for editor
// users; the answer lists the requested faces with src under the local uploads directory.
func (s *Server) wbRESTFontsCSS(c *gin.Context, _ *restRequest) {
	if wbUser(c, "edit_posts") == nil {
		return
	}
	families := c.QueryArray("family")
	if len(families) == 0 {
		restErr(c, http.StatusBadRequest, "rest_invalid_param", "Missing parameter(s): family")
		return
	}
	if len(families) > wbFontsPerRequest {
		restErr(c, http.StatusBadRequest, "rest_invalid_param", "Too many families in one request.", map[string]any{"limit": wbFontsPerRequest})
		return
	}
	fontsURL := s.links(c).HomeURL("/wp-content/uploads/" + gfontsBase + "/fonts/")
	var sb strings.Builder
	sb.WriteString("/* Google Fonts served locally by Pursae Web Builder (SIL Open Font License / Apache License, see fonts.google.com) */\n")
	seen := map[string]bool{}
	for _, v := range families {
		req, ok := wbParseCSS2Family(v)
		if !ok {
			sb.WriteString("/* skipped: not a library font */\n")
			continue
		}
		if seen[req.family] {
			continue
		}
		seen[req.family] = true
		cssPath := filepath.Join(s.gfontsDir("css"), gfontKey(req.family)+".css")
		if _, err := os.Stat(cssPath); err != nil {
			if s.wbFontsDiskUsage() >= wbFontsDiskCap {
				sb.WriteString("/* " + req.family + ": not downloaded, the font storage is full */\n")
				continue
			}
			if !s.gfontEnsure(req.family, wbFontsWait) {
				sb.WriteString("/* " + req.family + ": not available yet */\n")
				continue
			}
		}
		b, err := os.ReadFile(cssPath)
		if err != nil {
			continue
		}
		sb.WriteString(wbFontFaces(string(b), req, fontsURL))
	}
	c.Header("Cache-Control", wbFontsCacheMaxAge)
	c.Header("X-Content-Type-Options", "nosniff")
	c.Data(http.StatusOK, "text/css; charset=utf-8", []byte(sb.String()))
}
