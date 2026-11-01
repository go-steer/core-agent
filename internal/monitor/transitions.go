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

// Package monitor parses the structured transition records returned by a
// monitor.collect tool and defines the argument contract for monitor.ack.
package monitor

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Transition is one edge reported by the collect tool's transitions array.
type Transition struct {
	Kind       string            `json:"kind"`
	Edge       string            `json:"edge"`
	SubjectKey string            `json:"subject_key"`
	Summary    string            `json:"summary"`
	Attrs      map[string]string `json:"attrs"`
}

// Set is the parsed transition envelope extracted from a collect tool's
// output at monitor.transitions_from.
type Set struct {
	Transitions []Transition `json:"transitions"`
	Degraded    bool         `json:"degraded"`
}

// ParseResult extracts a transition Set from an ADK tool result map
// (looking up the "output" text key when present, or marshaling the map).
func ParseResult(result map[string]any) (Set, error) {
	if result == nil {
		return Set{}, fmt.Errorf("monitor: nil tool result")
	}
	if out, ok := result["output"].(string); ok {
		return Parse(out)
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return Set{}, fmt.Errorf("monitor: marshal tool result: %w", err)
	}
	return Parse(string(raw))
}

// Parse decodes the JSON text returned by a collect tool into a Set.
func Parse(raw string) (Set, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Set{}, fmt.Errorf("monitor: empty collect output")
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &obj); err != nil {
		return Set{}, fmt.Errorf("monitor: collect output is not a JSON object: %w", err)
	}
	transRaw, ok := obj["transitions"]
	if !ok {
		return Set{}, fmt.Errorf("monitor: collect output is missing required field \"transitions\"")
	}
	var transitions []Transition
	if err := json.Unmarshal(transRaw, &transitions); err != nil {
		return Set{}, fmt.Errorf("monitor: decode \"transitions\": %w", err)
	}
	if transitions == nil {
		return Set{}, fmt.Errorf("monitor: field \"transitions\" must be a JSON array, got null")
	}
	var degraded bool
	if degRaw, ok := obj["degraded"]; ok {
		if err := json.Unmarshal(degRaw, &degraded); err != nil {
			return Set{}, fmt.Errorf("monitor: decode \"degraded\": %w", err)
		}
	}
	return Set{
		Transitions: transitions,
		Degraded:    degraded,
	}, nil
}
