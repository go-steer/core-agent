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

package judge

import (
	"fmt"
	"sort"
	"strings"

	"github.com/go-steer/core-agent/v3/internal/evals"
)

// Cluster is the fixture cluster behind one scenario's run: which
// lookout tool returns what.
//
// # Which tools answer
//
// A tool answers when the intents it satisfies overlap the intents the
// scenario expects. Everything else returns an honest empty reading,
// which is what a real cluster does — querying storage during a DNS
// incident finds nothing wrong with storage.
//
// That makes tool choice the thing under measurement, which is what
// intent_coverage scores. It also has a limitation worth stating
// plainly: a *different but legitimate* read path returns empty and
// looks to the agent like a dead end, because the corpus's
// expected_tools list is the only evidence anything has about what this
// cluster contains. The judge tier inherits that ceiling from the
// corpus; it is the same assumption upstream's own tool_coverage makes,
// and it is why intent_coverage — which at least credits consolidation
// — is the primary metric rather than tool_coverage.
//
// # What an answering tool returns
//
// Its natural half of the fixture: log and event tools return
// [Observations.Messages], spec and topology tools return
// [Observations.Fields], and the three consolidators return both. A
// scenario whose observations cannot reach the agent through any
// answering tool is a build error, not a low score — see
// [NewCluster].
type Cluster struct {
	scenario string
	obs      Observations
	// answers maps tool name to the payload halves it serves. A tool
	// absent from the map returns the empty reading.
	answers map[string]payload
}

type payload struct{ messages, fields bool }

func (p payload) empty() bool { return !p.messages && !p.fields }

// toolShape is what each lookout tool reads, independent of scenario.
// The three consolidators return both halves; the rest are single-sided
// because a log tool that also returned specs would make tool choice
// cosmetic.
//
// Names and coverage come from testdata/evals/intents.yaml's
// lookout_tools block, which is checked against k8s-lookout's MCPName
// registry. [NewCluster] fails on any drift between the two.
var toolShape = map[string]payload{
	"k8s_triage_workload":  {messages: true, fields: true},
	"k8s_cluster_health":   {messages: true, fields: true},
	"k8s_triage_delta":     {messages: true, fields: true},
	"k8s_triage_logs":      {messages: true},
	"k8s_event_timeline":   {messages: true},
	"k8s_resource_spec":    {fields: true},
	"k8s_state_edges":      {fields: true},
	"k8s_resource_top":     {fields: true},
	"k8s_volume_conflicts": {fields: true},
	"k8s_recent_changes":   {fields: true},
	"k8s_cloud_quota":      {fields: true},
}

// NewCluster builds the fixture cluster for one scenario.
//
// It fails rather than degrades in two cases, both of which would
// otherwise show up as a low score the reader would attribute to mast:
//
//   - The scenario has observations no answering tool can return — say
//     log lines when every expected intent maps to a spec-shaped tool.
//     The agent could then read perfectly and still never see the
//     evidence.
//   - The intent table names a lookout tool this file has no shape for.
//     Silently treating it as non-answering would shrink the read path
//     whenever lookout grows a tool.
func NewCluster(tbl evals.IntentTable, sc evals.Scenario, obs Observations) (*Cluster, error) {
	for name := range tbl.LookoutTools {
		if _, ok := toolShape[name]; !ok {
			return nil, fmt.Errorf(
				"judge: intent table declares lookout tool %q with no shape in cluster.go — add one rather than letting the fixture quietly stop serving it", name)
		}
	}

	want, _ := tbl.IntentsFor(sc.Outputs.ExpectedTools)
	wanted := make(map[string]bool, len(want))
	for _, id := range want {
		wanted[id] = true
	}

	answers := make(map[string]payload)
	var servesMessages, servesFields bool
	for name, lt := range tbl.LookoutTools {
		overlap := false
		for _, id := range lt.Satisfies {
			if wanted[id] {
				overlap = true
				break
			}
		}
		if !overlap {
			continue
		}
		shape := toolShape[name]
		answers[name] = shape
		servesMessages = servesMessages || shape.messages
		servesFields = servesFields || shape.fields
	}

	if len(obs.Messages) > 0 && !servesMessages {
		return nil, fmt.Errorf(
			"judge: %s has %d message observation(s) but no expected intent maps to a message-bearing tool — the agent cannot reach the evidence, so its score would measure the fixture",
			sc.ID, len(obs.Messages))
	}
	if len(obs.Fields) > 0 && !servesFields {
		return nil, fmt.Errorf(
			"judge: %s has %d field observation(s) but no expected intent maps to a spec-bearing tool — the agent cannot reach the evidence, so its score would measure the fixture",
			sc.ID, len(obs.Fields))
	}

	return &Cluster{scenario: sc.ID, obs: obs, answers: answers}, nil
}

// AnsweringTools lists the tools that return data for this scenario, in
// stable order. Used by the report to say how wide the read path was.
func (c *Cluster) AnsweringTools() []string {
	out := make([]string, 0, len(c.answers))
	for name := range c.answers {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Read returns what the named tool sees.
//
// An unknown or non-answering tool gets the empty reading rather than
// an error: a tool call that finds nothing is a normal event in triage,
// and erroring would tell the agent it had guessed wrong, which is
// information a real cluster does not volunteer.
func (c *Cluster) Read(tool, scope string) string {
	reading, _ := c.ReadResult(tool, scope)
	return reading
}

// ReadResult is Read plus whether the reading carried any observation.
//
// The agent is given only the prose, exactly as before — the fixture's
// no-findings reading is deliberately indistinguishable from a real
// cluster's silence, and making it self-describing would change what
// the model reads and move every score on the board. The second return
// is for the recorder: an empty read is the difference between "never
// called the tool" and "called it against a scope that matched
// nothing", and #169 exists because the board could not tell those
// apart.
func (c *Cluster) ReadResult(tool, scope string) (string, bool) {
	p, ok := c.answers[tool]
	if !ok || p.empty() {
		return c.nothing(tool, scope), false
	}

	msgs := p.messages && len(c.obs.Messages) > 0
	fields := p.fields && len(c.obs.Fields) > 0
	if !msgs && !fields {
		// This tool answers this scenario, but its half of the fixture is
		// empty — a log tool on a row whose observations are all
		// identifiers. Returning the header and the echoed alert and
		// nothing else reads to the agent as a broken tool rather than a
		// clean reading, and the first live board showed exactly that:
		// four rows where the model reported "the tools are returning
		// only the subject echo" and declined to diagnose. An empty half
		// is the same fact as a non-answering tool, so it says the same
		// thing.
		return c.nothing(tool, scope), false
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s: reading %s\n", tool, describeScope(scope))
	fmt.Fprintf(&b, "subject: %s\n", c.obs.Subject)
	if msgs {
		b.WriteString("\nlog lines, events and controller messages:\n")
		for _, m := range c.obs.Messages {
			fmt.Fprintf(&b, "  - %s\n", m)
		}
	}
	if fields {
		b.WriteString("\nresource fields and identifiers:\n")
		for _, f := range c.obs.Fields {
			fmt.Fprintf(&b, "  - %s\n", f)
		}
	}
	return b.String(), true
}

func (c *Cluster) nothing(tool, scope string) string {
	return fmt.Sprintf("%s: reading %s\nno abnormal findings in this scope.\n", tool, describeScope(scope))
}

func describeScope(scope string) string {
	scope = strings.TrimSpace(scope)
	if scope == "" {
		return "the whole cluster"
	}
	return scope
}
