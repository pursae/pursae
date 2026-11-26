package server

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"wpgo/internal/store"
)

// The entry form's save follows the rights of the screen's user: the Publish button on a reviewed
// type needs publish_posts like publish_entry, an inverse relation may only edit the user's own
// owning entries (without edit_others_posts), and media the user may not read cannot be linked.
func TestT2NHeadlessAdminSaveRights(t *testing.T) {
	base, admin, srv := hgSite(t)
	ctx := context.Background()
	full := hgNewToken(t, srv, &hgToken{Name: "Full", Kind: "full-access"})

	// Publish button on a reviewed type, by a contributor, on an approved draft of theirs.
	hgMemo(t, srv, true)
	contrib := hgSecondUser(t, srv, base, "carl", "contributor")
	carl, err := srv.store.UserByLoginOrEmail(ctx, "carl")
	if err != nil {
		t.Fatal(err)
	}
	doc := hgDocOf(t, hgCall(t, base, "POST", "/memos", full, map[string]any{"data": map[string]any{"title": "Approved"}}))
	if _, err := srv.store.Pool.Exec(ctx, `UPDATE hg_entries SET created_by = $1 WHERE document_id = $2`, carl.ID, doc); err != nil {
		t.Fatal(err)
	}
	hgEntryForm(t, admin, base, "memo", doc, "submit_review", nil)
	hgEntryForm(t, admin, base, "memo", doc, "approve_review", nil)
	if e, _ := srv.hgGetEntry(ctx, "memo", doc, ""); e == nil || e.ReviewState() != hgReviewApproved {
		t.Fatalf("setup: the draft is not approved")
	}
	_, _, loc := hgEntryForm(t, contrib, base, "memo", doc, "save_entry", url.Values{"payload": {`{"title":"Approved"}`}, "publish": {"1"}})
	if e, _ := srv.hgGetEntry(ctx, "memo", doc, ""); e == nil || e.Published != nil || !strings.Contains(loc, "hg-error") {
		t.Fatalf("a contributor published through the form: %s", loc)
	}

	// Inverse relation: an author's own writer cannot claim somebody else's book.
	hgMustSchema(t, srv, &hgSchema{Kind: hgKindCollection, DisplayName: "Writer", SingularName: "writer", PluralName: "writers", Fields: []hgField{{Name: "name", Type: "text"}}})
	hgMustSchema(t, srv, &hgSchema{Kind: hgKindCollection, DisplayName: "Book", SingularName: "book", PluralName: "books",
		Fields: []hgField{{Name: "title", Type: "text"}, {Name: "writer", Type: "relation", Relation: "manyToOne", Target: "writer", InversedBy: "books"}}})
	book := hgDocOf(t, hgCall(t, base, "POST", "/books", full, map[string]any{"data": map[string]any{"title": "Not yours"}}))
	author := hgSecondUser(t, srv, base, "alice", "author")
	payload := fmt.Sprintf(`{"name":"Alice","books":[%q]}`, book)
	hgEntryForm(t, author, base, "writer", "", "save_entry", url.Values{"payload": {payload}})
	if e, err := srv.hgGetEntry(ctx, "book", book, ""); err != nil || len(hgList(e.Data["writer"])) != 0 {
		t.Fatalf("the author's writer took over another user's book: %v %+v", err, e)
	}
	// The administrator may.
	hgEntryForm(t, admin, base, "writer", "", "save_entry", url.Values{"payload": {fmt.Sprintf(`{"name":"Admin","books":[%q]}`, book)}})
	if e, err := srv.hgGetEntry(ctx, "book", book, ""); err != nil || len(hgList(e.Data["writer"])) != 1 {
		t.Fatalf("the administrator could not link the book: %v %+v", err, e)
	}

	// Media: a contributor cannot link the file of a draft post.
	hgMustSchema(t, srv, &hgSchema{Kind: hgKindCollection, DisplayName: "Card", SingularName: "card", PluralName: "cards", Fields: []hgField{{Name: "title", Type: "text"}, {Name: "image", Type: "media"}}})
	draft := &store.Post{Type: "post", Title: "Launch", Slug: "launch", Status: "draft", AuthorID: 1, Meta: map[string]any{}}
	if err := srv.store.InsertPost(ctx, draft); err != nil {
		t.Fatal(err)
	}
	att := &store.Attachment{UploaderID: 1, PostID: draft.ID, Slug: "embargoed", FilePath: "2026/11/embargoed.png", Mime: "image/png", FileSize: 10, Title: "Embargoed"}
	if err := srv.store.InsertAttachment(ctx, att); err != nil {
		t.Fatal(err)
	}
	hgEntryForm(t, contrib, base, "card", "", "save_entry", url.Values{"payload": {fmt.Sprintf(`{"title":"Probe","image":%d}`, att.ID)}})
	var linked int
	_ = srv.store.Pool.QueryRow(ctx, `SELECT count(*) FROM hg_entries WHERE type_uid = 'card' AND data->'image' @> to_jsonb($1::bigint)`, att.ID).Scan(&linked)
	var cards int
	_ = srv.store.Pool.QueryRow(ctx, `SELECT count(*) FROM hg_entries WHERE type_uid = 'card'`).Scan(&cards)
	if linked != 0 || cards != 0 {
		t.Fatalf("a contributor linked a draft post's file (linked %d, cards %d)", linked, cards)
	}
}
