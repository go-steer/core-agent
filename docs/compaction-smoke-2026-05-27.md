# Compaction (Mechanism A) smoke sweep — 2026-05-27

Smoke test for PR I of [`docs/context-management-design.md`](context-management-design.md)
— compaction substrate, `/compact` slash, post-turn threshold trigger,
history slicing at request-construction time.

Tick `[x]` as you go. Anything ✗ becomes a follow-up issue; anything
⚠ is "works but rough." Save your notes inline — this file gets
committed when you're done so we have a record.

## Setup

- Binary: `/tmp/core-agent-smoke` built from the `feat/compaction-pr-i`
  branch (PR [#49](https://github.com/go-steer/core-agent/pull/49)).
- Includes: `Agent.Compact`, `WithCompactor(NewDefaultCompactor())`
  wired by default in `cmd/core-agent/main.go`, `/compact [focus]`
  slash with `/summarize` alias, post-turn threshold check
  (default 0.85), history-slicing `compactingService` wrapper.
- Default config will compaction-enable automatically — pass
  `--no-compact` to disable for A/B comparison.

```sh
# default: compaction enabled
CORE_AGENT_TUI=core-tui /tmp/core-agent-smoke

# disabled (for comparison)
CORE_AGENT_TUI=core-tui /tmp/core-agent-smoke --no-compact
```

## 1 — Surface visibility

- [ ] `/help` lists `/compact [focus]` with alias `/summarize`
- [ ] `/tools` does NOT show `compact` (it's a slash, not a model-callable tool)

Notes:

## 2 — Empty-session behavior (fastest sanity check)

Launch a fresh binary. Without typing any prompts:

- [ ] `/compact` → shows "Compacting…" running notice
- [ ] Then shows: `/compact: nothing to summarize yet (empty session). Run at least one turn first.`
- [ ] No error in the chat; no LLM call attributed in `/stats`

Notes:

## 3 — Manual compaction happy path

Do this with a non-trivial conversation. Run **at least 3-4 turns** so
there's something to summarize:

```
> what are the main subsystems in this repo? brief answer
> [agent answers]
> pick one and walk me through its package layout
> [agent answers]
> what would a v3 of that subsystem look like? two paragraphs
> [agent answers]
```

Then:

- [ ] `/compact` (no focus) → running notice appears immediately
- [ ] After 1–10s, success message: `Compacted. Summary written (N chars, Xs). Prior events will be sliced from the next turn's context; the full audit log is preserved in the session.`
- [ ] `/stats` shows one extra turn's worth of cost (the compactor LLM call), attributed to whatever model is currently active

Notes:

## 4 — Slicing actually takes effect (the load-bearing check)

After the `/compact` from §3 lands, send a fresh prompt that asks
about something from **early** in the pre-compact conversation:

```
> recap what we discussed about <topic from turn 1>
```

- [ ] The agent's answer references the summary's content, not the
  original turn-1 detail (because the original was sliced)
- [ ] The next `/stats` reading shows a MUCH smaller input-token count
  than before the compact (proves slicing is engaged on the wire,
  not just visually). Compare with what input-tokens were trending at
  before the compact ran.

If the input-token count didn't drop on the next turn, the slicing
wrapper isn't engaging. That's a ✗.

Notes:

## 5 — Focus hint biases the summary

```
> /compact focus on the v3 architecture discussion
```

- [ ] Running notice shows: `Compacting (focus: focus on the v3 architecture discussion)…`
- [ ] After the compact, ask: `> what was the v3 architecture we sketched?`
- [ ] The agent's answer is detailed about the v3 thread — it should
  have been preserved disproportionately vs. the other threads.

(Subjective check — the operator judges whether the focus hint
actually shifted the summary's emphasis.)

Notes:

## 6 — `/summarize` alias works

- [ ] `/summarize` (no focus) behaves identically to `/compact`
- [ ] Help text lists alias

Notes:

## 7 — Automatic threshold trigger (best-effort)

Hard to trigger without burning real tokens. Two strategies:

**Strategy A (cheap, contrived):** Use a model with a small context
window. There aren't any 200K-or-smaller models that core-agent
supports out of the box — Claude 4.x at 200K is the smallest. To
hit 85% of 200K (= 170K input tokens), you'd need to spend that
much on prior turns. Probably not worth it for a smoke test.

**Strategy B (instrumented):** Temporarily edit `agent/compactor.go`
to set `DefaultCompactionThreshold = 0.01`, rebuild, send any
prompt, watch for auto-compact to fire on the second turn. **Revert
before shipping.**

- [ ] (skipped — not a v2.0 blocker for this sweep) Strategy A
- [ ] If you ran Strategy B: second turn shows a system message that
  the prior summary was used as the seed (slicing implicit — no
  banner, just verify via `/stats` input-token drop)

Notes:

## 8 — Audit-log integrity

The whole point of slicing at request-time (vs mutating the event
log) is that the audit trail stays complete. Verify:

- [ ] After `/compact`, the session JSON file (under `.agents/sessions/`
  or wherever your session lives) still contains the pre-compact
  events — they aren't deleted, just hidden from the next prompt.
- [ ] The summary event itself shows up in the JSON with
  `CustomMetadata.compaction == "summary"`
- [ ] If a focus hint was used, `CustomMetadata.compaction_focus`
  carries it

Use a sidebar shell:

```sh
ls -lt .agents/sessions/ | head -3
python3 -c "import json; d=json.load(open('.agents/sessions/<latest>.json')); print(json.dumps([m for m in d['messages']], indent=2))" | head -40
```

Notes:

## 9 — `--no-compact` actually disables auto-trigger

- [ ] Relaunch with `--no-compact`
- [ ] `/compact` still works as a manual command (the flag only
  turns off the automatic trigger)
- [ ] Even after many turns / high token usage, no auto-compact
  fires (verify by watching for the "compacting…" pre-turn message
  that the next-Run drain would surface — it shouldn't appear)

Notes:

## 10 — Error paths

- [ ] `/compact` while the model provider is unreachable (turn off
  network or use a wrong API key) → error surfaces as
  `/compact failed: <reason>` rather than hanging or panicking
- [ ] Killing the agent mid-compact (Ctrl+C) → next launch sees no
  half-written summary event (we don't write until the model
  returns a full text)

Notes:

## What this sweep does NOT cover

- **Subagent flow.** Mechanism B (`RunSubtask` + agentic tool
  wrappers) is task #92, not in this PR. Compaction works fine
  for parents even without B; tested independently.
- **Cross-session compaction recall.** Mechanism D (memory) is
  v2.1.
- **Distributed-runtime compaction.** AX integration unaffected
  by this PR; sweep separately if/when relevant.

## Summary

Fill in after walking the checklist:

- **Items passing:** _ / 22
- **Items failing (with notes):**
- **Items rough but functional (⚠):**
- **Verdict on PR #49:** ☐ ship / ☐ block / ☐ ship with follow-up
