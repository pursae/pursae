package server

import (
	"context"
	"fmt"
	"strings"

	"wpgo/internal/blocks"
	"wpgo/internal/store"
	"wpgo/internal/wp"
)

// WordPress content in GraphQL: read-only root fields for posts, pages, categories, tags, the media
// library, menus and authors, so a front end does not need /wp/v2 next to /graphql. They return
// what any visitor of the site can see (published, not password-protected content, public media,
// published menu items, authors who published something) and are always on while GraphQL is.
//
//	wpPosts / wpPosts_connection / wpPost(id|slug)     wpPages / wpPages_connection / wpPage(id|slug)
//	wpCategories / wpTags                              wpMediaItems / wpMediaItem(id)
//	wpMenus / wpMenu(id|slug|location)                 wpUsers / wpUser(id)

func (g *hgGQLSchema) addWPTypes() {
	str, intT, boolT := gqlNamed("String"), gqlNamed("Int"), gqlNamed("Boolean")
	id := gqlNonNull(gqlNamed("ID"))
	dt := gqlNamed("DateTime")
	f := func(name string, t *hgGQLRef) *hgGQLFieldDef { return &hgGQLFieldDef{name: name, typ: t} }
	g.add(&hgGQLTypeDef{kind: "OBJECT", name: "WpAuthor", desc: "The author of published content", fields: []*hgGQLFieldDef{
		f("id", id), f("username", str), f("name", str)}})
	g.add(&hgGQLTypeDef{kind: "OBJECT", name: "WpTerm", desc: "A category or tag", fields: []*hgGQLFieldDef{
		f("id", id), f("name", str), f("slug", str), f("description", str), f("count", intT), f("parent", intT), f("taxonomy", str), f("link", str)}})
	g.add(&hgGQLTypeDef{kind: "OBJECT", name: "WpMediaItem", desc: "A public file of the media library", fields: []*hgGQLFieldDef{
		f("id", id), f("title", str), f("altText", str), f("caption", str), f("mimeType", str), f("sourceUrl", str), f("width", intT), f("height", intT), f("date", dt)}})
	g.add(&hgGQLTypeDef{kind: "OBJECT", name: "WpContentNode", desc: "A published post or page", fields: []*hgGQLFieldDef{
		f("id", id), f("type", str), f("slug", str), f("status", str), f("title", str), f("excerpt", str), f("content", str), f("link", str),
		f("date", dt), f("modified", dt), f("featuredMedia", intT), f("featuredImage", gqlNamed("WpMediaItem")), f("author", gqlNamed("WpAuthor")),
		f("categories", gqlList(gqlNamed("WpTerm"))), f("tags", gqlList(gqlNamed("WpTerm")))}})
	g.add(&hgGQLTypeDef{kind: "OBJECT", name: "WpContentNodeConnection", fields: []*hgGQLFieldDef{
		f("nodes", gqlNonNull(gqlList(gqlNonNull(gqlNamed("WpContentNode"))))), f("pageInfo", gqlNonNull(gqlNamed("Pagination")))}})
	g.add(&hgGQLTypeDef{kind: "OBJECT", name: "WpMenuItem", desc: "An item of a menu, with its children", fields: []*hgGQLFieldDef{
		f("id", id), f("title", str), f("url", str), f("target", str), f("attrTitle", str), f("description", str), f("classes", gqlList(str)), f("type", str),
		f("object", str), f("objectId", intT), f("parent", intT), f("order", intT), f("children", gqlList(gqlNamed("WpMenuItem")))}})
	g.add(&hgGQLTypeDef{kind: "OBJECT", name: "WpMenu", desc: "A navigation menu", fields: []*hgGQLFieldDef{
		f("id", id), f("name", str), f("slug", str), f("location", str), f("locations", gqlList(str)), f("count", intT), f("items", gqlList(gqlNamed("WpMenuItem")))}})
	_ = boolT
}

func (g *hgGQLSchema) wpQueryFields() []*hgGQLFieldDef {
	str, intT, boolT := gqlNamed("String"), gqlNamed("Int"), gqlNamed("Boolean")
	page := []*hgGQLInput{{name: "page", typ: intT}, {name: "pageSize", typ: intT}}
	contentArgs := append([]*hgGQLInput{{name: "search", typ: str}, {name: "slug", typ: str}, {name: "category", typ: str, desc: "A category slug"},
		{name: "tag", typ: str, desc: "A tag slug"}, {name: "author", typ: gqlNamed("ID")}, {name: "orderBy", typ: str, desc: "date, title or modified"}, {name: "order", typ: str, desc: "ASC or DESC"}}, page...)
	one := []*hgGQLInput{{name: "id", typ: gqlNamed("ID")}, {name: "slug", typ: str}}
	termArgs := append([]*hgGQLInput{{name: "search", typ: str}, {name: "hideEmpty", typ: boolT}}, page...)
	node := gqlNamed("WpContentNode")
	field := func(name, desc string, args []*hgGQLInput, t *hgGQLRef) *hgGQLFieldDef {
		return &hgGQLFieldDef{name: name, desc: desc, args: args, typ: t}
	}
	return []*hgGQLFieldDef{
		field("wpPosts", "Published WordPress posts", contentArgs, gqlNonNull(gqlList(node))),
		field("wpPosts_connection", "Published WordPress posts with pagination info", contentArgs, gqlNamed("WpContentNodeConnection")),
		field("wpPost", "One published post, by id or slug", one, node),
		field("wpPages", "Published WordPress pages", contentArgs, gqlNonNull(gqlList(node))),
		field("wpPages_connection", "Published WordPress pages with pagination info", contentArgs, gqlNamed("WpContentNodeConnection")),
		field("wpPage", "One published page, by id or slug", one, node),
		field("wpCategories", "Post categories", termArgs, gqlNonNull(gqlList(gqlNamed("WpTerm")))),
		field("wpTags", "Post tags", termArgs, gqlNonNull(gqlList(gqlNamed("WpTerm")))),
		field("wpMediaItems", "Public files of the media library", append([]*hgGQLInput{{name: "search", typ: str}, {name: "mimeType", typ: str, desc: "image, video, audio or a full type"}}, page...), gqlNonNull(gqlList(gqlNamed("WpMediaItem")))),
		field("wpMediaItem", "One public media file", []*hgGQLInput{{name: "id", typ: gqlNonNull(gqlNamed("ID"))}}, gqlNamed("WpMediaItem")),
		field("wpMenus", "Navigation menus", nil, gqlNonNull(gqlList(gqlNamed("WpMenu")))),
		field("wpMenu", "A menu by id, slug or theme location, with its published items", []*hgGQLInput{{name: "id", typ: gqlNamed("ID")}, {name: "slug", typ: str}, {name: "location", typ: str}}, gqlNamed("WpMenu")),
		field("wpUsers", "Authors of published content", page, gqlNonNull(gqlList(gqlNamed("WpAuthor")))),
		field("wpUser", "One author of published content", []*hgGQLInput{{name: "id", typ: gqlNonNull(gqlNamed("ID"))}}, gqlNamed("WpAuthor")),
	}
}

// hgGQLWPNames are the root fields above (a content type may not shadow them: the executor tries
// these first).
func hgGQLIsWPRoot(name string) bool {
	switch strings.TrimSuffix(name, "_connection") {
	case "wpPosts", "wpPost", "wpPages", "wpPage", "wpCategories", "wpTags", "wpMediaItems", "wpMediaItem", "wpMenus", "wpMenu", "wpUsers", "wpUser":
		return true
	}
	return false
}

// hgGQLProjectWP projects a plain object by a selection set; every map carries its "__typename".
func hgGQLProjectWP(v any, sel []*hgGQLField) any {
	switch t := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for _, f := range hgGQLFlat(sel) {
			if f.name == "__typename" {
				out[f.key()] = t["__typename"]
				continue
			}
			out[f.key()] = hgGQLProjectWP(t[f.name], f.sel)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = hgGQLProjectWP(x, sel)
		}
		return out
	case []string:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = x
		}
		return out
	}
	return v
}

func hgGQLSelected(sel []*hgGQLField) map[string]bool {
	m := map[string]bool{}
	for _, f := range hgGQLFlat(sel) {
		m[f.name] = true
	}
	return m
}

// wpRoot resolves the WordPress root fields; handled is false for any other field.
func (ex *hgGQLExec) wpRoot(f *hgGQLField) (v any, handled bool, err error) {
	if !hgGQLIsWPRoot(f.name) {
		return nil, false, nil
	}
	if ex.a.Kind == "preview" {
		return nil, true, fmt.Errorf("Forbidden access") // a preview token reads its own entry only
	}
	if err := ex.readBudget().query(); err != nil {
		return nil, true, err
	}
	name, connection := strings.CutSuffix(f.name, "_connection")
	st := ex.s.hgLoad()
	arg := func(k string) string { return strings.TrimSpace(hgGQLString(ex.resolve(f.args[k]))) }
	num := func(k string) int {
		n, _ := hgToInt(ex.resolve(f.args[k]))
		return int(n)
	}
	pg := hgPagination{Page: 1, PageSize: st.DefaultPageSize, WithCount: connection}
	if n := num("page"); n > 1 {
		pg.Page = n
	}
	if n := num("pageSize"); n > 0 {
		pg.PageSize = min(n, st.MaxPageSize)
	}
	if pg.PageSize <= 0 {
		pg.PageSize = 25
	}
	switch name {
	case "wpPosts", "wpPages", "wpPost", "wpPage":
		typ := "post"
		if strings.HasPrefix(name, "wpPage") {
			typ = "page"
		}
		single := name == "wpPost" || name == "wpPage"
		q := store.PostQuery{Types: []string{typ}, Statuses: []string{"publish"}, ExcludePassworded: true, Page: pg.Page, PerPage: pg.PageSize, OrderBy: "date", Order: "DESC"}
		if single {
			q.Page, q.PerPage = 1, 1
			if id, ok := hgToInt(ex.resolve(f.args["id"])); ok && id > 0 {
				q.IDs = []int64{id}
			} else if s := arg("slug"); s != "" {
				q.Slugs = []string{s}
			} else {
				return nil, true, fmt.Errorf("The argument id or slug is required")
			}
		} else {
			q.Search = arg("search")
			if s := arg("slug"); s != "" {
				q.Slugs = []string{s}
			}
			switch strings.ToLower(arg("orderBy")) {
			case "title", "modified", "date":
				q.OrderBy = strings.ToLower(arg("orderBy"))
			case "":
			default:
				return nil, true, fmt.Errorf("orderBy must be date, title or modified")
			}
			if o := strings.ToUpper(arg("order")); o == "ASC" || o == "DESC" {
				q.Order = o
			}
			if id, ok := hgToInt(ex.resolve(f.args["author"])); ok && id > 0 {
				q.AuthorID = id
			}
			for tax, key := range map[string]string{"category": "category", "post_tag": "tag"} {
				if slug := arg(key); slug != "" {
					t, err := ex.s.store.TermBySlug(ex.c, tax, slug)
					if err != nil || t == nil {
						q.IDs = []int64{0} // an unknown term matches nothing
						continue
					}
					q.TermClauses = append(q.TermClauses, store.TermClause{IDs: []int64{t.ID}})
				}
			}
		}
		sel := f.sel
		if connection {
			for _, x := range hgGQLFlat(f.sel) {
				if x.name == "nodes" {
					sel = x.sel
				}
			}
		}
		// Memberships "hide completely": a non-member neither lists nor fetches restricted entries,
		// as on /wp/v2/posts (the caller is the token's WP user, anonymous otherwise).
		var member *store.User
		if ex.a != nil && ex.a.Kind == "user" && ex.a.UserID > 0 {
			member, _ = ex.s.store.UserByID(ex.c, ex.a.UserID)
		}
		q.ExcludeID = append(q.ExcludeID, ex.s.gooListingExclude(withPluginUser(ex.c, member), "post")...)
		posts, total, err := ex.s.store.QueryPosts(ex.c, q)
		if err != nil {
			return nil, true, err
		}
		if err := ex.readBudget().objects(len(posts)); err != nil {
			return nil, true, err
		}
		nodes := ex.wpNodes(posts, sel)
		if single {
			if len(nodes) == 0 {
				return nil, true, nil
			}
			return hgGQLProjectWP(nodes[0], f.sel), true, nil
		}
		items := make([]any, len(nodes))
		for i, n := range nodes {
			items[i] = hgGQLProjectWP(n, sel)
		}
		if !connection {
			return items, true, nil
		}
		out := map[string]any{}
		for _, x := range hgGQLFlat(f.sel) {
			switch x.name {
			case "nodes":
				out[x.key()] = items
			case "pageInfo":
				m := pg.meta(total)
				pi := map[string]any{}
				for _, y := range hgGQLFlat(x.sel) {
					pi[y.key()] = m[y.name]
				}
				out[x.key()] = pi
			case "__typename":
				out[x.key()] = "WpContentNodeConnection"
			}
		}
		return out, true, nil
	case "wpCategories", "wpTags":
		tax := "category"
		if name == "wpTags" {
			tax = "post_tag"
		}
		terms, _, err := ex.s.store.QueryTerms(ex.c, store.TermQuery{Taxonomy: tax, Search: arg("search"), HideEmpty: hgTruthy(ex.resolve(f.args["hideEmpty"])), Page: pg.Page, PerPage: pg.PageSize})
		if err != nil {
			return nil, true, err
		}
		out := make([]any, len(terms))
		for i, t := range terms {
			out[i] = ex.wpTerm(t)
		}
		return hgGQLProjectWP(out, f.sel), true, nil
	case "wpMediaItems", "wpMediaItem":
		q := store.AttachmentQuery{PublicParentsOnly: true, Page: pg.Page, PerPage: pg.PageSize, OrderBy: "date", Order: "DESC"}
		if name == "wpMediaItem" {
			id, ok := hgToInt(ex.resolve(f.args["id"]))
			if !ok || id <= 0 {
				return nil, true, fmt.Errorf("The argument id is required")
			}
			q.IDs, q.Page, q.PerPage = []int64{id}, 1, 1
		} else {
			q.Search = arg("search")
			if m := arg("mimeType"); m != "" {
				q.MimePrefix = []string{m}
			}
		}
		list, _, err := ex.s.store.QueryAttachments(ex.c, q)
		if err != nil {
			return nil, true, err
		}
		out := []any{}
		viewer := ex.s.hgWPUser(ex.c, ex.a)
		for _, a := range list {
			if a.PostID != 0 && ex.s.gmemHidesPost(ex.c, viewer, a.PostID) { // Memberships-restricted post's file
				continue
			}
			out = append(out, ex.wpMedia(a))
		}
		if name == "wpMediaItem" {
			if len(out) == 0 {
				return nil, true, nil
			}
			return hgGQLProjectWP(out[0], f.sel), true, nil
		}
		return hgGQLProjectWP(out, f.sel), true, nil
	case "wpMenus", "wpMenu":
		menus, err := ex.s.store.Menus(ex.c)
		if err != nil {
			return nil, true, err
		}
		if name == "wpMenus" {
			sel := hgGQLSelected(f.sel)
			out := []any{}
			for _, m := range menus {
				var full map[string]any
				if sel["items"] {
					if full, err = ex.s.hgMenuFull(ex.c, menus, itoa64(m.ID)); err != nil {
						return nil, true, err
					}
				}
				if full == nil {
					full = hgMenuBrief(m)
				}
				out = append(out, wpTyped(full, "WpMenu"))
			}
			return hgGQLProjectWP(out, f.sel), true, nil
		}
		key := arg("id")
		if key == "" {
			key = arg("slug")
		}
		if key == "" {
			key = arg("location")
		}
		if key == "" {
			return nil, true, fmt.Errorf("The argument id, slug or location is required")
		}
		m, err := ex.s.hgMenuFull(ex.c, menus, key)
		if err != nil || m == nil {
			return nil, true, err
		}
		return hgGQLProjectWP(wpTyped(m, "WpMenu"), f.sel), true, nil
	case "wpUsers", "wpUser":
		ids, err := ex.wpAuthorIDs(name == "wpUser", pg, ex.resolve(f.args["id"]))
		if err != nil {
			return nil, true, err
		}
		out := []any{}
		for _, id := range ids {
			if u, err := ex.s.store.UserByID(ex.c, id); err == nil && u != nil {
				out = append(out, map[string]any{"__typename": "WpAuthor", "id": u.ID, "username": u.Nicename, "name": u.DisplayName})
			}
		}
		if name == "wpUser" {
			if len(out) == 0 {
				return nil, true, nil
			}
			return hgGQLProjectWP(out[0], f.sel), true, nil
		}
		return hgGQLProjectWP(out, f.sel), true, nil
	}
	return nil, false, nil
}

// wpTyped sets __typename on a menu and, recursively, on its items.
func wpTyped(m map[string]any, typ string) map[string]any {
	m["__typename"] = typ
	if items, ok := m["items"].([]any); ok {
		for _, it := range items {
			wpTypedItem(it)
		}
	}
	return m
}

func wpTypedItem(v any) {
	if it, ok := v.(map[string]any); ok {
		it["__typename"] = "WpMenuItem"
		if ch, ok := it["children"].([]any); ok {
			for _, c := range ch {
				wpTypedItem(c)
			}
		}
	}
}

// wpAuthorIDs are the users who published a post or page (the ones WordPress' own REST API lists).
// hgHidesUsers reports whether accounts are hidden from the caller: an anonymous caller (the public
// role or a preview token) while user enumeration is blocked. Such a caller sees (or links) wp:user
// relations only to authors of published content, the same accounts wpUsers lists (wpAuthorIDs).
func (s *Server) hgHidesUsers(a *hgAuth) bool {
	return a != nil && (a.Kind == "public" || a.Kind == "preview") && s.blockEnumeration()
}

// hgUserPublished reports whether the user is the author of published content (a public author,
// whose name the site shows anyway).
func (s *Server) hgUserPublished(ctx context.Context, id int64) bool {
	var ok bool
	_ = s.store.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM posts WHERE author_id = $1 AND status = 'publish')`, id).Scan(&ok)
	return ok
}

func (ex *hgGQLExec) wpAuthorIDs(one bool, pg hgPagination, id any) ([]int64, error) {
	q := `SELECT DISTINCT author_id FROM posts WHERE status = 'publish' AND type IN ('post','page') AND author_id IS NOT NULL`
	args := []any{}
	if one {
		n, ok := hgToInt(id)
		if !ok || n <= 0 {
			return nil, fmt.Errorf("The argument id is required")
		}
		q += ` AND author_id = $1`
		args = append(args, n)
	}
	q += ` ORDER BY author_id LIMIT ` + itoa64(int64(pg.PageSize)) + ` OFFSET ` + itoa64(int64((pg.Page-1)*pg.PageSize))
	rows, err := ex.s.store.Pool.Query(ex.c, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var x int64
		if err := rows.Scan(&x); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func (ex *hgGQLExec) wpTerm(t *store.Term) map[string]any {
	return map[string]any{"__typename": "WpTerm", "id": t.ID, "name": t.Name, "slug": t.Slug, "description": t.Description, "count": t.Count,
		"parent": t.ParentID, "taxonomy": t.Taxonomy, "link": ex.s.links(ex.c).TermLink(t)}
}

func (ex *hgGQLExec) wpMedia(a *store.Attachment) map[string]any {
	var w, h any
	if a.Width > 0 {
		w, h = a.Width, a.Height
	}
	return map[string]any{"__typename": "WpMediaItem", "id": a.ID, "title": a.Title, "altText": hgNilEmpty(a.Alt), "caption": hgNilEmpty(a.Caption),
		"mimeType": a.Mime, "sourceUrl": ex.s.uploadURL(ex.c, a.FilePath), "width": w, "height": h, "date": hgTime(a.CreatedAt)}
}

// wpNodes serializes posts; the costly parts (rendered content, terms, authors, media) only when selected.
func (ex *hgGQLExec) wpNodes(posts []*store.Post, sel []*hgGQLField) []map[string]any {
	want := hgGQLSelected(sel)
	lb := ex.s.links(ex.c)
	ids := make([]int64, len(posts))
	for i, p := range posts {
		ids[i] = p.ID
	}
	var cats, tags map[int64][]*store.Term
	if want["categories"] {
		cats, _ = ex.s.store.TermsForPosts(ex.c, ids, "category")
	}
	if want["tags"] {
		tags, _ = ex.s.store.TermsForPosts(ex.c, ids, "post_tag")
	}
	authors := map[int64]any{}
	media := map[int64]any{}
	termList := func(ts []*store.Term) []any {
		out := []any{}
		for _, t := range ts {
			out = append(out, ex.wpTerm(t))
		}
		return out
	}
	// The caller's WP user for Memberships checks: a signed-in front-end user, nil for tokens and anonymous callers.
	var caller *store.User
	if ex.a != nil && ex.a.Kind == "user" && ex.a.UserID > 0 {
		caller, _ = ex.s.store.UserByID(ex.c, ex.a.UserID)
	}
	out := make([]map[string]any, len(posts))
	for i, p := range posts {
		n := map[string]any{"__typename": "WpContentNode", "id": p.ID, "type": p.Type, "slug": p.Slug, "status": p.Status, "title": p.Title,
			"link": lb.Permalink(p), "date": hgTime(p.Date()), "modified": hgTime(p.ModifiedAt), "featuredMedia": p.FeaturedMediaID}
		if want["excerpt"] || want["content"] {
			html := ""
			if want["content"] {
				rc := &renderCtx{s: ex.s, lb: lb, user: caller, postID: p.ID}
				html = blocks.Render(withRender(ex.c, rc), p.Content)
				if !blocks.HasBlocks(p.Content) {
					html = wp.Autop(p.Content)
				}
			}
			// Goocommerce Memberships: non-members get the restriction message (and no
			// excerpt when show_excerpts is off), exactly as core REST content.rendered.
			html, excerpt := ex.s.gooRESTRestrict(ex.c, caller, p, html, wp.KsesPost(p.Excerpt))
			if want["excerpt"] {
				n["excerpt"] = excerpt
			}
			if want["content"] {
				n["content"] = html
			}
		}
		if want["author"] && p.AuthorID > 0 {
			if _, ok := authors[p.AuthorID]; !ok {
				authors[p.AuthorID] = nil
				if u, err := ex.s.store.UserByID(ex.c, p.AuthorID); err == nil && u != nil {
					authors[p.AuthorID] = map[string]any{"__typename": "WpAuthor", "id": u.ID, "username": u.Nicename, "name": u.DisplayName}
				}
			}
			n["author"] = authors[p.AuthorID]
		}
		if want["featuredImage"] && p.FeaturedMediaID > 0 {
			if _, ok := media[p.FeaturedMediaID]; !ok {
				media[p.FeaturedMediaID] = nil
				if a, err := ex.s.store.GetAttachment(ex.c, p.FeaturedMediaID); err == nil && a != nil {
					media[p.FeaturedMediaID] = ex.wpMedia(a)
				}
			}
			n["featuredImage"] = media[p.FeaturedMediaID]
		}
		if want["categories"] {
			n["categories"] = termList(cats[p.ID])
		}
		if want["tags"] {
			n["tags"] = termList(tags[p.ID])
		}
		out[i] = n
	}
	return out
}
