# GKE drill batch — 2026-09-13 · std-simian-test · 11 × B + 10 × C · post-#935

**Not a scorecard.** This batch exists to answer one question that
[#1037](https://github.com/go-steer/core-agent/pull/1037) shipped without
answering: **do the delegation failures actually disappear?** That PR's own
adversarial review said the answer needs a cluster and that landing on a green
check would be the thing this milestone says to be suspicious of. This is the
cluster sitting.

|  |  |
|---|---|
| date (UTC) | 2026-09-13, 10:09 → 11:38 |
| scenarios | ☐ A bad image ☑ B OOMKill ☑ C RBAC-denied — **alternating** |
| cluster | `std-simian-test` (`gke-demos-345619`, us-central1) |
| daemon image | `ghcr.io/go-steer/core-agent:main-ca1cc73` (#1038's squash; #935's fix rides in it) |
| content image | `…/gke-platform-agent-content:v3` |
| model flavor | ☑ gemini ☐ anthropic |
| run directories | `~/.gke-drill/runs/20260913T100903Z-b` … `20260913T113431Z-c` |
| attempted / scored | 21 / 20 — **one rig failure**, [see below](#the-rig-failure-and-why-it-is-not-a-flake) |
| cost | $1.91 for the batch (mean $0.0957/run) |
| driver | `/tmp/drill-batch.sh` — alternating B/C, same shape as 2026-09-12 |
| scorer | filed from the daemon log + `evidence.md` × 20 — **not countersigned** |

Archive is now 59 runs: 38 pre-#935, 21 post.

---

## The result

**The retry works, it is not free, and it did not reduce the failure count to
zero.** All three clauses matter.

| | pre-#935 (38 runs) | post-#935 (21 runs) |
|---|---|---|
| delegations completed | 34 | 20 |
| **delegations killed by a 429** | **4** | **1** |
| runs with ≥1 failed delegation | 4 (10.5%) | 1 (4.8%) |

**Do not quote the rate.** Fisher's exact on 4/38 against 1/21 gives **p =
0.65**. The batch is nowhere near powered to detect a change of that size, and
if the rate table were the whole evidence this document would be reporting
nothing. It is not the evidence.

The evidence is mechanical, and it comes from the daemon log rather than the
transcripts:

| `core-agent: gemini: …` line | count |
|---|---|
| `transient provider error (…429…) — retrying once after 2s` | **13** |
| `transient provider error recovered on retry (attempt 2/2)` | 3 |
| `transient provider error (…) NOT retried: … within the 30s cooldown` | **1** |
| `transient provider error persisted after retry — surfacing to caller` | 0 |

**Fourteen Vertex 429s reached the policy in ninety minutes.** Thirteen were
retried and not one of those thirteen went on to kill anything. The single
surviving failure is the single suppressed retry. That is a much stronger
statement than the rate table, because it does not depend on the two batches
having faced comparable quota pressure — which, visibly, they did not.

Cost is unchanged to three decimal places: **$0.0957/run against $0.0959** for
the pre-fix batch of the day before. A retry that fires fourteen times in
ninety minutes does not show up in the bill.

---

## The one that still died, and it is our own cooldown that killed it

`20260913T101329Z-b`. The correlation is not circumstantial:

```
10:14:32.106  gemini: transient provider error (…429…) — retrying once after 2s
10:14:38.653  gemini: transient provider error (…429…) NOT retried:
                      another retry fired within the 30s cooldown
10:14:38.653  spawn_agent → {"status":"failed","stop_reason":"error",
                             "output":"Error 429 … RESOURCE_EXHAUSTED …"}
```

The functionResponse is stamped `10:14:38.653934Z` and the suppression line
`10:14:38.653189Z` — **745 microseconds apart.** Same event. The parent's own
429 at 10:14:32 consumed the window; the subagent's 429 six seconds later was
refused a retry and the delegation died.

#1037 made that cooldown **process-wide on purpose**, and the reasoning is in
`pkg/models/retry.go`: Vertex quota is enforced per project and region, so a
per-instance cooldown would let a parent plus three subagents fire four retries
into one shed. `TestTransientRetryCooldownIsProcessWide` exists to stop anyone
quietly making it a field.

That reasoning is still right. What it missed is that **429s are correlated by
construction** — a shed is exactly the condition under which several concurrent
callers are rejected within seconds of each other — so a one-rescue-per-30s
budget spends itself on whichever caller happens to be first and abandons the
rest. The mechanism is well tested and the *policy* is wrong in the one traffic
shape it was built for.

Filed as **[#1039](https://github.com/go-steer/core-agent/issues/1039)** with the
data above. The recommendation there is a small token bucket rather than a
single timestamp: burst 3, refill one per 30s, which bounds added load at three
extra requests per window instead of one and survives a correlated burst.
**Not changed here** — n = 1 for the suppression, and this project has already
been bitten once by acting on a three-run reading.

---

## Ten of thirteen retries logged no outcome at all

Thirteen retries fired; three logged `recovered on retry` and none logged
`persisted after retry`. The other ten are silent, and that is a defect in what
#1037 shipped, not a mystery.

`RetryPolicy.Wrap` returns bare on every `!yield(...)` — the consumer-stop path.
When the retry succeeds and the consumer stops mid-stream after taking the
content, the function returns from inside the loop and skips the
`recovered on retry` line at the bottom. The retry worked; nothing said so.

This matters because `docs/site/src/content/docs/concepts/providers.md`, also
shipped in #1037, tells operators to look for exactly those three lines. In
production, 10 of 13 real retries printed none of them. The doc is not wrong
about the lines, it is wrong about how often an operator will see one.

Filed with #1039 — the fix is to record the outcome in a `defer` rather than at
the one exit the happy path happens to take.

---

## The rig failure, and why it is not a flake

`20260913T110018Z-c` was lost. The drill had already streamed and written 36
frames; the run died **on the way to disk**:

```
lib.sh: line 461: /usr/bin/jq: Argument list too long
```

`drill_capture_subagents` carried the merged event array through
`jq --argjson`. A single argv entry is capped at **`MAX_ARG_STRLEN` = 128 KiB**
on Linux — which is not `ARG_MAX`, is per argument rather than per command line,
and is not raisable with `ulimit`. Subagent captures in this recipe run 50–60
KiB, so the margin was about 2×, and a chatty C run crossed it.

It then cost a whole run rather than one artifact, because `drill_write_meta`
redirected straight into `meta.json`: the shell truncates before `jq` runs, so
the failure left a **0-byte** `meta.json`, and `score.py` raises
`JSONDecodeError` on that instead of rendering the partial sheet the cleanup
trap exists to produce.

Both are fixed in the same PR as this note — accumulator through a file, meta
written through a temp and moved — and `dryrun.sh 14` reproduces the live
failure exactly against the pre-fix code: exit 126, zero frames captured,
0-byte `meta.json`, no sheet.

---

## The daemon log is a witness nobody had been reading

Three things in this batch are invisible in all 59 transcripts and obvious in
ninety minutes of `kubectl logs`:

**1. The retry lines themselves.** `models.RetryPolicy` logs to the daemon's
stderr. A drill run that recovers from a 429 leaves *no trace at all* in its own
transcript, which is why the rate table above is the weaker evidence.

**2. Two `400 INVALID_ARGUMENT` turn errors** — the [#898](https://github.com/go-steer/core-agent/issues/898)
shape, deliberately excluded from `gemini.IsTransient`. Both landed on turns
that ran *after* the drill had stopped capturing, so neither is in a transcript.

**3. A watchdog-halt livelock.** Three sessions ended the batch wedged:

| session | watchdog halts | over |
|---|---|---|
| `01a09a4d-ef48-…` | **13** | 10:47 → 11:38 (51 min) |
| `01a09a59-eb04-…` | 6 | 10:57 → 11:13 |
| `01a09a61-d70d-…` | 1 | 11:16 |

The pattern repeats every 3–4 minutes: `no-op-streak` halts the turn
(`mark_task_done` × 3 inert), the queued operator input drives another turn, the
tripped guardrail refuses it, and the refusal is itself logged as a halt. It is
not auto-continue — that stands down each cycle with *"operator input already
queued; standing down so it drives the next turn"*. The queued inbox item can
never be consumed and is never discarded. Nine minutes after the last run
finished, the daemon was still emitting a three-session stand-down every five
minutes with no operator anywhere.

Filed as **[#1040](https://github.com/go-steer/core-agent/issues/1040)**. It is
unrelated to #935 and the drill has almost certainly been producing it for
weeks; nobody looked at the daemon log after a batch before.

**Method note for the next batch: capture the daemon log.** `kubectl logs -f
--timestamps deploy/core-agent` for the duration, to a file. Without
`--timestamps` the retry lines cannot be tied to a run, which is how the first
half of this batch nearly lost the correlation that identified the cooldown.

---

## What this does and does not settle for #935

**Settled.** The retry fires against real Vertex 429s, with the exact error
string the archive predicted; it recovers; it costs nothing measurable; and
thirteen of fourteen rejections that would previously have been terminal were
not.

**Not settled.** Whether the failure *rate* moved — this batch cannot tell, and
a batch that could would need to be several times larger. That is the wrong
thing to spend cluster time on, because the mechanism is now directly observed
and the one residual failure has a named cause.

**Newly open.** #1039 (the cooldown spends its budget on the wrong caller; the
outcome logging is mostly silent) and #1040 (wedged sessions). Neither existed
as a known problem before this sitting.

### One datum for #1036

The parent of the failed delegation **did** disclose it, unprompted:

> *(Note: Diagnostic delegation to the specialist subagent encountered an
> upstream rate limit [Error 429: Resource Exhausted], so diagnosis was
> performed directly via GKE cluster read APIs.)*

— and then silently absorbed the whole investigation anyway and produced a
correct, cited answer. So the run scored. That is
[#1036](https://github.com/go-steer/core-agent/issues/1036) in one run: the
disclosure half happened, the absorption half is what makes the failure
invisible to the rubric. n = 1; recorded, not concluded.
