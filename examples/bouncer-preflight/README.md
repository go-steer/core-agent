# `bouncer-preflight` — porting a Python ADK multi-agent system onto core-agent as a library

[gke-demos/bouncer](https://github.com/gke-demos/bouncer) mines **completed GKE
TPU/JAX production workloads** and derives a verified single-slice "preflight"
smoke test from each one: same image, same framework, same ICI topology, one
slice, one step, synthetic data. The point is to catch "this cluster can't
actually run your workload" before a 256-chip job is queued.

Upstream is `google-adk` (Python): one agent per directory, launched as separate
`adk run` subprocesses, agreeing on state through environment variables and on
the verdict through a **stdout grep for `"success: True"`**.

This example ports **the generator + checker pair** — the load-bearing half — to
Go, with `github.com/go-steer/core-agent/v2` as an ordinary library dependency.

**The prompts are copied from upstream unedited.** Everything under `prompts/`
is the original text with its original placeholders. If the port were unfaithful,
the prompts would be the first thing to bend — so they are the fixed point, and
the Go code is what moves.

## Why a library port, not a config recipe

core-agent's config/recipe path (see `examples/gke-parallel-triage`) covers
"give an agent a persona, some MCP tools, and a permissions policy". bouncer
needs four things a recipe cannot express:

| bouncer needs | recipe | library |
|---|---|---|
| A **jail** for model-authored shell (`bwrap` + `sudo -u agent-runner`) | ✗ core-agent's built-in `bash` runs as the daemon's own uid | ✓ don't register `bash`; register the jail as the only shell |
| **Structured output** from the checker (`output_schema=CheckerResult`) | ✗ no such knob | ✓ invert it: the terminal *action* is a typed tool |
| An **agent calling an agent synchronously** and reading its typed result | ~ `spawn_agent` returns prose | ✓ a plain Go function call |
| A **model decorator** (the upstream `BaseApiClient.async_request` monkeypatch) | ✗ | ✓ `adkmodel.LLM` is a two-method interface |

The whole port is ~1,000 lines of Go against `pkg/` only — no `internal/`
imports, so the surface it exercises is the real external API.

## Run it hermetically

No cluster, no credentials, no cost: two scripted transcripts stand in for the
models and a fake `kubectl` stands in for the cluster.

```bash
PATH="$PWD/examples/bouncer-preflight/testdata/bin:$PATH" \
go run ./examples/bouncer-preflight \
  --source examples/bouncer-preflight/testdata/prod-jobset.yaml \
  --generator-script examples/bouncer-preflight/testdata/generator.jsonl \
  --checker-script examples/bouncer-preflight/testdata/checker.jsonl \
  --sandbox none \
  --state-dir /tmp/bouncer-preflight
```

You should see the hand-off and the verdict on stderr, then:

```
stop reason: completed
turns:       1
library:     maxtext-v5e-256-16x16
```

with `/tmp/bouncer-preflight/library/maxtext-v5e-256-16x16.yaml` (the manifest
that was actually verified) next to its `.json` provenance sidecar.

`go test ./examples/bouncer-preflight/` runs the same path as a test, plus the
jail, retry, slug and fail-closed unit tests.

## Run it live

```bash
GOOGLE_API_KEY=... go run ./examples/bouncer-preflight \
  --source /path/to/completed-jobset.yaml \
  --namespace test-preflight \
  --service-account preflight-sa \
  --policy-file /path/to/cluster-policy.md \
  --max-turns 40 --max-cost 25
```

Prerequisites, all of them upstream's too:

- **`bubblewrap`** on `PATH`. This is checked at boot and the run **fails closed**
  if it is missing — `--sandbox=none` is the only way to run uncontained, and it
  prints a warning to stderr when you do.
- **`sudo`** configured for a passwordless drop to an unprivileged **`agent-runner`**
  uid (`sudo -n -E -u agent-runner`).
- **`kubectl`** (plus `jq`/`yq` if your policy bullets mention them) inside the jail,
  and cluster credentials the jail can see — in-cluster that is the projected
  service-account token at `/var/run/secrets/kubernetes.io/serviceaccount`, which
  is bind-mounted read-only when present.
- A **test namespace** the agents may freely create and delete Jobs in, with real
  TPU node pools behind it. The preflights are only meaningful on hardware.

Both agents share one credentialled provider (`--provider gemini|vertex`,
`--model`, default `gemini-3.1-pro-preview` — bouncer's `DEFAULT_MODEL`).

## What maps to what

```
examples/bouncer-preflight/
├── main.go        # flags, prompt rendering, event log, autonomous.Run
├── generator.go   # parent agent + save_if_validated (the hand-off)
├── checker.go     # checker agent + report_verdict + submit_candidate_preflight
├── retry.go       # adkmodel.LLM decorator (the monkeypatch)
├── exec.go        # bwrap-backed sandbox_run_command + wait_seconds
├── store.go       # scratch dir, preflight library, experience log
├── prompts/       # VERBATIM from upstream
└── testdata/      # scripted transcripts + fake kubectl + a real-shaped JobSet
```

The five plumbing changes, and why each one is a change rather than a
translation:

1. **`output_schema=CheckerResult` → the `report_verdict` tool** (`checker.go`).
   core-agent has no output-schema knob. Instead of constraining the final
   *message*, the terminal *action* becomes a Go function whose signature is the
   schema: `report_verdict(success bool, details string)`. The model cannot
   report an outcome in a shape the compiler didn't approve. The fail-closed
   behaviour is preserved exactly — a checker that stops without calling it is a
   FAILURE, just as a missing `"success: True"` in the upstream stdout was.
   First verdict wins, so a second call can't flip a FAILURE to a SUCCESS.

2. **`subprocess.Popen(["adk","run","checker"])` → an in-process call**
   (`generator.go`). `save_if_validated` writes the candidate and calls
   `runChecker` directly, getting a typed `verdict` back. No second interpreter,
   no environment-variable handshake, no stdout parsing. A transport failure
   inside the checker surfaces as a *tool error*, not as "your manifest is
   broken" — a distinction the stdout grep could not make.

3. **`BaseApiClient.async_request` monkeypatch → an `adkmodel.LLM` decorator**
   (`retry.go`). ADK's model interface is two methods, so retry is an ordinary
   wrapper: 10 attempts, 60s backoff, retryable-only (429/503/RESOURCE_EXHAUSTED/
   UNAVAILABLE/DEADLINE_EXCEEDED/connection reset/EOF), with a per-attempt
   deadline the Python version doesn't have. One deliberate semantic difference:
   once a response chunk has reached the consumer, a mid-stream error is
   surfaced rather than retried, because replaying would duplicate it in the
   transcript.

4. **`sandbox_run_command` ported as-is, as the *only* shell** (`exec.go`).
   The bwrap flags are upstream's: `--unshare-{user,pid,ipc,cgroup}`, read-only
   `/`, private `/proc`, `/dev`, `/tmp`, and exactly one writable bind
   (`/workspace`). core-agent's built-in `bash` tool is never registered, so
   there is no uncontained shell for the model to reach. `TestSandboxArgvIsAJail`
   asserts every flag and the bind ordering: the jail is the security boundary,
   so a silently dropped flag must fail a test rather than a pentest.

5. **One-shot `adk run` → `autonomous.Run`** (`main.go`). The generator is a
   goal-driven loop with turn, wallclock and cost budgets, `report_done` injected
   as the terminal tool, and both agents' events in one SQLite event log (the
   checker under a derived `<session>:checker` id), so one audit query returns
   both halves of a derivation.

### The tools both agents get

Derived from the prompts, and enforced by `TestPromptsNameOnlyToolsWeRegister`:
every backticked `snake_case` name in `prompts/` must resolve to a tool the port
actually registers, or the test fails. Copying in a newer upstream prompt breaks
the build, not a live run.

| Tool | Agent | Notes |
|---|---|---|
| `sandbox_run_command` | both | the bwrap jail; non-zero exits are data, not errors |
| `wait_seconds` | both | capped at 900s/call so a model can't sleep for a day |
| `read_source_manifest` | generator | the completed production workload |
| `get_original_objective` | generator | verbatim task recall |
| `get_conversation_history` | generator | every hand-off + verdict so far |
| `bouncer_docs_retriever` | generator | substring grep over library + experience log, exactly as upstream |
| `reuse_existing_preflight` | generator | the "don't re-derive" fast path |
| `append_experience_log` | generator | durable lessons, grouped by topic |
| `save_if_validated` | generator | **the hand-off**; runs the checker in-process |
| `read_candidate_manifest` | checker | |
| `submit_candidate_preflight` | checker | takes **no arguments** — always applies the saved manifest into the configured namespace, so the model can't smuggle in a different one |
| `save_derived_preflight_to_library` | checker | takes **no manifest** — reads it from disk, so what's stored is what was tested |
| `report_verdict` | checker | the structured-output replacement |
| `report_done` | generator | injected by `autonomous.Run` |

Three places where the port is deliberately *stricter* than upstream, all
because a model authors the inputs:

- **Library filenames** are slugified (`slugify` in `store.go`), so a preflight
  named `../../etc/cron.d/pwn` cannot escape the library directory.
- **`checker-instruction:` lines** the generator writes to itself are stripped
  before the candidate is written, so they can never reach an API server or a
  stored artifact.
- **Namespace and manifest** for the submit are fixed by the operator's flags,
  not by tool arguments.

## What is not ported

This is a spike of the derivation core, not of bouncer. Left in Python (or left
out entirely):

- **`bouncer.py`** — the outer poller that watches for completed production jobs
  and starts a derivation per job. core-agent's counterpart would be
  `autonomous.Run` + a `Scheduler` (see `examples/scheduled-monitor`), but the
  trigger here is a `--source` file you point at.
- **The Dreamer, Lookup and Monitor agents** — the "what should we test next",
  reference-lookup and live-monitoring halves of upstream.
- **The Google Chat bridge** and the git sync of the preflight library. The
  library here is a directory of YAML + JSON sidecars; wiring it to a repo is
  your `git` invocation, not this example's.
- **The iptables egress lockdown** upstream applies to its sandbox pod. The
  bwrap jail is ported; the network policy around the pod is deployment config.
- **`get_conversation_history` is re-scoped**: upstream replays the ADK session,
  this returns the generator↔checker hand-off history (candidate names, sizes and
  verdicts), which is the part its prompt actually asks for. core-agent already
  keeps the live transcript in the model's context; this survives compaction.

## Composing further

- **Budgets**: `--max-turns`, `--max-wallclock`, `--max-cost`. A derivation that
  can't converge stops costing money.
- **Audit**: `--state-dir <dir>` puts `eventlog.db` next to the library; every
  tool call from both agents is queryable after the fact.
- **Cheaper checker**: the checker does far less reasoning than the generator.
  Splitting `resolveModels` to give it a Flash-class model is a two-line change
  and the obvious first cost lever.
- **Policy**: `--policy-file` splices operator bullets into the generator prompt
  at upstream's `{{CLUSTER_POLICY_RULES}}` placeholder — the supported way to add
  cluster rules without editing the vendored prompt.

## Provenance

`prompts/generator_prompt.md` and `prompts/checker_prompt.md` are copied from
[gke-demos/bouncer](https://github.com/gke-demos/bouncer) (Apache 2.0) with only
the placeholder substitution the upstream code performs at agent-construction
time. `testdata/prod-jobset.yaml` is a synthetic MaxText-on-v5e-256 JobSet
written for this example, not a captured customer workload.
