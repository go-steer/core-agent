---
title: Attach TUI
---

`core-agent-tui` is the operator-facing terminal UI for attach-mode — the remote client for an agent running elsewhere (workstation, K8s pod, peer-registered fleet member). It ships as a separate binary so the default `core-agent` stays distroless-clean (no terminal-rendering deps land in production K8s images).

**Related references:**
- [`core-agent-tui` CLI reference](/reference/core-agent-tui/) — flag table, env vars, exit codes, install.
- [Attach HTTP endpoints](/reference/attach-http/) — the daemon-side protocol the TUI speaks.
- [Configuration → attach](/reference/configuration/) — daemon-side listener config (`attach.listen`, tokens, `multi_session`, peer-hub).

For local interactive use, run `core-agent` directly — its in-process TUI is the default when stdin is a terminal. `core-agent-tui` is the remote client only.

## Why a separate binary

`core-agent-tui` is a thin shell over [`go-steer/core-tui`](https://github.com/go-steer/core-tui) (Bubble Tea + Glamour + Lipgloss live there now); the `core-agent` binary itself pulls in zero terminal-rendering deps. For the K8s use case — a long-running headless agent with `--attach-listen` — that distroless image stays tight. Splitting the operator surface into its own binary keeps both pieces single-purpose.

Two release artifacts:

```
core-agent_<os>_<arch>        # default — K8s, distroless, headless
core-agent-tui_<os>_<arch>    # for laptop operators
```

If you have Go installed: `go install github.com/go-steer/core-agent/v2/cmd/core-agent-tui@latest`.

## Quick start

```bash
# 1. Bare invocation — stdin prompts for an attach URL.
core-agent-tui

# 2. Remote — point at a running agent's --attach-listen.
ATTACH_TOKEN=$(openssl rand -hex 32) \
  core-agent --no-repl --attach-listen=:7777 \
  --attach-token=ATTACH_TOKEN

core-agent-tui http://localhost:7777 --token=ATTACH_TOKEN
```

`--no-repl` runs `core-agent` as an attach-only daemon (no stdin REPL, no in-process TUI). A durable eventlog comes with it — attach mode turns `--session-db` on by itself, because the live-tail broadcaster has nothing to pump from without one. Pass `--session-db-path=PATH` to choose where it lands; see [Sessions](/concepts/sessions/#attach-mode-implies-durability).

URL forms (same grammar as `core-agent attach`):

| URL | Behavior |
|---|---|
| `http(s)://host:port` | Hub form — TUI opens the session picker, enumerating local + peer sessions in parallel |
| `http(s)://host:port/sessions/<sid>` | Direct-jump — TUI skips the picker and enters that session |
| `http(s)://host:port/sessions/<app>/<sid>` | Qualified direct-jump |
| `unix:///path/to/socket` | Unix-socket hub |
| `unix:///path/to/socket/sessions/<sid>` | Unix-socket direct-jump |

## Flags

| Flag | Purpose |
|---|---|
| `--token=<ENVVAR>` | Name of the env var holding the bearer token (same indirection as `--attach-token` on the listener side). The secret never appears on the command line. |
| `--auth=<strategy>` | Auth strategy for outbound attach requests. `bearer` (default) sends the attach token in `Authorization: Bearer` — the direct-attach path. `google-id-token` (recommended for Cloud Run IAM / IAP) mints a Google ID token via Application Default Credentials, audience-bound to the connection URL, and stamps both `Authorization: Bearer <ID-token>` + `X-Attach-Token`. `google-oauth` is an alternative that uses OAuth access tokens via `google.FindDefaultCredentials` (matches MCP's pattern for Google APIs) — Cloud Run IAM rejects this in many deployments, prefer `google-id-token` unless you specifically need OAuth scope behavior. See "Behind an identity gateway" below. |
| `--theme=auto\|dark\|light` | Force a glamour theme for markdown rendering. Empty = auto (terminal background detection via OSC 11). |
| `--alias=<label>` | Display label for the agent identity in the status bar. Defaults to the session ID. |
| `--no-mouse` | Start with terminal mouse capture off, restoring native click-drag text selection. Capture is on by default so the wheel scrolls the chat viewport. The bypass modifier is terminal-specific (Shift-drag on most terminals, Alt/Option-drag in VS Code's integrated terminal), so this flag is the reliable answer when selection appears broken. Toggleable at runtime with `/mouse`, but this client reads no config file — only the flag persists the choice across launches. |
| `--version` | Print build identity (`core-agent-tui v2.2.0 (commit a1b2c3d4, built 2026-06-01T…)`) and exit. |

### Behind an identity gateway (Cloud Run IAM, IAP, Cloudflare Access, …)

Deployments behind an identity gateway have a single-Authorization-header problem: the gateway wants to validate the caller's identity token in `Authorization: Bearer`, and core-agent's listener wants the attach token in the same header. Both can't ride there at once.

The fix is two-sided:

- **Server side**: core-agent accepts `X-Attach-Token` as a side-channel header for the attach token, leaving `Authorization` for whatever the gateway needs. Available unconditionally — no flag to enable.
- **Client side**: `core-agent-tui` knows how to mint the gateway-appropriate credential and stamp both headers. The strategy is selected via `--auth`.

**Server-side header precedence** (whichever ride the attach token uses, compared in constant time):

| Headers a request carries | Outcome |
|---|---|
| `X-Attach-Token: <correct>` | 200 — `Authorization` is left for the gateway |
| `X-Attach-Token: <wrong>` | 401 — does **not** fall through to `Authorization`, since the operator explicitly sent it |
| `Authorization: Bearer <correct>` (no `X-Attach-Token`) | 200 — the direct-attach path, unchanged |
| Neither, or both wrong | 401 |

#### Client-side: `--auth=google-id-token` (recommended for Cloud Run IAM / IAP)

The TUI mints a Google ID token via `idtoken.NewTokenSource` (Application Default Credentials), audience-bound to the connection URL, and stamps both headers automatically. No manual `gcloud auth print-identity-token` invocation; no `gcloud run services proxy` hop.

```bash
# One-time setup on the operator's machine (skip on GCE/GKE/Cloud Run/Cloud Shell —
# ADC picks up the runtime's service account automatically):
gcloud auth application-default login

# Attach. Audience derives from the connection URL automatically.
core-agent-tui --auth=google-id-token \
  --token=ATTACH_TOKEN \
  https://my-svc-abc123-uc.a.run.app
```

Behavior:

- The TUI calls `idtoken.NewTokenSource(ctx, serviceURL)` — token source caches the ID token until expiry (~1 hour).
- Per request: `Authorization: Bearer <ID-token>` (gateway validates against the service's IAM bindings — operator must have `roles/run.invoker`) + `X-Attach-Token: <attach-token>` (core-agent validates against `--attach-token`).
- Cloud Run forwards the request to the container with the operator's identity attached as `X-Goog-Authenticated-User-Email` / `X-Goog-Authenticated-User-Id` headers. Core-agent doesn't consume these today; tracked separately under [#142](https://github.com/go-steer/core-agent/issues/142).

**Common failure modes:**

| Symptom | Cause | Fix |
|---|---|---|
| `Application Default Credentials unavailable` at startup | ADC isn't configured | `gcloud auth application-default login` |
| `unsupported credentials type: "authorized_user"` at startup | `idtoken.NewTokenSource` requires service-account-shaped ADC; end-user ADC isn't accepted. Most common on local workstations after a plain `gcloud auth application-default login`. | Re-login ADC with service-account impersonation: `gcloud auth application-default login --impersonate-service-account=SA_EMAIL` (operator needs `roles/iam.serviceAccountTokenCreator` on SA_EMAIL). Alternatively, set `GOOGLE_APPLICATION_CREDENTIALS=/path/to/sa-key.json`. |
| Gateway 401 from Cloud Run | Operator (or impersonated SA) lacks `roles/run.invoker` on the service | `gcloud run services add-iam-policy-binding <svc> --member="user:$(gcloud config get-value account)" --role=roles/run.invoker` (or the same for the impersonated SA) |
| Core-agent 401 after gateway passes | Wrong `ATTACH_TOKEN` or daemon running without `--attach-token` | Verify env var resolves to the right value; if daemon is in Posture B, omit `--token=` entirely (see below) |

#### Client-side: `--auth=google-oauth` (alternative; not recommended for Cloud Run IAM)

Uses `google.FindDefaultCredentials` to source a Google OAuth2 access token, stamps it on `Authorization`. Mirrors MCP's `google_oauth` pattern (`pkg/mcp/lifecycle.go`), which is the right shape for Google APIs that take OAuth access tokens (Vertex, GKE, etc.).

**Cloud Run IAM rejects OAuth access tokens in many deployments** (response: `error_description="The access token could not be verified"`). Use `--auth=google-id-token` for Cloud Run IAM instead. This strategy stays available for any future gateway that specifically accepts OAuth access tokens with a `cloud-platform` scope.

**Two postures the daemon can run in:**

- **Posture A — IAM + ATTACH_TOKEN (default-recommended, belt-and-suspenders):** server launched with `--attach-token=ATTACH_TOKEN`, client passes `--token=ATTACH_TOKEN`. Defense in depth against IAM misconfig (accidental grant to `allAuthenticatedUsers`, leaked invoker service account, future org-policy changes).
- **Posture B — IAM only (simpler, trusts IAM as the sole gate):** server launched without `--attach-token`, client omits `--token=` entirely. Removes a managed secret. Sensible when IAM bindings are tightly scoped to a small group of named principals.

#### IAP / other gateways

IAP specifically requires ID tokens with the OAuth client ID as audience (not the service URL). Today `--auth=google-id-token` derives the audience from the connection URL — fine for Cloud Run, wrong for IAP. An explicit `--auth-audience=<oauth-client-id>` override flag is the planned addition once an IAP target is available to validate against.

For other gateways (Cloudflare Access, AWS ALB+Cognito, …), today's workaround is to mint the gateway credential out-of-band and pipe it in via a shell wrapper; first-class support depends on the same future `--auth-audience` flag plus a "generic header-cmd" escape hatch that's been floated but not scoped.

Until then, the documented attach path for non-IAM gateways remains a wrapper around `gcloud run services proxy` or the equivalent.

## Operator surface (slash parity with the in-process TUI)

`core-agent-tui` shares its operator surface with the in-process TUI — all the slash commands from the [in-process slash reference](/run/interactive/slash-reference/) work end-to-end against a remote agent. Highlights:

| Command | Effect |
|---|---|
| `/help`, `/quit`, `/clear` | Standard housekeeping. |
| `/stats` | Cumulative token + cost totals, per-model breakdown. Pulls from the remote's `usage.Tracker`. |
| `/usage` | Extended attribution: cached-vs-uncached input tokens, per-turn history, cache-savings vs uncached-reference cost. Fetches `GET /sessions/<id>/usage`. Companion to `/stats` — `/stats` is the terse aggregate, `/usage` is where you look when digging into where the tokens went. |
| `/context` | Compactions, checkpoints, summarized chars, subtask cost. |
| `/memory` | Current `AGENTS.md` chain (project + user-global). |
| `/skills` | Loaded skills with trigger descriptions. |
| `/mcp` | Configured MCP servers and their status. |
| `/perms`, `/permissions` | Gate mode + active allow/deny patterns + per-session approval log. |
| `/allow <pattern>`, `/deny <pattern>` | Add patterns to the live gate (and to `.agents/config.json` if writable on the daemon side). |
| `/pricing`, `/pricing refresh`, `/pricing set <id> <in> <out>` | Inspect or override the pricing layer. |
| `/reload` | Re-walk memory + skills + MCP config on the daemon; surfaces per-surface results (`Memory: ✓`, `Skills: ✓`, `MCP: ✗` with errors inline). |
| `/title <name>` | Rename this session so the picker shows something you chose instead of the title the agent inferred from your first prompt. `/title --clear` drops the name and re-arms automatic titling; a bare `/title` prints usage rather than clearing, since typing a command to see what it does shouldn't destroy anything. POSTs `/sessions/<id>/title`; the reply reports the name as **stored** (the daemon trims and caps it), and warns when it couldn't be persisted durably. |
| `/compact [focus]`, `/done [note]` | Trigger summarization or task-boundary checkpoints on the remote agent. The TUI shows an in-chat preamble row during the 5–30 s round-trip. |
| `/btw <question>` | One-shot context-grounded side question. |
| `/subagent <goal>` | Spawn a background subagent on the remote agent (requires `--no-background-agents=false` daemon side). |
| `/tools [source]`, `/subagents` | List the daemon's tool palette and the configured subagent roster. `/tools` groups by `source` with a count per group (declarative subagents wired as parent tools show `subagent`; MCP/skill tools currently show `other`), `builtin` first and `other` last, and pass a source to bring that group's descriptions back — which is the difference between reading your own 14 built-ins and scrolling past 31 rows of somebody's MCP server. `/subagents` shows the roster the daemon loaded — name, model, `root`, and `sync`/`async` modes — from `GET /subagents` (distinct from `/agents`, which lists *running* instances). |
| `/interrupt` | Cancel the in-flight model turn on the remote **and hold the session** — see [The hold](#the-hold). Both halves run and both are reported, so an interrupt that killed a turn but failed to shut the gate says so. |
| `/pause` | Shut the gate without interrupting anything: the running turn finishes, and no new one starts until you resume. `POST /pause`. |
| `/continue` (`/cont`) | Open the gate and carry on where the agent left off. `POST /resume` with an empty body. |
| `/abandon` | Open the gate and inject nothing — the interrupted work is dropped and the agent stays quiet until something else drives it. `POST /resume {"mode":"abandon"}`. |
| `/reconnect` | Force-reconnect the SSE stream (resumes from `?since=<lastSeq>` — lossless). |
| `/sessions` | Pop back to the startup session picker (kills the TUI, re-launches). |
| `/switch [<sid>]`, `/sess` | Detach + reattach to a different session **in place**. Bare form opens an in-chat picker (local + peer sessions fanned in parallel via `GET /peers` → per-peer `GET /sessions`; peer rows tagged `[peer:<name>]`); `/switch <sid>` direct-jumps to a local session. Chat wipes; local SSE reader closes; the outgoing daemon session keeps running for later re-attach. |
| `/new` | POST `/sessions` on the current daemon (per-caller bearer auth, ACL-isolated) and detach + reattach to the fresh session in place. Companion to `/switch` for the "I need a clean slate" flow. |
| `/attach <url>`, `/attach <url> <sid>` | Escape hatch for reaching a daemon that isn't peer-registered on the current one (issue #246). Bare form enumerates that daemon's sessions into a system message; `/attach <url> <sid>` direct-jumps in place. An operator-typed URL is explicit intent, so it inherits the operator's startup `--auth` mode + `--token`. (Hub-advertised **peer** endpoints do NOT — see the credential-forwarding note below.) |
| `/transcripts [name]` | Lists and loads the transcript files core-tui writes under `AgentsDir/sessions`. **Not available in attach mode** — `core-agent-tui` wires no `AgentsDir`, so it answers `no AgentsDir wired`. Listed here because the command appears in `/help`; it is a local-TUI feature. (Renamed from `/resume` in core-tui v0.24.0, since `POST /resume` is the endpoint behind `/continue` and `/abandon`.) |
| `/theme dark\|light` | Switch glamour theme; re-renders existing assistant messages. |

Sync slashes (`/context`, `/pricing`, `/reload`, `/perms`, `/title`) hit the corresponding [attach read/mutation endpoints](/reference/configuration/) directly. Async slashes (`/compact`, `/done`, `/btw`, `/subagent`) flow through synchronous POSTs that block until the underlying agent operation completes; the remote TUI renders an in-chat preamble row at dispatch to bridge the 5–30 s gap.

## The hold

Esc against a daemon-driven agent used to be a no-op: the local cancel ended this client's subscription while the daemon's own context carried the turn through to the end, and the next thing to reopen the stream replayed the whole abandoned answer under an unrelated prompt. Since core-tui v0.23.0 the remote adapter implements `coretui.Pauser`, so Esc does what the key has always implied — **stop, and wait for me**.

Two things happen, in order: the in-flight turn is cancelled (`POST /interrupt`), and then the session's gate is shut (`POST /pause`). With nothing in flight the cancel is skipped. A failure of either half is reported rather than swallowed, because "was my work killed?" is the question the banner exists to answer.

**"In flight" is the daemon's answer, not the client's guess** (core-tui v0.24.1). The turn a hold most needs to stop is a turn producing no output — a parent blocked in `spawn_agent{wait: true}`, or the gap before a turn's first token — and until v0.24.1 the client decided from what it had recently rendered, so through exactly those stretches Esc silently degraded to a bare `/pause`: banner up, turn running on underneath. It now reads the `turn_state` on the [status-update frame](/reference/attach-http/), which the daemon has always emitted truthfully — `streaming` before a turn's first content, `idle` when it commits — and any non-idle state arms the cancel, including `awaiting_permission` and `awaiting_elicit`, since in observer mode that question went to a different client and this operator has no modal to escape.

One gap is left and it is on the daemon side: `GET /status` and the SSE seed still report `idle` for a session that is mid-turn, so a client that attaches *into* a running turn and presses Esc before the first frame arrives gets a hold rather than a cancel ([#896](https://github.com/go-steer/core-agent/issues/896)). Attach before the turn starts, or wait for one frame.

**Held is not idle.** An idle agent picks up the next queued prompt on its own; a held one starts nothing until you say so. That distinction is invisible from the outside, so the TUI renders a banner above the input while the gate is shut, saying whether your turn was interrupted or the loop is merely parked, the reason the daemon gave, and how many background subagents are still running (those are unaffected — the hold gates the main loop).

Three ways out:

| Action | Effect |
|---|---|
| Type and press Enter | **Steer.** The text becomes the new instruction rather than starting a turn that would block on the gate. Sent as `POST /resume {"mode":"steer","steer":"…"}`; the daemon frames it as an interrupt so the model knows its last turn was killed and does not silently redo the abandoned work. |
| `/continue` (`/cont`) | Carry on where it left off. |
| `/abandon` | Drop the interrupted work; the agent stays quiet. |

Slash commands typed while held all dispatch — nothing is in flight to refuse against.

The gate's state reaches the TUI two ways, and attach mode uses both deliberately. The [`pause` SSE frame](/reference/attach-http/) (protocol 1.5.0) is the live source and the one that narrates transitions into the scrollback, because the remote adapter is a `LiveAgent` with a standing subscription. `PauseState()` is polled at 1 Hz alongside the status bar and is seeded **once** from `GET /status` at attach time — which is the case the stream cannot cover, since a session that was already held before you connected had its transition before your `?since=` cursor.

Requires a daemon with a `PauseController`; without one, `/pause` and `/resume` answer **501** and the three commands report themselves unavailable the way every other ungranted capability does.

## Multi-daemon workflow

The `/switch` + `/attach` + peer-hub trio ([issue #246](https://github.com/go-steer/core-agent/issues/246)) turns one TUI window into a control plane for a fleet of daemons. The story:

**One laptop → one TUI → many daemons.** The operator launches `core-agent-tui` against any one daemon in the fleet, then hops between daemons — and between sessions inside each daemon — from the running TUI. No re-launch. No second terminal.

Three pieces make it work:

1. **Peer-hub registration** (daemon-side) — each daemon can register itself with a hub daemon at startup via `--attach-peer-hub` + `--attach-register-to=<hub-url>`. The hub keeps a live view of registered peers via heartbeats (see [Attach HTTP → peer endpoints](/reference/attach-http/#peer-endpoints)). Every daemon that participates knows about every other; there's no central controller.

2. **`/switch`** (TUI, in-process) — inside a running TUI, `/switch` opens an in-chat picker that fans `GET /sessions` in parallel across the current daemon's hub PLUS every registered peer (5 s per-peer timeout so a slow peer doesn't block the list). Pick a row, the TUI detaches from the current session and reattaches to the picked one in place — chat wipes, but the outgoing session keeps running on its daemon for later re-attach. Peer rows tag as `[peer:<name>]`; local rows are unadorned. Bare `/switch` opens the picker; `/switch <sid>` direct-jumps to a local session.

3. **`/attach <url>`** (TUI, escape hatch) — for reaching a daemon that ISN'T peer-registered on the current one (fresh laptop-local daemon, operator-typed URL from a Slack link, ad-hoc jump to a peer that hadn't checked in yet). Bare `/attach <url>` enumerates that daemon's sessions into a system message so the operator can pick manually; `/attach <url> <sid>` direct-jumps in place. An operator-typed `/attach` URL is explicit intent, so it inherits the operator's startup `--auth` mode + `--token` env var.

**Credential forwarding to peers.** Hub-advertised peer endpoints (the rows `/switch` fans in from `GET /peers`) are attacker-influenceable: any registrant on the hub can publish an arbitrary `endpoint`, and connecting to it with the operator's bearer/OAuth token would hand that token to whoever registered the row ([#384](https://github.com/go-steer/core-agent/issues/384)). The TUI therefore only forwards the operator's credentials to a peer endpoint when its **host matches the hub's own host** or is listed in `--trusted-peers` (comma-separated hostnames, no port); every other peer endpoint is contacted **credential-less**. Server-side, the peer hub validates endpoints (absolute http/https URL required), scopes each registration to its owner (only the owner or an admin can deregister), and hides `registration_id` from non-owners. Operator-typed `/attach <url>` targets are exempt — an explicitly typed URL is intent, not attacker-supplied data.

**Worked example.** One operator laptop, three daemons: local dev daemon on `localhost:7777`, staging daemon on Cloud Run, prod daemon on GKE. Staging + prod are peer-registered on the same hub daemon (say, staging is the hub); local isn't in the peer graph.

```bash
# Terminal setup — one TUI, connected to staging (the hub daemon):
core-agent-tui --auth=google-id-token \
  --token=ATTACH_TOKEN \
  https://staging.example.com

# Inside the TUI:
/switch                # → picker shows staging + prod sessions (peer)
/switch <prod-sid>     # → direct-jump to a known prod session
/attach http://localhost:7777 --token=DEV_TOKEN
                       # → escape hatch to the local daemon (not peer-registered)
/new                   # → fresh session on whichever daemon you're currently on
```

Every hop keeps the same TUI process alive. Cost totals, theme, and keybindings persist; only the session-scoped chat + status bar update.

**Auth propagation.** The TUI captures `--auth` + the token env var name at startup. When `/switch` or `/attach` spawns a subordinate client to reach a peer, the same auth strategy runs against the new endpoint (audience-bound per endpoint for `google-id-token`; trivial passthrough for `bearer`). Operators need `roles/run.invoker` (or equivalent) on every peer they intend to hop into — `--auth=google-id-token` doesn't magically grant access, it just relays the credential correctly.

**When to use what:**

| Scenario | Right slash |
|---|---|
| Same daemon, different session I own | `/switch <sid>` |
| Same daemon, pick from live sessions | `/switch` (bare) |
| Same daemon, fresh isolated session | `/new` |
| Registered peer daemon, direct-jump | `/switch <sid>` (picker also works) |
| Registered peer daemon, pick from live | `/switch` (bare) |
| Unregistered daemon (ad-hoc) | `/attach <url>` |
| Kill TUI + repick from startup | `/sessions` |

`/switch` and `/attach` are the ergonomic difference between "I manage a fleet" and "I manage one daemon at a time." Both landed in v2.7.0-dev.1 ([#246](https://github.com/go-steer/core-agent/issues/246) Phase 1).

## Observer mode (LiveAgent)

When the remote agent is running on its own — `autonomous.Run`, scheduled background subagents, MCP-server-triggered activity, other attached operators' injects — the TUI surfaces every event in the chat scrollback as it happens. You don't have to type anything to see what the agent is doing; attaching is enough.

Operator typing still works: the prompt goes through `POST /inject` and the agent's response streams back through the same observer feed. The scrollback shows the full mixture — your prompts, autonomous turns, subagent activity — in order.

Reconnection is automatic. If the daemon dies (restart, SIGHUP, network drop), the TUI shows a transient error row, retries with exponential backoff (5 s → 30 s cap), and resumes from the last-seen event sequence when the daemon comes back. An operator typing during a backoff window pre-empts the sleep so the next attempt happens immediately. No need to kill the TUI and reattach.

The `Live session — your messages drive the agent; events stream as they happen.` row at the top of the chat marks the start of the live feed. Read-only attachments (viewer identities without session-write) instead see `Attached as observer — agent runs autonomously; events stream below.` — same feed, honest disclosure that typing won't inject.

## Permission prompts

If the remote agent runs in `ask` mode (the default), tool calls that aren't pre-allowed pop a modal in the TUI:

```
┌────────────────────────────────────────────────────────────────┐
│ bash wants to run:                                             │
│                                                                │
│   git push origin main                                         │
│                                                                │
│ [y] allow once     [s] allow session     [v] allow `git *`     │
│ [t] allow tool     [a] allow always      [n] deny              │
└────────────────────────────────────────────────────────────────┘
```

The decision round-trips to the daemon via `POST /perms/respond`; the tool call resumes on the remote side. Picking `a` (allow-always) also persists the pattern to the daemon's `.agents/config.json` so subsequent sessions don't re-prompt.

Operators who want zero prompts can pass `--yolo` to the daemon or pre-populate `.agents/config.json`.

## Layout

```
┌─────────────────────────────────────────────────────────────────┐
│ core-agent-tui  ●  scion  ·  ◇ gemini-3.1-pro-customtools       │  status bar
├─────────────────────────────────────────────────────────────────┤
│ user │ what's the status of the canary?                         │
│                                                                 │
│ asst │ The canary deployment in prod is healthy.                │  scrollback
│      │   • 3/3 pods Ready                                       │  (viewport)
│      │   • last rollout: 2026-05-22 14:03 UTC                   │
│                                                                 │
│   ⚙ kubectl get pods (12.4 KB, 200 OK)                          │  tool call
│                                                                 │
├─────────────────────────────────────────────────────────────────┤
│   ↻ "redeploy the canary"                                       │  queue panel
│   ↻ "check the rollout log"                                     │  (only when non-empty)
├─────────────────────────────────────────────────────────────────┤
│ > _                                                             │  input box
└─────────────────────────────────────────────────────────────────┘
  /help  in: 12.4K  out: 1.9K  $0.12   ↳ this turn $0.03            footer
```

### Queue panel

The strip between the scrollback and the input box renders any operator messages typed while the agent is mid-turn. On turn-end, all queued entries get auto-submitted as a single follow-up turn (with a `↻` marker), wrapped in a system-note framing block so the model knows they arrived mid-task. Soft cap of 10 consecutive auto-continues.

### Status bar

`<alias> · ◇ <model>` (or `<wordmark> · ◇ <model>` when no alias was set). The diamond marks the current model; switch with `/model`.

### Footer

`/help` shortcut + cumulative tokens + cumulative cost + last-turn cost. The last-turn cost is computed client-side from the daemon's cached pricing rates so the footer updates per event without an extra round-trip.

## Keybindings

| Key | Effect |
|---|---|
| **Enter** | Submit input (or run slash command). Mid-turn: queue for after current turn finishes. |
| **Shift+Enter** | Insert a newline in the input |
| **Esc** | Contextual — backs out of the innermost surface first: a modal, the help sheet, transcript focus, and only then the agent, which it cancels **and holds**. See [The hold](#the-hold). |
| **Ctrl+C** (once) | Cancel the in-flight turn — unconditional, never absorbed by focus or a modal |
| **Ctrl+C** (twice within 1s) | Quit the TUI |
| **Ctrl+D** | EOF — quit the TUI |
| **PgUp / PgDn** | Scroll the scrollback |
| **Ctrl+E** | Open `$EDITOR` with the current input buffer (fallback: `$VISUAL` → `vi`) |
| **r** (in picker) | Refresh the session list |

### Transcript focus

Same bindings as the in-process TUI — see [Slash reference → Transcript focus](/run/interactive/slash-reference/#transcript-focus) for the full table. **Tab** moves the keyboard out of the composer and into the transcript, `↑`/`↓` move a per-item selection, **Space** folds the selected item, `Shift+←`/`Shift+→` pan a wide diff, and **y** / **c** copy the item / just its code. **Enter** or **Esc** hands the keyboard back.

Focus is a mode, and Esc backs out of it before it reaches anything else. If you Tab into the transcript while a turn is running, the first Esc only returns focus to the composer — the turn keeps going, and a second Esc cancels it. Ctrl+C cancels from anywhere.

Copies go out twice: an OSC 52 escape aimed at your terminal emulator, and a native clipboard write on the machine running the process. In attach mode that second one is the interesting one — `core-agent-tui` runs on your laptop even when the agent is in a pod on the other side of the network, so `pbcopy` / `wl-copy` / `xclip` / `xsel` / `clip.exe` writes the clipboard you will actually paste from. The footer reads `copied N lines` when the host write confirmed and `copied N lines · osc52` when only the escape went out (no clipboard helper on this machine).

## Read-only mode

When connected to a listener started with `--attach-readonly`, the TUI still works for everything except writes:

- ✅ Session enumeration, live tail, observer mode, `/tools`, `/stats`, `/context`, `/memory`, `/skills`, `/mcp`, `/perms`, `/transcript`
- ❌ Sending messages (typing + Enter), `/inject`, `/interrupt`, `/pause`, `/continue`, `/abandon`, `/allow`, `/deny`, `/reload`, `/title`, `/compact`, `/done`, `/subagent`, `/pricing refresh|set`

The hold is a write (`session:write`), so Esc against a read-only attachment reports the failure rather than showing a banner for a gate that never shut — an observer cannot park somebody else's agent.

Writes surface as red `✗` error lines in the scrollback (the server returns 403; the TUI shows the error rather than failing silently).

## Composition

- **Live stream**: SSE over `GET /sessions/<sid>/events`. Lossless replay via `?since=<seq>` so reconnects don't lose history. The adapter exposes [`coretui.LiveAgent`](https://github.com/go-steer/core-tui/blob/main/tui/agent.go) — core-tui's optional capability for hosts whose agent is observed via a continuous event stream rather than driven by per-turn `Run` calls.
- **Wake notifications**: the adapter exposes [`coretui.WakeRequester`](https://github.com/go-steer/core-tui/blob/main/tui/agent.go), fed by the daemon's [`wake` SSE frame](/reference/attach-http/#wake-notifications-protocol-170) — so another operator's `POST /wake`, or a host that wired `Agent.RequestWake` to a background alert, now reaches an attached TUI the same way it reaches a local one. core-tui answers with a toast **and** a permanent `system` row; see the caution on the HTTP reference for why that row's copy over-claims on a bare `POST /wake`. Requires a daemon speaking attach protocol 1.7.0 or later; older daemons send no frame and nothing appears ([#802](https://github.com/go-steer/core-agent/issues/802)). There is deliberately no `/wake` slash: nothing in the TUI ever needed to *ask* the remote to wake — typing a message does that — and the capability is a notification the daemon pushes, not a command the operator sends.
- **Operator hold**: the adapter exposes [`coretui.Pauser`](https://github.com/go-steer/core-tui/blob/main/tui/capabilities.go) over `POST /pause` + `POST /resume`, with the gate's state cached from the `pause` SSE frame and seeded once from `GET /status` at attach. See [The hold](#the-hold). The in-process TUI deliberately declines this capability: local mode is the per-turn `Run` path, where nothing starts a turn but the operator pressing Enter, so there is no gate to open.
- **Hub-and-spoke**: when the launch URL targets a peer-registration hub, the picker fans `GET /sessions` calls in parallel across the hub + every registered peer, with a 5-second per-peer timeout so a slow peer doesn't block the list.
- **Permissions bridge**: a background goroutine subscribes to `GET /perms/stream` (SSE) for pending prompts; each frame becomes a modal; the operator's decision posts to `POST /perms/respond` and the daemon's blocked `AskApproval` call unblocks.
- **Usage panel**: feeds from the same `CustomMetadata.input_tokens` / `output_tokens` shape that `usage.Tracker` consumes for headless runs. Updates on every model event.

For the full design rationale see [`docs/remote-tui-on-core-tui.md`](https://github.com/go-steer/core-agent/blob/main/docs/remote-tui-on-core-tui.md) and [`docs/remote-tui-observer-mode.md`](https://github.com/go-steer/core-agent/blob/main/docs/remote-tui-observer-mode.md).

## Debug logging

For diagnosing connection / render issues:

```bash
CORE_AGENT_TUI_DEBUG=/tmp/coreagent-tui.log core-agent-tui http://localhost:7777
# in another terminal:
tail -f /tmp/coreagent-tui.log
```

Pairs with `CORE_AGENT_DEBUG=<path>` on the daemon side for a two-file view of an attach session — adapter / bridge / broadcaster / SSE handler all log to whichever file each env var names. Silent unless the env var is set.
