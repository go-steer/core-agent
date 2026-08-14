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
	"errors"
	"iter"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v2/pkg/agent/internal/subsession"
)

// selfSpawningLLM is the 2026-08-13 GKE UAT verbatim: the cluster
// subagent's first act is to spawn "cluster" — itself — with the goal it
// was just handed. Turn 2 says something, so the run can end.
type selfSpawningLLM struct {
	calls atomic.Int32

	mu   sync.Mutex
	seen []string // every text part the model was fed, incl. tool results
}

func (*selfSpawningLLM) Name() string { return "self-spawner" }

func (l *selfSpawningLLM) GenerateContent(_ context.Context, req *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	l.record(req)
	n := l.calls.Add(1)
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		var content *genai.Content
		if n == 1 {
			fc := &genai.FunctionCall{Name: "spawn_agent", Args: map[string]any{
				"agent": "cluster",
				"goal":  "triage emailservice",
			}}
			content = &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: fc}}}
		} else {
			content = &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "done looking"}}}
		}
		yield(&adkmodel.LLMResponse{Content: content, FinishReason: genai.FinishReasonStop, TurnComplete: true}, nil)
	}
}

// record captures the conversation as handed to the model, so the test
// can assert the refusal actually reached it rather than only that the
// spawn didn't happen.
func (l *selfSpawningLLM) record(req *adkmodel.LLMRequest) {
	if req == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, c := range req.Contents {
		if c == nil {
			continue
		}
		for _, p := range c.Parts {
			if p == nil {
				continue
			}
			if p.Text != "" {
				l.seen = append(l.seen, p.Text)
			}
			if p.FunctionResponse != nil {
				for _, v := range p.FunctionResponse.Response {
					if s, ok := v.(string); ok {
						l.seen = append(l.seen, s)
					}
				}
			}
		}
	}
}

func (l *selfSpawningLLM) sawText(sub string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, s := range l.seen {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// The acceptance test for #732, end to end through a running subagent.
//
// In the UAT the cluster subagent spawned cluster-2 with a byte-identical
// goal at depth 1 — inside the depth cap of 2, which is why no cap could
// have caught it — and two agents investigated the same incident
// concurrently on the parent's budget. Pre-fix this test sees a second
// handle registered; post-fix the spawn is refused and the reason lands
// in the model's context.
func TestSpawn_SubagentCannotSpawnItself(t *testing.T) {
	t.Parallel()
	llm := &selfSpawningLLM{}
	prov := &recordingProvider{llm: llm}
	mgr := newTemplateManager(t, prov, nil, WithDefaultBudgets(Budgets{MaxTurns: 2}), WithSyncWaitTimeout(10*time.Second))
	// Registered after construction so the template's tool surface can
	// carry this manager's own spawn tools — the shape a declarative
	// subagent has in the daemon.
	if err := mgr.SetSubagentTemplates([]SubagentTemplate{{
		Name:         "cluster",
		Instruction:  "triage",
		ModelFactory: tmplFactory(prov, "cluster-model"),
		ModelID:      "cluster-model",
		Tools:        NewSpawnTools(mgr),
	}}); err != nil {
		t.Fatalf("SetSubagentTemplates: %v", err)
	}
	attachEchoParent(t, mgr)
	defer mgr.Close()

	h, err := mgr.SpawnTemplate(context.Background(), "", "cluster", RefOverrides{Goal: "triage emailservice"}, "")
	if err != nil {
		t.Fatalf("SpawnTemplate: %v", err)
	}
	waitDone(t, h)

	for _, live := range mgr.List() {
		if live.Name != h.Name {
			t.Errorf("subagent %q was spawned by %q — a second instance of the same subagent, on the same goal", live.Name, h.Name)
		}
	}
	if !llm.sawText("may not spawn itself") {
		t.Error("the refusal never reached the model: it can't reroute onto doing the work itself if it isn't told why the spawn failed")
	}
}

// The declared name is what's matched, not the instance name. Instance
// names are unique by construction ("cluster-1", "cluster-2"), so a
// guard that compared those would never fire.
func TestSpawnTemplate_SelfSpawnMatchesTheDeclaredName(t *testing.T) {
	t.Parallel()
	prov := &recordingProvider{llm: &stopRaceLLM{}}
	mgr := newTemplateManager(t, prov, []SubagentTemplate{
		{Name: "cluster", ModelFactory: tmplFactory(prov, "m"), ModelID: "m"},
		{Name: "gitops", ModelFactory: tmplFactory(prov, "m"), ModelID: "m"},
	})
	attachEchoParent(t, mgr)
	defer mgr.Close()

	// Inside an instance of "cluster" — the context launch hands the
	// subagent's goroutine.
	ctx := subsession.WithLineage(context.Background(), "cluster")

	_, err := mgr.SpawnTemplate(ctx, "", "cluster", RefOverrides{Goal: "again"}, "")
	if !errors.Is(err, ErrSelfSpawn) {
		t.Errorf("spawning cluster from inside cluster: err = %v, want ErrSelfSpawn", err)
	}
	if err != nil && !strings.Contains(err.Error(), "cluster") {
		t.Errorf("err = %q, want the subagent's name in it so the model knows what was refused", err)
	}

	// A different subagent is ordinary delegation, not recursion.
	h, err := mgr.SpawnTemplate(ctx, "", "gitops", RefOverrides{Goal: "roll it back"}, "")
	if err != nil {
		t.Fatalf("spawning a DIFFERENT subagent from inside cluster: %v", err)
	}
	waitDone(t, h)
}

// The catalog (predefined-spec) path is guarded too, and it is the path
// where the declared name is easiest to lose: resolvePredefinedSpec's
// caller overwrites Spec.Name with the instance name before Spawn ever
// sees it, so the guard reads Spec.Ref.
func TestSpawnRef_SelfSpawnOnThePredefinedPath(t *testing.T) {
	t.Parallel()
	prov := &recordingProvider{llm: &stopRaceLLM{}}
	mgr, err := NewManager(WithProvider(prov, "parent-model"),
		WithPredefinedSpecs([]Spec{{Name: "triage", SystemPrompt: "look", Goal: "look"}}))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	attachEchoParent(t, mgr)
	defer mgr.Close()

	ctx := subsession.WithLineage(context.Background(), "triage")
	_, err = mgr.SpawnRef(ctx, "", "triage", "look again", RefOverrides{}, "")
	if !errors.Is(err, ErrSelfSpawn) {
		t.Errorf("SpawnRef(triage) from inside triage: err = %v, want ErrSelfSpawn", err)
	}
}

// The top level spawns freely: the guard only refuses names already on
// the stack, and the parent is on nobody's stack.
func TestSpawnTemplate_ParentSpawnIsUnaffected(t *testing.T) {
	t.Parallel()
	prov := &recordingProvider{llm: &stopRaceLLM{}}
	mgr := newTemplateManager(t, prov, []SubagentTemplate{
		{Name: "cluster", ModelFactory: tmplFactory(prov, "m"), ModelID: "m"},
	})
	attachEchoParent(t, mgr)
	defer mgr.Close()

	h, err := mgr.SpawnTemplate(context.Background(), "", "cluster", RefOverrides{Goal: "triage"}, "")
	if err != nil {
		t.Fatalf("SpawnTemplate from the parent: %v", err)
	}
	waitDone(t, h)
}
