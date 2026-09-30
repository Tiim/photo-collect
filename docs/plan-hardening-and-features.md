# Plan: hardening and new features

Follows the codebase review. Twelve work items, grouped into six stages. Each stage ends in a
releasable state: `make test` green, README updated, one or more PRs. Stages are ordered so that
risk and dependencies come first; items inside a stage can be split into separate PRs.

Item numbering used below:

| # | Item | Stage |
|---|---|---|
| 1 | Cap image pixel dimensions (decompression bomb) | 1 |
| 2 | Upload rate limits, concurrency and export limits | 3 |
| 3 | OIDC access control (`email_verified` check) | 2 |
| 4 | Storage orphan sweeper, durable duplicate cleanup | 4 |
| 5 | Backfill loop and derivative ETag fixes | 1 |
| 6 | CI test workflow | 1 |
| 7 | "Delete selected photos" (soft delete with trash) | 4 |
| 8 | Filtering (tags, rating, uploader, …) | 5 |
| 9 | Trusted proxy / `X-Forwarded-*` config | 2 |
| 10 | GPS position on OpenStreetMap | 5 |
| 11 | i18n (English, German) | 6 |

(Item 12 is the cross-cutting test and docs work listed under "Definition of done".)

## Decisions (confirmed)

- **Execution order**: 1 -> 6.1 -> 2 -> 3 -> 4 -> 5 -> 6.2/6.3 (i18n infrastructure early, German
  translation last).
- **CI**: runs on `push` only (not `pull_request`).
- **Pixel cap**: default 60 MP.
- **Rate limits**: keep the defaults in 3.1 (100/min per IP, 300/min per link).
- **Quotas**: dropped (no per-folder/per-nickname byte caps, no min-free-disk check).
- **OIDC**: only the verified-email check; no email/domain/group allowlists.
- **Deletion**: soft delete with a trash. No automatic expiry; purge is manual or happens when the
  folder is deleted.
- **Orphan sweeper**: no dry-run mode; live from the first run, protected by the 24 h grace period.
- **Leaflet**: vendored under `web/static`, no CDN.
- **i18n**: `go-i18n/v2` with TOML catalogs.
- **Trash**: no "Empty trash" button; purge is per selection.

---

## Stage 1: quick wins and safety net

Goal: close the cheapest, most exploitable problems and get CI running so later stages are checked
automatically.

### 1.1 CI test workflow (item 6) — DONE
- Add `.github/workflows/test.yml`: on `push` only (decision: not on `pull_request`), set up Go from `go.mod`, run
  `make test` and `make check-generated`.
- Keep `docker.yml` as is; optionally make the image build depend on the test job.
- Acceptance: a push with a failing test, failed `go vet` or stale sqlc output goes red (the Makefile has no
  lint or govulncheck target yet; add them to `make test` if wanted).
  Known limitation: pull requests (including from forks) are not checked automatically.

### 1.2 Pixel dimension cap (item 1) — DONE
- `internal/config`: new `UPLOAD_MAX_PIXELS` (default 60_000_000, i.e. 60 MP; must be > 0; document in the README that peak decode memory is
  about `WORKER_COUNT x 4 bytes x pixels`, i.e. ~240 MB per worker at the default).
- `internal/images/formats.go`: `Inspect` returns a new `ErrTooManyPixels` when
  `width*height` (computed in `int64`) exceeds the limit or a dimension is <= 0. Pass the limit in
  (`Inspect(r, maxPixels)`) rather than using a package global. The check runs on every format
  (JPEG, PNG, WebP, HEIC) before any full decode.
- `internal/http/upload.go`: map `ErrTooManyPixels` to a user message ("Image resolution is too
  large").
- Tests: crafted JPEG/PNG headers declaring 60000x60000 in a few hundred bytes are rejected by
  `Inspect` and never reach `Decode`; a normal image passes; boundary value.

### 1.3 Backfill loop and derivative ETag (item 5) — DONE
- **Backfill**: `backfillPHashes` re-enqueues every image with `phash IS NULL` on each start, so
  permanently undecodable images loop forever.
  - Migration `0004`: `images.phash_attempted_at TEXT NULL`.
    Set it in the derive handler on success and on permanent failure.
  - `ListImagesMissingPHash` filters on it. Add a job error type `jobs.Permanent(err)` that skips the
    retry schedule and marks the job dead.
  - Also skip enqueueing when an unfinished `derive_image` job for that image already exists.
- **ETag**: add `images.derivative_version INTEGER NOT NULL DEFAULT 1`, incremented whenever the
  derive handler rewrites preview/thumbnail. ETag becomes `"<sha16>-<kind>-<version>"` for
  derivatives. Originals keep the current ETag (immutable). Also honour a comma-separated or weak
  `If-None-Match` (parse the list instead of `==`).
- Tests: backfill enqueues once, then never again for a failing image; ETag changes after
  re-derivation.

**Stage 1 exit** (all steps done): CI green on push, oversize images rejected, no restart-time job storm.

---

## Stage 2: access control and reverse-proxy awareness

Goal: make "who may sign in" and "who is the client" explicit configuration. These two share code
(client IP, cookie security), so they go together.

### 2.1 Trusted proxy config (item 9) — DONE
- Config:
  - `TRUSTED_PROXIES`: comma-separated CIDRs/IPs (empty = trust nothing, current behaviour).
    Accept `private` as shorthand for RFC 1918 + loopback + ULA.
  - `TRUST_FORWARDED_PROTO` is not needed separately: when the direct peer is trusted, honour
    `X-Forwarded-For` (right-most untrusted entry), `X-Forwarded-Proto`, and `X-Forwarded-Host`
    only for logging and IP derivation. `BASE_URL` remains the source of truth for links, cookies
    and the origin check.
- New package `internal/clientip` (or in `internal/http`): `func (s *Server) clientIP(r) netip.Addr`.
  Never trust the headers unless `r.RemoteAddr` is inside `TRUSTED_PROXIES`.
- Middleware: `realIP` stores the resolved IP in the request context. Use it in `accessLog`, the OIDC
  failure log (`remote`) and the rate limiter (2.4 below / stage 3).
- Startup warning when `BASE_URL` is `http://` and the listener is not loopback, and when
  `BASE_URL` is https but `TRUSTED_PROXIES` is empty and requests arrive with `X-Forwarded-For`
  (log once, do not fail).
- Tests: spoofed `X-Forwarded-For` from an untrusted peer is ignored; chain parsing with multiple
  hops; IPv6; malformed headers.

### 2.2 OIDC access control (item 3) — DONE
- Config:
  - `OIDC_REQUIRE_VERIFIED_EMAIL` (default `true`): reject when the `email_verified` claim is
    present and false. A missing claim is accepted. No email/domain/group allowlists (decision).
- `internal/oidc/oidc.go`: extend the claims struct (`EmailVerified *bool`) and add
  `authorize(claims) error` evaluated before `UpsertUser`. Denials return 403 with a neutral
  message and log `reason` (not the email) at warn level.
- Session hygiene while here: on login, delete any existing session token presented with the
  callback request (rotation); on logout, keep current behaviour. Add `photos` startup log listing
  which restrictions are active so a misconfiguration is visible.
- Tests: table test for `authorize` (verified, unverified, claim missing, option off); callback integration test with a fake OIDC provider if one exists in
  `server_test.go`, otherwise unit-level.
- Docs: README OIDC section describes the verified-email check and states the default behaviour
  change (unverified emails are now rejected).

**Stage 2 exit** (all steps done): sign-in is restricted by configuration, and client IPs are correct behind a
proxy.

---

## Stage 3: abuse protection for the anonymous upload (item 2)

Goal: a leaked link cannot exhaust CPU, disk or storage. Depends on 2.1 for client IPs.

### 3.1 Rate limiting — DONE
- `golang.org/x/time/rate`: in-memory token bucket keyed by string, with periodic eviction of idle
  keys.
- Config with sane defaults:
  - `RATE_UPLOAD_PER_IP` (default 100 requests/minute) and `RATE_UPLOAD_PER_LINK`
    (default 300/minute). Make sure the frontend respects this. Defaults are kept as decided; guests on
    one NAT'd wifi share an IP, so 429s are possible at busy events and the retry/back-off in
    `app.js` is the mitigation (needs a test).
  - `RATE_AUTH_PER_IP` (default 10/minute) for `/auth/login` and `/auth/callback`.
  - `RATE_NICKNAME_PER_IP` (default 5/minute).
- Middleware wrappers applied per route in `Handler()`; respond `429` with `Retry-After`. The
  upload JS (`web/static/app.js`) must treat 429 as retryable with back-off, not as a failure.
- Log limiter hits at info level with the route label, never the token.
- Also add a global concurrent-ingest semaphore (`UPLOAD_MAX_CONCURRENT`, default
  `2*WORKER_COUNT`) so many parallel uploads don't fill the temp directory. Excess requests get 503
  with `Retry-After`.

### 3.2 Export limits (belongs with disk protection) — DONE
- enforce 1 concurrent export , enforced in the export job handler via a semaphore and
  20 GiB max export size, refuse to build larger exports with a clear error.

**Stage 3 exit** (all steps done): a leaked link cannot exhaust CPU or disk. No quotas (dropped).

---

## Stage 4: data integrity and deletion (items 4 and 7)

### 4.1 Soft delete (trash) and durable purge — DONE
- Migration `0005`: `images.deleted_at TEXT NULL`, `images.deleted_by TEXT NULL` (user id), index on
  `images(folder_id, deleted_at)`.
- `TrashImages(ctx, folderID, ids, userID)` sets `deleted_at`; `RestoreImages` clears it. Shared
  logic lives in `internal/library` (handlers stay thin).
- Trashed images (`deleted_at IS NOT NULL`) are excluded from: grid and counts, filters (5.1),
  `map.json` (5.2), exports (including "download all matching"), duplicate detection and the duplicate
  review panel, tag suggestions/counts, the pHash backfill, and the image/file routes (404 except in
  the Trash view). Every sqlc query that touches `images` is reviewed; one test per query group.
- Duplicate pairs stay in the table; they are hidden while either image is trashed and reappear on
  restore. `duplicateResolve` moves the loser to the trash instead of deleting it.
- Purge: `PurgeImages(ctx, folderID, ids)` only accepts trashed images. In one transaction it
  hard-deletes the row (cascades tags, duplicates, export links) **and** enqueues the job
  `delete_image_objects` (payload: `folder_id`, `image_id`), which removes original, preview and
  thumbnail and is retryable.
- No automatic expiry (decision). Purge happens manually ("Delete permanently" on a selection) or
  when the folder is deleted; verify that `delete_folder` also removes trashed images and objects.
- README: trashed photos keep using storage until purged.

### 4.2 Orphan sweeper (item 4) — DONE
- New job `sweep_orphans`, scheduled at startup and then daily (reuse the ticker pattern of session
  purge). Uses `Store.List("folders/")`, and for each key parses `folders/<f>/images/<i>/<kind>`:
  - key whose image row does not exist (trashed rows **do** exist, so their objects are never
    candidates) -> candidate;
  - key whose folder is soft-deleted -> left to `delete_folder`.
- Runs live from the first start, no dry-run mode (decision). The only protection is a grace period
  (24h): candidates younger than that are never removed, so in-flight uploads (object stored,
  transaction not committed yet) are safe. That needs object age: filesystem has mtime, S3 has
  `LastModified`, WebDAV has `getlastmodified`. Extend `storage.Store.List` to return
  `[]ObjectInfo{Key, Modified, Size}` (update the contract tests in `storagetest/contract.go` and all
  three backends). Backends that cannot report a time are skipped with a warning.
- Log every deletion (key) and a summary per run. Proposed guard: abort with an error when candidates
  exceed 20% of listed objects and at least 100 (protects against a database pointed at the wrong
  bucket).
- Also report the reverse (image rows whose original is missing) as a warning, no auto-repair possible.
- Tests: contract test for `List` metadata; sweeper unit test with a fake store; grace period;
  trashed image objects are kept; guard triggers.

### 4.3 Trash UI and "delete selected photos" (item 7) — DONE (checked tiles only; "all matching" follows in stage 5)
- Grid: the folder grid already supports selection for exports (`input.sel`, "select all"). Add a
  "Move to trash" button next to "Download selected". Confirm dialog stating the count (`<dialog>`
  or `hx-confirm`; strings via i18n). Submits via htmx to `POST /folders/{id}/images/trash` with
  `image=<id>` repeated and replaces the grid and count fragment.
- Handler `imagesTrash`: validates that all IDs belong to the folder (`ListImagesByIDs` already
  scopes by folder), caps the number per request (e.g. 500), calls `TrashImages`, logs user and
  count, responds with the refreshed grid. Refuse when the selection is empty.
- Trash view `GET /folders/{id}/trash`: grid of trashed images with "Restore" and "Delete
  permanently" (confirm dialog, calls `PurgeImages`). Link with the trashed count in the folder
  header. No "Empty trash" button (decision); purging is per selection only.
- "Select across pages": the grid is paginated (60). After stage 5, add "all N photos in this
  filter" (posts the filter query, the server re-evaluates it and confirms the count). First
  version handles only checked tiles.
- Image detail modal: "Move to trash" action using the same service.
- Ownership: all signed-in users are equal today (README). Log trash/restore/purge with the user
  ID; `deleted_by` records who trashed. An audit table is optional (see "Later").
- Tests: trash hides images everywhere listed in 4.1; restore brings them back; IDs from another
  folder ignored; purge deletes rows and enqueues object cleanup; purge refuses non-trashed images;
  exports that referenced the image still build; CSRF required.

**Stage 4 exit** (all steps done): no path leaves orphaned objects, users can bulk-trash and restore photos, and
purging is durable.

---

## Stage 5: filtering and GPS map (items 8 and 10)

### 5.1 Filtering (item 8) — DONE
(Deviations: tags are entered as one comma-separated field; sort direction uses a computed `is_desc` column because sqlc cannot parameterise `ORDER BY`; the `EXPLAIN` test checks the index-backed sub-queries rather than the whole statement. Migration `0006` also holds the GPS columns.)
Deliberately small filter model (query string, so it is bookmarkable and works with the paginated
grid):

```
GET /folders/{id}?tag=forrest&tag=summer&tag_mode=all&rating_min=3&uploader=anna&from=2026-06-01&to=2026-06-02&sort=captured
```

- Supported filters:
  - `tag` (repeatable) with `tag_mode=all` (AND, default) or `tag_mode=any` (OR). No tag exclusion.
  - `rating_min` (1-5) and `rating_max` (1-5).
  - `uploader` (nickname).
  - `from` / `to`: capture-time range on the *corrected* time (falls back to the camera time).
  - `has_gps=1` (added together with 5.2).
  - `sort=uploaded|captured|rating` with `dir=asc|desc`.
- Explicitly **not** supported: tag exclusion, camera/device filter, duplicate filter (duplicates keep
  their own review panel).
- Package `internal/filter`: `type Filter struct{…}`, `Parse(url.Values) (Filter, error)` with
  validation and limits (max 10 tags, tags normalised via `domain.NormalizeTag`), and
  `Encode() url.Values` for links and pagination. It also maps a `Filter` to the sqlc params.
- **Query: sqlc only, no hand-built SQL.** One `ListImagesFiltered` query where every filter is an
  optional argument (`sqlc.narg`) guarded by `IS NULL OR`, the usual sqlc pattern:

  ```sql
  -- name: ListImagesFiltered :many
  SELECT i.* FROM images i
  WHERE i.folder_id = sqlc.arg(folder_id)
    AND i.deleted_at IS NULL
    AND (sqlc.narg(rating_min) IS NULL OR i.rating >= sqlc.narg(rating_min))
    AND (sqlc.narg(rating_max) IS NULL OR i.rating <= sqlc.narg(rating_max))
    AND (sqlc.narg(uploader)   IS NULL OR i.uploader_nickname = sqlc.narg(uploader))
    AND (sqlc.narg(from_time)  IS NULL OR <corrected time> >= sqlc.narg(from_time))
    AND (sqlc.narg(to_time)    IS NULL OR <corrected time> <= sqlc.narg(to_time))
    AND (sqlc.narg(has_gps)    IS NULL OR i.gps_lat IS NOT NULL)
    AND (sqlc.narg(tags)       IS NULL OR (
          SELECT COUNT(DISTINCT t.id) FROM image_tags it JOIN tags t ON t.id = it.tag_id
          WHERE it.image_id = i.id AND t.name IN (SELECT value FROM json_each(sqlc.narg(tags)))
        ) >= sqlc.arg(tags_needed))   -- AND: tags_needed = len(tags); OR: tags_needed = 1
  ORDER BY ...
  LIMIT ...;
  ```

  - The tag list is passed as one JSON array string (`json_each`), so the number of tags does not
    change the statement. `tags_needed` implements both modes with one query: number of tags for
    AND, `1` for OR. A separate count query (`CountImagesFiltered`) reuses the same predicate.
  - Sorting: sqlc cannot parameterise `ORDER BY` directly, so use one query per sort key (three
    small queries sharing the predicate, generated from the same text) or a `CASE` on a `sort_key`
    argument. Pick the `CASE` variant unless the query plan shows it defeats the index.
  - Corrected capture time is an expression over `exif_time` and `time_offset_seconds`
    (`datetime(exif_time, printf('%+d seconds', COALESCE(time_offset_seconds, 0)))`). If sorting by
    it is slow, add a generated column in the migration.
- Pagination: keyset on the chosen sort key plus `seq` as tie-breaker (extra optional args
  `after_key`, `after_seq`); replaces the current `before=<seq>` for filtered views. The unfiltered
  default sort keeps working and can be served by the same query.
- UI:
  - A filter bar above the grid: tag input with autocomplete (reuse `/tags/suggest`) and an
    "all / any" toggle, minimum-rating selector, uploader select, date range, sort select, "clear".
  - htmx: form `hx-get` swaps the grid and updates the URL (`hx-push-url`). Show the match count
    ("37 of 412").
  - The example from the request ("tag #forrest and rating >= 3") is `?tag=forrest&rating_min=3`;
    add it as a test case verbatim.
- Integrations: "Download selected" gains "Download all matching" (export created from the filter,
  IDs resolved server-side when the request is made); "Delete all matching" as described in 4.3.
- Indexes: verify `image_tags(tag_id, image_id)` and `images(folder_id, rating)`; add via migration
  `0006` if the query plan needs them (`EXPLAIN QUERY PLAN` in a test on a seeded DB of 5000 images).
- Tests: parser (valid/invalid/limits); query semantics table test (tag AND, tag OR, rating range,
  uploader, date range, no filter, combined); pagination stability with ties; `make check-generated`
  stays green.

### 5.2 GPS on OpenStreetMap with Leaflet (item 10) — DONE
(Deviations: `map.js` loads Leaflet on demand; the folder page itself allows the tile host in `img-src` when maps are on, because the image modal is loaded into it; the upload-page privacy sentence is left for stage 6 when the i18n strings are converted.)
- **Extraction**: extend `images.Meta` with `Lat, Lon float64` and `HasGPS bool`, filled from the EXIF
  GPS IFD in `ReadMeta` (`imagemeta` exposes `GPS`; verify the API and add a fallback for HEIC).
  Validate ranges (`|lat| <= 90`, `|lon| <= 180`), treat exact (0,0) as missing. Altitude is out of
  scope.
- **Storage**: migration `0006`: `images.gps_lat REAL NULL`, `images.gps_lon REAL NULL` (both null or
  both set, `CHECK`). Populate at upload in `Ingest` and add a backfill job `extract_gps` for existing
  images (uses the same "attempted" marker approach as 1.3 so it runs once).
- **Display** (Leaflet, standard OpenStreetMap tiles; no self-hosted tile server):
  - Vendor Leaflet (and the marker-cluster plugin for the folder map) under `web/static/leaflet/`
    (JS, CSS, marker and layer images), pinned to an exact version recorded in a
    `web/static/leaflet/VERSION` note with the upstream licence file. No CDN. A small local
    `web/static/map.js` initialises maps from `data-*` attributes (CSP forbids inline scripts) and sets
    Leaflet's icon path to the local images.
  - The content security policy stays `'self'` for scripts and styles. `img-src` additionally allows
    `tile.openstreetmap.org` **only on pages that show a map** (image detail, folder map tab), as a
    per-response override in the handlers that render map templates.
  - If the tiles are unreachable the map still renders; the container also shows the coordinates and
    the openstreetmap.org link (rendered server-side).
  - Tiles come from `https://tile.openstreetmap.org/{z}/{x}/{y}.png`. Keep the OSM attribution
    control enabled. Follow the OSM tile usage policy (no prefetching or bulk use; the map only
    loads when opened).
  - Image detail: if the image has GPS, show the coordinates and a Leaflet map with one marker,
    plus a link to the same spot on openstreetmap.org.
  - Folder view: "Map" tab with all geotagged photos of the current filter as markers (Leaflet
    vendored marker clustering plugin) linking to the detail view. Ship after the single-image
    map. It fetches `GET /folders/{id}/map.json` (filter query string, returns id, lat, lon only).
  - Filter integration: `has_gps=1`.
- **Privacy**: GPS is only ever shown to signed-in users. Originals stay untouched. The upload page says that a
  photo's embedded location is stored (i18n string). Test that derivatives contain no EXIF.
  Note in the README that opening a map sends the viewer's IP to the OSM tile servers.
- **Export**: write GPS latitude/longitude into the XMP sidecar (`BuildXMP`).
- Tests: EXIF fixtures with GPS (add to `imagetest`), range validation, `(0,0)` ignored, detail page
  renders the map container only when GPS exists `map.json` respects the
  filter, `img-src` allows the tile host only on map pages, and no third-party script or style host appears in any CSP.

**Stage 5 exit** (all steps done): photos can be filtered by tag/rating/uploader/date, and geotagged photos show on a
Leaflet map.


---

## Stage 6: localisation (item 11)

Do this last so that all new strings from stages 1–5 exist. Add English keys as each earlier stage
lands to avoid a large catch-up (see "Working agreement").

### 6.1 Infrastructure — DONE (base layout and upload page converted; the rest is 6.2)
- Use an existing i18n library instead of a custom one: **`github.com/nicksnyder/go-i18n/v2`**
  (with `golang.org/x/text/language` for tag matching, which is already in `go.mod`). It provides
  message bundles, CLDR plural rules and template arguments, and its `goi18n extract` / `merge`
  commands report untranslated and missing messages.
- Package `internal/i18n` is only a thin wrapper:
  - Catalogs as embedded TOML files `web/locales/active.en.toml` and `active.de.toml` (go-i18n's
    naming), message IDs like `folder.delete.confirm`, plural forms (`one`/`other`) and
    `{{.Count}}`-style template data.
  - `Bundle` loads the embedded files, and `Localizer(langs ...string)` returns a
    `func(id string, data ...any) string` for a request. A missing message falls back to English,
    then to the ID, and is logged once at debug level.
- Language resolution order (middleware `lang`, stored in request context):
  1. `?lang=` (sets the cookie),
  2. `lang` cookie (unsigned, not sensitive; `SameSite=Lax`, 1 year),
  3. `Accept-Language` matched against supported tags,
  4. Default `en`.
  Guests on the upload page and signed-in users use the same mechanism; optionally persist the
  choice on the user row for signed-in users (migration, later).
- Templates: register `t` in the FuncMap per request. Because templates are parsed once
  (`loadTemplates`), pass the translator through the view data (`.T`) or clone templates per
  request (cost is small but avoid it); simplest: `page()` and `fragment()` inject `T` and `Lang`
  into a wrapper struct, and templates call `{{call .T "key"}}`, exposed as `{{t . "key"}}`
  helper if needed. Also set `<html lang="{{.Lang}}">`.
- Language switcher in the base layout (footer or header), and on the upload page which is the
  guest-facing entry point.

### 6.2 Extraction and translation — DONE
(Deviations: templates are parsed once per language with `t`/`th` bound to that language's localizer, so htmx fragments need no translator in their data and templates use `{{t "id"}}` instead of `{{call $.T ...}}`. Script strings are exposed as one JSON block in the base layout (`jsKeys` in `templates.go`). Plural messages are inline TOML tables. The image detail shows coordinates and an openstreetmap.org link, not a map, matching the code and tests. Binary units (KiB) are kept. The `uploader/…` tag prefix is an identifier and is not translated.)
- Inventory strings from: `web/templates/**/*.html` (pages and partials), `web/static/app.js`
  (upload statuses; pass strings through `data-*` attributes or a `<script type="application/json"
  id="i18n">` block rendered by the server, not through inline scripts because of CSP), server
  error messages in `internal/http/*.go` (`http.Error` texts, upload result messages,
  `uploadErrorMessage`), domain validation errors (`CleanNickname`, `NormalizeTag`; return typed
  errors, translate at the edge), `formatOffset` and device status texts in `capture.go`, and
  the clock page.
- Errors: introduce `type UserError struct{ Key string; Args map[string]any }` for validation
  errors so the layer that knows the language renders the text. JSON upload results return
  message keys plus the translated text.
- Dates and numbers: `date` and `humanSize` helpers become locale-aware (German: `30.09.2026 14:05`,
  decimal comma). Decide on binary vs SI units and keep it consistent.
- German translation written by a person or reviewed by a native speaker; use the informal "du"
  consistently (guests at events) and note this in a comment at the top of `active.de.toml`.
- Not translated: log messages, tag names (user data), the `uploader/…` tag prefix (decide whether
  the prefix is an identifier, in which case it must stay stable across languages, which it
  should).

### 6.3 Quality gates — DONE
(`goi18n merge` is not run in CI: `internal/i18n` tests cover parity, placeholders, plural forms and key usage.)
- Test that every message ID in `active.en.toml` exists in `active.de.toml` and vice versa (also run
  `goi18n merge` in CI to detect drift), that template placeholders match, and
  that every `t "…"` key used in templates and Go code exists (extract with a small `go test` that
  scans templates by regex).
- Golden-ish HTML tests render the upload page in both languages.
- Adding a language later means adding one TOML file and one tag in the supported list.

**Stage 6 exit** (all steps done): the whole UI is available in English and German, switchable by the user.

---

## Cross-cutting

### Migration overview
| Migration | Content | Stage |
|---|---|---|
| 0004 | `images.phash_attempted_at`, `images.derivative_version`, job dead-state column if missing | 1 |
| 0005 | `images.deleted_at`, `images.deleted_by`, index `(folder_id, deleted_at)` | 4 |
| 0006 | `images.gps_lat/gps_lon`, filter indexes | 5 |
| 0007 | (optional) `users.language` | 6 |

All migrations are additive and backward compatible for startup, so a rollback to the previous image
still starts (unknown columns are ignored by the old queries). Caveat: an older image does not know
`deleted_at`, so trashed photos reappear after a rollback.

### New configuration (all optional, documented in the README table)
`UPLOAD_MAX_PIXELS`, `TRUSTED_PROXIES`, `OIDC_REQUIRE_VERIFIED_EMAIL`, `RATE_*`, `UPLOAD_MAX_CONCURRENT`,
`EXPORT_MAX_CONCURRENT`, `EXPORT_MAX_BYTES`. (The map has no configuration: it ships Leaflet in
`web/static` and uses the standard OSM tiles. The sweeper has no switch.)

Behaviour changes to call out in release notes: unverified OIDC emails are rejected by default,
new default rate limits, deleting photos now moves them to a trash, and the orphan sweeper deleting
unreferenced objects (after 24 h) from the first run.

### Working agreement
- One PR per numbered item where practical, each with tests and a README/spec update.
- From stage 2 on, new user-facing strings are added to `active.en.toml` as they are written (once the
  i18n infrastructure exists; before that, keep them in templates and list them in the PR
  description). To avoid the ordering problem, the 6.1 infrastructure is built right after stage 1 (decided order: **1 → 6.1 → 2 → 3 → 4 → 5 → 6.2/6.3**), so new strings from stages 2-5 go straight into the catalogs.
- Every stage updates `docs/spec.md` where requirements change and the README configuration table.
- Security-relevant changes (stages 1–3) get a negative test for each abuse case they close.

### Definition of done (per stage)
1. `make test` and `make check-generated` pass in CI.
2. New config documented, defaults safe, startup validation with clear errors.
3. Migrations tested against a copy of a populated database (upgrade from the previous release).
4. Manual smoke test through the real UI (`/run`): upload, browse, rate, tag, export, delete.
5. Release notes list behaviour changes.

### Risks
- **Filter query performance** (5.1): the `IS NULL OR` pattern can defeat indexes; mitigated by the
  query-plan test on 5000 images and a hard cap on tag count.
- **Map tiles and privacy** (5.2): tile requests reveal the viewer's IP to the OSM tile servers and the
  OSM tile policy forbids heavy use; mitigated by loading maps only when opened, vendored scripts, `img-src` relaxed only on map pages and a README note.
- **i18n retrofit churn** (6): mitigated by building the infrastructure early and extracting
  strings per stage.
- **Storage interface change** (4.2, `List` metadata) touches three backends; the shared contract
  test keeps them aligned.
- **Rate limiter memory** (3.1): bounded by eviction and key-count cap.

### Later (explicitly out of scope here)
Roles and per-folder permissions, audit log table, resumable/chunked uploads, video and RAW
support, Prometheus metrics, and a job-queue admin page.
