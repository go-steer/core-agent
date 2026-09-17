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
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
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
			// The URL alone is an operator's private knowledge. `read_only`
			// is the same fact stated where the runtime can act on it: it
			// classifies every tool the server exposes, which is what lets
			// `wait_and_verify` poll a gke read without a hand-maintained
			// `poll_allow` list drifting out of date behind it (#693/#971).
			// Declaring the flag while pointing at the read-write /mcp
			// endpoint would be a lie the runtime believes, so this only
			// makes sense guarded by the URL assertion above.
			if !gke.ReadOnly {
				t.Errorf("%s: gke server does not declare read_only:true; without it every tool "+
					"it exposes classifies as mutating and each pollable one has to be named by "+
					"hand in tools.wait_and_verify.poll_allow", dir)
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

// TestIAMCoversTheLogsToolTheSkillsCall is the counterweight to
// TestMCPSurfaceIsReadOnly. That test proves the recipe cannot do more
// than it says; this one proves it can do what it says.
//
// The gke-observability skill sends the agent to gke_get_k8s_logs, which
// the read-only endpoint serves. roles/container.viewer does not carry
// container.pods.getLogs and no predefined read-only container role does,
// so under plain viewer that one tool 403s while every other read
// succeeds — the agent investigates a crash without ever reading the
// crash message and reports what it could reach. Every scenario of the
// 2026-09-09 drill sitting hit it.
//
// Both halves are asserted, in both directions: the skill that needs the
// permission, and the script that grants it. Dropping either alone is the
// state that looks fine and is not.
func TestIAMCoversTheLogsToolTheSkillsCall(t *testing.T) {
	grantIAM, err := os.ReadFile(filepath.Join("scripts", "grant-iam.sh"))
	if err != nil {
		t.Fatalf("read grant-iam.sh: %v", err)
	}
	grantsLogs := strings.Contains(string(grantIAM), "container.pods.getLogs")

	var callsLogs bool
	root := filepath.Join(clusterRoot, "skills")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read %s: %v", root, err)
	}
	for _, e := range entries {
		b, readErr := os.ReadFile(filepath.Join(root, e.Name(), "SKILL.md"))
		if readErr != nil {
			continue
		}
		if strings.Contains(string(b), "gke_get_k8s_logs") {
			callsLogs = true
		}
	}

	switch {
	case callsLogs && !grantsLogs:
		t.Error("a cluster skill calls gke_get_k8s_logs but grant-iam.sh never grants " +
			"container.pods.getLogs; the daemon will 403 on that tool alone")
	case grantsLogs && !callsLogs:
		t.Error("grant-iam.sh grants container.pods.getLogs but no cluster skill calls " +
			"gke_get_k8s_logs; drop the permission or restore the content that needs it")
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

const gatedApplyDir = "deploy/components/gated-apply"

// gatedApplyObj is the slice of a Kubernetes object these tests read. The
// recipe has no client-go dependency and does not want one to check four
// fields on two committed manifests.
type gatedApplyObj struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name      string `yaml:"name"`
		Namespace string `yaml:"namespace"`
	} `yaml:"metadata"`
	Subjects []struct {
		Kind     string `yaml:"kind"`
		Name     string `yaml:"name"`
		APIGroup string `yaml:"apiGroup"`
	} `yaml:"subjects"`
	RoleRef struct {
		Kind string `yaml:"kind"`
		Name string `yaml:"name"`
	} `yaml:"roleRef"`
	Rules []struct {
		APIGroups []string `yaml:"apiGroups"`
		Resources []string `yaml:"resources"`
		Verbs     []string `yaml:"verbs"`
	} `yaml:"rules"`
}

func gatedApplyDecode(t *testing.T, path string) []gatedApplyObj {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()

	var out []gatedApplyObj
	dec := yaml.NewDecoder(f)
	for {
		var o gatedApplyObj
		err := dec.Decode(&o)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		if o.Kind != "" {
			out = append(out, o)
		}
	}
	return out
}

// subject username: serviceAccount:<project>.svc.id.goog[<namespace>/<sa>]
var gatedApplySubjectRE = regexp.MustCompile(
	`^serviceAccount:([^.]+)\.svc\.id\.goog\[([^/\]]+)/([^\]]+)\]$`)

// TestGatedApplySubjectMatchesDaemonNamespace pins the most dangerous string
// in this recipe to the manifests that decide whether it is true.
//
// The RoleBinding names the daemon by its GKE Workload Identity *username*,
// whose bracket is `<the namespace the daemon runs in>/<its ServiceAccount>`.
// Both halves are fixed by deploy/base — the namespace is a literal in
// 00-namespace.yaml and 10-serviceaccount-daemon.yaml, not a coordinate any
// script substitutes — so the bracket here is a literal that must agree with
// base, and nothing at runtime will tell you when it stops agreeing. A
// subject that does not match is the one failure in this component that is
// completely silent: RBAC declines to match, nothing errors, no event is
// emitted, and the only symptom is an agent that cannot do the thing it was
// deployed to do. Rename the namespace in base and this test goes red, which
// is the only reason the rename would ever be noticed.
//
// The same test pins the three shape choices with the same property. `kind:
// User` is what a Workload Identity username is; `kind: ServiceAccount` names
// the *projected-token* identity instead, which is the watcher's path, not
// the daemon's. `roleRef.kind: Role` keeps the grant namespaced — flipping it
// to ClusterRole widens it to every namespace in the cluster with no other
// visible change. And `roleRef.name` must equal `metadata.name`, because
// teardown deletes that one name: if they diverge the binding points at a
// Role that does not exist, which fails exactly as silently as a bad subject.
func TestGatedApplySubjectMatchesDaemonNamespace(t *testing.T) {
	var daemonNS, daemonSA string
	for _, o := range gatedApplyDecode(t, "deploy/base/10-serviceaccount-daemon.yaml") {
		if o.Kind == "ServiceAccount" {
			daemonNS, daemonSA = o.Metadata.Namespace, o.Metadata.Name
		}
	}
	if daemonNS == "" || daemonSA == "" {
		t.Fatal("no ServiceAccount with an explicit namespace in " +
			"deploy/base/10-serviceaccount-daemon.yaml; the gated-apply subject " +
			"has nothing to be checked against")
	}

	objs := gatedApplyDecode(t, filepath.Join(gatedApplyDir, "rolebinding.yaml"))
	if len(objs) != 1 || objs[0].Kind != "RoleBinding" {
		t.Fatalf("want exactly one RoleBinding in rolebinding.yaml, got %d objects", len(objs))
	}
	rb := objs[0]

	if len(rb.Subjects) != 1 {
		t.Fatalf("want exactly one subject, got %d — an extra subject is an extra "+
			"identity holding deployment-patch in the target namespace", len(rb.Subjects))
	}
	if rb.Subjects[0].Kind != "User" {
		t.Errorf("subject kind is %q, want User: a Workload Identity username is a User. "+
			"kind: ServiceAccount names the projected-token identity instead, binds "+
			"without error, and never matches the daemon's calls", rb.Subjects[0].Kind)
	}

	m := gatedApplySubjectRE.FindStringSubmatch(rb.Subjects[0].Name)
	if m == nil {
		t.Fatalf("subject %q is not a Workload Identity username of the form "+
			"serviceAccount:<project>.svc.id.goog[<ns>/<sa>]", rb.Subjects[0].Name)
	}
	if got, want := m[2]+"/"+m[3], daemonNS+"/"+daemonSA; got != want {
		t.Errorf("the subject binds [%s] but the daemon runs as [%s] per "+
			"deploy/base/10-serviceaccount-daemon.yaml.\n"+
			"RBAC will not match, and nothing — not an error, not an event, not a "+
			"log line — will say so.", got, want)
	}

	if rb.RoleRef.Kind != "Role" {
		t.Errorf("roleRef.kind is %q, want Role: a ClusterRole grants the same verbs "+
			"in every namespace, which is not a boundary", rb.RoleRef.Kind)
	}
	if rb.RoleRef.Name != rb.Metadata.Name {
		t.Errorf("roleRef.name %q != metadata.name %q; teardown deletes one name, and "+
			"a binding pointing at a Role that does not exist grants nothing, silently",
			rb.RoleRef.Name, rb.Metadata.Name)
	}
}

// TestGatedApplyProjectPlaceholderIsGateVisible keeps the runtime backstop
// real.
//
// set-up-demo.sh substitutes PROJECT_ID into the subject, and then — as a
// separate, later step — greps the *rendered* manifest for placeholder
// strings and refuses to apply if any survived. That second check is what
// makes a missing substitution loud instead of a cluster that boots healthy
// and 403s on its first call. It only works if the placeholder committed here
// is one the gate actually looks for, so this test reads the gate's own
// pattern out of the script rather than restating it.
//
// TARGET_NS is deliberately not covered: `your-target-namespace` is not in
// the gate's pattern, and a Role in a namespace by that name is inert rather
// than dangerous. The subject is the one worth a backstop.
func TestGatedApplyProjectPlaceholderIsGateVisible(t *testing.T) {
	setup, err := os.ReadFile(filepath.Join("scripts", "set-up-demo.sh"))
	if err != nil {
		t.Fatalf("read set-up-demo.sh: %v", err)
	}
	// The gate line, not a comment that mentions one: anchored at the start
	// of a line and keyed on the grep invocation.
	gateRE := regexp.MustCompile(`(?m)^\s*\| grep -nE '([^']+)'`)
	gm := gateRE.FindSubmatch(setup)
	if gm == nil {
		t.Fatal("set-up-demo.sh no longer greps the rendered manifest for surviving " +
			"placeholders; a missing substitution now reaches the cluster")
	}
	gate := regexp.MustCompile(string(gm[1]))

	objs := gatedApplyDecode(t, filepath.Join(gatedApplyDir, "rolebinding.yaml"))
	if len(objs) != 1 {
		t.Fatalf("want exactly one object in rolebinding.yaml, got %d", len(objs))
	}
	m := gatedApplySubjectRE.FindStringSubmatch(objs[0].Subjects[0].Name)
	if m == nil {
		t.Fatalf("subject %q is not a Workload Identity username", objs[0].Subjects[0].Name)
	}
	if !gate.MatchString(m[1]) {
		t.Errorf("the committed project id %q is not matched by set-up-demo.sh's "+
			"placeholder gate /%s/.\nIf the substitution is ever skipped, the gate "+
			"waves the manifest through and the deployment 403s on its first model "+
			"call instead of failing here.\n"+
			"(If you just ran set-up-demo.sh, it rewrote this tracked file — revert it.)",
			m[1], gm[1])
	}
}

// TestGatedApplyRoleGrantsOnlyDeploymentPatch pins the grant itself.
//
// This Role is the entire security argument for the unattended leg (#1105):
// with no human approving anything, what the API server refuses is the only
// boundary left. verify-gated-apply.sh probes the same three axes against a
// live cluster, but it needs a cluster and a real token — this is the copy
// that runs in CI.
//
// The widening that matters is `resources`. A Role broadened to
// apiGroups:["*"] resources:["*"] still passes every verb and namespace
// assertion while gaining the ability to patch Secrets, ServiceAccounts and
// RoleBindings in the target namespace.
//
// The file is decoded as a document STREAM, not with yaml.Unmarshal. A single
// Unmarshal silently keeps the first document and drops the rest, so a second
// document appended to this file — say a ClusterRole with
// apiGroups:["*"] verbs:["*"] — would render, apply, and survive teardown
// (cluster-scoped, differently named) while this test went on reporting one
// tightly-scoped rule.
func TestGatedApplyRoleGrantsOnlyDeploymentPatch(t *testing.T) {
	objs := gatedApplyDecode(t, filepath.Join(gatedApplyDir, "role.yaml"))
	if len(objs) != 1 {
		t.Fatalf("role.yaml holds %d objects, want exactly 1; anything smuggled in "+
			"beside the Role is applied with it and is not deleted by teardown", len(objs))
	}
	if objs[0].Kind != "Role" {
		t.Fatalf("role.yaml holds a %s, want Role", objs[0].Kind)
	}
	role := objs[0]

	if len(role.Rules) != 1 {
		t.Fatalf("gated-apply Role has %d rules, want exactly 1", len(role.Rules))
	}
	r := role.Rules[0]
	for _, tc := range []struct {
		field string
		got   []string
		want  string
	}{
		{"apiGroups", r.APIGroups, "apps"},
		{"resources", r.Resources, "deployments"},
		{"verbs", r.Verbs, "patch"},
	} {
		if len(tc.got) != 1 || tc.got[0] != tc.want {
			t.Errorf("gated-apply Role %s = %v, want exactly [%s]; the unattended "+
				"leg has no boundary other than this rule", tc.field, tc.got, tc.want)
		}
	}
}

// TestGatedApplyComponentShipsOnlyItsTwoObjects closes the other door into the
// rendered output.
//
// TestGatedApplyRoleGrantsOnlyDeploymentPatch and
// TestGatedApplySubjectMatchesDaemonNamespace both assert on two files reached
// by path. Neither reads the kustomization, so a third filename in `resources`
// — or a generator — renders an arbitrary object into every overlay that
// composes this component, with nothing checking it and nothing in teardown
// deleting it. The component creates two objects; this says so in the one
// place that decides.
//
// It also *modifies* the daemon, via exactly two patches, each named here.
// That is a deliberate widening of this guard rather than a hole in it: the
// `-c` swap and the plans remount are inseparable from the RBAC (see the
// component README), so they ship together, and the two filenames are pinned
// so a third patch is still a test failure. What each one does to the
// Deployment is TestGatedApplyPatchesGuardTheirIndices' job.
func TestGatedApplyComponentShipsOnlyItsTwoObjects(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(gatedApplyDir, "kustomization.yaml"))
	if err != nil {
		t.Fatalf("read kustomization.yaml: %v", err)
	}
	var comp struct {
		Kind       string   `yaml:"kind"`
		Resources  []string `yaml:"resources"`
		Components []string `yaml:"components"`
		Patches    []struct {
			Path   string `yaml:"path"`
			Patch  string `yaml:"patch"`
			Target struct {
				Group   string `yaml:"group"`
				Version string `yaml:"version"`
				Kind    string `yaml:"kind"`
				Name    string `yaml:"name"`
			} `yaml:"target"`
		} `yaml:"patches"`
		PatchesStrategic   []yaml.Node `yaml:"patchesStrategicMerge"`
		ConfigMapGenerator []yaml.Node `yaml:"configMapGenerator"`
		SecretGenerator    []yaml.Node `yaml:"secretGenerator"`
		Namespace          string      `yaml:"namespace"`
	}
	if err := yaml.Unmarshal(b, &comp); err != nil {
		t.Fatalf("parse kustomization.yaml: %v", err)
	}

	if comp.Kind != "Component" {
		t.Errorf("kind = %q, want Component: a Kustomization here is composed as a "+
			"base, not an opt-in add-on, and would apply unconditionally", comp.Kind)
	}
	want := []string{"role.yaml", "rolebinding.yaml"}
	if !slices.Equal(comp.Resources, want) {
		t.Errorf("resources = %v, want exactly %v; every other guard in this file "+
			"reaches those two by path and would not see a third", comp.Resources, want)
	}
	for _, f := range []struct {
		name string
		n    int
	}{
		{"components", len(comp.Components)},
		{"patchesStrategicMerge", len(comp.PatchesStrategic)},
		{"configMapGenerator", len(comp.ConfigMapGenerator)},
		{"secretGenerator", len(comp.SecretGenerator)},
	} {
		if f.n != 0 {
			t.Errorf("%s is set: this component may only contribute the two RBAC "+
				"objects and the two daemon patches it is reviewed as", f.name)
		}
	}

	// The patch list, by filename and target. A patch here lands on every
	// overlay that composes the component, and — because a component's
	// patches run AFTER the composing overlay's — it silently outranks
	// anything overlays/example says about the same field.
	wantPatches := []string{"patch-agent-config.yaml", "patch-plans-mount.yaml"}
	gotPatches := make([]string, 0, len(comp.Patches))
	for _, p := range comp.Patches {
		if p.Patch != "" {
			t.Errorf("patch %q is inline: keep these in files, so the JSON6902 ops "+
				"stay reviewable next to their rationale", p.Path)
		}
		gotPatches = append(gotPatches, p.Path)
		tgt := p.Target
		if tgt.Group != "apps" || tgt.Version != "v1" || tgt.Kind != "Deployment" || tgt.Name != "core-agent" {
			t.Errorf("patch %q targets %+v, want the apps/v1 Deployment core-agent; "+
				"an unnamed or wider target would also hit the watcher", p.Path, tgt)
		}
	}
	if !slices.Equal(gotPatches, wantPatches) {
		t.Errorf("patches = %v, want exactly %v", gotPatches, wantPatches)
	}
	if comp.Namespace != "" {
		t.Errorf("namespace = %q: a namespace field here would relocate both objects "+
			"away from the TARGET_NS their metadata pins them to", comp.Namespace)
	}
}

// TestGatedApplyOverlaysDoNotMoveTheDaemon guards the layer above the subject.
//
// The gated-apply RoleBinding names the daemon by a Workload Identity username
// whose bracket contains the daemon's namespace, and every check of that
// bracket — this file's, and set-up-demo.sh's — is only as good as the
// assumption that the daemon lands where deploy/base puts it. A single
// `namespace:` field in an overlay breaks that assumption without touching
// base: kustomize relocates every object base leaves unset, so the daemon
// moves, the subject does not, and RBAC silently stops matching. It relocates
// this component's Role and RoleBinding out of TARGET_NS at the same time.
//
// set-up-demo.sh catches this on the scripted path by checking the bracket
// against the rendered manifest. This is the CI copy, which has no kustomize:
// it forbids the field rather than evaluating its effect.
func TestGatedApplyOverlaysDoNotMoveTheDaemon(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("deploy", "overlays", "*", "kustomization.yaml"))
	if err != nil {
		t.Fatalf("glob overlays: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no overlay kustomizations found; this guard is checking nothing")
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		var k struct {
			Namespace string `yaml:"namespace"`
		}
		if err := yaml.Unmarshal(b, &k); err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		if k.Namespace != "" {
			t.Errorf("%s sets namespace: %q. That moves the daemon's ServiceAccount, "+
				"which the gated-apply subject names by namespace and cannot follow, "+
				"and moves the gated-apply Role out of TARGET_NS. Both fail silently.",
				f, k.Namespace)
		}
	}
}

// shellCode blanks out everything in a bash script that the shell will not
// execute as a command — comments, single- and double-quoted spans, and
// heredoc bodies — replacing their characters with spaces so that line
// numbers survive.
//
// It exists because scanning a script for a *call* with a regexp is a
// false-positive machine. A usage block is the natural place to write
//
//	cat <<'USAGE'
//	Preflight, run before anything else:
//	require_demo_ns_matches_base || exit 1
//	USAGE
//
// and a whole-body match cannot tell that from the call itself — a script can
// ship with the guard existing only as help text and satisfy the test. That
// is not hypothetical shape: `-h` usage output and `cat <<POD` /
// `read -r -d ” PROBE_SCRIPT <<EOF` heredocs already exist in these scripts.
// Anchoring to column zero does not fix it, because a heredoc body is
// unindented far more often than not.
//
// This is not a bash parser and does not try to be one. It needs to be sound
// in one direction only: it may blank something that was code (making a check
// stricter — the caller then reports a missing call that is present, which is
// loud), but it must not leave data looking like code.
//
// That is a property, not a promise, and it is asserted rather than argued:
// every case in TestShellCodeSelfTest is also RUN through bash, and keeping a
// line the shell does not execute fails the test. It was written as a promise
// first, and `cat <<""` — a heredoc that ends at the first empty line, whose
// delimiter word is empty — broke it within the hour.
//
// The rules it does model: `#` opens a comment only at the start of a word, so
// an apostrophe inside a comment cannot desynchronise the quote state; a
// backslash escapes the next character outside quotes and inside double quotes
// but not inside single quotes, which is bash's own rule; `$'…'` is its own
// state, because there the backslash escapes the closing quote too; and `<<`
// is a heredoc operator except as the tail of `<<<` or inside `$((…))`.
func shellCode(body string) string {
	var (
		out      strings.Builder
		inSingle bool
		inDouble bool
		inAnsiC  bool // $'…', where a backslash escapes the closing quote
		// inHeredoc is a separate flag rather than `heredoc != ""` because
		// the empty delimiter is a real one: `cat <<""` is a heredoc that
		// ends at the first empty line, and treating "" as "not in a
		// heredoc" left its body scanned as code — the one way this scanner
		// was ever found presenting data as code.
		inHeredoc   bool
		heredoc     string // delimiter we are inside
		heredocTabs bool   // <<- form: leading tabs are stripped before comparing
		arith       int    // depth inside $((…)), where `<<` is a shift
		pending     []struct {
			delim string
			tabs  bool
		}
	)
	blankLine := func(line string) {
		for range line {
			out.WriteByte(' ')
		}
	}

	for _, line := range strings.Split(body, "\n") {
		if inHeredoc {
			candidate := line
			if heredocTabs {
				candidate = strings.TrimLeft(candidate, "\t")
			}
			if candidate == heredoc {
				inHeredoc = false
			}
			blankLine(line)
			out.WriteByte('\n')
			continue
		}

		// An unclosed $(( is malformed rather than continued, and resetting
		// resumes recognising heredocs, which is the strict direction.
		arith = 0
		var lineOut strings.Builder
		for i := 0; i < len(line); i++ {
			c := line[i]
			switch {
			case inSingle:
				lineOut.WriteByte(' ')
				if c == '\'' {
					inSingle = false
				}
			case inDouble:
				lineOut.WriteByte(' ')
				if c == '\\' && i+1 < len(line) {
					i++
					lineOut.WriteByte(' ')
				} else if c == '"' {
					inDouble = false
				}
			case inAnsiC:
				lineOut.WriteByte(' ')
				if c == '\\' && i+1 < len(line) {
					i++
					lineOut.WriteByte(' ')
				} else if c == '\'' {
					inAnsiC = false
				}
			case c == '\\' && i+1 < len(line):
				lineOut.WriteString("  ")
				i++
			case c == '$' && i+1 < len(line) && line[i+1] == '\'':
				// $'…' is not a single-quoted string: a backslash escapes
				// the next character, INCLUDING the closing quote. Reading
				// `$'the daemon\'s namespace'` with the plain rule closes
				// the quote at the escaped apostrophe and re-opens at the
				// real one, and the window then runs to the next apostrophe
				// anywhere below — which in these scripts is a comment a few
				// lines down. That self-corrects, so it blanks a WINDOW
				// rather than the tail, and the EOF invariant cannot see it.
				inAnsiC = true
				lineOut.WriteString("  ")
				i++
			case c == '\'':
				inSingle = true
				lineOut.WriteByte(' ')
			case c == '"':
				inDouble = true
				lineOut.WriteByte(' ')
			case c == '#' && (i == 0 || strings.IndexByte(" \t;&|(", line[i-1]) >= 0):
				lineOut.WriteString(strings.Repeat(" ", len(line)-i))
				i = len(line)
			case c == '$' && i+2 < len(line) && line[i+1] == '(' && line[i+2] == '(':
				// Inside $((…)) a `<<` is a left shift, not a heredoc.
				// `$((1 << 2))` opened a heredoc whose delimiter was `2`.
				// Only the `$((` form is tracked: a bare `((` is ambiguous
				// with nested subshells, and guessing arithmetic there would
				// make us MISS a real heredoc opener, which is the unsound
				// direction.
				arith += 2
				lineOut.WriteString("$((")
				i += 2
			case arith > 0 && (c == '(' || c == ')'):
				if c == '(' {
					arith++
				} else {
					arith--
				}
				lineOut.WriteByte(c)
			case arith == 0 && c == '<' && i+1 < len(line) && line[i+1] == '<' &&
				(i+2 >= len(line) || line[i+2] != '<') &&
				(i == 0 || line[i-1] != '<'):
				// `<<` or `<<-`, then an optionally quoted delimiter word.
				//
				// Both `<<<` exclusions are load-bearing, and the second one
				// was found the hard way. A herestring is three `<`, so the
				// forward check skips the FIRST of them — and then the scan
				// arrives at the second, sees `<<` followed by `"`, and opens
				// a heredoc whose delimiter is the herestring's own contents.
				// That delimiter never appears again, so every line below is
				// blanked: `grep -q … <<<"${resources}"` at set-up-demo.sh:103
				// silently swallowed the remaining 500 lines of the file, and
				// the only symptom was a mutation that failed with the wrong
				// message.
				lineOut.WriteString("  ")
				i += 2
				tabs := false
				if i < len(line) && line[i] == '-' {
					tabs = true
					lineOut.WriteByte(' ')
					i++
				}
				for i < len(line) && (line[i] == ' ' || line[i] == '\t') {
					lineOut.WriteByte(line[i])
					i++
				}
				var delim strings.Builder
				quote := byte(0)
				if i < len(line) && (line[i] == '\'' || line[i] == '"') {
					quote = line[i]
					lineOut.WriteByte(' ')
					i++
				}
				for i < len(line) {
					c := line[i]
					if quote != 0 && c == quote {
						lineOut.WriteByte(' ')
						i++
						break
					}
					if quote == 0 && strings.IndexByte(" \t;&|)", c) >= 0 {
						break
					}
					if quote == 0 && c == '\\' && i+1 < len(line) {
						// `<<\EOF` is bash's third way of quoting a
						// delimiter, equivalent to `<<'EOF'`. Taking the
						// backslash as part of the word gives a delimiter
						// that never arrives, and everything below is
						// blanked.
						lineOut.WriteByte(' ')
						i++
						c = line[i]
					}
					delim.WriteByte(c)
					lineOut.WriteByte(c)
					i++
				}
				i--
				// An explicitly quoted EMPTY delimiter is a heredoc that ends
				// at the first empty line. Requiring a non-empty delimiter
				// here let `cat <<""` through as code.
				if quote != 0 || delim.Len() > 0 {
					pending = append(pending, struct {
						delim string
						tabs  bool
					}{delim.String(), tabs})
				}
			default:
				lineOut.WriteByte(c)
			}
		}
		// A quote left open continues onto the next line; a comment does not,
		// and a heredoc opened on this line starts on the next one.
		if len(pending) > 0 {
			heredoc, heredocTabs = pending[0].delim, pending[0].tabs
			inHeredoc = true
			pending = pending[1:]
		}
		out.WriteString(lineOut.String())
		out.WriteByte('\n')
	}
	return strings.TrimSuffix(out.String(), "\n")
}

// TestShellCodeSelfTest checks the scanner the guard checks depend on.
//
// shellCode is the only thing standing between "the script calls the guard"
// and "the script mentions the guard", so a desync in it is a silent hole in
// TestDemoNSGuardCoversEveryMutatingScript — and desync is its characteristic
// failure: one unbalanced quote or one heredoc whose delimiter never arrives
// and the whole rest of the file is blanked, which reads as "this script has
// no guard call" no matter what it has.
//
// The invariant at the end is the one that actually caught a bug. A
// herestring is `<<<`, and the first version skipped it by looking forward
// only: the scan stepped onto the SECOND `<`, saw `<<` followed by `"`, and
// opened a heredoc whose delimiter was the herestring's own contents. From
// set-up-demo.sh:103 the remaining 500 lines were blanked. Every guard test
// still passed, because blanking is the safe direction — the bug surfaced
// only as a mutation failing with the wrong message.
func TestShellCodeSelfTest(t *testing.T) {
	const marker = "require_demo_ns_matches_base || exit 1"
	cases := []struct {
		name string
		in   string
		want bool // is the marker still executable code afterwards?
	}{
		{"bare call", marker, true},
		{"commented out", "# " + marker, false},
		{"trailing comment on another line", "echo hi # note\n" + marker, true},
		{
			// An apostrophe in a comment must not open a quote. This is why
			// `#` is only a comment at the start of a word.
			"apostrophe in a comment",
			"# the daemon's namespace\n" + marker,
			true,
		},
		{"# inside a word is not a comment", "echo a#b\n" + marker, true},
		{"quoted heredoc body", "cat <<'USAGE'\n" + marker + "\nUSAGE\n", false},
		{"unquoted heredoc body", "cat <<EOF\n" + marker + "\nEOF\n", false},
		{"tab-stripped heredoc body", "cat <<-EOF\n" + marker + "\n\tEOF\n", false},
		{"code after a heredoc", "cat <<'EOF'\nhello\nEOF\n" + marker, true},
		{"code after a heredoc with a pipe", "cat <<POD | kubectl apply -f -\nx\nPOD\n" + marker, true},
		{"read -d '' heredoc", "read -r -d '' S <<EOF || true\n" + marker + "\nEOF\n", false},
		{"single-quoted span", "X='" + marker + "'\n", false},
		{"multi-line single-quoted span", "X='\n" + marker + "\n'\n", false},
		{"multi-line double-quoted span", "X=\"\n" + marker + "\n\"\n", false},
		{"code after a multi-line string", "X=\"\na\n\"\n" + marker, true},
		{
			// The regression: a herestring must not be read as a heredoc, or
			// everything below it disappears.
			"code after a herestring",
			"grep -q \"^x\" <<<\"${resources}\"\n" + marker,
			true,
		},
		{"code after a herestring with a quoted needle", "grep -qxF \"${a}\" <<<\"${b}\"\n" + marker, true},

		// `cat <<""` is a heredoc that ends at the first EMPTY line. Requiring
		// a non-empty delimiter word left its body scanned as code — the only
		// case ever found of this scanner presenting data as code, which is
		// the one direction the whole approach depends on not happening.
		{"empty quoted heredoc delimiter", "cat <<\"\" >/dev/null\n" + marker + "\n\nepilogue\n", false},
		{"code after an empty-delimiter heredoc", "cat <<\"\" >/dev/null\nbody\n\n" + marker, true},
		// $'…' is not a single-quoted string: the backslash escapes the
		// closing quote. Reading it with the plain rule opens a window that
		// closes again at the next apostrophe below — in these scripts, a
		// comment a few lines down — so it blanks a window, not a tail.
		{"ansi-c quoting with an escaped apostrophe", "N=$'the daemon\\'s namespace'\n" + marker, true},
		{"left shift is not a heredoc", "N=$((1 << 2))\n" + marker, true},
		{"backslash-quoted heredoc delimiter", "cat <<\\EOF\nbody\nEOF\n" + marker, true},
		{"parameter expansion # is not a comment", "p=${PWD##*/}\n" + marker, true},
		{"two heredocs on one line", "cat <<A <<B\na\nA\nb\nB\n" + marker, true},
		{"delimiter containing a quote", "cat <<\"E'F\"\n" + marker + "\nE'F\n", false},
		{"command substitution inside double quotes", "echo \"$(" + marker + ")\"", false},
	}

	// Cases where shellCode is deliberately STRICTER than bash: it blanks a
	// line the shell does run. That direction is safe — the guard checks then
	// report a missing call that is present, which is loud and gets fixed —
	// but it has to be a decision rather than an accident, so each one is
	// named here and the oracle below fails any that is not.
	strict := map[string]bool{
		"command substitution inside double quotes": true,
	}

	// The oracle. Pinning `want` only ever checks the scanner against what I
	// believed bash does, and the two bugs this test has caught were both
	// cases where that belief was wrong. So every case is also RUN, and the
	// result compared against what actually happened.
	//
	// The asymmetry is the point. shellCode blanking code is safe; shellCode
	// keeping DATA is the failure that makes the guard checks meaningless,
	// and that is the direction asserted unconditionally.
	bashPath, bashErr := exec.LookPath("bash")
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := strings.Contains(shellCode(tc.in), marker)
			if got != tc.want {
				t.Errorf("shellCode kept the marker as code = %v, want %v\n--- in ---\n%s\n--- out ---\n%s",
					got, tc.want, tc.in, shellCode(tc.in))
			}
			if bashErr != nil {
				t.Skip("bash not on PATH; scanner checked against `want` only")
			}
			// The stub prints only if the shell really reaches the call.
			script := "require_demo_ns_matches_base() { echo MARKER_RAN; }\n" + tc.in + "\n"
			path := filepath.Join(t.TempDir(), "case.sh")
			if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
				t.Fatalf("write case: %v", err)
			}
			if out, err := exec.Command(bashPath, "-n", path).CombinedOutput(); err != nil {
				t.Fatalf("this case is not valid bash, so running it proves nothing: %v\n%s", err, out)
			}
			out, _ := exec.Command(bashPath, path).CombinedOutput()
			bashRan := strings.Contains(string(out), "MARKER_RAN")
			switch {
			case got && !bashRan:
				t.Errorf("UNSOUND: shellCode keeps the marker as code, but bash does not run it — "+
					"it is data. Every guard check reads this output, so a call that only "+
					"LOOKS like one now satisfies them.\n--- in ---\n%s", tc.in)
			case !got && bashRan && !strict[tc.name]:
				t.Errorf("shellCode blanks the marker but bash runs it. Safe direction, but it "+
					"makes a present guard report as missing, which is a red test pointing at "+
					"the wrong thing. Fix the scanner, or add %q to `strict` and say why.\n"+
					"--- in ---\n%s", tc.name, tc.in)
			case got && bashRan && strict[tc.name]:
				t.Errorf("%q is listed in `strict` as a deliberate divergence, but shellCode and "+
					"bash now agree. Drop it from the map.", tc.name)
			}
		})
	}

	// And the invariant, over the real scripts: a script must not END with the
	// scanner still inside a quote or a heredoc. Any desync anywhere above
	// swallows everything below it, so "the last line of the file is still
	// code" is a cheap proxy for "the scanner stayed in step all the way
	// down" — and it is the assertion the `<<<` bug would have failed.
	paths, err := filepath.Glob(filepath.Join("scripts", "*.sh"))
	if err != nil {
		t.Fatalf("glob scripts: %v", err)
	}
	if len(paths) == 0 {
		t.Fatal("no scripts found; this invariant is checking nothing")
	}
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		raw := strings.Split(string(b), "\n")
		code := strings.Split(shellCode(string(b)), "\n")
		last := -1
		for i, l := range raw {
			if s := strings.TrimSpace(l); s != "" && !strings.HasPrefix(s, "#") {
				last = i
			}
		}
		if last < 0 {
			continue
		}
		if strings.TrimSpace(code[last]) == "" {
			t.Errorf("%s: shellCode blanked the file's last code line (%d: %q), which "+
				"means the scanner reached EOF still inside a quote or a heredoc. "+
				"Everything below the desync is invisible to the guard checks.",
				p, last+1, strings.TrimSpace(raw[last]))
		}
	}
}

// TestDemoNSGuardCoversEveryMutatingScript pins the split that
// require_demo_ns_matches_base makes, because the split is a judgement call
// and the next script added to this recipe will not know it was made.
//
// DEMO_NS is only a `kubectl -n` argument — deploy/base hardcodes the
// daemon's namespace — so overriding it desynchronises everything named from
// it. But the damage is not uniform, and the guard is deliberately not folded
// into require_coordinates:
//
//   - A script that CREATES, GRANTS or DELETES anything named from DEMO_NS
//     desynchronises it from where the daemon runs, and some of those fail
//     silently (grant-iam.sh's Workload Identity binding succeeds against a
//     namespace with no workloads; debug-pod.sh's Pod runs as `default`,
//     which exists everywhere, so the create succeeds wherever it is
//     pointed). Those must refuse.
//   - A script that genuinely only reads fails loudly and harmlessly against
//     an empty namespace. So does dev/uat/gke-drill, which sources prereqs.sh
//     and runs its dryrun against a fake kubectl with every coordinate at a
//     distinct value so that a hardcoded name anywhere in the drill is
//     caught. Refusing there would make DEMO_NS the one coordinate it cannot
//     vary — and it did, until this test's first run turned all 149 drill
//     cases red.
//
// Three properties, each of which was broken in an earlier draft:
//
//	presence   — every mutating script calls the guard, no read-only one does
//	precedence — the call comes before the script's first mutation, and in
//	             teardown.sh AFTER the local credential cleanup, which must
//	             not be behind a new way to exit 1
//	coverage   — every script that sources prereqs.sh is on one of the lists
//
// Two honest limits, because a test that overstates its reach is worse than
// one that admits where it stops:
//
// The precedence check finds the first LINE MENTIONING a mutating verb, not
// the first mutation. Five of the six are currently satisfied by something
// that is not one — a `kubectl` inside an error message, a `gcloud` inside
// printed advice, a `K="kubectl …"` assignment, and a `curl` that is not a
// request at all but the substring in `curlimages/curl:8.11.1`. That is the
// safe direction (an earlier `first` only makes the check stricter, and quoted
// text cannot push it later), and each script's real first mutation was
// checked by hand to be later still. It is deliberately not
// tightened to "before any kubectl runs", because that property is already
// false and cannot be made true: prereqs.sh itself runs `gcloud config
// get-value` and `kubectl config current-context` at SOURCE time, so every
// script in this recipe executes both before its guard. Those are reads.
//
// Coverage's domain is scripts that SOURCE prereqs.sh. A new script that sets
// its own DEMO_NS default and runs kubectl is invisible to this test by
// construction. That is not fixable here; it is why the classification rule
// is also written out in prereqs.sh, next to the function.
func TestDemoNSGuardCoversEveryMutatingScript(t *testing.T) {
	// Anchored, and matched against shellCode() rather than the raw body:
	// `# require_demo_ns_matches_base || exit 1` contains the call as a
	// substring, and a heredoc can contain the whole line at column zero.
	guardRE := regexp.MustCompile(`(?m)^[ \t]*require_demo_ns_matches_base \|\| exit 1[ \t]*$`)
	// Loose on purpose, in the opposite direction: this one decides whether a
	// script is in scope at all, so `if ! require_coordinates; then`, a
	// different exit code, or a line continuation must all still count.
	sourcesRE := regexp.MustCompile(`(?m)^[ \t]*(\.|source)[ \t]+.*prereqs\.sh`)
	// What counts as a mutating verb. Matched against the body with only
	// comments removed — NOT against shellCode() — because `K="kubectl …"` is
	// a real signal that the script is about to use kubectl, and blanking the
	// string would hide it. False positives are safe here; false negatives
	// are the failure mode, which is why `curl` (the probe's real PATCH
	// route) and `kustomize edit` (which rewrites a tracked file) are on the
	// list alongside the obvious three.
	//
	// `rm` and `mkdir` are deliberately absent. Local scratch under
	// RIG_STATE_DIR is allowed before the guard, and is in fact required:
	// teardown.sh's token cleanup has to run before every way of exiting 1.
	mutationRE := regexp.MustCompile(`kubectl|gcloud|sed -i|kustomize edit|curl|helm|crane|docker`)

	mutating := []string{
		"set-up-demo.sh", "teardown.sh", "gen-tokens.sh", "grant-iam.sh",
		"debug-pod.sh", "verify-gated-apply.sh",
	}
	// Read-only HERE means "creates, grants or deletes nothing named from
	// DEMO_NS" — not "makes no writes". break-workload.sh patches Deployments;
	// it is on this list because everything it names comes from TARGET_NS.
	// Nothing in this test can check that, so the list is a claim a human
	// makes. What keeps it honest is the negative assertion below: moving a
	// script here forces the guard call to be DELETED in the same diff, so the
	// reclassification cannot be quiet.
	readOnly := []string{"attach.sh", "break-workload.sh", "build-content-image.sh"}

	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join("scripts", name))
		if err != nil {
			t.Fatalf("read scripts/%s: %v", name, err)
		}
		return string(b)
	}

	// presence
	for _, name := range mutating {
		if !guardRE.MatchString(shellCode(read(name))) {
			t.Errorf("scripts/%s creates, grants or deletes state named from DEMO_NS "+
				"but has no `require_demo_ns_matches_base || exit 1` line that the shell "+
				"will execute. An overridden DEMO_NS would desynchronise it from the "+
				"namespace deploy/base hardcodes. (A commented-out call does not count, "+
				"and neither does one inside a heredoc or a quoted string.)", name)
		}
	}
	for _, name := range readOnly {
		if guardRE.MatchString(shellCode(read(name))) {
			t.Errorf("scripts/%s is on the read-only side and calls the guard. If it has "+
				"started creating or deleting anything in DEMO_NS, move it to the "+
				"mutating list; if not, drop the call — the drill sources prereqs.sh "+
				"and needs to run DEMO_NS at a value of its own.", name)
		}
	}

	// precedence: the guard is worth nothing after the thing it guards
	lineOf := func(body string, re *regexp.Regexp) int {
		for i, line := range strings.Split(body, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "#") {
				continue
			}
			if re.MatchString(line) {
				return i + 1
			}
		}
		return -1
	}
	for _, name := range mutating {
		body := read(name)
		guard := lineOf(shellCode(body), guardRE)
		if guard < 0 {
			continue // presence already reported it
		}
		first := lineOf(body, mutationRE)
		if first < 0 {
			// Not "nothing to check" — the precedence property is unverifiable
			// for this file, which is a thing the author has to resolve rather
			// than something the test may quietly skip. It fires for no file
			// in the tree today, so turning it on costs nothing, and the
			// escape hatch it closes is real: `kustomize edit set image`
			// rewrites a tracked file and matched none of the first three
			// verbs this list started with.
			t.Errorf("scripts/%s is listed as mutating but contains none of the verbs "+
				"this test knows how to find (%s), so the guard's position cannot be "+
				"checked. Add the verb it actually mutates through to mutationRE. "+
				"Reclassifying it as read-only also silences this, and is almost "+
				"certainly the wrong fix: nothing here verifies that a read-only "+
				"script is read-only (break-workload.sh is on that list and patches "+
				"Deployments all day), so the only thing standing behind the label is "+
				"that it creates nothing NAMED FROM DEMO_NS. If that is genuinely "+
				"true, say so in the review.", name, mutationRE)
			continue
		}
		if guard > first {
			t.Errorf("scripts/%s calls the guard at line %d but its first mutating line "+
				"is %d. A guard after the damage is decoration.", name, guard, first)
		}
	}

	// ...and the one script where the guard must NOT come first. teardown.sh
	// removes the local bearer tokens before anything that can fail, because
	// the operator most likely to trip a coordinate check is the one with a
	// stale DEMO_NS exported — exactly the person running teardown.
	teardown := shellCode(read("teardown.sh"))
	stash := lineOf(teardown, regexp.MustCompile(`^rm -f\b`))
	guard := lineOf(teardown, guardRE)
	coords := lineOf(teardown, regexp.MustCompile(`require_coordinates`))
	switch {
	case stash < 0:
		t.Error("scripts/teardown.sh no longer removes the local token stash")
	case guard > 0 && stash > guard, coords > 0 && stash > coords:
		t.Errorf("scripts/teardown.sh removes the token stash at line %d, after a "+
			"coordinate check (require_coordinates line %d, require_demo_ns_matches_base "+
			"line %d). Every check is a new way to exit 1 before the tokens are gone, "+
			"and live bearer tokens on disk is the worst thing to strand.",
			stash, coords, guard)
	}

	// coverage: nobody joins the recipe without being classified
	declared := make(map[string]bool, len(mutating)+len(readOnly))
	for _, n := range append(append([]string{}, mutating...), readOnly...) {
		declared[n] = true
	}
	var sourcing int
	err := filepath.WalkDir("scripts", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel("scripts", path)
		if err != nil {
			return err
		}
		// By path, not by basename: scripts/lib/prereqs.sh is a different
		// file from the one that defines the function, and skipping it by
		// name would put a hole in exactly the nested walk this loop exists
		// to do.
		if rel == "prereqs.sh" {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !sourcesRE.Match(b) {
			return nil
		}
		sourcing++
		if !declared[rel] {
			t.Errorf("scripts/%s sources prereqs.sh but is in neither list in this test. "+
				"Decide: does it create, grant or delete anything named from DEMO_NS? "+
				"Creating a Pod counts, whatever the script is for. If yes, add "+
				"`require_demo_ns_matches_base || exit 1` and list it as mutating. "+
				"If no, list it as read-only.", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk scripts: %v", err)
	}
	if sourcing != len(declared) {
		t.Errorf("%d scripts source prereqs.sh but %d are listed here; a listed script "+
			"has been renamed, deleted, or has stopped sourcing prereqs.sh in a form "+
			"this test recognises", sourcing, len(declared))
	}
}

// TestDemoNSGuardParsesTheNamespace runs the real shell function against
// mutated copies of the manifest it reads.
//
// require_demo_ns_matches_base decides whether a script may touch the cluster
// by comparing DEMO_NS to deploy/base/00-namespace.yaml — and it reads that
// file with sed, because prereqs.sh has no YAML parser and cannot acquire one
// without adding a dependency to a recipe whose whole claim is that it is
// self-contained. A sed parse is fine right up until the manifest changes
// shape, and then it is worse than no check at all: the first draft used
// `head -1`, which on a file that had grown a second document would have
// compared DEMO_NS against some other object's name and reported agreement.
//
// So the function is strict, and this proves the strictness by breaking the
// manifest rather than by asserting that the code says `grep -c`. Every case
// runs in ONE bash process that sources the real prereqs.sh once and then
// re-points DEMO_DEPLOY_DIR per case — the function reads it at call time.
// Sourcing prereqs.sh costs ~1.6s, so a process per case would put ten
// seconds into a package that otherwise runs in twenty milliseconds.
func TestDemoNSGuardParsesTheNamespace(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skipf("bash not on PATH: %v", err)
	}
	prereqs, err := filepath.Abs(filepath.Join("scripts", "prereqs.sh"))
	if err != nil {
		t.Fatalf("resolve prereqs.sh: %v", err)
	}
	committed, err := os.ReadFile(filepath.Join("deploy", "base", "00-namespace.yaml"))
	if err != nil {
		t.Fatalf("read 00-namespace.yaml: %v", err)
	}

	// The name the committed manifest actually carries, via a real YAML
	// parser. Hardcoding "gke-platform-agent" here would assert the same
	// literal on both sides and test nothing.
	var ns struct {
		Kind     string `yaml:"kind"`
		Metadata struct {
			Name string `yaml:"name"`
		} `yaml:"metadata"`
	}
	if err := yaml.Unmarshal(committed, &ns); err != nil {
		t.Fatalf("parse 00-namespace.yaml: %v", err)
	}
	if ns.Kind != "Namespace" || ns.Metadata.Name == "" {
		t.Fatalf("00-namespace.yaml is kind=%q name=%q; this test assumes one named Namespace",
			ns.Kind, ns.Metadata.Name)
	}

	// A second document, of the kind someone would plausibly add here.
	secondDoc := "---\napiVersion: v1\nkind: ResourceQuota\nmetadata:\n" +
		"  name: some-other-object\n  namespace: " + ns.Metadata.Name + "\n"

	const notSingle = "is not the single Namespace object this expects"
	cases := []struct {
		name     string
		manifest string // "" means do not create the file at all
		demoNS   string
		wantOK   bool
		wantMsg  string
	}{
		{
			name:     "committed manifest, DEMO_NS agrees",
			manifest: string(committed),
			demoNS:   ns.Metadata.Name,
			wantOK:   true,
		},
		{
			name:     "committed manifest, DEMO_NS overridden",
			manifest: string(committed),
			demoNS:   "somewhere-else",
			wantMsg:  "but this deploy tree hardcodes",
		},
		{
			// The head -1 regression. DEMO_NS matches the FIRST name in the
			// file, so a lenient parse reports agreement and the caller goes
			// on to apply into a tree it has not actually checked.
			name:     "second document appended, DEMO_NS matches the first name",
			manifest: string(committed) + secondDoc,
			demoNS:   ns.Metadata.Name,
			wantMsg:  notSingle,
		},
		{
			name:     "second two-space name: under another key",
			manifest: string(committed) + "  name: not-the-namespace\n",
			demoNS:   ns.Metadata.Name,
			wantMsg:  notSingle,
		},
		{
			name:     "no longer a Namespace",
			manifest: strings.Replace(string(committed), "kind: Namespace", "kind: Project", 1),
			demoNS:   ns.Metadata.Name,
			wantMsg:  notSingle,
		},
		{
			// Valid YAML that kustomize and kubectl both accept, and which a
			// sed parse carries through with the quotes still attached. It has
			// to refuse — but saying "DEMO_NS is 'x' but this tree hardcodes
			// '\"x\"'" would send the operator to doubt the check rather than
			// the file, so the refusal names the real problem.
			name: "name value is quoted",
			manifest: strings.Replace(string(committed),
				"name: "+ns.Metadata.Name, `name: "`+ns.Metadata.Name+`"`, 1),
			demoNS:  ns.Metadata.Name,
			wantMsg: "is not a bare",
		},
		{
			name: "name value carries a trailing comment",
			manifest: strings.Replace(string(committed),
				"name: "+ns.Metadata.Name, "name: "+ns.Metadata.Name+" # the daemon lives here", 1),
			demoNS:  ns.Metadata.Name,
			wantMsg: "is not a bare",
		},
		{
			name:    "manifest missing entirely",
			demoNS:  ns.Metadata.Name,
			wantMsg: "no deployment namespace manifest at",
		},
	}

	root := t.TempDir()
	args := []string{"bash", prereqs}
	for i, tc := range cases {
		caseDir := filepath.Join(root, fmt.Sprintf("case%d", i))
		if tc.manifest != "" {
			dir := filepath.Join(caseDir, "deploy", "base")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatalf("mkdir %s: %v", dir, err)
			}
			if err := os.WriteFile(filepath.Join(dir, "00-namespace.yaml"),
				[]byte(tc.manifest), 0o644); err != nil {
				t.Fatalf("write manifest: %v", err)
			}
		}
		args = append(args, caseDir, tc.demoNS)
	}

	// Sources prereqs.sh with SCRIPT_DIR pointed at a directory that does not
	// exist — the same override dev/uat/gke-drill/lib.sh uses — then ignores
	// the DEMO_DEPLOY_DIR it derived and sets one per case. Nothing here may
	// read the committed tree, or a case that should refuse would pass.
	const driver = `
SCRIPT_DIR=/nonexistent/scripts
. "$1"
shift
i=0
while [ $# -gt 0 ]; do
    DEMO_DEPLOY_DIR="$1/deploy"
    DEMO_NS="$2"
    shift 2
    echo "=== CASE ${i}"
    require_demo_ns_matches_base 2>&1
    echo "=== RC $?"
    i=$((i + 1))
done
`
	cmd := exec.Command(bash, append([]string{"-c", driver}, args...)...)
	// A bare PATH, deliberately: prereqs.sh seeds PROJECT_ID and KUBE_CONTEXT
	// from `gcloud config get-value` and `kubectl config current-context`,
	// both discarded on failure, so with neither reachable this cannot read
	// the developer's active project.
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + root}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("driver failed: %v\noutput:\n%s", err, out)
	}

	blocks := strings.Split(string(out), "=== CASE ")
	if len(blocks) != len(cases)+1 {
		t.Fatalf("got %d case blocks, want %d\noutput:\n%s", len(blocks)-1, len(cases), out)
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			block := blocks[i+1]
			body, rc, found := strings.Cut(block, "=== RC ")
			if !found {
				t.Fatalf("no exit code in case block:\n%s", block)
			}
			ok := strings.TrimSpace(rc) == "0"
			if ok != tc.wantOK {
				t.Fatalf("require_demo_ns_matches_base returned ok=%v, want %v\noutput:\n%s",
					ok, tc.wantOK, body)
			}
			if tc.wantMsg != "" && !strings.Contains(body, tc.wantMsg) {
				t.Errorf("refused, but not for the reason under test: want a message "+
					"containing %q\noutput:\n%s", tc.wantMsg, body)
			}
		})
	}
}
