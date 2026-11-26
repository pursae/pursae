package server

import (
	"context"
	"encoding/json"
	"net/url"
	"slices"
	"testing"

	"wpgo/internal/webbuilder"
)

// A form's own To/Cc come from the page document. When an author without manage_options or
// unfiltered_html published it, only the site's own addresses are used.
func TestT2NMiscWebBuilderFormRecipientsOfAuthors(t *testing.T) {
	w := wbformStart(t)
	ctx := context.Background()
	authorID := asCreateUser(t, w.srv, "writer", "author", asStrongPass)
	wbfSetContent(t, w.srv, w.page.ID, wbfNewsletter, map[string]string{
		webbuilder.FormKeyTo: "victim@elsewhere.example, writer@example.test", webbuilder.FormKeyCc: "other@elsewhere.example"})
	publishedBy := func(id int64) {
		if _, err := w.srv.store.Pool.Exec(ctx, `UPDATE web_builder_docs SET snapshot_user = $2 WHERE post_id = $1`, w.page.ID, id); err != nil {
			t.Fatal(err)
		}
	}
	recipients := func() (to, cc []string) {
		_, schema, ok := func() (any, webbuilder.FormSchema, bool) {
			row, err := w.srv.store.WebBuilder().Doc(ctx, w.page.ID)
			if err != nil {
				t.Fatal(err)
			}
			var doc webbuilder.DocState
			if err := json.Unmarshal(row.Doc, &doc); err != nil {
				t.Fatal(err)
			}
			s, ok := webbuilder.FindForm(&doc, wbfNewsletter)
			return nil, s, ok
		}()
		if !ok {
			t.Fatal("form not found")
		}
		return w.srv.wbFormRecipients(ctx, w.page, schema)
	}

	publishedBy(authorID)
	to, cc := recipients()
	if slices.Contains(to, "victim@elsewhere.example") || slices.Contains(cc, "other@elsewhere.example") {
		t.Errorf("an author's page mails outside addresses: to %v cc %v", to, cc)
	}
	if !slices.Contains(to, "writer@example.test") {
		t.Errorf("a site user's address was dropped: %v", to)
	}

	publishedBy(1)
	if to, cc := recipients(); !slices.Contains(to, "victim@elsewhere.example") || !slices.Contains(cc, "other@elsewhere.example") {
		t.Errorf("an administrator's recipients were limited: to %v cc %v", to, cc)
	}

	// End to end: the queued mail of an author's page goes to the site's own addresses only.
	publishedBy(authorID)
	if code, _, _ := w.submit(t, nil, w.page.ID, wbfNewsletter, url.Values{"email": {"bob@example.test"}}, true); code != 200 {
		t.Fatalf("submit: %d", code)
	}
	m := mailJobs(t, w.srv)[0]
	for _, k := range []string{"To", "Cc"} {
		list, _ := m[k].([]any)
		for _, a := range list {
			if a == "victim@elsewhere.example" || a == "other@elsewhere.example" {
				t.Errorf("%s holds %v", k, a)
			}
		}
	}
}
