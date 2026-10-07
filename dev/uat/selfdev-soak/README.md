# Self-development soak: image, config overlay and the A7 rig

This directory holds the self-development soak's rig
([#1213](https://github.com/go-steer/core-agent/issues/1213),
[`docs/selfdev-soak-design.md`](../../../docs/selfdev-soak-design.md)): prerequisite **P3**, an
image that can do development work, and phase 2's minimal rig for A7,
which deploys that image as a daemon and a dispatcher on GKE.

| File | What it is |
|---|---|
| `Dockerfile` | The soak image: core-agent, the dispatcher and the self-recipe, all from one pinned upstream ref. |
| `build-image.sh` | Builds it from a pinned ref. It pushes only when given `--push` and `--registry`. |
| `config.soak.json` | The soak's config overlay: the committed recipe plus the auto-mode and multi-session deltas. |
| `dispatcher/` | The minimal dispatcher ([design](../../../docs/selfdev-soak-dispatcher-design.md)). |
| `deploy/` | The kustomize rig for namespace `core-agent-selfdev`, rendered with `deploy/render.sh`. See [Deploying the A7 rig](#deploying-the-a7-rig). |
| `seed.sh` | Creates seed issues in the mirror from the private seed list. See [Seeding](#seeding). |
| `grade_a7.*` | The A7 grader, built separately. |
| `*_test.go` | Checks that the files above still follow the rules on this page. They run in `test-unit`. |

## Why it is separate from the release image

The release image (`/Dockerfile`) is distroless. It has no shell, git, Go or
`gh`, which is right for a daemon that operates a cluster and wrong for one
that edits and tests a Go repository. The soak needs a POSIX shell and
coreutils for the `bash` tool, plus git, the Go toolchain and CA certificates.
Adding those to the release image would put a shell and a compiler in every
production deployment to serve one internal soak. So the soak gets its own
Dockerfile, and the release image stays as it is.

The image contains:

- **The pinned Go toolchain.** It is the version `dev/tools/verify-go-toolchain --print`
  resolves from `go.mod`, and that script's static check covers this
  Dockerfile. The base is `golang:<version>-bookworm`, pinned by its
  multi-arch index digest. The builder fails if the digest's Go is not
  `GO_VERSION`. `GOTOOLCHAIN=local`, so `go` never downloads a different
  toolchain. If the mirror's `go.mod` moves past this toolchain, its builds
  fail loudly until the image is rebuilt.
- **git, `sh`/`bash`, coreutils, ca-certificates, python3 and gcc** (for
  `go test -race`), all from the same base image, plus **jq** from
  bookworm's archive. jq is the one package not pinned by digest.
  `verify-gke-drill` needs both jq and python3, and the presubmit sweep runs
  it.
- **`GOPATH` on the Go cache volume** (`/cache/go/path`), with its `bin`
  **last** on `PATH`. Dev tools that the presubmits `go install`
  (golangci-lint, govulncheck) then survive a pod restart. The directory is
  writable and outlives each issue's session. If it came first on `PATH`,
  one session could shadow `git` or `go` for every later issue.
- **No setuid or setgid binaries.** The runtime stage strips every such bit
  (`su`, `passwd`, `mount`, ...).
- **A core-agent binary built from a pinned upstream ref** (next section).
  A tag build reports the tag from `--version`. A SHA build reports the
  tree's own version string and the SHA as its commit, because attach peers
  read the version as a semver. The resolved commit is the image's
  `org.opencontainers.image.revision` label. The ref and the commit are also
  written to `/usr/local/share/core-agent-soak/`.
- **No `gh`, and no GitHub credential.** The agent stops at local commits.
  The dispatcher, in its own pod, pushes and opens the PR with the writer
  App's token (design decision 7).
- **The dispatcher**, `/usr/local/bin/selfdev-soak-dispatcher`, built from
  the same checkout as core-agent. The dispatcher's pod runs this image
  with its command overridden, so one digest pins both processes. The ref
  must contain `dev/uat/selfdev-soak/dispatcher` (upstream `61f00731` or
  later), or the build fails.
- **The self-recipe**, the ref's repo-root `AGENTS.md` and `.agents/` tree,
  at `/usr/local/share/core-agent-soak/recipe/`. The daemon's
  `--agents-dir` points into it (next section), so the instructions the
  agent runs under are pinned with the binary, and read-only.
- **A non-root user**, `soak`, uid/gid `10001`, with `HOME=/home/soak`.

The base image also brings the rest of `buildpack-deps:bookworm-scm`,
including `curl`, `wget` and `openssh-client`. The image doesn't restrict
egress; the namespace overlay's NetworkPolicy does. The design names Vertex,
GitHub and switchboard. The presubmits also reach the Go module proxy and
checksum database (`proxy.golang.org`, `sum.golang.org`) and `vuln.go.dev`,
so the policy has to allow those too, or a warm module cache has to cover
everything.

## The pin rule (decision 11)

**The daemon runs a pinned upstream build, never the mirror's `main`.**
Otherwise every reviewer merge in the mirror would change the agent being
measured.

- `CORE_AGENT_REF` is a required build argument with **no default**. The
  build fails without it.
- It must be a `go-steer/core-agent` **release tag** (`vX.Y.Z` or
  `vX.Y.Z-pre`) or a **full 40-character commit SHA**. Branch names, short
  SHAs and anything else are refused, by `build-image.sh` before docker
  starts, and again by the Dockerfile.
- A tag is fetched as `refs/tags/<ref>`, so a branch can't answer for it.
  A SHA must be an ancestor of upstream `main`. GitHub serves any object in
  the repository's network by id, unmerged pull-request heads included, so
  being immutable doesn't make a SHA an upstream build. For a commit that
  exists only on a release branch, pass its tag.
- The source is cloned from `https://github.com/go-steer/core-agent.git`
  inside the build. The build context is an empty directory, and the
  Dockerfile copies nothing from it, so neither the mirror nor a local
  checkout can get into the image.
- **A tag isn't proof of a commit.** `go-steer/core-agent` has no tag
  protection, so a tag can be moved. `build-image.sh` resolves the ref to a
  commit with `git ls-remote` and passes it as `CORE_AGENT_COMMIT`, which
  also has no default. The build fails if its checkout is a different
  commit, which catches a tag that moved between the lookup and the build.
  That commit becomes the revision label. Deploy by image digest, and
  record the commit beside it.
- Upgrading the ref is a deliberate soak event. Rebuild with the new ref,
  roll the daemon, and log the change on #1213 with both refs.

The ref has to be new enough for the overlay. `permissions.mode: "auto"` and
`eligible_bundles` came after `v2.10.0-dev.1`, and a daemon built from that
tag refuses the overlay with `unknown permissions.mode "auto"`.

## Building

```bash
dev/uat/selfdev-soak/build-image.sh --ref <release tag>         # local only
dev/uat/selfdev-soak/build-image.sh --ref <sha> \
    --push --registry us-docker.pkg.dev/<project>/<repo>          # and push
```

The local tag is `core-agent-selfdev-soak:<ref>`, and a push goes to
`<registry>/core-agent-selfdev-soak:<ref>`. `--push` without `--registry` is
refused, and so is `--registry` without `--push`. `--platform linux/amd64`
builds for GKE nodes from another architecture. The `:<ref>` tag is mutable,
because a rebuild overwrites it. The script prints the resolved commit when
it builds. After a push it prints the registry digest. Deploy by that
digest, and record it with the commit.

A direct `docker build` needs both `--build-arg CORE_AGENT_REF=...` and
`--build-arg CORE_AGENT_COMMIT=<the commit it resolves to>`.

When `go.mod`'s toolchain moves, `verify-go-toolchain` and this directory's
test both fail until the Dockerfile catches up. Update `ARG GO_VERSION` and
both `FROM` tags, then refresh the digest:

```bash
docker buildx imagetools inspect golang:<version>-bookworm   # the index "Digest:"
```

## Running: mounts, args, env

The entrypoint is `core-agent -c /etc/core-agent-soak/config.json`. The config
is pinned in the image, not left to discovery. `config.Find` walks up from
the working directory, and the working directory is a clone of the mirror,
whose committed recipe runs `permissions.mode: "ask"`. An unpinned daemon
would run that posture without any error. With `-c` in the entrypoint, a
missing overlay stops the daemon at startup with exit 2:
`core-agent: read /etc/core-agent-soak/config.json: open /etc/core-agent-soak/config.json: no such file or directory`.

One consequence: `core-agent attach` and `core-agent ls` dispatch only as
the first argument. Through this entrypoint they are not subcommands, and
the arguments after them are ignored. To use them inside the pod, run
`/usr/local/bin/core-agent` directly.

| Mount | Kind | Purpose |
|---|---|---|
| `/workspace` | PVC, read-write | The dispatcher's per-issue working copies (`/workspace/issues`) and the plans directory (`/workspace/.soak/plans`). It is also the working directory. |
| `/cache/go` | PVC, read-write | Go caches: `GOMODCACHE=/cache/go/mod`, `GOCACHE=/cache/go/build`, and `GOPATH=/cache/go/path`. Keeps `go test` and the installed dev tools warm across issues and restarts. |
| `/var/lib/core-agent` | PVC, read-write | The eventlog. Pass `--session-db-path=/var/lib/core-agent/sessions.db`. |
| `/etc/core-agent-soak/config.json` | ConfigMap, read-only | The config overlay, `config.soak.json`. |
| `/etc/core-agent-users/users.json` | Secret, read-only | The hashed bearer table the overlay's `attach.multi_session` names. |
| `/usr/local/share/core-agent-soak/recipe/.agents/plans` | the workspace PVC, `subPath: .soak/plans` | The one writable spot in the recipe. `record_plan` writes here, and the dispatcher reads the same directory as `/workspace/.soak/plans` for the PR body. |

Give the pod this security context:

```yaml
securityContext:            # pod
  runAsUser: 10001
  runAsGroup: 10001
  fsGroup: 10001
  runAsNonRoot: true
containers:
  - securityContext:        # container
      allowPrivilegeEscalation: false
      capabilities: { drop: [ALL] }
      readOnlyRootFilesystem: true
```

`fsGroup` makes the PVCs writable. `readOnlyRootFilesystem: true` also
needs `emptyDir` volumes at `/tmp` and `/home/soak`, because `go build`
creates its work directory under `/tmp` and git and the Go tools write
under HOME. With those two mounts, a commit, `go build` and `go vet`
all worked in a local run with a read-only root and all capabilities
dropped (`docker run --read-only --cap-drop ALL`).
**Run the dispatcher as
uid 10001 too.** It creates the worktrees on the shared PVC. Files it
creates under another uid get group 10001 through fsGroup, but with the
default umask they aren't group-writable. The agent would then fail on
`index.lock`, its edits and its commits.

The image also sets git's `safe.directory` to `'*'` system-wide. Git
refuses a repository another uid owns, and so does `go build`'s VCS
stamping. Without it, a clone that an init container or an archive restore
created under another uid would stop every git call. The check protects one
user from another on a shared host, and this container has one user.

**The dispatcher must not trust the worktree's git config or hooks.** The
agent can write everything under `.git/` in the worktrees it works in:
hooks, `core.hooksPath`, `core.fsmonitor`, `core.sshCommand`,
`credential.helper`, `url.<base>.insteadOf` and `pushInsteadOf`. Each of
those runs code inside, or redirects the push of, the pod that holds the
writer App's token. So the dispatcher must not run `git push` in the
agent's worktree. It should fetch `agent/issue-N` into a clone of its own,
then push from there to an explicit URL, with hooks off
(`-c core.hooksPath=/dev/null --no-verify`).

`-c` also makes the config's directory the agents dir, and a ConfigMap mount
is read-only. Pass `--agents-dir` pointing at the recipe baked into the
image, so that `AGENTS.md`, the skills, the `reviewer` subagent's root and
the plans directory resolve against it:

```
args: ["--agents-dir=/usr/local/share/core-agent-soak/recipe/.agents",
       "--no-repl", "--attach-listen=:7777",
       "--session-db-path=/var/lib/core-agent/sessions.db"]
```

The recipe comes from the image rather than a clone of the mirror for
three reasons. It is pinned with the binary (decision 11), so a reviewer
merge in the mirror can't change the agent being measured. The agent can't
edit it. And no persistent clone of the mirror has to exist for the daemon
to boot: the mirror's history once held the seed list (decision 20), and
the dispatcher's per-issue copies are shallow for that reason. A daemon
started with that `--agents-dir` loads both `AGENTS.md` files, the five
skills and the `reviewer` subagent, with a read-only root and every
capability dropped (checked with `docker run --read-only --cap-drop ALL`).

The per-issue copies need the upstream release tags.
`verify-version-fallback`, part of the presubmit sweep the recipe runs,
fails with "no release tags found" in a clone that has none. That gives the
agent a red check it can do nothing about. Either the mirror carries
upstream's tags, or the dispatcher fetches them into the copy. Today's
dispatcher does neither.

The attach listener's auth is the overlay's `attach.multi_session` block:
the hashed bearer table (#1269), anonymous access off. There is no
`--attach-token` or `--attach-token-file` on the daemon. A transport token
would sit in this pod in plaintext, readable by the agent's `bash`, which
runs as the daemon's uid. And the dispatcher sends one `Authorization`
header, which can't carry both credentials.

| Env | Why |
|---|---|
| `ANTHROPIC_VERTEX_PROJECT_ID` | The Vertex project for `anthropic-vertex`. |
| `CLOUD_ML_REGION=global` | Claude 5 is served only from the global Vertex endpoint. |
| `SOAK_SWITCHBOARD_URL`, `SOAK_SWITCHBOARD_TOKEN` | The `maintainer-chat` alert target. The daemon refuses to boot without them, because `approval_notify` names that target. core-agent keeps both out of the agent's child processes. |

The daemon sets no `GIT_AUTHOR_*` or `GIT_COMMITTER_*` variables. The
dispatcher writes the soak's dedicated commit identity (decision 16, its
`--commit-name` and `--commit-email`) into each working copy's local git
config, so the agent's plain `git commit -s` produces it. An identity in
the daemon's environment would override that config, and the dispatcher
refuses to push a commit whose author, committer or sign-off isn't the
configured identity.

## The config overlay

core-agent loads exactly one config file. It doesn't merge files, so the
overlay can't add only the soak's changes to `/.agents/config.json`. Instead,
`config.soak.json` restates the committed recipe and changes only what the
design calls for:

- `model`: `anthropic-vertex` / `claude-sonnet-5` (decision 14).
- `permissions.mode: "auto"`, plus the `approval_*` fields and the `auto`
  block from the design's "The auto recipe" section. The approver is
  `claude-haiku-4-5`, eligibility is the `coding` preset plus the repo's
  presubmit and tool scripts, and `task_from` is `sa:selfdev-dispatcher`.
- `agent.max_session_cost_usd: 25` (decision 14). The per-turn cap is $10,
  as in the committed recipe. core-agent has no per-day cap setting, so the
  $50 daily cap is enforced outside this file
  ([#1275](https://github.com/go-steer/core-agent/issues/1275)).
- `alerts.targets`: `maintainer-chat`, a `switchboard` target.
  **`conversation` is a placeholder that fails closed:**
  `REPLACE WITH THE SOAK SLACK CHANNEL ID`. `Validate` rejects whitespace in
  a conversation key, so a deployment that doesn't substitute the soak
  channel's Slack ID fails at startup. Without this, it would boot and send
  every notification to a conversation that doesn't exist.
  `config_test.go` asserts that the shipped file carries exactly this token
  and is refused. It then substitutes a fake ID (`C0123`) before checking
  everything else. `tools.disable: ["alert"]` keeps that target from also giving
  the model an `alert` tool. `approval_notify` sends through the alert
  sender directly, so notifications still go out. `dev/uat/self-dev/run.sh`
  does the same thing for T2.
- `attach.multi_session`: enabled, a `bearer_table` at
  `/etc/core-agent-users/users.json`, no anonymous callers, and
  `mastersingh24` as the one admin identity. The dispatcher creates its
  sessions as `sa:selfdev-dispatcher`, the identity `task_from` lists. Admin
  lets the maintainer attach to those sessions to answer an escalation. It
  sits at the end of the file, below the Slack placeholder, because
  `deploy/` substitutes that placeholder by position (see
  [Deploying the A7 rig](#deploying-the-a7-rig)).

`config_test.go` loads the overlay the way `-c` does (defaults, the file,
then `Validate`). It fails if any leaf of the committed recipe is missing
from the overlay, or differs without an entry in its `soakOverrides` list.
Changing the committed recipe therefore means updating the overlay too.
`plan_mode: "required"` stays, because the dispatcher builds the PR body
from the agent's plan artifact.

## Deploying the A7 rig

`deploy/` is phase 2's minimal rig (design, "Phases"): namespace
`core-agent-selfdev` on the drill cluster, with the daemon, the dispatcher,
their volumes, and the network policy between them.

| Path | What it is |
|---|---|
| `deploy/base/` | The rig. Every object is forced into `core-agent-selfdev`. |
| `deploy/fqdn-egress/` | The base with host-named egress (see [Egress](#egress)). |
| `deploy/a7-job/` | The A7 run alone: the dispatcher's `--once` Job, applied as its own step. |
| `deploy/inputs.env.example` | The operator's values, all placeholders. |
| `deploy/render.sh` | Renders one of the three and refuses to print it while anything is a placeholder. |

### What it deploys

- **The daemon** (`Deployment/core-agent-selfdev`): one replica, `Recreate`,
  the soak image by digest, `--no-repl --attach-listen=:7777`, multi-session
  against the hashed table. It mounts the config overlay from a ConfigMap
  built from `config.soak.json`, the users table read-only from its
  Secret, and PVCs for the eventlog (10Gi), the workspace (20Gi) and the Go
  cache (30Gi). Its KSA, `core-agent-selfdev-daemon`, gets
  `roles/aiplatform.user` through Workload Identity and nothing else. An
  init container creates `/workspace/.soak/plans` as uid 10001 before the
  kubelet mounts it into the recipe; a subPath the kubelet creates itself
  belongs to root, and plan-first would then refuse every edit. It runs
  with `--no-pricing-refresh`, so the cost ceilings use the pinned
  binary's built-in catalog and a restart doesn't wait on a fetch that
  `fqdn-egress/` blocks.
- **The A7 run** (`Job/selfdev-soak-dispatcher-a7`, in `deploy/a7-job/`):
  the dispatcher with `--once`, `backoffLimit: 0`, `restartPolicy: Never`.
  Its exit status is the verdict (0 for a PR, 1 for a stop or a signal), and
  a retry would hide a stop (decision 17). It is rendered and applied on
  its own, last, so re-applying the base never starts a run.
- **The polling dispatcher** (`Deployment/selfdev-soak-dispatcher`):
  the same pod spec without `--once`, at `replicas: 0`. It is for the
  2-week soak. Never scale it up while the A7 Job exists: both mount the
  one state file. Scaling it to 0 is the design's hard stop. Re-applying
  the base resets it to 0.
- **The dispatcher's pod** (either form): the same image, uid 10001, and
  the workspace at `/workspace`, like the daemon. The writer App's key and
  its attach token are Secret files mounted only here (`defaultMode: 0400`,
  which `fsGroup` turns into `0440`; both readers accept that). It has a
  state-file PVC and an `emptyDir` for its private bare repository, which no
  other pod can mount. Its KSA, `selfdev-soak-dispatcher`, holds no role
  anywhere. The workspace PVC is ReadWriteOnce, so the dispatcher's pod
  requires the daemon's node (`podAffinity`). The daemon in turn prefers
  the dispatcher's node, but a preference can't guarantee it: **stop the
  dispatcher before rolling the daemon** (a new image, a config change), or
  a daemon rescheduled to another node waits on a volume it can't attach.
- **Both pods**: `runAsNonRoot`, uid/gid 10001, `fsGroup: 10001`,
  `seccompProfile: RuntimeDefault`, `allowPrivilegeEscalation: false`,
  `drop: [ALL]`, `readOnlyRootFilesystem: true` with `emptyDir` at `/tmp`
  and `/home/soak`, and no service-account token. The namespace enforces
  the `restricted` Pod Security level.

### Inputs fail closed

The base reads two files that aren't in the tree: `inputs.env` and
`config.soak.json` (`a7-job/` reads `inputs.env` too, for the image).
`render.sh` copies `deploy/` to a temporary directory,
adds them, renders, and prints the result only if every input is filled
in, `SOAK_IMAGE` is `<repository>@sha256:<64 hex>`, the output contains no
`REPLACE`, and every image in it is a digest. Anything else exits non-zero
with nothing on stdout, so a pipe into `kubectl` applies nothing.
`kubectl apply -k deploy/base` fails outright, because neither file is
there.

Each placeholder also fails on its own if applied anyway: the image
placeholder isn't a valid image reference (the kubelet reports
`InvalidImageName`), the Slack channel placeholder contains spaces (config
validation stops the daemon), the App ID isn't a number, and the commit
email has no `@` (the dispatcher exits 2).

The Slack channel reaches the config through a kustomize replacement
that splits `config.json` on `"` and replaces one segment. A replacement
can't address a field inside a JSON string. Editing `config.soak.json`
above the placeholder moves the segment; `render_test.go` then fails and
prints the new `index:` for `deploy/base/kustomization.yaml`.

The inputs ConfigMap keeps a fixed name (no hash suffix), so the
separately rendered Job can name it and an input change doesn't alter the
Job's immutable pod template. A changed `SOAK_VERTEX_PROJECT`,
`SOAK_APP_ID`, `SOAK_COMMIT_NAME` or `SOAK_COMMIT_EMAIL` therefore reaches
a running pod only when it restarts
(`kubectl -n core-agent-selfdev rollout restart deployment/core-agent-selfdev`,
or delete and re-apply the Job). A new image or Slack channel rolls the
daemon by itself. Render the rig and the Job from the same inputs file:
the Job takes its image from its own render, but its other values from
the ConfigMap the rig applied.

### Egress

A standard NetworkPolicy can't name a host. `deploy/base` is as close as
the standard API gets:

- default deny, both directions, for the namespace;
- daemon ingress only from the dispatcher's pods, on 7777;
- daemon egress to DNS, the GKE metadata server (Workload Identity), and
  TCP 443 on public addresses;
- dispatcher: no ingress; egress to DNS, the daemon's pods on 7777, and
  TCP 443 on public addresses.

DNS is port 53 to kube-dns, to NodeLocal DNSCache (`169.254.20.10`), and to
the metadata server (`169.254.169.254`), which answers DNS on clusters that
use Cloud DNS. Only the daemon reaches the metadata server's token ports.

"Public" excludes the cluster, the VPC and link-local ranges, but not the
rest of the internet. **Use `deploy/fqdn-egress/` wherever the cluster
supports it.** It deletes the two "443 to public" policies and adds GKE
FQDNNetworkPolicies: the daemon may reach `aiplatform.googleapis.com` (and
regional `*-aiplatform.googleapis.com`), `proxy.golang.org`,
`sum.golang.org` and `vuln.go.dev`; the dispatcher may reach
`api.github.com` and `github.com`. It needs GKE Dataplane V2 with FQDN
network policy enabled
(`gcloud container clusters update std-simian-test --enable-fqdn-network-policy`).
Without it, `kubectl apply` refuses the FQDNNetworkPolicy kind, so the
variant fails closed rather than deploying unfenced. A host the agent
needs and the list lacks shows up as a dial timeout in its tool output.

switchboard (P4) isn't deployed yet. When it is, add an egress rule for
its pods to `deploy/base/21-networkpolicy-daemon.yaml` (there is a
commented example). Until then the daemon's approval notifications fail,
and the daemon logs each failure.

The maintainer reaches the daemon over `kubectl port-forward`, which goes
through the kubelet rather than the pod network. No ingress exists for it.
Attaching during an A7 run voids the run's "no human" assertion.

One thing the agent can still break: it owns `/workspace/.soak/plans`.
Replace that directory with a symlink out of the volume, and the kubelet
refuses the subPath mount, so the daemon's next restart fails with
`CreateContainerConfigError`. That stops the rig rather than widening
anything. To recover, replace the symlink with a real directory from any
pod that mounts the workspace.

### Secrets

None is committed, generated, or rendered. Create them by hand in
`core-agent-selfdev`. Keep the plaintext under
`~/.gke-drill/selfdev-soak/secrets/` (mode 0700), outside every
repository.

| Secret | Key | Mounted in | What it holds |
|---|---|---|---|
| `core-agent-selfdev-users` | `users.json` | daemon, read-only, `/etc/core-agent-users/` | The bearer table, **hashed rows only**: `sa:selfdev-dispatcher` and `mastersingh24`. |
| `selfdev-soak-dispatcher-attach` | `token` | dispatcher, `/var/run/selfdev-soak/attach/` | The plaintext of the `sa:selfdev-dispatcher` row. |
| `selfdev-soak-writer-app` | `private-key.pem` | dispatcher, `/var/run/selfdev-soak/app/` | The writer GitHub App's private key. |
| `core-agent-selfdev-switchboard` | `url`, `token` | daemon env | The `maintainer-chat` target. Until switchboard is deployed, any URL and a random token: the daemon won't boot without them. |

### IAM

One binding: the daemon's KSA gets `roles/aiplatform.user` on the Vertex
project, through Workload Identity Federation for GKE (no Google service
account, no KSA annotation). Grant nothing to `selfdev-soak-dispatcher`.

```bash
CLUSTER_PROJECT=gke-demos-345619                # owns std-simian-test and its identity pool
VERTEX_PROJECT=<the SOAK_VERTEX_PROJECT value>
PROJECT_NUMBER="$(gcloud projects describe "${CLUSTER_PROJECT}" --format='value(projectNumber)')"
gcloud projects add-iam-policy-binding "${VERTEX_PROJECT}" \
    --role=roles/aiplatform.user --condition=None \
    --member="principal://iam.googleapis.com/projects/${PROJECT_NUMBER}/locations/global/workloadIdentityPools/${CLUSTER_PROJECT}.svc.id.goog/subject/ns/core-agent-selfdev/sa/core-agent-selfdev-daemon"
```

### Runbook: A7

All local scratch goes under `/tmp/selfdev-soak/`. The seed list, the
inputs and the secrets live under `~/.gke-drill/selfdev-soak/`.

**0. Once, in the mirror.** Create the writer App and install it on
`mastersingh24/core-agent-selfdev` only, as the dispatcher design's
"What the maintainer creates by hand" says. Create the labels:

```bash
for l in soak:queue soak:pause soak:active soak:stopped \
         soak:expect-mergeable soak:expect-should-escalate soak:expect-should-stop; do
  gh label create --repo mastersingh24/core-agent-selfdev "$l"
done
```

Name the soak's commit identity in the mirror's attribution allowlist
(decision 16), never upstream's.

**1. Build and push the image, and record its digest.** The ref is an
upstream release tag or a full SHA on upstream `main`, at or after
`61f00731` (the dispatcher):

```bash
dev/uat/selfdev-soak/build-image.sh --ref <tag or SHA> --platform linux/amd64 \
    --push --registry us-docker.pkg.dev/<project>/<repo>
```

It prints `<registry>/core-agent-selfdev-soak@sha256:...`. Log the ref, the
commit and the digest on #1213.

The dispatcher in the image is the ref's, but its flags in `deploy/` are
this tree's, and `render_test.go` checks them against this tree's
`dispatcher/main.go` only. Build from a ref whose dispatcher takes the
same flags (this commit or later, until they change). A mismatch exits 2
at startup, and the A7 Job fails without claiming anything.

**2. Fill in the inputs.**

```bash
mkdir -p ~/.gke-drill/selfdev-soak && chmod 700 ~/.gke-drill/selfdev-soak
cp dev/uat/selfdev-soak/deploy/inputs.env.example ~/.gke-drill/selfdev-soak/inputs.env
$EDITOR ~/.gke-drill/selfdev-soak/inputs.env    # every REPLACE value
```

**3. Create the namespace and the Secrets.** Tokens are 32 random bytes,
and only their SHA-256 goes into the table (`core-agent auth hash-token`
computes the same digest):

```bash
kubectl create namespace core-agent-selfdev
S=~/.gke-drill/selfdev-soak/secrets
mkdir -p "$S" && chmod 700 "$S"
( umask 077
  openssl rand -hex 32 > "$S/dispatcher.token"
  openssl rand -hex 32 > "$S/maintainer.token"
  D="$(tr -d '\n' < "$S/dispatcher.token" | sha256sum | cut -d' ' -f1)"
  M="$(tr -d '\n' < "$S/maintainer.token" | sha256sum | cut -d' ' -f1)"
  printf '{"version":1,"users":[{"identity":"sa:selfdev-dispatcher","token_sha256":"%s"},{"identity":"mastersingh24","token_sha256":"%s"}]}\n' \
      "$D" "$M" > "$S/users.json"
  openssl rand -hex 32 > "$S/switchboard.token" )
NS=(-n core-agent-selfdev)
kubectl "${NS[@]}" create secret generic core-agent-selfdev-users --from-file=users.json="$S/users.json"
kubectl "${NS[@]}" create secret generic selfdev-soak-dispatcher-attach --from-file=token="$S/dispatcher.token"
kubectl "${NS[@]}" create secret generic selfdev-soak-writer-app --from-file=private-key.pem=<the App's downloaded .pem>
kubectl "${NS[@]}" create secret generic core-agent-selfdev-switchboard \
    --from-literal=url=<switchboard URL, or https://switchboard.invalid/ until P4> \
    --from-file=token="$S/switchboard.token"
```

**4. Grant Vertex to the daemon** ([IAM](#iam)).

**5. Render and apply the rig.** Add `--fqdn-egress` where the cluster
supports it ([Egress](#egress)):

```bash
mkdir -p /tmp/selfdev-soak
dev/uat/selfdev-soak/deploy/render.sh --inputs ~/.gke-drill/selfdev-soak/inputs.env \
    --fqdn-egress > /tmp/selfdev-soak/rendered.yaml
kubectl apply -f /tmp/selfdev-soak/rendered.yaml
kubectl -n core-agent-selfdev rollout status deployment/core-agent-selfdev
kubectl -n core-agent-selfdev logs deployment/core-agent-selfdev | grep -E 'multi-session|watchdog|cost ceiling|plan mode'
```

The boot lines must show `multi-session auth: bearer_table`, `watchdog:
enforce mode`, `cost ceiling: per-turn=$10.0000 per-session=$25.0000`, and
no warning about plaintext rows. One warning is expected: `credential file
/etc/core-agent-users/users.json is readable by the user this daemon runs
as`. It fires for a fully hashed table too (hashed-bearer-tokens design,
"Out of scope"); with hashed rows, reading the file yields nothing to
authenticate with. Nothing claims issues yet: the dispatcher Deployment is
at 0 and the A7 Job is step 7.

**6. Seed A7.** First check that no other `soak:queue` issue is open:
`--once` takes the oldest claimable one.

```bash
gh auth status                                   # logged in as mastersingh24
gh issue list --repo mastersingh24/core-agent-selfdev --label soak:queue --state open
dev/uat/selfdev-soak/seed.sh --only A7 --dry-run # read the title, body and upstream link
dev/uat/selfdev-soak/seed.sh --only A7
```

**7. Run the dispatcher once.** Applying the A7 Job starts it. It claims
the issue on its first poll, then runs to a PR or a stop. Watch it without
attaching to the daemon:

```bash
dev/uat/selfdev-soak/deploy/render.sh --inputs ~/.gke-drill/selfdev-soak/inputs.env \
    --a7-job > /tmp/selfdev-soak/a7-job.yaml
kubectl apply -f /tmp/selfdev-soak/a7-job.yaml
kubectl -n core-agent-selfdev logs -f job/selfdev-soak-dispatcher-a7
kubectl -n core-agent-selfdev get job selfdev-soak-dispatcher-a7 --watch   # until Complete or Failed
```

`Complete` means the dispatcher opened the PR (exit 0). `Failed` means it
exited non-zero: usually a stop, and then the issue carries `soak:stopped`
and the reason. It can also be a startup failure (the log's first lines
say why, and the issue is untouched), or the Job's 6-hour deadline, in
which case read the issue's labels before doing anything else. The Job
never retries (decision 17). To run again, delete the Job, remove
`soak:stopped` from the issue as the maintainer, delete any leftover
`agent/issue-N` branch, and apply the Job again.

**8. Grade.** Run the A7 grader, `dev/uat/selfdev-soak/grade_a7.*` (built
separately), against the run, then merge the PR yourself if it passes
(decision 10: a person merges A7's PR).

**Stopping.** Label any open mirror issue `soak:pause` to hold the
dispatcher before its next claim or push. `kubectl -n core-agent-selfdev
delete job selfdev-soak-dispatcher-a7` stops it outright. Deleting the
rendered rig also deletes the PVCs, and with them the eventlog: copy
`/var/lib/core-agent/sessions.db` out first.

## Seeding

`seed.sh` creates seed issues in the mirror from the private seed list,
under your own `gh` login, which must be the maintainer's (decision 6):

```bash
dev/uat/selfdev-soak/seed.sh --only A7 --dry-run   # print, create nothing, call no gh
dev/uat/selfdev-soak/seed.sh --only A7             # seed 0 only
dev/uat/selfdev-soak/seed.sh --only M1,M2          # or --all
```

- The list is read from `~/.gke-drill/selfdev-soak/seeds.md` (`--seeds`
  overrides). It is the soak's answer key, so the script refuses a file
  inside any git work tree, symlinks resolved, and a file with more than
  one hard link (decision 20). A work tree it can't see stays possible: a
  bare repository whose work tree is set only by a shell alias. Keep the
  list out of any directory a dotfiles repository manages.
- Seeds are the rows of the tables under a `## ` heading naming a class
  (`mergeable`, `should-escalate`, `should-stop`) or "Seed 0" (A7, which is
  mergeable). Any other heading, a `### ` one included, ends the section.
- Each issue's body starts with `Upstream issue: <url>` (decision 12),
  followed by the seed's full text. The upstream issue is the one the seed's
  evidence column starts with, else the one its text starts with, else
  `#1213`, with a warning on stderr. `--dry-run` shows which.
- The issue is labeled `soak:queue` and `soak:expect-<class>`, and assigned
  to the maintainer. **The class is never written into the title or the
  body.** Neither is anything from the seed list's other columns (file:line
  evidence, why a person must decide, what is missing), nor a bold
  "**Grade ...**" note in the text, which is dropped together with
  everything after it in the cell. A seed whose text would still name a class or say how it is
  graded is refused; reword it in the list.
- A seed whose title already exists in the mirror, or was filed earlier in
  the same run, is skipped, so a re-run doesn't duplicate issues.
