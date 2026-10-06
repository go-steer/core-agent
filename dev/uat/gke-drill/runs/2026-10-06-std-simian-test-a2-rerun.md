# A2 rerun after #1258 — 2026-10-06 · std-simian-test

This reruns the fault-injection batch that failed A2 earlier the same day
(`-a2-fault.md`), on a build where per-turn guardrail cuts and turn errors
are durable eventlog rows (#1258, PR #1260). It is graded the way #1258
says A2 must be graded: on **server-side replays of every session in the
window**, not on the drill's live capture.

| | |
|---|---|
| daemon | `main-7e3f648`, read-only config, fresh session DB; `--max-turn-cost-usd=0.01` for the batch only, then removed (args verified) |
| runs | drill A twice: `20261006T193336Z-a`, `20261006T193805Z-a` |
| daemon log | `~/.gke-drill/runs/a2-rerun-20261006/daemon.log`, from the patched pod's start |
| replays | `replay_sessions.sh --since 2026-10-06T19:33:29Z`, which found three sessions (both runs' incidents, including run 2's second session) and skipped the idle `default` session |
| window | `2026-10-06T19:33:29Z` → `19:42:16Z` (`window.env`) |

## Result

```
a2_count.py --log daemon.log --events replays/replay-*.sse \
    --since 2026-10-06T19:33:29Z --until 2026-10-06T19:42:16Z
```

| class | daemon log | transcript | verdict |
|---|---|---|---|
| provider retry | 0 | 0 | NOT EXERCISED |
| guardrail trip | 6 | 6 | **PASS** |
| turn error | 6 | 6 | **PASS** |

**Exit 2, "not closable":** nothing was log-only, but the retry class was
never exercised.

- **Same batch, before and after the fix:** the morning run gave 7 vs 3 on
  live captures and 0 on replay. Here every cut and every turn error the
  daemon logged is in a transcript a later reader can fetch.
- **The window didn't change the result:** it dropped 3 transcript events
  and 29 log lines (the pod's boot lines). Grading the same replays without
  the window gives the same 6 = 6 on both classes.
- **The remaining gap is the retry class.** No run on 2026-10-06 produced a
  429. The retry path's transcript surfaces (#1206) are unit-tested, and the
  2026-09-13 batch logged 13 real retries, but none has yet been counted
  live on a #1206+ build.
