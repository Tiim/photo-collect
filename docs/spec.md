# Requirements Document — Event Photo Collection Service

## 1. Purpose

Build a self-hosted Go web service for collecting photographs from anonymous participants of an event.

Anonymous participants receive a temporary upload magic link and can upload photographs through a browser. They must provide a nickname. Authenticated users can browse, rate, tag, manage, and download photographs.

The service is intended for a small sports club and should favor simplicity, reliability, and low operational overhead over horizontal scalability.

## 2. Core concepts

The primary organizational unit is a **Folder**.

A folder:

- Has a name.
- Contains photographs.
- Is accessible to all authenticated users.
- Has exactly one anonymous upload link at a time.
- Has configurable standard tags.
- Can be deleted by any authenticated user.
- Deletion must eventually remove its stored images to save storage costs.

The system does not need an explicit concept of an event beyond the folder.

## 3. Users and authentication

Authentication shall use OIDC.

Requirements:

- Support one configurable OIDC provider.
- The OIDC provider must be configurable; Authentik is the primary expected provider.
- After successful OIDC authentication, the application creates its own authenticated session.
- Sessions should use secure, opaque cookies.
- All authenticated users have the same permissions.
- There are no administrator/manager roles.
- Any authenticated user can:
    - Create folders.
    - Delete folders.
    - Create/revoke/extend upload links.
    - View all folders.
    - View all images.
    - Rate images.
    - Add/remove tags.
    - Download images.

No anonymous user may view existing images.

## 4. Anonymous upload

Each folder can have one anonymous upload magic link.

The link should have the general form:

```text
https://<domain>/upload/<random-token>
```

Requirements:

- The token must be cryptographically random and unguessable.
- The link has a configurable expiration date/time.
- An authenticated user can:
    - Create the link.
    - Extend/regenerate the link.
    - Revoke the link.
- There is only one active upload link per folder.
- Expired links must explicitly show that the link has expired.
- Possession of the upload link grants upload capability only.
- The upload link must not grant access to existing photographs.
- Forwarding the upload link is considered acceptable.

### Upload session

The uploader must enter a nickname.

The nickname:
- Is requested once per upload session.
- Is stored indefinitely in a browser cookie.
- Is reused for subsequent uploads from that browser.
- Is recorded against each uploaded image.
- Cannot subsequently be changed for an already uploaded image.
- Is visible to authenticated users.

The uploader does not need an account.

## 5. Upload UI

The anonymous upload interface shall be a web page.

Requirements:
- Drag-and-drop support.
- Multiple files per upload.
- Normal file selection through the browser.
- Upload progress should be displayed where practical.
- Images should become visible to authenticated users as soon as the image record/original is successfully stored.
- Thumbnail/preview generation may happen asynchronously after the upload.

The frontend should use server-rendered HTML with HTMX rather than a large JavaScript SPA.
Minimal vanilla JavaScript is accepptable.

## 6. Image formats and validation

The service accepts images only.
At minimum, support:
- JPEG
- PNG
- WebP
- HEIC/HEIF

The implementation should be extensible to additional image formats.

Requirements:
- The actual file content must be validated.
- The filename extension must not be trusted as proof that a file is an image.
- Invalid/non-image files must be rejected.
- Animated image formats should be rejected.
- Maximum individual file size must be configurable.
- Maximum number of images per folder must be configurable.
- Each file is uploaded on its own, in chunks that resume after a dropped connection; the chunk size should be configurable.

Initial expected scale is approximately 5,000 images per folder, with approximately 7,000–8,000 images across the largest expected deployment.

## 7. Original image preservation

Original photographs are immutable.

The service must:
- Preserve the original uploaded bytes.
- Preserve the original image metadata/EXIF.
- Not strip EXIF.
- Not re-encode the original.
- Not modify the original image when adding ratings or tags.

The original filename must be retained as metadata.
The service should record basic technical metadata such as:
- MIME type
- File size
- Image dimensions
- Original filename
- Upload timestamp
- SHA-256/hash where useful for integrity

### Camera clock calibration

Cameras often have a wrong clock or time zone. The public start page (`/`) shows the current
time as text and as a QR code (`PC1|<utc unix seconds>|<local wall time>`); people photograph it
with each of their devices and upload the photo like any other.

- A background job scans every upload for such a QR code. A hit marks the image as a calibration
  photo (tag `calibration`, excluded from "download all").
- A device is identified by uploader nickname + EXIF Make + Model (+ body serial when present).
  Images without Make/Model or capture time cannot be corrected.
- The offset is the QR wall time minus the photo's EXIF time. Each image of the same device in
  the same folder uses the nearest calibration photo (by EXIF time). Offsets are recomputed when a
  calibration photo arrives, so upload order does not matter.
- Corrections are stored in the database only (never in the original, see above) and written to
  the XMP sidecar (`xmp:CreateDate`, `exif:DateTimeOriginal`) of corrected images.
- Accuracy is about one second.

### Duplicate detection

The service flags duplicate photos within a folder; it never deletes anything automatically.

- Exact, byte-identical re-uploads (same SHA-256) within the same folder are skipped silently at
  upload time: no second image row or storage object is created, and the uploader sees a normal
  success response. This is race-safe under concurrent uploads of the same file (enforced by a
  database uniqueness constraint on folder + SHA-256, not just an application-level check).
- Near-duplicates (the same picture re-encoded or resized) are detected via a perceptual hash
  computed for every image and compared, within the same folder only, against every other image's
  hash. A match flags the pair for manual review; both images are kept until a person acts.
- Comparison runs in a background job, debounced to a fixed delay after the folder's last upload
  or hash computation, so a burst of uploads triggers one scan rather than one per photo.
- Only authenticated folder viewers/admins see flagged pairs and can resolve them (keep one and
  move the other to the trash, merging the trashed photo's tags and rating onto the kept one; or
  dismiss the pair as not a duplicate). Pairs are hidden while either photo is trashed.
  Anonymous uploaders never see any of this.
- The similarity threshold is a fixed, conservative constant, not configurable per folder.

## 8. Generated images

For every valid original image, the service shall asynchronously generate:
- Thumbnail
- Preview

Generated images are derived artifacts and may be regenerated from the original.
They do not need to preserve the original EXIF metadata.
Image processing should happen in a background worker rather than blocking the upload request.
Image processing should be isolated behind an interface so the rest of the application is not tightly coupled to a particular image-processing implementation.
HEIC support must be considered explicitly because iPhone photographs are an expected input.

## 9. Image storage

Image storage must be pluggable.
The application shall support:
1. Local filesystem
2. S3-compatible object storage
3. WebDAV (e.g. Hetzner Storage Box)
    

The S3 implementation should also support S3-compatible systems such as MinIO.
The filesystem backend shall use a configurable root directory, for example:

```text
STORAGE_PATH=/data/photos
```

The application must not depend on S3-specific semantics in the core domain.
A storage abstraction should provide at least:

```go
Put
Get
Delete
List
```

The storage backend is responsible only for image/object storage. SQLite remains the source of truth for application metadata.

## 10. Storage object naming

Original filenames must never be used as storage keys.
Storage keys should use generated internal identifiers, for example:

```text
folders/<folder-id>/images/<image-id>/original
folders/<folder-id>/images/<image-id>/preview
folders/<folder-id>/images/<image-id>/thumbnail
```

The original filename is stored separately in SQLite.
This avoids filename collisions and prevents user-controlled filenames from becoming storage paths.

## 11. Filename handling

Original filenames must be preserved for display and downloads.
When multiple images have the same filename in a download, filenames must be made unique using an `_N` suffix.
Example:

```text
IMG_1234.jpg
IMG_1234_2.jpg
IMG_1234_3.jpg
```

Do not use the `(2)` convention.

## 12. Tags

Tags are shared across all authenticated users.
Any authenticated user can:
- Add a tag.
- Remove a tag.
- Create a new tag.
- Apply an existing tag.

Tags are free-form but folders can define standard tags.

### Standard tags

Each folder may have a configurable set of standard tags.
Every newly uploaded image automatically receives all standard tags of its folder.
Example:

```text
Folder standard tags:
    football
    2026
    tournament
```

An uploaded image automatically receives:

```text
football
2026
tournament
uploader/alice
```

Users can add additional tags such as:

```text
goal
celebration
team-red
```

Standard tags should be represented as folder configuration rather than duplicated configuration data on every image.

### Uploader tag

The uploader nickname must also be represented as an image tag:

```text
uploader/<nickname>
```

The nickname should be sluggified (lowercase) appropriately so arbitrary user input cannot create malformed tags or security issues.
The application may additionally store the uploader nickname as image metadata if useful, but the tag is mandatory.

### Tag storage

Tags should be normalized in SQLite rather than storing duplicated tag strings on every image.
Tag uniqueness should be case-insensitive. The exact presentation/casing policy should be consistent throughout the UI.

## 13. Rating

Each image has exactly one shared rating.
Rating values are:

```text
(none)
1
2
3
4
5
```

Requirements:

- Any authenticated user can change the rating.
- There is no per-user rating.
- The rating is visible to all authenticated users.
- Updates use normal atomic database operations.
- Last write wins if two users update simultaneously.
- The database must enforce the valid NULL, 1–5 range.

## 14. Image browsing

Authenticated users can browse all folders and all images.
Requirements:
- Images should be displayed using thumbnails.
- Preview images should be used where appropriate.
- An image can be opened at a larger preview size.
- The original can be requested by authenticated users.
- Images must never be accessible to unauthenticated users.
- The UI should display:
    - Original filename
    - Uploader
    - Rating
    - Tags
    - Relevant image metadata

### Filtering
The folder grid can be filtered and sorted through query parameters: `tag` (repeatable, at most 10) with `tag_mode=all|any`, `rating_min`, `rating_max` (1-5), `uploader`, `from` / `to` (dates, on the corrected capture time), `has_gps=1`, `sort=uploaded|captured|rating` and `dir=asc|desc`. Invalid values are rejected with 400. Paging uses a keyset on the sort key and the upload sequence number. "Download all matching" and "Move all matching to trash" resolve the filter on the server; trashing all matches requires an active filter and the match count the user saw.

### Locations
The EXIF GPS position (range-checked, exact 0,0 ignored) is stored per image at upload; a background job fills it in for older images once. Positions are shown only to signed-in users, as coordinates with an openstreetmap.org link in the image detail and on a Leaflet folder map (vendored, standard OpenStreetMap tiles, opened on demand) of the current filter (`GET /folders/{id}/map.json`, id and position only). `img-src` allows the tile host only on pages that can show a map. Derivatives contain no EXIF; the XMP sidecar in exports contains the position.

Images should be visible even if thumbnail generation has not completed. A placeholder/loading state can be used until the derived image is available.

The expected number of images is in the hundreds to several thousand, so pagination or incremental loading should be used rather than rendering thousands of images into one HTML response.

### Language
The UI is available in English and German (informal "du"). The language comes from `?lang=` (stored in a `lang` cookie), then the cookie, then `Accept-Language`, then English. All user-facing texts, including validation errors, upload results and script messages, come from the catalogs in `web/locales/`; dates and sizes follow the language. Logs and user-entered data (tags, names) are not translated.

## 15. Downloads

Authenticated users can download:

- Selected images.
- All images in a folder.

Downloads should contain the original image files, not resized versions.
Rating and tags must be included in the download.
The representation is standard photography-compatible XMP sidecar files.

Example:

```text
photos/
    IMG_1234.jpg
    IMG_1234.xmp

    IMG_5678.jpg
    IMG_5678.xmp
```

The original image itself must not be modified to include application metadata.
The XMP should contain, as appropriate:
- 1–5 star rating
- Image tags
- Uploader information

The uploader is represented in the application's tags as:

```text
uploader/<nickname>
```

SQLite remains the canonical source for ratings/tags. XMP files are generated during export.

### Large downloads

Download generation should be asynchronous.
Recommended flow:

```text
User selects images
        ↓
Create download/export job
        ↓
Background worker
        ↓
Read originals
        ↓
Generate XMP sidecars
        ↓
Create ZIP
        ↓
Make ZIP available
        ↓
Authenticated user downloads ZIP
```

This avoids tying an HTTP request to potentially thousands of image reads.
Temporary export files should eventually be deleted.
The exact lifecycle/expiration of generated download archives should be configurable or clearly defined during implementation.

## 16. Folder deletion

Any authenticated user can delete a folder.
Deletion should be treated as a destructive operation.
Expected behavior:
1. Folder is marked deleted/inaccessible immediately.
2. Its upload link is invalidated.
3. A background job removes all associated storage objects.
4. The database records associated with the folder are eventually removed or retained according to the chosen cleanup strategy.
5. Storage cleanup must be retryable.
    

Deleting thousands of images must not require the HTTP request to synchronously delete every object.

The purpose of background deletion is specifically to reduce storage costs and avoid long-running HTTP requests.

### Photo trash and orphan cleanup

- Any authenticated user can move photos of a folder to its trash (soft delete: `images.deleted_at`,
  `images.deleted_by` = user id) and restore them. Trashed photos are excluded from the grid,
  counts, exports, duplicate review, the pHash backfill and the image routes (404, except the
  thumbnail shown in the trash view).
- Purging is only possible from the trash. It deletes the image row (cascading to tags, duplicate
  pairs and export links) and, in the same transaction, enqueues a retryable
  `delete_image_objects` job for the stored objects.
- There is no automatic expiry. Deleting a folder removes trashed photos too.
- A `sweep_orphans` job (startup, then daily) deletes stored image objects with no image row and
  older than 24 hours, and warns about image rows without an original. It refuses to run when the
  candidates are 20 % or more of all objects and at least 100.

## 17. Background jobs

The application requires background processing for at least:

- Thumbnail generation.
- Preview generation.
- Folder/image storage deletion (including purged photos and the orphan sweep).
- ZIP/XMP download generation.

A durable SQLite-backed job queue is preferred.

Example conceptual model:

```text
jobs
    id
    type
    payload
    status
    attempts
    created_at
    started_at
    finished_at
    error
```

Jobs must be idempotent and survive application/container restarts.
Failed jobs should be retryable.
No external queue such as Redis or RabbitMQ is required.

## 18. Database

SQLite is the only required database.

The application should be designed specifically for SQLite rather than attempting to support multiple relational databases.

SQLite must contain the canonical application state, including:

- Users
- Sessions
- Folders
- Upload links
- Upload sessions
- Images
- Ratings
- Tags
- Image/tag relationships
- Background jobs
- Any export/download state

Database migrations must be supported.

The service must provide a mechanism to initialize and migrate the database automatically on startup or through a suitable startup command.

## 19. Expected scale

The initial deployment is for a small sports club.

Expected scale:
- Approximately 5–10 active folders/events.
- 2–3 concurrent anonymous uploaders under normal conditions.
- Approximately 7,000–8,000 images across the deployment.
- Up to approximately 5,000 images in a single folder.
- No requirement for high availability.
- No requirement for horizontal scaling.
- No requirement for multi-node operation.

The architecture should nevertheless avoid obvious bottlenecks such as loading complete images into RAM or generating large downloads synchronously.

## 20. Security model

There are two distinct authentication mechanisms.

### Anonymous

The upload magic link grants:

```text
upload to one folder
```

It does **not** grant:

```text
view images
download images
view tags
view ratings
manage the folder
```

Tokens must be cryptographically random and practically impossible to guess.

### Authenticated

An authenticated OIDC user can access all application resources.
Image access must still pass through application authorization.
Images should not be exposed using publicly accessible storage URLs.
The Go service should proxy image downloads from storage.
The application should use secure cookie settings appropriate for HTTPS.

## 21. Upload security

The upload implementation must:
- Enforce configurable maximum file size.
- Enforce configurable maximum files/request.
- Enforce configurable maximum images/folder.
- Validate actual image content.
- Reject non-images.
- Reject animated formats.
- Avoid path traversal through filenames.
- Avoid using filenames as storage keys.
- Stream uploads rather than loading entire files into memory.
- Avoid leaving orphaned storage objects when validation fails.
- Handle malformed image files safely.

Malware scanning is not required initially.

Abuse protection (a leaked link must not exhaust CPU or disk):
- In-memory rate limits per client IP and per upload link on the upload routes, per IP on the login
  routes and on nickname changes. Exceeding a limit returns `429` with `Retry-After`; the upload
  page treats this as retryable and backs off.
- A cap on upload requests ingested concurrently (`503` with `Retry-After` beyond it).
- ZIP exports: a cap on concurrent builds and on the total size of the originals per export.
- No per-folder or per-nickname byte quotas.

## 22. HTTP architecture

The application is UI-first rather than API-first.

The primary interface is:

```text
HTML
HTMX
Server-side Go templates
```

A separate SPA is explicitly not required.

The service should still have clean internal HTTP handlers/domain boundaries so an API could be introduced later if necessary.

Representative routes could include:

```text
/auth/login
/auth/callback
/auth/logout

/

/folders
/folders/new
/folders/<id>
/folders/<id>/delete
/folders/<id>/trash
/folders/<id>/images/trash
/folders/<id>/images/restore
/folders/<id>/images/purge

/folders/<id>/upload-link
/folders/<id>/upload-link/revoke
/folders/<id>/upload-link/extend

/upload/<token>
/upload/<token>/chunked
/upload/<token>/chunked/<id>
/upload/<token>/chunked/<id>/complete

/images/<id>
/images/<id>/original
/images/<id>/preview
/images/<id>/thumbnail

/images/<id>/rating
/images/<id>/tags

/folders/<id>/downloads
/downloads/<id>
```

Exact routing is an implementation detail.

## 23. Application architecture

The service should be implemented as a single Go application/binary.
Recommended logical components:

```text
cmd/photos/

internal/
    auth/
    config/
    database/
    domain/
    http/
    images/
    storage/
        filesystem/
        s3/
    jobs/
    downloads/
    oidc/
    sessions/

web/
    templates/
    static/

migrations/
```

The architecture should favor straightforward Go code over excessive abstraction.

In particular, repository interfaces should not be introduced purely for theoretical database portability. SQLite is the intended database.

The storage backend is the major abstraction boundary because multiple storage implementations are an explicit requirement.

## 24. Deployment

The service must be deployable using Docker.

The expected deployment is a single container with persistent storage for:

```text
SQLite database
```

and, when using filesystem storage:

```text
Image storage
```

Example:

```text
Docker container
    │
    ├── /data/photos.db
    │
    └── /data/photos/
```

For S3-compatible storage, image persistence is external to the container.

The application should be stateless with respect to transient runtime state wherever practical.

## 25. Configuration

Configuration shall primarily use environment variables.

Configuration should include at least:

```text
DATABASE_PATH

STORAGE_BACKEND
STORAGE_PATH

S3_ENDPOINT
S3_BUCKET
S3_REGION
S3_ACCESS_KEY
S3_SECRET_KEY

OIDC_ISSUER_URL
OIDC_CLIENT_ID
OIDC_CLIENT_SECRET
OIDC_REDIRECT_URL
OIDC_REQUIRE_VERIFIED_EMAIL
TRUSTED_PROXIES

SESSION_SECRET

UPLOAD_MAX_FILE_SIZE
UPLOAD_CHUNK_SIZE
UPLOAD_MAX_PENDING
UPLOAD_MAX_IMAGES_PER_FOLDER
UPLOAD_MAX_PIXELS
UPLOAD_MAX_CONCURRENT

RATE_UPLOAD_PER_IP
RATE_UPLOAD_PER_LINK
RATE_AUTH_PER_IP
RATE_NICKNAME_PER_IP

EXPORT_MAX_CONCURRENT
EXPORT_MAX_BYTES

UPLOAD_LINK_DURATION

THUMBNAIL_SIZE
PREVIEW_SIZE
```

Exact variable names can be adjusted during implementation.

Secrets must not be committed to the repository or stored in the database.

## 26. Logging and operational behavior

The application shall provide structured logging.

Important events should be logged, including:
- Authentication failures.
- Authentication success where appropriate.
- Folder creation/deletion.
- Upload failures.
- Successful image uploads.
- Image-processing failures.
- Storage failures.
- Background job failures.
- Download/export failures.
    

Logs must not contain:
- OIDC client secrets.
- Session secrets.
- Magic-link tokens.
- Other credentials.

## 27. Health/readiness

The service shall provide health/readiness endpoints.

At minimum, readiness should be capable of detecting important application failures such as:
- Database unavailable.
- Required storage backend unavailable.
    

The exact distinction between liveness and readiness can follow standard Docker deployment practices.

## 28. Backups and recovery

The service shall provide a practical backup strategy.

At minimum, documentation should explain how to back up:

- SQLite database.
- Filesystem image storage, when used.
    

For S3, storage backup/versioning should be handled according to the selected S3 deployment.

The implementation should provide a safe mechanism for backing up the SQLite database without corrupting an active database.

Restoration procedures should be documented.

## 29. Explicit non-requirements

The initial implementation does not require:

- PostgreSQL.
- Redis.
- RabbitMQ/Kafka.
- Kubernetes.
- Horizontal scaling.
- High availability.
- Multiple OIDC providers.
- Per-user roles.
- Per-user ratings.
- Anonymous image viewing.
- Anonymous image deletion.
- Anonymous image management.
- Video uploads.
- Full-text search.
- Advanced filtering.
- Audit logging.
- Malware scanning.
- Rate limiting.
- Public image URLs.
- Direct browser-to-S3 uploads.
- Direct browser-to-filesystem uploads.
- Native mobile applications.
- SPA frontend.
- Admin CLI.
- Multiple storage backends for a single deployment/folder.
    

## 30. Key architectural principles

The implementation should follow these principles:
1. **SQLite is the source of truth.**
2. **Original photographs are immutable.**
3. **EXIF and original bytes must never be modified.**
4. **Derived previews/thumbnails are disposable.**
5. **Storage keys are generated IDs, never user filenames.**
6. **Anonymous upload capability is completely separate from authenticated image access.**
7. **All authenticated users have equal permissions.**
8. **Tags and ratings are shared.**
9. **Background work must not block normal HTTP requests.**
10. **Background jobs should survive container restarts.**
11. **Storage is pluggable; the rest of the application should not depend on S3.**
12. **Keep the deployment simple: one Go service, SQLite, and configurable image storage.**
13. **Use standard XMP sidecars for exported metadata rather than modifying original photographs.**
14. **Optimize for the actual scale of a small sports club rather than hypothetical large-scale SaaS requirements.**
