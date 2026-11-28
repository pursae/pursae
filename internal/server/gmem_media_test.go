package server

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"wpgo/internal/store"
)

// A Memberships-restricted post's attachments are not readable by visitors through the media API.
func TestGmemRestrictedPostMediaHidden(t *testing.T) {
	base, _, srv := gooSite(t)
	ctx := context.Background()
	locked := &store.Post{Type: "post", Title: "Locked", Slug: "locked", Status: "publish", AuthorID: 1, Meta: map[string]any{}}
	if err := srv.store.InsertPost(ctx, locked); err != nil {
		t.Fatal(err)
	}
	att, err := srv.insertAttachmentFile(ctx, "velvetfile.png", bytes.NewReader(hgPNG(10, 10)), 1, locked.ID)
	if err != nil {
		t.Fatal(err)
	}
	plan := &gooMembershipPlan{Name: "Velvet", AccessMethod: "manual", AccessLength: "unlimited", Rules: gmemRules{ContentPostIDs: []int64{locked.ID}}}
	if err := srv.gmemSavePlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	if err := srv.gooSetExtension(ctx, gmemKey, true); err != nil {
		t.Fatal(err)
	}
	get := func(path string) (int, string) {
		resp, err := http.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, _ := get(fmt.Sprintf("/wp-json/wp/v2/media/%d", att.ID)); code == http.StatusOK {
		t.Errorf("restricted post's attachment readable: %d", code)
	}
	if _, b := get("/wp-json/wp/v2/media"); strings.Contains(b, "velvetfile") {
		t.Errorf("restricted post's attachment listed")
	}
	// The hidden file is not counted either: X-WP-Total matches what a visitor can list.
	resp, err := http.Get(base + "/wp-json/wp/v2/media")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := resp.Header.Get("X-WP-Total"); got != "0" {
		t.Errorf("X-WP-Total counts the hidden attachment: %q", got)
	}
}
