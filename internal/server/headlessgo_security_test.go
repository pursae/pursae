package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"wpgo/internal/store"
)

// Filtering and sorting through a relation only consider targets the caller may read: types it
// has no find/findOne permission on are invalid keys, WordPress posts and pages must be published,
// and a user's login name is not a filter or sort key.
func TestHeadlessGoRelationFilterPermissions(t *testing.T) {
	base, _, srv := hgSite(t)
	ctx := context.Background()
	hgModel(t, srv)
	st := srv.hgLoad()
	st.Public = map[string][]string{"article": {"find", "findOne"}, "category": {"find", "findOne"}}
	_ = srv.hgSaveSettings(ctx, st)
	full := hgNewToken(t, srv, &hgToken{Name: "Full", Kind: "full-access"})

	page := &store.Post{Type: "page", Title: "Layoffs plan", Slug: "layoffs", Status: "publish", AuthorID: 1}
	if err := srv.store.InsertPost(ctx, page); err != nil {
		t.Fatal(err)
	}
	r := hgCall(t, base, "POST", "/authors", full, map[string]any{"data": map[string]any{"name": "Ada"}})
	ada := r.data()["documentId"].(string)
	r = hgCall(t, base, "POST", "/articles?status=published", full, map[string]any{"data": map[string]any{"title": "Public post", "writer": 1, "page": page.ID, "author": ada}})
	if r.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", r.Code, r.Raw)
	}
	// The page becomes private after it was linked.
	if _, err := srv.store.Pool.Exec(ctx, `UPDATE posts SET status = 'private' WHERE id = $1`, page.ID); err != nil {
		t.Fatal(err)
	}

	count := func(path string) (int, int) {
		r := hgCall(t, base, "GET", path, "", nil)
		return r.Code, len(r.list())
	}
	for _, path := range []string{
		"/articles?filters[writer][username][$startsWith]=a", // login names
		"/articles?sort=writer.username",
		"/articles?filters[author][name][$eq]=Ada", // the public may not read authors
		"/articles?filters[author][$notNull]=true",
		"/articles?sort=author.name",
	} {
		if code, n := count(path); code != http.StatusBadRequest {
			t.Errorf("%s: %d with %d results, want 400", path, code, n)
		}
	}
	for _, path := range []string{
		"/articles?filters[page][status][$eq]=private", // a private page is not a readable target
		"/articles?filters[page][title][$eq]=Layoffs%20plan",
		"/articles?filters[page][id][$gt]=0",
	} {
		if code, n := count(path); code != http.StatusOK || n != 0 {
			t.Errorf("%s: %d with %d results, want 200 and none", path, code, n)
		}
	}
	// Published again, the page is a readable target.
	_, _ = srv.store.Pool.Exec(ctx, `UPDATE posts SET status = 'publish' WHERE id = $1`, page.ID)
	if code, n := count("/articles?filters[page][title][$eq]=Layoffs%20plan"); code != 200 || n != 1 {
		t.Errorf("filter through a published page: %d %d", code, n)
	}
	// What the public may read still filters: the user's public slug and name, readable types.
	if code, n := count("/articles?filters[writer][slug][$eq]=admin"); code != 200 || n != 1 {
		t.Errorf("filter by the user's slug: %d %d", code, n)
	}
	// A full-access token reads authors, so it may filter through them.
	if r := hgCall(t, base, "GET", "/articles?filters[author][name][$eq]=Ada&sort=author.name", full, nil); r.Code != 200 || len(r.list()) != 1 {
		t.Errorf("full-access filter through authors: %d %s", r.Code, r.Raw)
	}
	if r := hgCall(t, base, "GET", "/articles?filters[writer][username][$eq]=admin", full, nil); r.Code != 400 {
		t.Errorf("login names are not filterable, even with a token: %d %s", r.Code, r.Raw)
	}
}

// A write's response with populate shows related entries as a read would: no drafts for a token
// that may not read them.
func TestHeadlessGoWritePopulateHidesDrafts(t *testing.T) {
	base, _, srv := hgSite(t)
	hgModel(t, srv)
	full := hgNewToken(t, srv, &hgToken{Name: "Full", Kind: "full-access"})
	r := hgCall(t, base, "POST", "/categories", full, map[string]any{"data": map[string]any{"name": "Unannounced"}})
	cat := r.data()["documentId"].(string)
	writer := hgNewToken(t, srv, &hgToken{Name: "Writer", Kind: "custom", Permissions: map[string][]string{
		"article": {"create", "find", "findOne"}, "category": {"find", "findOne"}}})
	r = hgCall(t, base, "POST", "/articles?populate=categories", writer, map[string]any{"data": map[string]any{"title": "Mine", "categories": []any{cat}}})
	if r.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", r.Code, r.Raw)
	}
	if strings.Contains(r.Raw, "Unannounced") {
		t.Errorf("a write response shows a related draft: %s", r.Raw)
	}
	// The full-access token reads drafts: it sees the related draft.
	r = hgCall(t, base, "POST", "/articles?populate=categories", full, map[string]any{"data": map[string]any{"title": "Ours", "categories": []any{cat}}})
	if !strings.Contains(r.Raw, "Unannounced") {
		t.Errorf("full-access write response: %s", r.Raw)
	}
}

// A preview token reads its own document's draft in its own locale, and nothing else: no other
// locale, no drafts of related entries, no types the public may not read.
func TestHeadlessGoPreviewTokenScope(t *testing.T) {
	base, c, srv := hgSite(t)
	ctx := context.Background()
	golylangToggle(t, c, base, "activate")
	pllAddLanguage(t, c, base, "English", "en_US", "en", "us")
	pllAddLanguage(t, c, base, "Français", "fr_FR", "fr", "fr")
	hgModel(t, srv)
	st := srv.hgLoad()
	st.Public = map[string][]string{"article": {"find", "findOne"}, "category": {"find", "findOne"}}
	_ = srv.hgSaveSettings(ctx, st)
	full := hgNewToken(t, srv, &hgToken{Name: "Full", Kind: "full-access"})
	r := hgCall(t, base, "POST", "/authors", full, map[string]any{"data": map[string]any{"name": "Ada Hidden"}})
	ada := r.data()["documentId"].(string)
	r = hgCall(t, base, "POST", "/categories", full, map[string]any{"data": map[string]any{"name": "Unannounced"}})
	cat := r.data()["documentId"].(string)
	r = hgCall(t, base, "POST", "/articles", full, map[string]any{"data": map[string]any{"title": "Draft en", "author": ada, "categories": []any{cat}}})
	doc := r.data()["documentId"].(string)
	if r := hgCall(t, base, "PUT", "/articles/"+doc+"?locale=fr", full, map[string]any{"data": map[string]any{"title": "Brouillon secret"}}); r.Code != 200 {
		t.Fatalf("fr draft: %d %s", r.Code, r.Raw)
	}
	ptok := srv.hgPreviewToken(srv.hgLoad(), "article", doc, "en", time.Hour)

	if r := hgCall(t, base, "GET", "/articles/"+doc+"?status=draft", ptok, nil); r.Code != 200 || r.data()["title"] != "Draft en" {
		t.Fatalf("preview read: %d %s", r.Code, r.Raw)
	}
	for _, path := range []string{"/articles/" + doc + "?status=draft&locale=fr", "/articles?status=draft&locale=fr"} {
		if r := hgCall(t, base, "GET", path, ptok, nil); r.Code != http.StatusForbidden || strings.Contains(r.Raw, "Brouillon") {
			t.Errorf("preview token in another locale %s: %d %s", path, r.Code, r.Raw)
		}
	}
	r = hgCall(t, base, "GET", "/articles/"+doc+"?status=draft&populate=author,categories,localizations", ptok, nil)
	if r.Code != 200 || strings.Contains(r.Raw, "Ada Hidden") || strings.Contains(r.Raw, "Unannounced") || strings.Contains(r.Raw, "Brouillon") {
		t.Errorf("preview populate reaches other drafts or types: %d %s", r.Code, r.Raw)
	}
	if r := hgCall(t, base, "GET", "/articles?status=draft&filters[categories][name][$eq]=Unannounced", ptok, nil); r.Code != 200 || len(r.list()) != 0 {
		t.Errorf("preview filter through a draft category: %d %s", r.Code, r.Raw)
	}
	if r := hgCall(t, base, "GET", "/articles?status=draft&filters[author][name][$eq]=Ada%20Hidden", ptok, nil); r.Code != 400 {
		t.Errorf("preview filter through a type the public cannot read: %d %s", r.Code, r.Raw)
	}
	if r := hgCall(t, base, "GET", "/categories/"+cat+"?status=draft", ptok, nil); r.Code != http.StatusForbidden {
		t.Errorf("preview token on another type: %d %s", r.Code, r.Raw)
	}
	// GraphQL: the same scope.
	q := `{"query":"{ article(documentId: \"` + doc + `\", status: DRAFT, locale: \"fr\") { title } }"}`
	req, _ := http.NewRequest(http.MethodPost, base+hgAPIPrefix+"/graphql", strings.NewReader(q))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+ptok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if raw, _ := json.Marshal(out); strings.Contains(string(raw), "Brouillon") || out["errors"] == nil {
		t.Errorf("GraphQL preview in another locale: %s", raw)
	}
}

// Webhooks are delivered to public addresses only: the address is checked when the connection
// is made (after DNS resolution), so a webhook cannot read the server's own services.
func TestHeadlessGoWebhookPublicOnly(t *testing.T) {
	_, _, srv := hgSite(t)
	ctx := context.Background()
	var hits atomic.Int32
	inside := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte("internal-secret-body"))
	}))
	defer inside.Close()
	u, _ := url.Parse(inside.URL)
	for _, target := range []string{inside.URL + "/x", "http://localhost:" + u.Port() + "/x"} {
		w := &hgWebhook{Name: "Inside", URL: target, Events: []string{"entry.create"}, Enabled: true}
		if err := srv.hgSaveWebhook(ctx, w); err != nil {
			t.Fatal(err)
		}
		res := srv.hgTestWebhook(ctx, w)
		if res.Success || res.StatusCode != 0 || strings.Contains(res.Response, "internal-secret") || res.Error == "" {
			t.Errorf("webhook to %s: %+v", target, res)
		}
	}
	if hits.Load() != 0 {
		t.Errorf("the loopback server was reached %d times", hits.Load())
	}
}

// The admin relation picker lists what the user may read: a contributor sees published posts and
// their own, never other users' drafts or private posts, and never login names.
func TestHeadlessGoAdminSearchScope(t *testing.T) {
	base, c, srv := hgSite(t)
	ctx := context.Background()
	hgModel(t, srv)
	contrib := hgSecondUser(t, srv, base, "contrib", "contributor")
	var cid int64
	_ = srv.store.Pool.QueryRow(ctx, `SELECT id FROM users WHERE login = 'contrib'`).Scan(&cid)
	_, _ = srv.store.Pool.Exec(ctx, `UPDATE users SET display_name = 'Site Owner', nicename = 'owner' WHERE id = 1`)
	for _, p := range []*store.Post{
		{Type: "post", Title: "Hello public", Slug: "hello-public", Status: "publish", AuthorID: 1},
		{Type: "post", Title: "Secret plan", Slug: "secret-plan", Status: "draft", AuthorID: 1},
		{Type: "post", Title: "Private memo", Slug: "private-memo", Status: "private", AuthorID: 1},
		{Type: "post", Title: "My own draft", Slug: "my-own-draft", Status: "draft", AuthorID: cid},
	} {
		if err := srv.store.InsertPost(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	_, body, _ := get(t, contrib, base+"/wp-admin/headlessgo/search?target=wp:post&q=")
	if !strings.Contains(body, "Hello public") || !strings.Contains(body, "My own draft") {
		t.Errorf("contributor search misses readable posts: %s", body)
	}
	if strings.Contains(body, "Secret plan") || strings.Contains(body, "Private memo") {
		t.Errorf("contributor search lists other users' drafts or private posts: %s", body)
	}
	_, body, _ = get(t, contrib, base+"/wp-admin/headlessgo/search?target=wp:user&q=adm")
	if strings.Contains(body, "admin") {
		t.Errorf("contributor user search by login name: %s", body)
	}
	_, body, _ = get(t, contrib, base+"/wp-admin/headlessgo/search?target=wp:user&q=Owner")
	if !strings.Contains(body, "Site Owner") || strings.Contains(body, "(admin)") {
		t.Errorf("contributor user search: %s", body)
	}
	// The administrator still finds everything, logins included.
	_, body, _ = get(t, c, base+"/wp-admin/headlessgo/search?target=wp:post&q=")
	if !strings.Contains(body, "Secret plan") || !strings.Contains(body, "Private memo") {
		t.Errorf("administrator search: %s", body)
	}
	_, body, _ = get(t, c, base+"/wp-admin/headlessgo/search?target=wp:user&q=adm")
	if !strings.Contains(body, "(admin)") {
		t.Errorf("administrator user search: %s", body)
	}
}

// The Headless wp* GraphQL fields must apply the Commerce Memberships restriction like core REST.
func TestHeadlessGoGraphQLWordPressContentMembershipRestricted(t *testing.T) {
	base, c, srv := gooSite(t)
	hgToggle(t, c, base, "activate")
	ctx := context.Background()
	news := &store.Post{Type: "post", Title: "Members news", Slug: "members-news", Content: "<p>The secret recipe.</p>", Excerpt: "A teaser.", Status: "publish", AuthorID: 1, Meta: map[string]any{}}
	if err := srv.store.InsertPost(ctx, news); err != nil {
		t.Fatal(err)
	}
	gold := &gooMembershipPlan{Name: "Gold", AccessMethod: "manual", Rules: gmemRules{ContentPostIDs: []int64{news.ID}}}
	if err := srv.gmemSavePlan(ctx, gold); err != nil {
		t.Fatal(err)
	}
	gxaOn(t, srv, gmemKey, true)
	gxaSetExt(t, srv, gmemKey, map[string]any{"show_excerpts": false})

	q := map[string]any{"query": `{ wpPosts { slug content excerpt } wpPost(slug: "members-news") { content } wpPosts_connection { nodes { content } } }`}
	r := hgCall(t, base, "POST", "/graphql", "", q)
	if r.Code != 200 || !strings.Contains(r.Raw, "members-news") {
		t.Fatalf("graphql: %d %s", r.Code, r.Raw)
	}
	if strings.Contains(r.Raw, "secret recipe") || strings.Contains(r.Raw, "A teaser.") {
		t.Errorf("non-member read restricted content: %s", r.Raw)
	}
	if !strings.Contains(r.Raw, "wc-memberships-content-restricted-message") {
		t.Errorf("no restriction message: %s", r.Raw)
	}

	// A caller with access (a membership manager) still gets the full body.
	ex := &hgGQLExec{s: srv, c: gooTestCtx(), a: &hgAuth{Kind: "user", UserID: 1}}
	nodes := ex.wpNodes([]*store.Post{news}, []*hgGQLField{{name: "content"}, {name: "excerpt"}})
	if got, _ := nodes[0]["content"].(string); !strings.Contains(got, "secret recipe") {
		t.Errorf("member content: %v", nodes[0])
	}
}

// The operators of one attribute ($not, $and, $or) and of a WordPress column filter nest no
// deeper than the filter tree itself: a long chain is a 400 after a few levels, not quadratic work.
func TestHeadlessGoFilterOperatorNestingBounded(t *testing.T) {
	sc := &hgSchema{UID: "api::article.article", Kind: hgKindCollection, Fields: []hgField{{Name: "title", Type: "text"},
		{Name: "cover", Type: "media"}, {Name: "page", Type: "relation", Relation: "manyToOne", Target: "wp:page"}}}
	compile := func(key string) error {
		qs := hgParseQS(url.Values{key: {"x"}})
		f, _ := qs["filters"].(map[string]any)
		q := &hgQuery{sql: &hgSQL{}, status: "published"}
		_, err := q.filters(sc, f, "e.published_data", "e")
		return err
	}
	for _, key := range []string{
		"filters[title]" + strings.Repeat("[$not]", 64) + "[$eq]",
		"filters[title]" + strings.Repeat("[$and][0]", 64) + "[$eq]",
		"filters[title]" + strings.Repeat("[$or][0][$not]", 32) + "[$eq]",
		"filters[page]" + strings.Repeat("[$not]", 64) + "[title][$eq]",    // WordPress columns (table)
		"filters[cover]" + strings.Repeat("[$and][0]", 64) + "[name][$eq]", // media columns (table)
		"filters[title]" + strings.Repeat("[$not]", 100000) + "[$eq]",      // a 1 MiB request line
	} {
		start := time.Now()
		err := compile(key)
		if err == nil || !strings.Contains(err.Error(), "nested too deeply") {
			t.Errorf("%.60s...: err = %v, want nested too deeply", key, err)
		}
		if el := time.Since(start); el > time.Second {
			t.Errorf("%.60s...: refusing took %v", key, el)
		}
	}
	// Ordinary operator use still compiles.
	for _, key := range []string{
		"filters[title][$not][$eq]", "filters[title][$or][0][$eq]", "filters[title][$and][0][$not][$containsi]",
		"filters[page][$not][title][$eq]", "filters[page][$or][0][slug][$eq]",
	} {
		if err := compile(key); err != nil {
			t.Errorf("%s: %v", key, err)
		}
	}
}

// Front-end user tokens and preview tokens are signed with the effective server secret: with
// PURSAE_SECRET set (newServer then never writes auth_secret) a token signed without it is refused,
// and tokens the server issues still verify.
func TestHeadlessGoUserJWTUsesServerSecret(t *testing.T) {
	base, _, srv := hgSite(t)
	ctx := context.Background()
	asCreateUser(t, srv, "alice", "subscriber", "alice-secret-pw")
	st := srv.hgLoad()
	st.UserAuth = true
	_ = srv.hgSaveSettings(ctx, st)
	// The state of a site started with PURSAE_SECRET from the first boot (server.go newServer).
	srv.cfg.Secret = "operator-provided-wpgo-secret"
	if err := srv.settings.Set(ctx, map[string]any{"auth_secret": ""}); err != nil {
		t.Fatal(err)
	}
	r := hgCall(t, base, "POST", "/auth/local", "", map[string]any{"identifier": "alice", "password": "alice-secret-pw"})
	jwt, _ := r.Body["jwt"].(string)
	if r.Code != 200 || jwt == "" {
		t.Fatalf("login: %d %s", r.Code, r.Raw)
	}
	if r := hgCall(t, base, "GET", "/users/me", jwt, nil); r.Code != http.StatusOK {
		t.Fatalf("an issued JWT: %d %s", r.Code, r.Raw)
	}
	parts := strings.Split(jwt, ".")
	raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims map[string]any
	_ = json.Unmarshal(raw, &claims)
	claims["exp"] = time.Now().Add(10 * 365 * 24 * time.Hour).Unix()
	for _, key := range []string{"|headlessgo-user-jwt", ""} { // no server secret involved
		forged := hgJWTSign([]byte(key), claims)
		if r := hgCall(t, base, "GET", "/users/me", forged, nil); r.Code != http.StatusUnauthorized {
			t.Fatalf("a JWT signed with %q was accepted: %d %s", key, r.Code, r.Raw)
		}
	}
	// Preview tokens: issued ones verify, ones signed without the server secret do not.
	st = srv.hgLoad()
	st.PreviewSecret = "front-end-preview-secret"
	tok := srv.hgPreviewToken(st, "api::article.article", "doc1", "", time.Minute)
	if uid, doc, _, ok := srv.hgCheckPreviewToken(st, tok); !ok || uid != "api::article.article" || doc != "doc1" {
		t.Fatalf("an issued preview token did not verify: %v %s %s", ok, uid, doc)
	}
	srv.cfg.Secret = ""
	if _, _, _, ok := srv.hgCheckPreviewToken(st, tok); ok {
		t.Fatalf("a preview token verified without the server secret it was signed with")
	}
}

// A WordPress post reached through a relation shows no more than core REST shows the same caller
// (no excerpt of a password-protected page), and media fields neither link nor show files of
// drafts or protected posts to callers who cannot browse the media library.
func TestHeadlessGoRelationsNoProtectedPostOrMediaLeak(t *testing.T) {
	base, _, srv := hgSite(t)
	ctx := context.Background()
	hgModel(t, srv)
	asCreateUser(t, srv, "mallory", "subscriber", "mallory-secret-pw")
	st := srv.hgLoad()
	st.UserAuth = true
	st.Authenticated = map[string][]string{"article": {"find", "findOne", "create", "update"}} // update: ?status=published needs it
	st.Public = map[string][]string{"article": {"find", "findOne"}}
	_ = srv.hgSaveSettings(ctx, st)
	full := hgNewToken(t, srv, &hgToken{Name: "Full", Kind: "full-access"})

	locked := &store.Post{Type: "page", Title: "Board notes", Slug: "board-notes", Status: "publish", Password: "letmein",
		Excerpt: "Layoffs start on May 5", Content: "<p>Details</p>", AuthorID: 1, Meta: map[string]any{}}
	open := &store.Post{Type: "page", Title: "About", Slug: "about", Status: "publish",
		Excerpt: "Who we are<script>alert(1)</script>", Content: "<p>Hi</p>", AuthorID: 1, Meta: map[string]any{}}
	draft := &store.Post{Type: "post", Title: "Launch", Slug: "launch", Status: "draft", AuthorID: 1, Meta: map[string]any{}}
	for _, p := range []*store.Post{locked, open, draft} {
		if err := srv.store.InsertPost(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	att := &store.Attachment{UploaderID: 1, PostID: draft.ID, Slug: "phone-x", FilePath: "2026/11/unreleased-phone-x.png", Mime: "image/png",
		FileSize: 2048, Width: 640, Height: 480, Title: "Unreleased Phone X", Caption: "Embargoed until launch"}
	logo := &store.Attachment{UploaderID: 1, Slug: "logo", FilePath: "2026/11/logo.png", Mime: "image/png",
		FileSize: 1024, Width: 64, Height: 64, Title: "Logo"}
	for _, a := range []*store.Attachment{att, logo} {
		if err := srv.store.InsertAttachment(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	r := hgCall(t, base, "POST", "/auth/local", "", map[string]any{"identifier": "mallory", "password": "mallory-secret-pw"})
	jwt, _ := r.Body["jwt"].(string)
	if jwt == "" {
		t.Fatalf("login: %d %s", r.Code, r.Raw)
	}
	// The front-end user cannot link the draft's file (it reads as missing).
	r = hgCall(t, base, "POST", "/articles?status=published&populate[0]=page&populate[1]=cover", jwt,
		map[string]any{"data": map[string]any{"title": "Probe", "page": locked.ID, "cover": att.ID}})
	if r.Code != http.StatusBadRequest || strings.Contains(r.Raw, "Unreleased Phone X") || strings.Contains(r.Raw, "unreleased-phone-x.png") {
		t.Fatalf("linking a draft's file: %d %s", r.Code, r.Raw)
	}
	// A protected page and a public file link; the page's excerpt stays hidden.
	r = hgCall(t, base, "POST", "/articles?status=published&populate[0]=page&populate[1]=cover", jwt,
		map[string]any{"data": map[string]any{"title": "Probe", "page": locked.ID, "cover": logo.ID}})
	if r.Code != http.StatusCreated || !strings.Contains(r.Raw, "board-notes") || !strings.Contains(r.Raw, "logo.png") {
		t.Fatalf("create: %d %s", r.Code, r.Raw)
	}
	if strings.Contains(r.Raw, "Layoffs start on May 5") {
		t.Errorf("the excerpt of a password-protected page leaked through a wp:page relation: %s", r.Raw)
	}
	// An editor (full-access token) links the draft's file and the public page; the public reads
	// neither the file nor the protected excerpt, the open page's excerpt is KSES-sanitized.
	r = hgCall(t, base, "POST", "/articles?status=published&populate[0]=cover", full,
		map[string]any{"data": map[string]any{"title": "Linked by an editor", "page": open.ID, "cover": att.ID}})
	if r.Code != http.StatusCreated || !strings.Contains(r.Raw, "Unreleased Phone X") {
		t.Fatalf("full-access token linking a draft's file: %d %s", r.Code, r.Raw)
	}
	r = hgCall(t, base, "GET", "/articles?populate[0]=page&populate[1]=cover", "", nil)
	if r.Code != http.StatusOK || !strings.Contains(r.Raw, "Who we are") {
		t.Fatalf("public read: %d %s", r.Code, r.Raw)
	}
	for _, leak := range []string{"Unreleased Phone X", "unreleased-phone-x.png", "Layoffs start on May 5", "<script", `\u003cscript`} {
		if strings.Contains(r.Raw, leak) {
			t.Errorf("public read shows %q: %s", leak, r.Raw)
		}
	}
	// Nor can the public filter on the hidden file's metadata.
	if r := hgCall(t, base, "GET", "/articles?filters[cover][caption][$eq]=Embargoed%20until%20launch", "", nil); r.Code != 200 || len(r.list()) != 0 {
		t.Errorf("filter on a draft's file: %d %s", r.Code, r.Raw)
	}
	// The full-access token still reads everything.
	if r := hgCall(t, base, "GET", "/articles?populate[0]=cover&filters[cover][caption][$eq]=Embargoed%20until%20launch", full, nil); r.Code != 200 || !strings.Contains(r.Raw, "Unreleased Phone X") {
		t.Errorf("full-access read: %d %s", r.Code, r.Raw)
	}
}
