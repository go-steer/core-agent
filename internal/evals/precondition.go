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

package evals

import "strings"

// Preconditions (#1061).
//
// A case asserts things about the agent's behaviour. It also *assumes*
// things about the process it is grading — and until this tier existed,
// it could not say so, which produced a specific and quiet failure:
//
// skill-steer-delegated-subject grades whether a skill can change which
// subject a delegated task is about. The steer arrives through a
// SKILL.md the fixture ships in the workspace, discovered from cwd with
// no flag. The fixture's probes confirm that file was MATERIALIZED. They
// cannot confirm it was LOADED. If skill discovery from cwd regressed —
// a change to config.Find's walk, to Load's fs.ErrNotExist tolerance, to
// Runner.exec's cmd.Dir, to copyTree's handling of dotfiles — then no
// skill loads, there is no steer, the case has nothing to resist, and it
// goes GREEN. Not "the framing held" but "there was no pressure on it".
// A check that cannot fail is worse than no check, because it occupies
// the slot where a real one would go.
//
// # Why this is not a witness
//
// Witnesses are files the WORLD wrote, never files this process
// produced, and that asymmetry is load-bearing: a process reporting on
// itself is exactly what a graded check must not trust. Skill load is
// not a fact about the world, though. It is a fact about the process,
// and the process is its only possible source.
//
// The resolution is to keep the two questions apart rather than to relax
// the rule:
//
//   - A CHECK grades behaviour, so its source must be the world or the
//     answer. A failed check is a violation — the agent did the wrong
//     thing.
//   - A PRECONDITION establishes that the case is testing anything at
//     all, so it may read the process's own startup report. A failed
//     precondition is INDETERMINATE — the harness never found out what
//     the agent did. It is never a violation, and it never scores.
//
// That is the same line #652 already draws between exit 1 and exit 2,
// and the filesystem sibling already exists: Role.Probes is "the world
// is the world", and this is "the process is the process".
//
// # Why coupling to the startup summary is acceptable here
//
// It is log-text coupling, which this package otherwise avoids, and the
// reason it is tolerable is the direction it fails in. If
// FormatStartupSummary changes shape, every precondition stops matching
// and every case becomes indeterminate: loud, non-zero, and pointing at
// the harness. The failure it replaces was silent green. A parser that
// can only err towards "I do not know" is a parser worth having, and
// TestStartupPreconditionMatchesTheRealSummary pins the agreement
// against the real producer so the drift is caught at unit time rather
// than at provider-call time.

// SourceStartup is the Source value naming the agent process's own
// startup report. Valid only in Case.Preconditions — a graded check may
// not read it, because then the process would be grading itself.
const SourceStartup = "startup"

// startupLinePrefix is what cmd/core-agent stamps on every line of its
// startup summary (main.go's `send`). Filtering on it keeps a provider
// SDK's chatter on stderr from being mistaken for the agent's own
// report; it is not a boundary against the agent's error lines, which
// carry the same prefix and should be visible to a precondition anyway.
const startupLinePrefix = "core-agent: "

// StartupSource extracts the agent's own startup report from a run's
// stderr.
//
// Present is false when stderr carried no such line at all, which is the
// case that matters most: a process that died before printing its
// summary establishes nothing, and a none_of-only precondition against
// the empty string would otherwise pass. Verify already marks an absent
// source vacuous, and a vacuous precondition is indeterminate.
func StartupSource(stderr string) Source {
	var kept []string
	for _, line := range strings.Split(stderr, "\n") {
		if i := strings.Index(line, startupLinePrefix); i >= 0 {
			kept = append(kept, line[i+len(startupLinePrefix):])
		}
	}
	if len(kept) == 0 {
		return Source{Present: false}
	}
	return Source{Text: strings.Join(kept, "\n"), Present: true}
}
