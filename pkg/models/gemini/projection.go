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

	"google.golang.org/adk/session"
	"google.golang.org/genai"
)

// GroundingProjection wraps a session.Service so that every event
// carrying Gemini server-side built-in evidence (today: GoogleSearch
// grounding) is followed by one or more synthetic events surfacing
// that evidence under a stable Author namespace:
//
//   - "gemini/google_search" — one event per web search query the
//     model issued, then one per grounded web source (title + URI)
//
// Synthetic events share the parent event's InvocationID and Branch
// so they participate in eventlog queries (WithAuthor,
// WithBranchPrefix, WithSessionTree). Their Content.Role is left
// empty so ADK's content processor skips them when assembling the
// LLM's conversation history — they're for audit + display, not
// conversation context.
//
// Wiring:
//
//	svc, _ := eventlog.Open(ctx, dialector)
//	svc.Service = gemini.GroundingProjection(svc.Service) // wrap before WithSessionService
//	agent.New(model, agent.WithSessionService(svc.Service))
//
// URLContext evidence is not projected today: ADK's gemini wrapper
// drops URLContextMetadata at conversion (only GroundingMetadata is
// lifted onto model.LLMResponse). Capturing it would require
// intercepting raw genai responses below the ADK boundary; deferred
// until a consumer needs it.
func GroundingProjection(inner session.Service) session.Service {
	return &groundingProjection{inner: inner}
}

type groundingProjection struct {
	inner session.Service
}

func (g *groundingProjection) Create(ctx context.Context, req *session.CreateRequest) (*session.CreateResponse, error) {
	return g.inner.Create(ctx, req)
}

func (g *groundingProjection) Get(ctx context.Context, req *session.GetRequest) (*session.GetResponse, error) {
	return g.inner.Get(ctx, req)
}

func (g *groundingProjection) List(ctx context.Context, req *session.ListRequest) (*session.ListResponse, error) {
	return g.inner.List(ctx, req)
}

func (g *groundingProjection) Delete(ctx context.Context, req *session.DeleteRequest) error {
	return g.inner.Delete(ctx, req)
}

func (g *groundingProjection) AppendEvent(ctx context.Context, sess session.Session, ev *session.Event) error {
	if err := g.inner.AppendEvent(ctx, sess, ev); err != nil {
		return err
	}
	for _, syn := range projectGrounding(ev) {
		if err := g.inner.AppendEvent(ctx, sess, syn); err != nil {
			return err
		}
	}
	return nil
}

// AuthorGoogleSearch is the synthetic event Author used for both
// "model issued a search query" and "model grounded on this web
// source" projected events. Stable across releases so eventlog
// consumers can filter on it.
const AuthorGoogleSearch = "gemini/google_search"

// projectGrounding returns synthetic events derived from ev's
// GroundingMetadata. Returns nil when ev carries no grounding
// evidence — most events have none, so the common path is cheap.
func projectGrounding(ev *session.Event) []*session.Event {
	if ev == nil || ev.GroundingMetadata == nil {
		return nil
	}
	gm := ev.GroundingMetadata
	if len(gm.WebSearchQueries) == 0 && len(gm.GroundingChunks) == 0 {
		return nil
	}
	// Vertex sometimes repeats the same query or grounding URI across
	// entries (e.g. one per search round-trip); dedupe so the
	// eventlog audit trail has one row per distinct thing the model
	// actually did.
	out := make([]*session.Event, 0, len(gm.WebSearchQueries)+len(gm.GroundingChunks))
	seen := make(map[string]struct{})
	add := func(text string) {
		if _, ok := seen[text]; ok {
			return
		}
		seen[text] = struct{}{}
		out = append(out, syntheticEvent(ev, AuthorGoogleSearch, text))
	}
	for _, q := range gm.WebSearchQueries {
		if q == "" {
			continue
		}
		add("query: " + q)
	}
	for _, ch := range gm.GroundingChunks {
		if ch == nil || ch.Web == nil || ch.Web.URI == "" {
			// A grounded source without a URI isn't actionable; skip.
			continue
		}
		text := ch.Web.URI
		if ch.Web.Title != "" {
			text = ch.Web.Title + " — " + ch.Web.URI
		}
		add(text)
	}
	return out
}

// syntheticEvent builds an event derived from parent with the given
// author and a single text part. Role is intentionally left empty
// so ADK's content processor (which skips events with empty Role
// when building LLM context) treats this as metadata, not history.
func syntheticEvent(parent *session.Event, author, text string) *session.Event {
	syn := session.NewEvent(parent.InvocationID)
	syn.Author = author
	syn.Branch = parent.Branch
	syn.Content = &genai.Content{
		Parts: []*genai.Part{{Text: text}},
	}
	return syn
}
