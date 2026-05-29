# Backlog & notes

Running list of follow-ups for `gphotos-cdp` (the engine) and its companion
`docker-gphotos-sync` (the container). Append new items; check off as done.

## Phase 2 validation — move date handling into the engine

**Context.** The container currently sets file timestamps with ExifTool in a
`-run` hook (`fix_time.sh`). Phase 2 replaces that with the engine's own
`-organize`/`-mtime` (which use the capture date Google shows, so they also
cover videos / HEIC / screenshots that frequently lack EXIF `DateTimeOriginal`)
and drops ExifTool + perl (~35 MB) from the image. It is **gated** on confirming
the engine's best-effort date detection actually resolves against a real
library.

**Checklist** (run against a real, authenticated library; `-dry-run` writes
nothing and never touches the download dir):

- [ ] Use the engine at the pinned release (≥ `v0.2.0`).
- [ ] Bounded dry-run with JSON logs:
      ```sh
      # container-style (headless):
      gphotos-cdp -dev -headless -dry-run -json -n 200 \
        -chrome-exec-path /usr/bin/chromium-browser \
        -chrome-flag --no-sandbox -chrome-flag --disable-dev-shm-usage
      # desktop-style (visible browser, after one interactive auth):
      gphotos-cdp -dev -dry-run -json -n 200
      ```
- [ ] Every line shows a real `photo_time=<RFC3339>` — **not** `photo_time=unknown`.
      Quantify: `... 2>&1 | grep -c 'photo_time=unknown'` vs total item count
      (target ~0%).
- [ ] Spot-check coverage across item types, since this is where EXIF
      post-processing silently fails:
  - [ ] JPEG / HEIC photos
  - [ ] **Videos (MP4/MOV)** — the main win over EXIF
  - [ ] Screenshots / messaging-app / edited images
  - [ ] Motion / Live Photos
- [ ] Dates are *correct* (the capture date, not today/import date) — verify a
      few known items against what Google Photos shows.
- [ ] Date-range filter behaves: `-from`/`-to` over a known window skips older
      items and stops once past `-to` (timeline is walked oldest → newest).
- [ ] Real `-organize -mtime` run to a scratch `-dldir` with a small `-n`:
      confirm `YYYY/MM/<file>` layout, correct file mtimes, nothing in `unknown/`.
- [ ] Headless download actually fires in the container build (a file lands in
      `-dldir`; `browser.SetDownloadBehavior` sticks).
- [ ] **Decision:** unknown-rate ~0% and dates correct → switch the container to
      `-organize -mtime`, drop the `-run`/`fix_time.sh`/ExifTool/perl. Otherwise
      → keep the ExifTool fallback and open an issue with the failing
      `aria-label` samples so the parser can be improved.

Safety: `-dry-run` is read-only; the real check uses a scratch dir; `.lastdone`
tracks progress by item URL, so changing the folder layout never breaks resume.

## Engine ideas

- **Concurrency (`-workers`).** Parallel downloads across multiple tabs — the
  biggest speedup for large libraries. Needs careful tab/profile management;
  validate against rate-limiting.
- **Durable progress index.** Replace the single `.lastdone` sentinel with a
  manifest (JSON or SQLite) of per-item id / date / files / checksum. Enables
  re-download by id, integrity checks, and resume that survives deletions.
  See **Repurposed from gdrive-migration** below for the expanded design — this
  is the keystone item that unlocks most of the others.
- **Integrity verification.** Record size (and a hash) per item; re-download on
  mismatch.
- **Date-detection hardening.** More `aria-label` / `<time>` selectors; an
  optional EXIF fallback behind a build tag (keeps the default dependency-free);
  log raw samples when a date is `unknown` to drive parser fixes.
- **Scope coverage.** Validate `-album` against real albums; add archive /
  favorites scopes.
- **Config via file/env.** So the container can configure without long arg
  lists.
- **Observability.** Periodic progress heartbeat log + optional healthcheck
  ping URL.
- **Upstream alignment.** Offer the broadly-useful fixes (live-photo handling,
  graceful shutdown, headless-auth debug dump, `-json`) to
  `perkeep/gphotos-cdp` to reduce fork divergence.

## Cross-repo (`docker-gphotos-sync`)

- **Phase 1 (in flight).** Pin engine `@v0.2.0`; add `-chrome-exec-path` +
  `-chrome-flag --no-sandbox` + `-chrome-flag --disable-dev-shm-usage`; fix the
  README `/config` ↔ `/tmp/gphotos-cdp` wording; rework the healthcheck so a
  graceful `SIGTERM` (exit 0) isn't reported as a failure.
- **Phase 2.** This checklist → engine-side dates, drop ExifTool/perl.
- **Release hygiene.** Keep tags moving so `@latest` never resolves back to the
  buggy `0.0.1`; the container should always pin an explicit version.

## Repurposed from gdrive-migration

`latetedemelon/gdrive-migration` is a manifest-driven, resumable Drive→Drive
tool. It talks to the Drive **API**, so its API-level mechanics don't transfer,
but its higher-level architecture maps almost 1:1 onto the gaps above. The
meta-lesson: it began as a ~120-line script and matured by moving state into a
**SQLite store**, which then unlocked a cluster of features. Our `.lastdone`
sentinel is at that "120-line script" stage; the same move unlocks most of the
items below.

### Adopt (strong fit)

- **State DB / manifest (replaces `.lastdone`).** Thread-safe SQLite (or JSON)
  store of per-item rows: photo id, URL, capture date, downloaded filenames,
  status (pending/done/errored/skipped), attempts, size/hash, batch/run id.
  The keystone — everything below depends on it. (gdrive: `state.py`.)
- **Error isolation + per-item retry (`-max-attempts`).** Today one failed item
  aborts the whole run. Instead: record the item as errored, continue, and retry
  up to N times across re-runs. Essential for multi-hour libraries. (gdrive:
  `--max-attempts`, errored items retried on re-run.)
- **Integrity + a `verify` mode.** Record each file's size/hash in the manifest;
  re-verify on demand and re-download mismatches. (gdrive: MD5-after-upload + a
  `verify` subcommand.)
- **De-duplication (`-dedupe report|skip`).** Group downloaded files by content
  hash; report or collapse duplicates, recording a pointer so re-runs stay
  idempotent and nothing is silently dropped. (gdrive: `--dedupe report|skip`.)
- **Status + CSV inventory.** A `status` view over the manifest (counts by
  year / media type / status) plus CSV export. Cheap once the manifest exists,
  and a natural body for the container's healthcheck ping. (gdrive: `status
  --csv`.)
- **Testable orchestration via a fake.** gdrive unit-tests its whole
  scan→migrate→verify flow against an in-memory **fake Drive** — no network, no
  creds. Our analog: hide the chromedp calls (navigate / read-date / download /
  move) behind a small interface so `navN`, the download loop and organize logic
  can run against a fake browser. Closes our biggest test gap — the
  browser-driving paths currently have no unit coverage. (gdrive:
  `tests/conftest.py`.)

### Consider (medium fit)

- **Richer `-organize` schemes.** Beyond `YYYY/MM`: by year, by media type
  (Photos/Videos/Motion), or type/year — made pluggable like gdrive's
  `category | category-year | topic`. (Their filename/path keyword "topic" axis
  is a poor fit for `IMG_####` photo names; skip that one.)
- **Provenance / metadata sidecar.** gdrive stamps `migrated_from` id + source
  hash + batch onto each file. We can't write Drive properties (we download
  locally), but we can emit a per-item JSON sidecar (URL, id, scraped date,
  original filename, run id) — the same shape Google Takeout uses, handy for
  re-import and auditing.
- **Scan/download phase split + subcommands.** gdrive separates `scan` (walk
  into the DB) from `migrate`, plus `status`/`verify`. A `scan` that records the
  timeline (what `-dry-run` already walks) followed by a `download` pass over the
  manifest would make discovery itself resumable and allow status before any
  download. Larger change; natural once the manifest lands.
- **`DECISIONS.md`.** They keep a design-rationale log; worth adopting here.

### Not applicable (API/Docs-specific — noted for completeness)

- OAuth / service-account auth, domain-wide delegation — we authenticate via the
  browser session; there is no API.
- Drive-v3 pagination and resumable chunked up/download — the browser performs
  the download; we only watch the file land.
- Google-native export (Doc→`.docx`) and Drive file-property tagging — no analog
  for photo bytes.
- Keyword "topic" taxonomy — poor fit for `IMG_####`-style photo names.
