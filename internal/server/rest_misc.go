package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"wpgo/internal/auth"
	"wpgo/internal/store"
	"wpgo/internal/wp"
)

// twentySixteenPalette is the theme's add_theme_support( 'editor-color-palette' ).
var twentySixteenPalette = []map[string]string{
	{"name": "Dark Gray", "slug": "dark-gray", "color": "#1a1a1a"},
	{"name": "Medium Gray", "slug": "medium-gray", "color": "#686868"},
	{"name": "Light Gray", "slug": "light-gray", "color": "#e5e5e5"},
	{"name": "White", "slug": "white", "color": "#fff"},
	{"name": "Blue Gray", "slug": "blue-gray", "color": "#4d545c"},
	{"name": "Bright Blue", "slug": "bright-blue", "color": "#007acc"},
	{"name": "Light Blue", "slug": "light-blue", "color": "#9adffd"},
	{"name": "Dark Brown", "slug": "dark-brown", "color": "#402b30"},
	{"name": "Medium Brown", "slug": "medium-brown", "color": "#774e24"},
	{"name": "Dark Red", "slug": "dark-red", "color": "#640c1f"},
	{"name": "Bright Red", "slug": "bright-red", "color": "#ff675f"},
	{"name": "Yellow", "slug": "yellow", "color": "#ffef8e"},
}

var postFormats = []string{"aside", "image", "video", "quote", "link", "gallery", "status", "audio", "chat"}

func (api *restAPI) registerMisc() {
	g := func(h restHandler) map[string]restHandler { return map[string]restHandler{http.MethodGet: h} }
	api.register("wp/v2", "/wp/v2/types", g(api.types), nil)
	api.register("wp/v2", `/wp/v2/types/(?P<type>[\w-]+)`, g(api.types), nil)
	api.register("wp/v2", "/wp/v2/taxonomies", g(api.taxonomies), nil)
	api.register("wp/v2", `/wp/v2/taxonomies/(?P<taxonomy>[\w-]+)`, g(api.taxonomies), nil)
	api.register("wp/v2", "/wp/v2/statuses", g(api.statuses), nil)
	api.register("wp/v2", `/wp/v2/statuses/(?P<status>[\w-]+)`, g(api.statuses), nil)
	api.register("wp/v2", "/wp/v2/settings", map[string]restHandler{http.MethodGet: api.getSettings, http.MethodPost: api.saveSettings, http.MethodPut: api.saveSettings}, nil)
	api.register("wp/v2", "/wp/v2/themes", g(api.themes), nil)
	api.register("wp/v2", `/wp/v2/themes/(?P<stylesheet>[\w-]+)`, g(api.themes), nil)
	api.register("wp/v2", "/wp/v2/search", g(api.search), nil)
	api.register("wp/v2", "/wp/v2/block-patterns/patterns", g(func(c *gin.Context, r *restRequest) { restJSON(c, http.StatusOK, corePatterns()) }), nil)
	api.register("wp/v2", "/wp/v2/block-patterns/categories", g(func(c *gin.Context, r *restRequest) { restJSON(c, http.StatusOK, corePatternCategories()) }), nil)
	api.register("wp/v2", "/wp/v2/pattern-directory/patterns", g(func(c *gin.Context, r *restRequest) { restJSON(c, http.StatusOK, []any{}) }), nil)
	api.register("wp/v2", "/wp/v2/block-directory/search", g(func(c *gin.Context, r *restRequest) { restJSON(c, http.StatusOK, []any{}) }), nil)
	api.register("wp/v2", "/wp/v2/wp_pattern_category", g(func(c *gin.Context, r *restRequest) { paginate(c, 0, 100, 1); restJSON(c, http.StatusOK, []any{}) }), nil)
	api.register("wp/v2", "/wp/v2/templates", g(func(c *gin.Context, r *restRequest) { restJSON(c, http.StatusOK, []any{}) }), nil)
	api.register("wp/v2", "/wp/v2/templates/lookup", g(func(c *gin.Context, r *restRequest) {
		restErr(c, http.StatusNotFound, "rest_template_not_found", "No templates exist with that id.")
	}), nil)
	api.register("wp/v2", "/wp/v2/template-parts", g(func(c *gin.Context, r *restRequest) { restJSON(c, http.StatusOK, []any{}) }), nil)
	api.register("wp/v2", `/wp/v2/global-styles/themes/(?P<stylesheet>[\w-]+)`, g(func(c *gin.Context, r *restRequest) {
		restJSON(c, http.StatusOK, map[string]any{"settings": map[string]any{}, "styles": map[string]any{}})
	}), nil)
	api.register("wp/v2", `/wp/v2/global-styles/themes/(?P<stylesheet>[\w-]+)/variations`, g(func(c *gin.Context, r *restRequest) { restJSON(c, http.StatusOK, []any{}) }), nil)
	api.register("wp/v2", `/wp/v2/global-styles/(?P<id>[\d]+)`, map[string]restHandler{
		http.MethodGet: func(c *gin.Context, r *restRequest) {
			restJSON(c, http.StatusOK, map[string]any{"id": r.ID(), "title": map[string]any{"raw": "Custom Styles", "rendered": "Custom Styles"}, "settings": map[string]any{}, "styles": map[string]any{}})
		},
	}, nil)
	api.register("wp/v2", "/wp/v2/block-types", g(func(c *gin.Context, r *restRequest) { restJSON(c, http.StatusOK, []any{}) }), nil)
	api.register("oembed/1.0", "/oembed/1.0/proxy", g(api.oembedProxy), nil)
	api.register("wp-block-editor/v1", "/wp-block-editor/v1/url-details", g(func(c *gin.Context, r *restRequest) {
		restJSON(c, http.StatusOK, map[string]any{"title": "", "icon": "", "description": "", "image": ""})
	}), nil)
}

func typeObject(home, slug, name, singular, base string, hierarchical bool, supports map[string]bool, taxonomies []string, labels map[string]string, edit bool) map[string]any {
	o := map[string]any{
		"description": "", "hierarchical": hierarchical, "has_archive": false, "name": name, "slug": slug,
		"taxonomies": taxonomies, "rest_base": base, "rest_namespace": "wp/v2", "icon": nil,
		"template": []any{}, "template_lock": false,
		"_links": map[string]any{
			"collection": []map[string]string{{"href": home + "/wp-json/wp/v2/types"}},
			"wp:items":   []map[string]string{{"href": home + "/wp-json/wp/v2/" + base}},
		},
	}
	switch slug {
	case "post":
		o["icon"] = "dashicons-admin-post"
	case "page":
		o["icon"] = "dashicons-admin-page"
	case "attachment":
		o["icon"] = "dashicons-admin-media"
	}
	if edit {
		o["viewable"] = slug != "wp_block"
		o["supports"] = supports
		o["visibility"] = map[string]bool{"show_ui": slug != "attachment", "show_in_nav_menus": slug == "post" || slug == "page"}
		o["labels"] = labels
		o["capabilities"] = map[string]string{"edit_post": "edit_post", "read_post": "read_post", "delete_post": "delete_post",
			"edit_posts": "edit_" + base, "edit_others_posts": "edit_others_" + base, "publish_posts": "publish_" + base,
			"read_private_posts": "read_private_" + base, "create_posts": "edit_" + base}
	}
	return o
}

func typeLabelsFull(singular, plural string) map[string]string {
	lp := strings.ToLower(plural)
	return map[string]string{
		"name": plural, "singular_name": singular, "add_new": "Add New " + singular, "add_new_item": "Add New " + singular,
		"edit_item": "Edit " + singular, "new_item": "New " + singular, "view_item": "View " + singular, "view_items": "View " + plural,
		"search_items": "Search " + plural, "not_found": "No " + lp + " found.", "not_found_in_trash": "No " + lp + " found in Trash.",
		"parent_item_colon": "Parent " + singular + ":", "all_items": "All " + plural, "archives": singular + " Archives",
		"attributes": singular + " Attributes", "insert_into_item": "Insert into " + strings.ToLower(singular),
		"uploaded_to_this_item": "Uploaded to this " + strings.ToLower(singular), "featured_image": "Featured image",
		"set_featured_image": "Set featured image", "remove_featured_image": "Remove featured image", "use_featured_image": "Use as featured image",
		"filter_items_list": "Filter " + lp + " list", "items_list_navigation": plural + " list navigation", "items_list": plural + " list",
		"item_published": singular + " published.", "item_published_privately": singular + " published privately.",
		"item_reverted_to_draft": singular + " reverted to draft.", "item_trashed": singular + " trashed.", "item_scheduled": singular + " scheduled.",
		"item_updated": singular + " updated.", "item_link": singular + " Link", "item_link_description": "A link to a " + strings.ToLower(singular) + ".",
		"menu_name": plural, "name_admin_bar": singular,
	}
}

func (api *restAPI) types(c *gin.Context, r *restRequest) {
	home := api.s.links(c).Home
	edit := restContext(c) == "edit" && restCurrent(c) != nil
	all := map[string]map[string]any{
		"post": typeObject(home, "post", "Posts", "Post", "posts", false, map[string]bool{"title": true, "editor": true, "author": true, "thumbnail": true,
			"excerpt": true, "trackbacks": true, "custom-fields": true, "comments": true, "revisions": true, "post-formats": true, "autosave": true}, []string{"category", "post_tag"}, typeLabelsFull("Post", "Posts"), edit),
		"page": typeObject(home, "page", "Pages", "Page", "pages", true, map[string]bool{"title": true, "editor": true, "author": true, "thumbnail": true,
			"page-attributes": true, "custom-fields": true, "comments": true, "revisions": true, "autosave": true}, []string{}, typeLabelsFull("Page", "Pages"), edit),
		"attachment": typeObject(home, "attachment", "Media", "Media", "media", false, map[string]bool{"title": true, "author": true, "comments": true}, []string{}, typeLabelsFull("Media", "Media"), edit),
		"wp_block":   typeObject(home, "wp_block", "Patterns", "Pattern", "blocks", false, map[string]bool{"title": true, "editor": true, "revisions": true, "custom-fields": true}, []string{"wp_pattern_category"}, typeLabelsFull("Pattern", "Patterns"), edit),
	}
	if t := r.Params["type"]; t != "" {
		o, ok := all[t]
		if !ok {
			restErr(c, http.StatusNotFound, "rest_type_invalid", "Invalid post type.")
			return
		}
		restJSON(c, http.StatusOK, o)
		return
	}
	out := map[string]any{}
	for k, v := range all {
		out[k] = v
	}
	restJSON(c, http.StatusOK, out)
}

func (api *restAPI) taxonomies(c *gin.Context, r *restRequest) {
	home := api.s.links(c).Home
	edit := restContext(c) == "edit" && restCurrent(c) != nil
	mk := func(slug, name, singular, base string, hier bool) map[string]any {
		o := map[string]any{"name": name, "slug": slug, "description": "", "types": []string{"post"}, "hierarchical": hier,
			"rest_base": base, "rest_namespace": "wp/v2",
			"_links": map[string]any{"collection": []map[string]string{{"href": home + "/wp-json/wp/v2/taxonomies"}},
				"wp:items": []map[string]string{{"href": home + "/wp-json/wp/v2/" + base}}}}
		if edit {
			o["show_cloud"] = slug == "post_tag"
			o["visibility"] = map[string]bool{"public": true, "publicly_queryable": true, "show_admin_column": true, "show_in_nav_menus": true, "show_in_quick_edit": true, "show_ui": true}
			o["capabilities"] = map[string]string{"manage_terms": "manage_categories", "edit_terms": "manage_categories", "delete_terms": "manage_categories", "assign_terms": "edit_posts"}
			o["labels"] = map[string]string{"name": name, "singular_name": singular, "search_items": "Search " + name, "popular_items": "Popular " + name,
				"all_items": "All " + name, "parent_item": "Parent " + singular, "parent_item_colon": "Parent " + singular + ":", "edit_item": "Edit " + singular,
				"view_item": "View " + singular, "update_item": "Update " + singular, "add_new_item": "Add New " + singular, "new_item_name": "New " + singular + " Name",
				"separate_items_with_commas": "Separate " + strings.ToLower(name) + " with commas", "add_or_remove_items": "Add or remove " + strings.ToLower(name),
				"choose_from_most_used": "Choose from the most used " + strings.ToLower(name), "not_found": "No " + strings.ToLower(name) + " found.",
				"no_terms": "No " + strings.ToLower(name), "back_to_items": "&larr; Go to " + name, "menu_name": name, "name_admin_bar": singular,
				"most_used": "Most Used", "item_link": singular + " Link"}
		}
		return o
	}
	all := map[string]map[string]any{
		"category": mk("category", "Categories", "Category", "categories", true),
		"post_tag": mk("post_tag", "Tags", "Tag", "tags", false),
	}
	if t := r.Params["taxonomy"]; t != "" {
		o, ok := all[t]
		if !ok {
			restErr(c, http.StatusNotFound, "rest_taxonomy_invalid", "Invalid taxonomy.")
			return
		}
		restJSON(c, http.StatusOK, o)
		return
	}
	if typ := c.Query("type"); typ != "" && typ != "post" {
		restJSON(c, http.StatusOK, map[string]any{})
		return
	}
	out := map[string]any{}
	for k, v := range all {
		out[k] = v
	}
	restJSON(c, http.StatusOK, out)
}

func (api *restAPI) statuses(c *gin.Context, r *restRequest) {
	mk := func(name, slug string, public bool) map[string]any {
		return map[string]any{"name": name, "slug": slug, "public": public, "queryable": public, "date_floating": slug == "draft" || slug == "pending" || slug == "auto-draft"}
	}
	all := map[string]any{"publish": mk("Published", "publish", true), "future": mk("Scheduled", "future", false), "draft": mk("Draft", "draft", false),
		"pending": mk("Pending", "pending", false), "private": mk("Private", "private", false), "trash": mk("Trash", "trash", false)}
	if st := r.Params["status"]; st != "" {
		if o, ok := all[st]; ok {
			restJSON(c, http.StatusOK, o)
			return
		}
		restErr(c, http.StatusNotFound, "rest_status_invalid", "Invalid status.")
		return
	}
	restJSON(c, http.StatusOK, all)
}

// restSettingsMap maps REST setting names to options (WP_REST_Settings_Controller).
var restSettingsMap = map[string]string{
	"title": "blogname", "description": "blogdescription", "url": "siteurl", "email": "admin_email", "timezone": "timezone_string",
	"date_format": "date_format", "time_format": "time_format", "start_of_week": "start_of_week", "language": "WPLANG",
	"use_smilies": "use_smilies", "default_category": "default_category", "default_post_format": "default_post_format",
	"posts_per_page": "posts_per_page", "show_on_front": "show_on_front", "page_on_front": "page_on_front", "page_for_posts": "page_for_posts",
	"default_ping_status": "default_ping_status", "default_comment_status": "default_comment_status", "site_logo": "site_logo", "site_icon": "site_icon",
}

func (api *restAPI) settingsObject() map[string]any {
	s := api.s
	out := map[string]any{}
	for k, opt := range restSettingsMap {
		out[k] = s.settings.Get(opt)
	}
	if out["language"] == "" {
		out["language"] = "en_US"
	}
	if out["site_logo"] == nil {
		out["site_logo"] = 0
	}
	return out
}

func (api *restAPI) getSettings(c *gin.Context, r *restRequest) {
	if !restCurrent(c).Can("manage_options", nil) {
		restErr(c, http.StatusForbidden, "rest_forbidden", "Sorry, you are not allowed to do that.")
		return
	}
	restJSON(c, http.StatusOK, api.settingsObject())
}

func (api *restAPI) saveSettings(c *gin.Context, r *restRequest) {
	if !restCurrent(c).Can("manage_options", nil) {
		restErr(c, http.StatusForbidden, "rest_forbidden", "Sorry, you are not allowed to do that.")
		return
	}
	values := map[string]any{}
	for k, opt := range restSettingsMap {
		if v, ok := r.Body[k]; ok {
			values[opt] = v
		}
	}
	if len(values) > 0 {
		if err := api.s.settings.Set(c, values); err != nil {
			restErr(c, http.StatusInternalServerError, "rest_settings_error", err.Error())
			return
		}
		api.s.purgeAll(c)
	}
	restJSON(c, http.StatusOK, api.settingsObject())
}

func (api *restAPI) themes(c *gin.Context, r *restRequest) {
	home := api.s.links(c).Home
	active := api.s.activeTheme()
	themeJSON := func(th *Theme) map[string]any {
		status := "inactive"
		if th.Slug == active.Slug {
			status = "active"
		}
		palette := any(false)
		if len(th.Palette) > 0 {
			palette = th.Palette
		}
		supports := map[string]any{
			"align-wide": false, "appearance-tools": false, "automatic-feed-links": true, "block-templates": false, "border": false,
			"custom-background": false, "custom-header": false, "custom-line-height": true, "custom-logo": map[string]any{"width": 240, "height": 240, "flex-width": false, "flex-height": true, "header-text": "", "unlink-homepage-logo": false},
			"customize-selective-refresh-widgets": true, "custom-spacing": false, "custom-units": true, "dark-editor-style": false,
			"disable-custom-colors": false, "disable-custom-font-sizes": false, "disable-custom-gradients": false, "disable-layout-styles": false,
			"editor-color-palette": palette, "editor-font-sizes": false, "editor-gradient-presets": false, "editor-spacing-sizes": false,
			"editor-styles": len(th.EditorStyles) > 0, "formats": append([]string{"standard"}, postFormats...),
			"html5":      []string{"search-form", "comment-form", "comment-list", "gallery", "caption", "script", "style", "navigation-widgets"},
			"link-color": false, "post-thumbnails": true, "responsive-embeds": true, "title-tag": true, "wp-block-styles": true,
		}
		for k, v := range th.Supports {
			supports[k] = v
		}
		return map[string]any{
			"stylesheet": th.Slug, "template": th.Slug,
			"author":         map[string]string{"raw": th.Author, "rendered": `<a href="` + th.AuthorURI + `">` + th.Author + `</a>`},
			"author_uri":     map[string]string{"raw": th.AuthorURI, "rendered": th.AuthorURI},
			"description":    map[string]string{"raw": th.Description, "rendered": th.Description},
			"is_block_theme": false, "name": map[string]string{"raw": th.Name, "rendered": th.Name}, "requires_php": "", "requires_wp": "",
			"screenshot": gweAbsURL(home, th.ScreenshotSrc()), "status": status, "version": th.Version, "textdomain": th.Slug,
			"tags":                   map[string]any{"raw": th.Tags, "rendered": strings.Join(th.Tags, ", ")},
			"theme_uri":              map[string]string{"raw": th.ThemeURI, "rendered": th.ThemeURI},
			"default_template_types": []any{}, "default_template_part_areas": []any{},
			"theme_supports": supports,
			"_links": map[string]any{"self": []map[string]string{{"href": home + "/wp-json/wp/v2/themes/" + th.Slug}},
				"collection": []map[string]string{{"href": home + "/wp-json/wp/v2/themes"}}},
		}
	}
	if slug := r.Params["stylesheet"]; slug != "" {
		th := api.s.installedTheme(slug)
		if th == nil {
			restErr(c, http.StatusNotFound, "rest_theme_not_found", "Theme not found.")
			return
		}
		restJSON(c, http.StatusOK, themeJSON(th))
		return
	}
	var out []any
	for _, th := range api.s.installedThemes() {
		if st := c.Query("status"); st == "active" && th.Slug != active.Slug {
			continue
		}
		out = append(out, themeJSON(th))
	}
	restJSON(c, http.StatusOK, out)
}

func (api *restAPI) search(c *gin.Context, r *restRequest) {
	s := api.s
	page, per := pageArgs(c)
	typ := c.DefaultQuery("type", "post")
	var out []map[string]any
	total := 0
	switch typ {
	case "term":
		tax := c.DefaultQuery("subtype", "category")
		var all []*store.Term
		for _, t := range []string{"category", "post_tag"} {
			if tax != "any" && tax != t && !(tax == "tag" && t == "post_tag") {
				continue
			}
			ts, _, _ := s.store.QueryTerms(c, store.TermQuery{Taxonomy: t, Search: c.Query("search"), PerPage: per, Page: page})
			all = append(all, ts...)
		}
		for _, t := range all {
			out = append(out, map[string]any{"id": t.ID, "title": t.Name, "url": s.links(c).TermLink(t), "type": t.Taxonomy, "subtype": t.Taxonomy})
		}
		total = len(out)
	default:
		types := []string{"post", "page"}
		if st := c.Query("subtype"); st != "" && st != "any" {
			types = strings.Split(st, ",")
		}
		posts, n, _ := s.store.QueryPosts(c, store.PostQuery{Types: types, Statuses: []string{"publish"}, Search: c.Query("search"), PerPage: per, Page: page})
		total = n
		for _, p := range posts {
			out = append(out, map[string]any{"id": p.ID, "title": p.Title, "url": s.links(c).Permalink(p), "type": "post", "subtype": p.Type})
		}
	}
	if out == nil {
		out = []map[string]any{}
	}
	paginate(c, total, per, page)
	restJSON(c, http.StatusOK, out)
}

// Terms ------------------------------------------------------------------------

func (s *Server) restTerm(c *gin.Context, t *store.Term, ctxName string) map[string]any {
	home := s.links(c).Home
	base := "categories"
	if t.Taxonomy == "post_tag" {
		base = "tags"
	}
	out := map[string]any{"id": t.ID, "count": t.Count, "description": t.Description, "link": s.links(c).TermLink(t),
		"name": t.Name, "slug": t.Slug, "taxonomy": t.Taxonomy, "meta": []any{},
		"_links": restLinks(home+"/wp-json/wp/v2/"+base+"/"+itoa64(t.ID), home+"/wp-json/wp/v2/"+base,
			map[string]any{"about": []map[string]string{{"href": home + "/wp-json/wp/v2/taxonomies/" + t.Taxonomy}}})}
	if t.Taxonomy == "category" {
		out["parent"] = t.ParentID
	}
	if ctxName == "embed" {
		return map[string]any{"id": t.ID, "link": out["link"], "name": t.Name, "slug": t.Slug, "taxonomy": t.Taxonomy, "_links": out["_links"]}
	}
	return out
}

func (api *restAPI) registerTerms() {
	for _, base := range []string{"categories", "tags"} {
		tax := "category"
		if base == "tags" {
			tax = "post_tag"
		}
		api.register("wp/v2", "/wp/v2/"+base, map[string]restHandler{
			http.MethodGet:  func(c *gin.Context, r *restRequest) { api.listTerms(c, r, tax) },
			http.MethodPost: func(c *gin.Context, r *restRequest) { api.saveTerm(c, r, tax, 0) },
		}, nil)
		api.register("wp/v2", "/wp/v2/"+base+`/(?P<id>[\d]+)`, map[string]restHandler{
			http.MethodGet: func(c *gin.Context, r *restRequest) {
				t, err := api.s.store.GetTerm(c, r.ID())
				if err != nil || t.Taxonomy != tax {
					restErr(c, http.StatusNotFound, "rest_term_invalid", "Term does not exist.")
					return
				}
				restJSON(c, http.StatusOK, api.s.restTerm(c, t, restContext(c)))
			},
			http.MethodPost:   func(c *gin.Context, r *restRequest) { api.saveTerm(c, r, tax, r.ID()) },
			http.MethodDelete: func(c *gin.Context, r *restRequest) { api.deleteTerm(c, r, tax) },
		}, nil)
	}
}

func (api *restAPI) listTerms(c *gin.Context, r *restRequest, tax string) {
	s := api.s
	page, per := pageArgs(c)
	q := store.TermQuery{Taxonomy: tax, Search: c.Query("search"), OrderBy: c.DefaultQuery("orderby", "name"),
		Order: c.DefaultQuery("order", "asc"), Page: page, PerPage: per, HideEmpty: c.Query("hide_empty") == "true"}
	if ids, ok := r.IDs(c, "include"); ok {
		q.IDs = ids
	}
	var terms []*store.Term
	var total int
	var err error
	if postID := c.Query("post"); postID != "" {
		id, _ := strconv.ParseInt(postID, 10, 64)
		m, _ := s.store.TermsForPosts(c, []int64{id}, tax)
		terms, total = m[id], len(m[id])
	} else if slug := c.Query("slug"); slug != "" {
		if t, err := s.store.TermBySlug(c, tax, slug); err == nil {
			terms, total = []*store.Term{t}, 1
		}
	} else {
		terms, total, err = s.store.QueryTerms(c, q)
		if err != nil {
			restErr(c, http.StatusInternalServerError, "rest_query_error", err.Error())
			return
		}
	}
	if p := c.Query("parent"); p != "" && tax == "category" {
		pid, _ := strconv.ParseInt(p, 10, 64)
		var f []*store.Term
		for _, t := range terms {
			if t.ParentID == pid {
				f = append(f, t)
			}
		}
		terms = f
	}
	out := []map[string]any{}
	for _, t := range terms {
		out = append(out, s.restTerm(c, t, restContext(c)))
	}
	paginate(c, total, per, page)
	restJSON(c, http.StatusOK, out)
}

func (api *restAPI) saveTerm(c *gin.Context, r *restRequest, tax string, id int64) {
	s := api.s
	u := restCurrent(c)
	if !(u.Can("manage_categories", nil) || (tax == "post_tag" && u.Can("edit_posts", nil))) {
		restErr(c, http.StatusForbidden, "rest_cannot_create", "Sorry, you are not allowed to create terms in this taxonomy.")
		return
	}
	t := &store.Term{Taxonomy: tax}
	if id != 0 {
		var err error
		if t, err = s.store.GetTerm(c, id); err != nil || t.Taxonomy != tax {
			restErr(c, http.StatusNotFound, "rest_term_invalid", "Term does not exist.")
			return
		}
	}
	if v, ok := r.Str(c, "name"); ok {
		t.Name = strings.TrimSpace(v)
	}
	if v, ok := r.Str(c, "slug"); ok {
		t.Slug = v
	}
	if v, ok := r.Str(c, "description"); ok {
		t.Description = v
	}
	if v, ok := r.Int(c, "parent"); ok && tax == "category" {
		t.ParentID = v
	}
	var err error
	if id == 0 {
		if t.Name == "" {
			restErr(c, http.StatusBadRequest, "rest_missing_callback_param", "Missing parameter(s): name")
			return
		}
		err = s.store.InsertTerm(c, t)
		if errors.Is(err, store.ErrTermExists) {
			existing, _ := s.store.TermByName(c, tax, t.Name)
			extra := map[string]any{}
			if existing != nil {
				extra["term_id"] = existing.ID
			}
			restErr(c, http.StatusBadRequest, "term_exists", "A term with the name provided already exists with this parent.", extra)
			return
		}
	} else {
		err = s.store.UpdateTerm(c, t)
	}
	if err != nil {
		restErr(c, http.StatusInternalServerError, "rest_term_error", err.Error())
		return
	}
	status := http.StatusOK
	if id == 0 {
		status = http.StatusCreated
	}
	restJSON(c, status, s.restTerm(c, t, "edit"))
}

func (api *restAPI) deleteTerm(c *gin.Context, r *restRequest, tax string) {
	s := api.s
	if !restCurrent(c).Can("manage_categories", nil) {
		restErr(c, http.StatusForbidden, "rest_cannot_delete", "Sorry, you are not allowed to delete this term.")
		return
	}
	t, err := s.store.GetTerm(c, r.ID())
	if err != nil || t.Taxonomy != tax {
		restErr(c, http.StatusNotFound, "rest_term_invalid", "Term does not exist.")
		return
	}
	if tax == "category" && t.ID == int64(s.settings.Int("default_category")) {
		restErr(c, http.StatusForbidden, "rest_cannot_delete", "Sorry, you are not allowed to delete this term.")
		return
	}
	prev := s.restTerm(c, t, "edit")
	_ = s.store.DeleteTerm(c, t, int64(s.settings.Int("default_category")))
	restJSON(c, http.StatusOK, map[string]any{"deleted": true, "previous": prev})
}

// Users ------------------------------------------------------------------------

func (s *Server) restUser(c *gin.Context, u *store.User, ctxName string) map[string]any {
	home := s.links(c).Home
	out := map[string]any{"id": u.ID, "name": u.DisplayName, "url": u.URL, "description": u.Profile["description"],
		"link": s.links(c).AuthorLink(u.ID, u.Nicename), "slug": u.Nicename,
		"avatar_urls": map[string]string{"24": s.avatarURL(c, u.Email, 24), "48": s.avatarURL(c, u.Email, 48), "96": s.avatarURL(c, u.Email, 96)},
		"meta":        map[string]any{"persisted_preferences": orEmptyAny(u.Meta["persisted_preferences"])},
		"_links":      restLinks(home+"/wp-json/wp/v2/users/"+itoa64(u.ID), home+"/wp-json/wp/v2/users", nil)}
	if ctxName == "edit" {
		out["username"] = u.Login
		out["email"] = u.Email
		out["first_name"] = u.Profile["first_name"]
		out["last_name"] = u.Profile["last_name"]
		out["nickname"] = u.Profile["nickname"]
		out["locale"] = defaultStr(u.Locale, "en_US")
		out["registered_date"] = u.RegisteredAt.UTC().Format(time.RFC3339)
		roles := u.Roles
		if roles == nil {
			roles = []string{}
		}
		out["roles"] = roles
		capsOut := map[string]bool{}
		for k, v := range u.Caps {
			capsOut[k] = v
		}
		out["capabilities"] = capsOut
		extra := map[string]bool{}
		for _, r := range u.Roles {
			extra[r] = true
		}
		out["extra_capabilities"] = extra
	}
	if ctxName == "embed" {
		return map[string]any{"id": u.ID, "name": u.DisplayName, "url": u.URL, "description": u.Profile["description"],
			"link": out["link"], "slug": u.Nicename, "avatar_urls": out["avatar_urls"], "_links": out["_links"]}
	}
	return out
}

func orEmptyAny(v any) any {
	if v == nil {
		return map[string]any{}
	}
	return v
}

func (api *restAPI) registerUsers() {
	api.register("wp/v2", "/wp/v2/users", map[string]restHandler{http.MethodGet: api.listUsers, http.MethodPost: api.createUser}, nil)
	api.register("wp/v2", "/wp/v2/users/me", map[string]restHandler{http.MethodGet: api.getMe, http.MethodPost: api.updateMe, http.MethodPut: api.updateMe, http.MethodDelete: api.deleteMe}, nil)
	api.register("wp/v2", `/wp/v2/users/(?P<id>[\d]+)`, map[string]restHandler{
		http.MethodGet:    api.getUser,
		http.MethodPost:   api.updateUser,
		http.MethodPut:    api.updateUser,
		http.MethodDelete: api.deleteUser,
	}, nil)
}

// userForbidden is 401 for visitors, 403 for logged-in users.
func userForbidden(c *gin.Context, code, msg string) { forbidden(c, code, msg) }

func (api *restAPI) getUser(c *gin.Context, r *restRequest) {
	cur := restCurrent(c)
	u, err := api.s.store.UserByID(c, r.ID())
	if err != nil {
		restErr(c, http.StatusNotFound, "rest_user_invalid_id", "Invalid user ID.")
		return
	}
	ctxName := restContext(c)
	self := cur != nil && cur.ID == u.ID
	if ctxName == "edit" && !self && !(cur != nil && cur.Can("list_users", nil)) {
		userForbidden(c, "rest_user_cannot_view", "Sorry, you are not allowed to list users.")
		return
	}
	if !self && !(cur != nil && cur.Can("list_users", nil)) {
		// Only users with published content are public.
		n, _ := api.s.store.UserPostCounts(c, []int64{u.ID})
		if n[u.ID] == 0 {
			var pages int
			_ = api.s.store.Pool.QueryRow(c, `SELECT count(*) FROM posts WHERE author_id = $1 AND status = 'publish' AND type IN ('post','page')`, u.ID).Scan(&pages)
			if pages == 0 {
				userForbidden(c, "rest_user_cannot_view", "Sorry, you are not allowed to list users.")
				return
			}
		}
	}
	restJSON(c, http.StatusOK, api.s.restUser(c, u, ctxName))
}

func (api *restAPI) listUsers(c *gin.Context, r *restRequest) {
	s := api.s
	cur := restCurrent(c)
	page, per := pageArgs(c)
	ctxName := restContext(c)
	canList := cur != nil && cur.Can("list_users", nil)
	orderby := c.DefaultQuery("orderby", "name")
	if !canList {
		if ctxName == "edit" {
			userForbidden(c, "rest_forbidden_context", "Sorry, you are not allowed to list users.")
			return
		}
		if orderby == "email" || orderby == "registered_date" {
			userForbidden(c, "rest_forbidden_orderby", "Sorry, you are not allowed to order users by this parameter.")
			return
		}
		if c.Query("who") == "authors" && !(cur != nil && cur.Can("edit_posts", nil)) {
			userForbidden(c, "rest_forbidden_who", "Sorry, you are not allowed to query users by this parameter.")
			return
		}
		if c.Query("roles") != "" || c.Query("capabilities") != "" {
			userForbidden(c, "rest_user_cannot_view", "Sorry, you are not allowed to filter users by role.")
			return
		}
	}
	q := store.UserQuery{Search: strings.TrimSpace(c.Query("search")), Page: page, PerPage: per, Order: c.DefaultQuery("order", "asc")}
	switch orderby {
	case "id", "include":
		q.OrderBy = orderby
	case "name":
		q.OrderBy = "name"
	case "registered_date":
		q.OrderBy = "registered"
	case "slug", "include_slugs":
		q.OrderBy = "slug"
	case "email", "url":
		q.OrderBy = orderby
	}
	if ids, ok := r.IDs(c, "include"); ok {
		q.IDs = ids
	}
	if ids, ok := r.IDs(c, "exclude"); ok {
		q.Exclude = ids
	}
	if roles := c.Query("roles"); roles != "" {
		q.Roles = strings.Split(roles, ",")
	}
	if slugs := c.QueryArray("slug[]"); len(slugs) > 0 {
		q.Nicenames = slugs
	} else if slug := c.Query("slug"); slug != "" {
		q.Nicenames = strings.Split(slug, ",")
	}
	caps := strings.Split(c.Query("capabilities"), ",")
	if c.Query("who") == "authors" {
		caps = append(caps, "edit_posts")
	}
	if !canList {
		q.HasPublishedPosts = true
	}
	var wantCaps []string
	for _, cp := range caps {
		if cp = strings.TrimSpace(cp); cp != "" {
			wantCaps = append(wantCaps, cp)
		}
	}
	if len(wantCaps) > 0 {
		// Capabilities are resolved per user (roles + exceptions): fetch everything matching the rest and filter here.
		q.Page, q.PerPage = 1, 100000
	}
	if offset, _ := strconv.Atoi(c.Query("offset")); offset > 0 && len(wantCaps) == 0 {
		q.Page, q.PerPage = 1, 100000
	}
	users, total, err := s.store.QueryUsers(c, q)
	if err != nil {
		restErr(c, http.StatusInternalServerError, "rest_query_error", err.Error())
		return
	}
	if len(wantCaps) > 0 || q.PerPage == 100000 {
		var kept []*store.User
		for _, u := range users {
			ok := true
			for _, cp := range wantCaps {
				if !u.Can(cp, nil) {
					ok = false
				}
			}
			if ok {
				kept = append(kept, u)
			}
		}
		total = len(kept)
		offset, _ := strconv.Atoi(c.Query("offset"))
		if offset <= 0 {
			offset = (page - 1) * per
		}
		users = nil
		for i, u := range kept {
			if i >= offset && i < offset+per {
				users = append(users, u)
			}
		}
	}
	out := []map[string]any{}
	for _, u := range users {
		out = append(out, s.restUser(c, u, ctxName))
	}
	paginate(c, total, per, page)
	restJSON(c, http.StatusOK, out)
}

func (api *restAPI) getMe(c *gin.Context, r *restRequest) {
	u := restCurrent(c)
	if u == nil {
		restErr(c, http.StatusUnauthorized, "rest_not_logged_in", "You are not currently logged in.")
		return
	}
	restJSON(c, http.StatusOK, api.s.restUser(c, u, restContext(c)))
}

// updateMe is /users/me: the current user's own profile (and the editor's persisted preferences).
func (api *restAPI) updateMe(c *gin.Context, r *restRequest) {
	u := restCurrent(c)
	if u == nil {
		restErr(c, http.StatusUnauthorized, "rest_not_logged_in", "You are not currently logged in.")
		return
	}
	api.saveUser(c, r, u)
}

func (api *restAPI) updateUser(c *gin.Context, r *restRequest) {
	u, err := api.s.store.UserByID(c, r.ID())
	if err != nil {
		restErr(c, http.StatusNotFound, "rest_user_invalid_id", "Invalid user ID.")
		return
	}
	api.saveUser(c, r, u)
}

// restRoles reads the roles parameter (a list or a comma string).
func (r *restRequest) roles(c *gin.Context) ([]string, bool) {
	if v, ok := r.Body["roles"]; ok {
		switch t := v.(type) {
		case []any:
			var out []string
			for _, x := range t {
				if sv, ok := x.(string); ok && sv != "" {
					out = append(out, sv)
				}
			}
			return out, true
		case string:
			return strings.Split(t, ","), true
		}
	}
	if v, ok := c.GetPostForm("roles"); ok {
		return strings.Split(v, ","), true
	}
	if vs := c.PostFormArray("roles[]"); len(vs) > 0 {
		return vs, true
	}
	return nil, false
}

// checkRoleUpdate is check_role_update(): every role must exist and the editor must be able to promote users
// without stripping their own administrator role.
func (api *restAPI) checkRoleUpdate(c *gin.Context, target *store.User, roles []string) bool {
	cur := restCurrent(c)
	known, _ := api.s.store.Roles(c)
	for _, want := range roles {
		found := false
		for _, kr := range known {
			if kr.Slug == want {
				found = true
			}
		}
		if !found {
			restErr(c, http.StatusBadRequest, "rest_user_invalid_role", "The role "+want+" does not exist.")
			return false
		}
		if !cur.Can("promote_users", nil) {
			restErr(c, http.StatusForbidden, "rest_cannot_edit_roles", "Sorry, you are not allowed to give users that role.")
			return false
		}
	}
	if target != nil && target.ID == cur.ID && cur.Can("manage_options", nil) {
		keeps := false
		for _, want := range roles {
			if want == "administrator" {
				keeps = true
			}
		}
		if !keeps {
			restErr(c, http.StatusForbidden, "rest_user_invalid_role", "Sorry, you are not allowed to give users that role.")
			return false
		}
	}
	return true
}

// saveUser is update_item(): profile fields, password, slug, roles and the editor's persisted preferences.
func (api *restAPI) saveUser(c *gin.Context, r *restRequest, u *store.User) {
	s := api.s
	cur := restCurrent(c)
	if cur == nil {
		restErr(c, http.StatusUnauthorized, "rest_cannot_edit", "Sorry, you are not allowed to edit this user.")
		return
	}
	if cur.ID != u.ID && !cur.Can("edit_users", nil) {
		restErr(c, http.StatusForbidden, "rest_cannot_edit", "Sorry, you are not allowed to edit this user.")
		return
	}
	if v, ok := r.Str(c, "username"); ok && v != u.Login {
		restErr(c, http.StatusBadRequest, "rest_user_invalid_argument", "Username is not editable.")
		return
	}
	if v, ok := r.Str(c, "email"); ok {
		if !validEmail(v) {
			restErr(c, http.StatusBadRequest, "rest_user_invalid_email", "Invalid email address.")
			return
		}
		if !strings.EqualFold(v, u.Email) {
			if _, taken, _ := s.store.LoginOrEmailTaken(c, "", v); taken {
				restErr(c, http.StatusBadRequest, "rest_user_invalid_email", "Invalid email address.")
				return
			}
		}
		u.Email = v
	}
	if v, ok := r.Str(c, "slug"); ok && v != u.Nicename {
		slug := wp.SanitizeTitle(v)
		if taken, _ := s.store.NicenameTaken(c, slug, u.ID); taken || slug == "" {
			restErr(c, http.StatusBadRequest, "rest_user_invalid_slug", "Invalid slug.")
			return
		}
		u.Nicename = slug
	}
	roles, hasRoles := r.roles(c)
	if hasRoles && !api.checkRoleUpdate(c, u, roles) {
		return
	}
	if v, ok := r.Str(c, "password"); ok {
		if v == "" {
			restErr(c, http.StatusBadRequest, "rest_user_invalid_password", "Passwords cannot be empty.")
			return
		}
		if strings.Contains(v, "\\") {
			restErr(c, http.StatusBadRequest, "rest_user_invalid_password", "Passwords cannot contain the \"\\\" character.")
			return
		}
		if hash, err := auth.HashPassword(v); err == nil {
			_ = s.store.SetUserPassword(c, u.ID, hash)
		}
	}
	for field, dst := range map[string]*string{"name": &u.DisplayName, "url": &u.URL, "locale": &u.Locale} {
		if v, ok := r.Str(c, field); ok {
			*dst = v
		}
	}
	for _, field := range []string{"first_name", "last_name", "nickname", "description"} {
		if v, ok := r.Str(c, field); ok {
			u.Profile[field] = v
		}
	}
	if err := s.store.UpdateUserFields(c, u); err != nil {
		restErr(c, http.StatusInternalServerError, "rest_user_update_error", err.Error())
		return
	}
	if hasRoles {
		var extra []string
		primary := ""
		if len(roles) > 0 {
			primary, extra = roles[0], roles[1:]
		}
		_ = s.store.SetUserRoles(c, u.ID, primary, extra)
	}
	if meta, ok := r.Body["meta"].(map[string]any); ok {
		if prefs, ok := meta["persisted_preferences"]; ok {
			raw, _ := json.Marshal(map[string]any{"persisted_preferences": prefs})
			if _, err := s.store.Pool.Exec(c, `UPDATE users SET meta = meta || $2::jsonb WHERE id = $1`, u.ID, raw); err != nil {
				restErr(c, http.StatusInternalServerError, "rest_user_error", err.Error())
				return
			}
		}
	}
	s.purgeAll(c)
	u, _ = s.store.UserByID(c, u.ID)
	restJSON(c, http.StatusOK, s.restUser(c, u, "edit"))
}

// createUser is create_item(): POST /wp/v2/users.
func (api *restAPI) createUser(c *gin.Context, r *restRequest) {
	s := api.s
	cur := restCurrent(c)
	if cur == nil || !cur.Can("create_users", nil) {
		userForbidden(c, "rest_cannot_create_user", "Sorry, you are not allowed to create new users.")
		return
	}
	if id, _ := r.Int(c, "id"); id != 0 {
		restErr(c, http.StatusBadRequest, "rest_user_exists", "Cannot create existing user.")
		return
	}
	login, _ := r.Str(c, "username")
	email, _ := r.Str(c, "email")
	pass, _ := r.Str(c, "password")
	var missing []string
	for _, f := range []string{"username", "email", "password"} {
		if _, ok := r.get(c, f); !ok {
			missing = append(missing, f)
		}
	}
	if len(missing) > 0 {
		restErr(c, http.StatusBadRequest, "rest_missing_callback_param", "Missing parameter(s): "+strings.Join(missing, ", "), map[string]any{"params": missing})
		return
	}
	login = strings.TrimSpace(login)
	switch {
	case login == "" || !validUsername.MatchString(login):
		restErr(c, http.StatusBadRequest, "rest_user_invalid_username", "Username contains invalid characters.")
		return
	case !validEmail(email):
		restErr(c, http.StatusBadRequest, "rest_user_invalid_email", "Invalid email address.")
		return
	case pass == "":
		restErr(c, http.StatusBadRequest, "rest_user_invalid_password", "Passwords cannot be empty.")
		return
	case strings.Contains(pass, "\\"):
		restErr(c, http.StatusBadRequest, "rest_user_invalid_password", "Passwords cannot contain the \"\\\" character.")
		return
	}
	if lt, et, _ := s.store.LoginOrEmailTaken(c, login, email); lt {
		restErr(c, http.StatusBadRequest, "existing_user_login", "Sorry, that username already exists!")
		return
	} else if et {
		restErr(c, http.StatusBadRequest, "existing_user_email", "Sorry, that email address is already used!")
		return
	}
	roles, hasRoles := r.roles(c)
	if hasRoles && !api.checkRoleUpdate(c, nil, roles) {
		return
	}
	if !hasRoles || len(roles) == 0 {
		roles = []string{s.settings.String("default_role")}
	}
	hash, err := auth.HashPassword(pass)
	if err != nil {
		restErr(c, http.StatusInternalServerError, "rest_user_create", err.Error())
		return
	}
	profile := map[string]string{}
	for _, field := range []string{"first_name", "last_name", "nickname", "description"} {
		if v, ok := r.Str(c, field); ok {
			profile[field] = v
		}
	}
	name, _ := r.Str(c, "name")
	id, err := s.store.CreateUser(c, store.NewUser{Login: login, Email: email, PasswordHash: hash, DisplayName: name, Role: roles[0], Profile: profile})
	if err != nil {
		restErr(c, http.StatusInternalServerError, "rest_user_create", "Error creating new user.")
		return
	}
	if len(roles) > 1 {
		_ = s.store.SetUserRoles(c, id, roles[0], roles[1:])
	}
	u, _ := s.store.UserByID(c, id)
	changed := false
	for field, dst := range map[string]*string{"url": &u.URL, "locale": &u.Locale} {
		if v, ok := r.Str(c, field); ok {
			*dst, changed = v, true
		}
	}
	if v, ok := r.Str(c, "slug"); ok && v != "" {
		u.Nicename, changed = wp.SanitizeTitle(v), true
	}
	if changed {
		_ = s.store.UpdateUserFields(c, u)
		u, _ = s.store.UserByID(c, id)
	}
	c.Header("Location", s.links(c).HomeURL("/wp-json/wp/v2/users/"+itoa64(u.ID)))
	restJSON(c, http.StatusCreated, s.restUser(c, u, "edit"))
}

func (api *restAPI) deleteMe(c *gin.Context, r *restRequest) {
	u := restCurrent(c)
	if u == nil {
		restErr(c, http.StatusUnauthorized, "rest_not_logged_in", "You are not currently logged in.")
		return
	}
	api.removeUser(c, r, u)
}

func (api *restAPI) deleteUser(c *gin.Context, r *restRequest) {
	u, err := api.s.store.UserByID(c, r.ID())
	if err != nil {
		restErr(c, http.StatusNotFound, "rest_user_invalid_id", "Invalid user ID.")
		return
	}
	api.removeUser(c, r, u)
}

// removeUser is delete_item(): force=true is required (users cannot be trashed); reassign moves the
// user's content to another user, otherwise it is deleted (wp_delete_user()).
func (api *restAPI) removeUser(c *gin.Context, r *restRequest, u *store.User) {
	s := api.s
	cur := restCurrent(c)
	if cur == nil || !cur.Can("delete_users", nil) {
		userForbidden(c, "rest_user_cannot_delete", "Sorry, you are not allowed to delete this user.")
		return
	}
	if force, _ := r.Bool(c, "force"); !force {
		restErr(c, http.StatusNotImplemented, "rest_trash_not_supported", "Users do not support trashing. Set 'force=true' to delete.")
		return
	}
	var reassign int64
	if v, ok := r.get(c, "reassign"); ok && v != "" && v != nil {
		id, valid := r.Int(c, "reassign")
		if !valid || id == u.ID {
			restErr(c, http.StatusBadRequest, "rest_user_invalid_reassign", "Invalid user ID for reassignment.")
			return
		}
		if id != 0 {
			if _, err := s.store.UserByID(c, id); err != nil {
				restErr(c, http.StatusBadRequest, "rest_user_invalid_reassign", "Invalid user ID for reassignment.")
				return
			}
		}
		reassign = id
	}
	prev := s.restUser(c, u, "edit")
	if err := s.store.DeleteUser(c, u.ID, reassign); err != nil {
		restErr(c, http.StatusInternalServerError, "rest_cannot_delete", "The user cannot be deleted.")
		return
	}
	s.purgeAll(c)
	restJSON(c, http.StatusOK, map[string]any{"deleted": true, "previous": prev})
}

// Media ------------------------------------------------------------------------

func (s *Server) restMedia(c *gin.Context, a *store.Attachment, ctxName string) map[string]any {
	home := s.links(c).Home
	loc := s.loc()
	url := s.uploadURL(c, a.FilePath)
	parts := strings.SplitN(a.Mime, "/", 2)
	mediaType := "file"
	if parts[0] == "image" {
		mediaType = "image"
	}
	sizes := map[string]any{}
	for _, v := range a.Variants {
		sizes[v.Name] = map[string]any{"file": path.Base(v.FilePath), "width": v.Width, "height": v.Height, "filesize": v.FileSize,
			"mime_type": v.Mime, "source_url": s.uploadURL(c, v.FilePath)}
	}
	if mediaType == "image" {
		sizes["full"] = map[string]any{"file": path.Base(a.FilePath), "width": a.Width, "height": a.Height, "mime_type": a.Mime, "source_url": url}
	}
	details := map[string]any{"width": a.Width, "height": a.Height, "file": a.FilePath, "filesize": a.FileSize, "sizes": sizes,
		"image_meta": map[string]any{"aperture": "0", "credit": "", "camera": "", "caption": "", "created_timestamp": "0", "copyright": "",
			"focal_length": "0", "iso": "0", "shutter_speed": "0", "title": "", "orientation": "0", "keywords": []any{}}}
	if orig, ok := a.Meta["original_image"].(string); ok {
		details["original_image"] = orig
	}
	out := map[string]any{
		"id": a.ID, "date": a.CreatedAt.In(loc).Format("2006-01-02T15:04:05"), "date_gmt": gmt(a.CreatedAt),
		"guid":     map[string]any{"rendered": url, "raw": url},
		"modified": a.UpdatedAt.In(loc).Format("2006-01-02T15:04:05"), "modified_gmt": gmt(a.UpdatedAt),
		"slug": a.Slug, "status": "inherit", "type": "attachment", "link": home + "/?attachment_id=" + itoa64(a.ID),
		"title":  map[string]any{"rendered": wp.EscText(a.Title), "raw": a.Title},
		"author": a.UploaderID, "featured_media": 0, "comment_status": "open", "ping_status": "closed", "template": "", "meta": []any{},
		"description": map[string]any{"rendered": wp.Autop(a.Description), "raw": a.Description},
		"caption":     map[string]any{"rendered": wp.Autop(a.Caption), "raw": a.Caption},
		"alt_text":    a.Alt, "media_type": mediaType, "mime_type": a.Mime, "media_details": details, "post": nullIntJSON(a.PostID),
		"source_url": url, "missing_image_sizes": []any{},
		"_links": restLinks(home+"/wp-json/wp/v2/media/"+itoa64(a.ID), home+"/wp-json/wp/v2/media", nil),
	}
	if ctxName == "embed" {
		return map[string]any{"id": a.ID, "date": out["date"], "slug": a.Slug, "type": "attachment", "link": out["link"], "title": out["title"],
			"author": a.UploaderID, "caption": out["caption"], "alt_text": a.Alt, "media_type": mediaType, "mime_type": a.Mime,
			"media_details": details, "source_url": url, "_links": out["_links"]}
	}
	return out
}

func nullIntJSON(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

func (api *restAPI) registerMedia() {
	api.register("wp/v2", "/wp/v2/media", map[string]restHandler{http.MethodGet: api.listMedia, http.MethodPost: api.uploadMedia}, nil)
	api.register("wp/v2", `/wp/v2/media/(?P<id>[\d]+)`, map[string]restHandler{
		http.MethodGet: func(c *gin.Context, r *restRequest) {
			a, err := api.s.store.GetAttachment(c, r.ID())
			if err != nil {
				restErr(c, http.StatusNotFound, "rest_post_invalid_id", "Invalid post ID.")
				return
			}
			restJSON(c, http.StatusOK, api.s.restMedia(c, a, restContext(c)))
		},
		http.MethodPost:   api.updateMedia,
		http.MethodDelete: api.deleteMedia,
	}, nil)
	api.register("wp/v2", `/wp/v2/media/(?P<id>[\d]+)/post-process`, map[string]restHandler{http.MethodPost: func(c *gin.Context, r *restRequest) {
		a, err := api.s.store.GetAttachment(c, r.ID())
		if err != nil {
			restErr(c, http.StatusNotFound, "rest_post_invalid_id", "Invalid post ID.")
			return
		}
		restJSON(c, http.StatusOK, api.s.restMedia(c, a, "edit"))
	}}, nil)
}

func (api *restAPI) listMedia(c *gin.Context, r *restRequest) {
	s := api.s
	page, per := pageArgs(c)
	q := store.AttachmentQuery{Search: c.Query("search"), Page: page, PerPage: per, OrderBy: c.DefaultQuery("orderby", "date"), Order: c.DefaultQuery("order", "desc")}
	if mt := c.Query("media_type"); mt != "" {
		if mt == "application" || mt == "text" || mt == "image" || mt == "audio" || mt == "video" {
			q.MimePrefix = []string{mt}
		}
	}
	if mt := c.Query("mime_type"); mt != "" {
		q.MimePrefix = []string{mt}
	}
	if ids, ok := r.IDs(c, "include"); ok {
		q.IDs = ids
	}
	if p := c.Query("parent"); p != "" {
		id, _ := strconv.ParseInt(p, 10, 64)
		q.PostID = &id
	}
	list, total, err := s.store.QueryAttachments(c, q)
	if err != nil {
		restErr(c, http.StatusInternalServerError, "rest_query_error", err.Error())
		return
	}
	out := []map[string]any{}
	for _, a := range list {
		out = append(out, s.restMedia(c, a, restContext(c)))
	}
	paginate(c, total, per, page)
	restJSON(c, http.StatusOK, out)
}

// uploadMedia handles multipart uploads (field "file") from the block editor.
func (api *restAPI) uploadMedia(c *gin.Context, r *restRequest) {
	s := api.s
	u := restCurrent(c)
	if !u.Can("upload_files", nil) {
		restErr(c, http.StatusForbidden, "rest_cannot_create", "Sorry, you are not allowed to upload media on this site.")
		return
	}
	postID, _ := strconv.ParseInt(c.PostForm("post"), 10, 64)
	a, err := s.saveUpload(c, "file", postID)
	if err != nil {
		code := "rest_upload_unknown_error"
		if strings.Contains(err.Error(), "file type") {
			code = "rest_upload_sideload_error"
		}
		restErr(c, http.StatusInternalServerError, code, err.Error())
		return
	}
	changed := false
	for field, dst := range map[string]*string{"title": &a.Title, "caption": &a.Caption, "description": &a.Description, "alt_text": &a.Alt} {
		if v, ok := c.GetPostForm(field); ok && v != "" {
			*dst = v
			changed = true
		}
	}
	if changed {
		_ = s.store.UpdateAttachment(c, a)
	}
	c.Header("Location", s.links(c).HomeURL("/wp-json/wp/v2/media/"+itoa64(a.ID)))
	restJSON(c, http.StatusCreated, s.restMedia(c, a, "edit"))
}

func (api *restAPI) updateMedia(c *gin.Context, r *restRequest) {
	s := api.s
	u := restCurrent(c)
	a, err := s.store.GetAttachment(c, r.ID())
	if err != nil {
		restErr(c, http.StatusNotFound, "rest_post_invalid_id", "Invalid post ID.")
		return
	}
	if a.UploaderID != u.ID && !u.Can("edit_others_posts", nil) {
		restErr(c, http.StatusForbidden, "rest_cannot_edit", "Sorry, you are not allowed to edit this post.")
		return
	}
	for field, dst := range map[string]*string{"title": &a.Title, "caption": &a.Caption, "description": &a.Description, "alt_text": &a.Alt, "slug": &a.Slug} {
		if v, ok := r.Str(c, field); ok {
			*dst = v
		}
	}
	if v, ok := r.Int(c, "post"); ok {
		a.PostID = v
	}
	if err := s.store.UpdateAttachment(c, a); err != nil {
		restErr(c, http.StatusInternalServerError, "rest_cannot_edit", err.Error())
		return
	}
	restJSON(c, http.StatusOK, s.restMedia(c, a, "edit"))
}

func (api *restAPI) deleteMedia(c *gin.Context, r *restRequest) {
	s := api.s
	u := restCurrent(c)
	a, err := s.store.GetAttachment(c, r.ID())
	if err != nil {
		restErr(c, http.StatusNotFound, "rest_post_invalid_id", "Invalid post ID.")
		return
	}
	if a.UploaderID != u.ID && !u.Can("delete_others_posts", nil) {
		restErr(c, http.StatusForbidden, "rest_cannot_delete", "Sorry, you are not allowed to delete this post.")
		return
	}
	if force, _ := r.Bool(c, "force"); !force {
		restErr(c, http.StatusNotImplemented, "rest_trash_not_supported", "The post does not support trashing. Set 'force=true' to delete.")
		return
	}
	prev := s.restMedia(c, a, "edit")
	s.deleteAttachmentFiles(a)
	_ = s.store.DeleteAttachment(c, a.ID)
	restJSON(c, http.StatusOK, map[string]any{"deleted": true, "previous": prev})
}
