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
	"reflect"
	"testing"
)

// reportingRegistrant satisfies SEVERAL capability interfaces
// structurally (like attachadapter.Adapter satisfies all of them)
// but implements CapabilityReporter to state actual wiredness — the
// #490 shape: interface presence must NOT win over the report.
type reportingRegistrant struct {
	featureRichRegistrant // satisfies broker/mcp/spawner/interrupt + slash interfaces
	report                CapabilityReport
}

func (r *reportingRegistrant) AttachCapabilities() CapabilityReport { return r.report }

func TestBuildFeatures_ReporterOverridesInterfacePresence(t *testing.T) {
	t.Parallel()
	// The registrant's TYPE advertises everything; its report says
	// only interrupt is wired. The frame must follow the report —
	// pre-#490, every attachadapter session claimed mcp/perms_stream/
	// specialists and clients hit empty payloads and 501s.
	entry := &Entry{
		AppName:   "core-agent",
		SessionID: "s1",
		Agent: &reportingRegistrant{
			featureRichRegistrant: featureRichRegistrant{
				stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s1"},
			},
			report: CapabilityReport{Interrupt: true},
		},
	}
	got := buildFeatures(entry, map[string]bool{featureMultiSession: false})
	want := map[string]bool{
		featureMultiSession: false,
		featureInterrupt:    true,
		featureCostCeiling:  false,
		featureGuardrails:   false,
		featureObserverMode: false,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("buildFeatures with reporter:\n got  %#v\n want %#v (report must beat interface presence)", got, want)
	}
}

func TestBuildSlashCommands_ReporterOverridesInterfacePresence(t *testing.T) {
	t.Parallel()
	entry := &Entry{
		AppName:   "core-agent",
		SessionID: "s1",
		Agent: &reportingRegistrant{
			featureRichRegistrant: featureRichRegistrant{
				stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s1"},
			},
			report: CapabilityReport{SlashCommands: []string{"done", "btw"}},
		},
	}
	got := buildSlashCommands(entry)
	want := []string{"btw", "done"} // sorted, and ONLY what the report claims
	if !reflect.DeepEqual(got, want) {
		t.Errorf("buildSlashCommands with reporter = %v, want %v", got, want)
	}
}

func TestBuildFeatures_NonReporterKeepsInterfaceProbing(t *testing.T) {
	t.Parallel()
	// Registrants that only satisfy what they support (the legacy
	// contract) keep working without implementing the reporter.
	entry := &Entry{
		AppName:   "core-agent",
		SessionID: "s1",
		Agent: &featureRichRegistrant{
			stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s1"},
		},
	}
	got := buildFeatures(entry, nil)
	for _, key := range []string{featurePermsStream, featureMCP, featureSpecialists, featureInterrupt} {
		if !got[key] {
			t.Errorf("feature %q = false for a non-reporter registrant that implements the interface", key)
		}
	}
}
