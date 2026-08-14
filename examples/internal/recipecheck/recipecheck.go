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
//     `bash`'s.
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
// Skill content avoids the same trap by phrasing limits without naming a
// CLI ("there is no shell to fall back to"), which is how the shipped
// content is written; a reference that spells out "do not use kubectl"
// will trip rule D, and should be reworded rather than waived.
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
	servers, err := mcp.Load(r.Dir)
	if err != nil {
		return nil, fmt.Errorf("recipecheck: %s: mcp.Load: %w", r.Name, err)
	}
	registered, err := registeredBuiltins(cfg, r.Dir)
	if err != nil {
		return nil, fmt.Errorf("recipecheck: %s: %w", r.Name, err)
	}

	files, err := skillFiles(r.Dir, cfg.ContentRoots)
	if err != nil {
		return nil, fmt.Errorf("recipecheck: %s: %w", r.Name, err)
	}

	var findings []Finding
	add := func(f Finding) {
		f.Waived = waived(f.File, p.WaiveFileGlobs)
		findings = append(findings, f)
	}

	// Rule G: poll_allow only means anything if wait_and_verify is in the
	// catalog. A populated list under a disabled tool is a set of read-only
	// assertions about a tool nobody can call — dead config that reads like
	// a capability.
	if len(cfg.Tools.WaitAndVerify.PollAllow) > 0 && !registered["wait_and_verify"] {
		add(Finding{File: "config.json", Line: 0, Name: "wait_and_verify",
			Reason: fmt.Sprintf("tools.wait_and_verify.poll_allow lists %d tool(s) but %s",
				len(cfg.Tools.WaitAndVerify.PollAllow), notRegisteredReason("wait_and_verify", cfg))})
	}

	// Rule E: poll_allow entries must name something reachable. This one
	// is config-only, so it runs even for a recipe with no skills.
	for _, name := range cfg.Tools.WaitAndVerify.PollAllow {
		if reason, ok := unreachableToolName(name, registered, servers); !ok {
			add(Finding{File: "config.json", Line: 0, Name: name, Reason: reason +
				" — an unmatched poll_allow entry is not a config error, so the poll is silently refused at call time"})
		}
	}

	pollAllow := make(map[string]bool, len(cfg.Tools.WaitAndVerify.PollAllow))
	for _, n := range cfg.Tools.WaitAndVerify.PollAllow {
		pollAllow[n] = true
	}

	for _, path := range files {
		body, readErr := os.ReadFile(filepath.Join(r.Dir, path))
		if readErr != nil {
			return nil, fmt.Errorf("recipecheck: %s: read %s: %w", r.Name, path, readErr)
		}
		for i, line := range strings.Split(string(body), "\n") {
			lineNo := i + 1

			// Rule A: a built-in the content names but the registry does
			// not contain.
			for _, name := range tools.BuiltinToolNames() {
				if registered[name] {
					continue
				}
				if !namesTool(line, name) {
					continue
				}
				add(Finding{File: path, Line: lineNo, Name: name,
					Reason: notRegisteredReason(name, cfg)})
			}

			// Rule D: a shell command, reachable only via `bash`.
			if !registered["bash"] {
				for _, cmd := range shellCmds {
					if !namesCommand(line, cmd) {
						continue
					}
					add(Finding{File: path, Line: lineNo, Name: cmd,
						Reason: "`bash` is not in the tool catalog, so the agent has no way to run a CLI"})
				}
			}

			// Rule B: a double-underscore MCP name matches nothing.
			for _, m := range doubleUnderscore.FindAllString(line, -1) {
				add(Finding{File: path, Line: lineNo, Name: m,
					Reason: "pkg/mcp/namespace.go joins server and tool with a SINGLE underscore, so no server exposes this name"})
			}

			// Rule C: names in an unambiguous tool position.
			for _, m := range toolPosition.FindAllStringSubmatch(line, -1) {
				name := m[1]
				if reason, ok := unreachableToolName(name, registered, servers); !ok {
					add(Finding{File: path, Line: lineNo, Name: name, Reason: reason})
					continue
				}
				// Rule F: reachable, but wait_and_verify additionally
				// requires an explicit read-only assertion.
				if !registered[name] && !pollAllow[name] {
					add(Finding{File: path, Line: lineNo, Name: name,
						Reason: "wait_and_verify polls it but it is not in tools.wait_and_verify.poll_allow; " +
							"MCP tools never self-classify read-only, so the call is refused"})
				}
			}
		}
	}

	sort.SliceStable(findings, func(i, j int) bool {
		if findings[i].File != findings[j].File {
			return findings[i].File < findings[j].File
		}
		return findings[i].Line < findings[j].Line
	})
	return findings, nil
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

// namesCommand is namesTool for CLIs. Same rule; kept separate because
// the reason string and the reachability question differ.
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

// skillFiles returns every markdown file in every skills/ tree the
// recipe loads — its own, plus one per entry in config.content_roots —
// relative to dir, in stable order. Following content_roots matters:
// kube-platform-agent loads most of its content from a vendored snapshot
// outside the agents dir, and a checker that only looked at
// <dir>/skills would have declared it clean while missing 18 skills.
//
// SKILL.md and its references are the prose the model is handed; assets
// and scripts under a skill are not, so only .md is scanned.
func skillFiles(dir string, contentRoots []string) ([]string, error) {
	roots := []string{filepath.Join(dir, "skills")}
	for _, cr := range contentRoots {
		if !filepath.IsAbs(cr) {
			cr = filepath.Join(dir, cr)
		}
		roots = append(roots, filepath.Join(cr, "skills"))
	}
	var out []string
	// A content root can overlap the recipe's own tree (content_roots: ["."]
	// resolves to the same skills dir), which would scan every file twice and
	// double every count — including the waived count a reviewer reads.
	seen := map[string]bool{}
	for _, root := range roots {
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
