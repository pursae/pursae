package server

import (
	"bytes"
	"errors"
	"html/template"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"wpgo/internal/store"
)

type taxLabels struct {
	Tax, Name, Singular, AddNew, Search, NotFound, ParentLabel, Description, Screen string
	Hierarchical                                                                    bool
	FilterParam                                                                     string
}

func taxLabelsFor(tax string) (taxLabels, bool) {
	switch tax {
	case "category":
		return taxLabels{Tax: "category", Name: "Categories", Singular: "Category", AddNew: "Add Category",
			Search: "Search Categories", NotFound: "No categories found.", ParentLabel: "Parent Category",
			Description: "The description is not prominent by default; however, some themes may show it.",
			Screen:      "edit-tags?taxonomy=category", Hierarchical: true, FilterParam: "category_name"}, true
	case "post_tag":
		return taxLabels{Tax: "post_tag", Name: "Tags", Singular: "Tag", AddNew: "Add Tag",
			Search: "Search Tags", NotFound: "No tags found.",
			Description: "The description is not prominent by default; however, some themes may show it.",
			Screen:      "edit-tags?taxonomy=post_tag", FilterParam: "tag"}, true
	}
	return taxLabels{}, false
}

type termRow struct {
	*store.Term
	Indent             template.HTML
	EditURL, DeleteURL string
	ViewURL, CountURL  string
	IsDefault          bool
}

type termsData struct {
	Labels      taxLabels
	Rows        []termRow
	All         []*store.Term // parent dropdown
	Search      string
	AddNonce    string
	BulkNonce   string
	InlineNonce string // Quick Edit (taxinlineeditnonce)
	Pagination  pagination
	Form        *store.Term // values to refill the add form after an error
	SortLinks   map[string]sortLink
	Hidden      map[string]bool // columns hidden through Screen Options
	ColCount    int
}

// termListColumns are the Screen Options column toggles of edit-tags.php.
var termListColumns = []screenColumn{{"description", "Description"}, {"slug", "Slug"}, {"posts", "Count"}}

func (s *Server) termsList(c *gin.Context) {
	u := currentUser(c)
	if s.termDeleteFromQuery(c) {
		return
	}
	lab, ok := taxLabelsFor(c.DefaultQuery("taxonomy", "post_tag"))
	if !ok {
		c.String(http.StatusBadRequest, "Invalid taxonomy.")
		return
	}
	if !u.Can("manage_categories", nil) {
		c.String(http.StatusForbidden, "Sorry, you are not allowed to manage terms in this taxonomy.")
		return
	}
	s.renderTerms(c, lab, nil, termNotices(c, lab)...)
}

func (s *Server) renderTerms(c *gin.Context, lab taxLabels, form *store.Term, notices ...notice) {
	ctx := c.Request.Context()
	search := strings.TrimSpace(c.Query("s"))
	page, _ := strconv.Atoi(c.Query("paged"))
	if page < 1 {
		page = 1
	}
	u := currentUser(c)
	perPage := s.userOptionInt(u, "edit_"+lab.Tax+"_per_page", 20)
	d := termsData{Labels: lab, Search: search, AddNonce: s.nonce(c, "add-tag"), BulkNonce: s.nonce(c, "bulk-tags"), Form: form,
		InlineNonce: s.nonce(c, "taxinlineeditnonce"), Hidden: s.hiddenColumns(u, "edit-"+lab.Tax),
		SortLinks: sortLinks(c, map[string]bool{"name": false, "description": false, "slug": false, "count": true}, "name", "asc")}
	d.ColCount = 2
	for _, col := range termListColumns {
		if !d.Hidden[col.ID] {
			d.ColCount++
		}
	}

	var terms []*store.Term
	var total int
	if lab.Hierarchical && search == "" && c.Query("orderby") == "" {
		all, err := s.store.HierarchicalTerms(ctx, lab.Tax, false)
		if err != nil {
			c.String(http.StatusInternalServerError, err.Error())
			return
		}
		total = len(all)
		start, end := (page-1)*perPage, page*perPage
		if start > total {
			start = total
		}
		if end > total {
			end = total
		}
		terms = all[start:end]
	} else {
		var err error
		terms, total, err = s.store.QueryTerms(ctx, store.TermQuery{Taxonomy: lab.Tax, Search: search,
			OrderBy: c.DefaultQuery("orderby", "name"), Order: c.DefaultQuery("order", "asc"), Page: page, PerPage: perPage})
		if err != nil {
			c.String(http.StatusInternalServerError, err.Error())
			return
		}
	}
	for _, t := range terms {
		d.Rows = append(d.Rows, s.termRow(c, lab, t))
	}
	if lab.Hierarchical {
		d.All, _ = s.store.HierarchicalTerms(ctx, lab.Tax, false)
	}
	d.Pagination = newPagination(total, perPage, page, *c.Request.URL)
	s.setScreenOptions(c, screenOptions{Screen: "edit-" + lab.Tax, Columns: termListColumns, Hidden: d.Hidden,
		PerPageOption: "edit_" + lab.Tax + "_per_page", PerPage: perPage})
	s.pageAssets(c).Script("inline-edit-tax")
	s.adminPage(c, lab.Screen, lab.Name, "edit-tags-php taxonomy-"+lab.Tax, "admin/terms", d, notices...)
}

// termRow builds one list row (WP_Terms_List_Table::single_row()).
func (s *Server) termRow(c *gin.Context, lab taxLabels, t *store.Term) termRow {
	r := termRow{Term: t, IsDefault: lab.Tax == "category" && t.ID == int64(s.settings.Int("default_category")),
		EditURL:   "/wp-admin/term?taxonomy=" + lab.Tax + "&tag_ID=" + itoa64(t.ID) + "&post_type=post",
		DeleteURL: "/wp-admin/edit-tags?action=delete&taxonomy=" + lab.Tax + "&tag_ID=" + itoa64(t.ID) + "&_wpnonce=" + s.nonce(c, "delete-tag_"+itoa64(t.ID)),
		ViewURL:   s.links(c).TermLink(t),
		CountURL:  "/wp-admin/edit?" + lab.FilterParam + "=" + t.Slug,
	}
	if t.Depth > 0 {
		r.Indent = template.HTML(strings.Repeat("&#8212; ", t.Depth))
	}
	return r
}

// inlineSaveTax is wp_ajax_inline_save_tax(): Quick Edit on edit-tags.php saves the name and slug
// and gets the re-rendered row back.
func (s *Server) inlineSaveTax(c *gin.Context) {
	ctx := c.Request.Context()
	if !s.checkNonceValue(c, c.PostForm("_inline_edit"), "taxinlineeditnonce") {
		c.String(http.StatusForbidden, "-1")
		return
	}
	lab, ok := taxLabelsFor(c.PostForm("taxonomy"))
	if !ok {
		c.String(http.StatusOK, "0")
		return
	}
	id, _ := strconv.ParseInt(c.PostForm("tax_ID"), 10, 64)
	if id == 0 || !currentUser(c).Can("manage_categories", nil) {
		c.String(http.StatusOK, "-1")
		return
	}
	t, err := s.store.GetTerm(ctx, id)
	if err != nil || t.Taxonomy != lab.Tax {
		c.String(http.StatusOK, "Item not updated.")
		return
	}
	t.Name = strings.TrimSpace(c.PostForm("name"))
	t.Slug = c.PostForm("slug")
	if t.Name == "" {
		c.String(http.StatusOK, "A name is required for this term.")
		return
	}
	if err := s.store.UpdateTerm(ctx, t); err != nil {
		c.String(http.StatusOK, template.HTMLEscapeString(err.Error()))
		return
	}
	s.purgeTags("term:"+itoa64(t.ID), "widgets")
	t, _ = s.store.GetTerm(ctx, t.ID)
	for parent := t.ParentID; parent > 0 && t.Depth < 50; t.Depth++ {
		pt, err := s.store.GetTerm(ctx, parent)
		if err != nil {
			break
		}
		parent = pt.ParentID
	}
	d := termsData{Labels: lab, Hidden: s.hiddenColumns(currentUser(c), "edit-"+lab.Tax)}
	var buf bytes.Buffer
	if err := s.tmpl.get(false).ExecuteTemplate(&buf, "terms-row", map[string]any{"R": s.termRow(c, lab, t), "D": d}); err != nil {
		c.String(http.StatusOK, err.Error())
		return
	}
	c.Data(http.StatusOK, "text/html; charset=UTF-8", buf.Bytes())
}

// tagSearchAjax is wp_ajax_ajax_tag_search(): tags-suggest.js asks for term names matching the typed text.
func (s *Server) tagSearchAjax(c *gin.Context) {
	tax := c.DefaultQuery("tax", c.PostForm("tax"))
	lab, ok := taxLabelsFor(tax)
	if !ok || !currentUser(c).Can("edit_posts", nil) {
		c.String(http.StatusOK, "0")
		return
	}
	q := strings.TrimSpace(c.DefaultQuery("q", c.PostForm("q")))
	if i := strings.LastIndex(q, ","); i >= 0 {
		q = strings.TrimSpace(q[i+1:])
	}
	if len([]rune(q)) < 2 {
		c.String(http.StatusOK, "")
		return
	}
	terms, _, err := s.store.QueryTerms(c, store.TermQuery{Taxonomy: lab.Tax, Search: q, OrderBy: "name", Order: "asc", PerPage: 20})
	if err != nil {
		c.String(http.StatusOK, "")
		return
	}
	names := make([]string, 0, len(terms))
	for _, t := range terms {
		names = append(names, t.Name)
	}
	c.String(http.StatusOK, strings.Join(names, "\n"))
}

func termNotices(c *gin.Context, lab taxLabels) []notice {
	msg := map[string]string{
		"1": lab.Singular + " added.",
		"2": lab.Singular + " deleted.",
		"3": lab.Singular + " updated.",
		"6": lab.Name + " deleted.",
	}[c.Query("message")]
	if msg == "" {
		return nil
	}
	return []notice{{Type: "success", Message: template.HTML(msg), Dismissible: true}}
}

// termsAction handles the Add New form and the bulk delete on edit-tags.php.
func (s *Server) termsAction(c *gin.Context) {
	ctx := c.Request.Context()
	u := currentUser(c)
	lab, ok := taxLabelsFor(c.DefaultPostForm("taxonomy", c.Query("taxonomy")))
	if !ok {
		c.String(http.StatusBadRequest, "Invalid taxonomy.")
		return
	}
	if !u.Can("manage_categories", nil) {
		c.String(http.StatusForbidden, "Sorry, you are not allowed to manage terms in this taxonomy.")
		return
	}
	back := "/wp-admin/edit-tags?taxonomy=" + lab.Tax
	if s.setScreenOption(c) {
		return
	}

	if action := c.PostForm("action"); action == "delete" || c.PostForm("action2") == "delete" {
		if !s.checkNonce(c, "bulk-tags") {
			s.denyNonce(c)
			return
		}
		def := int64(s.settings.Int("default_category"))
		for _, raw := range c.PostFormArray("delete_tags[]") {
			id, _ := strconv.ParseInt(raw, 10, 64)
			t, err := s.store.GetTerm(ctx, id)
			if err != nil || t.Taxonomy != lab.Tax || (lab.Tax == "category" && id == def) {
				continue
			}
			s.purgeTags("term:"+itoa64(t.ID), "widgets")
			_ = s.store.DeleteTerm(ctx, t, def)
		}
		c.Redirect(http.StatusFound, back+"&message=6")
		return
	}

	if !s.checkNonce(c, "add-tag") {
		s.denyNonce(c)
		return
	}
	t := &store.Term{Taxonomy: lab.Tax, Name: strings.TrimSpace(c.PostForm("tag-name")),
		Slug: c.PostForm("slug"), Description: c.PostForm("description")}
	if lab.Hierarchical {
		if p, _ := strconv.ParseInt(c.PostForm("parent"), 10, 64); p > 0 {
			t.ParentID = p
		}
	}
	if t.Name == "" {
		s.renderTerms(c, lab, t, notice{Type: "error", Message: "A name is required for this term."})
		return
	}
	if err := s.store.InsertTerm(ctx, t); err != nil {
		msg := template.HTML(template.HTMLEscapeString(err.Error()))
		if !errors.Is(err, store.ErrTermExists) {
			msg = "Item not added."
		}
		s.renderTerms(c, lab, t, notice{Type: "error", Message: msg})
		return
	}
	c.Redirect(http.StatusFound, back+"&message=1")
}

type termEditData struct {
	Labels    taxLabels
	Term      *store.Term
	All       []*store.Term
	Nonce     string
	DeleteURL string
	ViewURL   string
	IsDefault bool
}

func (s *Server) termEdit(c *gin.Context) {
	ctx := c.Request.Context()
	u := currentUser(c)
	lab, ok := taxLabelsFor(c.Query("taxonomy"))
	if !ok || !u.Can("manage_categories", nil) {
		c.String(http.StatusForbidden, "Sorry, you are not allowed to edit this item.")
		return
	}
	id, _ := strconv.ParseInt(c.Query("tag_ID"), 10, 64)
	t, err := s.store.GetTerm(ctx, id)
	if err != nil || t.Taxonomy != lab.Tax {
		c.String(http.StatusNotFound, "You attempted to edit an item that does not exist. Perhaps it was deleted?")
		return
	}
	d := termEditData{Labels: lab, Term: t, Nonce: s.nonce(c, "update-tag_"+itoa64(t.ID)),
		DeleteURL: "/wp-admin/edit-tags?action=delete&taxonomy=" + lab.Tax + "&tag_ID=" + itoa64(t.ID) + "&_wpnonce=" + s.nonce(c, "delete-tag_"+itoa64(t.ID)),
		ViewURL:   s.links(ctx).TermLink(t),
		IsDefault: lab.Tax == "category" && t.ID == int64(s.settings.Int("default_category"))}
	if lab.Hierarchical {
		all, _ := s.store.HierarchicalTerms(ctx, lab.Tax, false)
		for _, o := range all {
			if o.ID != t.ID {
				d.All = append(d.All, o)
			}
		}
	}
	var notices []notice
	if c.Query("message") == "3" {
		notices = append(notices, notice{Type: "success", Message: template.HTML(lab.Singular + " updated."), Dismissible: true})
	}
	s.adminPage(c, "term?taxonomy="+lab.Tax, "Edit "+lab.Singular, "term-php taxonomy-"+lab.Tax, "admin/term-edit", d, notices...)
}

func (s *Server) termSave(c *gin.Context) {
	ctx := c.Request.Context()
	u := currentUser(c)
	lab, ok := taxLabelsFor(c.PostForm("taxonomy"))
	if !ok || !u.Can("manage_categories", nil) {
		c.String(http.StatusForbidden, "Sorry, you are not allowed to edit this item.")
		return
	}
	id, _ := strconv.ParseInt(c.PostForm("tag_ID"), 10, 64)
	if !s.checkNonce(c, "update-tag_"+itoa64(id)) {
		s.denyNonce(c)
		return
	}
	t, err := s.store.GetTerm(ctx, id)
	if err != nil || t.Taxonomy != lab.Tax {
		c.String(http.StatusNotFound, "You attempted to edit an item that does not exist. Perhaps it was deleted?")
		return
	}
	t.Name = strings.TrimSpace(c.PostForm("name"))
	t.Slug = c.PostForm("slug")
	t.Description = c.PostForm("description")
	if lab.Hierarchical {
		t.ParentID, _ = strconv.ParseInt(c.PostForm("parent"), 10, 64)
		if t.ParentID < 0 {
			t.ParentID = 0
		}
	}
	if t.Name == "" {
		c.String(http.StatusBadRequest, "A name is required for this term.")
		return
	}
	if err := s.store.UpdateTerm(ctx, t); err != nil {
		c.String(http.StatusInternalServerError, err.Error())
		return
	}
	s.purgeTags("term:"+itoa64(t.ID), "widgets")
	c.Redirect(http.StatusFound, "/wp-admin/term?taxonomy="+lab.Tax+"&tag_ID="+itoa64(t.ID)+"&message=3")
}

// termDelete handles the single "Delete" row action (GET with a nonce, like WordPress).
func (s *Server) termDeleteFromQuery(c *gin.Context) bool {
	if c.Query("action") != "delete" {
		return false
	}
	ctx := c.Request.Context()
	lab, ok := taxLabelsFor(c.Query("taxonomy"))
	id, _ := strconv.ParseInt(c.Query("tag_ID"), 10, 64)
	if !ok || !currentUser(c).Can("manage_categories", nil) {
		c.String(http.StatusForbidden, "Sorry, you are not allowed to delete this item.")
		return true
	}
	if !s.checkNonce(c, "delete-tag_"+itoa64(id)) {
		s.denyNonce(c)
		return true
	}
	def := int64(s.settings.Int("default_category"))
	if t, err := s.store.GetTerm(ctx, id); err == nil && t.Taxonomy == lab.Tax && !(lab.Tax == "category" && id == def) {
		s.purgeTags("term:"+itoa64(t.ID), "widgets")
		_ = s.store.DeleteTerm(ctx, t, def)
	}
	c.Redirect(http.StatusFound, "/wp-admin/edit-tags?taxonomy="+lab.Tax+"&message=2")
	return true
}
