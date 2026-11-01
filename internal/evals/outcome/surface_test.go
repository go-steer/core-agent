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

package outcome

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// The selection is the intent table's own tool names, not a list. This
// is the property that makes it so: grow the table and the surface grows
// with it, with nothing here to update.
func TestTheSelectionIsTheIntentTable(t *testing.T) {
	tbl := intentTable(t)
	got := selectTools(tbl)
	if len(got) != len(tbl.LookoutTools) {
		t.Fatalf("selected %d tools, the table names %d", len(got), len(tbl.LookoutTools))
	}
	for _, name := range got {
		if _, ok := tbl.LookoutTools[name]; !ok {
			t.Errorf("selected %q, which the intent table does not name — the grader could not score a call to it", name)
		}
	}
	if !slices.IsSorted(got) {
		t.Errorf("the selection is not sorted: %v — it goes into a --tools= argument and into an error message, and both should be stable", got)
	}
}

// Every intent the shipped corpus names has to be reachable through the
// selection. If this fails, the tier would run and its intent checks
// would score nothing.
func TestTheShippedCorpusIsReachableThroughTheSelection(t *testing.T) {
	tbl := intentTable(t)
	corpus, err := Load(corpusDir, tbl)
	if err != nil {
		t.Fatal(err)
	}
	if bad := unreachableIntents(corpus, tbl, selectTools(tbl)); len(bad) > 0 {
		t.Errorf("unreachable through the selected surface: %v", bad)
	}
}

func TestUnreachableIntentsFindsOne(t *testing.T) {
	tbl := intentTable(t)
	corpus, err := Load(corpusDir, tbl)
	if err != nil {
		t.Fatal(err)
	}
	// A surface with nothing on it. Every intent the corpus names is
	// unreachable, and the list must be non-empty or the guard in
	// NewRunner is a no-op that reads like a check.
	bad := unreachableIntents(corpus, tbl, nil)
	if len(bad) == 0 {
		t.Fatal("an empty surface left every intent reachable — this guard cannot fire")
	}
	if !slices.Contains(bad, "inspect.workload_spec") {
		t.Errorf("got %v, want it to include inspect.workload_spec, which all three admitted cases rest on", bad)
	}
}

// A tool the intent table names that the binary does not advertise is
// the pre-flight's whole job. It is the difference between refusing here
// and shipping a check that measures the empty set.
func TestNotAdvertised(t *testing.T) {
	// The real shape of `lookout mcp --list-tools`: name, schema bytes,
	// summary, and a trailing total row whose first field is a count.
	const listed = `k8s_triage_workload           9906  bundle
k8s_cluster_health            7009  health
2 tools                      16915  advertised on every model call
`
	for _, tc := range []struct {
		name     string
		selected []string
		want     []string
	}{
		{"all advertised", []string{"k8s_cluster_health", "k8s_triage_workload"}, nil},
		{"one missing", []string{"k8s_triage_workload", "k8s_resource_spec"}, []string{"k8s_resource_spec"}},
		{"none advertised", []string{"k8s_nope"}, []string{"k8s_nope"}},
		{
			// The total row's first field is "2", which must not be
			// mistaken for a tool that satisfies a selection.
			"the total row is not a tool", []string{"2"}, []string{"2"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := notAdvertised(tc.selected, listed)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("notAdvertised = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestNewSurfaceNeedsACluster(t *testing.T) {
	if _, err := NewSurface(context.Background(), SurfaceConfig{Table: intentTable(t)}); err == nil {
		t.Fatal("a surface with no cluster was accepted; it would read whichever cluster the ambient kubeconfig names")
	}
}

// An empty table would select nothing, the model would be shown nothing,
// and every intent check would be vacuous against the empty surface.
// Refused at construction rather than discovered on the board.
func TestNewSurfaceRefusesAnEmptyIntentTable(t *testing.T) {
	_, err := NewSurface(context.Background(), SurfaceConfig{Cluster: &Cluster{}})
	if err == nil {
		t.Fatal("an empty intent table was accepted")
	}
	if !strings.Contains(err.Error(), "no lookout tools") {
		t.Errorf("error is %q, want it to name the empty selection", err)
	}
}

func TestFirstLine(t *testing.T) {
	for in, want := range map[string]string{
		"lookout v0.23.0 (commit abc)\nbuilt\n": "lookout v0.23.0 (commit abc)",
		"one line, no newline":                  "one line, no newline",
		"":                                      "",
		"\ntrailing":                            "",
	} {
		if got := firstLine(in); got != want {
			t.Errorf("firstLine(%q) = %q, want %q", in, got, want)
		}
	}
}
