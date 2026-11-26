package server

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"wpgo/internal/auth"
)

// The new CAPTCHA a failed submission returns counts toward its network's challenge limit like the
// one on the page.
func TestT2NMiscGontactRefillPerNetwork(t *testing.T) {
	_, _, srv := gontactSetup(t, true)
	resetGontactCaptchaNets()
	defer resetGontactCaptchaNets()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/", nil)
	c.Request.RemoteAddr = "203.0.113.77:1234"
	now := time.Now()
	for i := 0; i < gontactCaptchaNetLimit; i++ {
		gontactCaptchaNetAllow(auth.ClientNet("203.0.113.77"), now)
	}
	_, tags := gfScan(`[captchac your-captcha] [captchar your-captcha]`, nil)
	sub := &gfSubmission{s: srv, c: c, ctx: context.Background(), form: &gfForm{ID: 1}, tags: tags, unitTag: "wpcf7-f1-o1", status: "validation_failed"}
	if cp, ok := sub.result()["captcha"]; ok {
		t.Errorf("a network over its challenge limit got a new CAPTCHA: %v", cp)
	}
}
