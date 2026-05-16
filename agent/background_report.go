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

package agent

import (
	"time"

	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
)

// reportArgs is the JSON shape the spawned subagent's model sees when
// it calls report_alert or report_completed. A single message string.
type reportArgs struct {
	Text string `json:"text" jsonschema:"a one-paragraph message describing the alert or completion"`
}

type reportResult struct {
	OK bool `json:"ok"`
}

// newReportAlertTool builds a per-subagent report_alert tool. The
// from argument is baked in so the manager's Alert.From identifies
// which subagent reported, without the subagent's model having to
// remember to include its own name in every call.
//
// Each report_alert call pushes an Alert onto the manager's channel
// (drop-oldest backpressure if full). The parent's run loop drains
// the channel before its next turn and prepends formatted alert
// lines to the prompt the model sees.
func newReportAlertTool(mgr *BackgroundAgentManager, from string) tool.Tool {
	t, err := functiontool.New(functiontool.Config{
		Name:        "report_alert",
		Description: "Send an alert back to the parent agent. The text becomes a user-visible report the parent agent reads before its next turn. Use for noteworthy findings, status updates, or things the parent should react to.",
	}, func(_ tool.Context, args reportArgs) (reportResult, error) {
		mgr.pushAlert(Alert{
			From:      from,
			Text:      args.Text,
			Kind:      "alert",
			Timestamp: time.Now(),
		})
		return reportResult{OK: true}, nil
	})
	if err != nil {
		// functiontool.New only fails on programmer errors (bad
		// signature) which the literal call above can't hit.
		panic("agent: newReportAlertTool: " + err.Error())
	}
	return t
}

// newReportCompletedTool builds a per-subagent report_completed tool.
// Mirrors report_alert but is used by the subagent to declare it has
// finished its goal. Calling this is functionally equivalent to the
// autonomous driver's report_done tool, except the message is also
// surfaced as a "completed" Alert to the parent — the driver's
// terminal-state Alert is fired by the goroutine wrapper in Spawn
// when RunAutonomous returns, so calling report_completed is the
// model's "let the parent know what I did" signal, not a hard
// termination call (use report_done for that).
func newReportCompletedTool(mgr *BackgroundAgentManager, from string) tool.Tool {
	t, err := functiontool.New(functiontool.Config{
		Name:        "report_completed",
		Description: "Tell the parent agent that you've finished your goal. The text becomes a user-visible completion report. Call report_done separately to actually terminate the autonomous loop.",
	}, func(_ tool.Context, args reportArgs) (reportResult, error) {
		mgr.pushAlert(Alert{
			From:      from,
			Text:      args.Text,
			Kind:      "completed",
			Timestamp: time.Now(),
		})
		return reportResult{OK: true}, nil
	})
	if err != nil {
		panic("agent: newReportCompletedTool: " + err.Error())
	}
	return t
}

// PrependPendingAlerts drains every pending alert from the manager's
// channel (non-blocking) and, when non-empty, returns prompt with a
// "[Background reports]" header prepended. Empty channel returns
// prompt unchanged.
//
// Called by Agent.Run before each turn so the parent's model sees
// what its subagents have reported since the last turn.
func (m *BackgroundAgentManager) PrependPendingAlerts(prompt string) string {
	var pending []Alert
drain:
	for {
		select {
		case a := <-m.alerts:
			pending = append(pending, a)
		default:
			break drain
		}
	}
	if len(pending) == 0 {
		return prompt
	}
	return formatAlertsForPrompt(pending) + "\n\n---\n\n" + prompt
}

// formatAlertsForPrompt renders a slice of Alerts as a header block
// suitable for prepending to the next user turn's prompt. Format is
// stable + greppable so consumer tooling can find the boundary.
func formatAlertsForPrompt(alerts []Alert) string {
	var b []byte
	b = append(b, "[Background reports]\n"...)
	for _, a := range alerts {
		b = append(b, "- ["...)
		b = append(b, a.From...)
		if a.Kind != "" && a.Kind != "alert" {
			b = append(b, "] ("...)
			b = append(b, a.Kind...)
			b = append(b, ") "...)
		} else {
			b = append(b, "] "...)
		}
		b = append(b, a.Text...)
		b = append(b, '\n')
	}
	// Trim trailing newline to keep the separator clean.
	if len(b) > 0 && b[len(b)-1] == '\n' {
		b = b[:len(b)-1]
	}
	return string(b)
}
