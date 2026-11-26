package server

import (
	"context"
	"encoding/json"
	"html"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// The shared CAPTCHA helper: one verifier and one widget for every form that asks for a CAPTCHA
// (Pursae Contact Form, Web Builder forms, the login / register / lost-password forms and the
// Pursae Commerce My Account forms). The keys are the ones saved in Pursae Contact Form, so there
// is a single place to configure them: Turnstile first, then reCAPTCHA v3.

// captchaClient posts tokens to the siteverify endpoints; tests swap it.
var captchaClient = &http.Client{Timeout: 8 * time.Second}

const (
	captchaTurnstileVerify = "https://challenges.cloudflare.com/turnstile/v0/siteverify"
	captchaRecaptchaVerify = "https://www.google.com/recaptcha/api/siteverify"
)

// captchaProvider is one configured CAPTCHA service.
type captchaProvider struct{ kind, site, secret, endpoint, field string }

// captchaProvider returns the configured service, Turnstile before reCAPTCHA (ok=false: no keys).
func (s *Server) captchaProvider() (captchaProvider, bool) {
	if site, secret := s.settings.String("gontact_turnstile_sitekey"), s.revealSecret(s.settings.String("gontact_turnstile_secret")); site != "" && secret != "" {
		return captchaProvider{"turnstile", site, secret, captchaTurnstileVerify, "cf-turnstile-response"}, true
	}
	if site, secret := s.settings.String("gontact_recaptcha_sitekey"), s.revealSecret(s.settings.String("gontact_recaptcha_secret")); site != "" && secret != "" {
		return captchaProvider{"recaptcha", site, secret, captchaRecaptchaVerify, "g-recaptcha-response"}, true
	}
	return captchaProvider{}, false
}

// verifyCaptcha posts a token to a siteverify endpoint. Turnstile has no score: a valid token
// scores 1; reCAPTCHA v3 returns its own (callers ask for 0.5 or more).
func (s *Server) verifyCaptcha(ctx context.Context, endpoint, secret, token, ip string) (float64, bool) {
	form := url.Values{"secret": {secret}, "response": {token}, "remoteip": {ip}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, false
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := captchaClient.Do(req)
	if err != nil {
		slog.Warn("captcha verification", "err", err)
		return 0, false
	}
	defer resp.Body.Close()
	var out struct {
		Success bool    `json:"success"`
		Score   float64 `json:"score"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&out) != nil {
		return 0, false
	}
	if !strings.Contains(endpoint, "recaptcha") {
		out.Score = 1
	}
	return out.Score, out.Success
}

const captchaScriptKey = "wpgo.captcha.script"

// captchaWidget is the markup a form places before its submit button. The provider's script is
// printed once per page, so a page with two forms (My Account: login and register) loads it once.
func captchaWidget(c *gin.Context, p captchaProvider, action string) template.HTML {
	site := html.EscapeString(p.site)
	script := ""
	first := true
	if c != nil {
		_, seen := c.Get(captchaScriptKey)
		first = !seen
		c.Set(captchaScriptKey, true)
	}
	if p.kind == "turnstile" {
		if first {
			script = `<script` + cspNonceAttr() + ` src="https://challenges.cloudflare.com/turnstile/v0/api.js" async defer></script>`
		}
		return template.HTML(`<div class="cf-turnstile" data-sitekey="` + site + `"></div>` + script)
	}
	if first {
		script = `<script` + cspNonceAttr() + ` src="https://www.google.com/recaptcha/api.js?render=` + html.EscapeString(url.QueryEscape(p.site)) + `"></script>`
	}
	return template.HTML(`<input type="hidden" name="g-recaptcha-response" value="" />` + script +
		`<script` + cspNonceAttr() + `>(function(){var f=document.currentScript.closest('form');if(!f)return;var done=false,btn=null;f.addEventListener('click',function(e){var b=e.target.closest&&e.target.closest('button[type=submit],input[type=submit]');if(b)btn=b;});f.addEventListener('submit',function(e){if(done||!window.grecaptcha)return;e.preventDefault();grecaptcha.ready(function(){grecaptcha.execute('` + template.JSEscapeString(p.site) + `',{action:'` + template.JSEscapeString(action) + `'}).then(function(t){f.elements['g-recaptcha-response'].value=t;done=true;if(btn&&btn.name){var h=document.createElement('input');h.type='hidden';h.name=btn.name;h.value=btn.value;f.appendChild(h);}f.submit();});});});})();</script>`)
}

// captchaCheck verifies the token a form posted for provider p: "" when valid, otherwise the
// message to show.
func (s *Server) captchaCheck(c *gin.Context, p captchaProvider) string {
	token := strings.TrimSpace(c.PostForm(p.field))
	if token == "" {
		return "Please complete the CAPTCHA."
	}
	score, good := s.verifyCaptcha(c.Request.Context(), p.endpoint, p.secret, token, clientIP(c))
	if !good || score < 0.5 {
		return "The CAPTCHA could not be verified. Please try again."
	}
	return ""
}
