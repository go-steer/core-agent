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

package mcp

import (
	"strings"
	"testing"

	"google.golang.org/adk/tool"
	"google.golang.org/genai"
)

// describedTool carries a declaration description, which plainTool
// deliberately does not — and carries a DIFFERENT one from its
// Description(), because the gap between those two is the whole
// hazard ToolNotes has to clear. Description() feeds `/tools` and the
// `/mcp` listing; Declaration().Description is what gets serialized
// into the request. A note that reached only the first would fix the
// listing a human reads and leave the model exactly where #1016 found
// it.
type describedTool struct {
	name     string
	desc     string // Tool.Description() — the human listing
	declDesc string // Declaration().Description — what the model reads
}

func (d describedTool) Name() string        { return d.name }
func (d describedTool) Description() string { return d.desc }
func (d describedTool) IsLongRunning() bool { return false }
func (d describedTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{Name: d.name, Description: d.declDesc}
}
func (d describedTool) Run(tool.Context, any) (map[string]any, error) {
	return map[string]any{"ok": true}, nil
}

const fidelityNote = "outputFormat selects FIDELITY, not rendering."

// gkeish is the shape of the server #1016 is about: two tools, one of
// which the operator has something to say about.
func gkeish() tool.Toolset {
	return hintToolset{name: "gke", tools: []tool.Tool{
		describedTool{name: "get_k8s_resource", desc: "listing text", declDesc: "Get a Kubernetes resource."},
		describedTool{name: "list_k8s_events", desc: "listing text", declDesc: "List events."},
	}}
}

func declOf(t *testing.T, tl tool.Tool) *genai.FunctionDeclaration {
	t.Helper()
	rn, ok := tl.(runnable)
	if !ok {
		t.Fatalf("%T is not runnable", tl)
	}
	d := rn.Declaration()
	if d == nil {
		t.Fatalf("nil declaration for %s", tl.Name())
	}
	return d
}

// TestToolNotes_ReachesTheModelOnBothWrapPaths is the headline. Both
// compositions are covered for the reason #693 learned the hard way:
// the digest wrap is the DEFAULT path, so a feature that only reached
// withNamespace would be dead on every shipped deployment while its
// unit test stayed green.
func TestToolNotes_ReachesTheModelOnBothWrapPaths(t *testing.T) {
	t.Parallel()
	notes := map[string]string{"get_k8s_resource": fidelityNote}

	paths := map[string]tool.Toolset{
		"namespace only": withNamespace(gkeish(), "gke", true, notes),
		"digest wrapped": withNamespaceAndDigest(gkeish(), "gke", "gke", &DigestOptions{}, true, notes),
	}
	for name, ts := range paths {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			tools := toolsOf(t, ts)
			got := declOf(t, tools["gke_get_k8s_resource"]).Description

			if !strings.Contains(got, fidelityNote) {
				t.Errorf("declaration description does not carry the note.\ngot: %q", got)
			}
			// Appended, not substituted: the server's own text is the
			// only account of what the tool does, and a note is by
			// definition something it left out.
			if !strings.HasPrefix(got, "Get a Kubernetes resource.") {
				t.Errorf("note replaced the upstream description instead of appending.\ngot: %q", got)
			}
		})
	}
}

// TestToolNotes_OnlyTheNamedToolGetsIt guards the direction this can be
// wrong that costs the most: a note stamped onto every tool from the
// server would put a claim about outputFormat on tools that have no
// such parameter.
func TestToolNotes_OnlyTheNamedToolGetsIt(t *testing.T) {
	t.Parallel()
	ts := withNamespaceAndDigest(gkeish(), "gke", "gke", &DigestOptions{}, true,
		map[string]string{"get_k8s_resource": fidelityNote})
	tools := toolsOf(t, ts)

	if got := declOf(t, tools["gke_list_k8s_events"]).Description; got != "List events." {
		t.Errorf("unnamed tool's description changed: got %q, want %q", got, "List events.")
	}
}

// TestToolNotes_KeyedOnTheUpstreamName pins which name an operator
// writes. The prefix is the mcp.json server key, so repeating it in
// every entry would be noise that drifts the moment the key is
// renamed — and a key that silently matched BOTH forms would make the
// unmatched-key warning useless.
func TestToolNotes_KeyedOnTheUpstreamName(t *testing.T) {
	t.Parallel()
	ts := withNamespaceAndDigest(gkeish(), "gke", "gke", &DigestOptions{}, true,
		map[string]string{"gke_get_k8s_resource": fidelityNote})
	tools := toolsOf(t, ts)

	if got := declOf(t, tools["gke_get_k8s_resource"]).Description; strings.Contains(got, fidelityNote) {
		t.Errorf("a prefixed key should not match; got %q", got)
	}
}

// TestToolNotes_AbsentNoteChangesNothing. Every MCP server in the wild
// has no tool_notes at all, so the no-note path is the one that must
// be byte-identical to what shipped before.
func TestToolNotes_AbsentNoteChangesNothing(t *testing.T) {
	t.Parallel()
	for name, notes := range map[string]map[string]string{
		"nil map":      nil,
		"empty map":    {},
		"other tool":   {"list_k8s_events": "irrelevant"},
		"missing tool": {"no_such_tool": "irrelevant"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ts := withNamespaceAndDigest(gkeish(), "gke", "gke", &DigestOptions{}, true, notes)
			tools := toolsOf(t, ts)
			if got := declOf(t, tools["gke_get_k8s_resource"]).Description; got != "Get a Kubernetes resource." {
				t.Errorf("description changed with no note for this tool: %q", got)
			}
		})
	}
}

// TestToolNotes_ReachesTheHumanListingToo. Declaration() is what the
// model reads and Description() is what `/tools` and `/mcp` print; an
// operator debugging why the model behaved a certain way reads the
// listing, so the two must not disagree about what the tool's
// description is.
func TestToolNotes_ReachesTheHumanListingToo(t *testing.T) {
	t.Parallel()
	ts := withNamespaceAndDigest(gkeish(), "gke", "gke", &DigestOptions{}, true,
		map[string]string{"get_k8s_resource": fidelityNote})
	tools := toolsOf(t, ts)

	got := tools["gke_get_k8s_resource"].Description()
	if !strings.Contains(got, fidelityNote) {
		t.Errorf("Description() does not carry the note: %q", got)
	}
	if !strings.HasPrefix(got, "listing text") {
		t.Errorf("Description() lost its upstream text: %q", got)
	}
}

func TestWithNote(t *testing.T) {
	t.Parallel()
	cases := []struct{ desc, note, want string }{
		{"upstream", "note", "upstream\n\nnote"},
		{"upstream", "", "upstream"},
		// A server that documents nothing is the case a note helps
		// most; it must not arrive with a leading blank line.
		{"", "note", "note"},
		{"", "", ""},
	}
	for _, tc := range cases {
		if got := withNote(tc.desc, tc.note); got != tc.want {
			t.Errorf("withNote(%q, %q) = %q, want %q", tc.desc, tc.note, got, tc.want)
		}
	}
}

// TestUnmatchedToolNotes is the feature's own smoke alarm. A note that
// attaches to nothing fails by doing nothing, which is precisely the
// failure #1016 exists to stop happening silently.
func TestUnmatchedToolNotes(t *testing.T) {
	t.Parallel()
	exposed := []string{"gke_get_k8s_resource", "gke_list_k8s_events"}

	t.Run("all matched is silent", func(t *testing.T) {
		t.Parallel()
		if got := unmatchedToolNotes(map[string]string{"get_k8s_resource": "x"}, "gke", exposed); got != nil {
			t.Errorf("want no warning, got %v", got)
		}
	})
	t.Run("no notes is silent", func(t *testing.T) {
		t.Parallel()
		if got := unmatchedToolNotes(nil, "gke", exposed); got != nil {
			t.Errorf("want no warning, got %v", got)
		}
	})
	t.Run("a typo is named, with what was available", func(t *testing.T) {
		t.Parallel()
		got := unmatchedToolNotes(map[string]string{"get_k8s_resrouce": "x"}, "gke", exposed)
		if len(got) != 1 {
			t.Fatalf("want 1 warning, got %v", got)
		}
		// Naming the bad key is half an error message; the reader
		// needs the list to spot the transposition.
		if !strings.Contains(got[0], "get_k8s_resrouce") {
			t.Errorf("warning does not name the unmatched key: %q", got[0])
		}
		for _, name := range exposed {
			if !strings.Contains(got[0], name) {
				t.Errorf("warning does not list exposed tool %q: %q", name, got[0])
			}
		}
	})
	t.Run("a prefixed key is reported, not silently accepted", func(t *testing.T) {
		t.Parallel()
		// The likeliest operator mistake, because the prefixed name is
		// the one they see in transcripts and in `/mcp`.
		got := unmatchedToolNotes(map[string]string{"gke_get_k8s_resource": "x"}, "gke", exposed)
		if len(got) != 1 {
			t.Fatalf("want 1 warning, got %v", got)
		}
	})
	t.Run("several unmatched keys report in a stable order", func(t *testing.T) {
		t.Parallel()
		got := unmatchedToolNotes(map[string]string{"zeta": "x", "alpha": "y"}, "gke", exposed)
		if len(got) != 1 {
			t.Fatalf("want 1 warning, got %v", got)
		}
		// Map iteration order is randomized; an unstable warning would
		// be a flaky diff in every log an operator compares.
		if !strings.Contains(got[0], "alpha, zeta") {
			t.Errorf("unmatched keys are not sorted: %q", got[0])
		}
	})
}

func TestWarnings_PrefixesTheServerName(t *testing.T) {
	t.Parallel()
	got := Warnings([]*Server{
		nil,
		{Name: "gke", Warnings: []string{"bad note"}},
		{Name: "quiet"},
	})
	if len(got) != 1 {
		t.Fatalf("want 1 warning, got %v", got)
	}
	// Without the server name a multi-server daemon's stderr says a
	// note is wrong and not which file to open.
	if got[0] != "gke: bad note" {
		t.Errorf("got %q, want %q", got[0], "gke: bad note")
	}
}

func TestServerSpec_Validate_ToolNotes(t *testing.T) {
	t.Parallel()
	base := func(notes map[string]string) ServerSpec {
		return ServerSpec{Transport: "http", URL: "https://example.test", ToolNotes: notes}
	}
	t.Run("a real note validates", func(t *testing.T) {
		t.Parallel()
		if err := base(map[string]string{"get_k8s_resource": "x"}).Validate("gke"); err != nil {
			t.Errorf("Validate: %v", err)
		}
	})
	t.Run("an empty note is a config nobody finished writing", func(t *testing.T) {
		t.Parallel()
		if err := base(map[string]string{"get_k8s_resource": "  "}).Validate("gke"); err == nil {
			t.Error("want an error for a blank note")
		}
	})
	t.Run("an empty key cannot match anything", func(t *testing.T) {
		t.Parallel()
		if err := base(map[string]string{"": "x"}).Validate("gke"); err == nil {
			t.Error("want an error for a blank tool name")
		}
	})
}
