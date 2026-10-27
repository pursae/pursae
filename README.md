<p align="center">
  <img src="web/gowebeditor/images/gopher.png" alt="WordPress Go" width="96">
</p>

<h1 align="center">WordPress Go</h1>

<p align="center">
  WordPress core rewritten in Go — one binary, PostgreSQL, the familiar WordPress admin,<br>
  the block editor, a page builder, sandboxed WebAssembly plugins and classic themes.
</p>

<p align="center">
  <img alt="Go 1.27" src="https://img.shields.io/badge/Go-1.27-00ADD8?logo=go&logoColor=white">
  <img alt="PostgreSQL 17" src="https://img.shields.io/badge/PostgreSQL-17-336791?logo=postgresql&logoColor=white">
  <img alt="License GPL" src="https://img.shields.io/badge/license-GPL--2.0--or--later-blue">
</p>

---

## Contents

- [Quick start (Docker)](#quick-start-docker)
- [First run: the installer](#first-run-the-installer)
- [Run without Docker](#run-without-docker)
- [Configuration](#configuration)
- [Data and volumes](#data-and-volumes)
- [Everyday commands](#everyday-commands)
- [Editing theme files](#editing-theme-files)
- [Plugins](#plugins)
- [Golementor (page builder)](#golementor-page-builder)
- [Production checklist](#production-checklist)
- [Troubleshooting](#troubleshooting)
- [What works in 0.1](#what-works-in-01)
- [Project layout](#project-layout)
- [License](#license)

## Quick start (Docker)

The fastest way to try WordPress Go. You only need **Docker** (Docker Desktop on macOS/Windows,
Docker Engine + Compose v2 on Linux) and **git**.

```sh
git clone https://github.com/boracomet/wordpress-go.git
cd wordpress-go
make docker-up
```

No `make`? Run the same thing directly:

```sh
docker compose -f docker/compose.yaml up -d --build
```

The first build downloads Go modules and compiles the binary, so give it a minute or two.
When it finishes, open **http://localhost:8088** — you will land on the installer.

| Service | What it is | Host port |
|---------|------------|-----------|
| `wpgo`  | The WordPress Go server | `8088` |
| `db`    | PostgreSQL 17 | `5432` |

### Use another port or address

The site URL is stored at install time, so set it **before** the first run (or reset, see
[Troubleshooting](#troubleshooting)):

```sh
WPGO_PORT=9000 WPGO_SITE_URL=http://localhost:9000 make docker-up
```

To reach the site from another device on your network, use your machine's LAN address:

```sh
WPGO_SITE_URL=http://192.168.1.50:8088 make docker-up
```

## First run: the installer

Open the site and WordPress Go redirects to `/wp-admin/install`. Fill in the same fields as
WordPress' five-minute install:

1. **Site title**
2. **Username** and **password** for the first administrator
3. **Email**
4. Whether to ask search engines not to index the site

The installer creates "Hello world!", its first comment, the Sample Page, a draft Privacy
Policy page and the default sidebar widgets. Then sign in at `/wp-login` and you are in the
admin (`/wp-admin/`).

> Admin URLs have no `.php` suffix (`/wp-admin/edit`, `/wp-login`). The WordPress spellings
> (`/wp-admin/edit.php`, `/wp-login.php`) still work: GET requests redirect permanently, POSTs
> are served in place.

## Run without Docker

You need **Go 1.27** and a **PostgreSQL (17 is what the compose file uses)** database you can create tables in.

```sh
# 1. Start only the database from the compose file (or point DATABASE_URL at your own)
make db

# 2. Run the app; templates, CSS and JS are read from ./web so edits show up on refresh
export DATABASE_URL='postgres://wpgo:wpgo@localhost:5432/wpgo?sslmode=disable'
export WPGO_SITE_URL='http://localhost:8080'
WPGO_WEB_DIR=web go run ./cmd/wpgo serve
```

`make dev` does steps 1 and 2 for you. To build a single binary instead:

```sh
make build              # writes bin/wpgo
./bin/wpgo serve
```

Migrations run automatically on start (under a database advisory lock, so several instances
can start safely). You can also run them alone with `wpgo migrate`.

## Configuration

Everything is configured with environment variables.

**Core**

| Variable | Default | Purpose |
|----------|---------|---------|
| `DATABASE_URL` | `postgres://wpgo:wpgo@localhost:5432/wpgo?sslmode=disable` | PostgreSQL connection string |
| `WPGO_ADDR` | `:8080` | Address the server listens on |
| `WPGO_SITE_URL` | `http://localhost:8080` | Public URL; written to the settings at install |
| `WPGO_UPLOADS_DIR` | `./data/uploads` | Where media uploads are stored |
| `WPGO_SECRET` | generated | Signing secret; generated and stored on first start if unset |
| `WPGO_WEB_DIR` | embedded | Read templates/CSS/JS from disk instead of the embedded copy (development) |

**Compose-only**

| Variable | Default | Purpose |
|----------|---------|---------|
| `WPGO_PORT` | `8088` | Host port for the site |
| `WPGO_DB_PORT` | `5432` | Host port for PostgreSQL |

**Mail** — without an SMTP host, messages are written as `.eml` files so you can inspect them.

| Variable | Default | Purpose |
|----------|---------|---------|
| `WPGO_SMTP_HOST` | — | SMTP server |
| `WPGO_SMTP_PORT` | `587` | `465` = implicit TLS |
| `WPGO_SMTP_USER` / `WPGO_SMTP_PASS` | — | Credentials |
| `WPGO_MAIL_FROM` | — | Sender address |
| `WPGO_MAIL_DIR` | `mail/` next to uploads | Where `.eml` files go when SMTP is off |

**Themes and plugins**

| Variable | Default | Purpose |
|----------|---------|---------|
| `WPGO_THEMES_DIR` | `<data>/themes` | Where edited theme files are saved |
| `WPGO_DISALLOW_FILE_EDIT` | off | Set to make the Theme File Editor read-only |
| `WPGO_PLUGIN_KEYS` | — | Comma-separated Ed25519 public keys trusted to sign plugin packages |
| `WPGO_MARKET_DIR` | — | Plugin packages offered on Plugins › Add New |
| `WPGO_TRUST_DEV_KEY` | `1` in compose | Trust the demo signing key of the bundled example plugins (**development only**) |

## Data and volumes

Compose keeps state in named volumes, so `docker compose down` does not lose anything:

| Volume | Mounted at | Contains |
|--------|------------|----------|
| `pgdata` | `/var/lib/postgresql/data` | The database |
| `uploads` | `/data/uploads` | Media library files |
| `plugins` | `/data/plugins` | Installed plugin packages |
| `themes` | `/data/themes` | Theme files you edited in the admin |

Back up the database:

```sh
docker compose -f docker/compose.yaml exec db pg_dump -U wpgo -Fc wpgo > wpgo.dump
```

Restore it:

```sh
docker compose -f docker/compose.yaml exec -T db pg_restore -U wpgo -d wpgo --clean --if-exists < wpgo.dump
```

## Everyday commands

```sh
make docker-up          # build and start everything in the background
make docker-down        # stop (data is kept)
make test               # unit tests + PostgreSQL integration tests
make docker-test        # the same suite inside a Go container
make build              # compile bin/wpgo
```

Logs and restart:

```sh
docker compose -f docker/compose.yaml logs -f wpgo
docker compose -f docker/compose.yaml restart wpgo
```

Update to a newer version:

```sh
git pull
make docker-up          # rebuilds the image; migrations run on start
```

## Editing theme files

Appearance › **Theme File Editor** works without recompiling. Saving a file writes an override
into the themes volume (`/data/themes`, or `WPGO_THEMES_DIR`); the template set is re-parsed
immediately and the page is test-rendered. If the change has a template or render error it is
**rolled back automatically** and the editor shows the error. **Restore original** removes the
override. Set `WPGO_DISALLOW_FILE_EDIT=1` to turn editing off.

Themes can also be installed as code-free **theme packages** (`theme.json`, Go templates, assets)
through Appearance › Themes › Add Theme; the format is in [docs/theme-package.md](docs/theme-package.md)
and the theme developer guide at [/docs/themes/](https://wordpressgo.dev/docs/themes/).

## Plugins

Plugins are signed WebAssembly packages that run in a sandbox (wazero) with permissions you
grant at activation. A crash or runaway loop in a plugin is contained and does not take the
site down. Plugins can filter content, register shortcodes, add REST routes under their own
namespace, read and write posts, declare a settings page under Settings, schedule events, cache
data in transients and ship CSS/JS assets. The Go SDK lives in `sdk/go/wpgo`; examples are in
`examples/plugins` (`hello-gopher`, `word-counter` and `shortcode-demo`, which uses every SDK
feature); the ABI is documented in [internal/plugins/README.md](internal/plugins/README.md) and
the plugin developer guide at [/docs/plugins/](https://wordpressgo.dev/docs/plugins/).

```sh
tools/build-plugins.sh      # builds the example plugins into examples/plugins/dist
```

Rebuild the image afterwards and they appear under Plugins › Add New.

## Golementor (page builder)

Golementor is a built-in plugin, like Classic Editor and WPGo Cache, and it is **off on a fresh
install**. Switch it on under Plugins › Golementor › **Activate** (or Plugins › Add Plugin ›
Native Plugins). Until then it leaves no trace: no menu entries, "Edit with Golementor" links,
builder routes, assets, front-end output or Hello Golementor theme.

Deactivating it again hides everything. If Hello Golementor is the active theme the site is
switched to Twenty Sixteen (the Plugins screen says so). Pages built with Golementor show the
plain-text content saved with them while it is off; saved themes, theme parts and the builder
data stay in the database and come back on the next activation.

## Production checklist

- Put WordPress Go behind a TLS-terminating reverse proxy and set `WPGO_SITE_URL` to the
  `https://` address.
- Change the default database credentials in `docker/compose.yaml` (`wpgo` / `wpgo`) and do
  **not** publish port 5432 to the internet — remove the `ports:` entry of the `db` service.
- Set `WPGO_TRUST_DEV_KEY=0`. The example plugins are signed with a public demo key; for real
  plugins list your own keys in `WPGO_PLUGIN_KEYS`.
- Configure SMTP (`WPGO_SMTP_*`) so password resets and notifications are delivered.
- Back up the `pgdata` and `uploads` volumes.
- Pick a strong admin password; WordPress Go never calls Gravatar or other third-party
  services on its own.

## Troubleshooting

**The site redirects to `/wp-admin/install` again after a restart.** The database is empty or
a different one is being used. Check that the `pgdata` volume still exists
(`docker volume ls`) and that `DATABASE_URL` points to the database you installed into.

**Docker restarted and the site is down.** Containers do not always come back after the Docker
daemon restarts. Start them again — data is kept:

```sh
docker compose -f docker/compose.yaml up -d
```

**Links or assets point at the wrong address (old IP or port).** The site URL was stored at
install time. Either change it in Settings › General, or start over with the right
`WPGO_SITE_URL` (see the next item).

**Start from scratch.** This **deletes** the database, uploads, plugins and edited themes:

```sh
docker compose -f docker/compose.yaml down -v
make docker-up
```

**Port 8088 or 5432 is already in use.** Pick other host ports:

```sh
WPGO_PORT=9000 WPGO_DB_PORT=5433 WPGO_SITE_URL=http://localhost:9000 make docker-up
```

**The first build is slow or fails to download modules.** The Go image downloads modules on the
first build; make sure Docker has network access. Later builds are cached.

**Port already taken by a local WordPress.** Stop it, or use `WPGO_PORT` as above.

## What works in 0.1

- **Core** — typed PostgreSQL schema with JSONB, migrations on start, install wizard, argon2id
  passwords, sessions, login throttling, WordPress nonces, security headers, roles and
  capabilities with multi-role union and per-user allow/deny (deny wins).
- **Admin** (WordPress 7.1 markup and CSS, English) — Dashboard, Posts, Pages, Media Library
  (grid with the real `wp.media` modal, list, uploads with image sizes), Comments, Appearance
  (Themes with Live Preview, Customize, Widgets, Menus, Theme File Editor), Plugins, Users,
  Tools (Import/Export WXR, Site Health, personal data export/erase), Settings (General,
  Writing, Reading, Discussion, Media, Permalinks, Privacy), Updates, About.
- **Gotenberg** — WordPress' block editor (the 7.1 `@wordpress/*` bundles) on wpgo's REST API:
  preloading, autosaves, revisions, media, patterns, oEmbed proxy.
- **Classic Editor** (built-in plugin) — TinyMCE + Quicktags, Add Media, featured image;
  default editor and per-user switching under Settings › Writing.
- **Golementor** — a visual page builder: classic and atomic (V4) elements, theme builder
  (headers, footers, archives, single templates), popups, forms with integrations, loops,
  dynamic tags, saved themes, built on the Hello Golementor base theme.
- **REST API `wp/v2`** — posts, pages, media, blocks, terms, users, comments, settings, types,
  taxonomies, statuses, themes, search, patterns, sidebars/widgets; `_fields`, `_embed`,
  pagination headers, cookie + nonce and Application Password authentication.
- **Themes** — Twenty Sixteen, Twenty Seventeen, Twenty Twenty, Twenty Twenty-One and Hello
  Golementor with their original CSS/JS and markup, plus **Classic**: a white, minimal blog theme
  written for WordPress Go (system fonts, automatic dark mode, list/grid/minimal layouts, optional
  sidebar), configured in Appearance › Customize with a live preview.
- **WPGo Cache** (built-in plugin) — in-process full-page cache with tag-based purges, crawler,
  gzip + ETag, CDN purge headers, lazy images, deferred scripts, font preloads.
- **Plugins** — signed WebAssembly packages, granted permissions, hooks `init`, `the_content`,
  `admin_notices`, `wpgo_post_stats` and scheduled `cron_*` events; shortcodes, REST routes,
  posts and current-user APIs, settings pages, transients and package assets.
- **System** — job runner on the `jobs` table (multi-instance safe) with WordPress' scheduled
  events, e-mail, WXR import/export.

Not included yet: WooCommerce and Facebook widgets for Golementor.

## Project layout

```
cmd/wpgo            entry point (serve, migrate, version)
docker/             Dockerfile and compose.yaml
internal/blocks     block parser, renderer, serializer
internal/store      data access (pgx)
internal/server     routes, admin screens, REST API, editors, themes, cron, mail glue
internal/cache      WPGo Cache page-cache engine
internal/plugins    Wasm plugin runtime, hooks, permissions, signed packages
internal/builder    Golementor document model, renderer and CSS engine
internal/mail       wp_mail(): SMTP or .eml files
internal/links      permalinks and URL builders
internal/caps       capabilities and meta-capability mapping
internal/wp         WordPress formatting helpers (dates, slugs, autop, kses)
sdk/go/wpgo         plugin SDK; examples in examples/plugins
web/admin           admin templates and the vendored wp-admin CSS/JS
web/themes          bundled themes: assets (GPL) and their Go templates
web/wp-includes     vendored core CSS/JS (block library, dashicons, admin bar, jQuery)
```

## License

GPL-2.0-or-later. The bundled themes and the vendored WordPress CSS/JS are GPL-licensed by the
WordPress contributors. Golementor's editor UI is GPL-3.0 licensed third-party code, so builds
that include it are distributed under GPL-3.0. "WordPress" is a trademark of its owner;
WordPress Go is not affiliated with it. Third-party attributions are in [NOTICE](NOTICE).
