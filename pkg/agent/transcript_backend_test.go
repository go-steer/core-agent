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

package agent

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"

	"github.com/go-steer/core-agent/v3/pkg/eventlog"
	"github.com/go-steer/core-agent/v3/pkg/statestore/memory"
)

// TestRun_OverATranscriptBackend drives one turn through an Agent whose
// Handle was built by eventlog.OpenTranscript rather than eventlog.Open.
// The ADK runner appends through Handle.Service, so this is the proof
// that the agent loop needs nothing from the SQL overlay: the turn's
// events reach the transcript with seqs and Stream.Since reads them.
func TestRun_OverATranscriptBackend(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h, err := eventlog.OpenTranscript(ctx, memory.New(), sqlite.Open(filepath.Join(t.TempDir(), "local.db")))
	if err != nil {
		t.Fatalf("OpenTranscript: %v", err)
	}
	defer func() { _ = h.Close() }()

	a, err := New(oneShotLLM{}, WithEventLog(h))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var sawDone bool
	for ev, err := range a.Run(ctx, "hi") {
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if ev != nil && ev.Content != nil && len(ev.Content.Parts) > 0 && ev.Content.Parts[0].Text == "done" {
			sawDone = true
		}
	}
	if !sawDone {
		t.Fatal("the model's reply never surfaced from Run")
	}

	var authors []string
	var last int64
	for e, err := range h.Stream.Since(ctx, 0, eventlog.ForSession(a.AppName(), a.UserID(), a.SessionID())) {
		if err != nil {
			t.Fatalf("Since: %v", err)
		}
		if e.Seq <= last {
			t.Fatalf("seq %d after %d", e.Seq, last)
		}
		last = e.Seq
		authors = append(authors, e.Event.Author)
	}
	if len(authors) < 2 {
		t.Fatalf("transcript holds %d events after a turn (%v); want at least the user turn and the model reply", len(authors), authors)
	}
	if authors[0] != "user" {
		t.Fatalf("first transcript event author = %q, want user", authors[0])
	}
}
