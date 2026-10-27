package server

import (
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"

	"wpgo/internal/store"
	"wpgo/internal/wp"
)

// Navigation menus over REST: WP_REST_Menus_Controller (nav_menu terms), WP_REST_Menu_Items_Controller
// (nav_menu_item posts) and WP_REST_Menu_Locations_Controller, over the menus / menu_items tables.
// Like WordPress, none of them is public: every request needs edit_theme_options.

func (api *restAPI) registerMenus() {
	api.register("wp/v2", "/wp/v2/menus", map[string]restHandler{http.MethodGet: api.listMenus, http.MethodPost: api.createMenu}, nil)
	api.register("wp/v2", `/wp/v2/menus/(?P<id>[\d]+)`, map[string]restHandler{
		http.MethodGet: api.getMenu, http.MethodPost: api.updateMenu, http.MethodPut: api.updateMenu, http.MethodDelete: api.deleteMenu}, nil)
	api.register("wp/v2", "/wp/v2/menu-items", map[string]restHandler{http.MethodGet: api.listMenuItems, http.MethodPost: api.createMenuItem}, nil)
	api.register("wp/v2", `/wp/v2/menu-items/(?P<id>[\d]+)`, map[string]restHandler{
		http.MethodGet: api.getMenuItem, http.MethodPost: api.updateMenuItem, http.MethodPut: api.updateMenuItem, http.MethodDelete: api.deleteMenuItem}, nil)
	api.register("wp/v2", "/wp/v2/menu-locations", map[string]restHandler{http.MethodGet: api.menuLocations}, nil)
	api.register("wp/v2", `/wp/v2/menu-locations/(?P<location>[\w-]+)`, map[string]restHandler{http.MethodGet: api.menuLocations}, nil)
}

// menusAllowed is the controllers' permission check: edit_theme_options, with WordPress' status codes
// (401 for visitors, 403 for users without the capability).
func (api *restAPI) menusAllowed(c *gin.Context, code, msg string) bool {
	u := restCurrent(c)
	if u == nil {
		restErr(c, http.StatusUnauthorized, code, msg)
		return false
	}
	if !u.Can("edit_theme_options", nil) {
		restErr(c, http.StatusForbidden, code, msg)
		return false
	}
	return true
}

// Menus ------------------------------------------------------------------------

func (s *Server) restMenu(c *gin.Context, m *store.Menu) map[string]any {
	home := s.links(c).Home
	locations := []string{}
	locLinks := []map[string]any{}
	if m.Location != "" {
		locations = append(locations, m.Location)
		locLinks = append(locLinks, map[string]any{"embeddable": true, "href": home + "/wp-json/wp/v2/menu-locations/" + m.Location})
	}
	extra := map[string]any{"about": []map[string]any{{"href": home + "/wp-json/wp/v2/taxonomies/nav_menu"}}}
	if len(locLinks) > 0 {
		extra["wp:menu-location"] = locLinks
	}
	return map[string]any{
		"id": m.ID, "description": "", "name": m.Name, "slug": m.Slug, "meta": map[string]any{},
		"locations": locations, "auto_add": m.AutoAdd,
		"_links": restLinks(home+"/wp-json/wp/v2/menus/"+itoa64(m.ID), home+"/wp-json/wp/v2/menus", extra),
	}
}

func (s *Server) menuByID(c *gin.Context, id int64) *store.Menu {
	menus, _ := s.store.Menus(c)
	for _, m := range menus {
		if m.ID == id {
			return m
		}
	}
	return nil
}

func (api *restAPI) listMenus(c *gin.Context, r *restRequest) {
	s := api.s
	if !api.menusAllowed(c, "rest_cannot_view", "Sorry, you are not allowed to view menus.") {
		return
	}
	page, per := pageArgs(c)
	menus, err := s.store.Menus(c)
	if err != nil {
		restErr(c, http.StatusInternalServerError, "rest_query_error", err.Error())
		return
	}
	include, _ := r.IDs(c, "include")
	exclude, _ := r.IDs(c, "exclude")
	search := strings.ToLower(strings.TrimSpace(c.Query("search")))
	slug := c.Query("slug")
	var itemMenu int64
	if v, ok := r.Int(c, "menu_items"); ok {
		itemMenu = -1
		if it, err := s.store.MenuItem(c, v); err == nil {
			itemMenu = it.MenuID
		}
	}
	var list []*store.Menu
	for _, m := range menus {
		if len(include) > 0 && !containsID(include, m.ID) || containsID(exclude, m.ID) {
			continue
		}
		if search != "" && !strings.Contains(strings.ToLower(m.Name), search) && !strings.Contains(m.Slug, search) {
			continue
		}
		if slug != "" && !menuHas(strings.Split(slug, ","), m.Slug) {
			continue
		}
		if itemMenu != 0 && m.ID != itemMenu {
			continue
		}
		list = append(list, m)
	}
	out := []map[string]any{}
	for i, m := range list {
		if i < (page-1)*per || i >= page*per {
			continue
		}
		out = append(out, s.restMenu(c, m))
	}
	paginate(c, len(list), per, page)
	restJSON(c, http.StatusOK, out)
}

func (api *restAPI) getMenu(c *gin.Context, r *restRequest) {
	if !api.menusAllowed(c, "rest_cannot_view", "Sorry, you are not allowed to view this menu.") {
		return
	}
	m := api.s.menuByID(c, r.ID())
	if m == nil {
		restErr(c, http.StatusNotFound, "rest_term_invalid", "Term does not exist.")
		return
	}
	restJSON(c, http.StatusOK, api.s.restMenu(c, m))
}

func (api *restAPI) createMenu(c *gin.Context, r *restRequest) {
	s := api.s
	if !api.menusAllowed(c, "rest_cannot_create", "Sorry, you are not allowed to create new menus.") {
		return
	}
	name, ok := r.Str(c, "name")
	name = strings.TrimSpace(name)
	if !ok || name == "" {
		restErr(c, http.StatusBadRequest, "rest_missing_callback_param", "Missing parameter(s): name")
		return
	}
	menus, _ := s.store.Menus(c)
	for _, m := range menus {
		if strings.EqualFold(m.Name, name) {
			restErr(c, http.StatusBadRequest, "menu_exists", "The menu name "+name+" conflicts with another menu name. Please try another.", map[string]any{"term_id": m.ID})
			return
		}
	}
	id, err := s.store.CreateMenu(c, name)
	if err != nil {
		restErr(c, http.StatusInternalServerError, "rest_term_error", err.Error())
		return
	}
	if autoAdd, ok := r.Bool(c, "auto_add"); ok && autoAdd {
		_ = s.store.UpdateMenu(c, id, name, true)
	}
	api.assignMenuLocations(c, r, id)
	s.purgeAll(c)
	c.Header("Location", s.links(c).HomeURL("/wp-json/wp/v2/menus/"+itoa64(id)))
	restJSON(c, http.StatusCreated, s.restMenu(c, s.menuByID(c, id)))
}

// assignMenuLocations applies the "locations" argument: the menu takes each listed theme location.
func (api *restAPI) assignMenuLocations(c *gin.Context, r *restRequest, menuID int64) bool {
	s := api.s
	raw, ok := r.get(c, "locations")
	if !ok {
		return true
	}
	var want []string
	switch t := raw.(type) {
	case []any:
		for _, x := range t {
			if str, ok := x.(string); ok {
				want = append(want, str)
			}
		}
	case string:
		if t != "" {
			want = strings.Split(t, ",")
		}
	}
	if len(want) == 0 {
		want = c.QueryArray("locations[]")
	}
	registered := map[string]bool{}
	for _, l := range s.activeTheme().MenuLocations {
		registered[l.Slug] = true
	}
	locs := map[string]int64{}
	for _, l := range want {
		if !registered[l] {
			restErr(c, http.StatusBadRequest, "invalid_menu_location", "Invalid menu location.")
			return false
		}
		locs[l] = menuID
	}
	// Locations the menu held before and that are no longer listed are unassigned.
	if m := s.menuByID(c, menuID); m != nil && m.Location != "" && !menuHas(want, m.Location) {
		locs[m.Location] = 0
	}
	_ = s.store.SetMenuLocations(c, locs)
	return true
}

func (api *restAPI) updateMenu(c *gin.Context, r *restRequest) {
	s := api.s
	if !api.menusAllowed(c, "rest_cannot_update", "Sorry, you are not allowed to update this menu.") {
		return
	}
	m := s.menuByID(c, r.ID())
	if m == nil {
		restErr(c, http.StatusNotFound, "rest_term_invalid", "Term does not exist.")
		return
	}
	name, autoAdd := m.Name, m.AutoAdd
	if v, ok := r.Str(c, "name"); ok && strings.TrimSpace(v) != "" {
		name = strings.TrimSpace(v)
	}
	if v, ok := r.Bool(c, "auto_add"); ok {
		autoAdd = v
	}
	menus, _ := s.store.Menus(c)
	for _, other := range menus {
		if other.ID != m.ID && strings.EqualFold(other.Name, name) {
			restErr(c, http.StatusBadRequest, "menu_exists", "The menu name "+name+" conflicts with another menu name. Please try another.", map[string]any{"term_id": other.ID})
			return
		}
	}
	if err := s.store.UpdateMenu(c, m.ID, name, autoAdd); err != nil {
		restErr(c, http.StatusInternalServerError, "rest_term_error", err.Error())
		return
	}
	if !api.assignMenuLocations(c, r, m.ID) {
		return
	}
	s.purgeAll(c)
	restJSON(c, http.StatusOK, s.restMenu(c, s.menuByID(c, m.ID)))
}

func (api *restAPI) deleteMenu(c *gin.Context, r *restRequest) {
	s := api.s
	if !api.menusAllowed(c, "rest_cannot_delete", "Sorry, you are not allowed to delete this menu.") {
		return
	}
	m := s.menuByID(c, r.ID())
	if m == nil {
		restErr(c, http.StatusNotFound, "rest_term_invalid", "Term does not exist.")
		return
	}
	if force, _ := r.Bool(c, "force"); !force {
		restErr(c, http.StatusNotImplemented, "rest_trash_not_supported", "Terms do not support trashing. Set 'force=true' to delete.")
		return
	}
	prev := s.restMenu(c, m)
	if err := s.store.DeleteMenu(c, m.ID); err != nil {
		restErr(c, http.StatusInternalServerError, "rest_cannot_delete", "The menu cannot be deleted.")
		return
	}
	s.purgeAll(c)
	restJSON(c, http.StatusOK, map[string]any{"deleted": true, "previous": prev})
}

// Menu items -----------------------------------------------------------------

// menuItemTypeLabel is the item's type_label (the object's singular name, "Custom Link" for links).
func menuItemTypeLabel(it *store.EditItem) string {
	switch it.Kind {
	case "post_type":
		switch it.Object {
		case "page":
			return "Page"
		case "post":
			return "Post"
		}
		return ucfirstStr(it.Object)
	case "taxonomy":
		switch it.Object {
		case "category":
			return "Category"
		case "post_tag":
			return "Tag"
		}
		return ucfirstStr(it.Object)
	case "post_type_archive":
		return ucfirstStr(it.Object) + " Archives"
	}
	return "Custom Link"
}

// menuItemURL is the item's current link: the object's permalink for post and term items.
func (s *Server) menuItemURL(c *gin.Context, it *store.EditItem) string {
	lb := s.links(c)
	switch it.Kind {
	case "post_type":
		if p, err := s.store.GetPost(c, it.TargetID); err == nil {
			return lb.Permalink(p)
		}
	case "taxonomy":
		if t, err := s.store.GetTerm(c, it.TargetID); err == nil {
			return lb.TermLink(t)
		}
	}
	return it.URL
}

func splitWords(v string) []string {
	out := []string{}
	for _, w := range strings.Fields(v) {
		out = append(out, w)
	}
	return out
}

func (s *Server) restMenuItem(c *gin.Context, it *store.EditItem) map[string]any {
	home := s.links(c).Home
	self := home + "/wp-json/wp/v2/menu-items/" + itoa64(it.ID)
	extra := map[string]any{
		"about":   []map[string]any{{"href": home + "/wp-json/wp/v2/types/nav_menu_item"}},
		"wp:term": []map[string]any{{"taxonomy": "nav_menu", "embeddable": true, "href": home + "/wp-json/wp/v2/menus?menu_items=" + itoa64(it.ID)}},
	}
	switch it.Kind {
	case "post_type":
		if it.TargetID > 0 {
			base := "posts"
			if it.Object == "page" {
				base = "pages"
			}
			extra["wp:menu-item-object"] = []map[string]any{{"post_type": it.Object, "embeddable": true, "href": home + "/wp-json/wp/v2/" + base + "/" + itoa64(it.TargetID)}}
		}
	case "taxonomy":
		if it.TargetID > 0 {
			base := "categories"
			if it.Object == "post_tag" {
				base = "tags"
			}
			extra["wp:menu-item-object"] = []map[string]any{{"taxonomy": it.Object, "embeddable": true, "href": home + "/wp-json/wp/v2/" + base + "/" + itoa64(it.TargetID)}}
		}
	}
	return map[string]any{
		"id":          it.ID,
		"title":       map[string]any{"raw": it.Label, "rendered": wp.Texturize(wp.EscText(it.Label))},
		"status":      defaultStr(it.Status, "publish"),
		"url":         s.menuItemURL(c, it),
		"attr_title":  it.AttrTitle,
		"description": it.Description,
		"type":        defaultStr(it.Kind, "custom"),
		"type_label":  menuItemTypeLabel(it),
		"object":      defaultStr(it.Object, "custom"),
		"object_id":   it.TargetID,
		"parent":      it.ParentID,
		"menu_order":  it.Order,
		"target":      it.Target,
		"classes":     splitWords(it.Classes),
		"xfn":         splitWords(it.XFN),
		"invalid":     false,
		"meta":        map[string]any{},
		"menus":       it.MenuID,
		"_links":      restLinks(self, home+"/wp-json/wp/v2/menu-items", extra),
	}
}

func (api *restAPI) listMenuItems(c *gin.Context, r *restRequest) {
	s := api.s
	if !api.menusAllowed(c, "rest_cannot_view", "Sorry, you are not allowed to view menu items.") {
		return
	}
	page, per := pageArgs(c)
	menus, _ := r.IDs(c, "menus")
	var items []*store.EditItem
	if len(menus) == 0 {
		items, _ = s.store.AllMenuItems(c, 0)
	} else {
		for _, id := range menus {
			list, _ := s.store.AllMenuItems(c, id)
			items = append(items, list...)
		}
	}
	include, _ := r.IDs(c, "include")
	exclude, _ := r.IDs(c, "exclude")
	status := c.DefaultQuery("status", "publish")
	search := strings.ToLower(strings.TrimSpace(c.Query("search")))
	menuOrder, hasOrder := r.Int(c, "menu_order")
	var list []*store.EditItem
	for _, it := range items {
		if len(include) > 0 && !containsID(include, it.ID) || containsID(exclude, it.ID) {
			continue
		}
		if status != "any" && !menuHas(strings.Split(status, ","), defaultStr(it.Status, "publish")) {
			continue
		}
		if hasOrder && int64(it.Order) != menuOrder {
			continue
		}
		if search != "" && !strings.Contains(strings.ToLower(it.Label), search) && !strings.Contains(strings.ToLower(it.URL), search) {
			continue
		}
		list = append(list, it)
	}
	orderby, desc := c.DefaultQuery("orderby", "menu_order"), strings.EqualFold(c.Query("order"), "desc")
	sort.SliceStable(list, func(i, j int) bool {
		var less bool
		switch orderby {
		case "id":
			less = list[i].ID < list[j].ID
		case "title":
			less = strings.ToLower(list[i].Label) < strings.ToLower(list[j].Label)
		default:
			less = list[i].Order < list[j].Order || (list[i].Order == list[j].Order && list[i].ID < list[j].ID)
		}
		if desc {
			return !less && !(list[i].Order == list[j].Order && list[i].ID == list[j].ID)
		}
		return less
	})
	out := []map[string]any{}
	for i, it := range list {
		if i < (page-1)*per || i >= page*per {
			continue
		}
		out = append(out, s.restMenuItem(c, it))
	}
	paginate(c, len(list), per, page)
	restJSON(c, http.StatusOK, out)
}

func (api *restAPI) getMenuItem(c *gin.Context, r *restRequest) {
	if !api.menusAllowed(c, "rest_cannot_view", "Sorry, you are not allowed to view this menu item.") {
		return
	}
	it, err := api.s.store.MenuItem(c, r.ID())
	if err != nil {
		restErr(c, http.StatusNotFound, "rest_post_invalid_id", "Invalid post ID.")
		return
	}
	restJSON(c, http.StatusOK, api.s.restMenuItem(c, it))
}

// prepareMenuItem is WP_REST_Menu_Items_Controller::prepare_item_for_database(): the request's
// fields are merged into it and validated; the response error is already written on failure.
func (api *restAPI) prepareMenuItem(c *gin.Context, r *restRequest, it *store.EditItem, create bool) bool {
	s := api.s
	if v, ok := r.Int(c, "menus"); ok {
		it.MenuID = v
	} else if ids, ok := r.IDs(c, "menus"); ok && len(ids) > 0 {
		it.MenuID = ids[0]
	}
	if it.MenuID == 0 || s.menuByID(c, it.MenuID) == nil {
		restErr(c, http.StatusBadRequest, "invalid_menu_id", "Invalid menu ID.")
		return false
	}
	if v, ok := r.Str(c, "type"); ok {
		switch v {
		case "custom", "post_type", "taxonomy", "post_type_archive":
			it.Kind = v
		default:
			restErr(c, http.StatusBadRequest, "rest_invalid_param", "Invalid parameter(s): type")
			return false
		}
	}
	if it.Kind == "" {
		it.Kind = "custom"
	}
	if v, ok := r.Str(c, "title"); ok {
		it.Label = strings.TrimSpace(v)
	}
	if v, ok := r.Str(c, "object"); ok {
		it.Object = v
	}
	if v, ok := r.Int(c, "object_id"); ok {
		it.TargetID = v
	}
	if v, ok := r.Str(c, "url"); ok {
		it.URL = strings.TrimSpace(v)
	}
	for key, dst := range map[string]*string{"attr_title": &it.AttrTitle, "description": &it.Description, "target": &it.Target} {
		if v, ok := r.Str(c, key); ok {
			*dst = v
		}
	}
	for key, dst := range map[string]*string{"classes": &it.Classes, "xfn": &it.XFN} {
		raw, ok := r.get(c, key)
		if !ok {
			continue
		}
		switch t := raw.(type) {
		case []any:
			var words []string
			for _, x := range t {
				if str, ok := x.(string); ok && strings.TrimSpace(str) != "" {
					words = append(words, strings.TrimSpace(str))
				}
			}
			*dst = strings.Join(words, " ")
		case string:
			*dst = strings.Join(strings.Fields(strings.ReplaceAll(t, ",", " ")), " ")
		}
	}
	if v, ok := r.Int(c, "parent"); ok {
		it.ParentID = v
	}
	if v, ok := r.Int(c, "menu_order"); ok {
		it.Order = int(v)
	}
	if v, ok := r.Str(c, "status"); ok {
		if v != "publish" && v != "draft" {
			restErr(c, http.StatusBadRequest, "rest_invalid_param", "Invalid parameter(s): status")
			return false
		}
		it.Status = v
	}
	if it.Target != "" && it.Target != "_blank" {
		restErr(c, http.StatusBadRequest, "rest_invalid_param", "Invalid parameter(s): target")
		return false
	}
	switch it.Kind {
	case "custom":
		if it.Label == "" {
			restErr(c, http.StatusBadRequest, "rest_title_required", "The title is required when using a custom menu item type.")
			return false
		}
		if it.URL == "" {
			restErr(c, http.StatusBadRequest, "rest_url_required", "The url is required when using a custom menu item type.")
			return false
		}
		if u, err := url.Parse(it.URL); err != nil || (u.Scheme != "" && u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "mailto" && u.Scheme != "tel") {
			restErr(c, http.StatusBadRequest, "rest_invalid_url", "Invalid URL.")
			return false
		}
		it.Object = "custom"
	case "post_type":
		p, err := s.store.GetPost(c, it.TargetID)
		if err != nil || (it.Object != "" && p.Type != it.Object) {
			restErr(c, http.StatusBadRequest, "rest_post_invalid_id", "Invalid post ID.")
			return false
		}
		it.Object = p.Type
		if it.Label == "" {
			it.Label = p.Title
		}
	case "taxonomy":
		t, err := s.store.GetTerm(c, it.TargetID)
		if err != nil || (it.Object != "" && t.Taxonomy != it.Object) {
			restErr(c, http.StatusBadRequest, "rest_term_invalid_id", "Invalid term ID.")
			return false
		}
		it.Object = t.Taxonomy
		if it.Label == "" {
			it.Label = t.Name
		}
	case "post_type_archive":
		if it.Object == "" {
			restErr(c, http.StatusBadRequest, "rest_post_invalid_type", "Invalid post type.")
			return false
		}
	}
	if it.ParentID != 0 {
		parent, err := s.store.MenuItem(c, it.ParentID)
		if err != nil || parent.MenuID != it.MenuID || parent.ID == it.ID {
			restErr(c, http.StatusBadRequest, "rest_post_invalid_parent", "Invalid parent ID.")
			return false
		}
	}
	if create && it.Order == 0 {
		items, _ := s.store.AllMenuItems(c, it.MenuID)
		it.Order = len(items) + 1
	}
	if it.Status == "" {
		it.Status = "publish"
	}
	return true
}

func (api *restAPI) createMenuItem(c *gin.Context, r *restRequest) {
	s := api.s
	if !api.menusAllowed(c, "rest_cannot_create", "Sorry, you are not allowed to create new menu items.") {
		return
	}
	it := &store.EditItem{}
	if !api.prepareMenuItem(c, r, it, true) {
		return
	}
	if err := s.store.AddMenuItem(c, it); err != nil {
		restErr(c, http.StatusInternalServerError, "rest_cannot_create", err.Error())
		return
	}
	s.purgeAll(c)
	c.Header("Location", s.links(c).HomeURL("/wp-json/wp/v2/menu-items/"+itoa64(it.ID)))
	restJSON(c, http.StatusCreated, s.restMenuItem(c, it))
}

func (api *restAPI) updateMenuItem(c *gin.Context, r *restRequest) {
	s := api.s
	if !api.menusAllowed(c, "rest_cannot_update", "Sorry, you are not allowed to update this menu item.") {
		return
	}
	it, err := s.store.MenuItem(c, r.ID())
	if err != nil {
		restErr(c, http.StatusNotFound, "rest_post_invalid_id", "Invalid post ID.")
		return
	}
	if !api.prepareMenuItem(c, r, it, false) {
		return
	}
	if err := s.store.UpdateMenuItem(c, it); err != nil {
		restErr(c, http.StatusInternalServerError, "rest_cannot_update", err.Error())
		return
	}
	s.purgeAll(c)
	restJSON(c, http.StatusOK, s.restMenuItem(c, it))
}

func (api *restAPI) deleteMenuItem(c *gin.Context, r *restRequest) {
	s := api.s
	if !api.menusAllowed(c, "rest_cannot_delete", "Sorry, you are not allowed to delete this menu item.") {
		return
	}
	it, err := s.store.MenuItem(c, r.ID())
	if err != nil {
		restErr(c, http.StatusNotFound, "rest_post_invalid_id", "Invalid post ID.")
		return
	}
	if force, _ := r.Bool(c, "force"); !force {
		restErr(c, http.StatusNotImplemented, "rest_trash_not_supported", "The post does not support trashing. Set 'force=true' to delete.")
		return
	}
	prev := s.restMenuItem(c, it)
	if err := s.store.DeleteMenuItem(c, it.ID); err != nil {
		restErr(c, http.StatusInternalServerError, "rest_cannot_delete", "The post cannot be deleted.")
		return
	}
	s.purgeAll(c)
	restJSON(c, http.StatusOK, map[string]any{"deleted": true, "previous": prev})
}

// Menu locations ---------------------------------------------------------------

func (api *restAPI) menuLocations(c *gin.Context, r *restRequest) {
	s := api.s
	if !api.menusAllowed(c, "rest_cannot_view", "Sorry, you are not allowed to view menu locations.") {
		return
	}
	home := s.links(c).Home
	assigned := map[string]int64{}
	menus, _ := s.store.Menus(c)
	for _, m := range menus {
		if m.Location != "" {
			assigned[m.Location] = m.ID
		}
	}
	mk := func(l MenuLocation) map[string]any {
		extra := map[string]any{}
		if id := assigned[l.Slug]; id > 0 {
			extra["wp:menu"] = []map[string]any{{"embeddable": true, "href": home + "/wp-json/wp/v2/menus/" + itoa64(id)}}
		}
		return map[string]any{"name": l.Slug, "description": l.Name, "menu": assigned[l.Slug],
			"_links": restLinks(home+"/wp-json/wp/v2/menu-locations/"+l.Slug, home+"/wp-json/wp/v2/menu-locations", extra)}
	}
	locations := s.activeTheme().MenuLocations
	if want := r.Params["location"]; want != "" {
		for _, l := range locations {
			if l.Slug == want {
				restJSON(c, http.StatusOK, mk(l))
				return
			}
		}
		restErr(c, http.StatusNotFound, "rest_menu_location_invalid", "Invalid menu location.")
		return
	}
	out := map[string]any{}
	for _, l := range locations {
		out[l.Slug] = mk(l)
	}
	restJSON(c, http.StatusOK, out)
}

func containsID(ids []int64, id int64) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

func menuHas(list []string, v string) bool {
	for _, x := range list {
		if strings.TrimSpace(x) == v {
			return true
		}
	}
	return false
}
