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

package config

import (
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestEnvRefsNil(t *testing.T) {
	t.Parallel()
	var c *Config
	if got := c.EnvRefs(); got != nil {
		t.Errorf("nil Config should yield nil refs, got %v", got)
	}
	if got := (&Config{}).EnvRefs(); got != nil {
		t.Errorf("empty Config should yield nil refs, got %v", got)
	}
}

// TestEnvRefsAcrossEveryFieldShape exercises the walk against the real
// Config: a struct field (attach.token_env), a slice element
// (alerts.targets[]), and a pointer to a nested struct
// (alerts.targets[].auth). Those are the three shapes the reflective
// walk has to handle; a hand-written field list would have covered only
// whichever the author remembered.
func TestEnvRefsAcrossEveryFieldShape(t *testing.T) {
	t.Parallel()
	cfg := &Config{
		Alerts: AlertsConfig{Targets: []AlertTarget{
			{Name: "oncall", URLEnv: "ONCALL_WEBHOOK", Template: AlertTemplateGeneric},
			{
				Name:     "secops",
				URLEnv:   "SECOPS_WEBHOOK",
				Template: AlertTemplateGeneric,
				Auth:     &AlertAuth{BearerEnv: "SECOPS_TOKEN"},
			},
			{
				Name:     "legacy",
				URLEnv:   "LEGACY_WEBHOOK",
				Template: AlertTemplateGeneric,
				Auth:     &AlertAuth{BasicEnvUser: "LEGACY_USER", BasicEnvPass: "LEGACY_PASS"},
			},
		}},
		Attach: AttachConfig{TokenEnv: "ATTACH_TOKEN"},
	}

	want := []string{
		"ATTACH_TOKEN",
		"LEGACY_PASS",
		"LEGACY_USER",
		"LEGACY_WEBHOOK",
		"ONCALL_WEBHOOK",
		"SECOPS_TOKEN",
		"SECOPS_WEBHOOK",
	}
	got := cfg.EnvRefs()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("EnvRefs() =\n  %v\nwant sorted:\n  %v", got, want)
	}
}

// TestEnvRefsIgnoresValueFields is the property that keeps this from
// becoming a secret-leak surface: EnvRefs returns env var NAMES, and the
// only reason it is safe to log them in a drift warning is that no
// field carrying a VALUE is ever collected. alerts.targets[].url holds a
// literal webhook URL — which can embed a token — and must not appear.
func TestEnvRefsIgnoresValueFields(t *testing.T) {
	t.Parallel()
	cfg := &Config{Alerts: AlertsConfig{Targets: []AlertTarget{{
		Name:     "inline",
		URL:      "https://hooks.example.com/services/SECRET-PATH",
		Template: AlertTemplateGeneric,
	}}}}
	for _, ref := range cfg.EnvRefs() {
		if strings.Contains(ref, "SECRET-PATH") || strings.Contains(ref, "https://") {
			t.Errorf("EnvRefs leaked a value field: %q", ref)
		}
	}
	if got := cfg.EnvRefs(); got != nil {
		t.Errorf("target with url (not url_env) has no env refs, got %v", got)
	}
}

func TestEnvRefsDedupesAndSkipsBlanks(t *testing.T) {
	t.Parallel()
	cfg := &Config{
		Alerts: AlertsConfig{Targets: []AlertTarget{
			{Name: "a", URLEnv: "SHARED_WEBHOOK", Template: AlertTemplateGeneric},
			{Name: "b", URLEnv: "SHARED_WEBHOOK", Template: AlertTemplateGeneric},
			{Name: "c", URLEnv: "  ", Template: AlertTemplateGeneric},
			{Name: "d", URLEnv: "  PADDED  ", Template: AlertTemplateGeneric},
		}},
	}
	want := []string{"PADDED", "SHARED_WEBHOOK"}
	if got := cfg.EnvRefs(); !reflect.DeepEqual(got, want) {
		t.Errorf("EnvRefs() = %v, want %v", got, want)
	}
}

// TestEnvRefTagInventory pins the convention itself.
//
// EnvRefs discovers fields by JSON-tag shape rather than a maintained
// list, so nothing breaks when someone adds a `*_env` field — it is
// picked up for free. The risk runs the other way: a new env-NAME field
// tagged something else (`webhook_secret`, `token_var`) is invisible to
// the walk, and the only symptom is one spurious drift warning nobody
// traces back here.
//
// So: enumerate every env-ref-tagged field reachable from Config and
// compare against the known set. A new tagged field fails this test with
// a diff, which is the prompt to confirm the name follows the
// convention. Update the list when you add one deliberately.
func TestEnvRefTagInventory(t *testing.T) {
	t.Parallel()
	want := []string{
		"alerts.targets[].auth.basic_env_pass",
		"alerts.targets[].auth.basic_env_user",
		"alerts.targets[].auth.bearer_env",
		"alerts.targets[].url_env",
		"attach.token_env",
		"tools.call_peer.token_env",
	}
	got := envRefTagPaths(reflect.TypeOf(Config{}), "", make(map[reflect.Type]bool), 0)
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("env-ref-tagged fields in Config changed:\n got: %v\nwant: %v\n\n"+
			"If you added an env-var-NAME field, confirm its JSON tag ends in _env "+
			"(so EnvRefs finds it) and add it above. If you renamed one, update the list.",
			got, want)
	}
}

// envRefTagPaths walks a TYPE (not a value) and returns the dotted JSON
// path of every field EnvRefs would treat as an env-var name.
func envRefTagPaths(t reflect.Type, prefix string, seen map[reflect.Type]bool, depth int) []string {
	if t == nil || depth > maxEnvRefDepth {
		return nil
	}
	switch t.Kind() {
	case reflect.Pointer:
		return envRefTagPaths(t.Elem(), prefix, seen, depth+1)
	case reflect.Slice, reflect.Array:
		return envRefTagPaths(t.Elem(), prefix+"[]", seen, depth+1)
	case reflect.Map:
		return envRefTagPaths(t.Elem(), prefix+"[]", seen, depth+1)
	case reflect.Struct:
		// Recursive types (hooks.Config and friends) would loop forever;
		// one visit per type is enough to enumerate its tags.
		if seen[t] {
			return nil
		}
		seen[t] = true
		defer delete(seen, t)
		var out []string
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			name := jsonTagName(f)
			if name == "" {
				continue
			}
			path := name
			if prefix != "" {
				path = prefix + "." + name
			}
			if f.Type.Kind() == reflect.String {
				if isEnvRefTag(name) {
					out = append(out, path)
				}
				continue
			}
			out = append(out, envRefTagPaths(f.Type, path, seen, depth+1)...)
		}
		return out
	}
	return nil
}
