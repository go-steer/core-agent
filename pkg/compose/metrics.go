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
	"github.com/go-steer/core-agent/v2/pkg/agent"
	"github.com/go-steer/core-agent/v2/pkg/attach"
	"github.com/go-steer/core-agent/v2/pkg/usage"
)

// agentUnwrapper is the capability a registry Registrant exposes when
// it wraps a *agent.Agent (pkg/attachadapter's Adapter does). Kept as
// a local interface so pkg/attach stays free of agent/usage imports.
type agentUnwrapper interface {
	Agent() *agent.Agent
}

// registryTrackerProvider adapts a live SessionRegistry to
// usage.TrackerProvider — every registered session whose Registrant
// wraps a *agent.Agent contributes its tracker on each metrics
// export interval.
type registryTrackerProvider struct {
	reg *attach.SessionRegistry
}

// RegistryTrackerProvider bridges attach-mode sessions into the usage
// metrics observer (usage.RegisterMetrics). Entries whose Registrant
// does not expose the wrapped *agent.Agent (custom hosts registering
// their own Registrant implementations) or whose agent has a nil
// tracker are silently skipped — those sessions won't appear in
// usage metrics.
//
// The Entry's identity triple is authoritative for the metric
// attributes: it is complete from registration time, whereas the
// agent's own fields may lag construction.
func RegistryTrackerProvider(reg *attach.SessionRegistry) usage.TrackerProvider {
	if reg == nil {
		return nil
	}
	return &registryTrackerProvider{reg: reg}
}

// Trackers implements usage.TrackerProvider.
func (p *registryTrackerProvider) Trackers() []usage.TrackedSession {
	entries := p.reg.List()
	out := make([]usage.TrackedSession, 0, len(entries))
	for _, e := range entries {
		uw, ok := e.Agent.(agentUnwrapper)
		if !ok {
			continue
		}
		a := uw.Agent()
		if a == nil || a.Tracker() == nil {
			continue
		}
		out = append(out, usage.TrackedSession{
			Tracker:   a.Tracker(),
			SessionID: e.SessionID,
			AppName:   e.AppName,
			UserID:    e.UserID,
		})
	}
	return out
}
