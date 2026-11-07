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

package memorybank_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/adk/v2/session"
	"google.golang.org/grpc/codes"

	"github.com/go-steer/core-agent/v3/pkg/memory"
	"github.com/go-steer/core-agent/v3/pkg/memory/memorybank"
	"github.com/go-steer/core-agent/v3/pkg/memory/memtest"
)

func openFakeBackend(t *testing.T) (*memorybank.Backend, *fakeMemoryBankServer) {
	t.Helper()
	fake := newFakeMemoryBankServer()
	b, err := memorybank.Open(context.Background(), memorybank.Config{
		ReasoningEngine: testEngine,
		ClientOptions:   fake.start(t),
		RetryBackoff:    time.Millisecond,
	})
	if err != nil {
		t.Fatalf("memorybank.Open: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b, fake
}

func TestMemoryBank_Conformance(t *testing.T) {
	memtest.RunConformance(t, func(t *testing.T) memory.Backend {
		b, _ := openFakeBackend(t)
		return b
	})
}

func TestMemoryBank_BatchingOverFiveFacts(t *testing.T) {
	b, fake := openFakeBackend(t)
	ctx := context.Background()
	scope := memory.ScopeMap{
		"namespace":  "prod",
		"scope_kind": "shared",
		"pool":       "fleet-learnings",
	}

	facts := make([]memory.CandidateFact, 12)
	for i := range facts {
		facts[i] = memory.CandidateFact{
			Fact:   fmt.Sprintf("Learned heuristic #%d on shard %d", i+1, i+1),
			Topics: []string{"capacity"},
		}
	}

	deltas, err := b.Put(ctx, scope, memory.PutRequest{
		Facts:     facts,
		WriteMode: memory.WriteConsolidate,
	})
	if err != nil {
		t.Fatalf("Put(12 facts): %v", err)
	}
	if len(deltas) != 12 {
		t.Fatalf("len(deltas) = %d, want 12", len(deltas))
	}
	if fake.generateCalls != 3 {
		t.Fatalf("generateCalls = %d, want 3 (batches of 5 + 5 + 2)", fake.generateCalls)
	}
}

func TestMemoryBank_ConsolidatePreservesPerFactAnnotations(t *testing.T) {
	b, _ := openFakeBackend(t)
	ctx := context.Background()
	scope := memory.ScopeMap{
		"namespace":  "prod",
		"scope_kind": "shared",
		"pool":       "fleet-learnings",
	}

	_, err := b.Put(ctx, scope, memory.PutRequest{
		Facts: []memory.CandidateFact{
			{
				Fact:     "Temporary maintenance window on shard-1",
				Topics:   []string{"ops"},
				Metadata: map[string]string{"owner": "team-a"},
				TTL:      10 * time.Minute,
			},
			{
				Fact:     "Permanent regional architecture invariant",
				Topics:   []string{"arch"},
				Metadata: map[string]string{"owner": "team-b"},
				TTL:      0,
			},
		},
		WriteMode: memory.WriteConsolidate,
	})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	opsItems, err := b.Retrieve(ctx, scope, memory.RetrieveRequest{Topics: []string{"ops"}, TopK: 10})
	if err != nil {
		t.Fatalf("Retrieve(ops): %v", err)
	}
	if len(opsItems) != 1 || opsItems[0].Metadata["owner"] != "team-a" || opsItems[0].ExpireAt.IsZero() {
		t.Fatalf("opsItems = %+v, want 1 item with owner=team-a and non-zero ExpireAt", opsItems)
	}

	archItems, err := b.Retrieve(ctx, scope, memory.RetrieveRequest{Topics: []string{"arch"}, TopK: 10})
	if err != nil {
		t.Fatalf("Retrieve(arch): %v", err)
	}
	if len(archItems) != 1 || archItems[0].Metadata["owner"] != "team-b" || !archItems[0].ExpireAt.IsZero() {
		t.Fatalf("archItems = %+v, want 1 item with owner=team-b and zero ExpireAt", archItems)
	}
}

type stubVertexSession struct {
	session.Session
	engineParent string
	sessionRes   string
	ok           bool
}

func (s stubVertexSession) VertexSessionResource() (string, string, bool) {
	return s.engineParent, s.sessionRes, s.ok
}

func TestMemoryBank_VertexSessionOriginFastPath(t *testing.T) {
	b, fake := openFakeBackend(t)
	ctx := context.Background()
	scope := memory.ScopeMap{
		"namespace":  "prod",
		"scope_kind": "agent",
		"agent_id":   "coordinator",
	}

	// Same ReasoningEngine parent -> zero-copy VertexSessionSource.
	sessionRes := testEngine + "/sessions/sess-abc"
	startTime := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	deltas, err := b.ConsolidateConversation(ctx, scope, memory.ConversationRequest{
		SessionID: "sess-abc",
		Session: stubVertexSession{
			engineParent: testEngine,
			sessionRes:   sessionRes,
			ok:           true,
		},
		StartTime: &startTime,
		Turns: []memory.ConversationTurn{
			{Role: "user", Text: "Should not be uploaded as DirectContentsSource when fast path matches"},
		},
	})
	if err != nil {
		t.Fatalf("ConsolidateConversation (fast path): %v", err)
	}
	if len(deltas) != 1 {
		t.Fatalf("len(deltas) = %d, want 1", len(deltas))
	}
	if fake.lastVertexSession != sessionRes {
		t.Fatalf("lastVertexSession = %q, want %q", fake.lastVertexSession, sessionRes)
	}
	if fake.lastDirectEventsCount != 0 {
		t.Fatalf("lastDirectEventsCount = %d, want 0 on VertexSessionSource fast path", fake.lastDirectEventsCount)
	}

	// Different ReasoningEngine parent -> falls back to DirectContentsSource.
	otherEngine := "projects/other-proj/locations/us-central1/reasoningEngines/engine-99"
	deltasFallback, err := b.ConsolidateConversation(ctx, scope, memory.ConversationRequest{
		SessionID: "sess-other",
		Session: stubVertexSession{
			engineParent: otherEngine,
			sessionRes:   otherEngine + "/sessions/sess-other",
			ok:           true,
		},
		Turns: []memory.ConversationTurn{
			{Role: "user", Text: "Use regional failover on us-east1 outage."},
			{Role: "model", Text: "Noted regional failover procedure."},
		},
	})
	if err != nil {
		t.Fatalf("ConsolidateConversation (fallback): %v", err)
	}
	if len(deltasFallback) != 2 {
		t.Fatalf("len(deltasFallback) = %d, want 2", len(deltasFallback))
	}
	if fake.lastDirectEventsCount != 2 {
		t.Fatalf("lastDirectEventsCount = %d, want 2 on DirectContentsSource fallback", fake.lastDirectEventsCount)
	}
}

func TestMemoryBank_DeleteAcceptsFullResourceAndRejectsCrossEngine(t *testing.T) {
	b, _ := openFakeBackend(t)
	ctx := context.Background()
	scope := memory.ScopeMap{
		"namespace":  "prod",
		"scope_kind": "agent",
		"agent_id":   "triage",
	}

	deltas, err := b.Put(ctx, scope, memory.PutRequest{
		Facts:     []memory.CandidateFact{{Fact: "Temporary fact"}},
		WriteMode: memory.WriteVerbatim,
	})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	shortID := deltas[0].ID

	// Cross-engine full resource name must be rejected before hitting the server.
	crossEngineID := "projects/other/locations/us-central1/reasoningEngines/999/memories/" + shortID
	err = b.Delete(ctx, scope, crossEngineID)
	if !errors.Is(err, memory.ErrNotFound) || !errors.Is(err, memory.ErrScopeMismatch) {
		t.Fatalf("Delete(cross-engine): want ErrNotFound + ErrScopeMismatch, got %v", err)
	}

	// Full resource name under the same engine succeeds.
	fullSameEngineID := testEngine + "/memories/" + shortID
	if err := b.Delete(ctx, scope, fullSameEngineID); err != nil {
		t.Fatalf("Delete(fullSameEngineID): %v", err)
	}
}

func TestMemoryBank_TransientRetry(t *testing.T) {
	b, fake := openFakeBackend(t)
	ctx := context.Background()
	scope := memory.ScopeMap{
		"namespace":  "prod",
		"scope_kind": "agent",
		"agent_id":   "retry-agent",
	}

	// Inject a single ResourceExhausted on GenerateMemories -> retry succeeds.
	fake.mu.Lock()
	fake.failGenerate = codes.ResourceExhausted
	fake.failGenerateN = 1
	fake.mu.Unlock()

	deltas, err := b.Put(ctx, scope, memory.PutRequest{
		Facts:     []memory.CandidateFact{{Fact: "Retryable consolidation fact"}},
		WriteMode: memory.WriteConsolidate,
	})
	if err != nil {
		t.Fatalf("Put after single ResourceExhausted: %v", err)
	}
	if len(deltas) != 1 {
		t.Fatalf("len(deltas) = %d, want 1", len(deltas))
	}

	// Inject a single Unavailable on RetrieveMemories -> retry succeeds.
	fake.mu.Lock()
	fake.failRetrieve = codes.Unavailable
	fake.failRetrieveN = 1
	fake.mu.Unlock()

	items, err := b.Retrieve(ctx, scope, memory.RetrieveRequest{TopK: 5})
	if err != nil {
		t.Fatalf("Retrieve after single Unavailable: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("len(items) = %d, want 1", len(items))
	}

	// Inject persistent ResourceExhausted -> returns *memorybank.RetryError.
	fake.mu.Lock()
	fake.failRetrieve = codes.ResourceExhausted
	fake.failRetrieveN = 5
	fake.mu.Unlock()

	_, err = b.Retrieve(ctx, scope, memory.RetrieveRequest{TopK: 5})
	var retryErr *memorybank.RetryError
	if !errors.As(err, &retryErr) {
		t.Fatalf("expected *memorybank.RetryError on persistent ResourceExhausted, got %v", err)
	}
	if retryErr.Outcome != "persisted" {
		t.Fatalf("retryErr.Outcome = %q, want %q", retryErr.Outcome, "persisted")
	}
}

func TestMemoryBank_RegisteredOpenerAndValidate(t *testing.T) {
	if !slices.Contains(memory.RegisteredSchemes(), memory.SchemeMemoryBank) {
		t.Fatalf("RegisteredSchemes() = %v, missing %q", memory.RegisteredSchemes(), memory.SchemeMemoryBank)
	}
	if err := (memorybank.Config{ReasoningEngine: "bad-resource"}).Validate(); !errors.Is(err, memory.ErrInvalidRef) {
		t.Fatalf("Config.Validate(bad-resource) = %v, want ErrInvalidRef", err)
	}
	if err := (memorybank.Config{ReasoningEngine: "proj/us-central1/123"}).Validate(); err != nil {
		t.Fatalf("Config.Validate(compact) = %v, want nil", err)
	}
}

func TestMemoryBank_Conformance_Live(t *testing.T) {
	engine := strings.TrimSpace(os.Getenv("CORE_AGENT_TEST_AGENT_ENGINE"))
	if engine == "" {
		t.Skip("CORE_AGENT_TEST_AGENT_ENGINE not set; skipping live Memory Bank conformance test")
	}
	memtest.RunConformance(t, func(t *testing.T) memory.Backend {
		b, err := memorybank.Open(context.Background(), memorybank.Config{
			ReasoningEngine: engine,
		})
		if err != nil {
			t.Fatalf("memorybank.Open(live): %v", err)
		}
		return b
	})
}
