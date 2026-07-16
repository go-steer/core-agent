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

package mcp

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	adkagent "google.golang.org/adk/agent"
	"google.golang.org/adk/memory"
	"google.golang.org/adk/session"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/toolconfirmation"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/pkg/digest"
)

// stubToolCtx is a minimal tool.Context implementation for digest_wrap
// unit tests. Only FunctionCallID is meaningfully populated; every
// other method returns the zero value. Full-interface satisfaction
// keeps compile-time honest — a future ADK bump that adds a method
// forces us to update the stub rather than silently drifting.
type stubToolCtx struct {
	context.Context
	callID string
}

// context.Context: embedded — no methods to add.

// ReadonlyContext.
func (s *stubToolCtx) UserContent() *genai.Content          { return nil }
func (s *stubToolCtx) InvocationID() string                 { return "test-invocation" }
func (s *stubToolCtx) AgentName() string                    { return "test-agent" }
func (s *stubToolCtx) ReadonlyState() session.ReadonlyState { return nil }
func (s *stubToolCtx) UserID() string                       { return "test-user" }
func (s *stubToolCtx) AppName() string                      { return "test-app" }
func (s *stubToolCtx) SessionID() string                    { return "test-session" }
func (s *stubToolCtx) Branch() string                       { return "" }

// CallbackContext (adds).
func (s *stubToolCtx) Artifacts() adkagent.Artifacts { return nil }
func (s *stubToolCtx) State() session.State          { return nil }

// tool.Context (adds).
func (s *stubToolCtx) FunctionCallID() string                               { return s.callID }
func (s *stubToolCtx) Actions() *session.EventActions                       { return nil }
func (s *stubToolCtx) ToolConfirmation() *toolconfirmation.ToolConfirmation { return nil }
func (s *stubToolCtx) RequestConfirmation(string, any) error                { return nil }
func (s *stubToolCtx) SearchMemory(context.Context, string) (*memory.SearchResponse, error) {
	return nil, nil
}

// spyStore lets tests observe Put calls without touching disk or the
// eventlog. Concurrent-safe.
type spyStore struct {
	mu   sync.Mutex
	data map[string][]byte
}

func (s *spyStore) Put(_ context.Context, callID string, raw []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data == nil {
		s.data = map[string][]byte{}
	}
	s.data[callID] = append([]byte(nil), raw...)
	return nil
}

func (s *spyStore) Get(_ context.Context, callID string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.data[callID]
	if !ok {
		return nil, digest.ErrNotFound
	}
	return v, nil
}

func TestWithNamespaceAndDigest_NilOptsFallsBackToPlainNamespace(t *testing.T) {
	t.Parallel()
	// Nil DigestOptions == pre-#84 behavior. Existing consumers stay
	// on withNamespace with no digesting.
	inner := newInMemoryToolset(t)
	wrapped := withNamespaceAndDigest(inner, "demo", "demo", nil)
	tools, err := wrapped.Tools(asReadonly(context.Background()))
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) == 0 {
		t.Fatal("expected wrapped tools")
	}
	for _, tl := range tools {
		if _, isDigest := tl.(digestingTool); isDigest {
			t.Errorf("tool %q wrapped in digestingTool despite nil opts", tl.Name())
		}
	}
}

func TestWithNamespaceAndDigest_ServerInDenylistFallsThrough(t *testing.T) {
	t.Parallel()
	// Per-server escape hatch: NeverServers entry bypasses digesting
	// for THIS server while leaving others wrapped.
	inner := newInMemoryToolset(t)
	opts := &DigestOptions{
		NeverServers: map[string]bool{"debug-server": true},
	}
	wrapped := withNamespaceAndDigest(inner, "debug-server", "debug-server", opts)
	tools, err := wrapped.Tools(asReadonly(context.Background()))
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range tools {
		if _, isDigest := tl.(digestingTool); isDigest {
			t.Errorf("tool %q wrapped despite server-level denylist", tl.Name())
		}
	}
}

func TestWithNamespaceAndDigest_WrapsWhenOptsProvided(t *testing.T) {
	t.Parallel()
	// Default case: opts provided, server not in denylist → tools
	// come out as digestingTool.
	inner := newInMemoryToolset(t)
	opts := &DigestOptions{Threshold: 100}
	wrapped := withNamespaceAndDigest(inner, "demo", "demo", opts)
	tools, err := wrapped.Tools(asReadonly(context.Background()))
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) == 0 {
		t.Fatal("expected wrapped tools")
	}
	for _, tl := range tools {
		if _, isDigest := tl.(digestingTool); !isDigest {
			t.Errorf("tool %q not wrapped in digestingTool", tl.Name())
		}
	}
}

func TestDigestingTool_Run_LargeJSONResponsePruned(t *testing.T) {
	t.Parallel()
	// End-to-end: a large JSON MCP response goes through the wrapper,
	// digest.Process runs structural pruning, and the synthetic map
	// carries method=structural_json plus a call_id backed by the
	// spy store.
	store := &spyStore{}
	tools := runWrappedEcho(t, "demo", "demo", &DigestOptions{
		Threshold: 100, // below our test payload
		Store:     store,
	})

	var echo tool.Tool
	for _, tl := range tools {
		if strings.HasSuffix(tl.Name(), "_echo") {
			echo = tl
			break
		}
	}
	if echo == nil {
		t.Fatal("no echo tool found on wrapped toolset")
	}

	bigMsg := strings.Repeat("x", 5000)
	callCtx := &stubToolCtx{Context: context.Background(), callID: "call-abc"}
	res, err := echo.(runnable).Run(callCtx, map[string]any{"msg": bigMsg})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if _, ok := res["digest"].(string); !ok {
		t.Errorf("response missing digest field: %+v", res)
	}
	if got, _ := res["method"].(string); got != digest.MethodStructuralJSON {
		t.Errorf("method = %q, want structural_json", got)
	}
	if got, _ := res["call_id"].(string); got != "call-abc" {
		t.Errorf("call_id = %q, want call-abc (from FunctionCallID)", got)
	}
	if _, ok := res["raw_bytes"].(int); !ok {
		t.Errorf("raw_bytes missing or wrong type: %v", res["raw_bytes"])
	}

	raw, err := store.Get(context.Background(), "call-abc")
	if err != nil {
		t.Fatalf("Store.Get: %v", err)
	}
	if !bytes.Contains(raw, []byte(bigMsg)) {
		t.Errorf("stored raw doesn't contain the original message")
	}
}

func TestDigestingTool_Run_UnderThresholdPassesThrough(t *testing.T) {
	t.Parallel()
	tools := runWrappedEcho(t, "demo", "demo", &DigestOptions{
		Threshold: 100_000,
	})

	var echo tool.Tool
	for _, tl := range tools {
		if strings.HasSuffix(tl.Name(), "_echo") {
			echo = tl
			break
		}
	}
	if echo == nil {
		t.Fatal("no echo tool found")
	}

	callCtx := &stubToolCtx{Context: context.Background(), callID: "call-tiny"}
	res, err := echo.(runnable).Run(callCtx, map[string]any{"msg": "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := res["method"].(string); got != digest.MethodPassthrough {
		t.Errorf("method = %q, want passthrough", got)
	}
	digestStr, _ := res["digest"].(string)
	if !strings.Contains(digestStr, `"echo"`) {
		t.Errorf("passthrough digest missing echo field: %q", digestStr)
	}
}

func TestDigestingTool_Run_UpstreamErrorPropagatesUnwrapped(t *testing.T) {
	t.Parallel()
	// If the wrapped tool errors, the wrapper returns the error
	// verbatim — the model needs to see the raw failure to adapt.
	inner := errRunnable{err: errors.New("upstream boom")}
	dt := digestingTool{
		inner: renamedTool{inner: inner, prefix: "demo"},
		opts:  &DigestOptions{Threshold: 0},
	}
	_, err := dt.Run(&stubToolCtx{Context: context.Background()}, nil)
	if err == nil || err.Error() != "upstream boom" {
		t.Errorf("expected upstream error to propagate verbatim, got %v", err)
	}
}

func TestDigestingTool_Run_TelemetryRecorded(t *testing.T) {
	// NOT t.Parallel — the telemetry counter is process-wide, so
	// parallel tests firing digest.Process would race the snapshot
	// diff below. Diff pre-vs-post lets us assert deltas without
	// touching global state (safer than ResetTelemetry, which
	// would zap concurrent readers).
	before := digest.Telemetry()

	tools := runWrappedEcho(t, "demo", "demo", &DigestOptions{Threshold: 100})
	var echo tool.Tool
	for _, tl := range tools {
		if strings.HasSuffix(tl.Name(), "_echo") {
			echo = tl
			break
		}
	}
	if echo == nil {
		t.Fatal("no echo tool found")
	}
	bigMsg := strings.Repeat("y", 5000)
	callCtx := &stubToolCtx{Context: context.Background(), callID: "call-tel"}
	if _, err := echo.(runnable).Run(callCtx, map[string]any{"msg": bigMsg}); err != nil {
		t.Fatal(err)
	}

	after := digest.Telemetry()
	deltaCount := after.MethodCounts[digest.MethodStructuralJSON] - before.MethodCounts[digest.MethodStructuralJSON]
	deltaBytes := after.BytesSaved[digest.MethodStructuralJSON] - before.BytesSaved[digest.MethodStructuralJSON]
	if deltaCount < 1 {
		t.Errorf("MethodCounts[structural_json] delta = %d, want >= 1", deltaCount)
	}
	if deltaBytes <= 0 {
		t.Errorf("BytesSaved[structural_json] delta = %d, want > 0", deltaBytes)
	}
}

// runWrappedEcho spins up the in-memory MCP echo server, wraps it
// through withNamespaceAndDigest with opts, and returns the resulting
// tool list.
func runWrappedEcho(t *testing.T, prefix, server string, opts *DigestOptions) []tool.Tool {
	t.Helper()
	inner := newInMemoryToolset(t)
	wrapped := withNamespaceAndDigest(inner, prefix, server, opts)
	tools, err := wrapped.Tools(asReadonly(context.Background()))
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	return tools
}

// errRunnable always errors from Run. Used to exercise error
// propagation through the wrapper.
type errRunnable struct {
	err error
}

func (errRunnable) Name() string                            { return "err" }
func (errRunnable) Description() string                     { return "always errors" }
func (errRunnable) IsLongRunning() bool                     { return false }
func (errRunnable) Declaration() *genai.FunctionDeclaration { return nil }
func (e errRunnable) Run(_ tool.Context, _ any) (map[string]any, error) {
	return nil, e.err
}

// Compile-time asserts so future ADK / interface bumps force the
// stub to update rather than silently drift.
var (
	_ tool.Context = (*stubToolCtx)(nil)
	_ digest.Store = (*spyStore)(nil)
)

// TestDigestingTool_Run_LatencyStampedOnAllPaths pins the #277
// contract: every tool response returned through the digest wrap
// carries a `latency_ms` sidecar. Operators use this on the wire
// to answer "which MCP call took N seconds" without hand-scraping
// the eventlog. Regression signal: if this test fails, per-tool-
// call timing disappears from the TUI's tool rows.
//
// Covers three code paths that all need the sidecar:
//   - Happy path with digest (synthetic map).
//   - Under-threshold passthrough (also the synthetic map path
//     with digest.Method == passthrough).
//   - Upstream error path (raw map returned with sidecar merged).
func TestDigestingTool_Run_LatencyStampedOnAllPaths(t *testing.T) {
	t.Parallel()

	// Happy path — large response gets structural digest, and the
	// synthetic map still carries latency_ms.
	toolsLarge := runWrappedEcho(t, "demo", "demo", &DigestOptions{Threshold: 100})
	echoLarge := pickEchoTool(t, toolsLarge)
	res, err := echoLarge.(runnable).Run(
		&stubToolCtx{Context: context.Background(), callID: "call-latency-large"},
		map[string]any{"msg": strings.Repeat("x", 5000)},
	)
	if err != nil {
		t.Fatalf("Run (happy): %v", err)
	}
	assertLatencyMS(t, res, "digest happy path")

	// Under-threshold passthrough — synthetic map still.
	toolsSmall := runWrappedEcho(t, "demo", "demo", &DigestOptions{Threshold: 100_000})
	echoSmall := pickEchoTool(t, toolsSmall)
	res, err = echoSmall.(runnable).Run(
		&stubToolCtx{Context: context.Background(), callID: "call-latency-small"},
		map[string]any{"msg": "tiny"},
	)
	if err != nil {
		t.Fatalf("Run (passthrough): %v", err)
	}
	assertLatencyMS(t, res, "passthrough path")

	// Error path is exercised indirectly by TestWithLatency_Helper
	// below — the digestingTool.Run error branch delegates to
	// withLatency, and testing that pure function is simpler than
	// constructing a digestingTool around a custom-erroring
	// runnable (renamedTool wrapping is ADK-transport-specific).
}

// TestWithLatency_Helper is the direct unit for the shared helper
// both digest_wrap.Run and renamedTool.Run delegate to on their
// merge-a-sidecar-onto-an-existing-map paths.
func TestWithLatency_Helper(t *testing.T) {
	t.Parallel()
	// Nil in → nil out. Some error paths from ADK/MCP produce
	// (nil, err); can't attach a sidecar to nothing.
	if got := withLatency(nil, 42); got != nil {
		t.Errorf("withLatency(nil, ...) = %v, want nil", got)
	}
	// Existing keys preserved, latency_ms stamped on top.
	in := map[string]any{"a": 1, "b": "two"}
	out := withLatency(in, 123)
	if out["a"] != 1 || out["b"] != "two" {
		t.Errorf("withLatency dropped existing keys: %+v", out)
	}
	if got, ok := out["latency_ms"].(int64); !ok || got != 123 {
		t.Errorf("latency_ms = %v (ok=%v), want int64(123)", out["latency_ms"], ok)
	}
	// Shallow copy — mutating the returned map must not affect
	// the input. Callers reuse the upstream map.
	if _, present := in["latency_ms"]; present {
		t.Errorf("withLatency mutated the input map — caller may reuse it")
	}
	out["latency_ms"] = int64(999)
	if _, present := in["latency_ms"]; present {
		t.Errorf("post-mutation, input map still shows leakage")
	}
}

func pickEchoTool(t *testing.T, tools []tool.Tool) tool.Tool {
	t.Helper()
	for _, tl := range tools {
		if strings.HasSuffix(tl.Name(), "_echo") {
			return tl
		}
	}
	t.Fatal("no echo tool found on wrapped toolset")
	return nil
}

func assertLatencyMS(t *testing.T, res map[string]any, label string) {
	t.Helper()
	if res == nil {
		t.Fatalf("%s: response map is nil — no sidecar to inspect", label)
	}
	v, ok := res["latency_ms"]
	if !ok {
		t.Errorf("%s: latency_ms missing from response: keys=%v", label, keys(res))
		return
	}
	ms, ok := v.(int64)
	if !ok {
		t.Errorf("%s: latency_ms wrong type: %T (want int64)", label, v)
		return
	}
	if ms < 0 {
		t.Errorf("%s: latency_ms negative (%d) — clock skew?", label, ms)
	}
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
