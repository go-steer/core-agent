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

// Agent-side wiring for the event-hook observer surface (pkg/hooks).
// The hooks package doesn't depend on pkg/agent — this file is the
// bridge that fans events from the agent's Run iterator out to the
// two callbacks the operator wired via WithEventHook.

package agent

import (
	"google.golang.org/adk/session"
)

// WithEventHook wires per-event and end-of-turn observer callbacks.
// onEvent is called once per session.Event as events stream from
// Agent.Run — from inside the same iterator tap the watchdog and
// usage tracker already sit in, so observation is synchronous and
// ordered relative to the event yield. onTurnEnd is called from the
// post-turn cleanup that runs after wrapWithCleanup drains the
// iterator, alongside the watchdog / compaction / checkpoint hooks.
//
// Either callback may be nil to disable that half of the surface.
// Both nil is legal and turns the option into a no-op; useful when
// a consumer wraps WithEventHook in a builder that always sets it.
//
// Consumer contract: callbacks must not panic and should return
// quickly. A slow callback stalls the agent's event stream —
// synchronous by design so the hook mechanism can rely on ordering
// (matches pkg/hooks.Dispatcher, which spawns subprocesses with
// per-command timeouts to bound its own latency).
//
// Single-slot: calling WithEventHook twice replaces the previous
// binding. Multi-consumer fan-out is the caller's responsibility
// (wrap two callbacks in one). This matches WithWatchdog's shape.
func WithEventHook(onEvent func(*session.Event), onTurnEnd func()) Option {
	return func(o *options) {
		o.onEvent = onEvent
		o.onTurnEnd = onTurnEnd
	}
}
