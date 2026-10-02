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

// Package childenv keeps the daemon's own credentials out of the
// processes it starts.
//
// The attach bearer token is the credential that answers the agent's
// permission prompts. Inherited by the bash tool, it let the agent
// approve its own pending prompt, add a standing allow rule, or reset a
// tripped guardrail — not by a direct curl, which is itself a gated and
// visible call, but by test code it wrote and an approved `go test` then
// ran (#1157). An alert target's bearer_env, inherited the same way, let
// it post to the target as the daemon.
//
// Two things are needed and neither is sufficient alone:
//
//   - Environ returns the parent environment with every withheld name
//     removed. Every exec site the daemon owns — the bash tool, hooks,
//     MCP stdio servers, sciontool — builds its child's environment from
//     it instead of inheriting.
//   - Protect makes the daemon non-dumpable. Without it the first item
//     is cosmetic: /proc/<pid>/environ is the kernel's copy of the
//     environment the process was STARTED with, unaffected by anything
//     the process does to its own environment afterwards, and readable
//     by any same-uid process — so `cat /proc/$PPID/environ` from a
//     scrubbed child recovers the token verbatim. That was measured, not
//     assumed, before this package was written.
//
// What neither can close: an ANCESTOR's environment. A daemon started
// from a shell that exported the token leaves the token in that shell's
// /proc/<pid>/environ, which the agent can read just as well and which
// nothing in this process controls. In a container the daemon is PID 1
// and has no such ancestor; on a workstation, pass the token into the
// daemon's environment only (`TOKEN=… core-agent …`), not by `export`.
//
// The withheld set is process-global because the environment is.
package childenv

import (
	"os"
	"sort"
	"strings"
	"sync"
)

var (
	mu       sync.RWMutex
	withheld = map[string]struct{}{}
)

// Withhold adds env var names to the set no child process inherits.
// Empty names are ignored, so callers can pass optional config fields
// straight through. Safe for concurrent use; there is no way to un-
// withhold a name, because nothing should need one.
func Withhold(names ...string) {
	mu.Lock()
	defer mu.Unlock()
	for _, n := range names {
		if n != "" {
			withheld[n] = struct{}{}
		}
	}
}

// IsWithheld reports whether name is in the withheld set. Interpolation
// consults it: `${env:NAME}` splices a value into text the model reads,
// which is a more direct route to the model than any child process.
func IsWithheld(name string) bool {
	mu.RLock()
	defer mu.RUnlock()
	_, ok := withheld[name]
	return ok
}

// Withheld returns the withheld names, sorted, for startup reporting.
func Withheld() []string {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]string, 0, len(withheld))
	for n := range withheld {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Environ returns os.Environ() minus every withheld name. It always
// returns a non-nil slice, so assigning it to exec.Cmd.Env never falls
// back to inheriting the full environment — a nil Env does exactly that,
// and an empty parent environment would otherwise produce one.
func Environ() []string {
	return filter(os.Environ())
}

func filter(env []string) []string {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]string, 0, len(env))
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if _, drop := withheld[name]; drop {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// HoldsCredential reports whether any withheld name is actually set in
// this process's environment — the condition under which Protect is
// worth its cost.
func HoldsCredential() bool {
	mu.RLock()
	defer mu.RUnlock()
	for n := range withheld {
		if v, ok := os.LookupEnv(n); ok && v != "" {
			return true
		}
	}
	return false
}
