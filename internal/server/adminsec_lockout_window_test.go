package server

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// A lockout that a parallel request has just created moves the failure window past the failures that
// caused it. A request that passed the lockout check before the lockout existed must still be refused,
// not count an empty window.
func TestAdminSecAttemptsOverSeesFreshLockout(t *testing.T) {
	_, _, srv := asStart(t)
	asConfigure(t, srv, func(st *asSettings) { st.IPMaxFailures, st.UserMaxFailures, st.LockoutMinutes = 3, 0, 20 })
	ctx := context.Background()
	gc, _ := gin.CreateTestContext(httptest.NewRecorder())
	gc.Request = httptest.NewRequest("POST", "/wp-login", nil)
	gc.Request.RemoteAddr = "203.0.113.7:5555"
	st := srv.asLoad()
	b := asNetBucket("203.0.113.7")

	for i := 0; i < 3; i++ {
		if _, err := srv.store.Pool.Exec(ctx, `INSERT INTO login_attempts(ip, login, success, pending, attempted_at) VALUES ($1, 'admin', false, false, now() - interval '1 second')`, b.net); err != nil {
			t.Fatal(err)
		}
	}
	if !srv.asAttemptsOver(gc, st, b, 2) {
		t.Fatal("three failures over a limit of two were not counted")
	}

	// The lockout those failures caused: the window now starts after them.
	if _, err := srv.store.Pool.Exec(ctx, `INSERT INTO adminsec_lockouts (kind, value, reason, failures, long_lock, until) VALUES ('ip', $1, 'test', 3, false, now() + interval '20 minutes')`, b.value); err != nil {
		t.Fatal(err)
	}
	if !srv.asAttemptsOver(gc, st, b, 3) {
		t.Error("a request counted an empty window while its network was locked out")
	}

	// A released (or expired) lockout no longer refuses: the window after it is empty.
	if _, err := srv.store.Pool.Exec(ctx, `UPDATE adminsec_lockouts SET released = true`); err != nil {
		t.Fatal(err)
	}
	if srv.asAttemptsOver(gc, st, b, 3) {
		t.Error("a released lockout still refused the network")
	}
}
