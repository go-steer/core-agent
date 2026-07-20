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

package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/adk/tool"

	"github.com/go-steer/core-agent/v2/pkg/config"
	"github.com/go-steer/core-agent/v2/pkg/permissions"
)

const sampleK8sJSON = `{
  "kind": "PodList",
  "items": [
    {"metadata": {"name": "alpha", "namespace": "default"}, "status": {"phase": "Running"}},
    {"metadata": {"name": "beta",  "namespace": "default"}, "status": {"phase": "Pending"}},
    {"metadata": {"name": "gamma", "namespace": "kube-system"}, "status": {"phase": "Running"}}
  ]
}`

func TestJSONQuery_InlineJSON_BasicSelect(t *testing.T) {
	t.Parallel()
	fn := jsonQueryFunc(gateFor(t, t.TempDir()), config.DefaultConfig())
	res, err := fn(tool.Context(nil), jsonQueryArgs{
		JSON:  sampleK8sJSON,
		Query: ".items[].metadata.name",
	})
	if err != nil {
		t.Fatalf("json_query: %v", err)
	}
	if res.Count != 3 {
		t.Errorf("Count = %d, want 3", res.Count)
	}
	wantNames := []string{`"alpha"`, `"beta"`, `"gamma"`}
	for i, w := range wantNames {
		if string(res.Results[i]) != w {
			t.Errorf("Results[%d] = %s, want %s", i, res.Results[i], w)
		}
	}
}

func TestJSONQuery_FilterAndProject(t *testing.T) {
	t.Parallel()
	fn := jsonQueryFunc(gateFor(t, t.TempDir()), config.DefaultConfig())
	res, err := fn(tool.Context(nil), jsonQueryArgs{
		JSON:  sampleK8sJSON,
		Query: `.items[] | select(.status.phase == "Running") | .metadata.name`,
	})
	if err != nil {
		t.Fatalf("json_query: %v", err)
	}
	if res.Count != 2 {
		t.Errorf("Count = %d, want 2 (alpha + gamma)", res.Count)
	}
}

func TestJSONQuery_FromFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "pods.json")
	if err := os.WriteFile(path, []byte(sampleK8sJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	fn := jsonQueryFunc(gateFor(t, dir), config.DefaultConfig())
	res, err := fn(tool.Context(nil), jsonQueryArgs{
		Path:  path,
		Query: "length",
	})
	if err != nil {
		t.Fatalf("json_query: %v", err)
	}
	if res.Count != 1 || string(res.Results[0]) != "2" {
		t.Errorf("expected length=2 (PodList has 2 top-level keys), got %+v", res)
	}
}

func TestJSONQuery_RejectsBothPathAndJSON(t *testing.T) {
	t.Parallel()
	fn := jsonQueryFunc(gateFor(t, t.TempDir()), config.DefaultConfig())
	_, err := fn(tool.Context(nil), jsonQueryArgs{
		Path:  "/tmp/x.json",
		JSON:  "{}",
		Query: ".",
	})
	if err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Errorf("expected exactly-one error, got %v", err)
	}
}

func TestJSONQuery_RejectsNeither(t *testing.T) {
	t.Parallel()
	fn := jsonQueryFunc(gateFor(t, t.TempDir()), config.DefaultConfig())
	_, err := fn(tool.Context(nil), jsonQueryArgs{Query: "."})
	if err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Errorf("expected exactly-one error, got %v", err)
	}
}

func TestJSONQuery_RejectsEmptyQuery(t *testing.T) {
	t.Parallel()
	fn := jsonQueryFunc(gateFor(t, t.TempDir()), config.DefaultConfig())
	_, err := fn(tool.Context(nil), jsonQueryArgs{JSON: "{}"})
	if err == nil || !strings.Contains(err.Error(), "query is required") {
		t.Errorf("expected missing-query error, got %v", err)
	}
}

func TestJSONQuery_MalformedJSON(t *testing.T) {
	t.Parallel()
	fn := jsonQueryFunc(gateFor(t, t.TempDir()), config.DefaultConfig())
	_, err := fn(tool.Context(nil), jsonQueryArgs{JSON: "not json", Query: "."})
	if err == nil || !strings.Contains(err.Error(), "parse input") {
		t.Errorf("expected parse-input error, got %v", err)
	}
}

func TestJSONQuery_BadQueryExpression(t *testing.T) {
	t.Parallel()
	fn := jsonQueryFunc(gateFor(t, t.TempDir()), config.DefaultConfig())
	_, err := fn(tool.Context(nil), jsonQueryArgs{JSON: "{}", Query: "not a valid (((expr"})
	if err == nil || !strings.Contains(err.Error(), "parse query") {
		t.Errorf("expected parse-query error, got %v", err)
	}
}

func TestJSONQuery_EvalErrorSurfaces(t *testing.T) {
	t.Parallel()
	fn := jsonQueryFunc(gateFor(t, t.TempDir()), config.DefaultConfig())
	// `.foo / .bar` against ints both equal to zero is a jq runtime error
	// (division by zero); surfaced as a Go error so the model sees the
	// failure rather than getting empty results.
	_, err := fn(tool.Context(nil), jsonQueryArgs{
		JSON:  `{"foo": 0, "bar": 0}`,
		Query: ".foo / .bar",
	})
	if err == nil || !strings.Contains(err.Error(), "eval") {
		t.Errorf("expected eval-error, got %v", err)
	}
}

func TestJSONQuery_OutOfScope_Denied(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	other := t.TempDir()
	outside := filepath.Join(other, "pods.json")
	if err := os.WriteFile(outside, []byte(sampleK8sJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	// Build a non-yolo gate scoped to dir only — same shape as
	// TestReadFile_OutOfScope_Denied.
	scope, _ := permissions.NewPathScope(dir, "", nil)
	gate := permissions.New(permissions.Options{
		Mode:  permissions.ModeAllow, // no allowlist match → deny
		Scope: scope,
	})
	fn := jsonQueryFunc(gate, config.DefaultConfig())
	_, err := fn(tool.Context(nil), jsonQueryArgs{
		Path:  outside,
		Query: ".",
	})
	if err == nil {
		t.Fatalf("expected denial for out-of-scope read")
	}
}
