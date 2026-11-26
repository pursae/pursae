package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"wpgo/internal/i18n"
	"wpgo/internal/store"
)

// HeadlessGo's admin screens: /wp-admin/headlessgo?page=… Content Manager (entries, entry),
// Content-Type Builder (builder), API Tokens (tokens), Webhooks (webhooks) and Settings
// (settings). The Content Manager needs edit_posts; everything else manage_options. The entry
// form and the builder are rendered by headlessgo.js from the schema; they post JSON.

const (
	hgNonce    = "headlessgo-admin"
	hgAssetDir = "/wp-admin/assets/headlessgo/"
)

var hgAssetsOnce sync.Once

func hgRegisterAssets() {
	registry.Scripts["headlessgo"] = &regScript{Src: hgAssetDir + "headlessgo.js", Deps: []string{"jquery"}, Ver: hgAssetVer, Footer: true}
	registry.Styles["headlessgo"] = &regStyle{Src: hgAssetDir + "headlessgo.css", Ver: hgAssetVer}
}

// hgAdminData is the view of every screen.
type hgAdminData struct {
	Page, Title string
	Nonce       string
	Settings    hgSettings
	Types       []*hgSchema
	Components  []*hgSchema
	Type        *hgSchema
	Locales     []hgLocale
	Locale      string
	GolylangOn  bool
	CanAdmin    bool
	Home        string
	APIBase     string

	// Content Manager list
	Counts   map[string]int
	Rows     []hgAdminRow
	Columns  []*hgField
	Total    int
	PageNum  int
	NumPages int
	Search   string
	Status   string
	Sort     string
	Desc     bool
	QS       string

	// Entry form
	Entry       *hgEntry
	Doc         string
	EditorJSON  template.JS
	Versions    map[string]*hgEntry
	PreviewURL  string
	EntryStatus string
	EntryTitle  string // the value of the type's title field, for the editor's header
	Errors      hgValidationError
	PublishAt   string // schedule, as datetime-local values in the site's time zone
	UnpublishAt string
	TimeZone    string
	Lock        *hgLock // set while another user edits the entry (the form is read-only)
	LockBeat    int     // heartbeat interval in seconds
	BaseStamp   string  // the version the form was loaded from (see hgGuardWrite)
	ReviewState string  // review status of the draft (types with a review flow)
	CanReview   bool

	// History
	History []*hgEntryVersion
	Version *hgEntryVersion
	Diff    []hgDiffRow
	// RestoreErrs are the values of the chosen version that no longer fit the content type.
	RestoreErrs hgValidationError
	// RestoreDropped are the fields of the chosen version the content type no longer has.
	RestoreDropped []string

	// Builder
	BuilderJSON template.JS
	FieldTypes  any
	NewKind     string

	// Tokens
	Tokens    []*hgToken
	Token     *hgToken
	NewToken  string
	Actions   []string
	Durations []struct{ Key, Label string }

	// Webhooks
	Webhooks   []*hgWebhook
	Webhook    *hgWebhook
	Events     any
	Deliveries []hgDelivery
	Queue      []hgQueueItem
	HeaderText string

	// GraphQL playground
	GQLQueries   []string
	GQLMutations []string
}

type hgAdminRow struct {
	E        *hgEntry
	Title    string
	Cells    []string
	Status   string
	Edit     string
	Schedule string // "Publishes …" / "Unpublishes …"
}

func (d *hgAdminData) Allowed(uid, action string) bool {
	return containsStr(d.Settings.Public[uid], action)
}

// AuthAllowed and OwnerOnlyOn are the checkboxes of the signed-in users matrix.
func (d *hgAdminData) AuthAllowed(uid, action string) bool {
	return containsStr(d.Settings.Authenticated[uid], action)
}
func (d *hgAdminData) OwnerOnlyOn(uid, action string) bool {
	return containsStr(d.Settings.OwnerOnly[uid], action)
}

func (d *hgAdminData) TokenCan(uid, action string) bool {
	return d.Token != nil && d.Token.Can(uid, action)
}

func (d *hgAdminData) HasEvent(e string) bool {
	return d.Webhook != nil && containsStr(d.Webhook.Events, e)
}

func (d *hgAdminData) Origins() string { return strings.Join(d.Settings.CORSOrigins, "\n") }

func (d *hgAdminData) Route(sc *hgSchema) string {
	if sc.IsSingle() {
		return d.APIBase + "/" + sc.SingularName
	}
	return d.APIBase + "/" + sc.PluralName
}

func (d *hgAdminData) SortLink(field string) string {
	dir := "asc"
	if d.Sort == field && !d.Desc {
		dir = "desc"
	}
	q := url.Values{"page": {"entries"}, "type": {d.Type.UID}, "orderby": {field}, "order": {dir}}
	if d.Search != "" {
		q.Set("s", d.Search)
	}
	if d.Status != "" {
		q.Set("status", d.Status)
	}
	if d.Locale != "" {
		q.Set("locale", d.Locale)
	}
	return hgAdminPath + "?" + q.Encode()
}

func (d *hgAdminData) PageLink(n int) string {
	return hgAdminPath + "?" + d.QS + "&paged=" + strconv.Itoa(n)
}

func (d *hgAdminData) FieldLabel(t string) string { return hgFieldTypeLabel(t) }

func (s *Server) hgAdmin(c *gin.Context) {
	u := currentUser(c)
	page := c.Query("page")
	if c.Request.Method == http.MethodPost {
		page = c.PostForm("page")
	}
	needAdmin := page != "" && page != "entries" && page != "entry" && page != "history"
	if !u.Can("edit_posts", nil) || (needAdmin && !u.Can("manage_options", nil)) {
		c.String(http.StatusForbidden, "Sorry, you are not allowed to access this page.")
		return
	}
	if c.Query("action") == "export" {
		if !s.checkNonce(c, hgNonce) || !u.Can("manage_options", nil) {
			s.denyNonce(c)
			return
		}
		raw, _ := json.MarshalIndent(s.hgExportSchemas(c), "", "  ")
		c.Header("Content-Disposition", `attachment; filename="headlessgo-schemas-`+time.Now().Format("2006-01-02")+`.json"`)
		c.Data(http.StatusOK, "application/json; charset=utf-8", raw)
		return
	}
	if a := c.Query("action"); a == "export_entries" || a == "types" {
		if !s.checkNonce(c, hgNonce) || !u.Can("manage_options", nil) {
			s.denyNonce(c)
			return
		}
		day := time.Now().Format("2006-01-02")
		if a == "types" {
			full := &hgAuth{Kind: "token", Token: &hgToken{Kind: "full-access"}}
			c.Header("Content-Disposition", `attachment; filename="headlessgo-types-`+day+`.d.ts"`)
			c.Data(http.StatusOK, "text/plain; charset=utf-8", []byte(hgTypeScript(s.hgOpenAPI(c, full))))
			return
		}
		var uids []string
		if t := c.Query("type"); t != "" {
			uids = []string{t}
		}
		var buf bytes.Buffer
		if _, err := s.hgExportEntries(c, &buf, uids); err != nil {
			c.String(http.StatusBadRequest, err.Error())
			return
		}
		c.Header("Content-Disposition", `attachment; filename="headlessgo-entries-`+day+`.ndjson"`)
		c.Data(http.StatusOK, "application/x-ndjson; charset=utf-8", buf.Bytes())
		return
	}
	if c.Request.Method == http.MethodPost {
		if !s.checkNonce(c, hgNonce) {
			s.denyNonce(c)
			return
		}
		s.hgAdminPost(c, page)
		return
	}
	s.hgRender(c, page, nil)
}

// hgRedirect goes back to a screen with a message or an error.
func hgRedirect(c *gin.Context, q url.Values, msg string, err error) {
	if err != nil {
		q.Set("hg-error", err.Error())
	} else if msg != "" {
		q.Set("hg-msg", msg)
	}
	c.Redirect(http.StatusFound, hgAdminPath+"?"+q.Encode())
}

func (s *Server) hgBaseData(c *gin.Context, page string) *hgAdminData {
	reg := s.hgReg(c)
	u := currentUser(c)
	return &hgAdminData{Page: page, Nonce: s.nonce(c, hgNonce), Settings: s.hgLoad(), Types: reg.Types(), Components: reg.Components(),
		Locales: s.hgLocales(c), GolylangOn: s.golylangOn(), CanAdmin: u.Can("manage_options", nil), Home: s.links(c).HomeURL("/"),
		APIBase: strings.TrimRight(s.links(c).HomeURL("/"), "/") + hgAPIPrefix, Actions: hgActions}
}

// hgRender renders a screen; errs carries a failed entry save (the form keeps the input).
func (s *Server) hgRender(c *gin.Context, page string, failed *hgFailedSave) {
	d := s.hgBaseData(c, page)
	reg := s.hgReg(c)
	var notices []notice
	if m := c.Query("hg-msg"); m != "" {
		notices = append(notices, notice{Type: "success", Message: template.HTML(template.HTMLEscapeString(m)), Dismissible: true})
	}
	if e := c.Query("hg-error"); e != "" {
		notices = append(notices, notice{Type: "error", Message: template.HTML(template.HTMLEscapeString(e))})
	}
	hgAssetsOnce.Do(hgRegisterAssets)
	s.pageAssets(c).Style("headlessgo")
	title := "Content Manager"
	switch page {
	case "", "entries":
		if page == "" && len(d.Types) > 0 {
			d.Page = ""
			d.Counts = map[string]int{}
			rows, err := s.store.Pool.Query(c, `SELECT type_uid, count(DISTINCT document_id) FROM hg_entries GROUP BY type_uid`)
			if err == nil {
				for rows.Next() {
					var uid string
					var n int
					if rows.Scan(&uid, &n) == nil {
						d.Counts[uid] = n
					}
				}
				rows.Close()
			}
		}
		if page == "entries" {
			d.Type = reg.Get(c.Query("type"))
			if d.Type == nil || d.Type.IsComponent() {
				c.Redirect(http.StatusFound, hgAdminPath)
				return
			}
			if d.Type.IsSingle() {
				c.Redirect(http.StatusFound, hgAdminPath+"?page=entry&type="+url.QueryEscape(d.Type.UID))
				return
			}
			if err := s.hgAdminList(c, d); err != nil {
				notices = append(notices, notice{Type: "error", Message: template.HTML(template.HTMLEscapeString(err.Error()))})
			}
			title = d.Type.DisplayName
		}
	case "history":
		d.Type = reg.Get(c.Query("type"))
		if d.Type == nil || d.Type.IsComponent() || !s.hgAdminHistory(c, d) {
			c.Redirect(http.StatusFound, hgAdminPath)
			return
		}
		title = "History"
	case "graphql":
		title = "GraphQL"
		ex := &hgGQLExec{s: s, c: c}
		d.GQLQueries = ex.schema().RootFields("Query")
		d.GQLMutations = ex.schema().RootFields("Mutation")
		s.pageAssets(c).Script("headlessgo")
	case "entry":
		d.Type = reg.Get(c.Query("type"))
		if failed != nil {
			d.Type = failed.Type
		}
		if d.Type == nil || d.Type.IsComponent() {
			c.Redirect(http.StatusFound, hgAdminPath)
			return
		}
		if !s.hgAdminEntry(c, d, failed) {
			return
		}
		title = d.Type.DisplayName
		s.enqueueMedia(c, 0)
		s.pageAssets(c).Script("headlessgo")
	case "builder":
		title = "Content-Type Builder"
		s.hgAdminBuilder(c, d)
		s.pageAssets(c).Script("headlessgo")
	case "tokens":
		title = "API Tokens"
		d.Tokens = s.hgTokens(c)
		d.Durations = []struct{ Key, Label string }{{"7", "7 days"}, {"30", "30 days"}, {"90", "90 days"}, {"0", "Unlimited"}}
		if id, _ := strconv.ParseInt(c.Query("id"), 10, 64); id > 0 {
			d.Token = s.hgTokenByID(c, id)
		}
		if c.Query("new") == "1" {
			d.Token = &hgToken{Kind: "read-only", Permissions: map[string][]string{}}
		}
	case "webhooks":
		title = "Webhooks"
		d.Webhooks = s.hgWebhooks(c)
		d.Events = hgWebhookEvents
		if id, _ := strconv.ParseInt(c.Query("id"), 10, 64); id > 0 {
			d.Webhook = s.hgWebhookByID(c, id)
			if d.Webhook != nil {
				d.Deliveries = s.hgDeliveries(c, id, 50)
				var lines []string
				for k, v := range d.Webhook.Headers {
					lines = append(lines, k+": "+v)
				}
				d.HeaderText = strings.Join(lines, "\n")
			}
		} else if c.Query("new") == "1" {
			d.Webhook = &hgWebhook{Enabled: true, Events: []string{"entry.create", "entry.update", "entry.delete", "entry.publish", "entry.unpublish"}}
		} else {
			d.Deliveries = s.hgDeliveries(c, 0, 20)
		}
		id, _ := strconv.ParseInt(c.Query("id"), 10, 64)
		d.Queue = s.hgQueueItems(c, id, 50)
	case "settings":
		title = "Settings"
	default:
		d.Page = ""
	}
	d.Title = title
	screen := hgSlug
	if page != "" {
		screen += "?page=" + page
	}
	s.adminPage(c, screen, title+" ‹ Pursae Headless", "toplevel_page_headlessgo headlessgo-admin", "admin/headlessgo", d, notices...)
}

// Content Manager ----------------------------------------------------------------------------------

const hgAdminPerPage = 20

func (s *Server) hgAdminList(c *gin.Context, d *hgAdminData) error {
	sc := d.Type
	d.Search = strings.TrimSpace(c.Query("s"))
	d.Status = c.Query("status")
	d.Sort = c.DefaultQuery("orderby", "updatedAt")
	d.Desc = c.DefaultQuery("order", "desc") == "desc"
	if sc.Localized {
		d.Locale = c.Query("locale")
		if !s.hgValidLocale(c, d.Locale) {
			d.Locale = s.hgDefaultLocale(c)
		}
	}
	d.PageNum, _ = strconv.Atoi(c.DefaultQuery("paged", "1"))
	if d.PageNum < 1 {
		d.PageNum = 1
	}
	for i := range sc.Fields {
		f := &sc.Fields[i]
		if f.scalar() && f.Type != "password" && f.Type != "json" && f.Type != "richtext" && f.Name != sc.TitleField() && len(d.Columns) < 4 {
			d.Columns = append(d.Columns, f)
		}
	}
	p := &hgReadParams{Status: "draft", Locale: d.Locale, Page: hgPagination{Page: d.PageNum, PageSize: hgAdminPerPage, WithCount: true}}
	if d.Search != "" {
		var or []any
		for _, f := range sc.Fields {
			if f.Type == "text" || f.Type == "uid" || f.Type == "email" || f.Type == "enumeration" || (f.Type == "richtext" && f.Format == "markdown") {
				or = append(or, map[string]any{f.Name: map[string]any{"$containsi": d.Search}})
			}
		}
		or = append(or, map[string]any{"documentId": map[string]any{"$eq": d.Search}})
		p.Filters = map[string]any{"$or": or}
	}
	sortKey := hgSortKey{Field: d.Sort, Desc: d.Desc}
	if f := sc.Field(d.Sort); f == nil && hgSystemFields[d.Sort].col == "" {
		sortKey = hgSortKey{Field: "updatedAt", Desc: true}
		d.Sort = "updatedAt"
	}
	p.Sort = []hgSortKey{sortKey}
	o := hgFindOpts{}
	switch d.Status {
	case "draft":
		o.Extra = "e.published_data IS NULL"
	case "published":
		o.Extra = "e.published_data IS NOT NULL AND e.published_data = e.data"
	case "modified":
		o.Extra = "e.published_data IS NOT NULL AND e.published_data <> e.data"
	default:
		d.Status = ""
	}
	list, total, err := s.hgFind(c, sc, p, o)
	if err != nil {
		return err
	}
	d.Total = total
	d.NumPages = max(1, (total+hgAdminPerPage-1)/hgAdminPerPage)
	tf := sc.TitleField()
	for _, e := range list {
		row := hgAdminRow{E: e, Status: e.Status(), Edit: hgAdminPath + "?page=entry&type=" + url.QueryEscape(sc.UID) + "&doc=" + e.DocumentID}
		if e.PublishAt != nil {
			row.Schedule = "Publishes " + e.PublishAt.In(s.loc()).Format("2006-01-02 15:04")
		} else if e.UnpublishAt != nil {
			row.Schedule = "Unpublishes " + e.UnpublishAt.In(s.loc()).Format("2006-01-02 15:04")
		}
		if sc.Localized {
			row.Edit += "&locale=" + url.QueryEscape(e.Locale)
		}
		if tf != "" {
			row.Title = hgCell(e.Data[tf])
		}
		if row.Title == "" {
			row.Title = e.DocumentID
		}
		for _, f := range d.Columns {
			row.Cells = append(row.Cells, hgCell(e.Data[f.Name]))
		}
		d.Rows = append(d.Rows, row)
	}
	q := url.Values{"page": {"entries"}, "type": {sc.UID}, "orderby": {d.Sort}}
	if d.Desc {
		q.Set("order", "desc")
	} else {
		q.Set("order", "asc")
	}
	for k, v := range map[string]string{"s": d.Search, "status": d.Status, "locale": d.Locale} {
		if v != "" {
			q.Set(k, v)
		}
	}
	d.QS = q.Encode()
	return nil
}

// hgCell is a short text of a value for the list table.
func hgCell(v any) string {
	var s string
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		s = t
	case float64:
		s = strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		if t {
			s = "true"
		} else {
			s = "false"
		}
	default:
		raw, _ := json.Marshal(t)
		s = string(raw)
	}
	if r := []rune(s); len(r) > 80 {
		s = string(r[:80]) + "…"
	}
	return s
}

// hgFailedSave is a rejected entry form: the input and the errors, shown again.
type hgFailedSave struct {
	Type        *hgSchema
	Doc, Locale string
	Data        map[string]any
	Errs        hgValidationError
}

// hgAdminEntry prepares the entry form; it reports false when it answered with a redirect.
func (s *Server) hgAdminEntry(c *gin.Context, d *hgAdminData, failed *hgFailedSave) bool {
	sc := d.Type
	reg := s.hgReg(c)
	d.Doc = c.Query("doc")
	if failed != nil {
		d.Doc = failed.Doc
	}
	if sc.IsSingle() {
		d.Doc = s.hgSingleDoc(c, sc.UID)
	}
	if sc.Localized {
		d.Locale = c.Query("locale")
		if failed != nil {
			d.Locale = failed.Locale
		}
		if !s.hgValidLocale(c, d.Locale) {
			d.Locale = s.hgDefaultLocale(c)
		}
	}
	data := map[string]any{}
	if d.Doc != "" {
		d.Versions = s.hgDocLocales(c, sc.UID, d.Doc)
		if len(d.Versions) == 0 {
			c.Redirect(http.StatusFound, hgAdminPath+"?page=entries&type="+url.QueryEscape(sc.UID)+"&hg-error="+url.QueryEscape("The entry no longer exists."))
			return false
		}
		if e := d.Versions[d.Locale]; e != nil {
			d.Entry = e
			data = map[string]any{}
			for k, v := range e.Data {
				data[k] = v
			}
			for i := range sc.Fields {
				if f := &sc.Fields[i]; f.MappedBy != "" { // inverse relation: the owning entries that point here
					if ids := s.hgInverseIDs(c, f, e.DocumentID, e.Locale, "draft"); len(ids) > 0 {
						data[f.Name] = hgAnyStrings(ids)
					}
				}
			}
			d.EntryStatus = e.Status()
			d.EntryTitle, _ = e.Data[sc.TitleField()].(string)
			d.PreviewURL = s.hgPreviewURL(c, sc, e)
			d.History = s.hgVersions(c, e.ID, 8)
			d.BaseStamp = hgLockStamp(e)
			d.ReviewState = e.ReviewState()
			d.CanReview = hgCanReview(currentUser(c))
			d.LockBeat = hgLockBeat
			if failed == nil {
				d.Lock = s.hgAcquireLock(c, e.ID, currentUser(c).ID)
			}
			if e.PublishAt != nil {
				d.PublishAt = e.PublishAt.In(s.loc()).Format("2006-01-02T15:04")
			}
			if e.UnpublishAt != nil {
				d.UnpublishAt = e.UnpublishAt.In(s.loc()).Format("2006-01-02T15:04")
			}
		} else if from := c.Query("copy"); from != "" && d.Versions[from] != nil {
			data = d.Versions[from].Data // a new translation filled from another locale
		}
	}
	if failed != nil {
		data, d.Errors = failed.Data, failed.Errs
	}
	// The editor's model: the schemas it needs, the values, and labels of media and relations.
	comps := map[string]*hgSchema{}
	for _, cs := range reg.Components() {
		comps[cs.UID] = cs
	}
	types := map[string]any{}
	for _, t := range reg.Types() {
		types[t.UID] = map[string]any{"displayName": t.DisplayName, "titleField": t.TitleField()}
	}
	media := map[string]any{}
	rels := map[string]any{}
	s.hgCollectRefs(c, reg, sc, data, media, rels, 0)
	data = hgMaskPasswords(reg, sc, data, 0)
	var errs []hgFieldError = d.Errors
	if errs == nil {
		errs = []hgFieldError{}
	}
	d.TimeZone = s.loc().String()
	model := map[string]any{"schema": sc, "components": comps, "types": types, "data": data, "media": media, "relations": rels,
		"errors": errs, "searchURL": hgAdminPath + "/search", "locale": d.Locale, "isNew": d.Entry == nil, "locked": d.Lock != nil}
	raw, _ := json.Marshal(model)
	d.EditorJSON = template.JS(raw)
	return true
}

// hgCollectRefs gathers the media and related entries of a value tree, so the editor can show
// thumbnails and titles: media id -> {url, title, thumb}, "target|id" -> label.
func (s *Server) hgCollectRefs(c *gin.Context, reg *hgRegistry, sc *hgSchema, data map[string]any, media, rels map[string]any, depth int) {
	if depth > 10 || data == nil {
		return
	}
	for _, f := range sc.Fields {
		v := data[f.Name]
		if v == nil {
			continue
		}
		switch f.Type {
		case "media":
			for _, x := range hgList(v) {
				if id, ok := hgToInt(x); ok {
					if a, err := s.store.GetAttachment(c, id); err == nil {
						media[itoa64(id)] = s.hgMediaBrief(c, a)
					}
				}
			}
		case "relation":
			for _, x := range hgList(v) {
				key := f.Target + "|" + fmt.Sprint(hgRelKey(x))
				rels[key] = s.hgRelLabel(c, reg, f.Target, x)
			}
		case "component":
			comp := reg.Get(f.Component)
			if comp == nil {
				continue
			}
			for _, item := range hgList(v) {
				if m, ok := item.(map[string]any); ok {
					s.hgCollectRefs(c, reg, comp, m, media, rels, depth+1)
				}
			}
		case "dynamiczone":
			for _, item := range hgList(v) {
				if m, ok := item.(map[string]any); ok {
					cu, _ := m["__component"].(string)
					if comp := reg.Get(cu); comp != nil {
						s.hgCollectRefs(c, reg, comp, m, media, rels, depth+1)
					}
				}
			}
		}
	}
}

func hgRelKey(x any) any {
	if f, ok := x.(float64); ok {
		return int64(f)
	}
	return x
}

func (s *Server) hgMediaBrief(c *gin.Context, a *store.Attachment) map[string]any {
	thumb := s.uploadURL(c, a.FilePath)
	if v := a.Variant("thumbnail"); v != nil {
		thumb = s.uploadURL(c, v.FilePath)
	}
	if !strings.HasPrefix(a.Mime, "image/") {
		thumb = ""
	}
	return map[string]any{"id": a.ID, "url": s.uploadURL(c, a.FilePath), "title": defaultStr(a.Title, a.Slug), "thumb": thumb, "mime": a.Mime}
}

// hgRelLabel is the label of a related object in the editor.
func (s *Server) hgRelLabel(c *gin.Context, reg *hgRegistry, target string, x any) string {
	switch {
	case hgIsWPTerm(target):
		if id, ok := hgToInt(x); ok {
			if t, err := s.store.GetTerm(c, id); err == nil {
				return t.Name + " #" + itoa64(id)
			}
		}
	case hgIsWPPost(target):
		if id, ok := hgToInt(x); ok {
			if p, err := s.store.GetPost(c, id); err == nil && currentUser(c).Can("read_post", p) {
				return defaultStr(p.Title, "(no title)") + " #" + itoa64(id)
			}
			return "#" + itoa64(id) // a post the user may not read: no title
		}
	case target == "wp:user":
		if id, ok := hgToInt(x); ok {
			if u, err := s.store.UserByID(c, id); err == nil {
				return hgUserLabel(currentUser(c), u.DisplayName, u.Login)
			}
		}
	default:
		doc, _ := x.(string)
		sc := reg.Get(target)
		if sc != nil && doc != "" {
			var data []byte
			if s.store.Pool.QueryRow(c, `SELECT data FROM hg_entries WHERE type_uid = $1 AND document_id = $2 ORDER BY (locale = $3) DESC, id LIMIT 1`,
				target, doc, s.hgDefaultLocale(c)).Scan(&data) == nil {
				var m map[string]any
				_ = json.Unmarshal(data, &m)
				if tf := sc.TitleField(); tf != "" {
					if t, ok := m[tf].(string); ok && t != "" {
						return t
					}
				}
			}
		}
	}
	return fmt.Sprint(x)
}

// hgUserLabel is a user in the relation picker: the display name, with the login name only for
// users who may list users (login names are half of a credential).
func hgUserLabel(viewer *store.User, name, login string) string {
	if viewer.Can("list_users", nil) {
		return name + " (" + login + ")"
	}
	return name
}

// hgAdminSearch answers the relation picker: GET /wp-admin/headlessgo/search?target=&q=. It lists
// what the user may read: WordPress posts and pages as on the Posts screen (without
// edit_others_posts: published ones and the user's own; private ones with read_private_posts),
// users without their login name unless the user may list users. Headless entries are listed
// like the Content Manager, which every user with edit_posts manages.
func (s *Server) hgAdminSearch(c *gin.Context) {
	u := currentUser(c)
	if !u.Can("edit_posts", nil) {
		c.JSON(http.StatusForbidden, []any{})
		return
	}
	target, q := c.Query("target"), strings.TrimSpace(c.Query("q"))
	out := []map[string]any{}
	switch {
	case hgIsWPTerm(target):
		tax, _ := hgWPTaxonomy(target)
		terms, _, err := s.store.QueryTerms(c, store.TermQuery{Taxonomy: tax, Search: q, OrderBy: "name", Order: "ASC", Page: 1, PerPage: 20})
		if err == nil {
			for _, t := range terms {
				out = append(out, map[string]any{"id": t.ID, "label": t.Name + " #" + itoa64(t.ID)})
			}
		}
	case hgIsWPPost(target):
		typ, _ := hgWPPostType(target)
		pq := store.PostQuery{Types: []string{typ}, Statuses: []string{"publish", "draft", "future", "private", "pending"},
			Search: q, PerPage: 20, Page: 1, OrderBy: "date", Order: "DESC"}
		readPrivate := u.Can("read_private_"+typ+"s", nil)
		if !readPrivate {
			pq.Statuses = []string{"publish", "draft", "future", "pending"}
		}
		if !u.Can("edit_others_"+typ+"s", nil) {
			pq.ReadableBy, pq.ReadablePrivate = u.ID, readPrivate
		}
		posts, _, err := s.store.QueryPosts(c, pq)
		if err == nil {
			for _, p := range posts {
				out = append(out, map[string]any{"id": p.ID, "label": defaultStr(p.Title, "(no title)") + " #" + itoa64(p.ID), "status": p.Status})
			}
		}
	case target == "wp:user":
		cond := `display_name ILIKE '%' || $1 || '%' OR nicename ILIKE '%' || $1 || '%'`
		if u.Can("list_users", nil) {
			cond += ` OR login ILIKE '%' || $1 || '%'`
		}
		rows, err := s.store.Pool.Query(c, `SELECT id, display_name, login FROM users WHERE $1 = '' OR `+cond+` ORDER BY display_name LIMIT 20`, q)
		if err == nil {
			for rows.Next() {
				var id int64
				var name, login string
				if rows.Scan(&id, &name, &login) == nil {
					out = append(out, map[string]any{"id": id, "label": hgUserLabel(u, name, login)})
				}
			}
			rows.Close()
		}
	default:
		sc := s.hgReg(c).Get(target)
		if sc == nil || sc.IsComponent() {
			c.JSON(http.StatusOK, out)
			return
		}
		loc := ""
		if sc.Localized {
			loc = c.Query("locale")
			if !s.hgValidLocale(c, loc) {
				loc = s.hgDefaultLocale(c)
			}
		}
		p := &hgReadParams{Status: "draft", Locale: loc, Page: hgPagination{Page: 1, PageSize: 20}, Sort: []hgSortKey{{Field: "updatedAt", Desc: true}}}
		tf := sc.TitleField()
		if q != "" {
			or := []any{map[string]any{"documentId": map[string]any{"$eq": q}}}
			if tf != "" {
				or = append(or, map[string]any{tf: map[string]any{"$containsi": q}})
			}
			p.Filters = map[string]any{"$or": or}
		}
		list, _, err := s.hgFind(c, sc, p, hgFindOpts{})
		if err == nil {
			for _, e := range list {
				label := e.DocumentID
				if tf != "" {
					if t, ok := e.Data[tf].(string); ok && t != "" {
						label = t
					}
				}
				out = append(out, map[string]any{"id": e.DocumentID, "label": label, "status": e.Status()})
			}
		}
	}
	c.JSON(http.StatusOK, out)
}

// Builder ----------------------------------------------------------------------------------------

func (s *Server) hgAdminBuilder(c *gin.Context, d *hgAdminData) {
	reg := s.hgReg(c)
	d.NewKind = c.Query("new")
	if d.NewKind != hgKindCollection && d.NewKind != hgKindSingle && d.NewKind != hgKindComponent {
		d.NewKind = ""
	}
	if uid := c.Query("type"); uid != "" {
		d.Type = reg.Get(uid)
	}
	if d.Type == nil && d.NewKind == "" && len(reg.order) > 0 {
		d.Type = reg.order[0]
		if ts := reg.Types(); len(ts) > 0 {
			d.Type = ts[0]
		}
	}
	types := []map[string]any{}
	for _, t := range reg.order {
		types = append(types, map[string]any{"uid": t.UID, "kind": t.Kind, "displayName": t.DisplayName})
	}
	var cur any
	if d.Type != nil {
		cur = d.Type
	} else if d.NewKind != "" {
		cur = &hgSchema{Kind: d.NewKind, DraftAndPublish: d.NewKind != hgKindComponent, Fields: []hgField{}}
	}
	// The labels the builder shows, in the user's language (the page is translated after it is
	// rendered, but these travel as JSON).
	fieldTypes := make([]struct{ Key, Label, Help, Icon string }, len(hgFieldTypes))
	for i, ft := range hgFieldTypes {
		fieldTypes[i] = ft
		fieldTypes[i].Label, fieldTypes[i].Help = s.__(c, ft.Label), s.__(c, ft.Help)
	}
	relations := make([]struct{ Key, Label string }, len(hgRelations))
	for i, r := range hgRelations {
		relations[i] = struct{ Key, Label string }{r.Key, s.__(c, r.Label)}
	}
	wpTargetList := s.hgWPTargetList(c)
	wpTargets := make([]struct{ Key, Label string }, len(wpTargetList))
	for i, r := range wpTargetList {
		wpTargets[i] = struct{ Key, Label string }{r.Key, s.__(c, r.Label)}
	}
	model := map[string]any{"schema": cur, "isNew": d.Type == nil, "all": types, "fieldTypes": fieldTypes, "relations": relations,
		"wpTargets": wpTargets, "golylang": s.golylangOn()}
	raw, _ := json.Marshal(model)
	d.BuilderJSON = template.JS(raw)
}

// Posts ------------------------------------------------------------------------------------------

func (s *Server) hgAdminPost(c *gin.Context, page string) {
	u := currentUser(c)
	reg := s.hgReg(c)
	do := c.PostForm("do")
	back := url.Values{"page": {page}}
	switch do {
	case "save_entry", "delete_entry", "publish_entry", "unpublish_entry", "discard_entry", "schedule_entry", "restore_version", "bulk_entries", "lock_takeover", "submit_review", "approve_review", "reject_review":
		sc := reg.Get(c.PostForm("type"))
		if sc == nil || sc.IsComponent() {
			hgRedirect(c, url.Values{}, "", errors.New("Unknown content type."))
			return
		}
		s.hgAdminEntryPost(c, sc, do)
		return
	}
	if !u.Can("manage_options", nil) {
		c.String(http.StatusForbidden, "Sorry, you are not allowed to do that.")
		return
	}
	switch do {
	case "save_schema":
		var sc hgSchema
		if err := json.Unmarshal([]byte(c.PostForm("schema")), &sc); err != nil {
			hgRedirect(c, url.Values{"page": {"builder"}}, "", errors.New("The schema could not be read."))
			return
		}
		isNew := c.PostForm("is_new") == "1"
		var err error
		if c.PostForm("is_new") == "1" {
			sc.UID = ""
			hgNormalizeSchema(&sc)
			if sc.UID != "" && reg.Get(sc.UID) != nil {
				err = fmt.Errorf("A content type or component with the ID %q already exists.", sc.UID)
			}
		}
		if err == nil {
			err = s.hgSaveSchema(c, &sc)
		}
		if err != nil {
			q := url.Values{"page": {"builder"}}
			if !isNew {
				q.Set("type", sc.UID)
			} else {
				q.Set("new", sc.Kind)
			}
			hgRedirect(c, q, "", err)
			return
		}
		s.hgEmitSchema(c, map[bool]string{true: "create", false: "update"}[isNew], &sc)
		hgRedirect(c, url.Values{"page": {"builder"}, "type": {sc.UID}}, sc.DisplayName+" saved.", nil)
	case "delete_schema":
		uid := c.PostForm("uid")
		sc := reg.Get(uid)
		if err := s.hgDeleteSchema(c, uid); err != nil {
			hgRedirect(c, url.Values{"page": {"builder"}, "type": {uid}}, "", err)
			return
		}
		if sc != nil {
			s.hgEmitSchema(c, "delete", sc)
		}
		hgRedirect(c, url.Values{"page": {"builder"}}, "Deleted.", nil)
	case "import_schemas":
		raw := []byte(c.PostForm("json"))
		if fh, err := c.FormFile("file"); err == nil {
			if f, err := fh.Open(); err == nil {
				raw, _ = io.ReadAll(io.LimitReader(f, 4<<20))
				f.Close()
			}
		}
		n, err := s.hgImportSchemas(c, raw)
		if err != nil {
			hgRedirect(c, url.Values{"page": {"settings"}}, "", err)
			return
		}
		hgRedirect(c, url.Values{"page": {"builder"}}, fmt.Sprintf("%d schemas imported.", n), nil)
	case "import_entries":
		fh, err := c.FormFile("file")
		if err != nil {
			hgRedirect(c, url.Values{"page": {"settings"}}, "", errors.New("Choose a file to import."))
			return
		}
		f, err := fh.Open()
		if err != nil {
			hgRedirect(c, url.Values{"page": {"settings"}}, "", err)
			return
		}
		defer f.Close()
		res, err := s.hgImportEntries(c, io.LimitReader(f, 256<<20), c.PostForm("overwrite") == "1", u.ID)
		if err != nil {
			hgRedirect(c, url.Values{"page": {"settings"}}, "", err)
			return
		}
		msg := fmt.Sprintf("Entries imported: %d created, %d updated, %d skipped.", res.Created, res.Updated, res.Skipped)
		if len(res.Warnings) > 0 {
			msg += " " + strings.Join(res.Warnings[:min(3, len(res.Warnings))], " ")
		}
		hgRedirect(c, url.Values{"page": {"settings"}}, msg, nil)
	case "save_settings":
		st := s.hgLoad()
		st.FrontMode = c.PostForm("front_mode")
		st.RedirectURL = strings.TrimSpace(c.PostForm("redirect_url"))
		st.RedirectKeepPath = c.PostForm("redirect_keep_path") == "1"
		st.AllowEditors = c.PostForm("allow_editors") == "1"
		st.CORSOrigins = lines(c.PostForm("cors_origins"))
		st.PreviewTTL, _ = strconv.Atoi(c.PostForm("preview_ttl"))
		st.DefaultLocale = c.PostForm("default_locale")
		st.DefaultPageSize, _ = strconv.Atoi(c.PostForm("default_page_size"))
		st.MaxPageSize, _ = strconv.Atoi(c.PostForm("max_page_size"))
		st.GraphQL = c.PostForm("graphql") == "1"
		st.SchemaWebhooks = c.PostForm("schema_webhooks") == "1"
		st.WebhookAttempts, _ = strconv.Atoi(c.PostForm("webhook_attempts"))
		st.RateLimit, _ = strconv.Atoi(c.PostForm("rate_limit"))
		st.CacheMaxAge, _ = strconv.Atoi(c.PostForm("cache_max_age"))
		st.NoPublicIntro = c.PostForm("no_public_introspection") == "1"
		if st.FrontMode == "redirect" && !hgHTTPURL(st.RedirectURL) {
			hgRedirect(c, back, "", errors.New("The redirect URL must be a full http:// or https:// address."))
			return
		}
		if c.PostForm("regenerate_secret") == "1" {
			st.PreviewSecret = ""
		}
		if st.PreviewSecret == "" {
			st.PreviewSecret = hgRandomSecret()
		}
		pub := map[string][]string{}
		for _, t := range reg.Types() {
			for _, a := range hgActions {
				if c.PostForm("public["+t.UID+"]["+a+"]") == "1" {
					pub[t.UID] = append(pub[t.UID], a)
				}
			}
		}
		st.Public = pub
		st.UserAuth = c.PostForm("user_auth") == "1"
		st.UserRegister = c.PostForm("user_register") == "1"
		st.UserTokenTTL, _ = strconv.Atoi(c.PostForm("user_token_ttl"))
		authd, owner := map[string][]string{}, map[string][]string{}
		for _, t := range reg.Types() {
			for _, a := range hgActions {
				if c.PostForm("authd["+t.UID+"]["+a+"]") == "1" {
					authd[t.UID] = append(authd[t.UID], a)
				}
			}
			for _, a := range []string{"update", "delete"} {
				if c.PostForm("owner["+t.UID+"]["+a+"]") == "1" {
					owner[t.UID] = append(owner[t.UID], a)
				}
			}
		}
		st.Authenticated, st.OwnerOnly = authd, owner
		if err := s.hgSaveSettings(c, st); err != nil {
			hgRedirect(c, back, "", err)
			return
		}
		s.purgeAll(c)
		hgRedirect(c, back, "Settings saved.", nil)
	case "save_token":
		id, _ := strconv.ParseInt(c.PostForm("id"), 10, 64)
		t := &hgToken{ID: id, Name: c.PostForm("name"), Description: c.PostForm("description"), Kind: c.PostForm("kind"),
			Drafts: c.PostForm("drafts") == "1", Upload: c.PostForm("upload") == "1", Permissions: map[string][]string{}}
		for _, ty := range reg.Types() {
			for _, a := range hgActions {
				if c.PostForm("perm["+ty.UID+"]["+a+"]") == "1" {
					t.Permissions[ty.UID] = append(t.Permissions[ty.UID], a)
				}
			}
		}
		if id == 0 {
			if days, _ := strconv.Atoi(c.PostForm("duration")); days > 0 {
				exp := time.Now().Add(time.Duration(days) * 24 * time.Hour)
				t.ExpiresAt = &exp
			}
			tok, err := s.hgCreateToken(c, t)
			if err != nil {
				hgRedirect(c, url.Values{"page": {"tokens"}, "new": {"1"}}, "", err)
				return
			}
			s.hgShowToken(c, tok)
			return
		}
		if err := s.hgUpdateToken(c, t); err != nil {
			hgRedirect(c, url.Values{"page": {"tokens"}, "id": {itoa64(id)}}, "", err)
			return
		}
		hgRedirect(c, url.Values{"page": {"tokens"}}, "Token saved.", nil)
	case "regenerate_token":
		id, _ := strconv.ParseInt(c.PostForm("id"), 10, 64)
		tok, err := s.hgRegenerateToken(c, id)
		if err != nil {
			hgRedirect(c, url.Values{"page": {"tokens"}}, "", errors.New("The token could not be regenerated."))
			return
		}
		s.hgShowToken(c, tok)
	case "delete_token":
		id, _ := strconv.ParseInt(c.PostForm("id"), 10, 64)
		_ = s.hgDeleteToken(c, id)
		hgRedirect(c, url.Values{"page": {"tokens"}}, "Token deleted.", nil)
	case "save_webhook":
		id, _ := strconv.ParseInt(c.PostForm("id"), 10, 64)
		w := &hgWebhook{ID: id, Name: c.PostForm("name"), URL: c.PostForm("url"), Events: c.PostFormArray("events[]"),
			Secret: strings.TrimSpace(c.PostForm("secret")), Enabled: c.PostForm("enabled") == "1", Headers: map[string]string{}}
		for _, l := range lines(c.PostForm("headers")) {
			if k, v, ok := strings.Cut(l, ":"); ok {
				w.Headers[strings.TrimSpace(k)] = strings.TrimSpace(v)
			}
		}
		if err := s.hgSaveWebhook(c, w); err != nil {
			q := url.Values{"page": {"webhooks"}}
			if id > 0 {
				q.Set("id", itoa64(id))
			} else {
				q.Set("new", "1")
			}
			hgRedirect(c, q, "", err)
			return
		}
		hgRedirect(c, url.Values{"page": {"webhooks"}, "id": {itoa64(w.ID)}}, "Webhook saved.", nil)
	case "retry_delivery":
		id, _ := strconv.ParseInt(c.PostForm("id"), 10, 64)
		q := url.Values{"page": {"webhooks"}}
		if w, _ := strconv.ParseInt(c.PostForm("webhook"), 10, 64); w > 0 {
			q.Set("id", itoa64(w))
		}
		if err := s.hgRetryDelivery(c, id); err != nil {
			hgRedirect(c, q, "", errors.New("This delivery cannot be retried."))
			return
		}
		hgRedirect(c, q, "Delivery queued again.", nil)
	case "delete_webhook":
		id, _ := strconv.ParseInt(c.PostForm("id"), 10, 64)
		_ = s.hgDeleteWebhook(c, id)
		hgRedirect(c, url.Values{"page": {"webhooks"}}, "Webhook deleted.", nil)
	case "test_webhook":
		id, _ := strconv.ParseInt(c.PostForm("id"), 10, 64)
		w := s.hgWebhookByID(c, id)
		if w == nil {
			hgRedirect(c, url.Values{"page": {"webhooks"}}, "", errors.New("Unknown webhook."))
			return
		}
		res := s.hgTestWebhook(c, w)
		q := url.Values{"page": {"webhooks"}, "id": {itoa64(id)}}
		if res.Success {
			hgRedirect(c, q, fmt.Sprintf("Test sent: %d in %d ms.", res.StatusCode, res.DurationMS), nil)
		} else if res.Error != "" {
			hgRedirect(c, q, "", errors.New("Test failed: "+res.Error))
		} else {
			hgRedirect(c, q, "", fmt.Errorf("Test failed: the endpoint answered %d.", res.StatusCode))
		}
	default:
		hgRedirect(c, back, "", errors.New("Unknown action."))
	}
}

func hgRandomSecret() string {
	return strings.ReplaceAll(strings.ReplaceAll(hgNewDocumentID()+hgNewDocumentID(), "-", ""), "_", "")
}

// hgShowToken renders the token list with the new token shown once (never in a URL).
func (s *Server) hgShowToken(c *gin.Context, tok string) {
	d := s.hgBaseData(c, "tokens")
	d.Title = "API Tokens"
	d.Tokens = s.hgTokens(c)
	d.NewToken = tok
	hgAssetsOnce.Do(hgRegisterAssets)
	s.pageAssets(c).Style("headlessgo")
	c.Header("Cache-Control", "no-store")
	s.adminPage(c, hgSlug+"?page=tokens", "API Tokens ‹ Pursae Headless", "toplevel_page_headlessgo headlessgo-admin", "admin/headlessgo", d,
		notice{Type: "success", Message: "<strong>Make sure to copy this token now.</strong> You won't be able to see it again."})
}

// hgAdminEntryPost saves, publishes, unpublishes, discards or deletes an entry from the form.
func (s *Server) hgAdminEntryPost(c *gin.Context, sc *hgSchema, do string) {
	u := currentUser(c)
	doc := c.PostForm("doc")
	if sc.IsSingle() {
		doc = s.hgSingleDoc(c, sc.UID)
	}
	loc, err := s.hgEntryLocale(c, sc, c.PostForm("locale"))
	if err != nil {
		hgRedirect(c, url.Values{"page": {"entries"}, "type": {sc.UID}}, "", err)
		return
	}
	entryURL := func(doc string) url.Values {
		q := url.Values{"page": {"entry"}, "type": {sc.UID}}
		if doc != "" && !sc.IsSingle() {
			q.Set("doc", doc)
		}
		if loc != "" {
			q.Set("locale", loc)
		}
		return q
	}
	if err := s.hgAdminCaps(c, u, sc, do, doc, loc); err != nil {
		hgRedirect(c, entryURL(doc), "", err)
		return
	}
	switch do {
	case "save_entry", "publish_entry", "unpublish_entry", "discard_entry", "schedule_entry", "restore_version", "delete_entry":
		base := ""
		if do == "save_entry" {
			base = c.PostForm("base")
		}
		if err := s.hgGuardWrite(c, sc, doc, loc, u.ID, base); err != nil {
			hgRedirect(c, entryURL(doc), "", err)
			return
		}
	}
	switch do {
	case "lock_takeover":
		e, err := s.hgGetEntry(c, sc.UID, doc, loc)
		if err == nil {
			err = s.hgTakeOverLock(c, e.ID, u.ID)
		}
		if err != nil {
			hgRedirect(c, entryURL(doc), "", errors.New("The entry no longer exists."))
			return
		}
		hgRedirect(c, entryURL(doc), "You are now editing this entry.", nil)
	case "submit_review", "approve_review", "reject_review":
		var err error
		msg := "Submitted for review."
		switch do {
		case "submit_review":
			if err = s.hgGuardWrite(c, sc, doc, loc, u.ID, ""); err == nil {
				_, err = s.hgSubmitForReview(c, sc, doc, loc, u.ID)
			}
		default:
			if !hgCanReview(u) {
				hgRedirect(c, entryURL(doc), "", errors.New("You are not allowed to review entries."))
				return
			}
			note := strings.TrimSpace(c.PostForm("note"))
			_, err = s.hgDecideReview(c, sc, doc, loc, u.ID, do == "approve_review", note)
			msg = "Approved. The entry can now be published."
			if do == "reject_review" {
				msg = "Rejected."
			}
		}
		if err != nil {
			var ve hgValidationError
			if errors.As(err, &ve) {
				err = errors.New("The draft cannot be submitted yet: " + ve.Error())
			}
			hgRedirect(c, entryURL(doc), "", err)
			return
		}
		hgRedirect(c, entryURL(doc), msg, nil)
	case "save_entry":
		var data map[string]any
		if err := json.Unmarshal([]byte(c.PostForm("payload")), &data); err != nil || data == nil {
			hgRedirect(c, entryURL(doc), "", errors.New("The form data could not be read."))
			return
		}
		publish := c.PostForm("publish") == "1"
		wantPublish := publish && sc.ReviewWorkflow && sc.DraftAndPublish
		if wantPublish {
			publish = false // the draft is saved first; it goes live only when it is approved
		}
		e, events, err := s.hgSaveEntry(c, sc, doc, data, hgSaveOpts{Locale: loc, Publish: publish, UserID: u.ID, User: u})
		if err != nil {
			var ve hgValidationError
			if errors.As(err, &ve) {
				s.hgRender(c, "entry", &hgFailedSave{Type: sc, Doc: doc, Locale: loc, Data: data, Errs: ve})
				return
			}
			hgRedirect(c, entryURL(doc), "", err)
			return
		}
		for _, ev := range events {
			s.hgEmit(c, ev, sc, e)
		}
		msg := "Saved."
		if publish || !sc.DraftAndPublish {
			msg = "Published."
		}
		if wantPublish {
			pe, perr := s.hgPublishEntry(c, sc, e.DocumentID, loc, u.ID)
			if perr != nil {
				hgRedirect(c, entryURL(e.DocumentID), "", errors.New("Saved as a draft, but not published: "+perr.Error()))
				return
			}
			s.hgEmit(c, "entry.publish", sc, pe)
			msg = "Published."
		}
		hgRedirect(c, entryURL(e.DocumentID), msg, nil)
	case "publish_entry":
		e, err := s.hgPublishEntry(c, sc, doc, loc, u.ID)
		if err != nil {
			hgRedirect(c, entryURL(doc), "", fmt.Errorf("The entry cannot be published: %v", err))
			return
		}
		s.hgEmit(c, "entry.publish", sc, e)
		hgRedirect(c, entryURL(doc), "Published.", nil)
	case "unpublish_entry":
		e, err := s.hgUnpublishEntry(c, sc, doc, loc)
		if err != nil {
			hgRedirect(c, entryURL(doc), "", err)
			return
		}
		s.hgEmit(c, "entry.unpublish", sc, e)
		hgRedirect(c, entryURL(doc), "Unpublished.", nil)
	case "discard_entry":
		if err := s.hgDiscardDraft(c, sc, doc, loc); err != nil {
			hgRedirect(c, entryURL(doc), "", errors.New("There is no published version to go back to."))
			return
		}
		hgRedirect(c, entryURL(doc), "Changes discarded.", nil)
	case "schedule_entry":
		var at [2]*time.Time
		if c.PostForm("clear") != "1" {
			for i, k := range []string{"publish_at", "unpublish_at"} {
				v := strings.TrimSpace(c.PostForm(k))
				if v == "" {
					continue
				}
				t, err := time.ParseInLocation("2006-01-02T15:04", v, s.loc())
				if err != nil {
					hgRedirect(c, entryURL(doc), "", errors.New("Enter the dates as a date and a time."))
					return
				}
				at[i] = &t
			}
		}
		if _, err := s.hgSetSchedule(c, sc, doc, loc, at[0], at[1]); err != nil {
			if errors.Is(err, errHGNotFound) {
				err = errors.New("Save the entry first.")
			}
			hgRedirect(c, entryURL(doc), "", err)
			return
		}
		msg := "Schedule saved."
		if at[0] == nil && at[1] == nil {
			msg = "Schedule cleared."
		}
		hgRedirect(c, entryURL(doc), msg, nil)
	case "restore_version":
		vid, _ := strconv.ParseInt(c.PostForm("version"), 10, 64)
		v, err := s.hgVersionByID(c, vid)
		if err != nil || v.TypeUID != sc.UID || v.DocumentID != doc || v.Locale != loc {
			hgRedirect(c, entryURL(doc), "", errors.New("This version no longer exists."))
			return
		}
		dropped := hgVersionDropped(sc, v.Data)
		e, skipped, err := s.hgRestoreVersion(c, sc, vid, u.ID, c.PostForm("partial") == "1")
		if err != nil {
			var ve hgValidationError
			if errors.As(err, &ve) {
				back := url.Values{"page": {"history"}, "type": {sc.UID}, "doc": {doc}, "v": {strconv.FormatInt(vid, 10)}}
				if loc != "" {
					back.Set("locale", loc)
				}
				hgRedirect(c, back, "", errors.New("This version does not fit the current content type, nothing was restored: "+ve.Error()))
				return
			}
			hgRedirect(c, entryURL(doc), "", err)
			return
		}
		s.hgEmit(c, "entry.update", sc, e)
		// One message per case, so each is translated as a whole sentence.
		kept, gone := strings.Join(skipped, ", "), strings.Join(dropped, ", ")
		var msg string
		switch {
		case kept != "" && gone != "":
			msg = i18n.Sprintf("The version was restored as the draft, except for %1$s: those values no longer fit the content type and kept their current values. Left out, the content type no longer has these fields: %2$s.", kept, gone)
		case kept != "":
			msg = i18n.Sprintf("The version was restored as the draft, except for %s: those values no longer fit the content type and kept their current values.", kept)
		case gone != "":
			msg = i18n.Sprintf("The version was restored as the draft. Left out, the content type no longer has these fields: %s.", gone)
		default:
			msg = "The version was restored as the draft."
		}
		hgRedirect(c, entryURL(doc), msg, nil)
	case "bulk_entries":
		back := url.Values{"page": {"entries"}, "type": {sc.UID}}
		if loc != "" {
			back.Set("locale", loc)
		}
		if st := c.PostForm("status"); st != "" {
			back.Set("status", st)
		}
		action := c.PostForm("bulk_action")
		docs := c.PostFormArray("docs[]")
		if len(docs) == 0 {
			hgRedirect(c, back, "", errors.New("Select entries first."))
			return
		}
		var allowed []string
		for _, d := range docs {
			if s.hgAdminCaps(c, u, sc, "bulk:"+action, d, loc) == nil {
				allowed = append(allowed, d)
			}
		}
		if len(allowed) == 0 {
			hgRedirect(c, back, "", errors.New("You are not allowed to do this with the selected entries."))
			return
		}
		docs = allowed
		res, err := s.hgBulk(c, sc, action, docs, loc, c.PostForm("all_locales") == "1", u.ID)
		if err != nil {
			hgRedirect(c, back, "", err)
			return
		}
		if res.Done == 0 && len(res.Failed) > 0 {
			hgRedirect(c, back, "", errors.New(res.Message(action)))
			return
		}
		hgRedirect(c, back, res.Message(action), nil)
	case "delete_entry":
		target := loc
		if c.PostForm("all_locales") == "1" || !sc.Localized {
			target = "*"
		}
		deleted, err := s.hgDeleteEntries(c, sc, doc, target, u.ID)
		if err != nil {
			hgRedirect(c, url.Values{"page": {"entries"}, "type": {sc.UID}}, "", errors.New("The entry no longer exists."))
			return
		}
		for _, e := range deleted {
			s.hgEmit(c, "entry.delete", sc, e)
		}
		if sc.IsSingle() {
			hgRedirect(c, url.Values{"page": {"entry"}, "type": {sc.UID}}, "Deleted.", nil)
			return
		}
		hgRedirect(c, url.Values{"page": {"entries"}, "type": {sc.UID}}, "Deleted.", nil)
	}
}

// hgMaskPasswords copies a value tree with every password replaced by true ("set"): neither the
// stored hash nor a rejected plain password is printed into the page.
func hgMaskPasswords(reg *hgRegistry, sc *hgSchema, data map[string]any, depth int) map[string]any {
	if data == nil || depth > 10 {
		return data
	}
	out := make(map[string]any, len(data))
	for k, v := range data {
		out[k] = v
	}
	for _, f := range sc.Fields {
		v, ok := out[f.Name]
		if !ok || v == nil {
			continue
		}
		switch f.Type {
		case "password":
			out[f.Name] = !hgEmpty(v)
		case "component", "dynamiczone":
			var list []any
			for _, item := range hgList(v) {
				m, ok := item.(map[string]any)
				if !ok {
					continue
				}
				cu := f.Component
				if f.Type == "dynamiczone" {
					cu, _ = m["__component"].(string)
				}
				if comp := reg.Get(cu); comp != nil {
					m = hgMaskPasswords(reg, comp, m, depth+1)
				}
				list = append(list, m)
			}
			if f.Type == "component" && !f.Repeatable {
				if len(list) > 0 {
					out[f.Name] = list[0]
				}
			} else {
				if list == nil {
					list = []any{}
				}
				out[f.Name] = list
			}
		}
	}
	return out
}

// hgAdminHistory prepares the history screen of a document version: every kept version, the
// chosen one (?v=, else the newest) compared with the current draft. It reports false when the
// entry does not exist.
func (s *Server) hgAdminHistory(c *gin.Context, d *hgAdminData) bool {
	sc := d.Type
	d.Doc = c.Query("doc")
	if sc.IsSingle() {
		d.Doc = s.hgSingleDoc(c, sc.UID)
	}
	loc, err := s.hgEntryLocale(c, sc, c.Query("locale"))
	if err != nil {
		return false
	}
	d.Locale = loc
	e, err := s.hgGetEntry(c, sc.UID, d.Doc, loc)
	if err != nil {
		return false
	}
	d.Entry = e
	d.EntryStatus = e.Status()
	d.EntryTitle, _ = e.Data[sc.TitleField()].(string)
	d.History = s.hgVersions(c, e.ID, hgHistoryKeep)
	vid, _ := strconv.ParseInt(c.Query("v"), 10, 64)
	for _, v := range d.History {
		if v.ID == vid || (vid == 0 && d.Version == nil) {
			d.Version = v
		}
	}
	if d.Version != nil {
		d.Diff = hgDiff(s.hgReg(c), sc, d.Version.Data, e.Data)
		data := map[string]any{}
		for k, val := range d.Version.Data {
			if f := sc.Field(k); f != nil && f.MappedBy == "" {
				data[k] = val
			}
		}
		d.RestoreErrs = s.hgCheckRestored(c, sc, e.DocumentID, e.Locale, data)
		d.RestoreDropped = hgVersionDropped(sc, d.Version.Data)
	}
	return true
}

// hgAdminGraphQL runs a query of the playground as the logged-in administrator (full access):
// POST /wp-admin/headlessgo/graphql with the nonce in X-HG-Nonce.
func (s *Server) hgAdminGraphQL(c *gin.Context) {
	u := currentUser(c)
	if !u.Can("manage_options", nil) || !s.checkNonceValue(c, c.GetHeader("X-HG-Nonce"), hgNonce) {
		c.JSON(http.StatusForbidden, map[string]any{"data": nil, "errors": []any{map[string]any{"message": s.__(c, "Sorry, you are not allowed to do that.")}}})
		return
	}
	var req hgGQLRequest
	raw, _ := io.ReadAll(io.LimitReader(c.Request.Body, 1<<20))
	if err := json.Unmarshal(raw, &req); err != nil {
		c.JSON(http.StatusBadRequest, map[string]any{"data": nil, "errors": []any{map[string]any{"message": s.__(c, "The body must be JSON: {\"query\": \"...\"}")}}})
		return
	}
	a := &hgAuth{Kind: "token", Token: &hgToken{Kind: "full-access", Permissions: map[string][]string{}}}
	status, out := s.hgGQLExecute(c, a, s.hgLoad(), req)
	s.hgGQLTranslateErrors(c, out)
	c.Header("Cache-Control", "no-store")
	c.JSON(status, out)
}

// hgGQLTranslateErrors shows the playground's error messages (validation, syntax, access) in the
// user's language. The public endpoint answers in English, as GraphQL servers do: clients match the
// messages of the specification.
func (s *Server) hgGQLTranslateErrors(c *gin.Context, out map[string]any) {
	cat := s.catalog(c)
	if cat.Empty() {
		return
	}
	list, _ := out["errors"].([]any)
	for _, e := range list {
		if m, ok := e.(map[string]any); ok {
			if msg, ok := m["message"].(string); ok {
				m["message"] = cat.Text(msg)
			}
		}
	}
}

// hgAdminCaps maps an entry action of the admin screen to WordPress capabilities: publishing,
// unpublishing and scheduling need publish_posts, changing another user's entry needs
// edit_others_posts, deleting needs delete_posts (delete_others_posts for another user's entry).
// Taking over another editor's lock and submitting for review are changes too (edit_others_posts
// for another user's entry). Administrators (manage_options) are always allowed.
func (s *Server) hgAdminCaps(c *gin.Context, u *store.User, sc *hgSchema, do, doc, loc string) error {
	if u.Can("manage_options", nil) {
		return nil
	}
	action := strings.TrimPrefix(do, "bulk:")
	if sc.IsSingle() {
		doc = s.hgSingleDoc(c, sc.UID)
	}
	// Deleting, and a bulk action with "all languages", act on every locale version of the document.
	all := !sc.Localized || c.PostForm("all_locales") == "1" && (strings.HasPrefix(do, "bulk:") || action == "delete_entry")
	own := s.hgOwnsDoc(c, u, sc, doc, loc, all)
	deny := errors.New("Sorry, you are not allowed to do that.")
	switch action {
	case "approve_review", "reject_review":
		return nil // gated by hgCanReview
	case "lock_takeover", "submit_review":
		if !own && !u.Can("edit_others_posts", nil) {
			return deny
		}
	case "publish_entry", "unpublish_entry", "schedule_entry", "publish", "unpublish":
		if !u.Can("publish_posts", nil) || (!own && !u.Can("edit_others_posts", nil)) {
			return deny
		}
	case "delete_entry", "delete":
		if !u.Can("delete_posts", nil) || (!own && !u.Can("delete_others_posts", nil)) {
			return deny
		}
	case "save_entry":
		if !own && !u.Can("edit_others_posts", nil) {
			return deny
		}
		// A reviewed type's Publish button still publishes (an approved draft): the same right as publish_entry.
		if c.PostForm("publish") == "1" && !u.Can("publish_posts", nil) {
			return deny
		}
	default: // discard_entry, restore_version
		if !own && !u.Can("edit_others_posts", nil) {
			return deny
		}
	}
	return nil
}

// hgOwnsDoc reports whether u owns what an action on a document changes: a new entry (no document
// yet) is theirs; otherwise the locale version addressed (or, when it does not exist yet, every
// version of the document) must have been created by u, and with all every locale version must.
// A document that does not exist is not owned: a missing version is never a reason to skip the
// edit_others_posts / delete_others_posts check.
func (s *Server) hgOwnsDoc(ctx context.Context, u *store.User, sc *hgSchema, doc, loc string, all bool) bool {
	if doc == "" {
		return true
	}
	versions := s.hgDocLocales(ctx, sc.UID, doc)
	if len(versions) == 0 {
		return false
	}
	if !all {
		if e, ok := versions[loc]; ok {
			return e.CreatedBy == u.ID
		}
	}
	for _, e := range versions {
		if e.CreatedBy != u.ID {
			return false
		}
	}
	return true
}
