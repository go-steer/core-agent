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
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	adkagent "google.golang.org/adk/v2/agent"
)

const capturedAt = "2026-09-05T12:00:00Z"

func fixedClock(t *testing.T) func() time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339, capturedAt)
	if err != nil {
		t.Fatalf("parsing the fixture clock: %v", err)
	}
	return func() time.Time { return at }
}

func priorState() map[string]any {
	return map[string]any{
		"metadata": map[string]any{"name": "api", "namespace": "prod"},
		"spec":     map[string]any{"replicas": 3, "paused": false},
	}
}

func scaleCapture() *Capture {
	return &Capture{
		Read:     "get_deployment",
		ArgsFrom: map[string]string{"name": "deployment"},
		Fields:   []string{"spec.replicas"},
		Revert: &Revert{
			Call:            "scale_deployment",
			ArgsFromChange:  map[string]string{"deployment": "deployment"},
			ArgsFromCapture: map[string]string{"replicas": "spec.replicas"},
		},
	}
}

func changeArgs() map[string]any {
	return map[string]any{"deployment": "api", "replicas": 10}
}

type capturingRules struct {
	*CaptureRules
	reads    int
	readArgs map[string]any
}

func newCapturingRules(t *testing.T, decl *Capture, result map[string]any, readErr error) *capturingRules {
	t.Helper()
	c := &capturingRules{}
	c.CaptureRules = &CaptureRules{
		For: func(string) (*Capture, error) { return decl, nil },
		Read: func(_ adkagent.Context, _ string, args map[string]any) (map[string]any, error) {
			c.reads++
			c.readArgs = args
			if readErr != nil {
				return nil, readErr
			}
			return result, nil
		},
		Schema: func(string) (*jsonschema.Schema, error) { return mcpShaped(), nil },
		Now:    fixedClock(t),
	}
	return c
}

func takeScale(t *testing.T, c *capturingRules, decl *Capture) (*CaptureRecord, error) {
	t.Helper()
	return c.take(nil, "scale_deployment", CallKey("scale_deployment", changeArgs()), changeArgs(), decl)
}

func TestCaptureTake_RecordsTheOldValueAndTheCallThatPutsItBack(t *testing.T) {
	t.Parallel()
	decl := scaleCapture()
	c := newCapturingRules(t, decl, priorState(), nil)

	rec, err := takeScale(t, c, decl)
	if err != nil {
		t.Fatalf("take: %v", err)
	}

	if got := c.readArgs["name"]; got != "api" {
		t.Errorf("capture read args[name] = %v, want \"api\"", got)
	}
	if got, want := rec.Prior["spec.replicas"], 3; got != want {
		t.Errorf("prior[spec.replicas] = %v, want %v", got, want)
	}
	if len(rec.Prior) != 1 {
		t.Errorf("prior kept %d fields, want only the declared one: %v", len(rec.Prior), rec.Prior)
	}
	if rec.Tool != "scale_deployment" {
		t.Errorf("record tool = %q, want scale_deployment", rec.Tool)
	}
	if got := rec.Arguments["replicas"]; got != 10 {
		t.Errorf("record arguments[replicas] = %v, want the change's 10", got)
	}
	if rec.Revert == nil {
		t.Fatal("no revert recorded")
	}
	if rec.Revert.Tool != "scale_deployment" {
		t.Errorf("revert tool = %q, want scale_deployment", rec.Revert.Tool)
	}
	if got := rec.Revert.Arguments["replicas"]; got != float64(3) {
		t.Errorf("revert would set replicas to %v, want the prior 3", got)
	}
	if got := rec.Revert.Arguments["deployment"]; got != "api" {
		t.Errorf("revert deployment = %v, want api", got)
	}
	if !rec.Undoable() {
		t.Error("Undoable() is false on a record that carries a revert")
	}
}

func TestCaptureTake_WholeResultWhenNoFieldsDeclared(t *testing.T) {
	t.Parallel()
	decl := &Capture{Read: "get_deployment", ArgsFrom: map[string]string{"name": "deployment"}}
	c := newCapturingRules(t, decl, priorState(), nil)

	rec, err := takeScale(t, c, decl)
	if err != nil {
		t.Fatalf("take: %v", err)
	}
	if len(rec.PriorFields) != 0 {
		t.Errorf("PriorFields = %v, want empty", rec.PriorFields)
	}
	spec, ok := rec.Prior["spec"].(map[string]any)
	if !ok {
		t.Fatalf("prior did not keep the whole result: %v", rec.Prior)
	}
	if spec["paused"] != false {
		t.Errorf("prior dropped spec.paused: %v", spec)
	}
}

func TestCaptureTake_DigestCoversTheWholeReadEvenWhenPriorIsNarrowed(t *testing.T) {
	t.Parallel()
	decl := scaleCapture()

	first := priorState()
	second := priorState()
	second["spec"].(map[string]any)["paused"] = true

	recA, err := takeScale(t, newCapturingRules(t, decl, first, nil), decl)
	if err != nil {
		t.Fatalf("take (first): %v", err)
	}
	recB, err := takeScale(t, newCapturingRules(t, decl, second, nil), decl)
	if err != nil {
		t.Fatalf("take (second): %v", err)
	}
	if recA.Prior["spec.replicas"] != recB.Prior["spec.replicas"] {
		t.Fatal("fixture error: the narrowed field was supposed to be identical")
	}
	if recA.Digest == recB.Digest {
		t.Errorf("two reads differing outside the declared field share digest %q", recA.Digest)
	}
}

func TestResultDigest_IsStableAcrossMapOrder(t *testing.T) {
	t.Parallel()
	a := resultDigest(priorState())
	for i := 0; i < 32; i++ {
		if got := resultDigest(priorState()); got != a {
			t.Fatalf("digest of identical content differs across builds: %q vs %q", a, got)
		}
	}
	if a == "" {
		t.Error("digest is empty for a marshallable result")
	}
}

func TestCaptureTake_Refusals(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		decl    *Capture
		result  map[string]any
		readErr error
		rules   func(c *CaptureRules)
		want    string
	}{
		{
			name:   "no read tool named",
			decl:   &Capture{},
			result: priorState(),
			want:   "declares a prior-state capture with no read tool",
		},
		{
			name:   "read argument the call does not carry",
			decl:   &Capture{Read: "get_deployment", ArgsFrom: map[string]string{"name": "workload"}},
			result: priorState(),
			want:   `which this call does not carry`,
		},
		{
			name:    "the read itself failed",
			decl:    scaleCapture(),
			readErr: errors.New("connection refused"),
			want:    "capture read get_deployment",
		},
		{
			name:   "the read returned nothing at all",
			decl:   scaleCapture(),
			result: nil,
			want:   "there is nothing recorded to go back to",
		},
		{
			name:   "a declared field the read does not return",
			decl:   &Capture{Read: "get_deployment", ArgsFrom: map[string]string{"name": "deployment"}, Fields: []string{"spec.strategy"}},
			result: priorState(),
			want:   "would record nothing where it promised the old value",
		},
		{
			name:   "no read seam wired",
			decl:   scaleCapture(),
			result: priorState(),
			rules:  func(c *CaptureRules) { c.Read = nil },
			want:   "cannot run a read on its own behalf",
		},
		{
			name:   "revert path the read did not return",
			decl:   revertFrom(&Revert{Call: "scale_deployment", ArgsFromCapture: map[string]string{"replicas": "spec.desired"}}),
			result: priorState(),
			want:   "which the capture read did not return",
		},
		{
			name:   "revert argument the call does not carry",
			decl:   revertFrom(&Revert{Call: "scale_deployment", ArgsFromChange: map[string]string{"deployment": "workload"}}),
			result: priorState(),
			want:   "which this call does not carry",
		},
		{
			name:   "revert the tool would not accept",
			decl:   revertFrom(&Revert{Call: "scale_deployment", ArgsFromCapture: map[string]string{"replicas": "metadata.name"}}),
			result: priorState(),
			want:   "will not accept",
		},
		{
			name:   "no schema seam wired",
			decl:   scaleCapture(),
			result: priorState(),
			rules:  func(c *CaptureRules) { c.Schema = nil },
			want:   "cannot tell whether the revert it built is callable",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := newCapturingRules(t, tc.decl, tc.result, tc.readErr)
			if tc.rules != nil {
				tc.rules(c.CaptureRules)
			}
			rec, err := takeScale(t, c, tc.decl)
			if err == nil {
				t.Fatalf("take succeeded, want a refusal mentioning %q; record: %+v", tc.want, rec)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("refusal = %q, want it to mention %q", err, tc.want)
			}
			if rec != nil {
				t.Error("a refused capture returned a record")
			}
		})
	}
}

func revertFrom(r *Revert) *Capture {
	decl := scaleCapture()
	decl.Revert = r
	return decl
}

func TestCaptureTake_NoRevertDeclaredIsARecordNotAFailure(t *testing.T) {
	t.Parallel()
	decl := scaleCapture()
	decl.Revert = nil
	c := newCapturingRules(t, decl, priorState(), nil)

	rec, err := takeScale(t, c, decl)
	if err != nil {
		t.Fatalf("take: %v", err)
	}
	if rec.Undoable() {
		t.Error("Undoable() is true with no revert declared")
	}
	if rec.Prior["spec.replicas"] != 3 {
		t.Errorf("prior was not recorded without a revert: %v", rec.Prior)
	}
	if got := DescribeCapture(*rec); !strings.Contains(got, "declares no call that puts it back") {
		t.Errorf("description does not say the undo is missing: %s", got)
	}
}

func TestCaptureRules_NilIsInert(t *testing.T) {
	t.Parallel()
	var nilRules *CaptureRules
	decl, err := nilRules.declared("scale_deployment")
	if err != nil || decl != nil {
		t.Fatalf("nil rules declared = (%v, %v), want (nil, nil)", decl, err)
	}
	empty := &CaptureRules{}
	decl, err = empty.declared("scale_deployment")
	if err != nil || decl != nil {
		t.Fatalf("rules with no For declared = (%v, %v), want (nil, nil)", decl, err)
	}
	rec, err := empty.take(nil, "scale_deployment", "k", changeArgs(), nil)
	if err != nil || rec != nil {
		t.Fatalf("take with no declaration = (%v, %v), want (nil, nil)", rec, err)
	}
}

func TestCaptureRecord_RoundTrip(t *testing.T) {
	t.Parallel()
	decl := scaleCapture()
	c := newCapturingRules(t, decl, priorState(), nil)
	rec, err := takeScale(t, c, decl)
	if err != nil {
		t.Fatalf("take: %v", err)
	}
	rec.Workload = "sre"

	raw, err := EncodeCapture(*rec)
	if err != nil {
		t.Fatalf("EncodeCapture: %v", err)
	}
	back, err := DecodeCapture(raw)
	if err != nil {
		t.Fatalf("DecodeCapture: %v", err)
	}

	if back.Revert == nil {
		t.Fatal("the revert did not survive the round trip")
	}
	if got := back.Revert.Arguments["replicas"]; got != float64(3) {
		t.Errorf("revert replicas = %v after a round trip, want 3", got)
	}
	if got := back.Prior["spec.replicas"]; got != float64(3) {
		t.Errorf("prior replicas = %v after a round trip, want 3", got)
	}
	if back.Digest != rec.Digest || back.Workload != "sre" {
		t.Errorf("round trip lost digest or workload: %+v", back)
	}
	if !back.CapturedAt.Equal(rec.CapturedAt) {
		t.Errorf("captured_at = %v after a round trip, want %v", back.CapturedAt, rec.CapturedAt)
	}
}

func TestDecodeCapture_AcceptsWhatABackendHandsBack(t *testing.T) {
	t.Parallel()
	rec := CaptureRecord{Tool: "scale_deployment", Key: "scale_deployment(...)", Digest: "abc"}
	raw, err := EncodeCapture(rec)
	if err != nil {
		t.Fatalf("EncodeCapture: %v", err)
	}
	var asMap map[string]any
	if err := json.Unmarshal([]byte(raw), &asMap); err != nil {
		t.Fatalf("unmarshalling to a map: %v", err)
	}

	for _, v := range []any{raw, []byte(raw), asMap} {
		got, err := DecodeCapture(v)
		if err != nil {
			t.Fatalf("DecodeCapture(%T): %v", v, err)
		}
		if got.Tool != rec.Tool || got.Digest != rec.Digest {
			t.Errorf("DecodeCapture(%T) = %+v, want tool/digest preserved", v, got)
		}
	}

	if _, err := DecodeCapture(nil); err == nil {
		t.Error("DecodeCapture(nil) succeeded")
	}
	if _, err := DecodeCapture("{not json"); err == nil {
		t.Error("DecodeCapture accepted a non-record string")
	}
}

func TestCaptureStateKey_RoundTripsTheFunctionCallID(t *testing.T) {
	t.Parallel()
	const id = "fc-7f3a"
	key := CaptureStateKey(id)
	if !strings.HasPrefix(key, CaptureStateKeyPrefix) {
		t.Fatalf("key %q does not carry the prefix", key)
	}
	if got := strings.TrimPrefix(key, CaptureStateKeyPrefix); got != id {
		t.Errorf("trimming the prefix yields %q, want %q", got, id)
	}
}
