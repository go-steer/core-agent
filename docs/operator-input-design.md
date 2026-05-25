# Operator input during turns: queue panel, auto-continue, /btw, /subagent

Design doc for four related TUI features that all answer the same
operator complaint: *"I have something to add to the conversation
but the agent is busy."* The four answers are distinct, well-defined,
and complementary — there isn't one big architectural change here,
just four layers each addressing a different shape of "operator
wants to add input mid-turn."

| | Operator wants | Mechanism |
|---|---|---|
| **A** | To type the next prompt without waiting for the current turn | Input-while-streaming + visible queue panel |
| **B** | Their queued input to actually steer the next step the model takes | Auto-continue from inbox after turn-done, with system-note framing so the model decides relevance |
| **C** | To ask a quick side question (*"what was that file again?"*) without polluting the conversation or interrupting the turn | `/btw` parallel one-shot model call with no tools, dismissible overlay |
| **D** | To spawn a background subagent without asking the main agent to do it | `/subagent <prompt> [flags]` slash command |

`A + B` are tightly coupled (B's "auto-start a new turn from queued
input" is precisely A's "queue panel processes next entry"); they
ship as one PR. `C` and `D` are independent — separate PRs.

## Why this doc exists

PR 2 (`docs/embedded-tui-design-v2.md`) deferred the queue panel
graft into the in-process TUI because the lifted TUI disables the
textarea during `StateStreaming` and a queue panel without
input-while-streaming is just decoration. This doc locks in the
shape of all four operator-input mechanisms before any of them
ships, so the API decisions (system-note framing, overlay UX,
inbox-tagging, slash-command surface) are made once and the
implementation PRs are mechanical from there.

## A — Input-while-streaming + queue panel

**Status today:** the lifted `Model.handleSubmit` no-ops while
`state == StateStreaming`. Operator's keystrokes go nowhere; the
textarea is visually inactive.

**Change:**
- Allow `Enter` in the textarea during `StateStreaming`. Submitted
  text goes into `a.Inject()` (already exists) AND into a TUI-local
  queue model that renders a panel between the thinking indicator
  and the textarea.
- Queue panel shows each entry with a state glyph: queued (⏳),
  in-flight (the active turn the entry just started), done (·),
  failed (✗). Done entries fade after ~2s. Failed entries persist
  with the error and can be dismissed via focused-panel Esc.
- When the current turn completes, B kicks in to drain the inbox
  and auto-start a new turn — the queue panel's first entry moves
  from queued → in-flight.

**TUI changes:** `internal/tui/update.go` (drop the streaming-state
guard in `handleSubmit`); new `internal/tui/queue.go` (graft from
`cmd/core-agent-tui/queue.go` with the lifecycle simplified for
in-process semantics — no sending/acked HTTP round-trip); update
`view.go` to render the panel between thinking indicator and
textarea (only when non-empty).

## B — Auto-continue from inbox + system-note framing

**Status today:** `Agent.Run` drains the inbox once at turn start
(`agent.go:516`) via `prependInboxMessages`. Inside `Run`, ADK's
`runner.Run` takes over and we have no hooks between its internal
tool-call iterations. Anything operator-injected during the turn
sits in the inbox until the *next* operator-initiated turn.

**Change:** auto-start a follow-up turn whenever the current turn
completes with a non-empty inbox.

- Implementation lives in the TUI's turn driver (`agentcmd.go`'s
  `startAgentTurn` or wherever the post-turn handoff happens), NOT
  in `agent.Run`. No ADK surgery. No conversation-shape decisions
  — the auto-continue turn is a normal `Run()` call; the inbox
  formatting carries the mid-turn-added context.
- New `prependInboxMessagesAutoContinue` (sibling of the existing
  `prependInboxMessages`) formats the inbox with **system-note
  framing**: tells the model "the operator added these while you
  were working — consider relevance and use `todo` if not relevant"
  (exact wording locked below).
- Visual marker on the auto-continue turn (per locked decision):
  the user prefix renders as `↻ user` (or similar distinct glyph)
  with a muted-color treatment, so the operator sees at a glance
  that this turn was machine-initiated from queued input rather
  than from a fresh Enter.

**Why the model handles this well:** `todo` is already a built-in
tool the model knows. The framing instructs it to either incorporate
or log-and-continue. The decision lives where it belongs (the
model has the most context about the current task); we don't try
to guess relevance in code.

## C — `/btw` side queries

**Status today:** nothing.

**Change:** spawn an independent, parallel one-shot model call when
the operator types `/btw <question>`. Per Claude Code's semantics
([reference](https://code.claude.com/docs/en/interactive-mode#side-questions-with-/btw)):

- Sees full conversation history (re-uses the prompt cache for
  cheap cost)
- **No tools** — answers only from in-context info
- Single-shot, no follow-up turns
- Result lands in a **dismissible overlay** that never enters
  conversation history
- Runs in parallel — main turn is unaffected

**Implementation pieces:**
1. **Conversation-history accessor** on `Agent` (or via ADK session
   service). Need to verify ADK's API; if no direct accessor, we
   pull events from the session and reconstruct. Implementer
   investigates and picks the cleanest path.
2. **One-shot tool-less model call.** Construct an `LLMRequest`
   with `Tools: nil` and the conversation history as context.
   Use `models.Provider.Model(...)` directly; bypass `agent.Run`
   so no inbox/permissions/eventlog write-back happens.
3. **TUI overlay** — new modal type, dismissible with
   Space/Enter/Escape. Similar to the existing elicit modal but
   read-only.
4. **Slash + dispatch** — `SlashBTW` in `commands.go`, handler
   spawns the goroutine, sends results back via tea messages.

## D — `/subagent <prompt> [flags]`

**Status today:** the model can call `spawn_agent` as a tool. No
operator-direct slash exists. Operators today have to ask the main
agent to spawn one ("please spawn a subagent to monitor X").

**Change:** wire a slash command that calls `BackgroundAgentManager.
Spawn(...)` directly, bypassing the main agent's reasoning. Optional
args mirror the tool's parameters (model override, skill, etc.) so
operators can configure the subagent without round-tripping through
the main agent.

**Syntax:**
```
/subagent <prompt>
/subagent --model=gemini-3.1-flash <prompt>
/subagent --skill=code-review --name=reviewer <prompt>
```

Implementation: simple slash → `BackgroundAgentManager.Spawn`. The
spawned subagent participates in the existing alert hook
(`agent.Alert` → TUI inline notification) so the operator sees
progress without polling `/subagents`.

## Decisions (settled)

1. **Visual marker for auto-continue turns.** Use a distinct
   prefix glyph (`↻`) + muted color on the user-message line so
   the operator visually distinguishes machine-initiated turns
   (from queued input) from operator-initiated turns (from Enter).
2. **`/btw` overlay.** Dismissible overlay, like Claude Code.
   Dismissed with Space/Enter/Escape. Not in conversation history.
3. **`/subagent` accepts optional args.** Flag-style after the
   subcommand: `--model`, `--skill`, `--name`. Implementation
   delegates to the same arg parser the `spawn_agent` tool uses.

## System-note framing for B (settled)

The exact phrasing the model receives on every auto-continue turn:

```
[Operator notes added during the previous task]
- <message>

If any of these change the current task, adapt your next step. If they're separate requests, use the `todo` tool to capture them and continue what you were doing.
```

Concise + instructional (~30 words). The verbs "adapt" and
"capture" both name an action, so weaker-instruction-following
models reliably do the right thing without the framing dominating
the context window when multiple entries pile up.

If `<message>` is a single entry, the bullet list collapses to a
single line. If multiple entries arrived during the previous turn,
they all render as bullets under one shared instruction block.

## Open questions for the implementer

1. **ADK conversation-history accessor (for C).** Does ADK's
   session service expose the message log such that we can pass it
   as context to a parallel one-shot model call without going
   through the runner? If not, we reconstruct from `session.Events`.
   Decide during PR-C scoping.
2. ~~**`/btw` cost ceiling.**~~ **Settled:** no special cap.
   Treat `/btw` as just another user prompt against the same
   model; whatever token / cost limits the provider already
   enforces apply. The conversation cache reuse keeps marginal
   cost low for repeated `/btw`s during the same session.
3. **Auto-continue turn limit.** Should we cap the number of
   consecutive auto-continue turns (e.g. if the operator keeps
   typing during each auto-continue, we could chain forever)?
   Recommend yes, with a soft limit (~10) + a system note: "the
   operator is typing faster than you can process; consider asking
   them to slow down or batch."
4. **Queue panel persistence across the auto-continue boundary.**
   When B's auto-continue starts, does the consumed entry stay
   visible (briefly, faded) or disappear immediately from the
   queue panel? Recommend the existing "Done entry fades after
   ~2s" pattern from `cmd/core-agent-tui/queue.go`.

## Phased delivery — three PRs

1. **PR α (A + B):** Input-while-streaming + queue panel + auto-
   continue + system-note framing + visual marker. ~250–400 LoC
   across `internal/tui/` and `agent/inbox.go` (new
   `prependInboxMessagesAutoContinue`) + `agent/agent.go` (no
   change to `Run` itself; the auto-continue logic lives in the
   TUI's turn driver). Tests: queue lifecycle, auto-continue
   trigger, system-note formatting, soft-cap on consecutive
   auto-continues.
2. **PR β (C):** `/btw` slash + overlay + parallel one-shot. ~150–
   200 LoC. ADK conversation-history accessor investigation
   gates the implementation. Tests: overlay rendering, parallel
   execution (main turn unaffected), dismiss handling.
3. **PR γ (D):** `/subagent` slash + arg parsing. ~80 LoC. Tests:
   slash dispatch, arg parsing, spawn confirmation.

Each PR independently shippable. PR α and β can land in either
order; γ depends on neither.

## Out of scope

- **Mid-turn-within-a-turn injection** (operator input reaching
  the model between ADK's internal tool-call iterations). Would
  require ADK callback hooks or runner-wrapping. The B + C
  combination delivers most of the operator value without the
  architectural cost. Revisit if a real "operators wish they could
  steer mid-tool-chain" complaint surfaces.
- **`/btw` follow-up turns** (back-and-forth in the side panel).
  Per Claude Code's semantics, `/btw` is single-shot by design —
  if you need a conversation, use a normal prompt. Easy to add
  later if operators ask.
- **Queue reordering / editing** (operator can't drag-drop or
  edit queued entries before they process). FIFO only in v1.
  Failed entries can be dismissed; that's it.
- **`/subagent` UI for monitoring spawned subagents.** The
  existing `/subagents` slash already lists them with status.
  Don't add a second surface in v1.
