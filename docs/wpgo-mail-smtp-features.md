# Pursae SMTP — feature inventory

Pursae SMTP is Pursae's native (compiled-in) clone of **Pursae SMTP** by WPForms /
Awesome Motive (Lite and Pro), the most installed mail plugin for WordPress. Sources: the
wordpress.org listing (`wordpress.org/plugins/wp-mail-smtp`), the wpmailsmtp.com documentation,
and the plugin source (`awesomemotive/wp-mail-smtp`, `src/Options.php` for option keys and the
`WPMS_*` constants).

Like Classic Editor, Pursae Cache and Pursae Web Builder it ships inside the binary, is listed on
**Plugins › Installed Plugins** and **Add Plugin**, starts **inactive** and adds nothing (menu,
routes, notices, dashboard widget, mail behaviour) while inactive. While it is inactive,
`wp_mail()` keeps working exactly as before: the `PURSAE_SMTP_*` environment variables, or `.eml`
files in `PURSAE_MAIL_DIR` on development sites.

Legend: ✅ implemented · ◐ implemented with a Pursae specific difference · — left out (reason given).

## 1. How it plugs into Pursae

| Pursae SMTP | Pursae | |
|---|---|---|
| Replaces `wp_mail()`'s PHPMailer instance (`phpmailer_init`) | `Server.mailer()` attaches a `mail.Transport` to the `mail.Mailer` while the plugin is active. Every sender goes through it: the queued `wp_mail` job (core notifications) and direct `Send` calls. | ✅ |
| Plugin data in options `wp_mail_smtp`, `wp_mail_smtp_*` | One JSON option `wp_mail_smtp` (same group/key names: `mail.from_email`, `smtp.host`, `mailgun.api_key`, …), activation switch `native_wp_mail_smtp` | ✅ |
| Email log in `{prefix}wpmailsmtp_emails_log`, debug events in `{prefix}wpmailsmtp_debug_events` | Tables `wpmailsmtp_emails_log`, `wpmailsmtp_debug_events` (migration `0011_wp_mail_smtp.sql`) | ✅ |
| Secrets stored in plain text in `wp_options` (or a constant) | Secrets (SMTP password, API keys, client secrets, OAuth tokens, Twilio token) are AES-256-GCM encrypted with a key derived from the server secret (`PURSAE_SECRET` / `auth_secret`), stored as `enc:v1:…`, never printed back into a form (the field shows a "saved" placeholder and a *Remove* checkbox; leaving it blank keeps the saved value) | ◐ stronger than upstream |

## 2. Setup Wizard (`admin?page=wp-mail-smtp-setup-wizard`)

| Step | | |
|---|---|---|
| Welcome ("Let's get started") | Opens automatically on first activation; "Go back to the Dashboard" exits | ✅ |
| Import from other SMTP plugins (Easy WP SMTP, FluentSMTP, Post SMTP, …) | — no such plugins exist on Pursae |  — |
| Choose your SMTP mailer (logo grid, recommended mailers first) | Same grid, every mailer of §4 | ✅ |
| Configure mailer settings (+ From Email / From Name) | Same fields as the settings screen, saved through the same code | ✅ |
| Plugin features to enable (Email Log, Email Reports/weekly summary, Alerts, Improved deliverability, Smart routing…) | Checkboxes for Email Log, Complete Email Reports (log content), Weekly Email Summary, Instant Email Alert (to the admin e-mail), Detailed Error Reports (debug events) | ✅ |
| Help improve (usage tracking), license key | — no telemetry, no licensing (everything is included) | — |
| Check configuration: sends a test e-mail, shows success or the error with fix steps | Sends the test through the chosen mailer, shows the result and the debug output. Other SMTP failures are shown as a fixed category (could not connect, timed out, TLS, authentication, rejected; 0.6.3+) | ✅ |
| Domain checker (SPF/DKIM/DMARC) | DNS lookups of the From domain after a successful test: SPF (`TXT v=spf1`), DMARC (`_dmarc` TXT), MX, and DKIM: the selectors the mailer is known to use (SendGrid `s1`/`s2`, Mailgun `mx`/`k1`, Google `google`, Brevo `mail`, Microsoft `selector1`/`selector2`, …) then common defaults, TXT or CNAME at `<selector>._domainkey`; a custom selector can be typed in (Amazon SES uses random selectors) | ✅ |
| Recommended settings from a hosted service | — no phone-home | — |

## 3. Settings › General (`admin?page=wp-mail-smtp`)

| Setting | Behaviour | |
|---|---|---|
| From Email + **Force From Email** | Address used for every message. Without *Force*, a message that sets its own From (e.g. a Pursae Contact Form form's "From") keeps it; with *Force* it is always replaced | ✅ |
| From Name + **Force From Name** | Same for the display name (default: site title) | ✅ |
| Return Path ("Set the return-path to match the From Email") | The SMTP envelope sender (bounces) is the From Email; when a message keeps its own From (not forced) the envelope follows it only with Return Path on | ◐ (no PHP `mail()`) |
| Mailer | Default (none), Other SMTP and every mailer below. Pro-only mailers are included (no license) | ✅ |
| Other SMTP: SMTP Host, Encryption (None / SSL / TLS), SMTP Port (25 / 465 / 587 suggested per encryption), Auto TLS, Authentication, SMTP Username, SMTP Password | Native `net/smtp` client; SSL = implicit TLS, TLS = STARTTLS required, None + Auto TLS = STARTTLS when the server offers it; AUTH PLAIN (LOGIN fallback when the server only advertises LOGIN). A host on a private or loopback network needs `PURSAE_ALLOW_PRIVATE_ENDPOINTS=1` on the server (0.6.3+; `PURSAE_SMTP_HOST` does not) | ✅ |
| Default (none) = PHP `mail()` | Pursae's built-in transport: `PURSAE_SMTP_*` env SMTP, or `.eml` files | ◐ |
| Backup Connection (Pro) | Pick one Additional Connection; when the primary connection fails the message is retried once through it; the log records which connection delivered it | ✅ |
| Rate limiting (Pro, per minute/hour/day/week/month) | Under Misc. Each send reserves its slot in `wpmailsmtp_rate` inside a transaction under an advisory lock before it goes out (concurrent sends cannot overshoot; a failed send gives the slot back); over a limit the message is re-queued as a `wp_mail` job for when the window allows it (a form's own From is not kept on the re-queued copy) | ✅ |
| Optimized email sending (Pro, background queue) | Always on in Pursae: core e-mail is already sent by the job runner | ◐ |

## 4. Mailers

| Mailer (slug) | Fields | Transport | |
|---|---|---|---|
| Default (`mail`) | — | built-in | ✅ |
| Other SMTP (`smtp`) | host, encryption, port, autotls, auth, user, pass | SMTP | ✅ |
| SendLayer (`sendlayer`) | API Key | `POST console.sendlayer.com/api/v1/email` | ✅ |
| SMTP.com (`smtpcom`) | API Key, Sender Name (channel) | `POST api.smtp.com/v4/messages` | ✅ |
| Brevo (`sendinblue`) | API Key, Sending Domain | `POST api.brevo.com/v3/smtp/email` | ✅ |
| Mailgun (`mailgun`) | Private API Key, Domain Name, Region US/EU | `POST api[.eu].mailgun.net/v3/{domain}/messages` | ✅ |
| SendGrid (`sendgrid`) | API Key, Sending Domain | `POST api.sendgrid.com/v3/mail/send` | ✅ |
| Postmark (`postmark`) | Server API Token, Message Stream ID | `POST api.postmarkapp.com/email` | ✅ |
| SparkPost (`sparkpost`) | API Key, Region US/EU | `POST api[.eu].sparkpost.com/api/v1/transmissions` | ✅ |
| Amazon SES (`amazonses`, Pro) | Access Key ID, Secret Access Key, Region | SES v2 `outbound-emails` (raw MIME), AWS Signature V4 | ✅ |
| Microsoft 365 / Outlook (`outlook`, Pro) | Application ID, Client Secret, Redirect URI | OAuth 2 authorization code flow (own Azure app), Microsoft Graph `sendMail` (MIME) | ✅ |
| Google / Gmail (`gmail`) | Client ID, Client Secret, Authorized redirect URI | OAuth 2 (own Google Cloud app), Gmail API `users.messages.send` | ✅ |
| Zoho Mail (`zoho`, Pro) | Region (com/eu/in/com.au/jp/com.cn), Client ID, Client Secret, Redirect URI | OAuth 2, Zoho Mail `accounts/{id}/messages` | ✅ |
| Elastic Email (`elasticemail`) | API Key | `POST api.elasticemail.com/v4/emails/transactional` | ✅ |
| Mailjet (`mailjet`) | API Key, Secret Key | `POST api.mailjet.com/v3.1/send` | ✅ |
| MailerSend (`mailersend`) | API Key, Professional plan | `POST api.mailersend.com/v1/email` | ✅ |
| Resend (`resend`) | API Key | `POST api.resend.com/emails` | ✅ |
| SMTP2GO (`smtp2go`) | API Key | `POST api.smtp2go.com/v3/email/send` | ✅ |
| Mandrill (`mandrill`) | API Key | `POST mandrillapp.com/api/1.0/messages/send.json` | ✅ |
| Gmail / Microsoft **One-Click Setup** (Pro) | — needs WPForms' hosted OAuth relay and a license; the "own app" flow above is supported instead | — |
| Pepipost (deprecated upstream) | — removed upstream | — |

Every mailer delivers Cc and Bcc recipients and attachments (Contact Form additional headers, Commerce
e-mails): SMTP adds them to the envelope, the JSON APIs get them in each provider's own fields, SES in
`Destination`, Gmail and Microsoft Graph through a Bcc header they strip. Zoho Mail cannot attach files
without a separate upload, so a message with attachments fails there (and the backup connection is tried)
instead of going out without them. The log records Cc/Bcc in the headers and *Resend* to the original
recipients keeps them.

OAuth: the settings screen shows the redirect URI to register
(`{site}/wp-admin/admin?page=wp-mail-smtp&tab=auth`), a *Connect* button after the client
credentials are saved, the connected account and *Remove OAuth Connection*. Refresh tokens and
access tokens are stored encrypted; access tokens are refreshed automatically.

## 5. Additional Connections & Smart Routing (Pro)

| | | |
|---|---|---|
| Additional Connections: any number of extra mailer configurations, each with a name, From Email/Name and mailer settings; *Send test* per connection | ✅ |
| Smart Routing: ordered routes "Send with connection X if" condition groups (AND inside a group, OR between groups); properties Subject, Message, From Email, From Name, To, Reply-To, Initiator; operators is / is not / contains / does not contain / starts with / ends with / regex / does not match regex. First matching route wins, else the primary connection | ✅ |

## 6. Email Log (Pro) (`admin?page=wp-mail-smtp-logs`)

| | | |
|---|---|---|
| Settings › Email Log: Enable Log, Log Email Content, Save Attachments, Open/Click Email Tracking, Log Retention Period (forever, 1 day … 1 year) | Enable, content, retention ✅; attachments: the name and size of each file are logged and shown on the entry (the bytes are not stored, so a resend does not include them); open/click tracking ✅ (off by default, needs the log: a signed 1x1 pixel and signed link redirects under `/wp-mail-smtp/v1/t/`, Do Not Track / Global Privacy Control visits are never counted, redirect targets are HMAC-signed so it is not an open redirect) | ✅ |
| List table: Subject, From, To, Status (Sent/Failed/Waiting), Source, Date; status views (All/Sent/Failed), search (subject, recipient, content), date filter, pagination, bulk delete, "Delete All Logs" | ✅ |
| Single email view: headers, recipients, mailer, connection, attempts, error, plain/HTML content (HTML in a sandboxed iframe), *Resend*, *Delete*, Print | ✅ |
| Resend (single + bulk), optionally to different recipients ("forward") | ✅ |
| Export (Tools › Export): CSV / EML (zip) / XLSX, date range, field selection | CSV and EML (zip) ✅; XLSX — | ◐ |
| Retention: old entries removed by the daily job | ✅ |
| Repeated attempts of a queued message update the same entry (attempt counter) instead of adding new ones; the job runner still retries failed core e-mail (1, 4, 9, 16 minutes) | ◐ Pursae specific |

## 7. Email Reports (Pro) (`admin?page=wp-mail-smtp-reports`)

| | | |
|---|---|---|
| Chart of sent/failed per day (7/14/30 days, custom range) | Server-rendered SVG chart ✅ |
| Totals: total, sent, failed (+ open/click rates) | Totals ✅; open rate, click rate (e-mails with at least one open/click out of the e-mails that carried tracking) and bounce rate (bounced of sent) for the period, with raw hit counts and delivered/bounced/complaint totals; per-subject open/click rates; the weekly summary e-mail lists them too. Per-entry counters stay on the log detail page |
| Table grouped by subject with counts | ✅ |
| **Weekly Email Summary**: e-mail to the admin every Monday with last week's stats; can be disabled (Misc / constant) | Daily job sends it when 7 days passed ✅ |

## 8. Alerts (Pro) (`admin?page=wp-mail-smtp&tab=alerts`)

| Channel | | |
|---|---|---|
| Email (up to 3 addresses) | Sent through Pursae's built-in transport (the plugin's mailer just failed) | ✅ |
| Slack webhook, Discord webhook, Microsoft Teams webhook | JSON POST (`text` / `content` / MessageCard) | ✅ |
| Twilio SMS (Account SID, Auth Token, From, To) | `POST api.twilio.com/2010-04-01/Accounts/{sid}/Messages.json` | ✅ |
| Custom webhook (JSON POST of the failure) | ✅ |
| Push notifications (browser), WhatsApp | — need hosted services / Meta business templates | — |
| Failed-email threshold ("alert only after N failures in 30 minutes") | ✅ |
| Hard-bounce events (needs mailer webhooks) | Delivered / bounced / spam-complaint status on the log entry from Postmark, SendGrid and Mailgun webhooks: `POST /wp-mail-smtp/v1/webhook/<mailer>?token=…` (URLs with the secret token are shown under Settings › Email Log). Amazon SES: subscribe the token URL (`…/webhook/amazonses?token=…`) over HTTPS to the SNS topic that receives the SES bounce/complaint/delivery notifications. The SNS envelope is trusted only after its signature verifies (SignatureVersion 1 = SHA1withRSA, 2 = SHA256withRSA, over SNS's canonical string); `SigningCertURL` must be an https `.pem` on `sns.<region>.amazonaws.com(.cn)` (no port, no credentials, redirects not followed), the certificate must be in its validity period; the `SubscriptionConfirmation` is confirmed by fetching its `SubscribeURL` under the same host rule. Identity notifications (`notificationType`) and configuration-set events (`eventType`) are both read; transient bounces are ignored. Matched by the SES `MessageId` the API mailer logs (the SES SMTP relay's IDs are not logged, so use the Amazon SES mailer) | ✅ |

## 9. Email Controls (Pro) (`tab=control`)

Toggles for the notification e-mails Pursae core and Pursae Commerce send (identified by `mail.Message.Kind`, set by the sender; mail without a Kind falls back to the subject prefix):
comment moderation / new comment to author; admin e-mail change confirmation; user e-mail change
request; password reset request, password changed (to admin), new user registration (admin + user);
personal data confirm request / action confirmed / export ready / erasure fulfilled. Disabled
types are dropped silently. Automatic core/plugin/theme update e-mails don't exist in Pursae. ✅

## 10. Misc tab (`tab=misc`)

| | | |
|---|---|---|
| Do Not Send (+ confirmation) | Blocks all e-mail; a red notice reminds the admin; blocked messages become debug events | ✅ |
| Hide Announcements | No control: Pursae shows no product announcements (the stored value is kept for import/export) | — |
| Hide Email Delivery Errors | Hides the admin-wide "last e-mail failed" notice | ✅ |
| Hide Dashboard Widget | ✅ |
| Disable Email Summaries | ✅ |
| Debug Events retention period and "Debug Email Sending" | On Tools › Debug Events, as upstream | ✅ |
| Allow Usage Tracking | — no telemetry | — |
| Uninstall WP Mail SMTP ("remove ALL data on deletion") | Native plugins can't be deleted; when ticked, *deactivating* removes the plugin's options, logs and debug events | ◐ |

## 11. Tools (`admin?page=wp-mail-smtp-tools`)

| | | |
|---|---|---|
| Email Test: Send To, HTML/plain toggle, connection picker; result notice; on failure the debug output (SMTP conversation with credentials masked, HTTP status/body for API mailers) and suggested fixes | ✅ |
| Export (email log) | ✅ (§6) |
| Debug Events: list of errors/debug messages with type, source, date; view, delete, "Debug Email Sending" option to also record debug info for successful sends | ✅ |
| Email Detective (deliverability report via hosted service), Code Snippets (WPCode), Action Scheduler | — hosted / PHP-specific | — |

## 12. Dashboard widget & notices

| | | |
|---|---|---|
| Dashboard widget "Pursae SMTP": sent/failed totals and a 7-day chart, link to the log; can be hidden (Misc) | ✅ |
| Admin notice when the last e-mail failed (with the error and links to the log / settings) | ✅ |
| Admin bar "Pursae SMTP" error menu | — notice is used instead | — |
| Site Health tests | A "Pursae SMTP" row on Site Health and the dashboard widget: last delivery failure, failures in the last 24 hours, Do Not Send | ✅ |

## 13. Constants → environment variables

`WPMS_ON=true` enables the overrides (as in `wp-config.php`). Every constant of upstream's
`Options::get_const_value()` is read from the environment with the same name; overridden fields
are shown read-only with "The value of this field was set using a constant." Supported:
`WPMS_MAILER, WPMS_MAIL_FROM, WPMS_MAIL_FROM_FORCE, WPMS_MAIL_FROM_NAME, WPMS_MAIL_FROM_NAME_FORCE,
WPMS_SET_RETURN_PATH, WPMS_DO_NOT_SEND, WPMS_SMTP_HOST, WPMS_SMTP_PORT, WPMS_SSL, WPMS_SMTP_AUTH,
WPMS_SMTP_AUTOTLS, WPMS_SMTP_USER, WPMS_SMTP_PASS`, every mailer key
(`WPMS_SENDLAYER_API_KEY, WPMS_SMTPCOM_API_KEY, WPMS_SMTPCOM_CHANNEL, WPMS_SENDINBLUE_API_KEY,
WPMS_SENDINBLUE_DOMAIN, WPMS_MAILGUN_API_KEY, WPMS_MAILGUN_DOMAIN, WPMS_MAILGUN_REGION,
WPMS_SENDGRID_API_KEY, WPMS_SENDGRID_DOMAIN, WPMS_POSTMARK_SERVER_API_TOKEN,
WPMS_POSTMARK_MESSAGE_STREAM, WPMS_SPARKPOST_API_KEY, WPMS_SPARKPOST_REGION,
WPMS_AMAZONSES_CLIENT_ID, WPMS_AMAZONSES_CLIENT_SECRET, WPMS_AMAZONSES_REGION,
WPMS_GMAIL_CLIENT_ID, WPMS_GMAIL_CLIENT_SECRET, WPMS_OUTLOOK_CLIENT_ID, WPMS_OUTLOOK_CLIENT_SECRET,
WPMS_ZOHO_DOMAIN, WPMS_ZOHO_CLIENT_ID, WPMS_ZOHO_CLIENT_SECRET, WPMS_ELASTICEMAIL_API_KEY,
WPMS_MAILJET_API_KEY, WPMS_MAILJET_SECRET_KEY, WPMS_MAILERSEND_API_KEY, WPMS_MAILERSEND_HAS_PRO_PLAN,
WPMS_RESEND_API_KEY, WPMS_SMTP2GO_API_KEY, WPMS_MANDRILL_API_KEY`), logs
(`WPMS_LOGS_ENABLED, WPMS_LOGS_LOG_EMAIL_CONTENT, WPMS_LOGS_LOG_RETENTION_PERIOD`), alerts
(`WPMS_ALERT_EMAIL_SEND_TO, WPMS_ALERT_SLACK_WEBHOOK_URL, WPMS_ALERT_DISCORD_WEBHOOK_URL,
WPMS_ALERT_TEAMS_WEBHOOK_URL, WPMS_ALERT_CUSTOM_WEBHOOK_URL, WPMS_ALERT_TWILIO_SMS_ACCOUNT_SID,
WPMS_ALERT_TWILIO_SMS_AUTH_TOKEN, WPMS_ALERT_TWILIO_SMS_FROM_PHONE_NUMBER,
WPMS_ALERT_TWILIO_SMS_TO_PHONE_NUMBER`), `WPMS_SUMMARY_REPORT_EMAIL_DISABLED`,
`WPMS_DEBUG_EVENTS_RETENTION_PERIOD`. License, WhatsApp, multisite and one-click constants are not
applicable.

## 14. Left out (not applicable to single-site Pursae)

Multisite network settings, license/upgrade screens and "About Us"/product upsells, usage
tracking, WP-CLI commands, the WordPress Abilities API, importing other SMTP plugins' settings,
conflicting-plugin detection, attachment storage, XLSX export, push/WhatsApp
alerts, one-click Gmail/Microsoft setup through WPForms' relay.

## 15. Where it lives

`internal/mail/{smtp,api,oauth}.go` (transports, AWS SigV4, OAuth 2), `internal/mail/smtptest`
(fake SMTP server for tests), `internal/server/mailsmtp*.go` (options, delivery, log, alerts,
reports, admin screens), `web/admin/templates/pages/mailsmtp.html`,
`internal/db/migrations/0011_wp_mail_smtp.sql`. Hooks into shared code: `mail.Mailer.Transport`,
`Server.mailer()` (`mailSMTPAttach`), the `wp_mail_smtp_daily` job, the Plugins screen row,
`pluginAdmin` (`admin?page=wp-mail-smtp*`), `adminPage` (menu + notices) and the dashboard widget.
