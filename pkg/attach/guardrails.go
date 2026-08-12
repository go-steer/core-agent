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

// Guardrail state + reset wire contract (#666, resolving the design
// questions in #331).
//
// Two runtime backstops can halt a session: the behavioral watchdog
// (enforce mode) and the cost ceiling (per-turn / per-session). Both
// have had in-process Reset methods since they shipped, and neither had
// an operator-reachable caller — the only recovery was restarting the
// process. #642 then made watchdog=enforce and a $10 session ceiling
// the default for every unattended run, so a trip became something an
// operator hits without having asked for it.
//
// The contract here is deliberately small:
//
//   - GET  /sessions/{id}/guardrails       — what is armed, what tripped, why
//   - POST /sessions/{id}/guardrails/reset — clear it, optionally with budget
//
// "Reset" means exactly one thing: clear the trip flag. The optional
// additional_budget_usd RAISES the per-session ceiling; it never zeroes
// or re-windows the accumulator. Of the three interpretations #331
// weighed (bump / restart-window / wipe), bump is the only one that
// doesn't make the session's reported spend disagree with what it
// actually spent — /usage, the eventlog-derived cost, and the ceiling
// check all keep counting the same dollars.
//
// A reset that would provably not work is refused rather than
// performed: a per-session trip whose accumulator is already at or past
// the ceiling gets 409 with the numbers, not a 200 that re-trips on the
// operator's next turn.

package attach

import "errors"

// ErrGuardrailRetrip is returned by GuardrailResetter implementations
// when the requested reset would be undone by the next turn — a
// per-session cost trip whose accumulated spend is already at or past
// the ceiling, with no additional budget supplied. The handler maps it
// to 409 Conflict: the request was well-formed and the operator is
// authorized, but the state makes it a no-op, and a 200 that silently
// achieves nothing is exactly the failure mode this endpoint exists to
// remove. The error text carries the spend and the ceiling so the
// operator knows how much budget to add.
var ErrGuardrailRetrip = errors.New("attach: reset would immediately re-trip; additional budget required")

// Guardrail names accepted by GuardrailResetRequest.Guardrail and
// reported in GuardrailResetResponse.Reset.
const (
	// GuardrailWatchdog is the behavioral watchdog (enforce mode).
	GuardrailWatchdog = "watchdog"
	// GuardrailCostCeiling is the per-turn / per-session spend cap.
	GuardrailCostCeiling = "cost_ceiling"
	// GuardrailAll targets every guardrail that is currently tripped.
	// The default when the request names none.
	GuardrailAll = "all"
)

// GuardrailInfo is the response shape of GET /sessions/.../guardrails —
// the operator-facing answer to "why is this session refusing my
// prompts, and what do I do about it?"
type GuardrailInfo struct {
	Watchdog    WatchdogInfo    `json:"watchdog"`
	CostCeiling CostCeilingInfo `json:"cost_ceiling"`
	// Halted is true when at least one guardrail has tripped, so a
	// client can render the banner without knowing which backstops
	// exist.
	Halted bool `json:"halted"`
}

// WatchdogInfo reports the watchdog's configured posture and whether it
// has halted the session.
type WatchdogInfo struct {
	// Mode is the resolved watchdog mode: "off", "warn" or "enforce".
	// Only enforce can halt a session.
	Mode string `json:"mode"`
	// Tripped is true when a runaway pattern halted the agent and the
	// operator hasn't reset it.
	Tripped bool `json:"tripped"`
	// Reason is the operator-facing explanation of the trip. Empty
	// when Tripped is false.
	Reason string `json:"reason,omitempty"`
}

// CostCeilingInfo reports the configured spend caps, the session's
// spend against them, and whether a cap has halted the session.
type CostCeilingInfo struct {
	// MaxTurnUSD / MaxSessionUSD are the caps in force. 0 means that
	// bound is disabled. MaxSessionUSD reflects any budget added by a
	// prior reset, not just the configured startup value.
	MaxTurnUSD    float64 `json:"max_turn_usd"`
	MaxSessionUSD float64 `json:"max_session_usd"`
	// SessionCostUSD is the session's cumulative spend — the same
	// accumulator a per-session trip is measured against.
	SessionCostUSD float64 `json:"session_cost_usd"`
	Tripped        bool    `json:"tripped"`
	Reason         string  `json:"reason,omitempty"`
	// WouldRetrip is true when clearing the flag without additional
	// budget would be undone by the next turn's enforcement pass
	// (spend already >= MaxSessionUSD). Clients use it to require a
	// budget input before offering the reset.
	WouldRetrip bool `json:"would_retrip"`
}

// GuardrailResetRequest is the body of POST
// /sessions/.../guardrails/reset.
type GuardrailResetRequest struct {
	// Guardrail selects what to clear: "watchdog", "cost_ceiling", or
	// "all" (the default when empty).
	Guardrail string `json:"guardrail,omitempty"`
	// AdditionalBudgetUSD raises the per-session ceiling by this many
	// dollars as part of the reset. Required when the per-session
	// ceiling is what tripped and the accumulator is already at or
	// past it — otherwise the reset provably re-trips. Rejected (not
	// silently dropped) when Guardrail is "watchdog": budget has no
	// meaning there, and quietly discarding it would let an operator
	// believe they'd bought runway they hadn't.
	AdditionalBudgetUSD float64 `json:"additional_budget_usd,omitempty"`
}

// GuardrailResetResponse reports what the reset actually did. The
// post-reset state is echoed so a client needs no follow-up GET.
type GuardrailResetResponse struct {
	// Reset names the guardrails whose trip flag this call cleared.
	// Empty when nothing was tripped — not an error: a defensive
	// reset is legitimate.
	Reset []string `json:"reset"`
	// BudgetAddedUSD is the amount added to the per-session ceiling
	// (0 when none was requested).
	BudgetAddedUSD float64 `json:"budget_added_usd,omitempty"`
	// Guardrails is the post-reset state, same shape as GET.
	Guardrails GuardrailInfo `json:"guardrails"`
	// Message is the operator-facing one-liner a TUI renders.
	Message string `json:"message,omitempty"`
}

// GuardrailProvider is the optional capability for
// GET /sessions/.../guardrails. Absence reports zero-value state
// rather than 501, matching the other read-only projections.
type GuardrailProvider interface {
	AttachGuardrails() GuardrailInfo
}

// GuardrailResetter is the optional capability for
// POST /sessions/.../guardrails/reset. Unlike the read side, absence is
// a 501: an operator who POSTs a reset must know whether it took
// effect.
//
// Implementations return an error for a reset that cannot work —
// notably a per-session cost trip with no additional budget, which the
// handler translates to 409 rather than 500 (see ErrGuardrailRetrip).
type GuardrailResetter interface {
	AttachResetGuardrail(req GuardrailResetRequest) (GuardrailResetResponse, error)
}
