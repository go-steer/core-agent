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

package runner

import (
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"strings"

	"google.golang.org/adk/v2/session"

	"github.com/go-steer/core-agent/v3/pkg/usage"
)

// OutputFormat selects how Headless encodes assistant output on stdout (#1379).
type OutputFormat string

const (
	// OutputFormatText streams assistant text to stdout (omitting the
	// interactive "asst › " prefix when stdout is not a terminal).
	OutputFormatText OutputFormat = "text"
	// OutputFormatJSON emits a single JSON object (HeadlessJSONResult) on
	// stdout once the turn completes.
	OutputFormatJSON OutputFormat = "json"
	// OutputFormatStreamJSON emits newline-delimited JSON (NDJSON) events as
	// the turn progresses, ending with a type="result" HeadlessJSONResult line.
	OutputFormatStreamJSON OutputFormat = "stream-json"
)

// ParseOutputFormat validates and normalizes a CLI --output-format flag value.
// An empty string defaults to OutputFormatText.
func ParseOutputFormat(raw string) (OutputFormat, error) {
	switch OutputFormat(strings.TrimSpace(raw)) {
	case "", OutputFormatText:
		return OutputFormatText, nil
	case OutputFormatJSON:
		return OutputFormatJSON, nil
	case OutputFormatStreamJSON:
		return OutputFormatStreamJSON, nil
	default:
		return "", fmt.Errorf("--output-format: unknown value %q (want text|json|stream-json)", raw)
	}
}

// HeadlessJSONUsage summarizes token and cost accounting in JSON output mode.
type HeadlessJSONUsage struct {
	Turns                    int     `json:"turns"`
	InputTokens              int     `json:"input_tokens"`
	UncachedInputTokens      int     `json:"uncached_input_tokens"`
	CachedInputTokens        int     `json:"cached_input_tokens,omitempty"`
	CacheCreationInputTokens int     `json:"cache_creation_input_tokens,omitempty"`
	OutputTokens             int     `json:"output_tokens"`
	ThoughtsTokens           int     `json:"thoughts_tokens,omitempty"`
	CostUSD                  float64 `json:"cost_usd"`
	Unpriced                 bool    `json:"unpriced,omitempty"`
}

// HeadlessJSONResult is the top-level JSON payload written to stdout by
// --output-format=json (and as the final type="result" line of
// --output-format=stream-json).
type HeadlessJSONResult struct {
	Type       string            `json:"type,omitempty"`
	Text       string            `json:"text"`
	StopReason string            `json:"stop_reason"`
	Model      string            `json:"model,omitempty"`
	Usage      HeadlessJSONUsage `json:"usage"`
	CostUSD    float64           `json:"cost_usd"`
	Error      string            `json:"error,omitempty"`
}

// HeadlessStreamEvent is one intermediate NDJSON line emitted by
// --output-format=stream-json before the terminal HeadlessJSONResult line.
type HeadlessStreamEvent struct {
	Type     string         `json:"type"`
	Text     string         `json:"text,omitempty"`
	Name     string         `json:"name,omitempty"`
	Args     map[string]any `json:"args,omitempty"`
	Response map[string]any `json:"response,omitempty"`
	Line     string         `json:"line,omitempty"`
}

func writeJSONTurn(events iter.Seq2[*session.Event, error], out io.Writer, tracker *usage.Tracker, modelName string, format OutputFormat) error {
	var partialBuilder strings.Builder
	var fallbackBuilder strings.Builder
	sawPartial := false
	stopReason := ""
	seen := make(seenLines)

	for event, err := range events {
		if err != nil {
			res := buildHeadlessJSONResult(partialBuilder.String(), fallbackBuilder.String(), sawPartial, "error", modelName, tracker, err)
			if format == OutputFormatStreamJSON {
				res.Type = "result"
			}
			_ = writeJSONLine(out, res)
			return err
		}
		if event == nil {
			continue
		}
		if event.FinishReason != "" {
			stopReason = strings.ToLower(string(event.FinishReason))
		}
		if err := processJSONEventParts(event, out, format, &partialBuilder, &fallbackBuilder, &sawPartial); err != nil {
			return err
		}
		for _, line := range serverSideBuiltinLines(event) {
			if !seen.add(line) {
				continue
			}
			if format == OutputFormatStreamJSON {
				if err := writeJSONLine(out, HeadlessStreamEvent{Type: "grounding", Line: line}); err != nil {
					return err
				}
			}
		}
	}

	if stopReason == "" {
		stopReason = "stop"
	}
	res := buildHeadlessJSONResult(partialBuilder.String(), fallbackBuilder.String(), sawPartial, stopReason, modelName, tracker, nil)
	if format == OutputFormatStreamJSON {
		res.Type = "result"
	}
	return writeJSONLine(out, res)
}

func processJSONEventParts(
	event *session.Event,
	out io.Writer,
	format OutputFormat,
	partialBuilder, fallbackBuilder *strings.Builder,
	sawPartial *bool,
) error {
	if event.Content == nil {
		return nil
	}
	for _, p := range event.Content.Parts {
		if p == nil {
			continue
		}
		if event.Partial && (p.FunctionCall != nil || p.FunctionResponse != nil) {
			continue
		}
		switch {
		case p.FunctionCall != nil:
			if format == OutputFormatStreamJSON {
				if err := writeJSONLine(out, HeadlessStreamEvent{
					Type: "tool_call",
					Name: p.FunctionCall.Name,
					Args: p.FunctionCall.Args,
				}); err != nil {
					return err
				}
			}
		case p.FunctionResponse != nil:
			if format == OutputFormatStreamJSON {
				if err := writeJSONLine(out, HeadlessStreamEvent{
					Type:     "tool_result",
					Name:     p.FunctionResponse.Name,
					Response: p.FunctionResponse.Response,
				}); err != nil {
					return err
				}
			}
		case p.Text != "" && !p.Thought && event.Partial:
			*sawPartial = true
			partialBuilder.WriteString(p.Text)
			if format == OutputFormatStreamJSON {
				if err := writeJSONLine(out, HeadlessStreamEvent{
					Type: "text_delta",
					Text: p.Text,
				}); err != nil {
					return err
				}
			}
		case p.Text != "" && !p.Thought && !event.Partial:
			fallbackBuilder.WriteString(p.Text)
		}
	}
	return nil
}

func buildHeadlessJSONResult(partialText, fallbackText string, sawPartial bool, stopReason, modelName string, tracker *usage.Tracker, runErr error) HeadlessJSONResult {
	text := partialText
	if !sawPartial {
		text = fallbackText
	}
	var u HeadlessJSONUsage
	if tracker != nil {
		tot := tracker.Totals()
		u = HeadlessJSONUsage{
			Turns:                    tot.Turns,
			InputTokens:              tot.InputTokens,
			UncachedInputTokens:      tot.UncachedInputTokens(),
			CachedInputTokens:        tot.CachedInputTokens,
			CacheCreationInputTokens: tot.CacheCreationInputTokens,
			OutputTokens:             tot.OutputTokens,
			ThoughtsTokens:           tot.ThoughtsTokens,
			CostUSD:                  tot.CostUSD,
			Unpriced:                 tot.UnpricedTurns > 0,
		}
	}
	res := HeadlessJSONResult{
		Text:       text,
		StopReason: stopReason,
		Model:      modelName,
		Usage:      u,
		CostUSD:    u.CostUSD,
	}
	if runErr != nil {
		res.Error = runErr.Error()
	}
	return res
}

func writeJSONLine(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return fmt.Errorf("runner: write json: %w", err)
	}
	return nil
}
