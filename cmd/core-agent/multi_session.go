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
	"context"
	"fmt"

	"github.com/google/uuid"
	adkmodel "google.golang.org/adk/model"
	adktool "google.golang.org/adk/tool"

	"github.com/go-steer/core-agent/pkg/agent"
	"github.com/go-steer/core-agent/pkg/attach"
	"github.com/go-steer/core-agent/pkg/auth"
	"github.com/go-steer/core-agent/pkg/config"
	"github.com/go-steer/core-agent/pkg/eventlog"
	"github.com/go-steer/core-agent/pkg/instruction"
	"github.com/go-steer/core-agent/pkg/permissions"
	"github.com/go-steer/core-agent/pkg/usage"
)

// buildMultiSessionAuthn translates the operator's
// attach.multi_session config block into the pkg/auth Authenticator
// that the attach listener consults per-request. Returns:
//
//   - authn: the resolved Authenticator (or nil for single-user mode)
//   - fallback: the Caller stamped on requests that don't authenticate
//     (used by callerMiddleware as the no-cred default)
//   - err: a fatal startup error if the config is internally
//     inconsistent OR a referenced file can't be loaded
//
// In single-user mode (multi_session.enabled = false), returns
// (nil, zero-Caller, nil) — the attach server defaults its own
// AnonymousAuth and the wiring is a no-op.
func buildMultiSessionAuthn(cfg config.MultiSessionConfig) (auth.Authenticator, auth.Caller, error) {
	// Default Caller comes from the config knob (resolved to "anon"
	// when unset to match the design doc's documented default). Used
	// for the legacy / single-user path AND as the AllowAnonymous
	// fallback when multi-session is on.
	defaultCaller := auth.Caller{Identity: cfg.DefaultIdentity}
	if defaultCaller.Identity == "" {
		defaultCaller = auth.Anonymous
	}

	if !cfg.Enabled {
		return nil, defaultCaller, nil
	}

	switch cfg.Auth.Kind {
	case "", config.MultiSessionAuthKindBearerTable:
		users, err := auth.LoadUsersFile(cfg.Auth.TableFile)
		if err != nil {
			return nil, defaultCaller, fmt.Errorf("load users file: %w", err)
		}
		authn := auth.NewBearerTokenAuth(users.Users, cfg.AdminIdentities, cfg.ProxyIdentities)
		return authn, defaultCaller, nil
	default:
		// Validation in config.Validate() should catch this earlier;
		// guard anyway so a corrupted call path produces a clear error
		// instead of a silent fallback.
		return nil, defaultCaller, fmt.Errorf("unsupported auth.kind %q (only %q is shipped in this version)", cfg.Auth.Kind, config.MultiSessionAuthKindBearerTable)
	}
}

// sessionFactoryDeps bundles the daemon-wide configuration the
// per-session SessionFactory closure needs to capture. Constructed
// once at daemon startup; the resulting factory builds fresh
// *agent.Agent values for each POST /sessions request.
//
// v0 spike: substrate-essential options only (tools, eventlog,
// per-session sub-gate, per-caller instruction overlay, per-session
// prompter). Operator features that flow into the startup-time agent
// (BackgroundManager, Compactor, Watchdog, Checkpointer, CostCeiling,
// agentic tools, ask_user, MCP custom auth, etc.) are intentionally
// NOT wired into on-demand sessions in this iteration — every one of
// them needs scope-aware re-instantiation per session, which is a
// follow-up. Sessions created via POST /sessions are functional but
// see the substrate as it is, without the operator-feature
// extensions. Document the gap in the recipe.
type sessionFactoryDeps struct {
	model          adkmodel.LLM
	template       *permissions.Gate
	builtinTools   []adktool.Tool
	toolsets       []adktool.Toolset
	eventlogHandle *eventlog.Handle
	tracker        *usage.Tracker
	projectRoot    string
	userRoot       string
	usersDir       string
	registry       *attach.SessionRegistry
}

// buildSessionFactory returns an attach.SessionFactory closure that
// constructs a fresh *agent.Agent per POST /sessions request. The
// closure captures the deps by value (slices + pointers); per-call
// it generates a unique sessionID, derives a per-session sub-gate +
// prompter, loads the per-caller instruction overlay, and assembles
// a minimal-but-functional agent.
//
// The handler is responsible for calling RegisterOwned on the
// returned Registrant with the originating Caller.Identity — this
// factory deliberately does NOT take a WithSessionRegistry option,
// because that would self-register via the legacy Register() (no
// Owner stamp), losing the ACL ownership that's the whole point.
func buildSessionFactory(deps sessionFactoryDeps) attach.SessionFactory {
	return func(_ context.Context, caller auth.Caller) (attach.Registrant, error) {
		sid := newSessionID()

		// Per-session HTTP prompt broker. Each new session gets its
		// own broker so prompts route to the right per-session
		// /perms/stream subscriber.
		broker := attach.NewPromptBroker()

		// Per-session sub-gate isolates sessionAllow / planRecorded
		// / mode / approvals from sibling sessions. Shares Policy /
		// PathScope / requirePlanArtifact via the template (the
		// documented limitation in docs/multi-session-design.md).
		sessionGate := deps.template.DeriveForSession(sid, broker)

		// Per-caller instruction overlay: the operator's
		// <usersDir>/<caller.Identity>/.agents/ tree layered on
		// top of project + user scopes. Empty usersDir or unknown
		// caller falls through to the daemon-wide instruction stack.
		instr, err := instruction.LoadForSession(deps.projectRoot, deps.userRoot, caller.Identity, deps.usersDir)
		if err != nil {
			broker.Close()
			return nil, fmt.Errorf("load per-caller instructions: %w", err)
		}

		opts := []agent.Option{
			agent.WithTools(deps.builtinTools),
			agent.WithToolsets(deps.toolsets),
			agent.WithSystemInstructionPrefix(instr.Instruction),
			agent.WithGate(sessionGate),
			agent.WithSession(caller.Identity, sid),
			agent.WithAttachPromptBroker(broker),
		}
		if deps.eventlogHandle != nil {
			opts = append(opts, agent.WithEventLog(deps.eventlogHandle))
		}
		if deps.tracker != nil {
			opts = append(opts, agent.WithUsageTracker(deps.tracker))
		}

		ag, err := agent.New(deps.model, opts...)
		if err != nil {
			broker.Close()
			return nil, fmt.Errorf("agent.New: %w", err)
		}
		return ag, nil
	}
}

// newSessionID returns a unique session identifier suitable for the
// (app, user, sid) triple. UUID v7 is sortable by creation time so
// "newest session" queries are free; V4 fallback only fires on a
// genuinely broken OS clock.
func newSessionID() string {
	id, err := uuid.NewV7()
	if err != nil {
		// Fallback to V4 — V7 only fails when the OS clock is
		// unrecoverably broken. A V4 still uniquely identifies the
		// session; we just lose the time-sortable property.
		return uuid.NewString()
	}
	return id.String()
}
