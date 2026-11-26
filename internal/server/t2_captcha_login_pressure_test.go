package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"wpgo/internal/auth"
)

// stubCaptchaVerifier answers siteverify calls: the token "good" passes, anything else fails.
func stubCaptchaVerifier(t *testing.T) {
	t.Helper()
	verify := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		_, _ = w.Write([]byte(`{"success":` + strconv.FormatBool(r.PostForm.Get("response") == "good") + `}`))
	}))
	t.Cleanup(verify.Close)
	old := captchaClient
	captchaClient = &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
		r.URL.Scheme, r.URL.Host = "http", strings.TrimPrefix(verify.URL, "http://")
		return http.DefaultTransport.RoundTrip(r)
	})}
	t.Cleanup(func() { captchaClient = old })
}

// The Headless API sign-in asks for the CAPTCHA of an account under attack like /wp-login does.
func TestT2CaptchaHeadlessLoginPressure(t *testing.T) {
	base, _, srv := hgSite(t)
	ctx := context.Background()
	stubLoginSlowdown(t)
	stubCaptchaVerifier(t)
	asCreateUser(t, srv, "alice", "subscriber", "alice-secret-pw")
	st := srv.hgLoad()
	st.UserAuth = true
	_ = srv.hgSaveSettings(ctx, st)
	login := func(addr string, body map[string]any) hgResp {
		return hgCall(t, base, "POST", "/auth/local", "", body, "X-Forwarded-For", addr)
	}
	creds := func(extra ...string) map[string]any {
		m := map[string]any{"identifier": "alice", "password": "alice-secret-pw"}
		if len(extra) == 1 {
			m["captchaToken"] = extra[0]
		}
		return m
	}
	attack := func() {
		if _, err := getPool(t).Exec(ctx, `DELETE FROM login_attempts`); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < auth.MaxFailedLoginsPerLogin; i++ {
			if _, err := getPool(t).Exec(ctx, `INSERT INTO login_attempts(ip, login, success, pending) VALUES ($1, 'alice', false, false)`, fmt.Sprintf("203.0.113.%d", i+1)); err != nil {
				t.Fatal(err)
			}
		}
	}

	// No CAPTCHA keys: nothing changes, even under attack.
	attack()
	if r := login("198.51.100.30", creds()); r.Code != http.StatusOK {
		t.Fatalf("sign-in without CAPTCHA keys: %d %s", r.Code, r.Raw)
	}

	if err := srv.settings.Set(ctx, map[string]any{"gontact_turnstile_sitekey": "site123", "gontact_turnstile_secret": "sec"}); err != nil {
		t.Fatal(err)
	}
	attack()
	r := login("198.51.100.31", creds())
	if r.Code != http.StatusForbidden || !strings.Contains(r.Raw, `"captcha_required"`) || !strings.Contains(r.Raw, `"site123"`) {
		t.Fatalf("no CAPTCHA asked at an account under attack: %d %s", r.Code, r.Raw)
	}
	if n := asCount(t, `SELECT count(*) FROM login_attempts WHERE ip = '198.51.100.31'`); n != 0 {
		t.Errorf("a missing CAPTCHA was counted as a guess (%d rows)", n)
	}
	if r := login("198.51.100.31", creds("bad")); r.Code != http.StatusForbidden {
		t.Fatalf("wrong CAPTCHA token accepted: %d", r.Code)
	}
	if r := login("198.51.100.31", creds("good")); r.Code != http.StatusOK {
		t.Fatalf("owner with a solved CAPTCHA: %d %s", r.Code, r.Raw)
	}
}

// The Forum login form asks for the CAPTCHA of an account under attack like /wp-login does.
func TestT2CaptchaForumLoginPressure(t *testing.T) {
	ts, _, srv := forumAuthSetup(t)
	base := ts.URL
	ctx := context.Background()
	stubLoginSlowdown(t)
	stubCaptchaVerifier(t)
	asCreateUser(t, srv, "fran", "subscriber", "fran-secret-pw")
	if err := srv.settings.Set(ctx, map[string]any{"gontact_turnstile_sitekey": "site123", "gontact_turnstile_secret": "sec"}); err != nil {
		t.Fatal(err)
	}
	if _, err := getPool(t).Exec(ctx, `DELETE FROM login_attempts`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < auth.MaxFailedLoginsPerLogin; i++ {
		if _, err := getPool(t).Exec(ctx, `INSERT INTO login_attempts(ip, login, success, pending) VALUES ($1, 'fran', false, false)`, fmt.Sprintf("203.0.113.%d", i+1)); err != nil {
			t.Fatal(err)
		}
	}
	code, body, _ := forumLogin(t, base, forumJarClient(), "fran", "fran-secret-pw", "")
	if code == http.StatusFound || !strings.Contains(body, "complete the CAPTCHA") || !strings.Contains(body, `data-sitekey="site123"`) {
		t.Fatalf("no CAPTCHA asked at an account under attack: %d", code)
	}
}
