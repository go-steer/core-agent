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
	"testing"

	adktool "google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v2/pkg/config"
	"github.com/go-steer/core-agent/v2/pkg/permissions"
)

// builtinWritesSharedFilesystem is the expected classification of every
// name Build can put in the catalog. It is spelled out rather than
// derived so that adding a built-in is a decision someone has to write
// down: TestWritesSharedFilesystem_EveryBuiltinIsClassified fails on any
// catalog name missing from here.
var builtinWritesSharedFilesystem = map[string]bool{
	"read_file":        false,
	"read_many_files":  false,
	"stat":             false,
	"list_dir":         false,
	"glob":             false,
	"grep":             false,
	"json_query":       false,
	"fetch_url":        false, // GET-only network read
	"wait_and_verify":  false, // only ever calls read-only tools
	"todo":             false, // in-memory store
	"alert":            false, // outbound webhook
	"write_file":       true,
	"edit_file":        true,
	"delete_file":      true,
	"bash":             true, // arbitrary commands
	"record_plan":      true, // artifact under the agents dir
	"sciontool_status": true, // sticky status file
}

// buildEverything registers as much of the catalog as one process can:
// fetch_url and record_plan are conditionally registered, so the config
// has to earn them. sciontool_status depends on a binary being on PATH
// and alert on a live target, so neither is guaranteed here — the
// catalog-coverage half of the test below is what keeps them honest.
func buildEverything(t *testing.T) (*Registry, *permissions.Gate) {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.URLScope.Allow = []string{"example.com"}
	cfg.Permissions.PlanMode = "advisory"
	gate := permissions.New(permissions.Options{Mode: permissions.ModeYolo})
	reg, err := Build(cfg, gate, t.TempDir(), Default())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return reg, gate
}

// A new built-in must be classified for the #653 spawn guard. Two
// halves, because neither alone is sufficient: the catalog half covers
// names whose spec did not register in this process (sciontool_status
// needs a binary on PATH), and the object half is the only one that
// exercises WritesSharedFilesystem itself.
func TestWritesSharedFilesystem_EveryBuiltinIsClassified(t *testing.T) {
	t.Parallel()
	reg, gate := buildEverything(t)

	for name := range gate.RegisteredTools() {
		if _, ok := builtinWritesSharedFilesystem[name]; !ok {
			t.Errorf("built-in %q is in Build's catalog but unclassified — "+
				"decide whether it can clobber the shared working tree and add it to "+
				"builtinWritesSharedFilesystem (and, if it cannot, to nonFilesystemMutators)", name)
		}
	}

	seen := make(map[string]bool, len(reg.Tools))
	for _, tl := range reg.Tools {
		seen[tl.Name()] = true
		want, ok := builtinWritesSharedFilesystem[tl.Name()]
		if !ok {
			continue // already reported by the catalog half
		}
		if got := WritesSharedFilesystem(tl); got != want {
			t.Errorf("WritesSharedFilesystem(%q) = %v, want %v", tl.Name(), got, want)
		}
	}
	// The registrations this config was written to earn. Without this
	// the object half would pass vacuously if Build stopped registering
	// them.
	for _, name := range []string{"write_file", "edit_file", "bash", "record_plan", "fetch_url", "todo"} {
		if !seen[name] {
			t.Errorf("%q did not register — the classification above went untested", name)
		}
	}
}

// The control-plane tools live outside Build, so the table above cannot
// reach them. They mutate real state (#460 serializes them, correctly)
// and none of it is on disk.
func TestWritesSharedFilesystem_ControlPlaneToolsAreNotWriters(t *testing.T) {
	t.Parallel()
	for _, name := range []string{
		"spawn_agent", "stop_agent", "spawn_remote_agent",
		"mark_task_done", "return_result", "report_done", "report_completed",
		"report_alert", "schedule_next_turn",
	} {
		if WritesSharedFilesystem(stubTool(t, name)) {
			t.Errorf("%q classified as a filesystem writer — a subagent granted only "+
				"control-plane tools would be refused for a hazard it does not pose", name)
		}
	}
}

func TestWritesSharedFilesystem_UnknownNameIsFailSafe(t *testing.T) {
	t.Parallel()
	if !WritesSharedFilesystem(stubTool(t, "some_consumer_tool")) {
		t.Error("an unrecognised tool must classify as writing: a missed writer costs the guarantee, " +
			"a misclassified reader costs only concurrency")
	}
}

// An MCP server that declares readOnlyHint (#1098) settles the question
// for its own tools — the same authority IsReadOnlyTool gives it.
func TestWritesSharedFilesystem_ReadOnlyHintWins(t *testing.T) {
	t.Parallel()
	if WritesSharedFilesystem(hintingTool{name: "mcp_probe", readOnly: true}) {
		t.Error("a tool declaring readOnlyHint=true must not classify as a writer")
	}
	if !WritesSharedFilesystem(hintingTool{name: "mcp_probe", readOnly: false}) {
		t.Error("a tool declaring readOnlyHint=false must classify as a writer")
	}
}

// A hint of false must beat a name that happens to sit in the
// non-filesystem table: the declaration is about the tool in hand.
func TestWritesSharedFilesystem_HintBeatsTheNameTable(t *testing.T) {
	t.Parallel()
	if !WritesSharedFilesystem(hintingTool{name: "todo", readOnly: false}) {
		t.Error("readOnlyHint=false must win over the nonFilesystemMutators entry for the same name")
	}
}

func TestAnyWritesSharedFilesystem(t *testing.T) {
	t.Parallel()
	ro := stubTool(t, "grep")
	rw := stubTool(t, "write_file")
	cases := []struct {
		name string
		in   []adktool.Tool
		want bool
	}{
		{"nil", nil, false},
		{"all read-only", []adktool.Tool{ro, stubTool(t, "stat")}, false},
		{"one writer among readers", []adktool.Tool{ro, rw}, true},
		{"nil element is not a writer", []adktool.Tool{nil}, false},
	}
	for _, tc := range cases {
		if got := AnyWritesSharedFilesystem(tc.in); got != tc.want {
			t.Errorf("%s: AnyWritesSharedFilesystem = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func stubTool(t *testing.T, name string) adktool.Tool {
	t.Helper()
	type empty struct{}
	tl, err := functiontool.New(functiontool.Config{Name: name, Description: "stub"},
		func(_ adktool.Context, _ empty) (empty, error) { return empty{}, nil })
	if err != nil {
		t.Fatalf("functiontool.New(%q): %v", name, err)
	}
	return tl
}

// hintingTool satisfies ReadOnlyHinter the way pkg/mcp's tools do.
type hintingTool struct {
	name     string
	readOnly bool
}

func (h hintingTool) Name() string                            { return h.name }
func (h hintingTool) Description() string                     { return "hinting stub" }
func (h hintingTool) IsLongRunning() bool                     { return false }
func (h hintingTool) ReadOnlyHint() bool                      { return h.readOnly }
func (h hintingTool) Declaration() *genai.FunctionDeclaration { return nil }
