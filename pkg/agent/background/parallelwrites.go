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

package background

import (
	"errors"
	"fmt"

	"github.com/go-steer/core-agent/v2/pkg/attach"
	coretools "github.com/go-steer/core-agent/v2/pkg/tools"
)

// Parallel write policy (#653).
//
// Every in-process agent shares one working directory. Nothing in
// pkg/tools carries a per-agent cwd: bash execs without a Dir and file
// paths absolutize against the process cwd. Within one agent that is
// harmless, because #460's mutation serializer makes its own mutating
// calls mutually exclusive — and that covers SYNCHRONOUS subagent
// delegation too, since subagent tools are appended to the parent's
// tool list before the serializer wraps them. Measured, not assumed:
// TestConcurrentSyncSubagentDelegationsDoNotOverlap in pkg/agent pins
// it, with a read-only control proving the harness can see overlap.
//
// Background agents are the gap. Each launch builds its own agent with
// its own serializer, so two of them can land a write_file — or a bash
// running `git checkout` — on the same tree in the same instant.
//
// The fix that would make this a non-question is a per-subagent
// worktree, which #653 asks for and this does not deliver. What this
// delivers is a refusal of the shape: while one write-capable
// background agent is running, a second one is turned away.
//
// Sharing the parent's serializer with background agents was the other
// candidate and is wrong: a background `bash` running a ten-minute test
// suite would hold the mutex the whole time and block the parent's own
// edits. Background agents exist to be non-blocking; a guarantee bought
// with head-of-line blocking of the interactive session is not one an
// operator would choose.
const (
	policyRefuse = "refuse"
	policyWarn   = "warn"
	policyAllow  = "allow"
)

// ErrConcurrentWriters is returned by Spawn/SpawnTemplate when a
// write-capable subagent would start while another is already running
// and safety.parallel_subagent_writes is "refuse" (the default).
var ErrConcurrentWriters = errors.New("background: another write-capable subagent is already running")

// resolveWritePolicy normalizes the configured policy. An unrecognized
// value resolves to refuse rather than silently disarming: pkg/config
// validates the field, so anything else reaching here is a library
// caller's typo, and a typo that turns a guard off is the failure mode
// worth avoiding.
func resolveWritePolicy(s string) string {
	switch s {
	case policyRefuse, policyWarn, policyAllow:
		return s
	default:
		return policyRefuse
	}
}

// spawnWritesSharedFilesystem reports whether the surface granted to
// this spawn could modify the shared working tree.
//
// Two sources, because a spawn's tools arrive two ways. Built-in
// instances are classified directly by tools.WritesSharedFilesystem.
// Toolsets (MCP + skills, on the declarative-template path) are opaque
// here by design — enumerating one needs a ReadonlyContext and may
// reach a live MCP server — so the classification reads the name
// snapshot the builder took at registration (SubagentTemplate.
// ToolsetTools) instead.
//
// For that snapshot only the source is available, not a readOnlyHint,
// so:
//
//   - source "skill" is classified BY NAME, through
//     tools.WritesSharedFilesystemName. The namespace is exactly
//     list_skills / load_skill / load_skill_resource today, all reads
//     against the skills registry — the same judgement
//     permissions.planExemptTools makes. Going through the name table
//     rather than exempting the namespace is the difference between a
//     fourth skill tool being noticed and it inheriting the exemption
//     in silence; an unrecognised name classifies as writing, and
//     TestSkillToolsDoNotWriteTheSharedTree in pkg/skills fires first.
//   - anything else — MCP above all — is a writer, without consulting
//     the name at all. An MCP server exposes arbitrary tools under
//     arbitrary names, so a server calling its tool `grep` must not
//     inherit our `grep`'s classification, and the snapshot does not
//     record which tools declared readOnlyHint (#1098 plumbed the hint
//     onto the tool objects, not into attach.ToolInfo).
//   - a template that has toolsets but NO snapshot is a writer. The
//     field is optional, so an empty one means "unknown surface", and
//     unknown resolves the fail-safe way.
func spawnWritesSharedFilesystem(rs resolvedSpawn) bool {
	if coretools.AnyWritesSharedFilesystem(rs.tools) {
		return true
	}
	if len(rs.toolsets) == 0 {
		return false
	}
	if len(rs.toolsetTools) == 0 {
		return true
	}
	for _, ti := range rs.toolsetTools {
		if ti.Source != attach.ToolSourceSkill {
			return true
		}
		if coretools.WritesSharedFilesystemName(ti.Name) {
			return true
		}
	}
	return false
}

// runningWriterLocked returns the name of a running write-capable
// subagent, or "" if there is none. Caller holds m.mu.
//
// It reports one name rather than a count because the name is what
// makes the refusal actionable: the model can stop that agent, wait for
// it, or fold the work into it. "2 writers running" tells it nothing it
// can act on.
//
// One window this does not close, stated rather than papered over:
// StopAndReport flips status to StatusStopped and THEN cancels, so
// between the flip and the goroutine observing its context a
// stop_agent'd writer's in-flight bash can still be running while a new
// writer is admitted. Closing it would mean waiting on h.done inside
// launch — blocking a spawn for the length of an arbitrary tool call,
// and Close already documents that a tool wedged in uninterruptible I/O
// need not return at all. The guard's claim is about the shape the model
// asks for, not about cancellation being instantaneous.
func (m *Manager) runningWriterLocked() string {
	for name, h := range m.agents {
		if h.writesFS && h.Status() == StatusRunning {
			return name
		}
	}
	return ""
}

// concurrentWriterRefusal builds the error the refuse policy returns.
// It names the holder and the three ways out, because a refusal the
// model cannot route around just becomes a retry loop.
func concurrentWriterRefusal(newName, holder string) error {
	return fmt.Errorf("%w: %q holds the shared working directory, so starting %q could interleave writes to the same files. "+
		"Wait for %q to finish (or stop_agent it) and spawn again, give %q only read-only tools, "+
		"or — if the two really must write in parallel — use spawn_remote_agent, which runs out of process with its own filesystem",
		ErrConcurrentWriters, holder, newName, holder, newName)
}
