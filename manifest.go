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
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The manifest is a durable, per-item record of everything the tool has seen.
// It supersedes the single .lastdone sentinel while staying dependency-free (no
// SQLite): the whole set is held in memory and written back atomically. It is
// the foundation for resume-by-id, status reporting, integrity checks and
// (later) safe concurrency.
//
// For now .lastdone remains the authoritative resume sentinel (see firstNav);
// the manifest is written alongside it and records the richer per-item data.

const (
	manifestName    = ".manifest.json"
	manifestVersion = 1
)

// ItemStatus is the lifecycle state of an item.
type ItemStatus string

const (
	StatusPending ItemStatus = "pending"
	StatusDone    ItemStatus = "done"
	StatusErrored ItemStatus = "errored"
	StatusSkipped ItemStatus = "skipped"
)

// Item is one Google Photos item tracked across runs.
type Item struct {
	ID        string     `json:"id"`              // Google Photos item id (from the URL)
	URL       string     `json:"url"`             // full photo URL (the .lastdone sentinel value)
	Status    ItemStatus `json:"status"`          // pending|done|errored|skipped
	Date      time.Time  `json:"date"`            // capture date (zero if unknown; see HaveDate)
	HaveDate  bool       `json:"have_date"`       // whether Date is meaningful
	Files     []string   `json:"files,omitempty"` // destination paths of the downloaded file(s)
	Bytes     int64      `json:"bytes,omitempty"` // total bytes downloaded
	Attempts  int        `json:"attempts"`        // number of download attempts so far
	Err       string     `json:"err,omitempty"`   // last error message, if errored
	UpdatedAt time.Time  `json:"updated_at"`      // last time this record changed
}

// Manifest is a thread-safe, JSON-backed store of items, keyed by item id.
type Manifest struct {
	mu    sync.Mutex
	path  string
	items map[string]*Item
	order []string // ids in first-seen order, for stable output
}

// manifestFile is the on-disk envelope.
type manifestFile struct {
	Version int     `json:"version"`
	Items   []*Item `json:"items"`
}

// LoadManifest opens (or initialises) the manifest in dlDir. If no manifest file
// exists yet but a legacy .lastdone is present, that URL is migrated in as a
// single done item so resume keeps working on first upgrade.
func LoadManifest(dlDir string) (*Manifest, error) {
	m := &Manifest{
		path:  filepath.Join(dlDir, manifestName),
		items: map[string]*Item{},
	}
	data, err := os.ReadFile(m.path)
	switch {
	case err == nil:
		var mf manifestFile
		if err := json.Unmarshal(data, &mf); err != nil {
			return nil, fmt.Errorf("parsing %s: %w", m.path, err)
		}
		for _, it := range mf.Items {
			if it == nil || it.ID == "" {
				continue
			}
			if _, dup := m.items[it.ID]; dup {
				continue
			}
			m.items[it.ID] = it
			m.order = append(m.order, it.ID)
		}
	case os.IsNotExist(err):
		if last, _ := getLastDone(dlDir); last != "" {
			it := &Item{ID: photoIDFromURL(last), URL: last, Status: StatusDone, UpdatedAt: time.Now()}
			m.items[it.ID] = it
			m.order = append(m.order, it.ID)
		}
	default:
		return nil, err
	}
	return m, nil
}

// getOrCreate returns the item for url, creating a pending one if needed.
// The caller must hold m.mu.
func (m *Manifest) getOrCreate(url string) *Item {
	id := photoIDFromURL(url)
	it, ok := m.items[id]
	if !ok {
		it = &Item{ID: id, URL: url, Status: StatusPending}
		m.items[id] = it
		m.order = append(m.order, id)
	}
	return it
}

// Get returns the item with the given id.
func (m *Manifest) Get(id string) (*Item, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	it, ok := m.items[id]
	return it, ok
}

// Seen records that an item exists (status pending unless already further
// along) and updates its known capture date.
func (m *Manifest) Seen(url string, date time.Time, haveDate bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	it := m.getOrCreate(url)
	it.URL = url
	if haveDate {
		it.Date, it.HaveDate = date, true
	}
	it.UpdatedAt = time.Now()
}

// Done marks url's item completed with the given destination files and size.
func (m *Manifest) Done(url string, files []string, bytes int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	it := m.getOrCreate(url)
	it.Status = StatusDone
	it.Files = files
	it.Bytes = bytes
	it.Err = ""
	it.UpdatedAt = time.Now()
}

// Errored records a failed download attempt.
func (m *Manifest) Errored(url string, cause error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	it := m.getOrCreate(url)
	it.Status = StatusErrored
	it.Attempts++
	if cause != nil {
		it.Err = cause.Error()
	}
	it.UpdatedAt = time.Now()
}

// Skipped marks an item intentionally skipped (e.g. out of the date range).
func (m *Manifest) Skipped(url string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	it := m.getOrCreate(url)
	if it.Status != StatusDone {
		it.Status = StatusSkipped
	}
	it.UpdatedAt = time.Now()
}

// IsDone reports whether the item for url is already completed (resume helper).
func (m *Manifest) IsDone(url string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	it, ok := m.items[photoIDFromURL(url)]
	return ok && it.Status == StatusDone
}

// Counts summarises the manifest by status.
func (m *Manifest) Counts() map[ItemStatus]int {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[ItemStatus]int{}
	for _, it := range m.items {
		out[it.Status]++
	}
	return out
}

// snapshot returns the items in stable order. The caller must hold m.mu.
func (m *Manifest) snapshot() []*Item {
	items := make([]*Item, 0, len(m.order))
	for _, id := range m.order {
		if it := m.items[id]; it != nil {
			items = append(items, it)
		}
	}
	return items
}

// Save writes the manifest to disk atomically (temp file + rename).
func (m *Manifest) Save() error {
	m.mu.Lock()
	mf := manifestFile{Version: manifestVersion, Items: m.snapshot()}
	path := m.path
	m.mu.Unlock()

	data, err := json.MarshalIndent(mf, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// WriteCSV exports the inventory as CSV (id,url,status,date,bytes,attempts,files,err).
func (m *Manifest) WriteCSV(w io.Writer) error {
	m.mu.Lock()
	items := m.snapshot()
	m.mu.Unlock()

	cw := csv.NewWriter(w)
	if err := cw.Write([]string{"id", "url", "status", "date", "bytes", "attempts", "files", "err"}); err != nil {
		return err
	}
	for _, it := range items {
		date := ""
		if it.HaveDate {
			date = it.Date.Format(time.RFC3339)
		}
		row := []string{
			it.ID,
			it.URL,
			string(it.Status),
			date,
			strconv.FormatInt(it.Bytes, 10),
			strconv.Itoa(it.Attempts),
			strings.Join(it.Files, ";"),
			it.Err,
		}
		if err := cw.Write(row); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}
