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
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// recordingDispatcher captures every event the watcher dispatches
// for later assertion. Thread-safe.
type recordingDispatcher struct {
	mu     sync.Mutex
	events []TriageEvent
	// notify is closed after the first Dispatch — tests wait on
	// it to avoid racing the informer's async delivery.
	firstOnce sync.Once
	first     chan struct{}
}

func newRecordingDispatcher() *recordingDispatcher {
	return &recordingDispatcher{first: make(chan struct{})}
}

func (r *recordingDispatcher) Dispatch(_ context.Context, ev TriageEvent) {
	r.mu.Lock()
	r.events = append(r.events, ev)
	r.mu.Unlock()
	r.firstOnce.Do(func() { close(r.first) })
}

func (r *recordingDispatcher) snapshot() []TriageEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]TriageEvent, len(r.events))
	copy(out, r.events)
	return out
}

func TestWatcher_DispatchesEventsFromInformer(t *testing.T) {
	t.Parallel()
	// Seed the fake clientset with an existing Event; the informer's
	// initial list will surface it via AddFunc.
	seed := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "checkout-svc.evt",
			Namespace: "checkout",
		},
		Reason:  "CrashLoopBackOff",
		Message: "Back-off restarting failed container",
		InvolvedObject: corev1.ObjectReference{
			Kind:      "Pod",
			Name:      "checkout-svc-7b9d-x4kzq",
			Namespace: "checkout",
			UID:       "abc-123",
		},
		FirstTimestamp: metav1.Time{Time: time.Now().Add(-2 * time.Minute)},
		LastTimestamp:  metav1.Time{Time: time.Now()},
		Count:          3,
	}
	client := fake.NewClientset(seed)

	disp := newRecordingDispatcher()
	w := newWatcher(client, disp, "prod-us-central1", 0)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	// Wait for the informer's cache to sync + first dispatch.
	select {
	case <-disp.first:
	case <-ctx.Done():
		t.Fatal("no dispatch within timeout")
	}
	cancel()
	<-done // let Run finish cleanly

	events := disp.snapshot()
	if len(events) == 0 {
		t.Fatal("expected at least one dispatched event")
	}
	got := events[0]
	if got.Key.Reason != "CrashLoopBackOff" {
		t.Errorf("Reason = %q, want CrashLoopBackOff", got.Key.Reason)
	}
	if got.Key.UID != "abc-123" {
		t.Errorf("UID = %q, want abc-123", got.Key.UID)
	}
	if got.Namespace != "checkout" {
		t.Errorf("Namespace = %q, want checkout", got.Namespace)
	}
	if got.Count != 3 {
		t.Errorf("Count = %d, want 3", got.Count)
	}
}

func TestToTriageEvent_PopulatesAllFields(t *testing.T) {
	t.Parallel()
	// Direct unit test for the Event → TriageEvent conversion so
	// we don't need the informer running to cover edge cases.
	ev := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test.evt",
			Namespace: "default",
			Labels:    map[string]string{"team": "checkout"},
		},
		Reason:  "OOMKilled",
		Message: "Container was OOMKilled",
		InvolvedObject: corev1.ObjectReference{
			Kind:      "Pod",
			Name:      "test-pod",
			Namespace: "default",
			UID:       "uid-oom",
			FieldPath: "spec.containers{app}",
		},
		Source: corev1.EventSource{
			Host: "node-1",
		},
		FirstTimestamp: metav1.Time{Time: time.Unix(1000, 0)},
		LastTimestamp:  metav1.Time{Time: time.Unix(2000, 0)},
		Count:          7,
	}
	got := toTriageEvent(ev)
	if got.Key.Reason != "OOMKilled" {
		t.Errorf("Reason = %q", got.Key.Reason)
	}
	if got.Key.UID != "uid-oom" {
		t.Errorf("UID = %q", got.Key.UID)
	}
	if got.Namespace != "default" {
		t.Errorf("Namespace = %q", got.Namespace)
	}
	if got.KindOfObject != "Pod" {
		t.Errorf("KindOfObject = %q", got.KindOfObject)
	}
	if got.Container != "spec.containers{app}" {
		t.Errorf("Container = %q", got.Container)
	}
	if got.Node != "node-1" {
		t.Errorf("Node = %q", got.Node)
	}
	if got.Labels["team"] != "checkout" {
		t.Errorf("Labels[team] = %q", got.Labels["team"])
	}
	if got.Count != 7 {
		t.Errorf("Count = %d", got.Count)
	}
	if got.FirstSeen.Unix() != 1000 || got.LastSeen.Unix() != 2000 {
		t.Errorf("Timestamps: FirstSeen=%d LastSeen=%d", got.FirstSeen.Unix(), got.LastSeen.Unix())
	}
}

func TestTruncateMessage_LongPayload(t *testing.T) {
	t.Parallel()
	long := make([]byte, 3000)
	for i := range long {
		long[i] = 'x'
	}
	got := truncateMessage(string(long))
	if len(got) > 2200 { // 2048 + " [truncated by ...]" suffix ~ 30 chars
		t.Errorf("truncated len = %d, expected <= ~2100", len(got))
	}
	if !containsSubstr(got, "truncated by k8s-event-watcher") {
		t.Errorf("truncation marker missing from truncated output")
	}
}

func TestTruncateMessage_ShortPayloadUnchanged(t *testing.T) {
	t.Parallel()
	msg := "small message"
	got := truncateMessage(msg)
	if got != msg {
		t.Errorf("short message should pass through unchanged; got %q", got)
	}
}

func containsSubstr(s, sub string) bool {
	return len(s) >= len(sub) && findSubstr(s, sub) >= 0
}

func findSubstr(s, sub string) int {
	if len(sub) == 0 {
		return 0
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
