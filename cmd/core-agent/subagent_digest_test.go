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

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	adkagent "google.golang.org/adk/v2/agent"
	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	adktool "google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/adk/v2/tool/toolconfirmation"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v3/pkg/agent"
	"github.com/go-steer/core-agent/v3/pkg/agent/background"
	"github.com/go-steer/core-agent/v3/pkg/compose"
	"github.com/go-steer/core-agent/v3/pkg/config"
	"github.com/go-steer/core-agent/v3/pkg/digest"
	"github.com/go-steer/core-agent/v3/pkg/mcp"
	"github.com/go-steer/core-agent/v3/pkg/models/mock"
	"github.com/go-steer/core-agent/v3/pkg/permissions"
	"github.com/go-steer/core-agent/v3/pkg/tools"
)

const digestTestTailMarker = "AUDIT_PAYLOAD_TAIL_SENTINEL_1604"

// memDigestStore is an in-memory digest.Store for subagent digest tests.
type memDigestStore struct {
	mu   sync.Mutex
	data map[string][]byte
	puts int
}

func (s *memDigestStore) Put(_ context.Context, callID string, raw []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data == nil {
		s.data = map[string][]byte{}
	}
	s.data[callID] = append([]byte(nil), raw...)
	s.puts++
	return nil
}

func (s *memDigestStore) Get(_ context.Context, callID string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.data[callID]
	if !ok {
		return nil, digest.ErrNotFound
	}
	return v, nil
}

func (s *memDigestStore) putCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.puts
}

// singleToolCallLLM invokes toolName once, records the tool declarations it was
// offered and the FunctionResponse ADK delivered back, then terminates.
type singleToolCallLLM struct {
	mu        sync.Mutex
	toolName  string
	calls     int
	offered   []string
	responses map[string]map[string]any
}

func (*singleToolCallLLM) Name() string { return "single-tool-call" }

func (l *singleToolCallLLM) GenerateContent(_ context.Context, req *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	l.mu.Lock()
	l.calls++
	turn := l.calls
	if turn == 1 && req != nil && req.Config != nil {
		for _, gt := range req.Config.Tools {
			for _, fd := range gt.FunctionDeclarations {
				l.offered = append(l.offered, fd.Name)
			}
		}
	}
	if req != nil {
		for _, c := range req.Contents {
			if c == nil {
				continue
			}
			for _, p := range c.Parts {
				if p != nil && p.FunctionResponse != nil {
					if l.responses == nil {
						l.responses = map[string]map[string]any{}
					}
					l.responses[p.FunctionResponse.Name] = p.FunctionResponse.Response
				}
			}
		}
	}
	target := l.toolName
	l.mu.Unlock()

	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		var content *genai.Content
		if turn == 1 {
			content = &genai.Content{
				Role: genai.RoleModel,
				Parts: []*genai.Part{{
					FunctionCall: &genai.FunctionCall{
						ID:   "call-subagent-1604",
						Name: target,
						Args: map[string]any{},
					},
				}},
			}
		} else {
			content = &genai.Content{
				Role:  genai.RoleModel,
				Parts: []*genai.Part{{Text: "audit complete"}},
			}
		}
		yield(&adkmodel.LLMResponse{
			Content:      content,
			FinishReason: genai.FinishReasonStop,
			TurnComplete: true,
		}, nil)
	}
}

func (l *singleToolCallLLM) snapshot() (offered []string, responses map[string]map[string]any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.offered), l.responses
}

// fixedLLMProvider hands every Model() call the same test LLM instance, or a
// sequence of distinct LLMs when llms is populated.
type fixedLLMProvider struct {
	mu   sync.Mutex
	llm  adkmodel.LLM
	llms []adkmodel.LLM
	idx  int
}

func (*fixedLLMProvider) Name() string { return "fixed-test" }
func (p *fixedLLMProvider) Model(context.Context, string) (adkmodel.LLM, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.llms) > 0 {
		i := p.idx
		if i >= len(p.llms) {
			i = len(p.llms) - 1
		}
		p.idx++
		return p.llms[i], nil
	}
	return p.llm, nil
}

// startLargePayloadMCPServer starts a live Streamable HTTP MCP server exposing
// k8s_audit_all returning a JSON payload well above our test digest threshold.
func startLargePayloadMCPServer(t *testing.T, readOnlyHint bool) string {
	t.Helper()
	srv := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "lookout-test", Version: "v1"}, nil)
	type in struct{}
	type out struct {
		Cluster string `json:"cluster"`
		Report  string `json:"report"`
	}
	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "k8s_audit_all",
		Description: "Return cluster posture audit report",
		Annotations: &mcpsdk.ToolAnnotations{ReadOnlyHint: readOnlyHint},
	}, func(_ context.Context, _ *mcpsdk.CallToolRequest, _ in) (*mcpsdk.CallToolResult, out, error) {
		return nil, out{
			Cluster: "demo-gke",
			Report:  strings.Repeat("finding-detail-row-", 150) + digestTestTailMarker,
		}, nil
	})
	httpSrv := httptest.NewServer(mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server {
		return srv
	}, &mcpsdk.StreamableHTTPOptions{Stateless: true, JSONResponse: true}))
	t.Cleanup(func() {
		httpSrv.CloseClientConnections()
		httpSrv.Close()
	})
	return httpSrv.URL
}

func writeRootMCPConfig(t *testing.T, dir, serverURL string, readOnly bool) {
	t.Helper()
	cfgJSON := fmt.Sprintf(`{
		"version": 1,
		"agentic_wrap": true,
		"agentic_wrap_threshold": 200,
		"servers": {
			"lookout": {
				"transport": "http",
				"url": %q,
				"read_only": %t
			}
		}
	}`, serverURL, readOnly)
	if err := os.WriteFile(filepath.Join(dir, "mcp.json"), []byte(cfgJSON), 0o600); err != nil {
		t.Fatalf("write mcp.json: %v", err)
	}
}

func runSubagentTurn(t *testing.T, sub *agent.Agent) {
	t.Helper()
	for _, err := range sub.Run(context.Background(), "run posture audit") {
		if err != nil {
			t.Fatalf("sub.Run: %v", err)
		}
	}
}

type runnableADKTool interface {
	Run(ctx adkagent.Context, args any) (map[string]any, error)
}

type stubDigestToolCtx struct {
	adkagent.StrictContextMock
	callID string
}

func (s *stubDigestToolCtx) FunctionCallID() string                               { return s.callID }
func (s *stubDigestToolCtx) Actions() *session.EventActions                       { return nil }
func (s *stubDigestToolCtx) ToolConfirmation() *toolconfirmation.ToolConfirmation { return nil }
func (s *stubDigestToolCtx) RequestConfirmation(string, any) error                { return nil }

func runToolsetTool(t *testing.T, ts adktool.Toolset, toolName, callID string) map[string]any {
	t.Helper()
	list, err := mcp.ListTools(context.Background(), ts)
	if err != nil {
		t.Fatalf("mcp.ListTools: %v", err)
	}
	for _, tl := range list {
		if tl.Name() != toolName {
			continue
		}
		rn, ok := tl.(runnableADKTool)
		if !ok {
			t.Fatalf("tool %q is not runnable (%T)", toolName, tl)
		}
		ctx := &stubDigestToolCtx{
			StrictContextMock: adkagent.NewStrictContextMock(context.Background()),
			callID:            callID,
		}
		res, err := rn.Run(ctx, map[string]any{})
		if err != nil {
			t.Fatalf("tool %q Run: %v", toolName, err)
		}
		return res
	}
	t.Fatalf("tool %q not found in toolset %q", toolName, ts.Name())
	return nil
}

func mustMarshalJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return string(b)
}

// TestBuildDeclaredSubagents_RootedWithoutRetrieveRawSkipsDigest is the #1604
// regression test: a rooted declarative subagent with tools: [] and an MCP
// server returning a payload above the digest threshold must keep zero built-in
// tools, pass capability: read_only checks, and receive the full un-pruned MCP
// response with no dead call_id / retrieve_raw reference on both sync and async
// doors.
func TestBuildDeclaredSubagents_RootedWithoutRetrieveRawSkipsDigest(t *testing.T) {
	t.Parallel()
	mcpURL := startLargePayloadMCPServer(t, true)
	root := t.TempDir()
	writeRootAGENTS(t, root, "You are the posture-audit specialist.\n")
	writeRootMCPConfig(t, root, mcpURL, true)

	store := &memDigestStore{}
	retrieveTool, err := tools.NewRetrieveRawTool(tools.RetrieveRawOptions{Store: store})
	if err != nil {
		t.Fatalf("NewRetrieveRawTool: %v", err)
	}
	gate := permissions.New(permissions.Options{Mode: permissions.ModeYolo})
	llm := &singleToolCallLLM{toolName: "lookout_k8s_audit_all"}

	cfg := &config.Config{
		Model: config.ModelConfig{Provider: "fixed-test", Name: "fixed"},
		Subagents: []config.SubagentSpec{{
			Name:        "posture-audit",
			Description: "Cluster posture audit",
			Root:        root,
			Tools:       []string{},
			Capability:  config.CapabilityReadOnly,
		}},
	}
	surface := parentSurface{
		builtinTools: []adktool.Tool{fakeTool{"read_file"}, retrieveTool},
	}
	deps := subagentDeps{
		gate:       gate,
		digestOpts: &mcp.DigestOptions{Store: store, Threshold: 200},
		interp:     identityInterp,
		send:       discard,
		rootBase:   t.TempDir(),
	}

	subs, templates, servers, err := buildDeclaredSubagents(
		context.Background(), cfg, &fixedLLMProvider{llm: llm}, t.TempDir(), surface, deps,
	)
	defer mcp.CloseAll(servers)
	if err != nil {
		t.Fatalf("buildDeclaredSubagents: %v", err)
	}
	if len(subs) != 1 || len(templates) != 1 {
		t.Fatalf("got %d subs / %d templates, want 1 each", len(subs), len(templates))
	}

	// Contract check: tools: [] must still grant zero built-in tools.
	if got := toolNames(subs[0].Tools()); len(got) != 0 {
		t.Errorf("sync subagent built-in tools = %v, want [] (tools: [] contract must not inject retrieve_raw)", got)
	}
	if got := toolNames(templates[0].Tools); len(got) != 0 {
		t.Errorf("async template built-in tools = %v, want []", got)
	}

	// Sync door: run a real turn calling lookout_k8s_audit_all and inspect what ADK hands the model.
	runSubagentTurn(t, subs[0])
	offered, responses := llm.snapshot()
	if slices.Contains(offered, retrieveRawToolName) {
		t.Errorf("model was offered retrieve_raw despite tools: [] (offered=%v)", offered)
	}
	syncResp, ok := responses["lookout_k8s_audit_all"]
	if !ok {
		t.Fatalf("model never received FunctionResponse for lookout_k8s_audit_all (got %+v)", responses)
	}
	assertUndigestedMCPResponse(t, "sync subagent", syncResp)

	// Async template door: invoke the template's MCP toolset directly.
	if len(templates[0].Toolsets) != 1 {
		t.Fatalf("template toolsets = %d, want 1", len(templates[0].Toolsets))
	}
	asyncResp := runToolsetTool(t, templates[0].Toolsets[0], "lookout_k8s_audit_all", "call-async-1604")
	assertUndigestedMCPResponse(t, "async template", asyncResp)

	if puts := store.putCount(); puts != 0 {
		t.Errorf("digest store received %d Put(s) for subagent without retrieve_raw, want 0", puts)
	}
}

func assertUndigestedMCPResponse(t *testing.T, label string, resp map[string]any) {
	t.Helper()
	if _, hasDigest := resp["digest"]; hasDigest {
		t.Errorf("%s response was unexpectedly digested: %+v", label, resp)
	}
	if _, hasCallID := resp["call_id"]; hasCallID {
		t.Errorf("%s response stamped a dead call_id without retrieve_raw: %+v", label, resp)
	}
	if _, hasMethod := resp["method"]; hasMethod {
		t.Errorf("%s response carried digest method without retrieve_raw: %+v", label, resp)
	}
	encoded := mustMarshalJSON(t, resp)
	if !strings.Contains(encoded, digestTestTailMarker) {
		t.Errorf("%s response truncated payload; missing tail sentinel %q in %s", label, digestTestTailMarker, encoded)
	}
	if strings.Contains(encoded, "retrieve_raw") {
		t.Errorf("%s response referenced retrieve_raw despite lacking the tool: %s", label, encoded)
	}
}

// TestBuildDeclaredSubagents_RootedWithRetrieveRawKeepsDigest verifies the
// positive half of #1604: rooted subagents that DO include retrieve_raw (either
// explicitly in tools: or by inheriting tools: nil) continue to receive
// digested MCP responses with a working call_id backed by the shared store.
func TestBuildDeclaredSubagents_RootedWithRetrieveRawKeepsDigest(t *testing.T) {
	t.Parallel()
	mcpURL := startLargePayloadMCPServer(t, true)

	cases := []struct {
		name      string
		specTools []string
	}{
		{name: "explicit retrieve_raw", specTools: []string{retrieveRawToolName}},
		{name: "inherited tools nil", specTools: nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			writeRootAGENTS(t, root, "You are the posture-audit specialist.\n")
			writeRootMCPConfig(t, root, mcpURL, true)

			store := &memDigestStore{}
			retrieveTool, err := tools.NewRetrieveRawTool(tools.RetrieveRawOptions{Store: store})
			if err != nil {
				t.Fatalf("NewRetrieveRawTool: %v", err)
			}
			gate := permissions.New(permissions.Options{Mode: permissions.ModeYolo})
			llm := &singleToolCallLLM{toolName: "lookout_k8s_audit_all"}

			cfg := &config.Config{
				Model: config.ModelConfig{Provider: "fixed-test", Name: "fixed"},
				Subagents: []config.SubagentSpec{{
					Name:        "posture-audit",
					Description: "Cluster posture audit",
					Root:        root,
					Tools:       tc.specTools,
					Capability:  config.CapabilityReadOnly,
				}},
			}
			surface := parentSurface{
				builtinTools: []adktool.Tool{newFakeBuiltinFunctionTool(t, "read_file"), retrieveTool},
			}
			deps := subagentDeps{
				gate:       gate,
				digestOpts: &mcp.DigestOptions{Store: store, Threshold: 200},
				interp:     identityInterp,
				send:       discard,
				rootBase:   t.TempDir(),
			}

			subs, templates, servers, err := buildDeclaredSubagents(
				context.Background(), cfg, &fixedLLMProvider{llm: llm}, t.TempDir(), surface, deps,
			)
			defer mcp.CloseAll(servers)
			if err != nil {
				t.Fatalf("buildDeclaredSubagents: %v", err)
			}
			if !hasToolNamed(subs[0].Tools(), retrieveRawToolName) {
				t.Fatalf("sync subagent missing retrieve_raw: %v", toolNames(subs[0].Tools()))
			}
			if !hasToolNamed(templates[0].Tools, retrieveRawToolName) {
				t.Fatalf("async template missing retrieve_raw: %v", toolNames(templates[0].Tools))
			}

			runSubagentTurn(t, subs[0])
			_, responses := llm.snapshot()
			syncResp, ok := responses["lookout_k8s_audit_all"]
			if !ok {
				t.Fatalf("missing FunctionResponse for lookout_k8s_audit_all: %+v", responses)
			}
			assertDigestedMCPResponse(t, "sync subagent", syncResp, store)

			asyncResp := runToolsetTool(t, templates[0].Toolsets[0], "lookout_k8s_audit_all", "call-async-digested")
			assertDigestedMCPResponse(t, "async template", asyncResp, store)
		})
	}
}

func assertDigestedMCPResponse(t *testing.T, label string, resp map[string]any, store *memDigestStore) {
	t.Helper()
	if got, _ := resp["method"].(string); got != digest.MethodStructuralJSON {
		t.Errorf("%s method = %q, want %q (resp=%+v)", label, got, digest.MethodStructuralJSON, resp)
	}
	if _, ok := resp["digest"].(string); !ok {
		t.Errorf("%s missing digest field: %+v", label, resp)
	}
	callID, _ := resp["call_id"].(string)
	if callID == "" {
		t.Fatalf("%s missing call_id: %+v", label, resp)
	}
	raw, err := store.Get(context.Background(), callID)
	if err != nil {
		t.Fatalf("%s store.Get(%q): %v", label, callID, err)
	}
	if !strings.Contains(string(raw), digestTestTailMarker) {
		t.Errorf("%s stored raw payload missing tail sentinel: %s", label, string(raw))
	}
}

// TestBuildDeclaredSubagents_RootedWithoutRetrieveRaw_ReadOnlyRejectsMutatingMCP
// ensures that disabling MCP digest wrapping preserves read_only capability
// enforcement on rooted MCP servers.
func TestBuildDeclaredSubagents_RootedWithoutRetrieveRaw_ReadOnlyRejectsMutatingMCP(t *testing.T) {
	t.Parallel()
	mcpURL := startLargePayloadMCPServer(t, false)
	root := t.TempDir()
	writeRootAGENTS(t, root, "Mutating test persona.\n")
	writeRootMCPConfig(t, root, mcpURL, false)

	store := &memDigestStore{}
	retrieveTool, err := tools.NewRetrieveRawTool(tools.RetrieveRawOptions{Store: store})
	if err != nil {
		t.Fatalf("NewRetrieveRawTool: %v", err)
	}
	cfg := &config.Config{
		Model: config.ModelConfig{Provider: mock.ProviderEcho, Name: "echo"},
		Subagents: []config.SubagentSpec{{
			Name:        "posture-audit",
			Description: "Should reject mutating MCP tool",
			Root:        root,
			Tools:       []string{},
			Capability:  config.CapabilityReadOnly,
		}},
	}
	deps := subagentDeps{
		gate:       permissions.New(permissions.Options{Mode: permissions.ModeYolo}),
		digestOpts: &mcp.DigestOptions{Store: store, Threshold: 200},
		interp:     identityInterp,
		send:       discard,
		rootBase:   t.TempDir(),
	}
	_, _, servers, err := buildDeclaredSubagents(
		context.Background(), cfg, mock.NewEcho(), t.TempDir(),
		parentSurface{builtinTools: []adktool.Tool{retrieveTool}}, deps,
	)
	defer mcp.CloseAll(servers)
	if err == nil || !strings.Contains(err.Error(), "lookout_k8s_audit_all") {
		t.Fatalf("expected capability: read_only error naming lookout_k8s_audit_all, got %v", err)
	}
}

// TestBuildDeclaredSubagents_InlineWithoutRetrieveRawUsesUndigestedParentMCP
// verifies that inline (non-rooted) subagents sharing the parent's started MCP
// servers also skip digesting when their resolved tools omit retrieve_raw, while
// the parent and subagents that include retrieve_raw keep digesting intact.
func TestBuildDeclaredSubagents_InlineWithoutRetrieveRawUsesUndigestedParentMCP(t *testing.T) {
	t.Parallel()
	mcpURL := startLargePayloadMCPServer(t, true)
	agentsDir := t.TempDir()
	writeRootMCPConfig(t, agentsDir, mcpURL, true)

	store := &memDigestStore{}
	retrieveTool, err := tools.NewRetrieveRawTool(tools.RetrieveRawOptions{Store: store})
	if err != nil {
		t.Fatalf("NewRetrieveRawTool: %v", err)
	}
	gate := permissions.New(permissions.Options{Mode: permissions.ModeYolo})
	parentServers, _, err := mcp.Build(
		mcp.WithTrustedAgentsDir(context.Background(), agentsDir),
		agentsDir, "", discard, gate, nil,
		&mcp.DigestOptions{Store: store, Threshold: 200},
	)
	defer mcp.CloseAll(parentServers)
	if err != nil {
		t.Fatalf("parent mcp.Build: %v", err)
	}
	if len(parentServers) != 1 || parentServers[0].Toolset() == nil {
		t.Fatalf("unexpected parent servers: %+v", parentServers)
	}
	parentSrv := parentServers[0]

	// Parent's own Toolset() must remain digested.
	parentResp := runToolsetTool(t, parentSrv.Toolset(), "lookout_k8s_audit_all", "call-parent-1604")
	assertDigestedMCPResponse(t, "parent", parentResp, store)

	surface := parentSurface{
		builtinTools: []adktool.Tool{retrieveTool},
		mcpToolsets: []namedToolset{{
			name:              parentSrv.Name,
			toolset:           parentSrv.Toolset(),
			undigestedToolset: parentSrv.UndigestedToolset(),
			infos:             parentSrv.ToolInfos,
		}},
	}
	cfg := &config.Config{
		Model: config.ModelConfig{Provider: "fixed-test", Name: "fixed"},
		Subagents: []config.SubagentSpec{
			{
				Name:       "inline-no-tools",
				Tools:      []string{},
				MCP:        []string{"lookout"},
				Capability: config.CapabilityReadOnly,
			},
			{
				Name:       "inline-with-retrieve",
				Tools:      []string{retrieveRawToolName},
				MCP:        []string{"lookout"},
				Capability: config.CapabilityReadOnly,
			},
			{
				Name:       "inline-filtered-no-tools",
				Tools:      []string{},
				MCP:        []string{"lookout"},
				MCPTools:   []config.MCPToolAllowlist{{Server: "lookout", Tools: []string{"k8s_audit_all"}}},
				Capability: config.CapabilityReadOnly,
			},
		},
	}
	inlineNoToolsLLM := &singleToolCallLLM{toolName: "lookout_k8s_audit_all"}
	inlineWithRetLLM := &singleToolCallLLM{toolName: "lookout_k8s_audit_all"}
	inlineFilteredLLM := &singleToolCallLLM{toolName: "lookout_k8s_audit_all"}
	subs, templates, _, err := buildDeclaredSubagents(
		context.Background(), cfg,
		&fixedLLMProvider{llms: []adkmodel.LLM{inlineNoToolsLLM, inlineWithRetLLM, inlineFilteredLLM}},
		t.TempDir(), surface,
		subagentDeps{gate: gate, interp: identityInterp, send: discard},
	)
	if err != nil {
		t.Fatalf("buildDeclaredSubagents: %v", err)
	}
	if len(subs) != 3 || len(templates) != 3 {
		t.Fatalf("got %d subs / %d templates, want 3 each", len(subs), len(templates))
	}

	// Sync doors for inline-no-tools, inline-with-retrieve, and mcp_tools-filtered inline subagents.
	runSubagentTurn(t, subs[0])
	_, syncNoToolsResps := inlineNoToolsLLM.snapshot()
	assertUndigestedMCPResponse(t, "sync inline without retrieve_raw", syncNoToolsResps["lookout_k8s_audit_all"])

	runSubagentTurn(t, subs[1])
	_, syncWithRetResps := inlineWithRetLLM.snapshot()
	assertDigestedMCPResponse(t, "sync inline with retrieve_raw", syncWithRetResps["lookout_k8s_audit_all"], store)

	runSubagentTurn(t, subs[2])
	_, syncFilteredResps := inlineFilteredLLM.snapshot()
	assertUndigestedMCPResponse(t, "sync inline mcp_tools without retrieve_raw", syncFilteredResps["lookout_k8s_audit_all"])

	// Async doors + concurrent digested/undigested access under -race.
	var wg sync.WaitGroup
	var noToolsResp, withRetResp, filteredResp map[string]any
	wg.Add(3)
	go func() {
		defer wg.Done()
		noToolsResp = runToolsetTool(t, templates[0].Toolsets[0], "lookout_k8s_audit_all", "call-inline-raw")
	}()
	go func() {
		defer wg.Done()
		withRetResp = runToolsetTool(t, templates[1].Toolsets[0], "lookout_k8s_audit_all", "call-inline-digested")
	}()
	go func() {
		defer wg.Done()
		filteredResp = runToolsetTool(t, templates[2].Toolsets[0], "lookout_k8s_audit_all", "call-inline-filtered")
	}()
	wg.Wait()
	assertUndigestedMCPResponse(t, "async inline without retrieve_raw", noToolsResp)
	assertDigestedMCPResponse(t, "async inline with retrieve_raw", withRetResp, store)
	assertUndigestedMCPResponse(t, "async inline mcp_tools without retrieve_raw", filteredResp)
}

// TestBuildDeclaredSubagents_BuiltinSurveyToolsSkipDigestWithoutRetrieveRaw
// verifies that pre-digest survey built-ins (grep, glob, list_dir,
// read_many_files) are handed to subagents lacking retrieve_raw so they do not
// emit a dead call_id either, while subagents holding retrieve_raw keep the
// digested wrapper.
func TestBuildDeclaredSubagents_BuiltinSurveyToolsSkipDigestWithoutRetrieveRaw(t *testing.T) {
	t.Parallel()
	store := &memDigestStore{}
	retrieveTool, err := tools.NewRetrieveRawTool(tools.RetrieveRawOptions{Store: store})
	if err != nil {
		t.Fatalf("NewRetrieveRawTool: %v", err)
	}
	rawGrep, err := functiontool.New(
		functiontool.Config{Name: "grep", Description: "large grep"},
		func(_ adkagent.Context, _ struct{}) (map[string]any, error) {
			return map[string]any{
				"matches": strings.Repeat("match-line-entry-", 150) + digestTestTailMarker,
			}, nil
		},
	)
	if err != nil {
		t.Fatalf("functiontool.New(grep): %v", err)
	}
	undigested := []adktool.Tool{rawGrep, retrieveTool}
	digested := tools.NewDigester(&tools.DigestOptions{Store: store, Threshold: 200}).Wrap(undigested)

	cfg := &config.Config{
		Model: config.ModelConfig{Provider: mock.ProviderEcho, Name: "echo"},
		Subagents: []config.SubagentSpec{
			{Name: "grep-no-retrieve", Tools: []string{"grep"}, MCP: []string{}, Capability: config.CapabilityReadOnly},
			{Name: "grep-with-retrieve", Tools: []string{"grep", retrieveRawToolName}, MCP: []string{}, Capability: config.CapabilityReadOnly},
		},
	}
	surface := parentSurface{
		builtinTools:           digested,
		undigestedBuiltinTools: undigested,
	}
	subs, templates, _, err := buildDeclaredSubagents(
		context.Background(), cfg, mock.NewEcho(), t.TempDir(), surface, testDeps(),
	)
	if err != nil {
		t.Fatalf("buildDeclaredSubagents: %v", err)
	}

	callCtx := &stubDigestToolCtx{
		StrictContextMock: adkagent.NewStrictContextMock(context.Background()),
		callID:            "call-grep-1604",
	}

	// Subagent without retrieve_raw gets the un-pruned grep response on both doors.
	for _, tc := range []struct {
		label string
		ts    []adktool.Tool
	}{
		{"sync grep-no-retrieve", subs[0].Tools()},
		{"async grep-no-retrieve", templates[0].Tools},
	} {
		resp, err := tc.ts[0].(runnableADKTool).Run(callCtx, map[string]any{})
		if err != nil {
			t.Fatalf("%s Run: %v", tc.label, err)
		}
		assertUndigestedMCPResponse(t, tc.label, resp)
	}

	// Subagent with retrieve_raw gets the digested grep response with call_id.
	resp, err := subs[1].Tools()[0].(runnableADKTool).Run(callCtx, map[string]any{})
	if err != nil {
		t.Fatalf("sync grep-with-retrieve Run: %v", err)
	}
	assertDigestedMCPResponse(t, "sync grep-with-retrieve", resp, store)
}

// uncomparableFakeTool has a slice field (uncomparable dynamic type) to prove
// undigestCatalog never panics when a SessionCustomizer injects arbitrary tool
// implementations into scope.Tools.
type uncomparableFakeTool struct {
	name string
	tags []string
}

func (u uncomparableFakeTool) Name() string        { return u.name }
func (u uncomparableFakeTool) Description() string { return "custom uncomparable tool" }
func (u uncomparableFakeTool) IsLongRunning() bool { return false }

// TestSessionBackgroundRecipe_CatalogUsesUndigestedSurveyTools verifies that
// multi-session background managers receive undigested built-in survey tools in
// their ad-hoc catalog (matching single-session bgMgr), while the session
// parent retains digested tools and SessionCustomizer-replaced tools stay
// untouched (#1604).
func TestSessionBackgroundRecipe_CatalogUsesUndigestedSurveyTools(t *testing.T) {
	t.Parallel()
	store := &memDigestStore{}
	retrieveTool, err := tools.NewRetrieveRawTool(tools.RetrieveRawOptions{Store: store})
	if err != nil {
		t.Fatalf("NewRetrieveRawTool: %v", err)
	}
	rawGrep, err := functiontool.New(
		functiontool.Config{Name: "grep", Description: "large grep"},
		func(_ adkagent.Context, _ struct{}) (map[string]any, error) {
			return map[string]any{
				"matches": strings.Repeat("match-line-entry-", 150) + digestTestTailMarker,
			}, nil
		},
	)
	if err != nil {
		t.Fatalf("functiontool.New(grep): %v", err)
	}
	undigested := []adktool.Tool{rawGrep, retrieveTool}
	digested := tools.NewDigester(&tools.DigestOptions{Store: store, Threshold: 200}).Wrap(undigested)

	adhocLLM := &singleToolCallLLM{toolName: "grep"}
	r, daemonSpawn := testRecipe(t, nil)
	r.provider = &fixedLLMProvider{llm: adhocLLM}
	r.allowAdhoc = true
	r.undigestedCatalog = buildUndigestedToolPairs(digested, undigested)

	sessionTools := append(slices.Clone(digested), daemonSpawn...)
	sub, err := r.factory()(compose.SessionScope{
		SessionID: "sid-digest",
		Gate:      permissions.New(permissions.Options{Mode: permissions.ModeYolo}),
		ModelName: "fixed",
		Tools:     sessionTools,
	})
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	t.Cleanup(sub.Close)

	// Session parent keeps the digested grep wrapper (parent has retrieve_raw).
	callCtx := &stubDigestToolCtx{
		StrictContextMock: adkagent.NewStrictContextMock(context.Background()),
		callID:            "call-parent-grep",
	}
	var parentGrep adktool.Tool
	for _, tl := range sub.Tools {
		if tl.Name() == "grep" {
			parentGrep = tl
			break
		}
	}
	if parentGrep == nil {
		t.Fatal("session parent missing grep tool")
	}
	parentResp, err := parentGrep.(runnableADKTool).Run(callCtx, map[string]any{})
	if err != nil {
		t.Fatalf("parent grep Run: %v", err)
	}
	assertDigestedMCPResponse(t, "multi-session parent grep", parentResp, store)

	// Ad-hoc spawn omitting retrieve_raw receives the undigested grep from catalog.
	mgr := sub.Manager.(*background.Manager)
	if _, err := agent.New(mustEchoModel(t), agent.WithBackgroundManager(mgr)); err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	h, err := mgr.Spawn(context.Background(), "", background.Spec{
		Name:         "adhoc-grep",
		Goal:         "search files",
		SystemPrompt: "Search files and report findings.",
		Tools:        []string{"grep"},
		Budgets:      background.Budgets{MaxTurns: 2},
	})
	if err != nil {
		t.Fatalf("mgr.Spawn: %v", err)
	}
	select {
	case <-h.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("ad-hoc background subagent did not finish")
	}
	if err := h.Err(); err != nil {
		t.Fatalf("ad-hoc subagent err: %v", err)
	}
	_, adhocResps := adhocLLM.snapshot()
	grepResp, ok := adhocResps["grep"]
	if !ok {
		t.Fatalf("ad-hoc subagent never received grep FunctionResponse: %+v", adhocResps)
	}
	assertUndigestedMCPResponse(t, "multi-session ad-hoc grep", grepResp)

	// SessionCustomizer-replaced tool of the same name (even an uncomparable type)
	// must pass through untouched without panicking or being overwritten.
	customGrep := uncomparableFakeTool{name: "grep", tags: []string{"custom"}}
	gotCatalog := undigestCatalog([]adktool.Tool{customGrep}, r.undigestedCatalog)
	if len(gotCatalog) != 1 || gotCatalog[0].Description() != "custom uncomparable tool" {
		t.Errorf("undigestCatalog overwrote SessionCustomizer tool: %+v", gotCatalog)
	}
}

// TestBuildDeclaredSubagents_UnownedMemoryClosedOnError verifies that when a
// caller omits deps.onClose and a later subagent fails validation/wiring, any
// standalone memory backend opened by an earlier rooted subagent is still
// closed on the error path.
func TestBuildDeclaredSubagents_UnownedMemoryClosedOnError(t *testing.T) {
	t.Parallel()
	rootDir := t.TempDir()
	agentsDir := filepath.Join(rootDir, ".agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentsDir, "config.json"), []byte(`{
		"version": 1,
		"memory": {
			"ref": "inmemory://unowned-close-test",
			"namespace": "isolated",
			"agent": {"id": "specialist", "mode": "rw"}
		}
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	writeRootAGENTS(t, rootDir, "Standalone memory specialist.\n")

	cfg := &config.Config{
		Model: config.ModelConfig{Provider: mock.ProviderEcho, Name: "echo"},
		Subagents: []config.SubagentSpec{
			{Name: "good-mem", Root: rootDir, Tools: []string{}},
			{Name: "bad-tools", Tools: []string{"nonexistent_tool"}},
		},
	}
	_, _, servers, err := buildDeclaredSubagents(
		context.Background(), cfg, mock.NewEcho(), t.TempDir(), parentSurface{}, testDeps(),
	)
	defer mcp.CloseAll(servers)
	if err == nil || !strings.Contains(err.Error(), "nonexistent_tool") {
		t.Fatalf("expected unknown tool error for second subagent, got %v", err)
	}
}
