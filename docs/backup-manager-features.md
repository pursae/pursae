# Pursae Backup Manager

Native plugin (slug `backup-manager`, off on a fresh install). Backs a whole site up into one `.zip`
and restores it on the same or another Pursae site, like All-in-One WP Migration, but without an SQL
dump: rows are exported as JSON lines straight from PostgreSQL and restored through
`jsonb_populate_recordset`, so `pg_dump`/`psql` are not needed.

Menu: **Backup Manager** (administrators, `manage_options`). Tabs: Create backup, Backups, Schedule,
Remote storage, Import / Migrate, plus a restore screen per archive.

## What can be selected

Each part is a checkbox; the archive stores only what was ticked.

| Part | Tables | Files |
|------|--------|-------|
| Posts, pages & taxonomy | posts, revisions, terms, comments, menus, widgets … | |
| Media library | media, media_variants | uploads |
| Users & roles | users, roles, user_roles, user_capabilities, app_passwords | |
| Site settings | settings | |
| Themes | themes | edited/installed theme files |
| Plugins | plugins, plugin_data | plugin packages |
| Page builder | web_builder_* | |
| Pursae Commerce / Headless / Multi Language / SEO / Contact forms / CDN / Cookie / SMTP / Security | by table prefix | |
| Other plugin data | every table no part claims | |

Users can be limited to **all users**, **chosen roles** or **picked users**; the tables that cascade from
`users` (2FA devices, memberships …) follow the same filter. Posts written by users left out go to the
administrator who restores.

`sessions`, `transients`, `jobs` and `schema_migrations` are never exported.

Selecting a part also pulls in every table that depends on it through `ON DELETE CASCADE` (restoring
`posts` without `goo_products` would wipe the products).

## Archive (format 1)

```
manifest.json            site URL, Pursae version, newest migration, tables with row counts and columns, files groups
database/<table>.jsonl   one `to_jsonb(row)` per line
files/uploads/…  files/themes/…  files/plugins/…
```

## Restore

1. Optional **safety snapshot** of the current site (`pre-restore-*.zip`, on by default).
2. One transaction: foreign keys are made deferrable, the chosen tables emptied, the rows inserted in batches,
   orphans of a partial restore repaired by each key's own `ON DELETE` rule, sequences reset, constraints
   checked, foreign keys made immediate again. Any failure rolls the database back untouched.
3. Files are extracted afterwards (atomic writes, path traversal refused, symlinks skipped); existing files
   with other names are left alone.
4. Settings are reloaded and the page cache purged.

Kept from the target site: `auth_secret`, `siteurl`, `home` and the plugin's own switch. With *Replace the old
address* ticked (default when it differs) every occurrence of the archive's site URL, also in its JSON-escaped
form (`https:\/\/…`), is rewritten to this site's address in all restored rows.

An archive from a newer schema than the site's is refused. Columns missing from the archive take their
defaults; columns the site no longer has are ignored.

Restoring users replaces the sessions: everyone, including the administrator running it, logs in again with the
backup's accounts. Restoring plugins needs a restart of Pursae to load the packages.

## Scheduled backups

The **Schedule** tab turns on automatic local backups:

| Setting | Option | Default |
|---------|--------|---------|
| Automatic backups: Off, Daily, Weekly | `backup_manager_schedule` | Off |
| Keep (scheduled archives kept, 1–100) | `backup_manager_schedule_keep` | 7 |
| Parts (same parts as a manual backup; every user is included) | `backup_manager_schedule_cats` | all |

* Saving a schedule adds one recurring row (`wpgo_backup_scheduled`, `daily` or `weekly`) to the `jobs` table;
  the first run is 10 minutes later, then one per day or week (see the cron page of the developer docs).
  Saving again with the same frequency keeps the next run; Off, or deactivating the plugin, removes the row;
  activating the plugin puts it back.
* The scheduler dispatches the row to the `wpgo_backup_scheduled` action, which the core registers at start-up.
  A run within an hour of the previous one is ignored.
* A run writes `scheduled-<time>-<random>.zip` (manifest kind `scheduled`) into the backups folder, then
  deletes the oldest scheduled archives beyond *Keep*. Manual backups, uploads and safety snapshots are never
  pruned. If another backup or restore is running, the run fails and the error is shown under *Last scheduled
  backup* (`backup_manager_schedule_last`).
* A restore keeps this site's schedule options.

## Remote storage

The **Remote storage** tab copies backups to an S3-compatible bucket (AWS S3, Cloudflare R2, Backblaze B2,
Wasabi, MinIO …), encrypted on the server first. Off while no bucket is set.

| Setting | Option field | Environment |
|---------|--------------|-------------|
| Endpoint (https, public address) | `endpoint` | `PURSAE_BACKUP_S3_ENDPOINT` |
| Region (empty: us-east-1) | `region` | `PURSAE_BACKUP_S3_REGION` |
| Bucket | `bucket` | `PURSAE_BACKUP_S3_BUCKET` |
| Folder (key prefix) | `prefix` | `PURSAE_BACKUP_S3_PREFIX` |
| Path-style requests | `path_style` | `PURSAE_BACKUP_S3_PATH_STYLE` |
| Access key ID / secret access key | `access_key` / `secret_key` (sealed) | `PURSAE_BACKUP_S3_ACCESS_KEY` / `PURSAE_BACKUP_S3_SECRET_KEY` |
| Encryption passphrase (12+ characters) | `passphrase` (sealed) | `PURSAE_BACKUP_PASSPHRASE` |
| Automatic upload of new manual and scheduled backups | `auto` | |
| Keep in the bucket (0–1000, 0 = all) | `keep` | |

All in the `backup_manager_remote` option, which a restore keeps from this site. Secret fields are sealed
(AES-GCM, key from the site secret, prefix `bkr:v1:`), never rendered back (an empty field keeps the saved
value; *Forget secrets* deletes them) and absent from REST and CLI output; an environment variable wins over
the option and is shown read-only.

* **Encryption** (`internal/backupcrypt`): header `PSBK`, version, scrypt cost, 16-byte salt and 7-byte nonce
  prefix; then AES-256-GCM chunks of 64 KiB, nonce = prefix ‖ chunk counter ‖ last-chunk flag, header as
  additional data (the STREAM construction): chunks cannot be reordered, dropped or cut off unnoticed. Key:
  scrypt(passphrase, salt, N=2^16, r=8, p=1).
* **Upload** streams file → encryption → 16 MB parts (`CreateMultipartUpload` / `UploadPart` /
  `CompleteMultipartUpload`; one `PUT` below a part), each part retried twice, the upload aborted on failure.
  Objects are named `<prefix><archive>.zip.enc`. After each upload the oldest `*.zip.enc` directly under the
  prefix beyond *Keep* are deleted.
* **Automatic upload** runs inside the backup job (phase *Uploading to remote storage*) for manual and
  scheduled archives only. A failure is a warning (`remote_error` on the job, the local archive kept) and, for
  scheduled runs, the error of `backup_manager_schedule_last`.
* **Restore** (*Download and restore*, `backup remote pull`): the object is decrypted while it downloads into
  `<name>.zip.part`, checked like an upload, renamed, and the restore screen opens. An archive of the same name
  already in the folder is used as it is.
* **Test connection**: PUT, LIST, GET (decrypted and compared) and DELETE of `.pursae-connection-test-<random>`.
* **SSRF**: the endpoint is validated (http(s), no credentials, query or fragment; https only), and every
  connection's address is checked after DNS resolution; no proxy, no redirects. Private and loopback addresses
  (and plain http) need `PURSAE_BACKUP_S3_ALLOW_PRIVATE=1`; link-local and metadata addresses
  (169.254.0.0/16, fe80::/10, 100.100.100.200, fd00:ec2::254) are always refused.
* Credentials and the passphrase are removed from every error message shown or logged.
* CLI: `backup remote status|set|test|list|push|pull|delete`, `backup decrypt`. REST: `GET` / `POST
  /pursae/v1/backups/remote`.

## Storage and security

* Archives live in `PURSAE_BACKUPS_DIR` (default `<data>/backups`, next to uploads, never inside the public
  uploads directory) with mode 0600, named `backup-<time>-<random>.zip`.
* Download, delete, upload and restore need an administrator and a nonce; file names are validated against
  `^[A-Za-z0-9][A-Za-z0-9._-]*\.zip$`.
* A backup holds password hashes and site secrets; the screen says so.
* Uploads are streamed to disk (no memory buffering, 16 GB cap). Very large archives can instead be copied into
  the backups folder and appear in the list.
* The manifest lists a SHA-256 for every archive entry. A restore first reads the whole archive and
  refuses a damaged, tampered or half-copied one before anything changes (older archives without checksums
  are still read through, which checks the zip CRCs).
* Media that exists only in the Pursae CDN bucket ("Remove files from the server") is not in the archive;
  the job log warns about it and the manifest records `remote_only`.
* A restore unpacks files into a staging folder next to their target first; the database is replaced only
  after that succeeds, and only renames follow. If moving files then fails, the error names the safety
  snapshot to restore.
* Scheduled backups: hourly, daily or weekly, with a time of day (site time zone) and an e-mail to the
  admin address on failure (or after every run).
* One backup or restore runs at a time; progress is polled from `action=status`.

## Not included (yet)

Encrypting local archives with a password (only the remote copies are encrypted), bundling the CDN
bucket's files into the archive, FTP/SFTP and cloud drives as remote storage, restoring from the admin on a
site that is not installed yet (the CLI can), merge (non-destructive) restore.
