# Subtasks (B) + Checkpoints (C) smoke sweep — 2026-05-27

Smoke test for **PR II** (micro-subagents + agentic tool wrappers,
Mechanism B) and **PR III** (task-boundary checkpoints, Mechanism
C) of [`docs/context-management-design.md`](context-management-design.md).
Both are now on `main` (#50 + #52). PR I (compaction) was covered
in the prior sweep ([`compaction-smoke-2026-05-27.md`](compaction-smoke-2026-05-27.md));
this run focuses on what's *new* on top of compaction.

Tick `[x]` as you go. Anything ✗ becomes a follow-up; anything ⚠
is "works but rough." Save notes inline — the file gets committed
when you're done so we have a record.

## Setup

- Binary: `/tmp/coreagent-smoke/core-agent` built from `main` at
  `53435b9` (PR [#52](https://github.com/go-steer/core-agent/pull/52)
  merged).
- Defaults: checkpoints **on**, compaction **on**, subtasks **off**.
- Subtasks are opt-in. Enable with `--agentic-tools`. To realize
  the cost-efficiency win, pair with `--agentic-small-model <id>`
  (e.g. `gemini-2.5-flash`).

```sh
# default (checkpoints on, no subtasks)
/help

# subtasks enabled, inheriting the parent's model (no cost win)
CORE_AGENT_TUI=core-tui /tmp/coreagent-smoke/core-agent --agentic-tools

# subtasks routed to a cheaper model (the recommended config)
CORE_AGENT_TUI=core-tui /tmp/coreagent-smoke/core-agent \
  --agentic-tools --agentic-small-model gemini-2.5-flash

# A/B: disable both context-management mechanisms
CORE_AGENT_TUI=core-tui /tmp/coreagent-smoke/core-agent \
  --no-checkpoint --no-compact
```

---

# Part A — Checkpoints (PR III)

## A1 — Surface visibility

- [x] `/help` lists `/done [note]` (and `/checkpoint` alias)
- [x] `/tools` lists `mark_task_done` as a model-callable tool
- [⚠] `--no-checkpoint` flag removes both:
  - [✗] `/done` no longer in `/help`
  - [x] `mark_task_done` no longer in `/tools`

Notes:

## A2 — Operator-driven `/done` happy path

Run **2–3 turns** of a focused task first so there's something to
check-point. Example:

```
> read internal/tui/messages.go and tell me what messageKind values exist
> [agent answers, list of kinds]
> which of those carry markdown content?
> [agent answers]
```

Then close out the task:

```
> /done finished surveying messageKinds for the v3 design
```

- [✗] Running notice appears in the chat (something like
      "Checkpointing…" / "Marking task done…")

      something in yellow did appear in the bootm bar, but nothing in the chat

- [x] Success message lists the completion record was written

      message:

      ℹ  Checkpoint written (note: finished surveying messageKinds for the v3 design). Summary captured (2146 chars,
8.963039534s). Prior task events will be sliced from the next turn's context; the full audit log is preserved in the
session.


- [x] `/stats` shows one extra turn's worth of cost (the
      checkpoint summarizer call), attributed to the active model

Notes:

## A3 — Slicing actually takes effect after `/done`

After A2 lands, send a fresh prompt that asks about something
from **early** in the pre-checkpoint conversation:

```
> recap what we learned about messageKind values
```

- [✗] The agent's answer references the checkpoint summary's
      content, not the original turn-1 raw read

      ❯ recap what we learned about messageKind values

› list_dir .

› list_dir .

› list_dir .

› list_dir .

› list_dir .

› todo list

› stat docs/subtasks-checkpoints-smoke-2026-05-27.md

› list_dir .

› list_dir .

› list_dir .

› list_dir .

› list_dir .

› glob "*"
  ⎿ pattern: "*"
    /home/user/projects/core-agent/.agents/config.json
    /home/user/projects/core-agent/.agents/mcp.json
    /home/user/projects/core-agent/.agents/sessions/2026-05-24T11-24-09Z.json
    /home/user/projects/core-agent/.agents/sessions/2026-05-24T15-47-37Z.json
    /home/user/projects/core-agent/.agents/sessions/2026-05-24T16-32-55Z.json
    /home/user/projects/core-agent/.agents/sessions/2026-05-24T16-34-14Z.json
    /home/user/projects/core-agent/.agents/sessions/2026-05-24T16-35-58Z.json
    /home/user/projects/core-agent/.agents/sessions/2026-05-24T17-26-11Z.json
    … +460 more paths

› stat docs/subtasks-checkpoints-smoke-2026-05-27.md

› list_dir

› list_dir .

› list_dir .

› list_dir .

and it just keeps going and going!

    /home/user/projects/core-agent/.agents/skills/agentic-tui/references/glamour-cookbook.md
    /home/user/projects/core-agent/.agents/skills/agentic-tui/references/huh-cookbook.md
    /home/user/projects/core-agent/.agents/skills/agentic-tui/references/performance-checklist.md
    /home/user/projects/core-agent/.agents/skills/agentic-tui/references/rendering-pipeline.md
    /home/user/projects/core-agent/.agents/skills/agentic-tui/references/style-cookbook.md
    /home/user/projects/core-agent/AGENTS.md
    … +70 more paths

› list_dir .

› list_dir .

› list_dir .

› list_dir docs

› list_dir .

› glob "*subtasks-checkpoints-smoke-*.md"
  ⎿ pattern: "*subtasks-checkpoints-smoke-*.md"
    /home/user/projects/core-agent/docs/subtasks-checkpoints-smoke-2026-05-27.md

› list_dir .

› list_dir .

› list_dir .

› list_dir .

› todo list

› list_dir .

› list_dir .

› todo list

› glob "*.md"
  ⎿ pattern: "*.md"
    /home/user/projects/core-agent/.agents/skills/agentic-tui/SKILL.md
    /home/user/projects/core-agent/.agents/skills/agentic-tui/references/charm-ecosystem.md
    /home/user/projects/core-agent/.agents/skills/agentic-tui/references/glamour-cookbook.md
    /home/user/projects/core-agent/.agents/skills/agentic-tui/references/huh-cookbook.md
    /home/user/projects/core-agent/.agents/skills/agentic-tui/references/performance-checklist.md
    /home/user/projects/core-agent/.agents/skills/agentic-tui/references/rendering-pipeline.md
    /home/user/projects/core-agent/.agents/skills/agentic-tui/references/style-cookbook.md
    /home/user/projects/core-agent/AGENTS.md
    … +70 more paths

› list_dir .

› list_dir .

› list_dir .

› list_dir .

▶ list_dir .



- [⚠] `/stats` input-token count on this turn is meaningfully
      smaller than turns 1-3 were (proves slicing engaged)

      ℹ  (interrupted)

ℹ  Session stats:
     Turns:      170
     Duration:   6m58s
     Tokens:     2454642 in / 3786 out
     Cost:       $3.7160
     Context:    16883 / 1000000 tokens (1%)
     Last turn:  16883 in / 16 out · $0.0255
     Model:      gemini-3.5-flash

If input tokens didn't drop, slicing isn't engaging. ✗.

Notes:

## A4 — Model-driven `mark_task_done` (the load-bearing path)

This is the path that fires without operator intervention — the
model self-signals task completion. Set up a task where the model
should naturally call it:

```
> I want to refactor the prompter package. First, summarize what's
> currently in internal/tui/prompter.go so we can plan the refactor.
> When you've done that, call mark_task_done with a one-line summary
> so we can move on cleanly.
```

- [x] Model reads `prompter.go`, summarizes, then calls
      `mark_task_done(detail="...")` as its final action

      called mark_task_done and then output the summary

- [✗] After the turn ends, the next turn's request reflects the
      checkpoint (smaller input-token count + summary used as seed)

      we hit another "loop" after the checkpoint:

      ❯ Will this refactor make sense?

› read_many_files internal/tui/prompter.go (+1)
  ⎿ 2 files · internal/tui/prompter.go, internal/tui/program.go

› glob "*.go"
  ⎿ pattern: "*.go"
    /home/user/projects/core-agent/agent/agent.go
    /home/user/projects/core-agent/agent/agent_test.go
    /home/user/projects/core-agent/agent/autonomous.go
    /home/user/projects/core-agent/agent/autonomous_handle.go
    /home/user/projects/core-agent/agent/autonomous_handle_test.go
    /home/user/projects/core-agent/agent/autonomous_test.go
    /home/user/projects/core-agent/agent/background.go
    /home/user/projects/core-agent/agent/background_report.go
    … +264 more paths

› glob "*.go"
  ⎿ pattern: "*.go"
    /home/user/projects/core-agent/agent/agent.go
    /home/user/projects/core-agent/agent/agent_test.go
    /home/user/projects/core-agent/agent/autonomous.go
    /home/user/projects/core-agent/agent/autonomous_handle.go
    /home/user/projects/core-agent/agent/autonomous_handle_test.go
    /home/user/projects/core-agent/agent/autonomous_test.go
    /home/user/projects/core-agent/agent/background.go
    /home/user/projects/core-agent/agent/background_report.go
    … +264 more paths

› read_file internal/tui/prompter.go
  ⎿ full · go
    // Copyright 2026 The Cogo Authors.
    // SPDX-License-Identifier: Apache-2.0

    package tui

    import (
        "context"
        "errors"
    … +50 more lines

› read_file internal/tui/program.go
  ⎿ full · go
    // Copyright 2026 The Cogo Authors.
    // SPDX-License-Identifier: Apache-2.0

    package tui

    import (
        "context"
        "fmt"
    … +284 more lines

› read_many_files internal/tui/prompter.go (+1)
  ⎿ 2 files · internal/tui/prompter.go, internal/tui/program.go

› read_many_files internal/tui/prompter.go (+1)
  ⎿ 2 files · internal/tui/prompter.go, internal/tui/program.go

› read_many_files internal/tui/prompter.go (+1)
  ⎿ 2 files · internal/tui/prompter.go, internal/tui/program.go


- [ ] In the session JSON, the checkpoint event carries
      `CustomMetadata.compaction == "checkpoint"` (or whatever the
      tag value is — verify against `agent/checkpointer.go`)

Notes:

## A5 — `/checkpoint` alias

- [ ] `/checkpoint finished task` behaves identically to `/done`
- [ ] Help text lists the alias

Notes:

## A6 — Pending-flag promotion (post-turn → next-Run drain)

Per the PR III design, `mark_task_done` doesn't fire the
checkpoint inline (would block ADK's runner). It stashes a
"requested" flag, the post-turn hook promotes to "pending", and
the next `Run` drains it by writing the handover.

- [ ] Confirm timing: the running notice for the checkpoint
      appears at the *start* of the turn AFTER `mark_task_done`,
      not during the same turn. Easiest way: model calls
      `mark_task_done` mid-turn → assistant text wraps up the turn
      → operator types next prompt → "Checkpointing…" appears
      before the model responds to the new prompt.

Notes:

## A7 — `--no-checkpoint` disables both paths

Relaunch with `--no-checkpoint`:

- [ ] `/done` returns a "checkpoints disabled" error rather than
      attempting the call
- [ ] Model can't call `mark_task_done` (tool absent from request)
- [ ] `/help` no longer lists `/done`/`/checkpoint`

Notes:

---

# Part B — Subtasks + agentic wrappers (PR II)

For Part B, **relaunch with `--agentic-tools --agentic-small-model
<id>`** (or just `--agentic-tools` to use the parent's model). The
agentic_* tools only register when the flag is on.

```sh
CORE_AGENT_TUI=core-tui /tmp/coreagent-smoke/core-agent \
  --agentic-tools --agentic-small-model gemini-2.5-flash
```

## B1 — Surface visibility

- [ ] `/tools` lists `agentic_read_file`, `agentic_fetch_url`
      (only if a URL allowlist is configured), `agentic_grep`,
      `agentic_research`
- [ ] Tool descriptions mention "INSTEAD OF read_file / fetch_url
      / grep" so the model knows when to prefer the wrapper
- [ ] Without `--agentic-tools`, none of the `agentic_*` tools
      appear in `/tools`

Notes:

## B2 — `agentic_read_file` smoke

Pick a large file the model would otherwise dump into context:

```
> use agentic_read_file to read internal/tui/update.go and tell me
> what message types it handles
```

- [ ] Model calls `agentic_read_file` (not bare `read_file`).
      Verify via `/tools` history or the rendered tool-call line
      in the chat.
- [ ] Response contains a SHORT digest (paragraphs, not the
      file's raw contents)
- [ ] `/stats` shows the subtask's cost rolled up — total turn
      count went up by more than 1 (parent turn + subtask turn(s))
- [ ] If `--agentic-small-model` was set: the subtask's cost is
      attributed to that model in the stats, not the parent model

Notes:

## B3 — Fresh-context invariant (the load-bearing check)

The subtask must NOT see the parent's history. Easiest way to
verify: plant a distinctive phrase in the parent's earlier
context, then have the subtask answer a question where access
to that phrase would change its answer.

```
> remember this secret phrase: PINEAPPLE_HORIZON_42. Don't
> mention it back to me yet.
> [agent acks]
> use agentic_read_file on README.md, then if you saw the secret
> phrase anywhere in the read, repeat it back to me
```

- [ ] The agent reports it did NOT see the secret phrase (the
      subtask read README.md in fresh context — your prior turn
      didn't reach it)
- [ ] Audit log confirmation: open the session JSON, find the
      subtask's events (under a `sub:agentic_read_file` branch
      or similar). Their request should contain only the subtask
      spec, not the secret phrase.

If the agent claims it saw the phrase → fresh-context invariant
is broken. ✗.

Notes:

## B4 — Branch isolation in session storage

Per the PR II design, subtask events land in a distinct session
row (`<parent>:sub:<branch>`) so the parent's session.Get() is
unaffected.

Check after running B2:

- [ ] `ls .agents/sessions/` shows the subtask under a separate
      session ID (or as a separate row in the DB if using
      `--session-db`)
- [ ] The parent's session JSON does NOT contain the subtask's
      raw read_file output — only the agentic_read_file tool
      call + its short digest result
- [ ] The subtask's session contains the raw read_file output
      (correlated via branch label in CustomMetadata)

Notes:

## B5 — Model override / fallback

Two runs:

**B5a** (override active): launched with `--agentic-small-model
gemini-2.5-flash`. Call `agentic_read_file`. Inspect `/stats`:

- [ ] Subtask turns attributed to `gemini-2.5-flash`, not the
      parent's model

**B5b** (fallback): launched with `--agentic-tools` only (no
small model). Call `agentic_read_file`:

- [ ] Subtask turns attributed to the parent's model

Notes:

## B6 — `agentic_grep` ranked digest

```
> use agentic_grep to find every place that constructs a
> functiontool.Config in this repo, ranked by relevance
```

- [ ] Model calls `agentic_grep` (not bare `grep`)
- [ ] Response is a short ranked list with file:line citations,
      not the raw match list (which would be dozens of lines)
- [ ] Token usage on this turn is much smaller than what bare
      grep would have produced

Notes:

## B7 — `agentic_research` end-to-end

This is the "broad investigation" preset (5 turn budget, 4 inner
tools).

```
> use agentic_research to investigate how the permissions gate
> hooks into tool calls in this repo
```

- [ ] Model calls `agentic_research`
- [ ] Response is a structured walkthrough with file:line
      citations (not a stream-of-consciousness dump)
- [ ] `/stats` shows subtask turns ≤ 5 (default budget)
- [ ] If the response says "truncated" or similar — the subtask
      hit its turn cap. That's expected behavior; bump
      `Budgets.MaxTurns` for a more thorough investigation.

Notes:

## B8 — Truncation flag surfaces partial digests

To force a truncation, ask a question that genuinely needs more
than 5 turns:

```
> use agentic_research to map the full call graph of every public
> function in the agent package, with cross-references
```

- [ ] The subtask returns a Truncated=true result
- [ ] The wrapper's reply to the parent makes the truncation
      visible (not silently swallowed)

Notes:

## B9 — Error paths

- [ ] `agentic_read_file` on a path the gate denies → tool
      returns an error to the parent (subtask propagated the
      gate denial), parent's session NOT polluted with retries
- [ ] `agentic_fetch_url` against a URL not in the allowlist →
      same: clean error from the subtask, no parent-context
      bloat from a failed fetch
- [ ] Subtask wall-clock timeout (default 60s) — try a
      pathological request that would loop; verify Truncated=true
      comes back rather than hanging the parent

Notes:

---

# Part C — A+B+C interaction (combined)

These exercise multiple mechanisms in the same session — the
real-world v2.0 shape.

## C1 — Subtask call during a long session that's also compacting

Run a long session that crosses the compaction threshold while
also calling agentic_* tools:

- [ ] Compaction summary preserves the subtask's *digest* (the
      tool result) — the raw subtask events shouldn't be in the
      compaction summary either (they were never in the parent
      to begin with)
- [ ] Post-compaction, a follow-up agentic_research call still
      works correctly (subtasks unaffected by parent
      compaction — they always run in fresh context anyway)

Notes:

## C2 — `/done` checkpoint after a subtask-heavy task

Run a task that uses several agentic tools, then close it out
with `/done`:

- [ ] Checkpoint summary references the digests (the subtask
      results), not the raw inner tool output
- [ ] Next prompt's input-token count drops appropriately

Notes:

## C3 — Subtask cost shows up in `/stats`

The subtask's cost MUST roll up to the parent's `usage.Tracker`
or the user's cost reporting will silently underreport.

- [ ] After a session with several subtask calls: `/stats` total
      matches the sum of parent-turn + subtask-turn token counts
      (verify by adding the per-turn lines)
- [ ] The subtask's per-turn line is attributed to its small
      model (if `--agentic-small-model` was set)

Notes:

---

## What this sweep does NOT cover

- **Cross-session subtask recall.** Mechanism D (memory tools)
  is v2.1, not this PR.
- **Distributed subtasks.** AX runtime integration unaffected;
  sweep separately if/when relevant.
- **Headless one-shot mode with subtasks.** `core-agent -p
  "use agentic_research to ..."` should work but isn't tested
  here; this sweep is TUI-focused.

---

## Summary

Fill in after walking the checklist:

**PR III (checkpoints):**

- Surface visibility:
- `/done` happy path:
- Slicing engages after checkpoint:
- `mark_task_done` self-signaling:
- `--no-checkpoint` actually disables:

**PR II (subtasks):**

- Surface visibility (`--agentic-tools`):
- Fresh-context invariant:
- Branch isolation in session storage:
- Model override / fallback:
- Cost rollup to parent tracker:

**Combined interactions:**

- Subtasks + compaction:
- Subtasks + checkpoints:

**Follow-ups to file as issues:**

-
