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

package background

import (
	"fmt"
	"strings"
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/attach"
)

// TestPushAlert_WakesParent is the #780 regression pin. Alerts are
// PULLED at the top of a parent turn (Agent.Run calls
// PrependPendingAlerts), so an alert pushed after the parent's last
// turn lands in a queue nothing is scheduled to read. spawn_agent
// {wait: true} tells the model "result will be pushed" on timeout;
// the wake is the only thing that makes that true.
//
// Fails on pre-#780 code: pushAlert enqueued and returned, so the
// channel select below hit its default branch.
func TestPushAlert_WakesParent(t *testing.T) {
	t.Parallel()
	mgr, _ := newFakeManager(t)
	parent := newTestParent(t, mgr)

	select {
	case <-parent.WakeRequested():
		t.Fatalf("a wake was already pending before any alert — the rest of this test would pass for the wrong reason")
	default:
	}

	mgr.pushAlert(Alert{From: "kid", Kind: "completed", Text: "found three bad nodes"})

	select {
	case <-parent.WakeRequested():
	default:
		t.Errorf("no wake after pushAlert — a detached subagent's result sits in the queue until something else happens to start a turn, which for a parent that has already wrapped up is never")
	}
}

// TestPushAlert_WakePublishesEvent pins the RequestWake-not-fire
// choice. Nothing in this package reaches the wire, so for an operator
// attached over SSE this frame is the only evidence that a child's
// report moved the parent. The inbox path deliberately fires the
// signal without the event because it publishes its own frame first
// (#802); there is no such frame here.
func TestPushAlert_WakePublishesEvent(t *testing.T) {
	t.Parallel()
	mgr, _ := newFakeManager(t)
	parent := newTestParent(t, mgr)

	var kinds []string
	parent.SetOperatorEventEmitter(func(kind string, _ any) {
		kinds = append(kinds, kind)
	})

	mgr.pushAlert(Alert{From: "kid", Kind: "alert", Text: "halfway"})

	var sawWake bool
	for _, k := range kinds {
		if k == attach.EventWake {
			sawWake = true
		}
	}
	if !sawWake {
		t.Errorf("emitted events = %v, want one %q — an attached operator has no other way to see that a child report woke the parent",
			kinds, attach.EventWake)
	}
}

// TestPushAlert_WakeReachesObservers pins the fan-out half: the wake
// goes through the shared signal, so an operator surface holding a
// SubscribeWake subscription sees it alongside the driver rather than
// racing the driver for it (#813).
func TestPushAlert_WakeReachesObservers(t *testing.T) {
	t.Parallel()
	mgr, _ := newFakeManager(t)
	parent := newTestParent(t, mgr)

	observer, unsub := parent.SubscribeWake()
	defer unsub()

	mgr.pushAlert(Alert{From: "kid", Kind: "failed", Text: "boom"})

	select {
	case <-observer:
	default:
		t.Errorf("observer subscription saw no wake")
	}
	select {
	case <-parent.WakeRequested():
	default:
		t.Errorf("driver subscription saw no wake — the observer consumed it")
	}
}

// TestPushAlert_NoParentDoesNotPanic covers the window the manager is
// built in: it exists before the parent does (NewManager, then the
// spawn tools, then agent.New stamps the back-reference), and library
// consumers may never wire one at all.
func TestPushAlert_NoParentDoesNotPanic(t *testing.T) {
	t.Parallel()
	mgr, _ := newFakeManager(t)
	mgr.pushAlert(Alert{From: "kid", Kind: "alert", Text: "nobody home"})

	if got := mgr.PrependPendingAlerts("p"); !strings.Contains(got, "nobody home") {
		t.Errorf("PrependPendingAlerts = %q, want the alert still queued", got)
	}
}

// TestPrependPendingAlerts_ReportsDrops is the second half of #780.
// The buffer drops the OLDEST alerts when it fills, and pre-fix the
// only trace was a line on the daemon's stderr — so the model read a
// report list with no way to know it was truncated, which is the one
// reader that could have asked a subagent to say it again.
//
// Fails on pre-#780 code: the block contained the surviving reports
// and nothing else.
func TestPrependPendingAlerts_ReportsDrops(t *testing.T) {
	t.Parallel()
	mgr, _ := newFakeManager(t) // WithAlertBuffer(16)

	const pushed = 20
	for i := range pushed {
		mgr.pushAlert(Alert{From: "kid", Kind: "alert", Text: fmt.Sprintf("report-%02d", i)})
	}

	got := mgr.PrependPendingAlerts("carry on")

	if !strings.Contains(got, "4 earlier background reports were discarded") {
		t.Errorf("drain = %q\nwant a notice naming 4 discarded reports", got)
	}
	// The notice must lead: the dropped alerts are older than every
	// survivor, so anywhere else puts the block out of arrival order.
	head := strings.SplitN(got, "\n", 3)
	if len(head) < 2 || !strings.Contains(head[1], "discarded") {
		t.Errorf("drain first entry = %q, want the drop notice", got)
	}
	if !strings.Contains(got, "report-19") {
		t.Errorf("drain = %q, want the newest report kept", got)
	}
	if strings.Contains(got, "report-00") {
		t.Errorf("drain = %q, want the oldest report evicted (drop-oldest, not drop-newest)", got)
	}
	if !strings.HasSuffix(got, "carry on") {
		t.Errorf("drain = %q, want the caller's prompt preserved at the tail", got)
	}

	// Read-and-clear: a later turn must not be told about the same
	// drops again.
	if again := mgr.PrependPendingAlerts("carry on"); again != "carry on" {
		t.Errorf("second drain = %q, want the prompt unchanged — the drop notice repeated", again)
	}
}

// TestPrependPendingAlerts_DropNoticeSurvivesRivalDrain covers the
// consumer split. Alerts() and PrependPendingAlerts drain the same
// channel, so a library consumer reading Alerts() can empty it before
// the parent's turn starts. The drop count is not in the channel, so
// the notice still has to reach the model — with a queue that now
// looks empty, it is the ONLY evidence anything was lost.
func TestPrependPendingAlerts_DropNoticeSurvivesRivalDrain(t *testing.T) {
	t.Parallel()
	mgr, _ := newFakeManager(t) // WithAlertBuffer(16)

	for i := range 17 {
		mgr.pushAlert(Alert{From: "kid", Kind: "alert", Text: fmt.Sprintf("report-%02d", i)})
	}
	// A rival consumer takes everything that survived.
	for range 16 {
		<-mgr.Alerts()
	}

	got := mgr.PrependPendingAlerts("carry on")
	if !strings.Contains(got, "1 earlier background report was discarded") {
		t.Errorf("drain = %q\nwant a singular notice for the one discarded report", got)
	}
}
