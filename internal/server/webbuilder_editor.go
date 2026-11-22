package server

import (
	"context"
	"encoding/json"
	"errors"
	"html"
	"log"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"wpgo/internal/media"
	"wpgo/internal/store"
	"wpgo/internal/webbuilder"
)

// The full-screen editor of Pursae Web Builder: /wp-admin/post?post={id}&action=web-builder
// (postEdit dispatches here). The page is a bare HTML document — no admin chrome, no admin-page
// translation — that loads the editor bundle named by the manifest and hands it the boot config
// window.__WEB_BUILDER__ (contract §2). The admin CSP already fits: the bundle, its fonts and the
// REST calls are same-origin, the boot script is inline (script-src 'unsafe-inline'), media are
// same-origin or https images.

// wbEditor writes the editor page of p.
func (s *Server) wbEditor(c *gin.Context, p *store.Post) {
	c.Header("Cache-Control", "no-cache, no-store, must-revalidate, max-age=0")
	u := currentUser(c)
	if u == nil || (wbIsTemplate(p) && !u.Can(webbuilder.TemplateCap, nil)) || (!wbIsTemplate(p) && !u.Can("edit_post", p)) {
		c.String(http.StatusForbidden, "Sorry, you are not allowed to edit this item.")
		return
	}
	if p.Status == "trash" {
		c.String(http.StatusForbidden, "You cannot edit this item because it is in the Trash. Please restore it and try again.")
		return
	}
	if !s.wbEditable(u, p) {
		c.String(http.StatusForbidden, "This item cannot be edited with Pursae Web Builder.")
		return
	}
	m, err := s.wbManifest()
	if err != nil {
		c.String(http.StatusInternalServerError, "Pursae Web Builder is not built: "+err.Error())
		return
	}
	prefix, entry, err := wbEditorBundle(m)
	if err != nil {
		c.String(http.StatusInternalServerError, "Pursae Web Builder is not built: "+err.Error())
		return
	}
	// The core post lock (_edit_lock), as the block editor's post-lock dialog knows it: taken when
	// free, expired or already ours; another user's live lock is reported to the editor instead.
	if _, err := s.wbTakeLock(c, p.ID, u.ID, false); err != nil {
		s.wbEditorFailed(c, p, err)
		return
	}
	if fresh, err := s.store.GetPost(c, p.ID); err == nil {
		p = fresh
	}
	doc, err := s.wbDocumentFor(c, p, u)
	if err != nil {
		s.wbEditorFailed(c, p, err)
		return
	}
	boot := s.wbBootFor(c, p, u, doc)
	title := "Pursae Web Builder"
	if p.Title != "" {
		title = "Edit “" + p.Title + "” with Pursae Web Builder"
	}
	// The editor UI is left-to-right in every language (its catalogs translate text only).
	lang := strings.ReplaceAll(s.userLocale(c), "_", "-")
	page := wbEditorHTML(title, lang, "ltr", s.links(c).HomeURL(prefix), entry, wbScriptJSON(boot))
	// Code sections draw the design in a frame of the editor's page (it inherits this policy): the
	// origins the design kits name, for authors who may publish them.
	if u.Can("unfiltered_html", nil) {
		for _, k := range s.wbKitList() {
			for _, dir := range []string{"script-src", "connect-src", "style-src", "font-src"} {
				cspAllow(c, dir, k.Origins...)
			}
		}
		// And the origins the code sections' head markup names (a web font stylesheet), the
		// header's and footer's included: the frame draws them with the page.
		heads := []json.RawMessage{doc.Doc}
		if doc.Draft != nil {
			heads = append(heads, doc.Draft.Doc)
		}
		if boot.CodeParts != nil {
			for _, p := range []*webbuilder.CodeTemplate{boot.CodeParts.Header, boot.CodeParts.Footer} {
				if p != nil {
					heads = append(heads, p.Sections...)
				}
			}
		}
		if o := wbCodeHeadOrigins(heads...); len(o) > 0 {
			for _, dir := range []string{"script-src", "style-src", "font-src"} {
				cspAllow(c, dir, o...)
			}
		}
	}
	c.Data(http.StatusOK, "text/html; charset=UTF-8", []byte(page))
}

// wbEditorFailed answers a storage error of the editor page: logged, not shown.
func (s *Server) wbEditorFailed(c *gin.Context, p *store.Post, err error) {
	log.Printf("web builder: editor of post %d: %v", p.ID, err)
	c.String(http.StatusInternalServerError, "Pursae Web Builder could not load this item. Please try again.")
}

// wbEditorHTML is the editor document. base is the absolute bundle URL (ending in "/"), boot the
// encoded window.__WEB_BUILDER__ value (wbScriptJSON).
func wbEditorHTML(title, lang, dir, base string, entry webbuilder.ManifestEntry, boot string) string {
	var b strings.Builder
	b.WriteString("<!DOCTYPE html>\n<html lang=\"" + html.EscapeString(lang) + "\" dir=\"" + dir + "\">\n<head>\n")
	b.WriteString("<meta charset=\"utf-8\">\n<meta name=\"viewport\" content=\"width=device-width, initial-scale=1\">\n")
	b.WriteString("<meta name=\"robots\" content=\"noindex, nofollow\">\n")
	b.WriteString("<title>" + html.EscapeString(title) + "</title>\n")
	for _, css := range entry.CSS {
		b.WriteString("<link rel=\"stylesheet\" href=\"" + html.EscapeString(base+css) + "\">\n")
	}
	b.WriteString("</head>\n<body>\n<div id=\"root\"></div>\n")
	b.WriteString("<noscript>Pursae Web Builder needs JavaScript.</noscript>\n")
	b.WriteString("<script" + cspNonceAttr() + ">window." + webbuilder.BootGlobal + " = " + boot + ";</script>\n")
	b.WriteString("<script" + cspNonceAttr() + " type=\"module\" src=\"" + html.EscapeString(base+entry.JS) + "\"></script>\n")
	b.WriteString("</body>\n</html>\n")
	return b.String()
}

// wbScriptJSON encodes a value for an inline script: scriptJSON ("</" → "<\/") plus "<!--" escaped,
// which would otherwise put the HTML parser in the script's double-escaped state.
func wbScriptJSON(v any) string {
	return strings.ReplaceAll(scriptJSON(v), "<!--", `\u003c!--`)
}

// --- boot config ---------------------------------------------------------------------------------

// wbBoot is webbuilder.Boot with the document carried as raw JSON (wbDocument): the stored doc is
// the client's JSON and goes back unchanged, fields a newer editor added included. The JSON shape
// is the contract's (TestWebBuilderBootShape compares the keys).
type wbBoot struct {
	APIVersion      int                  `json:"apiVersion"`
	RendererVersion int                  `json:"rendererVersion"`
	Document        *wbDocument          `json:"document"`
	User            webbuilder.UserRef   `json:"user"`
	REST            webbuilder.BootREST  `json:"rest"`
	Links           webbuilder.BootLinks `json:"links"`
	Caps            webbuilder.BootCaps  `json:"caps"`
	Locale          string               `json:"locale"`
	Fonts           webbuilder.BootFonts `json:"fonts"`
	Media           webbuilder.BootMedia `json:"media"`
	SiteAssets      string               `json:"siteAssets"`
	AI              webbuilder.BootAI    `json:"ai"`
	// Theme Builder (contract APIVersion 2): Template is null unless the post is a template; Site
	// and Dynamic are always filled (webbuilder_dynamic_ctx.go).
	Template *webbuilder.TemplateBoot `json:"template"`
	Site     webbuilder.SiteBoot      `json:"site"`
	Dynamic  webbuilder.DynContext    `json:"dynamic"`
	// CodeParts: the header and footer templates of code sections around the page
	// (webbuilder_editor_parts.go).
	CodeParts *webbuilder.CodeParts `json:"codeParts"`
	// KitBreakpoints: local-style breakpoints of the kits the page and its parts use.
	KitBreakpoints map[string]webbuilder.Breakpoints `json:"kitBreakpoints"`
	// SiteStyle: the site style designs can follow (webbuilder_site_style.go).
	SiteStyle *webbuilder.SiteSettings `json:"siteStyle"`
}

// wbDocument is webbuilder.DocumentResponse with raw doc / page JSON (GET documents/{id} answers it
// too).
type wbDocument struct {
	PostID   int64           `json:"postId"`
	PostType string          `json:"postType"`
	Status   string          `json:"status"`
	Date     string          `json:"date"`
	Rev      int             `json:"rev"`
	Doc      json.RawMessage `json:"doc"` // null: a new page
	Page     json.RawMessage `json:"page"`
	Draft    *wbDraft        `json:"draft"`
	Detached bool            `json:"detached"`
	Modified string          `json:"modified"`
	// DocModified is when the stored doc was last written ("" without one): the editor keeps a
	// draft only when it is newer than this, not than the post (contract §3).
	DocModified string               `json:"docModified"`
	Renderer    int                  `json:"renderer"`
	Lock        *webbuilder.LockInfo `json:"lock"`
}

// wbDraft is webbuilder.DraftState with raw JSON.
type wbDraft struct {
	Doc     json.RawMessage `json:"doc"`
	Page    json.RawMessage `json:"page"`
	BaseRev int             `json:"baseRev"`
	SavedAt string          `json:"savedAt"`
}

// wbDocumentFor is the document of p as user u sees it: the stored doc and its rev, u's own draft
// when it is newer, and another user's live edit lock.
func (s *Server) wbDocumentFor(ctx context.Context, p *store.Post, u *store.User) (*wbDocument, error) {
	wb := s.store.WebBuilder()
	row, err := wb.Doc(ctx, p.ID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		row = nil
	case err != nil:
		return nil, err
	}
	prevDesc := ""
	if row != nil {
		prev, _ := wbPage(row.Page)
		prevDesc = prev.Description
	}
	postPage := wbPostPage(p, prevDesc)
	d := &wbDocument{PostID: p.ID, PostType: p.Type, Status: p.Status, Date: wbPostDate(p), Page: postPage,
		Modified: p.ModifiedAt.UTC().Format(time.RFC3339)}
	if row != nil {
		d.Rev, d.Doc, d.Detached, d.Renderer = row.Rev, row.Doc, row.Detached, row.Renderer
		d.DocModified = row.UpdatedAt.UTC().Format(time.RFC3339) // seconds, like draft.savedAt
		// The post owns title, slug, description and noindex: a rename through /wp/v2/pages (which
		// does not detach) must not be reverted by the next builder publish.
		d.Page = wbPageOverlay(row.Page, postPage)
	}
	var draftLoaded *wbPageLoaded
	if u != nil {
		dr, err := wb.Draft(ctx, p.ID, u.ID)
		switch {
		case errors.Is(err, store.ErrNotFound):
		case err != nil:
			return nil, err
		case row == nil || dr.UpdatedAt.After(row.UpdatedAt):
			page := wbPageJSON(dr.Page, d.Page)
			if dr.UpdatedAt.Before(p.ModifiedAt) {
				// The post changed after the draft was saved: its own fields win.
				page = wbPageOverlay(page, postPage)
			} else if l, ok := wbPageLoadedOf(page); ok && l.Rev == dr.BaseRev {
				// The draft keeps what its own session loaded (autosave stores it): a Pursae SEO
				// edit made since is not mistaken for a builder change of the draft's old values.
				draftLoaded = &l
			}
			d.Draft = &wbDraft{Doc: dr.Doc, Page: page, BaseRev: dr.BaseRev,
				SavedAt: dr.UpdatedAt.UTC().Format(time.RFC3339)}
		}
		d.Lock = s.wbLockHolder(ctx, p, u.ID)
	}
	// What this session loads of the post-owned fields Pursae SEO shares, echoed back on publish
	// (wbPageChanged): a later Pursae SEO edit is not overwritten by the unchanged loaded value.
	var ps webbuilder.PageSettings
	_ = json.Unmarshal(postPage, &ps)
	loaded := wbPageLoaded{Rev: d.Rev, Description: ps.Description, Indexable: ps.Indexable}
	d.Page = wbPageStamp(d.Page, loaded)
	style := s.wbSiteStyle()
	if d.Draft != nil {
		if draftLoaded != nil {
			loaded = *draftLoaded
		}
		d.Draft.Page = wbPageStamp(d.Draft.Page, loaded)
		d.Draft.Doc = webbuilder.TagCodeDoc(wbDocFollowStyle(wbDocSiteDefaults(d.Draft.Doc), style))
	}
	// Every element of a code section gets the id the editor addresses it by as a block. A design
	// that follows the site style gets the current one (webbuilder_site_style.go).
	d.Doc = webbuilder.TagCodeDoc(wbDocFollowStyle(wbDocSiteDefaults(d.Doc), style))
	return d, nil
}

// wbSiteDefaults are the editor's initialDoc.site values. A doc written through REST or MCP may
// leave site settings out ("site": {} validates), and the editor cannot draw a doc without them.
var wbSiteDefaults = map[string]string{"brand": "#4058e3", "fontPair": "elegant", "corners": "soft"}

// wbDocSiteDefaults fills empty or missing site settings of a stored doc with wbSiteDefaults;
// any other doc is returned unchanged.
func wbDocSiteDefaults(raw json.RawMessage) json.RawMessage {
	var doc map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &doc) != nil || doc == nil {
		return raw
	}
	site := map[string]any{}
	if v, ok := doc["site"]; ok && string(v) != "null" && json.Unmarshal(v, &site) != nil {
		return raw
	}
	changed := false
	for k, def := range wbSiteDefaults {
		if v, _ := site[k].(string); v == "" {
			site[k] = def
			changed = true
		}
	}
	if !changed {
		return raw
	}
	b, err := json.Marshal(site)
	if err != nil {
		return raw
	}
	doc["site"] = b
	out, err := json.Marshal(doc)
	if err != nil {
		return raw
	}
	return out
}

// wbPageLoadedOf is the _loaded key of a page object, if it has a valid one.
func wbPageLoadedOf(raw json.RawMessage) (wbPageLoaded, bool) {
	var obj map[string]json.RawMessage
	var l wbPageLoaded
	if json.Unmarshal(raw, &obj) != nil {
		return l, false
	}
	v, ok := obj[wbPageLoadedKey]
	if !ok || json.Unmarshal(v, &l) != nil {
		return l, false
	}
	return l, true
}

// wbPageStamp is the page object raw with its _loaded key set to l.
func wbPageStamp(raw json.RawMessage, l wbPageLoaded) json.RawMessage {
	m := map[string]any{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &m)
	}
	if m == nil {
		m = map[string]any{}
	}
	m[wbPageLoadedKey] = l
	b, err := json.Marshal(m)
	if err != nil {
		return raw
	}
	return b
}

// wbPostPage is the PageSettings the post owns: title, slug, the description and the Pursae SEO
// noindex flag. A builder publish writes the description to both the excerpt and the Pursae SEO
// description; prevDesc is that last published value ("" without a doc). The description is the
// one changed outside the builder since: the Pursae SEO description when it differs from prevDesc
// (Pursae SEO owns the field), else the excerpt (a core edit), else the Pursae SEO description.
func wbPostPage(p *store.Post, prevDesc string) json.RawMessage {
	desc := seogoMeta(p.Meta, "description")
	if (desc == "" || desc == prevDesc) && p.Excerpt != "" {
		desc = p.Excerpt
	}
	b, _ := json.Marshal(webbuilder.PageSettings{Title: p.Title, Slug: p.Slug, Description: desc,
		Indexable: seogoMeta(p.Meta, "noindex") != "noindex"})
	return b
}

// wbPageJSON is a stored page object, or def when nothing was stored ('{}').
func wbPageJSON(raw, def json.RawMessage) json.RawMessage {
	var m map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &m) != nil || len(m) == 0 {
		return def
	}
	return raw
}

// wbPageOverlay is a stored page object (or post when nothing was stored) with the post-owned
// fields (title, slug, description, indexable) taken from post; its other keys are kept.
func wbPageOverlay(raw, post json.RawMessage) json.RawMessage {
	var m map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &m) != nil || len(m) == 0 {
		return post
	}
	var pm map[string]json.RawMessage
	if json.Unmarshal(post, &pm) != nil {
		return raw
	}
	for _, k := range []string{"title", "slug", "description", "indexable"} {
		if v, ok := pm[k]; ok {
			m[k] = v
		}
	}
	b, err := json.Marshal(m)
	if err != nil {
		return raw
	}
	return b
}

// wbBootFor is window.__WEB_BUILDER__ for p. It carries no secret: the REST nonce is the user's
// own wp_rest nonce, AI is reported off and no key or sealed value is read.
func (s *Server) wbBootFor(c *gin.Context, p *store.Post, u *store.User, doc *wbDocument) *wbBoot {
	lb := s.links(c)
	nonce := s.nonce(c, "wp_rest")
	permalink := lb.Permalink(p)
	exit := "/wp-admin/edit"
	if p.Type != "post" {
		exit += "?post_type=" + p.Type
	}
	settings := "" // contract §2: "" without manage_options
	if u.Can("manage_options", nil) {
		settings = lb.HomeURL(wbSettingsURL)
	}
	preview := permalink + previewSep(permalink) + "preview=true"
	editPost := lb.HomeURL("/wp-admin/post?post=" + itoa64(p.ID) + "&action=edit")
	publish := u.Can("publish_post", p)
	parts := s.wbEditorCodeParts(c, p, u, doc)
	// Theme Builder (contract §11.7): a template's boot names its type, rules and preview object;
	// it exits to Appearance › Theme Builder and previews on the front with ?wb_template_preview.
	var tplBoot *webbuilder.TemplateBoot
	var tpl *store.WBTemplate
	if wbIsTemplate(p) {
		if t, err := s.store.WebBuilder().Template(c, p.ID); err == nil {
			tpl = t
			info, _ := s.wbTemplatePreviewInfo(c, t)
			starter := t.Starter
			if starter == "" {
				starter = webbuilder.DefaultStarters[t.Type]
			}
			if starter == "" {
				starter = webbuilder.StarterBlank
			}
			tplBoot = &webbuilder.TemplateBoot{Type: t.Type, Conditions: t.Conditions, Priority: t.Priority, Starter: starter,
				Preview: info, ManageURL: lb.HomeURL(wbThemeBuilderURL())}
			permalink, preview, editPost = info.URL, info.URL, ""
			exit = wbThemeBuilderURL()
		}
		publish = u.Can(webbuilder.TemplateCap, nil)
	}
	dyn := s.wbDynamicFor(c, p, tpl)
	return &wbBoot{
		APIVersion:      webbuilder.APIVersion,
		RendererVersion: webbuilder.RendererVersion,
		Document:        doc,
		User:            webbuilder.UserRef{ID: u.ID, Name: wbUserName(u)},
		REST:            webbuilder.BootREST{Root: lb.HomeURL("/wp-json/"), Namespace: webbuilder.RESTNamespace, Nonce: nonce},
		Links: webbuilder.BootLinks{
			Site:      strings.TrimRight(lb.HomeURL("/"), "/"),
			Permalink: permalink,
			Preview:   preview,
			Exit:      lb.HomeURL(exit),
			EditPost:  editPost,
			Settings:  settings,
		},
		Caps: webbuilder.BootCaps{
			Publish:          publish,
			UploadFiles:      u.Can("upload_files", nil),
			UnfilteredHTML:   u.Can("unfiltered_html", nil),
			EditOthers:       u.Can("edit_others_posts", nil),
			EditThemeOptions: u.Can("edit_theme_options", nil),
		},
		Locale:         s.userLocale(c),
		Fonts:          webbuilder.BootFonts{CSSURL: lb.HomeURL("/wp-json" + webbuilder.RouteFontsCSS + "?_wpnonce=" + nonce), Local: s.wbLocalFonts()},
		Media:          webbuilder.BootMedia{MaxUploadBytes: maxUploadBytes, Types: wbMediaTypes()},
		SiteAssets:     lb.HomeURL(webbuilder.SiteAssetsPath),
		AI:             webbuilder.BootAI{Enabled: false, Providers: []webbuilder.AIProviderInfo{}},
		Template:       tplBoot,
		Site:           s.wbSiteBoot(c, &dyn),
		Dynamic:        dyn,
		CodeParts:      parts,
		KitBreakpoints: s.wbEditorKitBreakpoints(doc, parts),
		SiteStyle:      s.wbSiteStyle(),
	}
}

// wbEditorKitBreakpoints is Boot.kitBreakpoints: the breakpoints of the kits the doc (its draft
// when there is one) and the code parts name.
func (s *Server) wbEditorKitBreakpoints(doc *wbDocument, parts *webbuilder.CodeParts) map[string]webbuilder.Breakpoints {
	out := map[string]webbuilder.Breakpoints{}
	add := func(sections []json.RawMessage) {
		for _, raw := range sections {
			var sec struct {
				Kind    string            `json:"kind"`
				Content map[string]string `json:"content"`
			}
			if json.Unmarshal(raw, &sec) != nil || sec.Kind != webbuilder.KindCode {
				continue
			}
			if k := sec.Content["kit"]; k != "" {
				if _, ok := out[k]; !ok && len(out) < 16 {
					out[k] = s.wbKitBreakpoints(k)
				}
			}
		}
	}
	if doc != nil {
		raw := doc.Doc
		if doc.Draft != nil {
			raw = doc.Draft.Doc
		}
		var d struct {
			Sections []json.RawMessage `json:"sections"`
		}
		if len(raw) > 0 && json.Unmarshal(raw, &d) == nil {
			add(d.Sections)
		}
	}
	if parts != nil {
		for _, p := range []*webbuilder.CodeTemplate{parts.Header, parts.Footer} {
			if p != nil {
				add(p.Sections)
			}
		}
	}
	return out
}

// wbMediaTypes are the MIME types a regular user may upload (wp_get_mime_types), sorted.
var wbMediaTypes = sync.OnceValue(func() []string {
	seen := map[string]bool{}
	out := []string{}
	for _, mt := range media.MimeTypes {
		if !seen[mt] {
			seen[mt] = true
			out = append(out, mt)
		}
	}
	sort.Strings(out)
	return out
})

// wbFontFamilies maps a font key (gfontKey) to its Google Fonts family name.
var wbFontFamilies = sync.OnceValue(func() map[string]string {
	m := map[string]string{}
	for _, fam := range webbuilder.FontFamilies() {
		m[gfontKey(fam)] = fam
	}
	return m
})

// wbLocalFonts are the Google Fonts families already localized under uploads/web-builder/fonts
// (one css/<key>.css per family, gfonts.go), sorted.
func (s *Server) wbLocalFonts() []string {
	out := []string{}
	entries, err := os.ReadDir(s.gfontsDir("css"))
	if err != nil {
		return out
	}
	fams := wbFontFamilies()
	for _, e := range entries {
		key, ok := strings.CutSuffix(e.Name(), ".css")
		if !ok || e.IsDir() {
			continue
		}
		if fam, ok := fams[key]; ok {
			out = append(out, fam)
		}
	}
	sort.Strings(out)
	return out
}

// wbUserName is the name the editor shows for a user.
func wbUserName(u *store.User) string {
	if u.DisplayName != "" {
		return u.DisplayName
	}
	return u.Login
}

// --- edit lock -------------------------------------------------------------------------------------

// wbLockValue matches WordPress' _edit_lock "<unix>:<user id>".
var wbLockValue = regexp.MustCompile(`^([0-9]{1,18}):([0-9]{1,18})$`)

// wbParseLock reads a post's _edit_lock meta.
func wbParseLock(v any) (uid int64, at time.Time, ok bool) {
	str, _ := v.(string)
	m := wbLockValue.FindStringSubmatch(str)
	if m == nil {
		return 0, time.Time{}, false
	}
	ts, _ := strconv.ParseInt(m[1], 10, 64)
	uid, _ = strconv.ParseInt(m[2], 10, 64)
	return uid, time.Unix(ts, 0), true
}

// wbLockHolder is another user's live lock on p (wp_check_post_lock), else nil.
func (s *Server) wbLockHolder(ctx context.Context, p *store.Post, me int64) *webbuilder.LockInfo {
	if !wbLockedByOther(p, me) {
		return nil
	}
	uid, at, _ := wbParseLock(p.Meta[webbuilder.EditLockMeta])
	info := &webbuilder.LockInfo{User: webbuilder.UserRef{ID: uid}, Since: at.UTC().Format(time.RFC3339)}
	if lu, err := s.store.UserByID(ctx, uid); err == nil {
		info.User.Name = wbUserName(lu)
	}
	return info
}

// wbLockedByOther reports whether a user other than me holds a live edit lock on p (no DB access).
func wbLockedByOther(p *store.Post, me int64) bool {
	uid, at, ok := wbParseLock(p.Meta[webbuilder.EditLockMeta])
	return ok && uid != 0 && uid != me && time.Since(at) <= webbuilder.LockTTLSeconds*time.Second
}

// wbTakeLock is wp_set_post_lock: it writes "<now>:<uid>" when the lock is free, expired, unreadable
// or uid's own — or always with take. It reports whether uid now holds the lock. The check and the
// write are one statement, so two editors opening at once cannot both win.
func (s *Server) wbTakeLock(ctx context.Context, postID, uid int64, take bool) (bool, error) {
	now := time.Now().Unix()
	tag, err := s.store.Pool.Exec(ctx, `UPDATE posts SET meta = coalesce(meta, '{}'::jsonb) || jsonb_build_object($2::text, $3::text)
		WHERE id = $1 AND ($4 OR CASE WHEN coalesce(meta->>$2, '') ~ '^[0-9]{1,18}:[0-9]{1,18}$'
			THEN split_part(meta->>$2, ':', 2)::bigint IN (0, $5) OR split_part(meta->>$2, ':', 1)::bigint < $6
			ELSE true END)`,
		postID, webbuilder.EditLockMeta, strconv.FormatInt(now, 10)+":"+strconv.FormatInt(uid, 10), take, uid, now-webbuilder.LockTTLSeconds)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// wbPostDate is the post's publish date for the editor (RFC 3339, "" while it has none).
func wbPostDate(p *store.Post) string {
	if p == nil || p.PublishedAt == nil {
		return ""
	}
	return wbTime(*p.PublishedAt)
}
