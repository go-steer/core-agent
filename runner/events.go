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
	"os"
	"sort"
	"strconv"
	"strings"

	"google.golang.org/adk/session"
)

// EventsOption configures WriteEvents. Use the With* helpers below.
type EventsOption func(*eventsConfig)

type eventsConfig struct {
	color bool
}

// eventsConfigFromOpts resolves the eventsConfig from a slice of
// options. Exposed (lowercase, same package) so the REPL's alert
// display path can know whether to render colored ↪ lines without
// re-parsing the opts itself.
func eventsConfigFromOpts(opts []EventsOption) eventsConfig {
	cfg := eventsConfig{}
	for _, opt := range opts {
		opt(&cfg)
	}
	return cfg
}

// WithColor enables ANSI color codes in WriteEvents output. Tool
// calls and responses render in cyan; partial assistant text in
// green. Off by default — colored output looks like garbage when
// piped to a file, so callers must opt in (typically guarded by
// IsTerminal(out)).
func WithColor(on bool) EventsOption {
	return func(c *eventsConfig) { c.color = on }
}

// seenLines is a small set used to dedupe ↪ output within one
// WriteEvents call. Vertex sometimes re-emits the same grounding
// metadata on more than one event in the stream (e.g. a partial
// aggregate and the final response), but each line should appear
// to the user exactly once per turn.
type seenLines map[string]struct{}

func (s seenLines) add(line string) (newLine bool) {
	if _, ok := s[line]; ok {
		return false
	}
	s[line] = struct{}{}
	return true
}

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
//   - Server-side built-in evidence (Gemini grounding metadata)
//     renders as `↪ google_search: ...` to info, AFTER the model's
//     text, since the metadata only lands on the aggregated final
//     event.
//   - Final TurnComplete events are skipped (they repeat the text
//     already streamed via Partial events).
//
// out and info may point at the same writer (e.g. both os.Stdout) when
// you want a single combined stream — useful for tmux capture or
// piping. They're separate parameters so the default CLI path can
// keep tool chatter on stderr away from the assistant's reply on
// stdout.
//
// Pass WithColor(true) to enable ANSI styling — typically gated on
// IsTerminal(out) so piped output stays clean.
//
// Returns the first iterator error, or nil on clean completion. A
// trailing newline is written to out if any text was streamed.
func WriteEvents(events iter.Seq2[*session.Event, error], out, info io.Writer, opts ...EventsOption) error {
	cfg := eventsConfig{}
	for _, opt := range opts {
		opt(&cfg)
	}
	wroteText := false
	seen := make(seenLines)
	for event, err := range events {
		if err != nil {
			if wroteText {
				_, _ = fmt.Fprintln(out)
			}
			return err
		}
		if event == nil {
			continue
		}
		if event.Content != nil {
			for _, p := range event.Content.Parts {
				if p == nil {
					continue
				}
				switch {
				case p.FunctionCall != nil:
					line := formatCall("→", p.FunctionCall.Name, p.FunctionCall.Args)
					_, _ = fmt.Fprintln(info, paint(line, ansiCyan, cfg.color))
				case p.FunctionResponse != nil:
					line := formatCall("←", p.FunctionResponse.Name, p.FunctionResponse.Response)
					_, _ = fmt.Fprintln(info, paint(line, ansiCyan, cfg.color))
				case p.Text != "" && event.Partial:
					text := paint(p.Text, ansiGreen, cfg.color)
					if _, err := io.WriteString(out, text); err != nil {
						return fmt.Errorf("runner: write text: %w", err)
					}
					wroteText = true
				}
			}
		}
		// Surface server-side built-in evidence the same way client
		// tool calls render — but with a different sigil so the
		// reader can tell "Google did this on the server" apart from
		// "your local tool ran." Drains AFTER any partial text from
		// the same event so the chat reads in source order.
		for _, line := range serverSideBuiltinLines(event) {
			if !seen.add(line) {
				continue
			}
			_, _ = fmt.Fprintln(info, paint(line, ansiMagenta, cfg.color))
		}
	}
	if wroteText {
		_, _ = fmt.Fprintln(out)
	}
	return nil
}

// FormatAlertLine renders one background-agent Alert as a single
// "↪ <from> <kind>: <text>" line in the same shape as the
// ↪ google_search lines from v1.1.0. Reuses the magenta sigil so
// consumers see a consistent "server-side / out-of-band activity"
// signal regardless of source.
//
// Exposed so consumers wiring their own alert sinks (a Slack adapter,
// a web UI, a logger) format alerts consistently with what the CLI
// shows. Use with paint(line, runner.AnsiMagenta(), color) when you
// want ANSI styling; raw string otherwise.
func FormatAlertLine(from, kind, text string) string {
	if kind == "" {
		kind = "alert"
	}
	if from == "" {
		from = "?"
	}
	return "↪ " + from + " " + kind + ": " + text
}

// AnsiMagenta returns the ANSI escape used for ↪-prefixed lines so
// downstream alert sinks can match the runner's color choice without
// duplicating the constant.
func AnsiMagenta() string { return ansiMagenta }

// serverSideBuiltinLines returns "↪ <tool>: <detail>" lines for any
// server-side built-in evidence carried on event. Today: Gemini
// GoogleSearch grounding (queries + grounded web sources). Empty
// slice for events with no such evidence, for partial events
// (grounding lands on the aggregate event; rendering partials too
// would double-print everything), and for events whose queries /
// sources are all empty.
//
// Format must stay in sync with models/gemini/projection.go so
// stdout chatter and eventlog rows describe the same thing.
func serverSideBuiltinLines(event *session.Event) []string {
	if event == nil || event.Partial {
		return nil
	}
	gm := event.GroundingMetadata
	if gm == nil {
		return nil
	}
	var out []string
	for _, q := range gm.WebSearchQueries {
		if q == "" {
			continue
		}
		out = append(out, "↪ google_search: query: "+q)
	}
	for _, ch := range gm.GroundingChunks {
		if ch == nil || ch.Web == nil || ch.Web.URI == "" {
			continue
		}
		text := ch.Web.URI
		if ch.Web.Title != "" {
			text = ch.Web.Title + " — " + ch.Web.URI
		}
		out = append(out, "↪ google_search: "+text)
	}
	return out
}

// IsTerminal reports whether w is connected to a terminal (TTY).
// Use to gate WithColor: pass WithColor(IsTerminal(os.Stdout)) so
// color renders interactively but not when output is captured to a
// file or piped to another process.
//
// Returns false for any writer that isn't a *os.File (buffers,
// pipes, network connections). On Unix and modern Windows, character
// devices report ModeCharDevice; that's the signal we trust.
func IsTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// ANSI escape codes. Kept minimal on purpose — four colors cover
// the chat-like display (tool calls cyan, agent text green,
// server-side built-in evidence magenta, plain for everything else).
const (
	ansiReset   = "\033[0m"
	ansiCyan    = "\033[36m"
	ansiGreen   = "\033[32m"
	ansiMagenta = "\033[35m"
)

// paint wraps s in the given ANSI color when on is true. When off
// (the default), returns s unchanged so non-TTY consumers don't see
// escape codes.
func paint(s, code string, on bool) string {
	if !on || code == "" {
		return s
	}
	return code + s + ansiReset
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
