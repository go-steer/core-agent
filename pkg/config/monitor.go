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

// Originally derived from go-steer/mast.

package config

import (
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"
)

// Monitor notify update modes (#1406).
const (
	MonitorNotifyUpdateTimeline = "timeline"
	MonitorNotifyUpdateReplace  = "replace"
)

// MonitorConfig configures deterministic pre-turn collection, transition
// gating, outbound notifications, and operator acknowledgements (#1406).
type MonitorConfig struct {
	Collect         *MonitorCollectConfig `json:"collect,omitempty" yaml:"collect,omitempty"`
	TransitionsFrom string                `json:"transitions_from,omitempty" yaml:"transitions_from,omitempty"`
	WakeOn          []string              `json:"wake_on,omitempty" yaml:"wake_on,omitempty"`
	Notify          *MonitorNotifyConfig  `json:"notify,omitempty" yaml:"notify,omitempty"`
	Ack             *MonitorAckConfig     `json:"ack,omitempty" yaml:"ack,omitempty"`
}

// SelfRunTools returns the names of all tools the runtime calls on its own
// behalf (collect.tool and ack.tool). These tools must be excluded from
// tool-calling subagent surfaces so the LLM cannot invoke them mid-turn and
// corrupt stateful cursor or acknowledgement state.
func (m *MonitorConfig) SelfRunTools() []string {
	if m == nil {
		return nil
	}
	var out []string
	if m.Collect != nil {
		if t := strings.TrimSpace(m.Collect.Tool); t != "" {
			out = append(out, t)
		}
	}
	if m.Ack != nil {
		if t := strings.TrimSpace(m.Ack.Tool); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// MonitorCollectConfig declares a deterministic read-only tool call executed
// by the runtime before each scheduled turn (#1406).
type MonitorCollectConfig struct {
	Tool string         `json:"tool,omitempty" yaml:"tool,omitempty"`
	Args map[string]any `json:"args,omitempty" yaml:"args,omitempty"`
}

// MonitorNotifyConfig configures outbound switchboard notifications emitted at
// the end of a monitoring cycle (#1406).
type MonitorNotifyConfig struct {
	Channel     string `json:"channel,omitempty" yaml:"channel,omitempty"`
	Endpoint    string `json:"endpoint,omitempty" yaml:"endpoint,omitempty"`
	TokenEnv    string `json:"token_env,omitempty" yaml:"token_env,omitempty" envref:"env_var_name"`
	Update      string `json:"update,omitempty" yaml:"update,omitempty"`
	RemindAfter string `json:"remind_after,omitempty" yaml:"remind_after,omitempty"`
	DigestAfter string `json:"digest_after,omitempty" yaml:"digest_after,omitempty"`
}

// ResolvedRemindAfter parses RemindAfter, returning 0 when unset or invalid.
func (n *MonitorNotifyConfig) ResolvedRemindAfter() time.Duration {
	if n == nil || strings.TrimSpace(n.RemindAfter) == "" {
		return 0
	}
	d, _ := time.ParseDuration(n.RemindAfter)
	return d
}

// ResolvedDigestAfter parses DigestAfter, returning 0 when unset or invalid.
func (n *MonitorNotifyConfig) ResolvedDigestAfter() time.Duration {
	if n == nil || strings.TrimSpace(n.DigestAfter) == "" {
		return 0
	}
	d, _ := time.ParseDuration(n.DigestAfter)
	return d
}

// MonitorAckConfig configures optional MCP tool forwarding when an operator
// acknowledges an active subject via POST /monitor-ack (#1406).
type MonitorAckConfig struct {
	Tool string `json:"tool,omitempty" yaml:"tool,omitempty"`
}

var validWakeOnEdges = []string{"opened", "remained", "closed", "re_opened", "degraded"}

func (c *Config) validateMonitor() error {
	m := c.Monitor
	if m == nil {
		return nil
	}
	hasCollect := m.Collect != nil
	hasTransitions := strings.TrimSpace(m.TransitionsFrom) != ""
	hasWakeOn := len(m.WakeOn) > 0
	hasNotify := m.Notify != nil && (m.Notify.Channel != "" || m.Notify.Endpoint != "" || m.Notify.TokenEnv != "" || m.Notify.Update != "" || m.Notify.RemindAfter != "" || m.Notify.DigestAfter != "")
	hasAck := m.Ack != nil && strings.TrimSpace(m.Ack.Tool) != ""

	if !hasCollect && !hasTransitions && !hasWakeOn && !hasNotify && !hasAck {
		return nil
	}
	if c.Schedule == nil || (strings.TrimSpace(c.Schedule.Every) == "" && strings.TrimSpace(c.Schedule.Interval) == "") {
		return fmt.Errorf("config: monitor requires schedule.every to be set")
	}

	policyByTool := make(map[string]ToolPolicyConfig, len(c.Tools.Policies))
	for _, p := range c.Tools.Policies {
		policyByTool[p.Name] = p
	}

	var collectTool string
	if hasCollect {
		var err error
		collectTool, err = validateMonitorCollect(m.Collect, policyByTool)
		if err != nil {
			return err
		}
	}
	if err := validateMonitorTransitionsAndWakeOn(m, hasCollect, hasTransitions, collectTool); err != nil {
		return err
	}
	if hasNotify {
		if err := validateMonitorNotify(m.Notify, hasTransitions); err != nil {
			return err
		}
	}
	if hasAck {
		if err := validateMonitorAck(m.Ack, hasTransitions, policyByTool); err != nil {
			return err
		}
	}
	return nil
}

func validateMonitorCollect(col *MonitorCollectConfig, policyByTool map[string]ToolPolicyConfig) (string, error) {
	toolName := strings.TrimSpace(col.Tool)
	if toolName == "" {
		return "", fmt.Errorf("config: monitor.collect.tool is required")
	}
	pol, ok := policyByTool[toolName]
	if !ok {
		return "", fmt.Errorf("config: monitor.collect.tool %q is not declared in tools.policies / tool_catalog.tools (must be declared with mutating: false)", toolName)
	}
	if pol.Mutating == nil || *pol.Mutating {
		return "", fmt.Errorf("config: monitor.collect.tool %q is declared mutating in tools.policies / tool_catalog.tools (monitor.collect only runs read-only tools with mutating: false)", toolName)
	}
	return toolName, nil
}

func validateMonitorTransitionsAndWakeOn(m *MonitorConfig, hasCollect, hasTransitions bool, collectTool string) error {
	if hasTransitions {
		if !hasCollect {
			return fmt.Errorf("config: monitor.transitions_from %q requires monitor.collect to be set", m.TransitionsFrom)
		}
		if strings.TrimSpace(m.TransitionsFrom) != collectTool {
			return fmt.Errorf("config: monitor.transitions_from %q must match monitor.collect tool (%q)", m.TransitionsFrom, collectTool)
		}
	}
	if len(m.WakeOn) > 0 && !hasTransitions {
		return fmt.Errorf("config: monitor.wake_on requires monitor.transitions_from to be set")
	}
	seenEdges := make(map[string]bool, len(m.WakeOn))
	for i, edge := range m.WakeOn {
		e := strings.TrimSpace(edge)
		if !slices.Contains(validWakeOnEdges, e) {
			return fmt.Errorf("config: monitor.wake_on[%d] %q must be one of \"opened\", \"remained\", \"closed\", \"re_opened\", \"degraded\"", i, edge)
		}
		if seenEdges[e] {
			return fmt.Errorf("config: monitor.wake_on[%d] duplicate edge %q", i, e)
		}
		seenEdges[e] = true
	}
	return nil
}

func validateMonitorNotify(n *MonitorNotifyConfig, hasTransitions bool) error {
	if strings.TrimSpace(n.Channel) == "" {
		return fmt.Errorf("config: monitor.notify.channel is required")
	}
	ep := strings.TrimSpace(n.Endpoint)
	if ep == "" {
		return fmt.Errorf("config: monitor.notify.endpoint is required")
	}
	u, err := url.Parse(ep)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("config: monitor.notify.endpoint %q must be an http or https URL", n.Endpoint)
	}
	if strings.TrimSpace(n.TokenEnv) == "" {
		return fmt.Errorf("config: monitor.notify.token_env is required")
	}
	switch n.Update {
	case "", MonitorNotifyUpdateTimeline, MonitorNotifyUpdateReplace:
	default:
		return fmt.Errorf("config: monitor.notify.update %q must be %q or %q", n.Update, MonitorNotifyUpdateTimeline, MonitorNotifyUpdateReplace)
	}
	if strings.TrimSpace(n.RemindAfter) != "" {
		if !hasTransitions {
			return fmt.Errorf("config: monitor.notify.remind_after requires monitor.transitions_from to be set")
		}
		d, err := time.ParseDuration(n.RemindAfter)
		if err != nil || d <= 0 {
			return fmt.Errorf("config: monitor.notify.remind_after %q must be > 0", n.RemindAfter)
		}
	}
	if strings.TrimSpace(n.DigestAfter) != "" {
		d, err := time.ParseDuration(n.DigestAfter)
		if err != nil || d <= 0 {
			return fmt.Errorf("config: monitor.notify.digest_after %q must be > 0", n.DigestAfter)
		}
	}
	return nil
}

func validateMonitorAck(ack *MonitorAckConfig, hasTransitions bool, policyByTool map[string]ToolPolicyConfig) error {
	if !hasTransitions {
		return fmt.Errorf("config: monitor.ack requires monitor.transitions_from to be set")
	}
	ackTool := strings.TrimSpace(ack.Tool)
	pol, ok := policyByTool[ackTool]
	if !ok {
		return fmt.Errorf("config: monitor.ack.tool %q is not declared in tools.policies / tool_catalog.tools (must be declared with mutating: true)", ackTool)
	}
	if pol.Mutating == nil || !*pol.Mutating {
		return fmt.Errorf("config: monitor.ack.tool %q must be declared with mutating: true in tools.policies / tool_catalog.tools", ackTool)
	}
	return nil
}

// CheckMonitorCollectSurface verifies that no tool in cfg.Monitor.SelfRunTools()
// (monitor.collect.tool or monitor.ack.tool) is reachable by any subagent whose
// EffectiveMode() != SubagentModeSingleTurn (#1406).
//
// Self-run monitor tools advance stateful cursors or record operator
// acknowledgements on the runtime's own behalf; allowing an LLM subagent to
// call them mid-turn would corrupt the diff cursor or spoof operator acks.
func CheckMonitorCollectSurface(cfg *Config, hasMCPServers bool) error {
	if cfg == nil || cfg.Monitor == nil {
		return nil
	}
	selfRun := cfg.Monitor.SelfRunTools()
	if len(selfRun) == 0 {
		return nil
	}
	for _, toolName := range selfRun {
		field := "monitor.collect"
		if cfg.Monitor.Ack != nil && strings.TrimSpace(cfg.Monitor.Ack.Tool) == toolName {
			field = "monitor.ack.tool"
		}
		for _, spec := range cfg.Subagents {
			if spec.EffectiveMode() == SubagentModeSingleTurn {
				continue
			}
			if subagentCanReachTool(spec, toolName, hasMCPServers) {
				return fmt.Errorf(
					"config: %s tool %q is also reachable by subagent %q (a self-run monitor tool runs on the runtime's own behalf; remove %q from subagent %q's tools or give the subagent an explicit mcp_tools allowlist that excludes it)",
					field, toolName, spec.Name, toolName, spec.Name,
				)
			}
		}
	}
	return nil
}

func subagentCanReachTool(spec SubagentSpec, toolName string, hasMCPServers bool) bool {
	for _, b := range spec.Tools {
		if strings.TrimSpace(b) == toolName {
			return true
		}
	}
	if spec.MCPTools != nil {
		if len(spec.MCPTools) == 0 {
			return false
		}
		for _, srv := range spec.MCPTools {
			if len(srv.Tools) == 0 {
				return true
			}
			for _, t := range srv.Tools {
				trimmed := strings.TrimSpace(t)
				if trimmed == toolName || srv.Server+"_"+trimmed == toolName {
					return true
				}
			}
		}
		return false
	}
	if spec.MCP != nil {
		return len(spec.MCP) > 0
	}
	return hasMCPServers
}
