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

package sqlfts_test

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/go-steer/core-agent/v3/pkg/memory"
	"github.com/go-steer/core-agent/v3/pkg/memory/memtest"
	"github.com/go-steer/core-agent/v3/pkg/memory/sqlfts"
)

func TestSQLFTS_Conformance(t *testing.T) {
	memtest.RunConformance(t, func(t *testing.T) memory.Backend {
		dbPath := filepath.Join(t.TempDir(), "memory.db")
		b, err := sqlfts.Open(context.Background(), dbPath)
		if err != nil {
			t.Fatalf("sqlfts.Open: %v", err)
		}
		return b
	})
}

func TestSQLFTS_PersistenceAcrossReopenAndTTL(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "nested", "state", "memory.db")
	uri := "sqlite://" + dbPath

	if !slices.Contains(memory.RegisteredSchemes(), memory.SchemeSQLite) {
		t.Fatalf("RegisteredSchemes() = %v, missing %q", memory.RegisteredSchemes(), memory.SchemeSQLite)
	}

	now := time.Now().UTC()
	b1, err := sqlfts.Open(ctx, dbPath, sqlfts.WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatalf("sqlfts.Open(1): %v", err)
	}

	scope := memory.ScopeMap{
		"namespace":  "local-dev",
		"scope_kind": "shared",
		"pool":       "team-notes",
	}
	if _, err := b1.Put(ctx, scope, memory.PutRequest{
		Facts: []memory.CandidateFact{
			{Fact: "Durable fact without TTL", Topics: []string{"permanent"}},
			{Fact: "Ephemeral fact with 10m TTL", Topics: []string{"temp"}, TTL: 10 * time.Minute},
		},
		WriteMode: memory.WriteVerbatim,
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := b1.Close(); err != nil {
		t.Fatalf("b1.Close: %v", err)
	}

	// Reopen via memory.Open(ctx, "sqlite://...") and verify both items exist before TTL expiry.
	b2, err := memory.Open(ctx, uri)
	if err != nil {
		t.Fatalf("memory.Open(%q): %v", uri, err)
	}
	itemsBefore, err := b2.Retrieve(ctx, scope, memory.RetrieveRequest{TopK: 10})
	if err != nil {
		t.Fatalf("Retrieve before TTL: %v", err)
	}
	if len(itemsBefore) != 2 {
		t.Fatalf("len(itemsBefore) = %d, want 2", len(itemsBefore))
	}
	_ = b2.Close()

	// Reopen with clock advanced past the 10m TTL -> ephemeral item is pruned.
	b3, err := sqlfts.Open(ctx, dbPath, sqlfts.WithClock(func() time.Time { return now.Add(15 * time.Minute) }))
	if err != nil {
		t.Fatalf("sqlfts.Open(3): %v", err)
	}
	defer func() { _ = b3.Close() }()

	itemsAfter, err := b3.Retrieve(ctx, scope, memory.RetrieveRequest{TopK: 10})
	if err != nil {
		t.Fatalf("Retrieve after TTL: %v", err)
	}
	if len(itemsAfter) != 1 || itemsAfter[0].Fact != "Durable fact without TTL" {
		t.Fatalf("itemsAfter = %+v, want only durable fact", itemsAfter)
	}
}
