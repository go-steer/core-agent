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
	"github.com/go-steer/core-agent/v2/pkg/attach"
	"github.com/go-steer/core-agent/v2/pkg/permissions"
)

// RecordPermModeChange writes the audit row for an operator's
// permission-mode change (#1168). caller is the verified identity, empty
// for the in-process TUI or an unverified transport. Goes through the
// out-of-band queue because a mode change typically lands mid-turn —
// often while the very prompt that motivated it is pending — and an
// out-of-band Get-then-Append there trips ADK's optimistic-concurrency
// check (the #565 lesson; see queueOutOfBandEvent).
//
// Call it AFTER Gate.SetMode, and only when the mode actually changed.
func (a *Agent) RecordPermModeChange(from, to permissions.Mode, caller string) {
	if a == nil || from == to {
		return
	}
	a.queueOutOfBandEvent(attach.NewPermModeAuditEvent(caller, from, to))
}
