package server

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"net/mail"
	"net/url"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"wpgo/internal/store"
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
		if theme.Package == nil && o.overridden(themeFileWebPath(theme.Slug, file)) { // a package's files are the originals
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

type personalRequest struct {
	ID        int64
	Email     string
	Type      string
	Status    string
	CreatedAt time.Time
}

func (s *Server) personalRequests(c *gin.Context, typ string) []personalRequest {
	raw, _ := s.settings.Get("wpgo_privacy_requests").([]any)
	var out []personalRequest
	for _, r := range raw {
		m, _ := r.(map[string]any)
		if m["type"] != typ {
			continue
		}
		pr := personalRequest{Email: fmt.Sprint(m["email"]), Type: typ, Status: fmt.Sprint(m["status"])}
		if f, ok := m["id"].(float64); ok {
			pr.ID = int64(f)
		}
		pr.CreatedAt, _ = time.Parse(time.RFC3339, fmt.Sprint(m["created"]))
		out = append(out, pr)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out
}

func (s *Server) savePersonalRequest(c *gin.Context, typ, email, status string, id int64) {
	raw, _ := s.settings.Get("wpgo_privacy_requests").([]any)
	var next int64 = 1
	found := false
	for _, r := range raw {
		m, _ := r.(map[string]any)
		if f, ok := m["id"].(float64); ok {
			if int64(f) >= next {
				next = int64(f) + 1
			}
			if int64(f) == id {
				m["status"] = status
				found = true
			}
		}
	}
	if !found {
		raw = append(raw, map[string]any{"id": next, "type": typ, "email": email, "status": status, "created": time.Now().UTC().Format(time.RFC3339)})
	}
	_ = s.settings.Set(c, map[string]any{"wpgo_privacy_requests": raw})
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
		if c.Request.Method == http.MethodPost {
			if !s.checkNonce(c, "personal-data-request") {
				s.denyNonce(c)
				return
			}
			email := strings.TrimSpace(c.PostForm("username_or_email_for_privacy_request"))
			if u, err := s.store.UserByLoginOrEmail(c, email); err == nil {
				email = u.Email
			}
			if _, err := mail.ParseAddress(email); err != nil {
				notices = append(notices, notice{Type: "error", Message: "Unable to add this request. A valid email address or username must be supplied."})
			} else {
				s.savePersonalRequest(c, typ, email, "request-confirmed", 0)
				notices = append(notices, notice{Type: "success", Message: "Request added successfully.", Dismissible: true})
			}
		}
		if id := c.Query("process"); id != "" && s.checkNonce(c, "personal-data-request") {
			for _, r := range s.personalRequests(c, typ) {
				if fmt.Sprint(r.ID) != id {
					continue
				}
				if typ == "export_personal_data" {
					s.downloadPersonalData(c, r.Email)
					s.savePersonalRequest(c, typ, r.Email, "request-completed", r.ID)
					return
				}
				n := s.erasePersonalData(c, r.Email)
				s.savePersonalRequest(c, typ, r.Email, "request-completed", r.ID)
				notices = append(notices, notice{Type: "success", Message: htmlf("Personal data was erased: %d comments anonymized.", n), Dismissible: true})
			}
		}
		s.adminPage(c, screen, title, screen+"-php privacy-settings", "admin/privacy-tool", map[string]any{
			"Type": typ, "Title": title, "Requests": s.personalRequests(c, typ), "Nonce": s.nonce(c, "personal-data-request"), "Screen": screen}, notices...)
	}
}

func htmlf(format string, a ...any) template.HTML {
	return template.HTML(template.HTMLEscapeString(fmt.Sprintf(format, a...)))
}

// downloadPersonalData sends a zip with the person's data, like wp_privacy_generate_personal_data_export_file().
func (s *Server) downloadPersonalData(c *gin.Context, email string) {
	data := map[string]any{"email": email}
	if u, err := s.store.UserByLoginOrEmail(c, email); err == nil {
		data["user"] = map[string]any{"login": u.Login, "display_name": u.DisplayName, "url": u.URL, "registered": u.RegisteredAt, "profile": u.Profile}
	}
	comments, _, _ := s.store.QueryComments(c, store.CommentQuery{Status: "approved", Search: email, PerPage: 10000})
	var cs []map[string]any
	for _, cm := range comments {
		if !strings.EqualFold(cm.AuthorEmail, email) {
			continue
		}
		cs = append(cs, map[string]any{"author": cm.AuthorName, "email": cm.AuthorEmail, "url": cm.AuthorURL, "ip": cm.AuthorIP,
			"user_agent": cm.UserAgent, "date": cm.CreatedAt, "content": cm.Content, "post_id": cm.PostID})
	}
	data["comments"] = cs
	js, _ := json.MarshalIndent(data, "", "  ")
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("export.json")
	_, _ = w.Write(js)
	w2, _ := zw.Create("index.html")
	_, _ = fmt.Fprintf(w2, "<!DOCTYPE html><html><head><meta charset=\"utf-8\"><title>Personal Data Export</title></head><body><h1>Personal Data Export for %s</h1><pre>%s</pre></body></html>",
		template.HTMLEscapeString(email), template.HTMLEscapeString(string(js)))
	_ = zw.Close()
	c.Header("Content-Disposition", "attachment; filename=wp-personal-data-file-"+time.Now().Format("20060102")+".zip")
	c.Data(http.StatusOK, "application/zip", buf.Bytes())
}

// erasePersonalData anonymizes the person's comments (wp_comments_personal_data_eraser()).
func (s *Server) erasePersonalData(c *gin.Context, email string) int {
	tag, err := s.store.Pool.Exec(c, `UPDATE comments SET author_name = 'Anonymous', author_email = 'deleted@site.invalid',
		author_url = 'https://site.invalid', author_ip = '192.168.0.0', user_agent = '' WHERE lower(author_email) = lower($1)`, email)
	if err != nil {
		return 0
	}
	s.purgeAll(c)
	return int(tag.RowsAffected())
}
