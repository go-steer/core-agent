---
title: Web UI (--ui flag)
---

The `--ui` flag mounts the [mast-web](https://github.com/go-steer/mast-web) operator UI at `/ui/*` on the attach listener. One binary, one port, one auth boundary — the SPA shares the same TLS cert, bearer token, and CORS origin as the attach API endpoints, eliminating cross-origin configuration entirely. The agent serves the UI; no separate web server required.

This is one of four ways to deploy mast-web. The others (hosted SPA, container image, self-host tarball) live in [`go-steer/mast-web`](https://github.com/go-steer/mast-web) and don't involve core-agent at all. Pick `--ui` when you want a single binary, an air-gapped deployment, or local-dev iteration.

## Quickstart

```bash
# Operator workstation or container
core-agent --attach-listen :7777 --session-db --ui

# Browser
open http://localhost:7777/ui/
```

Connect the SPA's first-run modal to `http://localhost:7777` with whatever bearer token the operator set via `--attach-token` (leave blank for an unauthenticated dev backend). Because the SPA loads same-origin against the attach listener, it can also use a relative path: `/attach` as the backend endpoint works.

## Where the assets come from

The UI assets ship inside the `core-agent` binary via `//go:embed`. The build pipeline (`dev/tools/fetch-mast-web`) downloads a pinned mast-web release tarball and extracts it into `internal/webui/dist/` before `go build`. The pin lives in the top-level `.mast-web-version` file:

```
# .mast-web-version
version=v0.1.0
```

Bumping mast-web is a one-line PR + rebuild.

If the embedded bundle is empty (no `fetch-mast-web` was run), `core-agent --ui` refuses to start with a clear error pointing at the fetch step. The fetch script tolerates a blank version pin (logs a skip; useful in CI when the agent build doesn't need the UI).

## `--ui-dir` for local development

When iterating on mast-web against a live agent, point `--ui-dir` at your checkout's `dist/` directory:

```bash
# In one terminal, build mast-web continuously
cd ~/projects/mast-web && make build   # populates dist/

# In another, run the agent serving from that dist/
core-agent --attach-listen :7777 --session-db --ui-dir ~/projects/mast-web/dist
```

The agent serves whatever's in the directory at request time — no rebuild needed when you tweak `web/app.js`. `--ui-dir` implies `--ui`.

## Auth + TLS

The UI route inherits the attach listener's auth boundary:

- `--attach-token=ENV_VAR` — bearer token required for both `/ui/*` and the attach API
- `--attach-tls-cert` + `--attach-tls-key` — TLS for the listener (UI served over HTTPS)
- `--attach-client-ca` — mTLS for client certs

The SPA reaches the attach API via same-origin relative paths, so the browser sends the bearer token (set in the first-run modal) on both UI asset loads and API calls. No CORS configuration required.

## When NOT to use `--ui`

- **Multi-tenant deployments** where the operator UI should be authenticated separately from the agent API. Use the [container image](https://github.com/go-steer/mast-web/pkgs/container/mast-web) deployment instead — it terminates auth in front of the agent and lets you front the UI with IAP / OIDC / SSO independently.
- **Hosted-SPA tryout** ("just let me see what this looks like"). Visit the published mast-web site and point at your backend; no agent rebuild required. *(Coming soon at `go-steer.github.io/mast-web/app/`.)*
- **Custom-branded UI**. Self-host the mast-web tarball with your modifications; the agent doesn't need to know.

## Why same-listener (not a second `--ui-listen`)

A separate port for the UI is occasionally useful — different auth, different LB routing — but the default trade-offs favor a shared listener:

- **One port to expose.** K8s service / Cloud Run / firewall rule simplification.
- **Same-origin automatically.** SPA fetches via relative paths; no CORS allowlist on the API.
- **Single auth boundary.** Can't accidentally leave the UI unauthenticated while gating the API.
- **One TLS cert.** No second listener to manage.

If you need a separate listener, run the [mast-web container image](https://github.com/go-steer/mast-web/pkgs/container/mast-web) on its own port with `BACKEND_URL` pointed at the agent — same effect, cleaner separation.
