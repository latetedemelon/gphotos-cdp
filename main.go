/*
Copyright 2019 The Perkeep Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

     http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// The gphotos-cdp program uses the Chrome DevTools Protocol to drive a Chrome session
// that downloads your photos stored in Google Photos.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

var (
	nItemsFlag    = flag.Int("n", -1, "number of items to download. If negative, get them all.")
	devFlag       = flag.Bool("dev", false, "dev mode. we reuse the same session dir (/tmp/gphotos-cdp), so we don't have to auth at every run.")
	dlDirFlag     = flag.String("dldir", "", "where to write the downloads. defaults to $HOME/Downloads/gphotos-cdp.")
	startFlag     = flag.String("start", "", "skip all photos until this location is reached. for debugging.")
	runFlag       = flag.String("run", "", "the program to run on each downloaded item, right after it is downloaded. It is also the responsibility of that program to remove the downloaded item, if desired.")
	verboseFlag   = flag.Bool("v", false, "be verbose (shortcut for -log-level=debug)")
	headlessFlag  = flag.Bool("headless", false, "Start chrome browser in headless mode (only in -dev mode; cannot do authentication this way).")
	sessDirFlag   = flag.String("session-dir", filepath.Join(os.TempDir(), "gphotos-cdp"), "where to load/save the chrome profile from in -dev mode")
	execPathFlag  = flag.String("chrome-exec-path", "", "path to the Chrome/Chromium binary to use (default: auto-detect)")
	jsonLogFlag   = flag.Bool("json", false, "output logs in JSON format")
	logLevelFlag  = flag.String("log-level", "info", "log level: debug, info, warn, error")
	fromFlag      = flag.String("from", "", "only download items taken on or after this date, YYYY-MM-DD (best-effort, see README)")
	toFlag        = flag.String("to", "", "only download items taken on or before this date, YYYY-MM-DD (best-effort, see README)")
	organizeFlag  = flag.Bool("organize", false, "sort downloads into YYYY/MM sub-folders by photo date (best-effort)")
	mtimeFlag     = flag.Bool("mtime", false, "set each downloaded file's modification time to the photo date (best-effort)")
	albumFlag     = flag.String("album", "", "download an album instead of the main library: an album id or a full URL (best-effort)")
	albumTypeFlag = flag.String("album-type", "album", "the path segment used to build the album URL, as seen in the browser (e.g. album, share)")
	dlTimeoutFlag = flag.Duration("dl-timeout", time.Minute, "how long a single download may stall (make no progress) before giving up")
)

// libraryURL is the entry point for the Google Photos main library.
const libraryURL = "https://photos.google.com/"

var tick = 500 * time.Millisecond

// logger is the process-wide structured logger, configured in setupLogger.
var logger = slog.New(slog.NewTextHandler(os.Stderr, nil))

// setupLogger configures the global logger from the -json, -log-level and -v flags.
func setupLogger() {
	level := parseLevel(*logLevelFlag)
	if *verboseFlag {
		level = slog.LevelDebug
	}
	opts := &slog.HandlerOptions{Level: level}
	var h slog.Handler
	if *jsonLogFlag {
		h = slog.NewJSONHandler(os.Stderr, opts)
	} else {
		h = slog.NewTextHandler(os.Stderr, opts)
	}
	logger = slog.New(h)
	slog.SetDefault(logger)
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error", "fatal", "panic":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// fatal logs an error and exits with a non-zero status.
func fatal(msg string, args ...any) {
	logger.Error(msg, args...)
	os.Exit(1)
}

func main() {
	flag.Parse()
	setupLogger()
	if *nItemsFlag == 0 {
		return
	}
	from, to, err := parseDateRange(*fromFlag, *toFlag)
	if err != nil {
		fatal("invalid -from/-to", "err", err)
	}
	if !*devFlag && *startFlag != "" {
		fatal("-start is only allowed in -dev mode")
	}
	if *headlessFlag && !*devFlag {
		fatal("-headless is only allowed in -dev mode")
	}

	// A context that is cancelled on SIGINT/SIGTERM so we can shut down
	// gracefully. Because the last successfully downloaded item is recorded in
	// .lastdone after every download, interrupting the run never loses more than
	// the item currently in flight.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	s, err := NewSession()
	if err != nil {
		fatal("could not create session", "err", err)
	}
	defer s.Shutdown()

	logger.Info("starting", "session_dir", s.profileDir, "download_dir", s.dlDir)

	if err := s.cleanDlDir(); err != nil {
		fatal("could not clean download dir", "err", err)
	}

	browserCtx, cancel := s.NewContext(ctx)
	defer cancel()

	if err := s.login(browserCtx); err != nil {
		if ctx.Err() != nil {
			logger.Warn("interrupted during login")
			return
		}
		fatal("login failed", "err", err)
	}

	if err := chromedp.Run(browserCtx,
		chromedp.ActionFunc(s.firstNav),
		chromedp.ActionFunc(s.navN(*nItemsFlag, from, to)),
	); err != nil {
		if ctx.Err() != nil {
			logger.Warn("interrupted; progress saved to .lastdone")
			return
		}
		fatal("run failed", "err", err)
	}
	logger.Info("all done")
	fmt.Println("OK")
}

type Session struct {
	parentContext context.Context
	parentCancel  context.CancelFunc
	dlDir         string // dir where the photos get stored
	profileDir    string // user data session dir. automatically created on chrome startup.
	// lastDone is the most recent (wrt to Google Photos timeline) item (its URL
	// really) that was downloaded. If set, it is used as a sentinel, to indicate that
	// we should skip dowloading all items older than this one.
	lastDone string
	// firstItem is the most recent item in the feed. It is determined at the
	// beginning of the run, and is used as the final sentinel.
	firstItem string
}

// defaultDownloadDir returns the directory downloads are written to, honouring
// the -dldir flag and otherwise defaulting to $HOME/Downloads/gphotos-cdp.
func defaultDownloadDir() string {
	if *dlDirFlag != "" {
		return *dlDirFlag
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = os.Getenv("HOME")
	}
	return filepath.Join(home, "Downloads", "gphotos-cdp")
}

// getLastDone returns the URL of the most recent item that was downloaded in
// the previous run. If any, it should have been stored in dlDir/.lastdone
func getLastDone(dlDir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(dlDir, ".lastdone"))
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func NewSession() (*Session, error) {
	var dir string
	if *devFlag {
		dir = *sessDirFlag
		if err := os.MkdirAll(dir, 0700); err != nil {
			return nil, err
		}
	} else {
		var err error
		dir, err = os.MkdirTemp("", "gphotos-cdp")
		if err != nil {
			return nil, err
		}
	}
	dlDir := defaultDownloadDir()
	if err := os.MkdirAll(dlDir, 0700); err != nil {
		return nil, err
	}
	lastDone, err := getLastDone(dlDir)
	if err != nil {
		return nil, err
	}
	s := &Session{
		profileDir: dir,
		dlDir:      dlDir,
		lastDone:   lastDone,
	}
	return s, nil
}

// NewContext builds a chromedp browser context whose lifetime is bound to
// parent, so cancelling parent (e.g. on Ctrl+C) tears down Chrome cleanly.
func (s *Session) NewContext(parent context.Context) (context.Context, context.CancelFunc) {
	opts := []chromedp.ExecAllocatorOption{
		chromedp.NoFirstRun,
		chromedp.NoDefaultBrowserCheck,
		chromedp.UserDataDir(s.profileDir),
		chromedp.Flag("enable-automation", true),
		chromedp.Flag("disable-web-security", true),
		chromedp.Flag("allow-running-insecure-content", true),
	}
	if *execPathFlag != "" {
		opts = append(opts, chromedp.ExecPath(*execPathFlag))
	}

	if *headlessFlag {
		opts = append(opts, chromedp.Headless)
	} else {
		// run with a visible window so the user can authenticate.
		opts = append(opts, chromedp.Flag("headless", false))
		opts = append(opts, chromedp.Flag("hide-scrollbars", false))
		opts = append(opts, chromedp.Flag("mute-audio", false))
		opts = append(opts, chromedp.Flag("disable-gpu", false))
	}
	ctx, cancel := chromedp.NewExecAllocator(parent, opts...)
	s.parentContext = ctx
	s.parentCancel = cancel
	ctx, cancel = chromedp.NewContext(s.parentContext)
	return ctx, cancel
}

func (s *Session) Shutdown() {
	if s.parentCancel != nil {
		s.parentCancel()
	}
}

// cleanDlDir removes all files (but not directories) from s.dlDir, except for
// the bookkeeping and debug files.
func (s *Session) cleanDlDir() error {
	if s.dlDir == "" {
		return nil
	}
	entries, err := os.ReadDir(s.dlDir)
	if err != nil {
		return err
	}
	for _, v := range entries {
		if v.IsDir() {
			continue
		}
		if isIgnoredDLFile(v.Name()) {
			continue
		}
		if err := os.Remove(filepath.Join(s.dlDir, v.Name())); err != nil {
			return err
		}
	}
	return nil
}

// isIgnoredDLFile reports whether name is a bookkeeping/debug file in the
// download dir that must not be treated as a downloaded photo.
func isIgnoredDLFile(name string) bool {
	switch name {
	case ".lastdone", ".lastdone.bak", "debug.png", "debug.html":
		return true
	}
	return false
}

// dumpDebug writes a screenshot and the page HTML to the download dir to help
// diagnose failures (e.g. authentication problems).
func (s *Session) dumpDebug(ctx context.Context, reason string) {
	var buf []byte
	if err := chromedp.CaptureScreenshot(&buf).Do(ctx); err == nil {
		_ = os.WriteFile(filepath.Join(s.dlDir, "debug.png"), buf, 0600)
	}
	var html string
	if err := chromedp.OuterHTML("html", &html, chromedp.ByQuery).Do(ctx); err == nil {
		_ = os.WriteFile(filepath.Join(s.dlDir, "debug.html"), []byte(html), 0600)
	}
	logger.Warn("wrote debug artifacts", "reason", reason, "dir", s.dlDir)
}

// login navigates to https://photos.google.com/ and waits for the user to have
// authenticated (or for 2 minutes to have elapsed).
func (s *Session) login(ctx context.Context) error {
	return chromedp.Run(ctx,
		browser.SetDownloadBehavior(browser.SetDownloadBehaviorBehaviorAllow).WithDownloadPath(s.dlDir),
		chromedp.Navigate(libraryURL),
		// when we're not authenticated, the URL is actually
		// https://www.google.com/photos/about/ , so we rely on that to detect when we have
		// authenticated.
		chromedp.ActionFunc(func(ctx context.Context) error {
			tick := time.Second
			timeout := time.Now().Add(2 * time.Minute)
			var location string
			for {
				if time.Now().After(timeout) {
					s.dumpDebug(ctx, "auth-timeout")
					return errors.New("timeout waiting for authentication")
				}
				if err := chromedp.Location(&location).Do(ctx); err != nil {
					return err
				}
				if location == libraryURL {
					logger.Info("authenticated")
					return nil
				}
				if *headlessFlag {
					s.dumpDebug(ctx, "headless-auth")
					return errors.New("authentication is not possible in -headless mode")
				}
				logger.Debug("not yet authenticated", "location", location)
				time.Sleep(tick)
			}
		}),
	)
}

// startURL is the page the run starts from: the main library, or an album when
// -album is set.
func startURL() string {
	if *albumFlag == "" {
		return libraryURL
	}
	if strings.HasPrefix(*albumFlag, "http://") || strings.HasPrefix(*albumFlag, "https://") {
		return *albumFlag
	}
	return strings.TrimRight(libraryURL, "/") + "/" + *albumTypeFlag + "/" + *albumFlag
}

// navigate goes to url, waits for the body to be ready, and retries a few times
// with linear backoff on transient failures. It returns the HTTP status code of
// the navigation.
func navigate(ctx context.Context, url string) (int64, error) {
	var lastErr error
	var status int64
	for attempt := 1; attempt <= 4; attempt++ {
		resp, err := chromedp.RunResponse(ctx, chromedp.Navigate(url))
		if err == nil {
			if resp != nil {
				status = resp.Status
			}
			if err := chromedp.WaitReady("body", chromedp.ByQuery).Do(ctx); err == nil {
				return status, nil
			} else {
				lastErr = err
			}
		} else {
			lastErr = err
		}
		if ctx.Err() != nil {
			return status, ctx.Err()
		}
		logger.Debug("navigation failed, retrying", "url", url, "attempt", attempt, "err", lastErr)
		time.Sleep(time.Duration(attempt) * time.Second)
	}
	return status, fmt.Errorf("navigating to %s: %w", url, lastErr)
}

// firstNav does either of:
// 1) if -album is set, navigates to that album;
// 2) if a specific photo URL was specified with -start, it navigates to it;
// 3) if the last session marked what was the most recent downloaded photo, it navigates to it;
// 4) otherwise it jumps to the end of the timeline (i.e. the oldest photo).
func (s *Session) firstNav(ctx context.Context) error {
	if *albumFlag != "" {
		if _, err := navigate(ctx, startURL()); err != nil {
			return err
		}
	}

	if err := s.setFirstItem(ctx); err != nil {
		return err
	}

	if *startFlag != "" {
		if _, err := navigate(ctx, *startFlag); err != nil {
			return err
		}
		return nil
	}
	if s.lastDone != "" {
		status, err := navigate(ctx, s.lastDone)
		if err != nil {
			return err
		}
		if status == http.StatusOK {
			return nil
		}
		lastDoneFile := filepath.Join(s.dlDir, ".lastdone")
		logger.Warn(".lastdone target no longer exists, restarting from scratch",
			"location", s.lastDone, "status", status, "file", lastDoneFile)
		s.lastDone = ""
		if err := os.Remove(lastDoneFile); err != nil && !os.IsNotExist(err) {
			return err
		}
		if _, err := navigate(ctx, startURL()); err != nil {
			return err
		}
	}

	if err := navToEnd(ctx); err != nil {
		return err
	}

	if err := navToLast(ctx); err != nil {
		return err
	}

	return nil
}

// setFirstItem looks for the first item, and sets it as s.firstItem.
// We always run it first even for code paths that might not need s.firstItem,
// because we also run it for the side-effect of waiting for the first page load to
// be done, and to be ready to receive scroll key events.
func (s *Session) setFirstItem(ctx context.Context) error {
	// wait for page to be loaded, i.e. that we can make an element active by using
	// the right arrow key.
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		chromedp.KeyEvent(kb.ArrowRight).Do(ctx)
		time.Sleep(tick)
		attributes := make(map[string]string)
		if err := chromedp.Run(ctx,
			chromedp.Attributes(`document.activeElement`, &attributes, chromedp.ByJSPath)); err != nil {
			return err
		}
		if len(attributes) == 0 {
			time.Sleep(tick)
			continue
		}

		photoHref, ok := attributes["href"]
		if !ok || !strings.HasPrefix(photoHref, "./photo/") {
			time.Sleep(tick)
			continue
		}

		s.firstItem = strings.TrimPrefix(photoHref, "./photo/")
		break
	}
	logger.Debug("page loaded", "most_recent_item", s.firstItem)
	return nil
}

// navToEnd scrolls down to the end of the page, i.e. to the oldest items.
func navToEnd(ctx context.Context) error {
	// try jumping to the end of the page. detect we are there and have stopped
	// moving when two consecutive screenshots are identical.
	// a minimum floor of duplicate screenshots is used to overcome any
	// false positives in determining the end of some larger libraries.
	var previousScr, scr []byte
	const minAmountOfDuplicateScr = 3
	amountOfDuplicateScr := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		chromedp.KeyEvent(kb.PageDown).Do(ctx)
		chromedp.KeyEvent(kb.End).Do(ctx)
		if err := chromedp.CaptureScreenshot(&scr).Do(ctx); err != nil {
			return err
		}
		if previousScr == nil {
			previousScr = scr
			continue
		}
		if bytes.Equal(previousScr, scr) {
			amountOfDuplicateScr++
			logger.Debug("screen unchanged while scrolling to end",
				"count", amountOfDuplicateScr, "threshold", minAmountOfDuplicateScr)
			if amountOfDuplicateScr == minAmountOfDuplicateScr {
				break
			}
		} else {
			amountOfDuplicateScr = 0
		}
		previousScr = scr
		time.Sleep(10 * tick)
	}

	logger.Debug("successfully jumped to the end (oldest items)")
	return nil
}

// navToLast sends the right arrow key event until we've reached the very last
// (oldest) item, opening the first focused item into the photo view along the way.
func navToLast(ctx context.Context) error {
	var location, prevLocation string
	ready := false
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		chromedp.KeyEvent(kb.ArrowRight).Do(ctx)
		time.Sleep(tick)
		if !ready {
			// Open the focused grid item into the single-photo view.
			chromedp.KeyEvent("\n").Do(ctx)
			time.Sleep(tick)
		}
		if err := chromedp.Location(&location).Do(ctx); err != nil {
			return err
		}
		if !ready {
			if strings.Contains(location, "/photo/") {
				ready = true
				logger.Debug("entered photo view", "location", location)
			}
			continue
		}

		if location == prevLocation {
			break
		}
		prevLocation = location
	}
	return nil
}

// doRun runs *runFlag as a command on the given filePath.
func doRun(filePath string) error {
	if *runFlag == "" {
		return nil
	}
	logger.Debug("running post-download command", "cmd", *runFlag, "file", filePath)
	cmd := exec.Command(*runFlag, filePath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// navLeft navigates to the next item to the left (i.e. the next more recent item).
func navLeft(ctx context.Context) error {
	time.Sleep(tick)
	muNavWaiting.Lock()
	listenEvents = true
	muNavWaiting.Unlock()
	chromedp.KeyEvent(kb.ArrowLeft).Do(ctx)
	muNavWaiting.Lock()
	navWaiting = true
	muNavWaiting.Unlock()
	t := time.NewTimer(time.Minute)
	select {
	case <-navDone:
		if !t.Stop() {
			<-t.C
		}
	case <-t.C:
		return errors.New("timeout waiting for left navigation")
	case <-ctx.Done():
		return ctx.Err()
	}
	muNavWaiting.Lock()
	navWaiting = false
	muNavWaiting.Unlock()
	return nil
}

// markDone saves location in the dldir/.lastdone file, to indicate it is the
// most recent item downloaded
func markDone(dldir, location string) error {
	logger.Debug("marking as done", "location", location)
	oldPath := filepath.Join(dldir, ".lastdone")
	newPath := oldPath + ".bak"
	if err := os.Rename(oldPath, newPath); err != nil {
		if !os.IsNotExist(err) {
			return err
		}
	}
	if err := os.WriteFile(oldPath, []byte(location), 0600); err != nil {
		// restore from backup
		if err := os.Rename(newPath, oldPath); err != nil {
			if !os.IsNotExist(err) {
				return err
			}
		}
		return err
	}
	return nil
}

// startDownload sends the Shift+D event, to start the download of the currently
// viewed item.
func startDownload(ctx context.Context) error {
	keyD, ok := kb.Keys['D']
	if !ok {
		return errors.New("no D key")
	}

	down := input.DispatchKeyEventParams{
		Key:                   keyD.Key,
		Code:                  keyD.Code,
		NativeVirtualKeyCode:  keyD.Native,
		WindowsVirtualKeyCode: keyD.Windows,
		Type:                  input.KeyDown,
		Modifiers:             input.ModifierShift,
	}
	if runtime.GOOS == "darwin" {
		down.NativeVirtualKeyCode = 0
	}
	up := down
	up.Type = input.KeyUp

	for _, ev := range []*input.DispatchKeyEventParams{&down, &up} {
		logger.Debug("dispatching key event", "event", fmt.Sprintf("%+v", *ev))
		if err := ev.Do(ctx); err != nil {
			return err
		}
	}
	return nil
}

// download starts the download of the currently viewed item, and on successful
// completion saves its location as the most recent item downloaded. It returns
// the list of files that were downloaded (more than one for e.g. live photos),
// and an error if the download stops making any progress for too long.
func (s *Session) download(ctx context.Context, location string) ([]string, error) {
	if err := startDownload(ctx); err != nil {
		return nil, err
	}

	started := false
	var maxSize int64
	deadline := time.Now().Add(*dlTimeoutFlag)
	cleanPolls := 0
	var result []string

	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		time.Sleep(tick)

		entries, err := os.ReadDir(s.dlDir)
		if err != nil {
			return nil, err
		}

		var completed []string
		var totalSize int64
		tempPresent := false
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			name := e.Name()
			if isIgnoredDLFile(name) {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			totalSize += info.Size()
			if strings.HasSuffix(name, ".crdownload") {
				tempPresent = true
			} else {
				completed = append(completed, name)
			}
		}

		if !started {
			if tempPresent || len(completed) > 0 {
				started = true
				deadline = time.Now().Add(*dlTimeoutFlag)
			} else if time.Now().After(deadline) {
				return nil, fmt.Errorf("downloading in %q took too long to start", s.dlDir)
			} else {
				continue
			}
		}

		if totalSize > maxSize {
			// push back the timeout as long as we make progress
			maxSize = totalSize
			deadline = time.Now().Add(*dlTimeoutFlag)
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("hit deadline while downloading in %q", s.dlDir)
		}

		// The download is over once there are no in-progress (.crdownload)
		// files and at least one completed file. We require two consecutive
		// clean polls to avoid racing the second file of a live photo.
		if !tempPresent && len(completed) > 0 {
			cleanPolls++
			if cleanPolls >= 2 {
				result = completed
				break
			}
		} else {
			cleanPolls = 0
		}
	}

	if err := markDone(s.dlDir, location); err != nil {
		return nil, err
	}

	sort.Strings(result)
	return result, nil
}

// moveDownload moves the downloaded files into their destination directory,
// optionally setting their modification time to the photo date. It returns the
// new paths of the moved files.
func (s *Session) moveDownload(files []string, location string, photoTime time.Time, haveTime bool) ([]string, error) {
	itemID := photoIDFromURL(location)
	dir := organizedDir(s.dlDir, photoTime, haveTime, itemID, *organizeFlag)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	var out []string
	for _, name := range files {
		src := filepath.Join(s.dlDir, name)
		dst := filepath.Join(dir, name)
		if err := os.Rename(src, dst); err != nil {
			return nil, err
		}
		if haveTime && (*organizeFlag || *mtimeFlag) {
			if err := os.Chtimes(dst, photoTime, photoTime); err != nil {
				logger.Warn("could not set modification time", "file", dst, "err", err)
			}
		}
		out = append(out, dst)
	}
	return out, nil
}

func (s *Session) dlAndMove(ctx context.Context, location string, photoTime time.Time, haveTime bool) ([]string, error) {
	files, err := s.download(ctx, location)
	if err != nil {
		return nil, err
	}
	return s.moveDownload(files, location, photoTime, haveTime)
}

// photoIDFromURL extracts the Google Photos item id from a photo URL. It works
// for both main-library URLs (.../photo/<id>) and album URLs
// (.../album/<album>/photo/<id>), falling back to the last path segment.
func photoIDFromURL(u string) string {
	if i := strings.IndexAny(u, "?#"); i >= 0 {
		u = u[:i]
	}
	u = strings.TrimRight(u, "/")
	if i := strings.Index(u, "/photo/"); i >= 0 {
		id := u[i+len("/photo/"):]
		if j := strings.IndexByte(id, '/'); j >= 0 {
			id = id[:j]
		}
		if id != "" {
			return id
		}
	}
	parts := strings.Split(u, "/")
	return parts[len(parts)-1]
}

// organizedDir computes the destination directory for a downloaded item. When
// organize is set and the photo date is known, items are grouped into YYYY/MM
// folders; otherwise each item keeps its own id-named folder (the historical
// behaviour).
func organizedDir(base string, photoTime time.Time, haveTime bool, itemID string, organize bool) string {
	if organize {
		if haveTime {
			return filepath.Join(base, photoTime.Format("2006"), photoTime.Format("01"))
		}
		return filepath.Join(base, "unknown")
	}
	return filepath.Join(base, itemID)
}

var (
	muNavWaiting             sync.RWMutex
	listenEvents, navWaiting = false, false
	navDone                  = make(chan bool, 1)
)

func listenNavEvents(ctx context.Context) {
	chromedp.ListenTarget(ctx, func(ev interface{}) {
		muNavWaiting.RLock()
		listen := listenEvents
		muNavWaiting.RUnlock()
		if !listen {
			return
		}
		switch ev.(type) {
		case *page.EventNavigatedWithinDocument:
			go func() {
				for {
					muNavWaiting.RLock()
					waiting := navWaiting
					muNavWaiting.RUnlock()
					if waiting {
						navDone <- true
						break
					}
					time.Sleep(tick)
				}
			}()
		}
	})
}

// navN successively downloads the currently viewed item, and navigates to the
// next item (to the left). It repeats N times or until the last (i.e. the most
// recent) item is reached. Set a negative N to repeat until the end is reached.
func (s *Session) navN(N int, from, to time.Time) func(context.Context) error {
	return func(ctx context.Context) error {
		n := 0
		if N == 0 {
			return nil
		}

		listenNavEvents(ctx)

		dateNeeded := !from.IsZero() || !to.IsZero() || *organizeFlag || *mtimeFlag
		if dateNeeded {
			// The info side panel persists across navigation and exposes the
			// capture date, so we open it once up front.
			openInfoPanel(ctx)
		}

		var location, prevLocation string
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := chromedp.Location(&location).Do(ctx); err != nil {
				return err
			}
			if location == prevLocation {
				break
			}
			prevLocation = location

			var photoTime time.Time
			haveTime := false
			if dateNeeded {
				photoTime, haveTime = getPhotoTime(ctx)
			}
			download, stop := dateDecision(photoTime, haveTime, from, to)
			if stop {
				logger.Info("reached -to bound, stopping", "location", location)
				break
			}

			if download {
				filePaths, err := s.dlAndMove(ctx, location, photoTime, haveTime)
				if err != nil {
					return err
				}
				for _, f := range filePaths {
					if err := doRun(f); err != nil {
						return err
					}
				}
				n++
				logger.Info("downloaded", "n", n, "files", len(filePaths), "location", location)
				if N > 0 && n >= N {
					break
				}
			} else {
				logger.Debug("skipping item (out of date range)", "location", location)
			}

			if strings.HasSuffix(location, s.firstItem) {
				break
			}

			if err := navLeft(ctx); err != nil {
				return fmt.Errorf("error at %v: %v", location, err)
			}
		}
		logger.Info("navigation complete", "downloaded", n)
		return nil
	}
}

// openInfoPanel toggles the Google Photos info side panel open (keyboard 'i'),
// which reveals the capture date used by the date-based features. Best-effort.
func openInfoPanel(ctx context.Context) {
	chromedp.KeyEvent("i").Do(ctx)
	time.Sleep(tick)
}

// getPhotoTime makes a best-effort attempt to read the capture date of the
// currently viewed item from the DOM. It never returns an error: if no date can
// be found or parsed, it reports haveTime=false and the caller proceeds as if
// dates were unavailable.
func getPhotoTime(ctx context.Context) (time.Time, bool) {
	const js = `(function(){
		var out=[];
		document.querySelectorAll('[aria-label]').forEach(function(e){
			var l=e.getAttribute('aria-label');
			if(l && /\d{4}/.test(l)) out.push(l);
		});
		document.querySelectorAll('time').forEach(function(e){
			var d=e.getAttribute('datetime');
			if(d) out.push(d);
			if(e.textContent) out.push(e.textContent);
		});
		return out.join('\n');
	})()`
	var raw string
	if err := chromedp.Evaluate(js, &raw).Do(ctx); err != nil {
		logger.Debug("could not read photo date", "err", err)
		return time.Time{}, false
	}
	return extractPhotoTime(raw)
}

// --- date parsing/decision helpers (pure, unit-tested) ---

var spaceReplacer = strings.NewReplacer(
	" ", " ", // no-break space
	" ", " ", // narrow no-break space (Google uses this before AM/PM)
	" ", " ", // thin space
	" ", " ", // figure space
	" ", " ", // hair space
)

func normalizeSpaces(s string) string { return spaceReplacer.Replace(s) }

var (
	reMonthDate = regexp.MustCompile(`[A-Z][a-z]{2,8}\s+\d{1,2},\s+\d{4}(?:,?\s+\d{1,2}:\d{2}(?::\d{2})?\s*(?:AM|PM|am|pm)?)?`)
	reISODate   = regexp.MustCompile(`\d{4}-\d{2}-\d{2}(?:[ T]\d{2}:\d{2}(?::\d{2})?(?:Z|[+-]\d{2}:?\d{2})?)?`)
	reWeekday   = regexp.MustCompile(`^(?:Mon|Tue|Wed|Thu|Fri|Sat|Sun)[a-z]*,?\s+`)
)

var photoTimeLayouts = []string{
	"Jan 2, 2006, 3:04:05 PM",
	"Jan 2, 2006, 3:04 PM",
	"Jan 2, 2006, 15:04:05",
	"Jan 2, 2006, 15:04",
	"Jan 2, 2006",
	"January 2, 2006, 3:04:05 PM",
	"January 2, 2006, 3:04 PM",
	"January 2, 2006",
	"2006-01-02T15:04:05Z07:00",
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05",
	"2006-01-02 15:04",
	"2006-01-02",
}

// parsePhotoTime parses a single date string (already extracted from the page)
// against the set of formats Google Photos is known to render.
func parsePhotoTime(s string) (time.Time, bool) {
	s = strings.TrimSpace(normalizeSpaces(s))
	s = reWeekday.ReplaceAllString(s, "")
	// Google sometimes abbreviates September as "Sept".
	s = strings.Replace(s, "Sept ", "Sep ", 1)
	for _, layout := range photoTimeLayouts {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// extractPhotoTime finds the first parseable date in a blob of text scraped from
// the page. ISO dates are preferred because they are unambiguous.
func extractPhotoTime(raw string) (time.Time, bool) {
	raw = normalizeSpaces(raw)
	for _, m := range reISODate.FindAllString(raw, -1) {
		if t, ok := parsePhotoTime(m); ok {
			return t, true
		}
	}
	for _, m := range reMonthDate.FindAllString(raw, -1) {
		if t, ok := parsePhotoTime(m); ok {
			return t, true
		}
	}
	return time.Time{}, false
}

// parseDate parses a YYYY-MM-DD flag value in the local timezone.
func parseDate(s string) (time.Time, error) {
	return time.ParseInLocation("2006-01-02", s, time.Local)
}

// parseDateRange parses the -from/-to flags. from is inclusive (start of day);
// to is exclusive (start of the day after), so -to is inclusive of that whole
// day. Either may be the zero time, meaning "unbounded".
func parseDateRange(fromS, toS string) (from, to time.Time, err error) {
	if fromS != "" {
		from, err = parseDate(fromS)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("invalid -from %q: %w", fromS, err)
		}
	}
	if toS != "" {
		t, e := parseDate(toS)
		if e != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("invalid -to %q: %w", toS, e)
		}
		to = t.AddDate(0, 0, 1)
	}
	if !from.IsZero() && !to.IsZero() && !from.Before(to) {
		return time.Time{}, time.Time{}, fmt.Errorf("-from (%s) must be before -to (%s)", fromS, toS)
	}
	return from, to, nil
}

// dateDecision decides, given an item's (possibly unknown) capture time and the
// requested [from,to) range, whether to download it and whether to stop the run.
// The timeline is traversed oldest -> newest, so once we pass the -to bound we
// can stop entirely. Items with an unknown date are always downloaded.
func dateDecision(photoTime time.Time, haveTime bool, from, to time.Time) (download, stop bool) {
	if !haveTime {
		return true, false
	}
	if !from.IsZero() && photoTime.Before(from) {
		return false, false // too old; skip but keep going (newer items follow)
	}
	if !to.IsZero() && !photoTime.Before(to) {
		return false, true // newer than range; stop
	}
	return true, false
}
