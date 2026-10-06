# A2 fault-injection batch — 2026-10-06 · std-simian-test

Box A2 (#1042): for one run, no failure class has more entries in the daemon
log than in the transcripts. The natural batch (`-a2-natural.md`) exercised
only turn errors. This batch injects a fault so guardrail trips happen too.

| | |
|---|---|
| daemon | `main-24f2ae3`, read-only config (`config.hub.json`), plus `--max-turn-cost-usd=0.01` for the batch only, removed afterwards (args verified back to normal) |
| runs | drill A twice: `20261006T173240Z-a`, `20261006T173629Z-a` |
| daemon log | `~/.gke-drill/runs/a2-fault-20261006/daemon.log` (the whole batch) |
| replays | `replay-<sid>.sse`: each session's `events?since=0`, read after the runs |

## Result: A2 FAILS, on a real visibility gap (#1258)

`a2_count.py` over the drill's live captures:

| class | daemon log | transcript | verdict |
|---|---|---|---|
| provider retry | 0 | 0 | NOT EXERCISED |
| guardrail trip | 7 | 3 | **FAIL** |
| turn error | 7 | 3 | **FAIL** |

Every cut in the log, by session:

| session | run | cuts logged | in the drill's live capture | on server-side replay |
|---|---|---|---|---|
| `01a11246…` | 1 | 3 | 2 | **0** frames; one durable **halt** marker (3 cuts in a row halted the session) |
| `01a11249-f329…` | 2 | 2 | 1 | **0** frames, no durable record |
| `01a11249-f131…` | 2 | 2 | not captured | **0** frames, no durable record |

**The cause is in the product, not the rig.** A per-turn cost cut, and the
turn error it ends the turn with, are delivered only as live SSE frames to
whoever is attached at that moment.
- Nothing persists them, and a replay never re-emits them.
- The only durable guardrail row is written when a guardrail *halts the
  session*, so a restarted pod can restore the halt.
- An operator reading the session afterwards, or attaching a minute late,
  sees a turn that just stopped, and only `kubectl logs` says why.
- The 3 the grader did find were frames the drill happened to be attached
  for. The natural batch's turn-error PASS rests on the same live-only frame.

**A rig gap on top of that.** Run 2's incident opened two sessions, and
`drill.sh` captured one. #1258 records both.

## What closing A2 now needs

1. **#1258:** persist each per-turn guardrail cut and each turn error durably,
   so replay and late attaches carry them.
2. **Grade on server-side replays** of every session the incidents opened,
   not on the drill's live capture window.
3. **A provider retry:** still needs a real 429, which no run today produced.
