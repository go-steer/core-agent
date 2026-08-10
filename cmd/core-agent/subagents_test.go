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
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	adkagent "google.golang.org/adk/agent"
	adktool "google.golang.org/adk/tool"

	"github.com/go-steer/core-agent/v2/pkg/agent"
	"github.com/go-steer/core-agent/v2/pkg/config"
	"github.com/go-steer/core-agent/v2/pkg/eventlog"
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

	subs, _, err := buildDeclaredSubagents(
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
	subs, _, err := buildDeclaredSubagents(
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
	subs, _, err := buildDeclaredSubagents(
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
	subs, _, err := buildDeclaredSubagents(
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
	got, err := resolveSubagentTools(config.SubagentSpec{Tools: nil}, parent)
	if err != nil {
		t.Fatalf("nil Tools: %v", err)
	}
	if len(got) != len(parent) {
		t.Errorf("nil Tools should inherit all %d, got %d", len(parent), len(got))
	}

	// list → exactly those, in the order requested.
	got, err = resolveSubagentTools(config.SubagentSpec{Tools: []string{"bash", "read_file"}}, parent)
	if err != nil {
		t.Fatalf("list Tools: %v", err)
	}
	if len(got) != 2 || got[0].Name() != "bash" || got[1].Name() != "read_file" {
		t.Errorf("scoped tools = %v, want [bash read_file]", toolNames(got))
	}

	// empty (non-nil) → grant none.
	got, err = resolveSubagentTools(config.SubagentSpec{Tools: []string{}}, parent)
	if err != nil {
		t.Fatalf("empty Tools: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("empty Tools should grant none, got %v", toolNames(got))
	}

	// unknown → fail loud.
	if _, err := resolveSubagentTools(config.SubagentSpec{Tools: []string{"nope"}}, parent); err == nil {
		t.Error("unknown tool should error")
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
	out, desc, err := resolveSubagentToolsets(context.Background(), config.SubagentSpec{}, surface)
	if err != nil {
		t.Fatalf("nil MCP: %v", err)
	}
	if names := toolsetNames(out); len(names) != 2 || !strings.Contains(desc, "mcp=inherit") {
		t.Errorf("nil MCP inherit = %v (desc %q), want [gke gke-readonly]", names, desc)
	}

	// list → only the named server.
	out, _, err = resolveSubagentToolsets(context.Background(), config.SubagentSpec{MCP: []string{"gke-readonly"}}, surface)
	if err != nil {
		t.Fatalf("scoped MCP: %v", err)
	}
	if names := toolsetNames(out); len(names) != 1 || names[0] != "gke-readonly" {
		t.Errorf("scoped MCP = %v, want [gke-readonly] (the read-only server only, not gke)", names)
	}

	// a named-but-broken server is not an error; it just contributes no
	// toolset (same as the parent sees).
	out, _, err = resolveSubagentToolsets(context.Background(), config.SubagentSpec{MCP: []string{"broken"}}, surface)
	if err != nil {
		t.Fatalf("broken MCP: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("broken server should contribute no toolset, got %v", toolsetNames(out))
	}

	// empty (non-nil) → grant none.
	out, _, err = resolveSubagentToolsets(context.Background(), config.SubagentSpec{MCP: []string{}}, surface)
	if err != nil {
		t.Fatalf("empty MCP: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("empty MCP should grant none, got %v", toolsetNames(out))
	}

	// unknown → fail loud.
	if _, _, err := resolveSubagentToolsets(context.Background(), config.SubagentSpec{MCP: []string{"nope"}}, surface); err == nil {
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
	out, desc, err := resolveSubagentToolsets(context.Background(), config.SubagentSpec{}, surface)
	if err != nil {
		t.Fatalf("nil Skills: %v", err)
	}
	if len(out) != 1 || !strings.Contains(desc, "skills=inherit") {
		t.Errorf("nil Skills = %d toolsets (desc %q), want 1 inherited", len(out), desc)
	}

	// list → a scoped skill toolset is present, and the description names it.
	out, desc, err = resolveSubagentToolsets(context.Background(), config.SubagentSpec{Skills: []string{"alpha"}}, surface)
	if err != nil {
		t.Fatalf("scoped Skills: %v", err)
	}
	if len(out) != 1 || !strings.Contains(desc, "skills=[alpha]") {
		t.Errorf("scoped Skills = %d toolsets (desc %q), want 1 scoped to alpha", len(out), desc)
	}

	// empty (non-nil) → grant none: no skill toolset at all.
	out, desc, err = resolveSubagentToolsets(context.Background(), config.SubagentSpec{Skills: []string{}}, surface)
	if err != nil {
		t.Fatalf("empty Skills: %v", err)
	}
	if len(out) != 0 || !strings.Contains(desc, "skills=[]") {
		t.Errorf("empty Skills = %d toolsets (desc %q), want none", len(out), desc)
	}

	// unknown → fail loud.
	if _, _, err := resolveSubagentToolsets(context.Background(), config.SubagentSpec{Skills: []string{"nope"}}, surface); err == nil {
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
	subs, _, err := buildDeclaredSubagents(
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
	out, desc, err := resolveSubagentToolsets(context.Background(), config.SubagentSpec{Root: root}, surface)
	if err != nil {
		t.Fatalf("resolveSubagentToolsets (nil skills): %v", err)
	}
	if len(out) != 1 || !strings.Contains(desc, "skills=inherit") {
		t.Errorf("nil Skills over root surface = %d toolsets (desc %q), want 1 (all root skills)", len(out), desc)
	}
	// A list scopes WITHIN the root.
	if _, _, err := resolveSubagentToolsets(context.Background(), config.SubagentSpec{Root: root, Skills: []string{"rootalpha"}}, surface); err != nil {
		t.Fatalf("scoped skills within root: %v", err)
	}
	// A name that isn't in the root fails loud — proving the scope is the
	// root's own tree, not the parent's.
	if _, _, err := resolveSubagentToolsets(context.Background(), config.SubagentSpec{Root: root, Skills: []string{"parentonly"}}, surface); err == nil {
		t.Error("a skill absent from the root should error (root scope, not parent scope)")
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

	subs, servers, err := buildDeclaredSubagents(
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
