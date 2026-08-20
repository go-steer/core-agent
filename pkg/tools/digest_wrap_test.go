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

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"google.golang.org/adk/model"
	adktool "google.golang.org/adk/tool"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v2/pkg/digest"
)

// surveyTool is a stand-in for a built-in whose response size the
// test controls. Named so it can impersonate any built-in — which is
// the point of half these tests, since the wrap dispatches on name.
type surveyTool struct {
	name string
	resp map[string]any
	err  error
}

func (s *surveyTool) Name() string        { return s.name }
func (s *surveyTool) Description() string { return "survey" }
func (s *surveyTool) IsLongRunning() bool { return false }
func (s *surveyTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{Name: s.name}
}
func (s *surveyTool) Run(_ adktool.Context, _ any) (map[string]any, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.resp, nil
}

// contentRootResponse builds a read_many_files-shaped payload of
// roughly the size #706 reported: a whole content root walked with
// `pattern: "*"`, ~54KB of file bodies.
func contentRootResponse(files, bodyBytes int) map[string]any {
	out := make([]any, 0, files)
	for i := range files {
		out = append(out, map[string]any{
			"path":    fmt.Sprintf("/opt/platform-agent-native/skills/skill-%02d/SKILL.md", i),
			"content": strings.Repeat("the quick brown fox jumps over the lazy dog. ", bodyBytes/45),
		})
	}
	return map[string]any{"files": out}
}

func newTestStore(t *testing.T) digest.Store {
	t.Helper()
	st, err := digest.NewFilesystemStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFilesystemStore: %v", err)
	}
	return st
}

func mustMarshalLen(t *testing.T, v any) int {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return len(b)
}

func runWrapped(t *testing.T, opts *DigestOptions, inner adktool.Tool) (map[string]any, adktool.Tool) {
	t.Helper()
	wrapped := NewDigester(opts).Wrap([]adktool.Tool{inner})
	if len(wrapped) != 1 {
		t.Fatalf("Wrap returned %d tools, want 1", len(wrapped))
	}
	rn, ok := wrapped[0].(runnableTool)
	if !ok {
		t.Fatalf("wrapped tool %T is not runnable", wrapped[0])
	}
	got, err := rn.Run(&planToolCtx{Context: context.Background()}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return got, wrapped[0]
}

// TestDigester_OversizeSurveyIsDigested is the #706 symptom itself:
// read_many_files over a content root used to hand the model every
// byte it walked. It must now arrive as a digest.
func TestDigester_OversizeSurveyIsDigested(t *testing.T) {
	t.Parallel()
	resp := contentRootResponse(20, 2700) // ~54KB, the reported size
	original := mustMarshalLen(t, resp)
	if original < 40_000 {
		t.Fatalf("fixture is only %d bytes; it must reproduce the reported ~54KB", original)
	}

	got, _ := runWrapped(t, &DigestOptions{Store: newTestStore(t)},
		&surveyTool{name: "read_many_files", resp: resp})

	if _, ok := got["files"]; ok {
		t.Errorf("response still carries the raw `files` array: %v", keysOf(got))
	}
	dg, ok := got["digest"].(string)
	if !ok || dg == "" {
		t.Fatalf("no digest in response: %v", keysOf(got))
	}
	if len(dg) >= original {
		t.Errorf("digest is %d bytes against a %d-byte payload — no saving", len(dg), original)
	}
	if got["raw_bytes"] != original {
		t.Errorf("raw_bytes = %v, want %d", got["raw_bytes"], original)
	}
	if got["call_id"] != "call-1" {
		t.Errorf("call_id = %v, want the tool call id", got["call_id"])
	}
	if _, ok := got["savings"].(map[string]any); !ok {
		t.Errorf("no savings sidecar; per-session savings accounting reads that key")
	}
}

// TestDigester_RetrieveRawUndoesTheDigest is the property that makes
// digesting a survey safe: nothing is destroyed, only deferred.
func TestDigester_RetrieveRawUndoesTheDigest(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	resp := contentRootResponse(20, 2700)
	want, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	got, _ := runWrapped(t, &DigestOptions{Store: store},
		&surveyTool{name: "read_many_files", resp: resp})

	callID, _ := got["call_id"].(string)
	if callID == "" {
		t.Fatal("no call_id — the model has no way back to the payload")
	}
	raw, err := store.Get(context.Background(), callID)
	if err != nil {
		t.Fatalf("store.Get(%q): %v", callID, err)
	}
	if string(raw) != string(want) {
		t.Errorf("retrieved payload differs from the original (%d vs %d bytes)", len(raw), len(want))
	}
}

// TestDigester_UnderThresholdIsUntouched guards the one place this
// wrap deliberately differs from the MCP one. The MCP wrap always
// substitutes the synthetic map; doing that here would rewrite the
// response shape of every modest list_dir in the product.
//
// The fixture is deliberately one the pruner WOULD compress — 40
// long paths, an array it collapses head+tail — and deliberately
// under the threshold. That combination is what makes the assertion
// load-bearing: a 3-entry list_dir survives even a wrap with no size
// gate at all, because the no-worse guard catches it. This one only
// survives because the size gate ran first.
func TestDigester_UnderThresholdIsUntouched(t *testing.T) {
	t.Parallel()
	entries := make([]any, 0, 40)
	for i := range 40 {
		entries = append(entries, fmt.Sprintf("/opt/platform-agent-native/skills/skill-%02d/references/cookbook.md", i))
	}
	resp := map[string]any{"entries": entries}
	if n := mustMarshalLen(t, resp); n >= DefaultDigestThreshold {
		t.Fatalf("fixture is %d bytes; it must stay under the %d-byte threshold", n, DefaultDigestThreshold)
	}

	got, _ := runWrapped(t, &DigestOptions{Store: newTestStore(t)},
		&surveyTool{name: "list_dir", resp: resp})

	if _, digested := got["digest"]; digested {
		t.Fatalf("an under-threshold list_dir was digested: %v", keysOf(got))
	}
	if len(got["entries"].([]any)) != 40 {
		t.Errorf("response was altered: %d entries", len(got["entries"].([]any)))
	}
}

// TestDigester_OnlySurveyToolsAreWrapped pins the membership rule.
// read_file in particular must stay out: it is the narrowing move a
// survey digest points the model at, and it is what precedes an
// exact-match edit_file.
func TestDigester_OnlySurveyToolsAreWrapped(t *testing.T) {
	t.Parallel()
	huge := map[string]any{"content": strings.Repeat("x", 60_000)}

	for _, name := range []string{"read_many_files", "grep", "glob", "list_dir"} {
		t.Run(name+" is wrapped", func(t *testing.T) {
			inner := &surveyTool{name: name, resp: contentRootResponse(20, 2700)}
			got, w := runWrapped(t, &DigestOptions{Store: newTestStore(t)}, inner)
			if w == adktool.Tool(inner) {
				t.Fatalf("%s was not wrapped", name)
			}
			if _, ok := got["digest"]; !ok {
				t.Errorf("%s response was not digested: %v", name, keysOf(got))
			}
		})
	}

	for _, name := range []string{"read_file", "bash", "json_query", "retrieve_raw", "edit_file"} {
		t.Run(name+" is left alone", func(t *testing.T) {
			inner := &surveyTool{name: name, resp: huge}
			got, w := runWrapped(t, &DigestOptions{Store: newTestStore(t)}, inner)
			if w != adktool.Tool(inner) {
				t.Errorf("%s was wrapped; it must not be", name)
			}
			if _, ok := got["digest"]; ok {
				t.Errorf("%s response was digested: %v", name, keysOf(got))
			}
		})
	}
}

// TestDigester_NeverReturnsAWorseAnswer covers the payload the
// structural pruner has nothing to work with — many short keys, no
// long strings, no long arrays. Substituting a same-size "digest"
// would cost the model the response's real shape and invite a
// retrieve_raw round trip that buys it nothing.
func TestDigester_NeverReturnsAWorseAnswer(t *testing.T) {
	t.Parallel()
	flat := make(map[string]any, 1200)
	for i := range 1200 {
		flat[fmt.Sprintf("k%04d", i)] = "v"
	}
	resp := map[string]any{"matches": flat}
	if n := mustMarshalLen(t, resp); n <= DefaultDigestThreshold {
		t.Fatalf("fixture is %d bytes; it must clear the %d-byte threshold", n, DefaultDigestThreshold)
	}

	got, _ := runWrapped(t, &DigestOptions{Store: newTestStore(t)},
		&surveyTool{name: "grep", resp: resp})

	if dg, ok := got["digest"].(string); ok {
		t.Errorf("substituted a %d-byte digest for a %d-byte payload", len(dg), mustMarshalLen(t, resp))
	}
	if len(got["matches"].(map[string]any)) != 1200 {
		t.Errorf("response was altered: %d keys", len(got["matches"].(map[string]any)))
	}
}

// TestDigester_FailurePathsReturnTheOriginal — a tool error, a
// response that will not marshal, and a nil response all have to
// reach the caller unchanged. A digest is a cost optimization; it is
// never worth failing a tool call over.
func TestDigester_FailurePathsReturnTheOriginal(t *testing.T) {
	t.Parallel()
	opts := &DigestOptions{Store: newTestStore(t)}

	t.Run("inner error propagates", func(t *testing.T) {
		inner := &surveyTool{name: "grep", err: context.DeadlineExceeded}
		wrapped := NewDigester(opts).Wrap([]adktool.Tool{inner})
		got, err := wrapped[0].(runnableTool).Run(&planToolCtx{Context: context.Background()}, nil)
		if err == nil {
			t.Fatal("error was swallowed")
		}
		if got != nil {
			t.Errorf("got %v, want the inner nil response", got)
		}
	})

	t.Run("unmarshalable oversize response is returned verbatim", func(t *testing.T) {
		// A channel value fails json.Marshal. Padded past the
		// threshold so the size gate can't be what saves us.
		resp := map[string]any{"pad": strings.Repeat("x", 20_000), "ch": make(chan int)}
		inner := &surveyTool{name: "grep", resp: resp}
		wrapped := NewDigester(opts).Wrap([]adktool.Tool{inner})
		got, err := wrapped[0].(runnableTool).Run(&planToolCtx{Context: context.Background()}, nil)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if _, ok := got["ch"]; !ok {
			t.Errorf("response was replaced despite the marshal failure: %v", keysOf(got))
		}
	})
}

// TestDigester_NilOptionsIsTheIdentity keeps --no-mcp-digest a
// genuine kill switch rather than a differently-configured wrap.
func TestDigester_NilOptionsIsTheIdentity(t *testing.T) {
	t.Parallel()
	inner := &surveyTool{name: "read_many_files", resp: contentRootResponse(20, 2700)}
	in := []adktool.Tool{inner}
	if got := NewDigester(nil).Wrap(in); got[0] != adktool.Tool(inner) {
		t.Errorf("nil options wrapped the tool: %T", got[0])
	}
	if got := NewDigester(nil).WrapToolset(&fakeToolset{tools: in}); got == nil {
		t.Error("nil options must pass the toolset through, not drop it")
	}
}

// TestDigester_ToolsetWrapsLazily — skills and rooted subagents
// arrive as toolsets, resolved per Tools() call.
func TestDigester_ToolsetWrapsLazily(t *testing.T) {
	t.Parallel()
	inner := &surveyTool{name: "grep", resp: contentRootResponse(20, 2700)}
	ts := NewDigester(&DigestOptions{Store: newTestStore(t)}).
		WrapToolset(&fakeToolset{tools: []adktool.Tool{inner}})

	got, err := ts.Tools(nil)
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	res, err := got[0].(runnableTool).Run(&planToolCtx{Context: context.Background()}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, ok := res["digest"]; !ok {
		t.Errorf("toolset-resolved tool was not digested: %v", keysOf(res))
	}
}

// TestDigester_ProcessRequestPacksTheWrapper mirrors the gate,
// serializer, and timer contract: ADK dispatch has to route through
// the wrapper, not around it.
func TestDigester_ProcessRequestPacksTheWrapper(t *testing.T) {
	t.Parallel()
	inner := &surveyTool{name: "grep", resp: map[string]any{"ok": true}}
	wrapped := NewDigester(&DigestOptions{}).Wrap([]adktool.Tool{inner})
	dt, ok := wrapped[0].(*digestingTool)
	if !ok {
		t.Fatalf("wrapped tool is %T, want *digestingTool", wrapped[0])
	}
	req := &model.LLMRequest{}
	if err := dt.ProcessRequest(nil, req); err != nil {
		t.Fatalf("ProcessRequest: %v", err)
	}
	if req.Tools["grep"] != adktool.Tool(dt) {
		t.Errorf("req.Tools[grep] = %T, want the *digestingTool wrapper", req.Tools["grep"])
	}
	if dt.Declaration() == nil || dt.Declaration().Name != "grep" {
		t.Error("Declaration not forwarded from the inner tool")
	}
}

// TestDigester_ReadOnlyHintIsForwarded — every tool in the set is
// read-only, so dropping the forward would reclassify the whole set
// as mutating for dispatch purposes (#460).
func TestDigester_ReadOnlyHintIsForwarded(t *testing.T) {
	t.Parallel()
	wrapped := NewDigester(&DigestOptions{}).Wrap([]adktool.Tool{&hintingSurveyTool{surveyTool{name: "grep"}}})
	h, ok := wrapped[0].(ReadOnlyHinter)
	if !ok {
		t.Fatal("wrapper does not implement ReadOnlyHinter")
	}
	if !h.ReadOnlyHint() {
		t.Error("ReadOnlyHint was not forwarded from the inner tool")
	}
}

// TestDigester_WrapIsIdempotent — a second layer would digest the
// first layer's synthetic map and stamp a second call_id over the one
// that pointed at the real payload.
func TestDigester_WrapIsIdempotent(t *testing.T) {
	t.Parallel()
	d := NewDigester(&DigestOptions{Store: newTestStore(t)})
	once := d.Wrap([]adktool.Tool{&surveyTool{name: "grep", resp: contentRootResponse(20, 2700)}})
	twice := d.Wrap(once)
	if twice[0] != once[0] {
		t.Fatalf("second Wrap produced a new wrapper: %T", twice[0])
	}
	got, err := twice[0].(runnableTool).Run(&planToolCtx{Context: context.Background()}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if dg, _ := got["digest"].(string); strings.Contains(dg, `"raw_bytes"`) {
		t.Errorf("digest is a digest of a digest: %q", dg[:min(200, len(dg))])
	}
}

type hintingSurveyTool struct{ surveyTool }

func (h *hintingSurveyTool) ReadOnlyHint() bool { return true }

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
