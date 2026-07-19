---
title: Scion adapter
---


[Scion](https://github.com/GoogleCloudPlatform/scion) runs `core-agent` directly — no adapter binary. Two features in the main `core-agent` binary cover the Scion contract:

- **`pkg/hooks`** — config-driven shell-command dispatch on tool/model/turn boundaries. Fires `sciontool hook --dialect=core-agent` to update Scion's transient-activity display (`$HOME/agent-info.json`).
- **`sciontool_status` built-in tool** — auto-registered when `sciontool` is on `PATH`. The model calls it to signal sticky lifecycle states (`ask_user`, `blocked`, `task_completed`, `limits_exceeded`) to Scion's hub.

The bundle staged at [`extras/scion/`](https://github.com/go-steer/core-agent/tree/main/extras/scion) holds the Scion-side files (harness `config.yaml`, event `dialect.yaml`, README) that Scion needs to launch `core-agent`. Ownership will shift to Scion's own `harnesses/core-agent/` once upstream adopts the bundle — until then, `extras/scion/` is the source of truth.

---

## How Scion drives core-agent

1. Scion launches `core-agent` inside a tmux session in a container. tmux gives it a real PTY, so core-agent's [TUI](/reference/tui/) auto-launches — `scion attach` shows the live TUI.
2. `scion message <agent> "..."` types into the TUI input field via `tmux send-keys`. No side-channel required.
3. As tool calls and model turns stream through the agent's event iterator, `pkg/hooks` shells out to `sciontool hook` with a JSON envelope on stdin, which updates `agent-info.json`.
4. When the model wants to signal a sticky state, it calls the `sciontool_status` tool, which shells out to `sciontool status <type> <message>`.

Outside a Scion container (no `sciontool` on `PATH`) the sticky-state tool isn't registered and the hooks config either goes unset or shells out to `/bin/true`-style no-ops — the same binary works for local development.

---

## Configuration

`.agents/config.json` in the agent's workspace:

```json
{
  "model": {"provider": "gemini", "name": "gemini-2.5-pro"},
  "hooks": {
    "tool-start":   [{"command": "sciontool hook --dialect=core-agent"}],
    "tool-end":     [{"command": "sciontool hook --dialect=core-agent"}],
    "model-start":  [{"command": "sciontool hook --dialect=core-agent"}],
    "agent-end":    [{"command": "sciontool hook --dialect=core-agent"}]
  }
}
```

Each handler's command is passed to `/bin/sh -c` with the envelope JSON on stdin. Handlers run sequentially with a per-command timeout (default 10s; override via `timeout_seconds`). See [Hooks](/concepts/hooks/) for the full mechanism.

Instructions to the model (typically in `AGENTS.md`) should mention the sticky-state tool: "Call `sciontool_status(task_completed, "...")` when the task is done; call `sciontool_status(ask_user, "...")` before waiting on user input."

---

## Container image

Any image with `core-agent`, `tmux`, and `sciontool` on `PATH` works. The `scion-base` image already ships tmux and sciontool; layering `core-agent` on top is a one-line Dockerfile:

```dockerfile
FROM scion-base:latest
COPY core-agent /usr/local/bin/core-agent
```

Or build `core-agent` from source inside the image if you prefer.

---

## Staging into a Scion checkout

Until Scion adopts the harness bundle upstream, copy it into a local Scion checkout:

```sh
cp -r extras/scion/. ../scion/harnesses/core-agent/
# then edit ../scion/harnesses/embed.go to include all:core-agent/*
cd ../scion && go build -o sciontool ./cmd/sciontool
```

Full instructions in [`extras/scion/README.md`](https://github.com/go-steer/core-agent/blob/main/extras/scion/README.md).

---

## Env-var contract

| Var | Set by | Purpose |
|---|---|---|
| `GOOGLE_API_KEY` | user / Scion | Gemini API key. |
| `GEMINI_API_KEY` | Scion's Gemini harness | Read directly by core-agent's gemini provider. |
| `ANTHROPIC_API_KEY` | user | Anthropic API key (when `model.provider: anthropic`). |
| `HOME` | container | Where `sciontool` writes `agent-info.json`. |

Other env vars follow core-agent's [Providers](/concepts/providers/) documentation.

---

## Sticky vs transient

| Kind | Examples | Mechanism | Frequency |
|---|---|---|---|
| Transient | `thinking`, `executing`, `working` | `sciontool hook` invoked by `pkg/hooks` on each tool/model boundary | per agent / tool boundary |
| Sticky | `ask_user`, `blocked`, `task_completed`, `limits_exceeded` | `sciontool status <type> <message>` invoked by the `sciontool_status` ADK tool | invoked intentionally by the model |

Scion's `sciontool` binary owns `$HOME/agent-info.json` — core-agent doesn't touch that file directly. Sticky states are also POSTed to Scion's Hub when running in hosted mode.

---

## See also

- [Hooks](/concepts/hooks/) — the general-purpose hook mechanism `pkg/hooks` exposes.
- [`extras/scion/README.md`](https://github.com/go-steer/core-agent/blob/main/extras/scion/README.md) — bundle staging + upstream instructions.
