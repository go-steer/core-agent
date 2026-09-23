---
name: presubmit-sweep
description: Run the local presubmit scripts before pushing to core-agent. Use when a change is ready to push, before `git push` or `gh pr create`, or when CI has gone red and you want to reproduce it locally. Covers which scripts to run, which are slow, and why a green local run is the same run as CI.
---

# Presubmit sweep

`dev/ci/presubmits/*` **are** the scripts CI runs. A green local sweep is the
same green run as remote CI, which is the whole reason to do it here: pushing
first and reading the failure off GitHub costs a full CI cycle to learn
something a local minute would have told you.

Run them before every push. Not before every commit — before every push.

## The sweep

**Run everything in `dev/ci/presubmits/` except the two `e2e-*` scripts.**
That is the whole rule, and it is simpler than the conditional table you
might expect because CI has no conditional table either: of the eighteen
scripts, sixteen run on every pull request and only `e2e-real-provider` and
`e2e-recipe-gke-troubleshoot-agent` are gated on credentials.

```bash
for s in dev/ci/presubmits/*; do
  n=$(basename "$s")
  case "$n" in e2e-*) continue ;; esac
  echo "=== $n ==="
  "$s" >/tmp/ps-$n.log 2>&1 && echo PASS || { echo FAIL; tail -30 /tmp/ps-$n.log; }
done
```

Globbing the directory rather than listing names is deliberate: a presubmit
added to CI and not to this skill would otherwise be a silent gap, and the
skill cannot be the thing that has to be remembered.

Capture to a file and tail on failure rather than letting output stream. Some
of these produce far more than a screen, and piping a long producer into
`head` is how you get a SIGPIPE that looks like a crash.

Three of them are the ones most often wrongly assumed to be conditional:

- **`examples-smoke`** is unconditional in the required `test` job (#852). A
  `pkg/` API change that an example consumes turns it red without anything
  under `examples/` being touched — exactly the case a paths filter would
  miss, which is why it does not have one.
- **`verify-gke-drill`** is the *offline* check. It runs on every PR as "GKE
  drill (offline)" and needs no cluster and no credentials. The scripts that
  need a live cluster are the `e2e-*` pair.
- **`verify-docs-lint`** and **`verify-version-fallback`** live in their own
  workflows with **no** `paths:` filter, precisely because `ci.yml` skips
  markdown-only changes and these two guard markdown drift.
- **`verify-no-agent-attribution`** also has its own unfiltered workflow. It
  scans the commits in `origin/main..HEAD`, so run it after you commit. CI
  also scans the PR title and body, which a local run can't see.

`e2e-real-provider` and `e2e-recipe-gke-troubleshoot-agent` need credentials
or a live cluster. Run them only when asked, and never assume their absence
means the change is unverified.

## Reading a failure

- **`lint-go` fails but the same code passed a minute ago.** The
  golangci-lint cache goes stale across worktrees, and it is stale in *both*
  directions — phantom failures and false local passes. `golangci-lint cache
  clean` using the binary from `GOPATH/bin`, then re-run.
- **`verify-go-format` fails.** Just run `gofmt -w` on the named files; there
  is nothing to think about.
- **`verify-mod-tidy` fails.** `go mod tidy` and stage the result.
- **`test-unit` fails in a package you did not touch.** Check it fails on a
  clean tree too before assuming you caused it.

## What the sweep does not prove

It proves the change does not break what is already checked. It does not
prove the new behaviour is correct, and it does not prove a new test would
have caught the bug it is supposed to catch — that is the
`prefix-failure-verification` skill, and it is a separate step.
