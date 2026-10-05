# Photo Collect

Self-hosted service for collecting photos from anonymous participants of an event.
Participants upload through a temporary magic link; signed-in users browse,
rate, tag and download the photos. One Go binary, SQLite, and local, S3 or WebDAV storage.

See [docs/spec.md](docs/spec.md) for the requirements.

## Running

```sh
docker compose -f docker-compose.example.yml up
```

This pulls the prebuilt image `ghcr.io/tiim/photo-collect` (tags: `edge` = latest `main`,
`X.Y.Z` / `X.Y` for releases, `sha-<commit>`). To build from source instead, swap the
`image:` line for `build: .` in the compose file and run `up --build`.

The container persists everything under `/data` (`photos.db`, `photos/`, `exports/`).

### Configuration (environment variables)

| Variable | Default | Description |
|---|---|---|
| `BASE_URL` | – (required) | Public URL, e.g. `https://photos.example.com` (used for links, cookies, origin checks) |
| `LISTEN_ADDR` | `:8080` | Listen address |
| `DATABASE_PATH` | `/data/photos.db` | SQLite file (migrations run automatically at startup) |
| `STORAGE_BACKEND` | `filesystem` | `filesystem`, `s3` or `webdav` |
| `STORAGE_PATH` | `/data/photos` | Root directory for the filesystem backend |
| `S3_ENDPOINT` / `S3_BUCKET` / `S3_REGION` / `S3_ACCESS_KEY` / `S3_SECRET_KEY` | | S3-compatible storage (MinIO works); `S3_PATH_STYLE` defaults to `true` |
| `WEBDAV_URL` / `WEBDAV_USER` / `WEBDAV_PASSWORD` / `WEBDAV_BASE_PATH` | | WebDAV storage, e.g. a Hetzner Storage Box (`https://uXXXXXX.your-storagebox.de`); `WEBDAV_BASE_PATH` defaults to `/`. Enable WebDAV in the Hetzner console and prefer a sub-account restricted to one directory |
| `OIDC_ISSUER_URL` / `OIDC_CLIENT_ID` / `OIDC_CLIENT_SECRET` | – (required) | OIDC provider |
| `OIDC_REDIRECT_URL` | `$BASE_URL/auth/callback` | Register this redirect URI with the provider |
| `OIDC_REQUIRE_VERIFIED_EMAIL` | `true` | Reject sign-ins whose `email_verified` claim is present and `false` (a missing claim is accepted) |
| `TRUSTED_PROXIES` | – (trust nothing) | Comma-separated IPs/CIDRs of reverse proxies whose `X-Forwarded-For` is believed; `private` means RFC 1918, loopback and IPv6 ULA. Used to determine the client IP for logs and rate limits; `BASE_URL` stays the source of truth for links, cookies and the origin check |
| `SESSION_SECRET` | – (required, ≥32 chars) | Signs the nickname and login-state cookies |
| `SESSION_TTL` | `720h` | Session lifetime (sliding) |
| `UPLOAD_MAX_FILE_SIZE` | `52428800` | Bytes per file |
| `UPLOAD_MAX_IMAGES_PER_FOLDER` | `5000` | |
| `UPLOAD_MAX_PIXELS` | `60000000` | Maximum pixels (width x height) per image. Peak decode memory is about `WORKER_COUNT` x 4 bytes x pixels, i.e. ~240 MB per worker at the default |
| `UPLOAD_MAX_CONCURRENT` | `2 x WORKER_COUNT` | Upload requests ingested at the same time; further requests get `503` with `Retry-After` and the upload page retries them |
| `UPLOAD_CHUNK_SIZE` | `524288` | Bytes per request when the upload page sends a file. Files are uploaded in chunks that resume after a dropped connection. Smaller chunks suit slow connections (each chunk must get through before a proxy timeout, see below); keep this at or below your reverse proxy's request body limit (nginx: `client_max_body_size`, default 1 MiB) |
| `UPLOAD_MAX_PENDING` | `64` | Chunked uploads started but not yet finished (each holds up to `UPLOAD_MAX_FILE_SIZE` on local temp disk). Further starts get `503` and the upload page retries them. Uploads idle for 2 hours are discarded |
| `RATE_UPLOAD_PER_IP` / `RATE_UPLOAD_PER_LINK` | `100` / `300` | Requests per minute on the anonymous `/upload/...` routes, per client IP (IPv6: per /64) and per upload link. Chunk requests of an already started upload are not counted. `0` disables the limit |
| `RATE_AUTH_PER_IP` | `10` | Requests per minute per IP on `/auth/login` and `/auth/callback` |
| `RATE_NICKNAME_PER_IP` | `5` | Nickname changes per minute per IP |
| `UPLOAD_LINK_DURATION` | `168h` | Default validity of new upload links |
| `THUMBNAIL_SIZE` / `PREVIEW_SIZE` | `400` / `1600` | Longest edge in pixels |
| `WORKER_COUNT` | `2` | Background job workers |
| `EXPORT_DIR` / `EXPORT_TTL` | `/data/exports` / `24h` | Where ZIP exports are built and how long they are kept |
| `EXPORT_MAX_CONCURRENT` | `1` | ZIP exports built at the same time; others wait as "Preparing…" |
| `EXPORT_MAX_BYTES` | `21474836480` (20 GiB) | Exports whose originals add up to more are refused with a message. `0` disables the limit |

### OIDC

Create an OAuth2/OpenID provider + application with redirect URI `https://<domain>/auth/callback`
and scopes `openid profile email`. Every user who can sign in has full access (no roles).

Sign-ins whose ID token says `email_verified: false` are rejected (403, reason logged at warn level
without the email). **Behaviour change:** unverified emails used to be accepted. Set
`OIDC_REQUIRE_VERIFIED_EMAIL=false` if your provider reports unverified addresses for legitimate
users. Logging in also invalidates any session token sent with the callback request.

#### Rate limits

Limits are in-memory token buckets (burst = the per-minute value) and reset on restart. Requests over
the limit get `429` with `Retry-After`; the upload page waits and retries automatically, so guests
only see a "Server busy, retrying" note. Guests on one shared wifi share an IP: if a busy event hits
`RATE_UPLOAD_PER_IP` (each photo counts two requests: starting and finishing its upload), raise it. Behind a reverse proxy the limits only
work per client when `TRUSTED_PROXIES` is set (see below); otherwise every guest counts as the proxy.

#### Behind a reverse proxy

Set `TRUSTED_PROXIES` (e.g. `private`) so access logs show the real client instead of the proxy.
The header chain is read from the right and the first untrusted address wins, so a client cannot
forge its address. The startup log lists the active restrictions, and a warning is logged if
`BASE_URL` is `http://` on a non-loopback listener, or if `X-Forwarded-For` arrives while
`TRUSTED_PROXIES` is empty.

#### Flaky connections and proxy timeouts

The upload page sends each photo in chunks of `UPLOAD_CHUNK_SIZE`. When a connection drops, the
server keeps the bytes it received and the page continues after them, retrying with back-off (and
right away when the browser is back online). Each chunk is a short request, so a proxy read timeout
only matters if a single chunk takes longer than that: Traefik v3 entrypoints default to
`respondingTimeouts.readTimeout=60s`, which a 512 KiB chunk needs about 70 kbit/s to beat. Raise
the timeout (e.g. `--entryPoints.websecure.transport.respondingTimeouts.readTimeout=10m`) or lower
`UPLOAD_CHUNK_SIZE` for very slow connections.

A chunk request whose body could not be read in full is logged at info level as `upload chunk
incomplete` with the read error (`err`), `received_bytes`, `content_length`, `duration_ms` and
`remote`. The logs record what the server observed and do not guess a cause: for example, many
requests ending after exactly 60 s point at a proxy timeout, while varying durations point at the
client's connection. `upload chunk offset mismatch` logs the client's and the server's offset, and
`image uploaded` reports `chunks`, `incomplete_chunks` and `offset_mismatches` per file. Uploads
idle for 2 hours are logged as `idle upload discarded`. Upload IDs are logged shortened (`upload`), never in full.

#### Authentik specific

Restrict who may sign in with an Authentik policy/group binding on the application.

## Design notes

- Static files are embedded in the binary. Pages link them with a content hash
  (`/static/app.js?v=<hash>`), which browsers cache for a year: a release that changes a file
  changes its URL, so nobody keeps running an old script after an update. Unversioned requests
  (Leaflet, loaded by `map.js`) are revalidated via `ETag` on every use.
- Originals are stored untouched (bytes and EXIF). Thumbnails/previews are JPEG derivatives
  made by a background worker; HEIC is decoded in pure Go via WASM (no cgo).
- Camera clocks: the start page (`/`) shows the time as text and QR code. Photograph it with each
  camera and upload the photo like any other: the uploader's photos from that camera (same nickname,
  make and model) then get a corrected capture time, shown in the image details and written to the
  XMP sidecar on export. The originals are never touched. Accuracy is about one second; photos
  that lack EXIF make/model or capture time cannot be corrected.
- Duplicate detection: an exact re-upload (same bytes) is skipped silently; a near-duplicate
  (same picture resized/re-encoded) is flagged for review in the folder, where an admin can keep
  one (the other goes to the trash) and merge tags/rating onto it, or dismiss the pair.
- Trash: "Move to trash" on the selected photos (or in the photo detail) hides them everywhere:
  grid, counts, exports, duplicate review and the pHash backfill. The folder's **Trash** view
  lists them; from there a selection can be restored or **deleted permanently**. There is no
  automatic expiry and no "empty trash" button. **Trashed photos keep using storage until they are
  purged** and still count towards `UPLOAD_MAX_IMAGES_PER_FOLDER`. Purging deletes the database
  row and queues a retryable job that removes the original, preview and thumbnail. Re-uploading
  a photo that is in the trash is skipped like any duplicate; restore it from the trash instead.
- Orphan sweeper: at startup and then daily, a job lists `folders/` in the object store and
  deletes image objects without a matching image row (trashed photos have rows, so they are never
  touched). Only objects older than 24 hours are removed, so uploads in flight are safe. The
  sweep aborts (job fails, nothing is deleted) if 20 % or more of the objects, and at least 100,
  would be removed, which usually means the database points at the wrong storage. Every deletion
  is logged. Image rows whose original is missing are logged as a warning. Backends that cannot
  report object age are skipped with a warning. The sweeper cannot be switched off.
- Filtering: the folder page has a filter bar (tags with all/any, minimum rating, uploader,
  capture date range on the clock-corrected time, "with location", sort). The filter lives in the
  query string (`/folders/<id>?tag=forrest&rating_min=3`), so it can be bookmarked; paging is a
  keyset on the sort key, so ties never skip or repeat photos. With a filter active, **Download all
  matching** and **Move all matching to trash** act on every match (the server re-evaluates the
  filter; the trash action is refused if the count changed since you saw it). Not supported: tag
  exclusion, camera filter.
- Locations: the EXIF GPS position of a photo is stored at upload (older photos are read once at
  startup by a background job) and shown to signed-in users only: coordinates and a map in the photo
  detail (coordinates and an openstreetmap.org link), and a map card at the top of the folder page
  that opens by itself when a photo matching the current filter has a location. Originals are never modified
  and thumbnails/previews contain no EXIF. The XMP sidecar in exports carries the position. Maps use
  Leaflet (vendored in `web/static/leaflet`, no CDN) with standard OpenStreetMap tiles.
  **Showing the map card makes the viewer's browser request tiles from `tile.openstreetmap.org`,
  which sees the viewer's IP address** (and the site's origin as referrer); the content security
  policy allows that host for images on the folder page only. Tile use must follow the
  [OSM tile policy](https://operations.osmfoundation.org/policies/tiles/) (no bulk use).
  Guests are not told on the upload page yet (translation follows in stage 6).
- Ratings and tags live only in SQLite; ZIP exports contain the originals plus `.xmp` sidecars.
- Storage keys are generated IDs (`folders/<folder>/images/<image>/original|preview|thumbnail`).
- Deleting a folder hides it immediately and queues a retryable job that removes all objects,
  then the database rows.
- Jobs are persisted in SQLite and resume after a restart (orphaned running jobs are requeued).
- Upload link tokens are stored in the database (so members can copy the link again) and are
  never logged; request logs use route patterns instead of raw paths.
- Languages: English and German (informal "du"). The UI language is chosen by `?lang=`
  (remembered in a `lang` cookie for a year), then the cookie, then `Accept-Language`, then
  English. Catalogs are TOML files in `web/locales/` (`active.<lang>.toml`, go-i18n message
  format with `one`/`other` plural forms); adding a language means adding one file and a name in
  `internal/http/lang.go`. Templates call `{{t "message.id"}}` (`{{th ...}}` for messages with
  markup); templates are parsed once per language, so htmx fragments are translated as well.
  The upload and clock scripts read their texts from a JSON block in the page. Validation errors
  carry a message ID (`domain.UserError`) and are translated where the request language is known.
  Dates and file sizes follow the language (`30.09.2026 14:05`, `1,5 KiB` in German). Missing
  messages fall back to English, then to the ID. Log messages and tag names are not translated.
  `go test ./internal/i18n` checks that both catalogs have the same IDs, placeholders and plural
  forms, and that every ID used in templates, scripts and Go code exists and is used.

## Development

```sh
make test        # vet + race tests
make generate    # regenerate sqlc code after editing migrations/ or internal/database/queries/
```

Requires Go 1.27. SQL is written in `internal/database/queries/*.sql` and compiled by
[sqlc](https://sqlc.dev) (installed as a Go tool: `go tool sqlc`). Schema changes go into a new
[goose](https://github.com/pressly/goose) migration in `migrations/`.

## Backup and restore

**SQLite database** – never copy `photos.db` while the service is running (WAL mode). Use:

```sh
docker compose exec photos /photos backup /data/backup/photos-$(date +%F).db
```

This runs `VACUUM INTO`, which produces a consistent snapshot while the service keeps running.
Copy the snapshot off the host (restic, rclone, …).

**Filesystem storage** – back up the `photos/` directory (e.g. `restic backup /data/photos`).
Originals are immutable and never overwritten, so incremental backups are cheap. Exports
(`/data/exports`) are temporary and need no backup.

**WebDAV storage** – use the provider's snapshots or `rclone sync` the remote directory.

**S3 storage** – enable bucket versioning and/or replication in your S3/MinIO deployment.

**Restore** – stop the container, put the snapshot at `DATABASE_PATH` (delete any leftover
`photos.db-wal` / `photos.db-shm`), restore the `photos/` directory, and start the container.
Derivatives missing from a restored storage can be regenerated from originals.

## Health

`/healthz` (liveness) and `/readyz` (database and storage reachable). The image's
`photos healthcheck` command probes `/readyz`.
