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

package compose

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
	adktool "google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v3/pkg/agent"
	"github.com/go-steer/core-agent/v3/pkg/auth"
	"github.com/go-steer/core-agent/v3/pkg/config"
	"github.com/go-steer/core-agent/v3/pkg/eventlog"
	"github.com/go-steer/core-agent/v3/pkg/permissions"
	"github.com/go-steer/core-agent/v3/pkg/tools/vcs"
)

func TestBuildVCSTools_UpdatesRegisteredToolsAndHonorsExtraDisables(t *testing.T) {
	t.Parallel()
	cfg := config.DefaultConfig()
	cfg.VCS = config.VCSConfig{
		Forges: []config.VCSForgeConfig{{
			Name:     "gh",
			Provider: config.VCSProviderGitHub,
			TokenEnv: "GH_TOKEN",
		}},
		Repositories: []config.VCSRepositoryConfig{{
			Forge:      "gh",
			Repository: "acme/infra",
			Role:       config.VCSRoleGitOps,
			BaseBranch: "main",
		}},
	}
	gate := permissions.New(permissions.Options{Mode: permissions.ModeYolo})
	gate.SetRegisteredTools(map[string]bool{"read_file": true})

	built, err := BuildVCSTools(cfg, gate, t.TempDir(), []string{"vcs_publish"})
	if err != nil {
		t.Fatalf("BuildVCSTools: %v", err)
	}
	if len(built) != len(vcs.ToolNames())-1 {
		t.Fatalf("len(built) = %d, want %d", len(built), len(vcs.ToolNames())-1)
	}
	reg := gate.RegisteredTools()
	if !reg["read_file"] || !reg["vcs_clone"] || !reg["vcs_propose"] {
		t.Errorf("gate.RegisteredTools() = %v, missing expected tools", reg)
	}
	if reg["vcs_publish"] {
		t.Errorf("disabled vcs_publish should not be registered on gate: %v", reg)
	}
}

func TestFormatVCSStartupLine(t *testing.T) {
	t.Parallel()
	if got := FormatVCSStartupLine(config.DefaultConfig()); got != "" {
		t.Errorf("FormatVCSStartupLine(default) = %q, want empty", got)
	}
	cfg := config.DefaultConfig()
	cfg.VCS = config.VCSConfig{
		BranchPrefix: "gitops/",
		Forges: []config.VCSForgeConfig{{
			Name:     "ghes",
			Provider: config.VCSProviderGitHub,
			Host:     "git.internal",
			TokenEnv: "GH_TOKEN",
		}},
		Repositories: []config.VCSRepositoryConfig{{
			Forge:      "ghes",
			Repository: "acme/infra",
			Role:       config.VCSRoleGitOps,
			BaseBranch: "main",
		}},
	}
	got := FormatVCSStartupLine(cfg)
	for _, want := range []string{"vcs: 1 forge(s)", "ghes=github/git.internal", "acme/infra@ghes[role=gitops,base=main]", `branch_prefix="gitops/"`} {
		if !strings.Contains(got, want) {
			t.Errorf("FormatVCSStartupLine = %q, want substring %q", got, want)
		}
	}
}

func TestVCSEventSink_AppendsDurableAuditEvents(t *testing.T) {
	t.Parallel()
	baseCtx := context.Background()
	dsn := filepath.Join(t.TempDir(), "eventlog.db")
	handle, err := eventlog.Open(baseCtx, sqlite.Open(dsn), eventlog.WithMetadataExtractor(agent.EventlogMetadataExtractor()))
	if err != nil {
		t.Fatalf("eventlog.Open: %v", err)
	}
	defer func() { _ = handle.Close() }()

	_, err = handle.Service.Create(baseCtx, &session.CreateRequest{
		AppName:   "core-agent",
		UserID:    "local",
		SessionID: "sess-vcs-1",
	})
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}

	authedCtx := auth.WithProxyBy(auth.WithCaller(baseCtx, auth.Caller{Identity: "alice@example.com"}), "sa:slack-relay")
	sink := VCSEventSink(func() *eventlog.Handle { return handle })
	sink(authedCtx, vcs.Event{
		Kind:       vcs.EventKindPublished,
		Forge:      "gh",
		Repository: "acme/infra",
		Branch:     "agent/scale-up",
		Revision:   "abc1234",
		AppName:    "core-agent",
		UserID:     "local",
		SessionID:  "sess-vcs-1",
	})
	sink(authedCtx, vcs.Event{
		Kind:       vcs.EventKindProposed,
		Forge:      "gh",
		Repository: "acme/infra",
		Number:     42,
		URL:        "https://github.com/acme/infra/pull/42",
		AppName:    "core-agent",
		UserID:     "local",
		SessionID:  "sess-vcs-1",
	})

	getResp, err := handle.Service.Get(baseCtx, &session.GetRequest{
		AppName:   "core-agent",
		UserID:    "local",
		SessionID: "sess-vcs-1",
	})
	if err != nil {
		t.Fatalf("Get session: %v", err)
	}
	events := getResp.Session.Events()
	if events.Len() != 2 {
		t.Fatalf("session events len = %d, want 2", events.Len())
	}
	first := events.At(0)
	if first.CustomMetadata["core_agent_event_kind"] != vcs.EventKindPublished {
		t.Errorf("first event kind = %v, want %q", first.CustomMetadata["core_agent_event_kind"], vcs.EventKindPublished)
	}
	if first.Content != nil {
		t.Errorf("first audit event Content = %+v, want nil (content-less audit row)", first.Content)
	}
	firstMeta, _ := first.CustomMetadata["vcs_event"].(map[string]any)
	if want := "vcs.published gh/acme/infra branch=agent/scale-up revision=abc1234"; firstMeta["summary"] != want {
		t.Errorf("first vcs_event.summary = %v, want %q", firstMeta["summary"], want)
	}

	second := events.At(1)
	if second.CustomMetadata["core_agent_event_kind"] != vcs.EventKindProposed {
		t.Errorf("second event kind = %v, want %q", second.CustomMetadata["core_agent_event_kind"], vcs.EventKindProposed)
	}
	if second.Content != nil {
		t.Errorf("second audit event Content = %+v, want nil (content-less audit row)", second.Content)
	}
	secondMeta, _ := second.CustomMetadata["vcs_event"].(map[string]any)
	if want := "vcs.proposed gh/acme/infra #42 https://github.com/acme/infra/pull/42"; secondMeta["summary"] != want {
		t.Errorf("second vcs_event.summary = %v, want %q", secondMeta["summary"], want)
	}

	var matchedCaller int
	for entry, serr := range handle.Stream.Since(baseCtx, 0, eventlog.ForSession("core-agent", "local", "sess-vcs-1")) {
		if serr != nil {
			t.Fatalf("Stream.Since: %v", serr)
		}
		if entry.Metadata[eventlog.MetadataKeyCaller] == "alice@example.com" && entry.Metadata[eventlog.MetadataKeyProxyBy] == "sa:slack-relay" {
			matchedCaller++
		}
	}
	if matchedCaller != 2 {
		t.Fatalf("audit entries with caller+proxy_by metadata = %d, want 2", matchedCaller)
	}
}

func TestVCSEventSink_InFlightSQLiteTurnDoesNotStaleSessionOrPolluteModelHistory(t *testing.T) {
	t.Parallel()
	baseCtx := context.Background()
	dsn := filepath.Join(t.TempDir(), "eventlog.db")
	handle, err := eventlog.Open(baseCtx, sqlite.Open(dsn), eventlog.WithMetadataExtractor(agent.EventlogMetadataExtractor()))
	if err != nil {
		t.Fatalf("eventlog.Open: %v", err)
	}
	defer func() { _ = handle.Close() }()

	sink := VCSEventSink(func() *eventlog.Handle { return handle })
	type emptyArgs struct{}
	proposeTool, err := functiontool.New(
		functiontool.Config{Name: "simulate_vcs_flow", Description: "Simulates publish and propose audit events."},
		func(tctx adkagent.Context, _ emptyArgs) (map[string]any, error) {
			sink(tctx, vcs.Event{
				Kind:       vcs.EventKindPublished,
				Forge:      "gh",
				Repository: "acme/infra",
				Branch:     "agent/fix-probe",
				Revision:   "deadbeef",
				AppName:    tctx.AppName(),
				UserID:     tctx.UserID(),
				SessionID:  tctx.SessionID(),
			})
			sink(tctx, vcs.Event{
				Kind:       vcs.EventKindProposed,
				Forge:      "gh",
				Repository: "acme/infra",
				Branch:     "agent/fix-probe",
				Revision:   "deadbeef",
				Number:     3,
				URL:        "https://github.com/acme/infra/pull/3",
				AppName:    tctx.AppName(),
				UserID:     tctx.UserID(),
				SessionID:  tctx.SessionID(),
			})
			return map[string]any{"url": "https://github.com/acme/infra/pull/3"}, nil
		},
	)
	if err != nil {
		t.Fatalf("functiontool.New: %v", err)
	}

	llm := &twoTurnVCSProbeLLM{toolName: "simulate_vcs_flow"}
	ag, err := agent.New(llm,
		agent.WithEventLog(handle),
		agent.WithSession("local", "sess-sqlite-1571"),
		agent.WithGate(permissions.New(permissions.Options{Mode: permissions.ModeYolo})),
		agent.WithTools([]adktool.Tool{proposeTool}),
	)
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}

	authedCtx := auth.WithProxyBy(auth.WithCaller(baseCtx, auth.Caller{Identity: "bob@example.com"}), "sa:switchboard")
	for _, runErr := range ag.Run(authedCtx, "open fix pull request") {
		if runErr != nil {
			t.Fatalf("turn 1 Run failed (stale session regression): %v", runErr)
		}
	}
	for _, runErr := range ag.Run(authedCtx, "what did you just propose?") {
		if runErr != nil {
			t.Fatalf("turn 2 Run failed: %v", runErr)
		}
	}

	getResp, err := handle.Service.Get(baseCtx, &session.GetRequest{
		AppName:   agent.DefaultAppName,
		UserID:    "local",
		SessionID: "sess-sqlite-1571",
	})
	if err != nil {
		t.Fatalf("Get session: %v", err)
	}
	var kinds []string
	for ev := range getResp.Session.Events().All() {
		if k, ok := ev.CustomMetadata["core_agent_event_kind"].(string); ok {
			kinds = append(kinds, k)
		}
	}
	if strings.Join(kinds, ",") != "vcs.published,vcs.proposed" {
		t.Fatalf("persisted kinds = %v, want [vcs.published vcs.proposed]", kinds)
	}

	var vcsEntriesWithCaller int
	for entry, serr := range handle.Stream.Since(baseCtx, 0, eventlog.ForSession(agent.DefaultAppName, "local", "sess-sqlite-1571")) {
		if serr != nil {
			t.Fatalf("Stream.Since: %v", serr)
		}
		if entry.Event != nil && entry.Event.CustomMetadata["core_agent_event_kind"] != nil {
			if entry.Metadata[eventlog.MetadataKeyCaller] != "bob@example.com" || entry.Metadata[eventlog.MetadataKeyProxyBy] != "sa:switchboard" {
				t.Fatalf("deferred VCS entry metadata = %v, want caller=bob@example.com and proxy_by=sa:switchboard", entry.Metadata)
			}
			vcsEntriesWithCaller++
		}
	}
	if vcsEntriesWithCaller != 2 {
		t.Fatalf("deferred VCS entries in stream = %d, want 2", vcsEntriesWithCaller)
	}

	lastReq := llm.lastRequest()
	for _, c := range lastReq.Contents {
		for _, p := range c.Parts {
			if strings.Contains(p.Text, "vcs.published") || strings.Contains(p.Text, "vcs.proposed") {
				t.Fatalf("audit event leaked into model history on follow-up turn: %q", p.Text)
			}
		}
	}
}

type twoTurnVCSProbeLLM struct {
	mu       sync.Mutex
	calls    int
	toolName string
	last     *adkmodel.LLMRequest
}

func (*twoTurnVCSProbeLLM) Name() string { return "two-turn-vcs-probe" }

func (l *twoTurnVCSProbeLLM) lastRequest() *adkmodel.LLMRequest {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.last
}

func (l *twoTurnVCSProbeLLM) GenerateContent(_ context.Context, req *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	l.mu.Lock()
	l.calls++
	n := l.calls
	l.last = req
	l.mu.Unlock()
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		var content *genai.Content
		if n == 1 {
			content = &genai.Content{
				Role:  genai.RoleModel,
				Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{Name: l.toolName, Args: map[string]any{}}}},
			}
		} else {
			content = &genai.Content{
				Role:  genai.RoleModel,
				Parts: []*genai.Part{{Text: "done"}},
			}
		}
		yield(&adkmodel.LLMResponse{Content: content, FinishReason: genai.FinishReasonStop, TurnComplete: true}, nil)
	}
}
