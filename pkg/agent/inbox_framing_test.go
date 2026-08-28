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
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/auth"
)

// #697: a wake-driven turn has no operator prompt, so the queued bundle
// IS the turn and has to carry the handling guidance the TUI's
// auto-continue flow has had since #144. Without it, two corroborating
// machine signals read as two independent asks.
func TestPrependInboxMessages_BundleIsTheTurnCarriesGuidance(t *testing.T) {
	t.Parallel()
	got := prependInboxMessages("", []string{
		`{"kind":"family.member","message":"blast-radius join: same Deployment as this session's incident"}`,
		`{"kind":"family.member","message":"rollout.stall on the same Deployment"}`,
	}, []string{"lookout", "lookout"}, true)

	if !strings.HasPrefix(got, "[Inbox]\n") {
		t.Errorf("the block must keep the stable, greppable [Inbox] header:\n%s", got)
	}
	if !strings.Contains(got, inboxHandlingGuidance) {
		t.Errorf("wake-driven turn got the bare list with no handling guidance:\n%s", got)
	}
	// The specific branch #697 was filed for.
	if !strings.Contains(got, "Corroborating detail on something you already handled") {
		t.Errorf("guidance is missing the corroboration branch:\n%s", got)
	}
	if strings.Contains(got, "---") {
		t.Errorf("nothing sits below the bundle, so the separator reads as a truncated prompt:\n%s", got)
	}
}

// The other half of the same decision: when the operator typed
// something, that text is the ask and the inbox is side context.
// Bundle guidance there ("treat the bundle as the next request and
// respond once") would compete with what the operator actually asked.
func TestPrependInboxMessages_OperatorPromptKeepsTheBareList(t *testing.T) {
	t.Parallel()
	got := prependInboxMessages("what's next?", []string{"deadline moved up"}, []string{"lookout"}, false)

	if strings.Contains(got, "How to handle the bundle") {
		t.Errorf("bundle guidance must not compete with the operator's own prompt:\n%s", got)
	}
	if !strings.HasSuffix(got, "\n\n---\n\n"+"what's next?") {
		t.Errorf("operator prompt must still sit below the separator:\n%s", got)
	}
}

// Framing is chosen by whether an OPERATOR asked something, which is
// not the same question as "is the prompt argument empty" — Agent.Run
// prepends background alerts before the inbox, so a wake-driven turn
// carrying a subagent report arrives with a non-empty prompt and still
// has nobody asking anything. Guidance applies; the alerts still have
// to survive underneath.
func TestPrependInboxMessages_AlertsBelowStillGetGuidance(t *testing.T) {
	t.Parallel()
	got := prependInboxMessages("[Background reports]\n- subagent finished", []string{"node pool drained"}, []string{"lookout"}, true)

	if !strings.Contains(got, inboxHandlingGuidance) {
		t.Errorf("an alert prepend must not cost the turn its guidance:\n%s", got)
	}
	if !strings.Contains(got, "[Background reports]\n- subagent finished") {
		t.Errorf("the alert block was dropped:\n%s", got)
	}
	if !strings.Contains(got, "\n\n---\n\n") {
		t.Errorf("something sits below the bundle, so the separator belongs:\n%s", got)
	}
}

// The two formatters drifted for two releases — the TUI got every
// guidance revision, the daemon's inject path got none — which is the
// whole of #697. One constant, asserted from both surfaces, is what
// stops it happening a third time.
func TestInboxGuidance_IsSharedByBothSurfaces(t *testing.T) {
	t.Parallel()
	msgs := []string{"x"}
	if !strings.Contains(FormatAutoContinueInbox(msgs), inboxHandlingGuidance) {
		t.Error("auto-continue framing no longer uses the shared guidance")
	}
	if !strings.Contains(prependInboxMessages("", msgs, nil, true), inboxHandlingGuidance) {
		t.Error("the daemon/wake framing no longer uses the shared guidance")
	}
}

// End to end through Agent.Run, on the path the daemon and the REPL
// actually take: WakeLoop calls Run(ctx, ""), the pre-turn drain builds
// the prompt, and the echo mock hands back what the model saw.
func TestRun_WakeDrivenTurnSeesTheGuidance(t *testing.T) {
	t.Parallel()
	a := newDeferredTestAgent(t)

	if err := a.InjectAs("blast-radius join on the resolved incident", auth.Caller{Identity: "lookout"}); err != nil {
		t.Fatalf("InjectAs: %v", err)
	}
	var saw string
	for ev, err := range a.Run(context.Background(), "") {
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if ev == nil || ev.Content == nil || ev.Partial {
			continue
		}
		for _, p := range ev.Content.Parts {
			if p != nil {
				saw += p.Text
			}
		}
	}
	if !strings.Contains(saw, "How to handle the bundle") {
		t.Errorf("the model's prompt on a wake-driven turn had no handling guidance; got %q", saw)
	}
	if !strings.Contains(saw, "blast-radius join on the resolved incident") {
		t.Errorf("the injected message did not reach the model; got %q", saw)
	}
}

// Contrast, same path: an operator-typed turn keeps the bare list, so
// the guidance can't talk over the operator.
func TestRun_OperatorTurnKeepsTheBareList(t *testing.T) {
	t.Parallel()
	a := newDeferredTestAgent(t)

	if err := a.InjectAs("fyi, node pool drained", auth.Caller{Identity: "lookout"}); err != nil {
		t.Fatalf("InjectAs: %v", err)
	}
	var saw string
	for ev, err := range a.Run(context.Background(), "roll back the deployment") {
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if ev == nil || ev.Content == nil || ev.Partial {
			continue
		}
		for _, p := range ev.Content.Parts {
			if p != nil {
				saw += p.Text
			}
		}
	}
	if strings.Contains(saw, "How to handle the bundle") {
		t.Errorf("operator-typed turn got bundle guidance competing with its prompt; got %q", saw)
	}
	if !strings.Contains(saw, "roll back the deployment") || !strings.Contains(saw, "fyi, node pool drained") {
		t.Errorf("both the prompt and the inbox message should reach the model; got %q", saw)
	}
}
