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

package compose

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	adkmodel "google.golang.org/adk/model"
	"google.golang.org/adk/session"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v2/pkg/agent"
	"github.com/go-steer/core-agent/v2/pkg/attach"
	"github.com/go-steer/core-agent/v2/pkg/auth"
	"github.com/go-steer/core-agent/v2/pkg/eventlog"
	"github.com/go-steer/core-agent/v2/pkg/permissions"
)

const acUser = "alice"
const acSID = "sid-ac"

// seedAC opens a temp eventlog and seeds the (core-agent, alice,
// sid-ac) session with the given events.
func seedAC(t *testing.T, events ...*session.Event) *eventlog.Handle {
	t.Helper()
	ctx := context.Background()
	h, err := eventlog.Open(ctx, sqlite.Open(filepath.Join(t.TempDir(), "s.db")))
	if err != nil {
		t.Fatalf("eventlog.Open: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })
	if _, err := h.Service.Create(ctx, &session.CreateRequest{AppName: "core-agent", UserID: acUser, SessionID: acSID}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	resp, err := h.Service.Get(ctx, &session.GetRequest{AppName: "core-agent", UserID: acUser, SessionID: acSID})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	for _, ev := range events {
		if err := h.Service.AppendEvent(ctx, resp.Session, ev); err != nil {
			t.Fatalf("AppendEvent: %v", err)
		}
	}
	return h
}

func acUserEvent(text string, ts time.Time) *session.Event {
	ev := session.NewEvent("inv-u")
	ev.Author = "user"
	ev.Timestamp = ts
	ev.LLMResponse = adkmodel.LLMResponse{
		Content: &genai.Content{Role: genai.RoleUser, Parts: []*genai.Part{{Text: text}}},
	}
	return ev
}

// acModelEvent stamps ts explicitly: the session service orders
// events by timestamp, so fixtures must be monotonic like real
// histories (a "later" event with an older stamp would reorder).
func acModelEvent(text string, ts time.Time) *session.Event {
	ev := session.NewEvent("inv-m")
	ev.Author = "core_agent"
	ev.Timestamp = ts
	ev.LLMResponse = adkmodel.LLMResponse{
		Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: text}}},
	}
	return ev
}

// acAgent builds an agent bound to the seeded triple without starting
// a wake loop, so the test can observe the inbox deterministically.
func acAgent(t *testing.T, h *eventlog.Handle) *agent.Agent {
	t.Helper()
	ag, err := agent.New(stubLLM{}, agent.WithEventLog(h), agent.WithSession(acUser, acSID))
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	return ag
}

func acDeps(h *eventlog.Handle, freshness time.Duration) SessionFactoryDeps {
	return SessionFactoryDeps{
		DaemonCtx:             context.Background(),
		EventlogHandle:        h,
		AutoContinueEnabled:   true,
		AutoContinueFreshness: freshness,
	}
}

// scanDeps builds full boot-scan deps: real temp eventlog, real ACL
// store on the same DB, registry with the production resumer wired.
// The wake loops started by resumed agents run against stubLLM (their
// turns fail and log; the loop stays alive) — the scan's observable
// contract here is registry membership + the boot-log record.
func scanDeps(t *testing.T, h *eventlog.Handle) SessionFactoryDeps {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	store, err := attach.NewSessionACLStore(context.Background(), h.DB)
	if err != nil {
		t.Fatalf("NewSessionACLStore: %v", err)
	}
	deps := SessionFactoryDeps{
		DaemonCtx:             ctx,
		Model:                 stubLLM{},
		Template:              permissions.New(permissions.Options{}),
		EventlogHandle:        h,
		ACLStore:              store,
		AutoContinueEnabled:   true,
		AutoContinueFreshness: time.Hour,
	}
	deps.Registry = attach.NewSessionRegistryWithStore(store).WithResumer(BuildSessionResumer(deps))
	return deps
}

func putACLRow(t *testing.T, store attach.SessionACLStore, sid string) {
	t.Helper()
	if err := store.Put(context.Background(), attach.SessionACLRow{
		AppName: "core-agent", UserID: acUser, SessionID: sid, Owner: acUser,
	}); err != nil {
		t.Fatalf("ACL Put(%s): %v", sid, err)
	}
}

func seedACSession(t *testing.T, h *eventlog.Handle, sid string, events ...*session.Event) {
	t.Helper()
	ctx := context.Background()
	if _, err := h.Service.Create(ctx, &session.CreateRequest{AppName: "core-agent", UserID: acUser, SessionID: sid}); err != nil {
		t.Fatalf("Create(%s): %v", sid, err)
	}
	resp, err := h.Service.Get(ctx, &session.GetRequest{AppName: "core-agent", UserID: acUser, SessionID: sid})
	if err != nil {
		t.Fatalf("Get(%s): %v", sid, err)
	}
	for _, ev := range events {
		if err := h.Service.AppendEvent(ctx, resp.Session, ev); err != nil {
			t.Fatalf("AppendEvent(%s): %v", sid, err)
		}
	}
}

func openAC(t *testing.T) *eventlog.Handle {
	t.Helper()
	h, err := eventlog.Open(context.Background(), sqlite.Open(filepath.Join(t.TempDir(), "s.db")))
	if err != nil {
		t.Fatalf("eventlog.Open: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return h
}

func TestAutoContinueBootScan_ContinuesInterruptedSkipsCompleted(t *testing.T) {
	t.Parallel()
	h := openAC(t)
	seedACSession(t, h, "sid-hung", acUserEvent("hello?", time.Now().Add(-5*time.Minute)))
	seedACSession(t, h, "sid-done",
		acUserEvent("q", time.Now().Add(-10*time.Minute)),
		acModelEvent("answered", time.Now().Add(-9*time.Minute)))
	deps := scanDeps(t, h)
	putACLRow(t, deps.ACLStore, "sid-hung")
	putACLRow(t, deps.ACLStore, "sid-done")

	AutoContinueBootScan(deps, 10)

	boots, err := h.RecentBoots(context.Background(), time.Now().Add(-time.Minute))
	if err != nil || len(boots) != 1 {
		t.Fatalf("boot log = (%v, %v), want exactly one record", boots, err)
	}
	if len(boots[0].Attempted) != 1 || boots[0].Attempted[0] != "sid-hung" {
		t.Errorf("attempted = %v, want [sid-hung] (completed session must not be touched)", boots[0].Attempted)
	}
}

func TestAutoContinueBootScan_BreakerAndSingleRetry(t *testing.T) {
	t.Parallel()
	t.Run("breaker trips after repeated attempting boots", func(t *testing.T) {
		t.Parallel()
		h := openAC(t)
		seedACSession(t, h, "sid-hung", acUserEvent("hello?", time.Now()))
		deps := scanDeps(t, h)
		putACLRow(t, deps.ACLStore, "sid-hung")
		for i := 0; i < 3; i++ {
			if _, err := h.RecordBoot(context.Background(), time.Now().Add(-time.Duration(i+1)*time.Minute), []string{"other-sid"}); err != nil {
				t.Fatalf("RecordBoot: %v", err)
			}
		}
		AutoContinueBootScan(deps, 10)
		boots, _ := h.RecentBoots(context.Background(), time.Now().Add(-breakerWindow))
		if len(boots) != 3 {
			t.Errorf("boot log has %d records, want 3 — a tripped breaker must stand down without recording an attempt", len(boots))
		}
	})
	t.Run("session attempted in a recent boot is skipped", func(t *testing.T) {
		t.Parallel()
		h := openAC(t)
		seedACSession(t, h, "sid-hung", acUserEvent("hello?", time.Now()))
		deps := scanDeps(t, h)
		putACLRow(t, deps.ACLStore, "sid-hung")
		if _, err := h.RecordBoot(context.Background(), time.Now().Add(-time.Minute), []string{"sid-hung"}); err != nil {
			t.Fatalf("RecordBoot: %v", err)
		}
		AutoContinueBootScan(deps, 10)
		boots, _ := h.RecentBoots(context.Background(), time.Now().Add(-breakerWindow))
		if len(boots) != 2 {
			t.Fatalf("boot log has %d records, want 2", len(boots))
		}
		if len(boots[1].Attempted) != 0 {
			t.Errorf("second boot attempted = %v, want empty (single automatic retry already spent)", boots[1].Attempted)
		}
	})
	t.Run("cumulative cap stops a self-renewing session", func(t *testing.T) {
		t.Parallel()
		h := openAC(t)
		// Fresh interrupted tail every time (a poisoned continuation
		// that makes partial progress before dying renews itself) —
		// only the attempt COUNT can terminate this.
		seedACSession(t, h, "sid-poison", acUserEvent("hello?", time.Now()))
		deps := scanDeps(t, h)
		putACLRow(t, deps.ACLStore, "sid-poison")
		// Three prior attempts spread outside the breaker window but
		// inside the lookback: single-retry and breaker both pass,
		// the cumulative cap must not.
		for i := 0; i < 3; i++ {
			if _, err := h.RecordBoot(context.Background(), time.Now().Add(-time.Duration(15+10*i)*time.Minute), []string{"sid-poison"}); err != nil {
				t.Fatalf("RecordBoot: %v", err)
			}
		}
		AutoContinueBootScan(deps, 10)
		boots, _ := h.RecentBoots(context.Background(), time.Now().Add(-time.Minute))
		if len(boots) != 1 || len(boots[0].Attempted) != 0 {
			t.Errorf("this boot's record = %+v, want empty attempted (cumulative cap reached)", boots)
		}
	})
	t.Run("boot intent is recorded before resumes are driven", func(t *testing.T) {
		t.Parallel()
		h := openAC(t)
		seedACSession(t, h, "sid-hung", acUserEvent("hello?", time.Now()))
		deps := scanDeps(t, h)
		putACLRow(t, deps.ACLStore, "sid-hung")
		// Nil out the registry's resumer path by using a fresh
		// registry with NO resumer: Lookup will fail — but the boot
		// record must exist anyway (write-ahead), listing the intent.
		deps.Registry = attach.NewSessionRegistryWithStore(deps.ACLStore)
		AutoContinueBootScan(deps, 10)
		boots, _ := h.RecentBoots(context.Background(), time.Now().Add(-time.Minute))
		if len(boots) != 1 || len(boots[0].Attempted) != 1 || boots[0].Attempted[0] != "sid-hung" {
			t.Errorf("boot log = %+v, want write-ahead intent [sid-hung] even though the resume failed", boots)
		}
	})
	t.Run("max_per_boot caps oldest-first", func(t *testing.T) {
		t.Parallel()
		h := openAC(t)
		seedACSession(t, h, "sid-older", acUserEvent("hello?", time.Now().Add(-30*time.Minute)))
		seedACSession(t, h, "sid-newer", acUserEvent("hello?", time.Now().Add(-5*time.Minute)))
		deps := scanDeps(t, h)
		putACLRow(t, deps.ACLStore, "sid-older")
		putACLRow(t, deps.ACLStore, "sid-newer")
		AutoContinueBootScan(deps, 1)
		boots, _ := h.RecentBoots(context.Background(), time.Now().Add(-breakerWindow))
		if len(boots) != 1 || len(boots[0].Attempted) != 1 || boots[0].Attempted[0] != "sid-older" {
			t.Errorf("attempted = %+v, want exactly [sid-older]", boots)
		}
	})
}

// countAttempted totals how many times sid appears across all boot
// records — i.e. how deep the per-session cumulative cap has been
// charged. The #575 fleet fix must keep this at 0 for a session no
// daemon ever actually continued (every peer lost the run lock).
func countAttempted(boots []eventlog.BootRecord, sid string) int {
	n := 0
	for _, b := range boots {
		for _, s := range b.Attempted {
			if s == sid {
				n++
			}
		}
	}
	return n
}

// TestAutoContinueBootScan_FleetLockLoserRefundsCap is the #575 defect-B
// accounting gate. A daemon that boot-scans a session another daemon
// already owns (run lock held) made NO real continuation attempt — yet
// the write-ahead record charged one. In a fleet all sharing one
// eventlog, every boot-scanning peer but the lock winner burns an attempt
// this way; after maxAttemptsPerSession such boots the session is capped
// out having never once been continued. The lock-loser must REFUND its
// write-ahead charge.
//
// Fails-first: pre-fix records the candidate unconditionally, so a
// lock-held scan leaves Attempted == [sid] and repeated scans exhaust the
// cap.
func TestAutoContinueBootScan_FleetLockLoserRefundsCap(t *testing.T) {
	t.Parallel()
	h := openAC(t)
	seedACSession(t, h, "sid-hung", acUserEvent("hello?", time.Now()))
	deps := scanDeps(t, h)
	putACLRow(t, deps.ACLStore, "sid-hung")

	// A peer daemon owns the run lock across every scan below.
	lock, err := h.AcquireLock(context.Background(), "core-agent", acUser, "sid-hung")
	if err != nil {
		t.Fatalf("AcquireLock: %v", err)
	}

	// Three lock-losing boots (a three-daemon fleet, or one daemon
	// retrying three times) must not charge the session even once.
	for i := 0; i < 3; i++ {
		AutoContinueBootScan(deps, 10)
	}
	boots, err := h.RecentBoots(context.Background(), time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatalf("RecentBoots: %v", err)
	}
	if got := countAttempted(boots, "sid-hung"); got != 0 {
		t.Fatalf("sid-hung charged %d times across lock-held scans, want 0 — a fleet of lock-losers must not burn the cap", got)
	}

	// Cap intact: once the lock frees, a scan continues the session for
	// real (a fresh attempt is now recorded).
	lock.Release()
	AutoContinueBootScan(deps, 10)
	boots, err = h.RecentBoots(context.Background(), time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatalf("RecentBoots: %v", err)
	}
	if got := countAttempted(boots, "sid-hung"); got != 1 {
		t.Errorf("sid-hung charged %d times after the lock freed, want exactly 1 (the cap was preserved, so the real attempt still lands)", got)
	}
}

// TestAutoContinueRetryLoop_SelfHealsAfterTransientLock is the #575
// defect-B retry gate. A continuation that fails for a TRANSIENT reason
// (here: the run lock briefly held by a peer) is stranded until a reboot
// or a human message, because the boot scan is one-shot per boot. The
// in-lifetime retry loop must self-heal it once the transient condition
// clears.
//
// Fails-first: pre-fix has no retry driver, so nothing re-attempts after
// the lock frees and the poll times out.
func TestAutoContinueRetryLoop_SelfHealsAfterTransientLock(t *testing.T) {
	t.Parallel()
	h := openAC(t)
	seedACSession(t, h, "sid-hung", acUserEvent("hello?", time.Now()))
	deps := scanDeps(t, h)
	putACLRow(t, deps.ACLStore, "sid-hung")

	// Hold the lock so the first ticks can't continue the session (each
	// must refund, per the accounting gate above), then release it.
	lock, err := h.AcquireLock(context.Background(), "core-agent", acUser, "sid-hung")
	if err != nil {
		t.Fatalf("AcquireLock: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	loopDone := make(chan struct{})
	go func() {
		defer close(loopDone)
		AutoContinueRetryLoop(ctx, 20*time.Millisecond, func() { AutoContinueBootScan(deps, 10) })
	}()
	// Honor AutoContinueRetryLoop's contract: cancel THEN join before the
	// eventlog Handle closes. cancel() only signals — a tick already inside
	// AutoContinueBootScan → AcquireLock keeps running, so without the join
	// openAC's LIFO h.Close() cleanup can nil the Handle mid-pass and race
	// (go test -race). This cleanup is registered after openAC's, so LIFO
	// runs it first: loop fully drained before the DB tears down.
	t.Cleanup(func() { cancel(); <-loopDone })

	// Let a few ticks fire against the held lock, then free it.
	time.Sleep(80 * time.Millisecond)
	lock.Release()

	// A later tick must now continue the session. Poll the boot log for
	// the recorded attempt.
	deadline := time.Now().Add(3 * time.Second)
	for {
		boots, err := h.RecentBoots(context.Background(), time.Now().Add(-time.Minute))
		if err != nil {
			t.Fatalf("RecentBoots: %v", err)
		}
		if countAttempted(boots, "sid-hung") >= 1 {
			return // self-healed after the transient lock cleared
		}
		if time.Now().After(deadline) {
			t.Fatal("retry loop never continued the session after the lock freed — no in-lifetime self-heal")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestAutoContinueStartupSession(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	t.Run("interrupted tail queues note and records intent", func(t *testing.T) {
		t.Parallel()
		h := seedAC(t, acUserEvent("hello?", time.Now().Add(-5*time.Minute)))
		ag := acAgent(t, h) // triple = (core-agent, alice, sid-ac) via WithSession
		AutoContinueStartupSession(ctx, h, ag, time.Hour)
		msgs := ag.DrainInbox()
		if len(msgs) != 1 || !strings.Contains(msgs[0], "previous turn did not complete") {
			t.Fatalf("inbox = %v, want the continuation note", msgs)
		}
		boots, _ := h.RecentBoots(ctx, time.Now().Add(-time.Minute))
		if len(boots) != 1 || len(boots[0].Attempted) != 1 || boots[0].Attempted[0] != acSID {
			t.Errorf("boot log = %+v, want write-ahead intent [%s]", boots, acSID)
		}
	})
	t.Run("clean tail burns no attempt", func(t *testing.T) {
		t.Parallel()
		// This trigger fires EVERY boot — recording an attempt for a
		// healthy restart would exhaust the cumulative cap and block
		// a real interruption later.
		h := seedAC(t,
			acUserEvent("q", time.Now().Add(-10*time.Minute)),
			acModelEvent("answered", time.Now().Add(-9*time.Minute)))
		ag := acAgent(t, h)
		AutoContinueStartupSession(ctx, h, ag, time.Hour)
		if msgs := ag.DrainInbox(); len(msgs) != 0 {
			t.Errorf("inbox = %v, want empty for a completed tail", msgs)
		}
		boots, _ := h.RecentBoots(ctx, time.Now().Add(-time.Minute))
		if len(boots) != 0 {
			t.Errorf("boot log = %+v, want no record for a clean boot", boots)
		}
	})
	t.Run("cumulative cap blocks the fourth attempt", func(t *testing.T) {
		t.Parallel()
		h := seedAC(t, acUserEvent("hello?", time.Now()))
		for i := 0; i < 3; i++ {
			if _, err := h.RecordBoot(ctx, time.Now().Add(-time.Duration(15+10*i)*time.Minute), []string{acSID}); err != nil {
				t.Fatalf("RecordBoot: %v", err)
			}
		}
		ag := acAgent(t, h)
		AutoContinueStartupSession(ctx, h, ag, time.Hour)
		if msgs := ag.DrainInbox(); len(msgs) != 0 {
			t.Errorf("inbox = %v, want empty (cumulative cap reached)", msgs)
		}
	})
	t.Run("stale interruption burns no attempt", func(t *testing.T) {
		t.Parallel()
		h := seedAC(t, acUserEvent("hello?", time.Now().Add(-3*time.Hour)))
		ag := acAgent(t, h)
		AutoContinueStartupSession(ctx, h, ag, time.Hour)
		if msgs := ag.DrainInbox(); len(msgs) != 0 {
			t.Errorf("inbox = %v, want empty beyond freshness", msgs)
		}
		boots, _ := h.RecentBoots(ctx, time.Now().Add(-time.Minute))
		if len(boots) != 0 {
			t.Errorf("boot log = %+v, want no record — a stale skip must not charge the cumulative cap", boots)
		}
	})
	t.Run("breaker stands the startup trigger down", func(t *testing.T) {
		t.Parallel()
		h := seedAC(t, acUserEvent("hello?", time.Now()))
		for i := 0; i < 3; i++ {
			if _, err := h.RecordBoot(ctx, time.Now().Add(-time.Duration(i+1)*time.Minute), []string{"other-sid"}); err != nil {
				t.Fatalf("RecordBoot: %v", err)
			}
		}
		ag := acAgent(t, h)
		AutoContinueStartupSession(ctx, h, ag, time.Hour)
		if msgs := ag.DrainInbox(); len(msgs) != 0 {
			t.Errorf("inbox = %v, want empty while the breaker is tripped", msgs)
		}
		boots, _ := h.RecentBoots(ctx, time.Now().Add(-breakerWindow))
		if len(boots) != 3 {
			t.Errorf("boot log has %d records, want 3 — stand-down must not record", len(boots))
		}
	})
}

func TestMaybeAutoContinue_QueuesNoteForFreshInterruption(t *testing.T) {
	t.Parallel()
	h := seedAC(t,
		acModelEvent("earlier answer", time.Now().Add(-10*time.Minute)),
		acUserEvent("hello? anyone?", time.Now().Add(-5*time.Minute)),
	)
	ag := acAgent(t, h)
	maybeAutoContinue(acDeps(h, time.Hour), auth.Caller{Identity: acUser}, acSID, ag)

	msgs := ag.DrainInbox()
	if len(msgs) != 1 {
		t.Fatalf("inbox has %d messages, want 1 continuation note", len(msgs))
	}
	if !strings.Contains(msgs[0], "previous turn did not complete") {
		t.Errorf("inbox message = %q, want the continuation system note", msgs[0])
	}
	// The note must NOT assert an unverifiable cause (#615): no "daemon
	// restart" claim, even though this can fire from the in-lifetime
	// retry loop with zero restarts.
	if strings.Contains(msgs[0], "daemon restart") {
		t.Errorf("inbox message = %q, must not claim a daemon restart", msgs[0])
	}
	// The run lock must be released again — a held lease would block
	// autonomous resume and the next auto-continue attempt.
	lock, err := h.AcquireLock(context.Background(), "core-agent", acUser, acSID)
	if err != nil {
		t.Fatalf("run lock still held after maybeAutoContinue: %v", err)
	}
	lock.Release()
}

func TestMaybeAutoContinue_SkipsCompletedAndStaleAndLocked(t *testing.T) {
	t.Parallel()
	t.Run("completed turn", func(t *testing.T) {
		t.Parallel()
		h := seedAC(t, acUserEvent("q", time.Now().Add(-2*time.Minute)), acModelEvent("done", time.Now().Add(-time.Minute)))
		ag := acAgent(t, h)
		maybeAutoContinue(acDeps(h, time.Hour), auth.Caller{Identity: acUser}, acSID, ag)
		if msgs := ag.DrainInbox(); len(msgs) != 0 {
			t.Errorf("inbox = %v, want empty for a completed tail", msgs)
		}
	})
	t.Run("stale interruption", func(t *testing.T) {
		t.Parallel()
		h := seedAC(t, acUserEvent("hello?", time.Now().Add(-3*time.Hour)))
		ag := acAgent(t, h)
		maybeAutoContinue(acDeps(h, time.Hour), auth.Caller{Identity: acUser}, acSID, ag)
		if msgs := ag.DrainInbox(); len(msgs) != 0 {
			t.Errorf("inbox = %v, want empty beyond the freshness window", msgs)
		}
	})
	t.Run("zero freshness disables the window", func(t *testing.T) {
		t.Parallel()
		h := seedAC(t, acUserEvent("hello?", time.Now().Add(-3*time.Hour)))
		ag := acAgent(t, h)
		maybeAutoContinue(acDeps(h, 0), auth.Caller{Identity: acUser}, acSID, ag)
		if msgs := ag.DrainInbox(); len(msgs) != 1 {
			t.Errorf("inbox = %v, want the note (freshness 0 = always continue)", msgs)
		}
	})
	t.Run("run lock held elsewhere", func(t *testing.T) {
		t.Parallel()
		h := seedAC(t, acUserEvent("hello?", time.Now()))
		lock, err := h.AcquireLock(context.Background(), "core-agent", acUser, acSID)
		if err != nil {
			t.Fatalf("AcquireLock: %v", err)
		}
		defer lock.Release()
		ag := acAgent(t, h)
		maybeAutoContinue(acDeps(h, time.Hour), auth.Caller{Identity: acUser}, acSID, ag)
		if msgs := ag.DrainInbox(); len(msgs) != 0 {
			t.Errorf("inbox = %v, want empty while another holder owns the session", msgs)
		}
	})
}
