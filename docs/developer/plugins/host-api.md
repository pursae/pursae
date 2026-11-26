---
title: Host API reference
description: Every host function a plugin can call, its SDK wrapper, arguments, result and the permission it needs.
weight: 50
section: plugins
---

Host functions are how a plugin reaches the site. Each one is a JSON request to a `wpgo` import;
the Go SDK wraps them in typed functions. A call the plugin is not allowed to make returns
`*wpgo.Error` with code `permission_denied` (`wpgo.IsPermissionDenied(err)`); invalid arguments
return `invalid_request`; failures inside the site return `host_error`. Outside the Wasm host
(unit tests) every function returns `wpgo.ErrNotInHost`.

## Hooks and logging

| SDK | Host import | Permission |
|-----|-------------|------------|
| `AddFilter(hook, priority, fn)` (generic over the value type), `AddAction(hook, priority, fn)` | `add_hook` | the hook's ([hooks](hooks.md)) |
| `OnInit(fn)` | — | runs after registrations are sent |
| `Log(level, msg)`, `Logf(format, …)` | `log` | none. Levels `debug`, `info`, `warning`, `error`; shown on the Plugins screen and in the server log |

## Options

| SDK | Host import | Permission |
|-----|-------------|------------|
| `GetOption(name) (string, found, error)` — non-string values come back JSON-encoded | `get_option` | `options:read` |
| `GetOptionJSON(name, &v) (found, error)` | `get_option` | `options:read` |
| `OptionBool(name, def) bool` | `get_option` | `options:read` |
| `UpdateOption(name, value) error` | `update_option` | `options:write` |

Readable: the plugin's own `<slug>_*` (dashes → underscores) and `plugin_<slug>_*` options and the
public core options listed under [permissions](permissions.md#options-a-plugin-can-read); anything
else reads as not found. Writable: only the plugin's own options. They are stored in the plugin's
namespace (`wasm:<slug>:<name>`), so they never collide with a core or built-in option; the plugin
uses the plain names. Names are at most 191 characters.

## Posts

| SDK | Host import | Permission |
|-----|-------------|------------|
| `GetPost(id) (*Post, found, error)` | `get_post` | `posts:read` |
| `QueryPosts(PostQuery) (*PostList, error)` | `query_posts` | `posts:read` |
| `InsertPost(PostInput) (*Post, error)` | `insert_post` | `posts:write` |
| `UpdatePost(id, PostInput) (*Post, error)` | `update_post` | `posts:write` |

`Post` has `ID, Title, Content, Excerpt, Status, Type, Slug, Date, Modified` (RFC 3339 in the
site's time zone), `Author` (user id), `Link` (permalink of published posts), `Terms` (map by
taxonomy: `category`, `post_tag`, each `{ID, Name, Slug, Taxonomy}`) and `Meta` (the post's meta
map).
`InsertPost` and `UpdatePost` refuse meta keys that start with an underscore. Those are protected: the core and
other plugins keep trusted data there (edit locks, Pursae Web Builder's `_web_builder` marker), and plugins never
hold `unfiltered_html`.

`PostQuery`: `Type` (`post` default, `page`), `Status` (`publish` default, `draft`, `pending`,
`private`, `future`, `any`), `Search`, `Author`, `Term` (term id), `OrderBy` (`date`, `title`,
`modified`, `id`), `Order` (`DESC`, `ASC`), `PerPage` (1–100, default 10), `Page`. The result has
`Posts`, `Total`, `Pages`.

`PostInput` fields are pointers (`wpgo.String("…")`) so an update changes only what you set:
`Title` (≤1000 chars), `Content` (≤1 MiB), `Excerpt` (≤10 000), `Status`, `Type` (`post` or
`page`), `Slug`, `Meta` (merged; a `nil` value deletes the key). Content, title and excerpt go
through the same KSES filter as a user without `unfiltered_html`. A new post defaults to type
`post`, status `draft`, the default category, and the current user as author (none in cron).
During a request (hooks, shortcodes, REST routes, admin pages, Dashboard widgets, meta boxes) the
plugin acts as the signed-in user: editing a post or publishing needs that user's capabilities.
Scheduled events may write any post. Outside both (`OnInit`, activation hooks) there is no user: a
draft or pending post can be created, but editing an existing post or publishing is refused. Posts
go through the same store functions as the admin, so slugs are made unique, revisions are kept and
the page cache is purged.

```go
p, err := wpgo.InsertPost(wpgo.PostInput{
	Title:   wpgo.String("Weekly digest"),
	Content: wpgo.String("<p>…</p>"),
	Status:  wpgo.String("publish"),
	Meta:    map[string]any{"digest_week": 43},
})
```

## Users

| SDK | Host import | Permission |
|-----|-------------|------------|
| `CurrentUser() (*User, error)` — `nil` for anonymous visitors and in cron | `get_current_user` | `users:read` |

`User` has `ID, Login, DisplayName, Roles, Caps` and `Can(cap) bool`. Plugins never see e-mail
addresses or password hashes.

## Shortcodes, REST routes, settings pages, assets

| SDK | Host import | Permission | Guide |
|-----|-------------|------------|-------|
| `AddShortcode(tag, fn)` | `register_shortcode` | `content:filter` + tag in manifest | [shortcodes](shortcodes.md) |
| `RegisterRestRoute(ns, route, methods, handler)`, `RegisterRestRouteWithHeaders(ns, route, methods, headers, handler)` | `register_rest_route` | `rest:routes` | [REST routes](rest-routes.md) |
| `RegisterSettingsPage(page)`, `Setting(id)`, `SettingString/Bool/Float`, `SettingName(id)` | `register_settings_page`, `get_option` | `admin:settings`, `options:read` | [settings pages](settings-pages.md) |
| `EnqueueStyle(Style)`, `EnqueueScript(Script)` | `enqueue_style`, `enqueue_script` | `content:filter` (+ `admin:settings` for admin styles, `content:html` / `admin:scripts` for scripts) | [assets](assets.md) |
| `RegisterAdminPage(AdminPage)` | `register_admin_page` | `admin:pages` | [admin UI](admin-ui.md) |
| `RegisterDashboardWidget(id, title, cap, fn)` | `register_dashboard_widget` | `admin:dashboard` | [admin UI](admin-ui.md) |
| `RegisterMetaBox(MetaBox)` | `register_meta_box` | `admin:metaboxes` | [admin UI](admin-ui.md) |
| `RegisterBlock(Block)` | `register_block` | `blocks:register` | [admin UI](admin-ui.md) |
| `RegisterBuilderWidget(BuilderWidget)` | `register_builder_widget` | `builder:widgets` | [admin UI](admin-ui.md) |

## Scheduled events

| SDK | Host import | Permission |
|-----|-------------|------------|
| `ScheduleEvent(hook, interval, args) error` | `schedule_event` | `cron` + hook in manifest |
| `ScheduleSingleEvent(hook, at, args) error` | `schedule_event` | `cron` |
| `UnscheduleEvent(hook) error` | `unschedule_event` | `cron` |
| `UnscheduleEventArgs(hook, args) error` | `unschedule_event` with `args` | `cron` |

See [scheduled events](scheduled-events.md).

## Storage and transients

| SDK | Host import | Permission |
|-----|-------------|------------|
| `Storage.Get(key) (string, found, error)`, `Storage.Set(key, value) error` | `storage_get`, `storage_set` | `storage` |
| `Storage.Delete(key) error`, `Storage.List(prefix, after, limit) (keys, next, error)` | `storage_delete`, `storage_list` | `storage` |
| `Transients.Get(key, &v) (found, error)`, `Transients.Set(key, value, ttl) error`, `Transients.Delete(key) error` | `transient_get`, `transient_set`, `transient_delete` | `storage` |

Storage is a private key/value table per plugin (keys ≤256 chars, values ≤1 MiB, strings); it
is deleted with the plugin. Transients are JSON values with a time-to-live (0 = none, keys ≤172
chars, values ≤1 MiB) in the site's transients table, scoped to the plugin and cleaned up daily.

## HTTP and notices

| SDK | Host import | Permission |
|-----|-------------|------------|
| `HTTPRequest(HTTPRequestArgs{Method, URL, Headers, Body})` — GET, POST, PUT, PATCH, DELETE or HEAD; at most 32 headers (`Host`, `Content-Length` and hop-by-hop headers are refused), body ≤1 MiB; same response, timeout and address rules as `HTTPGet` | `http_request` | `http:outbound` |
| `SendMail(Mail{To, Subject, Body, HTML, ReplyTo})` — queues an e-mail through the site's mail settings; 1-10 recipients, the sender is set by the site, 60 messages per plugin per hour | `send_mail` | `mail:send` |
| `HTTPGet(url) (*HTTPResponse, error)` — `Status`, `Header`, `Body` (≤1 MiB; a larger body is an error, not truncated), 8 s timeout, private and loopback addresses are blocked | `http_get` | `http:outbound` |
| `AdminNotice(kind, message) error` — kinds `NoticeInfo/Success/Warning/Error`, plain text ≤2000 chars, only meaningful inside an `admin_notices` callback | `add_admin_notice` | `admin:notices` |

## Translation

| SDK | Host import | Permission |
|-----|-------------|------------|
| `T(text) string`, `X(text, context) string` — the text in the language of the current request | `translate` | none |

With Pursae Multi Language on, a string a plugin passes to `T` or `X` is listed on
**Languages › Translations** under "Plugin: *name*" (domain = the plugin's slug, `X`'s context is
the gettext context) and comes back translated into the visitor's language: the site's own
translation first, then an installed language pack, else unchanged. With the plugin off, on a
request without a language (cron, admin) or on an older host, the text comes back as it is, so
calling `T` is always safe. Text ≤10000 bytes, context ≤200 bytes. Pass whole sentences (not
fragments you concatenate) and keep placeholders like `%s` inside the string:

```go
wpgo.AddFilter("the_content", 10, func(_ wpgo.Context, html string, _ []any) string {
	return html + "<p>" + wpgo.T("Thanks for reading!") + "</p>"
})
label := wpgo.X("Post", "noun")
```

## The wire format

For other languages, or curiosity: every import takes `(ptr, len)` of a UTF-8 JSON request in
the guest's memory and returns a packed `ptr<<32|len` of a JSON envelope `{"ok":true,"result":…}`
or `{"ok":false,"code":"…","error":"…"}` allocated with the guest's `wpgo_alloc`. The full ABI
(exports, request shapes, limits) is in `internal/plugins/README.md` of the repository.
