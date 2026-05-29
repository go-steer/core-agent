# Extension points

Reference for the `library-embedding` skill. The seven main customization surfaces. Each section has the interface contract + a minimal working example.

For exhaustive type signatures see [library/api]({{< relref "library/api.md" >}}); this reference is the "what + when + minimal example" overview.

---

## 1 — Prompter (custom approval UX)

**When:** the bundled CLI prompts via stdin. For any other surface (web modal, Slack, IDE plugin), implement the prompter.

**Contract:**

```go
type Prompter interface {
    AskApproval(ctx context.Context, req PromptRequest) (Decision, error)
}
```

`PromptRequest` carries the tool name + the call args + a hint string. Return `Allow`, `AllowAlways`, `Deny`, or `DenyAlways`. Block until the user responds; if context cancels, return promptly.

**Minimal example (web-served approval):**

```go
type webPrompter struct {
    pending chan promptPair  // delivered to the HTTP handler
}

func (p *webPrompter) AskApproval(ctx context.Context, req permissions.PromptRequest) (permissions.Decision, error) {
    result := make(chan permissions.Decision, 1)
    select {
    case p.pending <- promptPair{req, result}:
    case <-ctx.Done():
        return permissions.Deny, ctx.Err()
    }
    select {
    case d := <-result:
        return d, nil
    case <-ctx.Done():
        return permissions.Deny, ctx.Err()
    }
}

// Wire to the agent:
a, err := agent.New(model,
    agent.WithGate(permissions.NewGate(permissions.ModeAsk, prompter)),
)
```

The HTTP handler reads from `p.pending`, renders an approval UI to the user, writes the user's decision back to `result`. See `references/http-served-agent.md` for the full pattern.

---

## 2 — Tools (domain operations)

**When:** the built-in nine tools cover code investigation and shell. For domain-specific ops (database queries, internal API calls, message bus publishes), define your own.

**Contract:**

```go
type Tool interface { /* ADK's tool.Tool — name, description, schema, handler */ }
```

The easiest path is `functiontool.New(config, handler)` — it derives schema from your Go types.

**Minimal example:**

```go
import "google.golang.org/adk/tool/functiontool"

type queryArgs struct {
    SQL string `json:"sql" jsonschema:"the SQL query to execute, read-only"`
}
type queryResult struct {
    Rows []map[string]any `json:"rows"`
}

queryTool, err := functiontool.New(functiontool.Config{
    Name:        "db_query",
    Description: "Run a read-only SQL query against the production replica. Returns up to 100 rows. Use INSTEAD OF guessing about data shape — read it.",
}, func(toolCtx tool.Context, args queryArgs) (queryResult, error) {
    rows, err := db.QueryContext(toolCtx, args.SQL) // your DB connection
    // ... materialize, cap at 100 rows
    return queryResult{Rows: rows}, nil
})

a, err := agent.New(model, agent.WithTools([]tool.Tool{queryTool}))
```

**Key patterns:**

- **Name + description are model-facing.** They're the trigger for the model to invoke the tool. Be specific in the description ("read-only SQL"), include `Use INSTEAD OF` language if there's a tool the model might fall back to.
- **Schema derives from Go types.** `jsonschema:""` struct tags become field descriptions. The model sees these — write them as instructions, not as struct field docs.
- **`tool.Context`** carries the call's context (`context.Context`) plus the agent's session info. Pass `toolCtx` to context-aware operations.
- **Errors propagate to the model.** Return a clear error message; the model sees it and decides whether to retry or surface the failure.

---

## 3 — Provider (alternative LLM backends)

**When:** the LLM you want isn't in `core-agent`'s shipped providers (`gemini`, `vertex`, `anthropic`, `anthropic-vertex`, `mock`). Common cases: local Ollama, internal company model server, a different commercial API.

**Contract:**

```go
type Provider interface {
    Name() string
    Model(ctx context.Context, modelID string) (model.LLM, error)
}
```

The harder part is implementing `model.LLM` (ADK's interface for the actual LLM call). It's a streaming-iterator interface — see ADK's source for the contract.

**Minimal example (skeleton):**

```go
package myllm

import (
    "context"
    "iter"

    adkmodel "google.golang.org/adk/model"
    "github.com/go-steer/core-agent/models"
)

type Provider struct {
    endpoint string
}

func (p *Provider) Name() string { return "myllm" }

func (p *Provider) Model(ctx context.Context, modelID string) (adkmodel.LLM, error) {
    return &llm{endpoint: p.endpoint, modelID: modelID}, nil
}

type llm struct {
    endpoint, modelID string
}

func (l *llm) Name() string { return l.modelID }

func (l *llm) GenerateContent(ctx context.Context, req *adkmodel.LLMRequest, stream bool) iter.Seq2[*adkmodel.LLMResponse, error] {
    return func(yield func(*adkmodel.LLMResponse, error) bool) {
        // Translate req to your backend's request shape.
        // Issue the call.
        // Yield each streaming chunk as an LLMResponse.
        // Final yield with TurnComplete=true.
    }
}

// Register via init() so models.Resolve("myllm") finds it.
func init() {
    models.Register("myllm", func(*config.Config) (models.Provider, error) {
        return &Provider{endpoint: defaultEndpoint}, nil
    })
}
```

This is the most involved extension because you're shimming a different API's streaming semantics. Look at `models/anthropic` or `models/gemini` for working examples.

---

## 4 — session.Service (custom persistence)

**When:** the bundled `eventlog` (SQLite / Postgres / MySQL via gorm) doesn't fit. Common cases: existing internal storage, Redis, custom audit format.

**Contract:** ADK's `session.Service` interface — `Get` / `Create` / `AppendEvent` / `Delete` etc. Several methods.

Most embeddings don't need this. The eventlog covers SQL backends well; for non-SQL, weigh the implementation cost (~200-400 lines of correctness-critical code) against using the bundled SQLite + a separate sync to your other system.

**Pattern:**

```go
import "google.golang.org/adk/session"

type myService struct { /* your storage handle */ }

func (s *myService) Get(ctx context.Context, req *session.GetRequest) (*session.GetResponse, error) { /* ... */ }
func (s *myService) Create(ctx context.Context, req *session.CreateRequest) (*session.CreateResponse, error) { /* ... */ }
func (s *myService) AppendEvent(ctx context.Context, sess *session.Session, ev *session.Event) error { /* ... */ }
// ... etc.

a, err := agent.New(model, agent.WithSessionService(s))
```

ADK's interface is the authoritative spec. Read its godoc + tests when implementing.

---

## 5 — Compactor + Checkpointer (context management customization)

**When:** the built-in defaults work for almost everyone. Customize when you need a domain-specific summarization prompt or a custom threshold.

**Contract:**

```go
type Compactor interface {
    ShouldCompact(ctx context.Context, a *Agent) bool
    SummarizerInstruction(focus string) string
}

type Checkpointer interface {
    ShouldCheckpoint(ctx context.Context, a *Agent) bool
    CheckpointInstruction(taskNote string) string
}
```

**Minimal example (custom summarizer):**

```go
type domainCompactor struct{}

func (c *domainCompactor) ShouldCompact(_ context.Context, a *agent.Agent) bool {
    // Use the agent's tracker to inspect context utilization.
    // Default DefaultCompactor's threshold is 0.85; tune as needed.
    return agent.DefaultCompactionThreshold > /* your check */
}

func (c *domainCompactor) SummarizerInstruction(focus string) string {
    return `You are compacting a long conversation about <domain>. Produce a structured handover with these sections: ...`
}

a, err := agent.New(model, agent.WithCompactor(&domainCompactor{}))
```

For most cases, `agent.NewDefaultCompactor()` and `agent.NewDefaultCheckpointer()` are fine. Override only when you have specific domain needs (e.g., medical records summary that must preserve specific fields; legal-discovery summary with audit-trail markers).

---

## 6 — Background subagents (`BackgroundAgentManager`)

**When:** your embedded agent should be able to spawn background work (long-running subagents, fan-out tasks).

**Contract:** `BackgroundAgentManager` constructs subagents on demand; `NewBackgroundSpawnTools` adds `spawn_agent` / `list_agents` / `check_agent` / `stop_agent` tools to the parent.

**Minimal example:**

```go
bgMgr, err := agent.NewBackgroundAgentManager(
    agent.WithBackgroundProvider(provider, "gemini-2.5-flash"),
    agent.WithBackgroundGate(gate),
    agent.WithBackgroundCatalog(builtinTools),
)
if err != nil { log.Fatal(err) }
defer bgMgr.Close()

spawnTools := agent.NewBackgroundSpawnTools(bgMgr)
allTools := append(builtinTools, spawnTools...)

a, err := agent.New(model,
    agent.WithBackgroundManager(bgMgr),
    agent.WithTools(allTools),
)
```

Subagents run in-process by default. For remote execution (K8s Job, Cloud Run), see § 7.

---

## 7 — RemoteAgentSpawner (distributed subagent execution)

**When:** subagents need to run as separate processes / pods / cloud functions rather than in the parent's process.

**Contract:**

```go
type RemoteAgentSpawner interface {
    Spawn(ctx context.Context, req SpawnRequest) (SpawnResult, error)
}
```

The parent calls `Spawn` when `spawn_agent` is invoked; the spawner is responsible for actually starting the agent in your runtime (K8s, Cloud Run, etc.) and returning a handle the parent can `check_agent` / `stop_agent` against.

**Pattern (skeleton):**

```go
type k8sJobSpawner struct {
    client kubernetes.Interface
}

func (s *k8sJobSpawner) Spawn(ctx context.Context, req agent.SpawnRequest) (agent.SpawnResult, error) {
    // 1. Translate req (goal, budgets, etc.) to a Job spec
    // 2. Apply the Job
    // 3. Return a handle keyed on the Job name; the parent uses it
    //    for check_agent / stop_agent
}

bgMgr, err := agent.NewBackgroundAgentManager(
    // ... other options
    agent.WithRemoteSpawner(&k8sJobSpawner{client: kubeClient}),
)
```

This is the most distributed-systems-heavy extension. Look at `extras/scion-agent/` or `extras/ax-agent/` for working examples against specific runtimes.

---

## Composing all of them

A non-trivial embedded agent typically uses 3-4 extensions:

```go
provider := myCustomProvider // implements models.Provider
model, _ := provider.Model(ctx, "my-model")

prompter := &webPrompter{ /* ... */ }
gate := permissions.NewGate(permissions.ModeAsk, prompter)

handle, _ := eventlog.Open(ctx, sqlite.Open("./sessions.db"))

domainTools := []tool.Tool{queryTool, publishTool, ...}

a, err := agent.New(model,
    agent.WithGate(gate),
    agent.WithSessionService(handle.Service),
    agent.WithEventLog(handle),
    agent.WithTools(append(builtinTools, domainTools...)),
    agent.WithCompactor(agent.NewDefaultCompactor()),
    agent.WithUsageTracker(tracker),
)
```

Each option is independent. Defaults are sensible; override only what you need.

For a full worked HTTP-served agent that uses Prompter + custom tools + durable sessions + autonomous patterns, see `references/http-served-agent.md`.
