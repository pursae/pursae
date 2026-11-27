# Payment method logos

One file per payment gateway id: `<gateway id>.svg` (for example `goo_stripe.svg`, `goo_est_isbank.svg`), or
`<gateway id>.png` where the company publishes no official SVG. The lookup order is `.svg`, `.png`, then
`generic.svg` (the fallback for ids without a file). The ids are the gateway ids registered in
`internal/server`; the image is shown in Settings > Payments, on the checkout next to the payment
method and in the REST API (`icon` of `wc/v3/payment_gateways`). Served publicly at
`/wp-content/plugins/goocommerce/assets/images/payment-gateways/<id>.svg`.

The provider logos below are the companies' own logos. They are trademarks of their owners and are used
only to identify the payment provider. The offline, test and generic icons (`bacs`, `cheque`, `cod`,
`goo_test`, `generic`) are Pursae's own drawings. `goo_est_teb.png` is the only PNG: the TEB website
publishes no SVG of its logo.

## Rules for replacing a file

- Use the company's current primary full-colour logo for light backgrounds, from the company's own brand
  page or website, or from Wikimedia Commons. Never redraw, recolour or simplify a logo.
- Well-formed XML, a single root `<svg>` with a `viewBox`, no fixed `width`/`height` on the root.
- No `<script>`, `<foreignObject>`, `<iframe>`, `<image>` (no raster), `<style>` with `@import` or
  `url(http...)`, `on*` attributes, `href`/`xlink:href` other than an internal `#id`, `<!DOCTYPE>` or
  entity declarations, processing instructions other than the xml declaration, editor metadata
  (`<metadata>`, sodipodi/inkscape), or comments.
- No `<use>` and no `@font-face` (the safety test rejects them; inline the referenced shape instead).
- At most 40 KB. Update the table below.
- A PNG is allowed only where no official SVG exists: the company's own colour logo for light backgrounds,
  re-encoded without metadata (for example `sips -s format png in.png --out out.png`), at most 60 KB and
  600 px wide (scale down to 480 px wide, keeping proportions, when larger). It must start with the PNG
  signature and decode with `image/png`. Delete any `.svg` of the same id so the route is unambiguous.
  `TestGooGatewayLogosAreSafeSVG` and `TestGooGatewayLogosPNGLimits` enforce this.

## Sources (all fetched 2026-11-27)

Commons files carry the license "Public domain" with the Commons restriction tag "trademarked".

| File | Company | Page | File URL | License / usage statement |
|---|---|---|---|---|
| goo_paytr.svg | PayTR | https://www.paytr.com | https://www.paytr.com/wp-content/uploads/logo.svg | The site's own header logo; no license statement given |
| goo_iyzico.svg | iyzico | https://commons.wikimedia.org/wiki/File:Iyzico_logo.svg | https://upload.wikimedia.org/wikipedia/commons/f/f8/Iyzico_logo.svg | Public domain; trademark notice: yes |
| goo_sipay.svg | Sipay | https://sipay.com.tr | https://sipay.com.tr/wp-content/uploads/2024/10/logo_new.svg | The site's own logo; no license statement given |
| goo_paypal.svg | PayPal | https://commons.wikimedia.org/wiki/File:PayPal_2024.svg | https://upload.wikimedia.org/wikipedia/commons/c/c6/PayPal_2024.svg | Public domain; trademark notice: yes |
| goo_stripe.svg | Stripe | https://commons.wikimedia.org/wiki/File:Stripe_Logo,_revised_2016.svg | https://upload.wikimedia.org/wikipedia/commons/b/ba/Stripe_Logo%2C_revised_2016.svg | Public domain; trademark notice: yes |
| goo_est_isbank.svg | Türkiye İş Bankası | https://commons.wikimedia.org/wiki/File:T%C3%BCrkiye_%C4%B0%C5%9F_Bankas%C4%B1_logo.svg | https://upload.wikimedia.org/wikipedia/commons/2/24/T%C3%BCrkiye_%C4%B0%C5%9F_Bankas%C4%B1_logo.svg | Public domain; trademark notice: yes |
| goo_est_ziraat.svg | Ziraat Bankası | https://commons.wikimedia.org/wiki/File:Ziraat_Bankas%C4%B1_2025_Logo.svg | https://upload.wikimedia.org/wikipedia/commons/5/5f/Ziraat_Bankas%C4%B1_2025_Logo.svg | Public domain; trademark notice: yes |
| goo_est_halkbank.svg | Halkbank | https://commons.wikimedia.org/wiki/File:Halkbank_logo.svg | https://upload.wikimedia.org/wikipedia/commons/3/39/Halkbank_logo.svg | Public domain; trademark notice: yes |
| goo_est_sekerbank.svg | Şekerbank | https://commons.wikimedia.org/wiki/File:%C5%9Eekerbank_logo.svg | https://upload.wikimedia.org/wikipedia/commons/e/eb/%C5%9Eekerbank_logo.svg | Public domain; trademark notice: yes |
| goo_akbank.svg, goo_est_akbank.svg | Akbank | https://commons.wikimedia.org/wiki/File:Akbank_logo.svg | https://upload.wikimedia.org/wikipedia/commons/7/7b/Akbank_logo.svg | Public domain; trademark notice: yes |
| goo_garanti.svg | Garanti BBVA | https://commons.wikimedia.org/wiki/File:Garanti_Bankas%C4%B1_Logo.svg | https://upload.wikimedia.org/wikipedia/commons/1/12/Garanti_Bankas%C4%B1_Logo.svg | Public domain; trademark notice: yes |
| goo_qnb.svg | QNB (QNB group logo, used by QNB Türkiye) | https://commons.wikimedia.org/wiki/File:QNB_Logo.svg | https://upload.wikimedia.org/wikipedia/commons/8/8f/QNB_Logo.svg | Public domain; trademark notice: yes |
| goo_vakifbank.svg | VakıfBank | https://commons.wikimedia.org/wiki/File:Vak%C4%B1fbank_logo.svg | https://upload.wikimedia.org/wikipedia/commons/0/04/Vak%C4%B1fbank_logo.svg | Public domain; trademark notice: yes |
| goo_est_teb.png | TEB (Türk Ekonomi Bankası) | https://www.teb.com.tr | https://www.teb.com.tr/images/TEB/fLogo.png (86 x 40 px, 4 KB) | The site's own footer logo; no usage statement on the site (no SVG published; the header logo is a JPG) |
