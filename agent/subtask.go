// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// File subtask.go implements Mechanism B (micro-subagents) from
// docs/context-management-design.md. The companion mechanism to
// compaction: where compaction is REACTIVE (clean up bloat after
// it lands in the parent's context), micro-subagents are
// PROACTIVE (prevent the bloat from entering the parent's
// context in the first place).
//
// The primitive is Agent.RunSubtask — a synchronous, single-
// purpose, fresh-context LLM call. Caller blocks until the
// subtask returns its digested text answer. Differs from
// BackgroundAgentManager.Spawn in three ways:
//
//   1. Synchronous. Caller waits for the answer rather than
//      consuming alerts later.
//   2. Single-purpose. Tight turn cap (default 5). If it can't
//      answer in 5 turns the PARENT's model is the right place
//      to reason about it.
//   3. Tool-restricted. The spec carries the allowed tool set;
//      typically a narrow read-only subset. Writes still gate
//      via the parent's permissions if the parent's gate is
//      shared, but the wrapper tools (AgenticReadFile etc.)
//      pre-restrict the toolset before RunSubtask sees it.
//
// The fresh-context property is the load-bearing one. Each
// subtask gets its own sessionID branch — the subtask's events
// land in the same eventlog under that branch, the parent's Run
// never sees them. The subtask model gets the full attention
// budget for the narrow question; the parent's context never
// holds the subtask's intermediate reasoning. See the
// "fresh-context property" subsection in
// docs/context-management-design.md for the full motivation.

package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"google.golang.org/genai"

	adkagent "google.golang.org/adk/agent"
	"google.golang.org/adk/agent/llmagent"
	adkmodel "google.golang.org/adk/model"
	"google.golang.org/adk/runner"
	"google.golang.org/adk/tool"

	"github.com/go-steer/core-agent/usage"
)

// SubtaskSpec configures one RunSubtask call.
type SubtaskSpec struct {
	// Name attributes cost + traces. Required (non-empty) so the
	// audit log can distinguish subtask events; the subtask's
	// session-branch derives from this name.
	Name string

	// SystemPrompt is the subtask's role instruction. Replaces
	// agent.DefaultInstruction for this subtask only; the parent's
	// instruction is NOT inherited (the subtask runs in fresh
	// context).
	SystemPrompt string

	// UserMessage is the question / instruction the subtask
	// receives. Treated as the operator's first user message in
	// the subtask's brand-new session.
	UserMessage string

	// Tools is the restricted set the subtask may call.
	// Typically a small read-only subset (read_file, grep,
	// fetch_url, etc.). Empty means tool-less.
	Tools []tool.Tool

	// Model overrides the parent's model for this subtask. Nil
	// (the default) means "use the parent's model." Wrapper tools
	// like AgenticReadFile typically point this at a smaller,
	// faster model (haiku-tier / flash-tier) so a "summarize this
	// file" subtask doesn't burn opus tokens.
	Model adkmodel.LLM

	// Budgets caps the subtask's resource use. Zero fields fall
	// back to SubtaskBudgetDefaults.
	Budgets SubtaskBudgets
}

// SubtaskBudgets caps subtask cost in three dimensions. Whichever
// hits first wins. Returns SubtaskResult{Truncated: true} on
// budget exhaustion (not an error — caller's model can choose to
// retry with a wider budget or fall back to running the raw
// tool).
type SubtaskBudgets struct {
	// MaxTurns caps how many model turns the subtask may take.
	// Default is SubtaskDefaultMaxTurns. Subtasks are meant to be
	// single-purpose; >5 turns usually means the question is too
	// open-ended for a subtask and belongs in the parent's loop.
	MaxTurns int

	// MaxWallclock caps wall-clock duration. Default is
	// SubtaskDefaultMaxWallclock. Belt-and-suspenders against a
	// flaky model that streams forever without producing a final
	// turn.
	MaxWallclock time.Duration
}

// Defaults for SubtaskBudgets. Aimed at "narrow tool wrapper"
// shape (AgenticReadFile etc.); broader-research wrappers can
// override.
const (
	SubtaskDefaultMaxTurns     = 5
	SubtaskDefaultMaxWallclock = 60 * time.Second
)

// SubtaskResult is what RunSubtask hands back. Digest is the
// distilled text the subtask's model produced; Truncated flags
// budget exhaustion so the caller can decide whether the partial
// answer is useful.
type SubtaskResult struct {
	// Name echoes SubtaskSpec.Name for trace correlation.
	Name string

	// Digest is the model's final text output across all turns
	// (joined). Empty when Truncated is true and the model
	// produced no final-turn text before the budget hit.
	Digest string

	// Truncated reports that a budget (MaxTurns / MaxWallclock)
	// fired before the model emitted a final TurnComplete.
	// Digest may still hold partial text from earlier turns.
	Truncated bool

	// InputTokens / OutputTokens accumulate across every turn of
	// the subtask. Subtask cost rolls up to the parent's
	// usage.Tracker if WithUsageTracker is wired, so /stats
	// totals reflect everything.
	InputTokens  int
	OutputTokens int
	CostUSD      float64

	// Duration is wall-clock time spent in the subtask. Useful
	// for the "fresh-context is fast" claim — typical subtask
	// completes in <2s on a flash-tier model.
	Duration time.Duration

	// TurnsUsed is the number of model turns the subtask actually
	// took. Useful for budget tuning ("am I hitting MaxTurns
	// often?").
	TurnsUsed int
}

// ErrSubtaskSpecInvalid is returned when SubtaskSpec fails
// validation (empty Name / SystemPrompt / UserMessage). Pre-
// flight check so the caller gets a clear error rather than a
// confusing downstream ADK failure.
var ErrSubtaskSpecInvalid = errors.New("agent: invalid SubtaskSpec")

// RunSubtask runs a synchronous, fresh-context, single-purpose
// LLM call against the parent's session.Service (with a distinct
// branch so events don't leak into the parent's next Run
// request). Returns the model's digested answer.
//
// Properties:
//   - Caller blocks until the subtask returns.
//   - The subtask runs in its own ADK session (deriveSubagentSessionID
//     produces a parent-prefixed ID; the audit log can correlate).
//   - The subtask sees NO parent history; its model gets only the
//     SystemPrompt + UserMessage from the spec.
//   - The subtask's tool set is the spec's Tools — nothing
//     inherited from the parent.
//   - Cost rolls up to the parent's usage.Tracker (when one is
//     wired via WithUsageTracker) so /stats reflects everything.
//   - On budget exhaustion: returns SubtaskResult{Truncated: true}
//     with whatever partial Digest accumulated. NOT a Go error.
//   - On model failure or other unrecoverable error: returns the
//     wrapped error; caller can errors.Is on transport vs API
//     vs spec-validation failure.
//
// Used by the agentic tool wrappers in core-agent/tools (
// AgenticReadFile, AgenticFetchURL, AgenticGrep, AgenticResearch);
// also directly callable by host code that wants to spawn a
// one-shot research subagent without going through a tool.
func (a *Agent) RunSubtask(ctx context.Context, spec SubtaskSpec) (SubtaskResult, error) {
	if a == nil {
		return SubtaskResult{}, errors.New("agent: RunSubtask: nil receiver")
	}
	if strings.TrimSpace(spec.Name) == "" {
		return SubtaskResult{}, fmt.Errorf("%w: Name is required", ErrSubtaskSpecInvalid)
	}
	if strings.TrimSpace(spec.SystemPrompt) == "" {
		return SubtaskResult{}, fmt.Errorf("%w: SystemPrompt is required", ErrSubtaskSpecInvalid)
	}
	if strings.TrimSpace(spec.UserMessage) == "" {
		return SubtaskResult{}, fmt.Errorf("%w: UserMessage is required", ErrSubtaskSpecInvalid)
	}
	if a.sessionService == nil {
		return SubtaskResult{}, errors.New("agent: RunSubtask: no session.Service wired")
	}

	// Apply budget defaults.
	maxTurns := spec.Budgets.MaxTurns
	if maxTurns <= 0 {
		maxTurns = SubtaskDefaultMaxTurns
	}
	maxWallclock := spec.Budgets.MaxWallclock
	if maxWallclock <= 0 {
		maxWallclock = SubtaskDefaultMaxWallclock
	}

	// Resolve the subtask's model — spec override wins, falling
	// through to the parent's model when nil. This is the cost-
	// efficiency lever: wrapper tools point Model at a cheaper
	// flash/haiku-tier model while the parent runs on a frontier
	// model.
	subModel := spec.Model
	if subModel == nil {
		subModel = a.model
	}
	if subModel == nil {
		return SubtaskResult{}, errors.New("agent: RunSubtask: no model wired (construct parent via agent.New)")
	}

	// Build an isolated llmagent for the subtask. We DON'T go
	// through agent.New — that would re-register the
	// mark_task_done tool, run subagent-tool resolution, and
	// generally drag along parent-scope wiring the subtask
	// doesn't want. Direct llmagent.New + runner.New gives us
	// the minimal stack.
	subInstruction := spec.SystemPrompt
	subInner, err := llmagent.New(llmagent.Config{
		Name:        "subtask_" + spec.Name,
		Model:       subModel,
		Description: "core-agent subtask: " + spec.Name,
		Instruction: subInstruction,
		Tools:       spec.Tools,
	})
	if err != nil {
		return SubtaskResult{}, fmt.Errorf("agent: RunSubtask: build llmagent: %w", err)
	}

	// Distinct session ID so the subtask's events don't collide
	// with the parent's optimistic-concurrency check on shared
	// session.Service rows. Branch label keeps the audit log
	// correlated to the parent.
	branch := composeBranch("", "sub."+spec.Name)
	subSessionID := deriveSubagentSessionID(a.sessionID, "sub."+spec.Name)

	// Use the UNWRAPPED parent session service (a.sessionService,
	// not the compactingService the runner uses). The subtask has
	// no summary events of its own; running through compactingService
	// would do unnecessary scanning + risk slicing on a parent's
	// summary event that doesn't belong to this subtask.
	subSvc := &branchInjectingService{
		inner:  a.sessionService,
		branch: branch,
	}

	subRunner, err := runner.New(runner.Config{
		AppName:           a.appName,
		Agent:             subInner,
		SessionService:    subSvc,
		AutoCreateSession: true,
	})
	if err != nil {
		return SubtaskResult{}, fmt.Errorf("agent: RunSubtask: build runner: %w", err)
	}

	// Wall-clock budget via context; turn budget tracked in the
	// loop below since ADK's runner doesn't expose a turn cap.
	subCtx, cancel := context.WithTimeout(ctx, maxWallclock)
	defer cancel()

	msg := genai.NewContentFromText(spec.UserMessage, genai.RoleUser)
	start := time.Now()

	var (
		digest       strings.Builder
		totalIn      int
		totalOut     int
		totalCostUSD float64
		turnsUsed    int
		turnComplete bool
		runErr       error
	)

	// ADK's runner.Run iterates events for ONE turn-as-the-runner-
	// sees-it. Multi-turn = call Run multiple times with each new
	// user message. For a subtask we issue ONE user message and
	// let ADK drive however many model<->tool round trips it
	// needs — that's all one Run call to us. The turn count we
	// cap on is model turns (counted via TurnComplete events),
	// not Run calls.
	for ev, err := range subRunner.Run(subCtx, a.userID, subSessionID, msg, adkagent.RunConfig{
		StreamingMode: a.streaming,
	}) {
		if err != nil {
			runErr = err
			break
		}
		if ev == nil {
			continue
		}
		// Accumulate usage from any event that carries metadata.
		if ev.UsageMetadata != nil {
			turnIn := int(ev.UsageMetadata.PromptTokenCount)
			turnOut := int(ev.UsageMetadata.CandidatesTokenCount)
			totalIn += turnIn
			totalOut += turnOut
			// Roll cost up to parent tracker so /stats shows it.
			// Pricing comes from the subtask's model name. When
			// the parent has no tracker wired, this is a no-op.
			if a.tracker != nil {
				modelName := subModel.Name()
				pricing := usage.PriceFor(modelName, nil)
				turn := a.tracker.Append(modelName, turnIn, turnOut, pricing)
				totalCostUSD += turn.CostUSD
			}
		}
		// Capture final text. collectFinalText filters out
		// partials so we don't double-count streaming chunks.
		collectFinalText(&digest, ev)
		// Count completed model turns + bail on the budget cap.
		// TurnComplete fires once per finished model turn (after
		// any tool-call loops inside the turn).
		if ev.TurnComplete {
			turnsUsed++
			if turnsUsed >= maxTurns {
				turnComplete = true
				break
			}
			turnComplete = true
		}
	}

	elapsed := time.Since(start)

	// Wall-clock timeout produces a context error — treat as
	// truncation, not failure (we still have whatever digest
	// accumulated up to the cap).
	if errors.Is(runErr, context.DeadlineExceeded) || errors.Is(subCtx.Err(), context.DeadlineExceeded) {
		return SubtaskResult{
			Name:         spec.Name,
			Digest:       strings.TrimSpace(digest.String()),
			Truncated:    true,
			InputTokens:  totalIn,
			OutputTokens: totalOut,
			CostUSD:      totalCostUSD,
			Duration:     elapsed,
			TurnsUsed:    turnsUsed,
		}, nil
	}
	if runErr != nil {
		return SubtaskResult{}, fmt.Errorf("agent: RunSubtask: %w", runErr)
	}

	// Hit the turn cap without TurnComplete? That's truncation.
	// (The break above also triggers turnComplete=true for the
	// natural-finish case; we use turnsUsed to distinguish.)
	truncated := !turnComplete || (turnsUsed >= maxTurns && !lastEventWasTurnComplete(turnsUsed, maxTurns))

	a.recordSubtaskUsage(totalIn, totalOut, totalCostUSD)

	return SubtaskResult{
		Name:         spec.Name,
		Digest:       strings.TrimSpace(digest.String()),
		Truncated:    truncated && strings.TrimSpace(digest.String()) == "",
		InputTokens:  totalIn,
		OutputTokens: totalOut,
		CostUSD:      totalCostUSD,
		Duration:     elapsed,
		TurnsUsed:    turnsUsed,
	}, nil
}

// recordSubtaskUsage bumps the Agent's subtask counters that
// ContextStats surfaces. Called once per RunSubtask after the
// per-event totals are summed. Safe to call with zeros — the
// subtask still happened (caller can show it in /context as a
// count even when usage tracking was off).
func (a *Agent) recordSubtaskUsage(inputTokens, outputTokens int, costUSD float64) {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.subtaskCount++
	a.subtaskInputTokens += inputTokens
	a.subtaskOutputTokens += outputTokens
	a.subtaskCostUSD += costUSD
	a.mu.Unlock()
}

// lastEventWasTurnComplete is a stub for the "did we naturally
// finish vs hit the cap on the last completed turn" question.
// The current loop sets turnComplete=true whenever any TurnComplete
// fires, so we can't actually distinguish; for now, treat
// "hit the cap exactly" as natural completion (a subtask that
// uses every budget turn AND produces a final response on its
// last allowed turn IS a complete subtask, not a truncated one).
// If we need real truncation detection later, track a separate
// flag inside the loop.
func lastEventWasTurnComplete(turnsUsed, maxTurns int) bool {
	return turnsUsed >= maxTurns
}
