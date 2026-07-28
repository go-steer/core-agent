# `streaming` — built-in tools + terminal chat rendering

## What this shows

An agent wired with the standard built-in tools (`read_file`,
`list_dir`, `bash`, ...) behind a permissions gate, driven from a
command-line prompt and rendered as an interactive chat session via
`runner.WriteEvents`. When stdout is a terminal, tool calls render in
cyan and partial assistant text in green; pipe the output and the same
code path emits plain text with no escape codes.

The permissions mode is set to `yolo` so tool calls auto-approve — the
example has no human in the loop. Real apps keep `ask` and wire a
prompter into `permissions.FromConfig`.

## Run it

Needs real provider credentials; the provider auto-detects from env
when `--provider` is omitted:

```bash
GEMINI_API_KEY=...    go run ./examples/streaming "what's in main.go in this directory?"
ANTHROPIC_API_KEY=... go run ./examples/streaming --provider anthropic "list the .go files here"
```

Pick a prompt that forces tool use ("how many lines of go code in this
repo?" → `bash`) to see the formatter work.

## Key APIs

- `runner.WriteEvents` / `runner.WithColor` / `runner.IsTerminal` — `github.com/go-steer/core-agent/v2/pkg/runner`
- `tools.Build` + `tools.Default` — `github.com/go-steer/core-agent/v2/pkg/tools`
- `permissions.FromConfig` / `permissions.ModeYolo` — `github.com/go-steer/core-agent/v2/pkg/permissions`
- `agent.New` / `agent.WithTools` — `github.com/go-steer/core-agent/v2/pkg/agent`

## What you should see

```
> what's in main.go in this directory?
→ read_file {"path": "main.go"}
main.go is the streaming example: it builds the default toolset, ...
```

with the `→ tool` lines colored when run in a terminal.

## Next

- [`with-tools`](../with-tools/) — define your own tool and load MCP servers + skills.
- [`replay`](../replay/) — run the same event loop offline against a recorded transcript.
- Library guide: [`docs/site/src/content/docs/embed/guide.md`](../../docs/site/src/content/docs/embed/guide.md)
