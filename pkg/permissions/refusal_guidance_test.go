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
	"strings"
	"testing"
	"time"
)

// #1068, observed on the cluster during the #647 gated-daemon UAT: the
// refusal a model reads describes an event and says nothing about what
// follows from it, so a model with an instruction re-issues the call.
// One denial produced five identical `alert` calls, a
// `repeated-tool-call` critical and a halted session that refused every
// turn until an operator reset the guardrail. These tests pin the two
// sentences that make the refusal terminal.
func TestRefusal_TellsTheModelNotToReIssue(t *testing.T) {
	t.Parallel()
	t.Run("a denial is final for the call it answered", func(t *testing.T) {
		t.Parallel()
		g := New(Options{Mode: ModeAsk, Prompter: &fakePrompter{decision: DecisionDeny}})

		err := g.CheckBash(context.Background(), "kubectl delete ns prod")
		if err == nil {
			t.Fatal("expected the denial to fail the call")
		}
		for _, want := range []string{"denied by user", "do not re-issue it", "end the turn and report the refusal"} {
			if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(want)) {
				t.Errorf("denial does not say %q: %v", want, err)
			}
		}
	})
	t.Run("an expiry says a retry will not be answered either", func(t *testing.T) {
		t.Parallel()
		g := New(Options{Mode: ModeAsk, Prompter: newBlockingPrompter(), ApprovalTimeout: 40 * time.Millisecond})

		err := g.CheckBash(context.Background(), "kubectl apply -f patch.yaml")
		if err == nil {
			t.Fatal("expected the unanswered prompt to expire")
		}
		for _, want := range []string{"do not re-issue this call", "still pending approval"} {
			if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(want)) {
				t.Errorf("expiry does not say %q: %v", want, err)
			}
		}
		// The operator half of the message stays: the model is told to
		// stop, the human is told why nobody answered.
		if !strings.Contains(err.Error(), "/perms/stream") {
			t.Errorf("expiry dropped the operator's way out: %v", err)
		}
	})
}

// The watchdog quoted "permissions: permissions: approval request
// expired…" back on the #647 UAT. ErrPromptExpired names the package
// itself — it is matched as a sentinel and travels as a context cause,
// where no wrap reaches it — and the gate wrapped it a second time.
func TestRefusal_ExpiryNamesThePackageOnce(t *testing.T) {
	t.Parallel()
	g := New(Options{Mode: ModeAsk, Prompter: newBlockingPrompter(), ApprovalTimeout: 40 * time.Millisecond})

	err := g.CheckBash(context.Background(), "kubectl apply -f patch.yaml")
	if err == nil {
		t.Fatal("expected the unanswered prompt to expire")
	}
	if got := strings.Count(err.Error(), "permissions: "); got != 1 {
		t.Errorf("expiry carries %d %q prefixes, want exactly 1: %v", got, "permissions: ", err)
	}
}
