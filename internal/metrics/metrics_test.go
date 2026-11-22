package metrics

import (
	"bufio"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestGroup(t *testing.T) {
	for path, want := range map[string]int{
		"/":                          GroupFrontend,
		"/hello-world/":              GroupFrontend,
		"/wp-admin/":                 GroupAdmin,
		"/wp-admin/edit":             GroupAdmin,
		"/wp-login.php":              GroupAdmin,
		"/wp-json/wp/v2/posts":       GroupREST,
		"/wp-json":                   GroupREST,
		"/wp-admin/assets/css/a.css": GroupStatic,
		"/wp-content/uploads/a.jpg":  GroupStatic,
		"/wp-includes/js/x.js":       GroupStatic,
		"/wp-jsonx":                  GroupFrontend,
	} {
		if got := Group(path); got != want {
			t.Errorf("Group(%q) = %d, want %d", path, got, want)
		}
	}
}

func TestMiddlewareAndRender(t *testing.T) {
	m := New("1.2.3")
	h := m.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/wp-json/x":
			w.WriteHeader(http.StatusNotFound)
		case "/boom":
			http.Error(w, "x", 500)
		case "/moved":
			http.Redirect(w, r, "/", 301)
		case "/slow":
			time.Sleep(30 * time.Millisecond)
		}
		_, _ = w.Write([]byte("ok"))
	}))
	for _, p := range []string{"/", "/", "/wp-json/x", "/boom", "/moved", "/slow", "/wp-admin/"} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", p, nil))
	}
	m.SlowQuery()
	out := string(m.render(nil))
	for _, want := range []string{
		`pursae_build_info{version="1.2.3",go_version="` + runtime.Version() + `"} 1`,
		`pursae_http_requests_total{group="frontend",class="2xx"} 3`,
		`pursae_http_requests_total{group="rest",class="4xx"} 1`,
		`pursae_http_requests_total{group="frontend",class="5xx"} 1`,
		`pursae_http_requests_total{group="frontend",class="3xx"} 1`,
		`pursae_http_requests_total{group="admin",class="2xx"} 1`,
		`pursae_http_request_duration_seconds_bucket{group="frontend",class="2xx",le="+Inf"} 3`,
		`pursae_http_request_duration_seconds_count{group="frontend",class="2xx"} 3`,
		`pursae_http_requests_in_flight 0`,
		`pursae_db_slow_queries_total 1`,
		`go_goroutines `,
		`go_heap_objects_bytes `,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q", want)
		}
	}
	// The 30 ms request lands in the 0.05... bucket range: le=0.025 holds 2 (the fast ones), le=0.1 all 3.
	if !strings.Contains(out, `le="0.025"} 2`) || !strings.Contains(out, `le="0.1"} 3`) {
		t.Errorf("histogram buckets wrong:\n%s", out)
	}
	if strings.Contains(out, "/wp-json") || strings.Contains(out, "boom") {
		t.Error("a URL leaked into the labels")
	}
}

func TestHandlerMethods(t *testing.T) {
	m := New("x")
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/metrics", nil))
	if rec.Code != 405 {
		t.Errorf("POST = %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain; version=0.0.4") {
		t.Errorf("GET = %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	// every non-comment line is "name{labels} value"
	sc := bufio.NewScanner(rec.Body)
	for sc.Scan() {
		l := sc.Text()
		if l == "" || l[0] == '#' {
			continue
		}
		if i := strings.LastIndexByte(l, ' '); i < 1 {
			t.Errorf("bad line %q", l)
		}
	}
}

func TestStatusWriterKeepsInterfaces(t *testing.T) {
	m := New("x")
	h := m.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := w.(http.Flusher); !ok {
			t.Error("no Flusher")
		}
		if _, ok := w.(http.Hijacker); !ok {
			t.Error("no Hijacker")
		}
		if http.NewResponseController(w).Flush() != nil {
			t.Error("ResponseController.Flush failed")
		}
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
}

func TestMiddlewareNoAllocs(t *testing.T) {
	m := New("x")
	h := m.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	r := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	if n := testing.AllocsPerRun(1000, func() { h.ServeHTTP(w, r) }); n > 0 {
		t.Errorf("middleware allocates %v per request", n)
	}
}

func BenchmarkMiddleware(b *testing.B) {
	m := New("x")
	h := m.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	r := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			h.ServeHTTP(w, r)
		}
	})
}

// TestHeapFootprint: the collector is a fixed block of counters; a million requests add nothing.
func TestHeapFootprint(t *testing.T) {
	heap := func() int64 {
		runtime.GC()
		runtime.GC()
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		return int64(ms.HeapAlloc)
	}
	before := heap()
	m := New("x")
	h := m.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	r := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	for i := 0; i < 1_000_000; i++ {
		h.ServeHTTP(w, r)
	}
	_ = m.render(nil)
	after := heap()
	t.Logf("heap with metrics after 1M requests and a scrape: %+d bytes", after-before)
	if after-before > 32<<10 {
		t.Errorf("metrics retain %d bytes", after-before)
	}
	runtime.KeepAlive(m)
}
