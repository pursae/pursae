package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestHGOwnerAllowsFailsClosed: a lookup that fails (here a cancelled request) denies, while a
// document that does not exist is still let through for the caller to answer 404 or create it.
func TestHGOwnerAllowsFailsClosed(t *testing.T) {
	_, _, srv := hgSite(t)
	hgNote(t, srv)
	sc := &hgSchema{UID: "api::note.note", Kind: hgKindCollection}
	a := &hgAuth{Kind: hgUserKind, UserID: 1, OwnerOnly: map[string][]string{sc.UID: {"update", "delete"}}}

	ginCtx := func(ctx context.Context) *gin.Context {
		c, eng := gin.CreateTestContext(httptest.NewRecorder())
		eng.ContextWithFallback = true
		c.Request = httptest.NewRequest(http.MethodPut, "/", nil).WithContext(ctx)
		return c
	}
	if !srv.hgOwnerAllows(ginCtx(context.Background()), a, sc, "no-such-document", "update") {
		t.Error("a missing document must be let through (404 / create)")
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if srv.hgOwnerAllows(ginCtx(cancelled), a, sc, "no-such-document", "update") {
		t.Error("a failed owner lookup must deny (fail closed)")
	}
}
