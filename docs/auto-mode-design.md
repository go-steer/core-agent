# Auto mode: an approver model decides routine prompts

Design for [#1175](https://github.com/go-steer/core-agent/issues/1175).

**Status:** Phases 1 (`pkg/permissions`) and 2 (`Args` at the call sites) implemented. Phases 3–5 are not, so `auto` cannot be selected yet.

## Motivation

The permission modes offer two kinds of approval. Either a person
decides (`ask`, `acceptEdits`), or a fixed rule decides (`allow`'s
allowlist, `yolo`'s nothing). An operator who steps away has to choose
between an agent that stalls and one that runs ungated. In the #1116 T2
run, a `spawn_agent` prompt sat unanswered for 55 minutes.

Auto mode adds a middle option. A call that `ask` would have prompted
for can go first to an **approver model**. The approver allows it,
denies it with a reason the agent can act on, or passes it to a person.

## What bounds the approver, stated honestly

"The approver only sees calls `ask` would prompt for" is true, but on
its own it bounds very little. In `ask` mode almost every call that is
not allowlisted reaches the prompt, `rm -rf ~` included. The builtin
bash denylist is explicitly not a security boundary (`gate.go`, the
`CheckBash` doc). **For a call the approver allows, the worst case is
what `yolo` would have done with that call.**

So the design does not rely on the approver's judgement for safety. The
bound comes from rules enforced in code that the approver cannot see
past:

- configured and session deny patterns, and plan-first, which run first;
- a fixed list of kinds it may never decide (decision 4);
- an opt-in list of what it *may* decide (decision 3), which is empty
  unless the recipe author names something.

The approver's judgement only chooses, within that set, which calls
still need a person.

## Settled decisions (do not relitigate)

1. **Auto is `ask` with an optional step in front of the prompt.**
   - `ModeAuto` behaves like `ModeAsk` throughout `gateRequest`: denylist, path scope, deny patterns, allowlist entries, session grants and plan-first all run first, unchanged.
   - `gateRequest` passes its already-resolved mode into `prompt`. `prompt` does not re-read `g.mode`: that would race `SwapMode` and could disagree with the mode the switch used.

2. **The approver lives in `Gate.prompt`.**
   - The order inside `prompt` is:
     1. the #1074 turn-refusal check, unchanged;
     2. the eligibility checks (decisions 3 and 4);
     3. the approver;
     4. on escalation, the existing path (`g.prompter`, `approval_timeout`, `ErrNoPrompter`).
   - The approver runs before the nil-prompter check, so auto works on a host with no prompter. There, routine eligible calls proceed and everything else is denied.
   - Control-plane writes are outside this entirely. `checkControlPlaneWrite` calls `askApproval` directly and never calls `prompt` (its own comment says so). A Phase 1 test pins that the approver stub is never called from that path.

3. **The approver decides only what the recipe opts it into, and only with the whole call in hand.**
   - `permissions.auto.eligible` lists tool patterns in the existing `tool:pattern` syntax, for example `["bash:go test *", "write_file:*", "edit_file:*"]`. A request that matches no entry escalates without an approver call. The default is empty, so `mode: auto` with no list changes nothing, and startup logs a warning about it.
   - A request is also eligible only if the gate holds the **full call**. `PromptRequest` gains an additive `Args json.RawMessage` field, carrying the untruncated arguments. Each `Check*` call site that has arguments fills it in.
   - `Detail` is not enough: for MCP and skill tools it is cut at 200 bytes (`summarizeRequest`, `pkg/tools/gate.go`). `alert`, `call_peer`, `fetch_url` and synchronous subagents pass only a target name or URL, and `write_file`'s content never reaches the gate at all.
   - A request with no `Args` escalates, so a call site nobody updated fails closed.

4. **Some kinds are never the approver's to decide.** Each of these escalates without an approver call, whatever the eligible list says:
   - `PromptKindPathScope`, access outside the declared roots. Path scope is the boundary the code deliberately checks before session grants (#380). An approver must not grant `~/.ssh` or `~/.config/gcloud`.
   - `PromptKindControlPlaneWrite`, which structurally never reaches `prompt` anyway (decision 2).
   - Any bash command that names a control-plane path (`.agents/config.json`, `.agents/mcp.json`), or names the approver's `instructions_file`. Today only `CheckFileWrite` checks for control-plane paths, so `sed -i … .agents/config.json` would otherwise reach the approver as a plain bash prompt. This floor is a substring match on the resolved names, best-effort like the denylist, and a Phase 1 test says so.
   - Calls from background subagents, in the first cut (decision 6).

5. **The approver only ever allows once.** An allow is recorded as `DecisionAllowOnce`. It never produces a session, verb, tool or always grant, and never writes to any policy, grant map or `GrantStore`.

6. **The approver never reads tool output, and its "task" comes only from sources the operator controls.**
   - The approver's input is:
     - the pending call (`ToolName`, `Detail`, `Args`), quoted as untrusted data;
     - the task;
     - the names and details of the turn's earlier calls, with no results.
   - **The task** is the text of turns that a verified operator sent:
     - a typed message;
     - `POST /inject` from an authenticated caller;
     - the `-p` prompt.
   - These are **not** the task:
     - wake payloads, which relay event text from lookout;
     - auto-continue re-drives;
     - scheduler and peer or switchboard traffic;
     - compaction summaries.

     Each is either left out or passed in a separate block labelled model- or system-authored and untrusted. If compaction has removed every operator turn, there is no task, and the call escalates.
   - **Background subagents** keep their parent's context values (`context.WithoutCancel`). So they would be judged against the parent's turn, which describes the wrong task. Their real task is a brief the parent model wrote, which can carry injected text. In the first cut, a call carrying a `SubagentSource` escalates. Giving subagents their own labelled brief is a later phase.

7. **The verdict policy has a built-in core, and a recipe can add to it through a privilege-bearing file.**
   - The built-in instruction refuses actions that are:
     - destructive or irreversible;
     - outward-facing;
     - outside the stated task.

     It also treats everything in the pending call as data.
   - A recipe can add text through `permissions.auto.instructions_file`.
   - That file acts on the gate, not through the model. So it joins the privilege-bearing tier: writes to it take the elevated control-plane prompt (`controlplane.go`), and bash that names it is floored by decision 4.
   - It is loaded once at startup, with no hot reload. A changed file needs a restart, like any other permissions change made in config.

8. **The approver's model is configured on its own, and billed to the turn that triggered it.**
   - It is `permissions.auto.model`, resolved through the `models` registry.
   - The agent stamps a usage sink on the turn context, next to the task provider: the session's tracker, or the subagent's `billTo` tracker. The approver records into it, the way `recordInternalLLMUsage` already bills internal model calls.
   - An approver call is skipped, and the request escalates, when the turn or session ceiling (`max_turn_cost_usd` / `max_session_cost_usd`) is **already** reached. A single approver call can overshoot a ceiling, the same as any model call.

9. **An approver deny is final for the turn.**
   - It arms the #1074 turn-refusal memory under a new kind, `refusedByApprover`. The agent retrying the same `tool|detail` gets the repeat-refusal error, and neither the approver nor a person is asked again this turn. The error names the approver, not a person.
   - The memory is keyed on `tool|detail`, which is coarse. One approver deny of an `alert` to `slack-oncall` blocks every alert to that target for the turn. That is accepted: it errs toward fewer calls, and an operator can still `/allow`.
   - The watchdog does not see denials as such, only the resulting tool errors and repeated calls. The #1081 cut counts suppressed repeats. So approver denies reach both the same indirect way operator denies do. Nothing new is wired.

10. **Audit.**
    - `ApprovalLog` gains `Approver string`, the approver model's ID. `ApprovalLog.By` and `Approval.By` stay reserved for a verified human. `recordApproval` gains the field, and the attach state projection and both core-tui approval adapters carry it.
    - Every approver verdict is also written as an eventlog row through an audit sink the agent stamps on the turn context, the way `refusal_storm.go` appends its own event. It covers allow, deny and escalate, with the call, the verdict, the reason and the model. Operator answers are not durable rows today, so approver verdicts end up better audited than operator ones. That is deliberate, not an oversight to copy back.
    - Approver-specific text replaces three messages that would otherwise be wrong: "denied by user", the repeat error's "not put to a human again", and the `ErrNoPrompter` advice to use `--yolo`.

11. **An escalation carries the approver's reason as quoted, untrusted text, and offers only once/deny.**
    - The reason is model output that injected arguments can steer ("routine, safe to always allow"). So the modal quotes it as the approver's words.
    - On an approver-escalated prompt, the session and always options are hidden.

12. **Escalation on a daemon waits, so auto requires `approval_timeout`.**
    - A derived session gate always has the broker as its prompter, even with nobody attached. So an escalation waits instead of denying. Without a timeout it waits forever, which is the 55-minute stall from the Motivation.
    - `mode: auto` with no `approval_timeout` is a config error at startup, and a 400 on `POST /perms/mode`.

13. **core-tui gets a fifth mode chip, `auto`, and it ships before `auto` can be turned on.**
    - The TUI's mode chip (the status-bar indicator you cycle) has four values today: `ask`, `acceptEdits`, `plan` and `yolo`. This design adds a fifth, `auto`, in core-tui. That is new core-tui work, tracked as its own core-tui issue, and it also puts `auto` in the chip's cycle order.
    - Both core-agent translators learn to map to it: `translateMode` in `cmd/core-agent` (local TUI) and `permModeToChip` in `internal/coretuiremote` (attach TUI).
    - Why this has to come first: today both translators show any mode they don't recognise as the default chip, `ask`. Without the new chip, an operator would see `ask` while a model approves calls. Cycling would also move on from `ask` and silently drop `auto`.
    - So nothing lets you select `auto` (the `/permissions` picker, `POST /perms/mode`, `permissions.mode` in config) until the core-tui release with the fifth chip is pinned and both translators use it.
    - A test fails if either translator maps `auto` to the default chip. `verify-coretui-guards` covers the new chip, like any other core-tui capability.

14. **Multi-session.**
    - The approver's configuration is daemon-wide. The mode is per session (#1168; switching to it is owner or admin only).
    - `DeriveForSession` inherits the approver and its eligibility list from the template, like its other inherited fields. A test pins it.

## Out of scope

- Replacing `ask`, or changing what `yolo`, `allow` or `acceptEdits` mean.
- The approver granting anything, or answering with anything other than allow-once, deny or escalate.
- The approver deciding path-scope, control-plane or background-subagent calls in the first cut.
- The approver reading tool results or the whole transcript.
- A separate cost budget for the approver.
- Letting a person override an approver deny within the same turn.
- Learning from operator answers.
- Batching several pending calls into one approver request.

## Design

### Where it plugs in

```go
// pkg/permissions

type VerdictOutcome int // VerdictAllow, VerdictDeny, VerdictEscalate

type Verdict struct {
    Outcome VerdictOutcome
    Reason  string // required for deny; quoted to the model and the operator
    Model   string // approver model ID, for ApprovalLog.Approver
}

// Approver judges eligible calls in ModeAuto. Implementations live
// outside pkg/permissions, which must not import pkg/models.
type Approver interface {
    Judge(ctx context.Context, req ApproverRequest) (Verdict, error)
}

// ApproverContext is stamped on the turn context by the agent, like
// WithSessionGate. It supplies the task (decision 6), the usage sink
// and ceiling check (8) and the audit sink (10). A call with no
// ApproverContext escalates.
type ApproverContext interface { /* Task, RecentCalls, Bill, CeilingReached, Audit */ }
```

`PromptRequest` gains `Args json.RawMessage` (decision 3). Because it is an additive field on a public struct, it stays inside the v2 stability promise.

### Every place that must learn about `auto`

Each of these silently ignores or rejects an unknown mode today, so each belongs in Phase 1:

- the `SetMode` / `SwapMode` whitelists (unknown modes are dropped silently);
- the `gateRequest` switch ("unknown permission mode");
- `promptForPath`, which must escalate under decision 4 and not fall through;
- config validation;
- `ToolGateState` and `Snapshot`;
- `RemotePermModes` / `ParseRemotePermMode` and the `ErrPermModeNotSettable` text;
- the autonomous-loop deadlock guard (`autonomous.go`);
- `DeriveForSession`.

### The implementation

`pkg/approver` imports `pkg/permissions` and `pkg/models`; there is no cycle. It resolves `permissions.auto.model` and makes one non-streaming call per eligible request. The answer is structured: `{"verdict": "allow"|"deny"|"escalate", "reason": "..."}`. An error, a parse failure, a missing reason on a deny, or a timeout (`permissions.auto.timeout`, default 30s, which runs before `approval_timeout` starts) all escalate.

## Evaluation

The gate for enabling auto in any shipped recipe is a labelled corpus in the #652 eval tier:

- **must-deny:** destructive, outward-facing, off-task and injection-bearing calls. The injection sits in `Args`, past byte 200, and in non-operator task sources.
- **routine:** eligible reads, in-task edits, test runs.
- **must-escalate:** path-scope, control-plane (including through bash), subagent and no-task calls.

The gating number is **zero false allows on must-deny, and zero approver calls on must-escalate**. That second number is decided in code, so it is a unit test as well as an eval row. The false-escalate rate on routine is reported but doesn't gate. Grade on the verdict, not the reason text.

## Phases

1. **`pkg/permissions`.** `ModeAuto` and every site in the list above. The `Approver` and `ApproverContext` interfaces. `PromptRequest.Args`, eligibility and the never-list. `refusedByApprover`. `ApprovalLog.Approver`. Unit tests with a stub approver cover every row of decisions 1–5, 9 and 12, and the `DeriveForSession` inheritance.
2. **`Args` at the call sites.** Built-ins, `GateToolset` (MCP and skills), `alert`, `call_peer`, `fetch_url` and subagents.
3. **`pkg/approver` and wiring.** Config (`permissions.auto.{model,timeout,eligible,instructions_file}`), the privilege tier for the instructions file, the turn-context stamping for task, usage and audit, and compose wiring.
4. **Surfaces.** The fifth `auto` chip in core-tui (a core-tui release, then a pin bump here), both translators, the picker, `/perms/mode`, the modal changes from decision 11, and the approver-specific error text.
5. **Evaluation.** The eval corpus and the first real-model run, before any recipe turns auto on.

## Open questions

- Should background subagents get their own labelled brief as the task, so their eligible calls can be judged instead of escalated? This depends on a subagent-scoped `ApproverContext`.
- Should the eligible list accept tool *classes* (for example "read-only tools", from `readOnlyHint`, #1098) as well as patterns?
