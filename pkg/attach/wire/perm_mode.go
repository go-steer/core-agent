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

	"github.com/go-steer/core-agent/v3/pkg/permissions"
)

// Durable audit row for a permission-mode change (#1168, protocol
// 1.16.0). The request/response shapes and the handler stay in
// pkg/attach; only the row the agent writes lives here.
const (
	// PermModeEventName is the event name of a mode-change row.
	PermModeEventName = "attach-perm-mode"
	// PermModeEventAuthor authors a mode-change row.
	PermModeEventAuthor = "attach/perm-mode"
)

const (
	permModeMetaSource = "source"
	permModeMetaFrom   = "from"
	permModeMetaTo     = "to"
	permModeMetaCaller = "caller"
)

// NewPermModeAuditEvent builds the durable row recording an operator's
// mode change. It is an audit record only: a restarted or resumed
// session comes back in its configured mode, not the last one an
// operator picked, so nothing folds these rows back into state.
// identity is omitted when empty rather than written as a placeholder,
// so the row never claims an attribution the daemon did not verify.
func NewPermModeAuditEvent(identity string, from, to permissions.Mode) *session.Event {
	ev := session.NewEvent(context.Background(), PermModeEventName)
	ev.Author = PermModeEventAuthor
	meta := map[string]any{
		permModeMetaSource: "operator",
		permModeMetaFrom:   string(from),
		permModeMetaTo:     string(to),
	}
	if identity != "" {
		meta[permModeMetaCaller] = identity
	}
	ev.CustomMetadata = meta
	return ev
}
