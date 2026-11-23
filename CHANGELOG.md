# Changelog

All notable changes to Pursae are recorded here, in the [Keep a Changelog](https://keepachangelog.com/en/1.1.0/)
format; versions follow [Semantic Versioning](https://semver.org/). Entries start with 0.3.0; for 0.2.x and
older, see the [GitHub releases](https://github.com/pursae/pursae/releases).

Every pull request that changes behavior adds a line under **Unreleased**. Releasing moves those lines
under a new `## [x.y.z] - YYYY-MM-DD` heading, which `tools/release.sh` publishes as the release notes
(see [Publishing a release](docs/developer/operations/releases.md)).

## [Unreleased]

### Added

- **Cart and Checkout blocks.** The block editor inserts `woocommerce/cart` and `woocommerce/checkout` and edits
  their sections (cross-sells, coupon form, returning customer login, order notes, return to cart link),
  headings, the empty cart message and the checkout's company, apartment and phone fields. The front end
  renders them on the server with the cart and checkout pages of `[goocommerce_cart]` /
  `[goocommerce_checkout]`. Pages imported from WordPress keep WooCommerce's Cart and Checkout blocks and work
  as they were set up there: removed inner blocks hide their sections, the Empty cart block's content shows,
  unknown blocks are ignored.

- **Backup Manager: remote storage.** A new Remote storage tab copies backups to an S3-compatible bucket
  (AWS S3, Cloudflare R2, Backblaze B2, Wasabi, MinIO …), encrypted on the server with a passphrase first
  (AES-256-GCM, scrypt), streamed as a multipart upload so large backups are never held in memory. Optional
  automatic upload of every new manual and scheduled backup, retention of the newest *N* in the bucket,
  *Download and restore* from the bucket, and a connection test. The secret key and passphrase are stored
  sealed and never shown again; `PURSAE_BACKUP_S3_*` and `PURSAE_BACKUP_PASSPHRASE` keep them out of the
  database. The endpoint must be https on a public address, checked after DNS resolution
  (`PURSAE_BACKUP_S3_ALLOW_PRIVATE=1` for a MinIO on your own network). CLI: `pursae backup remote
  status|set|test|list|push|pull|delete` and `pursae backup decrypt`; REST: `/pursae/v1/backups/remote`.
  Without a bucket nothing changes.
- The documented `pg_dump` / `pg_restore` database backup is now tested in CI against PostgreSQL 16 and 17.
- **Passkeys in Pursae Security.** Users add any number of named passkeys under Users › Security and log in
  with them from the login form, by the button or the browser's passkey autofill, without typing a username.
  After a password, a passkey also works as the second step. Settings › Two-Factor switches passkeys off,
  decides whether a passkey sign-in counts as two-step login (default) or only replaces the password, and can
  require a passkey per role. Challenges are single use and expire after 5 minutes; a passkey whose signature
  counter goes backwards is refused as a possible copy.
- **Commerce: stock is reserved during checkout.** Placing an order holds its stock for *Hold stock
  (minutes)* while the customer pays (WooCommerce's `wc_reserve_stock_for_order`, new table
  `goo_reserved_stock`, expand-only migration `0113`), so the last unit can no longer be sold to two
  customers paying at the same time: the second checkout is told it is out of stock before paying. The hold
  ends when the order is paid, put on hold, cancelled or failed, or when the time runs out.
- **Prometheus metrics and slow query log.** `PURSAE_METRICS=1` with `PURSAE_METRICS_ADDR` (own listener) or
  `PURSAE_METRICS_TOKEN` (bearer token, `/metrics` on the site port) exposes request counts and duration
  histograms by route group and status class, requests in flight, the database pool, the Go runtime and build
  info, written by a small built-in text-format writer: no new dependency, fixed memory (about 12 KB of heap, no
  allocation per request), no URL or user labels. Off by default and not installed at all while off.
  `PURSAE_SLOW_QUERY=250ms` logs (SQL text only, never arguments) and counts slow database queries. See
  [metrics](docs/developer/operations/metrics.md).

- **Pursae SEO: SEO Details column and Table of Contents.** The posts and pages lists show one SEO Details
  column: the score as "84 / 100", the focus keyword, the schema type, readability and internal, external and
  incoming link counts. A new Table of Contents block (Pursae SEO category) lists the post's headings with
  jump links and gives headings without an id one; SEO › General Settings › Table of Contents sets its
  defaults (title, heading levels, numbered, collapsible) and can insert it automatically before the first
  heading of chosen post types.
- **Block theme sites point to Web Builder.** Importing a WordPress site that uses a block theme turns its
  navigation menus (`wp_navigation`) into ordinary menus (links to imported posts and terms kept, submenus
  as sub-items, the first one on the Primary Menu location when it is free). Its templates, template parts,
  global styles and fonts are counted in one report line with a link to the Theme Builder instead of one
  "Invalid post type" line each. New guide: [coming from the Site Editor](docs/developer/web-builder-site-editor.md).
- **Web Builder site style.** One brand colour, font pair, fonts and corners for the whole site (the Site
  Editor's global styles): **Items › Globals › Site style** saves a design's look as the site style, and
  pages and templates that follow it share it. New pages and templates start with it; after a change,
  Settings › Web Builder › Regenerate pages publishes the designs that follow it again. Contract: additive
  `Boot.siteStyle`, `doc.site.global`, site-settings `site_style` / `site_style_outdated`.
- **Web Builder drop-down menus.** Navigation sections bound to a menu location and the Menu widget open
  sub-menus as drop-downs on hover and keyboard focus (the third level indented inside); in the mobile
  menu they sit indented under their parent. Pages published earlier keep the flat list until they are
  published again. New `data-wb-menu-tree="dropdown"` placeholder (contract §11.5).

### Changed

- **Translations completed.** The 567 admin strings added since the language packs were last updated (Pursae
  product updates, forum, remote storage, import recovery, setup wizard and more) are now translated in every
  `web/languages/wpgo-*.json` pack except the English variants; regional variants share their base language.

## [0.5.0] - 2026-11-22

0.5.0, the stable beta: the contract is frozen (see [compatibility](docs/developer/compatibility.md)),
environment variables are renamed to `PURSAE_*`, and a whole WordPress site, WooCommerce store included, can
move to Pursae with users keeping their passwords. Same code as 0.5.0-rc.1; this section repeats its notes.
Later releases are 0.5.1, 0.5.2…

### Added

- **Move a WordPress site to Pursae (WP to Pursae).** Tools › Import › From WordPress imports a whole site
  from the WP to Pursae plugin's package. Users keep their WordPress passwords; content, media, menus and
  settings move, and so does the WooCommerce store: products, tax rates, coupons, orders with refunds and
  notes, and customers. Upload a package with its key, or generate a one-time migration token and let the
  plugin send it. `pursae import wordpress <package> --key=…` and `pursae import wordpress-token` do the
  same from the command line. Importing the same site again, or resuming one that stopped, adds only what
  is new. Accounts are merged only when the e-mail address matches; imported orders keep their WordPress
  order numbers. Packages and extracted files of imports that stopped are deleted after 7 days, and import
  reports keep no e-mail addresses or logins.
- **WP to Pursae WordPress plugin** (`integrations/wordpress/wp-to-pursae`). Install it on a WordPress
  site, open Tools › Move to Pursae, and move the whole site in one go. The package is built in short,
  resumable requests; on multisite only a super admin can use it. Download an encrypted package, or send
  it straight to Pursae with a one-time migration token, resuming if the connection drops. Works with
  WooCommerce's HPOS and the older order storage. WP-CLI: `wp pursae export`, `wp pursae push`. Turkish
  translation included. Docs: [Migrate from WordPress](docs/developer/operations/migrate-from-wordpress.md).
- `docs/developer/compatibility.md`: what is stable across 0.5.x (REST `wp/v2` and `pursae/v1`, CLI,
  `PURSAE_*` variables, plugin ABI v1, theme package, schema rules, MCP tool names), what is not, the
  deprecation process and versioning.
- Plugin manifest field `abi` (plugin ABI version, default 1; set it only for ABI 2 and later, together
  with `requires` 0.5.0 or later). Pursae refuses to install or activate a plugin built for an ABI it does
  not support (this release supports 1).
- Release archives `pursae-<os>-<arch>.tar.gz` (`.zip` on Windows) with the binary, LICENSE, NOTICE,
  LICENSES/ and a README, next to the bare binaries; SHA256SUMS covers both.
- A hardened systemd unit (`deploy/systemd/pursae.service`) that keeps Dashboard › Updates working, and a
  guide to installing a release binary as a service, updating and uninstalling.
- GitHub issue forms (bug report, feature request) and a pull request template.

### Changed

- Environment variables are now named `PURSAE_*` (`PURSAE_SITE_URL`, `PURSAE_SECRET`, …). Compose files,
  `.env.example`, the Docker image and the docs use the new names; compose still reads an existing `.env`
  with `WPGO_*` names.
- `pursae` is the binary's primary name in `make build` output and the docs.
- PostgreSQL 16 and 17 are the supported versions; CI runs the test suite on both.
- Backup Manager's Page builder part backs up the Web Builder's `web_builder_*` tables.
- Docs: the REST API endpoint reference matches the code, and a test keeps it in sync; feature docs for
  Custom Fields, Web Builder, Maintenance and Multi Language are corrected; README lists what is not
  supported; the install guide's build command and systemd example are corrected.

### Deprecated

- `WPGO_*` environment variables: still read when the `PURSAE_*` name is unset or empty, with a start-up
  warning naming the replacement. Docker users update the compose files before renaming `.env`. Removed no
  earlier than 1.0.
- The `wpgo` binary name (`bin/wpgo`, `/usr/local/bin/wpgo`): use `pursae`. Removed no earlier than 1.0.

### Fixed

- Personal data export and erasure: Contact Form entries, Web Builder form submissions and contact form
  webhook jobs match on the form's e-mail fields and on the account that sent them while signed in; an
  address typed into another field no longer makes the message that person's. Messages stored before this
  release match fields named `email`, `e-mail`, `your-email`, `eposta` or `email-NNN` and, for Contact Form
  entries, the e-mail fields of their form as it is now.
- Users › Delete: deleting two accounts that liked the same forum post no longer fails. The likes a deleted
  account gave stay as anonymous likes, so posts keep their like counts and authors keep their points.

### Removed

- Unused plugin logo images from the admin assets.

### Database

- Migration 0110: `gontact_entries` and `web_builder_form_submissions` get `email_fields` and `user_id`.
- Migration 0111: `wp2p_migrations` and `wp2p_map` for WordPress imports.
- Both only add, so 0.4.x still runs on the new schema.

### Internal

- CI smoke-tests the Linux and macOS release binaries, builds and checks the Docker image, and runs a
  wp-admin browser smoke test (`make e2e`); `make smoke` smoke-tests a local build.
- Docker base images are pinned by digest and govulncheck to a fixed version; Dependabot proposes weekly
  updates.

## [0.5.0-rc.1] - 2026-11-22

Release candidate for 0.5.0, the stable beta: the contract is frozen (see
[compatibility](docs/developer/compatibility.md)), environment variables are renamed to `PURSAE_*`, and a
whole WordPress site, WooCommerce store included, can move to Pursae with users keeping their passwords.
Published on the beta channel; 0.5.0 follows after at least a week on real sites.

### Added

- **Move a WordPress site to Pursae (WP to Pursae).** Tools › Import › From WordPress imports a whole site
  from the WP to Pursae plugin's package. Users keep their WordPress passwords; content, media, menus and
  settings move, and so does the WooCommerce store: products, tax rates, coupons, orders with refunds and
  notes, and customers. Upload a package with its key, or generate a one-time migration token and let the
  plugin send it. `pursae import wordpress <package> --key=…` and `pursae import wordpress-token` do the
  same from the command line. Importing the same site again, or resuming one that stopped, adds only what
  is new. Accounts are merged only when the e-mail address matches; imported orders keep their WordPress
  order numbers. Packages and extracted files of imports that stopped are deleted after 7 days, and import
  reports keep no e-mail addresses or logins.
- **WP to Pursae WordPress plugin** (`integrations/wordpress/wp-to-pursae`). Install it on a WordPress
  site, open Tools › Move to Pursae, and move the whole site in one go. The package is built in short,
  resumable requests; on multisite only a super admin can use it. Download an encrypted package, or send
  it straight to Pursae with a one-time migration token, resuming if the connection drops. Works with
  WooCommerce's HPOS and the older order storage. WP-CLI: `wp pursae export`, `wp pursae push`. Turkish
  translation included. Docs: [Migrate from WordPress](docs/developer/operations/migrate-from-wordpress.md).
- `docs/developer/compatibility.md`: what is stable across 0.5.x (REST `wp/v2` and `pursae/v1`, CLI,
  `PURSAE_*` variables, plugin ABI v1, theme package, schema rules, MCP tool names), what is not, the
  deprecation process and versioning.
- Plugin manifest field `abi` (plugin ABI version, default 1; set it only for ABI 2 and later, together
  with `requires` 0.5.0 or later). Pursae refuses to install or activate a plugin built for an ABI it does
  not support (this release supports 1).
- Release archives `pursae-<os>-<arch>.tar.gz` (`.zip` on Windows) with the binary, LICENSE, NOTICE,
  LICENSES/ and a README, next to the bare binaries; SHA256SUMS covers both.
- A hardened systemd unit (`deploy/systemd/pursae.service`) that keeps Dashboard › Updates working, and a
  guide to installing a release binary as a service, updating and uninstalling.
- GitHub issue forms (bug report, feature request) and a pull request template.

### Changed

- Environment variables are now named `PURSAE_*` (`PURSAE_SITE_URL`, `PURSAE_SECRET`, …). Compose files,
  `.env.example`, the Docker image and the docs use the new names; compose still reads an existing `.env`
  with `WPGO_*` names.
- `pursae` is the binary's primary name in `make build` output and the docs.
- PostgreSQL 16 and 17 are the supported versions; CI runs the test suite on both.
- Backup Manager's Page builder part backs up the Web Builder's `web_builder_*` tables.
- Docs: the REST API endpoint reference matches the code, and a test keeps it in sync; feature docs for
  Custom Fields, Web Builder, Maintenance and Multi Language are corrected; README lists what is not
  supported; the install guide's build command and systemd example are corrected.

### Deprecated

- `WPGO_*` environment variables: still read when the `PURSAE_*` name is unset or empty, with a start-up
  warning naming the replacement. Docker users update the compose files before renaming `.env`. Removed no
  earlier than 1.0.
- The `wpgo` binary name (`bin/wpgo`, `/usr/local/bin/wpgo`): use `pursae`. Removed no earlier than 1.0.

### Fixed

- Personal data export and erasure: Contact Form entries, Web Builder form submissions and contact form
  webhook jobs match on the form's e-mail fields and on the account that sent them while signed in; an
  address typed into another field no longer makes the message that person's. Messages stored before this
  release match fields named `email`, `e-mail`, `your-email`, `eposta` or `email-NNN` and, for Contact Form
  entries, the e-mail fields of their form as it is now.
- Users › Delete: deleting two accounts that liked the same forum post no longer fails. The likes a deleted
  account gave stay as anonymous likes, so posts keep their like counts and authors keep their points.

### Removed

- Unused plugin logo images from the admin assets.

### Database

- Migration 0110: `gontact_entries` and `web_builder_form_submissions` get `email_fields` and `user_id`.
- Migration 0111: `wp2p_migrations` and `wp2p_map` for WordPress imports.
- Both only add, so 0.4.x still runs on the new schema.

### Internal

- CI smoke-tests the Linux and macOS release binaries, builds and checks the Docker image, and runs a
  wp-admin browser smoke test (`make e2e`); `make smoke` smoke-tests a local build.
- Docker base images are pinned by digest and govulncheck to a fixed version; Dependabot proposes weekly
  updates.

## [0.4.1] - 2026-11-22

A livelier login screen, a readable Pursae News box, the team on About Pursae, forum data anonymized on
erasure, WordPress-matching pattern permissions, and the last Elementor-derived parts removed (the
project is GPL-2.0-or-later throughout).

### Added

- Login screens: the Pursae mascot comes alive. Its eyes follow the pointer, it glances around and
  blinks, and it looks away while you type a hidden password (and peeks when you show it).
  Reduced-motion users get a static mark. Sites with a custom Hub logo are unchanged.
- About Pursae: an "Our team" section with photo cards and social links (photos bundled with the admin,
  light and dark modes, Turkish translation).

### Changed

- Pursae Forum: Tools › Erase Personal Data anonymizes a member's forum data. Their topics and replies
  stay published as "Deleted user", with author, IP and revision authorship removed. Likes they gave stay
  as anonymous likes, so no other member's points change. Their points, subscriptions and forum profile
  (bio, uploaded photo, badges, settings, draft) are deleted. Reports they filed stay without reporter or
  note. The account and its forum sign-in are kept.
- Pursae Forum: posts without an author (erased or deleted accounts) show "Deleted user" across the
  front end, replacing "Former member" and the missing name in lists.
- The admin footer's Pursae link goes to pursae.com instead of GitHub.
- The plugin activation screen ("How would you like to set it up?") is left-aligned like the setup guide.

### Fixed

- Dashboard › Pursae News shows the release notes readably: rendered Markdown (no raw `###`, `-` or
  backticks, no double-escaped `&#39;`), a localized release date with relative age, an Update available /
  New badge, a short digest with colour-coded Added / Changed / Fixed / Security labels, and links to the
  full notes, Updates and Update now. Dashboard › Updates shows the full release notes.
- Patterns: creating a pattern requires `publish_posts`, as in WordPress. Contributors can still read and
  use patterns but no longer see Add Pattern, Import from JSON or the editor's Create pattern item, and
  `POST /wp/v2/blocks` returns 403 `rest_cannot_create` for them.
- Geist, Geist Mono, Google Sans and Google Sans Code from the Web Builder's font library are served
  locally like the other library fonts.

### Removed

- The last Elementor-derived parts: migrations 0005–0007 are empty steps (existing databases keep their
  history; 0028 already dropped the tables), the Google Fonts list generated from Elementor is replaced by
  the Web Builder's own font library, and Elementor's unused plugin icon is deleted. NOTICE no longer lists
  Elementor, and `LICENSES/GPL-3.0.txt` is gone.

### Database

- Migration 0109: `forum_likes.user_id` is nullable (anonymous likes); one like per member and post is
  enforced by a partial unique index. It only adds, so 0.4.0 still runs on the new schema.

## [0.4.0] - 2026-11-21

Editor gaps, complete personal-data tools, licensing notices and a security policy, plus WordPress
migration of passwords and WooCommerce products.

### Added

- REST `POST /wp/v2/media/{id}/edit`: the block editor's image crop, rotate and flip save a new
  "-edited" attachment, as in WordPress.
- Link previews in the block editor: `/wp-block-editor/v1/url-details` returns the page title,
  description, icon and image (cached for an hour).
- Patterns screen (`edit.php?post_type=wp_block`, Appearance › Patterns) with sync status, author,
  pattern categories and date; edit, trash, Export as JSON and Import from JSON. Patterns open in the
  block editor.
- `wp_pattern_category` taxonomy: REST CRUD, assignment through `/wp/v2/blocks`, the editor's category
  picker, an admin screen, and WXR export and import.
- Users moved from WordPress keep their passwords: WordPress 6.8+ bcrypt, phpass, plain bcrypt, legacy
  MD5 and PHP argon2i/argon2id hashes are verified and rehashed to argon2id at the first sign-in.
  `pursae user create --user_pass_hash` imports such a hash as-is.
- WordPress import: WooCommerce products and variations from a WXR file go into Pursae Commerce, with
  prices, stock, attributes, images, categories, tags, shipping classes, upsells, cross-sells, grouped
  products and reviews. Orders, refunds and coupons are counted in the report, not imported.
- `SECURITY.md`: supported versions, private vulnerability reporting through GitHub security advisories,
  response targets and disclosure policy.
- Security docs: every outbound connection of the core and the built-in plugins and how to turn each
  off, the core firewall (`WPGO_FIREWALL`), the setup token and the Web Builder trust model.
- `LICENSES/` with the full texts of every license used by bundled components; the Docker image ships
  `LICENSE`, `NOTICE` and `LICENSES/` in `/usr/share/doc/pursae/`. The Web Builder build writes
  `web/web-builder/THIRD_PARTY_LICENSES.txt`.

### Changed

- `NOTICE` lists every bundled third-party component (WordPress libraries, block editor packages,
  fonts, Web Builder, Flipbook, map and geolocation data, Go modules, Docker base image) with version,
  license, copyright holder and source.
- Web Builder (breaking): a code binding's custom field whose name no Pursae Custom Fields field group
  defines (other than a site import's `design_<n>` / `list<n>_<k>`) now renders empty instead of the
  post's plain meta; define a field group for it. `web_builder_bind` warns about such names.

### Fixed

- Personal data export and erasure now cover Pursae Contact Form entries, Web Builder form
  submissions, maintenance page subscribers, Pursae Forum data (posts, profile, likes, points,
  subscriptions, reports), Commerce billing/shipping fields, saved payment methods and gift cards,
  contact form notifications with the person as Reply-To, finished mail jobs and contact form webhook
  jobs; security logs are exported and reported as retained.
- `/wp-admin/edit.php?post_type=wp_block` no longer answers "Invalid post type."
- Patterns created as "Not synced" keep their sync status (`meta.wp_pattern_sync_status`).

### Security

- URL details are fetched only from public addresses (checked at connect time and on every redirect),
  on ports 80/443/8080, with a 10 s timeout and a 150 KB cap.
- Work factors of imported password hashes are capped, and a wrong password against a legacy hash takes
  as long as the unknown-user path, so response times do not reveal which accounts exist.
- Web Builder: a code binding no longer prints arbitrary non-underscore post meta of the posts a loop
  shows.

## [0.3.0] - 2026-11-21

Safe upgrades: every release is now gated on green CI, every pull request proves that upgrading from
older releases keeps the data, and an older binary no longer runs silently on a newer schema.

### Added

- Release gate: `tools/release.sh` refuses to publish a commit without a successful CI run on GitHub
  (every job green), a stable release from a branch other than `main`, or a stable version without a
  CHANGELOG section. `RELEASE_SKIP_CI_CHECK=1` overrides the CI check in an emergency, with a warning.
- Release notes come from this file's section for the version (`NOTES_FILE` still overrides them).
- golangci-lint runs in CI (new issues only) and as `make lint`.
- Upgrade-path test (`make upgrade-test`, CI job `upgrade`): installs v0.2.0 and the last three releases,
  seeds users, content, media, menus, settings and a Web Builder page through each release's own paths,
  then migrates to the current code and checks that every seeded value, login and file survived.
- Migration lint: new migrations need a unique `NNNN_name.sql` number, and destructive statements
  (DROP TABLE/COLUMN, ALTER … TYPE, RENAME) need a `-- pursae:contract` marker.
- Schema guard: the database records the oldest release that may run on it (`schema_meta`, floor
  0.2.4). A binary below the floor that finds migrations it does not know refuses to start with an
  actionable message; `WPGO_ALLOW_OLDER_BINARY=1` overrides it. `/readyz` reports the condition.
- Automatic core updates write a database backup (`pre-update-*.zip`, newest 3 kept) first and skip
  the update, with a notice in Dashboard › Updates, when the backup fails.

### Changed

- A core update is confirmed when `/readyz` answers 200 (database reachable, no pending migration,
  schema guard satisfied) instead of when `/wp-login` answers below 500.
- Automatic updates stay within the installed minor version; 0.3.x → 0.4.0 needs a click in
  Dashboard › Updates.
- Reinstall refuses to go back to a release below the schema's floor.
- Docs: `upgrading.md` describes the expand/contract policy for schema changes instead of claiming that
  releases only add schema (0028, 0038 and 0105 drop tables or columns).
