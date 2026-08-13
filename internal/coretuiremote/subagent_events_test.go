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

package coretuiremote

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/adk/session"
	"google.golang.org/genai"

	coretui "github.com/go-steer/core-tui/tui"

	"github.com/go-steer/core-agent/v2/internal/attachclient"
	"github.com/go-steer/core-agent/v2/pkg/attach"
)

// subagentEventsServer stands up a fake daemon serving one canned
// response on GET /sessions/{sid}/agents/{name}/events, and records
// the raw query string so cursor propagation can be asserted.
func subagentEventsServer(t *testing.T, status int, body any) (*Adapter, *string) {
	t.Helper()
	var lastQuery string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /sessions/{sid}/agents/{name}/events", func(w http.ResponseWriter, r *http.Request) {
		lastQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	parsed, err := attachclient.ParseURL(srv.URL + "/sessions/s1")
	if err != nil {
		t.Fatalf("ParseURL: %v", err)
	}
	return New(attachclient.New(parsed, "", 0), "/sessions/s1"), &lastQuery
}

func subagentFrame(seq int64, text string, parts ...*genai.Part) attach.Frame {
	ev := session.NewEvent("e-" + text)
	ev.Author = "cluster"
	ev.Timestamp = time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	all := []*genai.Part{}
	if text != "" {
		all = append(all, &genai.Part{Text: text})
	}
	all = append(all, parts...)
	ev.LLMResponse = adkmodel.LLMResponse{
		Content: &genai.Content{Role: genai.RoleModel, Parts: all},
	}
	return attach.Frame{Seq: seq, Event: ev}
}

// TestAdapterSubagentEvents_ProjectsTurns covers the shape the overlay
// and the inline tail both render: prose, a tool call, and its result
// with the conventional "error" key lifted out.
func TestAdapterSubagentEvents_ProjectsTurns(t *testing.T) {
	t.Parallel()
	call := &genai.Part{FunctionCall: &genai.FunctionCall{
		ID: "c1", Name: "bash", Args: map[string]any{"cmd": "kubectl get pods"},
	}}
	result := &genai.Part{FunctionResponse: &genai.FunctionResponse{
		ID: "c1", Name: "bash", Response: map[string]any{"error": "exit status 1"},
	}}
	a, query := subagentEventsServer(t, http.StatusOK, attach.SubagentEventsResponse{
		Agent:     "cluster",
		Events:    []attach.Frame{subagentFrame(41, "listing pods", call), subagentFrame(42, "", result)},
		NextSince: 42,
		Truncated: true,
	})

	page, err := a.SubagentEvents(context.Background(), "cluster", 7)
	if err != nil {
		t.Fatalf("SubagentEvents: %v", err)
	}
	if len(page.Events) != 2 {
		t.Fatalf("got %d events, want 2: %+v", len(page.Events), page.Events)
	}
	if page.NextSince != 42 || !page.Truncated {
		t.Errorf("page cursor = (%d, %v), want (42, true)", page.NextSince, page.Truncated)
	}
	first := page.Events[0]
	if first.Seq != 41 || first.Author != "cluster" || first.Text != "listing pods" {
		t.Errorf("first turn = %+v, want seq 41 / cluster / \"listing pods\"", first)
	}
	if len(first.ToolCalls) != 1 || first.ToolCalls[0].Name != "bash" {
		t.Fatalf("first turn tool calls = %+v, want one bash call", first.ToolCalls)
	}
	if got := first.ToolCalls[0].Args["cmd"]; got != "kubectl get pods" {
		t.Errorf("call args cmd = %v, want the command", got)
	}
	second := page.Events[1]
	if len(second.ToolResults) != 1 || second.ToolResults[0].Error != "exit status 1" {
		t.Errorf("second turn results = %+v, want the error lifted out", second.ToolResults)
	}
	// The cursor has to reach the wire, or the once-a-second tail
	// re-reads the whole log every tick.
	if *query != "since=7" {
		t.Errorf("query = %q, want since=7", *query)
	}
}

// TestAdapterSubagentEvents_SkipsPartials keeps a streamed prefix out
// of the turn log: a partial and its settled twin would otherwise
// print the same answer twice.
func TestAdapterSubagentEvents_SkipsPartials(t *testing.T) {
	t.Parallel()
	partial := subagentFrame(41, "listing")
	partial.Event.Partial = true
	a, _ := subagentEventsServer(t, http.StatusOK, attach.SubagentEventsResponse{
		Events: []attach.Frame{partial, subagentFrame(42, "listing pods")},
	})

	page, err := a.SubagentEvents(context.Background(), "cluster", 0)
	if err != nil {
		t.Fatalf("SubagentEvents: %v", err)
	}
	if len(page.Events) != 1 || page.Events[0].Seq != 42 {
		t.Errorf("events = %+v, want only the settled turn (seq 42)", page.Events)
	}
}

// TestAdapterSubagentEvents_NotFoundIsTyped is the #694 contract at
// the TUI boundary: the overlay must be able to say "no such subagent,
// here are the ones there are" rather than paint a convincing empty
// log.
func TestAdapterSubagentEvents_NotFoundIsTyped(t *testing.T) {
	t.Parallel()
	a, _ := subagentEventsServer(t, http.StatusNotFound, attach.SubagentNotFoundResponse{
		Error:     `no turns recorded for subagent "clustr" in this session`,
		Agent:     "clustr",
		Available: []string{"auditor", "cluster"},
	})

	_, err := a.SubagentEvents(context.Background(), "clustr", 0)
	var nf *coretui.SubagentNotFoundError
	if !errors.As(err, &nf) {
		t.Fatalf("err = %v (%T), want *coretui.SubagentNotFoundError", err, err)
	}
	if nf.Name != "clustr" {
		t.Errorf("Name = %q, want clustr", nf.Name)
	}
	if len(nf.Available) != 2 || nf.Available[1] != "cluster" {
		t.Errorf("Available = %v, want [auditor cluster]", nf.Available)
	}
}

// TestAdapterSubagentEvents_TransportErrorStaysError pins that a
// server fault is NOT reported as an empty page — core-tui keeps the
// turns it already has and shows an error row, and the inline tail
// treats it as a blip rather than "this tool has no turn log".
func TestAdapterSubagentEvents_TransportErrorStaysError(t *testing.T) {
	t.Parallel()
	a, _ := subagentEventsServer(t, http.StatusInternalServerError, map[string]string{"error": "boom"})

	page, err := a.SubagentEvents(context.Background(), "cluster", 0)
	if err == nil {
		t.Fatal("a 500 should surface as an error, not an empty page")
	}
	var nf *coretui.SubagentNotFoundError
	if errors.As(err, &nf) {
		t.Errorf("a 500 was misread as a not-found name: %v", err)
	}
	if len(page.Events) != 0 {
		t.Errorf("page = %+v, want zero events on error", page.Events)
	}
}
