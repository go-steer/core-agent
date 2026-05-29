# Cogo → core-agent flip: readiness audit (2026-05-26)

Companion to [`cogo-core-agent-integration.md`](cogo-core-agent-integration.md) (which laid out strategy A/B/C and recommended **C sequenced through A**). This note records the readiness audit done after operator-input PRs α/β/γ landed and while the core-tui adapter work is in flight — the goal is to lock in what blocks the flip and what doesn't, so the v2.0 cut can be planned around a known checklist instead of vibes.

## TL;DR

Flip is **mostly mechanical**. Three things to do on the core-agent side, everything else is `import` + `WithX(...)` registration on the cogo side.

- ✅ **Already covered by core-agent's existing surface:** permissions, MCP, skills, session, models, instruction loading, autonomous + subagents, slash commands (core-agent's set is a strict superset of cogo's). No monkey-patching in cogo, no reach-into-internals, no custom session/MCP/elicitor shapes.
- 🛠️ **Three deltas to close before flipping:** see "Action items" below.

## Audit findings by category

| Category | Cogo today | Core-agent today | Verdict |
|---|---|---|---|
| Custom tools | 6 Go-specific: `go_doc`, `go_build`, `go_test`, `go_vet`, `go_symbol_find`, `go_implements` (in `cogo/internal/tools/`) | None of these | **Keep in cogo, register via `WithTools(...)` on flip.** No core-agent change needed. |
| Slash commands | /help /clear /quit /memory /stats /model /mcp /skills /reload /mouse /permissions /allow /deny | All of cogo's + /tools /interrupt /pricing /btw /subagent | Already covered (superset). |
| Session backends | In-memory transcript only (`cogo/internal/session/transcript.go`) | In-memory + SQLite/Postgres + eventlog + crash-resume | Already covered; cogo gains durability for free. |
| MCP loaders + elicitor | `cogo/internal/mcp/` (config, lifecycle, namespace, elicitation) | `core-agent/mcp/` (same shape) | Already covered. |
| Skills loader | `cogo/internal/skills/discovery.go` | `core-agent/skills/load.go` | Already covered. |
| Permissions | `cogo/internal/permissions/` (gate, builtin_allow, recommend, policy, scope, verb, denylist, prompter) | Same files + `stdin.go`, `serialize.go`, snapshot support | Already covered (superset). |
| Autonomous + subagents | None | `agent.RunAutonomous`, `BackgroundAgentManager`, spawn/list/check tools | Already covered; cogo gains them on flip. |
| Config schema | Cogo has `UIConfig` (theme, mouse) that core-agent lacks | Core-agent has `Tools`, `Mock`, `Attach`, `Pricing` that cogo lacks | **Add `UIConfig` to `core-agent/config`** as part of the core-tui-default work (the chip switcher needs theme/mouse anyway). |
| Model providers | `cogo/internal/models/gemini/` | `gemini`, `anthropic`, `mock` | Already covered (superset). |
| Custom agent wrapper | `cogo/internal/agent/agent.go` mirrors `core-agent/agent/agent.go` (duplicate) | The real thing | **Delete cogo's wrapper on flip;** `cmd/cogo/main.go` calls `core-agent`'s `agent.New` directly with `WithAppName("cogo") + WithInstruction(cogoDefaults)`. |
| CLI flags | `-p -debug -yolo -permissions -h -version` | All of cogo's + `-c -m -provider -no-builtin-tools -disable-tools -script -color -ask -session-db -no-background-agents -allow-url-host -allow-path -attach-listen` (+ more) | Already covered (superset). cogo can stay as a thin CLI shell or adopt core-agent's flag set wholesale. |
| Branding | `cogo/internal/tui/branding.go` (app-name "Cogo") | Same shape, parameterized | Use existing `WithAppName` + `cfg.Agent.DisplayName` overrides. |
| Instruction defaults | Hardcoded in `cogo/internal/agent/agent.go` (~line 75) | `agent.DefaultInstruction` + `agent.DefaultSchedulingInstruction` | Use `WithInstruction(cogoDefaults)` on the flip; cogo's wording stays cogo-specific. |
| Monkey-patching | None found | n/a | Clean. |

## Action items (the only blockers)

These are the *only* things that need to land before the flip is purely mechanical:

1. **Land `feat/core-tui-adapter`** — the in-flight adapter PR that lets core-agent target the external `github.com/go-steer/core-tui` package. This is what makes "core-tui as the default TUI" a flippable switch instead of an architectural lift.
2. **Make core-tui the default TUI in core-agent.** Flip `launcher := launchTUI` → `launcher := launchTUIv2` in `cmd/core-agent/main.go`; gate the legacy path behind an env var or build tag for one release; cut v2.0.
3. **Add `UIConfig` to `core-agent/config`** (theme + mouse). Naturally pairs with action 2 because the core-tui chip switcher reads these; doing them together avoids a config-schema bump in two PRs.

Optional but worth doing in the same window:

- **Inventory cogo's hardcoded instruction wording** — copy verbatim into a `cogoInstruction` constant in cogo so the flip diff stays small.
- **Decide where the 6 Go tools live long-term:** stay in cogo and ship via `WithTools`, or get promoted into `core-agent/tools/go` as an opt-in subpackage. Recommend "stay in cogo" — Go-specific tools belong with the Go-coding-focused consumer, not the substrate.

## Estimated flip diff (cogo side)

- **Delete:** `cogo/internal/{agent,memory,mcp,permissions,skills,session,models}` — duplicates of what core-agent now owns.
- **Keep:** `cogo/internal/tools/go_*.go` (the 6 Go tools), `cogo/cmd/cogo/main.go`, `cogo/internal/tui/branding.go`, any cogo-specific skills/AGENTS.md.
- **Rewrite:** `cogo/cmd/cogo/main.go` to call `core-agent`'s `agent.New(...)` + `tui.Run(...)` with cogo's tools + branding + instruction layered on.

Realistic estimate: ~1 day mechanical work + ~1 day shakedown on a feature branch. Net cogo LoC delta likely -5,000 to -8,000 (most of `internal/` deletes).

## What this audit does NOT cover

- **Whether cogo's existing skills + AGENTS.md still make sense post-flip.** They should — skills format is shared between the projects — but worth a spot-check at flip time.
- **Test-suite migration.** Cogo has its own test fixtures for the packages we'd delete; some will move, some will go away with their subjects. Plan a tests pass alongside the flip.
- **`cogo` users in the wild.** If there are external consumers of `cogo/internal/*` (there shouldn't be — it's `internal`), they break. Worth a `grep` of the org's repos before flipping.

## Pointers

- Strategy + rationale: [`cogo-core-agent-integration.md`](cogo-core-agent-integration.md)
- core-tui adapter design: [`core-tui-adapter-design.md`](core-tui-adapter-design.md)
- Embedded TUI v2 plan (which led to the lift, then the extraction): [`embedded-tui-design-v2.md`](embedded-tui-design-v2.md)
