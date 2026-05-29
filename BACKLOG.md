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

## Ideas under evaluation

- **`latetedemelon/gdrive-migration`** — review for patterns worth repurposing
  (resumable sync / manifest, OAuth & token-refresh handling, concurrency,
  rate-limit backoff, deduplication, integrity verification, folder-structure
  preservation). *Blocked: the repo is private and outside this tool's access —
  needs read access or a pasted README/file list before it can be mined.*
