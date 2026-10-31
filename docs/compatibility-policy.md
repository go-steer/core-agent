# Public API Compatibility & Deprecation Policy

**Status:** Settled ([#1368](https://github.com/go-steer/core-agent/issues/1368), [#1373](https://github.com/go-steer/core-agent/issues/1373), [#1398](https://github.com/go-steer/core-agent/issues/1398))

This document defines which packages in `github.com/go-steer/core-agent/v3` carry a Semantic Versioning compatibility promise, how CI enforces that promise on every pull request, and how deprecated symbols are tracked to removal.

## Settled decisions (do not relitigate)

1. **The promised surface is an explicit subset of `pkg/`, recorded in [`dev/api-surface.txt`](../dev/api-surface.txt).** Exporting a package under `pkg/` so that `cmd/`, `internal/`, or sibling repositories can share wiring does not automatically place it under the SemVer guarantee. Every importable library package in `pkg/` is classified in `dev/api-surface.txt` as either `promised` or `unsupported`.
2. **`TestEveryImportablePackageIsADecision` gates new packages.** Adding a new importable package under `pkg/` without classifying it in `dev/api-surface.txt` fails `go test ./internal/testutil`.
3. **`dev/tools/verify-apidiff` gates incompatible changes to `promised` packages.** On every PR, CI compares each `promised` package against the nearest GA release tag (`v[1-9]*`, excluding `*-*` pre-releases) using `golang.org/x/exp/cmd/apidiff`.
4. **Every `Deprecated:` doc comment in `pkg/` ends with `Removed in v<N>.<M>.`.** `TestDeprecationMarkersNameTheirRemoval` in `internal/testutil/deprecation_test.go` parses every non-test Go file under `pkg/` and fails on any `Deprecated:` paragraph without a concrete removal version.

## Out of scope

- Moving `unsupported` packages under `internal/` before v4.0 when external sibling modules (`go-steer/cogo`, `extras/`) still import them.
- Enforcing `apidiff` on `cmd/`, `internal/`, `dev/`, `examples/`, or `unsupported` packages in `dev/api-surface.txt`.

---

## 1. Promised vs. unsupported packages (`dev/api-surface.txt`)

[`dev/api-surface.txt`](../dev/api-surface.txt) partitions `pkg/` into two dispositions:

### `promised` packages (SemVer-governed)

The 22 `promised` packages form the supported embedding API of `core-agent`:

- `pkg/agent`, `pkg/agent/autonomous`, `pkg/agent/background`
- `pkg/config`
- `pkg/eventlog`
- `pkg/instruction`
- `pkg/mcp`
- `pkg/models`, `pkg/models/anthropic`, `pkg/models/gemini`, `pkg/models/mock`, `pkg/models/profiles`
- `pkg/permissions`
- `pkg/pricing`
- `pkg/recording`
- `pkg/runner`
- `pkg/skills`
- `pkg/telemetry`
- `pkg/tools`
- `pkg/transcript`
- `pkg/usage`
- `pkg/watchdog`

Within a major series (`v3.x`), exported types, functions, methods, constants, and variables in `promised` packages do not change incompatibly in minor (`v3.X.0`) or patch (`v3.X.Y`) releases. Additive changes (new exported symbols, new optional struct fields, new functional options) ship in minor releases; bug fixes that preserve signatures ship in patch releases.

### `unsupported` packages (composition-only)

Packages marked `unsupported` in `dev/api-surface.txt` live under `pkg/` so `cmd/core-agent`, `cmd/core-agent-tui`, `examples/`, and sibling binaries can share daemon/CLI wiring, wire protocols, or internal routing tables without duplicating code:

- `pkg/agentenv` — daemon and sandbox environment readers (`CORE_AGENT_*`)
- `pkg/approver` — model-backed approver behind experimental permission mode `auto`
- `pkg/attach` — HTTP/SSE remote-attach server wire protocol and handler
- `pkg/attachadapter` — bridge adapting `pkg/agent` onto `pkg/attach`
- `pkg/auth` — static bearer-token authentication (moving to `go-steer/purser`)
- `pkg/childenv` — child-process credential-stripping helper
- `pkg/compose` — CLI/daemon composition glue lifted out of `cmd/core-agent`
- `pkg/digest` — local digesting primitives backing `pkg/tools/agentic`
- `pkg/hooks` — lifecycle hook runner
- `pkg/modeltier` — model-tier routing table
- `pkg/session` — deprecated forwarding alias for `pkg/transcript` (removed in v4.0)
- `pkg/taskclass` — task-classification heuristics
- `pkg/tools/agentic` — agentic-MCP wrapper toolset
- `pkg/tools/alert` — outbound webhook `alert` tool implementation
- `pkg/tools/peer` — peer-session messaging tools

`unsupported` packages may change signatures between minor releases without a major version bump.

---

## 2. Automated API diff check (`dev/tools/verify-apidiff`)

`dev/tools/verify-apidiff` (and its CI delegator `dev/ci/presubmits/verify-apidiff`) runs `golang.org/x/exp/cmd/apidiff` against the nearest GA release tag reachable from `HEAD` (`git describe --tags --abbrev=0 --match 'v[1-9]*' --exclude '*-*'`).

For every `promised` package in `dev/api-surface.txt` that existed at the baseline tag:

1. An API snapshot of the baseline tag is exported from `git archive <base-tag>`.
2. `apidiff` compares the baseline snapshot against the current working tree.
3. Any line under `Incompatible changes:` fails the presubmit unless it is explicitly acknowledged in either:
   - [`dev/api-breaks.txt`](../dev/api-breaks.txt) (`<package> <symbol> # <issue-or-reason>`), or
   - a `**BREAKING:**` bullet under `## [Unreleased]` in [`CHANGELOG.md`](../CHANGELOG.md) whose line names **both** the package (`` `pkg/foo` `` or `` `foo` ``) **and** the symbol (`` `Bar` ``, `` `Type.Method` `` or `` `foo.Bar` ``) in backticks. One bullet acknowledges the breaks it names, not every later break in that package.

`dev/api-breaks.txt` is cleared when cutting a new major release tag.

---

## 3. Deprecation policy & the v4.0 removal schedule (`#1373`)

When a symbol or package in `pkg/` is superseded:

1. Keep the old symbol working as a thin forwarder or alias for the remainder of the current major series.
2. Add a Go doc comment paragraph starting with `Deprecated:` that names the replacement and ends with `Removed in v<N>.<M>.` (for example, `// Deprecated: use Foo instead. Removed in v4.0.`).
3. `internal/testutil.TestDeprecationMarkersNameTheirRemoval` enforces that every `Deprecated:` marker in `pkg/` ends with a concrete `Removed in v<N>.<M>.` sentence.

### Symbols scheduled for removal in v4.0

| Package | Deprecated symbol(s) | Replacement | Removal target |
|---------|----------------------|-------------|----------------|
| `pkg/agent` | `DefaultInstruction`, `WithSystemInstructionPrefix` | `WithMode`, `WithExtraInstruction`, `WithUserInstruction` | `v4.0` |
| `pkg/agent` | `(*Agent).SetAttachEmitter` | `(*Agent).SetOperatorEventEmitter` | `v4.0` |
| `pkg/agent/autonomous` | `AutonomousHandle`, `AutonomousOption`, `RunAutonomous`, `StartAutonomous`, `ResumeAutonomous` | `Handle`, `Option`, `Run`, `Start`, `Resume` | `v4.0` |
| `pkg/attach` | `EmitTarget` | `OperatorEventTarget` | `v4.0` |
| `pkg/attachadapter` | `(*Adapter).SetAttachEmitter` | `(*Adapter).SetOperatorEventEmitter` | `v4.0` |
| `pkg/compose` | `ConfigGrantStore`, `AppendPathScopeEntry` | `permissions.ConfigGrantStore`, `permissions.AppendPathScopeEntry` | `v4.0` |
| `pkg/compose` | `AppendPathScope`, `AppendPermissionsAllow`, `AppendPermissionsDeny`, `AppendBuiltinAllowExtra`, `PersistModelChoice`, `PersistThemeChoice` | `config.AppendPathScope`, `config.AppendPermissionsAllow`, `config.AppendPermissionsDeny`, `config.AppendBuiltinAllowExtra`, `config.PersistModelChoice`, `config.PersistThemeChoice` | `v4.0` |
| `pkg/config` | `PermissionsConfig.RequirePlanArtifact` | `PermissionsConfig.PlanMode` (read via `ResolvedPlanMode`) | `v4.0` |
| `pkg/models/anthropic` | `WithCacheSystem` | `WithPromptCache` | `v4.0` |
| `pkg/models/gemini` | `GroundingProjection` | `github.com/go-steer/core-models/adkv2.GroundingProjection` | `v4.0` |
| `pkg/runner` | `REPL`, `REPLWithInitialPrompt`, `REPLWithAgent`, `REPLWithAgentAndInitialPrompt` | `runner.Run(ctx, runner.RunOptions{...})` | `v4.0` |
| `pkg/session` | Entire package (`SchemaVersion`, `SessionsDirName`, `Transcript`, `Message`, `Usage`, `ErrNoProject`, `Save`) | `pkg/transcript` | `v4.0` |

### Overlapping entry-point families tracked for v4.0 consolidation

In addition to the 33 deprecated symbols above (including `pkg/runner.REPL*`, which already forward to `runner.Run(ctx, runner.RunOptions{...})`), two multi-method entry-point families on `pkg/agent.Agent` are tracked under [#1373](https://github.com/go-steer/core-agent/issues/1373) for option-struct consolidation before `v4.0`:

- **Message injection (`Agent.Inject`, `Agent.InjectAs`, `Agent.InjectAsContext`, `Agent.InjectAsContextWithID`, `Agent.QueueAsContext`, `Agent.QueueAsContextWithID`)**: six methods differing on caller attribution, context cancellation, prompt-ID return, and immediate-wake vs. queue-only delivery.
- **Operator-hold resumption (`Agent.Resume`, `Agent.ResumeWithMode`, `Agent.ResumeWith`)**: three methods differing on whether a resume mode (`steer` / `continue` / `abandon`) and an atomic steering message + caller identity are supplied when releasing the pause gate.
