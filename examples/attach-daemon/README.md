# `attach-daemon` — a headless attach-mode daemon, built from the library

The canonical embedding shape after the pkg/agent decomposition
(#388): a **library-built** daemon that serves one agent over the
attach HTTP/SSE surface — no `cmd/core-agent`, no TUI, no
credentials. The example then talks to itself over plain `net/http`,
the same wire `core-agent-tui` and `curl` speak.

## What this shows

- `agent.New` with the credential-free echo mock — the core agent
  carries only the narrow frozen surface.
- `attachadapter.New(a, ...)` — the attach capabilities (memory +
  skills providers here) live on the adapter, not the agent.
- `attach.NewSessionRegistry()` + `Register`, then
  `attach.NewServer(attach.Options{Registry, Addr: "127.0.0.1:0"})`.
  Tokenless loopback is allowed for local dev, but the server logs a
  loud warning — you'll see it first thing in the output.
- `runner.WakeLoop` — the wake-driven inbox drain every headless
  surface runs: `POST /inject` queues a message and fires a wake; the
  loop runs one empty-prompt turn that drains it.

## Run it

```bash
go run ./examples/attach-daemon
```

Hermetic: mock model, loopback listener, temp dirs. Exits 0 after a
clean shutdown.

## Key APIs

| Step | API |
|---|---|
| build the agent | `agent.New` + `agent.WithEventLog` (required for `/events`) |
| attach capabilities | `attachadapter.New`, `WithMemoryProvider`, `WithSkillsProvider` |
| serve | `attach.NewServer`, `Server.Bind` / `Serve` / `Addr` / `Close` |
| keep it alive | `runner.WakeLoop(ctx, a, runner.WakeLoopOptions{})` |

## What you should see

```
GET /sessions        -> core-agent/main (eventlog=true)
GET .../status       -> state=idle model=echo
POST .../inject      -> 200 (message queued, wake fired)
GET .../events (SSE):
  frame 1: event=capabilities
  ...
  ok: capabilities frame arrived first
daemon: shut down cleanly
```

The SSE protocol (v1.4.0) always sends the `capabilities` frame
first, so clients can detect features before any event arrives.

## Next pointers

- Multi-session (per-caller auth + on-demand sessions):
  [`examples/compose-multi-session`](../compose-multi-session).
- Real remote operation: swap the echo mock for a credentialled
  provider and set `Options.Auth.BearerToken` before binding beyond
  loopback (the server refuses non-loopback without auth).
- Wire shape details: `docs/attach-mode-design.md` and
  `internal/attachclient` (the bundled client of this same surface).
