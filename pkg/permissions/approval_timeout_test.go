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

package permissions

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/core-agent/v2/pkg/config"
)

// blockingPrompter never answers. It reports the context it was handed
// so a test can assert what the gate told the prompter about why the
// wait ended — the fact a broker needs to answer a late approver.
type blockingPrompter struct {
	gotCtx  chan context.Context
	blocked chan struct{}
}

func newBlockingPrompter() *blockingPrompter {
	return &blockingPrompter{
		gotCtx:  make(chan context.Context, 1),
		blocked: make(chan struct{}, 1),
	}
}

func (p *blockingPrompter) AskApproval(ctx context.Context, _ PromptRequest) (Decision, error) {
	select {
	case p.gotCtx <- ctx:
	default:
	}
	select {
	case p.blocked <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return DecisionDeny, ctx.Err()
}

// An unanswered prompt with no timeout configured blocks forever, which
// is the pre-v3.0 behaviour and still the default. This pins it, so
// that turning the bound on by accident for every interactive operator
// fails here rather than in somebody's terminal.
func TestApprovalTimeout_ZeroWaitsIndefinitely(t *testing.T) {
	t.Parallel()
	p := newBlockingPrompter()
	g := New(Options{Mode: ModeAsk, Prompter: p})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- g.CheckBash(ctx, "kubectl apply -f patch.yaml") }()

	<-p.blocked
	select {
	case err := <-done:
		t.Fatalf("prompt returned without an answer and without a timeout: %v", err)
	case <-time.After(200 * time.Millisecond):
	}

	cancel()
	if err := <-done; err == nil {
		t.Fatal("expected the cancelled prompt to fail")
	}
}

// The headline case: a gated call in a deployment nobody is watching
// must end, and must end saying why.
func TestApprovalTimeout_ExpiresWithSentinel(t *testing.T) {
	t.Parallel()
	p := newBlockingPrompter()
	g := New(Options{Mode: ModeAsk, Prompter: p, ApprovalTimeout: 40 * time.Millisecond})

	err := g.CheckBash(context.Background(), "kubectl apply -f patch.yaml")
	if err == nil {
		t.Fatal("expected the unanswered prompt to expire")
	}
	if !errors.Is(err, ErrPromptExpired) {
		t.Fatalf("want ErrPromptExpired, got %v", err)
	}
	// A deadline the gate imposed on itself must not surface as the
	// generic context error: a caller matching on DeadlineExceeded
	// cannot tell this apart from an unrelated timeout further up.
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expiry leaked context.DeadlineExceeded: %v", err)
	}
	// The message has to be actionable by the person who finds it in a
	// log hours later, so it names the timeout and the way out.
	for _, want := range []string{"40ms", "/perms/stream", "kubectl apply"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("expiry error does not mention %q: %v", want, err)
		}
	}
}

// A turn the operator stopped themselves is a cancellation, not an
// expiry — even when the timeout was about to fire anyway. Reporting
// "nobody answered" to somebody who is watching closely enough to press
// stop tells them their approval channel is broken when it is not.
func TestApprovalTimeout_ParentCancellationWinsOverExpiry(t *testing.T) {
	t.Parallel()
	p := newBlockingPrompter()
	g := New(Options{Mode: ModeAsk, Prompter: p, ApprovalTimeout: time.Hour})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-p.blocked
		cancel()
	}()

	err := g.CheckBash(ctx, "kubectl apply -f patch.yaml")
	if err == nil {
		t.Fatal("expected the cancelled prompt to fail")
	}
	if errors.Is(err, ErrPromptExpired) {
		t.Fatalf("a cancelled turn was reported as an expiry: %v", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

// The prompter must be able to tell the two apart from the inside, or a
// broker holding the request cannot answer a late approver accurately.
// context.Cause is how that fact crosses the interface without widening
// it.
func TestApprovalTimeout_CausePassedToPrompter(t *testing.T) {
	t.Parallel()
	p := newBlockingPrompter()
	g := New(Options{Mode: ModeAsk, Prompter: p, ApprovalTimeout: 40 * time.Millisecond})

	_ = g.CheckBash(context.Background(), "kubectl apply -f patch.yaml")

	select {
	case ctx := <-p.gotCtx:
		if !errors.Is(context.Cause(ctx), ErrPromptExpired) {
			t.Fatalf("prompter saw cause %v, want ErrPromptExpired", context.Cause(ctx))
		}
	default:
		t.Fatal("prompter was never called")
	}
}

// A sub-gate is how the DAEMON makes gates, and the daemon is the
// deployment with nobody at a terminal. Dropping the bound here would
// leave the unbounded wait in place for exactly the sessions it was
// configured for.
func TestApprovalTimeout_InheritedByDerivedSession(t *testing.T) {
	t.Parallel()
	template := New(Options{Mode: ModeAsk, ApprovalTimeout: 40 * time.Millisecond})
	p := newBlockingPrompter()
	sub := template.DeriveForSession("sess-1", p)

	err := sub.CheckBash(context.Background(), "kubectl apply -f patch.yaml")
	if !errors.Is(err, ErrPromptExpired) {
		t.Fatalf("derived session did not inherit the approval timeout: %v", err)
	}
}

func TestApprovalTimeout_FromConfig(t *testing.T) {
	t.Parallel()
	cfg := config.DefaultConfig()
	cfg.Permissions.ApprovalTimeout = "40ms"
	p := newBlockingPrompter()

	g, err := FromConfig(cfg, t.TempDir(), t.TempDir(), p)
	if err != nil {
		t.Fatalf("FromConfig: %v", err)
	}
	if got := g.approvalTimeout; got != 40*time.Millisecond {
		t.Fatalf("approvalTimeout = %v, want 40ms", got)
	}
}

// A typo must not read as "no bound" on a deployment the operator
// cannot watch, so it fails the constructor rather than defaulting.
func TestApprovalTimeout_FromConfigRejectsGarbage(t *testing.T) {
	t.Parallel()
	cfg := config.DefaultConfig()
	cfg.Permissions.ApprovalTimeout = "ten minutes"

	if _, err := FromConfig(cfg, t.TempDir(), t.TempDir(), nil); err == nil {
		t.Fatal("expected FromConfig to reject an unparseable approval_timeout")
	}
}
