package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"wpgo/internal/store"
)

// Media upload through the delivery API:
//
//	POST /wp-json/headlessgo/v1/upload   (multipart/form-data)
//	  files     one or more files
//	  fileInfo  optional JSON {name, alternativeText, caption} (or a list, one per file)
//	  ref, refId, field, locale  optional: attach the files to a media field of an entry
//	                             (ref: the type's uid or API id, refId: the document id)
//	GET  /wp-json/headlessgo/v1/upload/files/{id}
//
// It needs an API token allowed to upload (full access, or a custom token with "May upload
// media"); attaching also needs update on the type. Files go through the core media upload path
// (insertAttachmentFile): the same file checks, image sizes and GoCDN offloading as the media
// library. The answer is the list of uploaded files in the API's media format.

const hgMaxUploadFiles = 20

func (a *hgAuth) canUpload() bool {
	return a.Kind == "token" && (a.Token.Kind == "full-access" || a.Token.Upload)
}

// hgUploadTokenMeta records which API token uploaded a file: a custom upload token reads back its
// own uploads, not the whole media library.
const hgUploadTokenMeta = "_hg_upload_token"

func (a *hgAuth) fullAccess() bool {
	return a.Kind == "token" && a.Token != nil && a.Token.Kind == "full-access"
}

// ownUpload reports whether the caller's token uploaded the file.
func (a *hgAuth) ownUpload(att *store.Attachment) bool {
	if a == nil || a.Kind != "token" || a.Token == nil || att == nil {
		return false
	}
	return gooInt(att.Meta[hgUploadTokenMeta]) == a.Token.ID
}

func (s *Server) hgUploadAPI(c *gin.Context, a *hgAuth, segs []string, method string) {
	switch {
	case len(segs) == 0 && method == http.MethodPost:
		s.hgUploadPost(c, a)
	case len(segs) == 2 && segs[0] == "files" && method == http.MethodGet:
		if !a.canUpload() {
			hgForbidden(c, a)
			return
		}
		id, _ := strconv.ParseInt(segs[1], 10, 64)
		att, err := s.store.GetAttachment(c, id)
		// Uploading does not make the whole library readable: the file must be one this caller
		// may read anyway (hgMediaReadable), or its own upload.
		if err != nil || id <= 0 || !s.hgMediaReadable(c, a, s.hgWPUser(c, a), att) {
			hgNotFound(c)
			return
		}
		c.JSON(http.StatusOK, s.hgMediaJSON(c, att))
	case len(segs) == 0 || (len(segs) == 2 && segs[0] == "files"):
		hgErr(c, http.StatusMethodNotAllowed, "MethodNotAllowedError", "Method Not Allowed", nil)
	default:
		hgNotFound(c)
	}
}

type hgFileInfo struct {
	Name            string `json:"name"`
	AlternativeText string `json:"alternativeText"`
	Caption         string `json:"caption"`
}

// hgFileInfos reads fileInfo: one JSON object or list per form value.
func hgFileInfos(vals []string) []hgFileInfo {
	var out []hgFileInfo
	for _, v := range vals {
		v = strings.TrimSpace(v)
		if strings.HasPrefix(v, "[") {
			var list []hgFileInfo
			if json.Unmarshal([]byte(v), &list) == nil {
				out = append(out, list...)
			}
			continue
		}
		var one hgFileInfo
		if json.Unmarshal([]byte(v), &one) == nil {
			out = append(out, one)
		}
	}
	return out
}

func (s *Server) hgUploadPost(c *gin.Context, a *hgAuth) {
	if !a.canUpload() {
		hgForbidden(c, a)
		return
	}
	if !strings.HasPrefix(c.ContentType(), "multipart/form-data") {
		hgErr(c, http.StatusBadRequest, "ValidationError", "The body must be multipart/form-data with the files in \"files\"", nil)
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 4*maxUploadBytes)
	if err := c.Request.ParseMultipartForm(32 << 20); err != nil {
		hgErr(c, http.StatusBadRequest, "ValidationError", "The upload could not be read (too large or malformed)", nil)
		return
	}
	form := c.Request.MultipartForm
	files := append(append(form.File["files"], form.File["files[]"]...), form.File["file"]...)
	if len(files) == 0 {
		hgErr(c, http.StatusBadRequest, "ValidationError", "Files are empty", nil)
		return
	}
	if len(files) > hgMaxUploadFiles {
		hgErr(c, http.StatusBadRequest, "ValidationError", "Too many files in one upload", nil)
		return
	}
	// Attaching: check everything that can be checked before storing the files.
	var sc *hgSchema
	var field *hgField
	var doc, loc string
	if ref := strings.TrimSpace(c.PostForm("ref")); ref != "" {
		reg := s.hgReg(c)
		sc = reg.Get(strings.TrimPrefix(ref, "api::"))
		if sc == nil {
			sc = reg.ByRoute(ref)
		}
		if sc == nil || sc.IsComponent() {
			hgErr(c, http.StatusBadRequest, "ValidationError", "Unknown ref "+ref, nil)
			return
		}
		if !a.can(sc.UID, "update") {
			hgForbidden(c, a)
			return
		}
		field = sc.Field(c.PostForm("field"))
		if field == nil || field.Type != "media" {
			hgErr(c, http.StatusBadRequest, "ValidationError", "The field must be a media field of "+sc.DisplayName, nil)
			return
		}
		if !field.Multiple && len(files) > 1 {
			hgErr(c, http.StatusBadRequest, "ValidationError", "The field "+field.Name+" accepts a single file", nil)
			return
		}
		var err error
		if loc, err = s.hgEntryLocale(c, sc, c.PostForm("locale")); err != nil {
			hgErr(c, http.StatusBadRequest, "ValidationError", err.Error(), nil)
			return
		}
		doc = c.PostForm("refId")
		if sc.IsSingle() {
			doc = s.hgSingleDoc(c, sc.UID)
		}
		if doc == "" {
			hgErr(c, http.StatusBadRequest, "ValidationError", "refId (the document id) is required", nil)
			return
		}
		if _, err := s.hgGetEntry(c, sc.UID, doc, loc); err != nil {
			hgNotFound(c)
			return
		}
	}
	infos := hgFileInfos(form.Value["fileInfo"])
	var saved []*store.Attachment
	rollback := func() bool {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), 2*time.Minute)
		defer cancel()
		failed := false
		for _, att := range saved {
			if err := s.deleteAttachmentRecordAndFiles(ctx, att); err != nil && !errors.Is(err, store.ErrNotFound) {
				failed = true
			}
		}
		if failed {
			hgErr(c, http.StatusInternalServerError, "ApplicationError", "Some uploaded files could not be rolled back.", nil)
		}
		return !failed
	}
	for i, fh := range files {
		f, err := fh.Open()
		if err != nil {
			if !rollback() {
				return
			}
			hgErr(c, http.StatusBadRequest, "ValidationError", "The file could not be read", nil)
			return
		}
		var info hgFileInfo
		if i < len(infos) {
			info = infos[i]
		}
		name := fh.Filename
		att, err := s.insertAttachmentFileWithMeta(c, name, f, 0, 0, map[string]any{hgUploadTokenMeta: a.Token.ID})
		f.Close()
		if err != nil {
			if !rollback() {
				return
			}
			msg := err.Error()
			if errors.Is(err, errUploadTooLarge) {
				hgErr(c, http.StatusRequestEntityTooLarge, "PayloadTooLargeError", msg, nil)
				return
			}
			hgErr(c, http.StatusBadRequest, "ValidationError", fh.Filename+": "+msg, nil)
			return
		}
		saved = append(saved, att)
		if info.Name != "" || info.AlternativeText != "" || info.Caption != "" {
			if info.Name != "" {
				att.Title = info.Name
			}
			att.Alt, att.Caption = info.AlternativeText, info.Caption
			_ = s.store.UpdateAttachment(c, att)
		}
		if field != nil && len(field.Allowed) > 0 && !containsStr(field.Allowed, hgMediaKind(att.Mime)) {
			if !rollback() {
				return
			}
			hgErr(c, http.StatusBadRequest, "ValidationError", "The file type of "+fh.Filename+" is not allowed in "+field.Name, nil)
			return
		}
	}
	if sc != nil {
		e, err := s.hgGetEntry(c, sc.UID, doc, loc)
		if err != nil {
			if !rollback() {
				return
			}
			hgNotFound(c)
			return
		}
		var ids []any
		if field.Multiple {
			ids = append(ids, hgList(e.Data[field.Name])...)
		}
		for _, att := range saved {
			ids = append(ids, float64(att.ID))
		}
		ne, events, err := s.hgSaveEntry(c, sc, doc, map[string]any{field.Name: ids}, hgSaveOpts{Locale: loc})
		if err != nil {
			if !rollback() {
				return
			}
			hgFail(c, err)
			return
		}
		for _, ev := range events {
			s.hgEmit(c, ev, sc, ne)
		}
	}
	out := []any{}
	for _, att := range saved {
		out = append(out, s.hgMediaJSON(c, att))
	}
	c.JSON(http.StatusCreated, out)
}
