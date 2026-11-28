package server

import (
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"

	"io/fs"

	"github.com/gin-gonic/gin"

	"wpgo/internal/store"
	"wpgo/internal/wp"
)

// pursae/v1 is the site-management API AI tools (and the MCP server, mcp.go) use: plugins, themes and
// theme files, Pursae Cache, Pursae SEO settings and the option groups of the built-in plugins.
// Every route checks the same capability its admin screen checks; Application Password scopes
// (app_password_scopes.go) narrow it further.

const pursaeNS = "pursae/v1"

func (s *Server) registerPursaeREST() {
	api := s.rest
	guard := func(cp string, h restHandler) restHandler {
		return func(c *gin.Context, r *restRequest) {
			u := restCurrent(c)
			if u == nil {
				restErr(c, http.StatusUnauthorized, "rest_not_logged_in", "You are not currently logged in.")
				return
			}
			if !u.Can(cp, nil) {
				restErr(c, http.StatusForbidden, "rest_forbidden", "Sorry, you are not allowed to do that.")
				return
			}
			h(c, r)
		}
	}
	slug := `(?P<slug>[a-z0-9_\-]+)`

	api.register(pursaeNS, "/pursae/v1/plugins", map[string]restHandler{
		http.MethodGet:  guard("activate_plugins", s.pursaePluginsList),
		http.MethodPost: guard("install_plugins", s.pursaePluginInstall),
	}, nil)
	api.register(pursaeNS, "/pursae/v1/plugins/"+slug, map[string]restHandler{
		http.MethodGet:    guard("activate_plugins", s.pursaePluginGet),
		http.MethodDelete: guard("delete_plugins", s.pursaePluginAction("delete")),
	}, nil)
	api.register(pursaeNS, "/pursae/v1/plugins/"+slug+"/activate", map[string]restHandler{http.MethodPost: guard("activate_plugins", s.pursaePluginAction("activate"))}, nil)
	api.register(pursaeNS, "/pursae/v1/plugins/"+slug+"/deactivate", map[string]restHandler{http.MethodPost: guard("activate_plugins", s.pursaePluginAction("deactivate"))}, nil)

	api.register(pursaeNS, "/pursae/v1/themes", map[string]restHandler{
		http.MethodGet:  guard("switch_themes", s.pursaeThemesList),
		http.MethodPost: guard("install_themes", s.pursaeThemeInstall),
	}, nil)
	api.register(pursaeNS, "/pursae/v1/themes/"+slug+"/activate", map[string]restHandler{http.MethodPost: guard("switch_themes", s.pursaeThemeActivate)}, nil)
	api.register(pursaeNS, "/pursae/v1/themes/"+slug+"/files", map[string]restHandler{http.MethodGet: guard("edit_themes", s.pursaeThemeFiles)}, nil)
	api.register(pursaeNS, "/pursae/v1/themes/"+slug+"/file", map[string]restHandler{
		http.MethodGet:    guard("edit_themes", s.pursaeThemeFileGet),
		http.MethodPost:   guard("edit_themes", s.pursaeThemeFilePut),
		http.MethodDelete: guard("edit_themes", s.pursaeThemeFileRestore),
	}, nil)

	// Media: a new file for an existing item (media_replace_admin.go).
	api.register(pursaeNS, `/pursae/v1/media/(?P<id>[\d]+)/replace`, map[string]restHandler{http.MethodPost: guard("upload_files", s.pursaeMediaReplace)}, nil)

	// Pursae Analytics: the overview's numbers for a period (analytics_admin.go).
	api.register(pursaeNS, "/pursae/v1/analytics", map[string]restHandler{http.MethodGet: guard("manage_options", s.pursaeAnalytics)}, nil)

	api.register(pursaeNS, "/pursae/v1/cache", map[string]restHandler{http.MethodGet: guard("manage_options", s.pursaeCacheStatus)}, nil)
	api.register(pursaeNS, "/pursae/v1/csp-reports", map[string]restHandler{ // CSP violation reports (csp_report.go)
		http.MethodGet: guard("manage_options", s.pursaeCSPReports), http.MethodDelete: guard("manage_options", s.pursaeCSPReportsClear)}, nil)
	api.register(pursaeNS, "/pursae/v1/csp", map[string]restHandler{ // allowed external sources of the front-end policy (csp_extra.go)
		http.MethodGet: guard("manage_options", s.pursaeCSPExtra), http.MethodPost: guard("manage_options", s.pursaeCSPExtra)}, nil)
	api.register(pursaeNS, "/pursae/v1/cache/purge", map[string]restHandler{http.MethodPost: guard("manage_options", s.pursaeCachePurge)}, nil)
	api.register(pursaeNS, "/pursae/v1/cache/settings", map[string]restHandler{
		http.MethodGet:  guard("manage_options", s.pursaeCacheSettings),
		http.MethodPost: guard("manage_options", s.pursaeCacheSettings),
	}, nil)

	api.register(pursaeNS, "/pursae/v1/seo/settings", map[string]restHandler{
		http.MethodGet:  guard("manage_options", s.pursaeOptionsHandler("seo")),
		http.MethodPost: guard("manage_options", s.pursaeOptionsHandler("seo")),
	}, nil)
	api.register(pursaeNS, "/pursae/v1/seo/redirects", map[string]restHandler{
		http.MethodGet:  guard("manage_options", s.pursaeRedirectsList),
		http.MethodPost: guard("manage_options", s.pursaeRedirectCreate),
	}, nil)
	api.register(pursaeNS, `/pursae/v1/seo/redirects/(?P<id>[\d]+)`, map[string]restHandler{http.MethodDelete: guard("manage_options", s.pursaeRedirectDelete)}, nil)

	api.register(pursaeNS, "/pursae/v1/options", map[string]restHandler{http.MethodGet: guard("manage_options", s.pursaeOptionGroups)}, nil)
	api.register(pursaeNS, "/pursae/v1/options/(?P<group>[a-z0-9_\\-]+)", map[string]restHandler{
		http.MethodGet:  guard("manage_options", s.pursaeOptionsHandler("")),
		http.MethodPost: guard("manage_options", s.pursaeOptionsHandler("")),
	}, nil)

	s.registerPursaeOps(guard)
	s.registerWP2PREST() // WP to Pursae push mode (wp2p_rest.go): its own token or an administrator

	// MCP over streamable HTTP (mcp.go). Authentication is the REST API's: an Application Password.
	api.register(pursaeNS, "/pursae/v1/mcp", map[string]restHandler{
		http.MethodPost:   s.mcpHTTP,
		http.MethodGet:    s.mcpHTTP,
		http.MethodDelete: s.mcpHTTP,
	}, nil)
	api.register(pursaeNS, "/pursae/v1/mcp/activity", map[string]restHandler{http.MethodGet: guard("manage_options", s.pursaeMCPActivity)}, nil)
}

// --- plugins ---------------------------------------------------------------------------------

func pluginJSON(r pluginRow) map[string]any {
	perms := []map[string]string{}
	for _, p := range r.Permissions {
		perms = append(perms, map[string]string{"name": p.Name, "description": p.Description})
	}
	out := map[string]any{"slug": r.Slug, "name": r.Name, "version": r.Version, "active": r.Active, "native": r.Native,
		"source": r.Source, "description": wp.StripTags(r.Description), "author": r.Author, "permissions": perms}
	if r.Disabled {
		out["disabled"], out["disabled_reason"] = true, r.DisabledWhy
	}
	if r.Update != nil {
		out["update_available"] = true
	}
	return out
}

func (s *Server) pursaePluginsList(c *gin.Context, _ *restRequest) {
	out := []map[string]any{}
	for _, r := range s.pluginInventory(c.Request.Context()) {
		out = append(out, pluginJSON(r))
	}
	restJSON(c, http.StatusOK, out)
}

func (s *Server) pursaePluginRow(c *gin.Context, slug string) (pluginRow, bool) {
	for _, r := range s.pluginInventory(c.Request.Context()) {
		if r.Slug == slug {
			return r, true
		}
	}
	return pluginRow{}, false
}

func (s *Server) pursaePluginGet(c *gin.Context, r *restRequest) {
	row, ok := s.pursaePluginRow(c, r.Params["slug"])
	if !ok {
		restErr(c, http.StatusNotFound, "rest_plugin_not_found", "Plugin not found.")
		return
	}
	restJSON(c, http.StatusOK, pluginJSON(row))
}

// pursaePluginInstall installs a signed package sent as base64 ("package"); a new plugin is stored
// inactive, a new version of an active one is loaded right away (installAndReload).
func (s *Server) pursaePluginInstall(c *gin.Context, r *restRequest) {
	data, err := restBase64(c, r, "package")
	if err != nil {
		restErr(c, http.StatusBadRequest, "rest_invalid_param", err.Error())
		return
	}
	m, _, err := s.installAndReload(withExtActor(c.Request.Context(), restCurrent(c)), data, pluginSourceUpload, "", false)
	if errors.Is(err, errPluginReloadDenied) {
		restErr(c, http.StatusForbidden, "rest_cannot_activate_plugin", err.Error())
		return
	}
	if err != nil {
		restErr(c, http.StatusBadRequest, "rest_plugin_install_failed", err.Error())
		return
	}
	row, _ := s.pursaePluginRow(c, m.Slug)
	restJSON(c, http.StatusCreated, pluginJSON(row))
}

// pursaePluginAction runs the Plugins screen's own action (pluginAction) for one plugin, so native
// switches, Wasm loading, auditing and statistics behave exactly as in the admin. A Wasm plugin is
// granted the permissions in "permissions"; without it, every permission its manifest asks for
// except the dangerous ones (content:html, admin:scripts: the equivalent of an administrator's
// session), which the consent screen leaves unticked too and which must be named to be granted.
func (s *Server) pursaePluginAction(action string) restHandler {
	return func(c *gin.Context, r *restRequest) {
		slug := r.Params["slug"]
		row, ok := s.pursaePluginRow(c, slug)
		if !ok {
			restErr(c, http.StatusNotFound, "rest_plugin_not_found", "Plugin not found.")
			return
		}
		if action == "delete" && row.Native {
			restErr(c, http.StatusBadRequest, "rest_plugin_native", "Built-in plugins cannot be deleted; deactivate them instead.")
			return
		}
		form := url.Values{"action": {action}, "plugin": {slug}}
		if action == "activate" && !row.Native {
			if v, has := r.Body["permissions"].([]any); has {
				for _, p := range v {
					if ps, ok := p.(string); ok {
						form.Add("permissions[]", ps)
					}
				}
			} else {
				for _, p := range safePluginPermissions(row.Permissions) {
					form.Add("permissions[]", p)
				}
			}
		}
		loc, status, body := s.runAdminAs(c, restCurrent(c), form, func(sub *gin.Context) {
			sub.Request.PostForm.Set("_wpnonce", s.nonce(sub, action+"-plugin_"+slug))
			s.pluginAction(sub, action, slug)
		})
		if status >= 400 || strings.Contains(loc, "error=") || (status != http.StatusFound && status != http.StatusOK) {
			msg := body
			if u, err := url.Parse(loc); err == nil && u.Query().Get("error") != "" {
				msg = u.Query().Get("error")
			}
			if msg == "" {
				msg = "The plugin could not be " + action + "d."
			}
			restErr(c, http.StatusBadRequest, "rest_plugin_"+action+"_failed", wp.StripTags(msg))
			return
		}
		if action == "delete" {
			restJSON(c, http.StatusOK, map[string]any{"deleted": true, "previous": pluginJSON(row)})
			return
		}
		row, _ = s.pursaePluginRow(c, slug)
		restJSON(c, http.StatusOK, pluginJSON(row))
	}
}

// runAdminAs runs an admin form handler on a copy of the request, signed in as u, and returns the
// redirect location, the status and (for non-redirects) the body.
func (s *Server) runAdminAs(c *gin.Context, u *store.User, form url.Values, run func(sub *gin.Context)) (string, int, string) {
	rec := httptest.NewRecorder()
	sub, _ := gin.CreateTestContext(rec)
	req := c.Request.Clone(c.Request.Context())
	req.Method = http.MethodPost
	req.Body = http.NoBody
	req.PostForm = form
	req.Form = form
	sub.Request = req
	sub.Keys = map[any]any{}
	for k, v := range c.Keys {
		sub.Keys[k] = v
	}
	sub.Keys[ctxUser] = u
	delete(sub.Keys, ctxSession)
	run(sub)
	status := rec.Code
	if sub.Writer.Written() {
		status = sub.Writer.Status()
	}
	return rec.Header().Get("Location"), status, rec.Body.String()
}

// --- themes ----------------------------------------------------------------------------------

func (s *Server) themeJSON(t *Theme) map[string]any {
	return map[string]any{"slug": t.Slug, "name": t.Name, "version": t.Version, "description": wp.StripTags(t.Description),
		"author": t.Author, "active": t.Slug == s.activeTheme().Slug, "package": t.Package != nil, "tags": t.Tags}
}

func (s *Server) pursaeThemesList(c *gin.Context, _ *restRequest) {
	out := []map[string]any{}
	for _, t := range s.installedThemes() {
		out = append(out, s.themeJSON(t))
	}
	restJSON(c, http.StatusOK, out)
}

func (s *Server) pursaeThemeActivate(c *gin.Context, r *restRequest) {
	if err := s.switchTheme(c, r.Params["slug"]); err != nil {
		restErr(c, http.StatusBadRequest, "rest_theme_activate_failed", err.Error())
		return
	}
	restJSON(c, http.StatusOK, s.themeJSON(s.activeTheme()))
}

// pursaeThemeInstall installs a theme package (docs/theme-package.md) sent as base64 ("package").
func (s *Server) pursaeThemeInstall(c *gin.Context, r *restRequest) {
	// Activating is switch_themes' job (/themes/<slug>/activate): install_themes alone must not change the
	// live site. Refused before anything is installed.
	if act, _ := r.Bool(c, "activate"); act {
		if u := restCurrent(c); u == nil || !u.Can("switch_themes", nil) {
			restErr(c, http.StatusForbidden, "rest_cannot_switch_themes", "Sorry, you are not allowed to switch themes.")
			return
		}
	}
	data, err := restBase64(c, r, "package")
	if err != nil {
		restErr(c, http.StatusBadRequest, "rest_invalid_param", err.Error())
		return
	}
	t, err := s.installThemePackage(withExtActor(c.Request.Context(), restCurrent(c)), data)
	if errors.Is(err, errThemeUpdateDenied) {
		restErr(c, http.StatusForbidden, "rest_cannot_update_themes", err.Error())
		return
	}
	if err != nil {
		restErr(c, http.StatusBadRequest, "rest_theme_install_failed", err.Error())
		return
	}
	if act, _ := r.Bool(c, "activate"); act {
		if err := s.switchTheme(c, t.Slug); err != nil {
			restErr(c, http.StatusBadRequest, "rest_theme_activate_failed", err.Error())
			return
		}
	}
	restJSON(c, http.StatusCreated, s.themeJSON(t))
}

func (s *Server) pursaeTheme(c *gin.Context, r *restRequest) *Theme {
	t := s.installedTheme(r.Params["slug"])
	if t == nil {
		restErr(c, http.StatusNotFound, "rest_theme_not_found", "Theme not found.")
	}
	return t
}

func (s *Server) pursaeThemeFiles(c *gin.Context, r *restRequest) {
	t := s.pursaeTheme(c, r)
	if t == nil {
		return
	}
	o, ok := s.overlay()
	files := []map[string]any{}
	for _, f := range s.themeFileList(t.Slug) {
		files = append(files, map[string]any{"file": f, "edited": ok && t.Package == nil && o.overridden(themeFileWebPath(t.Slug, f))})
	}
	restJSON(c, http.StatusOK, map[string]any{"theme": t.Slug, "editable": s.themeFilesWritable(t) == nil, "files": files})
}

// themeFilesWritable is nil when the theme's files can be saved (the Theme File Editor's conditions).
func (s *Server) themeFilesWritable(t *Theme) error {
	if s.themeFileEditError() != nil { // saveThemeFile / restoreThemeFile check it again
		return errors.New("File editing is disabled on this site.")
	}
	if _, ok := s.overlay(); !ok || t.Custom {
		return errors.New("Sorry, that theme cannot be edited here.")
	}
	return nil
}

func restThemeFileParam(c *gin.Context, r *restRequest) string {
	f, _ := r.Str(c, "file")
	return f
}

func (s *Server) pursaeThemeFileGet(c *gin.Context, r *restRequest) {
	t := s.pursaeTheme(c, r)
	if t == nil {
		return
	}
	file := restThemeFileParam(c, r)
	if !themeFileListed(s.themeFileList(t.Slug), file) {
		restErr(c, http.StatusNotFound, "rest_theme_file_not_found", "File does not exist! Please double check the name and try again.")
		return
	}
	src := themeFileWebPath(t.Slug, file)
	b, err := fs.ReadFile(s.web, src)
	if err != nil {
		restErr(c, http.StatusNotFound, "rest_theme_file_not_found", "File does not exist! Please double check the name and try again.")
		return
	}
	restJSON(c, http.StatusOK, map[string]any{"theme": t.Slug, "file": file, "content": string(b)})
}

func themeFileListed(files []string, f string) bool {
	for _, x := range files {
		if x == f {
			return true
		}
	}
	return false
}

func (s *Server) pursaeThemeFilePut(c *gin.Context, r *restRequest) {
	t := s.pursaeTheme(c, r)
	if t == nil {
		return
	}
	if err := s.themeFilesWritable(t); err != nil {
		restErr(c, http.StatusForbidden, "rest_theme_file_disallowed", err.Error())
		return
	}
	file := restThemeFileParam(c, r)
	content, ok := r.Body["content"].(string)
	if !ok {
		restErr(c, http.StatusBadRequest, "rest_missing_callback_param", "Missing parameter(s): content", map[string]any{"params": []string{"content"}})
		return
	}
	if err := s.saveThemeFile(c, t.Slug, file, content); err != nil {
		code := "rest_theme_file_error"
		var fe *themeFileError
		if errors.As(err, &fe) {
			code = "rest_theme_file_" + fe.code
		}
		restErr(c, http.StatusBadRequest, code, err.Error())
		return
	}
	restJSON(c, http.StatusOK, map[string]any{"theme": t.Slug, "file": file, "saved": true})
}

func (s *Server) pursaeThemeFileRestore(c *gin.Context, r *restRequest) {
	t := s.pursaeTheme(c, r)
	if t == nil {
		return
	}
	if err := s.themeFilesWritable(t); err != nil {
		restErr(c, http.StatusForbidden, "rest_theme_file_disallowed", err.Error())
		return
	}
	file := restThemeFileParam(c, r)
	if err := s.restoreThemeFile(c, t.Slug, file); err != nil {
		restErr(c, http.StatusBadRequest, "rest_theme_file_error", err.Error())
		return
	}
	restJSON(c, http.StatusOK, map[string]any{"theme": t.Slug, "file": file, "restored": true})
}

// --- helpers ---------------------------------------------------------------------------------

// restBase64 reads a base64 (standard or URL alphabet) parameter, up to 64 MB decoded.
func restBase64(c *gin.Context, r *restRequest, key string) ([]byte, error) {
	v, _ := r.Str(c, key)
	v = strings.TrimSpace(v)
	if i := strings.Index(v, ";base64,"); i >= 0 { // a data: URL
		v = v[i+len(";base64,"):]
	}
	if v == "" {
		return nil, errors.New("Missing parameter(s): " + key + " (base64 of the package zip)")
	}
	if len(v) > 90<<20 {
		return nil, errors.New("the package is too large")
	}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(v); err == nil {
			return b, nil
		}
	}
	return nil, errors.New(key + " is not valid base64")
}
