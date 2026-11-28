package server

import (
	"context"
	"fmt"
	"html"
	"html/template"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"wpgo/internal/links"
	"wpgo/internal/store"
)

// nav-menus.php with WordPress's own nav-menu.js: the markup below mirrors Walker_Nav_Menu_Edit and
// Walker_Nav_Menu_Checklist so drag-and-drop nesting, move links and live labels work unchanged.

type menuEditRow struct {
	Item       *store.EditItem
	Depth      int
	Title      string
	TypeLabel  string
	Original   string
	OrigURL    string
	ParentID   int64
	Pending    bool
	DeleteURL  string
	CancelURL  string
	IsCustom   bool
	TargetOpen bool
	ACF        template.HTML // Pursae Custom Fields: the item's fields (acfgo_menus.go)
}

type menuChoice struct {
	Value, Label string
	Selected     bool
}

type locationRow struct {
	Slug, Name  string
	Assigned    bool
	CurrentName string
	MenuID      int64
	Options     []menuChoice
}

type checklistItem struct {
	ObjectID int64
	Object   string
	Type     string
	Title    string
	URL      string
}

type navMenusData struct {
	Menus        []*store.Menu
	Selected     *store.Menu
	IsNew        bool
	Rows         []menuEditRow
	Locations    []locationRow
	Tab          string
	ColumnNonce  string
	UpdateNonce  string
	DeleteURL    string
	RecentPages  []checklistItem
	AllPages     []checklistItem
	RecentPosts  []checklistItem
	Categories   []checklistItem
	Tags         []checklistItem
	NoLocations  bool
	LocationsNon string
	MenuCount    int  // number of menus (manage-menus message variant)
	AddNew       bool // ?menu=0: the "create a new menu" screen
	Disabled     bool // no menu selected: wp_nav_menu_disabled_check() disables the left column

	// ACFMenu are the menu's own Pursae Custom Fields fields, under Menu Settings (acfgo_menus.go).
	ACFMenu template.HTML

	// PluginBoxes are "Add menu items" boxes of native plugins (Golylang's Language switcher).
	PluginBoxes template.HTML
}

func (s *Server) navMenus(c *gin.Context) {
	u := currentUser(c)
	if !u.Can("edit_theme_options", nil) {
		c.String(http.StatusForbidden, "Sorry, you are not allowed to edit theme options on this site.")
		return
	}
	ctx := c.Request.Context()
	if c.Request.Method == http.MethodPost {
		s.navMenusSave(c)
		return
	}
	if c.Query("action") == "delete" {
		id, _ := strconv.ParseInt(c.Query("menu"), 10, 64)
		if !s.checkNonce(c, "delete-nav_menu-"+itoa64(id)) {
			s.denyNonce(c)
			return
		}
		_ = s.store.DeleteMenu(ctx, id)
		s.purgeAll(c)
		c.Redirect(http.StatusFound, "/wp-admin/nav-menus?deleted=1")
		return
	}
	// Drafts abandoned more than a day ago (added, menu never saved) are dropped instead of coming back
	// on the screen and being published by whoever saves next (_wp_delete_orphaned_draft_menu_items).
	_ = s.store.DeleteOrphanedDraftMenuItems(ctx)
	menus, _ := s.store.Menus(ctx)
	d := navMenusData{Menus: menus, Tab: "edit", ColumnNonce: s.nonce(c, "add-menu_item"), UpdateNonce: s.nonce(c, "update-nav_menu"),
		LocationsNon: s.nonce(c, "save-menu-locations")}
	if c.Query("action") == "locations" {
		d.Tab = "locations"
	}
	theme := s.activeTheme()
	d.NoLocations = len(theme.MenuLocations) == 0

	sel, _ := strconv.ParseInt(c.Query("menu"), 10, 64)
	_, hasMenuParam := c.GetQuery("menu")
	d.MenuCount = len(menus)
	d.AddNew = hasMenuParam && sel == 0
	if len(menus) == 0 && d.Tab == "edit" {
		// nav-menus.php redirects to the add-new screen (?action=edit&menu=0) when there are no menus;
		// render that screen in place.
		d.AddNew = true
	}
	if !hasMenuParam || sel != 0 {
		if sel == 0 {
			if r, ok := u.Meta["nav_menu_recently_edited"].(float64); ok {
				sel = int64(r)
			}
		}
		for _, m := range menus {
			if m.ID == sel {
				d.Selected = m
			}
		}
		if d.Selected == nil && len(menus) > 0 {
			d.Selected = menus[0]
		}
	}
	d.IsNew = d.Selected == nil
	d.Disabled = d.IsNew
	if d.Selected != nil {
		_ = s.setUserMeta(c, u, "nav_menu_recently_edited", d.Selected.ID)
		d.DeleteURL = "/wp-admin/nav-menus?action=delete&menu=" + itoa64(d.Selected.ID) + "&_wpnonce=" + s.nonce(c, "delete-nav_menu-"+itoa64(d.Selected.ID))
		items, _ := s.store.AllMenuItems(ctx, d.Selected.ID) // pending (draft) items too, like wp_get_nav_menu_to_edit()
		d.Rows = s.menuEditRows(ctx, items)
		s.acfgoNavMenus(c, &d) // Pursae Custom Fields: Menu and Menu Item location rules
	}
	for _, loc := range theme.MenuLocations {
		row := locationRow{Slug: loc.Slug, Name: loc.Name}
		row.Options = append(row.Options, menuChoice{Value: "0", Label: "&mdash; Select a Menu &mdash;"})
		for _, m := range menus {
			if m.HasLocation(loc.Slug) {
				row.MenuID, row.CurrentName = m.ID, m.Name
				row.Assigned = d.Selected != nil && m.ID == d.Selected.ID
			}
		}
		for _, m := range menus {
			row.Options = append(row.Options, menuChoice{Value: itoa64(m.ID), Label: template.HTMLEscapeString(m.Name), Selected: m.ID == row.MenuID})
		}
		d.Locations = append(d.Locations, row)
	}
	s.golylangNavMenus(c, &d, menus) // Golylang: a location per language, the Language switcher box
	lb := s.links(c)
	pages, _, _ := s.store.QueryPosts(ctx, store.PostQuery{Types: []string{"page"}, Statuses: []string{"publish"}, PerPage: 50, Page: 1, OrderBy: "date", Order: "DESC"})
	for i, p := range pages {
		it := checklistItem{ObjectID: p.ID, Object: "page", Type: "post_type", Title: defaultStr(p.Title, "(no title)"), URL: lb.Permalink(p)}
		if i < 15 {
			d.RecentPages = append(d.RecentPages, it)
		}
		d.AllPages = append(d.AllPages, it)
	}
	sort.Slice(d.AllPages, func(i, j int) bool {
		return strings.ToLower(d.AllPages[i].Title) < strings.ToLower(d.AllPages[j].Title)
	})
	posts, _, _ := s.store.QueryPosts(ctx, store.PostQuery{Types: []string{"post"}, Statuses: []string{"publish"}, PerPage: 15, Page: 1, OrderBy: "date", Order: "DESC"})
	for _, p := range posts {
		d.RecentPosts = append(d.RecentPosts, checklistItem{ObjectID: p.ID, Object: "post", Type: "post_type", Title: defaultStr(p.Title, "(no title)"), URL: lb.Permalink(p)})
	}
	for _, tax := range []string{"category", "post_tag"} {
		terms, _, _ := s.store.QueryTerms(ctx, store.TermQuery{Taxonomy: tax, PerPage: 50, OrderBy: "count", Order: "DESC"})
		for _, t := range terms {
			it := checklistItem{ObjectID: t.ID, Object: tax, Type: "taxonomy", Title: t.Name, URL: lb.TermLink(t)}
			if tax == "category" {
				d.Categories = append(d.Categories, it)
			} else {
				d.Tags = append(d.Tags, it)
			}
		}
	}

	a := s.pageAssets(c)
	a.Script("nav-menu", "accordion")
	a.AddBefore("nav-menu", `var menus = {"oneThemeLocationNoMenus":false,"moveUp":"Move up one","moveDown":"Move down one","moveToTop":"Move to the top","moveUnder":"Move under %s","moveOutFrom":"Move out from under %s","under":"Under %s","outFrom":"Out from under %s","menuFocus":"Edit %1$s (%2$s, %3$d of %4$d)","subMenuFocus":"Edit %1$s (%2$s, sub-item %3$d of %4$d under %5$s)","subMenuMoreDepthFocus":"Edit %1$s (%2$s, sub-item %3$d of %4$d under %5$s, level %6$d)","menuItemDeletion":"item %s","itemsDeleted":"Deleted menu item: %s.","itemAdded":"Menu item added","itemRemoved":"Menu item removed","movedUp":"Menu item moved up","movedDown":"Menu item moved down","movedTop":"Menu item moved to the top","movedLeft":"Menu item moved out of submenu","movedRight":"Menu item is now a sub-item","parentUpdated":"Menu parent updated","orderUpdated":"Menu order updated"};`)

	var notices []notice
	switch {
	case c.Query("updated") != "":
		notices = append(notices, notice{Type: "success", Message: template.HTML("<strong>" + template.HTMLEscapeString(c.Query("updated")) + "</strong> has been updated."), Dismissible: true})
	case c.Query("created") != "":
		notices = append(notices, notice{Type: "success", Message: template.HTML("<strong>" + template.HTMLEscapeString(c.Query("created")) + "</strong> has been created."), Dismissible: true})
	case c.Query("deleted") == "1":
		notices = append(notices, notice{Type: "success", Message: "The menu has been successfully deleted.", Dismissible: true})
	case c.Query("locations-updated") == "1":
		notices = append(notices, notice{Type: "success", Message: "Menu locations updated.", Dismissible: true})
	}
	s.adminPage(c, "nav-menus", "Menus", "nav-menus-php", "admin/nav-menus", d, notices...)
}

// menuEditRows orders items depth-first and labels them like Walker_Nav_Menu_Edit.
func (s *Server) menuEditRows(ctx context.Context, items []*store.EditItem) []menuEditRow {
	children := map[int64][]*store.EditItem{}
	ids := map[int64]bool{}
	for _, it := range items {
		ids[it.ID] = true
	}
	for _, it := range items {
		p := it.ParentID
		if !ids[p] {
			p = 0
		}
		children[p] = append(children[p], it)
	}
	var out []menuEditRow
	var walk func(parent int64, depth int)
	walk = func(parent int64, depth int) {
		for _, it := range children[parent] {
			out = append(out, s.menuEditRow(ctx, it, depth, parent))
			walk(it.ID, depth+1)
		}
	}
	walk(0, 0)
	for i := range out {
		out[i].Item.Order = i + 1
	}
	return out
}

func (s *Server) menuEditRow(ctx context.Context, it *store.EditItem, depth int, parent int64) menuEditRow {
	r := menuEditRow{Item: it, Depth: depth, ParentID: parent, Pending: it.Status == "draft", IsCustom: it.Kind == "custom", TargetOpen: it.Target == "_blank"}
	orig, url := s.menuItemOriginal(ctx, it)
	r.Original, r.OrigURL = orig, url
	r.Title = it.Label
	if r.Title == "" {
		r.Title = orig
	}
	if r.Title == "" {
		r.Title = "(no label)"
	}
	switch it.Kind {
	case "custom":
		r.TypeLabel = "Custom Link"
	case "taxonomy":
		r.TypeLabel = map[string]string{"category": "Category", "post_tag": "Tag"}[it.Object]
	default:
		r.TypeLabel = map[string]string{"page": "Page", "post": "Post"}[it.Object]
		if s.settings.Int("page_on_front") == int(it.TargetID) && it.Object == "page" {
			r.TypeLabel = "Front Page"
		}
	}
	return r
}

// menuItemOriginal returns the title and URL of the object a menu item points to.
func (s *Server) menuItemOriginal(ctx context.Context, it *store.EditItem) (string, string) {
	lb := s.links(ctx)
	switch it.Kind {
	case "post_type":
		if p, err := s.store.GetPost(ctx, it.TargetID); err == nil {
			return defaultStr(p.Title, "(no title)"), lb.Permalink(p)
		}
	case "taxonomy":
		if t, err := s.store.GetTerm(ctx, it.TargetID); err == nil {
			return t.Name, lb.TermLink(t)
		}
	}
	return "", it.URL
}

var menuFieldRe = regexp.MustCompile(`^([a-z-]+)\[(-?\d+)\]$`)
var menuAddRe = regexp.MustCompile(`^menu-item\[(-?\d+)\]\[([a-z-]+)\]$`)

// navMenusSave handles action=update (create a menu, or save its name, items, settings and locations)
// and action=locations (Manage Locations tab).
func (s *Server) navMenusSave(c *gin.Context) {
	ctx := c.Request.Context()
	_ = c.Request.ParseMultipartForm(32 << 20)
	if c.PostForm("action") == "locations" {
		if !s.checkNonce(c, "save-menu-locations") {
			s.denyNonce(c)
			return
		}
		locs := map[string]int64{}
		for _, loc := range s.activeTheme().MenuLocations {
			id, _ := strconv.ParseInt(c.PostForm("menu-locations["+loc.Slug+"]"), 10, 64)
			locs[loc.Slug] = id
		}
		_ = s.store.SetMenuLocations(ctx, locs)
		s.golylangSaveMenuLocations(c) // Golylang: the locations of the other languages
		s.purgeAll(c)
		c.Redirect(http.StatusFound, "/wp-admin/nav-menus?action=locations&locations-updated=1")
		return
	}
	if !s.checkNonceValue(c, c.PostForm("update-nav-menu-nonce"), "update-nav_menu") {
		s.denyNonce(c)
		return
	}
	if !s.acfgoNavMenusValidate(c) { // Pursae Custom Fields: invalid field input saves nothing
		return
	}
	name := strings.TrimSpace(c.PostForm("menu-name"))
	if name == "" {
		name = "Menu"
	}
	id, _ := strconv.ParseInt(c.PostForm("menu"), 10, 64)
	created := false
	if id == 0 {
		nid, err := s.store.CreateMenu(ctx, name)
		if err != nil {
			c.String(http.StatusInternalServerError, err.Error())
			return
		}
		id, created = nid, true
	} else {
		_ = s.store.UpdateMenu(ctx, id, name, c.PostForm("auto-add-pages") == "1")
	}
	form := c.Request.PostForm
	fields := map[int64]map[string]string{}
	for key, vals := range form {
		m := menuFieldRe.FindStringSubmatch(key)
		if m == nil || len(vals) == 0 {
			continue
		}
		iid, _ := strconv.ParseInt(m[2], 10, 64)
		if fields[iid] == nil {
			fields[iid] = map[string]string{}
		}
		fields[iid][m[1]] = vals[0]
	}
	var items []*store.EditItem
	for iid, f := range fields {
		if f["menu-item-db-id"] == "" {
			continue
		}
		it, err := s.store.MenuItem(ctx, iid)
		if err != nil {
			continue
		}
		it.ParentID, _ = strconv.ParseInt(f["menu-item-parent-id"], 10, 64)
		it.Order, _ = strconv.Atoi(f["menu-item-position"])
		it.Label = f["menu-item-title"]
		if it.Kind == "custom" {
			it.URL = strings.TrimSpace(f["menu-item-url"])
		}
		it.Target = f["menu-item-target"]
		it.Classes = strings.Join(strings.Fields(f["menu-item-classes"]), " ")
		it.AttrTitle, it.Description, it.XFN = f["menu-item-attr-title"], f["menu-item-description"], f["menu-item-xfn"]
		items = append(items, it)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Order < items[j].Order })
	if !created {
		if err := s.store.SaveMenuItems(ctx, id, items); err != nil {
			c.String(http.StatusInternalServerError, err.Error())
			return
		}
		s.acfgoNavMenusSave(c, id) // Pursae Custom Fields
	}
	// Display locations: checked boxes assign this menu; unchecked ones that pointed here are cleared.
	locs := map[string]int64{}
	menus, _ := s.store.Menus(ctx)
	for _, loc := range s.activeTheme().MenuLocations {
		if form.Get("menu-locations["+loc.Slug+"]") != "" {
			locs[loc.Slug] = id
			continue
		}
		for _, m := range menus {
			if m.ID == id && m.HasLocation(loc.Slug) {
				locs[loc.Slug] = 0
			}
		}
	}
	_ = s.store.SetMenuLocations(ctx, locs)
	s.purgeAll(c)
	key := "updated"
	if created {
		key = "created"
	}
	c.Redirect(http.StatusFound, "/wp-admin/nav-menus?menu="+itoa64(id)+"&"+key+"="+url.QueryEscape(name))
}

// menuAjax answers the nav-menu.js admin-ajax actions.
func (s *Server) menuAjax(c *gin.Context, action string) bool {
	switch action {
	case "add-menu-item", "menu-quick-search", "menu-locations-save":
	default:
		return false
	}
	u := currentUser(c)
	if !u.Can("edit_theme_options", nil) {
		c.String(http.StatusForbidden, "-1")
		return true
	}
	ctx := c.Request.Context()
	switch action {
	case "add-menu-item":
		if !s.checkNonceValue(c, c.PostForm("menu-settings-column-nonce"), "add-menu_item") {
			c.String(http.StatusForbidden, "-1")
			return true
		}
		menuID, _ := strconv.ParseInt(c.PostForm("menu"), 10, 64)
		_ = c.Request.ParseForm()
		posted := map[string]map[string]string{}
		var order []string
		for key, vals := range c.Request.PostForm {
			m := menuAddRe.FindStringSubmatch(key)
			if m == nil || len(vals) == 0 {
				continue
			}
			if posted[m[1]] == nil {
				posted[m[1]] = map[string]string{}
				order = append(order, m[1])
			}
			posted[m[1]][m[2]] = vals[0]
		}
		sort.Slice(order, func(i, j int) bool {
			a, _ := strconv.Atoi(order[i])
			b, _ := strconv.Atoi(order[j])
			return a > b // checklist ids are negative and count down in page order
		})
		var sb strings.Builder
		for _, k := range order {
			f := posted[k]
			it := &store.EditItem{MenuID: menuID, Kind: defaultStr(f["menu-item-type"], "custom"), Object: f["menu-item-object"],
				URL: strings.TrimSpace(f["menu-item-url"]), Status: "draft"}
			it.TargetID, _ = strconv.ParseInt(f["menu-item-object-id"], 10, 64)
			if it.Kind == "custom" {
				it.Object = "custom"
				it.Label = f["menu-item-title"]
				if it.Label == "" {
					it.Label = "Menu Item"
				}
			}
			if menuID == 0 {
				continue
			}
			if err := s.store.AddMenuItem(ctx, it); err != nil {
				continue
			}
			row := s.menuEditRow(ctx, it, 0, 0)
			row.Pending = false
			row.ACF = s.acfgoNewMenuItemFields(c, menuID, it) // Pursae Custom Fields
			if it.Label == "" {
				it.Label = row.Original
			}
			sb.WriteString(string(s.renderMenuEditItem(c, row)))
		}
		s.adminHTML(c, http.StatusOK, sb.String())
	case "menu-quick-search":
		q := strings.TrimSpace(c.Query("q"))
		typ := c.Query("type") // quick-search-posttype-page etc.
		var sb strings.Builder
		if q != "" {
			lb := s.links(c)
			t := "page"
			if strings.HasSuffix(typ, "post") {
				t = "post"
			}
			posts, _, _ := s.store.QueryPosts(ctx, store.PostQuery{Types: []string{t}, Statuses: []string{"publish"}, Search: q, OrderBy: "relevance", PerPage: 10, Page: 1})
			for i, p := range posts {
				sb.WriteString(string(checklistHTML(checklistItem{ObjectID: p.ID, Object: t, Type: "post_type", Title: defaultStr(p.Title, "(no title)"), URL: lb.Permalink(p)}, -1000-i)))
			}
			if len(posts) == 0 {
				sb.WriteString(`<li><p>No results found.</p></li>`)
			}
		}
		s.adminHTML(c, http.StatusOK, sb.String())
	case "menu-locations-save":
		// wp_ajax_menu_locations_save(): check_ajax_referer( 'add-menu_item', 'menu-settings-column-nonce' ).
		if !s.checkNonceValue(c, c.PostForm("menu-settings-column-nonce"), "add-menu_item") {
			c.String(http.StatusForbidden, "-1")
			return true
		}
		locs := map[string]int64{}
		for _, loc := range s.activeTheme().MenuLocations {
			if v, ok := c.GetPostForm("menu-locations[" + loc.Slug + "]"); ok {
				id, _ := strconv.ParseInt(v, 10, 64)
				locs[loc.Slug] = id
			}
		}
		_ = s.store.SetMenuLocations(ctx, locs)
		s.purgeAll(c)
		c.String(http.StatusOK, "1")
	}
	return true
}

// checklistHTML is Walker_Nav_Menu_Checklist::start_el().
func checklistHTML(it checklistItem, idx int, disabled ...bool) template.HTML {
	e := html.EscapeString
	n := strconv.Itoa(idx)
	dis := ""
	if len(disabled) > 0 && disabled[0] {
		dis = `disabled='disabled' `
	}
	return template.HTML(fmt.Sprintf(`<li><label class="menu-item-title"><input type="checkbox" %sclass="menu-item-checkbox" name="menu-item[%s][menu-item-object-id]" value="%d" /> %s</label><input type="hidden" class="menu-item-db-id" name="menu-item[%s][menu-item-db-id]" value="0" /><input type="hidden" class="menu-item-object" name="menu-item[%s][menu-item-object]" value="%s" /><input type="hidden" class="menu-item-parent-id" name="menu-item[%s][menu-item-parent-id]" value="0" /><input type="hidden" class="menu-item-type" name="menu-item[%s][menu-item-type]" value="%s" /><input type="hidden" class="menu-item-title" name="menu-item[%s][menu-item-title]" value="%s" /><input type="hidden" class="menu-item-url" name="menu-item[%s][menu-item-url]" value="%s" /><input type="hidden" class="menu-item-target" name="menu-item[%s][menu-item-target]" value="" /><input type="hidden" class="menu-item-attr-title" name="menu-item[%s][menu-item-attr-title]" value="" /><input type="hidden" class="menu-item-classes" name="menu-item[%s][menu-item-classes]" value="" /><input type="hidden" class="menu-item-xfn" name="menu-item[%s][menu-item-xfn]" value="" /></li>`,
		dis, n, it.ObjectID, e(it.Title), n, n, e(it.Object), n, n, e(it.Type), n, e(it.Title), n, e(it.URL), n, n, n, n))
}

// renderMenuEditItem renders one item with the admin template (also used by the add-menu-item ajax call).
func (s *Server) renderMenuEditItem(c *gin.Context, r menuEditRow) template.HTML {
	var sb strings.Builder
	set := s.tmpl.get(false)
	if err := set.ExecuteTemplate(&sb, "menu-edit-item", r); err != nil {
		return template.HTML("<!-- " + html.EscapeString(err.Error()) + " -->")
	}
	return template.HTML(sb.String())
}

// Front end: resolve menu item URLs/labels and mark the current item (wp_nav_menu_objects). It returns
// the list without the items whose target is gone or not public (WordPress marks those _invalid and
// skips them), so a draft, private, scheduled or trashed page's title and link never reach the menu.
func (s *Server) resolveMenu(ctx context.Context, items []*store.MenuItem, lb *links.Builder, q *siteQuery) []*store.MenuItem {
	// The menu's posts and terms in one query each.
	var postIDs, termIDs []int64
	var collect func(list []*store.MenuItem)
	collect = func(list []*store.MenuItem) {
		for _, it := range list {
			switch it.Kind {
			case "post_type":
				postIDs = append(postIDs, it.TargetID)
			case "taxonomy":
				termIDs = append(termIDs, it.TargetID)
			}
			collect(it.Children)
		}
	}
	collect(items)
	s.pllPrimeObjLangs(ctx, pllPost, postIDs) // Golylang: the targets' languages in one query each
	s.pllPrimeObjLangs(ctx, pllTerm, termIDs)
	posts, err := s.store.PostsByIDsNoContent(ctx, postIDs) // titles and permalinks only; meta kept
	if err != nil {
		posts = map[int64]*store.Post{}
	}
	// The child pages' parent chains (their permalinks carry the ancestors' slugs): one query per depth.
	var children []*store.Post
	for _, p := range posts {
		if p.ParentID != 0 && (p.Type == "page" || siteTypeHierarchical(p.Type)) {
			children = append(children, p)
		}
	}
	s.prefetchAncestors(ctx, children)
	terms, err := s.store.TermsByIDs(ctx, termIDs)
	if err != nil {
		terms = map[int64]*store.Term{}
	}
	// Menus are cached for every visitor, so only public targets count (no per-viewer exceptions).
	var prune func(list []*store.MenuItem) []*store.MenuItem
	prune = func(list []*store.MenuItem) []*store.MenuItem {
		out := make([]*store.MenuItem, 0, len(list))
		for _, it := range list {
			if it.Kind == "post_type" {
				cacheDep(ctx, "post:"+itoa64(it.TargetID)) // Pursae Cache: publishing it brings it back
				p := posts[it.TargetID]
				if p == nil || (p.Status != "publish" && !(p.Type == "attachment" && p.Status == "inherit")) {
					continue
				}
			}
			it.Children = prune(it.Children)
			out = append(out, it)
		}
		return out
	}
	items = prune(items)
	var walk func(list []*store.MenuItem) bool
	walk = func(list []*store.MenuItem) bool {
		any := false
		for _, it := range list {
			switch it.Kind {
			case "post_type":
				cacheDep(ctx, "post:"+itoa64(it.TargetID)) // Pursae Cache: its title and permalink
				if siteTypeHierarchical(it.Object) {
					cacheDep(ctx, "archive:"+it.Object) // the permalink carries the ancestors' slugs
				}
				if p := posts[it.TargetID]; p != nil {
					it.URL = lb.Permalink(p)
					if it.Label == "" {
						it.Label = defaultStr(p.Title, "(no title)")
					}
					if q.Post != nil && q.Post.ID == p.ID && (q.Kind == "single" || q.Kind == "page" || q.Kind == "front-page") {
						it.Current = true
					}
				}
			case "taxonomy":
				cacheDep(ctx, "term:"+itoa64(it.TargetID))
				if t := terms[it.TargetID]; t != nil {
					it.URL = lb.TermLink(t)
					if it.Label == "" {
						it.Label = t.Name
					}
					if q.Term != nil && q.Term.ID == t.ID {
						it.Current = true
					}
				}
			case "custom":
				if strings.TrimRight(it.URL, "/") == strings.TrimRight(lb.Home, "/") && (q.Kind == "home" || q.Kind == "front-page") {
					it.Current = true
				}
			}
			if walk(it.Children) {
				it.CurrentAncestor = true
			}
			if it.Current || it.CurrentAncestor {
				any = true
			}
		}
		return any
	}
	walk(items)
	return items
}

// autoAddPage is _wp_auto_add_pages_to_menu(): a newly published top-level page joins menus with "auto add".
func (s *Server) autoAddPage(ctx context.Context, p *store.Post) {
	if p.Type != "page" || p.ParentID != 0 || p.Status != "publish" {
		return
	}
	_, _ = s.store.Pool.Exec(ctx, `INSERT INTO menu_items(menu_id, sort_order, kind, object, target_id)
		SELECT m.id, coalesce((SELECT max(sort_order) + 1 FROM menu_items WHERE menu_id = m.id), 1), 'post_type', 'page', $1
		FROM menus m WHERE m.auto_add AND NOT EXISTS (SELECT 1 FROM menu_items i WHERE i.menu_id = m.id AND i.kind = 'post_type' AND i.target_id = $1)`, p.ID)
}
