# Pursae Pixel: feature inventory (0.1)

Pursae Pixel is a native (compiled-in) Pursae plugin for ad and analytics tracking, in the class of
PixelYourSite Pro, Pixel Manager for WooCommerce and GTM4WP. It is written from scratch for Pursae; it
shares no code with those plugins. Like every native plugin it is **off on a fresh install**
(`native_pixelgo`). While it is off, nothing of it is printed on the site, and its admin menu, screens,
REST route, feed and assets answer 404. Deactivating keeps its settings and the delivery log.

Legend: **✓** implemented · **≈** implemented differently (reason given) · **✗** left out (reason given).

## Files

| File | What it holds |
| --- | --- |
| `internal/server/pixelgo.go` | settings model, defaults and sanitizer, platform definitions, on/off, plugin row, menu, routes |
| `internal/server/pixelgo_events.go` | the shared event model, platform event mapping, WooCommerce data (products, cart, order), hashing |
| `internal/server/pixelgo_front.go` | head tags (inert until consent), page configuration and `pixelgo.js` in the footer |
| `internal/server/pixelgo_capi.go` | `pixelgo/v1/event`, the four server-side senders, order hooks, delivery log, purge, test send |
| `internal/server/pixelgo_feed.go` | the product catalog feed |
| `internal/server/pixelgo_admin.go`, `web/admin/templates/pages/pixelgo.html` | the admin screens |
| `web/pixelgo/pixelgo.js` | the front-end runtime, served at `/wp-content/plugins/pixelgo/assets/` |
| `internal/db/migrations/0030_pixelgo.sql` | the `pixelgo_log` table |
| `internal/server/pixelgo_test.go`, `pixelgo_woo_test.go`, `pixelgo_gap_test.go`, `pixelgo_admin_test.go` | unit and integration tests |

Settings are one JSON option, `pixelgo_settings`. The page configuration is a JSON data block
(`#pixelgo-config`), not an inline script, so it needs no CSP exception.

## Platforms

| Platform | Browser tag | Server-side | Multiple IDs |
| --- | --- | --- | --- |
| Meta Pixel (Facebook, Instagram) | ✓ | ✓ Conversions API | ✓ |
| Google Analytics 4 | ✓ | ✓ Measurement Protocol, **Purchase only** (see below) | ✓ |
| Google Ads | ✓ conversions by label, remarketing events | ✗ server-side; ✓ browser-side enhanced conversions (the Purchase sets the hashed email, phone and name with `gtag('set','user_data')` before the conversion) | ✓ |
| TikTok | ✓ | ✓ Events API | ✓ |
| Pinterest | ✓ | ✓ Conversions API (needs the ad account id) | ✓ |
| Microsoft Advertising (UET) | ✓ | ✗ | one UET instance per tag ID, every event goes to each |
| Google Tag Manager (data layer) | ✓ container snippet; every event is pushed to `dataLayer` in the GA4 e-commerce schema (`{event, ecommerce: {items, value, currency, transaction_id}}`, cleared with `{ecommerce: null}` first) | ✗ | first ID only; when the same container is also added in Pursae Cookie › Scripts it is loaded once (from Pursae Cookie) and Pursae Pixel only pushes to `dataLayer` |

## Events

One event model for every platform: canonical names (the Meta standard names, plus ViewCategory,
RemoveFromCart, ViewCart, Download, OutboundClick, Comment, Scroll, TimeOnPage, VideoPlay, VideoComplete)
and canonical parameters (`value`, `currency`, `items`, `content_name`, `content_category`,
`search_string`, `order_id`, `num_items`, `tax`, `shipping`, `payment_type`, `extra`). The mapping to each
platform's own names lives in `pixelEventMap` and is sent to `pixelgo.js`, so browser and server agree.

- **General:** page view, search, scroll depth, time on page, `tel:` and `mailto:` clicks, outbound links,
  file downloads (extension list), form submissions (Lead), comments, registrations, HTML5 video. ✓
  YouTube and Vimeo embeds: `enablejsapi=1` is added and the player's postMessage state feeds VideoPlay and
  VideoComplete; iframes inserted later (Pursae Cookie after consent) are picked up. ✓
- **WooCommerce (Pursae Commerce):** ViewContent, ViewCategory, AddToCart (single product, loop buttons,
  AJAX), RemoveFromCart, ViewCart, InitiateCheckout, AddPaymentInfo, Purchase. ✓
  Product ids can be the product id or SKU, with a prefix and suffix, matching the catalog feed. Purchase
  value can be total, subtotal with tax or net. Purchase fires once per order and browser.
  A variable product is tracked by its parent id; the loop's AddToCart carries ids and quantity but no price
  (the loop markup has none). ≈
- **Custom events:** name, trigger (page visit, click on a CSS selector, scroll percent, time on page, form
  submit), URL filter, fixed value and currency, `key=value` parameters (`{url}`, `{title}`, `{referrer}`),
  platform selection, once per session. ✓
- **Advanced matching:** hashed customer data (e-mail, phone, name, address) on the order-received page and
  in server-side events, only when the setting is on. ✓
- **Attribution:** UTM parameters and click ids (`fbclid`, `gclid`, `ttclid`, `epik`, `msclkid`) are kept in a
  first-party cookie (`pixelgo_attr`, 90 days by default), added to events as parameters and used by the
  server-side senders. The cookie is only set when a platform is allowed. ✓

## Consent

With Pursae Cookie active and **Require consent** on (the default), every tag is printed inert
(`gooikeInert`) in its platform's category (GA4: statistics; the rest: marketing) and Pursae Cookie runs it
when the visitor accepts. `pixelgo.js` queues events until their platform is loaded and consent is given,
and a platform that was refused never receives one. A visitor who accepts later still gets the events of the
page they are on. The server-side route reads the consent cookie again. A completed payment is sent
server-side only when the checkout request recorded consent for that platform (stored with the order). ✓

With "Require consent" on and Pursae Cookie inactive no tag is printed at all (safe default); the Dashboard
says so, with an **Activate Pursae Cookie** button (one click, nonce-checked, for users who can activate
plugins). Switch "Require consent" off to load the tags directly when another tool handles consent.

## Server-side delivery and deduplication

Browser events and their server copies share one event id (`pixelgo.js` forwards each event to
`POST /wp-json/pixelgo/v1/event`; the server does not wait for the browser tag, so it survives ad blockers).
Meta, TikTok and Pinterest deduplicate on that id. A completed payment (`payment_complete`) sends the
Purchase with the id `purchase_<order id>`, the same id the order-received page uses, so off-site payments
are counted even when the customer never returns. A webhook retry sends nothing twice.

Delivery is retried: a platform that answers 429, 5xx or not at all gets the event again after 2 s, 15 s and
90 s (the log row of a failure says "After N attempts"). A Purchase is recorded on the order per platform
(`_pixelgo_sent`: platform → time) only after that platform answered 2xx, so a failed one is not lost: the
hourly sweep sends again the Purchases of the last 24 hours that a platform has not accepted (also after a
restart in the middle of a delivery), and the Delivery Log has a **Retry** button on failed Purchase rows.

GA4's Measurement Protocol cannot deduplicate by event id, so it only gets Purchase (deduplicated by
`transaction_id`) and, when an order GA4 recorded is refunded, the `refund` event (`transaction_id`, the refunded
value, the refunded items of a partial refund). Meta, TikTok and Pinterest have no refund event, so their reports
keep the order's revenue; a refund is not an event the browser may send. The route accepts only canonical events and the enabled custom events, from the same
origin, rate-limited per address; parameters are whitelisted and bounded. Hosts are fixed, redirects are not
followed, access tokens go in request headers or bodies (Google asks for the secret in the query, so that URL
is never logged), are stored encrypted with a key derived from the server secret, are never shown again after
saving, and never reach the page. Every delivery is logged
(platform, event, id, status, HTTP code, a short message) with a retention in days. ✓

Without a `_fbc` cookie the server builds Meta's click id from the `fbclid` of the attribution cookie and the time
the click was first seen (`fbts` in `pixelgo_attr`), so the value stays the same on every request.

## Product catalog feed

`/pixelgo-feed/<token>` serves a Google Merchant / Meta Commerce Manager RSS 2.0 feed (id, title, description,
link, image, availability, price and sale price, condition, brand, MPN, product type, Google category). One item
per product (a variable product at its lowest price). With **Use the ID of the chosen variation** (Events screen)
every published variation is an item of its own with `item_group_id` (the variable product's id), its own price,
stock, SKU, link, image and color/size/material/pattern, and the variable product is not listed itself;
cart, checkout and purchase events then use the variation id too (the add-to-cart button of a product page reads the
chosen `variation_id` from the form and sends that variation's id and price; the page carries a
variation-id map in the product event data). The feed is read in pages of
500 products and streamed (no product limit) and cached for 5 minutes; hidden, password-protected and unpublished products are
left out. The token is the only access control and can be regenerated. ✓

## Other

- Exclude roles from tracking (administrators by default) ✓
- Debug mode logs every dispatch to the browser console ✓
- `window.pixelgo.track(name, params, opts)` and the `pixelgo:event` DOM event for custom code ✓
- Settings export (tokens left out) and import (stored tokens kept) ✓
- Test event per server-side platform, using the platform's test code (GA4: the validation endpoint) ✓

## Not in 0.1

Google Ads server-side events, Microsoft Advertising server-side events,
Snapchat, Reddit and Twitter/X
tags, WooCommerce subscriptions and bookings specific events, automatic page-view deduplication for
single-page apps.
