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

import (
	"context"
	"errors"
	"iter"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/glebarez/sqlite"
	adkmodel "google.golang.org/adk/model"
	"google.golang.org/adk/session"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/pkg/eventlog"
)

// minimalLLM satisfies adkmodel.LLM with the smallest possible
// surface — enough to let agent.New succeed without hitting an
// actual provider. Tests in this file don't drive Run(), so
// GenerateContent never fires.
type minimalLLM struct{}

func (minimalLLM) Name() string { return "minimal" }
func (minimalLLM) GenerateContent(_ context.Context, _ *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		yield(nil, errors.New("minimalLLM should not be invoked in this test"))
	}
}

// recordingService is a no-op session.Service that lets tests assert
// Agent wired the exact instance they passed in via object identity.
type recordingService struct{}

func (*recordingService) Create(context.Context, *session.CreateRequest) (*session.CreateResponse, error) {
	return nil, errors.New("not implemented")
}
func (*recordingService) Get(context.Context, *session.GetRequest) (*session.GetResponse, error) {
	return nil, errors.New("not implemented")
}
func (*recordingService) List(context.Context, *session.ListRequest) (*session.ListResponse, error) {
	return nil, errors.New("not implemented")
}
func (*recordingService) Delete(context.Context, *session.DeleteRequest) error {
	return errors.New("not implemented")
}
func (*recordingService) AppendEvent(context.Context, session.Session, *session.Event) error {
	return errors.New("not implemented")
}

// recordingLLM captures each LLMRequest it sees so tests can assert
// what the runner constructed from session state. Returns a canned
// "ok" TurnComplete event.
type recordingLLM struct {
	mu       sync.Mutex
	requests []*adkmodel.LLMRequest
}

func (r *recordingLLM) Name() string { return "recording" }

func (r *recordingLLM) GenerateContent(_ context.Context, req *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	r.mu.Lock()
	r.requests = append(r.requests, req)
	r.mu.Unlock()
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		yield(&adkmodel.LLMResponse{
			Content: &genai.Content{
				Role:  genai.RoleModel,
				Parts: []*genai.Part{{Text: "ok"}},
			},
			TurnComplete: true,
		}, nil)
	}
}

func (r *recordingLLM) lastRequest() *adkmodel.LLMRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.requests) == 0 {
		return nil
	}
	return r.requests[len(r.requests)-1]
}

func TestNew_RejectsNilModel(t *testing.T) {
	t.Parallel()
	if _, err := New(nil); err == nil {
		t.Fatalf("expected error from nil model, got nil")
	}
}

func TestNew_DefaultUsesInMemorySessionService(t *testing.T) {
	t.Parallel()
	a, err := New(minimalLLM{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if a.SessionService() == nil {
		t.Fatalf("SessionService() = nil; expected the default in-memory service")
	}
	// Two agents constructed without WithSessionService should each
	// get their own service instance — that's the contract of the
	// default factory (one fresh InMemoryService per call). If a
	// future change accidentally shares a single global, this test
	// catches it.
	b, err := New(minimalLLM{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if a.SessionService() == b.SessionService() {
		t.Errorf("two default agents share the same Service instance; they should each get a fresh one")
	}
}

func TestNew_WithSessionService_PassedThrough(t *testing.T) {
	t.Parallel()
	svc := &recordingService{}
	a, err := New(minimalLLM{}, WithSessionService(svc))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got := a.SessionService()
	if got != session.Service(svc) {
		t.Errorf("SessionService() = %p, want the exact instance we passed (%p)", got, svc)
	}
}

func TestNew_WithSessionService_NilFallsBackToDefault(t *testing.T) {
	t.Parallel()
	// Passing nil shouldn't panic and shouldn't leave the agent
	// without a service. The default in-memory service should kick
	// in transparently — same shape as if WithSessionService had
	// not been called at all.
	a, err := New(minimalLLM{}, WithSessionService(nil))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if a.SessionService() == nil {
		t.Fatalf("WithSessionService(nil) should fall back to default; got nil")
	}
}

func TestNew_OptionOrderIndependent(t *testing.T) {
	t.Parallel()
	// WithSessionService should win regardless of where it appears
	// in the option list — same convention the other With* options
	// follow.
	svc := &recordingService{}
	a, err := New(minimalLLM{},
		WithName("first"),
		WithSessionService(svc),
		WithInstruction("last"),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if a.SessionService() != session.Service(svc) {
		t.Errorf("SessionService not preserved across other options")
	}
}

func TestNew_WithEventLog_WiresServiceAndExposesHandle(t *testing.T) {
	t.Parallel()
	dsn := filepath.Join(t.TempDir(), "session.db")
	h, err := eventlog.Open(context.Background(), sqlite.Open(dsn))
	if err != nil {
		t.Fatalf("eventlog.Open: %v", err)
	}
	defer h.Close()

	a, err := New(minimalLLM{}, WithEventLog(h))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if a.SessionService() != h.Service {
		t.Errorf("WithEventLog should install Handle.Service as the session.Service")
	}
	if a.EventLog() != h {
		t.Errorf("EventLog() should return the Handle that was passed")
	}
}

func TestNew_WithEventLog_NilIsNoop(t *testing.T) {
	t.Parallel()
	a, err := New(minimalLLM{}, WithEventLog(nil))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if a.SessionService() == nil {
		t.Errorf("WithEventLog(nil) should leave the default session service in place")
	}
	if a.EventLog() != nil {
		t.Errorf("WithEventLog(nil) should not stash a Handle")
	}
}

func TestDefaultInstruction_GuardsLoadBearingSubstrings(t *testing.T) {
	t.Parallel()
	// Each substring guards a load-bearing piece of DefaultInstruction.
	// If a reword removes one, this test fails so the change surfaces
	// in review rather than silently dropping behavior we depend on.
	for _, want := range []string{
		"in parallel",                      // parallelism mandate
		"do not execute them one by one",   // anti-serial instruction
		"parallel writes to the same file", // edit-collision guard
		"Efficiency is secondary",          // quality caveat
		"sketch your plan",                 // pre-action plan nudge
		"Skip the preamble for trivial",    // don't-narrate-trivial guard
	} {
		if !strings.Contains(DefaultInstruction, want) {
			t.Errorf("DefaultInstruction missing required substring %q", want)
		}
	}
}

func TestRunWithContents_FeedsHistoryToLLM(t *testing.T) {
	t.Parallel()
	rec := &recordingLLM{}
	a, err := New(rec)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	contents := []*genai.Content{
		{Role: genai.RoleUser, Parts: []*genai.Part{{Text: "first user message"}}},
		{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "first model reply"}}},
		{Role: genai.RoleUser, Parts: []*genai.Part{{Text: "follow-up question"}}},
	}
	for _, err := range a.RunWithContents(context.Background(), contents) {
		if err != nil {
			t.Fatalf("RunWithContents: %v", err)
		}
	}

	req := rec.lastRequest()
	if req == nil {
		t.Fatal("LLM was never invoked")
	}
	// The runner should have built the LLMRequest from session events
	// (pre-populated history) plus the new user message. We expect to
	// see all three contents in some form.
	got := flattenText(req.Contents)
	for _, want := range []string{"first user message", "first model reply", "follow-up question"} {
		if !strings.Contains(got, want) {
			t.Errorf("LLM request missing %q. Full request text: %q", want, got)
		}
	}
}

func TestDefaultOptions_UsesDefaultInstruction(t *testing.T) {
	t.Parallel()
	if got := defaultOptions().instruction; got != DefaultInstruction {
		t.Errorf("defaultOptions().instruction = %q, want DefaultInstruction", got)
	}
}

func TestRunWithContents_FreshSessionPerCall(t *testing.T) {
	t.Parallel()
	rec := &recordingLLM{}
	a, err := New(rec)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// First call with one history item.
	first := []*genai.Content{
		{Role: genai.RoleUser, Parts: []*genai.Part{{Text: "session A"}}},
	}
	for _, err := range a.RunWithContents(context.Background(), first) {
		if err != nil {
			t.Fatalf("first RunWithContents: %v", err)
		}
	}

	// Second call with completely different history. If sessions were
	// shared, the second call's LLM request would still reference
	// "session A" content from the first call.
	second := []*genai.Content{
		{Role: genai.RoleUser, Parts: []*genai.Part{{Text: "session B"}}},
	}
	for _, err := range a.RunWithContents(context.Background(), second) {
		if err != nil {
			t.Fatalf("second RunWithContents: %v", err)
		}
	}

	got := flattenText(rec.lastRequest().Contents)
	if strings.Contains(got, "session A") {
		t.Errorf("second call leaked content from first call's session: %q", got)
	}
	if !strings.Contains(got, "session B") {
		t.Errorf("second call missing its own content: %q", got)
	}
}

func TestRunWithContents_RejectsEmpty(t *testing.T) {
	t.Parallel()
	rec := &recordingLLM{}
	a, err := New(rec)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, err := range a.RunWithContents(context.Background(), nil) {
		if err == nil || !strings.Contains(err.Error(), "contents is empty") {
			t.Errorf("expected empty-contents error, got %v", err)
		}
		return
	}
	t.Fatal("expected an iteration with an error")
}

func TestRunWithContents_RejectsNonUserTrailing(t *testing.T) {
	t.Parallel()
	rec := &recordingLLM{}
	a, err := New(rec)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	contents := []*genai.Content{
		{Role: genai.RoleUser, Parts: []*genai.Part{{Text: "q"}}},
		{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "a"}}},
	}
	for _, err := range a.RunWithContents(context.Background(), contents) {
		if err == nil || !strings.Contains(err.Error(), "last content must be a user message") {
			t.Errorf("expected non-user-trailing error, got %v", err)
		}
		return
	}
	t.Fatal("expected an iteration with an error")
}

// flattenText concatenates all text parts across contents into one
// string for substring assertions.
func flattenText(contents []*genai.Content) string {
	var b strings.Builder
	for _, c := range contents {
		if c == nil {
			continue
		}
		for _, p := range c.Parts {
			if p != nil && p.Text != "" {
				b.WriteString(p.Text)
				b.WriteByte('\n')
			}
		}
	}
	return b.String()
}
