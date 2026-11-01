# `examples/embed/recipe` — One-Call Recipe Embedder (`agent.RunRecipe`)

This example demonstrates running a complete `core-agent` recipe directory in a
single function call via `agent.RunRecipe` (or `agent.RunOneShot` /
`agent.RunWorkload`) without manually wiring `config.Load`, `models.Resolve`,
`permissions.FromConfig`, `tools.GateTools`, `agent.NewSubagentTool`,
`usage.TurnTap`, and `agent.New`.

For callers that want `.agents/AGENTS.d/*.md` overlays and `@include` expansion
via `pkg/instruction`, `pkg/compose` exposes the identical signature as
`compose.Run`, `compose.RunRecipe`, and `compose.RunWorkload`.

## What `agent.RunRecipe` wires automatically

1. **Recipe discovery (`Dir`)** — loads `.agents/config.json`, Markdown-authored
   subagents under `.agents/subagents/*.md`, and `AGENTS.md`.
2. **Permission gate (`pkg/permissions`)** — builds a `permissions.Gate` from
   the recipe's `permissions` block and wraps caller-supplied `Tools` and
   `Toolsets` through `tools.GateTools` / `tools.GateToolset`.
3. **Declarative subagents** — constructs declared subagents with their
   configured `mode` (`chat`, `task`, or `single_turn`), `output_schema`,
   `tier`, `tools`, and `budgets`.
4. **Usage & structured output** — tracks main-turn and subagent token/cost
   totals in `OneShotResult.Usage` and parses structured JSON object responses
   into `OneShotResult.Output`.

## Running

The example uses an offline scripted `llm.LLM` and requires no credentials or
network access:

```bash
go run ./examples/embed/recipe
```

## Slim dependency guarantee

Like `examples/embed/slim`, `./examples/embed/recipe` is checked by
`dev/tools/verify-slim-deps` to ensure the one-call `pkg/agent` facade stays
within the slim-embed dependency boundary.
