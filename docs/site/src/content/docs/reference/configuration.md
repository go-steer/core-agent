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
  "content_roots": [ ... ],
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

### How the file is written back

Interactive flows (`/allow`, `/deny`, `/model`, `/theme`, "always allow this
path") edit `config.json` in place. The writer is deliberately conservative:

- **Partial stays partial.** Only the sections you actually set are written —
  substrate defaults are never materialized into the file. This keeps a future
  bump to a default (e.g. the default model) reaching you instead of being
  pinned to whatever was current when the file was first written.
- **Unknown keys are preserved.** A section written by a newer build is kept
  verbatim on round-trip, so an older build editing the file no longer drops
  fields it doesn't recognize. A misspelled key (e.g. `permisions`) is
  preserved but logs a warning at load, since it otherwise has no effect.
- **Permissions are protected.** A new file is created mode `0600` (the schema
  can hold `api_key` values); an existing file keeps whatever mode it already
  has — the writer never widens it.

---

## `model`

Selects the LLM backend.

| Field | Type | Default | Notes |
|---|---|---|---|
| `provider` | string | `""` (auto-detect) | One of `gemini`, `vertex`, `anthropic`, `anthropic-vertex`. Empty = auto-detect from env. |
| `name` | string | `gemini-3.6-flash` | Model ID. **Required.** For Gemini, version 3.0 or later is required when using the default tool suite — see [Providers → Gemini 3.0+ required](/concepts/providers/#gemini-30-required-when-combining-built-ins-with-function-tools). The default is a current-generation, generally-available flash model that combines server-side search built-ins with function tools out of the box. Override with a pro-class model, or with the `gemini-3.1-pro-preview-customtools` variant (fine-tuned to prefer developer-defined tools over raw bash), when you want that behavior. |
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
| `mode` | string | `ask` | One of `ask`, `allow`, `yolo`, `plan`, `acceptEdits`. `acceptEdits` auto-allows **all** file writes including out-of-scope paths — sandbox-only posture; see [Permissions → Modes](/concepts/permissions/#modes). |
| `allow` | string[] | `[]` | Allowlist patterns. Format: `<tool>:<glob>` or `<glob>`. |
| `deny` | string[] | `[]` | Denylist patterns. Always wins over allow. |
| `use_builtin_allow` | bool | `true` | Include the built-in read-only bundle in the effective allowlist (reads, greps, `list_dir`, `git status` / `git diff`, etc.). Prefix-matched bash entries only auto-allow single literal simple commands — chained/piped/redirected commands and dangerous `find` predicates (`-exec`, `-delete`, …) still prompt; see [Permissions → Safe-command guard](/concepts/permissions/#safe-command-guard-on-bash-prefix-rules). Turn off if you want to allowlist every tool from scratch. |
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

Setting `permissions.require_plan_artifact: true` turns on **substrate-enforced plan-before-action**. The gate denies mutating tool calls (`write_file`/`edit_file`/`delete_file`/`bash`, `fetch_url`, the `spawn_agent` family, and all MCP tools) until the model has called the `record_plan` built-in tool. Read tools (`read_file`/`read_many_files`/`stat`/`list_dir`/`glob`/`grep`/`json_query`/`todo`) and `record_plan` itself remain allowed so research happens normally and the model has an escape valve. `fetch_url` is deliberately **plan-gated** (v2.8+): it is network egress with a model-controlled URL — an exfiltration channel — so it only unlocks once a plan is recorded, like every other action tool.

Once `record_plan(plan: <markdown>)` is called, the plan is written to `.agents/plans/plan-<seq>.md` and the gate's `planRecorded` flag flips. From that point on, the configured `mode` resumes its usual semantics — see the composition table below.

```json
{
  "version": 1,
  "permissions": {
    "mode": "ask",
    "require_plan_artifact": true,
    "allow": ["read_file", "read_many_files", "grep", "glob", "list_dir", "stat", "json_query", "todo"]
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

#### CLI: `--plan-first` (v2.9+)

`--plan-first` is the command-line mirror of `require_plan_artifact`, and `--plan-first=false` is how you opt out of a [task class](/concepts/context-management/#tools-and-plan-first-since-v29) that turns the gate on. Precedence: `--plan-first` (either value) > `require_plan_artifact: true` in config > the task-class default > off. The task class can only turn the gate **on** — an operator who wrote `true` in config is never overruled by a class default.

The binary refuses to hand you a gate you can't clear. If `record_plan` won't register — no `.agents/` directory, `--no-builtin-tools`, or the tool sitting in `tools.disable` / `--disable-tools` — a task class's plan-first default is suppressed and startup says which of those it was. An explicit `--plan-first` or config `true` is still honored there, because you asked for it out loud, but startup warns that every mutating call will be denied with no way to clear the flag: `/replan` revokes a plan, it can't grant one.

Full recipe: [`examples/plan-first/`](https://github.com/go-steer/core-agent/tree/main/examples/plan-first) ships three `config.json` variants (one per row of the composition table) plus an AGENTS.md priming the model on the workflow. Design: [`docs/plan-first-design.md`](https://github.com/go-steer/core-agent/blob/main/docs/plan-first-design.md).

### Background subagent prompts (v1.2.0+)

When background subagents are enabled (default; `--no-background-agents` disables them) and one of them triggers a permission prompt in `ask` mode, the heading is prefixed with `[<subagent-name>]` so you know which agent is asking. Concurrent prompts from different subagents are serialized through a mutex — they queue rather than race for stdin.

The subagent inherits the parent's gate wholesale: the same allow/deny lists, the same mode, the same session-level approvals. If you approve `session-tool: bash` while a subagent is asking, every subagent gets the grant for the rest of the session (sibling included). Bounded-subset grants where the parent's model arbitrates out-of-subset requests is deferred to v1.3+.

**Teaching the model to use the spawn tools.** Just registering the tools isn't always enough — most models default to doing things synchronously. Drop a short paragraph into your project's `AGENTS.md` (or pass via `agent.WithExtraInstruction`, which composes with the layered baseline — v2.8) describing when background subagents are appropriate (monitoring, fan-out, long bounded delegations). See [Library API → Background subagents → Prompting patterns](/embed/api/#prompting-patterns) for a ready-to-paste system instruction.

### Declarative subagents (v2.9+)

A top-level `subagents` array declares a **fixed roster** of named delegates the parent can call by name — the config-driven counterpart to runtime `spawn_agent`. Each entry becomes a tool on the parent (named after the subagent), invoked like any other tool; the subagent runs headless in its own session on its own model and returns a digest. Unlike `spawn_agent`, the roster is authored ahead of time, so it deploys as part of a single `config.json` (one Kubernetes ConfigMap, for instance) rather than being decided at runtime.

```jsonc
{
  "model": { "provider": "vertex", "name": "gemini-3.5-flash" },
  "subagents": [
    {
      "name": "cluster",
      "description": "Read-only cluster investigator. Delegate GKE reads here.",
      "instructions": "@include ./personas/cluster.md",  // inline text or an @include chain
      "model": { "provider": "vertex", "name": "gemini-3.5-flash" },  // omit to inherit the parent's model
      "max_depth": 1,                     // recursion cap; 0 = substrate default
      "tools": ["read_file", "grep"],     // built-in allowlist
      "mcp": ["gke-readonly"],            // MCP servers by name (from mcp.json)
      "skills": ["fleet-audit"],          // skills by name (from skills/)
      "root": "../cluster"                // optional: load own AGENTS.md + skills/ + mcp.json from a content root
    }
  ]
}
```

**Fields.** `name` (unique, required) and `description` (required — the parent's model reads it to decide when to delegate). Since v2.9 the pair is injected into the `spawn_agent` schema itself (name + description in the tool description, and `agent` constrained to an enum of the configured names unless ad-hoc spawns are enabled), so a parent can route to a subagent its persona never mentions; write the description as *when to delegate here*, not as a title. `instructions` is the subagent's persona, inline or an `@include` chain expanded through the same scope-confined loader the parent's memory uses; it lands in the user-instruction layer, so the harness contract stays intact beneath it. `model` is its own `ModelConfig` (resolved through the same provider path the parent uses) or omitted to inherit the parent's model. `max_depth` caps how deep this subagent may itself nest.

**Tool-surface scoping — the nil / list / empty contract.** `tools`, `mcp`, and `skills` each narrow one dimension of the parent's surface, and each obeys the same three-way rule:

| Field value | Meaning |
|---|---|
| **omitted** (nil) | **Inherit** the parent's full set for that dimension |
| **non-empty list** | **Scope** to exactly the named entries (an unknown name is a fail-loud config error) |
| **empty list** (`[]`) | **Grant none** of that dimension |

So `"mcp": ["gke-readonly"]` gives the subagent only that one server's toolset (not the parent's `gke`), `"mcp": []` gives it no MCP at all, and omitting `mcp` lets it see every server the parent has. Scoping never re-runs `mcp.Build` or re-walks `skills/` — a scoped subagent reuses the parent's already-started MCP toolsets and a name-filtered view of the parent's loaded skills, and every inherited tool still carries the parent's permission gate, so a subagent cannot escalate past what the operator granted the parent.

**Independent content root — the `root` field.** Inline `tools`/`mcp`/`skills` can only *narrow* the parent's surface; they cannot give a subagent a persona, skill, or server the parent doesn't also load. When a delegate needs its **own** content — for least privilege (a skill the fleet parent must never reason with) or clean separation (sibling recipe trees under one image) — set `root` to a directory the subagent loads as its **own** scope:

- **Instructions** auto-assemble from `<root>/AGENTS.md` (plus `<root>/AGENTS.d/`), with `@include` confined to the root. An inline `instructions` field still overrides.
- **Skills** load from `<root>/skills/` via a dedicated walk — the subagent's own bundle, independent of the parent's.
- **MCP servers** start from `<root>/mcp.json`, private to the subagent.

With `root` set, the nil / list / empty contract for `mcp`/`skills` still applies but filters **within the root** (omit = all of the root's; a list scopes; `[]` grants none); `tools` remains a built-in allowlist resolved against the binary. A relative `root` resolves against the same base as [`content_roots`](#content_roots-v29) (the agents dir when the config was discovered under one, else the cwd); an absolute path passes through. `root` is operator-declared trust — it is **not** confined to the project root (the sibling-tree case needs `../cluster`) — but a missing or non-directory path is a **loud startup error**, and the subagent stays bound by the parent's permission gate: an independent *content* surface is never a *privilege* escalation.

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
| `max_turn_cost_usd` | float | `0` | Per-turn spend ceiling in USD (0 = disabled). When a single turn's cumulative cost (across all model calls + subtask costs) meets or exceeds this value, the agent emits a `cost_ceiling` turn-error and refuses new turns until the operator resets it (`/guardrail reset`, or `POST /sessions/{id}/guardrails/reset`). CLI: `--max-turn-cost-usd`. |
| `max_session_cost_usd` | float | `0` interactive / `10.00` unattended | Session-level spend ceiling in USD (0 = disabled). Cumulative across every turn including subtasks; same trip + refuse behavior. Recovering from a session trip needs `/guardrail reset +<usd>` (or `additional_budget_usd`) — a bare reset re-trips, since the accumulator is already past the bar. When neither this field nor `--max-session-cost-usd` is set, unattended runs (`-p`, `--no-repl`, or a non-TTY stdin) get a `$10.00` backstop and interactive runs stay disabled. Set this field to `0` — or pass `--max-session-cost-usd=0` — to opt an unattended run back out. CLI: `--max-session-cost-usd` (overrides this field, including an explicit `0`). |
| `display_name` | string | `""` | Operator-visible per-deployment label. Rendered in the TUI status-line banner (`core-agent · <name> · ◇ model`) so operators can distinguish between multiple agent deployments across windows. Empty falls back to the bare wordmark. |
| `description` | string | `""` | Human-readable summary of what this agent does. Surfaced by `/.well-known/agent-card.json` when the agent-card endpoint is enabled (see [Agent card](/reference/agent-card/)). Required (via file or `--agent-card-description` flag) to enable that endpoint. |
| `append_system_prompt` | string | `""` | Operator text appended to the assembled system prompt as its final layer (v2.8, #459). The built-in harness contract, provider quirks, and mode overlay stay intact underneath — this is the encouraged customization path. CLI: `--append-system-prompt <text\|@file>` (flag beats config). |
| `system_prompt_file` | string | `""` | Path to a file whose contents **replace** the assembled system prompt wholesale. You lose the harness contract (compaction summaries arrive unexplained; tool-dispatch rules are gone) — tool-use degradation is on you; prefer `append_system_prompt`. CLI: `--system-prompt-file` (flag beats config). |

### `agent.auto_continue`

Automatic continuation of interrupted turns ([design](https://github.com/go-steer/core-agent/blob/main/docs/auto-continue-design.md), #539/#558/#559). When a daemon-hosted agent finds a fresh interrupted turn in its committed history — an unanswered user message, a dangling tool call, or a repaired-but-unconsumed tool result — it queues a synthesized system-note turn ("the previous turn did not complete… continue") instead of waiting for the next human message. The note describes only what was detected, not a cause: interruption is inferred from tail shape and the retry loop below fires it with no restart, so it never claims a "daemon restart" (#615). Continuation turns are queued under the `core-agent/auto-continue` identity (visible in the audit log; if the note happens to drain into one turn with a concurrently-arriving human message, the human becomes the turn originator and the note text still marks the turn), guarded by the session run lock against double-continuation in shared-DB fleets, and spend under the session's normal cost ceilings. Self-healing (v2.8, #575): a continuation that fails transiently is retried in-lifetime up to the per-session cap (3 attempts / hour, minutes apart) rather than waiting for a reboot or a human message — `retry` controls this and is on by default when the feature is enabled. A true crash loop still bottoms out on the crash-loop breaker (3 attempting boots / 10 min → stand down). Deliberately-interrupted turns (`POST /interrupt`) are never resurrected.

**On by default when it can apply (#559):** a multi-session daemon or a headless `--no-repl` daemon, both with `--session-db` (there is nothing to detect against without a durable eventlog). It stays off — silently — for interactive REPL/TUI runs and in-process library use, so those callers are never surprised by unattended token spend; set `enabled: false` to opt out anywhere. When it turns on by default (rather than by an explicit `enabled: true`) the daemon logs a one-line notice naming the opt-out knob. Three triggers share the machinery: lazy resume on first touch (multi-session), a bounded boot-time scan over persisted sessions (multi-session), and the startup session of a headless `--no-repl` daemon (#558 — the `examples/gke-deploy` shape, checked once at boot). Autonomous runs are unaffected — they have their own checkpoint/resume machinery.

| Field | Type | Default | Notes |
|---|---|---|---|
| `enabled` | bool (tristate) | *unset* | Tristate. Omit for the precondition-gated default (on for a multi-session or `--no-repl` daemon with `--session-db`, off elsewhere). `true` forces it on where it can apply (and warns-and-ignores where it cannot); `false` is a hard opt-out that survives the default flip. |
| `freshness` | duration | `"1h"` | Only interruptions younger than this are continued; staler ones wait for the next real message. Explicit `"0s"` disables the window (always continue). |
| `max_per_boot` | int | `10` | Caps how many sessions the boot-time scan continues in one daemon start, oldest interruption first; the rest are logged and resume on touch. The lazy-resume trigger ignores it. |
| `retry` | bool | `true` | In-lifetime self-heal (#575): re-runs the guarded pass on `retry_interval` so a transiently-failed continuation recovers without a reboot. Still bounded by the per-session cap and the crash-loop breaker. Set `false` for the old one-shot-per-boot behaviour. |
| `retry_interval` | duration | `"5m"` | How often the retry driver re-checks. Must be `> 0`. The per-session single-retry window (10 min) is the effective cadence regardless, so shorter intervals mostly just re-check sooner after a lock frees. |

Opt out (the only line most daemons need, now that it defaults on):

```json
{ "agent": { "auto_continue": { "enabled": false } } }
```

Or tune the on-by-default behaviour — every field is optional:

```json
{ "agent": { "auto_continue": { "freshness": "1h", "max_per_boot": 10, "retry": true, "retry_interval": "5m" } } }
```

### System prompt layers (v2.8)

Since #459 the system prompt is assembled from ordered layers, stable → volatile:

| # | Layer | Source | Changes when |
|---|-------|--------|--------------|
| 1 | Core contract | `agent.CoreInstruction` (compaction/handover framing, tool-dispatch rules) | core-agent release |
| 2 | Provider quirks | selected from the model identifier (Gemini: parallelism mandate; Claude: none) | model changes |
| 3 | Mode overlay | interactive (default) or autonomous via `agent.WithMode` | mode changes |
| 4 | User memory | the instruction loader (AGENTS.md and friends, user → project → per-caller) | project/session |
| 5 | Operator append | `append_system_prompt` / `--append-system-prompt` / `agent.WithExtraInstruction` | deployment |

Later layers win on conflict — your AGENTS.md overrides the built-in communication defaults, but nothing short of `system_prompt_file` overrides the core contract. The `/memory` slash (and attach endpoint) reports the active layer stack as its first row.

---

## `session`

Session-scoped defaults picked up on startup.

| Field | Type | Default | Notes |
|---|---|---|---|
| `task_class` | string | `""` | Operator-declared task class — picks a bundle of defaults (model tier, compaction threshold, agentic-tools posture, ask mode, built-in tool set, plan-first posture) tuned for the kind of work being done. One of `debug`, `implement`, `chat`, `research`, `review`. Empty = no task class (substrate defaults). Explicit config fields + CLI flags always win over the task-class profile. CLI: `--task`. See [Context management → Task class](/concepts/context-management/#task-class). |

---

## `safety`

Startup safety checks that guard against footguns.

| Field | Type | Default | Notes |
|---|---|---|---|
| `small_tier_parent` | string | `"warn"` | What to do when an interactive session resolves to a small-tier parent model (Flash / Haiku-class — these work well as `agentic_*` subtask workers but loop and stall as the parent). One of `warn` / `refuse` / `allow`. `warn` logs a one-line operator notice and proceeds; `refuse` exits with a config error; `allow` suppresses the check. Skipped under `-p`, `--yolo`, or when the model's tier can't be classified. CLI: `--small-tier-parent`. |
| `watchdog` | string | mode-dependent | Behavioral-watchdog posture, as a ladder: `off` (no observation), `warn` (observe + alert the operator), `feedback` (warn + inject the observation into the model's next turn as a `[watchdog]` block), `enforce` (feedback + halt until `/guardrail reset`). Unset resolves to **`enforce` for unattended runs** (`-p`, `--no-repl`, or a non-TTY stdin) and **`warn` for interactive REPL/TUI runs**. Set it here so a recipe ships its own backstop instead of relying on every invocation passing the flag. CLI: `--watchdog` (overrides this field). See [Context management → Watchdog](/concepts/context-management/#watchdog-behavioral-observer--since-v25). |
| `bash_search_gate` | string | `"enforce"` | What happens when the model reaches for a search-shaped shell command — `grep`/`egrep`/`fgrep`/`rg`/`ag`/`ack` or `find`/`fd` — while the native `grep` / `glob` tools are registered. `enforce` refuses the call with an error naming the native tool; `warn` runs it and attaches a `notice` to the result; `allow` disables the check. Piped searches (`go test ./... \| grep -v ok`) and `find` with an action predicate (`-delete`, `-exec`) are never gated — the native tools can't do those. Nor is a binary whose replacement this build didn't register: put `grep` in `tools.disable` and `bash grep` stops being refused, since the refusal would name a tool the model can't call. CLI: `--bash-search-gate`. See [Tools → The bash search gate](/concepts/tools/#the-bash-search-gate). |

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
| `disable` | string[] | `[]` | Built-in tool names to turn off. Valid: `bash`, `read_file`, `read_many_files`, `write_file`, `edit_file`, `delete_file`, `stat`, `list_dir`, `glob`, `grep`, `json_query`, `fetch_url`, `alert`, `wait_and_verify`, `todo`, `record_plan`, `sciontool_status`. Unknown names cause a startup error. |
| `wait_and_verify` | object | `{}` | Bounds for the [`wait_and_verify`](/concepts/tools/#wait_and_verify-v29--bounded-poll-until-condition) poll loop. See below. |

Example — keep everything except shell access:

```json
{
  "tools": {
    "disable": ["bash"]
  }
}
```

The `--disable-tools=bash,write_file` CLI flag composes with this list by union — anything disabled in either path is off. To turn the entire suite off, use `--no-builtin-tools` (which makes `tools.disable` and `--disable-tools` moot).

A [task class](/concepts/context-management/#tools-and-plan-first-since-v29) can drop tools too — `--task=debug|research|review` removes `bash`. That is a *default*, so `--enable-tools=bash` puts it back. `--enable-tools` cancels the class's opinion only: it cannot re-enable something listed here or in `--disable-tools`, and passing both is a startup error rather than a silent win for either side. Naming a tool no class dropped is a no-op; naming a tool that doesn't exist fails at startup.

### `tools.wait_and_verify` (v2.9+)

| Field | Type | Default | Notes |
|---|---|---|---|
| `poll_allow` | string[] | `[]` | Tools that may be polled despite not being classified read-only by the runtime. Use the name the **model** sees, i.e. namespaced for MCP: `<server>_<tool>`, joined by a **single** underscore (`gke_get_pod`, not `gke__get_pod` or `get_pod`). A name that matches nothing is not an error — the poll is simply refused at call time. |
| `max_timeout_seconds` | int | `300` | Ceiling on the tool's `timeout_seconds` argument. A larger request is an error, not a silent clamp. |
| `max_attempts` | int | `60` | Ceiling on the tool's `max_attempts` argument. |

`wait_and_verify` refuses to poll anything the runtime can't classify as read-only, so it can never turn one approved call into sixty mutations. ADK's MCP adapter does not surface the server's `readOnlyHint` annotation, so **every MCP tool classifies as mutating** — `poll_allow` is the operator's explicit, per-tool assertion that a given MCP tool only observes state:

```json
{
  "tools": {
    "wait_and_verify": {
      "poll_allow": ["gke_get_pod", "gke_list_events"],
      "max_timeout_seconds": 300,
      "max_attempts": 60
    }
  }
}
```

Polling adds no authority: each attempt dispatches through the same permission gate, path scope, URL scope, plan-first gating and output caps a direct model call would hit.

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

### `otel.metrics`

Metrics run on a separate pipeline from traces (the daemon builds its own MeterProvider — ADK-go has none). Off by default.

| Field | Type | Default | Notes |
|---|---|---|---|
| `exporter` | string | `none` | One of `none`, `otlp`, `prometheus`, `both`. Env `OTEL_METRICS_EXPORTER` overrides. |
| `prometheus_addr` | string | `:9464` | Scrape endpoint bind address when `prometheus`/`both`. `--metrics-addr` overrides. |
| `session_labels` | bool | `true` | Stamp `session.id` / `app.name` / `user.id` on usage metrics. Set `false` to aggregate across sessions before export — for fleets where many short-lived sessions × models would blow up series cardinality. When `false`, `core_agent.session.duration` is not emitted (an aggregated wall-clock is meaningless) and the cost series' `priced` flag is the AND across sessions. |

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
| `allow_metadata_endpoints` | bool | `false` | Opts back into fetching link-local / cloud-metadata addresses (`169.254.0.0/16` incl. `169.254.169.254`, `fe80::/10`, AWS IMDSv6 `fd00:ec2::254`, IETF special-purpose `192.0.0.0/24` incl. `192.0.0.192`), which are otherwise hard-blocked in **every** permission mode regardless of the allowlist. Leave off unless you are deliberately building a metadata-service integration. |
| `proxy` | string | `""` | Outbound proxying for `fetch_url`. Empty (default) = **no proxy — the standard `HTTP_PROXY`/`HTTPS_PROXY` env vars are deliberately ignored**, because with a proxy in the path, hostname targets are resolved *at the proxy*, outside the SSRF guard's resolve-validate-pin dial. Set `"env"` to honor the env vars, or a fixed `http://`, `https://`, or `socks5://` URL to route every fetch through it. Either non-empty value is an explicit decision to delegate private/metadata SSRF policy for hostname targets to the proxy; literal-IP targets are still screened locally on the initial URL and every redirect hop. |

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

### SSRF defenses

The allowlist matches host *names*; a second layer vets the *addresses* those names resolve to, at dial time:

- **Link-local / cloud-metadata ranges are always blocked** — `169.254.0.0/16` (including the `169.254.169.254` metadata service), `fe80::/10`, the AWS IMDS IPv6 address `fd00:ec2::254`, and the IETF special-purpose block `192.0.0.0/24` (including the `192.0.0.192` metadata endpoint some clouds use) are refused in every permission mode (yolo included), no matter what the allowlist says. The only opt-out is `allow_metadata_endpoints: true`.
- **Loopback / private / special-purpose ranges need an exact-host opt-in** — `127.0.0.0/8`, `::1`, RFC1918 (`10/8`, `172.16/12`, `192.168/16`), CGNAT `100.64/10`, IPv6 ULA `fc00::/7`, `0.0.0.0/8` and the unspecified addresses `0.0.0.0`/`::`, NAT64 prefixes (`64:ff9b::/96`, `64:ff9b:1::/48`), benchmarking `198.18.0.0/15`, broadcast, and multicast are refused **unless** the request host appears in `allow` as an exact (non-wildcard) entry. Listing `internal-api.corp:8443` explicitly opts that host in; a broad `*` or `*.svc.cluster.local` wildcard does **not** unlock private ranges.
- **DNS rebinding is closed by IP pinning** — the host is resolved once, *every* returned IP is validated (one bad IP rejects the whole set), and the connection is dialed to one of those same vetted IPs. TLS SNI and the `Host` header still carry the original hostname; only the TCP dial target is pinned. Every redirect hop re-runs the full validation + pinning.
- **Proxies are explicit-only** — with `proxy` unset, ambient `HTTP_PROXY`/`HTTPS_PROXY` env vars are ignored so hostname resolution can't silently move to a proxy outside the guard. Setting `proxy: "env"` or a fixed proxy URL delegates the hostname-target SSRF policy above to that proxy — pick a proxy that enforces its own egress rules.

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

## `content_roots` (v2.9+)

A top-level array of **external directories to trust as additional instruction and skill scopes**. It lets a recipe run an *unmodified* external agent tree — a checkout of another repo's `agents/…` layout, for example — without vendoring a copy into `.agents/`. Nothing is added to the external tree; core-agent reads its `AGENTS.md` / `AGENTS.d/` and `skills/` in place.

```json
{
  "version": 1,
  "model": { "provider": "vertex", "name": "gemini-3.5-flash" },
  "content_roots": ["../kube-agents/agents/platform"]
}
```

- **Paths resolve relative to the agents dir** (the directory holding `config.json`), or the working directory when no `.agents/` was discovered. Absolute paths are used as-is.
- **Each root is its own trusted scope.** An `@include` inside a root resolves *within* that root; `@include`s or symlinks that escape the root are still rejected. The operator opt-in relaxes only the ban on reaching *across* trees — not confinement within one.
- **Ordering and precedence.** Instruction scopes **concatenate** in the order user → home-agents → **content_roots (listed order)** → project → per-caller, so external personas appear ahead of the project overlay. Skills follow **first-declarer-wins** at precedence **project > content_roots (listed order) > home-agents > user**, so a project skill shadows an external one of the same name.
- **A missing root is a loud error** (an operator typo shouldn't silently shrink the system prompt); a root without a `skills/` subdirectory simply contributes no skills.
- **MCP is not auto-loaded** from an external tree. Translate its servers once into the recipe's own `mcp.json`; the external tree stays untouched.

CLI convenience (no config edit needed):

- `--agents-content-dir <dir>` — repeatable; each value is an additional content root, merged after the config's `content_roots` and resolved the same way. Example: `core-agent -c .agents/config.json --agents-content-dir ../kube-agents/agents/platform`.

See [`external-content-root-design.md`](https://github.com/go-steer/core-agent/blob/main/docs/external-content-root-design.md) for the full decision record.

---

## `alerts`

Registers the webhook destinations the `alert` built-in is allowed to fire. Like `fetch_url`, the tool is **default-deny**: with no `targets` configured the `alert` tool is not registered at all, so the model can never notify an operator-unapproved endpoint. **SSRF is impossible by construction** — the tool has no arbitrary-URL argument. The model picks a target by *name* from this registry; an unknown name is rejected, not dialed.

The tool lets a headless daemon escalate — incident summaries, "I finished / I'm stuck" pings, decision notifications — without shelling out or standing up a separate MCP server.

| Field | Type | Default | Notes |
|---|---|---|---|
| `targets` | object[] | `[]` | The registered destinations. Empty → the `alert` tool is not registered. See the per-target fields below. |
| `rate_limit_per_target` | string | `""` | Optional cap applied independently to each target. Accepts `"30s"` (one every 30s), `"1/30s"`, `"5/min"`, `"100/hour"`. Empty → unlimited. In-memory only (per process; resets on restart). |

Each entry in `targets`:

| Field | Type | Default | Notes |
|---|---|---|---|
| `name` | string | — | **Required.** Unique per registry; `[A-Za-z0-9_-]`, 1–64 chars. This is the identifier the model fires and the string the [permissions gate](//#permissions) scopes on (`alert:<name>`). |
| `kind` | string | `"webhook"` | Reserved for future transports; only `""`/`"webhook"` are accepted today. |
| `url` | string | `""` | The destination URL (http/https). Mutually exclusive with `url_env` — set **exactly one**. |
| `url_env` | string | `""` | Name of the env var holding the destination URL. Prefer this for secret webhook URLs (e.g. a Slack Incoming Webhook) so the secret lives in your secret manager, not this file. Resolved at call time. |
| `template` | string | — | **Required.** Payload shape. Only `generic` ships today; `slack`, `discord`, and `pagerduty_events_v2` are declared but rejected at load with a "not yet implemented" error (Phase 2). |
| `auth` | object | `null` | Optional auth header, resolved from env at call time (rotates without a restart). One of: `bearer_env` (→ `Authorization: Bearer <env>`), or `basic_env_user` + `basic_env_pass` (→ HTTP Basic). The model never sets auth — the operator picks what ships. |
| `description` | string | `""` | Free text surfaced to the model in the tool description so it can match the right target to the situation. |

The `generic` template posts `application/json`:

```json
{ "level": "critical", "summary": "…", "details": { "…": "…" } }
```

`details` is omitted when empty. No timestamp is included by design — the eventlog's `tool/alert` record is the authoritative time source.

Worked example:

```json
{
  "alerts": {
    "rate_limit_per_target": "1/30s",
    "targets": [
      {
        "name": "slack-oncall",
        "url_env": "SLACK_ONCALL_WEBHOOK",
        "template": "generic",
        "description": "on-call channel; use for anything needing a human now"
      },
      {
        "name": "audit-sink",
        "url": "https://audit.internal.example.com/hook",
        "template": "generic",
        "auth": { "bearer_env": "AUDIT_SINK_TOKEN" },
        "description": "append-only audit log; fire on every significant decision"
      }
    ]
  }
}
```

Per-target gating composes with the [permissions gate](//#permissions): write `permissions.allow: ["alert:slack-oncall"]` to let the agent fire only that target, or `["alert:*"]` for all. Auth material never appears in the tool arguments or the audited call — only the target name, level, summary, and details do, and the result carries just the status code and duration (never the response body).

CLI convenience:

- `--disable-tools=alert` — turns the tool off even when targets are configured.

---

## `attach`

Default values for the attach-mode listener and the peer-registration client. Every field below is also exposed as a `--attach-*` CLI flag: names follow the `--attach-<kebab-case-field>` convention (`unix_socket` → `--attach-unix-socket`, `peer_hub` → `--attach-peer-hub`, `register_to` → `--attach-register-to`, and so on). The flag wins when explicitly set, otherwise the config value applies, otherwise the zero value. This section exists for K8s-style deployments where the same settings would otherwise be repeated on every invocation.

String fields are passed through `os.ExpandEnv` so per-pod values like `"https://${POD_IP}:7777"` can live in a shared ConfigMap and resolve to the right address at startup.

| Field | Type | Default | Notes |
|---|---|---|---|
| `listen` | string | `""` | Address the attach HTTP server binds to (e.g. `"127.0.0.1:7777"`). Empty → server off. Mutually exclusive with `unix_socket`. Requires `--session-db` at runtime (the broadcaster pumps from the event log). **Non-loopback addresses** (`":7777"`, `"0.0.0.0:7777"`, ...) refuse to start without authentication — set `token_env` (or mTLS via `client_ca`, or enforced multi-session auth). Tokenless loopback starts but logs a loud warning. |
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
  "model": { "provider": "vertex", "name": "gemini-3.6-flash",
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

### `attach.shutdown_timeout`

Caps how long the attach listener's graceful HTTP shutdown waits for in-flight requests after SSE streams are hung up. Duration string, must be greater than zero; omit the field to keep the default `"5s"`. This counts toward the daemon's total teardown budget — keep it comfortably under the supervisor's kill timeout (K8s `terminationGracePeriodSeconds`, default 30s).

```json
{ "attach": { "shutdown_timeout": "10s" } }
```

### `attach.cost_rate_limit`

Tunes the per-caller token bucket that bounds the **cost-bearing** attach endpoints — the five slash ops (`compact`, `done`, `btw`, `subagent`, `replan`), `POST /sessions`, and `pricing/refresh`. On by default; reads, `/events` streams, `/inject`, and `/wake` are never limited. Callers are the server-verified identities (bearer table, validated proxy assertion, or the single anonymous bucket in single-user mode). Over-limit requests get `429` with a `Retry-After` header.

| Field | Type | Default | Notes |
|---|---|---|---|
| `per_minute` | int | `10` | Sustained per-caller rate. `0` keeps the default; negative fails validation. |
| `burst` | int | `5` | Bucket size — how many back-to-back calls before the sustained rate applies. |
| `disabled` | bool | `false` | Turns enforcement off entirely. Prefer raising the limits over disabling on multi-session daemons. |

```json
{ "attach": { "cost_rate_limit": { "per_minute": 60, "burst": 15 } } }
```

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
