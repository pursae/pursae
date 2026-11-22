// Package metrics is Pursae's Prometheus endpoint. It is written to cost no memory worth measuring:
// a fixed set of atomic counters (route group x status class, never a URL or a user), a fixed-bucket
// duration histogram, and everything else (database pool, Go runtime) read at scrape time and not
// kept. Nothing grows with traffic. It is off unless PURSAE_METRICS=1; when off, the request
// middleware is not installed at all.
package metrics

import (
	"bufio"
	"errors"
	"io"
	"net"
	"net/http"
	"runtime"
	rtm "runtime/metrics"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Route groups and status classes: the only labels a request series carries.
const (
	GroupAdmin = iota
	GroupREST
	GroupFrontend
	GroupStatic
	numGroups
)

const numClasses = 4 // 2xx (and 1xx), 3xx, 4xx, 5xx

var groupNames = [numGroups]string{"admin", "rest", "frontend", "static"}
var classNames = [numClasses]string{"2xx", "3xx", "4xx", "5xx"}

// bucketBounds are the upper bounds of the duration histogram in seconds; one more slot holds +Inf.
var bucketBounds = [...]float64{0.005, 0.025, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}
var bucketNanos = [...]int64{5e6, 25e6, 100e6, 250e6, 500e6, 1e9, 2.5e9, 5e9, 10e9}

const numBuckets = len(bucketBounds) + 1

// Metrics holds every counter. The zero value is not usable; call New.
type Metrics struct {
	buckets [numGroups][numClasses][numBuckets]atomic.Uint64 // not cumulative; summed at scrape
	sumNs   [numGroups][numClasses]atomic.Uint64
	active  atomic.Int64
	slow    atomic.Uint64
	started time.Time
	version string

	mu   sync.Mutex
	pool func() *pgxpool.Stat
}

// New returns an empty collector. version is exposed in pursae_build_info.
func New(version string) *Metrics { return &Metrics{started: time.Now(), version: version} }

// SetPool registers the function that reads the database pool statistics at scrape time
// (pool.Stat).
func (m *Metrics) SetPool(f func() *pgxpool.Stat) {
	m.mu.Lock()
	m.pool = f
	m.mu.Unlock()
}

// SlowQuery counts one query slower than the configured threshold.
func (m *Metrics) SlowQuery() { m.slow.Add(1) }

// Group classifies a request path into one of the fixed route groups.
func Group(path string) int {
	switch {
	case hasPrefix(path, "/wp-content/") || hasPrefix(path, "/wp-includes/") ||
		hasPrefix(path, "/wp-admin/assets/") || hasPrefix(path, "/wp-admin/js/") ||
		hasPrefix(path, "/wp-admin/css/") || hasPrefix(path, "/wp-admin/images/"):
		return GroupStatic
	case path == "/wp-json" || hasPrefix(path, "/wp-json/"):
		return GroupREST
	case path == "/wp-admin" || hasPrefix(path, "/wp-admin/") || hasPrefix(path, "/wp-login"):
		return GroupAdmin
	}
	return GroupFrontend
}

func hasPrefix(s, p string) bool { return len(s) >= len(p) && s[:len(p)] == p }

func class(status int) int {
	switch {
	case status >= 500:
		return 3
	case status >= 400:
		return 2
	case status >= 300:
		return 1
	}
	return 0
}

func (m *Metrics) observe(group, status int, d time.Duration) {
	c := class(status)
	n := d.Nanoseconds()
	b := len(bucketNanos)
	for i, le := range bucketNanos {
		if n <= le {
			b = i
			break
		}
	}
	m.buckets[group][c][b].Add(1)
	m.sumNs[group][c].Add(uint64(n))
}

// Middleware counts and times every request of next. Install it only when metrics are on.
func (m *Metrics) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		m.active.Add(1)
		sw := writerPool.Get().(*statusWriter)
		sw.ResponseWriter, sw.status = w, 0
		defer func() {
			m.active.Add(-1)
			st := sw.status
			if st == 0 {
				st = http.StatusOK
			}
			m.observe(Group(r.URL.Path), st, time.Since(start))
			sw.ResponseWriter = nil
			writerPool.Put(sw)
		}()
		next.ServeHTTP(sw, r)
	})
}

var writerPool = sync.Pool{New: func() any { return new(statusWriter) }}

// statusWriter records the status code and keeps the optional interfaces of the real writer
// (streaming, websocket upgrades, sendfile) working.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 && (code >= 200 || code == http.StatusSwitchingProtocols) {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(p)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		if w.status == 0 {
			w.status = http.StatusOK
		}
		f.Flush()
	}
}

func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("metrics: the response writer does not support hijacking")
	}
	if w.status == 0 {
		w.status = http.StatusSwitchingProtocols
	}
	return h.Hijack()
}

type onlyWriter struct{ io.Writer }

func (w *statusWriter) ReadFrom(r io.Reader) (int64, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if rf, ok := w.ResponseWriter.(io.ReaderFrom); ok {
		return rf.ReadFrom(r)
	}
	return io.Copy(onlyWriter{w.ResponseWriter}, r)
}

// runtime/metrics samples exposed as gauges/counters: name, Prometheus name, help, type.
var runtimeSamples = []struct{ src, name, help, typ string }{
	{"/sched/goroutines:goroutines", "go_goroutines", "Number of live goroutines.", "gauge"},
	{"/sched/gomaxprocs:threads", "go_gomaxprocs", "Current GOMAXPROCS.", "gauge"},
	{"/memory/classes/total:bytes", "go_memory_total_bytes", "All memory mapped by the Go runtime.", "gauge"},
	{"/memory/classes/heap/objects:bytes", "go_heap_objects_bytes", "Heap memory occupied by live and not yet swept objects.", "gauge"},
	{"/gc/heap/goal:bytes", "go_gc_heap_goal_bytes", "Heap size target of the next GC cycle.", "gauge"},
	{"/gc/heap/allocs:bytes", "go_heap_allocs_bytes_total", "Cumulative bytes allocated on the heap.", "counter"},
	{"/gc/cycles/total:gc-cycles", "go_gc_cycles_total", "Completed GC cycles.", "counter"},
}

// Handler serves the Prometheus text format (version 0.0.4). The buffer is built per scrape and
// dropped with it.
func (m *Metrics) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		if r.Method == http.MethodHead {
			return
		}
		_, _ = w.Write(m.render(nil))
	})
}

func (m *Metrics) render(b []byte) []byte {
	b = append(b, "# HELP pursae_build_info Build information; the value is always 1.\n# TYPE pursae_build_info gauge\n"...)
	b = append(b, `pursae_build_info{version="`...)
	b = appendEsc(b, m.version)
	b = append(b, `",go_version="`...)
	b = appendEsc(b, runtime.Version())
	b = append(b, "\"} 1\n"...)
	b = head(b, "pursae_start_time_seconds", "Start time of the process, Unix seconds.", "gauge")
	b = appendInt(append(b, "pursae_start_time_seconds "...), m.started.Unix())
	b = append(b, '\n')

	// Snapshot first so count, sum and buckets of one series agree.
	var snap [numGroups][numClasses][numBuckets]uint64
	for g := range snap {
		for c := range snap[g] {
			for i := range snap[g][c] {
				snap[g][c][i] = m.buckets[g][c][i].Load()
			}
		}
	}
	b = head(b, "pursae_http_requests_total", "HTTP requests served, by route group and status class.", "counter")
	for g := range snap {
		for c := range snap[g] {
			var n uint64
			for _, v := range snap[g][c] {
				n += v
			}
			b = append(b, `pursae_http_requests_total{group="`...)
			b = append(b, groupNames[g]...)
			b = append(b, `",class="`...)
			b = append(b, classNames[c]...)
			b = append(b, `"} `...)
			b = strconv.AppendUint(b, n, 10)
			b = append(b, '\n')
		}
	}
	b = head(b, "pursae_http_request_duration_seconds", "HTTP request duration, by route group and status class (series with no request yet are left out).", "histogram")
	for g := range snap {
		for c := range snap[g] {
			var n uint64
			for _, v := range snap[g][c] {
				n += v
			}
			if n == 0 {
				continue
			}
			lbl := `group="` + groupNames[g] + `",class="` + classNames[c] + `"`
			var cum uint64
			for i := 0; i < numBuckets; i++ {
				cum += snap[g][c][i]
				le := "+Inf"
				if i < len(bucketBounds) {
					le = strconv.FormatFloat(bucketBounds[i], 'g', -1, 64)
				}
				b = append(b, "pursae_http_request_duration_seconds_bucket{"...)
				b = append(b, lbl...)
				b = append(b, `,le="`...)
				b = append(b, le...)
				b = append(b, `"} `...)
				b = strconv.AppendUint(b, cum, 10)
				b = append(b, '\n')
			}
			b = append(b, "pursae_http_request_duration_seconds_sum{"...)
			b = append(b, lbl...)
			b = append(b, "} "...)
			b = strconv.AppendFloat(b, float64(m.sumNs[g][c].Load())/1e9, 'g', -1, 64)
			b = append(b, "\npursae_http_request_duration_seconds_count{"...)
			b = append(b, lbl...)
			b = append(b, "} "...)
			b = strconv.AppendUint(b, n, 10)
			b = append(b, '\n')
		}
	}
	b = head(b, "pursae_http_requests_in_flight", "HTTP requests being served right now.", "gauge")
	b = appendInt(append(b, "pursae_http_requests_in_flight "...), m.active.Load())
	b = append(b, '\n')
	b = head(b, "pursae_db_slow_queries_total", "Queries slower than PURSAE_SLOW_QUERY (always 0 when it is unset).", "counter")
	b = strconv.AppendUint(append(b, "pursae_db_slow_queries_total "...), m.slow.Load(), 10)
	b = append(b, '\n')

	m.mu.Lock()
	pf := m.pool
	m.mu.Unlock()
	if pf != nil {
		if s := pf(); s != nil {
			b = gauge(b, "pursae_db_pool_connections", "Connections in the database pool by state.", "")
			b = appendKV(b, "pursae_db_pool_connections", `state="acquired"`, int64(s.AcquiredConns()))
			b = appendKV(b, "pursae_db_pool_connections", `state="idle"`, int64(s.IdleConns()))
			b = appendKV(b, "pursae_db_pool_connections", `state="constructing"`, int64(s.ConstructingConns()))
			b = gauge(b, "pursae_db_pool_max_connections", "Pool size limit.", "")
			b = appendKV(b, "pursae_db_pool_max_connections", "", int64(s.MaxConns()))
			b = head(b, "pursae_db_pool_acquires_total", "Connection acquires from the pool.", "counter")
			b = appendKV(b, "pursae_db_pool_acquires_total", "", s.AcquireCount())
			b = head(b, "pursae_db_pool_empty_acquires_total", "Acquires that had to wait because no connection was idle.", "counter")
			b = appendKV(b, "pursae_db_pool_empty_acquires_total", "", s.EmptyAcquireCount())
			b = head(b, "pursae_db_pool_canceled_acquires_total", "Acquires canceled while waiting.", "counter")
			b = appendKV(b, "pursae_db_pool_canceled_acquires_total", "", s.CanceledAcquireCount())
			b = head(b, "pursae_db_pool_acquire_wait_seconds_total", "Total time spent acquiring connections.", "counter")
			b = append(b, "pursae_db_pool_acquire_wait_seconds_total "...)
			b = strconv.AppendFloat(b, s.AcquireDuration().Seconds(), 'g', -1, 64)
			b = append(b, '\n')
		}
	}

	samples := make([]rtm.Sample, len(runtimeSamples))
	for i := range samples {
		samples[i].Name = runtimeSamples[i].src
	}
	rtm.Read(samples)
	for i, s := range samples {
		var v float64
		switch s.Value.Kind() {
		case rtm.KindUint64:
			v = float64(s.Value.Uint64())
		case rtm.KindFloat64:
			v = s.Value.Float64()
		default:
			continue // not provided by this Go version
		}
		rs := runtimeSamples[i]
		b = head(b, rs.name, rs.help, rs.typ)
		b = append(b, rs.name...)
		b = append(b, ' ')
		b = strconv.AppendFloat(b, v, 'g', -1, 64)
		b = append(b, '\n')
	}
	return b
}

func head(b []byte, name, help, typ string) []byte {
	b = append(b, "# HELP "...)
	b = append(b, name...)
	b = append(b, ' ')
	b = append(b, help...)
	b = append(b, "\n# TYPE "...)
	b = append(b, name...)
	b = append(b, ' ')
	b = append(b, typ...)
	return append(b, '\n')
}

func gauge(b []byte, name, help, _ string) []byte { return head(b, name, help, "gauge") }

func appendKV(b []byte, name, labels string, v int64) []byte {
	b = append(b, name...)
	if labels != "" {
		b = append(b, '{')
		b = append(b, labels...)
		b = append(b, '}')
	}
	b = append(b, ' ')
	b = strconv.AppendInt(b, v, 10)
	return append(b, '\n')
}

func appendInt(b []byte, v int64) []byte { return strconv.AppendInt(b, v, 10) }

// appendEsc escapes a label value (backslash, quote, newline).
func appendEsc(b []byte, s string) []byte {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			b = append(b, `\\`...)
		case '"':
			b = append(b, `\"`...)
		case '\n':
			b = append(b, `\n`...)
		default:
			b = append(b, s[i])
		}
	}
	return b
}
