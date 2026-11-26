# Pursae Commerce: overview

Pursae Commerce is a native (compiled-in) Pursae plugin that clones **WooCommerce**: products, cart,
checkout, tax, shipping, coupons, orders, payment gateways, e-mails and both REST APIs, with the premium
extensions built in as modules. Like every native plugin it is listed on **Plugins**, starts **off**
(option `native_goocommerce`), and while it is off its screens answer 404 and its REST routes answer
`rest_no_route`. This page is an overview for theme and API developers; it is not a feature-by-feature
inventory like the other built-in plugin pages.

## Data

- Products are posts of type `product` (and `product_variation`) plus a row in `goo_products`.
- Orders are HPOS-like: `goo_orders`, `goo_order_items`, `goo_order_notes`.
- Other tables: `goo_attributes`, `goo_coupons`, `goo_tax_rates`, `goo_shipping_zones`,
  `goo_shipping_methods`, `goo_sessions`, `goo_download_permissions`, `goo_api_keys`, `goo_webhooks`.
  Migrations: `0015_goocommerce.sql` and the `0016_goo_*` files of the modules.
- Personal data requests (Tools › Export / Erase Personal Data) cover orders, downloads, customer
  billing / shipping fields, saved payment methods, gift cards, wishlists, points, subscriptions and
  memberships; see "Personal data requests" in docs/GUIDE.md for what is erased and what is kept.

## Modules

Fourteen modules sit on **Pursae Commerce › Extensions**, all off by default and grouped by category:

| Category | Modules |
|----------|---------|
| Merchandising | Product Bundles, Pre-Orders, Product Add-Ons, Product Compare, Quick View, Dynamic Pricing |
| Store management | Bookings, PDF Invoices & Packing Slips |
| Payments & checkout | Subscriptions, Gift Cards |
| Shipping & delivery | Table Rate Shipping |
| Customer engagement | Memberships, Points and Rewards, Wishlists |

A module that is off changes nothing: every one of its hooks checks that the module is on.

**Gift Cards** disable the cards an order sold when it is cancelled or refunded (or when a refund returns the gift card line; reactivating the order enables them again), remind recipients a week before a card with a balance expires, and expose the Store API (`extensions.gift_cards` fields on `cart/add-item`: `amount`, `custom_amount`, `to`, `to_name`, `from`, `message`, `delivery`; `cart/apply-gift-card`, `cart/remove-gift-card`; applied codes in `extensions.gift_cards`) and `wc/v3/gift-cards` (list, read, `PUT` status / `date_expires`, `POST .../adjust`). Designs: five colour themes, each optionally with a Media Library image (Gift Cards settings › Designs); the buyer picks one the product enables. The delivery e-mail shows the design, attaches the card as a PDF and links a signed print-at-home page with a PDF download.

**Subscriptions** also expose `wc/v3/subscriptions` (list, create, read, update, delete with `force=true`, batch,
`/{id}/notes`, `/{id}/orders`; `manage_woocommerce`, 404 while the module is off) and the webhook topics
`subscription.created`, `subscription.updated` and `subscription.deleted`. Customers change their billing and shipping
address and switch the payment method (a gateway that renews without a stored card, Stripe — the new card is entered on Stripe's page —,
or any gateway when manual renewals are accepted) on the My Account subscription page, and resubscribe to a cancelled or expired one (its products go back
into the cart). Store managers add a subscription by hand (Subscriptions › Add Subscription), edit addresses and line
prices / quantities, add or remove line items (one line always stays), change the customer, and use "Process renewal" on the Edit Subscription screen.

**Product Add-Ons details.** File uploads (`/?goo-ajax=addon_upload`) need the nonce printed on the product page
(`goo-addon-upload`), are limited to 20 stored files per client address per 10 minutes, and the daily job deletes
uploads older than 24 hours that no cart session or order item references. A field can be shown conditionally
(`condition_field` / `condition_value`: another field's title and the option label or text that reveals it; hidden
fields are neither required nor stored). Date pickers take `date_min_days` (earliest = today + N days) and
`date_max`, enforced in the HTML and on the server. The Store API product `extensions.product_addons` lists the
fields (name, label, type, required, options, condition, date limits); `POST /cart/add-item` reads the values from
`extensions.product_addons` as `{<field name>: value | [values]}`.

## Storefront and themes

Store URLs render inside the active theme the way WooCommerce's theme-compatibility mode does: the main
query is a page whose content is the store markup (a synthetic page for products and archives, the real
Cart, Checkout and My account pages for those). The markup keeps WooCommerce's templates, classes and
field names, so themes made for WooCommerce style it. My account › Account details asks for the current
password to change the e-mail address (the attempts count toward the login throttle), and the new
address applies once the confirmation link mailed to it is followed. Shortcodes are named `goocommerce_*` and every one
also answers to its `woocommerce_*` alias.

To tell the store that a theme is made for it, declare support in `theme.json`:

```json
"supports": { "woocommerce": true }
```

A theme that does so prints the store titles itself through the store templates instead of the page
title, and is listed under **Pursae Commerce › Store Themes** (the library's store themes, plus installed packages that declare it; a library that cannot be reached shows a notice). Practical rules from the bundled store
themes (`library/themes`, published to Pursae Manager; `category: shop`):

- scope the store CSS under `#page` so it wins over `goocommerce.css`;
- the checkout lays its rows on a grid, so reset the width and float of `.form-row-first` and
  `.form-row-last`;
- store asset URLs carry a content hash, so a changed stylesheet is picked up without a version bump.

The store markup is shown in the site language (with Pursae Multi Language, the language of the
page), like WooCommerce loading its text domain on the front end: the WordPress.org WooCommerce
language pack, downloaded with the site language, translates it, and Pursae's own catalog
(`web/languages`) covers the texts that differ. Only the store's own markup is translated; product
names, descriptions, reviews and the theme around it stay as written. E-mails and the optional
modules' front-end texts are still English.

A password reset requested for an account with the `customer` role sends the store's "Reset password"
e-mail (Settings > Emails) when it is enabled; every other account gets the core "[Site] Password Reset" message.

A store theme may also ship demo content with a `goocommerce` section in `demo/demo.json` (products,
reviews, coupons, shipping), imported with the rest of the demo. The package format is in
`docs/theme-package.md`.

## REST APIs

| Namespace | What | Authentication |
|-----------|------|----------------|
| `wc/store/v1` | The Store API: public cart, checkout and catalog | The storefront session with a `Nonce` header, or a `Cart-Token` |
| `wc/v3` | The management API: products, orders, customers, coupons, settings | Logged-in cookie with `X-WP-Nonce`, an application password, or a consumer key and secret from `goo_api_keys` |

Both follow WooCommerce's JSON shapes, query parameters, error codes and headers, so clients written
for WooCommerce work unchanged.

## Extension points

The store's actions (`product_saved`, `low_stock`, `no_stock`, `order_created`, `order_status_changed`,
`payment_complete`, `order_refunded`, `customer_created`, `cart_emptied`) and product types are Go
registrations (`gooOn`, `gooRegisterProductType` in `internal/server/goo_hooks.go`) used by the
compiled-in modules. Outside the binary, integrate through the REST APIs and webhooks (`goo_webhooks`).
Webhook deliveries run on a small pool of background workers; a failed delivery is tried twice more
(after 10 s and 1 min) with the same `X-WC-Webhook-Delivery-ID`, then counts as one failure, and a
webhook is disabled after 5 consecutive failed deliveries. Deliveries still waiting are lost on restart.

Webhook topics: `coupon|customer|order|product` `.created/.updated/.deleted` (+ `.restored`), the module
topics `subscription.created|updated|deleted`, `booking.created|updated` and `membership.created|updated`
(offered while the module is on; in-cart bookings are not delivered), and `action.<hook>`, which
delivers `{"action": hook, "arg": id}` when the internal action of that name fires (with or without
the `woocommerce_` prefix, for example `action.woocommerce_order_status_changed`). Every attempt
is logged in `goo_webhook_deliveries` (time, duration, HTTP code, first 1 KB of the response or the
transport error; the last 50 per webhook), shown on the webhook edit screen with a "Send test
delivery" button; REST: `GET /wc/v3/webhooks/{id}/deliveries`, `POST /wc/v3/webhooks/{id}/ping`.

Module REST (wc/v3, `manage_woocommerce`, 404 while the module is off): `/subscriptions`, `/bookings`,
`/memberships` (user memberships) and `/gift-cards` with list (filters `status`, `customer`, ...; an
unknown `status` is a 400), read and update (status, dates); subscriptions and gift cards have the
fuller APIs described under Modules. The Store API add-item body passes `extensions.<namespace>` objects
(`product_addons`, `bookings`, `gift_cards`, ...) to the add-to-cart validation as the posted form.

## Orders under concurrency

A status change is serialized per order and starts from the status stored in the database, so a
payment and the hourly "cancel unpaid orders" job cannot both win (the job only cancels an order that
is still pending). Saving an order loaded earlier does not undo a status change or the stock / coupon
bookkeeping made meanwhile. Stock and coupon uses are taken with conditional updates when an order
goes on hold or is paid: when another order got the last unit or the coupon's last use first, a
payment puts the order on hold with a note ("Not enough units of … are available", "Usage limit for
coupon … has been reached") instead of overselling; products that allow backorders go below zero as
before, and a status set by a person in the admin or through `wc/v3` is applied like WooCommerce does.
Placing an order reserves its stock while the customer pays (WooCommerce's `wc_reserve_stock_for_order`
and `wc_reserved_stock`, table `goo_reserved_stock`) for **Hold stock (minutes)** (Settings › Products ›
Inventory; 0 turns it off): the product rows are locked while the reservation is checked and written, so
of two checkouts for the last unit only one reaches the payment page and the other is told it is out of
stock. A later checkout by the same visitor takes over the hold on the products in that cart, in the same
transaction, and leaves a hold on any other product alone. A payment cannot take units another unpaid order holds. The hold ends when the order is paid or
put on hold (the stock is reduced instead), cancelled or failed, or when the time runs out (expired holds
count for nothing; the hourly job deletes them and cancels the unpaid order). Products that do not manage
stock or allow backorders are not reserved.

## Known limits

- Payment methods: the built-in ones are direct bank transfer (BACS), check, cash on delivery, Stripe
  and the Test payments gateway. Stripe (Payments › Stripe: test/live secret keys and webhook signing
  secrets, kept out of the form once saved) takes cards on Stripe-hosted Checkout, so no card data
  reaches the server. The Checkout page expires after the hold-stock time (between 30 minutes and
  24 hours; one hour when hold stock is off) and every Checkout page opened for the order (one per payment attempt) is closed when the order is cancelled. Its webhook is `/wc-api/goo_stripe` (Stripe-Signature HMAC-SHA256, 5-minute
  timestamp tolerance, each event id handled once); order updates are idempotent, refunds from the
  order screen go through the Stripe API (Idempotency-Key per refund), saved cards appear under
  My Account › Payment methods (a provider reference only), and subscription renewals are charged
  off-session with the saved card (a processing payment puts the renewal on hold until the webhook
  settles it). iyzico is not built yet. BACS, check and cash on delivery reject automatic refunds, so
  refund those by hand and record it on the order. Bookings do not support refunds either.
- Geolocate uses the country from the request headers when available (CDN or proxy header) and falls
  back to the store base country; there is no GeoIP database lookup.

## Blocks

Server-rendered blocks for the block editor, reusing the shortcode renderers (same markup, nothing
rendered while the store is off): `goocommerce/products` (attributes `columns`, `limit`, `category`,
`orderby`, `order`, `onSale`, `visibility`, `ids`, `tag`), `goocommerce/featured-product` (`productId`),
`goocommerce/product-categories` (`limit`, `hideEmpty`, `parent`, `orderby`), `goocommerce/mini-cart` and
`goocommerce/product-search` (`title`). They render on the front end from block markup such as
`<!-- wp:goocommerce/products {"columns":3,"limit":6} /-->`; the editor inserter entries are not
registered yet (insert them with the code editor or a Custom HTML block holding the comment).

### Cart and Checkout blocks

`woocommerce/cart` and `woocommerce/checkout` (aliases `goocommerce/cart`, `goocommerce/checkout`) are in the
block editor's inserter. They are hybrid: the editor sets their attributes, and the front end renders the
cart and checkout pages of `[goocommerce_cart]` / `[goocommerce_checkout]` on the server (no Store API
React app, the same forms, scripts and validation). Attributes:

- Cart: `showCrossSells`, `showCouponForm` (booleans, default on), `emptyCartMessage`.
- Checkout: `showLoginReminder`, `showCouponForm`, `showOrderNotes`, `showReturnToCart` (booleans, default on),
  `billingTitle`, `additionalTitle`, `orderTitle` (headings, empty for the default ones), and the fields
  `showCompanyField` / `requireCompanyField`, `showApartmentField` / `requireApartmentField`,
  `showPhoneField` / `requirePhoneField`. The field attributes of the block on the store's checkout page
  replace the `checkout_*_field` options for the form and its validation (My account's address forms too);
  a field without attributes keeps its option.

Pages imported from WordPress keep WooCommerce's block markup and its inner blocks. The inner blocks are
read as settings: without `woocommerce/cart-cross-sells-block`, `woocommerce/cart-order-summary-coupon-form-block`,
`woocommerce/checkout-order-note-block` or `woocommerce/checkout-order-summary-coupon-form-block` the
section is not shown; the `title` of `woocommerce/checkout-billing-address-block` and
`woocommerce/checkout-additional-information-block` and `showReturnToCart` of
`woocommerce/checkout-actions-block` are used; the Empty cart block's own blocks show while the cart is
empty. Inner blocks Pursae does not render are ignored. The block's own attributes win over its inner
blocks. The Web Builder has the same cart and checkout as its Cart page and Checkout page widgets.

### Bookings: calendar and overnight blocks

Booking e-mails and My Account link to a signed `.ics` download (`/?goo-ajax=booking_ics`); Bookings > Calendar shows a private ICS feed URL (`booking_feed`) for Google/Apple/Outlook. Hour/minute products can allow blocks past midnight ("Allow blocks past midnight"). Booking e-mails (confirmed, reminder, new, cancelled) also attach the `.ics` file. Cancelling a paid booking refunds its order line through the order refund flow when the payment method supports refunds and the Bookings setting "Refund cancelled bookings" is on (default); otherwise the order gets a "refund required" note and the admin cancellation e-mail a refund line.

Bookings REST (`wc-bookings/v1`): `GET /products/slots?product_id=&date=` (public), `GET /bookings` (filters `product_id`, `customer_id`, `order_id`, `status`, `after`, `before`), `GET|PUT /bookings/{id}` (status changes use the admin flow); all but slots need `manage_woocommerce`. The Store API `cart/add-item` takes the booking form fields in `booking` (or `extensions.bookings`), e.g. `{"id": 12, "booking": {"wc_bookings_field_start_date": "2026-12-01"}}`. The daily reminder job catches up missed runs: every unreminded paid/confirmed booking that has not started and falls on or before today + N days is reminded once.

## Memberships: lifecycle, e-mails and drip

- A cancelled or refunded order cancels the memberships it granted; a subscription status change pauses, resumes, cancels or expires the memberships its orders granted. A partial refund cancels a membership only when the line of the product that granted it is refunded in full.
- E-mails (Goocommerce > Settings > Emails): Membership welcome, Membership ending soon (days before the end set by Memberships > Ending soon reminder, 0 = off) and Membership ended. Each is sent once per membership and end date.
- Plan > Restrict Content > Drip schedule (`post ID:days, tTERMID:days`) opens restricted content N days after the membership start; members see "available on <date>" until then.
- The comments of restricted content are restricted too: non-members do not see them on the page, in the comment feeds, in the Latest Comments block or through `wp/v2/comments`.
- The plan form checks that every product, post and term ID exists and has search boxes that add IDs to the fields.

Some payment methods do not support refunds; their cancelled paid bookings get a "refund required" order note.

## Analytics

The `wc-analytics` REST namespace serves `reports/revenue/stats`, `reports/orders/stats` (totals and per-interval subtotals; `after`, `before`, `interval=hour|day|week|month|quarter|year`) and `reports/products` for external dashboards, authenticated like wc/v3 (consumer key or core auth, `view_woocommerce_reports`). WooCommerce > Settings > Emails > Store summary enables a weekly e-mail (job `goocommerce_weekly`) with last week's orders, sales, refunds and AOV to the stock notification recipient(s).

Analytics reports (Overview, Products, Revenue, Orders, Variations, Categories, Coupons, Taxes, Downloads, Stock, Customers) and
their CSV downloads carry every row (no server-side cap); the screen lists the first 1,000 rows and says so. Products, Variations
and Categories subtract refunds, matching the Overview. Revenue computes all days with grouped queries. Subscriptions (active,
MRR, new, cancelled, churn, retention, renewal and sign-up revenue, paid and failed renewals), Bookings (bookings, booked hours, occupancy against the availability rules and per-block capacity, cancellations, revenue) and Gift Cards (sold, redeemed,
outstanding balance) tabs appear in the Analytics menu while the module is switched on.

## CSV import and export

- **Products** (Products › Export / Import) use WooCommerce's column names. The exporter has an "Export custom meta" option:
  every product meta key (subscription, booking, gift card and add-on settings, ...) is written as a `Meta: <key>` column
  (strings as they are, numbers, booleans, lists and objects as JSON); the importer maps `Meta: ` columns back (an empty cell
  leaves the stored value alone).
- The importer downloads remote image URLs (http/https, public addresses only) into the Media Library; the source URL is kept
  in the attachment meta `_wc_attachment_source_url`, so the same URL is downloaded once. A URL that cannot be fetched or is
  not an image fails its row.
- The import runs in batches of 50 rows (`step=batch`, JSON progress; the mapping screen shows a progress bar). Without
  JavaScript the form still imports in one request. Abandoned uploads (`goo-import-*.csv` and their state files in the temp
  directory) are deleted after 24 hours by the daily `goo_import_cleanup` event.
- **Orders**, **Coupons** and **Subscriptions** lists have an "Export CSV" link that exports every row matching the list's
  filters (status view, search, month, customer); Customers has "Download". The links carry the store nonce.

## WordPress import (WXR)

Tools › Import (and `pursae import`) reads WooCommerce products from a WordPress export file (Tools › Export on
the WooCommerce site, "All content" or "Products"). Import the file with "Download and import file attachments"
checked so the product images come along. The store does not have to be active; the products appear once it is.

- **Product types**: simple, variable (with its `product_variation` children), grouped (`_children`) and external
  (`_product_url`, `_button_text`), read from the `product_type` term. Other types (subscriptions, bundles,
  bookings, ... of other plugins) are imported as simple products and named in the report.
- **Product data** from the post meta: regular and sale price, sale schedule (`_sale_price_dates_from` / `_to`),
  SKU, stock management, quantity, stock status, backorders, low stock amount, sold individually, weight and
  dimensions, tax status and class, virtual, downloadable with the downloadable files (`_downloadable_files`;
  disabled files are left out; files on the old site's uploads that the import fetched point at the new copies,
  others keep their URL), download limit and expiry, purchase note.
- **Attributes**: `_product_attributes` and `_default_attributes`. A taxonomy attribute (`pa_*`) becomes a global
  attribute (created when missing; the file does not carry its label, so it is named after its slug, e.g.
  `pa_shoe-size` → "Shoe size", and can be renamed under Products › Attributes) with the product's terms as options;
  a custom attribute keeps its `A | B` values. Variations take their `attribute_*` values. Attribute slugs here are
  ASCII: a WooCommerce slug with accents is transliterated (`pa_größe` → `pa_grosse`), and one in another script
  (`pa_размер`) gets a generated slug (`pa_attr-…`, keeping the name "Размер") unless an attribute of that name
  already exists; its terms and the variations follow it.
- **Terms**: `product_cat` (with the hierarchy), `product_tag`, `product_shipping_class` and the `pa_*` terms. A
  product without a category goes to the default product category (Uncategorized unless changed). `product_visibility` sets "featured" and the catalog visibility
  (`exclude-from-catalog` / `exclude-from-search`; the older `_featured` / `_visibility` meta is read too). Category
  term meta (thumbnail, display type, order) is not imported.
- **Images**: `_thumbnail_id` (product and variation image) and `_product_image_gallery` are mapped to the imported
  attachments, which are attached to their product. Links in descriptions to the old uploads are rewritten.
- **Links**: upsells (`_upsell_ids`), cross-sells (`_crosssell_ids`) and grouped children are remapped to the
  imported products; ids of products that are not in the file are dropped.
- **Reviews**: product comments come with their comment meta (`rating`, `verified`), and the average rating is
  recalculated. `total_sales` and the stored rating counters are not copied.
- The purchase note and variation descriptions are filtered like the descriptions when the importing user may not
  post unfiltered HTML.
- Other post meta (custom fields, SEO plugins) is kept on the product post like for posts; the WooCommerce keys
  above are read into the product data instead of being copied.
- **Not imported**: orders (`shop_order`), refunds, coupons (`shop_coupon`) and subscriptions are counted and
  reported ("Skipped N orders / M coupons"). Customers are not in a WXR file (only post authors are). A WooCommerce
  site with High-Performance Order Storage does not export orders to WXR at all.
- **Running it again**: a product with the same slug and name is skipped ("Product … already exists"); a product
  without a slug (WordPress leaves it empty on drafts and pending products) is matched by name and date, as the
  WordPress importer does. Its variations
  that have the same attributes are skipped too; a variation that is missing (an import that stopped half way) is
  added. A SKU already used by another product here is left empty and named in the report.
