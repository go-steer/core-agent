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

package tools

import adktool "google.golang.org/adk/tool"

// Shared-filesystem classification (#653). Distinct from the #460
// read-only classification next door, and the difference is the whole
// point of having two:
//
//   - IsReadOnlyTool answers "may this dispatch concurrently with the
//     other calls in ONE agent's response". Anything that mutates any
//     state at all is excluded, including purely in-memory state.
//   - WritesSharedFilesystem answers "would running this at the same
//     instant as another agent's write corrupt the working tree".
//
// `todo` is the clearest gap between them: it mutates a TodoStore held
// in one agent's memory, so #460 correctly serializes it, and it cannot
// clobber anything on disk, so a filesystem guard that refused a
// subagent for holding it would be refusing on a fiction.
//
// Everything unrecognised classifies as WRITING. That is the fail-safe
// direction here for the same reason the mutating default is fail-safe
// in serialize.go: misclassifying a harmless tool costs some
// concurrency, and misclassifying a writer costs the guarantee.

// nonFilesystemMutators names the tools that IsReadOnlyTool excludes —
// they do mutate something — but which cannot touch the shared working
// tree. Listed with what they actually mutate, because that is the only
// evidence that justifies the entry:
//
//   - todo: a per-agent in-memory TodoStore.
//   - alert: an outbound webhook. Network egress, no local state.
//   - spawn_agent / stop_agent / spawn_remote_agent: the background
//     manager's handle table. A spawned agent may of course go on to
//     write — but every spawn funnels through the same guard that
//     consults this table, so the recursion is caught at the choke
//     point rather than by pessimising the name here.
//   - mark_task_done / return_result and its aliases / report_alert /
//     schedule_next_turn: control-plane signals against the agent's own
//     flags, the parent's alert channel, or the scheduler.
//   - list_skills / load_skill / load_skill_resource: reads against the
//     skills registry. These are the whole "skill" namespace, and
//     permissions.planExemptTools exempts that namespace wholesale on
//     the same reasoning. Listing them by NAME rather than accepting
//     the namespace is what makes a fourth skill tool show up as
//     unclassified instead of silently inheriting the exemption —
//     TestSkillToolsDoNotWriteTheSharedTree in pkg/skills is the alarm.
//
// Deliberately ABSENT, i.e. classified as writing: write_file,
// edit_file, delete_file, bash (arbitrary commands), record_plan (an
// artifact under the agents dir) and sciontool_status (a sticky status
// file).
var nonFilesystemMutators = map[string]bool{
	"todo":                true,
	"alert":               true,
	"spawn_agent":         true,
	"stop_agent":          true,
	"spawn_remote_agent":  true,
	"mark_task_done":      true,
	"return_result":       true,
	"report_done":         true,
	"report_completed":    true,
	"report_alert":        true,
	"schedule_next_turn":  true,
	"list_skills":         true,
	"load_skill":          true,
	"load_skill_resource": true,
}

// WritesSharedFilesystem reports whether t could modify the working
// directory every in-process agent shares.
//
// Order of authority mirrors IsReadOnlyTool: the tool's own
// ReadOnlyHinter declaration, then the read-only name table, then the
// non-filesystem table, then the fail-safe default.
//
// A tool that declares itself settles the question OUTRIGHT, including
// against nonFilesystemMutators. Both tables describe this tree's
// built-ins by name, and an MCP server is free to export a tool called
// `todo` that is nothing of the sort; letting our table absolve a remote
// tool of a hint it volunteered would be reading the name instead of the
// declaration.
func WritesSharedFilesystem(t adktool.Tool) bool {
	if t == nil {
		return false
	}
	if h, ok := t.(ReadOnlyHinter); ok {
		return !h.ReadOnlyHint()
	}
	if IsReadOnlyTool(t) {
		return false
	}
	return WritesSharedFilesystemName(t.Name())
}

// WritesSharedFilesystemName is the name-only form, for callers holding
// a name snapshot rather than a tool instance — the background
// manager's toolset classification (#653), which must not enumerate a
// live MCP server to answer a spawn-time question.
//
// It is strictly weaker than WritesSharedFilesystem: with no instance
// there is no ReadOnlyHinter to consult, so a name it does not
// recognise classifies as writing even if the tool behind it declared
// itself read-only. Callers that HAVE the instance should use
// WritesSharedFilesystem.
func WritesSharedFilesystemName(name string) bool {
	if IsReadOnlyToolName(name) {
		return false
	}
	return !nonFilesystemMutators[name]
}

// AnyWritesSharedFilesystem reports whether any tool in ts could modify
// the shared working directory — the question a spawn-time guard asks of
// a subagent's whole granted surface.
func AnyWritesSharedFilesystem(ts []adktool.Tool) bool {
	for _, t := range ts {
		if WritesSharedFilesystem(t) {
			return true
		}
	}
	return false
}
