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
	"path/filepath"
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/permissions"
)

// TestMCPFileName_ClassifiedAsControlPlane keeps the permissions
// package's control-plane classification (which duplicates "mcp.json"
// as a literal to avoid an import cycle) in lockstep with the actual
// MCPFileName loader constant. If MCPFileName is ever renamed, this
// fails until permissions.controlPlaneBasenames is updated.
func TestMCPFileName_ClassifiedAsControlPlane(t *testing.T) {
	t.Parallel()
	// A gate write to <root>/.agents/<MCPFileName> must be gated as an
	// elevated control-plane write: yolo + no prompter => denied.
	root := t.TempDir()
	path := filepath.Join(root, ".agents", MCPFileName)
	scope, err := permissions.NewPathScope(root, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	g := permissions.New(permissions.Options{Mode: permissions.ModeYolo, Scope: scope})
	if err := g.CheckFileWrite(context.Background(), "write_file", path); err == nil {
		t.Fatalf("write to .agents/%s should be gated as control-plane (denied under yolo/no-prompter)", MCPFileName)
	}
}
