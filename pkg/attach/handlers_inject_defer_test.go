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
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/otel/trace"

	"github.com/go-steer/core-agent/v2/pkg/auth"
)

// POST /inject {"wake": false} — file a message for the next turn
// without causing one (#698).

// deferringRegistrant implements the full DeferredInjector capability
// and records which of the two paths each request took, which is the
// only thing that distinguishes them from outside.
type deferringRegistrant struct {
	eventfulRegistrant
	queued   []injectedRecord
	queuedTC []trace.SpanContext
	injectCt int
}

func (d *deferringRegistrant) InjectAsContext(ctx context.Context, message string, caller auth.Caller) error {
	d.injectCt++
	return d.InjectAs(message, caller)
}

func (d *deferringRegistrant) QueueAsContext(ctx context.Context, message string, caller auth.Caller) error {
	d.queued = append(d.queued, injectedRecord{message: message, caller: caller})
	d.queuedTC = append(d.queuedTC, trace.SpanContextFromContext(ctx))
	return nil
}

// injectFixture stands up a server over reg and returns its /inject URL.
func injectFixture(t *testing.T, ag Registrant) string {
	t.Helper()
	reg := NewSessionRegistry()
	if _, err := reg.Register(ag); err != nil {
		t.Fatal(err)
	}
	base, cleanup := startTestServer(t, reg)
	t.Cleanup(cleanup)
	return base + "/sessions/core-agent/s1/inject"
}

func newDeferringRegistrant(t *testing.T) *deferringRegistrant {
	t.Helper()
	h, cleanup := openTestEventLog(t)
	t.Cleanup(cleanup)
	return &deferringRegistrant{eventfulRegistrant: eventfulRegistrant{
		stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s1"},
		handle:         h,
	}}
}

// postInject POSTs body and returns the status plus the decoded 200
// body (zero value on a non-200).
func postInject(t *testing.T, url string, body any) (int, InjectResponse) {
	t.Helper()
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	resp, err := http.Post(url, "application/json", bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	var out InjectResponse
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("decode 200 body: %v", err)
		}
	}
	return resp.StatusCode, out
}

func TestInject_WakeFalseQueuesWithoutWaking(t *testing.T) {
	ag := newDeferringRegistrant(t)
	url := injectFixture(t, ag)

	code, body := postInject(t, url, map[string]any{
		"message": "second alert corroborates the first",
		"wake":    false,
	})
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if body.Woke {
		t.Error(`response says woke=true for a {"wake": false} inject`)
	}
	if len(ag.queued) != 1 || ag.queued[0].message != "second alert corroborates the first" {
		t.Fatalf("queued = %v, want the one deferred message", ag.queued)
	}
	// The two paths must not both run, and the waking one must not be
	// the one that ran: it would deliver exactly the preemption the
	// caller asked to avoid, under a 200 that claims otherwise.
	if ag.injectCt != 0 || len(ag.injected) != 0 {
		t.Errorf("waking inject path ran %d times (%v) for a deferred request", ag.injectCt, ag.injected)
	}
	if ag.wakes != 0 {
		t.Errorf("wakes = %d, want 0", ag.wakes)
	}
}

// Back-compat is the whole reason `wake` is a tristate: every client
// written before 1.10.0 omits it and must keep getting a wake.
func TestInject_WakeOmittedOrTrueStillWakes(t *testing.T) {
	for _, tc := range []struct {
		name string
		body map[string]any
	}{
		{"omitted", map[string]any{"message": "operator note"}},
		{"explicit true", map[string]any{"message": "operator note", "wake": true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ag := newDeferringRegistrant(t)
			url := injectFixture(t, ag)

			code, body := postInject(t, url, tc.body)
			if code != http.StatusOK {
				t.Fatalf("status = %d, want 200", code)
			}
			if !body.Woke {
				t.Error("response says woke=false; an inject that did not say otherwise must wake")
			}
			if ag.injectCt != 1 {
				t.Errorf("waking inject path ran %d times, want 1", ag.injectCt)
			}
			if len(ag.queued) != 0 {
				t.Errorf("queued = %v, want nothing on the deferred path", ag.queued)
			}
		})
	}
}

// A registrant that can't defer gets a 501 and, crucially, does NOT
// get the message: silently upgrading to a wake would hand back the
// preemption the caller specifically asked to avoid, behind a 200 that
// says nothing went wrong.
func TestInject_WakeFalseIs501WithoutTheCapability(t *testing.T) {
	h, cleanup := openTestEventLog(t)
	t.Cleanup(cleanup)
	ag := &eventfulRegistrant{
		stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s1"},
		handle:         h,
	}
	url := injectFixture(t, ag)

	code, _ := postInject(t, url, map[string]any{"message": "fyi", "wake": false})
	if code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", code)
	}
	if len(ag.injected) != 0 {
		t.Errorf("injected = %v on a refused request, want nothing delivered", ag.injected)
	}
	if ag.wakes != 0 {
		t.Errorf("wakes = %d on a refused request, want 0", ag.wakes)
	}
	// The same registrant still takes an ordinary inject — the 501 is
	// about the flag, not the session.
	if code, _ := postInject(t, url, map[string]any{"message": "fyi"}); code != http.StatusOK {
		t.Errorf("plain inject status = %d, want 200", code)
	}
}

// The deferred path is a full peer of the waking one, so it has to
// carry the injecting request's span context too — otherwise a
// watcher's deferred signals become the untraceable ones.
func TestInject_WakeFalsePropagatesTraceContext(t *testing.T) {
	useTraceContextPropagator(t)
	ag := newDeferringRegistrant(t)
	url := injectFixture(t, ag)

	if code := postWithTraceparent(t, url, "cccccccccccccccc", map[string]any{
		"message": "node pool drained",
		"wake":    false,
	}); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if len(ag.queuedTC) != 1 {
		t.Fatalf("QueueAsContext calls = %d, want 1", len(ag.queuedTC))
	}
	got := ag.queuedTC[0]
	if !got.IsValid() {
		t.Fatal("span context on the deferred inject is invalid; traceparent did not survive the hop")
	}
	if got.TraceID().String() != testTraceID {
		t.Errorf("trace id = %s, want %s", got.TraceID(), testTraceID)
	}
}

// The caller identity has to thread through the deferred path exactly
// as it does through the waking one — it is what stamps the turn
// originator when the message eventually drains.
func TestInject_WakeFalseCarriesTheCaller(t *testing.T) {
	ag := newDeferringRegistrant(t)
	reg := NewSessionRegistry()
	if _, err := reg.Register(ag); err != nil {
		t.Fatal(err)
	}
	h := newHandlers(reg, nil)
	entry := reg.List()[0]

	body, _ := json.Marshal(map[string]any{"message": "fyi", "wake": false})
	req := httptest.NewRequest(http.MethodPost, "/sessions/s1/inject", bytes.NewReader(body))
	req = req.WithContext(auth.WithCaller(req.Context(), auth.Caller{Identity: "watcher@example.com"}))
	rec := httptest.NewRecorder()
	h.doInject(rec, req, entry)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if len(ag.queued) != 1 || ag.queued[0].caller.Identity != "watcher@example.com" {
		t.Fatalf("queued = %v, want the caller threaded through", ag.queued)
	}
}
