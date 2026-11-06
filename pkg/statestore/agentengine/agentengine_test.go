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

package agentengine_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/aiplatform/apiv1beta1/aiplatformpb"
	"github.com/glebarez/sqlite"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/go-steer/core-agent/v3/pkg/eventlog"
	"github.com/go-steer/core-agent/v3/pkg/statestore/agentengine"
	"github.com/go-steer/core-agent/v3/pkg/statestore/conformance"
)

// TestAgentEngine_Conformance runs the backend contract against the
// fake server through the real generated client, with Reopen so the
// handover case (a new process over a populated store) is covered.
func TestAgentEngine_Conformance(t *testing.T) {
	t.Parallel()
	servers := map[eventlog.Transcript]*fakeServer{}
	conformance.Run(t, conformance.Backend{
		Open: func(t *testing.T) eventlog.Transcript {
			tr, f := openFake(t)
			servers[tr] = f
			return tr
		},
		Reopen: func(t *testing.T, prev eventlog.Transcript) eventlog.Transcript {
			return reopenFake(t, servers[prev])
		},
	})
}

// TestAgentEngine_Conformance_Live runs the same suite against a real
// reasoning engine when CORE_AGENT_TEST_REASONING_ENGINE names one
// (projects/P/locations/L/reasoningEngines/N) and ADC is available.
// Provider-gated like the Vertex tests; skipped otherwise.
func TestAgentEngine_Conformance_Live(t *testing.T) {
	engine := os.Getenv("CORE_AGENT_TEST_REASONING_ENGINE")
	if engine == "" {
		t.Skip("CORE_AGENT_TEST_REASONING_ENGINE not set")
	}
	open := func(t *testing.T) eventlog.Transcript {
		tr, err := agentengine.Open(context.Background(), agentengine.Config{ReasoningEngine: engine})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		t.Cleanup(func() { _ = tr.Close() })
		return tr
	}
	conformance.Run(t, conformance.Backend{
		Open:        open,
		Reopen:      func(t *testing.T, _ eventlog.Transcript) eventlog.Transcript { return open(t) },
		WakeTimeout: 30 * time.Second,
	})
}

func TestConfig_Validate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		cfg  agentengine.Config
		ok   bool
	}{
		{"full resource name", agentengine.Config{ReasoningEngine: testEngine}, true},
		{"with ttl", agentengine.Config{ReasoningEngine: testEngine, SessionTTL: 48 * time.Hour}, true},
		{"empty", agentengine.Config{}, false},
		{"bare id", agentengine.Config{ReasoningEngine: "123"}, false},
		{"wrong collection", agentengine.Config{ReasoningEngine: "projects/p/locations/l/agents/1"}, false},
		{"ttl below minimum", agentengine.Config{ReasoningEngine: testEngine, SessionTTL: time.Hour}, false},
		{"negative ttl", agentengine.Config{ReasoningEngine: testEngine, SessionTTL: -time.Hour}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.cfg.Validate(); (err == nil) != tc.ok {
				t.Fatalf("Validate() = %v, want ok=%v", err, tc.ok)
			}
		})
	}
}

// Session IDs Agent Engine would reject are mapped on the wire and
// unmapped on the way back; IDs it accepts pass unchanged.
func TestAgentEngine_SessionIDMapping(t *testing.T) {
	t.Parallel()
	tr, f := openFake(t)
	ctx := context.Background()
	ids := []string{
		"rwc-0123abcd",                         // native: passes unchanged
		"019a2b3c-4d5e-7f80-9abc-def012345678", // UUIDv7, leading digit
		"rwc-0123abcd:sub:research:inv-1",      // derived child
		"rwc-0123abcd:digest",                  // derived sibling root
		strings.Repeat("x", 80),                // too long
	}
	for _, id := range ids {
		if _, err := tr.Service().Create(ctx, &session.CreateRequest{AppName: "app", UserID: "u", SessionID: id}); err != nil {
			t.Fatalf("Create %q: %v", id, err)
		}
	}
	f.mu.Lock()
	var names []string
	for n := range f.sessions {
		names = append(names, strings.TrimPrefix(n, testEngine+"/sessions/"))
	}
	f.mu.Unlock()
	if len(names) != len(ids) {
		t.Fatalf("server holds %d sessions, want %d", len(names), len(ids))
	}
	for _, n := range names {
		if !fakeIDPattern.MatchString(n) {
			t.Errorf("wire ID %q violates the service charset", n)
		}
	}
	found := map[string]bool{}
	for _, n := range names {
		found[n] = true
	}
	if !found["rwc-0123abcd"] {
		t.Errorf("native ID was rewritten; wire IDs: %v", names)
	}
	// Round trip: every original ID comes back from Get and List.
	for _, id := range ids {
		resp, err := tr.Service().Get(ctx, &session.GetRequest{AppName: "app", UserID: "u", SessionID: id})
		if err != nil {
			t.Fatalf("Get %q: %v", id, err)
		}
		if resp.Session.ID() != id {
			t.Errorf("Get %q returned ID %q", id, resp.Session.ID())
		}
	}
	list, err := tr.Service().List(ctx, &session.ListRequest{AppName: "app", UserID: "u"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list.Sessions) != len(ids) {
		t.Fatalf("List returned %d sessions, want %d", len(list.Sessions), len(ids))
	}
	// Mapping is deterministic across processes.
	tr2 := reopenFake(t, f)
	if _, err := tr2.Service().Get(ctx, &session.GetRequest{AppName: "app", UserID: "u", SessionID: ids[1]}); err != nil {
		t.Fatalf("Get after reopen for a mapped ID: %v", err)
	}
}

// After the process that owned a tree is gone, a new one restores the
// whole tree — root and children — from the store, continues the seq,
// and from then on serves reads without RPCs.
func TestAgentEngine_RestoreAfterLoss(t *testing.T) {
	t.Parallel()
	tr, f := openFake(t)
	ctx := context.Background()
	root := mustCreate(t, tr, "root-1")
	child := mustCreate(t, tr, "root-1:sub:k:1")
	for i := 1; i <= 3; i++ {
		mustAppend(t, tr, root, ev(fmt.Sprintf("r%d", i), "model", ""))
	}
	mustAppend(t, tr, child, ev("c1", "k", "k"))
	mustAppend(t, tr, root, ev("r4", "model", ""))
	_ = tr.Close()

	// Sandbox gone. New process, same store.
	tr2 := reopenFake(t, f)
	f.mu.Lock()
	f.listSessionsCalls, f.listEventsCalls, f.getSessionCalls = 0, 0, 0
	f.mu.Unlock()

	high, err := tr2.HighestSeq(ctx, "app", "u", "root-1")
	if err != nil {
		t.Fatalf("HighestSeq: %v", err)
	}
	if high != 5 {
		t.Fatalf("HighestSeq after restore = %d, want 5", high)
	}
	var got []string
	for e, err := range tr2.Since(ctx, 0, eventlog.WithSessionTree("app", "u", "root-1")) {
		if err != nil {
			t.Fatalf("Since: %v", err)
		}
		got = append(got, fmt.Sprintf("%d:%s", e.Seq, e.Event.ID))
	}
	want := "1:r1 2:r2 3:r3 4:c1 5:r4"
	if strings.Join(got, " ") != want {
		t.Fatalf("restored tree = %q, want %q", strings.Join(got, " "), want)
	}
	// Resume appends continue the sequence on the restored root.
	resp, err := tr2.Service().Get(ctx, &session.GetRequest{AppName: "app", UserID: "u", SessionID: "root-1"})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if resp.Session.Events().Len() != 4 {
		t.Fatalf("restored root has %d events, want 4", resp.Session.Events().Len())
	}
	mustAppend(t, tr2, resp.Session, ev("r5", "model", ""))
	if high, _ = tr2.HighestSeq(ctx, "app", "u", "root-1"); high != 6 {
		t.Fatalf("HighestSeq after resume append = %d, want 6", high)
	}
	// RPC budget: the restore was one ListSessions plus one ListEvents
	// per member; the Get, Since and HighestSeq after it cost nothing.
	f.mu.Lock()
	ls, le, gs := f.listSessionsCalls, f.listEventsCalls, f.getSessionCalls
	f.mu.Unlock()
	if ls != 1 || le != 2 || gs != 0 {
		t.Fatalf("restore RPCs: ListSessions=%d ListEvents=%d GetSession=%d; want 1, 2, 0", ls, le, gs)
	}
}

// A tree larger than one ListEvents page is restored whole.
func TestAgentEngine_RestorePaginates(t *testing.T) {
	t.Parallel()
	tr, f := openFake(t)
	f.pageSize = 7
	ctx := context.Background()
	root := mustCreate(t, tr, "big")
	for i := 1; i <= 50; i++ {
		mustAppend(t, tr, root, ev(fmt.Sprintf("e%02d", i), "model", ""))
	}
	tr2 := reopenFake(t, f)
	n := 0
	var last int64
	for e, err := range tr2.Since(ctx, 0, eventlog.ForSession("app", "u", "big")) {
		if err != nil {
			t.Fatalf("Since: %v", err)
		}
		if e.Seq != last+1 {
			t.Fatalf("seq gap: %d after %d", e.Seq, last)
		}
		last = e.Seq
		n++
	}
	if n != 50 {
		t.Fatalf("restored %d events, want 50", n)
	}
}

// A quota rejection that persists through the one retry surfaces from
// AppendEvent with its status intact (so a 429 is recognisable), and
// nothing is recorded locally: the next append reuses the seq.
func TestAgentEngine_AppendFailsClosed(t *testing.T) {
	t.Parallel()
	tr, f := openFake(t)
	ctx := context.Background()
	root := mustCreate(t, tr, "q")
	mustAppend(t, tr, root, ev("e1", "model", ""))
	f.mu.Lock()
	f.failAppend, f.failAppendN = codes.ResourceExhausted, 2
	f.mu.Unlock()
	err := tr.Service().AppendEvent(ctx, root, ev("e2", "model", ""))
	if err == nil {
		t.Fatal("AppendEvent succeeded while the store refused it twice")
	}
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("error lost its status: %v", err)
	}
	var re *agentengine.AppendRetryError
	if !errors.As(err, &re) || re.Outcome != "persisted" {
		t.Fatalf("error = %v, want an AppendRetryError with outcome persisted", err)
	}
	if high, _ := tr.HighestSeq(ctx, "app", "u", "q"); high != 1 {
		t.Fatalf("HighestSeq after failed append = %d, want 1", high)
	}
	if root.Events().Len() != 1 {
		t.Fatalf("in-turn session recorded the failed event: %d events", root.Events().Len())
	}
	mustAppend(t, tr, root, ev("e3", "model", ""))
	var seqs []int64
	for e := range tr.Since(ctx, 0, eventlog.ForSession("app", "u", "q")) {
		seqs = append(seqs, e.Seq)
	}
	if len(seqs) != 2 || seqs[1] != 2 {
		t.Fatalf("seqs after recovery = %v, want [1 2]", seqs)
	}
}

// One quota rejection is absorbed by the retry (#935's policy): the
// append succeeds and is written exactly once.
func TestAgentEngine_QuotaRejectionIsRetriedOnce(t *testing.T) {
	t.Parallel()
	tr, f := openFake(t)
	root := mustCreate(t, tr, "retry")
	f.mu.Lock()
	f.failAppend = codes.ResourceExhausted
	before := f.appendCalls
	f.mu.Unlock()
	mustAppend(t, tr, root, ev("e1", "model", ""))
	f.mu.Lock()
	calls, stored := f.appendCalls-before, len(f.events[testEngine+"/sessions/retry"])
	f.mu.Unlock()
	if calls != 2 || stored != 1 {
		t.Fatalf("append RPCs = %d, stored = %d; want 2 and 1", calls, stored)
	}
}

// An ambiguous failure is never retried blindly — that could write the
// event twice — and a rejection that is not a quota error is not either.
func TestAgentEngine_OnlyQuotaRejectionsAreRetried(t *testing.T) {
	t.Parallel()
	for _, code := range []codes.Code{codes.Unavailable, codes.InvalidArgument, codes.DeadlineExceeded} {
		t.Run(code.String(), func(t *testing.T) {
			t.Parallel()
			tr, f := openFake(t)
			root := mustCreate(t, tr, "nr")
			f.mu.Lock()
			f.failAppend = code
			before := f.appendCalls
			f.mu.Unlock()
			if err := tr.Service().AppendEvent(context.Background(), root, ev("e1", "model", "")); err == nil {
				t.Fatal("append succeeded")
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if n := f.appendCalls - before; n != 1 {
				t.Fatalf("%s was attempted %d times, want 1", code, n)
			}
		})
	}
}

// The budget is shared and small: once it is spent, a quota rejection
// fails at once with outcome "skipped" instead of waiting.
func TestAgentEngine_RetryBudgetIsShared(t *testing.T) {
	t.Parallel()
	tr, f := openFake(t)
	root := mustCreate(t, tr, "budget")
	for i := 0; i < 3; i++ { // spend the burst: each append is rejected once, then rescued
		f.mu.Lock()
		f.failAppend = codes.ResourceExhausted
		f.mu.Unlock()
		mustAppend(t, tr, root, ev(fmt.Sprintf("b%d", i), "model", ""))
	}
	f.mu.Lock()
	f.failAppend = codes.ResourceExhausted
	f.mu.Unlock()
	err := tr.Service().AppendEvent(context.Background(), root, ev("b3", "model", ""))
	var re *agentengine.AppendRetryError
	if !errors.As(err, &re) || re.Outcome != "skipped" {
		t.Fatalf("fourth rejection = %v, want outcome skipped", err)
	}
}

// The in-turn session object ADK holds sees the events appended to it,
// which the runner needs for the next model call of the same turn.
func TestAgentEngine_InTurnSessionSeesAppends(t *testing.T) {
	t.Parallel()
	tr, _ := openFake(t)
	ctx := context.Background()
	sess := mustCreate(t, tr, "turn")
	e := ev("s1", "model", "")
	e.Actions.StateDelta = map[string]any{"k": "v", session.KeyPrefixTemp + "x": 1}
	mustAppend(t, tr, sess, e)
	if sess.Events().Len() != 1 {
		t.Fatalf("in-turn session has %d events, want 1", sess.Events().Len())
	}
	if v, err := sess.State().Get("k"); err != nil || v != "v" {
		t.Fatalf("in-turn state k = %v, %v; want v", v, err)
	}
	if _, err := sess.State().Get(session.KeyPrefixTemp + "x"); err == nil {
		t.Fatal("temp: key leaked into persisted state")
	}
	got, _ := tr.Service().Get(ctx, &session.GetRequest{AppName: "app", UserID: "u", SessionID: "turn"})
	if _, err := got.Session.State().Get(session.KeyPrefixTemp + "x"); err == nil {
		t.Fatal("temp: key reached the store")
	}
}

func TestAgentEngine_TTLRefreshWhenHalfElapsed(t *testing.T) {
	t.Parallel()
	f := newFakeServer()
	tr, err := agentengine.Open(context.Background(), agentengine.Config{
		ReasoningEngine: testEngine, SessionTTL: 24 * time.Hour, ClientOptions: f.start(t),
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = tr.Close() })
	ctx := context.Background()
	mustAppend(t, tr, mustCreate(t, tr, "ttl"), ev("e1", "model", ""))
	f.mu.Lock()
	calls := f.updateCalls
	f.mu.Unlock()
	if calls != 0 {
		t.Fatalf("UpdateSession called %d times with a fresh TTL, want 0", calls)
	}
	// Age the server-side expiry past the halfway point and let a new
	// process read it back: its next append must refresh.
	f.mu.Lock()
	f.sessions[testEngine+"/sessions/ttl"].Expiration = &aiplatformpb.Session_ExpireTime{ExpireTime: timestamppb.New(time.Now().Add(time.Hour))}
	f.mu.Unlock()
	tr2 := reopenFake(t, f)
	got, err := tr2.Service().Get(ctx, &session.GetRequest{AppName: "app", UserID: "u", SessionID: "ttl"})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	mustAppend(t, tr2, got.Session, ev("e2", "model", ""))
	f.mu.Lock()
	calls = f.updateCalls
	exp := f.sessions[testEngine+"/sessions/ttl"].GetExpireTime().AsTime()
	f.mu.Unlock()
	if calls != 1 {
		t.Fatalf("UpdateSession called %d times after an append with <½ TTL left, want 1", calls)
	}
	if time.Until(exp) < 23*time.Hour {
		t.Fatalf("expiry after refresh is %s away, want ~24h", time.Until(exp).Round(time.Minute))
	}
	// And not again while the refreshed TTL is fresh.
	mustAppend(t, tr2, got.Session, ev("e3", "model", ""))
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.updateCalls != 1 {
		t.Fatalf("UpdateSession called again with a fresh TTL: %d", f.updateCalls)
	}
}

func TestAgentEngine_GetMissingIsNotFoundWithoutTreeScan(t *testing.T) {
	t.Parallel()
	tr, f := openFake(t)
	_, err := tr.Service().Get(context.Background(), &session.GetRequest{AppName: "app", UserID: "u", SessionID: "nope"})
	if !eventlog.IsSessionNotFound(err) {
		t.Fatalf("Get missing = %v, want not-found", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listSessionsCalls != 1 || f.getSessionCalls != 1 {
		t.Fatalf("RPCs for a missing session: ListSessions=%d GetSession=%d; want 1 and 1", f.listSessionsCalls, f.getSessionCalls)
	}
}

// A Since reader on the root tree while a subagent creates child
// sessions: the member map is written under t.mu and must be read
// under it too (caught by the review; -race reproduces the old bug).
func TestAgentEngine_SinceRacesCreate(t *testing.T) {
	t.Parallel()
	tr, _ := openFake(t)
	ctx := context.Background()
	root := mustCreate(t, tr, "race")
	mustAppend(t, tr, root, ev("r1", "model", ""))
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 20; i++ {
			c := mustCreate(t, tr, fmt.Sprintf("race:sub:k:%d", i))
			mustAppend(t, tr, c, ev(fmt.Sprintf("c%d", i), "k", "k"))
		}
	}()
	for {
		select {
		case <-done:
			return
		default:
		}
		for _, err := range tr.Since(ctx, 0, eventlog.WithSessionTree("app", "u", "race")) {
			if err != nil {
				t.Fatalf("Since: %v", err)
			}
		}
	}
}

// A session that exists on the service under the requested ID but
// belongs to another user or app is reported missing, and cannot be
// appended to, from the other identity.
func TestAgentEngine_OtherUserOrAppCannotSeeOrWrite(t *testing.T) {
	t.Parallel()
	tr, _ := openFake(t)
	ctx := context.Background()
	owner := mustCreate(t, tr, "owned")
	mustAppend(t, tr, owner, ev("o1", "model", ""))
	for _, who := range []struct{ app, user string }{{"app", "intruder"}, {"other-app", "u"}} {
		_, err := tr.Service().Get(ctx, &session.GetRequest{AppName: who.app, UserID: who.user, SessionID: "owned"})
		if !eventlog.IsSessionNotFound(err) {
			t.Fatalf("Get as %s/%s = %v, want not-found", who.app, who.user, err)
		}
		forged := &forgedSession{app: who.app, user: who.user, id: "owned"}
		if err := tr.Service().AppendEvent(ctx, forged, ev("x", "model", "")); !eventlog.IsSessionNotFound(err) {
			t.Fatalf("AppendEvent as %s/%s = %v, want not-found", who.app, who.user, err)
		}
		list, err := tr.Service().List(ctx, &session.ListRequest{AppName: who.app, UserID: who.user})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(list.Sessions) != 0 {
			t.Fatalf("List as %s/%s saw %d sessions, want 0", who.app, who.user, len(list.Sessions))
		}
	}
	if got, _ := tr.Service().Get(ctx, &session.GetRequest{AppName: "app", UserID: "u", SessionID: "owned"}); got.Session.Events().Len() != 1 {
		t.Fatalf("owner's session has %d events, want 1", got.Session.Events().Len())
	}
}

// forgedSession is a session.Session for a key the caller does not own.
type forgedSession struct{ app, user, id string }

func (f *forgedSession) ID() string                { return f.id }
func (f *forgedSession) AppName() string           { return f.app }
func (f *forgedSession) UserID() string            { return f.user }
func (f *forgedSession) State() session.State      { return nil }
func (f *forgedSession) Events() session.Events    { return nil }
func (f *forgedSession) LastUpdateTime() time.Time { return time.Time{} }

// An append whose reply is lost after the write landed must not leave
// a duplicate seq behind: the tree is reloaded from the store before
// the next seq is assigned.
func TestAgentEngine_AmbiguousAppendReloadsBeforeNextSeq(t *testing.T) {
	t.Parallel()
	tr, f := openFake(t)
	ctx := context.Background()
	root := mustCreate(t, tr, "amb")
	mustAppend(t, tr, root, ev("e1", "model", ""))
	f.mu.Lock()
	f.failAppend, f.storeThenFail = codes.Unavailable, true
	f.mu.Unlock()
	if err := tr.Service().AppendEvent(ctx, root, ev("e2", "model", "")); err == nil {
		t.Fatal("append with a lost reply reported success")
	}
	// The next append must see e2 (seq 2) on the service and take 3.
	resp, err := tr.Service().Get(ctx, &session.GetRequest{AppName: "app", UserID: "u", SessionID: "amb"})
	if err != nil {
		t.Fatalf("Get after ambiguous append: %v", err)
	}
	mustAppend(t, tr, resp.Session, ev("e3", "model", ""))
	tr2 := reopenFake(t, f)
	var got []string
	for e, err := range tr2.Since(ctx, 0, eventlog.ForSession("app", "u", "amb")) {
		if err != nil {
			t.Fatalf("Since: %v", err)
		}
		got = append(got, fmt.Sprintf("%d:%s", e.Seq, e.Event.ID))
	}
	if want := "1:e1 2:e2 3:e3"; strings.Join(got, " ") != want {
		t.Fatalf("after ambiguous append: %q, want %q (no duplicate seq)", strings.Join(got, " "), want)
	}
}

// The real service returns CreateSession as a not-done operation; the
// client must poll it to completion and decode the Session.
func TestAgentEngine_CreateWaitsOnPendingOperation(t *testing.T) {
	t.Parallel()
	f := newFakeServer()
	f.slowCreate = true
	tr, err := agentengine.Open(context.Background(), agentengine.Config{ReasoningEngine: testEngine, ClientOptions: f.start(t)})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = tr.Close() })
	sess := mustCreate(t, tr, "slow")
	if sess.ID() != "slow" {
		t.Fatalf("Create returned %q", sess.ID())
	}
	mustAppend(t, tr, sess, ev("e1", "model", ""))
}

// An event synthesised outside a turn (no author or invocation) still
// satisfies the envelope's required fields, and comes back unchanged.
func TestAgentEngine_SyntheticEventEnvelope(t *testing.T) {
	t.Parallel()
	tr, f := openFake(t)
	ctx := context.Background()
	sess := mustCreate(t, tr, "syn")
	e := &session.Event{ID: "cp1", Timestamp: time.Now()}
	e.CustomMetadata = map[string]any{"kind": "checkpoint"}
	mustAppend(t, tr, sess, e)
	f.mu.Lock()
	wire := f.events[testEngine+"/sessions/syn"][0]
	f.mu.Unlock()
	if wire.GetAuthor() == "" || wire.GetInvocationId() == "" {
		t.Fatalf("envelope missing required fields: author=%q invocation=%q", wire.GetAuthor(), wire.GetInvocationId())
	}
	for got := range tr.Since(ctx, 0, eventlog.ForSession("app", "u", "syn")) {
		if got.Event.Author != "" || got.Event.InvocationID != "" {
			t.Fatalf("placeholders leaked into the record: %+v", got.Event)
		}
	}
}

func TestAgentEngine_NativeIDMustEndAlphanumeric(t *testing.T) {
	t.Parallel()
	tr, f := openFake(t)
	mustCreate(t, tr, "rwc-abc-")
	f.mu.Lock()
	defer f.mu.Unlock()
	for n := range f.sessions {
		if strings.HasSuffix(n, "-") {
			t.Fatalf("ID ending in '-' reached the wire unmapped: %s", n)
		}
	}
}

func ev(id, author, branch string) *session.Event {
	e := &session.Event{ID: id, Author: author, Branch: branch, InvocationID: "inv-" + id, Timestamp: time.Now().UTC()}
	e.Content = genai.NewContentFromText("text for "+id, genai.RoleModel)
	return e
}

func mustCreate(t *testing.T, tr eventlog.Transcript, id string) session.Session {
	t.Helper()
	resp, err := tr.Service().Create(context.Background(), &session.CreateRequest{AppName: "app", UserID: "u", SessionID: id})
	if err != nil {
		t.Fatalf("Create %s: %v", id, err)
	}
	return resp.Session
}

func mustAppend(t *testing.T, tr eventlog.Transcript, sess session.Session, e *session.Event) {
	t.Helper()
	if err := tr.Service().AppendEvent(context.Background(), sess, e); err != nil {
		t.Fatalf("AppendEvent %s: %v", e.ID, err)
	}
}

// Decision 1 + F1 of the residency review: a tree handed from instance
// A to B and back must not be served from A's stale cache. Taking the
// lease invalidates the transcript's view, so A resumes on B's events
// and continues the seq instead of reusing one.
func TestAgentEngine_HandoffDoesNotServeAStaleTree(t *testing.T) {
	t.Parallel()
	f := newFakeServer()
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "control.db")
	openInstance := func() *eventlog.Handle {
		tr, err := agentengine.Open(ctx, agentengine.Config{ReasoningEngine: testEngine, ClientOptions: f.start(t), AppendRetryBackoff: time.Millisecond})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		h, err := eventlog.OpenTranscript(ctx, tr, sqlite.Open(dsn), eventlog.WithResidencyLease())
		if err != nil {
			t.Fatalf("OpenTranscript: %v", err)
		}
		t.Cleanup(func() { _ = h.Close() })
		return h
	}
	a, b := openInstance(), openInstance()
	acquire := func(h *eventlog.Handle) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			err := h.AcquireResidency(ctx, "app", "u", "ho")
			if err == nil {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("AcquireResidency: %v", err)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	appendVia := func(h *eventlog.Handle, id string) {
		t.Helper()
		resp, err := h.Service.Get(ctx, &session.GetRequest{AppName: "app", UserID: "u", SessionID: "ho"})
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if err := h.Service.AppendEvent(ctx, resp.Session, ev(id, "model", "")); err != nil {
			t.Fatalf("AppendEvent %s: %v", id, err)
		}
	}

	if _, err := a.Service.Create(ctx, &session.CreateRequest{AppName: "app", UserID: "u", SessionID: "ho"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	appendVia(a, "e1") // A's cache now holds [e1]
	a.ReleaseResidency("app", "u", "ho")

	acquire(b)
	appendVia(b, "e2")
	b.ReleaseResidency("app", "u", "ho")

	acquire(a)
	resp, err := a.Service.Get(ctx, &session.GetRequest{AppName: "app", UserID: "u", SessionID: "ho"})
	if err != nil {
		t.Fatalf("Get after handback: %v", err)
	}
	if n := resp.Session.Events().Len(); n != 2 {
		t.Fatalf("A resumed on %d events, want 2 (its cache was stale)", n)
	}
	appendVia(a, "e3")

	var got []string
	for e, err := range a.Stream.Since(ctx, 0, eventlog.ForSession("app", "u", "ho")) {
		if err != nil {
			t.Fatalf("Since: %v", err)
		}
		got = append(got, fmt.Sprintf("%d:%s", e.Seq, e.Event.ID))
	}
	if want := "1:e1 2:e2 3:e3"; strings.Join(got, " ") != want {
		t.Fatalf("after handoff and back: %q, want %q", strings.Join(got, " "), want)
	}
}
