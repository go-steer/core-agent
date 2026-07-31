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
	"errors"
	"fmt"
	"os"
	"time"

	"google.golang.org/adk/session"

	"github.com/go-steer/core-agent/v2/pkg/agent"
	"github.com/go-steer/core-agent/v2/pkg/auth"
	"github.com/go-steer/core-agent/v2/pkg/eventlog"
)

// maybeAutoContinue implements the lazy-resume trigger of
// docs/auto-continue-design.md (#539 PR 1): called from
// ReproduceAgent for origin=="resumed" sessions when the feature is
// enabled. It classifies the committed tail, applies the freshness
// window, takes the session run lock for fleet mutual exclusion, and
// queues the synthesized continuation into the agent's inbox — the
// wake loop (started by the caller right after) drains it as the
// session's first turn.
//
// Lock staging note: v1 holds agent_run_lock across detection +
// injection only, not across the continuation turn itself (the turn
// runs asynchronously in the wake loop; holding a lease across it
// needs a turn-end hook this path doesn't have). The residual window
// — two shared-DB daemons both lazily resuming the same session
// after one's injection but before its turn commits — narrows with
// the boot scan's synchronous driving in PR 2. The design doc's
// implementation notes record this deviation.
//
// Every skip path returns silently or with one stderr line; resume
// itself must never fail because auto-continue couldn't run.

// autoContinueScanWindow bounds the tail read. A window this size is
// classification-safe: the interrupted tail is by definition the last
// conversational event, and only annotation rows (audit, checkpoints,
// notes) can trail it — never dozens of them.
const autoContinueScanWindow = 128

func maybeAutoContinue(deps SessionFactoryDeps, caller auth.Caller, sid string, ag *agent.Agent) {
	// Fleet exclusion first — the lock is one cheap DB write, and
	// classifying under it means two daemons can't both read the
	// same interrupted tail.
	lock, err := deps.EventlogHandle.AcquireLock(deps.DaemonCtx, "core-agent", caller.Identity, sid)
	if err != nil {
		if errors.Is(err, eventlog.ErrSessionLocked) {
			return // another daemon (or an autonomous run) owns the session right now
		}
		fmt.Fprintf(os.Stderr, "core-agent: session %s: auto-continue: acquire run lock: %v\n", sid, err)
		return
	}
	defer func() { _ = lock.Release() }()

	// Bounded read (same window rationale as tail repair): the
	// classifier only needs the last conversational event plus
	// whatever annotations trail it, and this runs synchronously on
	// the resume path while the touching HTTP request waits — a
	// full-session scan on a 100k-event session would break the
	// "resume stays fast" promise.
	resp, err := deps.EventlogHandle.Service.Get(deps.DaemonCtx, &session.GetRequest{
		AppName:         "core-agent",
		UserID:          caller.Identity,
		SessionID:       sid,
		NumRecentEvents: autoContinueScanWindow,
	})
	if err != nil || resp == nil || resp.Session == nil {
		if err != nil {
			fmt.Fprintf(os.Stderr, "core-agent: session %s: auto-continue: read session: %v\n", sid, err)
		}
		return
	}
	var events []*session.Event
	for ev := range resp.Session.Events().All() {
		events = append(events, ev)
	}
	interruptedAt, interrupted := agent.ClassifyInterruptedTail(events)
	if !interrupted {
		return
	}
	if deps.AutoContinueFreshness > 0 && time.Since(interruptedAt) > deps.AutoContinueFreshness {
		fmt.Fprintf(os.Stderr, "core-agent: session %s: interrupted turn is %s old (> freshness %s); waiting for the next message\n",
			sid, time.Since(interruptedAt).Round(time.Second), deps.AutoContinueFreshness)
		return
	}
	if err := ag.InjectAs(agent.AutoContinueNote(interruptedAt), auth.Caller{Identity: agent.AutoContinueOriginator}); err != nil {
		fmt.Fprintf(os.Stderr, "core-agent: session %s: auto-continue: inject: %v\n", sid, err)
		return
	}
	fmt.Fprintf(os.Stderr, "core-agent: session %s: auto-continue queued (turn interrupted %s ago)\n",
		sid, time.Since(interruptedAt).Round(time.Second))
}
