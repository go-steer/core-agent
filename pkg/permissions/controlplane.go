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

package permissions

import (
	"path/filepath"
	"slices"
	"strings"
)

// Control-plane file classification (#378, #1354).
//
// Writes to any file the boot path loads — or that steers the host's
// git toolchain on the human's next git command — require an explicit
// interactive approval (PromptKindControlPlaneWrite) that no mode
// (yolo/acceptEdits), session grant, allowlist entry, or built-in
// bundle can satisfy. See Gate.CheckFileWrite.

// controlPlaneBasenames is the set of privilege-bearing filenames
// recognized directly inside any `.agents/` directory. The values
// mirror the loader constants (config.ConfigFileName = "config.json",
// mcp.MCPFileName = "mcp.json", etc.); they are duplicated here as
// literals rather than imported to avoid a permissions→config/mcp
// dependency (and a potential import cycle). Lockstep tests assert the
// literals track the loader constants: config.ConfigFileName in
// controlplane_test.go here, and mcp.MCPFileName in pkg/mcp.
var controlPlaneBasenames = map[string]struct{}{
	"config.json":     {}, // config.ConfigFileName — permissions/hooks/mode
	"mcp.json":        {}, // mcp.MCPFileName — stdio MCP server commands
	"env.yaml":        {}, // agentenv manifest
	"env.yml":         {}, // agentenv manifest alternate extension
	"env.json":        {}, // agentenv manifest JSON form
	"pricing.json":    {}, // pricing override layer (lowering prices defeats cost ceilings)
	"agent-card.json": {}, // A2A agent card config
}

// controlPlaneDirName is the directory (config.AgentsDirName) whose
// boot-loaded files and subtrees are privilege-bearing. Both the
// project-scope (`<workspace>/.agents/`) and user-scope (`~/.agents/`)
// trees are covered.
const controlPlaneDirName = ".agents"

// userCoreAgentDirName is the user-global runtime directory
// (`~/.core-agent`), which holds user-scope AGENTS.md, pricing.json,
// skills, and session state (#1354).
const userCoreAgentDirName = ".core-agent"

// controlPlaneAgentsSubtrees are the subdirectories under `.agents/`
// whose contents the boot path loads (skills, declarative subagents,
// provider profiles, and instruction overlays). Runtime output
// directories (.agents/plans/, .agents/sessions/) are intentionally
// excluded.
var controlPlaneAgentsSubtrees = setOf(
	"skills",
	"subagents",
	"providers",
	"AGENTS.d",
)

// controlPlaneInstructionBasenames are the instruction filenames
// pkg/instruction loads from the project root, `.agents/`, rooted
// subagents, `~/.agents/`, and `~/.core-agent/`.
var controlPlaneInstructionBasenames = setOf(
	"AGENTS.md",
	"CLAUDE.md",
	"GEMINI.md",
)

type controlPlaneRule struct {
	name  string
	match func(parts []string, base string) bool
}

// controlPlaneRules is the declarative list of everything the boot
// path loads plus `.git/**` host hooks/config (#1354).
var controlPlaneRules = []controlPlaneRule{
	{
		name: "git-metadata",
		match: func(parts []string, _ string) bool {
			return slices.Contains(parts, ".git")
		},
	},
	{
		name: "mcp-config",
		match: func(_ []string, base string) bool {
			// Covers .agents/mcp.json, ~/.agents/mcp.json,
			// ~/.core-agent/mcp.json, and any rooted declarative
			// subagent's <root>/mcp.json.
			return base == "mcp.json"
		},
	},
	{
		name: "agents-boot-files",
		match: func(parts []string, base string) bool {
			if _, ok := controlPlaneBasenames[base]; !ok {
				return false
			}
			return len(parts) >= 2 && parts[len(parts)-2] == controlPlaneDirName
		},
	},
	{
		name: "agents-boot-subtrees",
		match: func(parts []string, _ string) bool {
			for i := 0; i+1 < len(parts); i++ {
				if parts[i] == controlPlaneDirName {
					if _, ok := controlPlaneAgentsSubtrees[parts[i+1]]; ok {
						return true
					}
				}
			}
			return false
		},
	},
	{
		name: "boot-instructions",
		match: func(parts []string, base string) bool {
			if _, ok := controlPlaneInstructionBasenames[base]; ok {
				return true
			}
			return slices.Contains(parts, "AGENTS.d")
		},
	},
	{
		name: "user-core-agent-tree",
		match: func(parts []string, _ string) bool {
			return slices.Contains(parts, userCoreAgentDirName)
		},
	},
}

// isControlPlanePath reports whether resolved (which MUST already be a
// symlink-resolved absolute path — see Gate.CheckFileWrite) matches any
// rule in controlPlaneRules.
func isControlPlanePath(resolved string) bool {
	clean := filepath.Clean(resolved)
	base := filepath.Base(clean)
	parts := splitPathComponents(clean)
	for _, r := range controlPlaneRules {
		if r.match(parts, base) {
			return true
		}
	}
	return false
}

func splitPathComponents(clean string) []string {
	trimmed := strings.Trim(clean, string(filepath.Separator))
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, string(filepath.Separator))
}

// resolvedInstructionsPath is the approver instructions file in the
// form checkFileWrite compares against: absolute and symlink-resolved
// (ResolvePath), or "" when none is configured. A path whose symlinks
// cannot be resolved keeps its absolute spelling rather than dropping
// out of the tier.
func resolvedInstructionsPath(file string) string {
	if file == "" {
		return ""
	}
	if resolved, err := ResolvePath(file); err == nil {
		return resolved
	}
	if abs, err := filepath.Abs(expandUser(file)); err == nil {
		return filepath.Clean(abs)
	}
	return filepath.Clean(file)
}

// isApproverInstructions reports whether resolved (symlink-resolved,
// as for isControlPlanePath) is the approver's instructions file. The
// comparison ignores letter case, which on a case-sensitive filesystem
// over-matches toward the elevated prompt rather than away from it.
func (g *Gate) isApproverInstructions(resolved string) bool {
	return g.approverInstructions != "" && strings.EqualFold(resolved, g.approverInstructions)
}
