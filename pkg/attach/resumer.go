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

package attach

import (
	"context"

	"github.com/go-steer/core-agent/pkg/auth"
)

// SessionResumer reconstructs a session that exists on disk
// (persisted ACL row) but not in the current daemon's in-memory
// SessionRegistry. Called by Registry.Lookup / LookupSingle on miss
// when a Resumer is configured.
//
// Implementations:
//
//  1. Look up the persisted ACL row by (app, sid) — typically via
//     SessionACLStore.FindByAppSID.
//  2. Materialize the original Caller from the row's Owner.
//  3. Reconstruct the agent using the daemon's SessionFactory shape
//     with the EXPLICIT sessionID (not a freshly minted one) so
//     ADK's session.Service reattaches the prior conversation
//     history from the eventlog.
//  4. Return the new Registrant + the persisted ACL.
//
// The caller (Registry.Lookup) registers the returned Registrant
// under the returned ACL via the internal registerResumed path.
//
// Return ErrSessionACLNotFound when no ACL row exists for the
// triple — the registry maps that to ErrSessionNotFound so the
// handler returns 404. Any other error surfaces as 500 with the
// resume-failure message (see docs/session-resume-design.md OQ #2).
//
// Implementations live in cmd/core-agent (see buildSessionResumer)
// because they need sessionFactoryDeps — model, gate template,
// tools, eventlog handle, MCP servers, … all cmd-level wiring.
// The interface stays in pkg/attach so the handlers can consult it
// without importing cmd/core-agent.
type SessionResumer interface {
	Resume(ctx context.Context, app, sid string) (Registrant, auth.SessionACL, error)
}
