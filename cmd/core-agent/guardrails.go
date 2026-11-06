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
	"io"
	"time"

	"github.com/go-steer/core-agent/v3/pkg/agent"
	"github.com/go-steer/core-agent/v3/pkg/agent/background"
	"github.com/go-steer/core-agent/v3/pkg/compose"
	"github.com/go-steer/core-agent/v3/pkg/config"
)

// DefaultUnattendedSessionCostUSD and DefaultUnattendedApprovalTimeout
// re-export the canonical pkg/compose defaults (#1369).
const (
	DefaultUnattendedSessionCostUSD  = compose.DefaultUnattendedSessionCostUSD
	DefaultUnattendedApprovalTimeout = compose.DefaultUnattendedApprovalTimeout
)

// guardrailOpts bundles the runaway-backstop CLI flags so run()'s
// signature carries one parameter instead of three, and so the
// "operator actually passed --max-session-cost-usd" bit travels with
// the value it qualifies.
type guardrailOpts struct {
	watchdogMode      string
	maxTurnCostUSD    float64
	maxSessionCostUSD float64
	maxSessionCostSet bool
	bashSearchGate    string
	approvalTimeout   string
	trust             bool
	permissionMode    string
	sandbox           string
	noSandbox         bool
}

// flagWasSet reports whether name was present on the command line, as
// opposed to sitting at its registered default.
func flagWasSet(fs *flag.FlagSet, name string) bool {
	found := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

type guardrailInputs = compose.GuardrailInputs
type guardrailResolution = compose.GuardrailResolution

const (
	sourceFlag               = compose.WatchdogSourceFlag
	sourceConfig             = compose.WatchdogSourceConfig
	sourceUnattendedDefault  = compose.WatchdogSourceUnattendedDefault
	sourceInteractiveDefault = compose.WatchdogSourceInteractiveDefault

	sourceCostFlag              = compose.SessionCostSourceFlag
	sourceCostConfig            = compose.SessionCostSourceConfig
	sourceCostUnattendedDefault = compose.SessionCostSourceUnattendedDefault
	sourceCostUnset             = compose.SessionCostSourceUnset

	sourceApprovalTimeoutFlag              = compose.ApprovalTimeoutSourceFlag
	sourceApprovalTimeoutConfig            = compose.ApprovalTimeoutSourceConfig
	sourceApprovalTimeoutUnattendedDefault = compose.ApprovalTimeoutSourceUnattendedDefault
	sourceApprovalTimeoutUnset             = compose.ApprovalTimeoutSourceUnset
)

func resolveGuardrails(in guardrailInputs) (guardrailResolution, error) {
	return compose.ResolveGuardrails(in)
}

type ceilingPricing struct {
	ceiling       agent.CostCeiling
	sessionSource string
}

func (p ceilingPricing) toCompose() compose.CeilingPricing {
	return compose.CeilingPricing{
		Ceiling:       p.ceiling,
		SessionSource: p.sessionSource,
	}
}

func (p ceilingPricing) check(who, model, backend string, budget float64) (warn string, err error) {
	return p.toCompose().Check(who, model, backend, budget)
}

func unpricedCeilingWhy(who, model, backend string) string {
	return compose.UnpricedCeilingWhy(who, model, backend)
}

type ceilingModel = compose.CeilingModel

func startupCeilingModels(cfg *config.Config, templates []background.SubagentTemplate, smallModel string) []ceilingModel {
	return compose.StartupCeilingModels(cfg, templates, smallModel)
}

func (p ceilingPricing) checkStartup(ms []ceilingModel) (warns []string, err error) {
	return p.toCompose().CheckStartup(ms)
}

func (p ceilingPricing) door(who, model, backend string) error {
	return p.toCompose().Door(who, model, backend)
}

func (p ceilingPricing) spawnDoor(s background.SpawnPricing, warned *warnOnce) (warns []string, err error) {
	return p.toCompose().SpawnDoor(s, warned)
}

type warnOnce = compose.WarnOnce

// autoContinueResolution wraps compose.AutoContinueResolution so existing
// callers and tests in package main keep working unchanged (#1369).
type autoContinueResolution struct {
	enabled       bool
	freshness     time.Duration
	retry         bool
	retryInterval time.Duration
}

func resolveAutoContinue(ac *config.AutoContinueConfig, multiSession, noREPL, haveEventlog bool, stderr io.Writer) (autoContinueResolution, error) {
	res, err := compose.ResolveAutoContinue(ac, multiSession, noREPL, haveEventlog, stderr)
	return autoContinueResolution{
		enabled:       res.Enabled,
		freshness:     res.Freshness,
		retry:         res.Retry,
		retryInterval: res.RetryInterval,
	}, err
}
