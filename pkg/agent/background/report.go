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
	"fmt"
	"time"

	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
)

// reportArgs is the JSON shape the spawned subagent's model sees when
// it calls report_alert. A single message string.
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
func newReportAlertTool(mgr *Manager, from string) tool.Tool {
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
		panic("background: newReportAlertTool: " + err.Error())
	}
	return t
}

// report_completed used to be built here as a tool of its own. It
// pushed a "completed" Alert to the parent and returned ok WITHOUT
// ending the run — its own description told the model to "call
// report_done separately to actually terminate the autonomous loop".
// Splitting a delegation's return across two tool calls meant a model
// that announced it was finished was then handed "continue" and ran on
// past its own answer (#728). The name is now an alias of the driver's
// return tool (subagentReturnToolAliases), so calling it returns. The
// parent still receives a "completed" alert — the terminal alert fired
// by launch's goroutine wrapper covers it.

// PrependPendingAlerts drains every pending alert from the manager's
// channel (non-blocking) and, when non-empty, returns prompt with a
// "[Background reports]" header prepended. Nothing pending and nothing
// dropped returns prompt unchanged.
//
// Called by Agent.Run before each turn so the parent's model sees
// what its subagents have reported since the last turn.
//
// Alerts the buffer had to evict are reported too, as a synthetic
// leading entry. They are the OLDEST reports, so they lead the block
// for the same reason the rest of it is in arrival order. Before #780
// an eviction only reached the daemon's stderr, which left the model
// reading a report list it had no way to know was truncated — the one
// reader that could have asked a subagent to say it again.
func (m *Manager) PrependPendingAlerts(prompt string) string {
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
	// Read the counter AFTER the drain: an eviction concurrent with it
	// is better reported one turn late than not at all.
	if dropped := m.takeDroppedAlerts(); dropped > 0 {
		pending = append([]Alert{droppedAlert(dropped)}, pending...)
	}
	if len(pending) == 0 {
		return prompt
	}
	return formatAlertsForPrompt(pending) + "\n\n---\n\n" + prompt
}

// droppedAlert renders n evicted alerts as the synthetic entry that
// leads the report block. From is the manager rather than a subagent
// name because no subagent is at fault; the text says what was lost
// and what the model can do about it, since "some reports are missing"
// is only actionable if it also says who to ask.
func droppedAlert(n int) Alert {
	noun := "reports were"
	if n == 1 {
		noun = "report was"
	}
	return Alert{
		From: "background-manager",
		Kind: "dropped",
		Text: fmt.Sprintf("%d earlier background %s discarded: the report queue filled up before this turn drained it, "+
			"and the oldest entries were evicted to make room. They are unrecoverable. "+
			"If you are waiting on a subagent whose report is not below, check its status "+
			"(list_agents) or ask it again rather than assuming it has not finished.", n, noun),
		Timestamp: time.Now(),
	}
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
