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

package permissions

import (
	"context"
	"testing"
)

func TestSessionGateContext_RoundTrip(t *testing.T) {
	t.Parallel()
	g := New(Options{Mode: ModeAsk})
	ctx := WithSessionGate(context.Background(), g)
	got, ok := SessionGateFromContext(ctx)
	if !ok {
		t.Fatal("SessionGateFromContext returned ok=false; expected the gate we stored")
	}
	if got != g {
		t.Errorf("got different gate pointer: %p vs %p", got, g)
	}
}

func TestSessionGateContext_MissingReturnsNotOK(t *testing.T) {
	t.Parallel()
	_, ok := SessionGateFromContext(context.Background())
	if ok {
		t.Error("SessionGateFromContext on a fresh context returned ok=true; expected false")
	}
}

func TestSessionGateContext_NilGateIsNoOp(t *testing.T) {
	t.Parallel()
	// Passing nil shouldn't store a sentinel that would confuse
	// SessionGateFromContext into returning a typed-nil with ok=true.
	ctx := WithSessionGate(context.Background(), nil)
	_, ok := SessionGateFromContext(ctx)
	if ok {
		t.Error("WithSessionGate(nil) should leave the context clean; got ok=true")
	}
}

func TestSessionGateContext_DerivedGate(t *testing.T) {
	t.Parallel()
	// Real-world shape: factory derives a sub-gate from the template
	// and puts the sub-gate on context. SessionGateFromContext should
	// return the sub-gate, NOT the template.
	template := New(Options{Mode: ModeAsk})
	sub := template.DeriveForSession("sess-A", nil)
	ctx := WithSessionGate(context.Background(), sub)

	got, _ := SessionGateFromContext(ctx)
	if got != sub {
		t.Errorf("context returned template gate, not the derived sub-gate")
	}
	if got == template {
		t.Errorf("context returned template gate; should be the derived sub-gate")
	}
}
