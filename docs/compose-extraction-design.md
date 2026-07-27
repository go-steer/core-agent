# Extract reusable substrate from `cmd/core-agent` into `pkg/compose`

Roughly 5.5-6k lines of substrate-grade wiring — multi-session construction, the attach-only wake loop, agentic-tool assembly, the MCP digest LLM fallback, context-cache wiring, compactor construction, startup-summary formatting, and (critically) the only real implementation of "allow always" grant persistence — currently sit behind `package main` and are unreachable by cogo, scion, and ax. This doc lifts that logic into a new library package, `pkg/compose`, and promotes grant persistence to a first-class `pkg/permissions` API so the gate's "allow always" contract has a supported library-side implementation. The seam is deliberate: reusable *policy* moves; flag parsing, process wiring, `os.Exit`, and the TUI binary stay put.

**Status:** in progress (2026-07-27). Human-led / API-shape; design-doc first per docs/cleanup-execution-plan.md (Wave 3). #388 completed first as planned (all four phases on main). PR 1 (`permissions.GrantStore` + gate wiring) landed.

**Tracking issue:** [#386](https://github.com/go-steer/core-agent/issues/386)

## Sequencing relative to #388

This work is stacked **after** the `pkg/agent` split ([#388](https://github.com/go-steer/core-agent/issues/388)). #388 decomposes the agent package into `pkg/agent` (core), `pkg/agent/autonomous` (driver), `pkg/agent/background` (manager), and moves the 22 `Attach*` methods off `*agent.Agent` into a `pkg/attachadapter`. `pkg/compose` targets that post-split surface:

- `reproduceAgent` and `attachProviderOpts` construct agents via `agent.New` + `agent.Option` and wire the `WithAttachXProvider` options. After #388 the attach-provider options belong to `pkg/attachadapter`; compose targets them there.
- The attach-options helper (below) builds an `attachadapter`-shaped options value, not the current main-local `attachOpts`.

The #388 design doc (`docs/agent-package-split-design.md`) is on `main` and its phase 1 (the accessor seam) has landed; phases 2-4 are in flight. The phasing below marks which compose PRs are #388-blocked (need the autonomous/background/attachadapter packages to exist) and which are not.

## Why now

Every downstream consumer of core-agent-as-a-library re-implements the same wiring. Four examples of substrate that is correct, tested, and load-bearing, but stranded in `package main`:

- **On-demand multi-session construction** (`multi_session.go`, 520 LOC). `buildSessionFactory` / `reproduceAgent` / `buildSessionResumer` / `runSessionWakeLoop` are exactly what any daemon serving `POST /sessions` needs. cogo and scion each want this and cannot import it.
- **Grant persistence** (`tui_callbacks.go`). This is the *only* code in the tree that writes `/allow`, `/deny`, `/model`, `/theme`, and "allow always" decisions back to `.agents/config.json`. The permission gate promises `DecisionAllowAlways` means "persist a permanent allowlist entry" (prompter.go:31), but the gate never persists anything — see below. A library consumer that wires its own prompter gets a broken contract.
- **Agentic + digest wiring** (`agentic.go`, `mcp_digest_llm.go`, `context_cache.go`, `compactor.go`). Pure "given config, build the substrate object" functions with no CLI dependency.
- **Operator-visible formatting** (`startup_summary.go`, `renderContextStats`). Deterministic, table-tested formatters that any embedding surface wants.

The value of this doc is the seam, not the file moves. The seam is: *reusable substrate is library; flag parsing and process wiring are binary.*

## Current shape

Grounded in the actual files (LOC includes the license header):

| File | LOC | Extractable surface | Home |
|---|---|---|---|
| `multi_session.go` | 520 | `buildMultiSessionAuthn`, `sessionFactoryDeps`, `buildSessionFactory`, `reproduceAgent`, `buildSessionResumer`/`sessionResumer`, `attachProviderOpts`, `newSessionID`, `newSessionTracker` | compose (wake loop → runner) |
| `tui_callbacks.go` | 275 | `appendPathScope`, `appendPermissionsAllow`, `appendPermissionsDeny`, `appendBuiltinAllowExtra`, `persistModelChoice`, `persistThemeChoice`; pricing helpers (`describeRefresh`, `cfgToCatalogOverride`, `rebuildPricingCatalog`, `refreshPricingForTUI`, `setPricingForTUI`, `summarizeRefreshOutcome`) | compose (grant store + pricing) |
| `agentic.go` | 250 | `buildAgenticTools`, `renderContextStats`, `toolNames` | compose |
| `startup_summary.go` | 238 | `formatStartupSummary` + `format*Line` helpers, `startupSummaryInputs` | compose |
| `mcp_digest_llm.go` | 146 | `buildMCPDigestLLMFallback`, prompt const, tracer | compose |
| `context_cache.go` | 130 | `maybeWireContextCache` | compose |
| `allow_path.go` | 64 | `parseAllowPathSpec` | stays in main (flag-value parser) |
| `compactor.go` | 52 | `buildCompactor` | compose |
| `main.go` (subset) | ~200 | task-class precedence block (463-514), small-tier-parent guard (672-721), `--no-repl` wake loop (1615-1679), `mergeAttachOpts` (347-375), log filter (`installLogFilter`/`filteredLogWriter`, 1772-1802) | split — see below |

The `--no-repl` wake loop in `main.go` (1654-1678) and `runSessionWakeLoop` in `multi_session.go` (392-414) are the **same loop written twice**, and both hand-roll the per-turn usage tap that `pkg/runner` already owns as `usage.TurnTap` (`runner/headless.go:90` `tapTracker`).

### The gap in the "allow always" contract

`permissions.Decision` documents `DecisionAllowAlways` as "persist a permanent allowlist entry, then allow" (prompter.go:31). The gate does not keep that promise:

- For `PromptKindPathScope`, `gate.go:691-722` mutates the in-memory scope (`g.scope.AddAlwaysAllow`) but writes nothing to disk.
- For every other kind, it only calls `rememberSession` — it does not even add a policy pattern. The grant lasts one session.

Permanence is delivered entirely outside the library, by the TUI's `AlwaysAllow` closure (`coretui_enabled.go:201-216`), which calls `deps.Gate.AddAllowPatterns` **and** `appendPermissionsAllow` (the disk write in `tui_callbacks.go`). The stdin prompter (`stdin.go:77`) returns `DecisionAllowAlways` with no persistence path at all, so headless "allow always" silently degrades to allow-session. Any consumer that isn't the bundled TUI inherits a decision constant whose documented behavior does not happen. Decision #2 of this issue fixes that by making persistence a gate responsibility backed by a library interface.

## What moves vs. what stays

**Moves to `pkg/compose`:**

- Multi-session construction: the `sessionFactoryDeps` bundle and `buildSessionFactory` / `reproduceAgent` / `buildSessionResumer` / `attachProviderOpts` / `newSessionID` / `buildMultiSessionAuthn`. `reproduceAgent` targets the post-#388 `agent.New` surface; `attachProviderOpts` targets `pkg/attachadapter`. The `newSessionTracker` package-var test seam moves intact.
- Substrate builders: `buildAgenticTools`, `buildMCPDigestLLMFallback` (+ prompt const + tracer), `maybeWireContextCache`, `buildCompactor`.
- Formatters: `formatStartupSummary` (+ helpers + `startupSummaryInputs` renamed to an exported input struct), `renderContextStats`.
- Pricing operations: `refreshPricingForTUI`, `setPricingForTUI`, `rebuildPricingCatalog`, `cfgToCatalogOverride`, `describeRefresh`, `summarizeRefreshOutcome`.
- Grant persistence: the config-file `GrantStore` implementation and the exported `Append*` / `Persist*` helpers (see next section).
- The reusable log-filter writer (the `filteredLogWriter` type + `droppedLogPatterns`), exposed as a constructor returning an `io.Writer`.

**Moves to `pkg/runner`:**

- A single `runner.WakeLoop` (see open question below) that replaces both the inline `--no-repl` loop and `runSessionWakeLoop`, reusing `usage.TurnTap`.

**New in `pkg/permissions`:**

- The `GrantStore` interface + `Grant` type, and gate wiring so `DecisionAllowAlways` drives persistence.

**Stays in `package main` (the binary seam):**

- All flag registration, `flag.FlagSet`, `run()` orchestration, signal handling, `os.Exit` / exit codes.
- `parseAllowPathSpec` — parses a CLI flag *value*; flag-syntax-specific and cannot live in `pkg/config` (cycle).
- The CLI-precedence half of `mergeAttachOpts` — it needs `flag.CommandLine.Visit` to know which flags were explicitly set. The config→options translation (with `os.ExpandEnv`) moves to `compose.BuildAttachOptions`; main keeps the thin "CLI overrides config" overlay.
- The task-class precedence block and small-tier-parent guard **as orchestration**. These read `cfg`, mutate `cfg`, and emit operator lines through `send`. The reusable pieces they call (`taskclass.Resolve`, `taskclass.ModelForTier`, `modeltier.IsSmall`) already live in `pkg/taskclass` / `pkg/modeltier`. What remains is precedence glue tied to CLI flag values and `run()`'s control flow (it can `return runner.ExitConfigError`). It stays in main; if a consumer needs it, the right follow-up is a small `compose.ResolveTaskClass(cfg, overrides)` returning a result struct, not lifting the flow verbatim. Called out as an open boundary, not extracted in v1.
- The process-global `log.SetOutput(...)` call (main wraps `os.Stderr` with `compose.NewFilteredLogWriter`).

## The grant-persistence API

The design makes persistence a gate responsibility backed by an injectable store, so the "allow always" contract holds for every consumer, not just the bundled TUI.

### Interface (in `pkg/permissions`)

```go
// Grant is one "allow always" decision the gate resolved and wants
// persisted beyond the current session.
type Grant struct {
	// Kind mirrors the PromptRequest.Kind that produced the grant.
	Kind PromptKind

	// Tool and Key are the persistence coordinates carried on the
	// PromptRequest (PersistTool / PersistKey).
	Tool string
	Key  string

	// Pattern is the fully-expanded entry the gate installed
	// in-memory: the "<Tool>:<Key>" policy pattern, or the
	// subtree-expanded path pattern from expandAlwaysAllowPattern.
	// Persist this verbatim so a restart reloads the identical grant.
	Pattern string

	// Access is the resolved file access for PromptKindPathScope
	// grants, after the read->r / write->rw promotion. Zero
	// (AccessNone) for non-path grants.
	Access Access
}

// GrantStore persists "allow always" grants so they survive process
// restart. The gate calls Persist from its DecisionAllowAlways path,
// immediately after installing the grant in-memory.
//
// Persist must be idempotent (re-granting an existing pattern is a
// no-op) and safe for concurrent use. A nil GrantStore disables
// persistence — the grant still applies for the current session.
type GrantStore interface {
	Persist(ctx context.Context, g Grant) error
}
```

### Gate wiring

`Options` gains a `GrantStore GrantStore` field; `New`/`FromConfig` thread it in, and a `SetGrantStore` setter mirrors `SetPrompter` for the mid-startup UI swap. `DeriveForSession` shares the template's store by reference — consistent with the existing documented rule that `AddAllowPatterns` / `AddAlwaysAllow` mutations are daemon-wide (gate.go:315-327).

The `DecisionAllowAlways` branch (gate.go:691) becomes, after installing the in-memory grant:

```go
case DecisionAllowAlways:
	g.rememberSession(req.ToolName, req.Detail)
	grant := Grant{Kind: req.Kind, Tool: req.PersistTool, Key: req.PersistKey}
	if req.Kind == PromptKindPathScope {
		access := req.Access
		switch access {
		case AccessNone:
			access = AccessRead
		case AccessWrite:
			access = AccessReadWrite
		}
		grant.Pattern = expandAlwaysAllowPattern(req.PersistKey)
		grant.Access = access
		g.scope.AddAlwaysAllow(grant.Pattern, access)
	} else {
		// Non-path grants become a real policy pattern now, closing
		// today's gap where DecisionAllowAlways for bash/generic only
		// remembered the session.
		grant.Pattern = req.PersistTool + ":" + req.PersistKey
		if err := g.policy.AddAllow([]string{grant.Pattern}); err != nil {
			return err
		}
	}
	if g.grants != nil {
		if err := g.grants.Persist(ctx, grant); err != nil {
			return err // grant did not persist; surface, don't swallow
		}
	}
	g.recordApproval(req.ToolName, req.Detail, d)
	return nil
```

This is a deliberate behavior change: the in-memory policy add for non-path grants, and the disk write via the store, now happen inside the library. Bundled-TUI behavior is unchanged (it already did both, one layer up); the win is that stdin and any custom prompter now honor the contract.

### Reference implementation (in `pkg/compose`)

```go
// ConfigGrantStore persists grants into .agents/config.json
// (permissions.allow / path_scope.allow), atomically, via
// config.Load + config.Save. Lifted from cmd/core-agent/tui_callbacks.go.
//
// AgentsDir empty => Persist is a no-op, matching the TUI's
// "AgentsDir unwritable => fall back to allow-session" behavior.
type ConfigGrantStore struct {
	AgentsDir string
}

func (s *ConfigGrantStore) Persist(ctx context.Context, g permissions.Grant) error {
	if s.AgentsDir == "" {
		return nil
	}
	switch g.Kind {
	case permissions.PromptKindPathScope:
		return AppendPathScope(s.AgentsDir, g.Pattern)
	default:
		return AppendPermissionsAllow(s.AgentsDir, []string{g.Pattern})
	}
}
```

`config.Save` is already atomic (temp-file + rename, persist.go:25) and the `Append*` helpers are already idempotent (they scan the existing slice before appending), so `Persist` satisfies the interface's idempotency and concurrency contract as-is.

The explicit slash commands `/allow`, `/deny`, `/model`, `/theme`, and bundle enable/disable are **not** gate `DecisionAllowAlways` events — they are direct operator actions. Their helpers stay as exported compose functions (`AppendPermissionsAllow`, `AppendPermissionsDeny`, `AppendBuiltinAllowExtra`, `PersistModelChoice`, `PersistThemeChoice`) so cogo/scion/ax reuse the same code the TUI does. Main's TUI callbacks (`coretui_enabled.go`) delegate to these instead of the now-removed unexported functions.

## Package layout

```
pkg/compose/                    <- NEW library package
  multisession.go     buildMultiSessionAuthn, SessionFactoryDeps,
                      BuildSessionFactory, ReproduceAgent,
                      BuildSessionResumer, attachProviderOpts, newSessionID
  agentictools.go     BuildAgenticTools, toolNames
  contextstats.go     RenderContextStats
  mcpdigest.go        BuildMCPDigestLLMFallback (+ prompt, tracer)
  contextcache.go     MaybeWireContextCache
  compactor.go        BuildCompactor
  startupsummary.go   FormatStartupSummary, StartupSummaryInputs
  pricing.go          RefreshPricing, SetPricing, RebuildPricingCatalog,
                      cfgToCatalogOverride, DescribeRefresh, SummarizeRefreshOutcome
  grantstore.go       ConfigGrantStore, AppendPathScope,
                      AppendPermissionsAllow/Deny, AppendBuiltinAllowExtra,
                      PersistModelChoice, PersistThemeChoice
  attachopts.go       BuildAttachOptions (config -> attachadapter options)
  logfilter.go        NewFilteredLogWriter

pkg/permissions/
  grant.go            Grant, GrantStore (+ gate wiring in gate.go)

pkg/runner/
  wakeloop.go         WakeLoop (replaces the two hand-rolled loops)
```

Compose is a **leaf consumer**: it imports `pkg/agent`, `pkg/attachadapter`, `pkg/attach`, `pkg/auth`, `pkg/config`, `pkg/permissions`, `pkg/mcp`, `pkg/models`, `pkg/runner`, `pkg/usage`, `pkg/instruction`, `pkg/skills`, `pkg/taskclass`, `pkg/modeltier`, and `internal/pricing` + `internal/vertexcache`. Nothing in those packages imports compose, so there is no cycle risk. The `internal/*` imports are legal within the module and resolve fine for external consumers of `pkg/compose` (they never import `internal/` directly).

## Phasing (stacked PRs, each independently green)

Ordered so each PR compiles and tests clean on its own. PRs marked **[#388]** are blocked on the agent split.

**PR 1 — `permissions.GrantStore` + gate wiring.** Add `Grant`, `GrantStore`, `Options.GrantStore`, `SetGrantStore`, and the `DecisionAllowAlways` change. Behavior is unchanged when the store is nil and no policy-add regresses existing TUI behavior. Pure library; ships with gate tests covering the new persist call and the non-path policy-add. Not #388-blocked.

**PR 2 — `pkg/compose` skeleton + zero-dependency builders/formatters.** Move `BuildCompactor`, `FormatStartupSummary`, `RenderContextStats`, `NewFilteredLogWriter`, `BuildAgenticTools`, `BuildMCPDigestLLMFallback`, `MaybeWireContextCache`, and the pricing helpers. Repoint `main.go` and delete the old files. Their existing tests (`compactor_test.go`, `startup_summary_test.go`, `agentic_test.go`, `context_cache_test.go`) move with them. Not #388-blocked (these touch `agent.New`/`ContextStats`/`RunSubtask`, all stable across the split).

**PR 3 — `ConfigGrantStore` + persist helpers into compose.** Move the `Append*` / `Persist*` helpers, add `ConfigGrantStore`, and wire `Options.GrantStore = &compose.ConfigGrantStore{AgentsDir: agentsDir}` in main's gate construction. Repoint the TUI callbacks (`coretui_enabled.go`) to the exported compose helpers. Removes the split-brain contract. Not #388-blocked.

**PR 4 — `runner.WakeLoop` consolidation.** Add `runner.WakeLoop` built on `usage.TurnTap`; replace the inline `--no-repl` loop in `main.go` and (in the next PR) `runSessionWakeLoop`. This PR replaces only the `main.go` copy so it stays green stand-alone. Not #388-blocked.

**PR 5 — [#388] multi-session construction into compose.** Move `buildMultiSessionAuthn`, `SessionFactoryDeps`, `BuildSessionFactory`, `ReproduceAgent`, `BuildSessionResumer`, `attachProviderOpts`, `newSessionID`, and the `newSessionTracker` seam. `ReproduceAgent` targets the split `agent.New` surface; `attachProviderOpts` targets `pkg/attachadapter`. Wake loop calls `runner.WakeLoop`. `multi_session_test.go` moves along, keeping the tracker-capture regression for #275.

**PR 6 — [#388] `compose.BuildAttachOptions`.** Move the config→options translation (with `os.ExpandEnv`) targeting the `attachadapter` options shape; main keeps the CLI overlay that decides flag-vs-config precedence. `attach_merge_test.go` splits: the config-translation cases move to compose, the CLI-precedence cases stay in main.

## Non-goals

- **Extracting flag parsing.** No `flag.FlagSet`, flag registration, `run()` orchestration, signal handling, or exit-code logic moves. This is why the package is `compose`, not `cli`: it composes substrate, it does not parse a command line.
- **Moving the TUI.** `coretui_enabled.go` and the bubble-tea integration stay in the binary; compose only supplies the callbacks they invoke.
- **Changing multi-session isolation semantics.** Per-session policy/scope carve-outs remain deferred per docs/multi-session-design.md; grants stay daemon-wide.
- **A plugin/registry system.** Compose is plain functions and structs. No dynamic registration, no DI container.
- **Promoting `internal/pricing` or `internal/vertexcache` to `pkg/`.** Compose consumes them as-is; promotion waits for a concrete external need.
- **Lifting the task-class / small-tier-parent flow verbatim.** The reusable primitives already live in `pkg/taskclass` and `pkg/modeltier`; only CLI-bound precedence glue remains, and it stays in main.

## Open questions for review

- **Wake-loop home.** The `--no-repl` loop is duplicated in `main.go` and `multi_session.go`, and both hand-roll the usage tap that `pkg/runner` already owns (`usage.TurnTap`). `runner/headless.go` is one-shot and has no wake loop. Recommendation: consolidate into a new `runner.WakeLoop` that both call, rather than a compose helper — runner is already the "drive the agent through a conversation" package. Compose then consumes it. Alternative: keep it in compose.
- **`DecisionAllowAlways` behavior change.** ~~Ship as a fix, or gate behind opt-in?~~ **Resolved (PR 1, 2026-07-27): shipped as a fix.** Non-path "allow always" installs a real policy pattern; the nil-store default keeps persistence off, and `Persist` errors surface to the gated call. Documented in the CHANGELOG Feature entry.
- **`internal/` coupling.** Compose depends on `internal/pricing` and `internal/vertexcache`. Legal and transitively fine for external consumers, but cogo/scion/ax cannot reconfigure those internals through compose. Recommendation: accept for v1; defer promoting them to `pkg/` until a consumer needs it.

## Risks

- **Behavior change on the stdin/headless "allow always" path.** PR 1 makes non-path "allow always" add a real policy pattern and persist via the store, where today it lasts one session. This is a bug fix relative to the documented contract, but it is observable. Mitigation: land it as an explicit, documented fix; the nil-store default preserves the old behavior for anyone who wants it.
- **`internal/` coupling.** Compose depends on `internal/pricing` and `internal/vertexcache`. Legal and transitively fine for external consumers, but it means cogo/scion/ax cannot reconfigure those internals through compose. Accepted for v1; revisit only if a consumer needs to.
- **#388 sequencing.** PRs 5-6 cannot land until the agent split and `pkg/attachadapter` exist. PRs 1-4 are independent and can proceed in parallel with #388. If #388 slips, the high-value grant-persistence fix (PRs 1, 3) still ships.
- **Test-seam preservation.** `newSessionTracker` (the package-var indirection that lets `multi_session_test.go` assert distinct per-session trackers, the #275 regression gate) must survive the move as an exported-or-package-level seam in compose. Called out so the move doesn't quietly inline it.
- **Grant `Persist` error surfacing.** The gate now returns `Persist` errors to the caller instead of silently applying an in-memory-only grant. A read-only `.agents` dir that previously "worked" (allow-session) will now surface an error on "allow always". The `ConfigGrantStore` no-ops on empty `AgentsDir`, but a present-but-unwritable dir will error — arguably correct (the operator asked to persist and it failed), but a behavior operators may notice. Recommend documenting in the release notes.
