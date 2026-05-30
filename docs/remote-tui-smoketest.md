# Remote-TUI smoke test

Manual end-to-end checklist for the remote-TUI-on-core-tui flip
(PRs A1–A3, B, B-cmd, C). Run before announcing the v2.1 train
that contains them.

The remote TUI (`core-agent-tui`) is now a thin shell over
`go-steer/core-tui` driven by an `internal/coretuiremote` adapter.
This document smoke-tests the operator surface against a real
in-process `core-agent --attach-listen` instance to catch
regressions that unit tests (which use a fake SSE server) cannot.

## Prerequisites

- The PR C branch checked out (`feat/retire-internal-tui`). PR C
  is stacked on PR B-cmd which is stacked on the merged #78–#81
  chain, so this single tip carries every change in the train.
- Build both binaries from that one checkout:

  ```bash
  go install ./cmd/core-agent ./cmd/core-agent-tui
  ```

- A model provider credential — Gemini API key, Vertex creds, or
  Anthropic key. Set the relevant env var (`GEMINI_API_KEY`,
  `ANTHROPIC_API_KEY`, etc.).
- Two terminal windows.

## Run the agent (terminal 1)

Run from a dedicated scratch directory so the agent doesn't pick
up an unrelated project's `.agents/config.json` via the upward
walk:

```bash
mkdir -p /tmp/coreagent-smoke && cd /tmp/coreagent-smoke
```

Provider config comes from the env var you exported in
"Prerequisites" (`GEMINI_API_KEY` / `ANTHROPIC_API_KEY` / etc.) —
no `.agents/` is required for the smoke. If you want to test
project-local config too, drop a minimal `.agents/config.json`
in this scratch dir.

Then start the agent as an attach-only daemon (stays alive until
SIGTERM / SIGINT):

```bash
core-agent \
  --attach-listen=:7777 \
  --session-db \
  --session-db-path=/tmp/coreagent-smoketest.db \
  --agentic-tools \
  --agentic-small-model=gemini-2.5-flash \
  --no-repl
```

Notes:
- `--attach-listen=:7777` opens the HTTP+SSE attach server on
  localhost port 7777.
- `--session-db` is **required** for attach mode (live-tail needs
  the eventlog); `--session-db-path` overrides the default
  `~/.core-agent/sessions.db` so the smoke uses a throwaway file.
- `--agentic-tools` lets the smoke exercise `/context`'s subtask
  reporting; drop if you don't need it.
- `--no-repl` runs the agent as a daemon — no stdin REPL, no
  one-shot exit. The agent registers its session and idles
  waiting for attach traffic. Ctrl-C / SIGTERM ends it.

Watch stderr for the registration line — note the session ID:

```
registered session core-agent/<sid>
```

You'll need `<sid>` for the direct-jump form in terminal 2 (the
picker form discovers it automatically).

### What's the daemon actually doing?

Nothing yet. `--no-repl` with no `-p` means the agent is **idle**
— registered with the attach server, waiting for someone to
inject a prompt. Terminal 1 will look silent (only stderr lines
on startup + errors); the eventlog is empty until you drive a
turn from the remote TUI in terminal 2.

This is the intended shape for headless daemons (K8s pods,
background workers). The remote TUI **is** the operator surface
— without it you'd be reduced to `sqlite3 <session-db>` queries
or `core-agent attach <url>` for a CLI live-tail.

Two other shapes worth knowing about (not what the smoke covers):

- **Local TUI + attach in parallel**: drop `--no-repl`. On a TTY
  the binary lands in the in-process TUI; attach still works
  alongside so remote observers can watch.
- **Autonomous goal-pursuing worker**: uses the library API
  `agent.RunAutonomous` from a small Go binary (no CLI form).
  Combine with `--attach-listen` and the remote TUI to watch the
  autonomous loop progress. See `examples/autonomous/`.

## Attach (terminal 2)

`core-agent-tui` doesn't walk for `.agents/` — cwd doesn't matter.
Run from anywhere convenient (your home dir, the scratch from
terminal 1, etc.):

```bash
core-agent-tui http://localhost:7777
```

The picker should render the registered session (`core-agent/<sid>`).
Press Enter to attach. The chat view should appear empty (no
prior history — the agent started fresh).

Type a first prompt now (e.g. "say hello and list your tools") to
seed the session with some history; many checklist items below
assume non-zero usage / context state.

Direct-jump form (skip picker):

```bash
core-agent-tui http://localhost:7777/sessions/<sid>
```

Bare invocation (stdin prompt):

```bash
core-agent-tui
# prompt: attach URL (e.g. http://localhost:7777 or ...):
```

## Surface checklist

For each item, perform the action in the remote TUI and verify the
expected behavior. **Mark each** as `[x] pass` / `[ ] fail`.
Failures should be filed as issues against the appropriate PR
branch.

### Connection + chat

- [x] Picker displays the registered session with the right
      `app/sid` label.
- [x] After picker → chat, the scrollback is empty (no prior
      history because the daemon started fresh).
- [x] Typing a seed prompt and pressing Enter submits it; the
      model's response streams back in real time (chunk-by-chunk,
      not a single final blob).
- [ ] The status header shows the model name and "idle" → "running"
      transitions during a turn.
- [ ] Ctrl+C cleanly exits without leaving the listener (terminal
      1) in a bad state.

### Operator-state slashes (PR A1)

- [ ] `/stats` shows the session's cumulative input + output
      tokens + cost; non-zero after the seed turn.
- [ ] `/context` shows compaction / checkpoint / subtask counts;
      non-zero subtask turn count if `--agentic-tools` is on and
      you've issued a prompt that triggers an `agentic_*` wrapper.
- [ ] `/memory` lists any loaded `AGENTS.md` files.
- [ ] `/skills` lists registered skills (empty if no `.agents/skills/`).
- [ ] `/mcp` lists declared MCP servers (empty if no `.agents/mcp.json`).
- [ ] `/pricing` shows the current pricing snapshot — source,
      known model count, and the current model's rate.

### Mutation slashes (PR A2)

- [ ] `/perms` shows the gate state (mode + allow + deny patterns).
- [ ] `/allow tool.bash` adds the pattern; `/perms` reflects it on
      the next call.
- [ ] `/deny tool.delete_file` adds the deny pattern.
- [ ] `/pricing refresh` triggers a LiteLLM fetch and reports the
      outcome (updated / unchanged + model count).
- [ ] `/pricing set claude-opus-4-7 15 75` applies a manual rate;
      subsequent `/pricing` shows the new rate.
- [ ] `/reload` re-walks memory + skills + MCP; per-surface
      success flags surface in the result.

### Async slashes (PR A3)

- [ ] `/btw what tools do you have?` opens a modal with the
      agent's answer; the persistent chat scrollback is
      **unchanged** (no pollution).
- [ ] `/compact` writes a compaction summary; a preamble row
      ("Compacting context…") appears at dispatch; the post-summary
      `/context` shows one additional compaction.
- [ ] `/done shipped the smoke test` writes a checkpoint; preamble
      row appears; post-checkpoint `/context` shows the task note +
      one additional checkpoint.
- [ ] `/subagent watcher watch the disk for a while` spawns a
      background subagent; subagent's events flow into the chat
      under a branch label; `/subagents` lists the spawn.

### Built-in slashes (core-tui)

- [ ] `/help` lists every registered slash, including the eight
      adapter-provided ones above.
- [ ] `/tools` lists the agent's tool catalog (read_file, bash,
      etc.) with gate states.
- [ ] `/subagents` lists running subagents (matches what you
      spawned via `/subagent`).
- [ ] `/theme dark` and `/theme light` flip the palette
      immediately.
- [ ] `/quit` cleanly exits the remote TUI without leaving the
      listener in a bad state.

### Mid-turn injection

- [ ] Submit a prompt that triggers a multi-second response.
      While it's streaming, press Enter on the empty input — the
      operator inbox queue panel should show the queued state.
- [ ] After the turn completes, the queued message auto-submits
      as the next turn (with the auto-continue ↻ glyph if
      core-tui's auto-continue mode is the default).
- [ ] Esc during a streaming turn cancels it; the chat shows
      "turn cancelled".

### Resilience

- [ ] Kill terminal 1 (`pkill -9 core-agent`); the remote TUI
      should surface a connection-lost message and let the
      operator restart.
- [ ] Restart terminal 1's agent + re-attach; the chat shows the
      preserved session history from the eventlog.

## Failure modes worth verifying

- **Status banner stuck at `0 in · 0 out · $0.0000`** (observed
  during the v2.1 smoke). The banner pulls from
  `UsageTracker.SessionTotals / SessionCostUSD`, which the
  adapter projects from the `/usage` attach endpoint, which
  returns `agent.AttachUsage()` from a wired `*usage.Tracker`.
  Zeros suggest either (a) `--no-repl + --attach-listen` mode
  doesn't wire the tracker into the agent, or (b) the tracker
  isn't being appended for inject-driven turns (only `Run`-driven
  ones). Diagnose by querying the `/usage` endpoint directly
  (`curl http://localhost:7777/sessions/<sid>/usage`) — non-zero
  means the adapter is dropping it; zero means the tracker is the
  source of truth and isn't getting writes. Per-turn footer in
  the chat (`in · out · Xs`) is unaffected — that comes from
  per-event `UsageMetadata`, not the cumulative tracker.

- **Stuck on "Compacting context…" preamble**: `/compact` started
  but the response never came back. Check terminal 1 for a
  summarizer error; check the eventlog (`.db` file) for the
  expected `CustomMetadata["compaction"]` event.
- **`/subagent` returns 501**: the in-process agent wasn't
  constructed with `WithBackgroundManager`. The bundled CLI wires
  this by default; library callers may need to add it.
- **`/elicit` (MCP server requests user input)**: not yet wired
  on the remote TUI (deferred from PR A3). MCP-originated elicits
  decline server-side. Verify by attaching a `--mcp-servers` set
  that includes a server known to elicit (e.g., interactive
  approval flows) — you should see the decline, not a hang.

## Known limitations

- **Per-tool permission prompts don't reach the remote TUI**
  (tracked as PR D — HTTP-driven Prompter). Without it,
  `permissions.mode = "ask"` is effectively unusable for a
  headless attach-driven daemon: the agent's permission gate
  asks for approval, no operator is at the local stdin to
  answer, the gate has no HTTP escape hatch, and the tool call
  fails closed with `ErrNoPrompter`.

  **Operator workaround for now:** pre-grant the tools the
  daemon will need via `--allow tool.<name>` flags or via
  `.agents/config.json`'s `permissions.allow` list, and use
  `permissions.mode = "yolo"` for anything more permissive. The
  `/perms`, `/allow`, `/deny` slashes (PR A2) work over attach,
  so an operator attached to a running daemon CAN mutate the
  gate's pattern list live — they just can't be prompted
  per-call.

  Same HTTP-driven Prompter infra unblocks `ask_user` round-trips
  (PR A3's deferred `/elicit/respond`). Both share the pending-
  request map and the SSE-push delivery shape.

## When to update this doc

- New attach endpoints land → add a checklist row.
- New remote-TUI slash lands → add a checklist row.
- core-tui releases a new version with capability changes → re-run
  the whole checklist against the bumped binary.
