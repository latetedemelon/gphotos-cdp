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
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

func TestPhotoIDFromURL(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"https://photos.google.com/photo/AF1Qabc123", "AF1Qabc123"},
		{"https://photos.google.com/photo/AF1Qabc123/", "AF1Qabc123"},
		{"https://photos.google.com/album/XYZ/photo/AF1Qabc123", "AF1Qabc123"},
		{"https://photos.google.com/u/0/photo/AF1Qabc123", "AF1Qabc123"},
		{"https://photos.google.com/photo/AF1Qabc123?foo=bar", "AF1Qabc123"},
		{"https://photos.google.com/photo/AF1Qabc123#frag", "AF1Qabc123"},
		{"https://photos.google.com/share/AAA/photo/PID", "PID"},
		// No /photo/ segment: fall back to last path element.
		{"https://photos.google.com/lr/something/LASTSEG", "LASTSEG"},
	}
	for _, tc := range tests {
		if got := photoIDFromURL(tc.in); got != tc.want {
			t.Errorf("photoIDFromURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestParsePhotoTime(t *testing.T) {
	loc := time.Local
	tests := []struct {
		in   string
		want time.Time
		ok   bool
	}{
		{"Jan 2, 2006, 3:04:05 PM", time.Date(2006, 1, 2, 15, 4, 5, 0, loc), true},
		{"Mar 14, 2024, 12:08:27 PM", time.Date(2024, 3, 14, 12, 8, 27, 0, loc), true},
		{"Mar 14, 2024, 12:08 PM", time.Date(2024, 3, 14, 12, 8, 0, 0, loc), true},
		{"Mar 14, 2024", time.Date(2024, 3, 14, 0, 0, 0, 0, loc), true},
		{"January 2, 2006", time.Date(2006, 1, 2, 0, 0, 0, 0, loc), true},
		{"December 31, 2020, 11:59:59 PM", time.Date(2020, 12, 31, 23, 59, 59, 0, loc), true},
		{"2024-03-14", time.Date(2024, 3, 14, 0, 0, 0, 0, loc), true},
		{"2024-03-14 12:08:27", time.Date(2024, 3, 14, 12, 8, 27, 0, loc), true},
		// Weekday prefix should be stripped.
		{"Thursday, Mar 14, 2024", time.Date(2024, 3, 14, 0, 0, 0, 0, loc), true},
		// "Sept" abbreviation normalised to "Sep".
		{"Sept 5, 2021", time.Date(2021, 9, 5, 0, 0, 0, 0, loc), true},
		{"not a date", time.Time{}, false},
		{"", time.Time{}, false},
	}
	for _, tc := range tests {
		got, ok := parsePhotoTime(tc.in)
		if ok != tc.ok {
			t.Errorf("parsePhotoTime(%q) ok = %v, want %v", tc.in, ok, tc.ok)
			continue
		}
		if ok && !got.Equal(tc.want) {
			t.Errorf("parsePhotoTime(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestParsePhotoTimeNarrowSpace(t *testing.T) {
	// Google Photos uses a narrow no-break space (U+202F) before AM/PM.
	in := "Mar 14, 2024, 12:08:27 PM"
	got, ok := parsePhotoTime(in)
	if !ok {
		t.Fatalf("parsePhotoTime(%q) failed to parse", in)
	}
	want := time.Date(2024, 3, 14, 12, 8, 27, 0, time.Local)
	if !got.Equal(want) {
		t.Errorf("parsePhotoTime(%q) = %v, want %v", in, got, want)
	}
}

func TestExtractPhotoTime(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want time.Time
		ok   bool
	}{
		{
			name: "aria-label blob",
			in:   "Photo - Landscape\nPhoto taken on Mar 14, 2024, 12:08:27 PM\nDownload",
			want: time.Date(2024, 3, 14, 12, 8, 27, 0, time.Local),
			ok:   true,
		},
		{
			name: "iso datetime attribute preferred",
			in:   "some label with 2024 in it\n2024-03-14T08:00:00Z",
			want: time.Date(2024, 3, 14, 8, 0, 0, 0, time.UTC),
			ok:   true,
		},
		{
			name: "no date",
			in:   "Download\nShare\nInfo",
			want: time.Time{},
			ok:   false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := extractPhotoTime(tc.in)
			if ok != tc.ok {
				t.Fatalf("extractPhotoTime ok = %v, want %v", ok, tc.ok)
			}
			if ok && !got.Equal(tc.want) {
				t.Errorf("extractPhotoTime = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestParseDateRange(t *testing.T) {
	mustDate := func(s string) time.Time {
		d, err := parseDate(s)
		if err != nil {
			t.Fatalf("parseDate(%q): %v", s, err)
		}
		return d
	}

	from, to, err := parseDateRange("2024-01-01", "2024-12-31")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !from.Equal(mustDate("2024-01-01")) {
		t.Errorf("from = %v", from)
	}
	// to is exclusive: the day after 2024-12-31.
	if !to.Equal(mustDate("2024-12-31").AddDate(0, 0, 1)) {
		t.Errorf("to = %v, want start of 2025-01-01", to)
	}

	// Empty bounds are the zero time.
	from, to, err = parseDateRange("", "")
	if err != nil || !from.IsZero() || !to.IsZero() {
		t.Errorf("empty range: from=%v to=%v err=%v", from, to, err)
	}

	// from must be before to.
	if _, _, err := parseDateRange("2024-12-31", "2024-01-01"); err == nil {
		t.Error("expected error when from is after to")
	}

	// invalid format.
	if _, _, err := parseDateRange("not-a-date", ""); err == nil {
		t.Error("expected error for invalid -from")
	}
	if _, _, err := parseDateRange("", "2024/01/01"); err == nil {
		t.Error("expected error for invalid -to")
	}
}

func TestDateDecision(t *testing.T) {
	from := time.Date(2024, 2, 1, 0, 0, 0, 0, time.Local)
	to := time.Date(2024, 3, 1, 0, 0, 0, 0, time.Local) // exclusive

	tests := []struct {
		name         string
		t            time.Time
		haveTime     bool
		from, to     time.Time
		wantDownload bool
		wantStop     bool
	}{
		{"unknown date always downloads", time.Time{}, false, from, to, true, false},
		{"in range", time.Date(2024, 2, 15, 0, 0, 0, 0, time.Local), true, from, to, true, false},
		{"too old skips, keep going", time.Date(2024, 1, 15, 0, 0, 0, 0, time.Local), true, from, to, false, false},
		{"at to bound stops (exclusive)", time.Date(2024, 3, 1, 0, 0, 0, 0, time.Local), true, from, to, false, true},
		{"after to stops", time.Date(2024, 4, 1, 0, 0, 0, 0, time.Local), true, from, to, false, true},
		{"no bounds downloads", time.Date(1999, 1, 1, 0, 0, 0, 0, time.Local), true, time.Time{}, time.Time{}, true, false},
		{"only from, older skipped", time.Date(2024, 1, 1, 0, 0, 0, 0, time.Local), true, from, time.Time{}, false, false},
		{"only to, newer stops", time.Date(2025, 1, 1, 0, 0, 0, 0, time.Local), true, time.Time{}, to, false, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotDownload, gotStop := dateDecision(tc.t, tc.haveTime, tc.from, tc.to)
			if gotDownload != tc.wantDownload || gotStop != tc.wantStop {
				t.Errorf("dateDecision = (download=%v, stop=%v), want (download=%v, stop=%v)",
					gotDownload, gotStop, tc.wantDownload, tc.wantStop)
			}
		})
	}
}

func TestOrganizedDir(t *testing.T) {
	base := "/downloads"
	photoTime := time.Date(2024, 3, 14, 12, 0, 0, 0, time.Local)

	// organize off -> per-item-id folder (historical behaviour).
	if got := organizedDir(base, photoTime, true, "ITEMID", false); got != filepath.Join(base, "ITEMID") {
		t.Errorf("organize off: got %q", got)
	}
	// organize on with known date -> YYYY/MM.
	if got := organizedDir(base, photoTime, true, "ITEMID", true); got != filepath.Join(base, "2024", "03") {
		t.Errorf("organize on, known date: got %q", got)
	}
	// organize on with unknown date -> unknown.
	if got := organizedDir(base, time.Time{}, false, "ITEMID", true); got != filepath.Join(base, "unknown") {
		t.Errorf("organize on, unknown date: got %q", got)
	}
}

func TestLastDoneRoundTrip(t *testing.T) {
	dir := t.TempDir()

	// No file yet -> empty, no error.
	got, err := getLastDone(dir)
	if err != nil || got != "" {
		t.Fatalf("getLastDone empty: got %q err %v", got, err)
	}

	const loc = "https://photos.google.com/photo/AF1Qabc"
	if err := markDone(dir, loc); err != nil {
		t.Fatalf("markDone: %v", err)
	}
	got, err = getLastDone(dir)
	if err != nil || got != loc {
		t.Fatalf("getLastDone after mark: got %q err %v", got, err)
	}

	// Marking again should keep a .bak of the previous value.
	const loc2 = "https://photos.google.com/photo/AF1Qdef"
	if err := markDone(dir, loc2); err != nil {
		t.Fatalf("markDone 2: %v", err)
	}
	got, _ = getLastDone(dir)
	if got != loc2 {
		t.Fatalf("getLastDone after second mark: got %q", got)
	}
	bak, err := os.ReadFile(filepath.Join(dir, ".lastdone.bak"))
	if err != nil || string(bak) != loc {
		t.Fatalf(".lastdone.bak: got %q err %v", string(bak), err)
	}
}

func TestCleanDlDir(t *testing.T) {
	dir := t.TempDir()
	// Files that should be removed.
	for _, n := range []string{"photo1.jpg", "photo2.mp4", "x.crdownload"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	// Files/dirs that should be preserved.
	for _, n := range []string{".lastdone", ".lastdone.bak", "debug.png", "debug.html"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, "2024"), 0700); err != nil {
		t.Fatal(err)
	}

	s := &Session{dlDir: dir}
	if err := s.cleanDlDir(); err != nil {
		t.Fatalf("cleanDlDir: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	sort.Strings(got)
	want := []string{".lastdone", ".lastdone.bak", "2024", "debug.html", "debug.png"}
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("after clean got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("after clean got %v, want %v", got, want)
		}
	}
}

func TestStartURL(t *testing.T) {
	defer func() { *albumFlag = ""; *albumTypeFlag = "album" }()

	*albumFlag = ""
	if got := startURL(); got != libraryURL {
		t.Errorf("no album: got %q want %q", got, libraryURL)
	}

	*albumFlag = "ABC123"
	*albumTypeFlag = "album"
	if got, want := startURL(), "https://photos.google.com/album/ABC123"; got != want {
		t.Errorf("album id: got %q want %q", got, want)
	}

	*albumFlag = "https://photos.google.com/share/XYZ"
	if got, want := startURL(), "https://photos.google.com/share/XYZ"; got != want {
		t.Errorf("album url: got %q want %q", got, want)
	}
}

func TestIsIgnoredDLFile(t *testing.T) {
	for _, n := range []string{".lastdone", ".lastdone.bak", "debug.png", "debug.html", manifestName, manifestName + ".tmp"} {
		if !isIgnoredDLFile(n) {
			t.Errorf("isIgnoredDLFile(%q) = false, want true", n)
		}
	}
	for _, n := range []string{"photo.jpg", "movie.mp4", "x.crdownload", ".other"} {
		if isIgnoredDLFile(n) {
			t.Errorf("isIgnoredDLFile(%q) = true, want false", n)
		}
	}
}

func TestUniqueDest(t *testing.T) {
	dir := t.TempDir()

	// No existing file: the plain name is used.
	if got, want := uniqueDest(dir, "IMG_0001.jpg", "ITEMID"), filepath.Join(dir, "IMG_0001.jpg"); got != want {
		t.Errorf("uniqueDest (no collision) = %q, want %q", got, want)
	}

	// Existing file: disambiguate with the item id before the extension.
	if err := os.WriteFile(filepath.Join(dir, "IMG_0001.jpg"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if got, want := uniqueDest(dir, "IMG_0001.jpg", "ITEMID"), filepath.Join(dir, "IMG_0001_ITEMID.jpg"); got != want {
		t.Errorf("uniqueDest (collision) = %q, want %q", got, want)
	}

	// File with no extension still works.
	if err := os.WriteFile(filepath.Join(dir, "noext"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if got, want := uniqueDest(dir, "noext", "ID2"), filepath.Join(dir, "noext_ID2"); got != want {
		t.Errorf("uniqueDest (no ext) = %q, want %q", got, want)
	}
}

func TestParseChromeFlag(t *testing.T) {
	cases := []struct {
		in   string
		name string
		val  interface{}
	}{
		{"--no-sandbox", "no-sandbox", true},
		{"--disable-dev-shm-usage", "disable-dev-shm-usage", true},
		{"--headless=new", "headless", "new"},
		{"no-sandbox", "no-sandbox", true},
		{"-single-dash", "single-dash", true},
		{"--remote-debugging-port=9222", "remote-debugging-port", "9222"},
		{"  --window-size=1280,1024  ", "window-size", "1280,1024"},
		{"", "", nil},
		{"--", "", nil},
	}
	for _, c := range cases {
		name, val := parseChromeFlag(c.in)
		if name != c.name || val != c.val {
			t.Errorf("parseChromeFlag(%q) = (%q, %v), want (%q, %v)", c.in, name, val, c.name, c.val)
		}
	}
}

func TestTimeAttr(t *testing.T) {
	if got := timeAttr(time.Time{}, false); got != "unknown" {
		t.Errorf("timeAttr(unknown) = %q, want %q", got, "unknown")
	}
	tm := time.Date(2024, 3, 14, 12, 8, 27, 0, time.UTC)
	if got, want := timeAttr(tm, true), "2024-03-14T12:08:27Z"; got != want {
		t.Errorf("timeAttr = %q, want %q", got, want)
	}
}

func TestParseLevel(t *testing.T) {
	tests := map[string]string{
		"debug":   "DEBUG",
		"DEBUG":   "DEBUG",
		" info ":  "INFO",
		"warn":    "WARN",
		"warning": "WARN",
		"error":   "ERROR",
		"fatal":   "ERROR",
		"bogus":   "INFO",
	}
	for in, want := range tests {
		if got := parseLevel(in).String(); got != want {
			t.Errorf("parseLevel(%q) = %q, want %q", in, got, want)
		}
	}
}
