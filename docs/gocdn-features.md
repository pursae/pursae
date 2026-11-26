# Pursae CDN: feature inventory

Pursae CDN is a native (compiled-in) Pursae plugin for object storage, CDN delivery and on-the-fly
images. It covers the ground of **WP Offload Media** (Delicious Brains: offload to S3-compatible storage,
rewrite URLs, remove local files, bulk tools, `AS3CF_SETTINGS`), an **image CDN** (imgix / Cloudflare
Images style resize, crop, WebP and AVIF with signed URLs) and **CDN Enabler** (KeyCDN: rewrite theme,
plugin and core asset URLs to a pull CDN), with purging of **Pursae Cache** and one or more CDNs (**Cloudflare**,
**CloudFront**, **BunnyCDN**, **KeyCDN** or **Fastly**). Like every
native plugin it is **off on a fresh install** (`native_gocdn`), and while it is off nothing of it runs:
its screens and routes answer 404, uploads are served from disk and no response is rewritten — with one
deliberate exception: files Pursae CDN removed from the server keep being answered from the bucket after
deactivation (see "Deactivating the plugin" below).

Legend: **✓** implemented · **≈** implemented differently (reason given) · **✗** left out (reason given).

## 1. Storage connection (Pursae CDN › Storage)

| Feature | | Notes |
|---|---|---|
| Providers: Amazon S3, Cloudflare R2, DigitalOcean Spaces, Backblaze B2, MinIO / any S3 API | ✓ | One S3 client (`internal/gocdn`), provider presets fill the endpoint, region and public URL. |
| Endpoint, region, bucket, access key, secret key | ✓ | Empty endpoint = the provider's for the region. R2 uses region `auto`. The endpoint must be on a public address: a loopback or private one (a MinIO on your own network) needs `PURSAE_ALLOW_PRIVATE_ENDPOINTS=1`, or the endpoint set in `PURSAE_CDN_SETTINGS`. It is checked when saved and on every connection after DNS resolution; link-local and cloud metadata addresses are refused always. |
| Path-style / virtual-host-style addressing | ✓ | Toggle; MinIO and R2 default to path-style in the presets' help. |
| Key prefix ("Path in bucket") | ✓ | Default `wp-content/uploads/`, keys are `<prefix><year>/<month>/<file>`. |
| Object ACL `public-read` | ✓ | Toggle; off for buckets with ACLs disabled (the test explains AccessDenied). |
| Delivery domain (custom domain / CDN in front of the bucket) | ✓ | Mapped to the bucket root. Without it the provider's public bucket URL is used (R2 needs one). |
| Secret stored safely | ✓ | AES-GCM with a key derived from the server secret (`gocdnSeal`/`gocdnReveal`, the Pursae SMTP approach); never printed into a form: the field shows "•••• saved — type to replace" with a Clear box. Same for the Cloudflare token. |
| Configuration from the environment | ✓ | `PURSAE_CDN_SETTINGS` = JSON with the option's keys (`provider`, `endpoint`, `bucket`, `access-key-id`, `secret-access-key`, …, the names of `AS3CF_SETTINGS` where it has them). Keys set there override the database, are shown locked, and are never written to the database. |
| Test connection | ✓ | "Save and Test Connection" / Test: PUT a probe object, HEAD it (size check), GET it through the public delivery URL (warning only), DELETE it, ListObjectsV2. Each step is reported; S3 error codes get advice (SignatureDoesNotMatch → wrong secret, NoSuchBucket, AccessDenied/ACL, region mismatch, clock skew, unreachable endpoint). |
| SigV4 signer | ✓ | Own implementation (no AWS SDK), header signing and presigned URLs, checked against AWS's published S3 test vectors (GET object, PUT object, GET lifecycle, ListObjects, presigned GET). Also signs CloudFront invalidations. |
| Multipart upload | ✓ | Files from the "Multipart upload" size on (default 16 MB, 5 MB–5 GB) go through CreateMultipartUpload / UploadPart / CompleteMultipartUpload, read from the file in parts of the threshold's size (5–64 MB, raised so no file needs more than 10 000 parts); each part is retried on its own (3 tries) and a failed upload is aborted (AbortMultipartUpload) so no orphaned parts stay in the bucket. A 200 answer with an `<Error>` body is treated as an error. Smaller files: one PUT. Files are no longer read into memory whole. |
| Server-side encryption | ✓ | Encryption setting (`sse-type`): the bucket's default, **SSE-S3** (`x-amz-server-side-encryption: AES256`), **SSE-KMS** (`aws:kms`, optional `sse-kms-key-id`: key ID, alias or ARN; empty = the account's `aws/s3` key) on every PUT / CreateMultipartUpload / CopyObject, or **SSE-C** (`sse-c-key`: 32 bytes as 64 hex or 44 base64 characters). SSE-C sends the three `x-amz-server-side-encryption-customer-*` headers on PUT, CreateMultipartUpload, **every UploadPart, GET, ranged GET and HEAD**, and on CopyObject both for the new object and, as `x-amz-copy-source-…`, for the source (so a move between buckets or a key change by copy works); the client refuses SSE-C over plain http (except loopback). The key is sealed like the storage secret, never printed, never logged (errors carry only the S3 code and message), kept in a remembered previous connection so old objects stay readable after a bucket change. Because the headers cannot be put in a link, SSE-C objects have no public or presigned URL: media URLs are not rewritten to the bucket and removed files are always proxied through the site, whatever "Files removed from the server" says. Changing the key does not re-encrypt objects already uploaded (they are unreadable without the old key: keep it). The old `server-side-encryption` boolean still works (= SSE-S3). |
| Storage class | ✓ | Option: `x-amz-storage-class` (STANDARD_IA, INTELLIGENT_TIERING, GLACIER_IR, …; empty = bucket default). |

## 2. Media offload (Pursae CDN › Media Offload)

| Feature | | Notes |
|---|---|---|
| Copy new uploads and all generated sizes to the bucket | ✓ | Hooked after the attachment is inserted (async-upload, REST `wp/v2/media`, sideloads: WXR import, demo content, remote "copy" import) and after the image editor saves. Also the original of a `-scaled` image and the editor's backup file. `Cache-Control: public, max-age=31536000, immutable`, content type by extension. |
| Per-file offload state | ✓ | Table `gocdn_items` (migration `0021_gocdn.sql`): media id, uploads path, provider, region, bucket, key, size, MIME, `local_removed`. Rows go with the attachment (`ON DELETE CASCADE`). |
| Remove files from the server | ✓ | After every file of an attachment is in the bucket. |
| `/wp-content/uploads/…` for removed files | ✓ | Answered before the page cache: 302 to the delivery URL, a presigned URL (1 h, private buckets) or the file proxied through the site (with the uploads sandbox CSP). Files still on disk are served as before. |
| Server needs a removed file | ✓ | One helper, `gocdnFetchBack(ctx, path)` (`gocdn_fetch.go`), wraps every place that reads an upload from disk: the image editor (preview/save), the Customizer's site-icon sizes (which are then offloaded too), Pursae Commerce downloads and the PDF logo, Pursae Contact Form mail attachments. It downloads the file back when its row says "removed"; the copy is deleted again after 10 minutes while the row still says so; a daily housekeeping pass also deletes copies a restart left on disk, retries bucket deletes that failed when an attachment was deleted (option `gocdn_pending_deletes`), and resumes an interrupted job. The image service and `/wp-content/uploads` read the bucket directly (no disk write). |
| Delete attachment deletes remote objects | ✓ | Through `deleteAttachmentFiles`, used by every delete path. The URLs are sent to Cloudflare when automatic purge is on. |
| Bulk "Offload existing media" | ✓ | Background goroutine, one attachment at a time, progress in the option `gocdn_job` after each one; the screen polls `/wp-admin/gocdn?page=offload&action=progress` (JSON). Stop button. |
| Resumable | ✓ | A job interrupted by a restart is shown as interrupted; starting it again with "Resume" continues after the last attachment done; the housekeeping pass 45 seconds after start-up resumes it by itself. Jobs process up to 4 attachments in parallel (option `gocdn_job_workers`, 1 to 16); attachments that fail are retried once when the list is done, and the full error list downloads as CSV (`?action=errors-csv`). |
| "Download back to the server" / "Remove from the bucket" | ✓ | Same job runner. Remove downloads removed files first and refuses to delete the only copy. |
| Bucket / provider / path changed later | ✓ | Every row keeps provider, region, bucket, **endpoint** (migration `0023_gocdn_more.sql`) and key. When the Storage screen replaces a connection that still holds items, that connection is kept in the settings (`previous-connections`, secret sealed), so old items keep their old URLs (its delivery domain or public bucket URL) and are still served, fetched back and deleted there. Media Offload then offers **"Move to the current bucket"**: a resumable job with progress that copies each such file to the current bucket under the current path — CopyObject when old and new bucket share endpoint and access key, otherwise download to a temporary file and upload (multipart when large) — updates its row (so its URL switches), optionally deletes the old object, and drops connections no item uses any more. Rows written before 0023 have no endpoint and are taken to be on the configured endpoint; settings changed only through `PURSAE_CDN_SETTINGS` are remembered too: the connection in effect is stored (sealed) as `gocdn_conn_seen`, and the first load after the variable names another bucket, endpoint or provider keeps the old connection (with the old credentials) among the previous connections until no item uses it, so files offloaded to the old bucket keep being served. A connection first seen after an upgrade has no earlier record to remember. |
| Deactivating the plugin | ✓ | When files exist only in the bucket, the Plugins screen's Deactivate link opens a confirmation screen: **"Download them back to the server, then deactivate"** (the download job; Pursae CDN switches itself off when it finished without failures) or **"Deactivate now and keep serving them from the bucket"**. Any deactivation that leaves removed files (also the bulk action, which cannot ask) turns on the **uploads fallback** (option `gocdn_fallback`): while Pursae CDN is off, `/wp-content/uploads/<file>` for those files only is answered from the bucket (redirect / presigned / proxy as configured) and fetch-back keeps working; nothing else runs (no rewriting, image service or screens), and the Plugins row says so. The flag clears on activation and when no removed file is left. Chosen as the safest UX: URLs never break, and nothing has to be downloaded unless the admin asks. |

## 3. URL rewriting

| Feature | | Notes |
|---|---|---|
| Front end, feeds, sitemaps, REST API | ✓ | A gin middleware buffers HTML/XML/JSON responses and rewrites upload URLs (relative, or absolute on the site's own host) in `src`, `srcset`, `href`, CSS `url()` and JSON alike; another site's `/wp-content/uploads` URLs are untouched. Only offloaded files are rewritten (one batched lookup per page). |
| Custom domain | ✓ | The delivery domain. |
| Not in wp-admin, the login screen, or the REST API's `context=edit` | ✓ | So the editors keep and save local URLs. |
| Page cache | ✓ | The cache stores the rewritten page; every settings change, finished bulk job and import purges it. |
| CSP | ✓ | The delivery and asset CDN origins are added to the page's Content-Security-Policy when not covered by `https:`. |
| Reverse rewriting of content saved with CDN URLs | ✗ | Not needed while the editors get local URLs; content pasted with CDN URLs stays as typed. |

## 4. Remote storage browser (Media › Remote Storage)

| Feature | | Notes |
|---|---|---|
| Folder navigation (prefix + delimiter), breadcrumbs | ✓ | |
| Pagination | ✓ | 50 per page, ListObjectsV2 continuation tokens ("Next page", "First page"). |
| Thumbnails | ✓ | Image objects (≤ 20 MB) are fetched and shrunk to 160 px by an admin-only endpoint, so private buckets work too. |
| Import: Copy | ✓ | Downloaded like an upload (sizes generated; offloaded again under the library's own key when copying is on). |
| Import: Link (remote-only attachment) | ✓ | Registered with the key's path (`<key minus prefix>`, or `gocdn-remote/<key>`), width/height read from the first 512 KB, no local file, no sub-sizes; served through the uploads fallback. Only allowed upload types; duplicates and paths with a local file are skipped. |
| Use in the media picker | ✓ | A **"Remote storage" tab** in the `wp.media` modal (every frame built on `MediaFrame.Select`: Add Media, featured image, Customizer, widgets, Pursae Custom Fields…), for administrators while Pursae CDN is active and configured: folders, thumbnails, "Load more", import as copy or link. Clicking an object imports it and selects the new attachment (or the one already imported from that key) in the library, ready for Select / Insert. The tab's own strings are in its script and are not translated. |

## 5. Image service (Pursae CDN › Image Service)

| Feature | | Notes |
|---|---|---|
| Endpoint | ✓ | `/cdn-img/<path in uploads>?w=&h=&fit=&q=&fm=&s=`, and the same parameters on `/wp-content/uploads/<path>`. Aliases `width`, `height`, `quality`, `format`; fits `inside` (default, `contain`, `scale-down`), `cover` (`crop`), `fill`. Never upscales. |
| Output formats | ✓ | JPEG, PNG, GIF, **WebP** (libwebp transpiled to pure Go by `github.com/gen2brain/webp`) and **AVIF** (libavif + aom compiled to WebAssembly, run by wazero: `github.com/gen2brain/avif`; no cgo, `CGO_ENABLED=0` builds; release builds — `make build`, the Docker image — use `-tags nodynamic`, so they are statically linked and always use these bundled encoders; only a build without that tag would load a system libavif/libwebp through purego when installed). AVIF is a setting (off by default): measured on the test machine, a 1000×667 image at speed 8 takes ~150 ms (WebP ~130 ms) after a one-time ~0.9 s module compile; every encode still holds one slot of the per-CPU limit. Without the setting `fm=avif` answers WebP. |
| Animated GIFs | ✓ | Every frame is composed (offsets, disposal), resized and written as an animated GIF with the original delays and loop count, each frame on its own palette (no dithering). With `fm=auto` an animated GIF is answered as an **animated AVIF** for browsers that accept it (AVIF on), else an **animated WebP** (WebP on), else GIF; `fm=webp|avif|gif` force one (`EncodeAll` of `gen2brain/webp` and `gen2brain/avif`; the frames stay true colour, delays and loop count kept; measured: 12 frames of 240×160 take ~90 ms as WebP and ~0.9 s as AVIF, so the budget below bounds the AVIF cost too; variants are cached). Budget: at most "Animated GIFs" frames (default 200) and frames × (source + output pixels) ≤ 3 × the source pixel limit; over it the first frame is served (`X-GoCDN-Animation: flattened`). `fm=jpeg/png` gives a still. Whether a GIF is animated is found by walking its blocks once per source version. |
| Format negotiation | ✓ | `fm=auto` sends AVIF (when on) if `Accept` lists `image/avif`, else WebP if it lists `image/webp` (q > 0), else the source format, with `Vary: Accept`. |
| Quality | ✓ | Default quality setting, `q=` per request. |
| Signed URLs | ✓ | HMAC-SHA256 of the path and the canonical parameters with a random site key (stored sealed; "Make a new signing key" rotates it); parameter order and aliases don't matter. Unsigned / tampered requests get 403 when signing is on. |
| Abuse limits | ✓ | Max output width/height, max source megapixels (checked with `DecodeConfig` before decoding), max source 64 MB, the animation budget, one transformation per CPU at a time (AVIF included), path traversal refused. |
| Cache headers | ✓ | `Cache-Control: public, max-age=31536000, immutable`, strong `ETag`, `304` on `If-None-Match`. |
| Variant cache | ✓ | On disk (`<data>/gocdn-cache/`), optionally in the bucket (`gocdn-cache/…`, shared by several servers). Keyed by path, source version, parameters, quality and output format. "Empty the processed image cache". |
| Source offloaded / removed locally | ✓ | Read from the bucket. |
| Content images through the service | ✓ | Option: every upload image on the front end (src/srcset) becomes a signed `/cdn-img/` URL at a set quality, so browsers get WebP. |
| URL builder | ✓ | On the screen, for templates and testing; `gocdnImageURL` for code. |

## 6. Assets CDN (Pursae CDN › Assets CDN)

| Feature | | Notes |
|---|---|---|
| CDN base URL | ✓ | |
| Included folders | ✓ | Default `/wp-content/themes/`, `/wp-includes/`, `/wp-content/plugins/`, `/wp-admin/assets/`; other `/wp-admin/` paths and uploads are refused. |
| Included extensions, exclusions | ✓ | Comma list; substrings one per line. |
| Front-end HTML only, never wp-admin or JSON | ✓ | Relative and own-host absolute URLs; query strings (`?ver=`) kept. CDN origin added to `script-src`, `style-src`, `font-src`, `img-src`, `media-src`, `connect-src`. |

## 7. Purge (Pursae CDN › Purge)

| Feature | | Notes |
|---|---|---|
| Pursae Cache purge on settings changes, finished jobs, imports | ✓ | `purgeAll`. |
| Purge button | ✓ | Pursae Cache, every configured CDN (everything; "Purge all CDNs (…)" when there are several), processed image cache. |
| CDN provider selector | ✓ | None, Cloudflare, Amazon CloudFront, BunnyCDN, KeyCDN, Fastly; the screen shows the chosen provider's fields. Secrets sealed (AES-GCM, like the storage secret), never echoed, cleared when switching provider. Optional automatic purge (everything with Pursae CDN's automatic purges, the URLs of deleted offloaded files). API errors are shown. |
| Several providers at once | ✓ | The provider selector is the main one; "Additional providers" on the Purge screen ticks any of the others, each with its own zone / key ID / secret (`purge-extra` in the settings; sealed, never echoed; a secret left empty keeps the saved one). Every purge (button, automatic purge, URLs of deleted files) goes to all of them concurrently; one failing or slow provider does not hold up the others, and the result names each failure ("Purge failed at Fastly: 401 …. Purged: Cloudflare, BunnyCDN."). Background purges log one warning per failing provider. The old single-provider settings and form keep working. |
| Cloudflare | ✓ | Zone ID + API token (Zone › Cache Purge). `purge_cache` with `purge_everything` or `files` (30 per request). |
| Amazon CloudFront | ✓ | Distribution ID + IAM access key / secret (`cloudfront:CreateInvalidation`). `POST /2020-05-31/distribution/<id>/invalidation`, signed with Pursae CDN's SigV4 signer (service `cloudfront`, `us-east-1`); everything = `/*`, URLs → their paths (3000 per invalidation). |
| BunnyCDN | ✓ | Pull zone ID + account API key (`AccessKey` header): `POST /pullzone/<id>/purgeCache`, URLs one by one with `POST /purge?url=`. |
| KeyCDN | ✓ | Zone ID + API key (HTTP basic): `DELETE /zones/purge/<id>.json`, URLs (without scheme, 200 per request) with `DELETE /zones/purgeurl/<id>.json`. |
| Fastly | ✓ | Service ID + API token (`Fastly-Key`): `POST /service/<id>/purge_all`, URLs with `POST /purge/<host/path>`. |
| Surrogate-key / tag purges | ✗ | Purges are by URL or everything (on every configured provider). |

## 8. Translations

Pursae CDN's screens are translated after rendering like the rest of the admin (`internal/i18n`, Pursae's
own strings in `web/languages/wpgo-<locale>.json`). Turkish has every Pursae CDN admin string (brand names stay
as they are); in the other languages the strings added in the second round (AVIF, animations, multipart,
SSE, storage class, move, purge providers, deactivation screen) are still English. Numbers are kept out of
translatable sentences. The media-modal tab's strings are injected by the server (`gocdnModalStrings`) and translated where a catalog has them (Turkish does).

## 9. Not done / limits

- AVIF *sources* are not accepted by the image service.
- Cache tags / surrogate keys.
- SSE-C: no public or presigned URLs (see the encryption row); rotating the customer key does not re-encrypt
  existing objects (a "move" to another bucket does re-encrypt: it reads with the old connection's key and writes
  with the current one).
- Signed delivery of the *rewritten* URLs (presigned URLs in page HTML) is not offered: pages are cached;
  private buckets should use "presigned redirect" or "proxy" delivery for removed files.
- The media-modal tab is for administrators (`manage_options`), like Media › Remote Storage.

## Housekeeping additions

- New uploads are offloaded in the background (retried after 0s, 5s, 30s and 2min); the upload request no longer waits for the bucket. Uploads still failing after that are recorded in `gocdn_offload_failures` (migration 0098): the daily housekeeping pass and the Media Offload screen's **Retry failed** button try them again.
- Saving or changing a post queues a CDN purge (when "purge automatically" is on) of its permalink, the home page and the main feed.
- Media Library list: a "Storage" column (administrators) shows whether a file is in the bucket, with row actions Offload, Download back and Remove local copy.
- Image service: "Image CDN URL" (`img-cdn-url`) makes content image URLs point at a CDN pull zone whose origin is the site; "Image cache limit" (`img-cache-max-mb`, default 2048) prunes the oldest processed variants.
- **Find objects without a record** (Media Offload › Housekeeping) lists up to 500 objects under the uploads prefix that no `gocdn_items` row points at (the image cache and pending deletes are excluded); **Delete them from the bucket** removes them after a confirmation, skipping any key that got a record meanwhile.
- **Object versioning** (`object-versioning`, Storage settings): new objects get a time-stamped folder in their key (`<prefix><dir>/<YYYYMMDDhhmmss>/<file>`, as WP Offload Media), so a file written again under the same name gets a new URL. Existing objects keep their keys; the move job accepts both forms.
- Image-editor saves, restores and thumbnail regeneration: files written again under the same name have their old bucket object deleted, the bucket URL and the site URL purged on the CDN, and their image-service variants dropped before the new files are offloaded. Image-service variants live in one folder per source file (`gocdn-cache/<h2>/<hash>/`), so deleting an attachment deletes its variants too.

## Private files (protected downloads)

- An attachment is private when its files live under `uploads/private/` — the folder is the flag. Media Library list rows have **Make private / Make public** (nonce-checked; uploader or `edit_others_posts`). Making a file private moves its files, deletes its moved-path redirects, re-offloads it and purges the old URLs: the old public URL stops working at once.
- `/wp-content/uploads/private/…` is never served statically: only a link signed with an HMAC of the server secret that has not expired (`?e=…&s=…[&dl=1]`). Admin screens get links valid 1–2 h for users with `upload_files`; everybody else sees a dead URL. The image service refuses private files; URL rewriting leaves them alone.
- In the bucket, private objects get no public ACL, `Cache-Control: private, no-store`, and a key folder derived from an HMAC (unguessable even in a public-by-policy bucket). They are delivered only through 5-minute presigned URLs (SSE-C objects are proxied with `no-store`).
- Pursae Commerce: files chosen with the downloadable file "Choose file" upload straight into `uploads/private/` (`type=downloadable_product`, like WooCommerce's `woocommerce_uploads`). The download handler checks the order's permissions first, then streams the local file, or (with "Redirect only") redirects to a 5-minute signed link, or to a presigned bucket URL when the file is only in the bucket. A product that kept the public URL of a file made private later still works.
