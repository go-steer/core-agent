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

package attach

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"
)

// richRegistrant is a stubRegistrant that also implements the three
// optional provider interfaces (ToolsProvider, AgentsProvider,
// StatusProvider). The integration tests use it to exercise the
// /tools, /agents, /status endpoints end-to-end.
type richRegistrant struct {
	stubRegistrant
	tools  []ToolInfo
	agents []AgentInfo
	status StatusInfo
}

func (r *richRegistrant) AttachTools() []ToolInfo   { return r.tools }
func (r *richRegistrant) AttachAgents() []AgentInfo { return r.agents }
func (r *richRegistrant) AttachStatus() StatusInfo  { return r.status }

func TestIntegration_ToolsEndpoint(t *testing.T) {
	t.Parallel()
	reg := NewSessionRegistry()
	ag := &richRegistrant{
		stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s1"},
		tools: []ToolInfo{
			{Name: "read_file", Description: "read a file", Source: ToolSourceBuiltin, GateState: "allowed"},
			{Name: "kube_get", Description: "kubectl get", Source: ToolSourceMCP, Server: "kube-mcp", GateState: "prompted"},
		},
	}
	if _, err := reg.Register(ag); err != nil {
		t.Fatal(err)
	}
	base, cleanup := startTestServer(t, reg)
	defer cleanup()

	resp, err := http.Get(base + "/sessions/core-agent/s1/tools")
	if err != nil {
		t.Fatalf("GET /tools: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	var got struct {
		Tools []ToolInfo `json:"tools"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Tools) != 2 {
		t.Fatalf("len = %d, want 2", len(got.Tools))
	}
	if got.Tools[0].Name != "read_file" || got.Tools[0].GateState != "allowed" {
		t.Errorf("tool 0: %+v", got.Tools[0])
	}
	if got.Tools[1].Source != ToolSourceMCP || got.Tools[1].Server != "kube-mcp" {
		t.Errorf("tool 1: %+v", got.Tools[1])
	}
}

func TestIntegration_ToolsEndpoint_NoProvider_EmptyList(t *testing.T) {
	t.Parallel()
	// stubRegistrant doesn't implement ToolsProvider — endpoint
	// should still 200 with an empty list, not 501.
	reg := NewSessionRegistry()
	ag := &stubRegistrant{app: "core-agent", user: "u", sid: "s1"}
	if _, err := reg.Register(ag); err != nil {
		t.Fatal(err)
	}
	base, cleanup := startTestServer(t, reg)
	defer cleanup()

	resp, err := http.Get(base + "/sessions/core-agent/s1/tools")
	if err != nil {
		t.Fatalf("GET /tools: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != `{"tools":[]}`+"\n" && string(body) != `{"tools":[]}` {
		t.Errorf("body = %q, want empty tools list", body)
	}
}

func TestIntegration_AgentsEndpoint(t *testing.T) {
	t.Parallel()
	startedAt := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	reg := NewSessionRegistry()
	ag := &richRegistrant{
		stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s1"},
		agents: []AgentInfo{
			{ID: "monitor-1", Name: "monitor-1", Status: "running", StartedAt: startedAt, ParentSessionID: "s1"},
		},
	}
	if _, err := reg.Register(ag); err != nil {
		t.Fatal(err)
	}
	base, cleanup := startTestServer(t, reg)
	defer cleanup()

	resp, err := http.Get(base + "/sessions/core-agent/s1/agents")
	if err != nil {
		t.Fatalf("GET /agents: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var got struct {
		Agents []AgentInfo `json:"agents"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Agents) != 1 {
		t.Fatalf("len = %d, want 1", len(got.Agents))
	}
	if got.Agents[0].Name != "monitor-1" || got.Agents[0].Status != "running" {
		t.Errorf("agent 0: %+v", got.Agents[0])
	}
}

func TestIntegration_StatusEndpoint(t *testing.T) {
	t.Parallel()
	reg := NewSessionRegistry()
	ag := &richRegistrant{
		stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s1"},
		status: StatusInfo{
			State:     AgentStateRunning,
			ModelName: "gemini-3.1-pro-preview-customtools",
		},
	}
	if _, err := reg.Register(ag); err != nil {
		t.Fatal(err)
	}
	base, cleanup := startTestServer(t, reg)
	defer cleanup()

	resp, err := http.Get(base + "/sessions/core-agent/s1/status")
	if err != nil {
		t.Fatalf("GET /status: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var got StatusInfo
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.State != AgentStateRunning || got.ModelName != "gemini-3.1-pro-preview-customtools" {
		t.Errorf("status = %+v", got)
	}
}

func TestIntegration_StatusEndpoint_NoProvider_DefaultsToIdle(t *testing.T) {
	t.Parallel()
	reg := NewSessionRegistry()
	ag := &stubRegistrant{app: "core-agent", user: "u", sid: "s1"}
	if _, err := reg.Register(ag); err != nil {
		t.Fatal(err)
	}
	base, cleanup := startTestServer(t, reg)
	defer cleanup()

	resp, err := http.Get(base + "/sessions/core-agent/s1/status")
	if err != nil {
		t.Fatalf("GET /status: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var got StatusInfo
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.State != AgentStateIdle {
		t.Errorf("state = %q, want %q (default for no-provider)", got.State, AgentStateIdle)
	}
}

func TestIntegration_ShortcutForms(t *testing.T) {
	t.Parallel()
	reg := NewSessionRegistry()
	ag := &richRegistrant{
		stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s1"},
		tools:          []ToolInfo{{Name: "read_file", Source: ToolSourceBuiltin}},
	}
	if _, err := reg.Register(ag); err != nil {
		t.Fatal(err)
	}
	base, cleanup := startTestServer(t, reg)
	defer cleanup()

	// Single-segment /sessions/<sid>/tools form.
	resp, err := http.Get(base + "/sessions/s1/tools")
	if err != nil {
		t.Fatalf("GET shortcut: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
}
