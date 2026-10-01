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
	"encoding/json"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

type marshalsEscaped struct{}

func (marshalsEscaped) MarshalJSON() ([]byte, error) {
	return []byte(`{"path":".agents\/config.json"}`), nil
}

func TestCallArgs(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		in   any
		want string // "" = nil
	}{
		{"nil", nil, ""},
		{"struct", struct {
			Command string `json:"command"`
		}{"go test"}, `{"command":"go test"}`},
		{"map", map[string]any{"b": 1, "a": "x"}, `{"a":"x","b":1}`},
		{"raw escapes resolve", json.RawMessage(`{"path":".agents\/config.json"}`), `{"path":".agents/config.json"}`},
		{"bytes escapes resolve", []byte(`{"p":"\u0041"}`), `{"p":"A"}`},
		{"custom MarshalJSON re-encoded", marshalsEscaped{}, `{"path":".agents/config.json"}`},
		{"html not escaped", map[string]string{"c": "a < b && c > d"}, `{"c":"a < b && c > d"}`},
		{"large integer kept", json.RawMessage(`{"n":12345678901234567890}`), `{"n":12345678901234567890}`},
		{"whitespace dropped", json.RawMessage(" {\n\"a\" : 1 }\n"), `{"a":1}`},
		{"null", json.RawMessage(`null`), ""},
		{"nil map encodes null", map[string]any(nil), ""},
		{"invalid", json.RawMessage(`{"a":`), ""},
		{"trailing data", json.RawMessage(`{"a":1} {"b":2}`), ""},
		{"unencodable", map[string]any{"f": func() {}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := CallArgs(tc.in)
			if tc.want == "" {
				if got != nil {
					t.Errorf("CallArgs = %s, want nil", got)
				}
				return
			}
			if string(got) != tc.want {
				t.Errorf("CallArgs = %s, want %s", got, tc.want)
			}
		})
	}
}

// Each WithArgs sibling reaches the approver; its plain form, holding
// no arguments, escalates to a person instead (#1175 phase 2).
func TestWithArgsSiblings_ReachTheApprover(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	file := filepath.Join(dir, "out.txt")
	scope, err := NewPathScope(dir, "", nil)
	if err != nil {
		t.Fatalf("NewPathScope: %v", err)
	}
	args := map[string]string{"marker": "m"}
	for _, tc := range []struct {
		name     string
		eligible string
		withArgs func(context.Context, *Gate) error
		plain    func(context.Context, *Gate) error
	}{
		{"bash", "bash:go test *",
			func(ctx context.Context, g *Gate) error { return g.CheckBashWithArgs(ctx, testCmd, args) },
			func(ctx context.Context, g *Gate) error { return g.CheckBash(ctx, testCmd) }},
		{"file write", "write_file:*",
			func(ctx context.Context, g *Gate) error {
				return g.CheckFileWriteWithArgs(ctx, "write_file", file, args)
			},
			func(ctx context.Context, g *Gate) error { return g.CheckFileWrite(ctx, "write_file", file) }},
		{"generic", "fetch_url:*",
			func(ctx context.Context, g *Gate) error {
				return g.CheckGenericWithArgs(ctx, "fetch_url", "https://example.com", args)
			},
			func(ctx context.Context, g *Gate) error {
				return g.CheckGeneric(ctx, "fetch_url", "https://example.com")
			}},
		{"tool call", "mcp_srv:*",
			func(ctx context.Context, g *Gate) error {
				return g.CheckToolCallWithArgs(ctx, "mcp_srv", "t", "t {}", args)
			},
			func(ctx context.Context, g *Gate) error { return g.CheckToolCall(ctx, "mcp_srv", "t", "t {}") }},
		{"read-only tool call", "mcp_srv:*",
			func(ctx context.Context, g *Gate) error {
				return g.CheckReadOnlyToolCallWithArgs(ctx, "mcp_srv", "t", "t {}", args)
			},
			func(ctx context.Context, g *Gate) error { return g.CheckReadOnlyToolCall(ctx, "mcp_srv", "t", "t {}") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			build := func() (*Gate, *stubApprover, *fakePrompter) {
				a := &stubApprover{verdict: allowVerdict}
				p := &fakePrompter{decision: DecisionAllowOnce}
				return autoGate(t, a, p, func(o *Options) {
					o.AutoEligible = mustPolicy(t, []string{tc.eligible}, nil)
					o.Scope = scope
				}), a, p
			}
			ctx, _ := taskCtx()

			g, a, p := build()
			if err := tc.withArgs(ctx, g); err != nil {
				t.Fatalf("with args: %v", err)
			}
			if a.n() != 1 || len(p.calls) != 0 {
				t.Errorf("with args: approver %d / prompter %d, want 1 / 0", a.n(), len(p.calls))
			}
			if a.n() == 1 && string(a.calls[0].Args) != `{"marker":"m"}` {
				t.Errorf("approver saw Args %s, want the canonical call", a.calls[0].Args)
			}

			g, a, p = build()
			if err := tc.plain(ctx, g); err != nil {
				t.Fatalf("plain: %v", err)
			}
			if a.n() != 0 || len(p.calls) != 1 {
				t.Errorf("plain: approver %d / prompter %d, want 0 / 1", a.n(), len(p.calls))
			}
		})
	}
}

// The never-list match runs over canonical Args and ignores case, so
// neither a JSON escape nor a capital letter walks a control-plane path
// past it to the approver.
func TestAuto_NeverListSeesThroughEscapesAndCase(t *testing.T) {
	t.Parallel()
	for name, raw := range map[string]json.RawMessage{
		"slash escape":   json.RawMessage(`{"url":"file:///w/.agents\/config.json"}`),
		"unicode escape": json.RawMessage(`{"url":"file:///w/\u002eagents\u002fmcp.json"}`),
		"upper case":     json.RawMessage(`{"url":"file:///w/.AGENTS/CONFIG.JSON"}`),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			a := &stubApprover{verdict: allowVerdict}
			p := &fakePrompter{decision: DecisionDeny}
			g := autoGate(t, a, p, func(o *Options) {
				o.AutoEligible = mustPolicy(t, []string{"fetch_url:*"}, nil)
			})
			ctx, _ := taskCtx()
			_ = g.CheckGenericWithArgs(ctx, "fetch_url", "https://example.com", raw)
			if a.n() != 0 || len(p.calls) != 1 {
				t.Errorf("approver %d / prompter %d, want 0 / 1", a.n(), len(p.calls))
			}
		})
	}
}

// countingArgs counts how often the gate encodes it.
type countingArgs struct{ n *atomic.Int32 }

func (c countingArgs) MarshalJSON() ([]byte, error) {
	c.n.Add(1)
	return []byte(`{"k":"v"}`), nil
}

// The arguments are encoded only when a prompt reads them: a call that
// yolo or a policy allow settles never pays to re-encode its input,
// which for write_file is the whole file.
func TestWithArgs_EncodesOnlyOnThePromptPath(t *testing.T) {
	t.Parallel()
	ctx, _ := taskCtx()
	for _, tc := range []struct {
		name string
		opts Options
		want int32
	}{
		{"yolo", Options{Mode: ModeYolo}, 0},
		{"policy allow", Options{Mode: ModeAsk, Policy: mustPolicy(t, []string{"fetch_url:*"}, nil), Prompter: &fakePrompter{}}, 0},
		{"auto prompt", Options{Mode: ModeAuto, Approver: &stubApprover{verdict: allowVerdict},
			AutoEligible: mustPolicy(t, []string{"fetch_url:*"}, nil), ApprovalTimeout: time.Minute}, 1},
	} {
		n := &atomic.Int32{}
		if err := New(tc.opts).CheckGenericWithArgs(ctx, "fetch_url", "https://example.com", countingArgs{n}); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := n.Load(); got != tc.want {
			t.Errorf("%s: args encoded %d times, want %d", tc.name, got, tc.want)
		}
	}
}

// The encoder cannot avoid escaping a quote, so an instructions file
// whose name holds one appears in canonical Args only in its encoded
// spelling; the never-list matches that spelling too.
func TestAuto_NeverListMatchesTheEncodedSpelling(t *testing.T) {
	t.Parallel()
	name := `policy"v2.md`
	a := &stubApprover{verdict: allowVerdict}
	p := &fakePrompter{decision: DecisionDeny}
	g := autoGate(t, a, p, func(o *Options) {
		o.AutoEligible = mustPolicy(t, []string{"fetch_url:*"}, nil)
		o.ApproverInstructionsFile = filepath.Join(t.TempDir(), name)
	})
	ctx, _ := taskCtx()
	_ = g.CheckGenericWithArgs(ctx, "fetch_url", "https://example.com", map[string]string{"path": name})
	if a.n() != 0 || len(p.calls) != 1 {
		t.Errorf("approver %d / prompter %d, want 0 / 1", a.n(), len(p.calls))
	}
}
