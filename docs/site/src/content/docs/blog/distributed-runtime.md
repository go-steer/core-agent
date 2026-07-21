---
title: Distributed runtime and fleet observability
description: Multi-daemon changes what "the agent" means. What breaks at fleet scale — session identity, cost isolation, event dedup, tracing sampling — and the OTel + Prometheus pipeline that made the fleet legible.
template: doc
tableOfContents: true
sidebar:
  order: 6
---

For most of core-agent's life there was one daemon per operator. It ran locally, or in a container, or on a shared VM; either way, "the agent" was a specific process, addressable at a specific URL, holding a specific session's state in memory.

That model started to bend in v2.5, cracked in v2.6, and got explicitly redesigned in v2.7. The trigger was straightforward: operators wanted **more than one daemon** — a triage agent per cluster, a `k8s-event-watcher` sidecar per namespace, a locally-attached "chat" daemon *and* a remote autonomous daemon in the same TUI. Suddenly "the agent" wasn't a process anymore. It was a *logical identity* that had to survive process restarts, be aggregated across replicas, be observable as a fleet rather than as a machine, and — the hard one — be *reasoned about* by an operator who couldn't hold the whole picture in their head.

This post is about what broke, what we built, and how the OTel + Prometheus pipeline turned into the thing that made the fleet legible.

## What multi-daemon breaks

Single-daemon assumptions that fell over as soon as there were two:

**1. Session identity.** In single-daemon mode, a session ID is a UUID inside a process; nobody else knows about it. In multi-daemon, an operator with a TUI attached to daemon A wants to `/switch` to a session on daemon B — same TUI window, different daemon. That works only if session IDs are unambiguous across the fleet (they are — UUIDs), *and* if the TUI has a way to enumerate sessions across daemons (it didn't, until [#241](https://github.com/go-steer/core-agent/pull/241) shipped `SessionSwitcher` and [#253](https://github.com/go-steer/core-agent/pull/253) added `/switch` + `/attach` for multi-daemon).

**2. Cost accounting.** Every daemon has a `usage.Tracker`. In single-daemon mode, "session cost" and "process cost" are the same number and nobody cares. In multi-daemon mode, running more than one session per daemon (`multi_session.enabled=true`) meant the tracker started returning the *union* across every session, so `/stats` for session A showed the tokens session B had spent too. This is what [#275](https://github.com/go-steer/core-agent/pull/275) fixed — one `usage.Tracker` per session, in the session factory — and what [#336](https://github.com/go-steer/core-agent/pull/336) hardened, by having the tracker rebuild from the persisted eventlog on resume so daemon eviction didn't zero the counters.

**3. Event dedup.** [`k8s-event-watcher`](https://github.com/go-steer/core-agent/tree/main/cmd/k8s-event-watcher) watches Kubernetes events via a shared informer and injects them into the agent. Informers re-list on reconnect, which means the same event can be delivered twice — once during steady state, once during recovery. Fleet-wise, this is worse: two watchers on the same namespace get the same event twice each, four times total. [#240](https://github.com/go-steer/core-agent/pull/240) dedup by `Event.LastTimestamp`; [#236](https://github.com/go-steer/core-agent/pull/236) canonicalize the reason string so family variants collapse. Without both, a busy cluster produced enough duplicate injections to drive false-alarm alerts through the roof.

**4. Tracing sampling.** The remote TUI polls the daemon's read endpoints every ~1–2 seconds to refresh its status bar. When OTel HTTP instrumentation went in, *every one of those polls* generated a span. Multiply by (N poll paths) × (M TUI sessions) × (D daemons) and Cloud Trace's ingest quota was gone before the first turn ran. [#335](https://github.com/go-steer/core-agent/pull/335) added `otelhttp.WithFilter` to skip the eight known hydration-read paths; [#340](https://github.com/go-steer/core-agent/pull/340) caught the two the earlier filter missed (session enumeration, peer enumeration).

**5. Agent identity.** Standard OTel resource attributes — `service.name`, `service.instance.id`, `service.namespace` — cover "which process" for free. But `service.instance.id` churns every time a Pod restarts. So an operator asking "how much has *the triage agent* spent this week?" gets a graph fractured across a dozen instance IDs, each representing a different Pod restart of the same logical agent.

## What "the agent" means, in the multi-daemon world

The v2 answer, refined over several PRs and one design doc ([`docs/metrics-design.md`](https://github.com/go-steer/core-agent/blob/main/docs/metrics-design.md)), is that "the agent" is not the process. It's whichever level of the hierarchy the operator's question is at:

- **The process.** `service.instance.id` — churns on Pod restart, but useful when you're chasing a specific replica.
- **The logical agent.** `core_agent.peer.id` — when the daemon is registered against a [peer-registration hub](https://github.com/go-steer/core-agent/blob/main/docs/peer-registration-design.md), its `PeerRegistry.RegistrationID` is stamped as a resource attribute. Survives Pod churn. This is what the operator means by "the triage agent."
- **The session.** `session.id` — a UUID identifying one conversation. Present as a span attribute on every trace; present as a metric label only when the operator has opted in (session labels can explode cardinality on a busy daemon).
- **The turn.** No stable ID beyond a span ID, but attributable through `session.id` + trace timestamp.

Every dashboard question is a filter over the right level. "Which agent is misbehaving?" is `core_agent.peer.id` or `service.instance.id`. "Which session hit its cost ceiling?" is `session.id` (small fleets) or a trace search (large ones). "How much has our fleet spent?" is a sum across everything. "How much has *this* agent spent, across all its Pod restarts?" is a sum grouped by `core_agent.peer.id`.

The interesting design point is that **`core_agent.peer.id` isn't always populated.** Absent a peer-registration hub — small deployments that don't need cross-Pod identity — the attribute isn't set, and per-agent queries fall back to `service.instance.id`. This kept the fleet story additive: you don't pay for peer-registration complexity unless you want per-agent aggregation across restarts.

## The observability engineering choice

The load-bearing engineering decision was in the metrics pipeline. Two constraints pulled in different directions:

- **The trace pipeline already used OpenTelemetry.** ADK-go handles it: W3C `traceparent` propagates across the daemon, `k8s-event-watcher`, and every MCP tool call; spans flow through OTLP; on GKE, an `Instrumentation` CR auto-injects the standard OTel env vars. This all worked in v2.7. Metrics, in principle, should ride the same substrate.
- **The `k8s-event-watcher` sidecar already shipped six Prometheus counters.** Operators had them scraped, alerted on, and rolled up into dashboards. Deprecating `/metrics` in favor of an OTLP-only pipeline would have broken every one of those consumers.

The naive answers both fail:

- "Use OTLP for everything and let operators run an OpenTelemetry Collector to expose Prometheus." Real, but a support ticket per operator who doesn't want to run a Collector.
- "Ship two independent metric registries." Real, but every counter needs to be defined twice, in two libraries, with inevitable drift.

The answer that [`metrics-design.md`](https://github.com/go-steer/core-agent/blob/main/docs/metrics-design.md) landed on, and that [PR #345](https://github.com/go-steer/core-agent/pull/345) started shipping: **one OTel MeterProvider, two readers.**

- OTel Go's MeterProvider takes an OTLP exporter (push) *and* a Prometheus reader (pull) on the same provider. Instruments defined once against a `metric.Meter` land in both wire formats simultaneously.
- The Prometheus reader bridges to a `promhttp.Handler`. Served on `GET /metrics` from the attach listener when attach is configured, or on a standalone `--metrics-addr` listener when it isn't.
- `k8s-event-watcher`'s existing Prometheus counters migrate onto the same substrate — same `/metrics` output shape, backward-compatible for existing scrapes.

The operator-visible result: **`otel.metrics_exporter` = `otlp` / `prometheus` / `both` / `none`.** One config field, four modes, all served from the same instrument definitions. `OTEL_METRICS_EXPORTER` env var overrides it, matching the shape of the existing `OTEL_TRACES_EXPORTER` (which we shipped in v2.7 for OTLP exporter selection).

Two smaller decisions that mattered more than they looked:

- **Off by default.** Fresh invocations still make zero outbound network calls, and don't open a metrics port. Consumers opt in. This preserves the "core-agent runs on a laptop with no config" property — if a user just wants to try the CLI, they get zero telemetry, no ports, no dependencies.
- **v1 exposes only *async observable* instruments driven by the existing in-process counters.** No call-site changes; a single `RegisterCallback` per subsystem reads live values from the existing `usage.Tracker`, `PeerRegistry`, `MCP.Server.Status`, etc. Zero risk of double-counting or drift because the tracker is still the source of truth. Sync instruments at call sites (per-tool histograms, retry counters) are v2, when a real consumer asks for them.

The GenAI Semantic Conventions from the OTel SIG were the third quiet win: token usage and operation duration have stabilized names (`gen_ai.client.token.usage`, `gen_ai.client.operation.duration`). Adopting those means cloud-vendor dashboards (Google Cloud Monitoring, Datadog, Honeycomb) that already render GenAI panels light up for the daemon without a schema mapping. Custom metrics use `core_agent.*` so they stay distinguishable from any other producer.

## What we actually learned about fleets

Three lessons that are cheap to say and expensive to internalize:

**1. Cost is a metric, not a log line.** Per-turn cost had lived as a stderr message and a `/stats` field for months. Making it a first-class metric (`core_agent.session.cost_usd` with `model` as an attribute) is what turned "why is our bill spiking?" from a triage question into a dashboard. It costs almost nothing to expose — we were computing the number already — and it's the single most-asked question when a fleet gets big enough for anyone to notice the bill.

**2. Every polling loop needs a filter.** The remote TUI's ~1-second status refresh, harmless as one request, is a traffic pattern when it fans out across a fleet. Same for MCP heartbeat requests, same for `k8s-event-watcher` reconnect storms, same for `/whoami` calls from a dashboard doing session enumeration. We ended up shipping tracing filters in three separate PRs before we had all of them ([#335](https://github.com/go-steer/core-agent/pull/335), [#340](https://github.com/go-steer/core-agent/pull/340), a follow-up baked into [#345](https://github.com/go-steer/core-agent/pull/345)'s design). The pattern the third time we hit it: **if it fires on a poll cadence, list it explicitly and skip it — don't sample it.** Sampling produces confusing partial pictures; explicit skip is legible.

**3. Silent drops are the worst failure mode.** OTel exporter failures — the SDK's default noop diag/error handlers dropping them silently — cost us more than any wire bug in v2.7. Cloud Trace was rejecting every span for [~1 batch's worth of debugging](https://github.com/go-steer/core-agent/pull/334) before we found the missing `gcp.project_id` resource attribute, and there was no error surface. `pkg/telemetry.Setup` now installs `otel.SetLogger` (stderr, `otel-diag:` prefix) and `otel.SetErrorHandler` (`otel-export:` prefix), gated by `OTEL_LOG_LEVEL`. Load-bearing for any silent-drop debug. The general principle: **any pipeline whose value is "the data arrives somewhere else" needs a first-class surface for reporting when the arrival fails.** Otherwise the pipeline can be broken for weeks and nobody knows.

## What we'd do differently

Two things:

1. **Design for identity before you have three of anything.** `core_agent.peer.id` shipped after we already had multi-daemon deployments in the wild. Adding it retroactively meant a bunch of already-collected metrics couldn't be aggregated the way operators wanted. If you know at design time that a runtime is intended to be replicable, spend an afternoon on a `peer.id`-shaped attribute *before* the first fleet exists. It costs nothing when the fleet is one and pays back everything when it's a dozen.
2. **Wire the metrics pipeline the moment you have anything worth measuring.** We ran with "we count things internally but don't expose them" for months. Every internal counter that never became a metric was a future dashboard that operators didn't get to build. The heuristic: **if a subsystem has a counter, it should be an OTel observer.** Wiring one is cheap; the payoff is that operators can graph the counter without any code change from us.

The through-line: **multi-daemon isn't a scale problem. It's an identity problem.** Once "the agent" stops being one process, every metric, log line, and trace attribute needs to answer "which of us?" — and the answers vary by question. A process, a peer, a session, a turn. The engineering job is making it easy for the operator to pivot between those levels without having to know which one their question lives at.

That's what the OTel + Prometheus pipeline is for. Not "observability" as an abstract virtue. A specific set of pivots, on the specific fleet, that the operator will ask for the first time something goes wrong.
