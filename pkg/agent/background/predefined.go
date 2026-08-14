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

package background

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// This file implements async-by-reference spawning (#626): the parent
// model calls spawn_agent with an `agent: "<name>"` reference to an
// operator-curated predefined spec instead of authoring an ad-hoc
// persona inline. See docs/unified-subagent-invocation-design.md.
//
// The trust boundary is the whole point: a predefined spec is an
// operator allowlist entry with a pre-wired system prompt, tool grant,
// model, and budgets. A reference spawn may only NARROW it — replace
// the goal, drop tools, tighten budgets, downshift to the small tier —
// never widen it. Ad-hoc (inline-persona) spawns remain possible but
// are gated behind allow_adhoc, off by default for daemons (D4).

// ErrUnknownSubagent is returned when a spawn_agent reference names a
// predefined spec the manager doesn't have registered.
var ErrUnknownSubagent = errors.New("background: unknown predefined subagent")

// ErrModelNotOverridable is returned when a spawn requests a model
// override other than "small" (or inherit). Per D2, a specific model
// requires its own predefined spec — a reference may only downshift to
// the small tier or inherit the spec's configured model.
var ErrModelNotOverridable = errors.New("background: model override must be \"small\" or omitted")

// ErrNoSmallModel is returned when a spawn requests model "small" but
// the manager was constructed without a small-tier model id
// (WithSmallModelID).
var ErrNoSmallModel = errors.New("background: no small-tier model configured")

// ErrToolNotGranted is returned when a reference spawn's tools override
// lists a tool the predefined spec does not grant. Overrides may only
// narrow the spec's tool set, never widen it.
var ErrToolNotGranted = errors.New("background: tool not granted by predefined spec")

// ErrAdhocDisabled is returned when the model attempts an ad-hoc
// (inline-persona) spawn while allow_adhoc is off. The remedy is to
// reference a configured subagent by name.
var ErrAdhocDisabled = errors.New("background: ad-hoc subagents are disabled; reference a configured subagent by name")

// RefOverrides are the narrowing-only adjustments a caller may layer on
// top of a referenced predefined spec (#626). Every field is optional;
// the zero value means "take the spec's value unchanged".
//
// Narrowing semantics (D2/D5):
//   - Goal replaces the spec's goal outright (a template spec typically
//     carries no goal; the parent supplies the task per spawn).
//   - Model may only be "" / "inherit" (keep the spec's model) or
//     "small" (downshift to the manager's small tier). A specific model
//     is rejected.
//   - Tools, when non-empty, must be a SUBSET of the spec's granted
//     tools+extras — it can only drop, never add.
//   - Budgets tighten only: a smaller positive cap wins; a larger or
//     zero override is ignored.
type RefOverrides struct {
	Goal    string
	Model   string
	Tools   []string
	Budgets Budgets
}

// Predefined returns the registered spec for name (a copy) and whether
// it exists.
func (m *Manager) Predefined(name string) (Spec, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.predefined[name]
	return s, ok
}

// PredefinedNames returns the registered predefined-spec names, sorted.
// Backs the operator catalog surfaces (#627) and diagnostics.
func (m *Manager) PredefinedNames() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.predefined))
	for name := range m.predefined {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// smallModel returns the configured small-tier model id, or "" when
// none is set. Used by the ad-hoc spawn path's model resolution.
func (m *Manager) smallModel() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.smallModelID
}

// AllowAdhoc reports whether ad-hoc (inline-persona) spawns are
// permitted. False (the daemon default) means the model may only spawn
// by reference to a predefined spec.
func (m *Manager) AllowAdhoc() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.allowAdhoc
}

// resolvePredefinedSpec turns a reference (name + narrowing overrides)
// into a concrete Spec ready for Spawn. It copies the registered
// template, applies the overrides, and enforces the narrowing-only
// rules. The returned spec's Name is the template name; the caller
// assigns the per-instance name (see nextInstanceName).
func (m *Manager) resolvePredefinedSpec(name string, ov RefOverrides) (Spec, error) {
	m.mu.Lock()
	base, ok := m.predefined[name]
	smallModelID := m.smallModelID
	m.mu.Unlock()
	if !ok {
		return Spec{}, fmt.Errorf("%w: %q", ErrUnknownSubagent, name)
	}

	spec := base // struct copy; slices are shared but we reassign, never mutate in place
	// Remember which configured subagent this came from. The caller
	// overwrites Name with a per-instance name, and the self-spawn
	// guard needs the declaration (#732).
	spec.Ref = name

	// Goal: replace when supplied.
	if g := strings.TrimSpace(ov.Goal); g != "" {
		spec.Goal = ov.Goal
	}

	// Model: inherit the spec's configured model, or downshift to small.
	modelID, err := resolveModelChoice(ov.Model, base.ModelID, smallModelID)
	if err != nil {
		return Spec{}, err
	}
	spec.ModelID = modelID

	// Tools: subset-only narrowing. An empty/absent override keeps the
	// spec's full grant.
	if len(ov.Tools) > 0 {
		granted := make(map[string]struct{}, len(base.Tools)+len(base.Extras))
		for _, t := range base.Tools {
			granted[t] = struct{}{}
		}
		for _, t := range base.Extras {
			granted[t] = struct{}{}
		}
		for _, t := range ov.Tools {
			if _, autoWired := autoWiredSubagentTools[t]; autoWired {
				continue
			}
			if _, okTool := granted[t]; !okTool {
				return Spec{}, fmt.Errorf("%w: %q (spec %q grants %s)", ErrToolNotGranted, t, name, strings.Join(sortedKeys(granted), ", "))
			}
		}
		// The narrowed set becomes the whole grant; Extras folds in since
		// resolveTools looks both up in the same catalog.
		spec.Tools = append([]string(nil), ov.Tools...)
		spec.Extras = nil
	}

	// Budgets: tighten only.
	spec.Budgets = tightenBudgets(base.Budgets, ov.Budgets)

	return spec, nil
}

// resolveModelChoice maps a per-spawn model override onto a concrete
// model id. base is the fallback when the caller inherits (a predefined
// spec's configured model, or "" for ad-hoc = manager default). Only
// "small" and inherit/"" are permitted; anything else is rejected (D2).
func resolveModelChoice(choice, base, smallModelID string) (string, error) {
	switch strings.TrimSpace(choice) {
	case "", "inherit":
		return base, nil
	case "small":
		if smallModelID == "" {
			return "", ErrNoSmallModel
		}
		return smallModelID, nil
	default:
		return "", fmt.Errorf("%w: got %q (configure a dedicated subagent spec for a specific model)", ErrModelNotOverridable, choice)
	}
}

// resolveAdhocModel maps an ad-hoc spawn's model override to a model
// id. Unlike the referenced path (resolveModelChoice), an ad-hoc spawn
// may name a specific model outright — it's parent-authored already, so
// this is no additional escalation (D2/§1). "" / "inherit" → manager
// default; "small" → the small tier (ErrNoSmallModel if unconfigured);
// anything else is taken as a literal model id.
func (m *Manager) resolveAdhocModel(choice string) (string, error) {
	switch c := strings.TrimSpace(choice); c {
	case "", "inherit":
		return "", nil
	case "small":
		if sm := m.smallModel(); sm != "" {
			return sm, nil
		}
		return "", ErrNoSmallModel
	default:
		return c, nil
	}
}

// tightenBudgets returns base with each dimension replaced by ov's
// value only when ov's is a strictly tighter positive cap. A zero or
// looser override is ignored (treating zero as "no cap" = infinity).
func tightenBudgets(base, ov Budgets) Budgets {
	out := base
	if v := tightenInt(base.MaxTurns, ov.MaxTurns); v != base.MaxTurns {
		out.MaxTurns = v
	}
	if ov.MaxCost > 0 && (base.MaxCost <= 0 || ov.MaxCost < base.MaxCost) {
		out.MaxCost = ov.MaxCost
	}
	if ov.MaxWallclock > 0 && (base.MaxWallclock <= 0 || ov.MaxWallclock < base.MaxWallclock) {
		out.MaxWallclock = ov.MaxWallclock
	}
	if ov.PerTurnTimeout > 0 && (base.PerTurnTimeout <= 0 || ov.PerTurnTimeout < base.PerTurnTimeout) {
		out.PerTurnTimeout = ov.PerTurnTimeout
	}
	return out
}

// tightenInt returns the tighter of two int caps, treating <=0 as "no
// cap". base is returned when ov is absent (<=0) or looser.
func tightenInt(base, ov int) int {
	if ov <= 0 {
		return base
	}
	if base <= 0 || ov < base {
		return ov
	}
	return base
}

// nextInstanceName derives the per-instance name for a reference spawn.
// When the caller supplied an explicit name, it's used verbatim;
// otherwise the runtime auto-derives "<spec>-<n>" from a per-spec
// monotonic counter so repeated spawns of the same template don't
// collide on Spawn's unique-running-name check.
func (m *Manager) nextInstanceName(specName, explicit string) string {
	if e := strings.TrimSpace(explicit); e != "" {
		return e
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.instanceSeq[specName]++
	return fmt.Sprintf("%s-%d", specName, m.instanceSeq[specName])
}

// sortedKeys returns a set's keys sorted, for stable error messages.
func sortedKeys(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
