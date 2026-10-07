# Self-development soak: the minimal dispatcher (A7)

**Status:** implemented for A7 ([#1213](https://github.com/go-steer/core-agent/issues/1213), v3.0 box A7 in [#1042](https://github.com/go-steer/core-agent/issues/1042)). The parent design is [`selfdev-soak-design.md`](selfdev-soak-design.md). This note covers only what the dispatcher at `dev/uat/selfdev-soak/dispatcher` does and the choices made writing it. The parent's decisions are not restated.

## Problem

Phase 2 of the soak design needs a dispatcher that runs steps 0–3 and 7 of "The dispatcher". It pauses on `soak:pause`, polls the mirror for maintainer-queued issues, hands one to the daemon, and pushes the agent's local commits as a PR with the writer App's token. On a cap or halt it stops the issue. That is enough to run A7: one seeded issue, end to end, with no person in between. Steps 4–6 (re-waking a session on red CI, review comments or a stale branch) and 8 (the nightly harvest) are for the 2-week soak and come later.

## Shape

```
poll (2m) ── soak:pause open? ── yes ──▶ hold
   │ no
   ├── reap: tracked PR closed/merged ──▶ remove its working copy
   ├── active issue in state file? ──▶ resume the watch (never re-inject)
   ├── open agent/* PRs ≥ cap? ──▶ hold
   └── oldest claimable soak:queue issue that passes decision 6
          │  save state, label soak:active
          ▼
   private bare repo ◀── fetch --depth=1 main ── mirror (App token)
          │ clone --depth=1 (file://)
          ▼
   <worktrees-dir>/issue-N  (branch agent/issue-N, soak identity in local config)
          │ POST /sessions, GET /events, POST /inject
          ▼
   session idle ── trip / turn error / timeout / no commits ──▶ stop (step 7)
          │ clean
          ▼
   fetch agent/issue-N back into the private repo ── verify identity ── push SHA ── open PR
```

`--once` runs this for exactly one issue, to a PR or a stop, then exits: 0 for a PR, 1 for a stop. That is the A7 run.

## Settled decisions (do not relitigate)

1. **A Go program in the main module, reusing in-tree clients.** It is `dev/uat/selfdev-soak/dispatcher`, a `main` package, built, vetted, linted and tested with everything else. It talks to the daemon through `internal/attachclient`, reads the durable rows with `pkg/attach`'s own readers (`GuardrailHaltRow`, `GuardrailTurnTrip`, `TurnErrorRow`), reads the attach token with `childenv.TakeFile`, and finds the session's plan with `tools.ActivePlans`. GitHub is a dozen hand-rolled REST calls. The App JWT is built with the standard library, so no new dependency comes in.
2. **Provenance comes from the issue's event history, and it is strict.** The issue must be authored by the maintainer. It must have at least one `labeled soak:queue` event and at least one `assigned` event, and **every** such event must be the maintainer's. Every `renamed` event must be the maintainer's too, because the title is part of the task. One `soak:queue` label applied by anyone else disqualifies the issue for good: removing it and re-applying it does not launder it. Logins compare case-insensitively, and a missing actor (a deleted account) never matches.
3. **The claim is the `soak:active` label, not an assignment to the bot.** The maintainer's assignment is the go-ahead decision 6 reads. The dispatcher marks a taken issue with `soak:active`, and drops it for `soak:stopped` on a stop. An App's bot account can't be relied on as an issue assignee, and an assignee GitHub won't accept is dropped without an error. `--assign LOGIN` adds an assignment on top, best effort.
4. **A pause needs no provenance.** Any open issue labeled `soak:pause` holds the dispatcher, whoever wrote it. Pausing is the safe direction. The pause is checked before every claim and again before every push, so a pause that lands mid-issue holds the push as well.
5. **The working copy is a fresh shallow clone, not a `git worktree`, and credentials live in a private repository.** A linked worktree shares its `.git` with the repository it came from. The agent can write that `.git`, so every later dispatcher git command there would run whatever it planted: a hook, a `core.fsmonitor` command, a filter driver. Those would run in the dispatcher's pod, next to the App key. Instead:
   - the dispatcher keeps a bare repository the agent can't see (`--private-repo`). The App token only ever touches that repository.
   - each issue's copy is a `--depth=1` clone of the base tip from it, so nothing that was ever in the mirror's history is in reach (decision 20).
   - after the session, the branch is fetched back into the private repository and verified and pushed from there.

   The only process that runs inside the agent-writable copy after the agent has had it is the `upload-pack` serving that fetch. Every git call also pins `core.hooksPath=/dev/null`, `core.fsmonitor=false` and `protocol.ext.allow=never`, and clears `credential.helper`.
6. **The token reaches git through a 0600 file, never argv or the environment.** For each fetch or push, the installation token is written to a 0600 file in a fresh 0700 directory under the dispatcher's `TMPDIR`. Git reads it through the stock `store` helper, and the directory is deleted afterwards. The App key is mode-checked like a token file and never echoed. Reading the attach token through `TakeFile` makes the process non-dumpable.
7. **The verified SHA is pushed, never the branch, and never with force.** Pushing `<sha>:refs/heads/agent/issue-N` means a commit the agent adds after verification never goes out. A remote branch left over from an earlier attempt rejects the push. The issue then stops, and its branch is not overwritten.
8. **Identity is checked on every commit past the base (decisions 8 and 16).** The author and committer must both be the configured soak identity, compared by exact name and case-insensitive email. There must be at least one `Signed-off-by`, and every sign-off must name the soak identity. Any other credit trailer (`Co-authored-by` and the rest of the set `verify-no-agent-attribution` scans) is refused, as is any `[bot]@` or noreply address. A configured identity that could never pass fails at startup. The dispatcher sets the identity in the copy's local git config, so the agent's plain `git commit -s` produces it.
9. **"Idle" means a turn ended, the session reports idle, and it has stayed quiet for the settle window.** The event stream is opened *before* the inject, because `turn-complete` is a live-only frame. A turn has ended once a `turn-complete`, a `turn-error` frame or a durable `turn-error` row arrives. After that, `GET /status` must say `idle` with no turn in flight, and no activity may arrive for `--settle` (30s). The settle window covers auto-continue starting another turn. A dropped stream reconnects with `since=<last seq>`, so trip and turn-error rows are replayed, not missed. A session that never ends a turn within `--start-timeout` stops the issue, because a refused turn puts no frame on the wire.
10. **Any guardrail trip stops the issue, even when the agent committed.** That covers both the halt and the per-turn trip, read from the typed frame or the durable row and counted once by event ID. A7's "ended by work" excludes a ceiling or a halt. A last turn that ended in an error stops it too. A session still running at `--session-timeout` is interrupted and stopped.
11. **A stop is written down before GitHub is told.** The reason goes into the state file first, then the comment, the `soak:stopped` label and the removal of `soak:active`. A GitHub failure partway is retried on the next poll, so the stop is never forgotten. The dispatcher never retries the issue itself (decision 17). The stop comment says that removing `soak:stopped` re-queues it.
12. **Restarts resume, and never inject twice.** A state file holds the active issue and the open PRs. `Injected` is saved before the inject call. A restart before that point gives the issue back to the queue: nothing was attempted, so there's no stop to hide. A restart after it resumes watching the same session, and replays from seq 0 so trips that happened while the dispatcher was down are seen.
13. **The whole inject counts as the task, and `injectTask` is the P1 hook.** No `task_bytes` is sent. The issue text comes first, and the rig's fixed instructions follow it: the branch, `-C` addressing, stop at local commits. The approver should judge calls against both, and the dispatcher is the listed `task_from` caller. The text carries only the issue's title, body, number and upstream link, plus the rig's own instructions. It never carries labels or other issue metadata (decision 20). `taskInput` has no field for them. P1's standing-task flag gets set in `daemonClient.injectTask`, and nowhere else (decisions 15 and 19).
14. **The agent is told where its copy is, because a session can't be given a working directory.** `POST /sessions` has no working-directory field. The task names the copy's path and tells the agent to use `git -C` and `go -C`, one command at a time, because a compound `cd … &&` never matches an eligible prefix.
15. **A missing upstream link stops the issue before any session exists (decision 12).** The stop is visible, so the queue never skips an issue silently.
16. **The dispatcher's own `/events` subscription is an attach client.** A7's "no human" assertion has to exclude the dispatcher's identity (`sa:selfdev-dispatcher`) when it counts attach clients. Any other client during the run is still a failure.

## Where steps 4–6 and 8 attach

- **Steps 4–6** (re-wakes for CI, review and rebase) extend `reap`. That loop already visits every tracked PR on every poll. `openPR` keeps the session path and base SHA for this purpose. A re-wake injects into the same session and then reuses `publish`'s collect, verify and push. A non-forced push of a follow-up commit is a fast-forward. Step 5's comment forwarding needs its own provenance check, in the style of `checkProvenance`, for the reviewer and maintainer logins.
- **Step 8** (the harvest and archive) is a separate scheduled job over the eventlog and the PVCs. It does not belong in this process's loop.

## Deployment requirements

- **Shared paths and UID.** The daemon and dispatcher pods mount the workspace volume at the same path, because the task names the copy's path. They run as the same UID, so the agent can write the copy the dispatcher made.
- **The dispatcher's own volume.** `--private-repo`, `--state-file` and `TMPDIR` must be on a volume only the dispatcher pod mounts. The state file has to survive a pod restart, or the active issue is lost and stays `soak:active`.
- **Optional plan lookup.** `--agents-dir` points at the daemon's `.agents` directory as the dispatcher sees it. Without it, the PR says the session recorded no plan.

## What the maintainer creates by hand

- **The writer GitHub App,** owned by the maintainer's account:
  - repository permissions: Contents read & write, Pull requests read & write, Issues read & write, Metadata read;
  - no webhook, and no other permissions;
  - installed on `mastersingh24/core-agent-selfdev` only (Install App → Only select repositories).

  Note its App ID and generate a private key. The key goes into a Kubernetes Secret mounted only in the dispatcher's pod, with `defaultMode: 0400`, and its path is passed as `--app-key-file`. The installation ID is discovered; `--installation-id` skips the lookup.
- **The mirror's labels:** `soak:queue`, `soak:pause`, `soak:active` and `soak:stopped`.
- **The dispatcher's attach identity:** a `users.json` entry for `sa:selfdev-dispatcher`. Its token is mounted into the dispatcher's pod as a file and passed with `--token-file`.
- **The soak's commit identity:** a human-looking name and email for `--commit-name` and `--commit-email`. It is named in the mirror's attribution allowlist, never upstream's (decision 16 as amended).

## Out of scope

- Steps 4–6 and 8, above.
- P1's standing-task flag, and P2's hashed bearer tokens.
- **Body edits by someone other than the maintainer.** The REST events API does not show body edits. On the private mirror, only the maintainer and the two Apps can edit an issue, and only the dispatcher holds the writer App's key. GraphQL's `userContentEdits` is the route if that changes.
- More than one issue at a time, and self-selection from the queue (v2).
- The Dockerfile, the kustomize overlay, and the seed script (phase 2's other pieces).
- Deleting remote `agent/issue-N` branches. A person retrying a stopped issue deletes its old branch first.
