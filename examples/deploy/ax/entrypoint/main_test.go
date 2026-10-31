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
	"crypto/tls"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestEntryMain_RequiresCommand(t *testing.T) {
	t.Parallel()
	code := entryMain(discardLogger(), t.TempDir(), "", nil, nil)
	if code != 2 {
		t.Fatalf("entryMain(no args) = %d, want 2", code)
	}
}

func TestEntryMain_InvalidWaitForEgress(t *testing.T) {
	t.Parallel()
	code := entryMain(discardLogger(), t.TempDir(), "not-a-host-port", []string{"/bin/echo", "hi"}, nil)
	if code != 2 {
		t.Fatalf("entryMain(bad --wait-for-egress) = %d, want 2", code)
	}
}

func TestEntryMain_WritesResultAndDoneMarker_AndSkipsOnSecondBoot(t *testing.T) {
	t.Parallel()
	stateDir := filepath.Join(t.TempDir(), ".core-agent")
	counterFile := filepath.Join(t.TempDir(), "runs")

	script := `count=0; [ -f "$1" ] && count=$(cat "$1"); count=$((count + 1)); echo "$count" > "$1"; echo "turn-output-$count"`
	args := []string{"/bin/sh", "-c", script, "sh", counterFile}

	if code := entryMain(discardLogger(), stateDir, "", args, nil); code != 0 {
		t.Fatalf("first boot exit code = %d, want 0", code)
	}

	gotResult, err := os.ReadFile(filepath.Join(stateDir, "result.txt"))
	if err != nil {
		t.Fatalf("read result.txt: %v", err)
	}
	if strings.TrimSpace(string(gotResult)) != "turn-output-1" {
		t.Errorf("result.txt = %q, want %q", string(gotResult), "turn-output-1\n")
	}
	if _, err := os.Stat(filepath.Join(stateDir, "done")); err != nil {
		t.Fatalf("done marker missing after successful turn: %v", err)
	}

	// Second boot with the same stateDir must see the done marker and
	// skip re-running the child command.
	if code := entryMain(discardLogger(), stateDir, "", args, nil); code != 0 {
		t.Fatalf("second boot exit code = %d, want 0", code)
	}
	runs, err := os.ReadFile(counterFile)
	if err != nil {
		t.Fatalf("read counterFile: %v", err)
	}
	if strings.TrimSpace(string(runs)) != "1" {
		t.Errorf("command ran %s times across two boots, want 1", strings.TrimSpace(string(runs)))
	}
}

func TestEntryMain_FailedCommandLeavesNoDoneMarker(t *testing.T) {
	t.Parallel()
	stateDir := filepath.Join(t.TempDir(), ".core-agent")
	args := []string{"/bin/sh", "-c", "echo partial-output; exit 7"}

	code := entryMain(discardLogger(), stateDir, "", args, nil)
	if code != 7 {
		t.Fatalf("entryMain exit code = %d, want 7", code)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "done")); !os.IsNotExist(err) {
		t.Errorf("done marker must not be written when command fails, stat err = %v", err)
	}
}

func TestEntryMain_WaitForEgressProbesTLS(t *testing.T) {
	t.Parallel()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("url.Parse: %v", err)
	}
	tlsCfg := &tls.Config{InsecureSkipVerify: true} // #nosec G402 -- test server uses ephemeral self-signed cert
	stateDir := filepath.Join(t.TempDir(), ".core-agent")
	code := entryMain(discardLogger(), stateDir, u.Host, []string{"/bin/echo", "egress-ok"}, tlsCfg)
	if code != 0 {
		t.Fatalf("entryMain with TLS egress server = %d, want 0", code)
	}
	got, err := os.ReadFile(filepath.Join(stateDir, "result.txt"))
	if err != nil {
		t.Fatalf("read result.txt: %v", err)
	}
	if strings.TrimSpace(string(got)) != "egress-ok" {
		t.Errorf("result.txt = %q, want %q", string(got), "egress-ok")
	}
}
