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
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"google.golang.org/adk/session"
)

// Parent is the agent name given to frames from the parent session, so
// that a trajectory's Agent column is total and nothing has to test for
// the empty string. Subagents carry their own registered name.
const Parent = "parent"

// Frame is one recorded event, tagged with which agent produced it.
//
// Seq is a total order across parent AND subagents: they write through
// one event log, so interleaving by Seq is what makes "which turn was
// the parent in when the child read that" an answerable question.
type Frame struct {
	Agent string         `json:"agent"`
	Seq   int            `json:"seq"`
	Event *session.Event `json:"event"`

	// rawParts holds each part's original JSON, index-aligned with
	// Event.Content.Parts. Kept because decoding into a map loses the
	// key ORDER, and score.py's 200-character error-prose window is
	// sensitive to it — see pyDumps.
	rawParts []json.RawMessage
}

// Text returns the frame's concatenated text parts. Empty for a frame
// that only carried tool traffic.
func (f Frame) Text() string {
	if f.Event == nil || f.Event.Content == nil {
		return ""
	}
	var b strings.Builder
	for _, p := range f.Event.Content.Parts {
		if p != nil && p.Text != "" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// Role is the content role ("model", "user", ...), or "" if absent.
func (f Frame) Role() string {
	if f.Event == nil || f.Event.Content == nil {
		return ""
	}
	return f.Event.Content.Role
}

// Partial reports whether this is a streaming chunk rather than a
// settled frame. Measures should skip partials: the same text arrives
// again on the final frame, and counting both double-counts everything.
func (f Frame) Partial() bool { return f.Event != nil && f.Event.Partial }

// Meta is the subset of a drill run's meta.json this package reads.
// Unknown fields are ignored on purpose — meta.json is the drill's file
// and grows on its schedule, not ours.
type Meta struct {
	RunID        string `json:"run_id"`
	StartedAt    string `json:"started_at"`
	ScenarioID   string `json:"scenario_id"`
	ScenarioName string `json:"scenario_name"`
	Cluster      string `json:"cluster"`
	ModelFlavor  string `json:"model_flavor"`
	DaemonImage  string `json:"daemon_image"`
	SessionID    string `json:"session_id"`
	Followup     string `json:"followup"`
}

// Turn is one completed model turn, from the drill's typed
// `turn-complete` frames. Carried separately from Frames because it is
// attach-protocol telemetry rather than an agent event.
type Turn struct {
	PromptID  string `json:"prompt_id"`
	Model     string `json:"model"`
	TokensIn  int    `json:"tokens_in"`
	TokensOut int    `json:"tokens_out"`
	LatencyMS int    `json:"latency_ms"`
}

// Usage is the run's final cumulative spend, from the last
// `usage-update` frame.
type Usage struct {
	TokensIn  int     `json:"tokens_in_total"`
	TokensOut int     `json:"tokens_out_total"`
	CostUSD   float64 `json:"cost_usd_total"`
	Turns     int     `json:"turns_total"`
}

// Trajectory is one run: its identity, its ordered steps, and the raw
// frames the steps were derived from.
//
// Frames is kept because a measure about what the agent SAID needs the
// text, and reconstructing it from Steps is impossible — Steps are tool
// traffic only.
type Trajectory struct {
	Meta   Meta    `json:"meta"`
	Frames []Frame `json:"frames"`
	Steps  []Step  `json:"steps"`
	Turns  []Turn  `json:"turns"`
	Usage  Usage   `json:"usage"`
}

// sseRecord is one line of the drill's transcript.jsonl.
type sseRecord struct {
	SSE  string `json:"sse"`
	Data struct {
		Seq   int             `json:"seq"`
		Event json.RawMessage `json:"event"`
	} `json:"data"`
	// Raw is the whole `data` object, kept for the typed frames
	// (turn-complete, usage-update) that carry no Event at all.
	Raw json.RawMessage `json:"-"`
}

// LoadRun reads one drill run directory — the layout dev/uat/gke-drill
// writes under ~/.gke-drill/runs/<id>/.
//
// It merges transcript.jsonl (the parent) with subagents.json (one
// entry per child) and sorts by (Seq, Agent), which is the same merge
// score.py does. Missing subagents.json is not an error: a run with no
// delegation is a legitimate run.
func LoadRun(dir string) (*Trajectory, error) {
	t := &Trajectory{}

	if err := readJSONFile(filepath.Join(dir, "meta.json"), &t.Meta); err != nil {
		return nil, err
	}
	if t.Meta.RunID == "" {
		t.Meta.RunID = filepath.Base(dir)
	}

	recs, err := readTranscript(filepath.Join(dir, "transcript.jsonl"))
	if err != nil {
		return nil, err
	}
	for _, r := range recs {
		switch r.SSE {
		case "agent":
			fr, err := decodeFrame(Parent, r.Data.Seq, r.Data.Event)
			if err != nil {
				return nil, fmt.Errorf("%s: seq %d: %w", dir, r.Data.Seq, err)
			}
			t.Frames = append(t.Frames, fr)
		case "turn-complete":
			var turn Turn
			if err := json.Unmarshal(r.Raw, &turn); err == nil {
				t.Turns = append(t.Turns, turn)
			}
		case "usage-update":
			// Cumulative — the last one wins, so no accumulation here.
			var u Usage
			if err := json.Unmarshal(r.Raw, &u); err == nil {
				t.Usage = u
			}
		}
	}

	subs, err := readSubagents(filepath.Join(dir, "subagents.json"))
	if err != nil {
		return nil, err
	}
	for name, entries := range subs {
		for _, e := range entries {
			if len(e.Event) == 0 {
				continue
			}
			fr, err := decodeFrame(name, e.Seq, e.Event)
			if err != nil {
				return nil, fmt.Errorf("%s: subagent %s seq %d: %w", dir, name, e.Seq, err)
			}
			t.Frames = append(t.Frames, fr)
		}
	}

	sort.SliceStable(t.Frames, func(i, j int) bool {
		if t.Frames[i].Seq != t.Frames[j].Seq {
			return t.Frames[i].Seq < t.Frames[j].Seq
		}
		return t.Frames[i].Agent < t.Frames[j].Agent
	})

	t.Steps = buildSteps(t.Frames)
	return t, nil
}

// LoadRuns reads every immediate subdirectory of root that looks like a
// run (it has a transcript.jsonl), oldest first. A directory that fails
// to load is returned as an error rather than skipped — a run we cannot
// read is a finding about the recorder, not a run to drop quietly.
func LoadRuns(root string) ([]*Trajectory, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var out []*Trajectory
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		if _, err := os.Stat(filepath.Join(dir, "transcript.jsonl")); err != nil {
			continue
		}
		t, err := LoadRun(dir)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Meta.RunID < out[j].Meta.RunID })
	return out, nil
}

func decodeFrame(agent string, seq int, raw json.RawMessage) (Frame, error) {
	var ev session.Event
	if err := json.Unmarshal(raw, &ev); err != nil {
		return Frame{}, err
	}
	var shadow struct {
		Content struct {
			Parts []json.RawMessage `json:"parts"`
		} `json:"content"`
	}
	// Best effort: a frame whose parts we cannot re-read still decodes,
	// it just falls back to Go's own encoding for the prose scan.
	_ = json.Unmarshal(raw, &shadow)
	return Frame{Agent: agent, Seq: seq, Event: &ev, rawParts: shadow.Content.Parts}, nil
}

// rawResponse returns the original JSON of part i's functionResponse
// payload, or nil if it is unavailable.
func (f Frame) rawResponse(i int) json.RawMessage {
	if i < 0 || i >= len(f.rawParts) {
		return nil
	}
	var part struct {
		FunctionResponse *struct {
			Response json.RawMessage `json:"response"`
		} `json:"functionResponse"`
	}
	if err := json.Unmarshal(f.rawParts[i], &part); err != nil || part.FunctionResponse == nil {
		return nil
	}
	return part.FunctionResponse.Response
}

// maxLine bounds one transcript line. A tool result carrying a whole
// Deployment YAML runs to tens of kilobytes, and bufio.Scanner's 64 KiB
// default truncates it into a parse error that reads like corruption.
const maxLine = 16 << 20

func readTranscript(path string) ([]sseRecord, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), maxLine)

	var out []sseRecord
	line := 0
	for sc.Scan() {
		line++
		b := sc.Bytes()
		if len(strings.TrimSpace(string(b))) == 0 {
			continue
		}
		var r sseRecord
		if err := json.Unmarshal(b, &r); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, line, err)
		}
		// Re-extract `data` verbatim for the typed frames, which have
		// no Event and whose payload IS the data object.
		var envelope struct {
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(b, &envelope); err == nil {
			r.Raw = envelope.Data
		}
		out = append(out, r)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return out, nil
}

type subEntry struct {
	Seq   int             `json:"seq"`
	Event json.RawMessage `json:"event"`
}

func readSubagents(path string) (map[string][]subEntry, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var subs map[string][]subEntry
	if err := json.Unmarshal(b, &subs); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return subs, nil
}

func readJSONFile(path string, v any) error {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}
