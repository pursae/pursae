package server

import (
	"context"
	"testing"
	"time"

	"wpgo/internal/store"
)

// A forum taken offline hides its posts everywhere: they cannot be liked or reported, its tags have
// no tag page, and replies in it mail no follower.
func TestT2NMiscForumUnpublishedForum(t *testing.T) {
	ts, c, srv := testServerBuilderOff(t)
	installAndLogin(t, ts, c)
	forumActivateLink(t, c, ts.URL)
	ctx := context.Background()
	var member int64
	if err := srv.store.Pool.QueryRow(ctx, `INSERT INTO users (login, email, password_hash, nicename, display_name)
		VALUES ('fan', 'fan@example.test', '', 'fan', 'Fan') RETURNING id`).Scan(&member); err != nil {
		t.Fatal(err)
	}
	fid, err := srv.forumSaveForum(ctx, forumForumInput{Title: "Backroom"}, 1)
	if err != nil {
		t.Fatal(err)
	}
	tp, err := srv.forumCreateTopic(ctx, forumNewTopic{ForumID: fid, AuthorID: 1, Title: "Hidden plans", Content: "<p>plans</p>",
		Tags: "secretproject", IP: "192.0.2.1", At: time.Now().Add(-time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.forumTagBySlug(ctx, "secretproject"); err != nil {
		t.Fatalf("tag of a published forum: %v", err)
	}
	st := srv.forumLoad()
	st.Notify = true
	if err := srv.forumSave(ctx, st); err != nil {
		t.Fatal(err)
	}
	if err := srv.forumSubscribe(ctx, member, tp.ID, true); err != nil {
		t.Fatal(err)
	}
	r, err := srv.forumCreateReply(ctx, forumNewReply{TopicID: tp.ID, AuthorID: 1, Content: "<p>more plans</p>", IP: "192.0.2.1", At: time.Now()})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := srv.store.Pool.Exec(ctx, `UPDATE posts SET status = 'draft' WHERE id = $1`, fid); err != nil {
		t.Fatal(err)
	}
	fan := &store.User{ID: member}
	if _, _, err := srv.forumSetLike(ctx, fan, tp.ID, true); err == nil {
		t.Error("a topic of an unpublished forum was liked")
	}
	if _, _, err := srv.forumSetLike(ctx, fan, r.ID, true); err == nil {
		t.Error("a reply of an unpublished forum was liked")
	}
	if err := srv.forumReport(ctx, fan, r.ID, "spam", ""); err == nil {
		t.Error("a reply of an unpublished forum was reported")
	}
	if tag, err := srv.forumTagBySlug(ctx, "secretproject"); err == nil {
		t.Errorf("tag page of a tag used only in an unpublished forum: %+v", tag)
	}
	var before int
	_ = srv.store.Pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE name = 'wp_mail' AND payload->>'To' LIKE '%fan@example.test%'`).Scan(&before)
	srv.forumNotifyReply(ctx, r.ID)
	var after int
	_ = srv.store.Pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE name = 'wp_mail' AND payload->>'To' LIKE '%fan@example.test%'`).Scan(&after)
	if after != before {
		t.Error("a reply in an unpublished forum mailed its followers")
	}
}
