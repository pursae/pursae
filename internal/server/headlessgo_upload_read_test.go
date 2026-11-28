package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"wpgo/internal/store"
)

// An upload token reads public files and its own uploads, not a draft's attachment.
func TestHeadlessGoUploadReadNeedsAccess(t *testing.T) {
	base, _, srv := hgSite(t)
	ctx := context.Background()
	full := hgNewToken(t, srv, &hgToken{Name: "Full", Kind: "full-access"})
	up := hgNewToken(t, srv, &hgToken{Name: "Uploader", Kind: "custom", Upload: true})
	draft := &store.Post{Type: "post", Title: "Draft", Slug: "draft", Status: "draft", AuthorID: 1}
	if err := srv.store.InsertPost(ctx, draft); err != nil {
		t.Fatal(err)
	}
	att, err := srv.insertAttachmentFile(ctx, "secret.png", bytes.NewReader(hgPNG(10, 10)), 1, draft.ID)
	if err != nil {
		t.Fatal(err)
	}
	if r := hgCall(t, base, "GET", fmt.Sprintf("/upload/files/%d", att.ID), up, nil); r.Code != 404 {
		t.Errorf("upload token read a draft's file: %d %s", r.Code, r.Raw)
	}
	if r := hgCall(t, base, "GET", fmt.Sprintf("/upload/files/%d", att.ID), full, nil); r.Code != 200 {
		t.Errorf("full-access token: %d %s", r.Code, r.Raw)
	}
	// Its own upload stays readable, even once attached to the draft.
	r := hgUpload(t, base, up, map[string][]byte{"mine.png": hgPNG(10, 10)}, nil)
	if r.Code != 201 {
		t.Fatalf("upload: %d %s", r.Code, r.Raw)
	}
	var list []map[string]any
	_ = json.Unmarshal([]byte(r.Raw), &list)
	if len(list) != 1 {
		t.Fatalf("upload: %s", r.Raw)
	}
	id := int64(list[0]["id"].(float64))
	if _, err := srv.store.Pool.Exec(ctx, `UPDATE media SET post_id = $1 WHERE id = $2`, draft.ID, id); err != nil {
		t.Fatal(err)
	}
	if r := hgCall(t, base, "GET", fmt.Sprintf("/upload/files/%d", id), up, nil); r.Code != 200 {
		t.Errorf("own upload: %d %s", r.Code, r.Raw)
	}
}
