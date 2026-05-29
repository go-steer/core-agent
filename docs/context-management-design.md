# Context management: compaction + micro-subagents + checkpoints + memory

Design note for four mechanisms that together keep a long-running
core-agent session — and a multi-session relationship between an
operator and an agent — inside the context window without losing
continuity. The four are co-equal and complementary, not
alternatives: each addresses a distinct failure mode and shipping
only some leaves the others unaddressed.

| | Mode | When it fires | What it does |
|---|---|---|---|
| **A. Reactive compaction** | Pull | Context window near a token threshold (or operator typed `/compact`) | Summarizes prior turns into one dense message; slices history at the summary; next turn starts with the summary as the seed |
| **B. Micro-subagents** | Push | A tool runs whose raw output would otherwise pollute the parent's context | The heavy work (file read, URL fetch, broad grep) runs inside an isolated subagent on a smaller model with a narrow toolset; only the digested result reaches the parent |
| **C. Task-boundary checkpoints** | Pull | Model signals a task is complete (via `mark_task_done` tool, `/done` slash, or post-turn heuristic) | Compacts at a semantic boundary regardless of token count — preserves the just-finished task as a tight handover and clears the slate for the next one |
| **D. Persistent memory** | Push + pull | Across the session boundary | Structured memory store (semantic + episodic) survives between sessions; indexed for retrieval so the agent can recall "what did we decide about X" without re-reading the whole transcript. Detailed in [`shared-memory-design.md`](shared-memory-design.md); summarized here for how it ties into A/B/C. |

The four mechanisms map cleanly to four distinct failure modes:

| Failure | Mechanism |
|---|---|
| "Conversation got long and the model ran out of room" | A (reactive compaction) |
| "Every tool call dumped raw output into context until attention decayed" | B (micro-subagents) |
| "We finished feature X and the model is still drowning in feature X's exploration when the operator asks about feature Y" | C (task-boundary checkpoints) |
| "We had this exact debate three sessions ago and the agent doesn't remember" | D (persistent memory) |

A session at steady state on a long task should rely mostly on B
(proactive bloat-prevention at the tool layer) and C (proactive
cleanup at task boundaries), with A as the safety net that fires
when B and C can't keep up (long human messages, deep tool-call
chains, prompts that need full raw history visible to the parent).
D operates on a different timescale entirely — it's what lets a
new session pick up where last week's session left off without
the operator re-explaining context.

This doc covers all four mechanisms, the coordination primitive
that ties them together, and what we explicitly defer.
Implementation lives in the `agent/`, `usage/`, and `eventlog/`
packages; D's storage backend is the existing shared-memory work.
The TUI gets `/compact` and `/done` slash commands; no other UI
surface beyond what the existing `/subagents` slash already
provides.

## Why one doc, not four

Every primitive each mechanism needs is shared:

- A, B, and C all need to call the model **bypassing the normal
  `Agent.Run` flow** (no inbox drain, no permission gating against
  tools the side call doesn't even have, no event-log write-back
  of the side call's reasoning). `Agent.AskSideQuestion` (added
  in PR β for `/btw`) is the shape; A's summarizer, B's
  `RunSubtask`, and C's checkpoint summarizer all re-use the same
  primitive under different names.
- A, C, and D all need to **read the session's prior events** to
  construct their model input or to project them into memory
  records. `Agent.SessionService()` + the iteration pattern from
  `agent/btw.go` is the shape.
- A, B, and C all need to **bound cost** with budgets and
  **propagate cost back** to the parent's `usage.Tracker`.
- A and C rely on the **context-window-size accessor** added on
  the core-tui adapter branch (commit `43e3da9`) — A uses it as
  the threshold trigger; B uses it to know whether a tool's raw
  output is worth distilling vs. small enough to pass through; C
  uses it as the "did we actually need this checkpoint" telemetry
  signal.
- C and D share **summary structure**. C's task-boundary summary
  is the same five-section shape A produces; D's episodic-memory
  records can be derived from C's summaries cheaply (a checkpoint
  is by definition a high-quality, dense projection of a
  semantically bounded slice of conversation, which is exactly
  what an episodic memory record wants to be). The audit-derived-
  memory thesis from [`shared-memory-design.md`](shared-memory-design.md)
  becomes much more tractable when there are checkpoints to
  derive from rather than raw event streams.

If we design these in separate docs they re-derive the same
primitives multiple times and probably diverge on naming. One
doc, four mechanisms.

## Goals & non-goals

**Goals**

- Land all four mechanisms in core-agent (substrate, not TUI).
  Consumers flip each on with `With...` options at construction:
  `WithCompactor(...)`, `WithSubtaskRunner(...)`,
  `WithCheckpointer(...)`, `WithMemoryStore(...)`. All four are
  independent — a consumer can adopt one, two, three, or all four.
- Keep the existing `BackgroundAgentManager` + `/subagent` slash
  semantics unchanged. Micro-subagents are a *different shape*
  (synchronous, single-purpose, return a string) and get their own
  primitive rather than overloading the existing one.
- Make every tool that wraps a micro-subagent opt-in, not magical.
  `tools.AgenticReadFile` is a distinct constructor from
  `tools.NewReadFile`; consumers pick which to register.
- Compaction + checkpoints must survive crash-resume: summary
  events live in the event log; relaunching the agent reads the
  session and picks up where the last summary left off.
- Persistent memory (D) is **backend-pluggable** through the
  `Memory` interface in [`shared-memory-design.md`](shared-memory-design.md);
  core-agent ships with the in-tree FTS5-over-eventlog backend,
  consumers can swap in Redis AMS / Mem0 / their own via the
  extras adapter pattern.

**Non-goals**

- KV-cache layout micro-optimization (static-header / dynamic-tail
  reordering). The model provider's cache config (Anthropic
  prompt caching, Gemini context caching) handles most of the win
  without us hand-rolling boundaries.
- Viewport-aware prompt injection. Requires TUI ↔ agent coupling
  we don't have and don't want.
- Explore-plan-execute or skeptical-critic pipelines. These are
  composable from the primitives this doc ships; we don't need
  built-in drivers in core-agent for them.
- Token-level pruning that removes intermediate tool payloads from
  arbitrary historical events. Surgically fiddly, easy to break
  continuity. A + C subsume the use case.
- A vector-database hard dependency. D's in-tree default uses
  SQLite FTS5 over the existing eventlog; vector backends are
  available via the extras adapter pattern but not required to
  ship the feature.

## Mechanism A — reactive compaction

### Trigger

Two ways to fire:

1. **Automatic threshold.** After each turn completes, the
   compactor reads `usage.Tracker.ContextWindowUsed()` and
   compares to `ContextWindowSize()`. If `used / size ≥ 0.85`
   (configurable via `WithCompactorThreshold(float64)`), schedule
   a compaction turn before the next user prompt.

   When `ContextWindowSize()` returns 0 (model not in the lookup
   table, custom provider, etc.) the compactor is a no-op — better
   to do nothing than to trim prematurely with bad data.

2. **Manual.** The operator types `/compact` (TUI slash, also
   exposed as `Agent.Compact(ctx)` for programmatic use).
   `/compact <focus>` passes a focus hint into the summarizer
   prompt so the operator can say *"focus on the auth-rewrite
   thread, drop the unrelated diff exploration."*

The choice of 0.85 is arbitrary but a defensible starting point —
high enough that we don't compact too eagerly (every compaction
costs a tool-less model call), low enough that we have headroom
for one more full turn after the threshold trips before we'd
actually hit the wall.

### Summarizer turn

The compaction turn is a one-shot, tool-less LLM call against the
agent's existing model:

- Load session events via `SessionService().Get(...)` (same path
  `AskSideQuestion` uses).
- Filter to operator-visible events: skip Partial, skip
  background-subagent branch events.
- Format as `[]*genai.Content` and prepend a structured system
  instruction asking the model to produce a teammate-style
  handover with the sections below.
- Call `model.GenerateContent(ctx, req, false)` with `Tools: nil`.

The summarizer prompt mandates five sections (lifted from the
industry pattern that's now standard across coding-agent products):

1. **Current state** — exact user request, what's been completed,
   what's actively in progress, what's remaining.
2. **Files & changes** — files modified (with one-line
   descriptions), files read/analyzed, files needing changes that
   weren't touched yet, critical line numbers.
3. **Technical context** — architectural decisions made, patterns
   adopted, commands that worked, commands that failed and why,
   environment quirks.
4. **Strategy & approach** — the strategy chosen, alternatives
   considered and rejected, gotchas, assumptions, blockers.
5. **Exact next steps** — a concrete numbered list of the next
   developer-style actions.

When `/compact <focus>` was used, an additional `Compact focus:
<text>` line goes into the system instruction so the model knows
to prioritize that thread.

### Persistence

The summary is written to the session as a regular event with two
distinguishing markers:

- `event.CustomMetadata["compaction"] = "summary"` so post-hoc
  consumers can find it.
- The session's `SummaryEventID` field (new — added on
  `session.Session` via a small extension or via
  `eventlog.SetSessionSummaryID`) tracks the most recent summary
  so the slicing logic on the next turn knows where to cut.

Multiple summaries can accumulate over a long session; only the
latest one is used as the slicing boundary.

### History slicing

On every subsequent turn, when `Agent.Run` builds the model
request, the history-loading path:

1. Pulls all session events.
2. If `SummaryEventID` is set, drops every event with sequence
   number less than the summary's.
3. The summary event itself remains, but its role is rewritten to
   `genai.RoleUser` for the model's input — the resuming model
   reads the summary as the "user told me this is where we are"
   seed rather than as a synthetic system note.
4. Token accounting on the tracker resets `PromptTokens` to 0 and
   `CompletionTokens` to the summary's token count; subsequent
   turns reaccumulate from the slimmed baseline.

The actual session event log is **not** mutated. Slicing happens
at request-construction time so the audit trail stays complete and
later analysis can reconstruct what really happened.

### What survives compaction

Three things survive every compaction by design:

1. **The summary itself.** Always at position 0 of the post-slice
   history.
2. **`AGENTS.md` / `CLAUDE.md` / system instruction.** These are
   `WithInstruction` content, never part of session events, so
   slicing leaves them untouched.
3. **The agent's `todo` tool state.** Already persisted via the
   `todo` tool's own mechanism; not in the sliced event stream.

That's the survival contract. Anything else (raw tool output, old
assistant turns, MCP elicit history) is summary-or-discard.

### API

```go
// Construction
agent.New(model,
    agent.WithCompactor(agent.NewCompactor(agent.CompactorOptions{
        Threshold: 0.85,                   // fraction of context window
        Summarizer: nil,                   // optional override of the prompt builder
        OnCompact: nil,                    // optional callback (telemetry hook)
    })),
)

// Manual
result, err := a.Compact(ctx, "focus on the auth-rewrite thread")
// result has: SummaryEventID, TokensBefore, TokensAfter, Duration

// Slash dispatch (TUI)
//   /compact
//   /compact <focus text>
```

`Compactor` is an interface so consumers can plug their own
summarizer (different prompt, different model, additional sections)
without forking the agent.

## Mechanism B — micro-subagents

### Problem

Many tools produce raw output whose volume dwarfs the useful
signal:

- `read_file` on a 5,000-line config that the agent needed three
  lines of.
- `fetch_url` on a 200KB documentation page that answers a single
  question.
- `grep` across a large repo that returns hundreds of matches
  when the agent needed to confirm one symbol exists.
- `bash` running a build that prints thousands of lines of
  successful compilation logs surrounding a single warning the
  agent cares about.
- A broad investigation ("understand how the auth flow works
  end-to-end") that legitimately needs to read a dozen files but
  whose intermediate exploration is noise to the parent.

The bloat doesn't just consume the window — it actively degrades
the model's attention. Industry pattern: wrap the operation in an
isolated subagent whose only job is to execute, read the result,
and emit a digest the parent's model sees.

### The fresh-context property is the load-bearing one

Every micro-subagent — narrow tool wrapper or broad research dispatch
— gets a **fresh context window** with no parent history. That's
not a side effect of isolation; it's the main reason this pattern
works:

1. The subagent's reasoning is **not poisoned by the parent's
   accumulated assumptions**. A parent that's been debugging an
   auth bug for 50 turns will tend to interpret everything through
   that lens; a fresh subagent reading the same code sees what's
   actually there.
2. The subagent gets the **model's full attention budget** for the
   narrow question, not the residual after the parent's history
   has consumed most of it. A 1M-window model that's been burning
   through 800k of context is effectively a 200k-window model;
   the subagent gets the full 1M back.
3. The subagent can use a **smaller, cheaper model** because the
   task is bounded — a parent doing complex multi-step reasoning
   needs a frontier model, but "read this file and tell me where
   the OAuth callback handler is" doesn't.
4. The parent's context **only ever sees the digest**, so even
   for tasks that legitimately need broad exploration (read 12
   files, follow 4 sub-links, run 3 greps), only the conclusion
   reaches the parent.

This applies across the whole spectrum:

| Shape | Example | Typical model | Typical budget |
|---|---|---|---|
| **Narrow tool wrap** | `AgenticReadFile` on one file | small (haiku-tier) | 1 turn, 20K tokens, 10s |
| **Bounded query** | `AgenticGrep` ranking + summarizing matches | small | 1–2 turns, 30K tokens, 20s |
| **Broad research dispatch** | `AgenticResearch("how does X work?")` | medium (sonnet-tier) | 3–5 turns, 100K tokens, 90s |

All three shapes share the `RunSubtask` primitive below; they
differ only in their `SubtaskSpec` (toolset, model choice,
budgets, prompt). Consumers register whichever shape they need
as a tool; the parent's model calls them like any other tool and
just sees the digested string return.

### Primitive: `RunSubtask`

A synchronous, single-purpose, isolated-context model call:

```go
type SubtaskSpec struct {
    Name         string         // for traces + cost attribution
    SystemPrompt string         // the subtask's role
    UserMessage  string         // the question / instruction
    Tools        []tool.Tool    // narrow set; often read-only
    Model        adkmodel.LLM   // optional override (typically a cheaper, faster model)
    Budgets      SubtaskBudgets // turn cap, token cap, wallclock cap
}

type SubtaskBudgets struct {
    MaxTurns     int           // default 5
    MaxTokens    int           // default 50_000
    MaxWallclock time.Duration // default 60s
}

// Returns the digested string the subtask reported back, plus a
// SubtaskResult carrying cost / turn count / failure cause for the
// parent's tracker.
result, err := a.RunSubtask(ctx, spec)
```

Properties:

- **Synchronous.** Caller blocks until the subtask returns. Unlike
  `BackgroundAgentManager.Spawn` (which is fire-and-forget), this
  is "do the work, give me the answer, then go away."
- **Single-purpose.** Tight turn cap (default 5) so a subtask
  can't spiral into its own multi-step engagement. If it can't
  answer in 5 turns, the parent's model is the right place to
  reason about it.
- **Context-insulated.** The subtask gets its own session ID
  (`<parent-session>.sub.<n>`); its events land in the same event
  log on a distinct branch so the audit trail is unified but the
  parent's `Run` never sees them.
- **Cost-attributed.** On completion, the subtask's token /
  dollar totals are added to the parent's `usage.Tracker` with a
  `subtask=<name>` tag so `/stats` can show *"of $0.12 this
  session, $0.03 went to micro-subagents."*
- **Auto-approve internal tool calls.** Subtasks bypass the
  parent's permission gate for read-only operations they're
  configured with. The narrow toolset is the safety contract;
  prompting the operator for every sub-internal `read_file` would
  defeat the purpose. Writes / shell still gate normally — a
  subtask that needs them is the wrong shape and should be a
  full subagent.

### Tool wrappers

The primitive is general; the value is in opinionated wrappers
shipped in `core-agent/tools`:

```go
// Read a file and return a focused excerpt or summary.
tools.AgenticReadFile(opts AgenticToolOpts)

// Fetch a URL, extract the relevant section, return as markdown.
tools.AgenticFetchURL(opts AgenticToolOpts)

// Run a grep, rank results by relevance to the user's question,
// return the top N with surrounding context.
tools.AgenticGrep(opts AgenticToolOpts)

// Run a broad codebase exploration ("understand the auth flow"),
// return a structured walkthrough with file:line citations.
tools.AgenticResearch(opts AgenticToolOpts)
```

`AgenticToolOpts` carries the small-model handle, budgets, and a
`Provider models.Provider` so the wrapper can resolve a fresh LLM
per subtask without the caller plumbing it.

Each wrapper has a corresponding non-agentic version (`NewReadFile`
etc.) that just runs the tool and returns raw output. Consumers
register whichever shape fits their context budget. A coder-focused
consumer like cogo will likely register `AgenticReadFile` for the
file-read slot and `NewReadFile` for nothing (or expose both as
distinct tool names so the model can choose).

### Auto-approve and gating

`SubtaskSpec` carries an explicit `AutoApproveRead bool` (default
true) and `AutoApproveWrite bool` (default false). The compositor
inside `RunSubtask` wires a permissions gate that:

- Auto-allows any tool with `tool.IsReadOnly() == true` when
  `AutoApproveRead`.
- Falls through to the parent's normal gate for any tool not in
  the auto-allow set.

This means a subtask with `AutoApproveWrite=true` can write files
without prompting — but the operator never sets that on a generic
agentic tool wrapper; only a purpose-built consumer like a
batch-rename subtask would opt in.

### Failure modes

- **Budget exhaustion.** The subtask returns its best-effort
  digest plus a `truncated: true` annotation. Parent receives the
  partial answer with the truncation note so the model knows
  there might be more.
- **Tool error.** The subtask catches the error, formats it as
  part of the digest ("attempted X, hit: <err>"), and returns
  normally. Parent's model can choose to retry or adapt.
- **Subtask LLM failure.** Bubbles up as the error return from
  `RunSubtask`. Caller (typically a tool wrapper) decides whether
  to surface as a tool error to the parent's model or fall back
  to running the raw tool.

## Mechanism C — task-boundary checkpoints

### Problem

A long task — say "rewrite the auth middleware" — accumulates
context that is *useful while the task is in flight* and *dead
weight afterwards*. By the time the task is done, the parent's
history holds: the exploration that found the relevant files,
the dead-end attempts that didn't pan out, the tool output from
the build that verified the change, the back-and-forth deciding
between two implementation strategies. None of that helps the
next task — and if the next task is unrelated ("look into the
billing-team bug report"), it actively distracts.

Threshold-based compaction (Mechanism A) doesn't catch this
because the token count might still be well under the limit; the
problem is *semantic*, not quantitative. We need a trigger that
fires when a task is **complete**, not when the window is **full**.

### Trigger

Three ways to fire a checkpoint:

1. **Model-initiated.** A new built-in tool `mark_task_done(detail)`
   that the model can call when it believes the task is complete.
   The tool returns immediately; the post-turn hook detects the
   call, generates the checkpoint, and clears history. The
   prompt that ships with `WithCheckpointer(...)` instructs the
   model: *"When you've completed a coherent task, call
   `mark_task_done` with a one-paragraph summary. This frees up
   context for the next task."*
2. **Operator-initiated.** Slash `/done [optional note]` (or
   `Agent.Checkpoint(ctx, note)` programmatically) — operator
   says "we're done with this thread, clean up." Useful when the
   model didn't think to call `mark_task_done` itself.
3. **Heuristic** (optional, off by default). Post-turn check looks
   for "completed" / "all done" / "finished" patterns in the
   model's final assistant message. Disabled by default because
   false positives are costly; consumers can opt in via
   `WithCheckpointHeuristic(true)`.

### Checkpoint content

The checkpoint summary uses the same five-section structure as
Mechanism A's compaction prompt (current state, files & changes,
technical context, strategy & approach, next steps) but with an
additional **completion record** at the top:

```
[Task complete: <name>]
Outcome: <one-line summary>
Started: <timestamp>  Duration: <wallclock>  Cost: $<x>
```

This makes checkpoints distinguishable from threshold-triggered
compactions in the event log (different
`event.CustomMetadata["compaction"]` value: `"checkpoint"` vs
`"summary"`) and gives D something tight to derive a memory
record from.

### What's different from A

A and C share most of the machinery: same summarizer,
`AskSideQuestion`-shaped call, same history-slicing pattern. The
differences:

| | A (reactive) | C (proactive) |
|---|---|---|
| Trigger | Token threshold | Task-complete signal |
| Granularity | "Conversation got long" | "This specific task is done" |
| Frequency | Rare (when over threshold) | Common (at every task boundary) |
| Survives in event log | Yes (CustomMetadata=summary) | Yes (CustomMetadata=checkpoint + task name + duration) |
| Feeds Mechanism D | Yes, but lossy | Yes, and cheap — checkpoint is already a clean task-bounded record |

### API

```go
agent.New(model,
    agent.WithCheckpointer(agent.NewCheckpointer(agent.CheckpointerOptions{
        ToolName:    "mark_task_done",  // default; consumers can rename
        EnableHeuristic: false,         // off by default
    })),
)

// Manual / programmatic
result, err := a.Checkpoint(ctx, "auth middleware rewrite complete; tests green")

// Slash dispatch (TUI)
//   /done
//   /done finished the migration; tests green
```

Like `WithCompactor`, `WithCheckpointer` is fully optional. A
consumer that wants task-boundary compaction registers it;
consumers that don't get the default "no checkpointer" behavior
where `mark_task_done` is not registered as a tool and `/done`
isn't recognized.

## Mechanism D — persistent memory

### Problem

Compaction (A) and checkpoints (C) keep a single session
manageable. Neither survives across the session boundary. The
next time the operator launches the agent, all of last week's
context — what was decided, why, what worked, what failed — is
gone unless it happens to be written down in code or in
`AGENTS.md`. The operator either re-explains every time, or the
agent re-derives everything every time, or both.

### Two layers, one interface

Persistent memory has two distinct shapes that solve different
problems:

1. **Semantic memory** — structured facts and preferences the
   agent has learned about the user, the project, the conventions.
   "The user prefers `errors.Wrap` over `fmt.Errorf`." "The
   billing module's tests can't be run without a Postgres
   instance." "The user has been working on the auth-rewrite
   thread across 6 sessions." Stored as typed records; retrieved
   by exact match on tags / namespaces, not full-text search.
2. **Episodic memory** — projections of prior conversations
   ("last week we decided to defer multi-tenancy until v2"),
   indexed and searched when relevant. Sourced primarily from
   Mechanism C's checkpoints because checkpoints are already
   clean task-bounded summaries; can also be sourced from raw
   eventlog via background extraction (the audit-derived-memory
   thesis).

Both layers sit behind a single `Memory` interface defined in
[`shared-memory-design.md`](shared-memory-design.md). That doc
covers the full surface in depth; here we only sketch how D ties
into A, B, and C.

### Backends

`Memory` is the interface; consumers pick a backend:

- **In-tree default**: SQLite FTS5 over the existing
  `agent_eventlog` table — episodic memory comes for free from
  the audit log we're already writing, indexed via FTS5 for
  keyword + BM25 retrieval. No vector dependency.
- **Extras adapter**: Redis Agent Memory Server, Mem0,
  OpenClaw-style stores, or anything that fits the `Memory`
  interface. Wired via the extras-adapter pattern (similar to
  how scion / AX are wired today).

The audit-derived-memory thesis — "the audit log is already a
durable record; index it, don't duplicate it" — is what makes the
in-tree backend cheap to ship. C makes it even cheaper because
checkpoints are exactly the kind of dense, task-bounded summary
that indexes well and that the agent will actually find useful
when retrieved.

### Surface to the agent

Memory is exposed to the agent as two tools:

- `recall(query, k=5)` — retrieves the top-k memory records
  matching the query. Uses Mechanism B (`AgenticResearch` over
  the memory store) so the parent's context only sees the
  digested "here's what's relevant" answer, not the raw record
  dump.
- `remember(content, tags?)` — writes a semantic memory record.
  Typically the model calls this near the end of a task, often
  right before `mark_task_done`.

Episodic memory is read-mostly: the agent doesn't write to it
directly; it's populated automatically from checkpoints (and
optionally from background extraction over the eventlog).

### Pre-turn injection

When `WithMemoryStore(...)` is wired, every `Agent.Run` has the
option to:

1. Run a quick `recall(currentUserMessage)` (or skip if the
   user's message is too short / clearly conversational).
2. Prepend the top match(es) as a `[Memory recall]` block,
   sibling to the existing `[Inbox]` block.
3. Let the model decide whether to use them.

Pre-turn injection is opt-in via `WithMemoryRecallStrategy(...)`
because it adds latency to every turn and isn't always wanted.
The default is "don't auto-recall; expose the `recall` tool and
let the model fetch on demand." Auto-recall is appropriate for
consumers where the operator's queries tend to reference prior
context implicitly ("can we try that other approach?" — recall
helps); inappropriate where context is mostly fresh per session.

### Survival contract

Memory survives across **everything**: session restart,
compaction, checkpoint, agent rebuild. The eventlog is the
durable substrate; the FTS index rebuilds from it. The Redis /
Mem0 backends own their own persistence.

## Implementation note: all four mechanisms are hooks

A, B, C, and D are not four distinct subsystems with separate
control loops — they are four sets of pre/post hooks against
`Agent.Run`'s existing event flow. Specifically:

- **Mechanism A's threshold check** is a post-turn hook
  (`func(ctx, turnResult) error`) that inspects `usage.Tracker`
  and schedules a compaction call when over threshold.
- **Mechanism B's `RunSubtask`** is invoked from inside a tool
  handler — effectively a pre-tool hook that replaces the raw
  tool call with an isolated subagent call.
- **Mechanism C's `mark_task_done` detection** is a post-turn
  hook that scans for the tool call and triggers `Checkpoint`.
- **Mechanism D's pre-turn `recall()` injection** is a pre-turn
  hook (`func(ctx, prompt) (prompt, error)`) that prepends a
  `[Memory recall]` block.
- **D's checkpoint-to-memory write** is a post-checkpoint hook.

The internal implementation may use either direct function fields
on `Agent` (the simplest path) or compose against a general typed
hook surface — `WithPreTurnHook(...)`, `WithPostTurnHook(...)`,
`WithPreToolHook(...)`, `WithPostToolHook(...)`,
`WithPostCheckpointHook(...)` — that consumers can extend
directly.

Either way, this is a substrate concern that goes beyond context
management: hooks let consumers add behaviors we haven't
anticipated (custom telemetry, distributed tracing, per-tool
budgets, audit-log streaming, custom prompt augmentation) without
forking the agent or asking us to ship a one-off mechanism for
each. The hook surface is being designed separately in
[`agent-hooks-design.md`](agent-hooks-design.md) (TODO); the four
context-management mechanisms in this doc will land first as
internal hooks and may be refactored to sit on the public
surface once it stabilizes.

## Coordination primitives

Three lightweight conventions that tie the above together and
match what running consumers will need.

### 1. Parent → subagent injection

The existing `Agent.Inject` already covers this for background
subagents — each subagent is an `Agent`, and the parent can call
`subagent.Inject("focus on the build error in pkg/foo")` to drop
a system note into the subagent's next turn.

We expose this from the manager as a convenience so consumers
don't track subagent `*Agent` references themselves:

```go
err := mgr.Send(subagentName, "focus on the build error in pkg/foo")
// internally: looks up the subagent's Agent by name, calls Inject
// returns ErrSubagentNotFound when name doesn't exist
```

Mirrors the agent-to-agent send pattern that the industry has
converged on, but stays narrowly scoped (parent → named subagent;
not subagent → arbitrary peer, which we don't need yet).

### 2. Event-driven wake instead of polling

Already in place via `Agent.WakeRequested() <-chan struct{}` and
`Agent.RequestWake()`. Documenting the contract explicitly: any
consumer that wants to drive a multi-agent loop should
`select { case <-ag.WakeRequested(): … case <-ag.InboxArrived(): … }`
and never `sleep`-poll. Subagent alerts already invoke `RequestWake`
on the parent so this lights up automatically.

### 3. Subtask cost rollup

`Agent.RunSubtask` calls `parentAgent.tracker.AppendSubtask(...)`
with the subtask's costs tagged. Parent's `/stats` then displays:

```
Session totals:
  ↑ 124,500 in · ↓ 18,200 out · $0.45
    of which subtasks: $0.07 (4 subtasks)
```

Cost transparency matters when the parent's model is expensive and
subtasks run on a cheaper one — operators want to see the savings.

## Phased delivery

Six PRs, independently shippable, each behind a `With...` option
so consumers opt in. Ordering matters only where noted — A and B
can ship in either order; C wants A's summarizer to land first;
D wants C in place to derive episodic records from cleanly.

### PR I — Mechanism A: compaction substrate

`agent/compactor.go` (new): `Compactor` interface,
`DefaultCompactor`, `Agent.Compact(ctx, focus) (CompactionResult,
error)`, threshold-based trigger fired from `agent.Run`'s
post-turn hook.

`eventlog/`: store the summary's event ID on session (either
`session.Session` extension or `event.CustomMetadata` read-back).

`agent/agent.go`: history-loading path in `Run` checks for a
`SummaryEventID` and slices accordingly.

`internal/tui/commands.go`: `SlashCompact`.
`internal/tui/update.go`: `handleCompactCommand(args)` calls
`Agent.Compact(ctx, args)` in a goroutine, posts the result as a
chat system message.

Tests: synthetic session with N events → `Compact()` → assert
summary event written, `SummaryEventID` set, next `Run()` sees
only the summary; threshold trigger fires only when over
threshold; unknown context window is a no-op.

~400–600 LoC.

### PR II — Mechanism B: `RunSubtask` + agentic tool wrappers

`agent/subtask.go` (new): `SubtaskSpec`, `SubtaskBudgets`,
`SubtaskResult`, `Agent.RunSubtask(ctx, spec) (SubtaskResult,
error)`. Internally constructs a fresh sub-`Agent` with isolated
session ID, wires a read-only gate, runs synchronously, drains
events, rolls cost up to the parent tracker.

`tools/agentic_read_file.go`, `tools/agentic_fetch_url.go`,
`tools/agentic_grep.go`, `tools/agentic_research.go` (new):
wrapper constructors taking `AgenticToolOpts{Provider,
SmallModelID, Budgets}`.

Tests: `RunSubtask` with a stub LLM returns the digest, propagates
costs, respects budgets, isolates session events from the parent;
each wrapper test confirms raw tool runs, model digests, parent
sees only the digest; explicitly assert the subtask's session
events are *not* visible to a parent `Agent.Run` follow-up
(fresh-context invariant).

~500–700 LoC.

### PR III — Mechanism C: task-boundary checkpoints

`agent/checkpointer.go` (new): `Checkpointer` interface,
`DefaultCheckpointer`, `Agent.Checkpoint(ctx, note)`. Registers
the `mark_task_done` built-in tool when wired.

`agent/agent.go`: post-turn hook detects `mark_task_done` call,
triggers `Checkpoint`.

`internal/tui/commands.go`: `SlashDone`.

Reuses PR I's slicing machinery — checkpoint events use
`event.CustomMetadata["compaction"] = "checkpoint"` (vs
`"summary"` for threshold-triggered), and the history-loading
path treats them the same way.

Tests: `mark_task_done` tool call → checkpoint event written +
history sliced; `/done` slash → same; checkpoint metadata
distinguishable from summary metadata; auto-trigger heuristic
fires only when enabled.

~300–400 LoC.

### PR IV — Mechanism D-prep: memory tooling on top of `shared-memory`

Assumes the in-tree FTS5-over-eventlog backend from
[`shared-memory-design.md`](shared-memory-design.md) is already
landed (it's tracked separately and not blocked on this doc).

`agent/memory.go` (new): wires the `Memory` interface into the
agent. `WithMemoryStore(memory.Memory)` option. `Agent.Recall(ctx,
query, k)` and `Agent.Remember(ctx, content, tags)` helpers.

`tools/recall.go`, `tools/remember.go` (new): expose `recall` and
`remember` as model-callable tools. `recall` internally uses
`RunSubtask` (Mechanism B) so the raw record dump stays out of
the parent's context.

`agent/checkpointer.go`: when a checkpoint completes, optionally
write an episodic memory record from the checkpoint content (cheap
because the checkpoint is already a clean summary). Gated by
`WithCheckpointToMemory(true)` so consumers without a memory
store don't pay for it.

Tests: `recall` returns top-k matches; `remember` round-trips;
checkpoint-to-memory writes an episodic record with the
checkpoint's tags.

~300–400 LoC (excluding the shared-memory work itself).

### PR V — Pre-turn memory recall (optional)

`agent/recall_strategy.go` (new): `RecallStrategy` interface with
`AlwaysRecall`, `RecallOnKeywords`, and a `NoRecall` default.
Wired via `WithMemoryRecallStrategy(...)`.

`agent/agent.go`: in `Run`, if a strategy is configured, runs
`recall(prompt)` pre-turn and prepends `[Memory recall]` block to
the model's input (sibling to `[Inbox]`).

Tests: strategy fires / doesn't fire as expected; recall block
formatting; block absent when memory store not wired.

~200–300 LoC. Genuinely optional — many consumers won't want
auto-recall.

### PR VI — `BackgroundAgentManager.Send` + cost rollup polish

`agent/background.go`: `Send(name, message) error` calls
`Agent.Inject` on the named subagent.

`internal/tui/commands.go`: optional `SlashSend` (`/send <name>
<message>`) for operator-driven injection. Low-priority — most
sends will be programmatic, not operator-typed.

`usage/tracker.go`: extend `Append` with a `Tag string` field so
subtask totals can be reported separately in `/stats`.

`internal/tui/update.go`: `renderStatsInfo` shows the subtask
rollup line + a checkpoint count if Mechanism C is wired.

Tests: `Send` finds the subagent and injects; cost rollup totals
match across parent + subtasks; stats line shows checkpoints.

~150–250 LoC.

## Open questions for the implementer

1. **Where does `SummaryEventID` live?** Options: (a) extend
   `session.Session` with a new field — clean but touches ADK's
   session schema; (b) store as `event.CustomMetadata` on each
   summary event and scan backward to find the latest — schema-
   free but O(N) on every `Run`; (c) store in `eventlog`'s
   overlay table — durable and queryable but eventlog-only
   (in-memory service wouldn't have it). Recommend (b) for v1;
   the scan is bounded by post-compaction history length, which
   is by definition small.
2. **Which compactor prompt template ships in core-agent?**
   We propose the five-section handover above. Consumer override
   via `WithCompactor` lets a coder-focused agent (cogo) add a
   sixth "current diff state" section, etc. Locking the default
   here.
3. **Threshold default.** 0.85 is the proposal; could go lower
   (0.75 = more headroom, more frequent compactions) or higher
   (0.90 = less compaction, riskier on long turns). Implementer
   picks based on real-session measurements.
4. **Auto-approve scope inside subtasks.** Default to
   read-only-only. Need to enumerate the canonical "read-only"
   tool set: `read_file`, `glob`, `grep`, `list_dir`,
   `fetch_url`, `web_search`. Anything else (`bash`, `edit_file`,
   `write_file`, MCP-provided tools) gates normally.
5. **Subtask model resolution.** `SubtaskSpec.Model` is optional;
   when nil, we re-use the parent's model. The wrappers (`Agentic*`)
   should default to a smaller model for cost — but "smaller" is
   provider-dependent. Recommend: `AgenticToolOpts.SmallModelID`
   is required (no silent default), so the consumer picks
   explicitly.

## Out of scope (track separately if needed)

- **Episodic memory with vector retrieval.** A future PR could add
  `WithEpisodicStore(EpisodicStore)` that hooks into the summarizer
  to also index summaries for later RAG; out of scope for now.
- **Pre-emptive truncation of historical tool outputs in the
  event log itself.** Tempting (would shrink replay cost too) but
  destroys audit fidelity. Stay slicing-at-request-time.
- **Cross-subtask coordination (`send_message` between peer
  subtasks).** Not needed for the synchronous shape; if we ever
  add asynchronous subtask graphs we'll revisit.
- **Token-level "weeding" of intermediate payloads.** Subsumed
  by compaction; explore only if compaction proves insufficient.
- **Viewport-aware prompt injection (operator's current file
  focus injected as `<viewport_focus>` XML).** Requires TUI ↔
  agent coupling that conflicts with our "agent is the substrate,
  TUI is one consumer" architecture.
- **Built-in explore-plan-execute or skeptical-critic drivers.**
  These are pipeline shapes consumers compose from the primitives
  this doc ships; no need for a built-in orchestrator.

## Pointers

- `agent/btw.go` — `AskSideQuestion` is the prior art for
  tool-less, session-history-aware model calls (the same shape
  both A's summarizer and B's `RunSubtask` build on).
- `agent/background.go` + `agent/background_spawn.go` — the
  existing long-running-subagent plumbing that we deliberately
  do not reuse for micro-subtasks (different shape).
- `usage/tracker.go` — the cost-attribution surface that gets the
  subtask rollup in PR III.
- `cmd/core-agent/coretui_enabled.go:contextWindowSizeFor` — the
  hardcoded model → window-size table added on the core-tui
  adapter branch; the compaction trigger reads this.
- `docs/operator-input-design.md` layers B + D for the existing
  inbox + subagent surface this doc complements.
