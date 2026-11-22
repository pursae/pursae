---
title: Pursae Web Builder
description: The built-in visual page builder — how editing and publishing work, the Theme Builder and Hello Pursae, where the data lives, the web-builder/v2 REST API, how the editor is built, and what it does not do yet.
weight: 890
section: built-in-plugins
---

Pursae Web Builder is a section-based visual page builder that ships inside the binary as a
built-in plugin (slug `golementor`, option `native_golementor`). The editor is a React app built
from [`github.com/pursae/Builder`](https://github.com/pursae/Builder); visitors never load React:
a published page is a static HTML/CSS snapshot with a small optional script. With the
**Theme Builder** it also designs the site's header, footer and content templates, shown by the
bundled **Hello Pursae** theme ([user guide](web-builder-theme-builder.md)). The field-by-field
specification is the [integration contract](web-builder-contract.md); this page is the overview.

## For site owners

### Switching it on

Like Classic Editor and Pursae Cache it is listed on Plugins and is **off on a fresh install**.
Plugins › Pursae Web Builder › **Activate** turns it on and switches the site to **Hello Pursae**,
the blank theme the Theme Builder designs on; the theme it leaves is remembered
(`web_builder_previous_theme`) and a notice on the Dashboard, Plugins and Themes screens offers
**Switch back**. **Deactivate** puts that theme back (or the default theme when it is gone) while
the site is still on Hello Pursae; if you picked another theme in between, it stays (leaving Hello Pursae
forgets the remembered theme, so choosing Hello Pursae again later is your own choice and stays too). Sites that had
the plugin on before the Theme Builder existed are not switched: they get a one-time **Switch to
Hello Pursae** notice instead ([upgrading](getting-started/upgrading.md#release-notes)). There is
no setup wizard. While the plugin is off there are no menu entries, "Edit with" links, routes,
assets or front-end output, and built pages show their readable fallback inside the theme.

**Web Builder › Settings** (`manage_options`) picks the post types the builder edits — pages and
posts by default; Theme Builder templates are always editable by users with `edit_theme_options` —
and lists the pages whose published markup came from an older renderer
("Regenerate pages"; each one is refreshed by opening it in the builder and publishing again).

### Editing flow

1. Open a page with **Edit with Pursae Web Builder**: the button in the block editor's toolbar or
   under the classic editor's title (a new or changed post is saved first), the row action on the
   Pages/Posts list, **Add New** followed by that button, or the toolbar node on the site. A built
   page is marked "Pursae Web Builder" in the list. The editor URL is
   `/wp-admin/post?post={id}&action=web-builder`; it needs `edit_post`, refuses trashed posts and
   takes the WordPress post lock (another user's live lock is shown, and can be taken over).
2. Build the page from sections — navigation, hero, banner, features, media, steps, quote, FAQ,
   call to action, footer, text, image, form, layout and custom — or a saved section from the
   library, and switch between desktop, tablet and mobile views. Images come from the Media
   Library (uploads go through `/wp/v2/media`); Google Fonts are downloaded once and served from
   the site.
3. Changes **autosave** every few seconds as your own draft. The live page is untouched; reopening
   the editor offers the draft when it is newer than what is published.
4. **Publish** renders the page in the browser, sends the document plus the snapshot to the
   server, and the server validates and sanitizes both. Users who may not publish get **Pending
   Review** instead. If someone else published in the meantime the editor shows a conflict instead
   of overwriting it.
5. Every publish is also a core revision, and the server keeps the builder document of each one
   (REST `revisions`; restoring one writes it as your draft to review and publish). The editor
   has no revisions panel yet.
6. **Preview** (`?preview=true`) shows the last published snapshot; when your draft is newer a notice
   says so.

Editing a built page somewhere else (block or classic editor, Quick Edit, REST) **detaches** it: the
site then shows that edited content inside the theme, and the builder warns "Edited outside the
builder" with two choices — publish again from the builder (discarding the outside edits) or leave
the page to the other editor.

The editor UI is English; Turkish is included, picked from the user's language. Other languages
follow later.

### Site settings

While the builder is on, the Customizer is hidden (Appearance menu, toolbar, Dashboard panel;
`/wp-admin/customize` and every "Customize" button lead to Web Builder › Theme Builder). Its
settings are managed in the editor: open the **Pursae Web Builder** menu in the top bar and choose
**Site settings**. The dialog edits the site title and tagline, the logo (when the theme supports
one), the site icon, what the homepage shows (latest posts or a static page and the posts page) and
Additional CSS. Values are validated and stored exactly like the Customizer does, so switching the
builder off shows the same settings there. Needs `edit_theme_options`. Menus and widgets are still
managed under Appearance. REST: [contract §3.6](web-builder-contract.md).

### Theme Builder

**Web Builder › Theme Builder** (`edit_theme_options`) lists the site's templates: Header, Footer,
Single Post, Single Page, Single (a custom post type), Front Page, Archive, Search Results and 404
Page. A template is a builder document like a page, designed with **dynamic widgets** (site logo,
title, menu, search form, post title, content, featured image, post info, terms, author box,
comments, archive title, pagination…) and **post loops** (a card designed once and repeated for
the request's posts, a custom query or related posts). The editor draws them with the site's real
content, picked with **Preview as**. **Display conditions** (include/exclude rules from "Entire
Site" down to one entry, term or author) choose one template per place and request; the most
specific rule wins, then the priority. Published = active, draft = disabled.

While Hello Pursae is the active theme every front-end page is assembled from three parts: the
header template, the main content (the page's own builder design, else the matching main
template, else the theme's default) and the footer template; a place without a template shows
Hello Pursae's minimal header, content or footer. A builder page chooses **Header & footer: Site
templates** or **This page only (canvas)** in its page settings. With any other theme templates
are not used and pages keep the canvas below.

The [Theme Builder guide](web-builder-theme-builder.md) shows how to design each template and
gives condition examples; the rules are in [contract §11](web-builder-contract.md#11-theme-builder).

### What visitors get

With Hello Pursae, a page is the theme's document with the header, main and footer parts above;
each part's CSS is inlined as its own `<style>`, fonts and runtime features are merged, and the
runtime loads once. With any other theme a published page is a canvas document: no theme header,
footer or stylesheet. Either way SEO, analytics, cookie-consent and plugin heads are kept. The
page CSS is inlined up to 24 KiB (a cached file above that), fonts are local `@font-face`, images
go through the core image filters (sizes, `srcset`, lazy loading). The runtime (`runtime.<hash>.js`, under 8 KB gzipped) is loaded only when
the page uses menus, FAQ/accordions, tabs, dismissible bars, forms, motion or embeds, and the
content is readable with JavaScript off. Builder pages are cached by Pursae Cache and purged on
publish; publishing, disabling or trashing a template, or changing its conditions, purges the whole
cache, and pages with post loops are purged whenever a public post is saved.

Dynamic widgets and loops are placeholders in the published snapshot; the server fills them on each
request from the data the page already computed (escaped text, HTML only from the core's own
generators, URL attributes scheme-checked), so a published template never needs republishing when
posts, menus or the logo change.

Forms post to the site itself (`web-builder/v2/forms/{page}/{section}`): the fields come from the
stored page, not the request; a honeypot, a minimum fill time, a per-IP rate limit and the Pursae
Contact Form captcha (when one is configured) filter spam, and the message goes to the site's admin
e-mail through the core mailer.

Everywhere else — REST `content.rendered`, Pursae Headless, feeds, search, sitemaps, exports, and
the theme when the plugin is off or the page is detached — the page is the readable fallback HTML
the server generates from the document into `posts.content`.

### Popups

A template of type `popup` (Appearance › Theme Builder › Add New › Popup) is a modal dialog. Its display
conditions pick the pages exactly like any template, but several popups can match one page (up to five
are printed, best match first). **Popup Settings** in the template's row actions (also
`GET/PUT web-builder/v2/templates/{id}/popup`) set:

| Setting | Values |
|---------|--------|
| Opens | page load (after a delay), scroll depth, exit intent (the pointer leaves through the top; touch screens use the delay), click on `#id` / `.class` |
| Shown | every page view, once per session, once per visitor, again after N days; raise **Version** to show it again to everyone |
| Look | position (center, top, bottom), width, close on backdrop click (the close button and Esc always work) |
| Cookie banner | wait until the visitor has answered the Pursae Cookie banner (while Pursae Cookie is on) |

The markup is the same for every visitor, so Pursae Cache keeps serving the page: the dialog is
printed hidden before `</body>` with one inline script, and the visitor's choice is kept in the browser
(`localStorage` / `sessionStorage`, key `wb-popup:<id>`), not in a cookie. The dialog traps focus, closes
on Esc and gives focus back. A click trigger always opens the popup. While previewing a popup
(`?wb_template_preview=<id>`) only that popup shows, at once.

### Design tokens

Settings › Web Builder › Design tokens (and `tokens` in `web-builder/v2/site-settings`) define five
colours (`primary`, `secondary`, `accent`, `text`, `background`) and two fonts (`heading`, `body`) as CSS
custom properties on `:root` (`--wb-color-primary`, `--wb-font-heading`, …) in one
`<style id="wb-tokens">` of every page with builder parts. Published snapshots are static, so a snapshot
or a part's CSS that reads `var(--wb-color-primary)` follows a change at once, without publishing the
pages again. Values are validated (hex, `rgb()/hsl()` functions, colour keywords; plain font-family
lists); anything else is dropped.

### Site style

The Site Editor's global styles: one brand colour, font pair, heading and body fonts and corner style for
the whole site. In the editor, **Items › Globals › Site style**:

- **Save as site style** (administrators, `edit_theme_options`) stores this design's colours, fonts and
  corners as the site style; the design then follows it.
- **Follow the site style** switches a page or template to the site style (or back to its own look).
  New pages and templates start with the site style once one is saved. Changing a colour, font or
  corner of a design that follows the style gives it its own look again.

Published pages are static HTML, so a design that follows the style shows a new style once it is
published again: after saving, the editor says how many pages and templates are waiting, and
Settings › Web Builder › Regenerate pages lists them and publishes them all with one button. The
design tokens (below) stay the way to reach the site's colours and fonts from custom CSS and code
sections without publishing again.

### Menus and custom fields

A `data-wb-menu-loc` list repeats its card per menu item, depth first: each sub-menu item follows its
parent (three levels). The link of a sub-item gets the class `wb-menu-d1` / `wb-menu-d2`, a parent's
link `has-children`, so a card template can indent or fold them (`wb-tokens` indents the two levels).

`data-wb-slot="post.field"` with `data-wb-field="<field name>"` prints a Pursae Custom Fields value of the
post (text, escaped; lists are joined with a comma); `post.field-url` sets `href` (or `src` on an
`<img>`) from a URL, link or image field. Hidden fields (`_name`), unset fields, password-protected
posts and a site with Pursae Custom Fields off print nothing.

### Regenerating pages

Settings › Web Builder › Regenerate pages lists how many pages were published by an older renderer (or
imported from a WordPress export without markup) and publishes them all again with one button: the
editor's renderer runs in a frame of that screen, page after page, keeping each page's status, with a
progress bar and the pages that failed. The work list is `GET web-builder/v2/outdated?after=<id>&limit=<n>`
(no cap). Only the browser can draw a snapshot, so the tab has to stay open until it is done.

### Limits

Not in the builder (yet):

- **AI** — the upstream AI assistant, AI images and site cloning are stubbed out of the build; no
  AI request leaves the site. The design for a server-side proxy is in the contract (§9).
- **Drop-down menus**: a menu bound to a menu location lists its sub-menu items (three levels, indented
  by the classes below), but not as fold-out drop-downs; archives of Pursae Custom Fields taxonomies are
  not routed, so conditions on them match nothing.
- **Popup designer UI in the editor**: popups are designed like any template; their trigger and
  frequency are set under Appearance › Theme Builder › Popup Settings (no panel in the editor yet), and
  they show only with Hello Pursae.
- **Custom field widget in the editor**: the `post.field` slot works in snapshots and the code widget;
  the editor has no widget that inserts it yet.
- **Form integrations** (mailing lists, webhooks, CRMs, auto-replies) and **payments**. Forms keep their
  designed fields (no field editor); recipients, messages and the redirect are set per site under
  Settings › Web Builder (or per form through the `formTo`, `formCc`, `formSuccess`, `formError`,
  `formRedirect` section content keys), and submissions are stored and listed there.
- Pursae Commerce: product templates and widgets exist (Theme Builder), but not a product loop
  card designed widget by widget (the products grid uses the store's own cards), filters or the
  account page as a widget.
- **Global classes** and a colour picker in the editor that writes design tokens (popups, design
  kits and the site style do exist, see Popups and Site style above and [code sections](web-builder-code-sections.md)):
  the tokens themselves exist (below), but the editor's own colour and font fields still write fixed
  values; use `var(--wb-color-primary)` in custom CSS or an HTML section to follow a token.
- A setup wizard and a template library beyond saved sections. (The builder menu has **Revisions** — restore loads a published version as your draft — **Schedule publish**, and in a popup template **Popup settings**; builder revisions are pruned to the newest 50 per page, Settings › Web Builder › Revisions to keep.)

These come back as later features. The previous builder and its data were removed in the release
that introduced this one ([upgrading](getting-started/upgrading.md)).

### Building pages with an AI agent

An AI agent connected through [Pursae MCP](operations/ai-mcp.md#building-sites) (Tools › AI Access) builds
the same pages you edit here: it picks sections and designs from the library, writes the copy, publishes
— the server draws the page with the editor's own renderer — and gives you the **Edit with Pursae Web
Builder** link. It can also turn an HTML mock-up into free-form sections. Everything it makes stays
editable by drag and drop, with history and revisions like your own changes; a page it saved as a draft
waits in the editor until you (or it) publish.

## For developers

### Code map

| Path | What |
|------|------|
| `internal/webbuilder/` | server-independent pieces: contract types (`contract.go`), doc validation, HTML and CSS sanitizers, fallback HTML, forms, fonts; Theme Builder engine: `dynamic.go` (placeholder fill), `conditions.go` (display conditions) |
| `internal/webbuilder/code.go`, `codeimport.go`, `internal/server/webbuilder_kits.go`, `webbuilder_codeimport.go` | code sections (a design kept as its own markup) and design kits — see [Code sections and design kits](web-builder-code-sections.md) |
| `internal/store/webbuilder.go`, `webbuilder_templates.go` | storage (docs, drafts, revisions, library; templates and their index) |
| `internal/server/webbuilder_*.go` | plugin switch and settings, admin entry points, editor screen, assets, REST, front-end render, forms, hooks (drift, copies, export, cache), redirects of old URLs, one-time upgrade and cleanup |
| `internal/server/webbuilder_templates*.go`, `webbuilder_dynamic_ctx.go`, `webbuilder_theme_front.go`, `webbuilder_slots.go`, `webbuilder_shop.go` | Theme Builder: template index and pick, template REST, editor sample data, front assembly, slot resolver and loops, Commerce and language-switcher slots |
| `internal/server/theme_hellopursae.go`, `web/themes/hello-pursae/` | the Hello Pursae theme |
| `builder/` | Node workspace: `upstream/` (git submodule, pinned by `UPSTREAM_SHA`), `patches/`, `host/` (CMS adapter), `vite/`, `scripts/`, `test/` |
| `web/web-builder/` | the committed build output, embedded in the binary |

### Data model

The page **document** (`doc`, JSON) is the source of truth; everything the browser renders from it
is untrusted. Migration `0027_web_builder.sql` adds:

- `web_builder_docs` — one row per built post: `doc`, `page` settings, the sanitized snapshot
  (`html`, `css`, `fonts`, `features`), `renderer`, the publish counter `rev`, `content_hash` of the
  fallback written to `posts.content`, and `detached`.
- `web_builder_drafts` — per-user autosaves (`post_id`, `user_id`, `base_rev`).
- `web_builder_revisions` — the doc of each publish, keyed by the core `post_revisions` row it was
  created with.
- `web_builder_library` — saved sections.

Theme Builder templates are posts of the non-public type `wb_template` (migration
`0029_web_builder_templates.sql`) with the same docs, drafts and revisions; their type, display
conditions, priority, preview object and starter are post meta (`_wb_template_type`,
`_wb_conditions`, `_wb_priority`, `_wb_preview`, `_wb_starter`). Every template path checks
`edit_theme_options` explicitly — for a non-page type `edit_post` alone would let authors in.

The post itself only gets the marker meta `_web_builder = "1"`. Page settings map to the post:
title, slug, excerpt, and Pursae SEO description and noindex. WXR export carries the doc
(`_web_builder_doc`, `_web_builder_page`), import validates it again; Pursae Multi Language's
"Duplicate content" copies doc and snapshot to the translation; templates are a translated type,
and each language shows the translation of the winning template. WXR carries template meta, and
the ids in conditions are remapped on import (rules whose target was not imported are dropped and
logged).

### Publish = static snapshot

The editor calls the upstream live renderer (`renderStatic`, from `builder/patches/0002`) and
compiles the Tailwind classes it used. The server then, in one transaction: checks the API version
header, `edit_post`, the post lock and the revision (`baseRev`, else 409); validates the doc
(kinds, sizes, depth, URL schemes); strips html sections and custom CSS for users without
`unfiltered_html`; sanitizes the snapshot HTML (allowlist, one `div.wb-page` root, `data-wb-*`
attributes, restricted iframes) and the CSS (no `@import`, `expression()`, unsafe `url()`); writes
the fallback HTML to `posts.content` (a core revision); upserts the docs row; and purges the page
cache. The response lists what was stripped.

### REST: `web-builder/v2`

Cookie + `X-WP-Nonce` (or an Application Password), plus `X-WB-Api: 1` on every editor request
(a mismatch answers 409 so a stale editor reloads). Routes exist only while the plugin is on.

| Route | Purpose |
|-------|---------|
| `GET documents/{id}` | doc, page settings, your newer draft, lock, detached flag |
| `PUT` / `DELETE documents/{id}/autosave` | your draft |
| `POST documents/{id}/publish` | publish (validate, sanitize, store) |
| `POST documents/{id}/lock` | take or refresh the edit lock |
| `GET documents/{id}/revisions[/{rid}]`, `POST …/{rid}/restore` | revisions |
| `GET` / `POST library`, `DELETE library/{id}` | saved sections |
| `GET` / `PUT prefs` | the user's editor preferences |
| `GET fonts/css2` | Google Fonts css2 query answered with local files |
| `POST forms/{id}/{section}` | public form endpoint |
| `GET templates?type=`, `POST templates` | list templates, create a draft template |
| `PUT templates/{id}/conditions` | display conditions and priority |
| `PUT templates/{id}/preview` | the template's preview object, answers the new sample data |
| `GET templates/{id}/conflicts` | other published templates that can show on the same views |
| `GET navigator` | the page switcher list: templates and editable pages/posts with their builder URLs |
| `GET preview-context?object=` | sample data (DynContext) for the editor |

Payloads, status codes and limits: [contract §3](web-builder-contract.md#3-rest-namespace-web-builderv2)
and, for templates, [§11.8](web-builder-contract.md#118-template-rest). The Theme Builder raised
`APIVersion` to 2 (an editor tab opened before the upgrade reloads); `RendererVersion` stayed 1,
because the placeholders only add attributes.

### Building the editor

The dist in `web/web-builder/` is committed, so `go build` and the Docker image need no Node.
Rebuild it after changing `builder/`:

```sh
make builder          # submodule at UPSTREAM_SHA → patched copy in builder/.work → vite build → manifest, BUILD.json, fixtures, audit
make builder-watch    # rebuild the editor on change (with `make dev`, which reads web/ live)
make builder-test     # vitest (host), UI string lint, audit of the dist
```

Node 22+ (`builder/.nvmrc`). Commit `web/web-builder/` together with the source change.

**Patches.** The upstream code is never edited in place. `builder/patches/*.patch` are applied in
name order with `git apply --3way` to a copy of the submodule; a patch that no longer applies fails
the build loudly.

- `0001-host-seam` — the `BuilderHost` interface: load, autosave, publish, media, fonts, exit and
  site URLs go through the host (`builder/host/cms-host.ts`); upstream still runs standalone.
- `0002-live-render` — `src/builder/live/`: `renderStatic(doc)` and the vanilla `data-wb-*`
  runtime.
- `0004-i18n` — `t()` with English sources and the Turkish catalog
  ([contract §2.1](web-builder-contract.md#21-editor-ui-language)).
- `0005-parity`, `0006-no-remote-fonts`, `0007-page-title` — fixes found by the parity suite
  (`make builder-parity`), no remote font stylesheet in the editor, the page title in the top bar.
- `0008-dynamic` — dynamic widgets, the loop section, menu/logo binding, starters, the sample
  `DynContext` seam and placeholder output in live mode.
- `0009-template-mode` — template mode driven by the host (`BuilderHost.template()`, top-bar
  controls, `beforePublish`, the page `layout` setting while Hello Pursae is active). The Theme
  Builder dialogs themselves (Display Conditions, Preview as) live in `builder/host/`.

(`0003`, the AI seam, is reserved for when AI comes back.) Change a patch by editing the patched
copy, regenerating the diff against the pinned upstream and rebuilding.

**Bumping upstream.** `make builder-bump SHA=<commit>` moves the submodule and
`builder/UPSTREAM_SHA` and rebuilds; fix any patch that stops applying, then commit the pin, the
patches and `web/web-builder/` together. `BUILD.json` records the upstream SHA, the patch hashes
and the API and renderer versions; a Go test checks it against the repository. An incompatible
change to the static markup or the `data-wb-*` attributes bumps `RendererVersion` (pages then show
up under Regenerate pages; additive attributes do not); an incompatible payload change bumps
`APIVersion`.

`builder/test/audit.mjs` fails the build when an AI provider URL or key header shows up in the
bundles, and enforces the size budgets.

### License

The editor is built from `github.com/pursae/Builder`, which has **no LICENSE file yet**. Before the
builder is merged to `main` and shipped, that repository needs a GPL-compatible license (the repo
owner adds it); until then the integration lives on the `web-builder` branch.
