# Changelog

All notable changes to Pursae are recorded here, in the [Keep a Changelog](https://keepachangelog.com/en/1.1.0/)
format; versions follow [Semantic Versioning](https://semver.org/). Entries start with 0.3.0; for 0.2.x and
older, see the [GitHub releases](https://github.com/pursae/pursae/releases).

Every pull request that changes behavior adds a line under **Unreleased**. Releasing moves those lines
under a new `## [x.y.z] - YYYY-MM-DD` heading, which `tools/release.sh` publishes as the release notes
(see [Publishing a release](docs/developer/operations/releases.md)).

## [Unreleased]

### Fixed

- **Pursae Security:** sign-in attempts that arrive in parallel with the one that locks a network out are refused as
  well, so a burst of guesses cannot pass the failure limit.
- Theme installs, release notes and the Hub client: fixes from pull request #229.

## [0.6.0] - 2026-11-26

### Security

More fixes from the internal review. Upgrade every site; sites that run Pursae Commerce should upgrade right away.

- **Pursae Commerce:** a cancelled order's Stripe Checkout page is closed, and points and gift card balances
  returned by a cancellation are taken back if the order is paid after all. Booking person counts are limited.
  REST `meta_data` no longer writes account, security or Web Builder keys.
- **Scoped Application Passwords and OAuth tokens:** writing the CDN, SMTP, Security and Headless option groups
  needs full access; through `wc/v3/customers` they change or delete only customer and subscriber accounts.
- **Themes:** theme templates get public copies of authors and posts, without e-mail addresses, password data
  or user meta.
- **Pursae SEO:** head tags and sitemaps follow Memberships restrictions.
- **Hardening:** the "account under attack" CAPTCHA covers the Commerce, Forum and Headless sign-ins; the contact
  form CAPTCHA table cannot be filled; the firewall rate limit counts dynamic requests with a file extension;
  the maintenance gate stays closed on unrouted paths; maintenance custom CSS is escaped.

### Changed

- **Theme templates:** `.Q.Author` no longer carries the e-mail address, password hash, user meta, roles or
  capabilities, and post passwords are empty (`.HasPassword` still tells). `AuthorEmail` is always empty; use the
  new `AuthorAvatar`. ACF `get_field` on users and comments returns registered fields only.
- **Stripe:** the Checkout page stays open for the store's hold-stock time (31 minutes to just under a day; one
  hour when hold stock is off) instead of 24 hours. A cancelled order that is paid late and can no longer take
  back its returned points or gift card balance is put on hold for review.
- **Scoped credentials:** changing the maintenance page's custom CSS needs the Themes scope, as custom HTML does.
  `wc/v3` `meta_data` ignores Web Builder keys and `_wp_page_template` for every caller.
- **Headless:** `POST /auth/local` answers 403 `captcha_required` when CAPTCHA keys are set and the login CAPTCHA
  is on or the account is under attack; send the token as `captchaToken`.
- **Bookings:** more than 10,000 persons per type or in total (or more than the product's maximum) is refused.

## [0.5.9] - 2026-11-26

### Security

This release fixes security issues found in an internal review of 0.5.8. GitHub security advisories with
the details will follow. Upgrade every site; sites that run Pursae Commerce should upgrade right away.

- **Pursae Commerce:** cart lines are checked again before an order is placed. Order keys are compared in
  constant time, and a failed database read in a Stripe webhook is retried instead of ignored.
- **Themes:** theme templates read only public settings, theme scripts load only from the theme itself, and
  Live Preview runs the previewed theme in a sandbox.
- **REST API:** callers who are not signed in can send smaller request bodies, and responses no longer show
  database errors.
- **Sign-in limits:** failed attempts are counted per client network, so failures from one network no longer
  lock an account out everywhere.
- **Roles and credentials:** authors and editors get stricter checks for private media, products and other
  content types. Scoped Application Passwords and OAuth tokens get stricter checks too.
- **Hardening:**
  - admin and signed-in responses are not cached;
  - password-reset requests take the same time whether or not the account exists;
  - an OAuth error before consent shows a local page instead of redirecting;
  - the maintenance password set over REST is stored hashed;
  - background tasks recover from panics;
  - a release build no longer trusts a Pursae Manager on a local address.

### Changed

These fixes change what some sites do. Check the ones that apply to you:

- **Editors and products:** editors can no longer edit products or open the product category and tag screens.
  Shop managers and administrators can. A custom role that should edit products needs the `*_products`
  capabilities.
- **Content of disabled plugins:** while Pursae Commerce is off, the generic edit endpoints no longer open
  products, for administrators too. Forum types behave the same way while the forum is off, and so do
  disabled Custom Fields types.
- **Contact forms:** on the generic endpoints, contact forms (`wpcf7_contact_form`) need `manage_options`. The
  contact form screens themselves are unchanged.
- **Content-scoped credentials:** Application Passwords and OAuth tokens that hold only the **content** scope
  (the MCP default included) save HTML filtered at the Author level. Saving raw HTML or script needs the
  **themes** scope. The scope picker marks the themes, plugins and users scopes as equal to full access.
- **Users-scoped credentials:** credentials with the **users** scope can no longer change other accounts'
  e-mail address, password or role.
- **Plugin permissions:** activating a plugin over REST, MCP or `pursae plugin activate` grants only its
  non-dangerous permissions unless you name more. `--grant=all` now means every non-dangerous permission.
  Grant `content:html` and `admin:scripts` by name.
- **Theme package scripts:** a theme package whose manifest loads a script from a remote URL is refused. The
  site falls back to the default theme and the admin shows a notice. No bundled or library theme does this.
- **Sign-in limits (core and Pursae Security):**
  - Failures are counted per client network: an IPv4 address, or an IPv6 /64.
  - Pursae Security's "Per username" lockout locks the account only for the network that failed. A
    remembered device of the account is exempt.
  - An account with many recent failures from all networks counts as under attack. A network that has not
    signed in to it within a day then waits up to 3 seconds before its password is checked, gets two
    failures, and sees the CAPTCHA on `/wp-login` when keys are set.
  - Application Passwords have a budget of their own per network.
  - Per-account lockouts made by earlier versions are no longer enforced.
- **Request body size:** REST JSON bodies are limited to 1 MiB for visitors and front-end members
  (subscribers, customers, forum members), and to 32 MiB for signed-in staff and API credentials. Larger
  bodies get 413 `rest_request_too_large`.

## [0.5.8] - 2026-11-26

### Changed

- **Faster pages with fewer database queries.** A page with child-page and child-category menu links and the
  Latest Posts / Latest Comments blocks went from 65 to 22 queries: ancestors and category paths are resolved
  once per request, menus and the privacy-policy link no longer load post content, previous/next links read
  only what they show, and lists that throw the total away skip `count(*)`. A new partial index
  (`posts_sticky`, migration 0115, built concurrently) finds sticky posts without scanning every post.
- **Pursae Commerce:** a variable product's page and gallery load their images in one query instead of about six
  per variation; the cart page, mini-cart and Store API batch item images, reuse loaded products for
  cross-sells and tax rates within a request, and skip a repeated cart recalculation.
- **Less memory and CPU per request.** The Pursae Security and Maintenance settings caches now actually hit
  (they re-read settings on every request), Cookie consent, Multi Language and CSP settings are memoised the
  same way, the page cache reuses its gzip writers (about 1 MB less garbage per stored page) and stores
  compressed copies at their exact size, three whole-page scans (block styles, upload URLs, image tags) are
  10–130× faster, the forum's URL patterns no longer hold about 8 MB of memory in every process, and
  non-English admin pages translate with less work.
- **Bounded resource use.** On-the-fly CDN image transformations run at most `min(4, CPUs/2)` at a time; a
  multipart request keeps at most 8 MB in memory (1 MB for the store's form check) and spools the rest to the
  temp directory; the backup screen keeps the 20 most recent finished jobs in memory (archives are untouched).

### Fixed

- Theme previews that were evicted or rebuilt no longer leak their whole sandbox site in memory.
- English admin pages no longer grow the admin script-strings cache by one entry per request.
- **Release notes on the Dashboard** keep literal trailing `#`s, blank lines at the start of code blocks and
  shorter backtick fences inside a code block; code spans close only on a backtick run of the same length, and
  excerpts keep a whole word at the limit without running past it.
- **Pursae Manager catalogs:** releases sort by full SemVer (large numeric parts too); a catalog that exceeds its
  page limit, stops short or answers `null` is reported and the last complete cached catalog is kept.
- **Core updates:** an invalid rollback backup (for example a directory at the backup path) is refused before the
  running binary or the update state is touched, and retention never removes a directory.
- **Forum:** whitespace-padded sorts and the search `/page/1/` alias redirect to their canonical URL; only local
  images use attachment metadata for the social image.
- **Web Builder site import:** mixed-case index files belong to their folder, imported folders get valid UTF-8
  placeholder titles, and head elements are shared only when every source page has them complete.
- Packaged theme assets named `index.html` are served at their explicit URLs, and plugins removed after a load
  failure no longer keep a retry delay.

## [0.5.7] - 2026-11-25

### Added

- **Build your own plugin / theme.** Plugins has a **Build your own plugin** button next to Add Plugin and
  Themes a **Build your own theme** button next to Add Theme. The pages explain both kinds (Wasm or native
  plugins; theme packages or Web Builder themes), how to share one with everyone (email hello@pursae.com and
  open a forum topic; after review it is signed and listed) and how to keep one for your own site. New
  developer docs: native plugins, themes with Pursae Web Builder, sharing your theme, and "Your own plugins
  and themes" in Upgrading.

### Changed

- **Core updates no longer hide what they break.** An active plugin that cannot load (after an update: a
  newer `requires`, an unsupported ABI, an untrusted signer, a missing package) stays active, says why on the
  Plugins screen and emails the administrator once, instead of only logging every 10 seconds. A theme package
  that fails to load is named on Themes and the Dashboard (with the default theme shown meanwhile and the
  theme choice kept). Edited files of a bundled theme that Pursae changed since are listed in the Theme File
  Editor with the new original and Restore. Updating an uploaded theme keeps the files edited on the server
  in `themes/.backup/`.
- **A manual core update takes a backup first**, like the automatic one, and pre-update backups now hold the
  plugin packages and theme files besides the database. A failed backup stops the update.

### Fixed

- **The admin bar showed the Hub's dark logo on the dark bar.** With only a light-mode logo set on the Hub,
  the admin bar's mark now keeps the shipped light mark instead of the light-mode logo.

## [0.5.6] - 2026-11-25

### Added

- **Forum: a social image setting.** Forums › Settings has a **Social image** field (a site path or an http(s)
  address). Public forum pages use it for `og:image` and `twitter:image`; empty keeps the Pursae SEO default image.

## [0.5.5] - 2026-11-25

### Added

- **Plugin settings: an `image` field type with the media library picker.** A Wasm plugin's settings page or
  meta box can declare `Type: "image"`: the field shows the image's address, a **Select image** button that
  opens the media library (upload a file or pick one), a preview and Remove. The value is the image URL (an
  http(s) URL or a site-relative path; anything else is refused on save). Blocks and Web Builder widgets
  refuse the type for now. Plugins using it should require 0.5.5.

### Changed

- **The catalog follows Pursae Manager's order.** The theme library and Add Plugins list the Manager's
  listings in the order its operator set on the Manager's Plugins/Themes screens instead of by name; a
  directory theme library and local packages stay sorted by name.
- **Add Plugins reads the Manager catalog live.** A plugin published on the Manager shows at once instead of
  after the 12-hour cache expires; the cached copy is the fallback when the Manager cannot be reached.
- **Release notes as cards.** On Dashboard › Updates every section of the release notes has its colour
  (Added green, Changed blue, Fixed purple, Security red) and each item is a card that shows its title;
  a click opens the details.
- **Installer wording.** The setup token step is headed "Pursae Install" and names the command
  `pursae setup-token`.

### Security

- **Front-end members get no wp-admin and no API credentials.** Accounts whose only capabilities are
  `read`, `level_0` or `forum_participate` (subscribers, forum members, store customers) are sent from
  wp-admin to the forum account page or the home page, cannot create Application Passwords, and their
  existing Application Passwords, OAuth tokens and Goocommerce `ck_` keys no longer sign in. `ck_` keys also
  stop for an account Pursae Security holds for a password change or two-step setup. Authors, editors,
  moderators, shop managers and custom roles keep wp-admin and their credentials.

### Fixed

- **GoCDN S3 client and media metadata.** Copy and multipart results, `Content-Range` totals, listing
  names and continuation tokens are checked; MP3 (Xing, ID3v2.3), MP4 and WAV durations and bitrates are
  read correctly.
- **Settings and CLI.** Cached settings stay consistent with the database and keep their autoload
  visibility; CLI write failures are reported, YAML output quotes numeric-looking strings, the last of a
  repeated flag wins, `--` is kept and malformed global flags are reported.
- **Hello Pursae only with Pursae Web Builder.** The builder's blank-canvas theme is no longer listed,
  previewed or activated while the plugin is off (Themes screen, REST, MCP and CLI); a site that still
  runs it keeps it.
- **Theme cards keep the WordPress layout.** On Appearance › Themes every card has the same footer height:
  the active card shows Customize only (demo content stays in Theme Details), and Activate / Live Preview
  sit beside the name instead of covering it. On Add Themes the category line under the name no longer
  runs out of the card. In the theme preview the close, previous and next icons are centered.

## [0.5.4] - 2026-11-24

### Changed

- **Go plugin SDK: the `wpgotest` fake host behaves like a real site.** `query_posts` defaults to the `post`
  type, accepts `per_page` 0–100 and orders by date by default (also `title`, `modified`, `id`); transients
  expire; nested test hosts restore in order. A plugin test that queried pages without a type, passed
  `per_page` -1 or relied on ID order may need updating.

### Fixed

- **Custom post types keep their own admin list.** The list of a type created in Pursae Custom Fields
  highlights that type's menu instead of Posts, and its status views (All, Published, Drafts …), Mine, the
  author filter, **Add New** and Screen Options stay on that type instead of opening the Posts list.
- **Media and GoCDN: deletions no longer lose files.** A rejected or failed media delete keeps the
  attachment and its files; local files whose delete failed are retried later, and objects of detached
  attachments are removed from the bucket by the daily housekeeping. Linking or cleaning up remote media no
  longer takes over or deletes an object another attachment still uses, and a database error now keeps the
  object instead of deleting it.
- **Dates and feeds.** WordPress date formats accept signed and longer years, a site timezone written as an
  offset (`+05:45`) is honoured, and imported RSS/Atom items dated with US zone abbreviations (EST, PDT …) get
  their real time instead of UTC. Atom `xml:base` links and RSS `guid` permalinks resolve correctly.
- **Links, mail and blocks.** Plain `?p=` links of custom post types, comment feed links of a static front page
  and paged URLs keep their path and fragment. SparkPost and Mandrill sends count as delivered when any
  recipient was accepted, so a retry no longer sends duplicates; an OAuth token reply that is not JSON is
  rejected.
- **Translations.** Lookups match normalized strings, a plural form in a lower language-pack layer is found,
  a standalone `<locale>.mo` counts as installed, and a plugin language pack that cannot be extracted is
  reported.
- **Metrics and core updates.** `/wp-login` requests are grouped correctly, `Flush`, `Hijack` and `ReadFrom`
  keep their errors, and the slow-query limiter is safe under concurrency. A core update stages its backup and
  state with atomic renames and puts the previous backup back if saving the state fails.
- **Translations: JSON language packs pick the right plural form without the core language pack.**
  One-form languages (Japanese, Chinese and the others) no longer fall back to English for counts such as
  "%s seconds", and languages with three or more forms (Russian, Arabic, Polish and the others) no longer
  show the wrong form.
- **Metrics: a negative request duration no longer corrupts the duration histogram.** When the clock stepped
  backwards, the Prometheus duration sum wrapped into a multi-year value; such a duration now counts as zero.
- **Closing a request body while it is still being read no longer races on the idle-timeout flag.**
- **The access log no longer records credential query values.** OAuth codes, preview tokens, nonces,
  password-reset and API keys, `consumer_secret`, `password`, `token` and the same kind of parameter are
  written as `redacted`, even when the query string is malformed.
- **Pursae Security: the options API accepts a passkey requirement.** `POST /pursae/v1/options/security` (and
  the MCP `update_options` tool) only knew the `mfa` and `2fa` requirements, so on a site where a role must use
  a passkey (Two-Factor › Requirement per role) every security update was refused, whatever it changed.
  `passkey` is now accepted, as on the settings screen, while passkeys are allowed.
- **Commerce: cart, coupons, tax rounding, stock holds.** Logging in adds a guest line's quantity to the same
  line already in the account cart (sold individually stays at one). Recalculating an order no longer changes a
  fixed-cart coupon's total by a cent. With prices entered including tax and rounding at subtotal, the order
  lines add up to the charged total. A checkout of a different product no longer drops the stock hold of an
  order still awaiting payment, and taking that hold over cannot leave the units free in between. A payment
  blocked by another order's hold reports the units that are actually free. Coupon e-mail restrictions honour
  every `*` (`*@*.edu`), not only the first.
- **Backup Manager: remote retention, sealed secrets, restore paths.** A remote backup uploaded in the
  same second as another is no longer deleted in its place when their names sort the other way. A secret
  or passphrase that begins with `bkr:v1:` is stored sealed, and sealing fails closed if the system has
  no entropy. Endpoints that embed a metadata or loopback address in NAT64 or 6to4, and Azure's
  WireServer address, are refused even when `PURSAE_BACKUP_S3_ALLOW_PRIVATE=1` (that setting still
  allows loopback, private and carrier-grade NAT addresses). Restoring files no longer follows a symlink
  out of the destination directory, and a database row larger than 64 MB fails the restore instead of
  buffering without a limit.

## [0.5.3] - 2026-11-23

### Fixed

- **Pursae SEO: the front page and the posts page are analyzed as visitors see them.** A Pursae Multi
  Language translation of the front page (served at /tr/, for example) is now analyzed, titled and given
  breadcrumbs and schema as the front page, so it scores like the original; the posts page and its
  translations are scored on the blog listing a visitor sees there, not only their intro text; and the front
  page, whose address is the site root, no longer fails "Keyphrase in slug" (the check is left out of its score).
- **Pursae SEO: a keyphrase with accented letters is found in its slug.** "Keyphrase in slug" compared
  "özgeçmiş" with the slug `ozgecmis` letter by letter and failed; both are now compared without Latin
  accents, as WordPress drops them from slugs.

## [0.5.2] - 2026-11-23

The editor canvas is now the published page: an imported design runs its own scripts in the editor once you
trust it (Live mode), with Interact mode, edits without a reload, Basic mode blocks instead of HTML and Studio's
Edit code. Large WordPress sites and media over 64 MB move with WP to Pursae. Script trust given before this
release is asked once more.

### Added

- **Web Builder: the editor canvas is the published page (Live mode).** A design imported 1:1 as code
  sections now runs its own scripts in the editor, so three.js / WebGL scenes, theme toggles, sliders,
  counters and content a script draws look and work as on the site: the canvas is the page's own document
  at its own address, with the site's shell, header and footer, and each device button is that device's
  viewport (1440 × 900, 768 × 1024, 390 × 844) so `100vh` and media queries match. Live and Safe mode are
  two buttons in the bottom bar; Safe mode is the earlier static canvas. Details and what still differs:
  [code sections](docs/developer/web-builder-code-sections.md#editing).
- **Web Builder: trust is asked once per design, remembered per user, and revocable.** The first time a
  page of an untrusted design with scripts opens, the editor asks to run them with your admin access:
  **Always for this design**, **This session only** or **Not now (safe mode)**. "Always" is stored on the
  server in your own user meta, bound to that installation of the design kit and the hash of its `.js`,
  `.mjs`, `.wasm` and `.svg` files, together with the hashes of the page's inline code you approved; a
  button in the bottom bar stops it. "This session only" covers what the page ran when you answered. A
  kit deleted and uploaded again, a commit that changes the kit's scripts (for every user, the committer
  too; stylesheets, images and fonts keep the trust), and new or changed inline code or another kit on
  the page (an insert, a paste, Edit code, an undo) stop the scripts before the change reaches a frame
  that runs them and ask again. Only users with `unfiltered_html` can run or trust scripts.
- **Web Builder: Interact mode.** The **Interact** button, or holding Alt (⌥ on a Mac), sends clicks and
  keys to the design so a theme toggle, menu, tab or slider can be tried. What the interaction changes
  (`html.dark`, a stored theme) is kept when you go back to editing and is never written into the page.
- **Web Builder: edits without a reload.** In Live mode a text, style, class, attribute, move, duplicate,
  delete or insert is patched into the running page, so animations, the WebGL scene and script state keep
  running. The canvas reloads only when it must (a script changed, a section added or removed), and then
  keeps the scroll position and the theme state.
- **Web Builder: Basic mode shows blocks, not HTML.** A code section is a named block (Hero, Features,
  Pricing, FAQ …) with a small thumbnail (drawn without the design's scripts) and its headings, texts,
  button and link labels and addresses, images, form placeholders and repeated cards as fields. The
  HTML and Head code fields and the palette's HTML / Embed element are Studio only; background,
  decoration and motion settings, which have no effect on a code section, are not offered for one; on a
  mixed page a code section points to Studio. New elements seam patch `0024-basic-seam`.
- **Web Builder: live typing.** Text, label, alt and attribute fields of the Content tab and the Basic
  panel update the canvas while you type (debounced, patched into the running page in Live mode), as
  one history entry per typing session; Escape takes it back.
- **Web Builder: readable element names.** The navigator, canvas outline and Basic panel name elements by
  what they are and say (Menu, Slider, 3D / Canvas, Cards · 6, Hero, Form, Table, Map …) instead of
  "Code", taken from the tag, role, label, text, id or class.
- **Web Builder: Studio "Edit code".** The selected element's own markup (**Whole element**) or its
  **HTML inside** can be edited in a small code editor, checked for unclosed and mismatched tags, applied
  as one undo step, with the editor's ids preserved and unapplied drafts kept.
- **New Web Builder REST routes.** `PUT web-builder/v2/kits/{slug}/trust` stores the calling user's script
  trust of a design kit, and `POST web-builder/v2/documents/{id}/live-frame` renders the Live canvas frame.
  The trust route takes `{trust, code?}` (at most 512 SHA-256 hashes) and answers `{trust, scriptTrust,
  scriptCode}`. `Boot` gains `scriptTrust`, `scriptCode` and `canRunScripts` (additive). See the
  [contract](docs/developer/web-builder-contract.md#3-rest-namespace-web-builderv2).
- Test harnesses for the editor: `builder/test/editor-parity` (editor canvas against the published page,
  pixels and behaviour) and `builder/test/live` (the live frame), both run with Playwright; `PW_CHROMIUM`
  names the browser. See [Testing the editor](docs/developer/web-builder-code-sections.md#testing-the-editor).

### Changed

- **WP to Pursae: very large sites.** The WXR of a package (and of an uploaded WXR file) is read token by token,
  one item at a time, instead of being loaded whole, so the memory an import needs no longer grows with the
  size of the site. A first pass reads the authors and terms and checks the whole file is well formed (a damaged
  file still imports nothing); the second pass imports the items. Packages, resume, re-import and the id map
  behave as before. `PURSAE_WP2P_MAX_CONTENT_MB` and the "content.xml is more than this site imports in one
  go" error are gone.
- **WP to Pursae: media over 64 MB.** Media files of a package are no longer held to the 64 MB upload limit.
  They are streamed from the package to the uploads directory, up to `PURSAE_WP2P_MAX_MEDIA_MB` (default
  4096); the plugin packs files up to 4 GB (filter `wp2p_max_media_bytes`, never above 8 GiB) and gives a big
  file a request of its own. Only one request builds a package at a time, and the package checksum is made over
  several requests on PHP 8.1 and later, so a very large package no longer risks a killed request that repeats
  forever. Update Pursae before sending files over 64 MB: 0.5.0 and 0.5.1 skip them. The upload limit still applies to
  uploads. A WXR element larger than 256 MB (or a CDATA section never closed) fails the import cleanly.
- **Web Builder: "Live edit" is now Live mode, on by default for trusted designs.** The per-session
  "Run this design's scripts" confirmation is replaced by the per-design prompt above; mixed pages
  (code and builder sections) keep their Safe-mode frames and a Preview that runs scripts for a trusted
  design or after the per-session confirmation.
- **Web Builder: the editor's Content Security Policy allows only what its frames need.** A page of code
  sections only gets the style and font origins of its markup, head and kits and `blob:` images and
  media; a mixed page, whose Preview runs the design in frames of the editor's page, also gets the script,
  connect and frame origins the published page allows. The kits' script origins are no longer added to
  every editor page.
- **Pursae SEO: content analysis scores what visitors see.** Pages are rendered like the front end
  (theme template or Pursae Web Builder canvas) and the analysis reads their `<main>`, without navigation,
  sidebars, comments, the title heading or Pursae SEO's table of contents; posts read their rendered content
  (dynamic blocks expanded). A page whose text and images come from its template or a dynamic block no
  longer scores as nearly empty. The editor's live analysis renders the unsaved content the same way.

### Fixed

- **Pursae SEO: a post that passes every check now scores 100.** The *Single title* and *Competing links*
  checks gave 8 points for a pass instead of 9, so a fully green post stopped at 99/100. Both now give 9.
- **Login: the key icon on Log in with a passkey sits on the text line.** WordPress's button icon line height
  pushed it about 10 px below the label.
- **Login: the login page is never shown from the browser's cache.** It now sends `no-store` (as wp-login.php
  does), so going back to it or restoring a tab no longer shows a stale copy, such as the passkey button after
  Pursae Security was switched off.
- **WP to Pursae: killed `pursae import wordpress` runs.** A running import now touches its row every 30
  seconds, so a command that was killed (SIGKILL, a crash, a lost terminal) is recognised after 30 minutes
  instead of 6 hours (still 6 hours for an import started with 0.5.0, which does not heartbeat). Its import is
  closed as failed and the files it extracted (users and orders) are deleted, by the hourly cleanup or at once
  when `pursae import wordpress` is run again. A run whose row was closed meanwhile stops instead of going on,
  and cannot mark the row done afterwards. Failed command-line imports are closed after 7 days like the others.
- **Web Builder: Basic fields and block names keep the gaps between parts of a text.** A heading or label whose
  parts are laid out as blocks (`<br>`, block/flex spans, a flex link with an icon) no longer reads glued
  together ("ENERJİ OSGBSakarya"); the stored markup is unchanged.
- **Web Builder: no "unpublished changes restored" after an undo.** A draft equal to the stored page is not
  offered back when the editor opens.
- **Pursae Web Builder: removing a site import restores the right homepage.** Publishing an import a
  second time (after its report failed to save) recorded the imported page as the site's previous
  homepage, so *Remove this import* set the homepage to a page it had just deleted. Removal also left
  the homepage or posts page pointing at a deleted page when it had been moved to another page of the
  same import. The original homepage is now kept from the first change, and removal resets the
  reading settings whenever they point at a page it deletes.
- **Pursae SEO: the Table of Contents title follows the page language.** With Pursae Multi Language on, the
  default title (General Settings › Table of Contents) is a registered string under *Pursae SEO* on
  Languages › Translations, and each language shows its translation, so the automatic table of contents no
  longer reads "Table of Contents" on Turkish posts. A block's own title is shown as written.
- **Pursae SEO: the Table of Contents no longer touches scripts, styles or comments.** A heading-looking
  string inside a `<script>`, `<style>`, `<textarea>`, `<template>`, `<noscript>` or an HTML comment was
  listed and given an `id`, which broke the page's JavaScript or CSS; those are now skipped. The editor's
  Table of Contents block also reads heading text with an inert `DOMParser`, so markup in another
  author's heading (an `<img onerror>`) no longer runs in your editor.
- **Pursae Security: a required passkey/2FA or a forced password change now also closes the REST API.**
  Past the grace period these only kept the HTML admin on the fix-it screen; the same cookie still got a
  REST nonce from the heartbeat and full REST access, enough to mint an Application Password and skip the
  requirement for good. Cookie-authenticated REST now answers 403 `adminsec_account_action_required` and
  the heartbeat hands out no REST nonce until the account is fixed.

### Security

- **Web Builder: users without `unfiltered_html` get Safe mode only.** They cannot run a design's scripts
  or trust a kit (both routes answer 403), and the canvas's element operations, Basic fields, Edit code
  and Section code are read-only for them (the server already ignored such edits).
- **The Live frame is served only to its creator.** `?wb-live-frame=<token>` is answered at the page's own
  path only to the user who made it, only while they have `unfiltered_html`, only as a same-origin frame,
  for 15 minutes, from memory, with the front end's CSP and nonce, `no-store` and `noindex`. It is never
  given `allow-top-navigation`. Safe mode keeps the static, neutralised copy and the sandbox without
  `allow-scripts`.
- **The Live frame's address is the server's.** Its path is the post's permalink; a `url` in the request
  is ignored, and admin, REST, `wp-content` and `.php` paths are refused. Frames are kept in memory up to
  64 MB in all and 24 MB / 6 frames per user (413 for a larger page), and are served with
  `Referrer-Policy: no-referrer`. When the frame cannot be loaded the canvas stays in Safe mode with a
  notice: the design's scripts never run in a `srcdoc` frame under the editor's own policy.
- **Live text edits are not saved when the design changes the text meanwhile.** A script that rewrites
  the element being edited in place (a counter, a re-render) aborts the edit with a notice.
- **Existing script trust is asked again once.** Trust stored before code hashes (a bare kit
  installation id) is not trust any more.

## [0.5.1] - 2026-11-23

The first 0.5.x update: Cart and Checkout blocks, stock reservation, passkeys, remote backups, metrics, Web
Builder site style and drop-down menus, and completed translations. Releases continue as 0.5.2, 0.5.3…

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

### Fixed

- **Web Builder design import recovers from interruptions.** Retrying an import skips pages it already
  finished and keeps their later changes; scheduled results show as pending publication; the homepage and
  its rollback record are written together, and undo restores the previous homepage without deleting a
  reused page. Retries keep the original removal record and kit, kit files are restored when saving
  settings fails, and the wizard stops before publishing if an old staging folder cannot be cleared.
- **Interrupted media transfers in design imports.** A retry reuses the attachments it already created
  (same IDs and URLs), files of a rejected attachment are cleaned up, zero-byte multipart uploads are
  rejected like empty raw uploads, and the removal confirmation lists the remaining Media Library items.

### Database

- Migration 0112: `adminsec_passkey_users`, `adminsec_passkeys`, `adminsec_passkey_ceremonies`.
- Migration 0113: `goo_reserved_stock`.
- Both only add, so 0.5.0 still runs on the new schema.

### Internal

- Dependency updates (Dependabot), including TypeScript 7 and vitest 5 for the Web Builder.

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
