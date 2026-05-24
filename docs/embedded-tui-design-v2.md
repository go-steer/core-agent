# Embedded TUI v2: `core-agent` IS the TUI (lifted from cogo)

Design doc for replacing the line-mode REPL inside `core-agent` with
an in-process bubble-tea TUI, so a bare `core-agent` invocation drops
the operator directly into a Claude-Code-style interactive surface.
Reverses the central decision of
[`embedded-tui-design.md`](embedded-tui-design.md) (v1) after UAT
showed the spawn-and-attach approach has the wrong shape for the
"I just want to chat with an agent" case.

The implementation is a **lift** from `../cogo/internal/tui/` — the
same `go-steer/cogo` project from which `core-agent`'s agent loop
was originally extracted. cogo already built the in-process TUI we'd
otherwise be designing from scratch; we re-host it on core-agent's
runtime rather than re-derive it.

## Why this doc exists

v1 said: two binaries, `core-agent-tui --local` spawns a sibling
`core-agent` over a private unix socket and attaches via attach-mode.
The shape works — interrupt, queue panel, slash commands all flow
through the same `attachclient` pipe used for remote — but UAT
surfaced three persistent papercuts:

1. **Two processes for one mental model.** Cold-start lag (≥1 s),
   doubled memory footprint, log lines split across two streams,
   and the operator has to grep `/tmp/<sock>.log` to see what their
   own agent is doing. Operators kept asking "is this remote?".
2. **Indirection bugs.** SSE timeout hid model responses; OSC 11
   queries leaked into the input box; the spawned agent's stdin EOF
   killed the REPL; the queue panel's `[Inbox]` wrapper broke
   text-matching. Each was real and fixed, but each only exists
   because we routed local UI through an HTTP+SSE channel designed
   for remote control.
3. **The user-facing command is wrong.** `core-agent` by itself
   should be the thing you run to chat with an agent — that's the
   convention every comparable tool (Claude Code, Antigravity, the
   various IDE-embedded agents) follows. `core-agent-tui --local`
   is ceremony for the common case.

## What this replaces from v1

| v1 decision (do-not-relitigate) | v2 replacement | Why |
|---|---|---|
| One TUI codebase under `cmd/core-agent-tui/`; `--local` is spawn-and-attach | Move (lifted) TUI code to `internal/tui/`; `core-agent` runs it in-process | Removes the IPC layer for local use; reuses cogo's mature UI work |
| Default `core-agent` keeps line-mode REPL | Default `core-agent` launches the TUI when stdin is a TTY | One command to remember; non-TTY paths still get headless behavior |
| Two distroless images, both bubble-tea-clean for headless | `core-agent` carries bubble-tea by default; an optional `no_tui` build tag strips it for K8s images | Slightly bigger default binary, opt-out for size-sensitive deployments |
| `core-agent-tui` is the one-and-only TUI binary | `core-agent-tui` survives as the **remote-only** client (no agent runtime, no model providers, no tools) | Different use case — attach to a *remote* agent without shipping a model runtime locally |

## The lift: what comes from cogo

cogo's `internal/tui/` is ~7100 lines across ~35 files, Apache-2,
same `go-steer` org, in-process architecture (bubble-tea program +
agent goroutine + `tea.Msg` pump — exactly what v2 would otherwise
design). Coupling to cogo's internals is concentrated in two glue
files (`model.go`, `program.go`, ~7 internal imports each); the
other ~33 files are bubble-tea-pure UI.

**Comes over essentially as-is** (no core-agent-specific knowledge):

- `keys.go`, `palette.go`, `palette_test.go` — command palette
- `markdown.go`, `markdown_test.go`, `wrap_test.go` — markdown
  rendering + word wrap
- `thinking.go`, `thinking_test.go` — in-flight indicator
  (resolves [task #74])
- `messages.go`, `messages_history.go`, `messages_history_test.go`
  — chat history scroll + history navigation
- `branding.go`, `branding_test.go`, `styles.go` — visual identity
- `files.go`, `files_test.go` — file-reference (`@path`) handling
- `view.go`, `update.go`, `update_mouse_test.go` — main bubble-tea
  loop + mouse handling
- `prompter.go` — prompt entry textarea
- `model_picker.go` — model-selector UI
- `permissions_picker.go`, `permissions_picker_test.go`,
  `permissions_slash_test.go` — gate UI
- `elicit.go`, `elicitor.go`, `elicit_test.go`, `elicitor_test.go`
  — MCP elicit-request bridge (the `ask_user`-shaped interactive
  prompt surface)
- `commands.go`, `commands_test.go` — slash-command dispatch
- `program.go`, `program_test.go` — `tea.NewProgram` plumbing

**Adapted at the boundary** (rewrites in two files, ~200–400 LoC):

- `model.go` — replace cogo's `internal/agent.Agent` references
  with `core-agent`'s `agent.Agent`; same surface (`Run`, `Inject`,
  `Interrupt`, `WakeRequested`, `Tools`, etc.), just different
  import path. Permissions, MCP, skills, memory, config, usage —
  same swap, same shape.
- `agentcmd.go` — already shaped as `startAgentTurn(ctx, send,
  a, prompt)` → goroutine → `tea.Msg`. Re-point the agent type.

**Dropped** (cogo has them but core-agent doesn't need them for v2):

- `initcmd/wizard.go` lives outside the TUI package; cogo's init
  flow isn't core-agent's. Skip.

**Kept from our current build, transplanted into the lifted code:**

- Welcome landing screen (`cmd/core-agent-tui/welcome.go`) — cogo
  drops the user straight into chat; we want the welcome surface
  for `core-agent` invocations where multiple endpoints / spawn
  options exist. Keep as the entry view; chat is what it
  transitions into.
- `/attach`, `/spawn`, `/welcome`, `/interrupt`, `/sessions`,
  `/wake` slash commands — wire into cogo's `commands.go`
  dispatcher.
- Attach-mode integration in the remote-only `core-agent-tui`
  binary — unchanged.

**Decided to keep**:

- Queue panel. Originally built to surface SSE round-trip latency,
  but its value isn't only "show the round trip" — it's also
  "show the operator's typed-but-not-yet-sent backlog." Operators
  want to queue the next prompt while the current turn is still
  running; the queue panel makes that backlog visible so they
  don't lose track of what they've already enqueued. The thinking
  indicator complements the queue panel (one shows "agent is
  busy", the other shows "here's what's lined up next"), they
  don't replace each other.

## Wiring sketch (no code)

Three goroutines, same as the lifted cogo shape:

1. **Agent goroutine** — `a.Run(ctx, prompt)` driven by a wake/inbox
   loop, same as today's `--no-repl` mode. Emits events to the
   eventlog as it goes.
2. **Event-pump goroutine** (cogo's `startAgentTurn`) — consumes
   the `iter.Seq2[*session.Event, error]` from `a.Run`, converts
   each event into a `tea.Msg`, and sends it into the bubble-tea
   program via `p.Send`. No SSE, no JSON marshalling.
3. **Bubble-tea Update loop** — drives the lifted chat models.
   Slash commands talk directly to the agent (`/interrupt` →
   `a.Interrupt`, `/wake` → `a.RequestWake`); no HTTP indirection.

What disappears: `attachclient.Stream`, SSE framing/parsing, bearer
tokens, the broadcaster pool, the spawn-and-poll dance, the
`/tmp/<sock>.log` diagnostic hop. They all still exist for the
*remote* `core-agent-tui` binary — just not for local use.

## Migration path: A now, C eventually

Three shapes were considered for how core-agent and cogo share the
TUI code over time:

- **A. Copy-and-rehost.** Lift cogo's `internal/tui/` into
  core-agent, adapt the two glue files. cogo keeps its own copy;
  future syncs are manual. **This is the path v2 takes.**
- **B. Extract to a shared `agent-tui` module.** Both repos
  consume. Requires parameterizing over an agent interface. Adds
  a third repo to coordinate.
- **C. Cogo depends on core-agent's TUI.** Flips the dependency:
  core-agent becomes the library + TUI source, cogo's TUI shrinks
  to a thin wrapper. Completes the extraction pattern that
  produced core-agent in the first place.

A is the unblock; C is the architectural endgame. B is the wrong
middle if the trajectory is C — it adds a coordination repo rather
than collapsing duplication. C is a separate, larger reorg with
its own design doc; do not block v2 on it.

The expected lifecycle:

1. **v2 ships** (this doc): lift A, core-agent has its own TUI,
   cogo still has its independent copy. Brief period of two
   parallel TUIs.
2. **Sync window**: bug fixes in either copy get manually
   ported to the other. Bounded — both teams are small and the
   surface is stable.
3. **C lands** (future): cogo's `internal/tui/` is deleted and
   replaced with an import of `github.com/go-steer/core-agent/tui`
   (likely promoted out of `internal/` to a public package as part
   of that work).

## Decisions (settled)

1. **TTY-detect default.** Auto-TUI-on-TTY with `--no-tui` as the
   escape hatch. Flagged loudly in CHANGELOG as a default-behavior
   change. Matches Claude Code's expectation and removes ceremony
   for the common case.
2. **Line-mode REPL kept behind `--repl=stdin`** as a deprecated
   legacy fallback for one minor version; removed later. Gives
   script consumers a migration window without leaving them
   stranded on release day.
3. **Build tags + dual release artifacts.** TUI imports gated by
   `//go:build !no_tui` so `go build -tags no_tui ./cmd/core-agent`
   produces a slim variant (~5 MB smaller). Both variants ship
   every release: the slim variant goes out as a separate binary
   asset (e.g. `core-agent-slim_<os>_<arch>`) and as its own
   distroless container image (e.g. `core-agent-slim:<tag>`), so
   K8s deployments can pull whichever fits without rebuilding.
4. **`core-agent-tui` binary kept** as the remote-only client.
   Distinct use case (workstation UI → remote agent in a pod);
   stops being the recommended default for local use.
5. **Welcome surface conditional.** Welcome stays as the entry
   view only when the operator has multiple plausible targets
   (configured remotes, multiple sessions, etc.). Bare `core-agent`
   with no remotes goes straight to chat against the in-process
   agent — less surprising for the common case.
6. **Queue panel kept** in both in-process and remote modes. It
   surfaces the operator's typed-but-not-yet-sent backlog, not
   just SSE round-trip lag — operators want to queue the next
   prompt while the current turn is still running, and the queue
   panel makes that visible. Pairs with the thinking indicator
   ("agent is busy" + "here's what's lined up").

## Out of scope (defer)

- Multi-tab / multi-agent inside one TUI session (file under
  killer-feature short list; needs its own design).
- IDE embedding (VS Code / JetBrains extensions consuming the
  attach protocol).
- Headless TUI screenshots for CI (snapshot testing of bubble-tea
  views) — cogo's existing `program_test.go` patterns cover the
  unit-level need.
- The cogo→core-agent flip (path C). Separate doc, separate PR
  cycle. v2 does not block on it.
- Replacing the picker — single-session direct-jump remains the
  default for `core-agent` (one agent → one session); picker stays
  the multi-session surface.

## Lift scope (settled)

**Full lift of cogo's `internal/tui/`**, adapted at the boundary.
Inherit cogo's UI roadmap by default; cogo's permissions picker,
model picker, MCP elicit bridge etc. all come over on day one
even where they need light tweaking for core-agent's exact shapes.
Keeps the short-term diff small, keeps the path to C clean, and
avoids per-component "do we lift this yet?" decisions during the
sync window.

[task #74]: ../TODO.md
