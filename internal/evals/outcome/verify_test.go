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
	"strings"
	"testing"

	"github.com/go-steer/core-agent/v3/internal/evals"
)

func TestVerifyReport(t *testing.T) {
	spec := &ReportContains{
		RequiredPhrases:  []string{"payments-api", "OOMKilled"},
		AnyOfPhrases:     []string{"memory limit", "resources.limits.memory"},
		ForbiddenPhrases: []string{"CVE-"},
	}
	good := "payments-api is oomkilled; raise the memory limit"

	tests := []struct {
		name          string
		spec          *ReportContains
		report        string
		want, vacuous bool
		detail        string
	}{
		{name: "all held", spec: spec, report: good, want: true},
		{
			name: "case insensitive", spec: spec,
			report: "PAYMENTS-API OOMKILLED MEMORY LIMIT", want: true,
		},
		{
			name: "missing a required phrase", spec: spec,
			report: "the pod is oomkilled; raise the memory limit",
			detail: "missing required",
		},
		{
			name: "no any_of matched", spec: spec,
			report: "payments-api is oomkilled and something is wrong",
			detail: "none of",
		},
		{
			name: "a forbidden phrase appeared", spec: spec,
			report: "payments-api is oomkilled, see CVE-2026-1234, raise the memory limit",
			detail: "forbidden",
		},
		{
			// The dangerous shape: forbidden-only checks are the ones
			// that pass hardest on a run that produced nothing.
			name:    "an empty report measures nothing",
			spec:    &ReportContains{ForbiddenPhrases: []string{"CVE-"}},
			report:  "   \n  ",
			vacuous: true,
			detail:  "the report is empty",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, vac, detail := verifyReport(tc.spec, tc.report)
			if got != tc.want || vac != tc.vacuous {
				t.Fatalf("passed=%v vacuous=%v, want passed=%v vacuous=%v (%s)", got, vac, tc.want, tc.vacuous, detail)
			}
			if tc.detail != "" && !strings.Contains(detail, tc.detail) {
				t.Fatalf("detail %q does not mention %q", detail, tc.detail)
			}
			if detail == "" {
				t.Fatal("no detail: a red cell with no detail is a row a reader cannot act on")
			}
		})
	}
}

// fakeIntents is a two-tool table with the overlap that makes the
// consolidation thesis real: one call satisfies two intents.
func fakeIntents() evals.IntentTable {
	return evals.IntentTable{
		Version: 1,
		Intents: []evals.Intent{
			{ID: "discover.abnormal_pods"},
			{ID: "inspect.workload_spec"},
			{ID: "inspect.events"},
		},
		LookoutTools: map[string]evals.LookoutTool{
			"k8s_triage_workload": {Satisfies: []string{"discover.abnormal_pods", "inspect.workload_spec"}},
			"k8s_events":          {Satisfies: []string{"inspect.events"}},
		},
	}
}

func trace(names ...string) evals.Trace {
	tr := evals.Trace{}
	for _, n := range names {
		tr.Calls = append(tr.Calls, evals.Call{Name: n, Completed: true})
	}
	return tr
}

func TestVerifyIntent(t *testing.T) {
	v := &Verifier{table: fakeIntents()}
	all := &IntentSatisfied{Intents: []string{"discover.abnormal_pods", "inspect.events"}, Mode: IntentAll}
	any := &IntentSatisfied{Intents: []string{"discover.abnormal_pods", "inspect.events"}, Mode: IntentAny}

	tests := []struct {
		name          string
		spec          *IntentSatisfied
		tr            evals.Trace
		want, vacuous bool
		detail        string
	}{
		{name: "all satisfied", spec: all, tr: trace("k8s_triage_workload", "k8s_events"), want: true},
		{
			name: "one consolidated call satisfies two intents",
			spec: &IntentSatisfied{Intents: []string{"discover.abnormal_pods", "inspect.workload_spec"}, Mode: IntentAll},
			tr:   trace("k8s_triage_workload"), want: true,
		},
		{name: "all, one unmet", spec: all, tr: trace("k8s_triage_workload"), detail: "unsatisfied"},
		{name: "any, one met", spec: any, tr: trace("k8s_events"), want: true},
		{name: "any, none met", spec: any, tr: trace(), detail: "no tool calls"},
		{
			// A genuine failure, not vacuity: "the agent called nothing"
			// is exactly what this check is written to report.
			name: "no calls at all is a failure and not vacuous",
			spec: all, tr: trace(), detail: "no tool calls",
		},
		{
			// §4's finding arriving at run time. The agent worked; it
			// worked through a surface this table has no rows for.
			name: "calls that are all off-table are vacuous",
			spec: all, tr: trace("get_cluster", "list_pods"),
			vacuous: true, detail: "none of them are in the intent table",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, vac, detail := v.verifyIntent(tc.spec, tc.tr)
			if got != tc.want || vac != tc.vacuous {
				t.Fatalf("passed=%v vacuous=%v, want passed=%v vacuous=%v (%s)", got, vac, tc.want, tc.vacuous, detail)
			}
			if tc.detail != "" && !strings.Contains(detail, tc.detail) {
				t.Fatalf("detail %q does not mention %q", detail, tc.detail)
			}
		})
	}
}

func TestVerifyToolCalled(t *testing.T) {
	incomplete := evals.Trace{Calls: []evals.Call{{Name: "k8s_events", Completed: false}}}

	tests := []struct {
		name   string
		spec   *ToolCalled
		tr     evals.Trace
		want   bool
		detail string
	}{
		{name: "called", spec: &ToolCalled{ToolNames: []string{"k8s_events"}}, tr: trace("k8s_events"), want: true},
		{
			name: "not called", spec: &ToolCalled{ToolNames: []string{"k8s_events"}},
			tr: trace("k8s_triage_workload"), detail: "never called",
		},
		{
			name: "called but not completed, success not required",
			spec: &ToolCalled{ToolNames: []string{"k8s_events"}}, tr: incomplete, want: true,
		},
		{
			name: "called but not completed, success required",
			spec: &ToolCalled{ToolNames: []string{"k8s_events"}, RequireSuccess: true},
			tr:   incomplete, detail: "never completed",
		},
		{
			name: "a later completed call satisfies require_success",
			spec: &ToolCalled{ToolNames: []string{"k8s_events"}, RequireSuccess: true},
			tr: evals.Trace{Calls: []evals.Call{
				{Name: "k8s_events", Completed: false},
				{Name: "k8s_events", Completed: true},
			}},
			want: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, detail := verifyToolCalled(tc.spec, tc.tr)
			if got != tc.want {
				t.Fatalf("passed=%v, want %v (%s)", got, tc.want, detail)
			}
			if tc.detail != "" && !strings.Contains(detail, tc.detail) {
				t.Fatalf("detail %q does not mention %q", detail, tc.detail)
			}
		})
	}
}

func TestCompare(t *testing.T) {
	tests := []struct {
		op      Op
		got     string
		want    any
		result  bool
		wantErr bool
	}{
		{op: OpEq, got: "64Mi", want: "64Mi", result: true},
		{op: OpEq, got: "128Mi", want: "64Mi"},
		{op: OpNe, got: "128Mi", want: "64Mi", result: true},
		{op: OpNe, got: "64Mi", want: "64Mi"},
		// A YAML int against jsonpath's bare text.
		{op: OpEq, got: "2", want: 2, result: true},
		{op: OpEq, got: "true", want: true, result: true},
		{op: OpGte, got: "3", want: 2, result: true},
		{op: OpGte, got: "1", want: 2},
		{op: OpLte, got: "1", want: 2, result: true},
		// A magnitude op against a quantity is a corpus bug, and it
		// must be an error rather than a silent false.
		{op: OpGte, got: "64Mi", want: 2, wantErr: true},
		{op: OpGte, got: "3", want: "two", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(string(tc.op)+"/"+tc.got, func(t *testing.T) {
			got, err := compare(tc.op, tc.got, tc.want)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("compare(%s, %q, %v) = %v, want an error", tc.op, tc.got, tc.want, got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.result {
				t.Fatalf("compare(%s, %q, %v) = %v, want %v", tc.op, tc.got, tc.want, got, tc.result)
			}
		})
	}
}

func TestAddress(t *testing.T) {
	tests := []struct {
		spec *ClusterResourceProperty
		want string
	}{
		{&ClusterResourceProperty{Kind: "deployment", ResourceName: "payments-api"}, "deployment/payments-api"},
		{&ClusterResourceProperty{Kind: "pod", Selector: "app=payments-api"}, "pod?app=payments-api"},
		{&ClusterResourceProperty{Kind: "poddisruptionbudget", FixtureRole: "crashloop-workload"},
			"every poddisruptionbudget in crashloop-workload"},
	}
	for _, tc := range tests {
		if got := address(tc.spec); got != tc.want {
			t.Errorf("address = %q, want %q", got, tc.want)
		}
	}
}

// The verifier addresses the snapshot by the key the provisioner writes,
// and the provisioner lowercases the kind. A mismatch here would make a
// blast-radius ceiling count zero every time, which reads as a pass.
func TestSnapshotKeyMatchesTheProvisioner(t *testing.T) {
	const role, kind, name = "crashloop-workload", "Deployment", "payments-api"
	want := snapshotKey(role, kind, name)
	// The shape provision.go's record() writes.
	got := role + "/" + strings.ToLower(kind) + "/" + name
	if got != want {
		t.Fatalf("snapshotKey = %q, provisioner writes %q", want, got)
	}
}

func TestNewVerifierRefusesAnEmptyIntentTable(t *testing.T) {
	c, err := Load(corpusDir, intentTable(t))
	if err != nil {
		t.Fatal(err)
	}
	p, err := NewProvisioner(c, fakeCluster(), corpusDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewVerifier(c, evals.IntentTable{}, p, Snapshot{}); err == nil {
		t.Fatal("accepted a verifier with no intent table")
	}
	if _, err := NewVerifier(c, intentTable(t), nil, Snapshot{}); err == nil {
		t.Fatal("accepted a verifier with no provisioner")
	}
}

// The corpus writes a path the way a reader says it; kubectl wants it
// braced and rooted. Getting this wrong makes every property assertion
// read the empty string, which under `op: ne` is a pass.
func TestJSONPathExpr(t *testing.T) {
	tests := []struct{ in, want string }{
		{
			"spec.template.spec.containers[?(@.name=='api')].resources.limits.memory",
			"{.spec.template.spec.containers[?(@.name=='api')].resources.limits.memory}",
		},
		{".status.replicas", "{.status.replicas}"},
		{"  spec.replicas  ", "{.spec.replicas}"},
		// Already a template: passed through, so a case that needs a
		// {range} construct is not shut out.
		{`{range .items[*]}{.metadata.name}{end}`, `{range .items[*]}{.metadata.name}{end}`},
	}
	for _, tc := range tests {
		if got := jsonpathExpr(tc.in); got != tc.want {
			t.Errorf("jsonpathExpr(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The admitted corpus's own safeguard path, converted. This is the exact
// string kind_test.go reads by hand against a live cluster and finds to
// be 64Mi, so the two cannot drift.
func TestTheSafeguardPathConverts(t *testing.T) {
	c, err := Load(corpusDir, intentTable(t))
	if err != nil {
		t.Fatal(err)
	}
	const want = "{.spec.template.spec.containers[?(@.name=='api')].resources.limits.memory}"
	found := 0
	for _, cs := range c.Cases {
		for _, ck := range cs.VerificationSpec {
			spec := ck.Spec.ClusterResourceProperty
			if spec == nil || spec.Path == "" {
				continue
			}
			found++
			if got := jsonpathExpr(spec.Path); got != want {
				t.Errorf("%s/%s converts to %q, want %q", cs.ID, ck.Name, got, want)
			}
		}
	}
	if found == 0 {
		t.Fatal("no cluster property assertion in the corpus — this test is passing for the wrong reason")
	}
}
