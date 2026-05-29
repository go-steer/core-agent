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
	"reflect"
	"testing"
)

func TestGate_Snapshot_RoundTrip(t *testing.T) {
	t.Parallel()
	policy, err := NewPolicy(
		[]string{"bash:git status", "read_file:internal/**", "fetch_url:github.com/*"},
		[]string{"bash:sudo *"},
	)
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	g := New(Options{Mode: ModeAsk, Policy: policy})
	snap := g.Snapshot()

	if snap.Mode != ModeAsk {
		t.Errorf("Mode = %q, want %q", snap.Mode, ModeAsk)
	}
	wantAllow := []string{"bash:git status", "read_file:internal/**", "fetch_url:github.com/*"}
	if !reflect.DeepEqual(snap.Allow, wantAllow) {
		t.Errorf("Allow:\n got %v\n want %v", snap.Allow, wantAllow)
	}
	wantDeny := []string{"bash:sudo *"}
	if !reflect.DeepEqual(snap.Deny, wantDeny) {
		t.Errorf("Deny:\n got %v\n want %v", snap.Deny, wantDeny)
	}
}

func TestGate_Snapshot_EmptyPolicy(t *testing.T) {
	t.Parallel()
	g := New(Options{Mode: ModeYolo})
	snap := g.Snapshot()
	if snap.Mode != ModeYolo {
		t.Errorf("Mode = %q, want %q", snap.Mode, ModeYolo)
	}
	if len(snap.Allow) != 0 || len(snap.Deny) != 0 {
		t.Errorf("expected empty patterns, got allow=%v deny=%v", snap.Allow, snap.Deny)
	}
}

func TestGate_ToolGateState(t *testing.T) {
	t.Parallel()
	policy, err := NewPolicy(
		[]string{"fetch_url:*", "read_file:internal/**"},
		[]string{"bash:*"},
	)
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}

	cases := []struct {
		mode Mode
		tool string
		want string
	}{
		// Yolo mode short-circuits to "allowed" for anything without a deny.
		{ModeYolo, "fetch_url", ToolGateAllowed},
		{ModeYolo, "read_file", ToolGateAllowed},
		{ModeYolo, "anything_else", ToolGateAllowed},
		// Deny wins even in yolo.
		{ModeYolo, "bash", ToolGateDenied},
		// Ask mode + allow pattern → allowed.
		{ModeAsk, "fetch_url", ToolGateAllowed},
		// Ask mode + no pattern → prompted.
		{ModeAsk, "edit_file", ToolGatePrompted},
		// Ask mode + deny pattern → denied.
		{ModeAsk, "bash", ToolGateDenied},
		// Allow mode + no allowlist entry → denied-allow-mode.
		{ModeAllow, "edit_file", ToolGateDeniedInAllowMode},
		// Allow mode + allow pattern → allowed.
		{ModeAllow, "fetch_url", ToolGateAllowed},
		// Allow mode + deny pattern → denied.
		{ModeAllow, "bash", ToolGateDenied},
	}
	for _, c := range cases {
		g := New(Options{Mode: c.mode, Policy: policy})
		got := g.ToolGateState(c.tool)
		if got != c.want {
			t.Errorf("mode=%s tool=%s: got %q want %q", c.mode, c.tool, got, c.want)
		}
	}
}

func TestPolicy_RawPatterns_RoundTrip(t *testing.T) {
	t.Parallel()
	in := []string{"bash:git status", "fetch_url:github.com/*", "edit_file:**/*.go"}
	p, err := NewPolicy(in, []string{"bash:sudo *"})
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	allow, deny := p.RawPatterns()
	if !reflect.DeepEqual(allow, in) {
		t.Errorf("allow round-trip:\n got %v\n want %v", allow, in)
	}
	if !reflect.DeepEqual(deny, []string{"bash:sudo *"}) {
		t.Errorf("deny round-trip: got %v", deny)
	}
}
