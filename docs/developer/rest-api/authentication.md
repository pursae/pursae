---
title: Authentication
description: Cookie + nonce for browser clients, Application Passwords for everything else, and what anonymous requests can do.
weight: 20
section: rest-api
---

Requests without credentials are **anonymous**: they can read public content (published posts,
pages, media, terms, public user profiles, comments) and nothing else.

## Cookie and nonce (browser clients)

A logged-in user's session cookie authenticates a request **only together with a nonce**, exactly
as in WordPress, to stop cross-site requests from using the cookie:

- header `X-WP-Nonce: <nonce>`, or query `?_wpnonce=<nonce>`, with a nonce for the action
  `wp_rest`;
- a cookie without a nonce is treated as anonymous (`401` on writes);
- a wrong nonce returns `403 rest_cookie_invalid_nonce`.

Admin pages expose the nonce in `wpApiSettings.nonce`, and every authenticated REST response
returns a fresh one in the `X-WP-Nonce` response header. The editor's `wp.apiFetch` sends it
automatically.

## Application Passwords (scripts, apps, CI)

HTTP Basic authentication with the user's login and an Application Password — a 24-character
password shown once, grouped in blocks of four; spaces are ignored:

```bash
curl -u 'editor:abcd efgh ijkl mnop qrst uvwx' \
     -H 'Content-Type: application/json' \
     -d '{"title":"From the API","status":"draft"}' \
     https://blog.example.com/wp-json/wp/v2/posts
```

Each password is hashed like a login password, records its last use and address, and can be
revoked on its own. Create and revoke them under Users › Profile › Application Passwords (the
password is shown once), through the REST controller
(`/wp/v2/users/me/application-passwords`, `POST` returns the plaintext password once), or let an
app request one through the authorization flow at `/wp-admin/authorize-application` (the REST
index advertises it). The authorization screen shows the same scope checkboxes as the profile: an app
can pre-tick scopes with `scopes[]=content&scopes[]=seo` (or `scopes=content,seo`) in the request,
the user can change them, and the chosen scopes are stored with the password. A wrong or missing password returns `401 incorrect_password`.

Basic authentication counts toward the login throttle: after five failures from one address within
15 minutes further Basic-auth requests from it get `429 too_many_retries` (Pursae Security, when active,
applies its own lockouts instead). The server verifies the most recently used password first and
remembers a successful match for a few minutes, so a working client does not pay for a password hash
on every request; a revoked password stops working at once.

Use Application Passwords only over TLS: Basic authentication sends the password with every
request.

## Application Password scopes

A password is limited to **scopes**. Only a password created with **Full access** (`full_access`)
has all of its user's capabilities; one created without scopes gets `content` and `media`, the
OAuth default. A password with scopes is narrowed twice:

1. the user's capabilities are cut down to the ones the scopes need, so every capability check
   in a handler sees the smaller set, and
2. each route belongs to one scope; a route of another scope, or a route no scope covers (plugin
   routes, the Application Password routes themselves), returns
   `403 rest_scope_forbidden` with `data.required_scope`.

Scopes never add a capability the user does not have: an editor's password with the `plugins`
scope still cannot activate plugins.

| Scope | Routes | Capabilities kept |
|-------|--------|-------------------|
| `content` | `/wp/v2/posts`, `pages`, `blocks`, `categories`, `tags`, `comments`, `types`, `taxonomies`, `search`, `block-renderer`, `/web-builder/v2`, `/pll/v1`, `/batch/v1`, `/pursae/v1/forum` | post and page caps, `manage_categories`, `moderate_comments` (HTML is filtered as for a user without `unfiltered_html`; see below) |
| `media` | `/wp/v2/media` | `upload_files` and the post caps attachments use |
| `themes` | `/wp/v2/templates`, `template-parts`, `global-styles`, `themes`, `menus`, `menu-items`, `menu-locations`, `navigation`, `sidebars`, `widgets`, `/pursae/v1/themes` | `switch_themes`, `edit_theme_options`, `edit_themes`, `install_themes`, `delete_themes`, `unfiltered_html` |
| `plugins` | `/pursae/v1/plugins`, `/pursae/v1/updates` | `activate_plugins`, `install_plugins`, `delete_plugins` |
| `cache` | `/pursae/v1/cache` | `manage_options` |
| `seo` | `/seogo/v1`, `/pursae/v1/seo` | post caps, `manage_options`, the `seogo_*` caps |
| `settings` | `/wp/v2/settings`, `/pursae/v1/options`, `/pursae/v1/headless`, `/pursae/v1/health`, `/pursae/v1/backups`, `/pursae/v1/mcp/activity` | `manage_options` |
| `commerce` | `/wc/v3` | `manage_woocommerce`, `view_woocommerce_reports`, the product and shop order caps, `manage_product_terms`, `moderate_comments` |
| `users` | `/wp/v2/users` | `list_users`, `create_users`, `edit_users`, `delete_users` (roles, and other users' `email` and `password`, need full access; see below) |

**Raw HTML and site-wide code need `themes`.** `unfiltered_html` belongs to the `themes` scope only.
A password without it (for example `content` alone, or the OAuth default `content` and `media`) writes
posts, pages, comments, products and Web Builder documents through the same KSES filter as a user
without `unfiltered_html`, even on an administrator's account: no `<script>`, event handlers or
iframes. A stolen or prompt-injected token with the default scopes therefore cannot plant a script
that runs for an administrator. Raw HTML, and a design's own HTML, CSS and JavaScript that pages load
site wide, need the `themes` scope as well (or full access): raw HTML in posts and pages, Pursae Web
Builder html and code sections and custom CSS (a `content`-only password publishes the page without
them, or keeps the stored ones), code imports (`web_builder_import_html` method `code`; method `exact`
works with `content`), site imports, design kits and their class and variable overrides. The contact
form routes (`/contact-form-7/v1`, which set a form's mail recipients and headers) belong to no scope:
they need full access. The `settings` scope reads every `/pursae/v1/options` group but changes only
some: writing `gocdn` (the asset URL every page loads scripts from), `smtp` (where all site mail,
password resets included, is sent), `security` (two-step login, lockouts, the login page) or
`headless` (public permissions, front-end sign-up, CORS) needs full access (403 `rest_scope_forbidden`).
Changing the maintenance page's custom CSS or custom HTML needs `unfiltered_html`, so the `themes`
scope as well.

**`themes`, `plugins` and `users` are equivalent to full access** on an administrator's account, and
the scope picker says so: `themes` edits theme files and writes scripts that run on every page,
`plugins` installs plugins and can grant them the dangerous permissions (`content:html`,
`admin:scripts`) by naming them, and `users` can change the account's own email and password. Give
them only to a client you would give full access.

Every scoped password may also read the REST index, `/wp/v2/users/me` and use the MCP endpoint
`/pursae/v1/mcp` (whose tools are checked route by route). Batch requests (`/batch/v1`) re-check
each inner request. On `/wp/v2/users/me` a scoped password may edit the profile fields (name, URL,
description, …) but changing the account's own `email` or `password` needs the `users` scope, and
roles (the account's own or another user's) or another user's `email` or `password` need full access:
`403 rest_scope_forbidden` with `data.required_scope` (`users` or `full_access`) otherwise.
`wc/v3/customers` (single and batch) is narrower for shop integrations: with the `users` scope, a
credential may change the `email` or `password` of, or delete, accounts without a staff role (customers,
subscribers); any other account needs full access. Customer `meta_data` never writes or shows sign-in and
security state (password-reset keys, Pursae Security, sessions, capabilities, forum verification), and
product, coupon and order `meta_data` ignores core-internal keys such as Web Builder (`_wb_…`) meta and
`_wp_page_template`, whatever the credential. No scope
keeps `promote_users`, so a scoped password that creates a user with a `roles` value gets
`403 rest_cannot_edit_roles`; without `roles` the new user gets the site's default role. A password
whose stored scopes are all unknown (for example a scope removed in a later release) is refused
rather than given full access.

Set scopes with the checkboxes under **Profile › Application Passwords › Limit access to**, or
through REST: `scopes` is an array (or a comma list) on `POST /wp/v2/users/me/application-passwords`
and on `POST …/application-passwords/{uuid}`, or `"full_access": true` instead; the responses include
`scopes` (`["full_access"]` for full access). A scoped password cannot create, change or revoke
Application Passwords.

```bash
curl -u admin:… -H 'Content-Type: application/json' \
     -d '{"name":"Cursor","scopes":["content","media","seo"]}' \
     https://blog.example.com/wp-json/wp/v2/users/me/application-passwords
```

## OAuth 2.1 (MCP clients)

AI clients that speak the [MCP authorization spec](https://modelcontextprotocol.io/specification/2025-06-18/basic/authorization)
sign in with OAuth instead of a pasted password. The tokens work on every REST route like an
Application Password and are narrowed by scopes the same way; Basic auth keeps working.

| Endpoint | |
|----------|-|
| `/.well-known/oauth-protected-resource` (also `…/wp-json/pursae/v1/mcp`) | protected-resource metadata (RFC 9728) |
| `/.well-known/oauth-authorization-server` | authorization-server metadata (RFC 8414) |
| `POST /oauth/register` | dynamic client registration (RFC 7591), public clients only, a JSON body of at most 64 KB; 10 per address (an IPv6 /64 counts as one address), 25 per network (IPv4 /24, IPv6 /48) and 100 in total per hour, and a client that gets no token within an hour is removed |
| `/wp-admin/oauth-authorize` | consent screen: who asks, scope checkboxes, approve or deny |
| `POST /oauth/token` | `authorization_code` (PKCE) and `refresh_token` grants |
| `POST /oauth/revoke` | token revocation (RFC 7009): ends the whole grant |

- PKCE with `code_challenge_method=S256` is mandatory; codes live 5 minutes and work once; redirect URIs
  must match a registered one exactly (https, http on localhost, or an app scheme such as `cursor://`).
- Access tokens (`pat_…`) last one hour and are sent as `Authorization: Bearer …`; refresh tokens
  (`prt_…`) last 30 days and are rotated on every use. Only SHA-256 hashes are stored.
- Reuse detection (OAuth 2.1): a refresh token that a rotation already replaced gets `invalid_grant`, and
  more than 30 seconds after that rotation it also ends the whole grant (a client that refreshes twice
  at once, or retries a lost answer, is only refused). An authorization code presented a second time
  revokes the grant its first exchange created.
- A user keeps at most 20 grants: a new one ends the oldest. Grants whose refresh token has expired are
  deleted, and so are clients left without a grant.
- Scopes are the same names as above, with the same rules (no raw HTML without `themes`; `themes`, `plugins` and `users` marked as equivalent to full access). As for an Application Password, nothing ticked means `content` and `media`, not all of the user's permissions. The OAuth consent screen preselects `content` and `media` when the client asks for no scope, treats an empty selection as those two, and gives everything only when the separate **Full access** box is ticked. The Pursae Headless delivery API (`/wp-json/headlessgo/v1`) has its own API tokens and does not accept Application Passwords or OAuth tokens; manage Headless content types and entries with `/pursae/v1/headless/...` instead.
- An unauthenticated request to `/wp-json/pursae/v1/mcp` gets `401` with
  `WWW-Authenticate: Bearer resource_metadata="…"`, which is how clients find the flow. A bad or
  expired token gets `error="invalid_token"`.
- Users › Profile lists the connected applications; **Revoke** ends a grant at once.

## Capabilities

Writes are checked against the user's capabilities the way WordPress does: `edit_posts` for
posts and blocks, `edit_pages` for pages, `upload_files` for media, `manage_options` or
`edit_theme_options` for settings and global styles, `manage_categories` for terms,
`edit_theme_options` for sidebars and widgets, and the meta capabilities (`edit_post`,
`delete_post`, …) for individual items. An authenticated user without the capability receives
`403 rest_forbidden`; an anonymous one `401 rest_cannot_create`-style errors.

## Plugin routes

Plugin routes get the same authentication: the handler receives the user (or none) and decides
([plugin routes](plugin-routes.md)).
