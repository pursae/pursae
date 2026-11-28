---
title: Settings pages
description: Declare a settings page the admin renders under Settings, and read the saved values.
weight: 80
section: plugins
---

A plugin cannot render admin HTML itself (there is no PHP and no template access from the
sandbox). Instead it **declares** a page — title, description, fields — and the core renders it
under **Settings › \<Menu title\>** with the WordPress form styles, validates the input, saves
each field as an option and shows "Settings saved." Permission: `admin:settings` to register,
`options:read` to read the values.

```go
func init() {
	wpgo.RegisterSettingsPage(wpgo.SettingsPage{
		PageTitle:   "Gopher Facts",
		MenuTitle:   "Gopher Facts",
		Description: "Options of the Gopher Facts plugin.",
		Fields: []wpgo.SettingsField{
			{ID: "heading", Label: "Heading", Type: "text", Default: "Did you know?", Placeholder: "Shown above the list"},
			{ID: "count", Label: "Facts per page", Type: "number", Default: 3, Min: ptr(1), Max: ptr(10)},
			{ID: "greet", Label: "Greet logged-in readers", Type: "checkbox", Default: true,
				Description: "Adds a personal greeting."},
			{ID: "style", Label: "List style", Type: "select", Default: "cards",
				Options: []wpgo.SettingsOption{{Value: "cards", Label: "Cards"}, {Value: "plain", Label: "Plain list"}}},
			{ID: "footer", Label: "Footer note", Type: "textarea", Rows: 3},
		},
	})
}
```

## Fields

| Type | Saved as | Extra |
|------|----------|-------|
| `text` | string | `Placeholder` |
| `textarea` | string (CRLF normalised, ≤64 KiB) | `Rows`, `Placeholder` |
| `number` | JSON number | `Min`, `Max` (clamped on save), `Placeholder` |
| `checkbox` | `true` / `false` | the `Description` is the label next to the box |
| `select` | string; values not in `Options` are ignored | `Options` (1–100) |
| `radio` | string; values not in `Options` are ignored | `Options` (1–100) |
| `email` | string, a valid address or "" | `Placeholder` |
| `url` | string, an http(s) URL (cleaned) or "" | `Placeholder` |
| `password` | string; never printed back, an empty input keeps the stored value | `Placeholder` |
| `color` | string, `#rgb`, `#rrggbb` or `#rrggbbaa`, or "" | a hex text input with a colour swatch and **Clear**; an untouched field stays "" (before 0.6.2 it was a bare colour input that saved `#000000`) |
| `date` | string `YYYY-MM-DD` or "" | — |
| `image` | string, an http(s) URL or a site-relative path (`/wp-content/uploads/…`), or "" | `Placeholder`; a **Select image** button opens the media library (upload or pick), with a preview and Remove. Since 0.5.5 |

The same field types are used by [meta boxes, block attributes and Web Builder widget controls](admin-ui.md),
except `image`, which settings pages and meta boxes support and blocks and widgets refuse (use `url` there).

An `image` field stores the image's address, so `wpgo.SettingString("icon", "")` gives a URL you can
put in an `<img src>`. The field stays a plain URL input when JavaScript is off. Registering an
`image` field on a core older than 0.5.5 fails (unknown field type) and the whole settings page is
left out, so set `"requires": "0.5.5"` in plugin.json when you use it.

Rules: `ID` is `^[a-z0-9_]{1,64}$` and unique; 1–50 fields per page; labels and placeholders
≤200 characters, descriptions ≤2000. Everything is escaped when rendered — HTML in labels is
shown as text.

## Where values live

Each field is the option `plugin_<slug>_<id>`, dashes in the slug replaced by underscores:
`plugin_shortcode_demo_heading`. Only the owning plugin can read and write them (`get_option` /
`update_option` allow the prefix), and they are hidden from other plugins.

Read them with the helpers, which know the prefix once the page is registered:

```go
heading := wpgo.SettingString("heading", "Did you know?")
count   := int(wpgo.SettingFloat("count", 3))
greet   := wpgo.SettingBool("greet", true)
name    := wpgo.SettingName("heading") // "plugin_shortcode_demo_heading"
```

Unsaved fields return the `Default` you gave (the helpers fall back to the `def` argument). Saving
the page purges the page cache, so shortcodes and filters pick up the new values at once.

## What you do not get

Custom HTML, buttons that call the plugin, file uploads, or pages outside Settings. For a page
with your own HTML and form handling use an [admin page](admin-ui.md) (`admin:pages`).
