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

package gemini

import (
	"context"
	"errors"
	"strings"
	"testing"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/adk/session"
	"google.golang.org/genai"
)

// fakeSessionService records every AppendEvent for assertion. It's
// the minimum surface we need — the wrapper just forwards Create /
// Get / List / Delete, which is exercised separately.
type fakeSessionService struct {
	appended  []*session.Event
	appendErr error
}

func (f *fakeSessionService) Create(_ context.Context, _ *session.CreateRequest) (*session.CreateResponse, error) {
	return &session.CreateResponse{}, nil
}
func (f *fakeSessionService) Get(_ context.Context, _ *session.GetRequest) (*session.GetResponse, error) {
	return &session.GetResponse{}, nil
}
func (f *fakeSessionService) List(_ context.Context, _ *session.ListRequest) (*session.ListResponse, error) {
	return &session.ListResponse{}, nil
}
func (f *fakeSessionService) Delete(_ context.Context, _ *session.DeleteRequest) error {
	return nil
}
func (f *fakeSessionService) AppendEvent(_ context.Context, _ session.Session, ev *session.Event) error {
	if f.appendErr != nil {
		return f.appendErr
	}
	f.appended = append(f.appended, ev)
	return nil
}

func eventWithGrounding(queries []string, sources [][2]string) *session.Event {
	ev := session.NewEvent("inv-1")
	ev.Author = "agent"
	ev.Branch = "agent"
	ev.LLMResponse = adkmodel.LLMResponse{
		GroundingMetadata: &genai.GroundingMetadata{
			WebSearchQueries: queries,
		},
	}
	for _, s := range sources {
		ev.GroundingMetadata.GroundingChunks = append(ev.GroundingMetadata.GroundingChunks,
			&genai.GroundingChunk{Web: &genai.GroundingChunkWeb{Title: s[0], URI: s[1]}})
	}
	return ev
}

func TestGroundingProjection_NoMetadata_NoSynthetics(t *testing.T) {
	t.Parallel()
	fake := &fakeSessionService{}
	wrapped := GroundingProjection(fake)
	ev := session.NewEvent("inv-1")
	ev.Author = "agent"
	if err := wrapped.AppendEvent(context.Background(), nil, ev); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	if len(fake.appended) != 1 {
		t.Fatalf("expected 1 event (the original); got %d", len(fake.appended))
	}
}

func TestGroundingProjection_ProjectsQueriesAndSources(t *testing.T) {
	t.Parallel()
	fake := &fakeSessionService{}
	wrapped := GroundingProjection(fake)
	parent := eventWithGrounding(
		[]string{"q1", "q2"},
		[][2]string{
			{"Example", "https://example.com"},
			{"", "https://no-title.example/page"},
			{"Just title, no URI", ""}, // skipped: no URI means no actionable source
		},
	)
	if err := wrapped.AppendEvent(context.Background(), nil, parent); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	// 1 parent + 2 queries + 2 valid sources (third is dropped — no URI) = 5.
	if len(fake.appended) != 5 {
		t.Fatalf("expected 5 appended events; got %d", len(fake.appended))
	}
	if fake.appended[0] != parent {
		t.Errorf("first appended must be the parent (forwarded as-is)")
	}
	// Order: queries first, then sources.
	wantTexts := []string{
		"query: q1",
		"query: q2",
		"Example — https://example.com",
		"https://no-title.example/page",
	}
	for i, want := range wantTexts {
		syn := fake.appended[1+i]
		if syn.Author != AuthorGoogleSearch {
			t.Errorf("synthetic %d Author=%q want %q", i, syn.Author, AuthorGoogleSearch)
		}
		if syn.Branch != parent.Branch {
			t.Errorf("synthetic %d Branch=%q want %q", i, syn.Branch, parent.Branch)
		}
		if syn.InvocationID != parent.InvocationID {
			t.Errorf("synthetic %d InvocationID=%q want %q", i, syn.InvocationID, parent.InvocationID)
		}
		if syn.Content == nil || syn.Content.Role != "" {
			t.Errorf("synthetic %d Content.Role=%q want empty (so ADK skips it from LLM context)",
				i, syn.Content.Role)
		}
		if len(syn.Content.Parts) != 1 || syn.Content.Parts[0].Text != want {
			t.Errorf("synthetic %d text=%q want %q", i, syn.Content.Parts[0].Text, want)
		}
	}
}

func TestGroundingProjection_SkipsEmptyQueries(t *testing.T) {
	t.Parallel()
	fake := &fakeSessionService{}
	wrapped := GroundingProjection(fake)
	if err := wrapped.AppendEvent(context.Background(), nil,
		eventWithGrounding([]string{"", "real query"}, nil)); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	if len(fake.appended) != 2 {
		t.Fatalf("expected 1 parent + 1 query event; got %d", len(fake.appended))
	}
	got := fake.appended[1].Content.Parts[0].Text
	if got != "query: real query" {
		t.Errorf("unexpected projected text %q", got)
	}
}

func TestGroundingProjection_SkipsEmptySourceEntries(t *testing.T) {
	t.Parallel()
	fake := &fakeSessionService{}
	wrapped := GroundingProjection(fake)
	ev := session.NewEvent("inv-1")
	ev.LLMResponse = adkmodel.LLMResponse{
		GroundingMetadata: &genai.GroundingMetadata{
			GroundingChunks: []*genai.GroundingChunk{
				nil,                               // skip
				{Web: nil},                        // skip (non-web chunk)
				{Web: &genai.GroundingChunkWeb{}}, // skip (empty title + uri)
			},
		},
	}
	if err := wrapped.AppendEvent(context.Background(), nil, ev); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	if len(fake.appended) != 1 {
		t.Fatalf("expected only the parent event; got %d", len(fake.appended))
	}
}

func TestGroundingProjection_InnerErrorStopsProjection(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("inner append boom")
	fake := &fakeSessionService{appendErr: wantErr}
	wrapped := GroundingProjection(fake)
	ev := eventWithGrounding([]string{"q1"}, nil)
	err := wrapped.AppendEvent(context.Background(), nil, ev)
	if !errors.Is(err, wantErr) {
		t.Fatalf("want wantErr, got %v", err)
	}
}

func TestGroundingProjection_DelegatesNonAppendMethods(t *testing.T) {
	t.Parallel()
	fake := &fakeSessionService{}
	wrapped := GroundingProjection(fake)
	if _, err := wrapped.Create(context.Background(), &session.CreateRequest{}); err != nil {
		t.Errorf("Create: %v", err)
	}
	if _, err := wrapped.Get(context.Background(), &session.GetRequest{}); err != nil {
		t.Errorf("Get: %v", err)
	}
	if _, err := wrapped.List(context.Background(), &session.ListRequest{}); err != nil {
		t.Errorf("List: %v", err)
	}
	if err := wrapped.Delete(context.Background(), &session.DeleteRequest{}); err != nil {
		t.Errorf("Delete: %v", err)
	}
}

func TestGroundingProjection_QueryTextFormat(t *testing.T) {
	t.Parallel()
	// Pin the "query: " prefix in case consumers grep for it.
	fake := &fakeSessionService{}
	wrapped := GroundingProjection(fake)
	if err := wrapped.AppendEvent(context.Background(), nil,
		eventWithGrounding([]string{"hello world"}, nil)); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	if len(fake.appended) != 2 {
		t.Fatalf("want 2 events; got %d", len(fake.appended))
	}
	got := fake.appended[1].Content.Parts[0].Text
	if !strings.HasPrefix(got, "query: ") {
		t.Errorf("want 'query: ' prefix, got %q", got)
	}
}
