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

package approval

import (
	"maps"
	"slices"
	"strings"

	"github.com/go-steer/core-agent/v3/pkg/config"
	"github.com/go-steer/core-agent/v3/pkg/permissions"
)

// WithToolPolicies wires Mutating, ParkHintFields, Grants.Precondition, and
// Captures.For from a config.Tools.Policies slice (#1455).
func WithToolPolicies(policies []config.ToolPolicyConfig) Option {
	return func(cfg *Config) {
		if len(policies) == 0 {
			return
		}
		if cfg.Mutating == nil {
			cfg.Mutating = MutatingFromPolicies(policies, cfg.Gate)
		}
		if cfg.ParkHintFields == nil {
			cfg.ParkHintFields = ParkHintFieldsFromPolicies(policies)
		}
		hasPre := false
		hasCap := false
		for _, p := range policies {
			if p.Precondition != nil {
				hasPre = true
			}
			if p.Capture != nil {
				hasCap = true
			}
		}
		if hasPre {
			if cfg.Grants == nil {
				cfg.Grants = &Freshness{}
			}
			if cfg.Grants.Precondition == nil {
				cfg.Grants.Precondition = PreconditionFromPolicies(policies)
			}
		}
		if hasCap {
			if cfg.Captures == nil {
				cfg.Captures = &CaptureRules{}
			}
			if cfg.Captures.For == nil {
				cfg.Captures.For = CaptureFromPolicies(policies)
			}
		}
	}
}

// MutatingFromPolicies returns a tool mutation classifier backed by policies
// and falling back to gate.ToolClass (or permissions.DefaultToolClass when
// gate is nil).
func MutatingFromPolicies(policies []config.ToolPolicyConfig, gate *permissions.Gate) func(string) bool {
	byName := indexToolPolicies(policies)
	return func(toolName string) bool {
		if permissions.IsEngineControlCall(toolName) {
			return false
		}
		if p, _, ok := lookupToolPolicy(byName, toolName); ok && p.Mutating != nil {
			return *p.Mutating
		}
		if gate != nil {
			return gate.ToolClass(toolName) == permissions.ClassMutating
		}
		return permissions.DefaultToolClass(toolName) == permissions.ClassMutating
	}
}

// ParkHintFieldsFromPolicies returns a lookup function for per-tool
// park_hint_fields configured in policies.
func ParkHintFieldsFromPolicies(policies []config.ToolPolicyConfig) func(string) []string {
	byName := indexToolPolicies(policies)
	return func(toolName string) []string {
		p, _, ok := lookupToolPolicy(byName, toolName)
		if !ok || len(p.ParkHintFields) == 0 {
			return nil
		}
		return slices.Clone(p.ParkHintFields)
	}
}

// PreconditionFromPolicies builds a Freshness.Precondition lookup function from
// declarative config.ToolPolicyConfig entries (#1455).
func PreconditionFromPolicies(policies []config.ToolPolicyConfig) func(string) (*Precondition, error) {
	byName := indexToolPolicies(policies)
	return func(toolName string) (*Precondition, error) {
		p, prefix, ok := lookupToolPolicy(byName, toolName)
		if !ok || p.Precondition == nil {
			return nil, nil
		}
		pre := p.Precondition
		return &Precondition{
			Read:     qualifyWithPrefix(byName, prefix, pre.Read),
			Args:     maps.Clone(pre.Args),
			ArgsFrom: maps.Clone(pre.ArgsFrom),
			Fields:   slices.Clone(pre.Fields),
		}, nil
	}
}

// CaptureFromPolicies builds a CaptureRules.For lookup function from
// declarative config.ToolPolicyConfig entries (#1455).
func CaptureFromPolicies(policies []config.ToolPolicyConfig) func(string) (*Capture, error) {
	byName := indexToolPolicies(policies)
	return func(toolName string) (*Capture, error) {
		p, prefix, ok := lookupToolPolicy(byName, toolName)
		if !ok || p.Capture == nil {
			return nil, nil
		}
		c := p.Capture
		out := &Capture{
			Read:     qualifyWithPrefix(byName, prefix, c.Read),
			Args:     maps.Clone(c.Args),
			ArgsFrom: maps.Clone(c.ArgsFrom),
			Fields:   slices.Clone(c.Fields),
		}
		if c.Revert != nil {
			out.Revert = &Revert{
				Call:            qualifyWithPrefix(byName, prefix, c.Revert.Call),
				Args:            maps.Clone(c.Revert.Args),
				ArgsFromChange:  maps.Clone(c.Revert.ArgsFromChange),
				ArgsFromCapture: maps.Clone(c.Revert.ArgsFromCapture),
			}
		}
		return out, nil
	}
}

func indexToolPolicies(policies []config.ToolPolicyConfig) map[string]config.ToolPolicyConfig {
	byName := make(map[string]config.ToolPolicyConfig, len(policies))
	for _, p := range policies {
		name := strings.TrimSpace(p.Name)
		if name != "" {
			byName[name] = p
		}
	}
	return byName
}

func lookupToolPolicy(byName map[string]config.ToolPolicyConfig, toolName string) (config.ToolPolicyConfig, string, bool) {
	toolName = strings.TrimSpace(toolName)
	if p, ok := byName[toolName]; ok {
		return p, "", true
	}
	if !permissions.IsBuiltinTool(toolName) {
		for i := 0; i < len(toolName); i++ {
			if toolName[i] == '_' && i+1 < len(toolName) {
				if p, ok := byName[toolName[i+1:]]; ok {
					return p, toolName[:i+1], true
				}
			}
		}
	}
	return config.ToolPolicyConfig{}, "", false
}

func qualifyWithPrefix(byName map[string]config.ToolPolicyConfig, prefix, name string) string {
	name = strings.TrimSpace(name)
	if prefix == "" || name == "" {
		return name
	}
	if _, isPolicy := byName[name]; isPolicy {
		return prefix + name
	}
	if strings.HasPrefix(name, prefix) {
		return name
	}
	return prefix + name
}
