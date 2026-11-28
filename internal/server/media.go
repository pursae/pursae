package server

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"wpgo/internal/media"
	"wpgo/internal/store"
	"wpgo/internal/wp"
)

//go:embed defaults/media-l10n.json
var mediaL10nJSON []byte

//go:embed defaults/media-templates.html
var mediaTemplates string

var uploadErrorFlag = regexp.MustCompile(`"[a-z]+_upload_error":true,`)

// imageSizes are the registered sizes: the three from Settings › Media, medium_large and
// the two large sizes WordPress 5.3+ adds.
func (s *Server) imageSizes() []media.Size {
	return []media.Size{
		{Name: "thumbnail", Width: s.settings.Int("thumbnail_size_w"), Height: s.settings.Int("thumbnail_size_h"), Crop: s.settings.Bool("thumbnail_crop")},
		{Name: "medium", Width: s.settings.Int("medium_size_w"), Height: s.settings.Int("medium_size_h")},
		{Name: "medium_large", Width: s.settings.Int("medium_large_size_w"), Height: s.settings.Int("medium_large_size_h")},
		{Name: "large", Width: s.settings.Int("large_size_w"), Height: s.settings.Int("large_size_h")},
		{Name: "1536x1536", Width: 1536, Height: 1536},
		{Name: "2048x2048", Width: 2048, Height: 2048},
	}
}

// imageEditorOptions is image_editor_output_format for an upload: WordPress' default mapping (HEIC to
// JPEG), the image_output_format setting (Settings › Media) and the image_editor_output_format
// filter. "" keeps the default: WebP uploads get JPEG sizes; "webp" keeps WebP uploads in WebP (what
// WordPress does); "webp-all" also writes JPEG and PNG uploads as WebP, keeping the upload as the
// original image.
func (s *Server) imageEditorOptions(ctx context.Context, file, mime string) media.Options {
	m := map[string]any{"image/heic": "image/jpeg", "image/heif": "image/jpeg", "image/heic-sequence": "image/jpeg", "image/heif-sequence": "image/jpeg"}
	switch s.settings.String("image_output_format") {
	case "webp":
		m["image/webp"] = "image/webp"
	case "webp-all":
		m["image/webp"] = "image/webp"
		m["image/jpeg"] = "image/webp"
		m["image/png"] = "image/webp"
	}
	if v, ok := s.applyFilters(ctx, "image_editor_output_format", m, file, mime).(map[string]any); ok {
		m = v
	}
	out := map[string]string{}
	for k, v := range m {
		if to, ok := v.(string); ok {
			out[k] = to
		}
	}
	// Settings › Media "Remove location and camera data": on unless the site owner turned it off
	// (PURSAE_KEEP_EXIF=1 keeps the metadata server-wide, see media.Options.KeepEXIF).
	return media.Options{OutputFormat: out, KeepEXIF: !s.settings.Bool("pursae_strip_exif")}
}

// createImageSubsizes is wp_create_image_subsizes() for a new attachment: its dimensions, the
// "-scaled" or converted main file and the registered sizes.
//
// It returns a warning for the uploader ("" when everything worked): the image could not be
// processed (the error is logged), or its format has no sub-sizes (AVIF, HEIC).
func (s *Server) createImageSubsizes(ctx context.Context, a *store.Attachment) string {
	opts := s.imageEditorOptions(ctx, path.Base(a.FilePath), a.Mime)
	opts.Reserved = s.uploadReserved(ctx)(strings.TrimPrefix(path.Dir(a.FilePath), "."))
	optimize := s.mediaOptimizeOn() && mediaOptimizable(a.FilePath, a.Mime) // Media › Optimization (media_optimize.go)
	if optimize {
		s.optimizeOptions(&opts)
	}
	before := a.FileSize
	info, err := media.ProcessImageWith(s.cfg.UploadsDir, a.FilePath, s.imageSizes(), opts)
	if err != nil {
		slog.Warn("image sizes could not be generated", "file", a.FilePath, "mime", a.Mime, "err", err)
		return imageProcessWarning
	}
	a.Width, a.Height = info.Width, info.Height
	a.FilePath = info.RelPath
	a.FileSize = info.FileSize
	if a.Meta == nil {
		a.Meta = map[string]any{}
	}
	a.Meta["filesize"] = info.FileSize
	if info.OriginalFile != "" {
		a.Meta["original_image"] = info.OriginalFile
		if m := media.MimeFor(info.RelPath); m != "" {
			a.Mime = m // converted (image_editor_output_format)
		}
	}
	dir := path.Dir(info.RelPath)
	for _, v := range info.Variants {
		a.Variants = append(a.Variants, store.AttachmentVariant{Name: v.Name, FilePath: path.Join(dir, v.File),
			Width: v.Width, Height: v.Height, Mime: v.Mime, FileSize: v.Size})
	}
	if optimize {
		s.discardOriginal(a) // keep only the optimized file, when the screen says so
		a.Meta[mediaOptimizedMeta] = optimizedMark(before, info.FileSize, opts.Quality)
	}
	if len(info.Variants) == 0 && noSubsizeFormat(a.Mime) {
		return "No smaller image sizes can be generated for this file format, so the full-size file is used everywhere."
	}
	return ""
}

// imageProcessWarning is WordPress' message for an image the server cannot process.
const imageProcessWarning = "The server cannot process the image. This can happen if the server is busy or does not have enough resources to complete the task. Uploading a smaller image may help."

// noSubsizeFormat reports the formats that are decoded by browsers but not here.
func noSubsizeFormat(mime string) bool {
	switch mime {
	case "image/avif", "image/heic", "image/heif", "image/heic-sequence", "image/heif-sequence":
		return true
	}
	return false
}

// ksesAttachmentText is excerpt_save_pre / content_save_pre for an attachment: a user without
// unfiltered_html can't store raw HTML in its caption or description (both end up in post content
// when the item is inserted into the editor).
func ksesAttachmentText(u *store.User, a *store.Attachment) {
	if u != nil && u.Can("unfiltered_html", nil) {
		return
	}
	a.Caption = wp.KsesPost(a.Caption)
	a.Description = wp.KsesPost(a.Description)
}

// ksesMediaPatch is ksesAttachmentText for the per-language texts of Pursae Multi Language.
func ksesMediaPatch(u *store.User, patch pllMediaText) {
	if u != nil && u.Can("unfiltered_html", nil) {
		return
	}
	for _, k := range []string{"caption", "description"} {
		if v := patch[k]; v != nil {
			t := wp.KsesPost(*v)
			patch[k] = &t
		}
	}
}

// uploadReserved lists the names of an uploads sub-folder ("" or "2026/11") that live only in
// the bucket or are reserved by a pending deletion. A new upload must leave these paths and their
// generated size names alone until their mappings are removed.
func (s *Server) uploadReserved(ctx context.Context) func(sub string) []string {
	return func(sub string) []string {
		prefix := ""
		if sub != "" {
			prefix = strings.Trim(sub, "/") + "/"
		}
		rows, err := s.store.Pool.Query(ctx, `SELECT path FROM gocdn_items WHERE (local_removed OR media_id IS NULL) AND starts_with(path, $1)`, prefix)
		if err != nil {
			return s.pendingMediaFileNames(ctx, prefix)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var p string
			if rows.Scan(&p) == nil {
				if name := strings.TrimPrefix(p, prefix); name != "" && !strings.Contains(name, "/") {
					out = append(out, name)
				}
			}
		}
		return append(out, s.pendingMediaFileNames(ctx, prefix)...)
	}
}

// uploadURL is wp_get_attachment_url() for a path relative to the uploads dir. A file that was
// replaced or re-cropped under the same name carries its version (?v=…, media_replace.go): uploads
// are cached as immutable, so the changed file needs a URL of its own.
func (s *Server) uploadURL(c context.Context, rel string) string {
	if mediaIsPrivateRel(rel) { // a private file has no public URL: a signed link for those who may edit it
		if s.mediaPrivateSigner(c, rel) {
			return s.mediaPrivateAdminURL(c, rel)
		}
		return s.links(c).HomeURL("/wp-content/uploads/" + rel)
	}
	u := s.links(c).HomeURL("/wp-content/uploads/" + rel)
	if v := s.mediaIdx.version(rel); v != "" {
		u += "?v=" + v
	}
	return u
}

// saveUpload stores an uploaded file, generates image sizes and inserts the attachment (media_handle_upload()).
func (s *Server) saveUpload(c *gin.Context, field string, postID int64) (*store.Attachment, error) {
	u := currentUser(c)
	if u == nil {
		u = restCurrent(c) // REST uploads signed in with an Application Password have no cookie user
	}
	if u == nil {
		return nil, errors.New("Sorry, you are not allowed to upload files.")
	}
	fh, err := c.FormFile(field)
	if err != nil {
		return nil, errors.New("No file was uploaded.")
	}
	if fh.Size == 0 {
		return nil, errUploadEmpty
	}
	if fh.Size > maxUploadBytes {
		return nil, fmt.Errorf("The uploaded file exceeds the maximum upload size for this site.")
	}
	f, err := fh.Open()
	if err != nil {
		return nil, err
	}
	defer f.Close()
	name := fh.Filename
	if n := c.PostForm("name"); n != "" {
		name = n
	}
	var st *media.Stored
	download := c.PostForm("type") == "downloadable_product"
	if download {
		// Pursae Commerce's "Choose file" for a downloadable file (WooCommerce's woocommerce_uploads):
		// the file goes straight into the protected folder and never has a public URL (media_private.go).
		sub := mediaPrivateDir
		if s.settings.Bool("uploads_use_yearmonth_folders") {
			sub += time.Now().Format("/2006/01")
		}
		st, err = media.SaveIn(s.cfg.UploadsDir, sub, name, f, s.uploadReserved(c))
	} else {
		st, err = media.SaveWith(s.cfg.UploadsDir, s.settings.Bool("uploads_use_yearmonth_folders"), time.Now(), name, f, s.uploadReserved(c))
	}
	if err != nil {
		return nil, err
	}
	title := strings.TrimSuffix(filepath.Base(st.Original), filepath.Ext(st.Original))
	a := &store.Attachment{UploaderID: u.ID, PostID: postID, FilePath: st.RelPath, Mime: st.Mime, FileSize: st.Size,
		Title: title, Slug: wp.SanitizeTitle(title), Meta: map[string]any{"filesize": st.Size}}
	if download {
		a.Meta[mediaCommerceDownloadMeta] = true // signed links only for store managers (mediaPrivateSigner)
	}
	if strings.HasPrefix(st.Mime, "image/") {
		if w := s.createImageSubsizes(c, a); w != "" {
			c.Set("upload_warning", w)
		}
	} else if strings.HasPrefix(st.Mime, "audio/") || strings.HasPrefix(st.Mime, "video/") {
		s.readAVMeta(s.cfg.UploadsDir, a)
	}
	return s.registerUploadedAttachment(c, a)
}

var (
	errUploadTooLarge = errors.New("the file exceeds the maximum upload size for this site")
	errUploadEmpty    = errors.New("the file is empty")
)

// insertAttachmentFile is the common part of media_handle_sideload(): the file is stored in the
// uploads directory, the image sizes are generated and the attachment is inserted. The importers
// (WXR, theme demo content) use it.
func (s *Server) insertAttachmentFile(ctx context.Context, name string, r io.Reader, uploaderID, postID int64) (*store.Attachment, error) {
	return s.insertAttachmentFileWithMeta(ctx, name, r, uploaderID, postID, nil)
}

func (s *Server) insertAttachmentFileWithMeta(ctx context.Context, name string, r io.Reader, uploaderID, postID int64, meta map[string]any) (*store.Attachment, error) {
	return s.insertAttachmentFileLimit(ctx, name, r, uploaderID, postID, meta, maxUploadBytes)
}

// insertAttachmentFileLimit is insertAttachmentFileWithMeta with its own size limit: the upload limit
// applies to what people upload, a WP to Pursae package brings files the old site accepted (wp2pMaxMedia).
// The file is streamed to disk either way.
func (s *Server) insertAttachmentFileLimit(ctx context.Context, name string, r io.Reader, uploaderID, postID int64, meta map[string]any, limit int64) (*store.Attachment, error) {
	st, err := media.SaveWith(s.cfg.UploadsDir, s.settings.Bool("uploads_use_yearmonth_folders"), time.Now(), name, io.LimitReader(r, limit+1), s.uploadReserved(ctx))
	if err != nil {
		return nil, err
	}
	if st.Size > limit {
		_ = os.Remove(filepath.Join(s.cfg.UploadsDir, filepath.FromSlash(st.RelPath)))
		return nil, errUploadTooLarge
	}
	if st.Size == 0 {
		_ = os.Remove(filepath.Join(s.cfg.UploadsDir, filepath.FromSlash(st.RelPath)))
		return nil, errUploadEmpty
	}
	title := strings.TrimSuffix(filepath.Base(st.Original), filepath.Ext(st.Original))
	if meta == nil {
		meta = make(map[string]any)
	}
	meta["filesize"] = st.Size
	a := &store.Attachment{UploaderID: uploaderID, PostID: postID, FilePath: st.RelPath, Mime: st.Mime, FileSize: st.Size,
		Title: title, Slug: wp.SanitizeTitle(title), Meta: meta}
	if strings.HasPrefix(st.Mime, "image/") {
		s.createImageSubsizes(ctx, a)
	} else if strings.HasPrefix(st.Mime, "audio/") || strings.HasPrefix(st.Mime, "video/") {
		s.readAVMeta(s.cfg.UploadsDir, a)
	}
	return s.registerUploadedAttachment(ctx, a)
}

func (s *Server) registerUploadedAttachment(ctx context.Context, a *store.Attachment) (*store.Attachment, error) {
	if err := s.store.InsertAttachment(ctx, a); err != nil {
		if a.ID != 0 {
			// The connection can fail after COMMIT. Check the row before reclaiming its files.
			check, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			if _, loadErr := s.store.GetAttachment(check, a.ID); !errors.Is(loadErr, store.ErrNotFound) {
				return nil, err
			}
		}
		s.removeUnregisteredUpload(a)
		return nil, err
	}
	s.gocdnOffloadAsync(a)
	return a, nil
}

func (s *Server) removeUnregisteredUpload(a *store.Attachment) {
	root, err := os.OpenRoot(s.cfg.UploadsDir)
	if err != nil {
		slog.Warn("could not open uploads for cleanup", "err", err)
		return
	}
	defer root.Close()
	for _, rel := range gocdnAttachmentFiles(a) {
		if err := root.Remove(filepath.FromSlash(rel)); err != nil && !errors.Is(err, os.ErrNotExist) {
			slog.Warn("could not clean up an unregistered upload", "err", err)
		}
	}
}

// mimeIcon is wp_mime_type_icon() with the SVG icon set.
func mimeIcon(mime string) string {
	t := strings.SplitN(mime, "/", 2)[0]
	name := "default"
	switch {
	case t == "audio":
		name = "audio"
	case t == "video":
		name = "video"
	case t == "text":
		name = "text"
	case strings.Contains(mime, "pdf"), strings.Contains(mime, "word"), strings.Contains(mime, "opendocument.text"), strings.Contains(mime, "rtf"):
		name = "document"
	case strings.Contains(mime, "sheet"), strings.Contains(mime, "excel"), strings.Contains(mime, "csv"):
		name = "spreadsheet"
	case strings.Contains(mime, "zip"), strings.Contains(mime, "tar"), strings.Contains(mime, "rar"), strings.Contains(mime, "7z"), strings.Contains(mime, "gzip"):
		name = "archive"
	case strings.Contains(mime, "powerpoint"), strings.Contains(mime, "presentation"):
		name = "interactive"
	}
	return "/wp-includes/images/media/" + name + ".svg"
}

func orientation(w, h int) string {
	if h > w {
		return "portrait"
	}
	return "landscape"
}

// attachmentJS is wp_prepare_attachment_for_js().
func (s *Server) attachmentJS(c *gin.Context, a *store.Attachment) map[string]any {
	u := currentUser(c)
	parts := strings.SplitN(a.Mime, "/", 2)
	typ, sub := parts[0], ""
	if len(parts) > 1 {
		sub = parts[1]
	}
	url := s.uploadURL(c, a.FilePath)
	id := itoa64(a.ID)
	r := map[string]any{
		"id": a.ID, "title": a.Title, "filename": path.Base(a.FilePath), "url": url,
		"link": s.links(c).HomeURL("/?attachment_id=" + id), "alt": a.Alt, "author": itoa64(a.UploaderID),
		"description": a.Description, "caption": a.Caption, "name": a.Slug, "status": "inherit",
		"uploadedTo": a.PostID, "date": a.CreatedAt.UnixMilli(), "modified": a.UpdatedAt.UnixMilli(),
		"menuOrder": a.MenuOrder, "mime": a.Mime, "type": typ, "subtype": sub, "icon": mimeIcon(a.Mime),
		"dateFormatted": wp.FormatDate(a.CreatedAt.In(s.loc()), "F j, Y"),
		"nonces":        map[string]any{"update": false, "delete": false, "edit": false},
		"editLink":      false, "meta": false, "context": "",
		"compat":     map[string]string{"item": s.golylangMediaCompat(c, a) + s.focalCompatHTML(c, a), "meta": ""}, // Pursae Multi Language fields, focal point
		"focalPoint": focalJSON(a),
	}
	if author, err := s.store.UserByID(c, a.UploaderID); err == nil {
		r["authorName"] = author.DisplayName
		r["authorLink"] = "/wp-admin/user-edit?user_id=" + itoa64(author.ID)
	} else {
		r["authorName"] = "(no author)"
	}
	if a.PostID != 0 {
		if p, err := s.store.GetPost(c, a.PostID); err == nil && u.Can("read_post", p) {
			t := p.Title
			if t == "" {
				t = "(no title)"
			}
			r["uploadedToTitle"] = t
			r["uploadedToLink"] = "/wp-admin/post?post=" + itoa64(p.ID) + "&action=edit"
		}
	}
	if a.FileSize > 0 {
		r["filesizeInBytes"] = a.FileSize
		r["filesizeHumanReadable"] = media.HumanSize(a.FileSize)
	}
	if u.Can("upload_files", nil) && (a.UploaderID == u.ID || u.Can("edit_others_posts", nil)) {
		r["nonces"] = map[string]any{"update": s.nonce(c, "update-post_"+id), "edit": false, "delete": s.nonce(c, "delete-post_"+id)}
		if editableImage(a.Mime) { // like wp_image_editor_supports(): no Edit Image for SVG, AVIF, HEIC, WebP…
			r["nonces"].(map[string]any)["edit"] = s.nonce(c, "image_editor-"+id)
		}
		r["editLink"] = "/wp-admin/post?post=" + id + "&action=edit"
		r["replaceLink"] = "/wp-admin/post?post=" + id + "&action=edit#wpgo-replace-media" // media_replace_admin.go
	}
	if typ == "image" {
		sizes := map[string]any{}
		dir := path.Dir(a.FilePath)
		_ = dir
		for _, name := range []string{"thumbnail", "medium", "large"} {
			if v := a.Variant(name); v != nil {
				sizes[name] = map[string]any{"height": v.Height, "width": v.Width, "url": s.uploadURL(c, v.FilePath), "orientation": orientation(v.Width, v.Height)}
			}
		}
		full := map[string]any{"url": url}
		if a.Width > 0 {
			full["height"], full["width"], full["orientation"] = a.Height, a.Width, orientation(a.Width, a.Height)
			r["height"], r["width"], r["orientation"] = a.Height, a.Width, orientation(a.Width, a.Height)
		}
		sizes["full"] = full
		r["sizes"] = sizes
		if orig, ok := a.Meta["original_image"].(string); ok && orig != "" {
			r["originalImageURL"] = s.uploadURL(c, path.Join(path.Dir(a.FilePath), orig))
			r["originalImageName"] = orig
		}
	}
	if typ == "audio" || typ == "video" {
		icon := mimeIcon(a.Mime)
		r["meta"] = avModalMeta(a)
		if typ == "video" {
			if w, h := metaInt(a.Meta["width"]), metaInt(a.Meta["height"]); w > 0 && h > 0 {
				r["width"], r["height"] = w, h
			}
		}
		if l := metaInt(a.Meta["length"]); l > 0 {
			r["fileLength"], r["fileLengthHumanReadable"] = mediaLengthFormatted(l), mediaLengthHuman(l)
		}
		r["image"] = map[string]any{"src": icon, "width": 48, "height": 64}
		r["thumb"] = map[string]any{"src": icon, "width": 48, "height": 64}
	}
	return r
}

func jsonSuccess(c *gin.Context, data any) {
	c.JSON(http.StatusOK, gin.H{"success": true, "data": data})
}

func jsonError(c *gin.Context, status int, msg any) {
	c.JSON(status, gin.H{"success": false, "data": msg})
}

// asyncUpload is async-upload.php: the plupload endpoint for the media modal (action=upload-attachment)
// and for the Add Media File screen (short=1 returns the id; fetch=3 returns the item markup).
func (s *Server) asyncUpload(c *gin.Context) {
	u := currentUser(c)
	if c.Request.Method == http.MethodGet || c.PostForm("fetch") != "" {
		if !u.Can("upload_files", nil) { // async-upload.php: upload_files, whatever the request
			c.String(http.StatusForbidden, "Sorry, you are not allowed to upload files.")
			return
		}
		id, _ := strconv.ParseInt(c.DefaultQuery("attachment_id", c.PostForm("attachment_id")), 10, 64)
		a, err := s.store.GetAttachment(c, id)
		if err != nil {
			c.String(http.StatusNotFound, "")
			return
		}
		thumb := mimeIcon(a.Mime)
		if v := a.Variant("thumbnail"); v != nil {
			thumb = s.uploadURL(c, v.FilePath)
		} else if strings.HasPrefix(a.Mime, "image/") {
			thumb = s.uploadURL(c, a.FilePath)
		}
		fileURL := s.uploadURL(c, a.FilePath)
		s.adminHTML(c, http.StatusOK, fmt.Sprintf(`<img class="pinkynail" src="%s" alt="" /><a class="edit-attachment" href="/wp-admin/post?post=%d&amp;action=edit" target="_blank">Edit</a><div class="filename new"><span class="media-list-title"><strong>%s</strong></span> <span class="media-list-subtitle">%s</span><div class="attachment-tools"><span class="media-item-copy-container copy-to-clipboard-container edit-attachment"><button type="button" class="button button-small copy-attachment-url" data-clipboard-text="%s">Copy URL to clipboard</button><span class="success hidden" aria-hidden="true">Copied!</span></span></div></div>`,
			html.EscapeString(thumb), a.ID, html.EscapeString(a.Title), html.EscapeString(path.Base(a.FilePath)), html.EscapeString(fileURL)))
		return
	}
	if !u.Can("upload_files", nil) {
		if c.PostForm("action") == "upload-attachment" {
			c.JSON(http.StatusOK, gin.H{"success": false, "data": gin.H{"message": "Sorry, you are not allowed to upload files.", "filename": ""}})
			return
		}
		c.String(http.StatusForbidden, "Sorry, you are not allowed to upload files.")
		return
	}
	postID, _ := strconv.ParseInt(c.PostForm("post_id"), 10, 64)
	// The post an upload is attached to must be one the user may edit (wp_ajax_upload_attachment(),
	// async-upload.php): otherwise anyone who uploads could add files to another user's gallery.
	attachOK := true
	if postID > 0 {
		p, err := s.store.GetPost(c, postID)
		attachOK = err == nil && u.Can("edit_post", p)
	} else if postID < 0 {
		postID = 0
	}
	if c.PostForm("action") == "upload-attachment" {
		if !s.checkNonce(c, "media-form") {
			c.JSON(http.StatusOK, gin.H{"success": false, "data": gin.H{"message": "The link you followed has expired.", "filename": ""}})
			return
		}
		if !attachOK {
			c.JSON(http.StatusOK, gin.H{"success": false, "data": gin.H{"message": "Sorry, you are not allowed to attach files to this post.", "filename": ""}})
			return
		}
		a, err := s.saveUpload(c, "async-upload", postID)
		if err != nil {
			fh, _ := c.FormFile("async-upload")
			name := ""
			if fh != nil {
				name = fh.Filename
			}
			c.JSON(http.StatusOK, gin.H{"success": false, "data": gin.H{"message": err.Error(), "filename": name}})
			return
		}
		js := s.attachmentJS(c, a)
		if w, ok := c.Get("upload_warning"); ok {
			js["uploadWarning"] = w
		}
		jsonSuccess(c, js)
		return
	}
	if !s.checkNonce(c, "media-form") {
		c.String(http.StatusForbidden, "The link you followed has expired.")
		return
	}
	if !attachOK {
		postID = 0 // async-upload.php: the file is uploaded unattached
	}
	a, err := s.saveUpload(c, "async-upload", postID)
	if err != nil {
		c.String(http.StatusOK, `<div class="error-div error"><button type="button" class="dismiss button-link" data-action="remove" data-target="div.media-item">Dismiss</button><strong>`+html.EscapeString(err.Error())+`</strong></div>`)
		return
	}
	c.String(http.StatusOK, itoa64(a.ID))
}

// mediaAjax handles the admin-ajax actions used by wp.media and the editors.
func (s *Server) mediaAjax(c *gin.Context, action string) bool {
	u := currentUser(c)
	switch action {
	case "regenerate-progress": // the Media Library's "Regenerate all" progress bar
		if !u.Can("manage_options", nil) {
			jsonError(c, http.StatusForbidden, nil)
			return true
		}
		j := s.regenerateJobState()
		c.JSON(http.StatusOK, gin.H{"running": j.Running && regenerateRunning.Load(), "finished": j.Finished, "stopped": j.Stopped,
			"total": j.Total, "done": j.Done, "skipped": j.Skipped, "failed": j.Failed, "percent": j.Percent()})
		return true
	case "query-attachments":
		if !u.Can("upload_files", nil) {
			jsonError(c, http.StatusOK, nil)
			return true
		}
		q := store.AttachmentQuery{OrderBy: c.DefaultPostForm("query[orderby]", "date"), Order: c.DefaultPostForm("query[order]", "DESC")}
		q.PerPage, _ = strconv.Atoi(c.DefaultPostForm("query[posts_per_page]", "40"))
		if q.PerPage <= 0 || q.PerPage > 200 {
			q.PerPage = 40
		}
		q.Page, _ = strconv.Atoi(c.DefaultPostForm("query[paged]", "1"))
		q.Search = c.PostForm("query[s]")
		if t := c.PostForm("query[post_mime_type]"); t != "" {
			q.MimePrefix = strings.Split(t, ",")
		}
		for _, t := range c.PostFormArray("query[post_mime_type][]") {
			q.MimePrefix = append(q.MimePrefix, t)
		}
		q.Year, _ = strconv.Atoi(c.PostForm("query[year]"))
		q.Month, _ = strconv.Atoi(c.PostForm("query[monthnum]"))
		if p := c.PostForm("query[post_parent]"); p != "" {
			id, _ := strconv.ParseInt(p, 10, 64)
			q.PostID = &id
		}
		if c.PostForm("query[author]") != "" {
			q.AuthorID, _ = strconv.ParseInt(c.PostForm("query[author]"), 10, 64)
		}
		for _, raw := range c.PostFormArray("query[post__in][]") {
			if id, err := strconv.ParseInt(raw, 10, 64); err == nil {
				q.IDs = append(q.IDs, id)
			}
		}
		for _, raw := range c.PostFormArray("query[post__not_in][]") {
			if id, err := strconv.ParseInt(raw, 10, 64); err == nil {
				q.Exclude = append(q.Exclude, id)
			}
		}
		list, total, err := s.store.QueryAttachments(c, q)
		if err != nil {
			jsonError(c, http.StatusOK, nil)
			return true
		}
		s.pllMediaPrefetch(c, list) // the compat fields' translations in one query
		out := make([]map[string]any, 0, len(list))
		for _, a := range list {
			out = append(out, s.attachmentJS(c, a))
		}
		c.Header("X-WP-Total", strconv.Itoa(total))
		pages := (total + q.PerPage - 1) / q.PerPage
		c.Header("X-WP-TotalPages", strconv.Itoa(pages))
		jsonSuccess(c, out)
	case "get-attachment":
		id, _ := strconv.ParseInt(c.PostForm("id"), 10, 64)
		a, err := s.store.GetAttachment(c, id)
		if err != nil || !u.Can("upload_files", nil) {
			jsonError(c, http.StatusOK, nil)
			return true
		}
		jsonSuccess(c, s.attachmentJS(c, a))
	case "save-attachment", "save-attachment-compat":
		id, _ := strconv.ParseInt(c.PostForm("id"), 10, 64)
		a, err := s.store.GetAttachment(c, id)
		if err != nil {
			jsonError(c, http.StatusOK, nil)
			return true
		}
		if !s.checkNonceValue(c, c.PostForm("nonce"), "update-post_"+itoa64(id)) || !(a.UploaderID == u.ID || u.Can("edit_others_posts", nil)) {
			jsonError(c, http.StatusOK, nil)
			return true
		}
		for key, field := range map[string]*string{"changes[title]": &a.Title, "changes[caption]": &a.Caption,
			"changes[description]": &a.Description, "changes[alt]": &a.Alt} {
			if v, ok := c.GetPostForm(key); ok {
				*field = v
			}
		}
		if v, ok := c.GetPostForm("changes[parent]"); ok {
			parent, _ := strconv.ParseInt(v, 10, 64)
			parent = max(parent, 0)
			if !s.mayAttachTo(c, u, a.PostID, parent) {
				jsonError(c, http.StatusOK, nil)
				return true
			}
			a.PostID = parent
		}
		if v, ok := c.GetPostForm("changes[menuOrder]"); ok {
			a.MenuOrder, _ = strconv.Atoi(v)
		}
		ksesAttachmentText(u, a)
		if err := s.store.UpdateAttachment(c, a); err != nil {
			jsonError(c, http.StatusOK, nil)
			return true
		}
		s.purgeAttachment(a)
		if action == "save-attachment-compat" {
			s.golylangMediaCompatSave(c, a)                 // attachments[ID][pll_i18n][lang][field]
			if err := s.focalCompatSave(c, a); err != nil { // attachments[ID][focal_point]
				jsonError(c, http.StatusOK, nil)
				return true
			}
			jsonSuccess(c, s.attachmentJS(c, a))
		} else {
			jsonSuccess(c, nil)
		}
	case "save-attachment-order":
		jsonSuccess(c, nil)
	case "delete-post":
		id, _ := strconv.ParseInt(c.DefaultPostForm("id", c.Query("id")), 10, 64)
		if !s.checkNonce(c, "delete-post_"+itoa64(id)) {
			c.String(http.StatusOK, "-1")
			return true
		}
		if a, err := s.store.GetAttachment(c, id); err == nil {
			if a.UploaderID != u.ID && !u.Can("delete_others_posts", nil) {
				c.String(http.StatusOK, "-1")
				return true
			}
			if err := s.deleteAttachmentRecordAndFiles(c, a); err != nil {
				c.String(http.StatusOK, "0")
				return true
			}
			s.purgeAttachment(a)
			c.String(http.StatusOK, "1")
			return true
		}
		c.String(http.StatusOK, "0")
	case "send-attachment-to-editor":
		// wp_ajax_send_attachment_to_editor(): the media modal's nonce (settings.nonce.sendToEditor),
		// and only for someone who browses the library: the markup names the file's path.
		id, _ := strconv.ParseInt(c.PostForm("attachment[id]"), 10, 64)
		if !s.checkNonceValue(c, c.PostForm("nonce"), "media-send-to-editor") || !u.Can("upload_files", nil) {
			jsonError(c, http.StatusOK, nil)
			return true
		}
		a, err := s.store.GetAttachment(c, id)
		if err != nil {
			jsonError(c, http.StatusOK, nil)
			return true
		}
		jsonSuccess(c, s.attachmentEditorHTML(c, a))
	case "set-post-thumbnail", "get-post-thumbnail-html":
		postID, _ := strconv.ParseInt(c.PostForm("post_id"), 10, 64)
		thumbID, _ := strconv.ParseInt(c.PostForm("thumbnail_id"), 10, 64)
		p, err := s.store.GetPost(c, postID)
		if err != nil || !genericEditType(p.Type) || !u.Can("edit_post", p) {
			c.String(http.StatusOK, "-1")
			return true
		}
		if action == "set-post-thumbnail" {
			if !s.checkNonce(c, "update-post_"+itoa64(postID)) && !s.checkNonceValue(c, c.PostForm("json"), "") {
				// WordPress sends _ajax_nonce = set_post_thumbnail-{post_id}.
				if !s.checkNonceValue(c, c.PostForm("_ajax_nonce"), "set_post_thumbnail-"+itoa64(postID)) {
					c.String(http.StatusOK, "-1")
					return true
				}
			}
			if thumbID < 0 {
				thumbID = 0
			}
			p.FeaturedMediaID = thumbID
			if _, err := s.store.Pool.Exec(c, `UPDATE posts SET featured_media_id = $2 WHERE id = $1`, p.ID, nullable64(thumbID)); err != nil {
				c.String(http.StatusOK, "0")
				return true
			}
		}
		jsonSuccess(c, string(s.postThumbnailHTML(c, p, thumbID)))
	default:
		return false
	}
	return true
}

// mayAttachTo reports whether u may make parent the post an attachment belongs to (the edit_post
// check WordPress makes on the post an upload goes to): keeping the parent and detaching are
// always allowed, any other post must exist and be one u may edit, so nobody adds files to the
// gallery or attachments of another user's post.
func (s *Server) mayAttachTo(ctx context.Context, u *store.User, current, parent int64) bool {
	if parent == current || parent <= 0 {
		return true
	}
	p, err := s.store.GetPost(ctx, parent)
	return err == nil && u.Can("edit_post", p)
}

func nullable64(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

// checkNonceValue verifies an explicit nonce value (ajax handlers pass nonces under various names).
func (s *Server) checkNonceValue(c *gin.Context, nonce, action string) bool {
	if nonce == "" || action == "" {
		return false
	}
	var uid int64
	var token string
	if u := currentUser(c); u != nil {
		uid = u.ID
	}
	if sess := currentSession(c); sess != nil {
		token = sess.Token
	}
	return s.auth.VerifyNonce(nonce, action, uid, token) > 0
}

// deleteAttachmentFiles is wp_delete_attachment_files(): the main file, its sizes, the original of a
// "-scaled"/"-rotated" image and the image editor's backup (the pre-edit file and its sizes).
func (s *Server) deleteAttachmentFiles(a *store.Attachment) {
	s.removeAttachmentFiles(a, func() { s.gocdnBeforeDelete(a) })
}

// deleteAttachmentRecordAndFiles preserves the files when the database rejects the deletion.
func (s *Server) deleteAttachmentRecordAndFiles(ctx context.Context, a *store.Attachment) error {
	items, err := s.detachOffloadsAndDeleteAttachment(ctx, a.ID)
	if err != nil {
		return err
	}
	s.mediaIdx.put(a.ID, nil)
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()
	s.gocdnDeleteItems(cleanup, items)
	s.gocdnDropVariants(gocdnAttachmentFiles(a))
	s.cleanupMediaFileDeletes(cleanup, a.ID)
	return nil
}

func (s *Server) removeAttachmentFiles(a *store.Attachment, removeOffload func()) {
	s.mediaIdx.put(a.ID, nil) // its versions, focal point and moved paths (media_replace.go)
	removeOffload()
	paths := gocdnAttachmentFiles(a)
	s.gocdnDropVariants(paths)
	for _, rel := range paths {
		if p := s.gocdnLocalPath(rel); p != "" {
			_ = os.Remove(p)
		}
	}
}

// attachmentEditorHTML is wp_ajax_send_attachment_to_editor(): image markup for images, a link otherwise.
func (s *Server) attachmentEditorHTML(c *gin.Context, a *store.Attachment) string {
	size := c.DefaultPostForm("attachment[image-size]", "full")
	align := c.DefaultPostForm("attachment[align]", "none")
	link := c.PostForm("attachment[url]")
	alt := c.DefaultPostForm("attachment[image_alt]", a.Alt)
	caption := c.PostForm("attachment[post_excerpt]")
	title := c.DefaultPostForm("attachment[post_title]", a.Title)
	if !strings.HasPrefix(a.Mime, "image/") {
		url := s.uploadURL(c, a.FilePath)
		if link == "" {
			link = url
		}
		return fmt.Sprintf(`<a href="%s">%s</a>`, html.EscapeString(link), html.EscapeString(title))
	}
	src, w, h := s.uploadURL(c, a.FilePath), a.Width, a.Height
	if v := a.Variant(size); v != nil {
		src, w, h = s.uploadURL(c, v.FilePath), v.Width, v.Height
	}
	img := fmt.Sprintf(`<img class="wp-image-%d size-%s align%s" src="%s" alt="%s" width="%d" height="%d" />`,
		a.ID, size, align, html.EscapeString(src), html.EscapeString(alt), w, h)
	if caption != "" {
		img = fmt.Sprintf(`<img class="wp-image-%d size-%s" src="%s" alt="%s" width="%d" height="%d" />`, a.ID, size, html.EscapeString(src), html.EscapeString(alt), w, h)
	}
	if link != "" {
		img = `<a href="` + html.EscapeString(link) + `">` + img + `</a>`
	}
	if caption != "" {
		img = fmt.Sprintf(`[caption id="attachment_%d" align="align%s" width="%d"]%s %s[/caption]`, a.ID, align, w, img, caption)
	}
	return img
}

// postThumbnailHTML is _wp_post_thumbnail_html(): the Featured image meta box contents.
func (s *Server) postThumbnailHTML(c *gin.Context, p *store.Post, thumbID int64) template.HTML {
	setLabel := "Set featured image"
	if thumbID > 0 {
		if a, err := s.store.GetAttachment(c, thumbID); err == nil {
			src, w, h := s.uploadURL(c, a.FilePath), a.Width, a.Height
			for _, n := range []string{"medium", "thumbnail"} {
				if v := a.Variant(n); v != nil {
					src, w, h = s.uploadURL(c, v.FilePath), v.Width, v.Height
					break
				}
			}
			if w > 266 {
				h = h * 266 / w
				w = 266
			}
			return template.HTML(fmt.Sprintf(`<p class="hide-if-no-js"><a href="/wp-admin/media-upload?post_id=%d&amp;type=image&amp;TB_iframe=1" id="set-post-thumbnail" aria-describedby="set-post-thumbnail-desc" class="thickbox"><img width="%d" height="%d" src="%s" class="attachment-266x266 size-266x266" alt="%s" decoding="async" loading="lazy" /></a></p><p class="hide-if-no-js howto" id="set-post-thumbnail-desc">Click the image to edit or update</p><p class="hide-if-no-js"><a href="#" id="remove-post-thumbnail">Remove featured image</a></p><input type="hidden" id="_thumbnail_id" name="_thumbnail_id" value="%d" />`,
				p.ID, w, h, html.EscapeString(src), html.EscapeString(a.Alt), a.ID))
		}
	}
	return template.HTML(fmt.Sprintf(`<p class="hide-if-no-js"><a href="/wp-admin/media-upload?post_id=%d&amp;type=image&amp;TB_iframe=1" id="set-post-thumbnail" class="thickbox">%s</a></p><input type="hidden" id="_thumbnail_id" name="_thumbnail_id" value="-1" />`, p.ID, setLabel))
}

// enqueueMedia is wp_enqueue_media(): the media modal scripts, styles, settings and templates.
func (s *Server) enqueueMedia(c *gin.Context, postID int64) {
	if _, done := c.Get("wpgo.mediaEnqueued"); done {
		return
	}
	c.Set("wpgo.mediaEnqueued", true)
	var l10n struct {
		Views    map[string]any `json:"views"`
		Plupload string         `json:"plupload"`
	}
	_ = json.Unmarshal(mediaL10nJSON, &l10n)
	settings, _ := l10n.Views["settings"].(map[string]any)
	if settings == nil {
		settings = map[string]any{}
	}
	settings["nonce"] = map[string]string{"sendToEditor": s.nonce(c, "media-send-to-editor"), "setAttachmentThumbnail": s.nonce(c, "set-attachment-thumbnail")}
	settings["post"] = map[string]any{"id": postID}
	if postID > 0 {
		if p, err := s.store.GetPost(c, postID); err == nil {
			settings["post"] = map[string]any{"id": postID, "nonce": s.nonce(c, "update-post_"+itoa64(postID)), "featuredImageId": func() int64 {
				if p.FeaturedMediaID == 0 {
					return -1
				}
				return p.FeaturedMediaID
			}()}
		}
	}
	settings["mimeTypes"] = map[string]string{"image": "Images", "audio": "Audio", "video": "Video",
		"application/msword,application/vnd.openxmlformats-officedocument.wordprocessingml.document,application/vnd.ms-word.document.macroEnabled.12,application/vnd.oasis.opendocument.text,application/pdf,application/rtf": "Documents",
		"application/vnd.ms-excel,application/vnd.openxmlformats-officedocument.spreadsheetml.sheet,application/vnd.oasis.opendocument.spreadsheet,text/csv":                                                                  "Spreadsheets",
		"application/x-gzip,application/rar,application/x-tar,application/zip,application/x-7z-compressed":                                                                                                                    "Archives"}
	months, _ := s.store.AttachmentMonths(c)
	var ms []map[string]any
	for _, m := range months {
		ms = append(ms, map[string]any{"year": strconv.Itoa(m.Year), "month": strconv.Itoa(m.Month), "text": time.Month(m.Month).String() + " " + strconv.Itoa(m.Year)})
	}
	if ms == nil {
		ms = []map[string]any{}
	}
	settings["months"] = ms
	settings["defaultProps"] = map[string]any{"link": "none", "align": "", "size": ""}
	l10n.Views["settings"] = settings
	// wp_enqueue_media() adds these only when the post type supports thumbnails; without them the
	// featured-image frame's button has no label.
	if postID > 0 {
		l10n.Views["setFeaturedImageTitle"] = "Featured image"
		l10n.Views["setFeaturedImage"] = "Set featured image"
	}
	// wp_enqueue_media() takes these from the post type's insert_into_item / uploaded_to_this_item
	// labels; without them the Add Media frame's primary button is blank.
	l10n.Views["insertIntoPost"], l10n.Views["uploadedToThisPost"] = "Insert into post", "Uploaded to this post"
	if postID > 0 {
		if p, err := s.store.GetPost(c, postID); err == nil && p.Type == "page" {
			l10n.Views["insertIntoPost"], l10n.Views["uploadedToThisPost"] = "Insert into page", "Uploaded to this page"
		}
	}
	views, _ := json.Marshal(l10n.Views)

	a := s.pageAssets(c)
	a.Script("media-editor", "media-audiovideo", "mce-view", "image-edit").Style("media-views", "imgareaselect")
	a.AddBefore("media-views", "var _wpMediaViewsL10n = "+string(views)+";")
	// The details sidebar only tracks upload progress; redraw it once the upload finishes so the
	// preview, file URL and fields appear without reselecting the item.
	a.AddInline("media-views", `( function( Details ) {
	var init = Details.prototype.initialize;
	Details.prototype.initialize = function() {
		init.apply( this, arguments );
		this.listenTo( this.model, 'change:uploading', function( model, uploading ) {
			if ( ! uploading ) {
				this.render();
			}
		} );
	};
} )( wp.media.view.Attachment.Details );`)
	s.gocdnEnqueueMedia(c, a) // GoCDN: "Remote storage" tab in the media modal (gocdn_modal.go)
	plupload := strings.ReplaceAll(l10n.Plupload, `"url":"/wp-admin/async-upload.php"`, `"url":"/wp-admin/async-upload"`)
	// Go stores WebP/AVIF/HEIC files as they are, so the "editor can't process this type" guards are dropped.
	plupload = uploadErrorFlag.ReplaceAllString(plupload, "")
	a.AddBefore("wp-plupload", plupload)
	addBodyHTML(c, template.HTML(mediaTemplates))
	addBodyHTML(c, template.HTML(focalAssets)) // the Focal point field of the details sidebar (media_replace_admin.go)
}

// editableImage reports whether the image editor can open and save a file of this type.
func editableImage(mime string) bool {
	return mime == "image/jpeg" || mime == "image/png" || mime == "image/gif"
}

// readAVMeta stores wp_read_audio_metadata() / wp_read_video_metadata() values in the attachment's
// metadata: length, length_formatted, bitrate, the video size and the ID3 artist and album.
func (s *Server) readAVMeta(uploadsDir string, a *store.Attachment) {
	i := media.ReadAV(uploadsDir, a.FilePath)
	if a.Meta == nil {
		a.Meta = map[string]any{}
	}
	if i.Length > 0 {
		a.Meta["length"] = i.Length
		a.Meta["length_formatted"] = mediaLengthFormatted(i.Length)
	}
	if i.Bitrate > 0 {
		a.Meta["bitrate"] = i.Bitrate
		a.Meta["bitrate_mode"] = i.BitrateMode
	}
	if i.Width > 0 && i.Height > 0 {
		a.Meta["width"], a.Meta["height"] = i.Width, i.Height
		a.Width, a.Height = i.Width, i.Height
	}
	if i.Format != "" {
		a.Meta["fileformat"] = i.Format
	}
	for k, v := range map[string]string{"artist": i.Artist, "album": i.Album, "genre": i.Genre} {
		if v != "" {
			a.Meta[k] = v
		}
	}
	if a.Title != "" && i.Title != "" && a.Meta["title"] == nil {
		a.Meta["title"] = i.Title
	}
}

// avModalMeta is the "meta" of an audio/video item in the media modal.
func avModalMeta(a *store.Attachment) map[string]any {
	m := map[string]any{"artist": false, "album": false, "bitrate": false, "bitrate_mode": false}
	for _, k := range []string{"artist", "album", "bitrate", "bitrate_mode"} {
		if v, ok := a.Meta[k]; ok && v != "" && v != nil {
			m[k] = v
		}
	}
	return m
}

func metaInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return 0
}

// mediaLengthFormatted is wp_get_attachment_metadata()'s length_formatted: "m:ss" or "h:mm:ss".
func mediaLengthFormatted(sec int) string {
	if sec >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", sec/3600, sec%3600/60, sec%60)
	}
	return fmt.Sprintf("%d:%02d", sec/60, sec%60)
}

// mediaLengthHuman is human_readable_duration(): "1 minute, 5 seconds" style.
func mediaLengthHuman(sec int) string {
	return mediaLengthFormatted(sec)
}
