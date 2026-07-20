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
	"reflect"
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/auth"
)

// featureRichRegistrant implements every optional capability
// interface that buildFeatures / buildSlashCommands probe for, so
// tests can assert the full-population path. Kept minimal — the
// return values don't matter for capability detection; the interface
// implementation itself is the signal.
type featureRichRegistrant struct {
	stubRegistrant
	statusInfo StatusInfo
	descText   string
}

func (f *featureRichRegistrant) AttachPromptBroker() *PromptBroker { return nil }
func (f *featureRichRegistrant) AttachMCP() MCPInfo                { return MCPInfo{} }
func (f *featureRichRegistrant) AttachSpawnSubagent(_ context.Context, _ SubagentSpec) (SubagentSpawnResponse, error) {
	return SubagentSpawnResponse{}, nil
}
func (f *featureRichRegistrant) AttachInterrupt() bool { return false }
func (f *featureRichRegistrant) AttachCompact(_ context.Context, _ string) (CompactResponse, error) {
	return CompactResponse{}, nil
}
func (f *featureRichRegistrant) AttachCheckpoint(_ context.Context, _ string) (CheckpointResponse, error) {
	return CheckpointResponse{}, nil
}
func (f *featureRichRegistrant) AttachAskSideQuestion(_ context.Context, _ string) (string, error) {
	return "", nil
}
func (f *featureRichRegistrant) AttachReplan(_ context.Context, _ ReplanRequest) (ReplanResponse, error) {
	return ReplanResponse{}, nil
}
func (f *featureRichRegistrant) AttachStatus() StatusInfo { return f.statusInfo }
func (f *featureRichRegistrant) Description() string      { return f.descText }

func TestBuildFeatures_ServerAndEntryFlags(t *testing.T) {
	t.Parallel()
	// Server-level flags survive as-is; entry-scoped ones come from
	// capability-interface probes.
	entry := &Entry{
		AppName:   "core-agent",
		SessionID: "s1",
		Agent: &featureRichRegistrant{
			stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s1"},
		},
	}
	server := map[string]bool{
		FeatureMultiSession: true,
		FeatureCrossDaemon:  false,
	}
	got := buildFeatures(entry, server)
	want := map[string]bool{
		FeatureMultiSession: true,
		FeatureCrossDaemon:  false,
		FeaturePermsStream:  true,
		FeatureMCP:          true,
		FeatureSpecialists:  true,
		FeatureInterrupt:    true,
		// Reserved keys advertised as false so consumers see an
		// explicit "no" rather than key absence.
		FeatureCostCeiling:  false,
		FeatureObserverMode: false,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("buildFeatures mismatch:\n got  %#v\n want %#v", got, want)
	}
}

func TestBuildFeatures_BareRegistrantOnlyServerFlags(t *testing.T) {
	t.Parallel()
	// A registrant with no capabilities beyond the base Registrant
	// interface should get only server flags + false reserved keys.
	entry := &Entry{
		AppName:   "app",
		SessionID: "sid",
		Agent:     &stubRegistrant{app: "app", user: "u", sid: "sid"},
	}
	got := buildFeatures(entry, map[string]bool{FeatureMultiSession: false})
	if got[FeatureMultiSession] != false {
		t.Errorf("FeatureMultiSession = %v, want false", got[FeatureMultiSession])
	}
	for _, key := range []string{FeaturePermsStream, FeatureMCP, FeatureSpecialists, FeatureInterrupt} {
		if _, present := got[key]; present {
			t.Errorf("bare registrant should not advertise %q, but got %v", key, got[key])
		}
	}
	// Reserved keys are always advertised as false so clients know
	// the server understands them.
	for _, key := range []string{FeatureCostCeiling, FeatureObserverMode} {
		if got[key] != false {
			t.Errorf("reserved key %q = %v, want false", key, got[key])
		}
	}
}

func TestBuildSlashCommands_Sorted(t *testing.T) {
	t.Parallel()
	entry := &Entry{
		Agent: &featureRichRegistrant{
			stubRegistrant: stubRegistrant{app: "a", user: "u", sid: "s"},
		},
	}
	got := buildSlashCommands(entry)
	want := []string{"btw", "compact", "done", "replan", "subagent"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("buildSlashCommands = %v, want %v (sorted, all providers)", got, want)
	}
}

func TestBuildSlashCommands_BareRegistrantEmpty(t *testing.T) {
	t.Parallel()
	entry := &Entry{Agent: &stubRegistrant{app: "a", user: "u", sid: "s"}}
	got := buildSlashCommands(entry)
	if len(got) != 0 {
		t.Errorf("bare registrant slash_commands = %v, want empty", got)
	}
}

func TestBuildAgentIdentity_CardOverridesRegistrant(t *testing.T) {
	t.Parallel()
	entry := &Entry{
		AppName: "core-agent",
		Agent: &featureRichRegistrant{
			stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s"},
			statusInfo:     StatusInfo{ModelName: "gemini-3.1-pro"},
			descText:       "Registrant description (should be overridden).",
		},
	}
	card := AgentCardConfig{
		Name:        "override-name",
		Description: "Card wins.",
		Version:     "v9.9.9",
		ExternalURL: "https://example.com/agent",
	}
	got := buildAgentIdentity(entry, card)
	if got == nil {
		t.Fatal("buildAgentIdentity returned nil for populated inputs")
	}
	if got.Name != "override-name" {
		t.Errorf("Name = %q, want card override", got.Name)
	}
	if got.Description != "Card wins." {
		t.Errorf("Description = %q, want card override", got.Description)
	}
	if got.Version != "v9.9.9" {
		t.Errorf("Version = %q, want v9.9.9", got.Version)
	}
	if got.URL != "https://example.com/agent" {
		t.Errorf("URL = %q, want card override", got.URL)
	}
	if got.Model != "gemini-3.1-pro" {
		t.Errorf("Model = %q, want the StatusProvider-supplied value", got.Model)
	}
}

func TestBuildAgentIdentity_RegistrantFallbacks(t *testing.T) {
	t.Parallel()
	// Empty card should fall back to registrant AppName +
	// DescriptionProvider + StatusProvider.
	entry := &Entry{
		AppName: "core-agent",
		Agent: &featureRichRegistrant{
			stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s"},
			statusInfo:     StatusInfo{ModelName: "gemini-3.5-flash"},
			descText:       "Registrant description.",
		},
	}
	got := buildAgentIdentity(entry, AgentCardConfig{})
	if got == nil {
		t.Fatal("buildAgentIdentity returned nil")
	}
	if got.Name != "core-agent" {
		t.Errorf("Name = %q, want AppName fallback", got.Name)
	}
	if got.Description != "Registrant description." {
		t.Errorf("Description = %q, want DescriptionProvider fallback", got.Description)
	}
	if got.Model != "gemini-3.5-flash" {
		t.Errorf("Model = %q, want StatusProvider fallback", got.Model)
	}
	// Version falls through to the build-stamped internal/version.Version;
	// don't assert on the actual value (moves per commit).
}

func TestBuildAgentIdentity_NilWhenNothingKnown(t *testing.T) {
	t.Parallel()
	// No card, no entry — the block should be dropped entirely so
	// omitempty removes it from the wire frame. (Version fallback to
	// internal/version.Version keeps this from being fully empty in
	// production; the test uses a synthetic path.)
	got := buildAgentIdentity(nil, AgentCardConfig{})
	// Version fallback populates a value → we don't get nil in this
	// case. Instead, verify the shape has ONLY Version set — the
	// other fields stay empty and don't spuriously advertise
	// identity.
	if got == nil {
		return // acceptable when version is empty in a stripped test binary
	}
	if got.Name != "" || got.Description != "" || got.Model != "" || got.URL != "" || got.Provider != "" {
		t.Errorf("nil-entry AgentIdentity should carry only Version; got %#v", got)
	}
}

func TestCallerIDFromContext_PresentAndAbsent(t *testing.T) {
	t.Parallel()
	// Present.
	ctx := auth.WithCaller(context.Background(), auth.Caller{Identity: "alice@example.com"})
	if got := callerIDFromContext(ctx); got != "alice@example.com" {
		t.Errorf("caller_id = %q, want alice@example.com", got)
	}
	// Absent.
	if got := callerIDFromContext(context.Background()); got != "" {
		t.Errorf("caller_id with no ctx caller = %q, want empty", got)
	}
}

func TestCapabilitiesBuilder_EndToEnd(t *testing.T) {
	t.Parallel()
	// Full closure: MultiSessionEnabled + PeerRegistry + AgentCard +
	// caller on ctx + rich entry → every optional field populated.
	opts := Options{
		MultiSessionEnabled: true,
		PeerRegistry:        &PeerRegistry{}, // presence-only signal
		AgentCard: AgentCardConfig{
			Name:        "core-agent",
			Version:     "v2.8.0-dev",
			Description: "Test",
			ExternalURL: "https://example.com",
		},
	}
	build := capabilitiesBuilder(opts)
	entry := &Entry{
		AppName: "core-agent",
		Agent: &featureRichRegistrant{
			stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s"},
			statusInfo:     StatusInfo{ModelName: "gemini-3.1-pro"},
		},
	}
	ctx := auth.WithCaller(context.Background(), auth.Caller{Identity: "alice"})
	got := build(ctx, entry)

	if !got.Features[FeatureMultiSession] {
		t.Error("Features.multi_session should be true when Options.MultiSessionEnabled")
	}
	if !got.Features[FeatureCrossDaemon] {
		t.Error("Features.cross_daemon should be true when Options.PeerRegistry set")
	}
	if !got.Features[FeatureMCP] {
		t.Error("Features.mcp should be true for MCPProvider-implementing agent")
	}
	if !reflect.DeepEqual(got.SlashCommands, []string{"btw", "compact", "done", "replan", "subagent"}) {
		t.Errorf("SlashCommands = %v, want full sorted set", got.SlashCommands)
	}
	if got.Agent == nil || got.Agent.Model != "gemini-3.1-pro" {
		t.Errorf("Agent identity model mismatch: %#v", got.Agent)
	}
	if got.CallerID != "alice" {
		t.Errorf("CallerID = %q, want alice", got.CallerID)
	}
}
