# Theme packages

A Pursae theme is a directory (or a zip of that directory) with no Go code in it — but it is not
inert: its templates and `scripts` put JavaScript on every page of the site (see
[Trust](#trust)):

```
theme.json          metadata and everything a Go theme would register in code
templates/*.html    Go html/template files (the template hierarchy, see below)
assets/**           style.css, scripts, fonts, images, screenshot.png — the theme's web root
readme.md           optional, shown on the theme's marketplace page
```

Installed themes live in the themes directory (`PURSAE_THEMES_DIR`, default `<data>/themes`,
the same directory the Theme File Editor writes its overrides to), one sub-directory per
slug: `<data>/themes/<slug>/theme.json`. The server loads every such directory at start-up
and right after Appearance › Themes › Add Theme › Upload Theme installs one; a package that
does not validate is logged and skipped. A package whose slug matches a bundled theme
(`twentytwentyfive`, …) is rejected, and the directory name must equal the slug.
Appearance › Themes › Theme Details › Delete removes the directory and the registration
(capability `delete_themes`; the active theme cannot be deleted).

`assets/` is served at the theme URI, `/wp-content/themes/<slug>/`, so `assets/style.css`
is `/wp-content/themes/<slug>/style.css`. Every path in theme.json is theme-relative and
must point under `assets/`. The Theme File Editor edits a package's files in place (there
is no "Restore original": the package's files are the originals).

## theme.json

```json
{
  "slug": "starter",
  "name": "Starter",
  "version": "1.0.0",
  "description": "A minimal starting point.",
  "author": "Gopher Co.",
  "author_url": "https://example.com",
  "theme_url": "https://example.com/starter",
  "tags": ["blog", "one-column"],
  "requires_core": "0.1.0",
  "screenshot": "assets/screenshot.png",

  "widget_areas": [
    {"id": "sidebar-1", "name": "Sidebar", "description": "Appears on the right."}
  ],
  "menu_locations": [
    {"id": "primary", "name": "Primary Menu"}
  ],

  "styles": [
    {"handle": "starter-style", "src": "assets/style.css", "deps": [], "media": "all"}
  ],
  "scripts": [
    {"handle": "starter-nav", "src": "assets/js/navigation.js", "deps": [], "in_footer": true}
  ],
  "editor_styles": ["assets/editor-style.css"],
  "preload_fonts": ["assets/fonts/inter.woff2"],
  "block_styles": true,

  "palette": [
    {"name": "Ink", "slug": "ink", "color": "#1d2327"}
  ],
  "supports": {
    "post-thumbnails": true,
    "custom-logo": true,
    "title-tag": true,
    "html5": ["search-form", "comment-form", "gallery", "caption"],
    "responsive-embeds": true,
    "align-wide": false
  },

  "customizer": {
    "sections": [
      {
        "id": "starter_colors", "title": "Colors", "priority": 40,
        "settings": [
          {"id": "accent", "label": "Accent color", "type": "color", "default": "#2271b1", "transport": "postMessage", "css_var": "--accent"},
          {"id": "show_sidebar", "label": "Show sidebar", "type": "checkbox", "default": true, "body_class": "has-sidebar"},
          {"id": "layout", "label": "Layout", "type": "select", "default": "list", "body_class": "layout-{value}",
           "choices": [{"value": "list", "label": "List"}, {"value": "grid", "label": "Grid"}]}
        ]
      }
    ]
  }
}
```

Rules:

- `slug`: `^[a-z0-9]+(-[a-z0-9]+)*$`, 2–64 chars; `version`: semver; `name` required.
  `requires_core` (semver) must not be newer than the running Pursae. `author_url`
  and `theme_url` are http(s) URLs. `screenshot` defaults to `assets/screenshot.png`.
- `styles`/`scripts`: `handle` is `^[a-z0-9]+([_-][a-z0-9]+)*$` and unique within its
  list; `src` is a theme-relative path under `assets/` (served under the theme URI with
  `?ver=<version>`); a style may also be an absolute http(s) URL, a script may not (remote
  scripts are refused: a script gets the page's CSP nonce). `deps` may reference core handles
  (`wp-block-library`, `jquery`, …) or the theme's own handles; the theme's assets are
  printed after core's, ordered so that each follows its own dependencies like
  `wp_enqueue_*` (a cycle is an error), in declared order otherwise. Styles print in
  `wp_head`, scripts in `wp_head` or, with `in_footer`, in `wp_footer`. Every referenced
  theme file must exist in the package. `editor_styles` are `.css` files, `preload_fonts`
  `.woff2` files, both under `assets/`. Each `preload_fonts` file gets a
  `<link rel="preload" as="font">` in `wp_head` at its served URL (the theme URI plus the path
  without `assets/`); Pursae Cache's "Preload Fonts" does not add it a second time.
- `block_styles`: `true`, `false` (default) or an array of core block names without `core/`
  (`^[a-z0-9]+(-[a-z0-9]+)*$`). When on, the theme sets `Theme.BlockStyles` like Twenty
  Twenty-Five: `wp_head` drops the block library stylesheet and the classic theme/global
  styles, and each page inlines the `style.min.css` of the core blocks it uses, minus the listed
  blocks (their styles are the theme's own).
- Browser caching: assets requested with `?ver=<version>` are served with
  `Cache-Control: public, max-age=31536000, immutable` (an update installs a new version). A file
  saved in the Theme File Editor keeps the version, so it is listed in `.edited-files` in the
  package directory and served with `no-cache` until the package is updated or reinstalled;
  unversioned requests (fonts and images from `url()`) are `no-cache` too.
- Pursae Cache "Defer JavaScript" with an empty handle list defers the active package's
  `scripts` along with the core's safe defaults, under the usual exclusions (scripts in the
  body only, so use `in_footer`; not when an inline `<handle>-js-after` script follows).
- `widget_areas[].id` and `menu_locations[].id` are handles (same pattern), unique; `name`
  is required. Widget areas use the `<section id class="widget …">` / `<h2 class="widget-title">`
  markup of the bundled themes.
- `page_templates`: an object `{"page-projects": "Projects"}` of page templates, listed in the
  Page Attributes "Template" select of both editors, Quick Edit and Bulk Edit, and accepted by
  the REST `template` field (`get_page_templates()`). Each key is a handle that is not a
  hierarchy template (`index`, `page`, `front-page`, `singular`) and `templates/<key>.html`
  must exist; the label is one line of at most 80 characters without markup.
- `palette`: `name`, `slug` (handle) and a hex `color` each; it becomes the
  `editor-color-palette` support.
- `customizer.sections`: `id` (handle, unique), `title` required, sorted by `priority`.
  `settings[].id` is a handle, unique across the theme (`custom_logo` and `custom_css` are
  core's). `type`: `color`, `text`, `textarea`, `number`, `checkbox`, `select` (with
  `choices`, an ordered array of `{"value", "label"}` or an object `{"value": "Label"}`
  sorted by value), `image` (an attachment chosen from the media library). `default` must
  match the type (a boolean, an integer within `min`/`max` — defaults 0 and 1000000 —, a hex
  color, one of the choices, a string, an attachment id). A setting with `css_var`
  (`--name`) is printed as a CSS custom property on `:root` (checkboxes as `1`/`0`,
  numbers as plain numbers); one with `body_class` adds that class when truthy, or, when
  it contains `{value}`, the class with the value filled in (`layout-grid`).
  `transport: "postMessage"` previews live without a reload (not for `image`); the default
  is `refresh`. Values are stored as theme mods of the theme, exactly like a Go theme's.
- `supports` is reported to the block editor through the REST themes endpoint
  (`theme_supports`, with `editor-color-palette` from `palette` and `editor-styles` from
  `editor_styles`); `custom-logo` is on unless set, so Site Identity offers a logo. The
  bundled defaults (`title-tag`, `post-thumbnails`, `html5`, `responsive-embeds`,
  `wp-block-styles`) apply unless overridden.

## Templates

The templates directory follows the WordPress template hierarchy with Go templates: every
`templates/**/*.html` file is a template named by its path without the extension. For the
main query the first existing one of these is rendered:

| request      | candidates                                              |
|--------------|---------------------------------------------------------|
| front page   | `front-page`, `page`, `singular`, `index`               |
| blog         | `home`, `index`                                         |
| post         | `single-<format>`, `single`, `singular`, `index`        |
| page         | its template (below), `page`, `singular`, `index`       |
| category     | `category-<slug>`, `category`, `archive`, `index`       |
| tag          | `tag-<slug>`, `tag`, `archive`, `index`                 |
| author, date | `author` / `date`, `archive`, `index`                   |
| search       | `search`, `index`                                       |
| 404          | `404`, `index`                                          |

A page whose `_wp_page_template` is set (from the Template select of the editors, Quick Edit or
Bulk Edit, the REST `template` field, or demo content, see below) tries that template first,
also on the front page (`page-wide` or `page-wide.html` renders `templates/page-wide.html`).
A stored template the theme does not list in `page_templates` is kept: the editors show it as
an extra option and saving does not reset it to the default.
Only `index.html` is required. Parts are ordinary templates included with
`{{template "header" .}}`, `{{template "content" .}}` and so on (`header`, `footer`,
`sidebar`, `comments`, `searchform`, `content*`, … by convention).

The data is the same `siteView` the bundled themes receive: `.DocumentTitle`, `.Head`
(`wp_head`), `.Footer` (`wp_footer`), `.BodyOpen`, `.BodyClass` (`body_class`),
`.AdminBar`, `.SiteName`, `.Description`, `.HomeURL`, `.CustomLogo`, `.TitleIsH1`,
`.Template`, `.Q` (the main query), `.Posts` / `.Post` (each with `.ID`, `.Title`,
`.Permalink`, `.Content`, `.Excerpt`, `.ShowExcerpt`, `.Class` (`post_class`), `.Thumbnail`,
`.AuthorName`, `.AuthorURL`, `.AuthorAvatar` (avatar URL), `.DateText`, `.DateISO`, `.Categories`, `.Tags`,
`.CommentCount`, `.CommentsURL`, `.EntryMeta`, `.EditLink`, `.Biography`, …),
`.Pagination`, `.PostNav`, `.Comments` (`.Title`, `.List`, `.Count`, `.Open`, `.Form`),
`.ArchiveTitle`, `.ArchiveDesc`, `.SearchQuery`, `.TotalPosts`, `.PrivacyLink`,
`.CanPublish`, `.IsHomeHeader`, `.PostsPageTitle`, `.Areas` and `.Menus`. Package themes
also get `.Data.Settings` (every Customizer setting, typed, with defaults applied; image
settings hold the image URL) and `.Data.Nav` (rendered menus by location).

Functions, on top of the generic ones (`date`, `plural`, `num`, `dict`, `safe`,
`settingStr`, …; the `setting*` functions read only public settings such as `blogname`,
`date_format` or `page_on_front` and the active theme's mods, any other key reads as empty):
`{{nav_menu . "primary"}}` and `{{has_nav_menu . "primary"}}`
(`wp_nav_menu` / `has_nav_menu`), `{{dynamic_sidebar . "sidebar-1"}}` and
`{{is_active_sidebar . "sidebar-1"}}`, and `{{theme_mod . "accent"}}` (`get_theme_mod`
with the declared default). Package themes get `singular` or `hfeed` on the body like the
bundled ones. See `examples/themes/starter` for a complete, minimal theme and
`web/themes/twentytwentyfive/templates` for a bundled one.

## Packaging

`zip -r starter.zip theme.json templates assets readme.md` from inside the theme directory
(the files sit at the zip root, not under a folder; a single top-level folder is also
accepted and stripped; `tools/build-themes.sh` does this for the examples and for any
`library/themes/<slug>` given to it). Limits: 32 MiB
per package, 4 MiB per file, 64 MiB extracted, 2000 files, only these extensions:
`.json .html .css .js .map .svg .png .jpg .jpeg .gif .webp .avif .ico .woff .woff2 .ttf
.otf .md .txt`. The templates must parse before anything is replaced. Uploading needs the
`install_themes` capability; a package whose slug is already installed replaces it only
when its version is higher (theme mods and widgets are kept). Themes are not signed; the Hub
and the installer verify the SHA-256 of the zip.

## Trust

A package carries no Go code, but it is code all the same: its templates are free to print
`<script>` elements and its `scripts[]` are printed with the page's CSP nonce, so its JavaScript
runs on every page, also for logged-in administrators, with whatever a script on the site may do.
Themes are not signed — the SHA-256 the Manager lists is all that vouches for a Library package —
so **install and activate only themes you trust**. What the core does limit:

- the template functions `setting`, `settingStr`, … return only public settings: never secrets,
  keys, licenses, the admin e-mail or plugin settings;
- the users and posts a template reaches (`.Q.Author`, `.Q.Post`, `.Post.Post`) are public copies: an
  author has only its ID, login, nicename, display name, website, registration date and the public
  profile fields (`description`, `first_name`, `last_name`, `nickname`); no e-mail, password hash, meta,
  roles or capabilities. A post's `.Password` is empty (`.HasPassword` says whether it has one), and
  `.AuthorEmail` is always empty: use `.AuthorAvatar`. With ACF-Go, `get_field` on a user or comment
  returns only registered fields, never sign-in or security meta;
- `scripts[]` must be files of the package (no remote URLs), so what runs is what was installed;
- the file types are limited (below) and templates cannot read files or run commands;
- Live Preview shows the theme in a sandboxed frame (see [Live Preview](#live-preview-with-demo-content)), so
  previewing a theme you do not trust yet does not expose the site.

## Library metadata

`theme.json` may also carry, for the theme library (Appearance › Add Themes › Library) and the
Hub:

```json
"category": "blog",
"style": "Minimal, editorial serif",
"demo": "demo/demo.json"
```

- `category` (optional, one of): `blog`, `magazine`, `portfolio`, `photography`, `business`, `agency`,
  `restaurant`, `food`, `travel`, `fashion`, `technology`, `education`, `health`, `fitness`,
  `nonprofit`, `music`, `wedding`, `real-estate`, `personal`, `minimal`, `home`, `shop`, `legal`,
  `construction`, `finance`, `architecture`, `gaming`, `entertainment`, `podcast`, `beauty`, `kids`,
  `pets`, `automotive`, `hotel`, `startup`, `science`, `religion`, `environment`, `books`, `sports`.
- `style`: a short phrase shown on the library card (at most 80 characters, one line, no markup).
- `demo`: path of the demo content file inside the package, a `.json` file under `demo/` (below).
- `web_builder`: path of the Pursae Web Builder parts file, a `.json` file under `web-builder/` (below, "Pursae Web Builder").

The REST themes endpoint (`wp/v2/themes`) reports them as `category`, `style` and `has_demo`.

### Pursae Web Builder

A theme package has nothing to declare for Pursae Web Builder: a published builder page renders
without the theme (see `docs/developer/web-builder.md`), and the builder's Theme Builder headers,
footers and templates are shown only by the bundled Hello Pursae theme
(`docs/developer/themes/hello-pursae.md`), never by a package. A package may opt in to the builder's theme library with `"web_builder": "web-builder/parts.json"` (templates and demo pages as builder docs;
see `docs/developer/web-builder-theme-library.md`); the market install ignores that file. Older packages that still carry the retired page-builder design key and folder
install as plain themes; both are ignored. A store theme declares
`"supports": {"woocommerce": true}` and is listed under Pursae Commerce › Store Themes, like
Fernwood, Good Boy Supply and Kite & Crayon.

## Demo content

A theme may ship one-click demo content: `demo/demo.json` plus images under `demo/images/`
(`.jpg`, `.jpeg`, `.png` or `.webp`, each ≤ 1 MiB, ≤ 24 images). The demo file is validated
when the package is installed: a package whose demo does not validate is rejected, with every
problem listed (`demo/demo.json: pages[1]: {{post:missing}} does not refer to an item of the
demo; …`). Appearance › Themes shows **Import Demo Content** on the active theme's card and in
its details when it has a demo, and right after activating such a theme a notice asks "This
theme comes with demo content. Import it?"; Add Themes › Library offers **Install & Import
Demo** (install, activate, import in one step). Importing needs the `import` capability.

Importing is idempotent: every created post, page, attachment and term carries the meta
`_wpgo_demo = "<theme slug>"` (and `_wpgo_demo_ref` = its id/slug in demo.json), so a second
import skips existing items, and **Remove Demo Content** deletes exactly them, the menus and
widgets the import created, and restores what it changed: the options and reading settings,
the menu previously assigned to each location, the theme mods it set, and the widgets that
were in the demo's widget areas (an import moves them to the inactive widgets). A category or
tag whose slug already exists on the site is used as it is and never removed.

```json
{
  "version": 1,
  "options": { "blogname": "Northwind Journal", "blogdescription": "Notes on slow living" },
  "categories": [
    { "slug": "travel", "name": "Travel", "description": "Places we loved." }
  ],
  "tags": [ { "slug": "coffee", "name": "Coffee" } ],
  "media": [
    { "id": "hero", "file": "demo/images/hero.jpg", "alt": "Morning light over the harbour", "caption": "" }
  ],
  "pages": [
    { "slug": "home", "title": "Home", "content": "<!-- wp:paragraph --><p>…</p><!-- /wp:paragraph -->",
      "template": "", "featured_media": "hero", "front_page": true },
    { "slug": "blog", "title": "Blog", "content": "", "posts_page": true },
    { "slug": "about", "title": "About", "content": "…", "parent": "" }
  ],
  "posts": [
    { "slug": "a-week-in-lisbon", "title": "A week in Lisbon", "date": "2026-09-12T09:30:00",
      "excerpt": "…", "content": "<!-- wp:paragraph -->…", "categories": ["travel"], "tags": ["coffee"],
      "featured_media": "hero", "format": "standard", "sticky": false,
      "comments": [ { "author": "Ada", "email": "ada@example.com", "content": "Lovely!", "date": "2026-09-13T10:00:00" } ] }
  ],
  "menus": [
    { "name": "Main menu", "location": "primary", "items": [
      { "title": "Home", "page": "home" },
      { "title": "Travel", "category": "travel" },
      { "title": "About", "page": "about", "children": [ { "title": "Contact", "page": "contact" } ] },
      { "title": "Instagram", "url": "https://instagram.com/" }
    ] }
  ],
  "widgets": { "sidebar-1": [ "<!-- wp:search /-->", "<!-- wp:latest-posts {\"postsToShow\":4} /-->" ] },
  "customizer": { "accent": "#c2410c", "layout": "grid" }
}
```

Rules:

- `version` is `1`. Unknown fields are ignored.
- Content (and excerpts, widgets and text Customizer values) is block markup
  (Gotenberg/WordPress block comments) or plain HTML. Tokens: `{{media:hero}}` is the imported
  image's URL, `{{media_id:hero}}` its attachment id (for `<!-- wp:image {"id":{{media_id:hero}}} -->`
  and `wp-image-{{media_id:hero}}`), `{{page:about}}`, `{{post:a-week-in-lisbon}}`,
  `{{category:travel}}` and `{{tag:coffee}}` permalinks. Every token must name an item of the demo.
- Slugs (categories, tags, pages, posts) match `^[a-z0-9]+(-[a-z0-9]+)*$` and are unique per
  kind; media ids are handles. A category may name a `parent` category of the demo.
- Pages: `title` required; optional `content`, `excerpt`, `featured_media` (a media id),
  `parent` (another page's slug, in any order), `menu_order`, `template` (the name of a file in
  the theme's `templates/`, e.g. `page-wide`; see Templates), `front_page`, `posts_page` (at
  most one each; they set Settings › Reading, `show_on_front=page`).
- Posts: `title` required; `date` like `2026-09-12T09:30:00` (also `2026-09-12 09:30:00` or a
  bare day, in the site's timezone) and not in the future — undated posts get one day apart
  before now; `categories` / `tags` are slugs of the demo's own lists (a post without
  categories goes to the default category); `format` is a post format; `sticky`; `comments`
  (≤ 20 per post, `author` and `content` required, optional `email`, `url`, `date`) are
  imported approved.
- Menus (≤ 10): `name` required; `location` one of the theme's `menu_locations` (one menu per
  location); each item has exactly one of `page`, `post`, `category` (slugs of the demo) or
  `url` (http(s), a site path or `#anchor`); `title` is required for `url` items and defaults
  to the page/post title or category name otherwise; `children` nest up to 3 levels.
- `widgets` keys are the theme's `widget_areas`; each value is a list of block markup strings,
  added as block widgets.
- `customizer` keys are the theme's own setting ids (or `custom_logo`); values match the
  setting type (a hex color, `true`/`false`, an integer within min/max, one of the choices, a
  string; an `image` setting or `custom_logo` takes a media id, `"hero"` or `"{{media:hero}}"`).
  They are stored as theme mods of that theme.
- `options` may only set `blogname`, `blogdescription` (strings), `posts_per_page` (1–100),
  `date_format`.
- Demo pages are block or HTML content. Keys the format does not name (such as the page-builder
  trees older demos carried) are ignored and the page is imported with its `content`.
- A store theme's `goocommerce` section imports products, categories, attributes, coupons and
  shipping (`internal/server/goo_demo.go`). With Pursae Multi Language active every imported item gets the default
  language.
- Limits: 24 images, 50 pages, 100 posts, 10 menus. An import runs in the request, with a
  two-minute limit, and reports its counts in an admin notice.

Demo text is original (no lorem ipsum, no copyrighted text) and images are original
(generated) artwork.

The default theme, Twenty Twenty-Five, ships several demo sets in the same format, one per
`web/themes/twentytwentyfive/demos/<set>/demo/` (built by `tools/twentytwentyfive/demos.py` from
the theme's public-domain photographs). Its Import Demo Content opens a chooser; a set is marked
`_wpgo_demo = "twentytwentyfive-<set>"`, and importing another set first removes the one imported
before, so its pages, menus, front page and color scheme never stay behind.

## Theme library

The library of ready themes is **not part of the binary** (the bundled themes in `web/themes/`
are). Its sources live in the repository under `library/themes/<slug>/` (each a complete theme
package directory whose name is its slug) and are published to Pursae Manager as theme packages
(`make publish-themes`, below). A site reads the library from

- **Pursae Manager** (the default, `https://hub.pursae.com`): `GET /api/v1/themes`, every page, cached 12
  hours per license key like the plugin catalog. Each listing carries the version's `theme.json`
  (`manifest`), so cards, categories, styles, search and the store themes need no download. Pro,
  private and beta themes appear only for a site whose license includes them. A package is
  downloaded when it is installed, previewed or its files are needed: its size and SHA-256 must
  match the listing (themes are not signed), it is validated like an upload (and must carry the
  listed slug and version) and extracted into `<data>/theme-library-cache/<slug>/<version>/`.
  Screenshots are fetched once from the Manager into the same directory and served by the site
  (`/wp-admin/theme-install?action=library-screenshot&theme=<slug>`), so the browser never talks to
  the Manager. When the Manager does not answer, it is not asked again for two minutes.
- **a directory** when `PURSAE_THEME_LIBRARY_DIR` is set (development, offline sites, the tests:
  `PURSAE_THEME_LIBRARY_DIR=library/themes make dev`). A directory that does not validate is logged
  and left out.
- neither: the Library tab is empty and says why (no Manager configured, or not reachable).

Appearance › Add Themes opens on the **Library** tab: the themes with screenshot, name, category
and style, filter links for the categories that have themes, and search. **Install** hands the
package to the same installer as Upload Theme, so it lands in `<data>/themes` with the same
validation (including the demo file); an installed theme shows Activate / Live Preview, and when
the library carries a higher `version` than the installed one the card offers **Update** (theme
mods and widgets are kept). The installed theme can be customized and deleted like any package.
The All, Bundled and Uploaded tabs list the installed themes as before.

The installer records where a package came from in `<data>/themes/<slug>/.pursae-source`: `hub`
(the library from Pursae Manager), `library` (a library directory) or `upload`. Themes from the
Manager — and packages installed before the source was recorded — get **updates**: when the
catalog lists a higher version, Appearance › Themes shows WordPress' *New version available.
Update now* notice on the card and in the details, and Dashboard › Updates lists the theme under
**Themes** with **Update Themes**. Updating downloads the package, checks its size and SHA-256 and
installs it in place; the theme stays active if it was, and the site sends a `theme_update` event.
Nothing is updated automatically. Uploaded packages are updated by uploading the new version.

**Recommended themes.** The operator marks Manager listings `preinstall`. Right after the installer
of a new site finishes, the site installs, in the background, every such theme it may see (not
activated; one `theme_install` event each); errors are logged and never affect the installer, and
nothing happens without a Manager. Existing sites get the same with **Install recommended themes**
on the Library tab, shown while a recommended theme is not installed.

**Publishing the library.** `make publish-themes PURSAE_MANAGER_DIR=/path/to/pursae-manager`
(or `tools/publish-themes.sh`) runs the Manager's bulk import against `library/themes`:

```sh
pursae-manager import -kind theme -publish [-preinstall slug,slug|all] [-access pro] library/themes/*
```

from the pursae-manager checkout, with that checkout's environment (`DATABASE_URL`,
`PURSAE_MANAGER_DATA_DIR`). `PREINSTALL=slug,slug|all` and `ACCESS=public|pro|private` are passed
on; `PURSAE_MANAGER=/path/to/pursae-manager-binary` runs a built binary instead of `go run`. Versions
the Manager already has are skipped, so bump a theme's `version` to publish a change.

## Live Preview with demo content

Every card of Add Themes › Library has **Live Preview** (the screenshot, or a button when the
card has no Install & Import Demo), and the details of an installed package theme that ships a
demo have **Preview with demo content** on Appearance › Themes. Both open
`/wp-admin/theme-preview?theme=<slug>` (`&source=installed` for the installed files): WordPress's
full-screen theme-install overlay with the theme's name, author, version, description and the
actions Add Themes or Themes would offer (Install, Install & Import Demo, Update, Activate, Import
Demo Content), previous/next library theme, collapse, and the desktop/tablet/mobile switcher. The
iframe shows the theme as it looks with its demo content imported — pages, posts, menus, widgets,
images and Customizer values — while the site, its database, uploads and settings stay untouched.
Previews need `switch_themes` (and `install_themes` for a library theme).

Each previewed theme gets a **sandbox**: a second, complete site in the PostgreSQL schema
`wpgo_preview_<slug with _>` of the same database (the pool's `search_path`; migrated with the
normal migrations) with its uploads and themes directory in `<data>/preview/<slug>/`. It is
installed like a fresh site that copies the site's address, permalink, time zone and date settings,
and the theme is installed, activated and given its demo with the same code as on the site. The
first preview builds it in the background (a few seconds; the iframe shows *Preparing preview…*
and reloads when it is ready); later previews reuse it, also after a restart. It is rebuilt when
the theme's files or version, or the copied settings, change. At most 8 sandboxes are kept, the
least recently used go first; **Clear theme previews** on the Library tab drops them all (schemas
and files).

The sandbox is served under `/wp-admin/theme-preview/<slug>/` as an anonymous visitor: no
cookies reach it and none come back, only GET and HEAD are allowed (comments, forms and REST
writes are refused), and its dashboard, login and comment posting answer 403. Responses carry
`X-Robots-Tag: noindex, nofollow` (the sandbox also discourages search engines). The sandbox runs
as if it were the site at the site's own address; the preview prefix is added to every URL of its
responses on the way out (absolute and root-relative links, assets, uploads, `srcset`, `url()`,
JSON-escaped URLs, redirects), so navigation stays inside the preview. A sandbox keeps its theme
packages in its own registry and has no page cache, plugin runtime, scheduler, mail or Hub.

The previewed theme's code is not trusted, so every sandbox response carries
`Content-Security-Policy: sandbox allow-scripts allow-forms allow-popups` (merged into the
sandbox's own policy; no `allow-same-origin`, no `X-Frame-Options`, `frame-ancestors 'self'`):
the preview runs in an opaque origin, its scripts work but cannot read the site's cookies, call the
admin or the REST API as you, touch the preview screen around it or navigate the top window. A
sandboxed frame does not send the login cookie for its own assets and links, so a browser that
loads `/wp-admin/theme-preview/<slug>/…` (the iframe, a link) is redirected to
`/wp-content/theme-preview/<slug>/<token>/…`, where a token — an HMAC over the theme, the user
and an expiry (6–7 hours), checked together with the user's `switch_themes` on every request —
replaces the cookie. It only opens that theme's preview, read-only. The *Preparing preview…*
page is core markup and is not sandboxed (its status poll needs the cookie).
