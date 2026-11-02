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

// Workload bundle file and directory names (#1402).
const (
	CurrentWorkloadSchemaVersion = 1
	WorkloadFileName             = "workload.yaml"
	SpecialistsDirName           = "specialists"
	SchemasDirName               = "schemas"
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
		Tools:    ToolsConfig{Policies: expandWorkloadToolPolicies(b)},
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
	const sep = "---"
	trimmed := bytes.TrimLeft(data, " \t\r\n")
	if !bytes.HasPrefix(trimmed, []byte(sep)) {
		return specialistFrontmatter{}, "", errors.New("missing frontmatter (file must start with `---`)")
	}
	rest := trimmed[len(sep):]
	nl := bytes.IndexByte(rest, '\n')
	if nl < 0 {
		return specialistFrontmatter{}, "", errors.New("missing frontmatter terminator")
	}
	rest = rest[nl+1:]
	end := bytes.Index(rest, []byte("\n---"))
	if end < 0 {
		return specialistFrontmatter{}, "", errors.New("missing closing `---`")
	}
	yamlBlock := rest[:end]
	body := rest[end+len("\n---"):]
	if len(body) > 0 && body[0] == '\n' {
		body = body[1:]
	} else if len(body) > 1 && body[0] == '\r' && body[1] == '\n' {
		body = body[2:]
	}
	var fm specialistFrontmatter
	dec := yaml.NewDecoder(bytes.NewReader(yamlBlock))
	dec.KnownFields(true)
	if err := dec.Decode(&fm); err != nil && !errors.Is(err, io.EOF) {
		return specialistFrontmatter{}, "", fmt.Errorf("parse frontmatter yaml: %w", err)
	}
	return fm, string(body), nil
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

// ImportWorkload converts a legacy workload bundle directory (<workloadDir>)
// into a core-agent recipe directory (<outDir>):
//   - <outDir>/AGENTS.md
//   - <outDir>/.agents/config.json
//   - <outDir>/.agents/mcp.json (copied from <workloadDir>/mcp.json if present)
//   - <outDir>/.agents/subagents/<name>.md
//   - <outDir>/.agents/schemas/*.json (copied from <workloadDir>/schemas/)
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
	if err := os.MkdirAll(filepath.Join(agentsDir, SubagentsDirName), 0o755); err != nil {
		return nil, fmt.Errorf("config: create subagents dir: %w", err)
	}
	if err := copyWorkloadSchemas(workloadDir, agentsDir); err != nil {
		return nil, err
	}
	if err := copyWorkloadMCPConfig(workloadDir, agentsDir); err != nil {
		return nil, err
	}
	for _, spec := range b.ResolvedSpecs {
		if err := writeSubagentMarkdownFile(filepath.Join(agentsDir, SubagentsDirName, spec.Name+SubagentMarkdownExt), spec); err != nil {
			return nil, err
		}
	}
	cfg := buildConfigFromWorkload(b)
	if err := writeConvertedConfigJSON(filepath.Join(agentsDir, "config.json"), cfg); err != nil {
		return nil, err
	}
	if err := writeWorkloadAgentsMD(filepath.Join(outDir, "AGENTS.md"), b); err != nil {
		return nil, err
	}
	return Load(agentsDir)
}

func buildConfigFromWorkload(b *WorkloadBundle) *Config {
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
		cfg.Tools.Policies = expandWorkloadToolPolicies(b)
	}
	if sc, err := b.ParsedSchedule(); err == nil && sc != nil {
		cfg.Schedule = sc
	}
	cfg.Monitor = workloadMonitorToConfig(b.Monitor)
	if a2aCfg, err := b.ParsedA2A(); err == nil {
		if a2aCfg.Expose {
			if a2aCfg.SkillName == "" {
				a2aCfg.SkillName = b.Name
			}
			if a2aCfg.SkillDescription == "" {
				a2aCfg.SkillDescription = b.Description
			}
		}
		cfg.A2A = a2aCfg
	}
	return cfg
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
		if specSet[t.Name] || strings.Contains(t.Name, "-") {
			add("spawn_agent:" + t.Name)
			continue
		}
		if len(b.ToolCatalog.MCP) == 0 {
			add("mcp:" + t.Name + "*")
			continue
		}
		for _, srv := range b.ToolCatalog.MCP {
			prefix := srv.Server + "_"
			if strings.HasPrefix(t.Name, prefix) {
				add("mcp:" + t.Name + "*")
			} else {
				add("mcp:" + prefix + t.Name + "*")
			}
		}
	}
	return allow
}

func expandWorkloadToolPolicies(b *WorkloadBundle) []ToolPolicyConfig {
	out := make([]ToolPolicyConfig, 0, len(b.ToolCatalog.Tools))
	out = append(out, b.ToolCatalog.Tools...)
	return out
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
	return os.WriteFile(path, data, 0o644)
}

func copyWorkloadSchemas(workloadDir, agentsDir string) error {
	srcDir := filepath.Join(workloadDir, SchemasDirName)
	if _, err := os.Stat(srcDir); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("config: stat schemas dir %q: %w", srcDir, err)
	}
	dstDir := filepath.Join(agentsDir, SchemasDirName)
	srcFS := os.DirFS(srcDir)
	return fs.WalkDir(srcFS, ".", func(rel string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		targetPath := filepath.Join(dstDir, filepath.FromSlash(rel))
		if d.IsDir() {
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

func copyWorkloadMCPConfig(workloadDir, agentsDir string) error {
	src := filepath.Join(workloadDir, "mcp.json")
	data, err := os.ReadFile(src)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("config: read %q: %w", src, err)
	}
	dst := filepath.Join(agentsDir, "mcp.json")
	if filepath.Clean(src) == filepath.Clean(dst) {
		return nil
	}
	// #nosec G703 -- dst is mcp.json inside caller-selected agentsDir.
	return os.WriteFile(dst, data, 0o644)
}

func writeWorkloadAgentsMD(path string, b *WorkloadBundle) error {
	if _, err := os.Stat(path); err == nil {
		return nil
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
	return os.WriteFile(path, []byte(sb.String()), 0o644)
}
