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

package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSave_NoProjectIsNoOp(t *testing.T) {
	t.Parallel()
	path, err := Save("", Transcript{Model: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if path != "" {
		t.Errorf("expected empty path, got %q", path)
	}
}

func TestSave_RoundTripJSON(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	started := time.Date(2026, 5, 2, 12, 30, 0, 0, time.UTC)
	tr := Transcript{
		StartedAt: started,
		Model:     "gemini-3.1-pro-preview",
		Messages: []Message{
			{Role: "user", Text: "hi"},
			{Role: "assistant", Text: "hello"},
		},
		Usage: Usage{Turns: 1, InputTokens: 10, OutputTokens: 5, CostUSD: 0.001},
	}
	path, err := Save(dir, tr)
	if err != nil {
		t.Fatal(err)
	}
	if path == "" {
		t.Fatal("expected non-empty path")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got Transcript
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unparseable transcript: %v\n%s", err, body)
	}
	if got.Version != SchemaVersion {
		t.Errorf("version = %d, want %d", got.Version, SchemaVersion)
	}
	if got.Model != tr.Model || len(got.Messages) != 2 || got.Usage.Turns != 1 {
		t.Errorf("transcript round-trip wrong: %+v", got)
	}
	if got.EndedAt.IsZero() {
		t.Error("EndedAt should be auto-populated when caller leaves it zero")
	}
}

func TestSave_FilenameIsTimestamp(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	started := time.Date(2026, 5, 2, 12, 30, 0, 0, time.UTC)
	path, err := Save(dir, Transcript{StartedAt: started})
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Base(path)
	if !strings.HasPrefix(base, "2026-05-02T12-30-00") {
		t.Errorf("filename %q doesn't include start timestamp", base)
	}
	if !strings.HasSuffix(base, ".json") {
		t.Errorf("filename %q missing .json suffix", base)
	}
}

func TestSave_CreatesSessionsDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if _, err := Save(dir, Transcript{}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, SessionsDirName))
	if err != nil {
		t.Fatalf("sessions dir not created: %v", err)
	}
	if !info.IsDir() {
		t.Error("sessions path is not a directory")
	}
}
