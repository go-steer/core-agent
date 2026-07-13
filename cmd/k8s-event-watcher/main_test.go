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
	"context"
	"log"
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

// TestDispatcher_LogsFireOnSuccess verifies the "fire <reason>
// pod=<ns>/<name> → sid=..." success line surfaces (#212 part 2).
// Before this landed the successful-inject case was silent, forcing
// operators to correlate client-go informer warnings with daemon
// session-list dumps to infer whether the watcher fired.
func TestDispatcher_LogsFireOnSuccess(t *testing.T) {
	// NOT t.Parallel — captureLogOutput swaps global log.Default()
	// writer; concurrent tests would race on it.
	logBuf, restoreLog := captureLogOutput(t)
	defer restoreLog()

	base, injects, _ := newFakeDaemon(t)
	inj, err := newInjector(injectorConfig{daemonURL: base, bearerToken: "tok", assertedCaller: "sre@example.com"})
	if err != nil {
		t.Fatalf("newInjector: %v", err)
	}
	dedup, _ := newDedupCache(5*time.Minute, "")
	disp := &dispatcher{
		filter:   newFilter(newFilterConfig(nil, nil, nil, 0)),
		dedup:    dedup,
		injector: inj,
		metrics:  newMetrics(),
		cluster:  "test-cluster",
		mode:     "per-incident",
	}
	disp.Dispatch(context.Background(), TriageEvent{
		Key:       EventKey{UID: "u42", Reason: "ImagePullBackOff"},
		Namespace: "online-boutique",
		Name:      "paymentservice-abc123",
	})
	if len(*injects) != 1 {
		t.Fatalf("expected 1 inject; got %d", len(*injects))
	}
	log := logBuf.String()
	// Every field the operator actually needs — reason, target,
	// resulting sid, mode — must appear.
	wantSubstrings := []string{"fire ImagePullBackOff", "pod=online-boutique/paymentservice-abc123", "sid=", "mode=per-incident"}
	for _, s := range wantSubstrings {
		if !strings.Contains(log, s) {
			t.Errorf("fire log missing %q; got: %q", s, log)
		}
	}
}

// TestDispatcher_LogsDedupOnSuppress verifies the "dedup <reason>
// pod=<ns>/<name> (count=N, window active)" line fires when the
// same key repeats within the dedup window. Makes suppressed
// events visible so operators can distinguish "watcher missed
// the event" from "watcher saw + correctly deduped".
func TestDispatcher_LogsDedupOnSuppress(t *testing.T) {
	// NOT t.Parallel — see captureLogOutput note.
	logBuf, restoreLog := captureLogOutput(t)
	defer restoreLog()

	base, injects, _ := newFakeDaemon(t)
	inj, _ := newInjector(injectorConfig{daemonURL: base, bearerToken: "tok", assertedCaller: "sre@example.com"})
	dedup, _ := newDedupCache(5*time.Minute, "")
	disp := &dispatcher{
		filter:   newFilter(newFilterConfig(nil, nil, nil, 0)),
		dedup:    dedup,
		injector: inj,
		metrics:  newMetrics(),
		cluster:  "test-cluster",
		mode:     "per-incident",
	}
	ev := TriageEvent{
		Key:       EventKey{UID: "u7", Reason: "CrashLoopBackOff"},
		Namespace: "default",
		Name:      "flappy-pod",
	}
	ctx := context.Background()
	// First fire → dispatch → inject (no dedup log)
	disp.Dispatch(ctx, ev)
	// Reset log buffer so we only inspect the second-dispatch output.
	logBuf.Reset()
	// Second fire in the same window → dedup path
	disp.Dispatch(ctx, ev)
	if len(*injects) != 1 {
		t.Fatalf("expected 1 inject (second suppressed); got %d", len(*injects))
	}
	log := logBuf.String()
	wantSubstrings := []string{"dedup CrashLoopBackOff", "pod=default/flappy-pod", "count=2", "window active"}
	for _, s := range wantSubstrings {
		if !strings.Contains(log, s) {
			t.Errorf("dedup log missing %q; got: %q", s, log)
		}
	}
	// Fire log must NOT appear on the suppressed second dispatch.
	if strings.Contains(log, "fire CrashLoopBackOff") {
		t.Errorf("suppressed dispatch should not emit a fire log; got: %q", log)
	}
}

// captureLogOutput swaps log.Default()'s writer for a bytes.Buffer
// and returns (buffer, restore-fn). Used by dispatcher-log tests.
func captureLogOutput(t *testing.T) (*bytes.Buffer, func()) {
	t.Helper()
	buf := &bytes.Buffer{}
	prev := log.Writer()
	prevFlags := log.Flags()
	log.SetOutput(buf)
	// Drop the timestamp prefix so tests can grep for substrings
	// without hitting date-dependent noise.
	log.SetFlags(0)
	return buf, func() {
		log.SetOutput(prev)
		log.SetFlags(prevFlags)
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
