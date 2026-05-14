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

package tools

import (
	"strings"
	"testing"

	"github.com/go-steer/core-agent/config"
	"github.com/go-steer/core-agent/permissions"
)

func TestDefault_AllOn(t *testing.T) {
	t.Parallel()
	d := Default()
	if !d.Bash || !d.ReadFile || !d.WriteFile || !d.EditFile || !d.ListDir || !d.Todo {
		t.Errorf("Default() should enable everything; got %+v", d)
	}
}

func TestBuild_DefaultProducesSixTools(t *testing.T) {
	t.Parallel()
	cfg := config.DefaultConfig()
	gate := permissions.New(permissions.Options{Mode: permissions.ModeYolo})
	reg, err := Build(cfg, gate, Default())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(reg.Tools) != 6 {
		t.Fatalf("expected 6 tools, got %d", len(reg.Tools))
	}
	if reg.Todo == nil {
		t.Errorf("Registry.Todo should always be non-nil")
	}
	wantNames := []string{"read_file", "write_file", "edit_file", "list_dir", "bash", "todo"}
	got := make(map[string]bool, len(reg.Tools))
	for _, tl := range reg.Tools {
		got[tl.Name()] = true
	}
	for _, n := range wantNames {
		if !got[n] {
			t.Errorf("missing tool: %s (got %v)", n, got)
		}
	}
}

func TestBuild_SelectiveSubset(t *testing.T) {
	t.Parallel()
	cfg := config.DefaultConfig()
	gate := permissions.New(permissions.Options{Mode: permissions.ModeYolo})
	reg, err := Build(cfg, gate, BuiltinTools{Bash: true})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(reg.Tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(reg.Tools))
	}
	if reg.Tools[0].Name() != "bash" {
		t.Errorf("expected bash, got %q", reg.Tools[0].Name())
	}
	// Todo store is always created even when the todo tool is off,
	// so hosts can pre-populate the plan if they want.
	if reg.Todo == nil {
		t.Errorf("Registry.Todo should always be non-nil")
	}
}

func TestBuild_EmptySetProducesNoTools(t *testing.T) {
	t.Parallel()
	cfg := config.DefaultConfig()
	gate := permissions.New(permissions.Options{Mode: permissions.ModeYolo})
	reg, err := Build(cfg, gate, BuiltinTools{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(reg.Tools) != 0 {
		t.Errorf("zero-value BuiltinTools should produce 0 tools, got %d", len(reg.Tools))
	}
}

func TestBuild_NilGateRejected(t *testing.T) {
	t.Parallel()
	_, err := Build(config.DefaultConfig(), nil, Default())
	if err == nil || !strings.Contains(err.Error(), "gate is required") {
		t.Errorf("expected gate-required error, got %v", err)
	}
}

func TestBuild_NilCfgRejected(t *testing.T) {
	t.Parallel()
	gate := permissions.New(permissions.Options{Mode: permissions.ModeYolo})
	_, err := Build(nil, gate, Default())
	if err == nil || !strings.Contains(err.Error(), "cfg is required") {
		t.Errorf("expected cfg-required error, got %v", err)
	}
}
