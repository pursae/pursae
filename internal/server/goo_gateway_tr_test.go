package server

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"wpgo/internal/pay/all"
)

// gooTRSetup switches the site language to Turkish. The bundled catalog (web/languages/wpgo-tr_TR.json)
// is always part of the language, so an empty admin pack is enough.
func gooTRSetup(t *testing.T, srv *Server) {
	t.Helper()
	if err := os.MkdirAll(srv.lang.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srv.lang.Dir, "admin-tr_TR.mo"), i18nMO(map[string]string{"": "Plural-Forms: nplurals=2; plural=(n > 1);\n"}), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := srv.settings.Set(context.Background(), map[string]any{"WPLANG": "tr_TR"}); err != nil {
		t.Fatal(err)
	}
	srv.lang.Reset()
}

func gooTRWant(t *testing.T, what, body string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(body, w) {
			t.Errorf("%s lacks %q", what, w)
		}
	}
}

// The payment screens of the admin in Turkish: Settings › Payments, a hosted-payment method's settings
// (with the provider's own texts from internal/pay), a bank's installment settings, the Products tab.
func TestGooGatewayAdminTurkish(t *testing.T) {
	base, admin, srv := gooSite(t)
	ctx := context.Background()
	panel := base + "/wp-admin/goocommerce?page=settings&tab=checkout"
	if err := srv.gooSetOpts(ctx, map[string]any{
		"goo_paytr_settings":  map[string]any{"enabled": "yes"}, // enabled, credentials missing: "Setup incomplete"
		"goo_iyzico_settings": map[string]any{"enabled": "yes", "testmode": "yes", "api_key": "fake-key", "secret_key": "fake-secret"},
		"goo_paypal_settings": map[string]any{"enabled": "yes", "testmode": "no", "client_id": "fake-id", "client_secret": "fake-secret"},
	}); err != nil {
		t.Fatal(err)
	}

	_, en, _ := get(t, admin, panel)
	gooTRWant(t, "English panel", en, "Payment providers", "Bank virtual POS", "Checkout order", "Setup incomplete", "Test mode", "Live", ">Set up<", ">Manage<")

	gooTRSetup(t, srv)
	_, page, _ := get(t, admin, panel)
	gooTRWant(t, "Turkish panel", page, ">Ödeme yöntemleri<", ">Dünya geneli<", ">Çevrimdışı<", ">Türkiye<", ">Ödeme kuruluşları<", ">Banka sanal POS<", ">Test<",
		">Ödeme sırası<", `placeholder="Ödeme yöntemlerini filtrele…"`, `aria-label="Ödeme yöntemlerini filtrele"`, `aria-label="Ödeme yöntemi bölgeleri"`,
		"Eşleşen ödeme yöntemi yok", ">Kurulum eksik<", ">Test modu<", ">Canlı<", ">Kur<", ">Yönet<", ">İadeler<", ">Abonelikler<", ">Kayıtlı kartlar<", ">Taksit<",
		`aria-label="Etkinleştir: PayTR"`, `aria-label="PayTR yukarı taşı"`, "ödeme yöntemi etkin", "müşterilerin gördüğü sırayla",
		"Kart ödemeleri PayTR'nin güvenli iframe'inde alınır", "İş Bankası sanal POS (EST / Nestpay)")
	if strings.Contains(page, "Payment providers") || strings.Contains(page, "Setup incomplete") {
		t.Error("the Turkish panel still has English labels")
	}
	// The setup problem is in the title of the status badge, with the field title translated.
	gooTRWant(t, "Turkish panel", page, `title="&#34;Üye iş yeri numarası&#34; ayarı eksik."`)

	// A hosted-payment method's settings.
	_, page, _ = get(t, admin, panel+"&section=goo_paytr")
	gooTRWant(t, "PayTR settings", page, "Bu yöntem henüz gerçek bir üye iş yeri hesabıyla doğrulanmadı", ">Kurulum eksik:<", ">Belgeler<",
		">Test modu<", " Test modunu etkinleştir<", "Test modu, ödeme kuruluşunun test ortamını kullanır", ">Üye iş yeri numarası<", ">Mağaza anahtarı<",
		"Mağaza Parola; aynı sayfada yer alır. Kayıtlı değer bir daha gösterilmez", ">Koyu ödeme formu<", " Etkinleştir: PayTR<", " Ödeme sayfasında taksit sun<",
		">En fazla taksit sayısı<", ">Bildirim URL'si<", ">Kopyala<", `aria-label="Bildirim URL'si"`,
		"PayTR üye iş yeri panelinde bir bildirim (callback) URL'si istiyorsa aşağıdaki adresi girin.", `value="http`)
	if strings.Contains(page, "asks for a notification") || strings.Contains(page, "Merchant key") {
		t.Error("the PayTR settings still have English texts")
	}

	// A bank: the installment settings and the select options.
	_, page, _ = get(t, admin, panel+"&section=goo_est_isbank")
	gooTRWant(t, "bank settings", page, " Taksit sun<", ">Taksit planı<", "<code>count:rate</code> biçiminde yazılır", ">Taksit için asgari tutar<",
		">Mağaza No (Client ID)<", ">3D mağaza anahtarı (Store key)<", ">Mağaza tipi<", "3D Pay Hosting (kartı banka tahsil eder)", "Bankalar taksiti yalnızca Türk lirasıyla sunar")

	// Products › Installment options.
	_, page, _ = get(t, admin, base+"/wp-admin/goocommerce?page=settings&tab=products")
	gooTRWant(t, "Products tab", page, ">Taksit seçenekleri<", "Banka ödeme yöntemlerinin taksit seçeneklerini ürün sayfalarında göster")

	// The Copy button's "Copied" is a script string: it is in the page's script dictionary.
	gooTRWant(t, "script dictionary", srv.adminL10nFor(srv.lang.Catalog("tr_TR")).js, `"Copied":"Kopyalandı"`)

	// Messages built in Go: a refused enable and a bad installment plan are translated as formats.
	g := gooGatewayRegistry["goo_est_isbank"].(*gooPayGateway)
	st := map[string]any{"installment_plan": "13:5", "installment_min": "abc"}
	err := srv.gooPayCheckInstallmentSettings(g, st, map[string]any{})
	if err == nil {
		t.Fatal("a bad plan was accepted")
	}
	gooTRWant(t, "installment errors", err.Error(), `Taksit planı: "13:5" girdisi 2 ile 12 dışında bir taksit sayısı içeriyor.`, `Taksit için asgari tutar: "abc" bir tutar değil.`)
	if got := srv.gooGatewaySetupProblem(gooGatewayRegistry["goo_paytr"]); got != `"Üye iş yeri numarası" ayarı eksik.` {
		t.Errorf("setup problem = %q", got)
	}
}

// The order screen's payment texts in Turkish: the status check action, its outcomes and the attempts box.
func TestGooGatewayOrderTurkish(t *testing.T) {
	base, admin, srv := gooSite(t)
	ctx := context.Background()
	fake, _ := gooFakePayRegister(t, srv)
	o, _ := gooFakePayCheckout(t, srv, "Turkish Lamp", 12.5)
	a := gooFakePayAttempts(t, srv, o.ID)[0]
	edit := base + "/wp-admin/goocommerce?page=orders&action=edit&id=" + itoa64(o.ID)

	_, en, _ := get(t, admin, edit)
	gooTRWant(t, "English order screen", en, "Check payment status with FakePay", "Payment attempts", "reference "+a.Ref, "test mode")

	gooTRSetup(t, srv)
	_, page, _ := get(t, admin, edit)
	gooTRWant(t, "Turkish order screen", page, "FakePay ile ödeme durumunu sorgula", "Ödeme denemeleri", "referans "+a.Ref, "test modu", "başlatıldı")

	msg, err := srv.gooPayCheckStatus(ctx, o, "")
	if err != nil || msg != "Bekleniyor: FakePay ödemeyi henüz onaylamadı (deneme "+a.Ref+")." {
		t.Errorf("pending: %q %v", msg, err)
	}
	if n := gooFakePayNotes(t, srv, o.ID, "FakePay ile ödeme durumu sorgulandı: Bekleniyor"); n != 1 {
		t.Errorf("the order note is not in the site language (%d)", n)
	}
	fake.queryErr = os.ErrDeadlineExceeded
	if _, err := srv.gooPayCheckStatus(ctx, o, ""); err == nil || err.Error() != "FakePay ile bağlantı kurulamadı. Birkaç dakika sonra yeniden deneyin (deneme "+a.Ref+")." {
		t.Errorf("unreachable: %v", err)
	}
}

// Every English text a provider hands to the admin (internal/pay/*: the description, each field's title,
// help text and select labels) has a Turkish entry, so a new provider or a reworded text cannot slip
// through untranslated.
func TestGooPayProviderTextsTurkish(t *testing.T) {
	_, _, srv := gooSite(t)
	gooTRSetup(t, srv)
	cat := srv.lang.Catalog("tr_TR")
	check := func(what, en string) {
		t.Helper()
		if strings.TrimSpace(en) == "" {
			return
		}
		if cat.Text(en) == en {
			t.Errorf("no Turkish for %s: %q", what, en)
		}
	}
	for _, sp := range all.Specs() {
		check(sp.ID+" description", sp.Description)
		check(sp.ID+" enable label", "Enable "+sp.Title)
		for _, f := range sp.Fields {
			check(sp.ID+"."+f.ID+" title", f.Title)
			desc := f.Desc
			if f.Secret {
				if desc != "" {
					desc += " "
				}
				desc += "A saved value is not shown again; leave the field empty to keep it."
			}
			check(sp.ID+"."+f.ID+" help", desc)
			for _, o := range f.Options {
				check(sp.ID+"."+f.ID+" option", o.Label)
			}
		}
	}
}
