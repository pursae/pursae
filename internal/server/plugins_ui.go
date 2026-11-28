package server

import (
	"context"
	"encoding/json"
	"html"
	"html/template"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"wpgo/internal/blocks"
	"wpgo/internal/plugins"
	"wpgo/internal/store"
	"wpgo/internal/wp"
)

// The admin and editor surfaces Wasm plugins register (internal/plugins/ui.go): admin pages
// (admin?page=<slug>[.<sub>]), Dashboard widgets, meta boxes on the classic editor and Pursae
// Editor, dynamic blocks and Pursae Web Builder widgets. The plugin returns HTML; the runtime
// sanitises it (KsesAdmin in the admin, KSES on the front end) unless the plugin holds
// admin:scripts / content:html.

func (s *Server) pluginRT() *plugins.Runtime {
	if s.plugins == nil {
		return nil
	}
	return s.plugins.rt
}

// pluginUICtx bounds a render callback that runs while a page is being built.
func pluginUICtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
}

// Admin pages --------------------------------------------------------------------

// pluginUIMenu adds the plugins' admin pages to the menu: top-level pages at the end, sub pages
// under their plugin's top-level page or a core menu.
func (s *Server) pluginUIMenu(menu []*menuTop, u *store.User, screen string) []*menuTop {
	rt := s.pluginRT()
	if rt == nil || u == nil {
		return menu
	}
	pages := rt.AdminPages()
	tops := map[string]*menuTop{} // plugin slug -> its top-level entry
	for _, a := range pages {
		if a.Page.Parent != "" || !u.Can(a.Page.Capability, nil) {
			continue
		}
		slug := "admin?page=" + a.ID()
		icon := a.Page.Icon
		if icon == "" {
			icon = "dashicons-admin-plugins"
		}
		top := &menuTop{Slug: slug, Title: template.HTML(html.EscapeString(a.Page.MenuTitle)), ID: "toplevel_page_" + a.ID(), Icon: icon, Current: screen == slug}
		menu = append(menu, top)
		if a.Page.Slug == "" {
			tops[a.Plugin.Slug()] = top
		}
	}
	for _, a := range pages {
		if a.Page.Parent == "" || !u.Can(a.Page.Capability, nil) {
			continue
		}
		var top *menuTop
		if a.Page.Parent == "self" {
			top = tops[a.Plugin.Slug()]
		} else {
			for _, m := range menu {
				if m.ID == plugins.AdminPageParents[a.Page.Parent] {
					top = m
				}
			}
		}
		if top == nil {
			continue
		}
		if a.Page.Parent == "self" && len(top.Sub) == 0 { // add_submenu_page(): the main page is the first item
			top.Sub = append(top.Sub, &menuSub{Slug: top.Slug, Title: top.Title, First: true, Current: top.Current})
		}
		slug := "admin?page=" + a.ID()
		sub := &menuSub{Slug: slug, Title: template.HTML(html.EscapeString(a.Page.MenuTitle)), Current: screen == slug}
		if sub.Current {
			top.Current = true
		}
		top.Sub = append(top.Sub, sub)
	}
	return menu
}

type pluginPageData struct {
	Title, Nonce, Action string
	Body                 template.HTML
}

// wasmAdminPage serves admin?page=<id> for a plugin admin page; false when no plugin owns id.
func (s *Server) wasmAdminPage(c *gin.Context, id string) bool {
	rt := s.pluginRT()
	if rt == nil || id == "" {
		return false
	}
	a, ok := rt.FindAdminPage(id)
	if !ok {
		return false
	}
	u := currentUser(c)
	if u == nil || !u.Can(a.Page.Capability, nil) {
		c.String(http.StatusForbidden, "Sorry, you are not allowed to access this page.")
		return true
	}
	action := "plugin-page_" + id
	req := plugins.AdminRequest{Method: http.MethodGet, Query: map[string]string{}, Form: map[string]string{}, User: userInfo(u)}
	for k, v := range c.Request.URL.Query() {
		if len(v) > 0 && len(req.Query) < 100 {
			req.Query[k] = v[0]
		}
	}
	if c.Request.Method == http.MethodPost {
		if !s.checkNonce(c, action) {
			s.denyNonce(c)
			return true
		}
		req.Method = http.MethodPost
		_ = c.Request.ParseForm()
		for k, v := range c.Request.PostForm {
			if k == "_wpnonce" || k == "_wp_http_referer" || len(v) == 0 || len(req.Form) >= 500 {
				continue
			}
			req.Form[k] = v[0]
		}
	}
	ctx, cancel := pluginUICtx(c.Request.Context())
	defer cancel()
	resp, err := a.Plugin.RenderAdminPage(ctx, a.Page.Slug, req)
	var notices []notice
	d := pluginPageData{Title: a.Page.PageTitle, Nonce: s.nonce(c, action), Action: "/wp-admin/admin?page=" + id}
	if err != nil {
		notices = append(notices, notice{Type: "error", Message: template.HTML(html.EscapeString("The plugin " + a.Plugin.Slug() + " could not show this page."))})
	} else {
		d.Body = template.HTML(resp.HTML)
		for _, n := range resp.Notices {
			notices = append(notices, notice{Type: n.Type, Message: template.HTML(html.EscapeString(n.Message)), Dismissible: true})
		}
	}
	s.adminPage(c, "admin?page="+id, a.Page.PageTitle, "toplevel_page_"+id, "admin/plugin-page", d, notices...)
	return true
}

// Dashboard widgets --------------------------------------------------------------

// pluginDashboardWidgets renders the plugins' Dashboard widgets the user may see, as complete
// postboxes by id ("plugin_<slug>_<id>").
func (s *Server) pluginDashboardWidgets(c *gin.Context, u *store.User, closed map[string]bool) (ids []string, boxes map[string]template.HTML) {
	rt := s.pluginRT()
	if rt == nil {
		return nil, nil
	}
	boxes = map[string]template.HTML{}
	ctx, cancel := pluginUICtx(c.Request.Context())
	defer cancel()
	for _, w := range rt.DashboardWidgets() {
		if !u.Can(w.Widget.Capability, nil) {
			continue
		}
		id := "plugin_" + strings.ReplaceAll(w.Plugin.Slug(), "-", "_") + "_" + strings.ReplaceAll(w.Widget.ID, "-", "_")
		out, err := w.Plugin.RenderDashboardWidget(ctx, w.Widget.ID, userInfo(u))
		if err != nil {
			out = `<p>` + html.EscapeString("The plugin "+w.Plugin.Slug()+" could not show this widget.") + `</p>`
		}
		ids = append(ids, id)
		boxes[id] = template.HTML(dashboardPostbox(id, w.Widget.Title, out, closed[id]))
	}
	return ids, boxes
}

// dashboardPostbox is the markup of a Dashboard box (dashboard-postbox-start/end in dashboard.html).
func dashboardPostbox(id, title, inner string, closed bool) string {
	i, t := html.EscapeString(id), html.EscapeString(title)
	cl, exp := "", "true"
	if closed {
		cl, exp = " closed", "false"
	}
	return `<div id="` + i + `" class="postbox` + cl + `">
<div class="postbox-header"><h2 class="hndle ui-sortable-handle">` + t + `</h2>
<div class="handle-actions hide-if-no-js"><button type="button" class="handle-order-higher" aria-disabled="false" aria-describedby="` + i + `-handle-order-higher-description"><span class="screen-reader-text">Move up</span><span class="order-higher-indicator" aria-hidden="true"></span></button><span class="hidden" id="` + i + `-handle-order-higher-description">Move ` + t + ` box up</span><button type="button" class="handle-order-lower" aria-disabled="false" aria-describedby="` + i + `-handle-order-lower-description"><span class="screen-reader-text">Move down</span><span class="order-lower-indicator" aria-hidden="true"></span></button><span class="hidden" id="` + i + `-handle-order-lower-description">Move ` + t + ` box down</span><button type="button" class="handlediv" aria-expanded="` + exp + `"><span class="screen-reader-text">Toggle panel: ` + t + `</span><span class="toggle-indicator" aria-hidden="true"></span></button></div></div>
<div class="inside">
` + inner + `
</div>
</div>
`
}

// Meta boxes ----------------------------------------------------------------------

// pluginMetaBoxField is the marker input every rendered plugin meta box carries, so a save only
// touches boxes that were on the screen (an absent checkbox then means "off").
const pluginMetaBoxField = "wpgo_plugin_meta_boxes[]"

func pluginMetaBoxKey(pb plugins.PluginMetaBox) string { return pb.Plugin.Slug() + ":" + pb.Box.ID }

func pluginMetaBoxDOMID(pb plugins.PluginMetaBox) string {
	return "plugin_" + strings.ReplaceAll(pb.Plugin.Slug(), "-", "_") + "_" + strings.ReplaceAll(pb.Box.ID, "-", "_")
}

// pluginMetaBoxValues reads the stored values of a box's fields from the post, by field id.
func pluginMetaBoxValues(pb plugins.PluginMetaBox, p *store.Post) map[string]any {
	vals := map[string]any{}
	for _, f := range pb.Box.Fields {
		if v, ok := p.Meta[f.MetaKey(pb.Plugin.Slug())]; ok {
			vals[f.ID] = v
		}
	}
	return vals
}

// pluginMetaBoxesFor returns the boxes on p's edit screen the user may use (edit_post on p).
func (s *Server) pluginMetaBoxesFor(u *store.User, p *store.Post) []plugins.PluginMetaBox {
	rt := s.pluginRT()
	if rt == nil || p == nil || u == nil || !pluginPostTypes[p.Type] || !u.Can("edit_post", p) {
		return nil
	}
	return rt.MetaBoxes(p.Type)
}

// pluginMetaBoxInner renders a box's body: the plugin's own HTML (no form controls: KsesPost, so
// it can never add inputs to the post form) and the core-rendered fields.
func (s *Server) pluginMetaBoxInner(ctx context.Context, pb plugins.PluginMetaBox, p *store.Post, u *store.User) string {
	var b strings.Builder
	b.WriteString(`<input type="hidden" name="` + pluginMetaBoxField + `" value="` + html.EscapeString(pluginMetaBoxKey(pb)) + `" />`)
	vals := pluginMetaBoxValues(pb, p)
	if pb.Box.Callback != "" {
		out, err := pb.Plugin.RenderMetaBox(ctx, pb.Box.ID, p.ID, vals, userInfo(u))
		if err != nil {
			out = "<p>" + html.EscapeString("The plugin "+pb.Plugin.Slug()+" could not show this box.") + "</p>"
		} else if !pb.Plugin.HasPermission(plugins.PermAdminScripts) {
			out = wp.KsesPost(out)
		}
		b.WriteString(out)
	}
	if len(pb.Box.Fields) > 0 {
		b.WriteString(`<table class="form-table" role="presentation">`)
		for _, f := range pb.Box.Fields {
			var stored any
			if v, ok := vals[f.ID]; ok {
				stored = v
			}
			b.WriteString(pluginFieldRow(f.MetaKey(pb.Plugin.Slug()), f, stored))
		}
		b.WriteString(`</table>`)
	}
	return b.String()
}

// pluginMetaBoxesClassic renders the boxes of the classic editor for one context (normal, side).
func (s *Server) pluginMetaBoxesClassic(c *gin.Context, p *store.Post, where string) template.HTML {
	u := currentUser(c)
	boxes := s.pluginMetaBoxesFor(u, p)
	if len(boxes) == 0 {
		return ""
	}
	s.pluginMetaBoxMediaJS(c, boxes)
	ctx, cancel := pluginUICtx(c.Request.Context())
	defer cancel()
	var b strings.Builder
	for _, pb := range boxes {
		if pb.Box.Context != where {
			continue
		}
		b.WriteString(editPostbox(pluginMetaBoxDOMID(pb), pb.Box.Title, s.pluginMetaBoxInner(ctx, pb, p, u)))
	}
	return template.HTML(b.String())
}

// pluginMetaBoxMediaJS adds the image field script once per edit screen when a box has an image
// field (the edit screens enqueue the media modal themselves), and the color field script when a box
// has a color field.
func (s *Server) pluginMetaBoxMediaJS(c *gin.Context, boxes []plugins.PluginMetaBox) {
	for _, pb := range boxes {
		if _, done := c.Get("pluginMediaFieldJS"); !done && pluginFieldsHaveImage(pb.Box.Fields) {
			c.Set("pluginMediaFieldJS", true)
			s.pageAssets(c).AddInline("media-editor", pluginMediaFieldJS)
		}
		if _, done := c.Get("pluginColorFieldJS"); !done && pluginFieldsHave(pb.Box.Fields, "color") {
			c.Set("pluginColorFieldJS", true) // swatch + Clear of color fields (plugins_fields.go)
			s.pageAssets(c).Script("jquery").AddInline("jquery", pluginColorFieldJS)
		}
	}
}

// editPostbox is the markup of an edit-screen box (edit-postbox-start/end in post-edit.html).
func editPostbox(id, title, inner string) string {
	t := html.EscapeString(title)
	return `<div id="` + html.EscapeString(id) + `" class="postbox">
<div class="postbox-header"><h2 class="hndle ui-sortable-handle">` + t + `</h2>
<div class="handle-actions hide-if-no-js"><button type="button" class="handlediv" aria-expanded="true"><span class="screen-reader-text">Toggle panel: ` + t + `</span><span class="toggle-indicator" aria-hidden="true"></span></button></div></div>
<div class="inside">
` + inner + `
</div>
</div>
`
}

// pluginMetaBoxesBlockEditor prints the boxes for Pursae Editor the way the_block_editor_meta_boxes()
// does: a metabox-base-form plus one form per location inside #metaboxes, and
// setAvailableMetaBoxesPerLocation. After a save the editor posts these forms to _wpMetaBoxUrl
// (postSave → pluginMetaBoxLoader).
func (s *Server) pluginMetaBoxesBlockEditor(c *gin.Context, p *store.Post) template.HTML {
	u := currentUser(c)
	boxes := s.pluginMetaBoxesFor(u, p)
	if len(boxes) == 0 {
		return ""
	}
	s.pluginMetaBoxMediaJS(c, boxes)
	ctx, cancel := pluginUICtx(c.Request.Context())
	defer cancel()
	type loc struct {
		ID    string `json:"id"`
		Title string `json:"title"`
	}
	locs := map[string][]loc{"normal": {}, "side": {}, "advanced": {}}
	forms := map[string]*strings.Builder{"normal": {}, "side": {}}
	for _, pb := range boxes {
		id := pluginMetaBoxDOMID(pb)
		locs[pb.Box.Context] = append(locs[pb.Box.Context], loc{ID: id, Title: pb.Box.Title})
		forms[pb.Box.Context].WriteString(editPostbox(id, pb.Box.Title, s.pluginMetaBoxInner(ctx, pb, p, u)))
	}
	var b strings.Builder
	b.WriteString(`<form class="metabox-base-form"><input type="hidden" name="_wpnonce" value="` + s.nonce(c, "update-post_"+itoa64(p.ID)) + `" />` +
		`<input type="hidden" name="post_ID" value="` + itoa64(p.ID) + `" /><input type="hidden" name="post_type" value="` + html.EscapeString(p.Type) + `" />` +
		`<input type="hidden" name="action" value="editpost" /></form>`)
	for _, where := range []string{"side", "normal"} {
		if forms[where].Len() == 0 {
			continue
		}
		b.WriteString(`<form id="metabox-container-` + where + `" class="metabox-location-` + where + `" data-no-submit><div id="poststuff" class="sidebar-open"><div id="postbox-container-2" class="postbox-container"><div id="` + where + `-sortables" class="meta-box-sortables">` +
			forms[where].String() + `</div></div></div></form>`)
	}
	data, _ := json.Marshal(locs)
	s.pageAssets(c).AddInline("wp-edit-post", `window._wpLoadBlockEditor.then( function() { wp.data.dispatch( 'core/edit-post' ).setAvailableMetaBoxesPerLocation( `+string(data)+` ); } );`)
	return template.HTML(b.String())
}

// pluginMetaBoxesSave stores the posted fields of the plugin boxes that were on the screen into
// post meta, then runs their save callbacks. The caller has checked the nonce and edit_post.
func (s *Server) pluginMetaBoxesSave(c *gin.Context, p *store.Post) {
	shown := map[string]bool{}
	for _, k := range c.PostFormArray(pluginMetaBoxField) {
		shown[k] = true
	}
	if len(shown) == 0 {
		return
	}
	boxes := s.pluginMetaBoxesFor(currentUser(c), p)
	set := map[string]any{}
	type saved struct {
		pb   plugins.PluginMetaBox
		vals map[string]any
	}
	var done []saved
	for _, pb := range boxes {
		if !shown[pluginMetaBoxKey(pb)] {
			continue
		}
		vals := map[string]any{}
		for _, f := range pb.Box.Fields {
			key := f.MetaKey(pb.Plugin.Slug())
			raw, present := c.GetPostForm(key)
			if v, ok := pluginFieldValue(f, raw, present); ok {
				set[key] = v
				vals[f.ID] = v
			} else if old, ok := p.Meta[key]; ok {
				vals[f.ID] = old
			}
		}
		done = append(done, saved{pb, vals})
	}
	ctx := c.Request.Context()
	if len(set) > 0 {
		raw, _ := json.Marshal(set)
		if _, err := s.store.Pool.Exec(ctx, `UPDATE posts SET meta = meta || $2::jsonb WHERE id = $1`, p.ID, raw); err != nil {
			return
		}
		if p.Meta == nil {
			p.Meta = map[string]any{}
		}
		for k, v := range set {
			p.Meta[k] = v
		}
	}
	cctx, cancel := pluginUICtx(ctx)
	defer cancel()
	for _, d := range done {
		_ = d.pb.Plugin.SaveMetaBox(cctx, d.pb.Box.ID, p.ID, d.vals)
	}
}

// pluginMetaBoxLoader answers Pursae Editor's meta box POST (_wpMetaBoxUrl, meta-box-loader=1):
// only the plugin boxes are saved, the post itself was already saved through REST.
func (s *Server) pluginMetaBoxLoader(c *gin.Context) {
	id, _ := strconv.ParseInt(c.PostForm("post_ID"), 10, 64)
	p, err := s.store.GetPost(c.Request.Context(), id)
	if err != nil {
		c.Status(http.StatusNotFound)
		return
	}
	if !s.checkNonce(c, "update-post_"+itoa64(p.ID)) {
		s.denyNonce(c)
		return
	}
	if !currentUser(c).Can("edit_post", p) {
		c.Status(http.StatusForbidden)
		return
	}
	s.pluginMetaBoxesSave(c, p)
	s.purgePostAgain(c, p)
	c.Status(http.StatusOK)
}

// Blocks ---------------------------------------------------------------------------

func init() {
	// Plugin blocks have no fixed renderer: the fallback asks the plugin runtime of the server
	// rendering the request.
	blocks.SetFallback(func(ctx context.Context, b *blocks.Block, content string) (string, bool) {
		rc := renderFrom(ctx)
		if rc == nil || rc.s == nil {
			return "", false
		}
		rt := rc.s.pluginRT()
		if rt == nil {
			return "", false
		}
		cctx, cancel := pluginUICtx(ctx)
		defer cancel()
		if strings.Contains(content, cspPH) { // hide the nonce placeholder from the plugin (maskNoncePlaceholder)
			var ok bool
			out := maskNoncePlaceholder(content, func(m string) string {
				var o string
				o, ok = rt.RenderBlock(cctx, b.Name(), b.Attrs, m)
				return o
			})
			return out, ok
		}
		return rt.RenderBlock(cctx, b.Name(), b.Attrs, content)
	})
}

// pluginBlockAttrType maps a field type to a block attribute type.
func pluginBlockAttrType(t string) string {
	switch t {
	case "number":
		return "number"
	case "checkbox":
		return "boolean"
	}
	return "string"
}

// pluginBlockEditorScript registers the plugins' blocks in Pursae Editor: inspector controls from
// the declared attributes and a server-side rendered preview (/wp/v2/block-renderer).
func (s *Server) pluginBlockEditorScript(c *gin.Context) {
	rt := s.pluginRT()
	if rt == nil {
		return
	}
	list := rt.BlockTypes()
	if len(list) == 0 {
		return
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Block.Name < list[j].Block.Name })
	type jsAttr struct {
		ID, Label, Type, Help string
		AttrType              string         `json:"attrType"`
		Default               any            `json:"default,omitempty"`
		Options               []any          `json:"options,omitempty"`
		Extra                 map[string]any `json:"extra,omitempty"`
	}
	var out []map[string]any
	for _, pb := range list {
		b := pb.Block
		attrs := []jsAttr{}
		for _, f := range b.Attributes {
			a := jsAttr{ID: f.ID, Label: f.Label, Type: f.Type, Help: f.Description, AttrType: pluginBlockAttrType(f.Type), Default: f.Default}
			for _, o := range f.Options {
				a.Options = append(a.Options, map[string]string{"value": o.Value, "label": o.Label})
			}
			attrs = append(attrs, a)
		}
		icon := b.Icon
		if icon == "" {
			icon = "admin-plugins"
		}
		out = append(out, map[string]any{"name": b.Name, "title": b.Title, "description": b.Description, "category": b.Category,
			"icon": icon, "keywords": b.Keywords, "attributes": attrs})
	}
	data, _ := json.Marshal(out)
	a := s.pageAssets(c)
	a.Script("wp-blocks", "wp-element", "wp-block-editor", "wp-components", "wp-server-side-render")
	a.AddInline("wp-server-side-render", pluginBlocksJS(string(data)))
}

// pluginBlocksJS is the editor side of plugin blocks (data is the JSON list of blocks).
func pluginBlocksJS(data string) string {
	return `( function( list ) {
	var el = wp.element.createElement, be = wp.blockEditor, cp = wp.components, SSR = wp.serverSideRender;
	list.forEach( function( b ) {
		if ( wp.blocks.getBlockType( b.name ) ) { return; }
		var attributes = { className: { type: 'string' } };
		b.attributes.forEach( function( a ) {
			attributes[ a.ID ] = { type: a.attrType };
			if ( a.default !== undefined ) { attributes[ a.ID ].default = a.default; }
		} );
		wp.blocks.registerBlockType( b.name, {
			apiVersion: 3, title: b.title, description: b.description, category: b.category, icon: b.icon,
			keywords: b.keywords || [], attributes: attributes, supports: { html: false },
			edit: function( props ) {
				var at = props.attributes, set = function( id ) { return function( v ) { var o = {}; o[ id ] = v; props.setAttributes( o ); }; };
				var controls = b.attributes.map( function( a ) {
					var common = { key: a.ID, label: a.Label, help: a.Help || undefined, __nextHasNoMarginBottom: true, __next40pxDefaultSize: true };
					switch ( a.Type ) {
						case 'checkbox': return el( cp.ToggleControl, Object.assign( common, { checked: !! at[ a.ID ], onChange: set( a.ID ) } ) );
						case 'textarea': return el( cp.TextareaControl, Object.assign( common, { value: at[ a.ID ] || '', onChange: set( a.ID ) } ) );
						case 'select': return el( cp.SelectControl, Object.assign( common, { value: at[ a.ID ], options: a.options || [], onChange: set( a.ID ) } ) );
						case 'radio': return el( cp.RadioControl, Object.assign( common, { selected: at[ a.ID ], options: a.options || [], onChange: set( a.ID ) } ) );
						case 'number': return el( cp.TextControl, Object.assign( common, { type: 'number', value: at[ a.ID ] === undefined ? '' : at[ a.ID ], onChange: function( v ) { set( a.ID )( v === '' ? undefined : Number( v ) ); } } ) );
						default: return el( cp.TextControl, Object.assign( common, { type: a.Type === 'password' ? 'text' : a.Type, value: at[ a.ID ] || '', onChange: set( a.ID ) } ) );
					}
				} );
				return el( 'div', be.useBlockProps(),
					controls.length ? el( be.InspectorControls, null, el( cp.PanelBody, { title: 'Settings' }, controls ) ) : null,
					el( SSR, { block: b.name, attributes: at } ) );
			},
			save: function() { return null; }
		} );
	} );
} )( ` + data + ` );`
}

// Web Builder widgets ---------------------------------------------------------------

// wbPluginWidgets is GET web-builder/v2/plugin-widgets: the Web Builder widgets of the active
// plugins with their control schema (for the editor's widget panel).
func (s *Server) wbPluginWidgets(c *gin.Context, _ *restRequest) {
	if wbUser(c, "edit_posts") == nil {
		return
	}
	out := []map[string]any{}
	if rt := s.pluginRT(); rt != nil {
		for _, w := range rt.BuilderWidgets() {
			out = append(out, map[string]any{"type": w.Type(), "plugin": w.Plugin.Slug(), "title": w.Widget.Title,
				"description": w.Widget.Description, "icon": w.Widget.Icon, "controls": w.Widget.Controls})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["type"].(string) < out[j]["type"].(string) })
	restJSON(c, http.StatusOK, out)
}

// wbPluginWidgetRender is POST web-builder/v2/plugin-widgets/render {"type", "settings"}: the
// widget's HTML for the editor canvas (the same render a visitor gets).
func (s *Server) wbPluginWidgetRender(c *gin.Context, r *restRequest) {
	if wbUser(c, "edit_posts") == nil {
		return
	}
	typ, _ := r.Body["type"].(string)
	settings, _ := r.Body["settings"].(map[string]any)
	if raw, _ := json.Marshal(settings); len(raw) > plugins.MaxUIInput {
		restErr(c, http.StatusRequestEntityTooLarge, "rest_invalid_param", "The settings are too large.")
		return
	}
	out, ok := s.renderPluginWidget(c.Request.Context(), typ, settings)
	if !ok {
		restErr(c, http.StatusNotFound, "wb_unknown_widget", "No active plugin registers this widget.")
		return
	}
	restJSON(c, http.StatusOK, map[string]any{"rendered": out})
}

// renderPluginWidget renders a plugin Web Builder widget (type <plugin slug>/<id>).
func (s *Server) renderPluginWidget(ctx context.Context, typ string, settings map[string]any) (string, bool) {
	rt := s.pluginRT()
	if rt == nil {
		return "", false
	}
	cctx, cancel := pluginUICtx(ctx)
	defer cancel()
	return rt.RenderBuilderWidget(cctx, typ, settings)
}
