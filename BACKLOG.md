# Backlog & notes

Running list of follow-ups for `gphotos-cdp` (the engine) and its companion
`docker-gphotos-sync` (the container). Append new items; check off as done.

## Positioning: a living mirror (Takeout optional)

The thesis: **gphotos-cdp needs no Google Takeout** — it acquires originals
incrementally and reads their metadata (best-effort) straight from the browser.
But it can **use** a Takeout as a one-time, metadata-rich baseline, and then
**keep that export current going forward** as the incremental layer.

Every existing Takeout→app importer is a *one-time* batch. Our differentiator is
**continuity + a cross-source dedupe ledger** (the manifest), feeding an
**app-neutral normalized library** rather than locking to a single app. See
"Takeout interop" and "Metadata sourcing" below.

## Shipped (landed in master)

- **v0.2.0 engine**: bug fixes, `log/slog` (`-json`/`-log-level`), date filter
  (`-from`/`-to`), `-organize`/`-mtime`, `-album`, headless robustness,
  live-photo/multi-file, graceful shutdown, `-chrome-exec-path`, `-chrome-flag`,
  `-dry-run`.
- **Manifest store** (`.manifest.json`) + **Browser seam** → `navN` unit-tested
  against a fake browser. (#3)
- **Resume from the manifest** + **per-item retry & error isolation**
  (`-max-attempts`, `-fail-on-error`). (#4)
- **`status` / `verify` subcommands** (manifest summary + CSV; file existence/size
  check). (#5)

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

## Metadata sourcing (no Takeout required)

Where each field for a *scraped* (non-Takeout) photo comes from:

| Field | Source | Reliability |
| --- | --- | --- |
| Capture date/time | file EXIF; else info-panel scrape | high (file) / best-effort (panel) |
| GPS (camera-geotagged) | **file EXIF** — the owner's "download original" keeps it | high |
| GPS (manually added in Google) | info-panel scrape (place name; coords from the map element if exposed) | best-effort |
| Description / caption | info-panel scrape | best-effort |
| Album membership | **album-grid index pass** (read `./photo/<id>` anchors off each album page) | best-effort |
| Camera make/model, lens… | file EXIF | high |
| Favorite / archived | info-panel / UI scrape | best-effort |
| People / face tags | not exposed | unavailable |

Verified facts behind this:

- **Owner downloads keep EXIF, including GPS** — only *shared-link* downloads
  strip location. So camera-geotagged photos already carry GPS in the bytes we
  download; just read it with `exiftool`. (The original README's
  "EXIF stripped" caveat is about the *API*, not the owner download.)
- **Manually-added / estimated locations live server-side** (not in the file),
  but are **shown in the info panel** we already open for the date — so they're
  scrapeable (place name always; exact coords only if the map element exposes
  them, else geocode the name).

### Album index pass

Album membership is **not** on the single-photo page — but it *is* the album's
**grid page**: every thumbnail is an `<a href="./photo/<id>">`. So membership is
a cheap, separate pass (no downloads, no per-photo opens):

1. Scrape the Albums page for each album's id + title.
2. For each album, **scroll its grid and harvest member photo ids** from the
   anchors. The grid is **virtualized** (off-screen thumbnails drop from the
   DOM), so collect incrementally during the scroll and de-dup — same scroll
   mechanism as `navToEnd`.
3. Record `Albums []string` on each manifest item.

Fits the subcommand pattern: a `gphotos-cdp albums` index pass, decoupled from
the download walk. Re-run later to catch membership changes.

### Sidecar schema (Takeout-shaped)

Emit a per-item JSON sidecar (mirroring manifest fields) so the enhancer embeds
metadata uniformly regardless of source:

```json
{
  "id": "<google item id>",
  "url": "https://photos.google.com/photo/<id>",
  "photoTakenTime": "<RFC3339>",
  "geo": { "lat": 0.0, "lng": 0.0, "source": "exif|panel|none" },
  "description": "",
  "albums": ["Trip 2024"],
  "favorite": false,
  "originalFilename": "IMG_1234.jpg",
  "source": "cdp|takeout",
  "runId": "",
  "sha256": ""
}
```

Both paths — Google's Takeout JSON and our CDP scrape — normalize into this; the
enhancer writes EXIF/XMP (`DateTimeOriginal`, GPS) + mtime + album tags from it.

## Shared & partner albums (capture what Takeout can't)

**Why it's a differentiator.** Google Takeout does **not** include photos other
people shared with you (only your own library, plus partner-shared items you
saved); `immich-go` documents this as an unavoidable Takeout gap. The CDP path
drives the real Google Photos UI, which **can navigate shared content** — so we
can capture material no Takeout-based importer can reach. This is the strongest
single differentiator of the scraping approach.

**What's reachable in the UI**

- **Shared albums** — albums others shared with you, and ones you shared — under
  the *Sharing* tab (`photos.google.com/sharing`); each is an album-like grid.
- **Partner sharing** — a linked partner account's shared library.

**Two distinct uses**

1. **Membership only** — extend the `albums` index pass (above) to shared-album
   grids, so shared albums become tags in the manifest. Cheap; no downloads.
2. **Acquire shared-only items** — a `-include-shared` scope that *downloads*
   items present in shared albums but **not** in your own library, backing up
   what friends shared with you.

**Caveats (be honest)**

- Items shared *with* you that you never saved to your library behave like
  **shared-link** content: the download may be **reduced quality and
  GPS-stripped** (consistent with the owner-vs-shared EXIF finding above). Treat
  shared-only acquisitions as best-effort and stamp provenance (`source=shared`).
- **Dedup:** a shared item you also saved to your own library would appear twice;
  the manifest join (item id / content hash) collapses it.
- Shared/partner UI surfaces change; DOM-dependent, so best-effort like the rest
  of the scraping.

**Scope:** `-include-shared` (download) plus shared-album coverage in the
`albums` index pass (membership). Off by default — it changes what gets pulled.

## Takeout interop & the normalized-library pipeline

Target an **app-neutral normalized library** — embedded-EXIF files + a clean
folder scheme + the manifest as the index — with optional per-app adapters, not
a hard dependency on one app.

- **Baseline (optional):** ingest an existing Takeout once for its rich
  server-side metadata (GPS, album folders, descriptions). Reuse `immich-go` /
  GPTH or our own enhancer for the parse — don't re-implement it (see below).
- **Incremental (the core):** gphotos-cdp keeps the library current — new photos
  + best-effort metadata — reconciled against the baseline by the **manifest**
  (join key = Google item id, fallback content hash) so nothing is re-imported
  or duplicated, and Takeout's multi-album duplicates collapse to one file with
  N album tags.
- **Output:** EXIF-correct, deduped files + album tags that any photo app
  ingests (Immich, PhotoPrism, digiKam, Nextcloud Memories…) via folder import
  or a thin adapter.

### Differentiators vs existing tools

- vs **immich-go** (Immich-only, one-time, needs a running server): we're
  **app-neutral**, **incremental/ongoing**, and need **no server** to build the
  library.
- vs **GPTH / google-photos-migrate** (app-neutral but one-time, metadata-weak —
  GPS embedding regressed, album reconstruction is TODO): we're **incremental**
  and **metadata-stronger** (file EXIF + info-panel + album index).
- vs **all of them**: a **cross-source dedupe ledger** (Takeout baseline ⨉ live
  incremental, one manifest) and a standalone **`verify`** over the whole
  library — nobody else spans both sources.
- **Honest:** for a *one-time* Google→Immich move today, `immich-go` is better;
  don't re-implement its hard-won Takeout parsing (JSON pairing, truncated
  `.supplemental-metadata.json` names, edited/live splits, Google's breaking
  changes). **Interoperate**: baseline via immich-go, ongoing via us, reconcile
  via the manifest.

## Features to consider (mined from immich-go / GPTH / docker tools)

- **Shared / partner albums (differentiator).** Captures friend-shared photos
  Takeout can't — see the dedicated **Shared & partner albums** section above.
- **Favorites & archive status.** Scrape the star/archive state and map to the
  app's favorite/archive (server-side, UI-scrapeable).
- **Grouping / stacks.** Group RAW+JPEG, bursts, and live-photo (JPEG+MP4) pairs
  so the target app stacks them. (immich-go does RAW+JPEG and burst stacking.)
- **Edited-version policy.** Detect `*-edited.*` duplicates; choose
  original / edited / both. (A GPTH gap.)
- **XMP sidecar output mode.** Non-destructive alternative to embedding
  (`-sidecar xmp` vs `-embed`); immich-go and most apps read XMP sidecars.
- **Read Takeout zips directly.** For the enhancer path — no manual extraction
  (immich-go reads zips in place).
- **Tags / classification.** Map media type / keywords to the app's tags
  (gdrive-migration auto-classifies at scan time).
- **Per-app adapter (Immich API push).** Optional, behind the normalized-library
  contract, so direct-to-Immich users get it without coupling the core.
- **Content-hash dedupe** as the cross-source join key (gdrive `--dedupe`).
- **Scheduling.** The container already does cron; only add an engine
  `-loop <interval>` if non-container users want it.

## Engine ideas

- **Concurrency (`-workers`).** Parallel downloads across multiple tabs — the
  biggest speedup for large libraries. Needs careful tab/profile management;
  validate against rate-limiting. (Safer now that the manifest exists.)
- ✅ **Durable progress index.** Shipped as the JSON manifest (#3). *(Was: replace
  the `.lastdone` sentinel.)*
- ◑ **Integrity verification.** `verify` checks file existence + size (#5);
  recording a **content hash** per item (and re-download on mismatch) is still
  TODO.
- **Date-detection hardening.** More `aria-label` / `<time>` selectors; an
  optional EXIF fallback behind a build tag (keeps the default dependency-free);
  log raw samples when a date is `unknown` to drive parser fixes.
- **Scope coverage.** Validate `-album` against real albums; add archive /
  favorites / shared scopes (see "Features to consider").
- **Config via file/env.** So the container can configure without long arg
  lists.
- **Observability.** Periodic progress heartbeat log + optional healthcheck
  ping URL.
- **Upstream alignment.** Offer the broadly-useful fixes (live-photo handling,
  graceful shutdown, headless-auth debug dump, `-json`) to
  `perkeep/gphotos-cdp` to reduce fork divergence.

## Repurposed from gdrive-migration

`latetedemelon/gdrive-migration` is a manifest-driven, resumable Drive→Drive
tool. It talks to the Drive **API**, so its API-level mechanics don't transfer,
but its higher-level architecture maps almost 1:1 onto the gaps above. The
meta-lesson: it began as a ~120-line script and matured by moving state into a
**SQLite store**, which then unlocked a cluster of features. (We took the same
step with the JSON manifest in #3.)

### Adopt (strong fit)

- ✅ **State DB / manifest (replaces `.lastdone`).** Shipped (#3): JSON store of
  per-item id / URL / status / date / files / bytes / attempts / error.
- ✅ **Error isolation + per-item retry (`-max-attempts`).** Shipped (#4).
- ◑ **Integrity + a `verify` mode.** `verify` shipped (#5); content hashing TODO.
- **De-duplication (`-dedupe report|skip`).** Group downloaded files by content
  hash; report or collapse duplicates, recording a pointer so re-runs stay
  idempotent and nothing is silently dropped. (gdrive: `--dedupe report|skip`.)
  Also the cross-source join key for the Takeout pipeline above.
- ✅ **Status + CSV inventory.** Shipped (#5): `status` + `-csv`.
- ✅ **Testable orchestration via a fake.** Shipped (#3): the `Browser` seam +
  in-memory fake browser unit-test `navN`.

### Consider (medium fit)

- **Richer `-organize` schemes.** Beyond `YYYY/MM`: by year, by media type
  (Photos/Videos/Motion), or type/year — made pluggable like gdrive's
  `category | category-year | topic`. (Their filename/path keyword "topic" axis
  is a poor fit for `IMG_####` photo names; skip that one.)
- **Provenance / metadata sidecar.** Emit a per-item JSON sidecar (URL, id,
  scraped date, GPS, albums, original filename, run id) — see the **Sidecar
  schema** above; the same shape Google Takeout uses, handy for re-import and
  auditing.
- **Scan/download phase split + subcommands.** gdrive separates `scan` (walk
  into the DB) from `migrate`, plus `status`/`verify`. A `scan` that records the
  timeline (what `-dry-run` already walks) followed by a `download` pass over the
  manifest would make discovery itself resumable and allow status before any
  download. The `albums` index pass (above) is one such scan.
- **`DECISIONS.md`.** They keep a design-rationale log; worth adopting here.

### Not applicable (API/Docs-specific — noted for completeness)

- OAuth / service-account auth, domain-wide delegation — we authenticate via the
  browser session; there is no API.
- Drive-v3 pagination and resumable chunked up/download — the browser performs
  the download; we only watch the file land.
- Google-native export (Doc→`.docx`) and Drive file-property tagging — no analog
  for photo bytes.
- Keyword "topic" taxonomy — poor fit for `IMG_####`-style photo names.
