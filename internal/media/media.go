// Package media stores uploads the way WordPress does: year/month folders, unique
// file names, the registered image sizes and the "-scaled" big image handling.
package media

import (
	"cmp"
	"errors"
	"fmt"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/gen2brain/webp"
	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"

	"wpgo/internal/wp"
)

// MimeTypes mirrors wp_get_mime_types() for the types allowed to regular users
// (get_allowed_mime_types() without unfiltered_upload).
var MimeTypes = map[string]string{
	"jpg|jpeg|jpe": "image/jpeg", "gif": "image/gif", "png": "image/png", "bmp": "image/bmp", "tiff|tif": "image/tiff",
	"webp": "image/webp", "avif": "image/avif", "ico": "image/x-icon", "heic": "image/heic", "heif": "image/heif",
	"svg":     "image/svg+xml", // sanitized on upload (formats.go)
	"asf|asx": "video/x-ms-asf", "wmv": "video/x-ms-wmv", "wmx": "video/x-ms-wmx", "wm": "video/x-ms-wm", "avi": "video/avi",
	"divx": "video/divx", "flv": "video/x-flv", "mov|qt": "video/quicktime", "mpeg|mpg|mpe": "video/mpeg", "mp4|m4v": "video/mp4",
	"ogv": "video/ogg", "webm": "video/webm", "mkv": "video/x-matroska", "3gp|3gpp": "video/3gpp", "3g2|3gp2": "video/3gpp2",
	"txt|asc|c|cc|h|srt": "text/plain", "csv": "text/csv", "tsv": "text/tab-separated-values", "ics": "text/calendar",
	"rtx": "text/richtext", "css": "text/css", "vtt": "text/vtt", "dfxp": "application/ttaf+xml",
	"mp3|m4a|m4b": "audio/mpeg", "aac": "audio/aac", "ra|ram": "audio/x-realaudio", "wav|x-wav": "audio/wav", "ogg|oga": "audio/ogg",
	"flac": "audio/flac", "mid|midi": "audio/midi", "wma": "audio/x-ms-wma", "wax": "audio/x-ms-wax", "mka": "audio/x-matroska",
	"rtf": "application/rtf", "pdf": "application/pdf", "class": "application/java", "tar": "application/x-tar",
	"zip": "application/zip", "gz|gzip": "application/x-gzip", "rar": "application/rar", "7z": "application/x-7z-compressed",
	"psd": "application/octet-stream", "xcf": "application/octet-stream",
	"doc": "application/msword", "pot|pps|ppt": "application/vnd.ms-powerpoint", "wri": "application/vnd.ms-write",
	"xla|xls|xlt|xlw": "application/vnd.ms-excel", "mdb": "application/vnd.ms-access", "mpp": "application/vnd.ms-project",
	"docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	"docm": "application/vnd.ms-word.document.macroEnabled.12",
	"dotx": "application/vnd.openxmlformats-officedocument.wordprocessingml.template",
	"xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	"pptx": "application/vnd.openxmlformats-officedocument.presentationml.presentation",
	"odt":  "application/vnd.oasis.opendocument.text", "odp": "application/vnd.oasis.opendocument.presentation",
	"ods": "application/vnd.oasis.opendocument.spreadsheet", "odg": "application/vnd.oasis.opendocument.graphics",
	"key": "application/vnd.apple.keynote", "numbers": "application/vnd.apple.numbers", "pages": "application/vnd.apple.pages",
}

// MimeFor returns the allowed MIME type for a file name, or "" when the extension is not allowed.
func MimeFor(name string) string {
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(name), "."))
	if ext == "" {
		return ""
	}
	for exts, mime := range MimeTypes {
		for _, e := range strings.Split(exts, "|") {
			if e == ext {
				return mime
			}
		}
	}
	return ""
}

var ErrType = errors.New("Sorry, you are not allowed to upload this file type.")

// SanitizeFileName is sanitize_file_name(): lowercase-safe characters, dashes, one extension.
func SanitizeFileName(name string) string {
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	ext := strings.ToLower(filepath.Ext(name))
	base := strings.TrimSuffix(name, filepath.Ext(name))
	base = wp.RemoveAccents(base)
	var sb strings.Builder
	for _, r := range base {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			sb.WriteRune(r)
		case unicode.IsSpace(r) || r == '+' || r == '%':
			sb.WriteRune('-')
		}
	}
	base = sb.String()
	for strings.Contains(base, "--") {
		base = strings.ReplaceAll(base, "--", "-")
	}
	base = strings.Trim(base, "-._")
	if base == "" {
		base = "unnamed-file"
	}
	return base + ext
}

// Stored describes a saved upload.
type Stored struct {
	RelPath  string // e.g. 2026/10/photo.jpg, relative to the uploads dir
	Size     int64
	Mime     string
	Original string // name before sanitizing, used for the title
}

// Save writes r into the uploads directory under year/month (when dated) with a unique name.
func Save(uploadsDir string, dated bool, now time.Time, filename string, r io.Reader) (*Stored, error) {
	return SaveWith(uploadsDir, dated, now, filename, r, nil)
}

// SaveWith is Save with reserved: the file names (no folder) that count as taken in the target
// sub-folder ("" or "2026/11") besides the files on disk, e.g. offloaded files whose local copy
// was removed. A new upload must not take their names: it would overwrite their bucket objects.
func SaveWith(uploadsDir string, dated bool, now time.Time, filename string, r io.Reader, reserved func(sub string) []string) (*Stored, error) {
	sub := ""
	if dated {
		sub = fmt.Sprintf("%04d/%02d", now.Year(), int(now.Month()))
	}
	return SaveIn(uploadsDir, sub, filename, r, reserved)
}

// SaveIn is SaveWith into a given sub-folder of the uploads directory ("" or "2026/11"), e.g. the
// folder of an attachment whose file is being replaced.
func SaveIn(uploadsDir, sub, filename string, r io.Reader, reserved func(sub string) []string) (*Stored, error) {
	mime := MimeFor(filename)
	if mime == "" {
		return nil, ErrType
	}
	sub = strings.Trim(filepath.ToSlash(sub), "/")
	if sub == "." {
		sub = ""
	}
	root := filepath.Clean(uploadsDir)
	dir := filepath.Join(root, filepath.FromSlash(sub))
	if !pathWithin(root, dir) {
		return nil, ErrType
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	var taken []string
	if reserved != nil {
		taken = reserved(sub)
	}
	// uniqueName only checks; O_EXCL claims the name. A parallel upload of the same file name can
	// claim it in between (the editor uploads several files at once, pasted images are all
	// "image.png"): pick the next free name then instead of failing.
	var (
		name, dst string
		f         *os.File
		err       error
	)
	for try := 0; ; try++ {
		name = uniqueName(dir, SanitizeFileName(filename), taken...)
		dst = filepath.Join(dir, name)
		f, err = os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err == nil {
			break
		}
		if !errors.Is(err, fs.ErrExist) || try >= 20 {
			return nil, err
		}
	}
	n, err := io.Copy(f, r)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		// wp_check_filetype_and_ext(): an image's content decides its type. A file that is not the
		// image its extension claims is refused; another image format gets the right extension (a
		// PNG named .jpg would otherwise get JPEG-named sizes holding PNG data, and the other way
		// round).
		if real, ok := sniffImage(dst, mime); !ok {
			err = ErrType
		} else if real != "" {
			ext := formatExt(strings.TrimPrefix(real, "image/"))
			fixed := uniqueName(dir, strings.TrimSuffix(name, filepath.Ext(name))+ext, taken...)
			if err = os.Rename(dst, filepath.Join(dir, fixed)); err == nil {
				name, dst, mime = fixed, filepath.Join(dir, fixed), real
			}
		}
	}
	if err == nil && mime == "image/svg+xml" && n <= MaxSVGBytes {
		err = sanitizeSVGFile(dst)
		if st, serr := os.Stat(dst); err == nil && serr == nil {
			n = st.Size()
		}
	} else if err == nil && mime == "image/svg+xml" {
		err = ErrSVG
	}
	if err != nil {
		os.Remove(dst)
		return nil, err
	}
	return &Stored{RelPath: filepath.ToSlash(filepath.Join(sub, name)), Size: n, Mime: mime, Original: filename}, nil
}

// uniqueName is wp_unique_filename(): name.ext, name-1.ext, name-2.ext …, also avoiding
// collisions with generated size variants of other files (_wp_check_existing_file_names()): a
// name is taken when the directory already holds a file one of its sub-sizes or its -scaled copy
// would overwrite, e.g. logo.png is not free while logo-150x150.png exists. reserved are more
// names that count as present in dir.
func uniqueName(dir, name string, reserved ...string) string {
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	var existing []string
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			existing = append(existing, strings.ToLower(e.Name()))
		}
	}
	held := map[string]bool{}
	for _, r := range reserved {
		r = strings.ToLower(r)
		held[r] = true
		existing = append(existing, r)
	}
	cand := name
	for i := 1; ; i++ {
		if _, err := os.Stat(filepath.Join(dir, cand)); os.IsNotExist(err) && !held[strings.ToLower(cand)] &&
			!variantTaken(existing, strings.TrimSuffix(cand, ext)) && !variantOfOriginal(existing, cand) {
			return cand
		}
		cand = base + "-" + strconv.Itoa(i) + ext
	}
}

// variantTaken reports whether names (lower-cased) hold a generated variant of base:
// base-<w>x<h>.<image ext> or base-scaled.<image ext>, in any output format.
func variantTaken(names []string, base string) bool {
	prefix := strings.ToLower(base) + "-"
	for _, n := range names {
		rest, ok := strings.CutPrefix(n, prefix)
		if !ok {
			continue
		}
		dot := strings.LastIndexByte(rest, '.')
		if dot <= 0 {
			continue
		}
		if variantImageExt(rest[dot:]) && variantSuffix(rest[:dot]) {
			return true
		}
	}
	return false
}

// variantOfOriginal reserves names that an original image can generate later.
func variantOfOriginal(names []string, name string) bool {
	ext := filepath.Ext(name)
	base := strings.ToLower(strings.TrimSuffix(name, ext))
	dash := strings.LastIndexByte(base, '-')
	if dash < 0 || !variantSuffix(base[dash+1:]) || !variantImageExt(strings.ToLower(ext)) {
		return false
	}
	for _, n := range names {
		ext := filepath.Ext(n)
		if strings.TrimSuffix(n, ext) == base[:dash] && variantImageExt(ext) {
			return true
		}
	}
	return false
}

func variantImageExt(ext string) bool {
	switch ext {
	case ".jpg", ".jpeg", ".jpe", ".png", ".gif", ".webp", ".avif", ".heic", ".bmp", ".tif", ".tiff":
		return true
	}
	return false
}

func variantSuffix(s string) bool { return s == "scaled" || s == "rotated" || isSizeSuffix(s) }

// isSizeSuffix matches <digits>x<digits>.
func isSizeSuffix(s string) bool {
	w, h, ok := strings.Cut(s, "x")
	if !ok || w == "" || h == "" {
		return false
	}
	for _, r := range w + h {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// Size is a registered image size (add_image_size()).
type Size struct {
	Name          string
	Width, Height int
	Crop          bool
}

// Variant is one generated sub-size.
type Variant struct {
	Name          string
	File          string // file name only, in the same folder as the original
	Width, Height int
	Mime          string
	Size          int64
}

// ImageInfo is the result of processing an uploaded image (wp_create_image_subsizes()).
type ImageInfo struct {
	Width, Height int
	RelPath       string // may change to the -scaled file
	OriginalFile  string // set when a -scaled copy replaced the original as the main file
	Variants      []Variant
	FileSize      int64
}

// MaxImagePixels is the largest image (width × height) ProcessImageWith decodes: 100 megapixels,
// about 400 MB as RGBA. Larger uploads keep their original file without generated sizes.
const MaxImagePixels = 100_000_000

// processSem bounds how many images are decoded and resized at once (memory and CPU), like the
// CDN image service's semaphore (gocdn_img.go).
var processSem = make(chan struct{}, max(1, min(4, runtime.NumCPU()/2)))

// BigImageThreshold is the big_image_size_threshold default.
const BigImageThreshold = 2560

// Options are the image editor's choices for one image.
type Options struct {
	// OutputFormat is image_editor_output_format: a source MIME type mapped to the MIME type the
	// image's sub-sizes are saved in, e.g. {"image/webp": "image/webp"} keeps WebP uploads in WebP,
	// {"image/jpeg": "image/webp"} writes WebP sizes of JPEG uploads. When the mapping changes the
	// format, the main image is converted too (wp_create_image_subsizes()) and the uploaded file is
	// kept as the original image. Mappings to a format that cannot be written are ignored.
	OutputFormat map[string]string
	// Reserved are file names in the image's folder that count as taken besides the files on disk
	// (offloaded files whose local copy was removed), for the converted main file's name.
	Reserved []string
	// Focal is the image's focal point: cropped sizes keep it in view instead of cutting the
	// centre. Nil crops around the centre, like WordPress.
	Focal *Focal
	// Quality is the JPEG/WebP quality (1–100) of the files written; 0 keeps WordPress' defaults
	// (JPEG 82, WebP 86). Media › Optimization sets it.
	Quality int
	// OnlyIfSmaller keeps an image that is not scaled down in its own format when the converted
	// main file would not be smaller than the upload: the conversion is dropped and the sizes are
	// written in the upload's format (Media › Optimization).
	OnlyIfSmaller bool
	// KeepEXIF keeps a JPEG's EXIF/XMP metadata (GPS position, camera serial) in the stored
	// original: Settings › Media "Remove location and camera data" turned off. PURSAE_KEEP_EXIF=1
	// keeps it server-wide whatever this says.
	KeepEXIF bool
}

// Focal is a focal point in fractions of the (upright) image: X 0 = left edge, 1 = right edge;
// Y 0 = top, 1 = bottom. It has the shape of the block editor's focalPoint attribute.
type Focal struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

// Valid reports whether both coordinates are within 0…1.
func (f Focal) Valid() bool { return f.X >= 0 && f.X <= 1 && f.Y >= 0 && f.Y <= 1 }

// Centered reports whether the point is the centre (within half a percent), where a crop is the
// same as without a focal point.
func (f Focal) Centered() bool { return math.Abs(f.X-0.5) < 0.005 && math.Abs(f.Y-0.5) < 0.005 }

// CSS is the object-position / background-position value of the point, e.g. "30% 20%".
func (f Focal) CSS() string {
	pct := func(v float64) string {
		return strconv.FormatFloat(math.Round(v*1000)/10, 'f', -1, 64) + "%"
	}
	return pct(f.X) + " " + pct(f.Y)
}

// CropRect is the part of a sw×sh image that a crop to the w:h aspect ratio keeps: as large as
// the ratio allows, centred on the focal point and moved back inside the image where the point is
// near an edge. A nil or centred focal point gives WordPress' centre crop. The rectangle starts at
// 0,0 (add the image bounds' Min).
func CropRect(sw, sh, w, h int, f *Focal) image.Rectangle {
	if sw <= 0 || sh <= 0 || w <= 0 || h <= 0 {
		return image.Rect(0, 0, max(sw, 0), max(sh, 0))
	}
	targetRatio := float64(w) / float64(h)
	focal := f != nil && f.Valid() && !f.Centered()
	if float64(sw)/float64(sh) > targetRatio {
		cw := max(1, min(sw, int(float64(sh)*targetRatio)))
		x := (sw - cw) / 2
		if focal {
			x = clampInt(int(math.Round(f.X*float64(sw)-float64(cw)/2)), 0, sw-cw)
		}
		return image.Rect(x, 0, x+cw, sh)
	}
	ch := max(1, min(sh, int(float64(sw)/targetRatio)))
	y := (sh - ch) / 2
	if focal {
		y = clampInt(int(math.Round(f.Y*float64(sh)-float64(ch)/2)), 0, sh-ch)
	}
	return image.Rect(0, y, sw, y+ch)
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// ProcessImage is ProcessImageWith with the default options.
func ProcessImage(uploadsDir, relPath string, sizes []Size) (*ImageInfo, error) {
	return ProcessImageWith(uploadsDir, relPath, sizes, Options{})
}

// ProcessImageWith reads an uploaded image, creates the -scaled main file when needed and every
// registered size that is smaller than the original. Formats Go can't decode (AVIF, HEIC) keep
// their original file only.
//
// An image over MaxImagePixels is not decoded (a small file can declare a huge canvas: 30000×30000
// is 3.6 GB once decoded): it keeps its original file and measured size only. At most
// cap(processSem) images are decoded and resized at a time.
func ProcessImageWith(uploadsDir, relPath string, sizes []Size, opts Options) (*ImageInfo, error) {
	full := filepath.Join(uploadsDir, filepath.FromSlash(relPath))
	f, err := os.Open(full)
	if err != nil {
		return nil, err
	}
	cfg, cfmt, cerr := image.DecodeConfig(f)
	if cerr == nil && int64(cfg.Width)*int64(cfg.Height) > MaxImagePixels {
		f.Close()
		if cfmt == "jpeg" {
			stripJPEGFile(full, opts.KeepEXIF)
		}
		info := &ImageInfo{RelPath: relPath, Width: cfg.Width, Height: cfg.Height}
		if st, serr := os.Stat(full); serr == nil {
			info.FileSize = st.Size()
		}
		return info, nil
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		f.Close()
		return nil, err
	}
	processSem <- struct{}{}
	defer func() { <-processSem }()
	img, format, err := image.Decode(f)
	f.Close()
	if err != nil {
		// Formats that cannot be decoded here are still measured: SVG from its root element,
		// AVIF/HEIC from their "ispe" box. They get no sub-sizes.
		info := &ImageInfo{RelPath: relPath}
		if st, serr := os.Stat(full); serr == nil {
			info.FileSize = st.Size()
		}
		switch strings.ToLower(filepath.Ext(full)) {
		case ".svg":
			if data, rerr := os.ReadFile(full); rerr == nil {
				info.Width, info.Height = svgDimensions(data)
			}
			return info, nil
		case ".avif", ".heic", ".heif":
			if w, h, ok := isoDimensions(full); ok {
				info.Width, info.Height = w, h
			}
			return info, nil
		}
		return nil, err
	}
	// A JPEG with an EXIF orientation is turned upright first, like wp_maybe_exif_rotate(): the
	// sizes and the stored dimensions follow what the browser shows.
	rotated := false
	if format == "jpeg" {
		stripJPEGFile(full, opts.KeepEXIF) // no GPS or camera data in the original served to visitors
		if rf, err := os.Open(full); err == nil {
			if o := JPEGOrientation(rf); o > 1 {
				img, rotated = Orient(img, o), true
			}
			rf.Close()
		}
	}
	b := img.Bounds()
	info := &ImageInfo{Width: b.Dx(), Height: b.Dy(), RelPath: relPath}
	if st, err := os.Stat(full); err == nil {
		info.FileSize = st.Size()
	}
	dir := filepath.Dir(full)
	ext := filepath.Ext(full)
	base := strings.TrimSuffix(filepath.Base(full), ext)
	// Sub-sizes keep the upload's format, or the one opts.OutputFormat maps it to. Without a mapping,
	// WebP/BMP/TIFF sizes are JPEG (PNG with transparency) and the original stays as uploaded, with
	// no "-scaled" copy.
	out := outputFormat(format, img)
	convert := false
	if to, ok := opts.OutputFormat["image/"+format]; ok {
		if f := strings.TrimPrefix(to, "image/"); writable(f) {
			out = f
			convert = f != format
		}
	}
	native := out == format
	outExt := ext
	if !native {
		outExt = formatExt(out)
	}

	src := img
	big := info.Width > BigImageThreshold || info.Height > BigImageThreshold
	if (native || convert) && big {
		w, h := fitWithin(info.Width, info.Height, BigImageThreshold, BigImageThreshold)
		scaled := resize(img, w, h, false, nil)
		name := base + "-scaled" + outExt
		if err := encodeQ(filepath.Join(dir, name), scaled, out, opts.Quality); err == nil {
			info.OriginalFile = filepath.Base(full)
			info.RelPath = filepath.ToSlash(filepath.Join(filepath.Dir(relPath), name))
			info.Width, info.Height = w, h
			if st, err := os.Stat(filepath.Join(dir, name)); err == nil {
				info.FileSize = st.Size()
			}
			src = scaled
		}
	} else if native && rotated {
		// Turned upright at its own size: photo.jpg → photo-rotated.jpg, the upload kept as the original.
		name := base + "-rotated" + outExt
		if err := encodeQ(filepath.Join(dir, name), img, out, opts.Quality); err == nil {
			info.OriginalFile = filepath.Base(full)
			info.RelPath = filepath.ToSlash(filepath.Join(filepath.Dir(relPath), name))
			if st, err := os.Stat(filepath.Join(dir, name)); err == nil {
				info.FileSize = st.Size()
			}
		}
	} else if convert {
		// The image converted at its own size: photo.jpg → photo.webp, the upload kept as the original.
		name := uniqueName(dir, base+outExt, opts.Reserved...)
		if err := encodeQ(filepath.Join(dir, name), img, out, opts.Quality); err == nil {
			st, serr := os.Stat(filepath.Join(dir, name))
			if opts.OnlyIfSmaller && (serr != nil || st.Size() >= info.FileSize) {
				// Not smaller than the upload: keep it as uploaded, sizes in its own format.
				_ = os.Remove(filepath.Join(dir, name))
				out = outputFormat(format, img)
				outExt = ext
				if out != format {
					outExt = formatExt(out)
				} else if rotated { // upright at its own size, as without the conversion
					rname := base + "-rotated" + outExt
					if err := encodeQ(filepath.Join(dir, rname), img, out, opts.Quality); err == nil {
						info.OriginalFile = filepath.Base(full)
						info.RelPath = filepath.ToSlash(filepath.Join(filepath.Dir(relPath), rname))
						if rst, err := os.Stat(filepath.Join(dir, rname)); err == nil {
							info.FileSize = rst.Size()
						}
					}
				}
			} else {
				info.OriginalFile = filepath.Base(full)
				info.RelPath = filepath.ToSlash(filepath.Join(filepath.Dir(relPath), name))
				if serr == nil {
					info.FileSize = st.Size()
				}
			}
		}
	}

	for _, sz := range sizes {
		w, h, ok := resizeDims(info.Width, info.Height, sz.Width, sz.Height, sz.Crop)
		if !ok {
			continue
		}
		resized := resize(src, w, h, sz.Crop, opts.Focal)
		name := fmt.Sprintf("%s-%dx%d%s", base, w, h, outExt)
		path := filepath.Join(dir, name)
		if err := encodeQ(path, resized, out, opts.Quality); err != nil {
			continue
		}
		v := Variant{Name: sz.Name, File: name, Width: w, Height: h, Mime: "image/" + out}
		if st, err := os.Stat(path); err == nil {
			v.Size = st.Size()
		}
		info.Variants = append(info.Variants, v)
	}
	return info, nil
}

// resizeDims is image_resize_dimensions(): no upscaling, proportional fit or center crop.
func resizeDims(origW, origH, maxW, maxH int, crop bool) (int, int, bool) {
	if maxW <= 0 && maxH <= 0 {
		return 0, 0, false
	}
	if crop {
		w, h := maxW, maxH
		if w <= 0 {
			w = origW
		}
		if h <= 0 {
			h = origH
		}
		if w > origW {
			w = origW
		}
		if h > origH {
			h = origH
		}
		if w == origW && h == origH {
			return 0, 0, false
		}
		return w, h, true
	}
	if maxW <= 0 {
		maxW = origW
	}
	if maxH <= 0 {
		maxH = origH
	}
	w, h := fitWithin(origW, origH, maxW, maxH)
	if w >= origW && h >= origH {
		return 0, 0, false
	}
	return w, h, true
}

// fitWithin is wp_constrain_dimensions().
func fitWithin(w, h, maxW, maxH int) (int, int) {
	if w <= maxW && h <= maxH {
		return w, h
	}
	rw := float64(maxW) / float64(w)
	rh := float64(maxH) / float64(h)
	r := rw
	if rh < rw {
		r = rh
	}
	nw := int(float64(w)*r + 0.5)
	nh := int(float64(h)*r + 0.5)
	if nw < 1 {
		nw = 1
	}
	if nh < 1 {
		nh = 1
	}
	return nw, nh
}

func resize(src image.Image, w, h int, crop bool, focal *Focal) image.Image {
	sb := src.Bounds()
	srcRect := sb
	if crop {
		// Crop to the target aspect ratio first: around the focal point, or the centre.
		srcRect = CropRect(sb.Dx(), sb.Dy(), w, h, focal).Add(sb.Min)
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, srcRect, draw.Over, nil)
	return dst
}

func encode(path string, img image.Image, format string) error { return encodeQ(path, img, format, 0) }

// encodeQ is encode at a quality (0: the default).
func encodeQ(path string, img image.Image, format string, quality int) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	err = EncodeToQuality(f, img, format, quality)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(path)
	}
	return err
}

// HumanSize is size_format().
func HumanSize(n int64) string {
	units := []string{"B", "KB", "MB", "GB", "TB"}
	f := float64(n)
	i := 0
	for f >= 1024 && i < len(units)-1 {
		f /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%d B", n)
	}
	s := strconv.FormatFloat(f, 'f', 0, 64)
	if f < 10 {
		s = strings.TrimSuffix(strings.TrimSuffix(strconv.FormatFloat(f, 'f', 1, 64), "0"), ".")
	}
	return s + " " + units[i]
}

// Edit operations of the image editor (image_edit_apply_changes()).

// Op is one step of the editor history: Rotate (degrees, counter-clockwise), Flip (bit 1 = along the
// horizontal axis, bit 2 = along the vertical axis) or Crop (in preview coordinates; Ratio converts
// them to image pixels, 0 means use the preview ratio of the current image).
type Op struct {
	Rotate            int
	Flip              int
	Crop              bool
	X, Y, W, H, Ratio float64
}

// PreviewRatio is _image_get_preview_ratio().
func PreviewRatio(w, h int) float64 {
	m := w
	if h > m {
		m = h
	}
	if m > 600 {
		return 600 / float64(m)
	}
	return 1
}

// maxEditHistory is the longest editor history ApplyOps will honour. The client sends the whole
// history on every preview; only the latest steps can change the image.
const maxEditHistory = 64

// pathWithin reports whether child is root or a path under root after Clean and Join. On Windows
// an absolute second element makes Join discard root; Rel then starts with "..".
func pathWithin(root, child string) bool {
	rel, err := filepath.Rel(root, child)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// ApplyOps combines consecutive rotations and flips like WordPress, then applies the history.
func ApplyOps(img image.Image, ops []Op) image.Image {
	if len(ops) > maxEditHistory {
		ops = ops[len(ops)-maxEditHistory:]
	}
	var merged []Op
	for _, op := range ops {
		// Unsupported rotations are no-ops individually; do not let merging turn
		// two unsupported angles into a supported one.
		if !op.Crop && op.Flip == 0 && op.Rotate%90 != 0 {
			continue
		}
		if n := len(merged); n > 0 && !op.Crop && !merged[n-1].Crop {
			last := &merged[n-1]
			if op.Flip == 0 && last.Flip == 0 {
				last.Rotate += op.Rotate
				continue
			}
			if op.Rotate == 0 && last.Rotate == 0 {
				last.Flip ^= op.Flip
				continue
			}
		}
		merged = append(merged, op)
	}
	for _, op := range merged {
		switch {
		case op.Crop:
			if op.W <= 0 || op.H <= 0 {
				continue
			}
			b := img.Bounds()
			scale := op.Ratio
			if scale == 0 {
				scale = 1 / PreviewRatio(b.Dx(), b.Dy())
			}
			r := image.Rect(int(op.X*scale), int(op.Y*scale), int(op.X*scale)+int(op.W*scale), int(op.Y*scale)+int(op.H*scale)).
				Add(b.Min).Intersect(b)
			if r.Empty() {
				continue
			}
			dst := image.NewRGBA(image.Rect(0, 0, r.Dx(), r.Dy()))
			draw.Draw(dst, dst.Bounds(), img, r.Min, draw.Src)
			img = dst
		case op.Flip != 0:
			img = flip(img, op.Flip&1 != 0, op.Flip&2 != 0)
		default:
			img = rotate(img, op.Rotate)
		}
	}
	return img
}

func rotate(src image.Image, angle int) image.Image {
	angle = ((angle % 360) + 360) % 360
	if angle != 90 && angle != 180 && angle != 270 {
		return src
	}
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	var dst *image.RGBA
	if angle == 180 {
		dst = image.NewRGBA(image.Rect(0, 0, w, h))
	} else {
		dst = image.NewRGBA(image.Rect(0, 0, h, w))
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := src.At(b.Min.X+x, b.Min.Y+y)
			switch angle {
			case 90: // counter-clockwise, like imagerotate()
				dst.Set(y, w-1-x, c)
			case 180:
				dst.Set(w-1-x, h-1-y, c)
			case 270:
				dst.Set(h-1-y, x, c)
			}
		}
	}
	return dst
}

func flip(src image.Image, vertical, horizontal bool) image.Image {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			dx, dy := x, y
			if horizontal {
				dx = w - 1 - x
			}
			if vertical {
				dy = h - 1 - y
			}
			dst.Set(dx, dy, src.At(b.Min.X+x, b.Min.Y+y))
		}
	}
	return dst
}

// ErrTooLarge is returned by Decode for an image over MaxImagePixels.
var ErrTooLarge = errors.New("the image is too large to be edited")

// Decode opens an image file under uploadsDir. Images over MaxImagePixels are refused before
// their pixels are decoded, and at most cap(processSem) images are decoded at a time.
func Decode(uploadsDir, relPath string) (image.Image, string, error) {
	f, err := os.Open(filepath.Join(uploadsDir, filepath.FromSlash(relPath)))
	if err != nil {
		return nil, "", err
	}
	defer f.Close()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		return nil, "", err
	}
	if int64(cfg.Width)*int64(cfg.Height) > MaxImagePixels {
		return nil, "", ErrTooLarge
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, "", err
	}
	processSem <- struct{}{}
	defer func() { <-processSem }()
	return image.Decode(f)
}

// DecodeAndProcess opens an image file like Decode and runs fn on it while still holding the
// decode slot, so the whole edit (decode, transforms, encode) counts against cap(processSem) and
// concurrent edits are throttled like uploads. The decode error, else fn's error, is returned.
func DecodeAndProcess(uploadsDir, relPath string, fn func(img image.Image, format string) error) error {
	f, err := os.Open(filepath.Join(uploadsDir, filepath.FromSlash(relPath)))
	if err != nil {
		return err
	}
	defer f.Close()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		return err
	}
	if int64(cfg.Width)*int64(cfg.Height) > MaxImagePixels {
		return ErrTooLarge
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	processSem <- struct{}{}
	defer func() { <-processSem }()
	img, format, err := image.Decode(f)
	if err != nil {
		return err
	}
	return fn(img, format)
}

// Resize scales an image to exactly w×h.
func Resize(img image.Image, w, h int) image.Image { return resize(img, w, h, false, nil) }

// EncodeTo writes img to w in the given format (jpeg, webp, png or gif), at WordPress' default
// qualities (WP_Image_Editor::get_default_quality(): 82, WebP 86).
func EncodeTo(w io.Writer, img image.Image, format string) error {
	return EncodeToQuality(w, img, format, 0)
}

// EncodeToQuality is EncodeTo at a JPEG/WebP quality from 1 to 100 (0 or out of range: the default).
// PNG is written with the best compression when a quality is given.
func EncodeToQuality(w io.Writer, img image.Image, format string, quality int) error {
	if quality < 1 || quality > 100 {
		quality = 0
	}
	switch format {
	case "jpeg":
		return jpeg.Encode(w, img, &jpeg.Options{Quality: cmp.Or(quality, 82)})
	case "webp":
		// libwebp transpiled to Go (no cgo; the nodynamic build tag keeps it from loading a system library).
		return webp.Encode(w, img, webp.Options{Quality: cmp.Or(quality, 86), Method: 4})
	case "gif":
		return gif.Encode(w, img, nil)
	}
	if quality > 0 {
		return (&png.Encoder{CompressionLevel: png.BestCompression}).Encode(w, img)
	}
	return png.Encode(w, img)
}

// WriteFile encodes img to a new file under uploadsDir.
func WriteFile(uploadsDir, relPath string, img image.Image, format string) error {
	return encode(filepath.Join(uploadsDir, filepath.FromSlash(relPath)), img, format)
}
