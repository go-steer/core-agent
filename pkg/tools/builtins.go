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
	"os/exec"

	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"

	"github.com/go-steer/core-agent/v2/pkg/config"
	"github.com/go-steer/core-agent/v2/pkg/permissions"
	"github.com/go-steer/core-agent/v2/pkg/tools/alert"
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
	Alert         bool // Fire an operator-registered webhook alert target
	WaitAndVerify bool // Poll a read-only tool until a condition holds (#648)
	Todo          bool // In-process plan tracker
	RecordPlan    bool // Plan-first artifact + gate-flag flip (record_plan)
	// SciontoolStatus is enabled in the Default struct but Build only
	// registers it when `sciontool` is on PATH — inside a Scion
	// container. Outside Scion the tool would be inert (subprocess
	// no-op) and pointlessly visible in the model's schema.
	SciontoolStatus bool
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
	"alert",
	"wait_and_verify",
	"todo",
	"record_plan",
	"sciontool_status",
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
	case "alert":
		b.Alert = false
	case "wait_and_verify":
		b.WaitAndVerify = false
	case "todo":
		b.Todo = false
	case "record_plan":
		b.RecordPlan = false
	case "sciontool_status":
		b.SciontoolStatus = false
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
		// Alert is enabled in the Default struct but Build only
		// registers it when cfg.Alerts.Targets is non-empty — a binary
		// with no configured targets gets no escalation tool, matching
		// the fetch_url conditional-registration pattern. SSRF-safe by
		// construction: the model can only fire pre-registered targets.
		Alert: true,
		// WaitAndVerify is unconditional: unlike fetch_url and alert it
		// needs no operator-supplied registry to be useful (polling
		// read_file or stat works out of the box), and the bounds it
		// enforces have safe defaults.
		WaitAndVerify: true,
		Todo:          true,
		// RecordPlan is enabled in the Default struct but Build only
		// registers it when an agentsDir is available AND
		// permissions.plan_mode is advisory or required — there's no
		// point exposing record_plan to the model when the operator
		// asked for neither the artifact nor the gate (the call would
		// just be noise). The agentsDir requirement is structural
		// (plans need somewhere to live).
		RecordPlan: true,
		// SciontoolStatus is enabled here but Build only registers it
		// when `sciontool` is on PATH (inside a Scion container).
		SciontoolStatus: true,
	}
}

// whenTool returns note only when the cross-referenced tool is
// registered in this build, and "" otherwise. Use it for any clause in
// a model-facing description that names another tool: the sentence has
// to earn its place by being actionable, and "PREFERRED over `bash
// rm`" is not actionable in a container with no shell — it's a claim
// the model may act on and then have to unlearn.
//
// note carries its own leading space so call sites read as
// concatenation onto a complete base sentence.
func whenTool(present bool, note string) string {
	if !present {
		return ""
	}
	return note
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

// sciontoolOnPath reports whether the `sciontool` binary is on PATH.
// Used at Build time to gate sciontool_status registration — inside a
// Scion container the tool is functional; outside, we hide it.
func sciontoolOnPath() bool {
	_, err := exec.LookPath("sciontool")
	return err == nil
}

// Build constructs the registry. cfg supplies output-truncation caps;
// gate gates every tool call. agentsDir is the resolved .agents/
// directory (may be empty when none was found) and is required only
// by tools that persist artifacts to it (today: record_plan). Both
// cfg and gate are required.
//
// We deliberately do NOT set ADK's functiontool.Config.RequireConfirmation
// even when the gate is in "ask" mode. core-agent's gate handles
// approval itself by calling its Prompter from inside each tool
// handler — going through ADK's HITL flow would be a second approval
// round-trip on top of ours.
func Build(cfg *config.Config, gate *permissions.Gate, agentsDir string, b BuiltinTools) (*Registry, error) {
	if cfg == nil {
		return nil, fmt.Errorf("tools: cfg is required")
	}
	if gate == nil {
		return nil, fmt.Errorf("tools: gate is required")
	}
	store := NewTodoStore()

	// Tell the gate which native search tools this build registers, so
	// the bash search gate (#158) only refuses a shape it can actually
	// redirect. A recipe that drops `grep` from the catalog and keeps
	// `bash` would otherwise get a refusal naming a tool the model
	// cannot call. Set before the specs are constructed because the
	// bash tool's own description reads it back.
	gate.SetNativeSearchTools(map[string]bool{"grep": b.Grep, "glob": b.Glob})

	type spec struct {
		on   bool
		name string
		desc string
		ctor func() (tool.Tool, error)
	}
	specs := []spec{
		{b.ReadFile, "read_file", "Read a file from disk and return its contents.", func() (tool.Tool, error) {
			return functiontool.New(functiontool.Config{
				Name: "read_file", Description: "Read a file from disk. Honors offset/limit for large files, output truncation, and the permission gate." +
					whenTool(gate.HasTool("bash"), " PREFERRED over `bash cat`/`bash head`/`bash tail` for reading source files."),
			}, readFileFunc(gate, cfg))
		}},
		{b.ReadManyFiles, "read_many_files", "Read multiple files in a single call (explicit paths and/or glob pattern).", func() (tool.Tool, error) {
			return functiontool.New(functiontool.Config{
				Name: "read_many_files", Description: "Read multiple files in a single call. Pass `paths` (explicit list) and/or `pattern` (basename glob, walked from `path` root; defaults to '.'). The canonical way to fan out reads when you already know the set of files you need — saves turns. Useful when investigating a feature spread across several files, comparing implementations, or pulling context for an edit. Gate denials, missing files, and directories surface as entries with `skipped: \"<reason>\"` so the batch never aborts on one bad path." +
					whenTool(gate.HasTool("read_file"), " PREFERRED over multiple parallel `read_file` calls."),
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
				Name: "delete_file", Description: "Remove a regular file. Idempotent — deleting a missing file is a no-op success. Refuses to delete directories. Honors the permission gate (CheckFileWrite) and the path scope. Useful for cleaning up baseline / scratch files between scheduled-monitor cycles, log rotation, etc." +
					whenTool(gate.HasTool("bash"), " PREFERRED over `bash rm`."),
			}, deleteFileFunc(gate))
		}},
		{b.Stat, "stat", "Get metadata (size, mtime, mode, is_dir) for a single path.", func() (tool.Tool, error) {
			return functiontool.New(functiontool.Config{
				Name: "stat", Description: "Return metadata for a single file or directory: size, mtime (RFC3339 UTC), mode, is_dir. A missing path returns {exists: false} rather than an error — use for \"has this been written yet?\" checks without exception handling. Honors the permission gate." +
					whenTool(gate.HasTool("bash"), " PREFERRED over `bash stat`/`bash ls -l` — doesn't spawn a subprocess."),
			}, statFunc(gate))
		}},
		{b.ListDir, "list_dir", "List entries of a directory.", func() (tool.Tool, error) {
			return functiontool.New(functiontool.Config{
				Name: "list_dir", Description: "List the entries (files and subdirectories) of a directory.",
			}, listDirFunc(gate, cfg))
		}},
		{b.Bash, "bash", "Run a shell command and return its output.", func() (tool.Tool, error) {
			return functiontool.New(functiontool.Config{
				Name: "bash", Description: bashDescription(gate),
			}, bashFunc(gate, cfg))
		}},
		{b.Glob, "glob", "Find files by basename pattern.", func() (tool.Tool, error) {
			return functiontool.New(functiontool.Config{
				Name: "glob", Description: "Walk path (default '.') and return file paths whose basename matches the supplied filepath.Match pattern (e.g. *.go). Skips hidden / vendored directories.",
			}, globFunc(gate, cfg))
		}},
		{b.Grep, "grep", "Search file contents for a regex.", func() (tool.Tool, error) {
			return functiontool.New(functiontool.Config{
				Name: "grep", Description: "Walk path (default '.') and return matching lines for the supplied RE2 regex. Recursive on directories; single-file mode when path points at a file. Skips hidden / vendored directories. Honors the permission gate and per-tool output caps, and returns structured `{path, line, text}` matches you can pipe into follow-up tool calls without re-parsing." +
					whenTool(gate.HasTool("bash"), " PREFERRED over `bash grep`/`bash rg`/`bash find` for code search."),
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
		// alert is gated twice, mirroring fetch_url: the BuiltinTools
		// toggle (b.Alert) and the target registry (len(cfg.Alerts.Targets)
		// > 0). With no targets the tool isn't registered — the model
		// never sees an `alert` in its schema, and SSRF is impossible by
		// construction (no arbitrary-URL parameter; only named targets).
		{b.Alert && len(cfg.Alerts.Targets) > 0, "alert", "Fire an operator-registered webhook alert target.", func() (tool.Tool, error) {
			return alert.New(gate, cfg)
		}},
		// wait_and_verify is inert until something binds a tool
		// catalog to it (tools.BindCatalogs, called from agent.New).
		// Registering it anyway is deliberate: the binding is the
		// runtime's job, and a host that forgets it gets an explicit
		// "no tool catalog is bound" error instead of a tool that
		// silently vanished from the model's schema.
		{b.WaitAndVerify, WaitAndVerifyToolName, "Poll a read-only tool until its result satisfies a condition.", func() (tool.Tool, error) {
			opts := WaitAndVerifyOptionsFromConfig(cfg)
			opts.BashRegistered = gate.HasTool("bash")
			return NewWaitAndVerifyTool(cfg, opts)
		}},
		{b.Todo, "todo", "Maintain an agent-facing todo list (list/add/set_status/clear).", func() (tool.Tool, error) {
			return functiontool.New(functiontool.Config{
				Name: "todo", Description: "Maintain a short todo list visible to the user. Actions: list, add, set_status, clear.",
			}, todoFunc(store))
		}},
		// record_plan is registered only when (a) the operator asked
		// for it via permissions.plan_mode (advisory OR required —
		// advisory mode wants the artifact without the gate) AND
		// (b) we have an agentsDir to persist plans into. Otherwise
		// the tool would either be inert (no artifact wanted, no gate
		// to flip) or broken (nowhere to write). Skipping registration
		// is cleaner than registering a no-op.
		{b.RecordPlan && cfg.Permissions.PlanToolRegistered() && agentsDir != "", "record_plan", "Record the agent's plan and unblock plan-first gating.", func() (tool.Tool, error) {
			return RecordPlan(gate, agentsDir)
		}},
		// sciontool_status is registered only when `sciontool` is
		// on PATH — i.e. inside a Scion container. Outside Scion the
		// tool would degrade to a subprocess no-op, so we hide it
		// from the model entirely rather than pollute the schema.
		// Matches the fetch_url / record_plan conditional-registration
		// pattern (both are also `on && something_else` gated).
		{b.SciontoolStatus && sciontoolOnPath(), "sciontool_status", "Signal a sticky lifecycle event to Scion.", NewSciontoolStatusTool},
	}

	// Tell the gate what this build registers, so description text can
	// drop cross-references to tools that aren't here. Descriptions
	// routinely name other tools ("PREFERRED over `bash cat`", "call
	// this BEFORE any write_file / bash call"); on a distroless deploy
	// with no shell those sentences assert a capability the model
	// doesn't have, and it spends turns discovering that. Same failure
	// the search gate's ActiveNativeSearchTools already guards against,
	// pointed the other way.
	//
	// Derived from specs rather than from BuiltinTools so the
	// conditionally-registered tools (fetch_url, alert, record_plan,
	// sciontool_status) are reported by what actually happens, not by
	// the toggle alone — the map can't drift from the `on` expression
	// because it IS the `on` expression. Must run before the ctor
	// loop below: that's where descriptions are baked.
	catalog := make(map[string]bool, len(specs))
	for _, s := range specs {
		catalog[s.name] = s.on
	}
	gate.SetRegisteredTools(catalog)

	// Tell the gate every built-in this build registered, so record_plan
	// can name the set plan-first gating actually covers rather than
	// assert "mutating tools" at a recipe that disabled all of them
	// (#747). The gate drops the plan-exempt names itself — Build
	// declares what it registered, the gate decides what that means.
	// Called unconditionally, including when the loop below registers
	// nothing plan-gated: "the gate is inert in this build" is a true
	// answer and only a host that spoke can give it.
	registered := make([]string, 0, len(specs))

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
		registered = append(registered, s.name)
	}
	gate.RegisterPlanGatedTools(registered...)
	return out, nil
}
