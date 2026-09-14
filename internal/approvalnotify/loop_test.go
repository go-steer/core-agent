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

package approvalnotify

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/go-steer/core-agent/v2/pkg/attach"
	"github.com/go-steer/core-agent/v2/pkg/config"
	"github.com/go-steer/core-agent/v2/pkg/permissions"
)

// webhookSink is a stand-in for the operator's Slack/PagerDuty endpoint.
// It parses the generic template's payload and hands back the details
// map, which is where the request id an approver needs actually lives.
type webhookSink struct {
	*httptest.Server
	mu      sync.Mutex
	details map[string]any
	got     chan struct{}
}

func newSink(t *testing.T) *webhookSink {
	t.Helper()
	s := &webhookSink{got: make(chan struct{}, 4)}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload struct {
			Details map[string]any `json:"details"`
		}
		_ = json.Unmarshal(body, &payload)
		s.mu.Lock()
		s.details = payload.Details
		s.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		s.got <- struct{}{}
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *webhookSink) await(t *testing.T) map[string]any {
	t.Helper()
	select {
	case <-s.got:
	case <-time.After(5 * time.Second):
		t.Fatal("the webhook was never called")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.details
}

// gatedDaemon assembles the pieces a gated, unattended deployment runs:
// a gate in ask mode with an approval timeout, a prompt broker as its
// prompter, and out-of-band escalation pointed at sink.
func gatedDaemon(t *testing.T, sink *webhookSink, timeout time.Duration) (*permissions.Gate, *attach.PromptBroker) {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.Alerts = config.AlertsConfig{Targets: []config.AlertTarget{
		{Name: "oncall", URL: sink.URL, Template: config.AlertTemplateGeneric},
	}}
	cfg.Permissions.ApprovalNotify = "oncall"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("the gated config a daemon would load does not validate: %v", err)
	}

	n, err := New(cfg, quiet())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if n == nil {
		t.Fatal("approval_notify was set and produced no notifier")
	}

	broker := attach.NewPromptBroker()
	t.Cleanup(broker.Close)
	n.Attach(broker, "s1")

	gate := permissions.New(permissions.Options{
		Mode:            permissions.ModeAsk,
		Prompter:        broker,
		ApprovalTimeout: timeout,
	})
	return gate, broker
}

// The whole point of #647, assembled: a daemon running GATED rather
// than mode:yolo, with nobody at a console, takes a gated action only
// after a human who was told about it out-of-band says yes.
//
// Each seam has its own test; this one exists because the seams passing
// individually is what "every shipped autonomous recipe is on yolo"
// looked like before. The claim being made here is about the loop.
func TestAGatedDaemonRunsUnattendedEndToEnd(t *testing.T) {
	t.Parallel()
	sink := newSink(t)
	gate, broker := gatedDaemon(t, sink, 30*time.Second)

	done := make(chan error, 1)
	go func() { done <- gate.CheckBash(context.Background(), "kubectl apply -f deploy.yaml") }()

	details := sink.await(t)
	id, _ := details["request_id"].(string)
	if id == "" {
		t.Fatal("the notification carried no request id, so nothing off-box could answer it")
	}
	if detail, _ := details["detail"].(string); detail == "" {
		t.Error("the notification did not say what is being approved")
	}

	// The answer arrives from outside the process, keyed only on what
	// the notification carried. This is the call POST /perms/respond
	// makes; the HTTP hop itself is covered in pkg/attach.
	if err := broker.RespondAs(id, permissions.DecisionAllowOnce, "oncall-human"); err != nil {
		t.Fatalf("responding with the id from the notification: %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the approved action was refused anyway: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the gated call never resumed after approval")
	}
}

func TestAGatedDaemonHonoursADenial(t *testing.T) {
	t.Parallel()
	sink := newSink(t)
	gate, broker := gatedDaemon(t, sink, 30*time.Second)

	done := make(chan error, 1)
	go func() { done <- gate.CheckBash(context.Background(), "rm -rf /var/lib/data") }()

	details := sink.await(t)
	id, _ := details["request_id"].(string)
	if id == "" {
		t.Fatal("no request id in the notification")
	}
	if err := broker.RespondAs(id, permissions.DecisionDeny, "oncall-human"); err != nil {
		t.Fatalf("RespondAs: %v", err)
	}

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a denied call must fail; silently allowing it is the one outcome the gate exists to prevent")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the gated call never resumed after denial")
	}
}

// Timeout and notification are two halves of one behaviour. The bound
// alone turns a hung agent into one that quietly gives up; the
// notification alone leaves the agent hung if nobody reads it. This
// asserts both happen on the same prompt: somebody was told, AND the
// action was refused rather than left waiting.
func TestAnUnansweredPromptIsBothAnnouncedAndExpired(t *testing.T) {
	t.Parallel()
	sink := newSink(t)
	gate, broker := gatedDaemon(t, sink, 300*time.Millisecond)

	start := time.Now()
	done := make(chan error, 1)
	go func() { done <- gate.CheckBash(context.Background(), "kubectl delete ns payments-prod") }()

	details := sink.await(t)
	id, _ := details["request_id"].(string)
	if id == "" {
		t.Fatal("no request id in the notification")
	}
	if _, ok := details["expires_at"]; !ok {
		t.Error("an approval timeout was configured and the notification did not say when it runs out")
	}

	select {
	case err := <-done:
		if !errors.Is(err, permissions.ErrPromptExpired) {
			t.Fatalf("err = %v, want ErrPromptExpired — the operator needs to tell 'nobody answered' apart from 'somebody stopped this'", err)
		}
		if elapsed := time.Since(start); elapsed > 10*time.Second {
			t.Errorf("the call took %v against a 300ms bound", elapsed)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the gated call never expired; the bound did not bound anything")
	}

	// The late approver gets the honest answer rather than "unknown id".
	if err := broker.RespondAs(id, permissions.DecisionAllowOnce, "slow-human"); !errors.Is(err, attach.ErrPromptExpired) {
		t.Fatalf("late RespondAs err = %v, want attach.ErrPromptExpired", err)
	}
}
