package server

import (
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"wpgo/internal/store"
)

// The Flipbook screens (wp-admin/admin?page=pursae-flipbook): the list of books with their
// shortcodes, Add New (the Media Library modal, PDFs only), the edit screen with a live preview,
// and a full-screen viewer over the admin. Anyone who may upload files manages flipbooks.

type flipbookRow struct {
	flipbook
	Shortcode  string
	FullURL    string // /flipbook/N
	AbsFullURL string // the share link
	EditURL    string
	DeleteURL  string
	Date       string
}

type flipbookAdminData struct {
	View      string // list, edit
	Items     []flipbookRow
	Edit      flipbookRow
	Nonce     string
	AutoOpen  bool // Add New: the media modal opens on load
	AssetsURL string
	Version   string
}

func (s *Server) flipbookRowOf(c *gin.Context, b flipbook) flipbookRow {
	base := "/wp-admin/admin?page=" + flipbookSlug
	r := flipbookRow{flipbook: b, Shortcode: flipbookShortcodeText(b.ID), FullURL: flipbookPageURL(b.ID)}
	u := currentUser(c)
	if flipbookCan(u, b, "edit_others_posts") {
		r.EditURL = base + "&action=edit&id=" + strconv.Itoa(b.ID)
	}
	if flipbookCan(u, b, "delete_others_posts") {
		r.DeleteURL = base + "&action=delete&id=" + strconv.Itoa(b.ID) + "&_wpnonce=" + s.nonce(c, flipbookNonce+"-delete-"+strconv.Itoa(b.ID))
	}
	r.AbsFullURL = flipbookAbsURL(s.links(c).HomeURL("/"), r.FullURL)
	if t, err := time.Parse(time.RFC3339, b.Created); err == nil {
		r.Date = t.In(s.loc()).Format("2006/01/02 15:04")
	}
	return r
}

// flipbookCan: anyone who may upload files manages their own books; another user's book (or one
// made before authors were kept) needs others, edit_others_posts or delete_others_posts, like posts.
func flipbookCan(u *store.User, b flipbook, others string) bool {
	if u == nil || !u.Can("upload_files", nil) {
		return false
	}
	return (b.Author != 0 && b.Author == u.ID) || u.Can(others, nil)
}

// flipbookAdmin serves admin?page=pursae-flipbook.
func (s *Server) flipbookAdmin(c *gin.Context) {
	u := currentUser(c)
	if !s.flipbookOn() || u == nil || !u.Can("upload_files", nil) {
		c.String(http.StatusForbidden, "Sorry, you are not allowed to access this page.")
		return
	}
	base := "/wp-admin/admin?page=" + flipbookSlug
	if c.Request.Method == http.MethodPost {
		s.flipbookAdminPost(c, base)
		return
	}
	id, _ := strconv.Atoi(c.Query("id"))
	switch c.Query("action") {
	case "delete":
		if !s.checkNonce(c, flipbookNonce+"-delete-"+strconv.Itoa(id)) {
			s.denyNonce(c)
			return
		}
		err := s.flipbookUpdate(c, func(st *flipbookStore) error {
			i := slices.IndexFunc(st.Items, func(b flipbook) bool { return b.ID == id })
			if i < 0 {
				return nil
			}
			if !flipbookCan(u, st.Items[i], "delete_others_posts") {
				return errFlipbookNotYours
			}
			st.Items = slices.Delete(st.Items, i, i+1)
			return nil
		})
		if err != nil {
			c.Redirect(http.StatusFound, base+"&pfb-error="+url.QueryEscape(err.Error()))
			return
		}
		c.Redirect(http.StatusFound, base+"&pfb-msg=deleted")
		return
	}

	d := flipbookAdminData{View: "list", Nonce: s.nonce(c, flipbookNonce), AssetsURL: flipbookAssets, Version: flipbookVersion}
	screen, title := "admin?page="+flipbookSlug, "Flipbooks"
	if c.Query("action") == "edit" {
		b, ok := s.flipbookGet(id)
		if !ok {
			c.Redirect(http.StatusFound, base+"&pfb-error="+url.QueryEscape("That flipbook does not exist any more."))
			return
		}
		if !flipbookCan(u, b, "edit_others_posts") {
			c.Redirect(http.StatusFound, base+"&pfb-error="+url.QueryEscape(errFlipbookNotYours.Error()))
			return
		}
		d.View, d.Edit, title = "edit", s.flipbookRowOf(c, b), "Edit Flipbook"
	} else {
		for _, b := range s.flipbookLoad().Items {
			d.Items = append(d.Items, s.flipbookRowOf(c, b))
		}
		slices.Reverse(d.Items) // newest first
		d.AutoOpen = c.Query("action") == "new"
		if d.AutoOpen {
			screen += "&action=new"
		}
	}
	s.enqueueMedia(c, 0)

	var notices []notice
	switch c.Query("pfb-msg") {
	case "created":
		notices = append(notices, notice{Type: "success", Message: "Flipbook created. Copy its shortcode into any post or page.", Dismissible: true})
	case "saved":
		notices = append(notices, notice{Type: "success", Message: "Flipbook saved.", Dismissible: true})
	case "deleted":
		notices = append(notices, notice{Type: "success", Message: "Flipbook deleted. The PDF stays in the Media Library.", Dismissible: true})
	}
	if c.Query("activated") == "1" {
		notices = append(notices, notice{Type: "success", Message: "<strong>Pursae Flipbook is active.</strong> Click <em>Add New</em> and pick a PDF from the Media Library.", Dismissible: true})
	}
	if e := c.Query("pfb-error"); e != "" {
		notices = append(notices, notice{Type: "error", Message: template.HTML(template.HTMLEscapeString(e))})
	}
	s.adminPage(c, screen, title, "toplevel_page_"+flipbookSlug, "admin/flipbook", d, notices...)
}

// flipbookAdminPost creates a book from a picked PDF or saves the edit form.
func (s *Server) flipbookAdminPost(c *gin.Context, base string) {
	if !s.checkNonce(c, flipbookNonce) {
		s.denyNonce(c)
		return
	}
	str := func(k string) string { return strings.TrimSpace(c.PostForm(k)) }
	fail := func(back string, err error) {
		c.Redirect(http.StatusFound, back+"&pfb-error="+url.QueryEscape(err.Error()))
	}
	u := currentUser(c)
	// The PDF is the Media Library's: the picked attachment is looked up here and its own URL kept;
	// the address and type the form carries are not trusted.
	attID, _ := strconv.ParseInt(str("attachment_id"), 10, 64)
	var pdfURL, fileName string
	if str("op") == "create" || attID != 0 {
		var err error
		if pdfURL, fileName, err = s.flipbookPDF(c, attID); err != nil {
			fail(base, err)
			return
		}
	}
	switch str("op") {
	case "create":
		title := str("title")
		if title == "" {
			title = strings.TrimSuffix(fileName, path.Ext(fileName))
		}
		var id int
		err := s.flipbookUpdate(c, func(st *flipbookStore) error {
			b := flipbookNew(title, pdfURL, fileName, attID, u.ID)
			b.ID = st.NextID
			st.NextID++
			st.Items = append(st.Items, b)
			id = b.ID
			return nil
		})
		if err != nil {
			fail(base, err)
			return
		}
		c.Redirect(http.StatusFound, base+"&action=edit&id="+strconv.Itoa(id)+"&pfb-msg=created")
	case "save":
		id, _ := strconv.Atoi(str("id"))
		back := base + "&action=edit&id=" + strconv.Itoa(id)
		err := s.flipbookUpdate(c, func(st *flipbookStore) error {
			for i := range st.Items {
				b := &st.Items[i]
				if b.ID != id {
					continue
				}
				if !flipbookCan(u, *b, "edit_others_posts") {
					return errFlipbookNotYours
				}
				if t := str("title"); t != "" {
					b.Title = t
				}
				if pdfURL != "" {
					b.PDFURL, b.AttachmentID, b.FileName = pdfURL, attID, fileName
				}
				b.Height, _ = strconv.Atoi(str("height"))
				b.Background = str("background")
				b.Download = c.PostForm("download") == "1"
				b.Share = c.PostForm("share") == "1"
				b.Modified = time.Now().UTC().Format(time.RFC3339)
				*b = flipbookSanitize(*b)
				return nil
			}
			return errors.New("That flipbook does not exist any more.")
		})
		if err != nil {
			fail(back, err)
			return
		}
		c.Redirect(http.StatusFound, back+"&pfb-msg=saved")
	default:
		c.Redirect(http.StatusFound, base)
	}
}

var errFlipbookNotYours = errors.New("Sorry, you are not allowed to change that flipbook.")

// flipbookPDF is the public URL and file name of a PDF in the Media Library.
func (s *Server) flipbookPDF(c *gin.Context, id int64) (string, string, error) {
	errPick := errors.New("Please pick a PDF file from the Media Library.")
	if id <= 0 {
		return "", "", errPick
	}
	a, err := s.store.GetAttachment(c, id)
	if err != nil || a == nil {
		return "", "", errPick
	}
	// Only a PDF the user may edit: otherwise an author could publish another user's file as a book.
	if !canEditAttachment(currentUser(c), a) {
		return "", "", errPick
	}
	if !strings.EqualFold(a.Mime, "application/pdf") || !strings.EqualFold(path.Ext(a.FilePath), ".pdf") {
		return "", "", errPick
	}
	if mediaIsPrivateRel(a.FilePath) {
		return "", "", errors.New("That PDF is private. Visitors could not open it; pick a public PDF.")
	}
	return s.uploadURL(c, a.FilePath), path.Base(a.FilePath), nil
}
