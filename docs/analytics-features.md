# Pursae Analytics: feature inventory (0.1)

Pursae Analytics is a native (compiled-in) Pursae plugin for privacy-friendly site statistics and
real-user Core Web Vitals, in the class of Plausible, Jetpack Stats and the MonsterInsights dashboard.
It is written from scratch for Pursae. Like every native plugin it is **off on a fresh install**
(`native_pursae_analytics`): while it is off nothing is printed on the site, the collector, the admin
screen and the REST route answer 404, and the menu entry is gone. Deactivating keeps the statistics.

## Files

| File | What it holds |
| --- | --- |
| `internal/server/analytics.go` | settings, on/off, plugin row, menu, routes, the footer script |
| `internal/server/analytics_collect.go` | `POST /wpgo-analytics/collect`: filters, visitor hash, sources, devices, countries |
| `internal/server/analytics_admin.go`, `web/admin/templates/pages/analytics.html` | the Analytics screen, `GET /pursae/v1/analytics` |
| `internal/db/migrations/0096_analytics.sql` | `analytics_events`, `analytics_salts` |
| `internal/server/analytics_test.go` | unit and integration tests |

## Privacy

- **No cookies, no local or session storage, no fingerprinting script.** Nothing is stored in the browser.
- **No IP address is stored.** A visitor is `SHA-256(day salt, IP address, user agent, host)` cut to 63
  bits; the host is the site's configured address, not the request's `Host` header (0.6.3+). The day's salt is random, shared by all instances through `analytics_salts`, and deleted the
  next day, after which nobody can recompute or link the hashes: a visitor is counted within a day and
  can never be followed across days or identified.
- Signed-in users are not counted (setting), Do Not Track and Global Privacy Control are respected
  (setting), excluded IP addresses and ranges are compared, never stored.
- No consent banner is needed for it, and it is not listed by Pursae Cookie (it sets no cookies).

## What is collected

A 2.3 KB script (1.2 KB gzipped) in `wp_footer`, the same HTML for every visitor so it works behind the
page cache, sends with `navigator.sendBeacon` (fallback `fetch` with `keepalive`):

| Hit | When | Data |
| --- | --- | --- |
| page view | on load | path, query (only `utm_source`, `utm_medium`, `utm_campaign` are kept), referrer host, viewport width |
| event | on the action | `outbound` (link to another host), `download` (common document, archive and media extensions), `form` (submit; the form's id, name or action), `404` (theme 404 view) |
| end of view | when the page is hidden or left | visible time, LCP, INP, CLS, FCP, TTFB |

The server adds the device class, browser and operating system (from the user agent), the source
(known services by name: Google, Bing, ChatGPT, Perplexity, Facebook, X, LinkedIn, …; other sites by host)
and the country when a proxy or CDN reports it (`CF-IPCountry`, `CloudFront-Viewer-Country`,
`X-Vercel-IP-Country`, `X-Country-Code`, `X-Geo-Country`). There is no GeoIP database.

Dropped: requests without a user agent, bots and monitors (user-agent patterns, `navigator.webdriver`),
prefetches, admin paths, and more than 300 requests a minute from one address. Page views are counted
at most 60 a minute per address, and beacons sent from another site's page (by `Origin`, else
`Referer`) are ignored (0.6.3+). Web Vitals can be
sampled (percent of page views).

## The Analytics screen

One page, periods Today (hourly), Last 7 days, Last 30 days, This month, Last 90 days, Last 12 months:

- Unique visitors, visits (a visit ends after 30 minutes without a view), page views, views per visit,
  bounce rate (one-view visits), visit duration (visible time) — each with its change against the
  previous period of the same length; current visitors (last 5 minutes).
- Chart of visitors and page views (inline SVG, no chart library).
- Top pages, entry pages, sources, UTM campaigns (campaign with source / medium), countries, devices,
  browsers, operating systems, goals, outbound links, downloads, 404 pages.
- Core Web Vitals at the 75th percentile with Google's thresholds (good / needs improvement / poor),
  and the pages with the slowest LCP (at least 3 measured views) with their LCP, INP and CLS.

Settings: who is counted, excluded addresses, goals, Web Vitals and sampling, retention (30–3650 days,
default 395; a daily job deletes older events), and "Delete all statistics".

## API

- `GET /pursae/v1/analytics?range=today|7d|30d|month|90d|12m` (`manage_options`, scope `settings`):
  summary, reports and Web Vitals as JSON.
- `/pursae/v1/options/analytics`: the settings, like the other built-in plugins.

## Not in 0.1

- ≈ **Unique visitors over several days** add up the daily uniques: the salt changes every day by
  design, so the same person on two days is two visitors (Plausible counts the same way).
- ✗ Filters and drill-down (e.g. sources of one page), custom events from themes, e-commerce revenue
  goals, CSV export, a shared public dashboard, e-mail reports.
- ✗ A bundled GeoIP database; countries come from the proxy or CDN.
