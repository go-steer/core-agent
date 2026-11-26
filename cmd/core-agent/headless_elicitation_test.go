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
	"flag"
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
	"google.golang.org/adk/v2/session"
	adktool "google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/toolconfirmation"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v3/pkg/runner"
)

// elicitTestToolCtx is a minimal adkagent.Context backed by StrictContextMock
// so tests can invoke MCP tools built by stepRegisterCoreMetricsAndBuildMCP.
type elicitTestToolCtx struct {
	adkagent.StrictContextMock
}

func (e *elicitTestToolCtx) UserContent() *genai.Content                          { return nil }
func (e *elicitTestToolCtx) InvocationID() string                                 { return "test-inv" }
func (e *elicitTestToolCtx) AgentName() string                                    { return "test-agent" }
func (e *elicitTestToolCtx) ReadonlyState() session.ReadonlyState                 { return nil }
func (e *elicitTestToolCtx) UserID() string                                       { return "test-user" }
func (e *elicitTestToolCtx) AppName() string                                      { return "test-app" }
func (e *elicitTestToolCtx) SessionID() string                                    { return "test-session" }
func (e *elicitTestToolCtx) Branch() string                                       { return "" }
func (e *elicitTestToolCtx) Artifacts() adkagent.Artifacts                        { return nil }
func (e *elicitTestToolCtx) State() session.State                                 { return nil }
func (e *elicitTestToolCtx) FunctionCallID() string                               { return "call-1" }
func (e *elicitTestToolCtx) Actions() *session.EventActions                       { return nil }
func (e *elicitTestToolCtx) ToolConfirmation() *toolconfirmation.ToolConfirmation { return nil }
func (e *elicitTestToolCtx) RequestConfirmation(string, any) error                { return nil }

type runnableMCPTool interface {
	Run(ctx adkagent.Context, args any) (map[string]any, error)
}

// startCLIProbeMCPServer spins up a real HTTP MCP server mimicking k8s-lookout:
// when the client advertises Capabilities.Elicitation during initialize,
// calling pick_cluster emits a SEP-2322 InputRequests ElicitParams (which
// blocks forever on an undrained TUI elicitor channel). When the client omits
// elicitation capability, pick_cluster returns immediately.
//
// When checkOnly is true, pick_cluster records whether elicitation was
// advertised and returns immediately without issuing an InputRequests
// round-trip (used by interactive-mode tests that verify capability wiring
// without starting the Bubble Tea event loop).
func startCLIProbeMCPServer(t *testing.T, checkOnly bool) (string, *atomic.Bool) {
	t.Helper()
	var advertised atomic.Bool

	srv := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "cli-elicit-probe", Version: "v1"}, nil)
	type emptyIn struct{}
	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "pick_cluster",
		Description: "Tests whether MCP client advertises elicitation",
	}, func(_ context.Context, req *mcpsdk.CallToolRequest, _ emptyIn) (*mcpsdk.CallToolResult, any, error) {
		iparams := req.Session.InitializeParams()
		canAsk := iparams != nil && iparams.Capabilities != nil && iparams.Capabilities.Elicitation != nil
		advertised.Store(canAsk)
		if checkOnly {
			text := "advertised=false"
			if canAsk {
				text = "advertised=true"
			}
			return &mcpsdk.CallToolResult{
				Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: text}},
			}, nil, nil
		}
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
	return ts.URL, &advertised
}

func invokeMCPTool(t *testing.T, ctx context.Context, toolset adktool.Toolset, name string) (map[string]any, error) {
	t.Helper()
	tctx := &elicitTestToolCtx{StrictContextMock: adkagent.NewStrictContextMock(ctx)}
	tools, err := toolset.Tools(tctx)
	if err != nil {
		return nil, err
	}
	for _, tl := range tools {
		if tl.Name() == name {
			return tl.(runnableMCPTool).Run(tctx, map[string]any{})
		}
	}
	t.Fatalf("tool %q not found in toolset", name)
	return nil, nil
}

func writeProbeWorkspace(t *testing.T, parentURL, subagentURL string) (agentsDir, cfgPath string) {
	t.Helper()
	tmp := t.TempDir()
	agentsDir = filepath.Join(tmp, ".agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatalf("mkdir .agents: %v", err)
	}
	cfg := map[string]any{
		"model": map[string]any{
			"provider": "echo",
			"name":     "echo-test",
		},
	}
	if subagentURL != "" {
		subDir := filepath.Join(agentsDir, "subagents", "auditor")
		if err := os.MkdirAll(subDir, 0o755); err != nil {
			t.Fatalf("mkdir subagent root: %v", err)
		}
		subMCP, err := json.Marshal(map[string]any{
			"servers": map[string]any{
				"subprobe": map[string]any{
					"transport": "http",
					"url":       subagentURL,
				},
			},
		})
		if err != nil {
			t.Fatalf("marshal subagent mcp.json: %v", err)
		}
		if err := os.WriteFile(filepath.Join(subDir, "mcp.json"), subMCP, 0o644); err != nil {
			t.Fatalf("write subagent mcp.json: %v", err)
		}
		cfg["subagents"] = []map[string]any{
			{
				"name":        "auditor",
				"description": "audit specialist",
				"root":        "subagents/auditor",
			},
		}
	}
	cfgRaw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config.json: %v", err)
	}
	cfgPath = filepath.Join(agentsDir, "config.json")
	if err := os.WriteFile(cfgPath, cfgRaw, 0o644); err != nil {
		t.Fatalf("write config.json: %v", err)
	}
	mcpRaw, err := json.Marshal(map[string]any{
		"servers": map[string]any{
			"probe": map[string]any{
				"transport": "http",
				"url":       parentURL,
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal mcp.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentsDir, "mcp.json"), mcpRaw, 0o644); err != nil {
		t.Fatalf("write mcp.json: %v", err)
	}
	return agentsDir, cfgPath
}

func runStepsThrough(t *testing.T, s *runState, lastStep string) {
	t.Helper()
	for _, st := range startupSteps {
		if code := st.run(s); code != runner.ExitOK {
			t.Fatalf("startup step %q failed with exit code %d", st.name, code)
		}
		if st.name == lastStep {
			return
		}
	}
	t.Fatalf("startup step %q not found", lastStep)
}

func TestWillLaunchTUI_ExecutionModes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		driver   driverOpts
		attach   attachOpts
		stdinTTY bool
		want     bool
	}{
		{
			name:     "interactive TTY default",
			stdinTTY: true,
			want:     true,
		},
		{
			name:     "interactive TTY with initial prompt (-i)",
			driver:   driverOpts{initialPrompt: "hello"},
			stdinTTY: true,
			want:     true,
		},
		{
			name:     "interactive TTY with attach listener (without --no-repl)",
			attach:   attachOpts{Listen: "127.0.0.1:7777"},
			stdinTTY: true,
			want:     true,
		},
		{
			name:     "daemon --no-repl with TTY stdin",
			driver:   driverOpts{noREPL: true},
			attach:   attachOpts{Listen: "127.0.0.1:7777"},
			stdinTTY: true,
			want:     false,
		},
		{
			name:     "daemon --no-repl with non-TTY stdin",
			driver:   driverOpts{noREPL: true},
			attach:   attachOpts{Listen: "127.0.0.1:7777"},
			stdinTTY: false,
			want:     false,
		},
		{
			name:     "one-shot -p with TTY stdin",
			driver:   driverOpts{prompt: "audit cluster"},
			stdinTTY: true,
			want:     false,
		},
		{
			name:     "explicit --no-tui with TTY stdin",
			driver:   driverOpts{noTUI: true},
			stdinTTY: true,
			want:     false,
		},
		{
			name:     "piped / non-TTY stdin without flags",
			stdinTTY: false,
			want:     false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newRunState(context.Background(), func() {}, runOptions{
				driver: tc.driver,
				attach: tc.attach,
			})
			tty := tc.stdinTTY
			s.stdinIsTTY = func() bool { return tty }
			if got := s.willLaunchTUI(); got != tc.want {
				t.Errorf("willLaunchTUI() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestStepBuildMCP_HeadlessModesOmitElicitationAndDoNotBlock is the #1606
// regression test: in default (!no_tui) and slim (no_tui) builds alike,
// running in any non-TUI mode (--no-repl, -p, --no-tui, or non-TTY stdin)
// must NOT wire the TUI MCP elicitor, must NOT advertise MCP elicitation to
// either parent or rooted subagent MCP servers, and must return immediately
// instead of blocking on an undrained TUI channel.
//
// Not parallel: isolates HOME via t.Setenv so startup steps neither read the
// user's real ~/.agents/mcp.json nor write to ~/.core-agent/trusted_workspaces.json.
func TestStepBuildMCP_HeadlessModesOmitElicitationAndDoNotBlock(t *testing.T) {
	modes := []struct {
		name     string
		driver   driverOpts
		stdinTTY bool
	}{
		{
			name:     "no-repl daemon even with TTY stdin",
			driver:   driverOpts{noREPL: true, yolo: true},
			stdinTTY: true,
		},
		{
			name:     "one-shot -p even with TTY stdin",
			driver:   driverOpts{prompt: "check posture", yolo: true},
			stdinTTY: true,
		},
		{
			name:     "explicit --no-tui with TTY stdin",
			driver:   driverOpts{noTUI: true, yolo: true},
			stdinTTY: true,
		},
		{
			name:     "non-TTY piped stdin",
			driver:   driverOpts{yolo: true},
			stdinTTY: false,
		},
	}

	for _, tc := range modes {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			parentURL, parentAdvertised := startCLIProbeMCPServer(t, false)
			subURL, subAdvertised := startCLIProbeMCPServer(t, false)
			agentsDir, cfgPath := writeProbeWorkspace(t, parentURL, subURL)

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			s := newRunState(ctx, cancel, runOptions{
				driver: tc.driver,
				workspace: workspaceOpts{
					cfgPath:       cfgPath,
					agentsDirFlag: agentsDir,
				},
				model: modelOpts{
					noPricingRefresh: true,
				},
				toolCatalog: toolCatalogOpts{
					noBuiltinTools:     true,
					noBackgroundAgents: true,
					noMCPDigest:        true,
				},
				guardrails: guardrailOpts{
					trust: true,
				},
				prompt: promptOpts{
					quiet: true,
				},
			})
			s.flagSet = flag.NewFlagSet("test", flag.ContinueOnError)
			tty := tc.stdinTTY
			s.stdinIsTTY = func() bool { return tty }
			defer s.close()

			runStepsThrough(t, s, "build-declarative-subagents")

			if s.mcpElicitor != nil {
				t.Errorf("s.mcpElicitor != nil in %s mode; want nil", tc.name)
			}
			if s.tuiElicitor != nil {
				t.Errorf("s.tuiElicitor != nil in %s mode; want nil", tc.name)
			}
			if len(s.mcpToolsets) != 1 {
				t.Fatalf("got %d parent mcpToolsets, want 1", len(s.mcpToolsets))
			}

			// Bound each tool invocation tightly: on the pre-fix code, the
			// server sees Capabilities.Elicitation != nil and sends an
			// elicitation request that parks forever on coretui.Elicitor.
			callCtx, callCancel := context.WithTimeout(ctx, 1*time.Second)
			defer callCancel()
			res, err := invokeMCPTool(t, callCtx, s.mcpToolsets[0], "probe_pick_cluster")
			if err != nil {
				t.Fatalf("parent probe_pick_cluster failed (blocked on elicitor?): %v", err)
			}
			if parentAdvertised.Load() {
				t.Error("parent MCP client advertised elicitation in headless/non-TUI mode")
			}
			if got, _ := res["output"].(string); !strings.Contains(got, "no-elicitation-capability") {
				t.Errorf("parent probe_pick_cluster output = %v, want no-elicitation-capability", res)
			}

			if len(s.subagentServers) != 1 || s.subagentServers[0].Toolset() == nil {
				t.Fatalf("got subagentServers=%+v, want 1 healthy rooted subagent MCP server", s.subagentServers)
			}
			subRes, err := invokeMCPTool(t, callCtx, s.subagentServers[0].Toolset(), "subprobe_pick_cluster")
			if err != nil {
				t.Fatalf("subagent subprobe_pick_cluster failed (blocked on elicitor?): %v", err)
			}
			if subAdvertised.Load() {
				t.Error("rooted subagent MCP client advertised elicitation in headless/non-TUI mode")
			}
			if got, _ := subRes["output"].(string); !strings.Contains(got, "no-elicitation-capability") {
				t.Errorf("subagent subprobe_pick_cluster output = %v, want no-elicitation-capability", subRes)
			}
		})
	}
}
