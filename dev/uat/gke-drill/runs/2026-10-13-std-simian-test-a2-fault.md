# A2 fault-injection batch — 2026-10-13 · std-simian-test · 3.0.0-dev.1

Box A2 (#1042): for one run, no failure class has more entries in the daemon
log than in the transcripts. This repeats the 2026-10-06 rerun's method
(`-a2-rerun.md`) on the first 3.0 pre-release. That rerun was the run that
showed #1258's durable cut / turn-error rows working. This one checks they
survived ADK v2 (#1291), the `/v3` move (#1312) and the core-models provider
swap (#1296).

| | |
|---|---|
| daemon | `ghcr.io/go-steer/core-agent:3.0.0-dev.1`, read-only config (`config.hub.json`), existing session DB (not fresh, unlike 10-06's rerun: 17 sessions on the hub before run 1) |
| fault | `--max-turn-cost-usd=0.01` appended to the container args by JSON patch at `09:18:32Z` (`a2-ceiling.json`). Removed at `09:28:56Z` by a `test`-then-`remove` patch on `args/5` (`a2-unceiling.json`). Args verified back to the original five |
| watcher | lookout `v0.27.0` (the operator's record) |
| runs | drill A twice: `20261013T091913Z-a`, `20261013T092506Z-a` |
| daemon log | `~/.gke-drill/runs/a2-fault-v3-dev1/daemon.log`, from the patched pod's start (`09:18:49Z`) to its SIGTERM (`09:28:57Z`) |
| replays | `replay_sessions.sh --since 2026-10-13T09:18:32Z --include-idle` → `replays/` (4 sessions, including the empty `default`) |
| window | `2026-10-13T09:18:32Z` → `09:28:56Z`, the fault's own start and end |
| scorer | grading subagent, re-ran `a2_count.py` from the artifacts, 2026-10-13. **Not countersigned** |

## Result

```
a2_count.py --log daemon.log --events replays/replay-*.sse \
    --since 2026-10-13T09:18:32Z --until 2026-10-13T09:28:56Z
```

Re-run for this sheet. The output is in `a2-fault-a2_count-windowed.txt` beside it.

| class | daemon log | transcript | verdict |
|---|---|---|---|
| provider retry | 0 | 0 | NOT EXERCISED |
| guardrail trip | 3 | 3 | **PASS** |
| turn error | 2 | 2 | **PASS** |

**Exit 2, "not closable":** nothing was log-only, but the retry class was
never exercised. This is the same shape as the accepted 2026-10-06 rerun.
Without the window (`a2-fault-a2_count-unwindowed.txt`) it is **4 = 4** on both
classes, also exit 2.

Every cut and turn error, by session. The log-side and replay-side counts agree row for row:

| session | opened by | in a drill's live capture? | log (window) | replay rows (window) | after `09:28:56Z` (outside) |
|---|---|---|---|---|---|
| `01a1348f…` | run 1, `BackOff` | yes (run 1) | trip `09:22:35` ($0.1064, cut) + turn error; trip `09:23:08` ($0.0103, `halted_turn: false`) | 593 trip, 594 turn-error, 597 trip | — |
| `01a13494-b3b2…` | run 2, `BackOff` | **no** | trip `09:26:45` ($0.0542, cut) + turn error | 644 trip, 645 turn-error | 651 turn-error: an auto-continue turn killed by the SIGTERM, no `cut_by` |
| `01a13494-b5b6…` | run 2, `degradation.capacity` | yes (run 2), but its cut came after capture ended | — | — | 649 trip ($0.0593) + 650 turn-error, logged at the SIGTERM instant |

The window drops 3 log lines (the `09:28:57` trip and two `terminated signal received` lines) and 15 transcript events. The two runs' trips and errors after the window come from the un-ceiling restart, not from the fault. The window is what keeps them out.

## The replay selection (record it)

The first replay pass ran **without** `--include-idle` and skipped two
in-window sessions as `idle`: run 1's incident session (`01a1348f…`) and run 2's
`BackOff` session (`01a13494-b3b2…`). Graded on that pass, the transcript side
would have been short by the very rows A2 is about. The second pass, with
`--include-idle`, is what `replays/selection.tsv` now shows. The first pass's
manifest was overwritten, so this paragraph is the record of it.

Why they read as idle: both sessions' `last_touched_at` equals their creation time
(`09:19:24Z`, `09:25:22Z`). The cut, the turn error and b3b2's `09:28:49Z`
auto-continue did not advance it. By the second pass, `01a1348f…` was
`active` again (a watcher `resolved` inbox woke it on the new pod at
`09:29:49Z`), but `b3b2` was still `idle`. `replay_sessions.sh`'s own header
warns that reading an idle session lazily resumes it, and a resume can start an
auto-continue turn. Nothing in b3b2's 20 s replay tail shows one (its last row is
651 at `09:28:57Z`). The new pod's log was not captured, so that is not proven.

## What else this batch showed

- **#1258's durable rows survived the 3.0 build.** Each cut is an
  `agent/guardrail-turn-trip` row plus an `agent/turn-error` row
  (`cut_by: cost_ceiling`). They are on server-side replay, and a reader
  fetching the session later gets them.
- **The live capture would have under-counted again.** Run 1's drill saw 2
  trips (its `evidence.md` G5 is FAIL with 2 signals). Run 2's drill captured
  the *capacity* session `b5b6`, not the `BackOff` session `b3b2` that tripped.
  Its `evidence.md` shows G5 PASS with 0 signals. `drill.sh` declared that capture
  done after "stream quiet for 90s" while the parent's `spawn_agent` was still
  pending ("1 never got a response").
- **The ceiling overshoots by the length of a subagent run.** The per-turn
  ceiling is $0.01, and the cuts landed at $0.1064, $0.0542 and $0.0593. Each cut
  fires when `spawn_agent` *returns* to the parent (seq 592 → 593, 643 → 644),
  so the child's whole spend lands inside the turn before anything checks it.
  That is consistent with "a per-turn trip ends its turn only" (#1049), but on a
  delegating recipe the ceiling is a soft bound, which 0.01 makes very visible.
- **A stuck child, cause unknown.** In `b5b6` the `cluster` child called
  `return_result` at `09:26:56Z` (seq 646) and got its `ack` (647). Then nothing:
  the eventlog's next seq anywhere is 648 (b3b2's auto-continue note) and then
  649 (b5b6's trip at the SIGTERM), so the child wrote no closing text, and the
  parent's `spawn_agent` stayed open for two minutes. On the new pod, auto-continue repaired the dangling call
  (`tool_tail_repair`, seq 652) and re-drove the session. In run 1 the same
  step took 6 s (`09:22:29` → `09:22:35`). One instance, under a cost ceiling,
  so it may be an artefact of the fault. It is worth a look before the soak,
  because a parent blocked on a child that has already returned is a wedge,
  and A2's `wedged session` class is NOT COUNTABLE here.
- **Cosmetic:** `Event from an unknown agent: agent/turn-error` /
  `agent/guardrail-turn-trip` lines appear when a session with durable rows
  resumes. That is ADK's runner not recognising the synthetic authors, the
  same noise `docs/eventlog-decisions.md` records for checkpoints. `a2_count`
  does not count them, which is correct.
- **The retry class is still countable on 3.0.** The core-models swap gives
  Gemini a library policy with `MaxRetries = 0` (`pkg/models/gemini/gemini.go`
  `geminiRetry`). Transient retries still go through `pkg/models/retry.go`,
  whose `transient provider error … retrying once after` line is what
  `a2_count` matches. So "0 in the log" means no retry happened, not that the
  regex went blind.

## What closing A2 still needs

Unchanged from 2026-10-06: **a provider retry** counted live on a #1206+
build, which needs a real 429. This batch adds nothing toward it.
