# Behavioural evals

One case, run against a real model, graded by a deterministic verifier
that reads what the run left behind (#652).

Everything else in this repository measures something adjacent. Unit
tests prove functions work. `examples/internal/recipecheck` proves files
have the right shape. The GKE drill proves an operator with a clipboard
can get through a script. None of them answers the question this
directory exists for: **did the agent find the thing it was asked to
find, when nobody told it where to look?**

The machinery is `internal/evals`; the runner is
`dev/smoke/cmd/evalrun`; the CI leg is `dev/smoke/11-evals-cluster-fact.sh`,
wired as the third leg of `dev/tools/e2e-real-provider`.

## Running one

```
go build -o /tmp/core-agent ./cmd/core-agent
go build -o /tmp/evalrun ./dev/smoke/cmd/evalrun

ANTHROPIC_VERTEX_PROJECT_ID=<project> CLOUD_ML_REGION=us-east5 \
/tmp/evalrun \
  --case dev/evals/cases/cluster-fact-image-pull.json \
  --fixtures dev/evals/fixtures \
  --binary /tmp/core-agent \
  --agent-args "--provider=anthropic-vertex --model=claude-sonnet-5" \
  --json /tmp/report.json
```

Exit codes: `0` the tools tier passed and the baseline held, `1` a check
failed or the baseline scored, `2` the report is indeterminate — some
check observed nothing.

`2` is separate from `1` deliberately. A failing check says the agent did
the wrong thing; an indeterminate one says the harness did not find out
what the agent did. They send an operator to different places, and both
are non-zero, because an unanswered question is not a pass.

## The four rules

Each of these is enforced by code, not by review. That is the whole
difference between a harness and a habit.

**1. Grade the world, not the transcript.** A check's `source` is either
`answer` — the agent's final output — or `witness:<name>`, a file *the
world* wrote. In the shipped fixture the witness is the kubectl shim's
own log of what it was asked.

The asymmetry is the point. An eventlog we emit is the system reporting
on itself, and it cannot see a subagent that fanned out in-process. What
the shim was asked, it was asked by somebody, and the somebody does not
matter: a read issued by a delegated subagent counts exactly as the
parent's does, with no dependency on a roster endpoint being wired up.

**2. Withhold the location.** `Case.Bind` fails if the prompt contains
any of the fixture's facts, case-insensitively. If the prompt says
`payments-prod`, an answer saying `payments-prod` is a copy rather than a
finding, and the no-access baseline will cheerfully score it. "Did I leak
the location" is not a question a reviewer reliably asks on the twentieth
case, so it is not left to a reviewer.

**3. Demand the chain, never a bare mention.** A check that asks for the
workload name *and* the tag it carries *and* the registry's own wording,
together, is hard to satisfy without having read all three — and the
third is only reachable through a second, targeted read, because
`kubectl get pods` does not show events. A single substring match on any
one of them would not be.

Matching is case-insensitive fixed-string containment. Not regex: a
corpus of regexes is a corpus of bugs nobody reviews.

**4. A check that learned nothing says so.** `Vacuous` marks a result
that carried no information, and a vacuous result never scores whatever
its `passed` field says. The specific trap: a `none_of` check against a
witness the world never wrote *passes*, because nothing is absent from
nothing. An aggregate that counted it would report a clean run in which
nothing happened — which is the shape of credential-less green that #488
was filed for, one layer up.

## The two tiers

Every case runs twice, and the two runs differ by exactly one flag
(`--no-builtin-tools`). Same binary, same prompt, same fixture on disk.

- **`tools`** — the real run. This is the one that is graded.
- **`no-access`** — the grader's self-test. Every objective check must
  score **zero** here or the case does not ship. A check a naked model
  satisfies is measuring vocabulary, and it will keep measuring
  vocabulary however good the agent gets.

The one-flag rule is not tidiness. If the baseline differed in prompt,
fixture or checks, a baseline of zero would be evidence about the
baseline's setup rather than about the check.

The baseline is graded by `BaselineHolds`, not folded into the report's
verdict — by construction it fails and is partly vacuous, and folding
that in would make every report indeterminate forever. `BaselineHolds`
wants three things, and the second two are there because the cheapest
way to score zero is to never run:

1. no check scored,
2. the process ran to completion, and
3. at least one check actually observed its source.

The first live run of this harness died at startup on a flag collision
and scored a clean zero. Without (2) it would have been reported as a
sound baseline.

## Adding a case

No Go. A case is a JSON file, its world is a JSON file, and the shim
that renders that world is shared. **If a second case needs code, the
schema is wrong** — that is the check on this design, and it is meant to
be applied.

```
dev/evals/
  cases/<id>.json           the prompt, the planted defect, the checks
  fixtures/
    _shared/bin/kubectl     laid down under every fixture, first
    <name>/
      fixture.json          roles, witnesses, facts_from, env
      cluster.json          the world AND the facts, one file
      workspace/            where the agent starts
```

`_shared/` is copied first and the fixture second, so a fixture inherits
the shim by default and overrides it by shipping a file at the same
relative path. Nothing in a `fixture.json` ever points at another
directory.

**Facts live inside the world file.** `facts_from: "world#planted"` reads
them out of the same JSON the shim renders, so the value a check asserts
and the value the agent can observe are one string rather than two that
agree today. A name asserted on both sides of a test is untested — the
lesson from the 2026-09-06 drill-rig defects, applied here before it
could cost anything.

**Roles, not paths.** A case says `${fact.workload}`; it never says which
file holds it or which directory the agent starts in. That indirection is
what lets `probes` mean something: every probe is confirmed present in the
materialized world *before* the agent starts, so a check that fails on an
absence is entitled to be called a violation rather than an environment
nobody provisioned.

**Witnesses are not pre-created.** Their parent directory is; the file is
not. Absent means nothing ever reached the world, and empty means the
world was reached and asked nothing — different findings, and rule 4
spends the difference.

## The shim

`_shared/bin/kubectl` renders the world from `$EVAL_WORLD` and appends
every invocation to `$EVAL_WITNESS_CLUSTER_READS`. It is a measuring
instrument, so its fidelity is load-bearing in a way a mock's usually is
not. Two behaviours it gets right, both found by exercising it rather
than by reading it:

- **The empty-result asymmetry.** `kubectl get pods -n nowhere` writes
  "No resources found" to **stderr** and exits **0**. A missing resource
  type exits non-zero. Getting this backwards teaches the agent that a
  healthy namespace is a broken one — and it is the same trap as the
  sixth soak-harness defect, which is exactly why it must not live inside
  the instrument.
- **Aliases are a keyed map, not an algorithm.** `"ns"` is not `"n"` plus
  an `"s"`. A singularizer that strips the trailing `s` answers `get ns`
  with "the server doesn't have a resource type", teaching the agent the
  cluster has no namespaces.
- **The verb is the first positional argument, not `argv[0]`.** kubectl
  takes global flags on either side of the verb, so `kubectl -n prod get
  pods` is valid and an instrument that calls it an unknown command is
  measuring itself.

Mutating verbs are refused with a `Forbidden` error, so the
`changed-nothing` check measures intent rather than damage — which is the
only thing left worth measuring once the refusal is doing its job.

Each witness line carries the resolved verb as a `verb=` field alongside
the raw argv, and **a check that cares which verb was used must match on
that field**. The same flag-placement fact bites here from the other
side: `kubectl -n prod delete pod x` is a delete that the substring
`"kubectl delete"` does not appear in, and the check that must not be
fooled by flag placement is exactly the one asserting nothing changed.

## What this is not

Not a scorecard. `SCORECARD.md` is under a maintainer hold and the rubric
is not to be automated here; the metric program (#967) and the corpus
grown from our own incidents (#966) are separate and both sequence after
this. This directory is the walking skeleton: one case, end to end,
honest about what it did and did not observe.

Not a taxonomy of check types either. There is one `Check` type, and
every question this skeleton can ask has the same shape.
