package server

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"wpgo/internal/auth"
)

// t2nAccountsLogin signs a customer in through My Account and returns their client.
func t2nAccountsLogin(t *testing.T, base, login, pass string) *http.Client {
	t.Helper()
	v := gooFrontClient()
	_, body, _ := get(t, v, base+"/my-account/")
	if code, _, _ := post(t, v, base+"/my-account/", url.Values{"username": {login}, "password": {pass}, "woocommerce-login-nonce": {gooHidden(t, body, "woocommerce-login-nonce")}, "login": {"Log in"}}); code != http.StatusFound {
		t.Fatalf("login %s: %d", login, code)
	}
	return v
}

// t2nAccountsSave posts My Account › Account details and returns the page shown afterwards.
func t2nAccountsSave(t *testing.T, base string, v *http.Client, email, current string) string {
	t.Helper()
	_, body, _ := get(t, v, base+"/my-account/edit-account/")
	post(t, v, base+"/my-account/edit-account/", url.Values{"account_first_name": {"Ada"}, "account_last_name": {"L"}, "account_display_name": {"Ada"},
		"account_email": {email}, "password_current": {current}, "save-account-details-nonce": {gooHidden(t, body, "save-account-details-nonce")}, "save_account_details": {"1"}})
	_, body, _ = get(t, v, base+"/my-account/edit-account/")
	return body
}

func t2nAccountsEmail(t *testing.T, srv *Server, uid int64) string {
	t.Helper()
	u, err := srv.store.UserByID(context.Background(), uid)
	if err != nil {
		t.Fatal(err)
	}
	return u.Email
}

// My Account › Account details: a new e-mail address needs the current password and applies only once
// the link mailed to the new address is followed (the same confirmation as the wp-admin profile).
func TestT2NAccountsMyAccountEmailChange(t *testing.T) {
	base, _, srv := gooSite(t)
	ctx := context.Background()
	uid, err := srv.gooCreateCustomer(ctx, "ada@example.test", "ada", asStrongPass, nil)
	if err != nil {
		t.Fatal(err)
	}
	v := t2nAccountsLogin(t, base, "ada", asStrongPass)

	if body := t2nAccountsSave(t, base, v, "ada-new@example.test", ""); !strings.Contains(body, "Please enter your current password to change your email address.") {
		t.Error("no error for an e-mail change without the current password")
	}
	if body := t2nAccountsSave(t, base, v, "ada-new@example.test", "wrong-password"); !strings.Contains(body, "Your current password is incorrect.") {
		t.Error("no error for a wrong current password")
	}
	if got := t2nAccountsEmail(t, srv, uid); got != "ada@example.test" {
		t.Fatalf("e-mail changed without the current password: %q", got)
	}

	body := t2nAccountsSave(t, base, v, "ada-new@example.test", asStrongPass)
	if !strings.Contains(body, "Please check your inbox at ada-new@example.test") {
		t.Error("no pending-confirmation notice")
	}
	if got := t2nAccountsEmail(t, srv, uid); got != "ada@example.test" {
		t.Fatalf("e-mail changed before confirmation: %q", got)
	}
	change := lastMail(t, "Email Change Request")
	if len(change.To) != 1 || change.To[0] != "ada-new@example.test" {
		t.Fatalf("confirmation mail to %v", change.To)
	}
	confirm := firstURL(t, change.Body, "newuseremail=")
	u, _ := url.Parse(confirm)
	code, _, loc := get(t, v, base+u.RequestURI())
	if code != http.StatusFound || !strings.Contains(loc, "edit-account") {
		t.Fatalf("confirm: %d %s", code, loc)
	}
	if got := t2nAccountsEmail(t, srv, uid); got != "ada-new@example.test" {
		t.Fatalf("e-mail after confirmation: %q", got)
	}
	// The link opens nothing else of wp-admin for a customer.
	if code, _, loc := get(t, v, base+"/wp-admin/profile"); code != http.StatusFound || strings.Contains(loc, "/wp-admin") {
		t.Errorf("customer reached wp-admin profile: %d %s", code, loc)
	}
}

// The current-password check of Account details shares the login throttle.
func TestT2NAccountsMyAccountPasswordThrottle(t *testing.T) {
	old := loginSlowdown
	loginSlowdown = func(context.Context, time.Duration) {}
	t.Cleanup(func() { loginSlowdown = old })
	base, _, srv := gooSite(t)
	if _, err := srv.gooCreateCustomer(context.Background(), "bea@example.test", "bea", asStrongPass, nil); err != nil {
		t.Fatal(err)
	}
	v := t2nAccountsLogin(t, base, "bea", asStrongPass)
	throttled := false
	for i := 0; i < 30 && !throttled; i++ {
		throttled = strings.Contains(t2nAccountsSave(t, base, v, "bea-new@example.test", "wrong-"+itoa64(int64(i))), "Too many failed attempts.")
	}
	if !throttled {
		t.Fatal("unlimited guesses of the current password")
	}
}

// t2nAccountsAppPassword stores an application password with the given scopes for the user.
func t2nAccountsAppPassword(t *testing.T, srv *Server, uid int64, scopes string) string {
	t.Helper()
	plain := generateAppPassword()
	hash, err := auth.HashPassword(plain)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.store.Pool.Exec(context.Background(), `INSERT INTO app_passwords(user_id, uuid, app_id, name, password_hash, scopes) VALUES ($1, $2::uuid, '', 'test', $3, $4::text[])`,
		uid, newUUID(), hash, scopes); err != nil {
		t.Fatal(err)
	}
	return plain
}

// POST /wp/v2/users/me: a browser session changing its own e-mail gets the confirmation flow; an
// Application Password allowed to set it keeps WordPress' immediate change.
func TestT2NAccountsRESTOwnEmail(t *testing.T) {
	base, c, srv := gooSite(t)
	ctx := context.Background()
	admin, err := srv.store.UserByLoginOrEmail(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	rc := newRESTClient(t, c, base)
	if code, obj, _, _ := rc.do("POST", "/wp/v2/users/me", map[string]any{"email": "session-new@example.test"}); code != http.StatusOK {
		t.Fatalf("session update: %d %v", code, obj)
	}
	if got := t2nAccountsEmail(t, srv, admin.ID); got != "admin@example.test" {
		t.Fatalf("session changed the e-mail without confirmation: %q", got)
	}
	if m := lastMail(t, "Email Change Request"); len(m.To) != 1 || m.To[0] != "session-new@example.test" {
		t.Fatalf("confirmation mail to %v", m.To)
	}

	// The users scope may set the password directly as well (documented as equal to full access).
	scoped := &restClient{t: t, c: http.DefaultClient, base: base, basic: [2]string{"admin", t2nAccountsAppPassword(t, srv, admin.ID, "{users}")}}
	if code, obj, _, _ := scoped.do("POST", "/wp/v2/users/me", map[string]any{"email": "scoped-new@example.test"}); code != http.StatusOK || obj["email"] != "scoped-new@example.test" {
		t.Fatalf("users-scoped update: %d %v", code, obj)
	}
	full := &restClient{t: t, c: http.DefaultClient, base: base, basic: [2]string{"admin", t2nAccountsAppPassword(t, srv, admin.ID, "{full_access}")}}
	if code, obj, _, _ := full.do("POST", "/wp/v2/users/me", map[string]any{"email": "full-new@example.test"}); code != http.StatusOK {
		t.Fatalf("full-access update: %d %v", code, obj)
	}
	if got := t2nAccountsEmail(t, srv, admin.ID); got != "full-new@example.test" {
		t.Fatalf("full-access credential: e-mail %q", got)
	}
}
