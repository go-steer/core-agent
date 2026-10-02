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
	"net/http"
	"testing"
	"time"

	"github.com/go-steer/core-agent/v2/pkg/auth"
	"github.com/go-steer/core-agent/v2/pkg/permissions"
)

// askEscalatedInBackground puts a prompt ModeAuto's approver passed on
// to the broker, as the gate does (#1175).
func askEscalatedInBackground(t *testing.T, broker *PromptBroker) (<-chan askResult, PromptFrame) {
	t.Helper()
	out := make(chan askResult, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		a, err := broker.AskApprovalAttributed(ctx, permissions.PromptRequest{
			Kind: permissions.PromptKindBash, ToolName: "bash", Detail: "kubectl rollout restart deploy/api",
			ApproverModel: "judge-1", ApproverReason: "restarts production; the task did not ask for it",
		})
		out <- askResult{approval: a, err: err}
	}()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if p := broker.Pending(); len(p) == 1 {
			return out, p[0]
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("no prompt became pending within the deadline")
	return nil, PromptFrame{}
}

// The frame a client renders carries the approver and its reason, and
// an answer beyond once comes back as what the gate will apply:
// allow-once, downgraded (#1175 decision 11, protocol 1.18.0). Once and
// deny pass through.
func TestPermsRespond_EscalatedPromptCapsAtOnce(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		sent, want string
		downgraded bool
	}{
		{"allow-session", "allow-once", true},
		{"allow-session-tool", "allow-once", true},
		{"allow-always", "allow-once", true},
		{"allow-once", "allow-once", false},
		{"deny", "deny", false},
	} {
		mux, broker := promptRouteFixture(t)
		done, frame := askEscalatedInBackground(t, broker)
		if frame.ApproverModel != "judge-1" || frame.ApproverReason != "restarts production; the task did not ask for it" {
			t.Fatalf("frame = %+v, want the approver model and reason", frame)
		}
		r, rr := respondRequest(t, `{"id":"`+frame.ID+`","decision":"`+tc.sent+`"}`, auth.Caller{Identity: "ops@example.com"}, whoAmISourceBearer)
		mux.ServeHTTP(rr, r)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s: status = %d; body=%q", tc.sent, rr.Code, rr.Body.String())
		}
		if got := decodeRespond(t, rr); got.Decision != tc.want || got.Downgraded != tc.downgraded {
			t.Errorf("%s: response decision=%q downgraded=%v, want %s / %v", tc.sent, got.Decision, got.Downgraded, tc.want, tc.downgraded)
		}
		want, _ := DecisionFromWire(tc.want)
		if a := awaitApproval(t, done); a.Decision != want {
			t.Errorf("%s: prompt resolved with %v, want %v", tc.sent, a.Decision, want)
		}
	}
}

// An ordinary prompt is untouched: allow-session stays allow-session.
func TestPermsRespond_OrdinaryPromptIsNotCapped(t *testing.T) {
	t.Parallel()
	mux, broker := promptRouteFixture(t)
	done, id := askInBackground(t, broker)
	r, rr := respondRequest(t, `{"id":"`+id+`","decision":"allow-session"}`, auth.Caller{Identity: "ops@example.com"}, whoAmISourceBearer)
	mux.ServeHTTP(rr, r)
	if got := decodeRespond(t, rr); got.Decision != "allow-session" || got.Downgraded {
		t.Errorf("response decision=%q downgraded=%v, want allow-session / false", got.Decision, got.Downgraded)
	}
	if a := awaitApproval(t, done); a.Decision != permissions.DecisionAllowSession {
		t.Errorf("prompt resolved with %v", a.Decision)
	}
}
