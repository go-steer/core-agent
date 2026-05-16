---
title: Getting started
weight: 2
---


## Install

Requires Go 1.26 or newer.

### As a CLI

```bash
go install github.com/go-steer/core-agent/cmd/core-agent@latest
```

The binary lands in `$(go env GOBIN)` (or `$GOPATH/bin` if `GOBIN` is unset). Make sure that's on your `$PATH`.

### As a library

```bash
go get github.com/go-steer/core-agent
```

Then `import "github.com/go-steer/core-agent/agent"` (and the relevant submodules).

---

## First run — pick a provider

You need credentials for at least one model backend. Skip the sections you don't have keys for.

### Gemini API (fastest to set up)

Get a key at [aistudio.google.com](https://aistudio.google.com). Then:

```bash
export GEMINI_API_KEY=...   # or GOOGLE_API_KEY — either works
core-agent -p "what's the capital of France?"
```

Auto-detection picks the Gemini provider when `GEMINI_API_KEY` or `GOOGLE_API_KEY` is set and no other provider is configured.

### Vertex AI (Gemini)

If you have GCP infrastructure already:

```bash
gcloud auth application-default login
export GOOGLE_GENAI_USE_VERTEXAI=true
export GOOGLE_CLOUD_PROJECT=my-gcp-project
export GOOGLE_CLOUD_LOCATION=us-central1
core-agent -p "what's the capital of France?"
```

### Anthropic / Claude (first-party)

Get a key at [console.anthropic.com](https://console.anthropic.com).

```bash
export ANTHROPIC_API_KEY=...
core-agent --provider anthropic --model claude-opus-4-7 -p "what's the capital of France?"
```

### Anthropic / Claude via Vertex AI

If you'd rather use your existing GCP credentials and billing for Claude:

```bash
gcloud auth application-default login
export ANTHROPIC_VERTEX_PROJECT_ID=my-gcp-project   # or GOOGLE_CLOUD_PROJECT
export CLOUD_ML_REGION=us-east5                     # or GOOGLE_CLOUD_LOCATION
core-agent --provider anthropic-vertex --model claude-opus-4-7 -p "what's 2+2?"
```

Note: Vertex's Claude model IDs sometimes carry a `@version` suffix (e.g. `claude-opus-4-5@20251101`). If the bare alias doesn't resolve, check the [Vertex Model Garden](https://console.cloud.google.com/vertex-ai/model-garden) for the current ID.

See the [Providers reference]({{< relref "providers.md" >}}) for full details on each backend.

---

## Multi-turn REPL

Drop the `-p` flag to enter the stdin REPL. Conversation history is preserved across turns automatically.

```text
$ core-agent
core-agent REPL — /exit or Ctrl-D to quit
> Remember the number 73.
Got it — I'll remember 73.
> What number did I just give you?
73.
> /exit
```

Built-in commands: `/exit`, `/quit`, EOF (Ctrl-D).

---

## Layer in a project — the `.agents/` directory

`core-agent` walks up from the current working directory looking for a folder named `.agents/`, much like `git` looks for `.git`. It's the project-level home for everything `core-agent` reads or writes:

```
your-repo/
├── .agents/
│   ├── config.json          # provider, model, permissions, etc.
│   ├── mcp.json             # MCP server declarations
│   ├── skills/              # SKILL.md bundles
│   │   └── echo/SKILL.md
│   └── sessions/            # one-shot transcripts (auto-written)
└── AGENTS.md                # system prompt prefix (project-scoped)
```

A minimal `config.json`:

```json
{
  "version": 1,
  "model": {
    "provider": "anthropic",
    "name": "claude-opus-4-7"
  }
}
```

`core-agent` will pick up everything in `.agents/` automatically — no flags needed. See the [Configuration reference]({{< relref "configuration.md" >}}) for the full schema.

### Pin a system prompt with `AGENTS.md`

Drop a file named `AGENTS.md` at your repo root and `core-agent` prepends its contents to every system prompt:

```markdown
You are a helpful assistant for the Acme widget team.
Answer in plain prose. Do not use bullet lists unless explicitly asked.
```

Fallback chain: `AGENTS.md` → `CLAUDE.md` → `GEMINI.md` (first match wins). User-global memory at `~/.core-agent/AGENTS.md` is concatenated before the project file.

---

## Useful flags

Beyond `--provider` / `-m` / `-p`, three flags come up often:

```
--ask=stdin|auto|off            register an ask_user tool the model can call
                                (auto = stdin if interactive, refuse otherwise)
--session-db                    persist sessions + audit log to a durable database
                                (default off; in-memory)
--session-db-path=PATH          override the database path (default: ~/.<binary>/sessions.db)
```

Use `--ask=auto` when your `AGENTS.md` instructs the model to ask before some action — the agent gets a clean refusal in headless contexts instead of blocking forever. See [Library API → Prompter]({{< relref "library-api.md#prompter" >}}).

Use `--session-db` to persist conversation history across restarts and unlock the audit-log + crash-resume flows. See [Sessions and event log]({{< relref "sessions.md" >}}).

For long-running unattended work, see [Autonomous runs]({{< relref "autonomous.md" >}}).

---

## What to read next

- [Providers]({{< relref "providers.md" >}}) — full reference for each model backend, env vars, and gotchas
- [Configuration]({{< relref "configuration.md" >}}) — every field of `.agents/config.json`
- [MCP servers]({{< relref "mcp.md" >}}) — declarative third-party tool integration
- [Skills]({{< relref "skills.md" >}}) — Claude-compatible `SKILL.md` bundles
- [Permissions]({{< relref "permissions.md" >}}) — gating tool calls
- [Library API]({{< relref "library-api.md" >}}) — using `core-agent` from your own Go code
- [Autonomous runs]({{< relref "autonomous.md" >}}) — `agent.RunAutonomous` for unattended workers
- [Sessions and event log]({{< relref "sessions.md" >}}) — durable sessions, audit log, replay, crash-resume
