package server

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/gin-gonic/gin"

	"wpgo/internal/store"
)

// Theme files edited in Appearance › Theme File Editor are saved under <data>/themes/<slug>/… and
// layered over the copies embedded in the binary. Nothing has to be compiled: theme templates are Go
// html/template files parsed at run time and CSS/JS are served as they are, so an edit takes effect
// as soon as it is saved. Only a theme's Go glue (theme_*.go) is compiled into the binary.

// overlayFS serves "themes/…" paths from dir when a file exists there, and everything else (and every
// directory listing) from base.
type overlayFS struct {
	base fs.FS
	dir  string
}

func (o overlayFS) Open(name string) (fs.File, error) {
	if o.dir != "" {
		if rel, ok := strings.CutPrefix(name, "themes/"); ok && fs.ValidPath(name) {
			if f, err := os.Open(filepath.Join(o.dir, filepath.FromSlash(rel))); err == nil {
				if st, err := f.Stat(); err == nil {
					if !st.IsDir() {
						return f, nil
					}
					// A directory the binary does not have is a theme package's: list it from disk.
					if _, err := fs.Stat(o.base, name); err != nil {
						return f, nil
					}
				}
				f.Close()
			}
		}
	}
	return o.base.Open(name)
}

// overridden reports whether a web path ("themes/<slug>/assets/style.css") has an edited copy.
func (o overlayFS) overridden(name string) bool {
	rel, ok := strings.CutPrefix(name, "themes/")
	if !ok || o.dir == "" || !fs.ValidPath(name) {
		return false
	}
	st, err := os.Stat(filepath.Join(o.dir, filepath.FromSlash(rel)))
	return err == nil && !st.IsDir()
}

// themeOverrideDir is where edited theme files live (WPGO_THEMES_DIR, default <data>/themes).
func themeOverrideDir(uploadsDir string) string {
	if d := os.Getenv("WPGO_THEMES_DIR"); d != "" {
		return d
	}
	return filepath.Join(filepath.Dir(filepath.Clean(uploadsDir)), "themes")
}

// fileEditingDisabled is DISALLOW_FILE_EDIT.
func fileEditingDisabled() bool {
	v := strings.ToLower(os.Getenv("WPGO_DISALLOW_FILE_EDIT"))
	return v == "1" || v == "true" || v == "yes"
}

func (s *Server) overlay() (overlayFS, bool) {
	o, ok := s.web.(overlayFS)
	return o, ok
}

// themeFileWebPath maps an editor path ("style.css", "assets/js/x.js", "templates/single.html") to the
// web filesystem path. The theme root a browser sees is assets/; templates/ holds the Go templates.
func themeFileWebPath(slug, file string) string {
	if strings.HasPrefix(file, "templates/") {
		return "themes/" + slug + "/" + file
	}
	return "themes/" + slug + "/assets/" + file
}

var themeEditableExt = map[string]bool{".html": true, ".css": true, ".js": true, ".json": true, ".txt": true, ".svg": true, ".md": true}

// lastRenderError remembers the most recent template execution error for the save check.
var lastRenderError atomic.Value

var themeSaveMu sync.Mutex

type themeFileError struct{ code, message string }

func (e *themeFileError) Error() string { return e.message }

// saveThemeFile is wp_edit_theme_plugin_file() for themes: write, re-parse, render-check, roll back on failure.
func (s *Server) saveThemeFile(c *gin.Context, slug, file, content string) error {
	if fileEditingDisabled() {
		return &themeFileError{"disallowed", "File editing is disabled on this site (WPGO_DISALLOW_FILE_EDIT)."}
	}
	o, ok := s.overlay()
	if !ok {
		return &themeFileError{"unavailable", "Theme file editing is not available."}
	}
	th := s.installedTheme(slug)
	if th == nil || th.Custom {
		return &themeFileError{"bad_theme_path", "Sorry, that theme cannot be edited here."}
	}
	clean := path.Clean("/" + file)[1:]
	if clean == "" || clean != file || !themeEditableExt[path.Ext(clean)] {
		return &themeFileError{"bad_file", "Sorry, that file cannot be edited."}
	}
	webPath := themeFileWebPath(slug, clean)
	if _, err := fs.Stat(s.web, webPath); err != nil {
		return &themeFileError{"missing_file", "File does not exist! Please double check the name and try again."}
	}

	themeSaveMu.Lock()
	defer themeSaveMu.Unlock()

	target := filepath.Join(o.dir, filepath.FromSlash(strings.TrimPrefix(webPath, "themes/")))
	prev, hadPrev := []byte(nil), false
	if b, err := os.ReadFile(target); err == nil {
		prev, hadPrev = b, true
	}
	restore := func() {
		if hadPrev {
			_ = os.WriteFile(target, prev, 0o644)
		} else {
			_ = os.Remove(target)
		}
	}
	// Saving the original content back simply removes the edited copy.
	if orig, err := fs.ReadFile(o.base, webPath); err == nil && bytes.Equal(orig, []byte(content)) {
		if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
			return &themeFileError{"unable_to_write", err.Error()}
		}
	} else {
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return &themeFileError{"unable_to_write", "Unable to write to file: " + err.Error()}
		}
		tmp := target + ".tmp"
		if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
			return &themeFileError{"unable_to_write", "Unable to write to file: " + err.Error()}
		}
		if err := os.Rename(tmp, target); err != nil {
			os.Remove(tmp)
			return &themeFileError{"unable_to_write", "Unable to write to file: " + err.Error()}
		}
	}

	if strings.HasPrefix(clean, "templates/") {
		if err := s.tmpl.load(); err != nil {
			restore()
			_ = s.tmpl.load()
			return &themeFileError{"template_error", "Your change was not saved because the template does not parse: " + err.Error()}
		}
		if err := s.themeRenderCheck(c, th); err != nil {
			restore()
			_ = s.tmpl.load()
			return &themeFileError{"render_error", "Your change was rolled back because the site failed to render with it: " + err.Error()}
		}
	}
	s.purgeAll(c)
	return nil
}

// themeRenderCheck is WordPress's post-edit loopback "scrape": render a few pages in-process with the
// edited theme and fail if any of them errors.
func (s *Server) themeRenderCheck(c *gin.Context, th *Theme) error {
	urls := []string{"/", "/?s=wpgo-render-check", "/wpgo-render-check-404/"}
	if posts, _, err := s.store.QueryPosts(c, store.PostQuery{Types: []string{"post"}, Statuses: []string{"publish"}, PerPage: 1, Page: 1}); err == nil && len(posts) > 0 {
		urls = append(urls, "/?p="+itoa64(posts[0].ID))
	}
	if pages, _, err := s.store.QueryPosts(c, store.PostQuery{Types: []string{"page"}, Statuses: []string{"publish"}, PerPage: 1, Page: 1}); err == nil && len(pages) > 0 {
		urls = append(urls, "/?page_id="+itoa64(pages[0].ID))
	}
	preview := th.Slug != s.activeTheme().Slug
	for _, u := range urls {
		if preview {
			sep := "?"
			if strings.Contains(u, "?") {
				sep = "&"
			}
			u += sep + "theme=" + th.Slug
		}
		req := httptest.NewRequest(http.MethodGet, u, nil).WithContext(c.Request.Context())
		req.Host = c.Request.Host
		for _, ck := range c.Request.Cookies() {
			req.AddCookie(ck)
		}
		lastRenderError.Store("")
		rec := httptest.NewRecorder()
		s.engine.ServeHTTP(rec, req)
		if rec.Code >= 500 || strings.Contains(rec.Body.String(), "Template error") {
			msg, _ := lastRenderError.Load().(string)
			if msg == "" {
				msg = fmt.Sprintf("%s returned %d", u, rec.Code)
			}
			return errors.New(msg)
		}
	}
	return nil
}

// restoreThemeFile drops the edited copy of a file. A package theme's files are the originals, so
// there is nothing to restore them to.
func (s *Server) restoreThemeFile(c *gin.Context, slug, file string) error {
	o, ok := s.overlay()
	if th := s.installedTheme(slug); !ok || th == nil || th.Package != nil {
		return errors.New("Sorry, that theme cannot be edited here.")
	}
	clean := path.Clean("/" + file)[1:]
	if clean != file || clean == "" {
		return errors.New("Sorry, that file cannot be edited.")
	}
	webPath := themeFileWebPath(slug, clean)
	if err := os.Remove(filepath.Join(o.dir, filepath.FromSlash(strings.TrimPrefix(webPath, "themes/")))); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if strings.HasPrefix(clean, "templates/") {
		_ = s.tmpl.load()
	}
	s.purgeAll(c)
	return nil
}

// themeFileAjax answers admin-ajax action=edit-theme-plugin-file (theme-plugin-editor.js) for themes.
func (s *Server) themeFileAjax(c *gin.Context, action string) bool {
	if action != "edit-theme-plugin-file" {
		return false
	}
	fail := func(code, msg string) {
		c.JSON(http.StatusOK, gin.H{"success": false, "data": gin.H{"code": code, "message": msg}})
	}
	slug, file := c.PostForm("theme"), c.PostForm("file")
	if slug == "" {
		fail("bad_theme_path", "Plugins are signed WebAssembly packages and cannot be edited here.")
		return true
	}
	if !currentUser(c).Can("edit_themes", nil) {
		fail("unauthorized", "Sorry, you are not allowed to edit templates for this site.")
		return true
	}
	if !s.checkNonceValue(c, c.PostForm("nonce"), "edit-theme_"+slug+"_"+file) {
		fail("nonce_failure", "The link you followed has expired.")
		return true
	}
	content, ok := c.GetPostForm("newcontent")
	if !ok {
		fail("missing_content", "Missing file content.")
		return true
	}
	if err := s.saveThemeFile(c, slug, file, content); err != nil {
		var te *themeFileError
		if errors.As(err, &te) {
			fail(te.code, te.message)
		} else {
			fail("save_error", err.Error())
		}
		return true
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{"message": "File edited successfully."}})
	return true
}
