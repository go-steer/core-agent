# core-agent project memory

When an AGENTS.md-aware agent runs inside this repo, this file is
loaded into the agent's system prompt as the project-level
instruction prefix. Keep it short and load-bearing.

## What this project is

`core-agent` is a reusable Go base agent built on the Google ADK
(`google.golang.org/adk`). It's the bottom layer for any project that
needs a multi-turn LLM agent in Go — model providers, MCP servers,
skills, instruction loading, permission gating, telemetry, transcript
persistence — without the consumer-specific bits (no built-in
bash/file tools, no TUI, no slash-command framework).

It deliberately mirrors the structure and conventions of
[`go-steer/cogo`](https://github.com/go-steer/cogo), the project it
was extracted from.

## Layout

```
agent/                ADK llmagent + runner wrapper; Option pattern.
instruction/          AGENTS.md / CLAUDE.md / GEMINI.md fallback loader.
config/               .agents/config.json schema + discovery + atomic Save.
permissions/          ask/allow/yolo gate + bash denylist + path scope.
tools/                GateToolset wrapper (bridges permissions to ADK toolsets).
mcp/                  mcp.json schema + stdio/HTTP server lifecycle.
skills/               SKILL.md discovery → ADK skilltoolset.
models/
  provider.go         Provider interface + registry/Resolve.
  gemini/             Gemini API + Vertex AI.
  anthropic/          Native model.LLM adapter for Claude
                      (api.anthropic.com + Vertex AI backends).
telemetry/            OpenTelemetry exporter setup.
usage/                Per-turn token + cost tracker.
session/              Transcript persistence (.agents/sessions/).
runner/               Headless (one-shot) + REPL (multi-turn) drivers.
cmd/core-agent/       Reference CLI binary.
examples/             Library use examples.
dev/                  Build/test/lint tooling — see dev/README.md.
docs/                 Internal design docs (acceptance-mN.md, ...).
docs/site/            Published Hugo site (Hextra theme).
```

## Build & test

```bash
dev/tools/ci          # full local CI in fast-fail order
dev/tools/build       # go build ./...
dev/tools/test-unit   # go test -race -coverprofile, all packages
dev/tools/lint-go     # golangci-lint (auto-installs v2.12.1)
dev/tools/fix-go-format  # auto-fix gofmt + goimports
```

Provider-gated tests (e.g. `models/anthropic/vertex_test.go::TestResolve_AnthropicVertex_FromConfig`)
skip cleanly when their creds aren't present. The default test run
needs no network and no API keys.

## Conventions

- **Plan before non-trivial work.** Milestones are designed in plan
  mode; each lands as one or a few focused commits with an
  `acceptance-mN.md` plan written first.
- **License headers everywhere.** The full Apache 2.0 boilerplate
  attributed to Google LLC sits at the top of every Go / shell /
  YAML / Python source file. The `goheader` linter inside
  `dev/tools/lint-go` enforces this on `.go` files; for new shell /
  YAML / Python files, run `dev/tools/add-license-headers` (idempotent;
  also normalizes any older SPDX-style headers to the canonical form).
- **Small, self-contained commits with informative bodies.** Subject
  lines follow Conventional Commits (`feat:`, `fix:`, `docs:`,
  `chore:`, `refactor:`, `test:`, `ci:`, `build:`). Bodies explain
  *why* and call out the verification done.
- **No Co-Authored-By trailer.** Maintainer preference — author the
  work under your own name. DCO sign-off (`git commit -s`) is the
  expected practice; see [`CONTRIBUTING.md`](./CONTRIBUTING.md).
- **Tests before merging.** Every new package ships with unit tests.
  A new feature without a test is not done. A new bug fix without a
  regression test makes it easy for the bug to come back.
- **Errors flow to the user.** Provider / tool / config failures
  never panic — they surface as errors returned through the agent
  loop or as `core-agent: ...` lines on stderr.
- **Gate everything that consumes external state.** MCP and skill
  tool calls all pass through `permissions.Gate` so the same
  `ask` / `allow` / `yolo` semantics apply uniformly. Consumers that
  add their own tools should wrap them with `tools.GateToolset`.

## Pitfalls & gotchas (real ones we've hit)

- **`t.Setenv` and `t.Parallel()` don't mix** in Go's testing package.
  We hit this writing `models/anthropic/vertex_test.go`; tests that
  call `t.Setenv` cannot also call `t.Parallel()`.
- **ADK streaming requires `agent.RunConfig{StreamingMode: agent.StreamingModeSSE}`.**
  The default `StreamingModeNone` produces no `Partial` events.
- **ADK's `req.Tools` field is unused by the existing Gemini provider** —
  real tool declarations live on `req.Config.Tools` (`[]*genai.Tool`,
  each with `FunctionDeclarations`). The Anthropic adapter follows
  the same convention.
- **Anthropic's Vertex SDK panics on missing creds.**
  `vertex.WithGoogleAuth` calls `panic` when ADC isn't loadable. We
  load credentials explicitly via `google.FindDefaultCredentials` and
  pass them to `vertex.WithCredentials` so we surface a clean error
  instead.
- **Anthropic separates the system prompt** from messages — it's a
  top-level `System []TextBlockParam` field, not a role on the first
  message. The adapter's `convert.go` extracts it from
  `genai.GenerateContentConfig.SystemInstruction` and lifts it.
- **Vertex Claude model IDs sometimes carry `@VERSION` suffixes**
  (e.g. `claude-opus-4-5@20251101`). Bare aliases often work; if not,
  pass the date-suffixed form via `--model`.
- **Gemini function names must match `[A-Za-z0-9_]{1,64}`** — no dots
  in MCP tool namespaces; we use `<server>_<tool>` not
  `<server>.<tool>`. See `mcp/namespace.go::sanitizePrefix`.
- **The MCP SDK's `Toolset.Tools(ctx)` requires an
  `agent.ReadonlyContext`**, not a regular `context.Context`. There's
  a minimal stub at `mcp/listctx.go`.
- **ADK's `telemetry.New(...)` returns providers but does NOT install
  them as OTEL globals.** Always call
  `providers.SetGlobalOtelProviders()`. `telemetry/otel.go` does this.

## Status

M1 (extraction + Anthropic adapter) and M2 (Anthropic via Vertex AI)
shipped together as the initial commit. Acceptance plans live at
`docs/acceptance-m1.md` and `docs/acceptance-m2.md`. M3 candidates
listed in the [README's Milestones section](./README.md#milestones).

## Branch workflow

Single long-lived branch: `main`. Work happens on short-lived feature
branches (`feat/...`, `fix/...`, `chore/...`, `docs/...`) → PR
against `main` → merge once CI's four required status checks are
green. Branch protection on `main` requires `test`, `lint`,
`go mod tidy is clean`, and `govulncheck`; docs-only PRs satisfy
these via the companion `ci-docs.yml` workflow without running the
full Go pipeline. Commits should be DCO-signed off (`git commit -s`)
and follow Conventional Commits. See [`CONTRIBUTING.md`](./CONTRIBUTING.md)
for the full contributor flow.
