package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"wpgo/docs"
	"wpgo/internal/webbuilder"
	"wpgo/skills"
)

// MCP (Model Context Protocol) server: AI tools such as Claude and Cursor manage the site through it.
// Transport is streamable HTTP at /wp-json/pursae/v1/mcp (POST one JSON-RPC message, get one JSON
// answer); `wpgo mcp` (cmd/wpgo) bridges stdio clients to it. Authentication is the REST API's, normally
// an Application Password, and every tool runs as a REST request with the same credentials, so
// capability checks and Application Password scopes apply exactly as for direct REST calls.
// Resources are the developer docs (docs/developer), so an agent can read how themes and plugins work.

const mcpServerName = "pursae"

var mcpProtocolVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

type mcpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type mcpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type mcpResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *mcpError       `json:"error,omitempty"`
}

// mcpHTTP is the streamable HTTP endpoint (sessions and SSE: mcp_session.go). POST carries one JSON-RPC
// message, GET opens the server→client stream of a session, DELETE ends the session.
func (s *Server) mcpHTTP(c *gin.Context, r *restRequest) {
	u := restCurrent(c)
	if u == nil {
		c.Writer.Header().Add("WWW-Authenticate", s.oauthChallenge(c, "")) // OAuth 2.1: where to find the authorization server
		c.Writer.Header().Add("WWW-Authenticate", `Basic realm="Pursae"`)
		restErr(c, http.StatusUnauthorized, "rest_not_logged_in", "Sign in with OAuth or an Application Password (HTTP Basic auth).")
		return
	}
	var sess *mcpSession
	if id := c.GetHeader(mcpSessionHeader); id != "" {
		if sess = mcpSess.get(id, u.ID); sess == nil {
			restErr(c, http.StatusNotFound, "mcp_session_not_found", "The MCP session has ended. Initialize again.")
			return
		}
	}
	switch c.Request.Method {
	case http.MethodDelete:
		if sess == nil {
			restErr(c, http.StatusBadRequest, "mcp_session_required", "Send the Mcp-Session-Id header.")
			return
		}
		mcpSess.end(sess.id, u.ID)
		c.Status(http.StatusNoContent)
		return
	case http.MethodGet:
		if sess == nil || !strings.Contains(c.GetHeader("Accept"), "text/event-stream") {
			c.Header("Allow", "GET, POST, DELETE")
			c.AbortWithStatus(http.StatusMethodNotAllowed)
			return
		}
		s.mcpStream(c, sess)
		return
	}
	raw, _ := json.Marshal(r.Body)
	var req mcpRequest
	if err := json.Unmarshal(raw, &req); err != nil || req.Method == "" {
		c.JSON(http.StatusBadRequest, mcpResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &mcpError{-32600, "Invalid Request"}})
		return
	}
	resp := s.mcpHandle(c, &req)
	if resp == nil { // a notification
		c.Status(http.StatusAccepted)
		return
	}
	if req.Method == "initialize" && resp.Error == nil && sess == nil {
		c.Header(mcpSessionHeader, mcpSess.create(u.ID).id)
	}
	if wantsSSE(c.GetHeader("Accept")) {
		out, _ := json.Marshal(resp)
		sseHeaders(c)
		writeSSE(c, "message", out)
		return
	}
	c.JSON(http.StatusOK, resp)
}

// mcpHandle answers one JSON-RPC message (nil for notifications).
func (s *Server) mcpHandle(c *gin.Context, req *mcpRequest) *mcpResponse {
	if len(req.ID) == 0 || string(req.ID) == "null" {
		return nil // notifications/initialized, notifications/cancelled, …
	}
	resp := &mcpResponse{JSONRPC: "2.0", ID: req.ID}
	fail := func(code int, msg string) *mcpResponse {
		resp.Error = &mcpError{code, msg}
		return resp
	}
	var p map[string]any
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return fail(-32602, "Invalid params")
		}
	}
	switch req.Method {
	case "initialize":
		v, _ := p["protocolVersion"].(string)
		if !containsStr(mcpProtocolVersions, v) {
			v = mcpProtocolVersions[0]
		}
		resp.Result = map[string]any{
			"protocolVersion": v,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": true}, "resources": map[string]any{}, "prompts": map[string]any{}},
			"serverInfo":      map[string]any{"name": mcpServerName, "title": "Pursae", "version": Version},
			"instructions":    mcpInstructions,
		}
	case "ping":
		resp.Result = map[string]any{}
	case "tools/list":
		var tools []map[string]any
		for _, t := range mcpTools {
			if !s.mcpToolAvailable(c, t) {
				continue
			}
			tools = append(tools, map[string]any{"name": t.Name, "description": t.Desc, "inputSchema": t.schema(), "annotations": t.annotations()})
		}
		resp.Result = map[string]any{"tools": tools}
	case "tools/call":
		name, _ := p["name"].(string)
		args, _ := p["arguments"].(map[string]any)
		if args == nil {
			args = map[string]any{}
		}
		t := mcpToolByName(name)
		// The same filter as tools/list: a credential that cannot see a tool cannot call it.
		if t == nil || (c != nil && !s.mcpToolAvailable(c, t)) {
			return fail(-32602, "Unknown tool: "+name)
		}
		resp.Result = s.mcpCall(c, t, args)
	case "resources/list":
		var list []map[string]any
		for _, d := range mcpDocs() {
			list = append(list, map[string]any{"uri": d.uri, "name": d.name, "title": d.title, "mimeType": "text/markdown"})
		}
		resp.Result = map[string]any{"resources": list}
	case "resources/templates/list":
		resp.Result = map[string]any{"resourceTemplates": []any{}}
	case "resources/read":
		uri, _ := p["uri"].(string)
		b, ok := mcpReadDoc(uri)
		if !ok {
			return fail(-32002, "Resource not found: "+uri)
		}
		resp.Result = map[string]any{"contents": []map[string]any{{"uri": uri, "mimeType": "text/markdown", "text": string(b)}}}
	case "prompts/list":
		resp.Result = map[string]any{"prompts": mcpPromptsList()}
	case "prompts/get":
		name, _ := p["name"].(string)
		args, _ := p["arguments"].(map[string]any)
		out, msg := mcpPromptGet(name, args)
		if msg != "" {
			return fail(-32602, msg)
		}
		resp.Result = out
	default:
		return fail(-32601, "Method not found: "+req.Method)
	}
	return resp
}

// --- tools -----------------------------------------------------------------------------------

// mcpTool maps a tool to a REST route. Path holds {arg} placeholders filled from the arguments; the
// other arguments become the JSON body (POST/PUT) or the query string (GET/DELETE).
type mcpTool struct {
	Name, Desc   string
	Method, Path string
	Props        map[string]any
	Required     []string
	Open         bool // accepts more fields than Props lists (REST fields of create/update)
	special      string
	run          mcpRun // a tool implemented in Go (mcp_builder.go, mcp_site.go) instead of one REST call
	readOnly     bool   // a run tool that changes nothing
	listAlways   bool   // listed while its built-in plugin is off: it answers how to turn the plugin on
}

func (t *mcpTool) schema() map[string]any {
	props := map[string]any{}
	for k, v := range t.Props {
		props[k] = v
	}
	sch := map[string]any{"type": "object", "properties": props}
	if len(t.Required) > 0 {
		sch["required"] = t.Required
	}
	if !t.Open {
		sch["additionalProperties"] = false
	}
	return sch
}

// mcpAdditiveTools only add something new; every other tool that writes may change or remove existing
// data (an update overwrites, an install replaces a version, rest_request can do anything).
var mcpAdditiveTools = map[string]bool{"create_user": true, "create_comment": true, "commerce_create_product": true, "create_backup": true, "create_post": true, "create_page": true, "create_term": true, "create_menu_item": true, "upload_media": true}

// annotations are the MCP tool hints clients use to decide what needs the user's approval. Per the spec
// destructiveHint means "may change or delete existing data", so it is true for every write that is not
// purely additive.
func (t *mcpTool) annotations() map[string]any {
	readOnly := t.Method == http.MethodGet || t.Name == "seo_analyze" || t.readOnly // seo_analyze POSTs unsaved input but stores nothing
	return map[string]any{"readOnlyHint": readOnly, "destructiveHint": !readOnly && !mcpAdditiveTools[t.Name]}
}

func str(desc string) map[string]any  { return map[string]any{"type": "string", "description": desc} }
func num(desc string) map[string]any  { return map[string]any{"type": "integer", "description": desc} }
func flag(desc string) map[string]any { return map[string]any{"type": "boolean", "description": desc} }
func obj(desc string) map[string]any  { return map[string]any{"type": "object", "description": desc} }
func strList(desc string) map[string]any {
	return map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": desc}
}

func listProps(extra map[string]any) map[string]any {
	p := map[string]any{"search": str("Search text."), "per_page": num("Items per page (1-100, default 10)."), "page": num("Page number."),
		"context": str("view or edit (edit returns raw content).")}
	for k, v := range extra {
		p[k] = v
	}
	return p
}

var postFields = map[string]any{"title": str("Title."), "content": str("Content: block markup (<!-- wp:paragraph -->…) or HTML."),
	"excerpt": str("Excerpt."), "status": str("publish, draft, pending, private or future."), "slug": str("URL slug."),
	"date": str("Publish date (ISO 8601, site time)."), "featured_media": num("Attachment ID of the featured image."),
	"categories": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "Category IDs (posts)."},
	"tags":       map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "Tag IDs (posts)."},
	"parent":     num("Parent page ID (pages)."), "template": str("Page template."), "meta": obj("Registered meta fields.")}

func withID(m map[string]any) map[string]any {
	out := map[string]any{"id": num("Item ID.")}
	for k, v := range m {
		out[k] = v
	}
	return out
}

var mcpTools = func() []*mcpTool {
	var list []*mcpTool
	add := func(t *mcpTool) { list = append(list, t) }
	for _, typ := range []struct{ one, many, path string }{{"post", "posts", "/wp/v2/posts"}, {"page", "pages", "/wp/v2/pages"}} {
		add(&mcpTool{Name: "list_" + typ.many, Desc: "List " + typ.many + " (wp/v2). Filters: status, search, per_page, page, orderby, order, categories, tags, author.",
			Method: http.MethodGet, Path: typ.path, Props: listProps(map[string]any{"status": str("Status filter, e.g. publish,draft (needs edit rights for non-public).")}), Open: true})
		add(&mcpTool{Name: "get_" + typ.one, Desc: "Get one " + typ.one + " by ID. Use context=edit for the raw block markup.",
			Method: http.MethodGet, Path: typ.path + "/{id}", Props: map[string]any{"id": num(typ.one + " ID."), "context": str("view or edit.")}, Required: []string{"id"}})
		add(&mcpTool{Name: "create_" + typ.one, Desc: "Create a " + typ.one + ". Content is block markup or HTML; status defaults to draft.",
			Method: http.MethodPost, Path: typ.path, Props: postFields, Open: true})
		add(&mcpTool{Name: "update_" + typ.one, Desc: "Update a " + typ.one + " (only the fields given change).",
			Method: http.MethodPost, Path: typ.path + "/{id}", Props: withID(postFields), Required: []string{"id"}, Open: true})
		add(&mcpTool{Name: "delete_" + typ.one, Desc: "Move a " + typ.one + " to the trash, or delete it permanently with force=true.",
			Method: http.MethodDelete, Path: typ.path + "/{id}", Props: map[string]any{"id": num(typ.one + " ID."), "force": flag("Skip the trash.")}, Required: []string{"id"}})
	}
	add(&mcpTool{Name: "list_media", Desc: "List Media Library items.", Method: http.MethodGet, Path: "/wp/v2/media",
		Props: listProps(map[string]any{"media_type": str("image, video, audio, application.")}), Open: true})
	add(&mcpTool{Name: "upload_media", Desc: "Upload a file to the Media Library from base64 data, or from a public URL.",
		Method: http.MethodPost, Path: "/wp/v2/media", special: "upload",
		Props: map[string]any{"filename": str("File name with extension, e.g. hero.jpg."), "data_base64": str("File contents, base64."),
			"source_url": str("Instead of data_base64: an http(s) URL to fetch."), "title": str("Title."), "alt_text": str("Alternative text."),
			"caption": str("Caption."), "description": str("Description."), "post": num("Attach to this post ID.")}, Required: []string{"filename"}})
	add(&mcpTool{Name: "update_media", Desc: "Update a media item's title, alt_text, caption, description or focal point.", Method: http.MethodPost, Path: "/wp/v2/media/{id}",
		Props: withID(map[string]any{"title": str("Title."), "alt_text": str("Alt text."), "caption": str("Caption."), "description": str("Description."),
			"focal_point": map[string]any{"type": []string{"object", "null"}, "description": "Images only: the point cropped sizes and cover images keep in view, {x, y} from 0 to 1 (0,0 = top left); null resets it to the centre.",
				"properties": map[string]any{"x": map[string]any{"type": "number", "minimum": 0, "maximum": 1}, "y": map[string]any{"type": "number", "minimum": 0, "maximum": 1}}}}), Required: []string{"id"}})
	add(&mcpTool{Name: "replace_media_file", Desc: "Replace the file of a media item with a new one of the same kind, from base64 data or a public URL. The item keeps its ID, so every post, page and featured image using it shows the new file; a file of the same type also keeps its URL.",
		Method: http.MethodPost, Path: "/pursae/v1/media/{id}/replace", special: "upload",
		Props: map[string]any{"id": num("Attachment ID."), "filename": str("File name with extension, e.g. logo.png."), "data_base64": str("File contents, base64."),
			"source_url": str("Instead of data_base64: an http(s) URL to fetch.")}, Required: []string{"id", "filename"}})
	add(&mcpTool{Name: "delete_media", Desc: "Delete a media item permanently.", Method: http.MethodDelete, Path: "/wp/v2/media/{id}",
		Props: map[string]any{"id": num("Attachment ID."), "force": flag("Must be true for media.")}, Required: []string{"id"}})

	tax := map[string]any{"taxonomy": str("categories or tags (or another taxonomy's REST base).")}
	add(&mcpTool{Name: "list_terms", Desc: "List terms of a taxonomy.", Method: http.MethodGet, Path: "/wp/v2/{taxonomy}",
		Props: listProps(tax), Required: []string{"taxonomy"}, Open: true})
	termFields := map[string]any{"taxonomy": tax["taxonomy"], "name": str("Name."), "slug": str("Slug."), "description": str("Description."), "parent": num("Parent term ID.")}
	add(&mcpTool{Name: "create_term", Desc: "Create a category, tag or other term.", Method: http.MethodPost, Path: "/wp/v2/{taxonomy}",
		Props: termFields, Required: []string{"taxonomy", "name"}})
	add(&mcpTool{Name: "update_term", Desc: "Update a term.", Method: http.MethodPost, Path: "/wp/v2/{taxonomy}/{id}", Props: withID(termFields), Required: []string{"taxonomy", "id"}})
	add(&mcpTool{Name: "delete_term", Desc: "Delete a term.", Method: http.MethodDelete, Path: "/wp/v2/{taxonomy}/{id}",
		Props: map[string]any{"taxonomy": tax["taxonomy"], "id": num("Term ID."), "force": flag("Must be true: terms have no trash.")}, Required: []string{"taxonomy", "id", "force"}})

	add(&mcpTool{Name: "list_menus", Desc: "List navigation menus.", Method: http.MethodGet, Path: "/wp/v2/menus", Props: listProps(nil), Open: true})
	add(&mcpTool{Name: "create_menu", Desc: "Create a navigation menu; locations assigns theme menu locations.", Method: http.MethodPost, Path: "/wp/v2/menus",
		Props: map[string]any{"name": str("Menu name."), "locations": strList("Theme locations, e.g. primary.")}, Required: []string{"name"}, Open: true})
	add(&mcpTool{Name: "list_menu_locations", Desc: "List the active theme's menu locations.", Method: http.MethodGet, Path: "/wp/v2/menu-locations"})
	add(&mcpTool{Name: "list_menu_items", Desc: "List menu items (filter with menus=<menu id>).", Method: http.MethodGet, Path: "/wp/v2/menu-items",
		Props: listProps(map[string]any{"menus": num("Menu ID.")}), Open: true})
	itemFields := map[string]any{"menus": num("Menu ID."), "title": str("Label."), "type": str("custom, post_type or taxonomy."),
		"object": str("post, page, category… (type post_type/taxonomy)."), "object_id": num("Linked object ID."), "url": str("URL (type custom)."),
		"parent": num("Parent menu item ID."), "menu_order": num("Position (1-based)."), "status": str("publish (default).")}
	add(&mcpTool{Name: "create_menu_item", Desc: "Add an item to a menu.", Method: http.MethodPost, Path: "/wp/v2/menu-items", Props: itemFields, Required: []string{"menus"}, Open: true})
	add(&mcpTool{Name: "update_menu_item", Desc: "Update a menu item.", Method: http.MethodPost, Path: "/wp/v2/menu-items/{id}", Props: withID(itemFields), Required: []string{"id"}, Open: true})
	add(&mcpTool{Name: "delete_menu_item", Desc: "Delete a menu item.", Method: http.MethodDelete, Path: "/wp/v2/menu-items/{id}",
		Props: map[string]any{"id": num("Menu item ID."), "force": flag("Must be true.")}, Required: []string{"id"}})

	add(&mcpTool{Name: "get_global_styles", Desc: "Get the active theme's global styles (theme.json styles and settings). Read only: the core has no user global-styles store to write to.",
		Method: http.MethodGet, Path: "/wp/v2/global-styles/themes/{stylesheet}", Props: map[string]any{"stylesheet": str("Theme slug (see list_themes).")}, Required: []string{"stylesheet"}})

	add(&mcpTool{Name: "get_settings", Desc: "Get the site settings (title, tagline, front page, permalinks, …).", Method: http.MethodGet, Path: "/wp/v2/settings"})
	add(&mcpTool{Name: "update_settings", Desc: "Update site settings, e.g. {\"title\": \"…\", \"show_on_front\": \"page\", \"page_on_front\": 12}.",
		Method: http.MethodPost, Path: "/wp/v2/settings", Open: true})
	add(&mcpTool{Name: "list_users", Desc: "List users.", Method: http.MethodGet, Path: "/wp/v2/users", Props: listProps(map[string]any{"roles": str("Role filter.")}), Open: true})
	add(&mcpTool{Name: "get_me", Desc: "The signed-in user (who the Application Password belongs to).", Method: http.MethodGet, Path: "/wp/v2/users/me",
		Props: map[string]any{"context": str("view or edit.")}})

	add(&mcpTool{Name: "seo_get_meta", Desc: "Get a post's Pursae SEO fields (title, description, focus keyword, robots, canonical, social…).",
		Method: http.MethodGet, Path: "/seogo/v1/meta/{id}", Props: map[string]any{"id": num("Post ID.")}, Required: []string{"id"}})
	add(&mcpTool{Name: "seo_update_meta", Desc: "Update a post's Pursae SEO fields; pass them under fields, e.g. {\"fields\": {\"title\": \"…\", \"description\": \"…\", \"focus\": \"…\"}}.",
		Method: http.MethodPost, Path: "/seogo/v1/meta/{id}", Props: map[string]any{"id": num("Post ID."), "fields": obj("SEO fields to set.")}, Required: []string{"id"}, Open: true})
	add(&mcpTool{Name: "seo_analyze", Desc: "Run the Pursae SEO and readability analysis for a post, optionally with unsaved title/content/fields.",
		Method: http.MethodPost, Path: "/seogo/v1/analyze", Props: map[string]any{"post_id": num("Post ID."), "title": str("Title."), "content": str("Content."),
			"slug": str("Slug."), "fields": obj("SEO fields (focus, title, description…).")}})
	add(&mcpTool{Name: "seo_get_settings", Desc: "Get Pursae SEO's site-wide settings (modules, title templates per type, sitemap, robots, schema defaults…).",
		Method: http.MethodGet, Path: "/pursae/v1/seo/settings"})
	add(&mcpTool{Name: "seo_update_settings", Desc: "Merge changes into Pursae SEO's site-wide settings; send only the keys to change (same shape as seo_get_settings).",
		Method: http.MethodPost, Path: "/pursae/v1/seo/settings", Props: map[string]any{"settings": obj("Partial settings object.")}, Required: []string{"settings"}})
	add(&mcpTool{Name: "seo_list_redirects", Desc: "List Pursae SEO redirects.", Method: http.MethodGet, Path: "/pursae/v1/seo/redirects"})
	add(&mcpTool{Name: "seo_create_redirect", Desc: "Create (or, with id, update) a redirect.", Method: http.MethodPost, Path: "/pursae/v1/seo/redirects",
		Props: map[string]any{"id": num("Existing redirect ID to update."), "source": str("Source path, e.g. /old-page."), "target": str("Target URL or path."),
			"code": num("301 (default), 302, 307, 410 or 451."), "match_type": str("exact (default), contains, start, end or regex."),
			"status": str("active or inactive."), "category": str("Category label.")}, Required: []string{"source"}})
	add(&mcpTool{Name: "seo_delete_redirect", Desc: "Delete a redirect.", Method: http.MethodDelete, Path: "/pursae/v1/seo/redirects/{id}",
		Props: map[string]any{"id": num("Redirect ID.")}, Required: []string{"id"}})

	slug := map[string]any{"slug": str("Plugin slug (see list_plugins).")}
	add(&mcpTool{Name: "list_plugins", Desc: "List plugins: built-in (native) ones and installed Wasm packages, with active state.", Method: http.MethodGet, Path: "/pursae/v1/plugins"})
	add(&mcpTool{Name: "activate_plugin", Desc: "Activate a plugin. A Wasm plugin gets the permissions in permissions (default: all its manifest asks for except the dangerous content:html and admin:scripts, which give it the equivalent of an administrator's session and are granted only when listed; ask the user first).",
		Method: http.MethodPost, Path: "/pursae/v1/plugins/{slug}/activate", Props: map[string]any{"slug": slug["slug"], "permissions": strList("Wasm permissions to grant (replaces the default; list content:html or admin:scripts only with the user's consent).")}, Required: []string{"slug"}})
	add(&mcpTool{Name: "deactivate_plugin", Desc: "Deactivate a plugin.", Method: http.MethodPost, Path: "/pursae/v1/plugins/{slug}/deactivate", Props: slug, Required: []string{"slug"}})
	add(&mcpTool{Name: "install_plugin", Desc: "Install a signed Wasm plugin package (zip, base64). It is installed inactive.", Method: http.MethodPost, Path: "/pursae/v1/plugins",
		Props: map[string]any{"package": str("The package zip, base64.")}, Required: []string{"package"}})
	add(&mcpTool{Name: "delete_plugin", Desc: "Delete an installed (non built-in) plugin.", Method: http.MethodDelete, Path: "/pursae/v1/plugins/{slug}", Props: slug, Required: []string{"slug"}})

	theme := str("Theme slug (see list_themes).")
	file := str("File path as list_theme_files names it: style.css, assets paths without the assets/ prefix, Go templates as templates/….")
	add(&mcpTool{Name: "list_themes", Desc: "List installed themes and which one is active.", Method: http.MethodGet, Path: "/pursae/v1/themes"})
	add(&mcpTool{Name: "activate_theme", Desc: "Switch the site to a theme.", Method: http.MethodPost, Path: "/pursae/v1/themes/{slug}/activate",
		Props: map[string]any{"slug": theme}, Required: []string{"slug"}})
	add(&mcpTool{Name: "install_theme", Desc: "Install a theme package (zip with theme.json, base64; see the theme-package docs).", Method: http.MethodPost, Path: "/pursae/v1/themes",
		Props: map[string]any{"package": str("The theme zip, base64."), "activate": flag("Activate after installing.")}, Required: []string{"package"}})
	add(&mcpTool{Name: "list_theme_files", Desc: "List a theme's editable files (CSS, JS, JSON, SVG, Go templates).", Method: http.MethodGet, Path: "/pursae/v1/themes/{slug}/files",
		Props: map[string]any{"slug": theme}, Required: []string{"slug"}})
	add(&mcpTool{Name: "read_theme_file", Desc: "Read a theme file.", Method: http.MethodGet, Path: "/pursae/v1/themes/{slug}/file",
		Props: map[string]any{"slug": theme, "file": file}, Required: []string{"slug", "file"}})
	add(&mcpTool{Name: "write_theme_file", Desc: "Save a theme file. Template changes are parsed and render-checked; a change that breaks the site is rolled back and reported.",
		Method: http.MethodPost, Path: "/pursae/v1/themes/{slug}/file", Props: map[string]any{"slug": theme, "file": file, "content": str("New file contents.")}, Required: []string{"slug", "file", "content"}})
	add(&mcpTool{Name: "restore_theme_file", Desc: "Undo edits to a theme file (back to the theme's original).", Method: http.MethodDelete, Path: "/pursae/v1/themes/{slug}/file",
		Props: map[string]any{"slug": theme, "file": file}, Required: []string{"slug", "file"}})

	add(&mcpTool{Name: "delete_theme", Desc: "Delete an installed theme (not the active one).", Method: http.MethodDelete, Path: "/pursae/v1/themes/{slug}", Props: map[string]any{"slug": theme}, Required: []string{"slug"}})

	add(&mcpTool{Name: "site_health", Desc: "Health summary: version, database, cron queue and its last error, Site Health score, plugins, cache, updates waiting and the last backup. Run it after an update to see whether something broke.",
		Method: http.MethodGet, Path: "/pursae/v1/health"})
	add(&mcpTool{Name: "list_updates", Desc: "List the core, plugin and theme updates Pursae Manager offers.", Method: http.MethodGet, Path: "/pursae/v1/updates"})
	add(&mcpTool{Name: "run_update", Desc: "Install an update: type plugin or theme with its slug, or type core (replaces the binary; the site restarts a second after the answer). Take a backup first.",
		Method: http.MethodPost, Path: "/pursae/v1/updates", Props: map[string]any{"type": str("plugin, theme or core."), "slug": str("Plugin or theme slug (not for core).")}, Required: []string{"type"}})
	add(&mcpTool{Name: "list_backups", Desc: "List stored backups, or with job=<id> the state of a running backup.", Method: http.MethodGet, Path: "/pursae/v1/backups",
		Props: map[string]any{"job": str("Job id returned by create_backup.")}})
	add(&mcpTool{Name: "create_backup", Desc: "Start a backup in the background (all parts, or the categories given: content, media, users, settings, themes, plugins, …). Returns a job id for list_backups.",
		Method: http.MethodPost, Path: "/pursae/v1/backups", Props: map[string]any{"categories": strList("Parts to back up (default: all)."), "note": str("Note stored with the archive.")}})
	add(&mcpTool{Name: "ai_activity", Desc: "The audit log of tool calls that wrote: when, which client (OAuth application or Application Password), tool, route and status.",
		Method: http.MethodGet, Path: "/pursae/v1/mcp/activity", Props: map[string]any{"tool": str("Only this tool."), "client": str("Part of the client name."), "per_page": num("1-100, default 25."), "page": num("Page number.")}})

	add(&mcpTool{Name: "list_comments", Desc: "List comments. Filters: status (approve, hold, spam, trash, all), post, search, per_page, page.", Method: http.MethodGet, Path: "/wp/v2/comments",
		Props: listProps(map[string]any{"status": str("approve, hold, spam, trash or all."), "post": num("Post ID.")}), Open: true})
	add(&mcpTool{Name: "create_comment", Desc: "Add a comment (or a reply with parent) to a post.", Method: http.MethodPost, Path: "/wp/v2/comments",
		Props:    map[string]any{"post": num("Post ID."), "content": str("Comment text."), "parent": num("Parent comment ID."), "author_name": str("Author name."), "author_email": str("Author email.")},
		Required: []string{"post", "content"}})
	add(&mcpTool{Name: "moderate_comment", Desc: "Change a comment's status (approved, hold, spam, trash) or edit its content.", Method: http.MethodPost, Path: "/wp/v2/comments/{id}",
		Props: map[string]any{"id": num("Comment ID."), "status": str("approved, hold, spam or trash."), "content": str("New text.")}, Required: []string{"id"}})
	add(&mcpTool{Name: "delete_comment", Desc: "Move a comment to the trash, or delete it permanently with force=true.", Method: http.MethodDelete, Path: "/wp/v2/comments/{id}",
		Props: map[string]any{"id": num("Comment ID."), "force": flag("Skip the trash.")}, Required: []string{"id"}})

	userFields := map[string]any{"username": str("Login name (create only)."), "email": str("Email address."), "password": str("Password."), "name": str("Display name."),
		"first_name": str("First name."), "last_name": str("Last name."), "roles": strList("Roles, e.g. editor."), "description": str("Biography.")}
	add(&mcpTool{Name: "create_user", Desc: "Create a user.", Method: http.MethodPost, Path: "/wp/v2/users", Props: userFields, Required: []string{"username", "email", "password"}})
	add(&mcpTool{Name: "update_user", Desc: "Update a user (only the fields given change).", Method: http.MethodPost, Path: "/wp/v2/users/{id}", Props: withID(userFields), Required: []string{"id"}})
	add(&mcpTool{Name: "delete_user", Desc: "Delete a user permanently (force=true); reassign gives their content to another user ID.", Method: http.MethodDelete, Path: "/wp/v2/users/{id}",
		Props: map[string]any{"id": num("User ID."), "force": flag("Must be true."), "reassign": num("User ID that receives the content.")}, Required: []string{"id", "force"}})

	prodFields := map[string]any{"name": str("Product name."), "type": str("simple, variable, grouped or external."), "status": str("publish, draft, pending or private."),
		"regular_price": str("Regular price, e.g. 19.90."), "sale_price": str("Sale price."), "description": str("Long description (HTML)."), "short_description": str("Short description."),
		"sku": str("SKU."), "manage_stock": flag("Track stock."), "stock_quantity": num("Stock quantity."), "stock_status": str("instock, outofstock or onbackorder."),
		"categories": map[string]any{"type": "array", "items": map[string]any{"type": "object"}, "description": "Categories as [{\"id\": 12}]."},
		"images":     map[string]any{"type": "array", "items": map[string]any{"type": "object"}, "description": "Images as [{\"id\": media ID}] or [{\"src\": url}]."}}
	add(&mcpTool{Name: "commerce_list_products", Desc: "List Pursae Commerce products (wc/v3). Filters: search, status, sku, category, stock_status, type, per_page, page.", Method: http.MethodGet, Path: "/wc/v3/products",
		Props: listProps(map[string]any{"status": str("any, publish, draft, …"), "sku": str("Exact SKU."), "category": str("Category ID."), "stock_status": str("instock, outofstock, onbackorder."), "type": str("Product type.")}), Open: true})
	add(&mcpTool{Name: "commerce_get_product", Desc: "Get one product.", Method: http.MethodGet, Path: "/wc/v3/products/{id}", Props: map[string]any{"id": num("Product ID.")}, Required: []string{"id"}})
	add(&mcpTool{Name: "commerce_create_product", Desc: "Create a product.", Method: http.MethodPost, Path: "/wc/v3/products", Props: prodFields, Required: []string{"name"}, Open: true})
	add(&mcpTool{Name: "commerce_update_product", Desc: "Update a product (only the fields given change).", Method: http.MethodPut, Path: "/wc/v3/products/{id}", Props: withID(prodFields), Required: []string{"id"}, Open: true})
	add(&mcpTool{Name: "commerce_update_stock", Desc: "Set a product's stock quantity (stock management is switched on if it was off).", Method: http.MethodPut, Path: "/wc/v3/products/{id}",
		Props:    map[string]any{"id": num("Product ID."), "stock_quantity": num("New stock quantity."), "manage_stock": flag("Default true."), "stock_status": str("instock, outofstock or onbackorder.")},
		Required: []string{"id", "stock_quantity"}})
	add(&mcpTool{Name: "commerce_delete_product", Desc: "Move a product to the trash, or delete it permanently with force=true.", Method: http.MethodDelete, Path: "/wc/v3/products/{id}",
		Props: map[string]any{"id": num("Product ID."), "force": flag("Skip the trash.")}, Required: []string{"id"}})
	add(&mcpTool{Name: "commerce_list_orders", Desc: "List orders. Filters: status (pending, processing, on-hold, completed, cancelled, refunded, failed), customer, search, after, before, per_page, page.", Method: http.MethodGet, Path: "/wc/v3/orders",
		Props: listProps(map[string]any{"status": str("Order status."), "customer": num("Customer user ID."), "after": str("ISO 8601 date."), "before": str("ISO 8601 date.")}), Open: true})
	add(&mcpTool{Name: "commerce_get_order", Desc: "Get one order with its lines, totals and addresses.", Method: http.MethodGet, Path: "/wc/v3/orders/{id}", Props: map[string]any{"id": num("Order ID.")}, Required: []string{"id"}})
	add(&mcpTool{Name: "commerce_update_order", Desc: "Update an order: status, customer_note, set_paid, addresses.", Method: http.MethodPut, Path: "/wc/v3/orders/{id}",
		Props: map[string]any{"id": num("Order ID."), "status": str("New status."), "customer_note": str("Note to the customer."), "set_paid": flag("Mark paid."),
			"billing": obj("Billing address."), "shipping": obj("Shipping address.")}, Required: []string{"id"}})
	add(&mcpTool{Name: "commerce_sales_report", Desc: "Sales report (totals, orders, items) for a period.", Method: http.MethodGet, Path: "/wc/v3/reports/sales",
		Props: map[string]any{"period": str("week, month, last_month or year."), "date_min": str("YYYY-MM-DD."), "date_max": str("YYYY-MM-DD.")}})

	add(&mcpTool{Name: "web_builder_get_document", Desc: "Get a Pursae Web Builder document (the page's layout tree) with its revision (rev).", Method: http.MethodGet, Path: "/web-builder/v2/documents/{id}",
		Props: map[string]any{"id": num("Post ID of the page.")}, Required: []string{"id"}})
	add(&mcpTool{Name: "web_builder_save_draft", Desc: "Save a Web Builder document as the caller's draft (the live page is untouched). Publishing renders the page in the editor, so open the page in Pursae Web Builder to publish. Read the contract in the pursae-docs resources first.",
		Method: http.MethodPut, Path: "/web-builder/v2/documents/{id}/autosave", Props: map[string]any{"id": num("Post ID of the page."), "doc": obj("The full document."), "page": obj("Page settings (optional)."), "baseRev": num("rev the document was read at.")},
		Required: []string{"id", "doc"}})

	add(&mcpTool{Name: "cache_status", Desc: "Pursae Cache status: enabled, entries, size, hit ratio, crawler state.", Method: http.MethodGet, Path: "/pursae/v1/cache"})
	add(&mcpTool{Name: "cache_purge", Desc: "Purge the page cache: all=true, or url, or post (ID), or tags.", Method: http.MethodPost, Path: "/pursae/v1/cache/purge",
		Props: map[string]any{"all": flag("Purge everything."), "url": str("A page URL or path."), "post": num("Purge the pages of this post."), "tags": strList("Cache tags, e.g. home, post:12.")}})
	add(&mcpTool{Name: "cache_get_settings", Desc: "Get Pursae Cache settings (wpgo_cache_* and wpgo_opt_* options).", Method: http.MethodGet, Path: "/pursae/v1/cache/settings"})
	add(&mcpTool{Name: "csp_get_settings", Desc: "Get the external sources the site allows in its front-end Content-Security-Policy (a theme script fetching an API, a web font host). " + cspExtraHelp,
		Method: http.MethodGet, Path: "/pursae/v1/csp"})
	add(&mcpTool{Name: "csp_update_settings", Desc: "Security decision: allow external origins in the front-end Content-Security-Policy. sources replaces the lists of the directives it names " +
		"(an empty list clears one; others stay). Every page of the site can then reach these hosts, so add only services the site owner trusts. Invalid entries return 400 and nothing is saved. " + cspExtraHelp,
		Method: http.MethodPost, Path: "/pursae/v1/csp", Props: map[string]any{"sources": map[string]any{"type": "object",
			"description":          `Directive → origins, e.g. {"connect-src": ["https://api.github.com"]}.`,
			"properties":           map[string]any{"connect-src": strList("Origins scripts may fetch / open WebSockets to."), "font-src": strList("Web font hosts."), "style-src": strList("Stylesheet hosts.")},
			"additionalProperties": false}}, Required: []string{"sources"}})
	add(&mcpTool{Name: "cache_update_settings", Desc: "Update Pursae Cache settings; keys as in cache_get_settings.", Method: http.MethodPost, Path: "/pursae/v1/cache/settings", Open: true})

	hgUID := str("Content type or component UID, e.g. blog (see headless_list_types).")
	hgDoc := str("Entry document ID.")
	hgLoc := str("Locale code (types with i18n only; default: the default locale).")
	add(&mcpTool{Name: "headless_list_types", Desc: "List Pursae Headless content types and components with their fields (the schema).", Method: http.MethodGet, Path: "/pursae/v1/headless/types"})
	add(&mcpTool{Name: "headless_save_type", Desc: "Create a Pursae Headless content type or component from a schema, e.g. {\"schema\": {\"kind\": \"collectionType\", \"displayName\": \"Blog\", \"singularName\": \"blog\", \"pluralName\": \"blogs\", \"draftAndPublish\": true, \"attributes\": [{\"name\": \"title\", \"type\": \"text\", \"required\": true}]}}. Field types: text, richtext, number, boolean, date, datetime, time, email, uid, enumeration, json, media, relation, component, dynamiczone. Read an existing one with headless_list_types for the exact shape.",
		Method: http.MethodPost, Path: "/pursae/v1/headless/types", Props: map[string]any{"schema": obj("The schema (kind, displayName, singularName, pluralName, draftAndPublish, localized, attributes).")}, Required: []string{"schema"}})
	add(&mcpTool{Name: "headless_update_type", Desc: "Replace the schema of an existing content type or component (stored entries are migrated). Fields are matched by name; set renamedFrom to rename one.",
		Method: http.MethodPost, Path: "/pursae/v1/headless/types/{uid}", Props: map[string]any{"uid": hgUID, "schema": obj("The full new schema.")}, Required: []string{"uid", "schema"}})
	add(&mcpTool{Name: "headless_delete_type", Desc: "Delete a content type or component and all its entries. Pass force=true.", Method: http.MethodDelete, Path: "/pursae/v1/headless/types/{uid}",
		Props: map[string]any{"uid": hgUID, "force": flag("Must be true.")}, Required: []string{"uid"}})
	add(&mcpTool{Name: "headless_find", Desc: "List the entries of a content type (draft versions by default).", Method: http.MethodGet, Path: "/pursae/v1/headless/entries/{uid}",
		Props: map[string]any{"uid": hgUID, "status": str("draft (default) or published."), "locale": hgLoc, "page": num("Page number."), "per_page": num("Entries per page (1-100, default 25).")}, Required: []string{"uid"}})
	add(&mcpTool{Name: "headless_get_entry", Desc: "Get one entry (draft and published values).", Method: http.MethodGet, Path: "/pursae/v1/headless/entries/{uid}/{doc}",
		Props: map[string]any{"uid": hgUID, "doc": hgDoc, "locale": hgLoc}, Required: []string{"uid", "doc"}})
	add(&mcpTool{Name: "headless_create_entry", Desc: "Create an entry; data holds the attribute values. publish=true also publishes it.", Method: http.MethodPost, Path: "/pursae/v1/headless/entries/{uid}",
		Props: map[string]any{"uid": hgUID, "data": obj("Attribute values."), "publish": flag("Publish right away."), "locale": hgLoc}, Required: []string{"uid", "data"}})
	add(&mcpTool{Name: "headless_save_entry", Desc: "Update an entry: the values in data are merged over the stored ones. publish=true also publishes.", Method: http.MethodPost, Path: "/pursae/v1/headless/entries/{uid}/{doc}",
		Props: map[string]any{"uid": hgUID, "doc": hgDoc, "data": obj("Attribute values to change."), "publish": flag("Publish after saving."), "locale": hgLoc}, Required: []string{"uid", "doc", "data"}})
	add(&mcpTool{Name: "headless_publish", Desc: "Publish an entry's current draft.", Method: http.MethodPost, Path: "/pursae/v1/headless/entries/{uid}/{doc}/publish",
		Props: map[string]any{"uid": hgUID, "doc": hgDoc, "locale": hgLoc}, Required: []string{"uid", "doc"}})
	add(&mcpTool{Name: "headless_unpublish", Desc: "Unpublish an entry (the draft is kept).", Method: http.MethodPost, Path: "/pursae/v1/headless/entries/{uid}/{doc}/unpublish",
		Props: map[string]any{"uid": hgUID, "doc": hgDoc, "locale": hgLoc}, Required: []string{"uid", "doc"}})
	add(&mcpTool{Name: "headless_delete_entry", Desc: "Delete an entry. Pass force=true.", Method: http.MethodDelete, Path: "/pursae/v1/headless/entries/{uid}/{doc}",
		Props: map[string]any{"uid": hgUID, "doc": hgDoc, "locale": hgLoc, "force": flag("Must be true.")}, Required: []string{"uid", "doc"}})

	group := str("Option group: contact, cookie, gocdn, headless, maintenance, multilang, pixel, security, seo or smtp.")
	add(&mcpTool{Name: "list_option_groups", Desc: "List the built-in plugins whose settings get_options/update_options can change.", Method: http.MethodGet, Path: "/pursae/v1/options"})
	add(&mcpTool{Name: "get_options", Desc: "Get a built-in plugin's settings (secrets are masked as ********).", Method: http.MethodGet, Path: "/pursae/v1/options/{group}",
		Props: map[string]any{"group": group}, Required: []string{"group"}})
	add(&mcpTool{Name: "update_options", Desc: "Merge changes into a built-in plugin's settings; masked values sent back keep the stored secret.",
		Method: http.MethodPost, Path: "/pursae/v1/options/{group}", Props: map[string]any{"group": group, "settings": obj("Partial settings object.")}, Required: []string{"group", "settings"}})

	add(&mcpTool{Name: "site_info", Desc: "Site name, description, URL and the REST namespaces available.", Method: http.MethodGet, Path: "/", special: "info"})
	add(&mcpTool{Name: "rest_request", Desc: "Call any REST route of this site (e.g. GET /wp/v2/comments, POST /web-builder/v2/…). Same permissions as the other tools.",
		Method: "", Path: "", special: "rest",
		Props: map[string]any{"method": str("GET, POST, PUT, PATCH or DELETE."), "path": str("Route under /wp-json, e.g. /wp/v2/comments?per_page=5."),
			"body": obj("JSON body for POST/PUT/PATCH.")}, Required: []string{"method", "path"}})
	list = append(list, mcpBuilderTools()...)
	list = append(list, mcpSiteTools()...)
	return list
}()

// mcpRestRoute checks a rest_request path and returns the route under /wp-json.
// The path is percent-decoded (including double encoding) before the ".." check, because the
// HTTP server decodes it before routing: /wp/v2/%2e%2e/%2e%2e/wp-admin/ would otherwise leave
// /wp-json. The query string is kept as the client sent it.
func mcpRestRoute(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") ||
		strings.Contains(raw, "..") || strings.Contains(raw, "\\") || strings.ContainsRune(raw, 0) {
		return "", false
	}
	pathPart, query, _ := strings.Cut(raw, "?")
	decoded := pathPart
	for range 4 {
		next, err := url.PathUnescape(decoded)
		if err != nil || strings.ContainsRune(next, 0) {
			return "", false
		}
		if next == decoded {
			break
		}
		decoded = next
	}
	if decoded == "" || !strings.HasPrefix(decoded, "/") || strings.HasPrefix(decoded, "//") ||
		strings.Contains(decoded, "..") || strings.Contains(decoded, "\\") || strings.ContainsAny(decoded, "?#") {
		return "", false
	}
	for _, r := range decoded {
		if r < 0x20 || r == 0x7f {
			return "", false
		}
	}
	cleaned := path.Clean(decoded)
	slashless := strings.TrimSuffix(decoded, "/")
	if slashless == "" {
		slashless = "/"
	}
	// path.Clean drops a trailing slash and collapses "." / ".." / duplicate slashes.
	// Anything it rewrites, other than that trailing slash, is rejected.
	if cleaned != slashless {
		return "", false
	}
	if strings.Contains(strings.ToLower(cleaned), "/pursae/v1/mcp") {
		return "", false
	}
	out := decoded
	if len(decoded) >= len("/wp-json") && strings.EqualFold(decoded[:len("/wp-json")], "/wp-json") {
		rest := decoded[len("/wp-json"):]
		if rest != "" && !strings.HasPrefix(rest, "/") {
			return "", false
		}
		if rest == "" {
			rest = "/"
		}
		out = rest
	}
	if !strings.HasPrefix(out, "/") || strings.Contains(out, "..") {
		return "", false
	}
	full := path.Clean("/wp-json" + strings.TrimSuffix(out, "/"))
	if full != "/wp-json" && !strings.HasPrefix(full, "/wp-json/") {
		return "", false
	}
	if strings.Contains(strings.ToLower(full), "/pursae/v1/mcp") {
		return "", false
	}
	if query != "" {
		out += "?" + query
	}
	return out, true
}

func mcpToolByName(name string) *mcpTool {
	for _, t := range mcpTools {
		if t.Name == name {
			return t
		}
	}
	return nil
}

var mcpPathArg = regexp.MustCompile(`\{([a-z_]+)\}`)

// mcpRouteCache memoises mcpRouteFor by tool path (paths are constants).
var mcpRouteCache sync.Map

// mcpRouteFor replaces the {placeholders} of a tool path with plain words, once per path.
func mcpRouteFor(p string) string {
	if v, ok := mcpRouteCache.Load(p); ok {
		return v.(string)
	}
	r := mcpPathArg.ReplaceAllString(p, "x")
	mcpRouteCache.Store(p, r)
	return r
}

var mcpSafeSegment = regexp.MustCompile(`^[A-Za-z0-9_\-.]+$`)

// mcpCall runs a tool as a REST request and wraps the answer as an MCP tool result.
func (s *Server) mcpCall(c *gin.Context, t *mcpTool, args map[string]any) map[string]any {
	if t.run != nil {
		return s.mcpRunTool(c, t, args)
	}
	toolErr := func(msg string) map[string]any {
		return map[string]any{"content": []map[string]any{{"type": "text", "text": msg}}, "isError": true}
	}
	method, route := t.Method, t.Path
	rest := map[string]any{}
	for k, v := range args {
		// The arguments become query or body parameters of a fixed method. _method would override it
		// (rest.go), turning a read-only tool such as get_post into a DELETE; rest_request is the tool
		// that chooses the method.
		if t.special != "rest" && strings.EqualFold(k, "_method") {
			return toolErr("Unknown argument: " + k)
		}
		if _, known := t.Props[k]; !known && !t.Open {
			return toolErr("Unknown argument: " + k) // the schema says additionalProperties: false
		}
		rest[k] = v
	}
	if t.Name == "commerce_update_stock" {
		if _, ok := rest["manage_stock"]; !ok {
			rest["manage_stock"] = true
		}
	}
	var bad string
	route = mcpPathArg.ReplaceAllStringFunc(route, func(m string) string {
		k := m[1 : len(m)-1]
		v := fmt.Sprint(rest[k])
		if f, ok := rest[k].(float64); ok {
			v = strconv.FormatInt(int64(f), 10)
		}
		delete(rest, k)
		if !mcpSafeSegment.MatchString(v) {
			bad = k
		}
		return url.PathEscape(v)
	})
	if bad != "" {
		return toolErr("Missing or invalid argument: " + bad)
	}
	var body []byte
	contentType := ""
	switch t.special {
	case "rest":
		method = strings.ToUpper(fmt.Sprint(args["method"]))
		switch method {
		case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		default:
			return toolErr("method must be GET, POST, PUT, PATCH or DELETE")
		}
		var ok bool
		route, ok = mcpRestRoute(fmt.Sprint(args["path"]))
		if !ok {
			return toolErr("path must be a REST route starting with /, e.g. /wp/v2/comments")
		}
		pathOnly, _, _ := strings.Cut(route, "?")
		if c != nil && !restScopeAllows(c, pathOnly) {
			return toolErr("rest_scope_forbidden")
		}
		rest = map[string]any{}
		if b, ok := args["body"].(map[string]any); ok {
			rest = b
		}
	case "upload":
		var err error
		body, contentType, err = s.mcpUploadBody(c, args)
		if err != nil {
			return toolErr(err.Error())
		}
		rest = nil
	}
	if rest != nil && len(rest) > 0 {
		if method == http.MethodGet || method == http.MethodDelete {
			q := url.Values{}
			for k, v := range rest {
				switch x := v.(type) {
				case float64:
					q.Set(k, strconv.FormatFloat(x, 'f', -1, 64))
				case []any:
					var parts []string
					for _, e := range x {
						parts = append(parts, fmt.Sprint(e))
					}
					q.Set(k, strings.Join(parts, ","))
				default:
					q.Set(k, fmt.Sprint(x))
				}
			}
			sep := "?"
			if strings.Contains(route, "?") {
				sep = "&"
			}
			route += sep + q.Encode()
		} else {
			if inner, ok := rest["settings"].(map[string]any); ok && len(rest) == 1 && strings.HasPrefix(route, "/pursae/v1/") {
				rest = inner // update_options / seo_update_settings take the patch under "settings"
			}
			body, _ = json.Marshal(rest)
			contentType = "application/json"
		}
	}
	started := time.Now()
	status, out := s.mcpDispatch(c, method, route, body, contentType)
	s.mcpLogActivity(c, t.Name, method, route, status, time.Since(started))
	if t.special == "info" && status < 400 {
		var idx map[string]any
		if json.Unmarshal(out, &idx) == nil {
			out, _ = json.Marshal(map[string]any{"name": idx["name"], "description": idx["description"], "url": idx["url"], "home": idx["home"],
				"namespaces": idx["namespaces"], "version": Version, "scopes": restScopes(c)})
		}
	}
	text, truncated := mcpTruncate(string(out), mcpMaxText)
	res := map[string]any{"content": []map[string]any{{"type": "text", "text": text}}}
	if status >= 400 {
		res["isError"] = true
	} else if !truncated { // a cut answer must not travel again, whole, as structuredContent
		var v any
		if json.Unmarshal(out, &v) == nil {
			if m, ok := v.(map[string]any); ok {
				res["structuredContent"] = m
			}
		}
	}
	return res
}

// mcpMaxText is the longest tool answer sent as text.
const mcpMaxText = 400_000

// mcpTruncate cuts text to at most max bytes on a rune boundary and says whether it cut.
func mcpTruncate(text string, max int) (string, bool) {
	if len(text) <= max {
		return text, false
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(text[cut]) { // never split a multi-byte character
		cut--
	}
	return text[:cut] + "\n…(truncated; ask for fewer items with per_page / _fields)", true
}

// mcpDispatch runs a REST request in-process with the caller's credentials.
func (s *Server) mcpDispatch(c *gin.Context, method, route string, body []byte, contentType string) (int, []byte) {
	req := httptest.NewRequest(method, "/wp-json"+route, bytes.NewReader(body)).WithContext(c.Request.Context())
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for _, ck := range c.Request.Cookies() {
		req.AddCookie(ck)
	}
	for _, h := range []string{"Authorization", "X-WP-Nonce", "User-Agent", "X-Forwarded-Proto"} {
		if v := c.GetHeader(h); v != "" {
			req.Header.Set(h, v)
		}
	}
	if strings.HasPrefix(route, "/web-builder/v2") {
		req.Header.Set(webbuilder.APIHeader, strconv.Itoa(webbuilder.APIVersion)) // the editor handshake: the tool speaks the current contract
	}
	req.Host = c.Request.Host
	req.RemoteAddr = c.Request.RemoteAddr
	rec := httptest.NewRecorder()
	s.engine.ServeHTTP(rec, req)
	// The in-process response never passed the outer nonce swap (csp_nonce.go): the placeholder must not
	// reach the MCP client.
	return rec.Code, bytes.ReplaceAll(rec.Body.Bytes(), []byte(cspPH), []byte(newCSPNonce()))
}

// mcpUploadBody builds the multipart body of POST /wp/v2/media from base64 data or a fetched URL.
func (s *Server) mcpUploadBody(c *gin.Context, args map[string]any) ([]byte, string, error) {
	name, _ := args["filename"].(string)
	name = path.Base(strings.TrimSpace(name))
	if name == "" || name == "." || name == "/" {
		return nil, "", fmt.Errorf("filename is required")
	}
	var data []byte
	if b64, _ := args["data_base64"].(string); b64 != "" {
		r := &restRequest{Body: map[string]any{"d": b64}}
		var err error
		if data, err = restBase64(c, r, "d"); err != nil {
			return nil, "", err
		}
	} else if src, _ := args["source_url"].(string); src != "" {
		var err error
		if data, err = s.mcpFetch(c, src); err != nil {
			return nil, "", err
		}
	} else {
		return nil, "", fmt.Errorf("give data_base64 or source_url")
	}
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fw, err := w.CreateFormFile("file", name)
	if err != nil {
		return nil, "", err
	}
	_, _ = fw.Write(data)
	for _, k := range []string{"title", "alt_text", "caption", "description"} {
		if v, ok := args[k].(string); ok && v != "" {
			_ = w.WriteField(k, v)
		}
	}
	if v, ok := args["post"].(float64); ok && v > 0 {
		_ = w.WriteField("post", strconv.FormatInt(int64(v), 10))
	}
	_ = w.Close()
	return buf.Bytes(), w.FormDataContentType(), nil
}

// mcpFetch downloads a public http(s) URL for upload_media. The dialer refuses private and local
// addresses (publicOnly), like plugin HTTP, so the tool cannot reach the site's internal network.
func (s *Server) mcpFetch(c *gin.Context, src string) ([]byte, error) {
	u, err := url.Parse(src)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("source_url must be an http(s) URL")
	}
	req, err := http.NewRequestWithContext(c.Request.Context(), http.MethodGet, src, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Pursae/"+Version+" mcp")
	resp, err := mcpHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("source_url answered %s", resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, mcpFetchMax+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > mcpFetchMax {
		return nil, fmt.Errorf("the file is larger than %d MB", mcpFetchMax>>20)
	}
	return data, nil
}

// mcpFetchMax is the largest file source_url may bring in (a variable for tests).
var mcpFetchMax int64 = 64 << 20

var mcpHTTPClient = &http.Client{
	Timeout: 60 * time.Second,
	Transport: &http.Transport{DialContext: (&net.Dialer{Timeout: 5 * time.Second,
		Control: publicOnly(nil, "destination not allowed")}).DialContext},
}

// --- resources: developer docs ---------------------------------------------------------------

type mcpDoc struct{ uri, name, title, file string }

const mcpDocScheme = "pursae-docs://"

// mcpDocs lists the developer docs and skill files as resources. The embedded FS never changes, so
// the list is built once; the slice is shared and read-only.
var mcpDocs = sync.OnceValue(buildMCPDocs)

func buildMCPDocs() []mcpDoc {
	var out []mcpDoc
	_ = fs.WalkDir(docs.Developer, "developer", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || path.Ext(p) != ".md" {
			return nil //nolint:nilerr // an unreadable entry is skipped, the walk goes on
		}
		title := strings.TrimSuffix(p, ".md")
		if b, err := fs.ReadFile(docs.Developer, p); err == nil {
			for _, line := range strings.SplitN(string(b), "\n", 30) {
				if strings.HasPrefix(line, "# ") {
					title = strings.TrimSpace(line[2:])
					break
				}
				if strings.HasPrefix(line, "title:") {
					title = strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "title:")), `"'`)
					break
				}
			}
		}
		out = append(out, mcpDoc{uri: mcpDocScheme + p, name: p, title: title, file: p})
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].uri < out[j].uri })
	// The site-builder skill (skills/pursae-site-builder): playbooks for whole sites.
	_ = fs.WalkDir(skills.SiteBuilder, skills.SiteBuilderName, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || path.Ext(p) != ".md" {
			return nil
		}
		out = append(out, mcpDoc{uri: mcpSkillScheme + p, name: p, title: "Site builder skill: " + strings.TrimSuffix(path.Base(p), ".md"), file: p})
		return nil
	})
	return out
}

// mcpSkillScheme names the files of the site-builder skill as resources.
const mcpSkillScheme = "pursae-skill://"

func mcpReadDoc(uri string) ([]byte, bool) {
	if p, ok := strings.CutPrefix(uri, mcpSkillScheme); ok {
		if path.Clean(p) != p || !strings.HasPrefix(p, skills.SiteBuilderName+"/") || path.Ext(p) != ".md" {
			return nil, false
		}
		b, err := fs.ReadFile(skills.SiteBuilder, p)
		return b, err == nil
	}
	p := strings.TrimPrefix(uri, mcpDocScheme)
	if p == uri || path.Clean(p) != p || !strings.HasPrefix(p, "developer/") || path.Ext(p) != ".md" {
		return nil, false
	}
	b, err := fs.ReadFile(docs.Developer, p)
	return b, err == nil
}
