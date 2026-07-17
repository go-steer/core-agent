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

// Package hooks dispatches shell commands on agent-side event boundaries.
//
// Motivation and shape: an agent produces a stream of tool-call, tool-
// response, and model-text events. Operators occasionally want to observe
// these boundaries from outside the process — to write lifecycle files
// (Scion's agent-info.json via `sciontool hook`), forward alerts to a
// notification service, feed a custom telemetry pipeline, etc. Rather
// than building N adapter binaries that each duplicate cmd/core-agent's
// wiring, this package lets consumers declare `{event: [{command}]}`
// mappings in .agents/config.json and have the agent spawn those
// commands with a JSON envelope on stdin whenever an event fires.
//
// The wire shape (JSON payload + `hook_event_name` top-level field) is
// the same one Scion's `sciontool hook --dialect=<name>` expects, so
// the Scion integration is just `{"tool-start": [{"command":
// "sciontool hook --dialect=core-agent"}]}` — no core-agent code
// knows anything about Scion. Non-Scion consumers see the same envelope
// and can parse it however they like.
package hooks

import (
	"fmt"
	"sort"
)

// DefaultTimeoutSeconds is the per-invocation subprocess timeout applied
// when a Handler doesn't set TimeoutSeconds. Matches the 10-second cap
// Scion's Antigravity harness uses for its own hook config, since the
// same commands (`sciontool hook`) are the primary consumer.
const DefaultTimeoutSeconds = 10

// KnownEvents is the set of hook event names the dispatcher may fire.
// Names deliberately match Scion's normalized event vocabulary
// (pkg/sciontool/hooks/types.go in scion) so a Scion dialect.yaml can
// use an identity mapping.
//
// New events land here when the dispatcher gains a code path that
// emits them — no more, no less. Validate() rejects config entries for
// event names not in this set so typos fail loudly.
var KnownEvents = []string{
	"tool-start",  // fires on each FunctionCall part in an event
	"tool-end",    // fires on each FunctionResponse part in an event
	"model-start", // fires on the first Partial=true Text after turn start / tool-end
	"agent-end",   // fires from the post-turn cleanup once per turn
}

// Config is the on-disk shape loaded from `.agents/config.json`
// under `"hooks"`. Keys are hook event names (see KnownEvents);
// values are the ordered list of handlers to run when the event
// fires. Empty or nil is valid — the dispatcher becomes a no-op.
//
// Handlers run sequentially per event — parallelism between them
// would race on stdout/stderr writes and complicate cleanup with
// negligible wall-clock benefit (handlers are typically fast
// subprocesses).
type Config map[string][]Handler

// Handler declares one command to spawn when the parent event fires.
type Handler struct {
	// Command is passed to `/bin/sh -c` so pipes, redirections, and
	// shell substitutions work. Matches the Antigravity harness pattern
	// (`jq ... | sciontool hook --dialect=...`).
	Command string `json:"command"`

	// TimeoutSeconds bounds the subprocess wall-clock; the process is
	// killed if it hasn't exited by then. Zero means DefaultTimeoutSeconds.
	// Explicit negative values are rejected by Validate.
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`
}

// Validate rejects malformed configs at startup so operators find out
// about typos before the agent runs the first turn.
func (c Config) Validate() error {
	if len(c) == 0 {
		return nil
	}
	known := make(map[string]struct{}, len(KnownEvents))
	for _, e := range KnownEvents {
		known[e] = struct{}{}
	}
	// Sorted iteration so the error message stays stable for tests.
	names := make([]string, 0, len(c))
	for name := range c {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if _, ok := known[name]; !ok {
			return fmt.Errorf("hooks: unknown event %q (valid: %v)", name, KnownEvents)
		}
		for i, h := range c[name] {
			if h.Command == "" {
				return fmt.Errorf("hooks: event %q handler #%d: command is required", name, i)
			}
			if h.TimeoutSeconds < 0 {
				return fmt.Errorf("hooks: event %q handler #%d: timeout_seconds must be non-negative (got %d)", name, i, h.TimeoutSeconds)
			}
		}
	}
	return nil
}
