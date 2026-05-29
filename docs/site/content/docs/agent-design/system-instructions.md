---
title: System instructions
weight: 1
---

`AGENTS.md` is the most-impactful customization surface in `core-agent`. The model sees it on every turn. Get it right and the agent behaves consistently across operators and across sessions; get it wrong and the agent acts unpredictably regardless of what skills or tools you wire up.

This page covers the patterns that work, the failure modes to watch for, and how to iterate on an `AGENTS.md` instead of writing it once and hoping.

For the basic mechanics (file location, fallback chain, schema) see [Interactive quickstart → Step 1]({{< relref "cli/interactive/quickstart.md" >}}). This page is the prescriptive how-do-I-write-a-good-one companion.

---

## Mental model: three layers of instruction

A core-agent agent sees three layers of system instruction stacked together, in order:

1. **`agent.DefaultInstruction`** (built-in, always present). Baseline helpfulness directive + parallelism mandate ("execute independent tool calls in parallel") + post-boundary framing ("when prior conversation arrives wrapped in `[Conversation compacted...]` or `[The prior task is complete...]`, read it as authoritative shared history; don't re-run tools").
2. **User-global `~/.core-agent/AGENTS.md`** (optional, prepended). Your personal preferences across all projects — voice, style, "always show me file:line citations," etc.
3. **Project `AGENTS.md`** (optional, prepended last). What this specific project's agent IS — role, what it reviews, house style, do/don't lists.

The model concatenates all three for every turn. `core-agent` doesn't let you skip the default — it's load-bearing for behavior on a half-dozen specific failure modes (Gemini batching tool calls; post-summary recap behavior). Layer your own guidance ON TOP of it.

If you're building a library binary and need to override the default entirely, see [Library API → `agent.WithInstruction`]({{< relref "library/api.md" >}}).

---

## What goes in `AGENTS.md`

**Goes in:** role framing, voice, style preferences, cross-cutting do/don't lists, project-specific constraints, fact references the agent needs every turn.

**Does NOT go in:** multi-step procedures, large reference content, per-task playbooks. Those go in [skills]({{< relref "agent-design/skills.md" >}}).

**Heuristic:** if the content applies to *every turn*, it's `AGENTS.md`. If it applies to *specific tasks*, it's a skill. "Always wrap errors with `fmt.Errorf`" is `AGENTS.md`. "Here's how to run our deploy checklist" is a skill.

---

## Patterns that work

### Lead with role, then the do/don't list

```markdown
You are a Go code-reviewer for the Acme platform monorepo.
[role]

## What you review
- Staged diffs
- Test failures
- Build failures
[scope]

## House style
- Errors wrapped with fmt.Errorf("op: %w", err)
- Table-driven tests with t.Parallel()
[rules]

## What NOT to do
- Don't propose opportunistic refactors
- Don't run go test ./... without asking — slow in this repo
[anti-rules]
```

Models follow imperative direction much better than aspirational descriptions. "You are a thoughtful, helpful assistant" doesn't change behavior; "Always ask before running tests" does.

### Be specific. Concrete > general

Bad:

> Write good error handling.

Good:

> Wrap errors with `fmt.Errorf("op: %w", err)` — not `errors.New`. Bare `errors.New` only for sentinel errors at package scope.

The first version is unfalsifiable; the model has no way to tell whether its output complies. The second has a concrete pattern the model can check against.

### Include examples for patterns you care about

Two lines of code showing your error-wrapping convention is worth a paragraph describing it:

```markdown
## Logging

Use slog with structured fields, not log.Printf string interpolation:

```go
// good
slog.Info("user authenticated", "user_id", uid, "method", "oauth")

// bad
log.Printf("user %d authenticated via %s", uid, "oauth")
```
```

The agent's outputs will mirror the patterns you show.

### Don't fight the default instruction

`agent.DefaultInstruction` already tells the model to:
- Batch independent tool calls in parallel
- Treat post-boundary summaries as authoritative shared history
- Be concise and accurate

You don't need to repeat any of that. If you want to *amplify* one of these (e.g., "when batching tool calls, prefer read_many_files over multiple parallel read_file calls"), do it as an addition — but don't restate the baseline. Re-instruction crowds out your project-specific content.

### Name the failure modes you want the model to avoid

The most effective `AGENTS.md` rules name a specific failure pattern:

> Never use `panic` in library code; never call `os.Exit` outside of `main`. These bypass normal error propagation and make the binary unkillable from the agent's perspective.

The "and here's why" half makes the rule more durable — the model generalizes better when it understands the constraint, vs. memorizing a list of forbidden tokens.

---

## Patterns specific to autonomous use

The unattended agent needs an `AGENTS.md` that's *more* explicit than an interactive one. With no operator course-correcting in the loop, ambiguity becomes either over-eager action or paralysis.

### Crisp success criterion

Tell the agent what "done" looks like:

> Use `report_done` with a one-paragraph summary when ALL of the following are true: (1) every Pod is `Running`, (2) `kubectl rollout status` returned 0, (3) the SLO error budget hasn't decreased in the last 10 minutes.

Without an explicit completion signal, you're relying on budget caps alone to stop the run — the model will happily keep going indefinitely against a vague goal.

### Explicit don't-do list

A monitor that decides to scale a deployment because "that would fix it" is exactly the failure mode unattended runs are notorious for:

> ## What NOT to do
>
> - Don't propose remediations. You're a watcher, not an actor.
> - Don't run `kubectl describe` repeatedly on the same resource — it floods the audit log. Use `get -o json` and parse.
> - Don't post status updates when everything is healthy. Silence = good.

Be aggressive about the don't-do list for autonomous agents. The cost of an unintended action with no operator review is much higher than the cost of being slightly too restrictive.

### Bounded tool surface

If the agent has access to a tool it shouldn't use, name that explicitly:

> The `bash` tool is available but should be used ONLY for the `kubectl get` / `kubectl rollout status` / `kubectl logs` commands listed above. Any other shell invocation is a bug — use a more specific tool or surface a `report_alert` describing what you'd need.

You can also remove the tool entirely with `tools.disable`; the `AGENTS.md` framing is belt-and-suspenders for when removal is overkill.

---

## Model-specific quirks

### Frontier models (Gemini 3 Pro, Claude Opus 4.7)

Generally follow nuanced instruction. Tolerate some ambiguity. The big risk is *over-thoroughness* — they'll verify digests by re-reading the source, repeat tool calls "to be sure," explore tangents. Mitigate with explicit "don't verify; trust the digest" rules where the cost matters.

### Flash/Haiku tier (Gemini 2.5 Flash, Claude Haiku)

Instruction-following is weaker. The model may understand the rule but not connect it to the current decision. Mitigations:

- **Lead with the rule.** "STOP. Before any other action, read X." (Yes, in caps, at the start of the section. Flash needs the attention grab.)
- **Repeat critical rules.** What feels redundant to a human is load-bearing for Flash.
- **Name the failure mode the rule prevents.** "Without this you will accidentally run kubectl apply" — Flash generalizes better when it understands the consequence.
- **Test on the actual model.** Rules that work on Pro may not work on Flash. The post-checkpoint loop issue we caught in v2.0 was exactly this — Pro behaved perfectly, Flash needed stronger framing.

### Smaller / older models (Gemini 2.5, older Claude)

Behavior varies. Test what you're shipping; don't assume cross-model portability. The patterns above are starting points, not guarantees.

---

## Case study: the post-checkpoint loop

A real iteration cycle from v2.0 development, showing how `AGENTS.md` + boundary framing + tool descriptions all interact.

**Symptom (smoke 2026-05-27, Gemini Flash as parent):** after `/done` wrote a checkpoint, the next turn ran `list_dir` infinitely instead of reading from the checkpoint summary.

**Root cause analysis:**
1. The `compactingService` was correctly slicing pre-checkpoint history out of the LLM request — verified by smaller input-token counts on the next turn.
2. The checkpoint summary was being passed through as a `RoleUser` message wrapped with framing — verified by inspecting the request.
3. The framing said "[Conversation compacted. Below is the handover summary...]" — but Flash interpreted the summary's leading `# Task complete` heading as "everything's done, fresh start" and ignored the summary content.

**Fix (PR #54):**
- New `checkpointPrefix` distinct from compaction's: "[The prior task is complete... the conversation CONTINUES from here; the next user message is part of the SAME ongoing session, not a fresh start. When the user asks anything about the prior task — what was done, what files were touched, what was learned, recap, summary, status — read FROM the record below..."
- Renamed `# Task complete` → `# Task` in the summarizer prompt. Removed terminal language.
- Added a sentence to `agent.DefaultInstruction` about both framing shapes (`[Conversation compacted…]` and `[The prior task is complete…]`).

**Lesson for your `AGENTS.md`:**

If you're using Flash as parent and observe similar "model ignores context" patterns, the answer is rarely "the model is broken." It's almost always that:

1. Your `AGENTS.md` rule is too soft (aspirational, not imperative)
2. Your rule competes with another signal in the context (a section heading, a prior tool result, the default instruction)
3. The rule needs to lead with attention-grabbing language for Flash to register it

The fix is usually 1-2 sentences, not a rewrite. But the iteration loop — observe the failure, identify which signal won, strengthen the losing signal — applies whether you're tuning `core-agent`'s own framings or your own project's `AGENTS.md`.

---

## How to iterate on an `AGENTS.md`

**Start minimal.** A 20-line `AGENTS.md` with role + scope + 3-4 rules covers 80% of agents. Don't try to anticipate every failure mode up front; you'll write rules for things that never happen and miss the things that do.

**Add rules as failures surface.** Use `core-agent` for a few real tasks. When the agent does something wrong, ask yourself:

- Is it a one-off, or will it happen again? If one-off, ignore.
- If recurring: is the existing instruction too vague, or is it missing entirely?
- Write the new rule as a do/don't statement naming the specific behavior you want.

**Don't over-prompt.** A 500-line `AGENTS.md` is harder for the model to follow than a 100-line one. Long instructions get truncated in the model's attention; rules late in the document carry less weight. If your `AGENTS.md` is growing past ~150 lines, consider:

- Moving stable, task-specific procedures to skills
- Moving cross-cutting reference content (lists of services, library inventories) to a skill's `references/` directory the agent can fetch on demand
- Splitting per-team rules into team-specific skills with descriptions like "When asked about billing code"

**Version it.** `AGENTS.md` is in your repo, so it's already version-controlled. When you change a rule, leave a brief comment explaining the change. Six months from now you'll thank yourself.

---

## Anti-patterns

| Pattern | Why it fails | Fix |
|---|---|---|
| "You are a thoughtful, helpful assistant" | Aspirational; doesn't change behavior | Specific role: "You are a Go code-reviewer for X" |
| "Be careful with error handling" | Unfalsifiable | Concrete rule: "Wrap errors with `fmt.Errorf(\"op: %w\", err)`" |
| Repeating the default instruction | Crowds out project-specific content | Only mention defaults to amplify them, not restate |
| 30 do/don't rules, no role framing | Model has nothing to ground them in | Lead with what the agent IS, then layer rules on top |
| "Always do X" rules with no exception list | Edge cases force the model to silently violate | "Always X EXCEPT when Y" — name the carve-outs |
| Markdown subsection per individual rule | Visual noise, hard to scan | Group related rules under a single heading |
| Examples written in prose, not code | Models mirror what they see; show, don't tell | Use ``` fenced code blocks for examples |
| Soft language ("you might consider", "it would be nice if") | Models discount soft instructions | Imperative voice: "Do X", "Never Y" |

---

## Where to go next

- **[Skills]({{< relref "agent-design/skills.md" >}})** — when to write a skill vs. an `AGENTS.md` rule
- **[Subagents and wrappers]({{< relref "agent-design/subagents-and-wrappers.md" >}})** — getting the model to use `agentic_*` tools + spawn background subagents
- **[Cost efficiency]({{< relref "agent-design/cost-efficiency.md" >}})** — model selection, the Pro+Flash split, `/context` observability
- **[Interactive workflows]({{< relref "cli/interactive/workflows.md" >}})** — worked examples with full `AGENTS.md` files
- **[Reference → Skills]({{< relref "reference/skills.md" >}})** — `SKILL.md` schema details
