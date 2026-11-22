package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"

	"wpgo/internal/metrics"
)

// UseMetrics turns the Prometheus endpoint on (PURSAE_METRICS=1, internal/metrics). Call it before
// Handler. Without it nothing is installed: no middleware, no route, no cost.
func (s *Server) UseMetrics(m *metrics.Metrics) { s.metrics = m }

// instrument wraps h with the request counters when metrics are on, else returns h itself.
func (s *Server) instrument(h http.Handler) http.Handler {
	if s.metrics == nil {
		return h
	}
	return s.metrics.Middleware(h)
}

// serveMetrics answers /metrics on the site listener. It only does so when a token is configured
// and the request carries it as a bearer token; in every other case (metrics off, no token, wrong
// or missing token, a separate PURSAE_METRICS_ADDR listener in use) it reports false and the
// request goes on to the site, so a page called "metrics" still works and nothing is disclosed.
func (s *Server) serveMetrics(w http.ResponseWriter, r *http.Request) bool {
	if s.metrics == nil || s.cfg.MetricsAddr != "" || s.cfg.MetricsToken == "" {
		return false
	}
	if !MetricsTokenOK(r, s.cfg.MetricsToken) {
		return false
	}
	s.metrics.Handler().ServeHTTP(w, r)
	return true
}

// MetricsTokenOK reports whether r carries token as "Authorization: Bearer <token>".
func MetricsTokenOK(r *http.Request, token string) bool {
	h := r.Header.Get("Authorization")
	if len(h) < 8 || !strings.EqualFold(h[:7], "bearer ") {
		return false
	}
	a, b := sha256.Sum256([]byte(strings.TrimSpace(h[7:]))), sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(a[:], b[:]) == 1
}
