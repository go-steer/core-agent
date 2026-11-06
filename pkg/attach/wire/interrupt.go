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

package wire

import (
	"context"

	"google.golang.org/adk/v2/session"
)

// InterruptSelfAuditor is the optional capability a registrant
// implements when it records the operator-interrupt audit event itself,
// from inside its own serialized turn loop, instead of leaving the
// /interrupt handler to append it out-of-band.
//
// The handler's fallback (appendInterruptAudit in pkg/attach) does a
// Get-then-AppendEvent on the live session row while the interrupted
// turn is still flushing its final events. That extra write bumps the
// row's last_update_time and can trip the runner's optimistic-
// concurrency check mid-flush, so the operator's clean cancel surfaces
// as an opaque "stale session error" instead of ctx.Canceled (#565).
//
// A registrant that implements this interface takes ownership of the
// audit: MarkInterruptPending records the intent, and the registrant
// appends the audit row once the interrupted turn unwinds — the one
// window with no live runner handle racing the write. When a registrant
// implements it, the /interrupt handler calls MarkInterruptPending and
// skips its own append.
type InterruptSelfAuditor interface {
	// MarkInterruptPending records that an operator interrupt fired and
	// an audit row should be appended once the current turn unwinds.
	// Called from the /interrupt handler only after AttachInterrupt
	// reported an in-flight turn was cancelled. Safe to call
	// concurrently with the registrant's turn loop.
	MarkInterruptPending()
}

// NewInterruptAuditEvent builds the eventlog row that records an
// operator-initiated interrupt. Author + CustomMetadata identify the
// source so a later audit query (or attach /events tail) can
// distinguish operator cancels from any other ctx.Canceled flowing
// through the agent loop. Shared by the handler's out-of-band fallback
// (appendInterruptAudit) and by self-auditing registrants
// (InterruptSelfAuditor) so the two paths can never drift (#565).
func NewInterruptAuditEvent() *session.Event {
	ev := session.NewEvent(context.Background(), "attach-interrupt")
	ev.Author = "attach/interrupt"
	ev.CustomMetadata = map[string]any{"source": "operator"}
	return ev
}
