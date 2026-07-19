---
title: Built-in tools
weight: 5
---

The model-facing tool catalog `core-agent` registers by default, plus the optional lifecycle tools the runtime wires when the corresponding feature is enabled (checkpoints, ask, autonomous scheduling). For declaring third-party tools, see [MCP servers]({{< relref "/docs/reference/mcp.md" >}}). For writing your own tools from Go, see [Library API]({{< relref "/docs/library/api.md" >}}).

## The built-in catalog

Tools are grouped by domain — files, search, shell, data + network, planning, and interactive prompting. Each is configurable via the `BuiltinTools` struct in `pkg/tools` (library callers) or the `--disable-tools` flag / `tools.disable` config field (CLI users). Every call routes through the [permission gate]({{< relref "/docs/reference/permissions.md" >}}) under the `tool` namespace — denying a tool by pattern keeps it from running even if it's registered. Three tools are conditionally registered: `fetch_url` only when `url_scope.allow` has at least one entry, `record_plan` only when `permissions.RequirePlanArtifact` is on (see [Plan-first enforcement]({{< relref "/docs/reference/permissions.md" >}}#plan-first-enforcement)), and `sciontool_status` only when the `sciontool` binary is on `PATH`.

### File system

| Tool | Purpose | Key parameters |
|---|---|---|
| `read_file` | Read a file with optional `offset` / `limit` for large files. | `path`, `offset?`, `limit?` |
| `read_many_files` | Read a batch in one call. Per-file failures surface as `skipped: "<reason>"` entries — the batch never aborts. **Preferred over parallel `read_file` calls when the file set is known up front.** | `paths?`, `pattern?`, `path?` |
| `write_file` | Atomic create-or-overwrite. Asks for confirmation in `ask` mode. | `path`, `content` |
| `edit_file` | Replace exactly one occurrence of `old_string` with `new_string`. Fails if the string appears zero or multiple times. | `path`, `old_string`, `new_string` |
| `delete_file` | Idempotent removal of a regular file. Refuses directories. **Preferred over `bash rm`** — honors the gate's `CheckFileWrite` and the path scope. | `path` |
| `stat` | Metadata: `size`, `mtime` (RFC3339 UTC), `mode`, `is_dir`. Missing path returns `{exists: false}` instead of erroring — use for "has this been written yet?" without exception handling. | `path` |
| `list_dir` | Sorted directory listing. | `path` |

### Search

| Tool | Purpose | Key parameters |
|---|---|---|
| `glob` | Walk `path` (default `.`) and return file paths whose basename matches a `filepath.Match` pattern (e.g. `*.go`). Skips hidden + vendored directories. | `pattern`, `path?` |
| `grep` | Walk `path` and return matching lines for an RE2 regex. Recursive on directories; single-file mode for files. Returns structured `{path, line, text}` matches the model can pipe into follow-up tool calls without re-parsing. **Preferred over `bash grep` / `bash rg` / `bash find` for code search.** | `pattern`, `path?` |

### Data + network

| Tool | Purpose | Key parameters |
|---|---|---|
| `json_query` | Run a jq expression against JSON loaded from a file or supplied inline. | `expression`, `path?` or `data?` |
| `fetch_url` | HTTP GET against an operator-configured allowlist. **Default-deny**: not registered at all when `cfg.URLScope.Allow` is empty, so the model never sees a tool that would refuse every call. | `url` |

### Shell

| Tool | Purpose | Key parameters |
|---|---|---|
| `bash` | `/bin/sh -c` with a per-call timeout and a denylist of dangerous commands. **Use only for actions the structured tools can't perform**: builds, tests, git, formatters, package managers. The descriptions of `read_file`, `grep`, `glob`, `list_dir`, `stat`, `delete_file` all instruct the model to prefer them over the corresponding `bash` invocation so it doesn't fall back to the shell for things the structured tools handle. | `command`, `timeout?` |

### Planning

| Tool | Purpose | Key parameters |
|---|---|---|
| `todo` | In-process plan tracker. Actions: `list`, `add`, `set_status`, `clear`. Underlying `TodoStore` is exposed via `Registry.Todo` so a TUI can render plan progress (the in-process TUI's `/todo` slash command uses this). | `action`, `id?`, `text?`, `status?` |

### Runtime integration

| Tool | Purpose | Key parameters |
|---|---|---|
| `sciontool_status` | Signal a sticky lifecycle event (`ask_user`, `blocked`, `task_completed`, `limits_exceeded`) to a Scion hub. Registered only when the `sciontool` binary is on `PATH` — outside a Scion container the tool is hidden from the model rather than exposed as a subprocess no-op. See [Scion adapter]({{< relref "/docs/reference/scion-adapter.md" >}}). | `status_type`, `message` |

## Toggling individual tools

CLI:

```bash
core-agent --disable-tools bash,delete_file
```

Library:

```go
b := tools.Default()
b.Disable("bash")     // by canonical name; errors on typos so config typos fail loudly
b.WriteFile = false   // or set the field directly
reg, err := tools.Build(cfg, gate, b)
```

`tools.BuiltinToolNames()` returns the canonical list in struct order — useful for `--help` generation and config validation.

## Output truncation

Every tool's output is capped per-call by `cfg.MaxBytes` and `cfg.MaxLines` (see [Configuration]({{< relref "/docs/reference/configuration.md" >}})). When a result hits the cap, the response includes a `truncated: true` flag and the model sees only the head — preventing a single oversize `grep` or `bash` output from blowing the context window.

For repeated large-output operations, the [agentic wrappers]({{< relref "/docs/reference/context-management.md" >}}) (below) are a stronger answer: they route the bulk output through a cheaper model and return only a digest.

## Permission gating

Every tool call passes through the gate under the `tool` namespace:

```
tool.bash               # the bash tool itself
tool.bash.cmd:rm        # the bash tool, scoped to commands starting with rm
tool.read_file          # the read_file tool
tool.fetch_url          # the fetch_url tool
```

See [Permissions]({{< relref "/docs/reference/permissions.md" >}}) for the full pattern grammar, gate modes (`ask` / `accept-edits` / `plan` / `yolo`), and per-call vs. session-scoped grants.

The gate also enforces two cross-cutting scopes — both default-deny, both configured under `cfg`:

- **`path_scope.allow` / `deny`** — restricts which paths every file tool (`read_file`, `write_file`, `edit_file`, `delete_file`, `list_dir`, `glob`, `grep`, `stat`, `read_many_files`) can touch. Default: project root only.
- **`url_scope.allow`** — restricts which URLs `fetch_url` can reach. Default: empty allowlist means `fetch_url` is not registered at all.

## Optional lifecycle tools

These are registered conditionally based on agent construction. They're not in the `BuiltinTools` struct because their presence depends on which `agent.New` options were passed.

### `mark_task_done`

Auto-registered when `WithCheckpointer` is wired. The model calls it at logical task boundaries with a short description; the runtime fires `Agent.Checkpoint(ctx, taskNote)` which writes a six-section completion record to the session event log as `CustomMetadata["compaction"] = "checkpoint"` and slices the prior history out of future model requests. See [Context management]({{< relref "/docs/reference/context-management.md" >}}).

### `ask_user`

Registered when `--ask=stdin` / `--ask=auto` is set, or when a library caller provides `WithPrompter`. Lets the model ask the operator a question and receive a typed answer. In headless contexts (no TTY, `--ask=auto`) it returns a clean refusal rather than blocking forever.

```text
ask_user(question: "Should I delete the backup file before re-running?", default: "no")
```

### `schedule_next_turn`

Registered in autonomous-runner contexts. Lets the model emit a sleep / wake-at / wake-on-event signal that `agent.RunAutonomous` consumes between turns and feeds to a `Scheduler` implementation (e.g. `SleepScheduler` for long-lived daemons). See [Autonomous runs]({{< relref "/docs/cli/autonomous/operations.md" >}}).

## Agentic wrappers (subtask-routed tools)

By default (or when `tools/agentic` is registered manually in library use), four additional tools join the catalog:

- `agentic_read_file`
- `agentic_fetch_url`
- `agentic_grep`
- `agentic_research`

Each wrapper's handler delegates to `Agent.RunSubtask` against a separate, optionally cheaper model (via `--agentic-small-model=ID`, e.g. `gemini-2.5-flash`). The bulk tool output lands in the subtask's context; only a focused digest flows back to the parent. The wrapper descriptions explicitly tell the model "use INSTEAD OF `read_file` when the file might be large" so the agent reaches for the wrapper at the right moments.

Cost rolls up to the parent's `usage.Tracker` so `/stats` reflects subtask spend transparently. See [Context management]({{< relref "/docs/reference/context-management.md" >}}) for the design and the per-model cost breakdown.

## MCP tools

Tools declared in `.agents/mcp.json` are namespaced under the server name (`mcp.<server>.<tool>`). They route through the same permission gate under that namespace, with the same pattern grammar. The model sees them alongside built-ins; nothing in the catalog distinguishes built-in from MCP at the model interface. See [MCP servers]({{< relref "/docs/reference/mcp.md" >}}) for the declaration schema.

## Custom tools

Library callers can register arbitrary tools via `agent.WithTools`. The ADK `functiontool.New` factory accepts a Go function whose parameters become a JSON-schema declaration the model sees:

```go
declTool, _ := functiontool.New(functiontool.Config{
    Name:        "spell_check",
    Description: "Run aspell against the given text and return misspellings.",
}, func(ctx context.Context, text string) ([]string, error) { /* ... */ })

a := agent.New(model,
    agent.WithTools(declTool),
    agent.WithGate(gate),
)
```

For late binding to the constructed `*Agent` (when a custom tool needs to call back into the agent — `Agent.RunSubtask`, `Agent.AskSideQuestion`, etc.) use `agent.WithPostConstruct(func(*Agent))`. The in-tree `mark_task_done` tool and the agentic wrappers both use this pattern. See [Library API]({{< relref "/docs/library/api.md" >}}) for the full extension surface.

## Tool descriptions are model-facing prompts

A tool's description text shows up verbatim in the system prompt the model sees. `core-agent`'s built-in descriptions are deliberately prescriptive — they tell the model not just what the tool does but **when to prefer it over alternatives** ("use `grep` instead of `bash grep`", "use `read_many_files` instead of parallel `read_file` calls", "use `agentic_read_file` when the file might be large"). The prescriptive framing is what keeps the model from defaulting to `bash` for everything.

If you're authoring your own tools, mirror this pattern: a description that says only "Search files for a pattern" loses the model. One that says "Search files for a pattern. **Use this instead of `bash grep`** when investigating source code — the output is structured and the gate sees the call" wins the routing decision at every turn.
