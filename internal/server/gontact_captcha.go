package server

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"html"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"log/slog"
	"math"
	"math/big"
	mrand "math/rand/v2"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"

	"wpgo/internal/auth"
)

// The image CAPTCHA of Gontact Form: Contact Form 7's [captchac] / [captchar] form-tags with the
// Really Simple CAPTCHA engine built in. A challenge is a random prefix; the server keeps the PNG
// and an HMAC of the answer (table gontact_captcha) for an hour, serves the image at
// /wp-content/gontact-captcha/{prefix}.png and deletes the challenge on its first check, right or
// wrong. Pages served from the page cache get fresh challenges from the refill endpoint.

const (
	gfCaptchaPath  = "/wp-content/gontact-captcha"
	gfCaptchaTTL   = time.Hour
	gfCaptchaChars = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789" // no 0/O, 1/I/l: nothing to confuse
	gfCaptchaLen   = 4
	gfCaptchaScale = 3 // the PNG is drawn at 3× the displayed size, sharp on high-density screens
)

// gfCaptchaOpts are the image options of one [captchac] tag (wpcf7_captchac_options()).
type gfCaptchaOpts struct {
	W, H         int // displayed size (the img width/height attributes)
	BaseX, BaseY int // left edge and baseline of the first character
	FontSize     int
	CharWidth    int
	FG, BG       color.RGBA
}

var gfCaptchaColorRe = regexp.MustCompile(`^(?i)(fg|bg):#([0-9a-f]{2})([0-9a-f]{2})([0-9a-f]{2})$`)

// gfCaptchaOptions reads size:s|m|l, fg:#rrggbb and bg:#rrggbb (CF7's defaults otherwise).
func gfCaptchaOptions(t *gfTag) gfCaptchaOpts {
	o := gfCaptchaOpts{W: 72, H: 24, BaseX: 6, BaseY: 18, FontSize: 14, CharWidth: 15,
		FG: color.RGBA{0, 0, 0, 255}, BG: color.RGBA{255, 255, 255, 255}}
	if t == nil {
		return o
	}
	for _, opt := range t.Options {
		switch strings.ToLower(opt) {
		case "size:s":
			o.W, o.H, o.BaseX, o.BaseY, o.FontSize, o.CharWidth = 60, 20, 6, 15, 11, 13
		case "size:m":
			o.W, o.H, o.BaseX, o.BaseY, o.FontSize, o.CharWidth = 72, 24, 6, 18, 14, 15
		case "size:l":
			o.W, o.H, o.BaseX, o.BaseY, o.FontSize, o.CharWidth = 84, 28, 6, 20, 17, 19
		}
		if m := gfCaptchaColorRe.FindStringSubmatch(opt); m != nil {
			r, _ := strconv.ParseUint(m[2], 16, 8)
			g, _ := strconv.ParseUint(m[3], 16, 8)
			b, _ := strconv.ParseUint(m[4], 16, 8)
			c := color.RGBA{uint8(r), uint8(g), uint8(b), 255}
			if strings.EqualFold(m[1], "fg") {
				o.FG = c
			} else {
				o.BG = c
			}
		}
	}
	return o
}

// gfCaptchaWord is a random code of the unambiguous characters.
func gfCaptchaWord() string {
	b := make([]byte, gfCaptchaLen)
	for i := range b {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(gfCaptchaChars))))
		if err != nil {
			panic(err)
		}
		b[i] = gfCaptchaChars[n.Int64()]
	}
	return string(b)
}

// gfCaptchaPrefix is a random numeric prefix (numeric like Really Simple CAPTCHA's, so CF7's own
// front-end script, which reads it back from the image URL, works unchanged).
func gfCaptchaPrefix() string {
	n, err := rand.Int(rand.Reader, big.NewInt(9e15))
	if err != nil {
		panic(err)
	}
	return strconv.FormatInt(n.Int64()+1e15, 10)
}

var gfCaptchaPrefixRe = regexp.MustCompile(`^[0-9]{6,20}$`)

var gfCaptchaFont = sync.OnceValues(func() (*opentype.Font, error) { return opentype.Parse(gobold.TTF) })

// gfCaptchaRender draws code as a PNG: each character slightly rotated and shifted, over noise
// lines and dots in tints between the foreground and the background.
func gfCaptchaRender(code string, o gfCaptchaOpts) ([]byte, error) {
	ft, err := gfCaptchaFont()
	if err != nil {
		return nil, err
	}
	const s = gfCaptchaScale
	size := float64(o.FontSize * s)
	face, err := opentype.NewFace(ft, &opentype.FaceOptions{Size: size, DPI: 72, Hinting: font.HintingNone})
	if err != nil {
		return nil, err
	}
	defer face.Close()
	rnd := mrand.New(mrand.NewPCG(mrand.Uint64(), mrand.Uint64()))
	img := image.NewRGBA(image.Rect(0, 0, o.W*s, o.H*s))
	draw.Draw(img, img.Bounds(), &image.Uniform{o.BG}, image.Point{}, draw.Src)
	tint := func(f float64) color.RGBA {
		mix := func(a, b uint8) uint8 { return uint8(float64(a)*f + float64(b)*(1-f)) }
		return color.RGBA{mix(o.FG.R, o.BG.R), mix(o.FG.G, o.BG.G), mix(o.FG.B, o.BG.B), 255}
	}

	// Background noise: dots, then a few thin lines across the whole image.
	for i := 0; i < o.W*o.H/12; i++ {
		x, y := rnd.IntN(o.W*s), rnd.IntN(o.H*s)
		gfCaptchaDot(img, x, y, 1+rnd.IntN(s), tint(0.25+rnd.Float64()*0.35))
	}
	for i := 0; i < 4; i++ {
		gfCaptchaLine(img, rnd.Float64()*float64(o.W*s)/3, rnd.Float64()*float64(o.H*s),
			float64(o.W*s)*(0.66+rnd.Float64()/3), rnd.Float64()*float64(o.H*s), float64(s)/2+rnd.Float64()*float64(s)/2, tint(0.4+rnd.Float64()*0.3))
	}

	// The characters: each drawn into its own mask, rotated and composited.
	capH := size * 0.72
	box := int(size * 1.6)
	for i, r := range code {
		mask := image.NewAlpha(image.Rect(0, 0, box, box))
		adv, _ := face.GlyphAdvance(r)
		d := &font.Drawer{Dst: mask, Src: image.Opaque, Face: face,
			Dot: fixed.Point26_6{X: fixed.I(box)/2 - adv/2, Y: fixed.I(int(float64(box)/2 + capH/2))}}
		d.DrawString(string(r))
		rot := gfCaptchaRotate(mask, (rnd.Float64()-0.5)*0.6) // up to ±17°
		cx := float64((o.BaseX+i*o.CharWidth)*s) + float64(o.CharWidth*s)/2 + (rnd.Float64()-0.5)*float64(s)*2
		cy := float64(o.BaseY*s) - capH/2 + (rnd.Float64()-0.5)*float64(s)*2
		at := image.Pt(int(cx)-box/2, int(cy)-box/2)
		draw.DrawMask(img, image.Rectangle{Min: at, Max: at.Add(image.Pt(box, box))}, &image.Uniform{o.FG}, image.Point{}, rot, image.Point{}, draw.Over)
	}

	// A line over the characters, lighter, so it does not hide them.
	gfCaptchaLine(img, 0, float64(o.H*s)*(0.3+rnd.Float64()*0.4), float64(o.W*s), float64(o.H*s)*(0.3+rnd.Float64()*0.4), float64(s)/2, tint(0.45))

	var buf bytes.Buffer
	if err := (&png.Encoder{CompressionLevel: png.BestCompression}).Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// gfCaptchaRotate rotates a square mask around its centre (bilinear sampling).
func gfCaptchaRotate(src *image.Alpha, angle float64) *image.Alpha {
	b := src.Bounds()
	dst := image.NewAlpha(b)
	cx, cy := float64(b.Dx())/2, float64(b.Dy())/2
	sin, cos := math.Sincos(angle)
	at := func(x, y int) float64 {
		if x < 0 || y < 0 || x >= b.Dx() || y >= b.Dy() {
			return 0
		}
		return float64(src.Pix[y*src.Stride+x])
	}
	for y := 0; y < b.Dy(); y++ {
		for x := 0; x < b.Dx(); x++ {
			dx, dy := float64(x)+0.5-cx, float64(y)+0.5-cy
			sx, sy := cos*dx+sin*dy+cx-0.5, -sin*dx+cos*dy+cy-0.5
			x0, y0 := int(math.Floor(sx)), int(math.Floor(sy))
			fx, fy := sx-float64(x0), sy-float64(y0)
			v := at(x0, y0)*(1-fx)*(1-fy) + at(x0+1, y0)*fx*(1-fy) + at(x0, y0+1)*(1-fx)*fy + at(x0+1, y0+1)*fx*fy
			dst.Pix[y*dst.Stride+x] = uint8(math.Min(255, v+0.5))
		}
	}
	return dst
}

func gfCaptchaDot(img *image.RGBA, x, y, r int, c color.RGBA) {
	draw.Draw(img, image.Rect(x-r/2, y-r/2, x-r/2+r, y-r/2+r).Intersect(img.Bounds()), &image.Uniform{c}, image.Point{}, draw.Src)
}

func gfCaptchaLine(img *image.RGBA, x0, y0, x1, y1, width float64, c color.RGBA) {
	n := int(math.Hypot(x1-x0, y1-y0))
	w := int(math.Max(1, math.Round(width)))
	for i := 0; i <= n; i++ {
		t := float64(i) / float64(max(n, 1))
		gfCaptchaDot(img, int(x0+(x1-x0)*t), int(y0+(y1-y0)*t), w, c)
	}
}

// gontactCaptchaHash is the stored form of an answer: an HMAC over the prefix and the
// canonicalized code, so the table never holds the code itself.
func (s *Server) gontactCaptchaHash(prefix, answer string) string {
	m := hmac.New(sha256.New, []byte(s.settings.String(gontactSecretKey)+"|wpcf7_captcha"))
	m.Write([]byte(prefix + "|" + gfCanonicalize(answer)))
	return hex.EncodeToString(m.Sum(nil))
}

// gontactCaptchaMaxRows bounds the open challenges (each holds a PNG); they expire after an hour.
// A full table makes room (expired rows first, then the oldest) instead of refusing, so page loads
// alone cannot lock every CAPTCHA form until the hourly cleanup.
var gontactCaptchaMaxRows = 5000

// Challenges one client network may create per window: enough for any visitor reloading a form,
// too few for one network to rotate everyone else's challenges out of the capped table.
var (
	gontactCaptchaNetLimit  = 60
	gontactCaptchaNetWindow = 10 * time.Minute
	gontactCaptchaNetMaxKey = 20000 // bound of the in-memory counter map
)

type gontactCaptchaNetHits struct {
	start time.Time
	n     int
}

var gontactCaptchaNets = struct {
	sync.Mutex
	hits  map[string]*gontactCaptchaNetHits
	swept time.Time
}{hits: map[string]*gontactCaptchaNetHits{}}

// gontactCaptchaNetAllow counts one challenge for the client network net ("" when unknown: not limited).
func gontactCaptchaNetAllow(net string, now time.Time) bool {
	if net == "" {
		return true
	}
	l := &gontactCaptchaNets
	l.Lock()
	defer l.Unlock()
	if len(l.hits) >= gontactCaptchaNetMaxKey || now.Sub(l.swept) > gontactCaptchaNetWindow {
		for k, h := range l.hits {
			if now.Sub(h.start) >= gontactCaptchaNetWindow {
				delete(l.hits, k)
			}
		}
		l.swept = now
		if len(l.hits) >= gontactCaptchaNetMaxKey { // still full: forget rather than grow without bound
			l.hits = map[string]*gontactCaptchaNetHits{}
		}
	}
	h := l.hits[net]
	if h == nil || now.Sub(h.start) >= gontactCaptchaNetWindow {
		h = &gontactCaptchaNetHits{start: now}
		l.hits[net] = h
	}
	h.n++
	return h.n <= gontactCaptchaNetLimit
}

// gontactCaptchaClientNet is the client network of the request behind ctx ("" outside a request).
func gontactCaptchaClientNet(ctx context.Context) string {
	if gc, ok := ctx.(*gin.Context); ok && gc.Request != nil {
		return auth.ClientNet(clientIP(gc))
	}
	return ""
}

// gontactCaptchaNew creates a challenge and returns its prefix (wpcf7_generate_captcha()).
func (s *Server) gontactCaptchaNew(ctx context.Context, o gfCaptchaOpts) (string, error) {
	if !gontactCaptchaNetAllow(gontactCaptchaClientNet(ctx), time.Now()) {
		return "", errors.New("too many CAPTCHA challenges from this network")
	}
	// Only live challenges count; make room by dropping expired ones, then the oldest.
	var n int
	_ = s.store.Pool.QueryRow(ctx, `SELECT count(*) FROM gontact_captcha WHERE created_at > now() - $1::interval`, gfCaptchaInterval()).Scan(&n)
	if n >= gontactCaptchaMaxRows {
		_, _ = s.store.Pool.Exec(ctx, `DELETE FROM gontact_captcha WHERE created_at <= now() - $1::interval`, gfCaptchaInterval())
		_, _ = s.store.Pool.Exec(ctx, `DELETE FROM gontact_captcha WHERE prefix IN
			(SELECT prefix FROM gontact_captcha ORDER BY created_at ASC, prefix LIMIT $1)`, n-gontactCaptchaMaxRows+1)
	}
	word := gfCaptchaWord()
	png, err := gfCaptchaRender(word, o)
	if err != nil {
		return "", err
	}
	prefix := gfCaptchaPrefix()
	_, err = s.store.Pool.Exec(ctx, `INSERT INTO gontact_captcha (prefix, answer_hash, image) VALUES ($1, $2, $3)`,
		prefix, s.gontactCaptchaHash(prefix, word), png)
	return prefix, err
}

// gontactCaptchaCheck is wpcf7_check_captcha() followed by wpcf7_remove_captcha(): the challenge
// is used up whatever the answer.
func (s *Server) gontactCaptchaCheck(ctx context.Context, prefix, answer string) bool {
	if !gfCaptchaPrefixRe.MatchString(prefix) {
		return false
	}
	var stored string
	var fresh bool
	err := s.store.Pool.QueryRow(ctx, `DELETE FROM gontact_captcha WHERE prefix = $1 RETURNING answer_hash, created_at > now() - $2::interval`,
		prefix, gfCaptchaInterval()).Scan(&stored, &fresh)
	if err != nil || !fresh { // an expired challenge is removed as well, but never accepted
		return false
	}
	return hmac.Equal([]byte(stored), []byte(s.gontactCaptchaHash(prefix, answer)))
}

// gfCaptchaInterval is gfCaptchaTTL as a PostgreSQL interval.
func gfCaptchaInterval() string {
	return strconv.Itoa(int(gfCaptchaTTL.Seconds())) + " seconds"
}

// gontactCaptchaURL is wpcf7_captcha_url().
func (s *Server) gontactCaptchaURL(ctx context.Context, prefix string) string {
	return s.links(ctx).HomeURL(gfCaptchaPath + "/" + prefix + ".png")
}

// gontactCaptchaImage serves GET /wp-content/gontact-captcha/{prefix}.png, never cached.
func (s *Server) gontactCaptchaImage(c *gin.Context) {
	c.Header("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
	c.Header("X-Robots-Tag", "noindex")
	prefix, ok := strings.CutSuffix(c.Param("file"), ".png")
	if !ok || !gfCaptchaPrefixRe.MatchString(prefix) {
		c.Status(http.StatusNotFound)
		return
	}
	var img []byte
	err := s.store.Pool.QueryRow(c, `SELECT image FROM gontact_captcha WHERE prefix = $1 AND created_at > now() - $2::interval`,
		prefix, gfCaptchaInterval()).Scan(&img)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			slog.Error("gontact captcha image", "err", err)
		}
		c.Status(http.StatusNotFound)
		return
	}
	c.Data(http.StatusOK, "image/png", img)
}

// gontactCaptchaCleanup is the gontact_captcha_cleanup event (wpcf7_cleanup_captcha_files()).
func (s *Server) gontactCaptchaCleanup(ctx context.Context) error {
	_, err := s.store.Pool.Exec(ctx, `DELETE FROM gontact_captcha WHERE created_at < now() - $1::interval`, gfCaptchaInterval())
	if err != nil {
		return err
	}
	return s.gontactEntriesCleanup(ctx) // the hourly event also applies the entries retention
}

// gontactCaptchaRefill is wpcf7_captcha_ajax_refill(): a new image URL per [captchac] tag.
func (s *Server) gontactCaptchaRefill(ctx context.Context, tags []*gfTag) map[string]string {
	out := map[string]string{}
	for _, t := range tags {
		if t.BaseType != "captchac" || t.Name == "" {
			continue
		}
		prefix, err := s.gontactCaptchaNew(ctx, gfCaptchaOptions(t))
		if err != nil {
			slog.Error("gontact captcha", "err", err)
			continue
		}
		out[t.Name] = s.gontactCaptchaURL(ctx, prefix)
	}
	return out
}

// captchacHTML is wpcf7_captchac_form_tag_handler(): the challenge prefix and the image, plus a
// refresh button the script shows. The button is a span with role="button": forms usually wrap the
// image and the answer field in one <label>, and a real <button> would become the labelled control.
func (r *gfRender) captchacHTML(t *gfTag) string {
	o := gfCaptchaOptions(t)
	ctx := r.ctx
	if r.c != nil {
		ctx = r.c // carries the client network for the per-network limit
	}
	prefix, err := r.s.gontactCaptchaNew(ctx, o)
	if err != nil {
		slog.Error("gontact captcha", "err", err)
		return ""
	}
	class := gfControlsClass(t.Type) + " wpcf7-captcha-" + strings.ReplaceAll(t.Name, ":", "")
	a := &gfAtts{}
	a.add("class", t.classOption(class)).add("id", t.idOption()).add("width", strconv.Itoa(o.W)).add("height", strconv.Itoa(o.H))
	a.add("alt", "CAPTCHA: type the characters shown in this image").add("src", r.s.gontactCaptchaURL(r.ctx, prefix))
	return `<input type="hidden" name="_wpcf7_captcha_challenge_` + html.EscapeString(t.Name) + `" value="` + prefix + `" />` +
		`<img` + a.String() + ` /><span role="button" tabindex="0" class="wpcf7-captcha-refresh" data-captcha="` + html.EscapeString(t.Name) +
		`" title="Show a new image" hidden><span aria-hidden="true">&#8635;</span><span class="screen-reader-text">Show a new CAPTCHA image</span></span>`
}

// captcharHTML is wpcf7_captchar_form_tag_handler(): the text field for the code.
func (r *gfRender) captcharHTML(t *gfTag) string {
	class := gfControlsClass(t.Type, "wpcf7-text", r.notValid(t))
	a := &gfAtts{}
	maxl, minl := t.maxlengthOption(""), t.minlengthOption()
	if gfAtoi(maxl) > 0 && gfAtoi(minl) > 0 && gfAtoi(maxl) < gfAtoi(minl) {
		maxl, minl = "", ""
	}
	a.add("size", t.sizeOption("40")).add("maxlength", maxl).add("minlength", minl)
	a.add("class", t.classOption(class)).add("id", t.idOption()).add("tabindex", t.option1("tabindex", "signed_int")).add("autocomplete", "off")
	r.ariaState(a, t, false)
	value := first(t.Values)
	if r.sub != nil {
		value = ""
	}
	if t.has("placeholder") || t.has("watermark") {
		a.add("placeholder", value)
		value = ""
	}
	a.add("value", value).add("type", "text").add("name", t.Name)
	return r.wrap(t, "<input"+a.String()+" />")
}
