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

package attach

import (
	"bytes"
	"encoding/json"
	"os"
	"slices"
	"testing"
	"time"
)

// Conformance tests — the runtime types marshal to the exact byte
// shape shipped in testdata/conformance/. These fixtures are the
// canonical source for downstream consumers (mast-web, core-tui) that
// mirror them into their own harnesses; a struct-tag rename or field
// reorder here fails visibly instead of silently drifting.

func TestConformance_CapabilitiesV1_4_0(t *testing.T) {
	t.Parallel()
	caps := Capabilities{
		ProtocolVersion: "1.4.0",
		EventTypes: []string{
			EventStatusUpdate,
			EventUsageUpdate,
			EventInbox,
			EventTurnComplete,
			EventTurnError,
			"stream-chunk",
			"tool-call",
			"tool-result",
		},
		Server: "core-agent/2.8.0-dev",
		Features: map[string]bool{
			featureMultiSession: true,
			featurePermsStream:  true,
			featureMCP:          true,
			featureSpecialists:  true,
			featureCrossDaemon:  false,
			featureInterrupt:    true,
			featureCostCeiling:  false,
			featureObserverMode: false,
		},
		SlashCommands: []string{"btw", "compact", "done", "replan", "subagent"},
		Agent: &AgentIdentity{
			Name:        "core-agent",
			Version:     "v2.8.0-dev",
			Description: "Autonomous coding assistant for the core-agent repository.",
			Model:       "gemini-3.1-pro",
			URL:         "https://agents.example.com/core-agent",
		},
		CallerID: "alice@example.com",
	}
	assertMatchesConformanceFixture(t,
		"testdata/conformance/capabilities-v1.4.0.json",
		caps)
}

func TestConformance_StatusUpdateWithCapabilitiesV1_4_0(t *testing.T) {
	t.Parallel()
	pct := 42
	update := StatusUpdate{
		Model:      "gemini-3.1-pro",
		Provider:   "vertex",
		PermMode:   "default",
		TurnState:  TurnStateIdle,
		ContextPct: &pct,
		// Merge shape — a hot capability update mid-session. Only the
		// fields that changed populate; consumers merge into their
		// cached snapshot.
		Capabilities: &Capabilities{
			ProtocolVersion: "1.4.0",
			EventTypes: []string{
				EventStatusUpdate,
				EventUsageUpdate,
				EventInbox,
				EventTurnComplete,
				EventTurnError,
				"stream-chunk",
				"tool-call",
				"tool-result",
			},
			Features: map[string]bool{
				featureMCP:         true,
				featureSpecialists: true,
			},
			SlashCommands: []string{"btw", "compact", "done", "replan", "subagent"},
		},
	}
	assertMatchesConformanceFixture(t,
		"testdata/conformance/status-update-with-capabilities-v1.4.0.json",
		update)
}

func TestConformance_WakeV1_7_0(t *testing.T) {
	t.Parallel()
	// #802. Timestamp carries sub-second precision and an explicit
	// zone so the fixture can't be read as sanctioning a fixed
	// second-granularity layout — it's RFC 3339, parse it.
	at, err := time.Parse(time.RFC3339Nano, "2026-08-19T14:32:05.117Z")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	assertMatchesConformanceFixture(t,
		"testdata/conformance/wake-v1.7.0.json",
		WakeEvent{At: at})
}

// TestSupportedEventTypes_AdvertisesWake pins the advertisement, which
// is the half of a new event type that is easy to forget and invisible
// when missing: a consumer that feature-detects on the capabilities
// frame's event_types would keep taking its pre-1.7.0 path against a
// server that does emit the frame.
func TestSupportedEventTypes_AdvertisesWake(t *testing.T) {
	t.Parallel()
	if !slices.Contains(supportedEventTypes, EventWake) {
		t.Errorf("supportedEventTypes = %v, missing %q", supportedEventTypes, EventWake)
	}
}

// assertMatchesConformanceFixture marshals v, canonicalizes both the
// output and the fixture (Go's encoder sorts map keys alphabetically;
// the fixture is hand-written), and fails with a readable diff on
// mismatch. Same pattern the agent-card wire-format test uses.
func assertMatchesConformanceFixture(t *testing.T, path string, v any) {
	t.Helper()
	got, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %s: %v", path, err)
	}
	if !bytes.Equal(canonicalizeJSON(t, got), canonicalizeJSON(t, want)) {
		t.Fatalf("wire format drifted from %s\n--- got ---\n%s\n--- want ---\n%s", path, got, want)
	}
}
