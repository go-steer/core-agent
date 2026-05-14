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
	"fmt"

	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"

	"github.com/go-steer/core-agent/config"
	"github.com/go-steer/core-agent/permissions"
)

// BuiltinTools toggles core-agent's built-in tool suite. Each enabled
// flag becomes one entry in the returned Registry's Tools slice.
//
// All defaults are on — every consumer that's writing an agent will
// almost certainly want read/write/edit/list/bash. The Todo store is
// always created; the toggle just controls whether the model can drive
// it via the `todo` tool.
//
// To turn one off:
//
//	reg, _ := tools.Build(cfg, gate, tools.BuiltinTools{
//	    Bash: false,        // disable shell
//	    ReadFile: true,
//	    WriteFile: true,
//	    EditFile: true,
//	    ListDir: true,
//	    Todo: true,
//	})
//
// Or use Default() and override fields:
//
//	b := tools.Default()
//	b.Bash = false
//	reg, _ := tools.Build(cfg, gate, b)
type BuiltinTools struct {
	Bash      bool // /bin/sh -c with timeout + denylist + gate
	ReadFile  bool // Read a file with offset/limit
	WriteFile bool // Atomic write/create
	EditFile  bool // Single-occurrence string replacement
	ListDir   bool // Sorted directory listing
	Todo      bool // In-process plan tracker
}

// Default returns a BuiltinTools with every tool enabled. This is the
// recommended starting set for any agent that needs to act on its
// workspace.
func Default() BuiltinTools {
	return BuiltinTools{
		Bash:      true,
		ReadFile:  true,
		WriteFile: true,
		EditFile:  true,
		ListDir:   true,
		Todo:      true,
	}
}

// Registry is the assembled built-in tool set returned by Build.
//
// Tools is the slice you pass to agent.WithTools(...).
// Todo is the underlying store, exposed so hosts can render plan
// progress (e.g. for a /todo slash command in a TUI).
type Registry struct {
	Tools []tool.Tool
	Todo  *TodoStore
}

// Build constructs the registry. cfg supplies output-truncation caps;
// gate gates every tool call. Both are required.
//
// We deliberately do NOT set ADK's functiontool.Config.RequireConfirmation
// even when the gate is in "ask" mode. core-agent's gate handles
// approval itself by calling its Prompter from inside each tool
// handler — going through ADK's HITL flow would be a second approval
// round-trip on top of ours.
func Build(cfg *config.Config, gate *permissions.Gate, b BuiltinTools) (*Registry, error) {
	if cfg == nil {
		return nil, fmt.Errorf("tools: cfg is required")
	}
	if gate == nil {
		return nil, fmt.Errorf("tools: gate is required")
	}
	store := NewTodoStore()

	type spec struct {
		on   bool
		name string
		desc string
		ctor func() (tool.Tool, error)
	}
	specs := []spec{
		{b.ReadFile, "read_file", "Read a file from disk and return its contents.", func() (tool.Tool, error) {
			return functiontool.New(functiontool.Config{
				Name: "read_file", Description: "Read a file from disk. Honors offset/limit for large files.",
			}, readFileFunc(gate, cfg))
		}},
		{b.WriteFile, "write_file", "Write or overwrite a file with the given content.", func() (tool.Tool, error) {
			return functiontool.New(functiontool.Config{
				Name: "write_file", Description: "Create or overwrite a file. Asks for confirmation in 'ask' mode.",
			}, writeFileFunc(gate))
		}},
		{b.EditFile, "edit_file", "Replace one occurrence of an exact string in a file.", func() (tool.Tool, error) {
			return functiontool.New(functiontool.Config{
				Name: "edit_file", Description: "Replace exactly one occurrence of old_string with new_string in path.",
			}, editFileFunc(gate))
		}},
		{b.ListDir, "list_dir", "List entries of a directory.", func() (tool.Tool, error) {
			return functiontool.New(functiontool.Config{
				Name: "list_dir", Description: "List the entries (files and subdirectories) of a directory.",
			}, listDirFunc(gate, cfg))
		}},
		{b.Bash, "bash", "Run a shell command and return its output.", func() (tool.Tool, error) {
			return functiontool.New(functiontool.Config{
				Name: "bash", Description: "Execute a shell command via /bin/sh -c with a timeout.",
			}, bashFunc(gate, cfg))
		}},
		{b.Todo, "todo", "Maintain an agent-facing todo list (list/add/set_status/clear).", func() (tool.Tool, error) {
			return functiontool.New(functiontool.Config{
				Name: "todo", Description: "Maintain a short todo list visible to the user. Actions: list, add, set_status, clear.",
			}, todoFunc(store))
		}},
	}

	out := &Registry{Todo: store}
	for _, s := range specs {
		if !s.on {
			continue
		}
		t, err := s.ctor()
		if err != nil {
			return nil, fmt.Errorf("tools: build %s: %w", s.name, err)
		}
		out.Tools = append(out.Tools, t)
	}
	return out, nil
}
