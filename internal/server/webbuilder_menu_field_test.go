package server

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"wpgo/internal/store"
	"wpgo/internal/webbuilder"
)

func wbExec(t *testing.T, src string, r webbuilder.Resolver) string {
	t.Helper()
	prog, err := webbuilder.Compile(src)
	if err != nil {
		t.Fatal(err)
	}
	out, err := prog.ExecString(r)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// A menu location lists its sub-menu items too (depth first) and marks their level.
func TestWBMenuSubItems(t *testing.T) {
	leaf := &store.MenuItem{ID: 3, Label: "Deep", URL: "/deep/"}
	child := &store.MenuItem{ID: 2, Label: "Child", URL: "/child/", Children: []*store.MenuItem{leaf}}
	top := &store.MenuItem{ID: 1, Label: "Top", URL: "/top/", Children: []*store.MenuItem{child}}
	other := &store.MenuItem{ID: 4, Label: "Other", URL: "/other/", Current: true}
	env := &wbSlotEnv{v: &siteView{Menus: map[string][]*store.MenuItem{"primary": {top, other}}}}
	src := `<nav data-wb-menu-loc="primary"><a data-wb-loop-item="" data-wb-slot="menu.url"><span data-wb-slot="menu.label"></span></a></nav>`
	out := wbExec(t, src, &wbSlotResolver{env: env})
	for _, want := range []string{
		`href="/top/" class="has-children"`,
		`href="/child/" class="wb-menu-d1 has-children"`,
		`href="/deep/" class="wb-menu-d2"`,
		`href="/other/" aria-current="page" class="is-current"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in %s", want, out)
		}
	}
	if i, j := strings.Index(out, "Top"), strings.Index(out, "Other"); !(i < strings.Index(out, "Child") && strings.Index(out, "Child") < strings.Index(out, "Deep") && strings.Index(out, "Deep") < j) {
		t.Errorf("order is not depth first: %s", out)
	}
	// The depth is capped.
	l4 := &store.MenuItem{ID: 9, Label: "TooDeep", URL: "/x/"}
	leaf.Children = []*store.MenuItem{l4}
	if out := wbExec(t, src, &wbSlotResolver{env: env}); strings.Contains(out, "TooDeep") {
		t.Errorf("a fourth level is shown: %s", out)
	}
}

// post.field prints a Pursae Custom Fields value, escaped; post.field-url sets href/src.
func TestWBCustomFieldSlots(t *testing.T) {
	srv, base, admin := wbtfStart(t)
	ctx := context.Background()
	acfgoActivate(t, admin, base)
	if !srv.acfgoOn() {
		t.Fatal("Pursae Custom Fields is off")
	}
	acfgoSaveGroupForm(t, admin, base, 0, acfgoTestGroup())
	p := &store.Post{Type: "post", Title: "W", Status: "publish", AuthorID: 1, Meta: map[string]any{
		"subtitle": "<b>Best</b> widget", "_subtitle": "field_subtitle", "_secret": "x"}}
	if err := srv.store.InsertPost(ctx, p); err != nil {
		t.Fatal(err)
	}
	env := &wbSlotEnv{s: srv, ctx: ctx, v: &siteView{}}
	r := &wbSlotResolver{env: env, post: &wbSlotPost{p: p}}
	got, ok := env.fieldText(p, "subtitle", false)
	if !ok || !strings.Contains(got, "Best") {
		t.Fatalf("fieldText = %q %v", got, ok)
	}
	out := wbExec(t, `<p data-wb-slot="post.field" data-wb-field="subtitle"></p>`, r)
	if strings.Contains(out, "<b>") || !strings.Contains(out, "Best") {
		t.Errorf("field value not printed escaped: %s", out)
	}
	if out := wbExec(t, `<p data-wb-slot="post.field" data-wb-field="nope"></p>`, r); strings.Contains(out, "Best") {
		t.Errorf("unknown field printed: %s", out)
	}
	// Raw meta that is no registered field: an author's markup cannot print it (only a code
	// binding's element, which carries data-wb-meta, may fall back to the post's own meta).
	p.Meta["private_note"] = "hush"
	if out := wbExec(t, `<p data-wb-slot="post.field" data-wb-field="private_note"></p>`, r); strings.Contains(out, "hush") {
		t.Errorf("an author's post.field printed unregistered meta: %s", out)
	}
	if out := wbExec(t, `<a data-wb-slot="post.field-url" data-wb-field="private_note">x</a>`, r); strings.Contains(out, "hush") {
		t.Errorf("an author's post.field-url printed unregistered meta: %s", out)
	}
	// A code binding's element (data-wb-meta) does not make other meta public either: only the
	// names a site import stores (design_<n>, list<n>_<k> and their companions) fall back to the
	// post's meta.
	for _, src := range []string{
		`<p data-wb-slot="post.field" data-wb-field="private_note" data-wb-meta=""></p>`,
		`<p data-wb-slot="post.field-html" data-wb-field="private_note" data-wb-meta=""></p>`,
		`<a data-wb-slot="post.field-url" data-wb-field="private_note" data-wb-meta="">x</a>`,
	} {
		if out := wbExec(t, src, r); strings.Contains(out, "hush") {
			t.Errorf("a code binding printed unregistered meta: %s", out)
		}
	}
	p.Meta["design_1"] = "Imported part"
	p.Meta["design_2_href"] = "https://example.test/next"
	p.Meta["list1_2"] = "https://example.test/card.jpg"
	p.Meta["list1_2_alt"] = "Card"
	if out := wbExec(t, `<p data-wb-slot="post.field" data-wb-field="design_1" data-wb-meta=""></p>`, r); !strings.Contains(out, "Imported part") {
		t.Errorf("a code binding does not read a site import's field: %s", out)
	}
	if out := wbExec(t, `<p data-wb-slot="post.field" data-wb-field="design_1"></p>`, r); strings.Contains(out, "Imported part") {
		t.Errorf("an author's post.field printed unregistered meta: %s", out)
	}
	if out := wbExec(t, `<a data-wb-slot="post.field-attrs" data-wb-field="design_2" data-wb-meta="">x</a>`, r); !strings.Contains(out, `href="https://example.test/next"`) {
		t.Errorf("a site import's attribute field is not printed: %s", out)
	}
	if out := wbExec(t, `<img data-wb-slot="post.image" data-wb-raw="" data-wb-field="list1_2" data-wb-meta=""/>`, r); !strings.Contains(out, `src="https://example.test/card.jpg"`) || !strings.Contains(out, `alt="Card"`) {
		t.Errorf("a site import's image field is not printed: %s", out)
	}
	// The bind tool warns about a custom field name that will print nothing: neither a registered
	// field nor a site import's field.
	// (price is a field of acfgoTestGroup.)
	for name, warn := range map[string]bool{"subtitle": false, "design_1": false, "list1_2_alt": false, "price": false, "private_note": true, "unit_cost": true} {
		for _, field := range []string{"custom", "image"} {
			w := srv.wbBindFieldWarning(ctx, &webbuilder.CodeBind{Kind: webbuilder.BindField, Field: field, Name: name})
			if (w != "") != warn || (warn && !strings.Contains(w, name)) {
				t.Errorf("%s %s: warning %q", field, name, w)
			}
		}
	}
	if w := srv.wbBindFieldWarning(ctx, &webbuilder.CodeBind{Kind: webbuilder.BindField, Field: "title", Name: "price"}); w != "" {
		t.Errorf("a title binding warns: %q", w)
	}
	// Underscore (hidden) meta, an unknown field and a protected post print nothing.
	if _, ok := env.fieldText(p, "_secret", true); ok {
		t.Error("hidden meta is readable")
	}
	p.Password = "pw"
	if _, ok := env.fieldText(p, "subtitle", false); ok {
		t.Error("a password protected post leaks a field")
	}
	if _, ok := (&wbSlotEnv{}).fieldText(p, "subtitle", true); ok {
		t.Error("no server must give nothing")
	}
}

func TestWBTokenSanitize(t *testing.T) {
	in := wbTokens{
		Colors: map[string]string{"primary": "#2271B1", "secondary": "rgb(1, 2, 3)", "accent": "red;}</style><script>", "text": "url(x)", "bogus": "#fff"},
		Fonts:  map[string]string{"heading": "Inter, 'Open Sans', sans-serif", "body": "x;}body{display:none", "other": "Arial"},
	}
	got := wbSanitizeTokens(in)
	if got.Colors["primary"] != "#2271B1" || got.Colors["secondary"] != "rgb(1, 2, 3)" || got.Fonts["heading"] != "Inter, 'Open Sans', sans-serif" {
		t.Errorf("valid tokens lost: %+v", got)
	}
	if got.Colors["accent"] != "" || got.Colors["text"] != "" || got.Colors["bogus"] != "" || got.Fonts["body"] != "" || got.Fonts["other"] != "" {
		t.Errorf("unsafe or unknown tokens kept: %+v", got)
	}
	css := wbTokenCSS(got)
	if !strings.Contains(css, ":root{--wb-color-primary:#2271B1;--wb-color-secondary:rgb(1, 2, 3);--wb-font-heading:Inter, 'Open Sans', sans-serif}") || strings.Contains(css, "<script") {
		t.Errorf("css: %s", css)
	}
	if strings.Contains(wbTokenCSS(wbTokens{}), ":root{") {
		t.Error("an empty token set prints a :root block")
	}
}

func TestWebBuilderRESTTokens(t *testing.T) {
	srv, _, admin := wbrStart(t)
	const path = "/web-builder/v2/site-settings"
	put := map[string]any{"tokens": map[string]any{"colors": map[string]any{"primary": "#112233", "accent": "bad;}"}, "fonts": map[string]any{"body": "Georgia, serif"}}}
	code, v, _ := admin.do(http.MethodPut, path, put, nil)
	tk, _ := wbrObj(v)["tokens"].(map[string]any)
	if code != http.StatusOK || tk == nil {
		t.Fatalf("put: %d %v", code, v)
	}
	if c, _ := tk["colors"].(map[string]any); c["primary"] != "#112233" || c["accent"] != nil {
		t.Errorf("colors: %v", tk["colors"])
	}
	if got := srv.wbSiteTokens(); got.Fonts["body"] != "Georgia, serif" {
		t.Errorf("not stored: %+v", got)
	}
	// A request without tokens leaves them alone; a wrong shape is refused.
	if code, _, _ = admin.do(http.MethodPut, path, map[string]any{"blogname": "Tokens"}, nil); code != http.StatusOK || srv.wbSiteTokens().Colors["primary"] != "#112233" {
		t.Errorf("partial put dropped the tokens: %d", code)
	}
	if code, _, _ = admin.do(http.MethodPut, path, map[string]any{"tokens": "x"}, nil); code != http.StatusBadRequest {
		t.Errorf("bad tokens: %d", code)
	}
}

// GET outdated pages through the whole queue (no 200 cap), by keyset, for the regenerate frame.
func TestWebBuilderRESTOutdated(t *testing.T) {
	srv, base, admin := wbrStart(t)
	ctx := context.Background()
	var ids []int64
	for i := 0; i < 5; i++ {
		p := wbrPost(t, srv, "page", "Old "+strconv.Itoa(i), "publish", 1)
		ids = append(ids, p.ID)
		if _, err := srv.store.Pool.Exec(ctx, `INSERT INTO web_builder_docs (post_id, doc, renderer) VALUES ($1, '{"version":1}', 0)`, p.ID); err != nil {
			t.Fatal(err)
		}
	}
	cur := wbrPost(t, srv, "page", "Current", "publish", 1)
	if _, err := srv.store.Pool.Exec(ctx, `INSERT INTO web_builder_docs (post_id, doc, renderer) VALUES ($1, '{"version":1}', $2)`, cur.ID, webbuilder.RendererVersion); err != nil {
		t.Fatal(err)
	}
	const path = "/web-builder/v2/outdated"
	code, v, _ := admin.do(http.MethodGet, path+"?limit=2", nil, nil)
	o := wbrObj(v)
	items, _ := o["items"].([]any)
	if code != http.StatusOK || o["total"] != float64(5) || len(items) != 2 || wbrObj(items[0])["id"] != float64(ids[0]) {
		t.Fatalf("first batch: %d %v", code, v)
	}
	_, v, _ = admin.do(http.MethodGet, path+"?limit=2&after="+strconv.FormatInt(ids[3], 10), nil, nil)
	if items, _ = wbrObj(v)["items"].([]any); len(items) != 1 || wbrObj(items[0])["id"] != float64(ids[4]) {
		t.Errorf("last batch: %v", v)
	}
	sub := wbrUser(t, srv, base, "sub2", "subscriber")
	if code, _, _ := sub.do(http.MethodGet, path, nil, nil); code != http.StatusForbidden {
		t.Errorf("subscriber: %d", code)
	}
}

// A drop-down menu (data-wb-menu-tree="dropdown") lists the top level; a parent takes the
// "parent" template, whose sub-list holds all its descendants with their level classes.
func TestWBMenuDropdown(t *testing.T) {
	leaf := &store.MenuItem{ID: 3, Label: "Deep", URL: "/deep/"}
	child := &store.MenuItem{ID: 2, Label: "Child", URL: "/child/", Children: []*store.MenuItem{leaf}}
	top := &store.MenuItem{ID: 1, Label: "Top", URL: "/top/", Children: []*store.MenuItem{child}}
	other := &store.MenuItem{ID: 4, Label: "Other", URL: "/other/"}
	env := &wbSlotEnv{v: &siteView{Menus: map[string][]*store.MenuItem{"primary": {top, other}}}}
	src := `<nav><span class="contents" data-wb-menu-loc="primary" data-wb-menu-tree="dropdown">` +
		`<a class="l" data-wb-loop-item="default" data-wb-slot="menu.url"><span data-wb-slot="menu.label"></span></a>` +
		`<span class="wb-menu-group" data-wb-loop-item="parent"><a class="l" data-wb-slot="menu.url"><span data-wb-slot="menu.label"></span></a>` +
		`<span class="wb-menu-sub" data-wb-menu-sub=""><a data-wb-loop-item="" data-wb-slot="menu.url"><span data-wb-slot="menu.label"></span></a></span></span>` +
		`</span></nav>`
	out := wbExec(t, src, &wbSlotResolver{env: env})
	for _, want := range []string{
		`data-wb-loop-item="parent"><a class="l has-children" data-wb-slot="menu.url" href="/top/">`,
		`data-wb-menu-sub=""><a data-wb-loop-item="" data-wb-slot="menu.url" href="/child/" class="wb-menu-d1 has-children">`,
		`href="/deep/" class="wb-menu-d2">`,
		`<a class="l" data-wb-loop-item="default" data-wb-slot="menu.url" href="/other/">`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in %s", want, out)
		}
	}
	if strings.Count(out, "wb-menu-group") != 1 || strings.Count(out, "Other") != 1 {
		t.Errorf("wrong templates: %s", out)
	}
}
