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
	"iter"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/glebarez/sqlite"
	adkagent "google.golang.org/adk/v2/agent"
	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v3/pkg/agent"
	"github.com/go-steer/core-agent/v3/pkg/auth"
	"github.com/go-steer/core-agent/v3/pkg/eventlog"
	"github.com/go-steer/core-agent/v3/pkg/permissions"
)

type bgAuditCallerLLM struct {
	mu       sync.Mutex
	calls    int
	toolName string
}

func (*bgAuditCallerLLM) Name() string { return "bg-audit-caller" }

func (l *bgAuditCallerLLM) GenerateContent(_ context.Context, _ *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	l.mu.Lock()
	l.calls++
	first := l.calls == 1
	l.mu.Unlock()
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		var content *genai.Content
		if first {
			content = &genai.Content{
				Role:  genai.RoleModel,
				Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{Name: l.toolName, Args: map[string]any{}}}},
			}
		} else {
			content = &genai.Content{
				Role:  genai.RoleModel,
				Parts: []*genai.Part{{Text: "pr opened"}},
			}
		}
		yield(&adkmodel.LLMResponse{Content: content, FinishReason: genai.FinishReasonStop, TurnComplete: true}, nil)
	}
}

func TestSpawnAgent_EnqueueOrAppendSessionEventInFlightSQLiteTurn(t *testing.T) {
	t.Parallel()
	baseCtx := context.Background()
	dsn := filepath.Join(t.TempDir(), "eventlog.db")
	handle, err := eventlog.Open(baseCtx, sqlite.Open(dsn), eventlog.WithMetadataExtractor(agent.EventlogMetadataExtractor()))
	if err != nil {
		t.Fatalf("eventlog.Open: %v", err)
	}
	defer func() { _ = handle.Close() }()

	type emptyArgs struct{}
	auditTool, err := functiontool.New(
		functiontool.Config{Name: "bg_emit_vcs", Description: "Emits out-of-band VCS audit events."},
		func(tctx adkagent.Context, _ emptyArgs) (map[string]any, error) {
			for _, k := range []string{"vcs.published", "vcs.proposed"} {
				ev := session.NewEvent(tctx, "vcs")
				ev.Author = "system"
				ev.CustomMetadata = map[string]any{"core_agent_event_kind": k}
				if err := agent.EnqueueOrAppendSessionEvent(tctx, handle.Service, tctx.AppName(), tctx.UserID(), tctx.SessionID(), ev); err != nil {
					return nil, err
				}
			}
			return map[string]any{"ok": true}, nil
		},
	)
	if err != nil {
		t.Fatalf("functiontool.New: %v", err)
	}

	childLLM := &bgAuditCallerLLM{toolName: "bg_emit_vcs"}
	prov := &recordingProvider{llm: childLLM}
	mgr := newTemplateManager(t, prov, []SubagentTemplate{{
		Name:         "proposer",
		Instruction:  "Open a pull request.",
		ModelFactory: tmplFactory(prov, "proposer-model"),
		ModelID:      "proposer-model",
		Tools:        []tool.Tool{auditTool},
	}}, WithDefaultBudgets(Budgets{MaxTurns: 4}))
	defer mgr.Close()

	parentLLM := &spawnCallerLLM{
		args: map[string]any{
			"agent": "proposer",
			"goal":  "propose deployment fix",
			"wait":  true,
		},
	}
	parent, err := agent.New(parentLLM,
		agent.WithName("coordinator"),
		agent.WithEventLog(handle),
		agent.WithSession("u-bg", "sess-bg-parent"),
		agent.WithGate(permissions.New(permissions.Options{Mode: permissions.ModeYolo})),
		agent.WithBackgroundManager(mgr),
		agent.WithTools(NewSpawnTools(mgr)),
	)
	if err != nil {
		t.Fatalf("agent.New(parent): %v", err)
	}

	authedCtx := auth.WithProxyBy(auth.WithCaller(baseCtx, auth.Caller{Identity: "dana@example.com"}), "sa:slack-relay")
	for _, runErr := range parent.Run(authedCtx, "fix deployment") {
		if runErr != nil {
			t.Fatalf("parent.Run: %v", runErr)
		}
	}

	childSID := "sess-bg-parent:sub:bg.proposer-1"
	gotSess, err := handle.Service.Get(baseCtx, &session.GetRequest{
		AppName:   agent.DefaultAppName,
		UserID:    "u-bg",
		SessionID: childSID,
	})
	if err != nil {
		t.Fatalf("Service.Get(%s): %v", childSID, err)
	}
	var kinds []string
	for ev := range gotSess.Session.Events().All() {
		if k, ok := ev.CustomMetadata["core_agent_event_kind"].(string); ok {
			kinds = append(kinds, k)
			if ev.Branch != "bg.proposer-1" {
				t.Errorf("background subagent audit event Branch = %q, want %q", ev.Branch, "bg.proposer-1")
			}
		}
	}
	if strings.Join(kinds, ",") != "vcs.published,vcs.proposed" {
		t.Fatalf("background subagent persisted kinds = %v, want [vcs.published vcs.proposed] (stale session regression if missing)", kinds)
	}

	var matchedCaller int
	for entry, serr := range handle.Stream.Since(baseCtx, 0, eventlog.ForSession(agent.DefaultAppName, "u-bg", childSID)) {
		if serr != nil {
			t.Fatalf("Stream.Since: %v", serr)
		}
		if entry.Event != nil && entry.Event.CustomMetadata["core_agent_event_kind"] != nil {
			if entry.Metadata[eventlog.MetadataKeyCaller] == "dana@example.com" && entry.Metadata[eventlog.MetadataKeyProxyBy] == "sa:slack-relay" {
				matchedCaller++
			}
		}
	}
	if matchedCaller != 2 {
		t.Fatalf("background subagent audit entries with caller+proxy_by metadata = %d, want 2", matchedCaller)
	}
}
