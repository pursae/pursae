---
title: Docker
description: The image, the compose file, volumes, environment and running behind another compose stack.
weight: 20
section: operations
---

## The image

`docker/Dockerfile` is a two-stage build: `golang:1.27-alpine` compiles a static binary, the
final stage is `alpine:3.22` with `ca-certificates` and `tzdata`, an unprivileged user `wpgo`
(uid 10001) and the data directories `/data/uploads`, `/data/plugins`, `/data/plugin-cache`,
`/data/mail`, `/data/themes`, `/data/wpgo-cache`, `/data/languages`, `/data/bin` and `/data/backups`.
The entrypoint is `pursae launch` (it installs the binary into `/data/bin` and runs it from there, see
[upgrading](../getting-started/upgrading.md#docker)), the default command `serve`, the exposed port 8080.
The binary is on the `PATH` as both `wpgo` and `pursae`, so `docker compose exec wpgo pursae help` lists the
[command line](cli.md). Example plugin packages from `examples/plugins/dist` are copied to
`/usr/share/wpgo/plugins` if they exist.

```bash
docker build -f docker/Dockerfile -t wpgo:local .
docker run --rm -p 8080:8080 \
  -e DATABASE_URL='postgres://wpgo:wpgo@db:5432/wpgo?sslmode=disable' \
  -e PURSAE_SITE_URL=https://blog.example.com \
  -e PURSAE_UPLOADS_DIR=/data/uploads \
  -v wpgo-uploads:/data/uploads -v wpgo-plugins:/data/plugins -v wpgo-themes:/data/themes \
  wpgo:local
```

## compose.yaml

`docker/compose.yaml` defines three services:

| Service | Purpose |
|---------|---------|
| `db` | `postgres:17-alpine`, database `wpgo`, user and password `wpgo`, health-checked with `pg_isready`, published on `127.0.0.1:${PURSAE_DB_PORT:-5432}` |
| `wpgo` | the site, built from the Dockerfile (`GO_TAGS=devhub`), waits for the database to be healthy, `restart: unless-stopped` |
| `test` | profile `test`: runs `go test ./... -p 1` in a Go container against the same database (`make docker-test`; one package at a time, since the integration tests recreate `wpgo_test`) |

Environment of the `wpgo` service:

```yaml
DATABASE_URL: postgres://wpgo:wpgo@db:5432/wpgo?sslmode=disable
PURSAE_SITE_URL: ${PURSAE_SITE_URL:-http://localhost:8088}
PURSAE_UPLOADS_DIR: /data/uploads
PURSAE_TRUST_DEV_KEY: ${PURSAE_TRUST_DEV_KEY:-0}   # demo only: 1 trusts the public demo key
PURSAE_DEV_HUB_URL: ${PURSAE_DEV_HUB_URL:-http://pursae-manager.test:8090}   # devhub builds only
PURSAE_TRUST_HUB_KEYS: ${PURSAE_TRUST_HUB_KEYS:-0}                           # devhub builds only
PURSAE_TRUSTED_PROXIES: ${PURSAE_TRUSTED_PROXIES:-none}   # no proxy in development
PURSAE_SMTP_HOST: ${PURSAE_SMTP_HOST:-}      # outgoing mail; without a host: .eml files in /data/mail
PURSAE_SMTP_PORT: ${PURSAE_SMTP_PORT:-587}
PURSAE_SMTP_USER: ${PURSAE_SMTP_USER:-}
PURSAE_SMTP_PASS: ${PURSAE_SMTP_PASS:-}
PURSAE_MAIL_FROM: ${PURSAE_MAIL_FROM:-}
```

Volumes: `pgdata`, `bin` (`/data/bin`), `uploads`, `plugins`, `themes`, `pagecache` (`/data/wpgo-cache`),
`languages`, `backups` and `mail` (each under `/data/<name>` unless noted). Add anything else from the
[configuration reference](configuration.md) to the environment, for example `PURSAE_PLUGIN_KEYS`.

## Production changes

`compose.yaml` is the development setup (demo database password, PostgreSQL published on
`127.0.0.1`, the demo plugin signing key trusted, a `devhub` build that looks for a local Pursae
Manager instead of `https://hub.pursae.com`). For production `make prod-up` adds the override
`compose.prod.yaml`, which fixes all of that, with the settings from `docker/.env` (copy
`docker/.env.example`; the file is git-ignored). It refuses to start until `PURSAE_DB_PASSWORD` and
`PURSAE_SITE_URL` are set:

```bash
cp docker/.env.example docker/.env   # set PURSAE_DB_PASSWORD, PURSAE_SITE_URL, PURSAE_SMTP_*, PURSAE_MAIL_FROM
make prod-up                         # make prod-logs, make prod-down
# without make:
docker compose --env-file docker/.env -f docker/compose.yaml -f docker/compose.prod.yaml up -d --build
```

It builds without the `devhub` tag (the binary talks only to `https://hub.pursae.com`, so core updates
come from Dashboard › Updates, and `PURSAE_DEV_HUB_URL` / `PURSAE_TRUST_HUB_KEYS` are removed), takes the
database password from `PURSAE_DB_PASSWORD` (used for `POSTGRES_PASSWORD` and the `DATABASE_URL`), does
not publish PostgreSQL, publishes the site on `127.0.0.1:8088` only (for a TLS proxy on the same host;
`PURSAE_BIND` changes the address), sets `PURSAE_TRUST_DEV_KEY=0`, passes `PURSAE_PLUGIN_KEYS` and
`PURSAE_TRUSTED_PROXIES` through and restarts both services unless stopped. `POSTGRES_PASSWORD` only applies when the `pgdata` volume is first created; on
an existing database change the password first (`ALTER USER wpgo PASSWORD '...'`).

Also:

- Put a TLS-terminating proxy in front and set `PURSAE_SITE_URL` to the `https://` address
  ([reverse proxy and TLS](reverse-proxy-and-tls.md)).
- The installer asks for the setup token: open the link the server prints on its first start
  (`docker compose logs wpgo`), set `PURSAE_SETUP_TOKEN`, or print a new link with
  `docker compose exec wpgo pursae setup-token`.
- Set `PURSAE_SECRET` explicitly if you run more than one `wpgo` replica.

## Health checks

| Endpoint | Answers 200 when | Use it for |
|----------|------------------|------------|
| `GET /healthz` | the process serves HTTP (the database is not touched) | liveness: Docker `HEALTHCHECK`, a Kubernetes `livenessProbe` |
| `GET /readyz` | the database answers a ping within 2 s, every migration of the binary is applied and the binary is not older than the schema allows (`PURSAE_ALLOW_OLDER_BINARY`, [configuration](configuration.md)); `503` otherwise. A schema migrated by a newer but compatible release still answers `200`. The answer names no versions (0.6.3+); the reason is in the server log | readiness: load balancers, a Kubernetes `readinessProbe`; a core update is confirmed only once the new binary's `/readyz` answers `200` |

Both are unauthenticated, answer before maintenance mode, Pursae Security and the page cache,
send `Cache-Control: no-store` and are not written to the access log. The image's `HEALTHCHECK` runs
`pursae healthcheck` (the binary asks `/healthz` on `PURSAE_ADDR`; `pursae healthcheck ready` asks
`/readyz`), so it needs no curl; `compose.prod.yaml` declares the same check.

## Shutdown and timeouts

On SIGTERM the server stops accepting connections and lets requests in flight finish (up to 10 s),
then stops the job runner (the job in hand finishes or goes back to the queue, up to 10 s), drains
the background writes (404 log, counters, pending CDN purges; up to 5 s) and closes the database
pool. Docker's default grace period is 10 s, so `compose.prod.yaml` gives the `wpgo` service
`stop_grace_period: 35s`; set the same (or Kubernetes' `terminationGracePeriodSeconds`) when you run
the image another way.

The HTTP server limits request headers to 10 s, idle keep-alive connections to 120 s, and a request
body that makes no progress for 60 s fails. There is deliberately no overall read or write timeout:
backup downloads and imports, uploads over slow links and the Pursae Headless subscription
streams run for minutes. Put per-request limits in the reverse proxy if you need them. A request
body is limited to 512 MB (PHP's `post_max_size`; the largest upload a screen accepts is a 256 MB
WXR import), except the Backup Manager import, which takes archives of up to 16 GB; a larger body
is answered `413`. A request with neither a signed-in user nor an `Authorization` header is limited
to 32 MB, except the contact form submission endpoint and product add-on uploads (bounded by their
own file size settings) and XML-RPC.

Every response carries an `X-Request-ID` (the proxy's, when it sends a sane one, else a generated
one); the access log line ends with `id=<request id>`.

## Logs and restarts

```bash
docker compose -f docker/compose.yaml logs -f wpgo   # production: make prod-logs
docker compose -f docker/compose.yaml restart wpgo
```

The `wpgo` service has `restart: unless-stopped` (it also brings back a core update that did not
start, see [upgrading](../getting-started/upgrading.md)); `compose.prod.yaml` adds it to `db` too.
If containers still do not come back after the Docker daemon restarts, `docker compose up -d`
(or `make prod-up`) starts them again with their data intact.

## Several instances

Instances that share one database see each other's plugin changes: every process compares the
`plugins` table with what it has loaded every 10 seconds and loads, unloads or reloads the
difference. Plugin packages live in the `plugins` directory next to the uploads directory (with
`PURSAE_UPLOADS_DIR=/data/uploads` that is `/data/plugins`), so every instance
needs the same volume; an instance cannot load a package it does not have on disk.
