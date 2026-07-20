---
name: library-embedding
description: Walk a Go developer through embedding core-agent in their own binary. Use when the user asks "how do I embed core-agent", "use core-agent as a library", "build my own agent on core-agent", "agent.New", "custom prompter", "custom tools", "custom provider", "HTTP-served agent", "integrate core-agent into [Go project]", or asks any question implying they want to use the Go API rather than the bundled CLI.
---

When invoked:

1. **Confirm the use case.** Embedding fits when:
   - You're building a domain-specific agent (web service, custom UI, IDE plugin)
   - You need extension points the CLI doesn't expose (custom prompter, custom tools, custom provider)
   - You're wrapping `core-agent` in a larger system (orchestrator, agent platform)

   If the user just wants to customize `core-agent`'s behavior for their project, the CLI + skills covers it without embedding. Walk them to the `cli-setup` skill instead.

2. **Start with the minimal embed.** Show the 20-line "hello world" — `agent.New` + `Run` + iterate events. Most embedding work builds on this base. Use `references/minimal-embed.md`.

3. **Identify the extension point they need.** Most embedding use cases come down to one of:
   - Custom **Prompter** — different approval UX (web modal, Slack button, IDE dialog)
   - Custom **tool** — domain operations the built-in nine don't cover
   - Custom **Provider** — LLM backend not in the box (local Ollama, internal model server)
   - Custom **session.Service** — persistence beyond eventlog (Redis, custom DB)
   - **`RemoteAgentSpawner`** — delegate background subagents to K8s Jobs, Cloud Run, etc.

   Fetch `references/extension-points.md` to walk them through the relevant one.

4. **Show a full worked example.** For non-trivial embeddings (web service, HTTP-served agent), build out a complete program. Use `references/http-served-agent.md` for the canonical example.

5. **Discuss long-term maintenance.** `core-agent` is pre-1.0; breaking changes can land at any minor version. If the user is shipping production code on top of it:
   - Pin the dep version in `go.mod`
   - Subscribe to releases / `CHANGELOG.md`
   - Use the documented `agent.Option` interface — not internal/private symbols

## Triggers in detail

Beyond the description, also match on:

- "How do I add a custom tool to core-agent"
- "Build an HTTP-served agent"
- "Background workers with core-agent"
- "Integrate core-agent into my existing Go service"
- "Customize the approval UX for a web app"
- "Bring my own LLM backend"
- "Programmatic control of an agent run"

## References

Fetch based on what the user is building:

- **`references/minimal-embed.md`** — the smallest working `agent.New` + `Run` loop. Read first for any embedding question; it's the foundation.
- **`references/extension-points.md`** — the seven main customization surfaces (`WithTools`, `WithPrompter`, `WithProvider`, `WithSessionService`, `WithCompactor`, `WithCheckpointer`, `RemoteAgentSpawner`) with one paragraph per. Read when narrowing in on the user's specific extension need.
- **`references/http-served-agent.md`** — full worked HTTP-served agent example. Read when the user is building a web-facing service; covers session-per-request vs persistent-session shapes, prompter integration, streaming responses.

## Procedure: minimal embed

Walk the user through this skeleton first. Everything else is variations on it:

```go
package main

import (
    "context"
    "fmt"
    "log"
    "os"

    "github.com/go-steer/core-agent/v2/pkg/agent"
    "github.com/go-steer/core-agent/v2/pkg/models"
    _ "github.com/go-steer/core-agent/v2/pkg/models/gemini"
)

func main() {
    ctx := context.Background()
    provider, err := models.Resolve(nil) // reads .agents/config.json
    if err != nil { log.Fatal(err) }
    model, err := provider.Model(ctx, "gemini-3.1-pro-preview-customtools")
    if err != nil { log.Fatal(err) }

    a, err := agent.New(model)
    if err != nil { log.Fatal(err) }

    prompt := os.Args[1]
    for ev, err := range a.Run(ctx, prompt) {
        if err != nil { log.Fatal(err) }
        if ev.Content != nil {
            for _, p := range ev.Content.Parts {
                if p.Text != "" {
                    fmt.Print(p.Text)
                }
            }
        }
    }
    fmt.Println()
}
```

That's it. Multi-turn conversation: call `a.Run(ctx, prompt)` again with a new prompt — session history is preserved automatically. Run-to-completion (no streaming): use `runner.Headless` instead.

## Procedure: pick extension points

Walk the user through what they're customizing:

| Want | Extension | Reference |
|---|---|---|
| Different approval UX | `WithPrompter(impl)` | `references/extension-points.md` § Prompter |
| Domain-specific tool | `WithTools([]tool.Tool{...})` | `references/extension-points.md` § Tools |
| LLM backend not in box | Implement `models.Provider`, register with `models.Register` | `references/extension-points.md` § Provider |
| Custom persistence | `WithSessionService(impl)` | `references/extension-points.md` § Session |
| Long-session survival | `WithCompactor(...)` + `WithCheckpointer(...)` | (built-in defaults usually fine) |
| Distributed subagent execution | Implement `agent.RemoteAgentSpawner` | `references/extension-points.md` § Remote subagents |
| HTTP-served agent | All of the above + handler shape | `references/http-served-agent.md` |

For each one, fetch the relevant section and walk through the implementation contract + a minimal example.

## Procedure: structured persistence

If the user is building anything beyond a one-shot CLI, push them toward durable sessions early:

```go
import "github.com/go-steer/core-agent/v2/pkg/eventlog"
import "github.com/glebarez/sqlite"

handle, err := eventlog.Open(ctx, sqlite.Open("./sessions.db"))
if err != nil { log.Fatal(err) }
defer handle.Close()

a, err := agent.New(model,
    agent.WithSessionService(handle.Service),
    agent.WithEventLog(handle),
)
```

Now every turn, every tool call, every model response lands in `sessions.db`. Crash-resume, audit log, replay, live tail — all unlocked.

## Procedure: composition with autonomous + subagents

If the embedded agent is itself autonomous OR spawns subagents, use `RunAutonomous` + `BackgroundAgentManager`:

```go
import "github.com/go-steer/core-agent/v2/pkg/agent"

bgMgr, err := agent.NewBackgroundAgentManager(
    agent.WithBackgroundProvider(provider, "gemini-2.5-flash"),
    agent.WithBackgroundGate(gate),
    agent.WithBackgroundCatalog(builtinTools),
)
if err != nil { log.Fatal(err) }
defer bgMgr.Close()

a, err := agent.New(model,
    agent.WithBackgroundManager(bgMgr),
    agent.WithTools(append(builtinTools, agent.NewBackgroundSpawnTools(bgMgr)...)),
)
```

The parent now has `spawn_agent`, `list_agents`, `check_agent`, `stop_agent`. The user's `AGENTS.md` (or in-prompt instruction) governs when to use them.

## When NOT to use this skill

- The user is using the **bundled CLI** (`core-agent` binary) and asking about configuration. Use `cli-setup` skill.
- The user is configuring **autonomous / unattended runs** of the bundled CLI. Use `autonomous-setup` skill.
- The user is asking a reference question (function signature, type definition). Answer directly from [library/api]({{< relref "library/api.md" >}}) docs; no need to walk the whole embedding procedure.
- The user wants to **modify `core-agent` itself** (contribute upstream changes). That's a different conversation; point them at `CONTRIBUTING.md`.

## Output style

Code-heavy but with clear narrative. Embedding work is mostly Go — show the code, explain what each block does. Don't generate a wall of code without explanation; the user is going to maintain it.

For non-trivial embeddings, propose a project layout (`cmd/`, `internal/`, etc.) before writing code. Give the user a sense of where things go so the snippets fit into context.

When showing extension points, ALWAYS include the interface contract being satisfied. E.g., "you need to implement `permissions.Prompter` which is `interface { AskApproval(ctx, req) (Decision, error) }`" — not just "implement a prompter."
