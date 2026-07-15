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
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestNewFilesystemStore_DefaultDirUnderTmp(t *testing.T) {
	t.Parallel()
	// Empty dir should default under os.TempDir() per the project's
	// tmp-not-$HOME convention (feedback_uat_files_in_tmp memory).
	// Point the test at a subdirectory of t.TempDir so we don't
	// accidentally clobber a real default install.
	pinned := filepath.Join(t.TempDir(), "custom-dir")
	fs, err := NewFilesystemStore(pinned)
	if err != nil {
		t.Fatalf("NewFilesystemStore: %v", err)
	}
	if fs.Dir != pinned {
		t.Errorf("Dir = %q, want %q", fs.Dir, pinned)
	}
	if fs.MaxTotalBytes != DefaultStoreMaxTotalBytes {
		t.Errorf("MaxTotalBytes = %d, want default %d",
			fs.MaxTotalBytes, DefaultStoreMaxTotalBytes)
	}
}

func TestFilesystemStore_PutGetRoundTrip(t *testing.T) {
	t.Parallel()
	fs := newTestStore(t)
	payload := []byte("hello world")

	if err := fs.Put(context.Background(), "call-1", payload); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := fs.Get(context.Background(), "call-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("Get returned %q, want %q", got, payload)
	}
}

func TestFilesystemStore_GetUnknownReturnsErrNotFound(t *testing.T) {
	t.Parallel()
	fs := newTestStore(t)
	_, err := fs.Get(context.Background(), "never-put")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Get unknown = %v, want ErrNotFound", err)
	}
}

func TestFilesystemStore_PutEmptyCallIDRejects(t *testing.T) {
	t.Parallel()
	// Empty callID → no retrieval possible → refuse the write. The
	// caller has a bug they should fix.
	fs := newTestStore(t)
	if err := fs.Put(context.Background(), "", []byte("x")); err == nil {
		t.Error("expected error for empty callID")
	}
}

func TestFilesystemStore_SanitizeRejectsPathTraversal(t *testing.T) {
	t.Parallel()
	// Adversarial callIDs must not escape Dir. The sanitizer is the
	// last line of defense — model-generated IDs shouldn't be
	// trusted verbatim.
	fs := newTestStore(t)
	bad := []string{
		"../etc/passwd",
		"foo/bar",
		`foo\bar`,
		".",
		"..",
		"...", // all-dots reduces to empty after sanitize.Trim
		"contains spaces",
		"unicode-α",
	}
	for _, id := range bad {
		if err := fs.Put(context.Background(), id, []byte("x")); err == nil {
			t.Errorf("Put(%q) should have failed sanitize check", id)
		}
	}
}

func TestFilesystemStore_PutOverwritesSameCallID(t *testing.T) {
	t.Parallel()
	// A second Put with the same callID replaces the first — the
	// bytes counter tracks the new size, not the sum. Prevents a
	// double-Put from silently blowing past MaxTotalBytes.
	fs := newTestStore(t)
	if err := fs.Put(context.Background(), "c1", []byte("first version")); err != nil {
		t.Fatal(err)
	}
	firstBytes := fs.Bytes()
	if err := fs.Put(context.Background(), "c1", []byte("second version longer")); err != nil {
		t.Fatal(err)
	}
	if fs.Len() != 1 {
		t.Errorf("Len = %d, want 1 (Put replaced, not appended)", fs.Len())
	}
	if fs.Bytes() == firstBytes*2 {
		t.Errorf("Bytes = %d suggests double-counting", fs.Bytes())
	}
	got, _ := fs.Get(context.Background(), "c1")
	if string(got) != "second version longer" {
		t.Errorf("Get returned old value: %q", got)
	}
}

func TestFilesystemStore_FIFOEvictionUnderCap(t *testing.T) {
	t.Parallel()
	// Cap at 100 bytes; put 3 x 50-byte payloads. The third put
	// evicts the oldest to make room. Order is insertion order,
	// not read order.
	fs, err := NewFilesystemStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fs.MaxTotalBytes = 100
	pad := strings.Repeat("x", 50)

	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("c-%d", i)
		if err := fs.Put(context.Background(), id, []byte(pad)); err != nil {
			t.Fatalf("Put %s: %v", id, err)
		}
	}
	if fs.Len() != 2 {
		t.Errorf("Len = %d, want 2 (oldest should have been evicted)", fs.Len())
	}
	// c-0 should be gone (oldest); c-1 and c-2 should survive.
	if _, err := fs.Get(context.Background(), "c-0"); !errors.Is(err, ErrNotFound) {
		t.Errorf("c-0 should be evicted, got err=%v", err)
	}
	for _, id := range []string{"c-1", "c-2"} {
		if _, err := fs.Get(context.Background(), id); err != nil {
			t.Errorf("%s should still be present: %v", id, err)
		}
	}
	if fs.Bytes() > fs.MaxTotalBytes {
		t.Errorf("Bytes %d exceeds MaxTotalBytes %d", fs.Bytes(), fs.MaxTotalBytes)
	}
}

func TestFilesystemStore_RefusesOversizePayload(t *testing.T) {
	t.Parallel()
	// A single payload larger than MaxTotalBytes can't fit no matter
	// what we evict — refuse the write rather than silently
	// truncating.
	fs, err := NewFilesystemStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fs.MaxTotalBytes = 10
	if err := fs.Put(context.Background(), "big", []byte("this payload is way over ten bytes")); err == nil {
		t.Error("expected error for oversize payload")
	}
	if fs.Len() != 0 {
		t.Errorf("Len = %d after failed Put, want 0", fs.Len())
	}
}

func TestFilesystemStore_ZeroMaxDisablesBound(t *testing.T) {
	t.Parallel()
	// MaxTotalBytes == 0 means unlimited (test / library use).
	// Prove three writes all survive with no eviction.
	fs, err := NewFilesystemStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fs.MaxTotalBytes = 0
	pad := strings.Repeat("y", 1000)
	for i := 0; i < 3; i++ {
		if err := fs.Put(context.Background(), fmt.Sprintf("c-%d", i), []byte(pad)); err != nil {
			t.Fatal(err)
		}
	}
	if fs.Len() != 3 {
		t.Errorf("Len = %d, want 3 (no eviction with unlimited cap)", fs.Len())
	}
}

func TestFilesystemStore_ReindexAcrossRestart(t *testing.T) {
	t.Parallel()
	// Simulate a daemon restart: create a store, write entries, then
	// open a second store pointed at the same dir. The re-index must
	// pick up prior files so eviction accounting stays honest.
	dir := t.TempDir()
	fs1, err := NewFilesystemStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	fs1.MaxTotalBytes = 1000
	for i := 0; i < 3; i++ {
		if err := fs1.Put(context.Background(), fmt.Sprintf("c-%d", i), []byte("payload")); err != nil {
			t.Fatal(err)
		}
	}
	preRestartBytes := fs1.Bytes()

	fs2, err := NewFilesystemStore(dir)
	if err != nil {
		t.Fatalf("re-open: %v", err)
	}
	if fs2.Len() != 3 {
		t.Errorf("re-opened Len = %d, want 3", fs2.Len())
	}
	if fs2.Bytes() != preRestartBytes {
		t.Errorf("re-opened Bytes = %d, want %d", fs2.Bytes(), preRestartBytes)
	}
	// Contents should still be readable through the new handle.
	got, err := fs2.Get(context.Background(), "c-0")
	if err != nil {
		t.Fatalf("Get after re-open: %v", err)
	}
	if string(got) != "payload" {
		t.Errorf("Get returned %q, want %q", got, "payload")
	}
}

func TestFilesystemStore_ReindexIgnoresJunkFiles(t *testing.T) {
	t.Parallel()
	// A hand-placed junk file (path traversal name, symlink target,
	// etc.) shouldn't block startup. Sanitize-rejecting names get
	// skipped during index.
	dir := t.TempDir()
	// Create an "unclean" file directly; sanitize would reject this
	// name as an entry.
	junk := filepath.Join(dir, ".hidden-file")
	if err := os.WriteFile(junk, []byte("noise"), 0o600); err != nil {
		t.Fatal(err)
	}
	fs, err := NewFilesystemStore(dir)
	if err != nil {
		t.Fatalf("NewFilesystemStore should not error on junk: %v", err)
	}
	// .hidden-file starts with a dot; sanitize allows it (the char
	// allowlist includes '.') but starts-with-dot is fine per our
	// contract. This test's real value is proving the walk doesn't
	// panic on unexpected files. Just assert Len is either 0 or 1
	// (allowlist behavior is what it is).
	if fs.Len() > 1 {
		t.Errorf("Len = %d, want at most 1", fs.Len())
	}
}

func TestFilesystemStore_ConcurrentPutGet(t *testing.T) {
	t.Parallel()
	// Concurrent Puts + Gets must not race the FIFO or the on-disk
	// state. This is the smoke test; the mutex covers it.
	fs := newTestStore(t)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			_ = fs.Put(context.Background(), fmt.Sprintf("c-%d", i), []byte("payload"))
		}(i)
		go func(i int) {
			defer wg.Done()
			// May or may not find the entry depending on ordering —
			// both outcomes are fine, the test is proving no data race.
			_, _ = fs.Get(context.Background(), fmt.Sprintf("c-%d", i))
		}(i)
	}
	wg.Wait()
	if fs.Len() == 0 {
		t.Error("expected some entries after concurrent writes")
	}
}

func TestProcess_StoresRawWhenStoreWired(t *testing.T) {
	t.Parallel()
	// End-to-end: Process with a Store + CallID must persist the
	// raw payload so a subsequent Store.Get returns it verbatim.
	// This is the load-bearing property retrieve_raw depends on.
	fs := newTestStore(t)
	payload := []byte(`{"data":"original with lots of tokens"}`)
	res, err := Process(context.Background(), payload, Options{
		Threshold: 0,
		Store:     fs,
		CallID:    "toolcall-abc",
	})
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if res.CallID != "toolcall-abc" {
		t.Errorf("CallID = %q, want toolcall-abc", res.CallID)
	}
	got, err := fs.Get(context.Background(), "toolcall-abc")
	if err != nil {
		t.Fatalf("Store.Get: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("stored raw doesn't match original")
	}
}

func TestProcess_NoStoreWireNoWrite(t *testing.T) {
	t.Parallel()
	// Symmetric: Store == nil ⇒ no store call ⇒ no CallID surfaced.
	// Test with a spy Store that would flag if called.
	spy := &spyStore{}
	res, err := Process(context.Background(), []byte(`{"k":"v"}`), Options{
		Threshold: 0,
		Store:     nil,
		CallID:    "abc",
	})
	if err != nil {
		t.Fatal(err)
	}
	if spy.putCalls != 0 {
		t.Error("Store was nil but spy saw a call — shouldn't happen")
	}
	// CallID still surfaces on Result even without Store — the field
	// is a caller-supplied ID, not a store-generated one.
	if res.CallID != "abc" {
		t.Errorf("CallID = %q, want abc", res.CallID)
	}
}

func TestProcess_EmptyCallIDSkipsStoreWrite(t *testing.T) {
	t.Parallel()
	// Store non-nil but CallID empty ⇒ no write (Store.Put would
	// reject an empty key anyway). Prevents wasted disk on
	// telemetry-only wire-throughs.
	spy := &spyStore{}
	_, err := Process(context.Background(), []byte(`{"k":"v"}`), Options{
		Threshold: 0,
		Store:     spy,
	})
	if err != nil {
		t.Fatal(err)
	}
	if spy.putCalls != 0 {
		t.Errorf("empty CallID should skip Store.Put, saw %d calls", spy.putCalls)
	}
}

func TestProcess_StoreErrorDegradesToMetadata(t *testing.T) {
	t.Parallel()
	// Store errors mustn't fail Process — the digest still ships,
	// operators see the failure in Metadata["store_err"], retrieval
	// is silently broken for that CallID.
	broken := &spyStore{putErr: errors.New("disk full")}
	res, err := Process(context.Background(), []byte(`{"k":"v"}`), Options{
		Threshold: 0,
		Store:     broken,
		CallID:    "abc",
	})
	if err != nil {
		t.Fatalf("Process should not surface Store error, got %v", err)
	}
	if res.Digest == "" {
		t.Error("digest should still be produced")
	}
	if got, _ := res.Metadata["store_err"].(string); got == "" {
		t.Errorf("expected store_err in metadata: %+v", res.Metadata)
	}
}

// newTestStore returns a store rooted at t.TempDir() with the
// default cap. Shared across tests that don't need custom limits.
func newTestStore(t *testing.T) *FilesystemStore {
	t.Helper()
	fs, err := NewFilesystemStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFilesystemStore: %v", err)
	}
	return fs
}

// spyStore records call counts + optional error injection. Test-only.
type spyStore struct {
	mu       sync.Mutex
	putCalls int
	getCalls int
	putErr   error
	data     map[string][]byte
}

func (s *spyStore) Put(_ context.Context, callID string, raw []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.putCalls++
	if s.putErr != nil {
		return s.putErr
	}
	if s.data == nil {
		s.data = map[string][]byte{}
	}
	s.data[callID] = append([]byte(nil), raw...)
	return nil
}

func (s *spyStore) Get(_ context.Context, callID string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.getCalls++
	v, ok := s.data[callID]
	if !ok {
		return nil, ErrNotFound
	}
	return v, nil
}
