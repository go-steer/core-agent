# `gke-platform-agent` — a GKE platform agent, authored core-agent-native

A long-lived, propose-only GKE platform operator: it triages watcher-driven
incidents, delegates single-cluster diagnosis to a specialist subagent with its
own content root, and hands back a root-cause analysis plus a proposed manifest
patch. It never mutates a cluster, because nothing in its toolset can.

This is the second half of [#704](https://github.com/go-steer/core-agent/issues/704).
The first half froze [`examples/kube-platform-agent`](../kube-platform-agent/)
as a portability case study. **This is the recommended starting point for a GKE
agent on core-agent.**

## Why a second GKE platform recipe exists

`kube-platform-agent` set out to prove the v2 loader can consume an *unmodified*
third-party (Hermes) agent snapshot. It proved exactly that. What live UAT then
showed is narrower and more useful:

> The mechanics port. **Identity does not.**

That recipe's base persona is a Hermes **kanban worker** whose identity is
*accept a task → loop until done → file a completion report → exit the session*.
On a live cluster that produced:

- a **general prompt** ("how would you rate your performance?") forced into a
  canned "all tasks complete, environment healthy, exiting session" report
  instead of an answer;
- **confabulated** verification — `Running 1/1` and invented evidence codes,
  with zero tool calls — because the persona demands an authoritative status
  report ([#639](https://github.com/go-steer/core-agent/issues/639));
- a **loop** with no reachable "done", because *loop-until-done* plus no write
  path means the exit condition never arrives.

An overlay cannot out-argue a 166-line worker identity
([`upstream/SOUL.md`](../kube-platform-agent/upstream/SOUL.md)), and
[#703](https://github.com/go-steer/core-agent/issues/703) showed it cannot
out-argue a single skill either — skills load at the point of use, so they speak
last. The fix is not to patch the identity. It is to not inherit it.

## The authoring thesis

**Identity → equipment → conduct. Not role → lifecycle.**

- **Identity (general).** A long-lived autonomous agent that responds to whatever
  arrives — questions, alerts, tasks — and does not "complete and exit".
- **Equipment (specific).** Read-only `gke` tools, a `cluster` specialist and its
  six native GKE skills, a bounded `wait_and_verify` — described as
  *capabilities*, alongside the real constraints (propose-only, plan-first, no
  shell).
- **Conduct (general).** Verify before claiming, propose rather than fix,
  delegate and build on the result, don't hunt the filesystem.

Explicitly **dropped** rather than patched: the kanban protocol and
"always call `kanban_complete`", loop-until-done, the `SETTINGS.md` +
GitOps-repo-pull bootstrap, "exiting the session", and the *always-on* 3-part
incident frame — kept only as an optional format for actual incident write-ups.

`recipe_test.go`'s `TestPersonaDoesNotShipAKanbanLifecycle` is that paragraph as
an assertion. It is a blunt instrument on purpose: it can tell whether someone
pasted the lifecycle back in, not whether the agent behaves well. The
behavioural question belongs to the live drill
([#970](https://github.com/go-steer/core-agent/issues/970)).

## Layout

```
AGENTS.md              # native parent identity (the centerpiece)
cluster/               # the `cluster` subagent's OWN content root
  AGENTS.md            #   native single-cluster specialist persona
  mcp.json             #   read-only gke
  skills/              #   6 GKE domain skills (loaded from <root>/skills)
.agents/
  config.json          # local run wiring (plan-first, no bash, cluster subagent)
  config.hub.json      # the same, plus attach.listen + multi_session (deployed)
  mcp.json             # read-only gke
  env.yaml             # env manifest — REQUIRED for ${env:} to interpolate
  plans/.gitkeep       # pre-baked mount point for the nested writable emptyDir
deploy/                # kustomize base + 4 overlays; the content image Dockerfile
scripts/               # the operator rig — build, deploy, break, attach, teardown
DEMO.md                # the live-cluster walkthrough
recipe_test.go         # credential-free loader + content validation
```

Self-contained: **no `content_roots`, no `@include`, no vendored upstream.** That
self-containment is load-bearing rather than cosmetic — it is why the content
image has no `upstream/` layer, and why "which persona is actually mounted" is a
question with one answer.

The `cluster/` tree being a sibling of `.agents/` rather than a child is the
per-subagent content root shipped in [#619](https://github.com/go-steer/core-agent/issues/619)
/ [#621](https://github.com/go-steer/core-agent/issues/621): the specialist loads
its own `AGENTS.md`, its own `skills/` and its own `mcp.json`, and the parent
never sees any of them.

## Constraints are enforced, not requested

The recurring finding across assessments is that recipes *state* safety
properties the runtime does not enforce — a persona that says "you are
propose-only" while `write_file` sits registered and `mode: yolo` waves it
through. Here the config is the enforcement and the persona only *describes* it.

| Claim in the persona | What makes it true |
| --- | --- |
| Propose-only; no write path | `gke` MCP is the read-only endpoint; `write_file`/`edit_file`/`delete_file` disabled |
| No shell | `bash` disabled |
| Don't hunt the filesystem | `list_dir`/`glob`/`grep` disabled |
| Plan before any change | `permissions.plan_mode: "required"` (composes with `yolo`; the gate denies mutations pre-plan) |
| Escalation goes somewhere | `alerts.targets[oncall]` — the `alert` tool registers only when a target exists *and its `url_env` resolves* |
| Bounded spend | `max_turn_cost_usd` / `max_session_cost_usd` — halts the agent on trip |
| Runaway loops get halted | `safety.watchdog: "enforce"` — a `kind=watchdog` turn error, refusing new turns until reset |
| Delegation routes by description | the `spawn_agent` schema carries the configured roster, so the persona names no specialist |
| Re-checking is bounded, never a poll loop | `tools.wait_and_verify` — `poll_allow` lists five read-only `gke_*` tools and nothing else, capped at 20 attempts / 120s |

Three things worth knowing about that table:

- **`mode: yolo` is safe here because nothing dangerous is registered.** Yolo
  auto-approves what exists; with no shell, no writes, no fetch and a read-only
  MCP, that set is empty. The safety comes from the toolset, not the mode.
- **One row is narrower than it looks.** Disabling `list_dir`/`glob`/`grep` makes
  the *search* loop unreachable, but `read_file` and `read_many_files` stay
  registered — the `cluster` subagent needs them for its skills. A *targeted*
  read of a known path is still possible, and under `yolo` a path outside the
  project root is auto-approved rather than prompted. The persona's "don't read
  `/proc/self/environ`, don't read your own config tree" bullets are therefore
  genuinely requested, not enforced. They earn their place anyway: a 2026-08-13
  run burned 22 turns and $0.73 doing exactly that, and the real fix was
  `env.yaml` — so the values are substituted and there is nothing to hunt for —
  not a tool ban.
- **The `cluster` subagent inherits all of it, then narrows further.** A
  declarative subagent draws from the parent's already-gated catalog, so one
  `tools.disable` list hardens parent and specialist together. On top of that the
  spec pins an explicit `tools:` allowlist, because inheriting the parent's
  *whole* built-in registry gave the specialist two tools it has no business
  holding, and a live run showed why:
  - `spawn_agent` — it tried to delegate the investigation to another `cluster`,
    and only [#742](https://github.com/go-steer/core-agent/issues/742)'s
    self-spawn guard stopped it. Scoping the surface makes that guard a backstop
    rather than the only thing in the way.
  - `alert` — paging the on-call is an escalation decision the platform agent
    makes *from* the specialist's report, not one the specialist makes mid-read.

  `record_plan` is deliberately **kept** on both. The parent's plan states the
  decision it is about to make ("delegate to `cluster`, packing the verbatim
  inject"); the specialist's states the investigation it is about to run. The
  plan-first *gate* is per-session, so the specialist's plan unblocks nothing the
  parent had not already unblocked — it is an intent statement, not a key.

  The general lesson: a subagent's `tools:` is worth pinning even when the
  inherited surface looks safe, because "safe" and "in scope" are different
  properties.

## The skills are native, not ported

All six `cluster/skills/*/SKILL.md` are **core-agent-native rewrites**, not the
frozen recipe's files with a warning attached. They contain no shell fences, no
`scripts/` references, and no `kubectl`/`gcloud` invocations — every step names
the `gke_*` MCP read that performs it. The `scripts/audit_cluster.sh` that came
with the originals is gone, because nothing here could ever have run it.

The difference is measurable, and `examples/internal/recipecheck` is where it
shows up. The frozen recipe carries **188 waived findings** under
[#674](https://github.com/go-steer/core-agent/issues/674)'s accept-and-disclose
ruling — imported steps that shell out under a runtime with no shell. This recipe
is **absent from `allrecipes_test.go`'s `policies` map**, which means it is
checked with **zero waivers**, and it produces **zero findings**.

That matters more than it sounds. Leaving shell in the skills asks the *model* to
translate at read time, every time — which costs turns, invites the agent to go
hunting for a shell it does not have, and quietly turns a documented procedure
into an improvised one.

`gke-workload-troubleshooting`'s reporting step got the sharpest edit: upstream it
told the subagent to withhold the RCA from its reply and report only through the
kanban task — precisely the content-free-summary failure this recipe exists to
avoid. Its Step 6 now names `return_result` as the hand-off, in the same words
`cluster/AGENTS.md` uses, so the persona and the skill cannot drift into
describing two different contracts.

## Deliberately not ported

**Governance SOPs.** They are a different workload built on equipment this
deployment does not have: daily cron fleet audits, drift reconciliation, fleet
cost analysis and security-patch orchestration, spread across the 148 `gcloud`
and 73 `kubectl` invocations in
[`upstream/governance/`](../kube-platform-agent/upstream/governance/), Python
helpers, a GitOps workspace on disk, and a report
script that clones a repo and opens issues and PRs. This agent has no shell, no
filesystem writes, no `git`, no GitHub MCP and no cron. Shipping instructions the
runtime cannot execute is the exact failure this recipe exists to disprove, so
the honest move is not to ship them.

## Run it locally

The recipe's floor is **v2.9.0-dev.1** — `content_roots` and a rooted subagent do
not exist before v2.9, and an older daemon boots without either rather than
failing ([#680](https://github.com/go-steer/core-agent/issues/680)).

Every `${env:}` reference in the content is declared in `.agents/env.yaml`, and
all four coordinates are `required: true`, so the daemon refuses to start rather
than interpolating a blank:

```sh
export GOOGLE_CLOUD_PROJECT=your-project
export GOOGLE_CLOUD_LOCATION=global          # Vertex endpoint region
export GKE_CLUSTER=your-cluster
export GKE_LOCATION=us-central1              # where the cluster actually lives
gcloud auth application-default login

core-agent -c examples/gke-platform-agent/.agents/config.json
```

`GOOGLE_CLOUD_LOCATION` and `GKE_LOCATION` are deliberately separate. Folding them
together sends `gke` calls to the wrong place and Vertex calls to a region that
may not serve the model.

Try, in order:

1. **"who are you?"** — should get a direct answer, not an incident report. This
   is the identity probe the whole rewrite exists to pass.
2. **"what's wrong with the `<some-workload>` deployment?"** — should record a
   plan first, then delegate to `cluster` with `wait: true`.
3. **"apply that fix"** — should refuse and explain that the proposal *is* the
   deliverable, without going looking for a repo to edit.

## Deploy it to a cluster

The recipe ships its own deployment: [`deploy/`](deploy/) is a kustomize
base plus four overlays, and [`scripts/`](scripts/) is the operator rig that
drives them. Full walkthrough in [`DEMO.md`](DEMO.md).

```sh
./scripts/build-content-image.sh   # push the recipe content as an OCI image
./scripts/gen-tokens.sh            # bearer tokens -> Secrets
./scripts/set-up-demo.sh           # deploy hub + watcher, verify the mount
./scripts/break-workload.sh        # break a workload -> incident -> session
./scripts/attach.sh                # operator TUI
./scripts/teardown.sh
```

Deployed, the recipe is a **hub**: `config.hub.json` adds `attach.listen`
plus `multi_session` with a bearer table, and a [lookout](https://github.com/go-steer/lookout)
watcher injects one session per incident. The agent is the same in both
modes — `config.hub.json` is `config.json` plus exactly one `attach` block,
and `recipe_test.go` asserts nothing else has drifted between them.

Two decisions get made for you by probing the cluster, because both are
forced rather than preferred: content arrives as an **OCI image volume** on
GKE 1.35+ and via an **initContainer copy** below it, and **tracing** to
Cloud Trace turns on if the cluster serves GKE Managed OpenTelemetry. That
is the 2 × 2 of overlays. See [`deploy/README.md`](deploy/README.md).

## What CI proves, and what it does not

`recipe_test.go` runs as an ordinary unit test — no cloud credentials, no live
cluster, no LLM. It proves the recipe is well-formed: the config loads, the
watchdog is enforced, both MCP surfaces are the read-only endpoint, the subagent
is rooted and scoped, the six skills load where the subagent looks for them, the
hub config has not drifted from the base config, and every `${env:}` reference is
declared.

It cannot score an answer. Whether this agent is any *good* at GKE is measured by
the live drill in [#970](https://github.com/go-steer/core-agent/issues/970),
against a real cluster, and nothing in CI substitutes for it.
