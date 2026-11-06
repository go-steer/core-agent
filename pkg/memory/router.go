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
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"google.golang.org/adk/v2/session"
)

// RouterOption configures optional behavior on a Router.
type RouterOption func(*Router)

// WithDefaultTopK overrides the default TopK used when Recall receives topK <= 0.
func WithDefaultTopK(k int) RouterOption {
	return func(r *Router) {
		if k > 0 {
			r.defaultTopK = min(k, MaxTopK)
		}
	}
}

// WithStrictRecall causes multi-mount Recall to fail closed if any targeted
// mount returns an error, instead of returning items from healthy mounts plus
// MountWarning diagnostics.
func WithStrictRecall(strict bool) RouterOption {
	return func(r *Router) {
		r.strictRecall = strict
	}
}

// WithAuditRecorder attaches a callback invoked after every successful
// Remember, Forget, or ConsolidateSession mutation.
func WithAuditRecorder(rec AuditRecorder) RouterOption {
	return func(r *Router) {
		r.audit = rec
	}
}

// AppendAuditRecorder chains rec after any audit recorder already set,
// so a host's own recorder keeps running when pkg/agent adds the
// session-transcript recorder. Both run; their errors are joined. A nil
// rec is ignored.
func AppendAuditRecorder(rec AuditRecorder) RouterOption {
	return func(r *Router) {
		if rec == nil {
			return
		}
		prev := r.audit
		if prev == nil {
			r.audit = rec
			return
		}
		r.audit = func(ctx context.Context, author string, payload map[string]any) error {
			return errors.Join(prev(ctx, author, payload), rec(ctx, author, payload))
		}
	}
}

// With returns a copy of r with opts applied on top of r's existing
// configuration. r itself is unchanged, preserving Router's
// immutability; the copy shares r's Backend.
func (r *Router) With(opts ...RouterOption) *Router {
	if r == nil {
		return nil
	}
	cp := *r
	cp.sharedCeilings = make(map[string]sharedCeiling, len(r.sharedCeilings))
	for k, v := range r.sharedCeilings {
		cp.sharedCeilings[k] = v
	}
	for _, opt := range opts {
		if opt != nil {
			opt(&cp)
		}
	}
	return &cp
}

// WithUserResolver overrides how Router resolves the per-turn user_id for
// mounts with PartitionByUser: true. Defaults to UserIDFromContext.
func WithUserResolver(fn func(context.Context) string) RouterOption {
	return func(r *Router) {
		if fn != nil {
			r.userResolver = fn
		}
	}
}

// WithBackendName labels the backend in OpenTelemetry spans and metrics
// (for example, "memorybank", "sqlite", "inmemory").
func WithBackendName(name string) RouterOption {
	return func(r *Router) {
		if trimmed := strings.TrimSpace(name); trimmed != "" {
			r.backendName = trimmed
		}
	}
}

// WithNamespace sets the default logical namespace on a Router so
// DeriveForSubagent preserves it even when mounted scopes use custom keys
// without "namespace" or when the parent has zero active mounts.
func WithNamespace(ns string) RouterOption {
	return func(r *Router) {
		if trimmed := strings.TrimSpace(ns); trimmed != "" {
			r.namespace = trimmed
		}
	}
}

type sharedCeiling struct {
	maxMode         string
	partitionByUser bool
}

func withSharedCeilings(ceilings map[string]sharedCeiling, globalMax string) RouterOption {
	return func(r *Router) {
		if globalMax != "" {
			r.sharedGlobalMax = globalMax
		}
		for name, anc := range ceilings {
			cur, ok := r.sharedCeilings[name]
			if !ok {
				r.sharedCeilings[name] = anc
				continue
			}
			maxM := cur.maxMode
			if scopeRank(anc.maxMode) < scopeRank(maxM) {
				maxM = anc.maxMode
			}
			r.sharedCeilings[name] = sharedCeiling{
				maxMode:         maxM,
				partitionByUser: cur.partitionByUser || anc.partitionByUser,
			}
		}
	}
}

// Router binds a Backend to an agent's or subagent's mounted scopes, enforces
// AccessMode, resolves per-turn UserID from context, executes concurrent
// fan-out on Recall, and emits audit events on mutations.
//
// Router is immutable after construction and safe for concurrent use across
// goroutines and sessions.
type Router struct {
	backend         Backend
	backendName     string
	namespace       string
	mounts          []MountTemplate
	byName          map[string]MountTemplate
	sharedCeilings  map[string]sharedCeiling
	sharedGlobalMax string
	defaultTopK     int
	strictRecall    bool
	audit           AuditRecorder
	userResolver    func(context.Context) string
}

// NewRouter constructs a validated Router over backend and mounts.
// mounts may be empty (producing an inert router whose Recall returns nil and
// whose mutating operations return ErrNoWritableMount).
func NewRouter(backend Backend, mounts []MountTemplate, opts ...RouterOption) (*Router, error) {
	if backend == nil {
		return nil, errors.New("memory.NewRouter: nil backend")
	}
	cloned := make([]MountTemplate, 0, len(mounts))
	byName := make(map[string]MountTemplate, len(mounts))
	ceilings := make(map[string]sharedCeiling, len(mounts))
	for i, m := range mounts {
		mc := m.Clone()
		if mc.WriteMode == "" {
			mc.WriteMode = WriteConsolidate
		}
		if err := mc.Validate(); err != nil {
			return nil, fmt.Errorf("mounts[%d]: %w", i, err)
		}
		if _, dup := byName[mc.Name]; dup {
			return nil, fmt.Errorf("%w: duplicate mount name %q", ErrInvalidMount, mc.Name)
		}
		cloned = append(cloned, mc)
		byName[mc.Name] = mc
		if mc.Name != MountAgent {
			ceilings[mc.Name] = sharedCeiling{
				maxMode:         string(mc.Mode),
				partitionByUser: mc.PartitionByUser,
			}
		}
	}
	r := &Router{
		backend:         backend,
		backendName:     "custom",
		mounts:          cloned,
		byName:          byName,
		sharedCeilings:  ceilings,
		sharedGlobalMax: SubagentScopeRW,
		defaultTopK:     DefaultTopK,
		userResolver:    UserIDFromContext,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(r)
		}
	}
	return r, nil
}

// Backend returns the underlying Backend wrapped by r.
func (r *Router) Backend() Backend {
	if r == nil {
		return nil
	}
	return r.backend
}

// Mounts returns a deep copy of the MountTemplates configured on r.
func (r *Router) Mounts() []MountTemplate {
	if r == nil || len(r.mounts) == 0 {
		return nil
	}
	out := make([]MountTemplate, len(r.mounts))
	for i, m := range r.mounts {
		out[i] = m.Clone()
	}
	return out
}

// HasWritableMount reports whether at least one mounted scope has Mode == "rw".
func (r *Router) HasWritableMount() bool {
	if r == nil {
		return false
	}
	for _, m := range r.mounts {
		if m.Mode == AccessReadWrite {
			return true
		}
	}
	return false
}

// Namespace returns the logical namespace configured on r (falling back to any
// mounted BaseScope["namespace"] or "default").
func (r *Router) Namespace() string {
	if r == nil {
		return "default"
	}
	return r.inferredNamespace()
}

// BindingSpec exports r's mounted scopes alongside ref and namespace for
// passing to out-of-process subagent workers.
func (r *Router) BindingSpec(ref, namespace string) BindingSpec {
	ns := strings.TrimSpace(namespace)
	if ns == "" {
		ns = r.Namespace()
	}
	return BindingSpec{
		Ref:       ref,
		Namespace: ns,
		Mounts:    r.Mounts(),
	}
}

// Recall executes concurrent fan-out retrieval across all targeted mounts
// (all mounts when mounts is empty or contains "all"), stamps Mount and
// ReadOnly provenance on every returned Item, merges and ranks results, and
// caps the combined slice to topK.
func (r *Router) Recall(ctx context.Context, query string, mounts []string, topics []string, topK int) (items []Item, warnings []MountWarning, err error) {
	if r == nil || len(r.mounts) == 0 {
		return nil, nil, nil
	}
	mountLabel := MountAll
	if len(mounts) == 1 && mounts[0] != "" {
		mountLabel = mounts[0]
	}
	ctx, endSpan := startOpSpan(ctx, "recall", r.backendName, mountLabel)
	defer func() { endSpan(len(items), err) }()

	targets, err := r.selectMounts(mounts)
	if err != nil {
		return nil, nil, err
	}
	if len(targets) == 0 {
		return nil, nil, nil
	}
	limit := r.effectiveTopK(topK)
	cleanedTopics := cleanTopics(topics)

	type mountResult struct {
		mount MountTemplate
		order int
		items []Item
		err   error
	}
	results := make([]mountResult, len(targets))
	var wg sync.WaitGroup
	for i, m := range targets {
		wg.Add(1)
		go func(idx int, mt MountTemplate) {
			defer wg.Done()
			scope, sErr := r.resolveScope(ctx, mt)
			if sErr != nil {
				results[idx] = mountResult{mount: mt, order: idx, err: sErr}
				return
			}
			got, bErr := r.backend.Retrieve(ctx, scope, RetrieveRequest{
				Query:  query,
				Topics: slices.Clone(cleanedTopics),
				TopK:   limit,
			})
			results[idx] = mountResult{mount: mt, order: idx, items: got, err: bErr}
		}(i, m)
	}
	wg.Wait()

	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, nil, ctxErr
	}

	type rankedItem struct {
		item       Item
		mountOrder int
		localRank  int
	}
	var (
		collected []rankedItem
		errs      []error
		succeeded int
	)
	for _, res := range results {
		if res.err != nil {
			wrapped := fmt.Errorf("mount %q: %w", res.mount.Name, res.err)
			errs = append(errs, wrapped)
			warnings = append(warnings, MountWarning{
				Mount:   res.mount.Name,
				Message: res.err.Error(),
			})
			continue
		}
		succeeded++
		for rank, it := range res.items {
			if !matchesTopicsFilter(it.Topics, cleanedTopics) {
				continue
			}
			cloned := it.Clone()
			cloned.Mount = res.mount.Name
			cloned.ReadOnly = res.mount.Mode == AccessReadOnly
			collected = append(collected, rankedItem{
				item:       cloned,
				mountOrder: res.order,
				localRank:  rank,
			})
		}
	}

	if len(errs) > 0 && (r.strictRecall || succeeded == 0) {
		return nil, nil, errors.Join(errs...)
	}

	slices.SortStableFunc(collected, func(a, b rankedItem) int {
		return compareRankedItems(a.item, b.item, a.mountOrder, b.mountOrder, a.localRank, b.localRank)
	})
	if len(collected) > limit {
		collected = collected[:limit]
	}
	items = make([]Item, len(collected))
	for i, ri := range collected {
		items[i] = ri.item
	}
	return items, warnings, nil
}

// Remember writes or consolidates candidate facts into mount (defaulting to
// "agent" when mount is empty and "agent" is mounted rw).
func (r *Router) Remember(ctx context.Context, mount string, facts []CandidateFact) (deltas []ConsolidationDelta, err error) {
	if r == nil {
		return nil, ErrNoWritableMount
	}
	targetName := mount
	if targetName == "" {
		targetName = MountAgent
	}
	ctx, endSpan := startOpSpan(ctx, "remember", r.backendName, targetName)
	defer func() { endSpan(len(deltas), err) }()

	mt, err := r.resolveWritableMount(mount)
	if err != nil {
		return nil, err
	}
	if len(facts) == 0 {
		return nil, fmt.Errorf("%w: no facts provided", ErrEmptyFact)
	}
	prepared := make([]CandidateFact, len(facts))
	for i, f := range facts {
		trimmed := strings.TrimSpace(f.Fact)
		if trimmed == "" {
			return nil, fmt.Errorf("%w: facts[%d] is empty", ErrEmptyFact, i)
		}
		ttl := f.TTL
		if ttl == 0 && mt.DefaultTTL > 0 {
			ttl = mt.DefaultTTL
		}
		prepared[i] = CandidateFact{
			Fact:   trimmed,
			Topics: cleanTopics(f.Topics),
			TTL:    ttl,
		}
	}
	scope, err := r.resolveScope(ctx, mt)
	if err != nil {
		return nil, err
	}
	writeMode := mt.WriteMode
	if writeMode == "" {
		writeMode = WriteConsolidate
	}
	deltas, err = r.backend.Put(ctx, scope, PutRequest{
		Facts:     prepared,
		WriteMode: writeMode,
	})
	if err != nil {
		return nil, err
	}
	if err := r.recordAudit(ctx, AuditAuthorRemember, map[string]any{
		"mount":  mt.Name,
		"scope":  scope.Clone(),
		"deltas": slices.Clone(deltas),
	}); err != nil {
		return deltas, err
	}
	return deltas, nil
}

// Forget deletes memory item id from the named writable mount.
// mount is always required on Forget so cross-scope deletions cannot happen
// implicitly.
func (r *Router) Forget(ctx context.Context, mount string, id string) (err error) {
	if r == nil {
		return ErrNoWritableMount
	}
	ctx, endSpan := startOpSpan(ctx, "forget", r.backendName, mount)
	defer func() { endSpan(0, err) }()

	if strings.TrimSpace(mount) == "" {
		return fmt.Errorf("%w: mount name is required on Forget", ErrUnknownMount)
	}
	mt, ok := r.byName[mount]
	if !ok {
		return fmt.Errorf("%w: %q", ErrUnknownMount, mount)
	}
	if mt.Mode != AccessReadWrite {
		return fmt.Errorf("%w: %q", ErrReadOnlyMount, mt.Name)
	}
	trimmedID := strings.TrimSpace(id)
	if trimmedID == "" {
		return ErrEmptyID
	}
	scope, err := r.resolveScope(ctx, mt)
	if err != nil {
		return err
	}
	if err := r.backend.Delete(ctx, scope, trimmedID); err != nil {
		return err
	}
	return r.recordAudit(ctx, AuditAuthorForget, map[string]any{
		"mount": mt.Name,
		"scope": scope.Clone(),
		"id":    trimmedID,
	})
}

// ConsolidateSession extracts dialogue turns from sess (optionally since a
// cutoff timestamp) and consolidates them into mount (defaulting to "agent"
// when mount is empty).
func (r *Router) ConsolidateSession(ctx context.Context, mount string, sess session.Session, since *time.Time) (deltas []ConsolidationDelta, err error) {
	if r == nil {
		return nil, ErrNoWritableMount
	}
	targetName := mount
	if targetName == "" {
		targetName = MountAgent
	}
	ctx, endSpan := startOpSpan(ctx, "consolidate", r.backendName, targetName)
	defer func() { endSpan(len(deltas), err) }()

	if sess == nil {
		return nil, errors.New("memory.ConsolidateSession: nil session")
	}
	mt, err := r.resolveWritableMount(mount)
	if err != nil {
		return nil, err
	}
	if r.userResolver(ctx) == "" && strings.TrimSpace(sess.UserID()) != "" {
		ctx = WithUserID(ctx, sess.UserID())
	}
	scope, err := r.resolveScope(ctx, mt)
	if err != nil {
		return nil, err
	}
	turns := ExtractConversationTurns(sess, since)
	_, _, hasVertexOrigin := vertexOrigin(sess)
	if len(turns) == 0 && !hasVertexOrigin {
		return nil, nil
	}
	deltas, err = r.backend.ConsolidateConversation(ctx, scope, ConversationRequest{
		SessionID: sess.ID(),
		Session:   sess,
		Turns:     turns,
		StartTime: since,
	})
	if err != nil {
		return nil, err
	}
	source := "direct_contents"
	if hasVertexOrigin {
		source = "vertex_session_or_contents"
	}
	if err := r.recordAudit(ctx, AuditAuthorConsolidate, map[string]any{
		"mount":      mt.Name,
		"scope":      scope.Clone(),
		"session_id": sess.ID(),
		"source":     source,
		"deltas":     slices.Clone(deltas),
	}); err != nil {
		return deltas, err
	}
	return deltas, nil
}

// ExtractConversationTurns extracts non-partial user and model text turns from
// sess.Events(), skipping synthetic tail-repair events and internal memory/*
// audit events.
func ExtractConversationTurns(sess session.Session, since *time.Time) []ConversationTurn {
	if sess == nil || sess.Events() == nil {
		return nil
	}
	var out []ConversationTurn
	for ev := range sess.Events().All() {
		if ev == nil || ev.Partial || ev.Content == nil {
			continue
		}
		if since != nil && !ev.Timestamp.IsZero() && ev.Timestamp.Before(*since) {
			continue
		}
		if strings.HasPrefix(ev.Author, "memory/") {
			continue
		}
		if rep, ok := ev.CustomMetadata["core_agent_synthetic_repair"].(bool); ok && rep {
			continue
		}
		var parts []string
		for _, p := range ev.Content.Parts {
			if p == nil || p.Thought {
				continue
			}
			if txt := strings.TrimSpace(StripContextBlocks(p.Text)); txt != "" {
				parts = append(parts, txt)
			}
		}
		if len(parts) == 0 {
			continue
		}
		role := strings.TrimSpace(ev.Content.Role)
		if role == "" && ev.Author == "user" {
			role = "user"
		}
		if role != "user" && role != "model" {
			// Skip internal non-LLM audit markers (e.g. core-agent/autonomous
			// events whose Content.Role is intentionally left empty per ADK v2).
			continue
		}
		out = append(out, ConversationTurn{
			Role: role,
			Text: strings.Join(parts, "\n"),
		})
	}
	return out
}

func vertexOrigin(sess session.Session) (parent, res string, ok bool) {
	vo, implements := sess.(VertexSessionOrigin)
	if !implements {
		return "", "", false
	}
	return vo.VertexSessionResource()
}

func (r *Router) selectMounts(names []string) ([]MountTemplate, error) {
	if len(names) == 0 {
		return r.mounts, nil
	}
	for _, n := range names {
		if strings.TrimSpace(n) == MountAll {
			return r.mounts, nil
		}
	}
	seen := make(map[string]bool, len(names))
	out := make([]MountTemplate, 0, len(names))
	for _, raw := range names {
		n := strings.TrimSpace(raw)
		if n == "" {
			continue
		}
		if seen[n] {
			continue
		}
		mt, ok := r.byName[n]
		if !ok {
			return nil, fmt.Errorf("%w: %q", ErrUnknownMount, n)
		}
		seen[n] = true
		out = append(out, mt)
	}
	return out, nil
}

func (r *Router) resolveWritableMount(mount string) (MountTemplate, error) {
	if !r.HasWritableMount() {
		return MountTemplate{}, ErrNoWritableMount
	}
	name := strings.TrimSpace(mount)
	if name == "" {
		mt, ok := r.byName[MountAgent]
		if !ok || mt.Mode != AccessReadWrite {
			return MountTemplate{}, ErrAmbiguousWritableMount
		}
		return mt, nil
	}
	mt, ok := r.byName[name]
	if !ok {
		return MountTemplate{}, fmt.Errorf("%w: %q", ErrUnknownMount, name)
	}
	if mt.Mode != AccessReadWrite {
		return MountTemplate{}, fmt.Errorf("%w: %q", ErrReadOnlyMount, mt.Name)
	}
	return mt, nil
}

func (r *Router) resolveScope(ctx context.Context, mt MountTemplate) (ScopeMap, error) {
	scope := mt.BaseScope.Clone()
	if scope == nil {
		scope = make(ScopeMap, 1)
	}
	if mt.PartitionByUser {
		uid := strings.TrimSpace(r.userResolver(ctx))
		if uid == "" {
			uid = UserIDFromContext(ctx)
		}
		if uid == "" {
			return nil, fmt.Errorf("%w (mount %q)", ErrMissingUserIdentity, mt.Name)
		}
		scope[ScopeKeyUserID] = uid
	}
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	return scope, nil
}

func (r *Router) effectiveTopK(k int) int {
	if k <= 0 {
		return r.defaultTopK
	}
	return min(k, MaxTopK)
}

func (r *Router) recordAudit(ctx context.Context, author string, payload map[string]any) error {
	if r.audit == nil {
		return nil
	}
	if err := r.audit(ctx, author, payload); err != nil {
		return fmt.Errorf("%w: %w", ErrAuditFailed, err)
	}
	return nil
}

func matchesTopicsFilter(itemTopics, wanted []string) bool {
	cleaned := cleanTopics(wanted)
	if len(cleaned) == 0 {
		return true
	}
	if len(itemTopics) == 0 {
		return false
	}
	for _, wt := range cleaned {
		for _, it := range itemTopics {
			if strings.EqualFold(strings.TrimSpace(it), wt) {
				return true
			}
		}
	}
	return false
}

func cleanTopics(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, t := range in {
		trimmed := strings.TrimSpace(t)
		if trimmed == "" || seen[trimmed] {
			continue
		}
		seen[trimmed] = true
		out = append(out, trimmed)
	}
	return out
}

func compareRankedItems(a, b Item, mountOrderA, mountOrderB, rankA, rankB int) int {
	switch {
	case a.Distance != nil && b.Distance != nil:
		if c := cmp.Compare(*a.Distance, *b.Distance); c != 0 {
			return c
		}
	case a.Distance != nil && b.Distance == nil:
		return -1
	case a.Distance == nil && b.Distance != nil:
		return 1
	case a.Distance == nil && b.Distance == nil:
		// Preserve each backend's relevance order (e.g. BM25 / FTS5 rank)
		// before breaking ties by recency across mounts.
		if c := cmp.Compare(rankA, rankB); c != 0 {
			return c
		}
	}
	if !a.UpdatedAt.Equal(b.UpdatedAt) {
		if a.UpdatedAt.After(b.UpdatedAt) {
			return -1
		}
		return 1
	}
	if c := cmp.Compare(rankA, rankB); c != 0 {
		return c
	}
	if c := cmp.Compare(mountOrderA, mountOrderB); c != 0 {
		return c
	}
	return cmp.Compare(a.ID, b.ID)
}
