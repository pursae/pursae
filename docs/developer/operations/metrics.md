---
title: Metrics and slow queries
description: The optional Prometheus endpoint (PURSAE_METRICS), what it exposes, how to keep it private, its memory cost, and the slow query log.
weight: 75
section: operations
---

Pursae can expose its health as [Prometheus](https://prometheus.io/) metrics and log the database
queries that take too long. Both are **off by default**; while off they cost nothing (the request
counters are not even installed). Neither adds a dependency: the endpoint is a small writer of the
Prometheus text format inside the binary.

Added in 0.5.1. The metric names and labels below are stable from then on (see
[compatibility](../compatibility.md)); new metrics may be added.

## Turning it on

| Variable | Meaning |
|----------|---------|
| `PURSAE_METRICS=1` | enables the counters and the endpoint. **Not enough alone**: with neither of the two below set, Pursae logs a warning and stays off, so the counters are never readable by anyone on the public site |
| `PURSAE_METRICS_ADDR` | serve `/metrics` on its own listener (`127.0.0.1:9100`, `10.0.0.5:9100`), separate from the site. Not reachable from the internet unless you bind a public address; keep it on loopback or a private network |
| `PURSAE_METRICS_TOKEN` | a bearer token: scrapers send `Authorization: Bearer <token>`. With `PURSAE_METRICS_ADDR` it protects that listener too; without it, `/metrics` is served **on the site's own port**, and only to a request that carries the token. Any other request to `/metrics` (no header, wrong token) continues to the site as if the endpoint did not exist, so a page whose slug is `metrics` keeps working |
| `PURSAE_SLOW_QUERY` | log and count queries at least this slow: a duration (`250ms`, `1s`) or whole milliseconds (`250`). Unset, `0` or invalid: off. Independent of `PURSAE_METRICS` |

Prometheus example (own listener, token):

```yaml
scrape_configs:
  - job_name: pursae
    authorization: { credentials: change-me }   # PURSAE_METRICS_TOKEN
    static_configs: [{ targets: ["127.0.0.1:9100"] }]
```

Behind a reverse proxy, prefer `PURSAE_METRICS_ADDR` on a private address and do not proxy it. If you
use the token on the site port instead, use a long random token and TLS.

## What it exposes

Labels are fixed: a **route group** (`admin` = `/wp-admin`, `/wp-login`; `rest` = `/wp-json`;
`static` = `/wp-content`, `/wp-includes` and the admin asset directories; `frontend` = everything
else) and a **status class** (`2xx`, `3xx`, `4xx`, `5xx`). There is never a URL, user or query in a
label, so the number of series cannot grow with traffic.

| Metric | Type | Meaning |
|--------|------|---------|
| `pursae_http_requests_total{group,class}` | counter | requests served |
| `pursae_http_request_duration_seconds{group,class}` | histogram | duration; buckets 0.005, 0.025, 0.1, 0.25, 0.5, 1, 2.5, 5, 10 s and `+Inf` (series without a request yet are left out). Long-lived connections (SSE, WebSocket) are counted when they end. `/healthz`, `/readyz` and `/metrics` itself are not counted |
| `pursae_http_requests_in_flight` | gauge | requests being served right now |
| `pursae_db_pool_connections{state}` | gauge | pool connections: `acquired`, `idle`, `constructing` |
| `pursae_db_pool_max_connections` | gauge | pool size limit |
| `pursae_db_pool_acquires_total`, `…_empty_acquires_total`, `…_canceled_acquires_total`, `pursae_db_pool_acquire_wait_seconds_total` | counters | pool pressure: acquires, those that had to wait, those canceled, total wait |
| `pursae_db_slow_queries_total` | counter | queries slower than `PURSAE_SLOW_QUERY` (0 when unset) |
| `go_goroutines`, `go_gomaxprocs`, `go_memory_total_bytes`, `go_heap_objects_bytes`, `go_gc_heap_goal_bytes`, `go_heap_allocs_bytes_total`, `go_gc_cycles_total` | gauges, counters | Go runtime, read from `runtime/metrics` at scrape time |
| `pursae_build_info{version,go_version}` | gauge | always 1 |
| `pursae_start_time_seconds` | gauge | process start, Unix seconds |

The pool and runtime figures are read when Prometheus scrapes and are not stored.

## Memory cost

The collector is one fixed block of atomic counters (16 route/status series × 10 histogram buckets,
sums, and a few gauges: about 1.5 KB) plus a pooled response-writer wrapper. A request allocates
nothing (0 B/op, about 140 ns), a scrape builds its text in a buffer that is dropped afterwards,
and nothing depends on traffic. Measured with `go test ./internal/metrics -run TestHeapFootprint -v`:
about **12 KB** of live heap after a million requests and a scrape, test harness included.
With `PURSAE_SLOW_QUERY` set, each database query allocates one small context value (about 100 bytes,
garbage at once).

## Slow query log

With `PURSAE_SLOW_QUERY=250ms` a query that takes 250 ms or more writes a line like

```
slow query 312ms: SELECT … FROM wp_posts WHERE …
```

The SQL text is shown with whitespace collapsed and cut to 300 bytes. **Arguments are never logged**
(they can hold passwords or personal data). At most 10 lines are written per second so a database in
trouble cannot flood the log; `pursae_db_slow_queries_total` counts every slow query. The line format
is not part of the stability promise.
