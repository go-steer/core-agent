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

```bash
core-agent \
  --attach-listen=:7777 \
  --session-db=/tmp/coreagent-smoketest.db \
  --agentic-tools \
  --agentic-small-model=gemini-2.5-flash \
  -p "ready"
```

Notes:
- `--attach-listen=:7777` opens the HTTP+SSE attach server on
  localhost port 7777.
- `--session-db` is **required** for attach mode (live-tail needs
  the eventlog).
- `--agentic-tools` lets the smoke exercise `/context`'s subtask
  reporting; drop if you don't need it.
- `-p "ready"` runs a single bootstrap prompt so the session is
  registered and there's some history to view. Replace with the
  REPL (drop `-p`) if you want to type the bootstrap prompt
  yourself.

After the prompt completes, the binary stays alive listening on
:7777. Note the session ID from stderr (`registered session
core-agent/<sid>`).

## Attach (terminal 2)

```bash
core-agent-tui http://localhost:7777
```

The picker should render the registered session (`core-agent/<sid>`).
Press Enter to attach. The chat view should appear with the
bootstrap prompt + agent's response already in scrollback.

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

- [ ] Picker displays the registered session with the right
      `app/sid` label.
- [ ] After picker → chat, the bootstrap prompt and the agent's
      response both render in the scrollback.
- [ ] Typing a prompt and pressing Enter submits it; the model's
      response streams back in real time (chunk-by-chunk, not a
      single final blob).
- [ ] The status header shows the model name and "idle" → "running"
      transitions during a turn.
- [ ] Ctrl+C cleanly exits without leaving the listener (terminal
      1) in a bad state.

### Operator-state slashes (PR A1)

- [ ] `/stats` shows the session's cumulative input + output
      tokens + cost; non-zero after the bootstrap turn.
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

## When to update this doc

- New attach endpoints land → add a checklist row.
- New remote-TUI slash lands → add a checklist row.
- core-tui releases a new version with capability changes → re-run
  the whole checklist against the bumped binary.
