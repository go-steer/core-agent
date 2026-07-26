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

package agent

import "testing"

// The Inner/Emit accessors are the seam the split-out packages
// (pkg/agent/autonomous, pkg/agent/background) will build on instead of
// reaching Agent's unexported fields — see
// docs/agent-package-split-design.md. These tests pin the seam contract
// so a later phase can move code out of the package with confidence.

func TestInner_ReturnsUnderlyingAgent(t *testing.T) {
	t.Parallel()
	a, err := New(minimalLLM{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if a.Inner() == nil {
		t.Fatal("Inner() = nil; New should have wired an inner ADK agent")
	}
	if a.Inner() != a.inner {
		t.Error("Inner() did not return the inner field")
	}
}

func TestInner_NilReceiver(t *testing.T) {
	t.Parallel()
	if (*Agent)(nil).Inner() != nil {
		t.Error("Inner() on a nil receiver should return nil, not panic")
	}
}

func TestEmit_RoutesToAttachEmitter(t *testing.T) {
	t.Parallel()
	a := &Agent{}
	var gotType string
	var gotPayload any
	a.attachEmit = func(eventType string, payload any) {
		gotType = eventType
		gotPayload = payload
	}
	a.Emit("evt", 42)
	if gotType != "evt" || gotPayload != 42 {
		t.Errorf("Emit routed (%q, %v); want (\"evt\", 42)", gotType, gotPayload)
	}
}

func TestEmit_NoEmitterIsNoop(t *testing.T) {
	t.Parallel()
	// No attachEmit installed (no SSE subscriber) and a nil receiver
	// must both be safe no-ops — the emit() contract the seam wraps.
	(&Agent{}).Emit("evt", nil)
	(*Agent)(nil).Emit("evt", nil)
}
