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

// Package memtest provides the shared conformance test suite that every
// pkg/memory.Backend implementation (InMemory, Memory Bank, SQLite FTS5, and
// external adapters) must pass.
package memtest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/go-steer/core-agent/v3/pkg/memory"
)

// Factory constructs a fresh Backend instance for a conformance subtest and
// registers any cleanup via t.Cleanup.
type Factory func(t *testing.T) memory.Backend

// RunConformance executes the full Backend contract suite against newBackend.
func RunConformance(t *testing.T, newBackend Factory) {
	t.Helper()

	t.Run("ScopeValidation", func(t *testing.T) { testScopeValidation(t, newBackend) })
	t.Run("ExactScopeIsolation", func(t *testing.T) { testExactScopeIsolation(t, newBackend) })
	t.Run("ConsolidateVsVerbatimAndTopicFilter", func(t *testing.T) { testConsolidateVsVerbatim(t, newBackend) })
	t.Run("OwnershipVerifiedDelete", func(t *testing.T) { testOwnershipVerifiedDelete(t, newBackend) })
	t.Run("ConversationConsolidation", func(t *testing.T) { testConversationConsolidation(t, newBackend) })
	t.Run("ConcurrentAccess", func(t *testing.T) { testConcurrentAccess(t, newBackend) })
}

func testScopeValidation(t *testing.T, newBackend Factory) {
	t.Helper()
	b := newBackend(t)
	t.Cleanup(func() { _ = b.Close() })
	ctx := context.Background()

	invalidScopes := []struct {
		name  string
		scope memory.ScopeMap
	}{
		{"empty", memory.ScopeMap{}},
		{"wildcard_value", memory.ScopeMap{"namespace": "*"}},
		{"empty_value", memory.ScopeMap{"namespace": ""}},
		{"too_many_keys", memory.ScopeMap{
			"k1": "v1", "k2": "v2", "k3": "v3", "k4": "v4", "k5": "v5", "k6": "v6",
		}},
	}
	for _, tc := range invalidScopes {
		t.Run(tc.name, func(t *testing.T) {
			_, err := b.Put(ctx, tc.scope, memory.PutRequest{
				Facts:     []memory.CandidateFact{{Fact: "fact"}},
				WriteMode: memory.WriteVerbatim,
			})
			if !errors.Is(err, memory.ErrInvalidScope) {
				t.Fatalf("Put(%v): want ErrInvalidScope, got %v", tc.scope, err)
			}
			_, err = b.Retrieve(ctx, tc.scope, memory.RetrieveRequest{TopK: 5})
			if !errors.Is(err, memory.ErrInvalidScope) {
				t.Fatalf("Retrieve(%v): want ErrInvalidScope, got %v", tc.scope, err)
			}
			err = b.Delete(ctx, tc.scope, "some-id")
			if !errors.Is(err, memory.ErrInvalidScope) {
				t.Fatalf("Delete(%v): want ErrInvalidScope, got %v", tc.scope, err)
			}
			_, err = b.ConsolidateConversation(ctx, tc.scope, memory.ConversationRequest{
				SessionID: "sess-invalid",
				Turns:     []memory.ConversationTurn{{Role: "user", Text: "hello"}},
			})
			if !errors.Is(err, memory.ErrInvalidScope) {
				t.Fatalf("ConsolidateConversation(%v): want ErrInvalidScope, got %v", tc.scope, err)
			}
		})
	}
}

func testExactScopeIsolation(t *testing.T, newBackend Factory) {
	t.Helper()
	b := newBackend(t)
	t.Cleanup(func() { _ = b.Close() })
	ctx := context.Background()

	scopeAgentA := memory.ScopeMap{"namespace": "prod", "scope_kind": "agent", "agent_id": "db-investigator"}
	scopeAgentB := memory.ScopeMap{"namespace": "prod", "scope_kind": "agent", "agent_id": "gke-triage"}
	scopeSubset := memory.ScopeMap{"namespace": "prod", "scope_kind": "agent"}
	scopeSuperset := memory.ScopeMap{"namespace": "prod", "scope_kind": "agent", "agent_id": "db-investigator", "user_id": "alice"}
	scopeShared := memory.ScopeMap{"namespace": "prod", "scope_kind": "shared", "pool": "fleet"}

	if _, err := b.Put(ctx, scopeAgentA, memory.PutRequest{
		Facts: []memory.CandidateFact{{
			Fact:     "Cloud SQL replica lag threshold is 30 seconds",
			Topics:   []string{"database"},
			Metadata: map[string]string{"env": "prod"},
		}},
		WriteMode: memory.WriteVerbatim,
	}); err != nil {
		t.Fatalf("Put(agentA): %v", err)
	}
	if _, err := b.Put(ctx, scopeShared, memory.PutRequest{
		Facts:     []memory.CandidateFact{{Fact: "Envoy drain weight must be set to zero before pod restart", Topics: []string{"runbook"}}},
		WriteMode: memory.WriteVerbatim,
	}); err != nil {
		t.Fatalf("Put(shared): %v", err)
	}

	for name, other := range map[string]memory.ScopeMap{
		"agentB":   scopeAgentB,
		"subset":   scopeSubset,
		"superset": scopeSuperset,
	} {
		got, err := b.Retrieve(ctx, other, memory.RetrieveRequest{TopK: 10})
		if err != nil {
			t.Fatalf("Retrieve(%s): %v", name, err)
		}
		if len(got) != 0 {
			t.Fatalf("Retrieve(%s) leaked %d items from sibling scope: %+v", name, len(got), got)
		}
	}

	gotA, err := b.Retrieve(ctx, scopeAgentA, memory.RetrieveRequest{Query: "replica lag", TopK: 10})
	if err != nil {
		t.Fatalf("Retrieve(agentA): %v", err)
	}
	if len(gotA) != 1 || gotA[0].ID == "" {
		t.Fatalf("Retrieve(agentA) = %+v, want 1 item with non-empty ID", gotA)
	}

	// Mutating returned scalar AND reference fields (slices/maps) must not corrupt stored state.
	gotA[0].Fact = "MUTATED IN CALLER"
	if len(gotA[0].Topics) > 0 {
		gotA[0].Topics[0] = "MUTATED_TOPIC"
	}
	if gotA[0].Scope != nil {
		gotA[0].Scope["namespace"] = "MUTATED_NS"
	}
	if gotA[0].Metadata != nil {
		gotA[0].Metadata["env"] = "MUTATED_ENV"
	}
	if gotA[0].Distance != nil {
		*gotA[0].Distance = -999.0
	}
	gotAgain, err := b.Retrieve(ctx, scopeAgentA, memory.RetrieveRequest{TopK: 10})
	if err != nil {
		t.Fatalf("Retrieve(agentA second): %v", err)
	}
	if len(gotAgain) != 1 ||
		gotAgain[0].Fact == "MUTATED IN CALLER" ||
		(len(gotAgain[0].Topics) > 0 && gotAgain[0].Topics[0] == "MUTATED_TOPIC") ||
		gotAgain[0].Scope["namespace"] == "MUTATED_NS" ||
		(gotAgain[0].Metadata != nil && gotAgain[0].Metadata["env"] == "MUTATED_ENV") ||
		(gotAgain[0].Distance != nil && *gotAgain[0].Distance == -999.0) {
		t.Fatalf("caller mutation leaked into backend storage: %+v", gotAgain)
	}
}

func testConsolidateVsVerbatim(t *testing.T, newBackend Factory) {
	t.Helper()
	b := newBackend(t)
	t.Cleanup(func() { _ = b.Close() })
	ctx := context.Background()
	scope := memory.ScopeMap{"namespace": "prod", "scope_kind": "shared", "pool": "learnings"}

	d1, err := b.Put(ctx, scope, memory.PutRequest{
		Facts:     []memory.CandidateFact{{Fact: "Payments cluster runs in us-central1", Topics: []string{"topology"}}},
		WriteMode: memory.WriteConsolidate,
	})
	if err != nil {
		t.Fatalf("Put 1: %v", err)
	}
	if len(d1) == 0 || d1[0].ID == "" {
		t.Fatalf("Put 1 returned empty delta: %+v", d1)
	}

	// Consolidating the identical fact again must not duplicate verbatim entries.
	d2, err := b.Put(ctx, scope, memory.PutRequest{
		Facts:     []memory.CandidateFact{{Fact: "Payments cluster runs in us-central1", Topics: []string{"region"}}},
		WriteMode: memory.WriteConsolidate,
	})
	if err != nil {
		t.Fatalf("Put 2 (consolidate): %v", err)
	}
	if len(d2) == 0 {
		t.Fatalf("Put 2 returned empty delta")
	}

	items, err := b.Retrieve(ctx, scope, memory.RetrieveRequest{TopK: 10})
	if err != nil {
		t.Fatalf("Retrieve consolidated: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("after duplicate WriteConsolidate, got %d items, want 1", len(items))
	}

	// WriteVerbatim always creates an additional entry.
	if _, err := b.Put(ctx, scope, memory.PutRequest{
		Facts:     []memory.CandidateFact{{Fact: "Payments cluster runs in us-central1", Topics: []string{"verbatim-copy"}}},
		WriteMode: memory.WriteVerbatim,
	}); err != nil {
		t.Fatalf("Put 3 (verbatim): %v", err)
	}
	itemsAfterVerbatim, err := b.Retrieve(ctx, scope, memory.RetrieveRequest{TopK: 10})
	if err != nil {
		t.Fatalf("Retrieve after verbatim: %v", err)
	}
	if len(itemsAfterVerbatim) != 2 {
		t.Fatalf("after WriteVerbatim, got %d items, want 2", len(itemsAfterVerbatim))
	}

	filtered, err := b.Retrieve(ctx, scope, memory.RetrieveRequest{
		Topics: []string{"verbatim-copy", "other-unmatched-topic"},
		TopK:   10,
	})
	if err != nil {
		t.Fatalf("Retrieve filtered: %v", err)
	}
	if len(filtered) != 1 {
		t.Fatalf("topic filter returned %d items, want 1", len(filtered))
	}
}

func testOwnershipVerifiedDelete(t *testing.T, newBackend Factory) {
	t.Helper()
	b := newBackend(t)
	t.Cleanup(func() { _ = b.Close() })
	ctx := context.Background()

	scopeOwner := memory.ScopeMap{"namespace": "prod", "scope_kind": "agent", "agent_id": "owner"}
	scopeAttacker := memory.ScopeMap{"namespace": "prod", "scope_kind": "agent", "agent_id": "other"}

	deltas, err := b.Put(ctx, scopeOwner, memory.PutRequest{
		Facts:     []memory.CandidateFact{{Fact: "Private diagnostic heuristic"}},
		WriteMode: memory.WriteVerbatim,
	})
	if err != nil {
		t.Fatalf("Put(owner): %v", err)
	}
	targetID := deltas[0].ID

	// Cross-scope delete by ID MUST fail with ErrNotFound and leave owner's memory intact.
	err = b.Delete(ctx, scopeAttacker, targetID)
	if !errors.Is(err, memory.ErrNotFound) {
		t.Fatalf("cross-scope Delete: want ErrNotFound, got %v", err)
	}

	stillThere, err := b.Retrieve(ctx, scopeOwner, memory.RetrieveRequest{TopK: 10})
	if err != nil || len(stillThere) != 1 {
		t.Fatalf("owner item missing after rejected cross-scope Delete: items=%+v err=%v", stillThere, err)
	}

	// Deleting in owning scope succeeds.
	if err := b.Delete(ctx, scopeOwner, targetID); err != nil {
		t.Fatalf("owner Delete: %v", err)
	}
	afterDel, err := b.Retrieve(ctx, scopeOwner, memory.RetrieveRequest{TopK: 10})
	if err != nil {
		t.Fatalf("Retrieve after owner Delete: %v", err)
	}
	if len(afterDel) != 0 {
		t.Fatalf("expected 0 items after owner Delete, got %+v", afterDel)
	}

	// Deleting a non-existent ID returns ErrNotFound.
	if err := b.Delete(ctx, scopeOwner, targetID); !errors.Is(err, memory.ErrNotFound) {
		t.Fatalf("second Delete: want ErrNotFound, got %v", err)
	}
}

func testConversationConsolidation(t *testing.T, newBackend Factory) {
	t.Helper()
	b := newBackend(t)
	t.Cleanup(func() { _ = b.Close() })
	ctx := context.Background()

	scope := memory.ScopeMap{"namespace": "prod", "scope_kind": "agent", "agent_id": "triage"}
	sibling := memory.ScopeMap{"namespace": "prod", "scope_kind": "agent", "agent_id": "other"}

	deltas, err := b.ConsolidateConversation(ctx, scope, memory.ConversationRequest{
		SessionID: "sess-1",
		Turns: []memory.ConversationTurn{
			{Role: "user", Text: "Always check pod disruption budgets before draining nodes."},
			{Role: "model", Text: "Acknowledged: I will inspect PodDisruptionBudgets first."},
		},
	})
	if err != nil {
		t.Fatalf("ConsolidateConversation: %v", err)
	}
	if len(deltas) == 0 {
		t.Fatalf("ConsolidateConversation returned no deltas")
	}

	got, err := b.Retrieve(ctx, scope, memory.RetrieveRequest{Query: "disruption", TopK: 10})
	if err != nil {
		t.Fatalf("Retrieve consolidated conversation: %v", err)
	}
	if len(got) == 0 {
		t.Fatalf("expected consolidated conversation memory in target scope")
	}
	leaked, err := b.Retrieve(ctx, sibling, memory.RetrieveRequest{TopK: 10})
	if err != nil || len(leaked) != 0 {
		t.Fatalf("ConsolidateConversation leaked into sibling scope: %+v (err=%v)", leaked, err)
	}
}

func testConcurrentAccess(t *testing.T, newBackend Factory) {
	t.Helper()
	b := newBackend(t)
	t.Cleanup(func() { _ = b.Close() })
	ctx := context.Background()

	const workers = 8
	var wg sync.WaitGroup
	errCh := make(chan error, workers*4)

	for w := range workers {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			scope := memory.ScopeMap{
				"namespace":  "concurrent",
				"scope_kind": "agent",
				"agent_id":   fmt.Sprintf("worker-%d", id%3),
			}
			deltas, err := b.Put(ctx, scope, memory.PutRequest{
				Facts: []memory.CandidateFact{{
					Fact:   fmt.Sprintf("Worker %d observed spike on shard %d", id, id),
					Topics: []string{"load"},
				}},
				WriteMode: memory.WriteVerbatim,
			})
			if err != nil {
				errCh <- err
				return
			}
			if _, err := b.Retrieve(ctx, scope, memory.RetrieveRequest{Query: "shard", TopK: 5}); err != nil {
				errCh <- err
				return
			}
			if id%2 == 0 && len(deltas) > 0 {
				if err := b.Delete(ctx, scope, deltas[0].ID); err != nil && !errors.Is(err, memory.ErrNotFound) {
					errCh <- err
				}
			}
		}(w)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Errorf("concurrent operation failed: %v", err)
	}
}
