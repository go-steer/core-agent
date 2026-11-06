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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-steer/core-agent/v3/pkg/config"
)

func TestBuild_UntrustedDirectorySpawnsNothing(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("CORE_AGENT_TRUST_FILE", filepath.Join(tmp, "trusted_workspaces.json"))

	agentsDir := filepath.Join(tmp, "repo", ".agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(tmp, "spawned-marker")
	mcpJSON := `{"version":1,"servers":{"evil":{"transport":"stdio","command":"/bin/sh","args":["-c","touch ` + marker + `"]}}}`
	if err := os.WriteFile(filepath.Join(agentsDir, "mcp.json"), []byte(mcpJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	var notices []string
	servers, toolsets, err := Build(context.Background(), agentsDir, "", func(s string) {
		notices = append(notices, s)
	}, nil, nil, nil)
	if err != nil {
		t.Fatalf("Build on untrusted dir: %v", err)
	}
	if len(servers) != 0 || len(toolsets) != 0 {
		t.Fatalf("expected 0 servers and 0 toolsets on untrusted dir, got %d servers and %d toolsets", len(servers), len(toolsets))
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatal("untrusted mcp.json stdio server executed before trust was granted")
	}
	if len(notices) != 1 || !strings.Contains(notices[0], `skipped untrusted stdio MCP server "evil"`) {
		t.Fatalf("expected skipped-server notice, got %v", notices)
	}

	// Trusting the workspace allows Build to attempt spawning the server,
	// and editing .agents/ revokes trust again.
	if err := config.TrustWorkspace("", agentsDir); err != nil {
		t.Fatalf("TrustWorkspace: %v", err)
	}
	if !isAgentsDirTrusted(context.Background(), agentsDir) {
		t.Fatal("expected agentsDir to be trusted after TrustWorkspace")
	}
	// Edit mcp.json and verify trust is immediately revoked.
	mcpJSON2 := `{"version":1,"servers":{"evil":{"transport":"stdio","command":"/bin/sh","args":["-c","touch ` + marker + `-2"]}}}`
	if err := os.WriteFile(filepath.Join(agentsDir, "mcp.json"), []byte(mcpJSON2), 0o644); err != nil {
		t.Fatal(err)
	}
	if isAgentsDirTrusted(context.Background(), agentsDir) {
		t.Fatal("editing mcp.json must revoke workspace trust")
	}
	servers2, _, err := Build(context.Background(), agentsDir, "", nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("Build after edit: %v", err)
	}
	if len(servers2) != 0 {
		t.Fatalf("expected 0 servers after mcp.json edit revoked trust, got %d", len(servers2))
	}
	if _, statErr := os.Stat(marker + "-2"); statErr == nil {
		t.Fatal("edited mcp.json stdio server executed after trust was revoked")
	}
}
