---
title: Library guide
---

This guide is for Go engineers embedding `core-agent` in their own binary. Where the [user guide](../user-guide/) is about configuring the bundled CLI, this one is about *extending* the library — replacing the CLI's defaults with your own UI, tools, providers, and runtime topology.

The full API reference lives in [Library API](../library-api/). This guide is the narrative path through it, organized by extension point with worked examples for each.

## Who this is for

You're a Go engineer building one of:

- An agent with a UI that isn't a terminal (web, Slack, IDE plugin, custom TUI).
- An agent that needs domain-specific tools the built-ins don't cover (database queries, internal APIs, business workflows).
- An agent that delegates work to a runtime `core-agent` doesn't know about (Kubernetes Jobs, Cloud Run, a custom container scheduler).
- An agent against a model backend `core-agent` doesn't ship (OpenAI, a local Ollama instance, an internal inference service).
- Multiple agents composed into a larger system (orchestrators that fan work out to background workers, HTTP-served agents, batch processors).

If you just want to use the bundled CLI with custom configuration, the [user guide](../user-guide/) is enough.

## The minimal embed

The shortest possible program: pick a provider, build an agent, run one turn.

```go
package main

import (
    "context"
    "fmt"
    "log"

    "github.com/go-steer/core-agent/pkg/agent"
    "github.com/go-steer/core-agent/pkg/config"
    "github.com/go-steer/core-agent/pkg/models"
    _ "github.com/go-steer/core-agent/pkg/models/gemini"
)

func main() {
    cfg := config.DefaultConfig()
    cfg.Model.Provider = config.ProviderGemini

    provider, err := models.Resolve(cfg)
    if err != nil { log.Fatal(err) }

    ctx := context.Background()
    m, err := provider.Model(ctx, cfg.Model.Name)
    if err != nil { log.Fatal(err) }

    a, err := agent.New(m)
    if err != nil { log.Fatal(err) }

    for event, err := range a.Run(ctx, "what's the capital of France?") {
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

The blank `_` import on the provider package matters — it triggers the `init()` that registers the provider with `models.Register`. Each model package you import this way becomes available to `models.Resolve`.

This works, but it has no tools, no permissions, no persistence. The rest of this guide is about layering those in via extension points.

## The extension points

Every customization happens through one of these interfaces or option functions. The pattern across all of them is the same: `core-agent` ships sensible defaults; you replace any of them with `WithX(yourImplementation)`.

| Surface | Interface | When you'd extend it |
|---|---|---|
| Approvals | `permissions.Prompter` | UI is not a TTY (web, Slack, IDE plugin) |
| Tool execution | `tool.Tool` (via `functiontool.New`) | Domain operations, internal APIs |
| Model backend | `models.Provider` | LLM not in the box (OpenAI, local Ollama, …) |
| Remote subagents | `agent.RemoteAgentSpawner` | Delegate to K8s Job / Cloud Run / your runtime |
| Session persistence | `session.Service` | Beyond eventlog's SQLite/Postgres/MySQL |
| Background workers | `agent.BackgroundAgentManager` | Async tasks the parent's model spawns at runtime |
| Inbound messages | `agent.Inject(msg)` | Push input to a running agent from another goroutine |
| Tool inspection | `agent.WithBeforeTurn` | Rate limits, external approvals, custom budgets |
| Context management | `agent.Compactor` / `agent.Checkpointer` (v2.0+) | Custom summarizer prompts / thresholds; default `NewDefaultCompactor` + `NewDefaultCheckpointer` cover the common case |
| Agentic subtasks | `agent.RunSubtask` + `tools/agentic` wrappers (v2.0+) | Route specific tool calls through a cheap-model subtask so raw output doesn't bloat the parent's context |
| Late-binding hooks | `agent.WithPostConstruct` (v2.0+) | External tools whose handler needs the constructed `*Agent` (same pattern the in-tree `mark_task_done` uses) |

The rest of this page walks through the first six in order of how often you'd use them. The last four are covered in [Library API](../library-api/) and [Context management](/concepts/context-management/).

## Custom `Prompter` — bring your own approval UX

The bundled CLI prompts the user via stdin when the permission gate's `ask` mode triggers. For any other surface — a web app, a Slack bot, an IDE plugin — you implement `permissions.Prompter`:

```go
type Prompter interface {
    Ask(ctx context.Context, req Request) (Decision, error)
}
```

`Request` has the tool name, the arguments, and a human-readable summary. `Decision` is one of `Allow`, `Deny`, `AllowSession` (allow once, plus add to session allowlist), or `AllowAlways` (allow and persist to config).

A minimal HTTP-driven prompter:

```go
type httpPrompter struct {
    pending map[string]chan permissions.Decision
    mu      sync.Mutex
    notify  func(req permissions.Request, id string)
}

func (h *httpPrompter) Ask(ctx context.Context, req permissions.Request) (permissions.Decision, error) {
    id := uuid.NewString()
    ch := make(chan permissions.Decision, 1)
    h.mu.Lock()
    h.pending[id] = ch
    h.mu.Unlock()
    h.notify(req, id) // push to frontend via SSE / websocket

    select {
    case d := <-ch:
        return d, nil
    case <-ctx.Done():
        return permissions.Deny, ctx.Err()
    }
}

// Called from your HTTP handler when the user clicks Allow/Deny.
func (h *httpPrompter) Resolve(id string, d permissions.Decision) {
    h.mu.Lock()
    ch := h.pending[id]
    delete(h.pending, id)
    h.mu.Unlock()
    if ch != nil { ch <- d }
}
```

Wire it into the gate:

```go
gate, _ := permissions.FromConfig(cfg, cwd, "", myHTTPPrompter)
reg, _ := tools.Build(cfg, gate, tools.Default())
a, _ := agent.New(m, agent.WithTools(reg.Tools))
```

The gate handles everything else — pattern matching, the bash denylist, path scope, allow-list session memory. You only own the surface that displays the request and collects the user's choice.

For autonomous runs in CI (no human), use `permissions.AutoPrompter` (or `--ask=auto` in the CLI) — it refuses every prompt cleanly, which the model sees as a tool failure and adapts around. See [Library API → Prompter](../library-api/#prompter) for the full interface and behavior table.

## Custom tools — domain operations as model-callable functions

The built-in tools cover file I/O, shell, and search. For everything domain-specific — query a database, hit your internal API, run a business workflow — register a `tool.Tool` via `functiontool.New`:

```go
import (
    adktool "google.golang.org/adk/tool"
    "google.golang.org/adk/tool/functiontool"
)

type lookupOrderArgs struct {
    OrderID string `json:"order_id" jsonschema_description:"Acme order ID, e.g. ORD-12345"`
}

type lookupOrderResult struct {
    Status   string  `json:"status"`
    Total    float64 `json:"total_usd"`
    Customer string  `json:"customer"`
}

func lookupOrderTool(db *sql.DB, gate *permissions.Gate) adktool.Tool {
    t, err := functiontool.New(
        functiontool.Config{
            Name:        "lookup_order",
            Description: "Look up an Acme order by ID. Returns status, total, and customer.",
        },
        func(_ adktool.Context, in lookupOrderArgs) (lookupOrderResult, error) {
            if err := gate.Check(context.Background(), "lookup_order", in.OrderID); err != nil {
                return lookupOrderResult{}, err
            }
            row := db.QueryRow("SELECT status, total, customer FROM orders WHERE id = $1", in.OrderID)
            var r lookupOrderResult
            if err := row.Scan(&r.Status, &r.Total, &r.Customer); err != nil {
                return lookupOrderResult{}, fmt.Errorf("lookup_order: %w", err)
            }
            return r, nil
        },
    )
    if err != nil { panic(err) }
    return t
}
```

Then register it alongside the built-ins:

```go
reg, _ := tools.Build(cfg, gate, tools.Default())
allTools := append(reg.Tools, lookupOrderTool(db, gate))
a, _ := agent.New(m, agent.WithTools(allTools))
```

The `Description` is what the model reads to decide when to invoke the tool — write it the way you'd write a one-line API doc. The JSON schema for arguments is derived from your struct's `jsonschema` tags; field descriptions help the model populate them correctly.

A few patterns that matter:

- **Always route through the permission gate** with `gate.Check(...)`. Custom tools that bypass the gate undermine the security model the user configured for the rest of the agent.
- **Return errors as ordinary Go errors.** The agent loop wraps them and surfaces them to the model, which can recover. Don't `panic`.
- **Use the truncation helper.** If your tool can return a large response, wrap the output with `tools.Truncate(s, maxBytes, maxLines)` so a runaway result doesn't blow the context window. The built-in tools do this; consumer tools should too.

See [Library API → Adding custom tools](../library-api/#adding-custom-tools) for the full interface and edge cases.

## Custom `RemoteAgentSpawner` — delegate to remote runtimes

In-process subagents (`WithSubagents` / `BackgroundAgentManager`) run inside your binary. When you want to delegate work to a separate runtime — a Kubernetes Job, a Cloud Run invocation, your container scheduler — implement `agent.RemoteAgentSpawner`:

```go
type RemoteAgentSpawner interface {
    Spawn(ctx context.Context, req RemoteAgentRequest) (RemoteAgentHandle, error)
}

type RemoteAgentHandle interface {
    ID() string
    Events() <-chan RemoteAgentEvent
    Done() <-chan struct{}
    Stop(ctx context.Context) error
}
```

`Spawn` provisions a remote agent and returns a handle that streams events back. `Events()` delivers structured updates (`RemoteAgentEvent` carries kind, text, optional structured payload); `Done()` closes when the remote terminates; `Stop()` triggers shutdown.

The reference implementation for Scion's Hub HTTP API lives in [`extras/scion-remote-agent/`](https://github.com/go-steer/core-agent/tree/main/extras/scion-remote-agent) and is a good template. Sketch of a Kubernetes Jobs spawner:

```go
type k8sJobSpawner struct {
    client    *kubernetes.Clientset
    namespace string
    image     string
}

func (s *k8sJobSpawner) Spawn(ctx context.Context, req agent.RemoteAgentRequest) (agent.RemoteAgentHandle, error) {
    job := &batchv1.Job{
        ObjectMeta: metav1.ObjectMeta{GenerateName: "agent-"},
        Spec: batchv1.JobSpec{
            Template: corev1.PodTemplateSpec{
                Spec: corev1.PodSpec{
                    RestartPolicy: corev1.RestartPolicyNever,
                    Containers: []corev1.Container{{
                        Name:  "agent",
                        Image: s.image,
                        Args:  []string{"--input", req.Prompt, "--template", req.Template},
                    }},
                },
            },
        },
    }
    created, err := s.client.BatchV1().Jobs(s.namespace).Create(ctx, job, metav1.CreateOptions{})
    if err != nil { return nil, err }

    h := &k8sJobHandle{
        id:     created.Name,
        events: make(chan agent.RemoteAgentEvent, 32),
        done:   make(chan struct{}),
        client: s.client,
    }
    go h.tailLogs(ctx) // parse pod logs into RemoteAgentEvents
    return h, nil
}
```

Wire it into the agent so the model can invoke `spawn_remote_agent`:

```go
mgr := agent.NewBackgroundAgentManager(...)
spawner := &k8sJobSpawner{client: kClient, namespace: "agents", image: "myregistry/agent:v1"}
spawnRemote := agent.NewSpawnRemoteAgentTool(spawner, mgr)

a, _ := agent.New(m,
    agent.WithBackgroundManager(mgr),
    agent.WithTools(append(reg.Tools, spawnRemote)),
)
```

Each remote-spawn event lands in the parent's event log under `Branch="bg.<id>"` (or whatever branch label the spawner sets), so the audit trail stays unified across local + remote work. See [Library API → Remote (out-of-process) subagents](../library-api/#remote-out-of-process-subagents) for the full event taxonomy and three bundled classification strategies you can reuse (`PreferStructuredPayload`, `StringPrefix`, `Verbose`).

## Custom `models.Provider` — bring your own LLM backend

If you need a model `core-agent` doesn't ship — OpenAI, a local Ollama instance, an internal inference service — implement `models.Provider` and register it:

```go
type Provider interface {
    Name() string
    Model(ctx context.Context, name string) (model.LLM, error)
}
```

`Model(ctx, name)` returns an ADK `model.LLM` for the named model. The hard part is the `model.LLM` adapter — you need to translate ADK's request shape into your backend's API and stream the response back as `model.LLMResponse` events.

The Anthropic adapter in `models/anthropic/` is the reference implementation (~1,500 lines). For most backends, the pattern is:

```go
type myProvider struct {
    apiKey string
}

func (p *myProvider) Name() string { return "my-llm" }

func (p *myProvider) Model(ctx context.Context, name string) (model.LLM, error) {
    return &myLLM{apiKey: p.apiKey, modelName: name}, nil
}

type myLLM struct {
    apiKey    string
    modelName string
}

func (m *myLLM) Name() string { return m.modelName }

func (m *myLLM) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
    return func(yield func(*model.LLMResponse, error) bool) {
        // 1. Translate req.Contents into your backend's request shape.
        // 2. Translate req.Config.Tools into your backend's tool definitions.
        // 3. Call your backend; stream chunks back via yield.
        // 4. Emit one terminal !Partial response when done.
    }
}
```

Register via `init()` in your provider package:

```go
func init() {
    models.Register("my-llm", func(cfg *config.Config) (models.Provider, error) {
        return &myProvider{apiKey: cfg.Model.APIKey}, nil
    })
}
```

Then users select your provider via `cfg.Model.Provider = "my-llm"`. The blank-import pattern (`_ "yourorg/yourpkg/models/myllm"`) brings it into scope.

The thorny parts are tool-calling translation (every backend has different shapes) and streaming aggregation (turning per-chunk deltas into well-formed `Content.Parts`). The Anthropic adapter's `convert.go` and `stream.go` are good references. See [Library API → Adding custom providers](../library-api/#adding-custom-providers) for the full provider/model contract.

## Custom `session.Service` — alternative persistence

For most cases, `eventlog.Open(...)` with SQLite / Postgres / MySQL is enough. But if you need to plug into an existing system — your company's event bus, a NoSQL store, a managed memory service — implement `session.Service` directly:

```go
type Service interface {
    Create(ctx context.Context, req *CreateRequest) (*CreateResponse, error)
    Get(ctx context.Context, req *GetRequest) (*GetResponse, error)
    List(ctx context.Context, req *ListRequest) (*ListResponse, error)
    Delete(ctx context.Context, req *DeleteRequest) error
    AppendEvent(ctx context.Context, session Session, event *Event) error
}
```

Wire it in via `WithSessionService`:

```go
svc := myCustomSessionService{...}
a, _ := agent.New(m, agent.WithSessionService(svc))
```

This is the least common extension point — `eventlog.Open` with a real database covers most needs and gives you the `Stream` (`Since(seq)` / `Watch(seq)`) machinery for free. Only reach for a custom `Service` when you have an existing persistence layer you must integrate with. See [Library API → Durable sessions and audit log](../library-api/#durable-sessions-and-audit-log) for what the Stream API gives you out of the box.

## Background workers and the inbox

For agents that need to delegate fan-out work the parent's model decides on at runtime, use `BackgroundAgentManager` + the `spawn_agent` tool family. Background subagents run in goroutines inside the same process; events flow back through a per-turn drain so the parent sees them on its next turn.

```go
mgr := agent.NewBackgroundAgentManager(
    agent.WithBackgroundProvider(provider, cfg.Model.Name),
    agent.WithBackgroundTools(reg.Tools),
)
spawnLocal := agent.NewSpawnAgentTool(mgr)

parent, _ := agent.New(m,
    agent.WithBackgroundManager(mgr),
    agent.WithTools(append(reg.Tools, spawnLocal)),
)
```

The parent's model can now call `spawn_agent(name, prompt)` to fan out work in the background. Each spawned subagent's events land in the parent's event log under `Branch="bg.<name>"` so the audit trail stays unified.

For pushing input to a running agent from an external source — an HTTP handler, a webhook, a scheduled task — use the inbox:

```go
// In your HTTP handler:
agentInstance.Inject("new ticket arrived: " + ticketID)
```

The next turn drains the inbox and prepends queued messages above the prompt the model sees, sibling to background-subagent alerts. Use `agent.InboxArrived()` to wait for new input rather than polling. See [Library API → Inject](../library-api/#agentinjectmessage--queue-a-message-for-the-next-turn) for the full pattern.

## Worked example: HTTP-served agent

Pulling several extension points together — a thin HTTP server that exposes one agent per session, with custom approvals via a websocket back to the browser.

```go
package main

import (
    "context"
    "encoding/json"
    "log"
    "net/http"
    "sync"

    "github.com/glebarez/sqlite"
    "github.com/go-steer/core-agent/pkg/agent"
    "github.com/go-steer/core-agent/pkg/config"
    "github.com/go-steer/core-agent/pkg/eventlog"
    "github.com/go-steer/core-agent/pkg/models"
    _ "github.com/go-steer/core-agent/pkg/models/anthropic"
    "github.com/go-steer/core-agent/pkg/permissions"
    "github.com/go-steer/core-agent/pkg/tools"
)

type server struct {
    provider models.Provider
    cfg      *config.Config
    eventLog *eventlog.Handle
    prompter *httpPrompter // implementation from "Custom Prompter" section
    sessions sync.Map      // sessionID → *agent.Agent
}

func (s *server) handleChat(w http.ResponseWriter, r *http.Request) {
    sid := r.URL.Query().Get("session")
    a := s.getOrCreate(sid)

    var body struct{ Prompt string }
    json.NewDecoder(r.Body).Decode(&body)

    flusher, _ := w.(http.Flusher)
    w.Header().Set("Content-Type", "text/event-stream")

    for event, err := range a.Run(r.Context(), body.Prompt) {
        if err != nil {
            json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
            return
        }
        if event.Content == nil { continue }
        json.NewEncoder(w).Encode(event)
        flusher.Flush()
    }
}

func (s *server) getOrCreate(sid string) *agent.Agent {
    if a, ok := s.sessions.Load(sid); ok {
        return a.(*agent.Agent)
    }
    ctx := context.Background()
    m, _ := s.provider.Model(ctx, s.cfg.Model.Name)

    gate, _ := permissions.FromConfig(s.cfg, "", "", s.prompter)
    reg, _ := tools.Build(s.cfg, gate, tools.Default())

    a, _ := agent.New(m,
        agent.WithSession("user", sid),
        agent.WithEventLog(s.eventLog), // crash-resume: same sid resumes the session
        agent.WithTools(reg.Tools),
    )
    s.sessions.Store(sid, a)
    return a
}

func main() {
    cfg := config.DefaultConfig()
    cfg.Model.Provider = config.ProviderAnthropic
    cfg.Model.Name = "claude-opus-4-7"
    cfg.Permissions.Mode = config.PermissionModeAsk

    provider, err := models.Resolve(cfg)
    if err != nil { log.Fatal(err) }

    ctx := context.Background()
    eventLog, err := eventlog.Open(ctx, sqlite.Open("sessions.db"))
    if err != nil { log.Fatal(err) }
    defer eventLog.Close()

    s := &server{
        provider: provider, cfg: cfg, eventLog: eventLog,
        prompter: newHTTPPrompter(),
    }

    http.HandleFunc("/chat", s.handleChat)
    http.HandleFunc("/approve/", s.prompter.handleApprove) // user clicks Allow/Deny
    http.ListenAndServe(":8080", nil)
}
```

What's wired here:

- **One agent per session.** `sync.Map` keyed by `sessionID`; same SID across requests resumes the same conversation.
- **Custom prompter.** Permission requests get pushed to the browser via SSE; the user's Allow/Deny click resolves the gate via `prompter.Resolve(id, decision)`.
- **Durable event log.** Conversation + tool calls + permission decisions all land in SQLite. If the server crashes, restarting and hitting `/chat` with the same SID resumes the session.
- **Default tool suite gated.** File / shell / search tools all route through the permission gate the user clicks through in the browser.

This pattern scales to roughly a few hundred concurrent sessions in one process. For higher fan-out, split the agent runtime (the HTTP server above) from the model calls (an internal gRPC service per provider) and use the in-process agent against a thin LLM client that talks to that service.

## Where to go next

- **[Library API](../library-api/)** — exhaustive reference for every type and option mentioned here, plus details deferred for narrative flow.
- **[Autonomous runs](../autonomous/)** — the `RunAutonomous` driver with budgets, lifecycle tool, ask-mode behavior. Most server-side embedders also run autonomous workloads.
- **[Sessions and event log](../sessions/)** — the Stream API (`Since(seq)`, `Watch(seq)`), the session lock, the audit-log shape.
- **[Permissions](../permissions/)** — pattern grammar, path scope details, the prompter contract.
- **[`extras/scion-remote-agent/`](https://github.com/go-steer/core-agent/tree/main/extras/scion-remote-agent)** — full reference implementation of `RemoteAgentSpawner`, including the SSE log classification machinery.
- **[`examples/`](https://github.com/go-steer/core-agent/tree/main/examples)** — runnable embedding patterns: `basic`, `with-tools`, `with-subagent`, `background-monitor`, `autonomous`, `autonomous-handle`, `autonomous-resume`, `replay`, `streaming`, `scion-research-demo`.
