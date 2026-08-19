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
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/go-steer/core-agent/v2/pkg/auth"
)

// The write handlers must hand the inbound request's context — and
// therefore the span context otelhttp extracted from `traceparent` —
// to the registrant. Without it the cross-process trace dies at the
// handler: the handler's span ends when it returns, and the turn that
// eventually drains the message has nothing to link back to.

// tracingRegistrant records the span context each InjectAsContext call
// arrives with, so a test can assert the traceparent survived the hop.
type tracingRegistrant struct {
	eventfulRegistrant
	gotSpanCtx []trace.SpanContext
}

func (tr *tracingRegistrant) InjectAsContext(ctx context.Context, message string, caller auth.Caller) error {
	tr.gotSpanCtx = append(tr.gotSpanCtx, trace.SpanContextFromContext(ctx))
	return tr.InjectAs(message, caller)
}

var traceContextPropagatorOnce sync.Once

// useTraceContextPropagator installs the W3C propagator process-wide,
// which is what otelhttp reads `traceparent` with.
//
// Once, and never restored: OTel's global propagator wires its
// delegate under a sync.Once, so a "restore the previous value"
// cleanup would not actually detach it. Installing the standard
// propagator is also exactly what pkg/telemetry.Setup does in
// production, so nothing else in this package is perturbed by it.
func useTraceContextPropagator(t *testing.T) {
	t.Helper()
	traceContextPropagatorOnce.Do(func() {
		otel.SetTextMapPropagator(propagation.TraceContext{})
	})
}

// testTraceID is the trace id the fake watcher stamps on its inject.
const testTraceID = "0b177bbe8a4aafe7abcbec1d77fe84ca"

// postWithTraceparent POSTs body to url carrying a `traceparent`
// header for testTraceID / spanID, and returns the response status.
func postWithTraceparent(t *testing.T, url, spanID string, body any) int {
	t.Helper()
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("traceparent", "00-"+testTraceID+"-"+spanID+"-01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// TestInject_PropagatesTraceContextToRegistrant is the handler-side
// half of the two-trace defect. Fails on pre-fix code: doInject called
// InjectAs(message, caller) and dropped r.Context() on the floor, so
// InjectAsContext was never reached and the extracted traceparent went
// nowhere.
func TestInject_PropagatesTraceContextToRegistrant(t *testing.T) {
	useTraceContextPropagator(t)
	h, cleanupLog := openTestEventLog(t)
	defer cleanupLog()

	reg := NewSessionRegistry()
	ag := &tracingRegistrant{eventfulRegistrant: eventfulRegistrant{
		stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s1"},
		handle:         h,
	}}
	if _, err := reg.Register(ag); err != nil {
		t.Fatal(err)
	}
	base, cleanupSrv := startTestServer(t, reg)
	defer cleanupSrv()

	const spanID = "aaaaaaaaaaaaaaa1"
	if code := postWithTraceparent(t, base+"/sessions/core-agent/s1/inject", spanID,
		map[string]string{"message": "pod CrashLoopBackOff"}); code != http.StatusOK {
		t.Fatalf("inject status = %d, want 200", code)
	}

	if len(ag.gotSpanCtx) != 1 {
		t.Fatalf("InjectAsContext calls = %d, want 1 (the handler must prefer the context-carrying path)",
			len(ag.gotSpanCtx))
	}
	got := ag.gotSpanCtx[0]
	if !got.IsValid() {
		t.Fatalf("span context on inject ctx is invalid; traceparent did not survive the hop")
	}
	if got.TraceID().String() != testTraceID {
		t.Errorf("trace id = %s, want %s", got.TraceID(), testTraceID)
	}
	if len(ag.injected) != 1 || ag.injected[0] != "pod CrashLoopBackOff" {
		t.Errorf("injected = %v, want [\"pod CrashLoopBackOff\"]", ag.injected)
	}
}

// TestWake_PropagatesTraceContextToRegistrant — a wake carrying a
// prompt is an inject, and must behave identically.
func TestWake_PropagatesTraceContextToRegistrant(t *testing.T) {
	useTraceContextPropagator(t)
	h, cleanupLog := openTestEventLog(t)
	defer cleanupLog()

	reg := NewSessionRegistry()
	ag := &tracingRegistrant{eventfulRegistrant: eventfulRegistrant{
		stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s1"},
		handle:         h,
	}}
	if _, err := reg.Register(ag); err != nil {
		t.Fatal(err)
	}
	base, cleanupSrv := startTestServer(t, reg)
	defer cleanupSrv()

	const spanID = "bbbbbbbbbbbbbbb2"
	if code := postWithTraceparent(t, base+"/sessions/core-agent/s1/wake", spanID,
		map[string]string{"prompt": "rescan now"}); code != http.StatusOK {
		t.Fatalf("wake status = %d, want 200", code)
	}

	if len(ag.gotSpanCtx) != 1 {
		t.Fatalf("InjectAsContext calls = %d, want 1", len(ag.gotSpanCtx))
	}
	if got := ag.gotSpanCtx[0]; got.TraceID().String() != testTraceID {
		t.Errorf("trace id = %s, want %s", got.TraceID(), testTraceID)
	}
	if ag.wakes != 1 {
		t.Errorf("wakes = %d, want 1", ag.wakes)
	}
}

// TestInjectWithContext_FallsBackForPlainRegistrants keeps
// ContextInjector genuinely optional: a registrant that predates it
// must still get its message, just without the link.
func TestInjectWithContext_FallsBackForPlainRegistrants(t *testing.T) {
	t.Parallel()
	ag := &stubRegistrant{app: "a", user: "u", sid: "s"}
	if err := injectWithContext(context.Background(), ag, "hello", auth.Caller{Identity: "op"}); err != nil {
		t.Fatalf("injectWithContext: %v", err)
	}
	if len(ag.injectedAs) != 1 || ag.injectedAs[0].message != "hello" {
		t.Fatalf("injectedAs = %v, want one \"hello\"", ag.injectedAs)
	}
	if ag.injectedAs[0].caller.Identity != "op" {
		t.Errorf("caller = %q, want %q", ag.injectedAs[0].caller.Identity, "op")
	}
}
