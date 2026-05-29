# Minimal embed

Reference for the `library-embedding` skill. The smallest working `core-agent` integration. Foundation for everything else.

## The 20-line "hello world"

```go
package main

import (
    "context"
    "fmt"
    "log"
    "os"

    "github.com/go-steer/core-agent/pkg/agent"
    "github.com/go-steer/core-agent/pkg/models"
    _ "github.com/go-steer/core-agent/pkg/models/gemini"
)

func main() {
    ctx := context.Background()
    provider, err := models.Resolve(nil)  // reads .agents/config.json
    if err != nil { log.Fatal(err) }
    model, err := provider.Model(ctx, "gemini-3.1-pro-preview-customtools")
    if err != nil { log.Fatal(err) }

    a, err := agent.New(model)
    if err != nil { log.Fatal(err) }

    for ev, err := range a.Run(ctx, os.Args[1]) {
        if err != nil { log.Fatal(err) }
        if ev.Content != nil {
            for _, p := range ev.Content.Parts {
                if p.Text != "" { fmt.Print(p.Text) }
            }
        }
    }
    fmt.Println()
}
```

Build + run:

```bash
go build -o myagent
./myagent "what files are in this repo?"
```

That's it. The agent gets a model, registers no extra tools (built-ins are auto-wired), and runs one prompt to completion.

## What's happening

- **`models.Resolve(nil)`** — picks a provider from `.agents/config.json` (or auto-detects from env vars if no config). Pass a `*config.Config` to override.
- **`provider.Model(ctx, modelID)`** — constructs a model handle. The provider may pool / cache.
- **`agent.New(model)`** — constructs an agent with defaults (built-in tools registered, in-memory session, ADK runner wired). Options layer on top.
- **`a.Run(ctx, prompt)`** — returns `iter.Seq2[*session.Event, error]`. Iterate to drain; each event is one streaming chunk OR one tool call OR the final assistant message.

## Multi-turn

Call `a.Run` again with a new prompt. The session is preserved — the model sees the prior turn's exchange:

```go
for _, prompt := range []string{
    "what files are in this repo?",
    "which one looks most important?",
} {
    for ev, err := range a.Run(ctx, prompt) {
        // ... same as above
    }
}
```

Session is in-memory by default. For persistence across process restarts, use durable sessions (next section).

## Adding durable sessions

```go
import (
    "github.com/go-steer/core-agent/pkg/eventlog"
    "github.com/glebarez/sqlite"
)

handle, err := eventlog.Open(ctx, sqlite.Open("./sessions.db"))
if err != nil { log.Fatal(err) }
defer handle.Close()

a, err := agent.New(model,
    agent.WithSessionService(handle.Service),
    agent.WithEventLog(handle),
)
```

Now every turn lands in `sessions.db`. Stream replay via `handle.Stream.Since(ctx, seq, filters...)`. Crash-resume by reusing the same session ID across processes.

## Adding context management

Most embedded agents that handle non-trivial conversations need compaction + checkpoints:

```go
a, err := agent.New(model,
    agent.WithCompactor(agent.NewDefaultCompactor()),
    agent.WithCheckpointer(agent.NewDefaultCheckpointer()),
    agent.WithUsageTracker(tracker),  // required for compaction's threshold check
)
```

- `WithCompactor` enables automatic compaction at ~85% context utilization. Manual via `a.Compact(ctx, focus)`.
- `WithCheckpointer` enables `mark_task_done` model-facing tool + `a.Checkpoint(ctx, note)`.
- `WithUsageTracker` shares a `*usage.Tracker` so the threshold check has data to read.

See `library/api.md` for the full options surface.

## Run vs RunAutonomous vs Headless

Three driver shapes for different use cases:

| Driver | When |
|---|---|
| `a.Run(ctx, prompt)` | Interactive use, REPL-shaped, you control the loop |
| `agent.RunAutonomous(ctx, model, goal, opts...)` | Unattended, budget-bounded, model decides when done |
| `runner.Headless(ctx, model, prompt, stdout, stderr, ...)` | One-shot CLI-style with formatted output |

For most embedding, `a.Run` is what you want — it gives you full control over the event stream. `RunAutonomous` is for self-driven agents (see `references/extension-points.md` § Autonomous patterns or the `autonomous-setup` skill).

## Choosing models programmatically

If you need to swap models per-request (e.g., cheap model for short queries, frontier for hard ones):

```go
// At startup
providers := map[string]models.Provider{
    "gemini": geminiProvider,
    "claude": claudeProvider,
}

// Per request
modelID := pickModel(request)
model, err := providers[providerOf(modelID)].Model(ctx, modelID)
a, err := agent.New(model, opts...)
```

`agent.New` is cheap; constructing a new agent per-request is fine. The expensive bits (provider HTTP client setup) live in the `Provider`, which you reuse across agents.

## Logging events to stdout

The "hello world" above only prints text. For a Claude-Code-style streaming display, handle each event kind:

```go
for ev, err := range a.Run(ctx, prompt) {
    if err != nil { log.Fatal(err) }
    if ev.Content == nil { continue }
    for _, p := range ev.Content.Parts {
        switch {
        case p.Text != "":
            fmt.Print(p.Text)
        case p.FunctionCall != nil:
            fmt.Printf("\n[tool] %s(%v)\n", p.FunctionCall.Name, p.FunctionCall.Args)
        case p.FunctionResponse != nil:
            fmt.Printf("\n[result] %v\n", p.FunctionResponse.Response)
        }
    }
}
```

For richer formatting (color, indentation, glamour-rendered markdown), see how `runner.WriteEvents` does it — that's the source for the CLI's output.

## Concurrency

`agent.Agent` is NOT safe for concurrent `Run` calls on the same instance. The ADK runner manages session state; concurrent runs would race.

Patterns:

- **Per-request agent.** Construct a new `agent.New` per HTTP request. Cheap.
- **Per-user agent pool.** Map user ID → agent; mutex per agent. Reuses session state for repeat requests.
- **Per-session worker.** Goroutine owns one agent; route requests via channels.

The HTTP-served pattern (`references/http-served-agent.md`) walks through the per-request and per-session shapes.

## go.mod

```
require github.com/go-steer/core-agent v2.0.0
```

Pin the version. `core-agent` is pre-1.0; breaking changes can land at any minor version. Subscribe to releases / `CHANGELOG.md` to track.

## What to read next

- `references/extension-points.md` — customizing Prompter / tools / providers / sessions
- `references/http-served-agent.md` — full worked HTTP-served agent example
- [library/api]({{< relref "library/api.md" >}}) — exhaustive reference for every option function + public type
- [library/guide]({{< relref "library/guide.md" >}}) — narrative tour of the extension points
