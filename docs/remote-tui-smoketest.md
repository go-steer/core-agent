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
- [X] The status header shows the model name and "idle" → "running"
      transitions during a turn.
- [X] Ctrl+C cleanly exits without leaving the listener (terminal
      1) in a bad state.

### Operator-state slashes (PR A1)

- [x] `/stats` shows the session's cumulative input + output
      tokens + cost; non-zero after the seed turn.
- [x] `/context` shows compaction / checkpoint / subtask counts;
      non-zero subtask turn count if `--agentic-tools` is on and
      you've issued a prompt that triggers an `agentic_*` wrapper.
- [ ] `/memory` lists any loaded `AGENTS.md` files.
- [x] `/skills` lists registered skills (empty if no `.agents/skills/`).
- [x] `/mcp` lists declared MCP servers (empty if no `.agents/mcp.json`).
- [ ] `/pricing` shows the current pricing snapshot — source,
      known model count, and the current model's rate.

### Mutation slashes (PR A2)

- [x] `/perms` shows the gate state (mode + allow + deny patterns).
      `/permissions` (core-tui builtin) shows the per-session
      approval log including tool + key + decision.
- [ ] `/allow tool.bash` adds the pattern; `/perms` reflects it on
      the next call.
- [ ] `/deny tool.delete_file` adds the deny pattern.
- [ ] `/pricing refresh` triggers a LiteLLM fetch and reports the
      outcome (updated / unchanged + model count).
- [ ] `/pricing set claude-opus-4-7 15 75` applies a manual rate;
      subsequent `/pricing` shows the new rate.te
- [ ] `/reload` re-walks memory + skills + MCP; per-surface
      success flags surface in the result.

### Async slashes (PR A3)

- [x] `/btw what tools do you have?` opens a modal with the
      agent's answer; the persistent chat scrollback is
      **unchanged** (no pollution).
- [x] `/compact` writes a compaction summary; a preamble row
      ("Compacting context…") appears at dispatch; the post-summary
      `/context` shows one additional compaction.
- [x] `/done shipped the smoke test` writes a checkpoint; preamble
      row appears; post-checkpoint `/context` shows the task note +
      one additional checkpoint.
- [x] `/subagent watcher watch the disk for a while` spawns a
      background subagent; `/subagents` lists the spawn and shows
      status updates including the final report.
      *Note: subagent events do NOT stream into the main chat
      scrollback live — that's deferred to PR E (Pattern B
      observer mode). Poll `/subagents` for status + the trailing
      report.*

### Built-in slashes (core-tui)

- [x] `/help` lists every registered slash, including the eight
      adapter-provided ones above.
- [x] `/tools` lists the agent's tool catalog (read_file, bash,
      etc.) with gate states.
- [x] `/subagents` lists running subagents (matches what you
      spawned via `/subagent`); shows status updates as the
      subagent progresses + the final report.
- [x] `/quit` cleanly exits the remote TUI without leaving the
      listener in a bad state.

  > **Not supported in this train:** `/theme` (runtime theme swap).
  > core-tui v0.6.3 doesn't ship a `/theme` builtin and the adapter
  > can't register one usefully since the theme is set at
  > `coretui.Options.ForceTheme` startup with no runtime swap API.
  > Pass `core-agent-tui --theme=dark|light` at launch instead.
  > Tracked upstream as
  > [go-steer/core-tui#21](https://github.com/go-steer/core-tui/issues/21).

### Mid-turn injection

- [x] Submit a prompt that triggers a multi-second response.
      While it's streaming, type another prompt and hit Enter —
      the operator inbox queue panel shows the queued state
      (`○ <text>` in the queue panel).
- [ ] After the turn completes, the queued message auto-submits
      as the next turn (with the auto-continue ↻ glyph if
      core-tui's auto-continue mode is the default).
      *Known-broken in v2.1; see "Mid-turn queued prompts
      auto-drain incorrectly" under Known Limitations.*
- [ ] Esc during a streaming turn cancels it; the chat shows
      "turn cancelled".

### Resilience

- [ ] Kill terminal 1 (`pkill -9 core-agent`); the remote TUI
      should surface a connection-lost message and let the
      operator restart.

      There was no session lost; when trying to run a command it does show a connection refused.
      Note:  the pkill command kills both process as they both match "core-agent"

- [ ] Restart terminal 1's agent + re-attach; the chat shows the
      preserved session history from the eventlog.

      ❯ what's kubernetes?

⚠  stream: Get "http://localhost:7777/sessions/core-agent/default/events": dial tcp 127.0.0.1:7777: connect:
connection refused
─────────────────────────────────────────────────────────────────────────────────────────────────────────────────────

❯ what's kubernetes?


  I have the context from the previous task. I know we identified 12 GKE clusters in the  gke-demos-345619  project
  (located in  us-central1  and  us-central1-a ) and that we'll need to fetch credentials before interacting with
  any specific cluster's workloads.

  What would you like to do next?

└ 3.3K in · 82 out · 4.5s
─────────────────────────────────────────────────────────────────────────────────────────────────────────────────────

❯ which clsuters are autopilot?

⚠  stream: Get "http://localhost:7777/sessions/core-agent/default/events?since=10": dial tcp 127.0.0.1:7777: connect:
connection refused
─────────────────────────────────────────────────────────────────────────────────────────────────────────────────────

❯ try again


  Kubernetes is an open-source container orchestration system that automates the deployment, scaling, and
  management of containerized applications. Originally designed by Google and now maintained by the Cloud Native
  Computing Foundation (CNCF), it helps you manage applications that are made up of hundreds or thousands of
  containers, ensuring they run reliably and scale efficiently across clusters of servers.

  In the context of the work we just did, Google Kubernetes Engine (GKE) is Google Cloud's managed service for     
  running Kubernetes, which handles much of the underlying infrastructure and control plane management for you.    

  Let me know if you'd like to dive deeper into any specific Kubernetes concepts or start interacting with any of  
  the clusters we found!

└ 3.4K in · 141 out · 4.4s

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

- **Multiple `core-agent` daemons accidentally running** (the
  gotcha that caused half of the v2.1 smoke confusion). If a
  second `core-agent --attach-listen=:7777 ...` is launched
  while one is already bound, port-bind silently fails and the
  second process falls through to REPL/TUI mode without
  warning. Operator's TUI keeps talking to the ORIGINAL daemon
  on :7777 — which may have stale session state or a pre-fix
  binary — making the responses look like adapter bugs.
  Diagnose: `ps aux | grep core-agent | grep -v grep`. Hard
  reset: `pkill -f "core-agent --attach-listen"`. Tracked as
  Task #5 (silent port-bind degradation → fix is fatal error
  on `Listen`).

## Known limitations

- **No operator path to reset / fork a session** (Task #4).
  Persistent `--session-db` + the hardcoded `defaultSessionID =
  "default"` means every daemon restart reloads the FULL prior
  conversation as context. After many turns the model sees its
  own last "Final Summary" as the latest message and starts
  responding with continuation/summary text instead of fresh
  answers to new prompts. Symptom looks like "the chat is
  confused" or "every new prompt produces a wrap-up summary."
  This is not a smoke-setup issue — it's a real operator gap
  that production users will hit on long-lived daemons.

  **Workarounds today**:
  - `/compact` aggressively (lowers context pressure but keeps
    the running summary attached)
  - `/done <task-note>` between logical tasks (the checkpoint
    slices prior task history out of future requests)
  - Restart the daemon with a different session-id (CLI doesn't
    currently expose this for `--no-repl`, so you'd hardcode in
    `cfg` or use the library API)
  - Last resort for smoke runs: `rm <session-db-path>` and
    restart (NOT a production fix)

  Real fix: an operator-facing `/reset` or `/new-session` slash
  + optional `--session-id=<name>` CLI flag for `--no-repl`
  daemons. Tracked as Task #4.

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

- **Observer mode (Pattern B) isn't supported yet** (tracked as
  PR E — coretui LiveAgent capability, v2.2 target). v2.1's
  remote TUI is shaped for Pattern A: the operator drives turns
  one inject at a time. If the remote agent is running
  autonomously (`agent.RunAutonomous`, scheduled background
  subagents, MCP-triggered activity, other clients' injects),
  the operator won't see those events in the chat scrollback —
  `coretui.Agent.Run`'s per-turn iterator filters them out by
  design.

  **Workarounds for observing autonomous workers in v2.1:**
  - `core-agent attach http://host:7777/sessions/<sid>` —
    line-mode CLI live-tail (no rich UI but shows every event)
  - Direct sqlite queries against the session DB
  - On-disk eventlog replay tools if you've shipped any

  Design doc for the fix: `docs/remote-tui-observer-mode.md`.
  Targets v2.2 because it requires an upstream change to
  `go-steer/core-tui` to add a `LiveAgent` capability interface
  next to the existing `Agent` (per-turn) interface.

- **Reattach drops prior history** (related to PR E). On
  reconnect the chat scrollback starts empty even though the
  session has history in the eventlog. Required for clean
  per-turn correlation in v2.1's iterator model; LiveAgent
  (PR E) naturally fixes this since the per-turn filter
  disappears.

- **Mid-turn queued prompts auto-drain incorrectly** (also
  PR E). When the operator types a second prompt while the first
  is still streaming, coretui's queue panel correctly shows the
  pending entry — but on turn-end, the auto-drain's new Run()
  picks up the prior turn's tail events as if they were the new
  turn's response. Visible symptom: prompts and responses become
  offset by one (operator types A, sees prior-prompt's response;
  types B, sees A's response). Workaround: **don't type a second
  prompt until the first turn fully renders.** Root cause: the
  v2.1 adapter has no request_id correlation, so Turn N's
  iterator can't distinguish its own events from Turn N-1's tail.
  Fix lands with PR E (`docs/remote-tui-observer-mode.md`) when
  request_id correlation goes in alongside LiveAgent.

- **Background subagent output doesn't stream into the main chat**
  (also PR E). `/subagent` spawns successfully; `/subagents`
  polling shows status updates including the subagent's final
  report. But subagent events flow under a branch label in the
  eventlog while the parent's Run iterator filters to "current
  turn's events only" — so the subagent's mid-execution output
  never reaches the operator's chat scrollback in real time.
  Workaround: poll `/subagents` to see status + the trailing
  report. Live in-chat streaming of subagent activity is a
  Pattern B feature (LiveAgent in PR E).

## When to update this doc

- New attach endpoints land → add a checklist row.
- New remote-TUI slash lands → add a checklist row.
- core-tui releases a new version with capability changes → re-run
  the whole checklist against the bumped binary.
