# `gated-apply` — the apply-capable posture

Everything that turns `gke-platform-agent` from an agent that **describes**
the fix into one that **applies** it. Design of record:
[`docs/gated-apply-design.md`](../../../../../docs/gated-apply-design.md).

Composing this component does three things, and it exists so that they
cannot be done separately:

| | |
|---|---|
| `patch-agent-config.yaml` | Repoints the daemon's `-c` at the second content root, `…/gated-apply/.agents/config.d1.json`. |
| `patch-plans-mount.yaml` | Moves the writable `plans` emptyDir to follow it. |
| `role.yaml` + `rolebinding.yaml` | The RBAC, in `TARGET_NS`. |

The RBAC half is two objects:

| Object | What it grants |
|---|---|
| `Role/gated-apply-gke-platform-agent` | `patch` on `apps/deployments`. Nothing else. |
| `RoleBinding/gated-apply-gke-platform-agent` | That Role, to the daemon's Workload Identity username. |

(The suffix is the namespace the *daemon* runs in, which `deploy/base`
hardcodes — not `TARGET_NS`, where these two objects land, and not
`DEMO_NS`. See "Two coordinates, and one string that looks like a third".)

## Three things, one opt-in

Each proper subset of the three is broken, and each fails differently:

- **Config swap without the plans remount.** `record_plan` derives its
  output directory as `dir(-c) + "/plans"`, so moving `-c` without moving
  the emptyDir points it at a path inside the read-only image volume. The
  pod is healthy, every probe passes, and the leg dies at its first plan —
  which under `plan_mode: "required"` is before its first mutation.
- **Config swap without the RBAC.** Every patch returns 403. The agent
  behaves as designed right up to the API server.
- **RBAC without the config swap.** The one combination that is both
  useless and dangerous: the daemon holds patch rights on `TARGET_NS` and
  is running a configuration that was never meant to use them. Nothing
  reports this; it looks exactly like the read-only posture.

`kustomization.yaml` carries the same list next to the wiring.

### `d1` and `d2`

`d1` is attended — the approval gate is on, and a human answers it. `d2`
is unattended. The committed patch selects **`d1`**: if a rendered manifest
is deployed without anyone reading it, the posture it lands in should be
the one that still asks. Switch to `d2` by editing the one `value:` line
in `patch-agent-config.yaml`, which is what `LEG=d2 scripts/set-up-demo.sh`
does for you.

Both patches open with a JSON6902 `test` op naming the index they are
about to rewrite. That is not decoration: kustomize *enforces* `test`, so
reordering the base's `args` or `volumeMounts` fails the build with
`testing value … failed` instead of silently patching the wrong element.

## Why this is the whole security argument

The recipe's unattended leg runs with no human approving anything. At that
point the config file is not a boundary. An allowlist in `config.json`
tells the *model* what it may ask for; it does not stop a wrong decision,
and a prompt injection in a pod log routes around it eventually.

What does not route around it is what the API server refuses. So the
claim "this agent can fix one namespace and cannot touch anything else"
has to be true at the cluster, and this component is where it is made
true.

The Role lives in the **target** namespace — the one the agent is pointed
at — not the namespace the agent runs in. A Role is namespace-scoped, so
"cannot cross namespaces" becomes a property of the object rather than a
rule someone has to remember. Measured on `std-simian-test`, 2026-09-16:

| Request | Result |
|---|---|
| `PATCH` in `TARGET_NS` | **404** — authorized (the object does not exist) |
| `DELETE` in `TARGET_NS` | **403** — the Role grants `patch`, and only `patch` |
| `PATCH` in `default` | **403** |
| `PATCH` in the agent's own namespace | **403** |

The last row is the nice one: the agent cannot patch the namespace it runs
in either, and nobody had to remember not to grant that.

### What the boundary does *not* narrow

The grant carries no `resourceNames`, so it is `patch` on **every**
Deployment in `TARGET_NS`. Patching a Deployment is an arbitrary pod-spec
write — image, command, volumes, and `serviceAccountName` — so inside
that one namespace the agent can schedule a workload as any ServiceAccount
the namespace has. "Cannot touch anything else" is a claim about the
namespace boundary, which is measured above; it is not a claim about the
blast radius within `TARGET_NS`.

That is the right trade for a demo namespace running a sample workload,
and the wrong one for a namespace that holds credentials. The stated
threat model here is a prompt injection arriving in a pod log, so if you
point this at anything real, scope the Role with `resourceNames` to the
Deployment the drill actually repairs. Note that the grant does not reach
`deployments/scale` or `deployments/status` — subresources are not
implied by the parent resource.

## Composing it

Two overlays already do:

| | |
|---|---|
| [`overlays/gated-apply`](../../overlays/gated-apply) | tracing off |
| [`overlays/gated-apply-otel`](../../overlays/gated-apply-otel) | tracing on |

Both build on `overlays/example`, so the apply leg is image-volume only —
see `deploy/README.md` for why there is no `initcontainer-copy` variant.
In your own overlay it is one stanza:

```yaml
components:
  - ../../components/gated-apply
```

Note the ordering, which is why the config swap lives here rather than in
an overlay: a component's `patches:` are applied *after* those of the
overlay composing it, so this component's `-c` patch wins over
`overlays/example/patch-agent-config.yaml`. That is the same
"outer transformer wins" rule that makes the `*-otel` composers omit
`images:`.

It is deliberately **not** in `deploy/base`. Base is every deployment's
default posture, and this recipe's default posture is read-only — drill
scenarios A/B/C depend on it, and scorecard item G4 is literally "no
mutating call reaches the cluster". Patch rights are something an operator
opts into.

It is also deliberately not a manifest you `kubectl apply` by hand. These
objects live in `TARGET_NS`, so `kubectl delete namespace ${DEMO_NS}` does
not reach them; applied by hand they survive teardown *and* a full
rebuild, leaving the daemon holding patch rights on a cluster the operator
believes is clean. `scripts/teardown.sh` deletes them, which only works
because they have predictable, namespace-suffixed names.

## Two coordinates, and one string that looks like a third

| String | Becomes | Where |
|---|---|---|
| `your-target-namespace` | `TARGET_NS` | `metadata.namespace` in both files |
| `your-project-id` | `PROJECT_ID` | the subject name in `rolebinding.yaml` |
| `gke-platform-agent` | **nothing — leave it** | both object names, `roleRef.name`, and the subject's bracket |

`scripts/set-up-demo.sh` rewrites the first two, but only when the overlay
it is deploying actually composes this component. It detects that from the
rendered manifest rather than from a `components:` line, because the
`*-otel` overlays compose through `../example` — a `components:` line in
`overlays/gated-apply-otel/kustomization.yaml` names `otel-gke`, not this
one.

The detection is `renders_gated_apply` in `scripts/prereqs.sh`, which asks
whether the render contains a **`Role` named `gated-apply-*`**. It used to
ask whether the render contained the string `gated-apply` anywhere, and
that was wrong: the `initcontainer-copy` overlays copy the whole content
image, whose `cp -a` list names `/gated-apply` — the second content root
ships in every flavor of the image whether or not an overlay selects it.
A read-only below-floor deploy therefore announced that it had substituted
coordinates into RBAC it was not applying. Nothing was mis-granted, since
`kubectl` applies only what the overlay renders, but it rewrote two tracked
files and printed a claim about authorization that was not true.

The third row is the one to understand. `gke-platform-agent` is *not* a
stand-in for `DEMO_NS`: it is the namespace `deploy/base` hardcodes for the
daemon, in `00-namespace.yaml` and `10-serviceaccount-daemon.yaml`.
`DEMO_NS` is only ever a `kubectl -n` argument — nothing substitutes it
into `base` — so the daemon lands in the literal namespace no matter what
`DEMO_NS` says, and this bracket is therefore unconditionally correct as
committed. Rewriting it from `DEMO_NS` would *introduce* the silent
failure below rather than prevent it. Three things keep that honest:

- The recipe test `TestGatedApplySubjectMatchesDaemonNamespace` reads the
  daemon's ServiceAccount out of `base` and fails if the bracket stops
  agreeing with it.
- `require_demo_ns_matches_base` in `scripts/prereqs.sh` refuses to run at
  all when `DEMO_NS` has been overridden away from what `base` hardcodes,
  because an override also desynchronises the Secrets `gen-tokens.sh`
  creates, the Workload Identity grants in `grant-iam.sh` and the names
  `teardown.sh` deletes. Every script that *creates, grants or deletes*
  anything named from `DEMO_NS` calls it before the first `kubectl` —
  `set-up-demo.sh`, `teardown.sh`, `gen-tokens.sh`, `grant-iam.sh`,
  `debug-pod.sh` and this component's `verify-gated-apply.sh`. The last two
  read as diagnostics, but both create a Pod, and `debug-pod.sh` sets no
  `serviceAccountName`: it runs as `default`, which exists in every
  namespace, so under an override the create *succeeds*, in a namespace
  this recipe does not own. The genuinely read-only ones (`attach.sh`,
  `break-workload.sh`, `build-content-image.sh`) do not call it, because
  they fail loudly and harmlessly against an empty namespace — and neither
  does `dev/uat/gke-drill`, which sources `prereqs.sh` and needs to run
  every coordinate, `DEMO_NS` included, at a value of its own.
  `TestDemoNSGuardCoversEveryMutatingScript` pins the split, the call
  order, and the requirement that a new script sourcing `prereqs.sh` be
  classified one way or the other. It matches what the shell will
  *execute*, not what the file contains: comments, quoted spans and
  heredoc bodies are blanked first, so a guard that exists only in a
  `-h` usage block does not count as a call. The scanner that does the
  blanking has its own test, and every case in it is also run through
  real bash — it may blank code, but it must never keep something the
  shell would treat as data.
- `set-up-demo.sh` compares the *rendered* manifest against `DEMO_NS`
  before it applies anything — unconditionally, not only when this
  component is composed in — then compares the bracket against that same
  render, and re-derives the namespace once more from the final render
  after its own edits, so an overlay that relocates the daemon some other
  way is caught too.

**Note that this rewrites files tracked by git** — `role.yaml`,
`rolebinding.yaml`, and on a `LEG=d2` run the `-c` value in
`patch-agent-config.yaml`. That is the same property the overlay patches
have, and it is why a `git status` after a deploy is dirty. It also turns
this component's own recipe tests red in your checkout, since they assert
the committed values. Revert `deploy/components/gated-apply/` before
committing or running `go test`.

Which one bites depends on the path you take:

- **`your-project-id`** is the one that is *caught*, and only on the
  scripted path: `set-up-demo.sh`'s `LEFTOVER` grep scans the rendered
  manifest for `your-` strings and exits 1 before applying anything.
- **`your-target-namespace`** is **not** checked by anything.
  `require_coordinates` in `scripts/prereqs.sh` inspects the environment
  variables `PROJECT_ID`, `CLUSTER_NAME`, `KUBE_CONTEXT` and `REGION` — it
  never reads a manifest, and `TARGET_NS` is not in that list. It is also
  absent from the `LEFTOVER` grep. Applied unsubstituted, `kubectl` errors
  on the missing namespace, but only after everything else has landed.
- **`gke-platform-agent`** is not substituted and must not be, per the
  section above. Nothing at apply time could flag it if it were wrong —
  it is a working value rather than a `your-*` string — which is why it is
  checked in CI against `base` instead.

A wrong subject, however it gets there, is the failure mode to know
about: RBAC does not match, and there is no error, no event, no log line
anywhere. The agent's patch keeps returning 403 exactly as if the
component had never been applied, and the only symptom is an agent that
mysteriously cannot do the one thing you deployed it to do.

On the `kubectl apply -k` path **none** of these checks run. There is no
placeholder gate outside `set-up-demo.sh`; a RoleBinding carrying the
literal `your-project-id` applies cleanly and never matches. Run the
verification below.

## Verifying it

```sh
./scripts/verify-gated-apply.sh denied     # before applying — baseline
./scripts/verify-gated-apply.sh            # after applying  — the grant
```

That script is the only valid check *of the RBAC leg* — it talks to the
API server directly, so a green run says the daemon's identity is
authorized, not that the GKE MCP endpoint will issue the patch on its
behalf. The endpoint's tool surface and the mounted `tools` allowlist sit
in front of this grant; the drill scenario is what exercises the whole
path. In particular **do not** use
`kubectl auth can-i --as=<subject>`: on GKE that exercises only the RBAC
authorizer, while IAM arrives through a webhook keyed to the
*authenticated* identity, so it disagrees with reality in both directions
— it has been observed answering "no" to a read the real token is granted.
Only a real token proves anything here, which is why the script runs a pod
as the daemon's ServiceAccount.

It is safe against a live cluster: every request targets a Deployment name
that does not exist. Authorization is evaluated before existence, so
authorized returns 404 and denied returns 403, and nothing is created,
changed or deleted either way.

## Why the subject is `kind: User` and not `kind: ServiceAccount`

Compare `deploy/base/13-clusterrolebinding-watcher.yaml` and
`15-rolebinding-watcher-capacity.yaml`, which both bind
`kind: ServiceAccount`. Those are the **watcher**, which holds a projected
KSA token and talks to the API server directly, authenticating as
`system:serviceaccount:<ns>:<name>`.

The daemon does not work that way. It reaches the cluster through the GKE
MCP endpoint, which forwards the daemon's **Google** principal, and the
API server resolves that to a Workload Identity username:

```
serviceAccount:<PROJECT_ID>.svc.id.goog[<NAMESPACE>/core-agent-daemon]
```

`<NAMESPACE>` is the daemon's own namespace, the one `deploy/base`
hardcodes — not `DEMO_NS`, and not `TARGET_NS`.

Binding `kind: ServiceAccount` here would be silently inert. So would the
`principal://iam.googleapis.com/…` direct-binding form that appears in the
daemon's IAM policy — that form is what the design originally guessed, and
it was wrong. The string above was recovered from a live 403 (GKE names
the principal in its own denial message) rather than constructed. If you
ever need it for a different cluster, recover it the same way; do not
build it from parts.
