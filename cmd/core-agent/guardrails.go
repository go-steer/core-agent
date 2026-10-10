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
	"flag"
	"fmt"
	"strings"

	"github.com/go-steer/core-agent/v2/pkg/agent"
	"github.com/go-steer/core-agent/v2/pkg/config"
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

// checkProfileCeiling vets a cost ceiling against a provider-profile
// model that has no price. The catalog covers Gemini and Claude, and a
// self-hosted model is priced only if its profile declares rates;
// unpriced, every call costs $0 to the ceiling and it can never trip —
// a ceiling in name only, worse than none because the operator believes
// they have one.
//
// A ceiling the operator asked for (either flag, or config) is refused
// with err. The unattended default, which nobody asked for, is not: it
// returns a warning to print and the run goes ahead, since refusing it
// would fail every unattended run on a self-hosted model.
func checkProfileCeiling(cfg *config.Config, ceiling agent.CostCeiling, sessionSource string, unpriced bool) (warn string, err error) {
	if !unpriced || (ceiling.MaxTurnUSD <= 0 && ceiling.MaxSessionUSD <= 0) || !cfg.IsProfileProvider(cfg.Model.Provider) {
		return "", nil
	}
	why := fmt.Sprintf("provider profile %q: model %q has no price — no pricing layer has a rate for it and the profile declares none — so the cost ceiling counts its calls as $0 and can never trip. Declare models[].rates for it in the profile, or set model.pricing",
		cfg.Model.Provider, cfg.Model.Name)
	if ceiling.MaxTurnUSD > 0 || sessionSource != sourceCostUnattendedDefault {
		return "", fmt.Errorf("%s; or drop the ceiling", why)
	}
	return fmt.Sprintf("WARNING: %s. The unattended default session ceiling ($%.2f) is NOT protecting this run.", why, ceiling.MaxSessionUSD), nil
}
