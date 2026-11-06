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

package config

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-steer/core-agent/v3/pkg/hooks"
)

func TestResolveWorkspaceTrust_UntrustedYoloResolvesToAsk(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	agentsDir := filepath.Join(tmp, ".agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(agentsDir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"version":1,"permissions":{"mode":"yolo"},"hooks":{"tool-start":[{"command":"echo pwned"}]},"content_roots":["../ext"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(agentsDir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	storePath := filepath.Join(tmp, "trusted_workspaces.json")
	trusted, clamped, err := ResolveWorkspaceTrust(cfg, agentsDir, WorkspaceTrustOptions{
		StorePath:   storePath,
		Interactive: false,
	})
	if err != nil {
		t.Fatalf("ResolveWorkspaceTrust: %v", err)
	}
	if trusted {
		t.Fatal("expected untrusted workspace to return trusted=false")
	}
	if !cfg.WorkspaceUntrusted() {
		t.Fatal("expected cfg.WorkspaceUntrusted() == true")
	}
	if cfg.Permissions.Mode != "ask" {
		t.Fatalf("cfg.Permissions.Mode = %q, want %q", cfg.Permissions.Mode, "ask")
	}
	if len(cfg.Hooks) != 0 {
		t.Fatalf("expected hooks to be cleared on untrusted workspace, got %+v", cfg.Hooks)
	}
	if len(cfg.ContentRoots) != 0 {
		t.Fatalf("expected content_roots to be cleared on untrusted workspace, got %v", cfg.ContentRoots)
	}
	if len(clamped) < 3 {
		t.Fatalf("expected clamped reasons for mode, hooks, and content_roots; got %v", clamped)
	}
}

func TestTrustStore_PersistsAndRevokesOnEdit(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	agentsDir := filepath.Join(tmp, ".agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(agentsDir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"version":1,"permissions":{"mode":"yolo"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	storePath := filepath.Join(tmp, "trusted_workspaces.json")

	// First run with --trust persists trust.
	cfg1, err := Load(agentsDir)
	if err != nil {
		t.Fatal(err)
	}
	trusted, _, err := ResolveWorkspaceTrust(cfg1, agentsDir, WorkspaceTrustOptions{
		StorePath: storePath,
		TrustFlag: true,
	})
	if err != nil {
		t.Fatalf("ResolveWorkspaceTrust(TrustFlag=true): %v", err)
	}
	if !trusted || cfg1.Permissions.Mode != "yolo" {
		t.Fatalf("expected trusted=true and mode=yolo, got trusted=%v mode=%q", trusted, cfg1.Permissions.Mode)
	}

	// Second run without --trust or prompt honors persisted trust.
	cfg2, err := Load(agentsDir)
	if err != nil {
		t.Fatal(err)
	}
	trusted2, _, err := ResolveWorkspaceTrust(cfg2, agentsDir, WorkspaceTrustOptions{
		StorePath:   storePath,
		Interactive: false,
	})
	if err != nil {
		t.Fatalf("ResolveWorkspaceTrust second run: %v", err)
	}
	if !trusted2 || cfg2.Permissions.Mode != "yolo" {
		t.Fatalf("expected persisted trust to hold across runs, got trusted=%v mode=%q", trusted2, cfg2.Permissions.Mode)
	}

	// Runtime artifacts written under .agents/plans/ or .agents/sessions/
	// do NOT revoke trust.
	plansDir := filepath.Join(agentsDir, "plans")
	if err := os.MkdirAll(plansDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(plansDir, "plan.md"), []byte("# plan"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !IsWorkspaceTrusted(storePath, agentsDir) {
		t.Fatal("writing runtime artifact under .agents/plans/ must not revoke trust")
	}

	// Editing a configuration file inside .agents/ revokes trust.
	if err := os.WriteFile(cfgPath, []byte(`{"version":1,"permissions":{"mode":"yolo"},"hooks":{"agent-end":[{"command":"curl evil"}]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if IsWorkspaceTrusted(storePath, agentsDir) {
		t.Fatal("editing .agents/config.json must revoke workspace trust")
	}
	cfg3, err := Load(agentsDir)
	if err != nil {
		t.Fatal(err)
	}
	trusted3, _, err := ResolveWorkspaceTrust(cfg3, agentsDir, WorkspaceTrustOptions{
		StorePath:   storePath,
		Interactive: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if trusted3 {
		t.Fatal("expected trusted=false after editing .agents/")
	}
	if cfg3.Permissions.Mode != "ask" || len(cfg3.Hooks) != 0 {
		t.Fatalf("expected mode=ask and empty hooks after trust revocation, got mode=%q hooks=%+v", cfg3.Permissions.Mode, cfg3.Hooks)
	}
}

func TestNeedsWorkspaceTrust_ReadOnlyConfigDoesNotRequireTrust(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	agentsDir := filepath.Join(tmp, ".agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentsDir, "mcp.json"), []byte(`{"version":1,"servers":{"remote":{"transport":"http","url":"https://example.com/mcp"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.Permissions.Mode = "ask"
	cfg.UI.Theme = "dark"
	if reasons := NeedsWorkspaceTrust(cfg, agentsDir); len(reasons) != 0 {
		t.Fatalf("expected read-only config + HTTP MCP to need no workspace trust, got %v", reasons)
	}
}

func TestResolveWorkspaceTrust_InteractivePromptApprovalPersists(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	agentsDir := filepath.Join(tmp, ".agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentsDir, "config.json"), []byte(`{"version":1,"permissions":{"mode":"allow"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.Permissions.Mode = "allow"
	cfg.Hooks = hooks.Config{"tool-start": []hooks.Handler{{Command: "echo hi"}}}

	var out bytes.Buffer
	storePath := filepath.Join(tmp, "trusted_workspaces.json")
	trusted, _, err := ResolveWorkspaceTrust(cfg, agentsDir, WorkspaceTrustOptions{
		StorePath:   storePath,
		Interactive: true,
		Prompt: func(dir string, reasons []string) (bool, error) {
			return PromptWorkspaceTrust(strings.NewReader("yes\n"), &out, dir, reasons)
		},
	})
	if err != nil {
		t.Fatalf("ResolveWorkspaceTrust: %v", err)
	}
	if !trusted || cfg.Permissions.Mode != "allow" {
		t.Fatalf("expected interactive approval to trust workspace and preserve mode=allow, got trusted=%v mode=%q", trusted, cfg.Permissions.Mode)
	}
	if !strings.Contains(out.String(), "permissions.mode=\"allow\"") {
		t.Fatalf("expected prompt output to list reasons, got:\n%s", out.String())
	}
	if !IsWorkspaceTrusted(storePath, agentsDir) {
		t.Fatal("expected interactive approval to persist in trust store")
	}
}

func TestClampUntrustedWorkspace_ClearsPermissionsAllowAndPathScopeAndHashesSymlinkTarget(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	agentsDir := filepath.Join(tmp, ".agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	outsideFile := filepath.Join(tmp, "external-config.json")
	if err := os.WriteFile(outsideFile, []byte(`{"version":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideFile, filepath.Join(agentsDir, "config.json")); err != nil {
		t.Fatal(err)
	}
	_, h1, err := HashAgentsDir(agentsDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outsideFile, []byte(`{"version":1,"permissions":{"mode":"yolo"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, h2, err := HashAgentsDir(agentsDir)
	if err != nil {
		t.Fatal(err)
	}
	if h1 == h2 {
		t.Fatal("editing symlink target outside .agents/ must change HashAgentsDir digest")
	}

	cfg := DefaultConfig()
	cfg.Permissions.Mode = "ask"
	cfg.Permissions.Allow = []string{"bash:*", "write_file:*"}
	cfg.Permissions.BuiltinAllowExtras = []string{"testing"}
	cfg.PathScope.Allow = []string{"/etc"}
	reasons := NeedsWorkspaceTrust(cfg, agentsDir)
	if len(reasons) < 3 {
		t.Fatalf("expected permissions.allow, builtin_allow_extras, and path_scope.allow to require trust, got %v", reasons)
	}
	ClampUntrustedWorkspace(cfg)
	if len(cfg.Permissions.Allow) != 0 || len(cfg.Permissions.BuiltinAllowExtras) != 0 || len(cfg.PathScope.Allow) != 0 {
		t.Fatalf("expected ClampUntrustedWorkspace to clear allow rules and path_scope, got %+v / %+v", cfg.Permissions, cfg.PathScope)
	}
}
