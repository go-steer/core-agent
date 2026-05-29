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

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/itchyny/gojq"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"

	"github.com/go-steer/core-agent/pkg/config"
	"github.com/go-steer/core-agent/pkg/permissions"
)

type jsonQueryArgs struct {
	Path  string `json:"path,omitempty" jsonschema:"file to load JSON from. Mutually exclusive with json; provide exactly one."`
	JSON  string `json:"json,omitempty" jsonschema:"inline JSON string to query. Mutually exclusive with path; provide exactly one."`
	Query string `json:"query" jsonschema:"jq expression (https://jqlang.github.io/jq/manual/). Examples: '.items[].metadata.name', '.[] | select(.status == \"Running\") | .name', 'length'."`
}

type jsonQueryResult struct {
	// Results is the jq-evaluated output. jq queries can yield zero,
	// one, or many values; we always return a slice so the model
	// gets a consistent shape. Each entry is the JSON-marshaled form
	// of one jq output value.
	Results []json.RawMessage `json:"results"`
	// Count is len(Results) — surfaced so the model can check
	// emptiness without parsing the slice.
	Count int `json:"count"`
}

// NewJSONQueryTool returns the json_query tool. Uses gojq (pure Go,
// no CGO — safe for distroless) to evaluate a jq expression against
// JSON loaded from either a file path or an inline json string.
// Designed primarily to slice the large structured outputs that
// remote MCP servers (kubectl get pods -o json, etc.) return, so
// the model gets just the field it asked for rather than burning
// prompt-cache space on a hundreds-of-KB JSON blob.
//
// Returns an error result (not a Go error) for malformed JSON or
// bad jq expressions so the model can adapt rather than aborting
// the turn. Real errors (gate denials, file I/O) propagate as Go
// errors.
func NewJSONQueryTool(gate *permissions.Gate, cfg *config.Config) tool.Tool {
	t, err := functiontool.New(
		functiontool.Config{
			Name:        "json_query",
			Description: "Run a jq expression against JSON loaded from a file (path) or supplied inline (json). Returns the jq results as a slice of JSON values plus a count. PREFERRED for inspecting large structured outputs (kubectl -o json, gcloud --format=json, REST API responses) — pulls just the slice you asked for so the full blob never enters your context. Output is truncated to the per-tool cap; if you hit it, narrow the query.",
		},
		jsonQueryFunc(gate, cfg),
	)
	if err != nil {
		panic("tools: NewJSONQueryTool: " + err.Error())
	}
	return t
}

// jsonQueryFunc is the handler, extracted so tests can drive it
// without going through ADK's functiontool wrapper.
func jsonQueryFunc(gate *permissions.Gate, cfg *config.Config) functiontool.Func[jsonQueryArgs, jsonQueryResult] {
	return func(_ tool.Context, in jsonQueryArgs) (jsonQueryResult, error) {
		hasPath := in.Path != ""
		hasJSON := in.JSON != ""
		if hasPath == hasJSON {
			return jsonQueryResult{}, fmt.Errorf("json_query: provide exactly one of path or json (got %d)", boolToInt(hasPath)+boolToInt(hasJSON))
		}
		if in.Query == "" {
			return jsonQueryResult{}, errors.New("json_query: query is required")
		}

		var raw []byte
		if hasPath {
			path, err := absolutize(in.Path)
			if err != nil {
				return jsonQueryResult{}, err
			}
			if err := gate.CheckFileRead(context.Background(), "json_query", path); err != nil {
				return jsonQueryResult{}, err
			}
			raw, err = os.ReadFile(path)
			if err != nil {
				return jsonQueryResult{}, fmt.Errorf("json_query: %w", err)
			}
		} else {
			raw = []byte(in.JSON)
		}

		var input any
		if err := json.Unmarshal(raw, &input); err != nil {
			// Malformed JSON is a model-facing error (probably a bad
			// file or hand-rolled JSON string), not a transport
			// failure. Return as a Go error so the runner surfaces it
			// as a tool error result.
			return jsonQueryResult{}, fmt.Errorf("json_query: parse input: %w", err)
		}

		query, err := gojq.Parse(in.Query)
		if err != nil {
			return jsonQueryResult{}, fmt.Errorf("json_query: parse query: %w", err)
		}

		iter := query.Run(input)
		results := make([]json.RawMessage, 0, 4)
		caps := capsFor(cfg, "json_query", 256*1024, 5000)
		var totalBytes int
		for {
			v, ok := iter.Next()
			if !ok {
				break
			}
			if qerr, isErr := v.(error); isErr {
				return jsonQueryResult{}, fmt.Errorf("json_query: eval: %w", qerr)
			}
			encoded, mErr := json.Marshal(v)
			if mErr != nil {
				return jsonQueryResult{}, fmt.Errorf("json_query: marshal result: %w", mErr)
			}
			results = append(results, encoded)
			totalBytes += len(encoded)
			if caps.bytes > 0 && totalBytes >= caps.bytes {
				// Stop accumulating once we've passed the cap;
				// trailing results would be truncated anyway and
				// continuing the jq iterator can be expensive on a
				// query that fans out to N matches.
				break
			}
		}
		return jsonQueryResult{Results: results, Count: len(results)}, nil
	}
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
