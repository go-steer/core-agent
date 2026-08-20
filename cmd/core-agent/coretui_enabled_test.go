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

//go:build !no_tui

package main

import (
	"errors"
	"strings"
	"testing"

	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v2/pkg/attach"
)

func TestSplitFunctionResponse_NoError(t *testing.T) {
	resp := &genai.FunctionResponse{
		Name:     "read_file",
		Response: map[string]any{"content": "package main"},
	}
	body, errStr := splitFunctionResponse(resp)
	if errStr != "" {
		t.Errorf("expected empty errStr for success response, got %q", errStr)
	}
	if body["content"] != "package main" {
		t.Errorf("expected response body preserved, got %v", body)
	}
}

func TestSplitFunctionResponse_StringError(t *testing.T) {
	resp := &genai.FunctionResponse{
		Name:     "read_file",
		Response: map[string]any{"error": "permission denied"},
	}
	body, errStr := splitFunctionResponse(resp)
	if errStr != "permission denied" {
		t.Errorf("expected 'permission denied' errStr, got %q", errStr)
	}
	// Body is still passed through (caller may want to inspect both).
	if body["error"] != "permission denied" {
		t.Errorf("expected error key preserved on body, got %v", body)
	}
}

func TestSplitFunctionResponse_ErrorTypeError(t *testing.T) {
	resp := &genai.FunctionResponse{
		Name:     "read_file",
		Response: map[string]any{"error": errors.New("disk full")},
	}
	_, errStr := splitFunctionResponse(resp)
	if errStr != "disk full" {
		t.Errorf("expected 'disk full' from error-typed value, got %q", errStr)
	}
}

func TestSplitFunctionResponse_NilResp(t *testing.T) {
	body, errStr := splitFunctionResponse(nil)
	if body != nil || errStr != "" {
		t.Errorf("expected (nil, \"\") for nil input, got (%v, %q)", body, errStr)
	}
}

func TestSplitFunctionResponse_NilResponseMap(t *testing.T) {
	body, errStr := splitFunctionResponse(&genai.FunctionResponse{Name: "x"})
	if body != nil || errStr != "" {
		t.Errorf("expected (nil, \"\") for nil response map, got (%v, %q)", body, errStr)
	}
}

func TestSplitFunctionResponse_NonStringError(t *testing.T) {
	// An "error" key of an unexpected type (e.g. int) shouldn't crash
	// and shouldn't set errStr — we only recognize string / error.
	resp := &genai.FunctionResponse{
		Name:     "read_file",
		Response: map[string]any{"error": 42},
	}
	_, errStr := splitFunctionResponse(resp)
	if errStr != "" {
		t.Errorf("expected empty errStr for non-string error value, got %q", errStr)
	}
}

// TestPreambleFor pins the chat-visible "running…" rows that
// AsyncSlashProvider surfaces at dispatch (core-tui
// v0.6.3, issue #55). Unknown slashes return "" so they fall
// through to bare-async behavior; classified slashes echo the
// arg when supplied so the row confirms the command parsed
// correctly.
func TestPreambleFor(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		args   string
		expect string // exact match; "" means no preamble (row skipped)
	}{
		// /done — with and without a note
		{"done", "", "Capturing checkpoint summary…"},
		{"done", "finished surveying messageKinds", "Capturing checkpoint summary (note: finished surveying messageKinds)…"},
		{"checkpoint", "alias works too", "Capturing checkpoint summary (note: alias works too)…"},
		{"done", "   trimmed   ", "Capturing checkpoint summary (note: trimmed)…"},

		// /compact — with and without focus
		{"compact", "", "Summarizing session for context compaction…"},
		{"compact", "auth module", "Summarizing session for context compaction (focus: auth module)…"},
		{"summarize", "alias works too", "Summarizing session for context compaction (focus: alias works too)…"},

		// /btw — with and without a question
		{"btw", "", "Asking the model a side question…"},
		{"btw", "what was that file again?", "Asking the model: what was that file again?"},
		{"by-the-way", "alias works", "Asking the model: alias works"},

		// Unknown slash — no preamble (falls through to bare-async)
		{"unknown", "", ""},
		{"unknown", "with args", ""},
		{"context", "", ""},         // /context is fast (sync handler), no preamble needed
		{"subagent", "spawn x", ""}, // /subagent is a TODO stub today, no preamble until it does real work
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name+"_"+tc.args, func(t *testing.T) {
			t.Parallel()
			got := preambleFor(tc.name, tc.args)
			if got != tc.expect {
				t.Errorf("preambleFor(%q, %q) = %q, want %q", tc.name, tc.args, got, tc.expect)
			}
		})
	}
}

func TestFormatSubagentCatalog(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		cat        []attach.SubagentCatalogInfo
		wantSubstr []string
	}{
		{
			name:       "empty roster",
			cat:        nil,
			wantSubstr: []string{"0 configured sub-agent(s)"},
		},
		{
			name: "declarative subagent with model + root + modes + description",
			cat: []attach.SubagentCatalogInfo{
				{Name: "cluster", Model: "gemini-3.5-flash", Root: "../cluster", Modes: []string{"sync", "async"}, Description: "read-only cluster ops"},
			},
			wantSubstr: []string{
				"1 configured sub-agent(s)",
				"• cluster [model=gemini-3.5-flash, root=../cluster, sync+async, no tools] — read-only cluster ops",
			},
		},
		{
			name: "predefined spec: async-only, no root, empty model falls back to inherit",
			cat: []attach.SubagentCatalogInfo{
				{Name: "monitor", Modes: []string{"async"}},
			},
			wantSubstr: []string{"• monitor [model=inherit, async, no tools]"},
		},
		{
			// #768: the roster line has to settle "does this specialist
			// reach MCP at all?", which a bare count would not. Fixed
			// source order, so two lines are read against each other.
			name: "tool grant renders as a per-source breakdown",
			cat: []attach.SubagentCatalogInfo{
				{Name: "cluster", Modes: []string{"async"}, Tools: []attach.ToolInfo{
					{Name: "read_file", Source: attach.ToolSourceBuiltin},
					{Name: "gke_get_pod", Source: attach.ToolSourceMCP, Server: "gke"},
					{Name: "gke_list_clusters", Source: attach.ToolSourceMCP, Server: "gke"},
					{Name: "list_skills", Source: attach.ToolSourceSkill},
				}},
			},
			wantSubstr: []string{"• cluster [model=inherit, async, 4 tools: 1 builtin, 2 mcp, 1 skill]"},
		},
		{
			// A source the vocabulary doesn't cover (or an unset one)
			// still has to be counted, or the total stops matching the
			// breakdown and the line reads as a bug.
			name: "unclassified tools count as other",
			cat: []attach.SubagentCatalogInfo{
				{Name: "helper", Modes: []string{"async"}, Tools: []attach.ToolInfo{{Name: "spawn_agent"}}},
			},
			wantSubstr: []string{"1 tools: 1 other"},
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := formatSubagentCatalog(tc.cat)
			for _, s := range tc.wantSubstr {
				if !strings.Contains(got, s) {
					t.Errorf("formatSubagentCatalog: missing %q; got:\n%s", s, got)
				}
			}
		})
	}
}
