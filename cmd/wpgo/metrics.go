package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"wpgo/internal/config"
	"wpgo/internal/db"
	"wpgo/internal/metrics"
	"wpgo/internal/server"
)

// setupMetrics prepares the optional metrics endpoint and slow query log (docs/developer/operations/metrics.md).
// Call it before db.Connect: the slow query tracer must be on the pool's connections. It returns nil
// when metrics are off, which is the default; PURSAE_METRICS=1 without PURSAE_METRICS_ADDR or
// PURSAE_METRICS_TOKEN stays off too, so the counters are never readable by anyone on the public site.
func setupMetrics(cfg config.Config) *metrics.Metrics {
	var mx *metrics.Metrics
	if cfg.MetricsOn {
		if cfg.MetricsAddr == "" && cfg.MetricsToken == "" {
			log.Printf("warning: PURSAE_METRICS=1 is ignored: set PURSAE_METRICS_ADDR (own listener, e.g. 127.0.0.1:9100) or PURSAE_METRICS_TOKEN (bearer token for /metrics on the site)")
		} else {
			mx = metrics.New(server.Version)
		}
	}
	if cfg.SlowQuery > 0 {
		t := &metrics.SlowQueryTracer{Threshold: cfg.SlowQuery}
		if mx != nil {
			t.OnSlow = mx.SlowQuery
		}
		db.QueryTracer = t
	}
	return mx
}

// serveMetrics starts the separate metrics listener when PURSAE_METRICS_ADDR is set; the bearer
// token, when also set, is required there too. It stops with ctx.
func serveMetrics(ctx context.Context, cfg config.Config, mx *metrics.Metrics, pool *pgxpool.Pool) {
	if mx == nil {
		return
	}
	mx.SetPool(pool.Stat)
	if cfg.MetricsAddr == "" {
		return
	}
	h := mx.Handler()
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		if cfg.MetricsToken != "" && !server.MetricsTokenOK(r, cfg.MetricsToken) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="metrics"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	})
	srv := &http.Server{Addr: cfg.MetricsAddr, Handler: mux, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	go func() {
		log.Printf("metrics on %s/metrics", cfg.MetricsAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("metrics listener: %v", err)
		}
	}()
}
