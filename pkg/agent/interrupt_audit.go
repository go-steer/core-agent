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

package agent

import (
	"context"

	"google.golang.org/adk/session"

	"github.com/go-steer/core-agent/v2/pkg/attach"
)

// MarkInterruptPending is the agent side of attach.InterruptSelfAuditor.
// The attach /interrupt handler calls it (via the adapter) after an
// operator cancel actually fired, instead of appending the audit row
// out-of-band. The row is then written from the post-turn cleanup (see
// drainInterruptAudit) once the interrupted turn has fully unwound.
//
// Deferring the write this way is the whole fix for #565: the handler's
// out-of-band Get-then-AppendEvent bumped the live session row's
// last_update_time while the runner was still flushing the interrupted
// turn, tripping ADK's optimistic-concurrency check so the operator's
// clean cancel surfaced as an opaque "stale session error". Riding the
// turn loop moves the write to the one window with no live runner handle.
func (a *Agent) MarkInterruptPending() {
	if a == nil {
		return
	}
	a.pendingInterruptAudit.Store(true)
}

// drainInterruptAudit appends the operator-interrupt audit row if one is
// pending, then clears the flag. It runs from the post-turn cleanup
// closure in Run, AFTER the runner's event stream has fully drained and
// its session handle is released — so this write can't collide with the
// runner's in-flight AppendEvents (#565).
//
// Best-effort, mirroring the handler fallback it replaces: a nil
// eventlog or a Get/Append failure is swallowed. Uses a fresh context
// because the turn's runCtx is already cancelled by the time cleanup
// runs (the same reason recordInvocation uses context.Background here).
//
// In the vanishingly rare case where the interrupt is marked after this
// turn's cleanup already ran, the flag simply drains on the next turn —
// the audit still lands, just deferred; it never collides and never
// double-writes (Swap clears the flag atomically).
func (a *Agent) drainInterruptAudit() {
	if a == nil || !a.pendingInterruptAudit.Swap(false) {
		return
	}
	if a.eventLog == nil {
		return
	}
	getResp, err := a.eventLog.Service.Get(context.Background(), &session.GetRequest{
		AppName:   a.appName,
		UserID:    a.userID,
		SessionID: a.sessionID,
	})
	if err != nil {
		return
	}
	_ = a.eventLog.Service.AppendEvent(context.Background(), getResp.Session, attach.NewInterruptAuditEvent())
}
