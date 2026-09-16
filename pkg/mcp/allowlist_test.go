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
	"sort"
	"strings"
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/permissions"
	coretools "github.com/go-steer/core-agent/v2/pkg/tools"
)

// composedNames runs the shipped wrap over the in-memory annotated
// server and returns the model-facing names, sorted.
//
// The real server from readonlyhint_test.go is reused rather than a
// stub toolset on purpose: an allowlist that filtered a hand-rolled
// slice would prove nothing about whether the filter sits below the
// adapter, and "below the adapter" is the whole claim — a tool dropped
// above it has already been converted, named and declared.
func composedNames(t *testing.T, spec ServerSpec) ([]string, *allowlistToolset) {
	t.Helper()
	gate := permissions.New(permissions.Options{Mode: permissions.ModeYolo})
	composed, allow := wrapServerToolset(newAnnotatedToolset(t), "gke", spec, &DigestOptions{}, gate)
	tools := toolsOf(t, composed)
	names := make([]string, 0, len(tools))
	for name := range tools {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, allow
}

// TestToolAllowlist_UnlistedToolsAreNeverRegistered is the headline: a
// server exposing three tools is mounted for one of them, and the
// other two do not reach the model at all.
//
// Checked through the full wrap with a gate, because "the gate could
// have denied it" is exactly the outcome #759 rejected — a denial is a
// refusal of a promise already made, and this feature exists to not
// make the promise.
func TestToolAllowlist_UnlistedToolsAreNeverRegistered(t *testing.T) {
	t.Parallel()
	spec := ServerSpec{Transport: "http", URL: "u", Tools: []string{"get_pod"}}
	names, _ := composedNames(t, spec)

	want := []string{"gke_get_pod"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("registered %v, want %v", names, want)
	}
}

// TestToolAllowlist_AbsentMeansEveryTool pins the default in the
// direction that matters. An allowlist is one of the few config fields
// where the safe-looking reading of "unset" — expose nothing — would
// silently unmount every MCP server every existing config has.
func TestToolAllowlist_AbsentMeansEveryTool(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		tools []string
	}{
		{"nil", nil},
		{"empty", []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			spec := ServerSpec{Transport: "http", URL: "u", Tools: tc.tools}
			names, allow := composedNames(t, spec)

			want := []string{"gke_delete_pod", "gke_get_pod", "gke_legacy_thing"}
			if strings.Join(names, ",") != strings.Join(want, ",") {
				t.Errorf("registered %v, want all of %v", names, want)
			}
			if allow != nil {
				t.Error("an absent allowlist should not install a filter at all")
			}
		})
	}
}

// TestToolAllowlist_KeyedOnTheServerName guards the one mistake the
// field's shape invites. ToolNotes is keyed unprefixed and the tools
// the operator sees in /tools are prefixed, so writing the prefixed
// name here is the natural error — and because the allowlist's failure
// mode is a missing tool rather than a loud one, it would read as "the
// server didn't have it".
//
// It is a warning rather than a rejection because the same code path
// has to tolerate a server that renamed a tool between releases, but
// the warning must name both the unmatched entry and what was actually
// there, or it is half an error message.
func TestToolAllowlist_KeyedOnTheServerName(t *testing.T) {
	t.Parallel()
	spec := ServerSpec{Transport: "http", URL: "u", Tools: []string{"gke_get_pod"}}
	names, allow := composedNames(t, spec)

	if len(names) != 0 {
		t.Errorf("registered %v, want nothing — the prefixed name matches no tool the server exposes", names)
	}
	warnings := unmatchedToolAllowlist(allow)
	if len(warnings) != 1 {
		t.Fatalf("warnings = %v, want exactly one", warnings)
	}
	if !strings.Contains(warnings[0], "gke_get_pod") {
		t.Errorf("warning does not name the unmatched entry: %q", warnings[0])
	}
	if !strings.Contains(warnings[0], "get_pod") || !strings.Contains(warnings[0], "delete_pod") {
		t.Errorf("warning does not list what the server does expose: %q", warnings[0])
	}
}

// TestToolAllowlist_WarnsOnlyForNamesThatMatchedNothing keeps the
// warning from becoming noise an operator learns to ignore.
func TestToolAllowlist_WarnsOnlyForNamesThatMatchedNothing(t *testing.T) {
	t.Parallel()
	t.Run("every name matched", func(t *testing.T) {
		t.Parallel()
		spec := ServerSpec{Transport: "http", URL: "u", Tools: []string{"get_pod", "delete_pod"}}
		_, allow := composedNames(t, spec)
		if got := unmatchedToolAllowlist(allow); got != nil {
			t.Errorf("unmatchedToolAllowlist = %v, want nil", got)
		}
	})

	t.Run("one typo among good names", func(t *testing.T) {
		t.Parallel()
		spec := ServerSpec{Transport: "http", URL: "u", Tools: []string{"get_pod", "get_pdo"}}
		names, allow := composedNames(t, spec)
		if strings.Join(names, ",") != "gke_get_pod" {
			t.Errorf("registered %v, want just gke_get_pod", names)
		}
		got := unmatchedToolAllowlist(allow)
		if len(got) != 1 || !strings.Contains(got[0], "get_pdo") {
			t.Fatalf("warnings = %v, want one naming get_pdo", got)
		}
		if strings.Contains(got[0], "names 2 tool") {
			t.Errorf("warning counts the name that DID match: %q", got[0])
		}
	})

	t.Run("no allowlist configured", func(t *testing.T) {
		t.Parallel()
		if got := unmatchedToolAllowlist(nil); got != nil {
			t.Errorf("unmatchedToolAllowlist(nil) = %v, want nil", got)
		}
	})

	t.Run("Tools never ran", func(t *testing.T) {
		t.Parallel()
		// startOne skips the listing block when the server failed to
		// connect. Every entry would look unmatched against a catalog
		// nobody ever fetched, which is a warning about the wrong
		// thing entirely — the server being down.
		_, allow := withToolAllowlist(newAnnotatedToolset(t), []string{"nope"})
		if got := unmatchedToolAllowlist(allow); got != nil {
			t.Errorf("unmatchedToolAllowlist before any Tools() call = %v, want nil", got)
		}
	})
}

// TestToolAllowlist_KeepsTheLayersAboveIt checks that filtering below
// the namespace wrap does not cost the tools that survive it their
// read-only classification or their operator notes. The allowlist is
// the new innermost layer, so everything above it now sees a different
// upstream than it did.
func TestToolAllowlist_KeepsTheLayersAboveIt(t *testing.T) {
	t.Parallel()
	gate := permissions.New(permissions.Options{Mode: permissions.ModeYolo})
	spec := ServerSpec{
		Transport: "http",
		URL:       "u",
		Tools:     []string{"get_pod", "delete_pod"},
		ToolNotes: map[string]string{"get_pod": "Prefer this over kubectl."},
	}
	composed, _ := wrapServerToolset(newAnnotatedToolset(t), "gke", spec, &DigestOptions{}, gate)
	tools := toolsOf(t, composed)

	getPod := tools["gke_get_pod"]
	if getPod == nil {
		t.Fatal("gke_get_pod missing from the composed toolset")
	}
	if !coretools.IsReadOnlyTool(getPod) {
		t.Error("gke_get_pod lost its readOnlyHint through the allowlist")
	}
	if !strings.Contains(getPod.Description(), "Prefer this over kubectl.") {
		t.Errorf("gke_get_pod lost its tool note: %q", getPod.Description())
	}
	if coretools.IsReadOnlyTool(tools["gke_delete_pod"]) {
		t.Error("gke_delete_pod classified read-only")
	}
}

// TestToolAllowlist_Validate covers the two entries that can never do
// what the operator meant. `"tools": []` is deliberately NOT an error:
// it is an absent allowlist, and rejecting it would make an empty
// array mean something different from a missing key.
func TestToolAllowlist_Validate(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		tools   []string
		wantErr string
	}{
		{"empty list is fine", []string{}, ""},
		{"named tools are fine", []string{"get_pod", "delete_pod"}, ""},
		{"empty entry", []string{"get_pod", ""}, "empty tool name"},
		{"whitespace entry", []string{"  "}, "empty tool name"},
		{"duplicate entry", []string{"get_pod", "get_pod"}, `lists "get_pod" twice`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			spec := ServerSpec{Transport: "http", URL: "u", Tools: tc.tools}
			err := spec.Validate("gke")
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate = nil, want an error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("Validate = %v, want an error containing %q", err, tc.wantErr)
			}
		})
	}
}
