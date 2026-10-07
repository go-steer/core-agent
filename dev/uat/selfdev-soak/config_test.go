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
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
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

// conversationPlaceholder is what config.soak.json ships in the
// switchboard target's conversation. It contains spaces on purpose:
// Validate rejects whitespace in a conversation key, so a deployment that
// forgets to substitute the soak channel's ID fails at startup instead of
// booting and notifying a conversation that does not exist.
const conversationPlaceholder = "REPLACE WITH THE SOAK SLACK CHANNEL ID"

// readSoakConfig returns the shipped overlay's bytes.
func readSoakConfig(t *testing.T) []byte {
	t.Helper()
	body, err := os.ReadFile(soakConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// loadLikeDashC mirrors cmd/core-agent's loadConfig for -c: defaults,
// then the file, then Validate.
func loadLikeDashC(body []byte) (*config.Config, error) {
	cfg := config.DefaultConfig()
	if err := json.Unmarshal(body, cfg); err != nil {
		return nil, err
	}
	return cfg, cfg.Validate()
}

// deployed is the overlay as a deployment would mount it: the
// placeholder substituted with a real-looking channel ID.
func deployed(t *testing.T) *config.Config {
	t.Helper()
	body := bytes.ReplaceAll(readSoakConfig(t), []byte(conversationPlaceholder), []byte("C0123"))
	cfg, err := loadLikeDashC(body)
	if err != nil {
		t.Fatalf("%s, with the placeholder substituted, does not validate: %v", soakConfigPath, err)
	}
	return cfg
}

// TestSoakConfigPlaceholderFailsClosed: the shipped file carries the
// placeholder exactly once, in the switchboard target, and refuses to
// load until it is replaced.
func TestSoakConfigPlaceholderFailsClosed(t *testing.T) {
	body := readSoakConfig(t)
	if n := bytes.Count(body, []byte(conversationPlaceholder)); n != 1 {
		t.Fatalf("%s carries the conversation placeholder %d times, want exactly once", soakConfigPath, n)
	}
	var raw struct {
		Alerts config.AlertsConfig `json:"alerts"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	if len(raw.Alerts.Targets) != 1 || raw.Alerts.Targets[0].Conversation != conversationPlaceholder {
		t.Errorf("alerts.targets = %+v, want one target whose conversation is exactly %q", raw.Alerts.Targets, conversationPlaceholder)
	}
	if _, err := loadLikeDashC(body); err == nil || !strings.Contains(err.Error(), "conversation") {
		t.Errorf("the shipped overlay loaded with the placeholder in place (err=%v); an un-substituted deployment must fail at startup", err)
	}
}

func TestSoakConfigValidates(t *testing.T) {
	cfg := deployed(t)
	checkWorker(t, cfg)
	checkApprovalChannel(t, cfg)
	checkApprover(t, cfg)
	checkCaps(t, cfg)
}

// checkWorker pins decision 14's worker, reached through Vertex.
func checkWorker(t *testing.T, cfg *config.Config) {
	t.Helper()
	if cfg.Model.Provider != "anthropic-vertex" || cfg.Model.Name != "claude-sonnet-5" {
		t.Errorf("model = %s/%s, want anthropic-vertex/claude-sonnet-5 (decision 14, over Workload Identity)", cfg.Model.Provider, cfg.Model.Name)
	}
	if cfg.Safety.Watchdog != "enforce" {
		t.Errorf("safety.watchdog = %q, want enforce", cfg.Safety.Watchdog)
	}
}

// checkApprovalChannel pins the escalation path from The auto recipe.
func checkApprovalChannel(t *testing.T, cfg *config.Config) {
	t.Helper()
	p := cfg.Permissions
	got := [3]string{p.ApprovalTimeout, p.ApprovalNotify, p.ApprovalNotifyAfter}
	if want := [3]string{"30m", "maintainer-chat", "5m"}; got != want {
		t.Errorf("approval_timeout/approval_notify/approval_notify_after = %v, want %v", got, want)
	}
	if !slices.Contains(cfg.Tools.Disable, "alert") {
		t.Error("tools.disable lacks alert: the approval_notify target would also arm a model-facing alert tool")
	}
}

// checkApprover pins the approver and exactly what it may decide.
func checkApprover(t *testing.T, cfg *config.Config) {
	t.Helper()
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
	// Exact, not a superset check: a widened pattern ("bash:*") hands the
	// approver calls the design never made eligible.
	if want := []string{"bash:dev/ci/presubmits/*", "bash:dev/tools/*"}; !reflect.DeepEqual(p.Auto.Eligible, want) {
		t.Errorf("eligible = %v, want exactly %v (The auto recipe)", p.Auto.Eligible, want)
	}
	// Validate does not resolve bundle names; startup does, through this.
	if _, err := permissions.ResolveAutoEligible(p.Auto.Eligible, p.Auto.EligibleBundles); err != nil {
		t.Errorf("auto eligibility does not resolve: %v", err)
	}
}

// checkCaps pins decision 14's per-turn and per-session ceilings.
func checkCaps(t *testing.T, cfg *config.Config) {
	t.Helper()
	for name, got := range map[string]*float64{
		"max_turn_cost_usd":    cfg.Agent.MaxTurnCostUSD,
		"max_session_cost_usd": cfg.Agent.MaxSessionCostUSD,
	} {
		want := map[string]float64{"max_turn_cost_usd": 10, "max_session_cost_usd": 25}[name]
		switch {
		case got == nil:
			t.Errorf("agent.%s is unset, want %v (decision 14)", name, want)
		case *got != want:
			t.Errorf("agent.%s = %v, want %v (decision 14)", name, *got, want)
		}
	}
}

// TestSoakConfigMultiSession: the attach listener authenticates every
// caller against the hashed users.json table that deploy/ mounts
// read-only, with no anonymous fallback. The dispatcher creates sessions
// as sa:selfdev-dispatcher; the maintainer is the one admin, so they can
// attach to a dispatcher-owned session to answer an escalation.
func TestSoakConfigMultiSession(t *testing.T) {
	ms := deployed(t).Attach.MultiSession
	if !ms.Enabled || ms.AllowAnonymous {
		t.Fatalf("attach.multi_session enabled=%v allow_anonymous=%v, want an enforced table", ms.Enabled, ms.AllowAnonymous)
	}
	if ms.Auth.Kind != "bearer_table" || ms.Auth.TableFile != "/etc/core-agent-users/users.json" {
		t.Errorf("attach.multi_session.auth = %+v, want bearer_table at /etc/core-agent-users/users.json (deploy/base/50-deployment-daemon.yaml mounts it there)", ms.Auth)
	}
	if !slices.Equal(ms.AdminIdentities, []string{"mastersingh24"}) || len(ms.ProxyIdentities) != 0 {
		t.Errorf("admin=%v proxy=%v, want the maintainer as the only admin and no proxy identity", ms.AdminIdentities, ms.ProxyIdentities)
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
