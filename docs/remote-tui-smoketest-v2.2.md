# Remote-TUI v2.2 smoke test

Manual UAT for the four v2.2 PRs stacked on `main`:

| PR | Branch | What it fixes |
|----|--------|---------------|
| [#85](https://github.com/go-steer/core-agent/pull/85) | `feat/attach-listen-bind-fail-fast` | `--attach-listen` silently degrading to REPL when the port is taken |
| [#86](https://github.com/go-steer/core-agent/pull/86) | `feat/reload-server-action` | `/reload` only refreshing the client display; server-side 501 |
| [#87](https://github.com/go-steer/core-agent/pull/87) | `feat/attach-prompter` | Tool-approval modals never reaching the remote operator |
| [#88](https://github.com/go-steer/core-agent/pull/88) | `feat/observer-mode-liveagent` | Remote TUI blank while the daemon does autonomous work |

Each section starts with **the actual incident or gap that drove
the PR**, then walks an operator-visible reproducer. If the
reproducer behaves as described, the fix landed.

## Prerequisites

- The tip of the v2.2 stack checked out (`feat/observer-mode-liveagent` carries the full stack).
- Rebuilt binaries:

  ```bash
  go install ./cmd/core-agent ./cmd/core-agent-tui
  ```

- A model provider credential (`GEMINI_API_KEY` / `ANTHROPIC_API_KEY` / Vertex creds).
- A scratch dir so the daemon doesn't inherit an unrelated project's `.agents/`:

  ```bash
  mkdir -p /tmp/coreagent-v22-smoke && cd /tmp/coreagent-v22-smoke
  ```

- Three terminal windows: one for the daemon, one for the remote TUI, one for ad-hoc poking.

---

## §1 — `--attach-listen` fails loudly on bind failure (PR #85)

### Why we built it

During v2.1 smoke I spent a multi-hour debug session chasing what looked like adapter bugs — `/stats` showing zeros, weird Ruby code instead of math answers, etc. Root cause was much sillier: **two `core-agent --attach-listen=:7777` daemons were running**. The second one's bind silently failed, fell through to REPL mode, and exited (the prompt was gone in the noise). The first daemon — which actually had the port — was running an OLD binary from before my fixes. The TUI happily attached to it.

The fix: when the operator says `--attach-listen`, treat any bind failure as fatal. Don't degrade. Don't fall through. Exit non-zero with a clear error so the operator immediately sees what went wrong.

### Reproducer

**Terminal 1** — start the first daemon and leave it running:

```bash
core-agent \
  --attach-listen=:7777 \
  --session-db --session-db-path=/tmp/v22-smoke-1.db \
  --no-repl
```

Expect stderr:

```
core-agent: attach listener on :7777
core-agent: session db: /tmp/v22-smoke-1.db
```

**Terminal 2** — try to start a SECOND daemon on the same port:

```bash
core-agent \
  --attach-listen=:7777 \
  --session-db --session-db-path=/tmp/v22-smoke-2.db \
  --no-repl
```

**Expected behavior (the fix):**

- Exits within ~1 second with a non-zero code (`echo $?` → `2`).
- stderr contains the bind error clearly identified as an attach listener problem:

  ```
  core-agent: attach listener: attach: listen tcp ":7777": ... address already in use
  ```

**What WOULD have happened before the fix:**

- No error. The daemon would silently continue, fall through to REPL mode (or whatever the terminal looked like), and the operator would never know the port-bind failed. Any TUI connecting to `localhost:7777` would silently route to the first daemon.

### Pass criteria

- [x] Second daemon exits non-zero.
- [x] Error message names `attach listener` (not a generic Go bind error).
- [x] First daemon is unaffected (`curl http://localhost:7777/sessions` still works).

Kill terminal 1's daemon before moving on.

---

## §2 — `/reload` actually does something server-side (PR #86)

### Why we built it

v2.1's `/reload` was a fiction. The remote TUI refreshed its display of memory/skills/MCP after the slash, but the server-side action returned 501. So if an operator edited `AGENTS.md` and ran `/reload`, the daemon did *nothing* — but the TUI happily reported success-by-omission. There was no feedback loop for "did my edit parse cleanly?"

The fix: an `agent.WithAttachReloader` closure that actually re-walks `instruction.Load`, `skills.LoadAll`, and `mcp.Load` (config parse only), capturing per-surface success/failure into `ReloadResponse.Errors`. Honest scope: agent-internal state (system prompt, MCP server lifecycle) still needs a daemon restart — that's noted in the response.

### Reproducer

**Terminal 1** — start a clean daemon in the scratch dir:

```bash
cd /tmp/coreagent-v22-smoke
core-agent \
  --attach-listen=:7777 \
  --session-db --session-db-path=/tmp/v22-smoke-reload.db \
  --no-repl
```

**Terminal 2** — attach the TUI:

```bash
core-agent-tui http://localhost:7777
```

#### Sub-test 2a: happy path (memory + skills re-read OK)

In the scratch dir, create `.agents/AGENTS.md` with anything:

```bash
mkdir -p .agents && echo "# Test memory" > .agents/AGENTS.md
```

In the TUI, type `/reload`.

**Expected:** a system note row in the chat like:

```
reload: memory ✓ · skills ✓ · mcp ⚠ live restart not supported · errors: mcp: live server restart requires daemon restart (tracked for v2.3)
```

The `memory ✓` and `skills ✓` parts confirm the re-read succeeded. The mcp warning is honest scoping (NOT a regression).

#### Sub-test 2b: broken file → operator sees the error

Now break the mcp config:

```bash
echo "this is not json" > .agents/mcp.json
```

In the TUI, type `/reload` again.

**Expected:** the system note includes the parse error explicitly:

```
reload: memory ✓ · skills ✓ · mcp ⚠ live restart not supported · errors: mcp config: ...invalid character ...; mcp: live server restart requires daemon restart (tracked for v2.3)
```

The operator can now SEE that their `mcp.json` edit broke the config — without that feedback, the agent would silently behave the old way until next restart.

#### Sub-test 2c: in-process TUI's `/reload` works the same

Stop the daemon (Ctrl-C in terminal 1). Remove the broken file:

```bash
rm .agents/mcp.json
```

Start a TTY core-agent (in-process TUI) instead of the attach daemon:

```bash
core-agent  # bare invocation → in-process TUI
```

Type `/reload` in the in-process TUI.

**Expected:** the same `reload: memory ✓ · skills ✓ ...` note appears, with the same shape. Both surfaces share `agent.AttachReload` — they should produce identical output. (Before this PR, the in-process TUI's `/reload` printed "/reload not yet wired into the core-tui adapter".)

### Pass criteria

- [x] Happy path shows `memory ✓` and `skills ✓` in the system note.
- [x] Broken `mcp.json` surfaces the parse error inline.
- [x] In-process TUI's `/reload` returns the same shape as the remote TUI's.

Kill the daemon before moving on.

---

## §3 — HTTP-driven permission prompts (PR #87)

> 🚫 **Known issue blocking successful e2e — [core-tui#24](https://github.com/go-steer/core-tui/issues/24).**
> The wire protocol works (decisions round-trip correctly once the modal renders), but the modal does **not paint** until the operator presses a key after the tool call is gated. The agent appears frozen on the in-chat tool-call preamble (`▶ list_dir /tmp`) until any keypress wakes the bubble-tea v2 render scheduler. Until upstream lands a fix, §3 is **not a successful e2e** — the UX is broken for any operator running a daemon in `ask` mode (the safe default).
>
> **Workaround during smoke:** when the agent freezes on the preamble, type any character to wake the modal, then make your decision normally.

### Why we built it

Before this PR, remote attach was effectively read-only for any agent in `ask` mode (the safe default). The daemon's `permissions.Gate` would generate a prompt; with no in-process TUI prompter and no remote-bridge, the gate had nowhere to send it. The tool call would fail with `ErrNoPrompter`. Operators had three bad choices:
1. Run with `--yolo` (zero gating — unsafe for any non-toy agent)
2. Pre-populate `.agents/config.json` with every tool the agent might want (annoying, easy to miss something)
3. Use the in-process TUI instead (defeats the purpose of attach mode)

The fix: `attach.PromptBroker` bridges the gate to a `GET /sessions/<sid>/perms/stream` SSE feed; operator decisions come back via `POST /perms/respond`. The remote TUI's `coretuiremote.StartRemotePrompter` runs the bridge goroutine that pushes each frame into a `coretui.Prompter` modal and POSTs the chosen decision back.

### Reproducer

**Terminal 1** — start the daemon in the default `ask` mode (don't pass `--yolo`):

```bash
cd /tmp/coreagent-v22-smoke
rm -rf .agents  # ensure no pre-existing allow list
core-agent \
  --attach-listen=:7777 \
  --session-db --session-db-path=/tmp/v22-smoke-prompts.db \
  --no-repl
```

**Terminal 2** — attach the TUI:

```bash
core-agent-tui http://localhost:7777
```

#### Sub-test 3a: bash prompt round-trips

In the TUI, type:

```
run `ls /tmp` and tell me how many entries
```

**Expected (once [core-tui#24](https://github.com/go-steer/core-tui/issues/24) lands):**

1. The agent will want to run a `bash` tool call. **The permission modal should appear in the remote TUI** with the command and the standard set of options (`y` allow once, `s` allow session, `v` allow verb, `t` allow tool, `a` allow always, `n` deny).
2. Pick `s` (allow-session).
3. The bash command runs; the agent's response includes the directory count.

**What actually happens today** (per core-tui#24): the in-chat preamble row shows `▶ list_dir /tmp` and the TUI freezes there. Press any key — the modal pops up. Decide normally. The agent's response then streams in. The system row after the response confirms the decision round-tripped: `ℹ Permission allow-session: list_dir — read /tmp (out of scope)`.

**Before this PR**, the modal would never appear — the tool call would fail with a permission error and the agent would either error out or fall back to a non-shell answer.

#### Sub-test 3b: deny round-trips

Type another prompt that needs a tool:

```
read /etc/hostname for me
```

When the modal appears, pick `n` (deny).

**Expected:** the daemon's gate returns a denial; the agent reports the tool wasn't allowed. The TUI doesn't hang.

#### Sub-test 3c: prompt arrived while operator was away

Kill terminal 2 (`q` quit) so the daemon is running without an attached TUI. From terminal 3, fire an inject directly:

```bash
curl -X POST http://localhost:7777/sessions/<sid>/inject \
  -H 'Content-Type: application/json' \
  -d '{"message":"run `date`"}'
```

(Use the SID from the daemon's startup log, or just hit `/sessions` first to find it.)

The daemon's gate will block on `AskApproval`. Now reattach:

```bash
core-agent-tui http://localhost:7777
```

**Expected:** the pending prompt fires the modal **as soon as you attach** (late-subscriber snapshot delivers any pending prompts). Decide on it; the original `curl` doesn't time out, and the agent's response flows through normally.

#### Sub-test 3d: 501 when daemon has no broker

To exercise the "graceful degrade" path, start a daemon WITHOUT `--attach-listen` (which is what wires the broker — but that means no attach at all). Easier path: there is no easy way to disable the broker independently; this sub-test is "the code path exists" and is covered by the unit test (`TestIntegration_PromptEndpoints_501WhenNoBroker`). Skip the manual case.

### Pass criteria

- [ ] First bash command triggers the modal in the remote TUI **without requiring a keypress** — blocked by [core-tui#24](https://github.com/go-steer/core-tui/issues/24); §3 is NOT a successful e2e until this lands.
- [ ] `allow-session` round-trips; the command actually runs.
- [ ] `deny` round-trips; the agent reports the denial cleanly.
- [ ] Reattaching after a pending prompt drains it (sub-test 3c).

Kill the daemon before moving on.

---

## §4 — Observer mode via `coretui.LiveAgent` (PR #88)

### Why we built it

The user's request, verbatim: *"one of the ideas of the remote terminal is to be able to watch what the remote agent is actually doing? (in a way replacing a built-in internal REPL that has been active)."*

v2.1 was Pattern A only — the remote TUI's per-turn `Run()` iterator only surfaced events triggered by the operator's own injects. If the daemon was doing autonomous work — `RunAutonomous`, scheduled background subagents, MCP-server-triggered activity, OR another attached operator's inject — the operator saw nothing. The TUI looked idle while the agent worked. Same gap surfaced four symptoms:
1. Empty scrollback while autonomous work happened
2. No history visible after attaching (per-turn filter dropped everything older)
3. Mid-turn drain attribution bugs (no request_id correlation needed because of #1, but the lack thereof made things worse)
4. Subagent activity not flowing into chat (only `/subagents` polling could see it)

The fix: `internal/coretuiremote.Adapter` now implements `coretui.LiveAgent.Events(ctx)`. core-tui v0.6.6 prefers `LiveAgent` over `Run` when both are implemented, so all four symptoms are fixed in one shot.

### Reproducer

**Terminal 1** — start the daemon:

```bash
cd /tmp/coreagent-v22-smoke
rm -rf .agents
core-agent \
  --attach-listen=:7777 \
  --session-db --session-db-path=/tmp/v22-smoke-observer.db \
  --no-repl
```

#### Sub-test 4a: operator-driven turn still works

**Terminal 2** — attach:

```bash
core-agent-tui http://localhost:7777
```

Type a prompt:

```
say hello
```

**Expected:** the model responds in the chat as before. (Sanity check that `LiveAgent` taking over from `Run` didn't break the basic case.)

#### Sub-test 4b: foreign inject is visible in the TUI

Without leaving the TUI, from **terminal 3** fire an inject:

```bash
curl -X POST http://localhost:7777/sessions/<sid>/inject \
  -H 'Content-Type: application/json' \
  -d '{"message":"what is 2+2?"}'
```

**Expected:** the prompt + the agent's response appear in the TUI's chat scrollback **automatically**, even though the operator at the TUI never typed it. Before this PR, terminal 2 saw nothing — the inject was triggered by terminal 3, not terminal 2, so terminal 2's per-turn iterator never picked it up.

#### Sub-test 4c: history-on-attach

Kill the TUI (`q` quit). The daemon keeps running with several turns of conversation in the eventlog.

Reattach:

```bash
core-agent-tui http://localhost:7777
```

**Expected:** the chat scrollback shows the **prior turns** (the "say hello" exchange + the "what is 2+2" exchange), not an empty view. Operator can see the context they walked into. Before this PR, the replay filter discarded everything older than `connectedAt - 2s` so the scrollback was always empty on attach.

#### Sub-test 4d: subagent activity flows into chat

In the TUI, type:

```
/subagent investigate
```

(or any spec that spawns a subagent). With `LiveAgent`, the subagent's events flow under their branch labels into the parent's chat scrollback as they happen. Before this PR, `/subagents` polling was the only way to see subagent activity; the parent's `Run` iterator never surfaced them.

**Expected:** subagent's progress messages appear in the TUI chat rather than only being visible via the `/subagents` slash polling.

#### Sub-test 4e: daemon shutdown + restart, TUI auto-reconnects

With the TUI still attached, kill the daemon (Ctrl-C in terminal 1).

**Expected:** the TUI does NOT crash. It surfaces a transient error row in the chat (`stream disconnected: ... (reconnecting in 1s)`) and then keeps trying with exponential backoff (1s → 2s → 4s → ... capped at 30s). No stderr noise bleeds into the rendered chat — the prompt bridge's diagnostics route to `io.Discard` while the TUI owns the alt-screen.

#### Sub-test 4f: daemon restart, TUI resumes without restart

While the TUI is still attached and showing the reconnect error rows, restart the daemon (re-run the command from terminal 1).

**Expected:** within a few seconds the TUI reconnects automatically — the error rows stop, the chat resumes, the operator can type a new prompt and see the agent respond normally. No need to kill `core-agent-tui` and reattach. Reconnect uses the last-seen sequence so old history isn't re-streamed.

### Pass criteria

- [ ] 4a: operator-driven turn still works (no regression).
- [ ] 4b: foreign inject from terminal 3 visible in terminal 2's chat.
- [ ] 4c: history-on-attach shows prior turns instead of empty scrollback.
- [ ] 4d: subagent activity appears in chat (not just via `/subagents` poll).
- [ ] 4e: daemon kill surfaces a transient reconnect error inline; no stderr corruption.
- [ ] 4f: daemon restart → TUI reconnects automatically; chat resumes; no operator restart needed.

---

## Cleanup

```bash
rm -rf /tmp/coreagent-v22-smoke
rm -f /tmp/v22-smoke-*.db
```

## Known limitations after v2.2

- **🚫 Remote permission prompts need a keypress to render** — [core-tui#24](https://github.com/go-steer/core-tui/issues/24). Wire protocol is sound; modal paint stalls until the next external event wakes the bubble-tea v2 scheduler. **Blocks §3 from being a successful e2e.** Workaround: any keypress. Upstream fix required before v2.2 can ship.
- **Multi-session daemon** — `+ New session` picker entry. Task #4, deferred behind a multi-session-daemon design.
- **Request_id correlation + operator-initiated turn glyph** — small follow-up to `LiveAgent`. Tracked in the observer-mode design doc.
- **MCP `ask_user` / elicit round-trips** — reuse the same broker pattern as PR #87. Follow-up.
