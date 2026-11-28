---
title: Mail
description: How Pursae sends e-mail, SMTP settings, and the .eml drop folder for development.
weight: 60
section: operations
---

Pursae sends the same e-mails WordPress does: new-user notifications, password resets,
comment moderation and "new comment" notices, the admin e-mail change confirmation, personal
data export links. Messages are **queued** in the jobs table and delivered by the scheduler, so a
slow SMTP server never delays a request ([cron](cron.md)).

## SMTP

```bash
PURSAE_SMTP_HOST=smtp.example.com
PURSAE_SMTP_PORT=587            # 465 = implicit TLS; other ports use STARTTLS when offered
PURSAE_SMTP_USER=postmaster@example.com
PURSAE_SMTP_PASS=…
PURSAE_MAIL_FROM='Example Blog <no-reply@example.com>'
```

With Docker, `docker/compose.yaml` passes these variables through to the server. For `make prod-up`
put them in `docker/.env` (copied from `docker/.env.example`) and run `make prod-up` again to apply
a change.

Without `PURSAE_MAIL_FROM` the sender is `<Site title> <wordpress@host>`, the host taken from the
site URL — set a real address so receiving servers accept the mail (and set up SPF/DKIM for it).

Delivery failures are retried with back-off (1, 4, 9, 16 minutes); after five attempts the job is
marked failed and shows up in Tools › Site Health.

`PURSAE_SMTP_HOST` may name a private or loopback address (a local MTA). An SMTP host entered in
Pursae SMTP's *Other SMTP* mailer is set by an administrator, so it must be public unless the server
sets `PURSAE_ALLOW_PRIVATE_ENDPOINTS=1` (0.6.3+, [configuration](configuration.md)); Email Test then
says "the SMTP host is on a private network". Email Test reports failures as a fixed category
(could not connect, timed out, TLS failed, authentication failed, rejected), not the server's own reply.
While a saved *Other SMTP* host resolves to a private or loopback address without that switch, administrators see a
persistent admin notice and Site Health reports a critical "The SMTP server is on a private network" item.

## Development: .eml files

Without `PURSAE_SMTP_HOST` every message is written as an `.eml` file to `PURSAE_MAIL_DIR` (default:
`mail/` next to the uploads directory, `/data/mail` in Docker, kept in the `mail` volume). Open the file in a mail client or
read it as text to see the password reset link.

```bash
docker compose -f docker/compose.yaml exec wpgo ls /data/mail
```

## From the admin

Settings › General holds the administration e-mail address; Settings › Discussion decides which
comment events send a notification. Pursae never contacts Gravatar or any other third
party on its own.

Notifications that visitors trigger (comment moderation and "new comment" notices, contact form
mails) are capped (0.6.3+): 5 per 10 minutes from one IPv4 /24 or IPv6 /64, and 30 per 10 minutes
for the whole site. Beyond that the comment or form entry is still saved, only the e-mail is skipped,
and the server log says so once per window.
