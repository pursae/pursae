package server

import (
	"bytes"
	"context"
	"encoding/xml"
	"image/png"
	"io/fs"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"wpgo/web"
)

// uiFakeGateway is a registered gateway with chosen info and setup problem, built on the cash on delivery gateway.
type uiFakeGateway struct {
	gooGatewayDef
	id      string
	info    gooGatewayInfo
	problem string
}

func (g *uiFakeGateway) ID() string                  { return g.id }
func (g *uiFakeGateway) MethodTitle() string         { return "Fake " + g.id }
func (g *uiFakeGateway) Info() gooGatewayInfo        { return g.info }
func (g *uiFakeGateway) SetupProblem(*Server) string { return g.problem }

func uiRegisterFake(t *testing.T, id string, info gooGatewayInfo, problem string) {
	t.Helper()
	gooGatewayRegistry[id] = &uiFakeGateway{gooGatewayDef: gooGatewayRegistry["cod"], id: id, info: info, problem: problem}
	t.Cleanup(func() { delete(gooGatewayRegistry, id) })
}

// uiRegion is the markup of one region section of Settings › Payments.
func uiRegion(t *testing.T, body, region string) string {
	t.Helper()
	m := regexp.MustCompile(`(?s)<section class="goo-gw-region"[^>]*data-region="` + region + `">(.*?)</section>`).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no %s region in the page", region)
	}
	return m[1]
}

func uiCard(t *testing.T, section, id string) string {
	t.Helper()
	m := regexp.MustCompile(`(?s)<article class="goo-gw-card[^"]*" data-gateway_id="` + id + `"[^>]*>.*?</article>`).FindString(section)
	if m == "" {
		t.Fatalf("no card for %s", id)
	}
	return m
}

func TestGooGatewaySettingsRegions(t *testing.T) {
	base, c, srv := gooSite(t)
	ctx := context.Background()
	uiRegisterFake(t, "ui_bank", gooGatewayInfo{Region: gooRegionTR, Kind: "bank", Features: []string{"3ds", "installments", "query"}}, "")
	uiRegisterFake(t, "ui_psp", gooGatewayInfo{Region: gooRegionTR, Kind: "psp", Verified: true}, "The merchant key is missing.")
	uiRegisterFake(t, "ui_global", gooGatewayInfo{Region: gooRegionGlobal, Kind: "psp"}, "")
	_ = srv.gooSetOpts(ctx, map[string]any{
		"ui_psp_settings":    map[string]any{"enabled": "yes"},
		"ui_bank_settings":   map[string]any{"enabled": "yes", "testmode": "yes"},
		"ui_global_settings": map[string]any{"enabled": "yes"},
	})
	body := gooStoreGet(t, c, base+"/wp-admin/goocommerce?page=settings&tab=checkout")
	tr, gl, off := uiRegion(t, body, "tr"), uiRegion(t, body, "global"), uiRegion(t, body, "offline")
	if !strings.Contains(tr, "Bank virtual POS") || !strings.Contains(tr, "Payment providers") {
		t.Error("Türkiye lacks its two headings")
	}
	if i, j := strings.Index(tr, "Bank virtual POS"), strings.Index(tr, `data-gateway_id="ui_bank"`); i < 0 || j < i {
		t.Error("bank gateway is not under Bank virtual POS")
	}
	if !strings.Contains(tr, `data-gateway_id="ui_psp"`) || strings.Contains(tr, `data-gateway_id="ui_global"`) || strings.Contains(tr, `data-gateway_id="goo_stripe"`) {
		t.Error("Türkiye holds the wrong gateways")
	}
	if !strings.Contains(gl, `data-gateway_id="goo_stripe"`) || !strings.Contains(gl, `data-gateway_id="ui_global"`) {
		t.Error("Worldwide lacks Stripe or the fake global gateway")
	}
	for _, id := range []string{"bacs", "cheque", "cod", "goo_test"} {
		if !strings.Contains(off, `data-gateway_id="`+id+`"`) {
			t.Errorf("Offline lacks %s", id)
		}
	}
	if i, j := strings.Index(off, "Testing"), strings.Index(off, `data-gateway_id="goo_test"`); i < 0 || j < i {
		t.Error("test gateway is not under the Testing heading")
	}

	bank, psp := uiCard(t, tr, "ui_bank"), uiCard(t, tr, "ui_psp")
	gooMustContain(t, "bank card", bank, "goo-gw-badge-test", "Test mode", "3D Secure", "Installments", "Beta", "generic.svg", "Manage", "goo-gw-logobox", "goo-gw-on")
	if strings.Contains(bank, "Live") || strings.Contains(bank, ">query<") {
		t.Error("bank card shows a second status or the query feature")
	}
	gooMustContain(t, "psp card", psp, "goo-gw-badge-setup", "Setup incomplete", "The merchant key is missing.")
	if strings.Contains(psp, "Beta") {
		t.Error("verified gateway shows Beta")
	}
	if g := uiCard(t, gl, "ui_global"); !strings.Contains(g, "goo-gw-badge-setup") && !strings.Contains(g, "Live") {
		t.Errorf("global card has no status: %s", g)
	}
	if cod := uiCard(t, off, "cod"); !strings.Contains(cod, "goo-gw-badge-disabled") || !strings.Contains(cod, "Set up") || strings.Contains(cod, "Beta") {
		t.Errorf("cod card: %s", cod)
	}
	_ = srv.gooSetOpts(ctx, map[string]any{"ui_global_settings": map[string]any{"enabled": "yes"}})
	if g := uiCard(t, uiRegion(t, gooStoreGet(t, c, base+"/wp-admin/goocommerce?page=settings&tab=checkout"), "global"), "ui_global"); !strings.Contains(g, "goo-gw-badge-live") {
		t.Error("enabled complete gateway is not Live")
	}
	// The checkout order lists only the enabled methods.
	order := body[strings.Index(body, `class="goo-gw-order"`):]
	if !strings.Contains(order, `name="gateway_order[]" value="ui_bank"`) || strings.Contains(order, `name="gateway_order[]" value="cod"`) {
		t.Error("checkout order list is not the enabled methods")
	}
}

func TestGooGatewayRegionsAndDefaultTab(t *testing.T) {
	base, c, srv := gooSite(t)
	ctx := context.Background()
	body := gooStoreGet(t, c, base+"/wp-admin/goocommerce?page=settings&tab=checkout")
	if !strings.Contains(body, `data-default="global"`) {
		t.Error("a store outside Türkiye does not open on Worldwide")
	}
	// The hosted-payment providers land in their regions: unconfigured, so "Setup incomplete" and Beta.
	tr, gl := uiRegion(t, body, "tr"), uiRegion(t, body, "global")
	for _, id := range []string{"goo_paytr", "goo_iyzico", "goo_est_isbank", "goo_akbank"} {
		// Switched off and without credentials: "Disabled", Beta, and a "Set up" button.
		if !regexp.MustCompile(`(?s)data-gateway_id="` + id + `"[^>]*>.{0,900}?goo-gw-badge-disabled.*?goo-gw-badge-beta`).MatchString(tr) {
			t.Errorf("%s card is missing or has the wrong badges", id)
		}
	}
	if strings.Contains(tr, "No payment methods in this region yet") || strings.Contains(tr, "goo_paypal") {
		t.Error("Türkiye region is wrong")
	}
	// One card's markup: from its data-gateway_id to the next card's.
	card := func(section, id string) string {
		_, rest, ok := strings.Cut(section, `data-gateway_id="`+id+`"`)
		if !ok {
			t.Fatalf("no card for %s", id)
		}
		rest, _, _ = strings.Cut(rest, `data-gateway_id="`)
		return rest
	}
	if !strings.Contains(card(gl, "goo_paypal"), "goo-gw-badge-beta") || strings.Contains(card(gl, "goo_stripe"), "goo-gw-badge-beta") {
		t.Error("Worldwide region: PayPal must be Beta, Stripe must not")
	}
	_ = srv.gooSetOpts(ctx, map[string]any{"currency": "TRY"})
	if body := gooStoreGet(t, c, base+"/wp-admin/goocommerce?page=settings&tab=checkout"); !strings.Contains(body, `data-default="tr"`) {
		t.Error("TRY store does not open on Türkiye")
	}
}

func TestGooGatewayEnableRefusedAndOrderKept(t *testing.T) {
	base, c, srv := gooSite(t)
	uiRegisterFake(t, "ui_needs", gooGatewayInfo{Region: gooRegionTR, Kind: "psp"}, "The key is missing.")
	page := base + "/wp-admin/goocommerce?page=settings&tab=checkout"
	body := gooStoreGet(t, c, page)
	form := url.Values{"gateway_toggles": {"1"}, "gateway_enabled[ui_needs]": {"1"}, "gateway_enabled[cod]": {"1"}, "_wpnonce": {formNonce(t, body)}}
	code, _, loc := post(t, c, page, form)
	if code != 302 || !strings.Contains(loc, "goo-error=") || !strings.Contains(loc, "Fake") {
		t.Fatalf("enable with setup problem: %d %s", code, loc)
	}
	if srv.gooGateway("ui_needs").Enabled() || !srv.gooGateway("cod").Enabled() {
		t.Error("gateway with a setup problem was enabled, or cod was not")
	}
	// The single-gateway form refuses too.
	one := page + "&section=ui_needs"
	body = gooStoreGet(t, c, one)
	code, _, loc = post(t, c, one, url.Values{"woocommerce_ui_needs_enabled": {"1"}, "_wpnonce": {formNonce(t, body)}})
	if code != 302 || !strings.Contains(loc, "goo-error=") || srv.gooGateway("ui_needs").Enabled() {
		t.Errorf("single form enabled an incomplete gateway: %d %s", code, loc)
	}

	// Posted ids go first in posted order; the others keep their saved order behind them.
	_ = srv.gooSetOpts(context.Background(), map[string]any{"gateway_order": []string{"goo_test", "cheque", "bacs", "cod"}})
	body = gooStoreGet(t, c, page)
	form = url.Values{"gateway_toggles": {"1"}, "gateway_order[]": {"cod", "goo_test"}, "gateway_enabled[cod]": {"1"}, "gateway_enabled[goo_test]": {"1"}, "_wpnonce": {formNonce(t, body)}}
	if code, _, _ := post(t, c, page, form); code != 302 {
		t.Fatal(code)
	}
	var ids []string
	for _, g := range srv.gooGatewaysOrdered() {
		ids = append(ids, g.ID())
	}
	got := strings.Join(ids, ",")
	if !strings.HasPrefix(got, "cod,goo_test,cheque,bacs") {
		t.Errorf("order after post: %s", got)
	}
}

func TestGooGatewayLogoURL(t *testing.T) {
	_, _, srv := gooSite(t)
	ctx := context.Background()
	if u := srv.gooGatewayLogoURL(ctx, "goo_stripe"); !strings.HasPrefix(u, "http") || !strings.HasSuffix(u, "/images/payment-gateways/goo_stripe.svg") {
		t.Errorf("stripe logo: %s", u)
	}
	for _, id := range []string{"no_such_gateway", "../../etc/passwd", ""} {
		if u := srv.gooGatewayLogoURL(ctx, id); !strings.HasSuffix(u, "/images/payment-gateways/generic.svg") {
			t.Errorf("fallback for %q: %s", id, u)
		}
	}
}

func TestGooGatewayLogosAreSafeSVG(t *testing.T) {
	dir := "goocommerce/assets/images/payment-gateways"
	files, err := fs.Glob(web.FS(""), dir+"/*.svg")
	if err != nil || len(files) < 19 {
		t.Fatalf("logos: %d %v", len(files), err)
	}
	bad := regexp.MustCompile(`(?i)<script|<foreignObject|href\s*=\s*["']?\s*(https?:|//)|@font-face|@import|url\(\s*["']?https?:|<image|<use`)
	for _, f := range files {
		raw, _ := fs.ReadFile(web.FS(""), f)
		if err := xml.Unmarshal(raw, new(struct{ XMLName xml.Name })); err != nil {
			t.Errorf("%s does not parse: %v", f, err)
		}
		if bad.Match(raw) {
			t.Errorf("%s has forbidden content", f)
		}
	}
}

// A PNG logo is allowed only where the company publishes no SVG: a real, small, metadata-free-by-size image.
func TestGooGatewayLogosPNGLimits(t *testing.T) {
	dir := "goocommerce/assets/images/payment-gateways"
	files, _ := fs.Glob(web.FS(""), dir+"/*.png")
	if len(files) == 0 {
		t.Fatal("no PNG logo found (TEB's is expected)")
	}
	for _, f := range files {
		raw, _ := fs.ReadFile(web.FS(""), f)
		if !bytes.HasPrefix(raw, []byte("\x89PNG\r\n\x1a\n")) {
			t.Errorf("%s lacks the PNG signature", f)
			continue
		}
		cfg, err := png.DecodeConfig(bytes.NewReader(raw))
		if err != nil {
			t.Errorf("%s does not decode: %v", f, err)
			continue
		}
		if _, err := png.Decode(bytes.NewReader(raw)); err != nil {
			t.Errorf("%s does not fully decode: %v", f, err)
		}
		if len(raw) > 60*1024 || cfg.Width > 600 || cfg.Width < 1 || cfg.Height < 1 {
			t.Errorf("%s: %d bytes, %dx%d (limits: 60 KB, 600 px wide)", f, len(raw), cfg.Width, cfg.Height)
		}
	}
}

func TestGooGatewayLogoURLPNGFallback(t *testing.T) {
	_, _, srv := gooSite(t)
	ctx := context.Background()
	for id, want := range map[string]string{
		"goo_est_teb":  "/images/payment-gateways/goo_est_teb.png",
		"goo_paytr":    "/images/payment-gateways/goo_paytr.svg",
		"goo_unknown1": "/images/payment-gateways/generic.svg",
		"../etc":       "/images/payment-gateways/generic.svg",
	} {
		if got := srv.gooGatewayLogoURL(ctx, id); !strings.HasSuffix(got, want) {
			t.Errorf("%s: %s, want suffix %s", id, got, want)
		}
	}
}

func TestGooGatewayRESTFieldsAndCheckoutLogo(t *testing.T) {
	base, c, srv := gooSite(t)
	v3 := gooAdminAPI(t, base, c)
	r := v3("GET", "/payment_gateways/goo_stripe", nil)
	if gooJS(r.Body, "region") != "global" || gooJS(r.Body, "kind") != "psp" || !strings.HasSuffix(gooJS(r.Body, "icon"), "/goo_stripe.svg") ||
		gooJ(r.Body, "setup_complete") == nil || gooJ(r.Body, "test_mode") == nil || len(gooJ(r.Body, "features").([]any)) == 0 || gooJ(r.Body, "method_title") == nil {
		t.Errorf("single gateway: %s", r.Raw)
	}
	list := v3("GET", "/payment_gateways", nil)
	if !strings.Contains(list.Raw, `"region":"offline"`) || !strings.Contains(list.Raw, `"icon":`) {
		t.Errorf("gateway list: %.300s", list.Raw)
	}
	if gooGatewayShowsLogo(gooGatewayRegistry["cod"]) || !gooGatewayShowsLogo(gooGatewayRegistry["goo_test"]) {
		t.Error("offline methods must show no logo, others must")
	}
	// Credentials never leave through the API, and sending the blank back keeps them.
	_ = srv.gooSetOpts(context.Background(), map[string]any{"goo_paytr_settings": map[string]any{"merchant_id": "100", "merchant_key": "fake-key", "merchant_salt": "fake-salt"}})
	r = v3("GET", "/payment_gateways/goo_paytr", nil)
	if strings.Contains(r.Raw, "fake-key") || strings.Contains(r.Raw, "fake-salt") || !strings.Contains(r.Raw, `"value_set":true`) || !strings.Contains(r.Raw, `"value":"100"`) {
		t.Errorf("credentials in the gateway API: %.400s", r.Raw)
	}
	r = v3("PUT", "/payment_gateways/goo_paytr", map[string]any{"settings": map[string]any{"merchant_key": "", "merchant_id": "200"}})
	if st := srv.gooGatewaySettings("goo_paytr"); st["merchant_key"] != "fake-key" || st["merchant_id"] != "200" || strings.Contains(r.Raw, "fake-key") {
		t.Errorf("an empty credential in an update must keep the saved one: %v", st["merchant_id"])
	}

	ctx := context.Background()
	f := gooFrontCatalog(t, srv)
	_ = srv.gooSetOpts(ctx, map[string]any{"goo_test_settings": map[string]any{"enabled": "yes"}})
	z := &gooZone{Name: "US", Locations: []gooZoneLocation{{Code: "US", Type: "country"}}}
	_ = srv.gooSaveZone(ctx, z)
	_, _ = srv.gooAddMethod(ctx, z.ID, "flat_rate", map[string]any{"cost": "5"})
	fc := gooFrontClient()
	get(t, fc, base+"/?add-to-cart="+itoa64(f.mug.ID))
	_, body, _ := get(t, fc, base+"/checkout/")
	gooMustContain(t, "checkout", body, `class="goo-gateway-icon"`, `/images/payment-gateways/goo_test.svg" alt="`, `loading="lazy"`)
}

// uiCardIDs are the card ids of a section in page order.
func uiCardIDs(section string) []string {
	var ids []string
	for _, m := range regexp.MustCompile(`<article class="goo-gw-card[^"]*" data-gateway_id="([^"]+)"`).FindAllStringSubmatch(section, -1) {
		ids = append(ids, m[1])
	}
	return ids
}

func TestGooGatewayCardOrderAndTabCounts(t *testing.T) {
	base, c, srv := gooSite(t)
	page := base + "/wp-admin/goocommerce?page=settings&tab=checkout"
	body := gooStoreGet(t, c, page)
	tr := uiRegion(t, body, "tr")
	ids := uiCardIDs(tr)
	idx := func(id string) int {
		for i, v := range ids {
			if v == id {
				return i
			}
		}
		return -1
	}
	// Nothing enabled: the all.Specs() order (PayTR, iyzico, Sipay), not alphabetical.
	if !(idx("goo_paytr") >= 0 && idx("goo_paytr") < idx("goo_iyzico") && idx("goo_iyzico") < idx("goo_sipay")) {
		t.Errorf("Türkiye order is not the Specs order: %v", ids)
	}
	gl := uiCardIDs(uiRegion(t, body, "global"))
	if len(gl) < 2 || gl[0] != "goo_stripe" || gl[1] != "goo_paypal" {
		t.Errorf("Worldwide order: %v", gl)
	}
	// An enabled method moves to the front of its group and counts in the tab.
	_ = srv.gooSetOpts(context.Background(), map[string]any{"cod_settings": map[string]any{"enabled": "yes"}, "goo_test_settings": map[string]any{"enabled": "yes"}})
	body = gooStoreGet(t, c, page)
	off := uiCardIDs(uiRegion(t, body, "offline"))
	pos := func(id string) int {
		for i, v := range off {
			if v == id {
				return i
			}
		}
		return -1
	}
	if off[0] != "cod" && off[1] != "cod" || pos("bacs") < 0 || pos("bacs") > pos("cheque") || pos("cheque") > pos("goo_test") {
		t.Errorf("Offline order with cod enabled: %v", off)
	}
	// Every offline method counted (the booking gateway of Pursae Bookings may sit there too); cod and the test gateway are on.
	m := regexp.MustCompile(`data-region="offline"[^>]*>\s*<span class="goo-gw-tab-label">Offline</span> <span class="goo-gw-count"[^>]*>(\d+)/(\d+)</span>`).FindStringSubmatch(body)
	if m == nil || m[1] < "2" || m[2] != strconv.Itoa(len(off)) {
		t.Errorf("Offline tab count: %v (cards %d)", m, len(off))
	}
	if !regexp.MustCompile(`<strong>[2-9]</strong> payment methods enabled`).MatchString(body) {
		t.Error("summary does not count the enabled methods")
	}
	gooMustContain(t, "summary", body, `class="goo-gw-filter"`, `role="tabpanel"`)
}

func TestGooGatewayHeaderAndCopyableNotifyURL(t *testing.T) {
	base, c, _ := gooSite(t)
	page := base + "/wp-admin/goocommerce?page=settings&tab=checkout&section="
	body := gooStoreGet(t, c, page+"goo_paytr")
	gooMustContain(t, "paytr page", body, "goo-gw-hero", "goo-gw-logobox-lg", "Beta", "has not yet been verified against a live merchant account",
		"Setup incomplete", `goo-gw-copyinput`, `readonly`, `class="button goo-gw-copy"`, "/wc-api/goo_paytr")
	if strings.Contains(body, `<code>`+base) {
		t.Error("notification URL still shown as plain code")
	}
	if m := regexp.MustCompile(`href="([^"]+)" target="_blank" rel="noopener noreferrer">Documentation`).FindStringSubmatch(body); m == nil || !strings.HasPrefix(m[1], "https://") {
		t.Error("no Documentation link with target and rel")
	}
	// A verified method with no docs shows neither the Beta notice nor a link.
	body = gooStoreGet(t, c, page+"cod")
	if strings.Contains(body, "has not yet been verified") || strings.Contains(body, ">Documentation<") {
		t.Error("cod page shows a Beta notice or docs link")
	}
}
