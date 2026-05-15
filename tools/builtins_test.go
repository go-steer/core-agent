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

func TestBuiltinTools_Disable_KnownNames(t *testing.T) {
	t.Parallel()
	// Each canonical name must flip the matching field to false. The
	// table mirrors BuiltinToolNames so a future rename or addition
	// fails this test until the helper learns about it.
	cases := map[string]func(BuiltinTools) bool{
		"bash":       func(b BuiltinTools) bool { return b.Bash },
		"read_file":  func(b BuiltinTools) bool { return b.ReadFile },
		"write_file": func(b BuiltinTools) bool { return b.WriteFile },
		"edit_file":  func(b BuiltinTools) bool { return b.EditFile },
		"list_dir":   func(b BuiltinTools) bool { return b.ListDir },
		"todo":       func(b BuiltinTools) bool { return b.Todo },
	}
	if len(cases) != len(BuiltinToolNames) {
		t.Fatalf("test table size %d != BuiltinToolNames size %d — update both", len(cases), len(BuiltinToolNames))
	}
	for _, name := range BuiltinToolNames {
		field, ok := cases[name]
		if !ok {
			t.Errorf("BuiltinToolNames entry %q has no test-table entry", name)
			continue
		}
		b := Default()
		if !field(b) {
			t.Fatalf("Default() should have %q on before Disable", name)
		}
		if err := b.Disable(name); err != nil {
			t.Errorf("Disable(%q): %v", name, err)
			continue
		}
		if field(b) {
			t.Errorf("Disable(%q) did not flip the field off", name)
		}
	}
}

func TestBuiltinTools_Disable_UnknownName(t *testing.T) {
	t.Parallel()
	b := Default()
	err := b.Disable("grep")
	if err == nil {
		t.Fatal("expected error for unknown tool name")
	}
	if !strings.Contains(err.Error(), "unknown built-in tool") {
		t.Errorf("error %q missing 'unknown built-in tool'", err.Error())
	}
	if !strings.Contains(err.Error(), `"grep"`) {
		t.Errorf("error %q should quote the bad name", err.Error())
	}
	// Default fields stay untouched on rejection.
	if !b.Bash || !b.ReadFile {
		t.Errorf("rejection should not mutate fields; got %+v", b)
	}
}

func TestBuiltinTools_Disable_Idempotent(t *testing.T) {
	t.Parallel()
	b := Default()
	if err := b.Disable("bash"); err != nil {
		t.Fatalf("first Disable: %v", err)
	}
	if err := b.Disable("bash"); err != nil {
		t.Fatalf("second Disable should be a no-op, got %v", err)
	}
	if b.Bash {
		t.Errorf("Bash should still be off after double-disable")
	}
}
