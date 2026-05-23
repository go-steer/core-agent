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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-steer/core-agent/attach"
)

func TestUsagePanel_Render(t *testing.T) {
	t.Parallel()
	u := usagePanel{modelName: "gemini-3.1-pro", inTokens: 12400, outTokens: 1900}
	got := u.render(120, false, "")
	for _, want := range []string{"gemini-3.1-pro", "in 12.4K", "out 1.9K", "$"} {
		if !strings.Contains(got, want) {
			t.Errorf("render missing %q: %s", want, got)
		}
	}
}

func TestUsagePanel_Render_Reconnect(t *testing.T) {
	t.Parallel()
	u := usagePanel{inTokens: 100}
	got := u.render(80, true, "")
	if !strings.Contains(got, "reconnecting") {
		t.Errorf("reconnect indicator missing: %s", got)
	}
}

func TestUsagePanel_Ingest(t *testing.T) {
	t.Parallel()
	u := usagePanel{}
	// Top-level keys.
	u.ingest(map[string]any{"input_tokens": 100, "output_tokens": 50})
	if u.inTokens != 100 || u.outTokens != 50 {
		t.Errorf("top-level ingest: in=%d out=%d", u.inTokens, u.outTokens)
	}
	// Nested under "usage".
	u.ingest(map[string]any{"usage": map[string]any{"input_tokens": 200, "output_tokens": 25}})
	if u.inTokens != 300 || u.outTokens != 75 {
		t.Errorf("nested ingest: in=%d out=%d", u.inTokens, u.outTokens)
	}
	// float64 (JSON-decoded numbers).
	u.ingest(map[string]any{"input_tokens": float64(10)})
	if u.inTokens != 310 {
		t.Errorf("float64 ingest: in=%d", u.inTokens)
	}
}

func TestSITokens(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   int
		want string
	}{
		{0, "0"},
		{500, "500"},
		{1500, "1.5K"},
		{12483, "12.5K"},
		{1_400_000, "1.4M"},
	}
	for _, c := range cases {
		if got := siTokens(c.in); got != c.want {
			t.Errorf("siTokens(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRenderToolsList(t *testing.T) {
	t.Parallel()
	out := renderToolsList([]attach.ToolInfo{
		{Name: "read_file", Source: "builtin", GateState: "allowed", Description: "read a file"},
		{Name: "kube_get", Source: "mcp", Server: "kube-mcp", GateState: "prompted"},
	})
	for _, want := range []string{"read_file", "builtin", "allowed", "kube_get", "mcp", "prompted"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q: %s", want, out)
		}
	}
}

func TestRenderToolsList_Empty(t *testing.T) {
	t.Parallel()
	out := renderToolsList(nil)
	if !strings.Contains(out, "no tools") {
		t.Errorf("expected 'no tools' marker: %s", out)
	}
}

func TestWriteTranscript(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "transcript.md")
	events := []chatEvent{
		{kind: "user", body: "hi"},
		{kind: "asst", body: "hello there"},
		{kind: "tool", meta: "fetch_url(github.com/...) → 200 OK"},
	}
	if err := writeTranscript(path, events); err != nil {
		t.Fatalf("writeTranscript: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	for _, want := range []string{"**user:**", "**asst:**", "hello there", "tool: fetch_url"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("missing %q in transcript: %s", want, data)
		}
	}
}
