# `gated-apply` — the same agent, allowed to apply the fix it found

A **second content root** for this recipe. Everything above this directory is
propose-only: the agent diagnoses, writes a patch into its report, and a human
applies it. This tree is the same agent with exactly one write verb, and it
exists to answer one question — *when the agent is finally allowed to change
something, does it change the right thing, and only that?*

Design of record: [`docs/gated-apply-design.md`](../../../docs/gated-apply-design.md).
Box A6 of [#1042](https://github.com/go-steer/core-agent/issues/1042), tracked
by [#1105](https://github.com/go-steer/core-agent/issues/1105).

```
AGENTS.md              # variant persona — propose-only except in 7 marked regions
.agents/
  config.d1.json       # leg D1: mode ask — the patch needs a human
  config.d2.json       # leg D2: mode allow — the patch is allowlisted, nothing prompts
  mcp.json             # the FULL gke endpoint, 16 tools, one of them mutating
  env.yaml             # byte-identical to ../.agents/env.yaml
  plans/.gitkeep       # pre-baked mount POINT — nothing mounts it yet, see below
```

No `cluster/`. The subagent's root is `"../../cluster"`, which resolves back to
the recipe's one shared `cluster/` tree. That subagent stays read-only in both
legs, and it stays that way because it keeps its own read-only `mcp.json` — not
because this persona asks it nicely.

## Running it

```sh
core-agent -c examples/gke-platform-agent/gated-apply/.agents/config.d1.json
```

Nothing loads this tree unless `-c` points into it. The read-only recipe's
posture is completely unchanged by its presence — including in the content
image, which ships both roots and lets the deployment choose.

**On a cluster, not yet.** No overlay selects this leg, and pointing `-c` at
it without the rest of the follow-up will not work: `record_plan` derives its
output directory as `agentsDir + "/plans"`, so selecting this tree moves the
plans dir to `gated-apply/.agents/plans` — a path that sits on the *read-only*
content mount, because `deploy/base` nests its writable emptyDir at the base
recipe's `.agents/plans` and nowhere else. `plans/.gitkeep` pre-bakes the
mount point, which a read-only layer needs and which is not the same as a
mount. Both legs run `plan_mode: "required"`, so this is not a lost artifact;
it is the leg failing at its first plan, on the one guarantee the whole design
rests on. The `-c` swap and the plans remount are the same follow-up, and
`TestGatedApplyOverlayMustRemountPlans` fails if either lands without the
other. Until then this tree runs locally, where `plans/` is just a directory.

## Why a directory and not a third config file

Both reasons are path resolution, and both are the kind of thing you discover
by reading the loader rather than by reasoning about what *ought* to work.

**`mcp.json` is found by a fixed name.** `pkg/mcp.MCPFileName` is the constant
`"mcp.json"`, looked up inside the agents dir. No config field names a
different MCP file. Two MCP surfaces therefore need two agents dirs, full stop.

**`AGENTS.md` is loaded from `dir(agentsDir)`.** `agentsDir = dir(-c)` and
`projectRoot = filepath.Dir(agentsDir)`, so the extra directory level is also
what lets this leg have its own persona — which it needs, see below.

A corollary worth stating because the design doc got it wrong first: `mcp.json`
ships in the **content image**, not the deploy tree, so it could never have
been a kustomize patch even if the filename were configurable.

## The three deltas in `mcp.json`, and why each is load-bearing

Against `../.agents/mcp.json`:

**1. The endpoint loses its `/read-only` suffix.** `container.googleapis.com/mcp`
rather than `.../mcp/read-only`. The read-only sibling does not serve a
mutating verb at all, so no amount of configuration below this line could have
reached one through it.

**2. `read_only` is gone — absent, not `false`.** This is the subtle one.
`read_only` is a *server-level* declaration stamped onto every tool the server
serves, and plan-first is exempted for read-only-classified tools. So
`read_only: true` on a server carrying `patch_k8s_resource` would not just be
a false claim: it would silently exempt the patch from `plan_mode: required`,
cancelling the one structural guarantee this leg depends on. And `read_only:
false` is wrong in the other direction — it would force the fifteen reads onto
the mutating path and plan-first would deny the research the agent needs in
order to write a plan it is then allowed to execute. Absent is correct:
[#1098](https://github.com/go-steer/core-agent/issues/1098) makes the
per-tool `readOnlyHint` the primary signal, with the server declaration as a
fallback and fail-safe-mutating below that.

**3. `tools` appears at all**
([#1100](https://github.com/go-steer/core-agent/issues/1100)). The full
endpoint serves 23 tools; the base recipe has no `tools` field, so its parent
gets all 15 that the read-only twin serves. The allowlist exists to exclude
the mutating verbs *other than* patch — `apply_k8s_manifest` takes an
arbitrary manifest, so there is no useful upper bound on what it can create;
`delete_k8s_resource` fixes nothing this agent diagnoses and is not
recoverable by a retry.

**Narrowing the reads is never the intent, and it is the easy mistake.** This
list first shipped with five reads, which would have given scenario D's agent
a strictly smaller read surface than scenario A's — a confound in the
comparison the drill exists to draw, silently, in the direction that flatters
the apply leg. So the list is **all 15 reads the read-only twin serves**, and
the rule is parity, not curation: `list_clusters` is included even though the
persona forbids calling it, because matching A/B/C matters more than tidiness
and the persona is what stops it either way.

Getting there took two passes, and the second one is the lesson. The list was
first assembled from evidence — five names from this recipe's drill
transcripts, `check_k8s_auth` from the 2026-09-10 C runs, the rest from
`examples/gke-troubleshoot-agent`, which drives the same endpoint — and
`TestGatedApplyKeepsEveryReadTheRecipeNames` was written to hold it there,
scanning the persona, the subagent's skills, **and** the recorded drill runs.
That third source is not decoration: `check_k8s_auth` appears in none of the
recipe's own content, so a content-only scan let a narrowed list pass.

It was still short by two. `get_k8s_cluster_info` and `get_k8s_version` are
served by the endpoint, are named nowhere in this recipe, and had never been
called in a recorded run — so all three sources agreed a 13-entry list was
complete. The scan can only ask *is every read we mention registered?*; the
question that matters is *is every read the endpoint serves registered?*, and
no amount of scanning our own content answers it.

`TestGatedApplyRegistersTheWholeReadOnlyCatalog` is the strong form, and it
needs a real catalog. There is one: `examples/gke-parallel-triage` drives the
same read-only endpoint and enumerates it by category under "Tool palette you
have". Depending on another recipe's persona is unusual and deliberate — it is
the only enumeration in the repo, its accuracy is load-bearing for that recipe
independently of this one, and it checks out arithmetically against
`docs/site/.../concepts/mcp.md`'s count of 23 tools on the full endpoint: 15
reads leaves exactly the 8 mutating verbs (the three k8s writes,
`update_cluster`, and the four cluster/node-pool lifecycle verbs). The test
asserts equality in **both** directions, because a name the endpoint does not
serve is only a startup warning — a typo costs a read and fails nothing.

Two mechanical notes on `tools`: the entries are the server's **own,
unprefixed** names (filtering happens before the namespace rename, so
`get_k8s_resource`, not `gke_get_k8s_resource`), and an entry matching nothing
is a **startup warning, not an error**. See §First run for why that matters.

The `get_k8s_resource` fidelity note carries over from the base recipe
verbatim, and a test asserts it stays that way.

## The two legs

They differ by two config fields, but the *experiment* is one line: whether
`mcp:gke_patch_k8s_resource*` is on the allowlist.

| | D1 | D2 |
| --- | --- | --- |
| `mode` | `ask` | `allow` |
| patch allowlisted | no | yes |

Everything else — the fifteen reads, `spawn_agent:cluster`, `alert:oncall`,
`plan_mode: required`, the disabled builtins, the cost ceilings — is identical,
and a test enforces that the only permitted diff between the two files is
`permissions` and the display name.

**D1 is missing the two fields that make `ask` operable, and that is a
deferral, not a design choice.** `permissions.approval_timeout` and
`permissions.approval_notify` are what stop a gated daemon from prompting into
a void: without them D1 is `mode: ask` in a pod with nobody attached, and the
first gated call waits forever. They belong here. They are absent because
`recipecheck` derives a recipe's minimum-version floor as a union over *every*
`config*.json` the recipe ships (`examples/internal/recipecheck/imagepin.go`),
both fields require ≥ `2.10.0-dev.1`, and this repo has not cut that version —
so declaring them in D1 would raise the floor above the pin and fail the
*read-only* overlays too. Shipping them would make the whole recipe
undeployable to buy a field nobody can run yet.

`TestGatedApplyD1GainsApprovalFieldsWhenThePinAllows` is what keeps the wait
from becoming permanent. While the lowest overlay pin is below the gate it
asserts both fields are absent; the moment the pins move past it, the test
fails and names the values to add (`"10m"` and `"oncall"`) and the two
documents to update alongside them. Until then, run D1 attended — an operator
watching `/perms/stream` *is* the answer channel, which is the case the gate
was built for and the one that needs neither field.

**Why `allow` and not `yolo`.** `ModeAllow` denies anything unlisted and
*never prompts* (`pkg/permissions/gate.go:1133`). That is deny-by-default with
no human in the loop — the unattended posture, without yolo, and without a
prompt that can hang forever in a pod with nobody attached. A tool that is
neither allowlisted nor granted is refused, not queued. D2 is therefore the
genuinely unattended leg, and it is more constrained than the recipe's current
default, not less.

**Why the reads are allowlisted in both legs.** They have to be. `readOnly` is
threaded into `gateRequest` for exactly one consumer — `planFirstDenial`
(`gate.go:1095`) — and after that pre-check the policy match and mode switch
treat read-only and mutating calls identically. Under D2 an unlisted read is
refused outright; under D1 it prompts. Unlisted, D2 would boot an agent that
cannot read the cluster at all, and D1 would ask the operator to approve thirty
reads before reaching the one decision worth their attention. Allowlisting them
is also what makes D1 an experiment about the patch rather than about approval
fatigue.

`spawn_agent` and `alert` are gated buckets too, keyed by subagent name
(`pkg/agent/subagent.go:360`) and target name (`pkg/tools/alert/alert.go:172`),
so they need entries for the same reason. `record_plan`, `todo` and
`wait_and_verify` are not gated and need none.

### The allowlist grammar, which is easy to get silently wrong

`"mcp:gke_patch_k8s_resource*"` — every part of that string is doing work.

- **`mcp:`** is the *bucket*. Rules split on the first `:`; what follows is
  matched against the key, not against a namespaced tool identifier.
- **`gke_`** comes from the `"gke"` server key in `mcp.json`
  (`pkg/mcp/namespace.go`). Rename the server and every pattern here breaks.
- **The trailing `*`** is load-bearing. `pkg/tools/gate.go`'s
  `summarizeRequest` builds the key as `name + " " + json(args)`, truncated at
  200 bytes. Every call the model actually makes carries arguments, so a
  pattern matching only the bare name is **inert in practice while reading
  correctly in review** — the worst failure shape available here.

`TestGatedApplyAllowlistAuthorizesExactlyTheIntendedCalls` runs the real
patterns through `permissions.NewPolicy` against a realistic marshalled args
blob, precisely so that trap is caught in CI rather than mid-incident.

## The persona is part of the leg

The base `AGENTS.md` is propose-only in seven distinct passages — the
write-path rule, the MCP surface description, the `wait_and_verify` guidance
("not part of incident handling… does not poll"), the mutation section, the
plan-first framing, the incident-close checklist, and the finish line.
Shipping the apply leg with that persona would register the patch tool and
then instruct the model not to use it, and the run would read as *"the model
chose not to apply"* — a confounded result, not a negative one. Scenario D has
to be scenario A **plus apply**, or the A/B/C comparison means nothing.

So this tree carries a variant, and the two are kept honest by **marked stance
regions**: seven `<!-- stance:begin NAME -->` / `<!-- stance:end NAME -->`
pairs present in *both* files, wrapping exactly the passages allowed to
differ. Three tests enforce the arrangement:

- the two personas are byte-identical outside the markers;
- the same regions appear in the same order in both;
- every region's content actually *differs* — which catches a `cp` that was
  never edited. That one matters: a pure copy passes the identity check while
  telling the apply agent it may not apply.

To change anything outside a region, change it in both files. To change the
stance, change it inside the region. If a new passage needs to diverge, add a
new marker pair to both — the region-name test will tell you if you only did
one.

The line this persona can say that the base cannot: *"applied" is true only
when the patch call itself returned success; "resolved" is true only when a
read taken afterwards shows the workload healthy.* That distinction is why
`wait_and_verify` comes back in this leg, and why the honest intermediate
report is "patch applied; recovery not yet verified".

## What is a boundary here, and what is only a persona

The persona says "do not substitute an in-reach patch for an out-of-reach
fix". That is guidance, and guidance is not a gate. What actually holds:

| Claim | What makes it true |
| --- | --- |
| Only one write verb exists | `tools` in `mcp.json` — the others are never registered |
| No shell, no filesystem writes | `tools.disable` |
| No patch before a recorded plan | `plan_mode: required`, and `read_only` is absent so the exemption does not fire |
| No patch outside one namespace | a Role in the **target** namespace, bound to the daemon's Workload Identity user — `deploy/components/gated-apply/` |
| The subagent cannot mutate | its own read-only `mcp.json`, at its own content root |
| (D1) no patch without a human | `mode: ask` plus the absence of an allow entry |

The namespace boundary is confirmed, not inferred: the binding was applied on
the drill cluster, the same `PATCH` flipped 403 → 404, and `delete` plus both
other namespaces stayed 403. See §Probe status in the design doc.

## First run

Two things this tree asserts that no run has confirmed yet. Both are cheap to
check and both fail quietly:

1. **Is `patch_k8s_resource` the server's real name for that verb?** Every
   other tool in the allowlist appears in a recorded drill transcript; this one
   cannot, because no leg has ever been permitted to call it. An unmatched
   `tools` entry is a **warning**, so a wrong name boots a perfectly healthy
   agent with no write path, and you find out mid-incident. Read the daemon's
   startup warnings first — the warning lists what the server did expose,
   which is also the one authoritative check on the 15 reads: the catalog they
   are held against is another recipe's prose, accurate as far as anyone can
   tell from here, but not the server's own answer. A silent run means all 16
   entries matched.
2. **Does the endpoint publish `readOnlyHint` on all its tools?** The
   plan-first composition depends on it, and #1098's fallback is fail-safe
   mutating. If the annotations are missing, the reads classify mutating and
   plan-first denies the research. This shows up immediately, but know to look
   for it.
