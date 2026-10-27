# internal/plugins

The wpgo plugin system: WordPress-style hooks, plus sandboxed WebAssembly
plugins run with [wazero](https://wazero.io) (pure Go, no cgo).

| File | Contents |
|------|----------|
| `hooks.go` | `Registry`: actions and filters with priorities and owners |
| `permissions.go` | Permission vocabulary, hook-to-permission rules |
| `manifest.go` | `plugin.json` parsing and validation |
| `package.go` | Zip packages, Ed25519 signing and verification |
| `host.go` | `Host` interface the core implements, `MemoryHost` for tests |
| `runtime.go`, `plugin.go` | `Runtime`: loading, sandboxing, timeouts, failure handling |
| `abi.go`, `abi_ext.go` | The `wpgo` host import module |
| `registrations.go` | Shortcodes, REST routes, settings pages and enqueued assets a plugin registered |
| `cmd/wpgo-pkg` | CLI used by `tools/build-plugins.sh` (keygen, pack, verify) |

The plugin-author SDK lives in `sdk/go/wpgo`. Example plugins are in `examples/plugins`.

## Hooks

```go
hooks := plugins.NewRegistry()
hooks.AddFilter("the_content", 10, func(ctx context.Context, v any, args ...any) any { … })
out := hooks.ApplyFilters(ctx, "the_content", html, post)
hooks.DoAction(ctx, "admin_notices")
```

Callbacks run in ascending priority order. Callbacks with the same priority run
in the order they were registered. The default priority is 10. As in WordPress,
actions and filters share one namespace. Each registration records an owner:
`"core"` for `Registry.AddFilter` and `AddAction`, or a plugin slug through
`Registry.Owner(slug)`. `RemoveAll(owner)` unregisters everything the owner
added.

## Permissions

A plugin declares permissions in `plugin.json`, and the admin approves some or
all of them when calling `Runtime.Load`. The plugin gets only the permissions
that are both declared and granted.

| Permission | Allows |
|------------|--------|
| `options:read` | `get_option` (sensitive options are hidden; the plugin's own `plugin_<slug>_*` settings are readable) |
| `options:write` | `update_option` (only `<slug>_*` and `plugin_<slug>_*` options) |
| `posts:read` | `get_post`, `query_posts`, and hooks that expose post data (`save_post`, `save_post_*`, `wp_insert_post`, `transition_post_status`, `wpgo_post_stats`, …) |
| `posts:write` | `insert_post`, `update_post`, and hooks that change post data before saving (`wp_insert_post_data`, `content_save_pre`, …) |
| `http:outbound` | `http_get` |
| `cron` | hooks named `cron_*` / `wpgo_cron*`, `schedule_event`, `unschedule_event` |
| `admin:notices` | `admin_notices` hooks and `add_admin_notice` |
| `content:filter` | display filters: `the_*`, `comment_text`, `widget_text`, `body_class`, …; `register_shortcode`, `enqueue_style`, `enqueue_script` |
| `storage` | `storage_get` / `storage_set` (private to the plugin), `transient_get` / `transient_set` / `transient_delete` |
| `rest:routes` | `register_rest_route` |
| `users:read` | `get_current_user` |
| `admin:settings` | `register_settings_page` |

`HookPermission(hook)` maps a hook to the permission it needs. The `hooks`
array in the manifest must list every hook the plugin subscribes to, and
validation checks that the matching permissions are declared too. Likewise the
`shortcodes` array must list every shortcode tag the plugin registers. The
plugin needs no permission for `log`, or for hooks such as `init`.

## Packages and signing

A package is a zip file. These files sit at its root, and no other files are
allowed:

| File | Required | Limit |
|------|----------|-------|
| `plugin.json` | yes | 64 KiB |
| `plugin.wasm` | yes | 64 MiB |
| `readme.md`, `icon.svg` | no | 1 MiB / 256 KiB |
| `assets/…` | no | 2 MiB per file, 16 MiB and 256 files per package; `.css .js .svg .png .jpg .jpeg .webp .woff2` only |
| `signature` | if signatures are required | 1 KiB |

Files under `assets/` are served at `/wp-content/plugins/<slug>/assets/<path>`
while the plugin is active, and a plugin enqueues them with `enqueue_style` /
`enqueue_script`. Paths are plain segments (letters, digits, `.`, `-`, `_`; no
segment starts with a dot). The manifest's `assets` map lists the hex SHA-256
of every asset file: `WritePackage` (and `wpgo-pkg pack`) fills it in, and
`LoadPackage` rejects a package whose assets are unlisted, missing or do not
match. Because `plugin.json` is covered by the signature, so are the assets,
and the digest formula below is unchanged for packages without assets.

The signature is Ed25519 over a canonical SHA-256 digest:

```
digest    = SHA-256("wpgo-plugin-v1\n" || u64be(len(json)) || json || u64be(len(wasm)) || wasm)
signature = base64(Ed25519.Sign(key, digest)) + "\n"
```

`LoadPackage(r, size, trustedKeys, requireSignature)` behaves as follows:

- If the package has a signature and trusted keys are given, the signature must
  verify against one of the keys. Otherwise the call returns `ErrBadSignature`.
- If `requireSignature` is true, an unsigned package returns `ErrUnsigned`.
- `Sign`, `GenerateKey`, `EncodeKey`, `ParsePublicKey` and `ParsePrivateKey` are
  the helpers for creating and reading signatures and keys.
- Key files are base64. Lines starting with `#` are comments.

## ABI (version 1)

The plugin is a WASI preview1 **reactor** module. In Go, build it with
`GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared`. The default build mode
produces a command module (`_start`). Its runtime exits as soon as `main`
returns, so the host could not call its exports afterwards. `Load` rejects
command modules and tells the author to use `c-shared`.

Data crosses the boundary as UTF-8 JSON in guest linear memory. A *packed*
value is a `u64` equal to `ptr<<32 | len`.

Guest exports:

| Export | Signature | Purpose |
|--------|-----------|---------|
| `_initialize` | `() -> ()` | language runtime init (Go: runs package `init`s) |
| `wpgo_init` | `() -> ()` | called once after `_initialize`; registers hooks via `add_hook` |
| `wpgo_alloc` | `(size i32) -> i32` | allocate a buffer that the host writes into |
| `wpgo_free` | `(ptr, size i32)` | release a buffer returned by `wpgo_handle` |
| `wpgo_handle` | `(ptr, len i32) -> i64` | run a hook callback; returns packed response |

The host sends `wpgo_handle` this request:
`{"plugin":"slug","callback":"…","hook":"the_content","type":"filter","args":[…],"value":…}`.
The guest replies with `{"value":…}` or `{"error":"…"}`. The guest takes
ownership of the request buffer. `type` is `filter`, `action`, `shortcode`
(`hook` is the tag, `args` are `[attrs, content]`, the value must be a JSON
string) or `rest` (`hook` is the route path, `args` is `[request]`, the value is
`{"status":200,"body":…,"headers":{…}}`; see `RestRequest` / `RestResponse` in
`host.go`).

Host imports, module `wpgo`: every import has the signature
`(ptr, len i32) -> i64`. Each one takes a JSON request and returns a packed
pointer to a JSON envelope. The host allocates the envelope with `wpgo_alloc`,
and the guest then owns it. The envelope is either
`{"ok":true,"result":…}` or `{"ok":false,"code":"permission_denied|invalid_request|host_error","error":"…"}`.
The import returns `0` if no response could be delivered.

| Import | Request | Result | Permission |
|--------|---------|--------|------------|
| `add_hook` | `{"type":"filter\|action","hook":"…","priority":10,"callback":"…"}` | – | hook must be in manifest + its permission |
| `get_option` | `{"name":"…"}` | `{"found":bool,"value":…}` | `options:read` |
| `update_option` | `{"name":"…","value":…}` | – | `options:write` |
| `log` | `{"level":"debug\|info\|warning\|error","message":"…"}` | – | none |
| `http_get` | `{"url":"https://…"}` | `{"status":200,"header":{…},"body":"…"}` | `http:outbound` |
| `storage_get` | `{"key":"…"}` | `{"found":bool,"value":"…"}` | `storage` |
| `storage_set` | `{"key":"…","value":"…"}` | – | `storage` |
| `add_admin_notice` | `{"type":"info\|success\|warning\|error","message":"…"}` | – | `admin:notices` |
| `register_shortcode` | `{"tag":"…","callback":"…"}` | – | `content:filter` + tag in manifest `shortcodes` |
| `register_rest_route` | `{"namespace":"<slug>/v1","route":"/items/(?P<id>\\d+)","methods":["GET"],"callback":"…"}` | – | `rest:routes`; namespace is `<slug>` or `<slug>/vN` |
| `get_post` | `{"id":7}` | `{"found":bool,"post":{…}}` | `posts:read` |
| `query_posts` | `{"type","status","search","author","term","orderby","order","per_page"(≤100),"page"}` | `{"posts":[…],"total":n,"pages":n}` | `posts:read` |
| `insert_post` | `{"title","content","excerpt","status","type","slug","meta"}` | the post | `posts:write` |
| `update_post` | `{"id":7, …fields}` | the post | `posts:write` |
| `get_current_user` | `{}` | `{"found":bool,"user":{"id","login","display_name","roles","caps"}}` | `users:read` |
| `schedule_event` | `{"hook":"cron_x","interval":3600,"at":0,"args":{…}}` | – | `cron` + hook in manifest; `interval` 0 = one-off at `at` (unix) |
| `unschedule_event` | `{"hook":"cron_x"}` | – | `cron` |
| `register_settings_page` | `{"page_title","menu_title","description","fields":[…]}` | `{"option_prefix":"plugin_<slug>_"}` | `admin:settings` |
| `transient_get` | `{"key":"…"}` | `{"found":bool,"value":…}` | `storage` |
| `transient_set` | `{"key":"…","value":…,"ttl":60}` | – | `storage`; `ttl` seconds, 0 = no expiry |
| `transient_delete` | `{"key":"…"}` | – | `storage` |
| `enqueue_style` | `{"handle","src":"style.css","deps":[…],"where":"front\|admin\|both","media"}` | – | `content:filter`; `src` must be a package asset |
| `enqueue_script` | `{"handle","src":"app.js","deps":[…],"where","in_footer":bool}` | – | `content:filter` |

Post objects are `{"id","title","content","excerpt","status","type","slug","date","modified","author","link","terms":{"category":[…],"post_tag":[…]},"meta":{…}}`.
Settings fields are `{"id","label","type":"text\|textarea\|number\|checkbox\|select","description","placeholder","default","options":[{"value","label"}],"min","max","rows"}`;
the core renders the page under Settings › *menu_title* and saves each field
in the option `plugin_<slug>_<id>` (dashes in the slug become underscores).
Registrations made with `register_*` and `enqueue_*` are kept by the runtime
(`Plugin.Shortcodes()`, `RestRoutes()`, `SettingsPage()`, `Assets()`) and
re-sent by the guest after a re-instantiation, so repeating one is a no-op.

The guest may call host imports while it is inside any export. The host may
call `wpgo_alloc` from inside a host import.

## Sandbox and limits

- **Memory:** each plugin's linear memory is capped at `Config.MemoryLimitPages`.
  The default is 1024 pages (64 MiB).
- **Time:** each callback runs under a context deadline (`CallTimeout`, default
  2 s) with `WithCloseOnContextDone`. Instantiation and `wpgo_init` share a
  separate limit (`InitTimeout`, default 10 s).
- **Filesystem and network:** none. WASI is limited to stdout/stderr, clocks
  and `crypto/rand`. There are no preopens, args or environment variables.
  Guest stdout/stderr go to the plugin's log ring buffer, which you read with
  `Plugin.Logs()`.
- **Messages:** each message may be at most `MaxMessageSize` (default 8 MiB).
  `http_get` bodies may be at most `MaxHTTPBody` (default 4 MiB). Options,
  storage keys and values, and notices also have size checks. Post content is
  limited to 1 MiB, REST request and response bodies to 1 MiB, transient
  values to 1 MiB, settings pages to 50 fields, cron intervals to 60 s–1 year.
- **Failures:** a trap, timeout, `proc_exit`, panic (which the SDK recovers) or
  malformed response is logged. The filter then returns the unmodified value.
  If the module instance died, the next call creates a new one, and identical
  hook registrations are de-duplicated. After `MaxFailures` consecutive
  failures (default 5), the plugin is disabled: its hooks are removed and
  `Config.OnDisable` is called. Permission errors returned to the guest do not
  count as failures.
- **Re-entrancy:** calls into one plugin are serialised. Suppose a host function
  fires a hook that would call back into the same plugin. If it passes on the
  `ctx` it received, the runtime skips that callback and logs it instead of
  deadlocking.

## Wiring it into the core

```go
hooks := plugins.NewRegistry()
rt, _ := plugins.NewRuntime(ctx, hooks, plugins.Config{CoreVersion: version, CacheDir: cacheDir})
pkg, _ := plugins.LoadPackageFile(path, trustedKeys, true)
p, _ := rt.Load(ctx, pkg, approvedPermissions, coreHost) // coreHost implements plugins.Host
…
hooks.DoAction(ctx, "init")
html = hooks.ApplyFilters(ctx, "the_content", html, map[string]any{
	"id": post.ID, "type": post.Type, "is_single": isSingular,
}).(string)
hooks.DoAction(ctx, "admin_notices") // then render the notices queued through Host.AdminNotice

// Shortcodes, REST routes, settings pages and assets:
html = wp.DoShortcode(html, rt.ShortcodeTags(), func(tag string, attrs map[string]string, inner string) string {
	out, _ := rt.DoShortcode(ctx, tag, attrs, inner)
	return out
})
if p, route, params, ok := rt.FindRestRoute("/my-plugin/v1/items/7"); ok && route.Allows(method) {
	resp, err := p.ServeRest(ctx, *route, plugins.RestRequest{Method: method, Params: params, …})
}
for _, page := range rt.SettingsPages() { … }          // render under Settings, save plugin_<slug>_<field>
for _, a := range rt.EnqueuedAssets("front", "style") { … } // <link> tags; files from p.PackageAsset(path)
```

The core passes the current user to host functions through the context (it
reads it in `Host.CurrentUser`), and fires scheduled events by running the
hook named in `ScheduledEvent.Hook` as an action with the event payload.

These conventions describe the extra arguments each hook receives:

- **`the_content`:** a string value, plus one argument: a post object with at
  least `id`, `type` and `is_single`.
- **`wpgo_post_stats`:** a `map[string]any` value, plus one argument: a post
  object with `content`.
- **`admin_notices`:** no arguments. Plugins queue notices with
  `add_admin_notice`. The messages are plain text, so the core must escape them
  when it renders them.

Filters keep the Go type of the value. The JSON result is decoded back into the
value's dynamic type, so a `string` comes back as a `string`, and a struct as
the same struct.
