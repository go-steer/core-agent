# core-agent

A production-grade Go substrate for multi-turn LLM agents, built on the [Google Agent Development Kit](https://pkg.go.dev/google.golang.org/adk). Ships the wiring — providers, MCP, skills, permissions, durable sessions, remote attach, an in-process Bubble Tea TUI, and a headless CLI — so downstream projects can focus on their own tools and product logic.

**📚 Full documentation: [go-steer.github.io/core-agent](https://go-steer.github.io/core-agent/)**

[![CI](https://github.com/go-steer/core-agent/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/go-steer/core-agent/actions/workflows/ci.yml)
[![Docs](https://github.com/go-steer/core-agent/actions/workflows/docs.yml/badge.svg?branch=main)](https://go-steer.github.io/core-agent/)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](./LICENSE)
[![Go Reference](https://pkg.go.dev/badge/github.com/go-steer/core-agent/v2.svg)](https://pkg.go.dev/github.com/go-steer/core-agent/v2)
[![Release](https://img.shields.io/github/v/release/go-steer/core-agent?sort=semver)](https://github.com/go-steer/core-agent/releases/latest)

---

## Features

**Runtime**
- Multi-turn conversation via ADK's `runner.Runner`; parallel tool-call dispatch.
- In-process Bubble Tea TUI as the default TTY surface; `--no-tui` line REPL fallback; slim build (`-tags no_tui`) drops the TUI tree entirely.
- `agent.RunAutonomous` for unattended workers with turn / token / cost / wallclock budgets and model-driven termination; `ResumeAutonomous` picks up from a durable checkpoint after a crash.
- Long-session survivability: automatic post-turn compaction at ~85% context utilization, subtasks with `agentic_*` tool wrappers that keep bulk tool output out of the parent's context, and task-boundary checkpoints via `mark_task_done`. See [Context management](https://go-steer.github.io/core-agent/docs/reference/context-management/).

**Providers**
- Gemini API (`GEMINI_API_KEY` / `GOOGLE_API_KEY`) and Vertex AI (ADC + `GOOGLE_CLOUD_PROJECT`) with server-side `GoogleSearch` and `URLContext` wired up.
- Anthropic Claude via `api.anthropic.com` (`ANTHROPIC_API_KEY`) and via Vertex AI (ADC + `ANTHROPIC_VERTEX_PROJECT_ID`) as a native `model.LLM` adapter.
- Mock providers for credential-free testing: `--provider=echo` and `--provider=scripted --script=path.jsonl`; `--record-to=path.jsonl` captures any real session for later replay.

**Instructions, tools, MCP, skills**
- `AGENTS.md` primary + `AGENTS.d/*.md` overlay directory + `@include <path>` directive for composable, multi-file system instructions; three scopes (user `~/.core-agent/`, user-home `~/.agents/`, project) concatenated in order, with `CLAUDE.md` / `GEMINI.md` fallbacks.
- Built-in tool catalog covering files (`read_file`, `read_many_files`, `write_file`, `edit_file`, `delete_file`, `stat`, `list_dir`), search (`glob`, `grep`), data + network (`json_query`, `fetch_url`), shell (`bash`), planning (`todo`, opt-in `record_plan`), and interactive prompting (opt-in `ask_user`). Disable the whole suite with `--no-builtin-tools` or specific entries with `--disable-tools=bash,write_file`.
- MCP servers declared in `.agents/mcp.json` (project) and/or `~/.agents/mcp.json` (portable user scope); stdio and Streamable HTTP transports (including remote MCP with Google OAuth); tools are namespaced (`<server>_<tool>`) and route through the permission gate.
- Claude-compatible skills auto-discovered from `.agents/skills/<name>/SKILL.md` (project), `~/.agents/skills/` (portable user), and `~/.core-agent/skills/` (legacy user-global fallback).

**Permissions**
- `ask` / `allow` / `yolo` modes with pattern-based allow/deny lists, path-scope enforcement on file tools, URL-scope allowlist for `fetch_url`, and a non-overridable `bash` denylist that catches `rm -rf /`-class mistakes.
- **Plan-first enforcement** (`permissions.RequirePlanArtifact` + `record_plan` tool + `/replan` slash): denies mutating tool calls until the model has recorded a plan for the current turn — turns "research → approve → execute" from a prompt convention into a substrate primitive.
- Pluggable `Prompter` interface so the same gate works in a TTY, headless (`--ask=auto`), the in-process TUI, or a custom web frontend.

**Persistence + observability**
- Durable sessions and audit log via `eventlog.Open(...)` — SQLite (pure-Go, CGO-free), Postgres, or MySQL — with monotonic `seq`, `Since(seq)` replay, `Watch(seq)` live-tail, and a session lock for crash-safe multi-process resume.
- Opt-in OpenTelemetry export (console / OTLP); off by default so a fresh invocation makes zero outbound calls beyond the model.
- Per-model token + cost tracking with layered pricing (daily LiteLLM refresh, per-config overrides, longest-prefix fallback).

**Remote attach + operator surface**
- **Attach API**: HTTP + Server-Sent Events live-tail + `POST /sessions/<sid>/inject` + `/wake` for headless agents, over mTLS + bearer auth. Read-only `GET /sessions/<sid>/{tools,agents,status}` and `GET /permissions/snapshot` endpoints for operator dashboards.
- **Remote TUI client** (`core-agent-tui`, separately-versioned binary): thin bubble-tea shell on [`go-steer/core-tui`](https://github.com/go-steer/core-tui) that attaches to any daemon over the attach API — full slash parity with the in-process TUI (`/stats`, `/context`, `/compact`, `/done`, `/subagent`, `/btw`, `/memory`, `/skills`, `/mcp`, `/pricing`, `/perms`, `/allow`, `/deny`, `/reload`), per-turn cost footer, live tool-approval modals over HTTP, mid-turn `/inject`, session switcher.
- **Multi-session daemon**: one `core-agent` daemon serves many concurrent sessions with per-caller identity, ACL (`Owner`/`Viewers`/`Contributors`), permission grants, instruction overlays, and audit attribution. Sessions survive daemon restarts (config change, image upgrade, K8s pod replacement) — a reconnecting TUI resumes transparently. Opt-in via `multi_session.enabled: true`.
- **Attach-config** (`.agents/config.json` `attach.*` block + `${ENV_VAR}` expansion) so a K8s ConfigMap can replace half a dozen CLI flags.
- **Peer registration** for hub-and-spoke fleet discovery on top of attach.
- **Agent-card discovery**: opt-in `/.well-known/agent-card.json` endpoint describing name, description, skills, and required auth in the [A2A AgentCard](https://agent2agent.info/docs/concepts/agentcard/) shape so agent registries can index the binary.

**Subagents**
- In-process: `agent.WithSubagents([]*Agent)` for synchronous delegation; `agent.NewBackgroundAgentManager` + `spawn_agent` / `list_agents` / `check_agent` / `stop_agent` for background subagents the model spawns at runtime.
- Remote: `agent.NewSpawnRemoteAgentTool` with a consumer-supplied `RemoteAgentSpawner` for out-of-process spawning (gRPC / K8s Jobs / Cloud Run).
- Subagent events stream into the parent's audit log under a `Branch` label so the trail stays unified.

**Kubernetes triage sidecar** (v2.6+)
- The `k8s-event-watcher` sidecar now lives in [go-steer/k8s-lookout](https://github.com/go-steer/k8s-lookout) as the `lookout watch` subcommand (behavior-identical; image `ghcr.io/go-steer/lookout`) — a client-go informer that watches Kubernetes Events, filters + dedupes, and injects matched incidents into per-incident sessions on a core-agent daemon. Pairs with the bundled triage skill to drive diagnose → fix → verify loops via the GKE MCP, gated by plan-first. See [`examples/gke-troubleshoot-agent/`](./examples/gke-troubleshoot-agent/).

**Optional adapters**
- [`extras/scion-agent/`](./extras/scion-agent/) — runs `core-agent` inside [Scion](https://github.com/GoogleCloudPlatform/scion)'s container runtime with lifecycle status emission and a `sciontool_status` tool.
- `extras/ax-agent/` (on the `axplore` branch) — AX gRPC remote-agent packaging.

---

## Install

**CLI (Go toolchain, requires Go 1.26+):**

```bash
go install github.com/go-steer/core-agent/v2/cmd/core-agent@latest
```

**Pre-built binary (Sigstore-signed; linux/darwin × amd64/arm64):**

```bash
TAG=$(gh release view --repo go-steer/core-agent --json tagName -q .tagName)
OS=$(uname -s | tr A-Z a-z)
ARCH=$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')
gh release download "$TAG" --repo go-steer/core-agent \
  --pattern "core-agent_${TAG#v}_${OS}_${ARCH}.tar.gz"
tar xzf "core-agent_${TAG#v}_${OS}_${ARCH}.tar.gz"
./core-agent --version
```

`core-agent-tui` archives use the same naming pattern. Verification details are in each release's [notes footer](https://github.com/go-steer/core-agent/releases/latest).

**Container images (multi-arch amd64 + arm64; distroless static; Sigstore-signed):**

```bash
docker pull ghcr.io/go-steer/core-agent:latest        # daemon + in-process TUI
docker pull ghcr.io/go-steer/core-agent-slim:latest   # headless daemon, ~5MB smaller
docker pull ghcr.io/go-steer/core-agent-tui:latest    # remote TUI client only
```

Floating tags: `:latest`, `:X.Y.Z`, `:X.Y`, `:X` (semver, no `v` prefix), `:main`, `:main-<sha>`.

The K8s event-triage sidecar is now published from [go-steer/k8s-lookout](https://github.com/go-steer/k8s-lookout) as `ghcr.io/go-steer/lookout` (the `lookout watch` subcommand — behavior-identical to the former `ghcr.io/go-steer/k8s-event-watcher`; existing deployments swap the image reference with zero config change).

**Library:**

```bash
go get github.com/go-steer/core-agent/v2
```

> **Module path note (post-v2.7):** the module path carries the `/v2` suffix required by [Go's SIVE rule](https://go.dev/ref/mod#major-version-suffixes). Consumers upgrading from v2.6 or earlier need to rewrite imports: `github.com/go-steer/core-agent/pkg/...` → `github.com/go-steer/core-agent/v2/pkg/...`. Container images and source builds were never affected; only `go install ...@v2.X.Y` and `go get` require the migration. Pre-fix tags (v2.0.0 through v2.7.0-dev.4) can't be `go install`ed — use `@main`, a pinned commit SHA, container images, or a source build for those.

### Container variants at a glance

| Image | What it is | When to pull it |
|---|---|---|
| `core-agent` | Full daemon: multi-session runtime + in-process Bubble Tea TUI + remote-attach API. | Default target. |
| `core-agent-slim` | Same daemon, built `-tags no_tui`. ~5MB smaller. | Distroless K8s pods where the TUI is dead weight. |
| `core-agent-tui` | Remote TUI client only. Connects to a daemon over the attach API. | Operator workstations attaching to a remote daemon. |
| [`lookout`](https://github.com/go-steer/k8s-lookout) | Sidecar (`lookout watch`) that watches Kubernetes Events, dedupes, and injects matched ones into a daemon. Published from go-steer/k8s-lookout, not this repo. | GKE / K8s troubleshooting agents. |

---

## Quick start — CLI

```bash
# Gemini API key (either variable works)
GEMINI_API_KEY=... core-agent -p "what's 2+2?"

# Anthropic API key
ANTHROPIC_API_KEY=... core-agent --provider anthropic -p "what's 2+2?"

# Multi-turn TUI (bare invocation; conversation persists across turns)
core-agent
```

Vertex AI (both Gemini and Claude), scripted providers, `.agents/config.json` defaults, and the full CLI flag reference live in [Getting started](https://go-steer.github.io/core-agent/docs/getting-started/).

## Quick start — library

```go
package main

import (
    "context"
    "fmt"
    "log"

    "github.com/go-steer/core-agent/v2/pkg/agent"
    "github.com/go-steer/core-agent/v2/pkg/config"
    "github.com/go-steer/core-agent/v2/pkg/models"
    _ "github.com/go-steer/core-agent/v2/pkg/models/gemini"
)

func main() {
    cfg := config.DefaultConfig()
    cfg.Model.Provider = config.ProviderGemini

    provider, err := models.Resolve(cfg)
    if err != nil { log.Fatal(err) }

    ctx := context.Background()
    m, err := provider.Model(ctx, cfg.Model.Name)
    if err != nil { log.Fatal(err) }

    a, err := agent.New(m, agent.WithInstruction("Be concise."))
    if err != nil { log.Fatal(err) }

    for event, err := range a.Run(ctx, "What is the capital of France?") {
        if err != nil { log.Fatal(err) }
        if event.Content == nil { continue }
        for _, p := range event.Content.Parts {
            if p.Text != "" && event.Partial {
                fmt.Print(p.Text)
            }
        }
    }
    fmt.Println()
}
```

Fuller examples in [`examples/`](./examples/) — basic, with-tools, streaming, autonomous, autonomous-resume, with-subagent, background-monitor, scheduled-monitor, replay, plan-first, cloud-run-deploy, gke-*.

---

## Documentation

- **Site** — <https://go-steer.github.io/core-agent/>
- **Design rationale** — [`docs/DESIGN.md`](./docs/DESIGN.md)
- **Release process** — [`docs/release-process.md`](./docs/release-process.md)
- **Changelog + stability promise** — [`CHANGELOG.md`](./CHANGELOG.md)

The site is built with [Astro](https://astro.build) and [Starlight](https://starlight.astro.build); sources live in [`docs/site/`](./docs/site).

---

## Project layout

```
core-agent/
├── pkg/                    # public library surface (v1 stability)
│   ├── agent/              # ADK llmagent + runner wrapper; Option pattern
│   ├── attach/             # HTTP + SSE + auth + multi-session daemon
│   ├── config/             # .agents/config.json schema + discovery
│   ├── digest/             # structural digest system for tool responses
│   ├── eventlog/           # durable session.Service + audit/replay stream
│   ├── instruction/        # AGENTS.md / CLAUDE.md / GEMINI.md loader
│   ├── mcp/                # mcp.json schema, stdio/HTTP server lifecycle
│   ├── models/             # provider registry + gemini/ + anthropic/ + mock/
│   ├── permissions/        # ask/allow/yolo gate + denylist + path scope
│   ├── recording/          # LLM-wire recorder for offline replay
│   ├── runner/             # headless (one-shot) + REPL/TUI drivers
│   ├── session/            # JSON transcript persistence
│   ├── skills/             # SKILL.md discovery → ADK skilltoolset
│   ├── telemetry/          # OTEL exporter setup
│   ├── tools/              # built-in tools + GateToolset wrapper
│   └── usage/              # per-turn token + cost tracker
├── cmd/
│   ├── core-agent/         # daemon + CLI + in-process TUI
│   └── core-agent-tui/     # remote TUI client
├── extras/scion-agent/     # opt-in Scion runtime adapter
├── examples/               # 15+ worked examples
├── docs/                   # DESIGN.md, release-process.md, Astro site
├── SKILLS/                 # bundled meta-skills (cli-setup, autonomous-setup, library-embedding)
└── dev/                    # build/test/lint/release tooling
```

The `Provider` interface is the extension point — register your own model backend with `models.Register("name", constructor)` and the rest of the stack picks it up.

---

## Project conventions

- **`.agents/` directory** — walked up from the working directory like `.git`. Holds `config.json`, `mcp.json`, `skills/<name>/SKILL.md`, and per-session JSON transcripts.
- **`AGENTS.md`** — project-level system-instruction prefix. `CLAUDE.md` and `GEMINI.md` are picked up as fallbacks for repos that already have one.
- **`~/.<binary>/sessions.db`** — durable session storage when `--session-db` is set. The binary name is derived from `os.Executable()` so `core-agent`, adapters, and forks each get their own directory. Override with `--session-db-path`.

---

## Contributing

- Run `dev/tools/ci` before opening a PR — same checks GitHub Actions runs (vet, build, lint, mod-tidy, test, vuln scan), in fast-fail order. See [`dev/README.md`](./dev/README.md).
- Every source file carries the Apache 2.0 header. `goheader` in `dev/tools/lint-go` enforces this for `.go`; run `dev/tools/add-license-headers` for new shell / YAML / Python files.
- The library is meant to stay narrow. Downstream-specific tools, flags, and slash commands belong in consumer projects.

## License

Apache-2.0. See [LICENSE](./LICENSE).
