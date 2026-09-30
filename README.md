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
| `SESSION_SECRET` | – (required, ≥32 chars) | Signs the nickname and login-state cookies |
| `SESSION_TTL` | `720h` | Session lifetime (sliding) |
| `UPLOAD_MAX_FILE_SIZE` | `52428800` | Bytes per file |
| `UPLOAD_MAX_FILES_PER_REQUEST` | `50` | |
| `UPLOAD_MAX_IMAGES_PER_FOLDER` | `5000` | |
| `UPLOAD_MAX_PIXELS` | `60000000` | Maximum pixels (width x height) per image. Peak decode memory is about `WORKER_COUNT` x 4 bytes x pixels, i.e. ~240 MB per worker at the default |
| `UPLOAD_LINK_DURATION` | `168h` | Default validity of new upload links |
| `THUMBNAIL_SIZE` / `PREVIEW_SIZE` | `400` / `1600` | Longest edge in pixels |
| `WORKER_COUNT` | `2` | Background job workers |
| `EXPORT_DIR` / `EXPORT_TTL` | `/data/exports` / `24h` | Where ZIP exports are built and how long they are kept |

### OIDC

Create an OAuth2/OpenID provider + application with redirect URI `https://<domain>/auth/callback`
and scopes `openid profile email`. Every user who can sign in has full access (no roles).

#### Authentik specific

Restrict who may sign in with an Authentik policy/group binding on the application.

## Design notes

- Originals are stored untouched (bytes and EXIF). Thumbnails/previews are JPEG derivatives
  made by a background worker; HEIC is decoded in pure Go via WASM (no cgo).
- Camera clocks: the start page (`/`) shows the time as text and QR code. Photograph it with each
  camera and upload the photo like any other: the uploader's photos from that camera (same nickname,
  make and model) then get a corrected capture time, shown in the image details and written to the
  XMP sidecar on export. The originals are never touched. Accuracy is about one second; photos
  that lack EXIF make/model or capture time cannot be corrected.
- Duplicate detection: an exact re-upload (same bytes) is skipped silently; a near-duplicate
  (same picture resized/re-encoded) is flagged for review in the folder, where an admin can keep
  one and merge tags/rating onto it, or dismiss the pair.
- Ratings and tags live only in SQLite; ZIP exports contain the originals plus `.xmp` sidecars.
- Storage keys are generated IDs (`folders/<folder>/images/<image>/original|preview|thumbnail`).
- Deleting a folder hides it immediately and queues a retryable job that removes all objects,
  then the database rows.
- Jobs are persisted in SQLite and resume after a restart (orphaned running jobs are requeued).
- Upload link tokens are stored in the database (so members can copy the link again) and are
  never logged; request logs use route patterns instead of raw paths.

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
