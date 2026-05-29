# `AGENTS.md` patterns

Reference for the `cli-setup` skill. Fetch when the user is writing or revising their `AGENTS.md` and you need to suggest patterns or diagnose why their existing one isn't working.

## The three-layer instruction stack

The model sees three things concatenated on every turn:

1. `agent.DefaultInstruction` (built-in, always present) — baseline helpfulness + parallelism mandate + post-boundary framing
2. User-global `~/.core-agent/AGENTS.md` (optional, prepended)
3. Project `AGENTS.md` (optional, prepended last)

The user owns layers 2 and 3. Layer 1 is built-in and handles a half-dozen specific behaviors (Gemini tool batching, post-summary recap behavior) — DON'T tell the user to restate it in their `AGENTS.md`. That's wasted context.

## Minimum viable `AGENTS.md`

About 15 lines. Role + scope + house style + don't-do. Anything more on the first pass is premature optimization.

```markdown
You are a Go code-reviewer for the Acme platform monorepo.

## What you review
- Staged diffs (`git diff --staged`) the user pastes or asks you to fetch.
- Test failures: surface what broke, propose the minimum fix.

## House style
- Wrap errors with `fmt.Errorf("op: %w", err)`, not `errors.New`.
- Table-driven tests with `t.Parallel()`.
- Use `slog` with structured fields, not `log.Printf`.

## What NOT to do
- Don't propose opportunistic refactors. Suggest the smallest diff.
- Never use `panic` in library code.
- Before running tests, ALWAYS ask — `go test ./...` is slow in this repo.
```

When proposing one to the user, start at this size. Don't propose 100 lines of rules covering every edge case; iterate as failure modes surface.

## Patterns that work

**Lead with role, then the do/don't list.** Model behavior is grounded by the role framing; without it, rules feel arbitrary.

**Be specific. Concrete > general.** Bad: "write good error handling." Good: "wrap errors with `fmt.Errorf(\"op: %w\", err)`."

**Include examples for patterns you care about.** Two lines of code showing the pattern beats a paragraph describing it. Models mirror what they see.

**Imperative voice.** "Do X" / "Never Y" — not "you might consider" or "it would be nice if." Models discount soft instructions.

**Name the failure mode the rule prevents.** "Never use `panic` in library code; it bypasses normal error propagation and makes the binary unkillable from the agent's perspective." The model generalizes better when it understands the constraint.

## Patterns that DON'T work

| Pattern | Why it fails | Fix |
|---|---|---|
| "You are a thoughtful, helpful assistant" | Aspirational, doesn't change behavior | Specific role: "You are a Go code-reviewer for X" |
| "Be careful with error handling" | Unfalsifiable | Concrete rule with the exact convention |
| Repeating `agent.DefaultInstruction` ("execute tool calls in parallel") | Already in layer 1; wastes context | Only mention defaults to amplify, never to restate |
| 30 do/don't rules with no role framing | Model has nothing to ground them in | Lead with what the agent IS |
| "Always X EXCEPT in some cases" without naming the cases | Forces silent rule violation | Name the carve-outs explicitly |
| Soft language ("might consider", "would be nice if") | Models discount soft instructions | Imperative voice: "Do X", "Never Y" |
| 500-line `AGENTS.md` | Rules late in document carry less weight | If > ~150 lines, split into skills |

## Model-specific quirks

**Frontier models (Gemini 3 Pro, Claude Opus 4.7).** Generally follow nuanced instruction. The big risk is *over-thoroughness* — they'll re-read sources to verify digests, repeat tool calls "to be sure." If you observe this on the user's project, add an explicit "trust the digest; don't verify with bare read_file" rule.

**Flash/Haiku tier (Gemini 2.5 Flash, Claude Haiku).** Instruction-following is weaker. The model may understand the rule but not connect it to the current decision. Mitigations:
- Lead with the rule (yes, in attention-grabbing language, at the start of the section).
- Repeat critical rules — what feels redundant to a human is load-bearing for Flash.
- Name the failure mode the rule prevents.

**Test on the actual model the user is running.** Rules that work on Pro may not work on Flash. The post-checkpoint loop issue from `core-agent` v2.0 development was exactly this — Pro behaved perfectly, Flash needed stronger framing.

## Iteration approach

When the user reports "the agent did something wrong," walk this:

1. **One-off or recurring?** If one-off, ignore.
2. **If recurring:** is the existing instruction too vague, or missing entirely?
3. **Write the new rule** as a do/don't statement naming the specific behavior you want.
4. **Test** with a representative prompt; verify the rule fires.

Don't try to anticipate every failure mode up front. The user will hit specific ones; address those, not hypothetical ones.

## Fallback chain (reference)

`core-agent` reads in this order (first match per directory):

- **User-global:** `~/.core-agent/AGENTS.md` → `CLAUDE.md` → `GEMINI.md`
- **Project:** `<repo>/AGENTS.md` → `CLAUDE.md` → `GEMINI.md`

Both layers prepend; user-global comes first. The `AGENTS.md` / `CLAUDE.md` / `GEMINI.md` triple matches what Claude Code and Gemini CLI have settled on, so one file works across tools.

## When to advise splitting into a skill

If the `AGENTS.md` has grown past ~150 lines OR contains content that only applies to specific task types (a deploy procedure, a code-review rubric, a triage workflow), suggest moving it to a skill. Heuristic:

| Content | Goes in |
|---|---|
| Applies to every turn | `AGENTS.md` |
| Applies to specific named requests (procedures, runbooks) | A skill |
| Large reference content (long rubrics, library inventories) | A skill's `references/` directory |

See `references/skills-and-mcp.md` for skill design patterns.

## Tips for talking to the user

- Show them what's about to land in their repo before writing it. `write_file` is irreversible; misalignment costs them a `git checkout`.
- Propose 3-5 rules, not 15. They'll add more as friction surfaces.
- If they're not sure what the agent should do, ask "what would you tell a new teammate the agent should do" — that's almost always the right `AGENTS.md` shape.
- Don't write the rules ABOUT the user's project ("Acme has these libraries"). Write rules ABOUT the agent's behavior ("Always wrap errors with…"). The agent learns the project from the codebase; what it needs from `AGENTS.md` is policy.
