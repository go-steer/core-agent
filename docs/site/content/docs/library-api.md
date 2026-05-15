---
title: Library API
weight: 8
---

# Library API

`core-agent` is designed to be embedded as a Go library. The bundled `cmd/core-agent` is a thin reference wrapper; production consumers will typically write their own binary that composes these packages.

---

## Package overview

| Import path | Purpose |
|---|---|
| `github.com/go-steer/core-agent/agent` | Multi-turn agent wrapping ADK's `llmagent + runner`. |
| `github.com/go-steer/core-agent/instruction` | `AGENTS.md` / `CLAUDE.md` / `GEMINI.md` fallback loader. |
| `github.com/go-steer/core-agent/config` | `.agents/config.json` schema, discovery, atomic persist. |
| `github.com/go-steer/core-agent/permissions` | Ask / allow / yolo gate; bash denylist; path scope. |
| `github.com/go-steer/core-agent/tools` | `GateToolset` wrapper bridging permissions to ADK toolsets. |
| `github.com/go-steer/core-agent/mcp` | MCP server lifecycle from `.agents/mcp.json`. |
| `github.com/go-steer/core-agent/skills` | `SKILL.md` discovery → ADK `skilltoolset`. |
| `github.com/go-steer/core-agent/models` | `Provider` interface + registry / `Resolve()`. |
| `github.com/go-steer/core-agent/models/gemini` | Gemini API + Vertex AI provider. |
| `github.com/go-steer/core-agent/models/anthropic` | Anthropic / Claude provider (first-party + Vertex). |
| `github.com/go-steer/core-agent/telemetry` | OpenTelemetry exporter setup. |
| `github.com/go-steer/core-agent/usage` | Per-turn token + cost tracker. |
| `github.com/go-steer/core-agent/session` | Transcript persistence (`.agents/sessions/`). |
| `github.com/go-steer/core-agent/runner` | Headless (one-shot) + REPL (multi-turn) drivers. |

---

## Minimal example

The shortest possible program: pick a Gemini model, run one turn, print partial text:

```go
package main

import (
    "context"
    "fmt"
    "log"

    "github.com/go-steer/core-agent/agent"
    "github.com/go-steer/core-agent/config"
    "github.com/go-steer/core-agent/models"
    _ "github.com/go-steer/core-agent/models/gemini"
)

func main() {
    cfg := config.DefaultConfig()
    cfg.Model.Provider = config.ProviderGemini

    provider, err := models.Resolve(cfg)
    if err != nil { log.Fatal(err) }

    ctx := context.Background()
    m, err := provider.Model(ctx, cfg.Model.Name)
    if err != nil { log.Fatal(err) }

    a, err := agent.New(m, agent.WithInstruction("Be concise."))
    if err != nil { log.Fatal(err) }

    for event, err := range a.Run(ctx, "What is the capital of France?") {
        if err != nil { log.Fatal(err) }
        if event.Content == nil { continue }
        for _, p := range event.Content.Parts {
            if p.Text != "" && event.Partial {
                fmt.Print(p.Text)
            }
        }
    }
    fmt.Println()
}
```

The blank import on the provider package matters — it triggers the `init()` that registers the provider with `models.Register`. Without it, `models.Resolve` errors with "unknown provider".

---

## Multi-turn conversation

`agent.Agent` reuses the same `runner.Runner` across `Run()` calls. The ADK's session service appends events on each call, so the second call sees the first turn's history automatically:

```go
a, _ := agent.New(m)
ctx := context.Background()

for _, prompt := range []string{"My name is Alex.", "What's my name?"} {
    for event, err := range a.Run(ctx, prompt) {
        if err != nil { log.Fatal(err) }
        // …consume partial text…
    }
}
```

The default session ID is `"default"`. To run multiple isolated conversations from one process, construct distinct agents per session and pass `agent.WithSession(userID, sessionID)`:

```go
a1, _ := agent.New(m, agent.WithSession("alice", "session-1"))
a2, _ := agent.New(m, agent.WithSession("bob",   "session-2"))
```

---

## Agent options

```go
agent.WithAppName(s string)            // identity used by ADK runner; default "core-agent"
agent.WithName(s string)               // agent display name (visible in OTEL spans)
agent.WithDescription(s string)        // agent description
agent.WithInstruction(s string)        // base system instruction
agent.WithSystemInstructionPrefix(s)   // prepends s to the instruction
agent.WithStreaming(m StreamingMode)   // override; default is StreamingModeSSE
agent.WithSession(userID, sessionID)   // override session identity
agent.WithTools(ts []tool.Tool)        // register individual tools
agent.WithToolsets(ts []tool.Toolset)  // register groups (MCP, skills, ...)
```

Options are applied in the order they're passed. Tools and toolsets accumulate across multiple calls.

---

## Built-in tools

The `tools/` package ships a six-tool baseline suitable for any agent that acts on its workspace: `read_file`, `write_file`, `edit_file`, `list_dir`, `bash`, `todo`. All six route through `permissions.Gate` (so the bash denylist and path-scope checks apply), and all honor the per-tool output caps from `cfg.ToolOutput`.

Wire them up in your binary:

```go
import "github.com/go-steer/core-agent/tools"

reg, err := tools.Build(cfg, gate, tools.Default())
if err != nil { /* ... */ }

a, _ := agent.New(m, agent.WithTools(reg.Tools))
```

`reg.Todo` exposes the underlying `*TodoStore` so a host can render plan progress (e.g. for a `/todo` slash command in a TUI) without round-tripping through the model.

To turn one off, set the field directly or use `Disable` (handy when you're applying a list of names from config or a CLI flag):

```go
b := tools.Default()
b.Bash = false             // by field
_ = b.Disable("write_file") // by canonical name; errors on typos
reg, _ := tools.Build(cfg, gate, b)
```

Or replace wholesale:

```go
reg, _ := tools.Build(cfg, gate, tools.BuiltinTools{ReadFile: true, ListDir: true})
```

`tools.Build` requires both `cfg` and `gate` — passing nil returns an error. We deliberately don't ship ungated tools (the bash denylist + path scope would silently stop applying).

The bundled `cmd/core-agent` enables the full set by default. `--no-builtin-tools` disables the whole suite; `--disable-tools=bash,write_file` (or `tools.disable` in config) turn off specific entries.

---

## Recording LLM turns

`recording.NewRecorder(inner, w io.Writer)` wraps any `model.LLM` and appends each turn (request + response stream) to `w` as a single JSONL line in the shared `recording.RecordedTurn` shape. The wrapper is transparent — callers see the inner LLM's responses unchanged — and the writer's lifecycle is the caller's responsibility.

```go
import "github.com/go-steer/core-agent/recording"

f, err := os.Create("session.jsonl")
if err != nil { /* ... */ }
defer f.Close()

m, _ := provider.Model(ctx, cfg.Model.Name)
m = recording.NewRecorder(m, f)

a, _ := agent.New(m, agent.WithTools(reg.Tools))
```

Replay the captured file with `mock.NewScripted(path, strict)` (or `--provider=scripted --script=path` from the CLI). See [Providers → Scripted]({{< relref "providers.md#scripted-mock" >}}) for the lenient/strict tradeoff and the "tool environment isn't recorded" caveat.

The recorder lives in `recording/` (not `models/mock/`) because it's production observability, not a test fixture — the package name shouldn't suggest you're only allowed to use it in tests.

---

## Adding custom tools

Use ADK's `functiontool.New` to wrap a Go function as a tool the agent can call. Schema is generated from the input/output struct types via `jsonschema` tags.

```go
import (
    adktool "google.golang.org/adk/tool"
    "google.golang.org/adk/tool/functiontool"
)

type addArgs struct {
    A int `json:"a" jsonschema_description:"first number"`
    B int `json:"b" jsonschema_description:"second number"`
}

type addResult struct {
    Sum int `json:"sum"`
}

func addTool() adktool.Tool {
    t, err := functiontool.New(
        functiontool.Config{
            Name:        "add",
            Description: "Add two integers and return the sum.",
        },
        func(_ adktool.Context, in addArgs) (addResult, error) {
            return addResult{Sum: in.A + in.B}, nil
        },
    )
    if err != nil { panic(err) }
    return t
}

a, _ := agent.New(m, agent.WithTools([]adktool.Tool{addTool()}))
```

See `examples/with-tools/` in the repo for a fuller example.

---

## Adding custom providers

Implement `models.Provider` and register it from an `init()`:

```go
package myprovider

import (
    "context"

    adkmodel "google.golang.org/adk/model"
    "github.com/go-steer/core-agent/config"
    "github.com/go-steer/core-agent/models"
)

func init() {
    models.Register("my-provider", newProvider)
}

type Provider struct{ /* …client state… */ }

func (p *Provider) Name() string { return "my-provider" }

func (p *Provider) Model(ctx context.Context, modelID string) (adkmodel.LLM, error) {
    // return a type implementing google.golang.org/adk/model.LLM
    return &llm{...}, nil
}

func newProvider(cfg *config.Config) (models.Provider, error) {
    // read cfg, return constructor
    return &Provider{...}, nil
}
```

See `models/anthropic/anthropic.go` for the canonical example. The `model.LLM` interface is small but exact — it streams genai-shaped events, so providers wrapping non-Gemini APIs need a conversion layer (see `models/anthropic/convert.go` and `stream.go`).

---

## Composing the full stack

The bundled `cmd/core-agent/main.go` is the canonical reference for wiring everything together. The minimum useful composition is roughly:

```go
cfg, agentsDir, _ := config.LoadOrDefault(cwd)
provider, _ := models.Resolve(cfg)
m, _ := provider.Model(ctx, cfg.Model.Name)

gate, _ := permissions.FromConfig(cfg, cwd, userHome, prompter)

instr, _ := instruction.Load(projectRoot, userHome)

_, mcpToolsets, _ := mcp.Build(ctx, agentsDir, sendFn, gate, elicitor)
loadedSkills, _   := skills.Load(ctx, agentsDir, gate)

allToolsets := append([]adktool.Toolset{}, mcpToolsets...)
if !loadedSkills.Empty() {
    allToolsets = append(allToolsets, loadedSkills.Toolset)
}

a, _ := agent.New(m,
    agent.WithToolsets(allToolsets),
    agent.WithSystemInstructionPrefix(instr.Instruction),
    agent.WithTools(myCustomTools),
)
```

Each step is independent — skip the ones you don't need (e.g. no MCP, no skills, no permission gate) and the layers below still work.

---

## Prompter

The permission gate's `Prompter` is the seam for interactive consent in `ask` mode. The bundled CLI doesn't ship one, so REPL-with-tools effectively requires `mode: yolo` or pre-baked allowlists. To plug in your own:

```go
type myPrompter struct{ /* UI handle */ }

func (p *myPrompter) AskApproval(ctx context.Context, req permissions.PromptRequest) (permissions.Decision, error) {
    // open a modal / read from terminal / call a Slack bot
    // return one of:
    //   permissions.DecisionDeny
    //   permissions.DecisionAllowOnce
    //   permissions.DecisionAllowSession
    //   permissions.DecisionAllowSessionTool
    //   permissions.DecisionAllowAlways
    return permissions.DecisionAllowOnce, nil
}

gate, _ := permissions.FromConfig(cfg, cwd, userHome, &myPrompter{})
```

When picking `DecisionAllowAlways`, the caller is responsible for persisting `req.PersistTool` + `req.PersistKey` into `cfg.Permissions.Allow` (or `cfg.PathScope.Allow` for path-scope prompts) and writing it back via `config.Save`.

---

## MCP status

`mcp.Build()` returns three values: per-server records, the toolsets to register, and an error.

```go
servers, toolsets, err := mcp.Build(ctx, agentsDir, send, gate, elicitor)
if err != nil { /* unrecoverable error in mcp.json itself */ }

for _, s := range servers {
    fmt.Printf("%-20s %s  %v\n", s.Name, s.Status, s.Tools)
    if s.Status == mcp.StatusError {
        fmt.Printf("  error: %v\n", s.Err)
    }
}
```

The records are how you build a `/mcp` slash command — they survive even when `Toolset()` returns nil for a failed server. Call `Server.Close()` on each before exiting (or before reloading) to terminate stdio child processes.

---

## MCP elicitation

The `mcp.ElicitorFn` signature:

```go
type ElicitorFn func(
    ctx context.Context,
    serverName string,
    req *mcp.ElicitRequest,
) (*mcp.ElicitResult, error)
```

Pass nil for `mcp.Build`'s elicitor argument to use the bundled `DeclineHandler`, which auto-declines every request and emits a one-line notice. For interactive hosts:

```go
elicitor := func(ctx context.Context, server string, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
    answer, err := ui.PromptUserFor(req.Params.Message, req.Params.RequestedSchema)
    if err != nil { return nil, err }
    return &mcp.ElicitResult{Action: "accept", Content: answer}, nil
}

_, _, _ = mcp.Build(ctx, agentsDir, send, gate, elicitor)
```

---

## Headless and REPL drivers

`runner/headless.go` and `runner/repl.go` are the canonical drivers used by the bundled CLI:

```go
runner.Headless(ctx, m, prompt, stdout, stderr, tracker, pricing, agentOpts...)
runner.REPL    (ctx, m, stdin,  stdout, stderr, tracker, pricing, agentOpts...)
runner.WriteSummary(stderr, tracker, m.Name())
```

They share a one-turn streamer that consumes the agent's event iterator, splits partial text → stdout / tool-call summaries → stderr, and updates the usage tracker. Reach for them when you want the same I/O conventions in your own binary; replace them when you need different rendering (e.g. JSON-stream output, Slack formatting, Bubble Tea TUI).

---

## Telemetry

```go
shutdown, err := telemetry.Setup(ctx, cfg.OTEL.Exporter)
if err != nil { /* ... */ }
defer func() { _ = shutdown(context.Background()) }()
```

Modes: `none` (default — no spans), `console` (stderr JSON), `otlp` (honors standard `OTEL_EXPORTER_OTLP_*` env vars). The shutdown function flushes buffered spans — call it before `os.Exit` or you'll lose recent activity.

---

## Transcripts

```go
session.Save(agentsDir, session.Transcript{
    StartedAt: started,
    Model:     m.Name(),
    Messages:  []session.Message{{Role: "user", Text: prompt}},
    Usage:     session.Usage{Turns: tot.Turns, InputTokens: tot.InputTokens, ...},
})
```

Atomic write to `<agentsDir>/sessions/<RFC3339-timestamp>.json`. Empty `agentsDir` is a no-op (no project root → nowhere to write). Schema is versioned (`SchemaVersion = 1`) for forward compatibility.
