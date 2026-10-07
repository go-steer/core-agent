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

package selfdevsoak

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/config"
	"github.com/go-steer/core-agent/v2/pkg/permissions"
)

const soakConfigPath = "config.soak.json"

// soakOverrides are the committed recipe's leaves the soak overlay is
// allowed to change, each for a reason the design doc gives. Every
// other leaf of /.agents/config.json must appear in the overlay
// unchanged: core-agent loads exactly one config file, so the overlay
// is the committed recipe restated plus these deltas, and this list is
// what keeps the restatement from going stale.
var soakOverrides = map[string]string{
	"model.provider":             "Vertex Claude through the pod's Workload Identity",
	"model.name":                 "decision 14: the worker is Claude Sonnet 5",
	"permissions.mode":           "the soak runs auto (The auto recipe)",
	"agent.max_session_cost_usd": "decision 14: the per-session cap is $25",
}

// loadLikeDashC mirrors cmd/core-agent's loadConfig for -c: defaults,
// then the file, then Validate.
func loadLikeDashC(t *testing.T, path string) *config.Config {
	t.Helper()
	body, err := os.ReadFile(path) //nolint:gosec // a fixed path in this tree
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultConfig()
	if err := json.Unmarshal(body, cfg); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("%s does not validate: %v", path, err)
	}
	return cfg
}

func TestSoakConfigValidates(t *testing.T) {
	cfg := loadLikeDashC(t, soakConfigPath)
	p := cfg.Permissions
	if p.Mode != "auto" {
		t.Errorf("permissions.mode = %q, want auto", p.Mode)
	}
	if p.Auto == nil {
		t.Fatal("permissions.auto is unset; mode auto has no approver")
	}
	if p.Auto.Model != "claude-haiku-4-5" {
		t.Errorf("approver model = %q, want claude-haiku-4-5 (The auto recipe)", p.Auto.Model)
	}
	if !reflect.DeepEqual(p.Auto.TaskFrom, []string{"sa:selfdev-dispatcher"}) {
		t.Errorf("task_from = %v, want exactly the dispatcher", p.Auto.TaskFrom)
	}
	if !reflect.DeepEqual(p.Auto.EligibleBundles, []string{"coding"}) {
		t.Errorf("eligible_bundles = %v, want [coding]; the agent never pushes (decision 7), so no github preset", p.Auto.EligibleBundles)
	}
	// Validate does not resolve bundle names; startup does, through this.
	if _, err := permissions.ResolveAutoEligible(p.Auto.Eligible, p.Auto.EligibleBundles); err != nil {
		t.Errorf("auto eligibility does not resolve: %v", err)
	}
	if cfg.Safety.Watchdog != "enforce" {
		t.Errorf("safety.watchdog = %q, want enforce", cfg.Safety.Watchdog)
	}
	if !slices.Contains(cfg.Tools.Disable, "alert") {
		t.Error("tools.disable lacks alert: the approval_notify target would also arm a model-facing alert tool")
	}
	for name, got := range map[string]*float64{
		"max_turn_cost_usd":    cfg.Agent.MaxTurnCostUSD,
		"max_session_cost_usd": cfg.Agent.MaxSessionCostUSD,
	} {
		want := map[string]float64{"max_turn_cost_usd": 10, "max_session_cost_usd": 25}[name]
		if got == nil || *got != want {
			t.Errorf("agent.%s = %v, want %v (decision 14)", name, got, want)
		}
	}
}

// TestSoakConfigRestatesCommittedRecipe fails when /.agents/config.json
// changes and the overlay was not brought along, or when the overlay
// changes a committed value nobody listed in soakOverrides.
func TestSoakConfigRestatesCommittedRecipe(t *testing.T) {
	committed := leaves(t, filepath.Join(repoRoot, ".agents", "config.json"))
	soak := leaves(t, soakConfigPath)
	keys := make([]string, 0, len(committed))
	for k := range committed {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		got, ok := soak[k]
		_, overridable := soakOverrides[k]
		switch {
		case !ok:
			t.Errorf("%s is in the committed recipe but missing from %s", k, soakConfigPath)
		case !overridable && !reflect.DeepEqual(got, committed[k]):
			t.Errorf("%s = %v in %s, committed recipe has %v; restate it, or list it in soakOverrides with the design's reason", k, got, soakConfigPath, committed[k])
		}
	}
	for k := range soakOverrides {
		if _, ok := committed[k]; !ok {
			t.Errorf("soakOverrides lists %s, which the committed recipe no longer has", k)
		}
	}
}

// leaves flattens a JSON object to dotted paths. Arrays are leaves:
// the subagents block is compared whole.
func leaves(t *testing.T, path string) map[string]any {
	t.Helper()
	body, err := os.ReadFile(path) //nolint:gosec // a fixed path in this tree
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	out := map[string]any{}
	var walk func(prefix string, v any)
	walk = func(prefix string, v any) {
		obj, ok := v.(map[string]any)
		if !ok {
			out[prefix] = v
			return
		}
		for k, child := range obj {
			p := k
			if prefix != "" {
				p = prefix + "." + k
			}
			walk(p, child)
		}
	}
	walk("", root)
	return out
}
