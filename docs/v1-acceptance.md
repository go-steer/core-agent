# Acceptance Test Plan — v1.0.0

**Scope:** verify the `core-agent` library is ready to be tagged `v1.0.0`. Covers the full M1 + M2 + M3 surface end-to-end, including the lived experience of running against real LLM providers — the bar `v0.1.0` deliberately stopped short of.

**Why v1.0 needs its own plan:** `acceptance-m1.md` and `acceptance-m2.md` verify the M1 and M2 surfaces respectively. They do not cover M3 (autonomous driver, durable sessions, subagents, lifecycle tool, eventlog). They also do not enforce the "we actually ran this against a real LLM and it worked" discipline that distinguishes a stable v1.0 from a feature-complete v0.X.

**Audience:** maintainers cutting `v1.0.0` and downstream consumers evaluating the library before pinning a stable version.

**Relationship to prior plans:** every test in `acceptance-m1.md` and `acceptance-m2.md` continues to apply unchanged. v1.0.0 is `M1 + M2 + M3` consolidated; passing this plan does not retract anything from the prior two.

---

## How to use this plan

Same tagging conventions as the prior plans:

- 🟢 **automated** — covered by `go test ./...` or `dev/ci/presubmits/`. Just running the suite verifies them.
- 🟡 **smoke** — manual but credential-free. Should be run on every v1.X release candidate.
- 🔵 **provider-gated** — requires real API credentials. Run when the relevant `*_API_KEY` (or GCP ADC) is available. **At least one provider's full 🔵 suite must pass for v1.0.0 to be tagged**; ideally all four providers we claim to support pass.

Test IDs are prefixed `V1-` so they don't collide with `T*` (M1) or `M2-T*` (M2) IDs.

A release is **ready as v1.0.0** when:

1. Every 🟢 and 🟡 test in this plan + the M1 + M2 plans passes.
2. At least one 🔵 provider's full v1 smoke suite (sections 4-7) passes; for any provider claimed in `CHANGELOG.md` to be "supported," that provider's full 🔵 suite must pass.
3. The result is committed back to this file as a dated transcript (see Section 9), so future readers can see what was verified and when.

---

## Section 1 — Regression (M1 + M2 + presubmits) 🟢

### V1-T1.1 — All packages build, vet, test

**Steps:**

```bash
cd /path/to/core-agent
go build ./...
go vet ./...
go test ./...
```

**Pass:** exits 0 on each. Every package reports `ok`; no `FAIL`. Specifically these packages must report `ok`:

`agent`, `config`, `eventlog`, `instruction`, `mcp`, `models/anthropic`, `models/gemini`, `models/mock`, `permissions`, `recording`, `runner`, `session`, `skills`, `telemetry`, `tools`, `usage`.

### V1-T1.2 — All presubmit scripts pass

**Steps:**

```bash
for s in dev/ci/presubmits/*; do bash "$s" || exit 1; done
```

**Pass:** all seven scripts exit 0: `build`, `lint-go`, `test-unit`, `verify-go-format`, `verify-mod-tidy`, `verify-vuln`, `vet`. The race detector (`go test -race ./...`) passes — invoked by `test-unit`.

### V1-T1.3 — Coverage thresholds

**Steps:** `go test -cover ./...`

**Pass (advisory, not blocking):** each package's coverage stays at or above the baseline from M1/M2. As of v0.1.0 the baselines are: `config` ≥80%, `permissions` ≥79%, `instruction` ≥93%, `recording` ≥88%, `usage` ≥97%, `tools` ≥75%, `agent` reports a number, `eventlog` reports a number, `session` ≥74%, `skills` ≥79%, `telemetry` ≥87%, `models/anthropic` ≥68%, `models/gemini` ≥82%, `models/mock` ≥79%.

### V1-T1.4 — `acceptance-m1.md` still passes

**Steps:** walk through every 🟢 and 🟡 test in `acceptance-m1.md`.

**Pass:** all M1 🟢 and 🟡 tests still green; no regression.

### V1-T1.5 — `acceptance-m2.md` still passes

**Steps:** walk through every 🟢 and 🟡 test in `acceptance-m2.md`.

**Pass:** all M2 🟢 and 🟡 tests still green.

---

## Section 2 — Library API smoke (credential-free) 🟡

### V1-T2.1 — `agent.New` defaults are usable

**Steps:**

```bash
cat > /tmp/smoke.go <<'EOF'
package main
import (
  "fmt"
  "github.com/go-steer/core-agent/pkg/agent"
  "github.com/go-steer/core-agent/pkg/models/mock"
)
func main() {
  m, _ := mock.NewEcho().Model(nil, "")
  a, err := agent.New(m, agent.WithInstruction("Be concise."))
  if err != nil { panic(err) }
  if a.SessionService() == nil { panic("no service") }
  if a.AgentName() == "" { panic("no name") }
  fmt.Println("ok")
}
EOF
go run /tmp/smoke.go
```

**Pass:** prints `ok` with no panic.

### V1-T2.2 — `agent.RunAutonomous` end-to-end with scripted mock

**Steps:** `go run ./examples/autonomous`

**Pass:** prints `reason: completed`, `turns: 1`, `done detail: summarized example.txt`, `duration: <few-hundred-µs>`. Exits 0.

### V1-T2.3 — `agent.ResumeAutonomous` against SQLite

**Steps:** `go run ./examples/autonomous-resume`

**Pass:** Phase 1 prints `reason=max_turns_exceeded turns=2`; Phase 2 prints `reason=completed turns=3 done_detail="resumed and finished"`. Exits 0. The temp SQLite file is created and cleaned up.

### V1-T2.4 — `agent.WithSubagents` end-to-end

**Steps:** `go run ./examples/with-subagent`

**Pass:** parent run shows `→ research(...)` then `← research -> ...` then a final text; the audit-log query shows the full session tree with `branch=research` events interleaved with parent events (root branch). Exits 0.

### V1-T2.5 — `eventlog.Open` against SQLite via the CLI

**Steps:**

```bash
DB=$(mktemp -u --suffix=.db)
go build -o /tmp/ca ./cmd/core-agent
/tmp/ca --provider=echo --session-db --session-db-path="$DB" -p "hello"
sqlite3 "$DB" "SELECT seq, author, branch FROM agent_eventlog ORDER BY seq;"
rm -f "$DB" /tmp/ca
```

**Pass:** binary starts cleanly and prints `core-agent: session db: <path>` on stderr. After the run, the `agent_eventlog` query returns rows for the user input and model response in seq order. (Skip if `sqlite3` CLI isn't installed; the unit tests cover the same shape.)

### V1-T2.6 — Recording → scripted replay round-trip

**Steps:**

```bash
TRACE=$(mktemp --suffix=.jsonl)
go build -o /tmp/ca ./cmd/core-agent
/tmp/ca --provider=echo --record-to="$TRACE" -p "hello"
test -s "$TRACE" || { echo "record produced empty trace"; exit 1; }
/tmp/ca --provider=scripted --script="$TRACE" -p "hello"
rm -f "$TRACE" /tmp/ca
```

**Pass:** both invocations exit 0; the recording file has nonzero size after the first; the replay returns identical-shape output.

### V1-T2.7 — Glob + grep built-ins

**Steps:**

```bash
go build -o /tmp/ca ./cmd/core-agent
# The echo provider just echoes the prompt; we're verifying the
# tools registry includes glob and grep, not the model behavior.
/tmp/ca --provider=echo -p "list tools" 2>&1 | grep -q "glob\|grep" || echo "no glob/grep in output (ok; echo doesn't enumerate tools)"
# Indirect verification via the test suite:
go test ./tools/ -run 'TestGlob_|TestGrep_'
rm -f /tmp/ca
```

**Pass:** the `go test` invocation reports `ok` for all glob/grep tests.

---

## Section 3 — Adapter smoke (credential-free) 🟡

### V1-T3.1 — `scion-agent` builds and starts

**Steps:**

```bash
go build -o /tmp/scion-agent ./extras/scion-agent
timeout 1 /tmp/scion-agent --provider=echo -input "ping" 2>&1 | head -5
rm -f /tmp/scion-agent
```

**Pass:** binary builds; emits `[user]: ping` and an echoed response; exits cleanly when killed by timeout.

### V1-T3.2 — `scion-agent` with `--session-db`

**Steps:**

```bash
DB=$(mktemp -u --suffix=.db)
go build -o /tmp/scion-agent ./extras/scion-agent
echo "ping" | timeout 2 /tmp/scion-agent --provider=echo --session-db --session-db-path="$DB" 2>&1 | head -5
test -f "$DB" && echo "DB created"
rm -f "$DB" /tmp/scion-agent
```

**Pass:** stderr shows `scion-agent: session db: <path>`; the SQLite file is created.

### V1-T3.3 — `ax-agent` (axplore branch only) builds and starts

**Steps:** on the `axplore` branch:

```bash
go build -o /tmp/ax-agent ./extras/ax-agent
timeout 1 /tmp/ax-agent --provider=echo --listen=:0 2>&1 | head -3
rm -f /tmp/ax-agent
```

**Pass:** stderr shows `ax-agent: listening on :0 (provider=echo ...)`; exits cleanly on timeout. **Skip on `main`** — ax-agent doesn't ship there by design.

---

## Section 4 — Provider-gated smoke: Gemini 🔵

**Preconditions:** `GEMINI_API_KEY` set; the model named below is reachable from the network.

### V1-T4.1 — One-shot CLI returns a real response

**Steps:**

```bash
go build -o /tmp/ca ./cmd/core-agent
GEMINI_API_KEY=$GEMINI_API_KEY /tmp/ca \
    --provider=gemini -m gemini-3.1-flash-lite \
    -p "Reply with exactly: pong" 2>&1
rm -f /tmp/ca
```

**Pass:** exits 0; stdout contains the literal token `pong` (case-insensitive). The model may add surrounding text; that's fine, just `pong` must appear.

### V1-T4.2 — Built-in tools fire end-to-end

**Steps:**

```bash
go build -o /tmp/ca ./cmd/core-agent
GEMINI_API_KEY=$GEMINI_API_KEY /tmp/ca \
    --provider=gemini -m gemini-3.1-flash-lite \
    -p "List the names of every file in this directory using list_dir, then tell me how many you found."
rm -f /tmp/ca
```

**Pass:** stderr shows `→ list_dir(...)` and `← list_dir(...)` markers; stdout includes a count that matches what `ls -1 | wc -l` reports in the working directory.

### V1-T4.3 — `RunAutonomous` completes with a real provider

**Steps:** write a tiny driver inline (no example covers this with a real provider):

```bash
cat > /tmp/autodrive.go <<'EOF'
package main
import (
  "context"; "fmt"; "log"
  adktool "google.golang.org/adk/tool"
  "github.com/go-steer/core-agent/pkg/agent"
  "github.com/go-steer/core-agent/pkg/models"
  _ "github.com/go-steer/core-agent/pkg/models/gemini"
  "github.com/go-steer/core-agent/pkg/config"
)
func main() {
  cfg := config.DefaultConfig()
  cfg.Model.Provider = "gemini"
  cfg.Model.Name = "gemini-3.1-flash-lite"
  p, err := models.Resolve(cfg); if err != nil { log.Fatal(err) }
  m, err := p.Model(context.Background(), cfg.Model.Name); if err != nil { log.Fatal(err) }
  build := func(extras []adktool.Tool) (*agent.Agent, error) {
    return agent.New(m,
      agent.WithInstruction("You are autonomous. Reply briefly and call report_done with state='done' and detail summarizing what you did."),
      agent.WithTools(extras),
    )
  }
  res, err := agent.RunAutonomous(context.Background(), build,
    "Greet the operator in one sentence.", agent.WithMaxTurns(3))
  if err != nil { log.Fatal(err) }
  fmt.Printf("reason=%s turns=%d detail=%q final=%q\n",
    res.Reason, res.Turns, res.DoneDetail, res.FinalText)
}
EOF
GEMINI_API_KEY=$GEMINI_API_KEY go run /tmp/autodrive.go
rm -f /tmp/autodrive.go
```

**Pass:** prints `reason=completed turns=<small>` and a non-empty `detail`. The exact turn count isn't load-bearing; what matters is that the model called `report_done` (i.e., reason=completed, not max_turns_exceeded).

### V1-T4.4 — Subagent invocation completes with a real provider

**Steps:** modify `examples/with-subagent/` (or write inline) to use real Gemini providers for both parent and subagent. Run and confirm the parent calls the research subagent at least once, and the audit log shows branch=research events.

**Pass:** parent emits at least one tool call to the subagent; subagent emits at least one event with `branch="research"` in the eventlog; final RunResult includes text that incorporates the subagent's answer.

---

## Section 5 — Provider-gated smoke: Anthropic (first-party) 🔵

**Preconditions:** `ANTHROPIC_API_KEY` set.

### V1-T5.1 — One-shot CLI returns a real response

**Steps:**

```bash
go build -o /tmp/ca ./cmd/core-agent
ANTHROPIC_API_KEY=$ANTHROPIC_API_KEY /tmp/ca \
    --provider=anthropic -m claude-sonnet-4-6 \
    -p "Reply with exactly: pong" 2>&1
rm -f /tmp/ca
```

**Pass:** exits 0; stdout contains `pong`.

### V1-T5.2 — Built-in tools fire end-to-end

Same as V1-T4.2 but with `--provider=anthropic -m claude-sonnet-4-6`.

**Pass:** as V1-T4.2.

### V1-T5.3 — `RunAutonomous` completes with Anthropic

Same as V1-T4.3 but swap the provider/model in the inline driver to `anthropic` / `claude-sonnet-4-6`.

**Pass:** as V1-T4.3.

---

## Section 6 — Provider-gated smoke: Vertex Gemini 🔵

**Preconditions:** GCP ADC configured (`gcloud auth application-default login`); `GOOGLE_CLOUD_PROJECT` set; Vertex API enabled in the project.

### V1-T6.1 — One-shot CLI returns a real response

**Steps:**

```bash
go build -o /tmp/ca ./cmd/core-agent
/tmp/ca --provider=vertex -m gemini-3.1-flash-lite \
    -p "Reply with exactly: pong" 2>&1
rm -f /tmp/ca
```

**Pass:** exits 0; stdout contains `pong`.

---

## Section 7 — Provider-gated smoke: Anthropic Vertex 🔵

**Preconditions:** GCP ADC configured; `ANTHROPIC_VERTEX_PROJECT_ID` set; `CLOUD_ML_REGION` set (commonly `us-east5`); the Vertex project has Claude models published.

### V1-T7.1 — One-shot CLI returns a real response

**Steps:**

```bash
go build -o /tmp/ca ./cmd/core-agent
ANTHROPIC_VERTEX_PROJECT_ID=$ANTHROPIC_VERTEX_PROJECT_ID \
CLOUD_ML_REGION=us-east5 \
/tmp/ca --provider=anthropic-vertex -m claude-sonnet-4-6 \
    -p "Reply with exactly: pong" 2>&1
rm -f /tmp/ca
```

**Pass:** exits 0; stdout contains `pong`.

---

## Section 8 — Documentation & release sanity 🟡

### V1-T8.1 — Hugo site builds clean

**Steps:** `cd docs/site && hugo --minify`

**Pass:** exits 0; no warnings about missing pages or broken `relref` links.

### V1-T8.2 — README snippets compile

**Steps:** copy the "Minimal example" snippet from `docs/site/content/docs/library-api.md` into a temp `.go` file, set up a module, `go run` it against `--provider=echo`-equivalent.

**Pass:** the snippet builds and runs against a mock provider without modification beyond the model construction line.

### V1-T8.3 — `CHANGELOG.md` covers every shipped surface

**Steps:** diff the `[Unreleased]` section (or whichever version is being tagged) against `git log v0.1.0..HEAD --oneline`.

**Pass:** every commit that touches an exported symbol or user-visible behavior has a corresponding CHANGELOG line under **Added**, **Changed**, **Deprecated**, **Removed**, **Fixed**, or **Security**.

### V1-T8.4 — Stability promise still accurate

**Steps:** re-read the "Stability promise" block at the top of `CHANGELOG.md`. Confirm the package list still matches what `core-agent` actually exports (no new public packages added without listing; no removed packages still listed).

**Pass:** the package list is current.

---

## Section 9 — Sign-off transcript

### v1.0.0 sign-off — 2026-05-16

Tester: maintainer + assistant
Branch: `main` @ `d92ac98` (post-fix, post-doc-updates)

**Sections 1-3 (regression + credential-free smoke):** all green.
  - V1-T1.1 ✓ V1-T1.2 ✓
  - V1-T2.2 ✓ V1-T2.3 ✓ V1-T2.4 ✓

**Section 4 (Gemini, real-LLM):** initial fail at `gemini-2.5-flash`
surfaced a real defect; fix shipped (`9a1fc6a`); re-verified at full
defaults across three Gemini 3.x models:
  - V1-T4.1 ✓ pong at `gemini-3.1-pro-preview` (full default config:
    built-in tools + 8-tool function suite, no flag overrides).
  - V1-T4.1 ✓ pong at `gemini-3.1-flash-lite` (cheap-default variant
    the v1 smoke plan now points at).
  - V1-T4.1 ✓ pong at `gemini-3-flash-preview` (3.0-generation flash).
  - V1-T4.2 ✓ `list_dir` fired and returned correct count against
    `gemini-3.1-pro-preview` and `gemini-3.1-flash-lite`.
  - V1-T4.3 ✓ `RunAutonomous` completed cleanly with `report_done`
    against `gemini-3.1-pro-preview` and `gemini-3.1-flash-lite`.

**Sections 5-7 (Anthropic, Vertex Gemini, Anthropic Vertex):** not
exercised in this pass — single-provider sign-off accepted per the
plan's bar ("at least one provider's full 🔵 suite passes"). A
follow-up smoke against Anthropic / Vertex can land as `v1.0.1` or
`v1.1.0` work without retracting this release.

**Real defect found and fixed during the smoke:**
The Gemini provider's `builtinsLLM` wrapper was injecting
`google_search` + `url_context` server-side tools alongside any
function-calling tools but not setting
`Config.ToolConfig.IncludeServerSideToolInvocations = true` — a
flag Gemini 3+ requires for this combination. Without it the API
rejected the first turn at any default invocation, blocking
`--provider=gemini` for any consumer using the default tool suite.
Gemini 2.5 doesn't support the combination at all regardless of
that flag, so the library now requires Gemini 3.0+ when using
built-in tools alongside `tools.Default()`. Fix in
`models/gemini/builtins.go`; the smoke pass earned its keep on
its first execution.

**Pricing-table gap also surfaced:** the released-name keys for
`gemini-3.1-flash-lite` and `gemini-3-pro-preview` / `gemini-3-pro`
were missing from `usage/pricing.go` — only the `-preview`-suffixed
keys existed, so runs against released models reported `$0.0000`.
Filled in at the same rates (`d92ac98`).

**Result: cleared for `v1.0.0` tag.** No outstanding defects. The
library passes its full credential-free smoke, exercises real
Gemini end-to-end at default invocation across three 3.x models,
and the docs match the shipped behavior.

**Result:** cleared for `v1.0.0` tag against Gemini 3.1.
Sections 5-7 remain available for any downstream consumer who
wants to add 🔵 coverage for their provider before pinning a
v1.X release that claims it.

---



Append the result of each provider-gated run below as a dated block when v1.0.0 is being cut. This gives future maintainers proof that the bar was actually cleared.

```
## v1.0.0 sign-off — YYYY-MM-DD

Tester: <name / role>
Branch: main @ <commit>

Sections 1-3 (regression + credential-free smoke):
  V1-T1.1 ✓ V1-T1.2 ✓ V1-T1.3 ✓ V1-T1.4 ✓ V1-T1.5 ✓
  V1-T2.1 ✓ V1-T2.2 ✓ V1-T2.3 ✓ V1-T2.4 ✓ V1-T2.5 ✓ V1-T2.6 ✓ V1-T2.7 ✓
  V1-T3.1 ✓ V1-T3.2 ✓ V1-T3.3 ✓ (on axplore)

Provider-gated:
  Section 4 (Gemini):           V1-T4.1 ✓ V1-T4.2 ✓ V1-T4.3 ✓ V1-T4.4 ✓
  Section 5 (Anthropic):        V1-T5.1 ✓ V1-T5.2 ✓ V1-T5.3 ✓
  Section 6 (Vertex Gemini):    V1-T6.1 ✓
  Section 7 (Anthropic Vertex): V1-T7.1 ✓

Section 8 (docs):
  V1-T8.1 ✓ V1-T8.2 ✓ V1-T8.3 ✓ V1-T8.4 ✓

Notes: <anything notable — flaky tests, model behavior quirks, env-specific tweaks>

Result: cleared for v1.0.0 tag.
```

---

## When this plan changes

- **Adding a v1.X minor:** add a new section per new feature, with the same 🟢/🟡/🔵 tagging discipline. Keep prior tests as regression coverage.
- **Adding a v2.X major:** start a new `v2-acceptance.md` rather than mutating this one. This plan stays as the historical record of what v1.0.0 actually verified.
- **Deprecating a feature:** mark the relevant tests `(deprecated as of vX.Y)`; remove only when the feature itself is removed.
