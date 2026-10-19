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

package background

import (
	"context"
	"testing"
	"time"

	adkmodel "google.golang.org/adk/v2/model"

	"github.com/go-steer/core-agent/v3/pkg/agent"
	"github.com/go-steer/core-agent/v3/pkg/agent/autonomous"
	"github.com/go-steer/core-agent/v3/pkg/usage"
)

// Budgets.MaxCost must bind even when the parent has no usage
// tracker.
//
// Regression for #729: pricing rode along with WithTracker, so a
// tracker-less parent handed its subagents a MaxCost the driver could
// never evaluate — every turn priced out at exactly $0.00 and the
// budget was decoration. The subagent here would run its full
// MaxTurns allowance instead of stopping on spend.
func TestSpawn_MaxCostBindsWithoutParentTracker(t *testing.T) {
	t.Parallel()
	// A real catalog model ID so PriceFor returns non-zero rates —
	// the point is the wiring, and an unpriced model can't show it.
	const modelID = "gemini-3.5-flash"
	if p := usage.PriceFor(modelID, nil); p.IsZero() {
		t.Skipf("no builtin pricing for %s; cannot exercise the cost path", modelID)
	}

	prov := &usageProvider{name: "usage", in: 1_000_000, out: 0}
	mgr, err := NewManager(
		WithProvider(prov, modelID),
		WithMaxConcurrent(2),
		WithAlertBuffer(4),
	)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer mgr.Close()

	parentLLM, err := prov.Model(context.Background(), modelID)
	if err != nil {
		t.Fatalf("provider Model: %v", err)
	}
	// Deliberately NO agent.WithUsageTracker: this is the shape the
	// bug hid behind.
	parent, err := agent.New(parentLLM, agent.WithBackgroundManager(mgr))
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	if parent.Tracker() != nil {
		t.Fatal("test precondition: parent must have no usage tracker")
	}

	h, err := mgr.Spawn(context.Background(), "", Spec{
		Name: "budgeted", SystemPrompt: "go", Goal: "go", ModelID: modelID,
		// One turn of 1M input tokens costs far more than this, so
		// the run must stop on cost long before it exhausts MaxTurns.
		Budgets: Budgets{MaxCost: 0.0001, MaxTurns: 5},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	select {
	case <-h.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("subagent goroutine didn't finish")
	}

	res := h.Result()
	if res == nil {
		t.Fatalf("no RunResult; err = %v", h.Err())
	}
	if res.Reason != autonomous.StopReasonMaxCost {
		t.Errorf("Reason = %q, want %q (the cost budget never bound)", res.Reason, autonomous.StopReasonMaxCost)
	}
	if res.CostUSD <= 0 {
		t.Errorf("CostUSD = %v, want > 0 (no pricing reached the driver)", res.CostUSD)
	}
	if res.Turns != 1 {
		t.Errorf("Turns = %d, want 1", res.Turns)
	}
	if got := h.Status(); got != StatusDeferred {
		t.Errorf("Status = %q, want %q", got, StatusDeferred)
	}
}

// A declared template may run on a provider other than the manager's
// (its spec.Model names another), and its spawns must be priced on that
// provider's rows (SubagentTemplate.Backend). A "small" downshift runs
// on the manager's provider, so it prices there. gpt-5-mini has rows
// only under "openai/", which makes the backend visible: priced on the
// manager's it would read as unpriced.
func TestSpawnTemplate_PricesOnTheTemplatesBackend(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, model  string
		wantBackend  string
		wantUnpriced bool
	}{
		{"inherit", "", "openai", false},
		{"small", "small", "usage", true}, // the manager's provider, by its name
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			prov := &usageProvider{name: "usage", in: 1000, out: 100}
			mgr, err := NewManager(
				WithProvider(prov, "parent-model"),
				WithSmallModelID("gpt-5-mini"),
				WithDefaultBudgets(Budgets{MaxTurns: 1}),
			)
			if err != nil {
				t.Fatalf("NewManager: %v", err)
			}
			defer mgr.Close()
			if err := mgr.SetSubagentTemplates([]SubagentTemplate{{
				Name:         "coder",
				Instruction:  "write code",
				ModelFactory: func(c context.Context) (adkmodel.LLM, error) { return prov.Model(c, "gpt-5-mini") },
				ModelID:      "gpt-5-mini",
				Backend:      "openai",
			}}); err != nil {
				t.Fatalf("SetSubagentTemplates: %v", err)
			}
			parentLLM, _ := prov.Model(context.Background(), "parent-model")
			tracker := usage.NewTracker()
			if _, err := agent.New(parentLLM, agent.WithBackgroundManager(mgr), agent.WithUsageTracker(tracker)); err != nil {
				t.Fatalf("agent.New: %v", err)
			}

			h, err := mgr.SpawnTemplate(context.Background(), "", "coder", RefOverrides{Goal: "go", Model: tc.model}, "")
			if err != nil {
				t.Fatalf("SpawnTemplate: %v", err)
			}
			select {
			case <-h.Done():
			case <-time.After(5 * time.Second):
				t.Fatal("subagent goroutine didn't finish")
			}
			turns := tracker.All()
			if len(turns) == 0 {
				t.Fatal("no subagent turns reached the parent tracker")
			}
			for _, turn := range turns {
				if turn.Backend != tc.wantBackend || turn.Unpriced != tc.wantUnpriced {
					t.Errorf("turn %s: backend=%q unpriced=%v, want backend=%q unpriced=%v",
						turn.Model, turn.Backend, turn.Unpriced, tc.wantBackend, tc.wantUnpriced)
				}
			}
		})
	}
}
