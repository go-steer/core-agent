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

package coretuiremote

import coretui "github.com/go-steer/core-tui/tui"

// This file is the single auditable list of every core-tui capability
// *Adapter (attach mode) implements. Add to it; do not scatter new
// assertions next to the methods.
//
// Why a list of `var _` lines and not a doc comment:
//
// core-tui discovers optional capabilities by type assertion on
// Options.Agent, with no error and no log when the assertion fails —
// e.g. tui/agentcmd.go's `waker, ok := m.opts.Agent.(WakeRequester);
// if !ok { return nil }`. A near-miss (right intent, wrong method
// name or wrong signature) is therefore indistinguishable at runtime
// from a host that deliberately declines the capability: the feature
// simply never appears and nothing anywhere says why.
//
// A doc comment saying "X satisfies coretui.Y" reads to a reviewer
// exactly like a guarantee and is checked by nothing. Both known
// defects in this pattern (#802, #803) carried one and survived
// multiple core-tui bumps and a round of review each. The assertions
// below are the only form of that claim the compiler enforces: they
// cost nothing at runtime and turn a silently-dead feature into a
// build failure.
//
// The list is also deliberately EXHAUSTIVE over core-tui's exported
// interfaces: capabilities this adapter does not implement are named
// in the trailing comment with the reason. An unexplained absence is
// indistinguishable from an oversight, which is the same failure mode
// one level up.
//
// Cross-check against the interfaces core-tui exports — tui/agent.go,
// tui/capabilities.go, tui/slash.go, tui/asker.go, tui/elicitor.go,
// tui/prompter.go — whenever the core-tui pin moves. core-tui
// docs/design.md §3.3 ("Optional capability interfaces") is the
// declared roster and prescribes exactly this. See core-agent #804.

var (
	// tui/agent.go
	_ coretui.Agent             = (*Adapter)(nil) // Run
	_ coretui.InjectableAgent   = (*Adapter)(nil) // Inject
	_ coretui.LiveAgent         = (*Adapter)(nil) // Events — observer mode is THE attach-mode path
	_ coretui.RemoteInterrupter = (*Adapter)(nil) // Interrupt
	_ coretui.WakeRequester     = (*Adapter)(nil) // WakeRequested — fed by `wake` SSE frames (#802)

	// tui/pause.go
	_ coretui.Pauser = (*Adapter)(nil) // Pause, Resume, PauseState — the operator hold (protocol v1.5.0)

	// tui/capabilities.go
	_ coretui.SessionSwitcher      = (*Adapter)(nil) // Sessions, SwitchToSession
	_ coretui.Reloader             = (*Adapter)(nil) // Reload
	_ coretui.PermissionController = (*Adapter)(nil) // SessionApprovals, Add{Allow,Deny}Patterns, AddBuiltinAllowExtra
	_ coretui.PricingController    = (*Adapter)(nil) // Refresh, Set
	_ coretui.ToolLister           = (*Adapter)(nil) // Tools
	_ coretui.SubagentReporter     = (*Adapter)(nil) // Subagents, SubagentEvents
	_ coretui.StatusReporter       = (*Adapter)(nil) // Status
	// UsageTracker is the one capability core-tui does NOT find on the
	// Agent — cmd/core-agent-tui passes the same *Adapter a second
	// time through Options.UsageTracker (core-tui design.md §3.3), so
	// that field's interface type already checks it. The line stays
	// for the audit: dropping a method here would still delete the
	// footer's token/cost rows, and the wiring site is in a different
	// package from the methods.
	_ coretui.UsageTracker = (*Adapter)(nil) // SessionTotals … SessionDuration

	// tui/slash.go
	_ coretui.SlashProvider      = (*Adapter)(nil) // SlashCommands, InvokeSlash
	_ coretui.AsyncSlashProvider = (*Adapter)(nil) // SlashCommands, InvokeSlashAsync
)

// Deliberately NOT implemented by *Adapter — each absence is a
// decision, not an oversight.
//
// Each decision carries a `//coretui:declined <Interface>` directive.
// The directive is what dev/tools/verify-coretui-guards counts when it
// checks this list against the core-tui go.mod pins; the bullet under
// it is the decision itself, and the gate requires the two to name the
// same interface so neither can be renamed out from under the other.
// Every exported core-tui interface has to be either guarded above or
// declined here (#812).
//
//coretui:declined ModelSwapper
//   - coretui.ModelSwapper: the model is the daemon's choice, not the
//     attached operator's. /model against a remote session would have
//     to mutate server-side state for every other attached client;
//     the attach protocol deliberately exposes no such endpoint.
//
//coretui:declined InboxDrainer
//   - coretui.InboxDrainer: auto-continue is driven by the DAEMON's
//     own inbox loop on the far side of the wire, not by the TUI.
//     Draining it from here would race the daemon's own drain and
//     steal messages out of the running turn. Local mode (which owns
//     its runner in-process) is the one that implements this.
//
//coretui:declined PermanentStreamError
//   - coretui.PermanentStreamError: implemented, but by the error
//     values this adapter propagates rather than by the adapter —
//     see internal/attachclient's httpStatusError. That package
//     deliberately keeps no import dependency on core-tui (core-tui
//     duck-types the interface), so its guard is a locally-redeclared
//     structural mirror in status_error_test.go rather than a line
//     here.
//
//coretui:declined Asker
//coretui:declined Elicitor
//coretui:declined PermissionPrompter
//   - coretui.Asker / coretui.Elicitor / coretui.PermissionPrompter:
//     not host-implemented interfaces at all. core-tui ships the
//     concrete types (NewAsker / NewElicitor / NewPrompter) and a host
//     that wants one wires an instance into the matching Options
//     field, which is interface-typed and therefore already
//     compile-checked at the wiring site. Attach mode wires exactly
//     one of the three — StartRemotePrompter (prompter.go) returns a
//     coretui.NewPrompter() bridged to the daemon's /perms stream.
//     Options.Elicitor and Options.Asker are left unset here: MCP
//     elicits are answered on the daemon side, and nothing in the tree
//     wires Asker yet (it needs an ask-the-user tool behind it — see
//     the note in the v0.22.0 bump, #801).
