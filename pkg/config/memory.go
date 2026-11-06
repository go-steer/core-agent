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
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Memory backend, scope mode, write mode, preload, and consolidation constants.
const (
	MemoryBackendMemoryBank = "memorybank"
	MemoryBackendSQLite     = "sqlite"
	MemoryBackendInMemory   = "inmemory"
	MemoryBackendCustom     = "custom"

	MemoryModeRW  = "rw"
	MemoryModeRO  = "ro"
	MemoryModeOff = "off"

	MemoryWriteConsolidate = "consolidate"
	MemoryWriteVerbatim    = "verbatim"

	MemoryPreloadOff          = "off"
	MemoryPreloadSessionStart = "session_start"
	MemoryPreloadTurnStart    = "turn_start"

	MemoryConsolidateOff        = "off"
	MemoryConsolidateTurnEnd    = "turn_end"
	MemoryConsolidateSessionEnd = "session_end"

	maxMemoryScopeKeys = 5
	maxMemoryTopK      = 50
)

// AgentEngineConfig declares a Gemini Enterprise Agent Platform / Vertex AI
// Agent Engine (ReasoningEngine) resource shared across subsystems such as
// Session Store and Memory Bank.
//
// All env-name fields use the *_env suffix so Config.EnvRefs() discovers them
// automatically for .agents/env.yaml drift checks.
type AgentEngineConfig struct {
	// Resource is the full resource name
	// ("projects/{project}/locations/{location}/reasoningEngines/{id}") or
	// compact "{project}/{location}/{id}".
	Resource    string `json:"resource,omitempty" yaml:"resource,omitempty"`
	ResourceEnv string `json:"resource_env,omitempty" yaml:"resource_env,omitempty"`

	// Component form when Project/Location come from model.vertex.* or ADC env:
	Project     string `json:"project,omitempty" yaml:"project,omitempty"`
	ProjectEnv  string `json:"project_env,omitempty" yaml:"project_env,omitempty"`
	Location    string `json:"location,omitempty" yaml:"location,omitempty"`
	LocationEnv string `json:"location_env,omitempty" yaml:"location_env,omitempty"`
	ID          string `json:"id,omitempty" yaml:"id,omitempty"`
	IDEnv       string `json:"id_env,omitempty" yaml:"id_env,omitempty"`
}

// MemoryConfig configures long-term agent-specific and shared memory in
// .agents/config.json.
type MemoryConfig struct {
	// Backend selects "memorybank", "sqlite", "inmemory", or "custom" (optional
	// when Ref or RefEnv carries an explicit URI scheme).
	Backend string `json:"backend,omitempty" yaml:"backend,omitempty"`

	// Ref is a self-contained backend URI (for example,
	// "memorybank://projects/p/locations/us-central1/reasoningEngines/123",
	// "sqlite:///var/lib/core-agent/memory.db", or "inmemory://default").
	Ref    string `json:"ref,omitempty" yaml:"ref,omitempty"`
	RefEnv string `json:"ref_env,omitempty" yaml:"ref_env,omitempty"`

	// Namespace partitions scopes across environments or tenants (defaults to
	// "default" at runtime when unset).
	Namespace    string `json:"namespace,omitempty" yaml:"namespace,omitempty"`
	NamespaceEnv string `json:"namespace_env,omitempty" yaml:"namespace_env,omitempty"`

	// Agent configures the root agent's private ("agent") scope mount.
	Agent *AgentMemoryConfig `json:"agent,omitempty" yaml:"agent,omitempty"`

	// Shared configures zero or more named shared pool mounts ("shared:<pool>").
	Shared []SharedMemoryConfig `json:"shared,omitempty" yaml:"shared,omitempty"`

	// MemoryBank holds optional overrides when Memory Bank targets a different
	// ReasoningEngine than top-level agent_engine.
	MemoryBank *MemoryBankBackendConfig `json:"memorybank,omitempty" yaml:"memorybank,omitempty"`

	// SQLite configures the file path when backend is "sqlite" and Ref is unset.
	SQLite *SQLiteMemoryBackendConfig `json:"sqlite,omitempty" yaml:"sqlite,omitempty"`

	// Preload controls once-per-turn prompt-cache-safe recall injection into
	// the user message ("off" default, "session_start", or "turn_start").
	Preload string `json:"preload,omitempty" yaml:"preload,omitempty"`

	// Consolidate controls opt-in conversation extraction ("off" default,
	// "turn_end", or "session_end").
	Consolidate string `json:"consolidate,omitempty" yaml:"consolidate,omitempty"`

	// ConsolidateMount names the writable mount receiving conversation
	// extraction when Consolidate is enabled (defaults to "agent").
	ConsolidateMount string `json:"consolidate_mount,omitempty" yaml:"consolidate_mount,omitempty"`

	// TopK is the default maximum number of memories returned by recall (0 uses
	// the library default of 10; max 50).
	TopK int `json:"top_k,omitempty" yaml:"top_k,omitempty"`

	// StrictRecall fails multi-mount recall if any mounted scope errors, rather
	// than returning healthy mount items alongside warnings.
	StrictRecall bool `json:"strict_recall,omitempty" yaml:"strict_recall,omitempty"`
}

// AgentMemoryConfig configures an agent's private ("agent") memory mount.
type AgentMemoryConfig struct {
	// ID identifies the agent in ScopeMap["agent_id"] (defaults to the agent
	// name or "default" at runtime when empty).
	ID string `json:"id,omitempty" yaml:"id,omitempty"`

	// Mode is "rw" (default when Agent block is present), "ro", or "off".
	Mode string `json:"mode,omitempty" yaml:"mode,omitempty"`

	// WriteMode is "consolidate" (default) or "verbatim".
	WriteMode string `json:"write_mode,omitempty" yaml:"write_mode,omitempty"`

	// PartitionByUser adds "user_id" from the turn's resolved caller/session
	// identity to the scope dictionary.
	PartitionByUser bool `json:"partition_by_user,omitempty" yaml:"partition_by_user,omitempty"`

	// TTL sets an optional default expiration duration (time.ParseDuration
	// syntax, such as "720h") on facts written to this mount.
	TTL string `json:"ttl,omitempty" yaml:"ttl,omitempty"`
}

// ResolvedTTL parses a.TTL, returning 0 when empty.
func (a *AgentMemoryConfig) ResolvedTTL() (time.Duration, error) {
	if a == nil || strings.TrimSpace(a.TTL) == "" {
		return 0, nil
	}
	return parsePositiveDuration("memory.agent.ttl", a.TTL)
}

// SharedMemoryConfig configures a named shared pool ("shared:<pool>") mount.
type SharedMemoryConfig struct {
	// Pool is the logical shared pool name (mounted as "shared:<pool>").
	Pool string `json:"pool" yaml:"pool"`

	// Mode is "rw" or "ro" (defaults to "ro" when omitted so shared mounts
	// never become writable by accident).
	Mode string `json:"mode,omitempty" yaml:"mode,omitempty"`

	// WriteMode is "consolidate" (default) or "verbatim".
	WriteMode string `json:"write_mode,omitempty" yaml:"write_mode,omitempty"`

	// PartitionByUser adds "user_id" from the turn's resolved caller/session
	// identity to the scope dictionary.
	PartitionByUser bool `json:"partition_by_user,omitempty" yaml:"partition_by_user,omitempty"`

	// TTL sets an optional default expiration duration (time.ParseDuration
	// syntax) on facts written to this mount.
	TTL string `json:"ttl,omitempty" yaml:"ttl,omitempty"`

	// ScopeAttrs optionally overrides the static ScopeMap keys sent to the
	// backend (for example, {"app_name": "portal"} when interoperating with an
	// existing Python ADK Memory Bank).
	ScopeAttrs map[string]string `json:"scope_attrs,omitempty" yaml:"scope_attrs,omitempty"`
}

// EffectiveMode returns "rw" or "ro", defaulting to "ro" when Mode is unset.
func (s SharedMemoryConfig) EffectiveMode() string {
	if s.Mode == "" {
		return MemoryModeRO
	}
	return s.Mode
}

// ResolvedTTL parses s.TTL, returning 0 when empty.
func (s SharedMemoryConfig) ResolvedTTL() (time.Duration, error) {
	if strings.TrimSpace(s.TTL) == "" {
		return 0, nil
	}
	return parsePositiveDuration(fmt.Sprintf("memory.shared[%q].ttl", s.Pool), s.TTL)
}

// MemoryBankBackendConfig holds optional Memory Bank resource overrides when
// Memory Bank uses a different ReasoningEngine than top-level agent_engine.
type MemoryBankBackendConfig struct {
	Resource    string `json:"resource,omitempty" yaml:"resource,omitempty"`
	ResourceEnv string `json:"resource_env,omitempty" yaml:"resource_env,omitempty"`
	ID          string `json:"id,omitempty" yaml:"id,omitempty"`
	IDEnv       string `json:"id_env,omitempty" yaml:"id_env,omitempty"`
}

// SQLiteMemoryBackendConfig configures the local SQLite FTS5 memory store.
type SQLiteMemoryBackendConfig struct {
	Path    string `json:"path,omitempty" yaml:"path,omitempty"`
	PathEnv string `json:"path_env,omitempty" yaml:"path_env,omitempty"`
}

// SubagentMemoryConfig configures per-subagent memory scoping on SubagentSpec
// (both in .agents/config.json subagents[] and in .agents/subagents/*.md YAML
// frontmatter).
type SubagentMemoryConfig struct {
	// Agent sets the subagent's private scope mode ("rw", "ro", or "off").
	// Empty uses mode/capability-dependent defaults ("rw" for chat/task, "ro"
	// for capability=read_only, "off" for single_turn).
	Agent string `json:"agent,omitempty" yaml:"agent,omitempty"`

	// AgentID overrides SubagentSpec.Name as the private scope's agent_id.
	AgentID string `json:"agent_id,omitempty" yaml:"agent_id,omitempty"`

	// PartitionByUser partitions the subagent's private scope by user_id.
	PartitionByUser bool `json:"partition_by_user,omitempty" yaml:"partition_by_user,omitempty"`

	// WriteMode selects "consolidate" (default) or "verbatim" on the subagent's
	// private scope.
	WriteMode string `json:"write_mode,omitempty" yaml:"write_mode,omitempty"`

	// InheritShared controls how the parent's shared:* mounts are inherited
	// ("ro" default, "rw" clamped to parent mount's mode, or "off").
	InheritShared string `json:"inherit_shared,omitempty" yaml:"inherit_shared,omitempty"`

	// Shared declares additional shared pools or per-pool mode overrides for
	// this subagent.
	Shared []SharedMemoryConfig `json:"shared,omitempty" yaml:"shared,omitempty"`

	// Preload controls once-per-turn recall injection for this subagent ("off"
	// default, or "turn_start").
	Preload string `json:"preload,omitempty" yaml:"preload,omitempty"`
}

// ResolveAgentEngineResource resolves the shared top-level agent_engine block
// into a canonical "projects/{p}/locations/{l}/reasoningEngines/{id}" resource
// name, returning ("", nil) when agent_engine is nil or unset.
func (c *Config) ResolveAgentEngineResource(getenv func(string) string) (string, error) {
	if c == nil || c.AgentEngine == nil {
		return "", nil
	}
	if getenv == nil {
		getenv = os.Getenv
	}
	return c.resolveEngineCoords(
		c.AgentEngine.Resource,
		c.AgentEngine.ResourceEnv,
		c.AgentEngine.ID,
		c.AgentEngine.IDEnv,
		getenv,
	)
}

// ResolveMemoryRef resolves the effective memory reference URI from c.Memory
// and c.AgentEngine, returning ("", nil) when memory is unconfigured.
func (c *Config) ResolveMemoryRef(getenv func(string) string) (string, error) {
	if c == nil || c.Memory == nil {
		return "", nil
	}
	if getenv == nil {
		getenv = os.Getenv
	}
	m := c.Memory
	if rawRef := firstNonEmptyValue(m.Ref, m.RefEnv, getenv); rawRef != "" {
		if err := validateMemoryRefSyntax(rawRef); err != nil {
			return "", fmt.Errorf("config: memory.ref: %w", err)
		}
		return rawRef, nil
	}
	switch m.Backend {
	case "":
		return "", nil
	case MemoryBackendInMemory:
		return "inmemory://default", nil
	case MemoryBackendSQLite:
		var path string
		if m.SQLite != nil {
			path = firstNonEmptyValue(m.SQLite.Path, m.SQLite.PathEnv, getenv)
		}
		if path == "" {
			return "", errors.New("config: memory.backend=\"sqlite\" requires memory.ref or memory.sqlite.path (or path_env)")
		}
		return formatSQLiteMemoryRef(path)
	case MemoryBackendMemoryBank:
		var mbRes, mbResEnv, mbID, mbIDEnv string
		if m.MemoryBank != nil {
			mbRes = m.MemoryBank.Resource
			mbResEnv = m.MemoryBank.ResourceEnv
			mbID = m.MemoryBank.ID
			mbIDEnv = m.MemoryBank.IDEnv
		}
		res, err := c.resolveEngineCoords(mbRes, mbResEnv, mbID, mbIDEnv, getenv)
		if err != nil {
			return "", err
		}
		if res == "" {
			res, err = c.ResolveAgentEngineResource(getenv)
			if err != nil {
				return "", err
			}
		}
		if res == "" {
			return "", errors.New("config: memory.backend=\"memorybank\" requires memory.ref, memory.memorybank.resource, or top-level agent_engine")
		}
		return "memorybank://" + res, nil
	default:
		return "", fmt.Errorf("config: memory.backend=%q requires memory.ref (or memory.ref_env)", m.Backend)
	}
}

// ResolveNamespace returns the configured memory namespace (resolving
// NamespaceEnv when Namespace is empty), defaulting to "default".
func (m *MemoryConfig) ResolveNamespace(getenv func(string) string) string {
	if m == nil {
		return "default"
	}
	if getenv == nil {
		getenv = os.Getenv
	}
	if ns := firstNonEmptyValue(m.Namespace, m.NamespaceEnv, getenv); ns != "" {
		return ns
	}
	return "default"
}

func (c *Config) resolveEngineCoords(res, resEnv, id, idEnv string, getenv func(string) string) (string, error) {
	if rawRes := firstNonEmptyValue(res, resEnv, getenv); rawRes != "" {
		return normalizeReasoningEngineResource(rawRes)
	}
	engineID := firstNonEmptyValue(id, idEnv, getenv)
	if engineID == "" {
		return "", nil
	}
	proj, loc := c.fallbackGCPProjectAndLocation(getenv)
	if proj == "" || loc == "" {
		return "", errors.New("config: agent_engine id is set but GCP project or location could not be resolved (set agent_engine.project/location, model.vertex.project/location, or GOOGLE_CLOUD_PROJECT / GOOGLE_CLOUD_LOCATION)")
	}
	return normalizeReasoningEngineResource(fmt.Sprintf("projects/%s/locations/%s/reasoningEngines/%s", proj, loc, engineID))
}

func (c *Config) fallbackGCPProjectAndLocation(getenv func(string) string) (project, location string) {
	if c.AgentEngine != nil {
		project = firstNonEmptyValue(c.AgentEngine.Project, c.AgentEngine.ProjectEnv, getenv)
		location = firstNonEmptyValue(c.AgentEngine.Location, c.AgentEngine.LocationEnv, getenv)
	}
	if project == "" && c.Model.Vertex != nil {
		project = strings.TrimSpace(c.Model.Vertex.Project)
	}
	if location == "" && c.Model.Vertex != nil {
		location = strings.TrimSpace(c.Model.Vertex.Location)
	}
	if project == "" && c.Model.Anthropic != nil && c.Model.Anthropic.Vertex != nil {
		project = strings.TrimSpace(c.Model.Anthropic.Vertex.Project)
	}
	if location == "" && c.Model.Anthropic != nil && c.Model.Anthropic.Vertex != nil {
		location = strings.TrimSpace(c.Model.Anthropic.Vertex.Location)
	}
	if project == "" {
		project = strings.TrimSpace(getenv("GOOGLE_CLOUD_PROJECT"))
	}
	if location == "" {
		location = strings.TrimSpace(getenv("GOOGLE_CLOUD_LOCATION"))
		if location == "" {
			location = strings.TrimSpace(getenv("CLOUD_ML_REGION"))
		}
	}
	return project, location
}

func firstNonEmptyValue(literal, envName string, getenv func(string) string) string {
	if v := strings.TrimSpace(literal); v != "" {
		return v
	}
	if name := strings.TrimSpace(envName); name != "" && getenv != nil {
		return strings.TrimSpace(getenv(name))
	}
	return ""
}

func (c *Config) validateAgentEngine() error {
	ae := c.AgentEngine
	if ae == nil {
		return nil
	}
	if (ae.Resource != "" || ae.ResourceEnv != "") && (ae.ID != "" || ae.IDEnv != "") {
		return errors.New("config: agent_engine.resource (or resource_env) and agent_engine.id (or id_env) are mutually exclusive")
	}
	if ae.Resource != "" {
		if _, err := normalizeReasoningEngineResource(ae.Resource); err != nil {
			return fmt.Errorf("config: agent_engine.resource: %w", err)
		}
	}
	for _, seg := range []struct{ field, val string }{
		{"agent_engine.project", ae.Project},
		{"agent_engine.location", ae.Location},
		{"agent_engine.id", ae.ID},
	} {
		if seg.val != "" {
			if err := validateMemoryAtom(seg.field, seg.val); err != nil {
				return fmt.Errorf("config: %w", err)
			}
		}
	}
	return nil
}

func (c *Config) validateMemory() error {
	m := c.Memory
	if m == nil {
		return nil
	}
	switch m.Backend {
	case "", MemoryBackendMemoryBank, MemoryBackendSQLite, MemoryBackendInMemory, MemoryBackendCustom:
	default:
		return fmt.Errorf("config: unknown memory.backend %q (want %q, %q, %q, or %q)",
			m.Backend, MemoryBackendMemoryBank, MemoryBackendSQLite, MemoryBackendInMemory, MemoryBackendCustom)
	}
	if m.Ref != "" {
		if err := validateMemoryRefSyntax(m.Ref); err != nil {
			return fmt.Errorf("config: memory.ref: %w", err)
		}
	}
	if err := c.validateMemoryBackendCoords(); err != nil {
		return err
	}
	if m.Namespace != "" {
		if err := validateMemoryAtom("memory.namespace", m.Namespace); err != nil {
			return fmt.Errorf("config: %w", err)
		}
	}
	if m.MemoryBank != nil {
		if err := validateMemoryBankCoords(m.MemoryBank); err != nil {
			return err
		}
	}
	if m.SQLite != nil && m.SQLite.Path != "" {
		if _, err := formatSQLiteMemoryRef(strings.TrimSpace(m.SQLite.Path)); err != nil {
			return err
		}
	}
	if m.TopK < 0 || m.TopK > maxMemoryTopK {
		return fmt.Errorf("config: memory.top_k=%d must be between 0 and %d", m.TopK, maxMemoryTopK)
	}
	switch m.Preload {
	case "", MemoryPreloadOff, MemoryPreloadSessionStart, MemoryPreloadTurnStart:
	default:
		return fmt.Errorf("config: unknown memory.preload %q (want %q, %q, or %q)",
			m.Preload, MemoryPreloadOff, MemoryPreloadSessionStart, MemoryPreloadTurnStart)
	}
	switch m.Consolidate {
	case "", MemoryConsolidateOff, MemoryConsolidateTurnEnd, MemoryConsolidateSessionEnd:
	default:
		return fmt.Errorf("config: unknown memory.consolidate %q (want %q, %q, or %q)",
			m.Consolidate, MemoryConsolidateOff, MemoryConsolidateTurnEnd, MemoryConsolidateSessionEnd)
	}

	writableMounts := make(map[string]bool)
	if m.Agent != nil {
		if err := validateAgentMemoryBlock(m.Agent); err != nil {
			return err
		}
		mode := m.Agent.Mode
		if mode == "" {
			mode = MemoryModeRW
		}
		if mode == MemoryModeRW {
			writableMounts["agent"] = true
		}
	}
	if err := validateSharedMemorySlice("memory.shared", m.Shared, writableMounts); err != nil {
		return err
	}

	if m.Consolidate == MemoryConsolidateTurnEnd || m.Consolidate == MemoryConsolidateSessionEnd {
		target := strings.TrimSpace(m.ConsolidateMount)
		if target == "" {
			target = "agent"
		}
		if !writableMounts[target] {
			return fmt.Errorf("config: memory.consolidate=%q targets mount %q, which is not configured as writable (\"rw\")",
				m.Consolidate, target)
		}
	}
	return nil
}

func validateAgentMemoryBlock(a *AgentMemoryConfig) error {
	if a.ID != "" {
		if err := validateMemoryAtom("memory.agent.id", a.ID); err != nil {
			return fmt.Errorf("config: %w", err)
		}
	}
	switch a.Mode {
	case "", MemoryModeRW, MemoryModeRO, MemoryModeOff:
	default:
		return fmt.Errorf("config: unknown memory.agent.mode %q (want %q, %q, or %q)",
			a.Mode, MemoryModeRW, MemoryModeRO, MemoryModeOff)
	}
	switch a.WriteMode {
	case "", MemoryWriteConsolidate, MemoryWriteVerbatim:
	default:
		return fmt.Errorf("config: unknown memory.agent.write_mode %q (want %q or %q)",
			a.WriteMode, MemoryWriteConsolidate, MemoryWriteVerbatim)
	}
	if _, err := a.ResolvedTTL(); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	return nil
}

func validateSharedMemorySlice(prefix string, list []SharedMemoryConfig, writableOut map[string]bool) error {
	seen := make(map[string]bool, len(list))
	for i, s := range list {
		field := fmt.Sprintf("%s[%d]", prefix, i)
		if err := validateMemoryAtom(field+".pool", s.Pool); err != nil {
			return fmt.Errorf("config: %w", err)
		}
		if seen[s.Pool] {
			return fmt.Errorf("config: %s: duplicate pool %q", field, s.Pool)
		}
		seen[s.Pool] = true

		switch s.Mode {
		case "", MemoryModeRW, MemoryModeRO:
		default:
			return fmt.Errorf("config: unknown %s.mode %q (want %q or %q)", field, s.Mode, MemoryModeRW, MemoryModeRO)
		}
		switch s.WriteMode {
		case "", MemoryWriteConsolidate, MemoryWriteVerbatim:
		default:
			return fmt.Errorf("config: unknown %s.write_mode %q (want %q or %q)", field, s.WriteMode, MemoryWriteConsolidate, MemoryWriteVerbatim)
		}
		if s.TTL != "" {
			if _, err := parsePositiveDuration(field+".ttl", s.TTL); err != nil {
				return fmt.Errorf("config: %w", err)
			}
		}
		if len(s.ScopeAttrs) > 0 {
			limit := maxMemoryScopeKeys
			if s.PartitionByUser {
				limit = maxMemoryScopeKeys - 1
			}
			if len(s.ScopeAttrs) > limit {
				return fmt.Errorf("config: %s.scope_attrs has %d keys (max %d)", field, len(s.ScopeAttrs), limit)
			}
			if s.PartitionByUser {
				if _, conflict := s.ScopeAttrs["user_id"]; conflict {
					return fmt.Errorf("config: %s sets partition_by_user=true and also hardcodes \"user_id\" in scope_attrs", field)
				}
			}
			for k, v := range s.ScopeAttrs {
				if err := validateMemoryAtom(field+".scope_attrs key", k); err != nil {
					return fmt.Errorf("config: %w", err)
				}
				if err := validateMemoryAtom(fmt.Sprintf("%s.scope_attrs[%q]", field, k), v); err != nil {
					return fmt.Errorf("config: %w", err)
				}
			}
		}
		if writableOut != nil && s.EffectiveMode() == MemoryModeRW {
			writableOut["shared:"+s.Pool] = true
		}
	}
	return nil
}

func validateSubagentMemory(i int, sa SubagentSpec) error {
	sm := sa.Memory
	if sm == nil {
		return nil
	}
	prefix := fmt.Sprintf("subagents[%d].memory", i)
	switch sm.Agent {
	case "", MemoryModeRW, MemoryModeRO, MemoryModeOff:
	default:
		return fmt.Errorf("config: unknown %s.agent %q (want %q, %q, or %q)",
			prefix, sm.Agent, MemoryModeRW, MemoryModeRO, MemoryModeOff)
	}
	if sa.EffectiveMode() == SubagentModeSingleTurn && sm.Agent == MemoryModeRW {
		return fmt.Errorf("config: %s.agent=%q is invalid when mode is %q (single_turn subagents have no tools to write memory)",
			prefix, MemoryModeRW, SubagentModeSingleTurn)
	}
	if sm.AgentID != "" {
		if err := validateMemoryAtom(prefix+".agent_id", sm.AgentID); err != nil {
			return fmt.Errorf("config: %w", err)
		}
	}
	switch sm.WriteMode {
	case "", MemoryWriteConsolidate, MemoryWriteVerbatim:
	default:
		return fmt.Errorf("config: unknown %s.write_mode %q (want %q or %q)",
			prefix, sm.WriteMode, MemoryWriteConsolidate, MemoryWriteVerbatim)
	}
	switch sm.InheritShared {
	case "", MemoryModeRO, MemoryModeRW, MemoryModeOff:
	default:
		return fmt.Errorf("config: unknown %s.inherit_shared %q (want %q, %q, or %q)",
			prefix, sm.InheritShared, MemoryModeRO, MemoryModeRW, MemoryModeOff)
	}
	switch sm.Preload {
	case "", MemoryPreloadOff, MemoryPreloadTurnStart:
	default:
		return fmt.Errorf("config: unknown %s.preload %q (want %q or %q)",
			prefix, sm.Preload, MemoryPreloadOff, MemoryPreloadTurnStart)
	}
	return validateSharedMemorySlice(prefix+".shared", sm.Shared, nil)
}

func normalizeReasoningEngineResource(raw string) (string, error) {
	trimmed := strings.Trim(strings.TrimSpace(raw), "/")
	parts := strings.Split(trimmed, "/")
	var proj, loc, id string
	switch {
	case len(parts) == 6 && parts[0] == "projects" && parts[2] == "locations" && parts[4] == "reasoningEngines":
		proj, loc, id = parts[1], parts[3], parts[5]
	case len(parts) == 3 && parts[0] != "projects":
		proj, loc, id = parts[0], parts[1], parts[2]
	default:
		return "", fmt.Errorf("%q must match projects/{project}/locations/{location}/reasoningEngines/{id} or {project}/{location}/{id}", raw)
	}
	for _, seg := range []struct{ label, val string }{
		{"project", proj},
		{"location", loc},
		{"reasoningEngine id", id},
	} {
		if seg.val == "." || seg.val == ".." || strings.ContainsAny(seg.val, "/?#\\ ") {
			return "", fmt.Errorf("invalid %s %q", seg.label, seg.val)
		}
		if err := validateMemoryAtom(seg.label, seg.val); err != nil {
			return "", err
		}
	}
	return fmt.Sprintf("projects/%s/locations/%s/reasoningEngines/%s", proj, loc, id), nil
}

func (c *Config) validateMemoryBackendCoords() error {
	m := c.Memory
	if m.Ref != "" || strings.TrimSpace(m.RefEnv) != "" {
		return nil
	}
	switch m.Backend {
	case MemoryBackendCustom:
		return errors.New("config: memory.backend=\"custom\" requires memory.ref or memory.ref_env")
	case MemoryBackendSQLite:
		if m.SQLite == nil || (strings.TrimSpace(m.SQLite.Path) == "" && strings.TrimSpace(m.SQLite.PathEnv) == "") {
			return errors.New("config: memory.backend=\"sqlite\" requires memory.ref, memory.ref_env, memory.sqlite.path, or memory.sqlite.path_env")
		}
	case MemoryBackendMemoryBank:
		hasMB := m.MemoryBank != nil && (strings.TrimSpace(m.MemoryBank.Resource) != "" ||
			strings.TrimSpace(m.MemoryBank.ResourceEnv) != "" ||
			strings.TrimSpace(m.MemoryBank.ID) != "" ||
			strings.TrimSpace(m.MemoryBank.IDEnv) != "")
		hasAE := c.AgentEngine != nil && (strings.TrimSpace(c.AgentEngine.Resource) != "" ||
			strings.TrimSpace(c.AgentEngine.ResourceEnv) != "" ||
			strings.TrimSpace(c.AgentEngine.ID) != "" ||
			strings.TrimSpace(c.AgentEngine.IDEnv) != "")
		if !hasMB && !hasAE {
			return errors.New("config: memory.backend=\"memorybank\" requires memory.ref, memory.memorybank, or agent_engine coordinates")
		}
	}
	return nil
}

func validateMemoryBankCoords(mb *MemoryBankBackendConfig) error {
	if (mb.Resource != "" || mb.ResourceEnv != "") && (mb.ID != "" || mb.IDEnv != "") {
		return errors.New("config: memory.memorybank.resource (or resource_env) and memory.memorybank.id (or id_env) are mutually exclusive")
	}
	if mb.Resource != "" {
		if _, err := normalizeReasoningEngineResource(mb.Resource); err != nil {
			return fmt.Errorf("config: memory.memorybank.resource: %w", err)
		}
	}
	if mb.ID != "" {
		if err := validateMemoryAtom("memory.memorybank.id", mb.ID); err != nil {
			return fmt.Errorf("config: %w", err)
		}
	}
	return nil
}

func formatSQLiteMemoryRef(path string) (string, error) {
	if path == "" || path == "/" || path == "." {
		return "", fmt.Errorf("config: invalid sqlite memory path %q", path)
	}
	if strings.HasPrefix(path, "/") {
		return "sqlite://" + (&url.URL{Path: path}).EscapedPath(), nil
	}
	rel := strings.TrimPrefix(path, "./")
	if rel == "" || rel == "." {
		return "", fmt.Errorf("config: invalid sqlite memory path %q", path)
	}
	escaped := strings.TrimPrefix((&url.URL{Path: "/" + rel}).EscapedPath(), "/")
	return "sqlite://./" + escaped, nil
}

func validateMemoryRefSyntax(raw string) error {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return errors.New("empty URI")
	}
	if trimmed != raw {
		return fmt.Errorf("%q has surrounding whitespace", raw)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.Scheme == "" {
		return fmt.Errorf("missing scheme in %q", raw)
	}
	if u.Fragment != "" {
		return fmt.Errorf("unexpected fragment in %q", raw)
	}
	switch strings.ToLower(u.Scheme) {
	case MemoryBackendMemoryBank:
		if u.User != nil {
			return errors.New("memorybank URI must not contain userinfo")
		}
		if strings.Trim(u.Host+u.Path, "/") == "" {
			return errors.New("memorybank URI requires ReasoningEngine resource path")
		}
		if _, err := normalizeReasoningEngineResource(u.Host + u.Path); err != nil {
			return err
		}
	case MemoryBackendSQLite:
		if u.User != nil {
			return errors.New("sqlite URI must not contain userinfo")
		}
		var path string
		switch u.Host {
		case "", "localhost":
			path = u.Path
		case ".":
			path = strings.TrimPrefix(u.Path, "/")
		default:
			path = u.Host + u.Path
		}
		if strings.TrimSpace(path) == "" || path == "/" {
			return fmt.Errorf("sqlite URI %q requires a file path", raw)
		}
	case MemoryBackendInMemory:
		name := strings.TrimPrefix(u.Host+u.Path, "/")
		if strings.Contains(name, "/") {
			return fmt.Errorf("inmemory store name %q must not contain '/'", name)
		}
	default:
		if u.Host+u.Path == "" {
			return fmt.Errorf("%s URI %q has empty target", strings.ToLower(u.Scheme), raw)
		}
	}
	return nil
}

func validateMemoryAtom(label, val string) error {
	if val == "" {
		return fmt.Errorf("%s is empty", label)
	}
	if len(val) > 256 {
		return fmt.Errorf("%s exceeds 256 bytes", label)
	}
	if !utf8.ValidString(val) {
		return fmt.Errorf("%s is not valid UTF-8", label)
	}
	if strings.TrimSpace(val) != val {
		return fmt.Errorf("%s %q has surrounding whitespace", label, val)
	}
	if strings.ContainsRune(val, '*') {
		return fmt.Errorf("%s %q must not contain '*' wildcards", label, val)
	}
	for _, r := range val {
		if unicode.IsControl(r) {
			return fmt.Errorf("%s contains control character", label)
		}
	}
	return nil
}

func parsePositiveDuration(label, raw string) (time.Duration, error) {
	d, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil {
		return 0, fmt.Errorf("%s=%q: %w", label, raw, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%s=%q must be > 0", label, raw)
	}
	return d, nil
}
