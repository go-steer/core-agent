# Changelog

All notable changes to `core-agent` are recorded here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## Stability promise

The public API of `core-agent` is the exported surface of these packages:

- `agent`, `eventlog`, `tools`, `permissions`, `config`, `models` (+ `models/anthropic`, `models/gemini`, `models/mock`), `recording`, `runner`, `session`, `usage`, `instruction`, `mcp`, `skills`, `telemetry`

Pre-1.0, breaking changes are possible at any minor version (`v0.X`). When we make one, the change is called out in this file under **Changed** or **Removed**, and non-trivial removals get a one-version deprecation period when feasible. Patch versions (`v0.X.Y`) are bug fixes only.

The `extras/` adapters (`extras/scion-agent/`, `extras/ax-agent/`) and the `internal/` packages they ship with track `core-agent`'s minor version but do not promise their own stability — adapters target moving runtimes (Scion, AX) and follow whatever those upstream projects do.

---

## [Unreleased]

### Changes by Kind

#### Feature
- Telemetry: `OTEL_TRACES_EXPORTER` env var overrides `otel.exporter` in config. Matches the OTel SDK spec's env-vars-win convention. Load-bearing for multi-daemon k8s deployments where a shared `.agents/config.json` ConfigMap can't carry per-Pod exporter targets — operators wire the mode via a per-Deployment env patch instead of forking config.json. Empty env leaves the config value intact; invalid env values surface the same "unknown mode" error as invalid config values. Prep for the deploy/overlays/example-otel component.

#### Other (Cleanup)
- Release tooling: `dev/release/cut-dev-tag.sh` now runs the release-workflow preflight guards (starting with pricing-catalog freshness) BEFORE carving the CHANGELOG. Catches drift locally in seconds instead of a 4-minute CI round-trip that ends in a broken tag needing retag. Motivated by the 2026-07-18 v2.7.0-dev.4 retag caused by a 3-day stale `internal/pricing/builtin.go`. Guards intentionally mirror `.github/workflows/release.yml` — keep in sync as new guards land there.

## [2.7.0-dev.4] — 2026-07-18

_In flight toward v2.7.0. Commits since [2.7.0-dev.3]:_

### Changes by Kind

#### Feature
- MCP: agentic wrap — full stack ships. `pkg/digest.Savings` struct populated per-call, `LLMFallback` second-chance path for prose responses structural pruning can't reduce, `--mcp-agentic-wrap-llm` opt-in flag + `agentic_wrap_llm` mcp.json field, `usage.Tracker.DigestSavings` accumulators, `attach.ContextInfo.DigestSavings` wire fields, OTel spans (`mcp.tool_call` → `digest.process` → `subagent.llm_call`) with `core_agent.*` + `gen_ai.*` attributes. Closes [#223](https://github.com/go-steer/core-agent/issues/223) ([#290](https://github.com/go-steer/core-agent/pull/290))
- Pricing: Vertex explicit context caching v1 — creates a `CachedContent` resource after turn 1 and stamps it onto every subsequent `GenerateContent` call so the stable prefix (system instruction + tools) bills at cache-read rates instead of full input rates. Deterministic vs. Vertex's opportunistic implicit cache. Gated by `cfg.Model.Vertex.ContextCache.enabled` (default on) + `--no-context-cache` kill switch. Closes [#221](https://github.com/go-steer/core-agent/issues/221) ([#269](https://github.com/go-steer/core-agent/pull/269))
- MCP+attach: per-tool-call `latency_ms` sidecar on tool-result response maps. Emitted by every MCP wrapper (structural digest, agentic wrap, bare `renamedTool` on the passthrough path); consumers pick it up as a compact `[2.4s]` chip on the tool row + a chip in the tool-call detail overlay. Closes [#277](https://github.com/go-steer/core-agent/pull/277) ([#278](https://github.com/go-steer/core-agent/pull/278))
- CLI: new `-i` / `--interactive-prompt` flag seeds the first turn of a REPL or TUI session and stays interactive — useful for shell aliases and scripted onboarding (`core-agent -i "audit this repo for races"`). Mutually exclusive with `-p`; incompatible with `--no-repl`. Requires core-tui v0.13.0 (adds `Options.InitialPrompt`). Closes [#291](https://github.com/go-steer/core-agent/issues/291).
- CLI: `--log-file` mirrors the daemon's stderr to a file so operators driving detached sessions (kubectl exec, systemd, launchd) can retain the startup summary + turn-log without shell-scrollback loss. Closes [#179](https://github.com/go-steer/core-agent/issues/179) ([#306](https://github.com/go-steer/core-agent/pull/306))
- coretuiremote: `Adapter.Interrupt` implements core-tui's `RemoteInterrupter` capability so the remote TUI's `/interrupt` slash reaches the daemon's `POST /sessions/<sid>/interrupt` endpoint. Fixes the observer-mode gap where `/interrupt` short-circuited with "no turn in flight" even when the daemon had an in-flight autonomous turn ([#304](https://github.com/go-steer/core-agent/pull/304))
- coretuiremote: multi-daemon `/switch` + `/attach` (issue [#246](https://github.com/go-steer/core-agent/issues/246) Phase 1) ([#253](https://github.com/go-steer/core-agent/pull/253))

#### Bug or Regression
- coretuiremote: surface Context row in `/stats` — the remote-TUI adapter's `ContextWindowSize` / `ContextWindowUsed` were hardcoded to `0`, so `/stats` over attach always rendered `Context: (unknown)` regardless of model. Now delegates to `usage.Tracker` the same way the in-process TUI's bridge does. Closes the attach-mode `/stats` regression flagged post-#308 ([#313](https://github.com/go-steer/core-agent/pull/313))
- Digest: expand nested-JSON strings before truncating. MCP servers whose native wire encoding wraps structured data as a JSON-string inside a JSON envelope (GKE MCP, any `mcp/text-content` payload carrying JSON) previously digested to opaque `<truncated, N chars>` markers because the pruner saw the string as one long opaque value. The pruner now sniffs the prefix, parses the string as JSON, and recursively prunes the inner structure. Fixes a runaway `list_skills` loop root cause + ~6× cost regression observed on the 2026-07-17 demo drive ([#302](https://github.com/go-steer/core-agent/pull/302))
- Digest+agent: observe savings on the agent side. #290's `DigestOptions.OnResult` wiring only reached the process-level tracker; multi-session sessions never saw their own DigestSavings populated (each session gets its own `usage.Tracker`). Moved accumulation to the agent's per-turn event tap — reads the `savings` sidecar off `FunctionResponse.Response` and appends to the session's own tracker. Fixes `/context.digest_savings` in multi-session mode ([#308](https://github.com/go-steer/core-agent/pull/308))
- MCP: surface JSON-RPC error body when the MCP server returns 4xx/5xx. Prior behavior swallowed the response body, leaving operators with just the HTTP status. Now the full JSON-RPC error message + code appears in the daemon stderr + tool-result payload. Closes [#180](https://github.com/go-steer/core-agent/issues/180) ([#305](https://github.com/go-steer/core-agent/pull/305))
- Vertex cache: retry uncached when the cache reference 404s. Fixes the resumed-session wedge where a stale cache handle (server-side TTL elapsed while the daemon still held it) caused every subsequent turn to hard-fail with `NOT_FOUND: cached content metadata`. Wrapper catches the specific error signature, marks the manager evicted, restores the stripped SystemInstruction/Tools/ToolConfig, and retries once. Closes the #221 hotfix follow-up ([#299](https://github.com/go-steer/core-agent/pull/299))
- Pricing: #221 hotfixes — nil Vertex guard + strip Tools/SystemInstruction on cached turns (Vertex rejects the combination) ([#270](https://github.com/go-steer/core-agent/pull/270))
- Pricing: wire LiteLLM cache-read rates + expose cached rate on `/pricing` + Anthropic cache-read ([#259](https://github.com/go-steer/core-agent/issues/259) Slice A) ([#264](https://github.com/go-steer/core-agent/pull/264))
- coretuiremote: `Adapter.LastTurn()` falls back to `/usage` snapshot when live-stream state is empty ([#258](https://github.com/go-steer/core-agent/pull/258))
- Digest: `EventlogStore.Put` now writes to a derived `<sid>:digest` session row so mid-turn digest writes don't trip ADK's optimistic-concurrency check against the runner's session snapshot ([#273](https://github.com/go-steer/core-agent/issues/273))
- coretuiremote: `Adapter.SwitchToSession` returns a full `SwitchTarget` — UsageTracker, Memory, Skills, MCPServers, and Branding all refresh so `/switch` actually updates the title bar and `/stats` ([#274](https://github.com/go-steer/core-agent/issues/274))
- Multi-session: per-session `usage.Tracker` in the session factory so `AttachUsage`, the broadcaster's usage-update snapshot, and per-session cost ceilings stop returning the union across every session on the daemon ([#275](https://github.com/go-steer/core-agent/issues/275))
- Multi-session: wire Compactor / Checkpointer / CostCeiling in the session factory so on-demand sessions get the same context-management hooks the initial-run agent has ([#280](https://github.com/go-steer/core-agent/pull/280))
- Multi-session: three post-launch bugs — digest race, /switch chrome, shared tracker ([#276](https://github.com/go-steer/core-agent/pull/276))
- Release: dev-tag republish overlays release helpers from `origin/main` so tags cut before `dev/release/compose-release-notes.sh` existed can be republished via `gh workflow run release.yml --ref main -f tag=vX.Y.Z-dev.N`; unblocks the three orphan `v2.7.0-dev.[123]` tags that failed the initial workflow.
- Release images: bury the broken `set-package-descriptions.sh` (GitHub has no REST/GraphQL API for setting a GHCR package's description); add per-image `description` matrix entries + wire them through both `labels:` and manifest-index `annotations:` via `docker/metadata-action` (`DOCKER_METADATA_ANNOTATIONS_LEVELS=index,manifest`). `docker inspect` / OCI-aware registry mirrors now see the correct per-image description; GHCR's web UI still shows the shared repo description because packages linked to a repo (via the initial-push `image.source` LABEL) render the repo description regardless of subsequent OCI metadata, and the link is server-side + unbreakable via image metadata alone — a real GitHub product gap tracked in [community/discussions#26565](https://github.com/orgs/community/discussions/26565).

#### Documentation
- README: rewrite from a v1 launch document to a v2 product doc — drop the Milestones / Roadmap sections, replace paragraph-form Features with a category-grouped bulleted list covering multi-session, remote TUI, plan-first, agent-card, k8s-event-watcher, add a container-variants table, remove hard-coded version numbers (release-shield badge renders current dynamically).
- Site docs: kill "13-tool" / "Thirteen tools" phrasing across five files (`_index.md`, `docs/_index.md`, `why-core-agent.md`, `library/api.md`, `cli/interactive/workflows.md`, `reference/tools.md`) — describe tools by category so the count can't rot; convert five "from vX.Y.Z" prose fragments to `(since vX.Y.Z)` historical markers; fix pinned `@v2.0.0` install snippet on the landing page.
- AGENTS.md: refresh stale bits (dropped "we just did it for v1.8.0" anchor, removed obsolete "Bump the README pin" step now that the README doesn't hard-code a version, deleted the M1/M2/M3 `## Status` section that references a Milestones section the README no longer has), added a docs-lint mandate under the existing "Hugo site walks alongside" rule.
- `examples/gke-troubleshoot-agent/SCENARIOS.md` — 8 trigger-and-revert recipes for k8s-event-watcher reasons beyond `ImagePullBackOff` (which `DEMO.md` covers). Targets `online-boutique` so operators with the recipe deployed can exercise the watcher against known workloads. Three log-required scenarios (`checkoutservice` upstream, `cartservice`↔`redis-cart`, `paymentservice` bad env) test whether triage actually reads container logs vs. relying on `kubectl describe` alone ([#311](https://github.com/go-steer/core-agent/pull/311))
- Design: `docs/agentic-mcp-design.md` addendum for [#223](https://github.com/go-steer/core-agent/issues/223) — savings telemetry (`digest.Savings` struct threaded through eventlog + `/stats` + per-tool footer) and OTel span shape (`mcp.tool_call` → `digest.process` → `subagent.llm_call`) with `core_agent.*` + `gen_ai.*` attributes. Closes the "is the cost infra earning its keep" observability gap for both structural ([#128](https://github.com/go-steer/core-agent/issues/128), shipped) and agentic (#223, upcoming) digest paths, and ticks the MCP leg of [#217](https://github.com/go-steer/core-agent/issues/217)'s E2E distributed tracing.

#### Other (Cleanup)
- Extras/scion-agent: replace the adapter with hooks + a `sciontool_status` built-in — thinner integration surface, matches how other extras will grow. Closes the "replace bespoke extras with hooks" thread ([#298](https://github.com/go-steer/core-agent/pull/298))
- `retrieve_raw`: sharpen the tool description to prevent digest-defeat. Leads with the anti-pattern ("Treat the digest as authoritative — DO NOT call to spot-check") and names the cost. Field-observed cost regression when Flash reached for retrieve_raw casually ([#300](https://github.com/go-steer/core-agent/pull/300))
- Refactor: shared `TurnTap` helper for per-turn usage bookkeeping — de-duplicates the per-turn accumulation logic that had grown copies in every run entry point. Closes [#157](https://github.com/go-steer/core-agent/issues/157) ([#307](https://github.com/go-steer/core-agent/pull/307))
- Deps: bump core-tui v0.16.0 → v0.16.1 for fast attach to long remote sessions. Coalesces per-event `refreshViewport` calls into a single batched paint per ~1ms window; total work over N-event catch-up drops from O(N²) to O(N × batch-size). Applies to every event-driven message class (streamChunk / toolCall / toolResult / usage / statusUpdate / etc.); user-input paths and dialogs keep refreshing immediately. Adjacent win: WindowSizeMsg short-circuits when dimensions haven't changed. Fully backward-compatible — pure internals, no wire or API change. ([go-steer/core-tui#67](https://github.com/go-steer/core-tui/pull/67))
- Deps: bump core-tui v0.15.0 → v0.16.0 for live-session banner reword. Replaces the misleading "Live session — your messages drive the agent" wording with "Attached to live session — events stream below; type to send a message." Refines the observer-vs-live binary split (core-tui #50) to acknowledge autonomous-producer setups (k8s-triage, MCP alerting) where operator injection is one input source among many rather than the primary driver. Field-observed on the 2026-07-18 demo drive. ([go-steer/core-tui#66](https://github.com/go-steer/core-tui/pull/66))
- Deps: bump core-tui v0.14.0 → v0.15.0 for RemoteInterrupter. Wires coretuiremote.Adapter.Interrupt so the remote TUI's `/interrupt` slash reaches the daemon's `POST /sessions/<sid>/interrupt` endpoint — previously the slash short-circuited with "no turn in flight" in observer mode even when the daemon had an in-flight autonomous turn. Field-observed on the 2026-07-17 demo drive (session 019f71b3-5f2c-720e-8bb9-cf3723369eb9's runaway list_skills loop). ([go-steer/core-tui#65](https://github.com/go-steer/core-tui/pull/65))
- Deps: bump core-tui v0.13.0 → v0.14.0 for digest-wrap savings rendering (SSE spec v1.3.0). Adds the inline `[12k→2.1k tok · struct]` chip on every wrapped MCP tool row + matching chip in the tool-call detail overlay — makes the `savings` sidecar core-agent has been emitting since [#290](https://github.com/go-steer/core-agent/pull/290) operator-visible. Session-level cumulative `/stats` block deferred to a follow-up. ([go-steer/core-tui#64](https://github.com/go-steer/core-tui/pull/64))
- Deps: bump core-tui v0.10.1 → v0.10.2 for observer-mode per-turn footer ([#262](https://github.com/go-steer/core-agent/pull/262))
- Post-drive: implicit-caching caveat in docs + startup log line for mcp digest store binding ([#261](https://github.com/go-steer/core-agent/pull/261))
- Container images: per-image `org.opencontainers.image.description` in the release-images matrix, threaded through both `labels:` (for OCI-aware tooling) and `annotations:` with `DOCKER_METADATA_ANNOTATIONS_LEVELS=index,manifest` (GHCR pulls its package-page description from the manifest index annotation on multi-arch images — labels alone don't populate it); also sets `org.opencontainers.image.documentation` pointing at the site. Dropped the redundant `LABEL org.opencontainers.image.description` from `Dockerfile` since the workflow labels/annotations win.
- Docs freshness linter: new `dev/tools/docs-lint` + `dev/ci/presubmits/verify-docs-lint` + standalone `.github/workflows/docs-lint.yml`; hard-fails on four drift patterns (numeric tool counts, spelled-out image counts, pinned `@vX.Y.Z` in install snippets, wrong-major prose version pins) with a `--self-test` mode that verifies every rule fires against an inline fixture.


## [2.7.0-dev.3] — 2026-07-14

**Structural digest system + per-turn cost attribution.** Pre-release cut of v2.7 development. Ships the pkg/digest skeleton (Process + router + JSON pruner), the digest store (Filesystem + Eventlog backends), and MCP wrap that routes tool responses through the digest before they hit the model context — plus a `retrieve_raw` built-in for pulling the original unpruned response back on demand. Per-turn cost + cache-attribution on `/sessions/<id>/usage` makes the remote TUI's per-turn footer render correctly. First cut with the new `DELETE /sessions/{sid}` endpoint (issue [#81](https://github.com/go-steer/core-agent/issues/81)). Tracking: [#128](https://github.com/go-steer/core-agent/issues/128), [#84](https://github.com/go-steer/core-agent/issues/84), [#222](https://github.com/go-steer/core-agent/issues/222), [#81](https://github.com/go-steer/core-agent/issues/81).

### Changes by Kind

#### Feature
- MCP+digest: wire structural digest into MCP wrap + register `retrieve_raw` (closes [#84](https://github.com/go-steer/core-agent/issues/84) = [#130](https://github.com/go-steer/core-agent/issues/130) + [#129](https://github.com/go-steer/core-agent/issues/129)) ([#257](https://github.com/go-steer/core-agent/pull/257))
- Digest: EventlogStore + `retrieve_raw` built-in tool ([#128](https://github.com/go-steer/core-agent/issues/128) steps 3 + 4) ([#256](https://github.com/go-steer/core-agent/pull/256))
- Digest: Store interface + FilesystemStore + Process wire ([#128](https://github.com/go-steer/core-agent/issues/128) step 2) ([#255](https://github.com/go-steer/core-agent/pull/255))
- Digest: `pkg/digest` skeleton — Process + router + structural JSON pruner ([#128](https://github.com/go-steer/core-agent/issues/128) step 1) ([#250](https://github.com/go-steer/core-agent/pull/250))
- Attach: per-turn cost on usage-update so remote TUI's per-turn footer renders ([#249](https://github.com/go-steer/core-agent/pull/249))
- Attach: per-turn `UsageMetadata` with cache attribution on `/sessions/<id>/usage` (closes [#222](https://github.com/go-steer/core-agent/issues/222)) ([#248](https://github.com/go-steer/core-agent/pull/248))
- Attach: `DELETE /sessions/{sid}` + `/sessions/{app}/{sid}` endpoint (closes [#81](https://github.com/go-steer/core-agent/issues/81)) ([#244](https://github.com/go-steer/core-agent/pull/244))

#### Bug or Regression
- CI: version-fallback presubmit needs `fetch-depth: 0`, not `fetch-tags: true` ([#245](https://github.com/go-steer/core-agent/pull/245))

#### Documentation
- Cost-reduction plan + split version-fallback workflow to unblock docs PRs ([#247](https://github.com/go-steer/core-agent/pull/247))

#### Other (Cleanup)
- Version: bump fallback to `v2.7.0-dev` + add presubmit to enforce next-target pattern ([#242](https://github.com/go-steer/core-agent/pull/242))


## [2.7.0-dev.2] — 2026-07-14

**Session switcher + reliability tweaks.** Pre-release cut. Adds the `SessionSwitcher` on the remote TUI (upgrades to core-tui v0.10.0 for the new `SwitchTo` capability) and hardens the k8s-event-watcher's dedup to survive informer re-lists. Gemini bare-STOP empty responses now auto-retry once. Overlay image pinning is documented and `:latest` no longer floats to pre-release tags.

### Changes by Kind

#### Feature
- coretuiremote: `SessionSwitcher` + upgrade `/new` to core-tui v0.10.0 `SwitchTo` ([#241](https://github.com/go-steer/core-agent/pull/241))

#### Bug or Regression
- k8s-event-watcher: dedup by k8s `Event.LastTimestamp` to survive informer re-list ([#240](https://github.com/go-steer/core-agent/pull/240))
- Gemini: detect bare-STOP empty responses + auto-retry once ([#239](https://github.com/go-steer/core-agent/pull/239))

#### Other (Cleanup)
- Overlay image-pinning patterns + guard `:latest` against pre-release tags ([#238](https://github.com/go-steer/core-agent/pull/238))


## [2.7.0-dev.1] — 2026-07-13

**Post-v2.6.0 stabilization + observability foundation + v2.7 design docs.** First pre-release cut after v2.6.0. Lands W3C `traceparent` propagation + otelhttp wrappers as the foundation for full observability (issue [#217](https://github.com/go-steer/core-agent/issues/217)), a startup config summary so operators can see what the daemon actually loaded ([#212](https://github.com/go-steer/core-agent/issues/212) part 1), plus a raft of post-v2.6 fixes: dedup key canonicalization in k8s-event-watcher ([#219](https://github.com/go-steer/core-agent/issues/219)), gemini silent-empty-response detection ([#220](https://github.com/go-steer/core-agent/issues/220)), plan-first gate exemptions for read-only tools ([#213](https://github.com/go-steer/core-agent/issues/213)), and gke-troubleshoot recipe demo-readiness. Also lands the two design docs that unblock v2.7 features: MCP Streamable HTTP + OAuth 2.0 client (`docs/mcp-oauth-design.md`, [#190](https://github.com/go-steer/core-agent/issues/190)) and the native alert tool for headless escalation (`docs/alert-tool-design.md`, [#192](https://github.com/go-steer/core-agent/issues/192)).

### Changes by Kind

#### Feature
- Observability: W3C `traceparent` propagation + otelhttp wrappers ([#217](https://github.com/go-steer/core-agent/issues/217) foundation) ([#237](https://github.com/go-steer/core-agent/pull/237))
- k8s-event-watcher: log successful injects + dedup suppressions ([#212](https://github.com/go-steer/core-agent/issues/212) part 2) ([#233](https://github.com/go-steer/core-agent/pull/233))
- core-agent: startup config summary — surface what the daemon actually loaded ([#212](https://github.com/go-steer/core-agent/issues/212) part 1) ([#232](https://github.com/go-steer/core-agent/pull/232))
- CI: e2e recipe smoke test on kind for gke-troubleshoot-agent (closes [#211](https://github.com/go-steer/core-agent/issues/211)) ([#231](https://github.com/go-steer/core-agent/pull/231))

#### Bug or Regression
- k8s-event-watcher: canonicalize reason in dedup key to collapse family variants (closes [#219](https://github.com/go-steer/core-agent/issues/219)) ([#236](https://github.com/go-steer/core-agent/pull/236))
- Gemini: surface silent empty responses as `ErrEmptyResponse` (closes [#220](https://github.com/go-steer/core-agent/issues/220)) ([#235](https://github.com/go-steer/core-agent/pull/235))
- `record_plan`: route `MarkPlanRecorded` through per-session sub-gate (closes [#214](https://github.com/go-steer/core-agent/issues/214)) ([#234](https://github.com/go-steer/core-agent/pull/234))
- modeltier: reclassify `gemini-3.5-flash` as `TierMid` (closes [#210](https://github.com/go-steer/core-agent/issues/210)) ([#229](https://github.com/go-steer/core-agent/pull/229))
- CLI: add `--config` as long-form alias for `-c` (closes [#209](https://github.com/go-steer/core-agent/issues/209)) ([#228](https://github.com/go-steer/core-agent/pull/228))
- Permissions: exempt read-only introspection tools from plan-first gate (closes [#213](https://github.com/go-steer/core-agent/issues/213)) ([#227](https://github.com/go-steer/core-agent/pull/227))
- Instruction: expose `Searched` paths + log missing `AGENTS.md` at startup (closes [#218](https://github.com/go-steer/core-agent/issues/218)) ([#226](https://github.com/go-steer/core-agent/pull/226))
- gke-troubleshoot-agent: wire GKE MCP + complete WIF setup (recipe demo-readiness) ([#207](https://github.com/go-steer/core-agent/pull/207))
- Release: use herestring instead of pipe in CHANGELOG fallback ([#199](https://github.com/go-steer/core-agent/pull/199))
- Release: support retroactive tagging end-to-end ([#198](https://github.com/go-steer/core-agent/pull/198))

#### Documentation
- v2.6: GKE-troubleshoot demo runbook + recipe demo-readiness fixes ([#205](https://github.com/go-steer/core-agent/pull/205))
- MCP Streamable HTTP + OAuth 2.0 client design (closes [#190](https://github.com/go-steer/core-agent/issues/190) discussion) ([#191](https://github.com/go-steer/core-agent/pull/191))
- Native alert tool for headless escalation (closes [#192](https://github.com/go-steer/core-agent/issues/192) discussion) ([#193](https://github.com/go-steer/core-agent/pull/193))


## [2.6.0] — 2026-07-10

**Semi-autonomous Kubernetes triage agent.** Turns `core-agent` into a reactive first-responder for the top 10 real-world k8s failure modes (CrashLoopBackOff, ImagePullBackOff, OOMKilled, FailedMount, FailedScheduling, BackOff, Unhealthy, NetworkNotReady, NodeNotReady, Evicted). A new `k8s-event-watcher` sidecar streams filtered Kubernetes Events into per-incident sessions on the daemon; a router-shaped triage skill drives diagnose → fix → verify loops using the GKE MCP for cluster + workload operations; plan-first gates every mutation; structured audit trails land in the eventlog. Ships as a fourth signed container image (`ghcr.io/go-steer/k8s-event-watcher:2.6.0`) alongside the existing core-agent images. Substrate leans entirely on v2.4 (multi-session + `POST /sessions` + proxy pattern) and v2.5 (session resume) — zero new daemon primitives.

Turnkey escalation (ε.3) was dropped from v2.6 because the distroless image has no `bash` / `curl`; the router closes incidents with structured `INCIDENT SUMMARY` blocks in the eventlog. A native distroless-safe `alert` tool ([#192](https://github.com/go-steer/core-agent/issues/192)) and MCP-OAuth support for Slack's official MCP ([#190](https://github.com/go-steer/core-agent/issues/190)) both ship in v2.7.

Full recipe: `examples/gke-troubleshoot-agent/` · Design: `docs/k8s-event-agent-design.md` · Operator reference: `docs/site/content/docs/reference/troubleshooting-agent.md` · Tracking: [#186](https://github.com/go-steer/core-agent/issues/186).

### Changes by Kind

#### Feature
- k8s-event-watcher sidecar core binary — `cmd/k8s-event-watcher`, ~700 LoC using `k8s.io/client-go` Event informer, allow-list filtering on `Event.Reason`, `(uid, reason)` dedup with LRU eviction at 10k entries, optional `--dedup-persist PATH` restart resilience, per-incident sessions via `POST /sessions` + `X-Asserted-Caller` OR shared-session mode via `--target-session`, Prometheus counters + gauges on `--metrics-addr` ([#188](https://github.com/go-steer/core-agent/pull/188))
- k8s-triage router skill + `examples/gke-troubleshoot-agent/` recipe — one router owning envelope framing + fix-and-verify loop + escalation + budget tracking, 12 per-reason reference files lazy-loaded via ADK's `load_skill_resource`, `_fallback.md` for unknown reasons ([#189](https://github.com/go-steer/core-agent/pull/189))
- k8s-event-watcher container image (fourth image in the release pipeline) + v2.6 docs finalization — same distroless base, cosign keyless signing, multi-arch amd64+arm64 publishing on every `v*.*.*` tag ([#194](https://github.com/go-steer/core-agent/pull/194))

#### Documentation
- k8s-event-driven troubleshooting agent design ([#187](https://github.com/go-steer/core-agent/pull/187))
- Changelog split: `[Unreleased]` → `[2.4.0]` / `[2.5.0]` / `[2.6.0]` ([#195](https://github.com/go-steer/core-agent/pull/195))

#### Bug or Regression
- Release: fall back to `origin/main` CHANGELOG when tag predates entry — supports retroactively tagging historical commits ([#196](https://github.com/go-steer/core-agent/pull/196))
- Gitignore: add `/RELEASE_NOTES.md` ([#197](https://github.com/go-steer/core-agent/pull/197))

## [2.5.0] — 2026-07-02

**Session resume on daemon restart.** Sessions created via `POST /sessions` now survive `core-agent` restarts (config change, image upgrade, K8s pod replacement, crash). A TUI reconnecting after restart resumes transparently — same `SessionID`, same conversation history from the eventlog, same ACL. No `--new-session` fallback, no lost context. Cost: one DB query + one `agent.New` on the first reconnect (~50 ms); subsequent requests hit memory. Concurrent resume of the same session is deduplicated via `singleflight`.

Adds a background idle-eviction sweep bounded by the new `attach.multi_session.session_idle_timeout` config knob (default 24h; `"0s"` disables) so memory stays bounded — evicted sessions remain resumable from disk on next lookup. `GET /sessions` extended to union in-memory registry entries with persisted-ACL rows the caller can read (`sessionDescriptor` gains `Status: "active" | "idle"` + `LastTouchedAt` fields) so operators post-restart see their sessions immediately — clicking in triggers the lazy resume.

Backward compat: single-user deployments see zero behavior change (resume machinery only wires when `multi_session.enabled: true`); legacy `Register` sessions have no ACL row and stay non-resumable (matches "ACL row exists ⟺ session is resumable"); operators wanting pre-v2.5 behavior can set `session_idle_timeout: "0s"` to disable the sweep.

Design: `docs/session-resume-design.md` · Operator reference: `docs/site/content/docs/reference/multi-session.md` §"Session resume (v2.5+)" · Tracking: [#178](https://github.com/go-steer/core-agent/issues/178).

### Changes by Kind

#### Feature
- SessionACLStore — persist ACL rows for daemon-restart resume (ε.1) — new `pkg/attach.SessionACLStore` interface + GORM-backed impl over `agent_session_acl` table sharing the eventlog DB; `RegisterOwned` writes rows transactionally with in-memory registration and rolls back the memory insert on store failure ([#182](https://github.com/go-steer/core-agent/pull/182))
- SessionResumer — lazy resume on Lookup miss (ε.2) — new `pkg/attach.SessionResumer` interface with `cmd/core-agent` `buildSessionResumer` + shared `reproduceAgent` helper that both create-path and resume-path use for identical construction; `Registry.Lookup` / `LookupSingle` consult the resumer on miss with per-triple singleflight dedup; "no ACL row" translates to 404 (deliberate side-channel guard against SessionID probing) ([#183](https://github.com/go-steer/core-agent/pull/183))
- Idle eviction sweep + cancel-on-evict (ε.3) — new `attach.multi_session.session_idle_timeout` config knob (`""` → 24h default, `"0s"` disables); background eviction started by `Server.Bind` and stopped by `Close`; per-entry `LastTouchedAt` bumped lock-free on every Lookup memory-hit AND every broadcaster event pump so long-running autonomous work keeps a session non-idle; per-session `cancelOnEvict` stops the wake loop cleanly on eviction so the goroutine doesn't leak ([#184](https://github.com/go-steer/core-agent/pull/184))
- Session-resume docs + smoketest + `GET /sessions` union (ε.4) — Hugo reference page §"Session resume (v2.5+)"; new `dev/smoke/10-multi-session-resume.sh` end-to-end kill-restart smoketest asserting alice's session resumes with intact eventlog + ACL survives + bob still can't see it; `GET /sessions` unions in-memory registry with persisted-ACL rows the caller can read ([#185](https://github.com/go-steer/core-agent/pull/185))

#### Documentation
- Session-resume-on-daemon-restart design ([#181](https://github.com/go-steer/core-agent/pull/181))

### Breaking changes (internal-only surface)

Consumed only by `cmd/core-agent` and internal test doubles — no external library-consumer break expected. All three interfaces shipped in v2.4.

- `attach.SessionFactory` return type gained a `context.CancelFunc` between `Registrant` and `error`.
- `attach.SessionResumer.Resume` return signature is now `(Registrant, auth.SessionACL, context.CancelFunc, error)`.
- `attach.SessionRegistry.Lookup` / `LookupSingle` now take a leading `ctx context.Context` argument.
- New: `attach.SessionRegistry.RegisterOwnedWithCancel(ag, owner, cancelOnEvict)` alongside existing `RegisterOwned` (which delegates with nil cancel).

## [2.4.0] — 2026-06-13

**Multi-session daemon.** One `core-agent` daemon now safely serves multiple concurrent sessions belonging to different users — each with its own identity, ACL, permission grants, instruction overlays, and audit attribution. Opt-in and strictly backward-compatible: deployments that don't set `multi_session.enabled: true` see identical single-user behavior end-to-end. Substrate work split across α.1 (new `pkg/auth` skeleton — `Caller`, `Authenticator`, `BearerTokenAuth`, `AnonymousAuth`, `Authorize`, `LoadUsersFile` with 0600 file-mode enforcement), α.2 (caller threading through `Agent.InjectAs`, `SessionACL{Owner, Viewers, Contributors}`, full `Authorize` enforcement with 404-on-deny to avoid leaking session existence, proxy role + `X-Asserted-Caller` for chat-bot integrations, eventlog Metadata sidecar carrying `caller` + `proxy_by`), β (`Gate.DeriveForSession` per-session sub-gates), and γ (per-caller `instruction.LoadForSession` overlay path, always-derive wiring in `cmd/core-agent`, attach `Options` populated from config).

Ships with `POST /sessions` on-demand session creation for chat-bot / multi-operator deployments, an operator-facing `--ui` flag serving embedded mast-web at `/ui/*` on the attach listener, TUI flag-order tolerance, per-session wake-loop drivers for factory-created sessions, on-demand-session `AttachXProvider` closures for `/memory` / `/skills` / `/mcp` / `/pricing` slashes, per-session tool-gate routing for MCP tool calls + `/interrupt`, and a GoReleaser-driven binary release path. Plus the substrate for the v2.5 session-resume work: task-class profiles, cost ceilings, per-model-tier compaction threshold, behavioral watchdog observer, transparent MCP wrapping, agent-card discovery endpoint, and two new deploy recipes (Cloud Run + GKE).

Design: `docs/multi-session-design.md` · Operator reference: `docs/site/content/docs/reference/multi-session.md` · Recipe: `examples/multi-session-bearer/` · Additional designs: `docs/pkg-digest-design.md` (via [#137](https://github.com/go-steer/core-agent/pull/137)) · Tracking: [#162](https://github.com/go-steer/core-agent/issues/162).

### Changes by Kind

#### Feature
- `pkg/auth` skeleton + attach Authenticator wiring (α.1 of [#162](https://github.com/go-steer/core-agent/issues/162)) — new `Caller`, `Authenticator`, `BearerTokenAuth`, `AnonymousAuth`, `Authorize`, `LoadUsersFile` with 0600 file-mode enforcement ([#163](https://github.com/go-steer/core-agent/pull/163))
- Caller threading + SessionACL + Proxy role + EventMeta sidecar (α.2 of [#162](https://github.com/go-steer/core-agent/issues/162)) — `Agent.InjectAs(message, caller)`, `SessionACL{Owner, Viewers, Contributors}`, 404-on-deny in attach handlers, proxy role + `X-Asserted-Caller` for chat-bot integrations, eventlog Metadata sidecar carrying `caller` + `proxy_by` per event ([#169](https://github.com/go-steer/core-agent/pull/169))
- `Gate.DeriveForSession` + per-session sub-gates (β of [#162](https://github.com/go-steer/core-agent/issues/162)) — isolate `sessionAllow` / `sessionAllowTools` / `sessionAllowVerbs` / `planRecorded` / `approvals` / `mode` / `prompter` while sharing daemon-wide `Policy` / `PathScope` / `RequirePlanArtifact` ([#166](https://github.com/go-steer/core-agent/pull/166))
- Per-caller instruction overlays + MCP caller context (γ of [#162](https://github.com/go-steer/core-agent/issues/162)) — `instruction.LoadForSession` with path-traversal-safe identity validation, always-derive wiring in `cmd/core-agent` so single-user and multi-user paths share one shape, `eventlog.WithMetadataExtractor` populated from per-request `auth.Caller` ([#167](https://github.com/go-steer/core-agent/pull/167))
- `POST /sessions` endpoint + TUI affordances for on-demand session creation — chat-bot integrations and multi-operator platform-agent deployments create per-caller sessions programmatically ([#171](https://github.com/go-steer/core-agent/pull/171))
- `--ui` flag serves mast-web operator UI at `/ui/*` on the attach listener ([#170](https://github.com/go-steer/core-agent/pull/170))
- GoReleaser-driven binary releases + auto-publish GitHub Release on tag push ([#164](https://github.com/go-steer/core-agent/pull/164))
- Wire core-tui `/theme` picker through to `ui.theme` config ([#155](https://github.com/go-steer/core-agent/pull/155))
- Warn/refuse on small-tier parent in interactive sessions (closes #121)
- Behavioral watchdog observer (#123 PR 2) ([#150](https://github.com/go-steer/core-agent/pull/150))
- `--task` flag + task-class profile table (closes #123 PR 1) ([#149](https://github.com/go-steer/core-agent/pull/149))
- Per-turn / per-session cost ceiling kill switch (closes #145) ([#148](https://github.com/go-steer/core-agent/pull/148))
- Cloud Run deploy recipe ([#113](https://github.com/go-steer/core-agent/pull/113))
- `--auth=google-id-token` for Cloud Run IAM (closes #135) ([#143](https://github.com/go-steer/core-agent/pull/143))
- Accept attach token via `X-Attach-Token` header (closes #112) ([#141](https://github.com/go-steer/core-agent/pull/141))
- Consume SSE protocol v1.1.0 typed events in remote-TUI adapter ([#139](https://github.com/go-steer/core-agent/pull/139))
- Nudge plan-before-acting in `DefaultInstruction` ([#136](https://github.com/go-steer/core-agent/pull/136))
- Wire core-tui v0.9.0 Notifier + surface MCP startup failures ([#138](https://github.com/go-steer/core-agent/pull/138))
- Per-model-tier compaction threshold ([#127](https://github.com/go-steer/core-agent/pull/127))
- Provider-aware default for `--agentic-small-model` (closes #122) ([#126](https://github.com/go-steer/core-agent/pull/126))
- Default `--agentic-tools` to on; soften verify-loop wording ([#118](https://github.com/go-steer/core-agent/pull/118))
- Emit typed SSE events per protocol spec (closes #115) ([#117](https://github.com/go-steer/core-agent/pull/117))
- `examples/gke-deploy/` — config-only GKE deployment recipe using Workload Identity Federation for GKE direct binding (no GSA in the middle); `deploy/base/` + `deploy/overlays/example/` kustomize pattern; registers with Google Cloud's Agent Registry via `apphub.cloud.google.com/functional-type: "AGENT"`; four attach paths documented (Cloud Workstations / IAP / VPN / port-forward); plan-first OFF by default ([#109](https://github.com/go-steer/core-agent/pull/109))
- Publish `/.well-known/agent-card.json` for discovery

#### Bug or Regression
- Multi-session: route tool calls through per-session gate + propagate cancel — `pkg/tools/gate.go` correctness fix surfaced by γ review; `gatedTool.Run` now threads inbound `adktool.Context` through to `gate.CheckGeneric` instead of `context.Background()`, so audit logs see the caller for MCP tool calls and per-session prompter sees `SubagentSource` ([#177](https://github.com/go-steer/core-agent/pull/177))
- Multi-session: wire `AttachXProvider` closures into on-demand sessions ([#176](https://github.com/go-steer/core-agent/pull/176))
- Multi-session: drive factory-created sessions via per-session wake loop ([#175](https://github.com/go-steer/core-agent/pull/175))
- `core-agent-tui`: permute `os.Args` so flag order doesn't matter ([#174](https://github.com/go-steer/core-agent/pull/174))
- `coretui-remote`: shorten `/new` SystemMessage so URL survives narrow terminals ([#172](https://github.com/go-steer/core-agent/pull/172))
- `coretui`: commit usage once per `TurnComplete`, not per `UsageMetadata` event ([#156](https://github.com/go-steer/core-agent/pull/156))
- Auto-continue inbox framing — dedup + post-completion branches (closes #144) ([#147](https://github.com/go-steer/core-agent/pull/147))
- Agentic: assert digest authority + forbid bare-tool verify (closes #59)
- Agentic: drop `agentic_grep` subtask budget to 2 turns (closes #60)
- Agent: record summarizer + `/btw` LLM calls in usage tracker (closes #61)

#### Documentation
- Multi-session Hugo reference + recipe + end-to-end smoketest (δ of [#162](https://github.com/go-steer/core-agent/issues/162)) ([#168](https://github.com/go-steer/core-agent/pull/168))
- Multi-session core-agent design — per-user auth + cross-session isolation (v2.4) ([#105](https://github.com/go-steer/core-agent/pull/105))
- Per-MCP-server credential resolution design — pluggable providers + Auth Manager (v2.4) ([#106](https://github.com/go-steer/core-agent/pull/106))
- Friction log for deploying Go agents to Agent Engine / Agent Runtime ([#114](https://github.com/go-steer/core-agent/pull/114))
- Design docs for task-class model selection and transparent MCP wrapping ([#125](https://github.com/go-steer/core-agent/pull/125))
- `pkg/digest` design + Headroom-inspired addendum to MCP wrap ([#137](https://github.com/go-steer/core-agent/pull/137))
- Document the three-layer instruction composition pattern ([#146](https://github.com/go-steer/core-agent/pull/146))
- Correct image tag convention in README + CHANGELOG (no leading `v`)

#### Other (Cleanup)
- Conform smoketest to NN-prefix + add gen-users-json tool ([#173](https://github.com/go-steer/core-agent/pull/173))
- Bump core-tui v0.9.0 → v0.9.1 ([#140](https://github.com/go-steer/core-agent/pull/140))
- Bump core-tui v0.6.9 → v0.9.0 ([#133](https://github.com/go-steer/core-agent/pull/133))
- Bump dev version v2.3.1 → v2.4.0-dev

### Breaking changes

Interface-shape change on `attach.Registrant`. The `Agent.Inject(message)` shape itself is preserved for back-compat — no library-consumer break for the common case.

- `attach.Registrant` interface gained an `InjectAs(message string, caller auth.Caller) error` method alongside the existing `Inject(message string) error`. `*agent.Agent` implements both (`Inject` is now a shim calling `InjectAs` with a zero `auth.Caller`); external implementors need to add the new method.

## [2.3.1] — 2026-06-04

**Container image release pipeline.** Patch release that ships only release-infrastructure — no code changes to the binary surface. Closes the v2.2.0 deferred "publish container images" item. Three multi-arch (amd64 + arm64) images now publish to GitHub Container Registry on every tag push, signed via Sigstore keyless. Operators deploying core-agent to K8s / Cloud Run / Nomad can pin `image: ghcr.io/go-steer/core-agent:2.3.1` (semver tag without the leading `v` — matches the Docker / Helm appVersion convention) instead of building their own. Unblocks the `examples/gke-deploy/` recipe in v2.4.

### Changes by Kind

#### Feature
- Container image release pipeline (`Dockerfile` + `.github/workflows/release-images.yml`) — three multi-arch images (linux/amd64 + linux/arm64) publish to GHCR on every `v*.*.*` tag push plus a floating `:main-<sha>` on every main push: `ghcr.io/go-steer/core-agent:<tag>` (full build with in-process bubble-tea TUI), `ghcr.io/go-steer/core-agent-slim:<tag>` (`-tags no_tui`, ~5MB smaller, for headless K8s deployments), `ghcr.io/go-steer/core-agent-tui:<tag>` (remote TUI client only); all on `gcr.io/distroless/static-debian12:nonroot` (pure-Go binary, no shell, runs as UID 65532); signed via Sigstore keyless (GitHub Actions OIDC → Fulcio short-lived cert → Rekor transparency log; verify with `cosign verify ghcr.io/go-steer/core-agent:<tag> --certificate-identity-regexp '^https://github.com/go-steer/core-agent' --certificate-oidc-issuer https://token.actions.githubusercontent.com`); builder image is `golang:${GO_VERSION}-alpine` where `GO_VERSION` is parsed from `go.mod` at build time so bumping `go.mod` automatically bumps the build toolchain ([#108](https://github.com/go-steer/core-agent/pull/108))

#### Other (Cleanup)
- Bump dev version v2.3.0 → v2.4.0-dev

## [2.3.0] — 2026-06-04

**Ecosystem migration on-ramp + substrate-enforced plan-first.** Two headline features make this release: a multi-file instruction loader (`@include` + `AGENTS.d/` overlay directory) that drops in compatibly with the AGENTS.md / SOUL.md / governance-SOPs shape every adjacent agent framework already uses (Cursor, Antigravity, Hermes), and gate-level plan-first enforcement (`record_plan` built-in + `RequirePlanArtifact` opt-in + `/replan` slash) that turns "research → approve plan → execute" from a prompt-honored convention into a substrate primitive. Both target the same operator: someone moving a real distributed-agent workload onto core-agent who wants their existing Layer-0 markdown to load unchanged AND wants the agent genuinely incapable of touching the world before a written plan is approved.

Plus a new Examples index page (`docs/examples/`) catalogs every shipped recipe + library quickstart so operators can pick a starting point by use case. Two recipes ship: `examples/gke-parallel-triage/` (GKE incident triage via parallel `spawn_agent` fan-out, MCP-integrated, config-only) and `examples/plan-first/` (three `config.json` variants × `ask` / `acceptEdits` / `yolo` composition with `require_plan_artifact`). Deferred to v2.4: multi-session + per-user auth (task #12), per-MCP-server credential resolution with Auth Manager (task #13), plan-progress tracking (task #9).

Designs: `docs/instruction-loader-v2-design.md`, `docs/plan-first-design.md`, `docs/kube-agents-platform-fit.md`, `docs/scion-core-agent-architecture.md` · Manual UAT walkthrough: `docs/v2.3-smoketest.md`.

### Changes by Kind

#### Feature
- Plan-first gating (`pkg/permissions` + `pkg/tools/record_plan`) — new opt-in `permissions.require_plan_artifact: true` config flag denies mutating tools (`write_file` / `edit_file` / `delete_file` / `bash`, `spawn_agent` family, all MCP tools) until the model calls the new `record_plan` built-in, which persists the plan to `.agents/plans/plan-<seq>.md` and flips a per-session gate flag; read tools + `record_plan` itself remain unblocked so research happens normally; composes with every existing mode (`ask + require_plan_artifact` gives per-call approval after the plan, `acceptEdits + require_plan_artifact` auto-allows writes, `yolo + require_plan_artifact` auto-allows everything after the plan); new `/replan` slash (in-process TUI + `POST /sessions/<sid>/slash/replan`) clears the gate flag and renames the latest plan to `plan-<seq>-revoked.md` for audit; spawned subagents inherit the parent's `planRecorded` flag ([#100](https://github.com/go-steer/core-agent/pull/100))
- Multi-file instruction loader (`pkg/instruction` v2) — `@include <relative-path>` directive replaced in-place by the referenced file's content (relative-to-containing-file resolution, `../` allowed up to scope root, absolute paths + URLs rejected, recognized only on its own line outside fenced code blocks, missing target is a load error so operator typos surface immediately); `AGENTS.d/*.md` overlay directory loaded in lexical filename order after the scope's primary file (Linux conf.d convention); per-Load canonical-path dedup so the same file reached via any path (`@include` + directory entry, cycle A → B → A, cross-scope symlink) is read exactly once; YAML frontmatter stripping so editor metadata doesn't leak; UTF-8 validation + 32 KiB per-file truncation with visible `[...truncated by core-agent at 32768 bytes...]` marker; source provenance preserved in `Loaded.Sources` with canonical path + byte count + truncation flag; backwards-compatible for existing single-file AGENTS.md ([#98](https://github.com/go-steer/core-agent/pull/98))
- `examples/gke-parallel-triage/` — config recipe for GKE incident fan-out via parallel `spawn_agent`, MCP-integrated ([#95](https://github.com/go-steer/core-agent/pull/95))

#### Bug or Regression
- v2.3.0 blocker: instruction loader also searches `<root>/.agents/AGENTS.md` and `<root>/.agents/AGENTS.d/` (in addition to `<root>/AGENTS.md` and `<root>/AGENTS.d/`) so operators following the "everything agent-related lives under `.agents/`" convention get their content loaded without restructuring (both locations load additively; per-load canonical-path dedup ensures no file loads twice); instruction-load errors (malformed `@include`, escaped path, missing target, non-UTF-8 content) now exit with `ExitConfigError` instead of degrading silently to a truncated system prompt; smoketest correctness ([#107](https://github.com/go-steer/core-agent/pull/107))

#### Documentation
- Instruction loader v2 design — multi-file composition for migration on-ramp ([#96](https://github.com/go-steer/core-agent/pull/96))
- Instruction-loader-v2 open questions resolved + per-Load dedup added ([#97](https://github.com/go-steer/core-agent/pull/97))
- Plan-first enforcement design ([#99](https://github.com/go-steer/core-agent/pull/99))
- Examples index page cataloging every recipe + library quickstart ([#101](https://github.com/go-steer/core-agent/pull/101))
- Real-world fit analysis: running core-agent as kube-agents Platform Agent ([#102](https://github.com/go-steer/core-agent/pull/102))
- Scion + core-agent layered architecture analysis ([#103](https://github.com/go-steer/core-agent/pull/103))

#### Other (Cleanup)
- Cut v2.3.0 — multi-file instruction loader + plan-first gating

### Breaking changes

- `tools.Build` signature gained an `agentsDir string` parameter between `gate` and `b`; callers passing `""` get the prior behavior (no `record_plan` registered, no other change).

## [2.2.0] — 2026-06-01

**Observer-mode remote TUI + the operator-surface gaps v2.1 left open.** This release closes every v2.2-tracked limitation the v2.1 smoke doc flagged: the remote `core-agent-tui` now renders autonomous agent activity continuously via `coretui.LiveAgent`, surfaces tool-approval modals over HTTP, executes `/reload` server-side with per-surface feedback, and fails loudly on `--attach-listen` bind failure instead of silently degrading. Both binaries grew a standard `--version` flag.

core-tui bumped v0.6.3 → v0.6.9 to pick up three upstream render-coalescence fixes ([core-tui#24](https://github.com/go-steer/core-tui/issues/24), [core-tui#26](https://github.com/go-steer/core-tui/issues/26), [core-tui#28](https://github.com/go-steer/core-tui/issues/28)) found during smoke that together make the LiveAgent path actually paint without operator keystrokes. Deferred for follow-up: multi-session daemon for "+ New session" (task #4), MCP `ask_user` / elicit round-trips, live MCP-server restart on `/reload`, request-id correlation.

Design history: `docs/remote-tui-observer-mode.md` + `docs/remote-tui-on-core-tui.md` · Manual UAT: `docs/remote-tui-smoketest-v2.2.md`.

### Changes by Kind

#### Feature
- Observer mode for the remote TUI (`coretui.LiveAgent`) — `internal/coretuiremote.Adapter` implements `coretui.LiveAgent.Events(ctx)` (core-tui v0.6.6, issue #22) for continuous SSE drain of the remote eventlog; `RunAutonomous`, scheduled background subagents, MCP-server-triggered activity, and other clients' injects now surface in the chat scrollback instead of leaving it blank between operator-driven turns; on disconnect (daemon restart, network drop) the iterator auto-reconnects with exponential backoff and resumes from the last-seen sequence so history isn't re-replayed; core-tui bumped v0.6.3 → v0.6.9 for [core-tui#24](https://github.com/go-steer/core-tui/issues/24), [core-tui#26](https://github.com/go-steer/core-tui/issues/26), [core-tui#28](https://github.com/go-steer/core-tui/issues/28) render fixes ([#88](https://github.com/go-steer/core-agent/pull/88))
- HTTP-driven permission prompts for the remote TUI — `pkg/attach.PromptBroker` bridges the daemon's `permissions.Gate` to two new endpoints (`GET /sessions/<sid>/perms/stream` SSE feed of pending prompts, `POST /sessions/<sid>/perms/respond` operator decision); `internal/coretuiremote.StartRemotePrompter` runs the local-side bridge pushing each frame into a `coretui.Prompter` modal; new `attach.PromptBrokerProvider` capability + `agent.WithAttachPromptBroker` option; without the option the routes return 501; wire-format decision strings (`deny` / `allow-once` / `allow-session` / `allow-session-verb` / `allow-session-tool` / `allow-always`) and prompt kinds (`bash` / `file_write` / `path_scope` / `generic`) documented at `pkg/attach/types_prompts.go`; MCP `ask_user` / elicit round-trips deferred to a follow-up ([#87](https://github.com/go-steer/core-agent/pull/87))
- `agent.WithAttachReloader` + `/reload` server-side action — replaces the v2.1 stub with a best-effort re-walk over the agent's project deps; verifies `instruction.Load` + `skills.LoadAll` + `mcp.Load` and reports per-surface outcomes in `ReloadResponse.Errors` (`memory ✓ · skills ✓ · mcp ⚠ live restart not supported`); the in-process TUI's `/reload` delegates to the same `agent.AttachReload` path; live MCP-server restart and system-prompt rebuild remain out of scope (require reconstructing the running agent) ([#86](https://github.com/go-steer/core-agent/pull/86))
- `--version` flag on `core-agent` and `core-agent-tui` — both binaries print `<prog> <version> (commit <sha>[, modified], built <date>)`; new `internal/version` package centralizes format + ldflags overrides; plain `go build` reports `vX.Y.Z-dev` plus SHA + dirty flag + commit time auto-embedded by Go's `-buildvcs=true` default; release-time builds inject the real tag via `-ldflags "-X .../version.Version=v2.2.0 -X .../version.Commit=$(git rev-parse HEAD) -X .../version.Date=..."` — pattern lifted from `../cogo`

#### Bug or Regression
- `--attach-listen` now fails loudly on bind failure instead of degrading to REPL — previously starting a second `core-agent --attach-listen=:N` when port `:N` was already bound silently fell through to REPL mode and the operator's TUI kept talking to the OLD daemon; `pkg/attach.Server` now exposes `Bind()` (synchronous listener bind + TLS/http.Server setup) and `Serve()` (drives the bound listener) as a pair; `ListenAndServe` preserved as `Bind()` + `Serve()`; `cmd/core-agent/main.go` calls `Bind` in the main goroutine and exits with `ExitConfigError` on bind failure; regression test at `pkg/attach/bind_test.go` ([#85](https://github.com/go-steer/core-agent/pull/85))
- Match the remote TUI's multi-line `/reload` format ([#92](https://github.com/go-steer/core-agent/pull/92))
- Collapse nil-then-deref to satisfy staticcheck SA5011 ([#90](https://github.com/go-steer/core-agent/pull/90))

#### Documentation
- Refresh attach-tui + `/reload` docs for v2.1 + v2.2 state ([#93](https://github.com/go-steer/core-agent/pull/93))

#### Other (Cleanup)
- Drop golangci-lint cache for deterministic lint results ([#91](https://github.com/go-steer/core-agent/pull/91))
- Fold Unreleased into [2.2.0]; bump dev to v2.3.0-dev

## [2.1.0] — 2026-05-31

**Remote TUI on core-tui.** `cmd/core-agent-tui` is now a thin shell over `go-steer/core-tui` driven by a new `internal/coretuiremote` adapter — slash parity with the in-process TUI (`/stats`, `/context`, `/memory`, `/skills`, `/mcp`, `/pricing`, `/perms`, `/allow`, `/deny`, `/reload`, `/btw`, `/compact`, `/done`, `/subagent`, `/permissions` and the core-tui builtins), per-turn cost in the chat footer computed client-side from the server's pricing rates, status banner with cumulative tokens + cost, mid-turn injection queue panel. The lifted-from-cogo `internal/tui/` package and the `CORE_AGENT_TUI=internal` escape hatch are retired — core-tui is the only TUI codepath.

Net code reduction: `internal/tui/` ~7,000 LoC removed; `cmd/core-agent-tui/` shrunk from ~2,844 to ~565 LoC; five charm-bracelet direct dependencies dropped from `go.mod`. Sixteen new HTTP attach endpoints (six read-only state, six mutation, four async slash dispatchers) implemented across PRs A1/A2/A3 of `docs/remote-tui-on-core-tui.md`.

Design: `docs/remote-tui-on-core-tui.md` · Manual smoke checklist: `docs/remote-tui-smoketest.md` (with workarounds for the v2.2-tracked gaps: HTTP-driven prompter, observer mode + history-on-attach + mid-turn drain correlation, operator-facing session reset, attach-listen hard-fail on bind failure, server-side `/reload` action).

### Changes by Kind

#### Feature
- Attach-mode operator-state read endpoints (PR A1 of `docs/remote-tui-on-core-tui.md`) — six new read-only HTTP endpoints exposed by `pkg/attach`'s server, each available in qualified (`/sessions/<app>/<sid>/X`) and single-segment-shortcut (`/sessions/<sid>/X`) forms: `/usage`, `/context`, `/memory`, `/skills`, `/mcp`, `/pricing`; new capability interfaces on `Registrant` (`UsageProvider`, `ContextProvider`, `MemoryProvider`, `SkillsProvider`, `MCPProvider`, `PricingProvider`); `*agent.Agent` gains `AttachUsage()` + `AttachContext()`; the four caller-held capabilities surfaced via a new `attach.OperatorView` wrapper; `internal/attachclient` grows matching `Client.Usage`/`Context`/`Memory`/`Skills`/`MCP`/`Pricing` methods ([#78](https://github.com/go-steer/core-agent/pull/78))
- Remote TUI on core-tui — umbrella PR covering PR B (`internal/coretuiremote` adapter satisfying `coretui.Agent` plus a dozen capability interfaces, with turn-end detection via `session.Event.TurnComplete`, per-render usage cache with 2s TTL, sync + async slash dispatch), PR B-cmd (`cmd/core-agent-tui` rewrite to `coretui.Run(adapter)`, ~2,844 → ~565 LoC), PR C (retire `internal/tui/` and the `CORE_AGENT_TUI=internal` escape hatch), PR A2 (six operator-mutation endpoints: `GET /perms` + `POST /perms/allow` + `POST /perms/deny` + `POST /pricing/refresh` + `POST /pricing/set` + `POST /reload`; new `PermsProvider` / `PermsController` / `PricingController` / `Reloader` capabilities; new `attach.ErrCapabilityNotRegistered` sentinel), and PR A3 (four async slash dispatchers: `/slash/compact` + `/slash/done` + `/slash/btw` + `/slash/subagent`; new `attach.SubagentSpec` / `SubagentBudget` / `SubagentSpawnResponse` types; new `agent.ErrSubagentSpawnerUnavailable` sentinel mapped to HTTP 501); `/elicit/respond` deferred to a follow-up ([#83](https://github.com/go-steer/core-agent/pull/83))

### Breaking changes

Retirement of the in-tree bubble-tea TUI codepath. Operators of the remote `core-agent-tui` binary see the cutover; the in-process `core-agent` TUI already switched in v2.0.

- `cmd/core-agent-tui` is now a thin shell over `go-steer/core-tui` — parallel bubble-tea implementation (chat / queue / welcome / status / slash / model, ~2,300 LoC) is replaced by `coretui.Run` against `internal/coretuiremote`.
- `internal/tui/` and the `CORE_AGENT_TUI=internal` escape hatch removed; operators with the env var set get a silent fall-through to core-tui — the variable is no longer recognized.
- Direct `go.mod` dependencies removed: `github.com/charmbracelet/bubbles`, `glamour`, `x/ansi`, `x/exp/teatest`, `muesli/reflow` (all still pulled transitively by core-tui where needed).
- `--theme` semantics flip from "dark default" to "auto default" (passes through to `coretui.Options.ForceTheme`; empty = OSC-11 query). No flags removed.

## [2.0.0] — 2026-05-29

**Context management + core-tui default + `pkg/` reorg.** Two structural shifts make long-running agent sessions actually viable. (1) The context-management trilogy — automatic compaction at ~85% context utilization (Mechanism A), model-driven task-boundary checkpoints (Mechanism C), and subtask wrappers that route bulk tool output through a cheaper model (Mechanism B) — together let an agent run for hours or days without the operator-painful "input tokens climb until the next turn errors out" pattern. (2) The TUI defaults to core-tui (the source of truth for status-line, async-slash dispatch, mid-turn injection, per-model `/stats`); the legacy `internal/tui` stays available via `CORE_AGENT_TUI=internal` for one release.

Plus a full operator-docs redesign into Hugo sections (`cli` / `library` / `agent-design` / `reference` / `skills-library`), three bundled meta-skills that teach an agent how to configure another agent (`cli-setup`, `autonomous-setup`, `library-embedding`), global skill auto-discovery from `~/.core-agent/skills/`, and an all-public-packages-under-`pkg/` reshape. Pre-1.0 caveat: breaking changes are still possible at minor versions; the `config.ModelConfig.Pricing` shape changed from single `*PricingConfig` to per-model `PricingMap` mid-cycle, and package import paths all gained a `pkg/` prefix.

Designs: [`docs/context-management-design.md`](https://github.com/go-steer/core-agent/blob/main/docs/context-management-design.md), [`docs/operator-input-design.md`](docs/operator-input-design.md), [`docs/pricing-design.md`](docs/pricing-design.md), `docs/pkg-reorg-option-1.md`, core-tui adapter design · GKE multi-agent worked example based on [gke-labs/kube-agents](https://github.com/gke-labs/kube-agents) · Related core-tui: [`go-steer/core-tui#19`](https://github.com/go-steer/core-tui/pull/19) (SessionByModelTracker).

### Changes by Kind

#### Feature
- Move public packages under `pkg/` layout — `agent`, `attach`, `config`, `eventlog`, `instruction`, `mcp`, `models`, `permissions`, `recording`, `runner`, `session`, `skills`, `telemetry`, `tools`, `usage` now imported as `github.com/go-steer/core-agent/pkg/<name>`; `cmd/`, `internal/`, `examples/`, `extras/scion-agent`, `dev/`, `docs/`, `SKILLS/` stay at repo root; CLI binaries keep their install paths and behavior — only library consumers see the change ([#73](https://github.com/go-steer/core-agent/pull/73))
- Global skill auto-discovery from `~/.core-agent/skills/` — `skills.LoadAll(ctx, projectAgentsDir, userCoreHome, gate)` discovers skills from both project-scoped (`.agents/skills/`) and user-global sources and merges them; on name collision, project-scoped wins; small `overlayFS` composes the two `fs.FS` roots ([#71](https://github.com/go-steer/core-agent/pull/71))
- Three meta-skills bundle (`SKILLS/`) — Claude-Skills–shaped bundles at repo root teaching an agent how to configure and embed `core-agent` itself: `cli-setup` (four customization layers + permission posture), `autonomous-setup` (single-agent monitor OR multi-agent team), `library-embedding` (Go `agent.New` + seven extension points + HTTP-served example); each is a `SKILL.md` runbook + 3 focused references; new docs section `skills-library/` ([#69](https://github.com/go-steer/core-agent/pull/69))
- Flip TUI default to core-tui (`launchTUIv2`); `CORE_AGENT_TUI=internal` escape hatch for one release ([#62](https://github.com/go-steer/core-agent/pull/62))
- In-chat preamble row for long-running async slashes (closes [#55](https://github.com/go-steer/core-agent/issues/55)) — `/done`, `/compact`, `/btw` now surface a chat-visible system row at dispatch in addition to the bottom-bar toast; leverages core-tui v0.6.3's `AsyncSlashProviderWithPreamble` ([#57](https://github.com/go-steer/core-agent/pull/57))
- Micro-subagents + agentic tool wrappers — Mechanism B (PR II of context-management, v2.0 blocker) — new `Agent.RunSubtask(ctx, SubtaskSpec) (SubtaskResult, error)` primitive (synchronous, single-purpose, fresh-context LLM call against isolated llmagent + parent-prefixed session ID); `SubtaskSpec.Model` optional `adkmodel.LLM` override as the cost-efficiency lever; default budgets 5 turns / 60s wall-clock with `Truncated` flag; cost rolls up to parent's `usage.Tracker`; new `tools/agentic` sub-package with `AgenticReadFile`, `AgenticFetchURL`, `AgenticGrep`, `AgenticResearch` presets; new `agent.WithPostConstruct` for late binding; opt-in via `--agentic-tools` flag, `--agentic-small-model <id>` selects cheaper subtask model ([#52](https://github.com/go-steer/core-agent/pull/52))
- Task-boundary checkpoints — Mechanism C + PR-I polish folded in — new `Checkpointer` interface + `DefaultCheckpointer`; new `Agent.Checkpoint(ctx, taskNote)` shares summarizer/persistence/slicing with `Agent.Compact` via private `runSummarizer(spec)`; new `mark_task_done(detail)` built-in auto-registered when `WithCheckpointer` is wired; operator-driven `/done [note]` slash (alias `/checkpoint`); default-on in `cmd/core-agent/main.go`, disable via `--no-checkpoint`; kind-aware boundary framing + neutral checkpoint summary headings to prevent Gemini Flash re-running tools; new `Agent.HasCompactor()` / `HasCheckpointer()` predicates; `/context` slash + `Agent.ContextStats()` snapshot; per-model cost breakdown in `/context` + `/stats` (via core-tui v0.6.4's `SessionByModelTracker`, [core-tui#19](https://github.com/go-steer/core-tui/pull/19)) ([#50](https://github.com/go-steer/core-agent/pull/50))
- Compaction substrate — Mechanism A (PR I of context-management, v2.0 blocker) — new `Compactor` interface + `DefaultCompactor` (five-section "teammate handover" prompt + 0.85 utilization threshold); new `Agent.Compact(ctx, focus) (CompactionResult, error)` runs tool-less summarizer LLM call, writes result as `session.Event` with `CustomMetadata["compaction"] = "summary"`; `Agent.CompactIfNeeded` threshold-gated variant; post-`Run` threshold hook sets `compactionPending` flag drained on next `Run`; history slicing via `compactingService` wrapping runner's `session.Service`; audit log never mutated; new `WithCompactor(c Compactor)` and `WithUsageTracker(t *usage.Tracker)` options; new `/compact [focus]` slash (alias `/summarize`); strengthened summary framing wrapper for instruction-following-weak Gemini Flash ([#49](https://github.com/go-steer/core-agent/pull/49))
- `config.UIConfig` (theme + mouse) — new top-level `ui` block with `theme` (`auto` | `dark` | `light`, default `auto`) and `mouse` (bool, default `true`); defaults preserve prior behavior exactly ([#44](https://github.com/go-steer/core-agent/pull/44))
- Wire `cfg.UI.Theme` + `cfg.UI.Mouse` through to core-tui Options
- External-path allow-list with per-op access levels — `pkg/permissions` extension
- Emit tool RESULT events + bump core-tui to v0.3.0
- Surface Anthropic Claude models in the `/model` picker
- Tier 3 core-tui adapter — Provider in Status + SessionTurns/Duration on bridge + memory bytes/truncated
- Feed `usage.Tracker` + emit per-turn cost / model + nest MCP tools in the core-tui adapter
- Fill memory / MCP / skills / path-scope translators in the core-tui adapter
- Wire MCP elicit + ToolLister + SubagentLister + UsageTracker + AlwaysAllow in the core-tui adapter
- Wire prompter + ModelSwapper + PermissionController + `/btw` in the core-tui adapter
- Minimal `launchTUIv2` + `CORE_AGENT_TUI` env-var dispatch
- Wire `cfg.Agent.DisplayName` to core-tui `Branding.AgentIdentity` + bump v0.6.2
- `/subagent <goal> [flags]` slash command (PR γ of operator-input-design) — spawn a background subagent directly without going through the model; flags `--name`, `--prompt`, `--tools`, `--extras` (alias `--skill`), `--max-turns`, `--max-cost`, `--max-wallclock`, `--scheduler`; aliased `/sub` ([#34](https://github.com/go-steer/core-agent/pull/34))
- `/btw <question>` side queries (PR β of operator-input-design) — new `Agent.AskSideQuestion(ctx, question) (string, error)`: full session context, no tools, no event-log writeback, no permission gating; result lands in dismissible overlay; aliased `/by-the-way` ([#33](https://github.com/go-steer/core-agent/pull/33))
- Input-while-streaming + auto-continue from inbox (PR α of operator-input-design) — operators press Enter while a turn is streaming; typed text goes onto the agent's inbox via `Agent.Inject`; TUI-local queue panel mirrors pending items; when current turn completes with non-empty inbox, TUI auto-starts a follow-up with the queued notes wrapped in a system-note framing block; soft cap of 10 consecutive auto-continues; new `agent.FormatAutoContinueInbox` + `Agent.DrainInbox` + `Agent.PendingInboxCount` ([#30](https://github.com/go-steer/core-agent/pull/30))
- `-tags no_tui` slim build variant + remote-only `core-agent-tui` + `/interrupt` slash — `go build -tags no_tui ./cmd/core-agent` produces a binary omitting the in-process bubble-tea TUI tree (~8 MB / 14% smaller); `/interrupt` slash + `Agent.Interrupt()` + attach-mode `POST /sessions/<sid>/interrupt`; bound to Esc on empty input, aliased `/int` ([#29](https://github.com/go-steer/core-agent/pull/29))
- `/pricing refresh` + `/pricing set` slash commands (PR C of [`docs/pricing-design.md`](docs/pricing-design.md)) — `/pricing refresh` forces an out-of-cycle fetch from upstream; `/pricing set <model> <input_per_mtok> <output_per_mtok>` writes a per-model rate to the user file's `manual` section atomically ([#28](https://github.com/go-steer/core-agent/pull/28))
- Daily pricing refresh from LiteLLM (PR B) — fetches `model_prices_and_context_window.json` on startup (no more than once per 24h); ETag-aware; network failures non-fatal; opt out with `--no-pricing-refresh` or `pricing.refresh: false` ([#27](https://github.com/go-steer/core-agent/pull/27))
- Layered pricing lookup with per-model overrides (PR A) — catalog order: `cfg.Model.Pricing[name]` → `.agents/pricing.json` → `~/.core-agent/pricing.json` (manual + external sections) → compiled-in fallback → longest-prefix match → `$—` ([#26](https://github.com/go-steer/core-agent/pull/26))
- Lift in-process TUI from cogo + TTY-detect launch (v2 PR 1) ([#25](https://github.com/go-steer/core-agent/pull/25))
- `core-agent-tui` `--local` + welcome + queue panel + ESC + `/attach` + `/spawn` + `/interrupt` — bare invocation lands on attach-URL prompt; queue panel between scrollback and input renders inject lifecycle (queued → sending → acked → processing → done / failed); contextual ESC clears input or fires `/interrupt` ([#23](https://github.com/go-steer/core-agent/pull/23))
- `POST /sessions/<sid>/interrupt` + `Agent.Interrupt()` ([#22](https://github.com/go-steer/core-agent/pull/22))

#### Bug or Regression
- Subtask Flash error, usage double-count, `/context` Models row — `RunSubtask` cost-tracking mirror of `runner/headless.go`'s `tapUsage` (commit ONCE per `TurnComplete`, not per `UsageMetadata` event); new `WithoutBuiltins() adkmodel.LLM` on `builtinsLLM` so Gemini 2.5 Flash subtasks don't error on "Multiple tools are supported only when they are all search tools"; per-model row in `/context` when a session uses more than one model; also lands the summarizer-cost tracking so checkpoint + compaction LLM calls reach `usage.Tracker` (closes [#61](https://github.com/go-steer/core-agent/issues/61)) ([#58](https://github.com/go-steer/core-agent/pull/58))
- Post-checkpoint loop + `/context` slash + surface-gating `/compact` and `/done` ([#54](https://github.com/go-steer/core-agent/pull/54))
- `bash` tool: orphaned background processes no longer hang the tool — set `cmd.WaitDelay = 5 * time.Second` so Go's exec package force-closes inherited stdout/stderr and SIGKILLs subprocesses still holding them after the grace window ([#48](https://github.com/go-steer/core-agent/pull/48))
- Flip `MidTurnInjectionMode` to `QueueForNext` (core-tui#9 stopgap) — later replaced by `AutoContinueFromInbox` for full PR-α parity ([#47](https://github.com/go-steer/core-agent/pull/47))
- Sanitize `SKILL.md` frontmatter for ADK compatibility ([#46](https://github.com/go-steer/core-agent/pull/46))
- `write-always` grants subtree read+write, not write only ([#43](https://github.com/go-steer/core-agent/pull/43))
- `always-allow` grants the subtree, not just the exact path
- Resolve code-review findings across gate + adapter
- Tier 3+ core-tui adapter — populate `ContextWindowSize`/`Used` so `/stats` + sidebar show context
- Classify budget exceedance and scheduler deferrals as `StatusDeferred`
- UX polish for spawn-and-attach (welcome input · spawn flow · SSE timeout) ([#24](https://github.com/go-steer/core-agent/pull/24))

#### Documentation
- Site: clean banner hero on marketing page (no overlay) ([#77](https://github.com/go-steer/core-agent/pull/77))
- Site: move banner image from `/docs` landing to site root hero ([#76](https://github.com/go-steer/core-agent/pull/76))
- Site: add banner image to `/docs` landing + link `tools.md` from reference list ([#75](https://github.com/go-steer/core-agent/pull/75))
- Site: add `reference/tools.md` — built-in tool catalog ([#74](https://github.com/go-steer/core-agent/pull/74))
- Site: fix REF_NOT_FOUND from relref paths post-redesign ([#72](https://github.com/go-steer/core-agent/pull/72))
- Site: v2.0 redesign Phase 4 — GKE multi-agent worked example (three coordinating agents: platform + operator + devteam, based on [gke-labs/kube-agents](https://github.com/gke-labs/kube-agents), walking a real SLO-breach-during-rollout incident) ([#68](https://github.com/go-steer/core-agent/pull/68))
- Site: v2.0 redesign Phase 3 — agent-design section (prescriptive: system-instructions, skills, subagents-and-wrappers, cost-efficiency) ([#67](https://github.com/go-steer/core-agent/pull/67))
- Site: v2.0 redesign Phase 2 — 15-minute quickstarts for interactive + autonomous use; dismantle `user-guide.md` ([#66](https://github.com/go-steer/core-agent/pull/66))
- Site: fix relref leftovers from Phase 1 reorg ([#65](https://github.com/go-steer/core-agent/pull/65))
- Site: v2.0 redesign Phase 1 — structural reorg into Hugo sections `cli/{interactive,autonomous}/`, `library/`, `agent-design/`, `reference/`, `skills-library/` (hard-break URLs — external inbound links to the old layout 404) ([#64](https://github.com/go-steer/core-agent/pull/64))
- Site: v2.0 sweep — context-management hub + true up existing pages ([#63](https://github.com/go-steer/core-agent/pull/63))
- Add core-tui adapter design
- Add `/attach` + `/spawn` + `/welcome` slash commands to embedded TUI design ([#21](https://github.com/go-steer/core-agent/pull/21))
- Embedded TUI design — `--local` mode + queue panel + `/interrupt` ([#20](https://github.com/go-steer/core-agent/pull/20))
- Add "How we develop" + "How we release" sections to AGENTS.md ([#19](https://github.com/go-steer/core-agent/pull/19))

#### Other (Cleanup)
- Fold Unreleased into v2.0.0 ahead of retag
- Cut v2.0.0 — context management + core-tui default ([#70](https://github.com/go-steer/core-agent/pull/70))
- Bump core-tui to v0.6.3
- Bump core-tui to v0.6.1
- Bump core-tui to v0.4.1
- Bump core-tui to v0.3.1
- Bump otel exporters to clear GO-2026-4985
- Bump core-tui to v0.2.0
- Bump core-tui to v0.1.0
- Adapter: gofmt + goimports
- Adapter: fix goheader (Google LLC) + drop unnecessary conversion

### Breaking changes

Pre-1.0 minor-version breakage; consumers should update imports and config.

- All public packages moved under `pkg/`. Downstream sed snippet: `find . -name "*.go" -exec sed -i -E 's|github.com/go-steer/core-agent/(agent\|attach\|config\|eventlog\|instruction\|mcp\|models\|permissions\|recording\|runner\|session\|skills\|telemetry\|tools\|usage)|github.com/go-steer/core-agent/pkg/\1|g' {} +` then `go mod tidy`. See `docs/pkg-reorg-option-1.md`.
- `config.ModelConfig.Pricing` is now a map keyed by model name (`config.PricingMap`), not a single `*PricingConfig`. Prior single-pricing form only matched `cfg.Model.Name`, so any operator who `/model`-switched mid-session lost their rate override.
- `core-agent-tui` is now remote-only — `--local` (and `--no-cleanup`, and the entire `spawn.go` machinery) removed from `cmd/core-agent-tui/`. Spawn-and-attach for local interactive use moved to `core-agent` itself (the in-process TUI). Operators typing `/spawn` get a migration hint.

## [1.8.0] — 2026-05-23

**Remote operability — attach mode, peer discovery, and the operator TUI.** Five surfaces that together turn `core-agent` from a single-process CLI into a deployable, observable, controllable runtime: attach-mode (HTTP + Server-Sent Events live-tail + `POST /inject` + `/wake` for headless agents), peer registration (hub-and-spoke fleet discovery on top of attach), attach-config (`.agents/config.json` defaults + env-var expansion so K8s ConfigMaps can replace 8+ CLI flags), read-only state endpoints (`GET /sessions/<sid>/tools|agents|status` + `permissions.Gate.Snapshot()` so an operator surface can see what's gated without a re-run), and the `core-agent-tui` bubble-tea binary (separate `cmd/core-agent-tui/`; the default `core-agent` binary stays bubble-tea-free so distroless K8s images don't ship the TUI deps).

Also picks up `fetch_url` — HTTP GET as a structured built-in with a URL allowlist mirroring `path_scope`, the one tool picked from Hermes Agent's ~80-file catalog because it closes a real gap (`bash curl` shell-outs lose URL + status as structured eventlog metadata) and fits the structural-defaults posture (operator-declared `url_scope.allow` with HTTPS-only by default; per-host `headers` injection from env-var references so credentials never live on tool arguments). Two REPL fixes from UAT: `POST /inject` against a REPL-mode agent now actually triggers a turn (the loop selects on stdin + wake instead of stdin only), and streamed model output gets an `asst › ` chevron so the reply is visually distinct from the prompt.

Five PRs end-to-end validated via the new `dev/uat/attach/` harness (Sessions A-E). Design: `docs/attach-mode-design.md`, `docs/peer-registration-design.md`, `docs/attach-tui-design.md`, `docs/fetch-url-design.md`.

### Changes by Kind

#### Feature
- `core-agent-tui` bubble-tea consumer for attach-mode — new separate binary at `cmd/core-agent-tui/`; default `core-agent` stays distroless-clean (zero bubble tea / lipgloss / glamour imports, verifiable with `go list -deps ./cmd/core-agent/`); two-pane layout + status bar + live four-field usage panel; session picker enumerates hub-local + peer sessions in parallel (5-second per-peer timeout); direct-jump via `core-agent-tui <url>/sessions/<sid>` skips the picker; SSE streaming with glamour rendering finalized on message-complete; V1 slash commands (`/help`, `/quit`, `/exit`, `/clear`, `/sessions`, `/reconnect`, `/wake`, `/inject`, `/theme`, `/tools`, `/subagents`, `/status`, `/peers`, `/transcript`); editor handoff via `Ctrl+E` (fallback through `$VISUAL` → `$EDITOR` → `vi`); bearer auth via `--token=ENVVAR`; new shared `internal/attachclient/` package holds URL parsing + Unix-socket-aware HTTP client + typed verbs; release pipeline ships two artifacts going forward ([#16](https://github.com/go-steer/core-agent/pull/16))
- Attach-mode read-only state endpoints + `permissions.Gate.Snapshot()` — three new HTTP read endpoints (`GET /sessions/<app>/<sid>/tools`, `/agents`, `/status`), each a pure projection over in-memory agent state with no persistence churn; new `permissions.Gate.Snapshot()` returns `{mode, allow, deny}` plus `Gate.ToolGateState(name)` classifier feeding `gate_state` on `/tools`; new `agent.WithGate(g)` option; new optional provider interfaces `attach.ToolsProvider` / `AgentsProvider` / `StatusProvider`; `*agent.Agent` implements all three and gains `ModelName()`; agents without them return empty lists rather than 501 so mixed-vintage fleets work ([#15](https://github.com/go-steer/core-agent/pull/15))
- Attach-mode + peer-registration flags configurable via `.agents/config.json` — new `attach` section (`AttachConfig`) holds defaults for every `--attach-*` CLI flag (`listen`, `unix_socket`, `tls_cert`, `tls_key`, `client_ca`, `token_env`, `readonly`, `peer_hub`, `register_to`, `register_endpoint`, `register_name`); precedence is CLI flag > config > zero value; all string fields pass through `os.ExpandEnv` so per-pod values like `"https://${POD_IP}:7777"` can live in a shared K8s ConfigMap and resolve at startup; the bearer-token secret deliberately isn't a config field — only the *name* of the env var (`token_env`) is ([#13](https://github.com/go-steer/core-agent/pull/13))
- Peer registration — hub-and-spoke discovery for multi-agent deployments — 4 new endpoints (`POST /peers`, `GET /peers` with optional `?label=k=v`, `POST /peers/<id>/heartbeat`, `DELETE /peers/<id>`); TTL-based leases default 60s (server-capped at 5min); name-based upsert means restart issues a fresh registration ID rather than orphaning; expired leases pruned every 5 seconds; new `attach.PeerRegistry` + `attach.PeerClient.RegisterAndHeartbeat` (runs the heartbeat goroutine with automatic re-register-on-failure); CLI flags `--attach-peer-hub` (hub) + `--attach-register-to <hub-url>` / `--attach-register-endpoint <my-url>` / optional `--attach-register-name` (peers); `core-agent ls <hub-url>` surfaces a PEERS section when the hub has peer registration enabled; implements `docs/peer-registration-design.md` ([#12](https://github.com/go-steer/core-agent/pull/12))
- Attach mode — live-tail + inject for headless agents — new `attach/` package + `core-agent attach <url>` / `core-agent ls <url>` subcommands; HTTP + Server-Sent Events transport with 4 endpoints (`GET /sessions`, `GET /sessions/<app>/<sid>/events`, `POST /sessions/<app>/<sid>/inject`, `POST /sessions/<app>/<sid>/wake`) with a `/sessions/<sid>` shortcut for the unambiguous case (409 on collision); stdlib `net/http`, optional mTLS (`--attach-client-ca`) + bearer-token (`--attach-token=ENVVAR`) auth; `--attach-readonly` disables writes regardless of auth; Unix socket transport (`--attach-unix-socket`) for local dev / Cloud Run sidecar shapes; per-session broadcaster pumps from `eventlog.Stream.Watch` so live-tail is lossless and reconnecting clients can `?since=N` replay then resume; each agent self-registers via new `agent.WithSessionRegistry` Option; implements `docs/attach-mode-design.md` ([#11](https://github.com/go-steer/core-agent/pull/11))
- `fetch_url` built-in + `url_scope` config — HTTP GET as a first-class tool with an operator-controlled allowlist; new `tools/fetch.go` (~280 LoC) registers `fetch_url(url, max_bytes=64KB)` returning `{url, final_url, status, content_type, bytes, truncated, body}` — replaces `bash curl` so URL + status land structured in the eventlog; new `config.URLScopeConfig` (Allow/Deny grammar mirroring `PathScopeConfig`; HTTPS-only default; default-deny when `Allow` empty — the tool isn't even registered without an allowlist); per-host `headers` block injects `Authorization: Bearer ${ENV_VAR}` at request time; most-specific pattern wins; each redirect target re-checked (max 5 hops); non-text content returns `truncated=true, body=""`; CLI `--allow-url-host="github.com,*.googleapis.com"` for one-shot additions; `--disable-tools=fetch_url` to turn off; composes with the permissions gate (`permissions.allow: ["fetch_url:github.com/*"]`); implements `docs/fetch-url-design.md`

#### Bug or Regression
- REPL reacts to wake + `asst › ` sigil on model output — `POST /inject` against a REPL-mode agent used to queue the message + fire the wake signal but the REPL loop blocked on stdin only; the loop now keeps a persistent stdin-reader goroutine feeding a channel and `select`s on `(stdinLine, WakeRequested, ctx.Done)`; on wake it runs a turn with empty prompt and `Agent.Run`'s pre-turn drain processes the inbox via `formatInboxForPrompt`; a `[wake] inbox arrived — processing` banner goes to stderr; new `runner.REPLWithAgent` entry point takes a pre-constructed `*agent.Agent`; bold-cyan `asst › ` sigil now emitted before the first partial chunk of each assistant speaking block and resets across tool-call boundaries so multi-segment turns read as discrete blocks; surfaced by `dev/uat/attach/` Session A ([#17](https://github.com/go-steer/core-agent/pull/17))
#### Security
- Bumped `golang.org/x/net` v0.54.0 → v0.55.0 to clear [GO-2026-5026](https://pkg.go.dev/vuln/GO-2026-5026) (idna.ToASCII vuln reached via `mcp.googleAuthTransport.RoundTrip → http.Transport.RoundTrip`); local `govulncheck` now reports 0 affected; also fixes lint cache collision

#### Documentation
- Promote `[Unreleased]` to `[1.8.0]` + bump release pin ([#18](https://github.com/go-steer/core-agent/pull/18))
- Record attach-mode v1 decisions + peer-registration follow-on plan

## [1.7.0] — 2026-05-21

**Distroless-prep + runtime-supplied history.** Three new built-in tools (`delete_file`, `stat`, `json_query`) close the capability gaps that an upcoming K8s deployment using `gcr.io/distroless/static` (no shell, no `bash`, no external CLIs) would otherwise hit — the agent's effective surface becomes the built-in suite plus configured MCP tools. Plus a new `Agent.RunWithContents(ctx, []*genai.Content)` method for integrations (the AX adapter on the `axplore` branch is the motivating consumer) that supply the full conversation history per turn rather than relying on the session-managed prompt. Both additive — no breaking changes.

### Changes by Kind

#### Feature
- Three new built-in tools for distroless / static-binary deployments — `delete_file` removes a regular file (idempotent, refuses directories, honors `CheckFileWrite` gate + path scope; replaces `bash rm` for scheduled-monitor cleanup, log rotation, stale state); `stat` is a point query for file metadata (`size`, `mod_time` RFC3339 UTC, `mode`, `is_dir`; missing path returns `{exists: false}` rather than an error so the model can do "has this been written yet?" checks without exception handling; honors `CheckFileRead`); `json_query` runs a jq expression against JSON loaded from a file (`path`) or supplied inline (`json`) using `github.com/itchyny/gojq` (pure Go, no CGO — distroless-friendly) — designed to slice the large structured outputs remote MCP servers return (`kubectl -o json`, `gcloud --format=json`, REST APIs); output respects the per-tool truncation cap; malformed JSON / bad jq expressions / jq runtime errors surface as tool-result errors the model can adapt to; all three enabled by default in `tools.Default()`; opt out via `tools.disable` in config or `--disable-tools=<name>` on the CLI
- `Agent.RunWithContents(ctx, []*genai.Content)` — drives one agent turn from a caller-supplied conversation history instead of the session-managed prompt that `Agent.Run` uses; the trailing message is the new user input, everything before it is pre-populated into a fresh session as history events; each call mints a fresh sessionID (`crypto/rand`) so prior calls don't bleed state — caller-supplied history is authoritative; errors clearly when the trailing message isn't a user role, or when contents is empty; motivating consumer is the AX adapter (runtimes that own the conversation log and resend it per turn) on the `axplore` branch; the primitive itself is general-purpose; the `Agent` struct grows three new exported accessors' worth of fields (`sessionService`, `appName`, `agentName`) to support the `Create + AppendEvent` round-trip

#### Documentation
- Promote `[Unreleased]` to `[1.7.0]` + bump release pin

## [1.6.0] — 2026-05-21

**Scheduled monitoring — the missing primitive for long-running autonomous workloads.** New `tools.Scheduler` interface + `tools.SleepScheduler` / `tools.ExitOnDeferScheduler` impls + `schedule_next_turn` tool let the model emit *"wake me at T+N with prompt X"* intent that the autonomous driver honors between turns, without burning the prompt cache by sleeping inside a turn. Composes with `agent.BackgroundAgentManager` (via `WithBackgroundDefaultScheduler`) for the canonical supervisor-fans-out-to-N-monitors topology — validated end-to-end against a real GKE cluster with Vertex/Gemini in the new `dev/uat/scheduled-monitor` driver, including three-layer reactive fan-out (supervisor → long-running monitor → on-demand triage subagent spawned via `scheduler="none"`).

Plus the `Agent.WakeRequested` / `Agent.RequestWake` seam so out-of-band signals (operator inject, child-alert arrival) pierce active sleeps; an eventlog write-serialization mutex that lets concurrent agents share a SQLite session DB without `SQLITE_BUSY` races; and a `spawn_agent` fix that tolerates the model listing auto-wired tool names (`schedule_next_turn`, `report_done`, `report_alert`, `report_completed`) by silently skipping them.

Design: `docs/scheduled-monitoring-design.md` covers the design rationale, the canonical GKE fleet-monitoring topology, the three-tier acceptance plan (hermetic smoketest / UAT against real K8s / nightly real-LLM steering eval), and the open question on operator-driven wake (deferred to attach mode per `docs/attach-mode-design.md`). Example: `examples/scheduled-monitor/`.

### Changes by Kind

#### Feature
- Scheduler primitive for paced autonomous loops — new `tools.NewScheduleTool` registers the `schedule_next_turn` tool the model calls to defer, returning a buffered channel the driver consumes after each turn; tool description carries a cadence ladder (30s fast-changing, 5-15m steady-state, 1h+ slow-changing infra), good-vs-bad `next_prompt` examples, state-persistence reminder, and the report_done-wins-on-collision rule; `ScheduleOptions.MaxDefer` clamps at the tool layer, `ScheduleOptions.Name` / `Description` allow per-deployment customization; new `tools.Scheduler` interface + `tools.SchedulerFunc` adapter with two bundled impls — `tools.SleepScheduler()` (long-lived daemon: sleeps the goroutine until `WakeAt`, respects context cancellation) and `tools.ExitOnDeferScheduler()` (orchestrator-managed: returns `tools.ErrSchedulerDefer` so the loop exits with `StopReasonDeferred` + `RunResult.NextWakeAt` populated); new `agent.RunAutonomous` options `WithScheduler`, `WithMaxDefer`, `WithScheduleToolName` / `WithScheduleToolDescription` / `WithScheduleToolMaxDefer`; new `agent.StopReasonDeferred` + `agent.RunResult.NextWakeAt`; `agent.ResumeAutonomous` honors deferred checkpoints so `kill -9` mid-sleep resumes correctly (past wake-time proceeds immediately, matching the CronJob-fired-late case); `agent.BackgroundAgentManager` gains `WithBackgroundDefaultScheduler` + per-spawn `BackgroundSpec.Scheduler` string-enum (`""`/`"default"`/`"sleep"`/`"exit_on_defer"`/`"none"`) with `agent.ErrUnknownScheduler` at spawn time for unknown values; `spawn_agent` tool JSON schema gains a matching `scheduler` field so the parent's model can pick per-child cadence shape at runtime; new `agent.DefaultSchedulingInstruction` composable system-instruction constant covering cross-cutting cadence policy (slow-by-default, adapt on anomaly, state via files/todos, don't call schedule + done in the same turn) — the driver does NOT auto-inject
- Wake-on-event seam + UAT driver becomes reactive — `SleepScheduler` now selects on a third channel alongside its timer and `ctx.Done` so out-of-band signals interrupt an active sleep; new `Agent.RequestWake` + `Agent.WakeRequested` + `tools.ContextWithWake`; `Agent.Inject` calls `RequestWake` internally so operator input pierces sleep automatically; `AutonomousHandle.RequestWake` forwards to the underlying agent; originally scoped to attach mode in `docs/scheduled-monitoring-design.md` but pulled forward when the UAT driver needed reactive supervisor behavior — the in-process primitive is now in place, HTTP transport stays deferred to attach mode (which will just call `RequestWake` on the looked-up session)
- `--context` / `--namespace` flags on the bare UAT driver

#### Bug or Regression
- `spawn_agent` tolerates auto-wired tool names; UAT stdin waits for agent — silently skips model-listed `schedule_next_turn` / `report_done` / `report_alert` / `report_completed` in the tools request
- Supervisor calls `schedule_next_turn` after spawning, not `report_done` (UAT)
- Make `/tmp/` requirement absolute in the supervisor brief (UAT)
- Eventlog writes serialize through a mutex — `eventlog.service` now takes a per-handle `sync.Mutex` on `Create` / `Delete` / `AppendEvent` so a parent agent and its background subagents sharing the same SQLite eventlog don't race at the write lock; symptom that prompted the fix was a child subagent's first `session.Create` failing with `SQLITE_BUSY` because the parent was mid-checkpoint; WAL handles concurrent reads natively; reads (Get/List) skip the mutex
- Inject `busy_timeout(5000)` pragma into SQLite DSNs at `eventlog.Open` time via reflection on the dialector's `DSN` field — defense in depth for any future write path that bypasses the service wrapper

#### Documentation
- Version scheduled-monitoring + attach-mode for the wake seam landing
- Settle scheduled-monitoring open questions; port wake API to attach-mode
- Scheduled-monitoring design (`docs/scheduled-monitoring-design.md`) — Scheduler primitive + supervision-tree
- Promote `[Unreleased]` to `[1.6.0]` + bump release pin

#### Other (Cleanup)
- `dev/smoke/08-scheduled-monitor-gke.sh` — smoke against a real GKE cluster
- `dev/uat/scheduled-monitor` — UAT driver binary for scheduled-monitoring, exercises `examples/scheduled-monitor/` end-to-end against the echo mock provider so it works in CI without credentials

## [1.5.0] — 2026-05-20

**Remote MCP servers, batteries included.** Google OAuth (access-token) auth for `.agents/mcp.json` HTTP servers so `core-agent` can call Google-hosted endpoints like the GKE remote MCP server using only Application Default Credentials; plus two latent bugs both surfaced the first time a real remote MCP server was driven end-to-end — tool wrappers were silently stripping ADK's `RequestProcessor` interface (every MCP tool call failed preprocess), and the event renderer was double-rendering `→ function_call` lines via ADK's stream aggregator.

Smoke at `dev/smoke/07-mcp-google-oauth.sh` (requires `MCP_GOOGLE_OAUTH_SMOKE_PROJECT` + ADC).

### Changes by Kind

#### Feature
- Google OAuth (access-token) auth for remote MCP HTTP servers — `.agents/mcp.json` now supports `auth.google_oauth.scopes` on HTTP servers; `core-agent` sets `Authorization: Bearer <access-token>` on every outbound MCP request using `google.FindDefaultCredentials(ctx, scopes...)` from Application Default Credentials; the `oauth2.TokenSource` caches and refreshes internally; an init-time pre-fetch surfaces ADC misconfig at startup instead of on the first tool call; suitable for Google-hosted API endpoints that accept scoped access tokens — the GKE remote MCP server at `https://container.googleapis.com/mcp` is the canonical first target (caller needs `roles/mcp.toolUser` plus the relevant resource-viewer role, e.g. `roles/container.clusterViewer`); the auth layer wraps innermost so a misconfigured static `Authorization` header in `Headers` cannot overwrite the IAM token; non-conflicting static headers (e.g. `X-Custom`) still pass through; audience-scoped ID-token auth for Cloud Run / IAP / custom-OIDC endpoints is not yet supported — the new `AuthSpec` shape leaves room for a sibling `google_id_token` field once a consumer needs it

#### Bug or Regression
- MCP `ProcessRequest` + dedup repeat tool-call renders — `renamedTool` (`mcp/namespace.go`) and `gatedTool` (`tools/gate.go`) forwarded `Name` / `Description` / `IsLongRunning` / `Declaration` / `Run` but silently dropped `RequestProcessor`, so every wrapped MCP tool failed ADK's preprocess step with `tool "X" does not implement RequestProcessor() method` (no user-visible breakage shipped earlier only because no end-to-end MCP smoke existed before this release); both wrappers now implement `ProcessRequest` and pack themselves (not the inner tool) so the model sees the prefixed / renamed `Declaration` and ADK's call-back dispatch hits the wrapper's `Run` instead of bypassing namespace + gate; new public `tools.PackTool` reimplements ADK's internal `toolutils.PackTool` algorithm (~30 lines of public `model.LLMRequest` field manipulation); regression tests in `tools/pack_test.go`, `tools/gate_test.go`, `mcp/namespace_test.go` with a bug-tying comment so a future refactor doesn't strip the method again; also fixes `runner.WriteEvents` double-rendering `→ function_call` lines (ADK's stream aggregator can yield the same `FunctionCall` part on intermediate + final events) by deduping `→` and `←` lines within one invocation using the existing `seenLines` set in `runner/events.go` — per-invocation scope so consecutive turns with legitimately identical calls each render normally

#### Documentation
- M4 killer-feature design docs (attach, MCP, Code Mode, AX)
- README index + six prior design / handover notes
- Promote `[Unreleased]` to `[1.5.0]` + bump release pin

## [1.4.0] — 2026-05-19

**Gemini tool-calling optimization plus narrative documentation.** Closes the snappiness gap measured between `core-agent` on Gemini and Claude Code: a parallelism mandate in `agent.DefaultInstruction`, tool descriptions that steer models toward gate-covered primitives, a new `read_many_files` batch tool, and a default-model flip to a Vertex variant fine-tuned for developer-defined tools. Direct probe measurement (`dev/parallel-probe/`): Claude finishes the same code-search task in 4 turns instead of 17 (28s vs 89s); Gemini's tool choice on the same task flipped from 15 bash / 1 grep to 19 grep / 4 bash, putting every code-investigation call back under the permission gate.

Three new long-form documentation pages (Why `core-agent`, User guide, Library guide) round out a previously reference-only site.

### Changes by Kind

#### Feature
- Parallelism mandate in default agent instruction — new exported `agent.DefaultInstruction` constant holding the system instruction the agent applies when `WithInstruction` is not used; comprises the baseline helpfulness/concision directive plus a parallelism mandate adapted from `google-gemini/gemini-cli` (`packages/core/src/prompts/snippets.ts`) for independent operations (searching, reading several files, running independent shell commands); consumers who want to layer their own guidance on top can compose via `agent.WithInstruction(agent.DefaultInstruction + "\n\n" + extra)`; probe data (`dev/parallel-probe/`): vanilla `gemini-3.1-pro-preview` never batched tool calls across 65 search turns with no instruction prompt; the parallelism mandate cut Claude's same task from 17 turns to 4 (89s → 28s wall clock); on Gemini-customtools the mandate doesn't move the batching needle for open-ended search but pairs with the description rewrites below to flip tool choice dramatically
- Edit-collision + quality caveats in `DefaultInstruction` — two safety caveats added: do not parallel-edit the same file in one response (sequential writes only — parallel writes race), and efficiency is secondary to correctness (when in doubt, serialize); mandate and caveats adapted from `google-gemini/gemini-cli` at `packages/core/src/prompts/snippets.ts`
- Tool descriptions demote `bash` for code investigation — `read_file` and `grep` now say *"PREFERRED over `bash cat`/`bash grep`"* with the reasons (permission gate, output caps, structured results); `bash`'s description now explicitly defers code investigation to the structured tools and lists its own use cases (builds, tests, git, formatters, package managers); post-Tier-1 probe: Gemini-customtools on search went from 15 bash / 1 grep to 19 grep / 4 bash — the biggest behavior shift of the release, with the practical consequence that what was previously raw shell now routes through the permission gate
- `read_many_files` batch tool — reads multiple files in a single tool call via `paths` (explicit list), `pattern` (basename glob walked from `path`, default `.`), or both together; honors the permission gate per file; gate denials, missing files, and directories surface as entries with a `skipped: "<reason>"` field so the batch never aborts on one bad path; per-file content cap 64KB; whole-response cap defaults to 256KB / 5000 lines (overridable via `cfg.ToolOutput.PerTool["read_many_files"]`); tool description explicitly says "PREFERRED over multiple parallel `read_file` calls when you already know the set of files you need" — Gemini handles one tool call taking a list better than N parallel `read_file` calls; default-on; opt out via `tools.disable: ["read_many_files"]` in config or `--disable-tools=read_many_files` on the CLI; mirrors `google-gemini/gemini-cli`'s `read_many_files` at `packages/core/src/tools/definitions/read_many_files.ts`
- Default Gemini model is now `gemini-3.1-pro-preview-customtools` (was `gemini-3.1-pro-preview`) — the `-customtools` Vertex variant is fine-tuned to prefer developer-defined tools over raw shell; same price, same 1M context window, same reasoning quality, but it no longer routes around structured `grep` / `read_file` / `edit_file` to shell out via `bash`; direct measurement on a known-set multiread task: the vanilla model never batched (0 parallel `read_file` calls across 65 turns), the variant emits 5 parallel `read_file` calls in a single turn (mean batch 3.0 vs 1.0); bypass with `cfg.Model.Name = "gemini-3.1-pro-preview"` if you need the un-tuned behavior for baseline comparisons; variant is documented in `google-gemini/gemini-cli` at `packages/core/src/config/models.ts`

#### Bug or Regression
- Add Google LLC license header to `dev/parallel-probe/main.go`

#### Documentation
- `docs/site/content/docs/why-core-agent.md` — long-form pitch (12 capabilities with the "what problem it solves" framing) plus a Harvey-balls comparison table covering 26 capabilities across raw ADK Go vs `core-agent`; for engineers evaluating the substrate against starting from raw `google.golang.org/adk`
- `docs/site/content/docs/user-guide.md` — end-user narrative walkthrough of giving the CLI a personality (provider, `AGENTS.md`, skills, MCP servers, permission posture) anchored on a running "Go code-reviewer" example
- `docs/site/content/docs/library-guide.md` + index promotion of the new narrative pages — narrative tour of the Go-library extension points (custom `Prompter`, custom tools, custom `RemoteAgentSpawner`, custom `models.Provider`, custom `session.Service`, background workers + inbox), each with worked code; closes with a 100-line HTTP-served-agent example
- Promote `[Unreleased]` to `[1.4.0]` + bump release pin

#### Other (Cleanup)
- `dev/parallel-probe/` — standalone diagnostic that measures per-turn tool-call batching against any provider/model, used throughout this release's design + validation; flags `--provider`, `--model`, `--task={search,multiread}`, `--nudge`, `--no-bash`

## [1.3.0] — 2026-05-16

**Interrupt machinery — programmatic and interactive.** Two new public library surfaces (for harness embedding like Scion), a Scion adapter rewrite that consumes them, and a raw-mode terminal handler that gives the bundled CLI's REPL Claude Code / gemini-cli-style ESC-cancels-turn and double-Ctrl+C-exits gestures. `Agent.Inject(message)` + `Agent.InboxArrived()` give any caller (harness goroutine, HTTP handler, orchestrator gRPC stream, test fixture) a per-agent queue drained pre-turn as an `[Inbox]` block sibling to v1.2's `[Background reports]`, with drop-oldest backpressure at soft cap 256. `agent.StartAutonomous(ctx, build, goal, opts...)` returns an `*AutonomousHandle` exposing `Pause` / `Resume` / `Stop` / `Inject` / `Status` / `Wait` / `Done`; `RunAutonomous` keeps working unchanged as the synchronous wrapper. Pause/Resume emit synthetic audit events (`Author="<binary>/autonomous"`, `CustomMetadata.kind="paused"|"resumed"`) with empty `Content.Role` so ADK's content processor skips them from LLM context.

The bundled REPL's mid-turn interrupt lives in `runner/interrupt.go` (package-private `turnInterrupter`) and uses `golang.org/x/term`'s `MakeRaw`/`Restore` for cross-platform raw mode — a new direct dependency. Auto-enables when stdin is a TTY; piped / non-TTY falls back silently to the legacy single-Ctrl+C-exits behavior. Tool calls in flight when the cancel fires are best-effort: `bash` (via `exec.CommandContext`) cancels promptly; tools that ignore ctx finish their in-flight work before the loop unwinds.

Deferred (out of scope for v1.3.0): `AutonomousHandle.Redirect(newGoal)` (workaround: `handle.Stop()` then `StartAutonomous(newGoal)` with the same agent — the eventlog carries history); `extras/scion-remote-agent/` reference `RemoteAgentSpawner` for sibling-container spawning (implementation choice HTTP vs CLI depends on deployment model, needs Scion-side input); concurrent task multiplexing per Scion container; lifecycle status taxonomy enrichment for `sciontool_status` (richer progress % / ETA / blocking-on-what worth doing but should be designed against what Scion's UI actually wants); REPL `/inject` slash command (library-only for v1.3.0).

### Changes by Kind

#### Feature
- soft-interrupt + AutonomousHandle + mid-turn REPL interrupt (v1.3.0) — new `Agent.Inject` / `InboxArrived` inbox with drop-oldest backpressure at soft cap 256 and `[Inbox]` block prepend pre-turn; new `agent.StartAutonomous` + `*AutonomousHandle` (Pause blocks at next pre-turn checkpoint so current turn finishes normally, Stop cancels via ctx idempotently); new `agent.WithBeforeTurn(cb)` AutonomousOption that Pause uses internally (also directly usable for rate limits, external approvals, other per-turn checkpoint gating); Pause/Resume audit events emitted via new `emitNoteEvent` helper in `agent/checkpoint.go`; `extras/scion-agent` Scion adapter rewritten so a background goroutine reads stdin and calls `Agent.Inject` per line while the main loop waits on `Agent.InboxArrived` and runs a `"continue"` turn (messages arriving during an in-flight turn queue and land on the next turn instead of blocking; `--input` still seeds the first turn via `Inject` before the loop starts); mid-turn REPL interrupt using `golang.org/x/term`'s `MakeRaw` (auto-enables on TTY, silent non-TTY fallback), with 11 state-machine unit tests including the double-Ctrl+C window, hint deduplication, and non-TTY fallback; new `examples/autonomous-handle/` credential-free demo (`StartAutonomous` → `Pause` → `Inject` → `Resume` → `Wait`) using a slow-LLM wrapper around the echo mock so the Pause window is visible

#### Other (Cleanup)
- real-LLM smoke suite (dev/smoke/) — includes `dev/smoke/06-inject-autonomous.sh` wrapping `examples/autonomous-handle` (no credentials required; safe to run anywhere)

## [1.2.0] — 2026-05-16

**Dynamic in-process background subagents + remote-spawner seam.** The parent agent's model spawns subagents at runtime via a `spawn_agent` tool call (system prompt + goal + tools). A consumer-pluggable `agent.RemoteAgentSpawner` interface mirrors the shape for out-of-process work (gRPC, K8s Jobs, Cloud Run). Subagent reports flow back through both a synchronous `OnAlert` hook (for inline display) and a pre-turn drain that prepends alerts to the parent's next prompt.

Subagent permissions inherit the parent's `*permissions.Gate` wholesale; ask-mode prompts include a `[<subagent-name>]` source attribution so the human approving the call knows which agent is asking; concurrent prompt access is serialized through a mutex so background subagents can't race for `os.Stdin`. Depth cap 2, concurrency cap 8, default per-subagent budgets 50 turns / $1.00 / 10 min, alert channel buffer 256 with drop-oldest backpressure.

Deferred (out of scope for v1.2.0): bounded permission subsets + parent-as-arbiter — v1.2.0 inherits the parent gate wholesale; the richer model (subagent gets a subset, out-of-subset requests bubble up to the parent's model for a decision) is a v1.3+ feature needing per-subagent gate construction plus a cross-agent permission-request message type. Persistence across main-agent restarts — background subagent state is process-local; cross-restart resume needs the manager to persist its registry to eventlog and reconstruct on `ResumeAutonomous`. Subagent → subagent communication — subagents only `report_alert` to their parent. MCP / skill tools in the spawn catalog — v1.2.0's catalog defaults to the built-in tool suite; library callers can pass more via `WithBackgroundCatalog`, but the CLI doesn't enumerate MCP/skill toolsets into the catalog automatically. Budget pooling across siblings — each subagent has its own budget; no global cap.

### Changes by Kind

#### Feature
- dynamic background subagents + remote spawner seam (v1.2.0) — new `agent.BackgroundAgentManager` per-parent registry via `agent.NewBackgroundAgentManager(opts...)` requiring `WithBackgroundProvider(provider, modelID)`, optional knobs `WithBackgroundGate` / `WithBackgroundCatalog` / `WithBackgroundMaxDepth` (default 2) / `WithBackgroundMaxConcurrent` (default 8) / `WithBackgroundDefaultBudgets` (default 50 turns / $1.00 / 10 min) / `WithBackgroundAlertBuffer` (default 256), and Spawn / List / Get / Stop / Alerts / OnAlert / PrependPendingAlerts / Close lifecycle; new `agent.WithBackgroundManager(mgr)` Option — inside `agent.New`, the manager's `attachParent` is called so subsequent `Spawn` reads the parent's session triple + `session.Service`; `Agent.Run` pre-turn alert drain prepends pending alerts to the ADK runner's prompt so the parent's model always sees what its subagents reported; new `Agent.BackgroundManager()` accessor; four model-facing tools (`spawn_agent`, `list_agents`, `check_agent`, `stop_agent`) via `agent.NewSpawnAgentTool` plus one-line wiring via `agent.NewBackgroundSpawnTools(mgr)`; consumer-pluggable `agent.RemoteAgentSpawner` interface mirroring the `tools.Prompter` shape — implement `Spawn(ctx, spec) (RemoteAgentHandle, error)` against your substrate and the handle's `Events()` channel feeds into the same alert pipeline via `agent.NewSpawnRemoteAgentTool(spawner, mgr)`; `agent.RefuseRemoteAgentSpawner(reason)` is the analog of `tools.RefusePrompter`; `permissions.StdinPrompter` source attribution via new `Source` field on `permissions.PromptRequest` populated by `permissions.WithSubagentSource(ctx, name)` (public reader `permissions.SubagentSourceFromContext(ctx)`); new `permissions.Serialize(p Prompter) Prompter` mutex wrapper + `permissions.PrompterFunc` adapter; exposed `runner.FormatAlertLine(from, kind, text)` + `runner.AnsiMagenta()` so consumers render `↪ <from> <kind>: <text>` alert lines identically (REPL auto-installs an OnAlert hook writing to stderr in magenta when a manager is wired); `cmd/core-agent --no-background-agents` opt-out flag (default enabled); new `examples/background-monitor/` credential-free end-to-end demo against the echo mock provider (two stub subagents, exercises OnAlert + pre-turn drain)

## [1.1.0] — 2026-05-16

**Interactive permissions + Gemini server-side built-in visibility.** The bundled CLI gains a real `Prompter` for tool-approval prompts (v1.0.x passed `nil`), plus first-class visibility into Gemini's server-side built-in tool activity (search-grounding) — surfaced in both stdout and the eventlog audit trail via a new `session.Service` wrapper that projects `GroundingMetadata` into synthetic events.

`--yolo` on `cmd/core-agent` bypasses the gate for headless / scripted invocations (equivalent to `permissions.mode = "yolo"` in config). When stdin is a TTY and `--yolo` isn't set, `permissions.StdinPrompter(os.Stdin, os.Stderr)` wires automatically — tool calls in `ask` mode now prompt the user instead of erroring out. Non-TTY callers still get `ErrNoPrompter`, but the error message now points at `--yolo` and the `permissions.mode` config knob. `gemini.GroundingProjection(svc)` auto-wires when `--session-db` is used with `--provider=gemini` / `vertex`; synthetic events are authored `gemini/google_search`, branch-preserved, deduplicated, and use empty `Content.Role` so ADK's content processor skips them from LLM context.

Known limitations: `URLContext` evidence is not projected today — ADK's gemini converter (`internal/llminternal/converters`) only lifts `GroundingMetadata` into `model.LLMResponse`; `URLContextMetadata` is dropped before the wrapper can see it. Surfacing it would require intercepting raw genai responses below ADK; deferred until a consumer needs it. Anthropic server-side tools (`web_search`, `web_fetch`) aren't projected — those built-ins aren't surfaced in the Anthropic adapter yet (carried forward from v1.0.x Known gaps); the `↪` namespace under `anthropic/*` is reserved for them when they land. Grounding evidence appears *after* the model's text in the chat stream rather than during, because grounding metadata only lands on the aggregated response event, not on partial streaming chunks — acceptable trade for keeping the synchronous text flow uninterrupted.

### Changes by Kind

#### Feature
- interactive permissions + Gemini server-side built-in visibility (v1.1.0) — new public `permissions.StdinPrompter(in, out)` renders permission requests to `out` and reads `y` / `s` / `t` / `a` / `n` from `in`, mapping cleanly to the existing `Decision` enum (reprompts on invalid input, denies on bare enter, surfaces EOF / ctx cancel as errors) — replaces the placeholder `nil` v1.0.x passed for the gate prompter; new `--yolo` flag on `cmd/core-agent` (equivalent to `permissions.mode = "yolo"`); interactive permissions auto-wire in `cmd/core-agent` when stdin is a TTY and `--yolo` isn't set (non-TTY `ErrNoPrompter` message now points at `--yolo` + `permissions.mode`); new `gemini.GroundingProjection(svc)` public `session.Service` wrapper appending one synthetic event per `WebSearchQueries` entry and per `GroundingChunks[i].Web` source (authored `gemini/google_search`, branch-preserved, deduplicated, URI-less sources + empty queries filtered) — auto-wired when `--session-db` is used with `--provider=gemini` / `vertex`; `↪ google_search:` lines in `runner.WriteEvents` alongside client-side `→` / `←` calls using a new `↪` sigil and magenta color (new `ansiMagenta` added to the minimal palette), deduplicated per invocation, format mirrors the projection's eventlog rows so stdout and `agent_eventlog` describe the same activity

## [1.0.1] — 2026-05-16

**Critical bug fixes for `--provider=vertex`.** Two regressions surfaced after v1.0.0 shipped; both are fixed here, and Vertex search-grounding now delivers real results.

First: `models/gemini` now only sets `Config.ToolConfig.IncludeServerSideToolInvocations` when fronting the direct Gemini API (`genai.BackendGeminiAPI`), not when fronting Vertex AI. v1.0.0 set this flag unconditionally to satisfy the direct Gemini API's requirement when built-ins ride alongside function tools, but Vertex AI rejects the flag with `includeServerSideToolInvocations parameter is not supported in Gemini Enterprise Agent Platform (previously known as Vertex AI)`. `--provider=vertex` was completely broken at default invocation for any consumer using `tools.Default()` between v1.0.0 and this fix; `--provider=gemini` is unaffected. The `builtinsLLM` wrapper now learns which backend it's fronting at construction time. Tests pin both branches.

Second: `models/gemini` now tolerates Vertex's streaming SSE heartbeat chunks. Vertex's streaming search-grounding API intermittently emits frames carrying only `UsageMetadata` + `ResponseID` and an empty `Candidates[]`. ADK's stream aggregator (`internal/llminternal/stream_aggregator.go`) treated these as fatal and aborted the stream with `empty response`, poisoning the call before the real grounded chunks landed. Observed failure rate against `gemini-3.1-pro-preview` on Vertex with the default tool suite + GoogleSearch was 30–60% before the fix, 0% across 10 consecutive runs after. The `builtinsLLM` wrapper now drops `empty response` errors mid-stream on Vertex only — the direct Gemini API path is untouched, so a genuine "no content" failure there still surfaces normally. Non-streaming Vertex calls are also untouched: an empty non-streaming response is a real failure and should propagate.

Process: `docs/v1-acceptance.md` Section 6 (Vertex Gemini smoke) was not exercised when cutting v1.0.0 — single-provider sign-off met the plan's bar at the time. The regression slipped through as a result. Going forward, when a fix is added in one provider's request path, run the equivalent smoke against every sibling backend before tagging. The Vertex heartbeat-chunk bug above was found by following through on this discipline after the first Vertex regression report and is what most of the v1.0.1 investigation actually uncovered — the ADK-level `empty response` was masquerading as a clean Vertex failure, not a known protocol quirk.

### Changes by Kind

#### Bug or Regression
- tolerate Vertex streaming heartbeat chunks — also gates `Config.ToolConfig.IncludeServerSideToolInvocations` to `genai.BackendGeminiAPI` only so Vertex stops rejecting the flag; the wrapper now learns which backend it fronts at construction time and drops `empty response` mid-stream on Vertex only, leaving the direct Gemini API path and non-streaming Vertex calls untouched; both regressions surfaced by re-running `docs/v1-acceptance.md` Section 6 against Vertex

## [1.0.0] — 2026-05-16

**First stable release.** Same surface as v0.1.0 with one bug fix and one documented requirement that emerged from running `docs/v1-acceptance.md` against real Gemini. When combining core-agent's default tool suite with the Gemini provider's built-in tools (both default-on), the Gemini API requires a 3.0-or-later model — Gemini 2.5 rejects the combination outright. The default model already pins `gemini-3.1-pro-preview`, so consumers who don't override never hit this; workarounds for consumers who must use Gemini 2.5 are `--no-builtin-tools` (drops the function-calling suite) or library-level `gemini.WithGoogleSearch(false)` + `gemini.WithURLContext(false)` (drops the server-side built-ins). The site also migrated from Hextra to Docsy this cycle with light/dark/auto theme support and a cards-based landing page.

Stability promise (effective with this release): the public API surface listed in this file's preamble is now under SemVer. Breaking changes go through a minor-version bump (`v1.X.0`) with a one-version deprecation period when feasible; patch versions (`v1.0.X`) are bug fixes only.

Operator reference: `docs/site/content/docs/providers.md` · `docs/site/content/docs/configuration.md` · Test plan: `docs/v1-acceptance.md`.

### Changes by Kind

#### Bug or Regression
- price gemini-3.1-flash-lite + switch v1 smoke to it — adds `gemini-3.1-flash-lite` and `gemini-3-pro-preview` / `gemini-3-pro` entries to the built-in pricing table in `usage/pricing.go` (same rates as their preview counterparts) so the cost tracker no longer reports `$0.0000` for those models
- set include_server_side_tool_invocations for built-in + function tool combos — `models/gemini` sets `Config.ToolConfig.IncludeServerSideToolInvocations = true` whenever the `builtinsLLM` wrapper injects server-side built-ins (`google_search` / `url_context` / `code_execution`) alongside any function-calling tools; without this flag Gemini 3+ rejects the combined request with `Please enable tool_config.include_server_side_tool_invocations to use Built-in tools with Function calling`; surfaced by the v1.0.0 smoke pass (`docs/v1-acceptance.md`); fix in `models/gemini/builtins.go`
- drop docsy/dependencies submodule import (doesn't exist)

#### Documentation
- promote [Unreleased] to [1.0.0] + bump release pin + finalize sign-off
- document the Gemini fix + Gemini 3.0+ requirement for tool combos — added to `docs/site/content/docs/providers.md` and `docs/site/content/docs/configuration.md`
- Revert "docs(site): drop blocks/cover landing hero for theme-aware intro"
- drop blocks/cover landing hero for theme-aware intro
- switch inline-code tint to solid colors
- bump inline-code light-mode tint for visibility
- replace Bootstrap's loud inline-code color with a tint
- enable Docsy's light/dark/auto theme toggle in the navbar
- migrate from Hextra to Docsy + rewrite landing pages
- strip duplicate page H1s + rewrite home with cards landing
- add v1-acceptance.md test plan — switched smoke commands from `gemini-2.5-flash` (which can't combine built-ins with function tools) to `gemini-3.1-flash-lite` (the cheapest 3.x model, exercises the same code paths as `gemini-3.1-pro-preview`); Section 9 records the actual sign-off transcript from cutting this release

## [0.1.0] — 2026-05-16

**First tagged release.** Three milestones of work landed on `main` before this tag; the release is the consolidation rather than a discrete piece of work. **M1 + M2** shipped the core library (`agent`, `models`, `config`, `permissions`, `tools`, `mcp`, `skills`, `instruction`, `telemetry`, `usage`, `session`, `runner`, `recording` packages plus the `cmd/core-agent` bundled CLI). **M3** added autonomy + durable sessions + subagents: `agent.RunAutonomous` multi-turn driver with budgets and retry policy, `eventlog.Open(ctx, dialector)` returning a `*Handle` bundling a `session.Service` (wraps ADK's GORM-backed `database.SessionService`) and a `Stream` with monotonic seq numbers over SQLite (pure-Go via `glebarez/sqlite`, no CGO) / MySQL / Postgres, `eventlog.SessionLock` exclusive lease for `ResumeAutonomous` crash-resume (5s heartbeat, 30s staleness window, automatic theft on stale leases), `agent.WithSubagents` in-process delegation via `agent.NewSubagentTool` (subagent runs in a derived session row `<parent>:sub:<branch>` with `Branch="<parent>.<sub>"` for branch-scoped audit queries; depth cap 2), in-turn human consultation via `tools.NewAskUserTool` + three built-in `Prompter`s (`StdinPrompter`, `RefusePrompter`, `StaticPrompter`) driven by `--ask=stdin|auto|off`, and `tools.NewLifecycleTool` generic state emission.

Two adapters land in `extras/`: `extras/scion-agent/` packages core-agent for Scion's container runtime (lifecycle status, `--input`, `sciontool_status` tool, `--session-db` parity); `extras/ax-agent/` packages it as an AX (Agent eXecutor) gRPC remote agent (lives on the `axplore` branch since `github.com/google/ax` is currently private; same `--session-db` parity). Bundled CLI flags: `--provider`, `-m`, `-p`, `--no-builtin-tools`, `--disable-tools`, `--script`, `--script-strict`, `--record-to`, `--color`, `--ask`, `--session-db`, `--session-db-path` (default `~/.<binary>/sessions.db` derived from `os.Executable()`).

Design + plan docs: `docs/DESIGN.md` · `docs/autonomous-plan.md` · `docs/eventlog-plan.md` · `docs/eventlog-decisions.md` · `docs/subagents-plan.md` · `docs/tools-plan.md` · `docs/m3-followups-plan.md` · `docs/m3-followups-decisions.md`. Plan docs preserved as historical context with status headers; decisions docs are the canonical "what shipped + why" record. Examples: `basic/` (minimal one-turn agent), `with-tools/` (built-in tool suite), `streaming/` (`runner.WriteEvents` chat-style output), `replay/` (`mock.NewScripted` against a recorded transcript), `autonomous/` (`RunAutonomous` end-to-end with scripted mock), `autonomous-resume/` (crash-resume against SQLite), `with-subagent/` (parent + research subagent with branch-scoped audit log). New Hugo site pages: [Autonomous runs](https://go-steer.github.io/core-agent/docs/autonomous/), [Sessions and event log](https://go-steer.github.io/core-agent/docs/sessions/), plus comprehensive [Library API](https://go-steer.github.io/core-agent/docs/library-api/) covering subagents, autonomous, durable sessions, prompters, MCP, skills, telemetry, and transcripts.

Known gaps (not in this release; tracked for v0.2 candidates): subagent cost rollup into parent's `usage.Tracker` (subagent runs track usage internally; surfacing it back is a follow-up); Postgres / MySQL integration tests (multi-driver claim is verified for SQLite only — library callers can swap dialectors today; CI doesn't yet test Postgres); real-LLM end-to-end smoke (examples use scripted mocks; no automated smoke against actual Gemini / Anthropic); glob `**` recursive shorthand (explicitly out of scope — stdlib-only constraint; workaround is explicit walk root); Bubble Tea TUI + slash-command framework beyond `/exit` `/quit` (consumer concern, not library); Anthropic feature coverage (extended/adaptive thinking, structured outputs, server-side tools `web_search` / `code_execution`, vision); Amazon Bedrock + Claude Platform on AWS backends; auto-detection for `anthropic-vertex` from generic GCP env vars (explicit-only today); mid-run pause/resume for `RunAutonomous` (across-turn crash-resume shipped; mid-turn is a different design); native push for `Stream.Watch` (Postgres `LISTEN/NOTIFY`, SQLite `update_hook` — polling at 200ms today).

### Changes by Kind

#### Feature
- add WithSessionTree query option (M3 follow-up item 2)
- add glob + grep built-ins (M3 follow-up item 1)
- add subagents — NewSubagentTool + WithSubagents (Phase 4)
- add ResumeAutonomous + checkpoint events (Phase 3) — per-turn checkpoint events (`Author="<binary>/autonomous"`) land in the durable log; resume reads latest checkpoint and continues from the next turn; cross-binary resume via `WithAuthorSuffix("/autonomous")`; terminal-state short-circuit only on `Completed` so budget-exhausted runs can be resumed with a higher cap
- add SessionLock + WithAuthorSuffix (Phase 3 substrate) — exclusive lease on `(app, user, session)` via `Handle.AcquireLock`; 5s heartbeat, 30s staleness window, automatic theft on stale leases; concurrent attempts return `ErrSessionLocked` with the holder identifier
- wire eventlog through agent + CLIs (Phase 2)
- durable backend with seq + replay + watch (Phase 2) — `eventlog.Stream.Append` / `Since(fromSeq, opts...)` / `Watch(fromSeq, opts...)` / `Close`; query options `ForSession` / `WithSessionTree` / `WithBranchPrefix` / `WithAuthor` / `WithAuthorSuffix` / `WithLimit`; WAL mode default for SQLite; `Watch` polls at 200ms (`WithWatchInterval` to override)
- add WithSessionService option (eventlog plan, Phase 1)
- add RunAutonomous multi-turn driver — budgets via `WithMaxTurns` / `WithMaxTokens` / `WithMaxCost` / `WithMaxWallclock` / `WithPerTurnTimeout`; retry policy via `WithRetryPolicy` (`AbortRun` / `RetryTurn` / `SkipTurn`); permissions deadlock guard via `WithPermissionsGate`; returns structured `RunResult{Reason, Turns, Tokens, Cost, Duration, FinalText, DoneDetail}`
- add NewLifecycleTool generic status emitter — model uses it to signal "thinking" / "blocked" / "done" / custom labels; consumer-supplied handler decides where events go
- --ask flag wires ask_user tool into the bundled binary
- add NewAskUserTool + Prompter + 3 built-in prompters — `StdinPrompter`, `RefusePrompter`, `StaticPrompter` for in-turn human consultation
- bundled binary uses WriteEvents + --color flag
- WriteEvents WithColor option + IsTerminal helper
- add WriteEvents for chat-style streaming display
- surface server-side web_search built-in tool — Anthropic adapter
- credential-free echo + scripted providers and recording wrapper — `mock` package plus the `recording.NewRecorder(m, w)` LLM-wire recorder that pairs with `mock.NewScripted` for credential-free replay
- per-tool disable via --disable-tools and tools.disable
- scion-agent adapter for Scion's container runtime — lifecycle status, `--input`, `sciontool_status` tool, `--session-db` parity
- ship built-in tool suite (bash, file ops, todo) — eight default-on built-ins (`read_file`, `write_file`, `edit_file`, `list_dir`, `glob`, `grep`, `bash`, `todo`) plus `GateToolset` wrapper bridging the gate to ADK toolsets plus `Truncate` helper
- support Gemini built-in tools (Search, URLContext, CodeExecution)

#### Bug or Regression
- examples/replay: extract run() so deferred temp-file cleanup runs
- scion-agent: goimports order on the recording import
- ci: address first-CI-run lint + vuln findings

#### Documentation
- add CHANGELOG.md + Releases section in README for v0.1.0
- refresh plan docs with shipped status + decisions log (M3 follow-up item 3)
- add M3 follow-ups plan (glob/grep + WithSessionTree + plan-doc refresh)
- refresh README — M3 milestone entry + project layout + roadmap
- subagents: example + library-api + autonomous + sessions docs (Phase 4) — includes `examples/with-subagent/`
- site: add autonomous + sessions pages, surface new CLI flags
- add M3 plan for durable sessions + audit/replay event log
- examples: add streaming example showing WriteEvents + tools
- add M3 plans for subagent tool and glob/grep built-ins
- fix stale tools.Default(cfg, gate) signature references
- clarify Gemini built-in deferral rationale
- README + DESIGN.md mention Gemini built-in tools
- add DESIGN.md + Vertex Gemini quickstart in README

#### Other (Cleanup)
- bump go directive to 1.26.3 for stdlib vuln fixes
- move recorder out of models/mock to its own recording/ package
- tools: make BuiltinToolNames a function; derive --no-builtin-tools help text

#### Other
- Initial implementation: M1 + M2 + project scaffolding — `agent` package (wraps ADK's `llmagent` + `runner` with the `Option` pattern: `WithAppName`, `WithName`, `WithDescription`, `WithInstruction`, `WithStreaming`, `WithSession`, `WithTools`, `WithToolsets`, `WithSystemInstructionPrefix`; `Agent.Run(ctx, prompt)` streams ADK events for one turn), `models` package (`Provider` interface + registry; backends `gemini` (api.google.com + Vertex), `anthropic` (api.anthropic.com + `anthropic-vertex`), `mock` (echo + scripted)), `config` package (`.agents/config.json` schema + discovery + atomic persist; per-tool output caps via `ToolOutput.PerTool`), `permissions` package (ask/allow/yolo gate, pattern grammar, path scope, non-overridable bash denylist, `Prompter` interface), `tools` package (initial built-ins + `GateToolset` wrapper + `Truncate` helper), `mcp` package (`mcp.json` schema, stdio + Streamable HTTP transports, env-var interpolation, namespacing), `skills` package (Claude-compatible `SKILL.md` discovery → ADK `skilltoolset`), `instruction` package (`AGENTS.md` / `CLAUDE.md` / `GEMINI.md` fallback loader; user-global memory at `~/.core-agent/AGENTS.md`), `telemetry` package (opt-in OpenTelemetry exporter setup — console / OTLP / none), `usage` package (per-turn token tracker + cost helpers + built-in Gemini pricing table), `session` package (JSON transcript persistence at `.agents/sessions/<timestamp>.json` for one-shot runs), `runner` package (`Headless` one-shot + `REPL` multi-turn drivers), `cmd/core-agent` bundled CLI
- first commit

