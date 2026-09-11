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

package trajectory

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// WriteSteps renders one trajectory's step table.
//
// The table is the product for now. Before any measure exists, the
// question this package has to answer is whether the ordered steps show
// anything the drill's evidence sheets do not — and that is a question
// for a human reading rows, not for code deciding.
func WriteSteps(w io.Writer, t *Trajectory) error {
	b := &strings.Builder{}

	fmt.Fprintf(b, "run      %s\n", t.Meta.RunID)
	if t.Meta.ScenarioID != "" {
		fmt.Fprintf(b, "scenario %s  %s\n", t.Meta.ScenarioID, t.Meta.ScenarioName)
	}
	if t.Meta.ModelFlavor != "" || t.Meta.DaemonImage != "" {
		fmt.Fprintf(b, "model    %s   image %s\n", t.Meta.ModelFlavor, t.Meta.DaemonImage)
	}
	fmt.Fprintf(b, "usage    turns=%d in=%d out=%d cost=$%.4f\n",
		t.Usage.Turns, t.Usage.TokensIn, t.Usage.TokensOut, t.Usage.CostUSD)
	fmt.Fprintf(b, "frames   %d   steps %d\n\n", len(t.Frames), len(t.Steps))

	if len(t.Steps) == 0 {
		b.WriteString("  (no tool calls)\n")
		_, err := io.WriteString(w, b.String())
		return err
	}

	agentW := len("agent")
	toolW := len("tool")
	for _, s := range t.Steps {
		agentW = max(agentW, len(s.Agent))
		toolW = max(toolW, len(s.Tool))
	}

	fmt.Fprintf(b, "  %3s  %-*s  %9s  %8s  %-11s  %-*s  %s\n",
		"#", agentW, "agent", "seq", "elapsed", "status", toolW, "tool", "args")
	for _, s := range t.Steps {
		resp := "—"
		if s.RespSeq >= 0 {
			resp = fmt.Sprint(s.RespSeq)
		}
		el := "—"
		if d := s.Elapsed(); d > 0 {
			el = d.Round(time.Millisecond).String()
		}
		fmt.Fprintf(b, "  %3d  %-*s  %4d→%-4s  %8s  %-11s  %-*s  %s\n",
			s.Index, agentW, s.Agent, s.CallSeq, resp, el, s.Status,
			toolW, s.Tool, summarizeArgs(s.Args))
	}

	b.WriteString("\n")
	writeToolTally(b, t)
	return writeString(w, b.String())
}

// writeToolTally prints call counts per (agent, tool) with their
// non-ok results, which is the cheapest way to see a loop.
func writeToolTally(b *strings.Builder, t *Trajectory) {
	type key struct{ agent, tool string }
	counts := map[key]int{}
	bad := map[key]int{}
	for _, s := range t.Steps {
		k := key{s.Agent, s.Tool}
		counts[k]++
		if s.Status != StatusOK {
			bad[k]++
		}
	}
	keys := make([]key, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if counts[keys[i]] != counts[keys[j]] {
			return counts[keys[i]] > counts[keys[j]]
		}
		if keys[i].agent != keys[j].agent {
			return keys[i].agent < keys[j].agent
		}
		return keys[i].tool < keys[j].tool
	})
	b.WriteString("  tally\n")
	for _, k := range keys {
		note := ""
		if bad[k] > 0 {
			note = fmt.Sprintf("   (%d not ok)", bad[k])
		}
		fmt.Fprintf(b, "    %2d  %s/%s%s\n", counts[k], k.agent, k.tool, note)
	}
}

// summarizeArgs renders arguments on one line, longest-first truncated,
// so the column stays readable on a run whose tool takes a whole YAML
// document. The full value is in --json.
func summarizeArgs(args map[string]any) string {
	if len(args) == 0 {
		return ""
	}
	names := make([]string, 0, len(args))
	for k := range args {
		names = append(names, k)
	}
	sort.Strings(names)

	parts := make([]string, 0, len(names))
	for _, n := range names {
		parts = append(parts, n+"="+truncate(scalar(args[n]), 48))
	}
	return truncate(strings.Join(parts, " "), 120)
}

func scalar(v any) string {
	switch t := v.(type) {
	case string:
		return strings.Join(strings.Fields(t), " ")
	case nil:
		return "null"
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return fmt.Sprint(t)
		}
		return string(b)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return "…"
	}
	return s[:n-1] + "…"
}

func writeString(w io.Writer, s string) error {
	_, err := io.WriteString(w, s)
	return err
}
