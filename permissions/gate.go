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
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/go-steer/core-agent/config"
)

// ApprovalLog is one entry in the gate's per-session approval audit.
// It records every interactive permission decision the user made
// (excluding denials) so callers can later offer a "review approvals
// + recommend" workflow.
type ApprovalLog struct {
	Tool     string
	Key      string
	Decision Decision
	At       time.Time
}

// Mode mirrors the permission modes recognized by config.PermissionsConfig.
type Mode string

const (
	ModeAsk   Mode = "ask"
	ModeAllow Mode = "allow"
	ModeYolo  Mode = "yolo"
)

// Gate is the central permission chokepoint consulted before each tool
// call. It holds the configured policy, the path scope, the bash
// denylist (built-in), and an optional Prompter for interactive use.
//
// Gate is safe for concurrent use; tool handlers run in the agent's
// event-iteration goroutine, but the prompter call may yield while
// waiting for the user.
type Gate struct {
	mu sync.Mutex

	mode     Mode
	policy   *Policy
	scope    *PathScope
	prompter Prompter

	// In-session allow set keyed by tool|key. Populated by
	// DecisionAllowSession choices so we don't re-prompt the same call
	// repeatedly within one session.
	sessionAllow map[string]struct{}
	// Tool-wide in-session allow set, keyed by tool name only.
	// Populated by DecisionAllowSessionTool when the user trusts an
	// entire tool for the rest of the session. Bash denylist still
	// applies — that pre-check runs before the gate ever sees the request.
	sessionAllowTools map[string]struct{}

	// Chronological log of every non-deny interactive approval.
	approvals []ApprovalLog
}

// Options configures a Gate at construction time. All fields are
// optional; sensible defaults apply when omitted.
type Options struct {
	Mode     Mode
	Policy   *Policy
	Scope    *PathScope
	Prompter Prompter // nil = no interactive path; ask-mode unresolved → deny
}

// New builds a Gate from the supplied options. The Mode defaults to
// "ask"; missing Policy/Scope default to permissive empties.
func New(opts Options) *Gate {
	if opts.Mode == "" {
		opts.Mode = ModeAsk
	}
	if opts.Policy == nil {
		opts.Policy, _ = NewPolicy(nil, nil)
	}
	if opts.Scope == nil {
		opts.Scope, _ = NewPathScope("", "", nil)
	}
	return &Gate{
		mode:              opts.Mode,
		policy:            opts.Policy,
		scope:             opts.Scope,
		prompter:          opts.Prompter,
		sessionAllow:      make(map[string]struct{}),
		sessionAllowTools: make(map[string]struct{}),
	}
}

// FromConfig builds a Gate from a Config plus the resolved project root
// and user-global root. The Prompter is wired separately since it
// depends on whether we're running interactively or headless.
func FromConfig(cfg *config.Config, projectRoot, userRoot string, prompter Prompter) (*Gate, error) {
	policy, err := NewPolicy(cfg.Permissions.Allow, cfg.Permissions.Deny)
	if err != nil {
		return nil, fmt.Errorf("permissions policy: %w", err)
	}
	scope, err := NewPathScope(projectRoot, userRoot, cfg.PathScope.Allow)
	if err != nil {
		return nil, err
	}
	mode := Mode(cfg.Permissions.Mode)
	if mode == "" {
		mode = ModeAsk
	}
	return New(Options{Mode: mode, Policy: policy, Scope: scope, Prompter: prompter}), nil
}

// Mode reports the active permission mode.
func (g *Gate) Mode() Mode { return g.mode }

// HasPrompter reports whether an interactive Prompter is wired. False
// means an ask-mode call would fail with ErrNoPrompter rather than
// reach a human — useful for callers (e.g. autonomous drivers) that
// want to fail fast at startup instead of on the first tool call.
func (g *Gate) HasPrompter() bool { return g.prompter != nil }

// Scope exposes the path scope. Callers that mutate the scope should
// also persist the change via the config layer.
func (g *Gate) Scope() *PathScope { return g.scope }

// CheckGeneric gates an arbitrary tool call (used by MCP and skill
// toolsets, where we don't have a dedicated Check<Tool> method).
//
// toolName is the namespace under which policy lookups happen
// (typically "mcp" or "skill"); key is the human-readable detail
// shown in prompts (typically the tool's full namespaced name plus
// a brief argument summary).
func (g *Gate) CheckGeneric(ctx context.Context, toolName, key string) error {
	return g.gateRequest(ctx, PromptKindGeneric, toolName, key, toolName, key)
}

// CheckBash gates a bash invocation. The denylist is checked first and
// is non-overridable. After that, policy + mode determine whether the
// call needs a prompt.
func (g *Gate) CheckBash(ctx context.Context, command string) error {
	command = strings.TrimSpace(command)
	if denied, reason := IsBashDenied(command); denied {
		return fmt.Errorf("bash refused: %s", reason)
	}
	return g.gateRequest(ctx, PromptKindBash, "bash", command, "bash", command)
}

// CheckFileRead gates a read-only file operation. Read access only
// fails if the path is out of scope (and the user can't or won't
// extend scope).
func (g *Gate) CheckFileRead(ctx context.Context, toolName, path string) error {
	if g.sessionToolAllowed(toolName) {
		return nil
	}
	in, err := g.scope.Contains(path)
	if err != nil {
		return err
	}
	if in {
		return nil
	}
	return g.promptForPath(ctx, toolName, path, "read")
}

// CheckFileWrite gates a mutating file operation. Out-of-scope paths
// are escalated via prompt; in-scope paths still go through mode-aware
// approval (ask mode prompts; allow/yolo proceed unless deny rule hits).
func (g *Gate) CheckFileWrite(ctx context.Context, toolName, path string) error {
	if g.sessionToolAllowed(toolName) {
		return nil
	}
	in, err := g.scope.Contains(path)
	if err != nil {
		return err
	}
	if !in {
		return g.promptForPath(ctx, toolName, path, "write")
	}
	return g.gateRequest(ctx, PromptKindFileWrite, toolName, path, toolName, path)
}

func (g *Gate) gateRequest(ctx context.Context, kind PromptKind, toolName, key, persistTool, persistKey string) error {
	switch g.policy.Match(toolName, key) {
	case OutcomeDeny:
		return fmt.Errorf("%s denied by config policy: %q", toolName, key)
	case OutcomeAllow:
		return nil
	}
	if g.sessionToolAllowed(toolName) {
		return nil
	}
	if g.sessionAllowed(toolName, key) {
		return nil
	}
	switch g.mode {
	case ModeYolo:
		return nil
	case ModeAllow:
		return fmt.Errorf("%s requires an allowlist entry in 'allow' mode: %q", toolName, key)
	case ModeAsk:
		return g.prompt(ctx, PromptRequest{
			Kind:        kind,
			ToolName:    toolName,
			Detail:      key,
			PersistTool: persistTool,
			PersistKey:  persistKey,
			Source:      SubagentSourceFromContext(ctx),
		})
	}
	return fmt.Errorf("%s denied: unknown permission mode %q", toolName, g.mode)
}

func (g *Gate) promptForPath(ctx context.Context, toolName, path, op string) error {
	if g.mode == ModeYolo {
		return nil
	}
	if g.mode == ModeAllow {
		return fmt.Errorf("%s denied: path %q is outside scope and 'allow' mode does not prompt", toolName, path)
	}
	return g.prompt(ctx, PromptRequest{
		Kind:        PromptKindPathScope,
		ToolName:    toolName,
		Detail:      fmt.Sprintf("%s %s (out of scope)", op, path),
		PersistTool: "path_scope",
		PersistKey:  path,
		Source:      SubagentSourceFromContext(ctx),
	})
}

func (g *Gate) prompt(ctx context.Context, req PromptRequest) error {
	if g.prompter == nil {
		return fmt.Errorf("%w (tool=%s detail=%q); run with --yolo to bypass the gate, set permissions.mode=\"allow\" with an explicit allowlist for headless use, or attach an interactive stdin", ErrNoPrompter, req.ToolName, req.Detail)
	}
	d, err := g.prompter.AskApproval(ctx, req)
	if err != nil {
		return fmt.Errorf("permissions: %w", err)
	}
	switch d {
	case DecisionAllowOnce:
		g.recordApproval(req.ToolName, req.Detail, d)
		return nil
	case DecisionAllowSession:
		g.rememberSession(req.ToolName, req.Detail)
		g.recordApproval(req.ToolName, req.Detail, d)
		return nil
	case DecisionAllowSessionTool:
		g.rememberSessionTool(req.ToolName)
		g.rememberSession(req.ToolName, req.Detail)
		g.recordApproval(req.ToolName, req.Detail, d)
		return nil
	case DecisionAllowAlways:
		g.rememberSession(req.ToolName, req.Detail)
		if req.Kind == PromptKindPathScope {
			g.scope.AddAlwaysAllow(req.PersistKey)
		}
		g.recordApproval(req.ToolName, req.Detail, d)
		return nil
	default:
		return fmt.Errorf("%s denied by user: %s", req.ToolName, req.Detail)
	}
}

func (g *Gate) sessionAllowed(toolName, key string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	_, ok := g.sessionAllow[toolName+"|"+key]
	return ok
}

func (g *Gate) rememberSession(toolName, key string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.sessionAllow[toolName+"|"+key] = struct{}{}
}

func (g *Gate) sessionToolAllowed(toolName string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	_, ok := g.sessionAllowTools[toolName]
	return ok
}

func (g *Gate) rememberSessionTool(toolName string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.sessionAllowTools[toolName] = struct{}{}
}

func (g *Gate) recordApproval(toolName, key string, d Decision) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.approvals = append(g.approvals, ApprovalLog{
		Tool:     toolName,
		Key:      key,
		Decision: d,
		At:       time.Now(),
	})
}

// Approvals returns a defensive copy of the in-session approval log.
// Order is chronological. Safe for concurrent callers.
func (g *Gate) Approvals() []ApprovalLog {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]ApprovalLog, len(g.approvals))
	copy(out, g.approvals)
	return out
}

// Snapshot is a read-only view of the gate's configured policy + mode,
// suitable for surfacing to operators (attach-mode /tools endpoint, the
// TUI's tool catalog) without exposing the gate's internal state. The
// returned slices are defensive copies. Does not include session-level
// approvals (those are inherently fleeting and per-request); use
// Approvals() for the per-session audit log.
type Snapshot struct {
	Mode  Mode     `json:"mode"`
	Allow []string `json:"allow,omitempty"`
	Deny  []string `json:"deny,omitempty"`
}

// Snapshot returns the current gate configuration.
func (g *Gate) Snapshot() Snapshot {
	allow, deny := g.policy.RawPatterns()
	return Snapshot{
		Mode:  g.mode,
		Allow: allow,
		Deny:  deny,
	}
}

// ToolGateState classifies a tool name against the configured policy
// without actually requesting permission. Used by the attach-mode
// /tools endpoint so the TUI / WebUI / operator can see whether a tool
// would be allowed, denied, or prompted before the model tries it.
//
// Semantics:
//   - "denied"   — a deny pattern matches the bare tool name (no key).
//     Denials with key globs (e.g. "bash:sudo *") cannot be
//     pre-computed without a candidate key and are reported
//     as "prompted".
//   - "allowed"  — mode is "yolo" (gate is bypassed), OR an allow
//     pattern matches the bare tool name + no deny does.
//   - "prompted" — mode is "ask" and no preempting allow/deny applies.
//   - "denied-allow-mode" — mode is "allow" and no allowlist entry covers
//     the tool (so it would be refused with a
//     "requires an allowlist entry" error).
//
// This is a pre-flight projection, not a guarantee — interactive
// approvals at runtime can grant access that's not in the snapshot.
func (g *Gate) ToolGateState(toolName string) string {
	if matchAny(g.policy.denyRules(), toolName, "") {
		return ToolGateDenied
	}
	if g.mode == ModeYolo {
		return ToolGateAllowed
	}
	if matchAny(g.policy.allowRules(), toolName, "") {
		return ToolGateAllowed
	}
	if g.mode == ModeAllow {
		return ToolGateDeniedInAllowMode
	}
	return ToolGatePrompted
}

// Tool-gate state strings exposed via the attach-mode /tools endpoint.
// Kept as bare strings (not a typed enum) so JSON consumers downstream
// (TUI, WebUI, operator scripts) don't have to import a Go package to
// reason about them.
const (
	ToolGateAllowed           = "allowed"
	ToolGateDenied            = "denied"
	ToolGatePrompted          = "prompted"
	ToolGateDeniedInAllowMode = "denied-allow-mode"
)
