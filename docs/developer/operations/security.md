---
title: Security
description: What Pursae does for you, and the checklist for a production site.
weight: 80
section: operations
---

## Built in

- **Passwords** are hashed with argon2id. Sessions are server-side rows tied to a random cookie
  token; the cookie is `HttpOnly`, `SameSite=Lax` and `Secure` behind TLS. Sessions last 2 days,
  14 with "Remember Me".
- **Login throttling**: five failed logins from one address within 15 minutes block further
  attempts from that address, and twenty failures for one user name (from any address) block that
  name. Each attempt is counted before its password is checked, so parallel requests cannot slip past
  the limit. REST Basic authentication counts too. An unknown user name costs as much time as a wrong
  password, and at most twice as many password hashes as the server has CPUs run at once.
- **Lost-password requests** (wp-login and the store's My Account form) are limited to 3 per 15 minutes
  per client network and per account (0.6.3+). The count is kept in the database, so every instance
  behind a load balancer shares it, and the answer is the same whether or not the account exists.
- **Cross-site form posts** (0.6.3+): a `POST` to the login page, and a comment `POST` from a signed-in
  user, is refused with `403` when the browser marks it as coming from another site
  (`Sec-Fetch-Site: cross-site`, or an `Origin` that is neither the request's host nor the site
  address). Requests without those headers (scripts, older clients) are handled as before; anonymous
  comments are not affected.
- **Access changes end sessions** (0.6.3+): when another administrator changes a user's role or
  capabilities, that user's open sessions end and they sign in again. Blocking a forum member or
  changing their forum rank does the same. Editing your own profile keeps your current session.
- **Plugin and theme updates from the web** (0.6.3+): uploading a new package for an *active* plugin
  needs `activate_plugins` (the new code runs at once); overwriting an installed theme needs
  `update_themes`. The CLI is not affected.
- **Visitor address behind a proxy**: `X-Forwarded-For` is read right to left, and the first address
  that is not a trusted proxy is the visitor, so a value the client adds itself is ignored. Trusted
  proxies default to loopback and private ranges; set `PURSAE_TRUSTED_PROXIES` to your proxy's
  addresses (the list replaces the default), or `none` when Pursae faces the internet directly
  ([reverse proxy](reverse-proxy-and-tls.md)).
- **Hidden login address**: with Pursae Security's *Hide login* on, the store's My Account
  lost-password form and the reset e-mail never show the hidden address (0.6.3+).
- **Operational endpoints**: `/readyz` names no versions in its answer (they are in the server log)
  (0.6.3+); the `/metrics` listener starts without `PURSAE_METRICS_TOKEN` only on a loopback address
  ([metrics](metrics.md)).
- **Locked plugin keys**: `PURSAE_PLUGIN_KEYS_LOCKED=1` stops administrators from adding or removing
  trusted plugin signing keys; only `PURSAE_PLUGIN_KEYS` counts ([configuration](configuration.md)).
- **Nonces** protect every admin form and the REST API (`X-WP-Nonce`), exactly like WordPress.
- **Security headers** on every response: `X-Content-Type-Options: nosniff`,
  `X-Frame-Options: SAMEORIGIN`, `Referrer-Policy: strict-origin-when-cross-origin`, a
  `Content-Security-Policy` (front end: `script-src 'self' 'unsafe-inline'`, no remote scripts;
  admin: what the block editor needs), and `Strict-Transport-Security` behind TLS.
- **Script nonces** (Settings › Security): every script Pursae, its themes and plugins write carries
  a nonce that changes with each response, and a second policy, `script-src 'nonce-…'
  'strict-dynamic'`, allows only those. By default it is sent as `Report-Only`: browsers report what
  it would block to `/wpgo-csp-report`, and the screen lists the reports. **Enforce** makes it the
  real policy, so a script injected into a page is blocked. Scripts that administrators stored
  (posts and pages, Custom HTML blocks and widgets, Web Builder HTML sections, the maintenance
  page, product descriptions, Gooike custom snippets) get the nonce too, because only
  `unfiltered_html` users can save a script there; untick **Administrator scripts** to block them as
  well. A proxy or CDN that caches pages serves one nonce to every visitor until the page expires
  there.
- **Allowed external sources** (Settings › Security, `GET`/`POST /wp-json/pursae/v1/csp`, the MCP
  tools `csp_get_settings` / `csp_update_settings`): the front-end policy keeps `connect-src`,
  `font-src` and `style-src` on the site itself. The site owner may add named origins to those three
  directives, e.g. `connect-src https://api.github.com` for a theme script that reads the GitHub API.
  A directive the policy lacks starts from `default-src`, so the header becomes
  `connect-src 'self' https://api.github.com`. Accepted: `https://host`, `https://host:port`,
  `https://*.example.com` (leftmost wildcard only) and `wss://host` for `connect-src`; at most 20 per
  directive. Rejected: `*`, `https:`, any `http:` source, `data:`, `blob:`, quoted keywords, paths,
  IP addresses, `localhost` and public-suffix wildcards (`*.com`, `*.co.uk`). Hosts are converted to
  punycode and every source is rebuilt in canonical form; the raw input never reaches the header,
  and the stored option is validated again when it is read. `script-src` is never opened: it would
  undo the nonce and `'strict-dynamic'` policy above. `img-src`, `media-src` and `frame-src` already
  allow every `https:` source. The sources apply to front-end HTML only (not `/wp-admin`,
  `/wp-login` or the REST API) and only to the enforced `Content-Security-Policy`, not the
  report-only nonce policy. Page-cache entries remember the policy they were stored under, so a
  change takes effect without a purge. The REST route needs `manage_options` (the `settings` scope
  for Application Passwords); a list with an invalid entry is refused with 400 and nothing is saved.
  Changes go to the server log and, with Pursae Security on, its activity log.
- **Uploads are data, never code**: `/wp-content/uploads/` is served with a sandboxing CSP, so an
  uploaded HTML or SVG file cannot run scripts against the site.
- **Content filtering**: users without `unfiltered_html` and all plugins have their post content
  filtered (KSES) before it is stored.
- **Plugins are sandboxed**: WebAssembly with no filesystem, no network except what the host
  allows, a memory cap, a per-call timeout and permissions you approve
  ([limits and sandbox](../plugins/limits-and-sandbox.md)). Packages are signed; only keys you
  trust can install.
- **Installer locked by a setup token**: until the site is installed, `/wp-admin/install` asks for the
  setup token, so whoever reaches a fresh instance first cannot install it and make themselves its
  administrator. The token is `PURSAE_SETUP_TOKEN` when set; otherwise the first process that starts on an
  uninstalled site generates a random one, stores only its SHA-256 in the database (every instance and
  restart checks the same one; the comparison is constant time) and prints the install link
  `<PURSAE_SITE_URL>/wp-admin/install?token=…` in its log. `pursae setup-token` prints the link again (with a
  newly issued token that replaces the stored one, unless `PURSAE_SETUP_TOKEN` is set). A token given in
  the URL is moved into a signed cookie bound to that token (12 hours) and dropped from the URL, so it
  does not stay in the history or leak in a `Referer`. Installing the site deletes the token, so the link
  and the cookie stop working (`internal/server/setup_token.go`, [configuration](configuration.md#core)).
- **Core firewall** (`internal/server/firewall.go`), on by default and without any plugin, runs before
  everything else on every request:
  - a **per-IP rate limit**: 900 requests a minute from one address; more is answered with
    `429 Too Many Requests` and `Retry-After` until the minute ends. `GET`/`HEAD` for static files
    (`/wp-includes/`, `/wp-content/`, the admin's assets, and elsewhere `.css`, `.js`, images, fonts,
    media and PDF by extension when the URL has no query string) and the CSP report endpoint (which has its own limit) do not count; loopback addresses and trusted
    proxies (`PURSAE_TRUSTED_PROXIES`) are never limited, since without a forwarded header they carry
    every visitor. The visitor address is the one [the proxy settings](reverse-proxy-and-tls.md) derive.
    The count is kept in each process's memory: with several instances behind a load balancer each
    counts separately.
  - **request rules** (a basic WAF) on the URL path, the query string (decoded twice) and the
    `User-Agent`, never a request body (post content, block markup and theme code are legitimate
    input there): path traversal, null bytes, probes for `.env`, `.git`, `wp-config.php` and similar
    files, `/etc/passwd`, script and SQL injection patterns, `${jndi:` and shellshock. A match is
    answered with `403`.
  - Blocks are logged once per address, event and minute (`firewall` in the server log and, with
    Pursae Security on, its activity log). Pursae Security's **Firewall** tab changes the limit (0
    switches it off) and switches the rules off; addresses on its allow list (**Lockouts & IPs**) skip
    both. With the plugin off the defaults above apply. `PURSAE_FIREWALL=off` switches the whole
    firewall off for the process, for example when a WAF in front of Pursae does the same job
    ([configuration](configuration.md#core)). WordPress has no core firewall; this is Pursae's own.
- **No third-party calls from visitors' browsers**: no Gravatar, no remote fonts (Google Fonts are
  served from the site, below), no remote scripts in the core's pages. The server's own outbound
  connections are listed in [Outbound connections](#outbound-connections).

## Checklist

1. Terminate TLS in a proxy and set `PURSAE_SITE_URL` to the `https://` address
   ([reverse proxy and TLS](reverse-proxy-and-tls.md)).
2. Start with `make prod-up` and settings in `docker/.env` (it adds `docker/compose.prod.yaml`, see
   [Docker](docker.md#production-changes)): it takes the database password from `PURSAE_DB_PASSWORD`,
   does not publish port 5432, sets `PURSAE_TRUST_DEV_KEY=0` and builds the production binary that gets
   core updates from `https://hub.pursae.com`. The installer is locked by the setup token (above):
   open the install link from the log (`docker compose logs wpgo`), or set `PURSAE_SETUP_TOKEN`, and keep
   the link private until the site is installed.
3. List your own plugin signing keys in `PURSAE_PLUGIN_KEYS` or the `plugin_trusted_keys` option; leave
   `plugin_allow_unsigned` off and never trust the public demo key (`PURSAE_TRUST_DEV_KEY` must stay `0`).
4. Set `PURSAE_SECRET` yourself when running several instances, and keep it secret.
5. Set `PURSAE_DISALLOW_FILE_EDIT=1` (or Pursae Security › Hardening › Disable file editors) if nobody
   should edit theme files; it applies to the admin editors and the REST theme-file routes alike.
6. Let clients reach the server only through your proxy (`docker/compose.prod.yaml` publishes the port on
   `127.0.0.1`), and set `PURSAE_TRUSTED_PROXIES` to the proxy's address when other machines share its
   private network ([reverse proxy](reverse-proxy-and-tls.md)).
7. Configure SMTP so password resets reach their recipients ([mail](mail.md)).
8. Back up the database and the data directories ([backups](backups.md)).
9. Review plugin permissions on activation; grant only what a plugin needs. A plugin that keeps
   failing is disabled automatically after five consecutive failures.
10. The core already keeps visitors from listing accounts, without any plugin: `/?author=N` answers 404, `/wp/v2/users` needs a
   login, the users sitemap is empty and the login form gives the same message for an unknown account and a wrong password.
   Set `PURSAE_EXPOSE_USERS=1` only if you need the public author list (a headless front end, say). Author archive pages
   (`/author/<nicename>`) stay public for authors with published posts and the slug comes from the account's login name, so
   set a *display name* and nickname that differ from the login for every account that publishes. **Pursae Security** (Plugins ›
   Installed Plugins; [Pursae Security](../../adminsecgo-features.md)) adds two-step login, a custom login URL and
   an activity log on top, and can relax these defaults.
11. Keep the core up to date ([upgrading](../getting-started/upgrading.md)) and rebuild the images
    regularly: the Docker build uses the latest `golang:1.27` image, and `govulncheck ./...` should stay clean.

## Outbound connections

Everything the server itself connects to, and how to stop each. Requests to addresses a user or the
content supplies (feeds, trackbacks, webhooks, plugin requests, imports) are made with
WordPress's `wp_safe_remote_request()` rules: http(s) only, and private, loopback and link-local
addresses are refused when the connection is made (after DNS, and again on every redirect), so they
cannot reach your internal network or a cloud metadata service. Fixed service endpoints (the Hub,
DB-IP, Google Fonts, payment and mail APIs) are https and are named below.

### Core (no plugin needed)

| Connection | When | What it sends | How to turn it off |
|------------|------|---------------|--------------------|
| **Pursae Manager** `https://hub.pursae.com` (`hub.go`, `hub_telemetry.go`, `core_update.go`) | daily ping (cron `wpgo_hub_check`), update checks and one-click core updates, marketplace plugin and theme library downloads, license checks, the product logo the Hub announces | site URL, version, license key; with statistics on also language, OS/arch, the installed plugins and themes, and package events; whatever that switch, the name, e-mail address and role of users who opted in to Pursae product update e-mails ([what a site sends](../hub/index.md#what-a-site-sends-statistics)). Never content, visitors or other users | statistics only: Settings › License › "Send usage statistics". Everything: `PURSAE_HUB=off` (no updates, security fixes included, no marketplace, no licenses; **not recommended**) |
| **DB-IP** `https://download.db-ip.com` (`geoip.go`) | the daily cron event `wpgo_geoip_update` downloads the free IP-to-country database once a month (and Pursae Analytics › Settings can update it by hand) | nothing about the site or its visitors: a plain file download. Visitor addresses are only looked up locally, in memory | `PURSAE_GEOIP_UPDATE=off` (the copy shipped in the Docker image, or a file you place at `PURSAE_GEOIP_DB`, is used; without any file only CDN country headers count) |
| **Google Fonts** `https://fonts.googleapis.com`, `https://fonts.gstatic.com` (`gfonts.go`) | when a Web Builder page uses a Google font that is not on disk yet: the server downloads the stylesheet and `woff2` files once into `uploads/web-builder/google-fonts/` | the font family name | do not pick Google fonts in the Web Builder (system fonts and uploaded fonts never download anything). Visitors' browsers never contact Google: the fonts are served from the site |
| **oEmbed providers** (`rest_extra.go`) | `GET /oembed/1.0/proxy`, used by the block editor when an author embeds a URL; users with `edit_posts` only; results cached for a day | the embedded URL, to the provider's own oEmbed endpoint from a fixed list (YouTube, Vimeo, X/Twitter, SoundCloud, Spotify, Flickr, Dailymotion, TikTok, Reddit; `oembedProviders`). Unlike WordPress, no other provider and no discovery from arbitrary pages | no switch: do not embed those URLs |
| **Feeds** (`site_blocks.go`) | the RSS block or widget fetches the feed URL the author entered, cached for `feed_cache_transient_lifetime` | a GET to that URL (public hosts only) | remove the block |
| **Trackbacks** (`admin_trackbacks.go`) | publishing a post whose "Send trackbacks to" field lists URLs | the post's title, link, excerpt and site name to those URLs (public hosts only) | leave the field empty. Pursae sends no pingbacks and no update pings to ping services (WordPress's `ping_sites`, Ping-O-Matic) |
| **Language packs** `https://api.wordpress.org/translations` and the download URLs it returns (`internal/i18n`) | an administrator installs or updates a language (Settings › General › Site Language, Dashboard › Updates › Translations, `pursae language …`) | which packs are wanted (locale, version) | do not install languages; `PURSAE_TRANSLATIONS_API` points at a mirror |
| **WXR import** (`wxr.go`) | Tools › Import with "Download and import file attachments" ticked | GETs of the attachment URLs in the file (public hosts only, 5 redirects, 30 s each) | leave the box unticked |
| **MCP / AI tools** (`mcp.go`) | `upload_media` or `replace_media_file` called with a `source_url` | a GET to that URL (public hosts only, 64 MB at most) | do not pass URLs; the MCP server needs an OAuth grant or application password |
| **Mail** (`internal/mail`) | every message (`wp_mail`) | the message, to `PURSAE_SMTP_HOST` | without `PURSAE_SMTP_HOST` messages are written as `.eml` files to `PURSAE_MAIL_DIR` instead |
| **WebAssembly plugins** (`plugins.go`) | a plugin granted the `http:outbound` permission makes a request | what the plugin sends (public hosts only, 8 s) | do not grant the permission; deactivate the plugin |
| **CAPTCHA** (`captcha.go`) | a form protected by Cloudflare Turnstile or reCAPTCHA (Pursae Contact Form, Pursae Security's login form CAPTCHA) when the keys are set | the visitor's token, IP address and the secret, to the provider's `siteverify` | leave the keys empty |

### Built-in plugins (only while the plugin is active and the feature configured)

| Plugin | Connections | Off |
|--------|-------------|-----|
| Pursae SMTP | the mailer you choose: SMTP, or the API of SendGrid, Mailgun, Postmark, SparkPost, Amazon SES, Brevo, SMTP.com, SendLayer, Resend, SMTP2GO, Gmail or Microsoft (OAuth); failure alerts to Slack, Discord, Teams, a webhook or Twilio; SES bounce notifications (SNS) | the mailer and alert settings |
| Pursae CDN | the S3-compatible bucket (offload, Backup Manager's bucket copy) and the purge APIs of CloudFront, Bunny, KeyCDN or Fastly | the CDN settings (`PURSAE_CDN_SETTINGS`) |
| Pursae Cache | `PURGE` requests to the reverse proxy you configured (Varnish, nginx; private addresses allowed, link-local refused). Preloading runs in-process, without network | the proxy purge setting |
| Pursae SEO | IndexNow (`https://api.indexnow.org`, off by default) with the changed URLs; Google Search Console (OAuth, `googleapis.com`) when connected | the IndexNow module; disconnect Search Console |
| Pursae Pixel | server-side events to Meta Conversions API, TikTok Events API, Pinterest and GA4 Measurement Protocol, with the customer data each platform takes; with Pursae Cookie's consent gate on, only for the platforms a visitor consented to | the server-side tokens of each platform |
| Pursae Contact Form, Pursae Commerce, Pursae Headless | webhooks to the URLs you enter (public hosts only); Commerce: Stripe's API, and the product images of a CSV import (public hosts only) | the webhooks; the payment gateway; image URLs in the CSV |
| Pursae Multi Language | machine translation (OpenAI, DeepL or Google Cloud Translation) with the text to translate | the provider key |
| Pursae Security | Have I Been Pwned's range API (opt-in): only the first five hex characters of a new password's SHA-1 | Passwords › breached-password check |
| Pursae Forum | Google sign-in (OAuth, `accounts.google.com`, `oauth2.googleapis.com`) | the Google client id |

The Web Builder AI module (`PURSAE_BUILDER_AI*`) is not shipped and connects nowhere. Scripts in a
design or theme the administrator imports may load third-party resources in visitors' browsers; the
Content Security Policy above lists the hosts they may use.

## Trust model

Who Pursae trusts with what, and where it draws the line:

- **Administrators are trusted with code, as in WordPress.** A user with `unfiltered_html` may publish
  scripts (Custom HTML blocks, Web Builder HTML and code sections, the theme file editor unless
  `PURSAE_DISALLOW_FILE_EDIT`); those scripts get the CSP nonce. Users without it have their content
  filtered (KSES) and cannot change a stored script (the builder keeps the stored privileged section).
- **An administrator who imports a design trusts its scripts.** A design (an HTML site, a kit) may come
  from anywhere, so the import wizard treats the upload as hostile: it runs nothing while reading it,
  previews it under a sandboxing Content Security Policy, and serves kit files with a sandbox policy. Once imported, the
  design is the site's own code: its scripts run on the published pages with the nonce, like any
  script an administrator publishes. Import only designs you would publish yourself.
- **The editor does not run a design until you say so.** Opening a page of a design you have not
  trusted in the Web Builder element editor runs none of its scripts (Safe mode: a frame without
  `allow-scripts`, and a copy of the markup neutralised by the browser's parser). **Live** mode and
  **Preview** run the design's scripts only after you choose to: **Always for this design** (stored for
  you alone, bound to that installation of the design kit and the hash of its script, module,
  WebAssembly and SVG files, with the hashes of the page's inline code you approved; revocable; asked
  again after the kit is deleted and uploaded again or a commit changes its scripts, for every user
  including the committer) or **This session only** (never stored, covering what the page ran then).
  New or changed inline code or another kit (an insert, a paste, Edit code, an undo) stops the scripts
  before the new markup reaches a frame that runs them, and asks again. Then they run **in the editor's
  origin with your access** (the REST nonce): a script there can do whatever you can. Users without
  `unfiltered_html` never run them (Safe mode only, read-only code). An opaque origin was rejected
  because it breaks editing, the design's same-site fonts and modules and its storage, so the
  author's choice is the boundary. The Live frame itself is served at the page's own address (the server's
  permalink of the post, never a path the request names; admin, REST and `.php` paths are refused)
  under the front end's Content Security Policy, with `Referrer-Policy: no-referrer`, only to the user
  who made it, for 15 minutes, from a capped memory store. When it cannot be loaded the canvas stays in
  Safe mode: the design never runs in a frame under the editor's own policy. The editor page's policy
  adds only what its frames need (style and font origins for a page of code sections; script, connect
  and frame origins only for a mixed page, whose Preview runs the design). Details:
  [Web Builder code sections › security model](../web-builder-code-sections.md#security-model).
- **Plugins are untrusted** until you grant permissions: WebAssembly with no filesystem, network only
  as granted and public hosts only, memory and time limits, signed packages.
- **Contributors and authors** never see other users' drafts, private or password-protected pages
  through the builder's kit routes (only an `edit_theme_options` + `unfiltered_html` user sees every
  page), and markup they write can print only the custom
  fields a field group defines, never other post meta (a site import's code bindings add only the `design_<n>` / `list<n>_<k>` fields
  the import itself stores). Header and footer templates' form settings
  (recipients, redirects) are sent to the editor only for users with `edit_theme_options`.

## Reporting a vulnerability

Report it privately, never in a public issue: the repository's
[security policy](https://github.com/pursae/pursae/blob/main/SECURITY.md) (`SECURITY.md`) says how,
which versions get fixes and what to expect.
