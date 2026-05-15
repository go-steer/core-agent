# Subagent tool — M3 plan

## Status (2026-05-15): superseded

This plan documented an `agenttool`-wrapped design. The shipped Phase 4
implementation in [`docs/eventlog-plan.md`](./eventlog-plan.md#phase-4--subagent-integration-via-custom-runner-replaces-existing-subagents-plan)
replaces it with a custom runner that participates in the parent's
durable event log via `session.Event.Branch` — needed once durable
sessions + audit logs landed in Phases 1-3 of the eventlog plan.

**See [`docs/eventlog-decisions.md`](./eventlog-decisions.md) Phase 4
section for the shipped design**, including the discovery-during-impl
pivot from "shared parent session" to "derived session row" because
ADK's database session service has optimistic-concurrency checking
that rejects two concurrent runners writing to the same session.

The rest of this doc is preserved as historical context — the design
constraints (depth cap, gate inheritance, parallel-call safety, tool
naming) all carried into the shipped version.

---

## Recommendation summary (historical)

**Wrap, don't build.** ADK Go ships `google.golang.org/adk/tool/agenttool` (`/home/user/go/pkg/mod/google.golang.org/adk@v1.2.0/tool/agenttool/agent_tool.go`), which already implements agent-as-tool with: fresh per-call `session.InMemoryService()`, parent-state pass-through (filtering `_adk*` keys), JSON output extraction, and parallel-call safety. We add a thin `tools.NewSubagentTool(...)` constructor in `core-agent/tools/subagent.go` that builds an inner `agent.New(...)` (reusing all our existing options) and returns `agenttool.New(adk-inner, &agenttool.Config{SkipSummarization: false})` wrapped in our usual gate + truncation + recursion-depth checks. Library-only, opt-in (NOT in `tools.Default()`); CLI gets a single `--enable-subagent` flag for the smoke path. Same model and same gate as the parent by default; tools default to "research-safe" subset (no `bash`, no `write_file`, no `edit_file`) with a `WithTools([]tool.Tool)` override; depth cap of 2 enforced via a context value.

## Context

Today the primary `*agent.Agent` is the only thing that runs LLM turns. For two recurring shapes we want to be able to spawn a *subagent* as a tool call:

1. **Parallelizable independent subtasks** — the model can emit N parallel tool calls in one turn (ADK already dispatches them concurrently via goroutines: `internal/llminternal/base_flow.go:600` spawns `go func` per `FunctionCall`). Subagents-as-tools become an N-way fanout knob.
2. **Context isolation** — subagent runs against a *fresh session*, so a 200KB grep result never enters the parent's context window. Only the subagent's distilled answer comes back as the tool result string.

The marker for this work is at `agent/agent.go:74-77` (the `TODO(subagents)` block in `options`) and `docs/DESIGN.md:322` (`Subagent tool — deferred to M3`) and `docs/DESIGN.md:506` ("subagent-as-tool (Anthropic-style) or ADK's native sub-agent transfer. Pick the shape based on the first concrete consumer that needs it.").

The concrete consumer use cases (parallelize, protect context) both point to **subagent-as-tool**, not ADK's native transfer (`SubAgents` field on `llmagent.Config`, peer/parent transfer flow in `internal/llminternal/agent_transfer.go`). Transfer is a hand-off: the conversation continues in the sub-agent, and only one agent is "live" at a time. That doesn't parallelize and doesn't protect parent context (the user's message goes to the sub-agent verbatim).

ADK already ships `tool/agenttool` (`/home/user/go/pkg/mod/google.golang.org/adk@v1.2.0/tool/agenttool/agent_tool.go`) which is exactly the agent-as-tool primitive. It:

- Builds a function declaration from the agent's input schema (or a default `{request: string}` when none).
- On call: creates a `session.InMemoryService()`, copies non-`_adk*` state from parent, runs the wrapped agent through ADK's `runner.Runner` with `StreamingModeSSE`, joins all final-content text parts, and returns `map[string]any{"result": outputText}`.
- Names the tool after the agent (so we control the function name via `agent.WithName`).

**We wrap this.** Re-implementing fresh-session orchestration would duplicate ~50 lines of fiddly ADK plumbing for no gain.

## Design decisions

| Decision | Choice | Why |
|---|---|---|
| Build vs. wrap ADK's `agenttool` | **Wrap** `agenttool.New` from `google.golang.org/adk/tool/agenttool` | The ADK package already does fresh-session orchestration, parent-state propagation, and final-text joining (`agent_tool.go:121-251`). Reimplementing would duplicate ~50 lines of ADK plumbing for nothing. The same pattern is used in ADK's own `examples/tools/multipletools/main.go:103`. |
| Subagent-as-tool vs. ADK transfer | **Agent-as-tool** | The user's two motivations (parallelize, protect context) both require the parent to remain live and to receive only a distilled string back. Transfer hands the conversation off to a peer/sub-agent; the parent's turn ends. Wrong shape for the use case. Transfer remains available via raw `llmagent.Config.SubAgents` for consumers who want it — we don't expose it from `agent.New`. |
| Session isolation | **Fresh `session.InMemoryService()` per subagent call** (already the default in `agenttool`) | This is the whole point — a 200KB grep stays inside the subagent's session and never touches the parent's history. Inheriting parent history would defeat the context-protection use case. |
| State propagation | **Inherit parent state, filtered to non-`_adk*` keys** (already the default in `agenttool`) | Lets parent and subagent share user-defined state (e.g. workspace root) but keeps ADK-internal scratch out of the child. |
| Tool inheritance | **Curated subset by default; full override via `tools.SubagentOptions.Tools`** | Default subset is "research-safe": `read_file`, `list_dir`, `grep`-equivalent (when we have one), `todo`. No `bash`, no `write_file`, no `edit_file`, no recursive `subagent`. This matches Claude Code's default Agent tool surface and prevents a research subagent from accidentally rewriting the workspace. Library callers can pass any `[]tool.Tool` or `[]tool.Toolset` for richer surfaces. |
| Model choice | **Same model as parent by default; per-call override via `WithModel(adkmodel.LLM)`** | The cheapest, simplest default. Cost-conscious consumers can wire a `gemini-3.1-flash` subagent under a `claude-opus-4-7` parent; same `models.Provider` abstraction returns either. |
| Output capping | **Reuse `tools.Truncate` with `cfg.ToolOutput.PerTool["subagent"]` defaults** | Subagent output goes through the same path as `bash`/`read_file` results. Default cap mirrors `read_file` (256KB / 5000 lines) — subagents return summarized text, not raw output, so the cap is a safety net not a primary control. |
| Permissions | **Same gate as parent by default; subagent's tools go through the *same* `permissions.Gate`** | The bash denylist + path scope + session approvals carry over, so a subagent can't escape policy by being one. The subagent tool *itself* registers under a `subagent` policy namespace so consumers can write `permissions.allow: ["subagent:research", "subagent:plan"]` (matching by canonical agent name). |
| Concurrency | **Inherit ADK's behavior — concurrent across calls in one model turn, serial within a single call** | ADK's `internal/llminternal/base_flow.go:600` already dispatches each `FunctionCall` in a goroutine. Multiple `subagent(...)` calls in one parent turn run in parallel for free. No special wiring needed; document it. |
| Library API | **`tools.NewSubagentTool(opts SubagentOptions) (tool.Tool, error)`** + a convenience `agent.WithSubagents([]*agent.Agent) Option` that registers each as a tool | `NewSubagentTool` is the substantive entry point. `agent.WithSubagents` is sugar that calls `NewSubagentTool` for each `*agent.Agent` and forwards to `WithTools`. Two paths so the user can pick: explicit per-tool options vs. "I have agents, make them tools." |
| Default-on or opt-in | **Opt-in.** Not in `tools.Default()`. CLI exposes `--enable-subagent` (default false) | Subagent has a real cost surface (extra LLM calls per subagent invocation) and a real concurrency surface. Defaulting it on would surprise consumers' bills. Mirrors the rationale for `CodeExecution` defaulting off (`docs/DESIGN.md:260-267`). |
| Recursion limit | **Depth cap of 2 by default, configurable via `SubagentOptions.MaxDepth`. Enforced via `context.Context` value (`subagentDepthKey`)** | Subagents calling subagents could loop indefinitely, especially if the parent registers itself in the subagent's tool set by accident. A small integer in context is the cheapest enforcement; depth is read at the start of every subagent tool call and incremented for the child run. |
| Telemetry | **Same `usage.Tracker` (passed in via `SubagentOptions.Tracker`); ADK OTEL spans naturally nest** | The subagent's `runner.Runner` calls go through the same ADK telemetry path (`google.golang.org/adk/internal/telemetry`), and span parenting comes from the same `context.Context`. We surface a `tracker` knob so subagent token + cost rollups land in the parent CLI's `WriteSummary` line. |
| Tool name | **Tool function name = subagent's `agent.WithName(...)` value (e.g., `research`, `plan`)** | Lets the model distinguish between several registered subagents (`research`, `plan`, `summarize`) by name. Falls back to `subagent` when only one is registered. The name doubles as the policy bucket key (`subagent:research`). |
| Agent inner accessor | **Add `Agent.Inner() adkagent.Agent` to `agent/agent.go`** | `agenttool.New` requires an `agent.Agent` (ADK), but `core-agent/agent.Agent.inner` is private (`agent.go:53`). Adding a small accessor is the cleanest path; no other code needs to change. Alternatively `tools.NewSubagentTool` constructs the inner llmagent itself, but that duplicates the wiring in `agent.New`. |

## Files

### New

- `tools/subagent.go` — `SubagentOptions`, `NewSubagentTool`, the depth-tracking context key, default-research-tool set, and the wrapping logic that adds gate consultation, truncation, and depth enforcement around `agenttool.New`.
- `tools/subagent_test.go` — uses `models/mock`'s scripted/echo providers to drive end-to-end flows without credentials. Covers: tool declaration shape, fresh-session isolation, recursion cap fires, gate denial propagates, output truncation, and parallel-call safety.
- `examples/with-subagent/main.go` — credential-free demo using `--provider=echo` (the `echoLLM` returns the user's prompt verbatim, which is enough to demonstrate the `subagent(request="...")` round-trip end-to-end).

### Modified

- `agent/agent.go` — (a) replace the `TODO(subagents)` comment block at lines 74-77 with the real wiring; (b) add `func (a *Agent) Inner() adkagent.Agent { return a.inner }`; (c) add `WithSubagents([]*Agent) Option` that builds tool wrappers via `tools.NewSubagentTool` and accumulates into `o.tools`. Note: this introduces an import edge `agent → tools`, which is OK because `tools` does not import `agent` today (verified — `tools/builtins.go` imports only `config`, `permissions`, ADK). Alternative: keep `agent` decoupled from `tools` by making `WithSubagents` accept `[]tool.Tool` directly, leaving the tool construction in user code. Recommend the convenience version; cost is one new edge in the dep graph.
- `tools/builtins.go` — extend the `BuiltinTools` struct with a `Subagent bool` field and the corresponding `Disable`/`builtinToolNames` entry. **Default off** (so `tools.Default()` does NOT enable it). Build path constructs via `NewSubagentTool` with an options block populated from `cfg.Subagent` (see config below) when a subagent factory is wired by the host.
- `cmd/core-agent/main.go` — add `--enable-subagent` flag (default false). When set: build a default subagent (same model, same gate, default research-safe tools) and append its `tool.Tool` to the `builtinTools` slice before passing to `agent.WithTools`.
- `extras/scion-agent/main.go` — same flag wiring as core-agent (mirror it for parity, per existing pattern in `cmd/core-agent/main.go:58-60` and `extras/scion-agent/main.go`).
- `config/config.go` — add `SubagentConfig` struct and `Subagent SubagentConfig `json:"subagent,omitempty"`` top-level field. Fields: `MaxDepth int`, `Model string` (override; empty = inherit parent), `Tools []string` (canonical names from the parent's enabled tool set; empty = default research-safe subset). Extend `Validate()` to check that `MaxDepth >= 0` (0 means "use default of 2") and that named tools exist.
- `config/discovery_test.go` — round-trip test for the new `subagent` block.
- `docs/DESIGN.md` — replace `Subagent tool — deferred to M3` line at 322 with a brief reference to the new section, and add a new `## Subagent tool` section after `## Built-in tools`. Cover: agent-as-tool vs. transfer rationale, the wrapping-of-ADK choice, fresh-session decision, default-off CLI rationale, depth cap design.
- `README.md` — (a) add subagent to the built-in tools bullet around line 30 (mention default-off); (b) move "Subagents" from the Roadmap section (lines 221, 268) into the M3 milestone section (after the existing M2 entry). Update the marker in the agent.go reference (line 221).
- `docs/site/content/docs/library-api.md` — new `## Subagent tool` section after `## Built-in tools` (around line 161). Show `tools.NewSubagentTool` with options struct, plus the `agent.WithSubagents([]*agent.Agent)` shorthand. Mention recursion cap, default research-safe tool subset, parallel-call safety.
- `docs/site/content/docs/configuration.md` — new `## subagent` section after `## tools` (around line 187). Schema table for `max_depth`, `model`, `tools`.
- `docs/site/content/docs/permissions.md` — note that subagent calls flow through the gate under the `subagent` namespace (allowlist patterns: `subagent:<name>`, e.g., `subagent:research`).

## Implementation

### 1. `agent/agent.go` — accessor and option

Replace lines 74-77:

```go
// SubAgents are agents wrapped as tools the model can call. Built via
// tools.NewSubagentTool; see WithSubagents for the consumer-facing API.
subagents []*Agent
```

Add the accessor right after `Run` (so the public surface is contiguous):

```go
// Inner returns the underlying ADK agent. Exposed so callers can wrap
// it as a sub-agent tool via tool/agenttool.New (see tools.NewSubagentTool).
// Most consumers don't need this; prefer tools.NewSubagentTool or the
// WithSubagents option.
func (a *Agent) Inner() adkagent.Agent { return a.inner }
```

Add the option (do this in a new file `agent/subagents.go` to keep `agent.go` from growing past ~250 lines and to avoid the new import edge dirtying the `agent.go` history):

```go
// agent/subagents.go
package agent

import "github.com/go-steer/core-agent/tools"

// WithSubagents registers each agent as a callable tool. The model
// invokes the subagent by name (the wrapped agent's Name()) with a
// `request` string argument; the subagent runs in a fresh session
// (so its work doesn't pollute the parent's context window) and its
// final text reply is returned as the tool result.
//
// Each subagent tool is constructed with default options — same gate
// as the parent (when set via tools.Build), depth cap of 2, default
// research-safe tool set. For per-subagent overrides, use
// tools.NewSubagentTool directly and register via WithTools.
func WithSubagents(agents []*Agent) Option {
    return func(o *options) {
        for _, a := range agents {
            t, err := tools.NewSubagentTool(tools.SubagentOptions{Inner: a})
            if err != nil {
                // Construction errors here are programmer errors (nil
                // agent, etc.); panic so the consumer sees them at
                // wiring time, not at first model turn.
                panic("agent.WithSubagents: " + err.Error())
            }
            o.tools = append(o.tools, t)
        }
    }
}
```

The `panic` is consistent with the established convention — `examples/with-tools/main.go:62` and `tools/builtins.go:194` both panic on construction-time mistakes.

### 2. `tools/subagent.go` — the meat

```go
package tools

import (
    "context"
    "fmt"

    adkagent "google.golang.org/adk/agent"
    adkmodel "google.golang.org/adk/model"
    adktool "google.golang.org/adk/tool"
    "google.golang.org/adk/tool/agenttool"
    "google.golang.org/genai"

    "github.com/go-steer/core-agent/agent"
    "github.com/go-steer/core-agent/config"
    "github.com/go-steer/core-agent/permissions"
    "github.com/go-steer/core-agent/usage"
)

// SubagentOptions configures a subagent tool. Inner is required; everything
// else has sensible defaults.
type SubagentOptions struct {
    // Inner is the *agent.Agent to expose as a tool. The tool's function
    // name comes from the wrapped agent's Name() (set via agent.WithName).
    Inner *agent.Agent

    // Gate, when non-nil, gates each subagent invocation under the
    // "subagent" policy namespace. The wrapped agent's own tools should
    // already share this gate; setting it here adds an outer check on
    // the subagent dispatch itself (so policies like
    // `subagent:research` work).
    Gate *permissions.Gate

    // Cfg supplies output truncation caps. Looks up
    // cfg.ToolOutput.PerTool["subagent"] then falls back to defaults
    // (256KB / 5000 lines, mirroring read_file).
    Cfg *config.Config

    // MaxDepth is the maximum subagent call depth. Defaults to 2.
    // Subagents at MaxDepth that try to invoke another subagent will
    // get an error result.
    MaxDepth int

    // Tracker (optional) accumulates token + cost stats from the
    // subagent's runs into the parent's totals.
    Tracker *usage.Tracker

    // SkipSummarization passes through to agenttool.Config. When true,
    // the parent agent will use the subagent's response verbatim
    // without re-summarizing. Defaults to false (summarize).
    SkipSummarization bool
}

const (
    defaultMaxSubagentDepth = 2
    subagentToolNamespace   = "subagent"
)

type depthKey struct{}

// CurrentDepth reads the current subagent depth from ctx. Zero when
// not inside a subagent invocation.
func CurrentDepth(ctx context.Context) int {
    v, _ := ctx.Value(depthKey{}).(int)
    return v
}

// NewSubagentTool wraps an *agent.Agent as a tool the parent model can
// call. Returns the ADK Tool ready to pass to agent.WithTools.
func NewSubagentTool(opts SubagentOptions) (adktool.Tool, error) {
    if opts.Inner == nil {
        return nil, fmt.Errorf("tools: subagent: Inner is required")
    }
    maxDepth := opts.MaxDepth
    if maxDepth <= 0 {
        maxDepth = defaultMaxSubagentDepth
    }

    // The ADK tool that does the heavy lifting (fresh session, run,
    // join text). We then wrap it for gate + depth + truncate.
    inner := agenttool.New(opts.Inner.Inner(), &agenttool.Config{
        SkipSummarization: opts.SkipSummarization,
    })

    return &subagentTool{
        inner:    inner,
        gate:     opts.Gate,
        cfg:      opts.Cfg,
        maxDepth: maxDepth,
        tracker:  opts.Tracker,
        name:     opts.Inner.Inner().Name(),
    }, nil
}

type subagentTool struct {
    inner    adktool.Tool
    gate     *permissions.Gate
    cfg      *config.Config
    maxDepth int
    tracker  *usage.Tracker
    name     string
}

func (t *subagentTool) Name() string        { return t.name }
func (t *subagentTool) Description() string { return t.inner.Description() }
func (t *subagentTool) IsLongRunning() bool { return false }

func (t *subagentTool) Declaration() *genai.FunctionDeclaration {
    if rn, ok := t.inner.(interface{ Declaration() *genai.FunctionDeclaration }); ok {
        return rn.Declaration()
    }
    return nil
}

// Run is the gate + depth + truncate wrapper around the ADK agenttool.
func (t *subagentTool) Run(ctx adktool.Context, args any) (map[string]any, error) {
    // Depth check (against parent context, before increment).
    depth := CurrentDepth(ctx)
    if depth >= t.maxDepth {
        return map[string]any{
            "error": fmt.Sprintf("subagent: depth limit reached (%d); refusing to recurse", t.maxDepth),
        }, nil
    }

    // Gate. summarizeRequest already exists in tools/gate.go.
    if t.gate != nil {
        if err := t.gate.CheckGeneric(context.Background(), subagentToolNamespace, summarizeRequest(t.name, args)); err != nil {
            return nil, err
        }
    }

    // Push a deeper depth into the context. ADK's tool.Context wraps
    // a context.Context that the inner agenttool reads when constructing
    // its sub-runner; we use WithContext to thread the new value through.
    childCtx := context.WithValue(ctx, depthKey{}, depth+1)
    childToolCtx := ctx.WithContext(childCtx)

    out, err := t.inner.(interface {
        Run(adktool.Context, any) (map[string]any, error)
    }).Run(childToolCtx, args)
    if err != nil {
        return nil, err
    }

    // Truncate the "result" string field to keep large subagent
    // outputs from overrunning the parent's context window.
    if t.cfg != nil {
        caps := capsFor(t.cfg, "subagent", 256*1024, 5000)
        if s, ok := out["result"].(string); ok {
            out["result"] = Truncate(s, caps.bytes, caps.lines)
        }
    }

    // Tracker propagation: ADK's agenttool runs use their own runner
    // and the usage tally lands on the inner LLM. There's no clean hook
    // to read it back from the agenttool's iter.Seq2 (it's already
    // drained by the time Run returns). Defer cross-process tracking to
    // a follow-up; document the gap.
    _ = t.tracker

    return out, nil
}

// ProcessRequest delegates to the inner agenttool so the subagent's
// function declaration gets packed into the LLM request the same way.
func (t *subagentTool) ProcessRequest(ctx adktool.Context, req *adkmodel.LLMRequest) error {
    if pr, ok := t.inner.(interface {
        ProcessRequest(adktool.Context, *adkmodel.LLMRequest) error
    }); ok {
        return pr.ProcessRequest(ctx, req)
    }
    return nil
}
```

Two things to verify at impl time:

- **`tool.Context.WithContext(ctx)`** exists on ADK's tool.Context (it's used in `agenttool.go:201` already — `r.Run(toolCtx, ...)` reads the embedded context). Check the ADK API; if it's not `WithContext` named exactly, the closest equivalent. If no public method exists, fall back to wrapping in a custom toolCtx that returns the new context.
- **`agenttool.New` returns `tool.Tool`**, but the Run/Declaration methods are on the unexported `agentTool` struct. Use type-assertion as shown rather than direct method calls.

### 3. `tools/builtins.go` — register the subagent slot

Append to `BuiltinTools` (between `Todo` and the closing brace):

```go
Subagent bool // Wrap an agent as a tool the model can call (off by default)
```

Append to `builtinToolNames`: `"subagent"`. Add the case to `Disable`. Update `Default()` to leave `Subagent: false`.

In `Build`, append after the `Todo` spec:

```go
{b.Subagent, "subagent", "Run a focused subagent and return its result.", func() (tool.Tool, error) {
    if cfg.Subagent.Inner == nil {
        return nil, fmt.Errorf("tools: subagent enabled but no inner agent supplied (use tools.NewSubagentTool directly)")
    }
    return NewSubagentTool(SubagentOptions{
        Inner:    cfg.Subagent.Inner,
        Gate:     gate,
        Cfg:      cfg,
        MaxDepth: cfg.Subagent.MaxDepth,
    })
}},
```

But this requires `cfg.Subagent.Inner *agent.Agent` to exist on the config struct — which would create a `config → agent` cycle. **Resolution:** don't expose subagent through `tools.Build`. Instead, the CLI assembles its subagent tool itself (via `tools.NewSubagentTool`) and passes it as a separate slice to `agent.WithTools`. The `BuiltinTools.Subagent` field is then a no-op placeholder; better to omit it entirely from `BuiltinTools` and document the CLI path. **Recommend dropping the `BuiltinTools.Subagent` change**; the CLI wires the subagent tool directly. Update `cmd/core-agent/main.go` accordingly.

### 4. `cmd/core-agent/main.go` — `--enable-subagent` flag

After `disableTools` flag (around line 57):

```go
enableSubagent := flag.Bool("enable-subagent", false, "register a research-safe subagent as a tool the model can call (default: off; opt-in due to compute cost)")
```

After `builtinTools` is assembled (around line 184), before `tracker := usage.NewTracker()`:

```go
if *enableSubagent {
    // Build the subagent's own tool registry — research-safe subset.
    subBT := tools.BuiltinTools{ReadFile: true, ListDir: true, Todo: true}
    subReg, err := tools.Build(cfg, gate, subBT)
    if err != nil {
        fmt.Fprintf(os.Stderr, "core-agent: subagent tools: %v\n", err)
        return runner.ExitConfigError
    }
    subAgent, err := agent.New(m,
        agent.WithName("research"),
        agent.WithDescription("A focused research subagent. Reads files and lists directories. Returns a concise text answer."),
        agent.WithInstruction("You are a focused research subagent. The user has asked a single question. Answer concisely. You may read files and list directories."),
        agent.WithTools(subReg.Tools),
    )
    if err != nil {
        fmt.Fprintf(os.Stderr, "core-agent: subagent: %v\n", err)
        return runner.ExitConfigError
    }
    subTool, err := tools.NewSubagentTool(tools.SubagentOptions{
        Inner: subAgent, Gate: gate, Cfg: cfg, Tracker: tracker,
    })
    if err != nil {
        fmt.Fprintf(os.Stderr, "core-agent: subagent: %v\n", err)
        return runner.ExitConfigError
    }
    builtinTools = append(builtinTools, subTool)
}
```

### 5. `examples/with-subagent/main.go`

A tiny program that wires:

- `--provider=echo` (no creds; `models/mock`).
- A "research" subagent with `read_file` + `list_dir`.
- A parent agent that registers it via `agent.WithSubagents([]*agent.Agent{research})`.
- A single `Run("look at README.md and tell me the project's tagline")` to demonstrate the flow.

The echo provider just echoes the user's last message, so the demo is structural — it shows the wiring compiles, the tool is declared, and the registry resolves — not an actual research result. For a substantive demo, document switching to `--provider=anthropic` or `--provider=gemini`.

### 6. `config/config.go` additions

```go
type SubagentConfig struct {
    MaxDepth int      `json:"max_depth,omitempty"` // 0 → default of 2
    Model    string   `json:"model,omitempty"`     // empty → inherit parent's model
    Tools    []string `json:"tools,omitempty"`     // empty → research-safe defaults
}

// On Config struct, after Tools:
Subagent SubagentConfig `json:"subagent,omitempty"`
```

In `Validate()`, after the tools.disable validation:

```go
if c.Subagent.MaxDepth < 0 {
    return fmt.Errorf("config: subagent.max_depth must be >= 0 (got %d)", c.Subagent.MaxDepth)
}
for _, n := range c.Subagent.Tools {
    found := false
    for _, valid := range []string{"bash", "read_file", "write_file", "edit_file", "list_dir", "todo"} {
        if n == valid { found = true; break }
    }
    if !found {
        return fmt.Errorf("config: subagent.tools: unknown tool %q", n)
    }
}
```

The CLI then reads `cfg.Subagent` to populate the subagent's tool set / model when `--enable-subagent` is on, replacing the hardcoded defaults from step 4.

## Tests

`tools/subagent_test.go`:

- **`TestSubagentTool_DeclarationUsesAgentName`** — build a subagent named `research`, assert `tool.Declaration().Name == "research"` and that the `request` parameter is present.
- **`TestSubagentTool_RunReturnsResult`** — wire a parent that runs `echoLLM` (`models/mock`) as the subagent's model. The echo provider returns "ping" for input "ping"; assert the tool result is `{"result": "ping"}`.
- **`TestSubagentTool_FreshSession`** — register a subagent, drive two sequential subagent calls; assert the second call's `LLMRequest.Contents` length matches a fresh session (only the new user message), not parent + prior subagent history. Verifies isolation.
- **`TestSubagentTool_DepthCapFires`** — wire a subagent that itself has the same subagent registered. Drive a call from depth 0; assert the depth-2 call returns `{"error": "subagent: depth limit reached (2); refusing to recurse"}` rather than infinite recursion. Use a scripted LLM that always returns a `subagent` tool call to force the recursion attempt.
- **`TestSubagentTool_GateDeniesByPolicy`** — set `permissions.Mode: ModeAllow` with no allowlist entry for `subagent:research`. Assert `Run` returns the gate's error and the inner `agenttool` is never invoked.
- **`TestSubagentTool_OutputTruncated`** — wrap a subagent whose final text is 1MB. Set `cfg.ToolOutput.PerTool["subagent"] = {MaxBytes: 1024}`. Assert the `result` field length is ~1024 + the truncation marker.
- **`TestSubagentTool_ParallelCallsSafe`** — run two `Run` calls concurrently from goroutines; assert no data races (rely on `go test -race` in CI). Sanity check that the underlying `agenttool` makes a fresh session per call (it does, but pin the contract).

`agent/subagents_test.go`:

- **`TestWithSubagents_RegistersByName`** — call `agent.New(m, agent.WithSubagents([]*agent.Agent{a, b}))` where `a.Name() == "research"` and `b.Name() == "plan"`; assert both tools surface in the agent's tool list.
- **`TestWithSubagents_NilSliceNoop`** — passing nil slice doesn't panic and registers no tools.

`config/discovery_test.go` extension:

- **`TestLoad_SubagentBlockRoundtrips`** — parse `{"subagent":{"max_depth":3,"model":"gemini-3.1-flash","tools":["read_file","list_dir"]}}`, assert round-trip.
- **`TestValidate_SubagentMaxDepthNegative`** — `MaxDepth: -1` rejects with clear error.
- **`TestValidate_SubagentUnknownTool`** — `Tools: ["does_not_exist"]` rejects with clear error.

## Documentation

- **`docs/DESIGN.md`** — replace line 322 (`Subagent tool — deferred to M3`) with: `Subagent tool — see "Subagent tool" section below.` Add a new `## Subagent tool` section after `## Built-in tools`. Cover:
  - Why agent-as-tool, not transfer (parent must remain live; only distilled output flows back).
  - Why we wrap ADK's `agenttool` rather than reimplement (cite the `tool/agenttool/agent_tool.go` path).
  - Why fresh session (the whole point — context isolation).
  - Why default-off in `tools.Default()` and CLI (cost surface; mirrors CodeExecution rationale).
  - Recursion-cap design choice (context value vs. counter on the tool struct — context value because each tool struct is created once but invoked many times concurrently).
  - "Same gate by default" — subagent tools inherit the parent's gate so policies don't fragment.
- **`README.md`** — (a) line 30, after the `bash, todo` enumeration, add: `Optionally a research-safe **subagent** that the model can call to delegate focused tasks (read-only by default; off by default — enable with --enable-subagent).` (b) Move the subagents bullet from Roadmap (lines 221, 268) into the M3 milestone entry once that ships; for the plan, leave both pointers updated to "see M3."
- **`docs/site/content/docs/library-api.md`** — new `## Subagent tool` section after `## Built-in tools`. Show:
  ```go
  research, _ := agent.New(m,
      agent.WithName("research"),
      agent.WithInstruction("Research subagent. Be concise."),
      agent.WithTools(researchTools),
  )
  parent, _ := agent.New(m,
      agent.WithTools(myTools),
      agent.WithSubagents([]*agent.Agent{research}),
  )
  ```
  Plus the explicit `tools.NewSubagentTool(...)` form for per-subagent options.
- **`docs/site/content/docs/configuration.md`** — new `## subagent` section after `## tools`. Schema table:
  | Field | Type | Default | Notes |
  |---|---|---|---|
  | `max_depth` | int | 2 | Max nested subagent calls. 0 = default. |
  | `model` | string | "" (inherit) | Override model for the subagent (e.g., a cheaper one). |
  | `tools` | string[] | (research-safe subset) | Which built-in tools the subagent gets. Empty = `read_file`, `list_dir`, `todo`. |
- **`docs/site/content/docs/permissions.md`** — under the existing pattern grammar, add: `subagent:<name>` matches a subagent invocation by the wrapped agent's name.

## Verification

```bash
cd /home/user/projects/core-agent
go test ./tools/... ./agent/... ./config/...
go test -race ./tools/...
go vet ./...
go build ./...

# End-to-end smoke (no creds — uses echoLLM).
./core-agent --provider=echo --enable-subagent -p "ask the subagent: hello"
# Expected:
#   stderr line: → research          (the model "calls" the subagent)
#   stderr line: ← research          (the response comes back)
#   stdout: hello                    (echo returns the user's last message)
```

A second smoke uses scripted replay to pin behavior:

```bash
# Record a real subagent session against, say, Gemini.
GEMINI_API_KEY=... ./core-agent --enable-subagent --record-to=/tmp/sub.jsonl -p "what's in README.md?"

# Replay it with no creds.
./core-agent --provider=scripted --script=/tmp/sub.jsonl --enable-subagent -p "anything"
```

For library users: run `examples/with-subagent` end-to-end with `--provider=echo`, confirm wiring compiles + the `subagent` tool appears in the registry list.

## Out of scope (defer to a follow-up)

- **Subagent token / cost accounting roll-up** — `agenttool.New` runs an internal runner whose token totals don't surface back to the parent's `usage.Tracker`. Threading this requires a hook in ADK's `agenttool` (which would need an upstream change) or our own re-implementation of the agenttool path. Document the gap; revisit when a consumer asks.
- **Per-subagent model selection wired through `cfg.Subagent.Model`** — schema is in place but the CLI honoring it requires a second `models.Resolve` call with an overridden config. Implement in a follow-up if a consumer needs cheaper subagent inference.
- **Streaming subagent output** — today the subagent's text comes back as a single string after the subrun completes (`agenttool.go:222-229`). Streaming partial output through the parent's tool result is not part of ADK's model and would require non-trivial changes. Not needed for the use cases ("research/code-search returns one text answer").
- **Subagent calling MCP / skills tools** — the wrapped agent's tools come from the consumer's `agent.WithTools` / `WithToolsets` calls; nothing prevents passing MCP toolsets to the subagent. We don't auto-include the parent's MCP/skills surface to keep the subagent's surface predictable; consumers who want the inheritance can pass the same `[]tool.Toolset` to both.
- **Saving subagent transcripts** — `session.Save` writes only the parent's transcript. Subagent runs use ADK's in-memory session which is dropped after `agenttool.Run` returns. Persisting them would require new `transcript_subagent.go` paths; not requested.
- **An `agent_transfer`-style subagent shape** — explicitly chosen against (see design table). If a consumer later wants delegation-with-handoff, they can still wire `llmagent.Config.SubAgents` directly; we just don't expose it from `agent.New`.

---

### Critical Files for Implementation

- `/home/user/projects/core-agent/agent/agent.go` — add `Inner()` accessor; replace `TODO(subagents)` block.
- `/home/user/projects/core-agent/tools/subagent.go` — new file containing `SubagentOptions`, `NewSubagentTool`, depth tracking, gate + truncation wrapping.
- `/home/user/projects/core-agent/cmd/core-agent/main.go` — wire `--enable-subagent` flag and assemble the default subagent + tool.
- `/home/user/projects/core-agent/config/config.go` — add `SubagentConfig` struct + `Validate()` rules.
- `/home/user/go/pkg/mod/google.golang.org/adk@v1.2.0/tool/agenttool/agent_tool.go` — read-only reference; this is the ADK primitive being wrapped (cite for understanding its session/state behavior).
