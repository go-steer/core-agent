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
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	adkagent "google.golang.org/adk/agent"
	adktool "google.golang.org/adk/tool"

	"github.com/go-steer/core-agent/v2/pkg/agent"
	"github.com/go-steer/core-agent/v2/pkg/agent/background"
	"github.com/go-steer/core-agent/v2/pkg/attach"
	"github.com/go-steer/core-agent/v2/pkg/config"
	"github.com/go-steer/core-agent/v2/pkg/eventlog"
	"github.com/go-steer/core-agent/v2/pkg/mcp"
	"github.com/go-steer/core-agent/v2/pkg/models/mock"
	"github.com/go-steer/core-agent/v2/pkg/skills"
)

// fakeTool is a name-only adktool.Tool for exercising the built-in tool
// name filter without constructing real tools.
type fakeTool struct{ name string }

func (f fakeTool) Name() string        { return f.name }
func (f fakeTool) Description() string { return f.name + " (fake)" }
func (f fakeTool) IsLongRunning() bool { return false }

// fakeToolset is a name-only adktool.Toolset standing in for an MCP
// server's toolset; identity is compared by Name in the assertions.
type fakeToolset struct{ name string }

func (f fakeToolset) Name() string { return f.name }
func (f fakeToolset) Tools(adkagent.ReadonlyContext) ([]adktool.Tool, error) {
	return nil, nil
}

func toolsetNames(tss []adktool.Toolset) []string {
	out := make([]string, 0, len(tss))
	for _, ts := range tss {
		out = append(out, ts.Name())
	}
	return out
}

// identityInterp is the no-op interpolator: subagent tests don't exercise
// ${env:...} substitution (covered by pkg/instruction), only the plumbing.
func identityInterp(s string) string { return s }

// discard swallows the human-facing progress lines buildDeclaredSubagents
// emits — tests assert on returned agents, not on narration.
func discard(string) {}

// testDeps is the minimal subagentDeps for inline-path tests: identity
// interpolator, discarded narration, no gate/elicitor/root (the rooted path
// supplies those in its own tests).
func testDeps() subagentDeps {
	return subagentDeps{interp: identityInterp, send: discard}
}

// writeScript drops a one-turn JSONL transcript so the scripted provider
// resolves without a real recording. resolveSubagentModel only needs the
// provider to build; it never plays the turn back.
func writeScript(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "script.jsonl")
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write script: %v", err)
	}
	return path
}

// TestBuildDeclaredSubagents_OwnModelAndIdentity is the γ.2 acceptance
// check: a declared subagent is built with its own name / description /
// instructions and, when it declares a Model, runs on that model — distinct
// from the parent's. Parent is echo; the "cluster" subagent is scripted, so
// the two are trivially distinguishable by LLM identity.
func TestBuildDeclaredSubagents_OwnModelAndIdentity(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{
		Model: config.ModelConfig{Provider: mock.ProviderEcho, Name: "echo"},
		// Inherited by the scripted subagent's cfg copy in
		// resolveSubagentModel (newScripted reads Mock.Script).
		Mock: config.MockConfig{Script: writeScript(t)},
		Subagents: []config.SubagentSpec{{
			Name:         "cluster",
			Description:  "read-only cluster investigator",
			Instructions: "You are a read-only cluster investigator.\n",
			Model:        &config.ModelConfig{Provider: mock.ProviderScripted, Name: "scripted-x"},
		}},
	}

	subs, _, _, err := buildDeclaredSubagents(
		context.Background(), cfg, mock.NewEcho(), t.TempDir(),
		parentSurface{}, testDeps(),
	)
	if err != nil {
		t.Fatalf("buildDeclaredSubagents: %v", err)
	}
	if len(subs) != 1 {
		t.Fatalf("expected exactly 1 subagent, got %d", len(subs))
	}
	sub := subs[0]
	if got := sub.Inner().Name(); got != "cluster" {
		t.Errorf("subagent name = %q, want %q", got, "cluster")
	}
	if got := sub.Description(); got != "read-only cluster investigator" {
		t.Errorf("subagent description = %q, want %q", got, "read-only cluster investigator")
	}
	// Own model: scripted, NOT the parent's echo.
	if got := sub.Model().Name(); got != "scripted" {
		t.Errorf("subagent LLM = %q, want %q (its own model, not the parent's)", got, "scripted")
	}
}

// TestBuildDeclaredSubagents_InheritsParentModel: when a spec omits Model,
// resolveSubagentModel reuses the parent provider — the subagent runs on the
// parent's model (echo here), the OQ2 "inherit when unset" default.
func TestBuildDeclaredSubagents_InheritsParentModel(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{
		Model: config.ModelConfig{Provider: mock.ProviderEcho, Name: "echo"},
		Subagents: []config.SubagentSpec{{
			Name:        "helper",
			Description: "inherits the parent model",
		}},
	}
	subs, _, _, err := buildDeclaredSubagents(
		context.Background(), cfg, mock.NewEcho(), t.TempDir(),
		parentSurface{}, testDeps(),
	)
	if err != nil {
		t.Fatalf("buildDeclaredSubagents: %v", err)
	}
	if len(subs) != 1 {
		t.Fatalf("expected 1 subagent, got %d", len(subs))
	}
	if got := subs[0].Model().Name(); got != "echo" {
		t.Errorf("inherited LLM = %q, want %q (parent's model)", got, "echo")
	}
}

// TestBuildDeclaredSubagents_NoneDeclared: an empty Subagents block returns
// (nil, nil) so the caller skips agent.WithSubagents entirely.
func TestBuildDeclaredSubagents_NoneDeclared(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Model: config.ModelConfig{Provider: mock.ProviderEcho, Name: "echo"}}
	subs, _, _, err := buildDeclaredSubagents(
		context.Background(), cfg, mock.NewEcho(), t.TempDir(),
		parentSurface{}, testDeps(),
	)
	if err != nil {
		t.Fatalf("buildDeclaredSubagents: %v", err)
	}
	if subs != nil {
		t.Errorf("expected nil slice when no subagents declared, got %v", subs)
	}
}

// TestBuildDeclaredSubagents_RegisteredAsParentTool wires the built
// subagents into a parent via agent.WithSubagents and asserts the named
// tool shows up on the parent — the "invoked by name" half of γ.2. The
// parent needs a session-backed event log for WithSubagents to resolve the
// subsession service, matching the real cmd path.
func TestBuildDeclaredSubagents_RegisteredAsParentTool(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{
		Model: config.ModelConfig{Provider: mock.ProviderEcho, Name: "echo"},
		Subagents: []config.SubagentSpec{{
			Name:        "cluster",
			Description: "read-only cluster investigator",
		}},
	}
	subs, _, _, err := buildDeclaredSubagents(
		context.Background(), cfg, mock.NewEcho(), t.TempDir(),
		parentSurface{}, testDeps(),
	)
	if err != nil {
		t.Fatalf("buildDeclaredSubagents: %v", err)
	}

	dsn := filepath.Join(t.TempDir(), "session.db")
	h, err := eventlog.Open(context.Background(), sqlite.Open(dsn))
	if err != nil {
		t.Fatalf("eventlog.Open: %v", err)
	}
	defer func() { _ = h.Close() }()

	parentLLM, err := mock.NewEcho().Model(context.Background(), "echo")
	if err != nil {
		t.Fatalf("parent model: %v", err)
	}
	parent, err := agent.New(parentLLM,
		agent.WithName("platform"),
		agent.WithEventLog(h),
		agent.WithSession("u", "p"),
		agent.WithSubagents(subs),
	)
	if err != nil {
		t.Fatalf("New parent: %v", err)
	}
	var found bool
	for _, tl := range parent.Tools() {
		if tl.Name() == "cluster" {
			found = true
		}
	}
	if !found {
		t.Error("declared subagent should register as a parent tool named \"cluster\"")
	}
}

// --- γ.3: inline tools / mcp / skills filtering ---

// TestResolveSubagentTools covers the nil=inherit / list=scope /
// empty=grant-none / unknown=error contract for the built-in tool filter.
func TestResolveSubagentTools(t *testing.T) {
	t.Parallel()
	parent := []adktool.Tool{fakeTool{"read_file"}, fakeTool{"bash"}, fakeTool{"write_file"}}

	// nil → inherit the full registry (same instances).
	got, dropped, err := resolveSubagentTools(config.SubagentSpec{Tools: nil}, parent)
	if err != nil {
		t.Fatalf("nil Tools: %v", err)
	}
	if len(got) != len(parent) {
		t.Errorf("nil Tools should inherit all %d, got %d", len(parent), len(got))
	}
	if len(dropped) != 0 {
		t.Errorf("dropped = %v, want none — a registry without spawn tools loses nothing to the carve-out", dropped)
	}

	// list → exactly those, in the order requested.
	got, _, err = resolveSubagentTools(config.SubagentSpec{Tools: []string{"bash", "read_file"}}, parent)
	if err != nil {
		t.Fatalf("list Tools: %v", err)
	}
	if len(got) != 2 || got[0].Name() != "bash" || got[1].Name() != "read_file" {
		t.Errorf("scoped tools = %v, want [bash read_file]", toolNames(got))
	}

	// empty (non-nil) → grant none.
	got, _, err = resolveSubagentTools(config.SubagentSpec{Tools: []string{}}, parent)
	if err != nil {
		t.Fatalf("empty Tools: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("empty Tools should grant none, got %v", toolNames(got))
	}

	// unknown → fail loud.
	if _, _, err := resolveSubagentTools(config.SubagentSpec{Tools: []string{"nope"}}, parent); err == nil {
		t.Error("unknown tool should error")
	}
}

// TestResolveSubagentTools_SpawnCarveOut is the #748 regression at the
// unit level: inheriting the parent's registry must not inherit its
// authority to delegate. The sibling ad-hoc path (factory(), which
// strips r.spawnToolNames from the catalog it hands each session's
// manager) has withheld the same two tools since it was written; which
// path built the list should not decide whether a subagent can build a
// fleet.
//
// Fails on pre-fix code: nil Tools returned the parent slice verbatim,
// spawn_agent and stop_agent included.
func TestResolveSubagentTools_SpawnCarveOut(t *testing.T) {
	t.Parallel()
	parent := []adktool.Tool{
		fakeTool{"read_file"},
		fakeTool{background.SpawnAgentToolName},
		fakeTool{"bash"},
		fakeTool{background.StopAgentToolName},
	}

	// tools: omitted → everything EXCEPT the delegation surface.
	got, dropped, err := resolveSubagentTools(config.SubagentSpec{Tools: nil}, parent)
	if err != nil {
		t.Fatalf("nil Tools: %v", err)
	}
	if names := toolNames(got); !slices.Equal(names, []string{"read_file", "bash"}) {
		t.Errorf("inherited tools = %v, want [read_file bash] — inheriting the parent's hardening is not inheriting its delegation surface", names)
	}
	if !slices.Equal(dropped, []string{background.SpawnAgentToolName, background.StopAgentToolName}) {
		t.Errorf("dropped = %v, want both spawn tools reported so the boot line can say so", dropped)
	}

	// tools: listed explicitly → the deliberate orchestrator-subagent
	// case still works. The carve-out is about what inheritance means,
	// not about a tool an operator may never grant.
	got, dropped, err = resolveSubagentTools(config.SubagentSpec{
		Tools: []string{"read_file", background.SpawnAgentToolName},
	}, parent)
	if err != nil {
		t.Fatalf("explicit Tools: %v", err)
	}
	if names := toolNames(got); !slices.Equal(names, []string{"read_file", background.SpawnAgentToolName}) {
		t.Errorf("explicit tools = %v, want [read_file spawn_agent]", names)
	}
	if len(dropped) != 0 {
		t.Errorf("dropped = %v, want none — an explicit list drops nothing", dropped)
	}
}

func toolNames(ts []adktool.Tool) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.Name())
	}
	return out
}

// TestResolveSubagentToolsets_MCP covers MCP-server name selection: nil
// inherits every started server (nil-toolset servers skipped), a list
// selects by name, an unknown server errors, and an explicit empty list
// grants none. Skills are left nil against an empty surface so only the
// MCP dimension varies.
func TestResolveSubagentToolsets_MCP(t *testing.T) {
	t.Parallel()
	surface := parentSurface{
		mcpToolsets: []namedToolset{
			{name: "gke", toolset: fakeToolset{"gke"}},
			{name: "gke-readonly", toolset: fakeToolset{"gke-readonly"}},
			{name: "broken", toolset: nil}, // started but failed → nil toolset
		},
	}

	// nil → inherit every server that has a toolset (broken skipped).
	res, err := resolveSubagentToolsets(context.Background(), config.SubagentSpec{}, surface)
	if err != nil {
		t.Fatalf("nil MCP: %v", err)
	}
	if names := toolsetNames(res.sets); len(names) != 2 || !strings.Contains(res.desc, "mcp=inherit") {
		t.Errorf("nil MCP inherit = %v (desc %q), want [gke gke-readonly]", names, res.desc)
	}

	// list → only the named server.
	res, err = resolveSubagentToolsets(context.Background(), config.SubagentSpec{MCP: []string{"gke-readonly"}}, surface)
	if err != nil {
		t.Fatalf("scoped MCP: %v", err)
	}
	if names := toolsetNames(res.sets); len(names) != 1 || names[0] != "gke-readonly" {
		t.Errorf("scoped MCP = %v, want [gke-readonly] (the read-only server only, not gke)", names)
	}

	// a named-but-broken server is not an error; it just contributes no
	// toolset (same as the parent sees).
	res, err = resolveSubagentToolsets(context.Background(), config.SubagentSpec{MCP: []string{"broken"}}, surface)
	if err != nil {
		t.Fatalf("broken MCP: %v", err)
	}
	if len(res.sets) != 0 {
		t.Errorf("broken server should contribute no toolset, got %v", toolsetNames(res.sets))
	}

	// empty (non-nil) → grant none.
	res, err = resolveSubagentToolsets(context.Background(), config.SubagentSpec{MCP: []string{}}, surface)
	if err != nil {
		t.Fatalf("empty MCP: %v", err)
	}
	if len(res.sets) != 0 {
		t.Errorf("empty MCP should grant none, got %v", toolsetNames(res.sets))
	}

	// unknown → fail loud.
	if _, err := resolveSubagentToolsets(context.Background(), config.SubagentSpec{MCP: []string{"nope"}}, surface); err == nil {
		t.Error("unknown mcp server should error")
	}
}

// writeSkill drops a minimal <dir>/skills/<name>/SKILL.md so skills.LoadAll
// discovers a real skill, letting the skills dimension of
// resolveSubagentToolsets run against a genuine skill.Source (not a stub).
func writeSkill(t *testing.T, dir, name string) {
	t.Helper()
	skillDir := filepath.Join(dir, skills.SkillDirName, name)
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatalf("mkdir skill: %v", err)
	}
	body := "---\nname: " + name + "\ndescription: the " + name + " skill\n---\n\nbody for " + name
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(body), 0o600); err != nil {
		t.Fatalf("write SKILL.md: %v", err)
	}
}

// TestResolveSubagentToolsets_Skills covers the skills dimension end-to-end
// through the wiring layer (not just skills.Scoped in isolation): nil
// inherits the parent's skill toolset, a list scopes to a filtered view, and
// an explicit empty list grants none — each reflected in both the returned
// toolsets and the human-readable scope description.
func TestResolveSubagentToolsets_Skills(t *testing.T) {
	t.Parallel()
	project := t.TempDir()
	writeSkill(t, project, "alpha")
	writeSkill(t, project, "beta")
	loaded, err := skills.LoadAll(context.Background(), project, "", nil)
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if loaded.Empty() {
		t.Fatal("expected a non-empty skill surface")
	}
	surface := parentSurface{skills: loaded}

	// nil → inherit the parent's (single) skill toolset.
	res, err := resolveSubagentToolsets(context.Background(), config.SubagentSpec{}, surface)
	if err != nil {
		t.Fatalf("nil Skills: %v", err)
	}
	if len(res.sets) != 1 || !strings.Contains(res.desc, "skills=inherit") {
		t.Errorf("nil Skills = %d toolsets (desc %q), want 1 inherited", len(res.sets), res.desc)
	}

	// list → a scoped skill toolset is present, and the description names it.
	res, err = resolveSubagentToolsets(context.Background(), config.SubagentSpec{Skills: []string{"alpha"}}, surface)
	if err != nil {
		t.Fatalf("scoped Skills: %v", err)
	}
	if len(res.sets) != 1 || !strings.Contains(res.desc, "skills=[alpha]") {
		t.Errorf("scoped Skills = %d toolsets (desc %q), want 1 scoped to alpha", len(res.sets), res.desc)
	}

	// empty (non-nil) → grant none: no skill toolset at all.
	res, err = resolveSubagentToolsets(context.Background(), config.SubagentSpec{Skills: []string{}}, surface)
	if err != nil {
		t.Fatalf("empty Skills: %v", err)
	}
	if len(res.sets) != 0 || !strings.Contains(res.desc, "skills=[]") {
		t.Errorf("empty Skills = %d toolsets (desc %q), want none", len(res.sets), res.desc)
	}

	// unknown → fail loud.
	if _, err := resolveSubagentToolsets(context.Background(), config.SubagentSpec{Skills: []string{"nope"}}, surface); err == nil {
		t.Error("unknown skill should error")
	}
}

// TestBuildDeclaredSubagents_ScopedToolsSurface is the end-to-end γ.3
// check: a subagent that names a built-in tool subset is constructed with
// exactly that subset (asserted via the parent-visible Tools() list on the
// subagent itself).
func TestBuildDeclaredSubagents_ScopedToolsSurface(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{
		Model: config.ModelConfig{Provider: mock.ProviderEcho, Name: "echo"},
		Subagents: []config.SubagentSpec{{
			Name:  "reader",
			Tools: []string{"read_file"},
		}},
	}
	surface := parentSurface{
		builtinTools: []adktool.Tool{fakeTool{"read_file"}, fakeTool{"bash"}},
	}
	subs, _, _, err := buildDeclaredSubagents(
		context.Background(), cfg, mock.NewEcho(), t.TempDir(),
		surface, testDeps(),
	)
	if err != nil {
		t.Fatalf("buildDeclaredSubagents: %v", err)
	}
	if len(subs) != 1 {
		t.Fatalf("expected 1 subagent, got %d", len(subs))
	}
	var names []string
	for _, tl := range subs[0].Tools() {
		names = append(names, tl.Name())
	}
	if len(names) != 1 || names[0] != "read_file" {
		t.Errorf("scoped subagent tools = %v, want [read_file] (bash excluded)", names)
	}
}

// --- v2: per-subagent content root (spec.Root) ---

// writeRootAGENTS drops <dir>/AGENTS.md so a subagent root has a persona to
// auto-assemble from.
func writeRootAGENTS(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte(body), 0o600); err != nil {
		t.Fatalf("write AGENTS.md: %v", err)
	}
}

// TestRootedSubagentInstruction_AutoAssemble: with no inline Instructions, a
// rooted subagent's persona is assembled from the root's own AGENTS.md.
func TestRootedSubagentInstruction_AutoAssemble(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeRootAGENTS(t, root, "You are the cluster specialist.\n")

	got, err := rootedSubagentInstruction(config.SubagentSpec{Name: "cluster", Root: root}, root, identityInterp)
	if err != nil {
		t.Fatalf("rootedSubagentInstruction: %v", err)
	}
	if !strings.Contains(got, "You are the cluster specialist.") {
		t.Errorf("auto-assembled persona = %q, want it to include the root's AGENTS.md body", got)
	}
}

// TestRootedSubagentInstruction_InlineOverrides: an inline Instructions field
// takes precedence over the root's AGENTS.md (the root still supplies skills +
// mcp, but the persona is operator-authored inline).
func TestRootedSubagentInstruction_InlineOverrides(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeRootAGENTS(t, root, "AUTO-ASSEMBLED PERSONA — should not appear.\n")

	got, err := rootedSubagentInstruction(
		config.SubagentSpec{Name: "cluster", Root: root, Instructions: "INLINE PERSONA WINS."},
		root, identityInterp,
	)
	if err != nil {
		t.Fatalf("rootedSubagentInstruction: %v", err)
	}
	if got != "INLINE PERSONA WINS." {
		t.Errorf("persona = %q, want the inline override (not the root's AGENTS.md)", got)
	}
	if strings.Contains(got, "AUTO-ASSEMBLED") {
		t.Error("inline Instructions must override the root's AGENTS.md, not concatenate with it")
	}
}

// TestLoadSubagentRoot_OwnSkills: a rooted subagent loads its OWN skills/ tree
// — independent of the parent surface. The returned surface carries the root's
// skills (both of them) and no MCP servers (the root has no mcp.json).
func TestLoadSubagentRoot_OwnSkills(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeSkill(t, root, "rootalpha")
	writeSkill(t, root, "rootbeta")

	deps := subagentDeps{interp: identityInterp, send: discard, rootBase: t.TempDir()}
	rootAbs, surface, servers, err := loadSubagentRoot(
		context.Background(), config.SubagentSpec{Name: "cluster", Root: root}, deps,
	)
	if err != nil {
		t.Fatalf("loadSubagentRoot: %v", err)
	}
	if rootAbs != filepath.Clean(root) {
		t.Errorf("rootAbs = %q, want %q (absolute root passes through)", rootAbs, filepath.Clean(root))
	}
	if len(servers) != 0 {
		t.Errorf("got %d MCP servers, want 0 (root has no mcp.json)", len(servers))
	}
	if surface.skills.Empty() {
		t.Fatal("root's skills surface should be non-empty")
	}
	// nil Skills → the subagent inherits ALL of the root's skills (one toolset).
	res, err := resolveSubagentToolsets(context.Background(), config.SubagentSpec{Root: root}, surface)
	if err != nil {
		t.Fatalf("resolveSubagentToolsets (nil skills): %v", err)
	}
	if len(res.sets) != 1 || !strings.Contains(res.desc, "skills=inherit") {
		t.Errorf("nil Skills over root surface = %d toolsets (desc %q), want 1 (all root skills)", len(res.sets), res.desc)
	}
	// A list scopes WITHIN the root.
	if _, err := resolveSubagentToolsets(context.Background(), config.SubagentSpec{Root: root, Skills: []string{"rootalpha"}}, surface); err != nil {
		t.Fatalf("scoped skills within root: %v", err)
	}
	// A name that isn't in the root fails loud — proving the scope is the
	// root's own tree, not the parent's.
	if _, err := resolveSubagentToolsets(context.Background(), config.SubagentSpec{Root: root, Skills: []string{"parentonly"}}, surface); err == nil {
		t.Error("a skill absent from the root should error (root scope, not parent scope)")
	}
}

// TestRootInventory_DistinguishesEmptyRoot: the rooted subagent's boot line
// must report what the root actually yielded. The mcp=/skills= scoping fields
// read IDENTICALLY for a root with skills and a root without — so without the
// counts, a misnamed `skill/` directory boots clean and silent and the
// subagent runs persona-only, surfacing much later as a specialist that
// mysteriously can't do its job.
func TestRootInventory_DistinguishesEmptyRoot(t *testing.T) {
	t.Parallel()

	load := func(root string) (parentSurface, string) {
		t.Helper()
		deps := subagentDeps{interp: identityInterp, send: discard, rootBase: t.TempDir()}
		_, surface, _, err := loadSubagentRoot(
			context.Background(), config.SubagentSpec{Name: "cluster", Root: root}, deps,
		)
		if err != nil {
			t.Fatalf("loadSubagentRoot(%s): %v", root, err)
		}
		res, err := resolveSubagentToolsets(context.Background(), config.SubagentSpec{Root: root}, surface)
		if err != nil {
			t.Fatalf("resolveSubagentToolsets(%s): %v", root, err)
		}
		return surface, res.desc
	}

	populated := t.TempDir()
	writeSkill(t, populated, "rootalpha")
	writeSkill(t, populated, "rootbeta")

	// Persona but no skills/ tree — the silent-boot case.
	bare := t.TempDir()
	writeRootAGENTS(t, bare, "persona only\n")

	popSurface, popScope := load(populated)
	bareSurface, bareScope := load(bare)

	// Precondition: the scoping fields alone cannot tell these apart.
	if !strings.Contains(popScope, "skills=inherit") || !strings.Contains(bareScope, "skills=inherit") {
		t.Fatalf("both scopes should read skills=inherit, got %q and %q", popScope, bareScope)
	}

	gotPop, gotBare := rootInventory(popSurface), rootInventory(bareSurface)
	if want := "skills: 2 loaded"; !strings.Contains(gotPop, want) {
		t.Errorf("populated root inventory = %q, want it to contain %q", gotPop, want)
	}
	if want := "skills: 0 loaded"; !strings.Contains(gotBare, want) {
		t.Errorf("bare root inventory = %q, want it to contain %q", gotBare, want)
	}
	if want := "mcp: 0 server(s)"; !strings.Contains(gotBare, want) {
		t.Errorf("bare root inventory = %q, want it to contain %q (no mcp.json)", gotBare, want)
	}
	if gotPop == gotBare {
		t.Errorf("inventory must distinguish a populated root from a bare one; both = %q", gotPop)
	}
}

// TestBuildDeclaredSubagents_SpawnCarveOutReachesBothTwins is the #748
// regression where it was observed: the live GKE UAT of main-81020e9 had
// the `cluster` specialist — a spec with no tools: key — call
// spawn_agent to delegate its own investigation to another `cluster`.
// #742's lineage guard refused that particular call, but the guard is a
// backstop for a tool the subagent should not have been holding, and it
// only covers SELF-spawn: with two specialists on the roster, one could
// delegate to the other, unbounded, from a spec that never asked to.
//
// Both twins are asserted because they are built from the same resolved
// slice and either one leaking is the whole bug: the sync subagent-tool
// the parent calls, and the async template spawn_agent {agent: "..."}
// spawns. The boot line is asserted too — a carve-out an operator can't
// see at startup is one they rediscover from a refused call mid-run.
//
// Fails on pre-fix code: both surfaces carry spawn_agent + stop_agent,
// and the boot line says only "tools=inherit".
func TestBuildDeclaredSubagents_SpawnCarveOutReachesBothTwins(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{
		Model: config.ModelConfig{Provider: mock.ProviderEcho, Name: "echo"},
		Subagents: []config.SubagentSpec{{
			Name:        "cluster",
			Description: "read-only cluster investigator",
			// No Tools key: "inherit the parent's hardening".
		}},
	}
	// The parent's registry as main.go assembles it: built-ins with the
	// spawn tools appended (main.go:1262).
	surface := parentSurface{builtinTools: []adktool.Tool{
		fakeTool{"read_file"},
		fakeTool{"bash"},
		fakeTool{background.SpawnAgentToolName},
		fakeTool{background.StopAgentToolName},
	}}
	var lines []string
	deps := subagentDeps{interp: identityInterp, send: func(s string) { lines = append(lines, s) }}

	subs, templates, _, err := buildDeclaredSubagents(
		context.Background(), cfg, mock.NewEcho(), t.TempDir(), surface, deps,
	)
	if err != nil {
		t.Fatalf("buildDeclaredSubagents: %v", err)
	}
	if len(subs) != 1 || len(templates) != 1 {
		t.Fatalf("got %d subagents / %d templates, want 1 each", len(subs), len(templates))
	}

	for _, tc := range []struct {
		twin  string
		tools []adktool.Tool
	}{
		{"sync subagent-tool", subs[0].Tools()},
		{"async spawn template", templates[0].Tools},
	} {
		names := toolNames(tc.tools)
		for _, banned := range background.SpawnToolNames() {
			if slices.Contains(names, banned) {
				t.Errorf("%s holds %q; tools: was omitted, so delegation was never asked for (got %v)", tc.twin, banned, names)
			}
		}
		for _, want := range []string{"read_file", "bash"} {
			if !slices.Contains(names, want) {
				t.Errorf("%s lost %q — the carve-out must take the spawn tools and nothing else (got %v)", tc.twin, want, names)
			}
		}
	}

	if len(lines) != 1 || !strings.Contains(lines[0], "spawn=withheld") {
		t.Errorf("boot lines = %q, want one line reporting spawn=withheld", lines)
	}
}

// TestBuildDeclaredSubagents_ExplicitSpawnToolsStillGranted is the other
// half of #748: the carve-out changes what INHERITANCE means, not what an
// operator may grant. A spec that names the spawn tools gets them, which
// is how a deliberate orchestrator subagent is written.
func TestBuildDeclaredSubagents_ExplicitSpawnToolsStillGranted(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{
		Model: config.ModelConfig{Provider: mock.ProviderEcho, Name: "echo"},
		Subagents: []config.SubagentSpec{{
			Name:  "orchestrator",
			Tools: []string{"read_file", background.SpawnAgentToolName},
		}},
	}
	surface := parentSurface{builtinTools: []adktool.Tool{
		fakeTool{"read_file"},
		fakeTool{"bash"},
		fakeTool{background.SpawnAgentToolName},
		fakeTool{background.StopAgentToolName},
	}}
	var lines []string
	deps := subagentDeps{interp: identityInterp, send: func(s string) { lines = append(lines, s) }}

	subs, _, _, err := buildDeclaredSubagents(
		context.Background(), cfg, mock.NewEcho(), t.TempDir(), surface, deps,
	)
	if err != nil {
		t.Fatalf("buildDeclaredSubagents: %v", err)
	}
	names := toolNames(subs[0].Tools())
	if !slices.Contains(names, background.SpawnAgentToolName) {
		t.Errorf("orchestrator tools = %v, want spawn_agent — an explicit grant must survive the carve-out", names)
	}
	if slices.Contains(names, background.StopAgentToolName) {
		t.Errorf("orchestrator tools = %v, want no stop_agent — the list is the whole grant", names)
	}
	if len(lines) != 1 || strings.Contains(lines[0], "spawn=withheld") {
		t.Errorf("boot lines = %q, want no withheld note when nothing was withheld", lines)
	}
}

// TestRootInventory_ReportsDownServers: a server that is configured but failed
// to start is listed with a nil toolset. The boot line must say so rather than
// counting it as healthy — "mcp: 2 server(s)" with both down looks the same as
// two working ones otherwise.
func TestRootInventory_ReportsDownServers(t *testing.T) {
	t.Parallel()
	got := rootInventory(parentSurface{mcpToolsets: []namedToolset{
		{name: "gke", toolset: fakeToolset{"gke"}},
		{name: "developer_knowledge", toolset: nil},
	}})
	if !strings.Contains(got, "mcp: 2 server(s)") || !strings.Contains(got, "1 down") {
		t.Errorf("inventory = %q, want 2 servers with 1 reported down", got)
	}
}

// TestLoadSubagentRoot_RelativeResolvesAgainstBase: a relative spec.Root joins
// deps.rootBase, mirroring content_roots resolution.
func TestLoadSubagentRoot_RelativeResolvesAgainstBase(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	child := filepath.Join(base, "cluster")
	writeRootAGENTS(t, child, "child persona\n")

	deps := subagentDeps{interp: identityInterp, send: discard, rootBase: base}
	rootAbs, _, _, err := loadSubagentRoot(
		context.Background(), config.SubagentSpec{Name: "cluster", Root: "cluster"}, deps,
	)
	if err != nil {
		t.Fatalf("loadSubagentRoot: %v", err)
	}
	if rootAbs != child {
		t.Errorf("relative root resolved to %q, want %q (joined against rootBase)", rootAbs, child)
	}
}

// TestLoadSubagentRoot_MissingRootErrors: an operator-declared root that does
// not exist is a loud error, not a silently-empty scope.
func TestLoadSubagentRoot_MissingRootErrors(t *testing.T) {
	t.Parallel()
	deps := subagentDeps{interp: identityInterp, send: discard, rootBase: t.TempDir()}
	if _, _, _, err := loadSubagentRoot(
		context.Background(),
		config.SubagentSpec{Name: "cluster", Root: filepath.Join(t.TempDir(), "does-not-exist")},
		deps,
	); err == nil {
		t.Error("a missing root should error (declared trust, so a typo must surface)")
	}
}

// TestBuildDeclaredSubagents_RootedEndToEnd wires a rooted subagent through the
// full builder: its persona comes from the root's AGENTS.md, its tools are the
// named built-in subset (built-ins always resolve against the parent binary),
// and no MCP servers are returned (the root has none).
func TestBuildDeclaredSubagents_RootedEndToEnd(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeRootAGENTS(t, root, "rooted cluster persona\n")
	writeSkill(t, root, "clusterskill")

	cfg := &config.Config{
		Model: config.ModelConfig{Provider: mock.ProviderEcho, Name: "echo"},
		Subagents: []config.SubagentSpec{{
			Name:  "cluster",
			Root:  root,
			Tools: []string{"read_file"},
		}},
	}
	// Parent surface deliberately carries DIFFERENT built-ins so the tools
	// filter is observable; the subagent must not pick up parent skills/mcp
	// (it has a root), which this test confirms via the returned server slice
	// and the scoped tool list.
	surface := parentSurface{builtinTools: []adktool.Tool{fakeTool{"read_file"}, fakeTool{"bash"}}}

	subs, _, servers, err := buildDeclaredSubagents(
		context.Background(), cfg, mock.NewEcho(), t.TempDir(),
		surface, subagentDeps{interp: identityInterp, send: discard, rootBase: t.TempDir()},
	)
	if err != nil {
		t.Fatalf("buildDeclaredSubagents: %v", err)
	}
	if len(subs) != 1 {
		t.Fatalf("expected 1 subagent, got %d", len(subs))
	}
	if len(servers) != 0 {
		t.Errorf("rooted subagent with no mcp.json should return 0 servers, got %d", len(servers))
	}
	var names []string
	for _, tl := range subs[0].Tools() {
		names = append(names, tl.Name())
	}
	// read_file (scoped built-in) + list_skills/load_skill from the root's
	// own skill toolset. Assert the built-in subset is exactly [read_file].
	var hasReadFile, hasBash bool
	for _, n := range names {
		switch n {
		case "read_file":
			hasReadFile = true
		case "bash":
			hasBash = true
		}
	}
	if !hasReadFile || hasBash {
		t.Errorf("rooted subagent built-ins = %v, want read_file present and bash excluded", names)
	}
}

// TestResolveSubagentToolsets_InfosNameTheToolsInside is the #768
// snapshot: this is the only layer that knows which toolset came from
// which MCP server, so it has to record the answer. Downstream —
// background.Catalog, GET .../subagents — a tool.Toolset is opaque, and
// a lost server name is not recoverable from a tool name (which is
// exactly the prefix-splitting guesswork #767 removed from /tools).
func TestResolveSubagentToolsets_InfosNameTheToolsInside(t *testing.T) {
	t.Parallel()
	project := t.TempDir()
	writeSkill(t, project, "alpha")
	loaded, err := skills.LoadAll(context.Background(), project, "", nil)
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	surface := parentSurface{
		mcpToolsets: []namedToolset{
			{name: "gke", toolset: fakeToolset{"gke"}, infos: []mcp.ToolInfo{
				{Name: "gke_list_clusters", Description: "list clusters"},
				{Name: "gke_get_pod", Description: "get a pod"},
			}},
			// Started but failed: nil toolset, and no infos to contribute.
			{name: "broken", toolset: nil, infos: []mcp.ToolInfo{{Name: "phantom"}}},
		},
		skills: loaded,
	}

	res, err := resolveSubagentToolsets(context.Background(), config.SubagentSpec{}, surface)
	if err != nil {
		t.Fatalf("resolveSubagentToolsets: %v", err)
	}

	byName := map[string]attach.ToolInfo{}
	for _, ti := range res.infos {
		byName[ti.Name] = ti
	}
	if got := byName["gke_list_clusters"]; got.Source != attach.ToolSourceMCP || got.Server != "gke" || got.Description == "" {
		t.Errorf("gke_list_clusters = %+v, want source=mcp server=gke with a description", got)
	}
	if _, ok := byName["phantom"]; ok {
		t.Error("a failed server contributed a tool the subagent cannot call")
	}
	// The skill toolset exposes its fixed trio, not one tool per skill.
	if got, ok := byName["list_skills"]; !ok || got.Source != attach.ToolSourceSkill {
		t.Errorf("list_skills = %+v (present=%v), want source=skill", got, ok)
	}
	if _, ok := byName["alpha"]; ok {
		t.Error("the SKILL alpha was reported as a tool; the model calls load_skill, not alpha")
	}
}

// TestResolveSubagentToolsets_InfosFollowTheScope: the snapshot must
// describe what this subagent got, not what the parent has. A subagent
// scoped to no MCP and no skills reporting the parent's roster would be
// worse than reporting nothing — it is a confident wrong answer to
// "what can this specialist reach?".
func TestResolveSubagentToolsets_InfosFollowTheScope(t *testing.T) {
	t.Parallel()
	project := t.TempDir()
	writeSkill(t, project, "alpha")
	loaded, err := skills.LoadAll(context.Background(), project, "", nil)
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	surface := parentSurface{
		mcpToolsets: []namedToolset{
			{name: "gke", toolset: fakeToolset{"gke"}, infos: []mcp.ToolInfo{{Name: "gke_get_pod"}}},
			{name: "fs", toolset: fakeToolset{"fs"}, infos: []mcp.ToolInfo{{Name: "fs_read_file"}}},
		},
		skills: loaded,
	}

	// Scoped to one server, no skills.
	res, err := resolveSubagentToolsets(context.Background(), config.SubagentSpec{
		MCP:    []string{"fs"},
		Skills: []string{},
	}, surface)
	if err != nil {
		t.Fatalf("scoped: %v", err)
	}
	if len(res.infos) != 1 || res.infos[0].Name != "fs_read_file" {
		t.Fatalf("scoped infos = %+v, want only fs_read_file", res.infos)
	}

	// Granted nothing at all: no infos, not the parent's.
	res, err = resolveSubagentToolsets(context.Background(), config.SubagentSpec{
		MCP:    []string{},
		Skills: []string{},
	}, surface)
	if err != nil {
		t.Fatalf("empty: %v", err)
	}
	if len(res.infos) != 0 {
		t.Errorf("ungranted subagent reported %+v, want nothing", res.infos)
	}
}

// TestBuildDeclaredSubagents_TemplateCarriesToolsetSnapshot closes the
// loop: the async template the builder hands the manager must carry the
// snapshot, or Catalog() reports built-ins only and the MCP/skill half
// of #768 silently never ships.
func TestBuildDeclaredSubagents_TemplateCarriesToolsetSnapshot(t *testing.T) {
	t.Parallel()
	project := t.TempDir()
	writeSkill(t, project, "alpha")
	loaded, err := skills.LoadAll(context.Background(), project, "", nil)
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	cfg := &config.Config{
		Model: config.ModelConfig{Provider: mock.ProviderEcho, Name: "echo"},
		Subagents: []config.SubagentSpec{{
			Name:  "cluster",
			Tools: []string{"read_file"},
		}},
	}
	surface := parentSurface{
		builtinTools: []adktool.Tool{fakeTool{"read_file"}},
		mcpToolsets: []namedToolset{
			{name: "gke", toolset: fakeToolset{"gke"}, infos: []mcp.ToolInfo{{Name: "gke_get_pod", Description: "get a pod"}}},
		},
		skills: loaded,
	}

	_, templates, _, err := buildDeclaredSubagents(
		context.Background(), cfg, mock.NewEcho(), t.TempDir(), surface, testDeps(),
	)
	if err != nil {
		t.Fatalf("buildDeclaredSubagents: %v", err)
	}
	if len(templates) != 1 {
		t.Fatalf("expected 1 template, got %d", len(templates))
	}
	var sawMCP, sawSkill bool
	for _, ti := range templates[0].ToolsetTools {
		switch ti.Source {
		case attach.ToolSourceMCP:
			sawMCP = ti.Name == "gke_get_pod" && ti.Server == "gke"
		case attach.ToolSourceSkill:
			sawSkill = true
		}
	}
	if !sawMCP || !sawSkill {
		t.Errorf("template ToolsetTools = %+v, want the gke MCP tool (attributed) and the skill tools", templates[0].ToolsetTools)
	}
}
