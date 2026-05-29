gphotos-cdp
========

What?
--------

This program uses the Chrome DevTools Protocol to drive a Chrome session that
downloads your **original** photos and videos stored in Google Photos.

By default, it starts at the most ancient item in the library and progresses
towards the most recent. It can be run incrementally: it keeps track of the last
item that was downloaded (in `<dldir>/.lastdone`) and resumes from there on the
next run.

For each downloaded item, an external program can be run on it (with the `-run`
flag) right after it is downloaded, e.g. to upload it somewhere else. See the
`upload/perkeep` program, which uploads to a Perkeep server, for an example.


Why?
--------

We want to incrementally download our own photos out of Google Photos.

Google Photos used to have an API to do this (the Picasa Web Albums API) but
[they removed it](http://googlephotos.blogspot.com/2016/02/moving-on-from-picasa.html),
replacing it with a new API that doesn't let you download your
original photos. They instead let you download your photos
[with mangled EXIF, stripping location](https://developers.google.com/photos/library/guides/access-media-items#image-base-urls)
(and maybe recompressing the image bytes?).

There also used to be a way to sync your Google Photos to Google
Drive, and then you could use the Google Drive API to download your
original photos, but Google Photos
[removed that too](https://www.blog.google/products/photos/simplifying-google-photos-and-google-drive/).

We can get our original photos out with [Google Takeout](https://takeout.google.com/),
but only manually, and slowly. We don't want to have to remember to do
it (or remember to renew the time-limited scheduled takeouts) and we'd
like our photos mirrored in seconds or minutes, not weeks.

In [our original Perkeep
issue](https://github.com/perkeep/perkeep/issues/1144#issuecomment-525007239),
[@bradfitz](https://github.com/bradfitz/) said that we might have to give up on APIs and resort
to scraping, noting that the
[Chrome DevTools Protocol](https://github.com/ChromeDevTools/devtools-protocol) makes this
pretty easy. Brad hacked up some Go code to drive Chrome (using
https://github.com/chromedp/chromedp) and do a basic download and then
[Mathieu Lonjaret](https://github.com/mpl) made this tool, fleshing out the idea.


Requirements
--------

- Go 1.21 or newer (the tool uses the standard-library `log/slog` package).
- A local installation of Google Chrome or Chromium.


Build
--------

```sh
go build -o gphotos-cdp .
```

Or, if you use Nix, a flake is provided:

```sh
nix build
```


Quick start
--------

The first run opens a real Chrome window so you can log in to your Google
account. Use `-dev` so the browser profile is reused on subsequent runs and you
don't have to authenticate every time:

```sh
# First run: a Chrome window opens, log in, then the download starts.
./gphotos-cdp -dev

# Later runs resume where the previous run stopped.
./gphotos-cdp -dev
```

Download just the 20 oldest not-yet-downloaded items:

```sh
./gphotos-cdp -dev -n 20
```

Press `Ctrl+C` at any time to stop: the most recently completed item is always
recorded in `.lastdone`, so the next run picks up right after it.


Usage
--------

```
gphotos-cdp [flags]
```

| Flag | Default | Description |
| --- | --- | --- |
| `-n` | `-1` | Number of items to download. Negative means "all". |
| `-dry-run` | `false` | Walk the timeline and log what *would* be downloaded, without downloading anything or touching the download dir. |
| `-dev` | `false` | Reuse the same session dir so you don't have to authenticate every run. |
| `-dldir` | `$HOME/Downloads/gphotos-cdp` | Where to write downloads. |
| `-session-dir` | `$TMPDIR/gphotos-cdp` | Where to load/save the Chrome profile in `-dev` mode. |
| `-chrome-exec-path` | auto-detect | Path to the Chrome/Chromium binary. |
| `-chrome-flag` | – | Extra raw flag passed straight to Chrome; repeatable. Needed in containers (see below). |
| `-headless` | `false` | Run Chrome headless. Only valid with `-dev` (you must already be authenticated). |
| `-run` | – | Program to run on each downloaded file, right after it is downloaded. |
| `-from` | – | Only download items taken on or after this date (`YYYY-MM-DD`). Best-effort. |
| `-to` | – | Only download items taken on or before this date (`YYYY-MM-DD`). Best-effort. |
| `-organize` | `false` | Sort downloads into `YYYY/MM` sub-folders by photo date. Best-effort. |
| `-mtime` | `false` | Set each file's modification time to the photo date. Best-effort. |
| `-album` | – | Download an album instead of the main library (album id or full URL). Best-effort. |
| `-album-type` | `album` | Path segment used to build the album URL (e.g. `album`, `share`). |
| `-dl-timeout` | `1m` | How long a single download may stall before giving up. |
| `-json` | `false` | Emit logs as JSON. |
| `-log-level` | `info` | Log level: `debug`, `info`, `warn`, `error`. |
| `-v` | `false` | Verbose; shortcut for `-log-level=debug`. |
| `-start` | – | Skip all photos until this location is reached (debugging; `-dev` only). |


Examples
--------

Mirror everything, organising into year/month folders and stamping each file's
modification time with its capture date:

```sh
./gphotos-cdp -dev -organize -mtime
```

Only download items captured in 2023, as JSON logs (handy for piping into a log
processor or cron):

```sh
./gphotos-cdp -dev -from 2023-01-01 -to 2023-12-31 -json
```

Run headless from cron after a one-time interactive login, uploading each item
to Perkeep and then letting the upload script delete the local copy:

```sh
# One-time, interactive, to authenticate:
./gphotos-cdp -dev

# Then, unattended:
./gphotos-cdp -dev -headless -run ./upload/perkeep
```

Download an album by id (the value after `/album/` in the album's URL):

```sh
./gphotos-cdp -dev -album AF1QipMyAlbumId
```

Preview what a date-filtered run would do, without downloading anything (a safe
way to check the best-effort date detection works for your library before
committing to a long run):

```sh
./gphotos-cdp -dev -dry-run -from 2023-01-01 -to 2023-12-31 -n 50
```


Running headless in a container
--------

Chromium needs a couple of extra flags to launch inside a container (it won't
run as root without `--no-sandbox`, and crashes on the small default `/dev/shm`).
Pass them through with `-chrome-flag`, point `-chrome-exec-path` at the system
binary, and authenticate once (with the profile dir mounted) before going
unattended:

```sh
gphotos-cdp -json -dev -headless \
  -chrome-exec-path /usr/bin/chromium-browser \
  -chrome-flag --no-sandbox \
  -chrome-flag --disable-dev-shm-usage \
  -dldir /download -organize -mtime
```

On very new Chromium builds that have dropped the old headless mode, also add
`-chrome-flag --headless=new` (it overrides the default `-headless`). The
profile/auth lives in `-session-dir` (default `/tmp/gphotos-cdp`); mount that
path to persist the login across runs.


How it works
--------

1. It drives Chrome to `https://photos.google.com/` and waits for you to be
   authenticated (in `-dev` mode the profile is reused so this only happens once).
2. It jumps to the end of the timeline (the oldest item), or resumes from
   `.lastdone`, or starts at `-start`/`-album`.
3. It opens each item, triggers the native "download original" action
   (`Shift+D`), waits for the file(s) to finish downloading, moves them into
   place, optionally runs `-run` on them, and records progress in `.lastdone`.
4. It then navigates to the next (more recent) item and repeats, until `-n`
   items are downloaded or the most recent item is reached.

Live Photos / motion photos that download as more than one file are handled:
all resulting files are moved together and `-run` is invoked on each.

Every per-item log line carries the item's URL in a stable `location` field
(`location=https://photos.google.com/photo/…` in text logs, `"location"` in
JSON), so you can always recover the last processed item by grepping the logs.


Notes on the date-based features
--------

`-from`, `-to`, `-organize` and `-mtime` use the capture date Google shows in
the web UI (the tool opens the info side panel and reads it). This is the date
Google holds for the item, so it is available even for videos, HEIC, and
screenshots that often have **no EXIF `DateTimeOriginal`** — which is why doing
the foldering/timestamping here, in one pass, is more complete than EXIF-based
post-processing (no skipped files, no empty folders).

The flip side is that the web UI is not a stable API, so reading the date is
**best-effort**:

- If a date cannot be read for an item, the item is **still downloaded** (it is
  never silently skipped), and with `-organize` it lands in an `unknown/` folder.
- The date parser understands the common formats Google renders (e.g.
  `Mar 14, 2024, 12:08:27 PM`, including the narrow no-break space Google uses
  before AM/PM, and ISO `datetime` attributes).
- `-dry-run` is the safe way to confirm date detection works for your library
  before committing to a long run.

Folder layout:

- **Without `-organize`** each item keeps its own id-named sub-folder:
  `<dldir>/<itemId>/<file>` (the historical behaviour).
- **With `-organize`** items are grouped by date: `<dldir>/YYYY/MM/<file>`
  (unknown dates → `<dldir>/unknown/<file>`). If two items share an original
  filename in the same month, the later one gets its item id appended to keep
  them distinct.

`-organize`/`-mtime` are independent of resume: progress is tracked by item URL
in `.lastdone`, not by folder, so the layout can change between runs and `-run`
always receives the file's final path.


Limitations & future work
--------

- It downloads from the main library or a single `-album`. Archived items and
  cross-album de-duplication are not handled specially.
- Downloads are sequential (one item at a time). A `-workers` style concurrent
  mode would speed up large libraries but needs careful tab management.
- Progress tracking is a single `.lastdone` sentinel rather than a full index,
  so re-downloading a specific item means using `-start`.

Contributions are welcome.


What if Google Photos breaks this tool on purpose or by accident?
--------

I guess we'll have to continually update it.

But that's no different than using people's APIs, because companies all seem to
be deprecating and changing their APIs regularly too.
