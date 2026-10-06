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
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/config"
	"github.com/go-steer/core-agent/v2/pkg/models"
	"github.com/go-steer/core-agent/v2/pkg/permissions"
)

func approverTemplate(t *testing.T, cfg *config.Config, root string) *permissions.Gate {
	t.Helper()
	g, err := permissions.FromConfig(cfg, root, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestWireApprover(t *testing.T) {
	t.Parallel()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	agentsDir := filepath.Join(root, ".agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	instructions := filepath.Join(agentsDir, "approver.md")
	if err := os.WriteFile(instructions, []byte("refuse helm uninstall"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultConfig()
	cfg.Model.Provider = config.ProviderEcho
	cfg.Permissions.Mode = string(permissions.ModeYolo)
	provider, err := models.Resolve(cfg)
	if err != nil {
		t.Fatal(err)
	}

	// No auto block: nothing is wired and nothing is said.
	var stderr bytes.Buffer
	g := approverTemplate(t, cfg, root)
	if err := wireApprover(context.Background(), provider, cfg, agentsDir, g, &stderr); err != nil || g.HasApprover() || stderr.Len() != 0 {
		t.Errorf("no auto block: err %v, HasApprover %v, stderr %q; want nothing wired", err, g.HasApprover(), stderr.String())
	}

	// Configured: the approver is wired, sub-gates inherit it, the
	// instructions file is privilege-bearing, and an empty eligible
	// list is reported.
	cfg.Permissions.Auto = &config.AutoApproverConfig{InstructionsFile: "approver.md"}
	g = approverTemplate(t, cfg, root)
	if err := wireApprover(context.Background(), provider, cfg, agentsDir, g, &stderr); err != nil {
		t.Fatal(err)
	}
	sub := g.DeriveForSession("s1", nil)
	if !g.HasApprover() || !sub.HasApprover() {
		t.Errorf("HasApprover: template %v, derived %v; want both", g.HasApprover(), sub.HasApprover())
	}
	if err := sub.CheckFileWrite(context.Background(), "write_file", instructions); !errors.Is(err, permissions.ErrControlPlaneWrite) {
		t.Errorf("yolo write to the instructions file = %v, want ErrControlPlaneWrite", err)
	}
	if !strings.Contains(stderr.String(), "permissions.auto.eligible and eligible_bundles are both empty") {
		t.Errorf("stderr = %q, want the empty-eligible warning", stderr.String())
	}

	// A bundle alone is an eligible list: no warning (#1252).
	var quiet bytes.Buffer
	cfg.Permissions.Auto = &config.AutoApproverConfig{InstructionsFile: "approver.md", EligibleBundles: []string{permissions.AutoBundleCoding}}
	g = approverTemplate(t, cfg, root)
	if err := wireApprover(context.Background(), provider, cfg, agentsDir, g, &quiet); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(quiet.String(), "both empty") {
		t.Errorf("stderr = %q; a bundle-only auto block is not empty", quiet.String())
	}

	// A missing instructions file fails startup and wires nothing.
	cfg.Permissions.Auto.InstructionsFile = "missing.md"
	g = approverTemplate(t, cfg, root)
	if err := wireApprover(context.Background(), provider, cfg, agentsDir, g, &stderr); err == nil || g.HasApprover() {
		t.Errorf("missing instructions file: err %v, HasApprover %v; want an error and no approver", err, g.HasApprover())
	}
}
