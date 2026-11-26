package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// Headless sign-up asks for the account-forms CAPTCHA when Pursae Security has one, and is limited
// per network like the Forum's.
func TestT2NHeadlessRegisterGates(t *testing.T) {
	base, admin, srv := hgSite(t)
	ctx := context.Background()
	stubCaptchaVerifier(t)
	forumLimiterReset()
	t.Cleanup(forumLimiterReset)
	st := srv.hgLoad()
	st.UserAuth, st.UserRegister = true, true
	_ = srv.hgSaveSettings(ctx, st)
	asToggle(t, admin, base, "activate")
	asConfigure(t, srv, func(st *asSettings) { st.CaptchaForms = true })
	if err := srv.settings.Set(ctx, map[string]any{"gontact_turnstile_sitekey": "site123", "gontact_turnstile_secret": "sec"}); err != nil {
		t.Fatal(err)
	}
	reg := func(login string, extra ...string) hgResp {
		body := map[string]any{"username": login, "email": login + "@example.test", "password": "a-long-password"}
		if len(extra) == 1 {
			body["captchaToken"] = extra[0]
		}
		return hgCall(t, base, "POST", "/auth/local/register", "", body)
	}

	if r := reg("bot1"); r.Code != http.StatusForbidden || !strings.Contains(r.Raw, `"captcha_required"`) || !strings.Contains(r.Raw, `"site123"`) {
		t.Fatalf("sign-up without a CAPTCHA token: %d %s", r.Code, r.Raw)
	}
	if r := reg("bot2", "bad"); r.Code != http.StatusForbidden {
		t.Fatalf("sign-up with a wrong CAPTCHA token: %d %s", r.Code, r.Raw)
	}
	for _, l := range []string{"bot1", "bot2"} {
		if _, err := srv.store.UserByLoginOrEmail(ctx, l); err == nil {
			t.Fatalf("%s was created without a solved CAPTCHA", l)
		}
	}
	if r := reg("human", "good"); r.Code != http.StatusCreated {
		t.Fatalf("sign-up with a solved CAPTCHA: %d %s", r.Code, r.Raw)
	}

	// The per-network limit (the three sign-ups above count).
	code := 0
	for i := 0; i < 20 && code != http.StatusTooManyRequests; i++ {
		code = reg(fmt.Sprintf("flood%d", i), "good").Code
	}
	if code != http.StatusTooManyRequests {
		t.Fatalf("no limit on sign-ups from one network: last %d", code)
	}
}
