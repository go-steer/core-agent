# Scion harness bundle for core-agent

This directory stages the files Scion needs to launch `core-agent` as a Scion-managed agent. It is **not compiled into anything** on the core-agent side — it exists as reference and copy-source until Scion upstream adopts the bundle at `harnesses/core-agent/`, at which point this directory is deleted.

## What's in the bundle

| File | Purpose |
|---|---|
| `config.yaml` | Declarative harness config for Scion's `ContainerScriptHarness`. Tells Scion how to launch `core-agent`, what the task flag is, what env vars to inject. |
| `dialect.yaml` | Maps core-agent's hook event names → Scion's canonical event vocabulary. `pkg/hooks` emits Scion-canonical names already, so this is a near-identity mapping. |
| `Dockerfile` | Multi-stage build that layers `core-agent` (built from source) onto `scion-base`. Mirrors `harnesses/antigravity/Dockerfile` in shape (`ARG BASE_IMAGE`, root-then-scion user setup, `CMD` set to the runtime binary). Includes a commented alternative for the upstream mode that pulls the binary from the published GHCR image instead of rebuilding. |

## What core-agent provides on its side

Two features in the main binary make this bundle sufficient — no adapter binary required:

- **`pkg/hooks`** — config-driven shell-command dispatch on tool/model/turn boundaries. Configure the Scion consumer via `.agents/config.json`:
  ```json
  {
    "hooks": {
      "tool-start": [{"command": "sciontool hook --dialect=core-agent"}],
      "tool-end":   [{"command": "sciontool hook --dialect=core-agent"}],
      "model-start":[{"command": "sciontool hook --dialect=core-agent"}],
      "agent-end":  [{"command": "sciontool hook --dialect=core-agent"}]
    }
  }
  ```
  This drives Scion's `$HOME/agent-info.json` transient activity display (`thinking` / `executing` / `working`) via `sciontool hook`.

- **`sciontool_status` built-in tool** — automatically registered whenever `sciontool` is on `PATH`. The model calls this tool to signal sticky lifecycle states: `ask_user`, `blocked`, `task_completed`, `limits_exceeded`. Wired into Scion's `StatusHandler` the same way ADK's Python adapter does.

Instructions to the model should mention the tool (e.g., "call `sciontool_status(task_completed, "...")` when you're done"); place this in the agent's `AGENTS.md` (loaded automatically by core-agent).

## Building the image

The `Dockerfile` layers `core-agent` onto `scion-base`. You need a `scion-base` image available locally (build it from the Scion repo first).

```sh
# From the core-agent repo root — the builder stage needs the source tree
docker build \
  --build-arg BASE_IMAGE=scion-base:latest \
  -t scion-core-agent:latest \
  -f extras/scion/Dockerfile .
```

Multi-arch (matches how Scion builds Antigravity):

```sh
docker buildx build \
  --platform linux/amd64,linux/arm64 \
  --build-arg BASE_IMAGE=scion-base:latest \
  -t scion-core-agent:latest \
  -f extras/scion/Dockerfile .
```

Once upstream, the Dockerfile switches to pulling the `core-agent` binary from the published GHCR image (`ghcr.io/go-steer/core-agent:<version>`) instead of rebuilding — the alternative builder stage is commented in the Dockerfile itself.

## Staging the bundle into a Scion checkout for local testing

Until Scion upstream adopts this bundle, users who want to test the integration copy it into a local Scion checkout:

```sh
# From the core-agent repo root
cp -r extras/scion/. ../scion/harnesses/core-agent/
```

Then edit `../scion/harnesses/embed.go` to add `core-agent` to the compile-time embed:

```diff
-//go:embed all:antigravity/* all:claude/* all:codex/* all:copilot/* all:gemini-cli/* all:hermes/* all:opencode/*
+//go:embed all:antigravity/* all:claude/* all:codex/* all:copilot/* all:core-agent/* all:gemini-cli/* all:hermes/* all:opencode/*
```

Rebuild `sciontool`:

```sh
cd ../scion && go build -o sciontool ./cmd/sciontool
```

The Scion runtime can then create agents against the `core-agent` harness:

```sh
scion create core-agent-test "list the files in this directory"
scion attach core-agent-test   # drops you into core-agent's TUI (tmux + PTY)
scion message core-agent-test "now grep for TODO"   # types into the TUI input via send-keys
```

## Upstreaming

When the Scion team is ready to adopt this bundle, the intended upstream location is `harnesses/core-agent/` (same layout as `harnesses/antigravity/`). Steps:

1. Copy `config.yaml`, `dialect.yaml`, `Dockerfile`, `README.md` (rewritten to drop the "staging" framing) into `<scion>/harnesses/core-agent/`.
2. Switch the Dockerfile's builder stage to the commented "prebuilt from GHCR" block — Scion's tree doesn't have core-agent source.
3. Add `all:core-agent/*` to `harnesses/embed.go`.
4. Add a `cloudbuild.yaml` matching the shape of `harnesses/antigravity/cloudbuild.yaml` if Scion's release pipeline builds the image.
5. Delete `extras/scion/` from the core-agent repo.
6. Update `docs/site/content/docs/reference/scion-adapter.md` to point at the Scion-hosted copy.
