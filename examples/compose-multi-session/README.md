# `compose-multi-session` — a multi-session daemon from `pkg/compose`

The #386 story: everything `cmd/core-agent` wires for its
multi-session daemon is reachable as library composition helpers, so
an embedder builds the same substrate without re-implementing the
CLI. This example assembles the full stack — bearer auth, persistent
permission grants, per-caller session factory, ACL-backed resume —
and then drives it over HTTP as two identities.

## What this shows

- `compose.BuildMultiSessionAuthn` — a `users.json` bearer table
  (mode `0600` required) becomes the per-request `auth.Authenticator`.
- `permissions.New` + `gate.SetGrantStore(&permissions.ConfigGrantStore{...})`
  — an ask-mode check prompts, a scripted prompter answers
  **allow always**, and the grant persists into
  `.agents/config.json` (`permissions.allow`), surviving restarts.
- `compose.SessionFactoryDeps` + `BuildSessionFactory` /
  `BuildSessionResumer` — `POST /sessions` mints a fresh agent per
  caller (own sub-gate, own tracker, own wake loop); the resumer
  reconstructs evicted sessions from the persisted ACL store.
- `attach.NewServer` with `MultiSessionEnabled: true` — per-session
  ACL enforcement: a stranger's read is a **404, not 403**, so
  session existence never leaks.

## Run it

```bash
go run ./examples/compose-multi-session
```

Hermetic: echo mock model, loopback listener, temp dirs, throwaway
tokens. Exits 0 after a clean shutdown.

## Key APIs

| Concern | API |
|---|---|
| authn | `compose.BuildMultiSessionAuthn(config.MultiSessionConfig{...})` |
| grants | `permissions.Gate.SetGrantStore`, `permissions.ConfigGrantStore` |
| sessions | `compose.SessionFactoryDeps`, `BuildSessionFactory`, `BuildSessionResumer` |
| ACL persistence | `attach.NewSessionACLStore(ctx, handle.DB)`, `attach.NewSessionRegistryWithStore` |
| listener | `attach.Options{Authenticator, MultiSessionEnabled, SessionFactory, Resumer}` |

## What you should see

```
  prompt: bash wants "git status" -> answering "allow always"
  persisted to .agents/config.json: permissions.allow = ["bash:git status"]
alice: POST /sessions -> 019f...
bob:   POST /sessions -> 019f...
alice: GET /sessions -> 1 session(s): [...]
bob:   GET alice's /status -> 404 (denied reads as not-found)
alice: POST .../inject -> 200 (wake loop runs the turn)
```

Note the gotcha the example handles for you: state-changing attach
endpoints require `Content-Type: application/json` even on bodyless
requests (CSRF protection).

## Next pointers

- The config-only flavor of the same deployment (run the real CLI):
  [`examples/multi-session-bearer`](../multi-session-bearer).
- Single-session library embedding:
  [`examples/attach-daemon`](../attach-daemon).
- Design rationale: `docs/multi-session-design.md`,
  `docs/compose-extraction-design.md`,
  `docs/session-resume-design.md`.
