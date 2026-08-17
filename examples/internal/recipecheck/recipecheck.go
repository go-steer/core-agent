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

// Package recipecheck cross-checks a config-only recipe's skill content
// against the tool surface its own config actually produces (#645).
//
// The gke-troubleshoot-agent recipe shipped green while its playbook told
// the model to run `kubectl rollout undo` — with `bash` in the recipe's
// own `tools.disable` list and no MCP tool exposing that verb (#644). The
// existing recipe tests validated *structure* (does the config parse, do
// the skills load) and structure was fine. Nothing checked executability.
//
// This package is the missing check. It answers one question per finding:
// "the content names this — can the agent reach it?" Reachability is not
// re-derived here; it is read off the real registry by running the same
// tools.Default() → Disable(…) → tools.Build(…) sequence cmd/core-agent
// runs at boot, so a change to registration conditions can't drift from
// what the checker believes.
//
// # What is and isn't decidable offline
//
// An MCP server's tool list only exists once you can dial the server, and
// recipe tests run with no credentials and no cluster. So the checker does
// not claim to know whether `gke_get_k8s_resource` exists. It checks the
// things that ARE decidable without a network:
//
//   - A built-in named in content but absent from the built registry —
//     because it's in `tools.disable`, or because a registration
//     precondition isn't met (`fetch_url` needs a URL allowlist, `alert`
//     needs targets). This is the #644 failure exactly.
//
//   - A shell command named as something to run while `bash` is not
//     registered. `kubectl` is a tool reference too; its reachability is
//     `bash`'s. "Named as something to run" is load-bearing: the name has
//     to sit in an *executable position* — inside a shell code fence, on a
//     `$ ` transcript line, or in an inline code span that invokes it —
//     a span reading "kubectl apply -f x.yaml": the command, then an argv.
//     A skill that correctly disclaims the shell — "there is **no**
//     `kubectl` and **no** `gcloud`" — names it with no argv and is a
//     mention, not a step (#766). See executablePosition; the argv
//     requirement, not the fence, is what carries this rule.
//
//   - A double-underscore MCP name (`gke__get_pod`). pkg/mcp/namespace.go
//     joins with ONE underscore, so these match nothing — and an
//     unmatched name is not a config error, so it fails silently at call
//     time (#648).
//
//   - A name in an unambiguous *tool position* — `wait_and_verify`'s
//     `tool:` argument, or a `tools.wait_and_verify.poll_allow` entry —
//     that is neither a registered built-in nor namespaced onto a server
//     the recipe declares. In a tool position there's no ambiguity with
//     config keys, so this can be checked hard.
//
//   - A `wait_and_verify` target in content that is missing from
//     `poll_allow`, which the runtime refuses at call time.
//
//   - A populated `poll_allow` in a recipe whose `wait_and_verify` is not
//     registered at all — a list of assertions about a tool that isn't there.
//
// # Scopes
//
// A recipe is not one tool surface. The parent loads its own skills/ plus
// one tree per `content_roots` entry, and since #619 each declarative
// subagent with a `root` loads a SEPARATE tree — its own AGENTS.md,
// skills/, and mcp.json — that the parent never sees. Content under a
// subagent root is only ever handed to that subagent, so it is checked
// against that subagent's effective catalog: the parent's registry
// narrowed by the `subagents[].tools` allowlist, and the root's OWN
// mcp.json narrowed by `subagents[].mcp`. A skill under `cluster/` that
// names a tool the parent has and the `cluster` subagent does not is
// unreachable in the only place it is ever loaded, and checking it
// against the parent would wave it through (#766).
//
// A subagent root does not itself compose `content_roots`: cmd/core-agent's
// loadSubagentRoot reads <root>/AGENTS.md, <root>/skills/ and
// <root>/mcp.json and never loads a config.json from the root, so there is
// no content_roots list to follow there.
//
// Deliberately NOT checked: bare snake_case tokens in prose. `poll_allow`,
// `require_plan_artifact` and `imagePullSecrets` are shaped exactly like
// tool names, and a checker that guessed would be turned off within a
// week. The tool-position rule buys the same coverage without the guessing.
//
// Also deliberately NOT scanned: AGENTS.md. A hardened persona states its
// limits by naming them — gke-troubleshoot-agent's says "`bash` is disabled
// […] no `kubectl`, no `gcloud`, no `curl`" and lists the four disabled
// write tools — so scanning it yields 8 findings on the recipe #644 exists
// to have fixed. Negation is the dominant idiom there and this checker
// cannot read it, so it stays out. The cost is real and worth stating: a
// promise the persona makes and the config cannot keep is not caught here.
//
// # Two knowing divergences
//
// Rule A gets NO executable-position gate, so a skill that writes "there
// is no `bash` here" still costs a finding, and only rule D forgives the
// disclaimer. That asymmetry is deliberate. The position rule's teeth are
// its argv requirement (see executablePosition), and a built-in has no
// argv: `record_plan` IS the invocation form, so a span rule applied to
// rule A would forgive every real hit too — including the built-in half
// of #644, which is the class this package exists for. Rule D can afford
// the gate because a CLI carries arguments; rule A cannot. Skill content
// phrases built-in limits without naming the tool ("there is no shell to
// fall back to"), which is how the shipped content is written.
//
// subagents[].skills is NOT modeled. That field name-scopes which of the
// root's skills the subagent may load, so a skill file left in the tree
// but absent from the list is scanned here and never loaded at runtime —
// this package over-reports it. Modeling it means parsing SKILL.md
// frontmatter for the declared name, since the list keys on names and not
// paths; until a recipe uses it, over-reporting is the fail-loud
// direction and a dead file in a content root is worth a finding anyway.
//
// # The deploy-time counterpart
//
// Everything above asks whether the recipe's content is executable against
// the tool surface its config produces — on the daemon this repo builds
// today. minversion.go and imagepin.go ask the other half: whether the
// image the recipe's manifests actually ship can produce that surface at
// all (#680). It is the same bug one layer down. pkg/config has no
// DisallowUnknownFields, so a daemon older than a config feature does not
// fail on it; it boots clean, drops the block, and hands the model a skill
// naming tools that were never registered — which is exactly what this
// package's checks are blind to, because they run against HEAD's registry
// and not against the pinned tag's. See CheckDeployPins.
package recipecheck

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/go-steer/core-agent/v2/pkg/config"
	"github.com/go-steer/core-agent/v2/pkg/mcp"
	"github.com/go-steer/core-agent/v2/pkg/permissions"
	"github.com/go-steer/core-agent/v2/pkg/tools"
)

// Recipe is one resolved config root: the directory holding config.json,
// optionally mcp.json, and optionally a skills/ tree.
type Recipe struct {
	// Name identifies the recipe in failure messages. For a config root
	// at examples/foo/deploy/base/config it is "foo (deploy/base/config)".
	Name string
	// Dir is the config root itself — the path you would pass to
	// config.Load / mcp.Load / skills.Load.
	Dir string
}

// Policy tunes a check for one recipe.
type Policy struct {
	// ShellCommands are argv[0]-style names whose presence in skill
	// content means "run this in a shell". Reachable only when `bash` is
	// registered. Empty uses DefaultShellCommands.
	ShellCommands []string

	// WaiveFileGlobs are filepath.Match patterns, matched against the
	// path relative to Dir, whose findings are downgraded to a logged
	// note. Use for vendored content the recipe deliberately does not
	// modify. WaiveReason must be set alongside it.
	//
	// Waived findings are still counted and logged: "we ship 40 unreachable
	// tool references in a vendored snapshot" is a fact a reviewer should
	// see, not one the test should swallow.
	WaiveFileGlobs []string
	// WaiveReason explains why WaiveFileGlobs is justified. Required when
	// WaiveFileGlobs is non-empty; the checker errors without it, so a
	// waiver can never be added silently.
	WaiveReason string

	// WaiveMinFindings is a per-glob floor, keyed by an entry of
	// WaiveFileGlobs: Check errors when that tree yields FEWER findings
	// than the floor.
	//
	// A waiver is an assertion about a body of content ("these 18 vendored
	// skills are Hermes-shaped and we accept that"), and an assertion whose
	// subject can silently disappear is worse than no assertion. That is
	// literally #766: kube-platform-agent moved six skills under a subagent
	// root, the checker stopped seeing them, no test failed, and the waiver
	// text went on claiming to cover both trees for months. A floor turns
	// the next such move into a red build instead of a quieter number.
	//
	// Set it below the current count, not at it — the point is to catch a
	// tree going dark, not to pin vendored content byte-for-byte.
	WaiveMinFindings map[string]int
}

// DefaultShellCommands is the set of CLIs a Kubernetes/cloud playbook
// reaches for by reflex. Each one is a promise the agent cannot keep
// unless `bash` is in its catalog.
var DefaultShellCommands = []string{
	"kubectl", "gcloud", "helm", "docker", "terraform", "curl", "istioctl",
}

// Finding is one named-but-unreachable tool reference.
type Finding struct {
	File   string // path relative to the config root
	Line   int    // 1-indexed
	Name   string // the tool or command named
	Reason string // why it is unreachable
	Waived bool   // matched a Policy.WaiveFileGlobs pattern
}

func (f Finding) String() string {
	return fmt.Sprintf("%s:%d: %q is named but not reachable: %s", f.File, f.Line, f.Name, f.Reason)
}

// Discover walks examplesDir and returns every config root under it, in
// stable order. A config root is a directory holding config.json that
// also holds at least one of mcp.json, AGENTS.md, or skills/ — which
// distinguishes a recipe's agents dir from an unrelated config.json.
func Discover(examplesDir string) ([]Recipe, error) {
	var out []Recipe
	err := filepath.WalkDir(examplesDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		if _, statErr := os.Stat(filepath.Join(path, "config.json")); statErr != nil {
			return nil
		}
		var marker bool
		for _, sibling := range []string{"mcp.json", "AGENTS.md", "skills"} {
			if _, statErr := os.Stat(filepath.Join(path, sibling)); statErr == nil {
				marker = true
				break
			}
		}
		if !marker {
			return nil
		}
		rel, relErr := filepath.Rel(examplesDir, path)
		if relErr != nil {
			rel = path
		}
		out = append(out, Recipe{Name: filepath.ToSlash(rel), Dir: path})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Check returns every unreachable tool reference in r's skill content.
// A nil error with an empty slice means the recipe is executable as
// written, as far as anything decidable without a live MCP server goes.
//
// "r's skill content" spans every scope the recipe loads — its own
// skills/, each content_roots tree, and each rooted subagent's tree — with
// each scope judged against the catalog it is actually loaded with. See the
// Scopes section of the package doc.
func Check(r Recipe, p Policy) ([]Finding, error) {
	if len(p.WaiveFileGlobs) > 0 && strings.TrimSpace(p.WaiveReason) == "" {
		return nil, fmt.Errorf("recipecheck: %s: WaiveFileGlobs is set without a WaiveReason", r.Name)
	}
	shellCmds := p.ShellCommands
	if len(shellCmds) == 0 {
		shellCmds = DefaultShellCommands
	}

	cfg, err := config.Load(r.Dir)
	if err != nil {
		return nil, fmt.Errorf("recipecheck: %s: config.Load: %w", r.Name, err)
	}
	scopes, err := buildScopes(r.Dir, cfg)
	if err != nil {
		return nil, fmt.Errorf("recipecheck: %s: %w", r.Name, err)
	}

	var findings []Finding
	add := func(f Finding) {
		f.Waived = waived(f.File, p.WaiveFileGlobs)
		findings = append(findings, f)
	}

	// Config-level rules run against the parent scope only: config.json is
	// the parent's, and a rooted subagent contributes no config of its own.
	checkConfig(cfg, scopes[0], add)

	pollAllow := make(map[string]bool, len(cfg.Tools.WaitAndVerify.PollAllow))
	for _, n := range cfg.Tools.WaitAndVerify.PollAllow {
		pollAllow[n] = true
	}

	// Scopes can overlap (two subagents sharing a root, a subagent rooted at
	// the agents dir). Each is scanned in full against its own catalog and
	// the results are merged on finding identity, so a shared file costs one
	// finding per DISTINCT verdict rather than one per scope. Within a single
	// scope duplicates are kept, preserving the pre-#766 count for the
	// single-scope recipes that are the overwhelming majority.
	emitted := map[Finding]bool{}
	novel := func(f Finding) {
		if emitted[identity(f)] {
			return
		}
		add(f)
	}
	for i, sc := range scopes {
		collect := add
		if i > 0 {
			collect = novel
		}
		for _, path := range sc.files {
			body, readErr := os.ReadFile(filepath.Join(r.Dir, path))
			if readErr != nil {
				return nil, fmt.Errorf("recipecheck: %s: read %s: %w", r.Name, path, readErr)
			}
			scanContent(path, string(body), sc, shellCmds, pollAllow, collect)
		}
		for _, f := range findings {
			emitted[identity(f)] = true
		}
	}

	sort.SliceStable(findings, func(i, j int) bool {
		if findings[i].File != findings[j].File {
			return findings[i].File < findings[j].File
		}
		return findings[i].Line < findings[j].Line
	})
	if err := checkWaiverFloors(r.Name, p, findings); err != nil {
		return nil, err
	}
	return findings, nil
}

// checkWaiverFloors enforces Policy.WaiveMinFindings: a waived tree that
// stops producing findings has almost certainly stopped being scanned.
func checkWaiverFloors(name string, p Policy, findings []Finding) error {
	if len(p.WaiveMinFindings) == 0 {
		return nil
	}
	globs := make([]string, 0, len(p.WaiveMinFindings))
	for g := range p.WaiveMinFindings {
		globs = append(globs, g)
	}
	sort.Strings(globs)
	for _, g := range globs {
		floor := p.WaiveMinFindings[g]
		var n int
		for _, f := range findings {
			if waived(f.File, []string{g}) {
				n++
			}
		}
		if n < floor {
			return fmt.Errorf("recipecheck: %s: waived tree %q produced %d finding(s), below the floor of %d — "+
				"either the tree moved and is no longer being scanned (the #766 failure), or it was fixed and the floor should come down",
				name, g, n, floor)
		}
	}
	return nil
}

// identity reduces a Finding to what makes two reports the same report:
// where it is and what it names. The reason is excluded deliberately — when
// two scopes both flag a line, they agree on the verdict and differ only in
// how much detail they can give about it.
func identity(f Finding) Finding {
	return Finding{File: f.File, Line: f.Line, Name: f.Name}
}

// Unwaived filters to the findings that should fail a build.
func Unwaived(in []Finding) []Finding {
	var out []Finding
	for _, f := range in {
		if !f.Waived {
			out = append(out, f)
		}
	}
	return out
}

// --- scopes ---

// scope pairs one body of content with the tool catalog that content is
// actually loaded against. A recipe has at least one (its own skills/ plus
// every content_roots tree, all resolved against the parent config) and one
// more per declarative subagent that carries a `root`.
//
// Keeping the catalog on the scope rather than on the Recipe is the whole
// point of #766: a rooted subagent narrows both dimensions — built-ins by
// the `subagents[].tools` allowlist, MCP servers to the root's own mcp.json
// filtered by `subagents[].mcp` — so its content has to be judged against
// what IT can call, not against what the parent can.
type scope struct {
	// files are markdown paths relative to the config root, in stable order.
	files []string
	// registered is the effective built-in catalog for this scope.
	registered map[string]bool
	// servers are the MCP servers whose namespaces resolve in this scope.
	servers mcp.Servers
	// missing explains why a built-in is absent from this scope's catalog.
	missing func(name string) string
}

// buildScopes resolves the parent scope plus one scope per rooted subagent.
// The parent is always scopes[0].
//
// Every scope keeps its FULL file list, even where two scopes cover the same
// tree. Deduplication happens on the finding, not on the file (see Check):
// a file shared by two scopes is scanned once per distinct catalog, the
// identical findings collapse, and anything visible only to the narrower
// catalog survives. An earlier cut claimed the file for the first scope that
// walked it, which is #766's own bug one level down — two subagents sharing
// a root with different `tools` lists, or a subagent rooted at `.`, produced
// a confident clean answer about content that is unreachable where it is
// actually loaded.
func buildScopes(dir string, cfg *config.Config) ([]scope, error) {
	servers, err := mcp.Load(dir)
	if err != nil {
		return nil, fmt.Errorf("mcp.Load: %w", err)
	}
	registered, err := registeredBuiltins(cfg, dir)
	if err != nil {
		return nil, err
	}
	files, err := skillFiles(dir, cfg.ContentRoots)
	if err != nil {
		return nil, err
	}
	out := []scope{{
		files:      files,
		registered: registered,
		servers:    servers,
		missing:    func(name string) string { return notRegisteredReason(name, cfg) },
	}}
	for _, spec := range cfg.Subagents {
		if spec.Root == "" {
			continue
		}
		sc, err := subagentScope(dir, cfg, spec, registered)
		if err != nil {
			return nil, err
		}
		out = append(out, sc)
	}
	return out, nil
}

// subagentScope resolves the scope for one rooted subagent, mirroring
// cmd/core-agent's loadSubagentRoot + resolveSubagentTools +
// resolveSubagentToolsets: a relative root joins the agents dir, the root
// supplies its own mcp.json and skills/, and the nil/list/empty contract
// applies per dimension (nil inherits, a list scopes, an explicit empty
// list grants none).
func subagentScope(dir string, cfg *config.Config, spec config.SubagentSpec, parentRegistered map[string]bool) (scope, error) {
	root := spec.Root
	if !filepath.IsAbs(root) {
		root = filepath.Join(dir, root)
	}
	root = filepath.Clean(root)
	info, err := os.Stat(root)
	if err != nil {
		return scope{}, fmt.Errorf("subagents[%q].root: %w", spec.Name, err)
	}
	if !info.IsDir() {
		return scope{}, fmt.Errorf("subagents[%q].root %q is not a directory", spec.Name, root)
	}
	// The root's OWN mcp.json — no parent overlay, matching loadSubagentRoot.
	servers, err := mcp.Load(root)
	if err != nil {
		return scope{}, fmt.Errorf("subagents[%q].root: mcp.Load: %w", spec.Name, err)
	}
	files, err := markdownFiles(dir, []string{root})
	if err != nil {
		return scope{}, fmt.Errorf("subagents[%q].root: %w", spec.Name, err)
	}
	registered, err := scopedBuiltins(parentRegistered, spec.Tools)
	if err != nil {
		return scope{}, fmt.Errorf("subagents[%q]: %w", spec.Name, err)
	}
	scopedMCP, err := scopedServers(servers, spec.MCP)
	if err != nil {
		return scope{}, fmt.Errorf("subagents[%q]: %w", spec.Name, err)
	}
	return scope{
		files:      files,
		registered: registered,
		servers:    scopedMCP,
		missing: func(name string) string {
			return subagentMissingReason(name, spec, cfg, parentRegistered)
		},
	}, nil
}

// scopedBuiltins applies the `subagents[].tools` allowlist to the parent's
// registry. Built-ins live in the binary, not in a content root, so the
// subagent's ceiling is always the parent's registry: an allowlist can only
// take away. nil inherits the parent whole; an explicit empty list grants
// none.
//
// A name the parent registry does not hold is an ERROR, not a silent drop,
// because that is what the runtime does: cmd/core-agent's
// resolveSubagentTools returns `unknown tool %q` and the boot exits
// ExitConfigError. `tools.disable: ["write_file"]` alongside
// `subagents[0].tools: ["write_file"]` is a recipe that cannot start, and a
// checker that reported it clean would be making exactly the promise this
// package exists to stop making.
func scopedBuiltins(parent map[string]bool, allow []string) (map[string]bool, error) {
	if allow == nil {
		return parent, nil
	}
	out := make(map[string]bool, len(allow))
	for _, name := range allow {
		if !parent[name] {
			return nil, fmt.Errorf("tools: unknown tool %q — the parent registry does not hold it, "+
				"so cmd/core-agent refuses this config at boot", name)
		}
		out[name] = true
	}
	return out, nil
}

// scopedServers applies the `subagents[].mcp` selection to the root's own
// servers, with the same nil/list/empty contract — and the same hard failure
// on an unmatched name, mirroring resolveSubagentToolsets' `mcp: unknown
// server %q`. With a root set, the names resolve against the ROOT's mcp.json,
// so a name that only the parent declares is a boot error too.
func scopedServers(all mcp.Servers, allow []string) (mcp.Servers, error) {
	if allow == nil {
		return all, nil
	}
	out := mcp.Servers{Servers: make(map[string]mcp.ServerSpec, len(allow))}
	for _, name := range allow {
		spec, ok := all.Servers[name]
		if !ok {
			return mcp.Servers{}, fmt.Errorf("mcp: unknown server %q (not in the root's mcp.json)", name)
		}
		out.Servers[name] = spec
	}
	return out, nil
}

// subagentMissingReason distinguishes "the recipe never registers this" from
// "the parent registers it and this subagent does not" — different bugs with
// different fixes, and only the second one is invisible from the parent.
func subagentMissingReason(name string, spec config.SubagentSpec, cfg *config.Config, parentRegistered map[string]bool) string {
	if !parentRegistered[name] {
		return notRegisteredReason(name, cfg)
	}
	return fmt.Sprintf("the parent registers it, but subagents[%q].tools is an allowlist that omits it — "+
		"and this content is only ever loaded under that subagent's root", spec.Name)
}

// --- reachability ---

// registeredBuiltins runs the same assembly cmd/core-agent runs at boot —
// tools.Default(), then cfg.Tools.Disable, then tools.Build — and returns
// the names that actually made it into the registry. Reading the answer
// off the real builder is the point: `fetch_url` and `alert` are gated on
// config preconditions beyond the disable list, and record_plan on the
// agents dir, and a hand-maintained copy of those rules would drift.
func registeredBuiltins(cfg *config.Config, agentsDir string) (map[string]bool, error) {
	b := tools.Default()
	for _, name := range cfg.Tools.Disable {
		if err := b.Disable(name); err != nil {
			return nil, fmt.Errorf("tools.disable: %w", err)
		}
	}
	// A gate is required but never consulted: Build only stores it in the
	// tool closures, and we never call a tool.
	gate, err := permissions.FromConfig(cfg, agentsDir, "", nil)
	if err != nil {
		return nil, fmt.Errorf("permissions.FromConfig: %w", err)
	}
	reg, err := tools.Build(cfg, gate, agentsDir, b)
	if err != nil {
		return nil, fmt.Errorf("tools.Build: %w", err)
	}
	out := make(map[string]bool, len(reg.Tools))
	for _, t := range reg.Tools {
		out[t.Name()] = true
	}
	// One deliberate divergence from boot: Build also drops alert targets
	// whose url_env is unset in the CURRENT process, so a recipe checked
	// on a laptop or in CI — neither of which has the deployment's
	// webhook Secret — would report `alert` unreachable for every recipe
	// that configures one. That is a property of the checking
	// environment, not of the recipe. This check answers "is the recipe
	// self-consistent", so registering a target is enough; whether the
	// Secret is mounted is the deployment's problem, and cmd/core-agent
	// warns about it at boot.
	if b.Alert && len(cfg.Alerts.Targets) > 0 {
		out["alert"] = true
	}
	return out, nil
}

// unreachableToolName decides whether a name used in a tool position can
// resolve at runtime. Returns ("", true) when it can.
func unreachableToolName(name string, registered map[string]bool, servers mcp.Servers) (string, bool) {
	if registered[name] {
		return "", true
	}
	if strings.Contains(name, "__") {
		return "double underscore: pkg/mcp/namespace.go joins server and tool with a SINGLE underscore", false
	}
	for server := range servers.Servers {
		if strings.HasPrefix(name, server+"_") {
			// The server's actual tool list needs a live connection; the
			// namespace is as far as an offline check goes.
			return "", true
		}
	}
	if isBuiltinName(name) {
		return notRegisteredReasonShort(name), false
	}
	return fmt.Sprintf("not a registered built-in and not namespaced onto any declared MCP server %v",
		serverNames(servers)), false
}

func isBuiltinName(name string) bool {
	for _, n := range tools.BuiltinToolNames() {
		if n == name {
			return true
		}
	}
	return false
}

func notRegisteredReasonShort(name string) string {
	return fmt.Sprintf("built-in %q is not registered for this recipe", name)
}

// notRegisteredReason explains WHY a built-in is missing, which is the
// difference between "delete this line" and "fix the config".
func notRegisteredReason(name string, cfg *config.Config) string {
	for _, d := range cfg.Tools.Disable {
		if d == name {
			return "the recipe's own tools.disable list removes it from the catalog"
		}
	}
	switch name {
	case "fetch_url":
		return "fetch_url only registers when url_scope.allow is non-empty; this recipe sets no allowlist"
	case "alert":
		return "alert only registers when alerts.targets is non-empty; this recipe registers no target"
	case "record_plan":
		if !cfg.Permissions.PlanToolRegistered() {
			return "record_plan only registers when permissions.plan_mode is \"advisory\" or \"required\"; " +
				"without it there is no plan artifact and no gate flag to flip, so a skill telling the model to call it is dead instruction"
		}
		return "record_plan also needs a resolved agents dir to persist plans into"
	case "sciontool_status":
		return "sciontool_status only registers inside a Scion container (sciontool on PATH)"
	}
	return "it is not in the registry this recipe's config produces"
}

func serverNames(s mcp.Servers) []string {
	out := make([]string, 0, len(s.Servers))
	for name := range s.Servers {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// --- content scanning ---

// checkConfig runs the two config-only rules against the parent scope.
//
// Rule G: poll_allow only means anything if wait_and_verify is in the
// catalog. A populated list under a disabled tool is a set of read-only
// assertions about a tool nobody can call — dead config that reads like a
// capability.
//
// Rule E: poll_allow entries must name something reachable. This one is
// config-only, so it runs even for a recipe with no skills.
func checkConfig(cfg *config.Config, parent scope, add func(Finding)) {
	allow := cfg.Tools.WaitAndVerify.PollAllow
	if len(allow) > 0 && !parent.registered["wait_and_verify"] {
		add(Finding{File: "config.json", Line: 0, Name: "wait_and_verify",
			Reason: fmt.Sprintf("tools.wait_and_verify.poll_allow lists %d tool(s) but %s",
				len(allow), notRegisteredReason("wait_and_verify", cfg))})
	}
	for _, name := range allow {
		if reason, ok := unreachableToolName(name, parent.registered, parent.servers); !ok {
			add(Finding{File: "config.json", Line: 0, Name: name, Reason: reason +
				" — an unmatched poll_allow entry is not a config error, so the poll is silently refused at call time"})
		}
	}
}

// lineRef is one scanned line plus the facts the rules need about it:
// where it is, whether the whole line sits in a shell context, and the
// inline code spans on it.
type lineRef struct {
	file string
	no   int
	text string
	// shellLine is true inside a shell code fence or on a `$ `-prefixed
	// line: every CLI name on such a line is an instruction. See
	// fenceTracker and shellPrompt.
	shellLine bool
	// spans are the inline code spans on the line, in order, contents only
	// (no backticks). Parsed once per line rather than once per candidate
	// command. See invokesInSpan.
	spans []string
}

// scanContent applies the per-line rules to one markdown file, in the
// catalog of the scope that file belongs to.
func scanContent(path, body string, sc scope, shellCmds []string, pollAllow map[string]bool, add func(Finding)) {
	var fence fenceTracker
	lines := strings.Split(body, "\n")
	for i, text := range lines {
		if fence.track(text) {
			// The delimiter itself is punctuation, not content — with one
			// exception. An explicitly shell-labeled fence is a claim in its
			// own right ("what follows is a shell session"), and it is the
			// claim the whole check exists to catch when `bash` is gone. It
			// also covers the commands rule D's list does not name.
			if fence.openedLabeledShell() && !sc.registered["bash"] {
				add(Finding{File: path, Line: i + 1, Name: "bash",
					Reason: "the content opens a shell code fence, but " + sc.missing("bash")})
			}
			continue
		}
		l := lineRef{file: path, no: i + 1, text: text,
			shellLine: fence.inShell() || shellPrompt.MatchString(text),
			spans:     codeSpansFrom(lines, i)}
		ruleNamedBuiltin(l, sc, add)
		ruleShellCommand(l, sc, shellCmds, add)
		ruleDoubleUnderscore(l, add)
		ruleToolPosition(l, sc, pollAllow, add)
	}
}

// ruleNamedBuiltin (rule A) flags a built-in the content names but the
// registry this scope produces does not contain.
func ruleNamedBuiltin(l lineRef, sc scope, add func(Finding)) {
	for _, name := range tools.BuiltinToolNames() {
		if sc.registered[name] || !namesTool(l.text, name) {
			continue
		}
		add(Finding{File: l.file, Line: l.no, Name: name, Reason: sc.missing(name)})
	}
}

// ruleShellCommand (rule D) flags a CLI invocation when `bash` is not in
// the catalog. Only an executable position counts — see executablePosition.
func ruleShellCommand(l lineRef, sc scope, shellCmds []string, add func(Finding)) {
	if sc.registered["bash"] {
		return
	}
	for _, cmd := range shellCmds {
		if !namesCommand(l.text, cmd) || !executablePosition(l, cmd) {
			continue
		}
		add(Finding{File: l.file, Line: l.no, Name: cmd,
			Reason: "`bash` is not in the tool catalog, so the agent has no way to run a CLI"})
	}
}

// executablePosition decides whether cmd, which appears somewhere on l, is
// being given to the model as a step to run. It is a three-way union:
//
//	(a) the whole line is shell — inside a shell code fence, or a `$ `
//	    transcript line;
//	(b) an inline code span that INVOKES the command: its first token is the
//	    command and it carries at least one further token; or
//	(c) nothing, in which case the name is a mention and not a finding.
//
// (b) is the discriminator that matters. Real runbook content puts commands
// in prose — numbered steps and markdown table cells — far more often than
// in fences: the pre-#644 gke-troubleshoot tree this package was written for
// has 73 CLI references and not one of them is in a shell fence. An earlier
// cut of #766 gated on (a) alone and dropped every one of them, which would
// have retired the check while leaving it green. Meanwhile a lone `kubectl`
// span — the shape a correct disclaimer uses, "there is **no** `kubectl` and
// **no** `gcloud`" — carries no argv and stays a mention. The argv is the
// whole signal: an instruction says what to run the command ON.
//
// The cost, stated plainly. An argv-less imperative is missed: the pre-#644
// tree's "degrade gracefully to `bash` + `kubectl`" (SKILL.md:75, :153) is a
// real instruction this rule lets through. So is an invocation the author
// forgot to format at all — kube-platform-agent's gke-productionize step at
// SKILL.md:41 is a bare `kubectl get pods -n <ns> …` in running text, and
// the six neighbouring steps that ARE backticked are all caught. Widening
// to unformatted text is what the rule cannot afford: "via `gcloud` or
// Terraform" and "issued zero `kubectl`" would both come back as findings,
// and those are the shape #766 was filed about.
func executablePosition(l lineRef, cmd string) bool {
	if l.shellLine {
		return true
	}
	for _, span := range l.spans {
		if invokesInSpan(span, cmd) {
			return true
		}
	}
	return false
}

// invokesInSpan reports whether an inline code span is an invocation of cmd:
// first token is the command, and at least one argument follows. A leading
// `$ ` prompt inside the span is tolerated, as is a directory prefix
// (`/usr/bin/kubectl get pods`), because both still read as "run this".
func invokesInSpan(span, cmd string) bool {
	fields := strings.Fields(span)
	if len(fields) > 0 && fields[0] == "$" {
		fields = fields[1:]
	}
	if len(fields) < 2 {
		return false
	}
	head := fields[0]
	if i := strings.LastIndex(head, "/"); i >= 0 {
		head = head[i+1:]
	}
	return head == cmd
}

// codeSpansFrom returns the contents of the inline code spans that BEGIN on
// lines[i], backticks stripped.
//
// It follows a span that wraps onto the next source line, because runbook
// prose wraps at 72 columns and its commands are long: the pre-#644
// gke-troubleshoot tree writes `kubectl get events --field-selector
// involvedObject.kind=Node` across two lines, and a line-at-a-time matcher
// silently loses it. That would be an implementation gap wearing a rule's
// clothes. Joining follows CommonMark — the newline is a space, and a blank
// line ends the paragraph, so a span cannot cross one.
func codeSpansFrom(lines []string, i int) []string {
	var out []string
	line := lines[i]
	for pos := 0; pos < len(line); {
		open := strings.IndexByte(line[pos:], '`')
		if open < 0 {
			return out
		}
		open += pos
		n := backtickRun(line, open)
		body, endLine, endCol, ok := closeCodeSpan(lines, i, open+n, n)
		if !ok {
			return out
		}
		out = append(out, body)
		if endLine != i {
			// The span ran past the end of this line; nothing else can
			// start on it, and the closing line owns no opener of its own.
			return out
		}
		pos = endCol
	}
	return out
}

// backtickRun returns the length of the backtick run starting at pos.
func backtickRun(s string, pos int) int {
	n := 0
	for pos+n < len(s) && s[pos+n] == '`' {
		n++
	}
	return n
}

// closeCodeSpan scans forward from lines[i][col] for a backtick run of
// exactly n and returns the span's contents plus the position just past the
// closing run. Runs of a different length are span content, per CommonMark.
// An unterminated span is not a span: the scan gives up at the end of the
// paragraph (a blank line or a fence delimiter) and reports !ok.
func closeCodeSpan(lines []string, i, col, n int) (body string, endLine, endCol int, ok bool) {
	var b strings.Builder
	for ln := i; ln < len(lines); ln++ {
		s := lines[ln]
		if ln > i {
			if strings.TrimSpace(s) == "" || codeFence.MatchString(s) {
				return "", 0, 0, false
			}
			b.WriteByte(' ')
			col = 0
		}
		for c := col; c < len(s); c++ {
			if s[c] != '`' {
				continue
			}
			run := backtickRun(s, c)
			if run == n {
				b.WriteString(s[col:c])
				return b.String(), ln, c + run, true
			}
			b.WriteString(s[col : c+run])
			col, c = c+run, c+run-1
		}
		b.WriteString(s[col:])
	}
	return "", 0, 0, false
}

// ruleDoubleUnderscore (rule B) flags a doubled namespace separator, which
// matches no server anywhere.
func ruleDoubleUnderscore(l lineRef, add func(Finding)) {
	for _, m := range doubleUnderscore.FindAllString(l.text, -1) {
		add(Finding{File: l.file, Line: l.no, Name: m,
			Reason: "pkg/mcp/namespace.go joins server and tool with a SINGLE underscore, so no server exposes this name"})
	}
}

// ruleToolPosition (rule C) flags a name in an unambiguous tool slot that
// this scope cannot resolve, and (rule F) a resolvable wait_and_verify
// target missing its poll_allow assertion.
func ruleToolPosition(l lineRef, sc scope, pollAllow map[string]bool, add func(Finding)) {
	for _, m := range toolPosition.FindAllStringSubmatch(l.text, -1) {
		name := m[1]
		if reason, ok := unreachableToolName(name, sc.registered, sc.servers); !ok {
			add(Finding{File: l.file, Line: l.no, Name: name, Reason: reason})
			continue
		}
		if !sc.registered[name] && !pollAllow[name] {
			add(Finding{File: l.file, Line: l.no, Name: name,
				Reason: "wait_and_verify polls it but it is not in tools.wait_and_verify.poll_allow; " +
					"MCP tools never self-classify read-only, so the call is refused"})
		}
	}
}

// shellPrompt matches a transcript-style prompt line — `$ kubectl get pods`
// — which is an instruction to run something even outside a fence.
var shellPrompt = regexp.MustCompile(`^\s*\$ `)

// codeFence matches a markdown fence delimiter and captures its run of
// backticks/tildes plus the first word of the info string.
//
// The leading class allows ARBITRARY indentation and blockquote markers,
// not CommonMark's bare-paragraph `{0,3}` limit. A fence nested in a list
// item is indented to the item's content column, and a `\s{0,3}` pattern
// misses it: `upstream/skills/gke-manifest-generation/SKILL.md:109` opens a
// 5-space-indented ```bash block and
// `cluster/skills/gke-workload-troubleshooting/SKILL.md:67` a 4-space one.
// Missing them was worse than a plain false negative — the unrecognized
// delimiter fell through to rule A, which matched the word `bash` on it and
// emitted a finding with the wrong reason, so the count looked healthy while
// the commands inside went unscanned.
//
// The cost of being permissive is that a genuinely indented (4-space) code
// block whose first line happens to be ``` is read as a fence. That
// mislabels a block as shell rather than missing one, which is the safe
// direction for a gate, and there is no instance of it in the scanned trees.
var codeFence = regexp.MustCompile("^[\\s>]*(`{3,}|~{3,})\\s*([A-Za-z0-9_+.-]*)")

// fenceTracker carries code-fence state across the lines of one file so a
// rule can ask whether the current line is inside a shell block.
//
// This is what makes rule D a check on *instructions* rather than on
// vocabulary. Before #766 a skill that told the truth about its runtime —
// "there is **no** `kubectl` here, use the `gke` MCP" — produced two
// findings, so the gate punished exactly the content it exists to
// encourage.
type fenceTracker struct {
	open  bool
	shell bool
	info  string // the fence's language label, "" when unlabeled
	// marker is "`" or "~", so a ~~~ block isn't closed by a ```; length is
	// the opener's run length, so a ```` block isn't closed by an inner ```
	// (CommonMark: the closer must be at least as long as the opener). No
	// >3-char fence ships in the tree today, but getting it wrong would not
	// merely miss a block — it would close early and then re-open inverted,
	// mislabeling the rest of the file.
	marker string
	length int
}

// track updates the tracker and reports whether line was a fence delimiter.
func (f *fenceTracker) track(line string) bool {
	m := codeFence.FindStringSubmatch(line)
	if m == nil {
		return false
	}
	run := m[1]
	if f.open {
		if strings.HasPrefix(run, f.marker) && len(run) >= f.length {
			f.open, f.shell, f.info, f.marker, f.length = false, false, "", "", 0
			return true
		}
		return false
	}
	f.open, f.shell, f.info = true, isShellInfoString(m[2]), m[2]
	f.marker, f.length = run[:1], len(run)
	return true
}

// inShell reports whether the current line sits inside a shell code block.
func (f *fenceTracker) inShell() bool { return f.open && f.shell }

// openedLabeledShell reports whether the delimiter just consumed opened a
// fence that NAMES a shell (```bash, ```console). An unlabeled fence does
// not count: markdown uses it for captured output and manifests at least as
// often as for commands, so treating every ``` as a shell claim would bury
// the signal. Commands inside an unlabeled fence are still rule D's job.
func (f *fenceTracker) openedLabeledShell() bool { return f.open && f.shell && f.info != "" }

// isShellInfoString decides whether a fence's info string means "these are
// commands", for the purpose of reading the lines INSIDE it as shell.
//
// An empty info string counts. Not on historical grounds — an earlier draft
// of this comment claimed the pre-#644 gke-troubleshoot tree wrote its
// `kubectl rollout undo` in unlabeled fences, and that is false: all six of
// that tree's unlabeled fences hold JSON, `wait_and_verify(...)` pseudocode
// or an incident-report template, and its commands are in table cells. It
// counts because an unlabeled fence holding a line that STARTS with a known
// CLI is a command block whatever the author labeled it, and rule D fires
// only on such a line. The cost is bounded and measured: across both scanned
// trees, treating unlabeled fences as shell yields no false positive today.
//
// A fence labeled yaml/json/text does not count — a `kubectl` inside a
// manifest or a captured log is quoted output, not a step. Note that this
// governs fence CONTENTS only; whether the delimiter itself is a finding is
// openedLabeledShell's separate, stricter question.
func isShellInfoString(info string) bool {
	switch strings.ToLower(info) {
	case "", "bash", "sh", "shell", "zsh", "ksh", "console", "shell-session", "shellsession", "terminal", "cmd":
		return true
	}
	return false
}

// toolPosition matches a name in a syntactic tool slot: the `tool:`
// argument of a wait_and_verify block, as the references write it.
var toolPosition = regexp.MustCompile(`\btool:\s*"([A-Za-z][A-Za-z0-9_]*)"`)

// doubleUnderscore matches an identifier carrying a doubled namespace
// separator, e.g. `gke__get_pod`.
var doubleUnderscore = regexp.MustCompile(`\b[a-z][a-z0-9]*__[a-z][a-z0-9_]*`)

// namesTool reports whether line refers to the named built-in as a tool.
// Requires a word boundary on both sides so `bash` matches in "run `bash`"
// and "bash rm" but not in "bashful" or "/bin/bash-completion".
func namesTool(line, name string) bool {
	return wordRe(name).MatchString(line)
}

// namesCommand is namesTool for CLIs: the same word-boundary rule, kept
// separate because the reason string and the reachability question differ.
//
// It says nothing about WHERE on the page the name appears — that gate is
// lineRef.executable, applied by ruleShellCommand. Splitting the two keeps
// "is this the word kubectl" and "is this a step the model is told to run"
// separately testable.
func namesCommand(line, cmd string) bool {
	return wordRe(cmd).MatchString(line)
}

// wordReCache avoids recompiling the same boundary pattern once per line
// per candidate name (thousands of compiles across the real tree). It is
// mutex-guarded because Check is safe to call concurrently and the natural
// way to run this — a t.Parallel() subtest per recipe — would otherwise be
// a data race that only shows up under -race, long after the fact.
var (
	wordReMu    sync.Mutex
	wordReCache = map[string]*regexp.Regexp{}
)

func wordRe(word string) *regexp.Regexp {
	wordReMu.Lock()
	defer wordReMu.Unlock()
	if re, ok := wordReCache[word]; ok {
		return re
	}
	re := regexp.MustCompile(`(?:^|[^A-Za-z0-9_./-])` + regexp.QuoteMeta(word) + `(?:$|[^A-Za-z0-9_-])`)
	wordReCache[word] = re
	return re
}

// skillFiles returns the parent scope's markdown: every skills/ tree the
// recipe itself loads — its own, plus one per entry in config.content_roots
// — relative to dir, in stable order. Following content_roots matters:
// kube-platform-agent loads most of its content from a vendored snapshot
// outside the agents dir, and a checker that only looked at <dir>/skills
// would have declared it clean while missing 18 skills.
//
// Subagent roots are NOT included here; they are a different catalog and
// get their own scope (see subagentScope).
func skillFiles(dir string, contentRoots []string) ([]string, error) {
	roots := []string{dir}
	for _, cr := range contentRoots {
		if !filepath.IsAbs(cr) {
			cr = filepath.Join(dir, cr)
		}
		roots = append(roots, cr)
	}
	return markdownFiles(dir, roots)
}

// markdownFiles walks the skills/ subtree of each content root and returns
// the markdown paths, relative to dir, in stable order.
//
// SKILL.md and its references are the prose the model is handed; assets
// and scripts under a skill are not, so only .md is scanned.
func markdownFiles(dir string, contentRoots []string) ([]string, error) {
	var out []string
	// A content root can overlap the recipe's own tree (content_roots: ["."]
	// resolves to the same skills dir), which would scan every file twice and
	// double every count — including the waived count a reviewer reads.
	seen := map[string]bool{}
	for _, cr := range contentRoots {
		root := filepath.Join(cr, "skills")
		if _, err := os.Stat(root); err != nil {
			continue
		}
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(d.Name(), ".md") {
				return nil
			}
			rel, relErr := filepath.Rel(dir, path)
			if relErr != nil {
				return relErr
			}
			slash := filepath.ToSlash(rel)
			if seen[slash] {
				return nil
			}
			seen[slash] = true
			out = append(out, slash)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Strings(out)
	return out, nil
}

func waived(path string, globs []string) bool {
	for _, g := range globs {
		if ok, err := filepath.Match(g, path); err == nil && ok {
			return true
		}
		// Allow a directory-prefix form ("upstream/") without requiring
		// callers to spell out a recursive glob Match cannot express.
		if strings.HasSuffix(g, "/") && strings.HasPrefix(path, g) {
			return true
		}
	}
	return false
}
