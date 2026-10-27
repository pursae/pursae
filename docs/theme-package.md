# Theme packages

A WordPress Go theme is a directory (or a zip of that directory) with no Go code in it:

```
theme.json          metadata and everything a Go theme would register in code
templates/*.html    Go html/template files (the template hierarchy, see below)
assets/**           style.css, scripts, fonts, images, screenshot.png — the theme's web root
readme.md           optional, shown on the theme's marketplace page
```

Installed themes live in the themes directory (`WPGO_THEMES_DIR`, default `<data>/themes`,
the same directory the Theme File Editor writes its overrides to), one sub-directory per
slug: `<data>/themes/<slug>/theme.json`. The server loads every such directory at start-up
and right after Appearance › Themes › Add Theme › Upload Theme installs one; a package that
does not validate is logged and skipped. A package whose slug matches a bundled theme
(`twentysixteen`, `classic`, …) is rejected, and the directory name must equal the slug.
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
  `requires_core` (semver) must not be newer than the running WordPress Go. `author_url`
  and `theme_url` are http(s) URLs. `screenshot` defaults to `assets/screenshot.png`.
- `styles`/`scripts`: `handle` is `^[a-z0-9]+([_-][a-z0-9]+)*$` and unique within its
  list; `src` is a theme-relative path under `assets/` (served under the theme URI with
  `?ver=<version>`) or an absolute http(s) URL. `deps` may reference core handles
  (`wp-block-library`, `jquery`, …) or the theme's own handles; the theme's assets are
  printed after core's, ordered so that each follows its own dependencies like
  `wp_enqueue_*` (a cycle is an error), in declared order otherwise. Styles print in
  `wp_head`, scripts in `wp_head` or, with `in_footer`, in `wp_footer`. Every referenced
  theme file must exist in the package. `editor_styles` are `.css` files, `preload_fonts`
  `.woff2` files, both under `assets/`.
- `widget_areas[].id` and `menu_locations[].id` are handles (same pattern), unique; `name`
  is required. Widget areas use the `<section id class="widget …">` / `<h2 class="widget-title">`
  markup of the bundled themes.
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
| page         | `page`, `singular`, `index`                             |
| category     | `category-<slug>`, `category`, `archive`, `index`       |
| tag          | `tag-<slug>`, `tag`, `archive`, `index`                 |
| author, date | `author` / `date`, `archive`, `index`                   |
| search       | `search`, `index`                                       |
| 404          | `404`, `index`                                          |

Only `index.html` is required. Parts are ordinary templates included with
`{{template "header" .}}`, `{{template "content" .}}` and so on (`header`, `footer`,
`sidebar`, `comments`, `searchform`, `content*`, … by convention).

The data is the same `siteView` the bundled themes receive: `.DocumentTitle`, `.Head`
(`wp_head`), `.Footer` (`wp_footer`), `.BodyOpen`, `.BodyClass` (`body_class`),
`.AdminBar`, `.SiteName`, `.Description`, `.HomeURL`, `.CustomLogo`, `.TitleIsH1`,
`.Template`, `.Q` (the main query), `.Posts` / `.Post` (each with `.ID`, `.Title`,
`.Permalink`, `.Content`, `.Excerpt`, `.ShowExcerpt`, `.Class` (`post_class`), `.Thumbnail`,
`.AuthorName`, `.AuthorURL`, `.DateText`, `.DateISO`, `.Categories`, `.Tags`,
`.CommentCount`, `.CommentsURL`, `.EntryMeta`, `.EditLink`, `.Biography`, …),
`.Pagination`, `.PostNav`, `.Comments` (`.Title`, `.List`, `.Count`, `.Open`, `.Form`),
`.ArchiveTitle`, `.ArchiveDesc`, `.SearchQuery`, `.TotalPosts`, `.PrivacyLink`,
`.CanPublish`, `.IsHomeHeader`, `.PostsPageTitle`, `.Areas` and `.Menus`. Package themes
also get `.Data.Settings` (every Customizer setting, typed, with defaults applied; image
settings hold the image URL) and `.Data.Nav` (rendered menus by location).

Functions, on top of the generic ones (`date`, `plural`, `num`, `dict`, `safe`,
`settingStr`, …): `{{nav_menu . "primary"}}` and `{{has_nav_menu . "primary"}}`
(`wp_nav_menu` / `has_nav_menu`), `{{dynamic_sidebar . "sidebar-1"}}` and
`{{is_active_sidebar . "sidebar-1"}}`, and `{{theme_mod . "accent"}}` (`get_theme_mod`
with the declared default). Package themes get `singular` or `hfeed` on the body like the
bundled ones. See `examples/themes/starter` for a complete, minimal theme and
`web/themes/classic/templates` for a bundled one.

## Packaging

`zip -r starter.zip theme.json templates assets readme.md` from inside the theme directory
(the files sit at the zip root, not under a folder; a single top-level folder is also
accepted and stripped; `tools/build-themes.sh` does this for the examples). Limits: 32 MiB
per package, 4 MiB per file, 64 MiB extracted, 2000 files, only these extensions:
`.json .html .css .js .map .svg .png .jpg .jpeg .gif .webp .avif .ico .woff .woff2 .ttf
.otf .md .txt`. The templates must parse before anything is replaced. Uploading needs the
`install_themes` capability; a package whose slug is already installed replaces it only
when its version is higher (theme mods and widgets are kept). Themes carry no executable
code, so they are not signed; the Hub and the installer verify the SHA-256 of the zip.
