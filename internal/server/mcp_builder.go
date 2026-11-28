package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"wpgo/internal/webbuilder"
)

// Pursae Web Builder over MCP: an agent builds and edits real builder pages — the same documents the
// editor opens, so the user keeps changing them by drag and drop — and publishes them without a
// browser. The snapshot is drawn by the editor's own renderer running in the server
// (webbuilder.ServerRenderer, publish with serverRender), so a page published here is the page the
// editor would have published. Every step is an ordinary REST call with the caller's credentials
// (mcpDispatch), so capabilities, Application Password scopes, locks and revisions apply unchanged.
//
// Tools (mcpBuilderTools): web_builder_catalog, web_builder_get_page, web_builder_build_page,
// web_builder_edit_page, web_builder_import_html, web_builder_publish, the Theme Builder template
// tools and web_builder_import_theme_kit. They are listed while the plugin is off and then answer
// with what to do (ask the user, activate_plugin golementor), so an agent can offer to turn it on.

// mcpRun is a tool implemented in Go; the result is sent as JSON.
type mcpRun func(s *Server, c *gin.Context, args map[string]any) (any, error)

// mcpUserError is an error the agent can act on (sent as the tool's error text).
type mcpUserError struct{ msg string }

func (e *mcpUserError) Error() string { return e.msg }

func mcpErrf(format string, a ...any) error { return &mcpUserError{fmt.Sprintf(format, a...)} }

// mcpRunTool runs a Go tool and wraps the answer like mcpCall does.
func (s *Server) mcpRunTool(c *gin.Context, t *mcpTool, args map[string]any) map[string]any {
	for k := range args {
		if _, known := t.Props[k]; !known && !t.Open {
			return mcpToolError("Unknown argument: " + k)
		}
	}
	started := time.Now()
	out, err := t.run(s, c, args)
	status := http.StatusOK
	if err != nil {
		status = http.StatusBadRequest
	}
	if !t.readOnly {
		s.mcpLogActivity(c, t.Name, http.MethodPost, mcpToolRoute(t), status, time.Since(started))
	}
	if err != nil && out == nil {
		return mcpToolError(err.Error())
	}
	b, _ := json.Marshal(out)
	text, truncated := mcpTruncate(string(b), mcpMaxText)
	var res map[string]any
	if err != nil {
		res = mcpToolError(err.Error())
	} else {
		res = map[string]any{"content": []map[string]any{{"type": "text", "text": text}}}
	}
	if m, ok := out.(map[string]any); ok && !truncated {
		res["structuredContent"] = m
	}
	return res
}

func mcpToolError(msg string) map[string]any {
	return map[string]any{"content": []map[string]any{{"type": "text", "text": msg}}, "isError": true}
}

// mcpREST is one in-process REST call; a 4xx/5xx answer becomes an error carrying the REST message.
func (s *Server) mcpREST(c *gin.Context, method, route string, body any, out any) error {
	var raw []byte
	ct := ""
	if body != nil {
		raw, _ = json.Marshal(body)
		ct = "application/json"
	}
	status, resp := s.mcpDispatch(c, method, route, raw, ct)
	if status >= 400 {
		var e struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Data    any    `json:"data"`
		}
		_ = json.Unmarshal(resp, &e)
		msg := e.Message
		if msg == "" {
			msg = http.StatusText(status)
		}
		if e.Code != "" {
			msg = e.Code + ": " + msg
		}
		if d, ok := e.Data.(map[string]any); ok {
			if errs, ok := d["errors"]; ok {
				b, _ := json.Marshal(errs)
				msg += " " + string(b)
			}
		}
		return &mcpUserError{msg}
	}
	if out != nil && len(resp) > 0 {
		if err := json.Unmarshal(resp, out); err != nil {
			return fmt.Errorf("unexpected answer from %s: %w", route, err)
		}
	}
	return nil
}

// mcpNeedBuilder is the answer of every builder tool while the plugin is off.
func (s *Server) mcpNeedBuilder() error {
	if s.wbOn() {
		return nil
	}
	return mcpErrf("Pursae Web Builder is turned off on this site. Ask the user whether to turn it on; if they agree, call activate_plugin with slug %q (it also switches to the Hello Pursae theme; the previous theme comes back when the builder is turned off), then call this tool again.", webbuilder.PluginSlug)
}

func (s *Server) mcpRenderer() (*webbuilder.ServerRenderer, error) {
	r, err := webbuilder.SharedServerRenderer(s.web)
	if err != nil {
		return nil, mcpErrf("This Pursae build cannot draw Web Builder pages on the server (%v). Save a draft instead and ask the user to publish it in the editor.", err)
	}
	return r, nil
}

// --- arguments ---------------------------------------------------------------------------------

func argInt(args map[string]any, k string) int64 {
	switch v := args[k].(type) {
	case float64:
		return int64(v)
	case int:
		return int64(v)
	case int64:
		return v
	case string:
		n, _ := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		return n
	}
	return 0
}

func argStr(args map[string]any, k string) string {
	v, _ := args[k].(string)
	return strings.TrimSpace(v)
}

func argBool(args map[string]any, k string) bool {
	switch v := args[k].(type) {
	case bool:
		return v
	case string:
		return v == "true" || v == "1"
	}
	return false
}

func argObj(args map[string]any, k string) map[string]any {
	v, _ := args[k].(map[string]any)
	return v
}

func argList(args map[string]any, k string) []any {
	v, _ := args[k].([]any)
	return v
}

// --- documents ---------------------------------------------------------------------------------

// mcpWBDoc is GET documents/{id} as the tools use it.
type mcpWBDoc struct {
	PostID   int64           `json:"postId"`
	PostType string          `json:"postType"`
	Status   string          `json:"status"`
	Rev      int             `json:"rev"`
	Doc      json.RawMessage `json:"doc"`
	Page     json.RawMessage `json:"page"`
	Draft    *struct {
		Doc     json.RawMessage `json:"doc"`
		Page    json.RawMessage `json:"page"`
		BaseRev int             `json:"baseRev"`
		SavedAt string          `json:"savedAt"`
	} `json:"draft"`
	Detached bool `json:"detached"`
}

// wbDocJSON is a doc as maps, so fields the Go types do not know travel unchanged.
type wbDocJSON struct {
	Sections []map[string]any `json:"sections"`
	Site     map[string]any   `json:"site"`
	rest     map[string]any
}

func parseWBDoc(raw json.RawMessage) (*wbDocJSON, error) {
	d := &wbDocJSON{rest: map[string]any{}}
	if len(raw) == 0 || string(raw) == "null" {
		return d, nil
	}
	if err := json.Unmarshal(raw, &d.rest); err != nil {
		return nil, err
	}
	if v, ok := d.rest["sections"].([]any); ok {
		for _, x := range v {
			if m, ok := x.(map[string]any); ok {
				d.Sections = append(d.Sections, m)
			}
		}
	}
	d.Site, _ = d.rest["site"].(map[string]any)
	delete(d.rest, "sections")
	delete(d.rest, "site")
	return d, nil
}

func (d *wbDocJSON) marshal() map[string]any {
	out := map[string]any{}
	for k, v := range d.rest {
		out[k] = v
	}
	secs := d.Sections
	if secs == nil {
		secs = []map[string]any{}
	}
	out["sections"] = secs
	site := d.Site
	if site == nil {
		site = map[string]any{}
	}
	out["site"] = site
	return out
}

func (d *wbDocJSON) index(id string) int {
	for i, s := range d.Sections {
		if sid, _ := s["id"].(string); sid == id {
			return i
		}
	}
	return -1
}

// mcpLoadDoc reads a builder document; useDraft prefers the caller's newer autosave.
func (s *Server) mcpLoadDoc(c *gin.Context, id int64, useDraft bool) (*mcpWBDoc, *wbDocJSON, json.RawMessage, error) {
	var d mcpWBDoc
	if err := s.mcpREST(c, http.MethodGet, "/web-builder/v2/documents/"+itoa64(id), nil, &d); err != nil {
		return nil, nil, nil, err
	}
	raw, page := d.Doc, d.Page
	if useDraft && d.Draft != nil && len(d.Draft.Doc) > 0 {
		raw, page = d.Draft.Doc, d.Draft.Page
	}
	doc, err := parseWBDoc(raw)
	if err != nil {
		return nil, nil, nil, err
	}
	return &d, doc, page, nil
}

// mcpSite fills the site settings a doc needs (brand, fontPair, corners) from the catalog defaults.
func mcpSite(r *webbuilder.ServerRenderer, site map[string]any, patch map[string]any) (map[string]any, error) {
	out := map[string]any{}
	if cat, err := r.Catalog(); err == nil {
		var c struct {
			Site map[string]any `json:"site"`
		}
		if json.Unmarshal(cat, &c) == nil {
			for k, v := range c.Site {
				out[k] = v
			}
		}
	}
	for k, v := range site {
		out[k] = v
	}
	for k, v := range patch {
		out[k] = v
	}
	return out, nil
}

// mcpCreateSections turns specs (and an html page) into complete sections.
func mcpCreateSections(r *webbuilder.ServerRenderer, specs []any, htmlSrc string) ([]map[string]any, []string, string, error) {
	var out []map[string]any
	var warnings []string
	brand := ""
	all := append([]any{}, specs...)
	if htmlSrc != "" {
		res, err := webbuilder.ImportHTML(htmlSrc, webbuilder.ImportOptions{})
		if err != nil {
			return nil, nil, "", mcpErrf("html: %v", err)
		}
		for _, s := range res.Sections {
			all = append(all, s)
		}
		warnings = append(warnings, res.Warnings...)
		brand = res.Brand
	}
	for i, x := range all {
		spec, ok := x.(map[string]any)
		if !ok {
			return nil, nil, "", mcpErrf("sections[%d] must be an object like {\"kind\": \"hero\", \"design\": \"…\", \"content\": {…}}", i)
		}
		b, _ := json.Marshal(spec)
		sec, err := r.CreateSection(b)
		if err != nil {
			return nil, nil, "", mcpErrf("sections[%d]: %s", i, strings.TrimPrefix(err.Error(), "webbuilder: "))
		}
		var m map[string]any
		if err := json.Unmarshal(sec, &m); err != nil {
			return nil, nil, "", err
		}
		warnings = append(warnings, mcpSampleText(r, spec, m)...)
		out = append(out, m)
	}
	return out, warnings, brand, nil
}

// mcpSampleText removes the design's sample copy (Pursae's own demo text: "Early release · 0.1",
// a fake terminal, "One binary" cards…) that the spec did not replace, so a partial spec never
// publishes it: text fields the design shows and the spec left out are emptied, and the sample
// items are dropped when the spec gives no items. Pictures and links stay (a sample picture is
// still a picture). The answer lists what was emptied so the caller can fill it in.
func mcpSampleText(r *webbuilder.ServerRenderer, spec, sec map[string]any) []string {
	kind, _ := sec["kind"].(string)
	if kind == "custom" || kind == "layout" || kind == "html" || kind == "loop" {
		return nil
	}
	visible := mcpVisibleFields(r, kind, sec)
	given, _ := spec["content"].(map[string]any)
	content, _ := sec["content"].(map[string]any)
	var left []string
	for k, v := range content {
		str, _ := v.(string)
		if _, ok := given[k]; ok || !visible[k] || len([]rune(str)) <= 2 {
			continue
		}
		if strings.HasPrefix(str, "http") || strings.HasPrefix(str, "#") || strings.HasPrefix(str, "/") {
			continue // links and images: a sample picture is still a picture
		}
		content[k] = ""
		left = append(left, k)
	}
	droppedItems := 0
	if _, ok := spec["items"]; !ok {
		if items, _ := sec["items"].([]any); len(items) > 0 {
			droppedItems = len(items)
			sec["items"] = []any{}
		}
	}
	if len(left) == 0 && droppedItems == 0 {
		return nil
	}
	name, _ := sec["name"].(string)
	id, _ := sec["id"].(string)
	var what []string
	if len(left) > 0 {
		what = append(what, "emptied the sample text of "+strings.Join(sortedStrings(left), ", "))
	}
	if droppedItems > 0 {
		what = append(what, fmt.Sprintf("removed %d sample items (give items to add your own)", droppedItems))
	}
	return []string{fmt.Sprintf("section %s (%s, %q): %s — set them with web_builder_edit_page (op update, content/items) or pick another design", id, kind, name, strings.Join(what, "; "))}
}

// mcpVisibleFields are the content keys a section's design shows (from the catalog).
func mcpVisibleFields(r *webbuilder.ServerRenderer, kind string, sec map[string]any) map[string]bool {
	out := map[string]bool{}
	raw, err := r.Catalog()
	if err != nil {
		return out
	}
	var cat struct {
		Kinds []struct {
			Kind   string `json:"kind"`
			Fields []struct {
				Key string `json:"key"`
			} `json:"fields"`
			Designs []struct {
				ID     string   `json:"id"`
				Extras []string `json:"extras"`
				Hide   []string `json:"hide"`
			} `json:"designs"`
			ExtraFields []struct {
				Key string `json:"key"`
			} `json:"extraFields"`
		} `json:"kinds"`
	}
	if json.Unmarshal(raw, &cat) != nil {
		return out
	}
	design := "default"
	if c, ok := sec["content"].(map[string]any); ok {
		if v, _ := c["variant"].(string); v != "" {
			design = v
		}
	}
	for _, k := range cat.Kinds {
		if k.Kind != kind {
			continue
		}
		extra := map[string]bool{}
		for _, f := range k.ExtraFields {
			extra[f.Key] = true
		}
		for _, f := range k.Fields {
			if !extra[f.Key] {
				out[f.Key] = true
			}
		}
		for _, d := range k.Designs {
			if d.ID != design {
				continue
			}
			for _, h := range d.Hide {
				delete(out, h)
			}
			for _, e := range d.Extras {
				out[e] = true
			}
		}
	}
	return out
}

func sortedStrings(in []string) []string {
	out := append([]string{}, in...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// mcpSaveDoc publishes (server render) or saves the caller's draft, and answers with where the page is.
func (s *Server) mcpSaveDoc(c *gin.Context, id int64, d *mcpWBDoc, doc *wbDocJSON, page json.RawMessage, pagePatch map[string]any, status string, warnings []string) (map[string]any, error) {
	pg := map[string]any{}
	if len(page) > 0 && string(page) != "null" {
		_ = json.Unmarshal(page, &pg)
	}
	for k, v := range pagePatch {
		pg[k] = v
	}
	if _, ok := pg["title"]; !ok {
		pg["title"] = ""
	}
	for _, k := range []string{"slug", "description"} {
		if _, ok := pg[k]; !ok {
			pg[k] = ""
		}
	}
	if _, ok := pg["indexable"]; !ok {
		pg["indexable"] = true
	}
	out := map[string]any{"post_id": id, "edit_url": s.mcpAdminURL(c, wbEditURL(id))}
	if status == "draft-autosave" {
		var res map[string]any
		if err := s.mcpREST(c, http.MethodPut, "/web-builder/v2/documents/"+itoa64(id)+"/autosave", map[string]any{"doc": doc.marshal(), "page": pg, "baseRev": d.Rev}, &res); err != nil {
			return nil, err
		}
		out["saved"] = "draft"
		out["note"] = "Saved as your draft in the editor; the live page did not change. Publish it with web_builder_publish."
	} else {
		var res webbuilder.PublishResponse
		body := map[string]any{"baseRev": d.Rev, "status": status, "doc": doc.marshal(), "page": pg, "serverRender": true, "reattach": d.Detached}
		if err := s.mcpREST(c, http.MethodPost, "/web-builder/v2/documents/"+itoa64(id)+"/publish", body, &res); err != nil {
			return nil, err
		}
		out["status"], out["rev"], out["url"] = res.Status, res.Rev, res.Permalink
		if len(res.Stripped) > 0 {
			out["stripped"] = res.Stripped
			warnings = append(warnings, "the server removed parts the account may not publish: "+strings.Join(res.Stripped, ", "))
		}
	}
	out["sections"] = mcpOutline(doc)
	if len(warnings) > 0 {
		out["warnings"] = warnings
	}
	return out, nil
}

func (s *Server) mcpAdminURL(c *gin.Context, p string) string {
	return s.links(c).HomeURL(p)
}

// mcpOutline is a short list of a doc's sections.
func mcpOutline(doc *wbDocJSON) []map[string]any {
	out := []map[string]any{}
	for _, sec := range doc.Sections {
		o := map[string]any{"id": sec["id"], "kind": sec["kind"], "name": sec["name"]}
		if c, ok := sec["content"].(map[string]any); ok {
			if v, ok := c["variant"].(string); ok && v != "" {
				o["design"] = v
			}
			for _, k := range []string{"title", "brand", "text", "quote"} {
				if v, ok := c[k].(string); ok && v != "" {
					o["headline"] = webbuilder.ToPlain(v)
					break
				}
			}
		}
		if h, ok := sec["hidden"].(bool); ok && h {
			o["hidden"] = true
		}
		out = append(out, o)
	}
	return out
}

// mcpEnsurePost returns the post to build: post_id, or a new page/post titled title.
func (s *Server) mcpEnsurePost(c *gin.Context, args map[string]any) (int64, string, error) {
	if id := argInt(args, "post_id"); id > 0 {
		return id, "", nil
	}
	title := argStr(args, "title")
	if title == "" {
		return 0, "", mcpErrf("give post_id (an existing page) or title (a new page)")
	}
	typ := argStr(args, "post_type")
	route := "/wp/v2/pages"
	switch typ {
	case "", "page":
	case "post":
		route = "/wp/v2/posts"
	default:
		return 0, "", mcpErrf("post_type must be page or post (other types: create the post first, then pass post_id)")
	}
	body := map[string]any{"title": title, "status": "draft"}
	if slug := argStr(args, "slug"); slug != "" {
		body["slug"] = slug
	}
	if p := argInt(args, "parent"); p > 0 && route == "/wp/v2/pages" {
		body["parent"] = p
	}
	var created struct {
		ID int64 `json:"id"`
	}
	if err := s.mcpREST(c, http.MethodPost, route, body, &created); err != nil {
		return 0, "", err
	}
	return created.ID, route, nil
}

func mcpPublishStatus(args map[string]any) (string, error) {
	st := argStr(args, "status")
	switch st {
	case "", "publish":
		return "publish", nil
	case "draft":
		return "draft-autosave", nil
	case "private", "pending":
		return st, nil
	case "unpublished":
		return "draft", nil
	}
	return "", mcpErrf("status must be publish, draft (your editor draft; the live page is untouched), unpublished (store as a draft post), private or pending")
}

// --- tools ---------------------------------------------------------------------------------------

func mcpWBCatalog(s *Server, c *gin.Context, args map[string]any) (any, error) {
	r, err := s.mcpRenderer()
	if err != nil {
		return nil, err
	}
	raw, err := r.Catalog()
	if err != nil {
		return nil, err
	}
	var cat map[string]any
	if err := json.Unmarshal(raw, &cat); err != nil {
		return nil, err
	}
	kinds, _ := cat["kinds"].([]any)
	if k := argStr(args, "kind"); k != "" {
		for _, x := range kinds {
			if m, _ := x.(map[string]any); m["kind"] == k {
				return m, nil
			}
		}
		return nil, mcpErrf("unknown kind %q", k)
	}
	// The overview: every kind with its fields and design ids; details per kind with kind=….
	var brief []any
	for _, x := range kinds {
		m, _ := x.(map[string]any)
		var fields []string
		for _, f := range asList(m["fields"]) {
			fm, _ := f.(map[string]any)
			fields = append(fields, fmt.Sprint(fm["key"]))
		}
		var designs []string
		for _, d := range asList(m["designs"]) {
			dm, _ := d.(map[string]any)
			tags := ""
			if t := asList(dm["tags"]); len(t) > 0 {
				var ts []string
				for _, x := range t {
					ts = append(ts, fmt.Sprint(x))
				}
				tags = " [" + strings.Join(ts, "/") + "]"
			}
			designs = append(designs, fmt.Sprintf("%v=%v%s", dm["id"], dm["name"], tags))
		}
		e := map[string]any{"kind": m["kind"], "name": m["name"], "blurb": m["blurb"], "fields": fields}
		if it := m["items"]; it != nil {
			e["items"] = it
		}
		if len(designs) > 0 {
			e["designs"] = designs
		}
		brief = append(brief, e)
	}
	cat["kinds"] = brief
	cat["icons"] = fmt.Sprintf("%d icon names; featureIcons are the usual card icons", len(asList(cat["icons"])))
	cat["guide"] = mcpBuilderGuide
	return cat, nil
}

func asList(v any) []any {
	l, _ := v.([]any)
	return l
}

func mcpWBGetPage(s *Server, c *gin.Context, args map[string]any) (any, error) {
	if err := s.mcpNeedBuilder(); err != nil {
		return nil, err
	}
	id := argInt(args, "post_id")
	if id <= 0 {
		return nil, mcpErrf("post_id is required")
	}
	d, doc, page, err := s.mcpLoadDoc(c, id, argBool(args, "draft"))
	if err != nil {
		return nil, err
	}
	out := map[string]any{"post_id": d.PostID, "post_type": d.PostType, "status": d.Status, "rev": d.Rev, "detached": d.Detached,
		"has_draft": d.Draft != nil, "edit_url": s.mcpAdminURL(c, wbEditURL(id))}
	var pg any
	_ = json.Unmarshal(page, &pg)
	out["page"] = pg
	if d.Doc == nil || string(d.Doc) == "null" {
		out["note"] = "No builder document yet: build one with web_builder_build_page."
	}
	if argBool(args, "full") {
		out["doc"] = doc.marshal()
	} else {
		out["site"] = doc.Site
		var secs []any
		for _, sec := range doc.Sections {
			secs = append(secs, mcpSectionBrief(sec))
		}
		out["sections"] = secs
	}
	return out, nil
}

// mcpSectionBrief is a section without style detail: what an agent needs to edit its words.
func mcpSectionBrief(sec map[string]any) map[string]any {
	o := map[string]any{"id": sec["id"], "kind": sec["kind"], "name": sec["name"]}
	if h, _ := sec["hidden"].(bool); h {
		o["hidden"] = true
	}
	if c, ok := sec["content"].(map[string]any); ok {
		o["content"] = c
	}
	var items []any
	for _, x := range asList(sec["items"]) {
		it, _ := x.(map[string]any)
		e := map[string]any{"id": it["id"], "title": it["title"], "text": it["text"]}
		for _, k := range []string{"icon", "href", "node", "parent", "tag"} {
			if v, ok := it[k]; ok && v != "" {
				e[k] = v
			}
		}
		if sub, ok := it["sub"].(map[string]any); ok {
			e["sub"] = mcpSectionBrief(sub)
		}
		items = append(items, e)
	}
	if len(items) > 0 {
		o["items"] = items
	}
	if st, ok := sec["style"].(map[string]any); ok {
		o["style"] = map[string]any{"palette": st["palette"], "align": st["align"], "padding": st["padding"]}
	}
	return o
}

func mcpWBBuildPage(s *Server, c *gin.Context, args map[string]any) (any, error) {
	if err := s.mcpNeedBuilder(); err != nil {
		return nil, err
	}
	r, err := s.mcpRenderer()
	if err != nil {
		return nil, err
	}
	status, err := mcpPublishStatus(args)
	if err != nil {
		return nil, err
	}
	specs := argList(args, "sections")
	htmlSrc, _ := args["html"].(string)
	if len(specs) == 0 && strings.TrimSpace(htmlSrc) == "" {
		return nil, mcpErrf("give sections (see web_builder_catalog) or html")
	}
	mode := argStr(args, "mode")
	switch mode {
	case "", "replace", "append", "prepend":
	default:
		return nil, mcpErrf("mode must be replace, append or prepend")
	}
	// Build the sections before creating anything, so a bad spec leaves no empty page behind.
	secs, warnings, brand, err := mcpCreateSections(r, specs, htmlSrc)
	if err != nil {
		return nil, err
	}
	id, createdRoute, err := s.mcpEnsurePost(c, args)
	if err != nil {
		return nil, err
	}
	d, doc, page, err := s.mcpLoadDoc(c, id, false)
	if err != nil {
		return nil, err
	}
	switch mode {
	case "", "replace":
		doc.Sections = secs
	case "append":
		doc.Sections = append(doc.Sections, secs...)
	case "prepend":
		doc.Sections = append(secs, doc.Sections...)
	}
	sitePatch := argObj(args, "site")
	if brand != "" && sitePatch["brand"] == nil && doc.Site == nil {
		sitePatch = map[string]any{"brand": brand}
		for k, v := range argObj(args, "site") {
			sitePatch[k] = v
		}
	}
	if doc.Site, err = mcpSite(r, doc.Site, sitePatch); err != nil {
		return nil, err
	}
	pagePatch := argObj(args, "page")
	if t := argStr(args, "title"); t != "" && pagePatch["title"] == nil {
		if pagePatch == nil {
			pagePatch = map[string]any{}
		}
		pagePatch["title"] = t
	}
	if l := argStr(args, "layout"); l != "" {
		if l != "site" && l != "canvas" {
			return nil, mcpErrf("layout must be site (the theme's header and footer around the page) or canvas (only this page's own sections)")
		}
		if pagePatch == nil {
			pagePatch = map[string]any{}
		}
		pagePatch["layout"] = l
	}
	out, err := s.mcpSaveDoc(c, id, d, doc, page, pagePatch, status, warnings)
	if err != nil {
		if createdRoute != "" {
			_ = s.mcpREST(c, http.MethodDelete, createdRoute+"/"+itoa64(id)+"?force=true", nil, nil)
		}
		return nil, err
	}
	out["created"] = createdRoute != ""
	if argBool(args, "front_page") {
		if err := s.mcpREST(c, http.MethodPost, "/wp/v2/settings", map[string]any{"show_on_front": "page", "page_on_front": id}, nil); err != nil {
			out["warnings"] = append(asStrings(out["warnings"]), "could not set the front page: "+err.Error())
		} else {
			out["front_page"] = true
		}
	}
	return out, nil
}

func asStrings(v any) []string {
	l, _ := v.([]string)
	return l
}

// mcpWBEditPage applies section operations to a page and publishes it (or saves the draft).
func mcpWBEditPage(s *Server, c *gin.Context, args map[string]any) (any, error) {
	if err := s.mcpNeedBuilder(); err != nil {
		return nil, err
	}
	r, err := s.mcpRenderer()
	if err != nil {
		return nil, err
	}
	status, err := mcpPublishStatus(args)
	if err != nil {
		return nil, err
	}
	id := argInt(args, "post_id")
	if id <= 0 {
		return nil, mcpErrf("post_id is required")
	}
	d, doc, page, err := s.mcpLoadDoc(c, id, argBool(args, "from_draft"))
	if err != nil {
		return nil, err
	}
	var warnings []string
	for i, x := range argList(args, "ops") {
		op, _ := x.(map[string]any)
		if op == nil {
			return nil, mcpErrf("ops[%d] must be an object", i)
		}
		w, err := mcpApplyOp(r, doc, op)
		if err != nil {
			return nil, mcpErrf("ops[%d] (%v): %v", i, op["op"], err)
		}
		warnings = append(warnings, w...)
	}
	if site := argObj(args, "site"); site != nil {
		if doc.Site, err = mcpSite(r, doc.Site, site); err != nil {
			return nil, err
		}
	} else if doc.Site == nil {
		doc.Site, _ = mcpSite(r, nil, nil)
	}
	return s.mcpSaveDoc(c, id, d, doc, page, argObj(args, "page"), status, warnings)
}

func mcpApplyOp(r *webbuilder.ServerRenderer, doc *wbDocJSON, op map[string]any) ([]string, error) {
	sid := argStr(op, "section_id")
	find := func() (int, error) {
		if sid == "" {
			return -1, errors.New("section_id is required")
		}
		i := doc.index(sid)
		if i < 0 {
			var ids []string
			for _, s := range doc.Sections {
				ids = append(ids, fmt.Sprint(s["id"]))
			}
			return -1, fmt.Errorf("no section %q; sections: %s", sid, strings.Join(ids, ", "))
		}
		return i, nil
	}
	position := func(n int) (int, error) {
		if b := argStr(op, "before"); b != "" {
			if i := doc.index(b); i >= 0 {
				return i, nil
			}
			return 0, fmt.Errorf("no section %q", b)
		}
		if a := argStr(op, "after"); a != "" {
			if i := doc.index(a); i >= 0 {
				return i + 1, nil
			}
			return 0, fmt.Errorf("no section %q", a)
		}
		if _, ok := op["index"]; ok {
			i := int(argInt(op, "index"))
			if i < 0 || i > n {
				return 0, fmt.Errorf("index must be 0…%d", n)
			}
			return i, nil
		}
		return n, nil
	}
	insert := func(at int, secs []map[string]any) {
		doc.Sections = append(doc.Sections[:at], append(secs, doc.Sections[at:]...)...)
	}
	switch argStr(op, "op") {
	case "add":
		var specs []any
		if sp := argObj(op, "section"); sp != nil {
			specs = append(specs, sp)
		}
		specs = append(specs, argList(op, "sections")...)
		htmlSrc, _ := op["html"].(string)
		secs, warnings, _, err := mcpCreateSections(r, specs, htmlSrc)
		if err != nil {
			return nil, err
		}
		if len(secs) == 0 {
			return nil, errors.New("give section, sections or html")
		}
		at, err := position(len(doc.Sections))
		if err != nil {
			return nil, err
		}
		insert(at, secs)
		return warnings, nil
	case "update":
		i, err := find()
		if err != nil {
			return nil, err
		}
		sec := doc.Sections[i]
		if d := argStr(op, "design"); d != "" {
			b, _ := json.Marshal(sec)
			nb, err := r.ApplyDesign(b, d)
			if err != nil {
				return nil, errors.New(strings.TrimPrefix(err.Error(), "webbuilder: "))
			}
			sec = map[string]any{}
			if err := json.Unmarshal(nb, &sec); err != nil {
				return nil, err
			}
		}
		for _, k := range []string{"content", "style", "bg", "grid", "box", "els"} {
			patch := argObj(op, k)
			if patch == nil {
				continue
			}
			cur, _ := sec[k].(map[string]any)
			if cur == nil {
				cur = map[string]any{}
			}
			for pk, pv := range patch {
				if pv == nil {
					delete(cur, pk)
				} else {
					cur[pk] = pv
				}
			}
			sec[k] = cur
		}
		if v, ok := op["name"].(string); ok && v != "" {
			sec["name"] = v
		}
		if v, ok := op["hidden"].(bool); ok {
			sec["hidden"] = v
		}
		if items, ok := op["items"].([]any); ok {
			// a full new item list (ids kept when given); fillIds runs through CreateSection
			spec := map[string]any{"kind": sec["kind"], "items": items}
			b, _ := json.Marshal(spec)
			fresh, err := r.CreateSection(b)
			if err != nil {
				return nil, errors.New(strings.TrimPrefix(err.Error(), "webbuilder: "))
			}
			var m map[string]any
			_ = json.Unmarshal(fresh, &m)
			sec["items"] = m["items"]
		}
		for _, x := range argList(op, "item_updates") {
			up, _ := x.(map[string]any)
			iid := argStr(up, "id")
			found := false
			for _, y := range asList(sec["items"]) {
				it, _ := y.(map[string]any)
				if it["id"] == iid {
					for k, v := range up {
						if k != "id" {
							it[k] = v
						}
					}
					found = true
				}
			}
			if !found {
				return nil, fmt.Errorf("section %s has no item %q", sid, iid)
			}
		}
		doc.Sections[i] = sec
	case "remove":
		i, err := find()
		if err != nil {
			return nil, err
		}
		doc.Sections = append(doc.Sections[:i], doc.Sections[i+1:]...)
	case "move":
		i, err := find()
		if err != nil {
			return nil, err
		}
		sec := doc.Sections[i]
		doc.Sections = append(doc.Sections[:i], doc.Sections[i+1:]...)
		at, err := position(len(doc.Sections))
		if err != nil {
			return nil, err
		}
		insert(at, []map[string]any{sec})
	case "duplicate":
		i, err := find()
		if err != nil {
			return nil, err
		}
		b, _ := json.Marshal(doc.Sections[i])
		var cp map[string]any
		_ = json.Unmarshal(b, &cp)
		cp["id"] = ""
		mcpClearIDs(cp)
		nb, _ := json.Marshal(cp)
		fresh, err := r.CreateSection(nb)
		if err != nil {
			return nil, err
		}
		var m map[string]any
		_ = json.Unmarshal(fresh, &m)
		insert(i+1, []map[string]any{m})
	default:
		return nil, errors.New("op must be add, update, remove, move or duplicate")
	}
	return nil, nil
}

// mcpClearIDs empties item ids (and nested sections') so CreateSection gives the copy new ones;
// custom nodes keep their parent links through a rename map.
func mcpClearIDs(sec map[string]any) {
	ren := map[string]string{}
	n := 0
	for _, x := range asList(sec["items"]) {
		it, _ := x.(map[string]any)
		old, _ := it["id"].(string)
		n++
		nid := fmt.Sprintf("%s_c%d%d", old, time.Now().UnixNano()%100000, n)
		if len(nid) > 64 {
			nid = nid[len(nid)-64:]
		}
		ren[old] = nid
		it["id"] = nid
		if sub, ok := it["sub"].(map[string]any); ok {
			sub["id"] = ""
			mcpClearIDs(sub)
		}
	}
	for _, x := range asList(sec["items"]) {
		it, _ := x.(map[string]any)
		if p, ok := it["parent"].(string); ok && ren[p] != "" {
			it["parent"] = ren[p]
		}
	}
	if els, ok := sec["els"].(map[string]any); ok {
		next := map[string]any{}
		for k, v := range els {
			for old, nid := range ren {
				k = strings.Replace(k, "items."+old+".", "items."+nid+".", 1)
			}
			next[k] = v
		}
		sec["els"] = next
	}
}

func mcpWBImportHTML(s *Server, c *gin.Context, args map[string]any) (any, error) {
	if strings.TrimSpace(argStr(args, "html")) == "" {
		return nil, mcpErrf("html is required")
	}
	if err := s.mcpNeedBuilder(); err != nil {
		return nil, err
	}
	method := argStr(args, "method")
	if method == "" {
		method = "exact"
		if u := restCurrent(c); wbUnfiltered(c, u) {
			method = "code" // 1:1 by construction for anyone who may publish the design's own code
		}
	}
	switch method {
	case "code":
		return mcpWBImportCode(s, c, args)
	case "exact":
		return s.mcpWBImportHTMLExact(c, args)
	case "quick":
	default:
		return nil, mcpErrf("method must be code (the design's own markup, 1:1), exact (measured in a browser, rebuilt as builder sections) or quick (read from the source, approximate)")
	}
	delete(args, "method")
	if argStr(args, "layout") == "" {
		// An imported page brings its own header and footer as free-form sections.
		args["layout"] = "canvas"
	}
	return mcpWBBuildPage(s, c, args)
}

// mcpWBPublish publishes a page's current document (or the caller's draft) with the server renderer.
func mcpWBPublish(s *Server, c *gin.Context, args map[string]any) (any, error) {
	if err := s.mcpNeedBuilder(); err != nil {
		return nil, err
	}
	if _, err := s.mcpRenderer(); err != nil {
		return nil, err
	}
	id := argInt(args, "post_id")
	if id <= 0 {
		return nil, mcpErrf("post_id is required")
	}
	status := argStr(args, "status")
	if status == "" {
		status = "publish"
	}
	if status != "publish" && status != "private" && status != "pending" && status != "draft" {
		return nil, mcpErrf("status must be publish, private, pending or draft")
	}
	d, doc, page, err := s.mcpLoadDoc(c, id, true)
	if err != nil {
		return nil, err
	}
	if len(doc.Sections) == 0 && (d.Doc == nil || string(d.Doc) == "null") {
		return nil, mcpErrf("page %d has no builder document yet; build it with web_builder_build_page", id)
	}
	return s.mcpSaveDoc(c, id, d, doc, page, nil, status, nil)
}

// mcpWBImportThemeKit imports a theme of the theme library and publishes what it created.
func mcpWBImportThemeKit(s *Server, c *gin.Context, args map[string]any) (any, error) {
	if err := s.mcpNeedBuilder(); err != nil {
		return nil, err
	}
	if _, err := s.mcpRenderer(); err != nil {
		return nil, err
	}
	slug := argStr(args, "slug")
	if !mcpSafeSegment.MatchString(slug) {
		return nil, mcpErrf("slug is required (see web_builder_list_theme_kits)")
	}
	mode := argStr(args, "mode")
	if mode == "" {
		mode = "replace"
	}
	if mode != "replace" && mode != "keep" {
		return nil, mcpErrf("mode must be replace (the kit's templates go live) or keep (saved as drafts)")
	}
	pages := true
	if v, ok := args["pages"].(bool); ok {
		pages = v
	}
	type item struct {
		ID    int64  `json:"id"`
		Title string `json:"title"`
		Type  string `json:"type"`
	}
	var imp struct {
		Templates []item `json:"templates"`
		Pages     []item `json:"pages"`
	}
	base := "/web-builder/v2/themes/" + slug
	if err := s.mcpREST(c, http.MethodPost, base+"/import", map[string]any{"mode": mode, "pages": pages}, &imp); err != nil {
		return nil, err
	}
	var tplIDs, pageIDs []int64
	var problems []string
	publish := func(it item) bool {
		d, doc, page, err := s.mcpLoadDoc(c, it.ID, false)
		if err == nil {
			_, err = s.mcpSaveDoc(c, it.ID, d, doc, page, nil, "publish", nil)
		}
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s (%d): %v", it.Title, it.ID, err))
			return false
		}
		return true
	}
	if mode == "replace" {
		for _, t := range imp.Templates {
			if publish(t) {
				tplIDs = append(tplIDs, t.ID)
			}
		}
	}
	for _, p := range imp.Pages {
		if publish(p) {
			pageIDs = append(pageIDs, p.ID)
		}
	}
	var fin map[string]any
	if err := s.mcpREST(c, http.MethodPost, base+"/finish", map[string]any{"mode": mode, "templates": tplIDs, "pages": pageIDs}, &fin); err != nil {
		problems = append(problems, "finish: "+err.Error())
	}
	out := map[string]any{"mode": mode, "templates": imp.Templates, "pages": imp.Pages, "published_templates": tplIDs, "published_pages": pageIDs, "finish": fin}
	if len(problems) > 0 {
		out["problems"] = problems
	}
	return out, nil
}

// mcpBuilderGuide is the short authoring guide the catalog carries.
const mcpBuilderGuide = `A page is a list of sections. Build one with web_builder_build_page {title | post_id, sections: [spec…]}; a spec is
{kind, design?, name?, content?: {field: text}, items?: [{title, text, icon?, href?}], style?: {palette, align, padding, titleSize}, bg?, grid?, box?, els?}.
Pick kinds and designs from this catalog (web_builder_catalog kind=<kind> lists a kind's fields, extra fields per design and defaults).
Text fields you do not set are left empty (the design's sample text is never published) and a section without items gets none; the answer lists them as warnings, so set them all.
Text fields take plain text or the rich subset <b> <i> <u> <s> <code> <a href> <br> <ul>/<ol>/<li>. Buttons: content.primary / secondary,
links in content.primaryHref / secondaryHref; nav and footer items are menu links (title + href).
Images: an http(s) URL or a Media Library URL (upload_media first, then use its source_url).
palette: brand | dark | light | soft | sand. site: {brand: "#hex", fontPair: elegant | modern | plain, corners: sharp | soft | round, headingFont?, bodyFont?}.
Free layouts: kind layout (cells of heading/text/button/image/layout) or kind custom (a node tree: box/heading/text/button/link/image/icon/input/divider with parent ids),
or pass html: an HTML page becomes custom sections the user edits by drag and drop (inline styles and <style> rules are kept; CDN utility classes are not).
Pages with their own nav + footer sections use layout canvas; pages inside the site's Theme Builder header/footer use layout site (default).
Change pages later with web_builder_edit_page ops (add / update / remove / move / duplicate); the user edits the same page in the editor (edit_url).`

// mcpBuilderTools are the Web Builder tools added to mcpTools.
func mcpBuilderTools() []*mcpTool {
	sectionsProp := map[string]any{"type": "array", "description": "Section specs, top to bottom: {kind, design?, name?, content?, items?, style?, bg?, grid?, box?, els?, hidden?}. See web_builder_catalog.",
		"items": map[string]any{"type": "object"}}
	statusProp := str("publish (default), draft (your editor draft only; the live page does not change), unpublished, private or pending.")
	target := map[string]any{"post_id": num("Existing page/post ID. Omit to create a new page with title."), "title": str("Title of a new page (or the new title)."),
		"post_type": str("page (default) or post, for a new one."), "slug": str("Slug of a new page."), "parent": num("Parent page ID of a new page.")}
	with := func(base map[string]any, extra map[string]any) map[string]any {
		out := map[string]any{}
		for k, v := range base {
			out[k] = v
		}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	build := with(target, map[string]any{
		"sections": sectionsProp, "html": str("Instead of or after sections: an HTML page or fragment to turn into editable free-form sections."),
		"mode": str("replace (default), append or prepend to the page's sections."), "site": obj("Site look: {brand: \"#hex\", fontPair, corners, headingFont, bodyFont}."),
		"page": obj("Page settings: {title, slug, description, indexable, layout}."), "layout": str("site (theme header/footer around the page, default) or canvas (only this page's sections)."),
		"status": statusProp, "front_page": flag("Also make this page the site's front page."),
	})
	ops := map[string]any{"type": "array", "items": map[string]any{"type": "object"},
		"description": "Operations in order. {op: add, section: spec | sections: [spec] | html, before?|after?: section id, index?}; {op: update, section_id, content?, items?, item_updates?: [{id, title?, text?, icon?, href?}], style?, bg?, grid?, box?, els?, name?, hidden?, design?} (objects are merged; null removes a key); {op: remove, section_id}; {op: move, section_id, before?|after?|index?}; {op: duplicate, section_id}."}
	run := func(f mcpRun) mcpRun { return f }
	return []*mcpTool{
		{Name: "web_builder_catalog", Desc: "What Pursae Web Builder can draw: section kinds with fields and ready-made designs, palettes, fonts, corners, icons, plus a short authoring guide. Read it before building pages; pass kind for one kind's full detail (fields, design extras, defaults).",
			Path: "/web-builder/v2/documents/x", Props: map[string]any{"kind": str("One kind, e.g. hero, for its full detail.")}, run: run(mcpWBCatalog), readOnly: true, listAlways: true},
		{Name: "web_builder_get_page", Desc: "Read a Web Builder page: its sections (ids, kinds, designs, text, items), site look and page settings. full=true returns the whole document.",
			Path: "/web-builder/v2/documents/x", Props: map[string]any{"post_id": num("Page/post ID."), "full": flag("The whole document with styles."), "draft": flag("Your newer editor draft instead of the published document.")},
			Required: []string{"post_id"}, run: run(mcpWBGetPage), readOnly: true, listAlways: true},
		{Name: "web_builder_build_page", Desc: "Create or rebuild a Pursae Web Builder page from section specs (and/or HTML) and publish it, rendered on the server. The result is an ordinary builder page the user can keep editing by drag and drop (edit_url). Creates a new page when post_id is omitted.",
			Path: "/web-builder/v2/documents/x", Props: build, run: run(mcpWBBuildPage), listAlways: true},
		{Name: "web_builder_edit_page", Desc: "Change a Web Builder page with section operations (add, update text/items/style/design, remove, move, duplicate) and publish it again (or save your draft).",
			Path: "/web-builder/v2/documents/x", Props: map[string]any{"post_id": num("Page/post ID."), "ops": ops, "site": obj("Site look patch."), "page": obj("Page settings patch."),
				"status": statusProp, "from_draft": flag("Start from your newer editor draft instead of the published page.")}, Required: []string{"post_id"}, run: run(mcpWBEditPage), listAlways: true},
		{Name: "web_builder_import_html", Desc: "Turn an HTML design into an editable Web Builder page. method code (the default for administrators) keeps the design's own HTML, CSS and JavaScript, 1:1 including animations and scripts: each top-level part becomes a code section the user reorders, every element in it is a block the user selects, moves, duplicates, deletes and restyles, and texts, links and images are edited in place on the canvas; upload the design's folder first with web_builder_kit_upload and pass kit + path; templates: true moves the header and footer into site-wide Theme Builder templates (for a site whose other pages, e.g. the forum or shop, share them). method exact rebuilds the design as free-form builder sections (boxes, headings, text, buttons, images) the user can restyle: it measures the design in a real browser so Tailwind, CSS variables, grids and fonts come over as rendered: the answer gives an import_url to open in a browser (yours, or the user's), then call web_builder_import_finish. method quick reads only style attributes and simple <style> rules, without a browser (approximate). Creates a new page when post_id is omitted.",
			Path: "/web-builder/v2/documents/x", Props: with(target, map[string]any{"html": str("The complete HTML document (head with title, meta description and styles included)."), "method": str("code (default for administrators), exact or quick."),
				"kit": str("method code: the design kit (web_builder_kit_upload) holding the design's files."), "path": str("method code: the HTML file's path inside the kit, e.g. index.html or en/index.html (relative links resolve from it)."),
				"link_map":        obj("method code: the design's links to its other pages → site URLs, e.g. {\"forum/index.html\": \"/forum/\", \"en/index.html\": \"/en/\"}."),
				"templates":       flag("method code: put the header and footer into site-wide header/footer templates (Hello Pursae theme)."),
				"header_template": num("method code: update this header template instead of creating one."), "footer_template": num("method code: update this footer template instead of creating one."),
				"site_name":      str("method code: name used in new template titles."),
				"templates_only": flag("method code: only make the header and footer templates from this file (its content is not imported), e.g. a design's forum or shop page whose content the site renders itself."),
				"font_map":       obj("Fonts the builder's library lacks → library fonts, e.g. {\"Mona\": \"Geist\"} (common ones are mapped by default)."),
				"base_url":       str("Where the design's relative files (CSS, scripts, fonts, images) can be loaded from while it is measured, e.g. a local server for a folder. Upload images to the Media Library first if they must stay."), "mode": str("replace (default), append or prepend."),
				"layout": str("canvas (default for a page with its own header/footer) or site."), "status": statusProp, "front_page": flag("Also make it the front page."), "site": obj("Site look patch.")}),
			Required: []string{"html"}, run: run(mcpWBImportHTML), listAlways: true},
		{Name: "web_builder_kit_upload", Desc: "Upload a design's files (CSS, JS, fonts, images, with their folder structure) as a design kit, so pages imported with method code load them from the site. files: [{path: \"assets/style.css\", text: \"…\"} or {path: \"assets/img/a.webp\", data: \"<base64>\"}]. Large folders: send several calls with commit: false, then one with commit: true. Re-uploading replaces changed files (fresh: true starts from an empty kit). Needs an administrator (the files are site-wide code).",
			Path: "/web-builder/v2/kits", Props: map[string]any{"kit": str("Kit slug, e.g. merge."), "name": str("Display name."), "files": map[string]any{"type": "array", "description": "Files: {path, text} or {path, data (base64)}.", "items": map[string]any{"type": "object"}},
				"delete": strList("Paths to remove."), "fresh": flag("Start from an empty kit."), "commit": flag("Publish the staged files (default true).")},
			Required: []string{"kit"}, run: run(mcpWBKitUpload), listAlways: true},
		{Name: "web_builder_import_site", Desc: "Import a whole static HTML site (a folder of pages) as code sections in one call, 1:1 like web_builder_import_html method code: upload the folder's CSS, JS, fonts and images first with web_builder_kit_upload, then pass the kit and every HTML file as pages [{path, html}]. When published with homepage selection enabled, index.html becomes the front page. a/b.html becomes the page /a/b/ (under the page of a.html or a/index.html, else a draft parent), 404.html the 404 template; links between the pages are mapped to their URLs; the pages' headers and footers become as few Theme Builder templates as reproduce them, and the header marks the current page's menu link as the design does. Running it again updates the same pages and templates. Needs an administrator and the Hello Pursae theme.",
			Path: "/web-builder/v2/documents/x", Props: map[string]any{"kit": str("The design kit holding the site's files (web_builder_kit_upload)."),
				"pages":      map[string]any{"type": "array", "description": "Every HTML file of the folder: {path (relative, e.g. index.html, hizmet/3.html), html (the whole document)}.", "items": map[string]any{"type": "object"}},
				"status":     statusProp,
				"front_page": flag("Make index.html the front page when publishing (default true)."),
				"templates":  flag("Split each page's header and footer into Theme Builder templates (default true); false keeps them in the pages."),
				"menus":      flag("Make a Pursae menu from the header's navigation (primary location) and bind the design's list to it, so items are managed in Appearance › Menus with the design's markup (default true; only when the bound list reproduces the design)."),
				"collections": map[string]any{"description": "Folders whose pages share one skeleton (hizmet/*.html) to import as page content under one Theme Builder single template each (URLs unchanged), and lists of those pages elsewhere bound as loops: true for every folder that qualifies, or a list of folder names. Default: none. The answer's bindings.collection_candidates lists the folders.",
					"oneOf": []any{map[string]any{"type": "boolean"}, map[string]any{"type": "array", "items": map[string]any{"type": "string"}}}},
				"site_name": str("Name used in new template titles."),
				"old_domains": map[string]any{"type": "array", "items": map[string]any{"type": "string"},
					"description": "The domain(s) the design was saved from (a copy of a live site). Default: detected from the pages' canonical/og:url links, else the host most pages link to; [] turns it off. Their URLs are taken as this site's paths: forms and the URLs in inline scripts (AJAX endpoints) always point at this site, so nothing posts to the original site; the answer's original_site counts them."},
				"old_links": str("rewrite (default): links and files of old_domains become this site's paths (imported pages, kit files); keep: they keep pointing at the original site (forms and script endpoints still point here)."),
				"media":     obj("Files of the design kept outside the kit (too large for it, e.g. a video put into the Media Library): {\"<path in the folder>\": \"<URL>\"}; references to them point at the URL.")},
			Required: []string{"kit", "pages"}, run: run(mcpWBImportSite), listAlways: true},
		{Name: "web_builder_bind", Desc: "Bind an element of an imported design (a code section) to content, keeping the design's markup: kind menu (a list whose items are templates, filled from a menu location or menu id), submenu (a dropdown list inside a menu item), loop (a container whose first child is the card template, repeated per post of query {postType, perPage, orderBy, order, terms, parent, offset}), field (an element filled from the post of the nearest loop card or the template's post: field title|excerpt|content|image|url|link|date|modified|author|terms|custom|pagination); field custom (name) prints a Pursae Custom Fields field a field group defines, or the design_<n> / list<n>_<k> fields a site import stores, never other post meta: the result has a warning when the name is neither or form (a <form> posting to the Pursae form endpoint: fields maps input names to name|email|phone|message|subject|company|… or field_<name>, done/error name the design's message elements by data-wbc). bind null removes the binding. Publishes the page.",
			Path: "/web-builder/v2/documents/x", Props: map[string]any{"post_id": num("Page, post or template ID."), "element": str("The element's data-wbc id."),
				"html_id": str("Or the element's id attribute (e.g. contact-form)."), "bind": obj("The binding: {kind, …} (see the description), or null to remove it.")},
			Required: []string{"post_id"}, run: run(mcpWBBind), listAlways: true},
		{Name: "web_builder_list_kits", Desc: "List the design kits (uploaded design files for code sections) with their base URL, version and size.",
			Method: http.MethodGet, Path: "/web-builder/v2/kits"},
		{Name: "web_builder_import_finish", Desc: "Finish an exact HTML import after its import_url was opened in a browser: converts the measured design into Web Builder sections, maps title, meta description, robots and Open Graph tags to the page and Pursae SEO, publishes, and lists SEO and fidelity notes. Waits up to `wait` seconds (default 45) for the browser.",
			Path: "/web-builder/v2/documents/x", Props: map[string]any{"job": str("The job from web_builder_import_html."), "wait": num("Seconds to wait for the browser (max 110)."), "status": str("Override the status given at import."),
				"font_map": obj("Font replacements, as in web_builder_import_html.")},
			Required: []string{"job"}, run: run(mcpWBImportFinish), listAlways: true},
		{Name: "web_builder_publish", Desc: "Publish a Web Builder page as it stands (your newer editor draft if there is one), rendered on the server. Use after saving with status=draft, or to publish an imported page.",
			Path: "/web-builder/v2/documents/x", Props: map[string]any{"post_id": num("Page/post ID."), "status": str("publish (default), private, pending or draft.")}, Required: []string{"post_id"}, run: run(mcpWBPublish), listAlways: true},
		{Name: "web_builder_list_templates", Desc: "List Theme Builder templates (header, footer, single, archive, search, 404, front page, …) with status and display conditions.",
			Method: http.MethodGet, Path: "/web-builder/v2/templates", Props: map[string]any{"type": str("Filter by template type.")}},
		{Name: "web_builder_create_template", Desc: "Create a Theme Builder template (a draft): type header, footer, single, page, archive, search, 404, front, popup, …; then fill it with web_builder_build_page {post_id: <its id>, sections: […]} and it goes live. A header usually holds one nav section, a footer one footer section.",
			Method: http.MethodPost, Path: "/web-builder/v2/templates", Props: map[string]any{"type": str("Template type."), "title": str("Title (default: the type's label)."), "starter": str("Starter design (optional)."), "postType": str("Custom post type (single only).")}, Required: []string{"type"}},
		{Name: "web_builder_set_template_conditions", Desc: "Set where a Theme Builder template applies. conditions are rule strings \"<include|exclude>/<scope>\": include/general (every view), include/singular/page, include/singular/post, include/archive, include/archive/category/12, include/singular/front_page, exclude/singular/page/42, include/404 … (most specific wins; priority −100…100 breaks ties). New header and footer templates already apply to the whole site.",
			Method: http.MethodPut, Path: "/web-builder/v2/templates/{id}/conditions", Props: map[string]any{"id": num("Template ID."), "conditions": strList("Rules, e.g. [\"include/general\"]."), "priority": num("Priority (default 0).")}, Required: []string{"id", "conditions"}},
		{Name: "web_builder_list_theme_kits", Desc: "List the ready-made full-site themes of the theme library (header, footer, page templates, demo pages, menus).",
			Method: http.MethodGet, Path: "/web-builder/v2/themes"},
		{Name: "web_builder_import_theme_kit", Desc: "Import a full-site theme kit from the theme library and publish it: Theme Builder templates, demo pages and menus. mode replace (default) makes its templates live; keep saves them as drafts.",
			Path: "/web-builder/v2/themes/x/import", Props: map[string]any{"slug": str("Theme kit slug (web_builder_list_theme_kits)."), "mode": str("replace or keep."), "pages": flag("Also import the demo pages (default true).")},
			Required: []string{"slug"}, run: run(mcpWBImportThemeKit), listAlways: true},
	}
}
