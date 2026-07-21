# Library friction log

Living record of the friction we've hit integrating third-party libraries into core-agent — the actual bug, the workaround we shipped, and what we'd like to see upstream. Companion to [agent-runtime-go-friction-log.md](agent-runtime-go-friction-log.md), which covers the specific "why can't we deploy Go agents to Agent Runtime" story.

Compiled 2026-07-21 by fanning research agents across ADK Go, the Charm ecosystem, OpenTelemetry Go, the Gemini / Vertex SDKs, and every second-tier direct dep we lean on. Every finding cites a commit, PR, file:line, or upstream tracker item so it can be verified.

## How to read this

Each finding is:

- **Category** — `bug`, `missing-feature`, `api-friction`, `docs-gap`, `performance`, `packaging`, `behavior-surprise`.
- **Severity** — `high` = shipped-bug or material engineering cost; `medium` = recurring papercut we've routed around; `low` = documented tax we've accepted.
- **Issue** — what the friction is, at API-surface granularity.
- **Impact** — what it cost us (bugs shipped, hours spent, features deferred).
- **Workaround** — the specific code that routes around it, with file:line.
- **Recommendation** — what we'd ask upstream, or what we plan to do next.
- **Evidence** — commit hashes / PR numbers / file:line / doc paths so future maintainers can verify without re-running the sweep.

Findings within each section are ranked by severity → impact. Where friction is really one root cause showing up in multiple libraries (e.g. ADK's telemetry wrapper misconfiguring OTel), the canonical entry lives with the root cause and other sections cross-reference.

## Cross-cutting themes

Three patterns recur:

1. **Silent defaults.** OTel Go's diagnostic + error handlers are noop; ADK's `telemetry.New` builds providers but doesn't install them as globals; genai returns "successful" responses with no candidates, no error, and non-zero token charges. Every one produced a live-production failure before we grew defensive plumbing.
2. **Internal-only extension seams.** ADK's `RequestProcessor` and `PackTool` live in `internal/`; MCP SDK's `runnable` interface is unexported; each tool wrapper (namespacing, gating, digesting) re-implements the internal contract, one API break away from silently bypassing itself.
3. **API shape driven by first-party use case.** ADK Go ships no Anthropic backend, no MeterProvider, and no Agent Runtime deployment story; genai backend flags are asymmetric between Vertex and direct API (`IncludeServerSideToolInvocations` is required on one, rejected on the other); `CachedContent` and `Tools` coexist in the type system but 400 at the wire on Vertex. Anywhere the first-party path is Gemini + Python + Cloud Run, the Go + non-Google-model + non-Cloud-Run path pays for it.

## Table of contents

- [ADK Go (`google.golang.org/adk`)](#adk-go-googlegolangorgadk)
- [Bubbletea + the Charm ecosystem](#bubbletea--the-charm-ecosystem)
- [OpenTelemetry Go](#opentelemetry-go)
- [Gemini / Vertex AI (`google.golang.org/genai`)](#gemini--vertex-ai-googlegolangorggenai)
- [Second-tier dependencies](#second-tier-dependencies)

---

## ADK Go (`google.golang.org/adk`)

**Version:** `v1.2.0` (pinned in `go.mod`).

**Overall.** ADK Go carries most of the runtime load in core-agent — 137 files import it, and we lean on `runner`, `agent/llmagent`, `session` (both `InMemoryService` and `session/database`), `model.LLM`, `tool`, `tool/skilltoolset`, and `telemetry` as first-class primitives. The GORM-backed session service saved us ~500 LoC of persistence plumbing, `agent.Event.Branch` gave us a ready-made subagent hierarchy, and the ADK runner already parallel-dispatches multi-call turns via goroutines, letting us batch for free. The friction is that ADK is shaped for Google's Python-first "wrap the model" story rather than for a hosting framework like ours — several must-hit interfaces live in `internal/`, several telemetry defaults are silent no-ops or actively hostile, and several primitives our shape needs (Anthropic backend, MeterProvider, turn cap / middleware, monotonic seq) aren't there.

### 1. [high] `session/database` throws "stale session error" on any concurrent writer

**Category:** api-friction

**Issue.** `session/database` guards every `AppendEvent` with an optimistic-concurrency check against `last_update_time` ([`service.go:378`](https://pkg.go.dev/google.golang.org/adk/session/database)). The runner snapshots the stored session at turn start and holds it through every `AppendEvent` that follows — so any legitimate mid-turn writer (subagent, sub-runner, or a synchronous digest write from inside an MCP tool call) advances the row's timestamp and the runner's next append fails with `stale session error: last update time from request (T0) is older than in database (T1)`, killing the turn. No config knob disables the check or opts into last-writer-wins.

**Impact.** Two production bugs shipped as a direct consequence. Subagents can't write to the parent's session row; we derive a `<parent>:sub:<branch>` row and lose the ability to scope audit queries with `ForSession(parent)`. The MCP-response digest store hits the same race; we now write digests to `<parent>:digest`. Both fixes fragment the audit trail and force callers to run two queries to reconstruct the tree.

**Workaround.** `deriveSubagentSessionID` in `pkg/agent/subagent.go:351`; `storeSubSessionSuffix` in `pkg/digest/store_eventlog.go:129`. Documented in [`docs/eventlog-decisions.md:215`](eventlog-decisions.md#215) and [`docs/m3-followups-plan.md:64`](m3-followups-plan.md#64).

**Recommendation.** File upstream: expose a `WithoutOptimisticConcurrency` option on `session/database.NewSessionService`, OR a per-append `LastUpdateTime` override, OR make the check a soft warning. Also worth: a documented "child" session-row pattern that shares a parent's ACL/tree without tripping the check.

**Evidence.** commit `a2e5afe` (#273 "write digest to derived `<sid>:digest` row to avoid stale-session race"); `pkg/agent/subagent.go:343-356`; `pkg/digest/store_eventlog.go:76-98`; ADK source `session/database/service.go:378`.

### 2. [high] Rehydrated session events lose `TurnComplete` / `Partial` / `FinishReason` on read

**Category:** bug

**Issue.** The event stream returned by `session/database` via `Stream.Since` strips `TurnComplete`, `Partial`, and `FinishReason` — all zero-valued after the round trip. Live events carry these flags; persisted events don't. Any consumer keying off `TurnComplete` (usage rebuild, checkpoint scans, turn-count metrics) silently no-ops when replaying history.

**Impact.** Regression #337: the initial per-session `usage.Tracker` rebuild (#336) used `TurnTap.Commit(ev.TurnComplete)`, so on daemon restart the tracker stayed at zero. Not caught by unit tests because fixtures built events in memory with `TurnComplete` set. Diagnosed live against the GKE demo cluster: `/sessions/<sid>/events` returned 33 events with 15 `UsageMetadata` blocks and zero `TurnComplete=true` events. Fix cost us a full rebuild-pattern change (key off `UsageMetadata` presence) and a new class of test coverage.

**Workaround.** `pkg/usage/rebuild.go:59-109` keys off `UsageMetadata` presence + zero-token filter. Comment block at `pkg/usage/rebuild.go:72-85` documents which flags survive persistence.

**Recommendation.** Fix ADK's `session/database` serialization to round-trip these fields, or document which `Event` fields survive persistence so consumers don't build on flags that will silently disappear. This is a correctness footgun for every downstream consumer.

**Evidence.** commit `6485475` (#337); `pkg/usage/rebuild.go:72-85`.

### 3. [high] `adktelemetry.New` builds providers but doesn't install them as OTel globals — silent no-op by default

**Category:** api-friction

**Issue.** `adktelemetry.New(ctx, opts...)` returns a `Providers` struct but does NOT call `otel.SetTracerProvider` / `otel.SetLoggerProvider`. Every ADK instrumentation site (and every `otel.Tracer(...)` caller) reads the global TracerProvider — so unless the caller explicitly invokes `providers.SetGlobalOtelProviders()`, everything runs against the noop tracer and no spans emit. No error, no log line.

**Impact.** Called out as a load-bearing gotcha in `DESIGN.md`: "you wonder why nothing's being emitted." At least one debugging session per new consumer; the explicit call at `pkg/telemetry/otel.go:181` is what keeps our OTel pipeline alive.

**Workaround.** `pkg/telemetry/otel.go:181` always calls `providers.SetGlobalOtelProviders()`. Warning comment at `pkg/telemetry/otel.go:34-35`.

**Recommendation.** Make `telemetry.New` install globals by default with a `WithoutGlobalInstall` opt-out for library users, or return an error when consumers skip `SetGlobalOtelProviders`. Silent noop is the wrong default for a telemetry constructor.

**Evidence.** `docs/DESIGN.md:560`; `pkg/telemetry/otel.go:34-35`.

### 4. [high] `adktelemetry.WithGcpResourceProject` unconditionally overrides SDK-parsed resource attributes

**Category:** bug

**Issue.** `configureOTelResource` in ADK's `telemetry/setup_otel.go:145` unconditionally stamps `attribute.Key("gcp.project_id").String(cfg.gcpResourceProject)` onto the resource — even when the value is the empty default. OTel's resource merge is "later attributes override earlier," so the empty value clobbers whatever `OTEL_RESOURCE_ATTRIBUTES=gcp.project_id=<x>` supplied. Cloud Trace's OTLP receiver rejects every batch with `InvalidArgument: Resource is missing required attribute gcp.project_id`, dropping ALL spans.

**Impact.** Every span from the GKE demo silently dropped by the managed collector until PR #334 landed. Diagnosis required reading ADK source; the OTel SDK gave no signal that the env var had been overwritten.

**Workaround.** `pkg/telemetry/otel.go:144-146` reads `GOOGLE_CLOUD_PROJECT` and passes it to `adktelemetry.WithGcpResourceProject`, seeding `cfg.gcpResourceProject` before ADK's clobbering merge runs.

**Recommendation.** Only apply the resource attribute when `cfg.gcpResourceProject` is non-empty. Trivial upstream fix. Better still: read `GOOGLE_CLOUD_PROJECT` from the env when the option isn't supplied.

**Evidence.** commit `6487614` (#334); `pkg/telemetry/otel.go:131-146`.

### 5. [high] `adktelemetry` auto-appends its own `BatchSpanProcessor` — combining with `WithSpanProcessors` double-exports every span

**Category:** api-friction

**Issue.** `configureExporters` in ADK's `telemetry/setup_otel.go:185-194` auto-appends a BatchSpanProcessor whenever `OTEL_EXPORTER_OTLP_ENDPOINT` is set. If the caller also uses `adktelemetry.WithSpanProcessors(...)` (documented as the way to add an explicit exporter), the TracerProvider gets two BatchSpanProcessors and every span exports twice — with identical `span_id`, timestamp, and duration.

**Impact.** PR #333 added an explicit exporter for visibility; PR #335 had to rip it back out three days later after every Cloud Trace span appeared duplicated. Two round trips of production churn to converge on "trust ADK's implicit wiring, never combine."

**Workaround.** `pkg/telemetry/otel.go:156-175` relies on ADK's implicit OTLP-exporter wiring and only logs the resolved endpoint for operator visibility. Warning comment at `pkg/telemetry/otel.go:158-162`.

**Recommendation.** Deduplicate SpanProcessors inside `telemetry.New`, or explicitly document in `WithSpanProcessors` godoc that it stacks on top of implicit-from-env wiring.

**Evidence.** commit `3670435` (#335); `pkg/telemetry/otel.go:156-165`.

### 6. [high] No MeterProvider in ADK — TODO(#479) forces every consumer to build their own metrics init

**Category:** missing-feature

**Issue.** `google.golang.org/adk` (v1.2.0, still in v1.5.0 / v2.0.0) contains a literal `// TODO(#479) init meter provider` in `telemetry/setup_otel.go:91`. `Providers` exposes only `TracerProvider` and `LoggerProvider`. Anything wired via `otelhttp` handlers records instruments against the global noop MeterProvider and vanishes.

**Impact.** core-agent had zero OTel metrics through v2.7.0. The full pipeline (`pkg/telemetry/metrics.go`, ~250 LoC + tests + design doc) had to be built independently, running alongside — not through — the ADK telemetry init path. We couldn't reuse ADK's exporter selection or resource attribution, so `buildResource` re-stamps `gcp.project_id` on the metrics side too.

**Workaround.** `pkg/telemetry/metrics.go` — standalone `SetupMetrics` with its own resource builder, OTLP+Prometheus reader wiring, and shutdown fan-out. Documented in `docs/metrics-design.md:80-85`.

**Recommendation.** Close ADK #479. Google's own Agent-ADK observability doc says "Metric data isn't collected" — that's sticky because the primitive isn't there.

**Evidence.** [ADK #479](https://github.com/google/adk-go/issues/479) (referenced as `TODO(#479)` in upstream source at `telemetry/setup_otel.go:91`); `cmd/core-agent/main.go:523`; `pkg/telemetry/metrics.go:19-21`; `docs/metrics-design.md:31-38`.

### 7. [high] `toolinternal.RequestProcessor` + `toolutils.PackTool` are in `internal/` — every tool wrapper must re-implement them

**Category:** api-friction

**Issue.** ADK's runner preprocess pass (`internal/llminternal/base_flow.go:281-283`) requires every tool in `f.Tools` to implement `toolinternal.RequestProcessor`, failing with `tool %q does not implement RequestProcessor()` otherwise. The canonical helper `toolutils.PackTool` (registers the tool + declaration on `LLMRequest`) also lives in `internal/`. Any wrapper that renames, gates, or decorates a tool must re-implement both — because if the wrapper delegates to `inner.ProcessRequest`, ADK's callback dispatch hits the inner tool and bypasses the wrapper's Run.

**Impact.** Three separate wrapper types in `pkg/` needed independent `ProcessRequest` implementations that all call the same locally-copied `PackTool`: (1) `renamedTool` for MCP namespace prefixing, (2) `gatedTool` for permission checks, (3) `digestingTool` for MCP response digesting. Each is one uncaught API break away from silently bypassing gating / renaming / digesting.

**Workaround.** `pkg/tools/pack.go` re-implements `toolutils.PackTool` verbatim. `pkg/mcp/namespace.go:136`, `pkg/tools/gate.go:103`, `pkg/mcp/digest_wrap.go:403` each define `ProcessRequest` that packs the wrapper (not the inner).

**Recommendation.** Promote `toolinternal.RequestProcessor` and `toolutils.PackTool` to a public `tool` subpackage (`tool/registration` or similar). Tool wrappers are a load-bearing pattern in real agents; the interface required to satisfy the runner MUST be public.

**Evidence.** `pkg/tools/pack.go:24-48`; `pkg/mcp/namespace.go:124-131`; `pkg/tools/gate.go:97-105`; `pkg/mcp/digest_wrap.go:399-405`; ADK source `internal/llminternal/base_flow.go:283`.

### 8. [high] `skilltoolset` uses strict YAML unmarshaling — every unrecognized frontmatter field kills the load

**Category:** bug

**Issue.** `google.golang.org/adk/tool/skilltoolset/skill.Frontmatter` decodes SKILL.md YAML with strict semantics: unknown keys (Claude Skills 2.0's `user-invocable`, `disable-model-invocation`, `references`) raise `yaml: unmarshal errors: field user-invocable not found in type skill.Frontmatter`, and any map/sequence value for `compatibility` (the community spec's `{go: '>=1.20', ...}` shape) raises `cannot unmarshal !!map into string`. A single non-conforming skill drops the entire skill list from the loader.

**Impact.** core-agent could not load real-world Claude-2.0-shaped skill bundles until PR #46. Cost: an entire sanitizing FS layer (~160 LoC) that intercepts every `SKILL.md` open, parses the frontmatter loosely, filters to fields ADK understands, and stringifies complex `compatibility` values.

**Workaround.** `pkg/skills/load.go:186-320` — `sanitizingFS` wrapper + `sanitizeFrontmatter`. Full design in [`docs/adk-skills-issue.md`](adk-skills-issue.md) (proposal to upstream a relaxed Unmarshal with a concrete patch).

**Recommendation.** Switch `skilltoolset` to lenient unmarshaling (don't set `KnownFields(true)`); give `compatibility` a custom `UnmarshalYAML` that accepts both scalar and map.

**Evidence.** commit `e88f9ed` (#46); `docs/adk-skills-issue.md:17-65`; `pkg/skills/load.go:260-320`.

### 9. [high] ADK streaming aggregator raises "empty response" as fatal for benign Vertex heartbeat chunks

**Category:** bug

**Issue.** Vertex's streaming search-grounding path emits interstitial SSE chunks that carry only `UsageMetadata` + `ResponseID` (no `Candidates[]`). ADK's stream aggregator in `internal/llminternal` treats any empty-candidates chunk as a fatal error with the literal message text "empty response." The error isn't exported, so downstream code can't type-check it. Measured 30–60% failure rate on Vertex grounded responses before the fix.

**Impact.** Grounded queries against Vertex silently truncated. Diagnosis required reading ADK source. In parallel we hit a related class where Vertex returns `Content:{role:model, parts:nil}` with empty FinishReason/ErrorCode/UsageMetadata — ADK forwarded as-is; the agent loop went idle with no diagnostic surface (v2.6 GKE-triage session sat idle 5+ min mid-triage).

**Workaround.** `pkg/models/gemini/builtins.go` carries a `tolerateEmptyChunks` flag on `builtinsLLM` that string-matches the literal error text (`adkEmptyResponseError = "empty response"` at `builtins.go:723`) and swallows heartbeats on Vertex. Wrapped further by `wrapEmptyTailDetection` (#220 / commit `8cc0ed4`) which synthesizes an explicit `ErrEmptyResponse` when the entire iteration produces no usable content.

**Recommendation.** Distinguish "structural empty" (no `Candidates` → likely a heartbeat) from "terminal empty" (final chunk with no content) in the ADK aggregator, or expose a typed error variant so downstream code can `errors.Is`-check instead of string-match. Also: don't treat a mid-stream empty as fatal at all — let the caller decide.

**Evidence.** commit `8cc0ed4` (#220); `pkg/models/gemini/builtins.go:217-223`, `:719-723`; `docs/site/src/content/docs/concepts/providers.md:87`.

### 10. [medium] `runner.Runner.Run` exposes no turn cap and no middleware seam

**Category:** missing-feature

**Issue.** `runner.Runner.Run` returns an `iter.Seq2[*session.Event, error]` and drives however many model↔tool round-trips ADK's flow decides on, with no configurable turn cap and no hook for retry / rate-limit / cost-ceiling / "before tool call" middleware. Consumers wanting any of these wrap the iterator manually and count `TurnComplete` events themselves.

**Impact.** `pkg/agent/subtask.go:298-410` re-implements the entire turn-cap loop with a hand-rolled `TurnTap` + `TurnComplete` counter. Same applies to any consumer wanting pause/resume, cost ceiling enforcement, retry policies, or telemetry middleware — each projects onto the raw iterator. Called out explicitly at `docs/DESIGN.md:607-611` and `docs/subagents-plan.md:524` ("Subagent token / cost accounting roll-up ... would need a hook in ADK's `agenttool` (which would need an upstream change) or our own re-implementation").

**Workaround.** `pkg/agent/subtask.go:298-410` + `pkg/agent/agent.go:1749-1762` (`wrapWithCleanup`).

**Recommendation.** Add `runner.Config.MaxTurns` and a small `BeforeTurn` / `AfterTurn` / `BeforeToolCall` / `AfterToolCall` middleware chain. Every real production agent needs at least a turn cap and cost middleware; forcing every consumer to re-implement fragments the ecosystem.

**Evidence.** `pkg/agent/subtask.go:299`; `docs/DESIGN.md:607-611`; `docs/subagents-plan.md:524`.

### 11. [medium] `session.Service` type-asserts `session.Session` to concrete implementation types (compactor slice-view rejection)

**Category:** api-friction

**Issue.** ADK's `session.Service` implementations (chiefly `InMemoryService`) type-assert the `session.Session` interface to their own private concrete types inside `AppendEvent`. A wrapper that returns a `session.Session` implementation from its `Get()` — e.g. our `slicedSession` wrapper that exposes a subset of events for compaction — is rejected with `unexpected session type` when the runner's next `AppendEvent` tries to write. No exported "generic session" type or interface method lets callers substitute their own `Session` while delegating writes.

**Impact.** Our `compactingService` (`pkg/agent/compactor.go:655-666`) unwraps `slicedSession → inner` before every `AppendEvent` — forgetting the unwrap raises a runtime error deep in the runner. Every future "view over session" pattern (masked events, filtered events, redacted events) hits the same trap.

**Workaround.** `pkg/agent/compactor.go:655-666`.

**Recommendation.** `session.Service.AppendEvent` should key off `(AppName, UserID, SessionID)` instead of asserting on the session pointer's dynamic type. Or add a public `session.Wrapper` interface with `Unwrap() Session` that the service walks before the type check.

**Evidence.** `pkg/agent/compactor.go:655-666`; ADK source `session/inmemory.go:210`.

### 12. [medium] `session/database` has no eventual-atomic append, no exported `*gorm.DB`, and no monotonic seq

**Category:** api-friction

**Issue.** `session/database` doesn't expose the underlying `*gorm.DB`, so callers can't join an overlay table into ADK's transaction. Its event IDs are timestamp-based strings with no monotonic cursor — so "give me every event since seq N" is not expressible against ADK's schema. It also doesn't expose `Delete` on the `Handle`.

**Impact.** `pkg/eventlog` keeps a parallel `agent_eventlog` overlay table. Without a shared `*gorm.DB` we open our own `gorm.Dialector` against the same DSN, run our own AutoMigrate, and accept eventual consistency between the two tables — a unique index on `event_id` + retry-on-overlay-failure covers realistic failure modes but atomic guarantees are off the table. Documented at `docs/DESIGN.md:388-390`. Also blocks `pkg/attach/handlers_delete_session.go` from cleaning underlying event rows on session delete.

**Workaround.** `pkg/eventlog/sql.go:99-150` opens a parallel gorm connection to the same DSN. `pkg/eventlog/service.go` wraps `AppendEvent` with a two-write pattern (ADK first, then overlay).

**Recommendation.** Expose `DB() *gorm.DB` on the session service (or a `WithinTx(fn)` method) so downstream overlays can join ADK's transaction. Add a monotonic seq column, or expose the equivalent cursor. Add `Delete` lifecycle to the durable session service.

**Evidence.** `pkg/eventlog/sql.go:99-150`, `:109-112`; `docs/DESIGN.md:388-390`; `docs/eventlog-plan.md:71`.

### 13. [medium] ADK's `model/gemini` converter drops `URLContextMetadata` below its own boundary

**Category:** missing-feature

**Issue.** ADK's gemini model wrapper lifts `GroundingMetadata` onto `model.LLMResponse` but drops `URLContextMetadata` at conversion. Projecting URLContext evidence into the audit log — straightforward for GroundingMetadata — is impossible from above the ADK boundary because the raw genai response is already destructured by the time our code sees it.

**Impact.** `pkg/models/gemini/projection.go` can project Google Search grounding evidence into synthetic events but cannot do the same for URLContext. Any consumer that wants a full audit of what web content the model actually retrieved has to intercept below the ADK boundary — either fork the model package or talk to genai directly (defeating the purpose of the ADK abstraction).

**Workaround.** Deferred until a consumer asks. Comment at `pkg/models/gemini/projection.go:45-49`.

**Recommendation.** Lift `URLContextMetadata` (and any other metadata Gemini returns) onto `model.LLMResponse` — the ADK converter is the wrong place to filter based on what's interesting today.

**Evidence.** `pkg/models/gemini/projection.go:45-49`; `docs/site/src/content/docs/concepts/providers.md:121`.

### 14. [medium] ADK Go ships no Anthropic backend — first-class Claude support is entirely our code

**Category:** missing-feature

**Issue.** The `model.LLM` interface is genai-shaped (Gemini-first). ADK Go's bundled model backends are Gemini and Apigee only. No Anthropic or Vertex-Anthropic backend — any Go agent that wants Claude writes and maintains its own adapter between genai's `Content`/`Part`/`Tool`/`FunctionCall` types and Anthropic's Messages API shape.

**Impact.** `pkg/models/anthropic/` (`llm.go` + `anthropic.go` + `convert.go` + `stream.go`, ~600 LoC + tests) exists entirely because of this gap. Called out as the single largest area of first-party engineering in `docs/DESIGN.md:17`. Every genai schema change (thinking config, structured output, vision parts) is an integration cost we bear, not ADK.

**Workaround.** Full Anthropic adapter under `pkg/models/anthropic/`; the Vertex variant (`pkg/models/anthropic-vertex/`) adds ADC + region auth on top of the same conversion layer.

**Recommendation.** Ship an official ADK-Go Anthropic backend — even a minimal one — so downstream users don't each reinvent the genai↔Anthropic conversion. Prior art in Python ADK; the Go SDK community currently forks the same handful of adapters.

**Evidence.** `docs/DESIGN.md:17`, `:122-125`; `pkg/models/anthropic/anthropic.go:17-22`.

### 15. [medium] Agent Runtime deployment path is Python-only; ADK-Go punts to Cloud Run

**Category:** packaging

**Issue.** Google's official `github.com/google/adk-go` names Cloud Run as its deployment target and doesn't integrate with Vertex Agent Engine / Agent Runtime. Agent Runtime's BYOC / "custom agent" paths are cloudpickle-based Python — deployment is architecturally Python-only. No open-source "Agent Engine Functions Framework" to port to Go.

**Impact.** We can't ship core-agent onto Agent Runtime, only Cloud Run — losing managed Sessions / Memory Bank / Code Execution / Agent Identity / Agent Gateway unless we call them as clients. Deployment story splits into two tiers and complicates the enterprise Go/Rust/Node pitch. Full account in [`docs/agent-runtime-go-friction-log.md`](agent-runtime-go-friction-log.md) (466 lines).

**Workaround.** Cloud Run deploy recipe in `examples/cloud-run-deploy/` (commit `8d6d3f2`, PR #113); GKE recipe in `examples/gke-deploy/`; Agent Runtime services (Sessions, Memory Bank) consumed via `cloud.google.com/go/agentplatform` as client-side APIs when needed.

**Recommendation.** Publish the Agent Runtime BYOC HTTP wire contract, or add Agent Runtime to ADK-Go's deployment story with a reference container. Alternatively, document Cloud Run + Sessions/Memory-Bank-as-clients as the first-class Go story in ADK-Go's README.

**Evidence.** commit `175b131` (#114); `docs/agent-runtime-go-friction-log.md:12-20`, `:200-211`.

---

## Bubbletea + the Charm ecosystem

**Versions.** Direct: `charmbracelet/bubbletea v1.3.10`, `charmbracelet/lipgloss v1.1.1-0.20250404203927-76690c660834`. Indirect via [`core-tui`](https://github.com/go-steer/core-tui): `charm.land/bubbletea/v2 v2.0.6`, `bubbles/v2 v2.1.0`, `glamour/v2 v2.0.0`, `lipgloss/v2 v2.0.3`, `huh/v2 v2.0.3`, plus `charmbracelet/x/{ansi, cellbuf, term, termios, windows}`.

**Overall.** The Charm stack has been load-bearing for operator UX since v1.8.0 (PR #16) — the visual language (chat scrollback, status line, glamour-rendered markdown, slash palette, thinking indicator) all works and is why the in-process TUI is the default TTY surface after the 2026-05-23 embedded-TUI thesis flip. Direct use in this repo is deliberately thin (~1,489 LoC in `cmd/core-agent-tui/`, of which only `picker.go` + `styles.go` + a standalone-picker wrapper actually import bubbletea/lipgloss); the richer surface was extracted into the sibling `github.com/go-steer/core-tui` library after the main-binary consumer tree had ballooned to ~2,844 LoC — PR #82 (`6f6f99b`) deleted ~2,300 LoC of parallel bubble-tea code in favor of `coretui.Run(adapter)`. Direct friction attributable to the Charm libraries (not our wrapper) clusters around three shapes: (1) the alt-screen owns stdio, so any stray write corrupts the render; (2) glamour's `WithAutoStyle` queries the terminal after bubbletea has claimed stdin; (3) bubbletea's Update loop is single-goroutine, so any synchronous slash/tool handler freezes the UI.

### 1. [high] `glamour.WithAutoStyle` OSC-11 background query leaks into bubbletea's raw-mode textarea

**Category:** bug

**Issue.** Glamour's auto-detect theme path sends an OSC 11 escape (`\033]11;?\007`) asking the terminal for its background color. Bubbletea has already put stdin into raw mode by the time glamour is initialized, so the terminal's response (`\033]11;rgb:1818/1818/1818\007`) is captured as input and lands in the operator's textarea. Glamour and bubbletea have no shared coordination point.

**Impact.** Cost a UAT cycle of "why is `]11;rgb:...` appearing in my input box?" Then a `--theme=dark` default flip (still the recommended value in remote-TUI docs). Named themes (gopher, google, ...) now flow through a second knob (`InitialThemeName`) because the primary `ForceTheme` knob is reserved for the OSC-11 escape hatch — a schema split baked in specifically to route around the query.

**Workaround.** Flipped `--theme` default from `auto` to `dark` in `dba0fad` (PR #24). In the in-process TUI, config now has two theme fields: `cfg.UI.Theme='dark'|'light'` maps to `coretui.Options.ForceTheme` (skips OSC-11); named themes map to `InitialThemeName`. See `uiThemeToCoreTui` + `uiInitialThemeName` in `cmd/core-agent/coretui_enabled.go:1352-1386`.

**Recommendation.** `glamour.WithAutoStyle` should be a no-op when stdin isn't a controlling TTY it owns, or should coordinate with bubbletea via a shared "terminal is in raw mode, do not query" signal. Alternatively provide a synchronous "query once before bubbletea claims stdin, cache the result" helper.

**Evidence.** commit `dba0fad` (#24); `cmd/core-agent-tui/main.go:75`; `cmd/core-agent/coretui_enabled.go:1352-1369`; `docs/embedded-tui-design-v2.md:29`; `docs/site/src/content/docs/blog/embedded-tui-flip.md:60`.

### 2. [high] Bubbletea alt-screen owns stdio; any concurrent stderr/stdout write corrupts the rendered chat

**Category:** api-friction

**Issue.** Once bubbletea takes over the terminal (alt-screen + raw mode + cursor control), any other component writing to `os.Stderr` or `os.Stdout` interleaves its bytes with bubbletea's ANSI, producing visibly scrambled output. No framework-provided log sink or side-channel writer — every dependency has to be individually silenced or redirected.

**Impact.** Bit us four times: (a) spawned child agent's `cmd.Stderr = os.Stderr` scrambled the alt-screen (`dba0fad`); (b) the remote permission-prompter bridge got `io.Discard` because "bubble-tea owns the alt-screen and writes to stderr while it's running corrupt the rendered chat" (`cmd/core-agent-tui/main.go:184-188`); (c) `internal/coretuiremote` grew a bespoke `debugf` writer keyed off `CORE_AGENT_TUI_DEBUG` because "The TUI's stderr is unusable while bubble-tea owns the alt-screen" (`internal/coretuiremote/debug.go:29-33`); (d) MCP server startup failures were invisible because they were logged to stderr — plumbed through a bespoke `coretui.Notifier` channel to become visible in-chat (`cmd/core-agent/coretui_enabled.go:238-245`).

**Workaround.** Per-site sinks: `io.Discard` for the prompter bridge; env-gated file logger for coretuiremote; tee to `/tmp/<sock>.log` with an 8 KiB rolling in-memory tail for spawned children; a new `coretui.Notifier` channel to surface MCP startup errors as in-chat rows.

**Recommendation.** Bubbletea should either expose a framework-owned log writer (routed to a program-provided sink) or provide a documented "safe stderr" — buffered until the program tears down. Alternatively, `tea.Program` should intercept writes to the real fd during raw-mode.

**Evidence.** `cmd/core-agent-tui/main.go:184-188`; `internal/coretuiremote/debug.go:29-33`; `cmd/core-agent/coretui_enabled.go:238-245`; commit `dba0fad` (#24).

### 3. [high] `InvokeSlash` runs in bubbletea's single Update goroutine; long-running slash handlers freeze the UI

**Category:** api-friction

**Issue.** Bubbletea's Update loop is single-goroutine; anything that blocks it blocks all rendering + input. The wrapper library (`core-tui`) exposes `SlashProvider` synchronously from that loop, so an LLM-backed slash like `/compact` (which does a full model round-trip) hard-freezes the TUI for its duration. Comment at `cmd/core-agent/coretui_enabled.go:947-952` pins the workaround to `core-tui#10`.

**Impact.** `/compact` and `/btw` surface as "TUI unresponsive" during model calls (tens of seconds on slow models). Bug is inherent to bubbletea's Update-in-one-goroutine model; the wrapper needed to invent an entire async dispatch protocol (`InvokeSlashAsync` with in-chat preamble rows, PR #81) instead of the natural pattern of just kicking off a goroutine.

**Workaround.** Remote-TUI path: `InvokeSlashAsync` (PR #81) dispatches the four async slashes (`/compact`, `/done`, `/btw`, `/subagent`) with an in-chat preamble row. In-process TUI still has the sync freeze on `/compact`.

**Recommendation.** Bubbletea should document + provide a first-class "kick off Cmd, get a completion msg" primitive with a canonical example. `tea.Cmd` exists but the semantics of "do work then send a msg" are underemphasized in the docs; every consumer re-invents the goroutine+`p.Send` pattern.

**Evidence.** `cmd/core-agent/coretui_enabled.go:947-952`; PR #81.

### 4. [medium] Two parallel Charm ecosystems in `go.mod` (`charmbracelet/*` v1 + `charm.land/*/v2`) with no clear migration guidance

**Category:** packaging

**Issue.** `go.mod` carries direct deps on `charmbracelet/bubbletea v1.3.10` and `charmbracelet/lipgloss v1.1.1-pre` alongside indirect deps on `charm.land/bubbletea/v2`, `bubbles/v2`, `glamour/v2`, `lipgloss/v2`, `huh/v2`, plus the newer `charmbracelet/x/*` split-out packages and `charmbracelet/ultraviolet` at an untagged pseudoversion. Two module hosts, two API surfaces, one binary. The remaining direct bubbletea/lipgloss use (session picker) is pinned to v1; the sibling `core-tui` library is on the v2 track.

**Impact.** Every render bug fix needs to be assessed against two API sets. `go list -deps` legitimately hits both trees. Each transitive dep flip (e.g. ultraviolet's untagged pseudoversion) surfaces `GO-` advisories that take extra reasoning to triage. Newcomers looking for API docs have to know which era they're in.

**Workaround.** Kept the direct dep on v1 to avoid rewriting the picker; delegated all richer rendering to core-tui and let core-tui carry the v2 dependencies transitively.

**Recommendation.** Upstream: publish a canonical migration guide (v1 → v2) with an API-diff table and a compat-shim strategy. Downstream: migrate the picker to v2 in one focused PR so we're on one ecosystem.

**Evidence.** `go.mod:7`, `:41-45`, `:61-70`; `cmd/core-agent-tui/styles.go:19-27`.

### 5. [medium] Lipgloss provides no soft-wrap for long strings; overflow is silently truncated with `…`

**Category:** behavior-surprise

**Issue.** core-tui (layered on lipgloss + bubbles' viewport) truncates rows past terminal width with `…` rather than soft-wrapping. Bit us on `/new`'s system-row output on 80-col terminals: the URL an operator needs to copy got cut mid-host. Root problem is that Lipgloss's `Style.Width(w).Render(s)` doesn't offer graceful wrap for mid-string content.

**Impact.** Long output must be pre-formatted by every producer that ever might render narrow. Every fix is a per-string hack, not a policy.

**Workaround.** Insert explicit newlines in producer code. PR #172 (`aa45a50`): `SystemMessage("/new: created <sid>\n<url>")` instead of a single long line. Referenced upstream at [core-tui#49](https://github.com/go-steer/core-tui/issues/49).

**Recommendation.** Upstream lipgloss: add a `Wrap` / `SoftWrap` variant on `Style.Render` that word-wraps rather than truncates. Bubbles' viewport does have wrapping but doesn't compose cleanly with lipgloss-decorated rows.

**Evidence.** PR #172; commit `aa45a50`; [core-tui#49](https://github.com/go-steer/core-tui/issues/49).

### 6. [medium] Bubbletea's mouse capture breaks native terminal click-drag text selection

**Category:** behavior-surprise

**Issue.** Enabling mouse events for wheel scroll (`tea.WithMouseCellMotion`) takes over ALL mouse events, so the terminal no longer sees click-drag and can't perform native text selection. Operators can only select via a modifier bypass (Shift-drag on most terminals) or turn off mouse capture entirely.

**Impact.** A recurring papercut every operator hits on first launch ("I can't copy anything?"). Forces a doc burden (`Shift-drag to select` appears in every operator-facing reference page) and a persistent config surface (`ui.mouse` pointer semantics for nil-vs-false). Discoverability is poor: the toggle is `/mouse` at runtime, but the operator's mental model at that point is "I broke my terminal."

**Workaround.** `UIConfig.Mouse *bool` (nil = default on, explicit false disables), `/mouse` slash toggle. PR #44 (`8106de9`).

**Recommendation.** Bubbletea should support a mouse-events mode that captures only wheel events (SGR 1006 with report-motion off) so click-drag falls through to the terminal. The current all-or-nothing capture is the mismatch.

**Evidence.** `pkg/config/config.go:143-150`; `cmd/core-agent/coretui_enabled.go:1388-1399`; PR #44; commit `8106de9`.

### 7. [medium] Glamour re-renders the full buffer on every partial; live per-token markdown is prohibitively expensive

**Category:** performance

**Issue.** Glamour parses the entire markdown body per `Render()` call — no incremental / streaming render path. A naive "render on every SSE partial" would re-parse hundreds of times per turn and flicker.

**Impact.** Mid-stream messages render as plain monospace; glamour only fires on `TurnComplete`. Operators occasionally get startled when a formatted response "appears" at `TurnComplete` having looked like plain text throughout. Not a bug per se — a fundamental library limitation the design routed around.

**Workaround.** Per-token: plain monospace inside a faint border. On `partial=false`: re-render through glamour + replace the plain version in-place. Policy documented at `docs/attach-tui-design.md:151-158`.

**Recommendation.** Upstream glamour: incremental rendering (given a body + a delta, return the delta's ANSI without re-parsing prior blocks). Non-trivial because markdown parse state isn't purely local (fenced blocks span partials), but is the correct API shape for streaming LLM output.

**Evidence.** `docs/attach-tui-design.md:64-69`, `:151-158`.

### 8. [medium] Bubbletea's per-event rendering is O(N²) on catch-up; long remote-session attach was measurably slow

**Category:** performance

**Issue.** Bubbletea's rendering model paints per Update tick. When a batch of N events arrives in short succession (attach to a long remote session, catch-up replay from `?since=N`), each event triggers its own `refreshViewport`, giving O(N²) total layout work across the batch. No batching primitive for "process this stream of msgs, paint once at the end."

**Impact.** Slow initial attach to long remote sessions was a persistent field complaint before core-tui#67 coalesced the paints. Any bubbletea consumer that drains a stream of messages hits the same shape and has to invent its own coalescer.

**Workaround.** [core-tui#67](https://github.com/go-steer/core-tui/pull/67) — coalesce `refreshViewport` calls per ~1ms window, short-circuit `WindowSizeMsg` when dimensions unchanged. Pure downstream fix; no bubbletea API change.

**Recommendation.** Upstream bubbletea: first-class batch-msg primitive ("drain this channel then render once") so consumers don't invent per-project coalescers. Document the O(N²) hazard for stream-drain Update patterns explicitly.

**Evidence.** `CHANGELOG.md:137`.

### 9. [low] `tea.Program` has no clean "run modal, return a value" pattern

**Category:** api-friction

**Issue.** `tea.Program.Run()` returns the final `Model` (any), not a caller-shaped result. A screen that needs to run to completion and return "the row the operator picked" has to (a) mutate a field on the Model when Enter is pressed, (b) return `tea.Quit`, (c) have the caller type-assert the returned Model and pull the field out.

**Impact.** Every modal-shaped bubbletea screen (session picker, `/btw` overlay, elicitation dialog) re-invents the same pattern. Composition with parent Models is awkward: the picker Model can't just `return picked` — it has to set a field the parent inspects on each Update tick.

**Workaround.** `standalonePicker` at `cmd/core-agent-tui/main.go:262-301` — a 20-line wrapper that translates `pm.selected != nil → tea.Quit` into a returnable value.

**Recommendation.** Upstream: `tea.Program[T]` or a `Run()` variant that returns a typed value produced by a `QuitWithResult(v T)` message. Small API surface, removes a common wrapper pattern across every consumer.

**Evidence.** `cmd/core-agent-tui/main.go:262-301`.

### 10. [low] TUI dep tree drags ~5-8 MB into headless / K8s deployments

**Category:** packaging

**Issue.** The full Charm stack (bubbletea + lipgloss + glamour + chroma + all the `charmbracelet/x/*` subpackages) is a measurable binary-size hit that's dead weight in headless / K8s / CI deployments. Measured at ~8 MB / 14% smaller (60.2 MB → 51.9 MB) for the `-tags no_tui` slim build in PR #29 (`815bdbb`), restated as ~5 MB in `CHANGELOG.md:285`.

**Impact.** Forced core-agent to grow a full slim build variant, a separate GHCR image (`core-agent-slim`), and doc pages explaining which image to pull. Every TUI-facing code addition must preserve the `//go:build !no_tui` / `no_tui` pair.

**Workaround.** `//go:build` tag split at `cmd/core-agent/tui_enabled.go` / `tui_disabled.go`, plus `main.go:1674-1710` for the `didRun=false → REPL fallthrough`. Container release pipeline (PR #108) has three images (default, slim, remote-tui).

**Recommendation.** Upstream: factor out the color/style engine + chroma highlight so consumers who only render simple text UIs don't pull the whole markdown+syntax stack.

**Evidence.** PR #29; commit `815bdbb`; `cmd/core-agent/tui_disabled.go:17-27`; `CHANGELOG.md:285`, `:408`.

---

## OpenTelemetry Go

**Versions.** `go.opentelemetry.io/otel v1.44.0`, `otel/sdk v1.44.0`, `otel/sdk/metric v1.44.0`, `otelhttp v0.67.0`, `exporters/prometheus v0.66.0`, `exporters/otlp/otlptracehttp v1.44.0`, `otel/log v0.19.0`.

**Overall.** core-agent is a comparatively deep OTel Go integration: distributed traces across three binaries (daemon, k8s-event-watcher, tui) with W3C propagation, `otelhttp` wrappers on both server + client boundaries, custom span namespaces (`mcp.tool_call`, `digest.process`, `subagent.llm_call`), a freshly-landed MeterProvider + Prometheus reader on the same substrate (#345), GenAI semconv adoption for token/cost, and configurable exporters honoring the standard `OTEL_TRACES_EXPORTER` / `OTEL_METRICS_EXPORTER` conventions. The SDK is solid once wired correctly — v2.7 ships end-to-end validation against GKE Managed OTel + Cloud Trace + Cloud Monitoring. The pain, most of it, is the API's silent-noop defaults + the module-split packaging story. Several ADK-caused OTel bugs (MeterProvider TODO, double-export, resource merge) live in the [ADK section](#adk-go-googlegolangorgadk) as they belong to the root cause.

### 1. [high] OTel SDK's default diag + error handlers are noop — export failures vanish silently

**Category:** behavior-surprise

**Issue.** By default `otel.GlobalErrorHandler` and `otel.Logger` do nothing. With `OTEL_TRACES_EXPORTER=otlp` and any reachability problem (unreachable collector, TLS mismatch, wrong port, wrong protocol, dropped batch), the SDK swallows the failure — no boot log confirming the exporter was constructed, no error log when a batch fails to ship. During the v2.7.0-dev.5 GKE demo, `OTEL_TRACES_EXPORTER=console` streamed span JSON to stderr but `otlp` was completely silent for a full debug cycle before we realized the SDK was dropping batches quietly.

**Impact.** PR #333 exists solely to install stderr handlers so operators can grep `otel-export:` and `otel-diag` lines. Any user of the raw SDK who forgets these two lines has no way to distinguish "exporter never wired" from "backend rejecting silently."

**Workaround.** `pkg/telemetry/otel.go:90-105` installs `otel.SetLogger(stdr.New(...))` + `otel.SetErrorHandler(...)` unconditionally, gated by `OTEL_LOG_LEVEL`.

**Recommendation.** Make the default error handler log to stderr at least once per unique exporter error class, or expose a boot-time helper `otel.SetupDiagnostics(io.Writer, level)` so every SDK user doesn't have to rediscover the noop-handler footgun.

**Evidence.** PR #333; commit `8391cf1`; `pkg/telemetry/otel.go:90-105`; `docs/site/src/content/docs/concepts/otel.md:187`.

### 2. [medium] `otelhttp.NewHandler` has no per-route sampling — hand-rolled regex filter is the only escape

**Category:** api-friction

**Issue.** Wrapping the attach mux with `otelhttp.NewHandler(...)` (#217) meant every inbound HTTP request produced a server span. The remote TUI polls `/status` + `/usage` every 1-2s for status-bar rendering, plus `/tools /agents /context /memory /skills /mcp /pricing /perms` for slash-command hydration — 30+ noise spans/minute per TUI in Cloud Trace. `otelhttp` offers `WithFilter(func(*http.Request) bool)` but no per-route / per-method sampler tree.

**Impact.** `pkg/attach/server.go:388-414` — 26-line regex + `shouldTraceRequest` that must stay in sync with route additions. PR #340 had to expand it once to cover bare `GET /sessions` and `GET /peers`. Any new hydration read forgets this filter → Cloud Trace floods again.

**Workaround.** `otelhttp.WithFilter(shouldTraceRequest)` at `pkg/attach/server.go:344-359` + `pollingReadRe` at :404. Fail-open on unknown session subpaths so a new route doesn't silently drop.

**Recommendation.** Upstream `otelhttp` should accept a `SamplerByRoute(map[string]sampling.Sampler)` or at least an idiomatic `WithMethodFilter`.

**Evidence.** PR #335, PR #340; `pkg/attach/server.go:344-414`; commit `af4840e`.

### 3. [medium] `otel.Tracer(name)` memoizes — TracerProvider swaps in tests don't retroactively update captured tracers

**Category:** behavior-surprise

**Issue.** We resolve package-level tracers once at load time: `var tracer = otel.Tracer("core-agent/mcp")`. This is the idiomatic pattern, but `otel.GetTracerProvider()` memoizes returned `Tracer` instances per name — so `otel.SetTracerProvider(newTP)` in a test does NOT update our already-captured `tracer` variable. Tests emit into the OLD (noop) provider and assertions fail with 'expected 1 span, got 0'.

**Impact.** Every OTel-recording test in the tree manually resets the package-level `tracer` after swapping the provider (`tracer = tp.Tracer("core-agent/mcp")` at `pkg/mcp/otel_test.go:39`). Couples tests to package-private state and easy to forget — silent test degradation instead of a failure.

**Workaround.** `installRecorder`-style helpers in `pkg/digest/otel_test.go` and `pkg/mcp/otel_test.go` reset the package-level `tracer` directly, plus a `t.Cleanup` to restore it. Not `t.Parallel`-safe — everything using this pattern serializes.

**Recommendation.** Either resolve the tracer lazily at each span-start (small perf hit) or upstream a `TracerProvider.InvalidateTracers()` hook. In-tree, add a lint rule for `var tracer = otel.Tracer(...)` that mandates a test hook.

**Evidence.** `pkg/digest/digest.go:48-53`; `pkg/mcp/digest_wrap.go:38`; `pkg/digest/otel_test.go:41-45`.

### 4. [medium] Process-scoped global provider forces every telemetry test to be non-parallel

**Category:** api-friction

**Issue.** `otel.SetTracerProvider` / `SetMeterProvider` are process-global. Any test that installs a recorder to assert spans / metrics must serialize with every other test in the process — no `t.Parallel()`. This bleeds across package boundaries too: `pkg/telemetry`, `pkg/mcp`, `pkg/digest`, and any subsystem-observer test file must all coordinate.

**Impact.** Every otel-touching test file carries a "not `t.Parallel`-safe" comment. Slower wall-clock, harder to reason about test independence, any new dev adding `t.Parallel()` in the wrong file gets flaky span-count assertions. Counted 6 test files with the constraint.

**Workaround.** Comment blocks + `t.Cleanup` restoration of the prior provider.

**Recommendation.** Upstream: context-scoped `WithTracerProvider(ctx, tp)` for tests, mirroring `otelttrace.NewTracerProvider` scoping in Java.

**Evidence.** `pkg/telemetry/otel_test.go:56`; `pkg/telemetry/metrics_test.go:52`; `pkg/mcp/otel_test.go:29`; `pkg/digest/otel_test.go:32`; `pkg/mcp/digest_wrap_test.go:258,742`.

### 5. [medium] OTel spec has no `OTEL_METRICS_EXPORTER=prometheus` convention — Prometheus is pull-shaped so we invented our own

**Category:** docs-gap

**Issue.** The OTel SDK spec enumerates push exporters for `OTEL_METRICS_EXPORTER` (`otlp`, `console`, `none`) but Prometheus is pull-based — spec has no canonical string for "install a Prometheus reader." We invented the value `OTEL_METRICS_EXPORTER=prometheus` as a core-agent extension (`pkg/telemetry/metrics.go:49`), knowing that if upstream ever ratifies a different string we'll have to rename with a deprecation cycle. `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT` also doesn't cover the Prometheus scrape bind address, so we invented `PrometheusAddr` / `--metrics-addr`.

**Impact.** Design open question #6 (`docs/metrics-design.md:507-514`) flags the collision risk. Operators must learn a core-agent-specific env-var value alongside the standard ones.

**Workaround.** Custom `MetricsExporterEnvVar` constant + `PrometheusAddr` config field with `--metrics-addr` CLI flag.

**Recommendation.** Watch the OTel SDK spec issue tracker for a Prometheus-reader convention. If one lands, migrate with a deprecation cycle.

**Evidence.** `pkg/telemetry/metrics.go:47-65`; `docs/metrics-design.md:507-514`.

### 6. [medium] OTel Go module split forces multi-version dependency management (v1.44 + v0.66 + v0.19)

**Category:** packaging

**Issue.** The `go.opentelemetry.io/otel/*` modules ship on independent version tracks. Our `go.mod` juggles: core `otel/sdk/metric/trace v1.44.0`, `otelhttp v0.67.0`, `exporters/prometheus v0.66.0`, `exporters/stdout/stdouttrace v1.43.0`, `otlp/otlplog/otlploghttp v0.19.0`, `otel/log v0.19.0`, `sdk/log v0.19.0`. Every bump has to be checked across ~15 lines because a mismatched pair (e.g. metric SDK vs Prometheus exporter) compiles cleanly and then panics or silently drops points at runtime.

**Impact.** `chore(deps): bump otel exporters to clear GO-2026-4985` (commit `3d612e0`) had to touch 22 lines in `go.mod` for one security advisory. Bumping one exporter without the SDK is a common way to introduce silent breakage that only shows up in an integration test.

**Workaround.** Manual `go.mod` audits during every OTel version bump. `dev/ci/presubmits/verify-vuln` catches the CVE case but not the API-compat case.

**Recommendation.** Upstream: an `otel-all-vX.Y.Z` meta-module (like grpc-go) that pins compatible sub-module versions would eliminate the mismatch class.

**Evidence.** `go.mod:16-25`, `:136-142`; commit `3d612e0`.

### 7. [medium] GKE Managed OTel Instrumentation CR silently omits `OTEL_SERVICE_NAME` — spans appear as `unknown_service:core-agent`

**Category:** docs-gap

**Issue.** GKE's `Instrumentation` CR auto-injects `OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_TRACES_EXPORTER`, `OTEL_METRICS_EXPORTER`, `OTEL_LOGS_EXPORTER`, `OTEL_TRACES_SAMPLER[_ARG]`, `OTEL_METRIC_EXPORT_INTERVAL`, `K8S_POD_UID`, and `OTEL_RESOURCE_ATTRIBUTES` — but NOT `OTEL_SERVICE_NAME`. Without it, `resource.Default()` falls through to `unknown_service:<binary>` and Cloud Trace groups every daemon under one meaningless label.

**Impact.** Multiple internal docs claimed the CR handled service name until PR #339 audited a live cluster and corrected them. Real users saw `unknown_service:core-agent` in Cloud Trace and had no obvious remediation.

**Workaround.** Set `OTEL_SERVICE_NAME` in the Pod env explicitly (`examples/gke-troubleshoot-agent/deploy/components/otel/daemon-env.yaml`). Doc correction in `docs/site/src/content/docs/concepts/otel.md:144`.

**Recommendation.** Upstream Google: make the Instrumentation CR inject `OTEL_SERVICE_NAME` from the Deployment name by default (or at least document the omission on the GKE Managed OTel page).

**Evidence.** PR #339; commit `34252c4`.

### 8. [low] GenAI semantic conventions still moving — no stable `gen_ai.session.id` forces adjacent naming

**Category:** behavior-surprise

**Issue.** OTel GenAI SIG has stabilized `gen_ai.client.token.usage` + `gen_ai.client.operation.duration` + `gen_ai.request.model` + `gen_ai.token.type` (adopted verbatim in `pkg/usage/metrics.go:88-108`). But `gen_ai.session.id` was proposed and not finalized as of design write time; we fell back to `session.id` (semconv-adjacent). Any dashboards keyed on a future `gen_ai.session.id` won't work against our data without relabeling.

**Impact.** Attribute renames whenever GenAI semconv ratifies a new stable name. Consumer dashboards need a relabeling rule at scrape/collect time to bridge. Not blocking, but a maintenance tax.

**Workaround.** `pkg/usage/metrics.go:97-101` uses `session.id` with a comment noting the SIG's not-yet-finalized alternative. CHANGELOG entry on every rename.

**Recommendation.** Pin to a specific GenAI semconv version in a comment header; bump in dedicated PRs with CHANGELOG rows so ops can migrate dashboards deliberately.

**Evidence.** `pkg/usage/metrics.go:88-108`, `:97-101`; `docs/metrics-design.md:494-499`.

---

## Gemini / Vertex AI (`google.golang.org/genai`)

**Version:** `google.golang.org/genai v1.55.0`.

**Overall.** The genai SDK is the core LLM binding — Vertex + direct Gemini API, ~65 files import it, plus every provider adapter uses genai types as the shared internal shape (the Anthropic adapter converts to and from `genai.Content` / `Config`, `pkg/usage` aggregates `genai.GenerateContentResponseUsageMetadata`). The single-typed cross-provider surface + first-class Caches API + rich UsageMetadata is genuinely useful — Vertex explicit context caching (#221) mapped 1:1 to the SDK surface with no raw-REST fallback needed. Friction concentrates in three areas: (1) Vertex-vs-direct-API divergence exposed as one type but incompatible at the wire; (2) silent-failure shapes the SDK forwards without typed errors (empty responses, bare `STOP`, cached-content `NOT_FOUND`), forcing detection/retry wrappers and string-matching on error text; (3) ADK's converter dropping metadata (`URLContextMetadata`) that only reaches us if we intercept below ADK. Collectively ~1500 LoC of workaround code and multiple hotfix cycles per major Vertex feature we adopt.

### 1. [high] Vertex vs direct Gemini API diverge on `IncludeServerSideToolInvocations` — same SDK type, opposite acceptance

**Category:** behavior-surprise

**Issue.** `genai.ClientConfig.Backend` switches between `BackendGeminiAPI` and `BackendVertexAI` but the two backends have incompatible requirements for the SAME `Config.ToolConfig.IncludeServerSideToolInvocations` field when built-ins ride alongside function tools. Direct Gemini API REQUIRES the flag ("Please enable tool_config.include_server_side_tool_invocations to use Built-in tools with Function calling"). Vertex REJECTS it ("includeServerSideToolInvocations parameter is not supported in Gemini Enterprise Agent Platform (previously known as Vertex AI)"). Our v1.0.0 shipped the flag unconditionally to satisfy the direct API and broke `--provider=vertex` outright at default invocation.

**Impact.** v1.0.0 shipped with Vertex completely broken at default invocation; v1.0.1 was cut ~same day. The `builtinsLLM` wrapper carries an `isDirectGeminiAPI` bool learned at construction time; every future backend-divergent field will require the same investigation pattern.

**Workaround.** `pkg/models/gemini/gemini.go:86-109` detects backend via `p.cfg.Backend == genai.BackendVertexAI`; `pkg/models/gemini/builtins.go:339-345` sets the flag only when `isDirectGeminiAPI`. Regression tests pin both branches.

**Recommendation.** Upstream (`googleapis/go-genai`): reject the field on the wrong backend at Marshal time with a typed error, OR auto-strip it for Vertex. At minimum, a documented per-field "Vertex-only / Direct-API-only" matrix in godoc.

**Evidence.** commit `c505f69`; `pkg/models/gemini/builtins.go:320-346`; `CHANGELOG.md:631`.

### 2. [high] Silent-hang response shape: empty `Content` + empty `FinishReason` + no error, tokens still charged

**Category:** bug

**Issue.** Vertex Gemini occasionally returns a response with `Content.Parts == nil`, `FinishReason == ""`, `ErrorCode == ""`, `ErrorMessage == ""` — but non-zero `UsageMetadata` (tokens consumed). SDK forwards as a normal-looking response. Agent loops see no next action and no error → session goes idle indefinitely. A follow-up shape observed live: `FinishReason=STOP` with no parts. Both look "valid" to any code that doesn't know they're the model's silent-hang failure mode.

**Impact.** Live GKE-triage demo session (`019f5be4-...daf0d`, 2026-07-14 turn 4) sat idle for 5+ minutes with no diagnostic surface, tokens charged for a phantom turn. Required two PRs (#235 for the empty-content shape, #239 for the bare-STOP variant), a package-level `ErrEmptyResponse` sentinel, an `isUsableResponse` classifier, and a `retryOnceOnEmpty` wrapper with three stderr alerts.

**Workaround.** `pkg/models/gemini/builtins.go:641-702` (`wrapEmptyTailDetection` + `isUsableResponse`) synthesizes an `ErrEmptyResponse` at stream tail when nothing usable came through, treats bare-STOP as non-usable, retries once transparently. ~200 LoC of wrapper.

**Recommendation.** genai should not return responses with (a) no parts, (b) no finish reason, and (c) no error simultaneously. Even a synthetic `FinishReason=OTHER` with a hint attribute would let callers surface an actionable error instead of a mystery idle.

**Evidence.** PR #235, PR #239; `pkg/models/gemini/builtins.go:704-717`.

### 3. [high] Vertex `CachedContent` rejects `Tools` / `SystemInstruction` / `ToolConfig` on the same request — undocumented in genai types

**Category:** docs-gap

**Issue.** Setting `GenerateContentConfig.CachedContent` AND leaving `Config.Tools` / `SystemInstruction` / `ToolConfig` populated (as ADK naturally does) causes Vertex to 400 with `"Tool config, tools and system instruction should not be set in the request when using cached content."`. The genai type system does nothing to prevent or hint at this — `CachedContent` is a plain string field alongside `Tools`/`SystemInstruction` with no mutex.

**Impact.** PR #269 (v1 context caching) shipped and immediately broke every cached turn in prod — hotfix PR #270 was cut same day. Wrapper now snapshots the stripped fields on every cached turn so a subsequent cache-eviction retry can restore them; without the snapshot the uncached retry would go to the model missing its system prompt + tools, worse than the original failure.

**Workaround.** `pkg/models/gemini/builtins.go:292-311` stamps `CachedContent` and nils out `SystemInstruction`/`Tools`/`ToolConfig`; `pkg/models/gemini/builtins.go:411-494` `wrapCachedContentEvictionRetry` restores them on a `NOT_FOUND` retry.

**Recommendation.** `genai.GenerateContentConfig` godoc should call this out on the `CachedContent` field, and ideally return a typed error at Marshal time. Better: make `CachedContent` a sum type / union that excludes the incompatible fields at compile time.

**Evidence.** PR #270; commit `9120541`; `pkg/models/gemini/builtins.go:277-311`.

### 4. [medium] Cached-content `NOT_FOUND` on TTL eviction — no typed error, forced substring match on Vertex 404 text

**Category:** api-friction

**Issue.** When Vertex reaps an explicit cache server-side (TTL elapsed on a long-lived daemon holding a cache handle), subsequent `GenerateContent` calls stamped with the dead reference return: `Error 404, Message: Not found: cached content metadata for <id>., Status: NOT_FOUND`. genai does not expose a typed error — we substring-match on both `NOT_FOUND` AND (case-insensitive) `cached content` to distinguish from generic 404s (missing model, wrong region).

**Impact.** Long-lived daemons whose cache outlives its TTL wedged sessions with hard turn errors requiring daemon restart until PR #299 shipped the retry wrapper. Any change in Vertex's error text will silently break the retry path (only caught by false-negative test cases).

**Workaround.** `pkg/models/gemini/builtins.go:511-518` `isCachedContentNotFound`; `internal/vertexcache/manager.go:261-277` `Manager.MarkEvicted` resets the state machine.

**Recommendation.** Add a typed genai error (e.g. `errors.Is`-friendly `ErrCachedContentNotFound`) — clients need to distinguish TTL-eviction (retryable, expected) from generic 404s (config issue, don't retry). String-matching on user-facing prose is a maintenance liability.

**Evidence.** PR #299; `pkg/models/gemini/builtins.go:496-518`; `pkg/models/gemini/cache_eviction_retry_test.go:60-90`.

### 5. [medium] genai HTTP client is not `otelhttp`-wrapped — distributed traces stop at the LLM boundary

**Category:** missing-feature

**Issue.** `genai.ClientConfig` has an `HTTPClient` field but neither the SDK nor our provider wraps it with `otelhttp.NewTransport` by default. Result: ADK's `adk.call_llm` / `subagent.llm_call` spans exist, but no HTTP client span for the actual POST to `aiplatform.googleapis.com` / `generativelanguage.googleapis.com` is emitted, and no `traceparent` header rides on the request. Distributed traces stop at the LLM boundary.

**Impact.** Docs (`concepts/otel.md`) had to publish a "Known gap" section explicitly calling out that Vertex/Gemini calls don't participate in the distributed trace, even though every other outbound HTTP surface (attach, MCP, k8s-event-watcher) does. [Issue #325](https://github.com/go-steer/core-agent/issues/325) remains OPEN.

**Workaround.** None shipped yet. Sketch in the issue: wrap `genai.ClientConfig.HTTPClient` with `otelhttp.NewTransport` at three known construction sites.

**Recommendation.** genai should either wrap its default HTTP client with an `otelhttp` transport when OTel is initialized in the process, or document + prominently sample-code the wrap pattern in the `ClientConfig` godoc.

**Evidence.** Issue #325; PR #326; `pkg/models/gemini/gemini.go:146`.

### 6. [low] `cachedContentTokenCount` > `promptTokenCount` observed occasionally — no docs, forced defensive clamp

**Category:** bug

**Issue.** `UsageMetadata.CachedContentTokenCount` is documented as a subset of `PromptTokenCount`, but the tracker observed occasional Vertex responses where cached > prompt. Without a defensive clamp, the derived "uncached input = input - cached" math goes negative, breaking downstream cost accounting and any UI that renders both.

**Impact.** Cost accounting silently corrupts if raw values are used. Every path that surfaces "cached vs uncached" has to remember to clamp — currently only the record site does; `TurnUsageFromGenaiMetadata` deliberately doesn't clamp so raw per-event snapshots stay observable (adds a subtle multi-caller invariant).

**Workaround.** `pkg/usage/tracker.go:170-172` clamps `CachedInputTokens` to `InputTokens` at `AppendUsage`; `TestTracker_AppendUsage_ClampsCachedOverInput` pins the invariant.

**Recommendation.** Fix at source in Vertex/genai — `CachedContentTokenCount` should never exceed `PromptTokenCount` by definition. If it can, document why and rename the field.

**Evidence.** PR #248; `pkg/usage/tracker.go:162-173`.

### 7. [low] No place to carry `cache_creation` tokens across the genai↔ADK bridge → Anthropic-via-genai cost undercount

**Category:** api-friction

**Issue.** The Anthropic adapter projects Anthropic's usage into `genai.GenerateContentResponseUsageMetadata` so the same tracker + pricing math works cross-provider. Anthropic reports three input buckets (`InputTokens`, `CacheReadInputTokens`, `CacheCreationInputTokens`); genai's `UsageMetadata` has fields for total + cached but NO field for cache-creation tokens. Result: cache_creation tokens fold into the uncached-input bucket, billed at 1× the input rate rather than Anthropic's actual 125% — a systematic undercount of `~cache_creation × input_rate × 0.25` on cache-warming turns.

**Impact.** Anthropic cost tracking is silently biased low on cache-warming turns. Steady-state cache-hit turns unaffected. Slice B follow-up (#263) remains open precisely because the fix is invasive due to the genai type limitation.

**Workaround.** None — documented gap in `pkg/models/anthropic/stream.go:64-73`.

**Recommendation.** Add a `CacheCreationInputTokenCount` field to `genai.GenerateContentResponseUsageMetadata` (or a general `CachedInputBreakdown` sub-struct). Cross-provider adapters can then project honestly; Google-native consumers who don't have cache_creation just see zero.

**Evidence.** `pkg/models/anthropic/stream.go:64-73`; PR #264; Issue #263.

---

## Second-tier dependencies

These libraries work reliably in production, but almost every one required a bespoke wrapper to fit our runtime shape. Grouped by library, ranked within by severity.

### anthropic-sdk-go (`github.com/anthropics/anthropic-sdk-go v1.43.0`)

**[low] `vertex.WithGoogleAuth` panics on missing ADC — bypassed with `FindDefaultCredentials` + `WithCredentials`.** The Vertex constructor panics at startup when ADC isn't resolvable — the wrong failure mode for a daemon. We load credentials ourselves via `google.FindDefaultCredentials` and pass them to `vertex.WithCredentials`. Workaround at `pkg/models/anthropic/vertex.go:53-70`. **Recommendation:** upstream should return an error, not panic.

_(The cache_creation cost-accounting gap lives in the [Gemini section](#7-low-no-place-to-carry-cache_creation-tokens-across-the-genaiadk-bridge--anthropic-via-genai-cost-undercount) since the root cause is genai's `UsageMetadata` shape.)_

### modelcontextprotocol/go-sdk (`github.com/modelcontextprotocol/go-sdk v1.4.1`)

**[high] Non-2xx responses drop the JSON-RPC error body, hiding actionable server messages.** The MCP SDK reports non-2xx HTTP responses using only `http.StatusText(resp.StatusCode)` and drops the JSON-RPC error body — which is exactly where servers like Google's `container.googleapis.com/mcp` put the missing IAM permission name. Operators saw opaque `Forbidden` / `Unauthorized` strings with no way to diagnose. **Workaround:** `pkg/mcp/errbody.go` (`jsonRPCErrorTransport` wraps `http.RoundTripper` below OTel + above auth so it sees the raw response). Wired at `pkg/mcp/lifecycle.go:291-297`. ~200 LoC + 250 LoC of tests. **Recommendation:** upstream — the SDK's HTTP client layer should optionally surface the JSON-RPC error body. **Evidence:** `pkg/mcp/errbody.go:32-45`; PR #180.

**[medium] Pinned at v1.4.1; Streamable HTTP + OAuth2 primitives shipped but wiring is deferred.** Every primitive needed for RFC 9728 discovery + RFC 8414 metadata + RFC 7636 PKCE lives in the SDK (`auth/`, `oauthex/`, `mcp/streamable_client.go`) but `pkg/mcp` supports only stdio and http transports with `google_oauth` auth. Blocks Slack MCP and other spec-compliant OAuth-protected MCPs. **Impact:** v2.6's k8s-event-agent Slack escalation path was DROPPED — only workaround was a native alert tool in a separate design (`docs/alert-tool-design.md`, #192). **Workaround:** none yet; full plan in [`docs/mcp-oauth-design.md`](mcp-oauth-design.md). **Recommendation:** ship #190 (mcp-oauth) — three PRs, ~1 week.

**[low] Unexported `runnable` interface + no built-in elicitation fallback.** ADK's `mcptoolset` expects tools to implement an unexported `runnable` (`Declaration()` + `Run()`); we re-declare it at `pkg/mcp/namespace.go:34-37` so we can type-assert. `mcpsdk.ClientOptions.ElicitationHandler` accepts only a raw func — no default "decline" behavior — so we ship a `DeclineHandler` stub for headless runs (`pkg/mcp/elicitation.go:46-57`). ~15 LoC each, but every wrapper consumer downstream of `mcptoolset` hits the same unexported-interface trap. **Recommendation:** export the `Runnable` interface; upstream a default `Decline` handler helper.

### go-steer/core-tui (`github.com/go-steer/core-tui v0.16.1`)

**[medium] Extremely high cross-repo churn — ~15 version bumps in a few months.** Every UX improvement in the remote-TUI path requires (1) a core-tui PR, (2) a version tag, (3) a core-agent bump PR that adopts the new capability. From v0.6.3 → v0.16.1 that's 15+ bumps, each referencing a specific cross-repo issue (`core-tui#7-67` tracked in `CHANGELOG.md`). **Recommendation:** consider a monorepo or a stronger BFF contract. In the meantime, adopt trunk-based cross-repo development for the tightest pair (attach + core-tui). **Evidence:** commits `01930f5`, `ddad37e`; `CHANGELOG.md:134-137`.

**[medium] Synchronous slash dispatch (`core-tui#10`) freezes TUI during long slash handlers.** Same root cause as the [Bubbletea `InvokeSlash` finding](#3-high-invokeslash-runs-in-bubbleteas-single-update-goroutine-long-running-slash-handlers-freeze-the-ui). **Recommendation:** ship core-tui#10 (async slash dispatch) — ours to build since core-tui is in-house.

**[medium] Reconnect loop retried forever on daemon 404 — required cross-repo `PermanentStreamError` contract.** Before v0.11.0, the TUI's SSE reader retried forever when the daemon returned 4xx on attach (e.g., 404 after a session was reaped). Fixed cross-repo by adding a `PermanentStreamError` interface on the TUI side + typed `httpStatusError` wrapper on the client side (`internal/attachclient/status_error.go`). **Evidence:** commit `ddad37e` (#268); [core-tui#51](https://github.com/go-steer/core-tui/issues/51).

### gorm + glebarez/sqlite (`gorm.io/gorm v1.31.1`, `github.com/glebarez/sqlite v1.11.0`)

**[high] `SQLITE_BUSY` on concurrent writers — reflection into private DSN field to inject `busy_timeout` pragma.** SQLite defaults `busy_timeout=0` so concurrent writers fail immediately instead of waiting. GORM opens its own connection pool internally (ADK's `database.SessionService` doesn't expose it), so we can't set the timeout on the pool. Fix: reflect into the dialector's DSN string field and append `_pragma=busy_timeout(5000)` before ADK and the overlay each open their own `gorm.DB` pool. Reflection works for both `glebarez/sqlite` and `gorm.io/driver/sqlite`. **Impact:** surfaced first UAT run — `[alert] default-ns-monitor failed: error creating session on database: database is locked (5) (SQLITE_BUSY)`. Blocked parent-agent-with-background-subagents scenario until fixed. **Workaround:** `pkg/eventlog/sql.go:174-207` (`injectSQLitePragma` via `reflect.ValueOf(d).FieldByName("DSN")`). Belt-and-braces: also serialize writes at the Go layer through a `writeMu` (`pkg/eventlog/service.go:32-42`) because the connection pool timeout still isn't tunable. **Recommendation:** file upstream on gorm — a `PoolOption(func(*sql.DB))` or DSN-postprocessing hook. Alternative: propose to `glebarez/sqlite` a Dialector option for pragmas. **Evidence:** commit `ac6975e`.

**[low] ADK's `database.SessionService` hides its `*gorm.DB` → we open a second connection for the eventlog overlay.** Same root cause as the [ADK section entry](#12-medium-sessiondatabase-has-no-eventual-atomic-append-no-exported-gormdb-and-no-monotonic-seq). We chose to open a second `gorm.Open` against the same dialector rather than reflect into ADK's private field. **Evidence:** `pkg/eventlog/sql.go:109-114`; `docs/eventlog-decisions.md:75-78`.

**[low] Pure-Go trade-off is deliberate but perf tail-latency is a real cost.** We chose `glebarez/sqlite` (wraps `modernc.org/sqlite`) over `mattn/go-sqlite3` (cgo) to preserve `CGO_ENABLED=0` builds. Trade-off documented at `docs/DESIGN.md:392-395`: ~10MB binary growth, ~2x slower per write, not observed as a bottleneck. **Recommendation:** accept; revisit only if a real workload complains.

### k8s.io/client-go (`v0.36.2`)

**[medium] Deliberately kept out of core-agent → sidecar architecture.** client-go's transitive footprint (~5MB) and k8s API version churn made it too heavy to import from core-agent directly. Chose sidecar: `cmd/k8s-event-watcher` runs alongside the daemon and POSTs to `/inject`. Two binaries + a container image (~200 LoC of dispatch wiring) instead of one, but keeps core-agent k8s-agnostic and independently versioned. **Recommendation:** accept — the layering is right; friction is inherent to client-go's transitive surface. **Evidence:** `docs/k8s-event-agent-design.md:46`.

**[low] `cache.HandleCrash` races with `ctx.Done` on shutdown → mutate global `runtime.ErrorHandlers`.** client-go's informer emits noisy `unknown object type in cache` errors during graceful shutdown because `cache.HandleCrash` doesn't cooperate with `ctx.Done`. Only fix is to replace the global `runtime.ErrorHandlers` slice, which is process-scoped mutation. **Workaround:** `cmd/k8s-event-watcher/watcher.go:106-114`. **Recommendation:** upstream — informers should honor ctx and stop emitting cache errors on `ctx.Done`, or expose a scoped error-handler registration.

### google/jsonschema-go (`v0.4.2`)

**[low] Only used for a `$ref`-wrapped subset validation test — API is low-level.** The only real consumer is `pkg/attach/agentcard_test.go`, which validates an emitted card against a vendored A2A schema bundle. Because the emitted card is a subset (`#/definitions/AgentCard`), the test manually constructs a wrapper `Schema` with `Ref: "#/definitions/AgentCard"` and passes it through `.Resolve(nil)` — no helper for validating one definition out of a bundle. **Workaround:** `pkg/attach/agentcard_test.go:438-483`. **Recommendation:** ask upstream for a `ValidateAgainstDefinition(bundle, defName)` helper.

### itchyny/gojq (`v0.12.19`)

**[low] Whole-language dependency for one narrow tool — chosen deliberately for pure-Go/distroless.** gojq (a full jq implementation in Go) is imported solely for the `json_query` built-in tool. Chosen because it's pure Go (no CGO) so distroless keeps working. Larger binary than a purpose-built JSONPath, but the model already knows jq syntax; ergonomics win over binary-size cost. **Recommendation:** accept — no known friction. **Evidence:** `pkg/tools/json.go:48-51`; commit `8af06e5`.

### prometheus/client_golang (`v1.23.2`)

**[medium] Two-exporter split — `k8s-event-watcher`'s native Prometheus vs `pkg/telemetry`'s OTel Prometheus reader.** `prometheus/client_golang` is used two ways: (a) directly at `cmd/k8s-event-watcher/metrics.go` (native `CounterVec`/`GaugeVec` on a dedicated registry); (b) as an OTel-side reader bridge at `pkg/telemetry/metrics.go` (OTel `promexporter.New` writing to a fresh `prometheus.NewRegistry`). Two live in parallel until `k8s-event-watcher` migrates onto the same MeterProvider substrate. **Recommendation:** execute the migration in the metrics-design plan; wire format stays identical for backward compat. **Evidence:** `pkg/telemetry/metrics.go:132-146`; `cmd/k8s-event-watcher/metrics.go:46-84`; `docs/metrics-design.md:128-133`.

### golang.org/x/oauth2 (`v0.36.0`)

**[medium] `idtoken` rejects end-user (`authorized_user`) ADC — cryptic error required a rewrite with two workarounds.** `idtoken.NewTokenSource` doesn't accept end-user ADC — it wants a service-account key or impersonation. Its error is cryptic (`unexpected credentials type: authorized_user, wanted: service_account`) with no operator-facing guidance. We wrote a bespoke error rewriter that detects the `authorized_user` substring and returns a multi-line hint pointing to `--auth=google-oauth` or `--impersonate-service-account`. First-time Cloud Run operators hit this and got stuck. **Workaround:** `cmd/core-agent-tui/auth.go:139-162` (`explainIDTokenSourceError`). **Recommendation:** upstream — `idtoken` should either accept `authorized_user` (via a compat shim) or return a wrapped error with the workarounds. Doc gap is the fix at minimum. **Evidence:** commits `2cea792`, `3633da1`.

**[low] Token-source lazy failures require fail-fast pre-fetch pattern in every consumer.** `google.FindDefaultCredentials` returns a lazy `TokenSource` — misconfigurations (missing scopes grant, unreachable metadata server) don't surface until the first `Token()` call. MCP server-init succeeds but the first tool call fails opaquely. We pre-fetch a token at server-init time to surface config errors early. **Workaround:** `pkg/mcp/lifecycle.go:277-282`. **Recommendation:** consider an `x/oauth2` helper `TokenSource.PrefetchOrError(ctx)`.

**[low] No built-in "compose transport" primitive → three hand-rolled `RoundTripper` wrappers stacked in a specific order.** For MCP HTTP transports we need three composed `RoundTripper`s: `googleAuthTransport` (Bearer injection), `headerTransport` (custom static headers), `jsonRPCErrorTransport` (SDK error-body workaround), plus `otelhttp` outermost. Order is load-bearing — auth wraps innermost so custom headers can't overwrite Authorization. oauth2 ships `oauth2.NewClient` but that returns a full `*http.Client`, not a composable `RoundTripper` — inconvenient when you already have another transport chain. **Workaround:** `pkg/mcp/lifecycle.go:261-311` with explicit ordering comment. **Recommendation:** accept; alternative would be a helper like `oauth2.WrapRoundTripper(base, source)`.

---

## Appendix: how this log was compiled

Six research agents ran in parallel over `main` at commit `fa274ce` (2026-07-21):

1. A scout inventoried direct deps from `go.mod` + `package.json` and enumerated the doc + git-log search surface.
2. Four library agents (ADK Go, Bubbletea + Charm, OpenTelemetry Go, Gemini / Vertex) each grepped commit history + PR bodies + design docs + code comments for their scope.
3. One agent covered the second-tier deps flagged by the scout (Anthropic SDK, MCP SDK, core-tui, gorm+sqlite, client-go, jsonschema-go, gojq, prometheus, oauth2).

Each finding was required to cite a commit hash, PR number, or file:line — vibes-only claims were discarded. Total: ~890k tokens across 668 tool calls, 29 minutes wall clock. Raw agent output kept in the workflow transcript at `.claude/projects/-home-user-projects-core-agent--claude-worktrees-friction/*/subagents/workflows/wf_256dd1f4-3d5/journal.jsonl` for verification.

Next steps to consider:

- **Upstream tracker triage.** Every high-severity finding without an `upstreamIssue` link is a candidate for filing. Highest-value: ADK #479 (MeterProvider) is already tracked; the `session/database` optimistic-concurrency + stripped-events pair, the `RequestProcessor`/`PackTool` internal-only pair, and the genai backend-divergence field would each be worth an ADK/genai issue.
- **Blog adaptation.** Once findings are triaged, distill the strongest 8-10 (across all libraries) into a "lessons from a year building on ADK Go + Charm + OTel" post for `docs/site/src/content/docs/blog/`.
- **Keep it living.** Update this file alongside PRs that add new workarounds — each `// HACK` / `// WORKAROUND` / `// XXX` comment should have an entry here so the pattern doesn't decay into folklore.
