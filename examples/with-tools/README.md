# `with-tools` — custom tool + MCP servers + skills

## What this shows

An agent composed from the three tool sources a real embedding uses:

1. a custom function tool (`add`, defined inline with ADK's
   `functiontool.New`),
2. MCP servers loaded from `.agents/mcp.json` in the working directory
   (if present), and
3. `SKILL.md` skills from `.agents/skills/` (if any).

The agent is asked "What is 17 + 25?" and calls the `add` tool; the
function-call name prints to stderr, the streamed answer to stdout.

## Run it

Needs real Anthropic credentials (the example pins
`provider=anthropic`):

```bash
ANTHROPIC_API_KEY=... go run ./examples/with-tools
```

MCP + skills loading is best-effort — with no `.agents/` directory in
your cwd it just logs and continues.

## Key APIs

- `functiontool.New` — `google.golang.org/adk/tool/functiontool`
- `agent.WithTools` / `agent.WithToolsets` — `github.com/go-steer/core-agent/v2/pkg/agent`
- `mcp.Build` — `github.com/go-steer/core-agent/v2/pkg/mcp`
- `skills.Load` — `github.com/go-steer/core-agent/v2/pkg/skills`
- `permissions.FromConfig` — `github.com/go-steer/core-agent/v2/pkg/permissions`

## What you should see

```
→ add
The sum of 17 and 25 is 42.
```

(the `→ add` line is the model's function call, printed to stderr).

## Next

- [`with-subagent`](../with-subagent/) — delegate to a second agent instead of a function tool.
- [`streaming`](../streaming/) — the built-in tool registry with chat rendering.
- Library guide: [`docs/site/src/content/docs/embed/guide.md`](../../docs/site/src/content/docs/embed/guide.md)
