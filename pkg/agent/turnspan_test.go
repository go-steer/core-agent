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
	"strings"
	"sync"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/go-steer/core-agent/v2/pkg/auth"
)

// Turn-span tests. The defect these cover: an inject arriving over
// HTTP with a valid `traceparent` produced spans on one trace, and the
// turn that answered it produced a SECOND, unrelated trace rooted at
// ADK's `invoke_agent` — because Agent.Run handed runner.Run a context
// with no span on it, and the inject's span had ended at the handler.
//
// None of these tests may call t.Parallel(): they install a
// process-wide TracerProvider and read a shared recorder.

var (
	testTracerOnce   sync.Once
	testSpanRecorder *tracetest.SpanRecorder
)

// installSpanRecorder installs an SDK TracerProvider the first time
// it's called and returns the shared recorder, emptied for this test.
//
// Process-wide and once-only on purpose. OTel's global TracerProvider
// wires its delegate under a sync.Once, so package-level tracers
// captured at init — ours in turnspan.go, and ADK's `invoke_agent`
// tracer in its internal/telemetry package — permanently bind to
// whichever provider is installed FIRST and ignore every later
// SetTracerProvider. A per-test provider would silently record
// nothing.
func installSpanRecorder(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	testTracerOnce.Do(func() {
		testSpanRecorder = tracetest.NewSpanRecorder()
		otel.SetTracerProvider(sdktrace.NewTracerProvider(
			sdktrace.WithSpanProcessor(testSpanRecorder),
		))
	})
	testSpanRecorder.Reset()
	return testSpanRecorder
}

// spansForSession returns the recorded spans carrying
// gen_ai.conversation.id == sid, so a test can't be perturbed by spans
// another test in this package left behind.
func spansForSession(rec *tracetest.SpanRecorder, sid string) []sdktrace.ReadOnlySpan {
	var out []sdktrace.ReadOnlySpan
	for _, s := range rec.Ended() {
		for _, kv := range s.Attributes() {
			if string(kv.Key) == "gen_ai.conversation.id" && kv.Value.AsString() == sid {
				out = append(out, s)
				break
			}
		}
	}
	return out
}

// findSpan returns the single span whose name matches exactly, or nil.
func findSpan(spans []sdktrace.ReadOnlySpan, name string) sdktrace.ReadOnlySpan {
	for _, s := range spans {
		if s.Name() == name {
			return s
		}
	}
	return nil
}

// findSpanPrefix returns the first span whose name has the prefix.
// ADK names its agent span `invoke_agent <agent name>`.
func findSpanPrefix(spans []sdktrace.ReadOnlySpan, prefix string) sdktrace.ReadOnlySpan {
	for _, s := range spans {
		if strings.HasPrefix(s.Name(), prefix) {
			return s
		}
	}
	return nil
}

func spanNames(spans []sdktrace.ReadOnlySpan) []string {
	out := make([]string, 0, len(spans))
	for _, s := range spans {
		out = append(out, s.Name())
	}
	return out
}

// TestRun_TurnSpanParentsADKInvokeAgent is the core regression. Before
// the fix Agent.Run passed runner.Run a context with no span on it, so
// ADK's `invoke_agent <name>` — which inherits whatever parent is on
// the context handed to it — started a brand-new trace root and the
// whole turn hung off nothing. Now core-agent opens its own
// `agent.turn` span on the per-turn context first, and ADK's span is
// its child.
//
// Fails on pre-fix code: no `agent.turn` span exists at all, and
// `invoke_agent` has no parent.
func TestRun_TurnSpanParentsADKInvokeAgent(t *testing.T) {
	rec := installSpanRecorder(t)

	const sid = "s-turnspan-parent"
	a, err := New(oneShotLLM{}, WithSession("u-turnspan", sid))
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	for _, err := range a.Run(context.Background(), "hi") {
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	}

	spans := spansForSession(rec, sid)
	turn := findSpan(spans, turnSpanName)
	if turn == nil {
		t.Fatalf("no %q span recorded; got %v", turnSpanName, spanNames(spans))
	}
	invoke := findSpanPrefix(spans, "invoke_agent ")
	if invoke == nil {
		t.Fatalf("no ADK invoke_agent span recorded; got %v", spanNames(spans))
	}
	if invoke.Parent().SpanID() != turn.SpanContext().SpanID() {
		t.Errorf("invoke_agent parent = %s, want the %s span %s (the turn must own the trace root, not ADK)",
			invoke.Parent().SpanID(), turnSpanName, turn.SpanContext().SpanID())
	}
	if invoke.SpanContext().TraceID() != turn.SpanContext().TraceID() {
		t.Errorf("invoke_agent trace = %s, turn trace = %s; want the same trace",
			invoke.SpanContext().TraceID(), turn.SpanContext().TraceID())
	}
}

// TestRun_TurnSpanLinksDrainedInjects is the cross-process half: an
// inject that arrived with a live span context must show up as a LINK
// on the turn that answers it. Links rather than a parent edge because
// injects batch — this test queues two and expects two links, which a
// parent edge structurally cannot express.
//
// Fails on pre-fix code: InjectAsContext did not exist, nothing
// captured the span context, and there was no turn span to link from.
func TestRun_TurnSpanLinksDrainedInjects(t *testing.T) {
	rec := installSpanRecorder(t)

	const sid = "s-turnspan-links"
	a, err := New(oneShotLLM{}, WithSession("u-turnspan", sid))
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}

	// Two injects, each on its own (already-ended) span — the shape a
	// pair of watcher POST /inject calls produces while the daemon is
	// mid-turn.
	tracer := otel.Tracer("turnspan-test")
	var want []trace.SpanContext
	for _, msg := range []string{"pod CrashLoopBackOff", "node NotReady"} {
		injectCtx, span := tracer.Start(context.Background(), "POST /sessions/"+sid+"/inject")
		if err := a.InjectAsContext(injectCtx, msg, auth.Caller{Identity: "lookout-watch"}); err != nil {
			t.Fatalf("InjectAsContext: %v", err)
		}
		want = append(want, span.SpanContext())
		span.End() // the handler returns long before the turn runs
	}

	for _, err := range a.Run(context.Background(), "") {
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	}

	spans := spansForSession(rec, sid)
	turn := findSpan(spans, turnSpanName)
	if turn == nil {
		t.Fatalf("no %q span recorded; got %v", turnSpanName, spanNames(spans))
	}
	links := turn.Links()
	if len(links) != len(want) {
		t.Fatalf("turn span links = %d, want %d (one per drained inject)", len(links), len(want))
	}
	for i, w := range want {
		if links[i].SpanContext.SpanID() != w.SpanID() {
			t.Errorf("link[%d] span id = %s, want %s", i, links[i].SpanContext.SpanID(), w.SpanID())
		}
		if links[i].SpanContext.TraceID() != w.TraceID() {
			t.Errorf("link[%d] trace id = %s, want %s", i, links[i].SpanContext.TraceID(), w.TraceID())
		}
	}
	// The turn is deliberately NOT on the injects' trace — it is its
	// own trace, joined by links. Asserting this pins the design
	// decision so nobody "fixes" it into a parent edge later.
	if turn.SpanContext().TraceID() == want[0].TraceID() {
		t.Errorf("turn span shares the first inject's trace %s; a batched turn must be linked, not parented",
			turn.SpanContext().TraceID())
	}
	var linked int64
	for _, kv := range turn.Attributes() {
		if string(kv.Key) == "core_agent.inbox.linked_injects" {
			linked = kv.Value.AsInt64()
		}
	}
	if linked != int64(len(want)) {
		t.Errorf("core_agent.inbox.linked_injects = %d, want %d", linked, len(want))
	}
}

// TestRun_TurnSpanNoLinksWhenUntraced covers the clean no-op: a CLI /
// library inject carries no span context, so the turn span must exist
// (it still parents ADK's span) but carry zero links and no
// linked-injects attribute.
func TestRun_TurnSpanNoLinksWhenUntraced(t *testing.T) {
	rec := installSpanRecorder(t)

	const sid = "s-turnspan-untraced"
	a, err := New(oneShotLLM{}, WithSession("u-turnspan", sid))
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	if err := a.Inject("plain inject, no trace context"); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	// InjectAsContext with a bare context must behave identically.
	if err := a.InjectAsContext(context.Background(), "also untraced", auth.Caller{}); err != nil {
		t.Fatalf("InjectAsContext: %v", err)
	}
	for _, err := range a.Run(context.Background(), "") {
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	}

	turn := findSpan(spansForSession(rec, sid), turnSpanName)
	if turn == nil {
		t.Fatalf("no %q span recorded", turnSpanName)
	}
	if got := len(turn.Links()); got != 0 {
		t.Errorf("turn span links = %d, want 0 for untraced injects", got)
	}
	for _, kv := range turn.Attributes() {
		if string(kv.Key) == "core_agent.inbox.linked_injects" {
			t.Errorf("core_agent.inbox.linked_injects should be absent when nothing was traced; got %v", kv.Value)
		}
	}
}

// --- inboxTraceLinks unit coverage ------------------------------------

// testSpanContext returns a valid SpanContext with a distinguishable
// span id, without needing a tracer.
func testSpanContext(t *testing.T, n byte) trace.SpanContext {
	t.Helper()
	return trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, n},
		SpanID:     trace.SpanID{1, 2, 3, 4, 5, 6, 7, n},
		TraceFlags: trace.FlagsSampled,
	})
}

func TestInboxTraceLinks_SkipsInvalidSpanContexts(t *testing.T) {
	t.Parallel()
	msgs := []inboxMessage{
		{id: "a", text: "no trace"},
		{id: "b", text: "traced", spanCtx: testSpanContext(t, 2)},
		{id: "c", text: "no trace either"},
	}
	links, total := inboxTraceLinks(msgs)
	if total != 1 {
		t.Errorf("total = %d, want 1", total)
	}
	if len(links) != 1 {
		t.Fatalf("links = %d, want 1", len(links))
	}
	if links[0].SpanContext.SpanID() != msgs[1].spanCtx.SpanID() {
		t.Errorf("link span id = %s, want %s", links[0].SpanContext.SpanID(), msgs[1].spanCtx.SpanID())
	}
}

func TestInboxTraceLinks_NoneTracedAllocatesNothing(t *testing.T) {
	t.Parallel()
	links, total := inboxTraceLinks([]inboxMessage{{id: "a"}, {id: "b"}})
	if links != nil {
		t.Errorf("links = %v, want nil for a wholly untraced batch", links)
	}
	if total != 0 {
		t.Errorf("total = %d, want 0", total)
	}
}

// TestInboxTraceLinks_CapsAtMaxKeepingNewest pins the bound. The inbox
// soft cap is 256, so a paused agent really can hand one turn hundreds
// of injects; a span carrying hundreds of links is a span some
// backends drop outright. The NEWEST are kept because the turn
// originator is the last caller in the batch.
func TestInboxTraceLinks_CapsAtMaxKeepingNewest(t *testing.T) {
	t.Parallel()
	const n = maxTurnSpanLinks + 7
	msgs := make([]inboxMessage, 0, n)
	for i := range n {
		msgs = append(msgs, inboxMessage{text: "m", spanCtx: testSpanContext(t, byte(i))})
	}
	links, total := inboxTraceLinks(msgs)
	if total != n {
		t.Errorf("total = %d, want %d (pre-cap count must survive)", total, n)
	}
	if len(links) != maxTurnSpanLinks {
		t.Fatalf("links = %d, want %d", len(links), maxTurnSpanLinks)
	}
	// First kept link is msgs[n-maxTurnSpanLinks]; last is msgs[n-1].
	if links[0].SpanContext.SpanID() != msgs[n-maxTurnSpanLinks].spanCtx.SpanID() {
		t.Errorf("first kept link = %s, want %s (oldest links must be dropped, not newest)",
			links[0].SpanContext.SpanID(), msgs[n-maxTurnSpanLinks].spanCtx.SpanID())
	}
	if links[len(links)-1].SpanContext.SpanID() != msgs[n-1].spanCtx.SpanID() {
		t.Errorf("last kept link = %s, want %s", links[len(links)-1].SpanContext.SpanID(), msgs[n-1].spanCtx.SpanID())
	}
}

// TestInjectAsContext_CapturesSpanContextOnQueuedMessage is the
// narrow unit: the span context must be stamped at inject time, since
// that is the only moment it is available.
func TestInjectAsContext_CapturesSpanContextOnQueuedMessage(t *testing.T) {
	t.Parallel()
	a := &Agent{inbox: newInbox()}
	sc := testSpanContext(t, 9)
	ctx := trace.ContextWithSpanContext(context.Background(), sc)
	if err := a.InjectAsContext(ctx, "hello", auth.Caller{Identity: "watcher"}); err != nil {
		t.Fatalf("InjectAsContext: %v", err)
	}
	msgs := a.inbox.drain()
	if len(msgs) != 1 {
		t.Fatalf("drained %d messages, want 1", len(msgs))
	}
	if msgs[0].spanCtx.SpanID() != sc.SpanID() {
		t.Errorf("queued span id = %s, want %s", msgs[0].spanCtx.SpanID(), sc.SpanID())
	}
	if msgs[0].caller.Identity != "watcher" {
		t.Errorf("caller = %q, want %q", msgs[0].caller.Identity, "watcher")
	}
}

// TestInjectAs_LeavesSpanContextInvalid guards the no-op contract for
// the context-free entry points.
func TestInjectAs_LeavesSpanContextInvalid(t *testing.T) {
	t.Parallel()
	a := &Agent{inbox: newInbox()}
	if err := a.InjectAs("hello", auth.Caller{}); err != nil {
		t.Fatalf("InjectAs: %v", err)
	}
	msgs := a.inbox.drain()
	if len(msgs) != 1 {
		t.Fatalf("drained %d messages, want 1", len(msgs))
	}
	if msgs[0].spanCtx.IsValid() {
		t.Errorf("InjectAs must not fabricate a span context; got %v", msgs[0].spanCtx)
	}
}
