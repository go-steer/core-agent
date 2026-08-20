# Native `alert` tool for headless escalation

Design doc for a v2.7 addition to `pkg/tools`: a native, first-class `alert` built-in tool that lets a headless `core-agent` daemon fire escalations to webhooks (Slack Incoming Webhooks, Discord, PagerDuty Events v2, generic JSON endpoints) without shelling out or depending on an external MCP server. Distroless-safe.

**Status:** **shipped in v2.9** ([#593](https://github.com/go-steer/core-agent/issues/593), landed as #607; Phase 2 as [#749](https://github.com/go-steer/core-agent/issues/749)) — not v2.7. Originally written as proposed (2026-07-10) against tracking issue [#192](https://github.com/go-steer/core-agent/issues/192).

> **What shipped.** The tool, the config-driven named-target list, `url_env` resolution, per-target rate limiting, permission gating, and **five** templates: the four this doc specifies — `generic`, `slack`, `discord`, `pagerduty_events_v2` — plus one it did not anticipate, `switchboard` ([#798](https://github.com/go-steer/core-agent/issues/798) — the chat-gateway destination class, see [The `switchboard` template](#the-switchboard-template-798) below). Two details of the Phase 2 spec below were settled differently in the implementation and the sections say so: PagerDuty's routing key travels in the body with **no** `Authorization` header, and the Slack template renders Block Kit `blocks` rather than colored attachments (attachments are Slack's legacy surface; color moved to the Discord embed, where it is first-class).
>
> The consumer recipe is wired: `examples/gke-troubleshoot-agent/` registers an `oncall` target on `ONCALL_WEBHOOK_URL` with `template: "generic"` under a `rate_limit_per_target: "10/min"` and its router skill calls `alert()` rather than the eventlog-only shape this doc was written against.

## Motivation

The v2.6 k8s-triage recipe ([#186](https://github.com/go-steer/core-agent/issues/186)) surfaces the gap directly: a headless triage agent needs to escalate incidents (unresolved past budget, high-severity events, resolved-with-caveats) to a human. Today it has no way to do so from a distroless container:

- **No shell.** `distroless/static-debian12:nonroot` intentionally has no `bash`, no `curl`, no coreutils. That's the whole point — minimal attack surface.
- **No MCP option in-scope.** Slack's official MCP requires Streamable HTTP + OAuth 2.0 (v2.7 material, [#190](https://github.com/go-steer/core-agent/issues/190)). Community MCPs with bot-token auth work but add a moving part; operators who just want "fire a webhook" shouldn't have to run + maintain a separate MCP process.
- **Ad-hoc HTTP client tools** would be worse. A generic `http_post(url, body)` tool means every agent invocation is potential SSRF against the pod's metadata endpoint, sibling pods, or the internal K8s API. No thanks.

A **native, config-driven `alert` tool** solves all three:

- **Pure Go** — no external deps, ships in the `core-agent` binary, fits any distroless image.
- **Config-driven target list** — operators register named webhook targets in `.agents/config.json`; the agent invokes by name. SSRF impossible by construction.
- **Universal shape** — same tool covers Slack Incoming Webhooks, Discord embeds, PagerDuty Events v2, generic JSON. Templates handle the per-service body formatting.
- **Independent of MCP-OAuth** — ships whenever it's ready; no dependency on [#190](https://github.com/go-steer/core-agent/issues/190).
- **Audit-native** — every `alert()` call fires an eventlog entry with the target, level, HTTP response code, duration. Downstream analytics get "how often does the triage agent escalate, to which target, with what response?" for free.

## Goals

- **Distroless-native escalation.** Zero external dependencies. Works in the smallest supported container.
- **SSRF-safe by construction.** Agent can only fire targets the operator pre-registered in config. No arbitrary URL parameter on the tool.
- **Universal webhook target support.** Slack, Discord, PagerDuty Events v2, generic JSON — one tool, per-target template.
- **Per-target rate limiting.** Prevents alert loops (agent stuck in a recursive escalation pattern).
- **Auth per target.** Bearer tokens (PagerDuty), HTTP Basic (some older systems), no-auth (Slack Incoming Webhooks). All via env vars pointing to Secrets, matching the pattern for MCP-OAuth.
- **Audit through the eventlog.** Every alert emits a structured event; queryable via SQL like every other agent action.
- **Composable with plan-first + permission modes.** `alert` is a gated tool; `permissions.allow` patterns work; `record_plan` is NOT required for alert calls (alerts are informational, not mutating).

## Non-goals (v2.7)

- **Arbitrary HTTP client tool.** `alert` is deliberately narrower than a generic `http_post` — the operator's target registry is the allow-list. If operators want general HTTP, they wire an MCP server for that.
- **Two-way conversations.** `alert` is fire-and-forget. If the operator wants the agent to LISTEN for replies (Slack thread replies, Discord DMs), that's an MCP integration, not an alert-tool feature. *(Still true after the [`switchboard` template](#the-switchboard-template-798). It names the session in a header so the gateway can route a reply into `POST /sessions/{sid}/inject`; the tool itself still posts and returns a status code.)*
- **Alert routing rules** ("critical → pagerduty, warning → slack"). Keep the tool dumb — the agent picks the target explicitly. Routing rules can live in the caller's prompt.
- **Cross-daemon alert dedup.** In-memory rate-limit dedup only. A daemon restart resets the rate-limit budget.
- **Retry-until-success queuing.** Best-effort delivery. Failed alerts are logged + surface as tool errors to the agent; the agent decides whether to retry, chain to another target, or give up.
- **Alert acknowledgment / resolution tracking.** No "was this alert seen?" callback. That's downstream tooling territory (PagerDuty ack, Slack reaction bot).

## Conceptual model

### Target registry

Operator declares a set of named targets in `.agents/config.json` under a new `alerts.targets[]` field. Each target has:

- **`name`** — the identifier the agent uses. Alphanumeric + `-` + `_`.
- **`kind`** — always `webhook` in v2.7. Placeholder for future kinds (`smtp`, `pagerduty_api_v2` with polling, etc.).
- **`url` OR `url_env`** — the destination URL, either literal or pulled from an env var. Prefer `url_env` for anything with a token in the URL (Slack Incoming Webhooks include the token in the path). A target whose `url_env` (or `auth` env) is unset when the process starts is **dropped from the registry at startup** rather than advertised — see [Undeliverable targets](#undeliverable-targets-v29).
- **`template`** — how to format the body: `slack`, `discord`, `pagerduty_events_v2`, or `generic`.
- **`auth`** (optional) — for targets that need it. `{"bearer_env": "..."}` or `{"basic_env_user": "...", "basic_env_pass": "..."}`.
- **`description`** — human-readable hint for the LLM. Surfaces in the tool's schema so the agent knows what each target is for.

Example:

```jsonc
{
  "alerts": {
    "targets": [
      {
        "name": "slack-oncall",
        "kind": "webhook",
        "url_env": "SLACK_WEBHOOK_URL",
        "template": "slack",
        "description": "Post to the #sre-oncall Slack channel. Use for unresolved incidents."
      },
      {
        "name": "pagerduty-critical",
        "kind": "webhook",
        "url": "https://events.pagerduty.com/v2/enqueue",
        "template": "pagerduty_events_v2",
        "auth": { "bearer_env": "PAGERDUTY_TOKEN" },
        "description": "Fire a PagerDuty page. Use ONLY for genuinely critical, human-required incidents."
      },
      {
        "name": "audit-webhook",
        "kind": "webhook",
        "url_env": "AUDIT_WEBHOOK_URL",
        "template": "generic",
        "description": "Post structured incident summaries to our internal audit log."
      }
    ],
    "rate_limit_per_target": "1/30s"
  }
}
```

### The `alert` tool

Registered by the `agent.WithTools` path when config declares any targets. Tool schema exposed to the LLM:

```
Tool: alert
Description: Fire a pre-configured alert target. Use for
             escalation, incident summaries, or notifying humans
             of agent decisions.
Parameters:
  target: string  (required) — one of: slack-oncall, pagerduty-critical,
                   audit-webhook. Pick based on urgency + audience per
                   each target's description.
  level:  string  (required) — "info" | "warning" | "critical" | "resolved"
  summary: string (required) — one-line human-readable summary
  details: object (optional) — structured payload merged into the
                   target's template. Fields depend on template kind.
```

Agent calls `alert(target: "slack-oncall", level: "warning", summary: "Incident checkout-svc unresolved past 10 min budget", details: {"cluster": "prod-us-central1", "incident_uid": "abc-123", "session_url": "https://core-agent.../sessions/abc-123"})`.

### Templates

Per-`template` body formatting turns the tool's flat args into the target service's expected wire format.

**`slack`** — [Block Kit blocks](https://api.slack.com/block-kit).

```json
{
  "text": "[warning] Incident checkout-svc unresolved past 10 min budget",
  "blocks": [
    {"type": "header", "text": {"type": "plain_text", "text": "[warning] Incident checkout-svc unresolved past 10 min budget"}},
    {"type": "section", "fields": [
      {"type": "mrkdwn", "text": "*cluster:* prod-us-central1"},
      {"type": "mrkdwn", "text": "*incident_uid:* abc-123"}
    ]},
    {"type": "section", "text": {"type": "mrkdwn", "text": "<https://core-agent.../sessions/abc-123|Attach TUI to this session>"}}
  ]
}
```

**`discord`** — Discord webhook embed.

**`pagerduty_events_v2`** — [PD Events API v2](https://developer.pagerduty.com/docs/events-api-v2/trigger-events/) shape (routing_key from `auth.bearer_env`, event_action from `level` — "critical" → "trigger", "resolved" → "resolve").

**`generic`** — raw JSON pass-through: `{"level": "warning", "summary": "...", "details": {...}, "timestamp": "..."}`. For operators wiring bespoke internal endpoints.

New templates land as small PRs; the built-in registry lives in `pkg/tools/alert/templates/`.

#### What Phase 2 actually built (#749)

The three service templates shipped close to the spec above, with the differences below. They live in `pkg/tools/alert/template_{slack,discord,pagerduty}.go` — one file per template, one package rather than the `templates/` sub-package sketched in the layout, because the shared pieces (sorted detail iteration, character-counted truncation, the `rendered` request struct) are small and only exist for them.

*Every service limit is enforced at render time.* This is the part the spec did not say and the part that decides whether the template works: Slack rejects the entire message with `invalid_blocks` if the header exceeds 150 characters or a section carries more than ten fields; Discord 400s an embed over 6000 characters or 25 fields; PagerDuty caps `payload.summary` at 1024. A model writing a long one-line summary is not an edge case, so each cap is applied here, counted in **characters** rather than bytes (a byte-wise cut can split a rune), and any dropped detail is *named* in the message rather than silently omitted.

*Slack renders `blocks`, not colored attachments.* Colour would require the legacy `attachments` surface, which Slack steers new integrations away from; the level travels in the header text instead. Detail keys and values are entity-escaped (`&`, `<`, `>`) because they are model-supplied data and an unescaped `<` opens a Slack link that swallows the rest of the value — `<none>`, which is what kubectl prints for an empty field, would render as nothing. The header stays unescaped: it is a `plain_text` object, which Slack renders literally.

*Discord is where colour lives*, because an embed has a first-class `color`: blue / amber / red / green for `info` / `warning` / `critical` / `resolved`. A summary too long for the 256-character title is preserved in full in `description` rather than cut.

*PagerDuty's routing key goes in the body and nowhere else.* Events v2 never inspects `Authorization`, so this template suppresses the per-target auth header entirely; sending the key there as well would hand the same secret to whatever sits in front of the URL, mislabelled as a bearer token. It still reads from `auth.bearer_env` rather than a `routing_key_env` field of its own, and that is deliberate: reusing the existing auth surface is what makes an unset key **drop the target at startup** under [Undeliverable targets](#undeliverable-targets-v29). PagerDuty is the last destination that should be advertised to a model and then silently unable to page.

*Two details are promoted to first-class Events v2 fields*: `details.source` → `payload.source` (PagerDuty requires it; default `core-agent`) and `details.dedup_key` → the top-level `dedup_key`. They are ordinary details so the model needs no template-specific argument, and they are removed from `custom_details` once promoted. A `level: "resolved"` with no `dedup_key` is a **tool error naming the field**, not a page PagerDuty rejects: the tool holds no state between calls (the cross-daemon-dedup non-goal), so the correlating key can only come from the caller.

*The timestamp question is closed, as omitted.* The design flagged it as a Phase 2 call. None of the three service templates wants one — Slack and Discord stamp the post, PagerDuty stamps the event and takes an explicit `payload.timestamp` only for backfill — and the eventlog record for the tool call remains the authoritative time. Baking one into `generic` would only make the body nondeterministic.

*The accept-list and the renderer are pinned to each other.* `config.AlertTemplates` is the list the validator accepts, and `TestEveryAcceptedTemplateRenders` ranges over it in `pkg/tools/alert`. A template the config layer admits and the renderer does not know would be a daemon that boots clean and fails at the moment the alert is fired.

### The `switchboard` template (#798)

**Shipped.** `switchboard` posts to [go-steer/switchboard](https://github.com/go-steer/switchboard)'s outbound message API (`POST /v1/messages`) rather than at a chat platform directly:

```json
{ "conversation": "C0123", "text": "**[critical]** checkout-svc has no healthy endpoints\n- `cluster`: prod-us-east" }
```

It is a **destination class, not a service format**, and that is what distinguishes it from the `slack` template specified above. `slack` is Block Kit fired at an Incoming Webhook: one platform, one wire format, fire-and-forget into a channel. `switchboard` is one template for *every* platform the gateway bridges — Slack and Google Chat today, more later — because the gateway already owns the per-platform translation, chunking and rendering. So the body is plain CommonMark and the formatting decisions stay in the process that knows the platform.

Three properties follow from routing through the gateway that a direct webhook cannot have: the message lands in a thread the gateway can address later (edit, append, roll over); a human can reply and have the reply routed somewhere useful; and the caller-identity assertion the gateway performs stays intact.

**Config.** Two fields are required and both are checked at load, because neither can be supplied later:

- `conversation` — the gateway's conversation key. New field on `AlertTarget`, rejected on any other template so a field that would silently do nothing cannot be set. It is routing configuration, like the URL, which is why it is not a tool argument: same reason there is no arbitrary-URL parameter, one level in. A literal rather than a `*_env` sibling, because a conversation key is not a secret (unlike a Slack Incoming Webhook URL, which carries its token in the path).
- `auth.bearer_env` — switchboard's ingress token, reusing the existing auth surface. Its ingress refuses to *start* without a token, so a target lacking one is an escalation path that can only 401. Being a `*_env`, an unset value also drops the target at startup under the rule below.

**The session id rides a header.** `X-Agent-Session: <session id>` names the session the alert was fired from, so the gateway can bind the thread it creates back to that session and route a human's reply into it instead of into a fresh session with none of the incident context. A header rather than a body field for one concrete reason: switchboard's ingress decodes with `DisallowUnknownFields` — a deliberate choice on its side — so an unrecognised body field is a `400` from every deployed gateway, not a field it ignores. A header is the part of an HTTP request that is *defined* to be ignorable when unknown, so one binary talks to a gateway that reads it and one that doesn't, with no version negotiation. Switchboard already accepts caller metadata this way (`Idempotency-Key`). Only this template sends it; a session id is internal routing detail and a third-party webhook has no reason to learn one.

**This does not reopen the two-way-conversation non-goal.** The tool still posts and reports a status code; nothing here listens. Naming the session is what lets a *separate*, already-existing inbound path — `POST /sessions/{sid}/inject`, gated on `SessionACL.Contributors` ([#797](https://github.com/go-steer/core-agent/issues/797)) — deliver the reply. The alert tool's half stays fire-and-forget.

**Why not a translating shim.** A sidecar converting `{level, summary, details}` into `{conversation, text}` is three more things to deploy, monitor and explain, in order to avoid a small template in the place templates already live — and it would put a box in the architecture diagram that exists only because two of our own components disagree about a payload.

### Rate limiting

Per-target token bucket. Default: 1 alert per 30 seconds per target (configurable via `alerts.rate_limit_per_target`). Exceeded → tool returns a structured error so the agent knows "you're being rate-limited on this target"; agent decides whether to try another target or defer.

Not cross-target — the agent CAN fire `slack-oncall` immediately after `pagerduty-critical`, since they're distinct targets. The intent is to catch pathological loops, not to enforce operational cadence.

### Auth surface

- `auth.bearer_env: NAME` — env var carries the bearer token; tool adds `Authorization: Bearer <token>` header.
- `auth.basic_env_user: U`, `auth.basic_env_pass: P` — HTTP Basic. Both required if either is set.
- Absent `auth` → no auth headers (Slack Incoming Webhooks work this way — the token is in the URL).

Auth material never appears in the tool schema or in eventlog audit rows (only the target name + response code).

### Audit trail

Every `alert()` invocation emits an eventlog entry with:

- `Author = "tools/alert"`
- `Metadata = {"target": "slack-oncall", "level": "warning", "http_status": 200, "duration_ms": 143}`
- Response body NOT included (may contain PII from the destination service). Response status code IS included for troubleshooting.

Multi-session mode also stamps the calling identity via the existing metadata sidecar (`caller: "..."`).

## Detailed design

### Config parsing

New top-level `alerts` block in `pkg/config`:

```go
type AlertsConfig struct {
    Targets            []AlertTarget `json:"targets,omitempty"`
    RateLimitPerTarget string        `json:"rate_limit_per_target,omitempty"` // duration or "N/duration"
}

type AlertTarget struct {
    Name         string     `json:"name"`
    Kind         string     `json:"kind"`         // "webhook"
    URL          string     `json:"url,omitempty"`
    URLEnv       string     `json:"url_env,omitempty"`
    Template     string     `json:"template"`     // generic|switchboard|slack|discord|pagerduty_events_v2
    Conversation string     `json:"conversation,omitempty"` // switchboard only
    Auth         *AlertAuth `json:"auth,omitempty"`
    Description  string     `json:"description,omitempty"`
}

type AlertAuth struct {
    BearerEnv    string `json:"bearer_env,omitempty"`
    BasicEnvUser string `json:"basic_env_user,omitempty"`
    BasicEnvPass string `json:"basic_env_pass,omitempty"`
}
```

Validation:
- `name` matches `^[a-zA-Z0-9_-]+$` (safe for logging + schema).
- Exactly one of `url` / `url_env` set.
- `template` is one of the known values.
- If `kind == "webhook"` and no template matches, reject at load time.
- `conversation` is required by `switchboard` (and must hold no whitespace or control characters, mirroring the gateway's own check) and rejected by every other template.
- `auth.bearer_env` is required by `switchboard` (its ingress token) and by `pagerduty_events_v2` (the integration's routing key).
- Names unique across the target set.
- `rate_limit_per_target` parses via `time.ParseDuration` (single duration = 1 alert per that duration) OR `N/duration` for N-per-duration.

Validation is deliberately *structural only* — it never reads the environment, because a config file is valid or not independently of where it runs. The environment check happens at registration instead:

### Undeliverable targets (v2.9)

**Shipped.** The original design resolved `url_env` and `auth` env purely at call time, on the reasoning that a rotated Secret should be picked up without a restart. The rotation argument is right; the *registration* consequence was wrong.

A live 2026-08-14 run made it concrete. The deployment configured an `oncall` target reading `PLATFORM_AGENT_ALERT_WEBHOOK`, the Secret was never mounted, and the agent — which had `alert` in its schema and an `AGENTS.md` telling it to escalate — worked an incident to `UNRESOLVED`, called `alert`, and only then discovered nobody had been paged. Its own hand-off said it: *"The escalation webhook failed to deliver … so nobody has been paged — this report is the only hand-off."* The failure landed at the one moment the capability mattered.

A process's environment is fixed at exec time, so "unset at startup" already means "unset for this process's whole life"; the rotation case only covers a *changed* value, never an absent one. So `newHandler` now partitions the registry (`PartitionTargets`): unresolvable targets are dropped from the routing table and from the description the model reads, and `pkg/tools.Build` gates registration on `alert.HasLiveTarget(cfg)` so a build where none survive registers no `alert` tool at all. `cmd/core-agent` prints one stderr line per dropped target naming the env var. The call path still re-resolves both URL and auth, so rotation keeps working and a variable that disappears mid-process fails closed.

Same rule as the built-in description cross-references ([#759](https://github.com/go-steer/core-agent/pull/759)), one layer down: never name a capability the deployment doesn't have.

Two things this deliberately does *not* do. It does not fail the boot — alerting stays optional, and a daemon that can still triage is worth more than one that won't start. And it does not reach into a recipe's prose: an `AGENTS.md` that says "escalate via `alert`" survives the tool's absence, so recipes need their own fallback wording (the `gke-troubleshoot-agent` skill now carries one).

### Tool implementation

New package `pkg/tools/alert`:

```
pkg/tools/alert/
├── alert.go          — tool registration + top-level dispatcher
├── templates/
│   ├── generic.go
│   ├── slack.go
│   ├── discord.go
│   └── pagerduty_events_v2.go
├── ratelimit.go      — per-target token bucket
├── audit.go          — eventlog write helper
└── *_test.go         — one per file
```

Tool entry point:

```go
func (t *alertTool) Run(ctx tool.Context, args AlertArgs) (*AlertResult, error) {
    target, ok := t.targets[args.Target]
    if !ok {
        return nil, fmt.Errorf("alert: unknown target %q (available: %v)", args.Target, t.targetNames())
    }
    if !t.limiter.Take(args.Target) {
        return nil, fmt.Errorf("alert: rate-limited on target %q (try another target or wait)", args.Target)
    }
    body, err := t.renderTemplate(target.Template, args)
    if err != nil {
        return nil, fmt.Errorf("alert: render template %q: %w", target.Template, err)
    }
    req, err := t.buildRequest(target, body)
    if err != nil {
        return nil, err
    }
    start := time.Now()
    resp, err := t.httpClient.Do(req)
    duration := time.Since(start)
    // Audit event fires regardless of outcome.
    t.audit(ctx, args.Target, args.Level, resp, err, duration)
    if err != nil {
        return nil, fmt.Errorf("alert: post to %q: %w", args.Target, err)
    }
    defer resp.Body.Close()
    if resp.StatusCode < 200 || resp.StatusCode >= 300 {
        respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
        return nil, fmt.Errorf("alert: %q returned %d: %s", args.Target, resp.StatusCode, string(respBody))
    }
    return &AlertResult{Target: args.Target, StatusCode: resp.StatusCode, DurationMs: duration.Milliseconds()}, nil
}
```

### Permission scoping

`alert` fits the existing gate pattern. Default posture:

- `permissions.mode: ask` — every `alert()` prompts the operator.
- `permissions.mode: acceptEdits` — auto-allow (alert is informational, not mutating).
- `permissions.mode: yolo` — auto-allow.

Operators can further restrict via `permissions.allow` patterns:

```json
{
  "permissions": {
    "allow": [
      "alert:slack-oncall",       // allow only this target
      "alert:audit-webhook"
    ]
  }
}
```

`alert:*` allows all targets; per-target patterns give finer scoping.

### HTTP client

- `http.Client{Timeout: 10s}` — tight bound. Alerts must be responsive; a slow webhook shouldn't hang a triage session.
- No custom transport; uses `http.DefaultTransport`.
- No proxy handling beyond what `http.DefaultTransport` picks up from env (respects `HTTPS_PROXY`).

### Rate-limit configuration format

`rate_limit_per_target` accepts:
- `""` (default) — no rate limit
- `"1/30s"` — 1 per 30s
- `"5/min"` — 5 per minute
- `"100/hour"` — 100 per hour
- `"30s"` — shorthand for `"1/30s"`

Parsed by a small helper that produces a `rate.Limiter` per target on registration.

## Per-substrate impact

### `pkg/config/config.go`

- New `AlertsConfig` type at top level (`Config.Alerts`).
- New `AlertTarget` + `AlertAuth` types.
- Validator + tests.

### `pkg/tools/alert/` (new)

- Tool implementation per above.
- Template registry (initial 4 templates).
- Rate limiter.
- Eventlog audit helper.
- Tests: httptest-mock target servers, template rendering per kind, rate limiting behavior, auth-header verification, audit-event content.

### `pkg/tools/build.go` (wire-in)

- If `cfg.Alerts.Targets` non-empty AND at least one target validates: register the `alert` tool via `agent.WithTools`.
- If no targets configured: tool is not registered (LLM sees no `alert` in the toolset). Fits the "config-driven registration" pattern used elsewhere.

### `cmd/core-agent` — no changes

- Config already flows through the daemon's startup path; the new field just gets picked up.

### `docs/site/content/docs/reference/`

- Update `tools.md` with a new "Alert tool" section.
- New page: `alerts.md` walking through target registration, template shapes, permission scoping, examples.

### `examples/gke-troubleshoot-agent/`

- Once alert tool ships: update the recipe's `.agents/config.json` with sample alert targets, update the router SKILL.md to prefer `alert()` over the current eventlog-only shape.
- This is the ε.3-replacement PR of the v2.6 stack — files as v2.6.1 or v2.7 recipe update.

**Done (v2.9, [#644](https://github.com/go-steer/core-agent/issues/644)):** the recipe carries one `oncall` target (`generic`, `url_env: ONCALL_WEBHOOK_URL`, `10/min`) fed by an optional `core-agent-alerts` Secret, and the router skill calls `alert()`. `examples/gke-troubleshoot-agent/recipe_test.go` pins the target so it can't silently regress to eventlog-only.

## Migration story

Net-new feature. No migration.

- **Existing deployments** — no config change → no behavior change. `Config.Alerts.Targets == nil` means the tool isn't registered.
- **Adding alert targets** — operator adds `alerts.targets[]` to their `config.json`, creates the Secret(s) for any bearer / basic auth, rolling restart.
- **Slack Incoming Webhook setup** — 5-minute Slack app creation; produces one URL. Stash in a Secret, wire via `url_env`. No OAuth, no client secret, no bot approval.

## Implementation phases

### Phase 1 — config parsing + tool primitive + generic template (PR ε.1 of #192)

- `pkg/config` additions.
- `pkg/tools/alert/` skeleton + `templates/generic.go` + tool registration wire-up.
- Rate limiter + audit helper.
- Tests: config-parse matrix, tool end-to-end with httptest mock, rate-limit behavior, audit content.

Estimate: ~350 LoC prod + ~300 LoC tests. ~2 days.

### Phase 2 — service-specific templates (PR ε.2 of #192)

**Done ([#749](https://github.com/go-steer/core-agent/issues/749)).** `template_slack.go`, `template_discord.go` and `template_pagerduty.go` in `pkg/tools/alert`, plus the load-time requirements the three imply (`auth.bearer_env` for PagerDuty) and per-template tests covering each service's documented limits. See [What Phase 2 actually built](#what-phase-2-actually-built-749) for the four places the implementation and this spec differ.

### Phase 3 — docs + recipe update + CHANGELOG (PR ε.3 of #192)

- Update `docs/site/content/docs/reference/tools.md` and add `alerts.md`.
- Update `examples/gke-troubleshoot-agent/` recipe to demonstrate alert targets (this closes the v2.6 escalation gap).
- CHANGELOG v2.7.0 entry (paired with MCP-OAuth [#190](https://github.com/go-steer/core-agent/issues/190) which ships in the same release).
- Design doc status flip to "shipped in v2.7".

Estimate: ~200 LoC docs + ~150 LoC config/skill updates. ~1 day.

**Total**: ~1,450 LoC across 3 PRs, ~5 days of focused work. Similar shape to MCP-OAuth but half the design complexity.

## Open questions

### 1. Is `alert` the right tool name

- **`alert`** (current design) — matches "the intent" (agent alerts a human).
- **`notify`** — more general; could conflict with future non-webhook notification kinds (Slack MCP `post_message`, in-band prompts).
- **`post_webhook`** — most descriptive; matches the actual mechanism.
- **`escalate`** — matches the ONE use case we've thought about; too narrow (agents may want to send info-level alerts that aren't escalations).

**Recommendation**: `alert`. Widely understood; matches the primary intent. If future non-webhook kinds emerge (SMTP, native Slack), they get their own tools.

### 2. Should the tool schema list targets, or accept any string

- **List available targets in the schema** (current design). LLM sees `target: string, one of: [slack-oncall, pagerduty-critical, ...]` with descriptions inline. Better matching; harder to fat-finger.
- **Accept any string, return error on unknown target**. Simpler schema; the error message tells the LLM what's available.

**Recommendation**: list targets. The LLM's default matching is much better when the choices are enumerated + described. Trade: the schema regenerates when config changes; agent needs a reload after config edits (which is already true for other config-driven tools).

### 3. Where does the eventlog audit write happen — inside the tool, or a gate hook

- **Inside the tool** (current design). Tool implementation calls `eventlog.Append` directly with the audit metadata.
- **Gate hook** — the permission gate wraps every tool call with an audit-log wrapper. Consistent across tools but requires expanding the gate's audit surface to carry per-tool metadata (currently it's just tool name + allow/deny).

**Recommendation**: inside the tool for v2.7. The gate's audit is coarse-grained (permission decisions); tool-level audit is fine-grained (what happened during the call). Different concerns; both should coexist.

### 4. Do we support multi-target fan-out in one call

- **Single target per call** (current design). `alert(target: "slack-oncall", ...)`. Agent iterates if it wants multiple.
- **Multi-target fan-out** — `alert(targets: ["slack-oncall", "pagerduty-critical"], ...)`. One call, N HTTP requests, N audit events.

**Recommendation**: single-target for v2.7. Simpler contract; matches how most webhook services expect to be called; agent iteration is trivial. Fan-out is a v2.8+ enhancement if operators ask for atomic multi-target semantics.

### 5. Template extensibility — operator-defined templates via config

- **Built-in templates only** (current design). Ships with slack/discord/pagerduty_events_v2/generic; new ones require a PR.
- **Templates via Go text/template** in config — operator supplies a template string per target; tool renders with the args.
- **Templates via WebAssembly / plug-in** — extreme flexibility, extreme complexity.

**Recommendation**: built-in only for v2.7. Text-template-in-config is a v2.8+ enhancement if operators need bespoke internal formats beyond `generic` pass-through.

### 6. Retry-on-transient-failure

- **No retries** (current design). Tool returns failure; agent decides.
- **N retries with exponential backoff** — configurable per-target retry count.
- **Retry only on 5xx**, not on network errors or 4xx — 4xx is a config issue (bad body); no point retrying.

**Recommendation**: no retries for v2.7. Agent-level retry loop is more flexible (agent knows the incident context; can escalate to a fallback target on failure). Add retry as a target-config field if operators demonstrate need.

### 7. Cross-daemon rate-limit / dedup

- **In-memory only** (current design). Rate limit resets on daemon restart. Alert loops during a restart-storm are possible but rare.
- **Persistent rate-limit state** — write bucket state to disk (SQLite eventlog?).
- **External backend** — Redis / distributed rate limiter.

**Recommendation**: in-memory for v2.7. Daemon restarts are infrequent enough that a transient loop is acceptable; the fix (rate limits work) is more important than perfect coverage.

### 8. Alert response bodies in audit

Some webhook responses carry useful info (Slack returns the message timestamp for later thread replies). Currently the design captures response code only, no body.

- **Status code only** (current design). No PII risk; small audit footprint.
- **First 512 bytes of response body**. Useful for troubleshooting; some PII risk.
- **Configurable per target** — `capture_response: true` in target config.

**Recommendation**: status code only for v2.7. Add `capture_response` as a follow-up if operators ask for thread-reply support (which needs the response body to know the thread ID).

## Security considerations

- **SSRF impossible by construction.** Agent can only fire pre-registered targets. Even if the LLM's tool call has a malicious `target` value, the tool rejects unknown names.
- **URL secrets stay in env vars.** Slack Incoming Webhook URLs and PagerDuty routing keys are effectively passwords; treat them like OAuth refresh tokens. K8s Secret + `valueFrom.secretKeyRef` pattern.
- **Auth material doesn't appear in eventlog.** Target name, level, status code — yes. Bearer tokens, basic-auth creds, response bodies — no. Operators debugging via eventlog get enough to reason about behavior without exposing credentials.
- **Rate limit as a DoS-mitigation.** Not a security feature per se, but prevents a runaway agent from hammering a downstream service.
- **HTTP client timeout.** 10s hard timeout means a hostile webhook target can't hang an alert-issuing session.
- **No response-body echoing.** Response body from the webhook isn't returned to the agent; the LLM can't use the body as a smuggle channel back to itself.

## Out of scope (deferred to v2.8+)

- **Multi-target fan-out** (OQ #4).
- **Operator-defined templates** via text/template (OQ #5).
- **Configurable retry-on-failure** (OQ #6).
- **Persistent rate-limit state** (OQ #7).
- **Response body capture** for thread-reply-style workflows (OQ #8).
- **SMTP target kind** (email alerts) — different transport, different auth model, deserves its own design cycle.
- **Non-webhook alert kinds** — anything that isn't fire-and-forget HTTP POST.
- **Alert acknowledgment** — no callback from PagerDuty ack, Slack reaction, etc.

## Dependencies and related work

- **[#186](https://github.com/go-steer/core-agent/issues/186) v2.6 k8s-event agent** — the immediate consumer. Once alert tool ships, the triage recipe gets updated with sample targets + the router calls `alert()` instead of the current eventlog-only pattern. **Done in v2.9** (see above).
- **[#190](https://github.com/go-steer/core-agent/issues/190) MCP Streamable HTTP + OAuth 2.0** — complementary, not dependent. Different escalation shapes: alert-tool is fire-and-forget, MCP-OAuth-Slack is two-way conversational. MCP-OAuth shipped in v2.7; the alert tool did not — it landed in v2.9.
- **Slack Incoming Webhooks docs** — https://api.slack.com/messaging/webhooks
- **PagerDuty Events API v2** — https://developer.pagerduty.com/docs/events-api-v2/trigger-events/
- **Discord webhook shape** — https://discord.com/developers/docs/resources/webhook

## When this lands

- Phase 1 (config + tool + generic template): ~2 days
- Phase 2 (service templates): ~2 days
- Phase 3 (docs + recipe + CHANGELOG): ~1 day

~5 days of focused work across 3 PRs. Smaller than MCP-OAuth. Ships in parallel; no dependency ordering with [#190](https://github.com/go-steer/core-agent/issues/190).

**How it actually landed:** phases 1 and 3 shipped together in v2.9 (#607, then the recipe wiring in #644). Phase 2 — the service templates — has not been written; it is the only part of this doc still outstanding.
