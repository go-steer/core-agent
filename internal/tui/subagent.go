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

package tui

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/go-steer/core-agent/agent"
)

// defaultSubagentPrompt is the system prompt grafted onto a /subagent
// spawn when the operator didn't supply --prompt. Generic enough to
// work for one-shot triage tasks but explicit about the two
// completion hooks the runtime always wires (report_completed /
// report_alert) so the model knows how to report back.
const defaultSubagentPrompt = "You are a focused background subagent spawned directly by the operator. Complete the supplied goal autonomously and report back via the report_completed tool when you are done. Use report_alert to surface anything the operator should see before completion (e.g. partial findings, errors that block progress). Stay narrowly scoped to the goal — do not invent additional work."

// handleSubagentCommand parses /subagent's flag-style arg string and
// calls BackgroundAgentManager.Spawn directly, bypassing the main
// agent's reasoning. See docs/operator-input-design.md layer D.
//
// Syntax (everything after recognized flags is the goal):
//
//	/subagent <goal>
//	/subagent --name=research <goal>
//	/subagent --name=reviewer --tools=read_file,grep <goal>
//	/subagent --prompt="You are a code reviewer." review pending.diff
//	/subagent --max-turns=20 --max-wallclock=10m <goal>
//
// Empty goal or empty arg string prints a usage hint. Spawn errors
// (depth/concurrency cap, duplicate name, unknown tool) surface as
// error messages in the chat; success surfaces as a system message
// naming the subagent + branch.
func (m *Model) handleSubagentCommand(args string) (tea.Model, tea.Cmd) {
	if strings.TrimSpace(args) == "" {
		m.history.Append(Message{Role: RoleSystem, Text: subagentUsage()})
		m.refreshViewport()
		return m, nil
	}
	// Parse before touching the manager so syntax errors surface
	// even when /subagent is run on an agent built without
	// background-agent support — the operator gets fast feedback
	// on typos either way.
	spec, parseErr := parseSubagentArgs(args)
	if parseErr != nil {
		m.history.Append(Message{Role: RoleError, Text: "/subagent: " + parseErr.Error()})
		m.refreshViewport()
		return m, nil
	}
	if m.agent == nil {
		m.history.Append(Message{Role: RoleError, Text: "/subagent unavailable: no agent constructed."})
		m.refreshViewport()
		return m, nil
	}
	mgr := m.agent.BackgroundManager()
	if mgr == nil {
		m.history.Append(Message{Role: RoleError, Text: "/subagent unavailable: this agent was constructed without a BackgroundAgentManager. Pass --background-agents (or its config equivalent) when launching core-agent."})
		m.refreshViewport()
		return m, nil
	}

	// Auto-generate a name when the operator didn't supply one.
	// Operators usually don't care about the name — it shows up in
	// alerts as the From: field — but BackgroundSpec requires it.
	if spec.Name == "" {
		spec.Name = fmt.Sprintf("op-%d", time.Now().Unix())
	}
	if spec.SystemPrompt == "" {
		spec.SystemPrompt = defaultSubagentPrompt
	}

	handle, err := mgr.Spawn(context.Background(), "", spec)
	if err != nil {
		m.history.Append(Message{Role: RoleError, Text: "/subagent: spawn failed: " + err.Error()})
		m.refreshViewport()
		return m, nil
	}
	m.history.Append(Message{Role: RoleSystem, Text: fmt.Sprintf(
		"Spawned subagent %q (branch: %s, status: %s). You'll see its updates as alerts in the chat when it calls report_alert or completes.",
		handle.Name, handle.Branch, handle.Status())})
	m.refreshViewport()
	return m, nil
}

// parseSubagentArgs splits args into a BackgroundSpec and a goal.
// Recognized flags (all optional): --name, --prompt (alias
// --system-prompt), --tools, --extras (alias --skill), --max-turns,
// --max-cost, --max-wallclock, --scheduler. Both `--flag=value` and
// `--flag value` forms are supported. Everything after the last
// recognized flag (or the entire arg string when no flags are
// present) is treated as the goal.
//
// Returns an error for empty goal, malformed numeric values, or an
// unknown flag (so a typo doesn't silently consume the next token
// as its value).
func parseSubagentArgs(args string) (agent.BackgroundSpec, error) {
	var spec agent.BackgroundSpec
	tokens := tokenizeSubagentArgs(args)
	i := 0
	goalStart := 0
	for i < len(tokens) {
		t := tokens[i]
		if !strings.HasPrefix(t, "--") {
			// First non-flag token starts the goal.
			goalStart = i
			break
		}
		// Strip "--" and split on "=" if present.
		body := strings.TrimPrefix(t, "--")
		var key, val string
		if eq := strings.Index(body, "="); eq >= 0 {
			key = body[:eq]
			val = body[eq+1:]
			i++
		} else {
			key = body
			i++
			if i >= len(tokens) {
				return spec, fmt.Errorf("flag --%s requires a value", key)
			}
			val = tokens[i]
			i++
		}
		val = strings.Trim(val, `"'`)
		if err := applySubagentFlag(&spec, key, val); err != nil {
			return spec, err
		}
	}
	if goalStart == 0 && i > 0 {
		// All tokens consumed by flags; nothing left for the goal.
		return spec, fmt.Errorf("no goal supplied (everything was consumed by flags)")
	}
	goal := strings.TrimSpace(strings.Join(tokens[goalStart:], " "))
	if goal == "" {
		return spec, fmt.Errorf("no goal supplied (use /subagent <goal> or pass it after the flags)")
	}
	spec.Goal = goal
	return spec, nil
}

// applySubagentFlag mutates spec based on one --key=val pair. Returns
// an error for unknown flags or unparseable numeric values.
func applySubagentFlag(spec *agent.BackgroundSpec, key, val string) error {
	switch key {
	case "name":
		spec.Name = val
	case "prompt", "system-prompt":
		spec.SystemPrompt = val
	case "tools":
		spec.Tools = splitCSV(val)
	case "extras", "skill", "skills":
		spec.Extras = append(spec.Extras, splitCSV(val)...)
	case "max-turns":
		n, err := strconv.Atoi(val)
		if err != nil || n < 0 {
			return fmt.Errorf("--max-turns must be a non-negative integer: %q", val)
		}
		spec.Budgets.MaxTurns = n
	case "max-cost":
		f, err := strconv.ParseFloat(val, 64)
		if err != nil || f < 0 {
			return fmt.Errorf("--max-cost must be a non-negative number (USD): %q", val)
		}
		spec.Budgets.MaxCost = f
	case "max-wallclock":
		d, err := time.ParseDuration(val)
		if err != nil || d < 0 {
			return fmt.Errorf("--max-wallclock must be a Go duration (e.g. 10m, 1h): %q", val)
		}
		spec.Budgets.MaxWallclock = d
	case "scheduler":
		switch val {
		case "default", "sleep", "exit_on_defer", "none":
			spec.Scheduler = val
		default:
			return fmt.Errorf("--scheduler must be one of default|sleep|exit_on_defer|none: %q", val)
		}
	default:
		return fmt.Errorf("unknown flag --%s (try --name, --prompt, --tools, --extras, --max-turns, --max-cost, --max-wallclock, --scheduler)", key)
	}
	return nil
}

// splitCSV parses a comma-separated value list, trimming whitespace
// and dropping empty tokens.
func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// tokenizeSubagentArgs splits args on whitespace, honoring quoted
// runs (both single and double quotes) so multi-word flag values
// like --prompt="You are X." stay intact. Quotes are stripped by
// the caller.
//
// Not a full shell parser — backslash escapes are not honored. This
// is enough for typical operator slash-command usage; anything more
// elaborate belongs in a real shell.
func tokenizeSubagentArgs(args string) []string {
	var out []string
	var cur strings.Builder
	inQuote := byte(0)
	for i := 0; i < len(args); i++ {
		c := args[i]
		switch {
		case inQuote != 0:
			if c == inQuote {
				inQuote = 0
				continue
			}
			cur.WriteByte(c)
		case c == '"' || c == '\'':
			inQuote = c
		case c == ' ' || c == '\t':
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteByte(c)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// subagentUsage is the help blurb printed for bare /subagent.
func subagentUsage() string {
	return strings.Join([]string{
		"Usage:",
		"  /subagent <goal>                       spawn a subagent with autogenerated name + default prompt",
		"  /subagent --name=research <goal>       give the subagent an explicit name",
		"  /subagent --prompt=\"<system>\" <goal>   override the default system prompt",
		"  /subagent --tools=read_file,grep <goal>   restrict to specific built-in tools",
		"  /subagent --extras=kubectl_get <goal>  add MCP/skill tools beyond the built-ins",
		"  /subagent --max-turns=20 --max-wallclock=10m <goal>   set per-subagent budgets",
		"  /subagent --scheduler=sleep <goal>     long-lived daemon shape (default | sleep | exit_on_defer | none)",
		"",
		"Use the /subagents slash to list running ones; the subagent's report_alert + completion land as alerts in the chat.",
	}, "\n")
}
