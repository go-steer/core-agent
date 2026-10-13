# A2 natural batch — 2026-10-13 · std-simian-test · 3.0.0-dev.1

Box A2 (#1042): for one run, no failure class has more entries in the daemon
log than in the transcripts. This is the A/B/C batch on the first 3.0
pre-release, with no fault injected.

| | |
|---|---|
| daemon | `ghcr.io/go-steer/core-agent:3.0.0-dev.1`, read-only config (`config.hub.json`) |
| watcher | lookout `v0.27.0` (the operator's record) |
| runs | `20261013T085014Z-a`, `20261013T085922Z-b`, `20261013T090606Z-c` |
| daemon log | `~/.gke-drill/runs/a2-v3-dev1/daemon.log`, from the pod's start (`08:49:40Z`, just before A) to `09:09:40Z` (the end of C), captured at `09:11Z` |
| grader | `a2_count.py` with the three runs' `events.sse` and `--subagent-events <run>/subagents.json`, re-run for this sheet (`a2-natural-a2_count.txt` beside it) |
| scorer | grading subagent, 2026-10-13. **Not countersigned** |

| class | daemon log | transcript | verdict |
|---|---|---|---|
| provider retry | 0 | 0 | NOT EXERCISED |
| provider retry (side call) | — | — | NOT COUNTABLE (0 in the log) |
| guardrail trip | 0 | 0 | NOT EXERCISED |
| turn error | 0 | 0 | NOT EXERCISED |
| failed delegation | — | 0 | NOT COUNTABLE |
| wedged session | — | — | NOT COUNTABLE (graded by `soak_verdict.py`) |

**A2: not closable on this batch (exit 2).** No class was exercised at all.

## What the log contains

37 lines. 30 are the pod's boot lines (`08:49:40Z`). 5 are `session created`.
2 are `auto-continue: a turn is already running; standing down`, one for B's
incident session and one for C's second session. There is **no failure line of any class**: no
retry, no guardrail, no `turn:` error. The context-cache `INVALID_ARGUMENT`
the approval-gate rig logs is absent too.

## What that does and does not show

**It does show** that across three incident turns, three follow-ups and their
subagent runs (9 + 13 + 14 tool calls), the 3.0 build produced no turn error,
no cut and no retry on the live cluster. That includes 2026-10-06's B shape,
a bare `400 INVALID_ARGUMENT` on the follow-up turn (#1247). It did not
recur. The two sides agree trivially at zero, and nothing was log-only.

**It does not show** much about A2 itself. A2 asks whether failures that happen
are visible in transcripts, and with no failures there is nothing to compare.
In particular:
- **It is not evidence for the core-models provider path's error surfaces.**
  Zero errors means the swap's turn-error and retry plumbing was never
  exercised on this batch. The fault batch (`-a2-fault.md`) covers trips and
  turn errors on the same build. Nothing today covers retries.
- **The transcript side is incomplete, and it did not matter.** The log shows 5
  sessions created in the window. `drill.sh` captured 3 of them. B's
  `01a1347d-91f6…` (`09:00:07Z`) and C's `01a13484-2ec7…` (`09:07:20Z`)
  were opened during those runs and never captured. With zero failure lines
  in the log, nothing could have been log-only. On a batch with failures, that
  gap is exactly where #1258's lesson says they hide. Grade on
  `replay_sessions.sh --include-idle` output, not on the drills' live
  captures.
- **Zero is one batch, not a rate.** Three runs are not enough to say #1247 is
  gone. The 2026-10-06 natural batch had 1 turn error in the same three runs.

## What closes A2

Unchanged: a provider retry counted live on a #1206+ build. The fault batch
shows the other two classes passing on 3.0.0-dev.1.
