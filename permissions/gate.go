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
	"os"
	"path/filepath"
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

	// ModePlan disables all tool execution — every gate call returns
	// an error. Used by core-tui's "plan" chip (R-PERM-7) for
	// read-and-think sessions that shouldn't touch the world. The
	// operator cycles out via Shift+Tab when ready to act.
	ModePlan Mode = "plan"

	// ModeAcceptEdits auto-allows file-write tool calls (and
	// out-of-scope write paths) without prompting; every other tool
	// kind still flows through the normal Ask path. Used by core-
	// tui's "acceptEdits" chip so the operator can stream a
	// refactor without clicking through every diff modal.
	ModeAcceptEdits Mode = "acceptEdits"
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

	// Verb-scoped in-session allow set, keyed by "<tool>|<verb>".
	// Populated by DecisionAllowSessionVerb so the user can broaden
	// trust to "every `git *` command" without persisting an allowlist
	// entry. Bash denylist still applies (denylist pre-check runs
	// before the gate request).
	sessionAllowVerbs map[string]struct{}

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
		sessionAllowVerbs: make(map[string]struct{}),
	}
}

// FromConfig builds a Gate from a Config plus the resolved project root
// and user-global root. The Prompter is wired separately since it
// depends on whether we're running interactively or headless.
//
// Built-in allow bundles are merged on top of the configured Allow
// patterns: the read_only bundle is on by default and can be turned
// off with permissions.use_builtin_allow=false; additional bundles
// listed in permissions.builtin_allow_extras add to the merge. See
// builtin_allow.go for the bundle catalog.
func FromConfig(cfg *config.Config, projectRoot, userRoot string, prompter Prompter) (*Gate, error) {
	useBuiltin := true
	if cfg.Permissions.UseBuiltinAllow != nil {
		useBuiltin = *cfg.Permissions.UseBuiltinAllow
	}
	builtin, err := ResolveBuiltinAllow(useBuiltin, cfg.Permissions.BuiltinAllowExtras)
	if err != nil {
		return nil, fmt.Errorf("permissions: %w", err)
	}
	merged := make([]string, 0, len(builtin)+len(cfg.Permissions.Allow))
	merged = append(merged, builtin...)
	merged = append(merged, cfg.Permissions.Allow...)
	policy, err := NewPolicy(merged, cfg.Permissions.Deny)
	if err != nil {
		return nil, fmt.Errorf("permissions policy: %w", err)
	}
	entries := make([]pathEntry, 0, len(cfg.PathScope.Allow)+len(cfg.PathScope.AllowPaths))
	for _, p := range cfg.PathScope.Allow {
		entries = append(entries, pathEntry{Pattern: p, Access: AccessReadWrite})
	}
	for _, e := range cfg.PathScope.AllowPaths {
		access, err := ParseAccess(e.Access)
		if err != nil {
			return nil, fmt.Errorf("permissions: path_scope.allow_paths[%s]: %w", e.Path, err)
		}
		entries = append(entries, pathEntry{Pattern: e.Path, Access: access})
	}
	scope, err := NewPathScopeFromEntries(projectRoot, userRoot, entries)
	if err != nil {
		return nil, err
	}
	mode := Mode(cfg.Permissions.Mode)
	if mode == "" {
		mode = ModeAsk
	}
	return New(Options{Mode: mode, Policy: policy, Scope: scope, Prompter: prompter}), nil
}

// Mode reports the active permission mode. Acquires g.mu to pair
// with SetMode's writer — without the read lock, SetMode would race
// with every other mode reader (gateRequest, promptForPath,
// ToolGateState).
func (g *Gate) Mode() Mode {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.mode
}

// SetMode replaces the gate's permission mode at runtime. Used by
// the embedded TUI when the operator cycles the permission-mode
// chip (R-PERM-6 in core-tui). Unknown modes are silently ignored
// so a future TUI value can't smuggle in semantics the gate
// doesn't recognize.
func (g *Gate) SetMode(m Mode) {
	switch m {
	case ModeAsk, ModeAllow, ModeYolo, ModePlan, ModeAcceptEdits:
		g.mu.Lock()
		g.mode = m
		g.mu.Unlock()
	}
}

// HasPrompter reports whether an interactive Prompter is wired. False
// means an ask-mode call would fail with ErrNoPrompter rather than
// reach a human — useful for callers (e.g. autonomous drivers) that
// want to fail fast at startup instead of on the first tool call.
func (g *Gate) HasPrompter() bool { return g.prompter != nil }

// SetPrompter swaps the gate's interactive prompter. Used when the
// process changes UI mode mid-startup — e.g. core-agent's main.go
// constructs the gate with a stdin prompter for the headless path,
// then the TUI replaces it with one that sends messages into the
// bubble-tea program. Set to nil to disable interactive prompting
// (ask-mode calls then fail with ErrNoPrompter).
func (g *Gate) SetPrompter(p Prompter) { g.prompter = p }

// AddAllowPatterns extends the live policy with additional allow
// patterns and is safe to call concurrently with in-flight Match
// calls. Used by the TUI's /allow slash command to make new
// permissions take effect immediately rather than only after a
// restart. Returns the same error shape as NewPolicy when a pattern
// is malformed.
func (g *Gate) AddAllowPatterns(patterns []string) error {
	return g.policy.AddAllow(patterns)
}

// AddDenyPatterns is the symmetric extension for deny entries, used
// by /deny. Deny always wins in Match so adding here can override a
// previously-allowed pattern mid-session.
func (g *Gate) AddDenyPatterns(patterns []string) error {
	return g.policy.AddDeny(patterns)
}

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

// CheckFileRead gates a read-only file operation. An allow-list
// entry that grants read (r or rw) short-circuits the prompt;
// write-only entries (w) for the same path still escalate via
// promptForPath.
func (g *Gate) CheckFileRead(ctx context.Context, toolName, path string) error {
	if g.sessionToolAllowed(toolName) {
		return nil
	}
	access, err := g.scope.AccessFor(path)
	if err != nil {
		return err
	}
	if access.Allows(AccessRead) {
		return nil
	}
	return g.promptForPath(ctx, toolName, path, AccessRead)
}

// CheckFileWrite gates a mutating file operation. Paths the scope
// grants write to (w or rw) still go through mode-aware approval
// (ask mode prompts; allow/yolo proceed unless deny rule hits).
// Paths not covered for writes — even if the same scope entry
// permits reads — escalate via the path-scope prompt.
func (g *Gate) CheckFileWrite(ctx context.Context, toolName, path string) error {
	if g.sessionToolAllowed(toolName) {
		return nil
	}
	access, err := g.scope.AccessFor(path)
	if err != nil {
		return err
	}
	if !access.Allows(AccessWrite) {
		return g.promptForPath(ctx, toolName, path, AccessWrite)
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
	// Verb-scoped session allow (bash only today). Sits between the
	// per-call session allow and the prompt: if the user previously
	// chose DecisionAllowSessionVerb for "<verb>", every subsequent
	// command starting with that verb is approved without re-prompting.
	var verb string
	if kind == PromptKindBash {
		verb = extractBashVerb(key)
		if verb != "" && g.sessionVerbAllowed(toolName, verb) {
			return nil
		}
	}
	mode := g.Mode()
	switch mode {
	case ModeYolo:
		return nil
	case ModeAllow:
		return fmt.Errorf("%s requires an allowlist entry in 'allow' mode: %q", toolName, key)
	case ModePlan:
		return fmt.Errorf("%s denied: tool execution disabled in 'plan' mode — cycle the permission chip (Shift+Tab) to leave plan mode", toolName)
	case ModeAcceptEdits:
		// AcceptEdits auto-approves file-write tool calls (R-PERM-7
		// "accept all edits" semantics). Everything else still goes
		// through the ask path so the operator stays in control of
		// shell / generic tool calls.
		if kind == PromptKindFileWrite {
			return nil
		}
		fallthrough
	case ModeAsk:
		return g.prompt(ctx, PromptRequest{
			Kind:        kind,
			ToolName:    toolName,
			Detail:      key,
			PersistTool: persistTool,
			PersistKey:  persistKey,
			Verb:        verb,
			Source:      SubagentSourceFromContext(ctx),
		})
	}
	return fmt.Errorf("%s denied: unknown permission mode %q", toolName, mode)
}

func (g *Gate) promptForPath(ctx context.Context, toolName, path string, op Access) error {
	mode := g.Mode()
	if mode == ModeYolo {
		return nil
	}
	if mode == ModeAllow {
		return fmt.Errorf("%s denied: path %q is outside scope and 'allow' mode does not prompt", toolName, path)
	}
	if mode == ModePlan {
		return fmt.Errorf("%s denied: tool execution disabled in 'plan' mode (path %q)", toolName, path)
	}
	// AcceptEdits auto-allows out-of-scope writes so a refactor can
	// touch sibling repos without re-prompting every file. Reads
	// still ask — the operator explicitly opted into "accept edits"
	// not "expose new paths."
	if mode == ModeAcceptEdits && op == AccessWrite {
		return nil
	}
	return g.prompt(ctx, PromptRequest{
		Kind:        PromptKindPathScope,
		ToolName:    toolName,
		Detail:      fmt.Sprintf("%s %s (out of scope)", opLabel(op), path),
		PersistTool: "path_scope",
		PersistKey:  path,
		Source:      SubagentSourceFromContext(ctx),
		Access:      op,
	})
}

// opLabel renders an Access op as the verb the prompt UI shows in
// the Detail line ("read /path" / "write /path"). Kept tight so the
// path stays visible inside the modal width budget.
func opLabel(a Access) string {
	switch a {
	case AccessRead:
		return "read"
	case AccessWrite:
		return "write"
	default:
		return a.String()
	}
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
	case DecisionAllowSessionVerb:
		// Verb-scoped trust covers every subsequent command with the
		// same leading verb for the rest of this session. We also
		// remember the *current* exact request so a repeat of this
		// (or an empty-Verb fallback) doesn't re-prompt before the
		// next call's verb match.
		if req.Verb != "" {
			g.rememberSessionVerb(req.ToolName, req.Verb)
		}
		g.rememberSession(req.ToolName, req.Detail)
		// Record under a synthetic key so the approval log surfaces
		// the verb-pattern intent (e.g. "git *") rather than the
		// specific Detail string that triggered the prompt.
		key := req.Detail
		if req.Verb != "" {
			key = req.Verb + " *"
		}
		g.recordApproval(req.ToolName, key, d)
		return nil
	case DecisionAllowSessionTool:
		g.rememberSessionTool(req.ToolName)
		g.rememberSession(req.ToolName, req.Detail)
		g.recordApproval(req.ToolName, req.Detail, d)
		return nil
	case DecisionAllowAlways:
		g.rememberSession(req.ToolName, req.Detail)
		if req.Kind == PromptKindPathScope {
			// Asymmetric op promotion from the interactive prompt:
			//   write-always → install ReadWrite
			//   read-always  → install Read (writes still gate)
			//
			// Rationale: every realistic workflow that writes a
			// file also reads it back (verify, then edit, then
			// re-read). The reverse is NOT true — granting write
			// from a read prompt would surprise the operator who
			// said "always allow this read" and silently broaden
			// their grant.
			//
			// Write-only paths are a deliberate security posture
			// (append-only logs, credential-drop dirs, one-way
			// exports) and are still expressible directly in
			// .agents/config.json with `"path:w"` syntax. We just
			// don't reach that state through an interactive
			// always-allow click — operators who want it
			// configure it explicitly.
			access := req.Access
			switch access {
			case AccessNone:
				access = AccessRead
			case AccessWrite:
				access = AccessReadWrite
			}
			g.scope.AddAlwaysAllow(expandAlwaysAllowPattern(req.PersistKey), access)
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

// sessionVerbAllowed reports whether the user has trusted toolName for
// every command starting with verb for the rest of this session via
// DecisionAllowSessionVerb.
func (g *Gate) sessionVerbAllowed(toolName, verb string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	_, ok := g.sessionAllowVerbs[toolName+"|"+verb]
	return ok
}

func (g *Gate) rememberSessionVerb(toolName, verb string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.sessionAllowVerbs[toolName+"|"+verb] = struct{}{}
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
		Mode:  g.Mode(),
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
	mode := g.Mode()
	if mode == ModeYolo {
		return ToolGateAllowed
	}
	if mode == ModePlan {
		// Plan mode disables every tool call regardless of policy.
		return ToolGateDenied
	}
	if matchAny(g.policy.allowRules(), toolName, "") {
		return ToolGateAllowed
	}
	if mode == ModeAllow {
		return ToolGateDeniedInAllowMode
	}
	// AcceptEdits would auto-allow file-write tools, but ToolGateState
	// runs without the call's Kind so it can't distinguish edit
	// tools from other tools — degrades to "prompted".
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

// expandAlwaysAllowPattern broadens a path argument from a
// DecisionAllowAlways prompt into a subtree pattern so a single
// approval covers sibling files / nested subdirectories — what
// the operator almost certainly wants. Matches the conventions
// in Cursor / VS Code / Claude Code's prompt UX.
//
// Rules:
//   - Path is an existing directory → "<path>/...".
//   - Path is anything else (existing file, or a not-yet-created
//     write_file target) → "<parent>/..." so siblings in the same
//     directory don't re-prompt.
//
// One os.Stat per always-allow decision is cheap; we trade one
// syscall on grant-time for not asking the same question N times
// over the rest of the session.
func expandAlwaysAllowPattern(path string) string {
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		return strings.TrimRight(path, string(filepath.Separator)) + "/..."
	}
	return filepath.Dir(path) + "/..."
}
