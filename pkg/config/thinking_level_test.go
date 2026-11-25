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
	"strings"
	"testing"
)

func TestIsThinkingLevel(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"high", true},
		{" MAX ", true}, // surrounding space and case are ignored
		{"minimal", true},
		{"", false},
		{"extreme", false},
	} {
		if got := IsThinkingLevel(tc.in); got != tc.want {
			t.Errorf("IsThinkingLevel(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestValidateThinkingLevel(t *testing.T) {
	t.Parallel()
	for _, ok := range []string{"", "minimal", "low", "medium", "high", "xhigh", "max", "HIGH", " Max "} {
		if err := validateThinkingLevel(ok, "model"); err != nil {
			t.Errorf("%q: unexpected error %v", ok, err)
		}
	}
	for _, bad := range []string{"extreme", "hi", "budget:2048", "4096"} {
		err := validateThinkingLevel(bad, "subagents[2].model")
		if err == nil || !strings.Contains(err.Error(), "subagents[2].model.thinking_level") {
			t.Errorf("%q: err = %v, want one naming subagents[2].model.thinking_level", bad, err)
		}
	}
}

// A subagent keeps the parent's level unless its own model block sets one.
func TestSubagentThinkingLevel(t *testing.T) {
	t.Parallel()
	c := &Config{Model: ModelConfig{Name: "parent", ThinkingLevel: "high"}}
	for _, tc := range []struct {
		name string
		spec SubagentSpec
		want string
	}{
		{"no model block", SubagentSpec{Name: "a"}, "high"},
		{"model block without a level", SubagentSpec{Name: "b", Model: &ModelConfig{Name: "other"}}, "high"},
		{"its own level", SubagentSpec{Name: "c", Model: &ModelConfig{Name: "other", ThinkingLevel: "low"}}, "low"},
		{"blank level", SubagentSpec{Name: "d", Model: &ModelConfig{Name: "other", ThinkingLevel: "  "}}, "high"},
	} {
		if got := c.SubagentThinkingLevel(tc.spec); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}
