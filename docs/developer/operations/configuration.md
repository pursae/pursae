---
title: Configuration
description: Every PURSAE_* environment variable, what it does and its default.
weight: 10
section: operations
---

Pursae is configured entirely through environment variables. Everything else (site title,
permalinks, mail sender, cache settings, trusted plugin keys) lives in the options table and is
edited in the admin.

**Names before 0.5.0.** These variables were called `WPGO_*` (`WPGO_SITE_URL`, `WPGO_SECRET` …)
until Pursae 0.5.0. The old names still work: when only the old name is set, or the new one is set but
empty, the old value is used and the start-up log names its replacement once (`deprecated environment
variable old=WPGO_SITE_URL new=PURSAE_SITE_URL`); when both are set to different non-empty values the
`PURSAE_*` one wins and the log says the old one is ignored.
Values are never logged. The old names are removed no earlier than 1.0; the full list is in
[upgrading](../getting-started/upgrading.md#renamed-in-050-environment-variables-and-the-binary).

## Core

| Variable | Default | Purpose |
|----------|---------|---------|
| `DATABASE_URL` | `postgres://wpgo:wpgo@localhost:5432/wpgo?sslmode=disable` | PostgreSQL connection string (pgx URL format); PostgreSQL 16 or 17 |
| `PURSAE_ADDR` | `:8080` | address the HTTP server listens on |
| `PURSAE_SITE_URL` | `http://localhost:8080` | public URL without a trailing slash; written to the `siteurl` and `home` options by the installer |
| `PURSAE_SETUP_TOKEN` | generated on the first start of an uninstalled site | the token the installer (`/wp-admin/install`) asks for, so nobody but the operator can install a fresh instance and become its administrator. When empty, the first process to start on an uninstalled site generates a random token, stores only its SHA-256 in the database (every instance and restart checks the same one) and prints the install link `<PURSAE_SITE_URL>/wp-admin/install?token=…` in its log. `pursae setup-token` prints the link: with this variable set, with its value; otherwise with a newly issued token that replaces the stored one. Entered once, the token is remembered for 12 hours by a signed cookie; installing the site deletes it, so the token and the cookie stop working |
| `PURSAE_UPLOADS_DIR` | `./data/uploads` | media library files. The directories next to it hold plugins (`../plugins`), the compiled-plugin cache (`../plugin-cache`), themes (`../themes`) and development mail (`../mail`) |
| `PURSAE_SECRET` | generated on first start and stored in the settings | secret for session cookies and nonces. Set it when you run more than one instance against one database so every instance signs the same way |
| `PURSAE_WEB_DIR` | embedded | read admin templates, theme templates, CSS and JS from this directory instead of the binary, reloading per request (development) |
| `PURSAE_EMPTY_TRASH_DAYS` | `30` | days trashed posts and comments are kept before the daily `wp_scheduled_delete` job removes them (WordPress' `EMPTY_TRASH_DAYS`). `0` empties the trash at every daily run; unlike WordPress it does not hide the Trash links in the admin |
| `PURSAE_LOG_FORMAT` | text | `json` writes the access log as one JSON object per request (`status`, `method`, `uri`, `duration`, `ip`, `request_id`) for log shippers; other log lines are unchanged |
| `PURSAE_METRICS` | off | `1` enables the Prometheus endpoint and request counters ([metrics](metrics.md)). Needs `PURSAE_METRICS_ADDR` or `PURSAE_METRICS_TOKEN` too, otherwise it stays off |
| `PURSAE_METRICS_ADDR` | — | serve `/metrics` on this address (`127.0.0.1:9100`) instead of the site port |
| `PURSAE_METRICS_TOKEN` | — | bearer token required to read the metrics; without `PURSAE_METRICS_ADDR` it enables `/metrics` on the site port for requests that carry it |
| `PURSAE_SLOW_QUERY` | off | log and count database queries at least this slow (`250ms`, `1s`, or milliseconds); SQL text only, never arguments ([metrics](metrics.md#slow-query-log)) |
| `PURSAE_FIREWALL` | on | the core firewall ([security](security.md#built-in)): a per-IP rate limit of 900 requests a minute (static files, loopback and trusted proxies do not count; `429` with `Retry-After` above it, counted per process) and request rules on the URL and `User-Agent` (`403`). `off` (or `0`, `false`) switches both off for the process, for example behind a WAF that does the same; Pursae Security's Firewall tab overrides the limit and the rules otherwise |
| `PURSAE_BACKUPS_DIR` | `backups/` next to uploads | where Backup Manager keeps its archives (manual, scheduled and safety snapshots). Keep it outside the uploads directory, which is served publicly |
| `PURSAE_BACKUP_S3_ENDPOINT`, `PURSAE_BACKUP_S3_REGION`, `PURSAE_BACKUP_S3_BUCKET`, `PURSAE_BACKUP_S3_PREFIX`, `PURSAE_BACKUP_S3_ACCESS_KEY`, `PURSAE_BACKUP_S3_SECRET_KEY`, `PURSAE_BACKUP_S3_PATH_STYLE` | unset | Backup Manager remote storage: the S3-compatible bucket backups are copied to, encrypted ([backups](backups.md#remote-storage-s3-compatible-encrypted)). Each one overrides the saved field and is read-only on the Remote storage tab, so credentials can stay out of the database |
| `PURSAE_BACKUP_PASSPHRASE` | unset | passphrase remote backups are encrypted with (and `pursae backup decrypt` reads). Keep a copy away from the server: without it the remote backups cannot be opened |
| `PURSAE_BACKUP_S3_ALLOW_PRIVATE` | unset | `1` lets remote storage reach a private or loopback address and plain `http` (a MinIO on your own network). Link-local and cloud metadata addresses stay refused. `PURSAE_ALLOW_PRIVATE_ENDPOINTS=1` does the same |
| `PURSAE_ALLOW_PRIVATE_ENDPOINTS` | unset | `1` lets endpoints set by an administrator reach loopback, private (RFC 1918, IPv6 unique-local) and carrier-grade NAT addresses: the Pursae CDN storage endpoint (a MinIO, Ceph or Garage on your own network) and Backup Manager remote storage. Without it they must be public; the address is checked when the endpoint is saved and again on every connection, after DNS resolution. Link-local, cloud metadata and reserved addresses are refused always. An endpoint named in `PURSAE_CDN_SETTINGS` may be private without it. The cache's *Front Server Purge URL* does not need it (see [caching](caching.md)) |
| `PURSAE_LANG_DIR` | `languages/` next to uploads | installed language packs (Settings › General › Site Language, Dashboard › Updates › Translations) |
| `PURSAE_TRANSLATIONS_API` | `https://api.wordpress.org/translations` | the translation API language packs are downloaded from (a mirror, or a fake in tests) |
| `PURSAE_BIN_DIR` | — (`/data/bin` in the image) | persistent directory `pursae launch` installs the binary into, so a core update made from the admin survives a container restart ([upgrading](../getting-started/upgrading.md)) |
| `PURSAE_ALLOW_OLDER_BINARY` | unset | `1` lets a binary start on a database whose schema needs a newer Pursae. Every migration run records in the `schema_meta` table the oldest release that may run on the schema (`min_binary_version`, raised only by a release whose migrations remove or change something older releases use, never lowered). A binary that finds migrations it does not know **and** is older than that release refuses to start (`serve`, `migrate` and every command that opens the database) with the version it needs; restore a backup taken before the update or install that version instead. With this variable it starts anyway, logs the condition, and `/readyz` answers `503` until a compatible binary runs. An older binary that is still at or above the floor starts normally and logs a warning once. **Emergency use only** |
| `PURSAE_LISTEN_FD` | — | **internal**: set by the running binary when it hands its listening socket to an updated binary; do not set it yourself |

## Database pool

`DATABASE_URL` takes pgx's pool parameters as query parameters; each one you leave out gets the
default below (pgx alone would allow only `max(4, CPUs)` connections):

| Parameter | Default | Purpose |
|-----------|---------|---------|
| `pool_max_conns` | 4 × CPUs, at least 10, at most 40 | connections per instance. Keep the sum over all instances (plus backups and `psql`) under PostgreSQL's `max_connections` (100 by default) |
| `pool_min_conns` | `2` | connections kept open when idle |
| `pool_max_conn_lifetime` | `1h` (+ up to 5 min jitter) | a connection is replaced after this long |
| `pool_max_conn_idle_time` | `15m` | idle connections above the minimum are closed after this long |
| `pool_health_check_period` | `30s` | how often idle connections are checked |

```
DATABASE_URL=postgres://wpgo:secret@db:5432/wpgo?sslmode=disable&pool_max_conns=20
```

### Connection poolers (PgBouncer)

Pursae relies on session-level PostgreSQL features, so connect it directly to PostgreSQL, or put
PgBouncer in front only with `pool_mode = session`. Transaction and statement pooling are **not
supported**:

- **Session advisory locks** (`pg_advisory_lock` / `pg_try_advisory_lock`, released later on the
  same connection): Pursae Commerce Bookings, Subscriptions and Points and Rewards checks,
  install, database migrations and the search-stem rebuild. Behind a transaction pooler the unlock
  can reach a different server session, so locks leak or stop guarding anything. (Transaction-scoped
  `pg_advisory_xact_lock` uses, such as the forum, mail rate limits, plugin events, Web Builder
  publishing and post slugs, would be fine on their own.)
- **`LISTEN`/`NOTIFY`**: settings sync and Pursae Cache purges between instances
  ([several instances](reverse-proxy-and-tls.md#several-instances)); a transaction pooler does not
  deliver notifications.
- **Prepared statements**: pgx caches them per connection. Switching pgx to the simple protocol
  would fix only this point, not the two above, so it does not make transaction pooling safe.

Instead of a pooler, size `pool_max_conns` per instance against `max_connections`. Each instance
also keeps up to two long-lived listening connections (settings and cache purges) on top of
`pool_max_conns`; count them too.

## Mail

Without `PURSAE_SMTP_HOST`, messages are written as `.eml` files to `PURSAE_MAIL_DIR` so you can
inspect them. See [Mail](mail.md).

| Variable | Default | Purpose |
|----------|---------|---------|
| `PURSAE_SMTP_HOST` | — | SMTP server |
| `PURSAE_SMTP_PORT` | `587` | `465` means implicit TLS, anything else STARTTLS when offered |
| `PURSAE_SMTP_USER`, `PURSAE_SMTP_PASS` | — | credentials |
| `PURSAE_MAIL_FROM` | `<Site title> <wordpress@host>` | sender; the host is taken from the site URL |
| `PURSAE_MAIL_DIR` | `mail/` next to uploads | where `.eml` files go when SMTP is off |

## Themes and plugins

| Variable | Default | Purpose |
|----------|---------|---------|
| `PURSAE_THEMES_DIR` | `<data>/themes` | theme packages (`<slug>/theme.json`) and the overrides saved by the Theme File Editor |
| `PURSAE_THEME_LIBRARY_DIR` | — (the library comes from the Hub) | serve Add Themes › Library from this directory of theme package directories instead of the Hub (development, offline sites; the repository's is `library/themes`). Hub downloads are cached in `theme-library-cache/` next to the uploads |
| `PURSAE_DISALLOW_FILE_EDIT` | off | set to `1`/`true` to make the Theme File Editor read-only (like `DISALLOW_FILE_EDIT`) |
| `PURSAE_PLUGIN_KEYS` | — | comma-separated base64 Ed25519 public keys trusted to sign plugin packages, in addition to the `plugin_trusted_keys` option |
| `PURSAE_MARKET_DIR` | `examples/plugins/dist`, then `/usr/share/wpgo/plugins` | a directory of `.zip` packages offered on Plugins › Add Plugin |
| `PURSAE_TRUST_DEV_KEY` | unset | **Development builds only** (`-tags devhub`): trust the `dev-key.pub` found in the market directory. Production binaries ignore it |
| `PURSAE_ALLOW_UNSIGNED_PLUGINS` | unset | `1` lets the `plugin_allow_unsigned` option take effect (development only); without it the option is ignored, so a database or CLI write alone cannot turn plugin signature checks off |
| `PURSAE_EXPOSE_USERS` | unset | `1` lets visitors list accounts (public REST users, `/?author=N`, users sitemap). Off by default because those routes disclose account names |
| `PURSAE_TRUSTED_PROXIES` | loopback and private ranges | proxies (comma separated IPs or CIDRs) whose `X-Forwarded-For` / `X-Real-IP` is believed when deriving the visitor address; `none` trusts no proxy ([reverse proxy](reverse-proxy-and-tls.md)) |
| `PURSAE_GEOIP_DB` | `geoip/country.mmdb` next to the uploads (`/data/geoip/country.mmdb` in Docker) | IP-to-country database (MMDB) used for visitor countries when no proxy or CDN sends a country header. DB-IP "IP to Country Lite" (CC BY 4.0, attribution: "IP geolocation by DB-IP"); a MaxMind GeoLite2 Country file works too. The Docker image ships a copy; a missing or damaged file just means headers only |
| `PURSAE_GEOIP_UPDATE` | on | `off` stops the monthly download of the DB-IP database (Pursae Analytics › Settings can still update it by hand) |
| `PURSAE_HUB` | unset (on) | `off` switches Pursae Manager (hub.pursae.com) off in any build: the site sends nothing, and gets no update notices or one-click updates, no marketplace and no licenses. Every admin page then shows a warning (to users who may update) until `PURSAE_HUB` is removed and Pursae restarted. **Not recommended** |
| `PURSAE_DEV_HUB_URL` | unset (`http://pursae-manager.test:8090` in the development compose file) | **devhub builds only**: the Manager to use instead of `https://hub.pursae.com` (https, or http on localhost / `*.localhost` / `*.test`); `off` disables it. Production binaries ignore it ([development builds](#development-builds-devhub)) |
| `PURSAE_TRUST_HUB_KEYS` | unset | **devhub builds only**: `1` trusts every key the Manager lists, endorsed by the release root or not ([root of trust](../hub/index.md#root-of-trust)). Production binaries ignore it |

### Pursae Manager

Every Pursae install talks to Pursae Manager at `https://hub.pursae.com`. A production binary has
no environment variable or option that changes this address or relaxes key checking: the Pursae
release root key is pinned in it, and only Manager keys endorsed by that root verify core
self-updates and Manager-signed plugin packages ([root of trust](../hub/index.md#root-of-trust)).

### Development builds (devhub)

A binary built with the `devhub` tag can be pointed at a local Pursae Manager to test updates:

```sh
go run -tags devhub ./cmd/wpgo serve        # make dev does this
docker compose -f docker/compose.yaml up    # builds with GO_TAGS=devhub
```

It reads `PURSAE_DEV_HUB_URL` (the development compose file defaults to the Manager on the host,
`http://pursae-manager.test:8090`, an `extra_hosts` alias for the host gateway: plain http is
only accepted for `localhost`, `*.localhost` and `*.test` names) and `PURSAE_TRUST_HUB_KEYS` (off by default; set it to `1`
so a local Manager's throwaway signing key verifies releases and plugins; automatic core updates never run under it). The production compose file builds without the tag
(`GO_TAGS=""`, `make prod-up`) and drops both variables. Never ship a devhub binary.

## Content

| Variable | Default | Purpose |
|----------|---------|---------|
| `PURSAE_POST_REVISIONS` | `25` | revisions kept per post (WordPress' `WP_POST_REVISIONS`); older ones are deleted on save, Web Builder publishes included. A negative value keeps every revision. Tools › Revisions deletes the excess of existing posts |

Post search stems words in the site language (Settings › General › Site Language → the PostgreSQL text
search configuration, e.g. `english`, `turkish`; `simple` when PostgreSQL has none). Changing the
language rebuilds the stemmed vectors in the background.

## Built-in plugins

| Variable | Default | Purpose |
|----------|---------|---------|
| `PURSAE_ADMINSEC_DISABLE_HIDE_LOGIN` | unset | recovery switch for Pursae Security's custom login URL: `1`/`true`/`yes` brings `/wp-login` back while it is set (restart the server after setting it). `pursae adminsec disable-hide-login` turns the setting off for good |
| `PURSAE_CDN_SETTINGS` | unset | Pursae CDN settings as one JSON object (the keys of the `gocdn_settings` option, in the style of WP Offload Media's `AS3CF_SETTINGS`, e.g. `{"provider":"aws","bucket":"media","access-key-id":"…","secret-access-key":"…"}`). Keys given here override the saved settings and are shown read-only on the CDN screen, so credentials can stay out of the database. Invalid JSON is ignored with a warning in the log. An `endpoint` given here may be on the private network; one entered on the CDN screen needs `PURSAE_ALLOW_PRIVATE_ENDPOINTS=1` for that |

The `PURSAE_BUILDER_AI*` variables in `internal/webbuilder/contract.go` belong to the Web Builder AI module,
which is not shipped (its upstream modules are stubbed); setting them has no effect.

## Compose only

`make prod-up` reads these, `PURSAE_SITE_URL`, the mail settings and `PURSAE_PLUGIN_KEYS` /
`PURSAE_TRUSTED_PROXIES` from `docker/.env` (template: `docker/.env.example`).

| Variable | Default | Purpose |
|----------|---------|---------|
| `PURSAE_PORT` | `8088` | host port for the site |
| `PURSAE_BIND` | `127.0.0.1` | host address the compose files publish the site on. `0.0.0.0` opens it to the network: then set `PURSAE_TRUSTED_PROXIES`, since the default trusts the private ranges and a client there could choose its own address |
| `PURSAE_DB_PORT` | `5432` | host port for PostgreSQL, published on `127.0.0.1` by the development file only; `compose.prod.yaml` does not publish PostgreSQL |
| `PURSAE_DB_PASSWORD` | — (required) | database password, with `compose.prod.yaml` only (the development file uses `wpgo`); applied when the `pgdata` volume is created |

## Test suite

| Variable | Purpose |
|----------|---------|
| `PURSAE_TEST_ADMIN_DB` | a connection used to drop and recreate the test database |
| `PURSAE_TEST_DB` | the test database URL; both unset skips the integration tests |
| `PURSAE_I18N_MISSES` | development: append every UI string that has no translation to this file (JSON lines); `PURSAE_I18N_MISSES_*` are read by the i18n coverage tests only |

## Options worth knowing

These are options in the database (Settings screens or `/wp-json/wp/v2/settings`), not
environment variables:

| Option | Where | Meaning |
|--------|-------|---------|
| `plugin_trusted_keys` | — | newline- or comma-separated public keys, merged with `PURSAE_PLUGIN_KEYS` |
| `plugin_allow_unsigned` | — | `true` accepts unsigned plugin packages (development only; needs `PURSAE_ALLOW_UNSIGNED_PLUGINS=1` too) |
| `wpgo_cache_*`, `wpgo_opt_*` | Pursae Cache screens | page cache and page optimisation ([caching](caching.md)) |
| `image_output_format` | Settings › Media | the format of the sizes generated for each uploaded image: `""` (default) JPEG sizes for WebP uploads (PNG with transparency); `webp` WebP sizes for WebP uploads, as WordPress does; `webp-all` WebP for JPEG, PNG and WebP uploads: the image itself is saved as WebP (`photo.jpg` → `photo.webp`, or `photo-scaled.webp` above 2560 px) and the upload is kept as the original image. WebP is written at quality 86 by libwebp transpiled to Go (no cgo). Plugins can change the mapping with the `image_editor_output_format` filter ([hooks](../plugins/hooks.md#media)). Existing images are not regenerated |
| `pursae_license_key` | Settings › License | the license key sent to the Hub ([licenses](../hub/index.md#licenses)) |
