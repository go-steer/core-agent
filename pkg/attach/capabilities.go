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
	"sort"

	"github.com/go-steer/core-agent/v2/internal/version"
	"github.com/go-steer/core-agent/v2/pkg/auth"
)

// capabilitiesBuilder returns a closure that produces a per-request
// Capabilities snapshot for the given server-level state. Bound to
// Server.Bind so the closure captures Options-derived facts
// (multi_session, cross_daemon, agent card) once at startup; the
// per-entry / per-caller state is resolved on each call.
//
// The returned closure only populates the v1.4.0 additive fields —
// Broadcaster.deliverBootFrames preserves ProtocolVersion, EventTypes,
// and Server so the wire-format invariants stay owned by one place.
//
// serverFeatures pre-computes the daemon-level feature flags
// (multi_session, cross_daemon) so every call only walks the entry-
// scoped ones. Nil-safe: an empty map is fine.
func capabilitiesBuilder(opts Options) func(ctx context.Context, entry *Entry) Capabilities {
	serverFeatures := map[string]bool{
		FeatureMultiSession: opts.MultiSessionEnabled,
		FeatureCrossDaemon:  opts.PeerRegistry != nil,
	}
	card := opts.AgentCard
	return func(ctx context.Context, entry *Entry) Capabilities {
		return Capabilities{
			Features:      buildFeatures(entry, serverFeatures),
			SlashCommands: buildSlashCommands(entry),
			Agent:         buildAgentIdentity(entry, card),
			CallerID:      callerIDFromContext(ctx),
		}
	}
}

// buildFeatures merges the pre-computed server-level flags with the
// entry-scoped ones (probed via optional capability-interface
// presence). Returns a copy so callers can mutate the map without
// racing with other subscribers.
func buildFeatures(entry *Entry, serverFeatures map[string]bool) map[string]bool {
	out := make(map[string]bool, len(serverFeatures)+6)
	for k, v := range serverFeatures {
		out[k] = v
	}
	if entry == nil || entry.Agent == nil {
		return out
	}
	if _, ok := entry.Agent.(PromptBrokerProvider); ok {
		out[FeaturePermsStream] = true
	}
	if _, ok := entry.Agent.(MCPProvider); ok {
		out[FeatureMCP] = true
	}
	if _, ok := entry.Agent.(SubagentSpawner); ok {
		out[FeatureSpecialists] = true
	}
	if _, ok := entry.Agent.(InterruptProvider); ok {
		out[FeatureInterrupt] = true
	}
	// FeatureCostCeiling + FeatureObserverMode are reserved keys —
	// no capability interface for them today. Emitting them as false
	// (rather than omitting) advertises "server understands the key
	// name; the answer is no." Consumers that treat absent-key as
	// off see the same behavior either way; consumers that
	// distinguish absent from false get a truthful "no."
	out[FeatureCostCeiling] = false
	out[FeatureObserverMode] = false
	return out
}

// buildSlashCommands probes for each async slash provider interface
// and returns the sorted set of names the agent will accept. Mirrors
// the wire routes registered in handlers_operator.go:77-87.
func buildSlashCommands(entry *Entry) []string {
	if entry == nil || entry.Agent == nil {
		return nil
	}
	var out []string
	if _, ok := entry.Agent.(CompactSlashProvider); ok {
		out = append(out, "compact")
	}
	if _, ok := entry.Agent.(CheckpointSlashProvider); ok {
		out = append(out, "done")
	}
	if _, ok := entry.Agent.(SideQueryProvider); ok {
		out = append(out, "btw")
	}
	if _, ok := entry.Agent.(SubagentSpawner); ok {
		out = append(out, "subagent")
	}
	if _, ok := entry.Agent.(ReplanProvider); ok {
		out = append(out, "replan")
	}
	sort.Strings(out)
	return out
}

// buildAgentIdentity assembles the capabilities.agent block from the
// AgentCardConfig (name/description/version/url) and the registrant's
// StatusProvider (model). Returns nil when neither source has any
// data — consumers omit the block entirely.
//
// Provider stays empty for now — StatusInfo doesn't carry a provider
// field and AgentCardConfig.Provider carries an ADK-style organization
// URL, not a routing provider tag. A follow-up can wire an optional
// ProviderProvider capability without a spec bump.
func buildAgentIdentity(entry *Entry, card AgentCardConfig) *AgentIdentity {
	id := &AgentIdentity{
		Name:        card.Name,
		Version:     card.Version,
		Description: card.Description,
		URL:         card.ExternalURL,
	}
	if entry != nil && entry.Agent != nil {
		if id.Name == "" {
			id.Name = entry.AppName
		}
		if id.Description == "" {
			if dp, ok := entry.Agent.(DescriptionProvider); ok {
				id.Description = dp.Description()
			}
		}
		if sp, ok := entry.Agent.(StatusProvider); ok {
			id.Model = sp.AttachStatus().ModelName
		}
	}
	if id.Version == "" {
		id.Version = version.Version
	}
	// Drop empty AgentIdentity — nothing worth advertising.
	if *id == (AgentIdentity{}) {
		return nil
	}
	return id
}

// callerIDFromContext returns the resolved caller identity for the
// display-hint field, or "" when the middleware didn't stamp one
// (typical for tests that call Broadcaster.Subscribe with a bare
// context.Background).
func callerIDFromContext(ctx context.Context) string {
	c, ok := auth.CallerFromContext(ctx)
	if !ok {
		return ""
	}
	return c.Identity
}
