package server

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"wpgo/internal/store"
)

// Custom field values of an attachment are saved with the attachment screen's form: only for the
// uploader or users with edit_others_posts (attachmentSave's rule); an author could change the fields
// of anyone's media.
func TestACFGoAttachmentFieldsNeedOwnership(t *testing.T) {
	ts, c, srv := testServerBuilderOff(t)
	base := ts.URL
	ctx := context.Background()
	installAndLogin(t, ts, c)
	acfgoActivate(t, c, base)
	acfgoSaveGroupForm(t, c, base, 0, map[string]any{"title": "Media credits", "position": "normal",
		"location": []any{[]any{map[string]any{"param": "attachment", "operator": "==", "value": "all"}}},
		"fields":   []any{map[string]any{"key": "field_credit", "label": "Credit", "name": "credit", "type": "text"}}})
	a := &store.Attachment{UploaderID: 1, Slug: "logo", FilePath: "2026/11/logo.png", Mime: "image/png", Title: "Logo"}
	if err := srv.store.InsertAttachment(ctx, a); err != nil {
		t.Fatal(err)
	}
	asCreateUser(t, srv, "writer", "author", asStrongPass)
	ac := asNewClient()
	if code, _, _ := asLogin(t, ac, base, "/wp-login", "writer", asStrongPass); code != http.StatusFound {
		t.Fatalf("login: %d", code)
	}
	send := func(cl *http.Client, value string) {
		t.Helper()
		code, body, _ := get(t, cl, base+"/wp-admin/post?post="+itoa64(a.ID)+"&action=edit")
		if cl != c && code == http.StatusForbidden {
			return // the edit screen itself is refused to a user who may not edit the attachment
		}
		m := nonceRe.FindStringSubmatch(body)
		if m == nil {
			t.Fatal("no nonce on the attachment screen")
		}
		post(t, cl, base+"/wp-admin/post", url.Values{"_wpnonce": {m[1]}, "post_ID": {itoa64(a.ID)}, "post_type": {"attachment"}, "acf[field_credit]": {value}})
	}
	credit := func() string {
		got, err := srv.store.GetAttachment(ctx, a.ID)
		if err != nil {
			t.Fatal(err)
		}
		v, _ := got.Meta["credit"].(string)
		return v
	}
	send(c, "Studio")
	if credit() != "Studio" {
		t.Fatalf("administrator's own save: %q", credit())
	}
	send(ac, "Hijacked")
	if credit() == "Hijacked" {
		t.Fatal("an author changed custom fields of the administrator's attachment")
	}
}

// The relationship field's search matched titles and content of every post: a contributor could read
// other users' draft and private titles (and probe their content word by word).
func TestACFGoRelationshipSearchHidesUnreadablePosts(t *testing.T) {
	ts, c, srv := testServerBuilderOff(t)
	base := ts.URL
	ctx := context.Background()
	installAndLogin(t, ts, c)
	acfgoActivate(t, c, base)
	for _, p := range []*store.Post{
		{Type: "post", Title: "Zircon merger draft", Slug: "zircon-draft", Status: "draft", AuthorID: 1, Content: "zircon", Meta: map[string]any{}},
		{Type: "post", Title: "Zircon private memo", Slug: "zircon-private", Status: "private", AuthorID: 1, Content: "zircon", Meta: map[string]any{}},
		{Type: "post", Title: "Zircon public news", Slug: "zircon-public", Status: "publish", AuthorID: 1, Content: "zircon", Meta: map[string]any{}},
	} {
		if err := srv.store.InsertPost(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	asCreateUser(t, srv, "contrib", "contributor", asStrongPass)
	cc := asNewClient()
	if code, _, _ := asLogin(t, cc, base, "/wp-login", "contrib", asStrongPass); code != http.StatusFound {
		t.Fatalf("login: %d", code)
	}
	_, body, _ := get(t, cc, base+"/wp-admin/admin-ajax?action=acfgo/query_posts&s=zircon")
	if strings.Contains(body, "merger draft") || strings.Contains(body, "private memo") || !strings.Contains(body, "public news") {
		t.Errorf("contributor search: %s", body)
	}
	if _, body, _ := get(t, cc, base+"/wp-admin/admin-ajax?action=acfgo/query_posts&s=&post_type=wp_block"); strings.Contains(body, `"id"`) {
		t.Errorf("post type outside the field's types: %s", body)
	}
	if _, body, _ := get(t, c, base+"/wp-admin/admin-ajax?action=acfgo/query_posts&s=zircon"); !strings.Contains(body, "merger draft") {
		t.Errorf("administrator search lost drafts: %s", body)
	}
}

// ?acf_format=standard is open to anyone: related posts the requester cannot read and authors' e-mail
// addresses must not be expanded into a public REST response (get_field in a theme is unchanged).
func TestACFGoRESTStandardFormatHidesPrivateData(t *testing.T) {
	ts, c, srv := testServerBuilderOff(t)
	base := ts.URL
	ctx := context.Background()
	installAndLogin(t, ts, c)
	acfgoActivate(t, c, base)
	acfgoSaveGroupForm(t, c, base, 0, map[string]any{"title": "Links", "show_in_rest": 1,
		"location": []any{[]any{map[string]any{"param": "post_type", "operator": "==", "value": "post"}}},
		"fields": []any{
			map[string]any{"key": "field_rel", "label": "Related", "name": "related", "type": "relationship", "return_format": "object"},
			map[string]any{"key": "field_owner", "label": "Owner", "name": "owner", "type": "user", "return_format": "array"},
		}})
	draft := &store.Post{Type: "post", Title: "Secret draft", Slug: "secret-draft", Status: "draft", AuthorID: 1, Content: "draft-body-xyz", Meta: map[string]any{}}
	locked := &store.Post{Type: "post", Title: "Locked", Slug: "locked", Status: "publish", AuthorID: 1, Content: "locked-body-xyz", Password: "pw", Meta: map[string]any{}}
	pub := &store.Post{Type: "post", Title: "Host", Slug: "host", Status: "publish", AuthorID: 1, Content: "host", Meta: map[string]any{}}
	for _, p := range []*store.Post{draft, locked, pub} {
		if err := srv.store.InsertPost(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	if err := srv.acfUpdateField(ctx, "related", []any{itoa64(draft.ID), itoa64(locked.ID)}, pub.ID); err != nil {
		t.Fatal(err)
	}
	if err := srv.acfUpdateField(ctx, "owner", "1", pub.ID); err != nil {
		t.Fatal(err)
	}
	admin, _ := srv.store.UserByID(ctx, 1)
	body := bodyOf(anonGet(t, base+"/wp-json/wp/v2/posts/"+itoa64(pub.ID)+"?acf_format=standard"))
	if !strings.Contains(body, `"acf"`) || !strings.Contains(body, `"Locked"`) {
		t.Fatalf("anonymous standard format: %s", body)
	}
	for _, leak := range []string{"Secret draft", "draft-body-xyz", "locked-body-xyz", admin.Email} {
		if strings.Contains(body, leak) {
			t.Errorf("anonymous standard format leaks %q", leak)
		}
	}
	// The administrator still gets everything.
	if _, body := restGet(t, c, base, "/wp-json/wp/v2/posts/"+itoa64(pub.ID)+"?acf_format=standard"); !strings.Contains(body, "draft-body-xyz") || !strings.Contains(body, admin.Email) {
		t.Errorf("administrator standard format lost data: %s", body)
	}
	// Theme output (no visitor in the context: anonymous) leaves out the draft too.
	if v, _ := srv.acfGetField(ctx, "related", pub.ID, true).([]any); len(v) != 1 {
		t.Errorf("get_field related = %v", v)
	}
}

// A taxonomy field with "Create Terms" turns "new:<name>" input into a new term only for users who may
// manage terms; an author saving a post must not create categories through it.
func TestACFGoTaxonomyAddTermNeedsManageTerms(t *testing.T) {
	ts, c, srv := testServerBuilderOff(t)
	base := ts.URL
	ctx := context.Background()
	installAndLogin(t, ts, c)
	acfgoActivate(t, c, base)
	acfgoSaveGroupForm(t, c, base, 0, map[string]any{"title": "Topics",
		"location": []any{[]any{map[string]any{"param": "post_type", "operator": "==", "value": "post"}}},
		"fields": []any{map[string]any{"key": "field_topics", "label": "Topics", "name": "topics", "type": "taxonomy",
			"taxonomy": "category", "field_type": "checkbox", "add_term": 1}}})
	authorID := asCreateUser(t, srv, "writer2", "author", asStrongPass)
	author, err := srv.store.UserByID(ctx, authorID)
	if err != nil {
		t.Fatal(err)
	}
	p := &store.Post{Type: "post", Title: "Mine", Status: "draft", AuthorID: author.ID}
	if err := srv.store.InsertPost(ctx, p); err != nil {
		t.Fatal(err)
	}
	in := map[string]any{"field_topics": []any{"new:Author Made"}}
	_ = srv.acfgoSaveInput(acfgoWithEditor(ctx, author), srv.acfgoPostObject(p), in, false)
	if _, err := srv.store.TermByName(ctx, "category", "Author Made"); err == nil {
		t.Error("an author created a category through a taxonomy field")
	}
	admin, err := srv.store.UserByID(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	in = map[string]any{"field_topics": []any{"new:Admin Made"}}
	_ = srv.acfgoSaveInput(acfgoWithEditor(ctx, admin), srv.acfgoPostObject(p), in, true)
	if _, err := srv.store.TermByName(ctx, "category", "Admin Made"); err != nil {
		t.Errorf("an administrator could not create a term: %v", err)
	}
}

// The classic save stored every posted acf[field_…] key: a subscriber could set, on their own
// profile, a field from a group whose location rules show it to administrators only.
func TestACFGoSaveSkipsFieldsOutsideLocationRules(t *testing.T) {
	ts, c, srv := testServerBuilderOff(t)
	base := ts.URL
	ctx := context.Background()
	installAndLogin(t, ts, c)
	acfgoActivate(t, c, base)
	acfgoSaveGroupForm(t, c, base, 0, map[string]any{"title": "Staff only",
		"location": []any{[]any{
			map[string]any{"param": "user_form", "operator": "==", "value": "all"},
			map[string]any{"param": "current_user_role", "operator": "==", "value": "administrator"}}},
		"fields": []any{map[string]any{"key": "field_staff_flag", "label": "Flag", "name": "staff_flag", "type": "text"}}})
	acfgoSaveGroupForm(t, c, base, 0, map[string]any{"title": "Everyone",
		"location": []any{[]any{map[string]any{"param": "user_form", "operator": "==", "value": "all"}}},
		"fields":   []any{map[string]any{"key": "field_bio_extra", "label": "Extra", "name": "bio_extra", "type": "text"}}})
	subID := asCreateUser(t, srv, "reader", "contributor", asStrongPass) // a role with wp-admin (subscribers have none)
	sc := asNewClient()
	if code, _, _ := asLogin(t, sc, base, "/wp-login", "reader", asStrongPass); code != http.StatusFound {
		t.Fatalf("login: %d", code)
	}
	_, body, _ := get(t, sc, base+"/wp-admin/profile")
	m := nonceRe.FindStringSubmatch(body)
	if m == nil {
		t.Fatal("no nonce on the profile screen")
	}
	post(t, sc, base+"/wp-admin/profile", url.Values{"_wpnonce": {m[1]}, "user_id": {itoa64(subID)}, "action": {"update"},
		"email": {"reader@example.com"}, "acf[field_staff_flag]": {"granted"}, "acf[field_bio_extra]": {"hello"}})
	u, err := srv.store.UserByID(ctx, subID)
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := u.Meta["staff_flag"]; ok {
		t.Errorf("a subscriber set an administrator-only field: %v", v)
	}
	if v, _ := u.Meta["bio_extra"].(string); v != "hello" {
		t.Errorf("a field the subscriber may edit was not saved: %v", u.Meta["bio_extra"])
	}
}

// An options page with the default capability (edit_posts) is open to contributors. Its save stored
// every posted acf[field_…] key into the shared "options" storage, so a contributor could overwrite
// the fields of another options page that requires manage_options.
func TestACFGoOptionsPageSavesOnlyOwnPageFields(t *testing.T) {
	ts, c, srv := testServerBuilderOff(t)
	base := ts.URL
	installAndLogin(t, ts, c)
	acfgoActivate(t, c, base)
	for _, p := range []url.Values{
		{"page_title": {"Notes"}, "active": {"1"}},
		{"page_title": {"Site Settings"}, "capability": {"manage_options"}, "active": {"1"}},
	} {
		_, body, _ := get(t, c, base+"/wp-admin/acf-options-page")
		p.Set("_wpnonce", formNonce(t, body))
		if code, _, _ := post(t, c, base+"/wp-admin/acf-options-page", p); code != http.StatusFound {
			t.Fatalf("create options page: %d", code)
		}
	}
	acfgoSaveGroupForm(t, c, base, 0, map[string]any{"title": "Notes", "location": []any{[]any{map[string]any{"param": "options_page", "operator": "==", "value": "notes"}}},
		"fields": []any{map[string]any{"key": "field_note", "label": "Note", "name": "note", "type": "text"}}})
	acfgoSaveGroupForm(t, c, base, 0, map[string]any{"title": "Site", "location": []any{[]any{map[string]any{"param": "options_page", "operator": "==", "value": "site-settings"}}},
		"fields": []any{map[string]any{"key": "field_support_url", "label": "Support URL", "name": "support_url", "type": "text"}}})
	_, body, _ := get(t, c, base+"/wp-admin/admin?page=site-settings")
	if code, _, _ := post(t, c, base+"/wp-admin/admin?page=site-settings", url.Values{"_wpnonce": {formNonce(t, body)}, "acf[field_support_url]": {"https://good.example"}}); code != http.StatusFound {
		t.Fatalf("admin save: %d", code)
	}
	asCreateUser(t, srv, "contrib", "contributor", asStrongPass)
	cc := asNewClient()
	if code, _, _ := asLogin(t, cc, base, "/wp-login", "contrib", asStrongPass); code != http.StatusFound {
		t.Fatalf("login: %d", code)
	}
	if code, _, _ := get(t, cc, base+"/wp-admin/admin?page=site-settings"); code != http.StatusForbidden {
		t.Fatalf("contributor reached the manage_options page: %d", code)
	}
	code, body, _ := get(t, cc, base+"/wp-admin/admin?page=notes")
	if code != http.StatusOK {
		t.Fatalf("contributor notes page: %d", code)
	}
	post(t, cc, base+"/wp-admin/admin?page=notes", url.Values{"_wpnonce": {formNonce(t, body)}, "acf[field_note]": {"hi"}, "acf[field_support_url]": {"https://evil.example"}})
	if got := srv.settings.String("options_support_url"); got != "https://good.example" {
		t.Fatalf("contributor overwrote a field of the manage_options options page: options_support_url = %q", got)
	}
	if got := srv.settings.String("options_note"); got != "hi" {
		t.Fatalf("contributor's own options page field not saved: options_note = %q", got)
	}
}

// The block editor's meta box save (admin-ajax acfgo/meta_box_save) stored every posted
// acf[field_…] key: an author could set, on their own post, a field from a group whose location
// rules show it to administrators only (the sibling of the classic save in acfgoSaveScreen).
func TestACFGoMetaBoxSaveSkipsFieldsOutsideLocationRules(t *testing.T) {
	ts, c, srv := testServerBuilderOff(t)
	base := ts.URL
	ctx := context.Background()
	installAndLogin(t, ts, c)
	acfgoActivate(t, c, base)
	acfgoSaveGroupForm(t, c, base, 0, map[string]any{"title": "Editorial (admins)",
		"location": []any{[]any{
			map[string]any{"param": "post_type", "operator": "==", "value": "post"},
			map[string]any{"param": "current_user_role", "operator": "==", "value": "administrator"}}},
		"fields": []any{map[string]any{"key": "field_sponsored", "label": "Sponsored", "name": "sponsored_by", "type": "text"}}})
	acfgoSaveGroupForm(t, c, base, 0, map[string]any{"title": "Everyone",
		"location": []any{[]any{map[string]any{"param": "post_type", "operator": "==", "value": "post"}}},
		"fields":   []any{map[string]any{"key": "field_subhead", "label": "Subhead", "name": "subhead", "type": "text"}}})
	authorID := asCreateUser(t, srv, "writer", "author", asStrongPass)
	p := &store.Post{Type: "post", Title: "Mine", Slug: "mine", Status: "draft", AuthorID: authorID, Meta: map[string]any{}}
	if err := srv.store.InsertPost(ctx, p); err != nil {
		t.Fatal(err)
	}
	ac := asNewClient()
	if code, _, _ := asLogin(t, ac, base, "/wp-login", "writer", asStrongPass); code != http.StatusFound {
		t.Fatalf("login: %d", code)
	}
	_, body, _ := get(t, ac, base+"/wp-admin/post?post="+itoa64(p.ID)+"&action=edit")
	m := regexp.MustCompile(`data-nonce="([^"]+)"`).FindStringSubmatch(body)
	if m == nil {
		t.Fatal("no acfgo-save nonce on the author's edit screen")
	}
	_, out, _ := post(t, ac, base+"/wp-admin/admin-ajax", url.Values{"action": {"acfgo/meta_box_save"}, "_acfnonce": {m[1]}, "post_ID": {itoa64(p.ID)},
		"acf[field_subhead]": {"ok"}, "acf[field_sponsored]": {"ACME"}})
	got, err := srv.store.GetPost(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Meta["subhead"] != "ok" {
		t.Fatalf("own field not saved (response %s): %v", out, got.Meta["subhead"])
	}
	if v, ok := got.Meta["sponsored_by"]; ok {
		t.Fatalf("author set an administrator-only field on their post: sponsored_by = %v", v)
	}
}
