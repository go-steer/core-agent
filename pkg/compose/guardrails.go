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

package compose

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/go-steer/core-agent/v3/pkg/agent"
	"github.com/go-steer/core-agent/v3/pkg/agent/background"
	"github.com/go-steer/core-agent/v3/pkg/config"
	"github.com/go-steer/core-agent/v3/pkg/usage"
	"github.com/go-steer/core-agent/v3/pkg/watchdog"
)

// DefaultUnattendedSessionCostUSD is the session spend ceiling applied
// when an unattended run leaves both --max-session-cost-usd and
// agent.max_session_cost_usd unset (#642, #1369).
//
// The value is a backstop, not a budget: it is high enough that real
// autonomous work finishes under it and low enough that a drifting
// session stops before it becomes an invoice nobody chose. Operators
// who want a different number set one; operators who deliberately want
// no ceiling pass --max-session-cost-usd=0 or set
// agent.max_session_cost_usd to 0 in config.
const DefaultUnattendedSessionCostUSD = 10.0

// DefaultUnattendedApprovalTimeout is the permission-prompt deadline
// applied when an unattended run with a prompt surface (e.g.
// --attach-listen) runs in permissions.mode "ask" and leaves both
// --approval-timeout and permissions.approval_timeout unset (#1364, #1369).
//
// Without a finite deadline, an unanswered permission prompt holds the
// session's turnLock and wake-loop goroutine open forever. Operators
// who want a different bound set one; operators who deliberately want
// to wait indefinitely pass --approval-timeout=0s or set
// permissions.approval_timeout to "0s".
const DefaultUnattendedApprovalTimeout = 30 * time.Minute

// Sources reported in GuardrailResolution, kept as constants so the
// startup summary and tests assert on the same strings.
const (
	WatchdogSourceFlag               = "--watchdog flag"
	WatchdogSourceConfig             = "safety.watchdog config"
	WatchdogSourceUnattendedDefault  = "unattended default"
	WatchdogSourceInteractiveDefault = "interactive default"

	SessionCostSourceFlag              = "--max-session-cost-usd flag"
	SessionCostSourceConfig            = "agent.max_session_cost_usd config"
	SessionCostSourceUnattendedDefault = "unattended default"
	SessionCostSourceUnset             = "unset"

	ApprovalTimeoutSourceFlag              = "--approval-timeout flag"
	ApprovalTimeoutSourceConfig            = "permissions.approval_timeout config"
	ApprovalTimeoutSourceUnattendedDefault = "unattended default"
	ApprovalTimeoutSourceUnset             = "unset"
)

// GuardrailInputs holds the configuration and run-shape facts needed by
// ResolveGuardrails to decide the runaway-backstop posture (#1369).
type GuardrailInputs struct {
	// WatchdogFlag is --watchdog. Empty == operator left it unset.
	WatchdogFlag string
	// WatchdogConfig is safety.watchdog. Empty == unset.
	WatchdogConfig string

	// SessionCostFlag is --max-session-cost-usd, and SessionCostFlagSet
	// reports whether the operator actually passed it. The pair matters
	// because 0 is a meaningful explicit value ("no ceiling") that must
	// be distinguishable from the flag's zero default.
	SessionCostFlag    float64
	SessionCostFlagSet bool
	// SessionCostConfig is agent.max_session_cost_usd; nil == unset.
	SessionCostConfig *float64

	// Unattended reports that no operator is watching this run's
	// output: -p one-shot, a --no-repl daemon, or a non-TTY stdin.
	// Observe-and-log is not a backstop when nobody is reading the log.
	Unattended bool

	// ApprovalTimeoutFlag is --approval-timeout; empty == unset.
	ApprovalTimeoutFlag string
	// ApprovalTimeoutConfig is permissions.approval_timeout; empty == unset.
	ApprovalTimeoutConfig string
	// PermMode is the resolved permissions.mode (empty defaults to "ask").
	PermMode string
	// HasPromptSurface reports whether a permission prompt can be
	// delivered to a prompter (e.g. an attach listener's PromptBroker
	// or a TTY prompter). When true on an unattended run in "ask"
	// mode, an unset approval_timeout defaults to
	// DefaultUnattendedApprovalTimeout (#1364).
	HasPromptSurface bool
}

// GuardrailResolution is the decided posture plus, for each knob, the
// reason it came out that way — the startup summary prints the reason
// so an operator can tell "the default did this" from "my config did
// this" without re-deriving the precedence chain (#1369).
type GuardrailResolution struct {
	// Watchdog is one of config.WatchdogOff / WatchdogWarn /
	// config.WatchdogFeedback / config.WatchdogEnforce. Never empty.
	Watchdog       string
	WatchdogSource string

	// SessionCostUSD is the resolved session ceiling; 0 == disabled.
	SessionCostUSD    float64
	SessionCostSource string

	// ApprovalTimeout is the resolved permission-prompt deadline; 0 ==
	// wait indefinitely.
	ApprovalTimeout       time.Duration
	ApprovalTimeoutSource string
}

// ResolveGuardrailsFromConfig resolves the runaway-backstop posture from
// a parsed *config.Config and run-shape facts (unattended, hasPromptSurface),
// without requiring CLI flag inputs (#1369).
func ResolveGuardrailsFromConfig(cfg *config.Config, unattended, hasPromptSurface bool) (GuardrailResolution, error) {
	var in GuardrailInputs
	in.Unattended = unattended
	in.HasPromptSurface = hasPromptSurface
	if cfg != nil {
		in.WatchdogConfig = cfg.Safety.Watchdog
		in.SessionCostConfig = cfg.Agent.MaxSessionCostUSD
		in.ApprovalTimeoutConfig = cfg.Permissions.ApprovalTimeout
		in.PermMode = cfg.Permissions.Mode
	}
	return ResolveGuardrails(in)
}

// ResolveGuardrails decides the watchdog mode, session cost ceiling, and
// approval timeout for a run (#642, #1364, #1369).
//
// Watchdog precedence: --watchdog > safety.watchdog > mode default.
// The mode default is "enforce" when unattended and "warn" otherwise.
//
// Session-ceiling precedence: --max-session-cost-usd (including an
// explicit 0) > agent.max_session_cost_usd (including an explicit 0) >
// DefaultUnattendedSessionCostUSD when unattended > disabled.
//
// Approval-timeout precedence: --approval-timeout >
// permissions.approval_timeout > DefaultUnattendedApprovalTimeout
// (when unattended in "ask" mode with a prompt surface) > unset (0).
func ResolveGuardrails(in GuardrailInputs) (GuardrailResolution, error) {
	res := GuardrailResolution{}

	norm := func(s string) string { return strings.ToLower(strings.TrimSpace(s)) }
	valid := func(s string) bool {
		switch s {
		case config.WatchdogOff, config.WatchdogWarn, config.WatchdogFeedback, config.WatchdogEnforce:
			return true
		}
		return false
	}
	modes := strings.Join([]string{config.WatchdogOff, config.WatchdogWarn, config.WatchdogFeedback, config.WatchdogEnforce}, "|")

	switch flagMode, cfgMode := norm(in.WatchdogFlag), norm(in.WatchdogConfig); {
	case flagMode != "":
		if !valid(flagMode) {
			return res, fmt.Errorf("invalid --watchdog mode %q (want %s)", in.WatchdogFlag, modes)
		}
		res.Watchdog, res.WatchdogSource = flagMode, WatchdogSourceFlag
	case cfgMode != "":
		if !valid(cfgMode) {
			return res, fmt.Errorf("invalid safety.watchdog %q (want %s)", in.WatchdogConfig, modes)
		}
		res.Watchdog, res.WatchdogSource = cfgMode, WatchdogSourceConfig
	case in.Unattended:
		res.Watchdog, res.WatchdogSource = config.WatchdogEnforce, WatchdogSourceUnattendedDefault
	default:
		res.Watchdog, res.WatchdogSource = config.WatchdogWarn, WatchdogSourceInteractiveDefault
	}

	switch {
	case in.SessionCostFlagSet:
		res.SessionCostUSD, res.SessionCostSource = in.SessionCostFlag, SessionCostSourceFlag
	case in.SessionCostConfig != nil:
		res.SessionCostUSD, res.SessionCostSource = *in.SessionCostConfig, SessionCostSourceConfig
	case in.Unattended:
		res.SessionCostUSD, res.SessionCostSource = DefaultUnattendedSessionCostUSD, SessionCostSourceUnattendedDefault
	default:
		res.SessionCostUSD, res.SessionCostSource = 0, SessionCostSourceUnset
	}
	if res.SessionCostUSD < 0 {
		res.SessionCostUSD = 0
	}

	mode := strings.TrimSpace(in.PermMode)
	if mode == "" {
		mode = config.PermissionModeAsk
	}
	parseApprovalDur := func(raw, label string) (time.Duration, error) {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return 0, fmt.Errorf("invalid %s %q: %w (want a Go duration like \"30m\" or \"0s\")", label, raw, err)
		}
		if d < 0 {
			return 0, fmt.Errorf("invalid %s %q: duration must not be negative (use \"0s\" to wait indefinitely)", label, raw)
		}
		return d, nil
	}
	switch flagDur, cfgDur := strings.TrimSpace(in.ApprovalTimeoutFlag), strings.TrimSpace(in.ApprovalTimeoutConfig); {
	case flagDur != "":
		d, err := parseApprovalDur(flagDur, "--approval-timeout")
		if err != nil {
			return res, err
		}
		res.ApprovalTimeout, res.ApprovalTimeoutSource = d, ApprovalTimeoutSourceFlag
	case cfgDur != "":
		d, err := parseApprovalDur(cfgDur, "permissions.approval_timeout")
		if err != nil {
			return res, err
		}
		res.ApprovalTimeout, res.ApprovalTimeoutSource = d, ApprovalTimeoutSourceConfig
	case in.Unattended && mode == config.PermissionModeAsk && in.HasPromptSurface:
		res.ApprovalTimeout, res.ApprovalTimeoutSource = DefaultUnattendedApprovalTimeout, ApprovalTimeoutSourceUnattendedDefault
	default:
		res.ApprovalTimeout, res.ApprovalTimeoutSource = 0, ApprovalTimeoutSourceUnset
	}

	return res, nil
}

// AppendCostAndWallclockOpts appends the resolved cost ceiling, wallclock
// bound, max-steps cap, and final-report options to opts, emitting the
// startup cost-ceiling line via send when send is non-nil (#1369).
func AppendCostAndWallclockOpts(opts []agent.Option, cfg *config.Config, ceiling agent.CostCeiling, sessionCostSource string, send func(string)) []agent.Option {
	if send == nil {
		send = func(string) {}
	}
	if ceiling.MaxTurnUSD > 0 || ceiling.MaxSessionUSD > 0 {
		opts = append(opts, agent.WithCostCeiling(ceiling))
		sessionCeiling := "disabled"
		if ceiling.MaxSessionUSD > 0 {
			sessionCeiling = fmt.Sprintf("$%.4f", ceiling.MaxSessionUSD)
		}
		send(fmt.Sprintf("cost ceiling: per-turn=$%.4f per-session=%s [session ceiling from %s] (refuses new turns when exceeded; clear with /guardrail reset, or POST /sessions/{id}/guardrails/reset — a per-session trip needs +budget)", ceiling.MaxTurnUSD, sessionCeiling, sessionCostSource))
	}
	if cfg != nil && cfg.Agent.MaxWallclockSeconds > 0 {
		opts = append(opts, agent.WithMaxWallclock(time.Duration(cfg.Agent.MaxWallclockSeconds)*time.Second))
	}
	if cfg != nil && cfg.Agent.MaxSteps > 0 {
		opts = append(opts, agent.WithMaxSteps(cfg.Agent.MaxSteps))
	}
	if cfg != nil && cfg.Agent.FinalReport {
		opts = append(opts, agent.WithFinalReport(true))
	}
	return opts
}

// AppendWatchdogOpts appends the resolved behavioral watchdog options
// (warn, feedback, enforce) to opts and emits the startup watchdog line
// via send when send is non-nil (#1369).
func AppendWatchdogOpts(opts []agent.Option, guard GuardrailResolution, send func(string)) []agent.Option {
	if send == nil {
		send = func(string) {}
	}
	if guard.Watchdog == config.WatchdogOff {
		send(fmt.Sprintf("watchdog: off [%s] (no runaway-pattern observation)", guard.WatchdogSource))
		return opts
	}
	w := watchdog.NewDefaultWatchdog()
	opts = append(opts, agent.WithWatchdog(w, func(a watchdog.Alert) {
		send(fmt.Sprintf("watchdog %s", a.String()))
	}))
	detail := "observes tool-call stream; logs structured alerts on runaway patterns"
	if guard.Watchdog == config.WatchdogFeedback || guard.Watchdog == config.WatchdogEnforce {
		opts = append(opts, agent.WithWatchdogFeedback())
		detail += "; injects the observation into the model's next turn"
	}
	if guard.Watchdog == config.WatchdogEnforce {
		opts = append(opts, agent.WithWatchdogEnforce())
		detail += "; halts the turn in flight and refuses new ones until cleared with /guardrail reset, or POST /sessions/{id}/guardrails/reset"
	}
	send(fmt.Sprintf("watchdog: %s mode [%s] (%s)", guard.Watchdog, guard.WatchdogSource, detail))
	return opts
}

// CeilingPricing decides whether a cost ceiling can price a model: at
// startup, and at each door that picks a model later (/model, a
// background spawn). An unpriced model costs $0 to every ceiling, so the
// ceiling can never trip on it (#1299, #1327, #1369).
type CeilingPricing struct {
	Ceiling       agent.CostCeiling
	SessionSource string
}

// OperatorSet reports whether any active ceiling is one the operator
// asked for, as opposed to only the unattended default.
func (p CeilingPricing) OperatorSet() bool {
	return p.Ceiling.MaxTurnUSD > 0 || (p.Ceiling.MaxSessionUSD > 0 && p.SessionSource != SessionCostSourceUnattendedDefault)
}

// Check vets one model. who names it in the message ("model",
// `subagent "cluster"`). budget is that subagent's own max_cost_usd, 0
// for none. A refusal comes back as err; a warning to print as warn.
func (p CeilingPricing) Check(who, model, backend string, budget float64) (warn string, err error) {
	if model == "" || !usage.PriceOn(backend, model).Unpriced {
		return "", nil
	}
	why := UnpricedCeilingWhy(who, model, backend)
	if budget > 0 {
		return "", fmt.Errorf("%s budgets.max_cost_usd ($%.2f) cannot hold: %s; or drop the budget", who, budget, why)
	}
	if p.Ceiling.MaxTurnUSD <= 0 && p.Ceiling.MaxSessionUSD <= 0 {
		return "", nil
	}
	if p.OperatorSet() {
		return "", fmt.Errorf("%s; or drop the ceiling", why)
	}
	return fmt.Sprintf("WARNING: %s. The unattended default session ceiling ($%.2f) is NOT protecting this run.", why, p.Ceiling.MaxSessionUSD), nil
}

// UnpricedCeilingWhy returns the human-readable diagnostic explaining why a
// cost ceiling or subagent budget cannot price model on backend (#1299, #1369).
func UnpricedCeilingWhy(who, model, backend string) string {
	where := ""
	if backend != "" {
		where = fmt.Sprintf(" on backend %q", backend)
	}
	return fmt.Sprintf("%s %q%s has no price — no pricing layer has a rate for it — so the cost ceiling counts its calls as $0 and can never trip. %s",
		who, model, where, unpricedRemedy)
}

const unpricedRemedy = "Set model.pricing for it (zero rates are fine there for a self-hosted model you don't pay per token for), " +
	"or declare real models[].rates in its provider profile"

// CeilingModel is one model a ceiling or budget has to price at startup.
type CeilingModel struct {
	Who, Model, Backend string
	Budget              float64
}

// StartupCeilingModels lists every model the configured ceilings reach:
// the main model, each declared subagent's, and the background small
// tier a spawn may downshift to (#1369).
func StartupCeilingModels(cfg *config.Config, templates []background.SubagentTemplate, smallModel string) []CeilingModel {
	mainBackend := usage.BackendFor(cfg)
	var mainModel string
	if cfg != nil {
		mainModel = cfg.Model.Name
	}
	out := []CeilingModel{{Who: "model", Model: mainModel, Backend: mainBackend}}
	for _, t := range templates {
		out = append(out, CeilingModel{
			Who:     fmt.Sprintf("subagent %q: model", t.Name),
			Model:   t.ModelID,
			Backend: t.Backend,
			Budget:  t.Budgets.MaxCost,
		})
	}
	if smallModel != "" && smallModel != mainModel {
		out = append(out, CeilingModel{Who: "background small-tier model", Model: smallModel, Backend: mainBackend})
	}
	return out
}

// CheckStartup runs Check over every model the ceilings reach. All
// refusals are reported at once, so a config with three unpriced
// subagents needs one fix, not three restarts.
func (p CeilingPricing) CheckStartup(ms []CeilingModel) (warns []string, err error) {
	var errs []error
	for _, m := range ms {
		w, e := p.Check(m.Who, m.Model, m.Backend, m.Budget)
		if e != nil {
			errs = append(errs, e)
		} else if w != "" {
			warns = append(warns, w)
		}
	}
	return warns, errors.Join(errs...)
}

// Door is Check for a model picked after startup (/model, a background
// spawn) under the ceilings this process runs with. A warning is printed
// to stderr and the model is allowed; only a refusal stops it.
func (p CeilingPricing) Door(who, model, backend string) error {
	warn, err := p.Check(who, model, backend, 0)
	if warn != "" {
		fmt.Fprintf(os.Stderr, "core-agent: %s\n", warn)
	}
	return err
}

const spawnDoorWho = "background spawn: model"

// SpawnDoor is Door for a background spawn, which also brings the
// subagent's own budget (background.WithSpawnPricingCheck).
func (p CeilingPricing) SpawnDoor(s background.SpawnPricing, warned *WarnOnce) (warns []string, err error) {
	budget := 0.0
	if s.MaxCostExplicit {
		budget = s.MaxCost
	}
	warn, err := p.Check(spawnDoorWho, s.ModelID, s.Backend, budget)
	if err != nil {
		return nil, err
	}
	if warn != "" {
		warns = append(warns, warn)
	}
	if !s.MaxCostExplicit && s.MaxCost > 0 && s.ModelID != "" && usage.PriceOn(s.Backend, s.ModelID).Unpriced && warned.First(s.Backend+"/"+s.ModelID) {
		warns = append(warns, unpricedBudgetWarning(s))
	}
	return warns, nil
}

func unpricedBudgetWarning(s background.SpawnPricing) string {
	where := ""
	if s.Backend != "" {
		where = fmt.Sprintf(" on backend %q", s.Backend)
	}
	return fmt.Sprintf("WARNING: %s %q%s has no price, so a background subagent's budgets.max_cost_usd ($%.2f) counts its calls as $0 and cannot stop it. %s. (Warned once per model.)",
		spawnDoorWho, s.ModelID, where, s.MaxCost, unpricedRemedy)
}

// WarnOnce reports the first time it sees each key. The zero value is
// ready to use; a nil *WarnOnce returns true without deduplication.
type WarnOnce struct {
	mu   sync.Mutex
	seen map[string]bool
}

// First reports whether key has not been seen before, recording it.
func (w *WarnOnce) First(key string) bool {
	if w == nil {
		return true
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.seen[key] {
		return false
	}
	if w.seen == nil {
		w.seen = map[string]bool{}
	}
	w.seen[key] = true
	return true
}

// AutoContinueResolution is the decided auto-continue enablement plus
// the parsed freshness window and retry-driver settings, produced by
// ResolveAutoContinue (#539, #559, #1369).
type AutoContinueResolution struct {
	Enabled       bool
	Freshness     time.Duration
	Retry         bool
	RetryInterval time.Duration
}

// ResolveAutoContinue decides whether restart-interrupted turns are
// auto-continued (#539, #559, #1369), given the parsed config block and
// the run context. The default — config left unset (nil block or nil
// enabled) — is ON when the feature can apply: a multi-session daemon
// or a --no-repl single-user daemon, with a durable eventlog. It is OFF,
// silently, otherwise, so interactive REPL/TUI runs are never surprised
// by unattended token spend.
func ResolveAutoContinue(ac *config.AutoContinueConfig, multiSession, noREPL, haveEventlog bool, stderr io.Writer) (AutoContinueResolution, error) {
	if stderr == nil {
		stderr = io.Discard
	}
	res := AutoContinueResolution{Freshness: time.Hour, RetryInterval: 5 * time.Minute}
	applies := (multiSession || noREPL) && haveEventlog

	explicit := ac != nil && ac.Enabled != nil
	want := applies
	if explicit {
		want = *ac.Enabled
	}
	if !want {
		return res, nil
	}
	if !applies {
		switch {
		case !multiSession && !noREPL:
			fmt.Fprintln(stderr, "core-agent: agent.auto_continue has no effect in this mode; it applies to multi-session daemons and --no-repl single-user daemons — ignoring")
		case !haveEventlog:
			fmt.Fprintln(stderr, "core-agent: agent.auto_continue requires --session-db (durable eventlog); ignoring")
		}
		return res, nil
	}

	res.Enabled = true
	if ac != nil && ac.Freshness != "" {
		d, err := time.ParseDuration(ac.Freshness)
		if err != nil {
			return res, fmt.Errorf("parse agent.auto_continue.freshness=%q: %w", ac.Freshness, err)
		}
		if d < 0 {
			return res, fmt.Errorf("agent.auto_continue.freshness=%q: must be >= 0 (\"0s\" disables the window)", ac.Freshness)
		}
		res.Freshness = d
	}
	res.Retry = ac == nil || ac.RetryEnabled()
	if ac != nil && ac.RetryInterval != "" {
		d, err := time.ParseDuration(ac.RetryInterval)
		if err != nil {
			return res, fmt.Errorf("parse agent.auto_continue.retry_interval=%q: %w", ac.RetryInterval, err)
		}
		if d <= 0 {
			return res, fmt.Errorf("agent.auto_continue.retry_interval=%q: must be > 0", ac.RetryInterval)
		}
		res.RetryInterval = d
	}

	if !explicit {
		fmt.Fprintln(stderr, "core-agent: auto_continue on by default (multi-session/--no-repl + durable eventlog); set agent.auto_continue.enabled=false to opt out")
	}
	return res, nil
}
