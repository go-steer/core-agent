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

package coretuiremote

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	coretui "github.com/go-steer/core-tui/tui"

	"github.com/go-steer/core-agent/v2/internal/attachclient"
	"github.com/go-steer/core-agent/v2/pkg/attach"
)

// answeringPrompter answers for the operator with a fixed outcome and
// records which entry point the bridge used: AskApprovalDetailed is
// what puts "r" on the prompt, so calling it against a daemon that
// would drop the reason is the bug, not a detail.
type answeringPrompter struct {
	out         coretui.PermissionOutcome
	plainCalls  int
	detailCalls int
}

func (p *answeringPrompter) AskApproval(context.Context, coretui.PermissionRequest) (coretui.PermissionDecision, error) {
	p.plainCalls++
	return p.out.Decision, nil
}

func (p *answeringPrompter) AskApprovalDetailed(context.Context, coretui.PermissionRequest) (coretui.PermissionOutcome, error) {
	p.detailCalls++
	return p.out, nil
}

type fakeBridgeHost struct {
	version string
	mu      sync.Mutex
	notes   []error
}

func (h *fakeBridgeHost) DaemonProtocolVersion() string { return h.version }

func (h *fakeBridgeHost) NotifyOperator(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.notes = append(h.notes, err)
}

// respondRecorder is a /perms/respond that records every body. With
// rejectReason set it answers a body carrying a reason the way a
// 1.15.0+ daemon answers a reason it refuses: 400, prompt still pending.
type respondRecorder struct {
	rejectReason bool
	status       int // when non-zero, every request gets this status
	mu           sync.Mutex
	bodies       []attach.PromptResponse
}

func (r *respondRecorder) serve(t *testing.T) *attachclient.Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /sessions/{sid}/perms/respond", func(w http.ResponseWriter, req *http.Request) {
		raw, _ := io.ReadAll(req.Body)
		var body attach.PromptResponse
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("respond body %q: %v", raw, err)
		}
		r.mu.Lock()
		r.bodies = append(r.bodies, body)
		r.mu.Unlock()
		switch {
		case r.status != 0:
			http.Error(w, "nope", r.status)
		case r.rejectReason && body.Reason != "":
			http.Error(w, "perms/respond: reason is 900 bytes, over the 500-byte limit", http.StatusBadRequest)
		default:
			_ = json.NewEncoder(w).Encode(attach.PromptRespondResponse{Acknowledged: true})
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	parsed, err := attachclient.ParseURL(srv.URL + "/sessions/s1")
	if err != nil {
		t.Fatal(err)
	}
	return attachclient.New(parsed, "", 0)
}

func (r *respondRecorder) got() []attach.PromptResponse {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]attach.PromptResponse(nil), r.bodies...)
}

var testPromptFrame = attach.PromptFrame{ID: "p1", Kind: "bash", ToolName: "bash", Detail: "kubectl delete ns prod"}

// TestRemotePrompt_DenyReasonReachesTheDaemon is the attach half of
// #1165: against a daemon that takes a reason, the prompt offers "r"
// and the reason the operator typed is in the /perms/respond body.
func TestRemotePrompt_DenyReasonReachesTheDaemon(t *testing.T) {
	t.Parallel()
	for _, version := range []string{"1.15.0", "1.16.0", "v1.15.0", "1.15.1"} {
		t.Run(version, func(t *testing.T) {
			t.Parallel()
			rec := &respondRecorder{}
			client := rec.serve(t)
			p := &answeringPrompter{out: coretui.PermissionOutcome{Decision: coretui.DecisionDeny, Reason: "use the staging cluster"}}
			host := &fakeBridgeHost{version: version}

			handleRemotePromptFrame(context.Background(), client, "/sessions/s1", p, testPromptFrame, io.Discard, host)

			if p.detailCalls != 1 || p.plainCalls != 0 {
				t.Errorf("detailed calls = %d, plain = %d; want AskApprovalDetailed so the prompt offers \"r\"", p.detailCalls, p.plainCalls)
			}
			bodies := rec.got()
			if len(bodies) != 1 {
				t.Fatalf("got %d respond requests, want 1: %+v", len(bodies), bodies)
			}
			if b := bodies[0]; b.ID != "p1" || b.Decision != "deny" || b.Reason != "use the staging cluster" {
				t.Errorf("respond body = %+v, want a deny for p1 carrying the reason", b)
			}
			if len(host.notes) != 0 {
				t.Errorf("unexpected operator notes: %v", host.notes)
			}
		})
	}
}

// Every other answer goes over the wire exactly as it did before deny
// reasons existed: no reason field, the same decision strings.
func TestRemotePrompt_NoReasonOnAllowsOrPlainDeny(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		out  coretui.PermissionOutcome
		wire string
	}{
		{coretui.PermissionOutcome{Decision: coretui.DecisionDeny}, "deny"},
		{coretui.PermissionOutcome{Decision: coretui.DecisionAllowOnce}, "allow-once"},
		{coretui.PermissionOutcome{Decision: coretui.DecisionAllowSession}, "allow-session"},
		{coretui.PermissionOutcome{Decision: coretui.DecisionAllowAlways}, "allow-always"},
		// core-tui never sets one on an allow; if it ever did, the
		// daemon would 400 it and the approval would be lost.
		{coretui.PermissionOutcome{Decision: coretui.DecisionAllowOnce, Reason: "stray"}, "allow-once"},
	} {
		t.Run(tc.wire+"/"+tc.out.Reason, func(t *testing.T) {
			t.Parallel()
			rec := &respondRecorder{}
			client := rec.serve(t)
			p := &answeringPrompter{out: tc.out}
			handleRemotePromptFrame(context.Background(), client, "/sessions/s1", p, testPromptFrame, io.Discard, &fakeBridgeHost{version: "1.16.0"})
			bodies := rec.got()
			if len(bodies) != 1 || bodies[0].Decision != tc.wire || bodies[0].Reason != "" {
				t.Errorf("respond bodies = %+v, want one %q with no reason", bodies, tc.wire)
			}
		})
	}
}

// Against a daemon that can't take a reason — or whose version isn't
// known yet — the bridge must not offer "r" at all. A pre-1.15.0 daemon
// answers 200 and drops the field, so the operator would believe the
// model read words it never saw.
func TestRemotePrompt_OldOrUnknownDaemonDoesNotOfferReason(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		host PromptBridgeHost
	}{
		{"nil host", nil},
		{"version not yet known", &fakeBridgeHost{}},
		{"1.14.0", &fakeBridgeHost{version: "1.14.0"}},
		{"1.9.3", &fakeBridgeHost{version: "1.9.3"}},
		{"unparseable", &fakeBridgeHost{version: "latest"}},
		{"another major", &fakeBridgeHost{version: "2.0.0"}},
		{"pre-release of 1.15.0", &fakeBridgeHost{version: "1.15.0-rc.1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := &respondRecorder{}
			client := rec.serve(t)
			p := &answeringPrompter{out: coretui.PermissionOutcome{Decision: coretui.DecisionDeny, Reason: "would be dropped"}}
			handleRemotePromptFrame(context.Background(), client, "/sessions/s1", p, testPromptFrame, io.Discard, tc.host)
			if p.detailCalls != 0 || p.plainCalls != 1 {
				t.Errorf("detailed calls = %d, plain = %d; want plain AskApproval, which never offers \"r\"", p.detailCalls, p.plainCalls)
			}
			bodies := rec.got()
			if len(bodies) != 1 || bodies[0].Decision != "deny" || bodies[0].Reason != "" {
				t.Errorf("respond bodies = %+v, want one plain deny", bodies)
			}
		})
	}
}

// A refused reason must not cost the operator their deny. The daemon's
// 400 leaves the prompt pending, so the bridge re-sends a plain deny
// and tells the operator their reason didn't make it.
func TestRemotePrompt_RefusedReasonFallsBackToPlainDeny(t *testing.T) {
	t.Parallel()
	rec := &respondRecorder{rejectReason: true}
	client := rec.serve(t)
	p := &answeringPrompter{out: coretui.PermissionOutcome{Decision: coretui.DecisionDeny, Reason: "too long, say"}}
	host := &fakeBridgeHost{version: "1.15.0"}

	handleRemotePromptFrame(context.Background(), client, "/sessions/s1", p, testPromptFrame, io.Discard, host)

	bodies := rec.got()
	if len(bodies) != 2 {
		t.Fatalf("got %d respond requests, want the reasoned deny then a plain one: %+v", len(bodies), bodies)
	}
	if bodies[0].Reason == "" || bodies[1].Decision != "deny" || bodies[1].Reason != "" || bodies[1].ID != "p1" {
		t.Errorf("respond bodies = %+v, want a reasoned deny followed by a plain deny for p1", bodies)
	}
	if len(host.notes) != 1 || !strings.Contains(host.notes[0].Error(), "denied without it") {
		t.Errorf("operator notes = %v, want one saying the deny went without the reason", host.notes)
	}
}

// A failure a retry can't fix (the prompt is gone) is not retried, and
// the operator gets no misleading "denied without it".
func TestRemotePrompt_GonePromptIsNotRetried(t *testing.T) {
	t.Parallel()
	rec := &respondRecorder{status: http.StatusGone}
	client := rec.serve(t)
	p := &answeringPrompter{out: coretui.PermissionOutcome{Decision: coretui.DecisionDeny, Reason: "late"}}
	host := &fakeBridgeHost{version: "1.15.0"}
	var log strings.Builder

	handleRemotePromptFrame(context.Background(), client, "/sessions/s1", p, testPromptFrame, &log, host)

	if n := len(rec.got()); n != 1 {
		t.Errorf("got %d respond requests, want 1", n)
	}
	if len(host.notes) != 0 {
		t.Errorf("operator notes = %v, want none", host.notes)
	}
	if !strings.Contains(log.String(), "410") {
		t.Errorf("bridge log = %q, want the 410 logged", log.String())
	}
}

// The adapter is the host main wires in: it learns the version from the
// capabilities frame on its event stream, and its notes land on the
// channel the Events loop renders as chat rows.
func TestAdapter_PromptBridgeHost(t *testing.T) {
	t.Parallel()
	a := New(nil, "/sessions/s1")
	if v := a.DaemonProtocolVersion(); v != "" {
		t.Fatalf("version before any capabilities frame = %q, want empty", v)
	}
	if _, emit := a.consumeTypedFrame(attach.Frame{Type: attach.EventCapabilities, TypedData: &attach.Capabilities{ProtocolVersion: "1.16.0"}}); emit {
		t.Error("a capabilities frame must not render")
	}
	if v := a.DaemonProtocolVersion(); v != "1.16.0" {
		t.Errorf("version = %q, want 1.16.0", v)
	}
	if !daemonTakesDenyReason(a) {
		t.Error("an adapter that saw 1.16.0 must opt the bridge in")
	}

	note := errors.New("reason dropped")
	a.NotifyOperator(note)
	select {
	case got := <-a.injectErrs:
		if !errors.Is(got, note) {
			t.Errorf("queued %v, want %v", got, note)
		}
	default:
		t.Fatal("NotifyOperator queued nothing")
	}
	for range cap(a.injectErrs) + 2 {
		a.NotifyOperator(note) // must never block on a full queue
	}
}
