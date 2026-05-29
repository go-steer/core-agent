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

package permissions

import (
	"context"
	"errors"
)

// Decision is the user's choice in an interactive permission prompt.
type Decision int

const (
	DecisionDeny             Decision = iota // reject this call
	DecisionAllowOnce                        // allow this call, ask again next time
	DecisionAllowSession                     // allow this exact request for the rest of the session
	DecisionAllowSessionVerb                 // allow every bash command starting with this verb for the session (e.g. all "git *")
	DecisionAllowSessionTool                 // allow EVERY call to this tool for the rest of the session, regardless of args
	DecisionAllowAlways                      // persist a permanent allowlist entry, then allow
)

// String renders Decision for diagnostics.
func (d Decision) String() string {
	switch d {
	case DecisionDeny:
		return "deny"
	case DecisionAllowOnce:
		return "allow-once"
	case DecisionAllowSession:
		return "allow-session"
	case DecisionAllowSessionVerb:
		return "allow-session-verb"
	case DecisionAllowSessionTool:
		return "allow-session-tool"
	case DecisionAllowAlways:
		return "allow-always"
	default:
		return "?"
	}
}

// PromptKind classifies what the gate is asking the user about.
type PromptKind int

const (
	PromptKindBash      PromptKind = iota // mutating shell command
	PromptKindFileWrite                   // file write/edit/create
	PromptKindPathScope                   // file access outside the in-scope roots
	PromptKindGeneric                     // anything else
)

// PromptRequest carries everything the host needs to render a prompt.
//
// The persistence target — what would be written to .agents/config.json
// if the user picks DecisionAllowAlways — is held in PersistKey/PersistTool
// so the prompter doesn't have to re-derive it from Detail.
type PromptRequest struct {
	Kind        PromptKind
	ToolName    string
	Detail      string // user-facing description (the bash command, the file path, etc.)
	PersistTool string // tool name to use when adding to allowlist (e.g. "bash")
	PersistKey  string // pattern to add to allowlist

	// Verb, when populated, is the first whitespace-separated token of
	// a bash command (e.g. "git" for "git push origin main"). The TUI
	// modal uses this to offer DecisionAllowSessionVerb — broaden to
	// every command starting with this verb for the rest of the
	// session. Empty when the gate couldn't safely extract a verb
	// (path scripts, quoted commands, non-bash prompts); the modal
	// suppresses the [v] option in that case.
	Verb string

	// Source identifies the agent context the request originated
	// from when it isn't the top-level parent agent. Empty for the
	// parent's own tool calls; populated (e.g. "watch-prod-cluster")
	// when a background subagent's tool call triggered the prompt.
	// Prompters that surface it to the user help the human know which
	// agent they're approving for — the gate populates this from the
	// SubagentSourceFromContext context value the spawn machinery
	// stamps on each subagent's ctx.
	Source string

	// Access is the file operation being requested when Kind ==
	// PromptKindPathScope (AccessRead or AccessWrite). The
	// DecisionAllowAlways branch uses it to persist the matching
	// access bit on the new PathScope entry — granting rw via the
	// prompt requires two approvals (one for each op) instead of a
	// blanket grant the operator didn't explicitly choose. Zero
	// (AccessNone) for non-path-scope prompts.
	Access Access
}

// subagentSourceKey is the unexported context-value type used to
// carry a subagent's source label through to the permission gate
// without coupling the agent package to permissions internals.
type subagentSourceKey struct{}

// WithSubagentSource returns ctx tagged with name as the originating
// subagent's identifier. Called by the BackgroundAgentManager when a
// subagent runs; read by the gate when constructing PromptRequest
// values so the prompter can show "[name] tool wants to ..." in its
// heading.
func WithSubagentSource(ctx context.Context, name string) context.Context {
	if name == "" {
		return ctx
	}
	return context.WithValue(ctx, subagentSourceKey{}, name)
}

// SubagentSourceFromContext returns the subagent source name a prior
// WithSubagentSource call stamped onto ctx. Empty when none was set
// (the parent agent's own tool calls).
func SubagentSourceFromContext(ctx context.Context) string {
	v, _ := ctx.Value(subagentSourceKey{}).(string)
	return v
}

// Prompter is implemented by hosts that can interact with the user.
// Headless callers may pass nil; the gate treats a nil prompter as
// "no interactive path available".
type Prompter interface {
	AskApproval(ctx context.Context, req PromptRequest) (Decision, error)
}

// ErrNoPrompter is returned when the gate would prompt but no prompter
// is configured (e.g. headless mode without an explicit allowlist).
var ErrNoPrompter = errors.New("permissions: interactive approval required but no prompter is configured")
