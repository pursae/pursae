package server

import (
	"strings"

	"github.com/gin-gonic/gin"
)

// noStoreCacheControl is what a private response gets when its handler chose no Cache-Control.
const noStoreCacheControl = "no-store, private"

// noStoreWriter gives a response Cache-Control: no-store, private when it is meant for its requester
// alone and its handler set no Cache-Control of its own (static assets, uploads and downloads keep
// theirs): wp-admin and wp-login pages, and every response to a signed-in request, that is one with a
// valid session cookie or REST credentials (an Application Password, an OAuth access token). A browser
// or a shared cache must not keep one user's screens, nonces or API data for the next person at that
// computer or behind that proxy; WordPress sends nocache_headers() on its admin screens for the same
// reason. The decision is made when the status is set or the first byte goes out: by then the session
// (loadUser) and the REST credentials (restAuthUser) are known.
type noStoreWriter struct {
	gin.ResponseWriter
	c      *gin.Context
	admin  bool // a wp-admin or wp-login path
	static bool // a static asset file (fwStaticRequest) under the asset trees
	done   bool
}

func newNoStoreWriter(c *gin.Context) *noStoreWriter {
	p := c.Request.URL.Path
	admin := p == "/wp-admin" || strings.HasPrefix(p, "/wp-admin/") || p == "/wp-login" || strings.HasPrefix(p, "/wp-login.")
	static := fwStaticRequest(c.Request) && (strings.HasPrefix(p, "/wp-content/") || strings.HasPrefix(p, "/wp-includes/") ||
		strings.HasPrefix(p, "/assets/") || strings.HasPrefix(p, "/wp-admin/css/") || strings.HasPrefix(p, "/wp-admin/js/") || strings.HasPrefix(p, "/wp-admin/images/"))
	return &noStoreWriter{ResponseWriter: c.Writer, c: c, admin: admin && !static, static: static}
}

func (w *noStoreWriter) apply() {
	if w.done {
		return
	}
	w.done = true
	h := w.ResponseWriter.Header()
	if h.Get("Cache-Control") != "" {
		return
	}
	if w.static {
		return // the same file for everybody: a signed-in visitor's browser may cache it like anyone's (B9)
	}
	if w.admin || currentUser(w.c) != nil || restCurrent(w.c) != nil {
		h.Set("Cache-Control", noStoreCacheControl)
	}
}

func (w *noStoreWriter) WriteHeader(code int) {
	w.apply()
	w.ResponseWriter.WriteHeader(code)
}

func (w *noStoreWriter) WriteHeaderNow() {
	w.apply()
	w.ResponseWriter.WriteHeaderNow()
}

func (w *noStoreWriter) Write(b []byte) (int, error) {
	w.apply()
	return w.ResponseWriter.Write(b)
}

func (w *noStoreWriter) WriteString(s string) (int, error) {
	w.apply()
	return w.ResponseWriter.WriteString(s)
}

func (w *noStoreWriter) Flush() {
	w.apply()
	w.ResponseWriter.Flush()
}
