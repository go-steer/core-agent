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

package memory

import (
	"fmt"
	"maps"
	"strings"
)

// SubagentScopeMode controls a subagent's private or inherited shared scope
// posture ("rw", "ro", or "off").
const (
	SubagentScopeRW  = "rw"
	SubagentScopeRO  = "ro"
	SubagentScopeOff = "off"
)

// SubagentDeriveConfig describes how a child subagent's memory Router should
// be derived from a parent Router across all six core-agent subagent execution
// paths.
type SubagentDeriveConfig struct {
	// SubagentName is the operator-declared subagent name (used as default
	// agent_id for declared subagents).
	SubagentName string

	// Mode is the subagent execution mode ("chat", "task", "single_turn").
	Mode string

	// Capability is the subagent capability ("", "read_only", "change_executor").
	Capability string

	// AdHoc marks an inline ad-hoc spawn_agent call with no predefined
	// SubagentSpec. Ad-hoc subagents ALWAYS force AgentMode="off" and clamp
	// all shared mounts to "ro".
	AdHoc bool

	// IsSubtask marks an internal mechanical RunSubtask helper (such as
	// agentic_* output digesters). Subtasks always receive a nil Router.
	IsSubtask bool

	// Namespace overrides the parent's namespace when constructing the
	// subagent's private mount or ExtraShared mounts. Empty inherits from the
	// parent's mounts (falling back to "default").
	Namespace string

	// AgentMode is the operator-declared private scope mode ("", "rw", "ro", "off").
	AgentMode string

	// AgentIDOverride overrides SubagentName as the private scope's agent_id
	// on operator-declared subagents. Ignored when AdHoc is true.
	AgentIDOverride string

	// AgentPartitionByUser partitions the subagent's private scope by user_id.
	AgentPartitionByUser bool

	// AgentWriteMode selects WriteConsolidate or WriteVerbatim on the private
	// scope (defaults to WriteConsolidate).
	AgentWriteMode WriteMode

	// InheritShared controls how the parent's shared:* mounts are inherited
	// ("", "ro", "rw", "off"). Empty defaults to "ro". Inherited mounts never
	// exceed the parent mount's own AccessMode ("ro" on parent stays "ro").
	InheritShared string

	// ExtraShared adds or overrides shared:* mounts declared on the
	// SubagentSpec. Ignored when AdHoc is true.
	ExtraShared []MountTemplate

	// NarrowAgentMode applies an optional per-spawn runtime narrowing
	// ("rw" -> "ro" -> "off"). It can never widen the derived mode.
	NarrowAgentMode string

	// NarrowSharedMode applies an optional per-spawn runtime narrowing across
	// shared mounts ("rw" -> "ro" -> "off"). It can never widen a mount.
	NarrowSharedMode string
}

// DeriveForSubagent derives a child Router sharing r's Backend, audit recorder,
// user resolver, strict-recall posture, and default TopK while applying
// subagent isolation, narrowing-by-default, and anti-spoofing invariants.
//
// Returns (nil, nil) when r is nil, when cfg.IsSubtask is true, or when the
// derived subagent has zero active mounts.
func (r *Router) DeriveForSubagent(cfg SubagentDeriveConfig) (*Router, error) {
	if r == nil || cfg.IsSubtask {
		return nil, nil
	}
	ns := strings.TrimSpace(cfg.Namespace)
	if ns == "" {
		ns = r.inferredNamespace()
	}

	var mounts []MountTemplate

	agentMode, err := resolveSubagentPrivateMode(cfg)
	if err != nil {
		return nil, err
	}
	if cfg.NarrowAgentMode != "" {
		narrowed, nErr := narrowScopeMode(agentMode, cfg.NarrowAgentMode)
		if nErr != nil {
			return nil, nErr
		}
		agentMode = narrowed
	}

	if agentMode != SubagentScopeOff {
		agentID := strings.TrimSpace(cfg.AgentIDOverride)
		if agentID == "" {
			agentID = strings.TrimSpace(cfg.SubagentName)
		}
		if agentID == "" {
			return nil, fmt.Errorf("%w: subagent name is required when private agent memory is enabled", ErrInvalidMount)
		}
		priv := AgentMount(ns, agentID, AccessMode(agentMode))
		priv.PartitionByUser = cfg.AgentPartitionByUser
		if cfg.AgentWriteMode != "" {
			priv.WriteMode = cfg.AgentWriteMode
		}
		mounts = append(mounts, priv)
	}

	sharedMounts, nextCeilings, nextGlobalMax, err := r.deriveSharedMounts(cfg)
	if err != nil {
		return nil, err
	}
	mounts = append(mounts, sharedMounts...)

	if len(mounts) == 0 {
		return nil, nil
	}

	return NewRouter(
		r.backend,
		mounts,
		WithBackendName(r.backendName),
		WithNamespace(ns),
		withSharedCeilings(nextCeilings, nextGlobalMax),
		WithDefaultTopK(r.defaultTopK),
		WithStrictRecall(r.strictRecall),
		WithAuditRecorder(r.baseAudit),
		WithUserResolver(r.userResolver),
	)
}

func resolveSubagentPrivateMode(cfg SubagentDeriveConfig) (string, error) {
	if cfg.AdHoc {
		// Mandatory anti-spoofing rule: ad-hoc inline spawns have no vetted
		// persistent identity and can never mount a private agent scope.
		return SubagentScopeOff, nil
	}
	explicit := strings.TrimSpace(cfg.AgentMode)
	if explicit != "" {
		if err := validateScopePosture("agent", explicit); err != nil {
			return "", err
		}
		if cfg.Mode == "single_turn" && explicit == SubagentScopeRW {
			return "", fmt.Errorf("%w: single_turn subagent %q is toolless and cannot mount private memory as %q", ErrInvalidMount, cfg.SubagentName, SubagentScopeRW)
		}
		return explicit, nil
	}
	switch {
	case cfg.Mode == "single_turn":
		return SubagentScopeOff, nil
	case cfg.Capability == "read_only":
		return SubagentScopeRO, nil
	default:
		return SubagentScopeRW, nil
	}
}

func (r *Router) deriveSharedMounts(cfg SubagentDeriveConfig) ([]MountTemplate, map[string]sharedCeiling, string, error) {
	inherit := strings.TrimSpace(cfg.InheritShared)
	if inherit == "" {
		inherit = SubagentScopeRO
	}
	if err := validateScopePosture("inherit_shared", inherit); err != nil {
		return nil, nil, "", err
	}

	globalMax := r.sharedGlobalMax
	if globalMax == "" {
		globalMax = SubagentScopeRW
	}
	if cfg.AdHoc || cfg.Mode == "single_turn" || cfg.Capability == "read_only" {
		if scopeRank(SubagentScopeRO) < scopeRank(globalMax) {
			globalMax = SubagentScopeRO
		}
	}
	if cfg.NarrowSharedMode != "" {
		narrowedGlobal, err := narrowScopeMode(globalMax, cfg.NarrowSharedMode)
		if err != nil {
			return nil, nil, "", err
		}
		globalMax = narrowedGlobal
	}
	if scopeRank(globalMax) < scopeRank(inherit) {
		inherit = globalMax
	}

	nextCeilings := maps.Clone(r.sharedCeilings)
	if nextCeilings == nil {
		nextCeilings = make(map[string]sharedCeiling)
	}

	var out []MountTemplate
	indexByName := make(map[string]int)

	if inherit != SubagentScopeOff && globalMax != SubagentScopeOff {
		for _, pm := range r.mounts {
			if pm.Name == MountAgent {
				continue
			}
			cm := pm.Clone()
			if inherit == SubagentScopeRO || pm.Mode == AccessReadOnly {
				cm.Mode = AccessReadOnly
			}
			indexByName[cm.Name] = len(out)
			out = append(out, cm)
		}
	}

	// Operator-declared ExtraShared mounts apply only to predefined subagents,
	// never to ad-hoc inline spawns, and must always be shared:* pools subject
	// to ancestor ceilings and PartitionByUser preservation.
	if !cfg.AdHoc {
		for _, extra := range cfg.ExtraShared {
			em := extra.Clone()
			if _, ok := ParseSharedMountName(em.Name); !ok {
				return nil, nil, "", fmt.Errorf("%w: extra_shared mount %q must be a shared:<pool> mount", ErrInvalidMount, em.Name)
			}
			if err := em.Validate(); err != nil {
				return nil, nil, "", fmt.Errorf("extra_shared mount %q: %w", em.Name, err)
			}
			if globalMax == SubagentScopeOff {
				continue
			}
			effMode, err := narrowScopeMode(string(em.Mode), globalMax)
			if err != nil {
				return nil, nil, "", err
			}
			if anc, hasAnc := nextCeilings[em.Name]; hasAnc {
				if anc.maxMode == SubagentScopeOff {
					continue
				}
				effMode, err = narrowScopeMode(effMode, anc.maxMode)
				if err != nil {
					return nil, nil, "", err
				}
				if anc.partitionByUser {
					em.PartitionByUser = true
				}
			}
			if effMode == SubagentScopeOff {
				continue
			}
			em.Mode = AccessMode(effMode)
			if idx, exists := indexByName[em.Name]; exists {
				out[idx] = em
			} else {
				indexByName[em.Name] = len(out)
				out = append(out, em)
			}
		}
	}

	if globalMax == SubagentScopeOff {
		for k, anc := range nextCeilings {
			anc.maxMode = SubagentScopeOff
			nextCeilings[k] = anc
		}
		return nil, nextCeilings, globalMax, nil
	}

	for _, m := range out {
		cur, ok := nextCeilings[m.Name]
		if !ok {
			nextCeilings[m.Name] = sharedCeiling{
				maxMode:         string(m.Mode),
				partitionByUser: m.PartitionByUser,
			}
			continue
		}
		maxM := string(m.Mode)
		if scopeRank(cur.maxMode) < scopeRank(maxM) {
			maxM = cur.maxMode
		}
		nextCeilings[m.Name] = sharedCeiling{
			maxMode:         maxM,
			partitionByUser: cur.partitionByUser || m.PartitionByUser,
		}
	}

	return out, nextCeilings, globalMax, nil
}

func (r *Router) inferredNamespace() string {
	if ns := strings.TrimSpace(r.namespace); ns != "" {
		return ns
	}
	for _, m := range r.mounts {
		if ns := strings.TrimSpace(m.BaseScope[ScopeKeyNamespace]); ns != "" {
			return ns
		}
	}
	return "default"
}

func validateScopePosture(field, val string) error {
	switch val {
	case SubagentScopeRW, SubagentScopeRO, SubagentScopeOff:
		return nil
	default:
		return fmt.Errorf("%w: invalid %s %q (want %q, %q, or %q)", ErrInvalidMount, field, val, SubagentScopeRW, SubagentScopeRO, SubagentScopeOff)
	}
}

func scopeRank(mode string) int {
	switch mode {
	case SubagentScopeOff:
		return 0
	case SubagentScopeRO:
		return 1
	case SubagentScopeRW:
		return 2
	default:
		return -1
	}
}

func narrowScopeMode(current, requested string) (string, error) {
	if err := validateScopePosture("narrow", requested); err != nil {
		return "", err
	}
	if scopeRank(requested) < scopeRank(current) {
		return requested, nil
	}
	return current, nil
}
