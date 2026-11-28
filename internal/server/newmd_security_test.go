package server

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"wpgo/internal/store"
	"wpgo/internal/webbuilder"
)

// gooPasswordProduct is a published product behind a post password.
func gooPasswordProduct(t *testing.T, srv *Server) *gooProduct {
	t.Helper()
	return gooMakeSimple(t, srv, "Locked Lamp", 40, func(p *gooProduct) {
		p.Post = &store.Post{Type: "product", Status: "publish", CommentStatus: "open", PingStatus: "closed", Meta: map[string]any{},
			Password: "lamp-pass", Content: "<p>Locked lamp secret body</p>", Excerpt: "Locked lamp secret short"}
	})
}

// Web Builder product.* slots bound to a named product keep a password-protected product's
// description, short description and tabs behind the password, as the product page does.
func TestNewmdWebBuilderProductSlotsPassword(t *testing.T) {
	_, _, srv := gooSite(t)
	p := gooPasswordProduct(t, srv)
	if p.Post.Password == "" {
		t.Fatal("setup: product has no password")
	}
	pid := float64(p.ID)
	env := func(c *gin.Context) *wbSlotEnv {
		return &wbSlotEnv{s: srv, c: c, ctx: c.Request.Context(),
			shop: &wbShopEnv{cells: map[string]wbCell{"cell1": {widget: "product.description", dyn: webbuilder.DynSettings{Product: &pid}}}, products: map[int64]*gooProduct{}}}
	}
	slots := []webbuilder.SlotName{webbuilder.SlotProductDescription, webbuilder.SlotProductShortDesc, webbuilder.SlotProductTabs}
	for _, slot := range slots {
		if out, _ := env(gooTestCtx()).shopHTML(slot, webbuilder.SlotArgs{Item: "cell1"}); strings.Contains(out, "secret") {
			t.Errorf("%s prints a password-protected product's text to a visitor without the password: %q", slot, out)
		}
	}
	// With the wp-postpass cookie the text shows.
	c := gooTestCtx()
	c.Request.AddCookie(&http.Cookie{Name: postPassCookie, Value: srv.postPassValue("lamp-pass")})
	if out, _ := env(c).shopHTML(webbuilder.SlotProductDescription, webbuilder.SlotArgs{Item: "cell1"}); !strings.Contains(out, "Locked lamp secret body") {
		t.Errorf("description hidden after the password was entered: %q", out)
	}
}

// [product_page] prints the password form, not the body, of a password-protected product.
func TestNewmdProductPageShortcodePassword(t *testing.T) {
	_, _, srv := gooSite(t)
	p := gooPasswordProduct(t, srv)
	out := srv.gooShortcodes(gooTestCtx(), `[product_page id="`+itoa64(p.ID)+`"]`)
	if strings.Contains(out, "secret") {
		t.Errorf("[product_page] prints a password-protected product's text: %s", out)
	}
	if !strings.Contains(out, `name="post_password"`) {
		t.Errorf("[product_page] of a protected product has no password form: %s", out)
	}
	c := gooTestCtx()
	c.Request.AddCookie(&http.Cookie{Name: postPassCookie, Value: srv.postPassValue("lamp-pass")})
	if out := srv.gooShortcodes(c, `[product_page id="`+itoa64(p.ID)+`"]`); !strings.Contains(out, "Locked lamp secret") {
		t.Errorf("[product_page] still locked after the password was entered")
	}
}

// A customer who cancels an unpaid renewal order cannot reactivate the subscription for free: it
// stays on hold until a renewal is paid (WooCommerce Subscriptions: no Reactivate while the
// subscription needs payment).
func TestNewmdCancelledRenewalNoFreeReactivate(t *testing.T) {
	_, _, srv := gooSite(t)
	ctx := context.Background()
	gxaOn(t, srv, gsubKey, true)
	u := gxaCustomer(t, srv, "renewal-skip@example.test")
	next := time.Now().AddDate(0, 1, 0)
	sub := &gooSubscription{CustomerID: u.ID, Status: "active", Period: "month", Interval: 1, Currency: "USD", Start: time.Now().AddDate(0, -1, 0),
		NextPayment: &next, Billing: map[string]string{"email": u.Email}, Shipping: map[string]string{},
		Items: []gsubItem{{Name: "Box", Qty: 1, Total: 10}}}
	if err := srv.gsubSave(ctx, sub); err != nil {
		t.Fatal(err)
	}
	o, err := srv.gsubCreateRenewal(ctx, sub, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.gooSetStatus(ctx, o, "cancelled", "", "customer"); err != nil {
		t.Fatal(err)
	}
	sub, _ = srv.gsubGet(ctx, sub.ID)
	if sub.Status != "on-hold" {
		t.Fatalf("after the unpaid renewal was cancelled: %s, want on-hold", sub.Status)
	}
	var renew bool
	for _, a := range srv.gsubCustomerActions(ctx, sub) {
		if a.Value == "active" {
			t.Error("customer may reactivate a subscription whose renewal was never paid")
		}
		renew = renew || a.Value == "renew"
	}
	if !renew {
		t.Error("customer has no way to pay for the unpaid period")
	}
	// Paying a new renewal reactivates it.
	o2, err := srv.gsubCreateRenewal(ctx, sub, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.gooSetStatus(ctx, o2, "processing", "", ""); err != nil {
		t.Fatal(err)
	}
	if sub, _ = srv.gsubGet(ctx, sub.ID); sub.Status != "active" {
		t.Errorf("paid renewal did not reactivate: %s", sub.Status)
	}
	if sub.Meta["_unpaid_renewal"] != nil {
		t.Error("paid renewal left the unpaid flag")
	}
	// A customer's own suspension can still be undone by the customer.
	gxaSetExt(t, srv, gsubKey, map[string]any{"max_customer_suspensions": 1})
	_ = srv.gsubUpdateStatus(ctx, sub, "on-hold", "", "customer")
	var react bool
	for _, a := range srv.gsubCustomerActions(ctx, sub) {
		react = react || a.Value == "active"
	}
	if !react {
		t.Error("customer-suspended subscription lost its Reactivate action")
	}
}

// POST /wp/v2/users without roles (or with an empty list) gives default_role only when the actor
// may give it (actorMayGiveRole), like an explicit roles list and Add User.
func TestNewmdRESTCreateUserDefaultRole(t *testing.T) {
	s := secStart(t)
	ctx := context.Background()
	_ = s.srv.settings.Set(ctx, map[string]any{"default_role": "editor"})
	hrID := asCreateUser(t, s.srv, "hrrest", "subscriber", asStrongPass)
	if err := s.srv.store.SetUserCapExceptions(ctx, hrID, map[string]string{"create_users": "allow", "promote_users": "allow", "list_users": "allow"}); err != nil {
		t.Fatal(err)
	}
	hr := asNewClient()
	if code, _, _ := asLogin(t, hr, s.base, "/wp-login", "hrrest", asStrongPass); code != http.StatusFound {
		t.Fatalf("hr login: %d", code)
	}
	rc := newRESTClient(t, hr, s.base)
	for i, body := range []map[string]any{
		{"username": "rolless1", "email": "rolless1@example.test", "password": asStrongPass},
		{"username": "rolless2", "email": "rolless2@example.test", "password": asStrongPass, "roles": []string{}},
	} {
		code, out, _, _ := rc.do("POST", "/wp/v2/users", body)
		if code == http.StatusCreated {
			t.Errorf("case %d: a non-administrator promoter created a user with default_role editor: %v", i, out["roles"])
		}
	}
	// A default role the actor may give still works.
	_ = s.srv.settings.Set(ctx, map[string]any{"default_role": "subscriber"})
	if code, out, _, _ := rc.do("POST", "/wp/v2/users", map[string]any{"username": "rolless3", "email": "rolless3@example.test", "password": asStrongPass}); code != http.StatusCreated {
		t.Errorf("default subscriber: %d %v", code, out)
	}
	// The administrator is not affected.
	_ = s.srv.settings.Set(ctx, map[string]any{"default_role": "editor"})
	if code, out, _, _ := s.admin.do("POST", "/wp/v2/users", map[string]any{"username": "rolless4", "email": "rolless4@example.test", "password": asStrongPass}); code != http.StatusCreated {
		t.Errorf("admin default editor: %d %v", code, out)
	}
}
