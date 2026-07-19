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

package agentenv

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// mkLookup builds a lookup fn backed by a map, matching os.LookupEnv's
// (value, ok) contract. Empty-string set values return (v="", ok=true)
// so tests can distinguish "explicitly empty" from "unset."
func mkLookup(m map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		v, ok := m[name]
		return v, ok
	}
}

func TestNewResolverNilManifest(t *testing.T) {
	t.Parallel()
	r := NewResolver(nil, mkLookup(nil))
	if r != nil {
		t.Fatalf("NewResolver(nil, _) = %+v; want nil", r)
	}
	// Nil resolver methods are safe.
	if got := r.Interpolate("hello ${env:X}"); got != "hello ${env:X}" {
		t.Errorf("nil.Interpolate should be a no-op; got %q", got)
	}
	if r.IsSensitive("X") {
		t.Error("nil.IsSensitive should return false")
	}
	if errs := r.Errors(); errs != nil {
		t.Errorf("nil.Errors = %v; want nil", errs)
	}
	if r.InterpolateFunc() != nil {
		t.Error("nil.InterpolateFunc should return nil")
	}
}

func TestResolverRequiredMissing(t *testing.T) {
	t.Parallel()
	m := &Manifest{Env: []Entry{
		{Name: "GCP_PROJECT", Required: true, Description: "project id"},
		{Name: "OPTIONAL", Default: "def"},
	}}
	r := NewResolver(m, mkLookup(map[string]string{}))
	errs := r.Errors()
	if len(errs) != 1 {
		t.Fatalf("Errors = %v; want exactly one", errs)
	}
	if !strings.Contains(errs[0].Error(), "GCP_PROJECT") ||
		!strings.Contains(errs[0].Error(), "project id") {
		t.Errorf("error should name the var and its description; got %v", errs[0])
	}
	// Even with the error, the resolver still functions; interpolation
	// of the missing required var yields empty string (not the literal
	// placeholder, which would leak into the prompt).
	if got := r.Interpolate("value=${env:GCP_PROJECT}"); got != "value=" {
		t.Errorf("Interpolate missing required = %q; want value=", got)
	}
}

func TestResolverOptionalDefault(t *testing.T) {
	t.Parallel()
	m := &Manifest{Env: []Entry{
		{Name: "ONCALL", Default: "sre@example.com"},
		{Name: "MISSING_NO_DEFAULT"},
	}}
	r := NewResolver(m, mkLookup(map[string]string{}))
	if errs := r.Errors(); len(errs) != 0 {
		t.Errorf("unexpected errors for optional-only manifest: %v", errs)
	}
	if got := r.Interpolate("${env:ONCALL}"); got != "sre@example.com" {
		t.Errorf("default not applied: got %q", got)
	}
	if got := r.Interpolate("${env:MISSING_NO_DEFAULT}"); got != "" {
		t.Errorf("optional-no-default should resolve to empty; got %q", got)
	}
}

func TestResolverOverridesEnvSet(t *testing.T) {
	t.Parallel()
	m := &Manifest{Env: []Entry{
		{Name: "GCP_PROJECT", Required: true, Default: "should-not-be-used"},
	}}
	r := NewResolver(m, mkLookup(map[string]string{"GCP_PROJECT": "actual-project"}))
	if errs := r.Errors(); len(errs) != 0 {
		t.Errorf("unexpected errors: %v", errs)
	}
	if got := r.Interpolate("${env:GCP_PROJECT}"); got != "actual-project" {
		t.Errorf("set value should win over default; got %q", got)
	}
}

func TestResolverSensitive(t *testing.T) {
	t.Parallel()
	m := &Manifest{Env: []Entry{
		{Name: "TOKEN", Required: true, Sensitive: true},
		{Name: "PROJECT", Required: true},
	}}
	r := NewResolver(m, mkLookup(map[string]string{"TOKEN": "sekret", "PROJECT": "demo"}))

	if !r.IsSensitive("TOKEN") {
		t.Error("TOKEN should be sensitive")
	}
	if r.IsSensitive("PROJECT") {
		t.Error("PROJECT should not be sensitive")
	}
	if r.IsSensitive("UNKNOWN") {
		t.Error("unknown vars should not be sensitive")
	}
	sv := r.SensitiveValues()
	if !reflect.DeepEqual(sv, []string{"sekret"}) {
		t.Errorf("SensitiveValues = %v; want [sekret]", sv)
	}
}

func TestResolverInterpolateFallsThroughToOSGetenv(t *testing.T) {
	// Undeclared var that IS set in the process environment falls back
	// to the ambient env. Useful when a bundle references e.g. $HOME
	// without declaring it — resolves correctly, gets a drift warning.
	// t.Setenv is incompatible with t.Parallel by design.
	t.Setenv("AGENTENV_TEST_AMBIENT", "from-os")
	m := &Manifest{Env: []Entry{{Name: "DECLARED", Default: "d"}}}
	r := NewResolver(m, mkLookup(nil))

	got := r.Interpolate("${env:DECLARED}/${env:AGENTENV_TEST_AMBIENT}")
	if got != "d/from-os" {
		t.Errorf("mixed declared + ambient = %q; want d/from-os", got)
	}
}

func TestResolverReportDrift(t *testing.T) {
	t.Parallel()
	m := &Manifest{Env: []Entry{
		{Name: "USED"},
		{Name: "UNREFERENCED"},
	}}
	r := NewResolver(m, mkLookup(nil))

	// Simulate interpolation over bundle files.
	_ = r.Interpolate("hello ${env:USED} world")
	_ = r.Interpolate("${env:UNDECLARED_BUT_REFERENCED} is a common typo")

	warnings := r.ReportDrift()
	joined := strings.Join(warnings, "\n")

	if !strings.Contains(joined, "UNDECLARED_BUT_REFERENCED") {
		t.Errorf("expected undeclared-reference warning; got %v", warnings)
	}
	if !strings.Contains(joined, "UNREFERENCED") {
		t.Errorf("expected unreferenced-declaration warning; got %v", warnings)
	}
	if strings.Contains(joined, "\"USED\"") {
		// USED was interpolated AND declared — shouldn't appear on
		// either list.
		t.Errorf("USED should not appear in drift; got %v", warnings)
	}
}

func TestResolverIntegrationEndToEnd(t *testing.T) {
	// Simulates the daemon-side load path: parse manifest from disk,
	// build resolver, interpolate bundle contents, check drift.
	// t.Setenv is incompatible with t.Parallel by design.
	t.Setenv("GCP_PROJECT", "demo-project")
	t.Setenv("GKE_CLUSTER", "demo-cluster")
	t.Setenv("GKE_LOCATION", "us-central1")

	agentsDir := t.TempDir()
	manifest := `version: 1
env:
  - name: GCP_PROJECT
    required: true
    description: GCP project
  - name: GKE_CLUSTER
    required: true
    description: cluster name
  - name: GKE_LOCATION
    required: true
    description: cluster region
`
	if err := os.WriteFile(filepath.Join(agentsDir, ManifestFileYAML), []byte(manifest), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	m, err := LoadManifest(agentsDir)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	r := NewResolver(m, os.LookupEnv)
	if errs := r.Errors(); len(errs) != 0 {
		t.Fatalf("required-var errors: %v", errs)
	}

	agentsMD := "GCP project ${env:GCP_PROJECT} cluster ${env:GKE_CLUSTER} in ${env:GKE_LOCATION}."
	got := r.Interpolate(agentsMD)
	want := "GCP project demo-project cluster demo-cluster in us-central1."
	if got != want {
		t.Errorf("interpolated =\n%s\nwant:\n%s", got, want)
	}
	if warnings := r.ReportDrift(); len(warnings) != 0 {
		t.Errorf("unexpected drift warnings: %v", warnings)
	}
}
