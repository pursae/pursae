package server

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func resetGontactCaptchaNets() {
	gontactCaptchaNets.Lock()
	gontactCaptchaNets.hits, gontactCaptchaNets.swept = map[string]*gontactCaptchaNetHits{}, time.Time{}
	gontactCaptchaNets.Unlock()
}

func TestT2CaptchaGontactNetLimit(t *testing.T) {
	resetGontactCaptchaNets()
	defer resetGontactCaptchaNets()
	now := time.Now()
	for i := 0; i < gontactCaptchaNetLimit; i++ {
		if !gontactCaptchaNetAllow("203.0.113.9", now) {
			t.Fatalf("challenge %d refused", i)
		}
	}
	if gontactCaptchaNetAllow("203.0.113.9", now) {
		t.Fatal("one network is not limited")
	}
	if !gontactCaptchaNetAllow("198.51.100.7", now) {
		t.Fatal("another network is refused")
	}
	if !gontactCaptchaNetAllow("203.0.113.9", now.Add(gontactCaptchaNetWindow)) {
		t.Fatal("the window does not end")
	}
	if !gontactCaptchaNetAllow("", now) {
		t.Fatal("an unknown client is refused")
	}
	// The map stays bounded however many networks appear.
	old := gontactCaptchaNetMaxKey
	gontactCaptchaNetMaxKey = 10
	defer func() { gontactCaptchaNetMaxKey = old }()
	for i := 0; i < 50; i++ {
		gontactCaptchaNetAllow("10.0.0."+itoa64(int64(i)), now)
	}
	if n := len(gontactCaptchaNets.hits); n > 10 {
		t.Fatalf("counter map grew to %d", n)
	}
}

// A full table (expired rows included) and a flooding network must not keep a visitor from getting a
// working challenge.
func TestT2CaptchaGontactFullTable(t *testing.T) {
	_, _, srv := gontactSetup(t, true)
	ctx := context.Background()
	resetGontactCaptchaNets()
	defer resetGontactCaptchaNets()
	old := gontactCaptchaMaxRows
	gontactCaptchaMaxRows = 5
	defer func() { gontactCaptchaMaxRows = old }()
	if _, err := srv.store.Pool.Exec(ctx, `DELETE FROM gontact_captcha`); err != nil {
		t.Fatal(err)
	}
	// Expired rows fill the table up to the cap.
	for i := 0; i < 5; i++ {
		if _, err := srv.store.Pool.Exec(ctx, `INSERT INTO gontact_captcha (prefix, answer_hash, image, created_at)
			VALUES ($1, 'x', ''::bytea, now() - interval '2 hours')`, "9000"+itoa64(int64(i))); err != nil {
			t.Fatal(err)
		}
	}
	reqFrom := func(ip string) *gin.Context {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest("GET", "/", nil)
		c.Request.RemoteAddr = ip + ":1234"
		return c
	}
	flood := reqFrom("203.0.113.50")
	for i := 0; i < gontactCaptchaNetLimit+10; i++ {
		_, _ = srv.gontactCaptchaNew(flood, gfCaptchaOptions(nil))
	}
	if _, err := srv.gontactCaptchaNew(flood, gfCaptchaOptions(nil)); err == nil {
		t.Fatal("a flooding network is not limited")
	}
	prefix, err := srv.gontactCaptchaNew(reqFrom("198.51.100.20"), gfCaptchaOptions(nil))
	if err != nil {
		t.Fatalf("visitor refused a challenge: %v", err)
	}
	var live bool
	_ = srv.store.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM gontact_captcha WHERE prefix = $1)`, prefix).Scan(&live)
	if !live {
		t.Fatal("visitor challenge was not stored")
	}
	var n int
	_ = srv.store.Pool.QueryRow(ctx, `SELECT count(*) FROM gontact_captcha`).Scan(&n)
	if n > gontactCaptchaMaxRows {
		t.Fatalf("table holds %d rows, cap %d", n, gontactCaptchaMaxRows)
	}
}
