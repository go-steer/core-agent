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

package recipecheck_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-steer/core-agent/v2/examples/internal/recipecheck"
)

// fixture writes a synthetic recipe: a config.json, an optional mcp.json,
// and one skill file. Returns the Recipe pointing at it.
//
// Synthetic rather than golden-copied, because the point of each case is a
// single decidable difference (bash on vs off, server declared vs not) and
// a real recipe would drag in dozens of irrelevant ones.
func fixture(t *testing.T, cfgJSON, mcpJSON, skillMD string) recipecheck.Recipe {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, body string) {
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", path, err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	write("config.json", cfgJSON)
	if mcpJSON != "" {
		write("mcp.json", mcpJSON)
	}
	write("skills/demo/SKILL.md", skillMD)
	return recipecheck.Recipe{Name: "fixture", Dir: dir}
}

func check(t *testing.T, r recipecheck.Recipe, p recipecheck.Policy) []recipecheck.Finding {
	t.Helper()
	got, err := recipecheck.Check(r, p)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	return recipecheck.Unwaived(got)
}

// names flattens findings to the tool names they flagged, for comparison.
func names(fs []recipecheck.Finding) []string {
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		out = append(out, f.Name)
	}
	return out
}

func wantNames(t *testing.T, got []recipecheck.Finding, want ...string) {
	t.Helper()
	gotNames := names(got)
	if len(gotNames) != len(want) {
		t.Fatalf("got %d finding(s) %v, want %d %v\nfull: %v", len(gotNames), gotNames, len(want), want, got)
	}
	for i := range want {
		if gotNames[i] != want[i] {
			t.Errorf("finding[%d] = %q, want %q (%v)", i, gotNames[i], want[i], got[i])
		}
	}
}

const plainConfig = `{"version": 1, "tools": {"disable": []}}`

// TestFlagsBuiltinRemovedByDisableList is the #644 failure in miniature:
// content that calls a tool the recipe's own config took away. Nothing in
// the loader notices — the config is valid and the skill parses — so this
// check is the only thing between the recipe and a model calling a tool
// that isn't there.
func TestFlagsBuiltinRemovedByDisableList(t *testing.T) {
	const skill = "Persist the result with `write_file` to /tmp/out.md.\n"

	off := fixture(t, `{"version": 1, "tools": {"disable": ["write_file"]}}`, "", skill)
	got := check(t, off, recipecheck.Policy{})
	wantNames(t, got, "write_file")
	if !strings.Contains(got[0].Reason, "tools.disable") {
		t.Errorf("reason %q should point at the disable list, which is where the fix goes", got[0].Reason)
	}
	if got[0].Line != 1 {
		t.Errorf("finding line = %d, want 1", got[0].Line)
	}

	// The control: identical content, tool left enabled, no finding. Without
	// this the test would pass on a checker that flagged everything.
	on := fixture(t, plainConfig, "", skill)
	wantNames(t, check(t, on, recipecheck.Policy{}))
}

// TestFlagsShellCommandWithoutBash covers the literal pre-#644 content.
// `kubectl rollout undo` is a tool reference whose reachability is
// `bash`'s, and the distroless recipes disable `bash`.
func TestFlagsShellCommandWithoutBash(t *testing.T) {
	const skill = "Roll back:\n\n```\nkubectl rollout undo deployment/api\n```\n"

	off := fixture(t, `{"version": 1, "tools": {"disable": ["bash"]}}`, "", skill)
	wantNames(t, check(t, off, recipecheck.Policy{}), "kubectl")

	on := fixture(t, plainConfig, "", skill)
	wantNames(t, check(t, on, recipecheck.Policy{}))
}

// TestShellCommandMatchingIsWordBounded guards the checker's own false
// positives. A checker that flagged "kubectl-plugin" or "bashful" would be
// switched off, and then it protects nothing.
func TestShellCommandMatchingIsWordBounded(t *testing.T) {
	// The boundary rule has to hold in an executable position too, which is
	// the only place rule D looks now — so the near-miss spellings go inside
	// a fence rather than in prose, where the position gate alone would
	// have excused them.
	const skill = "The operator is not bashful about it. See the kubectl-plugin docs.\n\n" +
		"```\n" +
		"kubectl-plugin install foo\n" +
		"sha256sum /usr/local/bin/kubectl.sha256\n" +
		"```\n"
	r := fixture(t, `{"version": 1, "tools": {"disable": ["bash"]}}`, "", skill)
	wantNames(t, check(t, r, recipecheck.Policy{}))
}

// TestFlagsDoubleUnderscoreMCPName is #648's typo. pkg/mcp/namespace.go
// joins with ONE underscore, and an unmatched name is not a config error,
// so without this the poll is just silently refused at call time.
func TestFlagsDoubleUnderscoreMCPName(t *testing.T) {
	const mcpJSON = `{"version": 1, "servers": {"gke": {"transport": "http", "url": "https://example.invalid/mcp"}}}`
	r := fixture(t, plainConfig, mcpJSON, "Poll `gke__get_pod` until ready.\n")
	got := check(t, r, recipecheck.Policy{})
	wantNames(t, got, "gke__get_pod")
	if !strings.Contains(got[0].Reason, "SINGLE underscore") {
		t.Errorf("reason %q should name the actual rule", got[0].Reason)
	}
}

// TestToolPositionMustResolve covers the one place a name is unambiguously
// a tool rather than a config key, so the checker can be strict.
func TestToolPositionMustResolve(t *testing.T) {
	const mcpJSON = `{"version": 1, "servers": {"gke": {"transport": "http", "url": "https://example.invalid/mcp"}}}`
	const allowed = `{"version": 1, "tools": {"wait_and_verify": {"poll_allow": ["gke_get_pod"]}}}`

	t.Run("namespaced and allow-listed", func(t *testing.T) {
		r := fixture(t, allowed, mcpJSON, "wait_and_verify(tool: \"gke_get_pod\", ...)\n")
		wantNames(t, check(t, r, recipecheck.Policy{}))
	})

	t.Run("namespaced but not allow-listed", func(t *testing.T) {
		// Reachable, but wait_and_verify refuses to poll it: MCP tools never
		// self-classify read-only, so poll_allow is the operator's assertion.
		r := fixture(t, plainConfig, mcpJSON, "wait_and_verify(tool: \"gke_get_pod\", ...)\n")
		got := check(t, r, recipecheck.Policy{})
		wantNames(t, got, "gke_get_pod")
		if !strings.Contains(got[0].Reason, "poll_allow") {
			t.Errorf("reason %q should name poll_allow", got[0].Reason)
		}
	})

	t.Run("no such server", func(t *testing.T) {
		// The invented-tool case: nothing in the recipe can produce this name.
		r := fixture(t, plainConfig, "", "wait_and_verify(tool: \"gke_get_pod\", ...)\n")
		got := check(t, r, recipecheck.Policy{})
		wantNames(t, got, "gke_get_pod")
		if !strings.Contains(got[0].Reason, "not namespaced onto any declared MCP server") {
			t.Errorf("reason %q should say the namespace is undeclared", got[0].Reason)
		}
	})

	t.Run("built-in in tool position", func(t *testing.T) {
		r := fixture(t, plainConfig, "", "wait_and_verify(tool: \"stat\", ...)\n")
		wantNames(t, check(t, r, recipecheck.Policy{}))
	})
}

// TestFlagsPollAllowEntryThatMatchesNothing is the config-side twin: a
// poll_allow entry naming a server the recipe never declares is dead
// config that fails at call time, not at load time.
func TestFlagsPollAllowEntryThatMatchesNothing(t *testing.T) {
	cfg := `{"version": 1, "tools": {"wait_and_verify": {"poll_allow": ["ghost_get_pod"]}}}`
	r := fixture(t, cfg, "", "nothing to see\n")
	got := check(t, r, recipecheck.Policy{})
	wantNames(t, got, "ghost_get_pod")
	if got[0].File != "config.json" {
		t.Errorf("finding file = %q, want config.json", got[0].File)
	}
}

// TestFlagsPollAllowUnderDisabledWaitAndVerify covers the inverse of rule E:
// the entries all resolve, but the tool they authorize is not in the
// catalog. The config reads like a capability — five tools cleared for
// polling — and produces none, which is the shape of unenforced claim this
// whole check exists to remove.
func TestFlagsPollAllowUnderDisabledWaitAndVerify(t *testing.T) {
	const mcpJSON = `{"version": 1, "servers": {"gke": {"transport": "http", "url": "https://example.invalid/mcp"}}}`
	cfg := `{
	  "version": 1,
	  "tools": {"disable": ["wait_and_verify"], "wait_and_verify": {"poll_allow": ["gke_get_pod"]}}
	}`
	r := fixture(t, cfg, mcpJSON, "nothing to see\n")
	got := check(t, r, recipecheck.Policy{})
	wantNames(t, got, "wait_and_verify")
	if !strings.Contains(got[0].Reason, "tools.disable") {
		t.Errorf("reason %q should point at the disable list", got[0].Reason)
	}

	// Control: same poll_allow, tool left registered, no finding.
	ok := fixture(t, `{"version": 1, "tools": {"wait_and_verify": {"poll_allow": ["gke_get_pod"]}}}`, mcpJSON, "nothing to see\n")
	wantNames(t, check(t, ok, recipecheck.Policy{}))
}

// TestOverlappingContentRootScansOnce guards the waived/unwaived counts a
// reviewer reads. A content root that resolves onto the recipe's own tree
// would otherwise walk every file twice and report each finding twice.
func TestOverlappingContentRootScansOnce(t *testing.T) {
	cfg := `{"version": 1, "content_roots": ["."], "tools": {"disable": ["bash"]}}`
	r := fixture(t, cfg, "", "Run:\n\n```\nkubectl get pods\n```\n")
	wantNames(t, check(t, r, recipecheck.Policy{}), "kubectl")
}

// TestRegistrationPreconditionsAreRead asserts reachability comes from the
// real builder, not a copy of its rules. `fetch_url` and `alert` are on in
// tools.Default() but register only when their config precondition holds,
// so a recipe naming them with no allowlist / no targets is broken in a way
// the disable list cannot explain.
func TestRegistrationPreconditionsAreRead(t *testing.T) {
	r := fixture(t, plainConfig, "", "Escalate with `alert` and fetch context with `fetch_url`.\n")
	got := check(t, r, recipecheck.Policy{})
	// Same line, so the order is tools.BuiltinToolNames() order.
	wantNames(t, got, "fetch_url", "alert")
	for _, f := range got {
		if strings.Contains(f.Reason, "tools.disable") {
			t.Errorf("%q blamed the disable list, but it is not in it: %s", f.Name, f.Reason)
		}
	}

	// Satisfy both preconditions and the findings go away — which is what
	// makes this a reachability check and not a keyword blocklist.
	ok := fixture(t, `{
	  "version": 1,
	  "url_scope": {"allow": ["https://example.invalid/*"]},
	  "alerts": {"targets": [{"name": "oncall", "url": "https://example.invalid/hook", "template": "generic"}]}
	}`, "", "Escalate with `alert` and fetch context with `fetch_url`.\n")
	wantNames(t, check(t, ok, recipecheck.Policy{}))
}

// TestWaiverRequiresAReason keeps the escape hatch honest: a waiver is a
// statement that someone decided this is acceptable, so it has to carry
// who-said-what. A silent waiver is how the check stops meaning anything.
func TestWaiverRequiresAReason(t *testing.T) {
	r := fixture(t, `{"version": 1, "tools": {"disable": ["bash"]}}`, "", "Run:\n\n```\nkubectl get pods\n```\n")

	if _, err := recipecheck.Check(r, recipecheck.Policy{WaiveFileGlobs: []string{"skills/"}}); err == nil {
		t.Fatal("Check accepted a waiver with no reason; a waiver must be attributable")
	}

	withReason := recipecheck.Policy{
		WaiveFileGlobs: []string{"skills/"},
		WaiveReason:    "vendored upstream content, see #674",
	}
	all, err := recipecheck.Check(r, withReason)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(all) == 0 {
		t.Fatal("a waiver must not suppress the finding itself — callers report the waived count")
	}
	if got := recipecheck.Unwaived(all); len(got) != 0 {
		t.Errorf("waived finding still failing: %v", got)
	}
}

// TestScansContentRoots asserts the checker follows content_roots. A
// recipe can load nearly all of its content from outside the agents dir
// (kube-platform-agent does), and a walk that stopped at <dir>/skills
// would call it clean while missing everything that matters.
func TestScansContentRoots(t *testing.T) {
	dir := t.TempDir()
	agents := filepath.Join(dir, ".agents")
	external := filepath.Join(dir, "upstream", "skills", "vendored")
	for _, d := range []string{agents, external} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	cfg := `{"version": 1, "content_roots": ["../upstream"], "tools": {"disable": ["bash"]}}`
	if err := os.WriteFile(filepath.Join(agents, "config.json"), []byte(cfg), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(external, "SKILL.md"), []byte("Upgrade:\n\n```\nhelm upgrade api\n```\n"), 0o644); err != nil {
		t.Fatalf("write skill: %v", err)
	}

	got := check(t, recipecheck.Recipe{Name: "fixture", Dir: agents}, recipecheck.Policy{})
	wantNames(t, got, "helm")
	if !strings.Contains(got[0].File, "upstream") {
		t.Errorf("finding file = %q, want a path under the content root", got[0].File)
	}
}

// rootedFixture writes the two-tree shape a per-subagent content root
// produces (#619): an agents dir holding config.json (+ optional mcp.json
// and skill), and a sibling `cluster/` the config's subagent claims via
// root: "../cluster". Empty file bodies are skipped.
func rootedFixture(t *testing.T, cfgJSON, parentMCP, parentSkill, rootMCP, rootSkill string) recipecheck.Recipe {
	t.Helper()
	dir := t.TempDir()
	agents := filepath.Join(dir, ".agents")
	root := filepath.Join(dir, "cluster")
	write := func(base, rel, body string) {
		if body == "" {
			return
		}
		path := filepath.Join(base, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", path, err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", root, err)
	}
	write(agents, "config.json", cfgJSON)
	write(agents, "mcp.json", parentMCP)
	write(agents, "skills/parent/SKILL.md", parentSkill)
	write(root, "mcp.json", rootMCP)
	write(root, "skills/cluster/SKILL.md", rootSkill)
	return recipecheck.Recipe{Name: "fixture", Dir: agents}
}

// TestScansSubagentContentRoot is the #766 regression: kube-platform-agent
// moved its six GKE skills from the parent's .agents/skills/ into
// cluster/skills/ so the `cluster` subagent could own them (#621), and the
// checker — which walked only <dir>/skills plus content_roots — stopped
// seeing them entirely. Nothing failed; the waived count just dropped and
// the waiver text kept claiming to cover both trees.
//
// This is the same guard TestScansContentRoots provides for content_roots:
// a recipe that moves its skills under a subagent root must not silently
// stop being checked.
func TestScansSubagentContentRoot(t *testing.T) {
	cfg := `{
	  "version": 1,
	  "tools": {"disable": ["bash"]},
	  "subagents": [{"name": "cluster", "root": "../cluster"}]
	}`
	r := rootedFixture(t, cfg, "", "", "", "Scale it:\n\n```bash\nkubectl scale deployment/api --replicas=3\n```\n")

	got := check(t, r, recipecheck.Policy{})
	// The fence itself is a shell claim, then the command inside it.
	wantNames(t, got, "bash", "kubectl")
	for _, f := range got {
		if !strings.Contains(f.File, "cluster") {
			t.Errorf("finding file = %q, want a path under the subagent root", f.File)
		}
	}
}

// TestSubagentRootIsCheckedAgainstItsOwnCatalog is the other half of #766.
// Walking the tree is not enough: a rooted subagent narrows BOTH dimensions
// — built-ins by the subagents[].tools allowlist, MCP servers to the root's
// own mcp.json — so content judged against the parent's catalog would pass
// while being unreachable in the only place it is ever loaded.
func TestSubagentRootIsCheckedAgainstItsOwnCatalog(t *testing.T) {
	t.Run("tools allowlist narrows the built-ins", func(t *testing.T) {
		cfg := `{
		  "version": 1,
		  "subagents": [{"name": "cluster", "root": "../cluster", "tools": ["stat", "list_dir"]}]
		}`
		// Identical sentence in both trees. The parent registers write_file,
		// so its copy is fine; the subagent's allowlist omits it.
		const skill = "Persist the result with `write_file` to /tmp/out.md.\n"
		r := rootedFixture(t, cfg, "", skill, "", skill)

		got := check(t, r, recipecheck.Policy{})
		wantNames(t, got, "write_file")
		if !strings.Contains(got[0].File, "cluster") {
			t.Errorf("finding file = %q, want the subagent's copy, not the parent's", got[0].File)
		}
		if !strings.Contains(got[0].Reason, `subagents["cluster"].tools`) {
			t.Errorf("reason %q should name the allowlist that took the tool away", got[0].Reason)
		}
	})

	t.Run("inherits the parent registry when tools is omitted", func(t *testing.T) {
		// The control for the case above: nil tools inherits, so the same
		// sentence under the root is reachable and must NOT be flagged.
		cfg := `{"version": 1, "subagents": [{"name": "cluster", "root": "../cluster"}]}`
		r := rootedFixture(t, cfg, "", "", "", "Persist the result with `write_file` to /tmp/out.md.\n")
		wantNames(t, check(t, r, recipecheck.Policy{}))
	})

	t.Run("MCP namespaces come from the root's own mcp.json", func(t *testing.T) {
		const parentMCP = `{"version": 1, "servers": {
		  "gke": {"transport": "http", "url": "https://example.invalid/gke"},
		  "clusterio": {"transport": "http", "url": "https://example.invalid/clusterio"}}}`
		// The root declares only clusterio. `gke` exists for the parent and
		// is invisible here, which is exactly what a scoped subagent is for.
		const rootMCP = `{"version": 1, "servers": {
		  "clusterio": {"transport": "http", "url": "https://example.invalid/clusterio"}}}`
		cfg := `{
		  "version": 1,
		  "tools": {"wait_and_verify": {"poll_allow": ["gke_get_pod", "clusterio_get_pod"]}},
		  "subagents": [{"name": "cluster", "root": "../cluster"}]
		}`
		rootSkill := "wait_and_verify(tool: \"clusterio_get_pod\", ...)\n" +
			"wait_and_verify(tool: \"gke_get_pod\", ...)\n"
		r := rootedFixture(t, cfg, parentMCP, "", rootMCP, rootSkill)

		got := check(t, r, recipecheck.Policy{})
		wantNames(t, got, "gke_get_pod")
		if got[0].Line != 2 {
			t.Errorf("finding line = %d, want 2 (line 1 resolves through the root's own server)", got[0].Line)
		}
		if !strings.Contains(got[0].Reason, "not namespaced onto any declared MCP server") {
			t.Errorf("reason %q should say the namespace is undeclared in this scope", got[0].Reason)
		}
	})
}

// TestShellCommandOnlyCountsInAnExecutablePosition pins the narrowing #766
// asked for. Before it, a skill that told the truth about a no-shell runtime
// — "there is no kubectl here, use the gke MCP" — produced one finding per
// CLI it disclaimed, so the gate punished exactly the content it exists to
// encourage. An inline-code mention in prose is not a step the model is
// being told to run.
func TestShellCommandOnlyCountsInAnExecutablePosition(t *testing.T) {
	const bashOff = `{"version": 1, "tools": {"disable": ["bash"]}}`

	t.Run("prose disclaimer is not a finding", func(t *testing.T) {
		const skill = "Everything here is read-only, through the `gke` MCP.\n" +
			"There is **no** `kubectl` and **no** `gcloud`; do not reach for a shell.\n"
		wantNames(t, check(t, fixture(t, bashOff, "", skill), recipecheck.Policy{}))
	})

	t.Run("shell fence is a finding", func(t *testing.T) {
		const skill = "Check it:\n\n```sh\nkubectl get pods\n```\n"
		// The labeled fence is its own claim ("a shell session follows"),
		// then the command inside it.
		wantNames(t, check(t, fixture(t, bashOff, "", skill), recipecheck.Policy{}), "bash", "kubectl")
	})

	t.Run("prompt-prefixed line is a finding", func(t *testing.T) {
		const skill = "Check it:\n\n$ kubectl get pods\n"
		wantNames(t, check(t, fixture(t, bashOff, "", skill), recipecheck.Policy{}), "kubectl")
	})

	t.Run("non-shell fence is not a finding", func(t *testing.T) {
		// A CLI name inside a manifest or a captured log is quoted output,
		// not an instruction.
		const skill = "The annotation records who applied it:\n\n" +
			"```yaml\nmetadata:\n  annotations:\n    kubectl.kubernetes.io/last-applied: \"gcloud\"\n```\n"
		wantNames(t, check(t, fixture(t, bashOff, "", skill), recipecheck.Policy{}))
	})

	t.Run("the fence closes", func(t *testing.T) {
		// A tracker that never left the block would flag the prose after it,
		// which is the false positive this whole rule is meant to remove.
		const skill = "```bash\nls -la\n```\n\nAfterwards there is no `kubectl` to verify with.\n"
		wantNames(t, check(t, fixture(t, bashOff, "", skill), recipecheck.Policy{}), "bash")
	})

	// The fence arm alone is not enough, and this is the half an earlier cut
	// of #766 got wrong: real runbook content puts its commands in numbered
	// steps and table cells, not fences. The pre-#644 gke-troubleshoot tree
	// has 73 CLI references and not one is inside a shell fence, so a
	// fence-only rule scored 8 findings on it where the shipped gate scored
	// 81 — it would have retired the check while staying green.
	t.Run("an inline span with an argv is a finding", func(t *testing.T) {
		const skill = "| Recent bad deploy | `kubectl -n prod rollout undo deployment api` | 5m |\n"
		wantNames(t, check(t, fixture(t, bashOff, "", skill), recipecheck.Policy{}), "kubectl")
	})

	t.Run("a bare span with no argv is not", func(t *testing.T) {
		// The discriminator is the argv, not the backticks: an instruction
		// says what to run the command ON.
		const skill = "It must name one of `kubectl`, `gcloud`, or `helm`.\n"
		wantNames(t, check(t, fixture(t, bashOff, "", skill), recipecheck.Policy{}))
	})

	t.Run("a span that wraps onto the next line is a finding", func(t *testing.T) {
		// Runbook prose wraps at 72 columns and its commands are long; the
		// pre-#644 tree writes exactly this shape. A line-at-a-time matcher
		// loses it, which is an implementation gap wearing a rule's clothes.
		const skill = "Node-level Events (`kubectl get events --field-selector\n" +
			"involvedObject.kind=Node`) are the source of truth.\n"
		got := check(t, fixture(t, bashOff, "", skill), recipecheck.Policy{})
		wantNames(t, got, "kubectl")
		if got[0].Line != 1 {
			t.Errorf("finding line = %d, want 1 (the line the span opens on)", got[0].Line)
		}
	})

	t.Run("an unterminated backtick is not a span", func(t *testing.T) {
		// A blank line ends the paragraph, so the span cannot reach a closer
		// past it — and guessing one would make every stray backtick a
		// finding factory.
		const skill = "Budget: 4 turns (`kubectl get pods is not available\n\nhelm upgrade api`\n"
		wantNames(t, check(t, fixture(t, bashOff, "", skill), recipecheck.Policy{}))
	})
}

// TestFenceDelimiterEdges pins the three fence shapes the first cut of #766
// mis-parsed. Each one silently changed which rule fired: an unrecognised
// opener leaves the tracker outside the block, so the shell body is scanned
// as prose and its ```bash delimiter falls through to rule A — a `bash`
// finding with the wrong reason attached to the wrong line.
func TestFenceDelimiterEdges(t *testing.T) {
	const bashOff = `{"version": 1, "tools": {"disable": ["bash"]}}`

	// The names alone do not prove the fence was recognised: an unrecognised
	// opener still yields a `bash` finding, just from rule A and with a
	// reason about the disable list. The reason is what tells the two apart.
	fenceRecognised := func(t *testing.T, got []recipecheck.Finding) {
		t.Helper()
		for _, f := range got {
			if f.Name != "bash" {
				continue
			}
			if !strings.Contains(f.Reason, "opens a shell code fence") {
				t.Errorf("bash finding came from rule A, so the fence was never opened: %s", f.Reason)
			}
			return
		}
		t.Error("no bash finding for the fence delimiter")
	}

	t.Run("a fence nested in a list item", func(t *testing.T) {
		// Two live cases in kube-platform-agent's vendored trees indent the
		// opener past four spaces to sit inside a numbered step.
		const skill = "1. Roll it back:\n\n     ```bash\n     kubectl rollout undo deploy/api\n     ```\n"
		got := check(t, fixture(t, bashOff, "", skill), recipecheck.Policy{})
		wantNames(t, got, "bash", "kubectl")
		fenceRecognised(t, got)
	})

	t.Run("a fence inside a blockquote", func(t *testing.T) {
		const skill = "> Note:\n>\n> ```bash\n> kubectl get pods\n> ```\n"
		got := check(t, fixture(t, bashOff, "", skill), recipecheck.Policy{})
		wantNames(t, got, "bash", "kubectl")
		fenceRecognised(t, got)
	})

	t.Run("a shorter run does not close a longer fence", func(t *testing.T) {
		// CommonMark: the closer must be at least as long as the opener. A
		// tracker that closed on the inner ``` would leave the block, read
		// the rest as prose, and then flag the real closer as a second
		// fence — two bash findings where the document has one fence.
		const skill = "````bash\nkubectl apply -f - <<'EOF'\n```\nstill shell\nhelm upgrade api\n````\n"
		got := check(t, fixture(t, bashOff, "", skill), recipecheck.Policy{})
		wantNames(t, got, "bash", "kubectl", "helm")
		fenceRecognised(t, got)
	})
}

// TestOverlappingScopesDedupeOnTheFindingNotTheFile is the trap one level
// down from #766. Two scopes can legitimately claim the same file — a
// subagent root that also sits under content_roots, or two subagents sharing
// a root — and the cheap fix is to let the first scope claim it. That
// reintroduces the bug it was meant to solve: the parent's catalog is the
// widest one, so the file gets judged by the most permissive scope that
// touches it and the narrow subagent's real gap disappears. Dedupe belongs
// on (file, line, name), so a file is scanned once per DISTINCT catalog.
func TestOverlappingScopesDedupeOnTheFindingNotTheFile(t *testing.T) {
	cfg := `{
	  "version": 1,
	  "content_roots": ["../cluster"],
	  "subagents": [{"name": "cluster", "root": "../cluster", "tools": ["stat", "list_dir"]}]
	}`
	// The parent registers write_file and loads this file via content_roots;
	// the subagent's allowlist does not, and loads the same file via root.
	r := rootedFixture(t, cfg, "", "", "", "Persist the result with `write_file`.\n")

	got := check(t, r, recipecheck.Policy{})
	wantNames(t, got, "write_file")
	if !strings.Contains(got[0].Reason, `subagents["cluster"].tools`) {
		t.Errorf("reason %q blamed the wrong scope; the narrow catalog is the one that fails", got[0].Reason)
	}

	// And the same finding seen twice through one catalog stays one finding,
	// so the waived counts a reviewer reads do not double.
	same := `{"version": 1, "content_roots": ["../cluster"], "tools": {"disable": ["bash"]},
	  "subagents": [{"name": "cluster", "root": "../cluster"}]}`
	dup := rootedFixture(t, same, "", "", "", "Run:\n\n```\nkubectl get pods\n```\n")
	wantNames(t, check(t, dup, recipecheck.Policy{}), "kubectl")
}

// TestSubagentScopeRefusesWhatTheRuntimeRefuses keeps the mirror honest in
// both directions. cmd/core-agent's resolveSubagentTools exits with a config
// error on an unknown tool name and resolveSubagentToolsets on an unknown
// server; a checker that quietly dropped them would be claiming to model a
// boot sequence that never gets that far.
func TestSubagentScopeRefusesWhatTheRuntimeRefuses(t *testing.T) {
	t.Run("unknown tool in the allowlist", func(t *testing.T) {
		cfg := `{
		  "version": 1,
		  "subagents": [{"name": "cluster", "root": "../cluster", "tools": ["stat", "kubectl_apply"]}]
		}`
		r := rootedFixture(t, cfg, "", "", "", "Anything.\n")
		_, err := recipecheck.Check(r, recipecheck.Policy{})
		if err == nil {
			t.Fatal("Check accepted a tools allowlist naming a tool the registry has never held")
		}
		if !strings.Contains(err.Error(), "kubectl_apply") {
			t.Errorf("error %q should name the offending tool", err)
		}
	})

	t.Run("unknown server in the mcp allowlist", func(t *testing.T) {
		const rootMCP = `{"version": 1, "servers": {
		  "clusterio": {"transport": "http", "url": "https://example.invalid/clusterio"}}}`
		cfg := `{
		  "version": 1,
		  "subagents": [{"name": "cluster", "root": "../cluster", "mcp": ["gke"]}]
		}`
		r := rootedFixture(t, cfg, "", "", rootMCP, "Anything.\n")
		_, err := recipecheck.Check(r, recipecheck.Policy{})
		if err == nil {
			t.Fatal("Check accepted an mcp allowlist naming a server the root's mcp.json does not declare")
		}
		if !strings.Contains(err.Error(), "gke") {
			t.Errorf("error %q should name the offending server", err)
		}
	})

	t.Run("a missing root is a config error", func(t *testing.T) {
		cfg := `{"version": 1, "subagents": [{"name": "cluster", "root": "../nope"}]}`
		r := rootedFixture(t, cfg, "", "", "", "Anything.\n")
		if _, err := recipecheck.Check(r, recipecheck.Policy{}); err == nil {
			t.Fatal("Check accepted a subagent root that does not exist")
		}
	})
}

// TestWaiverFloorCatchesATreeGoingDark is the guard #766 itself needed. A
// waiver glob that stops matching anything — because the tree moved, or
// because nothing walks it any more — looks identical to a clean tree: no
// unwaived findings, test green, waived count quietly smaller. A per-glob
// floor is the difference between "we accepted these findings" and "we
// stopped producing them".
func TestWaiverFloorCatchesATreeGoingDark(t *testing.T) {
	r := fixture(t, `{"version": 1, "tools": {"disable": ["bash"]}}`, "",
		"Run:\n\n```\nkubectl get pods\n```\n")
	p := recipecheck.Policy{
		WaiveFileGlobs:   []string{"skills/"},
		WaiveReason:      "vendored upstream content, see #674",
		WaiveMinFindings: map[string]int{"skills/": 1},
	}
	if _, err := recipecheck.Check(r, p); err != nil {
		t.Fatalf("Check: %v", err) // one finding, floor of one
	}

	p.WaiveMinFindings = map[string]int{"skills/": 2}
	_, err := recipecheck.Check(r, p)
	if err == nil {
		t.Fatal("Check accepted a waived tree producing fewer findings than its floor")
	}
	if !strings.Contains(err.Error(), "skills/") {
		t.Errorf("error %q should name the glob that fell short", err)
	}
}

// TestDiscoverFindsRealRecipes pins the discovery rule against the actual
// tree. It is intentionally a lower bound plus a spot check rather than an
// exact list: a new recipe should not have to edit this test, but a walk
// that quietly stopped finding recipes must fail.
func TestDiscoverFindsRealRecipes(t *testing.T) {
	got, err := recipecheck.Discover(examplesDir)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(got) < 5 {
		t.Fatalf("Discover found only %d config roots; the walk is broken: %v", len(got), got)
	}
	want := map[string]bool{
		"gke-troubleshoot-agent/deploy/base/config": false,
		"kube-platform-agent/.agents":               false,
		"plan-first/.agents":                        false,
	}
	for _, r := range got {
		if _, ok := want[r.Name]; ok {
			want[r.Name] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("Discover missed %s", name)
		}
	}
}
