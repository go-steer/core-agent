# Skills and MCP

Reference for the `cli-setup` skill. Fetch when the user wants to extend the agent's tool surface beyond the nine built-ins.

## When to add a skill

`core-agent` ships nine built-in tools (`read_file`, `read_many_files`, `write_file`, `edit_file`, `list_dir`, `glob`, `grep`, `bash`, `todo`). Skills are *procedures* the agent invokes by name — they aren't additional tools, they're encapsulated workflows.

Heuristic:

| Content | Goes in |
|---|---|
| Applies to every turn (style, role, cross-cutting rules) | `AGENTS.md` |
| Applies to specific named requests (a runbook, a procedure) | A skill |
| Large reference content the agent fetches on demand | A skill's `references/` directory |

If the user is asking "the agent should follow procedure X when I say Y," that's a skill.

## Skill schema

Each skill lives in its own directory under `.agents/skills/`:

```
.agents/skills/
└── <skill-name>/
    ├── SKILL.md             # required: frontmatter + body
    └── references/          # optional: deep content the body fetches
        ├── topic-a.md
        └── topic-b.md
```

`SKILL.md` has YAML frontmatter + markdown body:

```markdown
---
name: <kebab-case-name>
description: <one sentence describing what + when. The "when" is critical — it's how the agent decides to invoke this skill.>
---

When invoked:

1. <numbered step>
2. <numbered step>
3. <output format>

## <Section heading>

<rules, examples, tables>

## When NOT to use this skill

- <carve-out 1>
- <carve-out 2>
```

## The description IS the trigger

The model uses the `description` field to decide whether to invoke the skill. The body never runs until the description matches. Get this wrong and the skill is invisible.

**Bad:**

```yaml
description: Code review helper
```

No signal about *when* this applies.

**Good:**

```yaml
description: Review a Go diff against Acme house style. Use when the user asks for a review, asks for feedback on changes, pastes a diff, or asks "what do you think of this code".
```

Lists trigger phrasings. The model matches semantically; listing the natural-language variations directly is the most reliable approach.

**Pattern:** write the description as one sentence describing the skill, then add "Use when the user…" with 2-4 trigger phrasings.

## Body structure that works

```markdown
When invoked:

1. [first concrete action]
2. [second concrete action]
3. [output format]

## [Reference material the skill needs inline]

[Bullet list of rules, table, examples]

## When NOT to use this skill

- [Carve-out 1]
- [Carve-out 2]
```

- **Number the procedure.** Models execute step-by-step procedures more reliably than prose.
- **Specify the output format.** "Output a structured review with these three sections..." reduces variance.
- **Include the rubric inline if short.** A 5-bullet style guide goes in the body. A 200-line guide goes in `references/`.
- **The "When NOT to use" section is load-bearing.** It prevents the skill from firing on adjacent requests.

## Composability with `references/`

Drop additional files alongside `SKILL.md`:

```
.agents/skills/code-review/
├── SKILL.md
└── references/
    ├── house-style-full.md
    ├── error-handling-deep.md
    └── good-vs-bad-tests.md
```

The skill body tells the agent when to fetch:

```markdown
For most diffs, the rubric below is enough. If the diff makes non-trivial
changes to error-handling, also read `references/error-handling-deep.md`
for the full pattern catalogue.
```

This keeps the always-loaded part short while preserving access to depth on demand. Heuristic: if a section is >50 lines and relevant to ~30% of invocations, move it to `references/`.

## Testing a skill fires

Common failure mode: the user types "review this code," the skill is registered, but the model handles the request directly without invoking the skill.

Quick diagnostic:

```text
> /skills
[lists loaded skills with descriptions]

> [the prompt that should trigger it]
[watch for the skill name in the tool-call stream]

> /btw why didn't you invoke the code-review skill for that prior turn?
[the model often tells you — "the request didn't match the phrasing" — and you have a fix]
```

The `/btw` introspection is particularly useful because it doesn't pollute history.

## When to wire MCP

MCP (Model Context Protocol) is for capabilities outside the built-in tool set. Common additions:

| Capability | MCP server |
|---|---|
| Web search | `tavily-mcp`, `brave-search-mcp` |
| GitHub API | `github-mcp-server` (first-party from GitHub) |
| Filesystem (extended beyond the built-in) | `mcp-server-filesystem` |
| Internal databases | Custom — your team writes one |

Don't wire MCP for capabilities the built-in tools cover. The built-in `read_file` is better than a generic `mcp-server-filesystem` for almost all uses.

## `mcp.json` schema

```json
{
  "version": 1,
  "servers": {
    "tavily": {
      "transport": "stdio",
      "command":   "uvx",
      "args":      ["mcp-server-tavily"],
      "env":       { "TAVILY_API_KEY": "${env:TAVILY_API_KEY}" }
    },
    "github": {
      "transport": "http",
      "url":       "https://api.githubcopilot.com/mcp/",
      "headers":   { "Authorization": "Bearer ${env:GITHUB_TOKEN}" }
    }
  }
}
```

- **`transport: stdio`** for locally-spawned servers (most). Pair with `command` + `args` + optional `env`.
- **`transport: http`** for HTTP-served (rare; mostly GitHub Copilot's MCP endpoint today). Pair with `url` + `headers`.
- **`${env:NAME}` interpolation** resolves at startup. Missing env vars fail loudly — that's intended; better than silently booting with no auth.
- **Server tools are namespaced** as `<server>_<tool>`. The `tavily-mcp`'s `search` tool becomes `tavily_search` in the agent's tool list. Prevents collisions when two servers both expose `search`.

## Skills + MCP composition

A skill can require MCP tools. Document the dependency in the body:

```markdown
## Required tools

This skill needs the `tavily` MCP server's `tavily_search` tool to verify
current behavior of third-party libraries. If `tavily` isn't configured,
fall back to noting "library behavior not verified" in your output.
```

The agent uses the tool when the skill body mentions it. If the tool isn't registered, the agent surfaces the absence rather than silently skipping.

## Anti-patterns

| Pattern | Why it fails | Fix |
|---|---|---|
| Vague description ("code helper") | Model doesn't know when to invoke | Specific: "Review a Go diff against Acme house style. Use when…" |
| Body as prose, not numbered procedure | Model doesn't extract the steps | Numbered steps |
| Single 500-line `SKILL.md` | Attention dilutes | Split into `references/` |
| No "When NOT to use" section | Skill fires on adjacent requests | Add explicit carve-outs |
| Description starts with "This skill..." | Wastes first words on meta-content | Lead with what it DOES |
| Skill that duplicates `AGENTS.md` rules | Wasted context budget | Keep skills for procedures; cross-cutting rules go in `AGENTS.md` |
| Adding MCP for capabilities built-ins cover | Slower, more brittle than the built-in | Use built-ins unless you need something outside the nine |
