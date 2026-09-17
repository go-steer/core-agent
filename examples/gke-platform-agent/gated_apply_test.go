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

// Tests for the gated-apply content root (#1105, box A6 of #1042) — the
// sibling recipe under gated-apply/ that lets this agent apply the fix it
// diagnosed instead of proposing it.
//
// It is a SECOND content root rather than a second config file because the
// mutating posture needs a different mcp.json, and mcp.json is found by a
// fixed name inside the agents dir (pkg/mcp.MCPFileName). Nothing points a
// config at a named MCP file, and mcp.json ships in the content image
// rather than the deploy tree, so it cannot be an overlay patch either.
// The whole difference between read-only and apply-capable therefore has to
// live in a directory, and `-c` selects it: agentsDir = dir(-c), and
// projectRoot = dir(agentsDir) = gated-apply/, which is what gives this leg
// its own AGENTS.md.
//
// What these tests are for: every file here is a near-copy of one under
// .agents/, and near-copies rot. The deployment loads exactly one of them,
// so a fix applied to the read-only recipe and not to this one tests green
// everywhere and reaches the cluster in neither leg.
package gkeplatformagent_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/go-steer/core-agent/v2/pkg/config"
	"github.com/go-steer/core-agent/v2/pkg/permissions"
)

const (
	gatedRoot      = "gated-apply"
	gatedAgentsDir = gatedRoot + "/.agents"

	// applyEndpoint is the full GKE MCP endpoint. Its whole significance is
	// the absence of the /read-only suffix: that sibling does not serve a
	// mutating verb at all, so no config could reach one through it.
	applyEndpoint = "https://container.googleapis.com/mcp"

	// applyScope is the read-write OAuth scope. The read-only scope would
	// withhold the authority the endpoint offers, and the failure would
	// arrive as a 403 mid-incident rather than at boot.
	applyScope = "https://www.googleapis.com/auth/cloud-platform"

	// mutatingTool is the one write verb this leg registers, namespaced by
	// the mcp.json server key ("gke") the way pkg/mcp/namespace.go does it.
	mutatingTool = "gke_patch_k8s_resource"
)

// gatedReadTools is the read surface the PARENT keeps. This is not the
// cluster subagent's surface: that subagent has its own content root and
// its own read-only mcp.json, and nothing here changes it.
//
// It is deliberately WIDE. The base recipe mounts the read-only endpoint
// with no `tools` field at all, so its parent gets everything that endpoint
// serves; the only reason this leg has an allowlist is to exclude the
// mutating verbs other than patch. Every read dropped here is a read
// scenario D's agent cannot make and scenario A's could — a confound in the
// comparison the drill exists to draw, in the direction that flatters the
// apply leg by giving it less rope.
//
// This is the read-only endpoint's catalog IN FULL — all 15 reads — and that
// is the property under test, not a list somebody curated. See
// TestGatedApplyRegistersTheWholeReadOnlyCatalog, which holds it equal to the
// enumeration in examples/gke-parallel-triage, the one recipe in this repo
// that writes the catalog down.
//
// It was assembled by evidence first and only then reconciled, which is worth
// recording because the evidence was not enough. Five names come from this
// recipe's own drill transcripts; check_k8s_auth from the 2026-09-10 C runs
// (dev/uat/gke-drill/runs/REVIEW-GUIDE.md finding 6 argues it should be used
// MORE); the rest from examples/gke-troubleshoot-agent, which drives the same
// endpoint. That produced 13, and 13 was wrong: get_k8s_cluster_info and
// get_k8s_version are served, are named nowhere in this recipe's content, and
// had never been called in a recorded run — invisible to every source above
// and to the scan built on them.
var gatedReadTools = []string{
	"gke_get_k8s_resource",
	"gke_describe_k8s_resource",
	"gke_list_k8s_events",
	"gke_get_k8s_logs",
	"gke_get_k8s_rollout_status",
	"gke_list_k8s_api_resources",
	"gke_get_k8s_cluster_info",
	"gke_get_k8s_version",
	"gke_check_k8s_auth",
	"gke_list_clusters",
	"gke_get_cluster",
	"gke_list_node_pools",
	"gke_get_node_pool",
	"gke_list_operations",
	"gke_get_operation",
}

// forbiddenTools are the mutating verbs the full endpoint also serves and
// this recipe must never register.
//
// apply_k8s_manifest is the interesting exclusion, because it is the more
// natural GitOps verb and it is excluded anyway: it takes an arbitrary
// manifest, so its blast radius has no upper bound, while patch is one
// named object and one field set. delete is excluded because nothing this
// agent diagnoses is fixed by deleting a Deployment.
var forbiddenTools = []string{
	"gke_apply_k8s_manifest",
	"gke_delete_k8s_resource",
	"gke_create_k8s_resource",
	"gke_delete_pod",
}

func gatedLegs() []string { return []string{"config.d1.json", "config.d2.json"} }

// readJSON parses a JSON file into a generic map. Generic rather than typed
// on purpose: these tests are about what the FILE says, and decoding
// through config.Config would silently supply defaults for anything the
// file omits — which is exactly the difference under test.
func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("unmarshal %s: %v", path, err)
	}
	return m
}

func canonical(t *testing.T, v any) string {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	return string(b)
}

// ── the persona ──────────────────────────────────────────────────────

var (
	stanceBegin = regexp.MustCompile(`^<!-- stance:begin (\S+) -->$`)
	stanceEnd   = regexp.MustCompile(`^<!-- stance:end (\S+) -->$`)
)

// splitStance returns the persona's shared lines (with each marked region
// replaced by a single placeholder, so a region moving is a difference) and
// the region names in file order.
func splitStance(t *testing.T, path string) (shared []string, names []string) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var open string
	for n, line := range strings.Split(string(body), "\n") {
		switch {
		case stanceBegin.MatchString(line):
			if open != "" {
				t.Fatalf("%s:%d: stance region %q opens inside %q; regions do not nest",
					path, n+1, stanceBegin.FindStringSubmatch(line)[1], open)
			}
			open = stanceBegin.FindStringSubmatch(line)[1]
			names = append(names, open)
			shared = append(shared, "\x00stance:"+open)
		case stanceEnd.MatchString(line):
			got := stanceEnd.FindStringSubmatch(line)[1]
			if got != open {
				t.Fatalf("%s:%d: stance:end %q closes stance:begin %q", path, n+1, got, open)
			}
			open = ""
		case open == "":
			shared = append(shared, line)
		}
	}
	if open != "" {
		t.Fatalf("%s: stance region %q is never closed", path, open)
	}
	return shared, names
}

// TestGatedApplyPersonaDiffersOnlyInStanceRegions is the test that makes a
// second persona affordable.
//
// gated-apply/AGENTS.md is a near-copy of AGENTS.md, and it has to be:
// scenario D compares an apply-capable agent against the read-only A/B/C
// runs, so any prose that differs for a reason unrelated to applying is a
// confound in the result. "Differs only in the stance" is therefore an
// experimental requirement, not tidiness — which is why it is enforced
// mechanically instead of promised in a comment.
//
// The marked regions are the permitted delta. Everything outside them must
// be byte-identical, so editing a shared paragraph in one file and not the
// other fails here and names the line.
func TestGatedApplyPersonaDiffersOnlyInStanceRegions(t *testing.T) {
	baseShared, baseNames := splitStance(t, "AGENTS.md")
	gatedShared, gatedNames := splitStance(t, filepath.Join(gatedRoot, "AGENTS.md"))

	if len(baseNames) == 0 {
		t.Fatal("AGENTS.md declares no stance regions; the marker convention has been removed " +
			"and this test is no longer checking anything")
	}
	if strings.Join(baseNames, ",") != strings.Join(gatedNames, ",") {
		t.Fatalf("the two personas declare different stance regions, so one of them has "+
			"grown or lost a divergence that the other does not account for.\n"+
			"AGENTS.md:            %v\ngated-apply/AGENTS.md: %v", baseNames, gatedNames)
	}

	if len(baseShared) != len(gatedShared) {
		t.Fatalf("shared persona text differs in length: AGENTS.md has %d lines outside "+
			"stance regions, gated-apply/AGENTS.md has %d. A paragraph was added to one "+
			"persona and not the other; either copy it across, or wrap it in a stance region "+
			"if it genuinely belongs to one leg.", len(baseShared), len(gatedShared))
	}
	for i := range baseShared {
		if baseShared[i] != gatedShared[i] {
			t.Fatalf("shared persona text diverged at shared line %d.\n"+
				"Everything outside a stance region must be byte-identical, so that the only "+
				"thing scenario D changes about this agent is whether it can apply a fix.\n"+
				"AGENTS.md:             %q\ngated-apply/AGENTS.md:  %q",
				i+1, baseShared[i], gatedShared[i])
		}
	}
}

// TestGatedApplyStanceRegionsActuallyDiffer is the other half of the test
// above, and it is the half that catches a lazy copy.
//
// A gated-apply/AGENTS.md produced by `cp` passes the identity check
// perfectly — every shared line matches, every region name matches — while
// telling the agent it is propose-only and has no write path. It would boot,
// register the patch tool, and decline to use it, and the leg would read as
// "the model chose not to apply" rather than "nobody wrote the persona".
func TestGatedApplyStanceRegionsActuallyDiffer(t *testing.T) {
	region := func(path string) map[string]string {
		t.Helper()
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		out, open := map[string]string{}, ""
		var buf []string
		for _, line := range strings.Split(string(body), "\n") {
			switch {
			case stanceBegin.MatchString(line):
				open, buf = stanceBegin.FindStringSubmatch(line)[1], nil
			case stanceEnd.MatchString(line):
				out[open] = strings.Join(buf, "\n")
				open = ""
			case open != "":
				buf = append(buf, line)
			}
		}
		return out
	}

	base := region("AGENTS.md")
	gated := region(filepath.Join(gatedRoot, "AGENTS.md"))
	for name, baseText := range base {
		if gated[name] == baseText {
			t.Errorf("stance region %q is identical in both personas.\n"+
				"A region exists because the two legs must say different things there; an "+
				"unchanged one means gated-apply/AGENTS.md was copied and not edited, and the "+
				"apply leg is running on propose-only instructions.", name)
		}
	}
}

// TestGatedApplyPersonaNamesTheToolItCanActuallyCall closes the loop between
// the persona and the allowlists.
//
// The persona tells the agent it has one write verb and names it. If that
// name and the one in mcp.json/config ever part company, the agent is told
// about a tool it does not have — and the symptom is indistinguishable from
// a model that declined to act.
func TestGatedApplyPersonaNamesTheToolItCanActuallyCall(t *testing.T) {
	body, err := os.ReadFile(filepath.Join(gatedRoot, "AGENTS.md"))
	if err != nil {
		t.Fatalf("read persona: %v", err)
	}
	if !strings.Contains(string(body), mutatingTool) {
		t.Errorf("gated-apply/AGENTS.md never names %s, the only mutating tool this leg "+
			"registers. An agent that is not told which verb it has does not go looking for one.",
			mutatingTool)
	}
	for _, forbidden := range forbiddenTools {
		if strings.Contains(string(body), forbidden) {
			t.Errorf("gated-apply/AGENTS.md names %s, which the mcp.json allowlist does not "+
				"register. Naming an unavailable tool in a persona is how an agent spends a "+
				"turn calling it.", forbidden)
		}
	}
}

// ── the MCP surface ──────────────────────────────────────────────────

// TestGatedApplyMCPSurface pins the transport-level posture: the endpoint,
// the scope, the allowlist, and one absence that matters more than any of
// them.
func TestGatedApplyMCPSurface(t *testing.T) {
	raw := readJSON(t, filepath.Join(gatedAgentsDir, "mcp.json"))
	servers, ok := raw["servers"].(map[string]any)
	if !ok {
		t.Fatal("gated-apply mcp.json has no servers block")
	}
	gke, ok := servers["gke"].(map[string]any)
	if !ok {
		t.Fatal("gated-apply mcp.json has no `gke` server; the server key is the tool-name " +
			"prefix, so renaming it renames every tool the allowlists refer to")
	}

	if got := gke["url"]; got != applyEndpoint {
		t.Errorf("url = %v, want %q.\nThe read-only endpoint serves no mutating verb, so this "+
			"leg cannot work through it; any other URL is not the endpoint the RBAC was "+
			"probed against.", got, applyEndpoint)
	}

	// read_only must be ABSENT, not false. ServerSpec.ReadOnly is stamped
	// onto every tool the server exposes (pkg/mcp/namespace.go), and a tool
	// the gate believes is read-only is exempt from the plan-first check
	// (pkg/tools/gate.go, #693). Declaring it here would let the patch run
	// before any record_plan — quietly cancelling permissions.plan_mode,
	// which is the structure the unattended leg rests on.
	if v, present := gke["read_only"]; present {
		t.Errorf("gated-apply mcp.json declares read_only=%v.\nThe declaration is stamped onto "+
			"EVERY tool from this server, and a read-only tool skips plan-first gating — so "+
			"this would let %s run before any plan was recorded, silently cancelling "+
			"permissions.plan_mode. Remove the field; per-tool readOnlyHint (#1098) is the "+
			"correct source for the reads.", v, mutatingTool)
	}

	auth, _ := gke["auth"].(map[string]any)
	oauth, _ := auth["google_oauth"].(map[string]any)
	scopes, _ := oauth["scopes"].([]any)
	if len(scopes) != 1 || scopes[0] != applyScope {
		t.Errorf("scopes = %v, want exactly [%q]", scopes, applyScope)
	}

	tools, ok := gke["tools"].([]any)
	if !ok {
		t.Fatalf("gated-apply mcp.json declares no `tools` allowlist.\nWithout one, mounting the "+
			"full endpoint registers everything it serves — including %v. The allowlist is "+
			"the posture, not a tidiness measure.", forbiddenTools)
	}
	got := make([]string, 0, len(tools))
	for _, v := range tools {
		s, _ := v.(string)
		got = append(got, s)
	}

	// Entries are UNPREFIXED upstream names: withToolAllowlist filters
	// before withNamespace renames (pkg/mcp/lifecycle.go wrapServerToolset).
	// A `gke_`-prefixed entry here matches nothing and silently drops the
	// tool, because an unmatched allowlist entry is a warning.
	want := make([]string, 0, len(gatedReadTools)+1)
	for _, n := range append(append([]string{}, gatedReadTools...), mutatingTool) {
		want = append(want, strings.TrimPrefix(n, "gke_"))
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("tools allowlist = %v\nwant                 %v\n"+
			"Entries are upstream names without the `gke_` prefix — the filter runs before the "+
			"rename. An entry matching nothing only WARNS (mcp.Server.Warnings), so a wrong "+
			"name here drops the tool and the daemon still boots.", got, want)
	}
}

// TestGatedApplyKeepsEveryReadTheRecipeNames guards the narrowing direction,
// which is the one the allowlist makes easy and nothing else notices.
//
// The base recipe mounts the read-only endpoint with NO `tools` field, so its
// parent registers everything that endpoint serves. This leg has an allowlist
// only to keep the mutating verbs other than patch unregistered — narrowing
// the *reads* is a side effect, never an intent. A read the apply leg lacks
// and scenario A had is a confound in the A-vs-D comparison, and it is
// completely silent: the tool is simply absent, the daemon boots clean, and
// the agent reasons its way around the gap.
//
// The oracle is the recipe's own content. Every gke_ tool it names, a reader
// of that content expects to exist. This caught exactly one real instance:
// the first draft of this leg shipped five reads while the persona promised
// "nodes, autoscaling, networking, storage" and the drill's own review guide
// asked for MORE use of gke_check_k8s_auth.
func TestGatedApplyKeepsEveryReadTheRecipeNames(t *testing.T) {
	toolRE := regexp.MustCompile(`gke_[a-z0-9_]{3,}`)
	forbidden := make(map[string]bool, len(forbiddenTools))
	for _, n := range forbiddenTools {
		forbidden[n] = true
	}
	allowed := make(map[string]bool, len(gatedReadTools)+1)
	for _, n := range append(append([]string{}, gatedReadTools...), mutatingTool) {
		allowed[n] = true
	}

	// Three sources, and the third is the one that matters.
	//
	// AGENTS.md and cluster/ are the recipe's own content: a name they use is
	// a name a reader expects to work. But the tool this test was written
	// after — gke_check_k8s_auth — appears in NEITHER. It was discovered by
	// the model at runtime and only ever recorded in the drill's scorecards.
	// Scanning the recipe alone let a hand-narrowed list pass while dropping
	// it, which is exactly the bug, so the recorded runs are part of the
	// oracle: a tool the agent has demonstrably called on this endpoint is a
	// tool the endpoint serves, whatever the content happens to mention.
	const drillRuns = "../../dev/uat/gke-drill/runs"
	if fi, err := os.Stat(drillRuns); err != nil || !fi.IsDir() {
		t.Fatalf("%s is missing (%v).\nIt is a third of this test's oracle — the reads that "+
			"only appear in recorded transcripts. If the drill moved, repoint this path; do "+
			"not delete it, or the narrowing this test exists to catch goes back to being "+
			"silent.", drillRuns, err)
	}
	var named []string
	for _, root := range []string{"AGENTS.md", "cluster", drillRuns} {
		err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(path, ".md") {
				return err
			}
			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			named = append(named, toolRE.FindAllString(string(body), -1)...)
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	if len(named) == 0 {
		t.Fatal("found no gke_ tool names in the recipe content — the scan is broken, and a " +
			"broken scan here passes silently")
	}

	seen := map[string]bool{}
	for _, name := range named {
		// The persona names the excluded verb FAMILIES as globs —
		// `gke_patch_*`, `gke_delete_*`. The regex stops at the `*` and
		// leaves a trailing underscore, which no real tool name has.
		if strings.HasSuffix(name, "_") {
			continue
		}
		if seen[name] || forbidden[name] {
			continue
		}
		seen[name] = true
		if !allowed[name] {
			t.Errorf("the recipe's content names %s, but gated-apply's tools allowlist does not "+
				"register it.\nThe base recipe's parent has it (no `tools` field = the whole "+
				"read-only endpoint), so scenario D's agent would be strictly less capable than "+
				"scenario A's — a confound, not a safety property. Add it to gatedReadTools and "+
				"to both legs' permissions.allow, or, if dropping it is deliberate, say so here.",
				name)
		}
	}
}

// TestGatedApplyRegistersTheWholeReadOnlyCatalog is the strong form of the
// test above, and it exists because the weak form was not enough.
//
// The scan asks "is every read the recipe MENTIONS registered?", which can
// only catch a tool somebody wrote down. The question that actually matters
// is "is every read the read-only endpoint SERVES registered?", because that
// is the surface scenario A's agent gets — the base recipe has no `tools`
// field, so its parent registers the endpoint's entire catalog. Any read in
// that catalog and missing here is a silent confound in the A-vs-D
// comparison, in the direction that flatters the apply leg.
//
// The scan missed exactly that, twice: `get_k8s_cluster_info` and
// `get_k8s_version` are served by the endpoint, are not named anywhere in
// this recipe's content, and had never been called in a recorded drill run —
// so all three of the scan's sources agreed the 13-entry list was complete
// when it was short by two.
//
// The oracle is `examples/gke-parallel-triage`, which drives the same
// read-only endpoint and enumerates its catalog by category, in prose, for
// the model. That is an unusual thing to depend on and it is deliberate: it
// is the only enumeration of this catalog in the repo, an agent-facing
// document whose accuracy is load-bearing for that recipe independently of
// this one, and it is corroborated arithmetically — 15 reads here, and
// `docs/site/.../concepts/mcp.md` counts 23 tools on the full endpoint, which
// leaves exactly the 8 mutating verbs (the three k8s writes, `update_cluster`
// and the four cluster/node-pool lifecycle verbs).
func TestGatedApplyRegistersTheWholeReadOnlyCatalog(t *testing.T) {
	const catalogDoc = "../gke-parallel-triage/.agents/AGENTS.md"

	body, err := os.ReadFile(catalogDoc)
	if err != nil {
		t.Fatalf("cannot read the read-only catalog enumeration at %s: %v.\nIt is this test's "+
			"only oracle. If that recipe moved or was deleted, repoint this path or copy the "+
			"enumeration here — do not delete the test, or a narrowed read surface goes back "+
			"to being invisible.", catalogDoc, err)
	}

	// The section is the bullet list between the "read-only endpoint" heading
	// line and the "Plus the core-agent built-in tools" line that follows it.
	// Stopping at "Plus" matters: the built-ins and the spawn family are
	// listed in the same backtick style and are not MCP tools at all.
	text := string(body)
	start := strings.Index(text, "read-only endpoint):")
	if start < 0 {
		t.Fatalf("%s no longer contains the 'read-only endpoint):' catalog heading; this test "+
			"cannot find its oracle and is silently passing on an empty list", catalogDoc)
	}
	rest := text[start:]
	if end := strings.Index(rest, "\nPlus "); end >= 0 {
		rest = rest[:end]
	} else {
		t.Fatalf("%s: found the catalog heading but not the 'Plus ' line that terminates it, so "+
			"the scan would run on past the MCP tools into the built-ins", catalogDoc)
	}

	catalog := map[string]bool{}
	for _, m := range regexp.MustCompile("`([a-z][a-z0-9_]{3,})`").FindAllStringSubmatch(rest, -1) {
		catalog[m[1]] = true
	}
	// A floor on the parse, not on the endpoint: if the document's formatting
	// changes the regex may quietly match nothing, and an empty catalog makes
	// every assertion below vacuous.
	if len(catalog) < 10 {
		t.Fatalf("parsed only %d tool names out of %s — the enumeration's format changed and "+
			"this test is no longer reading it", len(catalog), catalogDoc)
	}

	registered := map[string]bool{}
	for _, n := range gatedReadTools {
		registered[strings.TrimPrefix(n, "gke_")] = true
	}

	var missing []string
	for name := range catalog {
		if !registered[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("the read-only endpoint serves %v, which scenario A's agent therefore has and "+
			"gated-apply's `tools` allowlist does not register.\nNarrowing the reads is never "+
			"the intent of that allowlist — it exists to keep the mutating verbs other than "+
			"patch out — and a missing read is silent: the tool is simply absent and the model "+
			"reasons around the gap. Add each to gatedReadTools, to gated-apply/.agents/"+
			"mcp.json's `tools`, and to both legs' permissions.allow.", missing)
	}

	// The converse: a read registered here that the endpoint does not serve
	// is an entry the server will not match, which is a startup WARNING and
	// not an error, so it fails open — the tool is missing and nothing but a
	// log line says so.
	var unknown []string
	for name := range registered {
		if !catalog[name] {
			unknown = append(unknown, name)
		}
	}
	sort.Strings(unknown)
	if len(unknown) > 0 {
		t.Errorf("gatedReadTools names %v, which %s does not list as served by the read-only "+
			"endpoint.\nAn allowlist entry matching no tool is a startup warning, never an "+
			"error, so a typo here costs a read and produces no failure. Either the name is "+
			"wrong, or the catalog enumeration is stale and should be updated first.",
			unknown, catalogDoc)
	}
}

// TestGatedApplyToolNotesTrackTheBaseRecipe keeps the one piece of prose
// that is duplicated across the two MCP configs in sync.
//
// The get_k8s_resource note is a hard-won correction about outputFormat
// selecting fidelity rather than rendering; it cost real turns to discover.
// It applies verbatim to both legs, and a copy is exactly the kind of thing
// that gets improved in one file.
func TestGatedApplyToolNotesTrackTheBaseRecipe(t *testing.T) {
	notes := func(dir string) map[string]any {
		raw := readJSON(t, filepath.Join(dir, "mcp.json"))
		servers, _ := raw["servers"].(map[string]any)
		gke, _ := servers["gke"].(map[string]any)
		n, _ := gke["tool_notes"].(map[string]any)
		return n
	}
	base, gated := notes(agentsDir), notes(gatedAgentsDir)
	if len(base) == 0 {
		t.Fatal(".agents/mcp.json has no tool_notes; this test is checking nothing")
	}
	for name, text := range base {
		if gated[name] != text {
			t.Errorf("tool_notes[%q] differs between the two recipes.\n"+
				"The note describes the tool, not the posture, so it applies to both legs "+
				"verbatim.\n.agents:     %v\ngated-apply: %v", name, text, gated[name])
		}
	}
}

// ── the two configs ──────────────────────────────────────────────────

// TestGatedApplyD1GainsApprovalFieldsWhenThePinAllows is a deferral with an
// expiry, rather than a TODO nobody reads.
//
// D1 is meant to carry permissions.approval_timeout and approval_notify:
// together they are what makes `mode: ask` safe in a pod with nobody
// attached, which is the whole of #647's cluster leg. Both need daemon
// ≥ 2.10.0-dev.1 (examples/internal/recipecheck/minversion.go), and this
// repo has not cut that version — the four overlays pin 2.9.0. Adding the
// fields today raises the recipe's floor above every pin, and recipecheck's
// TestOverlayPinsSatisfyRecipeConfig fails, correctly: an operator running
// this config against the pinned image gets neither field, silently, and
// the symptom is a turn that blocks forever while the session still reports
// `working`.
//
// Note the direction of the union: recipecheck computes ONE floor over every
// config the recipe ships, so a field added here constrains the read-only
// overlays too, even though they never load this file.
//
// So the fields wait for the pin. This test is what stops the wait from
// becoming permanent — the moment the overlays move to a version that
// supports them, it starts failing until D1 declares them.
func TestGatedApplyD1GainsApprovalFieldsWhenThePinAllows(t *testing.T) {
	const gate = "2.10.0-dev.1"

	// The overlay pins are the honest signal: they are what the cluster
	// actually runs, and recipecheck judges the configs against them.
	pins, err := filepath.Glob("deploy/overlays/*/kustomization.yaml")
	if err != nil || len(pins) == 0 {
		t.Fatalf("no overlay kustomizations found (%v); this test's trigger is gone", err)
	}
	pinRE := regexp.MustCompile(`newTag:\s*"?([0-9][^"\s]*)"?`)
	var lowest string
	for _, path := range pins {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, m := range pinRE.FindAllStringSubmatch(string(body), -1) {
			if lowest == "" || semverLess(m[1], lowest) {
				lowest = m[1]
			}
		}
	}
	if lowest == "" {
		t.Fatal("found no newTag pin in any overlay; this test cannot tell when the wait is over")
	}

	raw := readJSON(t, filepath.Join(gatedAgentsDir, "config.d1.json"))
	perms, _ := raw["permissions"].(map[string]any)
	_, hasTimeout := perms["approval_timeout"]
	_, hasNotify := perms["approval_notify"]

	if semverLess(lowest, gate) {
		// Still waiting. Declaring the fields now would turn every overlay
		// red, so assert they are absent — a well-meant "while I'm here"
		// addition is exactly how this breaks.
		if hasTimeout || hasNotify {
			t.Errorf("config.d1.json declares approval_timeout/approval_notify, but the lowest "+
				"overlay pin is %s and both fields need ≥ %s.\nThe recipe's version floor is a "+
				"union over EVERY config it ships, so this makes the read-only overlays fail "+
				"recipecheck too. Cut %s and move the pins first.", lowest, gate, gate)
		}
		return
	}

	if !hasTimeout || !hasNotify {
		t.Errorf("the overlays now pin %s (≥ %s), so the wait is over: add approval_timeout and "+
			"approval_notify back to config.d1.json.\nWithout them D1 is `mode: ask` in a pod "+
			"with nobody attached — it prompts into the void and blocks forever, which is the "+
			"failure #647 exists to prevent and the reason D1 is the approval leg at all.\n"+
			"Intended values: approval_timeout \"10m\", approval_notify \"oncall\". Update the "+
			"leg table in gated-apply/README.md and §config.json in docs/gated-apply-design.md "+
			"in the same change.", lowest, gate)
	}
}

// semverLess orders the subset of semver these pins use: X.Y.Z with an
// optional "-dev.N" pre-release. Enough for a comparison against one
// hardcoded gate, and it treats a release as newer than its pre-releases.
func semverLess(a, b string) bool {
	part := func(v string) ([]int, int) {
		base, pre, hasPre := strings.Cut(v, "-")
		nums := make([]int, 0, 3)
		for _, f := range strings.Split(base, ".") {
			n := 0
			for _, c := range f {
				if c < '0' || c > '9' {
					n = -1
					break
				}
				n = n*10 + int(c-'0')
			}
			nums = append(nums, n)
		}
		for len(nums) < 3 {
			nums = append(nums, 0)
		}
		// A release sorts after every pre-release of the same base, so
		// give it a sentinel above any dev number.
		devN := 1 << 30
		if hasPre {
			devN = 0
			if _, rest, ok := strings.Cut(pre, "."); ok {
				devN = 0
				for _, c := range rest {
					if c >= '0' && c <= '9' {
						devN = devN*10 + int(c-'0')
					}
				}
			}
		}
		return nums[:3], devN
	}
	an, ad := part(a)
	bn, bd := part(b)
	for i := range an {
		if an[i] != bn[i] {
			return an[i] < bn[i]
		}
	}
	return ad < bd
}

// TestGatedApplyConfigsLoadAndValidate runs each leg through the real
// loader.
//
// config.Load reads a fixed filename, so each leg is staged into a temp
// agents dir as config.json. That is worth the copy: Validate is what
// rejects an approval_notify naming a target the alerts block never
// registered, and that mistake would otherwise surface as a daemon that
// refuses to start in the cluster.
func TestGatedApplyConfigsLoadAndValidate(t *testing.T) {
	for _, leg := range gatedLegs() {
		t.Run(leg, func(t *testing.T) {
			body, err := os.ReadFile(filepath.Join(gatedAgentsDir, leg))
			if err != nil {
				t.Fatalf("read %s: %v", leg, err)
			}
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "config.json"), body, 0o600); err != nil {
				t.Fatalf("stage %s: %v", leg, err)
			}
			cfg, err := config.Load(dir)
			if err != nil {
				t.Fatalf("config.Load(%s): %v", leg, err)
			}
			if cfg.Permissions.PlanMode != "required" {
				t.Errorf("plan_mode = %q, want \"required\".\nPlan-first is what keeps "+
					"unattended from meaning unstructured: it runs ahead of the mode check, so "+
					"it still gates the patch in allow mode.", cfg.Permissions.PlanMode)
			}
			if cfg.Safety.Watchdog != "enforce" {
				t.Errorf("safety.watchdog = %q, want \"enforce\"; an agent that can write to a "+
					"cluster is the last place to run the loop backstop in observe-only mode",
					cfg.Safety.Watchdog)
			}
		})
	}
}

// TestGatedApplyLegsDifferOnlyInPermissions pins the experiment.
//
// D1 (approve, then apply) and D2 (apply unattended) exist to isolate one
// variable: whether a human sees the patch first. Every other difference
// between the two files is a confound in whatever scenario D concludes, so
// the permitted delta is the permissions block and the display name — the
// latter only so the two runs are distinguishable in a transcript.
func TestGatedApplyLegsDifferOnlyInPermissions(t *testing.T) {
	d1 := readJSON(t, filepath.Join(gatedAgentsDir, "config.d1.json"))
	d2 := readJSON(t, filepath.Join(gatedAgentsDir, "config.d2.json"))

	if canonical(t, d1["permissions"]) == canonical(t, d2["permissions"]) {
		t.Fatal("the two legs have identical permissions blocks, so they are the same " +
			"experiment run twice")
	}
	for _, m := range []map[string]any{d1, d2} {
		delete(m, "permissions")
		if agent, ok := m["agent"].(map[string]any); ok {
			delete(agent, "display_name")
		}
	}
	if a, b := canonical(t, d1), canonical(t, d2); a != b {
		t.Errorf("config.d1.json and config.d2.json differ outside permissions.\n"+
			"Scenario D attributes its result to the approval gate, which only holds if the "+
			"gate is the only thing that changed.\nd1:\n%s\n\nd2:\n%s", a, b)
	}
}

// TestGatedApplyConfigsTrackTheHubConfig is the drift test against the
// recipe the gated legs were forked from.
//
// config.hub.json is the maintained one — it is what the read-only
// deployment runs and where a fix lands first. The gated legs must inherit
// every such fix: a tightened tools.disable, a lowered cost ceiling, a
// changed subagent tool list. Three deltas are legitimate and everything
// else is drift.
func TestGatedApplyConfigsTrackTheHubConfig(t *testing.T) {
	// normalize strips the three permitted divergences.
	//
	//   permissions        — the entire point of the fork.
	//   agent.display_name — so the leg is identifiable in a transcript.
	//   subagents[0].root  — "../cluster" resolves from .agents/;
	//                        gated-apply/.agents/ is one level deeper, so
	//                        the same shared tree is "../../cluster".
	//                        Asserted properly in the test below.
	normalize := func(m map[string]any) map[string]any {
		delete(m, "permissions")
		if agent, ok := m["agent"].(map[string]any); ok {
			delete(agent, "display_name")
		}
		subs, _ := m["subagents"].([]any)
		for _, s := range subs {
			if sub, ok := s.(map[string]any); ok {
				delete(sub, "root")
			}
		}
		return m
	}

	hub := canonical(t, normalize(readJSON(t, filepath.Join(agentsDir, "config.hub.json"))))
	for _, leg := range gatedLegs() {
		t.Run(leg, func(t *testing.T) {
			got := canonical(t, normalize(readJSON(t, filepath.Join(gatedAgentsDir, leg))))
			if got != hub {
				t.Errorf("%s has drifted from .agents/config.hub.json outside the permitted "+
					"deltas (permissions, agent.display_name, subagents[].root).\n"+
					"A fix applied to the read-only recipe has to reach this leg too, or the "+
					"apply run is testing an older agent.\nconfig.hub.json:\n%s\n\n%s:\n%s",
					leg, hub, leg, got)
			}
		})
	}
}

// TestGatedApplySubagentRootResolvesToTheSharedClusterTree checks the one
// path that the extra directory level breaks.
//
// The subagent root is resolved against the agents dir. gated-apply/.agents
// sits one level deeper than .agents, so the literal that works in one is
// wrong in the other — and wrong quietly: a subagent whose root does not
// exist boots with no persona and no skills, and simply answers worse.
func TestGatedApplySubagentRootResolvesToTheSharedClusterTree(t *testing.T) {
	want, err := filepath.Abs(clusterRoot)
	if err != nil {
		t.Fatalf("abs(%s): %v", clusterRoot, err)
	}
	for _, leg := range gatedLegs() {
		t.Run(leg, func(t *testing.T) {
			raw := readJSON(t, filepath.Join(gatedAgentsDir, leg))
			subs, _ := raw["subagents"].([]any)
			if len(subs) == 0 {
				t.Fatal("no subagents declared")
			}
			sub, _ := subs[0].(map[string]any)
			root, _ := sub["root"].(string)
			if root == "" {
				t.Fatal("subagents[0].root is empty")
			}
			got, err := filepath.Abs(filepath.Join(gatedAgentsDir, root))
			if err != nil {
				t.Fatalf("abs: %v", err)
			}
			if got != want {
				t.Fatalf("subagents[0].root = %q resolves to %s, want %s.\n"+
					"The gated agents dir is one level deeper than .agents, so the base "+
					"recipe's \"../cluster\" points outside the content image here. A root "+
					"that does not exist is not an error — the subagent boots with no persona "+
					"and no skills.", root, got, want)
			}
			if fi, err := os.Stat(got); err != nil || !fi.IsDir() {
				t.Fatalf("resolved subagent root %s is not a directory: %v", got, err)
			}
		})
	}
}

// TestGatedApplyClusterSubagentStaysReadOnly guards the promise in the
// subagent's own description.
//
// That description says it "never mutates cluster state", and #759's rule is
// that a description is a promise the tool catalog has to keep. The subagent
// has its own content root and its own mcp.json, and the parent's switch to
// the full endpoint must not reach it — which it does not, because
// subagents[0].root still points at the shared read-only tree. This asserts
// that rather than assuming it.
func TestGatedApplyClusterSubagentStaysReadOnly(t *testing.T) {
	raw := readJSON(t, filepath.Join(clusterRoot, "mcp.json"))
	servers, _ := raw["servers"].(map[string]any)
	gke, _ := servers["gke"].(map[string]any)
	if got := gke["url"]; got != readOnlyEndpoint {
		t.Errorf("cluster/mcp.json url = %v, want the read-only endpoint %q.\n"+
			"Both legs share this tree. The `cluster` subagent's description promises it never "+
			"mutates cluster state, and that promise is kept by its transport, not by the "+
			"model's restraint.", got, readOnlyEndpoint)
	}
	if ro, _ := gke["read_only"].(bool); !ro {
		t.Error("cluster/mcp.json no longer declares read_only: true")
	}
	if _, present := gke["tools"]; present {
		t.Log("note: cluster/mcp.json now declares a tools allowlist; harmless, but the " +
			"read-only endpoint already bounds this surface")
	}
}

// ── the permission allowlist ─────────────────────────────────────────

// TestGatedApplyAllowlistAuthorizesExactlyTheIntendedCalls is the test that
// would have caught a silently-inert allowlist.
//
// permissions.allow is matched against a key the gate builds as the tool
// name followed by the marshalled arguments (pkg/tools/gate.go
// summarizeRequest), under the "mcp" tool bucket, with `*` meaning
// open-prefix (pkg/permissions/policy.go matchGlob). Get any part of that
// grammar wrong and the entry matches nothing:
//
//   - in D2 (allow mode) every unmatched call is refused, so the agent can
//     neither read nor patch and the leg looks like a model that gave up;
//   - in D1 (ask mode) every unmatched read prompts instead, so an operator
//     who expected to approve one patch approves thirty reads.
//
// Neither failure names the allowlist. So rather than eyeball the patterns,
// this runs them through the real matcher.
func TestGatedApplyAllowlistAuthorizesExactlyTheIntendedCalls(t *testing.T) {
	// args is a realistic marshalled argument blob. The gate appends one to
	// every call that carries arguments, which is every call the model
	// actually makes — a pattern that only matches the bare name is inert
	// in practice while looking right in review.
	const args = ` {"name":"projects/p/locations/l/clusters/c","resourceType":"apps/v1/deployments"}`

	policyFor := func(t *testing.T, leg string) *permissions.Policy {
		t.Helper()
		raw := readJSON(t, filepath.Join(gatedAgentsDir, leg))
		perms, _ := raw["permissions"].(map[string]any)
		entries, _ := perms["allow"].([]any)
		if len(entries) == 0 {
			t.Fatalf("%s declares no permissions.allow entries", leg)
		}
		patterns := make([]string, 0, len(entries))
		for _, e := range entries {
			s, _ := e.(string)
			patterns = append(patterns, s)
		}
		p, err := permissions.NewPolicy(patterns, nil)
		if err != nil {
			t.Fatalf("%s: permissions.NewPolicy: %v", leg, err)
		}
		return p
	}

	for _, leg := range gatedLegs() {
		t.Run(leg, func(t *testing.T) {
			p := policyFor(t, leg)

			// The fifteen reads are allowlisted in BOTH legs, and that is what
			// makes D1 an experiment about the patch rather than about
			// approval fatigue.
			for _, name := range gatedReadTools {
				for _, key := range []string{name, name + args} {
					if got := p.Match("mcp", key); got != permissions.OutcomeAllow {
						t.Errorf("read %q: Match = %v, want allow.\nThe gate's key is the tool "+
							"name plus marshalled args, so the pattern needs the `*` "+
							"open-prefix form.", key, got)
					}
				}
			}

			// Delegation and escalation are gated too, under their own
			// buckets — spawn_agent keys on the subagent name and alert on
			// the target name. In allow mode an agent that cannot spawn
			// `cluster` cannot run the incident path at all.
			if got := p.Match("spawn_agent", "cluster"); got != permissions.OutcomeAllow {
				t.Errorf("spawn_agent:cluster: Match = %v, want allow; without it the parent "+
					"cannot delegate the diagnosis and step 2 of the incident flow dies", got)
			}
			if got := p.Match("alert", "oncall"); got != permissions.OutcomeAllow {
				t.Errorf("alert:oncall: Match = %v, want allow; escalation is the documented "+
					"fallback when the fix is out of reach", got)
			}

			// Nothing allowlists a verb the MCP surface does not register.
			// Belt and braces: mcp.json already bounds the name set, and
			// this catches an entry added here without one added there.
			for _, name := range forbiddenTools {
				if got := p.Match("mcp", name+args); got == permissions.OutcomeAllow {
					t.Errorf("%s is allowlisted in %s; it is not on the MCP surface and must "+
						"not be reachable if it is ever added", name, leg)
				}
			}
		})
	}

	t.Run("the patch is the only difference", func(t *testing.T) {
		d1, d2 := policyFor(t, "config.d1.json"), policyFor(t, "config.d2.json")
		key := mutatingTool + args

		// D1 must leave the patch UNMATCHED rather than denied: unmatched
		// falls through to the mode, and ask mode is what produces the
		// prompt. An explicit deny rule would refuse it outright and there
		// would be nothing for a human to approve.
		if got := d1.Match("mcp", key); got != permissions.OutcomeUnmatched {
			t.Errorf("D1: Match(%q) = %v, want unmatched.\nD1 is the approval leg: the patch "+
				"has to fall through the policy to reach ask mode and prompt. Allowing it "+
				"removes the human; denying it means there is nothing to approve.", key, got)
		}
		if got := d2.Match("mcp", key); got != permissions.OutcomeAllow {
			t.Errorf("D2: Match(%q) = %v, want allow.\nD2 is the unattended leg and allow mode "+
				"refuses anything unmatched — without this entry the agent cannot apply "+
				"anything and the run proves nothing.", key, got)
		}
	})
}

// ── the env manifest ─────────────────────────────────────────────────

// TestGatedApplyEnvManifestMatchesBase keeps the ${env:} surface identical
// across the two content roots.
//
// env.yaml is what switches ${env:VAR} interpolation on at all (#322): with
// no manifest the resolver is nil and every loader passes the body through
// untouched, so `${env:GOOGLE_CLOUD_PROJECT}` reaches the model as literal
// text. Both personas interpolate the same four coordinates — the shared
// text is byte-identical by the persona test above — so the manifests have
// no reason to differ, and a copy that quietly loses a declaration fails the
// same silent way.
func TestGatedApplyEnvManifestMatchesBase(t *testing.T) {
	base, err := os.ReadFile(filepath.Join(agentsDir, "env.yaml"))
	if err != nil {
		t.Fatalf("read base env.yaml: %v", err)
	}
	gated, err := os.ReadFile(filepath.Join(gatedAgentsDir, "env.yaml"))
	if err != nil {
		t.Fatalf("read gated env.yaml: %v; without it every ${env:} reference in "+
			"gated-apply/AGENTS.md reaches the model as literal text", err)
	}
	if string(base) != string(gated) {
		t.Errorf("gated-apply/.agents/env.yaml differs from .agents/env.yaml.\n" +
			"Both personas reference the same coordinates, so the manifests are a straight " +
			"copy; a dropped declaration makes that reference interpolate to nothing and the " +
			"failure is silent.")
	}
}

// TestGatedApplyPlansDirIsPrebaked guards the writable nested mount.
//
// record_plan derives plansDir = agentsDir + "/plans" and MkdirAll's it. The
// content image is a read-only volume, so the deployment overlays a writable
// emptyDir at that exact path — and kubelet cannot create a mount point
// inside a read-only image layer, so the directory has to exist in the
// image. Without it, plan-first is unsatisfiable and, because plan_mode is
// required, the agent cannot make any mutating call at all.
func TestGatedApplyPlansDirIsPrebaked(t *testing.T) {
	plans := filepath.Join(gatedAgentsDir, "plans")
	fi, err := os.Stat(plans)
	if err != nil || !fi.IsDir() {
		t.Fatalf("%s is not a directory: %v", plans, err)
	}
	// Git does not track empty directories, so the directory only survives
	// a clone if something in it does.
	entries, err := os.ReadDir(plans)
	if err != nil {
		t.Fatalf("read %s: %v", plans, err)
	}
	if len(entries) == 0 {
		t.Errorf("%s is empty, so git will not track it and the content image will not "+
			"contain it — add a .gitkeep", plans)
	}
}

// ── the shipping path ────────────────────────────────────────────────

const (
	contentDockerfile = "deploy/content.Dockerfile"
	initCopyPatch     = "deploy/overlays/initcontainer-copy/patch-content-via-initcontainer.yaml"
)

// dockerfileCopyTargets returns the image-root paths deploy/content.Dockerfile
// COPYs, normalized without a trailing slash.
func dockerfileCopyTargets(t *testing.T) []string {
	t.Helper()
	body, err := os.ReadFile(contentDockerfile)
	if err != nil {
		t.Fatalf("read %s: %v", contentDockerfile, err)
	}
	var out []string
	for _, line := range strings.Split(string(body), "\n") {
		fields := strings.Fields(line)
		// COPY <src> <dst>. The recipe uses no --from, --chown or multi-source
		// form; if one ever appears this parse must be revisited rather than
		// silently skipping the layer.
		if len(fields) == 0 || !strings.EqualFold(fields[0], "COPY") {
			continue
		}
		if len(fields) != 3 {
			t.Fatalf("%s: unsupported COPY form %q — this test parses `COPY <src> <dst>` "+
				"only, and skipping the line would turn a real divergence into a pass",
				contentDockerfile, line)
		}
		out = append(out, strings.TrimSuffix(fields[2], "/"))
	}
	sort.Strings(out)
	return out
}

// initContainerCopySources returns the paths the initcontainer-copy overlay's
// `cp -a` reads out of the image, normalized the same way.
func initContainerCopySources(t *testing.T) []string {
	t.Helper()
	body, err := os.ReadFile(initCopyPatch)
	if err != nil {
		t.Fatalf("read %s: %v", initCopyPatch, err)
	}
	// Value stays a raw node: this file's other ops carry a mapping there,
	// and a typed field would fail to unmarshal on them.
	var ops []struct {
		Op    string    `yaml:"op"`
		Path  string    `yaml:"path"`
		Value yaml.Node `yaml:"value"`
	}
	if err := yaml.Unmarshal(body, &ops); err != nil {
		t.Fatalf("unmarshal %s: %v", initCopyPatch, err)
	}
	for _, op := range ops {
		if op.Path != "/spec/template/spec/initContainers" {
			continue
		}
		var containers []struct {
			Name    string   `yaml:"name"`
			Command []string `yaml:"command"`
		}
		if err := op.Value.Decode(&containers); err != nil {
			t.Fatalf("%s: decode initContainers value: %v", initCopyPatch, err)
		}
		if len(containers) == 0 {
			t.Fatalf("%s: the initContainers patch adds no container", initCopyPatch)
		}
		cmd := containers[0].Command
		if len(cmd) < 3 || cmd[0] != "cp" {
			t.Fatalf("%s: initContainer command is %q, not a `cp` — this test reads the "+
				"copy list out of that command", initCopyPatch, cmd)
		}
		// Drop the verb, any flags, and the trailing destination.
		var out []string
		for _, a := range cmd[1 : len(cmd)-1] {
			if strings.HasPrefix(a, "-") {
				continue
			}
			out = append(out, strings.TrimSuffix(a, "/"))
		}
		sort.Strings(out)
		return out
	}
	t.Fatalf("%s declares no initContainers patch", initCopyPatch)
	return nil
}

// TestGatedApplyOverlayMustRemountPlans is a tripwire for the follow-up.
//
// `record_plan` derives its output directory as agentsDir + "/plans" and
// MkdirAll's it (pkg/tools/record_plan.go). The content mount is read-only,
// so `deploy/base` nests a writable emptyDir at exactly
// /opt/gke-platform-agent/.agents/plans — one specific path, chosen for the
// one config the base runs.
//
// Selecting this leg moves agentsDir. `-c <mount>/gated-apply/.agents/
// config.d1.json` puts plansDir at <mount>/gated-apply/.agents/plans, which
// the base does not mount, so it lands on the read-only image volume. The
// failure is not a missing artifact: both legs run `plan_mode: "required"`,
// and plan-first is the structural guarantee the whole gated-apply design
// rests on, so an unwritable plans dir breaks the leg rather than degrading
// it. gated-apply/.agents/plans/.gitkeep pre-bakes the mount POINT — a
// read-only layer cannot have one created at mount time — but a mount point
// is not a mount.
//
// No overlay selects this leg yet; the `-c` swap and the plans remount are
// the same follow-up. This test exists so they cannot land apart, because
// the half that is easy to remember is the half that does not fail in CI.
func TestGatedApplyOverlayMustRemountPlans(t *testing.T) {
	const (
		selector  = gatedRoot + "/.agents/config.d"
		plansPath = gatedRoot + "/.agents/plans"
	)

	var selects, mounts []string
	err := filepath.Walk("deploy", func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".yaml") {
			return err
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(body), selector) {
			selects = append(selects, path)
		}
		// Deliberately matched as a mountPath rather than anywhere in the
		// file: the Dockerfile-adjacent comments name this path too, and a
		// comment is not a mount.
		for _, line := range strings.Split(string(body), "\n") {
			if strings.Contains(line, "mountPath:") && strings.Contains(line, plansPath) {
				mounts = append(mounts, path)
				break
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk deploy/: %v", err)
	}

	switch {
	case len(selects) == 0 && len(mounts) == 0:
		// The shipped state: no overlay runs this leg, so nothing to remount.
		// Documented in gated-apply/README.md §Running it.
	case len(selects) > 0 && len(mounts) == 0:
		t.Errorf("%v point `-c` at the gated-apply leg, but nothing under deploy/ mounts a "+
			"writable volume at %s.\nrecord_plan derives plansDir from agentsDir, so this leg's "+
			"plans land on the read-only content mount and plan-first — which both legs require "+
			"— fails at the first plan. Add a `plans` volumeMount at that path, the way "+
			"deploy/base does for .agents/plans.", selects, plansPath)
	case len(selects) == 0 && len(mounts) > 0:
		t.Errorf("%v mount a writable volume at %s, but nothing points `-c` at the gated-apply "+
			"leg.\nEither the `-c` swap was dropped from the overlay — in which case the "+
			"deployment is still running the read-only recipe and the mount is inert — or this "+
			"path is stale and should be removed.", mounts, plansPath)
	}
}

// TestContentImageShipsEveryRootInBothFlavors pins the one coupling in this
// recipe that nothing else checks.
//
// The content image has two flavors built from one Dockerfile. The scratch
// flavor is mounted whole, so its COPY list IS what the pod sees. The busybox
// flavor is drained into an emptyDir by an initContainer running an explicit
// `cp`, so what the pod sees is that argument list — maintained by hand, in a
// different file, in a different directory.
//
// Both failure directions are quiet in CI and loud in the cluster. A path in
// the `cp` list that the Dockerfile does not COPY makes `cp` exit non-zero,
// which fails the whole command and crash-loops the pod at boot. A path the
// Dockerfile COPYs and the `cp` omits boots a daemon whose content is simply
// missing: the `cluster` subagent's root disappears, or — the case that
// motivated this test — gated-apply/ is absent and `-c` finds no config.
//
// Kustomize renders both flavors happily either way, so this is the only
// place the two lists are compared.
func TestContentImageShipsEveryRootInBothFlavors(t *testing.T) {
	copied := dockerfileCopyTargets(t)
	drained := initContainerCopySources(t)

	if diff := strings.Join(copied, " "); diff != strings.Join(drained, " ") {
		t.Errorf("the content image's COPY list and the initContainer `cp` list disagree.\n"+
			"  %s COPYs:      %v\n"+
			"  %s copies: %v\n"+
			"They must name the same set: a `cp` source the image lacks crash-loops the pod "+
			"at boot, and a COPY the `cp` omits boots a daemon with content missing.",
			contentDockerfile, copied, initCopyPatch, drained)
	}

	// Naming the roots explicitly, so that deleting BOTH lists' entry for a
	// content root still fails — the comparison above would call that
	// agreement.
	for _, want := range []string{"/AGENTS.md", "/.agents", "/cluster", "/" + gatedRoot} {
		if !slices.Contains(copied, want) {
			t.Errorf("%s does not COPY %s", contentDockerfile, want)
		}
		if !slices.Contains(drained, want) {
			t.Errorf("%s does not copy %s out of the image", initCopyPatch, want)
		}
	}
}
