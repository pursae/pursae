# Pursae Headless: feature inventory

Pursae Headless is a native (compiled-in) Pursae plugin that turns the site into a **headless CMS**:
a Content-Type Builder, a Content Manager and a delivery API for front-end apps (Next.js, Nuxt,
Astro, mobile apps…). Its model and query language follow the conventions of the popular open-source
headless CMSs (collection types, single types, components, dynamic zones, `filters[field][$op]`,
`populate`, `documentId`), so front-end code written for them needs few changes. Like every native
plugin it is **off on a fresh install** (`native_headlessgo`); while it is off its routes answer 404,
its menu is gone and the front end renders as usual. Schemas, entries, tokens and webhooks stay in
the database when it is deactivated.

Legend: **✓** implemented · **≈** implemented differently (reason given) · **✗** left out (reason given).

## 1. Storage

- **Schemas** (types and components) are JSON documents in `hg_schemas` (migration
  `0020_headlessgo.sql`). The registry is cached per server and dropped on every schema write.
- **Entries** of every type live in one table, `hg_entries`: one row per document and locale, with
  `data` (the draft: the latest saved values) and `published_data` (the published snapshot, NULL
  while unpublished) as JSONB, plus typed columns `document_id`, `type_uid`, `locale`,
  `published_at`, `created_at`, `updated_at`, `created_by`, `updated_by`. GIN indexes on both JSONB
  columns serve the unique checks and `@>` lookups.
- **No DDL per content type.** Schema changes migrate the stored data instead (§3).
- Relations and media are stored as lists of ids whatever their cardinality (document ids of
  entries, numeric ids of posts, pages, users and attachments); components and dynamic zones as
  nested objects (dynamic zone items carry `__component`, component values an `id`).
- Settings: the option `headlessgo_settings` (headless mode, CORS, public permissions, preview
  secret, page sizes, GraphQL switch, webhook attempts). API tokens: `hg_api_tokens` (SHA-256 only,
  `can_upload`). Webhooks: `hg_webhooks`, the delivery queue `hg_webhook_queue`, the attempt log
  `hg_webhook_deliveries` (last 200 per webhook).
- Migration `0022_headlessgo_more.sql` adds the schedule columns of `hg_entries` (`publish_at`,
  `unpublish_at`, `schedule_error`), the content history `hg_entry_versions`, `hg_api_tokens.can_upload`
  and `hg_webhook_queue`.
- Migration `0024_headlessgo_review_lock.sql` adds the edit locks (`hg_entry_locks`), the review columns of
  `hg_entries` (`review_status`, `review_hash`, `review_by`, `review_note`, `review_at`) and the history note.
- Inverse relation fields (`mappedBy`) store nothing: their value is computed from the owning side.

## 2. Headless mode

| Feature | Status |
|---|---|
| Opt-in headless mode (the default keeps the site's front end): the front end does not render, JSON 404 `{"data":null,"error":{"status":404,"name":"NotFoundError",…}}` | ✓ |
| Setting: plain-text 404, or a 302 to the front-end app's URL (optionally keeping path and query), or keep the site's front end | ✓ |
| wp-admin, wp-login, `/wp-json` (core REST and `?rest_route=`), `/wp-content/` (uploads, assets), `/wp-includes/`, wp-cron, robots.txt and the favicon keep working | ✓ |
| Logged-in users who can edit posts still see the front end (setting, on by default) so previews and theme work go on | ✓ |
| Runs in `Server.Handler` before the page cache (`headlessServe`); answers are `no-store` and `noindex` | ✓ |

| Delivery API reads carry a strong `ETag` and answer `If-None-Match` with 304; token-less reads get `Cache-Control: public, max-age=0, s-maxage=N` when a public cache lifetime is set (Settings), others `no-cache`, previews `no-store`. Publicly cacheable answers carry `Surrogate-Key`/`Cache-Tag: <prefix>headless`; any entry, schema, settings or WordPress post change purges that tag in Pursae Cache and forwards the purge to the configured front proxy/CDN | ✓ |
| Rate limit (Settings, requests per minute): per token, per client address without one; 429 with `Retry-After` | ✓ |
| GraphQL: at most 20 root fields per operation (aliases count), plus the node and depth limits; optional "disable introspection for callers without a token". Introspection (`__schema`, `__type`) and the "Did you mean" hints of validation errors show a caller only the types it may use and the components and relation targets they reach (as its OpenAPI document does); operations are still validated against the whole model, so a field the caller may not use answers "Forbidden access" | ✓ |
| Populated relations are loaded per request with a shared cache and a list-level prefetch (one query per relation field, not per entry); WordPress posts and users are read once per id | ✓ |
| Admin entry actions follow WordPress capabilities: publish/unpublish/schedule, and the form's Publish button (also on a reviewed type), need `publish_posts`, changing another user's entry `edit_others_posts`, deleting `delete_posts` (`delete_others_posts` for others'); administrators are unrestricted. A form save may only change an inverse relation's owning entries the user created (unless they can `edit_others_posts`), and may only link media the user could read through `/wp/v2/media` | ✓ |

## 3. Content-Type Builder

| Feature | Status |
|---|---|
| Collection types (many entries), single types (one entry), components (category.name), dynamic zones | ✓ |
| Field types: text (short/long), rich text (blocks or Markdown), number (integer, big integer, decimal, float), boolean, date, date-time, time, email, password, enumeration, media (single/multiple, allowed types), relation, JSON, UID (derived from a text field), component (single/repeatable), dynamic zone | ✓ |
| Relations: one-to-one, one-to-many, many-to-one, many-to-many, to content types (self included) and to WordPress posts, pages and users, to the posts of any other public post type (`wp:type:<post type>`, e.g. products) and to the terms of a public taxonomy (`wp:term:<taxonomy>`, e.g. categories, tags, product categories); the target must be registered when the schema is saved, populate returns the term (`id`, `name`, `slug`, `description`, `count`, `parent`, `taxonomy`, `link`), and the term relations are filterable (`id`, `slug`, `name`), sortable and in the GraphQL schema as `WpTerm` | ✓ |
| Bidirectional relations: an owning field with `inversedBy` gets an inverse field (`mappedBy`) on the target type with the mirrored kind (one-to-one ↔ one-to-one, one-to-many ↔ many-to-one, many-to-many ↔ many-to-many); self relations too. The builder offers "Inverse field (optional)" on relations between content types. Both sides stay in step on every save: renaming either side renames the link, removing the inverse field clears `inversedBy`, removing (or retargeting) the owning field removes the inverse field, changing the kind changes the mirror. Export / import keeps them | ✓ |
| The inverse side stores nothing: its value is the owning entries pointing at the document (in the matching locale and status), so it can never disagree. It is populated, filterable and sortable like any relation, shown and editable in the Content Manager, and writable through the API (id, list, `{connect, disconnect, set}`, `null` clears); a write changes the owning entries' draft and published version (a link belongs to both documents; for an owning type with **Review before publishing** only the draft). An API caller needs `update` on the owning type too (otherwise 400 naming the field), and every owning entry that changed is announced (`entry.update`, plus `entry.publish` when its published version changed) to webhooks and subscriptions — also the entries that lose their link to a deleted document. Cardinality is enforced (a single-valued inverse side takes one entry) | ✓ |
| Deleting a document (its last locale) removes it from every relation pointing at it: owning fields of every type, also inside components and dynamic zones, drafts and published versions | ✓ |
| One-to-one / one-to-many exclusivity (a target belongs to one entry) | ✓ |
| Options: required, unique, min/max (length, value or item count), default, private (never in the API), regex (text, UID, email) | ✓ |
| Type options: draft & publish, review before publishing, internationalization, preview URL template, description | ✓ |
| Builder page: type list, settings, field table with edit / delete / reorder, add-field panel with a type picker and per-type options | ✓ |
| Validation: kebab-case API ids, unique singular/plural ids, reserved names (`id`, `documentId`, `locale`, …), valid targets, no component cycles, no dynamic zone in components | ✓ |
| Schema changes migrate data: renamed fields (the builder sends `renamedFrom`) move their values; removed fields are dropped — in the type's entries, or inside every component value when a component changes (also nested and in dynamic zones); i18n on/off converts locales; draft & publish off publishes every draft | ✓ |
| Delete: a type (with its entries) or a component; refused while another schema uses or relates to it | ✓ |
| Export and import entries: Settings › "Export and import entries", or `pursae headless export [--type uid]… > entries.ndjson` / `pursae headless import [--overwrite] < entries.ndjson`. NDJSON, one entry version per line (`type`, `documentId`, `locale`, draft `data`, `publishedData`, dates, and the URLs of referenced media in `media`). Relations to entries are document ids and survive a move between sites; relations to posts, pages and users are numeric ids. Import needs the schemas first, creates missing entries, skips (default) or overwrites existing ones, drops unknown attributes and media ids that do not exist on the target (warnings), and keeps drafts as drafts; it writes directly, so it fires no webhooks or hooks | ✓ |
| Export all schemas as JSON; import (file or paste): components first in dependency order, then types in two passes so they may relate to each other | ✓ |
| Changing a field (type, number format, rich-text format, single ↔ multiple media or relation, relation target, enumeration values, component, repeatable, dynamic zone components) converts the stored values of drafts and published versions, also inside components: text ↔ number ↔ boolean (`yes/no`, `on/off`, `1/0`), dates ↔ date-times, text → UID (slugified), text → e-mail / enumeration (when valid, case-insensitive), blocks ↔ Markdown/text (plain text), anything → JSON, decimals → integers (rounded), multiple → single (first kept), repeatable → single component (first item). Values that cannot be converted are removed | ✓ |
| The builder lets an existing field change type, renames enumeration values changed on their line (`enumRenames`), shows a warning under the field when the change loses data and asks for confirmation on Save with the list of lossy changes | ✓ |

## 4. Content Manager

| Feature | Status |
|---|---|
| Home with a card per type (entry counts, API route); side navigation | ✓ |
| Entries list: search (text fields and document id), status filter (draft / published / modified), locale filter, sortable columns, pagination | ✓ |
| Edit form generated from the schema for every field type: visual editor for rich text with a classic-editor toolbar (paragraph, headings, quote, code block, bulleted / numbered / nested lists, image from the media library; bold, italic, underline, strikethrough, inline code, links; pasted HTML is cleaned into blocks; block types it does not know are kept as they are) and a Markdown toolbar for Markdown rich text, each field a row with its label beside it, media picker (WordPress media library), relation picker with search (WordPress posts and pages as the Posts screen shows them to the user: without `edit_others_posts` only published ones and their own; users by display name, with the login name only for users who may list users), nested components and dynamic zones with add / remove / reorder / collapse, JSON editor with validation, UID regenerate | ✓ |
| Validation per field options; a rejected save shows the errors next to the fields and keeps the input (passwords are never echoed back) | ✓ |
| Draft & publish: Save draft, Publish (validates required fields), Unpublish, Discard changes (back to the published version), status badges | ✓ |
| i18n: with Pursae Multi Language active its languages are the locales; each document has one version per locale, linked by `documentId`; the edit screen lists the locales, opens a translation empty or filled from the current locale. With Pursae Multi Language off there is one default locale (setting) | ✓ |
| Delete one locale or every locale | ✓ |
| Bulk actions in the list: publish, unpublish, delete the selected documents in the listed locale, or "in every locale"; failures (a required field empty) are reported per document | ✓ |
| Content history: every save, publish, unpublish, discard, restore, review step and scheduled (un)publication records the draft (last 50 per document version); the edit screen lists the latest, the History screen shows a version next to the current draft with changed fields marked, and restores it as the draft. Inverse relation fields are not part of a version; a change of a two-way relation is recorded on **both** sides as a "Relations changed" version with a note: on the entry whose data changed and on every entry whose inverse field now lists something else ("Linked by Book "Dune" via books"). That includes the entry that loses a link when another one takes it through the inverse field ("Unlinked by Book "Dune" via books: it now belongs to Writer "Bob""), the entries a deleted owner pointed at ("…: it was deleted"), and the entries that lose their link to a deleted document, also inside components ("Unlinked Writer "Bob" via writer: it was deleted") | ✓ |
| Links across a two-way relation: saving an entry through its owning relation field adds a "Relations changed" version, with a note (`Linked by Book "Dune" via books`), to every entry that was linked or unlinked (their inverse field now lists something else); writing the inverse side adds one, with the new draft and a note (`Linked Writer "Ada" via writer`), to every owning entry whose data changed. Works from the admin, REST and GraphQL alike | ✓ |
| Restoring a version checks its values against the **current** schema first (types and formats, required, min / max, regex, enumerations, media and relation targets that still exist, unique fields). If something no longer fits, nothing is restored and the mismatches are listed; the History screen shows them before you click, and offers "Restore the fields that still fit": the mismatching fields keep their current values and the message names them. Values of fields the type no longer has (removed or renamed) are left out; the History screen lists them before you click and the restore message names them. Password hashes are carried over untouched | ✓ |
| Scheduled publishing: "Publish on" / "Unpublish on" per document version (site time zone) in the edit screen, `POST …/{documentId}/schedule` in the API. The `headlessgo_tick` job of the site's job runner (every minute, rows claimed with `FOR UPDATE SKIP LOCKED`, safe across restarts and instances) publishes the draft as it is then, or unpublishes; a publication that fails validation is cleared and its reason shown on the edit screen; the list shows pending schedules. Publishing by hand (any route) clears a pending scheduled publication, unpublishing by hand clears a pending scheduled unpublication, and a due unpublication of an entry that is already a draft does nothing | ✓ |
| Edit lock (WordPress post-lock style): opening an existing entry takes its lock (one per document version); the page's script renews it every 15 s (`POST /wp-admin/headlessgo/lock`, nonce-checked) and releases it when the page closes; a lock nobody renewed for 90 s is free. While someone else holds it the entry opens read-only with "X is currently editing this entry" and a **Take over editing** button; the server refuses every change from another user (save, publish, unpublish, discard, schedule, restore, delete), and a taken-over editor's open page is told by its next heartbeat and switches to read-only. A form also carries the version it was loaded from: a save on top of a newer one is refused ("changed by someone else"), also without a lock. The REST and GraphQL APIs are not locked (tokens have no user) | ✓ |
| Review flow: the type option **Review before publishing** (needs draft & publish). A draft is *submitted for review*; a user who can edit others' posts (editor and above) *approves* or *rejects* it (optional note). Publishing needs an approved draft, whatever the route: the form's Publish button (it saves the draft and publishes when that draft is approved, otherwise it says the draft was saved but not published), the REST `publish` route and `?status=published` writes, GraphQL `publish…` / `status: PUBLISHED`, bulk publish, and scheduled publication (the schedule is cleared with the reason in the edit screen). Approval is bound to the exact draft: any later change (form, API, restore, relation link) makes it "not reviewed" again. Submit, approve and reject appear in the entry history with the reviewer's note. The APIs have no user, so they can neither submit nor approve; a type without the option is unchanged | ✓ |

## 5. Delivery REST API — `/wp-json/headlessgo/v1`

| Feature | Status |
|---|---|
| `GET/POST /{pluralApiId}`, `GET/PUT/PATCH/DELETE /{pluralApiId}/{documentId}`, single types `GET/PUT/DELETE /{singularApiId}` | ✓ |
| A write with `?status=published` publishes, so it needs `update` as well as `create`: `create` alone may only store a draft (otherwise 403; 0.6.3+) | ✓ |
| `POST …/{documentId}/publish` and `/unpublish` (needs `update`); `publish` is refused (400) for a type with the review flow until the draft is approved in the admin. Every publish locks the entry while it checks the approval and validates, so a save landing meanwhile is not put live | ✓ (extension) |
| `POST …/{documentId}/schedule` with `{"data": {"publishAt", "unpublishAt"}}` (RFC 3339, `null` clears; needs `update`) | ✓ (extension) |
| Media upload: `POST /upload` (multipart `files`, one or several; optional `fileInfo` JSON `{name, alternativeText, caption}` or a list) through the core media upload code (same file checks, image sizes, Pursae CDN); answer: the files in the API's media format. With `ref` (type uid or API id), `refId` (document id), `field` and optional `locale` the files are attached to the entry's draft (appended to a multiple field, replacing a single one; allowed types checked before anything is kept). `GET /upload/files/{id}` | ✓ |
| Response `{data, meta}`; entries carry `id`, `documentId`, attributes, `createdAt`, `updatedAt`, `publishedAt`, `locale`; errors `{data:null, error:{status, name, message, details}}` with `ValidationError` details `{errors:[{path, message}]}` | ✓ |
| `filters[field][$eq|$eqi|$ne|$nei|$lt|$lte|$gt|$gte|$in|$notIn|$contains|$notContains|$containsi|$notContainsi|$startsWith(i)|$endsWith(i)|$null|$notNull|$between]`, `$and`, `$or`, `$not`, implicit `$eq` | ✓ |
| Filtering through relations (`filters[author][name][$eq]`, nested), components (single and repeatable), media (`id`, `name`, `mime`), WordPress pages/posts (`id`, `slug`, `title`, `status`; only published posts/pages of the target type match) and users (`id`, `slug`, `name`; the login name is not filterable); `$null` on relations, media, components. A relation to a type the caller may not read (find/findOne) is an invalid key (400); a draft read sees related drafts only with a token allowed to read drafts, otherwise their published version. Media filters match, for callers that may not upload, only files core `/wp/v2/media` shows them (unattached, or of a published post without a password). Filter objects, and separately the operators of one attribute (`$not`, `$and`, `$or`), nest at most 12 levels (deeper: 400 "Filters are nested too deeply") | ✓ |
| `_q=text`: full-text search, a case-insensitive "contains" over every public text, UID, e-mail, enumeration and Markdown attribute plus an exact `documentId` match; private attributes are never searched; combines (AND) with `filters` | ✓ |
| `GET /openapi.json` (OpenAPI 3.0) and `GET /typescript` (a `.d.ts` with an interface per type, component and write input) generated from the content model; they describe only the types and actions the caller may use (public role, token or signed-in user). The admin offers "Download TypeScript types" under Settings | ✓ |
| `sort=a:asc,b:desc`, `sort[0]=…`, `sort[a]=desc` on scalar attributes and system fields | ✓ |
| Sorting through relations and single components: `sort=author.name:asc`, `author.company.name`, `seo.metaTitle`, `page.title` (WordPress posts/pages: `id`, `slug`, `title`, published ones only; users: `id`, `slug`, `name`), also through single-valued inverse relations. The same permission rules as for filtering apply. To-many relations and repeatable components are refused (400). Entries without the related value sort last | ✓ |
| Filtering through inverse relations (`filters[books][title][$eq]`, `filters[books][$null]=true`) | ✓ |
| `pagination[page]&pagination[pageSize]` or `pagination[start]&pagination[limit]` (mixing is a 400), `pagination[withCount]=false`; default/max page size are settings (25/100) | ✓ |
| `populate=*` (one level), `populate=a,b`, `populate=a.b`, `populate[a][populate]…`, `[fields]`, `[filters]`, `[sort]`, dynamic zones `populate[zone][on][comp.uid]…`, `populate=localizations` | ✓ |
| `fields[0]=title` / `fields=title,slug` | ✓ |
| `locale=` (default: the default locale), `status=draft|published` (drafts need a token allowed to read them) | ✓ |
| Writes: `{"data": {...}}`; `?status=published` publishes, `?locale=` writes that locale's version (a new translation of an existing document); partial updates; relations as id, list or `{connect, disconnect, set}`; media as id or list. Callers that may not upload (front-end users without `upload_files`, read-only tokens) can link only files core `/wp/v2/media` shows them; populated media and WordPress posts/pages show what core REST shows the caller (files of drafts or password-protected posts are left out, the excerpt of a password-protected post is empty unless the user can edit it, Memberships restrictions apply, excerpts are KSES-sanitized) | ✓ |
| Password fields: hashed (argon2id), never returned; an empty value keeps the hash | ✓ |
| Private fields: never returned, not filterable or sortable, but writable | ✓ |
| WordPress content: posts, pages and media are in the core REST API (`/wp-json/wp/v2/posts`, `/pages`, `/media`) and stay reachable in headless mode; Pursae Headless adds the missing menus: `GET /wp/menus` (menus with their theme `locations`, plus the theme's locations) and `GET /wp/menus/{id|location|slug}` (published items as a tree with resolved URLs; items pointing at a draft, private, scheduled or trashed target are left out, 0.6.3+) | ✓ |
| `GET /` lists the routes the caller may read | ✓ |
| `?rest_route=/headlessgo/v1/…` | ✓ |

## 6. Permissions and API tokens

| Feature | Status |
|---|---|
| Public role (no token): a matrix of type × find / findOne / create / update / delete under Settings. **Default: nothing is public.** Public callers never see drafts | ✓ |
| API tokens: read-only (find/findOne on every type), full access (everything, drafts included), custom (the same matrix per token, plus "may read drafts") | ✓ |
| Duration 7 / 30 / 90 days or unlimited; expired tokens get 401; last-used time (recorded at most once a minute); regenerate; delete | ✓ |
| Upload permission: full-access tokens always, custom tokens with "May upload media", read-only tokens and the public role never. Uploading does not open the media library: a custom upload token reads public files and the files it uploaded itself (0.6.3+) | ✓ |
| Only a SHA-256 of the token is stored; it is shown once (on the response page, never in a URL) with its last four characters in the list | ✓ |
| `Authorization: Bearer <token>`; a bad token is 401 `UnauthorizedError`, a missing permission 403 `ForbiddenError` | ✓ |
| Populated relations to a type the caller may not read are left out (not an error). Related entries (and other locales) of a draft response — a draft read or the answer to a write — are shown as drafts only to tokens allowed to read drafts; other callers get their published versions. The answer to an update (REST or GraphQL) or an unpublish that returns an existing document's draft shows a caller that may not read drafts only the attributes it wrote and the entry's metadata | ✓ |
| Signed-in users (off by default): Settings › "Signed-in users". `POST /auth/local {"identifier","password"}` (username or e-mail) returns `{jwt, expiresIn, user}`, an HS256 JWT signed with the site secret (`PURSAE_SECRET`, else the generated `auth_secret`) and valid for the configured lifetime (default 60 minutes) that is sent as a Bearer token; optional `POST /auth/local/register {"username","email","password"}` (default role, password 8+ characters) returns the same, and the site's password policy (Pursae Security) applies to it (0.6.3+); it is its own opt-in, independent of the site's "Anyone can register" setting, allows 20 sign-ups per network per 10 minutes, and asks for `captchaToken` (403 `captcha_required`) when Pursae Security's account-form CAPTCHA is on. No confirmation e-mail is sent. `GET /users/me` says who the token belongs to. A user may do what the public role and the **Authenticated** matrix allow; **own entries only** restricts `update` and `delete` (and the inverse-relation edits they cause) to entries the user created (`created_by` is set from the token). The login uses the same throttle as wp-login (and Pursae Security's lockouts); when CAPTCHA keys are set and the login forms ask for a CAPTCHA, or the account is under attack, it answers 403 with `error.details.code` `captcha_required` (plus `provider` and `siteKey`) until the body carries a solved token as `captchaToken`; accounts with a second factor are refused; changing the account's password, or switching the feature off, invalidates its tokens. The account's WordPress role gives nothing here, and users cannot read drafts, upload, or create tokens. Works for REST, GraphQL and subscriptions | ✓ |
| Cookie / Application Password auth on this namespace | ✗ tokens and user tokens only (the admin uses its own screens) |

## 7. CORS

- Allow-list of origins (one per line, `*` for any) under Settings. Allowed origins get
  `Access-Control-Allow-Origin` (the origin itself) and `Vary: Origin`; preflight `OPTIONS` requests
  are answered with 204, the methods and the `Authorization, Content-Type, Accept, X-Preview-Token`
  headers, and a one-day `Max-Age`. Other origins get no CORS headers. Applies to the Pursae Headless
  namespace only. ✓

## 8. Webhooks

| Feature | Status |
|---|---|
| Events `entry.create`, `entry.update`, `entry.publish`, `entry.unpublish`, `entry.delete`, and `schema.update` (setting) | ✓ |
| Per webhook: name, URL, events, custom headers (reserved ones refused), signing secret, enabled. The signing secret and header values are stored encrypted with the site secret (0.6.3+; existing ones are encrypted at start-up) | ✓ |
| Deliveries go to public addresses only: the address is checked when the connection is made (after DNS resolution), so loopback, private, link-local (cloud metadata) and carrier-grade NAT addresses fail with an error; redirects are not followed | ✓ |
| Payload `{event, createdAt, model, uid, entry}` (public scalar attributes; private fields never sent) | ✓ |
| `X-HeadlessGo-Signature: sha256=<hex HMAC-SHA256 of the body>`, `X-HeadlessGo-Event`, `X-HeadlessGo-Delivery` | ✓ |
| Durable delivery queue (`hg_webhook_queue`): the first attempt starts at once in the background; a network error or a non-2xx answer is retried with an exponential backoff (10 s, 40 s, 160 s, 640 s…, at most 6 h) up to the "Attempts per delivery" setting (5 by default, 1–10), then the delivery is failed. Short backoffs are retried in-process; the `headlessgo_tick` job delivers whatever is due (8 deliveries at a time, starting none after 30 s so the site's other jobs are not held up; what is left waits for the next tick), so retries survive restarts (rows claimed with `FOR UPDATE SKIP LOCKED`; rows left running by a crashed instance return after 5 minutes). Finished rows are removed after a day | ✓ |
| The webhook screen lists waiting and failed deliveries (attempts, next attempt, last error) with "Retry now" | ✓ |
| Delivery log per webhook: date, event, attempt, status code, duration, the first 1 KB of the response or the error | ✓ |
| "Send test" (`trigger-test`, synchronous, result shown as a notice) | ✓ |

## 9. Preview

- Per type a preview URL template with `{documentId}`, `{locale}`, `{status}`, `{secret}`,
  `{previewToken}`, `{uid}`, `{pluralApiId}`, `{singularApiId}` and any scalar field (`{slug}`); values
  are URL-encoded. The edit screen shows **Open preview**. ✓
- The preview secret (Settings, regenerable) is the `{secret}` front ends compare; it also keys the
  preview tokens. ✓
- `{previewToken}`: a signed, short-lived token (lifetime setting, 60 min by default) that reads one
  document's draft in the entry's locale: `findOne` of that document, or `find` restricted to it, with
  `status=draft`; another `locale=` is 403. Related entries, `localizations` and relation filters see
  only what the public role may read, in their published version — other entries of the token's own type
  (through a self-relation) included. Sent as `Authorization: Bearer`,
  `X-Preview-Token` or `?previewToken=`. Rotating the secret revokes every preview token. ✓

## 10. GraphQL

- `POST|GET /wp-json/headlessgo/v1/graphql` with `{query, variables, operationName}`. Root query fields:
  `articles` (camelCase plural id), `article(documentId:)`, single types (`homepage`), `articles_connection {
  nodes pageInfo { total page pageSize pageCount } }`. Arguments `filters` (operators without `$`), `sort`,
  `pagination`, `locale`, `status`. Selections drive populate; aliases and `__typename` work. A relation,
  component, dynamic zone or media field under an alias is loaded on its own, so
  `recent: posts(sort: …) { … } all: posts { … }` returns two lists; the same response key selected
  twice (directly and through a fragment) is merged, and with differing arguments it is an error. ✓
- Literals follow the spec: string escapes `\" \\ \/ \b \f \n \r \t \uXXXX` (surrogate pairs combined,
  also `\u{…}`), anything else is a syntax error; `"""block strings"""` with the common indentation and
  blank first and last lines removed; an integer literal beyond 2^53 keeps its digits (for `biginteger`
  fields). ✓
- Mutations (POST only), with the REST permissions: `createArticle(data:, locale:, status:)`,
  `updateArticle(documentId:, data:, locale:, status:)`, `deleteArticle(documentId:, locale:)` →
  `{ documentId }`, `publishArticle` / `unpublishArticle(documentId:, locale:)`; single types
  `updateHomepage`, `deleteHomepage`, `publishHomepage`, `unpublishHomepage` (no documentId). Relations
  and media take ids, components and dynamic zones JSON. Validation errors come back as GraphQL errors. ✓
- WordPress content, read-only and always on while GraphQL is: `wpPosts`, `wpPosts_connection`, `wpPost(id:|slug:)`, `wpPages`, `wpPages_connection`, `wpPage(…)` (arguments `search`, `slug`, `category` and `tag` slugs, `author`, `orderBy` date/title/modified, `order`, `page`, `pageSize`; fields `title`, `slug`, `excerpt`, rendered `content`, `link`, dates, `featuredImage`, `author`, `categories`, `tags`), `wpCategories`, `wpTags`, `wpMediaItems`, `wpMediaItem(id:)`, `wpMenus`, `wpMenu(id:|slug:|location:)` (items as a tree) and `wpUsers` / `wpUser(id:)` (authors of published content). They return what a visitor can see: published, not password-protected content, public media, published menu items. ✓
- Named fragments (`fragment Card on Article {…}`, nested, cycles refused), inline fragments with or
  without a type condition (`... on ComponentBlocksQuote`), `@include(if:)` / `@skip(if:)`, variables
  with default values, several operations selected by `operationName`. ✓
- Introspection: `__schema` (types, query and mutation types, the two directives) and `__type(name:)`
  over a schema generated from the content model (object types per type and component, a union per
  dynamic zone, `<Type>FiltersInput`, `<Type>Input`, scalar filter inputs, `PaginationArg`,
  `PublicationStatus`, `UploadFile`, `WpPost`, `WpUser`, scalars `JSON`, `Long`, `Date`, `DateTime`,
  `Time`), enough for GraphiQL-type clients. ✓
- Admin playground (Pursae Headless › GraphQL): a query and variables editor that runs as the logged-in
  administrator (`POST /wp-admin/headlessgo/graphql`, nonce-checked) or, with a pasted API token, against
  the public endpoint with that token's permissions; plus a reference of the root fields. ✓
- **Validation**: a document is checked against the generated schema before anything runs, following the
  GraphQL specification's validation rules: fields a type does not have (also inside fragments, mutation
  results and parts that `@skip` / `@include` leave out), an object field without a selection, a scalar with
  one (JSON scalars may have one), unknown arguments on any field (root fields and relation fields) and
  missing required ones, unknown fields inside input objects (`filters`, `data`, `pagination`; literals and
  variable values alike), enum literals that do not exist, variables the operation does not declare, unknown
  fragments, fragment cycles, type conditions that are unknown or can never apply ("Fragment "W" cannot be
  spread here…"), unknown directives and directive arguments. Every problem found is listed, with its
  location, and nothing runs (a mutation in the same request does not either): HTTP 400, no `data`,
  `{"errors":[{"message":"Cannot query field \"titel\" on type \"Article\". Did you mean \"title\"?",
  "locations":[{"line":3,"column":5}],"extensions":{"code":"GRAPHQL_VALIDATION_FAILED"}}]}`. A document that
  does not parse answers the same way with the code `GRAPHQL_PARSE_FAILED` and the location where parsing
  stopped. Scalar values are left to the resolvers, which accept the REST spellings (`status: published`, a
  relation as an id, a list or `{connect, disconnect, set}`). The filter input types follow the REST filters:
  `id`, `null` / `notNull` on relations, media and components, `UploadFileFiltersInput`, `WpPostFiltersInput`,
  `WpUserFiltersInput`, `DynamicZoneFiltersInput`.
- **Subscriptions** on the same endpoint, over two transports with the same root fields and rules:
  - **WebSocket**, the `graphql-transport-ws` protocol (the `graphql-ws` library, Apollo Client, urql,
    GraphiQL): `wss://site/wp-json/headlessgo/v1/graphql` with that sub-protocol. Send the API token in the
    `connection_init` payload (`{"Authorization": "Bearer …"}` or `{"token": "…"}`; browsers cannot set
    headers on a WebSocket) or as the upgrade request's `Authorization` header; without one the public
    permissions apply. One connection carries up to 50 operations at a time; queries and mutations work too
    (one `next`, then `complete`); `{"type":"complete"}` from the client stops a subscription. Validation and
    access errors answer the operation with an `error` message and the connection stays open; protocol
    violations close it with the protocol's codes (4400 bad message, 4401 subscribe before
    `connection_ack`, 4403 bad token, 4408 no `connection_init` within 10 s, 4409 duplicate id, 4429 a second
    `connection_init`). The server pings every 20 s. A browser's `Origin` must be the site itself or one of
    the CORS origins (Settings), otherwise the upgrade is refused (403).
  - **Server-sent events** (the "distinct connections" mode of the graphql-sse protocol): send the
    subscription as a `GET` (`?query=…`, works with `EventSource`) or a `POST` (JSON body, works with `fetch`)
    with the usual `Authorization: Bearer` header and read the stream (`Content-Type: text/event-stream`):
    `event: next` / `data: {"data":{"articleCreated":{…}}}`, `: ping` comments every 20 s, `event: complete`
    when the server ends the stream. Validation errors, 403 and a full server answer as plain JSON before
    the stream starts. Admission (both transports together): at most 500 open subscriptions, of which
    callers without a token share 400 (100 are kept for tokens); one client holds at most 20 (an address,
    without a token) or 100 (one API token); an address opens at most 10 WebSocket connections (429).
    Beyond a cap: 503 "Too many open subscriptions".

  Root fields per content type: `articleCreated`, `articleUpdated`, `articlePublished` and
  `articleUnpublished` (the last two only with draft & publish) return the entry, `articleDeleted` returns an
  `EntryReference { documentId locale }` (the entry is gone; `locale` is null for a type without
  translations); the introspected `Subscription` type lists them. Arguments `documentId` (collection types)
  and `locale`; one root field per subscription. Each event runs the selection against the entry, so
  aliases, populate, fragments and variables work as in a query. Access follows REST: the type's `findOne`
  (single types: `find`) permission; a token that may read drafts gets Created and Updated with the
  **draft** values, Published with the published ones, and every Unpublished and Deleted; every other caller
  gets Published, Unpublished, and Deleted for documents that had a published version (for types without
  draft & publish also Created and Updated, published values). Limits: events are fanned out in the process
  that handled the write (a multi-instance setup needs the webhooks instead); a stream or socket is ended
  after 30 minutes (WebSocket close code 1001; clients reconnect) or when GraphQL is switched off; a
  subscriber that falls 256 events behind gets an error and its subscription ends. Behind a proxy, WebSocket
  needs the `Upgrade` headers passed on (see the reverse-proxy guide). The admin playground cannot show a
  stream and says so.
- Not supported: custom directives; component inputs are JSON. The endpoint can be switched off under Settings.

## 11. Translations

- The server-rendered screens go through the admin's i18n layer (`internal/i18n`, after rendering); the
  builder's field-type, relation and target labels are translated before they are sent as JSON; the
  strings the editor and builder scripts build in the browser are translated by `wpgo-l10n.js`
  (`admin/assets/headlessgo/*.js` is one of its sources). User content is marked `translate="no"`.
- Turkish (`web/languages/wpgo-tr_TR.json`) covers Pursae Headless's admin screens. Other languages: the
  strings shared with WordPress core come from its language packs; Pursae Headless's own strings stay English.

## 12. Not built / limits

- Relation ordering beyond the stored order (the owning side's order; the inverse side lists owners by
  creation).
- History does not record link changes made by a schema conversion (changing a relation's target or
  cardinality rewrites the stored ids of every entry without a version per entry).
- Edit locks and the review flow belong to the admin: the REST and GraphQL APIs are not locked and
  cannot submit, approve or reject (they are refused when publishing an unapproved draft). The review
  capability is "may edit others' posts" and is not configurable per type; a reviewer may approve a draft
  they submitted themselves.
- Signed-in users: no refresh tokens (sign in again), no per-user read rules (`find` of "own entries only"), no
  e-mail confirmation or password reset through the API, and users cannot read their own drafts (create with
  `?status=published` or let an editor publish). A GraphQL subscription opened with a user token is not closed
  when the token expires. Entry import writes directly: no webhooks, plugin hooks or history, and it does not
  re-upload media (the `media` URLs are there for a migration script).
- GraphQL: custom directives; subscriptions only see the writes of the process they are connected to
  (see §10); component inputs are JSON; unused variables and fragments are not reported (the spec calls
  them errors, they are harmless here); scalar argument values are not type-checked during validation (the
  resolvers report them).
- Translations of Pursae Headless's own admin strings exist only in Turkish.
- Read budget: one REST read, one GraphQL operation (all its root fields together) or one subscription
  event may run at most 1000 queries (finds, populated relations, inverse lookups) and load at most 20000
  related entries; beyond that it answers 400 "The request is too expensive…". Populated relations are
  still loaded per parent entry (no batching across parents). A read stops when its client disconnects
  (writes and GraphQL mutations run to the end).
- Errors never show database or network messages (GraphQL answers "Internal server error" and logs the
  cause); a numeric filter value must be a decimal number (`id` an integer), `pagination[page]` at most
  10^9; a write with `?status=published` on a type with the review flow is a 400 with the reason.
- Large integers: JSON numbers arrive as 64-bit floats, so a `biginteger` value of 2^53 or more must be sent
  as a string (a JSON number that large is refused, 400); `integer` fields take at most 2^53 - 1.
- API updates of one entry do not overwrite each other: an update applies only to the version it read and is
  merged again over a newer one (409 after five tries). Unique checks are still a separate read, so two
  simultaneous creates with the same unique value can both pass.

## Managing Pursae Headless from AI tools and REST

Content types and entries can be managed with the site REST API (`/wp-json/pursae/v1/headless/types`, `.../entries/{uid}`),
so Application Passwords, OAuth tokens (settings scope) and the Pursae MCP tools `headless_*` work without the admin screens.
The delivery API keeps its own API tokens. See `docs/developer/rest-api/endpoints.md` and `operations/ai-mcp.md`.
