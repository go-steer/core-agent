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
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Workload bundle file and directory names (#1402, #1498).
const (
	CurrentWorkloadSchemaVersion = 1
	WorkloadFileName             = "workload.yaml"
	SpecialistsDirName           = "specialists"
	SchemasDirName               = "schemas"
	SubagentRootsDirName         = "roots"
	SpecialistFileExt            = ".specialist.md"
)

// WorkloadMode is the session mode declared by a workload bundle.
type WorkloadMode string

// Recognized WorkloadMode values.
const (
	WorkloadModeSingleSession WorkloadMode = "single_session"
	WorkloadModeMultiSession  WorkloadMode = "multi_session"
)

// Workload dispatch shapes.
const (
	WorkloadDispatchCoordinator = "coordinator"
	WorkloadDispatchGraph       = "graph"
	WorkloadDispatchFanout      = "fanout"
	WorkloadDispatchBounded     = "bounded"
	WorkloadDispatchAuto        = "auto"
)

// Workload HITL on_mutation values.
const (
	WorkloadOnMutationRequireApproval = "require_approval"
	WorkloadOnMutationGate            = "gate"
	WorkloadOnMutationApply           = "apply"
	WorkloadOnMutationDryRun          = "dry_run"
)

// WorkloadBundle is the parsed representation of a legacy mast workload
// directory (workload.yaml + specialists/*.specialist.md) for conversion
// into a core-agent recipe (#1402).
type WorkloadBundle struct {
	SchemaVersion int                 `yaml:"schema_version,omitempty"`
	Name          string              `yaml:"name"`
	Description   string              `yaml:"description,omitempty"`
	Mode          WorkloadMode        `yaml:"mode,omitempty"`
	ToolCatalog   WorkloadToolCatalog `yaml:"tool_catalog,omitempty"`
	Specialists   []string            `yaml:"specialists,omitempty"`
	Dispatch      string              `yaml:"dispatch,omitempty"`
	Fanout        WorkloadFanout      `yaml:"fanout,omitempty"`
	Budget        WorkloadBudget      `yaml:"budget,omitempty"`
	Safety        WorkloadSafety      `yaml:"safety,omitempty"`
	BuiltinTools  BuiltinToolsConfig  `yaml:"builtin_tools,omitempty"`
	PromptCache   WorkloadPromptCache `yaml:"prompt_cache,omitempty"`
	HITL          WorkloadHITL        `yaml:"hitl,omitempty"`
	HITLPolicy    WorkloadHITL        `yaml:"hitl_policy,omitempty"`
	Planner       WorkloadPlanner     `yaml:"planner,omitempty"`
	EdgeTrigger   WorkloadEdgeTrigger `yaml:"edge_trigger,omitempty"`
	Schedule      *ScheduleConfig     `yaml:"schedule,omitempty"`
	Monitor       WorkloadMonitor     `yaml:"monitor,omitempty"`
	A2A           map[string]any      `yaml:"a2a,omitempty"`
	AGUI          map[string]any      `yaml:"agui,omitempty"`

	// ResolvedSpecs holds the converted SubagentSpecs loaded from
	// specialists/<name>.specialist.md in roster order.
	ResolvedSpecs []SubagentSpec `yaml:"-"`
	Dir           string         `yaml:"-"`
}

// WorkloadToolCatalog is the tool_catalog block of workload.yaml.
type WorkloadToolCatalog struct {
	MCP   []WorkloadMCPServerRef `yaml:"mcp,omitempty"`
	Tools []ToolPolicyConfig     `yaml:"tools,omitempty"`
}

// WorkloadMCPServerRef references an MCP server in mcp.json.
type WorkloadMCPServerRef struct {
	Server string `yaml:"server"`
}

// WorkloadFanout is the fanout block of workload.yaml.
type WorkloadFanout struct {
	MaxConcurrency int `yaml:"max_concurrency,omitempty"`
}

// WorkloadBudget is the budget block of workload.yaml.
type WorkloadBudget struct {
	MaxTurns             int     `yaml:"max_turns,omitempty"`
	MaxWallclockSeconds  int     `yaml:"max_wallclock_seconds,omitempty"`
	MaxCostUSD           float64 `yaml:"max_cost_usd,omitempty"`
	FinalReport          bool    `yaml:"final_report,omitempty"`
	MaxModelCallsPerTurn int     `yaml:"max_model_calls_per_turn,omitempty"`
}

// WorkloadSafety is the safety block of workload.yaml.
type WorkloadSafety struct {
	Watchdog string `yaml:"watchdog,omitempty"`
}

// WorkloadPromptCache is the prompt_cache block of workload.yaml.
type WorkloadPromptCache struct {
	Enabled *bool  `yaml:"enabled,omitempty"`
	TTL     string `yaml:"ttl,omitempty"`
}

// WorkloadHITL is the hitl / hitl_policy block of workload.yaml.
type WorkloadHITL struct {
	RequireApproval bool   `yaml:"require_approval,omitempty"`
	OnMutation      string `yaml:"on_mutation,omitempty"`
	ChangeSetTTL    string `yaml:"change_set_ttl,omitempty"`
}

// WorkloadPlanner is the planner block of workload.yaml.
type WorkloadPlanner struct {
	Enabled bool `yaml:"enabled,omitempty"`
}

// WorkloadEdgeTrigger is the edge_trigger block of workload.yaml.
type WorkloadEdgeTrigger struct {
	HTTP      *WorkloadHTTPTrigger      `yaml:"http,omitempty"`
	Scheduled *WorkloadScheduledTrigger `yaml:"scheduled,omitempty"`
}

// WorkloadHTTPTrigger declares inbound HTTP trigger metadata.
type WorkloadHTTPTrigger struct {
	Path string `yaml:"path,omitempty"`
	Auth string `yaml:"auth,omitempty"`
}

// WorkloadScheduledTrigger declares a periodic schedule on workload.yaml.
type WorkloadScheduledTrigger struct {
	Every    string `yaml:"every,omitempty"`
	Interval string `yaml:"interval,omitempty"`
	Jitter   string `yaml:"jitter,omitempty"`
	Prompt   string `yaml:"prompt,omitempty"`
	MaxFires int    `yaml:"max_fires,omitempty"`
	Timeout  string `yaml:"timeout,omitempty"`
}

func (s *WorkloadScheduledTrigger) toScheduleConfig() *ScheduleConfig {
	if s == nil {
		return nil
	}
	return &ScheduleConfig{
		Every:    s.Every,
		Interval: s.Interval,
		Jitter:   s.Jitter,
		Prompt:   s.Prompt,
		MaxFires: s.MaxFires,
		Timeout:  s.Timeout,
	}
}

// EffectiveInterval parses edge_trigger.scheduled.interval. A nil receiver
// returns (0, nil).
func (s *WorkloadScheduledTrigger) EffectiveInterval() (time.Duration, error) {
	return s.toScheduleConfig().EffectiveInterval()
}

// EffectiveJitter resolves edge_trigger.scheduled.jitter against the interval.
func (s *WorkloadScheduledTrigger) EffectiveJitter() (time.Duration, error) {
	return s.toScheduleConfig().EffectiveJitter()
}

// ParsedSchedule returns the validated ScheduleConfig declared by the workload
// bundle (from either top-level `schedule:` or `edge_trigger.scheduled:`), or
// (nil, nil) when the bundle declares no schedule (#1405).
func (b *WorkloadBundle) ParsedSchedule() (*ScheduleConfig, error) {
	if b == nil {
		return nil, nil
	}
	if b.Schedule != nil && b.EdgeTrigger.Scheduled != nil {
		return nil, errors.New("both schedule: and edge_trigger.scheduled: are set; keep one")
	}
	var sc *ScheduleConfig
	switch {
	case b.Schedule != nil:
		cp := *b.Schedule
		sc = &cp
	case b.EdgeTrigger.Scheduled != nil:
		sc = b.EdgeTrigger.Scheduled.toScheduleConfig()
	default:
		return nil, nil
	}
	if err := sc.Validate(); err != nil {
		return nil, err
	}
	return sc, nil
}

// WorkloadMonitor declares deterministic monitoring cycle hooks.
type WorkloadMonitor struct {
	Collect         []WorkloadMonitorCollect `yaml:"collect,omitempty"`
	TransitionsFrom string                   `yaml:"transitions_from,omitempty"`
	WakeOn          []string                 `yaml:"wake_on,omitempty"`
	Notify          *WorkloadMonitorNotify   `yaml:"notify,omitempty"`
	Ack             *WorkloadMonitorAck      `yaml:"ack,omitempty"`
}

// UnmarshalYAML accepts monitor.collect as either a single mapping
// (tool/args) or a sequence of mappings so legacy mast workload.yaml files of
// either spelling decode into WorkloadMonitor.Collect.
func (m *WorkloadMonitor) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(value.Content); i += 2 {
			if value.Content[i].Value == "collect" && value.Content[i+1].Kind == yaml.MappingNode {
				value.Content[i+1] = &yaml.Node{
					Kind:    yaml.SequenceNode,
					Content: []*yaml.Node{value.Content[i+1]},
				}
			}
		}
	}
	raw, err := yaml.Marshal(value)
	if err != nil {
		return err
	}
	type plain WorkloadMonitor
	var p plain
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&p); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	*m = WorkloadMonitor(p)
	return nil
}

// WorkloadMonitorCollect is one collection call in monitor.collect.
type WorkloadMonitorCollect struct {
	Tool string         `yaml:"tool"`
	Args map[string]any `yaml:"args,omitempty"`
	As   string         `yaml:"as,omitempty"`
}

// WorkloadMonitorNotify configures monitor.notify.
type WorkloadMonitorNotify struct {
	Conversation string `yaml:"conversation,omitempty"`
	Channel      string `yaml:"channel,omitempty"`
	Endpoint     string `yaml:"endpoint,omitempty"`
	TokenEnv     string `yaml:"token_env,omitempty"`
	Update       string `yaml:"update,omitempty"`
	RemindAfter  string `yaml:"remind_after,omitempty"`
	DigestAfter  string `yaml:"digest_after,omitempty"`
}

// WorkloadMonitorAck configures monitor.ack.
type WorkloadMonitorAck struct {
	Tool string         `yaml:"tool"`
	Args map[string]any `yaml:"args,omitempty"`
}

// LoadWorkload reads and validates a legacy workload bundle directory
// (<workloadDir>/workload.yaml + specialists/<name>.specialist.md).
func LoadWorkload(workloadDir string) (*WorkloadBundle, error) {
	path := filepath.Join(workloadDir, WorkloadFileName)
	// #nosec G703 -- path is workload.yaml inside caller-selected workloadDir.
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read workload %q: %w", path, err)
	}
	if err := checkWorkloadSchemaVersion(data); err != nil {
		return nil, fmt.Errorf("config: workload %q: %w", path, err)
	}
	var b WorkloadBundle
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&b); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("config: parse workload %q: %w", path, workloadUnknownKeyHint(err))
	}
	if b.SchemaVersion == 0 {
		b.SchemaVersion = CurrentWorkloadSchemaVersion
	}
	b.Dir = workloadDir
	if err := b.foldHITLPolicy(); err != nil {
		return nil, fmt.Errorf("config: workload %q: %w", path, err)
	}
	if err := b.validate(); err != nil {
		return nil, fmt.Errorf("config: validate workload %q: %w", path, err)
	}
	if b.Mode == "" {
		b.Mode = WorkloadModeSingleSession
	}
	specs, err := loadWorkloadSpecialists(workloadDir, b.Specialists)
	if err != nil {
		return nil, err
	}
	if err := checkWorkloadCapabilitySplit(b, specs); err != nil {
		return nil, fmt.Errorf("config: validate workload %q: %w", path, err)
	}
	if monCfg := workloadMonitorToConfig(b.Monitor); monCfg != nil {
		if err := CheckMonitorCollectSurface(&Config{
			Monitor:   monCfg,
			Subagents: specs,
		}, len(b.ToolCatalog.MCP) > 0); err != nil {
			return nil, fmt.Errorf("config: validate workload %q: %w", path, err)
		}
	}
	b.ResolvedSpecs = specs
	return &b, nil
}

func checkWorkloadSchemaVersion(data []byte) error {
	var probe struct {
		SchemaVersion *int `yaml:"schema_version"`
	}
	if err := yaml.Unmarshal(data, &probe); err != nil || probe.SchemaVersion == nil {
		return nil
	}
	v := *probe.SchemaVersion
	if v < 1 {
		return fmt.Errorf("schema_version is %d; must be a positive integer (omit for schema 1)", v)
	}
	if v > CurrentWorkloadSchemaVersion {
		return fmt.Errorf("bundle declares schema_version %d and this binary speaks %d", v, CurrentWorkloadSchemaVersion)
	}
	return nil
}

func workloadUnknownKeyHint(err error) error {
	if err == nil || !strings.Contains(err.Error(), "not found in type") {
		return err
	}
	return fmt.Errorf("%w (unrecognised key in workload.yaml; check spelling against the bundle schema)", err)
}

func (b *WorkloadBundle) foldHITLPolicy() error {
	if b.HITLPolicy == (WorkloadHITL{}) {
		return nil
	}
	if b.HITL != (WorkloadHITL{}) {
		return errors.New("both hitl: and hitl_policy: are set; keep one")
	}
	b.HITL = b.HITLPolicy
	b.HITLPolicy = WorkloadHITL{}
	return nil
}

func (b *WorkloadBundle) validate() error {
	if strings.TrimSpace(b.Name) == "" {
		return errors.New("name is required")
	}
	if len(b.Specialists) == 0 {
		return errors.New("specialists roster is empty")
	}
	if b.Mode != "" && b.Mode != WorkloadModeSingleSession && b.Mode != WorkloadModeMultiSession {
		return fmt.Errorf("unknown mode %q (want %q or %q)", b.Mode, WorkloadModeSingleSession, WorkloadModeMultiSession)
	}
	if b.Planner.Enabled {
		return errors.New("planner.enabled is retired in core-agent (docs/mast-fold-design.md Out of scope)")
	}
	switch b.Dispatch {
	case "", WorkloadDispatchCoordinator, WorkloadDispatchFanout, WorkloadDispatchBounded, WorkloadDispatchAuto:
	case WorkloadDispatchGraph:
		return errors.New("dispatch \"graph\" is retired in core-agent (mast #504: one generalist diagnoser beat the routed graph roster 17/18 vs 13/18); omit dispatch or use coordinator")
	default:
		return fmt.Errorf("unknown dispatch %q (want coordinator, fanout, bounded, or auto)", b.Dispatch)
	}
	switch b.Safety.Watchdog {
	case "", WatchdogWarn, WatchdogFeedback, WatchdogEnforce:
	default:
		return fmt.Errorf("unknown safety.watchdog %q (want %q, %q, or %q)", b.Safety.Watchdog, WatchdogWarn, WatchdogFeedback, WatchdogEnforce)
	}
	switch b.PromptCache.TTL {
	case "", PromptCacheTTL5m, PromptCacheTTL1h:
	default:
		return fmt.Errorf("unknown prompt_cache.ttl %q (want %q or %q)", b.PromptCache.TTL, PromptCacheTTL5m, PromptCacheTTL1h)
	}
	if err := validateWorkloadHITL(b.HITL); err != nil {
		return err
	}
	if err := validateWorkloadRosterAndTools(b.Specialists, b.ToolCatalog.Tools); err != nil {
		return err
	}
	if _, err := b.ParsedA2A(); err != nil {
		return err
	}
	if err := b.validateWorkloadMonitor(); err != nil {
		return err
	}
	_, err := b.ParsedSchedule()
	return err
}

// ParsedA2A decodes and validates the workload's raw a2a: map into a typed
// A2AConfig using strict YAML field checking (#1407).
func (b *WorkloadBundle) ParsedA2A() (A2AConfig, error) {
	if b == nil || len(b.A2A) == 0 {
		return A2AConfig{}, nil
	}
	raw, err := yaml.Marshal(b.A2A)
	if err != nil {
		return A2AConfig{}, fmt.Errorf("a2a: %w", err)
	}
	var out A2AConfig
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&out); err != nil && !errors.Is(err, io.EOF) {
		return A2AConfig{}, fmt.Errorf("a2a: %w", workloadUnknownKeyHint(err))
	}
	if err := (&Config{A2A: out}).validateA2A(); err != nil {
		return A2AConfig{}, err
	}
	return out, nil
}

func (b *WorkloadBundle) validateWorkloadMonitor() error {
	if len(b.Monitor.Collect) > 1 {
		return fmt.Errorf("monitor.collect declares %d entries; core-agent supports at most 1 pre-turn collect tool", len(b.Monitor.Collect))
	}
	for i, col := range b.Monitor.Collect {
		if strings.TrimSpace(col.Tool) == "" {
			return fmt.Errorf("monitor.collect[%d].tool is required", i)
		}
	}
	var sc *ScheduleConfig
	switch {
	case b.Schedule != nil:
		cp := *b.Schedule
		sc = &cp
	case b.EdgeTrigger.Scheduled != nil:
		sc = b.EdgeTrigger.Scheduled.toScheduleConfig()
	}
	tmp := &Config{
		Schedule: sc,
		Monitor:  workloadMonitorToConfig(b.Monitor),
		Tools:    ToolsConfig{Policies: b.ToolCatalog.Tools},
	}
	return tmp.validateMonitor()
}

func workloadMonitorToConfig(m WorkloadMonitor) *MonitorConfig {
	hasCollect := len(m.Collect) > 0
	hasTransitions := strings.TrimSpace(m.TransitionsFrom) != ""
	hasWakeOn := len(m.WakeOn) > 0
	hasNotify := m.Notify != nil
	hasAck := m.Ack != nil
	if !hasCollect && !hasTransitions && !hasWakeOn && !hasNotify && !hasAck {
		return nil
	}
	out := &MonitorConfig{
		TransitionsFrom: m.TransitionsFrom,
		WakeOn:          m.WakeOn,
	}
	if hasCollect {
		out.Collect = &MonitorCollectConfig{
			Tool: m.Collect[0].Tool,
			Args: m.Collect[0].Args,
		}
	}
	if m.Notify != nil {
		ch := m.Notify.Channel
		if ch == "" {
			ch = m.Notify.Conversation
		}
		out.Notify = &MonitorNotifyConfig{
			Channel:     ch,
			Endpoint:    m.Notify.Endpoint,
			TokenEnv:    m.Notify.TokenEnv,
			Update:      m.Notify.Update,
			RemindAfter: m.Notify.RemindAfter,
			DigestAfter: m.Notify.DigestAfter,
		}
	}
	if m.Ack != nil {
		out.Ack = &MonitorAckConfig{
			Tool: m.Ack.Tool,
			Args: m.Ack.Args,
		}
	}
	return out
}

func validateWorkloadHITL(h WorkloadHITL) error {
	switch h.OnMutation {
	case "", WorkloadOnMutationRequireApproval, WorkloadOnMutationGate, WorkloadOnMutationApply, WorkloadOnMutationDryRun:
	default:
		return fmt.Errorf("unknown hitl.on_mutation %q (want require_approval, gate, apply, or dry_run)", h.OnMutation)
	}
	if strings.TrimSpace(h.ChangeSetTTL) != "" {
		d, err := time.ParseDuration(h.ChangeSetTTL)
		if err != nil || d <= 0 {
			return fmt.Errorf("hitl.change_set_ttl %q must be a positive duration (e.g. \"10m\")", h.ChangeSetTTL)
		}
	}
	return nil
}

func validateWorkloadRosterAndTools(specialists []string, tools []ToolPolicyConfig) error {
	seenSpec := make(map[string]bool, len(specialists))
	for _, s := range specialists {
		if strings.TrimSpace(s) == "" {
			return errors.New("specialists roster contains an empty entry")
		}
		if !validSubagentName(s) {
			return fmt.Errorf("specialists roster entry %q is not a valid subagent name", s)
		}
		if seenSpec[s] {
			return fmt.Errorf("specialists roster contains duplicate %q", s)
		}
		seenSpec[s] = true
	}
	seenTools := make(map[string]bool, len(tools))
	for _, p := range tools {
		if strings.TrimSpace(p.Name) == "" {
			return errors.New("tool_catalog.tools contains an entry without a name")
		}
		if seenTools[p.Name] {
			return fmt.Errorf("tool_catalog.tools contains duplicate %q", p.Name)
		}
		seenTools[p.Name] = true
		if err := validateWorkloadToolPolicy(p); err != nil {
			return err
		}
	}
	return nil
}

func validateWorkloadToolPolicy(p ToolPolicyConfig) error {
	for _, f := range p.ParkHintFields {
		if strings.TrimSpace(f) == "" {
			return fmt.Errorf("tool_catalog.tools[%q].park_hint_fields contains an empty field name", p.Name)
		}
	}
	if pre := p.Precondition; pre != nil {
		if strings.TrimSpace(pre.Read) == "" {
			return fmt.Errorf("tool_catalog.tools[%q].precondition names no read tool", p.Name)
		}
		if pre.Read == p.Name {
			return fmt.Errorf("tool_catalog.tools[%q].precondition reads %q (a tool cannot be its own precondition)", p.Name, pre.Read)
		}
		for _, f := range pre.Fields {
			if strings.TrimSpace(f) == "" {
				return fmt.Errorf("tool_catalog.tools[%q].precondition.fields contains an empty path", p.Name)
			}
		}
	}
	if capCfg := p.Capture; capCfg != nil {
		if strings.TrimSpace(capCfg.Read) == "" {
			return fmt.Errorf("tool_catalog.tools[%q].capture names no read tool", p.Name)
		}
		if capCfg.Read == p.Name {
			return fmt.Errorf("tool_catalog.tools[%q].capture reads %q (a tool cannot record itself)", p.Name, capCfg.Read)
		}
		for _, f := range capCfg.Fields {
			if strings.TrimSpace(f) == "" {
				return fmt.Errorf("tool_catalog.tools[%q].capture.fields contains an empty path", p.Name)
			}
		}
		if capCfg.Revert != nil && strings.TrimSpace(capCfg.Revert.Call) == "" {
			return fmt.Errorf("tool_catalog.tools[%q].capture.revert names no call tool", p.Name)
		}
	}
	return nil
}

// specialistFrontmatter is the YAML frontmatter of a legacy
// specialists/<name>.specialist.md file.
type specialistFrontmatter struct {
	Name         string               `yaml:"name"`
	Description  string               `yaml:"description"`
	Mode         string               `yaml:"mode"`
	Tier         string               `yaml:"tier"`
	Model        *frontmatterModel    `yaml:"model"`
	Capability   string               `yaml:"capability"`
	OutputSchema string               `yaml:"output_schema"`
	Inherit      []string             `yaml:"inherit"`
	Budget       *SubagentBudgets     `yaml:"budget"`
	Budgets      *SubagentBudgets     `yaml:"budgets"`
	Tools        specialistToolsBlock `yaml:"tools"`
}

type specialistToolsBlock struct {
	MCP     []MCPToolAllowlist `yaml:"mcp"`
	Builtin []string           `yaml:"builtin"`
	Skills  []string           `yaml:"skills"`
}

func loadWorkloadSpecialists(workloadDir string, names []string) ([]SubagentSpec, error) {
	specs := make([]SubagentSpec, 0, len(names))
	for _, name := range names {
		p := filepath.Join(workloadDir, SpecialistsDirName, name+SpecialistFileExt)
		spec, err := loadSpecialistMarkdownFile(p, name)
		if err != nil {
			return nil, err
		}
		specs = append(specs, spec)
	}
	return specs, nil
}

func loadSpecialistMarkdownFile(path, expectedName string) (SubagentSpec, error) {
	// #nosec G703 -- path is a validated specialist file inside workloadDir/specialists.
	data, err := os.ReadFile(path)
	if err != nil {
		return SubagentSpec{}, fmt.Errorf("config: read specialist %q: %w", path, err)
	}
	fm, body, err := splitSpecialistFrontmatter(data)
	if err != nil {
		return SubagentSpec{}, fmt.Errorf("config: specialist %q: %w", path, err)
	}
	if err := checkPlaceholders(path, body); err != nil {
		return SubagentSpec{}, err
	}
	name := fm.Name
	if name == "" {
		name = expectedName
	}
	if expectedName != "" && name != expectedName {
		return SubagentSpec{}, fmt.Errorf("config: specialist %q: frontmatter name %q does not match filename %q", path, name, expectedName)
	}
	if strings.TrimSpace(fm.Description) == "" {
		return SubagentSpec{}, fmt.Errorf("config: specialist %q: description is required", path)
	}
	mode, err := normalizeSpecialistMode(fm.Mode)
	if err != nil {
		return SubagentSpec{}, fmt.Errorf("config: specialist %q: %w", path, err)
	}
	if fm.Capability == CapabilityChangeExecutor && mode == SubagentModeSingleTurn {
		return SubagentSpec{}, fmt.Errorf("config: specialist %q: capability %q requires mode task (single_turn receives no tools)", path, CapabilityChangeExecutor)
	}
	budgets := fm.Budgets
	if budgets == nil {
		budgets = fm.Budget
	}
	var modelCfg *ModelConfig
	if fm.Model != nil {
		mc := fm.Model.ModelConfig
		modelCfg = &mc
	}
	builtinTools := fm.Tools.Builtin
	if builtinTools == nil && mode == SubagentModeTask {
		builtinTools = []string{}
	}
	baseDir := filepath.Dir(path)
	var mcpList []string
	if fm.Tools.MCP != nil && len(fm.Tools.MCP) == 0 {
		mcpList = []string{}
	}
	spec := SubagentSpec{
		Name:         name,
		Description:  fm.Description,
		Instructions: strings.TrimSpace(body),
		Mode:         mode,
		OutputSchema: fm.OutputSchema,
		Tier:         fm.Tier,
		Capability:   fm.Capability,
		Model:        modelCfg,
		Tools:        builtinTools,
		MCP:          mcpList,
		MCPTools:     fm.Tools.MCP,
		Skills:       fm.Tools.Skills,
		Inherit:      fm.Inherit,
		Budgets:      budgets,
		fromMarkdown: true,
		baseDir:      baseDir,
	}
	if err := (&Config{Subagents: []SubagentSpec{spec}}).validateSubagents(); err != nil {
		return SubagentSpec{}, fmt.Errorf("config: specialist %q: %w", path, err)
	}
	return spec, nil
}

func splitSpecialistFrontmatter(data []byte) (specialistFrontmatter, string, error) {
	yamlBlock, body, err := splitYAMLFrontmatterBytes(data)
	if err != nil {
		return specialistFrontmatter{}, "", err
	}
	var fm specialistFrontmatter
	dec := yaml.NewDecoder(bytes.NewReader(yamlBlock))
	dec.KnownFields(true)
	if err := dec.Decode(&fm); err != nil && !errors.Is(err, io.EOF) {
		return specialistFrontmatter{}, "", fmt.Errorf("parse frontmatter yaml: %w", err)
	}
	return fm, body, nil
}

func normalizeSpecialistMode(raw string) (string, error) {
	switch strings.TrimSpace(raw) {
	case "", "Task", SubagentModeTask:
		return SubagentModeTask, nil
	case "SingleTurn", SubagentModeSingleTurn:
		return SubagentModeSingleTurn, nil
	case "Chat", SubagentModeChat:
		return SubagentModeChat, nil
	default:
		return "", fmt.Errorf("unknown mode %q (want Task/task, SingleTurn/single_turn, or Chat/chat)", raw)
	}
}

func checkWorkloadCapabilitySplit(b WorkloadBundle, specs []SubagentSpec) error {
	mutatingTools := make(map[string]bool)
	for _, t := range b.ToolCatalog.Tools {
		if t.Mutating == nil || *t.Mutating {
			mutatingTools[t.Name] = true
		}
	}
	if len(mutatingTools) == 0 {
		return nil
	}
	for _, s := range specs {
		if s.Capability == CapabilityChangeExecutor || s.EffectiveMode() == SubagentModeSingleTurn {
			continue
		}
		if s.MCP == nil && len(s.MCPTools) == 0 && len(b.ToolCatalog.MCP) > 0 {
			return fmt.Errorf("diagnoser specialist %q omits tools.mcp while tool_catalog contains mutating tools; list read-only tools explicitly or declare capability: change_executor", s.Name)
		}
		for _, mt := range s.MCPTools {
			if len(mt.Tools) == 0 {
				return fmt.Errorf("diagnoser specialist %q grants all tools on MCP server %q while tool_catalog contains mutating tools; list read-only tools explicitly or declare capability: change_executor", s.Name, mt.Server)
			}
			for _, toolName := range mt.Tools {
				if mutatingTools[toolName] {
					return fmt.Errorf("diagnoser specialist %q declares mutating tool %q; move write tools to a change_executor or declare capability: change_executor", s.Name, toolName)
				}
			}
		}
	}
	return nil
}

// LoadWorkloadConfig loads and validates a legacy workload bundle directory
// (<workloadDir>/workload.yaml + specialists/<name>.specialist.md) into an
// in-memory *Config without writing files to disk (#1455).
func LoadWorkloadConfig(workloadDir string) (*Config, error) {
	b, err := LoadWorkload(workloadDir)
	if err != nil {
		return nil, err
	}
	cfg, err := buildConfigFromWorkload(b)
	if err != nil {
		return nil, err
	}
	cfg.Subagents = append([]SubagentSpec(nil), b.ResolvedSpecs...)
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// ImportWorkload converts a legacy workload bundle directory (<workloadDir>)
// into a core-agent recipe directory (<outDir>):
//   - <outDir>/AGENTS.md
//   - <outDir>/.agents/config.json
//   - <outDir>/.agents/roots/<name>/mcp.json (per-specialist MCP roots from <workloadDir>/mcp.json; #1498)
//   - <outDir>/.agents/subagents/<name>.md
//   - <outDir>/.agents/schemas/*.json (copied from <workloadDir>/schemas/)
//
// In mast, a workload's tool_catalog only classifies tools for the write gate;
// the coordinator (main agent) never holds MCP servers itself — only
// specialists hold MCP tools via their frontmatter allowlists (#1498). Each
// specialist that references MCP servers is therefore emitted with a dedicated
// content root (root: roots/<name>) holding only the servers it names, and
// the main agent receives no .agents/mcp.json.
//
// It then loads and validates the generated recipe with Load(<outDir>/.agents)
// and returns the resulting *Config (#1402).
func ImportWorkload(workloadDir, outDir string) (*Config, error) {
	b, err := LoadWorkload(workloadDir)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(outDir) == "" {
		outDir = workloadDir
	}
	agentsDir := filepath.Join(outDir, ".agents")
	subagentsDir := filepath.Join(agentsDir, SubagentsDirName)
	// #nosec G703 -- subagentsDir is .agents/subagents inside caller-selected outDir.
	if err := os.MkdirAll(subagentsDir, 0o755); err != nil {
		return nil, fmt.Errorf("config: create subagents dir: %w", err)
	}
	if err := copyWorkloadSchemas(workloadDir, agentsDir); err != nil {
		return nil, err
	}
	specs, err := writeWorkloadSpecialistRoots(workloadDir, agentsDir, b)
	if err != nil {
		return nil, err
	}
	if err := pruneStaleSubagentMarkdownFiles(subagentsDir, specs); err != nil {
		return nil, err
	}
	for _, spec := range specs {
		if err := writeSubagentMarkdownFile(filepath.Join(subagentsDir, spec.Name+SubagentMarkdownExt), spec); err != nil {
			return nil, err
		}
	}
	cfg, err := buildConfigFromWorkload(b)
	if err != nil {
		return nil, err
	}
	if err := writeConvertedConfigJSON(filepath.Join(agentsDir, "config.json"), cfg); err != nil {
		return nil, err
	}
	if err := writeWorkloadAgentsMD(workloadDir, outDir, filepath.Join(outDir, "AGENTS.md"), b); err != nil {
		return nil, err
	}
	return Load(agentsDir)
}

func pruneStaleSubagentMarkdownFiles(subagentsDir string, active []SubagentSpec) error {
	keep := make(map[string]bool, len(active))
	for _, s := range active {
		keep[s.Name+SubagentMarkdownExt] = true
	}
	entries, err := os.ReadDir(subagentsDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("config: read subagents dir %q: %w", subagentsDir, err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), SubagentMarkdownExt) {
			continue
		}
		if !keep[e.Name()] {
			// #nosec G703 -- e.Name() is a directory entry inside subagentsDir.
			if err := os.Remove(filepath.Join(subagentsDir, e.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("config: remove stale subagent %q: %w", e.Name(), err)
			}
		}
	}
	return nil
}

func buildConfigFromWorkload(b *WorkloadBundle) (*Config, error) {
	cfg := DefaultConfig()
	cfg.Agent.DisplayName = b.Name
	cfg.Agent.Description = b.Description
	if b.Mode == WorkloadModeMultiSession {
		cfg.Attach.MultiSession.Enabled = true
		cfg.Attach.MultiSession.Auth = MultiSessionAuthConfig{
			Kind:      MultiSessionAuthKindBearerTable,
			TableFile: ".agents/bearer-tokens.json",
		}
	}

	if b.Budget.MaxCostUSD > 0 {
		cost := b.Budget.MaxCostUSD
		cfg.Agent.MaxSessionCostUSD = &cost
	}
	if b.Budget.MaxTurns > 0 {
		cfg.Agent.MaxSteps = b.Budget.MaxTurns
	} else if b.Budget.MaxModelCallsPerTurn > 0 {
		cfg.Agent.MaxSteps = b.Budget.MaxModelCallsPerTurn
	}
	if b.Budget.MaxWallclockSeconds > 0 {
		cfg.Agent.MaxWallclockSeconds = b.Budget.MaxWallclockSeconds
	}
	if b.Budget.FinalReport {
		cfg.Agent.FinalReport = true
	}
	if b.Safety.Watchdog != "" {
		cfg.Safety.Watchdog = b.Safety.Watchdog
	}

	// mast's builtin_tools baseline is all three server-side built-ins off
	// unless explicitly enabled.
	ws := b.BuiltinTools.WebSearch != nil && *b.BuiltinTools.WebSearch
	uc := b.BuiltinTools.URLContext != nil && *b.BuiltinTools.URLContext
	ce := b.BuiltinTools.CodeExecution != nil && *b.BuiltinTools.CodeExecution
	cfg.Model.BuiltinTools = &BuiltinToolsConfig{
		WebSearch:     &ws,
		URLContext:    &uc,
		CodeExecution: &ce,
	}

	if b.PromptCache.Enabled != nil || b.PromptCache.TTL != "" {
		cfg.Model.Anthropic = &AnthropicConfig{
			PromptCache: &PromptCacheConfig{
				Enabled: b.PromptCache.Enabled,
				TTL:     b.PromptCache.TTL,
			},
		}
	}

	cfg.Permissions.Mode = mapWorkloadHITLMode(b.HITL)
	cfg.Permissions.Allow = buildWorkloadAllowPatterns(b)
	if len(b.ToolCatalog.Tools) > 0 {
		cfg.Tools.Policies = append([]ToolPolicyConfig(nil), b.ToolCatalog.Tools...)
	}
	sc, err := b.ParsedSchedule()
	if err != nil {
		return nil, err
	}
	cfg.Schedule = sc
	cfg.Monitor = workloadMonitorToConfig(b.Monitor)
	a2aCfg, err := b.ParsedA2A()
	if err != nil {
		return nil, err
	}
	if a2aCfg.Expose {
		if a2aCfg.SkillName == "" {
			a2aCfg.SkillName = b.Name
		}
		if a2aCfg.SkillDescription == "" {
			a2aCfg.SkillDescription = b.Description
		}
	}
	cfg.A2A = a2aCfg
	return cfg, nil
}

func mapWorkloadHITLMode(h WorkloadHITL) string {
	switch h.OnMutation {
	case WorkloadOnMutationApply:
		if h.RequireApproval {
			return PermissionModeAsk
		}
		return PermissionModeAllow
	case WorkloadOnMutationDryRun:
		return PermissionModePlan
	default:
		return PermissionModeAsk
	}
}

func sanitizeMCPServerPrefix(s string) string {
	var sb strings.Builder
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			sb.WriteRune(r)
		default:
			sb.WriteByte('_')
		}
	}
	return sb.String()
}

func qualifyMCPToolName(prefix, toolName string) string {
	if prefix == "" || strings.HasPrefix(toolName, prefix) {
		return toolName
	}
	return prefix + toolName
}

func buildWorkloadAllowPatterns(b *WorkloadBundle) []string {
	specSet := make(map[string]bool, len(b.Specialists))
	for _, s := range b.Specialists {
		specSet[s] = true
	}
	var allow []string
	seen := make(map[string]bool)
	add := func(pat string) {
		if !seen[pat] {
			seen[pat] = true
			allow = append(allow, pat)
		}
	}
	for _, s := range b.Specialists {
		add("spawn_agent:" + s)
	}
	for _, t := range b.ToolCatalog.Tools {
		if t.Mutating == nil || *t.Mutating {
			continue
		}
		if specSet[t.Name] {
			add("spawn_agent:" + t.Name)
			continue
		}
		if len(b.ToolCatalog.MCP) == 0 {
			add("mcp:" + t.Name + "*")
			continue
		}
		for _, srv := range b.ToolCatalog.MCP {
			prefix := sanitizeMCPServerPrefix(srv.Server) + "_"
			add("mcp:" + qualifyMCPToolName(prefix, t.Name) + "*")
		}
	}
	return allow
}

type subagentFrontmatterOut struct {
	Name         string             `yaml:"name"`
	Description  string             `yaml:"description,omitempty"`
	Mode         string             `yaml:"mode"`
	OutputSchema string             `yaml:"output_schema,omitempty"`
	Tier         string             `yaml:"tier,omitempty"`
	Capability   string             `yaml:"capability,omitempty"`
	Model        any                `yaml:"model,omitempty"`
	Tools        *[]string          `yaml:"tools,omitempty"`
	MCP          *[]string          `yaml:"mcp,omitempty"`
	MCPTools     []MCPToolAllowlist `yaml:"mcp_tools,omitempty"`
	Skills       *[]string          `yaml:"skills,omitempty"`
	Inherit      []string           `yaml:"inherit,omitempty"`
	Root         string             `yaml:"root,omitempty"`
	Budgets      *SubagentBudgets   `yaml:"budgets,omitempty"`
}

func writeSubagentMarkdownFile(path string, spec SubagentSpec) error {
	fm := subagentFrontmatterOut{
		Name:         spec.Name,
		Description:  spec.Description,
		Mode:         spec.Mode,
		OutputSchema: spec.OutputSchema,
		Tier:         spec.Tier,
		Capability:   spec.Capability,
		MCPTools:     spec.MCPTools,
		Inherit:      spec.Inherit,
		Root:         spec.Root,
		Budgets:      spec.Budgets,
	}
	if spec.Model != nil {
		if spec.Model.Provider == "" && spec.Model.APIKey == "" && spec.Model.Vertex == nil && spec.Model.Anthropic == nil && len(spec.Model.Pricing) == 0 && spec.Model.BuiltinTools == nil {
			fm.Model = spec.Model.Name
		} else {
			fm.Model = spec.Model
		}
	}
	if spec.Tools != nil {
		t := spec.Tools
		fm.Tools = &t
	}
	if spec.MCP != nil {
		m := spec.MCP
		fm.MCP = &m
	}
	if spec.Skills != nil {
		s := spec.Skills
		fm.Skills = &s
	}
	var buf bytes.Buffer
	buf.WriteString("---\n")
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&fm); err != nil {
		return fmt.Errorf("config: encode subagent frontmatter %q: %w", path, err)
	}
	_ = enc.Close()
	buf.WriteString("---\n\n")
	if spec.Instructions != "" {
		buf.WriteString(spec.Instructions)
		if !strings.HasSuffix(spec.Instructions, "\n") {
			buf.WriteByte('\n')
		}
	}
	// #nosec G703 -- path is a validated subagent markdown file inside .agents/subagents.
	return os.WriteFile(path, buf.Bytes(), 0o644)
}

func writeConvertedConfigJSON(path string, cfg *Config) error {
	// Marshal a minimal on-disk representation containing only the fields
	// set by the workload conversion (plus version and model).
	type diskConfig struct {
		Version     int               `json:"version"`
		Model       ModelConfig       `json:"model"`
		Permissions PermissionsConfig `json:"permissions,omitempty"`
		Agent       AgentConfig       `json:"agent,omitempty"`
		Tools       ToolsConfig       `json:"tools,omitempty"`
		Safety      SafetyConfig      `json:"safety,omitempty"`
		Attach      AttachConfig      `json:"attach,omitempty"`
		Schedule    *ScheduleConfig   `json:"schedule,omitempty"`
		Monitor     *MonitorConfig    `json:"monitor,omitempty"`
		A2A         *A2AConfig        `json:"a2a,omitempty"`
	}
	var a2aPtr *A2AConfig
	if cfg.A2A.Expose || cfg.A2A.SkillName != "" || cfg.A2A.SkillDescription != "" ||
		len(cfg.A2A.InputSchema) > 0 || len(cfg.A2A.OutputSchema) > 0 ||
		cfg.A2A.Auth.Required || len(cfg.A2A.Auth.Scopes) > 0 {
		a2aCopy := cfg.A2A
		a2aPtr = &a2aCopy
	}
	disk := diskConfig{
		Version:     cfg.Version,
		Model:       cfg.Model,
		Permissions: cfg.Permissions,
		Agent:       cfg.Agent,
		Tools:       cfg.Tools,
		Safety:      cfg.Safety,
		Attach:      cfg.Attach,
		Schedule:    cfg.Schedule,
		Monitor:     cfg.Monitor,
		A2A:         a2aPtr,
	}
	data, err := json.MarshalIndent(disk, "", "  ")
	if err != nil {
		return fmt.Errorf("config: marshal %q: %w", path, err)
	}
	data = append(data, '\n')
	// #nosec G703 -- path is .agents/config.json inside caller-selected outDir.
	return os.WriteFile(path, data, 0o644)
}

func copyWorkloadSchemas(workloadDir, agentsDir string) error {
	srcDir := filepath.Join(workloadDir, SchemasDirName)
	// #nosec G703 -- srcDir is schemas/ inside caller-selected workloadDir.
	info, err := os.Lstat(srcDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("config: stat schemas dir %q: %w", srcDir, err)
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return fmt.Errorf("config: schemas dir %q is a symlink (refusing to follow symlink)", srcDir)
	}
	dstDir := filepath.Join(agentsDir, SchemasDirName)
	srcFS := os.DirFS(srcDir)
	return fs.WalkDir(srcFS, ".", func(rel string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("config: schema %q is a symlink (refusing to follow symlink out of %q)", rel, srcDir)
		}
		targetPath := filepath.Join(dstDir, filepath.FromSlash(rel))
		if d.IsDir() {
			// #nosec G703 -- rel is a validated fs.WalkDir path inside os.DirFS(srcDir).
			return os.MkdirAll(targetPath, 0o755)
		}
		data, err := fs.ReadFile(srcFS, rel)
		if err != nil {
			return fmt.Errorf("config: read schema %q: %w", rel, err)
		}
		// #nosec G703 -- rel is a validated fs.WalkDir path inside os.DirFS(srcDir).
		if err := os.WriteFile(targetPath, data, 0o644); err != nil {
			return fmt.Errorf("config: write schema %q: %w", rel, err)
		}
		return nil
	})
}

func writeWorkloadSpecialistRoots(workloadDir, agentsDir string, b *WorkloadBundle) ([]SubagentSpec, error) {
	out := make([]SubagentSpec, len(b.ResolvedSpecs))
	copy(out, b.ResolvedSpecs)

	mainMCP := filepath.Join(agentsDir, "mcp.json")
	srcMCP := filepath.Join(workloadDir, "mcp.json")

	// #nosec G703 -- srcMCP is mcp.json inside caller-selected workloadDir.
	rawMCP, readErr := os.ReadFile(srcMCP)
	hasSrcMCP := true
	if readErr != nil {
		if errors.Is(readErr, fs.ErrNotExist) {
			hasSrcMCP = false
		} else {
			return nil, fmt.Errorf("config: read %q: %w", srcMCP, readErr)
		}
	}

	// Remove any stale main-agent .agents/mcp.json left by an earlier import
	// so the coordinator does not inherit the workload's MCP servers (#1498).
	// When the workload declares self-run monitor tools (monitor.collect /
	// monitor.ack), write a main-agent .agents/mcp.json narrowed strictly to
	// those self-run tools so MonitorCollector and MonitorAcker can execute
	// them on the runtime's behalf while cmd/core-agent strips them from the
	// model-facing agent toolsets.
	var selfRunTools []string
	if monCfg := workloadMonitorToConfig(b.Monitor); monCfg != nil {
		selfRunTools = monCfg.SelfRunTools()
	}
	if len(selfRunTools) > 0 && hasSrcMCP {
		narrowed, err := narrowWorkloadMCPTools(rawMCP, selfRunTools)
		if err != nil {
			return nil, fmt.Errorf("config: narrow %q for monitor self-run tools: %w", srcMCP, err)
		}
		// #nosec G703 -- mainMCP is .agents/mcp.json inside caller-selected agentsDir.
		if err := os.WriteFile(mainMCP, narrowed, 0o644); err != nil {
			return nil, fmt.Errorf("config: write %q: %w", mainMCP, err)
		}
	} else if filepath.Clean(srcMCP) != filepath.Clean(mainMCP) {
		if err := os.Remove(mainMCP); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("config: remove main-agent %q: %w", mainMCP, err)
		}
	}

	hasWorkloadMCP := hasSrcMCP || len(b.ToolCatalog.MCP) > 0
	rootsDir := filepath.Join(agentsDir, SubagentRootsDirName)
	keepRoots := make(map[string]bool, len(out))
	for i := range out {
		servers, inheritAll := specialistMCPServers(out[i], hasWorkloadMCP)
		if len(servers) == 0 && !inheritAll {
			continue
		}
		out[i].Root = filepath.ToSlash(filepath.Join(SubagentRootsDirName, out[i].Name))
		keepRoots[out[i].Name] = true
		specRootDir := filepath.Join(rootsDir, out[i].Name)
		// #nosec G703 -- specRootDir is .agents/roots/<name> with validated subagent name.
		if err := os.MkdirAll(specRootDir, 0o755); err != nil {
			return nil, fmt.Errorf("config: create specialist root %q: %w", specRootDir, err)
		}
		dstMCP := filepath.Join(specRootDir, "mcp.json")
		if !hasSrcMCP {
			if err := os.Remove(dstMCP); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return nil, fmt.Errorf("config: remove %q: %w", dstMCP, err)
			}
			continue
		}
		filtered, err := filterWorkloadMCPJSON(rawMCP, servers)
		if err != nil {
			return nil, fmt.Errorf("config: filter %q for specialist %q: %w", srcMCP, out[i].Name, err)
		}
		// #nosec G703 -- dstMCP is .agents/roots/<name>/mcp.json inside caller-selected agentsDir.
		if err := os.WriteFile(dstMCP, filtered, 0o644); err != nil {
			return nil, fmt.Errorf("config: write %q: %w", dstMCP, err)
		}
	}
	if err := pruneStaleSubagentRoots(rootsDir, keepRoots); err != nil {
		return nil, err
	}
	return out, nil
}

func specialistMCPServers(spec SubagentSpec, hasWorkloadMCP bool) (servers []string, inheritAll bool) {
	if spec.EffectiveMode() == SubagentModeSingleTurn {
		return nil, false
	}
	if spec.MCP == nil && len(spec.MCPTools) == 0 {
		return nil, hasWorkloadMCP
	}
	var out []string
	seen := make(map[string]bool)
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, mt := range spec.MCPTools {
		add(mt.Server)
	}
	for _, m := range spec.MCP {
		add(m)
	}
	return out, false
}

func filterWorkloadMCPJSON(raw []byte, wantServers []string) ([]byte, error) {
	if len(wantServers) == 0 {
		out := append([]byte(nil), raw...)
		if len(out) > 0 && out[len(out)-1] != '\n' {
			out = append(out, '\n')
		}
		return out, nil
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, err
	}
	serverKey := ""
	switch {
	case len(top["servers"]) > 0:
		serverKey = "servers"
	case len(top["mcpServers"]) > 0:
		serverKey = "mcpServers"
	default:
		out := append([]byte(nil), raw...)
		if len(out) > 0 && out[len(out)-1] != '\n' {
			out = append(out, '\n')
		}
		return out, nil
	}
	var allServers map[string]json.RawMessage
	if err := json.Unmarshal(top[serverKey], &allServers); err != nil {
		return nil, err
	}
	filtered := make(map[string]json.RawMessage, len(wantServers))
	for _, want := range wantServers {
		if entry, ok := allServers[want]; ok {
			filtered[want] = entry
			continue
		}
		wantPrefix := sanitizeMCPServerPrefix(want)
		for k, entry := range allServers {
			if sanitizeMCPServerPrefix(k) == wantPrefix {
				filtered[k] = entry
			}
		}
	}
	if len(filtered) == len(allServers) {
		out := append([]byte(nil), raw...)
		if len(out) > 0 && out[len(out)-1] != '\n' {
			out = append(out, '\n')
		}
		return out, nil
	}
	filteredRaw, err := json.Marshal(filtered)
	if err != nil {
		return nil, err
	}
	top[serverKey] = filteredRaw
	out, err := json.MarshalIndent(top, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

func narrowWorkloadMCPTools(raw []byte, tools []string) ([]byte, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, err
	}
	serverKey := ""
	switch {
	case len(top["servers"]) > 0:
		serverKey = "servers"
	case len(top["mcpServers"]) > 0:
		serverKey = "mcpServers"
	default:
		return append(append([]byte(nil), raw...), '\n'), nil
	}
	var allServers map[string]map[string]any
	if err := json.Unmarshal(top[serverKey], &allServers); err != nil {
		return nil, err
	}
	for k, srv := range allServers {
		if srv == nil {
			continue
		}
		prefix := sanitizeMCPServerPrefix(k) + "_"
		unprefixed := make([]string, 0, len(tools))
		seen := make(map[string]bool, len(tools))
		for _, t := range tools {
			u := strings.TrimSpace(strings.TrimPrefix(t, prefix))
			if u != "" && !seen[u] {
				seen[u] = true
				unprefixed = append(unprefixed, u)
			}
		}
		srv["tools"] = unprefixed
		allServers[k] = srv
	}
	srvRaw, err := json.Marshal(allServers)
	if err != nil {
		return nil, err
	}
	top[serverKey] = srvRaw
	out, err := json.MarshalIndent(top, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

func pruneStaleSubagentRoots(rootsDir string, keep map[string]bool) error {
	entries, err := os.ReadDir(rootsDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("config: read roots dir %q: %w", rootsDir, err)
	}
	for _, e := range entries {
		if keep[e.Name()] {
			continue
		}
		// #nosec G703 -- e.Name() is a directory entry inside .agents/roots.
		if err := os.RemoveAll(filepath.Join(rootsDir, e.Name())); err != nil {
			return fmt.Errorf("config: remove stale specialist root %q: %w", e.Name(), err)
		}
	}
	return nil
}

func writeWorkloadAgentsMD(workloadDir, outDir, path string, b *WorkloadBundle) error {
	coordPath := filepath.Join(workloadDir, "coordinator.md")
	// #nosec G703 -- coordPath is coordinator.md inside caller-selected workloadDir.
	if data, err := os.ReadFile(coordPath); err == nil {
		// #nosec G703 -- path is AGENTS.md inside caller-selected outDir.
		return os.WriteFile(path, data, 0o644)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("config: read %q: %w", coordPath, err)
	}
	if filepath.Clean(workloadDir) != filepath.Clean(outDir) {
		srcAgents := filepath.Join(workloadDir, "AGENTS.md")
		// #nosec G703 -- srcAgents is AGENTS.md inside caller-selected workloadDir.
		if data, err := os.ReadFile(srcAgents); err == nil {
			if !strings.Contains(string(data), "\n## Subagent Roster\n") {
				// #nosec G703 -- path is AGENTS.md inside caller-selected outDir.
				return os.WriteFile(path, data, 0o644)
			}
		} else if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("config: read %q: %w", srcAgents, err)
		}
	}
	// #nosec G703 -- path is AGENTS.md inside caller-selected outDir.
	if existing, err := os.ReadFile(path); err == nil {
		if !strings.Contains(string(existing), "\n## Subagent Roster\n") {
			return nil
		}
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "# %s\n\n", b.Name)
	if b.Description != "" {
		fmt.Fprintf(&sb, "%s\n\n", b.Description)
	}
	sb.WriteString("## Subagent Roster\n\n")
	for _, spec := range b.ResolvedSpecs {
		desc := strings.ReplaceAll(strings.TrimSpace(spec.Description), "\n", " ")
		fmt.Fprintf(&sb, "- **`%s`** (`mode: %s`): %s\n", spec.Name, spec.EffectiveMode(), desc)
	}
	if b.EdgeTrigger.HTTP != nil && b.EdgeTrigger.HTTP.Path != "" {
		fmt.Fprintf(&sb, "\n## Ingress\n\n- HTTP trigger path: `%s` (auth: `%s`)\n", b.EdgeTrigger.HTTP.Path, b.EdgeTrigger.HTTP.Auth)
	}
	// #nosec G703 -- path is AGENTS.md inside caller-selected outDir.
	return os.WriteFile(path, []byte(sb.String()), 0o644)
}
