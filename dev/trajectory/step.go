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
	"strings"
	"time"
)

// Status is what a tool call's result carried.
//
// The three values, and the asymmetry between them, are score.py's
// (dev/uat/gke-drill/score.py, response_status) and must stay that way.
// Two instruments reading the same transcript and disagreeing about
// which calls failed is the #694 failure — two surfaces disagreeing
// about one rule — and it would be worse here, because the disagreement
// would show up as a trajectory finding contradicting a scored sheet.
type Status string

const (
	// StatusOK is a result with no structural error signal.
	StatusOK Status = "ok"
	// StatusError is a structural failure: isError, an error key, or a
	// status field reading error/failure/failed.
	StatusError Status = "error"
	// StatusSuspect is prose that opens like an error. Only ever a
	// suspicion: a read that SUCCEEDED and returned text about a denial
	// is the normal case in scenario C, not a failure. A false suspect
	// costs one glance at the payload; a false ok would let a turn that
	// read nothing pass as grounded.
	StatusSuspect Status = "suspect"
	// StatusNoResponse is a call that never got a result frame at all.
	StatusNoResponse Status = "no-response"
)

// Step is one tool call with its result joined on, in trajectory order.
//
// This is the unit every measure works in. If a measure needs a field
// that is not here, add it here rather than reaching back into Frames —
// a measure that parses raw events is a measure that will disagree with
// the next one.
type Step struct {
	// Index is position within the trajectory, 0-based, across all
	// agents.
	Index int `json:"index"`
	// Agent is [Parent] or a subagent's registered name.
	Agent string `json:"agent"`
	// CallSeq and RespSeq are the frame sequence numbers. RespSeq is -1
	// when nothing answered.
	CallSeq int `json:"call_seq"`
	RespSeq int `json:"resp_seq"`
	// Invocation is the calling frame's InvocationID — the turn this
	// step belongs to. Steps sharing one Invocation happened inside one
	// model turn.
	Invocation string `json:"invocation"`
	// CallID is the provider's function-call id, the key the response
	// is matched on.
	CallID string `json:"call_id"`

	// At is the calling frame's timestamp and Done the responding
	// frame's; Done is zero when nothing answered. Kept because "where
	// did the wall clock go" is a trajectory question and the six boxes
	// cannot ask it.
	At   time.Time `json:"at"`
	Done time.Time `json:"done"`

	Tool     string         `json:"tool"`
	Args     map[string]any `json:"args"`
	Status   Status         `json:"status"`
	Response map[string]any `json:"response"`
}

// Elapsed is how long the call took, or 0 if it never returned.
//
// Read it as a bound, not a measurement. A model turn routinely issues
// several calls in ONE frame and the runtime answers them in one frame
// too, so every call in such a batch reports the whole batch's span:
// steps 3-6 of run 20260910T170709Z-a all say 496ms because they share
// frames 156 and 161. What the number is good for is spotting the step
// that ate the wall clock, which in practice is a spawn_agent.
func (s Step) Elapsed() time.Duration {
	if s.At.IsZero() || s.Done.IsZero() {
		return 0
	}
	return s.Done.Sub(s.At)
}

// Arg returns a string-valued argument, or "".
func (s Step) Arg(name string) string {
	v, ok := s.Args[name]
	if !ok {
		return ""
	}
	str, ok := v.(string)
	if !ok {
		return ""
	}
	return str
}

// Failed reports whether the step's result was a structural failure.
// Suspect does not count: see [StatusSuspect].
func (s Step) Failed() bool { return s.Status == StatusError }

// buildSteps walks frames in order, emitting one Step per function call
// and joining the response that carries the same call id.
//
// Responses are matched by id rather than by adjacency because a model
// turn can issue several calls in one frame and their results can come
// back in any order. Where a provider left the id empty the fallback is
// the first unclaimed response with the same tool name — the same
// heuristic, and the same caveat, as reading them positionally.
func buildSteps(frames []Frame) []Step {
	type pending struct{ idx int }

	var steps []Step
	byID := map[string]pending{}

	for _, f := range frames {
		if f.Partial() || f.Event == nil || f.Event.Content == nil {
			continue
		}
		for i, p := range f.Event.Content.Parts {
			if p == nil {
				continue
			}
			switch {
			case p.FunctionCall != nil:
				st := Step{
					Index:      len(steps),
					Agent:      f.Agent,
					CallSeq:    f.Seq,
					RespSeq:    -1,
					Invocation: f.Event.InvocationID,
					CallID:     p.FunctionCall.ID,
					At:         f.Event.Timestamp,
					Tool:       p.FunctionCall.Name,
					Args:       p.FunctionCall.Args,
					Status:     StatusNoResponse,
				}
				steps = append(steps, st)
				if st.CallID != "" {
					byID[st.CallID] = pending{idx: st.Index}
				}
			case p.FunctionResponse != nil:
				idx := -1
				if id := p.FunctionResponse.ID; id != "" {
					if pd, ok := byID[id]; ok {
						idx = pd.idx
						delete(byID, id)
					}
				}
				if idx == -1 {
					idx = firstUnanswered(steps, p.FunctionResponse.Name)
				}
				if idx == -1 {
					continue
				}
				steps[idx].RespSeq = f.Seq
				steps[idx].Done = f.Event.Timestamp
				steps[idx].Response = p.FunctionResponse.Response
				steps[idx].Status = responseStatus(p.FunctionResponse.Response, f.rawResponse(i))
			}
		}
	}
	return steps
}

func firstUnanswered(steps []Step, tool string) int {
	for i := range steps {
		if steps[i].Tool == tool && steps[i].RespSeq == -1 {
			return i
		}
	}
	return -1
}

// errorPrefixMarkers are phrases that mean "the call failed" when they
// are how the payload OPENS. Scanned over a short prefix only, never the
// whole blob: in scenario C the probe's own log says "forbidden", and a
// whole-payload search would stamp every successful log read as an error.
// Same list, same window, as score.py.
var errorPrefixMarkers = []string{
	"error", "forbidden", "permission denied", "unauthorized",
	"failed to", "denied", "not found", "unable to",
}

const errorPrefixWindow = 200

// responseStatus mirrors score.py's response_status. Structural signals
// decide; prose only raises a suspicion.
//
// Sharp edge, mirrored on purpose: the prose scan runs over the whole
// marshalled object, KEY NAMES INCLUDED, exactly as score.py's
// `json.dumps(payload).lower()[:200]` does. So a tool that always
// returns an `"error"` field comes back "suspect" even when the field is
// empty and the call plainly succeeded. That is a false-positive source
// worth fixing — but fixing it HERE and not there would mean two
// instruments reading one transcript and disagreeing about which calls
// failed, which is the thing this rule exists to prevent. The
// divergence is pinned by a test so that whoever fixes score.py is told
// to fix this too.
// raw is the payload's original JSON when the caller still has it; it is
// what the prose scan reads, because the window is order-sensitive.
// Passing nil falls back to Go's encoding, which is close but not equal.
func responseStatus(payload map[string]any, raw json.RawMessage) Status {
	if payload == nil {
		return StatusSuspect
	}
	if b, ok := payload["isError"].(bool); ok && b {
		return StatusError
	}
	if b, ok := payload["is_error"].(bool); ok && b {
		return StatusError
	}
	if v, ok := payload["error"]; ok && !isEmptyValue(v) {
		return StatusError
	}
	if s, ok := payload["status"].(string); ok {
		switch strings.ToLower(s) {
		case "error", "failure", "failed":
			return StatusError
		}
	}
	if len(raw) == 0 {
		blob, err := json.Marshal(payload)
		if err != nil {
			return StatusSuspect
		}
		raw = blob
	}
	head := strings.ToLower(pyDumps(raw, errorPrefixWindow))
	for _, needle := range errorPrefixMarkers {
		if strings.Contains(head, needle) {
			return StatusSuspect
		}
	}
	return StatusOK
}

// isEmptyValue mirrors Python's falsiness for the values that actually
// turn up under an "error" key: absent, null, "", 0, false, [] or {}.
// Without it a tool that always returns `"error": ""` on success would
// have every call scored as a failure.
func isEmptyValue(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return t == ""
	case bool:
		return !t
	case float64:
		return t == 0
	case []any:
		return len(t) == 0
	case map[string]any:
		return len(t) == 0
	}
	return false
}
