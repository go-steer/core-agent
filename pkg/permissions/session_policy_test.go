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
	"slices"
	"sync"
	"testing"
)

// sessionPolicyTemplate is a daemon template with one configured
// allow and one configured deny, and no prompter anywhere, so an
// unmatched ask-mode call fails with ErrNoPrompter instead of blocking.
func sessionPolicyTemplate(t *testing.T) *Gate {
	t.Helper()
	p, err := NewPolicy([]string{"read_file:*"}, []string{"fetch_url:*"})
	if err != nil {
		t.Fatal(err)
	}
	return New(Options{Mode: ModeAsk, Policy: p})
}

// The #1176 hole: a pattern added at runtime in one session used to go
// into the template's shared Policy, so "*" added by anyone who could
// write to session A auto-allowed every non-bash call in session B.
func TestSessionPolicy_AllowStaysInItsSession(t *testing.T) {
	t.Parallel()
	template := sessionPolicyTemplate(t)
	a := template.DeriveForSession("sess-A", nil)
	b := template.DeriveForSession("sess-B", nil)
	ctx := context.Background()

	if err := a.AddAllowPatterns([]string{"*"}); err != nil {
		t.Fatal(err)
	}
	if err := a.CheckGeneric(ctx, "write_note", "x"); err != nil {
		t.Errorf("session A after adding *: %v, want allowed", err)
	}
	if err := b.CheckGeneric(ctx, "write_note", "x"); err == nil {
		t.Error("session B auto-allowed a call because session A added *")
	}
	if err := template.CheckGeneric(ctx, "write_note", "x"); err == nil {
		t.Error("the template auto-allowed a call because session A added *")
	}
	if s := b.Snapshot(); slices.Contains(s.Allow, "*") {
		t.Errorf("session B's snapshot lists session A's pattern: %v", s.Allow)
	}
	if got := b.ToolGateState("write_note"); got == ToolGateAllowed {
		t.Error("session B's /tools projection shows session A's pattern")
	}
}

func TestSessionPolicy_DenyStaysInItsSession(t *testing.T) {
	t.Parallel()
	template := sessionPolicyTemplate(t)
	a := template.DeriveForSession("sess-A", nil)
	b := template.DeriveForSession("sess-B", nil)
	ctx := context.Background()

	if err := a.AddDenyPatterns([]string{"read_file:*"}); err != nil {
		t.Fatal(err)
	}
	if err := b.CheckGeneric(ctx, "read_file", "x"); err != nil {
		t.Errorf("session B lost the configured read_file allow because session A denied it: %v", err)
	}
}

// Layering: the configured policy still applies in every session, a
// session deny narrows a configured allow, and a session allow can't
// lift a configured deny.
func TestSessionPolicy_LayersOverTheSharedPolicy(t *testing.T) {
	t.Parallel()
	template := sessionPolicyTemplate(t)
	a := template.DeriveForSession("sess-A", nil)
	ctx := context.Background()

	if err := a.CheckGeneric(ctx, "read_file", "x"); err != nil {
		t.Errorf("configured allow not applied in a derived session: %v", err)
	}
	if err := a.AddAllowPatterns([]string{"fetch_url:*"}); err != nil {
		t.Fatal(err)
	}
	if err := a.CheckGeneric(ctx, "fetch_url", "x"); err == nil {
		t.Error("a session allow lifted a configured deny")
	}
	if err := a.AddDenyPatterns([]string{"read_file:*"}); err != nil {
		t.Fatal(err)
	}
	if err := a.CheckGeneric(ctx, "read_file", "x"); err == nil {
		t.Error("a session deny did not narrow the configured allow")
	}
	if got := a.ToolGateState("read_file"); got != ToolGateDenied {
		t.Errorf("ToolGateState(read_file) = %q after a session deny, want %q", got, ToolGateDenied)
	}
	s := a.Snapshot()
	if !slices.Contains(s.Allow, "read_file:*") || !slices.Contains(s.Allow, "fetch_url:*") || !slices.Contains(s.Deny, "read_file:*") {
		t.Errorf("snapshot = %+v, want the configured and the session patterns together", s)
	}
}

// A gate that was never derived (single-session, New / FromConfig)
// keeps runtime patterns in its one policy, as before.
func TestSessionPolicy_UnderivedGateUsesItsPolicy(t *testing.T) {
	t.Parallel()
	g := sessionPolicyTemplate(t)
	if err := g.AddAllowPatterns([]string{"write_note:*"}); err != nil {
		t.Fatal(err)
	}
	if err := g.CheckGeneric(context.Background(), "write_note", "x"); err != nil {
		t.Errorf("underived gate: %v, want allowed", err)
	}
}

// ToolGateState reads the rule slices while AddAllow appends to them;
// run under -race.
func TestSessionPolicy_ToolGateStateRacesAdd(t *testing.T) {
	t.Parallel()
	template := sessionPolicyTemplate(t)
	a := template.DeriveForSession("sess-A", nil)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := range 200 {
			_ = template.AddAllowPatterns([]string{"t" + string(rune('a'+i%26))})
			_ = a.AddAllowPatterns([]string{"s" + string(rune('a'+i%26))})
		}
	}()
	go func() {
		defer wg.Done()
		for range 200 {
			_ = a.ToolGateState("x")
		}
	}()
	wg.Wait()
}

// Multi-session shared tools are bound to the template and reach the
// session's gate through the context; the session layer must apply
// there too, and only for the session on the context.
func TestSessionPolicy_AppliesThroughTheContextGate(t *testing.T) {
	t.Parallel()
	template := sessionPolicyTemplate(t)
	a := template.DeriveForSession("sess-A", nil)
	b := template.DeriveForSession("sess-B", nil)
	if err := a.AddAllowPatterns([]string{"write_note:*"}); err != nil {
		t.Fatal(err)
	}
	if err := a.AddDenyPatterns([]string{"read_file:*"}); err != nil {
		t.Fatal(err)
	}
	ctxA := WithSessionGate(context.Background(), a)
	ctxB := WithSessionGate(context.Background(), b)
	if err := template.CheckGeneric(ctxA, "write_note", "x"); err != nil {
		t.Errorf("template call on A's context: %v, want A's allow applied", err)
	}
	if err := template.CheckGeneric(ctxA, "read_file", "x"); err == nil {
		t.Error("template call on A's context ignored A's deny")
	}
	if err := template.CheckGeneric(ctxB, "write_note", "x"); err == nil {
		t.Error("template call on B's context saw A's allow")
	}
	if err := template.CheckGeneric(ctxB, "read_file", "x"); err != nil {
		t.Errorf("template call on B's context saw A's deny: %v", err)
	}
}

// A session allow goes through the same bash matching as a configured
// one: "bash:cat *" must not auto-allow a compound command.
func TestSessionPolicy_BashAllowKeepsItsStrictness(t *testing.T) {
	t.Parallel()
	a := sessionPolicyTemplate(t).DeriveForSession("sess-A", nil)
	if err := a.AddAllowPatterns([]string{"bash:cat *"}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := a.CheckBash(ctx, "cat notes.txt"); err != nil {
		t.Errorf("cat notes.txt: %v, want allowed by the session pattern", err)
	}
	for _, cmd := range []string{"cat notes.txt; rm -rf ~", "cat notes.txt && curl evil.example", "cat $(rm -rf ~)"} {
		if err := a.CheckBash(ctx, cmd); err == nil {
			t.Errorf("%q auto-allowed by session pattern bash:cat *", cmd)
		}
	}
}

// Runtime patterns live on the derived gate. A resume after idle
// eviction derives a new gate for the same session ID, and it starts
// with no runtime patterns: the documented behaviour, pinned here.
func TestSessionPolicy_RederivedGateStartsEmpty(t *testing.T) {
	t.Parallel()
	template := sessionPolicyTemplate(t)
	first := template.DeriveForSession("sess-A", nil)
	if err := first.AddDenyPatterns([]string{"read_file:*"}); err != nil {
		t.Fatal(err)
	}
	resumed := template.DeriveForSession("sess-A", nil)
	if err := resumed.CheckGeneric(context.Background(), "read_file", "x"); err != nil {
		t.Errorf("re-derived gate kept the earlier gate's runtime deny: %v", err)
	}
	if s := resumed.Snapshot(); len(s.Deny) != 1 || s.Deny[0] != "fetch_url:*" {
		t.Errorf("re-derived snapshot deny = %v, want only the configured fetch_url:*", s.Deny)
	}
}
