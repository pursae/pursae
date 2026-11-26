package server

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"html"
	"html/template"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The public maintenance page: one self-contained HTML document (inline CSS and JS, no CDN; the
// dashicons font for the social icons comes from /wp-includes), styled by the active theme.

//go:embed maintenance/page.html
var maintPageHTML string

// cspNonce is the nonce attribute of the page's own scripts: <script {{cspNonce}}> (csp_nonce.go).
var maintPageTmpl = template.Must(template.New("maintenance").Funcs(template.FuncMap{
	"cspNonce": func() template.HTMLAttr { return template.HTMLAttr(strings.TrimSpace(cspNonceAttr())) },
}).Parse(maintPageHTML))

// maintTheme is a premade design: a class on <body> plus the Design values it starts from.
// Activating a theme on the Themes tab copies its values into the Design tab, as in the original.
type maintTheme struct {
	Key, Name, Description string
	BgType                 string
	BgColor, BgColor2      string
	FontFamily             string
	FontColor, Heading     string
	Link, Accent           string
	Position               string
	Box                    bool
	Overlay                int
}

var maintThemes = []*maintTheme{
	{Key: "default", Name: "Default", Description: "Centered white text on a dark background; add a photo under Design.",
		BgType: "gradient", BgColor: "#0f172a", BgColor2: "#1e3a5f", FontFamily: "system", FontColor: "#e2e8f0", Heading: "#ffffff",
		Link: "#7cc4ff", Accent: "#2271b1", Position: "middle", Overlay: 45},
	{Key: "minimal", Name: "Minimal", Description: "Clean white page with dark type and a single accent color.",
		BgType: "color", BgColor: "#ffffff", BgColor2: "#ffffff", FontFamily: "system", FontColor: "#3c434a", Heading: "#1d2327",
		Link: "#2271b1", Accent: "#2271b1", Position: "left", Overlay: 0},
	{Key: "card", Name: "Card", Description: "Content on a frosted card over a soft gradient.",
		BgType: "gradient", BgColor: "#c7d2fe", BgColor2: "#fbcfe8", FontFamily: "humanist", FontColor: "#334155", Heading: "#0f172a",
		Link: "#4f46e5", Accent: "#4f46e5", Position: "middle", Box: true, Overlay: 0},
	{Key: "split", Name: "Split", Description: "Text on the left, a full-height image or color panel on the right.",
		BgType: "color", BgColor: "#f6f7f7", BgColor2: "#2271b1", FontFamily: "system", FontColor: "#50575e", Heading: "#1d2327",
		Link: "#2271b1", Accent: "#2271b1", Position: "left", Overlay: 0},
	{Key: "neon", Name: "Neon", Description: "Dark page with a glowing accent, made for launches.",
		BgType: "gradient", BgColor: "#05010d", BgColor2: "#1a0b2e", FontFamily: "geometric", FontColor: "#c4b5fd", Heading: "#ffffff",
		Link: "#22d3ee", Accent: "#d946ef", Position: "middle", Overlay: 20},
	{Key: "sunrise", Name: "Sunrise", Description: "Warm gradient with large friendly type for Coming Soon pages.",
		BgType: "gradient", BgColor: "#f97316", BgColor2: "#db2777", FontFamily: "rounded", FontColor: "#fff7ed", Heading: "#ffffff",
		Link: "#ffffff", Accent: "#7c2d12", Position: "bottom", Overlay: 0},
	{Key: "editorial", Name: "Editorial", Description: "Serif headline on warm paper, like a printed notice.",
		BgType: "color", BgColor: "#f5f0e6", BgColor2: "#f5f0e6", FontFamily: "serif", FontColor: "#44403c", Heading: "#1c1917",
		Link: "#9a3412", Accent: "#1c1917", Position: "middle", Overlay: 0},
}

func maintThemeByKey(k string) *maintTheme {
	for _, t := range maintThemes {
		if t.Key == k {
			return t
		}
	}
	return nil
}

// apply makes t the active theme and copies its design values.
func (t *maintTheme) apply(st *maintSettings) {
	st.Theme = t.Key
	st.BgType, st.BgColor, st.BgColor2 = t.BgType, t.BgColor, t.BgColor2
	st.FontFamily, st.FontColor, st.HeadingColor = t.FontFamily, t.FontColor, t.Heading
	st.LinkColor, st.AccentColor = t.Link, t.Accent
	st.ContentPosition, st.ContentBox, st.Overlay = t.Position, t.Box, t.Overlay
}

// maintFonts are the font choices: system stacks, because the page loads nothing from outside
// (the original loads Google fonts through Bunny Fonts; WordPress Go's CSP allows neither).
var maintFonts = []struct{ Key, Label, Stack string }{
	{"system", "System UI (sans-serif)", `-apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Oxygen-Sans, Ubuntu, Cantarell, "Helvetica Neue", sans-serif`},
	{"humanist", "Humanist (Seravek, Gill Sans)", `Seravek, "Gill Sans Nova", Ubuntu, Calibri, "DejaVu Sans", source-sans-pro, sans-serif`},
	{"geometric", "Geometric (Avenir, Montserrat)", `Avenir, Montserrat, Corbel, "URW Gothic", source-sans-pro, sans-serif`},
	{"rounded", "Rounded (SF Pro Rounded, Nunito)", `ui-rounded, "Hiragino Maru Gothic ProN", Quicksand, Comfortaa, Manjari, "Arial Rounded MT", Calibri, source-sans-pro, sans-serif`},
	{"serif", "Old style serif (Iowan, Palatino)", `"Iowan Old Style", "Palatino Linotype", "URW Palladio L", P052, Georgia, serif`},
	{"slab", "Slab serif (Rockwell)", `Rockwell, "Rockwell Nova", "Roboto Slab", "DejaVu Serif", "Sitka Small", serif`},
	{"mono", "Monospace", `ui-monospace, "Cascadia Code", "Source Code Pro", Menlo, Consolas, "DejaVu Sans Mono", monospace`},
}

func maintFontStack(k string) string {
	for _, f := range maintFonts {
		if f.Key == k {
			return f.Stack
		}
	}
	return maintFonts[0].Stack
}

// maintPageView is the page template's data.
type maintPageView struct {
	RTL          bool
	S            maintSettings
	Title        string
	Heading      string
	Content      template.HTML
	Footer       template.HTML
	CustomHTML   template.HTML
	CSS          template.CSS
	FontStack    template.CSS
	SiteName     string
	HomeURL      string
	Lang         string
	Preview      bool
	EndUnix      int64 // countdown end, Unix seconds (0: no countdown)
	EndISO       string
	Social       []maintSocialLink
	SubscribeURL string
	UnlockURL    string
	Nonce        string
	Subscribed   bool
	SubscribeErr string
	Unlocked     string // "wrong" after a bad site password
	LoginAction  string
	RedirectTo   string
	PolicyURL    string
	ConsentHead  template.HTML // Pursae Cookie's consent defaults and styles (when the plugin is on)
	AnalyticsTag template.HTML // the Google Analytics snippet, inert until the visitor accepts statistics when Pursae Cookie is on
	ConsentFoot  template.HTML // Pursae Cookie's banner and script
	HasModules   bool
}

type maintSocialLink struct{ URL, Label, Icon string }

var maintColorRe = regexp.MustCompile(`^#(?:[0-9a-fA-F]{3}|[0-9a-fA-F]{4}|[0-9a-fA-F]{6}|[0-9a-fA-F]{8})$`)

// maintColor keeps only #rgb[a] / #rrggbb[aa] colors: they go into a style sheet.
func maintColor(v, fallback string) string {
	if maintColorRe.MatchString(v) {
		return v
	}
	return fallback
}

// maintCSSURL keeps an image URL safe inside url("…").
func maintCSSURL(v string) string {
	if v == "" || strings.ContainsAny(v, "\"'()\\\n\r<>") || !(strings.HasPrefix(v, "/") || strings.HasPrefix(v, "https://") || strings.HasPrefix(v, "http://")) {
		return ""
	}
	return v
}

// maintVars replaces %sitetitle%, %sitetagline% and %year% in the page's texts.
// maintVarsHTML is maintVars for markup: the site title and tagline are text, escaped.
func (s *Server) maintVarsHTML(v string) string {
	return strings.NewReplacer("%sitetitle%", html.EscapeString(s.settings.String("blogname")), "%sitetagline%", html.EscapeString(s.settings.String("blogdescription")),
		"%year%", strconv.Itoa(time.Now().In(s.loc()).Year())).Replace(v)
}

func (s *Server) maintVars(v string) string {
	return strings.NewReplacer("%sitetitle%", s.settings.String("blogname"), "%sitetagline%", s.settings.String("blogdescription"),
		"%year%", strconv.Itoa(time.Now().In(s.loc()).Year())).Replace(v)
}

// maintRender writes the maintenance page with the given status (503 sends Retry-After).
func (s *Server) maintRender(w http.ResponseWriter, r *http.Request, st maintSettings, status int) {
	ctx := r.Context()
	home := s.links(ctx).HomeURL("/")
	lang := s.maintTranslate(r, &st) // Pursae Multi Language: the visitor's language, when it is active
	if st.WBTemplate > 0 {
		wl := strings.ReplaceAll(s.settings.String("WPLANG"), "_", "-")
		if lang != nil {
			wl = lang.W3C()
		}
		if s.maintRenderWB(w, r, st, status, wl) {
			return
		}
	}
	v := maintPageView{
		S:       st,
		Title:   s.maintVars(st.Title),
		Heading: s.maintVars(st.Heading),
		// Raw only from an unfiltered_html administrator (kses otherwise): its scripts get the nonce.
		Content:      template.HTML(s.maintVarsHTML(s.cspTrustStored(st.Content))),
		Footer:       template.HTML(s.maintVarsHTML(s.cspTrustStored(st.FooterText))),
		CustomHTML:   template.HTML(s.cspTrustStored(st.CustomHTML)),
		FontStack:    template.CSS(maintFontStack(st.FontFamily)),
		SiteName:     s.settings.String("blogname"),
		HomeURL:      home,
		Lang:         strings.ReplaceAll(s.settings.String("WPLANG"), "_", "-"),
		SubscribeURL: "/wp-admin/admin-post?action=mtnc_subscribe",
		UnlockURL:    "/wp-admin/admin-post?action=mtnc_unlock",
		Nonce:        s.auth.Nonce("mtnc_public", 0, ""),
		Subscribed:   r.URL.Query().Get("mtnc_subscribed") == "1",
		SubscribeErr: maintSubscribeErrors[r.URL.Query().Get("mtnc_error")],
		Unlocked:     r.URL.Query().Get("mtnc_unlock"),
		LoginAction:  s.loginPath(),
		RedirectTo:   r.URL.RequestURI(),
	}
	_, v.Preview = r.URL.Query()[maintPreviewParam]
	if lang != nil {
		v.Lang, v.RTL = lang.W3C(), lang.RTL
		w.Header().Add("Vary", "Accept-Language, Cookie")
	}
	if v.Lang == "" {
		v.Lang = "en-US"
	}
	if v.Title == "" {
		v.Title = v.SiteName
	}
	if end, ok := s.maintEnd(st); ok && end.After(time.Now()) {
		v.EndUnix, v.EndISO = end.Unix(), end.Format(time.RFC3339)
	}
	for _, n := range maintSocialNetworks {
		if u := strings.TrimSpace(st.Social[n.Key]); u != "" {
			if n.Key == "email" && !strings.HasPrefix(u, "mailto:") {
				u = "mailto:" + u
			}
			v.Social = append(v.Social, maintSocialLink{URL: u, Label: n.Label, Icon: n.Icon})
		}
	}
	if st.GDPRPolicyLink {
		if id := s.settings.Int("wp_page_for_privacy_policy"); id > 0 {
			if p, err := s.store.GetPost(ctx, int64(id)); err == nil && p.Status == "publish" {
				v.PolicyURL = s.links(ctx).Permalink(p)
			}
		}
	}
	if st.Analytics != "" {
		tag := `<script async src="https://www.googletagmanager.com/gtag/js?id=` + template.HTMLEscapeString(st.Analytics) + `"` + cspNonceAttr() + `></script>` + "\n" +
			`<script` + cspNonceAttr() + `>window.dataLayer=window.dataLayer||[];function gtag(){dataLayer.push(arguments);}gtag('js',new Date());gtag('config',` + maintJSString(st.Analytics) + `);</script>`
		if s.gooikeOn() {
			// Analytics must not run before consent: Consent Mode defaults, the tag held back in the
			// statistics category and the banner, exactly as on the site's own pages.
			gs := s.gooikeLoad()
			s.gooikeLocalize(ctx, &gs)
			v.ConsentHead = template.HTML(gooikeConsentModeScript(&gs) +
				`<link rel="stylesheet" id="gooike-css" href="` + gooikeAssets + `/gooike.css?ver=` + gooikeVersion + `" media="all" />` + "\n" +
				`<style id="gooike-inline-css">` + gooikeCSSVars(gs.Design) + gooikeCustomCSS(gs.Design) + "</style>\n")
			tag = gooikeInert(tag, "statistics")
			v.ConsentFoot = template.HTML(s.gooikeMarkup(ctx, &gs, false) + gooikeConfigScript(&gs, false, s.cspMode() == cspModeEnforce) +
				`<script` + cspNonceAttr() + ` src="` + gooikeAssets + `/gooike.js?ver=` + gooikeVersion + `" id="gooike-js" defer></script>` + "\n")
		}
		v.AnalyticsTag = template.HTML(tag)
	}
	v.HasModules = v.EndUnix > 0 || st.Progress || st.Subscribe || len(v.Social) > 0
	v.CSS = template.CSS(s.maintCSS(st))

	var buf bytes.Buffer
	if err := maintPageTmpl.Execute(&buf, v); err != nil {
		log.Printf("maintenance: page: %v", err)
		http.Error(w, "The site is undergoing maintenance.", http.StatusServiceUnavailable)
		return
	}
	s.maintHeaders(w, st, "text/html; charset=utf-8")
	s.maintStatus(w, st, status)
	if r.Method != http.MethodHead {
		_, _ = w.Write(buf.Bytes())
	}
}

// maintCSS is the page's custom properties from the Design tab, then the admin's custom CSS.
func (s *Server) maintCSS(st maintSettings) string {
	var b strings.Builder
	bg := maintColor(st.BgColor, "#111111")
	bg2 := maintColor(st.BgColor2, bg)
	b.WriteString(":root{")
	b.WriteString("--mtnc-bg:" + bg + ";--mtnc-bg2:" + bg2 + ";")
	b.WriteString("--mtnc-text:" + maintColor(st.FontColor, "#ffffff") + ";")
	b.WriteString("--mtnc-heading:" + maintColor(st.HeadingColor, "#ffffff") + ";")
	b.WriteString("--mtnc-link:" + maintColor(st.LinkColor, "#7cc4ff") + ";")
	b.WriteString("--mtnc-accent:" + maintColor(st.AccentColor, "#2271b1") + ";")
	size := st.FontSize
	if size < 12 || size > 32 {
		size = 16
	}
	b.WriteString("--mtnc-size:" + strconv.Itoa(size) + "px;")
	width := st.ContentWidth
	if width < 320 || width > 1600 {
		width = 760
	}
	b.WriteString("--mtnc-width:" + strconv.Itoa(width) + "px;")
	logo := st.LogoWidth
	if logo < 16 || logo > 1200 {
		logo = 220
	}
	b.WriteString("--mtnc-logo:" + strconv.Itoa(logo) + "px;")
	ov := st.Overlay
	if ov < 0 {
		ov = 0
	} else if ov > 90 {
		ov = 90
	}
	b.WriteString("--mtnc-overlay:" + strconv.FormatFloat(float64(ov)/100, 'f', 2, 64) + ";")
	blur := st.BgBlur
	if blur < 0 || blur > 40 {
		blur = 0
	}
	b.WriteString("--mtnc-blur:" + strconv.Itoa(blur) + "px;")
	img := ""
	if st.BgType == "image" {
		img = maintCSSURL(st.BgImageURL)
	}
	if img != "" {
		b.WriteString(`--mtnc-image:url("` + img + `");`)
	} else {
		b.WriteString("--mtnc-image:none;")
	}
	sizes := map[string]string{"cover": "cover", "contain": "contain", "auto": "auto"}
	b.WriteString("--mtnc-bgsize:" + map[bool]string{true: sizes[st.BgSize], false: "cover"}[sizes[st.BgSize] != ""] + ";")
	pos := map[string]bool{"center center": true, "top center": true, "bottom center": true, "center left": true, "center right": true}
	b.WriteString("--mtnc-bgpos:" + map[bool]string{true: st.BgPosition, false: "center center"}[pos[st.BgPosition]] + ";")
	b.WriteString("}\n")
	// The admin's own CSS cannot close the style element: no "<" reaches it (maintSafeCSS).
	b.WriteString(maintSafeCSS(st.CustomCSS))
	return b.String()
}

// maintSafeCSS writes every "<" as the CSS escape \3C, so no tag can open or close inside the
// style element whatever the nesting or case, while a "<" in a string or selector keeps its meaning.
func maintSafeCSS(css string) string {
	return strings.ReplaceAll(css, "<", `\3C `)
}

// maintJSString is a JavaScript string literal of a short, already validated identifier.
func maintJSString(v string) string {
	raw, _ := json.Marshal(v)
	return string(raw)
}
