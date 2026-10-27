package server

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"wpgo/internal/auth"
	"wpgo/internal/blocks"
	"wpgo/internal/mail"
	"wpgo/internal/store"
	"wpgo/internal/wp"
)

// fileNode is one entry of the #templateside tree (wp_make_theme_file_tree / wp_make_plugin_file_tree).
type fileNode struct {
	Name, Path, Desc, URL string
	Dir, Current          bool
	Level, Pos, Size      int
	Children              []*fileNode
}

// wpFileDescriptions is the part of $wp_file_descriptions that applies to WordPress Go themes
// (Go templates take the place of the PHP template files).
var wpFileDescriptions = map[string]string{
	"style.css": "Stylesheet", "editor-style.css": "Visual Editor Stylesheet", "rtl.css": "RTL Stylesheet",
	"templates/index.html": "Main Index Template", "templates/header.html": "Theme Header", "templates/footer.html": "Theme Footer",
	"templates/sidebar.html": "Sidebar", "templates/comments.html": "Comments", "templates/single.html": "Single Post",
	"templates/page.html": "Single Page", "templates/archive.html": "Archives", "templates/search.html": "Search Results",
	"templates/404.html": "404 Template", "templates/searchform.html": "Search Form", "templates/attachment.html": "Attachment Template",
	"templates/front-page.html": "Homepage", "templates/home.html": "Posts Page", "templates/image.html": "Image Attachment Template",
}

// buildFileTree turns relative paths into WordPress's file tree. first lists files shown ahead of the
// rest (theme-editor.php puts style.css and functions.php first); url builds each file link.
func buildFileTree(paths []string, first []string, current string, url func(string) string) []*fileNode {
	root := &fileNode{Dir: true}
	add := func(p string) {
		parts := strings.Split(p, "/")
		n := root
		for i, part := range parts {
			last := i == len(parts)-1
			var next *fileNode
			for _, ch := range n.Children {
				if ch.Name == part && ch.Dir != last {
					next = ch
				}
			}
			if next == nil {
				next = &fileNode{Name: part, Dir: !last}
				if last {
					next.Path, next.URL, next.Current = p, url(p), p == current
					if d, ok := wpFileDescriptions[p]; ok {
						next.Desc = d
					}
				}
				n.Children = append(n.Children, next)
			}
			n = next
		}
	}
	seen := map[string]bool{}
	for _, f := range first {
		for _, p := range paths {
			if p == f && !seen[p] {
				add(p)
				seen[p] = true
			}
		}
	}
	for _, p := range paths {
		if !seen[p] {
			add(p)
		}
	}
	var number func(n *fileNode, level int)
	number = func(n *fileNode, level int) {
		for i, ch := range n.Children {
			ch.Level, ch.Pos, ch.Size = level, i+1, len(n.Children)
			number(ch, level+1)
		}
	}
	number(root, 2)
	return root.Children
}

// codeEditorSettings is wp_get_code_editor_settings() for a file. Plugin files (signed packages) are
// always read-only; theme files are read-only only when file editing is disabled.
func codeEditorSettings(file string, readOnly bool) template.JS {
	mode := any("text/plain")
	switch path.Ext(file) {
	case ".css":
		mode = "text/css"
	case ".js":
		mode = "text/javascript"
	case ".json":
		mode = map[string]any{"name": "javascript", "json": true}
	case ".html", ".htm", ".svg", ".xml":
		mode = "htmlmixed"
	}
	b, _ := json.Marshal(map[string]any{"codeEditor": map[string]any{"codemirror": map[string]any{
		"indentUnit": 4, "indentWithTabs": true, "inputStyle": "contenteditable", "lineNumbers": true, "lineWrapping": true,
		"styleActiveLine": true, "continueComments": true, "direction": "ltr", "gutters": []string{}, "mode": mode, "lint": false,
		"autoCloseBrackets": true, "matchBrackets": true, "readOnly": readOnly,
		"extraKeys": map[string]string{"Ctrl-Space": "autocomplete", "Ctrl-/": "toggleComment", "Cmd-/": "toggleComment",
			"Alt-F": "findPersistent", "Ctrl-F": "findPersistent", "Cmd-F": "findPersistent"},
	}}})
	return template.JS(b)
}

type fileEditorData struct {
	Kind, Name, Slug, File, Content, State string
	Tree                                   []*fileNode
	Choices                                []menuChoice
	Settings                               template.JS
	NotFound                               bool
	// Theme files can be edited: the edited copy is stored outside the binary (see theme_files.go).
	Editable, Edited, Restored bool
	Nonce                      string
	RestoreURL                 string
	OverrideDir                string
	Builder                    bool // Golementor is active (the read-only notice points to its Theme Builder)
}

func (s *Server) fileEditorAssets(c *gin.Context, d *fileEditorData) {
	a := s.pageAssets(c)
	if !syntaxHighlighting(currentUser(c)) {
		// "Disable syntax highlighting when editing code": wp_enqueue_code_editor() returns false.
		d.Settings = `{"codeEditor":false}`
		a.Script("wp-theme-plugin-editor")
		a.AddInline("wp-theme-plugin-editor", "jQuery( function( $ ) { wp.themePluginEditor.init( $( \"#template\" ), "+string(d.Settings)+" ); } );")
		return
	}
	d.Settings = codeEditorSettings(d.File, !d.Editable)
	a.Script("wp-theme-plugin-editor", "code-editor").Style("code-editor")
	a.AddInline("wp-theme-plugin-editor", "jQuery( function( $ ) { wp.themePluginEditor.init( $( \"#template\" ), "+string(d.Settings)+" ); } );")
}

// themeEditor is theme-editor.php. Saving goes through admin-ajax edit-theme-plugin-file
// (themeFileAjax); edited files are layered over the bundled ones (theme_files.go).
func (s *Server) themeEditor(c *gin.Context) {
	if !currentUser(c).Can("edit_themes", nil) {
		c.String(http.StatusForbidden, "Sorry, you are not allowed to edit templates for this site.")
		return
	}
	theme := s.installedTheme(c.DefaultQuery("theme", s.activeTheme().Slug))
	if theme == nil {
		theme = s.activeTheme()
	}
	if c.Query("action") == "restore" {
		f := c.Query("file")
		if !s.checkNonce(c, "restore-theme-file_"+theme.Slug+"_"+f) {
			s.denyNonce(c)
			return
		}
		if err := s.restoreThemeFile(c, theme.Slug, f); err != nil {
			c.String(http.StatusBadRequest, err.Error())
			return
		}
		c.Redirect(http.StatusFound, "/wp-admin/theme-editor?theme="+url.QueryEscape(theme.Slug)+"&file="+url.QueryEscape(f)+"&restored=1")
		return
	}
	// assets/ is the theme root that is served to the browser (style.css, screenshot.png, js/, …);
	// templates/ holds the Go templates that take the place of WordPress's PHP template files.
	editable := map[string]bool{".html": true, ".css": true, ".js": true, ".json": true, ".txt": true, ".svg": true, ".md": true}
	var files []string
	for _, dir := range []string{"assets", "templates"} {
		base := "themes/" + theme.Slug + "/" + dir
		_ = fs.WalkDir(s.web, base, func(p string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() && editable[path.Ext(p)] {
				rel := strings.TrimPrefix(p, base+"/")
				if dir == "templates" {
					rel = "templates/" + rel
				}
				files = append(files, rel)
			}
			return nil
		})
	}
	sort.Strings(files)
	file := path.Clean("/" + c.DefaultQuery("file", "style.css"))[1:]
	src := "themes/" + theme.Slug + "/assets/" + file
	if strings.HasPrefix(file, "templates/") {
		if _, err := fs.Stat(s.web, "themes/"+theme.Slug+"/"+file); err == nil {
			src = "themes/" + theme.Slug + "/" + file
		}
	}
	d := &fileEditorData{Kind: "theme", Name: theme.Name, Slug: theme.Slug, File: file, State: "inactive", Builder: s.golementorOn()}
	if o, ok := s.overlay(); ok && !fileEditingDisabled() && !theme.Custom {
		d.Editable, d.OverrideDir, d.Restored = true, filepath.Join(o.dir, theme.Slug), c.Query("restored") == "1"
		d.Nonce = s.nonce(c, "edit-theme_"+theme.Slug+"_"+file)
		if o.overridden(themeFileWebPath(theme.Slug, file)) {
			d.Edited = true
			d.RestoreURL = "/wp-admin/theme-editor?action=restore&theme=" + url.QueryEscape(theme.Slug) + "&file=" + url.QueryEscape(file) +
				"&_wpnonce=" + s.nonce(c, "restore-theme-file_"+theme.Slug+"_"+file)
		}
	}
	if theme.Slug == s.activeTheme().Slug {
		d.State = "active"
	}
	if b, err := fs.ReadFile(s.web, src); err == nil {
		d.Content = string(b)
	} else {
		d.NotFound = true
	}
	d.Tree = buildFileTree(files, []string{"style.css", "templates/functions.html"}, file, func(p string) string {
		return "/wp-admin/theme-editor?file=" + url.QueryEscape(p) + "&theme=" + url.QueryEscape(theme.Slug)
	})
	for _, t := range s.installedThemes() {
		d.Choices = append(d.Choices, menuChoice{Value: t.Slug, Label: template.HTMLEscapeString(t.Name), Selected: t.Slug == theme.Slug})
	}
	s.fileEditorAssets(c, d)
	s.adminPage(c, "theme-editor", "Edit Themes", "theme-editor-php", "admin/file-editor", d)
}

// pluginEditor is plugin-editor.php. Plugins are signed WebAssembly packages, so the editor shows the
// text files inside the package (plugin.json, readme) read-only.
func (s *Server) pluginEditor(c *gin.Context) {
	if !currentUser(c).Can("edit_plugins", nil) {
		c.String(http.StatusForbidden, "Sorry, you are not allowed to edit plugins for this site.")
		return
	}
	var rows []pluginRow
	for _, r := range s.installedPlugins(c) {
		if !r.Native {
			rows = append(rows, r)
		}
	}
	d := &fileEditorData{Kind: "plugin"}
	if len(rows) == 0 {
		s.adminPage(c, "plugin-editor", "Edit Plugins", "plugin-editor-php", "admin/file-editor", d)
		return
	}
	sel := rows[0]
	want := c.Query("plugin")
	if f := c.Query("file"); want == "" && f != "" {
		want = strings.SplitN(f, "/", 2)[0]
	}
	for _, r := range rows {
		if r.Slug == want {
			sel = r
		}
		d.Choices = append(d.Choices, menuChoice{Value: r.Slug, Label: template.HTMLEscapeString(r.Name)})
	}
	for i := range d.Choices {
		d.Choices[i].Selected = d.Choices[i].Value == sel.Slug
	}
	d.Name, d.Slug = sel.Name, sel.Slug
	if sel.Active {
		d.State = "active"
	} else {
		d.State = "inactive"
	}
	contents := map[string]string{}
	var files []string
	if zr, err := zip.OpenReader(path.Join(s.plugins.dir, sel.Slug+".zip")); err == nil {
		for _, f := range zr.File {
			ext := path.Ext(f.Name)
			if f.FileInfo().IsDir() || (ext != ".json" && ext != ".txt" && ext != ".md" && ext != ".css" && ext != ".js") {
				continue
			}
			rc, err := f.Open()
			if err != nil {
				continue
			}
			var buf bytes.Buffer
			_, _ = buf.ReadFrom(rc)
			rc.Close()
			p := sel.Slug + "/" + strings.TrimPrefix(f.Name, "/")
			contents[p] = buf.String()
			files = append(files, p)
		}
		zr.Close()
	}
	sort.Strings(files)
	main := sel.Slug + "/plugin.json"
	d.File = c.DefaultQuery("file", main)
	if !strings.HasPrefix(d.File, sel.Slug+"/") {
		d.File = main
	}
	content, ok := contents[d.File]
	d.Content, d.NotFound = content, !ok
	// wp_make_plugin_file_tree(): the tree lists paths inside the plugin folder.
	rel := make([]string, len(files))
	for i, f := range files {
		rel[i] = strings.TrimPrefix(f, sel.Slug+"/")
	}
	d.Tree = buildFileTree(rel, []string{"plugin.json"}, strings.TrimPrefix(d.File, sel.Slug+"/"), func(p string) string {
		return "/wp-admin/plugin-editor?file=" + url.QueryEscape(sel.Slug+"/"+p) + "&plugin=" + url.QueryEscape(sel.Slug)
	})
	s.fileEditorAssets(c, d)
	s.adminPage(c, "plugin-editor", "Edit Plugins", "plugin-editor-php", "admin/file-editor", d)
}

// Personal data (export-personal-data.php / erase-personal-data.php) ------------------
//
// Requests follow WordPress' user_request flow: a request is created (request-pending), the person
// confirms it from the link in "[Site] Confirm Action: …" (wp-login.php?action=confirmaction →
// request-confirmed), the administrator fulfils it (request-completed). Requests are kept as a
// list in the wpgo_privacy_requests option.

type personalRequest struct {
	ID          int64
	Email       string
	Type        string // export_personal_data | remove_personal_data
	Status      string // request-pending | request-confirmed | request-failed | request-completed
	CreatedAt   time.Time
	ConfirmedAt time.Time
	CompletedAt time.Time
	KeyHash     string    // hashed confirm key (post_password in WordPress)
	KeyAt       time.Time // when the key was generated (wp_user_request_key_expiration: 3 days)
	ExportFile  string    // generated export file name
}

const (
	privacyKeyExpiration    = 3 * 24 * time.Hour
	privacyExportExpiration = 3 * 24 * time.Hour
)

func (s *Server) loadPersonalRequests() []personalRequest {
	raw, _ := s.settings.Get("wpgo_privacy_requests").([]any)
	var out []personalRequest
	for _, r := range raw {
		b, _ := json.Marshal(r)
		var pr personalRequest
		if json.Unmarshal(b, &pr) == nil && pr.ID > 0 {
			out = append(out, pr)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out
}

func (s *Server) savePersonalRequests(c *gin.Context, list []personalRequest) {
	raw := make([]any, 0, len(list))
	for _, r := range list {
		raw = append(raw, r)
	}
	_ = s.settings.Set(c, map[string]any{"wpgo_privacy_requests": raw})
}

func (s *Server) personalRequests(c *gin.Context, typ string) []personalRequest {
	var out []personalRequest
	for _, r := range s.loadPersonalRequests() {
		if r.Type == typ {
			out = append(out, r)
		}
	}
	return out
}

func (s *Server) personalRequest(id int64) (personalRequest, bool) {
	for _, r := range s.loadPersonalRequests() {
		if r.ID == id {
			return r, true
		}
	}
	return personalRequest{}, false
}

// updatePersonalRequest stores one request (replacing the record with the same ID).
func (s *Server) updatePersonalRequest(c *gin.Context, r personalRequest) {
	list := s.loadPersonalRequests()
	for i := range list {
		if list[i].ID == r.ID {
			list[i] = r
			s.savePersonalRequests(c, list)
			return
		}
	}
	s.savePersonalRequests(c, append(list, r))
}

func (s *Server) deletePersonalRequest(c *gin.Context, id int64) {
	list := s.loadPersonalRequests()
	var keep []personalRequest
	for _, r := range list {
		if r.ID != id {
			keep = append(keep, r)
		} else if r.ExportFile != "" {
			_ = os.Remove(filepath.Join(s.privacyExportsDir(), r.ExportFile))
		}
	}
	s.savePersonalRequests(c, keep)
}

// createUserRequest is wp_create_user_request(): one open request per e-mail and action.
func (s *Server) createUserRequest(c *gin.Context, typ, email, status string) (personalRequest, error) {
	list := s.loadPersonalRequests()
	var next int64 = 1
	for _, r := range list {
		if r.ID >= next {
			next = r.ID + 1
		}
		if r.Type == typ && strings.EqualFold(r.Email, email) && r.Status != "request-completed" {
			return personalRequest{}, errors.New("An incomplete personal data request for this email address already exists.")
		}
	}
	r := personalRequest{ID: next, Email: email, Type: typ, Status: status, CreatedAt: time.Now().UTC()}
	if status == "request-confirmed" {
		r.ConfirmedAt = r.CreatedAt
	}
	s.savePersonalRequests(c, append(list, r))
	return r, nil
}

// privacyActionDescription is wp_user_request_action_description().
func privacyActionDescription(typ string) string {
	if typ == "remove_personal_data" {
		return "Erase Personal Data"
	}
	return "Export Personal Data"
}

func (s *Server) privacyAdminURL(c *gin.Context, typ string) string {
	if typ == "remove_personal_data" {
		return s.links(c).HomeURL("/wp-admin/erase-personal-data")
	}
	return s.links(c).HomeURL("/wp-admin/export-personal-data")
}

// sendUserRequest is wp_send_user_request(): the confirmation e-mail with a fresh key.
func (s *Server) sendUserRequest(c *gin.Context, r personalRequest) personalRequest {
	key := auth.RandomToken(20)
	sum := sha256.Sum256([]byte(key))
	r.KeyHash, r.KeyAt, r.Status = hex.EncodeToString(sum[:]), time.Now().UTC(), "request-pending"
	s.updatePersonalRequest(c, r)
	site := s.settings.String("blogname")
	confirm := s.links(c).HomeURL("/wp-login?action=confirmaction&request_id=" + itoa64(r.ID) + "&confirm_key=" + key)
	body := "Howdy,\n\nA request has been made to perform the following action on your account:\n\n     ###DESCRIPTION###\n\n" +
		"To confirm this, please click on the following link:\n###CONFIRM_URL###\n\n" +
		"You can safely ignore and delete this email if you do not want to\ntake this action.\n\nRegards,\nAll at ###SITENAME###\n###SITEURL###\n"
	body = strings.NewReplacer("###DESCRIPTION###", privacyActionDescription(r.Type), "###CONFIRM_URL###", confirm,
		"###SITENAME###", site, "###SITEURL###", s.links(c).HomeURL("/")).Replace(body)
	s.sendMail(c, mail.Message{To: []string{r.Email}, Subject: fmt.Sprintf("[%s] Confirm Action: %s", site, privacyActionDescription(r.Type)), Body: body})
	return r
}

// confirmAction is wp-login.php?action=confirmaction (wp_validate_user_request_key() +
// _wp_privacy_account_request_confirmed()).
func (s *Server) confirmAction(c *gin.Context, v loginView) {
	v.Title, v.Action = "User action confirmed.", "confirmaction"
	die := func(msg string) {
		c.Header("Content-Type", "text/html; charset=utf-8")
		c.String(http.StatusForbidden, `<!DOCTYPE html><html><head><meta charset="utf-8"><title>WordPress &rsaquo; Error</title>`+
			`<link rel="stylesheet" href="/wp-admin/assets/css/wp/install.min.css"></head><body id="error-page"><div class="wp-die-message"><p>`+template.HTMLEscapeString(msg)+`</p></div></body></html>`)
	}
	idRaw, key := c.Query("request_id"), c.Query("confirm_key")
	if idRaw == "" {
		die("Missing request ID.")
		return
	}
	if key == "" {
		die("Missing confirm key.")
		return
	}
	id, _ := strconv.ParseInt(idRaw, 10, 64)
	r, ok := s.personalRequest(id)
	if !ok {
		die("Invalid request.")
		return
	}
	if r.Status != "request-pending" {
		die("This personal data request has expired.")
		return
	}
	if r.KeyHash == "" {
		die("The confirmation key is missing from this personal data request.")
		return
	}
	sum := sha256.Sum256([]byte(key))
	if subtle.ConstantTimeCompare([]byte(hex.EncodeToString(sum[:])), []byte(r.KeyHash)) != 1 {
		die("The confirmation key is invalid for this personal data request.")
		return
	}
	if time.Since(r.KeyAt) > privacyKeyExpiration {
		die("The confirmation key has expired for this personal data request.")
		return
	}
	r.Status, r.ConfirmedAt = "request-confirmed", time.Now().UTC()
	s.updatePersonalRequest(c, r)
	s.notifyRequestConfirmed(c, r)
	if r.Type == "remove_personal_data" {
		v.Message = `<span class="message">Thanks for confirming your erasure request.</span></p><p>The site administrator has been notified. You will receive an email confirmation when they erase your data.`
	} else {
		v.Message = `<span class="message">Thanks for confirming your export request.</span></p><p>The site administrator has been notified. You will receive a link to download your export via email when they fulfil your request.`
	}
	s.renderAdminTemplate(c, http.StatusOK, "admin/login-confirmaction", v)
}

// notifyRequestConfirmed is _wp_privacy_send_request_confirmation_notification().
func (s *Server) notifyRequestConfirmed(c *gin.Context, r personalRequest) {
	site := s.settings.String("blogname")
	body := "Howdy,\n\nA user data privacy request has been confirmed on ###SITENAME###:\n\nUser: ###USER_EMAIL###\nRequest: ###DESCRIPTION###\n\n" +
		"You can view and manage these data privacy requests here:\n\n###MANAGE_URL###\n\nRegards,\nAll at ###SITENAME###\n###SITEURL###\n"
	body = strings.NewReplacer("###SITENAME###", site, "###USER_EMAIL###", r.Email, "###DESCRIPTION###", privacyActionDescription(r.Type),
		"###MANAGE_URL###", s.privacyAdminURL(c, r.Type), "###SITEURL###", s.links(c).HomeURL("/")).Replace(body)
	s.sendMail(c, mail.Message{To: []string{s.settings.String("admin_email")}, Subject: fmt.Sprintf("[%s] Action Confirmed: %s", site, privacyActionDescription(r.Type)), Body: body})
}

// completePersonalRequest is _wp_privacy_completed_request().
func (s *Server) completePersonalRequest(c *gin.Context, r personalRequest) personalRequest {
	r.Status, r.CompletedAt = "request-completed", time.Now().UTC()
	s.updatePersonalRequest(c, r)
	return r
}

type privacyRow struct {
	personalRequest
	StatusLabel string
	Actions     []privacyAction // row actions
	Next        template.HTML   // "Next steps" column
	ExportURL   string
}

type privacyAction struct {
	Class, Label, URL string
}

// privacyTool serves both screens; typ is "export_personal_data" or "remove_personal_data".
func (s *Server) privacyTool(typ string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !currentUser(c).Can("manage_options", nil) {
			c.String(http.StatusForbidden, "Sorry, you are not allowed to manage privacy on this site.")
			return
		}
		screen, title := "export-personal-data", "Export Personal Data"
		if typ == "remove_personal_data" {
			screen, title = "erase-personal-data", "Erase Personal Data"
		}
		var notices []notice
		note := func(kind, msg string) {
			notices = append(notices, notice{Type: kind, Message: template.HTML(msg), Dismissible: true})
		}
		if c.Request.Method == http.MethodPost && c.PostForm("action") == "add_export_personal_data_request" || c.Request.Method == http.MethodPost && c.PostForm("action") == "add_remove_personal_data_request" {
			if !s.checkNonce(c, "personal-data-request") {
				s.denyNonce(c)
				return
			}
			email := strings.TrimSpace(c.PostForm("username_or_email_for_privacy_request"))
			if u, err := s.store.UserByLoginOrEmail(c, email); err == nil {
				email = u.Email
			}
			if !validEmail(email) {
				note("error", "Unable to initiate confirmation request. Invalid email address or username.")
			} else {
				sendConfirm := c.PostForm("send_confirmation_email") == "1"
				status := "request-confirmed"
				if sendConfirm {
					status = "request-pending"
				}
				r, err := s.createUserRequest(c, typ, email, status)
				switch {
				case err != nil:
					note("error", template.HTMLEscapeString(err.Error()))
				case sendConfirm:
					s.sendUserRequest(c, r)
					note("success", "Confirmation request initiated successfully.")
				default:
					note("success", "Request added successfully.")
				}
			}
		}
		if action := c.Query("action"); action != "" {
			if !s.checkNonce(c, "bulk-privacy_requests") {
				s.denyNonce(c)
				return
			}
			ids := c.QueryArray("request_id[]")
			if id := c.Query("request_id"); id != "" {
				ids = append(ids, id)
			}
			var count int
			for _, raw := range ids {
				id, _ := strconv.ParseInt(raw, 10, 64)
				r, ok := s.personalRequest(id)
				if !ok || r.Type != typ {
					continue
				}
				switch action {
				case "resend":
					s.sendUserRequest(c, r)
					count++
				case "complete":
					s.completePersonalRequest(c, r)
					count++
				case "delete":
					s.deletePersonalRequest(c, id)
					count++
				case "download":
					if typ == "export_personal_data" {
						s.downloadPersonalData(c, r)
						return
					}
				case "send-export":
					if typ == "export_personal_data" {
						if r, err := s.sendPersonalDataExport(c, r); err != nil {
							note("error", "Email could not be sent.")
						} else {
							s.completePersonalRequest(c, r)
							note("success", "Email sent.")
						}
					}
				case "erase":
					if typ == "remove_personal_data" {
						n := s.erasePersonalData(c, r.Email)
						s.completePersonalRequest(c, r)
						s.notifyErasureFulfilled(c, r)
						note("success", template.HTMLEscapeString(fmt.Sprintf("Personal data was erased: %d comments anonymized.", n)))
					}
				}
			}
			switch action {
			case "resend":
				if count > 0 {
					note("success", "Confirmation request sent again successfully.")
				}
			case "complete":
				if count > 0 {
					note("success", "Request marked as complete.")
				}
			case "delete":
				if count == 1 {
					note("success", "1 request deleted successfully.")
				} else if count > 1 {
					note("success", template.HTMLEscapeString(fmt.Sprintf("%d requests deleted successfully.", count)))
				}
			}
		}
		nonce := s.nonce(c, "bulk-privacy_requests")
		link := func(action string, id int64) string {
			return "/wp-admin/" + screen + "?action=" + action + "&request_id=" + itoa64(id) + "&_wpnonce=" + nonce
		}
		filter := c.Query("filter-status")
		counts := map[string]int{}
		var rows []privacyRow
		for _, r := range s.personalRequests(c, typ) {
			counts["all"]++
			counts[r.Status]++
			if filter != "" && filter != r.Status {
				continue
			}
			row := privacyRow{personalRequest: r, StatusLabel: privacyStatusLabel(r.Status)}
			if typ == "export_personal_data" {
				row.Actions = append(row.Actions, privacyAction{"download-data", "Download personal data", link("download", r.ID)})
			}
			if r.Status != "request-completed" {
				row.Actions = append(row.Actions, privacyAction{"complete", "Mark as completed", link("complete", r.ID)})
			}
			row.Actions = append(row.Actions, privacyAction{"delete", "Remove request", link("delete", r.ID)})
			if r.Status == "request-pending" {
				row.Actions = append(row.Actions, privacyAction{"resend", "Resend confirmation request", link("resend", r.ID)})
			}
			esc := template.HTMLEscapeString
			switch {
			case r.Status == "request-completed":
				row.Next = template.HTML(`<a href="` + esc(link("delete", r.ID)) + `" class="button">Remove request</a>`)
			case typ == "export_personal_data" && r.Status == "request-pending":
				row.Next = "Waiting for confirmation"
			case typ == "export_personal_data" && r.Status == "request-failed":
				row.Next = template.HTML(`<span class="export-personal-data"><a href="` + esc(link("send-export", r.ID)) + `" class="button export-personal-data-handle">Retry</a></span>`)
			case typ == "export_personal_data":
				row.Next = template.HTML(`<span class="export-personal-data"><a href="` + esc(link("send-export", r.ID)) + `" class="button-link export-personal-data-handle">Send export link</a></span>`)
			case r.Status == "request-pending":
				row.Next = template.HTML(`Waiting for confirmation <span class="remove-personal-data"><a href="` + esc(link("erase", r.ID)) + `" class="button remove-personal-data-handle">Force erase personal data</a></span>`)
			case r.Status == "request-failed":
				row.Next = template.HTML(`<span class="remove-personal-data"><a href="` + esc(link("erase", r.ID)) + `" class="button remove-personal-data-handle">Force erase personal data</a></span>`)
			default:
				row.Next = template.HTML(`<span class="remove-personal-data"><a href="` + esc(link("erase", r.ID)) + `" class="button remove-personal-data-handle">Erase personal data</a></span>`)
			}
			rows = append(rows, row)
		}
		type view struct {
			Slug, Label string
			Count       int
			Current     bool
		}
		views := []view{{"", "All", counts["all"], filter == ""}}
		for _, st := range [][2]string{{"request-pending", "Pending"}, {"request-confirmed", "Confirmed"}, {"request-failed", "Failed"}, {"request-completed", "Completed"}} {
			if counts[st[0]] > 0 {
				views = append(views, view{st[0], st[1], counts[st[0]], filter == st[0]})
			}
		}
		s.adminPage(c, screen, title, screen+"-php privacy-settings", "admin/privacy-tool", map[string]any{
			"Type": typ, "Title": title, "Rows": rows, "Nonce": s.nonce(c, "personal-data-request"), "BulkNonce": nonce, "Screen": screen, "Views": views}, notices...)
	}
}

func privacyStatusLabel(status string) string {
	switch status {
	case "request-pending":
		return "Pending"
	case "request-confirmed":
		return "Confirmed"
	case "request-failed":
		return "Failed"
	case "request-completed":
		return "Completed"
	}
	return status
}

func htmlf(format string, a ...any) template.HTML {
	return template.HTML(template.HTMLEscapeString(fmt.Sprintf(format, a...)))
}

// Export ------------------------------------------------------------------------------

type exportItem struct {
	Name, Value string
}

type exportGroup struct {
	ID, Label, Description string
	Items                  [][]exportItem
}

// personalDataGroups runs the core exporters (wp_user_personal_data_exporter, wp_comments_personal_data_exporter,
// wp_media_personal_data_exporter) for the e-mail address.
func (s *Server) personalDataGroups(c *gin.Context, email string) []exportGroup {
	loc := s.loc()
	dateFmt := s.settings.String("date_format") + " " + s.settings.String("time_format")
	var groups []exportGroup
	about := exportGroup{ID: "about", Label: "About", Description: "Overview of export report.", Items: [][]exportItem{{
		{"Report generated for", email}, {"For site", s.settings.String("blogname")}, {"At URL", s.links(c).HomeURL("/")}, {"On", wp.FormatDate(time.Now().In(loc), dateFmt)}}}}
	groups = append(groups, about)
	u, err := s.store.UserByLoginOrEmail(c, email)
	if err == nil {
		groups = append(groups, exportGroup{ID: "user", Label: "User", Description: "User&#8217;s profile data.", Items: [][]exportItem{{
			{"User ID", itoa64(u.ID)}, {"User Login Name", u.Login}, {"User Nice Name", u.Nicename}, {"User Email", u.Email},
			{"User Registration Date", u.RegisteredAt.UTC().Format("2006-01-02 15:04:05")}, {"User Display Name", u.DisplayName},
			{"User Nickname", u.Profile["nickname"]}, {"User First Name", u.Profile["first_name"]}, {"User Last Name", u.Profile["last_name"]},
			{"User Description", u.Profile["description"]}, {"User URL", u.URL}}}})
		rows, err := s.store.Pool.Query(c, `SELECT expires_at, ip, user_agent, last_seen_at FROM sessions WHERE user_id = $1 AND expires_at > now() ORDER BY id`, u.ID)
		if err == nil {
			g := exportGroup{ID: "user-session-tokens", Label: "Session Tokens", Description: "User&#8217;s Session Tokens data."}
			for rows.Next() {
				var exp, seen time.Time
				var ip, ua string
				if rows.Scan(&exp, &ip, &ua, &seen) == nil {
					g.Items = append(g.Items, []exportItem{{"Expiration", wp.FormatDate(exp.In(loc), dateFmt)}, {"IP", ip}, {"User Agent", ua}, {"Last Login", wp.FormatDate(seen.In(loc), dateFmt)}})
				}
			}
			rows.Close()
			if len(g.Items) > 0 {
				groups = append(groups, g)
			}
		}
	}
	comments, _, _ := s.store.QueryComments(c, store.CommentQuery{Status: "all", PerPage: 100000, Order: "asc"})
	cg := exportGroup{ID: "comments", Label: "Comments", Description: "User&#8217;s comment data."}
	for _, cm := range comments {
		if !strings.EqualFold(cm.AuthorEmail, email) {
			continue
		}
		link := ""
		if p, err := s.store.GetPost(c, cm.PostID); err == nil {
			link = s.links(c).Permalink(p) + "#comment-" + itoa64(cm.ID)
		}
		cg.Items = append(cg.Items, []exportItem{{"Comment Author", cm.AuthorName}, {"Comment Author Email", cm.AuthorEmail}, {"Comment Author IP", cm.AuthorIP},
			{"Comment Author User Agent", cm.UserAgent}, {"Comment Date", cm.CreatedAt.In(loc).Format("2006-01-02 15:04:05")}, {"Comment Content", cm.Content}, {"Comment URL", link}})
	}
	if len(cg.Items) > 0 {
		groups = append(groups, cg)
	}
	if u != nil {
		atts, _, _ := s.store.QueryAttachments(c, store.AttachmentQuery{AuthorID: u.ID, PerPage: 100000, Page: 1, OrderBy: "id", Order: "asc"})
		mg := exportGroup{ID: "media", Label: "Media", Description: "User&#8217;s media data."}
		for _, a := range atts {
			mg.Items = append(mg.Items, []exportItem{{"URL", s.uploadURL(c, a.FilePath)}})
		}
		if len(mg.Items) > 0 {
			groups = append(groups, mg)
		}
	}
	return groups
}

// personalDataExportHTML is wp_privacy_generate_personal_data_export_file()'s index.html.
func personalDataExportHTML(email string, groups []exportGroup) string {
	esc := template.HTMLEscapeString
	var b strings.Builder
	b.WriteString("<!DOCTYPE html>\n<html>\n<head>\n<meta http-equiv='Content-Type' content='text/html; charset=UTF-8' />\n")
	b.WriteString("<style type='text/css'>body { color: #2c3338; font-family: Helvetica, Arial, sans-serif; font-size: 12px; }table { background: #f6f7f7; border: 1px solid #c3c4c7; margin-bottom: 20px; width: 100%; }th { padding: 5px; text-align: left; width: 20%; }td { padding: 5px; }tr:nth-child(odd) { background-color: #fff; }.return-to-top { text-align: right; }</style>\n")
	title := "Personal Data Export for " + esc(email)
	b.WriteString("<title>" + title + "</title>\n</head>\n<body>\n<h1 id=\"top\">" + title + "</h1>\n")
	b.WriteString("<h2 id=\"table_of_contents\">Table of Contents</h2>\n<ul class=\"toc\">\n")
	for _, g := range groups {
		count := ""
		if len(g.Items) > 1 {
			count = fmt.Sprintf(" <span class=\"count\">(%d)</span>", len(g.Items))
		}
		b.WriteString("<li><a href=\"#" + esc(g.ID) + "\">" + g.Label + count + "</a></li>\n")
	}
	b.WriteString("</ul>\n")
	for _, g := range groups {
		b.WriteString("<h2 id=\"" + esc(g.ID) + "\">" + g.Label + "</h2>\n")
		if g.Description != "" {
			b.WriteString("<p>" + g.Description + "</p>\n")
		}
		b.WriteString("<div>\n")
		for _, item := range g.Items {
			b.WriteString("<table>\n<tbody>\n")
			for _, kv := range item {
				b.WriteString("<tr>\n<th>" + esc(kv.Name) + "</th>\n<td>" + strings.ReplaceAll(esc(kv.Value), "\n", "<br />") + "</td>\n</tr>\n")
			}
			b.WriteString("</tbody>\n</table>\n")
		}
		b.WriteString("</div>\n<div class=\"return-to-top\">\n<a href=\"#top\"><span aria-hidden=\"true\">&uarr; </span> Go to top</a>\n</div>\n")
	}
	b.WriteString("</body>\n</html>\n")
	return b.String()
}

func (s *Server) privacyExportsDir() string {
	return filepath.Join(s.cfg.UploadsDir, "wp-personal-data-exports")
}

// buildPersonalDataExport writes the export zip (index.html + export.json) into
// wp-personal-data-exports/ and records its name on the request (wp_privacy_generate_personal_data_export_file()).
func (s *Server) buildPersonalDataExport(c *gin.Context, r personalRequest) (personalRequest, []byte, error) {
	groups := s.personalDataGroups(c, r.Email)
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("index.html")
	_, _ = w.Write([]byte(personalDataExportHTML(r.Email, groups)))
	js, _ := json.MarshalIndent(map[string]any{"Personal Data Export for " + r.Email: groups}, "", "  ")
	w2, _ := zw.Create("export.json")
	_, _ = w2.Write(js)
	if err := zw.Close(); err != nil {
		return r, nil, err
	}
	dir := s.privacyExportsDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return r, nil, err
	}
	_ = os.WriteFile(filepath.Join(dir, "index.php"), []byte("<?php\n// Silence is golden.\n"), 0o644)
	if r.ExportFile != "" {
		_ = os.Remove(filepath.Join(dir, r.ExportFile))
	}
	sum := sha256.Sum256([]byte(r.Email + auth.RandomToken(16) + itoa64(time.Now().UnixNano())))
	r.ExportFile = "wp-personal-data-file-" + hex.EncodeToString(sum[:16]) + "-" + time.Now().UTC().Format("2006-01-02-15-04-05") + ".zip"
	if err := os.WriteFile(filepath.Join(dir, r.ExportFile), buf.Bytes(), 0o644); err != nil {
		return r, nil, err
	}
	s.updatePersonalRequest(c, r)
	return r, buf.Bytes(), nil
}

// downloadPersonalData streams the export zip to the administrator.
func (s *Server) downloadPersonalData(c *gin.Context, r personalRequest) {
	_, data, err := s.buildPersonalDataExport(c, r)
	if err != nil {
		c.String(http.StatusInternalServerError, err.Error())
		return
	}
	c.Header("Content-Disposition", "attachment; filename=wp-personal-data-file-"+time.Now().Format("2006-01-02")+".zip")
	c.Data(http.StatusOK, "application/zip", data)
}

// sendPersonalDataExport is wp_privacy_send_personal_data_export_email().
func (s *Server) sendPersonalDataExport(c *gin.Context, r personalRequest) (personalRequest, error) {
	r, _, err := s.buildPersonalDataExport(c, r)
	if err != nil {
		return r, err
	}
	site := s.settings.String("blogname")
	expires := wp.FormatDate(time.Now().Add(privacyExportExpiration).In(s.loc()), s.settings.String("date_format"))
	body := "Howdy,\n\nYour request for an export of personal data has been completed. You may\ndownload your personal data by clicking on the link below. For privacy\nand security, we will automatically delete the file on ###EXPIRATION###,\nso please download it before then.\n\n###LINK###\n\nRegards,\nAll at ###SITENAME###\n###SITEURL###\n"
	body = strings.NewReplacer("###EXPIRATION###", expires, "###LINK###", s.uploadURL(c, "wp-personal-data-exports/"+r.ExportFile),
		"###SITENAME###", site, "###SITEURL###", s.links(c).HomeURL("/")).Replace(body)
	s.sendMail(c, mail.Message{To: []string{r.Email}, Subject: "[" + site + "] Personal Data Export", Body: body})
	return r, nil
}

// erasePersonalData runs wp_comments_personal_data_eraser(): every comment by the address is anonymized
// with wp_anonymize_comment() and detached from the user account.
func (s *Server) erasePersonalData(c *gin.Context, email string) int {
	rows, err := s.store.Pool.Query(c, `SELECT id, author_ip FROM comments WHERE lower(author_email) = lower($1)`, email)
	if err != nil {
		return 0
	}
	type row struct {
		id int64
		ip string
	}
	var list []row
	for rows.Next() {
		var r row
		if rows.Scan(&r.id, &r.ip) == nil {
			list = append(list, r)
		}
	}
	rows.Close()
	n := 0
	for _, r := range list {
		if _, err := s.store.Pool.Exec(c, `UPDATE comments SET author_name = 'Anonymous', author_email = 'deleted@site.invalid', author_url = 'https://site.invalid',
			author_ip = $2, user_agent = '', user_id = NULL WHERE id = $1`, r.id, anonymizeIP(r.ip)); err == nil {
			n++
		}
	}
	s.purgeAll(c)
	return n
}

// anonymizeIP is wp_privacy_anonymize_ip(): the last octet of an IPv4, the last 80 bits of an IPv6.
func anonymizeIP(ip string) string {
	parsed := net.ParseIP(strings.TrimSpace(ip))
	if parsed == nil {
		return ""
	}
	if v4 := parsed.To4(); v4 != nil {
		return fmt.Sprintf("%d.%d.%d.0", v4[0], v4[1], v4[2])
	}
	masked := parsed.Mask(net.CIDRMask(48, 128))
	if masked.Equal(net.IPv6zero) {
		return "::"
	}
	return masked.String()
}

// notifyErasureFulfilled is _wp_privacy_send_erasure_fulfillment_notification().
func (s *Server) notifyErasureFulfilled(c *gin.Context, r personalRequest) {
	site := s.settings.String("blogname")
	body := "Howdy,\n\nYour request to erase your personal data on ###SITENAME### has been completed.\n\nIf you have any follow-up questions or concerns, please contact the site administrator.\n\n"
	if id := int64(s.settings.Int("wp_page_for_privacy_policy")); id > 0 {
		if p, err := s.store.GetPost(c, id); err == nil && p.Status == "publish" {
			body += "For more information, you can also read our privacy policy: ###PRIVACY_POLICY_URL###\n\n"
			body = strings.ReplaceAll(body, "###PRIVACY_POLICY_URL###", s.links(c).Permalink(p))
		}
	}
	body += "Regards,\nAll at ###SITENAME###\n###SITEURL###\n"
	body = strings.NewReplacer("###SITENAME###", site, "###SITEURL###", s.links(c).HomeURL("/")).Replace(body)
	s.sendMail(c, mail.Message{To: []string{r.Email}, Subject: "[" + site + "] Erasure Request Fulfilled", Body: body})
}

// Privacy Policy Guide (options-privacy.php?tab=policyguide) ---------------------------

type policySection struct {
	Name string
	HTML template.HTML
}

// privacyPolicyGuide is privacy-policy-guide.php: WordPress' suggested policy text (the same content
// the "Create" button puts into a new Privacy Policy page) with the Policies accordion.
func (s *Server) privacyPolicyGuide(c *gin.Context) {
	if !currentUser(c).Can("manage_privacy_options", nil) && !currentUser(c).Can("manage_options", nil) {
		c.String(http.StatusForbidden, "Sorry, you are not allowed to manage privacy options on this site.")
		return
	}
	content := strings.ReplaceAll(privacyPolicyContent, "SITE_URL", s.links(c).HomeURL("/"))
	sections := []policySection{{Name: "WordPress", HTML: template.HTML(blocks.Render(c.Request.Context(), content))}}
	s.adminPage(c, "options-privacy", "Privacy Policy Guide", "privacy-policy-guide-php privacy-settings", "admin/privacy-policy-guide", map[string]any{"Sections": sections})
}
