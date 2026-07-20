---
title: core-agent-tui (CLI reference)
---


CLI reference for the `core-agent-tui` binary — the operator-facing terminal client for [attach mode](/reference/attach-tui/). Ships separately from `core-agent` so the daemon binary stays terminal-render-dependency-free (the whole reason for the split lives in the [attach-tui behavior doc](/reference/attach-tui/#why-a-separate-binary)).

This page is the flag / env / exit-code lookup. For **what the TUI does** — observer mode, permission prompts, layout, keybindings, multi-daemon workflow — see [Attach TUI](/reference/attach-tui/).

---

## Synopsis

```
core-agent-tui [FLAGS] [URL]
```

`URL` is optional — omit it and the TUI prompts on stdin for a connection URL. Flags may appear in any position (before, after, or interleaved with `URL`); the standard `flag` package's stop-at-first-positional behavior is worked around internally so `core-agent-tui http://... --token=T` and `core-agent-tui --token=T http://...` both parse identically.

## Flags

| Flag | Type | Default | Purpose |
|---|---|---|---|
| `--token=<ENVVAR>` | string | `""` | Name of the env var holding the bearer token (e.g. `--token=ATTACH_TOKEN`). The secret never appears on the command line — the TUI reads `os.Getenv(<ENVVAR>)` at startup. Empty env value is legal (Posture B; see [attach-tui: gateway postures](/reference/attach-tui/#client-side---authgoogle-oauth-alternative-not-recommended-for-cloud-run-iam)). |
| `--auth=<strategy>` | string | `bearer` | Auth strategy for outbound attach requests. Values: `bearer` \| `google-id-token` \| `google-oauth`. See [attach-tui: behind an identity gateway](/reference/attach-tui/#behind-an-identity-gateway-cloud-run-iam-iap-cloudflare-access-) for full behavior and failure-mode table. |
| `--theme=<t>` | string | `""` (auto) | Force a glamour rendering theme. Values: `dark` \| `light` \| `""`. Empty auto-detects the terminal's background via OSC 11. Switchable at runtime via `/theme dark\|light`. |
| `--alias=<label>` | string | `""` (session ID) | Display label for the agent identity in the status bar. Convenient when running multiple TUIs against different daemons in tmux panes — `--alias=prod`, `--alias=staging`. |
| `--new-session` | bool | `false` | Create a fresh session (`POST /sessions`, per-caller ACL-isolated) and attach to it in one shot; skips the picker. Requires the daemon to have `attach.multi_session.enabled` with a configured `SessionFactory` (see [multi-session](/concepts/multi-session/)). Daemons without multi-session return 501 and the TUI exits with a clear error. |
| `--version` | bool | `false` | Print build identity — `core-agent-tui v<semver> (commit <sha>, built <RFC3339>)` — and exit. Short-circuits before any other flag or arg processing. |

Any unrecognized flag surfaces the standard `flag provided but not defined` error and exits with code 2. Explicit `--help` isn't wired; `core-agent-tui -h` produces the auto-generated usage from the `flag` package.

## URL grammar

`URL` (positional argument) accepts:

| Form | Behavior |
|---|---|
| `http(s)://host:port` | Hub form — TUI opens the session picker, enumerating local + peer sessions in parallel. |
| `http(s)://host:port/sessions/<sid>` | Direct-jump — TUI skips the picker and enters that session. |
| `http(s)://host:port/sessions/<app>/<sid>` | Qualified direct-jump (multi-app daemons). |
| `unix:///path/to/socket` | Unix-socket hub. |
| `unix:///path/to/socket/sessions/<sid>` | Unix-socket direct-jump. |
| _omitted_ | TUI prompts on stdin. |

Same grammar as `core-agent attach` (the in-process attach subcommand); URLs are portable between both.

## Environment variables

| Name | Consumed by | Purpose |
|---|---|---|
| `<whatever>` (via `--token=<ENVVAR>`) | `core-agent-tui` | Bearer token for `bearer` auth. Convention: name it `ATTACH_TOKEN` to match `--attach-token=ATTACH_TOKEN` on the daemon side (the same env-var-name indirection). |
| `CORE_AGENT_TUI_DEBUG` | `core-agent-tui` | Path to append verbose adapter / bridge / SSE logs. Silent when unset. Pairs with `CORE_AGENT_DEBUG=<path>` on the daemon for a two-file view of the whole attach session. |
| `GOOGLE_APPLICATION_CREDENTIALS` | google.golang.org/api | Path to a service-account key JSON. Only consulted when `--auth=google-id-token` or `--auth=google-oauth`. Overrides Application Default Credentials discovery. |
| `NO_COLOR` | glamour / lipgloss | Standard — disables ANSI color output when set to any value. Useful for CI-piped `core-agent-tui < prompt.txt`-shape invocations, though the TUI's Bubble Tea render loop expects a real terminal for full interactivity. |

## Exit codes

| Code | When |
|---|---|
| `0` | Clean exit — Ctrl+D, `/quit`, or double-Ctrl+C. |
| `1` | Runtime error surfaced by `run()` — connection refusal, ADC failure, unresolvable URL, daemon 5xx during startup, session-picker cancellation with no fallback. Error message prints to stderr as `core-agent-tui: <reason>`. |
| `2` | Flag parse error — unknown flag, malformed `--auth` value, etc. |

Kills via SIGINT / SIGTERM cancel the context and the TUI exits `0` (the Bubble Tea program handles the cancel cleanly).

## Examples

Basic remote attach with bearer auth:

```bash
ATTACH_TOKEN=$(openssl rand -hex 32) \
  core-agent --no-repl --session-db --attach-listen=:7777 \
  --attach-token=ATTACH_TOKEN &

core-agent-tui http://localhost:7777 --token=ATTACH_TOKEN
```

Fresh session on a multi-session daemon:

```bash
core-agent-tui --new-session --token=ATTACH_TOKEN https://agent.example.com
```

Cloud Run IAM (identity gateway):

```bash
gcloud auth application-default login \
  --impersonate-service-account=operator@my-project.iam.gserviceaccount.com

core-agent-tui \
  --auth=google-id-token \
  --token=ATTACH_TOKEN \
  https://my-agent-abc123-uc.a.run.app
```

Multiple daemons, one TUI per pane, distinguishable aliases:

```bash
# pane A
core-agent-tui --alias=local          http://localhost:7777
# pane B — same operator, remote daemon
core-agent-tui --alias=prod-us-c1 --auth=google-id-token \
  --token=ATTACH_TOKEN https://agent.prod-us-central1.example.com
```

Or jump between them in a single pane via [`/switch` and `/attach`](/reference/attach-tui/#operator-surface-slash-parity-with-the-in-process-tui) — the multi-daemon workflow.

Debug a connection issue:

```bash
CORE_AGENT_TUI_DEBUG=/tmp/tui.log \
  core-agent-tui http://localhost:7777 --token=ATTACH_TOKEN &
tail -f /tmp/tui.log
```

## Install

```bash
# From GitHub Releases:
curl -L https://github.com/go-steer/core-agent/releases/download/v2.7.0/core-agent-tui_$(uname -s | tr A-Z a-z)_$(uname -m).tar.gz \
  | tar xz -C /usr/local/bin core-agent-tui

# From source (Go 1.24+):
go install github.com/go-steer/core-agent/v2/cmd/core-agent-tui@latest
```

The daemon binary (`core-agent`) is a separate download — see the [main install guide](/run/getting-started/) for both.

## See also

- [Attach TUI](/reference/attach-tui/) — what the TUI *does* (permissions, observer mode, layout, keybindings, multi-daemon workflow).
- [Attach HTTP endpoints](/reference/attach-http/) — the protocol the TUI speaks to the daemon.
- [Multi-session daemon](/concepts/multi-session/) — the daemon-side model backing `--new-session` and `/new`.
- [Configuration → attach](/reference/configuration/) — daemon-side listener config (`attach.listen`, `attach.token_env`, `attach.multi_session.*`, `attach.peer_hub`).
