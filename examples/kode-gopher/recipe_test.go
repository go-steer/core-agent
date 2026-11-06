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

package kodegopher_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/go-steer/core-agent/v3/pkg/config"
	"github.com/go-steer/core-agent/v3/pkg/instruction"
	"github.com/go-steer/core-agent/v3/pkg/mcp"
	"github.com/go-steer/core-agent/v3/pkg/permissions"
)

const agentsDir = ".agents"

func TestRecipeLoadsAndValidates(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load(agentsDir)
	if err != nil {
		t.Fatalf("config.Load(%q): %v", agentsDir, err)
	}
	if cfg.Permissions.Mode != string(permissions.ModeAsk) {
		t.Errorf("permissions.mode = %q, want %q", cfg.Permissions.Mode, permissions.ModeAsk)
	}

	servers, err := mcp.Load(agentsDir)
	if err != nil {
		t.Fatalf("mcp.Load(%q): %v", agentsDir, err)
	}
	spec, ok := servers.Servers["kode_gopher"]
	if !ok {
		t.Fatalf("mcp.json missing %q server; got %v", "kode_gopher", servers.Servers)
	}
	if spec.Transport != "stdio" {
		t.Errorf("kode_gopher.transport = %q, want \"stdio\"", spec.Transport)
	}
	if spec.Command != "kode-gopher" || !slices.Equal(spec.Args, []string{"serve"}) {
		t.Errorf("kode_gopher command/args = %q %v, want \"kode-gopher\" [\"serve\"]", spec.Command, spec.Args)
	}
	if spec.ReadOnly {
		t.Error("kode_gopher.read_only must be false because execute_go_code runs arbitrary compiled Go")
	}

	wantTools := []string{"execute_go_code", "lookup_package_docs", "gcp_auth_status"}
	if !slices.Equal(spec.Tools, wantTools) {
		t.Errorf("kode_gopher.tools = %v, want %v", spec.Tools, wantTools)
	}

	note := spec.ToolNotes["execute_go_code"]
	for _, needle := range []string{"func run(ctx", "any, error)", "JSON", "phase=build"} {
		if !strings.Contains(note, needle) {
			t.Errorf("tool_notes[\"execute_go_code\"] = %q, missing %q", note, needle)
		}
	}

	body, err := os.ReadFile(filepath.Join(agentsDir, "AGENTS.md"))
	if err != nil {
		t.Fatalf("read AGENTS.md: %v", err)
	}
	if _, _, err := instruction.Expand(string(body), agentsDir, agentsDir); err != nil {
		t.Fatalf("instruction.Expand(AGENTS.md): %v", err)
	}
	loaded, err := instruction.Load(".", "")
	if err != nil {
		t.Fatalf("instruction.Load(\".\"): %v", err)
	}
	if loaded.Empty() {
		t.Fatal("instruction.Load(\".\") returned empty instructions")
	}
}

func TestPermissionGateAllowsReadOnlyToolsAndGatesExecuteGoCode(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load(agentsDir)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	// Pass a nil prompter so any call that is NOT pre-allowed by policy
	// fails with permissions.ErrNoPrompter in ModeAsk.
	gate, err := permissions.FromConfig(cfg, ".", "", nil)
	if err != nil {
		t.Fatalf("permissions.FromConfig: %v", err)
	}

	ctx := context.Background()
	for _, allowedTool := range []string{
		"kode_gopher_lookup_package_docs",
		"kode_gopher_gcp_auth_status",
	} {
		detail := allowedTool + ` {"package":"k8s.io/client-go/kubernetes"}`
		if err := gate.CheckToolCallWithArgs(ctx, "mcp", allowedTool, detail, map[string]any{"package": "k8s.io/client-go/kubernetes"}); err != nil {
			t.Errorf("CheckToolCallWithArgs(%q) = %v, want nil (pre-allowed by policy)", allowedTool, err)
		}
	}

	execTool := "kode_gopher_execute_go_code"
	execDetail := execTool + ` {"code":"package main"}`
	err = gate.CheckToolCallWithArgs(ctx, "mcp", execTool, execDetail, map[string]any{"code": "package main"})
	if !errors.Is(err, permissions.ErrNoPrompter) {
		t.Errorf("CheckToolCallWithArgs(%q) = %v, want ErrNoPrompter (must stay gated in ask mode)", execTool, err)
	}
}

func TestDocumentedStreamableHTTPConfigValidates(t *testing.T) {
	t.Parallel()

	readme, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatalf("read README.md: %v", err)
	}
	const marker = "\"transport\": \"http\""
	idx := strings.Index(string(readme), marker)
	if idx < 0 {
		t.Fatalf("README.md does not contain a streamable-HTTP %s example", marker)
	}
	// Extract the enclosing ```json ... ``` fence around the http transport stanza.
	before := string(readme[:idx])
	fenceStart := strings.LastIndex(before, "```json\n")
	if fenceStart < 0 {
		t.Fatal("could not find opening ```json fence before http transport stanza")
	}
	bodyStart := fenceStart + len("```json\n")
	rest := string(readme[bodyStart:])
	fenceEnd := strings.Index(rest, "\n```")
	if fenceEnd < 0 {
		t.Fatal("could not find closing ``` fence for http transport stanza")
	}
	rawJSON := rest[:fenceEnd]

	var servers mcp.Servers
	if err := json.Unmarshal([]byte(rawJSON), &servers); err != nil {
		t.Fatalf("unmarshal README http mcp.json stanza: %v\n%s", err, rawJSON)
	}
	spec, ok := servers.Servers["kode_gopher"]
	if !ok {
		t.Fatalf("README http stanza missing kode_gopher server: %+v", servers)
	}
	if err := spec.Validate("kode_gopher"); err != nil {
		t.Fatalf("spec.Validate on README http stanza: %v", err)
	}
	if spec.Auth == nil || spec.Auth.GoogleOAuth == nil {
		t.Fatalf("README http stanza must demonstrate auth.google_oauth; got %+v", spec.Auth)
	}
}
