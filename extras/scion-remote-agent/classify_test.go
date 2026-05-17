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

package scionremote

import (
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

func TestPreferStructuredPayload_RecognisedKind(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	ev, ok := PreferStructuredPayload(hubclient.CloudLogEntry{
		Timestamp: now,
		Message:   "should be ignored when JSONPayload wins",
		JSONPayload: map[string]interface{}{
			"kind": "alert",
			"text": "pod restarted 5 times",
		},
	})
	if !ok {
		t.Fatal("expected ok=true for recognised JSON payload")
	}
	if ev.Kind != "alert" {
		t.Errorf("Kind = %q, want alert", ev.Kind)
	}
	if ev.Text != "pod restarted 5 times" {
		t.Errorf("Text = %q, want %q", ev.Text, "pod restarted 5 times")
	}
	if !ev.Timestamp.Equal(now) {
		t.Errorf("Timestamp = %v, want %v", ev.Timestamp, now)
	}
}

func TestPreferStructuredPayload_FallsBackToStringPrefix(t *testing.T) {
	t.Parallel()
	ev, ok := PreferStructuredPayload(hubclient.CloudLogEntry{
		Message: "[REPORT_ALERT] something noteworthy happened",
		// No JSONPayload.
	})
	if !ok {
		t.Fatal("expected ok=true for string-prefix fallback")
	}
	if ev.Kind != "alert" {
		t.Errorf("Kind = %q, want alert", ev.Kind)
	}
	if ev.Text != "something noteworthy happened" {
		t.Errorf("Text = %q, want trimmed text", ev.Text)
	}
}

func TestPreferStructuredPayload_DropsUnrecognised(t *testing.T) {
	t.Parallel()
	_, ok := PreferStructuredPayload(hubclient.CloudLogEntry{
		Message: "just a regular log line",
	})
	if ok {
		t.Errorf("expected ok=false for plain log line")
	}
}

func TestStringPrefix_RecognisesAllPrefixes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		msg      string
		wantKind string
		wantText string
	}{
		{"[REPORT_ALERT] alert text", "alert", "alert text"},
		{"[REPORT_COMPLETED] done text", "completed", "done text"},
		{"[REPORT_FAILED] err text", "failed", "err text"},
	}
	for _, tc := range cases {
		ev, ok := StringPrefix(hubclient.CloudLogEntry{Message: tc.msg})
		if !ok {
			t.Errorf("%q should match a prefix", tc.msg)
			continue
		}
		if ev.Kind != tc.wantKind {
			t.Errorf("%q: Kind = %q, want %q", tc.msg, ev.Kind, tc.wantKind)
		}
		if ev.Text != tc.wantText {
			t.Errorf("%q: Text = %q, want %q", tc.msg, ev.Text, tc.wantText)
		}
	}
}

func TestStringPrefix_IgnoresUnrecognised(t *testing.T) {
	t.Parallel()
	_, ok := StringPrefix(hubclient.CloudLogEntry{Message: "no prefix here"})
	if ok {
		t.Errorf("expected ok=false for non-prefixed message")
	}
}

func TestClassifyJSONPayload_IgnoresUnknownKind(t *testing.T) {
	t.Parallel()
	_, ok := classifyJSONPayload(hubclient.CloudLogEntry{
		JSONPayload: map[string]interface{}{
			"kind": "debug", // not one of alert/completed/failed
			"text": "...",
		},
	})
	if ok {
		t.Errorf("expected ok=false for unknown kind")
	}
}

func TestClassifyJSONPayload_NilPayload(t *testing.T) {
	t.Parallel()
	_, ok := classifyJSONPayload(hubclient.CloudLogEntry{})
	if ok {
		t.Errorf("expected ok=false for missing payload")
	}
}

func TestVerbose_EmitsEveryEntry(t *testing.T) {
	t.Parallel()
	ev, ok := Verbose(hubclient.CloudLogEntry{Message: "literally anything"})
	if !ok {
		t.Errorf("Verbose should always return ok=true")
	}
	if ev.Kind != "log" {
		t.Errorf("Kind = %q, want log", ev.Kind)
	}
	if ev.Text != "literally anything" {
		t.Errorf("Text = %q, want passthrough", ev.Text)
	}
}
