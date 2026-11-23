---
title: Backups
description: What to back up, how to dump and restore PostgreSQL, and how to move a site.
weight: 30
section: operations
---

A site is three things: the **database**, the **uploads** directory and the **plugins and themes**
directories next to it. The binary and the bundled themes need no backup.

| What | Where (compose) | Where (binary) |
|------|-----------------|----------------|
| database | volume `pgdata` | your PostgreSQL |
| media | volume `uploads` (`/data/uploads`) | `PURSAE_UPLOADS_DIR` |
| plugin packages | volume `plugins` (`/data/plugins`) | `<uploads>/../plugins` |
| theme packages and edited theme files | volume `themes` (`/data/themes`) | `PURSAE_THEMES_DIR` |
| Backup Manager archives | volume `backups` (`/data/backups`) | `PURSAE_BACKUPS_DIR` |

The other volumes need no backup: `bin` (the updated binary; the image or Dashboard › Updates puts
one back), `pagecache`, `languages` (downloaded again) and `mail` (development `.eml` files).

## Database

Dump (on a `make prod-up` stack add `-f docker/compose.prod.yaml` after `-f docker/compose.yaml`):

```bash
docker compose -f docker/compose.yaml exec db pg_dump -U wpgo -Fc wpgo > wpgo.dump
```

Restore into the same database:

```bash
docker compose -f docker/compose.yaml exec -T db pg_restore -U wpgo -d wpgo --clean --if-exists < wpgo.dump
```

With your own PostgreSQL use `pg_dump -Fc "$DATABASE_URL" > wpgo.dump` and
`pg_restore -d "$DATABASE_URL" --clean --if-exists wpgo.dump`. Use a `pg_dump` of the server's major
version or newer: an older one refuses to dump a newer server. This dump-and-restore round trip runs in CI
against PostgreSQL 16 and 17 (`TestBackupPgDumpRoundTrip`). To keep a dump off the server encrypted like
Backup Manager archives, encrypt it with any tool you trust (for example `age -p`) before copying it.

## Files

```bash
docker run --rm -v wpgo_uploads:/data/uploads -v wpgo_plugins:/data/plugins -v wpgo_themes:/data/themes \
  -v wpgo_backups:/data/backups -v "$PWD":/backup alpine tar czf /backup/wpgo-files.tgz /data
```

(Volume names are prefixed with the compose project name, `wpgo`.)

## Moving a site

1. Dump the database and copy the files to the new host.
2. Start the new instance with the same `PURSAE_SECRET` if you want existing sessions to survive.
3. Restore the dump, then update the URL under Settings › General if the address changed. The
   site URL is stored in the database, not in the environment, once installed.

## Backup Manager

The built-in **Backup Manager** plugin (off by default) makes the same backup from the admin screen as
one `.zip` (database rows as JSON lines plus the files), restores it on this or another site, and can
run **scheduled local backups** (daily or weekly, keeping the newest *N*). Archives go to
`PURSAE_BACKUPS_DIR` (default `<uploads>/../backups`, the `backups` volume in Docker, never inside the
public uploads). Archives stay on the same server unless you set up **remote storage** (below) or copy
them elsewhere yourself (for example with the `tar` command above).

A restore keeps this site's own secret (`auth_secret`), address and Backup Manager settings. Secrets
the built-in plugins store encrypted under the archive site's secret (Pursae SMTP passwords and
tokens, Pursae CDN credentials, the Multi Language translation key, Pursae Security authenticator
apps) are re-encrypted for this site, so they keep working after a move; Pursae Security backup codes
are only stored as hashes and have to be generated again. If the archive's site encrypted with
`PURSAE_SECRET`, start this site with the same value instead. An archive with accounts whose username or
e-mail differs only by case restores too: those columns then stay non-unique regardless of case, as
on the site the archive came from.

### Remote storage (S3-compatible, encrypted)

Backup Manager › **Remote storage** copies backups to an S3-compatible bucket: Amazon S3, Cloudflare R2,
Backblaze B2, Wasabi, MinIO and the like. You create the bucket and an access key with the provider;
Pursae only needs the endpoint, region, bucket, an optional folder (key prefix), the access key and an
**encryption passphrase**. Without a bucket nothing changes: backups stay local, as before.

* **Encrypted before upload.** Every archive is encrypted on this server (AES-256-GCM in 64 KiB chunks,
  key derived from the passphrase with scrypt) and stored as `<archive>.zip.enc`. The provider never sees a
  readable backup; a changed, truncated or reordered file, or a wrong passphrase, is refused on the way
  back. **Keep the passphrase somewhere else**: without it nobody, Pursae included, can open the backups.
* **Streaming, multipart.** The archive is read, encrypted and uploaded as one stream in 16 MB parts
  (larger for files over ~140 GB), so memory use does not grow with the backup. A failed part is retried;
  a failed upload is aborted so the bucket keeps no orphaned parts.
* **Automatic upload and retention.** With *Automatic upload* every new manual and scheduled backup is
  uploaded right after it is written (safety snapshots and pre-update backups stay local). *Keep in the
  bucket* deletes the oldest `.zip.enc` objects in the folder beyond *N* after each upload (0 keeps all).
  A failed upload never loses the local archive: it is a warning on the job, and for a scheduled run the
  error of *Last scheduled backup* (e-mailed like any failure).
* **Restore from the bucket.** *Download and restore* streams the object back, decrypts it into the backups
  folder, checks it, and opens the usual restore screen. A backup uploaded under an earlier passphrase
  takes that passphrase in the field below the list.
* **Test connection** writes, lists, reads back and deletes a small encrypted test object.

Secrets: the secret key and the passphrase are stored in the `backup_manager_remote` option sealed with
the site secret (never in plain text), never shown again on the screen, in the REST API or by the CLI, and
kept out of logs and error messages. Set `PURSAE_SECRET` so the seal does not depend on a key stored in the
same database, or set the values in the environment instead, which keeps them out of the database
altogether (each one is then read-only on the screen):

| Variable | Field |
|----------|-------|
| `PURSAE_BACKUP_S3_ENDPOINT` | endpoint, e.g. `https://s3.eu-central-1.amazonaws.com` |
| `PURSAE_BACKUP_S3_REGION` | region (empty: `us-east-1`; R2: `auto`) |
| `PURSAE_BACKUP_S3_BUCKET` | bucket |
| `PURSAE_BACKUP_S3_PREFIX` | folder, e.g. `site1/` |
| `PURSAE_BACKUP_S3_ACCESS_KEY` / `PURSAE_BACKUP_S3_SECRET_KEY` | credentials |
| `PURSAE_BACKUP_S3_PATH_STYLE` | `1` for path-style requests (MinIO) |
| `PURSAE_BACKUP_PASSPHRASE` | encryption passphrase |
| `PURSAE_BACKUP_S3_ALLOW_PRIVATE` | `1` allows a storage server on a private address and plain `http` |

The endpoint must be `https` on a public address. The address is checked again when each connection is
made, after DNS resolution, and redirects are not followed, so a host name cannot be used to reach the
server's own network (SSRF). Link-local and cloud metadata addresses are always refused. For a MinIO on
your own network set `PURSAE_BACKUP_S3_ALLOW_PRIVATE=1`; it is an environment setting on purpose, so an
administrator account alone cannot point the server at internal services.

The same from the command line (secrets are read from files, `-` for stdin, never from flags):

```bash
pursae backup remote set --endpoint=https://s3.eu-central-1.amazonaws.com --region=eu-central-1 \
  --bucket=my-backups --prefix=site1/ --access-key=AKIA… --secret-key-file=/run/secrets/s3 \
  --passphrase-file=/run/secrets/backup-pass --auto --keep=14
pursae backup remote test
pursae backup remote list
pursae backup remote push backup-20260101-120000-abcd1234.zip
pursae backup remote pull backup-20260101-120000-abcd1234.zip.enc   # then: pursae backup restore …
pursae backup remote delete backup-20260101-120000-abcd1234.zip.enc --yes
pursae backup decrypt ./backup-20260101-120000-abcd1234.zip.enc --passphrase-file=./pass.txt
```

**Disaster recovery on a new server:** the `backup` commands run before the site is installed. Start the
new instance with the `PURSAE_BACKUP_*` variables, run `pursae backup remote pull <name>.zip.enc`, then
`pursae backup restore <name>.zip`. Without Pursae at hand, download the object with any S3 tool and run
`pursae backup decrypt` on it. REST: `GET /pursae/v1/backups/remote` lists the bucket (settings without
secrets), `POST /pursae/v1/backups/remote {"file": "…"}` uploads a stored archive.

## Content export

Tools › Export writes a WXR file with posts, pages, media references, comments and terms; "All
content" also carries the posts and terms of Pursae Custom Fields post types and taxonomies. Tools ›
Import reads one; items of a post type not registered on the site are skipped and listed in the
report. WooCommerce products and variations in the file are imported into Pursae Commerce
([details](../../goocommerce-features.md#wordpress-import-wxr)); WooCommerce orders, refunds and coupons are
counted ("Skipped N orders / M coupons") and not imported. It is a content transfer, not a backup: it does not carry users' passwords,
settings, plugins or themes.
