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

// Package gketroubleshootagent_test is the loader-and-content validation
// for the gke-troubleshoot-agent recipe (v2.9, #644).
//
// The recipe used to promise a diagnose→fix→verify loop while its skill
// content told the model to run `kubectl rollout undo` — with `bash`
// unavailable in the distroless image and no MCP tool exposing that verb.
// The fix was to make the claim true by construction rather than by
// persona: a read-only MCP endpoint, a `tools.disable` list covering the
// local write path, and content that proposes rather than applies.
//
// These tests pin that. They are pure loader + content assertions — no
// cloud credentials, no live cluster, no LLM — so they run as ordinary
// unit tests under `go test ./...` (CI's test-unit presubmit). The live
// GKE run remains a manual UAT documented in DEMO.md.
package gketroubleshootagent_test

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/go-steer/core-agent/v2/pkg/config"
	"github.com/go-steer/core-agent/v2/pkg/mcp"
	"github.com/go-steer/core-agent/v2/pkg/skills"
)

// The recipe's agents dir is the ConfigMap-mounted tree under deploy/base.
// `go test` runs with the package dir as the working directory, so these
// relative paths resolve.
const (
	configDir     = "deploy/base/config"
	skillDir      = configDir + "/skills/k8s-triage"
	referencesDir = skillDir + "/references"

	// readOnlyEndpoint is the GKE MCP endpoint that makes propose-only
	// true at the transport: it does not serve the mutating verbs.
	readOnlyEndpoint = "https://container.googleapis.com/mcp/read-only"
)

// wantDisabled are the builtin tools the recipe must remove from the
// catalog. Each is a way for a model to act on the world despite a
// read-only MCP surface: `bash` (nonexistent in the distroless image, but
// a disabled tool errors legibly instead of confusing the model), the
// three file-mutation tools, and `fetch_url` (arbitrary egress, including
// POSTs — the `alert` tool is the sanctioned, target-allow-listed path).
var wantDisabled = []string{"bash", "write_file", "edit_file", "delete_file", "fetch_url"}

// wantReasons are the k8s Event reasons the watcher's default allow-list
// fires on. Every one needs a reference file or the router silently
// degrades to _fallback.md.
var wantReasons = []string{
	"BackOff",
	"CrashLoopBackOff",
	"ErrImagePull",
	"Evicted",
	"FailedMount",
	"FailedScheduling",
	"ImagePullBackOff",
	"NetworkNotReady",
	"NodeNotReady",
	"OOMKilled",
	"Unhealthy",
}

func loadConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Load(configDir)
	if err != nil {
		t.Fatalf("config.Load(%s): %v", configDir, err)
	}
	return cfg
}

// TestConfigEnforcesProposeOnly asserts the local half of the propose-only
// guarantee: the tools that could mutate anything from inside the pod are
// removed from the catalog, and plan-first is on. config.Load runs
// Validate, so a malformed config fails here rather than at daemon boot.
func TestConfigEnforcesProposeOnly(t *testing.T) {
	cfg := loadConfig(t)

	if cfg.Permissions.Mode != config.PermissionModeYolo {
		t.Errorf("permissions.mode = %q, want %q (a no-TTY daemon cannot answer a prompt)",
			cfg.Permissions.Mode, config.PermissionModeYolo)
	}
	// The property, not the spelling: either permissions.plan_mode:
	// "required" or the deprecated require_plan_artifact bool satisfies
	// this, and advisory mode deliberately does not — it records the
	// artifact but blocks nothing.
	if !cfg.Permissions.PlanGateArmed() {
		t.Errorf("plan gate not armed (plan_mode resolved to %q); "+
			"plan-first is what forces a written plan before any gke MCP call",
			cfg.Permissions.ResolvedPlanMode())
	}

	disabled := make(map[string]bool, len(cfg.Tools.Disable))
	for _, n := range cfg.Tools.Disable {
		disabled[n] = true
	}
	for _, want := range wantDisabled {
		if !disabled[want] {
			t.Errorf("tools.disable is missing %q; the recipe claims the agent cannot act on the cluster, "+
				"so every local mutation/egress path must be removed from the catalog (have %v)",
				want, cfg.Tools.Disable)
		}
	}
}

// TestMCPSurfaceIsReadOnly asserts the transport half of the guarantee.
// This is the assertion that actually makes "propose-only" a property
// rather than a persona instruction: the recipe wires exactly one MCP
// server, at the read-only endpoint, with a read-only OAuth scope. A
// regression to `/mcp` would hand the model apply/patch/delete verbs
// again and nothing else in the recipe would notice.
func TestMCPSurfaceIsReadOnly(t *testing.T) {
	servers, err := mcp.Load(configDir)
	if err != nil {
		t.Fatalf("mcp.Load(%s): %v", configDir, err)
	}
	if len(servers.Servers) != 1 {
		t.Fatalf("recipe declares %d MCP servers, want exactly 1 (\"gke\"): %v",
			len(servers.Servers), serverNames(servers))
	}
	gke, ok := servers.Servers["gke"]
	if !ok {
		t.Fatalf("no \"gke\" MCP server; have %v", serverNames(servers))
	}
	if gke.Transport != "http" {
		t.Errorf("gke transport = %q, want http", gke.Transport)
	}
	if gke.URL != readOnlyEndpoint {
		t.Errorf("gke url = %q, want the read-only endpoint %q; the full-access /mcp endpoint "+
			"would expose mutating verbs the recipe promises the agent does not have",
			gke.URL, readOnlyEndpoint)
	}
	if gke.Auth == nil || gke.Auth.GoogleOAuth == nil {
		t.Fatal("gke server has no google_oauth auth block")
	}
	const readOnlyScope = "https://www.googleapis.com/auth/cloud-platform.read-only"
	var sawScope bool
	for _, s := range gke.Auth.GoogleOAuth.Scopes {
		if s == readOnlyScope {
			sawScope = true
		}
		if s == "https://www.googleapis.com/auth/cloud-platform" {
			t.Errorf("gke oauth requests the read-write scope %q; the recipe's IAM story "+
				"(roles/container.viewer) assumes the read-only scope", s)
		}
	}
	if !sawScope {
		t.Errorf("gke oauth scopes = %v, want %q", gke.Auth.GoogleOAuth.Scopes, readOnlyScope)
	}
}

// mcpToolName matches a namespaced MCP tool name as the model sees it.
// pkg/mcp/namespace.go joins prefix and tool with a SINGLE underscore, so
// "gke__x" (a plausible-looking typo, and one that shipped in #648's docs)
// would be a name no server ever exposes.
var mcpToolName = regexp.MustCompile(`\bgke_[a-z0-9_]+`)

// TestPollAllowNamesRealNamespacedTools asserts wait_and_verify's escape
// hatch is wired correctly. MCP tools never look read-only to the runtime
// (the adapter drops readOnlyHint), so without poll_allow the convergence
// check — the only thing that can justify a RESOLVED status — is refused
// at every call. Names must be the namespaced ones the model sees.
func TestPollAllowNamesRealNamespacedTools(t *testing.T) {
	cfg := loadConfig(t)
	wv := cfg.Tools.WaitAndVerify

	if len(wv.PollAllow) == 0 {
		t.Fatal("tools.wait_and_verify.poll_allow is empty; every convergence check would be refused " +
			"as \"not read-only\", so the agent could never justify RESOLVED")
	}
	for _, name := range wv.PollAllow {
		if strings.HasPrefix(name, "gke__") {
			t.Errorf("poll_allow entry %q uses a double underscore; pkg/mcp namespacing joins "+
				"server and tool with ONE underscore, so this never matches a real tool", name)
			continue
		}
		if !strings.HasPrefix(name, "gke_") {
			t.Errorf("poll_allow entry %q is not namespaced onto the \"gke\" server; "+
				"poll_allow matches the name the model sees", name)
		}
	}
	if wv.MaxTimeoutSeconds <= 0 {
		t.Error("tools.wait_and_verify.max_timeout_seconds must be positive; the references " +
			"ask for waits up to 300s and an under-set cap rejects them outright")
	}
	if wv.MaxAttempts <= 0 {
		t.Error("tools.wait_and_verify.max_attempts must be positive")
	}

	// Every tool the poll_allow list names must actually be named by the
	// skill content — an allow-list entry nothing polls is dead config, and
	// (more importantly) a reference polling a tool that is NOT allow-listed
	// fails at runtime with a refusal the model cannot recover from.
	content := skillContent(t)
	allowed := make(map[string]bool, len(wv.PollAllow))
	for _, n := range wv.PollAllow {
		allowed[n] = true
	}
	for _, n := range wv.PollAllow {
		if !strings.Contains(content, n) {
			t.Errorf("poll_allow names %q but no skill file mentions it", n)
		}
	}
	// And the converse, restricted to the tools the references actually
	// hand to wait_and_verify.
	for _, tool := range pollTargets(content) {
		if !allowed[tool] {
			t.Errorf("a reference polls %q via wait_and_verify but it is not in "+
				"tools.wait_and_verify.poll_allow %v; the call is refused at runtime",
				tool, wv.PollAllow)
		}
	}
}

// pollTargets extracts the tool names passed as wait_and_verify's `tool:`
// argument across the skill content.
func pollTargets(content string) []string {
	re := regexp.MustCompile(`tool:\s*"(gke_[a-z0-9_]+)"`)
	seen := map[string]bool{}
	var out []string
	for _, m := range re.FindAllStringSubmatch(content, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	return out
}

// TestAlertTargetIsWired asserts escalation is real. The agent cannot fix
// anything, so an incident that does not self-heal MUST reach a human;
// the `alert` tool is that path, and it only registers when
// alerts.targets is non-empty.
func TestAlertTargetIsWired(t *testing.T) {
	cfg := loadConfig(t)
	if len(cfg.Alerts.Targets) != 1 {
		t.Fatalf("alerts.targets has %d entries, want exactly 1 (\"oncall\"): %+v",
			len(cfg.Alerts.Targets), cfg.Alerts.Targets)
	}
	tgt := cfg.Alerts.Targets[0]
	if tgt.Name != "oncall" {
		t.Errorf("alert target name = %q, want \"oncall\" (the name the skill fires)", tgt.Name)
	}
	if tgt.Template != config.AlertTemplateGeneric {
		t.Errorf("alert template = %q, want %q; the provider-shaped templates are designed "+
			"but not implemented and are rejected at config load",
			tgt.Template, config.AlertTemplateGeneric)
	}
	if tgt.URLEnv == "" || tgt.URL != "" {
		t.Errorf("alert target must take its URL from url_env (a K8s Secret), not an inline url: "+
			"url_env=%q url=%q", tgt.URLEnv, tgt.URL)
	}
	if tgt.Description == "" {
		t.Error("alert target has no description; the description is what the model sees in the " +
			"tool schema and is how it knows when to page")
	}
	if cfg.Alerts.RateLimitPerTarget == "" {
		t.Error("alerts.rate_limit_per_target is unset; an event storm becomes a page storm")
	} else if _, _, err := config.ParseAlertRateLimit(cfg.Alerts.RateLimitPerTarget); err != nil {
		t.Errorf("alerts.rate_limit_per_target %q does not parse: %v", cfg.Alerts.RateLimitPerTarget, err)
	}

	// The skill has to actually name the configured target: `alert` rejects
	// an unknown target name rather than dialing it, so a drift here turns
	// every escalation into a tool error.
	if !strings.Contains(skillContent(t), `target:  "`+tgt.Name+`"`) &&
		!strings.Contains(skillContent(t), `target: "`+tgt.Name+`"`) {
		t.Errorf("no skill file fires alert(target: %q); escalation would never happen", tgt.Name)
	}
}

// TestSkillAndReferencesLoad asserts the router is discoverable and every
// reason the watcher fires on has its reference. A missing file is not a
// load error — the router falls through to _fallback.md — so nothing but
// this test catches it.
func TestSkillAndReferencesLoad(t *testing.T) {
	got, err := skills.Load(context.Background(), configDir, nil)
	if err != nil {
		t.Fatalf("skills.Load(%s): %v", configDir, err)
	}
	var found bool
	for _, in := range got.Infos {
		if in.Name == "k8s-triage" {
			found = true
		}
	}
	if !found {
		names := make([]string, 0, len(got.Infos))
		for _, in := range got.Infos {
			names = append(names, in.Name)
		}
		t.Fatalf("k8s-triage skill not discovered; got %v", names)
	}

	for _, reason := range append(append([]string{}, wantReasons...), "_fallback") {
		path := filepath.Join(referencesDir, reason+".md")
		info, err := os.Stat(path)
		if err != nil {
			t.Errorf("reference for reason %q missing: %v", reason, err)
			continue
		}
		if info.Size() == 0 {
			t.Errorf("reference %s is empty", path)
		}
	}
}

// forbiddenInSkillContent are verbs that promise an action this agent
// cannot take. Before #644 the references were written as kubectl runbooks
// — `kubectl rollout undo`, `kubectl delete pod`, `sleep 120` — none of
// which the daemon can execute: `bash` is disabled and absent from the
// distroless image. Telling a model to run them produces either a
// confabulated "fixed it" or a tool error it cannot route around.
//
// This is the regression test for the recipe's central claim. It fails on
// the pre-#644 content.
var forbiddenInSkillContent = []string{
	"kubectl",
	"gcloud",
	"sleep ",
	"`bash`",
}

// TestSkillContentPromisesNothingItCannotDo is the content-side half of
// the propose-only guarantee: the skill and its references may not name a
// shell, a CLI, or a mutating MCP verb.
func TestSkillContentPromisesNothingItCannotDo(t *testing.T) {
	for _, path := range skillFiles(t) {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		text := string(body)
		for _, bad := range forbiddenInSkillContent {
			if idx := strings.Index(text, bad); idx >= 0 {
				t.Errorf("%s names %q (%s); the agent has no shell and no CLI — "+
					"remediation belongs in the proposal table, not as a command to run",
					path, bad, lineAt(text, idx))
			}
		}
		// Mutating MCP verbs are the other way to promise an action: the
		// read-only endpoint does not serve them, so naming one produces a
		// tool-not-found at best and a confabulated success at worst.
		for _, tool := range mcpToolName.FindAllString(text, -1) {
			verb := strings.TrimPrefix(tool, "gke_")
			for _, bad := range []string{"apply_", "patch_", "delete_", "create_", "update_", "set_", "scale_", "rollout_undo", "restart_", "cordon", "drain", "evict"} {
				if strings.HasPrefix(verb, bad) || strings.Contains(verb, bad) {
					t.Errorf("%s names mutating MCP tool %q; the recipe wires the read-only "+
						"endpoint, which does not serve it", path, tool)
					break
				}
			}
		}
		if strings.Contains(text, "gke__") {
			t.Errorf("%s uses a double-underscore MCP name (gke__…); pkg/mcp joins server and "+
				"tool with ONE underscore", path)
		}
	}
}

// TestReferencesCarryTheProposeOnlyShape asserts each reference actually
// has the three sections the router's Step 2 tells the model to expect.
// A reference that skips the convergence check leaves the model with no
// evidentiary basis for RESOLVED — which is exactly how a triage agent
// ends up claiming a fix that never happened (#639).
func TestReferencesCarryTheProposeOnlyShape(t *testing.T) {
	wantSections := []string{
		"## Diagnose (read-only)",
		"## Convergence check",
		"## Remediation proposals",
	}
	for _, path := range referenceFiles(t) {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		text := string(body)
		// A pure chain stub (ErrImagePull → ImagePullBackOff) delegates its
		// whole playbook to a sibling and carries no sections of its own.
		if strings.Contains(text, "Chain to `references/") && !strings.Contains(text, "## Diagnose") {
			continue
		}
		for _, sec := range wantSections {
			if !strings.Contains(text, sec) {
				t.Errorf("%s has no %q section; the router's Step 2 promises all three", path, sec)
			}
		}
		if !strings.Contains(text, "wait_and_verify(") {
			t.Errorf("%s has no concrete wait_and_verify(...) call; the convergence check is what "+
				"separates a verified RESOLVED from a guess", path)
		}
	}
}

// TestDeployWiresTheAlertSecret asserts the manifest half of escalation.
// The alert target reads ONNCALL_WEBHOOK_URL from the environment and
// resolves it at call time, so a missing envFrom is invisible until the
// first page fails. The secretRef must be optional so the pod still boots
// for operators who have not wired a webhook yet.
func TestDeployWiresTheAlertSecret(t *testing.T) {
	cfg := loadConfig(t)
	if len(cfg.Alerts.Targets) == 0 {
		t.Skip("no alert targets configured")
	}
	urlEnv := cfg.Alerts.Targets[0].URLEnv

	body, err := os.ReadFile(filepath.Join("deploy", "base", "50-deployment-daemon.yaml"))
	if err != nil {
		t.Fatalf("read daemon manifest: %v", err)
	}
	manifest := string(body)
	if !strings.Contains(manifest, "name: core-agent-alerts") {
		t.Error("daemon manifest has no core-agent-alerts secretRef; the alert target's " +
			"url_env would never be populated and every escalation would fail at call time")
	}
	if !strings.Contains(manifest, "optional: true") {
		t.Error("the alerts secretRef must be optional: true so the pod boots before an " +
			"operator has wired a webhook")
	}
	// The env var name has to appear somewhere an operator will read, or
	// the Secret key is a guess.
	if !strings.Contains(manifest, urlEnv) {
		t.Errorf("daemon manifest never mentions %q; operators have no way to know which "+
			"Secret key to set", urlEnv)
	}
}

// TestSetupWIFGrantsLeastPrivilege asserts the IAM half. The recipe's
// read-only MCP endpoint is only as good as the credential behind it: a
// KSA holding roles/container.admin can still be pointed at the
// full-access endpoint by a one-line config edit, so the script must not
// grant it.
func TestSetupWIFGrantsLeastPrivilege(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("scripts", "setup-wif.sh"))
	if err != nil {
		t.Fatalf("read setup-wif.sh: %v", err)
	}
	script := string(body)
	if !strings.Contains(script, `bind_project_role "roles/container.viewer"`) {
		t.Error("setup-wif.sh does not bind roles/container.viewer")
	}
	if strings.Contains(script, `bind_project_role "roles/container.admin"`) {
		t.Error("setup-wif.sh binds roles/container.admin; the recipe is propose-only and " +
			"reads through the read-only MCP endpoint, so container.viewer is sufficient " +
			"and container.admin re-opens the mutation path")
	}
}

// --- helpers ---

func serverNames(s mcp.Servers) []string {
	out := make([]string, 0, len(s.Servers))
	for name := range s.Servers {
		out = append(out, name)
	}
	return out
}

// skillFiles is SKILL.md plus every reference.
func skillFiles(t *testing.T) []string {
	t.Helper()
	return append([]string{filepath.Join(skillDir, "SKILL.md")}, referenceFiles(t)...)
}

func referenceFiles(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(referencesDir)
	if err != nil {
		t.Fatalf("read %s: %v", referencesDir, err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		out = append(out, filepath.Join(referencesDir, e.Name()))
	}
	if len(out) == 0 {
		t.Fatalf("no reference files under %s", referencesDir)
	}
	return out
}

// skillContent concatenates SKILL.md and every reference, for assertions
// that only care whether something is named anywhere in the playbook.
func skillContent(t *testing.T) string {
	t.Helper()
	var sb strings.Builder
	for _, path := range skillFiles(t) {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		sb.Write(body)
		sb.WriteString("\n")
	}
	return sb.String()
}

// lineAt renders "line N" for a byte offset, so a content failure points
// at somewhere editable.
func lineAt(text string, idx int) string {
	return "line " + strconv.Itoa(strings.Count(text[:idx], "\n")+1)
}

// ---------------------------------------------------------------------
// Watcher pin + watcher RBAC
//
// The two invariants this recipe depends on that nothing else checks.
// #680's image-pin gate covers neither: `imagepin.go` scopes its rules
// to the core-agent daemon images, so the lookout tag is outside it.
// ---------------------------------------------------------------------

// watcherImage is the sentinel this recipe deploys, and wantWatcherTag
// the release every declaration of it must agree on.
//
// v0.17.0 is a FLOOR, not a preference: it retired the
// `k8s-event-watcher` transition naming, so the resource names in
// deploy/base/, the `sa:lookout-watch` proxy identity, and the e2e's
// `lookout_*` metric assertion all assume it. Bump this constant and
// the four sites below together.
const (
	watcherImage   = "ghcr.io/go-steer/lookout"
	wantWatcherTag = "v0.21.0"
)

// TestWatcherImagePinIsConsistent asserts the four DEPLOY sites that
// name a lookout release agree: the base Deployment, both overlays'
// kustomize image transforms, and the e2e's WATCHER_IMAGE default.
// They are edited by hand, in two languages and three YAML shapes, and
// a partial bump deploys one version while the e2e certifies another —
// the failure this recipe's sibling has had a test for since it froze
// its own pin.
//
// The docs are covered separately by TestWatcherTagInDocsIsCurrent;
// keep the two together when bumping.
func TestWatcherImagePinIsConsistent(t *testing.T) {
	base := readRecipeFile(t, filepath.Join("deploy", "base", "51-deployment-watcher.yaml"))
	if want := "image: " + watcherImage + ":" + wantWatcherTag; !strings.Contains(base, want) {
		t.Errorf("base 51-deployment-watcher.yaml does not pin %q", want)
	}

	for _, overlay := range []string{"example", "example-otel"} {
		path := filepath.Join("deploy", "overlays", overlay, "kustomization.yaml")
		if got := kustomizeImageTag(t, path, watcherImage); got != wantWatcherTag {
			t.Errorf("overlay %s pins %s newTag %q, want %q", overlay, watcherImage, got, wantWatcherTag)
		}
	}

	// The e2e default is the fourth site and the easily-forgotten one:
	// it lives outside this directory.
	e2ePath := filepath.Join("..", "..", "dev", "tools", "e2e-recipe-gke-troubleshoot-agent")
	e2e := readRecipeFile(t, e2ePath)
	want := `WATCHER_IMAGE="${WATCHER_IMAGE:-` + watcherImage + ":" + wantWatcherTag + `}"`
	if !strings.Contains(e2e, want) {
		t.Errorf("%s does not default WATCHER_IMAGE to %s:%s", e2ePath, watcherImage, wantWatcherTag)
	}
}

// watcherTagDocs are the documents that quote this recipe's lookout
// release to a reader, relative to this package. A stale one is not
// merely an out-of-date sentence: DEMO.md's preflight SHELLS OUT
// (`crane digest ghcr.io/go-steer/lookout:<tag>`), so a missed bump
// hands an operator a command that verifies a tag the recipe no longer
// deploys — and the site pages are what a copier reads before they ever
// open a manifest.
//
// Precedent for asserting prose against one source of truth is #674's
// TestPublishedFindingCountsMatchTheDocs. The frozen sibling recipe's
// TestDeployWatcherImageFloor is docs-blind in the same way this test
// used to be; that is left alone deliberately (#704 froze the recipe,
// so its docs cannot go stale), but the two matchers below are the
// whole mechanism if it ever needs one.
var watcherTagDocs = []string{
	"README.md",
	"DEMO.md",
	filepath.Join("..", "..", "README.md"),
	filepath.Join("..", "..", "docs", "site", "src", "content", "docs", "reference", "troubleshooting-agent.md"),
	filepath.Join("..", "..", "docs", "site", "src", "content", "docs", "examples", "index.md"),
}

// The two shapes a doc names the pin in: a full image reference, and
// the prose "pins/pinned <tag>" (backticked or not). Deliberately NOT
// matched: "pinning back below `v0.17.0`" and the other floor mentions,
// which are history and must stay put across a bump.
var (
	watcherImageRefRe = regexp.MustCompile(`ghcr\.io/go-steer/lookout:(v[0-9]+\.[0-9]+\.[0-9]+)`)
	watcherPinProseRe = regexp.MustCompile("pin(?:s|ned) `?(v[0-9]+\\.[0-9]+\\.[0-9]+)`?")
)

// TestWatcherTagInDocsIsCurrent holds every reader-facing statement of
// the pin to wantWatcherTag. Each doc must state it at least once, so a
// bump that edits only the manifests fails here, and a doc that quietly
// drops its pin sentence fails too rather than passing vacuously.
func TestWatcherTagInDocsIsCurrent(t *testing.T) {
	for _, path := range watcherTagDocs {
		t.Run(filepath.ToSlash(path), func(t *testing.T) {
			body := readRecipeFile(t, path)

			var found int
			for _, re := range []*regexp.Regexp{watcherImageRefRe, watcherPinProseRe} {
				for _, m := range re.FindAllStringSubmatch(body, -1) {
					found++
					if m[1] != wantWatcherTag {
						t.Errorf("names lookout %s, want %s (in %q)", m[1], wantWatcherTag, m[0])
					}
				}
			}
			if found == 0 {
				t.Errorf("states no lookout pin at all; it is listed in watcherTagDocs because it "+
					"quoted %s — restore the statement or drop the file from the list", wantWatcherTag)
			}
		})
	}
}

// kustomizeImageTag returns the newTag pinned for image in a
// kustomization file's images: block — "" when the image is absent,
// carries no newTag, or appears only in a commented-out alternative.
func kustomizeImageTag(t *testing.T, path, image string) string {
	t.Helper()
	lines := strings.Split(readRecipeFile(t, path), "\n")
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue // commented-out digest / mirror example
		}
		if !strings.Contains(line, "name: "+image) {
			continue
		}
		for j := i + 1; j < len(lines); j++ {
			l := strings.TrimSpace(lines[j])
			if strings.HasPrefix(l, "- name:") {
				break // next image entry; this one has no newTag
			}
			if strings.HasPrefix(l, "newTag:") {
				return strings.Trim(strings.TrimSpace(strings.TrimPrefix(l, "newTag:")), `"`)
			}
		}
	}
	return ""
}

// enrichmentListRequirements mirrors
// state.LoadClusterListRequirements() in go-steer/k8s-lookout as of
// v0.21.0 (pkg/checks/state/cluster.go) — every group/resource the
// scoped-list enrichment pass reads, in the naming form --enrich-lists
// uses.
//
// It is a hand-copied literal because this repo does not depend on
// lookout and should not start: the coupling is a container image, not
// a Go import. Upstream guards its own shipped role against this list
// with an RBAC test; TestWatcherRBACMatchesEnrichLists is the
// equivalent for our NARROWED copy, and a lookout bump that changes
// the list surfaces here as a mismatch to reconcile by hand.
var enrichmentListRequirements = []string{
	"pods",
	"nodes",
	"deployments.apps",
	"replicasets.apps",
	"statefulsets.apps",
	"daemonsets.apps",
	"jobs.batch",
	"cronjobs.batch",
	"services",
	"endpointslices.discovery.k8s.io",
	"ingresses.networking.k8s.io",
	"configmaps",
	"secrets",
	"serviceaccounts",
	"rolebindings.rbac.authorization.k8s.io",
	"roles.rbac.authorization.k8s.io",
	"clusterrolebindings.rbac.authorization.k8s.io",
	"clusterroles.rbac.authorization.k8s.io",
}

// withheldEnrichmentLists are the enrichment reads this recipe
// deliberately does not grant: exactly one, for a security reason —
// `list` on Secrets returns their VALUES, cluster-wide.
var withheldEnrichmentLists = []string{"secrets"}

// TestWatcherRBACMatchesEnrichLists ties three things that drift
// independently into one assertion: the ClusterRole's `list` grants,
// the `--enrich-lists` subtraction the Deployment declares, and the
// documented withheld set. Any two agreeing while the third does not
// is a real deployment defect — either enrichment issues LISTs the API
// server rejects, or the recipe declares a gap it does not have.
func TestWatcherRBACMatchesEnrichLists(t *testing.T) {
	granted := watcherRoleVerbSets(t)

	withheld := map[string]bool{}
	for _, r := range withheldEnrichmentLists {
		withheld[r] = true
	}
	for _, req := range enrichmentListRequirements {
		switch hasList := granted[req]["list"]; {
		case withheld[req] && hasList:
			t.Errorf("ClusterRole grants list on %q, which --enrich-lists subtracts: grant it or stop subtracting it", req)
		case !withheld[req] && !hasList:
			t.Errorf("ClusterRole does not grant list on %q, an enrichment list requirement: "+
				"grant it, or add it to withheldEnrichmentLists AND to --enrich-lists", req)
		}
	}

	// The Deployment must declare exactly the withheld set, so the pass
	// deselects it up front instead of discovering it through a 403.
	args := watcherDeploymentArgs(t)
	wantFlag := "--enrich-lists=all"
	for _, r := range withheldEnrichmentLists {
		wantFlag += ",-" + r
	}
	if !sliceHas(args, wantFlag) {
		t.Errorf("watcher args %v do not carry %q", args, wantFlag)
	}

	// The single verb narrowing the role's header leans on. Everything
	// downstream — recovery tracking off, every informer-backed source
	// off, --storm resolving off — follows from this one fact, so
	// assert the fact rather than the consequences.
	for res, verbs := range granted {
		if verbs["watch"] && res != "events" {
			t.Errorf("ClusterRole grants watch on %q; the only watch in this role may be on events "+
				"(a watch verb turns on informer-backed sources this recipe defers)", res)
		}
	}
	if !granted["events"]["watch"] {
		t.Error("ClusterRole does not grant watch on events — the primary informer target")
	}

	// A ClusterRole has one namespace: the cluster. This one is
	// NARROWER than lookout's shipped role, so under the bare name
	// `kubectl apply -k` would silently downgrade a plain lookout
	// install sharing the cluster.
	role := readRecipeFile(t, filepath.Join("deploy", "base", "12-clusterrole-watcher.yaml"))
	if !strings.Contains(role, "name: lookout-watch-gke-troubleshoot") {
		t.Error("the watcher ClusterRole must be suffixed -gke-troubleshoot so it cannot collide with a plain lookout install")
	}
}

// watcherRoleVerbSets parses 12-clusterrole-watcher.yaml into
// resource → verb set, keyed the way --enrich-lists names things
// ("secrets", "deployments.apps", …). Subresources ("pods/log") get
// their own key and simply never match a requirement.
func watcherRoleVerbSets(t *testing.T) map[string]map[string]bool {
	t.Helper()
	var role struct {
		Rules []struct {
			APIGroups []string `yaml:"apiGroups"`
			Resources []string `yaml:"resources"`
			Verbs     []string `yaml:"verbs"`
		} `yaml:"rules"`
	}
	if err := yaml.Unmarshal([]byte(readRecipeFile(t,
		filepath.Join("deploy", "base", "12-clusterrole-watcher.yaml"))), &role); err != nil {
		t.Fatalf("parse ClusterRole: %v", err)
	}
	if len(role.Rules) == 0 {
		t.Fatal("ClusterRole parsed with no rules")
	}
	out := map[string]map[string]bool{}
	for _, rule := range role.Rules {
		for _, group := range rule.APIGroups {
			for _, res := range rule.Resources {
				key := res
				if group != "" {
					key = res + "." + group
				}
				if out[key] == nil {
					out[key] = map[string]bool{}
				}
				for _, v := range rule.Verbs {
					out[key][v] = true
				}
			}
		}
	}
	return out
}

// watcherDeploymentArgs returns the watcher container's args from
// 51-deployment-watcher.yaml.
func watcherDeploymentArgs(t *testing.T) []string {
	t.Helper()
	var dep struct {
		Spec struct {
			Template struct {
				Spec struct {
					Containers []struct {
						Name string   `yaml:"name"`
						Args []string `yaml:"args"`
					} `yaml:"containers"`
				} `yaml:"spec"`
			} `yaml:"template"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal([]byte(readRecipeFile(t,
		filepath.Join("deploy", "base", "51-deployment-watcher.yaml"))), &dep); err != nil {
		t.Fatalf("parse watcher Deployment: %v", err)
	}
	for _, c := range dep.Spec.Template.Spec.Containers {
		if c.Name == "watcher" {
			return c.Args
		}
	}
	t.Fatal(`watcher Deployment has no container named "watcher"`)
	return nil
}

// readRecipeFile reads a file relative to the recipe dir (the package
// dir is `go test`'s working directory), failing the test.
func readRecipeFile(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(body)
}

// sliceHas reports whether xs contains want.
func sliceHas(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
