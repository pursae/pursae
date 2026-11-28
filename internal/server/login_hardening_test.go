package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"wpgo/internal/auth"
)

func TestLockoutAlertSlotCapsMailsPerHour(t *testing.T) {
	asAlertMu.Lock()
	asAlertTimes = nil
	asAlertMu.Unlock()
	now := time.Now()
	for i := 0; i < asAlertsPerHour; i++ {
		if !asLockoutAlertSlot(now) {
			t.Fatalf("slot %d refused", i)
		}
	}
	if asLockoutAlertSlot(now.Add(time.Minute)) {
		t.Fatal("alert beyond the hourly cap was allowed")
	}
	if !asLockoutAlertSlot(now.Add(61 * time.Minute)) {
		t.Fatal("slots did not free up after an hour")
	}
}

// A CAPTCHA on the login/register/lost-password forms, and a throttle on reset requests (integration).
func TestLoginCaptchaAndResetThrottle(t *testing.T) {
	base, _, srv := asStart(t)
	ctx := context.Background()
	asConfigure(t, srv, func(st *asSettings) { st.CaptchaForms = true })
	// No keys saved: nothing is asked.
	if w := srv.loginCaptchaWidget(nil); w != "" {
		t.Fatalf("widget without keys: %q", w)
	}
	verify := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		_, _ = w.Write([]byte(`{"success":` + strconv.FormatBool(r.PostForm.Get("response") == "good") + `}`))
	}))
	defer verify.Close()
	_ = srv.settings.Set(ctx, map[string]any{"gontact_turnstile_sitekey": "site123", "gontact_turnstile_secret": "sec"})
	old := captchaClient
	captchaClient = &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
		r.URL.Scheme, r.URL.Host = "http", strings.TrimPrefix(verify.URL, "http://")
		return http.DefaultTransport.RoundTrip(r)
	})}
	defer func() { captchaClient = old }()

	if code, body, _ := get(t, asNewClient(), base+"/wp-login"); code != 200 || !strings.Contains(body, `data-sitekey="site123"`) {
		t.Fatalf("login form has no CAPTCHA widget: %d", code)
	}
	if _, body, _ := post(t, asNewClient(), base+"/wp-login", url.Values{"log": {"admin"}, "pwd": {"x"}}); !strings.Contains(body, "complete the CAPTCHA") {
		t.Error("login without a CAPTCHA token was processed")
	}
	if _, body, _ := post(t, asNewClient(), base+"/wp-login", url.Values{"log": {"admin"}, "pwd": {"x"}, "cf-turnstile-response": {"bad"}}); !strings.Contains(body, "could not be verified") {
		t.Error("login with a bad CAPTCHA token was processed")
	}
	if _, body, _ := post(t, asNewClient(), base+"/wp-login", url.Values{"log": {"admin"}, "pwd": {"x"}, "cf-turnstile-response": {"good"}}); strings.Contains(body, "CAPTCHA") {
		t.Error("a valid CAPTCHA token was refused")
	}
	asConfigure(t, srv, func(st *asSettings) { st.CaptchaForms = false })

	// Reset requests: three per account per window, then silently dropped (same redirect either way).
	u, err := srv.store.UserByLoginOrEmail(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	allowed := 0
	for i := 0; i < 6; i++ {
		_, _, loc := post(t, asNewClient(), base+"/wp-login?action=lostpassword", url.Values{"user_login": {u.Login}})
		if !strings.Contains(loc, "checkemail=confirm") {
			t.Fatalf("request %d: redirect %q", i, loc)
		}
	}
	// Stored in the database (auth.AllowResetRequest): the limit survives a restart.
	_ = srv.store.Pool.QueryRow(ctx, `SELECT count(*) FROM login_attempts WHERE login = $1`, auth.ResetBucket+strings.ToLower(u.Login)).Scan(&allowed)
	if allowed != resetRequestMax {
		t.Errorf("reset requests recorded for the account: %d, want %d", allowed, resetRequestMax)
	}
}

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
