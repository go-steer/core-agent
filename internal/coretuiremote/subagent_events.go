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

package coretuiremote

import (
	"context"
	"errors"

	coretui "github.com/go-steer/core-tui/tui"

	"github.com/go-steer/core-agent/v2/internal/attachclient"
	"github.com/go-steer/core-agent/v2/internal/coretuievent"
)

// SubagentEvents is the turn-log half of coretui.SubagentReporter (core-tui
// v0.18.0) over GET /sessions/<sid>/agents/<name>/events. Backs the
// `/subagents <name>` drill-down overlay and the live tail that grows
// under a running sync subagent's tool row.
//
// core-tui calls this off its render path with a bounded context (5s
// for an overlay fetch, 3s per tail tick) and discards late results, so
// this can — and does — pass ctx straight through: a wedged daemon
// costs one dropped poll, not a frozen TUI.
//
// Page size is left to the server. The overlay pages through with the
// returned cursor, so a larger explicit limit would only trade latency
// on the first paint for fewer round trips on a history nobody scrolls
// to the end of.
func (a *Adapter) SubagentEvents(ctx context.Context, name string, since int64) (coretui.SubagentEventPage, error) {
	resp, err := a.client.SubagentEvents(ctx, a.sessionPath, name, since, 0)
	if err != nil {
		// Project the server's 404-with-alternatives into core-tui's
		// typed form so the overlay can name the subagents that DO
		// exist. Everything else — session gone, auth, transport —
		// stays verbatim; core-tui renders it as an error row and
		// keeps whatever turns it already had.
		var nf *attachclient.SubagentNotFoundError
		if errors.As(err, &nf) {
			return coretui.SubagentEventPage{}, &coretui.SubagentNotFoundError{
				Name:      nf.Name,
				Available: nf.Available,
			}
		}
		return coretui.SubagentEventPage{}, err
	}

	page := coretui.SubagentEventPage{
		NextSince: resp.NextSince,
		Truncated: resp.Truncated,
		Events:    make([]coretui.SubagentEvent, 0, len(resp.Events)),
	}
	for _, f := range resp.Events {
		ev, ok := coretuievent.Subagent(f.Seq, f.Event)
		if !ok {
			continue
		}
		page.Events = append(page.Events, ev)
	}
	return page, nil
}

// Compile-time check: *Adapter must satisfy coretui.SubagentReporter.
// core-tui discovers the capability by type assertion, so a rename or a
// signature drift would otherwise degrade silently to "this host has no
// turn log" — the drill-down would just stop working with no build
// error and no runtime complaint.
//
// core-tui v0.21.0 merged SubagentLister and SubagentEventReader into
// this one interface: listing the roster and reading a name's turns are
// not things a host can plausibly do separately.
var _ coretui.SubagentReporter = (*Adapter)(nil)
