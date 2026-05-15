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
	"sort"
	"strconv"
	"strings"

	"google.golang.org/adk/session"
)

// WriteEvents formats events from agent.Run(...) for human-readable
// streaming display. It's the library-friendly counterpart to the
// formatter inside Headless / REPL — library callers who want their
// agent loop to look like an interactive chat session can pass the
// returned iterator straight in.
//
// Routing:
//   - Partial assistant text (event.Partial == true) streams to out
//     as it arrives, with no prefix — so a model's reply renders
//     character-by-character like an interactive chat.
//   - Tool calls render as `→ name(key=value, ...)` to info.
//   - Tool responses render as `← name(key=value, ...)` to info.
//   - Final TurnComplete events are skipped (they repeat the text
//     already streamed via Partial events).
//
// out and info may point at the same writer (e.g. both os.Stdout) when
// you want a single combined stream — useful for tmux capture or
// piping. They're separate parameters so the default CLI path can
// keep tool chatter on stderr away from the assistant's reply on
// stdout.
//
// Returns the first iterator error, or nil on clean completion. A
// trailing newline is written to out if any text was streamed.
func WriteEvents(events iter.Seq2[*session.Event, error], out, info io.Writer) error {
	wroteText := false
	for event, err := range events {
		if err != nil {
			if wroteText {
				_, _ = fmt.Fprintln(out)
			}
			return err
		}
		if event == nil || event.Content == nil {
			continue
		}
		for _, p := range event.Content.Parts {
			if p == nil {
				continue
			}
			switch {
			case p.FunctionCall != nil:
				_, _ = fmt.Fprintln(info, formatCall("→", p.FunctionCall.Name, p.FunctionCall.Args))
			case p.FunctionResponse != nil:
				_, _ = fmt.Fprintln(info, formatCall("←", p.FunctionResponse.Name, p.FunctionResponse.Response))
			case p.Text != "" && event.Partial:
				if _, err := io.WriteString(out, p.Text); err != nil {
					return fmt.Errorf("runner: write text: %w", err)
				}
				wroteText = true
			}
		}
	}
	if wroteText {
		_, _ = fmt.Fprintln(out)
	}
	return nil
}

// formatCall renders one tool call or response as
// `<arrow> <name>(<key>=<value>, ...)`. Keys are sorted for stable
// output. Values are JSON-encoded for type fidelity, then truncated
// at 80 chars so a single big payload doesn't dominate the display.
func formatCall(arrow, name string, args map[string]any) string {
	if name == "" {
		name = "(unnamed)"
	}
	if len(args) == 0 {
		return arrow + " " + name + "()"
	}
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(arrow)
	b.WriteByte(' ')
	b.WriteString(name)
	b.WriteByte('(')
	for i, k := range keys {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(formatValue(args[k]))
	}
	b.WriteByte(')')
	return b.String()
}

// formatValue produces a compact one-line representation of an
// argument value. Strings stay quoted; everything else is JSON. Long
// values are truncated with an ellipsis so one giant payload doesn't
// blow up the display.
func formatValue(v any) string {
	const maxLen = 80
	switch x := v.(type) {
	case string:
		return truncate(strconv.Quote(x), maxLen)
	case nil:
		return "null"
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return "<unmarshalable>"
	}
	return truncate(string(raw), maxLen)
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-3] + "..."
}
