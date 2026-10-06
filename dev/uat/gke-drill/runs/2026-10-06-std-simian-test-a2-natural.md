# A2 natural batch — 2026-10-06 · std-simian-test

Box A2 (#1042, wording settled 2026-10-04): for one run, no failure class has
more entries in the daemon log than in the transcripts. This was the first
batch on a build that has #1206 (`main-24f2ae3`).

| | |
|---|---|
| runs | `20261006T091637Z-a`, `20261006T092035Z-b`, `20261006T092406Z-c` (read-only deployment) |
| daemon log | `~/.gke-drill/runs/a2-batch-20261006/daemon-natural.log` (captured for the whole batch) |
| grader | `a2_count.py` with `--subagent-events <run>/subagents.json` for each run |

| class | daemon log | transcript | verdict |
|---|---|---|---|
| provider retry | 0 | 0 | NOT EXERCISED |
| provider retry (side call) | — | — | NOT COUNTABLE (0 in the log) |
| guardrail trip | 0 | 0 | NOT EXERCISED |
| turn error | 1 | 1 | **PASS** |
| failed delegation | — | 0 | NOT COUNTABLE |
| wedged session | — | — | NOT COUNTABLE (graded by `soak_verdict.py`) |

**A2: not closable on this batch (exit 2).** Nothing was log-only. Clean runs
don't produce retries or guardrail trips, so those two classes were never
exercised.

The one turn error is real: B's follow-up died on a bare
`400 INVALID_ARGUMENT`, filed as #1247. It was counted once on each side.

## What closes A2

The two unexercised classes need failures to happen:
- **Guardrail trip and turn error:** a fault-injection run with a tight
  `--max-turn-cost-usd` on the daemon. The session's auto-mode permission
  classifier refused that patch to the live deployment as a shared-cluster
  mutation. It needs a maintainer-granted permission rule before it can run.
- **Provider retry:** needs real 429s. Load can make them likely but can't
  force one. The 2026-09-13 batch had 13 retries under a 20-run load.
