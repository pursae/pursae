package server

import (
	"context"
	"html/template"
	"log"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"wpgo/internal/webbuilder"
)

// Settings › Web Builder (options-general?page=web-builder, add_options_page() style): the post types
// the builder edits and the pages whose published markup came from an older renderer. The plugin is
// a plain on/off switch on the Plugins screen (wbSetActive); while it is off this screen and its menu
// entry are gone, like any inactive plugin's.

const (
	// wbSettingsPage is the page slug of Settings › Web Builder.
	wbSettingsPage = "web-builder"
	// wbSettingsURL is the screen's admin URL.
	wbSettingsURL = "/wp-admin/options-general?page=" + wbSettingsPage
	// wbPostTypesOption lists the post types that open in the builder, comma separated.
	wbPostTypesOption = "web_builder_post_types"
	// wbSettingsNonce is the nonce action of the settings form.
	wbSettingsNonce = "web-builder-settings"
)

const (
	// wbRevisionsKeepOption is how many builder revisions (stored docs) each post keeps; 0 keeps all.
	wbRevisionsKeepOption  = "web_builder_revisions_keep"
	wbRevisionsKeepDefault = 50
	wbRevisionsKeepMax     = 1000
)

// wbRevisionsKeep is the number of builder revisions kept per post (Settings › Web Builder).
func (s *Server) wbRevisionsKeep() int {
	v := s.settings.Get(wbRevisionsKeepOption)
	switch n := v.(type) {
	case nil:
		return wbRevisionsKeepDefault
	case float64:
		return clampKeep(int(n))
	case int:
		return clampKeep(n)
	case int64:
		return clampKeep(int(n))
	case string:
		if n = strings.TrimSpace(n); n == "" {
			return wbRevisionsKeepDefault
		}
		if i, err := strconv.Atoi(n); err == nil {
			return clampKeep(i)
		}
	}
	return wbRevisionsKeepDefault
}

func clampKeep(n int) int {
	if n < 0 {
		return wbRevisionsKeepDefault
	}
	return min(n, wbRevisionsKeepMax)
}

// Form defaults of Settings › Web Builder (a form's own content keys, webbuilder.FormKey*, win).
const (
	wbFormToOption       = "web_builder_form_to"       // recipients; empty = admin_email
	wbFormCcOption       = "web_builder_form_cc"       // copy recipients
	wbFormSuccessOption  = "web_builder_form_success"  // message after a sent form
	wbFormFailureOption  = "web_builder_form_error"    // message when the form could not be sent
	wbFormRedirectOption = "web_builder_form_redirect" // page opened after a sent form
	wbFormStoreOption    = "web_builder_form_store"    // keep submissions (default on)
	wbFormListPer        = 50
)

// wbFormStoreOn reports whether form submissions are kept in the database (on unless switched off).
func (s *Server) wbFormStoreOn() bool {
	v := s.settings.Get(wbFormStoreOption)
	if v == nil {
		return true
	}
	return s.settings.Bool(wbFormStoreOption)
}

// wbEnabledTypes are the post types the builder edits (Settings › Web Builder; pages and posts by
// default).
func (s *Server) wbEnabledTypes() []string {
	var out []string
	for _, t := range strings.Split(s.settings.String(wbPostTypesOption), ",") {
		if t = strings.TrimSpace(t); t != "" && !slices.Contains(out, t) {
			out = append(out, t)
		}
	}
	return out
}

// wbEnabledType reports whether posts of the type open in the builder.
func (s *Server) wbEnabledType(typ string) bool {
	return typ != "" && slices.Contains(s.wbEnabledTypes(), typ)
}

// wbSetActive is the Plugins screen's Activate / Deactivate of Pursae Web Builder: the option, the
// theme switch and a cache purge (built pages render differently with the plugin off).
//
// Activation moves the site to Hello Pursae, the blank canvas the Theme Builder designs on, and
// remembers the theme it leaves (web_builder_previous_theme) for deactivation; a notice offers to
// switch back. Deactivation moves a site still on Hello Pursae back to that theme (the default theme
// when it is gone); a site whose theme the user changed in between keeps it.
func (s *Server) wbSetActive(ctx context.Context, on bool) error {
	was := s.wbOn()
	if err := s.settings.Set(ctx, map[string]any{webbuilder.PluginOption: on}); err != nil {
		return err
	}
	if on && !was {
		s.wbSwitchToHello(ctx)
	} else if !on && was {
		s.wbSwitchBack(ctx)
	}
	s.purgeAll(ctx)
	return nil
}

const (
	// wbPreviousThemeOption is the theme the site had before activation switched it to Hello Pursae.
	wbPreviousThemeOption = webbuilder.OptionPreviousTheme
	// wbThemeNoticeOption holds the pending "Your site now uses Hello Pursae" notice:
	// {"previous": "<slug>"}. Absent once dismissed, switched back or the plugin is off.
	wbThemeNoticeOption = webbuilder.OptionThemeNotice
	// wbHelloOfferedOption marks the "Switch to Hello Pursae" offer as done: set by activation and by
	// the offer's Dismiss or Switch link, so only a site that had the plugin on before Hello Pursae
	// existed sees the offer, once.
	wbHelloOfferedOption = webbuilder.OptionThemeOffer
	// wbThemeNoticeNonce is the nonce action of the notices' links (wbThemeNoticeAction).
	wbThemeNoticeNonce = "web-builder-theme-notice"
)

// wbSwitchToHello makes Hello Pursae the active theme (restoring it when it was deleted) and
// remembers the theme it replaces. A failed switch is logged: the plugin works with any theme.
func (s *Server) wbSwitchToHello(ctx context.Context) {
	_ = s.restoreTheme(ctx, wbHelloSlug) // the blank canvas comes with the plugin, even after a delete
	values := map[string]any{wbHelloOfferedOption: true}
	if cur := s.settings.String("stylesheet"); cur != wbHelloSlug {
		if err := s.switchTheme(ctx, wbHelloSlug); err != nil {
			log.Printf("web builder: switching to %s: %v", wbHelloSlug, err)
		} else {
			values[wbPreviousThemeOption] = cur
			values[wbThemeNoticeOption] = map[string]any{"previous": cur}
		}
	}
	if err := s.settings.Set(ctx, values); err != nil {
		log.Printf("web builder: %v", err)
	}
}

// wbSwitchBack moves a site on Hello Pursae back to the theme it had before (wbPreviousTheme) and
// forgets that theme and the notice. Only a switch activation made is undone: a site on another
// theme, or one whose owner chose Hello Pursae themselves (nothing remembered), keeps its theme.
func (s *Server) wbSwitchBack(ctx context.Context) {
	if s.settings.String("stylesheet") == wbHelloSlug && s.settings.String(wbPreviousThemeOption) != "" {
		back := s.wbPreviousTheme(ctx)
		_ = s.restoreTheme(ctx, back) // the default theme comes back even after a delete
		if err := s.switchTheme(ctx, back); err != nil {
			log.Printf("web builder: switching back to %s: %v", back, err)
		}
	}
	if err := s.settings.Set(ctx, map[string]any{wbPreviousThemeOption: nil, wbThemeNoticeOption: nil}); err != nil {
		log.Printf("web builder: %v", err)
	}
}

// wbForgetPreviousTheme drops the theme activation left and its notice (switchTheme calls it when the
// site leaves Hello Pursae): once the owner moved the site to another theme, deactivation must not
// later move it from a Hello Pursae they chose again themselves to that old theme, and the notice
// must not offer to switch back to it.
func (s *Server) wbForgetPreviousTheme(ctx context.Context) {
	if s.settings.String(wbPreviousThemeOption) == "" && s.settings.Get(wbThemeNoticeOption) == nil {
		return
	}
	if err := s.settings.Set(ctx, map[string]any{wbPreviousThemeOption: nil, wbThemeNoticeOption: nil}); err != nil {
		log.Printf("web builder: %v", err)
	}
}

// wbPreviousTheme is the theme to go back to from Hello Pursae: the remembered one while it is
// installed and neither Hello Pursae nor an old builder theme, else the default theme.
func (s *Server) wbPreviousTheme(ctx context.Context) string {
	prev := s.settings.String(wbPreviousThemeOption)
	if prev != "" && prev != wbHelloSlug && !s.wbOldBuilderTheme(ctx, prev) && !s.themeDeleted(prev) && s.installedTheme(prev) != nil {
		return prev
	}
	return defaultTheme
}

// wbThemeNotice is the Hello Pursae notice for this request, on the Dashboard, Plugins and Themes
// screens for users who may switch themes (pluginNotices calls it): after activation "Your site now
// uses Hello Pursae" with Switch back, or, on a site that had the plugin on before Hello Pursae
// existed, a one-time offer to switch to it. Both have a Dismiss link that sticks.
func (s *Server) wbThemeNotice(c *gin.Context) (notice, bool) {
	from := ""
	switch c.Request.URL.Path {
	case "/wp-admin", "/wp-admin/", "/wp-admin/index":
	case "/wp-admin/plugins", pluginSetupPath: // the first activation lands on the setup question (plugin_setup.go)
		from = "plugins"
	case "/wp-admin/themes":
		from = "themes"
	default:
		return notice{}, false
	}
	if !s.wbOn() {
		return notice{}, false
	}
	if u := currentUser(c); u == nil || !u.Can("switch_themes", nil) {
		return notice{}, false
	}
	link := func(action, id, label string) string {
		href := wbSettingsURL + "&wb-theme=" + action + "&_wpnonce=" + s.nonce(c, wbThemeNoticeNonce)
		if from != "" {
			href += "&from=" + from
		}
		return `<a href="` + template.HTMLEscapeString(href) + `" id="` + id + `">` + label + `</a>`
	}
	esc := template.HTMLEscapeString
	cur := s.settings.String("stylesheet")
	if m, ok := s.settings.Get(wbThemeNoticeOption).(map[string]any); ok && cur == wbHelloSlug {
		msg := "<strong>Your site now uses Hello Pursae</strong>, the blank canvas Pursae Web Builder designs headers, footers and templates on."
		prev, _ := m["previous"].(string)
		if t := s.installedTheme(prev); t != nil && prev != wbHelloSlug {
			msg += " Previous theme: <strong>" + esc(t.Name) + "</strong>. " + link("switch-back", "web-builder-theme-switch-back", "Switch back") + " |"
		}
		msg += " " + link("dismiss", "web-builder-theme-dismiss", "Dismiss")
		return notice{Type: "info", Message: template.HTML(msg), Dismissible: true}, true
	}
	if cur != wbHelloSlug && !s.settings.Bool(wbHelloOfferedOption) {
		msg := "<strong>Pursae Web Builder</strong> designs headers, footers and templates on the <strong>Hello Pursae</strong> theme. " +
			link("hello", "web-builder-theme-switch-hello", "Switch to Hello Pursae") + " | " + link("dismiss", "web-builder-theme-dismiss", "Dismiss")
		return notice{Type: "info", Message: template.HTML(msg), Dismissible: true}, true
	}
	return notice{}, false
}

// wbThemeNoticeAction runs a link of wbThemeNotice (GET options-general?page=web-builder&wb-theme=…
// with a nonce; manage_options checked by the options screen, switch_themes here) and goes back to
// the screen that showed it: dismiss, switch-back (to the previous theme) or hello (switch to Hello
// Pursae, plugin on only).
func (s *Server) wbThemeNoticeAction(c *gin.Context) {
	if !s.checkNonce(c, wbThemeNoticeNonce) {
		s.denyNonce(c)
		return
	}
	if u := currentUser(c); u == nil || !u.Can("switch_themes", nil) {
		c.String(http.StatusForbidden, "Sorry, you are not allowed to switch themes for this site.")
		return
	}
	ctx := c.Request.Context()
	switch c.Query("wb-theme") {
	case "dismiss":
		if err := s.settings.Set(ctx, map[string]any{wbThemeNoticeOption: nil, wbHelloOfferedOption: true}); err != nil {
			c.String(http.StatusInternalServerError, err.Error())
			return
		}
	case "switch-back":
		s.wbSwitchBack(ctx)
	case "hello":
		if !s.wbOn() {
			c.String(http.StatusForbidden, "Sorry, you are not allowed to access this page.")
			return
		}
		s.wbSwitchToHello(ctx)
	default:
		c.String(http.StatusBadRequest, "Unknown action.")
		return
	}
	back := "/wp-admin/"
	switch c.Query("from") {
	case "plugins":
		back = "/wp-admin/plugins"
	case "themes":
		back = "/wp-admin/themes"
	}
	c.Redirect(http.StatusFound, back)
}

// wbTypeChoice is a post type offered on the settings screen.
type wbTypeChoice struct {
	Slug, Label string
	Checked     bool
}

// wbPostTypeChoices are the registered, public, UI-visible post types with an editor (attachments
// excluded), in registration order, plus any enabled type that no longer qualifies so it can be
// switched off. content_types keeps rows of types whose plugin is off (posts reference them), so a
// row alone does not make a type available (wbTypeRegistered).
func (s *Server) wbPostTypeChoices(ctx context.Context) []wbTypeChoice {
	enabled := s.wbEnabledTypes()
	var out []wbTypeChoice
	rows, err := s.store.Pool.Query(ctx, `SELECT slug, name FROM content_types
		WHERE public AND slug <> 'attachment' AND coalesce(supports->>'editor', 'false') = 'true' ORDER BY id`)
	if err == nil {
		for rows.Next() {
			var ch wbTypeChoice
			if rows.Scan(&ch.Slug, &ch.Label) == nil && s.wbTypeRegistered(ch.Slug) {
				ch.Checked = slices.Contains(enabled, ch.Slug)
				out = append(out, ch)
			}
		}
		rows.Close()
	}
	for _, t := range enabled {
		if !slices.ContainsFunc(out, func(ch wbTypeChoice) bool { return ch.Slug == t }) {
			out = append(out, wbTypeChoice{Slug: t, Label: t, Checked: true})
		}
	}
	return out
}

// wbTypeRegistered reports whether post type typ exists right now with an admin UI: posts and
// pages always, products while Pursae Commerce is on, ACF-Go post types while ACF-Go registers them
// with show_ui.
func (s *Server) wbTypeRegistered(typ string) bool {
	switch typ {
	case "post", "page":
		return true
	case "product":
		return s.gooOn()
	}
	acfgoRegistry.RLock()
	pt := acfgoRegistry.types[typ]
	acfgoRegistry.RUnlock()
	return pt != nil && pt.ShowUI
}

// wbOutdatedPage is a built post whose snapshot an older renderer produced.
type wbOutdatedPage struct {
	ID              int64
	Title, Type     string
	Renderer        int
	EditURL, Status string
}

// wbOutdatedLimit caps the list on the settings screen.
const wbOutdatedLimit = 200

// wbOutdatedPages lists the built posts (not trashed) whose published markup came from a renderer
// older than webbuilder.RendererVersion, most recently modified first, and how many there are.
// Only the editor renders (renderStatic runs in the browser), so each needs opening and publishing.
func (s *Server) wbOutdatedPages(ctx context.Context) ([]wbOutdatedPage, int, error) {
	where, args := s.wbOutdatedWhere() // an older renderer, or a changed site style (webbuilder_site_style.go)
	var total int
	if err := s.store.Pool.QueryRow(ctx, `SELECT count(*) FROM web_builder_docs d JOIN posts p ON p.id = d.post_id
		WHERE `+where+` AND p.status NOT IN ('trash', 'auto-draft')`, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.store.Pool.Query(ctx, `SELECT p.id, p.title, p.type, p.status, d.renderer FROM web_builder_docs d
		JOIN posts p ON p.id = d.post_id
		WHERE `+where+` AND p.status NOT IN ('trash', 'auto-draft')
		ORDER BY p.modified_at DESC, p.id DESC LIMIT $3`, append(args, wbOutdatedLimit)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []wbOutdatedPage
	for rows.Next() {
		var p wbOutdatedPage
		if err := rows.Scan(&p.ID, &p.Title, &p.Type, &p.Status, &p.Renderer); err != nil {
			return nil, 0, err
		}
		if p.Title == "" {
			p.Title = "(no title)"
		}
		p.EditURL = wbEditURL(p.ID)
		out = append(out, p)
	}
	return out, total, rows.Err()
}

// wbSettingsData is the template data of Settings › Web Builder.
type wbSettingsData struct {
	Nonce           string
	OptionPage      string
	Types           []wbTypeChoice
	Outdated        []wbOutdatedPage
	OutdatedTotal   int
	OutdatedMore    bool
	OutdatedErr     bool
	RendererVersion int
	RevisionsKeep   int
	FormTo, FormCc  string
	FormSuccess     string
	FormError       string
	FormRedirect    string
	FormStore       bool
	RegenerateURL   string
	TokenFields     []wbTokenField
	Submissions     []wbSubmissionRow
	SubmissionTotal int
	SubmissionMore  bool
}

// wbSettingsScreen renders Settings › Web Builder (GET options-general?page=web-builder; the caller
// checked manage_options). It also takes the dismissal of the upgrade notice, which must work while
// the plugin is off.
func (s *Server) wbSettingsScreen(c *gin.Context) {
	if c.Query("dismiss") == wbUpgradeNoticeDismiss {
		s.wbDismissUpgradeNotice(c) // webbuilder_upgrade.go
		return
	}
	if c.Query("wb-theme") != "" { // the Hello Pursae notice's links (switch back works with the plugin off too)
		s.wbThemeNoticeAction(c)
		return
	}
	if !s.wbOn() {
		c.String(http.StatusForbidden, "Sorry, you are not allowed to access this page.")
		return
	}
	if c.Query("regenerate-frame") == "1" { // the bulk regenerate screen (webbuilder_regenerate.go)
		s.wbBundleFrame(c, "__WEB_BUILDER_REGENERATE__", "Regenerate pages")
		return
	}
	ctx := c.Request.Context()
	d := wbSettingsData{Nonce: s.nonce(c, wbSettingsNonce), OptionPage: pluginSettingsSlug + wbSettingsPage,
		Types: s.wbPostTypeChoices(ctx), RendererVersion: webbuilder.RendererVersion, RevisionsKeep: s.wbRevisionsKeep(),
		FormTo: s.settings.String(wbFormToOption), FormCc: s.settings.String(wbFormCcOption), FormSuccess: s.settings.String(wbFormSuccessOption),
		FormError: s.settings.String(wbFormFailureOption), FormRedirect: s.settings.String(wbFormRedirectOption), FormStore: s.wbFormStoreOn()}
	d.RegenerateURL = wbSettingsURL + "&regenerate-frame=1"
	d.TokenFields = s.wbTokenFields()
	s.wbLoadSubmissions(c, &d)
	var err error
	if d.Outdated, d.OutdatedTotal, err = s.wbOutdatedPages(ctx); err != nil {
		d.OutdatedErr = true
	}
	d.OutdatedMore = d.OutdatedTotal > len(d.Outdated)
	var notices []notice
	if c.Query("submissions-deleted") != "" {
		notices = append(notices, notice{Type: "success", Message: "<strong>Submissions deleted.</strong>", Dismissible: true})
	}
	if c.Query("settings-updated") == "true" {
		notices = append(notices, notice{Type: "success", Message: "<strong>Settings saved.</strong>", Dismissible: true})
	}
	s.adminPage(c, "options-general?page="+wbSettingsPage, "Web Builder Settings", "settings_page_"+wbSettingsPage,
		"admin/web-builder-settings", d, notices...)
}

// wbSettingsSave is options.php for Settings › Web Builder (option_page plugin:web-builder; the
// caller checked manage_options). Only post types offered on the screen are stored.
func (s *Server) wbSettingsSave(c *gin.Context) {
	if !s.wbOn() {
		c.String(http.StatusForbidden, "Sorry, you are not allowed to access this page.")
		return
	}
	if !s.checkNonce(c, wbSettingsNonce) {
		s.denyNonce(c)
		return
	}
	ctx := c.Request.Context()
	if c.PostForm("wb_submissions") != "" {
		s.wbSubmissionsAction(c)
		return
	}
	sent := c.PostFormArray(wbPostTypesOption + "[]")
	var keep []string
	for _, ch := range s.wbPostTypeChoices(ctx) {
		if slices.Contains(sent, ch.Slug) {
			keep = append(keep, ch.Slug)
		}
	}
	revKeep := wbRevisionsKeepDefault
	if v := strings.TrimSpace(c.PostForm(wbRevisionsKeepOption)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			revKeep = min(n, wbRevisionsKeepMax)
		}
	}
	if err := s.settings.Set(ctx, map[string]any{wbPostTypesOption: strings.Join(keep, ","), wbRevisionsKeepOption: revKeep,
		wbFormToOption:       strings.Join(webbuilder.FormAddresses(c.PostForm(wbFormToOption)), ", "),
		wbFormCcOption:       strings.Join(webbuilder.FormAddresses(c.PostForm(wbFormCcOption)), ", "),
		wbFormSuccessOption:  wbFormLine(c.PostForm(wbFormSuccessOption)),
		wbFormFailureOption:  wbFormLine(c.PostForm(wbFormFailureOption)),
		wbFormRedirectOption: strings.TrimSpace(c.PostForm(wbFormRedirectOption)),
		wbFormStoreOption:    c.PostForm(wbFormStoreOption) != ""}); err != nil {
		c.String(http.StatusInternalServerError, err.Error())
		return
	}
	tok := wbTokens{Colors: map[string]string{}, Fonts: map[string]string{}}
	for _, k := range wbTokenColors {
		tok.Colors[k] = c.PostForm("wb_token_color_" + k)
	}
	for _, k := range wbTokenFonts {
		tok.Fonts[k] = c.PostForm("wb_token_font_" + k)
	}
	if err := s.wbSetTokens(ctx, tok); err != nil {
		c.String(http.StatusInternalServerError, err.Error())
		return
	}
	s.purgeAll(ctx) // the toolbar's "Edit with Pursae Web Builder" and built pages follow the types
	c.Redirect(http.StatusFound, wbSettingsURL+"&settings-updated=true")
}
