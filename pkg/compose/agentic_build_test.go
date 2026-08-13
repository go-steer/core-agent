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

package compose

import (
	"slices"
	"strings"
	"testing"

	adktool "google.golang.org/adk/tool"

	"github.com/go-steer/core-agent/v2/pkg/agent"
)

// stubBuiltinTool stands in for a tool.Build product. BuildAgenticTools
// only reads Name(), so the rest is inert.
type stubBuiltinTool struct{ name string }

func (s stubBuiltinTool) Name() string          { return s.name }
func (s stubBuiltinTool) Description() string   { return "stub for tests" }
func (s stubBuiltinTool) IsLongRunning() bool   { return false }
func (s stubBuiltinTool) Confirmation() *string { return nil }

func stubTools(names ...string) []adktool.Tool {
	out := make([]adktool.Tool, 0, len(names))
	for _, n := range names {
		out = append(out, stubBuiltinTool{name: n})
	}
	return out
}

// BuildAgenticTools' whole job is deciding which wrappers are safe to
// register given what survived the tool gate. Both directions of a
// wrong answer are quiet: register agentic_research when glob was
// disabled and the subtask runs with a degraded kit while the parent
// is told it can research; skip a wrapper whose inner tool IS present
// and the operator silently loses the context saving they turned
// --agentic-tools on for.
func TestBuildAgenticTools_SelectionMatrix(t *testing.T) {
	t.Parallel()

	const full = "read_file,fetch_url,grep,list_dir,glob"
	cases := []struct {
		name    string
		builtin []string
		want    []string
	}{
		{
			name:    "full read-only kit registers all four wrappers",
			builtin: strings.Split(full, ","),
			want:    []string{"agentic_read_file", "agentic_fetch_url", "agentic_grep", "agentic_research"},
		},
		{
			// url_scope.allow empty → fetch_url never built. Everything
			// else must still be wrapped.
			name:    "no fetch_url",
			builtin: []string{"read_file", "grep", "list_dir", "glob"},
			want:    []string{"agentic_read_file", "agentic_grep", "agentic_research"},
		},
		{
			// research needs the complete investigation kit; one
			// missing member drops the wrapper rather than shipping a
			// subtask that can't glob.
			name:    "glob disabled drops research only",
			builtin: []string{"read_file", "fetch_url", "grep", "list_dir"},
			want:    []string{"agentic_read_file", "agentic_fetch_url", "agentic_grep"},
		},
		{
			name:    "list_dir disabled drops research only",
			builtin: []string{"read_file", "fetch_url", "grep", "glob"},
			want:    []string{"agentic_read_file", "agentic_fetch_url", "agentic_grep"},
		},
		{
			name:    "grep alone still gets its wrapper",
			builtin: []string{"grep"},
			want:    []string{"agentic_grep"},
		},
		{
			name:    "read_file alone",
			builtin: []string{"read_file"},
			want:    []string{"agentic_read_file"},
		},
		{
			// Tools the wrappers don't care about must not change the
			// selection — a write tool in the catalog is not a reason
			// to register or withhold a read-only wrapper.
			name:    "unrelated tools are ignored",
			builtin: []string{"grep", "write_file", "bash", "record_plan"},
			want:    []string{"agentic_grep"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := BuildAgenticTools(stubTools(tc.builtin...), func() *agent.Agent { return nil }, nil, "")
			if err != nil {
				t.Fatalf("BuildAgenticTools: %v", err)
			}
			names := make([]string, 0, len(got))
			for _, tool := range got {
				names = append(names, tool.Name())
			}
			if !slices.Equal(names, tc.want) {
				t.Errorf("wrappers = %v, want %v", names, tc.want)
			}
		})
	}
}

func TestBuildAgenticTools_NoUsableInnerToolIsAnError(t *testing.T) {
	t.Parallel()

	// --agentic-tools with every wrappable tool gated off is an
	// operator mistake, not a silent no-op: returning an empty slice
	// would boot a daemon that reports agentic tools enabled and
	// registers none.
	cases := [][]string{
		nil,
		{"write_file", "bash"},
		{"list_dir", "glob"}, // enough for research's kit, but research also needs read_file+grep
	}
	for _, builtin := range cases {
		got, err := BuildAgenticTools(stubTools(builtin...), func() *agent.Agent { return nil }, nil, "")
		if err == nil {
			t.Errorf("builtin=%v: got %d wrappers and no error, want an error", builtin, len(got))
			continue
		}
		if got != nil {
			t.Errorf("builtin=%v: wrappers = %v on error, want nil", builtin, got)
		}
		// The message has to name what WAS available, or the operator
		// can't tell which flag gated the tool away.
		if len(builtin) > 0 && !strings.Contains(err.Error(), builtin[0]) {
			t.Errorf("builtin=%v: error %q doesn't list the available tools", builtin, err)
		}
	}
}

func TestBuildAgenticTools_GrepWrapperTakesReadFileWhenAvailable(t *testing.T) {
	t.Parallel()

	// The grep subtask is handed read_file as a second inner tool so
	// it can pull surrounding context. That's a behavioural
	// difference the wrapper name doesn't reveal, so pin that both
	// shapes construct — grep-only must not error just because
	// read_file is absent.
	for _, builtin := range [][]string{{"grep"}, {"grep", "read_file"}} {
		got, err := BuildAgenticTools(stubTools(builtin...), func() *agent.Agent { return nil }, nil, "")
		if err != nil {
			t.Fatalf("builtin=%v: %v", builtin, err)
		}
		var haveGrep bool
		for _, tool := range got {
			if tool.Name() == "agentic_grep" {
				haveGrep = true
			}
		}
		if !haveGrep {
			t.Errorf("builtin=%v: agentic_grep missing", builtin)
		}
	}
}
