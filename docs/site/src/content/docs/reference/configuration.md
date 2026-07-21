---
title: Configuration
---


## The `.agents/` directory

`core-agent` walks up from the working directory looking for a folder named `.agents/`, analogous to how `git` looks for `.git`. The first match wins. Everything `core-agent` reads or writes for a project lives there:

```
.agents/
├── config.json          # this file — provider, model, permissions, scope, telemetry, etc.
├── mcp.json             # MCP server declarations (see MCP page)
├── skills/              # SKILL.md bundles (see Skills page)
└── sessions/            # one-shot transcripts; auto-written, safe to .gitignore
```

You don't have to create `.agents/` — without it, `core-agent` runs with built-in defaults and skips the project-specific bits (no transcripts, no MCP, no skills). It's required only when you want to customize.

### User-scope directories

Beyond the project `.agents/`, `core-agent` reads a few user-scope paths for assets that follow you across projects:

| Path | Contents | Notes |
|---|---|---|
| `~/.agents/` | `AGENTS.md`, `AGENTS.d/*.md`, `skills/`, `mcp.json` | Portable user assets, layered under project scope but above the legacy `~/.core-agent/` fallback. Use this as the primary user root. |
| `~/.core-agent/` | `AGENTS.md`, `AGENTS.d/*.md`, `skills/`, `pricing.json` | Historical user root plus runtime cache (`pricing.json` — auto-fetched pricing data, `/pricing set` writes). `AGENTS.md` + `skills/` remain read here as a lower-precedence fallback. |

Per-loader precedence (higher-scope entries win on collision):

- **Skills**: `<project>/.agents/skills/` > `~/.agents/skills/` > `~/.core-agent/skills/` — merged via overlay; project wins on skill-name collision.
- **AGENTS.md**: user (`~/.core-agent/`) → user-home (`~/.agents/`) → project — concatenated in order; canonical-path visited-set dedupes cross-scope duplicates.
- **MCP servers**: `<project>/.agents/mcp.json` > `~/.agents/mcp.json` — merged by server-name key; project wins on collision. Non-server fields (`agentic_wrap*`) take the first explicitly-set value.
- **Config**: `<project>/.agents/config.json` only — no user-scope layering today. If you want personal defaults, use the CLI `-c ~/.agents/config.json` to point at a HOME file explicitly.

---

## Multi-file instructions (v2.3+)

`AGENTS.md` is the single-file baseline (with `CLAUDE.md` / `GEMINI.md` as first-match-wins fallbacks). For larger instruction sets, two composition primitives let you split the prompt across multiple files without changing your model or wrapping code.

### Where the loader looks

Both primitives work at **three scopes**, loaded and concatenated in this order:

| Scope | Searched first | Fallback location |
|---|---|---|
| User (`~/.core-agent/`) | `~/.core-agent/.agents/AGENTS.md` and `~/.core-agent/.agents/AGENTS.d/*.md` | `~/.core-agent/AGENTS.md` and `~/.core-agent/AGENTS.d/*.md` |
| User-home (`~/.agents/`) | `~/.agents/AGENTS.md` and `~/.agents/AGENTS.d/*.md` | — (the root IS already `.agents/`; no nested fallback) |
| Project | `<project-root>/.agents/AGENTS.md` and `<project-root>/.agents/AGENTS.d/*.md` | `<project-root>/AGENTS.md` and `<project-root>/AGENTS.d/*.md` |

Each scope's primary file + `AGENTS.d/*.md` are concatenated into the prompt in the order above (user → user-home → project). The per-load canonical-path **dedup** ensures any single file reached from multiple paths (via `@include`, via both AGENTS.d directories, via cross-scope symlinks) loads exactly once.

Why two user-level roots? `~/.agents/` is the portable cross-tool convention — the same layout you'd use inside a project's `.agents/` but at `$HOME`. `~/.core-agent/` is the historical core-agent-specific root and remains supported. Drop your rules in whichever fits; both load additively.

Within the project scope, both locations (`.agents/` subdir and root) load additively — `.agents/AGENTS.md` content appears first, followed by `<root>/AGENTS.md`. Operators following the "everything agent-related lives under `.agents/`" convention drop their files in the subdir; operators following the broader-ecosystem `<project-root>/AGENTS.md` convention (Cursor, Antigravity, Hermes) keep them at root. Both work. Mixing is supported — root `AGENTS.md` as the cross-tool canonical document plus `.agents/AGENTS.md` for core-agent-specific additions is a legitimate layout.

Files within each scope load in this order:

1. User scope (`~/.core-agent/`): primary `AGENTS.md` from either location, then `AGENTS.d/*.md` lexically (from both directories, merged).
2. User-home scope (`~/.agents/`): primary `AGENTS.md`, then `AGENTS.d/*.md` lexically.
3. Project scope: primary `AGENTS.md` (or `CLAUDE.md` / `GEMINI.md`) from either location, then `AGENTS.d/*.md` lexically (from both directories, merged).

### `@include <relative-path>` directive

A line whose entire content is `@include <path>` (with optional leading whitespace) is replaced in-place by the referenced file's content. Useful for layering shared principles + per-project overrides:

```markdown
# Agent instructions

You are a GKE on-call orchestrator for the payments team.

@include base/principles.md
@include workflows/triage.md

## Project-specific overrides

Default cluster: prod-us-central1.
```

Rules:

- **Relative to the including file's directory.** So `AGENTS.md` `@include workflows/triage.md` resolves to `<dir-of-AGENTS.md>/workflows/triage.md`.
- **`../` is permitted** up to the scope root (project root or user-agent dir). Escaping the scope root is an error.
- **Absolute paths and URLs are rejected** — local files only.
- **Cycles handled by dedup** — A → B → A loads A and B once each, no error.
- **Max nesting depth: 8.** Beyond that errors fast (real trees rarely exceed 2–3).
- **Missing target = load error.** Typos surface immediately rather than silently shrinking the system prompt.
- **Inside fenced code blocks** (`` ``` `` or `~~~`) the directive is left literal so docs-about-includes don't expand.
- **Embedded in prose** (e.g. "see @include foo for details") is NOT processed — directive lines only.

### `AGENTS.d/*.md` directory

Drop a directory next to your primary file:

```
.agents/
├── AGENTS.md
└── AGENTS.d/
    ├── 10-principles.md
    ├── 20-tools.md
    └── 30-workflows.md
```

Every top-level `.md` file is loaded in **lexical filename order**, appended after the scope's primary file. Conventions:

- **`.md` only.** Other extensions (`.txt`, `README`) are ignored.
- **Top-level only.** Subdirectories are not recursed.
- **Hidden files skipped** (`.staging.md`, `.draft.md`) — useful for staging work-in-progress entries.
- **Absent directory is fine** — just no fan-in for that scope.

### Frontmatter

A leading YAML frontmatter block (between `---` lines at the very start of a file) is **stripped** before the body is added to the system prompt. The loader does not parse the metadata in v1 — this just keeps editor metadata out of the model's view.

```markdown
---
title: Triage workflow
tags: [oncall, gke]
---

# When an operator pages...
```

A `---` later in the file (used as a markdown horizontal rule) is **not** treated as frontmatter.

### Truncation

Each loaded file is capped at 32 KiB. Files larger than the cap are truncated and the assembled prompt gets a `[...truncated by core-agent at 32768 bytes...]` marker so both the model and the operator know.

### Migration recipes

| From | Recipe |
|---|---|
| Single AGENTS.md | No change. v2 loads existing files identically. |
| **Cursor** (`.cursor/rules/*.mdc`) | Rename the `rules/` directory to `AGENTS.d/` and rename `.mdc` → `.md`. Frontmatter is stripped automatically. |
| **Antigravity** (AGENTS.md with `@include`) | Drop in as-is — the directive syntax is identical. |
| **Hermes** (root-level `AGENTS.md` + `SOUL.md`) | Concatenate or split. To keep both: write a project-root `AGENTS.md` that just contains `@include SOUL.md` (or move `SOUL.md` to `AGENTS.d/20-soul.md`). Note: Hermes's `MEMORY.md` / `USER.md` are runtime memory concerns, not static instructions — they belong in core-agent's shared-memory layer, not the loader. |

### Provenance

The `/memory` slash command (and `Loaded.Sources` from the library API) lists every file that contributed to the assembled prompt — primary, included, and `AGENTS.d/`-scanned — with their canonical paths so you can trace where any line in the prompt came from.

---

## `config.json` schema

Top-level shape, with all fields optional except `version` and `model.name`:

```json
{
  "version": 1,
  "model": { ... },
  "permissions": { ... },
  "path_scope": { ... },
  "agent": { ... },
  "tool_output": { ... },
  "otel": { ... },
  "url_scope": { ... },
  "attach": { ... }
}
```

`version` must be `1`. Other versions are rejected with a clear upgrade message — the schema is bumped only on breaking changes.

A minimal viable config:

```json
{
  "version": 1,
  "model": {
    "provider": "anthropic",
    "name": "claude-opus-4-7"
  }
}
```

---

## `model`

Selects the LLM backend.

| Field | Type | Default | Notes |
|---|---|---|---|
| `provider` | string | `""` (auto-detect) | One of `gemini`, `vertex`, `anthropic`, `anthropic-vertex`. Empty = auto-detect from env. |
| `name` | string | `gemini-3.1-pro-preview-customtools` | Model ID. **Required.** For Gemini, version 3.0 or later is required when using the default tool suite — see [Providers → Gemini 3.0+ required](/concepts/providers/#gemini-30-required-when-combining-built-ins-with-function-tools). The default uses the `-customtools` variant, which is fine-tuned to prefer developer-defined tools over raw bash; same price, same context window. Override with the un-tuned `gemini-3.1-pro-preview` if you need behavior-baseline comparisons. |
| `api_key` | string | `""` | Inline key for `provider: gemini`. Usually unset; read from `GOOGLE_API_KEY` / `GEMINI_API_KEY` at runtime. |
| `vertex` | object | `null` | GCP project + region. Required when `provider: vertex`. |
| `vertex.project` | string | — | GCP project ID. |
| `vertex.location` | string | — | GCP region (e.g. `us-central1`). |
| `anthropic` | object | `null` | Claude-specific settings. |
| `anthropic.api_key` | string | `""` | Inline Anthropic key. Usually read from `ANTHROPIC_API_KEY`. |
| `anthropic.vertex` | object | `null` | When `provider: anthropic-vertex`, holds project + region. |
| `anthropic.vertex.project` | string | — | GCP project ID for Vertex Anthropic. Falls back to `ANTHROPIC_VERTEX_PROJECT_ID` then `GOOGLE_CLOUD_PROJECT`. |
| `anthropic.vertex.location` | string | — | Region (e.g. `us-east5`). Falls back to `CLOUD_ML_REGION` then `GOOGLE_CLOUD_LOCATION`. |
| `pricing` | map | `{}` | Per-model rate overrides keyed by model name (case-insensitive). Survives `/model` switches mid-session — every model the operator routes to can carry its own rates. |
| `pricing.<model>.input_per_mtok` | float | — | USD per 1M input tokens for `<model>`. |
| `pricing.<model>.output_per_mtok` | float | — | USD per 1M output tokens for `<model>`. |

Pricing resolves through a layered chain: this `model.pricing` map → `.agents/pricing.json` (project-local) → `~/.core-agent/pricing.json` (user-global; auto-fetched + manual sections) → compiled-in fallback → longest-prefix match → "$—" (rate unknown).

Example:

```json
{
  "model": {
    "name": "gemini-3.1-pro-preview",
    "pricing": {
      "gemini-3.1-pro-preview":     {"input_per_mtok": 1.25, "output_per_mtok": 5.00},
      "claude-opus-4-7":            {"input_per_mtok": 15.0, "output_per_mtok": 75.0},
      "internal-fine-tuned-v3":     {"input_per_mtok": 0.50, "output_per_mtok": 2.00}
    }
  }
}
```

See [Providers](/concepts/providers/) for full details on each backend.

---

## `permissions`

Configures the permission gate that consults every tool call. See [Permissions](/concepts/permissions/) for the full pattern grammar.

| Field | Type | Default | Notes |
|---|---|---|---|
| `mode` | string | `ask` | One of `ask`, `allow`, `yolo`. |
| `allow` | string[] | `[]` | Allowlist patterns. Format: `<tool>:<glob>` or `<glob>`. |
| `deny` | string[] | `[]` | Denylist patterns. Always wins over allow. |
| `use_builtin_allow` | bool | `true` | Include the built-in read-only bundle in the effective allowlist (reads, greps, `list_dir`, `git status` / `git diff`, etc.). Turn off if you want to allowlist every tool from scratch. |
| `builtin_allow_extras` | string[] | `[]` | Names of additional built-in bundles to fold into the effective allowlist (e.g. `["testing", "linting"]`). See `permissions.Bundles` in the Go source for the current catalog; also configurable interactively via the `/allow-bundle` slash. |

Example:

```json
{
  "permissions": {
    "mode": "ask",
    "allow": ["bash:git status", "bash:git log*", "read_file:internal/**"],
    "deny":  ["bash:sudo *"]
  }
}
```

### Interactive prompts

In `ask` mode the bundled CLI (`core-agent`) prompts on stderr whenever a tool call needs approval. The prompt looks like:

```text
core-agent (permissions): bash wants to run:
  rm -rf /tmp/foo
[y]es once · [s]ession · session-[t]ool · [a]lways · [N]o (default): 
```

Decision keys (case-insensitive, single character + enter):

| Key | Effect |
|---|---|
| `y` | Allow once. Next identical call asks again. |
| `s` | Allow this exact request for the rest of the session. |
| `t` | Allow every call to this tool for the rest of the session. |
| `a` | Allow always. Persists an entry to `.agents/config.json`'s `permissions.allow`. |
| `n` or bare enter | Deny. |

The prompter is auto-wired when stdin is a TTY. Non-TTY callers (piped stdin, CI, `nohup`) get `ErrNoPrompter`-wrapped errors that point at the bypass options below — they don't hang waiting for a non-existent user.

### `--yolo` (CLI flag)

`--yolo` forces the gate into `yolo` mode regardless of `config.permissions.mode`. Equivalent to setting `permissions.mode: "yolo"` in config; takes precedence at the call site so you don't have to edit config to unblock a one-off scripted run. Library callers achieve the same with `permissions.Options{Mode: permissions.ModeYolo}`.

### Plan-first gating (v2.3+) — `require_plan_artifact`

Setting `permissions.require_plan_artifact: true` turns on **substrate-enforced plan-before-action**. The gate denies mutating tool calls (`write_file`/`edit_file`/`delete_file`/`bash`, the `spawn_agent` family, and all MCP tools) until the model has called the `record_plan` built-in tool. Read tools (`read_file`/`read_many_files`/`stat`/`list_dir`/`glob`/`grep`/`json_query`/`fetch_url`/`todo`) and `record_plan` itself remain allowed so research happens normally and the model has an escape valve.

Once `record_plan(plan: <markdown>)` is called, the plan is written to `.agents/plans/plan-<seq>.md` and the gate's `planRecorded` flag flips. From that point on, the configured `mode` resumes its usual semantics — see the composition table below.

```json
{
  "version": 1,
  "permissions": {
    "mode": "ask",
    "require_plan_artifact": true,
    "allow": ["read_file", "read_many_files", "grep", "glob", "list_dir", "stat", "json_query", "fetch_url", "todo"]
  }
}
```

#### Composition

Plan-first composes with every existing mode. Pick the post-plan friction level you want:

| Composition | Behavior after `record_plan` |
|---|---|
| `ask` + `require_plan_artifact` | writes prompt per call ("approve each step") |
| `acceptEdits` + `require_plan_artifact` | writes auto-allow, bash still prompts |
| `yolo` + `require_plan_artifact` | everything auto-allows ("just tell me the plan") |

The third row is the "we just want to know the plan, then go" case — no new mode value needed; `yolo`'s "no prompts" promise still holds *after* the plan; the only deny is the one-time gate before the plan exists.

#### Plan artifacts

Plans persist to `<project-root>/.agents/plans/plan-<seq>.md` with monotonically increasing sequence numbers. When the operator runs `/replan`, the active plan is renamed to `plan-<seq>-revoked.md` (audit trail preserved), the gate flag clears, and the model is forced back through `record_plan` before any further mutating tool will succeed. Sequence numbers continue across revocations so revisions are always identifiable.

| Path | Content |
|---|---|
| `.agents/plans/plan-1.md` | first plan |
| `.agents/plans/plan-2-revoked.md` | operator `/replan`'d this one |
| `.agents/plans/plan-3.md` | currently active plan |

Add `.agents/plans/` to `.gitignore` if you don't want plans checked in. Or do check them in — they make excellent PR descriptions.

#### `/replan` slash command

Available in both the in-process TUI (`core-agent`) and the remote TUI (`core-agent-tui`). Optional reason argument: `/replan reconsider scope`. Effects: archive latest plan → clear gate flag → next mutating call gates again. Operator typically types a follow-up prompt explaining the rejection so the next `record_plan` reflects the new direction.

#### Library callers

```go
gate, err := permissions.FromConfig(cfg, projectRoot, userRoot, prompter)
// or directly:
gate := permissions.New(permissions.Options{
    Mode:                permissions.ModeAsk,
    RequirePlanArtifact: true,
})
// ... after record_plan tool fires its handler ...
gate.IsPlanRecorded() // → true
gate.ClearPlanRecorded() // /replan-like reset; pair with tools.RevokeLatestPlan to also archive
```

`tools.Build` registers the `record_plan` tool only when `permissions.require_plan_artifact: true` AND `agentsDir != ""` (an inert record_plan with nowhere to write would be confusing). Library callers wanting plan-first should pass an `agentsDir` to `tools.Build`.

Full recipe: [`examples/plan-first/`](https://github.com/go-steer/core-agent/tree/main/examples/plan-first) ships three `config.json` variants (one per row of the composition table) plus an AGENTS.md priming the model on the workflow. Design: [`docs/plan-first-design.md`](https://github.com/go-steer/core-agent/blob/main/docs/plan-first-design.md).

### Background subagent prompts (v1.2.0+)

When background subagents are enabled (default; `--no-background-agents` disables them) and one of them triggers a permission prompt in `ask` mode, the heading is prefixed with `[<subagent-name>]` so you know which agent is asking. Concurrent prompts from different subagents are serialized through a mutex — they queue rather than race for stdin.

The subagent inherits the parent's gate wholesale: the same allow/deny lists, the same mode, the same session-level approvals. If you approve `session-tool: bash` while a subagent is asking, every subagent gets the grant for the rest of the session (sibling included). Bounded-subset grants where the parent's model arbitrates out-of-subset requests is deferred to v1.3+.

**Teaching the model to use the spawn tools.** Just registering the tools isn't always enough — most models default to doing things synchronously. Drop a short paragraph into your project's `AGENTS.md` (or pass via `agent.WithInstruction`) describing when background subagents are appropriate (monitoring, fan-out, long bounded delegations). See [Library API → Background subagents → Prompting patterns](/embed/api/#prompting-patterns) for a ready-to-paste system instruction.

### REPL keybindings (v1.3.0+)

The bundled CLI's REPL recognizes Claude Code-style mid-turn interrupts:

| Key | Effect |
|---|---|
| **ESC** | Cancel the current turn. Conversation context is preserved; you can type a redirect. |
| **Ctrl+C** (single) | Same as ESC. Prints a hint that pressing again exits. |
| **Ctrl+C** twice within 1 s | Exit the REPL cleanly. |
| **Ctrl+D** | EOF — exit the REPL. |

Auto-enabled when stdin is a TTY. Disabled silently for piped / non-TTY use (Ctrl+C falls back to the legacy process-level exit). The REPL's startup banner reflects which mode is active. See [Library API → REPL keybindings](/embed/api/#repl-keybindings-v130) for the underlying mechanism.

### Library callers

The `permissions.Prompter` interface is public:

```go
type Prompter interface {
    AskApproval(ctx context.Context, req PromptRequest) (Decision, error)
}
```

`permissions.StdinPrompter(in, out)` is the implementation the CLI uses; wire your own if you have a different UI (a TUI, a web prompt, a chat-based approver, etc.). Pass it via `permissions.FromConfig(cfg, projectRoot, userRoot, prompter)` when constructing the gate.

---

## `path_scope`

Extra paths file tools may touch outside the default project root + user home.

| Field | Type | Default | Notes |
|---|---|---|---|
| `allow` | string[] | `[]` | Patterns. Exact paths, directory trees ending in `/...`, or `path/filepath.Match` globs. Grants both read + write. |
| `allow_paths` | object[] | `[]` | Typed form: each entry is `{ "path": "<pattern>", "mode": "r"\|"w"\|"rw" }` (long forms `read` / `write` / `readwrite` also accepted). Composes with `allow`. Also available as the repeatable `--allow-path PATH:MODE` CLI flag for one-off grants. |

Example:

```json
{
  "path_scope": {
    "allow": [
      "/etc/myapp/...",
      "/var/log/myapp.log",
      "~/scratch/*.json"
    ]
  }
}
```

---

## `agent`

Runtime tuning for the agent loop.

| Field | Type | Default | Notes |
|---|---|---|---|
| `max_steps` | int | `50` | Max tool-call cycles within a single turn before the agent gives up. |
| `max_turn_cost_usd` | float | `0` | Per-turn spend ceiling in USD (0 = disabled). When a single turn's cumulative cost (across all model calls + subtask costs) meets or exceeds this value, the agent emits a `cost_ceiling` turn-error and refuses new turns until `Agent.ResetCostCeiling` is called (interactively: `/resume-after-cost-ceiling`). CLI: `--max-turn-cost-usd`. |
| `max_session_cost_usd` | float | `0` | Session-level spend ceiling in USD (0 = disabled). Cumulative across every turn including subtasks; same trip + refuse behavior. Useful for long-running autonomous deploys where per-turn cost is reasonable but the session total adds up. CLI: `--max-session-cost-usd`. |
| `display_name` | string | `""` | Operator-visible per-deployment label. Rendered in the TUI status-line banner (`core-agent · <name> · ◇ model`) so operators can distinguish between multiple agent deployments across windows. Empty falls back to the bare wordmark. |
| `description` | string | `""` | Human-readable summary of what this agent does. Surfaced by `/.well-known/agent-card.json` when the agent-card endpoint is enabled (see [Agent card](/reference/agent-card/)). Required (via file or `--agent-card-description` flag) to enable that endpoint. |

---

## `session`

Session-scoped defaults picked up on startup.

| Field | Type | Default | Notes |
|---|---|---|---|
| `task_class` | string | `""` | Operator-declared task class — picks a bundle of defaults (model tier, compaction threshold, agentic-tools posture, ask mode) tuned for the kind of work being done. One of `debug`, `implement`, `chat`, `research`, `review`. Empty = no task class (substrate defaults). Explicit config fields + CLI flags always win over the task-class profile. CLI: `--task`. See [Context management → Task class](/concepts/context-management/#task-class). |

---

## `safety`

Startup safety checks that guard against footguns.

| Field | Type | Default | Notes |
|---|---|---|---|
| `small_tier_parent` | string | `"warn"` | What to do when an interactive session resolves to a small-tier parent model (Flash / Haiku-class — these work well as `agentic_*` subtask workers but loop and stall as the parent). One of `warn` / `refuse` / `allow`. `warn` logs a one-line operator notice and proceeds; `refuse` exits with a config error; `allow` suppresses the check. Skipped under `-p`, `--yolo`, or when the model's tier can't be classified. CLI: `--small-tier-parent`. |

---

## `compaction`

Overrides for the automatic context-window compaction trigger. See [Context management → Compaction](/concepts/context-management/) for the full picture.

| Field | Type | Default | Notes |
|---|---|---|---|
| `threshold` | float | tier default | Fraction of the model's context window (0-1) at which compaction fires. When unset, `threshold_by_tier` applies. |
| `threshold_by_tier` | object | see notes | Per-model-tier defaults keyed by tier name (`frontier`, `mid`, `small`, ...). Lets a shared config target different thresholds per model without a per-project override. |

---

## `ui`

Presentation choices for the in-process TUI (`core-agent`). The `/theme` and `/mouse` slash commands write back here when used.

| Field | Type | Default | Notes |
|---|---|---|---|
| `theme` | string | `"auto"` | One of the reserved buckets `auto` / `dark` / `light`, or any named theme from core-tui's BuiltinThemes registry (e.g. `gopher`, `google`). `auto` (or empty) lets core-tui detect the terminal background via OSC-11; explicit `dark` / `light` skips that query. Validation accepts any lowercase `[a-z0-9_-]{1,64}`; unknown names fall back to the auto path at launch. |
| `mouse` | bool | `true` | Terminal mouse capture so the wheel scrolls the chat viewport. When enabled, plain click-drag no longer selects text — hold Shift to select as usual. Toggle at runtime with `/mouse`. |

---

## `tool_output`

Caps tool result size before it enters model context. Prevents a runaway `cat /huge.log` from blowing through your token budget. The built-in tools (`read_file`, `read_many_files`, `write_file`, `edit_file`, `list_dir`, `glob`, `grep`, `bash`, `todo`) honor these caps; consumer-provided tools should call `tools.Truncate(...)` to do the same.

| Field | Type | Default | Notes |
|---|---|---|---|
| `max_bytes` | int | `32768` | Per-tool-result byte cap. |
| `max_lines` | int | `500` | Per-tool-result line cap. |
| `per_tool` | object | see below | Per-tool overrides keyed by tool name. |

Default `per_tool` overrides (apply to the built-in tools that ship with core-agent):

```json
{
  "tool_output": {
    "per_tool": {
      "bash":      { "max_bytes": 65536,  "max_lines": 2000 },
      "read_file":       { "max_bytes": 262144, "max_lines": 5000 },
      "read_many_files": { "max_bytes": 262144, "max_lines": 5000 },
      "glob":            { "max_bytes": 32768,  "max_lines": 500 },
      "grep":            { "max_bytes": 262144, "max_lines": 5000 }
    }
  }
}
```

(`list_dir` falls back to its compile-time default of 32 KB / 500 lines when no override is set; the same for any other unlisted tool.)

core-agent ships these tools by default in the bundled CLI; library callers opt in with `tools.Build(cfg, gate, tools.Default())`. Override per-tool caps with the per-tool block above; add an entry under `per_tool` for any consumer-provided tool that should follow a non-default cap.

---

## `tools`

Controls which built-in tools are wired into the bundled CLI. Defaults to the full set; list entries here to turn specific tools off without disabling the whole suite.

| Field | Type | Default | Notes |
|---|---|---|---|
| `disable` | string[] | `[]` | Built-in tool names to turn off. Valid: `bash`, `read_file`, `read_many_files`, `write_file`, `edit_file`, `list_dir`, `glob`, `grep`, `todo`. Unknown names cause a startup error. |

Example — keep everything except shell access:

```json
{
  "tools": {
    "disable": ["bash"]
  }
}
```

The `--disable-tools=bash,write_file` CLI flag composes with this list by union — anything disabled in either path is off. To turn the entire suite off, use `--no-builtin-tools` (which makes `tools.disable` and `--disable-tools` moot).

---

## `mock`

Configures the `echo` and `scripted` mock providers, plus the orthogonal recording wrapper. See [Providers → Echo](#echo-mock) and [Providers → Scripted](#scripted-mock) for the full story; this section is the schema.

| Field | Type | Default | Notes |
|---|---|---|---|
| `script` | string | `""` | Path to a JSONL transcript. **Required** when `model.provider: scripted`. |
| `strict` | bool | `false` | Scripted: assert each incoming request's `Contents` JSON-equal the recorded request. Catches prompt-construction regressions. |
| `record` | string | `""` | Write a JSONL recording of every LLM turn to this path. Works with **any** provider, not just the mocks. |

Example — record a real Gemini session for later replay:

```json
{
  "model": { "provider": "gemini" },
  "mock":  { "record": "fixtures/last-session.jsonl" }
}
```

Example — replay it under tests:

```json
{
  "model": { "provider": "scripted" },
  "mock":  { "script": "fixtures/last-session.jsonl", "strict": true }
}
```

CLI flags `--script`, `--script-strict`, and `--record-to` override the corresponding fields. `--record-to` is the orthogonal one — it's safe to combine with any provider.

---

## `otel`

OpenTelemetry exporter config. Off by default — a fresh invocation makes zero outbound spans. See the [OpenTelemetry concept page](/concepts/otel/) for enabling, span tree, K8s deployment, and pitfalls.

| Field | Type | Default | Notes |
|---|---|---|---|
| `exporter` | string | `none` | One of `none`, `console`, `otlp`. |
| `endpoint` | string | `""` | OTLP endpoint when `exporter: otlp` (or set via standard `OTEL_EXPORTER_OTLP_ENDPOINT` env). |

Console mode prints span JSON to stderr — useful for local debugging. OTLP mode honors all the standard `OTEL_*` env vars.

### Trace context propagation

Every outbound HTTP the daemon makes (Vertex / Anthropic / Gemini / MCP HTTP / attach peer calls) is wrapped in `otelhttp` and stamped with the W3C `traceparent` header, threading the current span's trace ID into upstream requests. When the OTEL exporter is off, header injection still fires but produces no-op values — hosts running their own tracer above the daemon can rely on continuity without needing to enable the built-in exporter. Inbound attach requests already extract `traceparent`; the propagation change closes the outbound half of the loop.

---

## `pricing` (top-level)

Governs the pricing-catalog refresh — distinct from `model.pricing` above (per-model rate overrides). Defaults: refresh enabled, daily cadence, LiteLLM upstream.

| Field | Type | Default | Notes |
|---|---|---|---|
| `refresh` | bool | `true` | Pull the upstream pricing JSON into `~/.core-agent/pricing.json`'s external section once per day on startup. Set to `false` for air-gapped pods or any environment where outbound network is blocked / undesirable. CLI flag `--no-pricing-refresh` always wins. |
| `source` | string | `https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json` | Upstream URL to fetch. Override for mirrors or internal pricing services. The fetched JSON must match LiteLLM's schema (per-token costs + mode field). |

The refresher uses `If-None-Match` against a stored ETag so re-fetches transfer zero bytes when upstream hasn't changed. Network failures are non-fatal: the existing cache stays in place, a one-line warning ("using N-day-old cache; network: …") goes to stderr, and the session continues.

From the in-process TUI, two slash commands give operators direct control without leaving the chat:

- `/pricing refresh` — force an out-of-cycle fetch from `pricing.source` (ignores the 24h cadence). Useful right after a provider price change. Result lands in the chat scrollback: "Refresh: updated 247 models from upstream" / "Refresh: upstream unchanged" / "Refresh failed; using N-day-old cache".
- `/pricing set <model> <input_per_mtok> <output_per_mtok>` — write a per-model rate to `~/.core-agent/pricing.json`'s `manual` section atomically + rebuild the live catalog so it takes effect immediately. Example: `/pricing set gemini-3.5-flash 0.075 0.30`. The manual section round-trips intact across the daily refresh (the auto-fetcher only rewrites `external`).

## `url_scope`

Governs which URLs the `fetch_url` built-in is allowed to reach. Same Allow/Deny grammar as [`path_scope`](//#path_scope) but for HTTP hosts instead of filesystem paths. `Deny` always wins over `Allow`. An **empty `allow` is default-deny** — `fetch_url` is not registered as a tool at all when no allowlist is configured, so the model can't even attempt a network call without an operator-declared scope.

| Field | Type | Default | Notes |
|---|---|---|---|
| `allow` | string[] | `[]` | Host patterns. `github.com` (exact), `*.googleapis.com` (subdomain wildcard), `*` (any host), `http://localhost:*` (HTTP + any-port opt-in). HTTPS by default — prefix with `http://` to allow plain HTTP for that pattern only. |
| `deny` | string[] | `[]` | Patterns that override `allow` on overlap (same grammar). |
| `max_body_bytes` | int | `65536` | Cap on the response body returned to the model. Per-call `max_bytes` argument can lower this, never raise it. |
| `timeout_seconds` | int | `30` | HTTP timeout per call. |
| `headers` | object | `{}` | Per-host header bundles. Map of host-pattern → header-name → value template. Values pass through `os.ExpandEnv` at request time, so rotated env vars take effect on the next fetch without a restart. Most-specific pattern wins (longer wins; exact match beats wildcard). The model **never** sets headers directly — keeps credential exfiltration off the tool-argument surface. |

Worked example:

```json
{
  "url_scope": {
    "allow": [
      "api.github.com",
      "*.googleapis.com",
      "*.svc.cluster.local",
      "http://localhost:*"
    ],
    "deny": ["*.internal.evil.com"],
    "max_body_bytes":  131072,
    "timeout_seconds": 30,
    "headers": {
      "api.github.com": {
        "Authorization": "Bearer ${GITHUB_TOKEN}",
        "Accept":        "application/vnd.github+json"
      }
    }
  }
}
```

Each fetch emits a `tool/fetch_url` event into the eventlog with structured metadata (`url`, `final_url`, `status`, `content_type`, `bytes`, `truncated`), so an audit query can answer "what URLs did this agent touch, when, and what came back" without parsing tool output. Composes with the [permissions gate](//#permissions) — write `permissions.allow: ["fetch_url:github.com/*"]` to gate per-host even within the URL allowlist.

What's **not** in `fetch_url` (by design):

- **No POST / forms / uploads** — GET only. Use a dedicated MCP server for structured POSTs where the operation can be schema-typed.
- **No JavaScript execution** — use the playwright MCP for dynamic pages.
- **No cookie persistence** — each call is stateless.
- **No model-set auth headers** — headers come from `url_scope.headers` + env expansion only. The model picks the host; the operator picks what auth ships with the request.

CLI conveniences (no config edit needed):

- `--allow-url-host="github.com,*.googleapis.com"` — appends to `url_scope.allow` for the current invocation.
- `--disable-tools=fetch_url` — turns the tool off even if an allowlist is configured.

See [`fetch-url-design.md`](https://github.com/go-steer/core-agent/blob/main/docs/fetch-url-design.md) for the full decision record.

---

## `attach`

Default values for the attach-mode listener and the peer-registration client. Every field below is also exposed as a `--attach-*` CLI flag: names follow the `--attach-<kebab-case-field>` convention (`unix_socket` → `--attach-unix-socket`, `peer_hub` → `--attach-peer-hub`, `register_to` → `--attach-register-to`, and so on). The flag wins when explicitly set, otherwise the config value applies, otherwise the zero value. This section exists for K8s-style deployments where the same settings would otherwise be repeated on every invocation.

String fields are passed through `os.ExpandEnv` so per-pod values like `"https://${POD_IP}:7777"` can live in a shared ConfigMap and resolve to the right address at startup.

| Field | Type | Default | Notes |
|---|---|---|---|
| `listen` | string | `""` | Address the attach HTTP server binds to (e.g. `"0.0.0.0:7777"`). Empty → server off. Mutually exclusive with `unix_socket`. Requires `--session-db` at runtime (the broadcaster pumps from the event log). |
| `unix_socket` | string | `""` | Bind path for the Unix-socket transport (e.g. `"/var/run/core-agent.sock"`). Same SSE protocol; useful for local dev and Cloud Run sidecar shapes. |
| `tls_cert` | string | `""` | TLS server certificate (PEM path). Pair with `tls_key` to enable HTTPS. |
| `tls_key` | string | `""` | TLS server key (PEM path). |
| `client_ca` | string | `""` | CA bundle (PEM path) for client-certificate verification (mTLS). When set, clients must present a cert signed by this CA. |
| `token_env` | string | `""` | **Env var *name*** (not the secret) holding the bearer token clients must present in `Authorization: Bearer <token>`. The secret itself never lives in this file — mount it via your secret manager. |
| `readonly` | bool | `false` | Disable `POST /inject` and `POST /wake`. Read endpoints (`GET /sessions`, `GET .../events`) stay open. |
| `peer_hub` | bool | `false` | Enable peer-registration endpoints (`POST /peers`, `GET /peers`, `POST /peers/<id>/heartbeat`, `DELETE /peers/<id>`) on the listener — this agent becomes a discovery hub. |
| `register_to` | string | `""` | Hub URL this agent registers with on startup (e.g. `"https://hub.default.svc:7777"`). Empty → no registration. Heartbeats automatically until shutdown. |
| `register_endpoint` | string | `""` | Reachable URL the hub records for this agent. Required when `register_to` is set, since the agent's own `listen` value is commonly `0.0.0.0` and not directly reachable. Typically `"https://${POD_IP}:7777"`. |
| `register_name` | string | hostname | Name to register under. Defaults to `os.Hostname()` when empty. Name-based upsert: a restart re-uses the slot rather than orphaning the old entry. |

Worked example for a K8s deployment ConfigMap:

```json
{
  "version": 1,
  "model": { "provider": "vertex", "name": "gemini-3.1-pro-preview-customtools",
             "vertex": { "project": "my-proj", "location": "us-central1" } },
  "attach": {
    "listen":            "0.0.0.0:7777",
    "tls_cert":          "/etc/attach/tls.crt",
    "tls_key":           "/etc/attach/tls.key",
    "client_ca":         "/etc/attach/ca.crt",
    "token_env":         "ATTACH_TOKEN",

    "register_to":       "https://core-agent-hub.default.svc:7777",
    "register_endpoint": "https://${POD_IP}:7777",
    "register_name":     "monitor-${HOSTNAME}"
  }
}
```

See [Attach mode TUI](/reference/attach-tui/) for the protocol and CLI overview, including the `--attach-token=<envvar>` flag that pairs with `token_env`.

### `attach.multi_session`

Nested under `attach`, enables the multi-tenant surface where distinct callers each drive their own session on the same daemon. See [Multi-session](/concepts/multi-session/) for the operator narrative; this table is the field reference.

| Field | Type | Default | Notes |
|---|---|---|---|
| `users_dir` | string | `""` | Directory holding per-caller overlays (`<usersDir>/<callerIdentity>/.agents/`). Empty disables the per-caller overlay path; the daemon behaves as single-user. |
| `auth.kind` | string | `""` | Authentication scheme: `bearer_table` (default when `table_file` is set), `asserted_caller_header`, or `""` (single-user / no per-caller auth). |
| `auth.table_file` | string | `""` | Path to the bearer-token → identity JSON table when `auth.kind == "bearer_table"`. Reloaded on file modification. |
| `admin_identities` | string[] | `[]` | Caller identities granted the admin surface (`/sessions/*` cross-caller reads, `DELETE /sessions/{sid}` against any owner, etc.). Non-admin callers only see their own sessions. |
| `allow_anonymous` | bool | `false` | Accept requests with no caller identity as the daemon-wide anonymous user. Off by default; useful for smoke tests. |
| `default_identity` | string | `""` | Identity used when the caller doesn't present one AND `allow_anonymous` is off. Empty rejects the request. |
| `proxy_identities` | string[] | `[]` | Identities trusted to set `X-Asserted-Caller` on behalf of others (typical: a front-door proxy that has already authenticated). |
| `asserted_caller_header` | string | `"X-Asserted-Caller"` | HTTP header the daemon reads for the pre-authenticated caller identity when the request came from a `proxy_identities` member. |
| `session_idle_timeout` | duration | `"0s"` | Reap sessions with no activity for this long. `0s` = never reap; interactive daemons typically leave off, long-lived multi-tenant daemons might set `"30m"` to prevent unbounded growth. |

---

## Discovery and merge

`core-agent` finds your config like this:

1. **Walk up** from the current working directory looking for a folder named `.agents/`. First match wins.
2. **Read** `<found>/config.json` if present. Missing file → use built-in defaults.
3. **Merge** the loaded JSON over `config.DefaultConfig()` — unspecified fields keep their defaults. Unknown fields are tolerated for forward compatibility.
4. **Validate** the merged result. Bad provider name, missing required field, or wrong schema version → fail fast at startup.

Override discovery with the CLI's `-c <path>` flag, which reads the file directly and treats its parent directory as the agentsDir for MCP / skills resolution.

### Startup summary

Every invocation prints a compact one-line-per-item summary to stderr right after config resolution — the exact model + provider, the source of the config (`.agents/` discovery vs. `-c <path>` vs. built-in defaults), the resolved `agentsDir`, and follow-up notices for MCP servers, skills, and multi-session auth. Use this to confirm at a glance which config actually loaded when a deployment behaves unexpectedly.

```
core-agent: config: source=/home/me/proj/.agents/config.json (via .agents/ discovery)
core-agent: agentsDir: /home/me/proj/.agents
core-agent: model: claude-opus-4-7 provider=anthropic-vertex
core-agent: mcp: 2 server(s) loaded — github(ok), grafana(ok)
core-agent: skills: 3 loaded — code-review, security-review, incident-triage
```

Structured JSON emission for machine consumers isn't wired today; if you need it, parse the stderr lines or open an issue.

Add `-i "seed prompt"` to seed the first turn of an interactive session and stay in the REPL/TUI. See the [interactive quickstart](/run/interactive/quickstart/).

---

## Atomic writes

`config.Save(path, cfg)` writes via temp file + `rename` so a partial write can never leave a corrupt `config.json` on disk. Use it when you build tooling that mutates config (e.g. an `init`-style command, or a `/permissions` slash command in a downstream consumer).

---

## Not in `config.json` — runtime-only flags

A handful of features are CLI-flag-only, with no `config.json` field today (consumers that want them per-project typically wrap the CLI in a script):

| Flag | Documented at |
|---|---|
| `-i` / `--interactive-prompt=TEXT` | [Interactive quickstart → Seed the first turn](/run/interactive/quickstart/) — submit an initial turn on startup and stay in the REPL/TUI. Mutually exclusive with `-p`; incompatible with `--no-repl`. |
| `--allow-path=PATH:MODE` | [Permissions → Path scope](/concepts/permissions/) — grant `r` / `w` / `rw` access to a tree outside project + user-home roots (repeatable). |
| `--ask=stdin\|auto\|off` | [Library API → Prompter](/embed/api/#prompter) |
| `--session-db`, `--session-db-path` | [Sessions and event log](/concepts/sessions/#cli-flags) |
| `--color=auto\|always\|never` | [Library API → Color](/embed/api/#color) |
| `--record-to`, `--script`, `--script-strict` | [Providers → Mock providers](/concepts/providers/) |
| `--no-tui` | [Getting started → Multi-turn TUI](/run/getting-started/) — skip the Bubble Tea TUI even on a TTY (slim build / scripts / unusual terminals) |
| `--log-file=PATH` | Mirror daemon stderr diagnostics to `PATH` in addition to the terminal. Empty or `-` keeps today's stderr-only behavior. Recommended: `/tmp/core-agent.log` so startup errors (MCP init, model resolution, watchdog notices) survive the TUI's screen takeover. Opened in append mode with `0600` perms. |
| `--no-compact` | [Context management → Compaction](/concepts/context-management/) — disable automatic compaction (`/compact` slash still works) |
| `--no-checkpoint` | [Context management → Task-boundary checkpoints](/concepts/context-management/) — disable `/done` slash + `mark_task_done` tool |
| `--agentic-tools` | [Context management → Agentic tool wrappers](/concepts/context-management/) — register the `agentic_*` tool family |
| `--agentic-small-model=ID` | [Context management → Agentic tool wrappers](/concepts/context-management/) — route agentic subtasks to a cheaper model |

The `CORE_AGENT_TUI=internal` environment variable picks the legacy `internal/tui` code path in place of the v2 default (core-tui). One-release escape hatch for operators who hit a regression; scheduled for removal in v2.1.
