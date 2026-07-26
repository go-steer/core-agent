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

import "path/filepath"

// Control-plane file classification (#378).
//
// Two tiers of files live under the `.agents/` project tree:
//
//   - Instruction-bearing files (AGENTS.md, skills content) shape what
//     the model is told, but a change to them takes effect through the
//     model, which is still gated. These stay normally writable.
//   - Privilege-bearing files — the agent config and the MCP config —
//     directly control the permission gate, the hook commands, and the
//     stdio MCP servers that the daemon spawns on the next
//     session/restart. A model with any write capability could add
//     `permissions.allow` entries, flip the mode, register a hook that
//     runs arbitrary `/bin/sh -c`, or add a malicious stdio MCP server
//     — a self-escalation + persistence path that no amount of
//     in-session gating catches, because the effect lands out-of-band.
//
// Writes to the privilege-bearing files therefore require an explicit
// interactive approval that no mode, session grant, allowlist entry,
// or built-in bundle can satisfy. See Gate.CheckFileWrite.

// controlPlaneBasenames is the set of privilege-bearing filenames,
// recognized inside any `.agents/` directory. The values mirror the
// loader constants (config.ConfigFileName = "config.json",
// mcp.MCPFileName = "mcp.json"); they are duplicated here as literals
// rather than imported to avoid a permissions→config/mcp dependency
// (and a potential import cycle). Lockstep tests assert the literals
// track the loader constants: config.ConfigFileName in
// controlplane_test.go here, and mcp.MCPFileName in pkg/mcp
// (which imports permissions).
var controlPlaneBasenames = map[string]struct{}{
	"config.json": {}, // config.ConfigFileName — permissions/hooks/mode
	"mcp.json":    {}, // mcp.MCPFileName — stdio MCP server commands
}

// controlPlaneDirName is the directory (config.AgentsDirName) whose
// config.json / mcp.json are privilege-bearing. Both the project-scope
// (`<workspace>/.agents/`) and user-scope (`~/.agents/`) trees the
// loaders read are covered, since classification keys on the immediate
// parent directory name rather than an absolute prefix.
const controlPlaneDirName = ".agents"

// isControlPlanePath reports whether resolved (which MUST already be a
// symlink-resolved absolute path — see Gate.CheckFileWrite) is a
// privilege-bearing control-plane file: a config.json or mcp.json
// sitting directly inside a `.agents/` directory.
//
// Keying on the immediate parent's base name (rather than a specific
// absolute path) means a symlink laundering trick can't dodge the
// classification — the caller resolves symlinks first — and both the
// project and home `.agents/` trees are covered without threading
// their absolute locations into the gate.
func isControlPlanePath(resolved string) bool {
	if _, ok := controlPlaneBasenames[filepath.Base(resolved)]; !ok {
		return false
	}
	return filepath.Base(filepath.Dir(resolved)) == controlPlaneDirName
}
