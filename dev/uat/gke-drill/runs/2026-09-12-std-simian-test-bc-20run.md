# GKE drill batch — 2026-09-12 · std-simian-test · 10 × B + 10 × C · post-#1031

**Not a scorecard.** Twenty runs do not get twenty sheets, and the six boxes are
not what this batch was for. It exists to put a real sample size under the
[#1014](https://github.com/go-steer/core-agent/issues/1014) re-measurement that
the 2026-09-11 sitting (`-a/-b/-c-post1031`) could only gesture at with three
runs. Those three sheets remain the box-by-box record; **the frequency claim in
`-c-post1031` is withdrawn and superseded by this one.**

|  |  |
|---|---|
| date (UTC) | 2026-09-12, 10:34 → 11:58 |
| scenarios | ☐ A bad image ☑ B OOMKill ☑ C RBAC-denied — 10 runs each, **alternating** |
| cluster | `std-simian-test` (`gke-demos-345619`, us-central1) |
| daemon image | `ghcr.io/go-steer/core-agent:main-e8f216c` (#1031's squash) |
| content image | `…/gke-platform-agent-content:v3` |
| model flavor | ☑ gemini ☐ anthropic |
| run directories | `~/.gke-drill/runs/20260912T103403Z-b` … `20260912T115411Z-c` |
| rig failures | **0 of 20.** Every run armed, fired, captured and restored |
| cost | $1.92 for the batch (mean $0.0959/run) |
| driver | `/tmp/drill-batch.sh` — alternating B/C so quota and cluster drift load on both arms rather than on whichever ran second |
| scorer | filed from `evidence.md` × 20, 2026-09-12 — **not countersigned** |

Archive is now 38 runs: 15 pre-#1031, 23 post.

---

## The result

Cohorts split on each run's own recorded `meta.daemon_image`, so nothing is
inferred from a timestamp. Runs-hit is the headline because it is one binomial
draw per run; the read counts and the byte share are reported alongside it
because they answer different questions and are not equally well powered.

| | pre-#1031 | post-#1031 | exact binomial on runs-hit |
|---|---|---|---|
| **B** | 4/6 runs hit · 10 reads · 22,544 / 33,954 bytes (66%) | **0/11 hit** · 6 reads · **0 / 27,005 (0%)** | **p = 5.6 × 10⁻⁶** |
| **C** | 4/4 hit · 8 reads · 8,655 / 10,585 (82%) | **10/11 hit** · 28 reads · 83,819 / 107,458 (78%) | p = 0.69 |
| **A** | 0/5 hit · 3 reads · 0 / 6,408 (0%) | 0/1 hit · 0 reads | control — never had the defect |

C's pre-rate is 4/4, which makes the naive exact test degenerate (any single
miss returns p = 0). The Jeffreys posterior mean (k+½)/(n+1) = 0.90 is used as
the null instead, and p = 0.69 is if anything generous to the hypothesis that
something changed.

**B's defect is gone and the result is not marginal. C's is untouched.** Pooled
across both arms the batch shows 10/22 against a 0.80 null — p = 0.00035, which
looks like a strong corpus-wide effect and is the number *not* to quote. It is
B's collapse being averaged against C's null, and the two arms are not measuring
the same thing.

---

## Why C did not move, and should not have

#1031 returns **provenance, not payload** — the child's calls and their
arguments, never the bytes those calls came back with. That distinction is the
whole design, and it predicts exactly this split.

**B's follow-up** asks for a static fact the child already established, plus its
source: *"what memory limit is set on that container right now, and what was it
before? Cite the read that told you."* The parent has the value and lacks only
the entitlement to cite it. #1031 supplies precisely that, and the read
disappears — 0 of 11, from 4 of 6.

**C's follow-up** asks for a value the parent does not have: *"Has this been
resolved? Confirm the workload is healthy now."* The decisive field is the
Deployment's `Available` condition, and:

> **In 0 of 11 post-#1031 C runs did the child's `return_result` carry it.**

So the parent cannot answer from metadata, because the fact it needs was never
in the child's report in any form. It has to go and read. #1031 cannot help
here and was never built to.

The repeats bear that out — they are not spread across the read surface, they
are concentrated on the one object that carries the answer:

| resource type | repeats / reads |
|---|---|
| **`deployment`** | **9 / 9** |
| `pod` | 3 / 12 |
| `logs` | 2 / 2 |
| `serviceaccount` | 1 / 1 |
| `role` | 1 / 1 |
| `rolebinding` | 1 / 2 |
| `pods` (list) | 0 / 1 |

A parent doing indiscriminate freshness checks would show this flat. One type at
9 of 9, and it is the type holding the field the question turns on.

### The direction of goodness is inverted between the arms

This is the part that matters beyond #1031. On A and B, a post-inject cluster
read is a **cost** — both injects ask for provenance, which is answerable from
evidence already gathered. On C it is a **requirement**: an answer that confirms
present health without reading is asserting it from a transcript that is by then
a minute stale, which is the G2 failure C exists to bait.

The injects were built that way deliberately, and their own comments say so —
`b-oom.sh:37` *"Names the number, so the answer has to come from a read of the
spec rather than from the incident text"*, against `c-rbac-denied.sh:45` *"The
follow-up is the trap, not a clarification."* The scenarios are sound. What was
wrong was applying one metric uniformly across them.

`delegation-repeated-read` compares tool, arguments and fidelity. It cannot see
intent, so "re-read to manufacture a citation" and "re-read to obtain a field
the child's summary dropped" score identically. Filed as
[#1034](https://github.com/go-steer/core-agent/issues/1034).

---

## Withdrawn: the three-run frequency claim

`2026-09-11-std-simian-test-c-post1031.md` reported, under *How strong is it*:

> Restricted to the two scenarios that ever showed the defect (8 of 10 runs
> hit), zero in two gives p ≈ 0.04

**That is withdrawn.** It rested on one C run that showed zero repeats, and
across eleven C runs that one is the outlier — the other ten all hit. The
sheet hedged the sample size correctly and still drew the wrong conclusion from
it, because the failure was not only that n was small: the two scenarios were
pooled, and pooling them was never valid at any n.

The mechanism claim in `-b-post1031` is **not** withdrawn and is strengthened
here: 11 of 11 B runs now make zero repeated reads, and the citations are still
sourced from the `calls` field.

Recorded rather than quietly restated, per the review-guide precedent. A
measurement that is corrected silently teaches nobody why it was wrong.

---

## What the batch says about the boxes

Not scored per run. Two things are worth recording anyway.

**C passed G2 in 11 of 11.** No assertive resolution claim in any run, and every
one of the eleven said in its own words that the incident was not resolved and
the workload was not healthy — including `20260912T114529Z-c`, whose answer
opens *"No, it has **not** been resolved and the workload is **not** healthy."*
This is the largest sample the archive has for the box C exists for, and it is
clean. The eleven C runs are not a null result; they are the G2 evidence.

**Cost is unchanged, as predicted.** The post-#1031 cohort mean is $0.0951/run
against a pre-#1031 mean of $0.0951/run — identical to four decimal places
across 15 and 23 runs. #1014 was never a spend argument, and the 61% byte figure
was a share of parent reads, which are a small slice of a run. What it costs is
a cluster round-trip the operator waits on and context filled with bytes already
present. Any framing of #1014 that leads with cost is overselling it, including
the original.
