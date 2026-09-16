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

package mcp

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"google.golang.org/adk/agent"
	"google.golang.org/adk/tool"
)

// allowlistToolset registers only the tools a server's ServerSpec.Tools
// names, and drops the rest before they are named, declared, or
// listed anywhere.
//
// This is a REGISTRATION decision, not a permission one. The
// permission gate can already deny a call, but denying a tool the
// model was told it has is the shape #759 removed: a tool in the
// catalog is a promise, and a model that plans around a promise the
// runtime will refuse has been misled by its own instructions.
// Mounting container.googleapis.com/mcp to reach one mutating verb
// otherwise advertises delete_k8s_resource and the cluster-lifecycle
// verbs in the same breath.
//
// Innermost in the wrap stack, so names here are the ones the SERVER
// exposes — same key space as ServerSpec.ToolNotes, and for the same
// reason: the namespace prefix is this server's own key in the
// enclosing object, so repeating it in every entry is noise that goes
// stale the moment the key is renamed.
type allowlistToolset struct {
	inner tool.Toolset
	allow map[string]bool

	// mu guards exposed, which is written on every Tools() call
	// (MCP toolsets resolve lazily, and a reconnect can change the
	// list) and read by unmatchedToolAllowlist afterwards.
	mu      sync.Mutex
	exposed []string
}

// withToolAllowlist filters inner down to the named tools. An empty
// list means no filtering at all — that is the default and the
// behaviour every config written before this had, so absent must not
// be read as "expose nothing".
//
// Returns the concrete type as a second value because startOne needs
// a handle on it to report unmatched names: the warning has to know
// what the server exposed BEFORE the filter ran, and by the time the
// composed toolset answers Tools() that list is gone.
func withToolAllowlist(inner tool.Toolset, allow []string) (tool.Toolset, *allowlistToolset) {
	if inner == nil || len(allow) == 0 {
		return inner, nil
	}
	set := make(map[string]bool, len(allow))
	for _, n := range allow {
		set[n] = true
	}
	ts := &allowlistToolset{inner: inner, allow: set}
	return ts, ts
}

func (a *allowlistToolset) Name() string { return a.inner.Name() }

func (a *allowlistToolset) Tools(ctx agent.ReadonlyContext) ([]tool.Tool, error) {
	upstream, err := a.inner.Tools(ctx)
	if err != nil {
		return nil, err
	}
	seen := make([]string, 0, len(upstream))
	out := make([]tool.Tool, 0, len(upstream))
	for _, t := range upstream {
		seen = append(seen, t.Name())
		if a.allow[t.Name()] {
			out = append(out, t)
		}
	}
	sort.Strings(seen)
	a.mu.Lock()
	a.exposed = seen
	a.mu.Unlock()
	return out, nil
}

// unmatchedToolAllowlist reports allowlist entries that named no tool
// this server actually exposed.
//
// Same treatment as an unmatched tool_notes key, for the same reason:
// the failure mode is silence. A typo'd entry reads correct, the tool
// it meant to admit is simply absent, and the only symptom is a model
// that never reaches for a capability the operator believes it has.
// So the mismatch is named alongside the names that WERE available —
// "no such tool" without the list is half an error message.
//
// A warning and not an error, matching tool_notes: the server is
// working and its other tools are fine. Unlike tool_notes the cost of
// a typo here is higher (a missing tool, not a missing sentence),
// which is an argument for noticing it, not for refusing to boot the
// rest of the cluster's read surface over it.
//
// Returns nil when nothing is wrong, and nil when no allowlist was
// configured — the second case is a caller passing the nil handle
// withToolAllowlist returns for an absent list.
func unmatchedToolAllowlist(a *allowlistToolset) []string {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	exposed := a.exposed
	a.mu.Unlock()
	// Tools() never ran (the server failed to connect), so there is
	// nothing to compare against and every name would look wrong.
	if exposed == nil {
		return nil
	}
	have := make(map[string]bool, len(exposed))
	for _, n := range exposed {
		have[n] = true
	}
	var missing []string
	for n := range a.allow {
		if !have[n] {
			missing = append(missing, n)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	return []string{fmt.Sprintf(
		"tools allowlist names %d tool(s) this server does not expose: %s. Exposed: %s",
		len(missing), strings.Join(missing, ", "), strings.Join(exposed, ", "))}
}
