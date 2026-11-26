package server

import (
	"context"
	"strings"

	"github.com/gin-gonic/gin"
)

// More option groups of /pursae/v1/options/{group}: Pursae Multi Language, Pursae Pixel, Pursae Cookie
// and the Pursae Contact Form integration keys (reCAPTCHA, Turnstile).

// gontactOptions are the Contact Form integration settings (Contact › Integration).
type gontactOptions struct {
	RecaptchaSitekey string `json:"recaptcha_sitekey"`
	RecaptchaSecret  string `json:"recaptcha_secret"`
	TurnstileSitekey string `json:"turnstile_sitekey"`
	TurnstileSecret  string `json:"turnstile_secret"`
}

func (s *Server) gontactLoadOptions() gontactOptions {
	return gontactOptions{RecaptchaSitekey: s.settings.String("gontact_recaptcha_sitekey"), RecaptchaSecret: s.settings.String("gontact_recaptcha_secret"),
		TurnstileSitekey: s.settings.String("gontact_turnstile_sitekey"), TurnstileSecret: s.settings.String("gontact_turnstile_secret")}
}

func (s *Server) gontactSaveOptions(ctx context.Context, o gontactOptions) error {
	t := strings.TrimSpace
	err := s.settings.Set(ctx, map[string]any{"gontact_recaptcha_sitekey": t(o.RecaptchaSitekey), "gontact_recaptcha_secret": t(o.RecaptchaSecret),
		"gontact_turnstile_sitekey": t(o.TurnstileSitekey), "gontact_turnstile_secret": t(o.TurnstileSecret)})
	s.purgeAll(ctx)
	return err
}

// pllCheckMTKeyTarget refuses an update that sends the saved machine translation key to another
// provider or endpoint host without a new key (or an empty one to remove it), as the settings form does.
func pllCheckMTKeyTarget(_ *Server, _ *gin.Context, old, v pllOptions) []string {
	if old.MTKey != "" && v.MTKey == old.MTKey && pllMTKeyTarget(v.MTProvider, v.MTEndpoint) != pllMTKeyTarget(old.MTProvider, old.MTEndpoint) {
		return []string{"Send mt_key again (or an empty mt_key to remove it): the saved key is not sent to another provider or endpoint."}
	}
	return nil
}

func init() {
	optionGroups["multilang"] = groupOf(golylangSlug, "Pursae Multi Language", (*Server).pllOpts,
		func(s *Server, ctx context.Context, v pllOptions) error {
			v.MTKey = s.pllSealMTKey(v.MTKey) // a plain key sent in the patch is stored encrypted, a sealed one is kept
			return s.pllSaveOpts(ctx, v)
		}, pllCheckMTKeyTarget)
	optionGroups["pixel"] = groupOf(pixelSlug, "Pursae Pixel", (*Server).pixelLoad,
		func(s *Server, ctx context.Context, v pixelSettings) error { return s.pixelSave(ctx, v) })
	optionGroups["analytics"] = groupOf(analyticsSlug, "Pursae Analytics", (*Server).analyticsLoad,
		func(s *Server, ctx context.Context, v analyticsSettings) error { return s.analyticsSave(ctx, v) })
	// The Google client secret is masked on output (maskSecrets); an empty one on save keeps the stored secret.
	optionGroups["forum"] = groupOf(forumSlug, "Pursae Forum", (*Server).forumLoad,
		func(s *Server, ctx context.Context, v forumSettings) error { return s.forumSave(ctx, v) })
	optionGroups["cookie"] = groupOf(gooikeSlug, "Pursae Cookie", (*Server).gooikeLoad,
		func(s *Server, ctx context.Context, v gooikeSettings) error { return s.gooikeSave(ctx, v) }, (*Server).gooikeCheckRaw)
	optionGroups["contact"] = groupOf(gontactSlug, "Pursae Contact Form", (*Server).gontactLoadOptions,
		func(s *Server, ctx context.Context, v gontactOptions) error { return s.gontactSaveOptions(ctx, v) })
}
