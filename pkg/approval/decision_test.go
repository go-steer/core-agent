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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"google.golang.org/genai"
)

func (p *gateProbe) decisions(t *testing.T) []Decision {
	t.Helper()
	var out []Decision
	for k, v := range p.state {
		if !strings.HasPrefix(k, DecisionStateKeyPrefix) {
			continue
		}
		d, err := DecodeDecision(v)
		if err != nil {
			t.Fatalf("state[%q] is not a decision record: %v", k, err)
		}
		out = append(out, d)
	}
	return out
}

func (p *gateProbe) onlyDecision(t *testing.T) Decision {
	t.Helper()
	got := p.decisions(t)
	if len(got) != 1 {
		t.Fatalf("decision records = %+v, want exactly one", got)
	}
	return got[0]
}

func wantDecision(t *testing.T, got Decision, want Decision) {
	t.Helper()
	if got.Outcome != want.Outcome {
		t.Errorf("Outcome = %q, want %q", got.Outcome, want.Outcome)
	}
	if got.Disposition != want.Disposition {
		t.Errorf("Disposition = %q, want %q", got.Disposition, want.Disposition)
	}
	if got.Refusal != want.Refusal {
		t.Errorf("Refusal = %q, want %q", got.Refusal, want.Refusal)
	}
	if got.Authority != want.Authority {
		t.Errorf("Authority = %q, want %q", got.Authority, want.Authority)
	}
	if got.Approver != want.Approver {
		t.Errorf("Approver = %q, want %q", got.Approver, want.Approver)
	}
}

func TestDecisionRecordedOnApprove(t *testing.T) {
	p := runGateProbe(t, gateProbeConfig{
		policy:  OnMutationRequireApproval,
		respond: approve(map[string]any{"verdict": "approve", "note": "expected", "approver": "user:sre-oncall"}),
	})
	if len(p.executions) != 1 {
		t.Fatalf("tool executed %d time(s), want 1: %+v", len(p.executions), p.executions)
	}
	d := p.onlyDecision(t)
	wantDecision(t, d, Decision{
		Outcome:     OutcomeApprove,
		Disposition: DispositionAuthorized,
		Authority:   AuthorityVerdict,
		Approver:    "user:sre-oncall",
	})
	if d.Tool != "scale_deployment" {
		t.Errorf("Tool = %q, want scale_deployment", d.Tool)
	}
	if d.Note != "expected" {
		t.Errorf("Note = %q, want the operator's note", d.Note)
	}
	if d.Session != sid {
		t.Errorf("Session = %q, want %q", d.Session, sid)
	}
	if d.DecidedAt.IsZero() {
		t.Error("DecidedAt is zero")
	}
	if d.ProposedKey == "" || d.ProposedArgs["deployment"] != "api" {
		t.Errorf("ProposedKey/ProposedArgs = %q/%v, want the model's call", d.ProposedKey, d.ProposedArgs)
	}
	if d.Edited() {
		t.Errorf("Edited() = true on a plain approval (executed key %q)", d.ExecutedKey)
	}
}

func TestDecisionRecordedOnReject(t *testing.T) {
	p := runGateProbe(t, gateProbeConfig{
		policy: OnMutationRequireApproval,
		respond: func(confID string) *genai.Content {
			return verdictResponse(confID, map[string]any{
				"confirmed": false,
				"payload": map[string]any{
					"verdict":  "reject",
					"note":     "we are not scaling into an OOM",
					"approver": "user:sre-oncall",
				},
			})
		},
	})
	if len(p.executions) != 0 {
		t.Fatalf("tool executed %d time(s) after a rejection: %+v", len(p.executions), p.executions)
	}
	d := p.onlyDecision(t)
	wantDecision(t, d, Decision{
		Outcome:     OutcomeReject,
		Disposition: DispositionRefusedByOperator,
		Refusal:     "denied_by_operator",
		Authority:   AuthorityVerdict,
		Approver:    "user:sre-oncall",
	})
	if d.Note != "we are not scaling into an OOM" {
		t.Errorf("Note = %q, want the operator's reason", d.Note)
	}
	if d.ProposedArgs["deployment"] != "api" {
		t.Errorf("ProposedArgs = %v, want the call that was refused", d.ProposedArgs)
	}
}

func TestDecisionRecordedOnEdit(t *testing.T) {
	p := runGateProbe(t, gateProbeConfig{
		policy: OnMutationRequireApproval,
		respond: approve(map[string]any{
			"verdict":  "edit",
			"args":     map[string]any{"deployment": "api", "replicas": 2},
			"note":     "10 would exhaust the node pool",
			"approver": "user:sre-oncall",
		}),
	})
	if len(p.executions) != 1 || p.executions[0].Replicas != 2 {
		t.Fatalf("executions = %+v, want one call with the operator's 2 replicas", p.executions)
	}
	d := p.onlyDecision(t)
	wantDecision(t, d, Decision{
		Outcome:     OutcomeEdit,
		Disposition: DispositionAuthorized,
		Authority:   AuthorityVerdict,
		Approver:    "user:sre-oncall",
	})
	if !d.Edited() {
		t.Fatalf("Edited() = false on an edit verdict: proposed %q, executed %q", d.ProposedKey, d.ExecutedKey)
	}
	if !strings.Contains(d.ProposedKey, "replicas=10") {
		t.Errorf("ProposedKey = %q, want the model's 10 replicas", d.ProposedKey)
	}
	if !strings.Contains(d.ExecutedKey, "replicas=2") {
		t.Errorf("ExecutedKey = %q, want the operator's 2 replicas", d.ExecutedKey)
	}
	if got := d.ProposedArgs["replicas"]; got != float64(10) {
		t.Errorf("ProposedArgs[replicas] = %v (%T), want the model's 10", got, got)
	}
	if got := d.ExecutedArgs["replicas"]; got != float64(2) {
		t.Errorf("ExecutedArgs[replicas] = %v (%T), want the operator's 2", got, got)
	}
}

func TestDecisionRecordsMastsOwnRefusals(t *testing.T) {
	t.Run("unattributed edit", func(t *testing.T) {
		p := runGateProbe(t, gateProbeConfig{
			policy: OnMutationRequireApproval,
			respond: approve(map[string]any{
				"verdict": "edit",
				"args":    map[string]any{"deployment": "api", "replicas": 2},
			}),
		})
		wantDecision(t, p.onlyDecision(t), Decision{
			Outcome:     OutcomeEdit,
			Disposition: DispositionRefusedByMast,
			Refusal:     "edit_unattributed",
			Authority:   AuthorityVerdict,
		})
	})

	t.Run("malformed verdict", func(t *testing.T) {
		p := runGateProbe(t, gateProbeConfig{
			policy: OnMutationRequireApproval,
			respond: func(confID string) *genai.Content {
				return verdictResponse(confID, map[string]any{
					"confirmed": false,
					"payload":   map[string]any{"verdict": "approve"},
				})
			},
		})
		d := p.onlyDecision(t)
		wantDecision(t, d, Decision{
			Disposition: DispositionRefusedByMast,
			Refusal:     "malformed_verdict",
			Authority:   AuthorityVerdict,
		})
		if d.Outcome != "" {
			t.Errorf("Outcome = %q, want empty", d.Outcome)
		}
	})
}

func TestAppliedEditSurfaceIsUnchanged(t *testing.T) {
	p := runGateProbe(t, gateProbeConfig{
		policy: OnMutationRequireApproval,
		respond: approve(map[string]any{
			"verdict":  "edit",
			"args":     map[string]any{"deployment": "api", "replicas": 2},
			"note":     "10 would exhaust the node pool",
			"approver": "user:sre-oncall",
		}),
	})
	edits := p.appliedEdits(t)
	if len(edits) != 1 {
		t.Fatalf("applied-edit records = %+v, want exactly one", edits)
	}
	e := edits[0]
	if e.Tool != "scale_deployment" || e.Approver != "user:sre-oncall" || e.Note != "10 would exhaust the node pool" {
		t.Errorf("AppliedEdit = %+v, want the W2.5 record unchanged", e)
	}
	if !strings.Contains(e.ProposedKey, "replicas=10") || !strings.Contains(e.ExecutedKey, "replicas=2") {
		t.Errorf("AppliedEdit keys = %q -> %q, want the W2.5 record unchanged", e.ProposedKey, e.ExecutedKey)
	}
	if e.Approver == RedactApprover(e.Approver) {
		t.Error("the applied-edit record must keep the raw approver")
	}
	if len(p.decisions(t)) != 1 {
		t.Error("the edit produced no decision record beside its applied-edit record")
	}
}

func TestRedactApprover(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, in, want string
	}{
		{"empty stays empty", "", ""},
		{
			"a person is digested",
			"user:sre-oncall",
			"sha256:" + digestOf(t, "user:sre-oncall"),
		},
		{
			"a proxied person is digested whole",
			"alice@example.com (asserted by svc-gateway)",
			"sha256:" + digestOf(t, "alice@example.com (asserted by svc-gateway)"),
		},
		{
			"an in-process mechanism passes through",
			"mast:internal",
			"mast:internal",
		},
		{"any mast: identity passes through", "mast:scheduler", "mast:scheduler"},
		{"any core-agent: identity passes through", "core-agent:internal", "core-agent:internal"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := RedactApprover(tt.in); got != tt.want {
				t.Errorf("RedactApprover(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}

	t.Run("distinct people get distinct digests", func(t *testing.T) {
		t.Parallel()
		if RedactApprover("user:a") == RedactApprover("user:b") {
			t.Error("two operators digested the same")
		}
	})
	t.Run("the digest is not reversible to the identity", func(t *testing.T) {
		t.Parallel()
		if got := RedactApprover("user:sre-oncall"); strings.Contains(got, "sre-oncall") {
			t.Errorf("RedactApprover leaked the identity: %q", got)
		}
	})
}

func digestOf(t *testing.T, s string) string {
	t.Helper()
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:16]
}

func TestDecisionRoundTrip(t *testing.T) {
	t.Parallel()
	want := Decision{
		DecidedAt:      time.Date(2026, 8, 16, 9, 0, 0, 0, time.UTC),
		Session:        "s-1",
		Workload:       "gke-triage",
		Specialist:     "remediator",
		FunctionCallID: "fc-1",
		Tool:           "scale_deployment",
		Outcome:        OutcomeEdit,
		Scope:          ScopeOnce,
		Authority:      AuthorityVerdict,
		Disposition:    DispositionAuthorized,
		ProposedKey:    "scale_deployment(replicas=10)",
		ProposedArgs:   map[string]any{"replicas": float64(10)},
		ExecutedKey:    "scale_deployment(replicas=2)",
		ExecutedArgs:   map[string]any{"replicas": float64(2)},
		Approver:       "user:sre-oncall",
		Note:           "too many",
	}
	raw, err := EncodeDecision(want)
	if err != nil {
		t.Fatalf("EncodeDecision: %v", err)
	}
	for _, form := range []any{raw, []byte(raw), decodedMap(t, raw)} {
		got, err := DecodeDecision(form)
		if err != nil {
			t.Fatalf("DecodeDecision(%T): %v", form, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("DecodeDecision(%T) = %+v, want %+v", form, got, want)
		}
	}
	if _, err := DecodeDecision("not json"); err == nil {
		t.Error("DecodeDecision accepted a non-record")
	}
}

func decodedMap(t *testing.T, raw string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return m
}
