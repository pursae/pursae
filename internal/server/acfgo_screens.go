package server

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/mail"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"wpgo/internal/store"
)

// Edit screens ------------------------------------------------------------------------------------

// acfgoTarget says where acfgo-input.js puts the boxes of a form screen.
type acfgoTarget struct{ Form, Before string }

// acfgoRenderScreen queues the field boxes of the object a GET admin screen edits.
func (s *Server) acfgoRenderScreen(c *gin.Context, screen string) {
	ctx := c.Request.Context()
	u := currentUser(c)
	var (
		groups   []*acfGroup
		o        *acfObject
		kind     = "form"
		target   acfgoTarget
		postID   int64
		editor   = ""
		postType = ""
	)
	switch screen {
	case "post", "post-new":
		var p *store.Post
		if screen == "post-new" {
			typ := c.DefaultQuery("post_type", "post")
			p = &store.Post{Type: typ, Status: "auto-draft", Format: "standard", Meta: map[string]any{}}
		} else {
			if a := c.Query("action"); a != "" && a != "edit" {
				return
			}
			id, _ := strconv.ParseInt(c.Query("post"), 10, 64)
			var err error
			p, err = s.store.GetPost(ctx, id)
			if err != nil {
				a, aerr := s.store.GetAttachment(ctx, id)
				if aerr != nil {
					return
				}
				o = &acfObject{Kind: "attachment", ID: a.ID, meta: orEmptyMap(a.Meta)}
				groups = s.acfgoGroupsFor(ctx, &acfScreen{Attachment: a.Mime, User: u})
				target = acfgoTarget{Form: "#post", Before: ""}
				kind, postID = "post", 0
				editor = "attachment"
				break
			}
			if !u.Can("edit_post", p) {
				return
			}
			o = s.acfgoPostObject(p)
			postID = p.ID
		}
		if strings.HasPrefix(p.Type, "acf-") {
			return
		}
		groups = s.acfgoGroupsFor(ctx, s.acfgoPostScreen(ctx, p, u))
		postType = p.Type
		kind = "post"
		editor = "classic"
		if s.useBlockEditor(c, p) {
			editor = "block"
			s.acfgoBlockEditorScript(c) // ACF blocks join Gotenberg's inserter
		}
	case "profile", "user-edit", "user-new":
		sc := &acfScreen{User: u, UserForm: "edit"}
		switch screen {
		case "profile":
			o = &acfObject{Kind: "user", ID: u.ID, meta: u.Meta}
			sc.UserRoles = u.Roles
			target = acfgoTarget{Form: "#your-profile", Before: "#your-profile > p.submit"}
		case "user-edit":
			id, _ := strconv.ParseInt(c.Query("user_id"), 10, 64)
			tu := s.acfgoUserByID(ctx, id)
			if tu == nil {
				return
			}
			o = &acfObject{Kind: "user", ID: tu.ID, meta: tu.Meta}
			sc.UserRoles = tu.Roles
			target = acfgoTarget{Form: "#your-profile", Before: "#your-profile > p.submit"}
		default:
			sc.UserForm = "add"
			target = acfgoTarget{Form: "#createuser", Before: "#createuser > p.submit"}
		}
		groups = s.acfgoGroupsFor(ctx, sc)
	case "term":
		id, _ := strconv.ParseInt(c.Query("tag_ID"), 10, 64)
		t, err := s.store.GetTerm(ctx, id)
		if err != nil {
			return
		}
		o = &acfObject{Kind: "term", ID: t.ID, meta: orEmptyMap(t.Meta)}
		groups = s.acfgoGroupsFor(ctx, &acfScreen{Taxonomy: t.Taxonomy, User: u})
		target = acfgoTarget{Form: "#edittag", Before: "#edittag .edit-tag-actions"}
	case "edit-tags":
		if c.Query("action") != "" {
			return
		}
		groups = s.acfgoGroupsFor(ctx, &acfScreen{Taxonomy: c.DefaultQuery("taxonomy", "post_tag"), User: u})
		target = acfgoTarget{Form: "#addtag", Before: "#addtag > p.submit"}
	case "comment":
		if c.Query("action") != "editcomment" {
			return
		}
		id, _ := strconv.ParseInt(c.Query("c"), 10, 64)
		cm, err := s.store.GetComment(ctx, id)
		if err != nil {
			return
		}
		typ := "post"
		if p, err := s.store.GetPost(ctx, cm.PostID); err == nil {
			typ = p.Type
		}
		o = &acfObject{Kind: "comment", ID: cm.ID, meta: orEmptyMap(cm.Meta)}
		groups = s.acfgoGroupsFor(ctx, &acfScreen{Comment: typ, User: u})
		target = acfgoTarget{Form: "#post", Before: "#post-body-content > :last-child"}
	default:
		return
	}
	if len(groups) == 0 {
		return
	}
	body, media, _ := s.acfgoBoxes(ctx, groups, o, kind, postID)
	var hide []string
	if editor == "classic" {
		for _, g := range groups {
			hide = append(hide, g.HideOnScreen...)
		}
	}
	h := fmt.Sprintf(`<div id="acfgo-boxes" class="acfgo-boxes acf-%s" style="display:none" data-kind="%s" data-editor="%s" data-form="%s" data-before="%s" data-post="%d" data-post-type="%s" data-nonce="%s" data-validate="%s">%s</div>%s`,
		esc(kind), esc(kind), esc(editor), esc(target.Form), esc(target.Before), postID, esc(postType), esc(s.nonce(c, "acfgo-save")), esc(s.nonce(c, "acfgo-validate")), body, acfgoHideOnScreenCSS(hide))
	addBodyHTML(c, template.HTML(h))
	s.pageAssets(c).Script("acfgo-input").Style("acfgo")
	if media && kind != "post" {
		s.enqueueMedia(c, 0)
	}
}

// acfgoHideOnScreen maps ACF's hide_on_screen items to the classic editor's boxes.
var acfgoHideOnScreen = map[string]string{"permalink": "#edit-slug-box", "the_content": "#postdivrich", "excerpt": "#postexcerpt", "discussion": "#commentstatusdiv",
	"comments": "#commentsdiv", "revisions": "#revisionsdiv", "slug": "#slugdiv", "author": "#authordiv", "format": "#formatdiv",
	"page_attributes": "#pageparentdiv", "featured_image": "#postimagediv", "categories": "#categorydiv", "tags": "#tagsdiv-post_tag",
	"send-trackbacks": "#trackbacksdiv"}

func acfgoHideOnScreenCSS(items []string) string {
	var sel []string
	seen := map[string]bool{}
	for _, it := range items {
		if css := acfgoHideOnScreen[it]; css != "" && !seen[css] {
			seen[css] = true
			sel = append(sel, css)
			sel = append(sel, ".metabox-prefs label[for=\""+strings.TrimPrefix(css, "#")+"-hide\"]")
		}
	}
	if len(sel) == 0 {
		return ""
	}
	sort.Strings(sel)
	return `<style id="acf-style">` + strings.Join(sel, ", ") + ` {display: none;}</style>`
}

// acfgoSaveScreen stores the acf[...] values posted by an edit screen before its handler runs;
// for add screens (the object does not exist yet) it saves after the handler.
func (s *Server) acfgoSaveScreen(c *gin.Context, screen string) {
	if !strings.Contains(c.ContentType(), "form") {
		return
	}
	// The Backup Manager upload streams its multipart body itself (bmgrUpload); parsing it here
	// would consume the body (and spool a multi-GB archive to disk) before the handler runs.
	if screen == "admin" && c.Query("page") == bmgrSlug && c.Query("action") == "upload" {
		return
	}
	_ = c.PostForm("_wpnonce") // parses the (multipart) form
	in := acfgoParseForm(c.Request.PostForm, "acf")
	if len(in) == 0 {
		return
	}
	ctx := c.Request.Context()
	u := currentUser(c)
	unfiltered := u.Can("unfiltered_html", nil)
	editorCtx := acfgoWithEditor(ctx, u)
	// valid is acf_validate_save_post(): input that fails the fields' rules is not stored and the
	// request ends with the list of problems (the form's JS check is only a convenience).
	valid := func() bool {
		errs := s.acfgoValidate(ctx, in)
		if len(errs) == 0 {
			return true
		}
		s.acfgoValidationDie(c, errs)
		return false
	}
	// save stores only the fields of groups whose location rules match the object's screen and
	// the current user (the groups the screen renders): a posted key from any other group is dropped.
	save := func(o *acfObject, sc *acfScreen) {
		if o == nil || sc == nil {
			return
		}
		sc.User = u
		in := acfgoKeepGroupFields(in, s.acfgoGroupsFor(ctx, sc))
		if len(in) == 0 || !valid() {
			return
		}
		if err := s.acfgoSaveInput(editorCtx, o, in, unfiltered); err != nil {
			log.Printf("acf-go: save %s %d: %v", o.Kind, o.ID, err)
		}
	}
	switch screen {
	case "post":
		id, _ := strconv.ParseInt(c.PostForm("post_ID"), 10, 64)
		if !s.checkNonce(c, "update-post_"+itoa64(id)) {
			return
		}
		if c.PostForm("post_type") == "attachment" {
			// The same rule as attachmentSave: one's own uploads, or edit_others_posts.
			if a, err := s.store.GetAttachment(ctx, id); err == nil && u.Can("upload_files", nil) && (a.UploaderID == u.ID || u.Can("edit_others_posts", nil)) {
				save(&acfObject{Kind: "attachment", ID: a.ID, meta: orEmptyMap(a.Meta)}, &acfScreen{Attachment: a.Mime})
			}
			return
		}
		p, err := s.store.GetPost(ctx, id)
		if err != nil || !u.Can("edit_post", p) || strings.HasPrefix(p.Type, "acf-") {
			return
		}
		save(s.acfgoPostObject(p), s.acfgoPostScreen(ctx, p, u))
	case "profile", "user-edit":
		id := u.ID
		if screen == "user-edit" {
			id, _ = strconv.ParseInt(defaultStr(c.PostForm("user_id"), c.Query("user_id")), 10, 64)
		}
		if !s.checkNonce(c, "update-user_"+itoa64(id)) || (id != u.ID && !u.Can("edit_users", nil)) {
			return
		}
		if tu := s.acfgoUserByID(ctx, id); tu != nil {
			save(&acfObject{Kind: "user", ID: tu.ID, meta: tu.Meta}, &acfScreen{UserForm: "edit", UserRoles: tu.Roles})
		}
	case "term":
		id, _ := strconv.ParseInt(c.PostForm("tag_ID"), 10, 64)
		if !s.checkNonce(c, "update-tag_"+itoa64(id)) || !u.Can("manage_categories", nil) {
			return
		}
		if t, err := s.store.GetTerm(ctx, id); err == nil {
			save(&acfObject{Kind: "term", ID: t.ID, meta: orEmptyMap(t.Meta)}, &acfScreen{Taxonomy: t.Taxonomy})
		}
	case "comment":
		id, _ := strconv.ParseInt(c.PostForm("comment_ID"), 10, 64)
		if !s.checkNonce(c, "update-comment_"+itoa64(id)) || !u.Can("moderate_comments", nil) {
			return
		}
		if cm, err := s.store.GetComment(ctx, id); err == nil {
			typ := "post"
			if p, err := s.store.GetPost(ctx, cm.PostID); err == nil {
				typ = p.Type
			}
			save(&acfObject{Kind: "comment", ID: cm.ID, meta: orEmptyMap(cm.Meta)}, &acfScreen{Comment: typ})
		}
	case "edit-tags", "user-new":
		if (screen == "edit-tags" && !s.checkNonce(c, "add-tag")) || (screen == "user-new" && !s.checkNonce(c, "create-user")) {
			return
		}
		if !valid() { // before the core creates the object
			return
		}
		tax, name, login := c.DefaultPostForm("taxonomy", "post_tag"), strings.TrimSpace(c.PostForm("tag-name")), strings.TrimSpace(c.PostForm("user_login"))
		c.Next() // the object exists after the core's handler
		// Only when the core created it (it redirects on success): a refused add ("already exists",
		// no permission) must not write the fields onto the existing term or user of that name.
		if c.Writer.Status() != http.StatusFound {
			return
		}
		if screen == "edit-tags" {
			if t, err := s.store.TermByName(ctx, tax, name); err == nil {
				save(&acfObject{Kind: "term", ID: t.ID, meta: orEmptyMap(t.Meta)}, &acfScreen{Taxonomy: tax})
			}
			return
		}
		var id int64
		if s.store.Pool.QueryRow(ctx, `SELECT id FROM users WHERE login = $1`, login).Scan(&id) == nil {
			if tu := s.acfgoUserByID(ctx, id); tu != nil {
				save(&acfObject{Kind: "user", ID: tu.ID, meta: tu.Meta}, &acfScreen{UserForm: "add"})
			}
		}
	}
}

// acfgoKeepGroupFields keeps the posted top-level field keys that belong to one of groups.
func acfgoKeepGroupFields(in map[string]any, groups []*acfGroup) map[string]any {
	allowed := map[string]bool{}
	for _, g := range groups {
		for _, f := range g.Fields {
			allowed[acfgoStr(f["key"])] = true
		}
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		if allowed[k] {
			out[k] = v
		}
	}
	return out
}

// acfgoValidationDie ends a classic form POST whose acf[...] values failed validation.
func (s *Server) acfgoValidationDie(c *gin.Context, errs []acfgoError) {
	var li strings.Builder
	for _, e := range errs {
		li.WriteString("<li>" + template.HTMLEscapeString(e.Message) + "</li>")
	}
	s.adminHTML(c, http.StatusUnprocessableEntity, `<!DOCTYPE html><html><head><meta charset="utf-8"><title>Validation failed</title>`+
		`<link rel="stylesheet" href="/wp-admin/assets/css/wp/install.min.css"></head><body id="error-page">`+
		`<div class="wp-die-message"><p><strong>Validation failed.</strong> Nothing was saved.</p><ul>`+li.String()+`</ul></div>`+
		`<p><a href="#" id="acfgo-back">Go back and correct the fields.</a></p>`+
		`<script`+cspNonceAttr()+`>document.getElementById("acfgo-back").addEventListener("click",function(e){e.preventDefault();history.back();});</script></body></html>`)
	c.Abort()
}

// acfgoSaveInput stores parsed acf[...] input (keyed by field key) on an object: every posted
// top-level field, like acf_update_values().
func (s *Server) acfgoSaveInput(ctx context.Context, o *acfObject, in map[string]any, unfiltered bool) error {
	w := newACFWrite()
	keys := make([]string, 0, len(in))
	for k := range in {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		f := s.acfgoFieldByKey(ctx, k)
		if f == nil {
			continue
		}
		s.acfgoUpdate(ctx, o, w, f, "", in[k], unfiltered)
	}
	if len(w.set) == 0 && len(w.del) == 0 {
		return nil
	}
	err := s.acfgoCommit(ctx, o, w)
	if o.Kind == "post" && o.Post != nil {
		s.afterPostSave(ctx, o.Post)
	}
	return err
}

// acfgoParseForm turns acf[a][b][] form keys into nested maps and lists. A key that is both a
// scalar ("" placeholder) and a map or list keeps the map or list.
func acfgoParseForm(form url.Values, root string) map[string]any {
	out := map[string]any{}
	keys := make([]string, 0, len(form))
	for k := range form {
		if strings.HasPrefix(k, root+"[") {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		segs := strings.Split(strings.TrimSuffix(strings.TrimPrefix(k, root+"["), "]"), "][")
		vals := form[k]
		cur := out
		ok := true
		for i, seg := range segs {
			last := i == len(segs)-1
			if last {
				if seg == "" { // list leaf: only reached for name[] at the end of the path
					break
				}
				if _, isMap := cur[seg].(map[string]any); isMap {
					break
				}
				if _, isList := cur[seg].([]any); isList {
					break
				}
				cur[seg] = vals[len(vals)-1]
				break
			}
			if segs[i+1] == "" && i+1 == len(segs)-1 { // seg[]
				var list []any
				for _, v := range vals {
					if v != "" {
						list = append(list, v)
					}
				}
				if list == nil {
					list = []any{}
				}
				if old, isList := cur[seg].([]any); isList {
					list = append(old, list...)
				}
				cur[seg] = list
				break
			}
			next, isMap := cur[seg].(map[string]any)
			if !isMap {
				next = map[string]any{}
				cur[seg] = next
			}
			cur = next
		}
		_ = ok
	}
	return out
}

// Validation ---------------------------------------------------------------------------------------

type acfgoError struct {
	Input   string `json:"input"`
	Message string `json:"message"`
}

// acfgoValidate is acf_validate_values(): required, limits and formats of posted input.
func (s *Server) acfgoValidate(ctx context.Context, in map[string]any) []acfgoError {
	var errs []acfgoError
	for k, v := range in {
		if f := s.acfgoFieldByKey(ctx, k); f != nil {
			errs = append(errs, s.acfgoValidateField(ctx, f, v, "acf["+k+"]")...)
		}
	}
	sort.Slice(errs, func(i, j int) bool { return errs[i].Input < errs[j].Input })
	return errs
}

func acfgoEmpty(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return strings.TrimSpace(t) == ""
	case []any:
		return len(t) == 0
	case map[string]any:
		for _, x := range t {
			if !acfgoEmpty(x) {
				return false
			}
		}
		return true
	}
	return false
}

func (s *Server) acfgoValidateField(ctx context.Context, f acfField, v any, input string) []acfgoError {
	var errs []acfgoError
	label := acfgoStr(f["label"])
	add := func(msg string) { errs = append(errs, acfgoError{input, msg}) }
	typ := acfgoStr(f["type"])
	switch typ {
	case "repeater", "flexible_content":
		rows := acfgoRows(v)
		if acfgoBool(f["required"]) && len(rows) == 0 {
			add(label + " value is required")
		}
		if min := acfgoInt(f["min"]); min > 0 && len(rows) < int(min) && (len(rows) > 0 || acfgoBool(f["required"])) {
			add(fmt.Sprintf("Minimum rows not reached (%d rows)", min))
		}
		if max := acfgoInt(f["max"]); max > 0 && len(rows) > int(max) {
			add(fmt.Sprintf("Maximum rows reached (%d rows)", max))
		}
		m, _ := v.(map[string]any)
		rowKeys := make([]string, 0, len(m))
		for k := range m {
			if !strings.HasPrefix(k, "acfcloneindex") {
				rowKeys = append(rowKeys, k)
			}
		}
		sort.Strings(rowKeys)
		for _, rk := range rowKeys {
			row, _ := m[rk].(map[string]any)
			subs := acfgoSubFields(f)
			if typ == "flexible_content" {
				subs = acfgoLayoutSubs(acfgoLayout(f, acfgoStr(row["acf_fc_layout"])))
			}
			for _, sf := range subs {
				if sv, ok := row[acfgoStr(sf["key"])]; ok {
					errs = append(errs, s.acfgoValidateField(ctx, sf, sv, input+"["+rk+"]["+acfgoStr(sf["key"])+"]")...)
				}
			}
		}
		return errs
	case "group", "clone":
		m, _ := v.(map[string]any)
		subs := acfgoSubFields(f)
		if typ == "clone" {
			subs = s.acfgoCloneFields(ctx, f)
		}
		for _, sf := range subs {
			if sv, ok := m[acfgoStr(sf["key"])]; ok {
				errs = append(errs, s.acfgoValidateField(ctx, sf, sv, input+"["+acfgoStr(sf["key"])+"]")...)
			}
		}
		return errs
	}
	if typ == "true_false" {
		if acfgoBool(f["required"]) && !acfgoBool(acfgoScalar(v)) {
			add(label + " value is required")
		}
		return errs
	}
	if acfgoEmpty(v) {
		if acfgoBool(f["required"]) {
			add(label + " value is required")
		}
		return errs
	}
	sv := acfgoStr(acfgoScalar(v))
	switch typ {
	case "text", "textarea":
		if n := acfgoInt(f["maxlength"]); n > 0 && utf8.RuneCountInString(sv) > int(n) {
			add(fmt.Sprintf("Value must not exceed %d characters", n))
		}
	case "number", "range":
		n, err := strconv.ParseFloat(strings.TrimSpace(sv), 64)
		if err != nil {
			add("Value must be a number")
			break
		}
		if min := acfgoStr(f["min"]); min != "" {
			if m, _ := strconv.ParseFloat(min, 64); n < m {
				add("Value must be equal to or higher than " + min)
			}
		}
		if max := acfgoStr(f["max"]); max != "" {
			if m, _ := strconv.ParseFloat(max, 64); n > m {
				add("Value must be equal to or lower than " + max)
			}
		}
	case "email":
		if _, err := mail.ParseAddress(sv); err != nil || !strings.Contains(sv, "@") || strings.ContainsAny(sv, " <>") {
			add("Value must be a valid email address")
		}
	case "url":
		if u, err := url.Parse(strings.TrimSpace(sv)); err != nil || (u.Scheme == "" && !strings.HasPrefix(sv, "/")) || (u.Scheme != "" && u.Host == "" && u.Scheme != "mailto" && u.Scheme != "tel") {
			add("Value must be a valid URL")
		}
	case "relationship", "gallery", "checkbox", "select", "post_object", "user":
		n := len(acfgoList(v))
		if min := acfgoInt(f["min"]); min > 0 && n < int(min) {
			add(fmt.Sprintf("%s requires at least %d selection%s", label, min, map[bool]string{true: "", false: "s"}[min == 1]))
		}
		if max := acfgoInt(f["max"]); max > 0 && n > int(max) {
			add(fmt.Sprintf("%s allows a maximum of %d selection%s", label, max, map[bool]string{true: "", false: "s"}[max == 1]))
		}
	case "image", "file":
		a, err := s.store.GetAttachment(ctx, acfgoInt(sv))
		if err != nil {
			add("Invalid attachment")
			break
		}
		if typ == "image" {
			if n := acfgoInt(f["min_width"]); n > 0 && a.Width < int(n) {
				add(fmt.Sprintf("Image width must be at least %dpx.", n))
			}
			if n := acfgoInt(f["min_height"]); n > 0 && a.Height < int(n) {
				add(fmt.Sprintf("Image height must be at least %dpx.", n))
			}
			if n := acfgoInt(f["max_width"]); n > 0 && a.Width > int(n) {
				add(fmt.Sprintf("Image width must not exceed %dpx.", n))
			}
			if n := acfgoInt(f["max_height"]); n > 0 && a.Height > int(n) {
				add(fmt.Sprintf("Image height must not exceed %dpx.", n))
			}
		}
		if types := acfgoStr(f["mime_types"]); types != "" {
			ext := strings.ToLower(strings.TrimPrefix(lastPathPart(a.FilePath)[strings.LastIndex(lastPathPart(a.FilePath), ".")+1:], "."))
			ok := false
			for _, t := range strings.Split(types, ",") {
				if strings.TrimPrefix(strings.ToLower(strings.TrimSpace(t)), ".") == ext {
					ok = true
				}
			}
			if !ok {
				add("File type must be " + types + ".")
			}
		}
	}
	return errs
}

// Ajax --------------------------------------------------------------------------------------------

// acfgoValidateAjax is acf/validate_save_post: the form's acf[...] input checked before submit.
func (s *Server) acfgoValidateAjax(c *gin.Context) {
	if !s.checkNonceValue(c, c.PostForm("_acfnonce"), "acfgo-validate") {
		c.JSON(http.StatusForbidden, gin.H{"success": false})
		return
	}
	errs := s.acfgoValidate(c.Request.Context(), acfgoParseForm(c.Request.PostForm, "acf"))
	if errs == nil {
		errs = []acfgoError{}
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{"valid": len(errs) == 0, "errors": errs}})
}

// acfgoMetaBoxSave stores the field boxes of the block editor: the editor posts its meta box
// forms to window._wpMetaBoxUrl, which acfgo-input.js points here.
func (s *Server) acfgoMetaBoxSave(c *gin.Context) {
	if c.Request.PostForm == nil {
		_ = c.Request.ParseMultipartForm(32 << 20)
	}
	if !s.checkNonceValue(c, c.PostForm("_acfnonce"), "acfgo-save") {
		c.String(http.StatusForbidden, "-1")
		return
	}
	ctx := c.Request.Context()
	id, _ := strconv.ParseInt(c.PostForm("post_ID"), 10, 64)
	u := currentUser(c)
	p, err := s.store.GetPost(ctx, id)
	if err != nil || !u.Can("edit_post", p) || strings.HasPrefix(p.Type, "acf-") {
		c.String(http.StatusForbidden, "-1")
		return
	}
	// Only the fields of the groups this post's screen renders for this user, like acfgoSaveScreen.
	in := acfgoKeepGroupFields(acfgoParseForm(c.Request.PostForm, "acf"), s.acfgoGroupsFor(ctx, s.acfgoPostScreen(ctx, p, u)))
	if errs := s.acfgoValidate(ctx, in); len(errs) > 0 {
		c.JSON(http.StatusOK, gin.H{"success": false, "data": gin.H{"errors": errs}})
		return
	}
	if err := s.acfgoSaveInput(acfgoWithEditor(ctx, u), s.acfgoPostObject(p), in, u.Can("unfiltered_html", nil)); err != nil {
		c.String(http.StatusInternalServerError, "0")
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true})
}

// acfgoQueryPostsAjax is the relationship field's search.
func (s *Server) acfgoQueryPostsAjax(c *gin.Context) {
	ctx := c.Request.Context()
	if !currentUser(c).Can("edit_posts", nil) {
		c.JSON(http.StatusForbidden, gin.H{"success": false})
		return
	}
	f := s.acfgoFieldByKey(ctx, c.Query("field_key"))
	if f == nil {
		f = acfField{"type": "relationship"}
	}
	type item struct {
		ID   int64  `json:"id"`
		HTML string `json:"html"`
		Text string `json:"text"`
	}
	out := []item{}
	if acfgoStr(f["type"]) == "user" { // user field: search users (the ajax select)
		for _, tu := range s.acfgoUserChoices(ctx, f, strings.TrimSpace(c.Query("s"))) {
			t := tu.DisplayName + " (" + tu.Login + ")"
			out = append(out, item{tu.ID, esc(t), t})
		}
		c.JSON(http.StatusOK, gin.H{"success": true, "data": out})
		return
	}
	typ := c.Query("post_type")
	if typ != "" && !containsStr(acfgoAllowedTypes(f), typ) {
		typ = "-" // the type filter narrows the field's post types; it cannot name another one
	}
	u := currentUser(c)
	for _, p := range s.acfgoPostChoices(ctx, f, strings.TrimSpace(c.Query("s")), typ) {
		// The search also matches content: never answer with posts the user may not read (other
		// users' drafts and private posts for a contributor).
		if p.Status != "publish" && !u.Can("read_post", p) {
			continue
		}
		out = append(out, item{p.ID, s.acfgoRelItemLabel(ctx, f, p), defaultStr(p.Title, "(no title)")})
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": out})
}

func (s *Server) acfgoOembedAjax(c *gin.Context) {
	if !currentUser(c).Can("edit_posts", nil) {
		c.JSON(http.StatusForbidden, gin.H{"success": false})
		return
	}
	w, _ := strconv.Atoi(c.Query("width"))
	h, _ := strconv.Atoi(c.Query("height"))
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{"html": acfgoEmbedHTML(strings.TrimSpace(c.Query("url")), w, h)}})
}

// Options pages ------------------------------------------------------------------------------------

type acfgoOptionsData struct {
	Page              *acfgoOptionsPage
	Boxes, SideBoxes  template.HTML
	Nonce, Validate   string
	HasSide, HasBoxes bool
}

// acfgoAdminPage serves admin?page=<slug> for ACF options pages; false when the page is not one.
func (s *Server) acfgoAdminPage(c *gin.Context) bool {
	if !s.acfgoOn() {
		return false
	}
	ctx := c.Request.Context()
	op := s.acfgoOptionsPage(ctx, c.Query("page"))
	if op == nil {
		return false
	}
	u := currentUser(c)
	if !u.Can(op.Capability, nil) {
		c.String(http.StatusForbidden, "Sorry, you are not allowed to access this page.")
		return true
	}
	if op.Redirect {
		for _, child := range s.acfgoOptionsPages(ctx) {
			if child.ParentSlug == op.MenuSlug && u.Can(child.Capability, nil) {
				c.Redirect(http.StatusFound, "/wp-admin/admin?page="+url.QueryEscape(child.MenuSlug))
				return true
			}
		}
	}
	o := s.acfgoObject(ctx, op.PostID, nil)
	groups := s.acfgoGroupsFor(ctx, &acfScreen{OptionsPage: op.MenuSlug, User: u})
	var notices []notice
	if c.Request.Method == http.MethodPost {
		if !s.checkNonce(c, "acfgo-options_"+op.MenuSlug) {
			s.denyNonce(c)
			return true
		}
		// Only the fields of the groups this page renders for this user: every options page shares
		// the "options" storage, so a key from another page's group must not be written here.
		in := acfgoKeepGroupFields(acfgoParseForm(c.Request.PostForm, "acf"), groups)
		if errs := s.acfgoValidate(ctx, in); len(errs) > 0 {
			var msgs []string
			for _, e := range errs {
				msgs = append(msgs, template.HTMLEscapeString(e.Message))
			}
			notices = append(notices, notice{Type: "error", Message: template.HTML("<strong>Validation failed.</strong> " + strings.Join(msgs, "; "))})
		} else {
			if err := s.acfgoSaveInput(acfgoWithEditor(ctx, u), o, in, u.Can("unfiltered_html", nil)); err != nil {
				notices = append(notices, notice{Type: "error", Message: template.HTML(template.HTMLEscapeString(err.Error()))})
			} else {
				s.purgeAll(ctx)
				c.Redirect(http.StatusFound, "/wp-admin/admin?page="+url.QueryEscape(op.MenuSlug)+"&message=1")
				return true
			}
		}
	}
	if c.Query("message") == "1" {
		notices = append(notices, notice{Type: "success", Message: template.HTML(template.HTMLEscapeString(op.Updated)), Dismissible: true})
	}
	var main, side []*acfGroup
	for _, g := range groups {
		if g.Position == "side" {
			side = append(side, g)
		} else {
			main = append(main, g)
		}
	}
	d := acfgoOptionsData{Page: op, Nonce: s.nonce(c, "acfgo-options_"+op.MenuSlug), Validate: s.nonce(c, "acfgo-validate"), HasSide: len(side) > 0, HasBoxes: len(groups) > 0}
	mb, media1, _ := s.acfgoBoxes(ctx, main, o, "options", 0)
	sb, media2, _ := s.acfgoBoxes(ctx, side, o, "options", 0)
	d.Boxes, d.SideBoxes = template.HTML(mb), template.HTML(sb)
	s.pageAssets(c).Script("acfgo-input", "postbox").Style("acfgo")
	if media1 || media2 {
		s.enqueueMedia(c, 0)
	}
	s.adminPage(c, "admin?page="+op.MenuSlug, op.PageTitle, "acf-admin-page acf-options-page toplevel_page_"+op.MenuSlug, "admin/acfgo-options-page", d, notices...)
	return true
}

// acfgoJSON is a small helper for inline data.
func acfgoJSON(v any) template.JS {
	b, _ := json.Marshal(v)
	return template.JS(b)
}
