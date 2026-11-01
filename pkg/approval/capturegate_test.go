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
	"errors"
	"sort"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	adkagent "google.golang.org/adk/v2/agent"
)

type capturingProbe struct {
	rules *CaptureRules
	reads []string
	prior map[string]any
	err   error
}

func newGateCaptures(t *testing.T) *capturingProbe {
	t.Helper()
	p := &capturingProbe{prior: priorState()}
	p.rules = &CaptureRules{
		For: func(string) (*Capture, error) { return scaleCapture(), nil },
		Read: func(_ adkagent.Context, name string, args map[string]any) (map[string]any, error) {
			p.reads = append(p.reads, CallKey(name, args))
			if p.err != nil {
				return nil, p.err
			}
			out := map[string]any{}
			for k, v := range p.prior {
				out[k] = v
			}
			return out, nil
		},
		Schema: func(string) (*jsonschema.Schema, error) { return mcpShaped(), nil },
		Now:    fixedClock(t),
	}
	return p
}

func captureRecords(t *testing.T, state map[string]any) []CaptureRecord {
	t.Helper()
	var out []CaptureRecord
	for k, v := range state {
		if !strings.HasPrefix(k, CaptureStateKeyPrefix) {
			continue
		}
		r, err := DecodeCapture(v)
		if err != nil {
			t.Fatalf("state[%q] is not a capture record: %v", k, err)
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

func wantOneCapture(t *testing.T, state map[string]any) CaptureRecord {
	t.Helper()
	recs := captureRecords(t, state)
	if len(recs) != 1 {
		t.Fatalf("capture records = %d, want exactly 1: %+v", len(recs), recs)
	}
	return recs[0]
}

func wantRevertsTo(t *testing.T, rec CaptureRecord, replicas float64) {
	t.Helper()
	if rec.Revert == nil {
		t.Fatalf("record for %s carries no revert", rec.Key)
	}
	if got := rec.Revert.Arguments["replicas"]; got != replicas {
		t.Errorf("revert would set replicas to %v, want the prior %v", got, replicas)
	}
}

func TestCapture_OnTheApplyPath(t *testing.T) {
	caps := newGateCaptures(t)
	p := runGateProbe(t, gateProbeConfig{policy: OnMutationApply, captures: caps.rules})

	if len(p.executions) != 1 {
		t.Fatalf("tool executed %d time(s), want 1: %+v", len(p.executions), p.executions)
	}
	if len(caps.reads) != 1 {
		t.Fatalf("capture read ran %d time(s), want 1: %v", len(caps.reads), caps.reads)
	}
	rec := wantOneCapture(t, p.state)
	if got := rec.Arguments["replicas"]; got != float64(10) {
		t.Errorf("record arguments[replicas] = %v, want the model's 10", got)
	}
	wantRevertsTo(t, rec, 3)
}

func TestCapture_OnTheApprovedPath(t *testing.T) {
	caps := newGateCaptures(t)
	p := runGateProbe(t, gateProbeConfig{
		policy:   OnMutationRequireApproval,
		respond:  approve(map[string]any{"verdict": "approve", "approver": "user:sre-oncall"}),
		captures: caps.rules,
	})

	if len(p.executions) != 1 {
		t.Fatalf("tool executed %d time(s), want 1: %+v", len(p.executions), p.executions)
	}
	rec := wantOneCapture(t, p.state)
	wantRevertsTo(t, rec, 3)

	if len(caps.reads) != 1 {
		t.Errorf("capture read ran %d time(s), want 1: %v", len(caps.reads), caps.reads)
	}
}

func TestCapture_OnTheEditedPath_RecordsWhatTheOperatorRan(t *testing.T) {
	caps := newGateCaptures(t)
	p := runGateProbe(t, gateProbeConfig{
		policy:   OnMutationRequireApproval,
		respond:  approve(editVerdict(map[string]any{"deployment": "api", "replicas": 2})),
		captures: caps.rules,
	})

	if len(p.executions) != 1 || p.executions[0].Replicas != 2 {
		t.Fatalf("executions = %+v, want the operator's 2 replicas", p.executions)
	}
	rec := wantOneCapture(t, p.state)
	if got := rec.Arguments["replicas"]; got != float64(2) {
		t.Errorf("record arguments[replicas] = %v, want the operator's 2", got)
	}
	if !strings.Contains(rec.Key, "replicas=2") {
		t.Errorf("record key = %q, want the edited call", rec.Key)
	}
	wantRevertsTo(t, rec, 3)
}

func TestCapture_OnTheGrantedPath(t *testing.T) {
	caps := newGateCaptures(t)
	scripts, turns := twoCallTurns(approveSet(), scaleCall("worker", 1))
	p := runChangeSetProbe(t, csConfig{
		set:      fixtureChangeSet(),
		scripts:  scripts,
		turns:    turns,
		captures: caps.rules,
	})

	if len(p.executions) != 2 {
		t.Fatalf("tool executed %d time(s), want 2: %+v", len(p.executions), p.executions)
	}
	recs := captureRecords(t, p.state)
	if len(recs) != 2 {
		t.Fatalf("capture records = %d, want one per executed call: %+v", len(recs), recs)
	}
	var granted bool
	for _, r := range recs {
		if strings.Contains(r.Key, "worker") {
			granted = true
			wantRevertsTo(t, r, 3)
		}
	}
	if !granted {
		keys := make([]string, 0, len(recs))
		for _, r := range recs {
			keys = append(keys, r.Key)
		}
		t.Errorf("no capture for the granted call; records name %v", keys)
	}
}

func TestCaptureFailure_RefusesTheCallOnEveryPath(t *testing.T) {
	paths := []struct {
		name string
		cfg  gateProbeConfig
	}{
		{"apply", gateProbeConfig{policy: OnMutationApply}},
		{"approved", gateProbeConfig{
			policy:  OnMutationRequireApproval,
			respond: approve(map[string]any{"verdict": "approve", "approver": "user:sre-oncall"}),
		}},
		{"edited", gateProbeConfig{
			policy:  OnMutationRequireApproval,
			respond: approve(editVerdict(map[string]any{"deployment": "api", "replicas": 2})),
		}},
	}

	for _, tc := range paths {
		t.Run(tc.name, func(t *testing.T) {
			caps := newGateCaptures(t)
			caps.err = errors.New("the cluster API is unreachable")
			cfg := tc.cfg
			cfg.captures = caps.rules
			p := runGateProbe(t, cfg)

			if len(p.executions) != 0 {
				t.Fatalf("tool executed %+v despite the capture failing", p.executions)
			}
			if len(captureRecords(t, p.state)) != 0 {
				t.Error("a failed capture still wrote a record")
			}
			resp := p.lastResponse(t)
			wantField(t, resp, "error", "capture_failed")
			wantDetailMentions(t, resp, "unreachable")
		})
	}
}

func TestCaptureFailure_RefusesTheGrantedCallAndLeavesTheGrantUnspent(t *testing.T) {
	caps := newGateCaptures(t)
	failing := caps.rules.Read
	seen := 0
	caps.rules.Read = func(ctx adkagent.Context, name string, args map[string]any) (map[string]any, error) {
		seen++
		if seen > 1 {
			return nil, errors.New("the cluster API is unreachable")
		}
		return failing(ctx, name, args)
	}

	scripts, turns := twoCallTurns(approveSet(), scaleCall("worker", 1))
	p := runChangeSetProbe(t, csConfig{
		set:      fixtureChangeSet(),
		scripts:  scripts,
		turns:    turns,
		captures: caps.rules,
	})

	if !p.executed("api", 3) {
		t.Fatalf("the directly-approved call did not run: %+v", p.executions)
	}
	if p.executed("worker", 1) {
		t.Fatalf("the granted call ran despite its capture failing: %+v", p.executions)
	}
	for _, g := range p.grants(t) {
		if g.ConsumedBy != "" {
			t.Errorf("grant %q was spent on a call that never ran", g.Signature)
		}
	}
}

func TestUndeclaredCaptureLeavesTheCallUntouched(t *testing.T) {
	reads := 0
	rules := &CaptureRules{
		For: func(string) (*Capture, error) { return nil, nil },
		Read: func(adkagent.Context, string, map[string]any) (map[string]any, error) {
			reads++
			return priorState(), nil
		},
	}
	p := runGateProbe(t, gateProbeConfig{policy: OnMutationApply, captures: rules})

	if len(p.executions) != 1 {
		t.Fatalf("tool executed %d time(s), want 1: %+v", len(p.executions), p.executions)
	}
	if reads != 0 {
		t.Errorf("capture read ran %d time(s) for a tool that declares none", reads)
	}
	if got := captureRecords(t, p.state); len(got) != 0 {
		t.Errorf("records written for a tool that declares no capture: %+v", got)
	}
}

func TestUndeclarableCaptureRefusesRatherThanSkips(t *testing.T) {
	rules := &CaptureRules{
		For: func(string) (*Capture, error) { return nil, errors.New("bundle is unreadable") },
	}
	p := runGateProbe(t, gateProbeConfig{policy: OnMutationApply, captures: rules})

	if len(p.executions) != 0 {
		t.Fatalf("tool executed %+v despite an undeclarable capture", p.executions)
	}
	wantField(t, p.lastResponse(t), "error", "capture_failed")
	wantDetailMentions(t, p.lastResponse(t), "unreadable")
}

func TestCaptureRecordCarriesItsProvenance(t *testing.T) {
	caps := newGateCaptures(t)
	p := runGateProbe(t, gateProbeConfig{policy: OnMutationApply, captures: caps.rules})

	rec := wantOneCapture(t, p.state)
	if rec.FunctionCallID == "" {
		t.Error("record carries no function call id")
	}
	if rec.Session != sid {
		t.Errorf("record session = %q, want %q", rec.Session, sid)
	}
	if rec.Specialist == "" {
		t.Error("record does not name the agent that made the call")
	}
	if rec.Read != "get_deployment" || len(rec.ReadArgs) == 0 {
		t.Errorf("record does not name the read that produced it: read=%q args=%v", rec.Read, rec.ReadArgs)
	}
	if rec.CapturedAt.IsZero() {
		t.Error("record has no capture time")
	}
	if got := DescribeCapture(rec); !strings.Contains(got, "undo with") {
		t.Errorf("description does not offer the undo: %s", got)
	}
}
