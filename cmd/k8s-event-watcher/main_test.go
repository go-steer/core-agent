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
	"context"
	"strings"
	"testing"
	"time"
)

// TestDispatcher_EndToEnd wires filter + dedup + injector against
// the fake daemon and verifies the whole pipeline. Two events for
// the same key → one CreateSession, one Inject.
func TestDispatcher_EndToEnd_DuplicateSuppressed(t *testing.T) {
	t.Parallel()
	base, injects, _ := newFakeDaemon(t)
	inj, err := newInjector(injectorConfig{
		daemonURL:      base,
		bearerToken:    "tok_test",
		assertedCaller: "sre@example.com",
	})
	if err != nil {
		t.Fatalf("newInjector: %v", err)
	}
	dedup, _ := newDedupCache(5*time.Minute, "")
	disp := &dispatcher{
		filter:    newFilter(newFilterConfig(nil, nil, nil, 0)),
		dedup:     dedup,
		injector:  inj,
		metrics:   newMetrics(),
		cluster:   "test-cluster",
		mode:      "per-incident",
		targetSid: "",
		dryRun:    false,
	}
	ev := TriageEvent{
		Key:       EventKey{UID: "u1", Reason: "CrashLoopBackOff"},
		Namespace: "default",
		Name:      "pod-1",
		Message:   "flapping",
		Count:     1,
	}
	ctx := context.Background()
	disp.Dispatch(ctx, ev)
	disp.Dispatch(ctx, ev) // duplicate within window — should be suppressed
	if len(*injects) != 1 {
		t.Errorf("expected 1 inject (second is dedup-suppressed); got %d", len(*injects))
	}
}

func TestDispatcher_EndToEnd_FilterRejects(t *testing.T) {
	t.Parallel()
	base, injects, _ := newFakeDaemon(t)
	inj, _ := newInjector(injectorConfig{daemonURL: base, bearerToken: "t", assertedCaller: "a@b"})
	dedup, _ := newDedupCache(5*time.Minute, "")
	disp := &dispatcher{
		filter:   newFilter(newFilterConfig([]string{"CrashLoopBackOff"}, nil, nil, 0)),
		dedup:    dedup,
		injector: inj,
		metrics:  newMetrics(),
		cluster:  "test-cluster",
		mode:     "per-incident",
	}
	// Reason not in allow-list → dispatcher must silently drop.
	disp.Dispatch(context.Background(), TriageEvent{
		Key:       EventKey{UID: "u1", Reason: "SomeOtherReason"},
		Namespace: "default",
	})
	if len(*injects) != 0 {
		t.Errorf("filter should have blocked; got %d injects", len(*injects))
	}
}

func TestDispatcher_EndToEnd_SharedMode(t *testing.T) {
	t.Parallel()
	// Shared mode: no per-incident CreateSession call; every
	// event injects to the pre-configured target session.
	base, injects, _ := newFakeDaemon(t)
	inj, _ := newInjector(injectorConfig{daemonURL: base, bearerToken: "t", assertedCaller: "a@b"})
	dedup, _ := newDedupCache(5*time.Minute, "")
	disp := &dispatcher{
		filter:    newFilter(newFilterConfig(nil, nil, nil, 0)),
		dedup:     dedup,
		injector:  inj,
		metrics:   newMetrics(),
		cluster:   "test-cluster",
		mode:      "shared",
		targetSid: "sess-shared",
	}
	disp.Dispatch(context.Background(), TriageEvent{
		Key:       EventKey{UID: "u1", Reason: "CrashLoopBackOff"},
		Namespace: "default",
		Name:      "pod-1",
	})
	if len(*injects) != 1 {
		t.Fatalf("expected 1 inject in shared mode; got %d", len(*injects))
	}
	// Inject URL should have carried the target SessionID.
	if !strings.Contains((*injects)[0], "k8s-event") {
		t.Errorf("captured body missing kind marker: %q", (*injects)[0])
	}
}

// -- parseFlags / validate coverage ------------------------------------

func TestParseFlags_MissingRequiredIsError(t *testing.T) {
	t.Parallel()
	// Neither --daemon-url nor --token-env, not --dry-run.
	f, err := parseFlags(nil)
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if err := f.validate(); err == nil {
		t.Error("validate should reject missing --daemon-url")
	}
}

func TestParseFlags_DryRunAllowsMissing(t *testing.T) {
	t.Parallel()
	f, err := parseFlags([]string{"--dry-run"})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if err := f.validate(); err != nil {
		t.Errorf("--dry-run should skip daemon-url + token-env requirements; got %v", err)
	}
}

func TestParseFlags_PerIncidentRequiresOwner(t *testing.T) {
	t.Parallel()
	f, err := parseFlags([]string{"--daemon-url", "http://x", "--token-env", "TOK"})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if err := f.validate(); err == nil {
		t.Error("per-incident mode should require --owner")
	}
}

func TestParseFlags_SharedRequiresTargetSession(t *testing.T) {
	t.Parallel()
	f, err := parseFlags([]string{"--daemon-url", "http://x", "--token-env", "TOK", "--mode", "shared"})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if err := f.validate(); err == nil {
		t.Error("shared mode should require --target-session")
	}
}

func TestSplitCSV(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"a", []string{"a"}},
		{"a,b,c", []string{"a", "b", "c"}},
		{"a, b , c", []string{"a", "b", "c"}},
		{"a,,b", []string{"a", "b"}},
	}
	for _, c := range cases {
		got := splitCSV(c.in)
		if !stringSlicesEqual(got, c.want) {
			t.Errorf("splitCSV(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
