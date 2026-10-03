# A1 soak — 2026-10-03 · std-simian-test · 8 hours unattended, compaction exercised

**The run note box A1 of [#1042](https://github.com/go-steer/core-agent/issues/1042)
asks for**, graded by `soak_verdict.py` per box A5 as amended on 2026-10-02:
the verdict is the grader's exit status, not a reading.

The [2026-09-14 soak](2026-09-14-std-simian-test-a1-soak-8h.md) answered three
of A1's questions and could not answer the fourth: its sessions never reached a
compaction threshold, so "no silently-disabled compaction" was never asked. This
run exists to ask it.

|  |  |
|---|---|
| date (UTC) | 2026-10-03, 08:04:17 → 16:04:25 — **8h 00m, zero human touches** |
| cluster | `std-simian-test` (`gke-demos-345619`, us-central1) |
| daemon image | `ghcr.io/go-steer/core-agent:main-f1befc7` (includes [#1226](https://github.com/go-steer/core-agent/issues/1226)) |
| compaction | `--compaction-threshold=0.021` — **22,020 tokens** on `gemini-3.7-flash`'s 1,048,576-token window (a local overlay edit, reverted after the run; see below) |
| cadence | incident every 1800s, held ~10 min, sample every 120s — 16 incidents, 234 samples |
| sessions | 17 touched, 429 model calls, **11 compactions** |
| pod | **0 restarts**, `ready 1/1` at every sample, memory 20–86 Mi |
| cost | **$5.82** in-window — $4.16 of it one session (see below) |
| run dir | `~/.gke-drill/soak/20261003T080413Z-std-simian-test` |

## The verdict

```
./soak_verdict.py --run-dir ~/.gke-drill/soak/20261003T080413Z-std-simian-test --compaction-at 22020

| survived   | PASS | 8.00h to the end row, no pod restart |
| no wedge   | PASS | no session halted or tripped; 16 of 16 observed incidents got a response |
| compaction | PASS | 10 session(s) crossed 22,020 tokens and were compacted; 5 more crossed and never took another turn |
| no drops   | PASS | the daemon log records no dropped inbox input (outside 1 blind window(s)) |

A1: closable on this run.   (exit 0)
```

**Box A1 is met on this run.**

## Why the threshold was lowered, and why it took three attempts

On a 1M-token window the default 0.85 is 850K tokens. This recipe's sessions
plateau at 21–24K on a ~16K floor, so no eight-hour soak can reach it. The soak
therefore runs the daemon with a lowered threshold, and the clause it tests is
the one A1 names: does compaction fire, unattended, when it is owed?

Getting there found two product defects and three grader defects, each on live
data:

1. **0.025 was out of reach too** — sessions peaked at 24,340 against 25,000.
   The soak was restarted at 0.021, above the ~18–19K a compacted session lands
   at (lower would compact every turn, which tests thrash, not survival).
2. **The flag did nothing ([#1226](https://github.com/go-steer/core-agent/issues/1226)).**
   An operator's threshold — config, `--compaction-threshold`, or a task class —
   ranked *below* the substrate per-tier defaults, so it governed no recognised
   model. The 0.021 run on the pre-fix image compacted at 0.85, i.e. never. Fixed
   in #1229 and re-run on its image: this run.
3. **The grader called deferred compaction a failure ([#1224](https://github.com/go-steer/core-agent/pull/1224)).**
   Compaction is marked when a turn ends and runs when the next one starts, so a
   session that crosses during its one incident and then idles owes nothing.
4. **The grader could credit a fake compaction (#1224).** A session whose last
   call was a side call reads that call's prompt in `window` for as long as it
   idles. The count now decides.
5. **The grader missed compactions between samples ([#1231](https://github.com/go-steer/core-agent/pull/1231)).**
   Nine of this run's ten compactions crossed and compacted inside one two-minute
   interval; no sample ever saw them above the line.

A sixth error was mine, in grading, and is recorded so nobody repeats it: the
first pass used `--compaction-at 21000` (0.021 × 1,000,000). The daemon's window
for `gemini-3.7-flash` is **1,048,576**, so 0.021 is **22,020**. Graded at 21,000
the clause read FAIL for three sessions; the daemon's own per-call ledger shows
each one ending its first turn *under* 22,020, owing nothing.

## What A1's PASS does not say — read this before quoting it

**One session ran away, and the guardrails contained it**
([#1235](https://github.com/go-steer/core-agent/issues/1235)). Session
`01a100e7-7ea0` looped on `record_plan` — **109 calls**, flagged three ways by
the watchdog but only at `[warn]` — was re-driven by auto-continue four times,
and spawned a background `cluster` subagent that grew its own context to
**791K tokens over 43 calls** before its budget stopped it. Because the
subagent runs the parent's model and bills into the parent's tracker, its rows
became the parent's `Last()` ([#1203](https://github.com/go-steer/core-agent/issues/1203),
now observed live). The per-turn cost ceiling cut three turns at $0.56,
**$1.19** and $0.53 against a $0.25 bound — the $1.19 one a single over-budget
call the ceiling cannot pre-empt. The session compacted at 11:37 and never
wedged. It spent $4.16 of the night's $5.82.

That is A4 ("a per-turn bound bounds a turn") observed live, and it is also
three defects. A1's clauses do not measure peak context or spend, so they pass;
the night is not clean.

**The summarizer returned empty text four times**
([#1234](https://github.com/go-steer/core-agent/issues/1234)) —
`model returned no summary text (finish_reason=STOP) after 2 attempts`, on three
sessions. Logged, so not *silent*; but one failure sets a two-turn backoff and
the mechanical fallback needs two consecutive failures, which a per-incident
session never reaches. Two sessions ended over their threshold because of it.

**Not exercised:** double-drive (one daemon, one session DB — #977 has its own
test), a pod restart mid-run, and the 850K production threshold.

## Evidence

- `timeline.jsonl`, `daemon.log`, `summary.md` in the run dir.
- `session-01a100e7-7ea0.events.sse` — the runaway session's replayed event
  stream, saved before the next redeploy wipes the session DB.
- Per-call ledgers read from the daemon's `/sessions/core-agent/<id>/usage`
  after the run (in #1234 and #1235).
- The soak overlay edits (image tag, `--compaction-threshold=0.021`) were local,
  never committed, and reverted after the run. The deployed daemon still carries
  the soak flag until the next `set-up-demo.sh`.
