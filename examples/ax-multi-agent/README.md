# Multi-agent example: devil + angel

Two `core-agent` instances run as remote AX agents and argue opposite sides of
any decision the user puts in front of the planner. The AX Gemini planner
routes each prompt to one or both, then synthesizes a balanced answer from
their conflicting outputs.

```
                    ┌────────────────────────┐
   user prompt ──▶  │     ax serve            │
                    │     (Gemini planner)    │
                    └─────────┬───────────────┘
                              │ "tool calls"
                  ┌───────────┴────────────┐
                  ▼                        ▼
          ┌───────────────┐        ┌───────────────┐
          │   devil       │        │   angel       │
          │   :50051      │        │   :50052      │
          │   (against)   │        │   (for)       │
          └───────────────┘        └───────────────┘
```

This is a worked example for the [`axplore` branch](../../docs/ax-plan.md) that
packages core-agent as an AX (`github.com/google/ax`) remote agent. Both
agents are vanilla `core-agent` builds — the only thing that makes them
"opposing roles" is each one's `AGENTS.md` instruction file.

## What you'll need

- `core-agent` binary built from the `axplore` branch (`go build -o core-agent ./extras/ax-agent`)
- `ax` binary built from [`github.com/google/ax`](https://github.com/google/ax) (the adapter imports the upstream proto directly now that the repo is public)
- A Gemini API key in `GEMINI_API_KEY` (or `GOOGLE_API_KEY`); the planner *and* both core-agent instances use it. To run credential-free, change both `config.json` files to `"provider": "echo"` — you'll see structural responses, not real arguments.

## Run it

Open four terminals.

**Terminal 1 — devil:**
```bash
cd examples/ax-multi-agent/devil
GEMINI_API_KEY=... ../../../core-agent ax-agent --listen=:50051
```

**Terminal 2 — angel:**
```bash
cd examples/ax-multi-agent/angel
GEMINI_API_KEY=... ../../../core-agent ax-agent --listen=:50052
```

**Terminal 3 — AX controller:**
```bash
cd examples/ax-multi-agent
GEMINI_API_KEY=... ax serve --config ax.yaml
```

**Terminal 4 — drive a conversation:**
```bash
ax exec --server localhost:8494 \
  --input "Should we rewrite our Go HTTP routing layer in Rust?"
```

The planner will see two registered agents (`devil`, `angel`), pick which to
call (typically both for a decision question), feed their outputs back, and
synthesize a final response. Continue the conversation:

```bash
ax exec --server localhost:8494 \
  --conversation <id-from-prior-call> \
  --input "What if we did it incrementally, one handler at a time?"
```

The conversation history is persisted in `eventlog.sqlite`, so both agents see
the prior turn's exchange when consulted on the follow-up. That's the
"agent-to-agent communication" mechanic — there's no direct wire between
devil and angel; the planner is the broker, and the event log is shared state.

## Why this shape

- **Each role is defined entirely by its `AGENTS.md`.** The two binaries are
  bit-identical; only the project context differs. Adding a third role
  (skeptic? optimist? parrot?) is one new directory + one new `AGENTS.md` +
  one new entry in `ax.yaml`.
- **Tools are deliberately limited.** Both agents have `bash`, `write_file`,
  and `edit_file` disabled (see each `config.json`). They're argumentative,
  not productive — read-only access to the workspace is enough to ground their
  arguments in the actual code.
- **Permissions mode is `yolo`** in both configs because the read-only tool
  set has no real action surface and AX provides its own UX layer for
  approvals. In a production deployment with `bash` enabled you'd flip these
  back to `ask`.

## Trying other prompts

The pattern works for any decision-shaped question. A few that have
demonstrated good multi-perspective synthesis in testing:

```bash
ax exec --input "Should we adopt OpenTelemetry across all services this quarter?"
ax exec --input "Should we delete /etc/legacy-config now or wait for the next release?"
ax exec --input "We're considering switching from Postgres to SQLite for the analytics service — thoughts?"
```

The planner's synthesis is what makes this useful; raw devil/angel pairs from
either alone would be one-sided.
