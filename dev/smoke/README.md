# dev/smoke/

Live smoke tests against real LLM providers. **NOT wired into
presubmits** — they cost money and need credentials. Run manually
after touching the provider surface, before tagging a release.

The v1.0.0 Vertex regression slipped through because we didn't have
a Vertex smoke that ran before tagging. These scripts are the
remediation.

## Setup

Keep your credentials in a personal script outside the repo and
source it before running. Suggested shape — never commit this file:

```bash
# ~/scripts/gemini.sh (your file, never in this repo)
export GEMINI_API_KEY=AQ.Ab8...                     # for 01-gemini-basic
export GOOGLE_CLOUD_PROJECT=my-gcp-project           # for 02 / 03 / 04 / 05
export GOOGLE_CLOUD_LOCATION=global                  # optional, default global
# Plus: gcloud auth application-default login        (for Vertex ADC)
```

```bash
source ~/scripts/gemini.sh
dev/smoke/run-all.sh
```

Each script self-skips (exit 77) when its required env vars are
missing, so a partial-credential setup doesn't punish you.

## Scripts

| Script | What it verifies | Required env |
|---|---|---|
| `01-gemini-basic.sh` | Direct Gemini API auth + basic single turn | `GEMINI_API_KEY` or `GOOGLE_API_KEY` |
| `02-vertex-basic.sh` | Vertex single turn — would catch v1.0.0-style `IncludeServerSideToolInvocations` regressions | `GOOGLE_CLOUD_PROJECT` + ADC |
| `03-vertex-grounding.sh` | Vertex + `GoogleSearch` + `--session-db`: `↪ google_search:` lines in stdout + `gemini/google_search`-authored rows in eventlog | `GOOGLE_CLOUD_PROJECT` + ADC |
| `04-background-spawn.sh` | Dynamic background subagents end-to-end (parent spawns two, both complete, `check_agent` returns terminal status) | `GOOGLE_CLOUD_PROJECT` + ADC |
| `05-headless-gate.sh` | Headless (no TTY) without `--yolo` produces the helpful `ErrNoPrompter` message pointing at `--yolo` and config | `GOOGLE_CLOUD_PROJECT` + ADC |
| `06-inject-autonomous.sh` | `examples/autonomous-handle` runs end-to-end; verifies `StartAutonomous` → `Pause` → `Inject` → `Resume` → `Wait` lifecycle (v1.3.0) | none (uses echo mock) |
| `07-mcp-google-oauth.sh` | Google OAuth (ADC access-token) wiring for remote MCP HTTP servers — uses the GKE remote MCP server as the real round-trip target | `GEMINI_API_KEY`/`GOOGLE_API_KEY` + `MCP_GOOGLE_OAUTH_SMOKE_PROJECT` + ADC |
| `08-scheduled-monitor-gke.sh` | scheduled-monitoring end-to-end against a real GKE cluster: spawns the supervisor + a sandbox deployment, watches the child monitor run multiple wake cycles, optionally injects a scale anomaly to exercise the alert flow. Required flags: `--context`, `--namespace`. Optional: `--ksa`/`--gsa` for Workload Identity, `--anomaly`, `--duration`, `--no-deploy`, `--keep` | `GOOGLE_CLOUD_PROJECT` + ADC + a kubectl context for the target GKE cluster |
| `09-scion-research-orchestrator.sh` | `examples/scion-research-demo` orchestrator binary boots, drives an in-process subagent via `spawn_agent`, and gracefully refuses `spawn_remote_agent` when Scion env is unset | `GOOGLE_CLOUD_PROJECT` + ADC |

## Exit codes

- `0` — passed
- `1` — failed (assertion mismatch or process error)
- `77` — skipped (required env vars not set; autotools convention)

`run-all.sh` aggregates: exits 0 if every script that ran passed
(skipped scripts don't count as failures), 1 if any failed.

## Adding new scripts

1. Copy a similar script as a starting point — keep the numeric
   prefix convention so `run-all.sh` picks them up in order.
2. Source `_common.sh` and use its helpers: `require_env`,
   `require_one_of`, `assert_contains`, `assert_not_contains`,
   `pass`, `fail`, `skip`.
3. **Use loose assertions on model output.** Match `"spawn_agent"` to
   prove the tool was called, not the exact arguments the model
   chose. LLM output varies; exact-text matches will flake.
4. Document the new script in the table above.

## Tradeoffs worth knowing

- **LLM output is non-deterministic.** A flaky run is part of the
  protocol — re-run it. If a single script flakes consistently,
  loosen its assertions or split it into smaller scenarios.
- **Vertex streaming heartbeats are intermittent.** The v1.0.1
  empty-response tolerance fix is what keeps the grounding test
  consistent; if it regresses, this test will go red about 30–60% of
  the time. Treat sustained failure as a real signal.
- **Cost.** Each script runs one or two turns against Vertex /
  Gemini. Full suite is under $0.05 per run.
- **No deterministic alternative.** The reason these tests exist is
  to catch real-API behaviors (request shape rejections, streaming
  quirks, model-driven tool dispatch). A fully mocked suite defeats
  the purpose.

## Inspector helper

`03-vertex-grounding.sh` uses a small Go inspector
(`dev/smoke/cmd/inspect-grounding/main.go`) to query the eventlog
SQLite database without depending on the `sqlite3` CLI being on
PATH. The inspector lives in the same dev tree so it tracks the
eventlog API as it evolves.
