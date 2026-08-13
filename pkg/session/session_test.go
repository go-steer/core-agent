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

package session_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-steer/core-agent/v2/pkg/session"
	"github.com/go-steer/core-agent/v2/pkg/transcript"
)

// pkg/session is the deprecated former name of pkg/transcript (#492).
// It has no logic of its own, which is exactly why it was at 0%
// coverage — and also why it's worth a test: the contract this package
// exists to keep is that an external consumer still importing the old
// path gets bit-identical behaviour, including reading transcripts
// written through the new path. An alias quietly changed to a defined
// type, or a forwarder pointed at a different function, breaks that
// without breaking the build here.
//
// The test lives in package session_test (external) deliberately —
// that's the position a downstream consumer occupies.

// These helpers take the pkg/transcript spelling only. Passing the
// pkg/session spelling compiles while the two are aliases (=); two
// distinct defined types with the same underlying struct are NOT
// assignable to each other, so a `=` quietly dropped from any of the
// alias declarations breaks the build here.
func acceptsTranscript(transcript.Transcript) {}
func acceptsMessages([]transcript.Message)    {}
func acceptsUsage(transcript.Usage)           {}

func TestAliasesAreTheSameTypes(t *testing.T) {
	t.Parallel()
	acceptsTranscript(session.Transcript{})
	acceptsUsage(session.Usage{})
	// A consumer's []session.Message must be usable where the new
	// type's slice is wanted, without a copy loop.
	acceptsMessages([]session.Message{{Role: "user", Text: "hello"}})
}

func TestConstantsAndSentinelMatch(t *testing.T) {
	t.Parallel()
	if session.SchemaVersion != transcript.SchemaVersion {
		t.Errorf("SchemaVersion = %d, want %d", session.SchemaVersion, transcript.SchemaVersion)
	}
	if session.SessionsDirName != transcript.SessionsDirName {
		t.Errorf("SessionsDirName = %q, want %q", session.SessionsDirName, transcript.SessionsDirName)
	}
	// Sentinel identity, not just equal text: a consumer doing
	// errors.Is(err, session.ErrNoProject) against an error produced
	// by pkg/transcript has to match.
	if !errors.Is(transcript.ErrNoProject, session.ErrNoProject) {
		t.Error("session.ErrNoProject is not the same sentinel as transcript.ErrNoProject")
	}
}

func TestSave_WritesTheSameFileAsTranscriptSave(t *testing.T) {
	t.Parallel()

	started := time.Date(2026, 8, 12, 9, 30, 0, 0, time.UTC)
	tr := session.Transcript{
		StartedAt: started,
		EndedAt:   started.Add(time.Minute),
		Model:     "gemini-3.5-flash",
		Messages:  []session.Message{{Role: "user", Text: "hello"}},
		Usage:     session.Usage{InputTokens: 10, OutputTokens: 3},
	}

	viaShim := t.TempDir()
	shimPath, err := session.Save(viaShim, tr)
	if err != nil {
		t.Fatalf("session.Save: %v", err)
	}
	viaNew := t.TempDir()
	newPath, err := transcript.Save(viaNew, tr)
	if err != nil {
		t.Fatalf("transcript.Save: %v", err)
	}

	if filepath.Base(shimPath) != filepath.Base(newPath) {
		t.Errorf("file names differ: %q vs %q", filepath.Base(shimPath), filepath.Base(newPath))
	}
	// Both must land under .agents/sessions/, not the agentsDir root.
	if got := filepath.Base(filepath.Dir(shimPath)); got != session.SessionsDirName {
		t.Errorf("parent dir = %q, want %q", got, session.SessionsDirName)
	}

	shimBody, err := os.ReadFile(shimPath)
	if err != nil {
		t.Fatalf("read shim output: %v", err)
	}
	newBody, err := os.ReadFile(newPath)
	if err != nil {
		t.Fatalf("read transcript output: %v", err)
	}
	if string(shimBody) != string(newBody) {
		t.Errorf("on-disk bytes differ:\nshim:       %s\ntranscript: %s", shimBody, newBody)
	}

	// And the bytes are the documented schema-v1 shape, not just
	// self-consistently wrong.
	var decoded map[string]any
	if err := json.Unmarshal(shimBody, &decoded); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	if v, _ := decoded["version"].(float64); int(v) != session.SchemaVersion {
		t.Errorf("version field = %v, want %d", decoded["version"], session.SchemaVersion)
	}
}

func TestSave_StampsTheSchemaVersion(t *testing.T) {
	t.Parallel()
	// A zero Version must be filled in, or a transcript written
	// through the deprecated path would be unreadable by a loader
	// that switches on the version field.
	path, err := session.Save(t.TempDir(), session.Transcript{StartedAt: time.Now()})
	if err != nil {
		t.Fatalf("session.Save: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var decoded struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.Version != session.SchemaVersion {
		t.Errorf("version = %d, want %d", decoded.Version, session.SchemaVersion)
	}
}

func TestSave_NoAgentsDirIsANoOp(t *testing.T) {
	t.Parallel()
	// Running outside a project isn't an error — there's just nowhere
	// to archive to. Callers rely on the empty path to decide whether
	// to print "saved to ...".
	path, err := session.Save("", session.Transcript{StartedAt: time.Now()})
	if err != nil {
		t.Errorf("err = %v, want nil", err)
	}
	if path != "" {
		t.Errorf("path = %q, want empty", path)
	}
}
