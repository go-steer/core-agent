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

package skills

import (
	"context"
	"testing"

	coretools "github.com/go-steer/core-agent/v2/pkg/tools"
)

// Two places in the tree assert that the skill toolset cannot touch the
// working directory, and neither of them can see this package:
//
//   - permissions.planExemptTools exempts the whole "skill" namespace
//     from plan-first gating, with a comment saying the exemption must
//     be revisited if a mutating skill tool is ever added.
//   - the #653 parallel-write guard in pkg/agent/background classifies
//     a declarative subagent's skill tools through
//     tools.WritesSharedFilesystemName, so a skill tool that is not in
//     that table makes the subagent write-capable.
//
// Both were promises with nothing behind them. This is the alarm: add a
// tool to the skill toolset and it fails here, naming both call sites,
// instead of quietly inheriting an exemption that was argued for three
// specific read-only tools.
//
// It tests the real toolset (whatever ADK's skill facade registers
// today) rather than a hardcoded roster, so an upstream rename is
// caught too.
func TestSkillToolsDoNotWriteTheSharedTree(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeSkill(t, dir, "cli-setup", "set up the CLI")

	got, err := LoadAll(context.Background(), dir, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	infos := got.ToolInfos()
	if len(infos) == 0 {
		t.Fatal("no skill tools enumerated — the assertion below would be vacuous")
	}
	for _, ti := range infos {
		if coretools.WritesSharedFilesystemName(ti.Name) {
			t.Errorf("skill tool %q is not classified as non-filesystem.\n"+
				"If it only reads the skills registry, add it to nonFilesystemMutators in "+
				"pkg/tools/fswrites.go.\n"+
				"If it can write, mutate state or reach the network, then ALSO re-argue the "+
				"blanket \"skill\" entry in permissions.planExemptTools — it exempts this tool "+
				"from plan-first gating on the assumption that the namespace is read-only.", ti.Name)
		}
	}
}
