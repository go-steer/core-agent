# A1 soak — 2026-09-14 · std-simian-test · 8 hours unattended

**This is the run note box A1 of [#1042](https://github.com/go-steer/core-agent/issues/1042)
asks for**, and nothing else. A1's wording: *"One unattended run of 8 hours or
more, on a live cluster, completes with no human touch: no wedged session, no
silently-disabled compaction, no double-drive, no unbounded growth. Proven by a
run note in `dev/uat/gke-drill/runs/`, not by a green suite."*

The soak asserts nothing on its own — `soak.sh` samples, it does not grade. What
follows is the reading, with the evidence for each of the four questions and an
explicit list of what eight hours of this cadence did **not** exercise.

|  |  |
|---|---|
| date (UTC) | 2026-09-14, 12:45:08 → 20:45:20 — **8h 00m, zero human touches** |
| cluster | `std-simian-test` (`gke-demos-345619`, us-central1) |
| daemon image | `ghcr.io/go-steer/core-agent:main-8d9ee38` |
| namespaces | agent `gke-platform-agent`, target `online-boutique`, workload `emailservice` |
| cadence | incident every 1800s (cadence break → ~10 min → restore), sample every 120s |
| observed | **16 incidents**, 234 samples, 19 sessions touched, 116 turns added |
| pod | **0 restarts**, `ready 1/1` at every one of the 234 samples |
| cost | **≈ $1.84** in-window (17 sessions created during the run at full cost, pre-existing ones by delta) — ≈ $0.115 per incident |
| run dir | `~/.gke-drill/soak/20260914T124504Z-std-simian-test` |
| runbook | [`dev/uat/gke-drill/README.md`](../README.md) |

---

## The four questions

### 1. Wedged session — **no**

Nineteen sessions, 2 659 session samples, and the `status` column reads `active`
in every one of them. `halted` is `false` in every one; `tripped` is `false` in
every one. No session sat in `working` across a quiet gap, and none of the eight
30-minute quiet windows ended with a session still holding a turn.

The stronger version of the same evidence is the shape each session traces.
Every incident produces the same arc: a session appears 0.4–2.8 minutes after
the cadence break, climbs to 11–15 turns over the following ~17 minutes — the
last turn landing *after* the restore, which is the agent observing the recovery
— and then never moves again. Sessions created at 13:15 were still being sampled
at 20:45, seven hours later, at exactly the turn count they finished with.
**That is what "not wedged" looks like from outside: the work stops when the
work is done, and the process keeps answering about it.**

### 2. Silently-disabled compaction — **not exercised, and that is the honest answer**

There were **zero compactions**, because nothing came close to needing one.
`window` per session tops out at 24 239 tokens for sixteen of the nineteen; the
one long-lived session reaches 37 321. There is no cliff in the `window` column
anywhere in the run because there was nothing to cut, and the
`context-reduction` section of the summary is correspondingly empty.

So this run does **not** answer question 2. It does not falsify it either — the
failure it is written against (a window climbing in a straight line to its
threshold with no compaction and no degraded row) requires reaching a threshold.
The largest in-window growth of any session was 19 409 → 37 321 tokens in eight
hours, or about 2.2 K/hour; against the 128 K assumed window that
[#974](https://github.com/go-steer/core-agent/issues/974) shipped, that session
needs roughly **forty more hours** to get there.

**Reading this as a pass for A1 would be reading an unasked question as
answered.** Closing question 2 wants either a much longer run or a soak variant
that drives one session hard rather than spreading turns across a new session
per incident.

### 3. Double-drive — **no**

47 turn-increase events across the run, and every one of them is attributable:
each sits either 0.4–4 minutes after a cadence break (the incident arriving), or
6–17 minutes after one (the session's own follow-up work and the post-restore
observation). Between the end of a session's arc and the next incident there is
not a single turn on any session — across eight ~13-minute quiet tails, which is
precisely the window where a re-drive would show.

Worth being exact about what this does and does not cover. #1042 lists
[#977](https://github.com/go-steer/core-agent/issues/977) — the run lock not
held across the continuation turn — as an A1 dependency, and that defect needs
**two daemons on a shared session DB**. This run had one daemon and one DB, so
it is evidence against single-process double-drive only. #977 is untouched by
it.

### 4. Unbounded growth — **no, with one number to watch**

Memory over the eight hours, as hourly means of the 234 samples:

| hour (UTC) | 12 | 13 | 14 | 15 | 16 | 17 | 18 | 19 | 20 |
|---|---|---|---|---|---|---|---|---|---|
| mean RSS (Mi) | 87 | 94 | 91 | 92 | 93 | 97 | 99 | 97 | 96 |

Start 87 Mi, end 97 Mi, **and the curve is a plateau, not a ramp** — it settles
at 92–99 Mi from hour 13 onward and the last three hours trend *down*. One
sample reads 156 Mi (13:18, during a turn) and never recurs; every other sample
in the run is between 84 and 99. Against a 512 Mi limit, with 0 restarts, there
is no OOM trajectory here.

The number to watch is not memory. `sessions` on the daemon goes **142 → 158**:
one new session per incident, none ever closed, forever. That is by design
(the session DB is the record) and it is bounded by operator behaviour rather
than by the daemon, but a monitoring deployment that creates a session per alert
and retains all of them is an unbounded row count on a long enough timeline.
Not an A1 failure; a thing to have counted before somebody reports it as one.
Per-session `window_max` shows no ceiling anybody is stuck at — median 22 599,
max 37 321.

---

## The headline: a failure nobody was awake for, self-healed in 4m17s

The one genuinely interesting minute of the night is 19:18:52, and it is the
single best piece of evidence in the run *for* the milestone:

```
19:18:52  session 01a09fd8 turn: Error 400, Message: Request contains an
          invalid argument., Status: INVALID_ARGUMENT, Details: []
19:23:09  session 01a09fd8: auto-continue queued (turn interrupted 4m17s ago)
19:23:09  auto-continue boot scan: queued continuations for 1 session(s)
19:25:16  turns 32 → 33
```

A provider error killed a turn mid-flight, with nobody attached. Four minutes
and seventeen seconds later the in-lifetime retry driver
([#575](https://github.com/go-steer/core-agent/issues/575) defect B, the loop at
`cmd/core-agent/main.go`'s `AutoContinueRetryLoop`) noticed the stranded tail
and queued a continuation; the session resumed two minutes after that and went
on to add two more turns before the window closed. No restart, no operator, no
wedge. **Recovery from an unattended provider failure is the thing A1 is
actually about, and this is it happening.**

Two notes on it, one of which is a defect.

**The log calls it a boot scan, and it was not one.** Every message
`pkg/compose/auto_continue.go` writes is prefixed `auto-continue boot scan:`,
including the ones the in-lifetime retry loop emits. The pod had zero restarts
and 6.5 hours of uptime at 19:23. An operator reading this log — which is the
entire audience for a line printed at 19:23 on an unattended daemon — concludes
the process rebooted, and starts looking for a crash that did not happen. It
cost me a detour through `main.go` to establish that it had not. Filed as
[#1066](https://github.com/go-steer/core-agent/issues/1066); this run is its
evidence.

**The 400 itself is unexplained.** `INVALID_ARGUMENT` with no detail, on the one
session with the largest history in the run, at turn 32. It did not recur. It is
not obviously the window (37 K against a 1 M model window). Recording it here
because "it self-healed" is the A1 answer and "we do not know why it broke" is
still true.

## Provider retries: one 429, rescued

```
13:00:34  gemini: transient provider error (Error 429 … RESOURCE_EXHAUSTED)
          — retrying once after 2s
13:00:41  gemini: transient provider error recovered on retry (attempt 2/2)
```

One 429 in eight hours, retried, recovered, zero surfaced to a caller. Post-
[#935](https://github.com/go-steer/core-agent/issues/935)/#1039 behaviour
holding under a quiet-quota window — much lower pressure than the 2026-09-13
batch's fourteen, and consistent with it.

## What limits this evidence

- **The daemon log has a hole.** `soak: --- log stream ended, reconnecting ---`
  at 16:47:18, and the `session created` line for `01a0a0d0` (created ~16:47)
  is missing as a result. Lines written in that gap are gone. The sampled tables
  are unaffected — they are polled, not streamed — but any claim of the form
  "the daemon never logged X" is weaker than it looks for that window.
- **One daemon, one session DB.** Says nothing about #977.
- **No compaction, no guardrail trip, no cost-ceiling trip** occurred, so the
  degraded paths #974, #891 and #1049 shipped are all untested by this run.
- **The verdict below is a maintainer reading logs**, which is exactly what box
  A5 exists to replace. A5 is blocked behind the SCORECARD hold; until it is
  not, every A1 note including this one is the thing A5 calls insufficient.

## Verdict

**Three of A1's four questions are answered in the affirmative on this run; the
fourth was never asked.** No wedged session, no double-drive, no unbounded
growth, across eight unattended hours with sixteen injected incidents, zero
restarts and one self-healed provider failure. Compaction did not run because
nothing needed compacting, so **A1 should not be recorded as met on this run
alone** — it needs either a longer window or a soak variant that concentrates
turns on one session.
