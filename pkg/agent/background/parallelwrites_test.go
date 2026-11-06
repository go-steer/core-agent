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
	"iter"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	adkagent "google.golang.org/adk/v2/agent"
	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v3/pkg/attach/wire"
	"github.com/go-steer/core-agent/v3/pkg/config"
	"github.com/go-steer/core-agent/v3/pkg/permissions"
	coretools "github.com/go-steer/core-agent/v3/pkg/tools"
)

// parkedProvider yields an LLM that parks until release is closed,
// so a spawned subagent is reliably still Running when the next Spawn
// is attempted. Without it the echo mock finishes in microseconds and
// every "while another is running" assertion becomes a race the test
// usually loses.
type parkedProvider struct{ release <-chan struct{} }

func (p *parkedProvider) Name() string { return "parked" }
func (p *parkedProvider) Model(context.Context, string) (adkmodel.LLM, error) {
	return &parkedLLM{release: p.release}, nil
}

type parkedLLM struct{ release <-chan struct{} }

func (*parkedLLM) Name() string { return "parked-llm" }
func (l *parkedLLM) GenerateContent(ctx context.Context, _ *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		select {
		case <-l.release:
		case <-ctx.Done():
		case <-time.After(30 * time.Second): // test-hang backstop
		}
		yield(&adkmodel.LLMResponse{
			Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "done"}}},
			FinishReason: genai.FinishReasonStop,
			TurnComplete: true,
		}, nil)
	}
}

// newWriteGuardManager builds a manager whose catalog holds one
// mutating tool (write_file) and one read-only tool (grep), both real
// enough for tools.WritesSharedFilesystem to classify by name.
func newWriteGuardManager(t *testing.T, policy string, release <-chan struct{}) *Manager {
	t.Helper()
	mgr, err := NewManager(
		WithProvider(&parkedProvider{release: release}, "parked"),
		WithMaxConcurrent(8),
		WithAlertBuffer(16),
		WithCatalog([]tool.Tool{newNamedStubTool(t, "write_file"), newNamedStubTool(t, "grep")}),
		WithParallelWritePolicy(policy),
	)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	newTestParent(t, mgr)
	return mgr
}

func spawnWith(t *testing.T, mgr *Manager, name string, tools ...string) (*Handle, error) {
	t.Helper()
	return mgr.Spawn(context.Background(), "", Spec{
		Name: name, Goal: "work", SystemPrompt: "work", Tools: tools,
	})
}

// The behaviour #653 asks for: while one write-capable background
// subagent holds the shared working directory, a second one is turned
// away rather than allowed to interleave writes into the same tree.
func TestSpawn_RefusesASecondWriteCapableSubagent(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	mgr := newWriteGuardManager(t, "", release) // "" resolves to refuse
	t.Cleanup(func() { _ = mgr.Close() })

	if _, err := spawnWith(t, mgr, "w1", "write_file"); err != nil {
		t.Fatalf("first writer: %v", err)
	}
	_, err := spawnWith(t, mgr, "w2", "write_file")
	if !errors.Is(err, ErrConcurrentWriters) {
		t.Fatalf("second writer: got %v, want ErrConcurrentWriters", err)
	}
	// A refusal the model can't route around is a retry loop. It has to
	// name the agent holding the tree and the way out.
	for _, want := range []string{`"w1"`, "stop_agent", "spawn_remote_agent"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not mention %q: %v", want, err)
		}
	}
}

// The guard is about the filesystem, not about concurrency. Read-only
// subagents fan out exactly as before — refusing them would be refusing
// on a hazard they do not pose.
func TestSpawn_ReadOnlySubagentsStillFanOut(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	mgr := newWriteGuardManager(t, "refuse", release)
	t.Cleanup(func() { _ = mgr.Close() })

	for _, n := range []string{"r1", "r2", "r3"} {
		if _, err := spawnWith(t, mgr, n, "grep"); err != nil {
			t.Fatalf("read-only spawn %s: %v", n, err)
		}
	}
	// And a writer may join them: none of the three holds the tree.
	if _, err := spawnWith(t, mgr, "w1", "write_file"); err != nil {
		t.Fatalf("writer alongside readers: %v", err)
	}
}

// A subagent with no tools at all gets only the auto-wired control-plane
// set (report_completed / report_alert), which mutates nothing on disk.
func TestSpawn_ToollessSubagentsAreNotWriters(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	mgr := newWriteGuardManager(t, "refuse", release)
	t.Cleanup(func() { _ = mgr.Close() })

	if _, err := spawnWith(t, mgr, "t1"); err != nil {
		t.Fatalf("first toolless spawn: %v", err)
	}
	if _, err := spawnWith(t, mgr, "t2"); err != nil {
		t.Fatalf("second toolless spawn: %v", err)
	}
}

// The refusal message tells the model to use spawn_remote_agent when two
// agents really must write in parallel. That advice is only true if a
// remote agent does not itself hold the local tree — and today it does
// not, because registerRemote leaves Handle.writesFS at its zero value
// (a remote runs out of process, against its own filesystem).
//
// That is a one-word change away from being false, and the word is in
// another file. Set writesFS on the remote handle and the escape hatch
// in our own error message closes: the model would be told to do the one
// thing that is also refused.
func TestSpawn_ARemoteAgentDoesNotHoldTheLocalTree(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	mgr := newWriteGuardManager(t, "refuse", release)
	t.Cleanup(func() { _ = mgr.Close() })

	rh := &fakeRemoteHandle{id: "r1", events: make(chan RemoteAgentEvent, 1)}
	defer close(rh.events) // lets fanInRemote exit
	mgr.registerRemote(rh, RemoteAgentSpec{Name: "remote-writer"})

	if _, err := spawnWith(t, mgr, "w1", "write_file"); err != nil {
		t.Fatalf("local writer alongside a remote agent: %v", err)
	}
	// And the converse, so this is not passing merely because
	// registerRemote failed to register anything.
	if _, err := spawnWith(t, mgr, "w2", "write_file"); !errors.Is(err, ErrConcurrentWriters) {
		t.Fatalf("a LOCAL second writer should still be refused, got %v", err)
	}
}

// The escape hatches. warn and allow both start the second writer; the
// difference is only whether the operator gets told.
func TestSpawn_WarnAndAllowPermitConcurrentWriters(t *testing.T) {
	for _, policy := range []string{"warn", "allow"} {
		t.Run(policy, func(t *testing.T) {
			release := make(chan struct{})
			defer close(release)
			mgr := newWriteGuardManager(t, policy, release)
			t.Cleanup(func() { _ = mgr.Close() })

			if _, err := spawnWith(t, mgr, "w1", "write_file"); err != nil {
				t.Fatalf("first writer: %v", err)
			}
			if _, err := spawnWith(t, mgr, "w2", "write_file"); err != nil {
				t.Fatalf("second writer under %q: %v", policy, err)
			}
		})
	}
}

// The tree is held by a RUNNING writer, not by a name that once ran.
// Without this the first writer would burn the slot for the life of the
// process and every later write-capable spawn would be refused.
func TestSpawn_TerminalWriterReleasesTheTree(t *testing.T) {
	release := make(chan struct{})
	mgr := newWriteGuardManager(t, "refuse", release)
	t.Cleanup(func() { _ = mgr.Close() })

	h1, err := spawnWith(t, mgr, "w1", "write_file")
	if err != nil {
		t.Fatalf("first writer: %v", err)
	}
	if _, err := spawnWith(t, mgr, "w2", "write_file"); !errors.Is(err, ErrConcurrentWriters) {
		t.Fatalf("precondition: second writer should be refused while w1 runs, got %v", err)
	}
	close(release)
	select {
	case <-h1.Done():
	case <-time.After(20 * time.Second):
		t.Fatal("w1 never finished")
	}
	if _, err := spawnWith(t, mgr, "w2", "write_file"); err != nil {
		t.Fatalf("writer after w1 terminated: %v", err)
	}
}

// A concurrency cap and a write guard are different questions. This
// pins that the write refusal is not just ErrTooManyConcurrent wearing
// a different name: max is 8 here and two spawns trip it.
func TestSpawn_WriteRefusalIsNotTheConcurrencyCap(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	mgr := newWriteGuardManager(t, "refuse", release)
	t.Cleanup(func() { _ = mgr.Close() })

	if _, err := spawnWith(t, mgr, "w1", "write_file"); err != nil {
		t.Fatalf("first writer: %v", err)
	}
	_, err := spawnWith(t, mgr, "w2", "write_file")
	if errors.Is(err, ErrTooManyConcurrent) {
		t.Fatalf("refused by the concurrency cap, not the write guard: %v", err)
	}
	if !errors.Is(err, ErrConcurrentWriters) {
		t.Fatalf("got %v, want ErrConcurrentWriters", err)
	}
}

func TestResolveWritePolicy(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"":         policyRefuse,
		"refuse":   policyRefuse,
		"warn":     policyWarn,
		"allow":    policyAllow,
		"worktree": policyWorktree,
		"REFUSE":   policyRefuse,
		"Allow":    policyRefuse, // case-sensitive, and a typo must not disarm
		"whatever": policyRefuse,
	}
	for in, want := range cases {
		if got := resolveWritePolicy(in); got != want {
			t.Errorf("resolveWritePolicy(%q) = %q, want %q", in, got, want)
		}
	}
}

func initTestGitRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	runGit("init")
	runGit("config", "user.email", "test@example.com")
	runGit("config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(repo, "shared.txt"), []byte("parent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit("add", "shared.txt")
	runGit("commit", "-m", "init")
	return repo
}

// Per-subagent git worktree isolation (#653): under policy "worktree",
// two write-capable background subagents run concurrently, each in its
// own detached git worktree, and writing to relative paths or running
// bash modifies only each subagent's own worktree without touching the
// parent repo or the sibling's worktree.
func TestSpawn_WorktreePolicyIsolatesConcurrentWriters(t *testing.T) {
	repo := initTestGitRepo(t)
	scope, err := permissions.NewPathScope(repo, "", nil)
	if err != nil {
		t.Fatalf("NewPathScope: %v", err)
	}
	gate := permissions.New(permissions.Options{
		Mode:  permissions.ModeYolo,
		Scope: scope,
	})
	reg, err := coretools.Build(config.DefaultConfig(), gate, repo, coretools.Default())
	if err != nil {
		t.Fatalf("tools.Build: %v", err)
	}
	built := reg.Tools

	// scriptedProvider issues a write_file("shared.txt", "<agentName>")
	// on turn 1 and completes on turn 2.
	prov := &worktreeWriterProvider{}
	mgr, err := NewManager(
		WithProvider(prov, "writer"),
		WithGate(gate),
		WithMaxConcurrent(8),
		WithAlertBuffer(16),
		WithCatalog(built),
		WithParallelWritePolicy("worktree"),
		WithWorktreeBaseDir(repo),
	)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	newTestParent(t, mgr)

	h1, err := mgr.Spawn(context.Background(), "", Spec{
		Name: "w1", Goal: "write w1", SystemPrompt: "writer", Tools: []string{"write_file"},
	})
	if err != nil {
		t.Fatalf("spawn w1: %v", err)
	}
	h2, err := mgr.Spawn(context.Background(), "", Spec{
		Name: "w2", Goal: "write w2", SystemPrompt: "writer", Tools: []string{"write_file"},
	})
	if err != nil {
		t.Fatalf("spawn w2 concurrently under worktree policy: %v", err)
	}

	if h1.WorktreeDir() == "" || h2.WorktreeDir() == "" {
		t.Fatalf("WorktreeDir empty: w1=%q w2=%q", h1.WorktreeDir(), h2.WorktreeDir())
	}
	if h1.WorktreeDir() == h2.WorktreeDir() {
		t.Fatalf("w1 and w2 share WorktreeDir %q", h1.WorktreeDir())
	}

	for _, h := range []*Handle{h1, h2} {
		select {
		case <-h.Done():
		case <-time.After(20 * time.Second):
			t.Fatalf("%s did not finish", h.Name)
		}
		if h.Err() != nil {
			t.Fatalf("%s failed: %v", h.Name, h.Err())
		}
	}

	// Each worktree has its own edit, while the parent repo is untouched.
	got1, err := os.ReadFile(filepath.Join(h1.WorktreeDir(), "shared.txt"))
	if err != nil || string(got1) != "from-write w1\n" {
		t.Errorf("w1 worktree shared.txt = %q (err=%v), want %q", got1, err, "from-write w1\n")
	}
	got2, err := os.ReadFile(filepath.Join(h2.WorktreeDir(), "shared.txt"))
	if err != nil || string(got2) != "from-write w2\n" {
		t.Errorf("w2 worktree shared.txt = %q (err=%v), want %q", got2, err, "from-write w2\n")
	}
	gotParent, err := os.ReadFile(filepath.Join(repo, "shared.txt"))
	if err != nil || string(gotParent) != "parent\n" {
		t.Errorf("parent shared.txt = %q (err=%v), want untouched %q", gotParent, err, "parent\n")
	}

	// Closing the manager removes the temporary worktrees.
	wt1, wt2 := h1.WorktreeDir(), h2.WorktreeDir()
	if err := mgr.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(wt1); !os.IsNotExist(err) {
		t.Errorf("w1 worktree %q still exists after Close: %v", wt1, err)
	}
	if _, err := os.Stat(wt2); !os.IsNotExist(err) {
		t.Errorf("w2 worktree %q still exists after Close: %v", wt2, err)
	}
}

type worktreeWriterProvider struct{}

func (*worktreeWriterProvider) Name() string { return "writer" }
func (*worktreeWriterProvider) Model(context.Context, string) (adkmodel.LLM, error) {
	return &worktreeWriterLLM{}, nil
}

type worktreeWriterLLM struct {
	called bool
}

func (*worktreeWriterLLM) Name() string { return "writer-llm" }
func (l *worktreeWriterLLM) GenerateContent(_ context.Context, req *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		if !l.called {
			l.called = true
			goal := "subagent"
			if req != nil && len(req.Contents) > 0 && len(req.Contents[0].Parts) > 0 {
				goal = strings.TrimSpace(req.Contents[0].Parts[0].Text)
			}
			yield(&adkmodel.LLMResponse{
				Content: &genai.Content{
					Role: genai.RoleModel,
					Parts: []*genai.Part{{
						FunctionCall: &genai.FunctionCall{
							ID:   "call-write",
							Name: "write_file",
							Args: map[string]any{
								"path":    "shared.txt",
								"content": "from-" + goal + "\n",
							},
						},
					}},
				},
				FinishReason: genai.FinishReasonStop,
				TurnComplete: true,
			}, nil)
			return
		}
		yield(&adkmodel.LLMResponse{
			Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "done"}}},
			FinishReason: genai.FinishReasonStop,
			TurnComplete: true,
		}, nil)
	}
}

// stubToolset is a tool.Toolset with no tools — the guard classifies
// the toolset dimension from the ToolsetTools snapshot, never by
// enumerating, so the contents are irrelevant and the snapshot is not.
type stubToolset struct{ name string }

func (s stubToolset) Name() string { return s.name }
func (s stubToolset) Tools(adkagent.ReadonlyContext) ([]tool.Tool, error) {
	return nil, errors.New("stubToolset: Tools must not be called — the guard reads the snapshot")
}

func TestSpawnWritesSharedFilesystem(t *testing.T) {
	t.Parallel()
	ts := []tool.Toolset{stubToolset{name: "some-server"}}
	skillInfo := wire.ToolInfo{Name: "load_skill", Source: wire.ToolSourceSkill}
	mcpInfo := wire.ToolInfo{Name: "k8s_apply", Source: "mcp", Server: "k8s"}

	cases := []struct {
		name string
		rs   resolvedSpawn
		want bool
	}{
		{"nothing granted", resolvedSpawn{}, false},
		{"read-only builtin", resolvedSpawn{tools: []tool.Tool{newNamedStubTool(t, "grep")}}, false},
		{"mutating builtin", resolvedSpawn{tools: []tool.Tool{newNamedStubTool(t, "write_file")}}, true},
		{"control-plane only", resolvedSpawn{tools: []tool.Tool{newNamedStubTool(t, "report_completed")}}, false},
		{"skills-only toolset", resolvedSpawn{toolsets: ts, toolsetTools: []wire.ToolInfo{skillInfo}}, false},
		{"mcp toolset", resolvedSpawn{toolsets: ts, toolsetTools: []wire.ToolInfo{mcpInfo}}, true},
		{"mcp among skills", resolvedSpawn{toolsets: ts, toolsetTools: []wire.ToolInfo{skillInfo, mcpInfo}}, true},
		// The snapshot is optional (SubagentTemplate.ToolsetTools), so
		// its absence means "unknown surface" — which resolves the
		// fail-safe way, not the convenient one.
		{"toolset with no snapshot", resolvedSpawn{toolsets: ts}, true},
		// A snapshot with no toolsets cannot describe anything runnable.
		{"snapshot with no toolsets", resolvedSpawn{toolsetTools: []wire.ToolInfo{mcpInfo}}, false},
	}
	for _, tc := range cases {
		if got := spawnWritesSharedFilesystem(tc.rs); got != tc.want {
			t.Errorf("%s: spawnWritesSharedFilesystem = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// An MCP server is free to name its tool after one of ours. The
// snapshot records the source, so a tool called `grep` coming from a
// server must NOT inherit our grep's read-only classification —
// otherwise any server could opt itself out of the guard by choosing a
// familiar name.
func TestSpawnWritesSharedFilesystem_McpDoesNotInheritBuiltinNames(t *testing.T) {
	t.Parallel()
	rs := resolvedSpawn{
		toolsets:     []tool.Toolset{stubToolset{name: "impostor"}},
		toolsetTools: []wire.ToolInfo{{Name: "grep", Source: "mcp", Server: "impostor"}},
	}
	if !spawnWritesSharedFilesystem(rs) {
		t.Error("an MCP tool named `grep` was classified read-only by name; source must decide first")
	}
}

// Skill tools go through the name table rather than a namespace
// blanket, so a skill tool nobody has classified is a writer. That is
// what makes TestSkillToolsDoNotWriteTheSharedTree in pkg/skills
// load-bearing rather than decorative.
func TestSpawnWritesSharedFilesystem_UnclassifiedSkillToolIsFailSafe(t *testing.T) {
	t.Parallel()
	rs := resolvedSpawn{
		toolsets:     []tool.Toolset{stubToolset{name: "skills"}},
		toolsetTools: []wire.ToolInfo{{Name: "mutate_skill", Source: wire.ToolSourceSkill}},
	}
	if !spawnWritesSharedFilesystem(rs) {
		t.Error("an unrecognised skill tool must classify as writing — a namespace blanket would have hidden it")
	}
}
