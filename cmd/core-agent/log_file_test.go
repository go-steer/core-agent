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

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// safeBuf is a bytes.Buffer with a mutex so the tee goroutine and
// the test can race-free on read.
type safeBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// waitForContains polls sink for substr up to timeout. The tee
// goroutine writes asynchronously; the test needs to give it time
// to drain the pipe.
func waitForContains(t *testing.T, sink *safeBuf, substr string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if strings.Contains(sink.String(), substr) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %q in sink; got: %q", substr, sink.String())
}

// withSavedStderr restores os.Stderr after the test. teeStderrTo
// swaps it out; callers must not run in parallel.
func withSavedStderr(t *testing.T) {
	t.Helper()
	orig := os.Stderr
	t.Cleanup(func() { os.Stderr = orig })
}

func TestTeeStderrTo_WritesToSinkAndPreservesStderrWrites(t *testing.T) {
	withSavedStderr(t)

	sink := &safeBuf{}
	if err := teeStderrTo(sink); err != nil {
		t.Fatalf("teeStderrTo: %v", err)
	}

	msg := "hello from stderr\n"
	if _, err := os.Stderr.WriteString(msg); err != nil {
		t.Fatalf("stderr write: %v", err)
	}
	waitForContains(t, sink, "hello from stderr", 2*time.Second)
}

func TestInstallLogFileTee_EmptyPathIsNoop(t *testing.T) {
	withSavedStderr(t)
	orig := os.Stderr
	if err := installLogFileTee(""); err != nil {
		t.Fatalf("empty path returned err: %v", err)
	}
	if os.Stderr != orig {
		t.Errorf("empty path unexpectedly swapped os.Stderr")
	}
	if err := installLogFileTee("-"); err != nil {
		t.Fatalf(`"-" returned err: %v`, err)
	}
	if os.Stderr != orig {
		t.Errorf(`"-" unexpectedly swapped os.Stderr`)
	}
}

func TestInstallLogFileTee_AppendsToFile(t *testing.T) {
	withSavedStderr(t)

	dir := t.TempDir()
	path := filepath.Join(dir, "core-agent.log")
	// Pre-seed the file so we can prove O_APPEND semantics rather
	// than O_TRUNC.
	if err := os.WriteFile(path, []byte("seed\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := installLogFileTee(path); err != nil {
		t.Fatalf("installLogFileTee: %v", err)
	}

	msg := "diagnostic line\n"
	if _, err := os.Stderr.WriteString(msg); err != nil {
		t.Fatalf("stderr write: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(path)
		if err == nil && bytes.Contains(b, []byte("diagnostic line")) && bytes.HasPrefix(b, []byte("seed\n")) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	b, _ := os.ReadFile(path)
	t.Fatalf("expected seed+diagnostic in %s, got: %q", path, b)
}

func TestInstallLogFileTee_UnwritablePathIsFatalError(t *testing.T) {
	withSavedStderr(t)

	// A path under a nonexistent directory will fail to open.
	bad := filepath.Join(t.TempDir(), "does-not-exist", "core-agent.log")
	err := installLogFileTee(bad)
	if err == nil {
		t.Fatal("expected error opening path under nonexistent dir")
	}
}
