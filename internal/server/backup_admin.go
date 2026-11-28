package server

import (
	"context"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// The Backup Manager screen (wp-admin/admin?page=backup-manager). Administrators only, plugin active.
// Screens: Create (parts to back up), Backups (stored archives), Import (upload an archive), and a
// restore confirmation per archive. Backups and restores run in the background; the screen polls
// action=status.

type bmgrCatView struct {
	Key, Label, Desc string
	Rows             int64
	FilesBytes       int64
	FilesText        string
	HasFiles         bool
	Checked          bool
	Tables           int
}

type bmgrRoleView struct{ Slug, Name string }

type bmgrUserView struct {
	ID          int64
	Login, Mail string
}

type bmgrRestoreCat struct {
	Key, Label string
	Rows       int64
	Tables     int
	Files      int
	FilesBytes int64
	FilesText  string
}

type bmgrAdminData struct {
	Tab         string
	Nonce       string
	Dir         string
	Cats        []bmgrCatView
	Roles       []bmgrRoleView
	Users       []bmgrUserView
	Entries     []bmgrEntryView
	Job         string // a job id to follow right away
	UsedText    string // total size of the stored archives
	MaxUpload   string
	SiteURL     string
	Restore     *bmgrRestoreView
	Schedule    *bmgrScheduleView
	Remote      *bmgrRemoteView
	RemoteReady bool // the Backups tab offers "Upload to remote storage"
	CanDownload bool
}

type bmgrEntryView struct {
	Name, SizeText, When, Kind, Site, Note string
	Parts                                  []string
	Err                                    string
	Safety                                 bool
	SiteKey                                bool // the archive carries the site's encryption key
	DownloadURL, RestoreURL, DeleteURL     string
}

type bmgrRestoreView struct {
	File, Created, Site, Version, Note string
	Cats                               []bmgrRestoreCat
	OldURL, NewURL                     string
	SameURL                            bool
	UserNote                           string
}

func (s *Server) bmgrBack(extra string) string {
	return "/wp-admin/admin?page=" + bmgrSlug + extra
}

// bmgrAdmin serves admin?page=backup-manager.
func (s *Server) bmgrAdmin(c *gin.Context) {
	if !s.bmgrAllowed(c) {
		c.String(http.StatusForbidden, "Sorry, you are not allowed to access this page.")
		return
	}
	if s.bmgrUpload(c) {
		return
	}
	switch c.Query("action") {
	case "status":
		if j := s.bmgrJobByID(c.Query("job")); j != nil {
			c.JSON(http.StatusOK, j)
		} else {
			c.JSON(http.StatusNotFound, gin.H{"error": "unknown job"})
		}
		return
	case "cancel":
		if !s.checkNonce(c, bmgrNonce) {
			s.denyNonce(c)
			return
		}
		c.JSON(http.StatusOK, gin.H{"canceled": s.bmgrCancel(c.Query("job"))})
		return
	case "download":
		if !s.checkNonce(c, bmgrNonce) {
			s.denyNonce(c)
			return
		}
		p := s.bmgrPath(c.Query("file"))
		if p == "" {
			c.String(http.StatusNotFound, "Backup not found.")
			return
		}
		c.Header("Cache-Control", "private, no-store")
		c.Header("X-Content-Type-Options", "nosniff")
		c.FileAttachment(p, filepath.Base(p))
		return
	case "delete":
		if !s.checkNonce(c, bmgrNonce) {
			s.denyNonce(c)
			return
		}
		if p := s.bmgrPath(c.Query("file")); p != "" {
			_ = os.Remove(p)
		}
		c.Redirect(http.StatusFound, s.bmgrBack("&tab=backups&bmgr-msg=deleted"))
		return
	}
	if c.Request.Method == http.MethodPost {
		if !s.checkNonce(c, bmgrNonce) {
			s.denyNonce(c)
			return
		}
		switch c.PostForm("do") {
		case "backup":
			s.bmgrPostBackup(c)
		case "restore":
			s.bmgrPostRestore(c)
		case "schedule":
			s.bmgrPostSchedule(c)
		case "remote-save", "remote-test", "remote-object", "remote-upload":
			s.bmgrPostRemote(c, c.PostForm("do"))
		default:
			c.Redirect(http.StatusFound, s.bmgrBack(""))
		}
		return
	}
	s.bmgrScreen(c)
}

// bmgrUpload receives an archive (POST admin?page=backup-manager&action=upload): streamed to disk,
// checked, then the browser goes straight to the restore screen.
func (s *Server) bmgrUpload(c *gin.Context) bool {
	if c.Query("action") != "upload" || c.Request.Method != http.MethodPost {
		return false
	}
	// The nonce travels in the query string: reading it from the form would buffer the whole upload.
	var uid int64
	var token string
	if u := currentUser(c); u != nil {
		uid = u.ID
	}
	if sess := currentSession(c); sess != nil {
		token = sess.Token
	}
	if s.auth.VerifyNonce(c.Query("_wpnonce"), bmgrNonce, uid, token) <= 0 {
		s.denyNonce(c)
		return true
	}
	limit := int64(16) << 30
	// A middleware that already parsed the form leaves the file in MultipartForm and an empty body;
	// take it from there instead of reporting that nothing was received.
	if mf := c.Request.MultipartForm; mf != nil {
		if fh := mf.File["archive"]; len(fh) > 0 && fh[0].Filename != "" && fh[0].Size <= limit {
			f, err := fh[0].Open()
			if err != nil {
				c.Redirect(http.StatusFound, s.bmgrBack("&tab=import&bmgr-error="+url.QueryEscape("The upload failed: "+err.Error())))
				return true
			}
			defer f.Close()
			s.bmgrSaveUpload(c, f)
			return true
		}
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, limit)
	mr, err := c.Request.MultipartReader()
	if err != nil {
		c.Redirect(http.StatusFound, s.bmgrBack("&tab=import&bmgr-error="+url.QueryEscape("No file was received.")))
		return true
	}
	for {
		part, err := mr.NextPart()
		if err != nil {
			break
		}
		if part.FormName() != "archive" || part.FileName() == "" {
			continue
		}
		s.bmgrSaveUpload(c, part)
		return true
	}
	c.Redirect(http.StatusFound, s.bmgrBack("&tab=import&bmgr-error="+url.QueryEscape("Choose a backup file first.")))
	return true
}

// bmgrSaveUpload stores an uploaded archive in the backups folder, checks it is a backup, and
// sends the browser to its restore screen.
func (s *Server) bmgrSaveUpload(c *gin.Context, src io.Reader) {
	if err := os.MkdirAll(s.bmgrDir(), 0o700); err != nil {
		c.Redirect(http.StatusFound, s.bmgrBack("&tab=import&bmgr-error="+url.QueryEscape(err.Error())))
		return
	}
	name := "uploaded-" + time.Now().Format("20060102-150405") + "-" + bmgrToken() + ".zip"
	dst := filepath.Join(s.bmgrDir(), name)
	f, err := os.OpenFile(dst+".part", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err == nil {
		_, err = io.Copy(f, src)
		f.Close()
	}
	if err != nil {
		os.Remove(dst + ".part")
		c.Redirect(http.StatusFound, s.bmgrBack("&tab=import&bmgr-error="+url.QueryEscape("The upload failed: "+err.Error())))
		return
	}
	if _, err := bmgrReadManifest(dst + ".part"); err != nil {
		os.Remove(dst + ".part")
		c.Redirect(http.StatusFound, s.bmgrBack("&tab=import&bmgr-error="+url.QueryEscape("That file is not a usable backup: "+err.Error())))
		return
	}
	if err := os.Rename(dst+".part", dst); err != nil {
		c.Redirect(http.StatusFound, s.bmgrBack("&tab=import&bmgr-error="+url.QueryEscape(err.Error())))
		return
	}
	c.Redirect(http.StatusFound, s.bmgrBack("&tab=restore&file="+url.QueryEscape(name)))
}

func (s *Server) bmgrPostBackup(c *gin.Context) {
	sel := bmgrSelection{Categories: c.PostFormArray("cat"), UserMode: c.PostForm("user_mode"), Roles: c.PostFormArray("role"),
		Note: strings.TrimSpace(c.PostForm("note")), IncludeKey: c.PostForm("include_key") == "1"}
	if len(sel.Note) > 300 {
		sel.Note = sel.Note[:300]
	}
	for _, v := range c.PostFormArray("user") {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil {
			sel.UserIDs = append(sel.UserIDs, id)
		}
	}
	if sel.UserMode != "roles" && sel.UserMode != "ids" {
		sel.UserMode = "all"
	}
	if sel.UserMode == "roles" && len(sel.Roles) == 0 || sel.UserMode == "ids" && len(sel.UserIDs) == 0 {
		c.Redirect(http.StatusFound, s.bmgrBack("&tab=create&bmgr-error="+url.QueryEscape("Choose at least one role or user, or back up all users.")))
		return
	}
	if len(sel.Categories) == 0 {
		c.Redirect(http.StatusFound, s.bmgrBack("&tab=create&bmgr-error="+url.QueryEscape("Select at least one part to back up.")))
		return
	}
	j, err := s.bmgrStart("backup")
	if err != nil {
		c.Redirect(http.StatusFound, s.bmgrBack("&tab=create&bmgr-error="+url.QueryEscape(err.Error())))
		return
	}
	safeGo("backup_admin.bmgrPostBackup", func() {
		name, err := s.bmgrRunBackup(s.bmgrCancelable(j), j, sel)
		s.bmgrUpdate(j, func(j *bmgrJob) { j.File = name })
		s.bmgrFinish(j, err)
	})
	c.Redirect(http.StatusFound, s.bmgrBack("&tab=create&job="+j.ID))
}

func (s *Server) bmgrPostRestore(c *gin.Context) {
	file := c.PostForm("file")
	if s.bmgrPath(file) == "" {
		c.Redirect(http.StatusFound, s.bmgrBack("&tab=backups&bmgr-error="+url.QueryEscape("The backup file was not found.")))
		return
	}
	back := s.bmgrBack("&tab=restore&file=" + url.QueryEscape(file))
	if c.PostForm("confirm") != "1" {
		c.Redirect(http.StatusFound, back+"&bmgr-error="+url.QueryEscape("Tick the confirmation box: a restore replaces the selected parts of this site."))
		return
	}
	req := bmgrRestoreReq{File: file, Categories: c.PostFormArray("cat"), RewriteURL: c.PostForm("rewrite") == "1", Safety: c.PostForm("safety") == "1", Me: currentUser(c).ID}
	if len(req.Categories) == 0 {
		c.Redirect(http.StatusFound, back+"&bmgr-error="+url.QueryEscape("Select at least one part to restore."))
		return
	}
	j, err := s.bmgrStart("restore")
	if err != nil {
		c.Redirect(http.StatusFound, back+"&bmgr-error="+url.QueryEscape(err.Error()))
		return
	}
	safeGo("backup_admin.bmgrPostRestore", func() { s.bmgrFinish(j, s.bmgrRunRestore(context.Background(), j, req)) })
	c.Redirect(http.StatusFound, back+"&job="+j.ID)
}

// bmgrScreen renders the page.
func (s *Server) bmgrScreen(c *gin.Context) {
	tab := c.DefaultQuery("tab", "create")
	switch tab {
	case "create", "backups", "schedule", "remote", "import", "restore":
	default:
		tab = "create"
	}
	d := bmgrAdminData{Tab: tab, Nonce: s.nonce(c, bmgrNonce), Dir: s.bmgrDir(), SiteURL: bmgrSiteURL(s), Job: c.Query("job")}
	var notices []notice
	switch c.Query("bmgr-msg") {
	case "deleted":
		notices = append(notices, notice{Type: "success", Message: "Backup deleted.", Dismissible: true})
	case "scheduled":
		notices = append(notices, notice{Type: "success", Message: "Schedule saved.", Dismissible: true})
	case "remote-saved":
		notices = append(notices, notice{Type: "success", Message: "Remote storage settings saved.", Dismissible: true})
	case "remote-ok":
		notices = append(notices, notice{Type: "success", Message: "Connection works: a test file was encrypted, written, listed, read back and deleted.", Dismissible: true})
	case "remote-deleted":
		notices = append(notices, notice{Type: "success", Message: "Remote backup deleted.", Dismissible: true})
	}
	if e := c.Query("bmgr-error"); e != "" {
		notices = append(notices, notice{Type: "error", Message: template.HTML(template.HTMLEscapeString(e))})
	}

	ctx := c.Request.Context()
	switch tab {
	case "create":
		d.Cats = s.bmgrCatViews(ctx)
		roles, _ := s.store.Roles(ctx)
		for _, r := range roles {
			d.Roles = append(d.Roles, bmgrRoleView{Slug: r.Slug, Name: r.Name})
		}
		rows, err := s.store.Pool.Query(ctx, `SELECT id, login, email FROM users ORDER BY login LIMIT 500`)
		if err == nil {
			for rows.Next() {
				var u bmgrUserView
				if rows.Scan(&u.ID, &u.Login, &u.Mail) == nil {
					d.Users = append(d.Users, u)
				}
			}
			rows.Close()
		}
	case "backups":
		d.UsedText = bmgrHuman(s.bmgrUsage())
		d.RemoteReady = s.bmgrRemote().problem() == nil
		for _, e := range s.bmgrList() {
			d.Entries = append(d.Entries, s.bmgrEntryView(c, e))
		}
	case "schedule":
		d.Schedule = s.bmgrScheduleView(c)
	case "remote":
		d.Remote = s.bmgrRemoteView(ctx)
	case "restore":
		file := c.Query("file")
		p := s.bmgrPath(file)
		if p == "" {
			c.Redirect(http.StatusFound, s.bmgrBack("&tab=backups&bmgr-error="+url.QueryEscape("The backup file was not found.")))
			return
		}
		m, err := bmgrReadManifest(p)
		if err != nil {
			c.Redirect(http.StatusFound, s.bmgrBack("&tab=backups&bmgr-error="+url.QueryEscape(err.Error())))
			return
		}
		d.Restore = s.bmgrRestoreView(file, m)
		if _, _, perr := s.bmgrMakePlan(ctx, m, bmgrManifestCats(m)); perr != nil {
			notices = append(notices, notice{Type: "error", Message: template.HTML(template.HTMLEscapeString(perr.Error()))})
			d.Restore = nil
		}
	}
	s.adminPage(c, "admin?page="+bmgrSlug, "Backup Manager", "toplevel_page_backup-manager", "admin/backup", d, notices...)
}

func bmgrManifestCats(m *bmgrManifest) []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range m.Tables {
		if !seen[t.Category] {
			seen[t.Category] = true
			out = append(out, t.Category)
		}
	}
	for _, f := range m.Files {
		for _, c := range bmgrCategories {
			if c.Files == f.Group && !seen[c.Key] {
				seen[c.Key] = true
				out = append(out, c.Key)
			}
		}
	}
	return out
}

// bmgrCatViews lists the categories with live row counts and file sizes.
func (s *Server) bmgrCatViews(ctx context.Context) []bmgrCatView {
	sc, err := bmgrLoadSchema(ctx, s.store.Pool)
	if err != nil {
		return nil
	}
	est := map[string]int64{}
	if rows, err := s.store.Pool.Query(ctx, `SELECT relname, GREATEST(reltuples, 0)::bigint FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = 'public' AND relkind = 'r'`); err == nil {
		for rows.Next() {
			var t string
			var n int64
			if rows.Scan(&t, &n) == nil {
				est[t] = n
			}
		}
		rows.Close()
	}
	roots := s.bmgrFileRoots()
	var out []bmgrCatView
	for _, c := range bmgrCategories {
		v := bmgrCatView{Key: c.Key, Label: c.Label, Desc: c.Desc, Checked: true, HasFiles: c.Files != ""}
		for _, t := range sc.Tables {
			if bmgrCategoryOf(t) == c.Key {
				v.Tables++
				v.Rows += est[t]
			}
		}
		if c.Files != "" {
			v.FilesBytes = bmgrDirSize(roots[c.Files])
			v.FilesText = bmgrHuman(v.FilesBytes)
		}
		if v.Tables == 0 && !v.HasFiles {
			continue
		}
		out = append(out, v)
	}
	return out
}

// bmgrDirSize is the total size of the regular files below dir (0 when it does not exist).
func bmgrDirSize(dir string) int64 {
	var n int64
	_ = filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err == nil && fi.Mode().IsRegular() {
			n += fi.Size()
		}
		return nil
	})
	return n
}

func (s *Server) bmgrEntryView(c *gin.Context, e bmgrEntry) bmgrEntryView {
	v := bmgrEntryView{Name: e.Name, SizeText: bmgrHuman(e.Size), When: e.Modified.In(s.loc()).Format("2006-01-02 15:04"), Err: e.Err}
	n := s.nonce(c, bmgrNonce)
	q := "&file=" + url.QueryEscape(e.Name)
	v.DownloadURL = s.bmgrBack("&action=download" + q + "&_wpnonce=" + n)
	v.DeleteURL = s.bmgrBack("&action=delete" + q + "&_wpnonce=" + n)
	if e.M != nil {
		v.RestoreURL = s.bmgrBack("&tab=restore" + q)
		v.Safety, v.SiteKey = e.M.Kind == "safety", e.M.SiteKey
		v.Site, v.Note = e.M.SiteURL, e.M.Note
		v.Kind = map[bool]string{true: "Safety snapshot", false: "Backup"}[v.Safety]
		switch e.M.Kind {
		case bmgrScheduleKind:
			v.Kind = "Scheduled backup"
		case bmgrPreUpdateKind:
			v.Kind = "Pre-update backup (database only)"
		}
		labels := map[string]string{}
		for _, c := range bmgrCategories {
			labels[c.Key] = c.Label
		}
		for _, k := range bmgrManifestCats(e.M) {
			v.Parts = append(v.Parts, labels[k])
		}
	}
	return v
}

func (s *Server) bmgrRestoreView(file string, m *bmgrManifest) *bmgrRestoreView {
	v := &bmgrRestoreView{File: file, Created: m.Created.In(s.loc()).Format("2006-01-02 15:04"), Site: m.SiteURL, Version: m.Version, Note: m.Note,
		OldURL: strings.TrimRight(m.SiteURL, "/"), NewURL: strings.TrimRight(bmgrSiteURL(s), "/")}
	v.SameURL = v.OldURL == v.NewURL || v.OldURL == ""
	if m.UserMode == "roles" || m.UserMode == "ids" {
		v.UserNote = strconv.Itoa(m.Users) + " selected users are in this backup."
	}
	labels := map[string]string{}
	for _, c := range bmgrCategories {
		labels[c.Key] = c.Label
	}
	byCat := map[string]*bmgrRestoreCat{}
	for _, t := range m.Tables {
		rc := byCat[t.Category]
		if rc == nil {
			rc = &bmgrRestoreCat{Key: t.Category, Label: labels[t.Category]}
			byCat[t.Category] = rc
		}
		rc.Tables++
		rc.Rows += t.Rows
	}
	for _, f := range m.Files {
		for _, c := range bmgrCategories {
			if c.Files != f.Group {
				continue
			}
			rc := byCat[c.Key]
			if rc == nil {
				rc = &bmgrRestoreCat{Key: c.Key, Label: labels[c.Key]}
				byCat[c.Key] = rc
			}
			rc.Files, rc.FilesBytes = f.Count, f.Bytes
			rc.FilesText = bmgrHuman(f.Bytes)
		}
	}
	for _, c := range bmgrCategories { // screen order
		if rc := byCat[c.Key]; rc != nil {
			v.Cats = append(v.Cats, *rc)
		}
	}
	return v
}
