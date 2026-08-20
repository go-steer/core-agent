---
title: Built-in tools
---

The model-facing tool catalog `core-agent` registers by default, plus the optional lifecycle tools the runtime wires when the corresponding feature is enabled (checkpoints, ask, autonomous scheduling). For declaring third-party tools, see [MCP servers](/concepts/mcp/). For writing your own tools from Go, see [Library API](/embed/api/).

## The built-in catalog

Tools are grouped by domain — files, search, shell, data + network, planning, and interactive prompting. Each is configurable via the `BuiltinTools` struct in `pkg/tools` (library callers) or the `--disable-tools` flag / `tools.disable` config field (CLI users). Every call routes through the [permission gate](/concepts/permissions/) under the `tool` namespace — denying a tool by pattern keeps it from running even if it's registered. Three tools are conditionally registered: `fetch_url` only when `url_scope.allow` has at least one entry, `record_plan` only when [`permissions.plan_mode`](/reference/configuration/#plan-mode-v29--plan_mode) is `advisory` or `required`, and `sciontool_status` only when the `sciontool` binary is on `PATH`.

### File system

| Tool | Purpose | Key parameters |
|---|---|---|
| `read_file` | Read a file with optional `offset` / `limit` for large files. | `path`, `offset?`, `limit?` |
| `read_many_files` | Read a batch in one call. Per-file failures surface as `skipped: "<reason>"` entries — the batch never aborts. **Preferred over parallel `read_file` calls when the file set is known up front.** | `paths?`, `pattern?`, `path?` |
| `write_file` | Atomic create-or-overwrite. Asks for confirmation in `ask` mode. | `path`, `content` |
| `edit_file` | Replace exactly one occurrence of `old_string` with `new_string`. Fails if the string appears zero or multiple times. | `path`, `old_string`, `new_string` |
| `delete_file` | Idempotent removal of a regular file. Refuses directories. **Preferred over `bash rm`** — honors the gate's `CheckFileWrite` and the path scope. | `path` |
| `stat` | Metadata: `size`, `mtime` (RFC3339 UTC), `mode`, `is_dir`. Missing path returns `{exists: false}` instead of erroring — use for "has this been written yet?" without exception handling. | `path` |
| `list_dir` | Sorted directory listing. | `path` |

### Search

| Tool | Purpose | Key parameters |
|---|---|---|
| `glob` | Walk `path` (default `.`) and return file paths whose basename matches a `filepath.Match` pattern (e.g. `*.go`). Skips hidden + vendored directories. | `pattern`, `path?` |
| `grep` | Walk `path` and return matching lines for an RE2 regex. Recursive on directories; single-file mode for files. Returns structured `{path, line, text}` matches the model can pipe into follow-up tool calls without re-parsing. **Preferred over `bash grep` / `bash rg` / `bash find` for code search** — and since v2.9 that preference is enforced by [the bash search gate](#the-bash-search-gate) rather than left to the model. | `pattern`, `path?` |

### Data + network

| Tool | Purpose | Key parameters |
|---|---|---|
| `json_query` | Run a jq expression against JSON loaded from a file or supplied inline. | `expression`, `path?` or `data?` |
| `fetch_url` | HTTP GET against an operator-configured allowlist. **Default-deny**: not registered at all when `cfg.URLScope.Allow` is empty, so the model never sees a tool that would refuse every call. Built-in SSRF guard: link-local/cloud-metadata IPs are always blocked, loopback/private ranges require an exact-host allowlist entry, and resolved IPs are pinned through to the dial (DNS-rebinding defense) — see [`url_scope`](/reference/configuration/#url_scope). | `url` |
| `alert` | POST to an operator-registered webhook target — escalation, incident summaries, "I'm stuck" pings — without a shell or a separate MCP server. **SSRF-impossible by construction**: no URL parameter; the model picks a target by *name*. Same default-deny shape as `fetch_url`, and targets whose env-supplied URL or token is unset are [dropped at startup](/reference/configuration/#undeliverable-targets-are-dropped-at-startup) rather than advertised. See [`alerts`](/reference/configuration/#alerts). | `target`, `level`, `summary`, `details?` |

### Shell

| Tool | Purpose | Key parameters |
|---|---|---|
| `bash` | `/bin/sh -c` with a per-call timeout and a denylist of dangerous commands. **Use only for actions the structured tools can't perform**: builds, tests, git, formatters, package managers. The descriptions of `read_file`, `grep`, `glob`, `list_dir`, `stat`, `delete_file` all instruct the model to prefer them over the corresponding `bash` invocation — and for search specifically that preference is *enforced*, not requested: see [the bash search gate](#the-bash-search-gate). | `command`, `timeout?` |

### Planning

| Tool | Purpose | Key parameters |
|---|---|---|
| `todo` | In-process plan tracker. Actions: `list`, `add`, `set_status`, `clear`. Underlying `TodoStore` is exposed via `Registry.Todo` so a TUI can render plan progress (the in-process TUI's `/todo` slash command uses this). | `action`, `id?`, `text?`, `status?` |
| `record_plan` | Writes the turn's plan to `.agents/plans/plan-<seq>.md` for the operator's audit trail. Registered only under [`plan_mode`](/reference/configuration/#plan-mode-v29--plan_mode) `advisory` or `required`; under `required` it also satisfies the gate's plan pre-check. Its **description is mode-aware** — under `required` it tells the model that mutating calls are denied until the plan is on file; under `advisory` it says the opposite, so the model records and proceeds instead of stalling for an approval nobody will send. Since v2.9 its **result** is mode-aware too, and names the tools this build actually gates — see below. | `plan` |

### Verification

| Tool | Purpose | Key parameters |
|---|---|---|
| `wait_and_verify` | Poll another tool until its result satisfies a condition, or a bounded budget expires. Closed-loop fix-and-verify without a shell — see below. | `tool`, `args_json?`, `expect_jq?` / `expect_contains?` / `expect_not_contains?`, `interval_seconds?`, `timeout_seconds?`, `max_attempts?` |

### Delegation

| Tool | Purpose | Key parameters |
|---|---|---|
| `call_peer` | Hand a self-contained prompt to another core-agent daemon registered with this hub's [peer registry](/reference/attach-http/#peer--hub-endpoints), and return its answer. **Off by default**, and registered only on a peer hub — see below. | `peer`, `prompt` |

### Runtime integration

| Tool | Purpose | Key parameters |
|---|---|---|
| `sciontool_status` | Signal a sticky lifecycle event (`ask_user`, `blocked`, `task_completed`, `limits_exceeded`) to a Scion hub. Registered only when the `sciontool` binary is on `PATH` — outside a Scion container the tool is hidden from the model rather than exposed as a subprocess no-op. See [Scion adapter](/reference/scion-adapter/). | `status_type`, `message` |

## `wait_and_verify` (v2.9+) — bounded poll-until-condition

Fix-and-verify needs a wait in the middle: apply the change, let the system converge, confirm the new state. Doing that with `bash sleep` needs a shell the distroless recipes deliberately don't have, and doing it by re-checking across turns costs a full prompt per look — so agents skipped it and asserted success instead. `wait_and_verify` collapses the whole loop into one tool call and one tool result:

```text
wait_and_verify(
  tool:             "gke_get_pod",
  args_json:        "{\"namespace\": \"prod\", \"name\": \"api-7d9f\"}",
  expect_jq:        ".status.phase == \"Running\"",
  interval_seconds: 15,
  timeout_seconds:  180
)
```

```json
{
  "verified": true, "outcome": "verified", "attempts": 5,
  "interval_seconds": 15, "elapsed_seconds": 61.2,
  "condition": "expect_jq=.status.phase == \"Running\"",
  "observations": [{"attempt": 1, "at_seconds": 0, "matched": false}, "..."]
}
```

Three properties are enforced by the runtime rather than asked for in a prompt:

- **No shell.** Pure Go, so it works wherever the binary does — including `distroless/static-debian12:nonroot` with `tools.disable: ["bash"]`.
- **Bounded.** Wall clock and attempt count both capped, with operator ceilings ([`tools.wait_and_verify`](/reference/configuration/#toolswait_and_verify-v29)) the model can't raise; a request past the ceiling is an error, not a silent clamp. Token cost is bounded by construction: N polls, one result.
- **Read-only by construction.** It refuses to poll anything not classified read-only — a loop that could call `write_file` sixty times is an amplifier, not a verifier. `wait_and_verify` itself and `ask_user` are refused unconditionally. MCP tools need an explicit `poll_allow` entry, because ADK's MCP adapter doesn't surface the server's `readOnlyHint`.

An unverified wait is **not** an error: it returns `verified: false` with `outcome` of `timeout` / `attempts_exhausted` / `canceled` plus the observation trail, because "it never became Ready in three minutes" is a finding the model should report rather than a failure it should retry. A poll that errors is treated as transient and retried; a malformed `expect_jq` aborts on the first attempt instead of burning the budget.

For waits longer than a turn is worth, use [`schedule_next_turn`](#schedule_next_turn) to come back later and `wait_and_verify` to confirm cheaply on arrival. Full rationale: [`docs/wait-and-verify-design.md`](https://github.com/go-steer/core-agent/blob/main/docs/wait-and-verify-design.md).

## `call_peer` (v2.9+) — named delegation to a peer agent

A fleet of daemons that can see each other but can't talk to each other isn't a fleet. `POST /peers` and `GET /peers` have made the roster visible since v2.7; `call_peer` ([#595](https://github.com/go-steer/core-agent/issues/595)) is the turn-level counterpart — one tool that lets a model ask a named peer a question and use the answer in the same turn.

```text
call_peer(
  peer:   "operator-prod-1",
  prompt: "How many nodes are Ready in the prod-1 node pool right now?"
)
```

```json
{
  "peer": "operator-prod-1", "session_id": "s-8f21",
  "response": "prod-1 has 12 nodes, all Ready.", "duration_ms": 4180
}
```

Enable it under [`tools.call_peer`](/reference/configuration/#toolscall_peer-v29). It requires attach mode plus `--attach-peer-hub`; enabling it anywhere else is a startup error, not a tool that registers and then fails every call.

The safety properties are structural, not prompted:

- **The model can't name a destination.** The arguments are a peer *name* and a prompt — there is no URL parameter to inject into. Names resolve against the live peer registry only, so an untrusted instruction to call a cloud-metadata endpoint resolves to nothing; the error just lists the registered peers. Same shape as the [`alert`](/reference/configuration/#alerts) tool's named channels.
- **No credential is model-visible.** The bearer token comes from `token_env` in the daemon's environment and never enters the schema, the arguments, or the transcript. Configured-but-unset fails the call instead of going out anonymously.
- **Every call is bounded.** One operator-set deadline covering session creation through turn end, and one response-byte cap after which the tool stops reading and flags `truncated`. A peer that streams forever costs the caller its timeout, not its context window.
- **Gated per peer.** The permission key is `call_peer:<peer-name>`, so `--deny call_peer:prod-*` works like any other tool pattern, and renaming the tool moves the key with it. Declarative subagents draw from the parent's already-gated catalog, so one parent-level policy hardens both.

Each call opens a **fresh session** on the peer, so concurrent callers can't interleave prompts into one transcript — which makes `attach.multi_session.enabled` a requirement on the *peer* (a peer without it returns 501, and the tool tells you so). The delegated turn stays in the peer's own event log under the returned `session_id`; the caller gets the answer, and the audit trail lives where the work happened. Wire-level detail: [attach HTTP → Calling a peer from the model](/reference/attach-http/#calling-a-peer-from-the-model-call_peer).

## `record_plan` — what the result says, and who wrote the artifact

`record_plan` is the plan-first escape valve: it writes the turn's plan to `.agents/plans/plan-<seq>.md` and, under `plan_mode: required`, flips the flag the gate checks. Three things about it changed in v2.9 ([#747](https://github.com/go-steer/core-agent/issues/747)), all found in a live GKE run where the recipe had `bash`, `write_file`, `edit_file` and `delete_file` disabled and reached the cluster through an `gke` MCP server instead.

**The result names the tools this build actually gates.** The old message said "mutating tools are now unblocked for this session" unconditionally. In that run it was wrong twice: none of the tools it implied were registered at all, and the surface the plan really unblocked — the whole `gke` MCP namespace, which is deliberately *not* plan-exempt — went unmentioned. The message now reports the runtime instead of a category:

| Situation | What the model is told |
| --- | --- |
| `plan_mode: required`, gated set known | `Now unblocked for this session: gke, fetch_url.` |
| `plan_mode: required`, nothing gated | `…this build registered no plan-gated tools — nothing was blocked and nothing is unblocked; the artifact is the only effect.` |
| `plan_mode: required`, no host ever reported a catalog | `…the tool calls it was denying are now unblocked for this session.` — declines to enumerate rather than claim an empty set |
| `plan_mode: advisory` | `…no tool call was ever blocked on this plan and none becomes callable because of it — carry the plan out in this turn rather than waiting for approval.` |

The set is reported by whoever registers tools — the built-in catalog, each namespaced toolset (`mcp`, and `skill` which the gate drops as exempt), and `call_peer` under its operator-chosen name. Library callers that wire tools by hand register nothing and get the third row: **unknown is not the same as none**, the same three-state contract the [bash search gate](#the-bash-search-gate) uses.

**Every artifact says who wrote it.** Plans now open with a small YAML block:

```yaml
---
plan: 2
agent: "cluster"
session: "s-8f21"
---
```

A parent and its [declarative subagent](/agent-design/subagents-and-wrappers/) write into one `.agents/plans/` directory and share one sequence, and in [multi-session](/concepts/multi-session/) mode the gate flag is per-session while the directory is process-global — so before this, concurrent tenants produced a pile of anonymous markdown. Keys are omitted rather than emitted empty, so "no attribution recorded" is distinguishable from "recorded as empty".

**`/replan` archives the operator's own plan.** The [`/replan`](/run/interactive/slash-reference/#permissions) command used to archive whichever artifact had the highest sequence number, which after a subagent recorded plan-2 meant the operator revoked the subagent's plan and left the parent's in place. It is now scoped to the agent and session it was issued from, falling back to the newest artifact when no plan carries attribution (pre-v2.9 directories). If the newest plan belongs to someone else, the command says so and leaves it alone — and the gate flag clears either way, so the safety contract holds even when there was nothing to file. Sessions created by a [multi-session](/concepts/multi-session/) daemon get the same command against their own sub-gate; before v2.9 those sessions answered `501` and a recipe running `plan_mode: required` under the hub could arm the gate with no way to revoke it.

Full rationale: [`docs/plan-first-design.md`](https://github.com/go-steer/core-agent/blob/main/docs/plan-first-design.md).

## Toggling individual tools

CLI:

```bash
core-agent --disable-tools bash,delete_file
```

Library:

```go
b := tools.Default()
b.Disable("bash")     // by canonical name; errors on typos so config typos fail loudly
b.WriteFile = false   // or set the field directly
reg, err := tools.Build(cfg, gate, b)
```

`tools.BuiltinToolNames()` returns the canonical list in struct order — useful for `--help` generation and config validation.

### Descriptions follow the catalog

Disabling a tool also rewrites what the *remaining* tools tell the model about themselves. Descriptions cross-reference each other — `read_file` is "PREFERRED over `bash cat`", `record_plan` in [required mode](/reference/configuration/#plan-mode-v29--plan_mode) says "call this BEFORE any `write_file` / `edit_file` / `bash` call" — and a sentence naming a tool that isn't registered is worse than no sentence: it asserts a capability the model doesn't have, and the model spends turns discovering that. On a distroless deploy with `tools.disable: ["bash"]` every shell cross-reference drops out, and `bash`'s own redirect names only the structured tools that were actually built.

The rule applies to the built-in catalog only. Tools wired outside `tools.Build` — `spawn_agent`, MCP tools — are unknown rather than absent, so references to them are always kept.

`spawn_agent` follows the same rule from the other side: its `tools` parameter used to carry a fixed example (`… glob, grep, bash, todo …`), and now lists the names the manager's catalog will actually resolve — built-ins, MCP tools, and skills alike. A name the model reads there is a name the spawn will accept.

`alert` applies the rule *inside* a tool: a configured target whose `url_env` isn't set in this process can never deliver, so it is dropped from the target list the description enumerates, and a build where no target survives registers no `alert` tool at all. See [Undeliverable targets are dropped at startup](/reference/configuration/#undeliverable-targets-are-dropped-at-startup).

## Output truncation

Every tool's output is capped per-call by `cfg.MaxBytes` and `cfg.MaxLines` (see [Configuration](/reference/configuration/)). When a result hits the cap, the response includes a `truncated: true` flag and the model sees only the head — preventing a single oversize `grep` or `bash` output from blowing the context window.

For repeated large-output operations, the [agentic wrappers](/concepts/context-management/) (below) are a stronger answer: they route the bulk output through a cheaper model and return only a digest.

## Permission gating

Every tool call passes through the gate under the `tool` namespace:

```
tool.bash               # the bash tool itself
tool.bash.cmd:rm        # the bash tool, scoped to commands starting with rm
tool.read_file          # the read_file tool
tool.fetch_url          # the fetch_url tool
```

See [Permissions](/concepts/permissions/) for the full pattern grammar, gate modes (`ask` / `accept-edits` / `plan` / `yolo`), and per-call vs. session-scoped grants.

The gate also enforces two cross-cutting scopes — both default-deny, both configured under `cfg`:

- **`path_scope.allow` / `deny`** — restricts which paths every file tool (`read_file`, `write_file`, `edit_file`, `delete_file`, `list_dir`, `glob`, `grep`, `stat`, `read_many_files`) can touch. Default: project root only.
- **`url_scope.allow`** — restricts which URLs `fetch_url` can reach. Default: empty allowlist means `fetch_url` is not registered at all.

## The bash search gate

Tool descriptions tell the model to prefer `grep` / `glob` over the shell. Measured against real sessions, that instruction lost 15 times out of 27: the model reached for `grep -rn` anyway, got a wall of untruncated text or a non-zero exit, and kept going. One session opened with three bash-as-grep calls in its first four and ran 164 turns and $5.41 without producing a diagnosis. Since v2.9 ([#158](https://github.com/go-steer/core-agent/issues/158)) the preference is enforced rather than requested.

By default, a `bash` call whose command is **search-shaped** is refused with an error naming the tool to use instead:

```text
bash refused: `grep` is a search-shaped command; use the native `grep` tool
instead. It returns structured {path, line, text} matches, honors the
permission gate and the path scope, and applies per-tool output caps. Piping
into a search binary is unaffected — `go test ./... | grep -v ok` filters a
stream, which the native tool does not do. Operator override:
safety.bash_search_gate = "warn" or "allow" (CLI: --bash-search-gate).
```

Search-shaped means the command's verb is `grep`, `egrep`, `fgrep`, `rgrep`, `rg`, `ag`, or `ack` (→ the `grep` tool), or `find` or `fd` (→ the `glob` tool), including via an absolute path like `/usr/bin/grep`. The gate looks through `;`, `&&`, `||`, subshells, and blocks, so `make build && grep -rn TODO .` is refused on the right-hand side.

Two shapes are deliberately **not** gated, because the native tools genuinely can't replace them:

- **Piped searches.** In `go test ./... | grep -v ok` the grep filters a stream, not a file tree. Refusing it would refuse work that has no alternative — and a gate that blocks legitimate commands gets turned off, which is worse than not having one.
- **`find` with an action predicate.** `find . -name '*.tmp' -delete` is a file operation; `glob` only lists. The carve-out uses the same predicate list (`-exec`, `-delete`, `-fprintf`, …) the permission gate already uses to decide that a `find` needs approval.

| `safety.bash_search_gate` | Behavior |
|---|---|
| `enforce` *(default)* | Refuse the call. The model gets the error above and, in practice, reissues as a `grep` tool call. |
| `warn` | Run the command, and attach the same advice to the result as a `notice` field. Use when you want the telemetry without changing behavior. |
| `allow` | Disable the check entirely. |

CLI: `--bash-search-gate=enforce|warn|allow`, which overrides the config field.

The gate is **steering, not security**. It reads the command with the same shell parser as the [safe-command](/concepts/permissions/) check and fails *open* on anything it can't resolve literally — `$TOOL -rn foo .`, `eval`, `sh -c '...'` all pass through. Anyone trying to evade it can; the point is to stop a model from taking the wrong path by habit, and a false refusal on a real build command costs more than a missed nudge. For an actual boundary, drop `bash` from the catalog with `tools.disable`.

The check runs before the permission mode is consulted, so `--yolo` does not wave it through — the posture an operator configured shouldn't depend on how permissive the session is. It's also inherited by every session a daemon derives, rather than re-defaulted per session.

The blunt alternative is to not register `bash` at all, which is what the investigation-shaped [task classes](/concepts/context-management/#tools-and-plan-first-since-v29) do: `--task=debug|research|review` drops it. The two compose — `--task=debug --enable-tools=bash` puts the shell back and the gate still refuses `bash grep`.

It refuses only what it can redirect. A recipe that puts `grep` in `tools.disable` and keeps `bash` gets no refusal for `bash grep`, because a refusal naming a tool the model can't call is a dead end — the same unenforceable-claim problem, pointed the other way. With neither `grep` nor `glob` registered the gate is inert, and the startup line says so: `bash search gate: enforce but INERT (…)`. Otherwise the line names the posture and the binaries actually covered, on every boot including the default, and the `bash` tool's own description switches from "prefer the structured tools" to "REFUSED" so the model learns the rule from the catalog instead of from an error.

## Optional lifecycle tools

These are registered conditionally based on agent construction. They're not in the `BuiltinTools` struct because their presence depends on which `agent.New` options were passed.

### `mark_task_done`

Auto-registered when `WithCheckpointer` is wired. The model calls it at logical task boundaries with a short description; the runtime fires `Agent.Checkpoint(ctx, taskNote)` which writes a six-section completion record to the session event log as `CustomMetadata["compaction"] = "checkpoint"` and slices the prior history out of future model requests. See [Context management](/concepts/context-management/).

### `ask_user`

Registered when `--ask=stdin` / `--ask=auto` is set, or when a library caller provides `WithPrompter`. Lets the model ask the operator a question and receive a typed answer. In headless contexts (no TTY, `--ask=auto`) it returns a clean refusal rather than blocking forever.

```text
ask_user(question: "Should I delete the backup file before re-running?", default: "no")
```

### `schedule_next_turn`

Registered in autonomous-runner contexts. Lets the model emit a sleep / wake-at / wake-on-event signal that `autonomous.Run` consumes between turns and feeds to a `Scheduler` implementation (e.g. `SleepScheduler` for long-lived daemons). See [Autonomous runs](/run/autonomous/operations/).

## Agentic wrappers (subtask-routed tools)

By default (or when `tools/agentic` is registered manually in library use), four additional tools join the catalog:

- `agentic_read_file`
- `agentic_fetch_url`
- `agentic_grep`
- `agentic_research`

Each wrapper's handler delegates to `Agent.RunSubtask` against a separate, optionally cheaper model (via `--agentic-small-model=ID`, e.g. `gemini-3.5-flash-lite`). The bulk tool output lands in the subtask's context; only a focused digest flows back to the parent. The wrapper descriptions explicitly tell the model "use INSTEAD OF `read_file` when the file might be large" so the agent reaches for the wrapper at the right moments.

Cost rolls up to the parent's `usage.Tracker` so `/stats` reflects subtask spend transparently. See [Context management](/concepts/context-management/) for the design and the per-model cost breakdown.

## MCP tools

Tools declared in `.agents/mcp.json` are namespaced under the server name (`mcp.<server>.<tool>`). They route through the same permission gate under that namespace, with the same pattern grammar. The model sees them alongside built-ins; nothing in the catalog distinguishes built-in from MCP at the model interface. See [MCP servers](/concepts/mcp/) for the declaration schema.

## Digested survey tools (v2.9+)

Four built-ins go through the same [structural digest wrap](/concepts/mcp/#structural-digest-wrap---no-mcp-digest) as MCP responses: `read_many_files`, `grep`, `glob`, and `list_dir`. Above 8000 bytes their response is replaced by a digest plus a `call_id`, exactly as an MCP response would be, and the raw payload goes to the store for `retrieve_raw`.

The rule for membership is *survey*: these four answer "what is out there" over a set the model named loosely — a glob, a directory, a pattern. A survey answer is useful in summary, and the model has an obvious way to narrow it, so the trade is cheap in both directions. Before v2.9 a `read_many_files {pattern: "*"}` over a content root put its entire walk into context verbatim and left it there for every remaining turn of the session; a `gke_get_k8s_resource` returning the same bytes did not. Nothing in the tool surface signalled the difference.

Notably **not** digested:

- **`read_file`** — the narrowing move itself. Digesting a survey is only safe because "then read the one file you actually want" is a cheap next step. `read_file` is also what precedes `edit_file`, which matches `old_string` exactly; a truncated copy of a file the model is about to edit trades a few thousand tokens for a failed edit.
- **`bash`** — output the model routinely needs verbatim (a diff, a stack trace, an exit banner), with no structure the JSON pruner can exploit.
- **`json_query`** — the model already narrowed. Pruning a `jq` result second-guesses the extraction that was the point of the call.

Two behaviours differ from the MCP wrap. Responses **under** the threshold are returned unchanged rather than re-shaped into the synthetic map — digesting is a cost intervention, so it stays invisible until there is a cost. And a digest that came out no smaller than the payload it replaced is discarded in favour of the original.

The same `--no-mcp-digest` switch turns this off; despite the name it governs the whole digest layer, because the digest is only safe while `retrieve_raw` can undo it.

## `retrieve_raw` — digest escape hatch

| Tool | Purpose | Params |
| --- | --- | --- |
| `retrieve_raw` | Fetch the raw, un-digested payload for a prior tool call whose response arrived compressed by the [structural digest wrap](/concepts/mcp/#structural-digest-wrap---no-mcp-digest). The `call_id` is the marker the wrapper stamped on the digest. | `call_id` |

Registered automatically whenever the MCP digest wrap is on **and** a store is wired (i.e. `--session-db` is set); the `--no-mcp-digest` kill switch removes it. It is the model's reversal path when a digest looks like it dropped a load-bearing field — but every call re-inflates the full payload back into context, undoing the wrap's savings, so its description tells the model to treat the digest as authoritative and only reach for `retrieve_raw` when the digest itself flags a truncated field it needs. See [MCP → Structural digest wrap](/concepts/mcp/#structural-digest-wrap---no-mcp-digest).

## Custom tools

Library callers can register arbitrary tools via `agent.WithTools`. The ADK `functiontool.New` factory accepts a Go function whose parameters become a JSON-schema declaration the model sees:

```go
declTool, _ := functiontool.New(functiontool.Config{
    Name:        "spell_check",
    Description: "Run aspell against the given text and return misspellings.",
}, func(ctx context.Context, text string) ([]string, error) { /* ... */ })

a := agent.New(model,
    agent.WithTools(declTool),
    agent.WithGate(gate),
)
```

For late binding to the constructed `*Agent` (when a custom tool needs to call back into the agent — `Agent.RunSubtask`, `Agent.AskSideQuestion`, etc.) use `agent.WithPostConstruct(func(*Agent))`. The in-tree `mark_task_done` tool and the agentic wrappers both use this pattern. See [Library API](/embed/api/) for the full extension surface.

## Tool descriptions are model-facing prompts

A tool's description text shows up verbatim in the system prompt the model sees. `core-agent`'s built-in descriptions are deliberately prescriptive — they tell the model not just what the tool does but **when to prefer it over alternatives** ("use `grep` instead of `bash grep`", "use `read_many_files` instead of parallel `read_file` calls", "use `agentic_read_file` when the file might be large"). The prescriptive framing is what keeps the model from defaulting to `bash` for everything.

If you're authoring your own tools, mirror this pattern: a description that says only "Search files for a pattern" loses the model. One that says "Search files for a pattern. **Use this instead of `bash grep`** when investigating source code — the output is structured and the gate sees the call" wins the routing decision at every turn.

That said, a description is still advisory, and measured against a Gemini variant the search hint lost 15 times out of 27. Where a routing preference actually matters, back it with a gate that refuses the alternative — that's what [the bash search gate](#the-bash-search-gate) does, and the `bash` description changes to say "REFUSED" when it's armed, because a description that undersells the runtime costs the model a turn to discover the real rule.
