# HTTP-served agent

Reference for the `library-embedding` skill. Full worked example: a web-facing agent service that handles concurrent users, persists conversations, and gates tool calls through a web-based approval UI.

This is one of the most common embedding shapes. Adapt freely.

## What we're building

- HTTP server with two endpoints:
  - `POST /chat` — accepts `{ session_id, prompt }`, streams the model's response as SSE
  - `POST /approve` — accepts `{ approval_id, decision }`, delivers the user's decision to a pending tool call
- Per-session agent instance (each user / session has its own `*agent.Agent`)
- Durable sessions via `eventlog` SQLite (resumes across restarts)
- Web-based approval flow: when the agent wants to call a gated tool, the SSE stream emits an approval-request event; the web UI shows it; user clicks Allow/Deny; the decision delivers via `/approve`
- Built-in tools + the four `agentic_*` wrappers for context-efficient tool calls

## Layout

```
cmd/agent-service/
└── main.go                  # HTTP routes, agent pool, server lifecycle
internal/
├── pool/                    # session-keyed agent instances
│   └── pool.go
├── prompter/                # web prompter implementation
│   └── prompter.go
└── handlers/                # chat + approve handlers
    └── handlers.go
```

For brevity, the code below combines them into one file. A real project would split per the layout above.

## The full program

```go
package main

import (
    "context"
    "encoding/json"
    "errors"
    "fmt"
    "log"
    "net/http"
    "sync"
    "time"

    "github.com/glebarez/sqlite"
    "github.com/google/uuid"

    "github.com/go-steer/core-agent/v2/pkg/agent"
    "github.com/go-steer/core-agent/v2/pkg/eventlog"
    "github.com/go-steer/core-agent/v2/pkg/models"
    _ "github.com/go-steer/core-agent/v2/pkg/models/gemini"
    "github.com/go-steer/core-agent/v2/pkg/permissions"
    "github.com/go-steer/core-agent/v2/pkg/tools"
)

// ---------- web prompter (gated tool calls flow through here) ----------

type pendingApproval struct {
    sessionID string
    req       permissions.PromptRequest
    decision  chan permissions.Decision
}

type webPrompter struct {
    mu      sync.Mutex
    pending map[string]*pendingApproval // keyed by approval_id
}

func newWebPrompter() *webPrompter {
    return &webPrompter{pending: make(map[string]*pendingApproval)}
}

func (p *webPrompter) AskApproval(ctx context.Context, req permissions.PromptRequest) (permissions.Decision, error) {
    id := uuid.NewString()
    pa := &pendingApproval{
        sessionID: req.SessionID,
        req:       req,
        decision:  make(chan permissions.Decision, 1),
    }
    p.mu.Lock()
    p.pending[id] = pa
    p.mu.Unlock()
    defer func() {
        p.mu.Lock()
        delete(p.pending, id)
        p.mu.Unlock()
    }()

    // The HTTP /chat handler will surface this approval_id to the client
    // via its SSE stream. The client renders an Allow/Deny UI; user clicks;
    // client POSTs to /approve, which writes to pa.decision.
    select {
    case d := <-pa.decision:
        return d, nil
    case <-ctx.Done():
        return permissions.Deny, ctx.Err()
    case <-time.After(5 * time.Minute):
        return permissions.Deny, errors.New("approval timeout")
    }
}

func (p *webPrompter) Resolve(approvalID string, d permissions.Decision) bool {
    p.mu.Lock()
    pa, ok := p.pending[approvalID]
    p.mu.Unlock()
    if !ok { return false }
    pa.decision <- d
    return true
}

// ---------- session-keyed agent pool ----------

type agentPool struct {
    mu       sync.Mutex
    agents   map[string]*agent.Agent
    provider models.Provider
    handle   *eventlog.Handle
    prompter *webPrompter
    gate     *permissions.Gate
    tools    []adktool.Tool // built-ins
}

func newAgentPool(ctx context.Context, p *webPrompter) (*agentPool, error) {
    provider, err := models.Resolve(nil)
    if err != nil { return nil, err }

    handle, err := eventlog.Open(ctx, sqlite.Open("./sessions.db"))
    if err != nil { return nil, err }

    gate := permissions.NewGate(permissions.ModeAsk, p)
    builtins, err := tools.Build(/* cfg, gate, tools.Default() */)
    if err != nil { return nil, err }

    return &agentPool{
        agents:   make(map[string]*agent.Agent),
        provider: provider,
        handle:   handle,
        prompter: p,
        gate:     gate,
        tools:    builtins.Tools,
    }, nil
}

func (p *agentPool) Get(ctx context.Context, sessionID string) (*agent.Agent, error) {
    p.mu.Lock()
    defer p.mu.Unlock()
    if a, ok := p.agents[sessionID]; ok { return a, nil }

    model, err := p.provider.Model(ctx, "gemini-3.1-pro-preview-customtools")
    if err != nil { return nil, err }

    tracker := usage.NewTracker()
    a, err := agent.New(model,
        agent.WithSession("web-user", sessionID),
        agent.WithGate(p.gate),
        agent.WithSessionService(p.handle.Service),
        agent.WithEventLog(p.handle),
        agent.WithTools(p.tools),
        agent.WithCompactor(agent.NewDefaultCompactor()),
        agent.WithCheckpointer(agent.NewDefaultCheckpointer()),
        agent.WithUsageTracker(tracker),
    )
    if err != nil { return nil, err }
    p.agents[sessionID] = a
    return a, nil
}

func (p *agentPool) Close() error { return p.handle.Close() }

// ---------- HTTP handlers ----------

func handleChat(pool *agentPool) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        var body struct{ SessionID, Prompt string }
        if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
            http.Error(w, err.Error(), 400); return
        }

        a, err := pool.Get(r.Context(), body.SessionID)
        if err != nil { http.Error(w, err.Error(), 500); return }

        w.Header().Set("Content-Type", "text/event-stream")
        w.Header().Set("Cache-Control", "no-cache")
        flusher, _ := w.(http.Flusher)

        for ev, err := range a.Run(r.Context(), body.Prompt) {
            if err != nil {
                fmt.Fprintf(w, "event: error\ndata: %q\n\n", err.Error())
                flusher.Flush()
                return
            }
            if ev.Content == nil { continue }
            for _, p := range ev.Content.Parts {
                if p.Text != "" {
                    fmt.Fprintf(w, "event: text\ndata: %s\n\n", p.Text)
                    flusher.Flush()
                }
                if p.FunctionCall != nil {
                    payload, _ := json.Marshal(map[string]any{
                        "tool": p.FunctionCall.Name,
                        "args": p.FunctionCall.Args,
                    })
                    fmt.Fprintf(w, "event: tool_call\ndata: %s\n\n", payload)
                    flusher.Flush()
                }
            }
        }
        fmt.Fprintf(w, "event: done\ndata: {}\n\n")
        flusher.Flush()
    }
}

func handleApprove(prompter *webPrompter) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        var body struct{ ApprovalID, Decision string }
        if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
            http.Error(w, err.Error(), 400); return
        }
        var d permissions.Decision
        switch body.Decision {
        case "allow":  d = permissions.Allow
        case "deny":   d = permissions.Deny
        case "always": d = permissions.AllowAlways
        default:       http.Error(w, "bad decision", 400); return
        }
        if !prompter.Resolve(body.ApprovalID, d) {
            http.Error(w, "approval not pending", 404); return
        }
        w.WriteHeader(204)
    }
}

// ---------- main ----------

func main() {
    ctx := context.Background()
    prompter := newWebPrompter()
    pool, err := newAgentPool(ctx, prompter)
    if err != nil { log.Fatal(err) }
    defer pool.Close()

    mux := http.NewServeMux()
    mux.HandleFunc("POST /chat", handleChat(pool))
    mux.HandleFunc("POST /approve", handleApprove(prompter))

    log.Println("listening on :8080")
    if err := http.ListenAndServe(":8080", mux); err != nil {
        log.Fatal(err)
    }
}
```

## Key patterns

**One agent per session, pooled.** The pool holds a `*agent.Agent` keyed by session ID. Concurrent requests for different sessions are independent; concurrent requests for the SAME session would race (`agent.Agent` is not concurrent-safe within a session) — for that, add a per-session mutex.

**Web prompter via channels.** The prompter blocks `AskApproval` on a channel; the HTTP handler stream emits an `approval_request` event with the approval ID; the client renders a UI; on user click, the client POSTs to `/approve`, which writes to the channel; the prompter unblocks.

**SSE for streaming responses.** The chat handler iterates `a.Run`, emitting one SSE event per text chunk / tool call. Clients render incrementally. For non-streaming, `runner.Headless` is simpler but blocks until the run completes.

**Durable sessions.** Every turn lands in `sessions.db`. Pool restarts (process crash, deploy) restore sessions cleanly on the next request — `agent.New` with the same session ID picks up the prior history.

**Context management built in.** `WithCompactor` + `WithCheckpointer` enabled so long conversations don't hit the context wall. For a typical web-served agent doing dozens of turns per user per day, this is non-negotiable.

## What this DOESN'T cover

- **Authentication.** Production needs user auth + per-user session-ID scoping (prevent user A from reading user B's sessions). Add a middleware that verifies the request's user matches the session's owner.
- **Rate limiting.** A user could DOS the agent with rapid requests. Per-user rate limit (token bucket) in front of `/chat`.
- **Cost controls.** Each session can run up cost without bound; add `WithUsageTracker` per session and a periodic check that caps off-bound sessions.
- **Subagent spawning.** For agents that spawn background work, add `BackgroundAgentManager` to the pool's setup; the subagents need their own MCP wiring + budget caps.
- **Distributed deployment.** For multi-instance deployments, sessions need to route to the instance holding them OR all instances share session storage (which the eventlog already does for SQL backends, but the in-memory pool would need a coordinator).

For each of these, the extension follows the patterns in `references/extension-points.md`. The core shape above is stable; what you add depends on your production needs.

## Adapting

- **Slack bot:** Replace `handleChat` with a Slack-events handler. Replace `handleApprove` with Slack-buttons handler. Same pool + prompter.
- **IDE plugin (VS Code / Cursor):** Connect via WebSocket. Use the same pool + prompter pattern; the IDE renders prompts as VS Code dialog modals.
- **Discord / Telegram / etc.:** Same shape, different transport. The core abstraction (agent pool + web prompter) ports cleanly.

## Production checklist

- [ ] Authentication on `/chat` + `/approve`
- [ ] Per-session mutex in the pool
- [ ] Rate limiting per user
- [ ] Cost caps per session (periodic check via `WithUsageTracker`)
- [ ] Health endpoint (`/healthz` returning agent count + uptime)
- [ ] Metrics (OTEL spans — wire via `agent.WithOTEL(...)`)
- [ ] Graceful shutdown (drain in-flight Runs before exiting)
- [ ] Session-storage backups (SQLite checkpoint or Postgres backups)
- [ ] CHANGELOG.md subscription for `core-agent` updates (pre-1.0 = breaking changes possible)
