package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"html/template"
	"math"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"wpgo/internal/links"
	"wpgo/internal/store"
	"wpgo/internal/wp"
)

func itoa64(n int64) string { return strconv.FormatInt(n, 10) }

type typeLabels struct {
	Type, Name, Singular, AddNew, Search, NotFound, NotFoundTrash string
}

func labelsFor(typ string) typeLabels {
	if typ == "page" {
		return typeLabels{"page", "Pages", "Page", "Add Page", "Search Pages", "No pages found.", "No pages found in Trash."}
	}
	return typeLabels{"post", "Posts", "Post", "Add Post", "Search Posts", "No posts found.", "No posts found in Trash."}
}

type statusLink struct {
	Key, Label, URL string
	Count           int
	Current         bool
}

type postRow struct {
	ID                   int64
	Title                string
	Depth                string // "— " per hierarchy level for pages
	States               []string
	EditURL, ViewURL     string
	TrashURL, RestoreURL string
	DeleteURL            string
	Author               string
	AuthorFilterURL      string
	Categories, Tags     template.HTML
	Comments, Pending    int
	DateLabel, DateValue string
	Classes              string
	CanEdit, CanDelete   bool
	Status               string
	IsTrash              bool
	ViewLabel            string
	BuilderURL           string // "Edit with Golementor"
	Excerpt              string // shown in the Extended view
	Inline               inlineData
}

// inlineData is get_inline_data(): the hidden values inline-edit-post.js copies into the Quick Edit row.
type inlineData struct {
	Slug, AuthorID, CommentStatus, PingStatus, Status string
	JJ, MM, AA, HH, MN, SS                            string
	Password, Parent, Template, MenuOrder             string
	Categories, Tags, Sticky, Format                  string
}

type postsListData struct {
	Labels       typeLabels
	Statuses     []statusLink
	Rows         []postRow
	Search       string
	Status       string
	Months       []monthOption
	Month        string
	Categories   []*store.Term
	Cat          int64
	Pagination   pagination
	BulkNonce    string
	CanCreate    bool
	IsTrash      bool
	Orderby      string
	Order        string
	SortLinks    map[string]sortLink
	ShowCatCols  bool
	EditorSwitch bool
	Hidden       map[string]bool // columns hidden through Screen Options
	Mode         string          // list or excerpt (Compact / Extended view)
	Formats      []formatOption  // the post-format filter, only formats in use
	Format       string
	Sticky       bool // the "Sticky" view
	ColCount     int  // visible columns, for the inline editor's colspan
	// Quick Edit / Bulk Edit (inline_edit()).
	InlineNonce string
	Authors     []*store.User
	Pages       []*store.Post
	Templates   []templateOption
	CanPublish  bool
	CanOthers   bool
}

type formatOption struct {
	Slug, Label string
}

type templateOption struct {
	Value, Label string
}

// postFormatLabels is get_post_format_strings().
var postFormatLabels = map[string]string{"standard": "Standard", "aside": "Aside", "chat": "Chat", "gallery": "Gallery", "link": "Link",
	"image": "Image", "quote": "Quote", "status": "Status", "video": "Video", "audio": "Audio"}

// postListColumns are the Screen Options column toggles of edit.php (every column but cb and the primary one).
func postListColumns(typ string) []screenColumn {
	cols := []screenColumn{{"author", "Author"}}
	if typ == "post" {
		cols = append(cols, screenColumn{"categories", "Categories"}, screenColumn{"tags", "Tags"})
	}
	return append(cols, screenColumn{"comments", "Comments"}, screenColumn{"date", "Date"})
}

type monthOption struct {
	Value, Label string
}

type sortLink struct {
	URL, Class, AriaSort, SRText string
}

type pagination struct {
	Total, Pages, Page      int
	First, Prev, Next, Last string
	ItemsLabel              string
}

func newPagination(total, perPage, page int, base url.URL) pagination {
	pages := int(math.Ceil(float64(total) / float64(perPage)))
	if pages < 1 {
		pages = 1
	}
	p := pagination{Total: total, Pages: pages, Page: page}
	if total == 1 {
		p.ItemsLabel = "1 item"
	} else {
		p.ItemsLabel = formatNumber(total) + " items"
	}
	link := func(n int) string {
		q := base.Query()
		if n <= 1 {
			q.Del("paged")
		} else {
			q.Set("paged", strconv.Itoa(n))
		}
		base.RawQuery = q.Encode()
		return base.String()
	}
	if page > 2 {
		p.First = link(1)
	}
	if page > 1 {
		p.Prev = link(page - 1)
	}
	if page < pages {
		p.Next = link(page + 1)
	}
	if page < pages-1 {
		p.Last = link(pages)
	}
	return p
}

// postsList is edit.php: the posts or pages list table.
func (s *Server) postsList(c *gin.Context) {
	ctx := c.Request.Context()
	u := currentUser(c)
	if c.Query("action") == "elementor_new_post" && s.golementorOn() { // Golementor: create a draft and open the builder
		s.gweNewPost(c)
		return
	}
	typ := c.DefaultQuery("post_type", "post")
	if typ != "post" && typ != "page" {
		c.String(http.StatusBadRequest, "Invalid post type.")
		return
	}
	if !u.Can("edit_"+typ+"s", nil) {
		c.String(http.StatusForbidden, "Sorry, you are not allowed to edit posts in this post type.")
		return
	}
	if a := bulkAction(c); a != "" {
		s.postsBulkAction(c, typ, a)
		return
	}
	lab := labelsFor(typ)
	status := c.DefaultQuery("post_status", "all")
	search := strings.TrimSpace(c.Query("s"))
	page, _ := strconv.Atoi(c.Query("paged"))
	if page < 1 {
		page = 1
	}
	perPage := s.userOptionInt(u, "edit_"+typ+"_per_page", 20)
	mode := s.listMode(c)
	showSticky := typ == "post" && c.Query("show_sticky") != "" && c.Query("show_sticky") != "0"

	q := store.PostQuery{Types: []string{typ}, Search: search, Page: page, PerPage: perPage,
		OrderBy: c.Query("orderby"), Order: c.DefaultQuery("order", "desc"), Loc: s.loc()}
	if typ == "page" && c.Query("orderby") == "" {
		q.OrderBy, q.Order = "title", c.DefaultQuery("order", "asc")
	}
	switch status {
	case "all", "":
		q.Statuses = []string{"publish", "future", "draft", "pending", "private"}
	default:
		q.Statuses = []string{status}
	}
	if !u.Can("read_private_"+typ+"s", nil) && status == "all" {
		q.Statuses = []string{"publish", "future", "draft", "pending"}
	}
	if showSticky {
		yes := true
		q.Sticky = &yes
	}
	if f := c.Query("post_format"); typ == "post" && f != "" && postFormatLabels[f] != "" {
		q.Format = f
	}
	if a, _ := strconv.ParseInt(c.Query("author"), 10, 64); a > 0 {
		q.AuthorID = a
	}
	if m := c.Query("m"); len(m) == 6 {
		q.Year, _ = strconv.Atoi(m[:4])
		q.Month, _ = strconv.Atoi(m[4:])
	}
	var cat int64
	if v, _ := strconv.ParseInt(c.Query("cat"), 10, 64); v > 0 {
		cat = v
		q.TermIDs = []int64{v}
	}
	for param, tax := range map[string]string{"category_name": "category", "tag": "post_tag"} {
		if slug := c.Query(param); slug != "" {
			if t, err := s.store.TermBySlug(ctx, tax, slug); err == nil {
				q.TermIDs = []int64{t.ID}
			} else {
				q.TermIDs = []int64{-1}
			}
		}
	}
	posts, total, err := s.store.QueryPosts(ctx, q)
	if err != nil {
		c.String(http.StatusInternalServerError, err.Error())
		return
	}

	// Pages list as a tree when browsing all pages without search or custom order.
	if typ == "page" && search == "" && c.Query("orderby") == "" && (status == "all" || status == "") {
		posts, total, _ = s.pageTree(c, q, page, perPage)
	}

	d := postsListData{Labels: lab, Search: search, Status: status, BulkNonce: s.nonce(c, "bulk-posts"),
		CanCreate: u.Can("edit_"+typ+"s", nil), IsTrash: status == "trash", Month: c.Query("m"), Cat: cat,
		Orderby: q.OrderBy, Order: strings.ToLower(q.Order), ShowCatCols: typ == "post", EditorSwitch: s.settings.String("classic-editor-allow-users") == "allow",
		Hidden: s.hiddenColumns(u, "edit-"+typ), Mode: mode, Format: q.Format, Sticky: showSticky}

	counts, _ := s.store.StatusCounts(ctx, typ, 0)
	all := counts["publish"] + counts["future"] + counts["draft"] + counts["pending"] + counts["private"]
	base := "/wp-admin/edit"
	if typ == "page" {
		base += "?post_type=page"
	}
	withStatus := func(st string) string {
		if typ == "page" {
			return base + "&post_status=" + st
		}
		return base + "?post_status=" + st
	}
	d.Statuses = append(d.Statuses, statusLink{"all", "All", base, all, (status == "all" || status == "") && !showSticky})
	mine, _ := s.store.StatusCounts(ctx, typ, u.ID)
	mineCount := mine["publish"] + mine["future"] + mine["draft"] + mine["pending"] + mine["private"]
	if mineCount > 0 && mineCount != all {
		sep := "?"
		if strings.Contains(base, "?") {
			sep = "&"
		}
		d.Statuses = append(d.Statuses, statusLink{"mine", "Mine", base + sep + "author=" + itoa64(u.ID), mineCount, q.AuthorID == u.ID})
	}
	for _, st := range []struct{ key, label string }{
		{"publish", "Published"}, {"future", "Scheduled"}, {"draft", "Drafts"}, {"pending", "Pending"},
		{"private", "Private"}, {"trash", "Trash"},
	} {
		if counts[st.key] > 0 {
			d.Statuses = append(d.Statuses, statusLink{st.key, st.label, withStatus(st.key), counts[st.key], status == st.key})
		}
	}
	// The Sticky view comes after Published, or after All when nothing is published (WP_Posts_List_Table::get_views()).
	if typ == "post" {
		if n, _ := s.store.StickyCount(ctx, []string{"publish", "future", "draft", "pending", "private"}); n > 0 {
			sticky := statusLink{"sticky", "Sticky", base + "?show_sticky=1", n, showSticky}
			at := 1
			for i, l := range d.Statuses {
				if l.Key == "publish" {
					at = i + 1
				}
			}
			d.Statuses = append(d.Statuses[:at], append([]statusLink{sticky}, d.Statuses[at:]...)...)
		}
		used, _ := s.store.UsedFormats(ctx)
		for _, f := range used {
			if label := postFormatLabels[f]; label != "" {
				d.Formats = append(d.Formats, formatOption{f, label})
			}
		}
	}

	s.postRows(c, &d, posts, base)

	if typ == "post" {
		months, _ := s.store.MonthlyArchives(ctx, s.loc())
		for _, m := range months {
			d.Months = append(d.Months, monthOption{fmt.Sprintf("%d%02d", m.Year, m.Month), time.Month(m.Month).String() + " " + strconv.Itoa(m.Year)})
		}
	}
	s.inlineEditData(c, &d)
	cur := *c.Request.URL
	d.Pagination = newPagination(total, perPage, page, cur)
	d.SortLinks = map[string]sortLink{}
	for _, col := range []string{"title", "author", "comment_count", "date"} {
		d.SortLinks[col] = s.sortLinkFor(c, col, q.OrderBy, strings.ToLower(q.Order))
	}

	screen := "edit"
	if typ == "page" {
		screen = "edit?post_type=page"
	}
	s.setScreenOptions(c, screenOptions{Screen: "edit-" + typ, Columns: postListColumns(typ), Hidden: d.Hidden,
		PerPageOption: "edit_" + typ + "_per_page", PerPage: perPage, ViewMode: mode})
	s.pageAssets(c).Script("inline-edit-post")
	s.adminPage(c, screen, lab.Name, "edit-php post-type-"+typ, "admin/posts-list", d, postNotices(c, lab)...)
}

// inlineEditData fills what the Quick Edit and Bulk Edit rows need: categories, authors, parent
// pages, page templates and the nonce.
func (s *Server) inlineEditData(c *gin.Context, d *postsListData) {
	ctx := c.Request.Context()
	u := currentUser(c)
	typ := d.Labels.Type
	d.InlineNonce = s.nonce(c, "inlineeditnonce")
	d.CanPublish = u.Can("publish_posts", nil)
	d.CanOthers = u.Can("edit_others_"+typ+"s", nil)
	if typ == "post" {
		d.Categories, _ = s.store.HierarchicalTerms(ctx, "category", false)
	} else {
		d.Pages, _, _ = s.store.QueryPosts(ctx, store.PostQuery{Types: []string{"page"}, Statuses: []string{"publish", "private", "draft", "pending", "future"},
			OrderBy: "menu_order", Order: "asc", PerPage: -1})
		d.Templates = s.pageTemplates()
	}
	if d.CanOthers {
		d.Authors, _ = s.store.Authors(ctx)
	}
	d.ColCount = 2 // cb + title
	for _, col := range postListColumns(typ) {
		if !d.Hidden[col.ID] {
			d.ColCount++
		}
	}
}

// postRows builds the list rows (WP_Posts_List_Table::single_row()) for posts of d's type.
func (s *Server) postRows(c *gin.Context, d *postsListData, posts []*store.Post, base string) {
	ctx := c.Request.Context()
	u := currentUser(c)
	typ := d.Labels.Type
	lb := s.links(ctx)
	ids := make([]int64, 0, len(posts))
	authorIDs := []int64{}
	for _, p := range posts {
		ids = append(ids, p.ID)
		authorIDs = append(authorIDs, p.AuthorID)
	}
	authors, _ := s.store.UserNames(ctx, authorIDs)
	cats, _ := s.store.TermsForPosts(ctx, ids, "category")
	tags, _ := s.store.TermsForPosts(ctx, ids, "post_tag")
	loc := s.loc()
	frontID := int64(s.settings.Int("page_on_front"))
	postsPageID := int64(s.settings.Int("page_for_posts"))
	privacyID := int64(s.settings.Int("wp_page_for_privacy_policy"))
	builderOn := s.golementorOn()

	for _, p := range posts {
		r := postRow{ID: p.ID, Title: p.Title, Status: p.Status, IsTrash: p.Status == "trash",
			EditURL: links.EditLink(p.ID), Comments: p.CommentCount,
			CanEdit: u.Can("edit_post", p), CanDelete: u.Can("delete_post", p)}
		if strings.TrimSpace(r.Title) == "" {
			r.Title = "(no title)"
		}
		if depth, ok := p.Meta["_depth"].(int); ok && depth > 0 {
			r.Depth = strings.Repeat("&#8212; ", depth)
		}
		if d.Mode == "excerpt" && typ == "post" {
			r.Excerpt = wp.TrimWords(template.HTMLEscapeString(wp.StripTags(defaultStr(p.Excerpt, p.Content))), 55, "&hellip;")
		}
		r.Inline = s.inlineDataFor(p, cats[p.ID], tags[p.ID])
		switch p.Status {
		case "draft":
			r.States = append(r.States, "Draft")
		case "pending":
			r.States = append(r.States, "Pending")
		case "future":
			r.States = append(r.States, "Scheduled")
		case "private":
			r.States = append(r.States, "Private")
		}
		if p.Password != "" {
			r.States = append(r.States, "Password protected")
		}
		if p.Sticky && typ == "post" {
			r.States = append(r.States, "Sticky")
		}
		if typ == "page" {
			switch p.ID {
			case frontID:
				r.States = append(r.States, "Front Page")
			case postsPageID:
				r.States = append(r.States, "Posts Page")
			case privacyID:
				r.States = append(r.States, "Privacy Policy Page")
			}
		}
		nonce := s.nonce(c, "trash-post_"+itoa64(p.ID))
		r.TrashURL = "/wp-admin/post?post=" + itoa64(p.ID) + "&action=trash&_wpnonce=" + nonce
		r.RestoreURL = "/wp-admin/post?post=" + itoa64(p.ID) + "&action=untrash&_wpnonce=" + s.nonce(c, "untrash-post_"+itoa64(p.ID))
		r.DeleteURL = "/wp-admin/post?post=" + itoa64(p.ID) + "&action=delete&_wpnonce=" + s.nonce(c, "delete-post_"+itoa64(p.ID))
		r.BuilderURL = s.gweRowAction(c, p)
		if builderOn && gweBuilt(p) {
			r.States = append(r.States, "Golementor")
		}
		if p.Status == "publish" || p.Status == "private" {
			r.ViewURL, r.ViewLabel = lb.Permalink(p), "View"
		} else if p.Status != "trash" {
			r.ViewURL, r.ViewLabel = lb.Permalink(p)+previewSep(lb.Permalink(p))+"preview=true", "Preview"
		}
		if a := authors[p.AuthorID]; a != nil {
			r.Author = a.DisplayName
			r.AuthorFilterURL = base + map[bool]string{true: "&", false: "?"}[strings.Contains(base, "?")] + "author=" + itoa64(a.ID)
		}
		r.Categories = termLinksHTML(cats[p.ID], "/wp-admin/edit?category_name=")
		r.Tags = termLinksHTML(tags[p.ID], "/wp-admin/edit?tag=")

		t := p.Date().In(loc)
		stamp := wp.FormatDate(t, "Y/m/d") + " at " + wp.FormatDate(t, "g:i a")
		switch p.Status {
		case "publish", "private":
			r.DateLabel = "Published"
		case "future":
			r.DateLabel = "Scheduled"
		default:
			r.DateLabel = "Last Modified"
			stamp = wp.FormatDate(p.ModifiedAt.In(loc), "Y/m/d") + " at " + wp.FormatDate(p.ModifiedAt.In(loc), "g:i a")
		}
		r.DateValue = stamp
		classes := []string{"iedit", "level-0", "post-" + itoa64(p.ID), "type-" + p.Type, "status-" + p.Status}
		if p.AuthorID == u.ID {
			classes = append(classes, "author-self")
		} else {
			classes = append(classes, "author-other")
		}
		if typ == "post" {
			classes = append(classes, "format-"+p.Format)
		}
		classes = append(classes, "hentry")
		for _, ct := range cats[p.ID] {
			classes = append(classes, "category-"+ct.Slug)
		}
		r.Classes = strings.Join(classes, " ")
		d.Rows = append(d.Rows, r)
	}
}

// inlineDataFor fills the hidden Quick Edit values of a row.
func (s *Server) inlineDataFor(p *store.Post, cats, tags []*store.Term) inlineData {
	t := p.Date().In(s.loc())
	in := inlineData{Slug: p.Slug, AuthorID: itoa64(p.AuthorID), CommentStatus: p.CommentStatus, PingStatus: p.PingStatus, Status: p.Status,
		JJ: t.Format("02"), MM: t.Format("01"), AA: t.Format("2006"), HH: t.Format("15"), MN: t.Format("04"), SS: t.Format("05"),
		Password: p.Password, Parent: itoa64(p.ParentID), MenuOrder: strconv.Itoa(p.MenuOrder), Template: "default"}
	if tpl, _ := p.Meta["_wp_page_template"].(string); tpl != "" {
		in.Template = tpl
	}
	var ids, names []string
	for _, ct := range cats {
		ids = append(ids, itoa64(ct.ID))
	}
	for _, tg := range tags {
		names = append(names, tg.Name)
	}
	in.Categories, in.Tags = strings.Join(ids, ","), strings.Join(names, ", ")
	if p.Sticky {
		in.Sticky = "sticky"
	}
	if p.Format != "" && p.Format != "standard" {
		in.Format = p.Format
	}
	return in
}

// pageTemplates lists the active theme's page templates (get_page_templates()); Golementor adds
// its canvas and full-width templates like Elementor's theme_page_templates filter.
func (s *Server) pageTemplates() []templateOption {
	var out []templateOption
	if t := s.activeTheme(); t != nil {
		keys := make([]string, 0, len(t.Templates))
		for k := range t.Templates {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			out = append(out, templateOption{k, t.Templates[k]})
		}
	}
	if s.golementorOn() {
		out = append(out, templateOption{"elementor_canvas", "Golementor Canvas"}, templateOption{"elementor_header_footer", "Golementor Full Width"})
	}
	return out
}

func previewSep(u string) string {
	if strings.Contains(u, "?") {
		return "&"
	}
	return "?"
}

// sortLinkFor builds a sortable column header link like WP_List_Table::print_column_headers().
func (s *Server) sortLinkFor(c *gin.Context, col, orderby, order string) sortLink {
	if orderby == "" {
		orderby = "date"
	}
	u := *c.Request.URL
	q := u.Query()
	q.Del("paged")
	q.Set("orderby", col)
	cur := orderby == col
	next := "asc"
	if cur && order == "asc" {
		next = "desc"
	}
	if !cur && (col == "date" || col == "comment_count") {
		next = "desc"
	}
	q.Set("order", next)
	u.RawQuery = q.Encode()
	sl := sortLink{URL: u.String()}
	if cur {
		sl.Class = "sorted " + order
		if order == "asc" {
			sl.AriaSort, sl.SRText = "ascending", "Sort descending."
		} else {
			sl.AriaSort, sl.SRText = "descending", "Sort ascending."
		}
	} else {
		if next == "asc" {
			sl.Class, sl.SRText = "sortable desc", "Sort ascending."
		} else {
			sl.Class, sl.SRText = "sortable asc", "Sort descending."
		}
	}
	return sl
}

// pageTree orders pages parent-first with their depth recorded in Meta["_depth"].
func (s *Server) pageTree(c *gin.Context, q store.PostQuery, page, perPage int) ([]*store.Post, int, error) {
	q.PerPage, q.Page = -1, 1
	q.OrderBy, q.Order = "menu_order", "asc"
	all, _, err := s.store.QueryPosts(c, q)
	if err != nil {
		return nil, 0, err
	}
	children := map[int64][]*store.Post{}
	ids := map[int64]bool{}
	for _, p := range all {
		ids[p.ID] = true
	}
	for _, p := range all {
		parent := p.ParentID
		if !ids[parent] {
			parent = 0
		}
		children[parent] = append(children[parent], p)
	}
	for k := range children {
		list := children[k]
		// menu_order, then title, like WordPress' page list.
		for i := 1; i < len(list); i++ {
			for j := i; j > 0 && (list[j].MenuOrder < list[j-1].MenuOrder ||
				(list[j].MenuOrder == list[j-1].MenuOrder && strings.ToLower(list[j].Title) < strings.ToLower(list[j-1].Title))); j-- {
				list[j], list[j-1] = list[j-1], list[j]
			}
		}
	}
	var ordered []*store.Post
	var walk func(parent int64, depth int)
	walk = func(parent int64, depth int) {
		for _, p := range children[parent] {
			p.Meta["_depth"] = depth
			ordered = append(ordered, p)
			walk(p.ID, depth+1)
		}
	}
	walk(0, 0)
	total := len(ordered)
	start := (page - 1) * perPage
	if start > total {
		start = total
	}
	end := start + perPage
	if end > total {
		end = total
	}
	return ordered[start:end], total, nil
}

func termLinksHTML(terms []*store.Term, base string) template.HTML {
	if len(terms) == 0 {
		return ""
	}
	var parts []string
	for _, t := range terms {
		parts = append(parts, `<a href="`+template.HTMLEscapeString(base+url.QueryEscape(t.Slug))+`">`+template.HTMLEscapeString(t.Name)+`</a>`)
	}
	return template.HTML(strings.Join(parts, ", "))
}

// bulkAction is WP_List_Table::current_action(): the chosen bulk action when Apply (bulk_action)
// or Bulk Edit's Update (bulk_edit) submitted the form.
func bulkAction(c *gin.Context) string {
	if c.Query("bulk_action") == "" && c.Query("bulk_edit") == "" {
		return ""
	}
	for _, k := range []string{"action", "action2"} {
		if v := c.Query(k); v != "" && v != "-1" {
			return v
		}
	}
	return ""
}

// postsBulk answers POSTs to edit.php: the Screen Options form (set_screen_options()).
func (s *Server) postsBulk(c *gin.Context) {
	if s.setScreenOption(c) {
		return
	}
	c.Redirect(http.StatusSeeOther, "/wp-admin/edit")
}

// postsBulkAction applies a bulk action from the list table (trash, untrash, delete, edit).
func (s *Server) postsBulkAction(c *gin.Context, typ, action string) {
	if !s.checkNonce(c, "bulk-posts") {
		s.denyNonce(c)
		return
	}
	if action == "edit" {
		s.bulkEditPosts(c, typ)
		return
	}
	u := currentUser(c)
	n := 0
	var done []string
	for _, raw := range c.QueryArray("post[]") {
		id, _ := strconv.ParseInt(raw, 10, 64)
		p, err := s.store.GetPost(c, id)
		if err != nil || p.Type != typ || !u.Can("delete_post", p) {
			continue
		}
		s.afterPostSave(c, p)
		switch action {
		case "trash":
			err = s.store.TrashPost(c, id)
		case "untrash":
			err = s.store.UntrashPost(c, id)
		case "delete":
			err = s.store.DeletePost(c, id)
		default:
			continue
		}
		if err == nil {
			n++
			done = append(done, raw)
		}
	}
	key := map[string]string{"trash": "trashed", "untrash": "untrashed", "delete": "deleted"}[action]
	target := "/wp-admin/edit?post_type=" + typ
	if key != "" {
		target += "&" + key + "=" + strconv.Itoa(n) + "&ids=" + strings.Join(done, ",")
	}
	c.Redirect(http.StatusFound, target)
}

func postNotices(c *gin.Context, lab typeLabels) []notice {
	var out []notice
	lower := strings.ToLower(lab.Singular)
	lowers := strings.ToLower(lab.Name)
	one := func(n int, single, multi string) string {
		if n == 1 {
			return "1 " + lower + " " + single
		}
		return strconv.Itoa(n) + " " + lowers + " " + multi
	}
	if n, _ := strconv.Atoi(c.Query("trashed")); n > 0 {
		msg := one(n, "moved to the Trash.", "moved to the Trash.")
		if ids := c.Query("ids"); ids != "" {
			msg += ` <a href="/wp-admin/edit?post_type=` + lab.Type + `&amp;doaction=undo&amp;action=untrash&amp;ids=` + template.HTMLEscapeString(ids) + `">Undo</a>`
		}
		out = append(out, notice{Type: "success", Message: template.HTML(msg), Dismissible: true})
	}
	if n, _ := strconv.Atoi(c.Query("untrashed")); n > 0 {
		out = append(out, notice{Type: "success", Message: template.HTML(one(n, "restored from the Trash.", "restored from the Trash.")), Dismissible: true})
	}
	if n, _ := strconv.Atoi(c.Query("deleted")); n > 0 {
		out = append(out, notice{Type: "success", Message: template.HTML(one(n, "permanently deleted.", "permanently deleted.")), Dismissible: true})
	}
	if n, _ := strconv.Atoi(c.Query("updated")); n > 0 {
		out = append(out, notice{Type: "success", Message: template.HTML(one(n, "updated.", "updated.")), Dismissible: true})
	}
	if n, _ := strconv.Atoi(c.Query("skipped")); n > 0 {
		out = append(out, notice{Type: "error", Message: template.HTML(one(n, "not updated, invalid post status.", "not updated, invalid post status.")), Dismissible: true})
	}
	return out
}

// bulkEditPosts is bulk_edit_posts(): the Bulk Edit row of edit.php applies the fields that were
// changed to every selected post and redirects with the number of posts updated.
func (s *Server) bulkEditPosts(c *gin.Context, typ string) {
	ctx := c.Request.Context()
	u := currentUser(c)
	q := c.Request.URL.Query()
	has := func(k string) bool { v, ok := q[k]; return ok && v[0] != "" && v[0] != "-1" }
	var updated, skipped int
	newCats := []int64{}
	for _, raw := range q["post_category[]"] {
		if id, _ := strconv.ParseInt(raw, 10, 64); id > 0 {
			newCats = append(newCats, id)
		}
	}
	indeterminate := map[int64]bool{}
	for _, raw := range q["indeterminate_post_category[]"] {
		id, _ := strconv.ParseInt(raw, 10, 64)
		indeterminate[id] = true
	}
	newTags, _ := s.store.TermIDsByNames(ctx, "post_tag", strings.Split(c.Query("tax_input[post_tag]"), ","))
	for _, raw := range q["post[]"] {
		id, _ := strconv.ParseInt(raw, 10, 64)
		p, err := s.store.GetPost(ctx, id)
		if err != nil || p.Type != typ || !u.Can("edit_post", p) {
			skipped++
			continue
		}
		if has("post_author") && u.Can("edit_others_"+typ+"s", nil) {
			p.AuthorID, _ = strconv.ParseInt(c.Query("post_author"), 10, 64)
		}
		if has("comment_status") {
			p.CommentStatus = map[bool]string{true: "open", false: "closed"}[c.Query("comment_status") == "open"]
		}
		if has("ping_status") {
			p.PingStatus = map[bool]string{true: "open", false: "closed"}[c.Query("ping_status") == "open"]
		}
		if has("_status") {
			st := c.Query("_status")
			if (st == "publish" || st == "future" || st == "private") && !u.Can("publish_post", p) {
				st = "pending"
			}
			if st == "publish" || st == "future" || st == "private" || st == "pending" || st == "draft" {
				p.Status = st
			}
		}
		if typ == "page" {
			if has("post_parent") {
				p.ParentID, _ = strconv.ParseInt(c.Query("post_parent"), 10, 64)
			}
			if has("page_template") {
				setPageTemplate(p, c.Query("page_template"))
			}
		}
		if typ == "post" {
			if has("sticky") && u.Can("edit_others_posts", nil) {
				p.Sticky = c.Query("sticky") == "sticky"
			}
			if has("post_format") {
				p.Format = c.Query("post_format")
				if p.Format == "0" || postFormatLabels[p.Format] == "" {
					p.Format = "standard"
				}
			}
		}
		if err := s.store.UpdatePost(ctx, p, u.ID); err != nil {
			skipped++
			continue
		}
		if typ == "post" {
			// Categories: checked ones are added, indeterminate ones keep the post's own state.
			cats, _ := s.store.TermsForPosts(ctx, []int64{p.ID}, "category")
			if len(newCats) > 0 || len(indeterminate) > 0 {
				keep := map[int64]bool{}
				for _, ct := range cats[p.ID] {
					if indeterminate[ct.ID] {
						keep[ct.ID] = true
					}
				}
				for _, id := range newCats {
					if !indeterminate[id] {
						keep[id] = true
					}
				}
				ids := make([]int64, 0, len(keep))
				for id := range keep {
					ids = append(ids, id)
				}
				sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
				if len(ids) == 0 {
					ids = []int64{int64(s.settings.Int("default_category"))}
				}
				_ = s.store.SetPostTerms(ctx, p.ID, "category", ids)
			}
			if len(newTags) > 0 {
				tags, _ := s.store.TermsForPosts(ctx, []int64{p.ID}, "post_tag")
				ids := []int64{}
				for _, tg := range tags[p.ID] {
					ids = append(ids, tg.ID)
				}
				for _, id := range newTags {
					if !slices.Contains(ids, id) {
						ids = append(ids, id)
					}
				}
				_ = s.store.SetPostTerms(ctx, p.ID, "post_tag", ids)
			}
		}
		s.afterPostSave(ctx, p)
		updated++
	}
	target := "/wp-admin/edit?post_type=" + typ + "&updated=" + strconv.Itoa(updated)
	if skipped > 0 {
		target += "&skipped=" + strconv.Itoa(skipped)
	}
	c.Redirect(http.StatusFound, target)
}

// setPageTemplate stores the Page Attributes template in _wp_page_template; "default" removes it.
func setPageTemplate(p *store.Post, tpl string) {
	if tpl == "default" || tpl == "" {
		delete(p.Meta, "_wp_page_template")
		return
	}
	p.Meta["_wp_page_template"] = tpl
}

// inlineSave is wp_ajax_inline_save(): Quick Edit posts the row's fields and gets the re-rendered row back.
func (s *Server) inlineSave(c *gin.Context) {
	ctx := c.Request.Context()
	u := currentUser(c)
	if !s.checkNonceValue(c, c.PostForm("_inline_edit"), "inlineeditnonce") {
		c.String(http.StatusForbidden, "-1")
		return
	}
	id, _ := strconv.ParseInt(c.PostForm("post_ID"), 10, 64)
	p, err := s.store.GetPost(ctx, id)
	if err != nil || id == 0 {
		c.String(http.StatusOK, "")
		return
	}
	typ := p.Type
	if !u.Can("edit_post", p) {
		c.String(http.StatusOK, "Sorry, you are not allowed to edit this "+typ+".")
		return
	}
	wasFloating := p.PublishedAt == nil
	p.Title = strings.TrimSpace(c.PostForm("post_title"))
	p.Slug = strings.TrimSpace(c.PostForm("post_name"))
	if a, _ := strconv.ParseInt(c.PostForm("post_author"), 10, 64); a > 0 && u.Can("edit_others_"+typ+"s", nil) {
		p.AuthorID = a
	}
	p.CommentStatus = map[bool]string{true: "open", false: "closed"}[c.PostForm("comment_status") == "open"]
	p.PingStatus = map[bool]string{true: "open", false: "closed"}[c.PostForm("ping_status") == "open"]
	if c.PostForm("aa") != "" {
		num := func(k string) int { n, _ := strconv.Atoi(c.PostForm(k)); return n }
		when := time.Date(num("aa"), time.Month(num("mm")), num("jj"), num("hh"), num("mn"), num("ss"), 0, s.loc())
		// A draft keeps its floating date unless the date was actually edited (post_date_gmt stays 0000 in WordPress).
		if !wasFloating || !when.Equal(p.Date().In(s.loc()).Truncate(time.Second)) {
			p.PublishedAt = &when
		}
	}
	status := c.PostForm("_status")
	if c.PostForm("keep_private") == "private" {
		status, p.Password = "private", ""
	} else {
		p.Password = c.PostForm("post_password")
	}
	if (status == "publish" || status == "future" || status == "private") && !u.Can("publish_post", p) {
		status = "pending"
	}
	switch status {
	case "publish", "future", "private", "pending", "draft":
		p.Status = status
	}
	if p.Status == "auto-draft" {
		p.Status = "draft"
	}
	if typ == "page" {
		p.ParentID, _ = strconv.ParseInt(c.PostForm("post_parent"), 10, 64)
		p.MenuOrder, _ = strconv.Atoi(c.PostForm("menu_order"))
		if tpl, ok := c.GetPostForm("page_template"); ok {
			setPageTemplate(p, tpl)
		}
	} else {
		if u.Can("edit_others_posts", nil) && u.Can("publish_posts", nil) {
			p.Sticky = c.PostForm("sticky") == "sticky" && p.Password == "" && p.Status != "private"
		}
		if f, ok := c.GetPostForm("post_format"); ok {
			p.Format = f
			if f == "0" || f == "" || postFormatLabels[f] == "" {
				p.Format = "standard"
			}
		}
	}
	if err := s.store.UpdatePost(ctx, p, u.ID); err != nil {
		c.String(http.StatusOK, err.Error())
		return
	}
	if typ == "post" {
		var catIDs []int64
		for _, raw := range c.PostFormArray("post_category[]") {
			if cid, err := strconv.ParseInt(raw, 10, 64); err == nil && cid > 0 {
				catIDs = append(catIDs, cid)
			}
		}
		if len(catIDs) == 0 {
			catIDs = []int64{int64(s.settings.Int("default_category"))}
		}
		_ = s.store.SetPostTerms(ctx, p.ID, "category", catIDs)
		tagIDs, _ := s.store.TermIDsByNames(ctx, "post_tag", strings.Split(c.PostForm("tax_input[post_tag]"), ","))
		_ = s.store.SetPostTerms(ctx, p.ID, "post_tag", tagIDs)
	}
	s.afterPostSave(ctx, p)
	p, _ = s.store.GetPost(ctx, p.ID)
	// Pages show their depth in the tree like the list does.
	depth := 0
	for parent := p.ParentID; parent > 0 && depth < 50; depth++ {
		pp, err := s.store.GetPost(ctx, parent)
		if err != nil {
			break
		}
		parent = pp.ParentID
	}
	p.Meta["_depth"] = depth
	c.Data(http.StatusOK, "text/html; charset=UTF-8", []byte(s.postRowHTML(c, p, c.PostForm("post_view"))))
}

// postRowHTML renders one list row (WP_Posts_List_Table::display_rows() for a single post).
func (s *Server) postRowHTML(c *gin.Context, p *store.Post, mode string) string {
	u := currentUser(c)
	base := "/wp-admin/edit"
	if p.Type == "page" {
		base += "?post_type=page"
	}
	d := postsListData{Labels: labelsFor(p.Type), ShowCatCols: p.Type == "post", EditorSwitch: s.settings.String("classic-editor-allow-users") == "allow",
		Hidden: s.hiddenColumns(u, "edit-"+p.Type), Mode: defaultStr(mode, "list")}
	s.postRows(c, &d, []*store.Post{p}, base)
	var buf bytes.Buffer
	if err := s.tmpl.get(false).ExecuteTemplate(&buf, "posts-row", map[string]any{"R": d.Rows[0], "D": d}); err != nil {
		return err.Error()
	}
	return buf.String()
}

// Screen Options --------------------------------------------------------------------

// screenColumn is one checkbox of the Screen Options "Columns" list.
type screenColumn struct {
	ID, Label string
}

// screenOptions describes the Screen Options tab of a list screen (WP_Screen::render_screen_options()).
type screenOptions struct {
	Screen        string // screen id: edit-post, edit-page, edit-category, edit-comments
	Columns       []screenColumn
	Hidden        map[string]bool
	PerPageOption string // user option, e.g. edit_post_per_page
	PerPage       int
	PerPageLabel  string         // "Number of items per page:" unless the screen says otherwise
	ViewMode      string         // list or excerpt; "" when the screen has no view mode
	Boxes         []screenColumn // post edit: the "Screen elements" (metabox) toggles
}

// screenOptionNames are the per-page options set_screen_options() accepts.
var screenOptionNames = map[string]bool{"edit_post_per_page": true, "edit_page_per_page": true, "edit_category_per_page": true,
	"edit_post_tag_per_page": true, "edit_comments_per_page": true}

// setScreenOptions queues the Screen Options tab for the page being rendered.
func (s *Server) setScreenOptions(c *gin.Context, so screenOptions) {
	var b strings.Builder
	b.WriteString("<div id=\"screen-options-wrap\" class=\"hidden\" tabindex=\"-1\" aria-label=\"Screen Options Tab\">\n<form id=\"adv-settings\" method=\"post\">\n")
	if len(so.Boxes) > 0 {
		b.WriteString("<fieldset class=\"metabox-prefs\"><legend>Screen elements</legend><p>Some screen elements can be shown or hidden by using the checkboxes. Expand or collapse the elements by clicking on their headings, and arrange them by dragging and dropping or by using the arrows in the headings.</p>\n")
		for _, box := range so.Boxes {
			checked := ` checked="checked"`
			if so.Hidden[box.ID] {
				checked = ""
			}
			fmt.Fprintf(&b, "<label for=\"%s-hide\"><input class=\"hide-postbox-tog\" name=\"%s-hide\" type=\"checkbox\" id=\"%s-hide\" value=\"%s\"%s />%s</label>\n",
				box.ID, box.ID, box.ID, box.ID, checked, template.HTMLEscapeString(box.Label))
		}
		b.WriteString("</fieldset>\n")
	}
	if len(so.Columns) > 0 {
		b.WriteString("<fieldset class=\"metabox-prefs\"><legend>Columns</legend>\n")
		for _, col := range so.Columns {
			checked := ` checked="checked"`
			if so.Hidden[col.ID] {
				checked = ""
			}
			fmt.Fprintf(&b, "<label><input class=\"hide-column-tog\" name=\"%s-hide\" type=\"checkbox\" id=\"%s-hide\" value=\"%s\"%s />%s</label>\n",
				col.ID, col.ID, col.ID, checked, template.HTMLEscapeString(col.Label))
		}
		b.WriteString("</fieldset>\n")
	}
	if so.PerPageOption != "" {
		fmt.Fprintf(&b, "<fieldset class=\"screen-options\"><legend>Pagination</legend>\n<label for=\"%s\">%s</label>\n"+
			"<input type=\"number\" step=\"1\" min=\"1\" max=\"999\" class=\"screen-per-page\" name=\"wp_screen_options[value]\" id=\"%s\" maxlength=\"3\" value=\"%d\" />\n"+
			"<input type=\"hidden\" name=\"wp_screen_options[option]\" value=\"%s\" />\n</fieldset>\n",
			so.PerPageOption, defaultStr(so.PerPageLabel, "Number of items per page:"), so.PerPageOption, so.PerPage, so.PerPageOption)
	}
	if so.ViewMode != "" {
		list, excerpt := ` checked="checked"`, ""
		if so.ViewMode == "excerpt" {
			list, excerpt = "", ` checked="checked"`
		}
		fmt.Fprintf(&b, "<fieldset class=\"metabox-prefs view-mode\"><legend>View mode</legend>\n"+
			"<label for=\"list-view-mode\"><input id=\"list-view-mode\" type=\"radio\" name=\"mode\" value=\"list\"%s />Compact view</label>\n"+
			"<label for=\"excerpt-view-mode\"><input id=\"excerpt-view-mode\" type=\"radio\" name=\"mode\" value=\"excerpt\"%s />Extended view</label>\n</fieldset>\n", list, excerpt)
	}
	if so.PerPageOption != "" || so.ViewMode != "" {
		b.WriteString("<p class=\"submit\"><input type=\"submit\" name=\"screen-options-apply\" id=\"screen-options-apply\" class=\"button button-primary\" value=\"Apply\" /></p>\n")
	}
	fmt.Fprintf(&b, "<input type=\"hidden\" id=\"screenoptionnonce\" name=\"screenoptionnonce\" value=\"%s\" />\n</form>\n</div>\n", s.nonce(c, "screen-options-nonce"))
	c.Set("wpgo.screenOptions", template.HTML(b.String()))
}

// setScreenOption is set_screen_options(): the Screen Options form of a list screen saves the
// per-page option (and the view mode) and sends the user back to the list.
func (s *Server) setScreenOption(c *gin.Context) bool {
	option := c.PostForm("wp_screen_options[option]")
	if option == "" && c.PostForm("mode") == "" {
		return false
	}
	if !s.checkNonceValue(c, c.PostForm("screenoptionnonce"), "screen-options-nonce") {
		s.denyNonce(c)
		return true
	}
	u := currentUser(c)
	if screenOptionNames[option] {
		if n, _ := strconv.Atoi(c.PostForm("wp_screen_options[value]")); n >= 1 && n <= 999 {
			_ = s.setUserMeta(c, u, option, n)
		}
	}
	if m := c.PostForm("mode"); m == "list" || m == "excerpt" {
		s.setUserSetting(c, "posts_list_mode", m)
	}
	back := c.Request.Referer()
	if ref, err := url.Parse(back); err == nil && strings.HasPrefix(ref.Path, "/wp-admin/") {
		q := ref.Query()
		q.Del("paged")
		ref.RawQuery = q.Encode()
		back = ref.RequestURI()
	} else {
		back = c.Request.URL.RequestURI()
	}
	c.Redirect(http.StatusFound, back)
	return true
}

// userOptionInt reads a numeric user option (get_user_option()) with a default.
func (s *Server) userOptionInt(u *store.User, key string, def int) int {
	switch v := u.Meta[key].(type) {
	case float64:
		if v >= 1 {
			return int(v)
		}
	case int:
		if v >= 1 {
			return v
		}
	}
	return def
}

// hiddenColumns is get_hidden_columns(): the manage{screen}columnshidden user option.
func (s *Server) hiddenColumns(u *store.User, screen string) map[string]bool {
	return userMetaSet(u, "manage"+screen+"columnshidden")
}

// userMetaSet reads a list-valued user option into a set.
func userMetaSet(u *store.User, key string) map[string]bool {
	out := map[string]bool{}
	list, _ := u.Meta[key].([]any)
	for _, v := range list {
		if sv, ok := v.(string); ok && sv != "" {
			out[sv] = true
		}
	}
	return out
}

// hiddenColumnsAjax is wp_ajax_hidden_columns(): common.js saves the Columns checkboxes.
func (s *Server) hiddenColumnsAjax(c *gin.Context) {
	if !s.checkNonceValue(c, c.PostForm("screenoptionnonce"), "screen-options-nonce") {
		c.String(http.StatusForbidden, "-1")
		return
	}
	page := c.PostForm("page")
	if page == "" || page != sanitizeKey(page) {
		c.String(http.StatusOK, "0")
		return
	}
	hidden := []string{}
	for _, h := range strings.Split(c.PostForm("hidden"), ",") {
		if h = strings.TrimSpace(h); h != "" {
			hidden = append(hidden, h)
		}
	}
	_ = s.setUserMeta(c, currentUser(c), "manage"+page+"columnshidden", hidden)
	c.String(http.StatusOK, "1")
}

// closedPostboxesAjax is wp_ajax_closed_postboxes(): postbox.js saves which metaboxes are closed
// and which are hidden through Screen Options.
func (s *Server) closedPostboxesAjax(c *gin.Context) {
	if !s.checkNonceValue(c, c.PostForm("closedpostboxesnonce"), "closedpostboxes") {
		c.String(http.StatusForbidden, "-1")
		return
	}
	page := c.PostForm("page")
	if page == "" || page != sanitizeKey(page) {
		c.String(http.StatusOK, "0")
		return
	}
	split := func(v string) []string {
		out := []string{}
		for _, h := range strings.Split(v, ",") {
			if h = strings.TrimSpace(h); h != "" {
				out = append(out, h)
			}
		}
		return out
	}
	u := currentUser(c)
	_ = s.setUserMeta(c, u, "closedpostboxes_"+page, split(c.PostForm("closed")))
	_ = s.setUserMeta(c, u, "metaboxhidden_"+page, split(c.PostForm("hidden")))
	c.String(http.StatusOK, "1")
}

// listMode is the posts_list_mode user setting (Compact / Extended view); ?mode= changes it like edit.php.
func (s *Server) listMode(c *gin.Context) string {
	if m := c.Query("mode"); m == "list" || m == "excerpt" {
		s.setUserSetting(c, "posts_list_mode", m)
		return m
	}
	if s.userSetting(c, "posts_list_mode") == "excerpt" {
		return "excerpt"
	}
	return "list"
}

// userSetting reads a key of the wp-settings-{uid} cookie (get_user_setting()).
func (s *Server) userSetting(c *gin.Context, key string) string {
	u := currentUser(c)
	if u == nil {
		return ""
	}
	ck, err := c.Cookie("wp-settings-" + itoa64(u.ID))
	if err != nil {
		return ""
	}
	for _, kv := range strings.Split(ck, "&") {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			return v
		}
	}
	return ""
}

// setUserSetting updates the wp-settings-{uid} cookie (set_user_setting()).
func (s *Server) setUserSetting(c *gin.Context, key, value string) {
	u := currentUser(c)
	if u == nil {
		return
	}
	var parts []string
	if ck, err := c.Cookie("wp-settings-" + itoa64(u.ID)); err == nil {
		for _, kv := range strings.Split(ck, "&") {
			if k, _, ok := strings.Cut(kv, "="); ok && k != key && k != "" {
				parts = append(parts, kv)
			}
		}
	}
	parts = append(parts, key+"="+value)
	secure := c.Request.TLS != nil
	c.SetCookie("wp-settings-"+itoa64(u.ID), strings.Join(parts, "&"), 365*24*3600, "/", "", secure, false)
	c.SetCookie("wp-settings-time-"+itoa64(u.ID), strconv.FormatInt(time.Now().Unix(), 10), 365*24*3600, "/", "", secure, false)
}

// Editing ----------------------------------------------------------------------

type editData struct {
	Post         *store.Post
	Labels       typeLabels
	IsNew        bool
	Nonce        string
	Categories   []*store.Term
	Checked      map[int64]bool
	TagNames     string
	Authors      []*store.User
	Pages        []*store.Post
	CanPublish   bool
	CanAuthor    bool
	Permalink    string
	ViewURL      string
	StatusLabel  string
	Visibility   string
	DateLabel    string
	DateInput    string
	TimeInput    string
	TrashURL     string
	Revisions    int
	Formats      []string
	PublishLabel string
	SlugBase     string
	SampleLink   template.HTML
	SampleNonce  string
	Editor       template.HTML
	Thumbnail    template.HTML
	BlockURL     string
	LastEdit     string
	TS           touchTime
	PluginStats  string

	MetaRows       []metaRow     // Custom Fields (post_custom_meta_box())
	MetaRowsHTML   template.HTML // the rows, _list_meta_row()
	MetaNonce      string        // add-meta
	Templates      []templateOption
	Template       string            // _wp_page_template, "default" when unset
	RevisionsList  template.HTML     // wp_list_post_revisions()
	LatestRevision int64             // the "Browse" link of the publish box
	BoxClass       map[string]string // extra postbox classes: " hide-if-js" (Screen Options) and " closed"
	ToPing         string            // Send Trackbacks (to_ping)
	ClosedNonce    string            // closedpostboxes
	OrderNonce     string            // meta-box-order
}

// metaRow is one Custom Fields entry (_list_meta_row()). WordPress Go keeps post meta as a JSON map,
// so the row id is a hash of the key rather than a meta_id.
type metaRow struct {
	ID          uint32
	Key, Value  string
	DeleteNonce string
}

// metaRowID is the stable numeric id of a custom field, used in the row and ajax markup.
func metaRowID(key string) uint32 {
	h := fnv.New32a()
	h.Write([]byte(key))
	return h.Sum32()&0x7fffffff + 1
}

// metaValueString renders a meta value the way the Custom Fields box edits it.
func metaValueString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// customFields lists the post's Custom Fields: every meta key that is not protected (underscore-prefixed).
func (s *Server) customFields(c *gin.Context, p *store.Post) []metaRow {
	keys := make([]string, 0, len(p.Meta))
	for k := range p.Meta {
		if k != "" && !strings.HasPrefix(k, "_") {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	rows := make([]metaRow, 0, len(keys))
	for _, k := range keys {
		id := metaRowID(k)
		rows = append(rows, metaRow{ID: id, Key: k, Value: metaValueString(p.Meta[k]), DeleteNonce: s.nonce(c, "delete-meta_"+strconv.FormatUint(uint64(id), 10))})
	}
	return rows
}

// metaKeyByID finds the custom field a row id stands for.
func metaKeyByID(p *store.Post, id uint32) (string, bool) {
	for k := range p.Meta {
		if !strings.HasPrefix(k, "_") && metaRowID(k) == id {
			return k, true
		}
	}
	return "", false
}

// defaultHiddenBoxes are the metaboxes WordPress hides on the editor until Screen Options shows them
// (get_hidden_meta_boxes()).
var defaultHiddenBoxes = []string{"slugdiv", "trackbacksdiv", "postcustom", "postexcerpt", "commentstatusdiv", "commentsdiv", "authordiv", "revisionsdiv"}

// editBoxes lists the editor's metaboxes for the Screen Options "Screen elements" toggles.
func editBoxes(typ string, revisions, author bool) []screenColumn {
	var boxes []screenColumn
	if typ == "post" {
		boxes = append(boxes, screenColumn{"formatdiv", "Format"}, screenColumn{"categorydiv", "Categories"}, screenColumn{"tagsdiv-post_tag", "Tags"})
	} else {
		boxes = append(boxes, screenColumn{"pageparentdiv", "Page Attributes"})
	}
	boxes = append(boxes, screenColumn{"postimagediv", "Featured image"})
	if typ == "post" {
		boxes = append(boxes, screenColumn{"postexcerpt", "Excerpt"}, screenColumn{"trackbacksdiv", "Send Trackbacks"})
	}
	boxes = append(boxes, screenColumn{"postcustom", "Custom Fields"}, screenColumn{"commentstatusdiv", "Discussion"}, screenColumn{"slugdiv", "Slug"})
	if author {
		boxes = append(boxes, screenColumn{"authordiv", "Author"})
	}
	if revisions {
		boxes = append(boxes, screenColumn{"revisionsdiv", "Revisions"})
	}
	return boxes
}

// metaRowHTML is _list_meta_row(): one row of the Custom Fields table.
func metaRowHTML(c *gin.Context, s *Server, r metaRow) string {
	id := strconv.FormatUint(uint64(r.ID), 10)
	return "\n\t<tr id='meta-" + id + "'>" +
		"\n\t\t<td class='left'><label class='screen-reader-text' for='meta-" + id + "-key'>Key</label><input name='meta[" + id + "][key]' id='meta-" + id + "-key' type='text' size='20' value='" + template.HTMLEscapeString(r.Key) + "' />" +
		"\n\t\t<div class='submit'>" +
		`<input type="submit" name="deletemeta[` + id + `]" id="deletemeta[` + id + `]" class="button deletemeta button-small" value="Delete" data-wp-lists="delete:the-list:meta-` + id + `::_ajax_nonce=` + r.DeleteNonce + `" />` +
		"\n\t\t" +
		`<input type="submit" name="meta-` + id + `-submit" id="meta-` + id + `-submit" class="button updatemeta button-small" value="Update" data-wp-lists="add:the-list:meta-` + id + `::_ajax_nonce-add-meta=` + s.nonce(c, "add-meta") + `" />` +
		"</div>" +
		`<input type="hidden" id="_ajax_nonce" name="_ajax_nonce" value="` + s.nonce(c, "change-meta") + `" />` +
		"</td>" +
		"\n\t\t<td><label class='screen-reader-text' for='meta-" + id + "-value'>Value</label><textarea name='meta[" + id + "][value]' id='meta-" + id + "-value' rows='2' cols='30'>" + template.HTMLEscapeString(r.Value) + "</textarea></td>\n\t</tr>"
}

// addMetaAjax is wp_ajax_add_meta(): adds a custom field (metakeyinput + metavalue) or updates one
// (meta[id][key] / meta[id][value]) and answers with the row.
func (s *Server) addMetaAjax(c *gin.Context) {
	ctx := c.Request.Context()
	u := currentUser(c)
	if !s.checkNonceValue(c, c.PostForm("_ajax_nonce-add-meta"), "add-meta") {
		c.String(http.StatusForbidden, "-1")
		return
	}
	pid, _ := strconv.ParseInt(c.PostForm("post_id"), 10, 64)
	p, err := s.store.GetPost(ctx, pid)
	if err != nil {
		c.String(http.StatusOK, "0")
		return
	}
	if !u.Can("edit_post", p) {
		c.String(http.StatusOK, "-1")
		return
	}
	if _, isAdd := c.GetPostForm("metakeyinput"); isAdd || c.PostForm("metakeyselect") != "" {
		key := strings.TrimSpace(c.PostForm("metakeyinput"))
		if sel := c.PostForm("metakeyselect"); sel != "" && sel != "#NONE#" && key == "" {
			key = sel
		}
		if key == "" {
			c.String(http.StatusOK, "1")
			return
		}
		value := c.PostForm("metavalue")
		if strings.TrimSpace(value) == "" {
			c.String(http.StatusOK, "Please provide a custom field value.")
			return
		}
		if strings.HasPrefix(key, "_") {
			c.String(http.StatusOK, "-1")
			return
		}
		if p.Status == "auto-draft" { // the post is saved as a draft first, like WordPress
			p.Status = "draft"
			p.Title = strings.TrimSpace(c.PostForm("post_title"))
		}
		p.Meta[key] = value
		if err := s.store.UpdatePost(ctx, p, u.ID); err != nil {
			c.String(http.StatusOK, "0")
			return
		}
		s.afterPostSave(ctx, p)
		id := metaRowID(key)
		sendAjaxResponse(c, ajaxResponse{What: "meta", Action: "add-meta", ID: int64(id), Position: "1",
			Data:         metaRowHTML(c, s, metaRow{ID: id, Key: key, Value: value, DeleteNonce: s.nonce(c, "delete-meta_"+strconv.FormatUint(uint64(id), 10))}),
			Supplemental: [][2]string{{"postid", itoa64(p.ID)}}})
		return
	}
	// Update: meta[<id>][key] and meta[<id>][value].
	var id uint32
	var key, value string
	for name, vals := range c.Request.PostForm {
		if rest, ok := strings.CutPrefix(name, "meta["); ok {
			mid, field, _ := strings.Cut(rest, "][")
			n, _ := strconv.ParseUint(mid, 10, 32)
			id = uint32(n)
			switch strings.TrimSuffix(field, "]") {
			case "key":
				key = vals[0]
			case "value":
				value = vals[0]
			}
		}
	}
	if strings.TrimSpace(key) == "" {
		c.String(http.StatusOK, "Please provide a custom field name.")
		return
	}
	oldKey, ok := metaKeyByID(p, id)
	if !ok {
		c.String(http.StatusOK, "0")
		return
	}
	if strings.HasPrefix(key, "_") {
		c.String(http.StatusOK, "-1")
		return
	}
	if oldKey != key || metaValueString(p.Meta[oldKey]) != value {
		delete(p.Meta, oldKey)
		p.Meta[key] = value
		if err := s.store.UpdatePost(ctx, p, u.ID); err != nil {
			c.String(http.StatusOK, "0")
			return
		}
		s.afterPostSave(ctx, p)
	}
	newID := metaRowID(key)
	sendAjaxResponse(c, ajaxResponse{What: "meta", Action: "add-meta", ID: int64(newID), OldID: int64(id), Position: "0",
		Data:         metaRowHTML(c, s, metaRow{ID: newID, Key: key, Value: value, DeleteNonce: s.nonce(c, "delete-meta_"+strconv.FormatUint(uint64(newID), 10))}),
		Supplemental: [][2]string{{"postid", itoa64(p.ID)}}})
}

// deleteMetaAjax is wp_ajax_delete_meta(): removes a custom field.
func (s *Server) deleteMetaAjax(c *gin.Context) {
	ctx := c.Request.Context()
	u := currentUser(c)
	idStr := c.PostForm("id")
	if !s.checkNonceValue(c, c.PostForm("_ajax_nonce"), "delete-meta_"+idStr) {
		c.String(http.StatusForbidden, "-1")
		return
	}
	n, _ := strconv.ParseUint(idStr, 10, 32)
	pid, _ := strconv.ParseInt(c.PostForm("post_id"), 10, 64)
	p, err := s.store.GetPost(ctx, pid)
	if err != nil {
		c.String(http.StatusOK, "1")
		return
	}
	key, ok := metaKeyByID(p, uint32(n))
	if !ok {
		c.String(http.StatusOK, "1")
		return
	}
	if !u.Can("edit_post", p) {
		c.String(http.StatusOK, "-1")
		return
	}
	delete(p.Meta, key)
	if err := s.store.UpdatePost(ctx, p, u.ID); err != nil {
		c.String(http.StatusOK, "0")
		return
	}
	s.afterPostSave(ctx, p)
	c.String(http.StatusOK, "1")
}

// touchTime holds the fields of touch_time(): the classic editor's date picker.
type touchTime struct {
	MM, JJ, AA, HH, MN, SS string
	Months                 []monthChoice
}

type monthChoice struct {
	Value, Label string
	Selected     bool
}

func (s *Server) postNew(c *gin.Context) {
	u := currentUser(c)
	typ := c.DefaultQuery("post_type", "post")
	if typ != "post" && typ != "page" {
		c.String(http.StatusBadRequest, "Invalid post type.")
		return
	}
	if !u.Can("edit_"+typ+"s", nil) {
		c.String(http.StatusForbidden, "Sorry, you are not allowed to create posts as this user.")
		return
	}
	p, err := s.store.CreateAutoDraft(c, typ, u.ID)
	if err != nil {
		c.String(http.StatusInternalServerError, err.Error())
		return
	}
	p.Title = c.Query("post_title")
	p.Content = c.Query("content")
	s.renderEdit(c, p, true)
}

// postEdit is post.php: action=edit shows the editor, trash/untrash/delete act on the post.
func (s *Server) postEdit(c *gin.Context) {
	u := currentUser(c)
	id, _ := strconv.ParseInt(c.Query("post"), 10, 64)
	p, err := s.store.GetPost(c, id)
	if errors.Is(err, store.ErrNotFound) {
		if a, aerr := s.store.GetAttachment(c, id); aerr == nil {
			if c.Query("action") == "delete" {
				if !s.checkNonce(c, "delete-post_"+itoa64(id)) {
					s.denyNonce(c)
					return
				}
				if a.UploaderID != currentUser(c).ID && !currentUser(c).Can("delete_others_posts", nil) {
					c.String(http.StatusForbidden, "Sorry, you are not allowed to delete this item.")
					return
				}
				s.deleteAttachmentFiles(a)
				_ = s.store.DeleteAttachment(c, id)
				c.Redirect(http.StatusFound, "/wp-admin/upload?deleted=1")
				return
			}
			s.attachmentEdit(c, a)
			return
		}
		c.String(http.StatusNotFound, "You attempted to edit an item that does not exist. Perhaps it was deleted?")
		return
	}
	if err != nil {
		c.String(http.StatusInternalServerError, err.Error())
		return
	}
	switch c.DefaultQuery("action", "edit") {
	case "trash", "untrash", "delete":
		action := c.Query("action")
		if !s.checkNonce(c, action+"-post_"+itoa64(p.ID)) {
			s.denyNonce(c)
			return
		}
		if !u.Can("delete_post", p) {
			c.String(http.StatusForbidden, "Sorry, you are not allowed to move this item to the Trash.")
			return
		}
		key := ""
		s.afterPostSave(c, p)
		switch action {
		case "trash":
			err, key = s.store.TrashPost(c, p.ID), "trashed"
		case "untrash":
			err, key = s.store.UntrashPost(c, p.ID), "untrashed"
		case "delete":
			err, key = s.store.DeletePost(c, p.ID), "deleted"
		}
		if err != nil {
			c.String(http.StatusInternalServerError, err.Error())
			return
		}
		c.Redirect(http.StatusFound, "/wp-admin/edit?post_type="+p.Type+"&"+key+"=1&ids="+itoa64(p.ID))
		return
	}
	if !u.Can("edit_post", p) {
		c.String(http.StatusForbidden, "Sorry, you are not allowed to edit this item.")
		return
	}
	if p.Status == "trash" {
		c.String(http.StatusForbidden, "You cannot edit this item because it is in the Trash. Please restore it and try again.")
		return
	}
	if c.Query("action") == "elementor" && s.golementorOn() { // Golementor
		s.gweEditor(c, p)
		return
	}
	s.renderEdit(c, p, false)
}

func (s *Server) renderEdit(c *gin.Context, p *store.Post, isNew bool) {
	block := s.useBlockEditor(c, p)
	s.gweEditorButton(c, p, block)
	if block {
		if _, ok := c.GetQuery("classic-editor__forget"); ok {
			delete(p.Meta, "classic-editor-remember")
		}
		s.rememberEditor(c, p, "block-editor")
		s.renderBlockEditor(c, p, isNew)
		return
	}
	s.rememberEditor(c, p, "classic-editor")
	s.renderClassicEdit(c, p, isNew)
}

// rememberEditor stores the editor last used for a post when users may switch editors.
func (s *Server) rememberEditor(c *gin.Context, p *store.Post, which string) {
	if s.settings.String("classic-editor-allow-users") != "allow" || p.Meta["classic-editor-remember"] == which {
		return
	}
	p.Meta["classic-editor-remember"] = which
	_, _ = s.store.Pool.Exec(c, `UPDATE posts SET meta = meta || jsonb_build_object('classic-editor-remember', $2::text) WHERE id = $1`, p.ID, which)
}

func (s *Server) renderClassicEdit(c *gin.Context, p *store.Post, isNew bool) {
	ctx := c.Request.Context()
	u := currentUser(c)
	lab := labelsFor(p.Type)
	lb := s.links(ctx)
	loc := s.loc()
	d := editData{Post: p, Labels: lab, IsNew: isNew, Nonce: s.nonce(c, "update-post_"+itoa64(p.ID)),
		CanPublish: u.Can("publish_post", p), CanAuthor: u.Can("edit_others_"+p.Type+"s", nil),
		Checked: map[int64]bool{}, TrashURL: "/wp-admin/post?post=" + itoa64(p.ID) + "&action=trash&_wpnonce=" + s.nonce(c, "trash-post_"+itoa64(p.ID)),
		Formats: []string{"standard", "aside", "image", "video", "quote", "link", "gallery", "status", "audio", "chat"}}

	if p.Type == "post" {
		d.Categories, _ = s.store.HierarchicalTerms(ctx, "category", false)
		cats, _ := s.store.TermsForPosts(ctx, []int64{p.ID}, "category")
		for _, t := range cats[p.ID] {
			d.Checked[t.ID] = true
		}
		if isNew {
			d.Checked[int64(s.settings.Int("default_category"))] = true
		}
		tags, _ := s.store.TermsForPosts(ctx, []int64{p.ID}, "post_tag")
		var names []string
		for _, t := range tags[p.ID] {
			names = append(names, t.Name)
		}
		d.TagNames = strings.Join(names, ", ")
	} else {
		d.Pages, _, _ = s.store.QueryPosts(ctx, store.PostQuery{Types: []string{"page"}, Statuses: []string{"publish", "private", "draft", "pending", "future"}, ExcludeID: []int64{p.ID}, OrderBy: "title", Order: "asc", PerPage: -1})
	}
	if d.CanAuthor {
		d.Authors, _ = s.store.Authors(ctx)
	}
	if p.Status != "auto-draft" {
		d.Permalink = lb.Permalink(p)
		if p.Status == "publish" || p.Status == "private" {
			d.ViewURL = d.Permalink
		} else {
			d.ViewURL = d.Permalink + previewSep(d.Permalink) + "preview=true"
		}
	}
	if p.Status != "auto-draft" {
		d.SampleLink = s.samplePermalinkHTML(c, p, p.Title, p.Slug)
	}
	d.SampleNonce = s.nonce(c, "samplepermalink")
	d.SlugBase = strings.TrimSuffix(strings.Replace(lb.PermalinkTemplate(p), "%postname%", "", 1), "/")
	d.SlugBase = strings.Replace(d.SlugBase, "%pagename%", "", 1)
	// Revisions: the box and the publish-box row appear once there is more than one, like WordPress.
	if p.Status != "auto-draft" {
		if revs, _ := s.store.ListRevisions(ctx, p.ID, false); len(revs) > 1 {
			d.Revisions = len(revs)
			d.LatestRevision = revs[0].ID
			d.RevisionsList = s.revisionsListHTML(c, revs)
		}
	}
	d.MetaRows = s.customFields(c, p)
	for _, r := range d.MetaRows {
		d.MetaRowsHTML += template.HTML(metaRowHTML(c, s, r))
	}
	d.MetaNonce = s.nonce(c, "add-meta")
	d.ClosedNonce = s.nonce(c, "closedpostboxes")
	d.OrderNonce = s.nonce(c, "meta-box-order")
	d.ToPing, _ = p.Meta["_to_ping"].(string)
	d.ToPing = strings.ReplaceAll(d.ToPing, "\n", " ") // post_trackback_meta_box() shows to_ping space-separated
	if p.Type == "page" {
		d.Templates = s.pageTemplates()
		d.Template = "default"
		if tpl, _ := p.Meta["_wp_page_template"].(string); tpl != "" {
			d.Template = tpl
		}
	}
	// Hidden and closed metaboxes come from the user's Screen Options (postbox_classes()).
	hidden := userMetaSet(u, "metaboxhidden_"+p.Type)
	if _, set := u.Meta["metaboxhidden_"+p.Type]; !set {
		for _, id := range defaultHiddenBoxes {
			hidden[id] = true
		}
	}
	closed := userMetaSet(u, "closedpostboxes_"+p.Type)
	d.BoxClass = map[string]string{}
	for id := range hidden {
		d.BoxClass[id] += " hide-if-js"
	}
	for id := range closed {
		d.BoxClass[id] += " closed"
	}
	s.setScreenOptions(c, screenOptions{Screen: p.Type, Boxes: editBoxes(p.Type, d.Revisions > 0, d.CanAuthor), Hidden: hidden})

	switch p.Status {
	case "publish":
		d.StatusLabel = "Published"
	case "future":
		d.StatusLabel = "Scheduled"
	case "pending":
		d.StatusLabel = "Pending Review"
	case "private":
		d.StatusLabel = "Privately Published"
	default:
		d.StatusLabel = "Draft"
	}
	switch {
	case p.Status == "private":
		d.Visibility = "private"
	case p.Password != "":
		d.Visibility = "password"
	default:
		d.Visibility = "public"
	}
	if p.PublishedAt != nil {
		t := p.PublishedAt.In(loc)
		d.DateInput, d.TimeInput = t.Format("2006-01-02"), t.Format("15:04")
		switch p.Status {
		case "future":
			d.DateLabel = "Scheduled for: <b>" + wp.FormatDate(t, "M j, Y") + " at " + wp.FormatDate(t, "H:i") + "</b>"
		case "publish", "private":
			d.DateLabel = "Published on: <b>" + wp.FormatDate(t, "M j, Y") + " at " + wp.FormatDate(t, "H:i") + "</b>"
		default:
			d.DateLabel = "Schedule for: <b>" + wp.FormatDate(t, "M j, Y") + " at " + wp.FormatDate(t, "H:i") + "</b>"
		}
	} else {
		d.DateLabel = "Publish <b>immediately</b>"
	}
	switch {
	case p.Status == "publish" || p.Status == "private" || p.Status == "future":
		d.PublishLabel = "Update"
	case !d.CanPublish:
		d.PublishLabel = "Submit for Review"
	case p.PublishedAt != nil && p.PublishedAt.After(time.Now()):
		d.PublishLabel = "Schedule"
	default:
		d.PublishLabel = "Publish"
	}

	title := "Edit " + lab.Singular
	screen := "post?post_type=" + p.Type
	if isNew {
		title = lab.AddNew
		screen = "post-new"
		if p.Type == "page" {
			screen = "post-new?post_type=page"
		}
	}
	ts := time.Now().In(loc)
	if p.PublishedAt != nil {
		ts = p.PublishedAt.In(loc)
	}
	d.TS = touchTime{MM: ts.Format("01"), JJ: ts.Format("02"), AA: ts.Format("2006"), HH: ts.Format("15"), MN: ts.Format("04"), SS: ts.Format("05")}
	for m := 1; m <= 12; m++ {
		v := fmt.Sprintf("%02d", m)
		d.TS.Months = append(d.TS.Months, monthChoice{Value: v, Label: v + "-" + time.Month(m).String()[:3], Selected: v == d.TS.MM})
	}
	d.PluginStats = statsLine(s.pluginPostStats(c, p))
	d.Editor = s.wpEditor(c, p)
	d.Thumbnail = s.postThumbnailHTML(c, p, p.FeaturedMediaID)
	if s.settings.String("classic-editor-allow-users") == "allow" {
		d.BlockURL = "/wp-admin/post?post=" + itoa64(p.ID) + "&action=edit&classic-editor__forget"
	}
	if !isNew {
		d.LastEdit = "Last edited on " + wp.FormatDate(p.ModifiedAt.In(loc), "F j, Y") + " at " + wp.FormatDate(p.ModifiedAt.In(loc), "g:i a")
		if ed, err := s.store.UserByID(c, p.AuthorID); err == nil {
			d.LastEdit = "Last edited by " + ed.DisplayName + " on " + wp.FormatDate(p.ModifiedAt.In(loc), "F j, Y") + " at " + wp.FormatDate(p.ModifiedAt.In(loc), "g:i a")
		}
	}
	s.enqueueMedia(c, p.ID)
	s.adminPage(c, screen, title, "post-php post-type-"+p.Type, "admin/post-edit", d, s.editNotices(c, lab, d.ViewURL)...)
}

func (s *Server) editNotices(c *gin.Context, lab typeLabels, view string) []notice {
	restored := ""
	if rid, _ := strconv.ParseInt(c.Query("revision"), 10, 64); rid > 0 && c.Query("message") == "5" {
		if rev, err := s.store.GetRevision(c, rid); err == nil {
			restored = lab.Singular + " restored to revision from " + template.HTMLEscapeString(s.revisionTitle(c, rev, false)) + "."
		}
	}
	msg := map[string]string{
		"5":  restored,
		"1":  lab.Singular + " updated. <a href=\"" + template.HTMLEscapeString(view) + "\">View " + strings.ToLower(lab.Singular) + "</a>",
		"6":  lab.Singular + " published. <a href=\"" + template.HTMLEscapeString(view) + "\">View " + strings.ToLower(lab.Singular) + "</a>",
		"7":  lab.Singular + " saved.",
		"8":  lab.Singular + " submitted. <a target=\"_blank\" href=\"" + template.HTMLEscapeString(view) + "\">Preview " + strings.ToLower(lab.Singular) + "</a>",
		"9":  lab.Singular + " scheduled. <a target=\"_blank\" href=\"" + template.HTMLEscapeString(view) + "\">Preview " + strings.ToLower(lab.Singular) + "</a>",
		"10": lab.Singular + " draft updated. <a target=\"_blank\" href=\"" + template.HTMLEscapeString(view) + "\">Preview " + strings.ToLower(lab.Singular) + "</a>",
	}[c.Query("message")]
	if msg == "" {
		return nil
	}
	return []notice{{Type: "success", Message: template.HTML(msg), Dismissible: true}}
}

// postSave handles the classic edit form (editpost()).
func (s *Server) postSave(c *gin.Context) {
	ctx := c.Request.Context()
	u := currentUser(c)
	id, _ := strconv.ParseInt(c.PostForm("post_ID"), 10, 64)
	if c.PostForm("post_type") == "attachment" {
		if a, err := s.store.GetAttachment(ctx, id); err == nil {
			s.attachmentSave(c, a)
			return
		}
	}
	p, err := s.store.GetPost(ctx, id)
	if err != nil {
		c.String(http.StatusNotFound, "You attempted to edit an item that does not exist. Perhaps it was deleted?")
		return
	}
	if !s.checkNonce(c, "update-post_"+itoa64(p.ID)) {
		s.denyNonce(c)
		return
	}
	if !u.Can("edit_post", p) {
		c.String(http.StatusForbidden, "Sorry, you are not allowed to edit this item.")
		return
	}
	wasPublished := p.Status == "publish" || p.Status == "private" || p.Status == "future"
	p.Title = strings.TrimSpace(c.PostForm("post_title"))
	content := c.PostForm("content")
	if !u.Can("unfiltered_html", nil) {
		content = wp.KsesPost(content)
	}
	// Classic content is stored as written; wpautop runs on render for content without blocks.
	p.Content = strings.ReplaceAll(content, "\r\n", "\n")
	if raw := c.PostForm("_thumbnail_id"); raw != "" {
		if id, _ := strconv.ParseInt(raw, 10, 64); id > 0 {
			p.FeaturedMediaID = id
		} else {
			p.FeaturedMediaID = 0
		}
	}
	p.Excerpt = c.PostForm("excerpt")
	p.Slug = strings.TrimSpace(c.PostForm("post_name"))
	p.CommentStatus = map[bool]string{true: "open", false: "closed"}[c.PostForm("comment_status") == "open"]
	p.PingStatus = map[bool]string{true: "open", false: "closed"}[c.PostForm("ping_status") == "open"]
	if p.Type == "page" {
		p.ParentID, _ = strconv.ParseInt(c.PostForm("parent_id"), 10, 64)
		p.MenuOrder, _ = strconv.Atoi(c.PostForm("menu_order"))
		if tpl, ok := c.GetPostForm("page_template"); ok {
			setPageTemplate(p, tpl)
		}
	} else {
		p.Format = c.DefaultPostForm("post_format", "standard")
		// Send Trackbacks: the URLs are kept in to_ping, one per line, like edit_post() does.
		if raw, ok := c.GetPostForm("trackback_url"); ok {
			var urls []string
			for _, f := range strings.FieldsFunc(raw, func(r rune) bool { return r == ' ' || r == '\n' || r == '\r' || r == '\t' }) {
				urls = append(urls, f)
			}
			if len(urls) > 0 {
				p.Meta["_to_ping"] = strings.Join(urls, "\n")
			} else {
				delete(p.Meta, "_to_ping")
			}
		}
	}
	s.savePostMetaForm(c, p)
	if a, _ := strconv.ParseInt(c.PostForm("post_author_override"), 10, 64); a > 0 && u.Can("edit_others_"+p.Type+"s", nil) {
		p.AuthorID = a
	}
	if p.AuthorID == 0 {
		p.AuthorID = u.ID
	}

	// Date: the form carries a local date and time; empty means "immediately".
	if c.PostForm("edit_date") == "1" && c.PostForm("aa") != "" {
		num := func(k string) int { n, _ := strconv.Atoi(c.PostForm(k)); return n }
		when := time.Date(num("aa"), time.Month(num("mm")), num("jj"), num("hh"), num("mn"), num("ss"), 0, s.loc())
		p.PublishedAt = &when
	} else if d, t := c.PostForm("post_date"), c.PostForm("post_time"); d != "" {
		if when, err := time.ParseInLocation("2006-01-02 15:04", d+" "+defaultStr(t, "00:00"), s.loc()); err == nil {
			p.PublishedAt = &when
		}
	}

	visibility := c.DefaultPostForm("visibility", "public")
	p.Password = ""
	if visibility == "password" {
		p.Password = c.PostForm("post_password")
	}
	p.Sticky = p.Type == "post" && c.PostForm("sticky") == "sticky" && visibility == "public"

	// Status from the button that was pressed, like _wp_translate_postdata().
	switch {
	case c.PostForm("publish") != "":
		if u.Can("publish_post", p) {
			p.Status = "publish"
		} else {
			p.Status = "pending"
		}
	case c.PostForm("save") != "":
		if !wasPublished {
			p.Status = "draft"
			if c.PostForm("post_status") == "pending" {
				p.Status = "pending"
			}
		} else if st := c.PostForm("post_status"); st == "draft" || st == "pending" {
			// Status › Edit on a published post can switch it back to Draft or Pending Review.
			p.Status = st
		}
	}
	if visibility == "private" && u.Can("publish_post", p) && (p.Status == "publish" || wasPublished) {
		p.Status = "private"
	} else if p.Status == "private" && visibility != "private" {
		p.Status = "publish"
	}
	if p.Status == "auto-draft" {
		p.Status = "draft"
	}

	if err := s.store.UpdatePost(ctx, p, u.ID); err != nil {
		c.String(http.StatusInternalServerError, err.Error())
		return
	}
	if p.Type == "post" {
		var catIDs []int64
		for _, raw := range c.PostFormArray("post_category[]") {
			if id, err := strconv.ParseInt(raw, 10, 64); err == nil && id > 0 {
				catIDs = append(catIDs, id)
			}
		}
		if len(catIDs) == 0 {
			catIDs = []int64{int64(s.settings.Int("default_category"))}
		}
		_ = s.store.SetPostTerms(ctx, p.ID, "category", catIDs)
		tagIDs, _ := s.store.TermIDsByNames(ctx, "post_tag", strings.Split(c.PostForm("tax_input[post_tag]"), ","))
		_ = s.store.SetPostTerms(ctx, p.ID, "post_tag", tagIDs)
	}

	msg := "1"
	switch {
	case !wasPublished && (p.Status == "publish" || p.Status == "private"):
		msg = "6"
	case p.Status == "future":
		msg = "9"
	case p.Status == "pending":
		msg = "8"
	case p.Status == "draft":
		msg = "10"
	}
	s.afterPostSave(ctx, p)
	if target, ok := s.gweClassicRedirect(c, p.ID); ok { // classic editor's "Edit with Golementor" on a new post
		c.Redirect(http.StatusFound, target)
		return
	}
	c.Redirect(http.StatusFound, links.EditLink(p.ID)+"&message="+msg)
}

// savePostMetaForm applies the Custom Fields box of the edit form the way edit_post() does: rows
// (meta[id][key] / meta[id][value]) are updated, deletemeta[id] rows removed, and a filled
// metakeyinput + metavalue pair added. Protected (underscore) keys are never touched.
func (s *Server) savePostMetaForm(c *gin.Context, p *store.Post) {
	type change struct{ key, value string }
	updates := map[uint32]*change{}
	for name, vals := range c.Request.PostForm {
		if rest, ok := strings.CutPrefix(name, "deletemeta["); ok {
			n, _ := strconv.ParseUint(strings.TrimSuffix(rest, "]"), 10, 32)
			if key, ok := metaKeyByID(p, uint32(n)); ok {
				delete(p.Meta, key)
			}
			continue
		}
		rest, ok := strings.CutPrefix(name, "meta[")
		if !ok {
			continue
		}
		mid, field, _ := strings.Cut(rest, "][")
		n, _ := strconv.ParseUint(mid, 10, 32)
		ch := updates[uint32(n)]
		if ch == nil {
			ch = &change{}
			updates[uint32(n)] = ch
		}
		switch strings.TrimSuffix(field, "]") {
		case "key":
			ch.key = vals[0]
		case "value":
			ch.value = vals[0]
		}
	}
	for id, ch := range updates {
		oldKey, ok := metaKeyByID(p, id)
		if !ok || strings.TrimSpace(ch.key) == "" || strings.HasPrefix(ch.key, "_") {
			continue
		}
		delete(p.Meta, oldKey)
		p.Meta[ch.key] = ch.value
	}
	key := strings.TrimSpace(c.PostForm("metakeyinput"))
	if sel := c.PostForm("metakeyselect"); key == "" && sel != "" && sel != "#NONE#" {
		key = sel
	}
	if key != "" && !strings.HasPrefix(key, "_") && strings.TrimSpace(c.PostForm("metavalue")) != "" {
		p.Meta[key] = c.PostForm("metavalue")
	}
}

func defaultStr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// sanitizeKey is sanitize_key(): lowercase letters, digits, dashes and underscores only.
func sanitizeKey(v string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(v) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		}
	}
	return b.String()
}
