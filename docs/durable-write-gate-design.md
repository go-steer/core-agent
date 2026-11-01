# Durable Write Gate & Recorded-Effect Outbox Design

**Status:** settled, 2026-11-01 ([#1399](https://github.com/go-steer/core-agent/issues/1399)). Implementation tracked in [#1400](https://github.com/go-steer/core-agent/issues/1400).
**Parent:** [`docs/mast-fold-design.md`](./mast-fold-design.md) (§ Settled decisions 5–6, § Phases P2).
**Ported from:** `go-steer/mast` (`pkg/approval`, `internal/effects`, `internal/transcript/{ack,decisions}.go`, `internal/inject/parks.go`, `internal/cli/permsource.go`, `docs/durable-execution-design.md`).

## Problem statement

In `core-agent` today, permission gating for mutating tool calls (`pkg/permissions.Gate` + `pkg/tools.GateToolset` + `pkg/attach.PromptBroker`) is an **in-process blocking channel**: `PromptBroker.Prompt(ctx, req)` blocks the calling goroutine on a Go channel (`waiter.ch`) while waiting for an interactive TUI or SSE operator to `POST /perms/respond`.

Two durability hazards follow from that shape in unattended or long-running deployments:

1. **Ephemeral approval state.** If the daemon restarts, rolls, or loses its pod while waiting for an operator's answer — or if no operator is attached when an autonomous turn proposes a mutation — the pending prompt vanishes with the process.
2. **No recorded-effect outbox for mutating tool calls.** If the process crashes *after* a mutating tool call is dispatched to an external system (or mid-execution after an approval) before its `FunctionResponse` is persisted to the session store, an automatic continuation turn cannot distinguish *"the tool finished and its response was recorded"* from *"the tool was in flight when the process died and may or may not have mutated the world"*. `pkg/agent/tail_repair.go` (`repairDanglingToolCalls`) synthesizes a `[interrupted before tool result was recorded]` placeholder so the LLM provider accepts the history, leaving the model free to re-invoke the mutating tool a second time.

Phase 2 of the one-harness fold (`#1399`, `#1400`) brings `mast`'s durable execution core into `core-agent`:

1. **Recorded-effect outbox (`internal/effects`)**: treats the durable session event log as an effect outbox (`FunctionCall` = intent, `FunctionResponse` = completion), short-circuits re-execution of already-completed mutating calls on replay, and halts the session fail-closed (`ambiguous_prior_effect`) if an unacknowledged dangling mutating intent is present.
2. **Durable write gate (`pkg/approval`)**: parks mutating tool calls in the session event log via ADK's tool-confirmation flow (`adk_request_confirmation`) so the turn ends cleanly with zero goroutines blocked, and resumes on a later turn when a verdict (`approve`, `reject`, `edit`) arrives.
3. **Unified `PromptBroker` view**: makes `PromptBroker` a view over durable parks when a durable session store is wired, while keeping interactive `ask` (`core-agent` TUI, REPL, `/perms/stream` + `/perms/respond`) byte-identical.

## Settled decisions (do not relitigate)

1. **Two ADK runner plugins bracket every tool call in fixed registration order: `effects` first, `approval` second.**
   - ADK's `PluginManager.runBeforeToolCallbacks` iterates `PluginConfig.Plugins` in registration order; the first plugin returning a non-nil `map[string]any` short-circuits `tool.Run` and every subsequent plugin.
   - `effects.NewPlugin` **must** be registered first and `approval.NewPlugin` **must** be registered second on `runner.Config.PluginConfig`. Reordering is a correctness bug:
     - On replay of a turn that already completed a mutating tool call, `effects` intercepts `BeforeToolCallback` and returns the cached `FunctionResponse` before `approval` runs — preventing a completed write from parking for a second approval.
     - When a mutating call has not yet executed, `effects` returns `(nil, nil)` and `approval` runs next to enforce policy, consume a change-set grant, or call `ctx.RequestConfirmation` to park durably before any external side effect occurs.

2. **The session event log is the outbox (`FunctionCall` = intent, `FunctionResponse` = completion).**
   - ADK persists the model event carrying `FunctionCall` to `session.Service` *before* dispatching `handleFunctionCalls`. Every tool call therefore leaves a durable intent row before its Go handler executes.
   - At the start of each runner invocation (`BeforeRunCallback`), `effects` scans the session's prior events (`pairScan`) and constructs a per-invocation `runState`:
     - **Completed mutating calls (`completed[callID]`)**: if ADK replays a `FunctionCall` whose ID is in `completed`, `BeforeToolCallback` returns the recorded response map verbatim without re-invoking the tool.
     - **Dangling mutating intents (`dangling`)**: a mutating `FunctionCall` from a prior invocation with no paired completion `FunctionResponse` (and not currently parked awaiting confirmation or rejected; see § Fixing `mast` #561 below) whose timestamp is after the session's `EffectsAckedAt` watermark puts the invocation into **ambiguous-effect mode** (`status: ambiguous_prior_effect`). Every tool call in that invocation is refused before execution with a structured error naming the dangling tool, `function_call_id`, arguments, and timestamp, and `AfterRunCallback` surfaces the refusal if the model emits no tool calls. The session stays blocked until an operator runs `ack-effects`.
     - **Dangling spawning intents (`danglingSpawns`)**: a `ClassSpawning` call (`task`, `single_turn`, or a declarative subagent tool) with no paired `FunctionResponse` does not itself mutate external state; its side effects are governed by the subagent's own mutating tool calls. `effects` inspects the subagent's deterministic child session (via `SubSessionScanner`); any unacknowledged dangling mutating call inside the sub-session puts the parent into `ambiguous_prior_effect` mode, and if no scanner is wired, a dangling spawn fails closed as an ambiguous effect.

3. **A park is durable via ADK's tool-confirmation flow (`adk_request_confirmation`), and `PromptBroker` becomes a view over durable parks.**
   - When a mutating tool call requires operator approval in durable mode, `approval`'s `BeforeToolCallback` calls `ctx.RequestConfirmation(hint, approval.Request{...})` and returns `{"status": "awaiting_operator_approval", "message": hint}`.
   - ADK appends the placeholder `FunctionResponse` (with `Actions.RequestedToolConfirmations[callID]`) and emits a model event containing a long-running `FunctionCall` to `adk_request_confirmation` (`LongRunningToolIDs: [confirmCallID]`). The runner turn ends cleanly with zero blocked goroutines.
   - Resuming posts a user `Content` carrying a `FunctionResponse` with `ID: confirmCallID`, `Name: "adk_request_confirmation"`, and `Response: ConfirmationResponse(verdict)` (`{"confirmed": bool, "payload": Verdict}`). ADK's step-0 confirmation processor unwraps the response and re-dispatches the original `FunctionCall` with `ctx.ToolConfirmation()` populated.
   - `approval_timeout`, `approval_notify`, `auto` mode, and `allow-always` / session grants are preserved (see § Interactive mode and `PromptBroker` projection below).

4. **Three-valued verdict (`approve` / `reject` / `edit`), exact `(tool, arguments)` change-set grants with freshness re-read, and prior-state capture with a never-auto-fired proposed revert.**
   - **Verdicts (`pkg/approval/verdict.go`, `edit.go`)**:
     - `approve`: permits the call (`ScopeOnce` or `ScopeChangeSet`).
     - `reject`: refuses the call without executing it and returns `{"status": "rejected_by_operator", "tool": ..., "reason": ...}` to the model so it can replan or conclude cleanly.
     - `edit`: operator supplies `EditedArgs map[string]any`. Requires a non-empty authenticated `Approver` identity; validates `EditedArgs` against the tool's input JSON schema (`NormalizeArgs`); re-runs `Gate.CheckMutatingToolCall` on the edited `(tool, key)` so an edit cannot step into a deny rule; mutates the `args` map in place before `tool.Run`; and records an `AppliedEdit` in `ctx.Actions().StateDelta[EditStateKey(callID)]` (`app:approval_edit:<callID>`).
   - **Change-set producer & scoped grants (`changeset.go`, `grant.go`, `grantgate.go`)**:
     - Validates `proposed_change` (`tool`, `arguments`, `precondition`) on `finish_task` against the live tool catalog and JSON schema before the turn closes.
     - Approving with `ScopeChangeSet` mints a durable `Grant` in `StateDelta[GrantStateKey(sig)]` (`approval_grant:<sha256(tool + canonicalJSON(args))>`) with a bounded TTL (`DefaultGrantTTL = 10m`) and an optional `GrantPrecondition` (`tool`, `arguments`, `expected` snapshot).
     - Before a subsequent mutating call consumes a matching grant (`tryConsumeGrant`), if a `GrantPrecondition` is present, the gate re-executes the read-only precondition tool and compares its normalized JSON output (`outcomeEqual`). If the live state drifted from `expected`, the grant is voided (`VoidedReason`) and the call parks for fresh operator approval.
   - **Prior-state capture (`capture.go`)**:
     - Every permitted path (`apply` mode, `approve` verdict, `edit` verdict, and `change_set` grant consumption) funnels through `permit()`.
     - When a `PriorStateCapture` declaration exists for the mutating tool, `permit()` runs the paired read-only capture tool *before* the mutating tool executes and records `Prior` + `SHA-256 Digest` + schema-validated `Revert` (`ProposedChange`) into `ctx.Actions().StateDelta[CaptureStateKey(callID)]` (`app:approval_capture:<callID>`).
     - Fail-closed: if the capture read fails or the revert arguments fail schema validation, `permit()` refuses the mutating call before it runs. The revert payload is recorded for operator inspection and is **never auto-fired**.

5. **Default-deny mutation classification; MCP `readOnlyHint` is opt-in per server (`mast` #573).**
   - Any tool not positively classified as `ClassReadOnly` or `ClassSpawning` is `ClassMutating` (`DefaultClass == ClassMutating`).
   - Precedence order (highest to lowest):
     1. **Engine control calls** (`adk_request_input`, `adk_request_confirmation`, `finish_task`, `mark_task_done`, `transfer_to_agent`, `pause_session`, `request_operator_input`, `exit_loop`): always `ClassReadOnly` (never configurable).
     2. **Explicit operator config overrides** (`permissions` / `tool_catalog` per-tool classification): `ClassMutating` or `ClassReadOnly`.
     3. **Built-in `core-agent` tool table**:
        - `ClassReadOnly`: `read_file`, `read_many_files`, `view_file_outline`, `grep`, `glob`, `stat`, `list_dir`, `json_query`, `todo`, `record_plan`, `wait_and_verify`, `ask_user`, `retrieve_raw`, `list_skills`, `load_skill`, `read_skill_resource`, `schedule_next_turn`, `report_done`, `return_result`, `check_subagent`, `list_subagents`, `read_subagent_logs`.
        - `ClassSpawning`: `task`, `single_turn`, `spawn_subagent`, `spawn_agent` (in-process), and any synchronous declarative subagent tool registered via `WithSubagents`.
        - `ClassMutating`: `write_file`, `edit_file`, `delete_file`, `bash`, `fetch_url`, `alert`, `spawn_remote_agent`, `stop_subagent`, `send_message`.
     4. **MCP tool annotations (`annotations.readOnlyHint == true`)**: honored *only* when the MCP server entry explicitly opts in (`trust_read_only_hint: true`, `mast` `#573`); untrusted MCP tools default to `ClassMutating`.
     5. **Fallback**: `ClassMutating`.

6. **Subagent dispatches (`task` / `single_turn` / declarative subagents) and park/resume semantics.**
   - `#1399` requires choosing between:
     - **(a)** running `task` / `single_turn` subagent tools directly in the parent session row (the `mast` `TaskAgentTool` shape), or
     - **(b)** running synchronous subagent tools in deterministic derived child sessions (`subsession.DeriveID(parentSessionID, subagentName, parentFunctionCallID)` = `<parentSID>:sub:<subName>:<callID>`) on the parent's `session.Service` while making outbox scanning and park/resume sub-session-aware.
   - **Decision:** We adopt **both where each applies**:
     - For `task` / `single_turn` mode constructors ported from `mast` (#1401) that run as leaf delegations without inner mutating tools (`capability: read_only` or toolless `single_turn`), no write-gate park can occur inside the delegation; `task` mode dispatches that carry `capability: change_executor` (or general declarative subagents in `pkg/agent/subagent.go`) use deterministic child session IDs (`subsession.DeriveID(parentSID, subName, callID)`) on the shared `session.Service` with `effects.WrapSubRunner` (`internal/effects/subrun.go`).
     - Why deterministic child session IDs are preserved for sub-runners: as documented in `core-agent` `#546` (`pkg/agent/subagent.go:244-253`) and `mast` `#45`, ADK's database `session.Service` holds an optimistic-concurrency write lease (`updatedAt` check) on the parent session handle while the parent's `tool.Run` is in flight; appending inner subagent events directly to the parent `SessionID` mid-call bumps `updated_at` and fails the parent's subsequent `AppendEvent` with `"stale session error"`.
     - Because `subsession.DeriveID(parentSID, subName, callID)` is a pure function of the parent `FunctionCall` event (`Name` + `ID`), a replay or `SubSessionScanner` deterministically locates `<parentSID>:sub:<subName>:<callID>`, replays already-completed inner mutating calls from the child session log without re-executing them, blocks the parent turn if the child session has an unacknowledged dangling mutating call, and bubbles any child confirmation park up through the parent `tool.Context` (`tc.RequestConfirmation`) so the park is recorded in the parent session log and resumes from the root session.

7. **Interactive mode is unchanged (strict superset guarantee).**
   - In interactive TUI / REPL sessions (`cmd/core-agent`, `cmd/core-agent-tui`) and existing `/perms/stream` + `/perms/respond` consumers, `ask` mode remains byte-identical.

## Interactive mode and `PromptBroker` projection

`core-agent`'s `permissions.Gate` and `attach.PromptBroker` support features that `mast` did not have: interactive TUI overlays (`allow-once`, `allow-session`, `allow-session-verb`, `allow-session-tool`, `allow-always`), `approval_timeout`, `approval_notify`, and model-backed `auto` mode (`pkg/approver`). The durable write gate preserves every one of them:

1. **Pre-park synchronous verdict sources (zero park overhead when already decided):**
   - **Session and persistent `allow-always` grants**: `approval.Plugin` checks `Gate.CheckMutatingToolCall(tool, key)` and `tryConsumeGrant` before parking. If an operator previously granted `allow-session*` or `allow-always` (persisted via `.agents/config.json` `SaveAllowPattern`), or if a valid `ScopeChangeSet` grant matches `(tool, args)`, the call is permitted in-turn without parking.
   - **Model-backed `auto` mode (`pkg/approver`)**: When `Gate.Mode() == ModeAuto` and the call is eligible for the model approver, `Gate` consults the wired `Approver` before raising a park. An `Approve` verdict records a durable `Decision` (`Approver: "auto:<model>"`) and runs the tool in-turn; a `Deny` verdict refuses the call in-turn; an `Escalate` verdict (or approver error when `escalation != "refuse"`) falls through to the human prompt / durable park path.
2. **Live interactive channel vs. durable park projection on `PromptBroker`:**
   - When `PromptBroker` is used in-process without durable confirmation parking (or for non-confirmation prompts such as read path-scope expansions), `PromptBroker.Prompt(ctx, req)` blocks on `waiter.ch` with `approval_timeout` and `approval_notify` exactly as today.
   - When a session uses durable parks (`pkg/approval` + `pkg/eventlog`):
     - Raising a park fires the configured `Notifier` (`approval.WithNotifier` / `approval_notify`).
     - `PromptBroker.SubscribePrompts(ctx)` projects open durable approval parks from the session event log alongside live in-memory waiters as standard `PromptFrame` events (`ID: interruptID, Kind: ..., ToolName: ..., Detail: ...`). A client attaching to `/perms/stream` after a daemon restart immediately receives the open durable parks in its initial seed.
     - `POST /perms/respond` (`PromptBroker.RespondPrompt`) resolves live in-memory waiters first; if the ID matches an open durable approval park, it maps the `permissions.Decision` to an `approval.Verdict` (applying any `allow-session*` or `allow-always` grant on `Gate`), stamps the authenticated caller, and invokes the session's durable resume handler.

## Fixing `mast` #561 and #562 in the port

### 1. `mast` #561: Approved mutating call interrupted mid-execution

**The failure mode in `mast`:**
1. Model emits mutating `FunctionCall{ID: "call-1", Name: "write_file"}`.
2. `approval` calls `ctx.RequestConfirmation`. ADK writes:
   - Event A (`agent`): `FunctionCall{ID: "adk-conf-1", Name: "adk_request_confirmation", Args: {"originalFunctionCall": {"id": "call-1", ...}}}` (`LongRunningToolIDs: ["adk-conf-1"]`).
   - Event B (`user`): placeholder `FunctionResponse{ID: "call-1", Name: "write_file", Response: {"status": "awaiting_operator_approval"}}` with `Actions.RequestedToolConfirmations["call-1"]`.
3. Operator approves. ADK writes Event C (`user`): `FunctionResponse{ID: "adk-conf-1", Name: "adk_request_confirmation", Response: {"confirmed": true, "payload": {"verdict": "approve"}}}` and re-dispatches `write_file` (`ID: "call-1"`).
4. `write_file` executes its external side effect, and **the process crashes before Event D (`FunctionResponse{ID: "call-1"}`) is persisted**.

In `mast`, `pairScan` unconditionally executed `delete(open, fr.ID)` when it saw Event B's placeholder `FunctionResponse{ID: "call-1"}`. When Event C (`confirmed: true`) arrived later, `"call-1"` was already gone from `open`. After the crash:
- `ScanDangling` reported `mutating = 0` (blind to the interrupted post-approval execution).
- On auto-resume, ADK's step-0 confirmation processor saw Event C as the trailing user event with no post-approval response for `"call-1"` and **re-executed `write_file` a second time**.
- Furthermore, `approval.writeDecision` only wrote the `Decision` record into `ctx.Actions().StateDelta` (which rides on Event D), so the crash lost the audit record of *who* approved `"call-1"`.

**The three-part fix in `core-agent` (#1400):**

1. **Confirmation-aware `pairScan` in `internal/effects/effects.go`:**
   - When `pairScan` encounters a `FunctionResponse` `fr` on event `ev` where `_, isPlaceholder := ev.Actions.RequestedToolConfirmations[fr.ID]` is true, it **does not** `delete(open, fr.ID)` and **does not** record `fr` in `completed[fr.ID]`. Instead it keeps `open[fr.ID]` in `open` with `excluded = true` (while parked awaiting an operator decision, the call has not executed and is not dangling).
   - `pairScan` tracks `confirmTargetByID[confirmCallID] = origCallID` from every `adk_request_confirmation` `FunctionCall` (`Args["originalFunctionCall"]["id"]`).
   - When `pairScan` encounters the operator's `FunctionResponse` for `adk_request_confirmation` (`origID := confirmTargetByID[fr.ID]`):
     - If `confirmed == true` (and the payload verdict is not `reject`), ADK dispatches `origID` in that turn: `pairScan` flips `open[origID].excluded = false` and advances `open[origID].at = ev.Timestamp` (so an earlier `EffectsAckedAt` watermark cannot pre-ack the newly approved execution).
     - If `confirmed == false` (or the payload verdict is `reject`), the call never ran: `pairScan` deletes `open[origID]`.
   - Only a subsequent non-placeholder, non-synthetic `FunctionResponse` for `origID` closes `open[origID]` and moves it to `completed[origID]`.
   - Consequently, if the process dies after Event C (`confirmed: true`) and before Event D, `open["call-1"]` remains open with `excluded == false`, `ScanDangling` returns `mutating = 1`, and the next turn enters fail-closed `ambiguous_prior_effect` mode until an operator runs `ack-effects`.
2. **Pre-execution durable `DecisionSink` and per-`FunctionCallID` dispatch guard in `pkg/approval`:**
   - `approval.WithDecisionSink(func(ctx context.Context, d Decision) error)` persists the `Decision` record (`Approver`, `Verdict`, `FunctionCallID`, `Tool`, `Key`, `DecidedAt`) to the durable store *before* `BeforeToolCallback` returns `nil` to run the tool handler, in addition to writing `ctx.Actions().StateDelta[DecisionStateKey(callID)]`. Even if the process crashes mid-tool, the approver attribution is durably recorded.
   - `writeGate` also maintains a per-`FunctionCallID` dispatch guard (`dispatched sync.Map` + state check) in `honorVerdict`: if ADK ever attempts to re-dispatch the same `FunctionCallID` without a fresh confirmation cycle, `honorVerdict` refuses duplicate execution with `status: ambiguous_prior_effect`.
3. **Coordinating `pkg/agent/tail_repair.go` (`repairDanglingToolCalls`) with `effects` and `adk_request_confirmation`:**
   - `findDanglingCalls` in `pkg/agent/tail_repair.go` skips any `FunctionCall` whose ID is listed in `ev.LongRunningToolIDs` (such as `adk_request_confirmation` or `adk_request_input`) so an open durable park is never closed by synthetic tail repair.
   - Placeholder `FunctionResponse` events carrying `Actions.RequestedToolConfirmations[id]` do not count as final tool completions once their paired `adk_request_confirmation` has been answered with `confirmed: true`.
   - Every synthetic repair event appended by `repairDanglingToolCalls` carries `CustomMetadata["core_agent_synthetic_repair"] = true`, and `effects.pairScan` ignores synthetic repair responses so LLM transcript repair never masks an unacknowledged dangling mutating effect.

### 2. `mast` #562: Request-context turns vs. daemon-owned turn contexts

- In `mast`, HTTP handlers for `/inject`, `/resume`, and A2A passed `r.Context()` directly into `runner.Run(ctx, ...)`. When an HTTP caller disconnected mid-turn, `net/http` cancelled `r.Context()`, killing any in-flight tool call and stranding a dangling mutating intent.
- In `core-agent`, once a request to start or resume a turn (`/inject`, `/perms/respond`, `/resume`, `/ack-effects`, A2A) is authenticated and admitted, the turn executes on a **daemon-owned context** (`context.WithoutCancel(r.Context())` bounded by the daemon/session shutdown context, preserving caller identity and OTel trace links). Client disconnect ends only the HTTP response wait, never the admitted turn.

## Package architecture (`#1400`)

| Package | Role | `dev/api-surface.txt` |
|---|---|---|
| `pkg/permissions` (`mutating.go`) | `ToolClass` (`ClassMutating`, `ClassReadOnly`, `ClassSpawning`), `DefaultToolClass`, `MCPToolClass`, `IsEngineControlCall`, `Gate.ToolClass`, `Gate.CheckMutatingToolCall`, `Gate.RecordMutationVerdict` | `promised` (existing) |
| `internal/effects` (`effects.go`, `subrun.go`) | Recorded-effect outbox ADK runner plugin (`NewPlugin`), confirmation-aware `pairScan`, `ScanDangling`, `WrapSubRunner`, `SubSessionScannerFor` | internal (exempt) |
| `pkg/approval` (`plugin.go`, `verdict.go`, `decision.go`, `edit.go`, `changeset.go`, `grant.go`, `grantgate.go`, `capture.go`, `describe.go`, `notify.go`, `suppress.go`) | Durable write-gate ADK runner plugin (`NewPlugin`), 3-valued verdicts, pre-execution `DecisionSink`, per-call-ID dispatch guard, change-set producer + scoped grants with precondition re-read, prior-state capture | `unsupported` |
| `pkg/eventlog` (`decisions.go`) | Durable `DecisionStore` (`agent_approval_decisions`) and `EffectsAck` watermark helpers on `eventlog.Handle` | `promised` (existing) |
| `pkg/attach` + `pkg/agent` | `WithPlugins` on `agent.New`, `tail_repair.go` long-running + synthetic-repair coordination, `/parks` and `/ack-effects` routes, `sessions export-decisions` CLI | existing |

## Out of scope

- `mast`'s watchdog pool and its three hand-written turn drivers (`core-agent`'s guardrails and `#1389` own turn outcomes).
- `graph` and `planner` dispatch shapes (excluded by `docs/mast-fold-design.md`).
- A2A server (`#1407`) and AG-UI (`#1409`), which build on top of durable parks in Phase 5.
