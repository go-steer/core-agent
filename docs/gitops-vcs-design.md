# GitOps for the distroless image: a forge-neutral `vcs` tool suite

> **Status — shipped (P1+P2), 2026-11-06.** Supersedes the W3 shape in
> [`hermes-replacement-design.md`](hermes-replacement-design.md) §5 and the
> scope of [#592](https://github.com/go-steer/core-agent/issues/592), which
> were GitHub-only and MCP-first. Tracking:
> [#1490](https://github.com/go-steer/core-agent/issues/1490) (this repo) and
> [core-vcs#1](https://github.com/go-steer/core-vcs/issues/1) (`go-steer/core-vcs@v1.1.0`).

## Why

Every deployment story we have for the distroless image ends at the same
cliff. The kube-platform-agent governance overlay says the write path is "a
GitOps **pull request**, not an in-cluster mutation"
(`examples/kube-platform-agent/AGENTS.d/50-governance.md`). The gated-apply
recipe tells the agent that a manifest in a GitOps repo is a change it
"cannot apply" and must hand to a human. k8s-lookout already drafts
GitOps-PR payloads. kube-agents' field evidence is unambiguous: operators do
not want an agent pushing to production, they want it to open a proposal a
human merges. And the agent cannot do that today, because the image is
`distroless/static` — no `git`, no `gh`, no `glab`, no shell — and the only
PR-opening path in the tree is `bash` + `gh` in the self-development recipe,
which exists precisely because that recipe does not run in the image.

The forge is not one thing. The installs asking are on github.com, GitHub
Enterprise Server, gitlab.com, self-managed GitLab behind a private CA, and
plain git remotes with no forge at all. An agent that speaks only GitHub is
one most of them cannot evaluate. kube-agents hit this first and wrote the
design of record for it
(`kube-agents/docs/designs/version-control-support.md`). We take its
thinking — the `proposal` noun, the publish guards, the error taxonomy, the
forges/repositories config split — rather than deriving it a second time.
We do not take its protocol: that design's shape (a credential-free sandbox,
a broker pod, history moved as bundles) follows from a threat model and a
process topology this image does not have.

## Settled decisions (do not relitigate)

1. **Pure Go, in the binary.** A go-git working copy plus native forge API
   providers, behind kube-agents' neutral verbs. No child process, no shell,
   no `git` binary. This is the only shape that runs in
   `distroless/static-debian12:nonroot` without changing the image, and the
   only one a no-model path (a cron sweep, a guardrail, a test) can import.
   **MCP is an optional surface on top, never the mechanism**
   (kube-agents §9 has the argument; both halves of it apply here).

2. **kube-agents' vocabulary, guards and error taxonomy as the starting
   point; no wire compatibility.** A change offered for review is a
   `proposal`; `pull request` and `merge request` are aliases in prose,
   never in a tool name or a field. The publish guards (`TARGET_IS_BRANCH`,
   `BRANCH_NOT_OURS`, `BASE_MOVED` in the base-under-target direction,
   `PROTECTED_BRANCH`) and the `FORGE_*` code family are adopted because
   they are right, and are ours to extend or rename. The Go API is designed
   for Go consumers (typed structs, `context`, errors), not shaped to
   kube-agents' `/v1/vcs/*` JSON. The abstraction already admits a broker
   as one more `Provider` if a deployment ever needs one; we do not commit
   to anyone's protocol in advance of that.

3. **First slice: GitHub, GitLab, bare git.** One `github` provider covers
   github.com and GHES (host-configurable, API base `/api/v3` on GHES). One
   `gitlab` provider covers gitlab.com and self-managed (API base `/api/v4`,
   arbitrary-depth group paths, private CA via `ca_file`). One `git`
   provider covers any remote go-git can reach: it supports `clone`,
   `commit`, `publish` and reports `proposal` as `missing` from
   `capabilities`, so the model is told up front that the branch is the
   deliverable. Bitbucket, Gitea/Forgejo, Azure Repos: later, each as a new
   package and one registry line, proven by the parameterised contract test.

4. **v1 identity is a per-forge bot credential from config.** `token_file`
   or `token_env`, nothing else. Per-caller identity (W0,
   [#106](https://github.com/go-steer/core-agent/issues/106),
   [#204](https://github.com/go-steer/core-agent/issues/204)) is a named
   follow-up that slots in behind the same `Credential` seam. GitHub App
   installation-token minting is a credential *source* for the `github`
   provider, also later.

5. **The working copy is a real directory under the agent's path scope,
   edited with the existing file tools.** `vcs_clone` materialises it;
   `read_file`, `edit_file`, `write_file`, `grep`, `glob` work on it
   unchanged; `vcs_status` / `vcs_diff` / `vcs_log` read it; `vcs_commit`,
   `vcs_publish` and `vcs_propose` are the **only new write surfaces**. No
   in-memory-only mode, no parallel `vcs_write_file`. The model already
   knows how to edit files; we are not teaching it a second way.

6. **The agent never writes the target branch.** `publish` refuses a branch
   equal to the repository's base branch (`TARGET_IS_BRANCH`) and a branch
   it did not create in this process (`BRANCH_NOT_OURS`) before anything
   leaves the process. There is no flag that relaxes either. An operator
   who wants direct pushes has `bash` on a non-distroless image; that is not
   this feature.

7. **The forge token is unreachable from the model.** It is read once by
   the daemon, held in the provider, withheld from every child process via
   `pkg/childenv`, and the file that holds it is added to the permission
   gate's refuse list (the same mechanism as `config.CredentialFiles`, under
   a sibling accessor since the existing doc comment scopes that list to
   caller-to-daemon credentials). A token the model could `cat` is a token
   the model could use to push to `main` with `fetch_url`, and decision 6
   would be prose.

8. **No shallow clones by default; single-branch always.** go-git supports
   `Depth`, but a shallow copy makes `log`/`annotate` lie and breaks the
   ancestry checks `publish` depends on. `branch` is the size control.
   `max_clone_bytes` (default 256 MiB of working tree) is the ceiling, and
   its refusal says so.

9. **Thin hand-written forge clients over `net/http`.** The verb set is
   eleven routes; `google/go-github` and `xanzy/go-gitlab` are each larger
   than the feature. go-git is the one new direct dependency. If a client
   grows past what a provider needs, that is the moment to reconsider, not
   before.

10. **The library is its own module, `github.com/go-steer/core-vcs`.**
    Same pattern as `core-models` and `core-tui`: a pluggable-backend layer
    behind a narrow interface, released on the backends' cadence, importable
    by kube-agents' Go side, k8s-lookout, cogo and scion without dragging
    in the agent runtime, and keeping go-git's transitive graph out of
    `examples/embed/slim` (`verify-slim-deps`). The provider boundary test
    becomes a module boundary. core-agent keeps only what needs the gate:
    `pkg/tools/vcs`, `config.VCSConfig` and the skill. **New providers and
    provider fixes go to core-vcs, not here** — the same rule `AGENTS.md`
    states for core-models.

11. **core-vcs is `v0.x` until a second provider ships.** The `Provider` /
    `Workspace` API churns through P1 and P2; apidiff and an API freeze
    arrive with P2's `gitlab` provider. core-agent pins it the way it pins
    core-tui: a `var _ vcs.Provider = (*T)(nil)`-style guard per
    implemented interface, and a `verify-core-vcs-pin` presubmit once the
    pin exists.

12. **`vcs.workspace_dir` defaults to `<agent cwd>/.vcs`.** Inside the
    default path scope with no configuration. Pod manifests and the Helm
    chart set `/workspace/repos` explicitly.

13. **`vcs_clone` is read-class.** No prompt under `ask`, no plan required
    under plan-first. It is path-scope-checked and size-capped; it is the
    step before every read and prompting on it is noise.

14. **No sparse or shallow checkout in the first slice.** `max_clone_bytes`
    is the backstop. A per-repository `sparse_paths` is added when the
    first real monorepo arrives, not before.

15. **A forge-less remote can still yield a proposal link.** A
    `provider: git` forge may set `proposal_url_template`
    (`https://{host}/{path}/compare/{target}...{source}`). When set,
    `vcs_propose` returns that URL with `proposalNoun: link`; when unset,
    `capabilities` reports `proposal` as `missing` and the branch is the
    deliverable.

## Out of scope

- **Merging.** No `proposal-merge` verb exists and none is planned. The
  human merges. (kube-agents has the same absence, on purpose.)
- **Downstream CD.** No Flux/Argo awareness, no "wait for reconcile".
  `wait_and_verify` over a read-only cluster tool covers the cases that
  matter, and the reconciler is the operator's.
- **Per-caller identity** (decision 4). The seam is left; the resolution is
  W0.
- **A broker transport / kube-agents wire compatibility** (decision 2).
  If a deployment needs core-agent to reach a forge through a credential
  broker, that is one more `Provider`, designed then against that broker.
- **Issue tracking verbs** (`issue-*`). kube-agents ships them; we do not
  need them for GitOps and they double the provider surface. Deferred, with
  the verb names reserved so a later addition is additive.
- **Bitbucket, Gitea/Forgejo, Azure Repos, Gerrit.** Decision 3. Gerrit is
  not a "forge of git" in the sense the design relies on (a change is a
  ref, not a branch) and would need its own thought.
- **SSH transport.** HTTPS with a token is what every target forge offers
  for bots and what the credential rules can reason about. SSH keys,
  known_hosts and agent forwarding in a distroless pod are a different
  design.
- **Rebasing or resolving conflicts on behalf of the model.** A `BASE_MOVED`
  refusal tells the model to `vcs_clone` again; a diverged proposal is
  rebased on the forge by a human.
- **Signing commits.** The commit author is the bot identity; signature
  verification is a forge policy the operator sets.
- **Sparse and shallow checkouts** (decision 14). `max_clone_bytes` and
  single-branch cloning are the size controls until a monorepo proves
  otherwise.
- **Changing the image.** No `-git` variant, no debug image.

## The shape

```
                 ┌────────────────────────── agent process ──────────────────────────┐
                 │  core-agent                                                       │
  model ──tool──▶│ pkg/tools/vcs ──▶ ┌── core-vcs (separate module) ───────────────┐ │
                 │                   │ vcs: verbs, guards, RepoRef, Workspace      │ │
                 │                   │         │                 │                 │ │
                 │                   │  go-git worktree     Provider (forge API)   │ │
                 │                   │  <workspace>/<repo>   github | gitlab | git │ │
                 │                   └─────────┼─────────────────┼─────────────────┘ │
                 └─────────────────────────────┼─────────────────┼───────────────────┘
                                               ▼                 ▼
                                    git smart HTTP (token)   REST (token)
```

Three layers, bottom up. The first two live in
`github.com/go-steer/core-vcs`; the third in core-agent.

### `core-vcs` `vcs` — the library

Importable without a model. Everything `pkg/tools/vcs` does is a thin call
into here, which is what lets a cron sweep, a guardrail or a test drive the
same code.

```go
// RepoRef is the one parser (kube-agents "Repository identity").
// Host is read only where the syntax states one, or where the first
// segment of a schemeless value is a configured forge host.
type RepoRef struct{ Host, Path string }   // Path is opaque, arbitrary depth

type Forge struct {                         // one entry in config.vcs.forges
    Name, Provider, Host, Scheme string
    Port     int
    CAFile   string
    Cred     Credential
}

type Credential interface {                 // the W0 seam
    Token(ctx context.Context) (string, error)
}

type Provider interface {
    Name() string                                   // "github" | "gitlab" | "git"
    Resolve(f Forge, ref RepoRef) (Repo, error)     // host check + grammar
    Capabilities(ctx, Repo) (Capabilities, error)
    CloneURL(Repo) string
    BaseBranch(ctx, Repo) (string, error)
    ProposalCreate(ctx, Repo, ProposalRequest) (Proposal, error)
    ProposalList / ProposalView / ProposalComment / ProposalUpdate /
    ProposalClose / ProposalCommits / ProposalAcknowledge
}
```

`Registry` maps provider name → constructor and is the one shared file a
new forge edits. Two tests hold the boundary: every registered provider
passes the parameterised contract test against a recorded fixture, and no
file outside `providers/<name>/` may contain that provider's host
strings or API paths (a `git grep` test over the core-vcs tree, modelled on
core-agent's `TestEveryToolRegisteringPackageHasASweep`).

The `Workspace` type owns the go-git side: `Clone`, `Status`, `Diff`,
`Log`, `Commit`, `Publish`. It records, in memory, the revision each
branch was cloned at and every branch it created, which is how
`BRANCH_NOT_OURS` and the publish base are answered in-process, without
state on disk that the model could edit.

### `core-vcs` `providers/{github,gitlab,git}`

| | `github` | `gitlab` | `git` |
|---|---|---|---|
| Hosts | `github.com`, `www.github.com`, or the forge's `host` (GHES) | `gitlab.com` or the forge's `host` | any |
| Identity grammar | exactly two segments | ≥ two segments (groups/subgroups) | opaque |
| API base | `https://api.github.com` / `https://HOST/api/v3` | `https://HOST/api/v4` | — |
| Auth | `Authorization: Bearer` | `PRIVATE-TOKEN` | basic over smart HTTP only |
| Clone auth | basic, user `x-access-token` | basic, user `oauth2` | basic, user from `forge.username` |
| `proposalNoun` | `pull request` | `merge request` | `missing` |
| Protected-branch read | `GET /repos/{r}/branches/{b}` → `PROTECTED_BRANCH` | `GET /projects/{id}/protected_branches` | not knowable; the push error maps to `FORGE_REJECTED` |
| TLS | system roots | system roots + `ca_file` | system roots + `ca_file` |

Error translation is per provider and lands in the shared code set. A
`FORGE_CALL_FAILED` carries the forge's own message verbatim in `detail`;
the model sees the code and the detail, never the HTTP status alone.

### core-agent `pkg/tools/vcs` — the model-facing surface

Ten tools, each one verb, registered as a toolset and gated like every
other built-in. Names follow the `<domain>_<verb>` convention of the
existing suite. Descriptions and `jsonschema:` tags are prompt text and go
through `internal/testutil.ModelFacingBans`.

| Tool | Verb | Gate class | Notes |
|---|---|---|---|
| `vcs_capabilities` | `capabilities` | read | the first call a skill makes; tells the model `proposalNoun`, `baseBranch`, what is `missing` |
| `vcs_clone` | `clone` | read (decision 13) | materialises `<workspace>/<path>`; refuses outside path scope |
| `vcs_status` / `vcs_diff` / `vcs_log` | local | read | go-git, no network |
| `vcs_commit` | local | write | author = forge's `commit_identity`; refuses an empty tree and a message under 10 chars |
| `vcs_publish` | `publish` | write | all guards of decision 6; network |
| `vcs_propose` | `proposal-create` | write | network; returns `{proposal: {number, url, ...}}` |
| `vcs_proposal_view` / `vcs_proposal_list` | read | | the review-loop half: a later turn reads review comments |
| `vcs_proposal_comment` | write | | answering a review |

`proposal-update`, `-close`, `-commits`, `-acknowledge` ship in the
library and the contract test in the first slice; their tools follow once
a recipe needs them (kube-agents' "Replacing `gh`" shows which skills do).

`vcs_publish` and `vcs_propose` are `plan-first` aware: when
`require_plan_artifact` is on, they are refused until a plan is recorded,
like every other mutating tool. Under `permissions.mode: ask` each prompts;
under `auto` the model-backed approver sees the diff summary the tool
attaches to its request.

### Configuration

```jsonc
{
  "vcs": {
    "forges": [
      { "name": "gh",        "provider": "github",
        "token_file": "/var/run/secrets/vcs/github-token",
        "commit_identity": { "name": "platform-agent[bot]", "email": "platform-agent@acme.example" } },
      { "name": "ghes",      "provider": "github", "host": "git.corp.example",
        "token_env": "GHES_TOKEN" },
      { "name": "gl-self",   "provider": "gitlab", "host": "gitlab.internal", "ca_file": "/etc/ssl/corp/ca.pem",
        "token_file": "/var/run/secrets/vcs/gitlab-token" },
      { "name": "mirror",    "provider": "git",    "host": "git.internal", "username": "agent",
        "token_file": "/var/run/secrets/vcs/mirror-token",
        "proposal_url_template": "https://git.internal/{path}/compare/{target}...{source}" }
    ],
    "repositories": [
      { "forge": "gh",      "repository": "acme/infra",                 "role": "gitops",  "base_branch": "main" },
      { "forge": "gl-self", "repository": "platform/clusters/prod",     "role": "managed" },
      { "forge": "gh",      "repository": "acme/platform-terraform",    "role": "context" }
    ],
    "workspace_dir": "/workspace/repos",     // default: <agent cwd>/.vcs
    "max_clone_bytes": 268435456,
    "branch_prefix": "agent/"                // every branch the agent creates starts with this
  }
}
```

Mirrors kube-agents' `spec.integration.{forges,repositories}` one-for-one
so the CRD → `.agents/config.json` projection is mechanical. `role` is
`gitops` (at most one), `managed` (read-write) or `context` (read-only:
`clone` works, `commit`/`publish`/`propose` return `ROLE_VCS_VERBS`).
`base_branch` is refused on a `context` entry. Exactly one of `token_file`
/ `token_env` per forge, except `provider: git` where both may be absent
for an anonymous read-only remote.

Validation runs in `config.Validate`: unknown provider, duplicate forge
name, repository naming an unknown forge, two `gitops` roles,
`ca_file` on `provider: github` with no `host` (github.com does not take a
private CA), `branch_prefix` that is empty or contains `..`.

### The `propose-change` skill

A `builtin:` skill (or a recipe-side SKILL.md; the loader accepts either)
that scripts the happy path once so every recipe does not re-derive it:

1. `vcs_capabilities` on the `gitops` repository; stop with a clear
   message if `proposal` is `missing` and the recipe requires a proposal.
2. `vcs_clone` at `baseBranch`.
3. Edit with the file tools. `vcs_diff` to confirm the change is the one
   diagnosed and nothing else.
4. `vcs_commit` with a message that names the finding.
5. `vcs_publish` to `agent/<slug>-<shortid>`.
6. `vcs_propose` with the diagnosis as the body, the evidence the recipe
   collected, and the rollback step.
7. Report the proposal URL via `alert` or `return_result`.

It names what the body must *contain*, per the tool-description rules.

## Guards, and where each one lives

| Guard | Code | Where | Why there |
|---|---|---|---|
| Branch equals base/target | `TARGET_IS_BRANCH` | `Workspace.Publish`, before any network | the default clone→edit→commit→publish sequence must not be able to write the shared line |
| Branch not created by this process | `BRANCH_NOT_OURS` | `Workspace.Publish` | an existing remote branch of the same name is never clobbered |
| Branch lacks `branch_prefix` | `BRANCH_NOT_OURS` | `Workspace.Publish` | the forge's branch-protection rules can be written against the prefix |
| Target rewritten since clone | `BASE_MOVED` | `Workspace.Publish`, first publish only, base-under-target direction | kube-agents §3 "The seam" has the argument for the direction; we keep it |
| Target protected against the bot | `PROTECTED_BRANCH` | provider, before push | a clear code instead of a push error |
| Role is `context` | `ROLE_VCS_VERBS` | core-vcs verb dispatch | read-only declared intent |
| Tree exceeds ceiling | `CLONE_TOO_LARGE` | `Workspace.Clone`, streaming count | the refusal names the ceiling |
| Token file named by a tool call | permission gate refusal | `permissions.Gate` via the credential-file list | decision 7 |
| Forge host not in config | `FORGE_UNSUPPORTED` | `RepoRef` → `Resolve` | no implicit github.com fallback |
| TLS failure on a self-managed host | `FORGE_TLS_UNTRUSTED` | provider transport | the message says `ca_file` |

A force-push is not a verb. There is no `advance` without the branch
already being ours and the push being a fast-forward of our own last tip.

## What goes in the eventlog

Every `vcs_*` call is a tool event like any other. `vcs_publish` and
`vcs_propose` additionally record a typed `vcs.published` /
`vcs.proposed` entry carrying forge, repository, branch, revision and (for
propose) the proposal number and URL — the fields an audit asks for and
`bash gh pr create` never gave us. The outbox/effects plugin from the mast
fold ([`durable-write-gate-design.md`](durable-write-gate-design.md)) sees
them as effects, so a crash between `publish` and `propose` resumes
correctly instead of opening a second proposal.

## Phases

Each phase is a core-vcs release followed by a core-agent PR that bumps the
pin. The two repos never need to land in lockstep: core-vcs ships first.

| Phase | core-vcs lands | core-agent lands | Proves |
|---|---|---|---|
| **P1** | `v0.1`: `vcs` (RepoRef, Forge, Credential, Workspace over go-git with every guard, registry, error codes, parameterised contract test), `providers/git` (+ `proposal_url_template`), `providers/github` (github.com + GHES) | pin core-vcs; `config.vcs.*` schema + validation; `vcs_capabilities` `vcs_clone` `vcs_status` `vcs_diff` `vcs_log` `vcs_commit` `vcs_publish` `vcs_propose`; token-file refusal in the gate; `childenv` withholding; `dev/smoke/NN-vcs-github.sh` against a local remote and an httptest forge | the model opens a PR from the slim image with no shell |
| **P2** | `v0.2`: `providers/gitlab` incl. `ca_file`; apidiff gate; API freeze → `v1.0.0` | `vcs_proposal_view/list/comment`; the `propose-change` skill; `docs/site` page; kube-platform-agent recipe switched from prose to the skill; `verify-core-vcs-pin` | the second forge costs a package and one registry line |
| **P3** | per-caller `Credential` source; GitHub App installation tokens | W0 wiring | identity |

The adversarial review gate applies to every PR in both repos.

## Testing

- **Contract test, parameterised over providers**, run against recorded
  `httptest` fixtures: every verb, every error code, GHES and self-managed
  hosts, group/subgroup paths. A new provider that does not pass it does
  not register.
- **Workspace tests against a real on-disk remote** (go-git can serve a
  bare repo over `httptest` via `git http-backend`-equivalent in Go, or
  simply file:// for the local half): clone, commit, publish, every guard
  in the table above, each as a test that fails when the guard is
  commented out (prefix-failure verification applies).
- **Gate tests**: a `vcs_*` call naming the token file is refused; under
  `require_plan_artifact` `vcs_publish` is refused until `record_plan`.
- **Description sweep** in `pkg/tools/vcs` (mandatory per
  `TestEveryToolRegisteringPackageHasASweep`).
- **Smoke** `dev/smoke/NN-vcs-*.sh`: mock model, local remote, asserts a
  proposal appears on the httptest forge and that the base branch's tip is
  unchanged. Pins config with `-c`.
- **Boundary test** (core-vcs): no file outside `providers/<p>/` names that
  provider's hosts or API paths.

## Resolved questions

The four questions the first draft left open were settled 2026-11-03 and
are decisions 12–15 above. The repository split (decisions 10–11) was
settled the same day, and the first draft's wire-compatibility clause in
decision 2 was withdrawn: the kube-agents protocol's shape (bundles, a
stateless broker) follows from a sandbox/broker topology this image does
not have, and compatibility bought nothing a `Provider` implementation
could not buy later.

## References

- kube-agents `docs/designs/version-control-support.md` — the design whose
  thinking this one borrows; §2 concepts, §3 seam (the guard arguments),
  §5 modularity, §7 GitLab, §9 MCP. Its protocol is not adopted.
- [`hermes-replacement-design.md`](hermes-replacement-design.md) §5 W3 —
  the superseded GitHub-MCP shape; #592.
- [`kube-agents-scenarios-design.md`](kube-agents-scenarios-design.md)
  §4 — "Open a PR" is the gap in every demo row.
- [`gated-apply-design.md`](gated-apply-design.md) — why direct mutation
  was scoped narrowly and GitOps named as the production path.
- [`credential-files-design.md`](credential-files-design.md),
  `pkg/childenv` — the token-containment mechanisms decision 7 reuses.
- [`plan-first-design.md`](plan-first-design.md),
  [`durable-write-gate-design.md`](durable-write-gate-design.md) — the
  gates `vcs_publish` / `vcs_propose` sit behind.
