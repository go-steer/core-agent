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

// Package gkeplatformagent_test is the loader-and-content validation for
// the gke-platform-agent recipe (#704).
//
// This is the core-agent-native GKE platform agent: the second half of
// #704, whose first half froze examples/kube-platform-agent as a
// portability case study. The two recipes carry six skills with the same
// six names and they are not the same files. The frozen ones are an
// unmodified gke-labs/kube-agents snapshot that instructs `kubectl` and
// `gcloud` into a distroless image with no shell — 188 findings that
// examples/internal/recipecheck waives by policy under #674's
// accept-and-disclose ruling. These were rewritten against the toolset
// that actually exists here, and carry no waiver at all: the recipe is
// absent from allrecipes_test.go's `policies` map, which means it is
// checked with zero waivers and produces zero findings.
//
// These tests are pure loader + content assertions — no cloud
// credentials, no live cluster, no LLM — so they run as ordinary unit
// tests under `go test ./...`. What they cannot do is score the agent's
// answers; that is the live GKE drill (#970), and the honest division of
// labour is that CI proves the recipe is well-formed and only the drill
// proves it is any good.
package gkeplatformagent_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/go-steer/core-agent/v2/pkg/config"
	"github.com/go-steer/core-agent/v2/pkg/mcp"
	"github.com/go-steer/core-agent/v2/pkg/skills"
)

// `go test` runs with the package dir as the working directory, so these
// relative paths resolve.
const (
	agentsDir   = ".agents"
	clusterRoot = "cluster"

	// readOnlyEndpoint is the GKE MCP endpoint that makes propose-only true
	// at the transport: it does not serve the mutating verbs.
	readOnlyEndpoint = "https://container.googleapis.com/mcp/read-only"

	// readOnlyScope is the OAuth scope matching that endpoint. The
	// read-write sibling would hand back the authority the endpoint
	// withholds.
	readOnlyScope = "https://www.googleapis.com/auth/cloud-platform.read-only"
)

// clusterSkills are the six GKE domain skills the `cluster` subagent loads
// from its own content root (subagents[0].root, the #619/#621 shape). The
// parent loads none of them — it orchestrates and delegates.
var clusterSkills = []string{
	"gke-observability",
	"gke-reliability",
	"gke-storage",
	"gke-workload-scaling",
	"gke-workload-security",
	"gke-workload-troubleshooting",
}

// wantDisabled are the built-ins the recipe must remove from the catalog.
// Each is a way to act on the world despite a read-only MCP surface:
// `bash`, the three file-mutation tools, and the three filesystem-search
// tools — the last because there is nothing on disk to find, and the live
// failure they cause is not a mutation but a turn burned hunting for a
// GitOps repo that was never mounted.
var wantDisabled = []string{
	"bash", "write_file", "edit_file", "delete_file", "glob", "grep", "list_dir",
}

func loadConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Load(agentsDir)
	if err != nil {
		t.Fatalf("config.Load(%s): %v", agentsDir, err)
	}
	return cfg
}

// TestWatchdogIsEnforcedByTheRecipe closes #972.
//
// `--watchdog=enforce` had exactly one assertion in the tree and it was on
// examples/kube-platform-agent — the recipe frozen on 2026-08-13. The
// maintained GKE recipe relied on the implicit unattended default in
// cmd/core-agent/guardrails.go, which is correct behaviour and an untested
// guarantee: the tested guarantee was on the dead recipe.
//
// It has to be declared rather than inherited because the default is
// conditional on how the daemon is launched, and this recipe's whole
// autonomy story rests on the watchdog being a kill switch (#628, then
// #719 moving enforcement inside the turn). A recipe that silently
// downgraded to observe-only would still boot, still pass every other
// test here, and lose its only backstop against a runaway loop.
func TestWatchdogIsEnforcedByTheRecipe(t *testing.T) {
	for _, name := range []string{"config.json", "config.hub.json"} {
		t.Run(name, func(t *testing.T) {
			var raw struct {
				Safety struct {
					Watchdog string `json:"watchdog"`
				} `json:"safety"`
			}
			body, err := os.ReadFile(filepath.Join(agentsDir, name))
			if err != nil {
				t.Fatalf("read %s: %v", name, err)
			}
			if err := json.Unmarshal(body, &raw); err != nil {
				t.Fatalf("unmarshal %s: %v", name, err)
			}
			// Read off the file, not the loaded config: config.Load
			// resolves defaults, so a loaded value of "enforce" cannot
			// distinguish "the recipe declared it" from "the runtime
			// defaulted it", and the distinction is the whole point.
			if raw.Safety.Watchdog != "enforce" {
				t.Errorf("%s declares safety.watchdog = %q, want %q — "+
					"an unattended recipe must state the kill switch rather than inherit it",
					name, raw.Safety.Watchdog, "enforce")
			}
		})
	}
}

// TestConfigEnforcesProposeOnly asserts the local half of the propose-only
// guarantee: every tool that could act from inside the pod is out of the
// catalog, and the plan gate is armed. config.Load runs Validate, so a
// malformed config fails here rather than at daemon boot.
func TestConfigEnforcesProposeOnly(t *testing.T) {
	cfg := loadConfig(t)

	if cfg.Permissions.Mode != config.PermissionModeYolo {
		t.Errorf("permissions.mode = %q, want %q (a no-TTY daemon cannot answer a prompt)",
			cfg.Permissions.Mode, config.PermissionModeYolo)
	}
	if !cfg.Permissions.PlanGateArmed() {
		t.Errorf("plan gate not armed (plan_mode resolved to %q); plan-first is what forces "+
			"a written plan before the agent starts spending the turn",
			cfg.Permissions.ResolvedPlanMode())
	}

	disabled := make(map[string]bool, len(cfg.Tools.Disable))
	for _, n := range cfg.Tools.Disable {
		disabled[n] = true
	}
	for _, want := range wantDisabled {
		if !disabled[want] {
			t.Errorf("tools.disable is missing %q; the persona tells the agent this tool is "+
				"not registered, and a persona that describes a toolset it does not have is "+
				"the #644 failure (have %v)", want, cfg.Tools.Disable)
		}
	}
}

// TestMCPSurfaceIsReadOnly asserts the transport half, for both scopes
// that dial GKE: the parent's .agents/mcp.json and the `cluster`
// subagent's own cluster/mcp.json. The subagent's file is the one that
// matters most and the one easiest to forget — a rooted subagent loads its
// own MCP config that the parent never sees (#619/#621), so a regression
// to the full-access `/mcp` endpoint there would hand the diagnostician
// apply/patch/delete verbs while every parent-scoped assertion stayed
// green.
func TestMCPSurfaceIsReadOnly(t *testing.T) {
	for _, dir := range []string{agentsDir, clusterRoot} {
		t.Run(dir, func(t *testing.T) {
			servers, err := mcp.Load(dir)
			if err != nil {
				t.Fatalf("mcp.Load(%s): %v", dir, err)
			}
			if len(servers.Servers) != 1 {
				t.Fatalf("%s declares %d MCP servers, want exactly 1 (\"gke\"): %v",
					dir, len(servers.Servers), serverNames(servers))
			}
			gke, ok := servers.Servers["gke"]
			if !ok {
				t.Fatalf("%s: no \"gke\" MCP server; have %v", dir, serverNames(servers))
			}
			if gke.URL != readOnlyEndpoint {
				t.Errorf("%s: gke url = %q, want the read-only endpoint %q; the full-access "+
					"/mcp endpoint serves the mutating verbs this recipe promises the agent does not have",
					dir, gke.URL, readOnlyEndpoint)
			}
			if gke.Auth == nil || gke.Auth.GoogleOAuth == nil {
				t.Fatalf("%s: gke server has no google_oauth auth block", dir)
			}
			var sawReadOnly bool
			for _, s := range gke.Auth.GoogleOAuth.Scopes {
				if s == readOnlyScope {
					sawReadOnly = true
				}
				if s == "https://www.googleapis.com/auth/cloud-platform" {
					t.Errorf("%s: gke oauth requests the read-write scope %q; the recipe's IAM "+
						"story assumes the read-only one", dir, s)
				}
			}
			if !sawReadOnly {
				t.Errorf("%s: gke oauth scopes = %v, want %q", dir, gke.Auth.GoogleOAuth.Scopes, readOnlyScope)
			}
		})
	}
}

// TestClusterSubagentIsRootedAndScoped pins the delegation shape the
// persona describes. The parent's AGENTS.md tells the model to route
// single-cluster diagnosis to `cluster` with `wait: true` and to read the
// roster off the spawn_agent schema, so the spec's description is
// load-bearing model-facing text, not documentation.
func TestClusterSubagentIsRootedAndScoped(t *testing.T) {
	cfg := loadConfig(t)
	if len(cfg.Subagents) != 1 {
		t.Fatalf("recipe declares %d subagents, want exactly 1 (\"cluster\")", len(cfg.Subagents))
	}
	spec := cfg.Subagents[0]
	if spec.Name != "cluster" {
		t.Errorf("subagent name = %q, want \"cluster\"", spec.Name)
	}
	if spec.Root != "../cluster" {
		t.Errorf("subagents[\"cluster\"].root = %q, want \"../cluster\"; the six GKE skills and the "+
			"specialist persona live in their own content root, which is what keeps them off the parent",
			spec.Root)
	}
	if strings.TrimSpace(spec.Description) == "" {
		t.Error("subagents[\"cluster\"].description is empty; it is the roster entry the model reads " +
			"off the spawn_agent schema to decide what to route here")
	}

	// The allowlist is a ceiling, so anything absent from it is
	// unreachable for the subagent no matter what the parent holds. These
	// are the ones whose absence the specialist persona explicitly
	// promises ("no write path and no shell", "nothing on disk to find").
	granted := make(map[string]bool, len(spec.Tools))
	for _, n := range spec.Tools {
		granted[n] = true
	}
	if len(spec.Tools) == 0 {
		t.Fatal("subagents[\"cluster\"].tools is unset; a nil allowlist inherits the parent's whole " +
			"catalog, which is not the scoping cluster/AGENTS.md describes")
	}
	for _, banned := range append(append([]string{}, wantDisabled...), "fetch_url", "alert", "spawn_agent") {
		if granted[banned] {
			t.Errorf("subagents[\"cluster\"].tools grants %q; cluster/AGENTS.md tells the specialist "+
				"it does not have this", banned)
		}
	}
	if !granted["record_plan"] {
		t.Error("subagents[\"cluster\"].tools omits record_plan, which cluster/AGENTS.md opens with")
	}
}

// TestClusterSkillsLoadUnderTheSubagentRoot asserts the six skills are
// discoverable where the subagent looks for them, and nowhere else. A
// skills/ tree under .agents/ would load them onto the parent too —
// #617 did exactly that before #621 un-vendored it — which both doubles
// the parent's prompt and hands fleet-level reasoning a single-cluster
// playbook.
func TestClusterSkillsLoadUnderTheSubagentRoot(t *testing.T) {
	if entries, err := os.ReadDir(filepath.Join(agentsDir, "skills")); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				t.Errorf(".agents/skills/%s exists; the parent orchestrates and must carry no "+
					"single-cluster skills (they belong under cluster/skills/)", e.Name())
			}
		}
	}

	got, err := skills.Load(context.Background(), clusterRoot, nil)
	if err != nil {
		t.Fatalf("skills.Load(%s): %v", clusterRoot, err)
	}
	want := make(map[string]bool, len(clusterSkills))
	for _, n := range clusterSkills {
		want[n] = true
	}
	gotNames := make([]string, 0, len(got.Infos))
	for _, in := range got.Infos {
		gotNames = append(gotNames, in.Name)
		if !want[in.Name] {
			t.Errorf("cluster/skills/ has unexpected skill %q", in.Name)
		}
		delete(want, in.Name)
	}
	sort.Strings(gotNames)
	for n := range want {
		t.Errorf("cluster skill %q missing; discovered %v", n, gotNames)
	}
}

// TestHubConfigIsTheBaseConfigPlusAttach pins the one structural risk in
// shipping two configs.
//
// config.json is the single-session/local shape and config.hub.json adds
// the multi-session attach block the deployment runs with. They are
// otherwise byte-identical, and nothing makes them stay that way: the
// deployment loads only the hub file, so a fix applied to config.json —
// tightening tools.disable, lowering a cost ceiling, changing the
// subagent's tool list — would test green here and never reach the
// cluster. Diffing the parsed objects with `attach` removed is the
// cheapest way to make that divergence loud.
func TestHubConfigIsTheBaseConfigPlusAttach(t *testing.T) {
	load := func(name string) map[string]any {
		t.Helper()
		body, err := os.ReadFile(filepath.Join(agentsDir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		var m map[string]any
		if err := json.Unmarshal(body, &m); err != nil {
			t.Fatalf("unmarshal %s: %v", name, err)
		}
		return m
	}

	base, hub := load("config.json"), load("config.hub.json")
	if _, ok := hub["attach"]; !ok {
		t.Error("config.hub.json has no attach block; the hub config exists precisely to add one")
	}
	if _, ok := base["attach"]; ok {
		t.Error("config.json has an attach block; the split exists so the local config does not listen")
	}
	delete(hub, "attach")

	baseJSON, err := json.MarshalIndent(base, "", "  ")
	if err != nil {
		t.Fatalf("re-marshal config.json: %v", err)
	}
	hubJSON, err := json.MarshalIndent(hub, "", "  ")
	if err != nil {
		t.Fatalf("re-marshal config.hub.json: %v", err)
	}
	if string(baseJSON) != string(hubJSON) {
		t.Errorf("config.hub.json is not config.json + attach.\n"+
			"The deployment loads the hub file, so anything only in config.json never runs.\n"+
			"config.json:\n%s\n\nconfig.hub.json (attach removed):\n%s", baseJSON, hubJSON)
	}
}

// envRef matches an ${env:NAME} interpolation reference in content.
var envRef = regexp.MustCompile(`\$\{env:([A-Z0-9_]+)\}`)

// TestEnvManifestCoversEveryContentReference guards the silent failure
// documented at the top of .agents/env.yaml: when a referenced variable is
// missing from the manifest the loader passes the literal `${env:VAR}`
// text through, the model reads it as a value, and it lands inside an MCP
// argument the API rejects. Observed live on 2026-08-13 as 22 turns and
// $0.73 spent by an agent hunting for its own coordinates.
//
// The scope that matters is both content roots: a rooted subagent's
// AGENTS.md is interpolated by the same resolver, and cluster/AGENTS.md
// references all three coordinates.
func TestEnvManifestCoversEveryContentReference(t *testing.T) {
	var manifest struct {
		Env []struct {
			Name string `yaml:"name"`
		} `yaml:"env"`
	}
	body, err := os.ReadFile(filepath.Join(agentsDir, "env.yaml"))
	if err != nil {
		t.Fatalf("read env.yaml: %v", err)
	}
	if err := yaml.Unmarshal(body, &manifest); err != nil {
		t.Fatalf("unmarshal env.yaml: %v", err)
	}
	declared := make(map[string]bool, len(manifest.Env))
	for _, e := range manifest.Env {
		declared[e.Name] = true
	}

	for _, root := range []string{".", clusterRoot} {
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			// cluster/ is walked on its own pass; skipping it here keeps
			// each finding attributed to one root.
			if d.IsDir() {
				if root == "." && (path == clusterRoot || path == "deploy") {
					return filepath.SkipDir
				}
				return nil
			}
			if filepath.Ext(path) != ".md" {
				return nil
			}
			text, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			for _, m := range envRef.FindAllStringSubmatch(string(text), -1) {
				if !declared[m[1]] {
					t.Errorf("%s references ${env:%s} but .agents/env.yaml does not declare it; "+
						"the loader will pass the literal text through to the model", path, m[1])
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
}

// TestPersonaDoesNotShipAKanbanLifecycle is the #704 thesis as an
// assertion.
//
// The frozen recipe's base persona is a Hermes kanban worker whose
// identity is accept-a-task → loop-until-done → file-a-completion-report →
// exit, and every live failure that recipe produced traced back to it: a
// general question answered in incident-report costume, a loop with no
// reachable "done", and — the one this recipe exists to avoid — a
// confabulated "fully resolved" with zero tool calls (#639, and G2 of the
// #970 drill).
//
// So the native persona must not reacquire that shape. These are the
// phrases that carry it, checked across both content roots. This is a
// blunt instrument and it is deliberately blunt: it cannot tell whether
// the agent behaves well, only whether someone pasted the lifecycle back
// in. The behavioural question belongs to the drill.
func TestPersonaDoesNotShipAKanbanLifecycle(t *testing.T) {
	banned := []string{
		"kanban_complete",
		"kanban_block",
		"exit the session",
		"exiting session",
		"completion report",
		"all tasks complete",
	}
	// The persona legitimately names these constructs in order to REJECT
	// them ("There is no session to close and no completion report you are
	// obliged to file"). A passage that also carries a negation is the
	// recipe working as intended, not a regression.
	negations := []string{"no ", "not ", "never ", "don't", "do not", "cannot", "rather than"}

	// Scanning per PARAGRAPH, not per line: this content is hard-wrapped
	// prose, so "don't / \"exit the session.\"" puts the instruction and
	// its negation on different lines and a line-scoped check reports the
	// recipe's own refutation as a violation. Paragraphs are the smallest
	// unit that reliably holds both.
	for _, path := range personaFiles(t) {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		line := 1
		for _, para := range strings.Split(string(body), "\n\n") {
			lower := strings.ToLower(para)
			var negated bool
			for _, n := range negations {
				if strings.Contains(lower, n) {
					negated = true
					break
				}
			}
			if !negated {
				for _, phrase := range banned {
					if strings.Contains(lower, phrase) {
						t.Errorf("%s:%d instructs the kanban-worker lifecycle (%q) without negating it:\n%s",
							path, line, phrase, strings.TrimSpace(para))
					}
				}
			}
			line += strings.Count(para, "\n") + 2
		}
	}
}

// personaFiles returns every markdown file the runtime loads as
// instructions or skills, across both content roots.
func personaFiles(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, root := range []string{"AGENTS.md", clusterRoot} {
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() && filepath.Ext(path) == ".md" {
				out = append(out, path)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	if len(out) < 1+1+len(clusterSkills) {
		t.Fatalf("found only %d persona/skill files (%v); expected AGENTS.md, cluster/AGENTS.md "+
			"and the six cluster skills", len(out), out)
	}
	return out
}

// serverNames is for failure messages; map iteration order would otherwise
// make them non-deterministic.
func serverNames(s mcp.Servers) []string {
	out := make([]string, 0, len(s.Servers))
	for n := range s.Servers {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
