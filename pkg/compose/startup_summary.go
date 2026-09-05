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

package compose

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/go-steer/core-agent/v2/pkg/auth"
	"github.com/go-steer/core-agent/v2/pkg/config"
	"github.com/go-steer/core-agent/v2/pkg/mcp"
	"github.com/go-steer/core-agent/v2/pkg/models"
	"github.com/go-steer/core-agent/v2/pkg/skills"
)

// StartupSummaryInputs bundles everything FormatStartupSummary needs.
// Keeping the input surface explicit (vs pulling from package-level
// state) is what makes the formatter unit-testable.
type StartupSummaryInputs struct {
	// CfgPath is the value of the -c / --config flag as passed on the
	// CLI. Empty means the daemon fell through to config discovery
	// (walk-up from cwd looking for .agents/config.json).
	CfgPath string
	// Cfg is the fully-resolved config (post CLI overrides, post
	// task-class tier fills).
	Cfg *config.Config
	// AgentsDir is the resolved .agents/ directory (from
	// filepath.Dir(cfgPath) when -c is set, else from config
	// discovery). Empty means "no agentsDir was found" — the daemon
	// still runs; MCP + skills + record_plan just have nowhere to
	// live.
	AgentsDir string
	// AgentsDirOrigin is the human-readable reason AgentsDir has the
	// value it does — "derived from filepath.Dir(-c)", "via
	// --agents-dir", "via .agents/ discovery". The value alone is not
	// enough to debug a wrong one: the three routes to it are fixed in
	// three different places, and knowing which applied is the
	// difference between "fix your flag" and "you are running from the
	// wrong directory" (#945).
	//
	// Empty falls back to inferring the origin from CfgPath, which is
	// correct for every caller that predates --agents-dir.
	AgentsDirOrigin string
	// DiscoveredConfigDir is the directory config discovery actually
	// landed on, which is where config.json was read from when CfgPath
	// is empty. It is NOT the same thing as AgentsDir once --agents-dir
	// is in play: the flag moves AgentsDir without moving the config,
	// and inferring the config's location from AgentsDir would then name
	// a file that does not exist (#945). Empty falls back to AgentsDir,
	// which is correct for every caller that predates --agents-dir.
	DiscoveredConfigDir string
	// ProviderName is the concrete provider name after resolution
	// (vertex / gemini / anthropic / anthropic-vertex / echo /
	// scripted). Comes from provider.Name() at the call site.
	ProviderName string
	// BuiltinTools is the rendered server-side built-in tool set, from
	// BuiltinToolsSummary(provider) at the call site. Empty means the
	// resolved provider has no such concept and the model line omits
	// the segment entirely; "none" means it has one and everything in
	// it is off. The distinction is the point — see BuiltinToolsSummary.
	BuiltinTools string
	// MCPServers describes every MCP server the daemon successfully
	// or unsuccessfully started. mcp.Server carries the name +
	// Status + Err — this summary calls the ones with a nil Err
	// "ok" and the ones with Status != "" but Err != nil "failed".
	MCPServers []*mcp.Server
	// LoadedSkills describes the discovered skills — count + names
	// via LoadedSkills.Infos.
	LoadedSkills skills.Skills
}

// FormatStartupSummary produces the config-summary block emitted at
// daemon startup right after the config / instruction / MCP / skills
// resolution completes. Seven lines, one per topic, in the standard
// core-agent: <topic>: <detail> shape. Callers wrap each returned line
// with the `send` helper defined in run().
//
// Kept pure (no I/O beyond os.Getenv for the Vertex env-var summary)
// so it can be unit-tested by table-driven fixtures. Operators reading
// the daemon log see these lines FIRST (before "attach listener on"
// and the other established lines) — this is the "what did the
// daemon actually load" answer that was silent before #212.
func FormatStartupSummary(in StartupSummaryInputs) []string {
	lines := make([]string, 0, 7)

	// 1. config: source + resolution path.
	lines = append(lines, formatConfigLine(in.CfgPath, in.DiscoveredConfigDir, in.AgentsDir))

	// 2. agentsDir: resolved absolute path + how we got there.
	lines = append(lines, formatAgentsDirLine(in.CfgPath, in.AgentsDir, in.AgentsDirOrigin))

	// 3. model + provider + project/location (for cloud providers) +
	//    the provider's server-side built-in tools.
	lines = append(lines, formatModelLine(in.Cfg, in.ProviderName, in.BuiltinTools))

	// 4. mcp: N server(s) loaded — names.
	lines = append(lines, formatMCPLine(in.MCPServers))

	// 5. skills: N loaded — names.
	lines = append(lines, formatSkillsLine(in.LoadedSkills))

	// 6. subagents: N configured — the declarative roster (#627), so
	//    operators can verify what the daemon loaded without grepping the
	//    per-subagent boot lines.
	lines = append(lines, formatSubagentsLine(in.Cfg))

	// 7. multi-session auth: kind, user count, admin/proxy lists.
	//    Reads users.json directly (LoadUsersFile) rather than
	//    depending on the BuildMultiSessionAuthn call in the attach
	//    branch — the summary must fire regardless of attach mode.
	lines = append(lines, formatAuthLine(in.Cfg))

	return lines
}

func formatConfigLine(cfgPath, discoveredConfigDir, agentsDir string) string {
	if discoveredConfigDir == "" {
		// Pre-#945 callers pass only AgentsDir, which was the directory
		// discovery landed on back when nothing could move it.
		discoveredConfigDir = agentsDir
	}
	switch {
	case cfgPath != "":
		return fmt.Sprintf("config: source=%s (via -c)", cfgPath)
	case discoveredConfigDir != "":
		// Discovery walked up from cwd and landed on .agents/.
		return fmt.Sprintf("config: source=%s (via .agents/ discovery)", filepath.Join(discoveredConfigDir, "config.json"))
	default:
		return "config: source=<none> (pure defaults; no -c and no .agents/ discovered)"
	}
}

func formatAgentsDirLine(cfgPath, agentsDir, origin string) string {
	if agentsDir == "" {
		return "agentsDir: <none> (record_plan / MCP / skills have no place to live)"
	}
	if origin == "" {
		// Pre-#945 inference, for callers that do not set the field.
		origin = "via .agents/ discovery"
		if cfgPath != "" {
			origin = "derived from filepath.Dir(-c)"
		}
	}
	return fmt.Sprintf("agentsDir: %s (%s)", agentsDir, origin)
}

// BuiltinToolsSummary renders the provider's effective server-side
// built-in tool set for the startup summary: a comma-joined list of the
// provider-neutral names, or "none" when the provider supports them and
// has them all off.
//
// Returns "" — not "none" — for a provider with no server-side built-in
// concept at all (echo, scripted), so the model line can omit the
// segment rather than assert an absence that means nothing there.
//
// Read off the constructed Provider rather than off config, for the
// same reason MaybeWirePromptCacheTTL reports what the provider carries:
// a line derived independently of the requests can drift from them. It
// also makes the line answer the question an operator actually has —
// `builtin_tools` keys are silently discarded when misspelled (config
// decoding does not reject unknown fields), so "did my key take" is
// only answerable from the far side of the constructor.
func BuiltinToolsSummary(provider models.Provider) string {
	r, ok := provider.(models.BuiltinToolsReporter)
	if !ok {
		return ""
	}
	names := r.BuiltinToolNames()
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ",")
}

func formatModelLine(cfg *config.Config, providerName, builtinTools string) string {
	if cfg == nil {
		return "model: <unknown> (nil cfg)"
	}
	model := cfg.Model.Name
	if model == "" {
		model = "<unset>"
	}
	provider := providerName
	if provider == "" {
		provider = cfg.Model.Provider
	}
	if provider == "" {
		provider = "<unset>"
	}
	// For Vertex specifically, GOOGLE_CLOUD_PROJECT / _LOCATION are
	// the load-bearing values operators need to verify — every gke-mcp
	// (and any other GCP-facing MCP) call fails without them. Surfacing
	// them here catches the #4665e3c-class recipe bug (envFrom missing)
	// long before the model is invoked.
	extras := ""
	if provider == "vertex" || provider == "anthropic-vertex" {
		project := os.Getenv("GOOGLE_CLOUD_PROJECT")
		location := os.Getenv("GOOGLE_CLOUD_LOCATION")
		if project == "" {
			project = "<unset>"
		}
		if location == "" {
			location = "<unset>"
		}
		extras = fmt.Sprintf(" project=%s location=%s", project, location)
	}
	// Server-side built-ins are invisible everywhere else: they never
	// produce a tool call, so no other log line, no permission prompt
	// and no `tools.disable` entry mentions them. Naming the effective
	// set here is the only place an operator learns that the model can
	// reach the public internet — and the only way to confirm a
	// `builtin_tools` key wasn't quietly dropped as a typo.
	if builtinTools != "" {
		extras += " builtin-tools=" + builtinTools
	}
	return fmt.Sprintf("model: %s provider=%s%s", model, provider, extras)
}

func formatMCPLine(servers []*mcp.Server) string {
	// Drop nils before sorting, not during the render loop: the
	// comparator dereferences .Name, so a nil entry would panic the
	// startup summary before the loop's guard could skip it.
	sorted := make([]*mcp.Server, 0, len(servers))
	for _, s := range servers {
		if s != nil {
			sorted = append(sorted, s)
		}
	}
	if len(sorted) == 0 {
		return "mcp: 0 servers loaded"
	}
	// Sort by name for deterministic output (operators grepping
	// startup logs across sessions want stable order).
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	names := make([]string, 0, len(sorted))
	failures := 0
	for _, s := range sorted {
		status := "ok"
		if s.Err != nil {
			status = "failed"
			failures++
		}
		names = append(names, fmt.Sprintf("%s(%s)", s.Name, status))
	}
	suffix := ""
	if failures > 0 {
		suffix = fmt.Sprintf(" [%d failed — see 'core-agent: mcp:' error lines above]", failures)
	}
	return fmt.Sprintf("mcp: %d server(s) loaded — %s%s", len(names), strings.Join(names, ", "), suffix)
}

func formatSkillsLine(loaded skills.Skills) string {
	if loaded.Empty() {
		return "skills: 0 loaded"
	}
	names := make([]string, 0, len(loaded.Infos))
	for _, info := range loaded.Infos {
		names = append(names, info.Name)
	}
	sort.Strings(names)
	return fmt.Sprintf("skills: %d loaded — %s", len(names), strings.Join(names, ", "))
}

func formatSubagentsLine(cfg *config.Config) string {
	if cfg == nil || len(cfg.Subagents) == 0 {
		return "subagents: 0 configured"
	}
	// Sort by name for deterministic output across restarts.
	entries := make([]string, 0, len(cfg.Subagents))
	for _, s := range cfg.Subagents {
		desc := s.Name
		attrs := make([]string, 0, 2)
		if s.Model != nil && s.Model.Name != "" {
			attrs = append(attrs, "model="+s.Model.Name)
		}
		if s.Root != "" {
			attrs = append(attrs, "root="+s.Root)
		}
		if len(attrs) > 0 {
			desc += " (" + strings.Join(attrs, ", ") + ")"
		}
		entries = append(entries, desc)
	}
	sort.Strings(entries)
	return fmt.Sprintf("subagents: %d configured — %s", len(cfg.Subagents), strings.Join(entries, ", "))
}

func formatAuthLine(cfg *config.Config) string {
	if cfg == nil {
		return "multi-session auth: <disabled> (nil cfg)"
	}
	ms := cfg.Attach.MultiSession
	if !ms.Enabled {
		return "multi-session auth: disabled (single-user mode; use --attach-token for bearer auth)"
	}
	// Report the resolved Kind — empty string is bearer_table per
	// MultiSessionAuthConfig contract.
	kind := ms.Auth.Kind
	if kind == "" {
		kind = config.MultiSessionAuthKindBearerTable
	}

	// User count: try to load users.json directly. Failures are
	// non-fatal for the summary — we surface the error text so the
	// operator sees it, but we don't panic on missing file (the
	// attach branch will do the load-and-validate later; this is
	// belt-and-suspenders visibility).
	userCount := "?"
	if ms.Auth.TableFile != "" {
		if uf, err := auth.LoadUsersFile(ms.Auth.TableFile); err != nil {
			userCount = fmt.Sprintf("? (load error: %v)", err)
		} else {
			userCount = fmt.Sprintf("%d", len(uf.Users))
		}
	}

	admins := "[]"
	if len(ms.AdminIdentities) > 0 {
		admins = "[" + strings.Join(ms.AdminIdentities, ",") + "]"
	}
	proxies := "[]"
	if len(ms.ProxyIdentities) > 0 {
		proxies = "[" + strings.Join(ms.ProxyIdentities, ",") + "]"
	}

	return fmt.Sprintf("multi-session auth: %s, %s users, admin=%s proxy=%s", kind, userCount, admins, proxies)
}
