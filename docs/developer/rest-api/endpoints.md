---
title: Endpoints
description: Every route the core registers, one line each.
weight: 30
section: rest-api
---

Routes are regular expressions; `{id}` stands for `(?P<id>[\d]+)`. Methods are the registered
ones; `HEAD` works wherever `GET` does and `PUT`/`PATCH` wherever `POST` does.

**Request checks (0.6.3+)**, as in WordPress:

- The `wp/v2` collections (posts, pages, blocks, media, terms, users, comments) validate `page` (≥ 1),
  `per_page` (1–100), `order` (`asc`/`desc`) and `orderby` (the route's own list); a value outside
  them is a 400 `rest_invalid_param` naming every bad parameter. `per_page=-1` is still accepted
  and returns at most 100 items per page.
- A JSON body that is not valid UTF-8 or contains a NUL character is a 400 `rest_invalid_json`.
- `search` uses at most 200 characters and 32 words; `-word` excludes items containing that word.
  A search stops after 5 seconds.
- `GET /wp-json/<namespace>` (for example `/wp-json/wp/v2`) lists that namespace and its routes.

## Posts, pages, blocks (`wp/v2`)

| Route | Methods | Notes |
|-------|---------|-------|
| `/posts`, `/pages`, `/blocks` | GET, POST | `page`, `per_page`, `offset`, `search`, `author`, `author_exclude`, `categories`, `categories_exclude`, `tags`, `tags_exclude`, `tax_relation` (`AND` default, or `OR`), `after`, `before`, `modified_after`, `modified_before`, `status`, `orderby`, `order`, `slug` (comma list or `slug[]`), `include`, `exclude`, `parent`, `parent_exclude`, `sticky` (posts only), `_embed`, `_fields`; a malformed filter value is a 400 `rest_invalid_param`. Without `edit_others_posts` other statuses list only what the caller may read. `/blocks` (synced patterns) needs `edit_posts` to read, and a new pattern is published only for users with `publish_posts`; `context=edit` for raw content (needs `edit_posts`/`edit_pages`; items the caller cannot edit omit `password`, and password-protected or members-only ones also omit `content.raw` and `excerpt.raw`). `meta.footnotes` (the block editor's footnotes, a JSON string) is read and written. Writing an unknown `author`, `featured_media` or `parent` is a 400 (`rest_invalid_author`, `rest_invalid_featured_media`, `rest_post_invalid_id`) (0.6.3+) |
| `/posts/{id}`, `/pages/{id}`, `/blocks/{id}` | GET, POST, DELETE | `?force=true` deletes instead of trashing. Pages read and write `template` (`_wp_page_template`): `""` is the default template, otherwise one of the theme's page templates (a package's `page_templates`) or the page's current template; another value is a 400 |
| `/posts/{id}/autosaves`, `/pages/{id}/autosaves`, `/blocks/{id}/autosaves` | GET, POST | the editor's autosave |
| `/posts/{parent}/autosaves/{id}`, `/pages/{parent}/autosaves/{id}`, `/blocks/{parent}/autosaves/{id}` | GET | one autosave of the parent (404 `rest_post_invalid_id` when it is not one) |
| `/posts/{id}/revisions`, `/pages/{id}/revisions`, `/blocks/{id}/revisions` | GET | |
| `/posts/{parent}/revisions/{id}`, `/pages/{parent}/revisions/{id}`, `/blocks/{parent}/revisions/{id}` | GET, DELETE | one revision; `DELETE` needs `?force=true` |

With Pursae Multi Language on, posts, pages, categories and tags (and the translated custom types and
taxonomies) carry `lang` and `translations` (`{"en": 12, "fr": 15}`). Collections take `?lang=fr` (that
language only; objects without a language count as the default language). Writes take `lang` and
`translations` in the JSON or form body, as Polylang's REST fields, or in the query string
(`?lang=fr&translations[en]=12`); the body wins. A translation link is only made to an object in that
language, of the same type, that the user may edit; an entry for the saved object's own language is ignored.

## Media

| Route | Methods | Notes |
|-------|---------|-------|
| `/media` | GET, POST | upload with `multipart/form-data` (`file`) or a raw body with `Content-Disposition: attachment; filename=…`; image sizes are generated (in the format of Settings › Media › Image output format). Location and camera data (EXIF/XMP) is removed from uploaded JPEGs unless Settings › Media › *Remove location and camera data from uploaded photos* is unticked or the server sets `PURSAE_KEEP_EXIF=1` (0.6.3+). For callers without `upload_files`, attachments of members-only posts the caller cannot read are not listed |
| `/media/{id}` | GET, POST, DELETE | `title`, `caption`, `alt_text`, `description`, `post`; images also `focal_point` (`{"x": 0.3, "y": 0.2}` from the top left, 0–1, or `null` for the centre): cropped sizes are made again around it, and `media_details.focal_point` returns it. A replaced or re-cropped file has `media_details.file_version`, and its URLs end in `?v=<version>`. With Pursae Multi Language and its Media module on: `?lang=` returns that language's texts, `context=edit` adds `pll_i18n` (per-language overrides) and updates accept it |
| `/media/{id}/edit` | POST | the block editor's image crop / rotate / flip: `modifiers` are applied in order and the result is saved as a new item `<name>-edited.<ext>` (201); the original is untouched. JPEG, PNG, GIF and WebP only |
| `/media/{id}/post-process` | POST | |

## Terms

| Route | Methods | Notes |
|-------|---------|-------|
| `/categories`, `/tags`, `/wp_pattern_category` | GET, POST | `search`, `parent`, `post`, `slug`, `hide_empty`, `orderby`. `/wp_pattern_category` holds the categories of synced patterns |
| `/categories/{id}`, `/tags/{id}`, `/wp_pattern_category/{id}` | GET, POST, DELETE | updating or deleting needs `manage_categories` (creating a tag or pattern category only needs `edit_posts`). Terms cannot be trashed: `DELETE` needs `?force=true`, otherwise 501 `rest_trash_not_supported` (0.6.3+). An unknown `parent` is a 400 `rest_term_invalid` |
| `/taxonomies`, `/taxonomies/{taxonomy}` | GET | |

## Users and comments

| Route | Methods | Notes |
|-------|---------|-------|
| `/users` | GET, POST | `GET`: public profiles (`who=authors`), or all users with `list_users`; `capabilities`, `offset`. Without `list_users`, `search` matches only the id, login, slug and display name. `POST` creates a user (`create_users`; `username`, `email`, `password`) |
| `/users/{id}` | GET, POST, DELETE | `POST` updates; `DELETE` needs `?force=true` (users cannot be trashed) and `delete_users`, and `reassign` moves the user's content to another user |
| `/users/me` | GET, POST, DELETE | the current user; `context=edit` for the full record (and `meta.persisted_preferences`, which only that context returns) |
| `/comments` | GET, POST | `post`, `parent`, `parent_exclude`, `include`, `exclude`, `author`, `author_exclude`, `author_email`, `type`, `status`, `offset`; anonymous comments honour the Discussion settings |
| `/comments/{id}` | GET, POST, DELETE | |
| `/users/{user_id}/application-passwords` | GET, POST, DELETE | `{user_id}` is an ID or `me`. List, create (`name`, `app_id`, `scopes` or `full_access`; the password is returned once) or revoke all. Also managed on the profile page |
| `/users/{user_id}/application-passwords/{uuid}` | GET, POST, DELETE | read, rename and change scopes, or revoke one |
| `/users/{user_id}/application-passwords/introspect` | GET | the Application Password that authenticated this request (404 `rest_no_authenticated_app_password` for any other login) |

## Site

| Route | Methods | Notes |
|-------|---------|-------|
| `/settings` | GET, POST | `title`, `description`, `timezone`, `date_format`, `posts_per_page`, `default_category`, `show_on_front`, `page_on_front`, … (`manage_options`); values are typed and validated like the Settings screens (http(s) `url`, valid `email`, known `language`, loadable `timezone`, `posts_per_page` ≥ 1, existing pages); any invalid value is a 400 `rest_invalid_param` and nothing is saved |
| `/types`, `/types/{type}` | GET | `post`, `page`, `attachment`, `wp_block`, … |
| `/statuses`, `/statuses/{status}` | GET | |
| `/themes`, `/themes/{stylesheet}` | GET | `status=active` returns the active theme with its `theme_supports` |
| `/search` | GET | `search`, `type`, `subtype`: post subtypes are limited to the searchable public types (posts, pages, public custom types); another subtype is a 400 |
| `/sidebars` | GET | widget areas |
| `/sidebars/{id}` | GET, POST | one widget area; `POST` changes its widgets |
| `/widgets`, `/widgets/{id}` | GET, POST, DELETE | |
| `/widget-types`, `/widget-types/{id}`, `/widget-types/{id}/encode`, `/widget-types/{id}/render` | GET, POST | |
| `/menus`, `/menu-items` | GET, POST | navigation menus and their items; like WordPress, every request needs `edit_theme_options` (401 for visitors, 403 otherwise). `menu-items` takes `menus` as a filter |
| `/menus/{id}`, `/menu-items/{id}` | GET, POST, DELETE | `DELETE` needs `?force=true` (menus cannot be trashed) |
| `/menu-locations`, `/menu-locations/{location}` | GET | theme menu locations and the menu assigned to each (`edit_theme_options`) |
| `/block-renderer/{name}` | GET, POST | server-side rendering of dynamic blocks |
| `/block-patterns/patterns`, `/block-patterns/categories` | GET | core's patterns and the active theme's (Twenty Twenty-Five); users who can edit posts |
| `/block-types` | GET | the blocks registered on the server (dynamic blocks; `namespace` filters). Blocks the editor registers only in JavaScript are not listed |
| `/block-directory/search`, `/pattern-directory/patterns` | GET | empty lists |
| `/templates`, `/templates/lookup`, `/template-parts` | GET | stubs: empty (there is no Site Editor; templates are Web Builder's, [coming from the Site Editor](../web-builder-site-editor.md)) |
| `/global-styles/{id}`, `/global-styles/themes/{stylesheet}`, `/global-styles/themes/{stylesheet}/variations` | GET | stubs: empty settings and styles; read only |

## Site management (`pursae/v1`)

The routes AI agents and scripts use to run a site. Each checks the capability of its admin screen
(shown in brackets) and belongs to an [Application Password scope](authentication.md#application-password-scopes).

| Route | Methods | Notes |
|-------|---------|-------|
| `/pursae/v1/plugins` | GET, POST | list built-in and Wasm plugins (`activate_plugins`); `POST {"package": "<base64 zip>"}` installs a signed package, inactive (`install_plugins`) |
| `/pursae/v1/plugins/{slug}` | GET, DELETE | one plugin; `DELETE` removes an installed Wasm plugin (`delete_plugins`; built-in plugins only deactivate) |
| `/pursae/v1/plugins/{slug}/activate`, `/deactivate` | POST | the Plugins screen's own action; a Wasm plugin gets `permissions` (array; default: all its manifest asks for except the dangerous `content:html` and `admin:scripts`, which give it the equivalent of an administrator's session and are granted only when listed, as the consent screen leaves them unticked) |
| `/pursae/v1/themes` | GET, POST | list themes (`switch_themes`); `POST {"package": "<base64 zip>", "activate": true}` installs a [theme package](../themes/index.md) (`install_themes`) |
| `/pursae/v1/themes/{slug}` | DELETE | delete an inactive theme (`delete_themes`) |
| `/pursae/v1/themes/{slug}/activate` | POST | switch theme |
| `/pursae/v1/themes/{slug}/files` | GET | editable files (`edit_themes`): `style.css`, asset paths, `templates/…` |
| `/pursae/v1/themes/{slug}/file?file=…` | GET, POST, DELETE | read; `POST {"file", "content"}` saves like the Theme File Editor (template parse + render check, rolled back on failure); `DELETE` restores the original. Refused while file editing is disabled (`PURSAE_DISALLOW_FILE_EDIT` or Pursae Security) |
| `/pursae/v1/media/{id}/replace` | POST | a new file for a media item, as `multipart/form-data` (`file`) or a raw body with `Content-Disposition: attachment; filename=…` (`upload_files`, plus own item or `edit_others_posts`; scope `media`). The item keeps its ID; a file of the same type keeps its URL and size names, another type (or aspect ratio) gets new names and the old paths redirect (301) to them. Same kind only: image for image, video for video, audio for audio, other files for other files. Answers like `GET /wp/v2/media/{id}` (`upload_warning` when no sizes could be made) |
| `/pursae/v1/cache` | GET | Pursae Cache status, stats and crawler (`manage_options`) |
| `/pursae/v1/cache/purge` | POST | `{"all": true}`, `{"url": "/about/"}`, `{"post": 12}` or `{"tags": ["home"]}` |
| `/pursae/v1/cache/settings` | GET, POST | the `wpgo_cache_*` / `wpgo_opt_*` options (typed); saving rebuilds the cache |
| `/pursae/v1/headless/types` | GET, POST | Pursae Headless content types and components (`manage_options`, plugin active); POST creates from `{"schema": {...}}` |
| `/pursae/v1/headless/types/{uid}` | GET, POST, DELETE | read, replace (entries are migrated) or delete (`force=true`) a schema |
| `/pursae/v1/headless/entries/{uid}` | GET, POST | list (`status`, `locale`, `page`, `per_page`) or create `{"data": {...}, "publish": true}` |
| `/pursae/v1/headless/entries/{uid}/{doc}` | GET, POST, DELETE | read, update (merge) or delete (`force=true`) an entry; `/publish` and `/unpublish` POST |
| `/pursae/v1/seo/settings` | GET, POST | Pursae SEO site-wide settings; `POST {"settings": {…}}` merges a partial object |
| `/pursae/v1/seo/redirects` | GET, POST | list; create or update (`id`) with `source`, `target`, `code` (301, 302, 307, 410, 451), `match_type`, `status` |
| `/pursae/v1/seo/redirects/{id}` | DELETE | |
| `/pursae/v1/analytics` | GET | Pursae Analytics overview for `?range=today\|7d\|30d\|month\|90d\|12m`: `summary`, `reports` (pages, entry, sources, campaigns, countries, devices, browsers, os, goals, outbound, downloads, notfound), `web_vitals` p75 (`manage_options`, plugin active, scope `settings`) |
| `/pursae/v1/csp` | GET, POST | allowed external sources of the front-end Content Security Policy (see [Security](../operations/security.md)) |
| `/pursae/v1/csp-reports` | GET, DELETE | the CSP violation reports the site collected: list, or clear (`manage_options`) |
| `/pursae/v1/health` | GET | version, database, cron queue (`queued`, `running`, `failed`, `overdue`, `last_error`), Site Health score, plugins, cache, `updates_available`, last backup (`manage_options`) |
| `/pursae/v1/updates` | GET, POST | updates Pursae Manager offers (`update_plugins`); `POST {"type": "plugin"\|"theme"\|"core", "slug": "…"}` installs one. Core replaces the binary and restarts the site a second after the answer (`update_core`) |
| `/pursae/v1/backups` | GET, POST | stored archives, or `?job=<id>` for a running backup, upload or download (`remote` / `remote_error` tell how an automatic upload went); `POST {"categories": [...], "note": "…"}` starts one in the background (202 with `job`; default: every category) (`manage_options`) |
| `/pursae/v1/backups/remote` | GET, POST | Backup Manager remote storage: `GET` gives the settings without secrets (`secret_key_set`, `passphrase_set`) and the encrypted backups in the bucket; `POST {"file": "<archive>"}` encrypts and uploads a stored archive in the background (202 with `job`) (`manage_options`) |
| `/pursae/v1/mcp/activity` | GET | audit log of MCP tool calls that wrote: `tool`, `client`, `per_page`, `page` filters (`manage_options`) |
| `/pursae/v1/forum/forums` | GET, POST | Pursae Forum categories and forums, flat with counts and URLs; `POST {title, kind: forum\|category, parent, slug, description, closed, order}` creates one (`manage_options`; **409** `forum_off` while the plugin is off) |
| `/pursae/v1/forum/forums/{id}` | POST, PUT, DELETE | change (fields given only) or delete a forum or category (`manage_options`) |
| `/pursae/v1/forum/topics` | POST | start a topic as the caller `{forum_id, title, content}` (`forum_participate`), with the checks of the site's new-topic form: closed and staff-only forums and an unconfirmed email address when the forums ask for one (403), the member's HTML filter, the title and text limits and the disallowed words (422 `forum_invalid`), the flood and hourly limits (429 `forum_throttled`); `status` in the answer is `pending` when the topic waits for a moderator |
| `/pursae/v1/options` | GET | the option groups below (`manage_options`) |
| `/pursae/v1/options/{group}` | GET, POST | `contact` (reCAPTCHA / Turnstile keys), `cookie`, `forum` (Pursae Forum; the Google client secret reads as `********`), `gocdn`, `headless`, `maintenance`, `multilang`, `pixel`, `security`, `seo`, `smtp`: the built-in plugin's settings object. Secrets (passwords, keys, tokens) read as `********`; `POST {"settings": {…}}` deep-merges a partial object, and a `********` sent back keeps the stored secret. `security` changes follow the settings screen's rules (number ranges, login slug, IP lists, idle timeout, alert address; the caller's own IP on the block list or a two-step requirement while no e-mail can be delivered is refused): a 400 `rest_invalid_param` saves nothing |
| `/pursae/v1/mcp` | POST, GET, DELETE | the [MCP server](../operations/ai-mcp.md) (streamable HTTP) |

## Pursae Forum (`forum/v1`)

Only while Pursae Forum is on (otherwise `rest_no_route`, and the namespace is left out of the
index). Only published, non-spam content of published forums is listed. The read routes below are
public; the 0.2.5 community routes (second table) also write. The topic and reply lists take `page` and `per_page` (default 10, max 100); every list
sends `X-WP-Total` / `X-WP-TotalPages` (the forum tree comes in one page).

| Route | Methods | Notes |
|-------|---------|-------|
| `/forum/v1/forums` | GET | the whole tree, flat: each category followed by its forums. `id`, `parent`, `slug`, `title`, `kind` (`forum` or `category`), `description` (HTML), `closed`, `order`, `topic_count`, `reply_count`, `last_active`, `last_topic` (`id`, `title`, `link`, or null), `link` |
| `/forum/v1/forums/{id}/topics` | GET | a forum's topics, sticky first, then by last activity (a category has none: empty list). Topic: `id`, `forum`, `forum_title`, `title`, `slug`, `content.rendered`, `author`, `sticky`, `closed`, `reply_count`, `date`, `last_active`, `edited`, `link`; 404 `rest_forum_invalid_id` |
| `/forum/v1/topics/{id}` | GET | one topic with its opening post (same fields); 404 `rest_topic_invalid_id` |
| `/forum/v1/topics/{id}/replies` | GET | the replies, oldest first: `id`, `topic`, `forum`, `position`, `content.rendered`, `author`, `date`, `edited`, `link` |

0.2.5 adds to the forum `icon`, `color` (0.2.6: `custom` with `color_hex` and the computed `color_ink`; topics `forum_color_hex`, `forum_color_ink`), `label`, `number`, `chips` (`name`, `slug`, `link`), `tips`,
`staff_only`; to the topic `tags`, `views`, `likes`, `solved`, `solution`, `forum_color`,
`forum_icon`, `forum_label`; to the reply `likes`, `solution`. `/forums/{id}/topics` takes
`sort=latest|popular|unanswered|solved` and `tag=<slug>`.

| Route | Methods | Notes |
|-------|---------|-------|
| `/forum/v1/topics/{id}/view` | POST | the view beacon (no auth, no body): `{views, counted}`; robots, repeats within 30 minutes and floods are not counted |
| `/forum/v1/presence` | POST | marks the visitor online (`forum` optional): `{online, members, minutes, forum_online}` |
| `/forum/v1/online` | GET | the same counts (`?forum=`), nothing recorded |
| `/forum/v1/stats` | GET | `forums topics replies members solved solved_pct new_topics_week new_members_week online online_members` |
| `/forum/v1/tags` | GET | tags in use (`?forum=`): `name slug link count` |
| `/forum/v1/topics/similar` | GET | `?q=<title>&tags=a,b`: up to 5 `{id, title, link, forum_title, reply_count, solved}` |
| `/forum/v1/levels`, `/badges`, `/weekly` | GET | the levels with `rank` and the point weights; the enabled badges; members of the week `{member, delta}` |
| `/forum/v1/me` | GET | the member's state (`?topic=`: `liked`, `subscribed`, `can_solve`); `{logged_in:false}` for visitors; `no-store` |
| `/forum/v1/posts/{id}/like` | POST, DELETE | like / unlike: `{liked, likes}`; 403 `rest_forum_own_post`, 429 after 60 in 10 minutes |
| `/forum/v1/posts/{id}/report` | POST | `reason` (`spam abuse off_topic other`), `note` |
| `/forum/v1/topics/{id}/subscription` | POST, DELETE | follow / stop following by email |
| `/forum/v1/topics/{id}/solution` | POST, DELETE | `reply=<id>` accepts it (the topic's author or a moderator); DELETE withdraws |
| `/forum/v1/preview` | POST | `content` rendered as it would be posted: `{rendered}` |
| `/forum/v1/draft` | GET, POST, DELETE | the member's new-topic draft (`forum title content tags notify`) |

The member routes need cookie + `X-WP-Nonce` (`wp_rest`, forum.js reads it from
`<script id="pf-config">` on member pages) or an Application Password; without them they answer
401 `rest_not_logged_in`. Every response of the community routes is `no-store`.

`author` is `{id, name, avatar, link, moderator, initials, hue, has_photo}` plus `points`,
`progress` and `level` (`id`, `name`, `rank`, `min`) when loaded (never the login or the email), or
null for a deleted account. Dates are RFC 3339 UTC. A page past the last one is a 400
`rest_post_invalid_page_number`. The forum's settings are under `/pursae/v1/options/forum` (above).

## Other namespaces

| Route | Methods | Notes |
|-------|---------|-------|
| `/` (`/wp-json/`) | GET | the index: site info, namespaces, every route with its methods |
| `/batch/v1` | POST | up to 25 requests in one call; a sub-request cannot be another batch |
| `/oembed/1.0/embed` | GET | Pursae as an oEmbed provider: `url=` (a post or page of this site), `format=json\|xml`, `maxwidth` (200–600); 404 `oembed_invalid_url` otherwise. Public |
| `/oembed/1.0/proxy` | GET | `url=` — fetches the provider's oEmbed for the editor (`edit_posts`) |
| `/wp-block-editor/v1/url-details` | GET | `url=` — title, icon, description of a link (public addresses only; cached for an hour) |
| `/pll/v1/languages`, `/pll/v1/languages/{id or code}`, `/pll/v1/settings` | GET, POST, DELETE | Pursae Multi Language (Polylang's routes); only while the plugin is on. Settings are checked like the URL modifications form: `domains` are http(s) URLs, `force_lang` 2 and 3 need pretty permalinks, 3 a domain for every language (else 400 `rest_invalid_param`) |
| `/pll/v1/media/{id}/i18n` | GET, POST | per-language `alt_text`, `title`, `caption`, `description` of a media item: `{"fr":{"alt_text":"…"}}`, null or blank falls back to the item; `upload_files` plus own item or `edit_others_posts`; 404 while the Media module is off |
| `/pll/v1/translate`, `/pll/v1/translate/test` | POST | machine translation proxy: `{texts[], target, source?, html?}` → `{translations, target}`; `manage_options`, cookie auth with the `wp_rest` nonce, 60 calls per user per minute, 200 texts / 200 KB, never saves |
| `/pll/v1/visual/strings` | POST | saves string translations `{lang, items:[{original, translation}]}` (visual translation); `manage_options` and the `wp_rest` nonce, ≤200 items |
| `/web-builder/v2/…` | various | Pursae Web Builder: `documents/{id}` (load, autosave, publish, lock, revisions, `live-frame` at the post's own path), `library`, `prefs`, `fonts/css2`, `kits/{slug}/trust` (`{trust, code?}` → `{trust, scriptTrust, scriptCode}`) and the public `forms/{id}/{section}`; only while the builder is on. Requests and errors are in the [contract](../web-builder-contract.md#3-rest-namespace-web-builderv2) |
| `/<plugin-slug>/v1/…` | as registered | [plugin routes](plugin-routes.md) |
