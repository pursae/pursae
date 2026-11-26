package server

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Publishing a reviewed type puts the approved draft live, never a draft saved while the publish
// was checking the approval.
func TestT2NHeadlessPublishBindsApprovedDraft(t *testing.T) {
	base, admin, srv := hgSite(t)
	ctx := context.Background()
	hgMemo(t, srv, true)
	full := hgNewToken(t, srv, &hgToken{Name: "Full", Kind: "full-access"})
	doc := hgDocOf(t, hgCall(t, base, "POST", "/memos", full, map[string]any{"data": map[string]any{"title": "Approved text"}}))
	hgEntryForm(t, admin, base, "memo", doc, "submit_review", nil)
	hgEntryForm(t, admin, base, "memo", doc, "approve_review", nil)
	sc := srv.hgReg(ctx).Get("memo")

	// A concurrent save holds the row while the publish starts.
	tx, err := srv.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `UPDATE hg_entries SET data = '{"title":"Never reviewed"}', updated_at = now() WHERE type_uid = 'memo' AND document_id = $1`, doc); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := srv.hgPublishEntry(ctx, sc, doc, "", 0)
		done <- err
	}()
	waiting := false
	for i := 0; i < 500 && !waiting; i++ {
		var n int
		_ = srv.store.Pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&n)
		waiting = n > 0
		if !waiting {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if !waiting {
		t.Fatal("the publish never waited for the concurrent save")
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	err = <-done
	e, gerr := srv.hgGetEntry(ctx, "memo", doc, "")
	if gerr != nil {
		t.Fatal(gerr)
	}
	if e.Published != nil {
		t.Fatalf("an unreviewed draft went live: %v (publish error %v)", e.Published, err)
	}
	if !errors.Is(err, errHGNotApproved) {
		t.Fatalf("publish error: %v", err)
	}
}
