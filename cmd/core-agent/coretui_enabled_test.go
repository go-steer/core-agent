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

package main

import (
	"errors"
	"testing"

	"google.golang.org/genai"
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
// AsyncSlashProviderWithPreamble surfaces at dispatch (core-tui
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
