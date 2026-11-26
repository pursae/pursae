# Pursae Contact Form — Contact Form 7 feature inventory

Pursae Contact Form is Pursae's native (compiled-in) clone of **Contact Form 7** (studied at CF7 6.2.3,
`rocklobster-in/contact-form-7`, plus contactform7.com docs). Like Classic Editor, Pursae Cache and Pursae Web Builder it is
listed under Plugins, **off on a fresh install**, and while off it adds nothing: no menu, no screens (404/403), no
REST namespace, no shortcode, no block, no front-end assets.

Legend: ✅ implemented · ◐ implemented differently / partly · ✗ left out (with the reason).

## 1. Data model

| CF7 | Pursae Contact Form |
|---|---|
| Custom post type `wpcf7_contact_form` | ✅ same post type (non-public `content_types` row created on activation) |
| Post meta `_form`, `_mail`, `_mail_2`, `_messages`, `_additional_settings`, `_locale`, `_hash` | ✅ same keys in the post's `meta` jsonb; `post_content` keeps the form template too |
| Default form, Mail, Mail (2), messages templates (`WPCF7_ContactFormTemplate`) | ✅ identical defaults |
| "Contact form 1" created on first activation | ✅ |
| Flamingo storage of submissions | ✅ built in: table `gontact_entries` (accepted submissions only, stored before the mail is queued); Contact › Entries lists, filters, searches, shows, deletes and exports them as CSV (formula-safe); retention in days (hourly event); `do_not_store: true` turns it off per form; Tools › Export / Erase Personal Data export and delete the entries with the person's address in an e-mail field (email tags and the fields the mails take their sender, Reply-To or Mail (2) recipient from, recorded per entry) or sent by their account while signed in |
| Capabilities `wpcf7_edit_contact_form(s)`, `wpcf7_read_contact_forms`, `wpcf7_delete_contact_form`, `wpcf7_manage_integration`, `wpcf7_submit` | ◐ mapped like CF7's `map_meta_cap` defaults: edit/delete → `publish_pages`, read → `edit_posts`, integration → `manage_options`, submit → `read` |

## 2. Admin screens (menu **Contact**, `dashicons-email`, after Comments)

| CF7 | Pursae Contact Form |
|---|---|
| Contact › Contact Forms (`admin.php?page=wpcf7`): list table with Title, Shortcode, Author, Date; search; bulk Delete; row actions Edit / Duplicate; pagination | ✅ (`/wp-admin/admin?page=wpcf7`) |
| Contact › Add Contact Form (`page=wpcf7-new`) | ✅ prefilled with the default templates |
| Edit screen (`page=wpcf7&post=ID&action=edit`): title, shortcode box, Status box (Duplicate, Delete, Save), "Do you need help?" box | ✅ |
| Editor tabs **Form**, **Mail**, **Messages**, **Additional Settings** | ✅ same panels, field names (`wpcf7-form`, `wpcf7-mail[…]`, `wpcf7-mail-2[…]`, `wpcf7-messages[…]`, `wpcf7-additional-settings`) |
| Form tab tag generator buttons: text, email, URL, tel, number, date, text area, drop-down menu, checkboxes, radio buttons, acceptance, quiz, CAPTCHA (image), CAPTCHA (answer), file, submit | ✅ dialog with Field type (required), Name, Default value / placeholder, options, id/class, "Insert Tag" |
| Mail tab: To, From, Subject, Additional headers, Message body, Exclude lines with blank mail-tags, Use HTML content type, File attachments; list of available mail-tags | ✅ (the mail-tag list refreshes on save, not live while typing) |
| Mail (2) with "Use Mail (2)" switch | ✅ |
| Messages tab: every message with its description | ✅ all messages of the active modules |
| Additional Settings | ✅ see §6 |
| Configuration validator (warnings on the edit screen) | ◐ the common checks: empty/invalid To, From not an e-mail, unknown mail-tags in the mail templates, file attachment tag missing, invalid additional headers |
| Help tabs (screen-meta) | ✗ the general Pursae help panel is shown instead |
| Contact › Integration (`page=wpcf7-integration`) | ✅ see §8 |
| Welcome panel / dashboard news | ✗ promotional |
| Legacy "Contact Form 7" admin REST (`/contact-forms` CRUD) | ✅ GET/POST `/contact-forms`, GET/POST(PUT)/DELETE `/contact-forms/{id}` for users with the caps |

## 3. Embedding

| CF7 | Pursae Contact Form |
|---|---|
| `[contact-form-7 id="<hash or id>" title="…"]` and `[contact-form …]` | ✅ through the core shortcode parser (`wp.DoShortcode`) in `theContent` |
| Shortcode attributes `html_id`, `html_name`, `html_title`, `html_class`, `output="raw_form"` | ✅ |
| 7-char hash ids (CF7 5.8+), legacy numeric ids, lookup by title | ✅ |
| Block `contact-form-7/contact-form-selector` | ✅ registered in Pursae Editor while on (select a form); saved markup is CF7's: a `wp-block-contact-form-7-contact-form-selector` div holding the shortcode |
| Widgets / text widget shortcode | ◐ wherever Pursae renders `the_content` |

## 4. Form-tags

Syntax `[type* name option:value "value" 'value']`, escaped `[[tag]]`, enclosing `[acceptance x]…[/acceptance]`,
pipes `"Label|value"`, `default:get` / `default:post_meta` / `default:user_*` / `default:1_2`. Unknown tags are left as is.

| Tag | Options | Status |
|---|---|---|
| `text`, `email`, `url`, `tel` (+`*`) | `size:`/`maxlength:`/`minlength:` (`40/400`), `placeholder`/`watermark`, `default:`, `id:`, `class:`, `autocomplete:`, `readonly`, `tabindex:`, `list:` | ✅ |
| `textarea` | `cols/rows` (`40x10`), `maxlength` (2000), `minlength`, placeholder, default, enclosed content as default | ✅ |
| `number`, `range` | `min:`, `max:`, `step:`, placeholder | ✅ |
| `date` | `min:`, `max:` (YYYY-MM-DD or `today`, `today+7days`…), `step:` | ✅ |
| `select` | `multiple`, `include_blank`, `first_as_label`, `default:N`, `data:` | ✅ `data:countries` and `data:states:XX` fill the list from Pursae Commerce's country data (values are the names; explicit values win) |
| `checkbox`, `radio` | `label_first`, `use_label_element`, `exclusive`, `free_text`, `default:` | ✅ |
| `acceptance` | `optional`, `invert`, enclosed condition text | ✅ |
| `quiz` | `"Question|answer"` pairs, random question, hashed answer | ✅ |
| `file` | `limit:` (1mb default, `kb`/`mb`), `filetypes:` (`jpg|pdf|…` or `image/*`) | ✅ |
| `submit` | value, id, class | ✅ (`has-spinner`) |
| `hidden` | default | ✅ |
| `count` (character counter) | `down`, `id` of the target | ✅ |
| `response` (custom response output position) | | ✅ |
| `captchac` (CAPTCHA image, the Really Simple CAPTCHA engine) | `size:s\|m\|l` (60×20, 72×24, 84×28), `fg:#rrggbb`, `bg:#rrggbb`, `id:`, `class:` | ✅ built in, no plugin or external service; see §8 |
| `captchar` (CAPTCHA answer field, same name as its `captchac`) | `size:`, `maxlength:`, `minlength:`, `placeholder`, `id:`, `class:`, `tabindex:` | ✅ |
| `stripe` | | ✗ payments are out of scope |
| `reflection`, `listo` data lists | | ✗ rarely used, would need data files |

Front-end markup matches CF7 exactly: `div.wpcf7.no-js#wpcf7-f{id}-p{post}-o{n}` with `data-wpcf7-id`, the
`screen-reader-response` block, `form.wpcf7-form.init` with `data-status`, the hidden `fieldset.hidden-fields-container`
(`_wpcf7`, `_wpcf7_version`, `_wpcf7_locale`, `_wpcf7_unit_tag`, `_wpcf7_container_post`, `_wpcf7_posted_data_hash`,
`_wpnonce` for logged-in users), `span.wpcf7-form-control-wrap[data-name]`, `wpcf7-form-control wpcf7-text
wpcf7-validates-as-required …` classes, `wpcf7-not-valid-tip`, `wpcf7-response-output`, `wpcf7-list-item(-label)`,
`wpcf7-spinner`. Forms are wrapped with `wpcf7_autop` (paragraphs/line breaks) like CF7 (`WPCF7_AUTOP`).

## 5. Submission, validation and response

| CF7 | Pursae Contact Form |
|---|---|
| REST `POST /wp-json/contact-form-7/v1/contact-forms/{id}/feedback` (multipart only, 415 otherwise; 404 `wpcf7_not_found`; 400 `wpcf7_unit_tag_not_found`) | ✅ |
| Response `{contact_form_id, status, message, invalid_fields:[{field,message,idref,error_id}], posted_data_hash, into, demo_mode?}` | ✅ |
| Statuses `validation_failed`, `acceptance_missing`, `spam`, `aborted`, `mail_sent`, `mail_failed` | ✅ |
| `GET …/feedback/schema` (SWV rules), `GET …/refill` | ✅ schema with `required`, `email`, `url`, `tel`, `number`, `date`, `minlength`/`maxlength`, `file`, `minnumber`/`maxnumber`, `mindate`/`maxdate`, `enum`; refill returns quiz refresh and new CAPTCHA images (`captcha: {name: url}`) |
| Server-side validation and messages (`invalid_required`, `invalid_too_long/short`, `invalid_email/url/tel/number/date`, `number_too_small/large`, `date_too_early/late`, `quiz_answer_not_correct`, `captcha_not_match`, `upload_*`, `accept_terms`) | ✅ |
| Non-JS fallback (plain POST back to the page, `#unit-tag` action) with "hangover" values and inline errors | ✅ |
| `posted_data_hash` | ✅ |
| Mail-tags `[field]`, `[_raw_field]`, `[_format_field "Y-m-d"]` (date), pipes, special tags `_remote_ip`, `_user_agent`, `_url`, `_date`, `_time`, `_invalid_fields`, `_contact_form_title`, `_serial_number`, `_post_id/_name/_title/_url/_author/_author_email`, `_site_title/_description/_url/_domain/_admin_email`, `_user_login/_email/_url/_first_name/_last_name/_nickname/_display_name` | ✅ (`_serial_number` = submission count kept in meta) |
| Delivery status | ✅ the queued job reports back: entry `mail_status` is queued, sent, failed or skipped; Contact Forms and Entries warn about failures of the last 7 days; Mail (2) failures are logged |
| Spam: honeypot and minimum fill time | ✅ on by default: a hidden `_wpcf7_website` input and an HMAC-signed render time `_wpcf7_ts` (posts faster than 3 s, or with a forged stamp, are spam; posts without the fields, e.g. API clients, are still accepted) |
| Rate limits | ✅ per address: 30 submissions and 30 refills per minute (HTTP 429, `wpcf7_rate_limited`); at most 5000 open CAPTCHA challenges |
| Mail sent through `wp_mail()` | ✅ the core mailer queue (`sendMail`), From/Reply-To/Cc/Bcc from the template |
| Exclude blank lines, HTML mail (body wrapped in CF7's HTML template) | ✅ |
| File attachments (`[your-file]` in the attachments field, plus `uploads/…` paths) | ✅ held in memory (per field `limit:`, 25 MiB for all fields of a submission) and attached to the queued `wp_mail` job; the hourly event removes the attachments from finished jobs |
| Upload dir `wpcf7_uploads` with deny-all, random sub-dirs, file name sanitizing | ◐ no upload directory: files never touch the disk (see above); file names are sanitized |
| Hooks `wpcf7_before_send_mail`, `wpcf7_spam`, `wpcf7_validate_*`, `wpcf7_mail_sent`… | ✅ Wasm hooks (permission `forms:access`): `wpcf7_validate` (filter: extra errors `{field: message}`), `wpcf7_before_send_mail` (filter: change `posted_data`, set `skip_mail`, or `abort` with a message), `wpcf7_spam` (filter), `wpcf7_mail_sent` (action) |
| Webhook out | ✅ Additional Settings `webhook_url: https://…` (repeatable) and optional `webhook_secret:`; JSON POST after the mail is sent, queued with retries, signed `X-Pursae-Signature: sha256=<hmac>`, public addresses only |
| Conditional fields | ✅ `[group id]…[/group]` plus `conditional: group \| field \| equals\|not equals\|is empty\|not empty\|contains\|greater than\|less than \| value` settings; hidden groups are not validated or mailed (checked again on the server) |
| Upload content check | ✅ the first 512 bytes must fit the extension for common types (images, pdf, zip/Office, mp3, txt/csv) |

## 6. Additional settings

| Setting | Status |
|---|---|
| `demo_mode: on` (no mail, `demo` class, `demo_mode: true`) | ✅ |
| `skip_mail: on` | ✅ |
| `subscribers_only: true` | ✅ form replaced with the notice; REST answers `error` for visitors; spam checks skipped for members |
| `acceptance_as_validation: on` | ✅ |
| `do_not_store: true` | ✅ the submission is not written to Contact › Entries |
| `flamingo_*`, `akismet: …`, `on_sent_ok`/`on_submit` (removed in CF7 5.0) | ✗ |

## 7. JavaScript (own file, same behaviour and DOM events)

`wpcf7` global (`api.root`, `api.namespace`, `cached`), `no-js` → `js`, AJAX submit with `FormData`, status classes
(`init`, `submitting`, `invalid`, `unaccepted`, `spam`, `aborted`, `sent`, `failed`, `resetting`, `custom-*`),
`data-status`, events `wpcf7beforesubmit`, `wpcf7submit`, `wpcf7invalid`, `wpcf7unaccepted`, `wpcf7spam`,
`wpcf7mailsent`, `wpcf7mailfailed`, `wpcf7aborted`, `wpcf7statuschanged`, `wpcf7reset` (all bubbling, CF7's `detail`
shape), inline tips + screen-reader list, reset after `mail_sent`, acceptance disabling the submit button, exclusive
checkboxes, free text, URL auto-prefix, character counters, quiz and CAPTCHA refresh (after every submission, on load of
a page from the page cache, and from the CAPTCHA refresh button), spinner, `inert-on-*`, live validation on
change via the schema. ✅

## 8. Spam protection & integrations

| CF7 | Pursae Contact Form |
|---|---|
| Short User-Agent → spam; nonce check for logged-in users | ✅ |
| Disallowed comment keys (Settings › Discussion `disallowed_keys`) | ✅ |
| Quiz | ✅ |
| reCAPTCHA v3 (Integration: site key / secret, score threshold 0.5, `wpcf7_recaptcha_response`) | ✅ keys on Integration, server verification against Google; with Pursae Cookie active the script loads only after consent (Pursae Cookie's embed category) and its cookies appear in the cookie scan |
| Cloudflare Turnstile (site key / secret, `_wpcf7_turnstile_response`) | ✅ |
| Akismet (`akismet:author` …) | ✗ no Akismet in Pursae |
| Constant Contact, Brevo (Sendinblue), Stripe | ✗ third-party marketing/payment accounts, not relevant to a contact form clone here |
| Really Simple CAPTCHA (`[captchac]` / `[captchar]`) | ✅ built in, see below |

### Image CAPTCHA (`[captchac]` / `[captchar]`)

CF7 needs the Really Simple CAPTCHA plugin for these tags; Pursae Contact Form generates the images itself, in pure Go:

- **Challenge**: 4 characters from `ABCDEFGHJKLMNPQRSTUVWXYZ23456789` (no 0/O, 1/I/l), drawn with the Go Bold font
  (`golang.org/x/image/font/gofont`, BSD licence, compiled in) at 3× the displayed size, each character rotated up to
  ±17° and shifted a little, over noise dots and lines in tints between `fg` and `bg`.
- **Storage**: table `gontact_captcha` (migration `0012_gontact_captcha.sql`): random numeric prefix → HMAC-SHA256 of
  prefix + answer (keyed with `gontact_form_secret`) and the PNG. The answer itself is never stored.
- **Image URL**: `/wp-content/gontact-captcha/{prefix}.png`, `Cache-Control: no-store` (the page cache never keeps it);
  404 for unknown, used or expired prefixes, and while the plugin is off.
- **Markup** (CF7's): `<input type="hidden" name="_wpcf7_captcha_challenge_{name}" value="{prefix}">` +
  `<img class="wpcf7-form-control wpcf7-captchac wpcf7-captcha-{name}" width height alt src>`; the answer field is
  `<input class="wpcf7-form-control wpcf7-captchar wpcf7-text" autocomplete="off">` in the usual wrap. The image has a
  descriptive `alt`, and a refresh control (`.wpcf7-captcha-refresh`, a `span role="button"` so it never steals the
  `<label>` of the answer field) that the script shows and that fetches a new image.
- **Validation**: the answer is canonicalized (case and blanks ignored) and checked against the posted prefix; the
  challenge is deleted on its first check, right or wrong (single use). Wrong, reused, expired or missing →
  `captcha_not_match` "Your entered code is incorrect." in `invalid_fields` (editable on the Messages tab).
- **Expiry**: an hour. The hourly core event `gontact_captcha_cleanup` deletes older rows.
- **Page cache**: a cached page carries one stale challenge; with `wpcf7.cached` the script resets the form on load and
  the refill endpoint (`GET …/contact-forms/{id}/refill`, never cached) returns
  `{"captcha": {"your-captcha": "…/{new prefix}.png"}}`, which the script puts into the image and the hidden field. The
  feedback response carries the same `captcha` key, so every submission shows a new image. CF7's own front-end script
  works unchanged against this response (it reads the prefix from the URL).
- **Accessibility**: there is no audio CAPTCHA. The tag generator says so and points to the `[quiz]` tag as the
  accessible alternative (a text question works with screen readers).

## 9. Not ported (summary)

Flamingo, Akismet, Constant Contact, Brevo, Stripe, reflection/listo,
translations (UI is English), CF7's welcome panel and promotional notices, multisite bits.

## 10. Implementation map

| Piece | Where |
|---|---|
| On/off gate, activation (post type row, secret, "Contact form 1"), form model, Contact menu | `internal/server/gontact_plugin.go` |
| Form-tag scanner and options | `internal/server/gontact_formtags.go` |
| Shortcode, `form_html()`, tag handlers, asset tags | `internal/server/gontact_render.go` |
| Submission, validation, spam, uploads, REST (feedback, schema, refill, CRUD), non-JS fallback | `internal/server/gontact_submit.go` |
| Mail-tags and mail composition (queued through the core `wp_mail` job) | `internal/server/gontact_mail.go` |
| Admin screens, config validator, Integration, block assets | `internal/server/gontact_admin.go`, `web/admin/templates/pages/gontact-*.html` |
| Front-end and admin CSS/JS, Pursae Editor block | `web/admin/assets/gontact/` (served at `/wp-content/gontact-form/` only while on) |
| Image CAPTCHA (`[captchac]`/`[captchar]`): generation, storage, image route, refill, cleanup | `internal/server/gontact_captcha.go`, `internal/db/migrations/0012_gontact_captcha.sql` |
| Tests | `internal/server/gontact_test.go`, `internal/server/gontact_captcha_test.go` |

Core changes (small hooks): `internal/mail` `Message` gained `From`, `Cc`, `Bcc` and `Attachments` (multipart/mixed);
the REST cookie check also reads `_wpnonce` from multipart bodies (WordPress reads `$_REQUEST`); the site handler lets
a non-JS form POST render the page; plus one-line registrations in `plugins.go`, `admin.go`, `wpgocache.go`
(`pluginAdmin`), `rest.go` (namespace hidden while off), `site_view.go`, `server.go` and `editor_block.go`; `scheduler.go` has the `gontact_captcha_cleanup` event.
Options: `native_gontact_form`, `gontact_form_installed`, `gontact_form_secret`, `gontact_recaptcha_sitekey/secret`,
`gontact_turnstile_sitekey/secret`. The two secret keys are stored encrypted (AES-256-GCM with a key derived from the
server secret, prefix `sec:v1:`); the Integration screen shows them masked and `/pursae/v1/options/contact` never
returns them.
