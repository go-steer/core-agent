# Acceptance Test Plan — Milestone 1

**Scope:** verify the M1 deliverables of `core-agent`: the library extraction from cogo, the new Anthropic / Claude `model.LLM` adapter, the headless CLI (one-shot + REPL), and end-to-end behavior with AGENTS.md / MCP / skills / permissions.

**Out of scope for M1** (do not test, by design): subagents, Bubble Tea TUI, file-backed session service, slash-command framework beyond `/exit` and `/quit`.

**Audience:** anyone validating an `core-agent` build (a maintainer cutting a release, a downstream consumer evaluating it, a CI bot running the gated tests).

---

## How to use this plan

Each test has:

- **ID** — stable identifier (e.g. `T4.1`); reference these in PR descriptions and bug reports.
- **Preconditions** — what must be true before the test runs (env vars set, fixtures placed).
- **Steps** — exact commands to run, with full paths.
- **Expected** — what success looks like.
- **Pass criteria** — the explicit observable that decides pass/fail.

Tests are tagged:

- 🟢 **automated** — covered by `go test ./...`; just running the suite verifies them.
- 🟡 **smoke** — manual but credential-free; should be run on every release candidate.
- 🔵 **provider-gated** — requires real API credentials (Gemini, Vertex, or Anthropic). Run when the relevant `*_API_KEY` is available.

A release is **ready** when every 🟢 and 🟡 test passes and at least one 🔵 test passes for each provider that is being claimed as supported in the release notes.

---

## Section 1 — Build & static checks 🟢

### T1.1 — `go build ./...` succeeds

**Steps:**

```bash
cd /path/to/core-agent
go build ./...
```

**Expected:** exits 0, no output, no errors.

**Pass:** exit code 0; `echo $?` returns `0`.

### T1.2 — `go vet ./...` clean

**Steps:** `go vet ./...`

**Pass:** exit 0, no output.

### T1.3 — `go test ./...` all pass

**Steps:** `go test ./...`

**Expected:** every package with tests reports `ok`; no `FAIL`. Packages with no test files report `[no test files]` — that's not a failure.

**Pass:** the final line of output is not `FAIL`; `echo $?` is `0`.

### T1.4 — `go install ./cmd/core-agent` produces a working binary

**Steps:**

```bash
go install ./cmd/core-agent
"$(go env GOBIN)/core-agent" -h 2>&1 | head -10
```

**Expected:** `Usage of …/core-agent:` followed by the four flags `-c`, `-m`, `-p`, `-provider`.

**Pass:** all four flags appear in the help output.

---

## Section 2 — Library smoke 🟡

No credentials required; just compile-checks for the example programs.

### T2.1 — `examples/basic` builds

**Steps:** `go build ./examples/basic`

**Pass:** exit 0.

### T2.2 — `examples/with-tools` builds

**Steps:** `go build ./examples/with-tools`

**Pass:** exit 0.

---

## Section 3 — CLI surface 🟡

### T3.1 — Help output includes every flag

**Steps:** `core-agent -h 2>&1`

**Expected:** output contains `-c`, `-m`, `-p`, `-provider`. Each line has a description.

**Pass:** all four flag tokens grepable in stdout.

### T3.2 — No-credentials error message is actionable

**Preconditions:** `unset GOOGLE_API_KEY ANTHROPIC_API_KEY GOOGLE_GENAI_USE_VERTEXAI GOOGLE_CLOUD_PROJECT`.

**Steps:** `core-agent -p hi`

**Expected:** stderr contains `no provider configured and none could be auto-detected` and names at least one of `GOOGLE_API_KEY` / `ANTHROPIC_API_KEY` / `GOOGLE_GENAI_USE_VERTEXAI`. Exit code 2 (`ExitConfigError`).

**Pass:** stderr substring match + exit code 2.

### T3.3 — Unknown provider produces a clear error

**Preconditions:** `GOOGLE_API_KEY=fake`.

**Steps:** `core-agent --provider openai -p hi`

**Expected:** stderr contains `unknown provider "openai"` and lists the registered providers (`gemini`, `vertex`, `anthropic` in some order). Exit code 2.

**Pass:** error message + exit code 2.

---

## Section 4 — Provider: Gemini 🔵

**Preconditions:** `export GOOGLE_API_KEY=<real key>`.

### T4.1 — One-shot returns a streamed response

**Steps:**

```bash
core-agent -p "What is 2 + 2? Answer with just the number."
```

**Expected:** stdout contains `4`. Exit code 0. Stderr ends with the one-line usage summary (`core-agent: 1 turn(s) · …`).

**Pass:** stdout contains `4`; summary line appears on stderr.

### T4.2 — REPL preserves history across turns

**Steps:**

```bash
printf '%s\n' "Remember the number 73." "What number did I just give you?" "/exit" | core-agent
```

**Expected:** the second response references `73` (the model recalls the prior turn).

**Pass:** stdout contains `73` after the second `>` prompt.

### T4.3 — `--provider vertex` selects Vertex backend

**Preconditions:** `GOOGLE_GENAI_USE_VERTEXAI=true`, valid ADC, `GOOGLE_CLOUD_PROJECT=<project>`, `GOOGLE_CLOUD_LOCATION=<region>`.

**Steps:** `core-agent --provider vertex -p "ping"`

**Expected:** non-empty assistant text on stdout; exit 0.

**Pass:** any non-empty stdout response + exit 0.

---

## Section 5 — Provider: Anthropic / Claude 🔵

**Preconditions:** `export ANTHROPIC_API_KEY=<real key>`.

### T5.1 — One-shot via flag

**Steps:**

```bash
core-agent --provider anthropic -m claude-opus-4-7 -p "Say the word 'pong' and nothing else."
```

**Expected:** stdout contains `pong`. Exit 0.

**Pass:** stdout contains `pong`; exit 0.

### T5.2 — Auto-detection picks Anthropic when only `ANTHROPIC_API_KEY` is set

**Preconditions:** `unset GOOGLE_API_KEY GOOGLE_GENAI_USE_VERTEXAI GOOGLE_CLOUD_PROJECT`. `ANTHROPIC_API_KEY` set.

**Steps:** `core-agent -p "ping"`

**Expected:** non-empty assistant text; exit 0; stderr summary line includes a `claude-` model name.

**Pass:** non-empty response + summary references a Claude model.

### T5.3 — REPL multi-turn coherence (Claude)

**Steps:**

```bash
printf '%s\n' "My favorite fruit is mango." "Name my favorite fruit." "/exit" | \
  core-agent --provider anthropic
```

**Pass:** the second response contains `mango`.

### T5.4 — Streaming arrives incrementally, not in one batch

**Steps:**

```bash
core-agent --provider anthropic -p "Count from 1 to 10, one number per line." \
  | head -c 1 ; echo
```

**Expected:** the first byte arrives well before the full response (i.e. `head -c 1` returns quickly).

**Pass:** first character returned in under ~3 seconds; suggests partials are flushing as they arrive rather than buffered to end-of-turn.

### T5.5 — Tool round-trip via `examples/with-tools`

**Preconditions:** `ANTHROPIC_API_KEY` set; `cd /path/to/core-agent`.

**Steps:** `go run ./examples/with-tools`

**Expected:** stderr shows `→ add` (the assistant called the `add` tool); stdout includes the result `42`.

**Pass:** both observables present.

### T5.6 — System prompt is extracted, not duplicated

**Preconditions:** `ANTHROPIC_API_KEY` set; create a temp `AGENTS.md` in the working directory containing the line `Always end your response with the literal string [DONE].`

**Steps:**

```bash
mkdir -p /tmp/ca-test/.agents && cd /tmp/ca-test
echo "Always end your response with the literal string [DONE]." > AGENTS.md
core-agent --provider anthropic -p "say hi"
```

**Expected:** stdout response ends with `[DONE]` (instruction was applied as a top-level system block, not appended to the user message).

**Pass:** `[DONE]` substring appears in the response.

---

## Section 6 — AGENTS.md instruction loading 🟡 + 🔵

### T6.1 — Project AGENTS.md is loaded

**Preconditions:** working directory contains `AGENTS.md` with a recognizable instruction. Provider creds available.

**Steps:** `core-agent -p "what's your prime directive?"`

**Pass:** response references the AGENTS.md content.

### T6.2 — `CLAUDE.md` fallback when no AGENTS.md

**Preconditions:** working directory has `CLAUDE.md` only (no `AGENTS.md`, no `GEMINI.md`).

**Steps:** same as T6.1.

**Pass:** response references the CLAUDE.md content.

### T6.3 — User-global instruction concatenates with project

**Preconditions:** `~/.core-agent/AGENTS.md` exists with content `<user-line>`. Working directory has `AGENTS.md` with `<project-line>`.

**Steps:** any prompt; inspect agent.Run via `examples/basic` modified to print the constructed instruction (or run with `OTEL_EXPORTER=console`).

**Expected:** the assembled instruction prefix contains both lines, **user before project**.

**Pass:** verified via `instruction.Load()` unit test (already present at [`instruction/load_test.go:TestLoad_UserAndProjectConcatenated`](./instruction/load_test.go)) — the unit test is the authoritative check; this acceptance test confirms the wiring at runtime.

---

## Section 7 — MCP servers 🟡 + 🔵

### T7.1 — Missing `mcp.json` is a no-op

**Preconditions:** `.agents/` exists but no `mcp.json`.

**Steps:** any `core-agent -p ...` with creds.

**Pass:** no MCP-related stderr noise; agent runs normally. Inspectable: `mcp.Build()` returns nil, nil, nil.

### T7.2 — stdio MCP server tools become callable (manual)

**Preconditions:** an `.agents/mcp.json` declaring one stdio server (e.g. the official filesystem server). `ANTHROPIC_API_KEY` set.

**Steps:**

```bash
core-agent --provider anthropic -p "Use the filesystem tool to list files in /tmp."
```

**Expected:** stderr contains `→ filesystem_*` (a namespaced tool call); the response describes the contents of `/tmp`.

**Pass:** namespaced tool call observed.

### T7.3 — Failed MCP server doesn't kill the run

**Preconditions:** `mcp.json` includes one stdio server with a non-existent command, alongside one good server.

**Steps:** any prompt.

**Expected:** stderr shows `core-agent: mcp: …` for the bad server; the agent still runs and the good server's tools work.

**Pass:** error surfaced as a warning, not a fatal exit; exit code 0.

### T7.4 — Env-var interpolation in `mcp.json`

**Preconditions:** `mcp.json` contains `"Authorization": "Bearer ${env:MY_TOKEN}"` in a header. Set `MY_TOKEN=test`.

**Pass:** unit test [`mcp/config_test.go:TestInterpolateMap`](./mcp/config_test.go) green; runtime confirmed by inspecting outgoing MCP requests if needed.

---

## Section 8 — Skills 🟡 + 🔵

### T8.1 — SKILL.md is discovered and invokable

**Preconditions:** `.agents/skills/echo/SKILL.md` exists with valid frontmatter. Provider creds set.

**Steps:**

```bash
core-agent -p "Use the echo skill to repeat the word 'hello'."
```

**Expected:** stderr shows `→ echo` (skill invoked); stdout response references the skill output.

**Pass:** namespaced skill tool call observed.

### T8.2 — Missing skills dir is a no-op

**Pass:** unit test [`skills/load_test.go:TestLoad_NoSkillsDir`](./skills/load_test.go) green.

---

## Section 9 — Permissions 🟢

### T9.1 — Bash denylist is non-overridable

**Pass:** [`permissions/gate_test.go:TestGate_BashDenylistAlwaysWins`](./permissions/gate_test.go) green — even `ModeYolo` cannot run `rm -rf /`.

### T9.2 — Path scope blocks out-of-scope file access in `ask` mode without a prompter

**Pass:** [`permissions/gate_test.go:TestGate_AskMode_NoPrompterFailsClearly`](./permissions/gate_test.go) green.

### T9.3 — `allow` mode rejects calls without an explicit allowlist entry

**Pass:** [`permissions/gate_test.go:TestGate_AllowMode_RequiresExplicitAllow`](./permissions/gate_test.go) green.

### T9.4 — `Decision*` semantics

**Pass:** [`permissions/sessiontool_test.go`](./permissions/sessiontool_test.go) green — `DecisionAllowSessionTool` suppresses further prompts for the same tool.

---

## Section 10 — Telemetry, usage, transcripts 🟡 + 🔵

### T10.1 — One-shot writes a transcript when `.agents/` exists

**Preconditions:** `mkdir -p /tmp/ca-test/.agents && cd /tmp/ca-test`; provider creds set.

**Steps:** `core-agent -p "ping"` then `ls .agents/sessions/`.

**Expected:** one new file matching `<RFC3339-timestamp>.json` containing the prompt and usage totals.

**Pass:** file exists; `jq .messages[0].text` returns `"ping"`.

### T10.2 — No transcript when `.agents/` is absent

**Preconditions:** clean working directory with no `.agents/` discoverable up the tree.

**Steps:** `core-agent -p "ping"`.

**Pass:** no new file written; no error.

### T10.3 — One-line usage summary on stderr

**Steps:** any one-shot run with creds.

**Pass:** stderr's last line matches `core-agent: 1 turn(s) · ↑\d+ ↓\d+ tokens · \$\d+\.\d+ (...)`.

### T10.4 — OTEL console exporter emits spans

**Preconditions:** `.agents/config.json` with `"otel": {"exporter": "console"}` (or set via env per `telemetry.Setup`).

**Pass:** stderr contains JSON span output during the run.

---

## Section 11 — REPL behavior 🟡

### T11.1 — `/exit` cleanly returns 0

**Steps:** `printf '/exit\n' | core-agent` (with creds).

**Pass:** exit code 0.

### T11.2 — `/quit` cleanly returns 0

**Pass:** as T11.1 with `/quit`.

### T11.3 — Ctrl-D (EOF on stdin) cleanly returns 0

**Steps:** `core-agent < /dev/null` (with creds).

**Pass:** exit 0.

### T11.4 — Empty input continues without prompting

**Steps:** `printf '\n\nhi\n/exit\n' | core-agent`.

**Pass:** the agent ignores blank lines, runs one turn for `hi`, then exits 0.

### T11.5 — Ctrl-C cancels mid-response without leaking goroutines

**Steps:** Start the REPL; submit a long prompt; press Ctrl-C while streaming.

**Expected:** REPL exits with code 0 (signal context honored); no goroutine-leak panics on shutdown.

**Pass:** clean exit; no panic in stderr.

---

## Section 12 — Anthropic adapter unit coverage 🟢

The new code in `models/anthropic/` carries the highest regression risk. Cover via existing tests:

| Test | What it pins |
|---|---|
| [`TestBuildParams_TextOnly`](./models/anthropic/convert_test.go) | Plain user-text request shape |
| `TestBuildParams_SystemExtractedAndCached` | System prompt extracted to top-level + ephemeral cache_control attached |
| `TestBuildParams_RoleMapping` | genai `user`/`model` ↔ Anthropic `user`/`assistant` |
| `TestBuildParams_ToolRoundTrip` | Multi-turn user → assistant(tool_use) → user(tool_result) → assistant |
| `TestBuildParams_ToolDeclarations` | genai.Schema → Anthropic InputSchema with required fields |
| `TestBuildParams_MaxTokensOverride` | Config override beats default |
| `TestMapStopReason` | Every Anthropic StopReason maps to a sensible genai FinishReason |
| `TestFinalResponseFromMessage_TextAndToolUse` | Accumulated Message → genai Content with text + FunctionCall parts; usage propagated |

**Pass:** all eight tests green under `go test ./models/anthropic/...`.

---

## Section 13 — Extension points 🟢

### T13.1 — Custom `Provider` registers and resolves

**Pass:** the registry is exercised by every provider package's init() (`gemini` + `anthropic`) and by `models.Resolve` in `gemini_test.go`. Adding a third provider in a downstream project — via `models.Register("foo", ctor)` — is the canonical extension path; verified by importing `models/anthropic` from `cmd/core-agent` and seeing it auto-register.

### T13.2 — `agent.WithTools` adds tools without breaking the runner

**Pass:** `examples/with-tools` (T5.5) exercises this end-to-end with the `add` function tool.

### T13.3 — `agent.WithAppName` overrides the runner identity

**Pass:** verified by code inspection — [`agent/agent.go`](./agent/agent.go) `New()` passes `o.appName` to `runner.Config{AppName: …}`. No regression test today; add one in M2 if AppName is referenced more broadly.

---

## Release-readiness gate

Before tagging an M1 release:

1. ✅ Sections 1, 9, 12, 13 (🟢 automated) — must all pass via `go test ./...`.
2. ✅ Sections 2, 3, 6 (🟡 smoke) — manual run of every test in these sections.
3. ✅ Section 4 (Gemini 🔵) — at minimum T4.1 and T4.2 if the release notes claim Gemini support.
4. ✅ Section 5 (Anthropic 🔵) — at minimum T5.1, T5.3, T5.5 if release notes claim Claude support. T5.5 is the load-bearing one — it's the only end-to-end check that the new adapter handles a tool round-trip correctly.
5. ✅ Section 11 (REPL 🟡) — T11.1, T11.3 minimum.

If any 🟢 fails, **block** the release. If a 🔵 test for a claimed provider fails, either fix or downgrade the release notes to remove that provider claim.

---

## Notes for M2 and beyond

When a future milestone adds capability (subagents, file-backed sessions, slash commands, additional providers), append a new section here rather than editing existing ones — that way the document stays an honest record of what each milestone was held to.
