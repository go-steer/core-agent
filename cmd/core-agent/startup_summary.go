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

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/go-steer/core-agent/pkg/auth"
	"github.com/go-steer/core-agent/pkg/config"
	"github.com/go-steer/core-agent/pkg/mcp"
	"github.com/go-steer/core-agent/pkg/skills"
)

// startupSummaryInputs bundles everything formatStartupSummary needs.
// Keeping the input surface explicit (vs pulling from package-level
// state) is what makes the formatter unit-testable.
type startupSummaryInputs struct {
	// cfgPath is the value of the -c / --config flag as passed on the
	// CLI. Empty means the daemon fell through to config discovery
	// (walk-up from cwd looking for .agents/config.json).
	cfgPath string
	// cfg is the fully-resolved config (post CLI overrides, post
	// task-class tier fills).
	cfg *config.Config
	// agentsDir is the resolved .agents/ directory (from
	// filepath.Dir(cfgPath) when -c is set, else from config
	// discovery). Empty means "no agentsDir was found" — the daemon
	// still runs; MCP + skills + record_plan just have nowhere to
	// live.
	agentsDir string
	// providerName is the concrete provider name after resolution
	// (vertex / gemini / anthropic / anthropic-vertex / echo /
	// scripted). Comes from provider.Name() at the call site.
	providerName string
	// mcpServers describes every MCP server the daemon successfully
	// or unsuccessfully started. mcp.Server carries the name +
	// Status + Err — this summary calls the ones with a nil Err
	// "ok" and the ones with Status != "" but Err != nil "failed".
	mcpServers []*mcp.Server
	// loadedSkills describes the discovered skills — count + names
	// via loadedSkills.Infos.
	loadedSkills skills.Skills
}

// formatStartupSummary produces the config-summary block emitted at
// daemon startup right after the config / instruction / MCP / skills
// resolution completes. Six lines, one per topic, in the standard
// core-agent: <topic>: <detail> shape. Callers wrap each returned line
// with the `send` helper defined in run().
//
// Kept pure (no I/O beyond os.Getenv for the Vertex env-var summary)
// so it can be unit-tested by table-driven fixtures. Operators reading
// the daemon log see these lines FIRST (before "attach listener on"
// and the other established lines) — this is the "what did the
// daemon actually load" answer that was silent before #212.
func formatStartupSummary(in startupSummaryInputs) []string {
	lines := make([]string, 0, 6)

	// 1. config: source + resolution path.
	lines = append(lines, formatConfigLine(in.cfgPath, in.agentsDir))

	// 2. agentsDir: resolved absolute path + how we got there.
	lines = append(lines, formatAgentsDirLine(in.cfgPath, in.agentsDir))

	// 3. model + provider + project/location (for cloud providers).
	lines = append(lines, formatModelLine(in.cfg, in.providerName))

	// 4. mcp: N server(s) loaded — names.
	lines = append(lines, formatMCPLine(in.mcpServers))

	// 5. skills: N loaded — names.
	lines = append(lines, formatSkillsLine(in.loadedSkills))

	// 6. multi-session auth: kind, user count, admin/proxy lists.
	//    Reads users.json directly (LoadUsersFile) rather than
	//    depending on the buildMultiSessionAuthn call in the attach
	//    branch — the summary must fire regardless of attach mode.
	lines = append(lines, formatAuthLine(in.cfg))

	return lines
}

func formatConfigLine(cfgPath, agentsDir string) string {
	switch {
	case cfgPath != "":
		return fmt.Sprintf("config: source=%s (via -c)", cfgPath)
	case agentsDir != "":
		// Discovery walked up from cwd and landed on .agents/.
		return fmt.Sprintf("config: source=%s (via .agents/ discovery)", filepath.Join(agentsDir, "config.json"))
	default:
		return "config: source=<none> (pure defaults; no -c and no .agents/ discovered)"
	}
}

func formatAgentsDirLine(cfgPath, agentsDir string) string {
	if agentsDir == "" {
		return "agentsDir: <none> (record_plan / MCP / skills have no place to live)"
	}
	origin := "via .agents/ discovery"
	if cfgPath != "" {
		origin = "derived from filepath.Dir(-c)"
	}
	return fmt.Sprintf("agentsDir: %s (%s)", agentsDir, origin)
}

func formatModelLine(cfg *config.Config, providerName string) string {
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
	return fmt.Sprintf("model: %s provider=%s%s", model, provider, extras)
}

func formatMCPLine(servers []*mcp.Server) string {
	if len(servers) == 0 {
		return "mcp: 0 servers loaded"
	}
	// Sort by name for deterministic output (operators grepping
	// startup logs across sessions want stable order).
	sorted := make([]*mcp.Server, len(servers))
	copy(sorted, servers)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	names := make([]string, 0, len(sorted))
	failures := 0
	for _, s := range sorted {
		if s == nil {
			continue
		}
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
