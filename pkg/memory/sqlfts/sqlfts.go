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

// Package sqlfts implements pkg/memory.Backend on top of pure-Go SQLite with
// FTS5 full-text search (github.com/glebarez/sqlite).
//
// Importing this package registers the "sqlite" URI scheme with
// pkg/memory.RegisterOpener so memory.Open(ctx, "sqlite:///path/to/memory.db")
// and memory.OpenRouter work automatically.
package sqlfts

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	// Register the pure-Go "sqlite" database/sql driver.
	_ "github.com/glebarez/sqlite"

	"github.com/go-steer/core-agent/v3/pkg/memory"
)

// ErrClosed is returned when an operation is called on a closed Backend.
var ErrClosed = errors.New("sqlfts: backend is closed")

func init() {
	memory.RegisterOpener(memory.SchemeSQLite, OpenRef)
}

// Option configures a SQLite FTS5 Backend.
type Option func(*Backend)

// WithClock overrides time.Now for deterministic timestamp and TTL testing.
func WithClock(now func() time.Time) Option {
	return func(b *Backend) {
		if now != nil {
			b.now = now
		}
	}
}

// Backend is a SQLite FTS5 implementation of pkg/memory.Backend partitioned by
// exact ScopeMap equality.
type Backend struct {
	mu     sync.Mutex
	db     *sql.DB
	hasFTS bool
	now    func() time.Time
	closed bool
}

var _ memory.Backend = (*Backend)(nil)

// Open opens or creates a SQLite database at dbPath, initializes the memory
// schema and FTS5 index, and returns a Backend.
func Open(ctx context.Context, dbPath string, opts ...Option) (*Backend, error) {
	trimmed := strings.TrimSpace(dbPath)
	if trimmed == "" {
		return nil, fmt.Errorf("sqlfts: %w: empty database path", memory.ErrInvalidRef)
	}
	if trimmed != ":memory:" && !strings.HasPrefix(trimmed, "file:") {
		dir := filepath.Dir(trimmed)
		if dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o750); err != nil {
				return nil, fmt.Errorf("sqlfts: create parent dir: %w", err)
			}
		}
	}

	dsn := trimmed
	if !strings.Contains(dsn, "?") && dsn != ":memory:" {
		dsn += "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("sqlfts: open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)

	b := &Backend{
		db:  db,
		now: time.Now,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(b)
		}
	}
	if err := b.initSchema(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return b, nil
}

// OpenRef implements memory.Opener for "sqlite://" URIs.
func OpenRef(ctx context.Context, ref memory.ParsedRef) (memory.Backend, error) {
	if ref.Scheme != memory.SchemeSQLite {
		return nil, fmt.Errorf("sqlfts: unexpected scheme %q", ref.Scheme)
	}
	return Open(ctx, ref.Resource)
}

func (b *Backend) initSchema(ctx context.Context) error {
	const ddl = `
CREATE TABLE IF NOT EXISTS memory_seq (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	val INTEGER NOT NULL
);
INSERT OR IGNORE INTO memory_seq (id, val) VALUES (1, 0);

CREATE TABLE IF NOT EXISTS memory_items (
	id            TEXT PRIMARY KEY,
	scope_key     TEXT NOT NULL,
	norm_fact     TEXT NOT NULL,
	fact          TEXT NOT NULL,
	scope_json    TEXT NOT NULL,
	topics_json   TEXT NOT NULL,
	metadata_json TEXT NOT NULL,
	created_at_ns INTEGER NOT NULL,
	updated_at_ns INTEGER NOT NULL,
	expire_at_ns  INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_memory_items_scope ON memory_items(scope_key);
CREATE INDEX IF NOT EXISTS idx_memory_items_scope_norm ON memory_items(scope_key, norm_fact);
`
	if _, err := b.db.ExecContext(ctx, ddl); err != nil {
		return fmt.Errorf("sqlfts: init schema: %w", err)
	}
	const ftsDDL = `
CREATE VIRTUAL TABLE IF NOT EXISTS memory_items_fts USING fts5(
	item_id UNINDEXED,
	scope_key UNINDEXED,
	fact,
	topics
);
`
	if _, err := b.db.ExecContext(ctx, ftsDDL); err == nil {
		b.hasFTS = true
	}
	return nil
}

// Retrieve searches or lists non-expired items in scope.
func (b *Backend) Retrieve(ctx context.Context, scope memory.ScopeMap, req memory.RetrieveRequest) ([]memory.Item, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	topK := req.TopK
	if topK <= 0 {
		topK = memory.DefaultTopK
	}
	topK = min(topK, memory.MaxTopK)

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil, ErrClosed
	}

	now := b.now()
	scopeKey := scope.CanonicalKey()
	if err := b.pruneExpiredLocked(ctx, scopeKey, now); err != nil {
		return nil, err
	}

	query := strings.TrimSpace(req.Query)
	ftsRanks := b.queryFTSRanksLocked(ctx, scopeKey, query)

	rows, err := b.db.QueryContext(ctx, `
SELECT id, fact, scope_json, topics_json, metadata_json, created_at_ns, updated_at_ns, expire_at_ns
FROM memory_items
WHERE scope_key = ?
`, scopeKey)
	if err != nil {
		return nil, fmt.Errorf("sqlfts: query items: %w", err)
	}
	defer func() { _ = rows.Close() }()

	queryTokens := tokenize(query)
	var candidates []memory.Item
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		if !matchesTopicsFilter(it.Topics, req.Topics) {
			continue
		}
		if query != "" {
			dist, matched := computeDistance(query, queryTokens, it, ftsRanks)
			if !matched {
				continue
			}
			it.Distance = &dist
		}
		candidates = append(candidates, it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlfts: iterate items: %w", err)
	}

	slices.SortFunc(candidates, func(a, b memory.Item) int {
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

// Put writes or consolidates candidate facts into scope.
func (b *Backend) Put(ctx context.Context, scope memory.ScopeMap, req memory.PutRequest) ([]memory.ConsolidationDelta, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if len(req.Facts) == 0 {
		return nil, fmt.Errorf("%w: empty facts slice", memory.ErrEmptyFact)
	}
	writeMode := req.WriteMode
	if writeMode == "" {
		writeMode = memory.WriteConsolidate
	}
	if !writeMode.Valid() {
		return nil, fmt.Errorf("sqlfts: invalid write mode %q", writeMode)
	}
	for i, f := range req.Facts {
		if strings.TrimSpace(f.Fact) == "" {
			return nil, fmt.Errorf("%w: facts[%d]", memory.ErrEmptyFact, i)
		}
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil, ErrClosed
	}

	now := b.now()
	scopeKey := scope.CanonicalKey()
	if err := b.pruneExpiredLocked(ctx, scopeKey, now); err != nil {
		return nil, err
	}

	scopeJSON, err := json.Marshal(scope)
	if err != nil {
		return nil, fmt.Errorf("sqlfts: marshal scope: %w", err)
	}

	deltas := make([]memory.ConsolidationDelta, 0, len(req.Facts))
	for _, f := range req.Facts {
		delta, err := b.putOneFactLocked(ctx, scopeKey, string(scopeJSON), f, writeMode, now)
		if err != nil {
			return nil, err
		}
		deltas = append(deltas, delta)
	}
	return deltas, nil
}

func (b *Backend) putOneFactLocked(
	ctx context.Context,
	scopeKey string,
	scopeJSON string,
	f memory.CandidateFact,
	writeMode memory.WriteMode,
	now time.Time,
) (memory.ConsolidationDelta, error) {
	factText := strings.TrimSpace(f.Fact)
	norm := normalizeFact(factText)
	topics := cleanTopics(f.Topics)
	var expireNs int64
	if f.TTL > 0 {
		expireNs = now.Add(f.TTL).UnixNano()
	}

	if writeMode == memory.WriteConsolidate {
		existingID, existingTopics, existingMeta, found, err := b.findExistingLocked(ctx, scopeKey, norm)
		if err != nil {
			return memory.ConsolidationDelta{}, err
		}
		if found {
			mergedTopics := mergeTopics(existingTopics, topics)
			mergedMeta := maps.Clone(existingMeta)
			if len(f.Metadata) > 0 {
				if mergedMeta == nil {
					mergedMeta = make(map[string]string, len(f.Metadata))
				}
				maps.Copy(mergedMeta, f.Metadata)
			}
			topicsJSON, _ := json.Marshal(mergedTopics)
			metaJSON, _ := json.Marshal(mergedMeta)
			if expireNs > 0 {
				_, err = b.db.ExecContext(ctx, `
UPDATE memory_items
SET fact = ?, topics_json = ?, metadata_json = ?, updated_at_ns = ?, expire_at_ns = ?
WHERE id = ?
`, factText, string(topicsJSON), string(metaJSON), now.UnixNano(), expireNs, existingID)
			} else {
				_, err = b.db.ExecContext(ctx, `
UPDATE memory_items
SET fact = ?, topics_json = ?, metadata_json = ?, updated_at_ns = ?
WHERE id = ?
`, factText, string(topicsJSON), string(metaJSON), now.UnixNano(), existingID)
			}
			if err != nil {
				return memory.ConsolidationDelta{}, fmt.Errorf("sqlfts: update consolidated item: %w", err)
			}
			if err := b.upsertFTSLocked(ctx, existingID, scopeKey, factText, mergedTopics); err != nil {
				return memory.ConsolidationDelta{}, err
			}
			return memory.ConsolidationDelta{
				ID:     existingID,
				Action: memory.ActionUpdated,
				Fact:   factText,
			}, nil
		}
	}

	id, err := b.nextIDLocked(ctx)
	if err != nil {
		return memory.ConsolidationDelta{}, err
	}
	topicsJSON, _ := json.Marshal(topics)
	metaJSON, _ := json.Marshal(f.Metadata)
	nowNs := now.UnixNano()
	if _, err := b.db.ExecContext(ctx, `
INSERT INTO memory_items (id, scope_key, norm_fact, fact, scope_json, topics_json, metadata_json, created_at_ns, updated_at_ns, expire_at_ns)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
`, id, scopeKey, norm, factText, scopeJSON, string(topicsJSON), string(metaJSON), nowNs, nowNs, expireNs); err != nil {
		return memory.ConsolidationDelta{}, fmt.Errorf("sqlfts: insert item: %w", err)
	}
	if err := b.upsertFTSLocked(ctx, id, scopeKey, factText, topics); err != nil {
		return memory.ConsolidationDelta{}, err
	}
	return memory.ConsolidationDelta{
		ID:     id,
		Action: memory.ActionCreated,
		Fact:   factText,
	}, nil
}

// Delete removes id only if it exists in scope. Cross-scope delete attempts
// return an error wrapping both memory.ErrScopeMismatch and memory.ErrNotFound
// without modifying the target item.
func (b *Backend) Delete(ctx context.Context, scope memory.ScopeMap, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := scope.Validate(); err != nil {
		return err
	}
	trimmed := strings.TrimSpace(id)
	if trimmed == "" {
		return memory.ErrEmptyID
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return ErrClosed
	}

	nowNs := b.now().UnixNano()
	var gotScopeKey string
	var expireNs int64
	err := b.db.QueryRowContext(ctx, `SELECT scope_key, expire_at_ns FROM memory_items WHERE id = ?`, trimmed).Scan(&gotScopeKey, &expireNs)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %q", memory.ErrNotFound, trimmed)
	}
	if err != nil {
		return fmt.Errorf("sqlfts: lookup item for delete: %w", err)
	}
	if expireNs > 0 && expireNs <= nowNs {
		_ = b.deleteByIDLocked(ctx, trimmed)
		return fmt.Errorf("%w: %q", memory.ErrNotFound, trimmed)
	}
	if gotScopeKey != scope.CanonicalKey() {
		return fmt.Errorf("%w (%w): %q", memory.ErrScopeMismatch, memory.ErrNotFound, trimmed)
	}
	return b.deleteByIDLocked(ctx, trimmed)
}

// ConsolidateConversation extracts non-empty dialogue turns from req.Turns and
// consolidates them into scope.
func (b *Backend) ConsolidateConversation(ctx context.Context, scope memory.ScopeMap, req memory.ConversationRequest) ([]memory.ConsolidationDelta, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	var facts []memory.CandidateFact
	for _, turn := range req.Turns {
		txt := strings.TrimSpace(turn.Text)
		if txt == "" {
			continue
		}
		role := strings.TrimSpace(turn.Role)
		if role == "" {
			role = "turn"
		}
		facts = append(facts, memory.CandidateFact{
			Fact:   fmt.Sprintf("[%s] %s", role, txt),
			Topics: []string{"conversation"},
		})
	}
	if len(facts) == 0 {
		return nil, nil
	}
	return b.Put(ctx, scope, memory.PutRequest{
		Facts:     facts,
		WriteMode: memory.WriteConsolidate,
	})
}

// Close closes the underlying SQLite database connection.
func (b *Backend) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil
	}
	b.closed = true
	return b.db.Close()
}

func (b *Backend) nextIDLocked(ctx context.Context) (string, error) {
	var seq int64
	if err := b.db.QueryRowContext(ctx, `UPDATE memory_seq SET val = val + 1 WHERE id = 1 RETURNING val`).Scan(&seq); err != nil {
		return "", fmt.Errorf("sqlfts: increment seq: %w", err)
	}
	return fmt.Sprintf("mem-%d", seq), nil
}

func (b *Backend) findExistingLocked(ctx context.Context, scopeKey, norm string) (string, []string, map[string]string, bool, error) {
	var id, topicsRaw, metaRaw string
	err := b.db.QueryRowContext(ctx, `
SELECT id, topics_json, metadata_json
FROM memory_items
WHERE scope_key = ? AND norm_fact = ?
LIMIT 1
`, scopeKey, norm).Scan(&id, &topicsRaw, &metaRaw)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil, nil, false, nil
	}
	if err != nil {
		return "", nil, nil, false, fmt.Errorf("sqlfts: lookup existing consolidated item: %w", err)
	}
	var topics []string
	_ = json.Unmarshal([]byte(topicsRaw), &topics)
	var meta map[string]string
	_ = json.Unmarshal([]byte(metaRaw), &meta)
	return id, topics, meta, true, nil
}

func (b *Backend) pruneExpiredLocked(ctx context.Context, scopeKey string, now time.Time) error {
	nowNs := now.UnixNano()
	if b.hasFTS {
		_, _ = b.db.ExecContext(ctx, `
DELETE FROM memory_items_fts
WHERE item_id IN (SELECT id FROM memory_items WHERE scope_key = ? AND expire_at_ns > 0 AND expire_at_ns <= ?)
`, scopeKey, nowNs)
	}
	if _, err := b.db.ExecContext(ctx, `
DELETE FROM memory_items WHERE scope_key = ? AND expire_at_ns > 0 AND expire_at_ns <= ?
`, scopeKey, nowNs); err != nil {
		return fmt.Errorf("sqlfts: prune expired: %w", err)
	}
	return nil
}

func (b *Backend) upsertFTSLocked(ctx context.Context, id, scopeKey, fact string, topics []string) error {
	if !b.hasFTS {
		return nil
	}
	if _, err := b.db.ExecContext(ctx, `DELETE FROM memory_items_fts WHERE item_id = ?`, id); err != nil {
		return fmt.Errorf("sqlfts: delete prior fts row: %w", err)
	}
	if _, err := b.db.ExecContext(ctx, `
INSERT INTO memory_items_fts (item_id, scope_key, fact, topics) VALUES (?, ?, ?, ?)
`, id, scopeKey, fact, strings.Join(topics, " ")); err != nil {
		return fmt.Errorf("sqlfts: insert fts row: %w", err)
	}
	return nil
}

func (b *Backend) deleteByIDLocked(ctx context.Context, id string) error {
	if b.hasFTS {
		if _, err := b.db.ExecContext(ctx, `DELETE FROM memory_items_fts WHERE item_id = ?`, id); err != nil {
			return fmt.Errorf("sqlfts: delete fts row: %w", err)
		}
	}
	if _, err := b.db.ExecContext(ctx, `DELETE FROM memory_items WHERE id = ?`, id); err != nil {
		return fmt.Errorf("sqlfts: delete item row: %w", err)
	}
	return nil
}

func (b *Backend) queryFTSRanksLocked(ctx context.Context, scopeKey, query string) map[string]float64 {
	if !b.hasFTS || query == "" {
		return nil
	}
	tokens := tokenize(query)
	if len(tokens) == 0 {
		return nil
	}
	quoted := make([]string, 0, len(tokens))
	for _, tok := range tokens {
		quoted = append(quoted, `"`+strings.ReplaceAll(tok, `"`, `""`)+`"`)
	}
	matchExpr := strings.Join(quoted, " OR ")
	rows, err := b.db.QueryContext(ctx, `
SELECT item_id, bm25(memory_items_fts)
FROM memory_items_fts
WHERE scope_key = ? AND memory_items_fts MATCH ?
`, scopeKey, matchExpr)
	if err != nil {
		return nil
	}
	defer func() { _ = rows.Close() }()
	ranks := make(map[string]float64)
	for rows.Next() {
		var id string
		var score float64
		if err := rows.Scan(&id, &score); err == nil {
			ranks[id] = score
		}
	}
	return ranks
}

func scanItem(rows *sql.Rows) (memory.Item, error) {
	var (
		id, fact, scopeRaw, topicsRaw, metaRaw string
		createdNs, updatedNs, expireNs         int64
	)
	if err := rows.Scan(&id, &fact, &scopeRaw, &topicsRaw, &metaRaw, &createdNs, &updatedNs, &expireNs); err != nil {
		return memory.Item{}, fmt.Errorf("sqlfts: scan item: %w", err)
	}
	var scope memory.ScopeMap
	_ = json.Unmarshal([]byte(scopeRaw), &scope)
	var topics []string
	_ = json.Unmarshal([]byte(topicsRaw), &topics)
	var meta map[string]string
	_ = json.Unmarshal([]byte(metaRaw), &meta)
	if len(meta) == 0 {
		meta = nil
	}
	it := memory.Item{
		ID:        id,
		Fact:      fact,
		Topics:    topics,
		Scope:     scope,
		CreatedAt: time.Unix(0, createdNs).UTC(),
		UpdatedAt: time.Unix(0, updatedNs).UTC(),
		Metadata:  meta,
	}
	if expireNs > 0 {
		it.ExpireAt = time.Unix(0, expireNs).UTC()
	}
	return it, nil
}

func computeDistance(query string, queryTokens []string, it memory.Item, ftsRanks map[string]float64) (float64, bool) {
	lowerQuery := strings.ToLower(strings.TrimSpace(query))
	lowerFact := strings.ToLower(it.Fact)
	if strings.Contains(lowerFact, lowerQuery) {
		return 0.05, true
	}
	if bm25Score, ok := ftsRanks[it.ID]; ok {
		// SQLite FTS5 bm25() returns negative numbers where more negative means better match.
		dist := 1.0 / (1.0 + max(0.0, -bm25Score))
		return dist, true
	}
	if len(queryTokens) == 0 {
		return 1.0, false
	}
	corpusTokens := tokenize(it.Fact + " " + strings.Join(it.Topics, " "))
	set := make(map[string]bool, len(corpusTokens))
	for _, ct := range corpusTokens {
		set[ct] = true
	}
	hits := 0
	for _, qt := range queryTokens {
		if set[qt] {
			hits++
		}
	}
	if hits == 0 {
		return 1.0, false
	}
	return 1.0 - 0.8*(float64(hits)/float64(len(queryTokens))), true
}

func normalizeFact(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

func tokenize(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
}

func cleanTopics(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, raw := range in {
		t := strings.TrimSpace(raw)
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func mergeTopics(existing, added []string) []string {
	return cleanTopics(append(slices.Clone(existing), added...))
}

func matchesTopicsFilter(itemTopics, filter []string) bool {
	cleaned := cleanTopics(filter)
	if len(cleaned) == 0 {
		return true
	}
	if len(itemTopics) == 0 {
		return false
	}
	for _, want := range cleaned {
		for _, got := range itemTopics {
			if strings.EqualFold(strings.TrimSpace(got), want) {
				return true
			}
		}
	}
	return false
}
