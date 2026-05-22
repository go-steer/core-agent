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
// Or use Default() and override fields directly or via Disable:
//
//	b := tools.Default()
//	b.Disable("bash")               // by canonical name; errors on typos
//	b.WriteFile = false             // or set the field directly
//	reg, _ := tools.Build(cfg, gate, b)
type BuiltinTools struct {
	Bash          bool // /bin/sh -c with timeout + denylist + gate
	ReadFile      bool // Read a file with offset/limit
	ReadManyFiles bool // Read a batch of files (paths + pattern) in one call
	WriteFile     bool // Atomic write/create
	EditFile      bool // Single-occurrence string replacement
	DeleteFile    bool // Remove a regular file (refuses directories)
	Stat          bool // Metadata (size / mtime / mode / is_dir) for a single path
	ListDir       bool // Sorted directory listing
	Glob          bool // Walk + filepath.Match by basename
	Grep          bool // Walk + RE2 regex per line
	JSONQuery     bool // jq expression over JSON loaded from file or inline string
	FetchURL      bool // HTTP GET against url_scope.allow; URL-allowlist enforced
	Todo          bool // In-process plan tracker
}

// builtinToolNames is the canonical name of every built-in tool, in
// the same order as the BuiltinTools struct fields. Kept private so
// callers can't accidentally mutate it; access via BuiltinToolNames().
var builtinToolNames = []string{
	"bash",
	"read_file",
	"read_many_files",
	"write_file",
	"edit_file",
	"delete_file",
	"stat",
	"list_dir",
	"glob",
	"grep",
	"json_query",
	"fetch_url",
	"todo",
}

// BuiltinToolNames returns a fresh copy of the canonical built-in tool
// names. Order matches the field order in BuiltinTools so callers can
// iterate deterministically.
func BuiltinToolNames() []string {
	out := make([]string, len(builtinToolNames))
	copy(out, builtinToolNames)
	return out
}

// Disable turns off the named tool. Returns an error for unknown names
// so typos in --disable-tools or .agents/config.json fail loudly at
// startup rather than silently leaving the tool on. Calling Disable
// twice with the same name is a no-op.
func (b *BuiltinTools) Disable(name string) error {
	switch name {
	case "bash":
		b.Bash = false
	case "read_file":
		b.ReadFile = false
	case "read_many_files":
		b.ReadManyFiles = false
	case "write_file":
		b.WriteFile = false
	case "edit_file":
		b.EditFile = false
	case "delete_file":
		b.DeleteFile = false
	case "stat":
		b.Stat = false
	case "list_dir":
		b.ListDir = false
	case "glob":
		b.Glob = false
	case "grep":
		b.Grep = false
	case "json_query":
		b.JSONQuery = false
	case "fetch_url":
		b.FetchURL = false
	case "todo":
		b.Todo = false
	default:
		return fmt.Errorf("tools: unknown built-in tool %q (valid: %v)", name, builtinToolNames)
	}
	return nil
}

// Default returns a BuiltinTools with every tool enabled. This is the
// recommended starting set for any agent that needs to act on its
// workspace.
func Default() BuiltinTools {
	return BuiltinTools{
		Bash:          true,
		ReadFile:      true,
		ReadManyFiles: true,
		WriteFile:     true,
		EditFile:      true,
		DeleteFile:    true,
		Stat:          true,
		ListDir:       true,
		Glob:          true,
		Grep:          true,
		JSONQuery:     true,
		// FetchURL is enabled in the Default struct, but Build only
		// registers it when cfg.URLScope.Allow is non-empty — a binary
		// with no allowlist gets no network-reaching tool, matching
		// the default-deny posture in URLScopeConfig.
		FetchURL: true,
		Todo:     true,
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
				Name: "read_file", Description: "Read a file from disk. Honors offset/limit for large files. PREFERRED over `bash cat`/`bash head`/`bash tail` for reading source files — honors output truncation and the permission gate.",
			}, readFileFunc(gate, cfg))
		}},
		{b.ReadManyFiles, "read_many_files", "Read multiple files in a single call (explicit paths and/or glob pattern).", func() (tool.Tool, error) {
			return functiontool.New(functiontool.Config{
				Name: "read_many_files", Description: "Read multiple files in a single call. Pass `paths` (explicit list) and/or `pattern` (basename glob, walked from `path` root; defaults to '.'). PREFERRED over multiple parallel `read_file` calls when you already know the set of files you need — saves turns and is the canonical way to fan-out reads. Useful when investigating a feature spread across several files, comparing implementations, or pulling context for an edit. Gate denials, missing files, and directories surface as entries with `skipped: \"<reason>\"` so the batch never aborts on one bad path.",
			}, readManyFilesFunc(gate, cfg))
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
		{b.DeleteFile, "delete_file", "Remove a regular file.", func() (tool.Tool, error) {
			return functiontool.New(functiontool.Config{
				Name: "delete_file", Description: "Remove a regular file. Idempotent — deleting a missing file is a no-op success. Refuses to delete directories. PREFERRED over `bash rm` — honors the permission gate (CheckFileWrite) and the path scope. Useful for cleaning up baseline / scratch files between scheduled-monitor cycles, log rotation, etc.",
			}, deleteFileFunc(gate))
		}},
		{b.Stat, "stat", "Get metadata (size, mtime, mode, is_dir) for a single path.", func() (tool.Tool, error) {
			return functiontool.New(functiontool.Config{
				Name: "stat", Description: "Return metadata for a single file or directory: size, mtime (RFC3339 UTC), mode, is_dir. A missing path returns {exists: false} rather than an error — use for \"has this been written yet?\" checks without exception handling. PREFERRED over `bash stat`/`bash ls -l` — honors the permission gate and doesn't spawn a subprocess.",
			}, statFunc(gate))
		}},
		{b.ListDir, "list_dir", "List entries of a directory.", func() (tool.Tool, error) {
			return functiontool.New(functiontool.Config{
				Name: "list_dir", Description: "List the entries (files and subdirectories) of a directory.",
			}, listDirFunc(gate, cfg))
		}},
		{b.Bash, "bash", "Run a shell command and return its output.", func() (tool.Tool, error) {
			return functiontool.New(functiontool.Config{
				Name: "bash", Description: "Execute a shell command via /bin/sh -c with a timeout. For code investigation (reading files, searching source, listing directories), prefer the structured `read_file`, `grep`, `glob`, `list_dir` tools — they honor the permission gate and per-tool output caps. Use this tool for actions those tools cannot perform: builds, tests, git, formatters, package managers, and other shell-native workflows.",
			}, bashFunc(gate, cfg))
		}},
		{b.Glob, "glob", "Find files by basename pattern.", func() (tool.Tool, error) {
			return functiontool.New(functiontool.Config{
				Name: "glob", Description: "Walk path (default '.') and return file paths whose basename matches the supplied filepath.Match pattern (e.g. *.go). Skips hidden / vendored directories.",
			}, globFunc(gate, cfg))
		}},
		{b.Grep, "grep", "Search file contents for a regex.", func() (tool.Tool, error) {
			return functiontool.New(functiontool.Config{
				Name: "grep", Description: "Walk path (default '.') and return matching lines for the supplied RE2 regex. Recursive on directories; single-file mode when path points at a file. Skips hidden / vendored directories. PREFERRED over `bash grep`/`bash rg`/`bash find` for code search — honors the permission gate, per-tool output caps, and returns structured `{path, line, text}` matches the model can pipe into follow-up tool calls without re-parsing.",
			}, grepFunc(gate, cfg))
		}},
		{b.JSONQuery, "json_query", "Run a jq expression against JSON loaded from a file or supplied inline.", func() (tool.Tool, error) {
			return NewJSONQueryTool(gate, cfg), nil
		}},
		// fetch_url is gated twice: the BuiltinTools toggle (b.FetchURL)
		// and the URL allowlist (len(cfg.URLScope.Allow) > 0). With no
		// allowlist the tool isn't registered at all — matches
		// URLScopeConfig's default-deny posture and keeps the model
		// from seeing a tool that would refuse every call.
		{b.FetchURL && len(cfg.URLScope.Allow) > 0, "fetch_url", "HTTP GET against an operator-configured URL allowlist.", func() (tool.Tool, error) {
			return NewFetchURLTool(gate, cfg), nil
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
