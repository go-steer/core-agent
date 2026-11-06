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

package compose

import (
	"context"
	"fmt"
	"os"

	"github.com/go-steer/core-agent/v3/pkg/agent"
)

// evictFunc is what the registry runs when it evicts a tenant session:
// stop the wake loop, give up the session's residency lease (which also
// refuses any write still in flight from the turn or a subagent), and
// tear down its subagents. The subagents matter because their goroutines run
// under context.WithoutCancel, so cancelling the wake loop leaves them
// running against a session nobody can reach; their wait runs off the
// caller's goroutine so one wedged subagent can't stall the registry's
// eviction sweep across every other tenant.
func evictFunc(cancelLoop context.CancelFunc, bgClose func(), releaseResidency func()) context.CancelFunc {
	return func() {
		cancelLoop()
		releaseResidency()
		if bgClose != nil {
			go bgClose()
		}
	}
}

// holdResidency makes a tenant session resident on this daemon and ties
// it to its residency lease (docs/external-state-design.md decisions
// 1–2, #1192). It registers the loss handler first and then takes the
// lease, so a loss can never fall through to the process-wide fallback
// in between: losing the lease evicts the session — wake loop stopped,
// nothing more written (the Handle already fences writes) — rather than
// leaving it running against a tree another instance now owns. A
// session resident on another instance fails here with
// ErrSessionLocked, which is the resume's answer (decision 1). It
// returns the release to run on eviction. A Handle without a residency
// lease makes all of it a no-op.
func holdResidency(deps SessionFactoryDeps, ag *agent.Agent) (func(), error) {
	h := deps.EventlogHandle
	if h == nil || ag == nil {
		return func() {}, nil
	}
	app, user, sid := ag.AppName(), ag.UserID(), ag.SessionID()
	unwatch := h.WatchResidency(app, user, sid, func(stolen bool) {
		cause := "the control store was unreachable for the loss window"
		if stolen {
			cause = "another instance took it"
		}
		fmt.Fprintf(os.Stderr, "core-agent: session %s lost its residency lease (%s); evicting it without further writes\n", sid, cause)
		if deps.Registry != nil {
			deps.Registry.Unregister(app, user, sid)
		}
	})
	ctx := deps.DaemonCtx
	if ctx == nil {
		ctx = context.Background()
	}
	if err := h.AcquireResidency(ctx, app, user, sid); err != nil {
		unwatch()
		return nil, fmt.Errorf("session %s: %w", sid, err)
	}
	return func() {
		unwatch()
		h.ReleaseResidency(app, user, sid)
	}, nil
}
