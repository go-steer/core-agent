# Self-development soak: image and config overlay

This directory holds prerequisite **P3** of the self-development soak
([#1213](https://github.com/go-steer/core-agent/issues/1213),
[`docs/selfdev-soak-design.md`](../../../docs/selfdev-soak-design.md)). P3 is an image that can do
development work. Later phases add the dispatcher (`dispatcher/`) and the
namespace overlay here.

| File | What it is |
|---|---|
| `Dockerfile` | The soak image. |
| `build-image.sh` | Builds it from a pinned ref. It pushes only when given `--push` and `--registry`. |
| `config.soak.json` | The soak's config overlay: the committed recipe plus the auto-mode deltas. |
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
- **`GOPATH` on the Go cache volume** (`/cache/go/path`), with its `bin` on
  `PATH`. Dev tools that the presubmits `go install` (golangci-lint,
  govulncheck) then survive a pod restart.
- **A core-agent binary built from a pinned upstream ref** (next section).
  A tag build reports the tag from `--version`. A SHA build reports the
  tree's own version string and the SHA as its commit, because attach peers
  read the version as a semver. The ref and the resolved commit are also
  written to `/usr/local/share/core-agent-soak/`.
- **No `gh`, and no GitHub credential.** The agent stops at local commits.
  The dispatcher, in its own pod, pushes and opens the PR with the writer
  App's token (design decision 7).
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
because a rebuild overwrites it. After a push the script prints the
registry digest. Deploy by that digest, and record it with the ref.

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
| `/workspace` | PVC, read-write | The mirror's clone and the dispatcher's per-issue worktrees. It is also the working directory. |
| `/cache/go` | PVC, read-write | Go caches: `GOMODCACHE=/cache/go/mod`, `GOCACHE=/cache/go/build`, and `GOPATH=/cache/go/path`. Keeps `go test` and the installed dev tools warm across issues and restarts. |
| `/var/lib/core-agent` | PVC, read-write | The eventlog. Pass `--session-db-path=/var/lib/core-agent/sessions.db`. |
| `/etc/core-agent-soak/config.json` | ConfigMap, read-only | The config overlay, `config.soak.json`. |

Give the pod `runAsUser: 10001`, `runAsGroup: 10001`, `fsGroup: 10001` and
`runAsNonRoot: true`, so the PVCs are writable. **Run the dispatcher as
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
is read-only. Pass `--agents-dir` pointing at the clone's recipe, so that
`AGENTS.md`, the skills, the `reviewer` subagent's root and the plans
directory resolve against it:

```
args: ["--agents-dir", "/workspace/mirror/.agents",
       "--no-repl", "--attach-listen", ":8080",
       "--session-db-path", "/var/lib/core-agent/sessions.db"]
```

The workspace clone needs the upstream release tags.
`verify-version-fallback`, part of the presubmit sweep the recipe runs,
fails with "no release tags found" in a clone that has none. That gives the
agent a red check it can do nothing about. Either the mirror carries
upstream's tags, or the dispatcher fetches them into the clone.

The attach listener's auth, and the multi-session block the dispatcher's
`POST /sessions` needs, come with the namespace overlay. They wait on P2's
hashed bearer table.

| Env | Why |
|---|---|
| `ANTHROPIC_VERTEX_PROJECT_ID` | The Vertex project for `anthropic-vertex`. |
| `CLOUD_ML_REGION=global` | Claude 5 is served only from the global Vertex endpoint. |
| `SOAK_SWITCHBOARD_URL`, `SOAK_SWITCHBOARD_TOKEN` | The `maintainer-chat` alert target. The daemon refuses to boot without them, because `approval_notify` names that target. core-agent keeps both out of the agent's child processes. |
| `GIT_AUTHOR_NAME`, `GIT_AUTHOR_EMAIL`, `GIT_COMMITTER_NAME`, `GIT_COMMITTER_EMAIL` | The soak's dedicated commit identity (decision 16). `git commit -s` signs off with the committer identity, and `verify-no-agent-attribution`'s allowlist must name it. Not the maintainer's own, and not a `[bot]` address (decision 8). |

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
  $50 daily cap is enforced outside this file.
- `alerts.targets`: `maintainer-chat`, a `switchboard` target.
  **`conversation` is a placeholder (`SOAK-SLACK-CHANNEL-ID`), and it
  validates.** A deployment that doesn't replace it boots normally and
  posts every notification to a conversation that doesn't exist. The
  namespace overlay that generates the ConfigMap must substitute the soak
  channel's Slack ID, and fail if the placeholder is still there. `tools.disable: ["alert"]` keeps that target from also giving
  the model an `alert` tool. `approval_notify` sends through the alert
  sender directly, so notifications still go out. `dev/uat/self-dev/run.sh`
  does the same thing for T2.

`config_test.go` loads the overlay the way `-c` does (defaults, the file,
then `Validate`). It fails if any leaf of the committed recipe is missing
from the overlay, or differs without an entry in its `soakOverrides` list.
Changing the committed recipe therefore means updating the overlay too.
`plan_mode: "required"` stays, because the dispatcher builds the PR body
from the agent's plan artifact.
