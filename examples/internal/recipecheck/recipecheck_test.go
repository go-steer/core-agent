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
	const skill = "See the kubectl-plugin docs. The operator is not bashful about it.\n" +
		"Paths like /usr/local/bin/kubectl.sha256 are not invocations either.\n"
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
	r := fixture(t, cfg, "", "run `kubectl get pods`\n")
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
	r := fixture(t, `{"version": 1, "tools": {"disable": ["bash"]}}`, "", "run `kubectl get pods`\n")

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
	if err := os.WriteFile(filepath.Join(external, "SKILL.md"), []byte("run `helm upgrade api`\n"), 0o644); err != nil {
		t.Fatalf("write skill: %v", err)
	}

	got := check(t, recipecheck.Recipe{Name: "fixture", Dir: agents}, recipecheck.Policy{})
	wantNames(t, got, "helm")
	if !strings.Contains(got[0].File, "upstream") {
		t.Errorf("finding file = %q, want a path under the content root", got[0].File)
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
