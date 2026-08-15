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
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	adkmodel "google.golang.org/adk/model"
	adktool "google.golang.org/adk/tool"

	"github.com/go-steer/core-agent/v2/pkg/agent"
	"github.com/go-steer/core-agent/v2/pkg/agent/background"
	"github.com/go-steer/core-agent/v2/pkg/compose"
	"github.com/go-steer/core-agent/v2/pkg/config"
	"github.com/go-steer/core-agent/v2/pkg/instruction"
	"github.com/go-steer/core-agent/v2/pkg/mcp"
	"github.com/go-steer/core-agent/v2/pkg/models"
	"github.com/go-steer/core-agent/v2/pkg/permissions"
	"github.com/go-steer/core-agent/v2/pkg/skills"
)

// defaultSyncWaitTimeout caps how long a synchronous spawn (spawn_agent
// {wait: true}, #626/D5) holds the parent turn open before the tool returns
// a partial/timeout result. Tighter than the async fire-and-continue
// wall-clock budget: a slow subagent shouldn't hang the parent turn, and its
// result is still delivered later by push. Chosen against typical subagent
// latencies; revisit if real workloads need a different bound.
const defaultSyncWaitTimeout = 5 * time.Minute

// sessionBackgroundRecipe captures everything the daemon knows about
// background subagents at startup, so each multi-session session can
// stand up its OWN background.Manager from it (#637). The zero value is
// inert: factory() returns nil, and compose then wires no manager at
// all — which is exactly what --no-background-agents should produce.
//
// It lives here rather than in pkg/compose so compose keeps no
// dependency on pkg/agent/background: the daemon already owns the
// provider, the small-model id, the ad-hoc policy and the resolved
// declarative templates, and hands compose one closure.
type sessionBackgroundRecipe struct {
	provider     models.Provider
	smallModelID string
	allowAdhoc   bool
	syncWait     time.Duration
	// spawnToolNames are the DAEMON-bound spawn tools baked into the
	// shared builtin list. They must be stripped from every session's
	// surface and replaced with session-bound ones, or the session's
	// spawns would run on the daemon manager (daemon-wide gate, wrong
	// parent, cross-tenant alert channel).
	spawnToolNames map[string]struct{}
	// templates are the declarative subagents, pre-resolved once at
	// startup. Safe to share across managers: each spawn builds a fresh
	// LLM from the template's ModelFactory, and toolsets are stateless
	// process-lifetime handles.
	templates []background.SubagentTemplate
	// live tracks the managers currently handed out, so daemon shutdown
	// can drain them alongside the daemon's own.
	live *sessionManagerSet
}

// sessionManagerSet tracks the live per-session managers so daemon
// shutdown drains them the way it drains the daemon's own: cancel every
// subagent, wait out the bounded window, log the stragglers. Without it
// a SIGTERM would tear in-flight session subagents down mid-tool-call —
// their goroutines run under context.WithoutCancel, so only a Close
// reaches them, and a session's Close is otherwise wired to eviction
// alone. Entries are removed on eviction so a long-lived daemon's set
// tracks live sessions, not every session it ever created.
type sessionManagerSet struct {
	mu   sync.Mutex
	live map[*background.Manager]struct{}
}

func newSessionManagerSet() *sessionManagerSet {
	return &sessionManagerSet{live: make(map[*background.Manager]struct{})}
}

func (s *sessionManagerSet) add(m *background.Manager) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.live[m] = struct{}{}
}

// len reports how many managers are currently tracked.
func (s *sessionManagerSet) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.live)
}

func (s *sessionManagerSet) remove(m *background.Manager) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.live, m)
}

// closeAll drains every live manager concurrently, so total shutdown
// latency stays one Close window rather than one per session. Close is
// idempotent, so racing with an eviction that is closing the same
// manager is safe.
func (s *sessionManagerSet) closeAll() {
	s.mu.Lock()
	mgrs := make([]*background.Manager, 0, len(s.live))
	for m := range s.live {
		mgrs = append(mgrs, m)
	}
	clear(s.live)
	s.mu.Unlock()

	var wg sync.WaitGroup
	for _, m := range mgrs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := m.Close(); err != nil {
				fmt.Fprintf(os.Stderr, "core-agent: shutdown: session subagents: %v\n", err)
			}
		}()
	}
	wg.Wait()
}

// factory adapts the recipe into the compose seam. Returns nil when the
// daemon has no provider for background work, which leaves the
// pre-#637 behavior in place.
func (r sessionBackgroundRecipe) factory() compose.SessionBackgroundFactory {
	if r.provider == nil {
		return nil
	}
	return func(scope compose.SessionScope) (compose.SessionSubagents, error) {
		// The catalog a subagent may draw from is the session's own tool
		// surface MINUS the spawn tools, matching the daemon manager's
		// posture (its catalog was snapshotted before its spawn tools
		// were appended) — a subagent must not spawn subagents by
		// picking the spawn tool out of the catalog.
		base := make([]adktool.Tool, 0, len(scope.Tools))
		for _, t := range scope.Tools {
			if t == nil {
				continue
			}
			if _, isSpawn := r.spawnToolNames[t.Name()]; isSpawn {
				continue
			}
			base = append(base, t)
		}
		mgr, err := background.NewManager(
			background.WithProvider(r.provider, scope.ModelName),
			// THIS session's sub-gate, so its subagents inherit its mode,
			// approvals and plan-first state instead of the daemon's.
			background.WithGate(scope.Gate),
			background.WithCatalog(base),
			background.WithAllowAdhoc(r.allowAdhoc),
			background.WithSmallModelID(r.smallModelID),
			background.WithSyncWaitTimeout(r.syncWait),
		)
		if err != nil {
			return compose.SessionSubagents{}, err
		}
		if len(r.templates) > 0 {
			if err := mgr.SetSubagentTemplates(r.templates); err != nil {
				_ = mgr.Close()
				return compose.SessionSubagents{}, fmt.Errorf("register async templates: %w", err)
			}
		}
		sid := scope.SessionID
		if r.live != nil {
			r.live.add(mgr)
		}
		return compose.SessionSubagents{
			Manager: mgr,
			Tools:   append(base, background.NewSpawnTools(mgr)...),
			Close: func() {
				if r.live != nil {
					r.live.remove(mgr)
				}
				if err := mgr.Close(); err != nil {
					fmt.Fprintf(os.Stderr, "core-agent: session %s subagents: %v\n", sid, err)
				}
			},
		}, nil
	}
}

// namedToolset pairs an MCP server's name with its already-started
// toolset, so a scoped subagent can select servers by name. Built from
// the parent's []*mcp.Server at the call site; a server that failed to
// start carries a nil toolset (still listed so name selection can tell
// "exists but down" from "unknown server").
type namedToolset struct {
	name    string
	toolset adktool.Toolset
}

// parentSurface is the parent agent's fully-resolved tool surface, handed
// to buildDeclaredSubagents so each subagent can inherit it whole or take
// a name-scoped subset. It is the single source both the parent and its
// declarative subagents draw from — one config.json + one mcp.json + one
// skills/ tree (docs/declarative-subagents-design.md, resolved OQ1).
type parentSurface struct {
	// builtinTools is the parent's built-in registry (read_file, bash,
	// spawn tools, agentic wrappers, …) after all disable/append passes.
	builtinTools []adktool.Tool
	// mcpToolsets is the parent's already-started MCP servers as (name,
	// toolset) pairs; a scoped subagent selects among them by name and
	// reuses each server's existing toolset (no second mcp.Build, no
	// per-subagent lifecycle).
	mcpToolsets []namedToolset
	// skills is the parent's loaded skill bundle; a scoped subagent gets
	// a name-filtered view via skills.Scoped (no filesystem re-walk).
	skills skills.Skills
}

// subagentDeps bundles the process-scoped collaborators buildDeclaredSubagents
// needs beyond the parent surface — chiefly to stand up a rooted subagent's
// OWN mcp.json + skills/ + persona from a dedicated content root. They are the
// same instances the parent used (one permission gate, one elicitor, one digest
// config), so a rooted subagent's servers and skills are gated and digested
// identically; it gains its own scope, never its own privileges.
type subagentDeps struct {
	// gate is the shared permission gate every subagent's tools run behind.
	gate *permissions.Gate
	// elicitor + digestOpts are handed to mcp.Build for a rooted subagent's
	// own servers, matching the parent's MCP behavior exactly.
	elicitor   mcp.ElicitorFn
	digestOpts *mcp.DigestOptions
	// interp substitutes ${env:VAR} in loaded instruction + skill bodies.
	interp func(string) string
	// send emits a startup line per subagent (and per rooted-MCP hiccup).
	send func(string)
	// rootBase resolves a relative spec.Root, mirroring content_roots: the
	// agents dir when the config was discovered under one, else the cwd.
	rootBase string
	// noPromptCache is the parent's --no-prompt-cache kill switch. A
	// subagent with its own model resolves its own provider, which never
	// sees the parent's CLI flags, so it has to be carried here for the
	// switch to mean "off everywhere" rather than "off for the parent".
	noPromptCache bool
}

// buildDeclaredSubagents turns the config's declarative subagents[] block
// into fully-constructed *agent.Agent values, ready to hand to the parent
// via agent.WithSubagents. It is the one piece of glue that makes
// docs/declarative-subagents-design.md real; pkg/agent's subagent
// substrate is reused essentially unchanged.
//
// Each subagent gets:
//   - its own name / description (shown to the parent's model),
//   - its own instruction — inline or an @include chain expanded through
//     pkg/instruction, or (with a root) auto-assembled from the root's own
//     AGENTS.md,
//   - its own model (its ModelConfig, or the parent's when unset),
//   - a recursion depth cap (spec.MaxDepth, honored via
//     WithSubagentMaxDepth; 0 = substrate default), and
//   - a tool surface drawn from one of two sources. Inline (spec.Root
//     unset) resolves MCP + skills by name against the SHARED parent
//     surface — each dimension inherited whole when the field is nil,
//     name-scoped when a non-empty list, or none when an explicit empty
//     list. Rooted (spec.Root set) loads the subagent's OWN mcp.json and
//     skills/ from a dedicated content root, then applies that same
//     nil/list/empty contract WITHIN the root. Built-in tools (spec.Tools)
//     always resolve against the parent registry — built-ins live in the
//     binary, not a directory — minus the spawn tools, which inheritance
//     withholds and only an explicit tools: list grants (#748). Every tool
//     instance carries the shared permission gate, so a subagent cannot
//     escalate.
//
// Returns the subagents, the async-spawn templates (one per subagent, so
// the same subagent the parent can call synchronously is also spawnable
// by reference via spawn_agent {agent: "<name>"} — #626, wire with
// background.Manager.SetSubagentTemplates), and the MCP servers stood up
// for rooted subagents (empty for the all-inline case). The caller owns
// the servers' lifecycle: close them on shutdown and fold them into the
// one RegisterMetrics call. On error the already-started servers are
// still returned so the caller can close them.
//
// Returns (nil, nil, nil, nil) when no subagents are declared — the
// caller then skips agent.WithSubagents entirely.
func buildDeclaredSubagents(
	ctx context.Context,
	cfg *config.Config,
	parentProvider models.Provider,
	projectRoot string,
	surface parentSurface,
	deps subagentDeps,
) ([]*agent.Agent, []background.SubagentTemplate, []*mcp.Server, error) {
	if len(cfg.Subagents) == 0 {
		return nil, nil, nil, nil
	}
	subs := make([]*agent.Agent, 0, len(cfg.Subagents))
	templates := make([]background.SubagentTemplate, 0, len(cfg.Subagents))
	var rootedServers []*mcp.Server
	for i, spec := range cfg.Subagents {
		// Resolve the provider + model name once; the sync path builds one
		// LLM from it, the async template a fresh-LLM-per-spawn factory.
		subProvider, modelName, err := resolveSubagentProvider(cfg, parentProvider, spec, deps.noPromptCache, deps.send)
		if err != nil {
			return nil, nil, rootedServers, fmt.Errorf("subagents[%d] %q: model: %w", i, spec.Name, err)
		}
		llm, err := subProvider.Model(ctx, modelName)
		if err != nil {
			return nil, nil, rootedServers, fmt.Errorf("subagents[%d] %q: model: %w", i, spec.Name, err)
		}

		// Built-ins always resolve against the parent registry — they ship
		// in the binary, not in any content root. droppedTools is the
		// spawn-tool carve-out (#748), reported on the boot line below.
		subTools, droppedTools, err := resolveSubagentTools(spec, surface.builtinTools)
		if err != nil {
			return nil, nil, rootedServers, fmt.Errorf("subagents[%d] %q: tools: %w", i, spec.Name, err)
		}

		var (
			subToolsets     []adktool.Toolset
			userInstruction string
			scopeDesc       string
		)
		if spec.Root != "" {
			rootAbs, rootSurface, servers, err := loadSubagentRoot(ctx, spec, deps)
			// servers may be non-nil even on a later error (skills load fails
			// after mcp.Build succeeded) — collect them for shutdown either way.
			rootedServers = append(rootedServers, servers...)
			if err != nil {
				return nil, nil, rootedServers, fmt.Errorf("subagents[%d] %q: %w", i, spec.Name, err)
			}
			subToolsets, scopeDesc, err = resolveSubagentToolsets(ctx, spec, rootSurface)
			if err != nil {
				return nil, nil, rootedServers, fmt.Errorf("subagents[%d] %q: %w", i, spec.Name, err)
			}
			userInstruction, err = rootedSubagentInstruction(spec, rootAbs, deps.interp)
			if err != nil {
				return nil, nil, rootedServers, fmt.Errorf("subagents[%d] %q: instructions: %w", i, spec.Name, err)
			}
			scopeDesc = fmt.Sprintf("root=%s (%s), %s", rootAbs, rootInventory(rootSurface), scopeDesc)
		} else {
			subToolsets, scopeDesc, err = resolveSubagentToolsets(ctx, spec, surface)
			if err != nil {
				return nil, nil, rootedServers, fmt.Errorf("subagents[%d] %q: %w", i, spec.Name, err)
			}
			if spec.Instructions != "" {
				// Inline refs share the project scope: @include resolves
				// against projectRoot exactly like the parent's memory.
				userInstruction, _, err = instruction.Expand(spec.Instructions, projectRoot, projectRoot, instruction.WithInterpolator(deps.interp))
				if err != nil {
					return nil, nil, rootedServers, fmt.Errorf("subagents[%d] %q: instructions: %w", i, spec.Name, err)
				}
			}
		}

		sub, err := assembleSubagent(spec, llm, userInstruction, subTools, subToolsets)
		if err != nil {
			return nil, nil, rootedServers, fmt.Errorf("subagents[%d] %q: %w", i, spec.Name, err)
		}
		subs = append(subs, sub)

		// The async-spawn twin (#626): same persona, model, and tool
		// surface, resolved once here. ModelFactory rebuilds a fresh LLM
		// per spawn (provider.Model caches transport, so cheap) so
		// concurrent instances don't share one client; the toolsets ARE
		// shared (stateless, process-long-lived handles). Capture the
		// provider/name per iteration — the closure must not close over
		// the loop variables.
		p, mn := subProvider, modelName
		templates = append(templates, background.SubagentTemplate{
			Name:         spec.Name,
			Description:  spec.Description,
			ModelFactory: func(c context.Context) (adkmodel.LLM, error) { return p.Model(c, mn) },
			ModelID:      mn,
			Instruction:  userInstruction,
			Tools:        subTools,
			Toolsets:     subToolsets,
			MaxDepth:     spec.MaxDepth,
			Root:         spec.Root,
		})

		if len(droppedTools) > 0 {
			// Say out loud what inheritance withheld, so an operator who
			// wants a delegating subagent learns the remedy at boot rather
			// than from a refused spawn_agent call mid-run.
			scopeDesc += fmt.Sprintf(", spawn=withheld (%s; list them in tools: to grant)", strings.Join(droppedTools, "+"))
		}
		deps.send(fmt.Sprintf("subagent %q: model=%s, %s", spec.Name, llm.Name(), scopeDesc))
	}
	return subs, templates, rootedServers, nil
}

// assembleSubagent builds the *agent.Agent from a resolved model, persona,
// and tool surface — the common tail shared by the inline and rooted paths.
func assembleSubagent(spec config.SubagentSpec, llm adkmodel.LLM, userInstruction string, subTools []adktool.Tool, subToolsets []adktool.Toolset) (*agent.Agent, error) {
	opts := []agent.Option{
		agent.WithName(spec.Name),
		agent.WithDescription(spec.Description),
		// A declarative subagent is invoked as a tool with no human
		// reading its output in real time — run it headless, like
		// the other in-tree spawn paths.
		agent.WithMode(agent.ModeAutonomous),
		agent.WithTools(subTools),
		agent.WithToolsets(subToolsets),
		// #727: the same return contract the async spawn path installs.
		// A subagent reached as a tool has no return tool at all — its
		// last message IS the value — so the contract is rendered
		// without one rather than naming a gesture that doesn't exist
		// here. Same roster, same instruction, either way it's reached.
		agent.WithExtraInstruction(agent.SubagentReturnContract("")),
	}
	if userInstruction != "" {
		// Layer 4 (user memory), same slot the parent's AGENTS.md lands in
		// — the harness contract (layers 1–3) stays intact beneath the
		// subagent's persona.
		opts = append(opts, agent.WithUserInstruction(userInstruction))
	}
	if spec.MaxDepth > 0 {
		opts = append(opts, agent.WithSubagentMaxDepth(spec.MaxDepth))
	}
	return agent.New(llm, opts...)
}

// loadSubagentRoot stands up a rooted subagent's OWN scope from a dedicated
// content root: its mcp.json servers and skills/ tree, returned as a
// parentSurface the shared resolveSubagentToolsets can scope exactly as it
// scopes the inline path — the only difference is the surface is the root's,
// not the parent's. A relative spec.Root resolves against deps.rootBase
// (mirroring content_roots); an absolute path passes through. A missing or
// non-directory root is a loud error: the operator declared it, so a typo
// must surface rather than silently yield an empty scope.
//
// The MCP servers are returned (even when a later step errors) so the caller
// can terminate their stdio children on shutdown. mcp.Build errors are
// non-fatal, matching the parent: a down server surfaces as StatusError with
// a nil toolset and is skipped by name selection.
func loadSubagentRoot(ctx context.Context, spec config.SubagentSpec, deps subagentDeps) (string, parentSurface, []*mcp.Server, error) {
	rootAbs := spec.Root
	if !filepath.IsAbs(rootAbs) {
		rootAbs = filepath.Join(deps.rootBase, rootAbs)
	}
	rootAbs = filepath.Clean(rootAbs)
	info, err := os.Stat(rootAbs)
	if err != nil {
		return "", parentSurface{}, nil, fmt.Errorf("root: %w", err)
	}
	if !info.IsDir() {
		return "", parentSurface{}, nil, fmt.Errorf("root %q is not a directory", rootAbs)
	}

	// Own MCP servers from <root>/mcp.json (no home-agents overlay — the
	// root is self-contained, which is the point of an independent scope).
	servers, _, mcpErr := mcp.Build(ctx, rootAbs, "", deps.send, deps.gate, deps.elicitor, deps.digestOpts)
	if mcpErr != nil {
		deps.send(fmt.Sprintf("subagent %q: mcp: %v", spec.Name, mcpErr))
	}
	named := make([]namedToolset, 0, len(servers))
	for _, s := range servers {
		if s == nil {
			continue
		}
		named = append(named, namedToolset{name: s.Name, toolset: s.Toolset()})
	}

	// Own skills from <root>/skills/ (again no home/user overlay).
	rootSkills, err := skills.LoadAll(ctx, rootAbs, "", deps.gate, skills.WithInterpolator(deps.interp))
	if err != nil {
		return "", parentSurface{}, servers, fmt.Errorf("skills: %w", err)
	}

	// builtinTools intentionally nil: resolveSubagentTools already resolved
	// built-ins against the parent registry; resolveSubagentToolsets reads
	// only mcpToolsets + skills from the surface.
	return rootAbs, parentSurface{mcpToolsets: named, skills: rootSkills}, servers, nil
}

// rootInventory reports what a content root actually yielded — server and
// skill counts — for the rooted subagent's boot line.
//
// This is not redundant with the mcp=/skills= fields printed beside it: those
// describe the *scoping policy* (inherit the root's surface whole, or take a
// named subset) and read identically whether the root supplied six skills or
// none. Without a count, a misnamed `skill/` directory or an absent mcp.json
// boots clean and silent, and the subagent runs persona-only — the failure
// only shows up later as a subagent that mysteriously can't do its job. The
// parent gets the same courtesy from compose's "skills: N loaded" line.
func rootInventory(surface parentSurface) string {
	down := 0
	for _, s := range surface.mcpToolsets {
		if s.toolset == nil {
			down++
		}
	}
	mcpDesc := fmt.Sprintf("mcp: %d server(s)", len(surface.mcpToolsets))
	if down > 0 {
		mcpDesc += fmt.Sprintf(", %d down", down)
	}
	return fmt.Sprintf("%s, skills: %d loaded", mcpDesc, len(surface.skills.Infos))
}

// rootedSubagentInstruction resolves a rooted subagent's persona. An inline
// spec.Instructions overrides the root's memory files (with @include confined
// to the root); otherwise the persona auto-assembles from the root's own
// AGENTS.md + AGENTS.d/, loaded as a self-contained content root so an
// @include cannot escape it.
func rootedSubagentInstruction(spec config.SubagentSpec, rootAbs string, interp func(string) string) (string, error) {
	if spec.Instructions != "" {
		expanded, _, err := instruction.Expand(spec.Instructions, rootAbs, rootAbs, instruction.WithInterpolator(interp))
		return expanded, err
	}
	loaded, err := instruction.Load("", "", instruction.WithContentRoots([]string{rootAbs}), instruction.WithInterpolator(interp))
	if err != nil {
		return "", err
	}
	return loaded.Instruction, nil
}

// spawnToolCarveOut is what an inheriting subagent does NOT get: the
// delegation surface. See resolveSubagentTools.
var spawnToolCarveOut = func() map[string]struct{} {
	m := make(map[string]struct{}, 2)
	for _, n := range background.SpawnToolNames() {
		m[n] = struct{}{}
	}
	return m
}()

// resolveSubagentTools returns the built-in tool subset a subagent runs
// with. Per the nil-vs-empty contract (pinned by config's
// TestSubagents_EmptyVsOmittedRefs): a nil spec.Tools inherits the
// parent's registry; a non-nil list selects those tools by name; an
// explicit empty list grants none. An unknown name is a config error
// (fail loud rather than silently dropping a tool the operator asked for).
//
// Inheritance has one carve-out: the spawn tools (#748). Omitting
// `tools:` says "give me the parent's hardening", not "give me the
// parent's authority to build a fleet" — and the sibling ad-hoc path has
// withheld them from its catalog since it was written (see factory()
// above, which strips r.spawnToolNames for the same reason). Delegation
// stays available to a spec that asks for it by name, which is the
// deliberate orchestrator-subagent case; the returned dropped names let
// the caller say so in the startup summary, since a carve-out invisible
// at boot is one an operator rediscovers from a live run.
func resolveSubagentTools(spec config.SubagentSpec, parent []adktool.Tool) (tools []adktool.Tool, dropped []string, err error) {
	if spec.Tools == nil {
		out := make([]adktool.Tool, 0, len(parent))
		for _, t := range parent {
			if _, isSpawn := spawnToolCarveOut[t.Name()]; isSpawn {
				dropped = append(dropped, t.Name())
				continue
			}
			out = append(out, t)
		}
		return out, dropped, nil
	}
	byName := make(map[string]adktool.Tool, len(parent))
	for _, t := range parent {
		byName[t.Name()] = t
	}
	out := make([]adktool.Tool, 0, len(spec.Tools))
	for _, name := range spec.Tools {
		t, ok := byName[name]
		if !ok {
			return nil, nil, fmt.Errorf("unknown tool %q (not among the %d built-in tools)", name, len(parent))
		}
		out = append(out, t)
	}
	return out, nil, nil
}

// resolveSubagentToolsets assembles a subagent's MCP + skills toolsets
// from the shared parent surface, honoring the same nil=inherit /
// list=scope / empty=none contract per dimension. It also returns a short
// human-readable scope description for the startup log.
func resolveSubagentToolsets(ctx context.Context, spec config.SubagentSpec, surface parentSurface) ([]adktool.Toolset, string, error) {
	var out []adktool.Toolset
	var desc []string

	// MCP: reuse each already-started server's toolset — never a second
	// mcp.Build. A named server that exists but failed to start has a nil
	// toolset; skip it silently (the parent skips it too, and its
	// StatusError is already surfaced in the startup summary).
	switch spec.MCP {
	case nil:
		for _, s := range surface.mcpToolsets {
			if s.toolset != nil {
				out = append(out, s.toolset)
			}
		}
		desc = append(desc, "mcp=inherit")
	default:
		byName := make(map[string]namedToolset, len(surface.mcpToolsets))
		for _, s := range surface.mcpToolsets {
			byName[s.name] = s
		}
		for _, name := range spec.MCP {
			s, ok := byName[name]
			if !ok {
				return nil, "", fmt.Errorf("mcp: unknown server %q (not in mcp.json)", name)
			}
			if s.toolset != nil {
				out = append(out, s.toolset)
			}
		}
		desc = append(desc, fmt.Sprintf("mcp=[%s]", strings.Join(spec.MCP, " ")))
	}

	// Skills: inherit the full toolset, or a name-scoped view.
	switch spec.Skills {
	case nil:
		if !surface.skills.Empty() {
			out = append(out, surface.skills.Toolset)
		}
		desc = append(desc, "skills=inherit")
	default:
		scoped, err := surface.skills.Scoped(ctx, spec.Skills)
		if err != nil {
			return nil, "", err
		}
		if !scoped.Empty() {
			out = append(out, scoped.Toolset)
		}
		desc = append(desc, fmt.Sprintf("skills=[%s]", strings.Join(spec.Skills, " ")))
	}

	if spec.Tools == nil {
		desc = append(desc, "tools=inherit")
	} else {
		desc = append(desc, fmt.Sprintf("tools=[%s]", strings.Join(spec.Tools, " ")))
	}

	return out, strings.Join(desc, ", "), nil
}

// resolveSubagentProvider returns the provider + model name a subagent
// runs on: its own ModelConfig when spec.Model is set, otherwise the
// parent's provider + model. When the subagent declares its own model, a
// provider is resolved for it through the same models.Resolve path the
// parent used — so provider-specific config (Vertex project/location,
// Anthropic-Vertex, scripted transcript) and env auto-detection behave
// identically.
//
// Returning the (provider, name) pair rather than a built LLM lets both
// the synchronous path (build one LLM for the subagent-tool) and the
// async template path (a factory that builds a fresh LLM per spawn, #626)
// draw from the same resolution — provider.Model caches auth + transport
// internally, so per-spawn calls are cheap.
func resolveSubagentProvider(cfg *config.Config, parentProvider models.Provider, spec config.SubagentSpec, noPromptCache bool, send func(string)) (models.Provider, string, error) {
	if spec.Model == nil {
		// The parent provider already went through MaybeWirePromptCache.
		return parentProvider, cfg.Model.Name, nil
	}
	// Own model: shallow-copy cfg with the subagent's Model so
	// models.Resolve reads the right provider + provider-specific
	// sub-blocks. Only Model is overwritten; the copy shares the rest of
	// cfg by value, which Resolve does not mutate.
	subCfg := *cfg
	subCfg.Model = *spec.Model
	// Overwriting Model also drops the parent's prompt_cache setting,
	// since that block hangs off model.anthropic. An operator who turned
	// caching off project-wide means it for the whole process, so
	// inherit it — unless the subagent's own model block says otherwise,
	// which is a deliberate per-subagent override.
	subCfg.Model.Anthropic = inheritPromptCache(cfg.Model.Anthropic, spec.Model.Anthropic)
	p, err := models.Resolve(&subCfg)
	if err != nil {
		return nil, "", fmt.Errorf("resolve provider: %w", err)
	}
	// Announce only a deviation: the daemon already printed its own
	// prompt-cache line, and repeating "enabled" once per declared
	// subagent is noise on every default run.
	if status, enabled := compose.MaybeWirePromptCache(p, noPromptCache); status != "" && !enabled && send != nil {
		send(fmt.Sprintf("subagent %q: %s", spec.Name, status))
	}
	return p, spec.Model.Name, nil
}

// inheritPromptCache returns the subagent's Anthropic block with the
// parent's prompt_cache filled in where the subagent didn't set one.
// Returns sub unchanged when there is nothing to inherit, and never
// mutates either input — subCfg is a shallow copy, so writing through
// the parent's pointer would corrupt the real config.
func inheritPromptCache(parent, sub *config.AnthropicConfig) *config.AnthropicConfig {
	if parent == nil || parent.PromptCache == nil {
		return sub
	}
	if sub == nil {
		return &config.AnthropicConfig{PromptCache: parent.PromptCache}
	}
	if sub.PromptCache != nil {
		return sub
	}
	merged := *sub
	merged.PromptCache = parent.PromptCache
	return &merged
}
