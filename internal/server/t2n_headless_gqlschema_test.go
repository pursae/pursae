package server

import (
	"context"
	"strings"
	"testing"
)

// GraphQL introspection and validation hints show a caller only the types it may use (and what
// they reach), like its OpenAPI document; a full-access token sees the whole model.
func TestT2NHeadlessGraphQLSchemaPerCaller(t *testing.T) {
	base, _, srv := hgSite(t)
	ctx := context.Background()
	hgMemo(t, srv, false)
	hgMustSchema(t, srv, &hgSchema{Kind: hgKindCollection, DisplayName: "Merger plan", SingularName: "mergerplan", PluralName: "mergerplans",
		Fields: []hgField{{Name: "targetcompany", Type: "text"}}})
	st := srv.hgLoad()
	st.Public = map[string][]string{"memo": {"find", "findOne"}}
	_ = srv.hgSaveSettings(ctx, st)
	full := hgNewToken(t, srv, &hgToken{Name: "Full", Kind: "full-access"})
	gql := func(tok, q string) string {
		return hgCall(t, base, "POST", "/graphql", tok, map[string]any{"query": q}).Raw
	}
	intro := `{ __schema { types { name fields { name } } mutationType { fields { name } } } }`

	pub := gql("", intro)
	if !strings.Contains(pub, `"Memo"`) || strings.Contains(pub, "Mergerplan") || strings.Contains(pub, "targetcompany") || strings.Contains(pub, "createMemo") {
		t.Fatalf("public introspection: %s", pub)
	}
	if r := gql("", `{ __type(name: "Mergerplan") { name fields { name } } }`); strings.Contains(r, "targetcompany") {
		t.Fatalf("public __type: %s", r)
	}
	if r := gql("", `{ mergerplanz { targetcompany } }`); strings.Contains(r, "Did you mean") {
		t.Fatalf("a validation hint names a hidden field: %s", r)
	}
	if r := gql("", `{ memos { titel } }`); !strings.Contains(r, `Did you mean \"title\"`) {
		t.Fatalf("hints on visible types are gone: %s", r)
	}
	if r := gql(full, intro); !strings.Contains(r, "Mergerplan") || !strings.Contains(r, "targetcompany") || !strings.Contains(r, "createMemo") {
		t.Fatalf("full-access introspection: %s", r)
	}
}
