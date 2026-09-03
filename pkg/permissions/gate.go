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
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-steer/core-agent/v2/pkg/config"
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

	// By names the principal that approved, when the prompter could
	// attribute the answer (see Approval.By). Empty for the ordinary
	// single-operator case — a person at a terminal is identified by
	// being the person at the terminal — and empty is also what an
	// unattributable answer records, never a guess.
	By string
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

	// ModeAcceptEdits auto-allows file-write tool calls — INCLUDING
	// out-of-scope writes to ANY path (see promptForPath) — without
	// prompting; every other tool kind still flows through the
	// normal Ask path. Used by core-tui's "acceptEdits" chip so the
	// operator can stream a refactor without clicking through every
	// diff modal. This is "trust this agent with your filesystem"
	// mode; see the promptForPath comment for the full blast-radius
	// warning.
	ModeAcceptEdits Mode = "acceptEdits"
)

// Gate is the central permission chokepoint consulted before each tool
// call. It holds the configured policy, the path scope, the bash
// denylist (built-in), and an optional Prompter for interactive use.
//
// Gate is safe for concurrent use; tool handlers run in the agent's
// event-iteration goroutine, but the prompter call may yield while
// waiting for the user.
//
// Multi-session deployments build one template Gate at daemon start
// (typically via FromConfig) and derive a per-session sub-gate for
// each agent via DeriveForSession. Sub-gates share the template's
// daemon-wide configuration (policy / scope / requirePlanArtifact)
// but carry their own per-session mutable state (sessionAllow /
// sessionAllowTools / sessionAllowVerbs / approvals / planRecorded /
// prompter / mode). See docs/multi-session-design.md.
type Gate struct {
	mu sync.Mutex

	// sessionID is set when this gate was created by DeriveForSession.
	// Empty on template gates and on gates built directly via New /
	// FromConfig (single-session deployments). Used for diagnostics
	// and future audit-log threading.
	sessionID string

	mode     Mode
	policy   *Policy
	scope    *PathScope
	prompter Prompter

	// grants persists DecisionAllowAlways outcomes across restarts.
	// Nil disables persistence (the grant still applies in-memory
	// for the process lifetime). Shared by reference across
	// DeriveForSession sub-gates, consistent with the daemon-wide
	// Policy/PathScope mutation rules documented there.
	grants GrantStore

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

	// requirePlanArtifact + planRecorded implement the plan-first
	// gating pre-check. When requirePlanArtifact is true, mutating
	// tool calls are denied until planRecorded flips to true (via
	// MarkPlanRecorded, called by the record_plan tool's handler).
	// See docs/plan-first-design.md.
	requirePlanArtifact bool
	planRecorded        bool

	// planGatedTools, when non-nil, is the set of tool names the host
	// registered that planFirstDenial would actually deny before a plan
	// exists. Populated additively by RegisterPlanGatedTools —
	// tools.Build for the built-ins, GateToolset for each namespaced
	// toolset — because the gate is constructed long before anyone
	// knows which catalog it will serve.
	//
	// The reason it exists is the same one behind nativeSearchTools:
	// operator- and model-facing text should be able to state what this
	// build gates rather than recite a category the config may have
	// emptied. record_plan used to answer "mutating tools are now
	// unblocked" unconditionally, which in the 2026-08-14 GKE recipe was
	// wrong twice over — bash/write_file/edit_file/delete_file were all
	// disabled, and what the plan actually unblocked was the entire
	// `gke` MCP read surface (#747).
	//
	// nil means "no host ever said", which is not the same as "nothing
	// is gated": a library caller that wires tools by hand gets prose
	// that declines to enumerate rather than a confident empty list.
	planGatedTools map[string]bool

	// bashSearchGate is the resolved search-gate posture ("enforce" |
	// "warn" | "allow"). Immutable after New, so it needs no lock and
	// is inherited as-is by DeriveForSession sub-gates. See
	// searchgate.go and #158.
	bashSearchGate string
	// nativeSearchTools, when non-nil, is the set of native tool names
	// the host actually registered ("grep", "glob"). The search gate's
	// whole value is that its refusal names a replacement, so a build
	// that dropped `grep` from the catalog must not be told to use it —
	// that would be the same unenforceable-claim failure the gate exists
	// to prevent, pointed the other way. Set once by tools.Build before
	// the gate serves any call; nil means "assume registered", so a host
	// that wires tools by hand keeps the gate rather than silently
	// disarming it.
	nativeSearchTools map[string]bool
	// registeredTools, when non-nil, is the set of built-in tool names
	// this build actually registered. It exists for model-facing
	// DESCRIPTION text, not for gating: descriptions routinely
	// cross-reference other tools ("PREFERRED over `bash cat`", "call
	// this BEFORE any write_file / bash call"), and on a build that
	// dropped those tools the sentence is worse than noise — it asserts
	// a capability the model does not have. A distroless deployment
	// with no shell is the standard case.
	//
	// Same contract as nativeSearchTools: set once by tools.Build
	// before the gate serves a call, and nil means "assume registered"
	// so a host that wires tools by hand keeps today's text verbatim.
	//
	// SCOPE: only tools.Build's own catalog. Tools wired elsewhere
	// (spawn_agent, MCP tools) are absent from the map and must not be
	// filtered through it — HasTool would report them missing when
	// they are merely unknown.
	registeredTools map[string]bool
}

// planExemptTools is the set of tool names that bypass the plan-
// first pre-check even when RequirePlanArtifact is set. Three
// categories:
//
//   - Read-only research tools — research has to happen BEFORE the
//     plan can be written; gating reads would deadlock the workflow.
//   - record_plan itself — the escape valve. The tool whose call
//     flips planRecorded can't itself be plan-gated.
//   - Read-only introspection: enumerating available skills. This is
//     the "what tools do I have?" question the model reasonably asks
//     before deciding what to plan.
//
// Note on namespaces: skill tools (list_skills / load_skill /
// load_skill_resource) and MCP tools are registered through
// GateToolset in pkg/tools/gate.go, which routes every underlying
// tool through gate.CheckGeneric with the namespace as the toolName.
// That's why the entry below is "skill" (the namespace) rather than
// each individual skill tool name — the gate never sees the
// underlying names for these categories. "mcp" is deliberately NOT
// exempt: MCP servers expose arbitrary tools including mutating
// ones, so recipes should judge case-by-case rather than blanket-
// exempt the namespace. The case-by-case answer arrived in #693 and
// does not live in this table: pkg/tools' gate wrapper classifies
// each call with tools.IsReadOnlyTool and routes read-only ones to
// CheckReadOnlyToolCall, which exempts them via planFirstDenial's
// readOnly parameter. A name table can't express that — the name is
// "mcp" for every tool from every server.
//
// Anything not in this set (write_file/edit_file/delete_file/bash,
// fetch_url, spawn_agent, spawn_remote_agent, every MCP tool) is
// plan-gated. This matches the original design's Q1 ("gate everything
// by default; per-server allowlist later if it bites") and Q3
// ("subagents inherit the parent's planRecorded flag — gate spawn
// family so subagents only run under an approved plan").
//
// This paragraph was aspirational until #758: pkg/agent/background
// consulted no gate at all, so under plan_mode=required a model was
// told spawn_agent would be denied, called it, and it ran. The spawn
// doors call CheckGeneric now. stop_agent, which the earlier wording
// swept in as "spawn_agent family", is deliberately NOT gated: it
// cancels, so every denial of it leaves running exactly what the model
// was trying to stop. See NewStopAgentTool for the full argument.
//
// fetch_url is deliberately NOT exempt (#385): it is network egress,
// and an outbound GET whose URL the model controls is an exfiltration
// channel — query strings can carry anything the model has read.
// Letting it run before a plan is recorded defeats the point of
// plan-first gating. Research that needs the network happens after
// record_plan, or the operator allowlists specific hosts and accepts
// the trade-off consciously.
var planExemptTools = map[string]bool{
	// Read-only filesystem + research tools
	"read_file":       true,
	"read_many_files": true,
	"stat":            true,
	"list_dir":        true,
	"glob":            true,
	"grep":            true,
	"json_query":      true,
	"todo":            true,
	"record_plan":     true,

	// Read-only skill introspection, exempt at NAMESPACE level: skill
	// tools are registered through GateToolset(ts, gate, "skill") in
	// pkg/skills/load.go, so "skill" (the namespace) is the only
	// toolName planFirstDenial ever sees for them — a per-tool exempt
	// entry is impossible at this layer. The blanket exemption is safe
	// because every tool in the namespace today (list_skills /
	// load_skill / load_skill_resource) only READs from the skills
	// registry; none mutates state or touches the network. If a
	// mutating or network-capable skill tool is ever added to the
	// namespace, this entry must be revisited — the exemption would
	// silently cover it too.
	"skill": true,
}

// Options configures a Gate at construction time. All fields are
// optional; sensible defaults apply when omitted.
type Options struct {
	Mode     Mode
	Policy   *Policy
	Scope    *PathScope
	Prompter Prompter // nil = no interactive path; ask-mode unresolved → deny

	// GrantStore persists "allow always" grants beyond the process
	// lifetime. nil = grants apply in-memory only (pre-v2.8 behavior).
	// See the GrantStore interface docs; wire the bundled
	// config-backed implementation or your own.
	GrantStore GrantStore

	// RequirePlanArtifact, when true, denies mutating tool calls
	// (write_file/edit_file/delete_file/bash, spawn family, MCP
	// tools, and anything else not in planExemptTools) until the
	// model has called the record_plan tool at least once this
	// session. Read tools and record_plan itself are exempt so
	// research happens normally and the model has an escape valve.
	//
	// Composes with every existing Mode. Even ModeYolo respects
	// the plan-first pre-check; once a plan is recorded, the mode's
	// usual semantics resume. See docs/plan-first-design.md.
	RequirePlanArtifact bool

	// BashSearchGate selects what CheckBash does with a search-shaped
	// command (`grep -rn foo .`, `find . -name '*.go'`) while native
	// grep/glob tools exist: "enforce" (default) refuses with a
	// structured error naming the replacement, "warn" allows it and
	// lets the caller surface a notice, "allow" disables the check.
	// Empty == "enforce". See searchgate.go and #158.
	BashSearchGate string
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
	if opts.BashSearchGate == "" {
		opts.BashSearchGate = config.BashSearchGateEnforce
	}
	return &Gate{
		mode:                opts.Mode,
		policy:              opts.Policy,
		scope:               opts.Scope,
		prompter:            opts.Prompter,
		grants:              opts.GrantStore,
		sessionAllow:        make(map[string]struct{}),
		sessionAllowTools:   make(map[string]struct{}),
		sessionAllowVerbs:   make(map[string]struct{}),
		requirePlanArtifact: opts.RequirePlanArtifact,
		bashSearchGate:      opts.BashSearchGate,
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
	return New(Options{
		Mode:     mode,
		Policy:   policy,
		Scope:    scope,
		Prompter: prompter,
		// PlanGateArmed, not the raw RequirePlanArtifact bool: advisory
		// mode registers record_plan and persists the artifact but must
		// never deny a mutating call on plan state.
		RequirePlanArtifact: cfg.Permissions.PlanGateArmed(),
		BashSearchGate:      cfg.Safety.BashSearchGate,
	}), nil
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

// DeriveForSession returns a per-session sub-gate derived from this
// (template) gate. The sub-gate shares the template's daemon-wide
// configuration by reference — Policy, PathScope, requirePlanArtifact
// — and carries its own per-session mutable state: sessionAllow /
// sessionAllowTools / sessionAllowVerbs / approvals / planRecorded
// start empty, and Mode is copied so per-session SetMode (e.g., a
// TUI chip toggle) doesn't bleed into the template or sibling sessions.
//
// prompter is the per-session interactive handler — typically the
// HTTP-driven broker for an attach-mode session, or stdin for a
// local interactive run. nil disables interactive prompting on this
// sub-gate (ask-mode calls then fail with ErrNoPrompter, same as a
// directly-constructed Gate without a prompter).
//
// sessionID is stored for diagnostics; an empty string is accepted
// for back-compat with callers that haven't threaded it through yet.
//
// Limitations (documented because operators read this surface):
//   - Policy mutations via AddAllowPatterns / AddDenyPatterns mutate
//     the shared template Policy and therefore affect every derived
//     sub-gate. /allow + /deny are intentionally daemon-wide today
//     per docs/multi-session-design.md §"Per-substrate isolation
//     rules"; per-session policy carve-outs are a follow-up.
//   - PathScope mutations via AddAlwaysAllow (triggered by
//     DecisionAllowAlways) similarly mutate the shared scope.
//
// Both limitations are by design for v2.4 — the typical operator
// model is "one config, many users" with per-user authorization on
// top of a shared substrate. Per-session policy/scope isolation can
// layer on later without changing this method's shape.
func (template *Gate) DeriveForSession(sessionID string, prompter Prompter) *Gate {
	template.mu.Lock()
	mode := template.mode
	template.mu.Unlock()
	return &Gate{
		sessionID:           sessionID,
		mode:                mode,
		policy:              template.policy,
		scope:               template.scope,
		prompter:            prompter,
		grants:              template.grants,
		sessionAllow:        make(map[string]struct{}),
		sessionAllowTools:   make(map[string]struct{}),
		sessionAllowVerbs:   make(map[string]struct{}),
		requirePlanArtifact: template.requirePlanArtifact,
		// Inherited, not defaulted: a sub-gate that dropped this would
		// leave the search gate off for every daemon-created session
		// while --print-config still reported it on.
		bashSearchGate:    template.bashSearchGate,
		nativeSearchTools: template.nativeSearchTools,
		registeredTools:   template.registeredTools,
		// Also inherited: the catalog is daemon-wide, so a sub-gate
		// that dropped this would make record_plan stop naming what it
		// unblocked for exactly the sessions an operator is watching.
		planGatedTools: template.planGatedToolSet(),
	}
}

// planGatedToolSet returns the raw set under lock, for DeriveForSession.
// Shares the map by reference: registration happens at startup, before
// any session is derived, and RegisterPlanGatedTools copy-on-writes so
// a late registration can't race a reader.
func (g *Gate) planGatedToolSet() map[string]bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.planGatedTools
}

// RegisterPlanGatedTools tells the gate which tool names this build
// actually registered, so operator- and model-facing text can name the
// set plan-first gating covers instead of asserting a category (#747).
// Names in planExemptTools are dropped — the caller declares what it
// registered and the gate decides what that means, the same split
// SetNativeSearchTools uses.
//
// Additive, and safe to call more than once: the built-ins land at
// tools.Build time and each namespaced toolset (mcp, skill) registers
// as it is wrapped, which happens at several points during startup.
// Calling it with no names still marks the set as known — that is how
// a host says "I registered nothing plan-gated", which is a true and
// useful thing for record_plan to be able to report.
func (g *Gate) RegisterPlanGatedTools(names ...string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	// Copy-on-write: DeriveForSession hands the map out by reference.
	next := make(map[string]bool, len(g.planGatedTools)+len(names))
	for k, v := range g.planGatedTools {
		next[k] = v
	}
	for _, n := range names {
		if n == "" || planExemptTools[n] {
			continue
		}
		next[n] = true
	}
	g.planGatedTools = next
}

// PlanGatedTools returns the registered plan-gated tool names, sorted.
// known is false when no host ever called RegisterPlanGatedTools, which
// callers must render as "can't say" rather than "nothing" — see the
// field comment. known is true with an empty slice when the host did
// call it and every name it registered was exempt.
func (g *Gate) PlanGatedTools() (names []string, known bool) {
	g.mu.Lock()
	set := g.planGatedTools
	g.mu.Unlock()
	if set == nil {
		return nil, false
	}
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	return out, true
}

// SessionID returns the session identifier this gate was derived for,
// or "" when the gate was built directly via New / FromConfig (i.e.,
// it IS the template). Useful for diagnostics; not exposed in the
// JSON Snapshot.
func (g *Gate) SessionID() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.sessionID
}

// MarkPlanRecorded flips the per-gate planRecorded flag. Called by
// the record_plan tool's handler after the plan artifact has been
// written. Idempotent — calling twice is harmless.
//
// After this returns, subsequent mutating tool calls bypass the
// plan-first pre-check and resume the configured Mode's normal
// gating semantics.
func (g *Gate) MarkPlanRecorded() {
	g.MarkPlanRecordedOnce()
}

// MarkPlanRecordedOnce is MarkPlanRecorded plus the answer to "did
// this call open the gate?" — true only for the transition, false if a
// plan was already recorded.
//
// It exists because reading IsPlanRecorded and then marking is two
// operations, and ADK dispatches a turn's function calls concurrently:
// two record_plan calls in one turn could both observe an unset flag
// and both announce an unblock that only happened once. record_plan
// uses the answer to decide whether to announce at all (#906) — a
// re-announcement of a transition that didn't happen is what a loop
// reads as progress.
func (g *Gate) MarkPlanRecordedOnce() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	opened := !g.planRecorded
	g.planRecorded = true
	return opened
}

// ClearPlanRecorded resets the per-gate planRecorded flag. Called
// by the /replan slash handler when the operator rejects the
// current plan; the model is forced back through record_plan
// before any further mutating tool call.
func (g *Gate) ClearPlanRecorded() {
	g.mu.Lock()
	g.planRecorded = false
	g.mu.Unlock()
}

// IsPlanRecorded reports whether a plan has been recorded this
// session. Exposed so the TUI can render a "plan recorded" badge
// and so /replan can short-circuit if no plan exists to revoke.
func (g *Gate) IsPlanRecorded() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.planRecorded
}

// PlanRequired reports whether RequirePlanArtifact was set at
// construction. Exposed so TUI / config-introspection code can tell
// the difference between "no plan recorded but it's not required"
// and "no plan recorded and we're gated".
func (g *Gate) PlanRequired() bool {
	// Read-only after construction; no lock needed.
	return g.requirePlanArtifact
}

// planFirstDenial returns a non-nil error if the plan-first gating
// rule blocks this tool call. Called early in gateRequest and
// promptForPath so the pre-check runs BEFORE mode-based logic —
// even ModeYolo denies until the plan is recorded.
//
// Returns nil (allow continuation) in four cases:
//  1. RequirePlanArtifact wasn't set at construction.
//  2. The tool is in planExemptTools (research tools + record_plan).
//  3. The caller classified this call read-only. Plan-first gates
//     MUTATION; a read is research, and research is what produces the
//     plan. Built-in reads get this from planExemptTools by name; the
//     parameter is how a namespaced toolset — where the only name the
//     gate ever sees is the namespace, "mcp" — says the same thing per
//     call (#693). Callers pass the tools.IsReadOnlyTool verdict, which
//     is fail-safe mutating for anything that hasn't declared itself.
//  4. A plan has already been recorded this session.
func (g *Gate) planFirstDenial(toolName string, readOnly bool) error {
	if !g.requirePlanArtifact {
		return nil
	}
	if readOnly || planExemptTools[toolName] {
		return nil
	}
	g.mu.Lock()
	recorded := g.planRecorded
	g.mu.Unlock()
	if recorded {
		return nil
	}
	return fmt.Errorf("%s denied: plan-first mode requires record_plan to be called before any mutating tool. Call record_plan(plan: <your-markdown-plan>) first, then retry", toolName)
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

// SetGrantStore swaps the gate's grant persistence backend. Mirrors
// SetPrompter for hosts that construct the gate before the store's
// dependencies (e.g. the resolved .agents dir) are known. Set to nil
// to disable persistence — DecisionAllowAlways grants then apply for
// the process lifetime only.
func (g *Gate) SetGrantStore(s GrantStore) { g.grants = s }

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

// resolveSessionGate returns the per-session sub-gate threaded on
// ctx via permissions.WithSessionGate, falling back to g itself when
// no override is present. The single-line check at the top of every
// public Check* method is how tool wrappers constructed against the
// daemon-wide template gate route their permission checks through
// the per-session sub-gate's prompter (the per-session attach
// broker) without each tool having to know about session gating.
//
// The sg != g guard prevents the trivial self-loop when a session
// gate calls its own Check* method (which would recurse into
// itself); any other "chained" override case isn't possible in our
// architecture because WithSessionGate is only called once per turn
// by agent.Run.
func (g *Gate) resolveSessionGate(ctx context.Context) *Gate {
	if sg, ok := SessionGateFromContext(ctx); ok && sg != g {
		return sg
	}
	return g
}

// CheckGeneric gates an arbitrary tool call (used by MCP and skill
// toolsets, where we don't have a dedicated Check<Tool> method).
//
// toolName is the namespace under which policy lookups happen
// (typically "mcp" or "skill"); key is the human-readable detail
// shown in prompts (typically the tool's full namespaced name plus
// a brief argument summary).
//
// A DecisionAllowSessionTool grant made through this entry point is
// keyed by toolName (the whole namespace). Prefer CheckToolCall for
// namespaced toolsets so the grant is scoped per underlying tool.
func (g *Gate) CheckGeneric(ctx context.Context, toolName, key string) error {
	g = g.resolveSessionGate(ctx)
	return g.gateRequest(ctx, PromptKindGeneric, toolName, key, toolName, key, toolName, false)
}

// CheckToolCall gates a call to a specific tool within a namespaced
// toolset (MCP, skills). namespace is the policy bucket used for
// allow/deny lookups ("mcp"/"skill"); tool is the underlying tool's
// name; key is the prompt detail (typically tool + arg summary).
//
// Unlike CheckGeneric, a DecisionAllowSessionTool grant here is keyed
// per underlying tool ("<namespace>/<tool>"), so "allow every call to
// this tool for the session" trusts only the tool the user actually
// saw named in the prompt — not every tool from every MCP server or
// the whole skills surface (#379). Policy allow/deny matching still
// uses namespace, preserving the "mcp:<tool>" / "skill:<tool>"
// pattern grammar.
func (g *Gate) CheckToolCall(ctx context.Context, namespace, tool, key string) error {
	return g.checkToolCall(ctx, namespace, tool, key, false)
}

// CheckReadOnlyToolCall is CheckToolCall for a call the caller has
// classified read-only (tools.IsReadOnlyTool). Everything about the
// check is identical except plan-first gating, which the classification
// exempts: plan-first is a rule about mutating before there is a plan,
// and a namespaced toolset is the one place the gate can't tell reads
// from writes on its own, because the only name it sees is the
// namespace (#693).
//
// It is a sibling rather than a fifth parameter on CheckToolCall
// because pkg/permissions is inside the stability promise — hosts
// embedding the gate keep compiling, and a caller that doesn't know
// about dispatch classes keeps getting the conservative answer.
//
// Policy allow/deny, permission mode, and prompting are untouched: a
// read-only MCP tool in ask mode still prompts.
func (g *Gate) CheckReadOnlyToolCall(ctx context.Context, namespace, tool, key string) error {
	return g.checkToolCall(ctx, namespace, tool, key, true)
}

func (g *Gate) checkToolCall(ctx context.Context, namespace, tool, key string, readOnly bool) error {
	g = g.resolveSessionGate(ctx)
	return g.gateRequest(ctx, PromptKindGeneric, namespace, key, namespace, key, sessionToolKey(namespace, tool), readOnly)
}

// sessionToolKey builds the per-underlying-tool session-grant key for
// a namespaced toolset. The "/" separator can't appear in a bare
// namespace, so the key is unambiguous and stable across grant and
// check time (#379).
func sessionToolKey(namespace, tool string) string {
	return namespace + "/" + tool
}

// CheckBash gates a bash invocation. The built-in denylist is checked
// first; it is not overridable by config, but it is best-effort
// defense-in-depth, NOT a security boundary — a small pattern set that
// is trivially evadable (see denylist.go). Then the search gate (#158)
// refuses search-shaped commands in enforce mode. After that, policy +
// mode determine whether the call needs a prompt. For a real bash
// boundary, run in allow/ask mode with an explicit allowlist rather
// than relying on the denylist.
func (g *Gate) CheckBash(ctx context.Context, command string) error {
	g = g.resolveSessionGate(ctx)
	command = strings.TrimSpace(command)
	if denied, reason := IsBashDenied(command); denied {
		return fmt.Errorf("bash refused: %s", reason)
	}
	if g.bashSearchGate == config.BashSearchGateEnforce {
		if binary, native, hit := SearchShapedCommand(command); hit && g.nativeRegistered(native) {
			return fmt.Errorf("bash refused: %s", SearchGateMessage(binary, native))
		}
	}
	return g.gateRequest(ctx, PromptKindBash, "bash", command, "bash", command, "bash", false)
}

// BashSearchGate reports the resolved search-gate posture.
func (g *Gate) BashSearchGate() string { return g.bashSearchGate }

// SetNativeSearchTools tells the gate which native search tools the
// host registered, keyed by tool name ("grep", "glob"). Called by
// tools.Build at construction time, before the gate serves a call, so
// no lock is taken — mirrors SetPrompter / SetGrantStore.
func (g *Gate) SetNativeSearchTools(registered map[string]bool) {
	g.nativeSearchTools = registered
}

// NativeSearchTools returns what SetNativeSearchTools was given, so a
// host that built its catalog against a derived gate can forward the
// same knowledge to the template every later session derives from.
func (g *Gate) NativeSearchTools() map[string]bool { return g.nativeSearchTools }

// SetRegisteredTools tells the gate which built-in tools the host
// registered, so description text can drop cross-references to tools
// that aren't there. Same timing contract as SetNativeSearchTools:
// called by tools.Build at construction, before the gate serves a
// call, so no lock is taken.
func (g *Gate) SetRegisteredTools(registered map[string]bool) {
	g.registeredTools = registered
}

// RegisteredTools returns what SetRegisteredTools was given, so a host
// that built its catalog against a derived gate can forward the same
// knowledge to the template every later session derives from.
func (g *Gate) RegisteredTools() map[string]bool { return g.registeredTools }

// HasTool reports whether name is registered in this build, for
// description text that would otherwise point the model at a tool it
// cannot call. Unset map means "assume registered" — an unconfigured
// gate must not silently strip every cross-reference.
//
// Only ask about tools tools.Build registers; see registeredTools.
func (g *Gate) HasTool(name string) bool {
	// Nil-receiver-safe: bashDescription and friends already tolerate a
	// nil gate, and "assume registered" is the same answer an unset map
	// gives — a description must never lose text just because a caller
	// wired tools without a gate.
	if g == nil || g.registeredTools == nil {
		return true
	}
	return g.registeredTools[name]
}

func (g *Gate) nativeRegistered(name string) bool {
	if g.nativeSearchTools == nil {
		return true
	}
	return g.nativeSearchTools[name]
}

// ActiveSearchBinaries lists the search binaries this gate would
// actually act on — the gated set minus any whose native replacement
// isn't registered. Empty means the gate is inert no matter what the
// posture says, which is a thing operator-facing output should be able
// to state plainly rather than let an operator infer.
func (g *Gate) ActiveSearchBinaries() []string {
	out := make([]string, 0, len(searchBinaries))
	for _, name := range SearchGatedBinaries() {
		if g.nativeRegistered(searchBinaries[name]) {
			out = append(out, name)
		}
	}
	return out
}

// ActiveNativeSearchTools lists the native tools ActiveSearchBinaries
// would redirect to, deduped and sorted. Operator- and model-facing
// text uses this rather than a literal "grep/glob" so a build that
// registered only one of them doesn't advertise the other.
func (g *Gate) ActiveNativeSearchTools() []string {
	var out []string
	seen := map[string]bool{}
	for _, name := range g.ActiveSearchBinaries() {
		native := searchBinaries[name]
		if seen[native] {
			continue
		}
		seen[native] = true
		out = append(out, native)
	}
	sort.Strings(out)
	return out
}

// BashSearchNotice returns the advisory text for a search-shaped
// command in "warn" mode, or "" when the gate is not in warn mode or
// the command isn't search-shaped.
//
// Warn mode needs a channel back to the model, and the tool result is
// the one that already exists and is guaranteed in-context on the very
// next inference — the whole point of #158 is feedback at the moment
// of the wrong choice, and a notice delivered a turn later is a notice
// delivered after the model has already built on the result. Callers
// (the bash tool) attach this to their output; CheckBash itself stays
// a pure allow/deny so hooks and other callers are unaffected.
//
// Resolves the ctx session gate for the same reason CheckBash does:
// in a multi-session daemon the shared bash tool closes over the
// daemon's gate, and the advice has to come from the gate that would
// have made the decision.
func (g *Gate) BashSearchNotice(ctx context.Context, command string) string {
	g = g.resolveSessionGate(ctx)
	if g.bashSearchGate != config.BashSearchGateWarn {
		return ""
	}
	binary, native, hit := SearchShapedCommand(strings.TrimSpace(command))
	if !hit || !g.nativeRegistered(native) {
		return ""
	}
	return SearchGateMessage(binary, native)
}

// CheckFileRead gates a read-only file operation. An allow-list
// entry that grants read (r or rw) short-circuits the prompt;
// write-only entries (w) for the same path still escalate via
// promptForPath.
func (g *Gate) CheckFileRead(ctx context.Context, toolName, path string) error {
	g = g.resolveSessionGate(ctx)
	// Scope is consulted BEFORE any per-tool session grant (#380): a
	// session-tool grant may suppress the mode prompt for in-scope
	// operations, but it must not silently drop the path boundary and
	// let the tool read arbitrary out-of-scope files. In-scope reads
	// already pass without a prompt; out-of-scope reads still escalate
	// via promptForPath even when the tool is trusted for the session.
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
	g = g.resolveSessionGate(ctx)
	// Control-plane classification runs FIRST and on the symlink-
	// resolved path, before any mode/session/allowlist short-circuit,
	// so a write (or a symlink laundering one) to .agents/config.json
	// or .agents/mcp.json cannot be auto-approved by yolo, acceptEdits,
	// a session-tool grant, or an allowlist entry. See #378.
	if resolved, err := ResolvePath(path); err == nil && isControlPlanePath(resolved) {
		return g.checkControlPlaneWrite(ctx, toolName, resolved)
	}
	// Scope is consulted BEFORE the per-tool session grant (#380).
	// An out-of-scope write always escalates via promptForPath, even
	// when the tool is trusted for the session — the grant suppresses
	// the mode prompt for in-scope writes only, it does not widen the
	// path boundary.
	access, err := g.scope.AccessFor(path)
	if err != nil {
		return err
	}
	if !access.Allows(AccessWrite) {
		return g.promptForPath(ctx, toolName, path, AccessWrite)
	}
	if g.sessionToolAllowed(toolName) {
		return nil
	}
	return g.gateRequest(ctx, PromptKindFileWrite, toolName, path, toolName, path, toolName, false)
}

// checkControlPlaneWrite is the elevated gate for privilege-bearing
// control-plane files (#378). It deliberately bypasses every
// auto-approval path: mode (yolo/acceptEdits), session/verb/tool
// grants, allowlist entries, and built-in bundles are all ignored.
// A configured deny rule still wins (deny is always maximal), the
// plan-first pre-check still applies, and otherwise the write
// requires a fresh interactive approval every time. With no prompter
// wired the write is denied with a clear, actionable error.
//
// A non-deny decision authorizes only THIS write — nothing is
// remembered, so a session-tool/always choice on a control-plane
// prompt can't install a standing bypass.
func (g *Gate) checkControlPlaneWrite(ctx context.Context, toolName, path string) error {
	if err := g.planFirstDenial(toolName, false); err != nil {
		return err
	}
	if g.policy.Match(toolName, path) == OutcomeDeny {
		return fmt.Errorf("%s denied by config policy: %q", toolName, path)
	}
	if g.prompter == nil {
		return fmt.Errorf("%s denied: %q is a privilege-bearing control-plane file (%w); it can only be modified with an explicit interactive approval, which is unavailable in this session. Edit it directly outside the agent if the change is intended", toolName, path, ErrControlPlaneWrite)
	}
	approval, err := askApproval(ctx, g.prompter, PromptRequest{
		Kind:        PromptKindControlPlaneWrite,
		ToolName:    toolName,
		Detail:      fmt.Sprintf("modify control-plane file %s", path),
		PersistTool: toolName,
		PersistKey:  path,
		Source:      SubagentSourceFromContext(ctx),
		Access:      AccessWrite,
	})
	if err != nil {
		return fmt.Errorf("permissions: %w", err)
	}
	if approval.Decision == DecisionDeny {
		return fmt.Errorf("%s denied by user: control-plane write to %s", toolName, path)
	}
	// Any non-deny decision authorizes exactly this write. We record
	// the approval for the audit log but intentionally do NOT remember
	// it (no rememberSession / rememberSessionTool / allowlist persist)
	// so the elevated gate re-prompts on the next control-plane write.
	g.recordApproval(toolName, path, DecisionAllowOnce, approval.By)
	return nil
}

func (g *Gate) gateRequest(ctx context.Context, kind PromptKind, toolName, key, persistTool, persistKey, sessToolKey string, readOnly bool) error {
	// Plan-first pre-check runs before mode/policy logic. Even
	// ModeYolo respects it — the operator opted into "no actions
	// before plan" by setting RequirePlanArtifact. Once a plan is
	// recorded, this returns nil and normal flow resumes.
	if err := g.planFirstDenial(toolName, readOnly); err != nil {
		return err
	}
	switch g.policy.Match(toolName, key) {
	case OutcomeDeny:
		return fmt.Errorf("%s denied by config policy: %q", toolName, key)
	case OutcomeAllow:
		return nil
	}
	if g.sessionToolAllowed(sessToolKey) {
		return nil
	}
	if g.sessionAllowed(toolName, key) {
		return nil
	}
	// Verb-scoped session allow (bash only today). Sits between the
	// per-call session allow and the prompt: if the user previously
	// chose DecisionAllowSessionVerb for "<verb>", every subsequent
	// command starting with that verb is approved without re-prompting.
	//
	// The grant only applies when safecmd confirms the command is a
	// single simple command — `git status; evil` extracts verb "git"
	// but must NOT ride a `git` session grant past the prompt, since
	// the user trusted "git *", not "git-then-anything". Compound or
	// non-literal commands fall through to normal prompting.
	var verb string
	if kind == PromptKindBash {
		verb = extractBashVerb(key)
		if verb != "" && g.sessionVerbAllowed(toolName, verb) {
			if _, safe := parseSafeArgv(key); safe {
				return nil
			}
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
			Kind:           kind,
			ToolName:       toolName,
			Detail:         key,
			PersistTool:    persistTool,
			PersistKey:     persistKey,
			Verb:           verb,
			SessionToolKey: sessToolKey,
			Source:         SubagentSourceFromContext(ctx),
		})
	}
	return fmt.Errorf("%s denied: unknown permission mode %q", toolName, mode)
}

func (g *Gate) promptForPath(ctx context.Context, toolName, path string, op Access) error {
	// Plan-first pre-check first. Out-of-scope writes / reads under
	// the plan-first regime go through the same denial as gated
	// tools, so a clever bypass via "write to a path outside scope"
	// doesn't escape the gate.
	if err := g.planFirstDenial(toolName, false); err != nil {
		return err
	}
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
	//
	// SECURITY: "out-of-scope writes" means writes to ANY path the
	// process can reach — ~/.bashrc, ~/.ssh/authorized_keys, cron
	// files, systemd units — not just sibling repos. The path scope
	// is NOT a boundary in this mode; acceptEdits is "trust this
	// agent with your filesystem" and is recommended only inside a
	// sandbox/container (or an equally disposable environment).
	// Operators who want auto-approved writes ONLY within declared
	// paths should stay in ask mode and grant path_scope entries
	// (path_scope.allow / allow_paths / --allow-path) instead.
	// (Control-plane files remain the one exception: CheckFileWrite
	// routes them through the elevated prompt before mode is ever
	// consulted, so acceptEdits cannot self-escalate the gate's own
	// config.) Documented loudly in
	// docs/site/src/content/docs/concepts/permissions.md; keep the
	// two in sync. Deliberately NOT changed for #385 — this is the
	// mode's contract, the fix is making sure nobody mistakes it
	// for a scoped-writes mode.
	if mode == ModeAcceptEdits && op == AccessWrite {
		return nil
	}
	return g.prompt(ctx, PromptRequest{
		Kind:           PromptKindPathScope,
		ToolName:       toolName,
		Detail:         fmt.Sprintf("%s %s (out of scope)", opLabel(op), path),
		PersistTool:    "path_scope",
		PersistKey:     path,
		SessionToolKey: toolName,
		Source:         SubagentSourceFromContext(ctx),
		Access:         op,
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
	approval, err := askApproval(ctx, g.prompter, req)
	if err != nil {
		return fmt.Errorf("permissions: %w", err)
	}
	d := approval.Decision
	switch d {
	case DecisionAllowOnce:
		g.recordApproval(req.ToolName, req.Detail, d, approval.By)
		return nil
	case DecisionAllowSession:
		g.rememberSession(req.ToolName, req.Detail)
		g.recordApproval(req.ToolName, req.Detail, d, approval.By)
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
		g.recordApproval(req.ToolName, key, d, approval.By)
		return nil
	case DecisionAllowSessionTool:
		// Key the tool-wide grant by SessionToolKey so namespaced
		// toolsets (MCP/skill) remember per underlying tool, not per
		// whole namespace (#379). Falls back to ToolName for callers
		// that don't set it.
		key := req.SessionToolKey
		if key == "" {
			key = req.ToolName
		}
		g.rememberSessionTool(key)
		g.rememberSession(req.ToolName, req.Detail)
		g.recordApproval(req.ToolName, req.Detail, d, approval.By)
		return nil
	case DecisionAllowAlways:
		g.rememberSession(req.ToolName, req.Detail)
		grant := Grant{Kind: req.Kind, Tool: req.PersistTool, Key: req.PersistKey}
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
			grant.Pattern = expandAlwaysAllowPattern(req.PersistKey)
			grant.Access = access
			g.scope.AddAlwaysAllow(grant.Pattern, access)
		} else {
			// Non-path grants become a real in-memory policy
			// pattern, closing the pre-v2.8 gap where "allow
			// always" for bash/generic prompts only remembered the
			// session (the persistent half of the contract lived
			// exclusively in the bundled TUI's callbacks). With
			// this, the very next identical call short-circuits at
			// the policy layer regardless of which prompter the
			// host wired.
			grant.Pattern = req.PersistTool + ":" + req.PersistKey
			if err := g.policy.AddAllow([]string{grant.Pattern}); err != nil {
				return fmt.Errorf("permissions: install always-allow pattern %q: %w", grant.Pattern, err)
			}
		}
		if g.grants != nil {
			// The in-memory grant above already applies; a persist
			// failure must still surface, not silently downgrade
			// the operator's "always" to "this session" — they can
			// retry, fix the config dir, or re-answer with a
			// session-scoped choice.
			if err := g.grants.Persist(ctx, grant); err != nil {
				return fmt.Errorf("permissions: persist always-allow grant %q: %w", grant.Pattern, err)
			}
		}
		g.recordApproval(req.ToolName, req.Detail, d, approval.By)
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

func (g *Gate) recordApproval(toolName, key string, d Decision, by string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.approvals = append(g.approvals, ApprovalLog{
		Tool:     toolName,
		Key:      key,
		Decision: d,
		At:       time.Now(),
		By:       by,
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
