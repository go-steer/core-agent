---
title: Skills
weight: 2
---

Skills are named, reusable procedures the agent invokes when the task matches the skill's description. This page covers the design patterns: when to write a skill vs. an `AGENTS.md` rule, how to write a description that actually triggers, what belongs in the body vs. a `references/` file, and how to test that a skill fires when you expect it to.

For the schema and discovery details (file locations, YAML frontmatter, allow/deny lists), see [Reference → Skills]({{< relref "reference/skills.md" >}}). This page is the prescriptive companion.

---

## When to write a skill vs. an `AGENTS.md` rule

**Use `AGENTS.md` for things the agent should know on every turn.**

- Role + scope ("you're a code-reviewer for Acme")
- House style ("wrap errors with `fmt.Errorf`")
- Cross-cutting do/don'ts ("never call `os.Exit` outside of `main`")
- Facts the agent needs constant access to (a short list of services, build commands)

**Use a skill for procedures the agent invokes on specific requests.**

- "Run our deploy checklist"
- "Triage a Jira ticket"
- "Do a code review"
- "Investigate a CI failure"

**Heuristic:** if the user's prompt clearly maps to one of N named procedures you've defined, that's a skill. If the content applies regardless of what the user asked, it's `AGENTS.md`.

**Edge case — large reference content:** if you have a 500-line house style guide, the natural impulse is "stuff it in `AGENTS.md`." Resist. Long `AGENTS.md` files dilute the model's attention; rules late in the document carry less weight. Put the reference content in a skill's `references/` directory and have the skill body tell the agent when to fetch it.

---

## The description IS the trigger

A skill's YAML `description` is what the model uses to decide whether the skill applies. The body never runs until the description matches. This makes the description the single most important field in your `SKILL.md`.

**Bad:**

```yaml
description: Code review helper
```

The model has no signal about *when* to invoke this. It might fire on "review the diff," but it might also miss "I want feedback on these changes" or "look at what I changed" — phrasings that mean the same thing.

**Good:**

```yaml
description: Review a Go diff against Acme house style. Use when the user asks for a review, asks for feedback on changes, pastes a diff, or asks "what do you think of this code".
```

Lists the trigger phrases the model should match. Notice it doesn't say *how* the review happens — that's the body's job. Description = "when does this skill apply." Body = "what does the skill do."

**Pattern:** write the description as a sentence describing the skill, then add "Use when the user…" with 2-4 trigger phrasings. Models match against the description as a fuzzy semantic search; covering the natural language variations directly is the most reliable approach.

---

## What goes in the body

The body is the prompt content the agent sees when the skill is invoked. Write it like a runbook you'd hand a new team member.

**Structure that works:**

```markdown
When invoked:

1. [first action]
2. [second action]
3. [output format]

## [Section with reference material the skill needs]

[Bullet list of rules, tables, examples]

## When NOT to use this skill

- [Carve-out 1]
- [Carve-out 2]
```

**Patterns:**

- **Number the procedure.** Models execute step-by-step procedures more reliably than prose descriptions.
- **Specify the output format.** "Output a structured review with these three sections..." reduces variance.
- **Include the rubric inline if it's short.** A 5-bullet house-style list goes in the body. A 200-line guide goes in `references/`.
- **The "When NOT to use" section is load-bearing.** It tells the model what's out of scope and prevents the skill from firing on adjacent but unrelated requests.

---

## Composability: skills + `references/`

Each skill lives in its own directory under `.agents/skills/`. You can drop additional files alongside `SKILL.md` and the agent can read them on demand:

```
.agents/skills/
└── code-review/
    ├── SKILL.md
    └── references/
        ├── house-style-full.md      # the long version
        ├── error-handling-deep.md   # specific examples
        └── good-vs-bad-tests.md     # paired examples
```

The skill body tells the agent when to fetch:

```markdown
For most diffs, the rubric below is enough. If the diff makes
non-trivial changes to error-handling or concurrency, also read
`references/error-handling-deep.md` for the full pattern catalogue.
```

This keeps the always-loaded part of the skill short while preserving access to depth when the agent needs it. The model fetches the file with `read_file`; nothing magic about `references/` — it's a convention.

**Heuristic:** if a section of your skill body is >50 lines and only relevant to ~30% of invocations, move it to `references/`.

---

## Skills + MCP

A skill can require MCP tools the agent has access to. Document the dependency in the body:

```markdown
## Required tools

This skill needs the `search` MCP server's `search_tavily_search` tool to
verify current behavior of third-party libraries. If `search` isn't
configured, fall back to noting "library behavior not verified, please
double-check the docs" in your review.
```

The agent will use the tool when the skill body mentions it. If the tool isn't registered, the agent will surface the absence rather than silently skipping the step.

**Pattern:** name the specific tool the skill expects (`search_tavily_search`, not "a search tool"). The namespacing (`<server>_<tool>`) is deterministic — the skill can match it exactly.

---

## Testing that a skill fires

The frustrating failure mode: the user types "review this code," the skill `code-review` is registered, and nothing happens — the model handles the request directly without invoking the skill. You spend 30 minutes wondering if it's broken; it isn't, the description just doesn't match well enough.

**Cheap test:**

```text
> /skills
[lists loaded skills with their descriptions]

> review the diff in pending.diff
[watch for "calling code-review" or equivalent in the tool stream]

> /btw why didn't you invoke the code-review skill for the prior turn?
[the model often will tell you — "the request didn't seem to match the
 description's phrasing" — and that's your signal to tighten]
```

The `/btw` query is particularly useful because it doesn't pollute conversation history. Ask "why did/didn't you use skill X?" and use the answer to refine the description.

**Common reasons a skill doesn't fire:**

| Symptom | Likely cause | Fix |
|---|---|---|
| Skill doesn't fire on similar requests | Description too specific | Add trigger phrasings: "Use when the user asks for X, asks for Y, mentions Z" |
| Skill fires on unrelated requests | Description too broad | Add "Use when…" with specific contexts; add "When NOT to use" section |
| Skill fires but ignores body steps | Body too long or too unstructured | Number the procedure; remove background prose |
| Skill fires intermittently | Description ambiguous; model picks differently each time | Rewrite description as one sentence + explicit trigger phrases |

---

## Worked example: a deploy-checklist skill

Showing the patterns together. This skill walks a release deploy:

```markdown
---
name: deploy-checklist
description: Run the Acme release deploy checklist. Use when the user says "deploy", "ship", "release", or "cut a release", or when the user mentions a release tag or version number in the context of pushing to production.
---

When invoked:

1. Confirm the release target with the user (which version? which environment?).
2. Run the pre-deploy checks IN ORDER. Stop on the first failure; do NOT
   continue past a failed check.
3. Apply the rollout.
4. Run the post-deploy verification.
5. Report the outcome.

## Pre-deploy checks

1. CI green on `main`:
   `gh pr list --state merged --limit 1 --json statusCheckRollup`
   All check runs should be `success`.
2. No active incidents:
   `curl -s https://status.acme.internal/api/active | jq '.count'`
   Must be 0.
3. Database migrations applied:
   `kubectl exec -n prod deploy/db-migrator -- /usr/local/bin/migrate status`
   Must show no pending migrations.

## Apply the rollout

`kubectl apply -k overlays/prod/v<VERSION>` then `kubectl rollout status deployment/app -n prod --timeout=10m`.

## Post-deploy verification

1. Health check passes: `curl -fsS https://app.prod.acme.internal/healthz`
2. SLO error budget stable: `gh workflow run slo-check.yml -f deployment_id=<NEW_TAG>`
3. Sample request succeeds: `curl -fsS https://app.prod.acme.internal/api/version | jq '.version'` — should match the deployed tag.

## Report

Output a single message with:
- ✅ / ❌ for each pre-deploy check
- "Rollout: complete in Xs" / "Rollout: failed at Y"
- ✅ / ❌ for each post-deploy check
- Final status: "DEPLOY OK" / "DEPLOY FAILED — recommend rollback"

## When NOT to use this skill

- For hotfixes that bypass the normal release flow — use `hotfix-deploy` instead.
- For staging deploys — they don't need the full checklist; just `kubectl apply` directly.
- For rollbacks — use `rollback-checklist`.
```

**What this demonstrates:**

- **Description names triggers** ("deploy", "ship", "release", "cut a release"). The fuzzy match catches the natural phrasings without listing every possible verb.
- **Body is a numbered procedure** with explicit "stop on first failure" semantics. Removes ambiguity about what the model should do if step N fails.
- **Each check has the exact command.** No prose like "verify CI is passing" — the model knows what to run.
- **Output format is specified.** The final report has a known structure, reducing turn-to-turn variance.
- **"When NOT to use" lists adjacent skills.** Tells the model where to redirect if the user's intent doesn't quite match.

---

## Anti-patterns

| Pattern | Why it fails | Fix |
|---|---|---|
| Vague description ("code helper") | Model doesn't know when to invoke | Specific: "Review a Go diff against Acme house style. Use when…" |
| Body as prose explanation | Model doesn't extract the procedure | Numbered steps |
| Skill that duplicates `AGENTS.md` rules | Wasted context budget | Keep skills for procedures; cross-cutting rules go in `AGENTS.md` |
| Single 500-line `SKILL.md` | Model attention dilutes; rules late get ignored | Split rubric into `references/` files the body fetches on demand |
| No "When NOT to use" section | Skill fires on adjacent requests | Add explicit carve-outs |
| Description starts with "This skill..." | Wastes the precious first words on meta-content | Lead with what the skill DOES |
| Skill body imports another skill's procedure | Coupling makes both harder to maintain | Inline the shared content or extract a `references/` file both skills read |

---

## Where to go next

- **[System instructions]({{< relref "agent-design/system-instructions.md" >}})** — patterns for `AGENTS.md` (the other half of the customization)
- **[Subagents and wrappers]({{< relref "agent-design/subagents-and-wrappers.md" >}})** — when to push a task into a subtask vs. handling it in the parent
- **[Cost efficiency]({{< relref "agent-design/cost-efficiency.md" >}})** — model selection considerations for skill-heavy workflows
- **[Reference → Skills]({{< relref "reference/skills.md" >}})** — schema, discovery, allow/deny lists, permission gating
- **[Interactive workflows]({{< relref "cli/interactive/workflows.md" >}})** — full worked examples with skills wired in
