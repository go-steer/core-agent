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
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/core-agent/v2/pkg/auth"
	"github.com/go-steer/core-agent/v2/pkg/permissions"
)

// promptRouteFixture registers /perms/{stream,respond} on a bare mux
// over a live broker. Routes rather than a full server so the test can
// stamp the caller-resolution verdict onto the request context the way
// the middleware does — attribution is a question about that verdict,
// so it has to be the thing under the test's control.
func promptRouteFixture(t *testing.T) (*http.ServeMux, *PromptBroker) {
	t.Helper()
	broker := NewPromptBroker()
	t.Cleanup(broker.Close)
	reg := NewSessionRegistry()
	ag := &promptRegistrant{
		stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s1"},
		broker:         broker,
	}
	if _, err := reg.Register(ag); err != nil {
		t.Fatalf("Register: %v", err)
	}
	h := &handlers{reg: reg, pool: newBroadcasterPool()}
	mux := http.NewServeMux()
	h.registerPrompts(mux)
	return mux, broker
}

// respondRequest builds a POST /perms/respond carrying the context a
// caller-resolution middleware would have stamped. source == "" means
// no middleware ran at all, which is a distinct case from "ran and
// verified nothing".
func respondRequest(t *testing.T, body string, caller auth.Caller, source string) (*http.Request, *httptest.ResponseRecorder) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/sessions/core-agent/s1/perms/respond", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	ctx := auth.WithCaller(r.Context(), caller)
	if source != "" {
		ctx = withAuthSource(ctx, source)
	}
	return r.WithContext(ctx), httptest.NewRecorder()
}

// askResult is one background AskApprovalAttributed outcome. The error
// is carried rather than asserted in the goroutine because the
// refused-body cases deliberately end by closing the broker, which
// unblocks the prompt with an error that is the expected outcome there.
type askResult struct {
	approval permissions.Approval
	err      error
}

// askInBackground fires one AskApprovalAttributed and returns a channel
// carrying its result plus the broker-assigned prompt id, so a test can
// respond to a prompt that is genuinely in flight.
func askInBackground(t *testing.T, broker *PromptBroker) (<-chan askResult, string) {
	t.Helper()
	out := make(chan askResult, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		a, err := broker.AskApprovalAttributed(ctx, permissions.PromptRequest{
			Kind:     permissions.PromptKindBash,
			ToolName: "bash",
			Detail:   "kubectl rollout restart deploy/api",
		})
		out <- askResult{approval: a, err: err}
	}()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if p := broker.Pending(); len(p) == 1 {
			return out, p[0].ID
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("no prompt became pending within the deadline")
	return nil, ""
}

func decodeRespond(t *testing.T, rr *httptest.ResponseRecorder) PromptRespondResponse {
	t.Helper()
	var got PromptRespondResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode respond body %q: %v", rr.Body.String(), err)
	}
	return got
}

// TestPermsRespond_AttributesToVerifiedCaller is the #830 case. A relay
// answering for a named human is the whole reason the endpoint needs
// attribution: without it the audit record for an approved write names
// the bearer token that carried the click, which is precisely the claim
// an approval gate exists to be able to make.
func TestPermsRespond_AttributesToVerifiedCaller(t *testing.T) {
	t.Parallel()
	mux, broker := promptRouteFixture(t)
	done, id := askInBackground(t, broker)

	r, rr := respondRequest(t, `{"id":"`+id+`","decision":"allow-once"}`,
		auth.Caller{Identity: "responder@example.com"}, whoAmISourceAsserted)
	mux.ServeHTTP(rr, r)
	if rr.Code != http.StatusOK {
		t.Fatalf("respond: status = %d, want 200; body=%q", rr.Code, rr.Body.String())
	}
	if got := decodeRespond(t, rr); !got.Acknowledged || got.Approver != "responder@example.com" {
		t.Errorf("respond body = %+v, want acknowledged with approver responder@example.com", got)
	}

	got := <-done
	if got.err != nil {
		t.Fatalf("AskApprovalAttributed: %v", got.err)
	}
	if got.approval.Decision != permissions.DecisionAllowOnce {
		t.Errorf("decision = %v, want allow-once", got.approval.Decision)
	}
	if got.approval.By != "responder@example.com" {
		t.Errorf("approval.By = %q, want the verified caller", got.approval.By)
	}
}

// TestPermsRespond_AnonymousRecordsNoApprover — the daemon's default
// identity ("anon") is a placeholder, and writing a placeholder into an
// audit line makes an unattributed approval read exactly like an
// attributed one. Empty is the honest answer.
func TestPermsRespond_AnonymousRecordsNoApprover(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		caller auth.Caller
		source string
	}{
		{"middleware verified nothing", auth.Caller{Identity: "anon"}, whoAmISourceAnonymous},
		{"no middleware at all", auth.Caller{Identity: "embedded"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mux, broker := promptRouteFixture(t)
			done, id := askInBackground(t, broker)

			r, rr := respondRequest(t, `{"id":"`+id+`","decision":"allow-session"}`, tc.caller, tc.source)
			mux.ServeHTTP(rr, r)
			if rr.Code != http.StatusOK {
				t.Fatalf("respond: status = %d, want 200; body=%q", rr.Code, rr.Body.String())
			}
			if got := decodeRespond(t, rr); got.Approver != "" {
				t.Errorf("approver = %q, want empty — nothing was verified", got.Approver)
			}
			if got := <-done; got.err != nil || got.approval.By != "" {
				t.Errorf("approval = %+v err = %v, want an unattributed approval", got.approval, got.err)
			}
		})
	}
}

// TestPermsRespond_ApproverFieldIsCheckedNotBelieved — the field is
// accepted only so a client whose idea of the approver differs from the
// server's finds out. Silently dropping it would let a relay believe it
// had attributed a decision it hadn't, which is the same invisible
// failure #830 reports.
func TestPermsRespond_ApproverFieldIsCheckedNotBelieved(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		body     string
		caller   auth.Caller
		source   string
		want     int
		wantBody string
		wantBy   string
	}{
		{
			name:   "matches the verified caller",
			body:   `{"decision":"allow-once","approver":"responder@example.com"}`,
			caller: auth.Caller{Identity: "responder@example.com"},
			source: whoAmISourceAsserted,
			want:   http.StatusOK,
			wantBy: "responder@example.com",
		},
		{
			name:     "disagrees with the verified caller",
			body:     `{"decision":"allow-once","approver":"someone-else@example.com"}`,
			caller:   auth.Caller{Identity: "responder@example.com"},
			source:   whoAmISourceAsserted,
			want:     http.StatusBadRequest,
			wantBody: "does not match the verified caller",
		},
		{
			name:     "nothing was verified to check it against",
			body:     `{"decision":"allow-once","approver":"U0123456"}`,
			caller:   auth.Caller{Identity: "anon"},
			source:   whoAmISourceAnonymous,
			want:     http.StatusBadRequest,
			wantBody: "verified no identity",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mux, broker := promptRouteFixture(t)
			done, id := askInBackground(t, broker)

			body := strings.Replace(tc.body, `{"decision"`, `{"id":"`+id+`","decision"`, 1)
			r, rr := respondRequest(t, body, tc.caller, tc.source)
			mux.ServeHTTP(rr, r)
			if rr.Code != tc.want {
				t.Fatalf("status = %d, want %d; body=%q", rr.Code, tc.want, rr.Body.String())
			}
			if tc.wantBody != "" && !strings.Contains(rr.Body.String(), tc.wantBody) {
				t.Errorf("body = %q, want it to mention %q", rr.Body.String(), tc.wantBody)
			}
			if tc.want != http.StatusOK {
				// The prompt must still be pending — a refused body
				// cannot have unblocked the tool call.
				if p := broker.Pending(); len(p) != 1 {
					t.Errorf("pending = %d after a refused respond, want the prompt still waiting", len(p))
				}
				broker.Close()
				<-done
				return
			}
			if got := <-done; got.err != nil || got.approval.By != tc.wantBy {
				t.Errorf("approval = %+v err = %v, want By = %q", got.approval, got.err, tc.wantBy)
			}
		})
	}
}

// TestPermsRespond_ApprovalLogNamesTheApprover is the end-to-end claim
// the issue actually makes: "who allowed this" has to be answerable
// after the fact, from the same audit log GET /perms serves. It runs
// the broker through Serialize because that is how the daemon wires a
// gate shared by concurrent subagents — and a wrapper that failed to
// forward the attributed form would land every approval in the log
// anonymous while every narrower test still passed.
func TestPermsRespond_ApprovalLogNamesTheApprover(t *testing.T) {
	t.Parallel()
	mux, broker := promptRouteFixture(t)
	gate := permissions.New(permissions.Options{
		Mode:     permissions.ModeAsk,
		Prompter: permissions.Serialize(broker),
	})

	gated := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		gated <- gate.CheckGeneric(ctx, "deploy", "restart deploy/api")
	}()
	var id string
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && id == "" {
		if p := broker.Pending(); len(p) == 1 {
			id = p[0].ID
			break
		}
		time.Sleep(time.Millisecond)
	}
	if id == "" {
		t.Fatal("gate never prompted")
	}

	r, rr := respondRequest(t, `{"id":"`+id+`","decision":"allow-once"}`,
		auth.Caller{Identity: "oncall@example.com"}, whoAmISourceBearer)
	mux.ServeHTTP(rr, r)
	if rr.Code != http.StatusOK {
		t.Fatalf("respond: status = %d; body=%q", rr.Code, rr.Body.String())
	}
	if err := <-gated; err != nil {
		t.Fatalf("gated call: %v", err)
	}

	approvals := gate.Approvals()
	if len(approvals) != 1 {
		t.Fatalf("approvals = %d, want 1", len(approvals))
	}
	if approvals[0].By != "oncall@example.com" {
		t.Errorf("ApprovalLog.By = %q, want oncall@example.com", approvals[0].By)
	}
}
