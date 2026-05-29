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
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakeItem is one scripted item the fakeBrowser walks through.
type fakeItem struct {
	location  string
	date      time.Time
	haveDate  bool
	files     []string // base filenames "downloaded" into the download dir
	failTimes int      // number of leading Download attempts that should fail
}

// fakeBrowser implements Browser without a real Chrome: Location/PhotoTime read
// the current scripted item, Download writes the item's files into dlDir, and
// Next advances. It lets navN's orchestration be tested end-to-end.
type fakeBrowser struct {
	dlDir     string
	items     []fakeItem
	idx       int
	starts    int
	panels    int
	nexts     int
	downloads int
	attempts  map[string]int // Download calls per location, for failTimes
}

func (b *fakeBrowser) Start(context.Context) error         { b.starts++; return nil }
func (b *fakeBrowser) OpenInfoPanel(context.Context) error { b.panels++; return nil }

func (b *fakeBrowser) Location(context.Context) (string, error) { return b.cur().location, nil }

func (b *fakeBrowser) PhotoTime(context.Context) (time.Time, bool) {
	it := b.cur()
	return it.date, it.haveDate
}

func (b *fakeBrowser) Download(context.Context) ([]string, error) {
	b.downloads++
	it := b.cur()
	if b.attempts == nil {
		b.attempts = map[string]int{}
	}
	b.attempts[it.location]++
	if b.attempts[it.location] <= it.failTimes {
		return nil, fmt.Errorf("fake download failure #%d for %s", b.attempts[it.location], it.location)
	}
	var names []string
	for _, n := range it.files {
		if err := os.WriteFile(filepath.Join(b.dlDir, n), []byte("fake:"+n), 0600); err != nil {
			return nil, err
		}
		names = append(names, n)
	}
	return names, nil
}

func (b *fakeBrowser) Next(context.Context) error {
	b.nexts++
	if b.idx < len(b.items)-1 {
		b.idx++
	}
	return nil
}

func (b *fakeBrowser) cur() fakeItem {
	if b.idx >= len(b.items) {
		return b.items[len(b.items)-1]
	}
	return b.items[b.idx]
}

var _ Browser = (*fakeBrowser)(nil)

func photo(id string) string { return "https://photos.google.com/photo/" + id }

func mustExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Errorf("expected file to exist: %s (%v)", path, err)
	}
}

func mustNotExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("expected file to be absent: %s (err=%v)", path, err)
	}
}

func TestNavNDownloadsAll(t *testing.T) {
	dir := t.TempDir()
	s := &Session{dlDir: dir, firstItem: "id3"}
	m, _ := LoadManifest(dir)
	fb := &fakeBrowser{dlDir: dir, items: []fakeItem{
		{location: photo("id1"), files: []string{"a.jpg"}},
		{location: photo("id2"), files: []string{"b.jpg"}},
		{location: photo("id3"), files: []string{"c.jpg"}},
	}}

	if err := s.navN(fb, m, -1, time.Time{}, time.Time{})(context.Background()); err != nil {
		t.Fatalf("navN: %v", err)
	}

	// Default (no -organize): each item gets its own id-named folder.
	mustExist(t, filepath.Join(dir, "id1", "a.jpg"))
	mustExist(t, filepath.Join(dir, "id2", "b.jpg"))
	mustExist(t, filepath.Join(dir, "id3", "c.jpg"))

	if c := m.Counts(); c[StatusDone] != 3 {
		t.Errorf("done count = %d, want 3 (counts=%v)", c[StatusDone], c)
	}
	if fb.starts != 1 {
		t.Errorf("Start called %d times, want 1", fb.starts)
	}
	// Manifest persisted, and .lastdone points at the most recent item.
	mustExist(t, filepath.Join(dir, manifestName))
	if last, _ := getLastDone(dir); last != photo("id3") {
		t.Errorf(".lastdone = %q, want %q", last, photo("id3"))
	}
}

func TestNavNRespectsN(t *testing.T) {
	dir := t.TempDir()
	s := &Session{dlDir: dir, firstItem: "id3"}
	m, _ := LoadManifest(dir)
	fb := &fakeBrowser{dlDir: dir, items: []fakeItem{
		{location: photo("id1"), files: []string{"a.jpg"}},
		{location: photo("id2"), files: []string{"b.jpg"}},
		{location: photo("id3"), files: []string{"c.jpg"}},
	}}

	if err := s.navN(fb, m, 2, time.Time{}, time.Time{})(context.Background()); err != nil {
		t.Fatalf("navN: %v", err)
	}

	mustExist(t, filepath.Join(dir, "id1", "a.jpg"))
	mustExist(t, filepath.Join(dir, "id2", "b.jpg"))
	mustNotExist(t, filepath.Join(dir, "id3", "c.jpg"))
	if c := m.Counts(); c[StatusDone] != 2 {
		t.Errorf("done count = %d, want 2", c[StatusDone])
	}
}

func TestNavNDateFilter(t *testing.T) {
	dir := t.TempDir()
	s := &Session{dlDir: dir, firstItem: "id3"}
	m, _ := LoadManifest(dir)
	fb := &fakeBrowser{dlDir: dir, items: []fakeItem{
		{location: photo("id1"), date: time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC), haveDate: true, files: []string{"a.jpg"}},
		{location: photo("id2"), date: time.Date(2024, 2, 15, 0, 0, 0, 0, time.UTC), haveDate: true, files: []string{"b.jpg"}},
		{location: photo("id3"), date: time.Date(2024, 4, 1, 0, 0, 0, 0, time.UTC), haveDate: true, files: []string{"c.jpg"}},
	}}

	from := time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC) // exclusive

	if err := s.navN(fb, m, -1, from, to)(context.Background()); err != nil {
		t.Fatalf("navN: %v", err)
	}

	mustNotExist(t, filepath.Join(dir, "id1", "a.jpg")) // too old, skipped
	mustExist(t, filepath.Join(dir, "id2", "b.jpg"))    // in range
	mustNotExist(t, filepath.Join(dir, "id3", "c.jpg")) // past -to, stop

	if it, ok := m.Get("id1"); !ok || it.Status != StatusSkipped {
		t.Errorf("id1 status = %v (ok=%v), want skipped", statusOf(it), ok)
	}
	if it, ok := m.Get("id2"); !ok || it.Status != StatusDone {
		t.Errorf("id2 status = %v (ok=%v), want done", statusOf(it), ok)
	}
	// id3 was seen (panel/date read) before the stop decision, so it's pending.
	if it, ok := m.Get("id3"); !ok || it.Status != StatusPending {
		t.Errorf("id3 status = %v (ok=%v), want pending", statusOf(it), ok)
	}
}

func TestNavNDryRunWritesNothing(t *testing.T) {
	dir := t.TempDir()
	defer func(v bool) { *dryRunFlag = v }(*dryRunFlag)
	*dryRunFlag = true

	s := &Session{dlDir: dir, firstItem: "id2"}
	m, _ := LoadManifest(dir)
	fb := &fakeBrowser{dlDir: dir, items: []fakeItem{
		{location: photo("id1"), files: []string{"a.jpg"}},
		{location: photo("id2"), files: []string{"b.jpg"}},
	}}

	if err := s.navN(fb, m, -1, time.Time{}, time.Time{})(context.Background()); err != nil {
		t.Fatalf("navN: %v", err)
	}

	if fb.downloads != 0 {
		t.Errorf("dry-run triggered %d downloads, want 0", fb.downloads)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("dry-run wrote to the download dir: %v", names)
	}
}

func TestNavNMultiFileLivePhoto(t *testing.T) {
	dir := t.TempDir()
	s := &Session{dlDir: dir, firstItem: "id1"}
	m, _ := LoadManifest(dir)
	fb := &fakeBrowser{dlDir: dir, items: []fakeItem{
		{location: photo("id1"), files: []string{"live.jpg", "live.mp4"}},
	}}

	if err := s.navN(fb, m, -1, time.Time{}, time.Time{})(context.Background()); err != nil {
		t.Fatalf("navN: %v", err)
	}

	mustExist(t, filepath.Join(dir, "id1", "live.jpg"))
	mustExist(t, filepath.Join(dir, "id1", "live.mp4"))
	it, ok := m.Get("id1")
	if !ok || len(it.Files) != 2 || it.Bytes == 0 {
		t.Errorf("live photo item wrong: %+v (ok=%v)", it, ok)
	}
}

func TestNavNOrganize(t *testing.T) {
	dir := t.TempDir()
	defer func(v bool) { *organizeFlag = v }(*organizeFlag)
	*organizeFlag = true

	s := &Session{dlDir: dir, firstItem: "id1"}
	m, _ := LoadManifest(dir)
	fb := &fakeBrowser{dlDir: dir, items: []fakeItem{
		{location: photo("id1"), date: time.Date(2024, 3, 14, 0, 0, 0, 0, time.UTC), haveDate: true, files: []string{"a.jpg"}},
	}}

	if err := s.navN(fb, m, -1, time.Time{}, time.Time{})(context.Background()); err != nil {
		t.Fatalf("navN: %v", err)
	}

	mustExist(t, filepath.Join(dir, "2024", "03", "a.jpg"))
	if it, ok := m.Get("id1"); !ok || it.Status != StatusDone || len(it.Files) != 1 {
		t.Errorf("organized item wrong: %+v (ok=%v)", it, ok)
	}
}

func TestNavNRetriesTransientFailure(t *testing.T) {
	defer func(d time.Duration) { retryBaseDelay = d }(retryBaseDelay)
	retryBaseDelay = 0 // no backoff in tests

	dir := t.TempDir()
	s := &Session{dlDir: dir, firstItem: "id1"}
	m, _ := LoadManifest(dir)
	fb := &fakeBrowser{dlDir: dir, items: []fakeItem{
		{location: photo("id1"), files: []string{"a.jpg"}, failTimes: 2}, // fails twice, ok on 3rd
	}}

	if err := s.navN(fb, m, -1, time.Time{}, time.Time{})(context.Background()); err != nil {
		t.Fatalf("navN: %v", err)
	}

	mustExist(t, filepath.Join(dir, "id1", "a.jpg"))
	if it, ok := m.Get("id1"); !ok || it.Status != StatusDone {
		t.Errorf("id1 status = %v, want done", statusOf(it))
	}
	if fb.downloads != 3 {
		t.Errorf("downloads = %d, want 3 (2 failures + 1 success)", fb.downloads)
	}
}

func TestNavNErrorIsolationContinues(t *testing.T) {
	defer func(d time.Duration) { retryBaseDelay = d }(retryBaseDelay)
	retryBaseDelay = 0

	dir := t.TempDir()
	s := &Session{dlDir: dir, firstItem: "id3"}
	m, _ := LoadManifest(dir)
	fb := &fakeBrowser{dlDir: dir, items: []fakeItem{
		{location: photo("id1"), files: []string{"a.jpg"}},
		{location: photo("id2"), files: []string{"b.jpg"}, failTimes: 99}, // always fails
		{location: photo("id3"), files: []string{"c.jpg"}},
	}}

	// A single bad item must not abort the run.
	if err := s.navN(fb, m, -1, time.Time{}, time.Time{})(context.Background()); err != nil {
		t.Fatalf("navN should not return an error on an isolated item failure: %v", err)
	}

	mustExist(t, filepath.Join(dir, "id1", "a.jpg"))
	mustNotExist(t, filepath.Join(dir, "id2", "b.jpg"))
	mustExist(t, filepath.Join(dir, "id3", "c.jpg"))

	c := m.Counts()
	if c[StatusDone] != 2 || c[StatusErrored] != 1 {
		t.Errorf("counts = %v, want done=2 errored=1", c)
	}
	if it, ok := m.Get("id2"); !ok || it.Status != StatusErrored || it.Attempts != *maxAttemptsFlag {
		t.Errorf("id2 = %+v, want errored with %d attempts", it, *maxAttemptsFlag)
	}
}

func TestNavNSkipsAlreadyDone(t *testing.T) {
	dir := t.TempDir()
	s := &Session{dlDir: dir, firstItem: "id2"}
	m, _ := LoadManifest(dir)
	// id1 was completed on a previous run.
	m.Done(photo("id1"), []string{"/prev/id1/a.jpg"}, 1)

	fb := &fakeBrowser{dlDir: dir, items: []fakeItem{
		{location: photo("id1"), files: []string{"a.jpg"}},
		{location: photo("id2"), files: []string{"b.jpg"}},
	}}

	if err := s.navN(fb, m, -1, time.Time{}, time.Time{})(context.Background()); err != nil {
		t.Fatalf("navN: %v", err)
	}

	// id1 must not be re-downloaded this run; id2 is new.
	if fb.attempts[photo("id1")] != 0 {
		t.Errorf("id1 was downloaded %d times, want 0 (already done)", fb.attempts[photo("id1")])
	}
	mustExist(t, filepath.Join(dir, "id2", "b.jpg"))
	mustNotExist(t, filepath.Join(dir, "id1", "a.jpg"))
}

func TestNavNGivesUpAfterMaxAttempts(t *testing.T) {
	defer func(v int) { *maxAttemptsFlag = v }(*maxAttemptsFlag)
	*maxAttemptsFlag = 2

	dir := t.TempDir()
	s := &Session{dlDir: dir, firstItem: "id1"}
	m, _ := LoadManifest(dir)
	// id1 already used up its attempts on previous runs.
	m.Errored(photo("id1"), fmt.Errorf("x"))
	m.Errored(photo("id1"), fmt.Errorf("x"))

	fb := &fakeBrowser{dlDir: dir, items: []fakeItem{
		{location: photo("id1"), files: []string{"a.jpg"}},
	}}

	if err := s.navN(fb, m, -1, time.Time{}, time.Time{})(context.Background()); err != nil {
		t.Fatalf("navN: %v", err)
	}

	if fb.downloads != 0 {
		t.Errorf("downloads = %d, want 0 (attempts already exhausted)", fb.downloads)
	}
	mustNotExist(t, filepath.Join(dir, "id1", "a.jpg"))
}

// statusOf is a nil-safe helper for error messages.
func statusOf(it *Item) ItemStatus {
	if it == nil {
		return "<nil>"
	}
	return it.Status
}
