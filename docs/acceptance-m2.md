# Acceptance Test Plan — Milestone 2

**Scope:** verify the M2 deliverables of `core-agent`: a second backend for the Anthropic provider that talks to Claude through Google Vertex AI, registered under the provider name `anthropic-vertex`.

**Why M2 exists:** users with established GCP infrastructure can use Claude without managing a separate `ANTHROPIC_API_KEY` — they reuse the same Application Default Credentials, billing, and IAM posture they already have for Vertex Gemini.

**Out of scope for M2** (deferred to M3+, do not test here): Amazon Bedrock as a third Anthropic backend, Claude Platform on AWS, the Anthropic feature surface beyond text + tool-use (extended thinking, structured outputs, server-side tools, vision), and everything from `acceptance-m1.md` Section "Notes for M2 and beyond" not explicitly listed below.

**Audience:** anyone validating an `core-agent` build that claims `anthropic-vertex` support.

**Relationship to M1:** every test in `acceptance-m1.md` continues to apply unchanged. M2 adds new tests; it does not retract or modify existing ones. A release that bundles M1 + M2 (the planned single PR) must pass the union.

---

## How to use this plan

Same conventions as `acceptance-m1.md`:

- 🟢 **automated** — covered by `go test ./...`.
- 🟡 **smoke** — manual but credential-free.
- 🔵 **provider-gated** — requires real GCP credentials with Vertex AI access in a region/project where Anthropic's Claude models are deployed (commonly `us-east5`).

Each test has an ID prefixed `M2-` so it can't collide with M1's `T*` numbering.

A combined M1+M2 release is **ready** when:

1. Every 🟢 and 🟡 test in **both** plans passes.
2. At least one 🔵 test passes for each provider claimed in the release notes — including `anthropic-vertex` if M2 is being shipped.

---

## Section 1 — Build & static checks (regression) 🟢

### M2-T1.1 — `go build ./...` still succeeds

After the M2 changes (new `models/anthropic/vertex.go`, new transitive deps from the SDK's `vertex` subpackage and `golang.org/x/oauth2/google`).

**Steps:** `go build ./...`

**Pass:** exit 0; no missing-go.sum errors.

### M2-T1.2 — `go vet ./...` clean

**Pass:** exit 0.

### M2-T1.3 — `go test ./...` all pass

**Pass:** every package reports `ok`; no `FAIL`. Specifically `models/anthropic` reports `ok` (M1 + M2 tests both green).

---

## Section 2 — Config schema 🟢

### M2-T2.1 — `ProviderAnthropicVertex` validates

**Pass:** `config/discovery_test.go::TestValidate` includes the `anthropic-vertex with creds ok` and `anthropic-vertex without project errors` cases and they pass under `go test ./config/...`.

### M2-T2.2 — `AnthropicConfig.Vertex` round-trips through JSON

**Steps:** create a `config.json` fixture with a `model.anthropic.vertex` block; load via `config.Load`; assert fields populated.

**Pass:** the existing `TestLoad_MergesPartialOverrides` covers this pattern; if M2 wants belt-and-suspenders, add an explicit case for the anthropic-vertex shape. (Optional — current schema reuses `VertexConfig` so the existing test fully exercises the JSON path.)

---

## Section 3 — Anthropic provider unit coverage (M2 additions) 🟢

All in `models/anthropic/vertex_test.go`:

| Test | What it pins |
|---|---|
| `TestNewVertex_RequiresProject` | Empty project errors before reaching the SDK |
| `TestNewVertex_RequiresRegion` | Empty region errors before reaching the SDK |
| `TestResolve_AnthropicVertex_FromConfig` | `models.Resolve` routes `provider: anthropic-vertex` to the Vertex constructor; provider name returned is `anthropic-vertex` (not `anthropic`) |
| `TestResolve_AnthropicVertex_MissingProjectErrors` | With no project in config or env, Resolve surfaces the project-required error |
| `TestNewVertexProvider_HonorsEnvFallbacks` | `ANTHROPIC_VERTEX_PROJECT_ID` + `CLOUD_ML_REGION` are picked up when config is absent |

**Pass:** all five tests green.

> **Note on credential-dependent tests:** `TestResolve_AnthropicVertex_FromConfig` and `TestNewVertexProvider_HonorsEnvFallbacks` skip cleanly if Application Default Credentials aren't available on the test machine. CI without GCP creds is expected to skip rather than fail. Run with `gcloud auth application-default login` or `GOOGLE_APPLICATION_CREDENTIALS` set to convert skips to passes.

---

## Section 4 — CLI surface 🟡

### M2-T4.1 — `--provider` help mentions `anthropic-vertex`

**Steps:** `core-agent -h 2>&1 | grep provider`

**Expected:** the description includes `anthropic-vertex` alongside `gemini`, `vertex`, `anthropic`.

**Pass:** substring match on `anthropic-vertex`.

### M2-T4.2 — `--provider anthropic-vertex` without GCP creds errors clearly

**Preconditions:** `unset GOOGLE_APPLICATION_CREDENTIALS`; no ADC configured (or test machine genuinely has no GCP auth).

**Steps:**

```bash
core-agent --provider anthropic-vertex \
           --model claude-opus-4-7 \
           -p "ping"
```

**Expected:** stderr contains `anthropic-vertex:` and either `project is required` (if no env vars set) or `load default credentials` (if project resolved but creds missing). Exit 2 (`ExitConfigError`).

**Pass:** error message identifies the missing prerequisite + exit 2.

---

## Section 5 — Provider: Anthropic via Vertex 🔵

**Preconditions:**

```bash
gcloud auth application-default login                    # or workload identity
export ANTHROPIC_VERTEX_PROJECT_ID=<gcp-project>          # or GOOGLE_CLOUD_PROJECT
export CLOUD_ML_REGION=us-east5                           # or GOOGLE_CLOUD_LOCATION

# A region where Anthropic's Claude is deployed; us-east5 is the
# most common today, but check the GCP console for current availability.
```

The Vertex model ID may differ from the API model ID. If `claude-opus-4-7` doesn't resolve, try the date-suffixed variant Vertex publishes (e.g. `claude-opus-4-5@20251101`) — see the GCP Vertex Model Garden for the current list.

### M2-T5.1 — One-shot via Vertex

**Steps:**

```bash
core-agent --provider anthropic-vertex \
           --model claude-opus-4-7 \
           -p "Say the word 'pong' and nothing else."
```

**Expected:** stdout contains `pong`; exit 0; stderr summary line names the Claude model.

**Pass:** stdout substring match + exit 0.

### M2-T5.2 — Auto-load from env vars only (no `--model`, no config)

**Preconditions:** preconditions above; no `.agents/config.json` in cwd.

**Steps:** `core-agent --provider anthropic-vertex -p "ping"`

**Expected:** non-empty assistant text; exit 0.

**Pass:** non-empty response.

### M2-T5.3 — REPL multi-turn coherence (Vertex)

**Steps:**

```bash
printf '%s\n' "My favorite color is teal." "Name my favorite color." "/exit" \
  | core-agent --provider anthropic-vertex
```

**Pass:** the second response contains `teal`.

### M2-T5.4 — Streaming arrives incrementally (Vertex)

**Steps:**

```bash
core-agent --provider anthropic-vertex \
           -p "Count from 1 to 10, one number per line." \
           | head -c 1 ; echo
```

**Expected:** first byte returns within ~3 seconds (suggests Vertex streamRawPredict path is wired through and partials flush as they arrive).

**Pass:** subjective speed check.

### M2-T5.5 — Tool round-trip via `examples/with-tools` against Vertex

**Preconditions:** edit `examples/with-tools/main.go` (or copy + tweak) to set `cfg.Model.Provider = config.ProviderAnthropicVertex` and supply a `Vertex` block in `AnthropicConfig`.

**Expected:** stderr shows `→ add` (assistant called the tool); stdout includes `42`.

**Pass:** both observables present.

This is M2's **load-bearing** test — it's the only end-to-end check that the genai → Anthropic conversion (system extraction, tool round-trip, streaming) survives the Vertex middleware path.

### M2-T5.6 — Provider name surfaces correctly in usage summary

**Steps:** any successful M2-T5.x run.

**Expected:** the trailing `core-agent: 1 turn(s) · …` line includes the model name passed via `--model`. Provider name itself isn't currently in the summary; if M3 adds it, update this assertion.

**Pass:** line present, model name correct.

---

## Section 6 — Coexistence with M1 🟡 + 🔵

### M2-T6.1 — `--provider anthropic` still works exactly as in M1

The Vertex addition must not regress the first-party path.

**Preconditions:** `ANTHROPIC_API_KEY` set; GCP creds may or may not be present.

**Steps:** `core-agent --provider anthropic -p "ping"`

**Pass:** every M1 Section 5 test (T5.1–T5.6) still passes.

### M2-T6.2 — Auto-detection precedence is unchanged

`models.Resolve` auto-detection order from M1 is: Vertex Gemini → Gemini API → Anthropic API. M2 deliberately does **not** add `anthropic-vertex` to auto-detection (the env signals are too overlapping with Vertex Gemini to disambiguate safely).

**Steps:** with **all** of `GOOGLE_GENAI_USE_VERTEXAI=true`, `GOOGLE_CLOUD_PROJECT`, `GOOGLE_API_KEY`, and `ANTHROPIC_API_KEY` set, run `core-agent -p ping`.

**Expected:** Vertex Gemini is selected (highest precedence), not anthropic-vertex.

**Pass:** the model in the summary line is a Gemini model, not a Claude one.

### M2-T6.3 — `provider.go` registry contains both Anthropic providers

**Steps:** Trigger an unknown-provider error to dump the registered list.

```bash
core-agent --provider not-real -p ping 2>&1 | grep registered
```

**Expected:** the message lists both `anthropic` and `anthropic-vertex` (alongside `gemini`, `vertex`).

**Pass:** both appear.

---

## Release-readiness gate (combined M1 + M2)

For the bundled PR closing both milestones:

1. ✅ Sections from `acceptance-m1.md` Release Gate items 1–5 all pass.
2. ✅ M2 Section 1 (build/vet/test regression) passes.
3. ✅ M2 Sections 2 + 3 (config + adapter unit coverage) pass.
4. ✅ M2 Section 4 (CLI surface) passes.
5. ✅ M2 Section 5 (🔵 Vertex live tests): at minimum **M2-T5.1, M2-T5.3, and M2-T5.5** pass against a real Vertex AI deployment of Claude. M2-T5.5 is the load-bearing one.
6. ✅ M2 Section 6 (M1 coexistence): all three tests pass — proves the M2 changes didn't regress M1.

If any 🟢 in either plan fails, **block** the release. If 🔵 tests for a claimed provider fail, either fix or downgrade the release notes to remove the claim.

---

## Notes for M3 and beyond

When M3 ships, append an `acceptance-m3.md` rather than editing this file. Keep this document as an honest record of what M2 was held to.

The most likely M3 scope additions, each with its own future acceptance plan: Amazon Bedrock as a third Anthropic backend (mirrors the M2 pattern almost exactly — same conversion code, different client construction), Anthropic-operated Claude Platform on AWS (SigV4-authed), subagents, file-backed session service, slash-command framework, and broader Anthropic feature coverage (extended thinking, structured outputs, server-side tools, vision).
