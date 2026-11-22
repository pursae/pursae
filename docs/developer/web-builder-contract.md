---
title: Web Builder integration contract
description: The contract between the Pursae Web Builder editor (React) and the Go server — boot config, REST, snapshots, runtime attributes, storage, AI proxy and sanitizer.
weight: 900
section: built-in-plugins
---

This page is the single source of truth for how the Pursae Web Builder editor (the React app from
`github.com/pursae/Builder`, built into `web/web-builder/`) talks to the Go server. Every work
package of the builder integration cites it. It has two code halves that must match the tables
below field for field:

- Go: `internal/webbuilder/contract.go` (package `webbuilder`, types and constants only)
- TypeScript: `builder/host/contract.ts` (the doc model is imported from the pinned upstream
  `@/builder/types`, the rest is declared there)

A change to the contract changes this page and both files in one commit. Incompatible payload
changes bump `APIVersion`; incompatible changes to the static markup or the `data-wb-*`
attributes bump `RendererVersion` (see the rules in §1).

The plugin keeps its identity: slug `golementor`, option `native_golementor`, visible name
"Pursae Web Builder", edit URL `/wp-admin/post?post={id}&action=web-builder`.

## 1. Versions and handshake

| Constant | Value | Meaning |
|----------|-------|---------|
| `APIVersion` / `API_VERSION` | `2` | REST and boot schema (2 = Theme Builder, §11) |
| `RendererVersion` / `RENDERER_VERSION` | `1` | static markup + runtime attributes |
| `APIHeader` | `X-WB-Api` | sent by the editor on every `web-builder/v2` request |
| `UpstreamTypesSHA` | `22ff4976a1416accd3b7370e176d7206bf390aee` | upstream commit the Go doc types were copied from |

Rules:

- `APIVersion` changes with any incompatible change to a payload of §2, §3 or §11 (a field
  removed, renamed or retyped, a new required field, a new value an older editor would misread).
  Version 2 added the Theme Builder: `template`, `site` and `dynamic` in the boot config, the
  template routes, `Item.dyn`, `PageSettings.layout` and the `loop` section kind. An editor bundle
  built for another version stops at boot ("does not match the server") or on its first request
  (409 below).
- `RendererVersion` changes only with an **incompatible** change to the static markup: one that
  makes a stored snapshot render wrong, or makes the runtime or the server misread it. Additive
  markup — new `data-wb-*` attributes, new cell types or section kinds that an older snapshot
  never contains, the Theme Builder placeholders of §11.5 — keeps the version, so existing
  snapshots stay current and need no Regenerate.
- Every editor request to `web-builder/v2` sends `X-WP-Nonce: <wp_rest nonce>` and
  `X-WB-Api: <API_VERSION>`.
- A missing or different `X-WB-Api` answers **409** `wb_api_mismatch` with
  `data: {status: 409, reload: true, apiVersion: <server>}`. The editor stops autosaving, keeps
  the unsaved doc in memory and offers "Reload" (the draft on the server stays).
- Exempt from `X-WB-Api`: `GET fonts/css2` (loaded as a stylesheet; the nonce is in
  `?_wpnonce=`) and `POST forms/…` (public).
- A publish whose `render.renderer` is not the server's `RendererVersion` answers **422**
  `wb_invalid_doc` with an error of code `bad_renderer` (a stale editor bundle).

## 2. Editor boot config: `window.__WEB_BUILDER__`

The editor page (`wbEditor`) writes one `<script>window.__WEB_BUILDER__ = {…}</script>` before the
bundle. The JSON is encoded with `"</"` escaped as `"<\/"`. **No AI key, sealed value or secret is
ever part of it.**

| Field | Type | Notes |
|-------|------|-------|
| `apiVersion` | number | `APIVersion` |
| `rendererVersion` | number | `RendererVersion` |
| `document` | DocumentResponse | same shape as `GET documents/{id}` (§3.1) |
| `user` | `{id, name}` | the current user |
| `rest.root` | string | absolute, ends with `/wp-json/` |
| `rest.namespace` | string | `web-builder/v2` |
| `rest.nonce` | string | `wp_rest` nonce |
| `links.site` | string | home URL, no trailing slash |
| `links.permalink` | string | the post's permalink |
| `links.preview` | string | preview URL (`?preview=true`) |
| `links.exit` | string | logo / Exit target: the post type's list screen (templates: Web Builder › Theme Builder) |
| `links.editPost` | string | the core editor for this post |
| `links.settings` | string | Web Builder › Settings; `""` without `manage_options` |
| `caps.publish` | bool | `publish_posts`/`publish_pages` for the type; false = publish becomes pending (templates: `edit_theme_options`, always true there) |
| `caps.uploadFiles` | bool | `upload_files` |
| `caps.unfilteredHtml` | bool | `unfiltered_html`: html sections and custom CSS are kept |
| `caps.editOthers` | bool | `edit_others_posts`: may delete others' library items |
| `caps.editThemeOptions` | bool | `edit_theme_options`: the builder menu shows "Site settings" |
| `locale` | string | the user's WordPress locale (`en_US`, `tr_TR`; `userLocale`); its language part picks the editor UI catalog, see §2.1 |
| `fonts.cssUrl` | string | absolute `fonts/css2` URL with `_wpnonce` |
| `fonts.local` | string[] | families already localized under `uploads/web-builder/fonts/` |
| `media.maxUploadBytes` | number | upload size limit |
| `media.types` | string[] | accepted MIME types |
| `siteAssets` | string | absolute `/wp-content/web-builder/site/` |
| `ai` | BootAi | below; always present |
| `template` | TemplateBoot \| null | Theme Builder: `null` unless the post is a `wb_template` (§11.7) |
| `site` | SiteBoot | site identity, menus, post types and taxonomies (§11.7); always present |
| `dynamic` | DynContext | sample data for dynamic widgets (§11.6): the template's preview object, else the post itself |
| `kitBreakpoints` | `{[kit]: {tablet, mobile}}` | local-style breakpoints (max-width px) of every design kit the page's code sections and its `codeParts` name (`content.kit`); a section without a kit uses `{tablet: 1023, mobile: 639}`. The element editor's Style panel prints the frame's local styles with them |
| `codeParts` | CodeParts \| null | the header and footer templates Hello Pursae draws around this page, when they are made of code sections: `{header, footer}`, each `null` or `{id, title, editUrl, sections}` (`sections`: the template's published visible sections, every one `kind: "code"`, as stored; without `edit_theme_options` their `content.binds` is left out, since a bound form's recipients and redirects are there). The element editor draws them locked in the page's frame. `null` for a template, a canvas page, a page without code sections, or another theme |
| `siteStyle` | SiteSettings \| null | the site style: `{brand, fontPair, corners, headingFont?, headingWeight?, bodyFont?, bodyWeight?}`, `null` while none was saved. A doc whose `site.global` is `true` follows it (§3.x *Site style*) |

`ai` (BootAi):

| Field | AI off (`PURSAE_BUILDER_AI=0`) | AI on |
|-------|------------------------------|-------|
| `enabled` | `false` | `true` |
| `providers` | `[]` | configured providers only (`{id, models:[{id,name}]}`); `[]` when no key is set — the editor then runs only the demo engine and nothing leaves the browser |
| `llmBase` | `""` | absolute `…/wp-json/web-builder/v2/ai/llm/` |
| `imagesUrl` | `""` | absolute `…/wp-json/web-builder/v2/ai/images` |
| `allowCode` | `false` | `false` (AI tools may not write html sections or custom CSS) |

### 2.1 Editor UI language

The editor UI is English; translations are catalogs in the patched upstream sources
(`builder/patches/0004-i18n.patch`):

- `src/builder/i18n.ts`: `t('English source', { name: value })` returns the string in the current
  language (`{name}` placeholders), falling back to the English source. `setLocale(locale)` picks the
  language by the locale's language part (`tr_TR`, `tr-TR`, `tr` → Turkish); a language without a
  catalog is English. Outside a browser (Node: fixtures, tests) it is English.
- `src/builder/i18n/tr.ts`: the Turkish catalog (English source → Turkish). Shipped catalogs: `tr`.
- `builder/host/locale.ts` calls `setLocale(window.__WEB_BUILDER__.locale)`. `host/editor-entry.tsx`
  imports it and only then loads the editor (`host/editor-main.tsx`) with a dynamic import: some
  strings are translated once at module load (section kinds, dynamic widget labels, design names,
  constant option tables), and a statically imported chunk would run before the entry's code and
  see the upstream default (Turkish). The language is fixed for the page; a locale change takes
  effect on the next editor load.
- Only the editor chrome is translated. Document content (the default page, the default texts of a
  new section or design, form placeholders) is plain English and stays as stored.
- `renderStatic` runs inside `withSourceLocale()`: published HTML never depends on the editor's
  language (`test/host/i18n.test.ts` renders the same document in `en_US` and `tr_TR`).
- `make builder-test` runs `builder/scripts/lint-strings.mjs`: it fails on Turkish text (Turkish
  letters, or words only found on the Turkish side of the catalog) in any string of the patched
  upstream sources or `builder/host`, on a `t()` string without a Turkish entry, and on catalog
  entries whose placeholders differ from the source. `src/builder/ai/**` (stubbed out of the build)
  is not scanned; a non-UI literal can opt out with an `i18n-ignore` comment.

New UI strings: write `t('English')` upstream and add the Turkish entry to `tr.ts` in the same
patch. Other CMS languages (the 135 of `internal/i18n`) are a follow-up: the editor screen would
pass a catalog for `locale` in the boot config and `i18n.ts` would register it before the builder
modules load; nothing reads such a field yet.

## 3. REST: namespace `web-builder/v2`

Authentication is the core one (`restAuthUser`): cookie + `X-WP-Nonce`, or an Application
Password. Routes exist only while the plugin is active; AI routes only while AI is on. Errors use
the WordPress shape `{"code", "message", "data": {"status", …}}`. Times are RFC 3339 strings.
Request bodies of document routes are capped at **4 MiB** (`413 wb_too_large`, `data.limit`).

| Method | Path | Capability | Body → Response |
|--------|------|------------|-----------------|
| GET | `/documents/{id}` | `edit_post` | — → DocumentResponse |
| PUT | `/documents/{id}/autosave` | `edit_post` | AutosaveRequest → AutosaveResponse |
| DELETE | `/documents/{id}/autosave` | `edit_post` | — → `{deleted: true}` |
| POST | `/documents/{id}/publish` | `edit_post` | PublishRequest → PublishResponse |
| POST | `/documents/{id}/lock` | `edit_post` | LockRequest → LockResponse |
| GET | `/documents/{id}/revisions?page=N` | `edit_post` | — → RevisionSummary[] (newest first, 50 per page, `X-WP-Total`, `X-WP-TotalPages`) |
| GET | `/documents/{id}/revisions/{rid}` | `edit_post` | — → RevisionResponse |
| POST | `/documents/{id}/revisions/{rid}/restore` | `edit_post` | RestoreRequest → DraftState |
| GET | `/documents/{id}/plugins` | `edit_post` | — → PagePanels (§3.7) |
| GET | `/documents/{id}/languages` | `edit_post` | — → LanguagesInfo (§3.7) |
| POST | `/documents/{id}/translations` | `edit_post` + `edit_posts` (a template: `edit_theme_options`) | `{lang}` → Translation (201, §3.7) |
| GET | `/library` | `edit_posts` | — → LibraryItem[] |
| POST | `/library` | `edit_posts` | LibraryCreateRequest → LibraryItem (201) |
| DELETE | `/library/{id}` | author, or `edit_others_posts` | — → `{deleted: true}` |
| GET | `/prefs` | logged in, `edit_posts` | — → Prefs |
| PUT | `/prefs` | logged in, `edit_posts` | Prefs (merged; `null` deletes a key) → Prefs |
| GET | `/fonts/css2?family=…&display=swap&_wpnonce=…` | `edit_posts` | — → `text/css` |
| POST | `/forms/{id}/{section}` | public | form fields → FormResponse or 303 |
| GET | `/ai/status` | `edit_posts` | — → AiStatusResponse |
| POST | `/ai/llm/{provider}/{path}` | `edit_posts` | provider-native JSON → provider response / SSE |
| POST | `/ai/images` | `edit_posts` + `upload_files` | AiImageRequest → AiImageResponse |
| GET | `/templates?type=` | `edit_theme_options` | — → TemplateSummary[] (§11.8) |
| POST | `/templates` | `edit_theme_options` | TemplateCreateRequest → TemplateCreateResponse (201) |
| PUT | `/templates/{id}/conditions` | `edit_theme_options` | TemplateConditionsRequest → TemplateConditionsResponse |
| PUT | `/templates/{id}/preview` | `edit_theme_options` | TemplatePreviewRequest → TemplatePreviewResponse |
| GET | `/templates/{id}/conflicts` | `edit_theme_options` | — → TemplateConflict[] |
| GET, PUT | `/templates/{id}/popup` | `edit_theme_options` | popup templates only: PopupSettings `{trigger: load\|scroll\|exit\|click, delay 0–300, scroll 1–100, selector (#id\|.class), frequency: always\|session\|once\|days, days 1–365, version, position: center\|top\|bottom, width 200–1600, overlay, afterConsent}`; 422 on an invalid value |
| GET | `/outdated?after=&limit=` | `manage_options` | pages published by an older renderer → `{renderer, total, items[{id,title,type,status,renderer}]}` (id ascending, keyset) |
| GET | `/navigator` | `edit_posts` or `edit_theme_options` | — → NavigatorResponse: templates (`edit_theme_options` only, ≤200) and per enabled post type the ≤100 newest-modified posts the user may edit; trash excluded. The editor's page switcher |
| GET | `/themes` | `edit_theme_options` | — → `{themes, existing, source}` (theme library, [guide](web-builder-theme-library.md)) |
| POST | `/themes/{slug}/import` | `edit_theme_options` (+ `edit_pages`, `publish_pages`) | `{mode, pages}` → drafts with docs `{templates, pages, media}` |
| POST | `/themes/{slug}/finish` | `edit_theme_options` | `{mode, templates, pages}` → `{disabled, menus, pages}` |
| GET | `/site-settings` | `edit_theme_options` | — → SiteSettings (§3.6) |
| PUT | `/site-settings` | `edit_theme_options` | partial SiteSettingsRequest → SiteSettings; `tokens: {colors:{primary,secondary,accent,text,background}, fonts:{heading,body}}` sets the design tokens (CSS variables `--wb-color-*`, `--wb-font-*`) |
| GET | `/preview-context?object=` | `edit_posts` (+ `read_post` for `post:<id>`) | — → DynContext (§11.6) |

`{id}` of a document route must be a post of an enabled post type (pages by default, posts
optional) or a `wb_template`: otherwise **404** `wb_not_enabled`. Trashed posts answer **404**
`wb_not_found`. A `wb_template` additionally needs `edit_theme_options` on **every** document
route (403 `rest_forbidden` otherwise): the core maps its meta caps to `*_posts`, so `edit_post`
alone would admit authors and editors. The server's check is `wbEditable(user, post)`.

### 3.1 Documents

DocumentResponse:

| Field | Type | Notes |
|-------|------|-------|
| `postId` | number | |
| `postType` | string | |
| `status` | string | core status |
| `rev` | number | published builder revision counter; `0` = never published from the builder |
| `doc` | DocState \| null | `null` = new page, the editor starts from its default doc |
| `page` | PageSettings | the stored page object with the post-owned fields (title, slug, description, indexable) taken from the post; the description is the Pursae SEO description when it was changed since the last builder publish, else the excerpt. It (and `draft.page`) also carries `_loaded: {rev, description, indexable}`, the post's values as loaded, which the editor sends back unchanged (see Publish step 6); `_loaded` is never stored |
| `draft` | DraftState \| null | the **current user's** autosave, only when newer than `doc` |
| `detached` | bool | `posts.content` was edited outside the builder (§6.3) |
| `modified` | string | `posts.modified_at` |
| `docModified` | string | when the stored doc was last written (`""` without one); the editor keeps `draft` only when its `savedAt` is not older than this (not `modified`: a rename outside the builder must not discard the draft) |
| `renderer` | number | renderer of the stored snapshot |
| `lock` | LockInfo \| null | another user's live lock |

DraftState: `{doc, page, baseRev, savedAt}`.

**Autosave** (`PUT`): body `{doc, page, baseRev}`. Writes the caller's row in
`web_builder_drafts`; the live page is untouched. The client debounces to at least 2 s
(`AutosaveMinMillis`). The doc is validated like a publish (422 on structure errors) and,
without `unfiltered_html`, html sections, code sections, `content.html` and `style.css` keep their
stored values (§8.1, "Privileged parts on save") and are stripped where nothing is stored. Answers
`{savedAt, rev}`; `rev > baseRev` means someone else published since the editor loaded. Also
refreshes the caller's edit lock.

**Publish** (`POST`) body PublishRequest:

| Field | Type | Notes |
|-------|------|-------|
| `baseRev` | number | the `rev` the editor started from |
| `status` | `publish` \| `draft` \| `pending` \| `private` | without `caps.publish`, `publish`/`private` become `pending` |
| `reattach` | bool | required (`true`) on a detached page, else 409 `wb_detached` |
| `doc` | DocState | source of truth |
| `page` | PageSettings | |
| `render` | RenderResult | §4 |
| `utilityCss` | string | Tailwind `compile()` of `render.classes` |
| `serverRender` | bool | `true` = the server draws `render` itself with the server render bundle (§4.1) and ignores `render` / `utilityCss` from the body: how clients without the editor publish (Pursae MCP). **503** `wb_server_render_unavailable` when the build has no bundle; **422** `wb_server_render_failed` when the renderer throws |

Server steps, in one transaction:

1. `X-WB-Api` check; `edit_post`; lock check (another live lock → **409** `wb_locked`,
   `data.lock`).
2. `baseRev != rev` → **409** `wb_rev_conflict` with `data: {rev, modified, modifiedBy}`.
3. Validate the doc (§8.1) → **422** `wb_invalid_doc`, `data.errors: ValidationError[]`.
4. Without `unfiltered_html`: the doc's privileged parts are the stored ones (§8.1, "Privileged
   parts on save"); any other html or code section and `style.css` is dropped from the doc **and**
   the snapshot, and `render.css` and `utilityCss` are dropped (both are page-wide custom CSS; the
   editor sends `utilityCss` empty). When stored privileged parts were kept, the server draws the
   snapshot itself from the resulting doc (§4.1, `serverRender`) and sanitizes it as an
   `unfiltered_html` author's, so html sections, code sections and custom CSS stay published;
   without the render bundle the request's render is sanitized as usual and only the code
   sections are filled in. The stored row then has `snapshot_unfiltered = true`.
5. Sanitize `render.html` (§8.2) and `utilityCss` + `render.css` (§8.3).
6. Map PageSettings: `title` → post title, `slug` → `post_name`, `description` → excerpt and
   Pursae SEO description, `indexable=false` → Pursae SEO noindex. `description` and `indexable`
   are written only when the editor changed them: compared with `page._loaded` while
   `_loaded.rev == baseRev`, else with the last published page object (no previous publish = changed).
   A Pursae SEO edit made while the editor was open (its SEO tab, the classic metabox) is not
   overwritten by the unchanged value the editor loaded.
7. `posts.content` = fallback HTML generated in Go from the doc (§6.2); store
   `content_hash` = sha256 hex of exactly that string. The core update creates a
   `post_revisions` row; a `web_builder_revisions` row is written for it.
8. Upsert `web_builder_docs` (`rev = rev + 1`, `detached = false`), set `_web_builder=1`,
   delete the caller's draft, purge Pursae Cache for the post.

PublishResponse: `{rev, status, permalink, revisionId, modified, stripped}` (for a template
`permalink` is `template.preview.url`, §11.7); `stripped` lists
what the server removed: `html-sections`, `custom-css`, `markup` (sanitizer changed the
snapshot), `css` (CSS rules dropped). The editor shows a notice when it is not empty.

**Lock**: WordPress format `_edit_lock = "<unix>:<user id>"`, 150 s TTL. `POST lock` with
`{}` takes a free or own lock; `{take: true}` takes another user's lock. Answers
`{lock: null | LockInfo (the other holder), expires}`. The editor refreshes every 60 s; autosave
also refreshes.

**Revisions**: RevisionSummary `{id (post_revisions.id), rev, author, date, title, renderer}`.
RevisionResponse `{id, rev, doc, page}`. **Restore** writes the revision as the caller's draft
(`baseRev` from the body) and returns that DraftState; the editor loads it and the user
publishes (or a client publishes it with `serverRender`, §4.1).

### 3.2 Library and prefs

LibraryItem = SavedComponent `{id, name, section}` + `{author, created}`. `id` is the
`web_builder_library.id` as a decimal **string** (upstream ids are strings). The section is
validated like a doc section; without `unfiltered_html` html sections and `style.css` are
refused/stripped. At most 500 items per site.

Prefs: a JSON object of the user's editor preferences (upstream localStorage keys such as
`pursae-builder:recent-fonts`, `pursae-builder:layout-width`), stored in user meta
`_web_builder_prefs`. Keys match `^[a-z0-9:._-]{1,64}$`; the encoded object is at most 16 KiB.

### 3.3 Fonts

`GET fonts/css2` takes the Google Fonts css2 query (`family=Inter:wght@400;700&display=swap`)
and answers CSS whose `src` URLs are under `/wp-content/uploads/web-builder/fonts/`. Downloads
happen only here (editor users), with a weight allowlist, one in-flight download per family and a
disk cap. Public pages link only fonts that are already local; an anonymous request never
triggers a download.

### 3.4 Media

The core routes: `GET /wp/v2/media?search=&page=&per_page=&media_type=image` and
`POST /wp/v2/media` (multipart field `file`). The host reads `id`, `source_url`, `alt_text`,
`mime_type`, `media_details.{width,height,sizes}`. Media are never stored as `blob:` URLs.

### 3.5 Forms

`POST /forms/{id}/{section}` is public. The schema comes from the **stored** doc (the section
with that id must be a `form` section of a published builder page), never from the request.

| Field (input `name`) | Rule |
|----------------------|------|
| `name` | ≤ 200 chars |
| `email` | valid address, ≤ 254 |
| `phone` | ≤ 50 |
| `message` | ≤ 5000 |
| `date`, `choice` | ≤ 200 |
| `company`, `subject` | ≤ 200 (a design's form bound to the endpoint, below) |
| `field_<name>` | `^field_[a-z0-9_]{1,40}$`, ≤ 200 (≤ 5000 for a textarea); a bound design form only |
| `wb_hp` | honeypot; must be empty (non-empty → silently "sent", nothing mailed) |
| `wb_t` | ms between page load and submit, set by the runtime; `< 2000` → treated as spam |
| `cf-turnstile-response`, `g-recaptcha-response` | checked when Pursae Contact Form has a captcha configured |

A code section's `<form>` bound to the endpoint (`content.binds` kind `form`, §8.1) posts to
`forms/{id}/{section id}-w{data-wbc}`. Its schema is read from the stored markup on each submit
(`webbuilder.CodeFormSchema`): every named input, select and textarea (not buttons, files, hidden or
password inputs) under its own input name, mapped to a field name above by the binding's `fields`
(default: the name itself when it is one of them, else `field_<name>`), required by `required` /
`aria-required="true"` or the binding's `required` list, a select's value one of its options; values
are stored and mailed under the field names with the inputs' labels.

Body is `application/x-www-form-urlencoded` or `multipart/form-data` (no files), at most 64 KiB.
Per-IP rate limit → **429** `wb_rate_limited` with `Retry-After`. Invalid → **422**
`wb_form_invalid`, `data.fields: {field: "required"|"invalid"|"too_long"}` (a code form's fields by the design's input names). Mail goes through the
core mailer to the form's recipients: its own `formTo` / `formCc` section content keys, else Settings ›
Web Builder (Form recipients, Form copies), else the admin email. Every valid submission is also stored
(`web_builder_form_submissions`, listed on Settings › Web Builder unless switched off), so a message
survives a failed mail. Texts: `formSuccess` / `formError` content keys, else the site settings, else the
built-in English defaults, translated with the request language (Pursae Multi Language "Strings" or the
language pack). `formRedirect` (a path or URL of this site) / the site setting names a page to open after
a sent form.

Captcha: when Pursae Contact Form has Turnstile or reCAPTCHA keys the server adds the widget at the end of
every `form[data-wb-form]` of a filled snapshot (Turnstile `.cf-turnstile`; reCAPTCHA v3 a hidden
`g-recaptcha-response` filled by a small script before the runtime submits) together with the vendor script.

- `Accept: application/json` (runtime) → `200 {ok: true, message, redirect?}` (`redirect` is an absolute
  URL of this site; the runtime does not follow it, a small server-added script does via
  `data-wb-redirect` on the form).
- Otherwise (no JS) → **303** to the page with `?wb_form=sent` (or `?wb_form=error`) and
  `#<section id>`, or to the redirect URL after a sent form.

### 3.6 Site settings

While the builder is active the editor manages what the Customizer manages otherwise, so the
Customizer is hidden (menu, admin bar, `/wp-admin/customize` redirects to the builder's settings).
The builder menu (product name in the top bar) has a **Site settings** row (patch 0014,
`BuilderHost.menuExtra`) that opens the dialog.

SiteSettings (GET and the PUT response): `blogname`, `blogdescription`, `site_icon`
`{id, url} | null`, `custom_logo` `{id, url} | null` (only when the active theme supports a custom
logo: `supports_logo`), `show_on_front` (`posts` | `page`), `page_on_front`, `page_for_posts`
(page ids, 0 = none), `custom_css`, `pages` `[{id, title}]` (published pages, for the pickers),
`can_upload`.

PUT takes any subset of `blogname`, `blogdescription`, `site_icon`, `custom_logo` (attachment ids,
0 removes), `show_on_front`, `page_on_front`, `page_for_posts` and `custom_css`. The server validates
with the Customizer's own rules (`czSanitize`: tags stripped, images must be image attachments,
pages must be pages, homepage and posts page must differ) and stores the same options and theme mods,
then purges the page cache. Unknown keys and invalid values: **400** `rest_invalid_param`, nothing is
saved. Menus and widgets are not part of this route.

### 3.7 Page tools: languages and SEO

While Pursae Multi Language or Pursae SEO is on, the builder menu has a **Page tools** row (patch
0014, `BuilderHost.menuExtra`; host code `builder/host/page-tools.ts`) that opens a dialog with one
tab per plugin: **Languages** (the page's language, its translations, a button per missing
language) and **SEO** (title, description, focus keyphrase, canonical, robots, social fields and the
score). A page with no tab has no row, and the row is never shown in template mode. The three routes
below are the host's own; the SEO tab talks to the plugin's routes (`seogo/v1/meta/{id}` GET and POST
`{fields}`, `seogo/v1/analyze`), which enforce Pursae SEO's own capabilities.

All three answer `Cache-Control: no-store` and follow the document-route rules of §3: a post of an
enabled type or a `wb_template`, otherwise 404 `wb_not_enabled`; trashed → 404 `wb_not_found`;
`edit_post` on the post (`edit_theme_options` for a template), otherwise **403** `rest_forbidden`.

**PagePanels** (`GET /documents/{id}/plugins`): `{languages, seo, seoEdit, seoAdvanced, seoSocial}`, all booleans.

| Field | `true` when |
|-------|-------------|
| `languages` | Pursae Multi Language is on, the post type is a translated type (templates are, §11.1) and at least one language is active |
| `seo` | Pursae SEO is on, the post is not a `wb_template` and the user has the plugin's `seogo_onpage_general` capability (administrators always; others need `edit_posts` while the Role Manager module is off, else the role's grant) |
| `seoEdit` | the user may save the general fields (title, description, focus keyword). Today equal to `seo`; the editor shows the SEO tab only when both are `true` |
| `seoAdvanced` | `seo` and the user has `seogo_onpage_advanced`: canonical URL, noindex and nofollow are saved. Without it the plugin drops them silently, so the dialog disables those inputs |
| `seoSocial` | `seo` and the user has `seogo_onpage_social`: social title, description and image are saved; otherwise the dialog disables them |

Both off, or a capability missing, answer `200` with `false` values, never an error.

**LanguagesInfo** (`GET /documents/{id}/languages`):

| Field | Type | Notes |
|-------|------|-------|
| `enabled` | bool | `false` (and nothing else filled) while Multi Language is off, the type is not translated or no language is active |
| `canTranslate` | bool | the user has the post type's create capability (`edit_pages` for pages, `edit_posts` for the other types), which is not implied by being able to edit this post; for a template, `edit_theme_options` |
| `current` | LangRef | the page's own language (assigned the default one when it has none); absent when `enabled` is `false` |
| `translations` | LangTranslation[] | the page itself and every live translation, in language order. One the user may not `edit_post` is listed with `readOnly: true` and empty `title`, `status` and `editUrl`; a trashed or deleted one is not listed |
| `missing` | LangRef[] | active languages with no live translation (a trashed translation counts as missing) |

LangRef: `{slug, name, flag, rtl?}` (`flag` is an emoji). LangTranslation: LangRef +
`{postId, title, status, editUrl, current?, readOnly?}`; `editUrl` is the builder URL
(`/wp-admin/post?post={id}&action=web-builder`), `current` marks the page itself.

**Translation** (`POST /documents/{id}/translations`, body `{lang: "<language slug>"}`) creates the
translation Pursae Multi Language's "Duplicate content" would: a **draft** of the same type authored
by the current user, linked to the page's translation group, with title, slug, content, excerpt,
discussion settings, format, terms and custom fields copied like Add translation does, and the
page's builder doc and snapshot copied to it (§6.4), so the translation opens in the builder with
the same layout. Answer **201** with a LangTranslation (`status: "draft"`, no `current`). A
template's translation is a draft (disabled) template with the same type, display conditions and
priority; it renders in its language once it is published (§11.3).

| Status | Code | When |
|--------|------|------|
| 400 | `wb_bad_language` | `lang` missing, unknown or not an active language |
| 400 | `wb_not_enabled` | Multi Language is off or the type is not translated (nothing is created) |
| 403 | `rest_forbidden` | no `edit_post` on the page, no create capability for its type (`edit_pages` / `edit_posts`), or a template without `edit_theme_options` (the only capability a template's translation needs) |
| 409 | `wb_translation_exists` | the page already has a live translation in `lang` (the page's own language included); a trashed translation does not count, the new draft replaces it in the group |
| 500 | `db_error` | the draft could not be created or linked to the group; the draft is removed again (or trashed), so no unlinked copy is left |

Error codes added: `wb_bad_language` (400), `wb_translation_exists` (409).

**The dialog** (`builder/host/page-tools.ts`, `PageToolsDialog.tsx`; routes in `contract.ts`
`routes.documentPlugins`, `documentLanguages`, `documentTranslations`, `seoMeta`):

- At start the editor calls `GET /plugins` once. No tab (or a failed call, or template mode) → no
  "Page tools" row. The Languages tab (`languages`) and the SEO tab (`seo` and `seoEdit`) appear as
  reported; each tab loads its data every time it is shown, except that an SEO form with unsaved edits
  is never reloaded: switching tabs or closing and reopening the dialog keeps what was typed.
- **Languages** lists the translations (`Open` = `editUrl`, only same-site paths and `http(s)` URLs
  are followed) and, when `canTranslate`, an **Add translation** button per `missing` language. The
  button first sends pending edits (`autosaver.flush()`); if they still cannot be saved nothing is
  created. Then it POSTs `{lang}`, flushes once more (edits made while the request ran are not in
  the copy) and navigates to the answer's `editUrl`; when that second flush leaves edits unsaved the
  dialog stays open with a message. `409 wb_translation_exists` reloads the list. **Open** also
  flushes first and stays (with a message) when edits remain unsaved instead of dropping them; a
  Ctrl/Cmd-click opens the page in a new tab without asking. A `readOnly` translation has no Open button.
- **SEO** reads `seogo/v1/meta/{id}` and POSTs only the changed fields: `title`, `description`,
  `focus`, `canonical`, `og_title`, `og_description`, `og_image` (a URL, from the media library or
  typed), `noindex` (`"noindex"` or `""`; an explicit stored `"index"` stays unless the toggle is
  turned on) and `nofollow` (`"1"` or `""`). The response's `seo_score` (text, `""` = never
  analyzed) is shown with Pursae SEO's bands: 71 and up good, 41 and up needs improvement, else poor.
  The social image preview is drawn only for a same-origin address or one chosen from the media
  library in this session (with `referrerpolicy="no-referrer"`); typed or stored third-party URLs are
  not requested by the editor.

### 3.x Additions (API 2, backwards compatible)

- **Scheduled publish**: `PublishRequest.status` may be `future` with `date` (RFC 3339). The snapshot is
  stored at once and the post gets status `future` and that publish date; the core `publish_future_post`
  cron puts it live and purges Pursae Cache. A past date publishes now; without `publish_posts` the answer is
  `pending`; templates cannot be scheduled (400). `DocumentResponse.date` and `PublishResponse.date` report the
  date. Publishing a scheduled page with status `publish` publishes it now.
- **Template files**: `GET templates/{id}/export` returns a `TemplateFile`
  `{format: "web-builder-template", version: 1, type, title, conditions, priority, popup?, doc}`;
  `POST templates/import` takes one and creates a disabled template (doc validated like a publish for the
  importing user). Theme Builder screen: row action *Export*, *Import Template*, and *Export Theme Parts*
  (the published templates, single excluded, as a library `web-builder/parts.json`).
- **Site style** (the Site Editor's global styles): option `web_builder_site_style`, the doc.site keys
  `brand`, `fontPair`, `corners`, `headingFont`, `headingWeight`, `bodyFont`, `bodyWeight`, validated like
  doc.site (brand, fontPair and corners required). `Boot.siteStyle` carries it; site-settings GET adds
  `site_style` (object or `null`) and `site_style_outdated` (docs that follow it and still hold another look);
  PUT takes `site_style` (an object, or `null` to remove it; 400 `rest_invalid_param` when invalid, needs
  `edit_theme_options` like the rest of the route). A doc follows it with `site.global: true`: the editor
  starts new pages and templates with the style and `global: true`; `GET documents/{id}` (and the boot
  document and draft) answer such a doc with the current style's values in `site`; changing a colour, font
  or corner in the editor sets `global: false`. `GET outdated` (and Regenerate pages) also lists published
  docs with `site.global` whose stored site keys differ from the style, so after a style change they are
  drawn and published again with it.
- **Audience rules** (§11): `include|exclude/user/logged_in`, `user/logged_out`, `user/role/<role>`. They never
  place or score a template: with include audience rules the visitor must match one of them, a matching
  exclude vetoes. `SiteBoot.roles` lists the roles. Cache: while any published template has an audience rule,
  pages of logged-in visitors are never stored; anonymous visitors share the logged-out variant.

## 4. `renderStatic` result

The upstream live renderer (`src/builder/live/`) runs in the editor:
`renderStatic(doc, opts: RenderOptions): RenderResult`.

RenderOptions: `{postId, formAction(sectionId) → absolute URL, siteUrl}` — the renderer reads
no globals.

| Field | Type | Notes |
|-------|------|-------|
| `html` | string | exactly one root `<div class="wb-page …" data-wb-root style="--brand:…">…</div>`; brand/palette CSS variables are on the root; no editor attributes (`data-label`, selection ids), no inline `opacity:0`; ≤ 2 MiB |
| `css` | string | section custom CSS (`style.css`, the word `selector` already replaced by `#<cssId>`); privileged |
| `classes` | string[] | Tailwind candidate classes in `html` |
| `fonts` | FontNeed[] | `{family, weights: number[], italic}` |
| `features` | Feature[] | sorted, unique subset of `menu acc tabs dismiss form motion embed`; empty = no runtime |
| `renderer` | number | `RENDERER_VERSION` |

`hideOn` becomes container-query classes; the root is the `canvas` container
(`.wb-page{container-type:inline-size;container-name:canvas}` in the base CSS).

### 4.1 Server render bundle

`make builder` also writes `web/web-builder/render/render.js` (`builder/host/ssr.ts`,
`scripts/live.mjs` `buildServerRender`): one ES2017 IIFE with React and the same `renderStatic`, plus a
prelude of the globals React's server build touches. It is embedded in the binary and **not served**.
`internal/webbuilder/ssr.go` runs it in an embedded JS engine (goja, pooled runtimes, 20 s limit) for
`serverRender` publishes. Globals (JSON strings in and out):

| Global | Does |
|--------|------|
| `__wbRender(doc, opts)` | `renderStatic`; `opts = {postId, siteUrl, formActionPrefix, sample?}`, a form action is `formActionPrefix + encodeURIComponent(sectionId)` |
| `__wbCatalog()` | section kinds (fields, item lists, options, designs with `extras`/`hide`, defaults), palettes, font pairs, corners, icons, decors, layout structures |
| `__wbCreate(spec)` | a complete section from `{kind, design?, name?, content?, items?, style?, bg?, grid?, box?, els?, hidden?}` (createSection + applyVariant, ids filled) |
| `__wbDesign(section, id)` | the section in another design of its kind (applyVariant) |

Without a DOM, `rich.ts` calls the host: `__wbHostRich(html, lists)` is `webbuilder.SanitizeRichLists`
(the same rules) and `__wbHostDecode` is `html.UnescapeString`. Recolored remote SVG icons keep their
own colors (svgTint needs a DOM). `TestServerRenderMatchesFixtures` checks the bundle against every
`testdata/render` fixture byte for byte; the audit checks the globals.

### 4.2 HTML import

Two ways turn an HTML page into free-form (`custom`) sections:

- **Exact (1:1)** — `internal/webbuilder/extract.js` runs in a real browser and reads the computed
  styles and boxes of the rendered page (Tailwind, CSS variables, grids, web fonts, gradients all
  resolved), at 1440 px and at 390 px. Pursae serves the browser page itself:
  `GET /web-builder-import/{token}` (a one-time token of an import job, two hours, kept in memory; at
  most 5 open jobs per user and 64 MB of HTML and extractions in all) draws
  the HTML in two `sandbox="allow-scripts"` frames (opaque origin: no access to the site, its cookies or
  the page) and `POST`s both extractions back to the same URL; its own CSP lets the design load CDN
  scripts and fonts (srcdoc frames inherit it) and forbids framing. `webbuilder.ConvertExtract`
  whitelists node kinds, layout and style keys, clamps numbers, checks colors, CSS strings and URLs,
  sanitizes rich text and inline SVG icons, and maps the phone-width pass to `stack` (rows), `colsM`
  (grids), `menu` (link rows hidden on phones), `hideOn` (parts hidden on phones), `els.*.m` (phone font
  size, line height, padding) and `ratioM` (pictures). The page's title, meta description, robots and
  Open Graph tags go to the page settings and Pursae SEO. Pursae MCP: `web_builder_import_html` +
  `web_builder_import_finish`.
- **Code** — the design kept as its own markup in `code` sections (privileged, top-level only):
  `content.html`, `content.head`, `content.wrap`, `content.kit`, and the front end's `content.lang`,
  `content.htmlAttrs`, `content.bodyAttrs` (the design's document attributes), `content.active` (a
  site import's active-link rule on a header or footer template) and `content.binds` (content
  bindings, §8.1). The renderer draws
  `<div data-wb-code="{id}"></div>`; the publish route puts the markup in its place after sanitizing
  (`webbuilder.FillCode`); the front end prints a shell the header, page and footer share once.
  A code section may carry **`local`** (the element editor's Style panel): local styles per element,
  see "Local styles" in §8.1.
  Files come from a design kit (`web-builder/v2/kits`); a whole site comes in with
  `web_builder_import_site` (`webbuilder.ConvertSite`). See
  [Code sections and design kits](web-builder-code-sections.md); builder patch `0018-code-sections`.
- **Quick** — `webbuilder.ImportHTML` reads style attributes and simple `<style>` rules in Go, without
  a browser (approximate; CDN utility classes are not resolved).

Patch 0017 (`import-fidelity`) adds what 1:1 needs to the free-form model: `NodeLayout.colsT` /
`colsM` (grid columns on tablets / phones), `NodeLayout.ratioM`, `ElStyle.m = {fontSize, lineHeight,
padding}` (phone sizes; unset = the automatic scaling), and colored `span`s in rich text.

## 5. `data-wb-*` runtime

The public runtime (`site/runtime.<hash>.js`, IIFE, no dependencies, ≤ 8 KB gzip) is loaded with
`defer` only when `features` is not empty. The base CSS (`site/wb.<hash>.css`) is linked on every
builder page. **Content must be readable with JavaScript off**: every hiding rule is scoped to
`.wb-js`, a class the page sets on `<html>` with a one-line inline script in `<head>` only when
the runtime will load. The runtime honours `prefers-reduced-motion: reduce` (no animation,
everything shown at once). All storage access is wrapped in try/catch.

| Feature | Markup | Behaviour (with JS) | Without JS |
|---------|--------|---------------------|------------|
| `menu` | `<details data-wb-menu><summary aria-label="Menu">…</summary><div>links</div></details>` (narrow-width markup; wide links are shown by container query) | closes on Escape, outside click and link click; syncs `aria-expanded` on the summary | native `<details>` toggle |
| `acc` | `<details data-wb-acc>` items, `<summary>` question; first item `open`; the group element carries `data-wb-acc-single` when only one may be open | closes siblings in a `data-wb-acc-single` group | native `<details>` |
| `tabs` | container `[data-wb-tabs]`; buttons `role=tab data-wb-tab="<id>" aria-controls aria-selected`; panels `role=tabpanel data-wb-panel="<id>"`; inactive panels carry `data-wb-inactive`; optional `data-wb-autoplay="<ms>"` on the container (slides) | switches panels, arrow/Home/End keys, autoplay paused on hover/focus and off with reduced motion | `.wb-js` scoping: all panels visible, stacked |
| `dismiss` | element `[data-wb-dismiss="<key>"]` containing a `button[data-wb-dismiss-btn]` | hides the element and remembers it in `sessionStorage["wb-dismiss:<postId>:<key>"]` | stays visible |
| `form` | `<form data-wb-form method="post" action="<formAction>">`, inputs named per §3.5, a visually hidden `wb_hp` input (`tabindex=-1 autocomplete=off`), a hidden `wb_t`, an `[data-wb-form-status]` element with `aria-live="polite"` | fills `wb_t`, posts with `fetch` + `Accept: application/json`, sets `data-wb-state="sending|sent|error"` on the form, shows the message | plain POST, 303 back |
| `motion` | `data-wb-motion="<entrance>"` plus optional `data-wb-dur`, `data-wb-delay` (ms), `data-wb-ease`, `data-wb-repeat`, `data-wb-stagger="<ms>"` (children `[data-wb-stagger-item]`), `data-wb-parallax="-100…100"` + `data-wb-parallax-on="bg|content"`, `data-wb-scroll-fade`, `data-wb-scroll-scale` | adds `wb-in` when in view (IntersectionObserver), scroll effects on rAF | visible, static |
| `embed` | `<iframe data-wb-embed="<id>" sandbox="allow-scripts" srcdoc="…">` (html sections); the srcdoc posts `{__wb: "<id>", h: <px>}` to the parent | sets the iframe height from messages whose `__wb` matches and whose source is that iframe | default height |

CSS-only, no runtime needed: `data-wb-sticky` (position: sticky) and `data-wb-hover="lift|glow|scale"`
+ `data-wb-hover-on="self|items"` (styles in the base CSS). Entrance values are the upstream
`EntranceId`s (`fade`, `fade-up`, `zoom-in`, …); easing the upstream `EasingId`s. A failsafe
in the base CSS reveals `.wb-js [data-wb-motion]:not(.wb-in)` after 3 s if the runtime never runs.

## 6. Storage

### 6.1 Migration `0027_web_builder.sql`

The next free number after `0026_plugins_source.sql`. Idempotent (`IF NOT EXISTS`), like the
other migrations; existing migration files are never edited.

```sql
-- Pursae Web Builder: the page doc (source of truth), its sanitized snapshot, per-user drafts,
-- doc revisions next to the core post revisions, saved components and AI usage.
CREATE TABLE IF NOT EXISTS web_builder_docs (
	post_id             bigint PRIMARY KEY REFERENCES posts(id) ON DELETE CASCADE,
	doc                 jsonb   NOT NULL,
	page                jsonb   NOT NULL DEFAULT '{}',
	html                text    NOT NULL DEFAULT '',   -- sanitized snapshot (RenderResult.html)
	css                 text    NOT NULL DEFAULT '',   -- sanitized utility CSS + section CSS
	fonts               jsonb   NOT NULL DEFAULT '[]', -- FontNeed[]
	features            text[]  NOT NULL DEFAULT '{}',
	renderer            integer NOT NULL DEFAULT 0,
	rev                 integer NOT NULL DEFAULT 0,
	content_hash        text    NOT NULL DEFAULT '',   -- sha256 hex of the posts.content we wrote
	detached            boolean NOT NULL DEFAULT false,
	snapshot_user       bigint REFERENCES users(id) ON DELETE SET NULL,
	snapshot_unfiltered boolean NOT NULL DEFAULT false,
	created_at          timestamptz NOT NULL DEFAULT now(),
	updated_at          timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS web_builder_drafts (
	post_id    bigint  NOT NULL REFERENCES posts(id) ON DELETE CASCADE,
	user_id    bigint  NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	doc        jsonb   NOT NULL,
	page       jsonb   NOT NULL DEFAULT '{}',
	base_rev   integer NOT NULL DEFAULT 0,
	updated_at timestamptz NOT NULL DEFAULT now(),
	PRIMARY KEY (post_id, user_id)
);

CREATE TABLE IF NOT EXISTS web_builder_revisions (
	revision_id bigint  PRIMARY KEY REFERENCES post_revisions(id) ON DELETE CASCADE,
	post_id     bigint  NOT NULL REFERENCES posts(id) ON DELETE CASCADE,
	rev         integer NOT NULL,
	doc         jsonb   NOT NULL,
	page        jsonb   NOT NULL DEFAULT '{}',
	renderer    integer NOT NULL DEFAULT 0,
	created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS web_builder_revisions_post ON web_builder_revisions (post_id, rev DESC);

CREATE TABLE IF NOT EXISTS web_builder_library (
	id         bigserial PRIMARY KEY,
	name       text   NOT NULL,
	data       jsonb  NOT NULL,                           -- Section
	author_id  bigint REFERENCES users(id) ON DELETE SET NULL,
	created_at timestamptz NOT NULL DEFAULT now(),
	updated_at timestamptz NOT NULL DEFAULT now()
);

-- AI proxy: per-user, per-model token counts per UTC day (rate limit and daily budget).
CREATE TABLE IF NOT EXISTS web_builder_ai_usage (
	day           date   NOT NULL,
	user_id       bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	provider      text   NOT NULL,
	model         text   NOT NULL,
	requests      integer NOT NULL DEFAULT 0,
	input_tokens  bigint  NOT NULL DEFAULT 0,
	output_tokens bigint  NOT NULL DEFAULT 0,
	PRIMARY KEY (day, user_id, provider, model)
);
```

Not in `posts.meta`: that column is read with every post and carries a GIN index, so 2 s
autosaves there would be costly. The post only gets the marker meta `_web_builder = "1"`, so
list screens can show "Pursae Web Builder" cheaply. Old builder data is removed by a separate
migration (`0028_drop_old_builder.sql`, package R3); 0027 does not touch it.

### 6.2 Fallback HTML (`posts.content`)

Generated in Go from the doc (`internal/webbuilder`), never from the snapshot. Semantic and
readable: per visible section, headings (`h1` for the first hero title, `h2` for section titles,
`h3` for items), paragraphs, lists, `<img>` with alt, links and buttons as `<a>`, quotes as
`<blockquote>`, FAQ as `<h3>` + `<p>`; nav and footer link rows as `<ul>`. Forms, html sections,
decor and motion are omitted. It is what REST `content.rendered`, Pursae Headless, feeds, search,
sitemaps and the theme fallback (plugin off or detached) show.

### 6.3 Drift (detached)

A save from outside the builder (block or classic editor, `/wp/v2/posts`) that makes
`sha256(posts.content) != content_hash` sets `web_builder_docs.detached = true`. The front then
renders `posts.content` in the theme; the editor shows "Edited outside the builder" with two
choices: publish with `reattach: true` (discard the outside edits) or leave the builder for this
page.

The check runs in `afterPostSave` (every admin, REST and Quick Edit save), the core autosave of a
draft and the Wasm host's `UpdatePost`, whether or not the plugin is active. It hashes the
**stored** `posts.content` and never clears the flag; only a publish does.

### 6.4 Export, import and copies

- **WXR export** adds three post meta rows to a post that has a doc: `_web_builder_doc` (DocState
  JSON), `_web_builder_page` (PageSettings JSON) and, on a detached page, `_web_builder_detached =
  "1"`, next to the ordinary `_web_builder = "1"` marker. The snapshot is not exported.
- **WXR import** validates the doc like a publish (stripping html sections and custom CSS without
  `unfiltered_html`), applies the attachment URL backfill to it and writes a docs row with
  `renderer = 0`, an empty snapshot, `rev = 1` and `content_hash` of the imported `posts.content`.
  The transport keys never stay in `posts.meta`; an invalid doc is reported in the import log and
  the post stays an ordinary post (marker removed). Until Regenerate renders it, the front shows
  the fallback (`renderer < RendererVersion` with no `html`).
- **Pursae Multi Language** "Duplicate content" copies the doc and snapshot to the new translation
  (form actions retargeted to it, `content_hash` of its content, detached when the source is);
  custom-field synchronization never copies `_web_builder*` meta. The editor's `POST
  /documents/{id}/translations` (§3.7) uses the same copy.
- **Pursae Cache**: public builder pages carry the `web-builder` tag (`wbPurgePages`) besides
  `post:<id>`.

## 7. Assets and build output

| URL prefix | Served from | Rules |
|------------|-------------|-------|
| `/wp-content/plugins/web-builder/dist/editor/` | `web/web-builder/editor/` | AI build; **404 when `PURSAE_BUILDER_AI=0`**; plugin active only |
| `/wp-content/plugins/web-builder/dist/editor-noai/` | `web/web-builder/editor-noai/` | no-AI build (no provider strings, no `/ai/` paths); plugin active only |
| `/wp-content/web-builder/site/` | `web/web-builder/site/` | public `runtime.<hash>.js` and `wb.<hash>.css`; `Cache-Control: public, max-age=31536000, immutable` |
| `/wp-content/uploads/web-builder/css/` | uploads | page CSS above 24 KiB of published pages without a password: `post-<id>-<rev>.css` |
| `/wp-content/uploads/web-builder/fonts/` | uploads | localized Google Fonts |

None of these overlap the old builder's `/wp-content/plugins/web-builder/assets/`.

Today `make builder` writes only `editor/` (AI modules stubbed, see §9), `site/` and `render/` (the
server render bundle, §4.1, embedded and never served); the
manifest has no `editor-noai` entry and the server serves whichever editor bundle the manifest
names. The `editor-noai` prefix stays reserved for the AI split.

`web/web-builder/manifest.json` (Manifest; paths relative to each bundle directory):

```json
{
  "editor":      { "js": "assets/index-3f9a1c.js", "css": ["assets/index-8b2e4d.css"] },
  "editor-noai": { "js": "assets/index-77c0aa.js", "css": ["assets/index-8b2e4d.css"] },
  "site":        { "runtime": "runtime.1d2e3f.js", "css": "wb.4a5b6c.css" }
}
```

`web/web-builder/BUILD.json` (BuildInfo):
`{"upstreamSha": "<40 hex>", "patches": {"0001-host-seam.patch": "<sha256>", …}, "apiVersion": 2, "rendererVersion": 1}`.
A Go test compares it with `builder/UPSTREAM_SHA`, the patch files and the constants in
`contract.go`.

## 8. Trust: validation and sanitizer allowlist

The snapshot and the CSS come from the browser and are **untrusted**. The doc is the source of
truth. Allowlists are the Go variables in `contract.go`; the policy in `internal/webbuilder` is
built from them.

### 8.1 Doc validation

| Rule | Limit / allowed |
|------|-----------------|
| Request body | ≤ 4 MiB |
| Top-level sections | ≤ 200 |
| Items per section | ≤ 500 |
| `Item.sub` nesting | ≤ 4 |
| Ids (section, item, saved component) | `^[A-Za-z0-9_-]{1,64}$`, unique per doc |
| `kind` | `nav hero banner features media steps quote faq cta footer text image form layout html custom`, and `loop` (§11.4) once its validation lands |
| `Item.dyn` | DynSettings (§11.4): enums, lengths and ranges of that table; only on a layout cell whose `title` is a dynamic widget |
| `PageSettings.layout` | `site`, `canvas` or absent |
| Strings | ≤ 64 KiB; `content.html` of a `html` section ≤ 256 KiB |
| Rich text in content strings | upstream rich.ts subset: `b i u s code a(href) br ul ol li`, and `span` whose only style is a color (`style="color: rgb(…)"`, patch 0017) |
| `href` | `http`, `https`, `mailto`, `tel`, `#…`, relative |
| Image URLs (`image`, `bgImage`, item image) | `http`, `https`, relative, `data:image/*` ≤ 32 KiB |
| `bg.decorSvg` | sanitized to the SVG subset below |
| CSS strings (`style.css`, `els.*.bgImage`, `border`, `shadow`) | no `</style`, CSS rules of §8.3 |
| Privileged (`unfiltered_html` only; with an Application Password also the `themes` scope or full access) | `html` sections, `style.css` |

Enum fields with unknown values are a 422 `bad_value`. Fields the Go types do not know are kept
as sent (the stored doc is the client JSON with privileged fields removed at the JSON level).

**Privileged parts on save.** A user without `unfiltered_html` may edit a page or template that an
administrator gave html or code sections or custom CSS, without losing them
(`webbuilder.DocOptions.Stored`). Autosave, publish and revision restore compare the doc with the
stored docs (the published doc, then the saver's own draft; a restore also the revision), by
section id:

- an html or code section the doc lists by the id of a stored one is the stored section, at the
  position the doc gives it (only its `hidden` flag is the doc's; a code section only at the top
  level). Reordering, hiding and removing work; edits of its content are ignored;
- `content.html` and `style.css` of other sections are the stored values of the section with the
  same id;
- privileged parts with no stored counterpart are dropped. An edit that was ignored or dropped is
  reported in `stripped`.

**Local styles** (`local`, code sections only; `internal/webbuilder/local.go`,
`builder/host/elements/local.ts`). A code section's style edits, keyed by the element's `data-wbc` id:

```json
"local": {"214": {"label": "optional", "v": [
  {"bp": "",       "st": "",      "p": {"padding-top": "96px"}},
  {"bp": "tablet", "st": "",      "p": {"padding-top": "64px"}},
  {"bp": "",       "st": "hover", "p": {"background-color": "#0f172a !important"}}]}}
```

- `bp`: `""` (every device), `tablet`, `mobile`; `st`: `""`, `hover`, `focus`, `focus-visible`,
  `active`. `p`: CSS property → raw value (any unit, `calc()`, `var()`, `clamp()`); a value may end in
  `!important` (the editor adds it when the design's winning declaration is important). `label` is
  optional and not printed (the editor names elements in `content.names`).
- Validation (with `unfiltered_html`; 422 `bad_css` at `/sections/i/local/{id}/v/j/…`): ids
  `[A-Za-z0-9_-]{1,64}`; property names `--custom` or `-?[a-z][a-z0-9-]*`; values pass the CSS
  sanitizer's declaration rules (forbidden tokens, URL schemes, balanced) and hold no `;`, `{`, `}`,
  `<`, comment or control character, at most 4096 bytes; at most 2000 elements, 15 variants, 120
  properties per variant, 512 KB in all. `local` on any other section kind is dropped.
- Privileged like `content.html`: the code section is kept whole for users without `unfiltered_html`
  (above), so their edits of `local` are ignored and reported in `stripped`.
- Publish: `FillCode` adds the class `e{post}-{id}` to every element with a printed declaration
  (`data-wbc` is still stripped; no other byte of the markup changes) and appends
  `<style data-wb-local="">` to the code head (after the design's head), generated by `LocalCSS`:
  per device (desktop, then `@media (max-width:{tablet}px)`, then `@media (max-width:{mobile}px)`),
  per element (ids sorted bytewise), per state, one rule `.e{post}-{id}:not(#_):not(#_)[:state]{…}`
  with the declarations sorted by property name. The breakpoints are the section's kit's
  (`wb_design_kits[slug].breakpoints`, suggested from the kit's stylesheets on commit; default
  1023 / 639). A doc without local styles publishes exactly what it published before. The editor's
  TypeScript generator prints the same bytes (shared fixtures `internal/webbuilder/testdata/local`).

**Content bindings** (`content.binds`, code sections only; `internal/webbuilder/codebind.go`,
`builder/host/elements/bind.ts`; [Binding to content](web-builder-code-sections.md#binding-to-content)).
JSON `{"<data-wbc>": CodeBind}` (ids `^[0-9]{1,9}$`, at most 200):

| Field | Kinds | Rule |
|-------|-------|------|
| `kind` | all | `menu submenu loop field form` |
| `location` / `menu` | menu | a menu location (`^[A-Za-z0-9_-]{1,64}$`) or a menu id; one is required |
| `items` | menu | `{"<menu item id>": "<data-wbc of its template>"}`, ≤ 200 |
| `query` | loop | LoopSpec keys of §11.4 (`source` defaults to `query`) plus `parent` (children of that entry); ≤ 16 keys, values ≤ 400 characters |
| `field` | field | `title excerpt content image url link date modified author terms custom pagination` |
| `name` | field | custom: the field (`^[A-Za-z0-9_-]{1,64}$`, required); image: a URL field used without a featured image |
| `attr`, `text` | field (custom) | `attr` `"" html url attrs`; `text` with `attrs`: the element's text from `name` too |
| `format`, `tax`, `sep`, `links`, `words`, `size`, `srcset` | field | as DynSettings (§11.4): format `^[A-Za-z ,.:/-]{1,32}$`, tax a slug, sep ≤ 8 runes, words 5…100, size `thumbnail medium large full`; `links` terms as links; `srcset` image attachment attributes |
| `fields` | form | `{"<input name>": "<FormFields name \| field_<name> \| ->"}`, input names `^[A-Za-z0-9_.\[\]-]{1,64}$`, ≤ 40 |
| `required` | form | input names |
| `done`, `error` | form | `data-wbc` of the design's success and error elements |
| `title`, `to`, `cc`, `success`, `failure`, `redirect` | form | mail subject (≤ 200), recipients (≤ 1000), messages (≤ 500), redirect (≤ 2000) |

A bad entry answers 422 `invalid_enum` at `/sections/i/content/binds`; entries the server cannot read
are ignored on publish. Privileged like `content.html`. Publish: `FillCodeWith` →
`CompileBinds` adds §11.5 placeholders to the bound elements (splicing the markup), loops and forms
are keyed `{section id}-w{data-wbc}` (`BindKey`): `data-wb-loop` values, form `{section}` route
parameters; the loop query and the form schema are read from the stored doc.

**Kit overrides** (classes and variables of a design; `internal/webbuilder/kitoverride.go`,
`internal/server/webbuilder_kitoverride.go`, `builder/host/elements/overrides.ts`, `kitstyle.ts`). Not
part of the doc: one override per design kit, in the `wb_kit_overrides` option
(`{slug: {live, version, draft, draftVersion, updated}}`), edited through
`GET|PUT kits/{slug}/override` and `POST kits/{slug}/override/publish` (routes in
[Kits: REST](web-builder-code-sections.md#kits-rest); writing needs `edit_theme_options` and
`unfiltered_html`; the stored option is validated again with `ParseKitOverride` when it is read):

```json
{"rules": [
  {"ctx": ["@layer utilities"], "sel": ".rounded-2xl", "p": {"border-radius": "2rem"}},
  {"ctx": ["@layer theme"], "sel": ":root, :host", "p": {"--color-brand-600": "#1d4ed8"}}]}
```

- `ctx`: at most 6 preludes of `@media`, `@layer`, `@supports` or `@container` (no blocks, `<`, `>`,
  `\`, comments), outermost first; `sel`: a selector (no `{ } ; < >`, comment, URL or forbidden token,
  balanced, ≤ 1024 bytes), unique per context; `p`: declarations as local styles take them. At most
  2000 rules, 120 properties per rule, 256 KB. The printed stylesheet must pass `SanitizeCSS`
  unchanged, else 422 `bad_css`.
- `KitOverrideCSS` prints each rule on one line, wrapped in its contexts, declarations sorted by
  property name: `@layer utilities{.rounded-2xl{border-radius:2rem}}`; a rule without a valid
  declaration, or whose selector or context the parser refuses, prints nothing. The editor's `overrideCSS` prints the same bytes (shared fixtures
  `internal/webbuilder/testdata/override`).
- Front end: a page whose code head links a kit's files (`/wp-content/uploads/web-builder/kits/{slug}/…`)
  gets `<style data-wb-kit-override="{slug}">` with the live override right after the code head. The
  snapshot never holds it, so publishing the override needs no page to be republished, and a kit
  without a live override prints nothing.
- The editor keeps its working copy in the doc's top-level `wbKits` field (`{slug: {rules}}`) so that
  undo and redo cover class and variable edits; the host removes the field before every autosave and
  publish, and from a loaded doc. The server ignores it.

**Elements seam** (patch `0020-elements-seam`, `src/builder/elementsSeam.ts`): `handles(doc)`,
`Canvas`, `Nav`, `Tab` (Style and Advanced), `Content` (`{s, fallback}`: the Content tab of a code
section of a page the host draws; `fallback` is the builder's own content tab, the section's HTML and
head code) and `title`.

### 8.2 Snapshot HTML

- Elements: `div section header footer nav main article aside h1–h6 p span a strong b em i u s
  small mark sub sup br hr ul ol li dl dt dd blockquote q cite figure figcaption img picture source
  video button form input textarea select option label fieldset legend details summary table thead
  tbody tfoot tr th td caption time address code pre kbd iframe` and the SVG subset `svg g path
  circle ellipse line polyline polygon rect defs linearGradient radialGradient stop clipPath mask
  use title desc symbol text tspan`.
- Global attributes: `class style id title role lang dir hidden tabindex`, `aria-*`,
  `data-wb-*`. Every other `data-*` and every `on*` attribute is dropped.
- Per element: see `ElementAttrs` / `SVGAttrs` in `contract.go`. `input type` ∈
  `text email tel date hidden checkbox radio submit`.
- `a target` only `_blank`, which forces `rel="noopener noreferrer"`.
- `form action` only the site's own `web-builder/v2/forms/{this post}/{section}` URL,
  `method="post"`.
- `style` attributes go through the CSS sanitizer.
- URLs: `href` per §8.1; `src`/`srcset`/`poster` `http`, `https`, relative or `data:image/*`
  (≤ 32 KiB); SVG `href`/`xlink:href`, `clip-path`, `mask` only same-document `#id` /
  `url(#id)`.
- `iframe`: either `src` on `www.youtube-nocookie.com`, `www.youtube.com`, `player.vimeo.com`
  (https, embed paths) for every author, or `sandbox="allow-scripts"` + `srcdoc` (never
  `allow-same-origin`) from `unfiltered_html` authors; anything else is dropped.
- Dropped with their content: `script style link meta base object embed applet frame frameset
  noscript template foreignObject animate animateMotion animateTransform set math portal`.
- Exactly one root `div.wb-page[data-wb-root]`; anything else → 422.
- Comparison with fixtures is by DOM equivalence, not bytes (there is no hydration, so
  normalization is harmless).

### 8.3 CSS

- At-rules kept: `@media @container @supports @keyframes @font-face @layer @property`; all
  others (`@import`, `@charset`, `@namespace`, …) are dropped.
- A declaration is dropped when, after unescaping, it contains `expression(`, `behavior`,
  `-moz-binding`, `javascript:`, `vbscript:`, `-o-link` or `@import`.
- `url()` only `http`, `https`, relative, `#id`, or `data:image/*` ≤ 32 KiB.
- `@font-face src` only under `/wp-content/uploads/web-builder/fonts/`.
- `</style` anywhere → 422 `bad_css`. Total CSS ≤ 1 MiB.
- Pages ≤ 24 KiB of CSS inline it in `<style id="wb-css">`, larger ones get a file (§7).

## 9. AI

> **Status: not implemented.** AI is out of scope for the first release. The only bundle built is
> `editor/`, with every upstream `src/builder/ai/**` module stubbed out (`builder/vite/noai.ts`);
> the boot config always sends `ai.enabled = false` with no providers, no `/ai/*` route is
> registered and no `PURSAE_BUILDER_AI*` variable is read. This section is the design for when AI
> comes back; nothing below is live yet.

### 9.1 Environment (read once by `internal/config` as `Config.BuilderAI` and friends)

| Variable | Values | Effect |
|----------|--------|--------|
| `PURSAE_BUILDER_AI` | unset, `""`, `1`, `true`, `on`, `yes` → **on**; `0`, `false`, `off`, `no` → **off**; anything else → off with a startup warning | master switch |
| `PURSAE_BUILDER_AI_ANTHROPIC_KEY` | API key | enables provider `anthropic` |
| `PURSAE_BUILDER_AI_OPENAI_KEY` | API key | enables `openai` |
| `PURSAE_BUILDER_AI_GEMINI_KEY` | API key | enables `google` (Gemini, OpenAI-compatible endpoint) |
| `PURSAE_BUILDER_AI_MISTRAL_KEY` | API key | enables `mistral` |
| `PURSAE_BUILDER_AI_OLLAMA_URL` | `http(s)://host:port` | enables `ollama` at that base |
| `PURSAE_BUILDER_AI_MODELS` | `provider:model,provider:model` | model allowlist; unset = defaults (`DefaultAIModels`) |
| `PURSAE_BUILDER_AI_DAILY_BUDGET` | integer tokens/day (input + output, site-wide, UTC day) | unset = 1 000 000; `0` = no cap |

**AI off (`=0`)**: only `editor-noai` is served and `dist/editor/` answers 404; no
`/web-builder/v2/ai/*` route is registered (404); no AI settings UI; no key is read; zero outbound
requests; `ai.enabled=false`; Site Health shows "AI off".

**AI on, no key**: the editor bundle with AI is served, `ai.providers = []`, the client runs only
its rule-based demo engine and nothing leaves the server or the browser.

Keys live only in the environment. They are never sent to the browser, logged or echoed.

### 9.2 `POST /ai/llm/{provider}/{path}`

- `{provider}` ∈ `anthropic openai google mistral ollama` with a key/URL configured, else
  **503** `wb_ai_no_key` (**404** for an unknown provider).
- `{path}` must be in the provider allowlist, else **400** `wb_ai_path`:

  | Provider | Fixed base | Paths |
  |----------|------------|-------|
  | `anthropic` | `https://api.anthropic.com` | `v1/messages` |
  | `openai` | `https://api.openai.com` | `v1/chat/completions` |
  | `google` | `https://generativelanguage.googleapis.com` | `v1beta/openai/chat/completions` |
  | `mistral` | `https://api.mistral.ai` | `v1/chat/completions` |
  | `ollama` | `PURSAE_BUILDER_AI_OLLAMA_URL` | `v1/chat/completions` |

- Request body: the provider-native JSON the upstream `ai/llm.ts` already builds (Anthropic
  Messages or OpenAI chat completions), ≤ 8 MiB. `model` must be in the allowlist (**422**
  `wb_ai_model`); `max_tokens` is capped at 8192. Browser-sent `authorization`, `x-api-key`,
  `anthropic-*` and `cookie` headers are dropped; the server adds the key
  (`x-api-key` + `anthropic-version: 2023-06-01` for Anthropic, `Authorization: Bearer` for the
  others, none for Ollama). Upstream's `anthropic-dangerous-direct-browser-access` header is
  never sent.
- Response: without `"stream": true` the provider's JSON is passed through with its status.
  With `"stream": true` the response is `Content-Type: text/event-stream`, the provider's SSE
  bytes passed through unchanged and flushed per event (`data: …\n\n`; OpenAI-style streams end
  with `data: [DONE]`). Usage is read from the JSON `usage` or from the final SSE events and
  added to `web_builder_ai_usage`.
- Provider `401`/`403` become **502** `wb_ai_upstream` ("the provider rejected the server key");
  other provider errors pass through. Network failure → **502** `wb_ai_upstream`; timeout
  (120 s total, 30 s to first byte) → **504** `wb_ai_timeout`. Redirects are never followed.
- Per-user rate limit (default 30 requests/minute) → **429** `wb_rate_limited`;
  daily budget spent → **429** `wb_ai_budget`. Each call is written to the audit log
  (`web_builder.ai`: user, provider, model, tokens; never the prompt or key).
- Ollama: the base comes only from the environment (operator-trusted) and the request never
  chooses a host. Loopback and private ranges are allowed (Ollama usually runs there);
  link-local and cloud metadata addresses (`169.254.0.0/16`, `fe80::/10`, `fd00:ec2::254`) are
  refused even if configured.

`GET /ai/status` → `{enabled, providers, budget: {daily, used}}`.

### 9.3 `POST /ai/images`

Body/response match upstream `ai/images.ts` (`ImageQuery` → `ImageResult`) plus `mediaId`:
`{query, count? (1–12, default 6), orientation?}` → `{images: [{url, mediaId, width, height,
alt, credit, license, source}], error?}`. Searches Openverse and Wikimedia only
(`api.openverse.org`, `commons.wikimedia.org`, `upload.wikimedia.org`), image MIME types only,
≤ 10 MiB per file; results are sideloaded into the Media Library and `url` is the local URL.
Site cloning is not ported (no endpoint; the tool reports itself unavailable).

## 10. Front-end page (for reference)

No React for visitors. A builder page is a canvas document: `<!doctype html>`, `lang`, the
non-theme head (SEO, analytics, cookie, plugin heads), page CSS (inline ≤ 24 KiB or a file; only
a published page without a password gets the file, every other status stays inline), local
`@font-face` with a preload of the first face, the base CSS, the `.wb-js` inline script when
`features` is not empty, the body-open hooks, the stored snapshot (images through
`filterContentTags`) inside `<main id="wb-main">` (left out when the snapshot has its own main
landmark), `<script defer src="…/site/runtime.<hash>.js">` when `features` is not
empty, the admin bar and the footer hooks. Pages, posts and every builder-enabled post type get
the canvas. Plugin off or detached → the theme renders `posts.content`.

With **Hello Pursae** active (§11.9) the canvas is replaced by the theme's own document: the
header, main and footer parts (each a snapshot with its placeholders filled) are printed inside
Hello Pursae's skeleton, with the head and footer hooks of an ordinary theme page. A builder page
with `layout: canvas` still shows only its own doc between Hello Pursae's head and footer (no
header or footer part). Every other theme keeps the canvas behaviour above.

## 11. Theme Builder

Header, footer and main-content **templates** are designed in the builder like pages and shown on
the site by the bundled **Hello Pursae** theme. Display conditions choose one template per
location and request; dynamic widgets and post loops fill in the site's real content at request
time. Go names are in `contract.go` (section "Theme Builder"), TypeScript names in
`builder/host/contract.ts` (section "Theme Builder").

### 11.1 Identity

| Thing | Value | Notes |
|-------|-------|-------|
| Post type | `wb_template` (`TemplatePostType`) | not public, no archive, no core editor; translatable by Pursae Multi Language |
| Capability | `edit_theme_options` (`TemplateCap`) | checked explicitly on every template path: REST, editor, forms, hooks, core trash/untrash/delete |
| Type meta | `_wb_template_type` | one of the types of §11.2 |
| Conditions meta | `_wb_conditions` | JSON array of rules (§11.3), e.g. `["include/general"]` |
| Priority meta | `_wb_priority` | decimal −100…100, default 0 |
| Preview meta | `_wb_preview` | preview object (§11.6); empty = the type's default |
| Starter meta | `_wb_starter` | the starter id the template was created from |
| Active | `post_status = publish` | `draft` = disabled; `pending`/`private` are refused (422 `wb_bad_template`) |
| Theme | `hello-pursae` (`HelloPursaeTheme`) | menu locations `primary` ("Primary Menu"), `footer` ("Footer Menu") |
| Preview query | `?wb_template_preview=<id>` | forces that template on a front URL; `edit_theme_options` only; never cached |
| Cache tags | `wb-theme`, `wb-loop` | §11.9 |
| Admin screen | `/wp-admin/themes?page=web-builder-templates` | Web Builder › Theme Builder, plugin active only |
| Options | `web_builder_previous_theme`, `web_builder_theme_notice`, `web_builder_theme_offer` | §11.10 |

The doc, snapshot, drafts, revisions, lock and library are the existing ones (§3, §6):
a template is an ordinary builder document of another post type. Migration
`0029_web_builder_templates.sql` holds whatever indexes the template index needs (package TB).

### 11.2 Template types

Header and footer fill their own location; every other type fills **main**. A main template is
considered only on the views of its type (the *view gate*), before conditions are scored: a
single template never renders an archive, even with `include/general`.

| Type | Label | Location | Views | Default conditions | Starter | Default preview |
|------|-------|----------|-------|--------------------|---------|-----------------|
| `header` | Header | header | all | `include/general` | `header` | `front` |
| `footer` | Footer | footer | all | `include/general` | `footer` | `front` |
| `single-post` | Single Post | main | singular | `include/singular/post` | `single` | `latest` |
| `single-page` | Single Page | main | singular | `include/singular/page` | `single` | `latest` |
| `single` | Single | main | singular | `include/singular/<postType>` (create request) | `single` | `latest` |
| `front-page` | Front Page | main | singular, archive | `include/singular/front_page` | `blank` | `front` |
| `archive` | Archive | main | archive | `include/archive` | `archive` | `home` |
| `search-results` | Search Results | main | archive (search only) | `include/archive/search` | `search` | `search:a` |
| `error-404` | 404 Page | main | 404 | `include/404` | `blank` | `404` |
| `single-product` | Single Product | main | singular (store product views only) | `include/singular/product` | `product` | `latest` |
| `popup` | Popup | popup (any number) | all | `include/general` | `blank` | `front` |
| `product-archive` | Product Archive | main | archive (store shop and product taxonomy views only) | `include/archive/type/product`, `include/archive/product_cat`, `include/archive/product_tag` | `product-archive` | `type:product` |

Views: **singular** = a single post, page or custom post type entry (a static front page too);
**archive** = the posts index, a term, author, date or post type archive, search results;
**404**. `front-page` passes both gates because the front page may be the posts index.
`search-results` passes the archive gate on search views only (`TemplateTypeArchives`): even
with `include/archive` or `include/general` it never renders a term, date, author or post type
archive.

Starters (`blank header footer single archive search product product-archive`, upstream
`dynamic/starters.ts`) are loaded by the editor when a template's doc is empty; `blank` is an
empty doc.

**Store gate** (`StoreTemplateKinds`, `webbuilder.StoreGateOK`): Pursae Commerce store views
(`q.Store` / `QueryFacts.StoreKind`: `product`, `shop`, `product_cat`, `product_tag`,
`product_attr`, `cart`, `checkout`, `myaccount`) only take the store main types, and those types
render nowhere else: `single-product` on `product`, `product-archive` on `shop`, `product_cat`,
`product_tag` and `product_attr` (the attribute archive needs its own `archive/pa_<x>` rule). A
generic main type (`single-page`, `archive`, …) never renders on a store view, whatever its
rules; the cart, checkout and account pages get no main template. Header and footer apply
everywhere. The gate also holds for `?wb_template_preview`. The two store types are offered (Add
New, `POST templates`) only while Pursae Commerce is on; otherwise `POST templates` answers 422
`wb_bad_template`.

**Store pages' own docs**: the shop page, product taxonomy archives (whose synthetic post id is a
term id), the account page and the cart / checkout pages never render their page's own builder
doc, so a store page designed in the builder cannot replace the store (and a term id never loads
the doc of the post with the same id). Exceptions: a single product keeps its own doc (the
synthetic id is the real product id), and the Cart / Checkout page keeps it when the doc shows
the `shop-cart-page` / `shop-checkout-page` widget.

### 11.3 Display conditions

A rule is `<mode>/<scope>`, mode `include` or `exclude`. Scopes:

| Scope | Matches | Score |
|-------|---------|-------|
| `general` | every view | 10 |
| `singular` | every singular view | 20 |
| `archive` | every archive view | 20 |
| `singular/<ptype>` | singular views of that post type | 30 |
| `archive/home` | the posts index (`is_home`) | 30 |
| `archive/date` | date archives | 30 |
| `archive/search` | search results | 30 |
| `archive/author` | author archives | 30 |
| `archive/type/<ptype>` | that post type's archive | 30 |
| `archive/<tax>` | any term archive of that taxonomy | 30 |
| `singular/<ptype>/in/<tax>/<id>` | entries of the type that have that term | 40 |
| `singular/<ptype>/child_of/<id>` | direct children of that entry (hierarchical types) | 40 |
| `singular/<ptype>/by_author/<id>` | entries of the type written by that user | 40 |
| `singular/front_page` | the front page (`is_front_page`: static page or posts index) | 45 |
| `404` | the 404 view | 45 |
| `singular/<ptype>/<id>` | that entry | 50 |
| `archive/<tax>/<id>` | that term's archive | 50 |
| `archive/author/<id>` | that user's archive | 50 |

- `<ptype>` and `<tax>` match `^[a-z0-9_-]{1,32}$`; `<id>` is a positive decimal without
  leading zeros (≤ 19 digits). The whole grammar is `ConditionPattern` / `CONDITION_PATTERN`.
- Reserved words cannot be a `<ptype>` after `singular/` (`front_page in child_of by_author`) or
  a `<tax>` after `archive/` (`home date search author type`). The regular expression cannot
  express this; the parser (`internal/webbuilder/conditions.go`) rejects them.
- v1 resolves the taxonomies `category` and `post_tag`, the types `post`, `page` and public
  Pursae Custom Fields types; other names are valid rules that match nothing (custom taxonomy
  archives and sub-menus come later without a grammar change). With Pursae Commerce on, `product`,
  `product_cat` and `product_tag` resolve too (`singular/product`, `singular/product/in/product_cat/<id>`,
  `archive/type/product`, `archive/product_cat[/<id>]`); main templates on store views still pass
  the store gate (§11.2). `wb_template` never matches.
- At most **50** rules per template, **200** bytes per rule. An invalid rule answers **422**
  `wb_bad_condition` with `data.errors: ValidationError[]` (`path: /conditions/<i>`,
  `code: bad_condition`). Rules are trimmed; duplicates are dropped, order is kept.

Resolution for one location and request (pure code over a `QueryFacts` struct, package TA):

1. Candidates: published templates of the location's types whose view gate admits the view.
   With Pursae Multi Language and a request language, a template in a language that is neither
   the request's nor the default one is no candidate, and of each translation group only one
   member competes (the request language's, else the default language's). When the request
   language is not the default one, the templates in the request language (and those without a
   language) are resolved first; the default-language templates are only the fallback when none
   of them matches.
2. Any matching `exclude` rule vetoes the template. With Pursae Multi Language a rule that names
   a post, page, parent or term (`singular/<ptype>/<id>`, `singular/<ptype>/in/<tax>/<id>`,
   `singular/<ptype>/child_of/<id>`, `archive/<tax>/<id>`) also matches that object's translations.
3. The template's score is its best matching `include` score; no include match = not a
   candidate. On a search view a `search-results` template gets **+1** (`SearchTypeBonus`), so it
   beats an `archive` template with the same rule.
4. The highest score wins; ties go to the higher `_wb_priority`, then the newer `modified_at`,
   then the higher id.
5. With Pursae Multi Language the winner is swapped for its translation in the request's
   language when one is published.
6. The winner must have a published doc with a snapshot; otherwise the location falls back to
   the theme default (§11.9).

A template with no rules (or only excludes) matches nothing; the editor opens the conditions
dialog on the first publish of such a template.

### 11.4 Doc additions

**Dynamic widgets** are layout cells whose `Item.title` (upstream `CellType`) is one of:

`site-logo site-title site-tagline nav-menu search-form copyright post-title post-content
post-excerpt post-image post-meta post-terms post-date post-author author-box post-nav
post-comments archive-title archive-desc search-query pagination`

so `CellType = '' | 'heading' | 'text' | 'button' | 'image' | 'layout' | DynWidget`. Their
settings are `Item.dyn` (DynSettings, every field optional):

| Field | Type | Rule |
|-------|------|------|
| `tag` | string | `h1`…`h6`, `p`, `div`, `span` |
| `link` | bool | wrap the value in the post (or site) URL |
| `format` | string | WordPress date format, `^[A-Za-z ,.:/-]{1,32}$`; empty = the site's `date_format` |
| `size` | string | `thumbnail medium large full` |
| `ratio` | number | image width / height, 0.2…5 |
| `fields` | string[] | post-meta, in order: `date modified author comments terms` |
| `sep` | string | separator, ≤ 8 characters |
| `tax` | string | post-terms taxonomy (slug), default `category` |
| `location` | string | nav-menu: a menu location of the active theme |
| `menuLayout` | string | `row column burger` |
| `burgerAt` | number | container width (px) below which a row menu becomes a burger; 0 = never |
| `words` | number | excerpt length, 5…100 |
| `prefix` | string | text before the value ("Posted on", "Search results for:") |
| `placeholder` | string | search-form input placeholder |
| `button` | string | search-form button label; empty = icon only |
| `empty` | string | text when the value is empty, ≤ 200 characters |

**Loop section** (`kind: 'loop'`): a grid of post cards. The query is `content` (all strings),
the card design is `items[0].sub` (a layout section whose cells use post widgets), the grid is
`section.grid`. The server always reads the query from the **stored** doc, never from snapshot
attributes.

| `content` key | Values | Default |
|---------------|--------|---------|
| `source` | `main` (the request's own posts), `query` (custom), `related` (same type, shares a term with the current post) | `main` |
| `postType` | a viewable post type (query, related) | `post` |
| `perPage` | `1`…`24` (query, related; main uses `posts_per_page`) | `6` |
| `orderBy` | `date modified title menu_order comment_count rand` | `date` |
| `order` | `desc asc` | `desc` |
| `terms` | `category:12,post_tag:4` (≤ 20 pairs; related: the taxonomies to compare) | `""` (related: `category`) |
| `author` | a user id or `""` | `""` |
| `exclude` | `current` (leave out the post being viewed) or `""` | `""` |
| `offset` | `0`…`50` | `0` |
| `pagination` | `numbers prev-next none` (main only) | `numbers` |
| `empty` | text when there are no posts, ≤ 200 characters | `""` |

Loops nest at most 2 deep and render at most 50 items each. Custom loops show published posts
(plus private ones for users with `read_private_posts`), never Pursae Commerce membership-excluded
posts, never non-public types.

**nav / footer binding** (`content` keys of a `nav` or `footer` section):
`menu = '<location>'` takes the links from that menu location (read-only in the editor; `""` =
the section's own links); `logoSource = 'site'` takes the custom logo or, without one, the site
title.

**`PageSettings.layout`**: `site` (the page shows the header and footer templates around its doc)
or `canvas` (the page is the whole document). Absent = `canvas` when the doc has a top-level
`nav` or `footer` section (pages built before the Theme Builder, so no page shows two headers),
else `site` (`DefaultPageLayout`). It only matters while Hello Pursae renders the front; templates
ignore it. The server keeps unknown PageSettings keys when it merges post fields
(`wbPageMerge`). While Hello Pursae is active, a new page starts without nav and footer and with
`layout: 'site'`.

### 11.5 Snapshot placeholders

In live mode the renderer emits placeholders only, never sample values. The attributes are
existing elements with `data-wb-*` attributes, so the sanitizer allowlist of §8.2 is unchanged
(one exception: the search form below).

| Attribute | On | Meaning |
|-----------|----|---------|
| `data-wb-slot="<slot>"` | any element | filled from the slot (table below) |
| `data-wb-if="<slot>"` | any element | removed when the slot is empty |
| `data-wb-if-not="<slot>"` | any element | removed when the slot is not empty |
| `data-wb-loop="<section id>"` | the loop's list | repeats its `data-wb-loop-item` child per post; spec from the stored doc |
| `data-wb-loop-item` | exactly one child | the card template |
| `data-wb-loop-empty` | one child | shown only when the loop has no posts |
| `data-wb-menu-loc="<location>"` | a link list | repeats its `data-wb-loop-item` (`<a data-wb-slot="menu.url"><span data-wb-slot="menu.label">`) per menu item, depth first (three levels; a sub-item's link has class `wb-menu-d<level>`, a parent's `has-children`) |
| `data-wb-loop-part="1/2"` \| `"2/2"` | a `data-wb-menu-loc` list | first / second half of the items (split-logo navs) |
| `data-wb-format`, `data-wb-size`, `data-wb-tax`, `data-wb-sep`, `data-wb-words`, `data-wb-field` | a slot element | `Item.dyn` values; the server re-validates them (§11.4 rules) |
| `data-wb-search` | `<form method="get" action="<home>/">` | search form; one `<input type="search" name="s">` |
| `data-wb-item="<item id>"` | a slot element of a Commerce or language-switcher widget | the cell whose `Item.dyn` the server reads from the **stored** doc (§11.12); an id the doc does not have gets the widget defaults |
| `data-wb-text="<text slot>"` | any element (code bindings) | only the element's first direct text that is not blank takes the slot's value (escaped), its white space and other children stay |
| `data-wb-menu-tree` | a `data-wb-menu-loc` list (code bindings) | top-level items only; the list may hold several `data-wb-loop-item` templates: an item takes the one whose value lists `k:<menu item id>`, else `parent` when it has sub-items, else `default`, else the first; `menu-<id>` as the location names a menu by id |
| `data-wb-menu-sub` | an element inside such a template | repeats its own `data-wb-loop-item` templates per sub-item of the item (one level below a menu) |
| `data-wb-raw` | a `post.title` / `post.content` / `post.image` slot element | content printed as stored when it is imported markup (starts with an element, no blocks, no password), image `src` and `alt` only (else `data-wb-field`'s URL) |
| `data-wb-meta` | a `post.field*` / `post.image` slot element (code bindings only) | the field may also be the plain post meta a site import stores (`design_<n>`, `list<n>_<k>` and their attribute companions, `webbuilder.SiteFieldName`), not just a registered Pursae Custom Fields field; other meta is never read; the snapshot sanitizer removes it from an author's markup, the bindings are compiled after sanitizing |

In every loop or menu the blank text right after the first item template is written between two items
(the design's own white space between cards); blank text between templates is dropped. A snapshot with
code sections (`data-wb-code-page` or `<template data-wb-code-head>`) is compiled element by element:
only elements carrying placeholders are parsed (in their parent's context), every other byte is printed
as stored.

The menu binding is **not** `data-wb-menu`: that name is the runtime's disclosure menu
(`<details data-wb-menu>`, §5) and its feature detection.

The search form is the one sanitizer exception: a `form[data-wb-search]` is kept with
`method="get"`, `action` equal to the site's home URL (with a trailing slash) and only
`input[type=search][name=s]` plus a submit button; any other form still needs the builder form
action of §8.2. At render time the server sets that `action` to the home URL of the request
language (Pursae Multi Language, the same action core search forms use); in the query language
mode (`/?lang=fr`) the language goes into a hidden `lang` input, so a search started on a
translated page stays in that language.

Slots:

| Slot | Mode | Value |
|------|------|-------|
| `site.title`, `site.tagline` | text | `blogname`, `blogdescription` |
| `site.url` | attr | `href` = the home page in the request language (`links.Front()`: `/fr/` with Pursae Multi Language) |
| `site.languages` | html | the Pursae Multi Language switcher (§11.12); empty while it is off |
| `site.logo` | attr | `img` attributes of `custom_logo` (`attachmentImage`) |
| `site.year` | text | current year in the site time zone |
| `menu.label` | text | menu item label |
| `post.field` | text | Pursae Custom Fields value of the post named by `data-wb-field` (`[A-Za-z0-9_-]{1,64}`), a field some field group defines; with `data-wb-meta` (code bindings) also the post meta string of a site import's field name (`design_<n>`, `list<n>_<k>`, …) |
| `post.field-url` | attr | `href` (`src` on `<img>`) from a URL, link or image field named by `data-wb-field` |
| `menu.url` | attr | `href`; the current item also gets `aria-current="page"` and class `is-current` |
| `menu.link` | attr | `href` and `target` only (a code section's menu: the design's active-link rule marks the current item) |
| `post.title-text` | text | the title as stored (no texturizing, no title filters) |
| `post.term-names` | text | names of the post's terms of `data-wb-tax`, joined by `data-wb-sep` |
| `post.field-html` | html | a custom field (with `data-wb-meta` also a site import's field from post meta, as for `post.field`) as HTML, sanitized like post content (`KsesPost`) |
| `post.field-attrs` | attr | the attributes in SlotAttrs from the fields `<data-wb-field>_<attr>` (`_href`, `_src`, `_alt`, `_aria_current`, …) |
| `post.title` | html | `theTitle` |
| `post.url` | attr | `href` = permalink |
| `post.content` | html | `theContent` (rendered only when the snapshot uses the slot) |
| `post.excerpt` | html | `theExcerpt`, or the content trimmed to `data-wb-words` words |
| `post.date`, `post.modified` | text | `wp.FormatDate(data-wb-format or date_format)`; on `<time>` also `datetime` (RFC 3339) |
| `post.author` | text | display name |
| `post.author-url` | attr | `href` = author archive |
| `post.avatar` | attr | `img` attributes of the Media Library avatar |
| `post.author-bio` | text | user description |
| `post.image` | attr | `img` attributes of the featured image at `data-wb-size` |
| `post.terms` | html | escaped term links of `data-wb-tax`, joined by `data-wb-sep` |
| `post.comments-count` | text | approved comments |
| `post.comments-url` | attr | `href` = permalink + `#comments` |
| `post.nav` | html | previous / next post links (`v.PostNav`) |
| `post.comments` | html | Hello Pursae's comments template for the post |
| `archive.title`, `archive.description` | html | archive title and description |
| `search.query` | text | the search query |
| `search.count` | text | posts found (`v.TotalPosts`) |
| `loop.pagination` | html | `paginateLinks` of the main query (main loops only) |
| `shop.*`, `product.*` | text / attr / html | Pursae Commerce widgets, §11.12 |

- `post.*` resolves to the nearest loop item, else the main post on a singular view, else empty.
- **text** slots replace the element's content with `html.EscapeString(value)`; **html** slots
  only take HTML from the trusted core generators named above; **attr** slots write only
  `href src srcset sizes width height alt datetime aria-current class target` (target `_blank _self _parent _top`), URLs through the same
  scheme check as §8.1, and keep the element's content.
- An unknown slot renders empty. Values come from what `buildSiteView` already computed
  (`v.Posts`, `v.Pagination`, `v.Post`, `v.PostNav`, `v.Comments`, `v.Menus`, archive title,
  search query); loops add one batched query each.
- Engine (`internal/webbuilder/dynamic.go`, package TA): compiled programs are cached in an LRU of
  256 keyed `postID:rev:sha256(html)[:16]`; a snapshot without placeholders is one static segment
  (no cost). Front order: fill → `filterContentTags` → the existing filters.

### 11.6 Sample data: DynContext

The editor shows real content, never invented samples. `GET preview-context?object=` and the boot
`dynamic` field carry a DynContext:

| Field | Type | Notes |
|-------|------|-------|
| `object` | string | the preview object it was built for |
| `site` | `{title, tagline, home, logo: DynImage \| null, year}` | |
| `locations` | `{slug, name}[]` | the active theme's menu locations |
| `menus` | `Record<location, DynMenuItem[]>` | Pursae Multi Language swapped |
| `view` | `{kind, archiveTitle, archiveDesc, search, total}` | `kind`: `single page front-page home archive search 404` |
| `post` | DynPost \| null | the singular post |
| `posts` | DynPost[] | the main query's posts, ≤ 24; on a singular view (which lists none) the recent published entries of its type with the current one, then recent posts when the type is another one, ≤ 24 per type |
| `pagination` | `{current, total}` | |
| `shop` | DynShop \| null | Pursae Commerce sample (§11.12); `null` while it is off. Optional for the editor (older servers omit it) |
| `languages` | `{slug, name, flag, url, current}[]` | Pursae Multi Language languages with the object's URL in each; `[]` while it is off |

DynPost: `{id, type, title, url, excerpt, contentHtml, date, dateText, modified, modifiedText,
author: {name, url, avatar, bio}, image: DynImage | null, terms: Record<tax, {name, url}[]>,
commentsCount, commentsUrl}`. `title` and `excerpt` are plain text; `contentHtml` is sanitized
with the UGC policy and shown read-only in an overlay; `date`/`modified` are RFC 3339 and
`dateText`/`modifiedText` use the site's `date_format` (the editor formats a custom
`dyn.format` itself). DynImage: `{url, width, height, alt}`. DynMenuItem:
`{id, label, url, current, children}`; `children` is always `[]` in v1 (top level only).

Preview objects (≤ 200 bytes):

| Object | Means |
|--------|-------|
| `post:<id>` | that entry (needs `read_post`) |
| `term:<tax>:<id>` | that term's archive |
| `author:<id>` | that author's archive |
| `type:<ptype>` | that post type's archive |
| `search:<q>` | search results for `q` (≤ 100 characters) |
| `front` | the site's front page (`/`) |
| `home` | the posts index (`is_home`) |
| `404` | the 404 view |
| `latest` | template-relative: single-post → newest post; single-page → static front page, else newest page; single → newest entry of the template's post type (its first `include/singular/<ptype>` rule); single-product → newest product; elsewhere `front` |

With Pursae Commerce on, `post:<product id>` resolves to the store's product view, `type:product`
to the shop and `term:product_cat:<id>` / `term:product_tag:<id>` to the product taxonomy
archives (labels "Product: X", "Shop", "Product category: X", "Product tag: X").

A malformed object answers **422** `wb_bad_preview`; one that names nothing answers **404**
`wb_not_found`.

### 11.7 Boot additions

`template` (TemplateBoot, `null` on ordinary posts):

| Field | Type | Notes |
|-------|------|-------|
| `type` | TemplateType | §11.2 |
| `conditions` | string[] | the stored rules |
| `priority` | number | |
| `starter` | StarterId | loaded when the doc is empty |
| `preview` | `{object, label, url}` | `label` like "Post: Hello world"; `url` the object's front URL with `?wb_template_preview=<id>` |
| `manageUrl` | string | Web Builder › Theme Builder |

`site` (SiteBoot, always present): `{title, tagline, home, logo: DynImage | null, locations,
menus, themeParts, postTypes: {slug, label, archive, hierarchical}[], taxonomies: {slug, label,
types}[]}`. `themeParts` is true while Hello Pursae is active (pages may then use
`layout: 'site'`); `postTypes` lists the viewable types (post, page, public custom types; never
`wb_template`), `archive` is the archive URL or `""`; `taxonomies` is `category` and
`post_tag`. With Pursae Commerce on, `postTypes` also has `product` (archive = the shop URL) and
`taxonomies` `product_cat` and `product_tag` (types `["product"]`).

For a template: `links.exit` = the Theme Builder screen, `links.preview` and `links.permalink` =
`template.preview.url`, `links.editPost` = `""`, `caps.publish` = `edit_theme_options` (true).
In template mode the editor shows a type badge, a "Display Conditions" dialog and a "Preview as"
picker; nav and footer sections are not pinned; page settings show only the title.

### 11.8 Template REST

All routes need `X-WB-Api` and `edit_theme_options`, except `preview-context` (`edit_posts`, and
`read_post` for a `post:<id>` object).

- `GET templates?type=` → TemplateSummary[] `{id, title, type, status, conditions, priority,
  author: {id, name}, modified, editUrl, previewUrl}`, newest first, trash excluded; `type`
  filters (unknown → 422 `wb_bad_template`).
- `POST templates` `{type, title, starter?, postType?}` → **201** `{id, editUrl}`: a draft with
  the type's default conditions and preview, `_wb_starter` set. `title` empty = the type label;
  `starter` empty = the type's default. `postType` is required for `single` (a public custom
  type, never `post` or `page`, which have their own types) and refused for other types. Bad
  type, starter or postType → 422 `wb_bad_template`.
- `PUT templates/{id}/conditions` `{conditions, priority}` → the normalized `{conditions,
  priority}`. Validates (§11.3; priority outside −100…100 → 422 `wb_bad_condition` with path
  `/priority`), writes the meta and calls `wbTemplatesChanged()`.
- `PUT templates/{id}/preview` `{object}` → `{preview: {object, label, url}, dynamic:
  DynContext}`; stores `_wb_preview`.
- `GET templates/{id}/conflicts` → TemplateConflict[] `{id, title, type, conditions, priority,
  editUrl}`: other published templates of the same location whose include rules can match a view
  this template's include rules match. A warning only; scores decide at request time.
- `GET preview-context?object=` → DynContext.
- Document routes (§3.1) accept a `wb_template` through `wbEditable`. Publishing a template needs
  `edit_theme_options`; its status is `publish` or `draft` (no pending path; `pending`/`private`
  → 422 `wb_bad_template`). Publishing, unpublishing, trashing a template and changing its
  conditions call `wbTemplatesChanged()`: the template index generation is bumped and Pursae Cache
  is purged completely.

Error codes added: `wb_bad_condition` (422), `wb_bad_template` (422), `wb_bad_preview` (422).

### 11.9 Front with Hello Pursae

While the plugin is on and the active (or previewed) theme is `hello-pursae`, `wbRenderPage`
builds the parts and renders the theme's own skeleton (`renderSiteTemplate`); `site.go` is not
changed. Other themes keep the canvas of §10.

- **Template index** (`webbuilder_templates.go`): published templates in memory, rebuilt by one
  query when the generation changes or after 5 minutes. `wbPickTemplate(c, location, q)` applies
  §11.3.
- **Main**: a singular post with its own builder doc → that snapshot, filled (`layout: canvas` →
  no header or footer part); else the picked main template, filled over the request's view; else
  none (the theme's default loop / content / 404).
- **Header / footer**: the picked template, else none (Hello Pursae's minimal header: logo or
  site title plus the `primary` menu; minimal footer).
- Each part is wrapped in `div.wb-part.wb-part-<header|main|footer>`; `<body>` gets `wb-site`
  when at least one part renders. Fonts are merged (deduplicated), features are the union; each
  part's CSS is `<style id="wb-css-<postID>">`. The base CSS and the `.wb-js` script are printed
  once in the head, the runtime `<script defer>` once before `</body>`. The runtime reads the
  dismiss key's post id from `el.closest('[data-wb-root]')` (several roots per page).
- `?wb_template_preview=<id>`: with `edit_theme_options` that template fills its location
  whatever the conditions; the response is never cached.
- **Cache**: pages with a part carry `wb-theme`; pages whose parts run a loop also carry
  `wb-loop`, purged when any public post is saved. Menu, logo and site title changes purge
  everything.
- **Admin bar**: "Edit Header", "Edit Footer", "Edit Template" for the parts in use.
- **Forms** inside a template post to `forms/<template id>/<section>`; a published `wb_template`
  is a valid form target. Without JavaScript the 303 goes back to the same-origin `Referer`, else
  home.
- Store views print `v.Post.Content` like Twenty Twenty-Five.

### 11.10 Theme switch

- **Plugin on**: Hello Pursae is installed if needed; when the active stylesheet is not
  `hello-pursae` it is stored in `web_builder_previous_theme`, the site switches to Hello Pursae
  and `web_builder_theme_notice` holds a one-shot notice ("Your site now uses Hello Pursae.
  Previous theme: X." with "Switch back").
- **Plugin off**: when the active theme is still `hello-pursae` and activation stored a previous
  theme, the site switches to it (if it is installed and not `hello-pursae`), else to the default
  theme; the option is cleared. If the user changed the theme in between, or the site was already
  on Hello Pursae when the plugin was turned on (nothing stored), nothing happens.
- Sites that had the plugin on before the Theme Builder are never switched silently: they get a
  dismissible "Switch to Hello Pursae" notice; dismissing it (or switching) sets
  `web_builder_theme_offer` to `true`, which activation also sets.
- The old builder's `golementor_previous_theme` is never read or written, and the upgrade code
  does not treat `hello-pursae` as an old builder theme.

### 11.11 Decisions on open points

Where the design left a choice, the contract fixed it as follows:

- The menu binding attribute is `data-wb-menu-loc`, not `data-wb-menu` (runtime clash, §11.5).
- Higher `_wb_priority` wins a tie; then the newer, then the higher id.
- Main templates pass a view gate per type (§11.2) before scoring.
- `404` is a top-level scope (score 45), not a singular one.
- `latest` and `front` were added to the preview objects; `home` is the posts index.
- `PUT templates/{id}/preview` answers `{preview, dynamic}` (the editor needs the new label and
  URL), not a bare DynContext.
- `POST templates` takes `postType` for the `single` type.
- DynPost carries `dateText`/`modifiedText` and `commentsUrl` besides the RFC 3339 dates.
- `SiteBoot.postTypes[].hierarchical` tells the conditions dialog where `child_of` applies.
- `loop` joins the accepted section kinds together with its validation (package TA); until then
  the Go `SectionKinds` list, and so validation, is unchanged.
- Commerce widgets are dynamic cells whose settings the server reads from the stored doc by
  `data-wb-item` (never from snapshot attributes), and whose markup is the store's own (the
  snapshot carries no store forms; the sanitizer would strip them anyway).
- A product's own builder doc is kept on its product view; every other store page's doc is not,
  except the Cart / Checkout page with its cart / checkout widget (§11.2).
- TS: the doc model additions (`DynSettings`, `CellType`, `LoopContent`, `PageLayout`) are
  declared in `host/contract.ts` until patch 0008 adds the same shapes to upstream `types.ts`;
  from then on they must stay identical.

### 11.12 Commerce and language widgets

Patch `0015-commerce.patch`. Dynamic widgets (§11.4) whose slot elements carry
`data-wb-item="<cell id>"`; the server reads that cell's `Item.dyn` from the **stored** doc
(`wbCellDefs`, cached per doc revision) and fills every slot with Pursae Commerce's own markup
(`internal/server/webbuilder_shop.go`). All of them render empty while Pursae Commerce (or, for
the switcher, Pursae Multi Language) is off. Widget panel groups: `site` (language switcher),
`shop` and `product` (any page, while Commerce is on: a product widget can show a chosen
product), `shop-archive` (templates only).

| Widget | Group | Slots | Settings (`Item.dyn`) |
|--------|-------|-------|-----------------------|
| `language-switcher` | site | `site.languages` | `dropdown`, `flags`, `names` (default true), `hideCurrent` |
| `shop-products` | shop | `shop.products` (`[products]`) | `source` (`recent featured sale best-selling top-rated ids`), `columns` 1…6 (4), `limit` 1…24 (8), `orderBy` (`date title price price-desc popularity rating rand menu_order`), `order`, `cats`, `tags`, `ids`, `paginate` |
| `shop-categories` | shop | `shop.categories` (`[product_categories]`) | `columns`, `limit`, `hideEmpty` (true), `parent` (`""` all, `"0"` top level, or an id), `cats` |
| `shop-add-to-cart` | shop | `product.add-to-cart-button`, `product.price` | `product`, `showPrice` |
| `shop-cart` | shop | `shop.cart-url`, `shop.cart-count`, `shop.cart-subtotal`, `shop.mini-cart` | `showTotal`, `dropdown` (mini cart in a `<details data-wb-menu>` panel, default true) |
| `shop-mini-cart` | shop | `shop.mini-cart` | |
| `shop-notices` | shop | `shop.notices` | |
| `shop-cart-page`, `shop-checkout-page` | shop | `shop.cart-page`, `shop.checkout-page` | rendered only on the Cart / Checkout store view (no-store responses); the fallback HTML is `[goocommerce_cart]` / `[goocommerce_checkout]` |
| `product-title` | product | `product.title`, `product.url` | `product`, `tag`, `link` |
| `product-images` | product | `product.gallery`, or `product.image` (+ `data-wb-size`) with `gallery: false` | `product`, `gallery`, `size` |
| `product-price`, `product-add-to-cart`, `product-rating`, `product-stock`, `product-meta`, `product-short-desc`, `product-content`, `product-tabs`, `product-related`, `product-upsells`, `product-sale-badge` | product | `product.price`, `.add-to-cart`, `.rating`, `.stock`, `.meta`, `.short-description`, `.description`, `.tabs`, `.related`, `.upsells`, `.on-sale` | `product` |
| `shop-archive-products`, `shop-archive-title`, `shop-archive-desc`, `shop-breadcrumb` | shop-archive | `shop.archive` (the store's own archive markup of the view, no second query), `shop.archive-title`, `shop.archive-desc`, `shop.breadcrumb` | `tag` (title) |

Slot modes: text `shop.archive-title product.title product.on-sale product.sku`; attr
`shop.cart-url shop.checkout-url product.url product.image`; every other `shop.*` / `product.*`
slot and `site.languages` is html (generated by the store or the switcher, localized with the
store's strings).

- **Product resolution** (`product.*`): `dyn.product` (an id, mapped to its translation in the
  request language; only a published, non-variation product the visitor may see, Memberships
  included), else the product being viewed (store product view), else empty.
- **Ids** (`cats`, `tags`, `ids`): `^[1-9][0-9]{0,18}(,[1-9][0-9]{0,18}){0,23}$` (`SHOP_IDS_PATTERN`),
  at most 24; category and tag ids are mapped to the request language.
- **Single product views**: the main part (template or the product's own doc) is wrapped in
  `div.woocommerce.wb-shop > div#product-<id>.<product classes>` (gallery, tabs and variation
  scripts look for it); notices the store body already took are printed before it unless
  `shop-notices` shows them. On archive views `shop.archive` carries them.
- **Assets**: a part with a store slot other than the cart icon's (`shop.cart-url`,
  `shop.checkout-url`, `shop.cart-count`, `shop.cart-subtotal`) prints the store's stylesheets and
  scripts (canvas: through `wp_head`/`wp_footer`; Hello Pursae: added after the parts are filled).
- **Cache**: parts that show catalog data add the cache tag `goo:shop` (purged on product saves).
  Cart slots render the visitor's cart: cart holders bypass Pursae Cache and `cart-fragments.js`
  refreshes `[data-goo-cart-count]` / `[data-goo-cart-subtotal]` on cached pages (fragments
  `span.goo-cart-count`, `span.goo-cart-subtotal`).
- **DynShop** (`DynContext.shop`): `{product: DynProduct | null, products: DynProduct[] (≤ 24
  newest catalog products), categories: {id, name, url, count, parent, image}[] (≤ 100), cart:
  {count, subtotalHtml}, shopUrl, cartUrl, checkoutUrl}`. DynProduct: `{id, type, title, url, sku,
  priceHtml, onSale, inStock, stockHtml, rating, ratingCount, shortHtml, contentHtml, image,
  gallery, categories: number[], addToCartText}`; `shortHtml`, `contentHtml` and `gallery` only
  for the preview product.
