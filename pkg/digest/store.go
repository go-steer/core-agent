// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package digest

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// ErrNotFound is returned by Store.Get when a callID has no entry.
// Callers surface this to the model via retrieve_raw as a clear
// "unknown call_id" error rather than a generic 500.
var ErrNotFound = errors.New("digest: store entry not found")

// Store is the CCR backing for raw payloads. The retrieve_raw
// built-in tool (follow-up PR) is the model-facing consumer; Process
// is the writer. Implementations must be safe for concurrent use.
type Store interface {
	// Put records raw under callID, overwriting any prior entry.
	// Callers that pass an empty callID get a synthetic error — the
	// key is what makes retrieval possible; storing anonymously is
	// a bug at the call site.
	Put(ctx context.Context, callID string, raw []byte) error

	// Get returns the raw payload previously Put under callID, or
	// ErrNotFound when the key is unknown or was evicted.
	Get(ctx context.Context, callID string) ([]byte, error)
}

// FilesystemStore is a file-per-callID Store rooted at Dir. Defaults
// to a bounded LRU/FIFO eviction policy so long-running sessions
// don't accumulate indefinitely. Safe for concurrent use.
//
// Storage layout: <Dir>/<sanitizedCallID>. The sanitizer prevents
// path traversal (../foo) and unsafe characters — callIDs are opaque
// from the caller's perspective, but tool-call IDs originate from
// the model and shouldn't be trusted verbatim.
//
// Default Dir per feedback_uat_files_in_tmp memory: under
// os.TempDir(), never $HOME. Callers pin Dir explicitly for
// production wiring; tests and library defaults land in /tmp.
type FilesystemStore struct {
	// Dir is the storage root. Must exist and be writable. Create
	// with NewFilesystemStore rather than a bare struct literal so
	// the directory is validated + created up front.
	Dir string

	// MaxTotalBytes bounds cumulative on-disk usage. When a Put
	// would push total bytes over the limit, oldest entries are
	// evicted (FIFO on insert order) until the new payload fits.
	// Zero disables the bound — useful for tests, not recommended
	// for production.
	MaxTotalBytes int64

	mu    sync.Mutex
	order *list.List               // FIFO of *entry, oldest at Front
	index map[string]*list.Element // callID → order element
	bytes int64                    // cumulative bytes currently on disk
}

// entry is the value type stored in FilesystemStore.order. Tracks
// per-callID bytes so eviction can decrement the running total
// without re-Stating the file.
type entry struct {
	callID string
	bytes  int64
}

// DefaultStoreMaxTotalBytes is the FilesystemStore default cap.
// 100 MiB is generous for a session's tool-call raw payloads while
// staying well under any reasonable tmp partition. Tune per your
// deployment; zero disables the bound.
const DefaultStoreMaxTotalBytes = 100 * 1024 * 1024

// NewFilesystemStore creates or opens a FilesystemStore rooted at
// dir. Empty dir defaults to
// <os.TempDir()>/core-agent-digest — the project's tmp-not-$HOME
// convention (per feedback_uat_files_in_tmp memory).
//
// Existing files under dir are indexed on open so restarts don't
// silently exceed MaxTotalBytes; the FIFO order after re-index is
// deterministic (lexicographic callID) since insertion order isn't
// recoverable from mtime cheaply. Callers that need cross-restart
// LRU should use EventlogStore (follow-up PR) instead.
func NewFilesystemStore(dir string) (*FilesystemStore, error) {
	if dir == "" {
		dir = filepath.Join(os.TempDir(), "core-agent-digest")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("digest: mkdir %q: %w", dir, err)
	}
	fs := &FilesystemStore{
		Dir:           dir,
		MaxTotalBytes: DefaultStoreMaxTotalBytes,
		order:         list.New(),
		index:         make(map[string]*list.Element),
	}
	if err := fs.indexExisting(); err != nil {
		return nil, err
	}
	return fs, nil
}

// indexExisting walks Dir and populates order/index/bytes from files
// already on disk (from a prior process or a manually-populated
// directory). Corrupt entries — files that aren't a valid stored
// callID or whose read errors — are logged to stderr and skipped
// rather than blocking startup.
func (s *FilesystemStore) indexExisting() error {
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		return fmt.Errorf("digest: read dir %q: %w", s.Dir, err)
	}
	for _, de := range entries {
		if de.IsDir() {
			continue
		}
		info, err := de.Info()
		if err != nil {
			continue
		}
		name := de.Name()
		// Reject any name that couldn't have come from sanitize —
		// keeps a hand-placed junk file out of the FIFO.
		if sanitize(name) != name {
			continue
		}
		e := &entry{callID: name, bytes: info.Size()}
		s.index[name] = s.order.PushBack(e)
		s.bytes += e.bytes
	}
	return nil
}

// Put implements Store. Fails fast on empty callID (see interface
// doc); on a duplicate callID, evicts the prior entry's bytes from
// the counter before writing the new one so the running total stays
// honest.
func (s *FilesystemStore) Put(_ context.Context, callID string, raw []byte) error {
	if callID == "" {
		return errors.New("digest: FilesystemStore.Put: empty callID")
	}
	safe := sanitize(callID)
	if safe == "" {
		return fmt.Errorf("digest: FilesystemStore.Put: callID %q reduced to empty after sanitize", callID)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Drop any prior entry for this callID before evicting others —
	// avoids double-counting the same key against the cap.
	if el, ok := s.index[safe]; ok {
		s.bytes -= el.Value.(*entry).bytes
		s.order.Remove(el)
		delete(s.index, safe)
		_ = os.Remove(filepath.Join(s.Dir, safe)) // best-effort; Rename below overwrites
	}

	// Evict oldest entries until the new payload fits under the cap.
	// Skip when MaxTotalBytes == 0 (test / unlimited mode).
	if s.MaxTotalBytes > 0 {
		newTotal := s.bytes + int64(len(raw))
		for newTotal > s.MaxTotalBytes && s.order.Len() > 0 {
			oldest := s.order.Front()
			ev := oldest.Value.(*entry)
			_ = os.Remove(filepath.Join(s.Dir, ev.callID))
			s.bytes -= ev.bytes
			s.order.Remove(oldest)
			delete(s.index, ev.callID)
			newTotal = s.bytes + int64(len(raw))
		}
		// If a single payload exceeds the cap after evicting everything,
		// refuse the write rather than silently truncating.
		if int64(len(raw)) > s.MaxTotalBytes {
			return fmt.Errorf("digest: FilesystemStore.Put: payload %d bytes exceeds MaxTotalBytes %d",
				len(raw), s.MaxTotalBytes)
		}
	}

	path := filepath.Join(s.Dir, safe)
	if err := atomicWriteFile(path, raw); err != nil {
		return fmt.Errorf("digest: FilesystemStore.Put: %w", err)
	}
	e := &entry{callID: safe, bytes: int64(len(raw))}
	s.index[safe] = s.order.PushBack(e)
	s.bytes += e.bytes
	return nil
}

// Get implements Store. Reads directly from disk without touching
// the FIFO order — the store is FIFO on insertion, not LRU on read.
func (s *FilesystemStore) Get(_ context.Context, callID string) ([]byte, error) {
	if callID == "" {
		return nil, ErrNotFound
	}
	safe := sanitize(callID)
	if safe == "" {
		return nil, ErrNotFound
	}
	s.mu.Lock()
	_, ok := s.index[safe]
	s.mu.Unlock()
	if !ok {
		return nil, ErrNotFound
	}
	data, err := os.ReadFile(filepath.Join(s.Dir, safe)) //nolint:gosec // path constructed via sanitize
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Index says present but file's gone — someone deleted
			// out-of-band. Surface as ErrNotFound so callers don't
			// have to double-branch.
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("digest: FilesystemStore.Get: %w", err)
	}
	return data, nil
}

// Len returns the number of entries currently indexed. Test helper;
// callers should not rely on it for production logic.
func (s *FilesystemStore) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.index)
}

// Bytes returns cumulative on-disk usage tracked by the store. Test
// helper.
func (s *FilesystemStore) Bytes() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bytes
}

// sanitize maps a caller-supplied callID to a filesystem-safe name.
// Rejects path separators, parent-dir references, and any character
// that isn't alphanumeric, dash, underscore, or dot. Model-generated
// tool-call IDs are typically UUID-shaped and pass through
// unchanged; adversarial inputs get filtered.
func sanitize(callID string) string {
	if callID == "" || callID == "." || callID == ".." {
		return ""
	}
	if strings.ContainsAny(callID, `/\`) {
		return ""
	}
	// Character allowlist: A-Z a-z 0-9 - _ .
	for i := 0; i < len(callID); i++ {
		c := callID[i]
		switch {
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9':
		case c == '-', c == '_', c == '.':
		default:
			return ""
		}
	}
	// Extra guard: reject names that are just dots.
	if strings.Trim(callID, ".") == "" {
		return ""
	}
	return callID
}

// atomicWriteFile writes data to path via a temp file + rename so
// readers never see a partial write. Mode 0o600 — payloads may
// contain secrets pulled through by MCP tools, no need to expose to
// group/other.
func atomicWriteFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".digest-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	// Defer a cleanup that fires only if we exit before rename;
	// Remove-after-rename is a no-op.
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
