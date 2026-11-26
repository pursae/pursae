---
title: The Hub
description: The central service behind the marketplace and update checks, and what a site shares with it.
weight: 10
section: hub
---

The **Hub** is a small service (`wpgo-hub`) every Pursae site talks to. It has two faces:

- a **public JSON API** under `/api/v1/` that sites and the public site read — no authentication,
  cacheable, rate limited;
- an **admin panel** under `/admin/` where the operator manages the plugin and theme marketplace,
  core releases and signing keys.

The Hub is `https://hub.pursae.com`, and every production Pursae binary talks to it: there is no
environment variable or option to point a site elsewhere. Only development builds (`-tags devhub`)
read `PURSAE_DEV_HUB_URL` to use a local Manager
([development builds](../operations/configuration.md#development-builds-devhub)). Sites only talk to
a Hub over **https**; plain http is accepted for `localhost`, loopback addresses and `*.localhost` /
`*.test` development hosts.
Redirects never downgrade to http and drop the license and site headers when they leave the Hub's
host.

## What a site gets from it

| Feature | Endpoint | Where it shows |
|---------|----------|----------------|
| Plugin marketplace | `GET /api/v1/plugins` | Plugins › Add Plugin ([marketplace](marketplace.md)) |
| Theme marketplace | `GET /api/v1/themes` | Appearance › Themes › Add Theme |
| Package downloads | `…/{slug}/download[/{version}]` | one-click install; the site checks the SHA-256 and, for plugins, the Hub signature |
| Core releases | `GET /api/v1/index` → `core` | Dashboard › Updates; with a signed build for the site's platform, **Update to version X** installs it ([upgrading](../getting-started/upgrading.md)); the binaries are GitHub release assets the Manager signed on import ([publishing a release](../operations/releases.md)). Every other admin screen shows "Pursae X is available! Please update now." (or "Please notify the site administrator." without `update_core`), a count on Updates and "Get Version X" in the footer, from the release remembered at the last check; pre-releases sort before their release (0.2.0-beta.1 < 0.2.0) |
| Previous core release | `GET /api/v1/core/{version}` → `core` | Dashboard › Updates › **Reinstall version Y** once the kept previous binary is gone (24 hours after an update): the signed build of the version the site came from ([upgrading](../getting-started/upgrading.md)); beta needs a beta license (403), unknown versions are 404 |
| Plugin updates | `GET /api/v1/plugins` | Plugins and Dashboard › Updates, for plugins installed from the Hub ([marketplace](marketplace.md#updates)) |
| License check | `GET /api/v1/license` | Settings › License (see below) |
| Signing keys | `GET /api/v1/index` → `keys` | added to the site's trusted plugin keys automatically (replaced on every refresh, so a key the Hub drops is no longer trusted); verify core builds only when a release root key endorses them ([root of trust](#root-of-trust)) |
| Product logo | `GET /api/v1/index` → `brand` | replaces the shipped logo on every screen (see below) |

## Product logo

The logo on the admin bar, login screen, favicons and dashboard is
served from `/wp-includes/images/brand/` (`logo-32.png` … `logo-512.png`, `logo.svg`: the dark mark
for light backgrounds; `pursae-mark-white.svg`, `logo-dark.svg`: the light mark for dark backgrounds
such as the admin bar). A release
changes it by replacing those files in `web/wp-includes/images/brand`. A Hub changes it on every
connected site without a release by adding a `brand` block to `GET /api/v1/index`:

```json
"brand": {"logo_url": "/media/brand/logo.svg", "logo_version": "2026-11-01"}
```

`logo_url` is absolute or relative to the Hub; it must be a PNG, WebP or SVG of at most 1 MB. A
logo hosted elsewhere than the Hub must be https on a public address (private and loopback
addresses are refused at connect time). The logo is served, `/favicon.ico` included, with a
sandbox Content-Security-Policy and `nosniff`, so an SVG can never run as a page of the site.
When `logo_version` changes the site downloads the image into `uploads/brand/` and serves it for
every size; when the block goes away the shipped logo comes back.

The block may also carry `logo_dark_url` and `logo_dark_version`: a logo for dark backgrounds (a
light mark). It is fetched the same way into `uploads/brand/logo-dark.*` and served in place of the
dark-background files (`pursae-mark-white.svg`, `logo-dark.svg`); every other file keeps `logo_url`.
Without it, those files keep the shipped light mark (never `logo_url`, a dark mark that would vanish
on the dark admin bar). Sites read the index when they
check for updates (at most every 12 hours, or at once from Dashboard › Updates › Check again).

## Root of trust

A key listed in the same answer as the release it verifies proves nothing, so a core self-update
needs a **release root key** compiled into the binary:

- `rootKeysBuiltin` in `internal/server/hub_trust.go`: the Pursae release root
  `LxoPRqTiJfpXB0fc+peWabyDCWO/NzATjpk7pSe1IdY=`, pinned in every build, plus
- `go build -ldflags "-X wpgo/internal/server.rootKeysLinked=<base64>[,<base64>…]"` (the Docker
  image: `--build-arg ROOT_KEYS=…`), which **adds** roots and never removes the pinned one.

A Hub key verifies a release only if a root key endorsed it. The index then carries the
endorsement next to the key:

```json
"keys": [{"id": "hub-2026", "public": "<base64 Ed25519>", "endorsement": "<base64 Ed25519 signature>"}]
```

where `endorsement = base64(Ed25519.Sign(rootKey, "pursae-hub-key-v1\n" + id + "\n" + public))`
(`public` exactly as listed, no trailing newline). The root's private key stays offline; the
Hub operator asks for an endorsement whenever they generate a new key.

Plugin packages are verified only against endorsed Hub keys too (with `wpgo_hub_trust_keys` on).
A production binary cannot relax any of this. A development build (`-tags devhub`) with
`PURSAE_TRUST_HUB_KEYS=1` (opt-in, also in the development compose file) trusts every listed key, endorsed
or not, so a local Manager with a throwaway key can publish test releases and plugins. Automatic core
updates never install under that trust; update by hand.

### Rotating the release root

1. Generate the new root offline and endorse the Manager's current signing key with it (the
   Manager lists one endorsement per key; during the overlap, endorse with the root the sites
   in the field carry).
2. Ship a release whose builds carry both roots: add the new public key to `rootKeysBuiltin` (or
   pass it with `ROOT_KEYS` for a trial build). Sites trust a Manager key endorsed by either.
3. Once sites have updated, re-endorse the Manager keys with the new root and remove the old
   root from `rootKeysBuiltin` in a later release.

A compromised Manager key is rotated without touching the root: generate a new key in the
Manager, have the root endorse it, and stop listing the old one (sites drop a key the Manager
no longer lists).

## Licenses

Settings › License takes the license key bought from Pursae (option `pursae_license_key`, stored
encrypted with a key derived from the server secret, shown masked on the screen, never readable by
plugins). The site checks it with `GET /api/v1/license` and shows the answer: valid or
not (with the Hub's reason: unknown, revoked, expired, not valid for this domain, activation limit),
the plan, whether it includes Pro packages and the beta channel, the expiry date and the sites it is
active on. The answer is cached for 12 hours; saving the key and **Check now** ask again. Code that
needs it calls `licensePro()` / `licenseBeta()` (cached answer only, never the network).

Every request to the Hub carries `X-Pursae-Site` (the site's home URL, `scheme://host[:port]`) and,
when a key is entered, `X-Pursae-License`. The Hub then also lists the Pro and private packages and
the beta versions and core releases the license unlocks (cards show **Pro**, **Private** and
**Beta** badges). Because the answers depend on the key, the cached catalog and index are kept per
key and dropped when it changes.

The daily `wpgo_hub_check` also re-asks the Hub about the license. An expiring (14 days), expired,
revoked, wrong-site or unknown key shows a notice to administrators on every admin screen and sends
the site's admin address one e-mail per alert (options `pursae_license_alert`,
`pursae_license_alert_sent`). A Hub that cannot be reached changes nothing. Settings › License shows
a Pro/Beta badge next to *Valid*.

## What a site sends (statistics)

Statistics are on by default and can be switched off under Settings › License › "Send usage
statistics" (option `pursae_send_stats`). Off, the ping carries only `url`, `version` and `license`
(what updates need), with empty `plugins`/`themes`, and no events are queued. Only a development build with `PURSAE_DEV_HUB_URL=off` sends
nothing — it then has no marketplace, no license and no update notices either.

**Every request** carries the site's home URL and, if one is entered, its license key (above).

**The daily ping** (`POST /api/v1/sites/ping`, also run by Dashboard › Updates' cron event
`wpgo_hub_check`):

```json
{"url": "https://shop.example.com", "version": "0.2.0", "locale": "tr_TR", "os": "linux", "arch": "amd64",
 "license": "PRS-…", "theme": "twentytwentyfive",
 "plugins": [{"slug": "goocommerce", "version": "1.0.0", "active": true, "source": "native"},
             {"slug": "word-counter", "version": "1.0.0", "active": false, "source": "hub"}],
 "themes":  [{"slug": "twentytwentyfive", "version": "1.5", "active": true, "source": "native"}],
 "contacts": [{"email": "owner@shop.example.com", "name": "Owner", "role": "administrator"}]}
```

`contacts` are the users who asked for Pursae product update e-mails: users who can `edit_posts` and
opted in. It is off by default and opt-in only: the unticked "Email me Pursae product updates" box on
the install screen (the owner) or the "Pursae updates" switch in the user's own Users › Profile
(user meta `pursae_update_emails`, with the time of consent in `pursae_update_emails_consent_at`,
RFC 3339). An administrator editing another user can turn it off but not on, and the one-time admin
notice only invites the user to opt in. A `true` without a consent time (sites from before opt-in)
is not reported, and that user is invited again. Turning it off removes the consent time, and the
next ping no longer lists the user. The list is always sent (empty when no one gets updates) and does
not depend on the statistics switch. The Hub shows it on the site's page.

`plugins` lists every plugin, built-in (`native`), installed from the Hub (`hub`) or uploaded
(`upload`); `themes` lists the installed themes (bundled themes are `native`, library themes
installed from the Hub `hub`, uploaded packages `upload`).

**Events** (`POST /api/v1/sites/events`) record what happens on the site: `plugin_install`,
`plugin_update`, `plugin_activate`, `plugin_deactivate`, `plugin_delete` (built-in and package
plugins), `theme_install`, `theme_update`, `theme_activate`, `theme_delete`, `theme_preview` (Add
Themes › Live Preview), `demo_import` (a theme's demo content, with kind `theme`) and `core_update`. Each event is
`{"type", "kind", "slug", "version", "at"}`. They are queued in memory and sent in batches of at
most 100: every 30 seconds, as soon as 50 are waiting, and with the daily ping. While the Hub cannot
be reached up to 500 are kept (the oldest are dropped first); nothing is written to disk. Theme
preview sandboxes never send anything.

The site never sends user data, e-mail addresses, post content, titles or visitor information. The
public count of sites is `GET /api/v1/sites/count`; the list of sites is not public.

## Caching and limits

Catalog responses carry `Cache-Control: public, max-age=300`; sites cache the catalog for 12
hours and the index for the Updates screen. One ping per host per 10 minutes is accepted
(`429` otherwise); invalid or non-http(s) URLs are rejected with `400`.
