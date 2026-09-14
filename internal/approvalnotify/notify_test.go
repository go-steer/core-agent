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
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-steer/core-agent/v2/pkg/attach"
	"github.com/go-steer/core-agent/v2/pkg/config"
	"github.com/go-steer/core-agent/v2/pkg/permissions"
)

// fakeSender records what it was asked to deliver.
type fakeSender struct {
	mu      sync.Mutex
	level   string
	summary string
	details map[string]any
	session string
	err     error
	calls   int
	done    chan struct{}
}

func newFake() *fakeSender { return &fakeSender{done: make(chan struct{}, 4)} }

func (f *fakeSender) Target() string { return "oncall" }

func (f *fakeSender) Send(_ context.Context, level, summary string, details map[string]any, session string) error {
	f.mu.Lock()
	f.level, f.summary, f.details, f.session = level, summary, details, session
	f.calls++
	err := f.err
	f.mu.Unlock()
	f.done <- struct{}{}
	return err
}

func (f *fakeSender) await(t *testing.T) {
	t.Helper()
	select {
	case <-f.done:
	case <-time.After(2 * time.Second):
		t.Fatal("no notification was sent")
	}
}

func quiet() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestNewIsOffByDefault(t *testing.T) {
	t.Parallel()
	cfg := config.DefaultConfig()
	n, err := New(cfg, quiet())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if n != nil {
		t.Fatal("no approval_notify configured, so there should be no notifier")
	}
	// A nil notifier must be safe to wire unconditionally, or every call
	// site grows a branch and one of them will forget it.
	n.Attach(attach.NewPromptBroker(), "sess-1")
	if got := n.Target(); got != "" {
		t.Errorf("Target() = %q, want empty", got)
	}
}

// An operator who names a target that cannot be delivered to must find
// out at startup. Starting anyway and warning on the console delivers
// the failure to the one place they told us they are not reading.
func TestNewRefusesAnUndeliverableTarget(t *testing.T) {
	t.Parallel()
	cfg := config.DefaultConfig()
	cfg.Alerts = config.AlertsConfig{Targets: []config.AlertTarget{
		{Name: "oncall", URLEnv: "DEFINITELY_UNSET_HOOK_URL_FOR_TEST", Template: config.AlertTemplateGeneric},
	}}
	cfg.Permissions.ApprovalNotify = "oncall"

	if _, err := New(cfg, quiet()); err == nil {
		t.Fatal("want a startup error when the notify target cannot be delivered to")
	} else if !strings.Contains(err.Error(), "approval_notify") {
		t.Errorf("the error should name the field the operator set, got: %v", err)
	}
}

func TestNotificationTellsTheRecipientHowToAnswer(t *testing.T) {
	t.Parallel()
	f := newFake()
	n := &Notifier{snd: f, log: quiet()}
	b := attach.NewPromptBroker()
	defer b.Close()
	n.Attach(b, "sess-abc")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_, _ = b.AskApproval(ctx, permissions.PromptRequest{
			Kind: permissions.PromptKindBash, ToolName: "bash",
			Detail: "kubectl delete ns payments-prod", Source: "watch-prod-cluster",
		})
	}()
	f.await(t)

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.session != "sess-abc" {
		t.Errorf("session = %q", f.session)
	}
	if f.level != "warning" {
		t.Errorf("level = %q; paging on a single expected approval trains the recipient to mute the channel", f.level)
	}
	if !strings.Contains(f.summary, "bash") {
		t.Errorf("summary does not name the tool: %q", f.summary)
	}
	id, _ := f.details["request_id"].(string)
	if id == "" {
		t.Fatal("no request_id; the recipient has been told something is waiting and given no way to answer it")
	}
	respond, _ := f.details["respond"].(string)
	for _, want := range []string{"sess-abc", "perms/respond", id, "allow-once", "deny"} {
		if !strings.Contains(respond, want) {
			t.Errorf("the respond instruction is missing %q: %q", want, respond)
		}
	}
	if f.details["detail"] != "kubectl delete ns payments-prod" {
		t.Errorf("detail = %v", f.details["detail"])
	}
	if f.details["source"] != "watch-prod-cluster" {
		t.Errorf("source = %v; a subagent's request and the parent's are not equally surprising", f.details["source"])
	}
}

// The two cases ask a human for different things, so they must not read
// the same. "Expires in nine minutes" is a deadline; "the agent is
// blocked until somebody answers" is an outage nothing else will
// escalate.
func TestNotificationDistinguishesABoundedWaitFromAnUnboundedOne(t *testing.T) {
	t.Parallel()

	t.Run("no approval timeout", func(t *testing.T) {
		f := newFake()
		n := &Notifier{snd: f, log: quiet()}
		b := attach.NewPromptBroker()
		defer b.Close()
		n.Attach(b, "s")
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() { _, _ = b.AskApproval(ctx, permissions.PromptRequest{ToolName: "bash"}) }()
		f.await(t)

		f.mu.Lock()
		defer f.mu.Unlock()
		if _, ok := f.details["expires_at"]; ok {
			t.Error("there is no deadline, so the notification must not invent one")
		}
		exp, _ := f.details["expires"].(string)
		if !strings.Contains(exp, "never") {
			t.Errorf("expires = %q, want it to say the wait is unbounded", exp)
		}
		if !strings.Contains(f.summary, "waiting") {
			t.Errorf("summary = %q", f.summary)
		}
	})

	t.Run("with an approval timeout", func(t *testing.T) {
		f := newFake()
		n := &Notifier{snd: f, log: quiet()}
		b := attach.NewPromptBroker()
		defer b.Close()
		n.Attach(b, "s")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		go func() { _, _ = b.AskApproval(ctx, permissions.PromptRequest{ToolName: "bash"}) }()
		f.await(t)

		f.mu.Lock()
		defer f.mu.Unlock()
		if _, ok := f.details["expires_at"]; !ok {
			t.Error("the prompt expires and the notification did not say when")
		}
		if _, ok := f.details["expires"]; ok {
			t.Error("the bounded case must not also claim the wait is unbounded")
		}
		if !strings.Contains(f.summary, "within") {
			t.Errorf("summary = %q, want it to carry the deadline", f.summary)
		}
	})
}

// A failing notifier must not take the prompt with it, and must not be
// silent about having failed: "we tried to tell you and could not" is
// the most useful line in the log of a run that stalled.
func TestADeliveryFailureDoesNotBreakThePrompt(t *testing.T) {
	t.Parallel()
	f := newFake()
	f.err = errors.New("webhook refused the connection")
	n := &Notifier{snd: f, log: quiet()}
	b := attach.NewPromptBroker()
	defer b.Close()
	n.Attach(b, "s")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan permissions.Decision, 1)
	go func() {
		d, _ := b.AskApproval(ctx, permissions.PromptRequest{ToolName: "bash"})
		out <- d
	}()
	f.await(t)

	var id string
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		if p := b.Pending(); len(p) == 1 {
			id = p[0].ID
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if id == "" {
		t.Fatal("the prompt vanished when its notification failed")
	}
	if err := b.RespondAs(id, permissions.DecisionAllowOnce, "operator"); err != nil {
		t.Fatalf("RespondAs: %v", err)
	}
	select {
	case d := <-out:
		if d != permissions.DecisionAllowOnce {
			t.Errorf("decision = %v", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the prompt never resolved after a failed notification")
	}
}
