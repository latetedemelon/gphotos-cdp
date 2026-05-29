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

package main

import (
	"context"
	"time"

	"github.com/chromedp/chromedp"
)

// Browser is the seam between the orchestration loop (navN) and the actual
// Chrome automation. Everything navN needs from the browser goes through this
// interface, so the loop's logic — date filtering, organization, manifest
// bookkeeping, stop conditions — can be unit-tested against a fake without a
// real Chrome (see fakeBrowser in browser_test.go). The production
// implementation, cdpBrowser, is a thin adapter over the existing chromedp
// helpers, so the live behaviour is unchanged.
type Browser interface {
	// Start performs any one-time setup (e.g. installing event listeners).
	Start(ctx context.Context) error
	// OpenInfoPanel best-effort opens the info side panel so PhotoTime can read
	// the capture date.
	OpenInfoPanel(ctx context.Context) error
	// Location returns the URL of the currently viewed item.
	Location(ctx context.Context) (string, error)
	// PhotoTime returns the capture date of the current item, best-effort.
	PhotoTime(ctx context.Context) (time.Time, bool)
	// Download triggers the download of the current item and waits for it to
	// finish, returning the base filenames that landed in the download dir.
	Download(ctx context.Context) ([]string, error)
	// Next navigates to the next (more recent) item.
	Next(ctx context.Context) error
}

// cdpBrowser is the production Browser, backed by chromedp and the session.
type cdpBrowser struct{ s *Session }

func (b cdpBrowser) Start(ctx context.Context) error {
	listenNavEvents(ctx)
	return nil
}

func (b cdpBrowser) OpenInfoPanel(ctx context.Context) error {
	ensureInfoPanel(ctx)
	return nil
}

func (b cdpBrowser) Location(ctx context.Context) (string, error) {
	var loc string
	if err := chromedp.Location(&loc).Do(ctx); err != nil {
		return "", err
	}
	return loc, nil
}

func (b cdpBrowser) PhotoTime(ctx context.Context) (time.Time, bool) {
	return getPhotoTime(ctx)
}

func (b cdpBrowser) Download(ctx context.Context) ([]string, error) {
	return b.s.download(ctx)
}

func (b cdpBrowser) Next(ctx context.Context) error {
	return navLeft(ctx)
}

// compile-time check that cdpBrowser satisfies Browser.
var _ Browser = cdpBrowser{}
