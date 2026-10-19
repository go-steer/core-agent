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

package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/go-steer/core-agent/v3/pkg/agent"
	"github.com/go-steer/core-agent/v3/pkg/agent/background"
	"github.com/go-steer/core-agent/v3/pkg/config"
	"github.com/go-steer/core-agent/v3/pkg/usage"
)

// DefaultUnattendedSessionCostUSD is the session spend ceiling applied
// when an unattended run leaves both --max-session-cost-usd and
// agent.max_session_cost_usd unset (#642).
//
// The value is a backstop, not a budget: it is high enough that real
// autonomous work finishes under it and low enough that a drifting
// session stops before it becomes an invoice nobody chose. Operators
// who want a different number set one; operators who deliberately want
// no ceiling pass --max-session-cost-usd=0 or set
// agent.max_session_cost_usd to 0 in config.
const DefaultUnattendedSessionCostUSD = 10.0

// guardrailOpts bundles the runaway-backstop CLI flags so run()'s
// signature carries one parameter instead of three, and so the
// "operator actually passed --max-session-cost-usd" bit travels with
// the value it qualifies.
type guardrailOpts struct {
	// watchdogMode is --watchdog; empty == unset.
	watchdogMode string
	// maxTurnCostUSD is --max-turn-cost-usd; 0 == unset (there is no
	// mode default for the per-turn ceiling, so 0 and "unset" mean the
	// same thing here and no set-bit is needed).
	maxTurnCostUSD float64
	// maxSessionCostUSD is --max-session-cost-usd, qualified by
	// maxSessionCostSet — 0 is a meaningful explicit value.
	maxSessionCostUSD float64
	maxSessionCostSet bool
	// bashSearchGate is --bash-search-gate; empty == unset, which
	// leaves config (then the "enforce" default) to decide. Unlike
	// the watchdog this has no mode-dependent resolution: bash-as-grep
	// is the wrong call whether or not an operator is watching.
	bashSearchGate string
}

// flagWasSet reports whether name was present on the command line, as
// opposed to sitting at its registered default. flag exposes this only
// through Visit (set flags) versus VisitAll (all flags).
func flagWasSet(fs *flag.FlagSet, name string) bool {
	found := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

// guardrailInputs is everything resolveGuardrails needs to decide the
// run's runaway-backstop posture. Split out from run()'s parameter
// list so the resolution is a pure function with a table test rather
// than an inline switch nobody can exercise.
type guardrailInputs struct {
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
}

// guardrailResolution is the decided posture plus, for each knob, the
// reason it came out that way — the startup summary prints the reason
// so an operator can tell "the default did this" from "my config did
// this" without re-deriving the precedence chain.
type guardrailResolution struct {
	// Watchdog is one of config.WatchdogOff / WatchdogWarn /
	// WatchdogFeedback / WatchdogEnforce. Never empty.
	Watchdog       string
	WatchdogSource string

	// SessionCostUSD is the resolved session ceiling; 0 == disabled.
	SessionCostUSD    float64
	SessionCostSource string
}

// Sources reported in guardrailResolution, kept as constants so the
// tests assert on the same strings the startup summary prints.
const (
	sourceFlag               = "--watchdog flag"
	sourceConfig             = "safety.watchdog config"
	sourceUnattendedDefault  = "unattended default"
	sourceInteractiveDefault = "interactive default"

	sourceCostFlag              = "--max-session-cost-usd flag"
	sourceCostConfig            = "agent.max_session_cost_usd config"
	sourceCostUnattendedDefault = "unattended default"
	sourceCostUnset             = "unset"
)

// resolveGuardrails decides the watchdog mode and session cost ceiling
// for a run.
//
// Watchdog precedence: --watchdog > safety.watchdog > mode default,
// mirroring --small-tier-parent / safety.small_tier_parent (#660). The
// mode default is "enforce" when unattended and "warn" otherwise
// (#642): an interactive operator sees the alert and can hit Ctrl-C,
// an unattended daemon cannot, so for it warn is indistinguishable
// from off.
//
// Session-ceiling precedence: --max-session-cost-usd (including an
// explicit 0) > agent.max_session_cost_usd (including an explicit 0) >
// DefaultUnattendedSessionCostUSD when unattended > disabled.
//
// Returns an error for an unrecognized watchdog value from either
// source. config.Validate already rejects a bad config value, but a
// library caller can hand-build a Config, so this re-checks rather
// than silently falling through to a default.
func resolveGuardrails(in guardrailInputs) (guardrailResolution, error) {
	res := guardrailResolution{}

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
		res.Watchdog, res.WatchdogSource = flagMode, sourceFlag
	case cfgMode != "":
		if !valid(cfgMode) {
			return res, fmt.Errorf("invalid safety.watchdog %q (want %s)", in.WatchdogConfig, modes)
		}
		res.Watchdog, res.WatchdogSource = cfgMode, sourceConfig
	case in.Unattended:
		res.Watchdog, res.WatchdogSource = config.WatchdogEnforce, sourceUnattendedDefault
	default:
		res.Watchdog, res.WatchdogSource = config.WatchdogWarn, sourceInteractiveDefault
	}

	switch {
	case in.SessionCostFlagSet:
		res.SessionCostUSD, res.SessionCostSource = in.SessionCostFlag, sourceCostFlag
	case in.SessionCostConfig != nil:
		res.SessionCostUSD, res.SessionCostSource = *in.SessionCostConfig, sourceCostConfig
	case in.Unattended:
		res.SessionCostUSD, res.SessionCostSource = DefaultUnattendedSessionCostUSD, sourceCostUnattendedDefault
	default:
		res.SessionCostUSD, res.SessionCostSource = 0, sourceCostUnset
	}
	// A negative ceiling would trip on the first token; treat it the
	// way the agent does and normalize to disabled.
	if res.SessionCostUSD < 0 {
		res.SessionCostUSD = 0
	}

	return res, nil
}

// ceilingPricing decides whether a cost ceiling can price a model: at
// startup, and at each door that picks a model later (/model, a
// background spawn). An unpriced model costs $0 to every ceiling, so the
// ceiling can never trip on it — a ceiling in name only, worse than none
// because the operator believes they have one.
//
// The policy (decided 2026-10-19, for #1299/#1327):
//   - A ceiling the operator asked for (either flag, or config) over an
//     unpriced model is refused, on every provider: the catalog prices
//     Gemini, Claude, OpenAI and xAI per backend, and a self-hosted model
//     is priced only by model.pricing (zero rates price it at $0) or
//     by real rates declared in its profile (core-models rejects
//     all-zero profile rates at load).
//   - The unattended default session ceiling, which nobody asked for,
//     only warns: refusing it would fail every unattended run on a
//     self-hosted model.
//   - A subagent's own budgets.max_cost_usd on an unpriced model is
//     always refused. Somebody wrote that budget down, and it cannot hold.
//
// Pricing is a property of the configuration, so this is where it is
// refused. A model that still reaches a ceiling at runtime is skipped
// and warned about by pkg/agent (warnUnpricedOnce), never refused per
// request. The remedies named here and there are the same.
type ceilingPricing struct {
	ceiling       agent.CostCeiling
	sessionSource string
}

// operatorSet reports whether any active ceiling is one the operator
// asked for, as opposed to only the unattended default.
func (p ceilingPricing) operatorSet() bool {
	return p.ceiling.MaxTurnUSD > 0 || (p.ceiling.MaxSessionUSD > 0 && p.sessionSource != sourceCostUnattendedDefault)
}

// check vets one model. who names it in the message ("model",
// `subagent "cluster"`). budget is that subagent's own max_cost_usd, 0
// for none. A refusal comes back as err; a warning to print as warn.
func (p ceilingPricing) check(who, model, backend string, budget float64) (warn string, err error) {
	if model == "" || !usage.PriceOn(backend, model).Unpriced {
		return "", nil
	}
	why := unpricedCeilingWhy(who, model, backend)
	if budget > 0 {
		return "", fmt.Errorf("%s budgets.max_cost_usd ($%.2f) cannot hold: %s; or drop the budget", who, budget, why)
	}
	if p.ceiling.MaxTurnUSD <= 0 && p.ceiling.MaxSessionUSD <= 0 {
		return "", nil
	}
	if p.operatorSet() {
		return "", fmt.Errorf("%s; or drop the ceiling", why)
	}
	return fmt.Sprintf("WARNING: %s. The unattended default session ceiling ($%.2f) is NOT protecting this run.", why, p.ceiling.MaxSessionUSD), nil
}

// unpricedCeilingWhy is the reason both halves of the policy give. It
// names the remedies in the same words as pkg/agent's runtime warning.
func unpricedCeilingWhy(who, model, backend string) string {
	where := ""
	if backend != "" {
		where = fmt.Sprintf(" on backend %q", backend)
	}
	return fmt.Sprintf("%s %q%s has no price — no pricing layer has a rate for it — so the cost ceiling counts its calls as $0 and can never trip. "+
		"Set model.pricing for it (zero rates are fine there for a self-hosted model you don't pay per token for), "+
		"or declare real models[].rates in its provider profile",
		who, model, where)
}

// ceilingModel is one model a ceiling or budget has to price at startup.
type ceilingModel struct {
	who, model, backend string
	budget              float64
}

// startupCeilingModels lists every model the configured ceilings reach:
// the main model, each declared subagent's (both doors share one
// template), and the background small tier a spawn may downshift to.
func startupCeilingModels(cfg *config.Config, templates []background.SubagentTemplate, smallModel string) []ceilingModel {
	mainBackend := usage.BackendFor(cfg)
	out := []ceilingModel{{who: "model", model: cfg.Model.Name, backend: mainBackend}}
	for _, t := range templates {
		out = append(out, ceilingModel{
			who:     fmt.Sprintf("subagent %q: model", t.Name),
			model:   t.ModelID,
			backend: t.Backend,
			budget:  t.Budgets.MaxCost,
		})
	}
	if smallModel != "" && smallModel != cfg.Model.Name {
		out = append(out, ceilingModel{who: "background small-tier model", model: smallModel, backend: mainBackend})
	}
	return out
}

// checkStartup runs check over every model the ceilings reach. All
// refusals are reported at once, so a config with three unpriced
// subagents needs one fix, not three restarts.
func (p ceilingPricing) checkStartup(ms []ceilingModel) (warns []string, err error) {
	var errs []error
	for _, m := range ms {
		w, e := p.check(m.who, m.model, m.backend, m.budget)
		if e != nil {
			errs = append(errs, e)
		} else if w != "" {
			warns = append(warns, w)
		}
	}
	return warns, errors.Join(errs...)
}

// door is check for a model picked after startup — /model, a background
// spawn — under the ceilings this process runs with. A warning is
// printed to stderr and the model is allowed; only a refusal stops it.
//
// It applies the ceiling rule only, never the budget rule. Every
// background spawn inherits the manager's default MaxCost, which no
// operator wrote, so the budget rule here would refuse every spawn on
// a self-hosted model with no ceiling configured at all. Budgets an
// operator declared are vetted at startup (startupCeilingModels).
func (p ceilingPricing) door(who, model, backend string) error {
	warn, err := p.check(who, model, backend, 0)
	if warn != "" {
		fmt.Fprintf(os.Stderr, "core-agent: %s\n", warn)
	}
	return err
}
