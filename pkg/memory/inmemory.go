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

package memory

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
)

// InMemoryOption configures an InMemory Backend.
type InMemoryOption func(*InMemory)

// WithClock overrides the clock used for timestamps and TTL expiration in
// deterministic unit tests.
func WithClock(now func() time.Time) InMemoryOption {
	return func(m *InMemory) {
		if now != nil {
			m.now = now
		}
	}
}

type storedEntry struct {
	item     Item
	scopeKey string
	normFact string
}

// InMemory is a thread-safe, hermetic Backend implementation partitioned by
// exact ScopeMap equality. It requires no external services or drivers and
// enforces the same scope-isolation and ownership-checked Delete semantics as
// Vertex AI Memory Bank.
type InMemory struct {
	mu      sync.RWMutex
	now     func() time.Time
	nextSeq uint64
	byID    map[string]*storedEntry
	byScope map[string][]string // scope.CanonicalKey() -> ordered item IDs
}

// NewInMemory constructs an empty InMemory Backend.
func NewInMemory(opts ...InMemoryOption) *InMemory {
	m := &InMemory{
		now:     time.Now,
		byID:    make(map[string]*storedEntry),
		byScope: make(map[string][]string),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(m)
		}
	}
	return m
}

// Retrieve searches or lists non-expired items matching scope by exact
// dictionary equality.
func (m *InMemory) Retrieve(ctx context.Context, scope ScopeMap, req RetrieveRequest) ([]Item, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	topK := req.TopK
	if topK <= 0 {
		topK = DefaultTopK
	}
	topK = min(topK, MaxTopK)
	now := m.now()
	key := scope.CanonicalKey()
	query := strings.TrimSpace(req.Query)
	queryTokens := tokenize(query)

	m.mu.RLock()
	ids := slices.Clone(m.byScope[key])
	var candidates []Item
	for _, id := range ids {
		se, ok := m.byID[id]
		if !ok {
			continue
		}
		if isExpired(se.item, now) {
			continue
		}
		if !matchesTopicsFilter(se.item.Topics, req.Topics) {
			continue
		}
		cp := se.item.Clone()
		if query != "" {
			dist, matched := computeTokenDistance(query, queryTokens, cp.Fact, cp.Topics)
			if !matched {
				continue
			}
			cp.Distance = &dist
		}
		candidates = append(candidates, cp)
	}
	m.mu.RUnlock()

	slices.SortFunc(candidates, func(a, b Item) int {
		if a.Distance != nil && b.Distance != nil {
			if c := cmp.Compare(*a.Distance, *b.Distance); c != 0 {
				return c
			}
		}
		if !a.UpdatedAt.Equal(b.UpdatedAt) {
			if a.UpdatedAt.After(b.UpdatedAt) {
				return -1
			}
			return 1
		}
		return cmp.Compare(b.ID, a.ID)
	})

	if len(candidates) > topK {
		candidates = candidates[:topK]
	}
	return candidates, nil
}

// Put writes or consolidates facts into scope.
func (m *InMemory) Put(ctx context.Context, scope ScopeMap, req PutRequest) ([]ConsolidationDelta, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if len(req.Facts) == 0 {
		return nil, fmt.Errorf("%w: empty facts slice", ErrEmptyFact)
	}
	writeMode := req.WriteMode
	if writeMode == "" {
		writeMode = WriteConsolidate
	}
	if !writeMode.Valid() {
		return nil, fmt.Errorf("memory: invalid write mode %q", writeMode)
	}

	for i, f := range req.Facts {
		if strings.TrimSpace(f.Fact) == "" {
			return nil, fmt.Errorf("%w: facts[%d]", ErrEmptyFact, i)
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.now()
	key := scope.CanonicalKey()
	m.pruneExpiredLocked(key, now)

	deltas := make([]ConsolidationDelta, 0, len(req.Facts))
	for _, f := range req.Facts {
		factText := strings.TrimSpace(f.Fact)
		norm := normalizeFact(factText)
		topics := cleanTopics(f.Topics)
		var expireAt time.Time
		if f.TTL > 0 {
			expireAt = now.Add(f.TTL)
		}

		if writeMode == WriteConsolidate {
			if existing := m.findConsolidatedMatchLocked(key, norm, now); existing != nil {
				existing.item.Fact = factText
				existing.item.Topics = mergeTopics(existing.item.Topics, topics)
				if len(f.Metadata) > 0 {
					if existing.item.Metadata == nil {
						existing.item.Metadata = make(map[string]string, len(f.Metadata))
					}
					maps.Copy(existing.item.Metadata, f.Metadata)
				}
				existing.item.UpdatedAt = now
				if !expireAt.IsZero() {
					existing.item.ExpireAt = expireAt
				}
				deltas = append(deltas, ConsolidationDelta{
					ID:     existing.item.ID,
					Action: ActionUpdated,
					Fact:   factText,
				})
				continue
			}
		}

		m.nextSeq++
		id := fmt.Sprintf("mem-%d", m.nextSeq)
		it := Item{
			ID:        id,
			Fact:      factText,
			Topics:    topics,
			Scope:     scope.Clone(),
			CreatedAt: now,
			UpdatedAt: now,
			ExpireAt:  expireAt,
			Metadata:  maps.Clone(f.Metadata),
		}
		m.byID[id] = &storedEntry{
			item:     it,
			scopeKey: key,
			normFact: norm,
		}
		m.byScope[key] = append(m.byScope[key], id)
		deltas = append(deltas, ConsolidationDelta{
			ID:     id,
			Action: ActionCreated,
			Fact:   factText,
		})
	}
	return deltas, nil
}

// Delete removes id only if it exists in scope. If id belongs to a different
// scope, Delete returns an error wrapping both ErrScopeMismatch and
// ErrNotFound without modifying the other scope's entry.
func (m *InMemory) Delete(ctx context.Context, scope ScopeMap, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := scope.Validate(); err != nil {
		return err
	}
	trimmed := strings.TrimSpace(id)
	if trimmed == "" {
		return ErrEmptyID
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.now()
	se, ok := m.byID[trimmed]
	if !ok || isExpired(se.item, now) {
		if ok {
			m.removeIDLocked(se.scopeKey, trimmed)
		}
		return fmt.Errorf("%w: %q", ErrNotFound, trimmed)
	}
	key := scope.CanonicalKey()
	if se.scopeKey != key {
		return fmt.Errorf("%w (%w): %q", ErrScopeMismatch, ErrNotFound, trimmed)
	}
	m.removeIDLocked(key, trimmed)
	return nil
}

// ConsolidateConversation extracts non-empty dialogue turns from req.Turns and
// consolidates them into scope so turn_end / session_end consolidation can be
// tested hermetically without external LLM services.
func (m *InMemory) ConsolidateConversation(ctx context.Context, scope ScopeMap, req ConversationRequest) ([]ConsolidationDelta, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	var facts []CandidateFact
	for _, turn := range req.Turns {
		txt := strings.TrimSpace(turn.Text)
		if txt == "" {
			continue
		}
		role := strings.TrimSpace(turn.Role)
		if role == "" {
			role = "turn"
		}
		facts = append(facts, CandidateFact{
			Fact:   fmt.Sprintf("[%s] %s", role, txt),
			Topics: []string{"conversation"},
		})
	}
	if len(facts) == 0 {
		return nil, nil
	}
	return m.Put(ctx, scope, PutRequest{
		Facts:     facts,
		WriteMode: WriteConsolidate,
	})
}

// Close implements Backend.
func (m *InMemory) Close() error {
	return nil
}

func (m *InMemory) findConsolidatedMatchLocked(scopeKey, normFact string, now time.Time) *storedEntry {
	for _, id := range m.byScope[scopeKey] {
		se, ok := m.byID[id]
		if !ok || isExpired(se.item, now) {
			continue
		}
		if se.normFact == normFact {
			return se
		}
	}
	return nil
}

func (m *InMemory) pruneExpiredLocked(scopeKey string, now time.Time) {
	ids := m.byScope[scopeKey]
	if len(ids) == 0 {
		return
	}
	kept := ids[:0]
	for _, id := range ids {
		se, ok := m.byID[id]
		if !ok {
			continue
		}
		if isExpired(se.item, now) {
			delete(m.byID, id)
			continue
		}
		kept = append(kept, id)
	}
	if len(kept) == 0 {
		delete(m.byScope, scopeKey)
	} else {
		m.byScope[scopeKey] = kept
	}
}

func (m *InMemory) removeIDLocked(scopeKey, id string) {
	delete(m.byID, id)
	ids := m.byScope[scopeKey]
	idx := slices.Index(ids, id)
	if idx >= 0 {
		ids = slices.Delete(ids, idx, idx+1)
	}
	if len(ids) == 0 {
		delete(m.byScope, scopeKey)
	} else {
		m.byScope[scopeKey] = ids
	}
}

func isExpired(it Item, now time.Time) bool {
	return !it.ExpireAt.IsZero() && !now.Before(it.ExpireAt)
}

func normalizeFact(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}

func mergeTopics(existing, added []string) []string {
	if len(added) == 0 {
		return existing
	}
	return cleanTopics(append(slices.Clone(existing), added...))
}

func tokenize(s string) []string {
	lower := strings.ToLower(s)
	fields := strings.FieldsFunc(lower, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	if len(fields) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(fields))
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	return out
}

func computeTokenDistance(rawQuery string, queryTokens []string, fact string, topics []string) (float64, bool) {
	corpus := strings.ToLower(fact + " " + strings.Join(topics, " "))
	lowerQuery := strings.ToLower(rawQuery)
	if len(queryTokens) == 0 {
		if strings.Contains(corpus, lowerQuery) {
			return 0.0, true
		}
		return 1.0, false
	}
	corpusTokens := tokenize(corpus)
	corpusSet := make(map[string]bool, len(corpusTokens))
	for _, t := range corpusTokens {
		corpusSet[t] = true
	}
	hits := 0
	for _, qt := range queryTokens {
		if corpusSet[qt] || strings.Contains(corpus, qt) {
			hits++
		}
	}
	if hits == 0 {
		return 1.0, false
	}
	score := float64(hits) / float64(len(queryTokens))
	if strings.Contains(corpus, lowerQuery) {
		score = min(1.0, score+0.15)
	}
	return 1.0 - score, true
}
