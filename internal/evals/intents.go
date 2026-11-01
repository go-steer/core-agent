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

package evals

import (
	"fmt"
	"os"
	"sort"

	"gopkg.in/yaml.v3"
)

// Intent is one diagnostic question a scenario expects the agent to
// answer, independent of which tool answers it.
type Intent struct {
	ID          string `yaml:"id"`
	Description string `yaml:"description"`
	Write       bool   `yaml:"write"`
}

// UpstreamTool maps one of the upstream agent's tool names onto an
// intent, carrying the annotations that keep upstream's own ceiling
// visible instead of folded into a score.
type UpstreamTool struct {
	Intent string `yaml:"intent"`
	Write  bool   `yaml:"write"`

	// LookoutExcluded marks a deliberate non-mapping — the write tool
	// that lookout's read-only surface does not and should not serve.
	LookoutExcluded bool   `yaml:"lookout_excluded"`
	ExclusionReason string `yaml:"exclusion_reason"`

	// UnreachableUpstream marks a name absent from upstream's own tool
	// registry. No upstream run can satisfy it, so any coverage number
	// compared against upstream must normalize for it.
	UnreachableUpstream bool `yaml:"unreachable_upstream"`
	// RegistryNearMiss is the registry name it was probably meant to be
	// (singular/plural slip); empty when there is no near miss.
	RegistryNearMiss string `yaml:"registry_near_miss"`
	// SidecarRepair is the real registry name the unwired .json sidecar
	// substitutes for this one; empty when the sidecar drops it.
	SidecarRepair string `yaml:"sidecar_repair"`
}

// LookoutTool is one lookout MCP tool and the intents a single call to
// it satisfies. The sets overlap heavily and that is the point: one
// k8s_cluster_health call answers what upstream spends four calls on.
type LookoutTool struct {
	Satisfies []string `yaml:"satisfies"`
	Note      string   `yaml:"note"`
}

// IntentTable is testdata/evals/intents.yaml.
type IntentTable struct {
	Version       int                     `yaml:"version"`
	Intents       []Intent                `yaml:"intents"`
	UpstreamTools map[string]UpstreamTool `yaml:"upstream_tools"`
	LookoutTools  map[string]LookoutTool  `yaml:"lookout_tools"`
}

// LoadIntentTable reads and validates the intent table. Validation is
// strict — an intent id referenced but never defined is a silent hole
// in the primary metric, not a warning.
func LoadIntentTable(path string) (IntentTable, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return IntentTable{}, fmt.Errorf("evals: read %q: %w", path, err)
	}
	var t IntentTable
	if err := yaml.Unmarshal(data, &t); err != nil {
		return IntentTable{}, fmt.Errorf("evals: parse %q: %w", path, err)
	}
	if err := t.validate(); err != nil {
		return IntentTable{}, fmt.Errorf("evals: validate %q: %w", path, err)
	}
	return t, nil
}

func (t IntentTable) validate() error {
	if len(t.Intents) == 0 {
		return fmt.Errorf("no intents defined")
	}
	defined := make(map[string]bool, len(t.Intents))
	for _, in := range t.Intents {
		if in.ID == "" {
			return fmt.Errorf("intent with an empty id")
		}
		if defined[in.ID] {
			return fmt.Errorf("duplicate intent id %q", in.ID)
		}
		defined[in.ID] = true
	}
	for name, ut := range t.UpstreamTools {
		if ut.Intent == "" {
			return fmt.Errorf("upstream tool %q maps to no intent", name)
		}
		if !defined[ut.Intent] {
			return fmt.Errorf("upstream tool %q maps to undefined intent %q", name, ut.Intent)
		}
	}
	for name, lt := range t.LookoutTools {
		if len(lt.Satisfies) == 0 {
			return fmt.Errorf("lookout tool %q satisfies no intents", name)
		}
		for _, id := range lt.Satisfies {
			if !defined[id] {
				return fmt.Errorf("lookout tool %q satisfies undefined intent %q", name, id)
			}
		}
	}
	return nil
}

// IntentFor resolves an upstream tool name to its intent id.
func (t IntentTable) IntentFor(upstreamTool string) (string, bool) {
	ut, ok := t.UpstreamTools[upstreamTool]
	if !ok {
		return "", false
	}
	return ut.Intent, true
}

// IntentsFor resolves a scenario's expected_tools to the deduplicated,
// sorted set of intents it expects. Unknown names are returned
// separately rather than dropped — a name the table has never seen is a
// gap in the table, and swallowing it would quietly inflate coverage.
func (t IntentTable) IntentsFor(upstreamTools []string) (intents []string, unknown []string) {
	seen := make(map[string]bool)
	seenUnknown := make(map[string]bool)
	for _, name := range upstreamTools {
		id, ok := t.IntentFor(name)
		if !ok {
			// Deduplicated like the intents are: a name repeated in one
			// scenario's expected_tools is one gap, not two.
			if !seenUnknown[name] {
				seenUnknown[name] = true
				unknown = append(unknown, name)
			}
			continue
		}
		if !seen[id] {
			seen[id] = true
			intents = append(intents, id)
		}
	}
	sort.Strings(intents)
	sort.Strings(unknown)
	return intents, unknown
}

// SatisfiedBy returns the set of intents a recorded trace satisfies,
// given the lookout tool names it actually called.
func (t IntentTable) SatisfiedBy(calledTools []string) []string {
	seen := make(map[string]bool)
	var out []string
	for _, name := range calledTools {
		lt, ok := t.LookoutTools[name]
		if !ok {
			// A call to a tool outside the table satisfies nothing here.
			// It is not an error: rosters carry tools this dataset does
			// not exercise.
			continue
		}
		for _, id := range lt.Satisfies {
			if !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	sort.Strings(out)
	return out
}

// Unreachable reports whether an upstream tool name is one of the
// phantoms — present in the dataset, absent from upstream's registry.
func (t IntentTable) Unreachable(upstreamTool string) bool {
	return t.UpstreamTools[upstreamTool].UnreachableUpstream
}

// Emittable reports whether a tool name is one a run under this table
// can actually call.
//
// lookout_tools is that catalog by construction rather than by
// convention: judge.Rig builds its fixture tools from these keys, and
// judge.NewCluster refuses to start when the table names one it has no
// shape for. So a name absent from lookout_tools cannot appear in any
// trace, and an expectation written in terms of it can never be met —
// which is the difference between a metric that scored badly and one
// that never ran (#174).
func (t IntentTable) Emittable(name string) bool {
	_, ok := t.LookoutTools[name]
	return ok
}
