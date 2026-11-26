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

package mcp

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	adkagent "google.golang.org/adk/v2/agent"
)

func TestDeclineHandler_DeclinesAndNotifies(t *testing.T) {
	t.Parallel()
	var got string
	send := func(s string) { got = s }
	h := DeclineHandler("github", send)

	res, err := h(context.Background(), &mcpsdk.ElicitRequest{
		Params: &mcpsdk.ElicitParams{Message: "what's your token?"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Action != "decline" {
		t.Errorf("action = %q, want decline", res.Action)
	}
	if !strings.Contains(got, "github") || !strings.Contains(got, "what's your token?") {
		t.Errorf("notification missing detail: %q", got)
	}
}

func TestDeclineHandler_NilSendOK(t *testing.T) {
	t.Parallel()
	h := DeclineHandler("x", nil)
	if _, err := h(context.Background(), &mcpsdk.ElicitRequest{}); err != nil {
		t.Errorf("nil-send variant should be safe: %v", err)
	}
}

func TestHandlerFor_NilReturnsNilAndNonNilForwardsServerName(t *testing.T) {
	t.Parallel()
	if h := handlerFor("lookout", nil); h != nil {
		t.Fatal("handlerFor with nil elicitor returned non-nil handler; MCP client would advertise elicitation capability")
	}

	var gotServer string
	var gotMsg string
	h := handlerFor("lookout", func(_ context.Context, serverName string, req *mcpsdk.ElicitRequest) (*mcpsdk.ElicitResult, error) {
		gotServer = serverName
		gotMsg = req.Params.Message
		return &mcpsdk.ElicitResult{Action: "accept", Content: map[string]any{"cluster": "prod"}}, nil
	})
	if h == nil {
		t.Fatal("handlerFor with non-nil elicitor returned nil")
	}
	res, err := h(context.Background(), &mcpsdk.ElicitRequest{
		Params: &mcpsdk.ElicitParams{Message: "which cluster?"},
	})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if gotServer != "lookout" || gotMsg != "which cluster?" || res.Action != "accept" {
		t.Errorf("got server=%q msg=%q res=%+v", gotServer, gotMsg, res)
	}
}

// startElicitProbeServer spins up an HTTP MCP server exposing two tools:
//   - "pick_cluster": mirrors k8s-lookout's multi-cluster behavior — checks
//     InitializeParams().Capabilities.Elicitation and emits a SEP-2322
//     InputRequests ElicitParams when advertised, or returns a non-interactive
//     answer immediately when omitted.
//   - "force_elicit": unconditionally calls Session.Elicit to verify that even
//     an unsolicited server elicitation fails immediately instead of blocking.
func startElicitProbeServer(t *testing.T) (url string, advertised *atomic.Bool) {
	t.Helper()
	var sawAdvertised atomic.Bool

	srv := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "elicit-probe", Version: "v1"}, nil)
	type emptyIn struct{}
	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "pick_cluster",
		Description: "Asks for cluster via elicitation when the client advertises it",
	}, func(_ context.Context, req *mcpsdk.CallToolRequest, _ emptyIn) (*mcpsdk.CallToolResult, any, error) {
		iparams := req.Session.InitializeParams()
		canAsk := iparams != nil && iparams.Capabilities != nil && iparams.Capabilities.Elicitation != nil
		sawAdvertised.Store(canAsk)
		if resp, ok := req.Params.InputResponses["cluster-choice"]; ok {
			if er, ok := resp.(*mcpsdk.ElicitResult); ok && er.Action == "accept" {
				if c, _ := er.Content["cluster"].(string); c != "" {
					return &mcpsdk.CallToolResult{
						Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "picked=" + c}},
					}, nil, nil
				}
			}
			return &mcpsdk.CallToolResult{
				Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "declined"}},
			}, nil, nil
		}
		if canAsk {
			return &mcpsdk.CallToolResult{
				InputRequests: mcpsdk.InputRequestMap{
					"cluster-choice": &mcpsdk.ElicitParams{
						Message: "which cluster?",
						RequestedSchema: map[string]any{
							"type": "object",
							"properties": map[string]any{
								"cluster": map[string]any{"type": "string"},
							},
						},
					},
				},
			}, nil, nil
		}
		return &mcpsdk.CallToolResult{
			Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "no-elicitation-capability"}},
		}, nil, nil
	})

	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "force_elicit",
		Description: "Calls Session.Elicit unconditionally",
	}, func(ctx context.Context, req *mcpsdk.CallToolRequest, _ emptyIn) (*mcpsdk.CallToolResult, any, error) {
		_, err := req.Session.Elicit(ctx, &mcpsdk.ElicitParams{
			Message: "forced elicit",
			RequestedSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"answer": map[string]any{"type": "string"},
				},
			},
		})
		if err != nil {
			return &mcpsdk.CallToolResult{
				Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "elicit-error: " + err.Error()}},
			}, nil, nil
		}
		return &mcpsdk.CallToolResult{
			Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "elicit-ok"}},
		}, nil, nil
	})

	handler := mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return srv }, nil)
	srvCtx, srvCancel := context.WithCancel(context.Background())
	ts := httptest.NewUnstartedServer(handler)
	ts.Config.BaseContext = func(net.Listener) context.Context { return srvCtx }
	ts.Start()
	t.Cleanup(func() {
		srvCancel()
		ts.CloseClientConnections()
		ts.Close()
	})
	return ts.URL, &sawAdvertised
}

func writeHTTPMCPConfig(t *testing.T, url string) string {
	t.Helper()
	agentsDir := filepath.Join(t.TempDir(), ".agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatalf("mkdir .agents: %v", err)
	}
	raw, err := json.Marshal(map[string]any{
		"servers": map[string]any{
			"probe": map[string]any{
				"transport": "http",
				"url":       url,
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal mcp.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentsDir, "mcp.json"), raw, 0o644); err != nil {
		t.Fatalf("write mcp.json: %v", err)
	}
	return agentsDir
}

func TestBuild_NilElicitorOmitsCapabilityAndDoesNotBlock(t *testing.T) {
	t.Parallel()
	url, advertised := startElicitProbeServer(t)
	agentsDir := writeHTTPMCPConfig(t, url)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	servers, toolsets, err := Build(ctx, agentsDir, "", nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() { CloseAll(servers) })
	if len(toolsets) != 1 {
		t.Fatalf("got %d toolsets, want 1 (servers=%+v)", len(toolsets), servers)
	}

	tools, err := toolsets[0].Tools(asReadonly(ctx))
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	byName := make(map[string]runnable, len(tools))
	for _, tl := range tools {
		byName[tl.Name()] = tl.(runnable)
	}

	callCtx := &stubToolCtx{StrictContextMock: adkagent.NewStrictContextMock(ctx)}
	res, err := byName["probe_pick_cluster"].Run(callCtx, map[string]any{})
	if err != nil {
		t.Fatalf("probe_pick_cluster: %v", err)
	}
	if advertised.Load() {
		t.Error("client with nil elicitor advertised MCP elicitation capability; want omitted")
	}
	if got, _ := res["output"].(string); !strings.Contains(got, "no-elicitation-capability") {
		t.Errorf("probe_pick_cluster output = %v, want no-elicitation-capability", res)
	}

	forceRes, err := byName["probe_force_elicit"].Run(callCtx, map[string]any{})
	if err != nil {
		t.Fatalf("probe_force_elicit: %v", err)
	}
	if got, _ := forceRes["output"].(string); !strings.Contains(got, "does not support elicitation") {
		t.Errorf("probe_force_elicit output = %v, want immediate unsupported error", forceRes)
	}
}

func TestBuild_NonNilElicitorAdvertisesCapabilityAndRoutesAnswer(t *testing.T) {
	t.Parallel()
	url, advertised := startElicitProbeServer(t)
	agentsDir := writeHTTPMCPConfig(t, url)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var elicitedServer string
	elicitor := func(_ context.Context, serverName string, _ *mcpsdk.ElicitRequest) (*mcpsdk.ElicitResult, error) {
		elicitedServer = serverName
		return &mcpsdk.ElicitResult{
			Action:  "accept",
			Content: map[string]any{"cluster": "staging"},
		}, nil
	}

	servers, toolsets, err := Build(ctx, agentsDir, "", nil, nil, elicitor, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() { CloseAll(servers) })
	if len(toolsets) != 1 {
		t.Fatalf("got %d toolsets, want 1", len(toolsets))
	}

	tools, err := toolsets[0].Tools(asReadonly(ctx))
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	byName := make(map[string]runnable, len(tools))
	for _, tl := range tools {
		byName[tl.Name()] = tl.(runnable)
	}

	callCtx := &stubToolCtx{StrictContextMock: adkagent.NewStrictContextMock(ctx)}
	res, err := byName["probe_pick_cluster"].Run(callCtx, map[string]any{})
	if err != nil {
		t.Fatalf("probe_pick_cluster: %v", err)
	}
	if !advertised.Load() {
		t.Error("client with interactive elicitor did not advertise MCP elicitation capability")
	}
	if elicitedServer != "probe" {
		t.Errorf("elicitedServer = %q, want %q", elicitedServer, "probe")
	}
	if got, _ := res["output"].(string); !strings.Contains(got, "picked=staging") {
		t.Errorf("probe_pick_cluster output = %v, want picked=staging", res)
	}
}

// TestBuild_NilElicitorRejectsUnsolicitedInputRequestsOnStatelessServer
// exercises the client-side (*mcpsdk.Client).elicit rejection path directly
// over a stateless Streamable HTTP server (protocol 2026-07-28 / SEP-2322):
// even if a server ignores client capabilities and returns InputRequests
// carrying ElicitParams, Build with a nil elicitor fails fast with an
// unsupported error instead of blocking.
func TestBuild_NilElicitorRejectsUnsolicitedInputRequestsOnStatelessServer(t *testing.T) {
	t.Parallel()

	srv := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "stateless-probe", Version: "v1"}, nil)
	type emptyIn struct{}
	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "unsolicited_elicit",
		Description: "Returns SEP-2322 InputRequests unconditionally",
	}, func(_ context.Context, _ *mcpsdk.CallToolRequest, _ emptyIn) (*mcpsdk.CallToolResult, any, error) {
		return &mcpsdk.CallToolResult{
			InputRequests: mcpsdk.InputRequestMap{
				"q": &mcpsdk.ElicitParams{
					Message: "unsolicited prompt",
					RequestedSchema: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"answer": map[string]any{"type": "string"},
						},
					},
				},
			},
		}, nil, nil
	})

	handler := mcpsdk.NewStreamableHTTPHandler(
		func(*http.Request) *mcpsdk.Server { return srv },
		&mcpsdk.StreamableHTTPOptions{Stateless: true},
	)
	srvCtx, srvCancel := context.WithCancel(context.Background())
	ts := httptest.NewUnstartedServer(handler)
	ts.Config.BaseContext = func(net.Listener) context.Context { return srvCtx }
	ts.Start()
	t.Cleanup(func() {
		srvCancel()
		ts.CloseClientConnections()
		ts.Close()
	})

	agentsDir := writeHTTPMCPConfig(t, ts.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	servers, toolsets, err := Build(ctx, agentsDir, "", nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() { CloseAll(servers) })
	if len(toolsets) != 1 {
		t.Fatalf("got %d toolsets, want 1", len(toolsets))
	}

	tools, err := toolsets[0].Tools(asReadonly(ctx))
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	var tl runnable
	for _, item := range tools {
		if item.Name() == "probe_unsolicited_elicit" {
			tl = item.(runnable)
		}
	}
	if tl == nil {
		t.Fatal("probe_unsolicited_elicit tool not found")
	}

	callCtx := &stubToolCtx{StrictContextMock: adkagent.NewStrictContextMock(ctx)}
	_, err = tl.Run(callCtx, map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "does not support elicitation") {
		t.Fatalf("probe_unsolicited_elicit err = %v, want immediate 'does not support elicitation' error", err)
	}
}
