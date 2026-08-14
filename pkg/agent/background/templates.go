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

package background

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/adk/tool"

	"github.com/go-steer/core-agent/v2/pkg/agent"
	"github.com/go-steer/core-agent/v2/pkg/attach"
)

// This file bridges the two subagent worlds (#626, option B): the
// declarative subagents wired for SYNCHRONOUS invocation via
// agent.WithSubagents (own model, own content root, own MCP + skills)
// are also registered here as templates so the SAME subagent is
// spawnable ASYNCHRONOUSLY by reference — spawn_agent {agent: "cluster"}.
//
// A SubagentTemplate is the async-side view of a declarative subagent:
// its persona, model, and tool surface are already resolved by the
// builder (cmd/core-agent/subagents.go), so a template spawn skips the
// catalog-name resolution the ad-hoc/catalog Spec path does. The
// resolved toolsets are process-long-lived, stateless handles (MCP
// servers, skills), so sharing them across concurrent async instances
// of the same template is safe — each instance still gets its own
// session, branch, and freshly-built LLM.

// SubagentTemplate is a predefined declarative subagent, pre-resolved
// for async-by-reference spawning. Unlike a catalog Spec (persona +
// tool NAMES resolved against the manager's catalog at spawn time), a
// template already carries built tool instances, MCP + skills toolsets,
// and a model factory — because a declarative subagent may be rooted
// (its own content root, mcp.json, and skills/ tree) and can't be
// reconstructed from the parent's catalog.
type SubagentTemplate struct {
	// Name is the reference key (spawn_agent {agent: Name}) and seeds
	// auto-derived instance names ("cluster-1"). Must be branch-safe.
	Name string
	// Description is the operator-facing summary (backs the #627 catalog).
	Description string
	// Root is the subagent's content root (its own AGENTS.md + mcp.json +
	// skills/ tree), relative to the recipe as authored in config. Empty
	// for an inline (non-rooted) declarative subagent. Display-only —
	// surfaced in the #627 operator catalog so operators can see which
	// subagents carry their own content bundle.
	Root string
	// ModelFactory builds a fresh LLM for each spawn — session isolation,
	// same as the catalog path's provider.Model call. Required.
	ModelFactory func(context.Context) (adkmodel.LLM, error)
	// ModelID labels the template's model for /usage pricing attribution.
	ModelID string
	// Instruction is the fully-resolved persona, installed as layer 4
	// (user memory) exactly like the synchronous declarative path.
	Instruction string
	// Tools are the built-in tools (already resolved to instances) the
	// subagent runs with. Shared, stateless — safe across instances.
	Tools []tool.Tool
	// Toolsets are the MCP + skills groups. Process-long-lived, stateless
	// handles — shared across concurrent instances of the template.
	Toolsets []tool.Toolset
	// MaxDepth caps the subagent's OWN nesting (0 = substrate default).
	MaxDepth int
	// Budgets bound each async run; per-spawn overrides may only tighten.
	Budgets Budgets
	// Scheduler is the between-turn scheduler choice ("" = manager
	// default); see resolveScheduler for the accepted values.
	Scheduler string
	// Mode selects how the run terminates. Empty derives it from the
	// resolved Scheduler; see Mode.
	Mode Mode
}

// WithSubagentTemplates registers the declarative-subagent roster at
// construction time. Most callers instead use SetSubagentTemplates,
// because the templates are built (cmd/core-agent/subagents.go) after
// the manager is constructed but the spawn tools reference the manager —
// a construction-ordering cycle the post-construction setter breaks.
func WithSubagentTemplates(ts []SubagentTemplate) ManagerOption {
	return func(c *bgMgrConfig) { c.templates = ts }
}

// SetSubagentTemplates installs (or replaces) the declarative-subagent
// roster after construction. Call it once, before the parent agent
// starts running — the declarative builder produces the templates only
// after the manager (and its spawn tools) exist, so they can't be passed
// to NewManager. Names must be non-empty, branch-safe, carry a non-nil
// ModelFactory, be unique among themselves, and not collide with a
// predefined (catalog) spec name. Returns an error without mutating
// state when any check fails.
//
// Each template's Tools are rebound to THIS manager (see
// rebindSpawnTools) as they are installed, so one shared roster can be
// registered on many managers — which is exactly what a multi-session
// daemon does.
func (m *Manager) SetSubagentTemplates(ts []SubagentTemplate) error {
	mine := m.ownSpawnTools()
	next := make(map[string]SubagentTemplate, len(ts))
	for _, t := range ts {
		if err := validateTemplate(t); err != nil {
			return err
		}
		if _, dup := next[t.Name]; dup {
			return fmt.Errorf("background: duplicate subagent template name %q", t.Name)
		}
		t.Tools = rebindSpawnTools(t.Tools, mine)
		next[t.Name] = t
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for name := range next {
		if _, clash := m.predefined[name]; clash {
			return fmt.Errorf("background: subagent template name %q collides with a predefined spec", name)
		}
	}
	m.templates = next
	return nil
}

// ownSpawnTools indexes this manager's spawn tools by name, for
// rebindSpawnTools. Built once per SetSubagentTemplates call: the tools
// are stateless wrappers over the manager, so every template can share
// one instance the way the parent's tool surface does.
func (m *Manager) ownSpawnTools() map[string]tool.Tool {
	tools := NewSpawnTools(m)
	mine := make(map[string]tool.Tool, len(tools))
	for _, t := range tools {
		mine[t.Name()] = t
	}
	return mine
}

// rebindSpawnTools returns in with every tool whose name is in mine —
// the installing manager's own spawn tools — replaced by that one. in
// itself is never mutated.
//
// A declarative subagent's Tools are resolved ONCE, at daemon startup,
// against the parent's built-in registry — which by then already carries
// the daemon manager's spawn tools (cmd/core-agent/main.go appends
// NewSpawnTools before it builds the subagents). A multi-session daemon
// then registers that one shared roster on every session's OWN manager,
// so without this rebind a session's subagent holds a spawn_agent
// pointing back at the DAEMON manager: its spawns would run under the
// daemon-wide gate, branch off the daemon's parent rather than the
// session's, and push their "[Background reports]" into another tenant's
// turn. That is the same four-way breakage the per-session manager exists
// to prevent (see compose.SessionBackgroundFactory), reached one level
// down — and it is not theoretical: in the 2026-08-13 GKE UAT a session's
// bg.cluster-1 spawned bg.cluster-2 onto the daemon's "default" session.
//
// Matching is by tool NAME against this manager's own spawn tools, which
// keeps the operator's scoping intact: a template whose spec listed no
// spawn tool gains none, and one that listed only spawn_agent does not
// also acquire stop_agent. The names are unambiguous — nothing but
// NewSpawnTools registers them.
func rebindSpawnTools(in []tool.Tool, mine map[string]tool.Tool) []tool.Tool {
	if len(in) == 0 {
		return in
	}
	var out []tool.Tool
	for i, t := range in {
		if t == nil {
			continue
		}
		repl, ok := mine[t.Name()]
		if !ok {
			continue
		}
		if out == nil {
			// Copy on first write: the caller's slice is shared with every
			// other manager this roster is registered on, so rebinding in
			// place would hand the last-registered manager's tools to all
			// of them.
			out = slices.Clone(in)
		}
		out[i] = repl
	}
	if out == nil {
		return in
	}
	return out
}

// validateTemplate checks a declarative-subagent template. Name must be
// branch-safe (it seeds instance names) and a ModelFactory is required
// (there's no manager-model fallback for templates — the declarative
// builder always resolves one, inheriting the parent's when unset).
func validateTemplate(t SubagentTemplate) error {
	if err := validateSpawnName(t.Name); err != nil {
		return err
	}
	if t.ModelFactory == nil {
		return fmt.Errorf("background: subagent template %q needs a ModelFactory", t.Name)
	}
	switch t.Mode {
	case ModeAuto, ModeBounded, ModeStanding:
	default:
		return fmt.Errorf("background: subagent template %q has unknown Mode %q (want %q, %q, or empty)", t.Name, t.Mode, ModeBounded, ModeStanding)
	}
	return nil
}

// hasTemplate reports whether name refers to a registered declarative
// subagent template.
func (m *Manager) hasTemplate(name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.templates[name]
	return ok
}

// TemplateNames returns the registered declarative-subagent template
// names, sorted. Backs the operator catalog surfaces (#627).
func (m *Manager) TemplateNames() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.templates))
	for name := range m.templates {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// SpawnTemplate launches a registered declarative subagent
// asynchronously (spawn_agent {agent: name}, #626). The template's
// persona, tools, toolsets, and model are pre-resolved; the caller
// supplies the goal via ov and may narrow the model (to "small") and
// budgets. explicitName, when non-empty, names the instance; otherwise
// the runtime auto-derives "<name>-<n>".
//
// Tool narrowing is intentionally NOT supported for templates: a rooted
// subagent's grant spans built-ins, MCP toolset tools, and skills, so a
// name-subset can't be applied coherently here — configure a dedicated,
// narrower subagent instead. Model overrides other than inherit/"small"
// are rejected (D2), matching the catalog reference path.
func (m *Manager) SpawnTemplate(ctx context.Context, parentBranch, name string, ov RefOverrides, explicitName string) (*Handle, error) {
	m.mu.Lock()
	tmpl, ok := m.templates[name]
	small := m.smallModelID
	prov := m.provider
	m.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownSubagent, name)
	}
	if len(ov.Tools) > 0 {
		return nil, fmt.Errorf("background: cannot narrow tools of preconfigured subagent %q (its grant spans built-ins, MCP, and skills); configure a dedicated subagent", name)
	}
	goal := strings.TrimSpace(ov.Goal)
	if goal == "" {
		return nil, errors.New("background: goal is required when spawning a preconfigured subagent")
	}

	// Model override: inherit the template's own model, or downshift to
	// the manager's small tier. A "small" downshift resolves against the
	// manager's provider — the small tier is defined relative to the
	// manager, not the template's (possibly different) provider.
	buildModel := tmpl.ModelFactory
	priceModelID := tmpl.ModelID
	switch strings.TrimSpace(ov.Model) {
	case "", "inherit":
		// Keep the template's configured model.
	case "small":
		if small == "" {
			return nil, ErrNoSmallModel
		}
		buildModel = func(c context.Context) (adkmodel.LLM, error) { return prov.Model(c, small) }
		priceModelID = small
	default:
		return nil, fmt.Errorf("%w: got %q (configure a dedicated subagent spec for a specific model)", ErrModelNotOverridable, ov.Model)
	}

	sched, err := m.resolveScheduler(tmpl.Scheduler)
	if err != nil {
		return nil, err
	}

	var instrOpts []agent.Option
	if strings.TrimSpace(tmpl.Instruction) != "" {
		instrOpts = []agent.Option{agent.WithUserInstruction(tmpl.Instruction)}
	}

	return m.launch(ctx, parentBranch, resolvedSpawn{
		name:         m.nextInstanceName(name, explicitName),
		goal:         goal,
		instrOpts:    instrOpts,
		tools:        tmpl.Tools,
		toolsets:     tmpl.Toolsets,
		buildModel:   buildModel,
		priceModelID: priceModelID,
		maxDepth:     tmpl.MaxDepth,
		budgets:      tightenBudgets(mergeBudgets(m.defaultBudgets, tmpl.Budgets), ov.Budgets),
		scheduler:    sched,
		mode:         tmpl.Mode,
	})
}

// SpawnRef spawns a configured subagent by name — routing a declarative
// template to SpawnTemplate and a catalog predefined spec to Spawn — with
// the given goal and narrowing-only overrides. It is the operator-facing
// twin of the model's spawn_agent reference path (the /subagent TUI command
// uses it to reach the same roster the model can), and never opens the
// ad-hoc inline-persona path. Unknown names return ErrUnknownSubagent.
func (m *Manager) SpawnRef(ctx context.Context, parentBranch, name, goal string, ov RefOverrides, explicitName string) (*Handle, error) {
	if m.hasTemplate(name) {
		ov.Goal = goal
		return m.SpawnTemplate(ctx, parentBranch, name, ov, explicitName)
	}
	spec, err := m.resolvePredefinedSpec(name, RefOverrides{
		Goal:    goal,
		Model:   ov.Model,
		Tools:   ov.Tools,
		Budgets: ov.Budgets,
	})
	if err != nil {
		return nil, err
	}
	spec.Name = m.nextInstanceName(name, explicitName)
	return m.Spawn(ctx, parentBranch, spec)
}

// ReferenceNames returns every subagent name spawnable by reference —
// declarative templates plus catalog predefined specs — sorted. Used by
// operator surfaces (the /subagent command) to list what can be spawned.
func (m *Manager) ReferenceNames() []string {
	names := append(m.TemplateNames(), m.PredefinedNames()...)
	sort.Strings(names)
	return names
}

// syncSubagentNames indexes the subagents the attached parent exposes as
// a synchronous, parent-callable tool (agent.WithSubagents). Empty when
// no parent is attached yet, and — the case this exists for — when the
// parent was handed the manager but not the sync tools:
// compose.ReproduceAgent wires each multi-session session with
// WithBackgroundManager and no WithSubagents, so a session can reach a
// declarative subagent by reference only. Reporting those as "sync" told
// operators about a tool the session's model was never offered; in the
// 2026-08-13 GKE UAT /subagents claimed sync+async on a session whose
// /tools listing had no such tool (#741).
func (m *Manager) syncSubagentNames() map[string]struct{} {
	names := m.Parent().SubagentNames() // nil-safe on both hops
	if len(names) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(names))
	for _, n := range names {
		set[n] = struct{}{}
	}
	return set
}

// Catalog returns the configured-subagent roster (#627) — declarative
// templates first, then predefined catalog specs (async-only), each
// sorted by name. This is what the daemon LOADED, as
// opposed to Manager.List / ListSubagents (live spawned instances). Backs
// the operator-facing surfaces: GET .../subagents (via the SubagentManager
// interface's ListSubagentCatalog), the /subagent listing, and the boot
// dump. Never nil.
//
// Modes records how each subagent can actually be invoked HERE, on this
// manager's parent. Every declarative template is "async" (spawn_agent
// {agent}); it is additionally "sync" only when the attached parent
// exposes it as a tool, which is a property of that parent's
// WithSubagents set, not of the template. Predefined catalog specs are
// always "async" only (spawn-by-reference, no synchronous tool).
func (m *Manager) Catalog() []attach.SubagentCatalogInfo {
	// Read before taking m.mu: Parent() locks, and SubagentNames() is a
	// call out of this package, which we'd rather not make under our own
	// lock.
	sync := m.syncSubagentNames()

	m.mu.Lock()
	defer m.mu.Unlock()

	templates := make([]attach.SubagentCatalogInfo, 0, len(m.templates))
	for _, t := range m.templates {
		modes := []string{"async"}
		if _, ok := sync[t.Name]; ok {
			modes = []string{"sync", "async"}
		}
		templates = append(templates, attach.SubagentCatalogInfo{
			Name:        t.Name,
			Description: t.Description,
			Model:       t.ModelID,
			Root:        t.Root,
			Modes:       modes,
		})
	}
	sort.Slice(templates, func(i, j int) bool { return templates[i].Name < templates[j].Name })

	predefined := make([]attach.SubagentCatalogInfo, 0, len(m.predefined))
	for _, s := range m.predefined {
		predefined = append(predefined, attach.SubagentCatalogInfo{
			Name:        s.Name,
			Description: s.Description,
			Model:       s.ModelID,
			Modes:       []string{"async"},
		})
	}
	sort.Slice(predefined, func(i, j int) bool { return predefined[i].Name < predefined[j].Name })

	return append(templates, predefined...)
}

// ListSubagentCatalog implements the agent.SubagentManager seam's
// operator-catalog method (#627): the configured roster in attach types,
// so the adapter can serve GET .../subagents without importing this
// package. Identical data to Catalog (which callers holding the concrete
// *Manager may use directly).
func (m *Manager) ListSubagentCatalog() []attach.SubagentCatalogInfo {
	return m.Catalog()
}
