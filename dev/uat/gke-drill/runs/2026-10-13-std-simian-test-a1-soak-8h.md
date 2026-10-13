# A1 soak — 2026-10-13 · std-simian-test · 8 hours unattended on v3.0.0-dev.1

**Box A1 of [#1042](https://github.com/go-steer/core-agent/issues/1042), re-run on the first 3.0 build.**
The verdict is `soak_verdict.py`'s exit status (box A5), not a reading.

A1 was met on 2026-10-03 on `main-f1befc7`, an ADK v1 build. This run asks the same four questions of
`v3.0.0-dev.1`. That build has ADK v2 ([#1291](https://github.com/go-steer/core-agent/pull/1291)), the
`/v3` module path ([#1312](https://github.com/go-steer/core-agent/pull/1312)) and the core-models provider
swap ([#1296](https://github.com/go-steer/core-agent/pull/1296)). Same cluster, recipe, content image
(`:v5`), workload and cadence. It's also the same lowered compaction threshold, for the reason the
2026-10-03 note gives: these sessions plateau in the low 20Ks and never reach the 850K default.

|  |  |
|---|---|
| date (UTC) | 2026-10-13, 14:18:29 → 22:18:45, **8h 00m, zero human touches** |
| cluster | `std-simian-test` (`gke-demos-345619`, us-central1) |
| daemon image | `ghcr.io/go-steer/core-agent:3.0.0-dev.1` |
| watcher | `ghcr.io/go-steer/lookout:v0.29.0` (the recipe pin; 2026-10-03 ran v0.27.0) |
| compaction | `--compaction-threshold=0.021`, i.e. **22,020 tokens** on `gemini-3.7-flash`'s 1,048,576-token window. Added to the live Deployment's args with a JSON patch and removed the same way after the run, guarded by a `test` op. The daemon's boot line afterwards reads `fires at 0.85 … = 891289 tokens`. |
| cadence | incident every 1800s, held ~10 min, sample every 120s: 16 incidents, 232 samples |
| sessions | 17 touched in the window; 256 turns between the first and last sample |
| pod | **0 restarts**, `ready 1/1` at every sample, memory 38–66 Mi |
| cost | about **$1.86** in-window: the 17 sessions' cumulative cost at the last sample minus the first |
| run dir | `~/.gke-drill/soak/20261013T141824Z-std-simian-test` |

## The verdict

```
dev/uat/gke-drill/soak_verdict.py --run-dir ~/.gke-drill/soak/20261013T141824Z-std-simian-test

| survived   | PASS | 8.00h to the end row, no pod restart |
| no wedge   | PASS | no session halted or tripped; 16 of 16 observed incidents got a response |
| compaction | PASS | 16 session(s) crossed 22,020 tokens and were compacted; 4 more crossed and never took another turn |
| no drops   | PASS | the daemon log records no dropped inbox input (outside 1 blind window(s)) |

A1: closable on this run.   (exit 0)
```

The compaction threshold was read from the daemon's own boot line (`daemon compacts at 22020 tokens`), not
passed by hand. The one blind window is a ~6 s log-stream reconnect at 18:20:33.

**Box A1 holds on v3.0.0-dev.1.**

## A2 on the same run: the retry class, exercised naturally

The fault batch earlier the same day ([2026-10-13 A2 fault](2026-10-13-std-simian-test-a2-fault.md)) passed
guardrail trips (3 = 3) and turn errors (2 = 2), but it could not produce a provider retry. This soak hit
four natural Vertex 429s on `gemini-3.7-flash`. Three recovered on retry and one persisted and was
surfaced.

```
a2_count.py --log daemon.log --events replays/replay-*.sse replays-extra/replay-*.sse \
    --subagent-events replays/sub-*/subagents.json \
    --since 2026-10-13T14:18:29Z --until 2026-10-13T22:18:45Z

| provider retry | 4 | 4 | 0 | PASS |
| guardrail trip | 0 | 0 | 0 | NOT EXERCISED |
| turn error     | 0 | 0 | 0 | NOT EXERCISED |
```

**Every A2 class has now passed on v3.0.0-dev.1, across the two batches.**

The first grading read **4 vs 2, FAIL**, and the 2 was the rig's fault, not v3's.
- **The two "missing" retries:** parent calls in two sessions left on the daemon from the morning,
  `01a1357d-9e13` (429 at 14:19:12) and `01a1353d-4d27` (429 at 14:33:47). Cloud Trace's 429 spans located
  them.
- **They were stamped all along.** Both sessions turned inside the window and both carry the
  `provider_retry` stamp (`attempts: 2`) on the recovering response, within 15 ms of each "recovered" log
  line.
- **Why they were missed:** `replay_sessions.sh` selects by `last_touched_at`. The daemon advances that only
  when a client touches a session, not when it drives a turn itself (auto-continue, a background report, a
  wake). Both sessions turned with a `last_touched_at` from before the window, so neither was replayed.
- **The fix:** [#1324](https://github.com/go-steer/core-agent/pull/1324) adds `--every-session`, which
  still requires and records the window. The two sessions were fetched by ID into `replays-extra/` for this
  grading.
- **This rig error is one-sided:** it can only turn a PASS into a FAIL. A probe through the real Gemini
  facade (429 once, then a streamed answer) stamped the aggregate. Every side call is marked with
  `AsSideCall`.

**Open product question:** should `last_touched_at` advance on turns the daemon drives itself? Today an
operator listing sessions by recency can't see that a session ran while nobody was attached.

## This was the third attempt, and the first two are not evidence

This is recorded so nobody reuses those run dirs.

1. **09:58, `20261013T095849Z-…`.** The run started at the default 0.85 threshold, which no session can
   reach. It was stopped with SIGTERM to add the flag.
2. **10:03, `20261013T100347Z-…`.** The first run had **not** stopped. `soak.sh`'s
   `trap soak_cleanup EXIT INT TERM` ran the cleanup on SIGTERM and then resumed the main loop. The cleanup
   begins with `trap - EXIT INT TERM`, so it had also disarmed itself. For four hours two soaks broke
   `emailservice` about 4.5 minutes apart. Their one-revision `rollout undo` restores undid each other and
   left the Deployment's spec on the bad image *and* the 8Mi limit (revisions 259/260). It looked healthy
   only because the old ReplicaSet's pod was still serving. A double "restoring after scenario B" at 10:39
   and 10:44 was the visible clue. It was found at 14:15, and both runs were killed with SIGKILL.
   `emailservice` was rolled back to revision 250 (`v0.10.5`, 128Mi), the namespace checked, and this run
   started at 14:18. The bug is fixed in
   [#1323](https://github.com/go-steer/core-agent/pull/1323): INT and TERM now only exit, EXIT runs the
   cleanup once, and a self-test signals real bash.

**Still open:** nothing stops two soaks running against one cluster at once. A lock would turn this
incident into a refusal to start.

## Evidence

- `timeline.jsonl`, `daemon.log`, `summary.md` and `console.log` in the run dir.
- `replays/`: `replay_sessions.sh --since 2026-10-13T14:18:29Z --include-idle` (18 sessions), plus each
  session's `cluster` subagent events under `replays/sub-<sid>/subagents.json`. These were captured after
  the run for the A2 count below.
