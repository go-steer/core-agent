# Cogo + core-agent: integration strategy (handover note)

Strategy options for how cogo and core-agent should fit together, given
that core-agent is now a functional superset of cogo's substrate (it
has autonomous, subagents, eventlog, multi-provider, grounding
projection — cogo has none of these) and cogo has the polished
Bubble Tea TUI that core-agent deliberately deferred.

Author's recommendation: **C, sequenced through A**.

## Option A — cogo wraps core-agent

Cogo keeps its TUI; rips out its own permissions / MCP / skills /
providers / session handling and replaces them with core-agent.

**Pros**
- Lowest-risk migration. core-agent's public surface is already
  designed for embedding.
- Cogo gets autonomous, subagents, eventlog, multi-provider,
  grounding projection, durable sessions for free.
- Proves core-agent can really back a non-headless consumer (the
  substrate has never been stressed by a TUI yet — prompter
  contract, error-surface formatting, MCP elicitation, mid-turn
  cancellation are all likely to have seams that look fine in
  theory but creak when a real UI tries to drive them).

**Cons**
- Migration is real work. Cogo's permission gate, MCP plumbing,
  skills loader, providers, session handling all get replaced.
- Cogo becomes a "thin wrapper" — reasonable people ask why it
  exists as a separate project.

## Option B — pull the TUI into core-agent

core-agent gains a TUI mode. Cogo gets retired or becomes a branded
deployment.

**Pros**
- One product, less fragmentation.
- Users get the best of both projects without choosing.

**Cons (the dealbreaker)**
- Bubble Tea + Glamour + lipgloss + their transitive deps is a
  heavy set. Forcing every library consumer (Scion, AX, future
  embedders) to compile that in to get the `agent` package
  contradicts the v0.1.0 extraction decision.
- Build tags or `cmd/tui/` subcommand can hide it, but you maintain
  that boundary forever and the binary still has it.
- core-agent's value proposition has been "small, clean,
  embeddable." Adding a TUI dilutes that positioning.

## Option C — TUI as its own project

After A proves core-agent is a viable substrate, lift cogo's
`internal/tui/` (or wherever the Bubble Tea code lives) out into a
separate repo — something like `core-agent-tui`. Depends on
`core-agent`. Can be embedded by cogo's existing binary, used
standalone, or embedded by any other consumer who wants a TUI.

Cogo then becomes either:
- (a) a branded distribution of `core-agent-tui` + `core-agent` for
      users who already type `cogo`, or
- (b) gracefully retired.

**Pros**
- Clean separation of concerns: substrate / UI / branded shell.
- Library users don't pay the TUI cost.
- TUI maintainers can move independently of substrate changes.
- The same TUI can be reused outside cogo.

**Cons**
- More repos = more release coordination. But you're already
  managing two (cogo + core-agent); cost is "one more," not
  "going from one to three."
- `core-agent-tui` would move slower than core-agent once it
  stabilizes — fine, but worth knowing.

## Recommended sequencing

1. **A first.** Pick one slice of cogo (e.g. the permission gate) and
   replace it with core-agent behind a flag. See what breaks. This
   tells you whether full A is a 2-week migration or a 2-month one
   before you commit.
2. **Complete A.** Cogo runs entirely on core-agent internals, TUI
   still in cogo. This is the validation milestone — core-agent has
   one external consumer beyond its own bundled CLI / Scion / AX.
3. **C when stable.** Once cogo-on-core-agent is stable and the TUI
   has settled, lift the TUI into `core-agent-tui`. Cogo becomes
   either a branded distribution or gets retired.

## Why B is not recommended

Adding a TUI to core-agent itself is the architectural choice that's
hardest to reverse. Once `agent.New(...)` indirectly depends on a
rendering library, every embedder pays for it, and every future
non-TUI consumer becomes harder to support. C avoids this entirely
while still letting the TUI exist as a first-class thing.
