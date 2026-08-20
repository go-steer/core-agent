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

import "github.com/go-steer/core-agent/v2/pkg/agentenv"

// newEnvInterpolator builds the ${env:VAR} interpolator threaded into
// every content loader in the process, together with the tracker that
// watches what comes out the other side.
//
// Boot-time loads are what the diagnostic reports on: the parent's
// instruction files and skills, and each declarative subagent's own
// instructions, root AGENTS.md and root skills/ tree. The multi-session
// factory's per-session re-loads get the same interpolator rather than
// their own, so no call site can regress to the nil path; they re-read
// the files the boot scan already covered, so in practice they add
// nothing to the report, and nothing re-emits it if they ever do.
//
// The returned function is non-nil even when r is nil, and that is the
// fix for #712 rather than an implementation detail. Pre-fix,
// r.InterpolateFunc() on a nil resolver returned nil, every loader read
// nil as "no interpolation" and passed bodies through untouched, and
// nothing observed that the persona still said
// ${env:GOOGLE_CLOUD_PROJECT}. Wrapping makes the no-manifest path flow
// through the same scan as the manifest path, so the loud-at-boot
// diagnostic does not have to be inferred from the manifest's absence.
//
// Callers emit tracker.Warning(agentsDir) once every loader has run.
func newEnvInterpolator(r *agentenv.Resolver) (func(string) string, *agentenv.ResidualRefs) {
	residual := &agentenv.ResidualRefs{}
	return residual.Track(r), residual
}
