package server

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"wpgo/internal/config"
	"wpgo/internal/metrics"
)

func TestServeMetricsAuth(t *testing.T) {
	get := func(s *Server, auth string) bool {
		r := httptest.NewRequest("GET", "/metrics", nil)
		if auth != "" {
			r.Header.Set("Authorization", auth)
		}
		w := httptest.NewRecorder()
		ok := s.serveMetrics(w, r)
		if ok && w.Code != 200 {
			t.Errorf("served with status %d", w.Code)
		}
		return ok
	}
	on := &Server{cfg: config.Config{MetricsToken: "s3cret"}, metrics: metrics.New("t")}
	if !get(on, "Bearer s3cret") {
		t.Error("valid token refused")
	}
	for _, a := range []string{"", "Bearer nope", "s3cret", "Basic s3cret"} {
		if get(on, a) {
			t.Errorf("served with Authorization %q", a)
		}
	}
	if get(&Server{cfg: config.Config{MetricsToken: "s3cret"}}, "Bearer s3cret") {
		t.Error("served with metrics off")
	}
	if get(&Server{cfg: config.Config{}, metrics: metrics.New("t")}, "Bearer ") {
		t.Error("served without a token")
	}
	if get(&Server{cfg: config.Config{MetricsToken: "s3cret", MetricsAddr: "127.0.0.1:9100"}, metrics: metrics.New("t")}, "Bearer s3cret") {
		t.Error("served on the site listener although PURSAE_METRICS_ADDR is set")
	}
}

func TestInstrumentOffIsIdentity(t *testing.T) {
	h := http.NotFoundHandler()
	s := &Server{}
	if got := s.instrument(h); !sameHandler(got, h) {
		t.Error("instrument wrapped the handler although metrics are off")
	}
}

func sameHandler(a, b http.Handler) bool {
	return reflect.ValueOf(a).Pointer() == reflect.ValueOf(b).Pointer()
}
