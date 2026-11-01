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
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"
)

type declaredTool struct {
	name string
	decl *genai.FunctionDeclaration
}

func (d declaredTool) Name() string                            { return d.name }
func (d declaredTool) Description() string                     { return "" }
func (d declaredTool) IsLongRunning() bool                     { return false }
func (d declaredTool) Declaration() *genai.FunctionDeclaration { return d.decl }

type undeclaredTool struct{ name string }

func (u undeclaredTool) Name() string        { return u.name }
func (u undeclaredTool) Description() string { return "" }
func (u undeclaredTool) IsLongRunning() bool { return false }

func mcpShaped() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type:     "object",
		Required: []string{"deployment"},
		Properties: map[string]*jsonschema.Schema{
			"deployment": {Type: "string"},
			"replicas":   {Type: "integer"},
		},
	}
}

func mcpTool() tool.Tool {
	return declaredTool{name: "scale_deployment", decl: &genai.FunctionDeclaration{
		Name:                 "scale_deployment",
		ParametersJsonSchema: mcpShaped(),
	}}
}

func TestNormalizeEdit(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		tool    tool.Tool
		args    map[string]any
		wantErr string
	}{
		{
			name: "valid edit",
			tool: mcpTool(),
			args: map[string]any{"deployment": "api", "replicas": 2},
		},
		{
			name:    "undeclared argument the schema itself would allow",
			tool:    mcpTool(),
			args:    map[string]any{"deployment": "api", "namespace": "prod"},
			wantErr: "does not declare",
		},
		{
			name:    "wrong type",
			tool:    mcpTool(),
			args:    map[string]any{"deployment": "api", "replicas": "two"},
			wantErr: "input schema",
		},
		{
			name:    "missing a required argument",
			tool:    mcpTool(),
			args:    map[string]any{"replicas": 2},
			wantErr: "input schema",
		},
		{
			name:    "empty edit",
			tool:    mcpTool(),
			args:    map[string]any{},
			wantErr: "carries no arguments",
		},
		{
			name:    "tool with no declaration at all",
			tool:    undeclaredTool{name: "opaque"},
			args:    map[string]any{"deployment": "api"},
			wantErr: "does not declare its arguments",
		},
		{
			name:    "tool that declares no parameters",
			tool:    declaredTool{name: "bare", decl: &genai.FunctionDeclaration{Name: "bare"}},
			args:    map[string]any{"deployment": "api"},
			wantErr: "declares no input schema",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := normalizeEdit(tt.tool, tt.args)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("normalizeEdit = %v, want an error mentioning %q", got, tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error %q does not mention %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizeEdit: %v", err)
			}
			if _, ok := got["replicas"].(float64); !ok {
				t.Errorf("replicas is %T, want the JSON-normalized float64", got["replicas"])
			}
		})
	}
}

func TestApplyArgsReplacesInPlace(t *testing.T) {
	t.Parallel()
	args := map[string]any{"deployment": "api", "replicas": 10.0}
	live := args
	applyArgs(args, map[string]any{"deployment": "api", "replicas": 2.0})
	if live["replicas"] != 2.0 {
		t.Errorf("live map has replicas=%v, want 2", live["replicas"])
	}
	applyArgs(args, map[string]any{"deployment": "api"})
	if _, ok := live["replicas"]; ok {
		t.Error("an argument the operator dropped survived the rewrite")
	}
}

func TestDecodeAppliedEdit(t *testing.T) {
	t.Parallel()
	want := AppliedEdit{
		Tool:         "scale_deployment",
		Approver:     "user:sre-oncall",
		ProposedKey:  "scale_deployment(deployment=api, replicas=10)",
		ExecutedKey:  "scale_deployment(deployment=api, replicas=2)",
		ProposedArgs: map[string]any{"replicas": 10.0},
		ExecutedArgs: map[string]any{"replicas": 2.0},
		Note:         "node pool is too small",
	}
	raw, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var asMap any
	if err := json.Unmarshal(raw, &asMap); err != nil {
		t.Fatal(err)
	}
	for name, v := range map[string]any{"string": string(raw), "map": asMap} {
		t.Run(name, func(t *testing.T) {
			got, err := DecodeAppliedEdit(v)
			if err != nil {
				t.Fatalf("DecodeAppliedEdit: %v", err)
			}
			if got.ExecutedKey != want.ExecutedKey || got.Approver != want.Approver {
				t.Errorf("record = %+v, want %+v", got, want)
			}
			if !strings.Contains(got.String(), want.Approver) {
				t.Errorf("String() = %q, want it to name the approver", got.String())
			}
		})
	}
}

func TestDecodeAppliedEditRejectsNonRecords(t *testing.T) {
	t.Parallel()
	for _, v := range []any{"not json", []byte("{"), 7} {
		if _, err := DecodeAppliedEdit(v); err == nil {
			t.Errorf("DecodeAppliedEdit(%#v) succeeded, want an error", v)
		}
	}
}
