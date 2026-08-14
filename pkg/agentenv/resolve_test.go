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
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
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

// TestResolverInterpolateConcurrent is the #371 regression guard: the
// same Resolver's InterpolateFunc is shared across sessions, so
// concurrent Interpolate calls (plus a concurrent ReportDrift reader)
// mutate/range seenRefs from multiple goroutines. Before the mutex this
// tripped `go test -race` (and could panic with "concurrent map writes"
// under load). Run this suite with -race to enforce the fix.
func TestResolverInterpolateConcurrent(t *testing.T) {
	t.Parallel()
	m := &Manifest{Env: []Entry{{Name: "DECLARED", Default: "d"}}}
	r := NewResolver(m, mkLookup(nil))

	const goroutines = 64
	var wg sync.WaitGroup

	// Concurrent writers: each hits a distinct undeclared name so
	// seenRefs takes writes for many different keys at once.
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = r.Interpolate(fmt.Sprintf("${env:DECLARED}-${env:VAR_%d}", i))
		}(i)
	}

	// Concurrent readers ranging over seenRefs via ReportDrift.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < goroutines; j++ {
				_ = r.ReportDrift()
			}
		}()
	}

	wg.Wait()

	// After the storm, every undeclared VAR_* must have been recorded so
	// ReportDrift can flag them — proves writes weren't lost to the lock.
	warnings := r.ReportDrift()
	joined := strings.Join(warnings, "\n")
	for i := 0; i < goroutines; i++ {
		if !strings.Contains(joined, fmt.Sprintf("VAR_%d", i)) {
			t.Errorf("VAR_%d not recorded in seenRefs; drift = %v", i, warnings)
		}
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

// TestReportDriftConfigRefCountsAsReferenced is the regression guard for
// the false positive observed live on 2026-08-14: a var declared in
// .agents/env.yaml and consumed ONLY by an alerts.targets[].url_env
// field was reported as "nothing in the bundle references it", because
// config never flows through Interpolate. NoteConfigRefs is the second
// reference channel; a name reached through it must not be called
// unreferenced.
func TestReportDriftConfigRefCountsAsReferenced(t *testing.T) {
	t.Parallel()
	m := &Manifest{Env: []Entry{
		{Name: "PLATFORM_AGENT_ALERT_WEBHOOK"},
		{Name: "TRULY_UNREFERENCED"},
	}}
	r := NewResolver(m, mkLookup(nil))

	// The bundle references neither; only the config names the webhook.
	r.NoteConfigRefs([]string{"PLATFORM_AGENT_ALERT_WEBHOOK"})

	joined := strings.Join(r.ReportDrift(), "\n")
	if strings.Contains(joined, "PLATFORM_AGENT_ALERT_WEBHOOK") {
		t.Errorf("config-referenced var reported as drift; got %q", joined)
	}
	if !strings.Contains(joined, "TRULY_UNREFERENCED") {
		t.Errorf("want unreferenced warning for TRULY_UNREFERENCED; got %q", joined)
	}
}

// TestReportDriftUndeclaredConfigRefNamesTheConvention covers the other
// half of the check, which did not exist before: a *_env field naming a
// var the manifest never declares is drift too — and the warning must
// not send the operator looking for a ${env:...} that is in no file.
func TestReportDriftUndeclaredConfigRefNamesTheConvention(t *testing.T) {
	t.Parallel()
	r := NewResolver(&Manifest{Env: []Entry{{Name: "DECLARED"}}}, mkLookup(nil))
	_ = r.Interpolate("${env:DECLARED}")
	r.NoteConfigRefs([]string{"SNEAKY_TOKEN"})

	joined := strings.Join(r.ReportDrift(), "\n")
	if !strings.Contains(joined, "SNEAKY_TOKEN") {
		t.Fatalf("want a warning for the undeclared config ref; got %q", joined)
	}
	if !strings.Contains(joined, "*_env") {
		t.Errorf("warning should name the config convention, not ${env:}; got %q", joined)
	}
	if strings.Contains(joined, "${env:SNEAKY_TOKEN}") {
		t.Errorf("warning points at a bundle reference that does not exist; got %q", joined)
	}
}

// TestReportDriftBothConventionsReportsOnce guards against a name
// reached by BOTH channels producing two undeclared warnings for one var.
func TestReportDriftBothConventionsReportsOnce(t *testing.T) {
	t.Parallel()
	r := NewResolver(&Manifest{}, mkLookup(nil))
	_ = r.Interpolate("${env:SHARED}")
	r.NoteConfigRefs([]string{"SHARED"})

	warnings := r.ReportDrift()
	var n int
	for _, w := range warnings {
		if strings.Contains(w, "SHARED") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("want exactly 1 warning for SHARED, got %d: %v", n, warnings)
	}
	if !strings.Contains(strings.Join(warnings, "\n"), "${env:SHARED}") {
		t.Errorf("a name reached both ways should report as the bundle reference; got %v", warnings)
	}
}

func TestNoteConfigRefsNilSafeAndIdempotent(t *testing.T) {
	t.Parallel()
	var nilResolver *Resolver
	nilResolver.NoteConfigRefs([]string{"X"}) // must not panic

	r := NewResolver(&Manifest{Env: []Entry{{Name: "A"}}}, mkLookup(nil))
	r.NoteConfigRefs(nil)
	r.NoteConfigRefs([]string{})
	r.NoteConfigRefs([]string{"  ", ""}) // blanks are not references
	if joined := strings.Join(r.ReportDrift(), "\n"); !strings.Contains(joined, `"A"`) {
		t.Errorf("blank names should not mark anything referenced; got %q", joined)
	}
	r.NoteConfigRefs([]string{"A"})
	r.NoteConfigRefs([]string{"A"})
	if warnings := r.ReportDrift(); len(warnings) != 0 {
		t.Errorf("repeat NoteConfigRefs should be idempotent; got %v", warnings)
	}
}

// TestNoteConfigRefsConcurrent extends the #371 race guard to the second
// reference set: cfgRefs shares the mutex with seenRefs, and ReportDrift
// now ranges over both.
func TestNoteConfigRefsConcurrent(t *testing.T) {
	t.Parallel()
	r := NewResolver(&Manifest{Env: []Entry{{Name: "DECLARED", Default: "d"}}}, mkLookup(nil))

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r.NoteConfigRefs([]string{fmt.Sprintf("CFG_%d", i)})
			_ = r.Interpolate(fmt.Sprintf("${env:BUNDLE_%d}", i))
			_ = r.ReportDrift()
		}(i)
	}
	wg.Wait()

	joined := strings.Join(r.ReportDrift(), "\n")
	for i := 0; i < 32; i++ {
		if !strings.Contains(joined, fmt.Sprintf("CFG_%d", i)) {
			t.Errorf("CFG_%d lost from cfgRefs; drift = %q", i, joined)
		}
	}
}
