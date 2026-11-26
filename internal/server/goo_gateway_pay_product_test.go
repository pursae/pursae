package server

import (
	"context"
	"strings"
	"testing"
)

func TestGooPayProductInstallments(t *testing.T) {
	_, _, srv := gooSite(t)
	ctx := context.Background()
	f := gooFrontCatalog(t, srv)
	c := gooTestCtx()

	// No gateway with installments: nothing is printed, in any state of the other settings.
	if got := gooPayProductInstallments(srv, c, f.mug); got != "" {
		t.Fatalf("block without a qualifying gateway: %s", got)
	}

	gooFakeBankRegister(t, srv, "2:0, 3:5", "")

	got := string(gooPayProductInstallments(srv, c, f.mug))
	gooMustContain(t, "simple product", got, `<details class="goo-installments">`, "<summary>Installment options</summary>", "FakeBank",
		`class="goo-gateway-icon"`, "2 installments", "3 installments", "no surcharge", "5.00", "3.50", "10.50", "calculated at checkout")
	if strings.Contains(got, "from ") {
		t.Errorf("a simple product price is not a 'from' price: %s", got)
	}
	if strings.Count(got, "no surcharge") != 1 {
		t.Errorf("only the 0-rate entry is marked: %s", got)
	}
	if strings.Contains(got, " open") {
		t.Errorf("the block must start collapsed: %s", got)
	}

	// A variable product shows its lowest variation (50.00) as "from".
	got = string(gooPayProductInstallments(srv, c, f.hoodie))
	gooMustContain(t, "variable product", got, "from ", "25.00", "52.50", "17.50")

	// Types without one price to show.
	for name, p := range map[string]*gooProduct{"grouped": f.group, "external": f.ext} {
		if got := gooPayProductInstallments(srv, c, p); got != "" {
			t.Errorf("%s product: %s", name, got)
		}
	}

	// The gateway's minimum amount applies to the product price.
	_ = srv.gooSetOpts(ctx, map[string]any{gooFakeBankID + "_settings": map[string]any{"enabled": "yes", "installments": "yes", "installment_plan": "2:0, 3:5", "installment_min": "20"}})
	if got := gooPayProductInstallments(srv, c, f.mug); got != "" {
		t.Errorf("below the minimum: %s", got)
	}
	if got := gooPayProductInstallments(srv, c, f.hoodie); got == "" {
		t.Error("the hoodie is above the minimum")
	}

	// A disabled gateway, a non-TRY store and the store option each switch the block off.
	_ = srv.gooSetOpts(ctx, map[string]any{gooFakeBankID + "_settings": map[string]any{"enabled": "no", "installments": "yes", "installment_plan": "2:0"}})
	if got := gooPayProductInstallments(srv, c, f.mug); got != "" {
		t.Errorf("disabled gateway: %s", got)
	}
	_ = srv.gooSetOpts(ctx, map[string]any{gooFakeBankID + "_settings": map[string]any{"enabled": "yes", "installments": "yes", "installment_plan": "2:0"}})
	if got := gooPayProductInstallments(srv, c, f.mug); got == "" {
		t.Fatal("re-enabled gateway shows nothing")
	}
	_ = srv.gooSetOpts(ctx, map[string]any{gooPayProductOpt: "no"})
	if got := gooPayProductInstallments(srv, c, f.mug); got != "" {
		t.Errorf("store option off: %s", got)
	}
	_ = srv.gooSetOpts(ctx, map[string]any{gooPayProductOpt: "yes", "currency": "USD"})
	if got := gooPayProductInstallments(srv, c, f.mug); got != "" {
		t.Errorf("USD store: %s", got)
	}
}
