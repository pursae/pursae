package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"html/template"
	"net/http"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"

	"wpgo/internal/store"
)

// Pursae Pixel is a native tracking plugin in the class of PixelYourSite Pro, Pixel Manager for
// WooCommerce and GTM4WP: one place for the Meta Pixel, Google Analytics 4, Google Ads, TikTok,
// Pinterest and Microsoft Advertising tags, with a shared event model (page view, search, scroll,
// time on page, clicks, downloads, forms, the full WooCommerce funnel and custom events with
// triggers), server-side delivery (Meta Conversions API, TikTok Events API, Pinterest Conversions API,
// GA4 Measurement Protocol) with browser/server deduplication through one event id, attribution
// (UTM and click ids), a Meta/Google product catalog feed and a delivery log.
//
// Like every native plugin it is off on a fresh install (native_pixelgo). While it is off nothing of
// it is printed on the site, its routes answer 404 and the admin menu entry is gone; the settings and
// the log stay. It is built for the full-page cache: every page prints the same HTML for every
// visitor (pixelgo.js builds events in the browser); only the order-received page carries
// per-order data, and that page is never cached.
//
// Consent: when Pursae Cookie is active the tags are printed inert (gooikeInert) in their consent
// category and activated by Pursae Cookie once the visitor accepts it; pixelgo.js queues events until
// a platform is loaded and drops them if consent is withdrawn. The server-side routes check the
// consent cookie again.

const (
	pixelSlug      = "pixelgo"
	pixelSetting   = "native_pixelgo"
	pixelOptions   = "pixelgo_settings"
	pixelVersion   = "0.1.0"
	pixelNamespace = "pixelgo/v1"
	pixelAssets    = "/wp-content/plugins/pixelgo/assets"
	pixelPurgeJob  = "pixelgo_purge_logs"
	pixelNonce     = "pixelgo-admin"
)

// pixelPlatformDef describes one advertising / analytics platform.
type pixelPlatformDef struct {
	Key, Name, IDLabel, IDExample, Category string
	IDPattern                               *regexp.Regexp
	TokenLabel                              string // "" when the platform has no server-side API
	TokenHelp                               string
	AccountLabel                            string // Pinterest: the ad account id
	TestLabel                               string // the test-events code field
	Script, Connect, Img                    []string
	Cookies                                 []gooikeCookieRow
}

var pixelPlatforms = []*pixelPlatformDef{
	{Key: "meta", Name: "Meta Pixel (Facebook & Instagram)", IDLabel: "Pixel ID", IDExample: "123456789012345", Category: "marketing",
		IDPattern:  regexp.MustCompile(`^[0-9]{6,20}$`),
		TokenLabel: "Conversions API access token", TokenHelp: "Events Manager › your pixel › Settings › Conversions API › Generate access token.",
		TestLabel: "Test event code (TEST12345)",
		Script:    []string{"https://connect.facebook.net"}, Connect: []string{"https://www.facebook.com", "https://connect.facebook.net"},
		Img: []string{"https://www.facebook.com"},
		Cookies: []gooikeCookieRow{
			{Name: "_fbp", Provider: "Meta", Purpose: "Identifies browsers to deliver and measure Meta advertising.", Duration: "3 months"},
			{Name: "_fbc", Provider: "Meta", Purpose: "Stores the click id of a Meta ad you came from.", Duration: "3 months"},
		}},
	{Key: "ga4", Name: "Google Analytics 4", IDLabel: "Measurement ID", IDExample: "G-XXXXXXXXXX", Category: "statistics",
		IDPattern:  regexp.MustCompile(`^G-[A-Z0-9]{4,16}$`),
		TokenLabel: "Measurement Protocol API secret", TokenHelp: "Admin › Data streams › your stream › Measurement Protocol API secrets › Create.",
		Script:  []string{"https://www.googletagmanager.com"},
		Connect: []string{"https://*.google-analytics.com", "https://*.analytics.google.com", "https://www.googletagmanager.com"},
		Img:     []string{"https://*.google-analytics.com", "https://www.googletagmanager.com"},
		Cookies: []gooikeCookieRow{
			{Name: "_ga", Provider: "Google Analytics", Purpose: "Distinguishes visitors to count visits and page views.", Duration: "2 years"},
			{Name: "_ga_*", Provider: "Google Analytics", Purpose: "Keeps the session state of a Google Analytics 4 property.", Duration: "2 years"},
		}},
	{Key: "gads", Name: "Google Ads", IDLabel: "Conversion ID", IDExample: "AW-123456789", Category: "marketing",
		IDPattern: regexp.MustCompile(`^AW-[0-9]{6,14}$`),
		Script:    []string{"https://www.googletagmanager.com"},
		Connect:   []string{"https://www.google.com", "https://*.doubleclick.net", "https://*.googleadservices.com", "https://www.googletagmanager.com"},
		Img:       []string{"https://www.google.com", "https://*.doubleclick.net", "https://*.googleadservices.com"},
		Cookies: []gooikeCookieRow{
			{Name: "_gcl_au", Provider: "Google Ads", Purpose: "Stores ad click information to measure conversions.", Duration: "3 months"},
		}},
	{Key: "tiktok", Name: "TikTok Pixel", IDLabel: "Pixel code", IDExample: "C1ABCDEFGHIJKLMNOPQR", Category: "marketing",
		IDPattern:  regexp.MustCompile(`^[A-Z0-9]{10,32}$`),
		TokenLabel: "Events API access token", TokenHelp: "TikTok Events Manager › your pixel › Settings › Web Events › Set up Events API › Generate access token.",
		TestLabel: "Test event code (TEST12345)",
		Script:    []string{"https://analytics.tiktok.com"}, Connect: []string{"https://analytics.tiktok.com", "https://business-api.tiktok.com"},
		Img: []string{"https://analytics.tiktok.com"},
		Cookies: []gooikeCookieRow{
			{Name: "_ttp", Provider: "TikTok", Purpose: "Measures and improves the performance of TikTok advertising.", Duration: "13 months"},
		}},
	{Key: "pinterest", Name: "Pinterest Tag", IDLabel: "Tag ID", IDExample: "2612345678901", Category: "marketing",
		IDPattern:  regexp.MustCompile(`^[0-9]{10,16}$`),
		TokenLabel: "Conversions API access token", TokenHelp: "Pinterest Ads › Conversions › Conversions API › Generate token.",
		AccountLabel: "Ad account ID",
		Script:       []string{"https://s.pinimg.com"}, Connect: []string{"https://ct.pinterest.com", "https://s.pinimg.com"},
		Img: []string{"https://ct.pinterest.com"},
		Cookies: []gooikeCookieRow{
			{Name: "_pin_unauth", Provider: "Pinterest", Purpose: "Groups actions of visitors who are not signed in to Pinterest.", Duration: "1 year"},
		}},
	{Key: "bing", Name: "Microsoft Advertising (UET)", IDLabel: "UET tag ID", IDExample: "12345678", Category: "marketing",
		IDPattern: regexp.MustCompile(`^[0-9]{5,12}$`),
		Script:    []string{"https://bat.bing.com"}, Connect: []string{"https://bat.bing.com", "https://bat.bing.net"},
		Img: []string{"https://bat.bing.com", "https://bat.bing.net"},
		Cookies: []gooikeCookieRow{
			{Name: "_uetsid", Provider: "Microsoft Advertising", Purpose: "Stores the session of a visitor for ad measurement.", Duration: "1 day"},
			{Name: "_uetvid", Provider: "Microsoft Advertising", Purpose: "Recognizes returning visitors for ad measurement.", Duration: "13 months"},
		}},
	{Key: "gtm", Name: "Google Tag Manager (data layer)", IDLabel: "Container ID", IDExample: "GTM-XXXXXXX", Category: "statistics",
		IDPattern: regexp.MustCompile(`^GTM-[A-Z0-9]{4,12}$`),
		Script:    []string{"https://www.googletagmanager.com", "https://tagmanager.google.com"},
		Connect:   []string{"https://www.googletagmanager.com", "https://*.google-analytics.com", "https://*.analytics.google.com"},
		Img:       []string{"https://www.googletagmanager.com", "https://*.google-analytics.com"},
		Cookies: []gooikeCookieRow{
			{Name: "_ga", Provider: "Google Analytics", Purpose: "Distinguishes visitors to count visits and page views (when your container loads Google Analytics).", Duration: "2 years"},
		}},
}

func pixelPlatformByKey(k string) *pixelPlatformDef {
	for _, p := range pixelPlatforms {
		if p.Key == k {
			return p
		}
	}
	return nil
}

// pixelPlatformCfg is the saved configuration of a platform.
type pixelPlatformCfg struct {
	Enabled    bool              `json:"enabled"`
	IDs        []string          `json:"ids"`
	Token      string            `json:"token,omitempty"`
	ServerSide bool              `json:"server_side"`
	TestCode   string            `json:"test_code,omitempty"`
	AccountID  string            `json:"account_id,omitempty"`
	Labels     map[string]string `json:"labels,omitempty"` // Google Ads: event key → conversion label
}

// pixelCustomEvent is a user-defined event with a trigger.
type pixelCustomEvent struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Enabled   bool     `json:"enabled"`
	Trigger   string   `json:"trigger"`  // page_visit, click, scroll, time, form_submit
	Selector  string   `json:"selector"` // click, form_submit: a CSS selector
	URL       string   `json:"url"`      // page_visit: URL contains (also limits the other triggers when set)
	Number    int      `json:"number"`   // scroll: percent, time: seconds
	Value     string   `json:"value"`    // a fixed value, e.g. 10.00
	Currency  string   `json:"currency"`
	Params    string   `json:"params"`    // one key=value per line
	Platforms []string `json:"platforms"` // empty: every enabled platform
	Once      bool     `json:"once"`      // once per visitor session
}

// pixelSettings are every Pursae Pixel setting (the JSON option pixelgo_settings).
type pixelSettings struct {
	Platforms      map[string]pixelPlatformCfg `json:"platforms"`
	RequireConsent bool                        `json:"require_consent"`
	ExcludeRoles   []string                    `json:"exclude_roles"`

	General      map[string]bool `json:"general"`
	ScrollDepths string          `json:"scroll_depths"`
	TimeSeconds  string          `json:"time_seconds"`
	DownloadExts string          `json:"download_exts"`

	Woo          map[string]bool `json:"woo"`
	ContentID    string          `json:"content_id"` // id, sku
	IDPrefix     string          `json:"id_prefix"`
	IDSuffix     string          `json:"id_suffix"`
	VariationIDs bool            `json:"variation_ids"` // events and feed use the id of the chosen variation
	ValueMode    string          `json:"value_mode"`    // total, subtotal, net
	PurchaseOnce bool            `json:"purchase_once"`
	Advanced     bool            `json:"advanced_matching"`

	Attribution bool `json:"attribution"`
	CookieDays  int  `json:"cookie_days"`

	Custom []pixelCustomEvent `json:"custom"`

	FeedEnabled   bool   `json:"feed_enabled"`
	FeedToken     string `json:"feed_token"`
	FeedBrand     string `json:"feed_brand"`
	FeedCondition string `json:"feed_condition"` // new, refurbished, used
	FeedGoogleCat string `json:"feed_google_category"`

	LogServer    bool `json:"log_server"`
	LogRetention int  `json:"log_retention_days"`
	Debug        bool `json:"debug"`
}

// pixelGeneralEvents are the switches of the Events screen (general events).
var pixelGeneralEvents = []struct{ Key, Label, Help string }{
	{"page_view", "Page view", "PageView / page_view on every page. Server-side copies share the same event id."},
	{"search", "Search", "The Search event on the site's search results (and the store's product search)."},
	{"scroll", "Page scroll", "Fires at the depths set below, once per page."},
	{"time", "Time on page", "Fires after the number of seconds set below, counted while the tab is visible."},
	{"tel", "Phone link clicks", "Clicks on tel: links send a Contact event."},
	{"mail", "E-mail link clicks", "Clicks on mailto: links send a Contact event."},
	{"outbound", "Outbound link clicks", "Clicks on links to other sites send an OutboundClick event."},
	{"download", "File downloads", "Clicks on links to the file types set below send a Download event."},
	{"form", "Form submissions", "Any form (Pursae Contact Form included, except the store's own and search forms) sends a Lead event."},
	{"comment", "Comments", "A submitted comment form sends a Comment event."},
	{"video", "HTML5 video", "Play and complete events of <video> elements."},
	{"registration", "Registrations", "The WordPress / store registration form sends CompleteRegistration."},
}

// pixelWooEvents are the switches of the WooCommerce section.
var pixelWooEvents = []struct{ Key, Label, Help string }{
	{"view_content", "Product page view (ViewContent)", ""},
	{"view_category", "Category / shop view (ViewCategory)", "Item list of a product category, tag or the shop."},
	{"add_to_cart", "Add to cart (AddToCart)", "Single-product and loop buttons, including AJAX add to cart."},
	{"remove_from_cart", "Remove from cart (RemoveFromCart)", ""},
	{"view_cart", "Cart page (ViewCart)", ""},
	{"initiate_checkout", "Checkout page (InitiateCheckout)", ""},
	{"add_payment_info", "Payment info (AddPaymentInfo)", "Fires when the visitor picks a payment method or places the order."},
	{"purchase", "Purchase", "On the order-received page, and server-side when a payment completes."},
}

func pixelDefaults() pixelSettings {
	return pixelSettings{
		Platforms:      map[string]pixelPlatformCfg{},
		RequireConsent: true,
		ExcludeRoles:   []string{"administrator"},
		General: map[string]bool{"page_view": true, "search": true, "tel": true, "mail": true, "outbound": false,
			"download": true, "form": true, "comment": true, "scroll": false, "time": false, "video": false, "registration": true},
		ScrollDepths: "25,50,75,100",
		TimeSeconds:  "30,60,120",
		DownloadExts: "pdf,zip,doc,docx,xls,xlsx,ppt,pptx,csv,mp3,mp4,epub",
		Woo: map[string]bool{"view_content": true, "view_category": true, "add_to_cart": true, "remove_from_cart": true,
			"view_cart": true, "initiate_checkout": true, "add_payment_info": true, "purchase": true},
		ContentID:     "id",
		ValueMode:     "total",
		PurchaseOnce:  true,
		Attribution:   true,
		CookieDays:    90,
		FeedCondition: "new",
		LogServer:     true,
		LogRetention:  30,
	}
}

// pixelOn reports whether the plugin is active: one cached option read.
func (s *Server) pixelOn() bool { return s.settings.Bool(pixelSetting) }

// pixelOnly is the middleware of the plugin's own routes: 404 while it is off.
func (s *Server) pixelOnly() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !s.pixelOn() {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		c.Next()
	}
}

// pixelRESTHidden hides the plugin's REST routes while it is off.
func (s *Server) pixelRESTHidden(ns string) bool { return ns == pixelNamespace && !s.pixelOn() }

// pixelLoad returns the saved settings over the defaults, sanitized.
func (s *Server) pixelLoad() pixelSettings {
	st := pixelDefaults()
	if v := s.settings.Get(pixelOptions); v != nil {
		if raw, err := json.Marshal(v); err == nil {
			var saved pixelSettings
			if json.Unmarshal(raw, &saved) == nil {
				st = pixelMerge(st, saved)
			}
		}
	}
	st = pixelSanitize(st)
	for k, cfg := range st.Platforms { // API tokens are stored sealed (pixelSave)
		cfg.Token = s.revealSecret(cfg.Token)
		st.Platforms[k] = cfg
	}
	return st
}

// pixelMerge lays saved settings over the defaults: maps are merged key by key so a release that
// adds an event switch gets its default.
func pixelMerge(def, saved pixelSettings) pixelSettings {
	for k, v := range def.General {
		if _, ok := saved.General[k]; !ok {
			if saved.General == nil {
				saved.General = map[string]bool{}
			}
			saved.General[k] = v
		}
	}
	for k, v := range def.Woo {
		if _, ok := saved.Woo[k]; !ok {
			if saved.Woo == nil {
				saved.Woo = map[string]bool{}
			}
			saved.Woo[k] = v
		}
	}
	if saved.ScrollDepths == "" {
		saved.ScrollDepths = def.ScrollDepths
	}
	if saved.TimeSeconds == "" {
		saved.TimeSeconds = def.TimeSeconds
	}
	if saved.DownloadExts == "" {
		saved.DownloadExts = def.DownloadExts
	}
	return saved
}

var (
	pixelNameRe  = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_ -]{0,39}$`)
	pixelParamRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,39}$`)
	pixelNumsRe  = regexp.MustCompile(`[^0-9,]+`)
	pixelExtRe   = regexp.MustCompile(`[^a-z0-9]+`)
	pixelCurRe   = regexp.MustCompile(`^[A-Z]{3}$`)
	pixelIDRe    = regexp.MustCompile(`^[a-z0-9]{6,16}$`)
	pixelAcctRe  = regexp.MustCompile(`^[0-9]{0,24}$`)
	pixelLabelRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	pixelValueRe = regexp.MustCompile(`^[0-9]{1,9}(\.[0-9]{1,4})?$`)
)

// pixelSanitize keeps the settings consistent: IDs match their platform, names and numbers are valid.
func pixelSanitize(st pixelSettings) pixelSettings {
	if st.Platforms == nil {
		st.Platforms = map[string]pixelPlatformCfg{}
	}
	for _, def := range pixelPlatforms {
		cfg := st.Platforms[def.Key]
		var ids []string
		seen := map[string]bool{}
		for _, id := range cfg.IDs {
			id = strings.TrimSpace(id)
			if def.Key == "ga4" || def.Key == "gads" || def.Key == "tiktok" || def.Key == "gtm" {
				id = strings.ToUpper(id)
			}
			if id != "" && def.IDPattern.MatchString(id) && !seen[id] && len(ids) < 10 {
				seen[id] = true
				ids = append(ids, id)
			}
		}
		cfg.IDs = ids
		cfg.Token = strings.TrimSpace(cfg.Token)
		if !secretLooksSealed(cfg.Token) && (len(cfg.Token) > 600 || strings.ContainsAny(cfg.Token, " \t\r\n\"'<>&")) {
			cfg.Token = "" // a sealed token (pixelSave) was checked in clear before it was sealed
		}
		cfg.TestCode = strings.TrimSpace(cfg.TestCode)
		if len(cfg.TestCode) > 40 || strings.ContainsAny(cfg.TestCode, " \"'<>&") {
			cfg.TestCode = ""
		}
		cfg.AccountID = strings.TrimSpace(cfg.AccountID)
		if !pixelAcctRe.MatchString(cfg.AccountID) {
			cfg.AccountID = ""
		}
		labels := map[string]string{}
		for ev, l := range cfg.Labels {
			l = strings.TrimSpace(l)
			if l != "" && len(l) <= 64 && pixelLabelRe.MatchString(l) {
				labels[ev] = l
			}
		}
		cfg.Labels = labels
		if len(cfg.IDs) == 0 {
			cfg.Enabled = false
		}
		if cfg.Token == "" || def.TokenLabel == "" {
			cfg.ServerSide = false
		}
		st.Platforms[def.Key] = cfg
	}
	switch st.ContentID {
	case "id", "sku":
	default:
		st.ContentID = "id"
	}
	switch st.ValueMode {
	case "total", "subtotal", "net":
	default:
		st.ValueMode = "total"
	}
	switch st.FeedCondition {
	case "new", "refurbished", "used":
	default:
		st.FeedCondition = "new"
	}
	st.IDPrefix, st.IDSuffix = pixelTrim(st.IDPrefix, 20), pixelTrim(st.IDSuffix, 20)
	st.FeedBrand, st.FeedGoogleCat = pixelTrim(st.FeedBrand, 100), pixelTrim(st.FeedGoogleCat, 250)
	st.ScrollDepths = pixelNumList(st.ScrollDepths, 1, 100, 8)
	st.TimeSeconds = pixelNumList(st.TimeSeconds, 1, 3600, 8)
	st.DownloadExts = strings.Trim(pixelExtRe.ReplaceAllString(strings.ToLower(st.DownloadExts), ","), ",")
	if st.CookieDays < 1 || st.CookieDays > 400 {
		st.CookieDays = 90
	}
	if st.LogRetention < 1 || st.LogRetention > 365 {
		st.LogRetention = 30
	}
	if st.FeedEnabled && len(st.FeedToken) < 16 {
		st.FeedToken = pixelRandom(16)
	}
	var custom []pixelCustomEvent
	seenIDs := map[string]bool{}
	for _, ce := range st.Custom {
		ce.Name = strings.TrimSpace(ce.Name)
		if !pixelNameRe.MatchString(ce.Name) || len(custom) >= 50 {
			continue
		}
		if !pixelIDRe.MatchString(ce.ID) || seenIDs[ce.ID] {
			ce.ID = pixelRandom(6)
		}
		seenIDs[ce.ID] = true
		switch ce.Trigger {
		case "page_visit", "click", "scroll", "time", "form_submit":
		default:
			ce.Trigger = "page_visit"
		}
		ce.Selector = pixelSelector(ce.Selector)
		if (ce.Trigger == "click" || ce.Trigger == "form_submit") && ce.Selector == "" {
			continue
		}
		ce.URL = pixelTrim(strings.TrimSpace(ce.URL), 200)
		switch ce.Trigger {
		case "scroll":
			ce.Number = min(max(ce.Number, 1), 100)
		case "time":
			ce.Number = min(max(ce.Number, 1), 3600)
		default:
			ce.Number = 0
		}
		ce.Value = strings.TrimSpace(ce.Value)
		if !pixelValueRe.MatchString(ce.Value) {
			ce.Value = ""
		}
		ce.Currency = strings.ToUpper(strings.TrimSpace(ce.Currency))
		if !pixelCurRe.MatchString(ce.Currency) {
			ce.Currency = ""
		}
		ce.Params = strings.Join(pixelParamLines(ce.Params), "\n")
		var pl []string
		for _, p := range ce.Platforms {
			if pixelPlatformByKey(p) != nil && !pixelHas(pl, p) {
				pl = append(pl, p)
			}
		}
		ce.Platforms = pl
		custom = append(custom, ce)
	}
	st.Custom = custom
	return st
}

// pixelSelector accepts a CSS selector without anything that could break out of the page config.
func pixelSelector(v string) string {
	v = strings.TrimSpace(v)
	if len(v) > 200 || strings.ContainsAny(v, "<>`\\{}\n\r") {
		return ""
	}
	return v
}

func pixelTrim(v string, n int) string {
	v = strings.TrimSpace(v)
	if len(v) > n {
		v = v[:n]
	}
	return strings.Map(func(r rune) rune {
		if r < 32 || r == '<' || r == '>' {
			return -1
		}
		return r
	}, v)
}

// pixelNumList cleans "25, 50;75" into "25,50,75" within a range, sorted and limited.
func pixelNumList(v string, lo, hi, limit int) string {
	var nums []int
	for _, p := range strings.Split(pixelNumsRe.ReplaceAllString(v, ","), ",") {
		n := 0
		for _, r := range p {
			n = n*10 + int(r-'0')
			if n > 100000 {
				break
			}
		}
		if p != "" && n >= lo && n <= hi {
			ins := true
			for _, x := range nums {
				ins = ins && x != n
			}
			if ins && len(nums) < limit {
				nums = append(nums, n)
			}
		}
	}
	for i := range nums {
		for j := i + 1; j < len(nums); j++ {
			if nums[j] < nums[i] {
				nums[i], nums[j] = nums[j], nums[i]
			}
		}
	}
	out := make([]string, len(nums))
	for i, n := range nums {
		out[i] = itoa64(int64(n))
	}
	return strings.Join(out, ",")
}

// pixelParamLines keeps the valid "key=value" lines of the custom parameters.
func pixelParamLines(v string) []string {
	var out []string
	for _, ln := range strings.Split(strings.ReplaceAll(v, "\r", ""), "\n") {
		k, val, ok := strings.Cut(ln, "=")
		k, val = strings.TrimSpace(k), pixelTrim(val, 200)
		if ok && pixelParamRe.MatchString(k) && len(out) < 20 {
			out = append(out, k+"="+val)
		}
	}
	return out
}

func pixelHas(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func pixelRandom(n int) string {
	b := make([]byte, (n+1)/2)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)[:n]
}

// pixelSave stores the settings and drops every cached page: the tags are in them.
func (s *Server) pixelSave(ctx context.Context, st pixelSettings) error {
	st = pixelSanitize(st)
	for k, cfg := range st.Platforms { // API tokens are stored sealed; pixelLoad opens them
		cfg.Token = s.sealSecret(cfg.Token)
		st.Platforms[k] = cfg
	}
	if err := s.settings.Set(ctx, map[string]any{pixelOptions: st}); err != nil {
		return err
	}
	s.purgeAll(ctx)
	return nil
}

// pixelActive lists the enabled platforms (with at least one ID), in display order.
func (st *pixelSettings) active() []*pixelPlatformDef {
	var out []*pixelPlatformDef
	for _, d := range pixelPlatforms {
		if c := st.Platforms[d.Key]; c.Enabled && len(c.IDs) > 0 {
			out = append(out, d)
		}
	}
	return out
}

// serverReady lists the platforms that deliver server-side.
func (st *pixelSettings) serverReady() []*pixelPlatformDef {
	var out []*pixelPlatformDef
	for _, d := range st.active() {
		if c := st.Platforms[d.Key]; c.ServerSide && c.Token != "" {
			out = append(out, d)
		}
	}
	return out
}

// pixelActivate switches the plugin on and schedules the daily log purge.
func (s *Server) pixelActivate(ctx context.Context) error {
	if err := s.settings.Set(ctx, map[string]any{pixelSetting: true}); err != nil {
		return err
	}
	if _, err := s.store.Pool.Exec(ctx, `INSERT INTO jobs(name, run_at, recurrence)
		SELECT $1, now() + interval '10 minutes', 'daily' WHERE NOT EXISTS (SELECT 1 FROM jobs WHERE name = $1 AND recurrence <> '')`, pixelPurgeJob); err != nil {
		return err
	}
	s.purgeAll(ctx)
	return nil
}

// pixelDeactivate switches the plugin off. Settings and the log are kept.
func (s *Server) pixelDeactivate(ctx context.Context) error {
	if err := s.settings.Set(ctx, map[string]any{pixelSetting: false}); err != nil {
		return err
	}
	_, _ = s.store.Pool.Exec(ctx, `DELETE FROM jobs WHERE name = $1`, pixelPurgeJob)
	s.purgeAll(ctx)
	return nil
}

// pixelPluginRow is the plugin's row on the Plugins screen.
func (s *Server) pixelPluginRow() pluginRow {
	r := pluginRow{Slug: pixelSlug, Name: "Pursae Pixel", Version: pixelVersion, Native: true, Active: s.pixelOn(),
		Description: "Every ad and analytics tag in one place: Meta Pixel, Google Analytics 4, Google Ads, TikTok, Pinterest and Microsoft Advertising with the full WooCommerce funnel, custom events with triggers, server-side delivery (Conversions API) with deduplication, UTM and click-id attribution, a product catalog feed and a delivery log. Honors Pursae Cookie. While it is off, nothing of it reaches the site.",
		Author:      "Pursae"}
	if r.Active {
		r.SettingsURL = "/wp-admin/pixelgo"
	}
	return r
}

// pixelPluginAction is the Activate / Deactivate link of the Plugins screen.
func (s *Server) pixelPluginAction(c *gin.Context, action string) {
	var err error
	if action == "activate" {
		err = s.pixelActivate(c)
	} else {
		err = s.pixelDeactivate(c)
	}
	if err != nil {
		c.Redirect(http.StatusFound, "/wp-admin/plugins?error="+template.URLQueryEscaper(err.Error()))
		return
	}
	c.Redirect(http.StatusFound, "/wp-admin/plugins?"+action+"=true")
}

// pixelMenuPages are the screens of the Pursae Pixel menu.
var pixelMenuPages = []struct{ Page, Title string }{
	{"", "Dashboard"}, {"pixels", "Pixels &amp; APIs"}, {"events", "Events"}, {"custom", "Custom Events"},
	{"feed", "Product Feed"}, {"logs", "Delivery Log"}, {"settings", "Settings"},
}

// pixelMenu adds the Pursae Pixel top-level menu after Settings, only while the plugin is active.
func (s *Server) pixelMenu(menu []*menuTop, u *store.User, screen string) []*menuTop {
	if !s.pixelOn() || u == nil || !u.Can("manage_options", nil) {
		return menu
	}
	top := &menuTop{Slug: "pixelgo", Title: "Pursae Pixel", ID: "toplevel_page_pixelgo", Icon: "dashicons-chart-area"}
	for i, p := range pixelMenuPages {
		slug := "pixelgo"
		if p.Page != "" {
			slug += "?page=" + p.Page
		}
		ms := &menuSub{Slug: slug, Title: template.HTML(p.Title), First: i == 0, Current: slug == screen}
		top.Current = top.Current || ms.Current
		top.Sub = append(top.Sub, ms)
	}
	at := len(menu)
	for i, m := range menu {
		if m.ID == "menu-settings" {
			at = i + 1
			m.Last = false
		}
	}
	// Directly after the Settings menu (and after Pursae Cookie when it already sits there).
	for at < len(menu) && !menu[at].Separator && menu[at].ID == "toplevel_page_gooike" {
		at++
	}
	top.Last = at == len(menu) || menu[at].Separator
	out := append([]*menuTop{}, menu[:at]...)
	out = append(out, top)
	return append(out, menu[at:]...)
}

// registerPixel wires the plugin's routes; every one answers 404 while it is off.
func (s *Server) registerPixel(r *gin.Engine, admin *gin.RouterGroup) {
	s.static(r, pixelAssets, "pixelgo", s.pixelOnly())
	admin.GET("/pixelgo", s.pixelOnly(), s.pixelAdmin)
	admin.POST("/pixelgo", s.pixelOnly(), s.pixelAdmin)
	r.GET("/pixelgo-feed/:token", s.pixelOnly(), s.pixelFeed)
}
