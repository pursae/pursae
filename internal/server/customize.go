package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"html/template"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"wpgo/internal/media"
	"wpgo/internal/store"
)

// Appearance › Customize (customize.php). A lightweight Customizer: a left pane of sections with
// controls and a live preview of the site on the right. It is not a port of WP_Customize_Manager:
//
//   - Settings are flat ids: core options (blogname, blogdescription, site_icon, show_on_front,
//     page_on_front, page_for_posts), custom_css, and "mod:<name>" for theme mods.
//   - Theme mods live in the theme_mods_<slug> option (a JSON object), like WordPress; Additional
//     CSS is kept per theme in the same object under "custom_css".
//   - The preview iframe loads the site with ?customize_changeset_uuid=<id>. The id names a
//     server-side changeset (unsaved, sanitized values) owned by the user who opened the Customizer;
//     only that user, logged in with edit_theme_options, sees the overrides, and those responses
//     are never cached.

const czParam = "customize_changeset_uuid"

// czChangeset is an unpublished set of values used by the preview.
type czChangeset struct {
	UserID  int64
	Theme   string
	Options map[string]any // sanitized core options
	Mods    map[string]any // the theme's full theme mods with the changes applied
	Expires time.Time
}

var czChangesets = struct {
	sync.Mutex
	m map[string]*czChangeset
}{m: map[string]*czChangeset{}}

func newChangesetID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func jsonUnmarshalString(s string, v any) error { return json.Unmarshal([]byte(s), v) }

// czCoreOptions are the options the Customizer may write.
var czCoreOptions = []string{"blogname", "blogdescription", "site_icon", "show_on_front", "page_on_front", "page_for_posts"}

var czTagRe = regexp.MustCompile(`<[a-zA-Z/!?][^>]*>?`)

// czSanitize validates the submitted values (setting id → raw value) for a theme. It returns the
// options to write and the theme's complete theme mods after the changes.
func (s *Server) czSanitize(ctx context.Context, t *Theme, values map[string]string) (map[string]any, map[string]any, error) {
	opts := map[string]any{}
	mods := s.themeMods(t.Slug)
	spec := czSpecs[t.Slug]
	image := func(raw string) (int64, error) {
		id, _ := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
		if id <= 0 {
			return 0, nil
		}
		a, err := s.store.GetAttachment(ctx, id)
		if err != nil || !strings.HasPrefix(a.Mime, "image/") {
			return 0, errors.New("Invalid value.")
		}
		return id, nil
	}
	for id, raw := range values {
		switch id {
		case "blogname", "blogdescription":
			opts[id] = strings.TrimSpace(czTagRe.ReplaceAllString(raw, ""))
		case "site_icon":
			n, err := image(raw)
			if err != nil {
				return nil, nil, err
			}
			opts[id] = n
		case "show_on_front":
			if raw != "posts" && raw != "page" {
				return nil, nil, errors.New("Invalid value.")
			}
			opts[id] = raw
		case "page_on_front", "page_for_posts":
			n, _ := strconv.ParseInt(raw, 10, 64)
			if n > 0 {
				if p, err := s.store.GetPost(ctx, n); err != nil || p.Type != "page" {
					return nil, nil, errors.New("Invalid value.")
				}
			} else {
				n = 0
			}
			opts[id] = n
		case "custom_css":
			css := czTagRe.ReplaceAllString(raw, "")
			if strings.TrimSpace(css) == "" {
				delete(mods, "custom_css")
			} else {
				mods["custom_css"] = css
			}
		case "mod:custom_logo":
			n, err := image(raw)
			if err != nil {
				return nil, nil, err
			}
			if n == 0 {
				delete(mods, "custom_logo")
			} else {
				mods["custom_logo"] = n
			}
		default:
			name, ok := strings.CutPrefix(id, "mod:")
			if !ok || spec == nil {
				return nil, nil, fmt.Errorf("Unknown setting %q.", id)
			}
			ctl := spec.control(name)
			if ctl == nil {
				return nil, nil, fmt.Errorf("Unknown setting %q.", id)
			}
			if raw == "" {
				delete(mods, name)
				continue
			}
			switch ctl.Type {
			case "select", "radio":
				valid := false
				for _, ch := range ctl.Choices {
					valid = valid || ch[0] == raw
				}
				if !valid {
					return nil, nil, errors.New("Invalid value.")
				}
				mods[name] = raw
			case "color":
				c := sanitizeHex(raw)
				if c == "" {
					return nil, nil, errors.New("Invalid value.")
				}
				if name == "background_color" {
					c = strings.TrimPrefix(c, "#") // sanitize_hex_color_no_hash()
				}
				mods[name] = c
			case "hue":
				n, err := strconv.Atoi(raw)
				if err != nil {
					return nil, nil, errors.New("Invalid value.")
				}
				if n < 0 {
					n = -n
				}
				mods[name] = n % 360
			case "json":
				if v := sanitizeTT20Colors(raw); v != nil {
					mods[name] = v
				} else {
					delete(mods, name)
				}
			case "checkbox":
				mods[name] = raw == "1" || raw == "true" || raw == "on"
			case "image": // an attachment id (theme packages)
				n, err := image(raw)
				if err != nil {
					return nil, nil, err
				}
				if n == 0 {
					delete(mods, name)
				} else {
					mods[name] = n
				}
			case "number":
				n, err := strconv.Atoi(strings.TrimSpace(raw))
				if err != nil {
					return nil, nil, errors.New("Invalid value.")
				}
				mods[name] = min(max(n, ctl.Min), ctl.Max)
			case "text", "textarea":
				if ctl.Sanitize != nil {
					raw = ctl.Sanitize(raw)
				} else {
					raw = czTagRe.ReplaceAllString(raw, "")
				}
				if raw = strings.TrimSpace(raw); raw == "" {
					delete(mods, name)
				} else {
					mods[name] = raw
				}
			}
		}
	}
	// pageOnFrontError: the homepage and the posts page must differ.
	show, front, posts := s.settings.String("show_on_front"), int64(s.settings.Int("page_on_front")), int64(s.settings.Int("page_for_posts"))
	if v, ok := opts["show_on_front"].(string); ok {
		show = v
	}
	if v, ok := opts["page_on_front"].(int64); ok {
		front = v
	}
	if v, ok := opts["page_for_posts"].(int64); ok {
		posts = v
	}
	if show == "page" && front != 0 && front == posts {
		return nil, nil, errors.New("Homepage and posts page must be different.")
	}
	// JSON round trip so preview values have the same types as stored ones.
	b, _ := json.Marshal(mods)
	mods = map[string]any{}
	_ = json.Unmarshal(b, &mods)
	return opts, mods, nil
}

// Screen ----------------------------------------------------------------------------

type czControlView struct {
	czControl
	Setting, Value, DefaultValue string
	// Image and CanUpload serve image controls (theme packages): the chosen attachment.
	Image     czImageView
	CanUpload bool
}

// czCheckbox is a checkbox control's value on the screen: "1" or "0".
func czCheckbox(mods map[string]any, ctl czControl) string {
	if modBool(mods, ctl.ID, ctl.Default == "1") {
		return "1"
	}
	return "0"
}

type czSectionView struct {
	czSection
	Controls []czControlView
}

type czImageView struct {
	ID       int64
	URL      string
	Portrait bool
}

type czPageData struct {
	Title, SiteName, HomeURL string
	Theme                    *Theme
	Nonce, UUID              string
	PreviewURL, ReturnURL    string
	Values                   map[string]string
	SiteIcon, Logo           czImageView
	HasLogo                  bool
	Pages                    []*store.Post
	Colors                   []czControlView
	Identity                 []czControlView // theme controls in Site Identity
	Sections                 []*czSectionView
	PostMessage              []string
	MenuLocations            []czMenuLocation
	WidgetAreas              []WidgetArea
	CanUpload, CanSwitch     bool
	ThemeScript              template.JS
	ThemeScripts             []string
	Styles, HeadScripts      template.HTML
	FooterScripts, BodyHTML  template.HTML
}

type czMenuLocation struct {
	Name, Menu string
}

func (s *Server) czImage(ctx context.Context, id int64) czImageView {
	if id <= 0 {
		return czImageView{}
	}
	a, err := s.store.GetAttachment(ctx, id)
	if err != nil {
		return czImageView{}
	}
	src := s.uploadURL(ctx, a.FilePath)
	if v := a.Variant("medium"); v != nil {
		src = s.uploadURL(ctx, v.FilePath)
	}
	return czImageView{ID: id, URL: src, Portrait: a.Height > a.Width}
}

func themeSupportsLogo(t *Theme) bool {
	if t.Supports != nil && t.Supports["custom-logo"] != nil {
		return true
	}
	for _, tag := range t.Tags {
		if tag == "Custom Logo" {
			return true
		}
	}
	return false
}

// customizeReturn is the Close link: ?return=, else the admin page the user came from.
func customizeReturn(c *gin.Context) string {
	ok := func(p string) bool {
		return strings.HasPrefix(p, "/wp-admin") && !strings.HasPrefix(p, "/wp-admin/customize") && !strings.HasPrefix(p, "//")
	}
	if r := c.Query("return"); r != "" {
		if u, err := url.Parse(r); err == nil && (u.Host == "" || u.Host == c.Request.Host) && ok(u.Path) {
			return u.RequestURI()
		}
	}
	if r, err := url.Parse(c.Request.Referer()); err == nil && r.Host == c.Request.Host && ok(r.Path) {
		return r.RequestURI()
	}
	return "/wp-admin/themes"
}

// customizeScreen renders /wp-admin/customize.
func (s *Server) customizeScreen(c *gin.Context) {
	u := currentUser(c)
	if !u.Can("edit_theme_options", nil) {
		c.String(http.StatusForbidden, "Sorry, you are not allowed to customize this site.")
		return
	}
	ctx := c.Request.Context()
	t := s.activeTheme()
	mods := s.themeMods(t.Slug)
	lb := s.links(ctx)

	uuid := newChangesetID()
	czChangesets.Lock()
	for k, cs := range czChangesets.m {
		if time.Now().After(cs.Expires) {
			delete(czChangesets.m, k)
		}
	}
	czChangesets.m[uuid] = &czChangeset{UserID: u.ID, Theme: t.Slug, Mods: mods, Options: map[string]any{}, Expires: time.Now().Add(12 * time.Hour)}
	czChangesets.Unlock()

	preview := "/"
	if p := c.Query("url"); strings.HasPrefix(p, "/") && !strings.HasPrefix(p, "//") && !strings.HasPrefix(p, "/wp-admin") {
		preview = p
	}
	d := &czPageData{Title: "Customize: " + t.Name, SiteName: s.settings.String("blogname"), HomeURL: lb.HomeURL("/"), Theme: t,
		Nonce: s.nonce(c, "customize"), UUID: uuid, PreviewURL: preview, ReturnURL: customizeReturn(c),
		HasLogo: themeSupportsLogo(t), CanUpload: u.Can("upload_files", nil), CanSwitch: u.Can("switch_themes", nil)}
	d.Values = map[string]string{
		"blogname":        s.settings.String("blogname"),
		"blogdescription": s.settings.String("blogdescription"),
		"site_icon":       strconv.Itoa(s.settings.Int("site_icon")),
		"show_on_front":   s.settings.String("show_on_front"),
		"page_on_front":   strconv.Itoa(s.settings.Int("page_on_front")),
		"page_for_posts":  strconv.Itoa(s.settings.Int("page_for_posts")),
		"custom_css":      modString(mods, "custom_css", ""),
	}
	d.SiteIcon = s.czImage(ctx, int64(s.settings.Int("site_icon")))
	if d.HasLogo {
		d.Values["mod:custom_logo"] = strconv.Itoa(modInt(mods, "custom_logo", 0))
		d.Logo = s.czImage(ctx, int64(modInt(mods, "custom_logo", 0)))
	}
	if spec := czSpecs[t.Slug]; spec != nil {
		sections := map[string]*czSectionView{}
		for _, sec := range spec.Sections {
			d.Sections = append(d.Sections, &czSectionView{czSection: sec})
			sections[sec.ID] = d.Sections[len(d.Sections)-1]
		}
		for _, ctl := range spec.Controls {
			def, val := ctl.Default, modString(mods, ctl.ID, ctl.Default)
			if ctl.Type == "checkbox" {
				val = czCheckbox(mods, ctl)
			}
			if ctl.Type == "image" {
				val = strconv.Itoa(modInt(mods, ctl.ID, 0))
			}
			d.Values["mod:"+ctl.ID] = val
			cv := czControlView{czControl: ctl, Setting: "mod:" + ctl.ID, Value: val, DefaultValue: def, CanUpload: d.CanUpload}
			if ctl.Type == "image" {
				cv.Image = s.czImage(ctx, int64(modInt(mods, ctl.ID, 0)))
			}
			if ctl.Section == "title_tagline" {
				d.Identity = append(d.Identity, cv)
			} else if sec := sections[ctl.Section]; sec != nil {
				sec.Controls = append(sec.Controls, cv)
			}
		}
		d.PostMessage = spec.PostMessage
		for _, ctl := range spec.Colors {
			def := ctl.Default
			switch {
			case ctl.ID == "background_color" && spec.BackgroundDefault != nil:
				def = "#" + spec.BackgroundDefault(mods)
			case t.Slug == "twentysixteen" && ctl.Type == "color":
				scheme := tt16Scheme(mods)
				def = scheme[map[string]int{"page_background_color": 1, "link_color": 2, "main_text_color": 3, "secondary_text_color": 4}[ctl.ID]]
			}
			val := def
			switch ctl.Type {
			case "json":
				val = ""
				if m, ok := mods[ctl.ID]; ok {
					b, _ := json.Marshal(m)
					val = string(b)
				}
			case "color":
				if v := modString(mods, ctl.ID, ""); v != "" {
					val = sanitizeHex(v)
				}
			case "checkbox":
				val = czCheckbox(mods, ctl)
			default:
				val = modString(mods, ctl.ID, def)
			}
			d.Values["mod:"+ctl.ID] = val
			if ctl.Type != "json" {
				d.Colors = append(d.Colors, czControlView{czControl: ctl, Setting: "mod:" + ctl.ID, Value: val, DefaultValue: def})
			}
		}
		d.ThemeScript = template.JS(spec.Script)
		d.ThemeScripts = spec.Scripts
	}
	d.Pages, _, _ = s.store.QueryPosts(ctx, store.PostQuery{Types: []string{"page"}, Statuses: []string{"publish"}, OrderBy: "title", Order: "asc", PerPage: -1})
	for _, loc := range t.MenuLocations {
		ml := czMenuLocation{Name: loc.Name}
		_ = s.store.Pool.QueryRow(ctx, `SELECT name FROM menus WHERE location = $1 LIMIT 1`, loc.Slug).Scan(&ml.Menu)
		d.MenuLocations = append(d.MenuLocations, ml)
	}
	d.WidgetAreas = t.WidgetAreas

	if d.CanUpload {
		s.enqueueMedia(c, 0)
	}
	a := s.pageAssets(c)
	a.Script("jquery", "wp-color-picker", "wp-a11y").Style("wp-color-picker")
	d.Styles = s.styleTags(a)
	d.HeadScripts = s.scriptTags(c, a, false)
	d.FooterScripts = s.scriptTags(c, a, true)
	if extra, ok := c.Get("wpgo.bodyHTML"); ok {
		d.BodyHTML = extra.(template.HTML)
	}
	c.Header("Cache-Control", "no-store")
	s.renderAdminTemplate(c, http.StatusOK, "admin/customize", d)
}

// customizeSave handles the screen's requests: action=preview updates the changeset the preview
// renders, action=publish saves the values.
func (s *Server) customizeSave(c *gin.Context) {
	u := currentUser(c)
	if !u.Can("edit_theme_options", nil) {
		c.JSON(http.StatusForbidden, gin.H{"success": false, "data": gin.H{"message": "Sorry, you are not allowed to customize this site."}})
		return
	}
	if !s.checkNonce(c, "customize") {
		c.JSON(http.StatusForbidden, gin.H{"success": false, "data": gin.H{"message": "The link you followed has expired."}})
		return
	}
	values := map[string]string{}
	if raw := c.PostForm("customized"); raw != "" {
		var in map[string]any
		if err := json.Unmarshal([]byte(raw), &in); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "data": gin.H{"message": "Invalid data."}})
			return
		}
		for k, v := range in {
			switch t := v.(type) {
			case string:
				values[k] = t
			case float64:
				values[k] = strconv.FormatFloat(t, 'f', -1, 64)
			case nil:
				values[k] = ""
			default:
				b, _ := json.Marshal(t)
				values[k] = string(b)
			}
		}
	}
	t := s.activeTheme()
	opts, mods, err := s.czSanitize(c, t, values)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "data": gin.H{"message": err.Error()}})
		return
	}
	switch c.PostForm("action") {
	case "preview":
		uuid := c.PostForm("uuid")
		czChangesets.Lock()
		cs := czChangesets.m[uuid]
		if cs != nil && cs.UserID == u.ID {
			cs.Theme, cs.Options, cs.Mods, cs.Expires = t.Slug, opts, mods, time.Now().Add(12*time.Hour)
		}
		czChangesets.Unlock()
		if cs == nil || cs.UserID != u.ID {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "data": gin.H{"message": "The changeset has expired. Reload the Customizer."}})
			return
		}
		c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{"uuid": uuid}})
	case "publish":
		values := map[string]any{"theme_mods_" + t.Slug: mods}
		for k, v := range opts {
			values[k] = v
		}
		if err := s.settings.Set(c, values); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "data": gin.H{"message": err.Error()}})
			return
		}
		if id, ok := opts["site_icon"].(int64); ok && id > 0 {
			s.siteIconSizes(c, id)
		}
		s.purgeAll(c)
		c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{"message": "Published"}})
	default:
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "data": gin.H{"message": "Unknown action."}})
	}
}

// Site icon --------------------------------------------------------------------------

// siteIconSizes are WP_Site_Icon's sizes.
var siteIconSizes = []int{512, 270, 192, 180, 32}

// siteIconSizes creates the square crops wp_site_icon() links to, stored as site_icon-<n> variants.
func (s *Server) siteIconSizes(ctx context.Context, id int64) {
	a, err := s.store.GetAttachment(ctx, id)
	if err != nil {
		return
	}
	var sizes []media.Size
	for _, n := range siteIconSizes {
		if a.Variant("site_icon-"+strconv.Itoa(n)) == nil {
			sizes = append(sizes, media.Size{Name: "site_icon-" + strconv.Itoa(n), Width: n, Height: n, Crop: true})
		}
	}
	if len(sizes) == 0 {
		return
	}
	info, err := media.ProcessImage(s.cfg.UploadsDir, a.FilePath, sizes)
	if err != nil {
		return
	}
	dir := ""
	if i := strings.LastIndex(a.FilePath, "/"); i >= 0 {
		dir = a.FilePath[:i+1]
	}
	for _, v := range info.Variants {
		_, _ = s.store.Pool.Exec(ctx, `INSERT INTO media_variants(media_id, name, file_path, width, height, mime_type, file_size)
			VALUES ($1,$2,$3,$4,$5,$6,$7)`, id, v.Name, dir+v.File, v.Width, v.Height, v.Mime, v.Size)
	}
}

// siteIconLinks is wp_site_icon().
func (s *Server) siteIconLinks(ctx context.Context, id int64) string {
	if id <= 0 {
		return ""
	}
	a, err := s.store.GetAttachment(ctx, id)
	if err != nil {
		return ""
	}
	src := func(n int) string {
		if v := a.Variant("site_icon-" + strconv.Itoa(n)); v != nil {
			return html.EscapeString(s.uploadURL(ctx, v.FilePath))
		}
		if v := a.Variant("thumbnail"); v != nil && n <= 150 {
			return html.EscapeString(s.uploadURL(ctx, v.FilePath))
		}
		return html.EscapeString(s.uploadURL(ctx, a.FilePath))
	}
	return fmt.Sprintf("<link rel=\"icon\" href=\"%s\" sizes=\"32x32\" />\n<link rel=\"icon\" href=\"%s\" sizes=\"192x192\" />\n"+
		"<link rel=\"apple-touch-icon\" href=\"%s\" />\n<meta name=\"msapplication-TileImage\" content=\"%s\" />\n", src(32), src(192), src(180), src(270))
}

// Front end -------------------------------------------------------------------------

// customizeChangeset returns the changeset a Customizer preview request asks for, or nil.
func (s *Server) customizeChangeset(c *gin.Context) *czChangeset {
	uuid := c.Query(czParam)
	if uuid == "" {
		return nil
	}
	u := currentUser(c)
	if !u.Can("edit_theme_options", nil) {
		return nil
	}
	czChangesets.Lock()
	cs := czChangesets.m[uuid]
	czChangesets.Unlock()
	if cs == nil || cs.UserID != u.ID || time.Now().After(cs.Expires) {
		return nil
	}
	// Never cache a preview.
	c.Header("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
	c.Header("X-WPGo-Cache-Control", "no-store")
	return cs
}

func (cs *czChangeset) option(key string, def any) any {
	if cs != nil {
		if v, ok := cs.Options[key]; ok {
			return v
		}
	}
	return def
}

// customizeRequery re-resolves the front page and the posts page with previewed Homepage Settings.
func (s *Server) customizeRequery(c *gin.Context, q *siteQuery, cs *czChangeset) {
	if q == nil || q.Feed != "" {
		return
	}
	_, a := cs.Options["show_on_front"]
	_, b := cs.Options["page_on_front"]
	_, d := cs.Options["page_for_posts"]
	if !a && !b && !d {
		return
	}
	toInt := func(v any) int64 {
		switch n := v.(type) {
		case int64:
			return n
		case int:
			return int64(n)
		case float64:
			return int64(n)
		}
		return 0
	}
	show, _ := cs.option("show_on_front", s.settings.String("show_on_front")).(string)
	front := toInt(cs.option("page_on_front", int64(s.settings.Int("page_on_front"))))
	posts := toInt(cs.option("page_for_posts", int64(s.settings.Int("page_for_posts"))))
	if strings.Trim(c.Request.URL.Path, "/") == "" && (q.Kind == "home" || q.Kind == "front-page") && q.PostsPage == nil {
		q.Kind, q.Post = "home", nil
		if show == "page" && front > 0 {
			if p, err := s.store.GetPost(c, front); err == nil && p.Status == "publish" && p.Type == "page" {
				q.Kind, q.Post = "front-page", p
			}
		}
		return
	}
	switch {
	case q.Kind == "page" && q.Post != nil && show == "page" && q.Post.ID == posts:
		q.Kind, q.PostsPage, q.Post = "home", q.Post, nil
	case q.Kind == "home" && q.PostsPage != nil && (show != "page" || q.PostsPage.ID != posts):
		q.Kind, q.Post, q.PostsPage = "page", q.PostsPage, nil
	}
}

// customizeView fills the customizer-driven view data: site identity, theme mods, custom logo and
// site icon, with the previewed values when cs is set.
func (s *Server) customizeView(c *gin.Context, v *siteView, cs *czChangeset) {
	ctx := v.ctx
	v.Customize = cs
	if cs != nil && cs.Theme == v.Theme.Slug {
		v.ThemeMods = cs.Mods
	} else {
		v.ThemeMods = s.themeMods(v.Theme.Slug)
	}
	if cs != nil {
		if name, ok := cs.Options["blogname"].(string); ok {
			v.SiteName = name
		}
		if desc, ok := cs.Options["blogdescription"].(string); ok {
			v.Description = desc
		}
	}
	var icon int64
	switch n := cs.option("site_icon", nil).(type) {
	case int64:
		icon = n
	default:
		icon = int64(s.settings.Int("site_icon"))
	}
	v.SiteIcon = template.HTML(s.siteIconLinks(ctx, icon))
	v.CustomCSS = modString(v.ThemeMods, "custom_css", "")
	if id := int64(modInt(v.ThemeMods, "custom_logo", 0)); id > 0 && themeSupportsLogo(v.Theme) {
		if a, err := s.store.GetAttachment(ctx, id); err == nil {
			img := *a
			if img.Alt == "" {
				img.Alt = v.SiteName
			}
			tag := string(s.attachmentImage(ctx, &img, "full", "custom-logo"))
			unlink := false
			if cl, ok := v.Theme.Supports["custom-logo"].(map[string]any); ok {
				unlink, _ = cl["unlink-homepage-logo"].(bool)
			}
			switch {
			case unlink && v.IsFront:
				v.CustomLogo = template.HTML(`<span class="custom-logo-link">` + tag + `</span>`)
			case v.IsFront:
				v.CustomLogo = template.HTML(`<a href="` + html.EscapeString(v.HomeURL) + `" class="custom-logo-link" rel="home" aria-current="page">` + tag + `</a>`)
			default:
				v.CustomLogo = template.HTML(`<a href="` + html.EscapeString(v.HomeURL) + `" class="custom-logo-link" rel="home">` + tag + `</a>`)
			}
		}
	}
}

// customizeBodyClasses are core's custom-background and wp-custom-logo body classes.
func customizeBodyClasses(v *siteView) []string {
	var out []string
	if customBackground(v) != "" {
		out = append(out, "custom-background")
	}
	if v.CustomLogo != "" {
		out = append(out, "wp-custom-logo")
	}
	return out
}

// customizeHead is printed at the end of wp_head: _custom_background_cb() (priority 10),
// wp_site_icon() (99) and wp_custom_css_cb() (101).
func customizeHead(v *siteView) string {
	var sb strings.Builder
	if bg := customBackground(v); bg != "" {
		sb.WriteString("<style id=\"custom-background-css\">\nbody.custom-background { background-color: #" + bg + "; }\n</style>\n")
	}
	sb.WriteString(string(v.SiteIcon))
	if css := strings.TrimSpace(czTagRe.ReplaceAllString(v.CustomCSS, "")); css != "" {
		sb.WriteString("<style id=\"wp-custom-css\">\n\t\t\t" + css + "\t\t</style>\n")
	}
	return sb.String()
}

// customizeFinish adjusts a preview after the view is built: the document title uses the previewed
// site title and tagline, and a footer script keeps navigation inside the preview.
func (s *Server) customizeFinish(c *gin.Context, v *siteView, cs *czChangeset) {
	if cs == nil {
		return
	}
	title := string(v.DocumentTitle)
	if name, ok := cs.Options["blogname"].(string); ok {
		if old := html.EscapeString(s.settings.String("blogname")); old != "" {
			title = strings.ReplaceAll(title, old, html.EscapeString(name))
		}
	}
	if desc, ok := cs.Options["blogdescription"].(string); ok {
		if old := html.EscapeString(s.settings.String("blogdescription")); old != "" {
			title = strings.ReplaceAll(title, old, html.EscapeString(desc))
		}
	}
	v.DocumentTitle = template.HTML(title)
	uuid := c.Query(czParam)
	v.Footer += template.HTML(`<script id="wpgo-customize-preview">
( function() {
	var uuid = ` + strconvQuote(uuid) + `, param = ` + strconvQuote(czParam) + `;
	function clean( u ) { u.searchParams.delete( param ); return u.pathname + u.search + u.hash; }
	function inside( u ) { return u.origin === location.origin && ! /^\/wp-(admin|login)/.test( u.pathname ); }
	if ( window.parent !== window ) {
		window.parent.postMessage( { wpgoCustomize: 'url', url: clean( new URL( location.href ) ), title: document.title }, location.origin );
	}
	document.addEventListener( 'click', function( e ) {
		var a = e.target.closest && e.target.closest( 'a[href]' ), u;
		if ( ! a || a.getAttribute( 'href' ).charAt( 0 ) === '#' ) { return; }
		u = new URL( a.href, location.href );
		if ( ! inside( u ) ) { e.preventDefault(); return; }
		u.searchParams.set( param, uuid );
		a.href = u.toString();
		a.removeAttribute( 'target' );
	}, true );
	document.addEventListener( 'submit', function( e ) {
		var f = e.target, u = new URL( f.action || location.href, location.href ), i;
		if ( ! inside( u ) || ( f.method || 'get' ).toLowerCase() !== 'get' ) { e.preventDefault(); return; }
		i = document.createElement( 'input' );
		i.type = 'hidden'; i.name = param; i.value = uuid;
		f.appendChild( i );
	}, true );
} )();
</script>
`)
}
