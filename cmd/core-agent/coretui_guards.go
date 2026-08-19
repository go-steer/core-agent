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

// The build tag MUST match coretui_enabled.go's, which is where the
// asserted types live. Drop it and this file fails to compile in the
// slim (`-tags no_tui`) image; widen it and the guards silently leave
// the default build along with the adapter they guard.
//go:build !no_tui

package main

import coretui "github.com/go-steer/core-tui/tui"

// This file is the single auditable list of every core-tui capability
// the local `--tui` host implements. Add to it; do not scatter new
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
// interfaces: capabilities this host does not implement are named in
// the trailing comment with the reason. An unexplained absence is
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
	_ coretui.Agent           = (*coreAgentAdapter)(nil) // Run
	_ coretui.InjectableAgent = (*coreAgentAdapter)(nil) // Inject
	_ coretui.InboxDrainer    = (*coreAgentAdapter)(nil) // DrainInbox, PendingInboxCount
	_ coretui.WakeRequester   = (*coreAgentAdapter)(nil) // WakeRequested

	// tui/capabilities.go
	_ coretui.ModelSwapper         = (*coreAgentAdapter)(nil) // AvailableModels, SwitchModel
	_ coretui.Reloader             = (*coreAgentAdapter)(nil) // Reload
	_ coretui.PermissionController = (*coreAgentAdapter)(nil) // SessionApprovals, Add{Allow,Deny}Patterns, AddBuiltinAllowExtra
	_ coretui.PricingController    = (*coreAgentAdapter)(nil) // Refresh, Set
	_ coretui.ToolLister           = (*coreAgentAdapter)(nil) // Tools
	_ coretui.SubagentReporter     = (*coreAgentAdapter)(nil) // Subagents, SubagentEvents
	_ coretui.StatusReporter       = (*coreAgentAdapter)(nil) // Status
	//
	// UsageTracker is the one capability core-tui does NOT find on
	// the Agent — it comes in through Options.UsageTracker (core-tui
	// design.md §3.3), so it may live on a type of its own, and here
	// it does: launchTUIv2 wires a *coreUsageBridge over usage.Tracker
	// because the tracker outlives any single /model swap. The Options
	// field is interface-typed so the wiring site already type-checks;
	// the line is here so the capability shows up in the audit list
	// instead of reading as absent.
	_ coretui.UsageTracker = (*coreUsageBridge)(nil)

	// tui/slash.go
	_ coretui.SlashProvider      = (*coreAgentAdapter)(nil) // SlashCommands, InvokeSlash
	_ coretui.AsyncSlashProvider = (*coreAgentAdapter)(nil) // SlashCommands, InvokeSlashAsync
)

// Deliberately NOT implemented by the local host — each absence is a
// decision, not an oversight:
//
//   - coretui.RemoteInterrupter: added by #803. coreAgentAdapter has
//     an Interrupt() bool that satisfies nothing (core-tui wants
//     Interrupt(context.Context) error), so the guard would not
//     compile today. #803 owns both the fix and the line.
//
//   - coretui.LiveAgent: local mode IS the per-turn Run path. Events()
//     would take precedence over Run and silently disable it (see the
//     precedence note on coretui.LiveAgent); the autonomous-daemon
//     stream it exists for is coretuiremote's job.
//
//   - coretui.SessionSwitcher: /session switching enumerates sessions
//     over the attach protocol, which is attach mode's surface. The
//     in-process host owns exactly one session for its lifetime.
//
//   - coretui.PermanentStreamError: an error-value interface, not a
//     host one, and only meaningful for a reconnecting remote stream.
//     Nothing in local mode can raise it.
//
//   - coretui.PermissionPrompter / coretui.Elicitor: not
//     host-implemented interfaces. core-tui ships the concrete types
//     and launchTUIv2 wires instances into the interface-typed Options
//     fields (coretui.NewPrompter, coretui.NewElicitor), which is
//     where the compile-time check for those already lives. The
//     adaptation this host writes runs the OTHER way —
//     gatePrompterBridge wraps a coretui.PermissionPrompter as a
//     permissions.Prompter, coreMCPElicitor wraps a coretui.Elicitor
//     as an mcp.ElicitorFn — so neither is a coretui implementation to
//     guard.
//
//   - coretui.Asker: same shape as the two above (core-tui ships
//     NewAsker), but Options.Asker is not wired anywhere in this repo
//     yet. It wants an ask-the-user tool behind it, which is a feature
//     and not a bump — see the v0.22.0 pickup note (#801).
