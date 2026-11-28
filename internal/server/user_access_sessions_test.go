package server

import (
	"context"
	"fmt"
	"testing"
)

// Changing another user's roles ends that user's sessions; saving without a change keeps them.
func TestRoleChangeEndsSessions(t *testing.T) {
	base, c, srv := gooSite(t)
	ctx := context.Background()
	id := asCreateUser(t, srv, "promoted", "subscriber", asStrongPass)
	count := func() (n int) {
		_ = srv.store.Pool.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE user_id = $1`, id).Scan(&n)
		return
	}
	if _, err := srv.auth.Create(ctx, id, false, "203.0.113.9", "test"); err != nil {
		t.Fatal(err)
	}
	rc := newRESTClient(t, c, base)
	if code, obj, _, _ := rc.do("POST", fmt.Sprintf("/wp/v2/users/%d", id), map[string]any{"roles": []string{"subscriber"}}); code != 200 {
		t.Fatalf("same role: %d %v", code, obj)
	}
	if n := count(); n != 1 {
		t.Fatalf("unchanged role ended sessions: %d", n)
	}
	if code, obj, _, _ := rc.do("POST", fmt.Sprintf("/wp/v2/users/%d", id), map[string]any{"roles": []string{"editor"}}); code != 200 {
		t.Fatalf("promote: %d %v", code, obj)
	}
	if n := count(); n != 0 {
		t.Errorf("sessions after a role change: %d", n)
	}
}

// Forum moderation that changes a member's access (block, forum role) ends the member's sessions.
func TestForumAccessChangeEndsSessions(t *testing.T) {
	fx := forumFrontStart(t, false)
	srv, ctx := fx.srv, fx.ctx
	admin, err := srv.store.UserByID(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	_, id := fx.member(t, "forummember", forumRoleParticipant)
	count := func() (n int) {
		_ = srv.store.Pool.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE user_id = $1`, id).Scan(&n)
		return
	}
	login := func() {
		if _, err := srv.auth.Create(ctx, id, false, "203.0.113.9", "test"); err != nil {
			t.Fatal(err)
		}
	}
	if n := count(); n != 1 {
		t.Fatalf("member sessions before: %d", n)
	}
	if err := srv.forumSetBlocked(ctx, admin, id, true); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 0 {
		t.Errorf("sessions after a block: %d", n)
	}
	login()
	if err := srv.forumSetBlocked(ctx, admin, id, true); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 1 {
		t.Errorf("a repeated block ended sessions: %d", n)
	}
	if err := srv.forumSetForumRole(ctx, admin, id, forumRoleModerator); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 0 {
		t.Errorf("sessions after a forum role change: %d", n)
	}
}
