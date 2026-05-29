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
	"bytes"
	"errors"
	"iter"
	"os"
	"strings"
	"testing"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/adk/session"
	"google.golang.org/genai"
)

// eventSeq builds an iter.Seq2 from a fixed list of events and an
// optional terminating error.
func eventSeq(events []*session.Event, finalErr error) iter.Seq2[*session.Event, error] {
	return func(yield func(*session.Event, error) bool) {
		for _, ev := range events {
			if !yield(ev, nil) {
				return
			}
		}
		if finalErr != nil {
			yield(nil, finalErr)
		}
	}
}

func partialText(s string) *session.Event {
	return &session.Event{LLMResponse: adkmodel.LLMResponse{
		Content: &genai.Content{
			Role:  genai.RoleModel,
			Parts: []*genai.Part{{Text: s}},
		},
		Partial: true,
	}}
}

func turnComplete(s string) *session.Event {
	return &session.Event{LLMResponse: adkmodel.LLMResponse{
		Content: &genai.Content{
			Role:  genai.RoleModel,
			Parts: []*genai.Part{{Text: s}},
		},
		TurnComplete: true,
	}}
}

func toolCall(name string, args map[string]any) *session.Event {
	return &session.Event{LLMResponse: adkmodel.LLMResponse{
		Content: &genai.Content{
			Role: genai.RoleModel,
			Parts: []*genai.Part{{
				FunctionCall: &genai.FunctionCall{Name: name, Args: args},
			}},
		},
	}}
}

func toolResult(name string, resp map[string]any) *session.Event {
	return &session.Event{LLMResponse: adkmodel.LLMResponse{
		Content: &genai.Content{
			Role: genai.RoleUser,
			Parts: []*genai.Part{{
				FunctionResponse: &genai.FunctionResponse{Name: name, Response: resp},
			}},
		},
	}}
}

func TestWriteEvents_StreamsPartialTextOnlyToOut(t *testing.T) {
	t.Parallel()
	var out, info bytes.Buffer
	err := WriteEvents(eventSeq([]*session.Event{
		partialText("hel"),
		partialText("lo"),
		turnComplete("hello"), // skipped — already streamed
	}, nil), &out, &info)
	if err != nil {
		t.Fatalf("WriteEvents: %v", err)
	}
	// The `asst › ` sigil prefixes the first partial chunk of each
	// assistant speaking block so model text is visually distinct
	// from the user prompt (especially under the echo provider).
	if got := out.String(); got != "asst › hello\n" {
		t.Errorf("out = %q, want %q (sigil + streamed text + trailing newline)", got, "asst › hello\n")
	}
	if info.Len() != 0 {
		t.Errorf("info should be empty for text-only events, got %q", info.String())
	}
}

// TestWriteEvents_SigilResetsAcrossToolCall verifies the chevron is
// emitted twice when a tool call interrupts a streaming response —
// one block before the call, one after — so the conversation reads
// as discrete asst speaking segments rather than one run-on stream.
func TestWriteEvents_SigilResetsAcrossToolCall(t *testing.T) {
	t.Parallel()
	var out, info bytes.Buffer
	err := WriteEvents(eventSeq([]*session.Event{
		partialText("before "),
		partialText("the call"),
		toolCall("kubectl_get", map[string]any{"resource": "pods"}),
		toolResult("kubectl_get", map[string]any{"ok": true}),
		partialText("after the call"),
	}, nil), &out, &info)
	if err != nil {
		t.Fatalf("WriteEvents: %v", err)
	}
	got := out.String()
	want := "asst › before the call\nasst › after the call\n"
	if got != want {
		t.Errorf("out = %q\n  want %q", got, want)
	}
}

func TestWriteEvents_FormatsToolCallsToInfo(t *testing.T) {
	t.Parallel()
	var out, info bytes.Buffer
	err := WriteEvents(eventSeq([]*session.Event{
		toolCall("bash", map[string]any{"command": "ls -la"}),
		toolResult("bash", map[string]any{"exit_code": float64(0), "stdout": "main.go\nREADME.md\n"}),
		partialText("Done."),
	}, nil), &out, &info)
	if err != nil {
		t.Fatalf("WriteEvents: %v", err)
	}
	got := info.String()
	if !strings.Contains(got, "→ bash(command=") {
		t.Errorf("missing call line. info = %q", got)
	}
	if !strings.Contains(got, `"ls -la"`) {
		t.Errorf("call args not formatted as expected. info = %q", got)
	}
	if !strings.Contains(got, "← bash(") {
		t.Errorf("missing response line. info = %q", got)
	}
	if !strings.Contains(got, "exit_code=0") {
		t.Errorf("response args not formatted as expected. info = %q", got)
	}
}

// partialToolCall builds a Partial=true event carrying a FunctionCall
// part. This is the shape ADK's streamingResponseAggregator yields as
// an intermediate before the final non-Partial event with the same
// call — and the renderer was double-printing both before the fix.
func partialToolCall(name string, args map[string]any) *session.Event {
	ev := toolCall(name, args)
	ev.Partial = true
	return ev
}

func TestWriteEvents_DedupsRepeatedFunctionCall(t *testing.T) {
	t.Parallel()
	// Regression for the visual duplicate the GKE MCP smoke surfaced
	// (dev/smoke/07-mcp-google-oauth.sh): the eventlog showed exactly
	// one FunctionCall persisted but stdout printed two `→` lines.
	// Root cause was ADK's streaming aggregator yielding the same
	// FunctionCall on both a Partial and a non-Partial event; the
	// renderer rendered both. After the dedup it renders one.
	var out, info bytes.Buffer
	args := map[string]any{"parent": "projects/x/locations/-"}
	err := WriteEvents(eventSeq([]*session.Event{
		partialToolCall("gke_list_clusters", args), // ADK intermediate
		toolCall("gke_list_clusters", args),        // ADK final
		toolResult("gke_list_clusters", map[string]any{"output": "{}"}),
	}, nil), &out, &info)
	if err != nil {
		t.Fatalf("WriteEvents: %v", err)
	}
	got := info.String()
	if n := strings.Count(got, "→ gke_list_clusters("); n != 1 {
		t.Errorf("expected exactly one `→ gke_list_clusters(` line, got %d:\n%s", n, got)
	}
	if n := strings.Count(got, "← gke_list_clusters("); n != 1 {
		t.Errorf("expected exactly one `← gke_list_clusters(` line, got %d:\n%s", n, got)
	}
}

func TestWriteEvents_DifferentArgsBothRender(t *testing.T) {
	t.Parallel()
	// Dedup must NOT swallow a legitimate second call that happens
	// to share a name but has different args (e.g. two read_file
	// calls in one parallel batch). Same name + different formatted
	// line text = different `seen` keys = both render.
	var out, info bytes.Buffer
	err := WriteEvents(eventSeq([]*session.Event{
		toolCall("read_file", map[string]any{"path": "a.go"}),
		toolCall("read_file", map[string]any{"path": "b.go"}),
	}, nil), &out, &info)
	if err != nil {
		t.Fatalf("WriteEvents: %v", err)
	}
	got := info.String()
	if n := strings.Count(got, "→ read_file("); n != 2 {
		t.Errorf("expected two `→ read_file(` lines for distinct args, got %d:\n%s", n, got)
	}
	if !strings.Contains(got, `"a.go"`) || !strings.Contains(got, `"b.go"`) {
		t.Errorf("both arg values should appear: %s", got)
	}
}

func TestWriteEvents_DedupIsPerInvocation(t *testing.T) {
	t.Parallel()
	// The seen set is per-WriteEvents invocation (per-turn in the
	// REPL). Two separate WriteEvents calls with the same line must
	// each render — otherwise a tool called identically across
	// consecutive turns would silently vanish from the second turn.
	var info1, info2 bytes.Buffer
	var out bytes.Buffer
	args := map[string]any{"k": "v"}
	_ = WriteEvents(eventSeq([]*session.Event{toolCall("t", args)}, nil), &out, &info1)
	_ = WriteEvents(eventSeq([]*session.Event{toolCall("t", args)}, nil), &out, &info2)
	if !strings.Contains(info1.String(), "→ t(") {
		t.Errorf("turn 1 should render the call: %q", info1.String())
	}
	if !strings.Contains(info2.String(), "→ t(") {
		t.Errorf("turn 2 should also render the same call (per-turn scope): %q", info2.String())
	}
}

func TestWriteEvents_KeyOrderingIsStable(t *testing.T) {
	t.Parallel()
	var out, info bytes.Buffer
	args := map[string]any{"zeta": "z", "alpha": "a", "mid": "m"}
	_ = WriteEvents(eventSeq([]*session.Event{toolCall("t", args)}, nil), &out, &info)
	got := info.String()
	// Sorted keys: alpha, mid, zeta
	if got != "→ t(alpha=\"a\", mid=\"m\", zeta=\"z\")\n" {
		t.Errorf("keys not in stable sort order. info = %q", got)
	}
}

func TestWriteEvents_LongValueTruncated(t *testing.T) {
	t.Parallel()
	var out, info bytes.Buffer
	bigVal := strings.Repeat("x", 500)
	_ = WriteEvents(eventSeq([]*session.Event{
		toolCall("t", map[string]any{"k": bigVal}),
	}, nil), &out, &info)
	got := info.String()
	if len(got) > 200 {
		t.Errorf("output should be truncated, got %d bytes: %q", len(got), got)
	}
	if !strings.Contains(got, "...") {
		t.Errorf("expected ellipsis in truncated value, got %q", got)
	}
}

func TestWriteEvents_ErrorPropagates(t *testing.T) {
	t.Parallel()
	var out, info bytes.Buffer
	wantErr := errors.New("boom")
	err := WriteEvents(eventSeq([]*session.Event{
		partialText("partial"),
	}, wantErr), &out, &info)
	if !errors.Is(err, wantErr) {
		t.Errorf("expected boom error, got %v", err)
	}
	// Trailing newline should still get written so a downstream
	// terminal renders cleanly even on error.
	if !strings.HasSuffix(out.String(), "\n") {
		t.Errorf("expected trailing newline on error after partial, got %q", out.String())
	}
}

func TestWriteEvents_SharedWriterCombined(t *testing.T) {
	t.Parallel()
	// Caller wants one combined stream (e.g., for tmux capture).
	var combined bytes.Buffer
	_ = WriteEvents(eventSeq([]*session.Event{
		toolCall("read_file", map[string]any{"path": "main.go"}),
		toolResult("read_file", map[string]any{"content": "package main"}),
		partialText("Read it."),
	}, nil), &combined, &combined)
	got := combined.String()
	for _, want := range []string{"→ read_file(", "← read_file(", "Read it."} {
		if !strings.Contains(got, want) {
			t.Errorf("combined output missing %q. got %q", want, got)
		}
	}
}

func TestWriteEvents_NoArgsCallShowsParens(t *testing.T) {
	t.Parallel()
	var out, info bytes.Buffer
	_ = WriteEvents(eventSeq([]*session.Event{
		toolCall("ping", nil),
	}, nil), &out, &info)
	if got := info.String(); got != "→ ping()\n" {
		t.Errorf("got %q", got)
	}
}

func TestWriteEvents_NoColorByDefault(t *testing.T) {
	t.Parallel()
	var out, info bytes.Buffer
	_ = WriteEvents(eventSeq([]*session.Event{
		toolCall("ping", nil),
		partialText("hi"),
	}, nil), &out, &info)
	combined := out.String() + info.String()
	if strings.Contains(combined, "\033[") {
		t.Errorf("default output should have no ANSI codes, got %q", combined)
	}
}

func TestWriteEvents_WithColor_WrapsCallsAndText(t *testing.T) {
	t.Parallel()
	var out, info bytes.Buffer
	_ = WriteEvents(eventSeq([]*session.Event{
		toolCall("ping", nil),
		partialText("hi"),
	}, nil), &out, &info, WithColor(true))

	if !strings.Contains(info.String(), "\033[36m→ ping()\033[0m") {
		t.Errorf("tool call should be wrapped in cyan, got %q", info.String())
	}
	if !strings.Contains(out.String(), "\033[32mhi\033[0m") {
		t.Errorf("text should be wrapped in green, got %q", out.String())
	}
}

func TestWriteEvents_WithColorOff_SameAsDefault(t *testing.T) {
	t.Parallel()
	var outA, infoA, outB, infoB bytes.Buffer
	events := []*session.Event{
		toolCall("t", map[string]any{"k": "v"}),
		toolResult("t", map[string]any{"r": "v"}),
		partialText("done"),
	}
	_ = WriteEvents(eventSeq(events, nil), &outA, &infoA)
	_ = WriteEvents(eventSeq(events, nil), &outB, &infoB, WithColor(false))
	if outA.String() != outB.String() || infoA.String() != infoB.String() {
		t.Errorf("WithColor(false) should match default — got out:%q,%q info:%q,%q",
			outA.String(), outB.String(), infoA.String(), infoB.String())
	}
}

func groundedEvent(text string, queries []string, sources [][2]string) *session.Event {
	parts := []*genai.Part{}
	if text != "" {
		parts = append(parts, &genai.Part{Text: text})
	}
	ev := &session.Event{LLMResponse: adkmodel.LLMResponse{
		Content:      &genai.Content{Role: genai.RoleModel, Parts: parts},
		TurnComplete: true,
		GroundingMetadata: &genai.GroundingMetadata{
			WebSearchQueries: queries,
		},
	}}
	for _, s := range sources {
		ev.GroundingMetadata.GroundingChunks = append(ev.GroundingMetadata.GroundingChunks,
			&genai.GroundingChunk{Web: &genai.GroundingChunkWeb{Title: s[0], URI: s[1]}})
	}
	return ev
}

func TestWriteEvents_RendersGroundingEvidence(t *testing.T) {
	t.Parallel()
	var out, info bytes.Buffer
	err := WriteEvents(eventSeq([]*session.Event{
		partialText("Some news today: "),
		groundedEvent("", []string{"SF news 2026-05-16"},
			[][2]string{{"Example", "https://example.com/news"}}),
	}, nil), &out, &info)
	if err != nil {
		t.Fatalf("WriteEvents: %v", err)
	}
	got := info.String()
	if !strings.Contains(got, "↪ google_search: query: SF news 2026-05-16") {
		t.Errorf("expected query line; got %q", got)
	}
	if !strings.Contains(got, "↪ google_search: Example — https://example.com/news") {
		t.Errorf("expected source line; got %q", got)
	}
}

func TestWriteEvents_GroundingSkippedOnPartial(t *testing.T) {
	t.Parallel()
	// Grounding metadata can appear on both a partial event and the
	// final aggregate — rendering both would double-print. Render
	// only when Partial=false.
	var out, info bytes.Buffer
	partial := groundedEvent("partial text", []string{"q"}, nil)
	partial.Partial = true
	_ = WriteEvents(eventSeq([]*session.Event{partial}, nil), &out, &info)
	if strings.Contains(info.String(), "↪") {
		t.Errorf("partial events should not render grounding; got %q", info.String())
	}
}

func TestWriteEvents_GroundingSkipsEmptyQueryStrings(t *testing.T) {
	t.Parallel()
	// Vertex sometimes returns an empty string in WebSearchQueries;
	// don't render "query: " with a trailing blank.
	var out, info bytes.Buffer
	_ = WriteEvents(eventSeq([]*session.Event{
		groundedEvent("", []string{"", "real"}, nil),
	}, nil), &out, &info)
	got := info.String()
	if strings.Contains(got, "query: \n") {
		t.Errorf("empty query should be skipped; got %q", got)
	}
	if !strings.Contains(got, "query: real") {
		t.Errorf("real query missing; got %q", got)
	}
}

func TestWriteEvents_SkipsGroundingWhenAbsent(t *testing.T) {
	t.Parallel()
	var out, info bytes.Buffer
	_ = WriteEvents(eventSeq([]*session.Event{
		partialText("plain"),
	}, nil), &out, &info)
	if strings.Contains(info.String(), "↪") {
		t.Errorf("no grounding metadata = no ↪ output; got %q", info.String())
	}
}

func TestWriteEvents_GroundingHonorsColor(t *testing.T) {
	t.Parallel()
	var out, info bytes.Buffer
	_ = WriteEvents(eventSeq([]*session.Event{
		groundedEvent("", []string{"q"}, nil),
	}, nil), &out, &info, WithColor(true))
	if !strings.Contains(info.String(), "\033[35m↪ google_search: query: q\033[0m") {
		t.Errorf("grounding line should be wrapped in magenta; got %q", info.String())
	}
}

func TestFormatAlertLine_FormatsAndDefaults(t *testing.T) {
	t.Parallel()
	got := FormatAlertLine("watch-prod", "alert", "pod restarted")
	if got != "↪ watch-prod alert: pod restarted" {
		t.Errorf("unexpected line: %q", got)
	}
	// Empty kind defaults to "alert".
	got = FormatAlertLine("x", "", "msg")
	if got != "↪ x alert: msg" {
		t.Errorf("empty kind not defaulted: %q", got)
	}
	// Empty from defaults to "?".
	got = FormatAlertLine("", "completed", "done")
	if got != "↪ ? completed: done" {
		t.Errorf("empty from not defaulted: %q", got)
	}
}

func TestIsTerminal_FalseForBuffer(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if IsTerminal(&buf) {
		t.Errorf("bytes.Buffer is not a terminal")
	}
}

func TestIsTerminal_FalseForPipe(t *testing.T) {
	t.Parallel()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer r.Close()
	defer w.Close()
	if IsTerminal(r) {
		t.Errorf("pipe read-end is not a terminal")
	}
	if IsTerminal(w) {
		t.Errorf("pipe write-end is not a terminal")
	}
}
