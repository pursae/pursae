package server

import (
	"strings"
	"testing"
)

// A caller that may update entries but not read drafts gets back what it wrote, not the rest of
// the draft (which may hold someone else's unpublished changes), over REST and GraphQL.
func TestT2NHeadlessWriteAnswerHidesDraft(t *testing.T) {
	base, _, srv := hgSite(t)
	hgMustSchema(t, srv, &hgSchema{Kind: hgKindCollection, DisplayName: "Note", SingularName: "note", PluralName: "notes", DraftAndPublish: true,
		Fields: []hgField{{Name: "title", Type: "text"}, {Name: "body", Type: "text"}}})
	full := hgNewToken(t, srv, &hgToken{Name: "Full", Kind: "full-access"})
	writer := hgNewToken(t, srv, &hgToken{Name: "Writer", Kind: "custom", Permissions: map[string][]string{"note": {"find", "findOne", "update"}}})

	doc := hgDocOf(t, hgCall(t, base, "POST", "/notes?status=published", full, map[string]any{"data": map[string]any{"title": "Live", "body": "public"}}))
	if r := hgCall(t, base, "PUT", "/notes/"+doc, full, map[string]any{"data": map[string]any{"body": "unpublished secret"}}); r.Code != 200 {
		t.Fatalf("draft edit: %d %s", r.Code, r.Raw)
	}

	// An empty update answers the metadata only.
	r := hgCall(t, base, "PUT", "/notes/"+doc, writer, map[string]any{"data": map[string]any{}})
	if r.Code != 200 || strings.Contains(r.Raw, "secret") || r.data()["documentId"] != doc {
		t.Fatalf("empty update answer: %d %s", r.Code, r.Raw)
	}
	// A real update answers the written attribute.
	r = hgCall(t, base, "PUT", "/notes/"+doc, writer, map[string]any{"data": map[string]any{"title": "Renamed"}})
	if r.Code != 200 || strings.Contains(r.Raw, "secret") || r.data()["title"] != "Renamed" {
		t.Fatalf("update answer: %d %s", r.Code, r.Raw)
	}
	gql := hgCall(t, base, "POST", "/graphql", writer, map[string]any{"query": `mutation { updateNote(documentId: "` + doc + `", data: {}) { documentId title body } }`})
	if strings.Contains(gql.Raw, "secret") || !strings.Contains(gql.Raw, doc) {
		t.Fatalf("GraphQL update answer: %s", gql.Raw)
	}
	// Unpublishing leaves only the draft: its values stay hidden.
	r = hgCall(t, base, "POST", "/notes/"+doc+"/unpublish", writer, nil)
	if r.Code != 200 || strings.Contains(r.Raw, "secret") {
		t.Fatalf("unpublish answer: %d %s", r.Code, r.Raw)
	}
	gql = hgCall(t, base, "POST", "/graphql", writer, map[string]any{"query": `mutation { unpublishNote(documentId: "` + doc + `") { documentId body } }`})
	if strings.Contains(gql.Raw, "secret") || !strings.Contains(gql.Raw, doc) {
		t.Fatalf("GraphQL unpublish answer: %s", gql.Raw)
	}
	// A token that reads drafts still gets the whole draft.
	r = hgCall(t, base, "PUT", "/notes/"+doc, full, map[string]any{"data": map[string]any{}})
	if r.Code != 200 || r.data()["body"] != "unpublished secret" {
		t.Fatalf("full-access update answer: %d %s", r.Code, r.Raw)
	}
}
