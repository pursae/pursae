package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"wpgo/internal/wp"
)

// templates holds the admin and theme template sets. Each page is a uniquely named
// template ("admin/dashboard", "theme/single"), so one set per area is enough.
type templates struct {
	s      *Server
	mu     sync.Mutex
	admin  *template.Template
	themes map[string]*template.Template // by theme slug
}

func newTemplates(s *Server) (*templates, error) {
	t := &templates{s: s}
	if err := t.load(); err != nil {
		return nil, err
	}
	return t, nil
}

func (t *templates) load() error {
	admin, err := parseTree(t.s.web, "admin/templates", t.funcs())
	if err != nil {
		return fmt.Errorf("admin templates: %w", err)
	}
	sets := map[string]*template.Template{}
	for _, th := range allThemes() {
		if th.Custom {
			continue
		}
		if th.Package != nil { // a theme package: a broken one is logged, never a reason to fail
			set, err := parsePackageTemplates(t.s.web, "themes/"+th.Slug+"/templates", t.funcs())
			if err != nil {
				log.Printf("theme package %s templates: %v", th.Slug, err)
				continue
			}
			sets[th.Slug] = set
			continue
		}
		set, err := parseTree(t.s.web, "themes/"+th.Slug+"/templates", t.funcs())
		if err != nil {
			return fmt.Errorf("theme %s templates: %w", th.Slug, err)
		}
		sets[th.Slug] = set
	}
	t.mu.Lock()
	t.admin, t.themes = admin, sets
	t.mu.Unlock()
	return nil
}

// gweShell is the template set Golementor theme parts and saved themes render through: Hello
// Golementor's (theme_hellogowebeditor.go).
func (t *templates) gweShell() *template.Template {
	return t.getTheme(true, helloThemeSlug)
}

func parseTree(fsys fs.FS, dir string, funcs template.FuncMap) (*template.Template, error) {
	root := template.New("").Funcs(funcs)
	err := fs.WalkDir(fsys, dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".html") {
			return err
		}
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		if _, err := root.New(p).Parse(string(b)); err != nil {
			return err
		}
		return nil
	})
	return root, err
}

func (t *templates) get(theme bool) *template.Template {
	return t.getTheme(theme, defaultTheme)
}

func (t *templates) getTheme(theme bool, slug string) *template.Template {
	if t.s.cfg.WebDir != "" {
		if err := t.load(); err != nil {
			log.Printf("template reload: %v", err)
		}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if theme {
		if set := t.themes[slug]; set != nil {
			return set
		}
		if th := themeBySlug(slug); th != nil && th.Custom { // Golementor saved theme (live preview)
			return t.themes[helloThemeSlug]
		}
		return t.themes[defaultTheme]
	}
	return t.admin
}

func (s *Server) renderAdminTemplate(c *gin.Context, status int, name string, data any) {
	s.renderTemplate(c, status, s.tmpl.get(false), name, data)
}

func (s *Server) renderThemeTemplate(c *gin.Context, theme string, status int, name string, data any) {
	s.renderTemplate(c, status, s.tmpl.getTheme(true, theme), name, data)
}

func (s *Server) renderTemplate(c *gin.Context, status int, set *template.Template, name string, data any) {
	var buf bytes.Buffer
	if err := set.ExecuteTemplate(&buf, name, data); err != nil {
		log.Printf("render %s: %v", name, err)
		lastRenderError.Store(err.Error())
		c.String(http.StatusInternalServerError, "Template error")
		return
	}
	c.Data(status, "text/html; charset=UTF-8", buf.Bytes())
}

func (t *templates) funcs() template.FuncMap {
	s := t.s
	m := themeTemplateFuncs() // nav_menu, dynamic_sidebar, theme_mod, … (theme_package.go)
	for k, f := range t.coreFuncs(s) {
		m[k] = f
	}
	return m
}

func (t *templates) coreFuncs(s *Server) template.FuncMap {
	return template.FuncMap{
		"safe":    func(v string) template.HTML { return template.HTML(v) },
		"safeURL": func(v string) template.URL { return template.URL(v) },
		"safeAttr": func(v string) template.HTMLAttr {
			return template.HTMLAttr(v)
		},
		"json": func(v any) template.JS {
			b, _ := json.Marshal(v)
			return template.JS(b)
		},
		"date": func(t time.Time, format string) string {
			if format == "" {
				format = s.settings.String("date_format")
			}
			return wp.FormatDate(t.In(s.loc()), format)
		},
		"dateFormat": func() string { return s.settings.String("date_format") },
		"timeFormat": func() string { return s.settings.String("time_format") },
		"plural": func(n int, one, many string) string {
			if n == 1 {
				return strings.Replace(one, "%s", "1", 1)
			}
			return strings.Replace(many, "%s", strconv.Itoa(n), 1)
		},
		"num": func(n any) string {
			switch v := n.(type) {
			case int64:
				return formatNumber(int(v))
			case int:
				return formatNumber(v)
			}
			return fmt.Sprint(n)
		},
		"add": func(a, b int) int { return a + b },
		"sub": func(a, b int) int { return a - b },
		"seq": func(a, b int) []int {
			var out []int
			for i := a; i <= b; i++ {
				out = append(out, i)
			}
			return out
		},
		"query":       func(v string) string { return url.QueryEscape(v) },
		"setting":     func(key string) any { return s.settings.Get(key) },
		"settingStr":  func(key string) string { return s.settings.String(key) },
		"settingBool": func(key string) bool { return s.settings.Bool(key) },
		"settingInt":  func(key string) int { return s.settings.Int(key) },
		"settingID":   func(key string) int64 { return int64(s.settings.Int(key)) },
		"repeat":      strings.Repeat,
		"contains":    strings.Contains,
		"lower":       strings.ToLower,
		"texturize":   func(v string) template.HTML { return template.HTML(wp.Texturize(wp.EscText(v))) },
		"dict": func(kv ...any) map[string]any {
			m := map[string]any{}
			for i := 0; i+1 < len(kv); i += 2 {
				m[fmt.Sprint(kv[i])] = kv[i+1]
			}
			return m
		},
		"version": func() string { return Version },
		"list":    func(v ...string) []string { return v },
		"split":   strings.Split,
		"checklist": func(it checklistItem, idx int, disabled ...bool) template.HTML {
			return checklistHTML(it, idx, disabled...)
		},
		"ucfirst": func(v string) string {
			if v == "" {
				return v
			}
			return strings.ToUpper(v[:1]) + v[1:]
		},
		"trimSlash": func(v string) string { return strings.TrimRight(v, "/") },
	}
}

// formatNumber is number_format_i18n() for English: thousands separated by commas.
func formatNumber(n int) string {
	s := strconv.Itoa(n)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var out []byte
	for i := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, s[i])
	}
	if neg {
		return "-" + string(out)
	}
	return string(out)
}

func urlEscape(v string) string { return url.QueryEscape(v) }
