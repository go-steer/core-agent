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

package approval

import (
	"fmt"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
)

func catalog() ChangeSetChecker {
	return ChangeSetChecker{
		Declares: func(name string) bool { return name == "scale_deployment" },
		Schema: func(name string) (*jsonschema.Schema, error) {
			if name != "scale_deployment" {
				return nil, fmt.Errorf("tool %q is not wired in this daemon", name)
			}
			return mcpShaped(), nil
		},
	}
}

func TestParseChangeSet(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		report  map[string]any
		want    []ProposedChange
		wantErr string
	}{
		{
			name:   "no change-set field at all",
			report: map[string]any{"detail": "nothing to do"},
		},
		{
			name:   "explicitly null",
			report: map[string]any{ChangeSetField: nil},
		},
		{
			name:   "empty list",
			report: map[string]any{ChangeSetField: []any{}},
			want:   []ProposedChange{},
		},
		{
			name: "arguments as a JSON string — the wire form",
			report: map[string]any{ChangeSetField: []any{
				map[string]any{"tool": "scale_deployment", "arguments": `{"deployment":"api","replicas":2}`},
			}},
			want: []ProposedChange{{Tool: "scale_deployment", Arguments: map[string]any{
				"deployment": "api", "replicas": float64(2),
			}}},
		},
		{
			name: "arguments as an object",
			report: map[string]any{ChangeSetField: []any{
				map[string]any{"tool": "scale_deployment", "arguments": map[string]any{"deployment": "api"}},
			}},
			want: []ProposedChange{{Tool: "scale_deployment", Arguments: map[string]any{"deployment": "api"}}},
		},
		{
			name: "a tool that takes no arguments",
			report: map[string]any{ChangeSetField: []any{
				map[string]any{"tool": "drain_node", "arguments": "{}"},
			}},
			want: []ProposedChange{{Tool: "drain_node", Arguments: map[string]any{}}},
		},
		{
			name: "arguments omitted entirely",
			report: map[string]any{ChangeSetField: []any{
				map[string]any{"tool": "drain_node"},
			}},
			want: []ProposedChange{{Tool: "drain_node"}},
		},
		{
			name:    "not a list",
			report:  map[string]any{ChangeSetField: "scale the deployment"},
			wantErr: "not a list",
		},
		{
			name: "arguments that are not an object",
			report: map[string]any{ChangeSetField: []any{
				map[string]any{"tool": "scale_deployment", "arguments": "replicas=2"},
			}},
			wantErr: "not a JSON object",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseChangeSet(tt.report)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("ParseChangeSet = %+v, want an error mentioning %q", got, tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error %q does not mention %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseChangeSet: %v", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("ParseChangeSet = %+v, want %+v", got, tt.want)
			}
			for i := range got {
				if got[i].Tool != tt.want[i].Tool {
					t.Errorf("[%d].Tool = %q, want %q", i, got[i].Tool, tt.want[i].Tool)
				}
				if len(got[i].Arguments) != len(tt.want[i].Arguments) {
					t.Fatalf("[%d].Arguments = %v, want %v", i, got[i].Arguments, tt.want[i].Arguments)
				}
				for k, v := range tt.want[i].Arguments {
					if got[i].Arguments[k] != v {
						t.Errorf("[%d].Arguments[%q] = %#v, want %#v", i, k, got[i].Arguments[k], v)
					}
				}
			}
		})
	}
}

func TestChangeSetCheck(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		changes []ProposedChange
		wantErr string
	}{
		{
			name:    "no changes at all",
			changes: nil,
		},
		{
			name: "a declared tool with valid arguments",
			changes: []ProposedChange{{Tool: "scale_deployment", Arguments: map[string]any{
				"deployment": "api", "replicas": 2,
			}}},
		},
		{
			name:    "a tool the catalog does not declare",
			changes: []ProposedChange{{Tool: "kubectl_scale", Arguments: map[string]any{"deployment": "api"}}},
			wantErr: `does not declare`,
		},
		{
			name:    "an entry naming nothing",
			changes: []ProposedChange{{Tool: "  ", Arguments: map[string]any{}}},
			wantErr: "names no tool",
		},
		{
			name: "arguments the tool's schema rejects",
			changes: []ProposedChange{{Tool: "scale_deployment", Arguments: map[string]any{
				"deployment": "api", "replicas": "two",
			}}},
			wantErr: "input schema",
		},
		{
			name:    "a required argument missing",
			changes: []ProposedChange{{Tool: "scale_deployment", Arguments: map[string]any{"replicas": 2}}},
			wantErr: "input schema",
		},
		{
			name: "an argument the tool does not declare",
			changes: []ProposedChange{{Tool: "scale_deployment", Arguments: map[string]any{
				"deployment": "api", "namespace": "prod",
			}}},
			wantErr: "does not declare",
		},
		{
			name: "the second entry is the bad one",
			changes: []ProposedChange{
				{Tool: "scale_deployment", Arguments: map[string]any{"deployment": "api"}},
				{Tool: "rm_minus_rf", Arguments: map[string]any{}},
			},
			wantErr: ChangeSetField + "[1]",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := catalog().Check(tt.changes)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("Check = %+v, want an error mentioning %q", got, tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error %q does not mention %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Check: %v", err)
			}
			if len(got) != len(tt.changes) {
				t.Fatalf("Check returned %d entries, want %d", len(got), len(tt.changes))
			}
			for _, ch := range got {
				if v, ok := ch.Arguments["replicas"]; ok {
					if _, isFloat := v.(float64); !isFloat {
						t.Errorf("replicas is %T, want the JSON-normalized float64", v)
					}
				}
			}
		})
	}
}

func TestChangeSetCheckRefusesAnUnwiredTool(t *testing.T) {
	t.Parallel()
	c := ChangeSetChecker{
		Declares: func(string) bool { return true },
		Schema: func(name string) (*jsonschema.Schema, error) {
			return nil, fmt.Errorf("tool %q is declared by the workload but not wired in this daemon", name)
		},
	}
	_, err := c.Check([]ProposedChange{{Tool: "ghost", Arguments: map[string]any{}}})
	if err == nil {
		t.Fatal("a change naming an unwired tool was accepted")
	}
	if !strings.Contains(err.Error(), "not wired") {
		t.Errorf("error %q does not carry the resolver's own reason", err)
	}
}

func TestChangeSetCheckNeedsBothHalves(t *testing.T) {
	t.Parallel()
	only := ChangeSetChecker{Declares: func(string) bool { return true }}
	if _, err := only.Check(nil); err == nil {
		t.Fatal("a checker with no Schema accepted a change set")
	}
	other := ChangeSetChecker{Schema: func(string) (*jsonschema.Schema, error) { return mcpShaped(), nil }}
	if _, err := other.Check(nil); err == nil {
		t.Fatal("a checker with no Declares accepted a change set")
	}
}

func TestSignatureIsByteStable(t *testing.T) {
	t.Parallel()
	a, err := Signature("scale_deployment", map[string]any{"deployment": "api", "replicas": 2, "zone": "us-c1"})
	if err != nil {
		t.Fatalf("Signature: %v", err)
	}
	b, err := Signature("scale_deployment", map[string]any{"zone": "us-c1", "replicas": 2, "deployment": "api"})
	if err != nil {
		t.Fatalf("Signature: %v", err)
	}
	if a != b {
		t.Fatalf("signatures differ by argument order:\n  %s\n  %s", a, b)
	}

	long := strings.Repeat("x", 200)
	c, _ := Signature("apply", map[string]any{"manifest": long + "a"})
	d, _ := Signature("apply", map[string]any{"manifest": long + "b"})
	if c == d {
		t.Fatal("two different manifests share a signature")
	}

	empty, err := Signature("drain_node", nil)
	if err != nil {
		t.Fatalf("Signature: %v", err)
	}
	if empty != "drain_node({})" {
		t.Errorf("Signature with no arguments = %q", empty)
	}

	if _, err := Signature("apply", map[string]any{"ch": make(chan int)}); err == nil {
		t.Error("an unencodable argument produced a signature")
	}
}

func TestChangeSetRoundTrip(t *testing.T) {
	t.Parallel()
	changes := []ProposedChange{
		{Tool: "scale_deployment", Arguments: map[string]any{"deployment": "api", "replicas": float64(2)}},
		{Tool: "drain_node", Arguments: map[string]any{}},
	}
	raw, err := EncodeChangeSet(changes)
	if err != nil {
		t.Fatalf("EncodeChangeSet: %v", err)
	}

	for _, stored := range []any{raw, []byte(raw), []any{
		map[string]any{"tool": "scale_deployment", "arguments": map[string]any{"deployment": "api", "replicas": float64(2)}},
		map[string]any{"tool": "drain_node", "arguments": map[string]any{}},
	}} {
		got, err := DecodeChangeSet(stored)
		if err != nil {
			t.Fatalf("DecodeChangeSet(%T): %v", stored, err)
		}
		if len(got) != 2 || got[0].Tool != "scale_deployment" || got[1].Tool != "drain_node" {
			t.Fatalf("DecodeChangeSet(%T) = %+v", stored, got)
		}
		want, _ := changes[0].Signature()
		gotSig, _ := got[0].Signature()
		if gotSig != want {
			t.Errorf("DecodeChangeSet(%T) signature = %q, want %q", stored, gotSig, want)
		}
	}

	if got, err := DecodeChangeSet(nil); err != nil || got != nil {
		t.Errorf("DecodeChangeSet(nil) = %v, %v; want nil, nil", got, err)
	}
	if got, err := DecodeChangeSet("  "); err != nil || got != nil {
		t.Errorf("DecodeChangeSet(blank) = %v, %v; want nil, nil", got, err)
	}
	if _, err := DecodeChangeSet(`{"tool":"x"}`); err == nil {
		t.Error("a record that is not a change set decoded cleanly")
	}
}

func TestChangeSetStateKeyIsPerSpecialist(t *testing.T) {
	t.Parallel()
	if ChangeSetStateKey("OOMKilled") == ChangeSetStateKey("Evicted") {
		t.Fatal("two specialists share a change-set key")
	}
	if !strings.HasPrefix(ChangeSetStateKey("OOMKilled"), ChangeSetStateKeyPrefix) {
		t.Error("the key is not under the documented prefix")
	}
}

func TestDescribeChangeSet(t *testing.T) {
	t.Parallel()
	got := DescribeChangeSet([]ProposedChange{
		{Tool: "scale_deployment", Arguments: map[string]any{"deployment": "api"}},
		{Tool: "drain_node"},
	})
	want := "1. scale_deployment({\"deployment\":\"api\"})\n2. drain_node({})"
	if got != want {
		t.Errorf("DescribeChangeSet =\n%s\nwant\n%s", got, want)
	}
	if DescribeChangeSet(nil) != "" {
		t.Errorf("an empty change set renders as %q, want nothing", DescribeChangeSet(nil))
	}
}
