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
	"bytes"
	"encoding/csv"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const (
	urlA = "https://photos.google.com/photo/idA"
	urlB = "https://photos.google.com/photo/idB"
	urlC = "https://photos.google.com/photo/idC"
)

func TestManifestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	m, err := LoadManifest(dir)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}

	date := time.Date(2024, 3, 14, 12, 8, 27, 0, time.UTC)
	m.Seen(urlA, date, true)
	m.Done(urlA, []string{"/dl/idA/a.jpg"}, 123)
	m.Seen(urlB, time.Time{}, false)
	m.Skipped(urlB)
	m.Errored(urlC, errors.New("boom"))

	if err := m.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	m2, err := LoadManifest(dir)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}

	itA, ok := m2.Get("idA")
	if !ok || itA.Status != StatusDone || itA.Bytes != 123 || !itA.HaveDate || !itA.Date.Equal(date) {
		t.Errorf("idA reloaded wrong: %+v (ok=%v)", itA, ok)
	}
	if len(itA.Files) != 1 || itA.Files[0] != "/dl/idA/a.jpg" {
		t.Errorf("idA files wrong: %v", itA.Files)
	}
	if itB, ok := m2.Get("idB"); !ok || itB.Status != StatusSkipped {
		t.Errorf("idB reloaded wrong: %+v (ok=%v)", itB, ok)
	}
	if itC, ok := m2.Get("idC"); !ok || itC.Status != StatusErrored || itC.Attempts != 1 || itC.Err != "boom" {
		t.Errorf("idC reloaded wrong: %+v (ok=%v)", itC, ok)
	}
}

func TestManifestMigratesLastdone(t *testing.T) {
	dir := t.TempDir()
	// No manifest yet, but a legacy .lastdone exists.
	if err := markDone(dir, urlA); err != nil {
		t.Fatalf("markDone: %v", err)
	}
	m, err := LoadManifest(dir)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	it, ok := m.Get("idA")
	if !ok || it.Status != StatusDone || it.URL != urlA {
		t.Errorf("migrated item wrong: %+v (ok=%v)", it, ok)
	}
	if !m.IsDone(urlA) {
		t.Error("IsDone(urlA) = false, want true after migration")
	}
}

func TestManifestCounts(t *testing.T) {
	m, _ := LoadManifest(t.TempDir())
	m.Done(urlA, nil, 0)
	m.Skipped(urlB)
	m.Errored(urlC, errors.New("x"))
	m.Seen("https://photos.google.com/photo/idD", time.Time{}, false) // pending

	c := m.Counts()
	if c[StatusDone] != 1 || c[StatusSkipped] != 1 || c[StatusErrored] != 1 || c[StatusPending] != 1 {
		t.Errorf("counts = %v", c)
	}
}

func TestManifestSaveAtomicAndIgnored(t *testing.T) {
	dir := t.TempDir()
	m, _ := LoadManifest(dir)
	m.Done(urlA, []string{"a.jpg"}, 1)
	if err := m.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, manifestName)); err != nil {
		t.Fatalf("manifest file missing: %v", err)
	}
	// No leftover temp file.
	if _, err := os.Stat(filepath.Join(dir, manifestName+".tmp")); !os.IsNotExist(err) {
		t.Errorf("temp file should not remain: %v", err)
	}
	// The download loop must never treat the manifest as a downloaded photo.
	if !isIgnoredDLFile(manifestName) || !isIgnoredDLFile(manifestName+".tmp") {
		t.Error("manifest files should be ignored by the download dir scan")
	}
}

func TestManifestCSV(t *testing.T) {
	m, _ := LoadManifest(t.TempDir())
	date := time.Date(2024, 3, 14, 0, 0, 0, 0, time.UTC)
	m.Seen(urlA, date, true)
	m.Done(urlA, []string{"/dl/2024/03/a.jpg", "/dl/2024/03/a.mp4"}, 42)

	var buf bytes.Buffer
	if err := m.WriteCSV(&buf); err != nil {
		t.Fatalf("WriteCSV: %v", err)
	}
	rows, err := csv.NewReader(&buf).ReadAll()
	if err != nil {
		t.Fatalf("parse CSV: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("want header + 1 row, got %d rows", len(rows))
	}
	if rows[0][0] != "id" || rows[0][6] != "files" {
		t.Errorf("header wrong: %v", rows[0])
	}
	got := rows[1]
	if got[0] != "idA" || got[2] != string(StatusDone) || got[4] != "42" {
		t.Errorf("row wrong: %v", got)
	}
	if got[6] != "/dl/2024/03/a.jpg;/dl/2024/03/a.mp4" {
		t.Errorf("files column wrong: %q", got[6])
	}
}
