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

// Package memorybank implements pkg/memory.Backend on top of Vertex AI Agent
// Engine Memory Bank (cloud.google.com/go/aiplatform/apiv1beta1.MemoryBankClient).
//
// Importing this package registers the "memorybank" URI scheme with
// pkg/memory.RegisterOpener so memory.Open(ctx, "memorybank://...") and
// memory.OpenRouter work automatically.
package memorybank

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	aiplatform "cloud.google.com/go/aiplatform/apiv1beta1"
	"cloud.google.com/go/aiplatform/apiv1beta1/aiplatformpb"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/go-steer/core-agent/v3/pkg/memory"
)

// maxDirectMemoriesPerCall is Vertex AI Memory Bank's hard limit on
// DirectMemoriesSource.DirectMemories entries per GenerateMemories RPC.
const maxDirectMemoriesPerCall = 5

// ErrClosed is returned when an operation is invoked on a closed Backend.
var ErrClosed = errors.New("memorybank: backend is closed")

func init() {
	memory.RegisterOpener(memory.SchemeMemoryBank, OpenRef)
}

// Config configures a Memory Bank Backend.
type Config struct {
	// ReasoningEngine is the Agent Engine parent resource name in either canonical
	// form ("projects/{project}/locations/{location}/reasoningEngines/{id}") or
	// compact 3-segment form ("{project}/{location}/{id}").
	ReasoningEngine string

	// ClientOptions are appended after the regional endpoint
	// ("{location}-aiplatform.googleapis.com:443") so caller overrides such as
	// option.WithGRPCConn in tests take precedence.
	ClientOptions []option.ClientOption

	// RetryBackoff is the wait duration before retrying a transient
	// ResourceExhausted or Unavailable response. Zero defaults to 2s; a negative
	// value disables retries.
	RetryBackoff time.Duration

	// Now overrides time.Now for expiration checks in tests.
	Now func() time.Time
}

// Validate checks Config without dialing the service.
func (c Config) Validate() error {
	if _, _, _, err := memory.ParseReasoningEngineResource(c.ReasoningEngine); err != nil {
		return fmt.Errorf("memorybank: %w", err)
	}
	return nil
}

// Backend implements pkg/memory.Backend against Vertex AI Agent Engine Memory Bank.
type Backend struct {
	client       *aiplatform.MemoryBankClient
	parent       string
	retryBackoff time.Duration
	retryBudget  *retryBudget
	now          func() time.Time
	closed       atomic.Bool
}

var _ memory.Backend = (*Backend)(nil)

// Open validates cfg and constructs a Memory Bank Backend connected to the
// regional Vertex AI endpoint for cfg.ReasoningEngine.
func Open(ctx context.Context, cfg Config) (*Backend, error) {
	proj, loc, id, err := memory.ParseReasoningEngineResource(cfg.ReasoningEngine)
	if err != nil {
		return nil, fmt.Errorf("memorybank: %w", err)
	}
	parent := memory.FormatReasoningEngineResource(proj, loc, id)
	opts := append(
		[]option.ClientOption{option.WithEndpoint(loc + "-aiplatform.googleapis.com:443")},
		cfg.ClientOptions...,
	)
	client, err := aiplatform.NewMemoryBankClient(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("memorybank: connect: %w", err)
	}
	backoff := cfg.RetryBackoff
	if backoff == 0 {
		backoff = defaultRetryBackoff
	}
	nowFn := cfg.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	return &Backend{
		client:       client,
		parent:       parent,
		retryBackoff: backoff,
		retryBudget:  newRetryBudget(),
		now:          nowFn,
	}, nil
}

// OpenRef implements memory.Opener for "memorybank://" URIs.
func OpenRef(ctx context.Context, ref memory.ParsedRef) (memory.Backend, error) {
	if ref.Scheme != memory.SchemeMemoryBank {
		return nil, fmt.Errorf("memorybank: unexpected scheme %q", ref.Scheme)
	}
	return Open(ctx, Config{
		ReasoningEngine: ref.Resource,
	})
}

// Parent returns the canonical ReasoningEngine resource name
// ("projects/{project}/locations/{location}/reasoningEngines/{id}").
func (b *Backend) Parent() string { return b.parent }

// Retrieve queries or lists memories in scope via RetrieveMemories.
func (b *Backend) Retrieve(ctx context.Context, scope memory.ScopeMap, req memory.RetrieveRequest) ([]memory.Item, error) {
	if err := b.checkPreconditions(ctx, scope); err != nil {
		return nil, err
	}
	topK := req.TopK
	if topK <= 0 {
		topK = memory.DefaultTopK
	}
	topK = min(topK, memory.MaxTopK)

	fetchK := topK
	if len(req.Topics) > 0 {
		fetchK = min(max(topK*3, memory.DefaultTopK), memory.MaxTopK)
	}

	query := strings.TrimSpace(req.Query)
	rpcReq := &aiplatformpb.RetrieveMemoriesRequest{
		Parent: b.parent,
		Scope:  scope.Clone(),
	}
	if query != "" {
		rpcReq.RetrievalParams = &aiplatformpb.RetrieveMemoriesRequest_SimilaritySearchParams_{
			SimilaritySearchParams: &aiplatformpb.RetrieveMemoriesRequest_SimilaritySearchParams{
				SearchQuery: query,
				TopK:        int32(fetchK),
			},
		}
	} else {
		rpcReq.RetrievalParams = &aiplatformpb.RetrieveMemoriesRequest_SimpleRetrievalParams_{
			SimpleRetrievalParams: &aiplatformpb.RetrieveMemoriesRequest_SimpleRetrievalParams{
				PageSize: int32(fetchK),
			},
		}
	}

	var resp *aiplatformpb.RetrieveMemoriesResponse
	if err := b.withRetry(ctx, func() error {
		var callErr error
		resp, callErr = b.client.RetrieveMemories(ctx, rpcReq)
		return callErr
	}); err != nil {
		return nil, fmt.Errorf("memorybank: retrieve: %w", err)
	}

	now := b.now()
	out := make([]memory.Item, 0, len(resp.GetRetrievedMemories()))
	for _, rm := range resp.GetRetrievedMemories() {
		pbMem := rm.GetMemory()
		if pbMem == nil {
			continue
		}
		it := b.itemFromProto(pbMem, scope)
		if !it.ExpireAt.IsZero() && !now.Before(it.ExpireAt) {
			continue
		}
		if !matchesTopicsFilter(it.Topics, req.Topics) {
			continue
		}
		if query != "" {
			d := rm.GetDistance()
			it.Distance = &d
		}
		out = append(out, it)
	}

	slices.SortFunc(out, func(a, b memory.Item) int {
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

	if len(out) > topK {
		out = out[:topK]
	}
	return out, nil
}

// Put writes candidate facts into scope using either WriteConsolidate
// (GenerateMemories with DirectMemoriesSource in chunks of 5) or WriteVerbatim
// (CreateMemory per candidate fact).
func (b *Backend) Put(ctx context.Context, scope memory.ScopeMap, req memory.PutRequest) ([]memory.ConsolidationDelta, error) {
	if err := b.checkPreconditions(ctx, scope); err != nil {
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
		return nil, fmt.Errorf("memorybank: invalid write mode %q", writeMode)
	}
	for i, f := range req.Facts {
		if strings.TrimSpace(f.Fact) == "" {
			return nil, fmt.Errorf("%w: facts[%d]", memory.ErrEmptyFact, i)
		}
	}

	if writeMode == memory.WriteVerbatim {
		return b.putVerbatim(ctx, scope, req.Facts)
	}
	return b.putConsolidate(ctx, scope, req.Facts)
}

func (b *Backend) putVerbatim(ctx context.Context, scope memory.ScopeMap, facts []memory.CandidateFact) ([]memory.ConsolidationDelta, error) {
	deltas := make([]memory.ConsolidationDelta, 0, len(facts))
	for _, f := range facts {
		factText := strings.TrimSpace(f.Fact)
		pbMem := &aiplatformpb.Memory{
			Fact:        factText,
			Scope:       scope.Clone(),
			Description: encodeSidecar(cleanTopics(f.Topics), f.Metadata),
		}
		if f.TTL > 0 {
			pbMem.Expiration = &aiplatformpb.Memory_Ttl{
				Ttl: durationpb.New(f.TTL),
			}
		}
		var (
			op      *aiplatform.CreateMemoryOperation
			created *aiplatformpb.Memory
		)
		if err := b.withRetry(ctx, func() error {
			if op == nil {
				var callErr error
				op, callErr = b.client.CreateMemory(ctx, &aiplatformpb.CreateMemoryRequest{
					Parent: b.parent,
					Memory: pbMem,
				})
				if callErr != nil {
					return callErr
				}
			}
			var waitErr error
			created, waitErr = op.Wait(ctx)
			return waitErr
		}); err != nil {
			return nil, fmt.Errorf("memorybank: create memory: %w", err)
		}
		gotFact := created.GetFact()
		if gotFact == "" {
			gotFact = factText
		}
		deltas = append(deltas, memory.ConsolidationDelta{
			ID:     b.shortMemoryID(created.GetName()),
			Action: memory.ActionCreated,
			Fact:   gotFact,
		})
	}
	return deltas, nil
}

func (b *Backend) putConsolidate(ctx context.Context, scope memory.ScopeMap, facts []memory.CandidateFact) ([]memory.ConsolidationDelta, error) {
	var deltas []memory.ConsolidationDelta
	for start := 0; start < len(facts); {
		end := start + 1
		for end < len(facts) && end-start < maxDirectMemoriesPerCall && sameFactAnnotations(facts[start], facts[end]) {
			end++
		}
		batchDeltas, err := b.consolidateBatch(ctx, scope, facts[start:end])
		if err != nil {
			return nil, err
		}
		deltas = append(deltas, batchDeltas...)
		start = end
	}
	return deltas, nil
}

func sameFactAnnotations(a, b memory.CandidateFact) bool {
	return a.TTL == b.TTL &&
		slices.Equal(cleanTopics(a.Topics), cleanTopics(b.Topics)) &&
		maps.Equal(a.Metadata, b.Metadata)
}

func (b *Backend) consolidateBatch(ctx context.Context, scope memory.ScopeMap, batch []memory.CandidateFact) ([]memory.ConsolidationDelta, error) {
	direct := make([]*aiplatformpb.GenerateMemoriesRequest_DirectMemoriesSource_DirectMemory, 0, len(batch))
	batchTopics := cleanTopics(batch[0].Topics)
	batchMeta := maps.Clone(batch[0].Metadata)
	batchTTL := batch[0].TTL
	for _, f := range batch {
		factText := strings.TrimSpace(f.Fact)
		direct = append(direct, &aiplatformpb.GenerateMemoriesRequest_DirectMemoriesSource_DirectMemory{
			Fact: factText,
		})
	}

	rpcReq := &aiplatformpb.GenerateMemoriesRequest{
		Parent: b.parent,
		Scope:  scope.Clone(),
		Source: &aiplatformpb.GenerateMemoriesRequest_DirectMemoriesSource_{
			DirectMemoriesSource: &aiplatformpb.GenerateMemoriesRequest_DirectMemoriesSource{
				DirectMemories: direct,
			},
		},
	}

	var (
		op   *aiplatform.GenerateMemoriesOperation
		resp *aiplatformpb.GenerateMemoriesResponse
	)
	if err := b.withRetry(ctx, func() error {
		if op == nil {
			var callErr error
			op, callErr = b.client.GenerateMemories(ctx, rpcReq)
			if callErr != nil {
				return callErr
			}
		}
		var waitErr error
		resp, waitErr = op.Wait(ctx)
		return waitErr
	}); err != nil {
		return nil, fmt.Errorf("memorybank: generate memories: %w", err)
	}

	needsSidecarUpdate := len(batchTopics) > 0 || len(batchMeta) > 0 || batchTTL > 0
	deltas := make([]memory.ConsolidationDelta, 0, len(resp.GetGeneratedMemories()))
	for _, gm := range resp.GetGeneratedMemories() {
		mem := gm.GetMemory()
		action := mapGeneratedAction(gm.GetAction())
		if needsSidecarUpdate && mem != nil && mem.GetName() != "" &&
			(action == memory.ActionCreated || action == memory.ActionUpdated) {
			if err := b.annotateConsolidatedMemory(ctx, mem, batchTopics, batchMeta, batchTTL); err != nil {
				return nil, err
			}
		}
		deltas = append(deltas, memory.ConsolidationDelta{
			ID:     b.shortMemoryID(mem.GetName()),
			Action: action,
			Fact:   mem.GetFact(),
		})
	}
	return deltas, nil
}

func (b *Backend) annotateConsolidatedMemory(ctx context.Context, mem *aiplatformpb.Memory, topics []string, meta map[string]string, ttl time.Duration) error {
	existingTopics, existingMeta := decodeSidecar(mem.GetDescription())
	mergedTopics := mergeTopics(existingTopics, topics)
	var mergedMeta map[string]string
	if len(existingMeta) > 0 || len(meta) > 0 {
		mergedMeta = make(map[string]string, len(existingMeta)+len(meta))
		maps.Copy(mergedMeta, existingMeta)
		maps.Copy(mergedMeta, meta)
	}
	updMem := &aiplatformpb.Memory{
		Name:        mem.GetName(),
		Fact:        mem.GetFact(),
		Description: encodeSidecar(mergedTopics, mergedMeta),
	}
	paths := []string{"description"}
	if ttl > 0 {
		updMem.Expiration = &aiplatformpb.Memory_Ttl{Ttl: durationpb.New(ttl)}
		paths = append(paths, "ttl")
	}
	var op *aiplatform.UpdateMemoryOperation
	if err := b.withRetry(ctx, func() error {
		if op == nil {
			var callErr error
			op, callErr = b.client.UpdateMemory(ctx, &aiplatformpb.UpdateMemoryRequest{
				Memory:     updMem,
				UpdateMask: &fieldmaskpb.FieldMask{Paths: paths},
			})
			if callErr != nil {
				return callErr
			}
		}
		_, waitErr := op.Wait(ctx)
		return waitErr
	}); err != nil {
		return fmt.Errorf("memorybank: update memory metadata: %w", err)
	}
	return nil
}

// Delete verifies that id belongs to scope via GetMemory and then deletes the
// memory resource via DeleteMemory. Cross-scope delete attempts return an error
// wrapping both memory.ErrScopeMismatch and memory.ErrNotFound without
// modifying the target memory.
func (b *Backend) Delete(ctx context.Context, scope memory.ScopeMap, id string) error {
	if err := b.checkPreconditions(ctx, scope); err != nil {
		return err
	}
	trimmed := strings.TrimSpace(id)
	if trimmed == "" {
		return memory.ErrEmptyID
	}
	fullName, err := b.resolveMemoryName(trimmed)
	if err != nil {
		return err
	}

	var got *aiplatformpb.Memory
	if err := b.withRetry(ctx, func() error {
		var callErr error
		got, callErr = b.client.GetMemory(ctx, &aiplatformpb.GetMemoryRequest{Name: fullName})
		return callErr
	}); err != nil {
		if status.Code(err) == codes.NotFound {
			return fmt.Errorf("%w: %q", memory.ErrNotFound, trimmed)
		}
		return fmt.Errorf("memorybank: get memory for delete: %w", err)
	}

	if !memory.ScopeMap(got.GetScope()).Equal(scope) {
		return fmt.Errorf("%w (%w): %q", memory.ErrScopeMismatch, memory.ErrNotFound, trimmed)
	}

	var delOp *aiplatform.DeleteMemoryOperation
	if err := b.withRetry(ctx, func() error {
		if delOp == nil {
			var callErr error
			delOp, callErr = b.client.DeleteMemory(ctx, &aiplatformpb.DeleteMemoryRequest{Name: fullName})
			if callErr != nil {
				return callErr
			}
		}
		return delOp.Wait(ctx)
	}); err != nil {
		if status.Code(err) == codes.NotFound {
			return fmt.Errorf("%w: %q", memory.ErrNotFound, trimmed)
		}
		return fmt.Errorf("memorybank: delete memory: %w", err)
	}
	return nil
}

// ConsolidateConversation extracts and consolidates memories from session
// dialogue turns via GenerateMemories. When req.Session implements
// memory.VertexSessionOrigin and belongs to the same ReasoningEngine parent,
// ConsolidateConversation uses zero-copy VertexSessionSource; otherwise it
// sends req.Turns via DirectContentsSource.
func (b *Backend) ConsolidateConversation(ctx context.Context, scope memory.ScopeMap, req memory.ConversationRequest) ([]memory.ConsolidationDelta, error) {
	if err := b.checkPreconditions(ctx, scope); err != nil {
		return nil, err
	}

	rpcReq := &aiplatformpb.GenerateMemoriesRequest{
		Parent: b.parent,
		Scope:  scope.Clone(),
	}

	if engParent, sessRes, ok := extractVertexSessionOrigin(req.Session); ok && engParent == b.parent && sessRes != "" {
		vss := &aiplatformpb.GenerateMemoriesRequest_VertexSessionSource{
			Session: sessRes,
		}
		if req.StartTime != nil && !req.StartTime.IsZero() {
			vss.StartTime = timestamppb.New(*req.StartTime)
		}
		rpcReq.Source = &aiplatformpb.GenerateMemoriesRequest_VertexSessionSource_{
			VertexSessionSource: vss,
		}
	} else {
		var events []*aiplatformpb.GenerateMemoriesRequest_DirectContentsSource_Event
		for _, turn := range req.Turns {
			txt := strings.TrimSpace(turn.Text)
			if txt == "" {
				continue
			}
			role := strings.TrimSpace(turn.Role)
			if role == "" {
				role = "user"
			}
			events = append(events, &aiplatformpb.GenerateMemoriesRequest_DirectContentsSource_Event{
				Content: &aiplatformpb.Content{
					Role: role,
					Parts: []*aiplatformpb.Part{
						{Data: &aiplatformpb.Part_Text{Text: txt}},
					},
				},
			})
		}
		if len(events) == 0 {
			return nil, nil
		}
		rpcReq.Source = &aiplatformpb.GenerateMemoriesRequest_DirectContentsSource_{
			DirectContentsSource: &aiplatformpb.GenerateMemoriesRequest_DirectContentsSource{
				Events: events,
			},
		}
	}

	var (
		op   *aiplatform.GenerateMemoriesOperation
		resp *aiplatformpb.GenerateMemoriesResponse
	)
	if err := b.withRetry(ctx, func() error {
		if op == nil {
			var callErr error
			op, callErr = b.client.GenerateMemories(ctx, rpcReq)
			if callErr != nil {
				return callErr
			}
		}
		var waitErr error
		resp, waitErr = op.Wait(ctx)
		return waitErr
	}); err != nil {
		return nil, fmt.Errorf("memorybank: consolidate conversation: %w", err)
	}

	deltas := make([]memory.ConsolidationDelta, 0, len(resp.GetGeneratedMemories()))
	for _, gm := range resp.GetGeneratedMemories() {
		mem := gm.GetMemory()
		deltas = append(deltas, memory.ConsolidationDelta{
			ID:     b.shortMemoryID(mem.GetName()),
			Action: mapGeneratedAction(gm.GetAction()),
			Fact:   mem.GetFact(),
		})
	}
	return deltas, nil
}

// Close closes the underlying gRPC MemoryBankClient connection.
func (b *Backend) Close() error {
	if !b.closed.CompareAndSwap(false, true) {
		return nil
	}
	return b.client.Close()
}

func (b *Backend) checkPreconditions(ctx context.Context, scope memory.ScopeMap) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if b.closed.Load() {
		return ErrClosed
	}
	return scope.Validate()
}

func (b *Backend) resolveMemoryName(id string) (string, error) {
	prefix := b.parent + "/memories/"
	if strings.HasPrefix(id, "projects/") {
		if !strings.HasPrefix(id, prefix) {
			return "", fmt.Errorf("%w (%w): %q does not belong to engine %q", memory.ErrScopeMismatch, memory.ErrNotFound, id, b.parent)
		}
		short := strings.TrimPrefix(id, prefix)
		if short == "" || strings.Contains(short, "/") {
			return "", fmt.Errorf("%w: invalid memory resource %q", memory.ErrNotFound, id)
		}
		return id, nil
	}
	if strings.Contains(id, "/") {
		return "", fmt.Errorf("%w: invalid memory id %q", memory.ErrNotFound, id)
	}
	return prefix + id, nil
}

func (b *Backend) shortMemoryID(name string) string {
	prefix := b.parent + "/memories/"
	if strings.HasPrefix(name, prefix) {
		return strings.TrimPrefix(name, prefix)
	}
	return name
}

func (b *Backend) itemFromProto(pbMem *aiplatformpb.Memory, fallbackScope memory.ScopeMap) memory.Item {
	topics, meta := decodeSidecar(pbMem.GetDescription())
	scope := memory.ScopeMap(pbMem.GetScope()).Clone()
	if len(scope) == 0 {
		scope = fallbackScope.Clone()
	}
	it := memory.Item{
		ID:       b.shortMemoryID(pbMem.GetName()),
		Fact:     pbMem.GetFact(),
		Topics:   topics,
		Scope:    scope,
		Metadata: meta,
	}
	if ts := pbMem.GetCreateTime(); ts != nil {
		it.CreatedAt = ts.AsTime()
	}
	if ts := pbMem.GetUpdateTime(); ts != nil {
		it.UpdatedAt = ts.AsTime()
	}
	if exp := pbMem.GetExpireTime(); exp != nil {
		it.ExpireAt = exp.AsTime()
	}
	return it
}

func extractVertexSessionOrigin(sess any) (reasoningEngineParent, sessionResource string, ok bool) {
	if sess == nil {
		return "", "", false
	}
	if origin, matches := sess.(memory.VertexSessionOrigin); matches {
		return origin.VertexSessionResource()
	}
	type vertexOrigin interface {
		VertexSessionResource() (string, string, bool)
	}
	if origin, matches := sess.(vertexOrigin); matches {
		return origin.VertexSessionResource()
	}
	return "", "", false
}

func mapGeneratedAction(a aiplatformpb.GenerateMemoriesResponse_GeneratedMemory_Action) string {
	switch a {
	case aiplatformpb.GenerateMemoriesResponse_GeneratedMemory_CREATED:
		return memory.ActionCreated
	case aiplatformpb.GenerateMemoriesResponse_GeneratedMemory_UPDATED:
		return memory.ActionUpdated
	case aiplatformpb.GenerateMemoriesResponse_GeneratedMemory_DELETED:
		return memory.ActionDeleted
	default:
		return memory.ActionNoop
	}
}

type descriptionSidecar struct {
	CoreAgentV1 bool              `json:"core_agent_v1"`
	Topics      []string          `json:"topics,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

func encodeSidecar(topics []string, meta map[string]string) string {
	if len(topics) == 0 && len(meta) == 0 {
		return ""
	}
	raw, err := json.Marshal(descriptionSidecar{
		CoreAgentV1: true,
		Topics:      topics,
		Metadata:    meta,
	})
	if err != nil {
		return ""
	}
	return string(raw)
}

func decodeSidecar(desc string) ([]string, map[string]string) {
	trimmed := strings.TrimSpace(desc)
	if !strings.HasPrefix(trimmed, "{") {
		return nil, nil
	}
	var sc descriptionSidecar
	if err := json.Unmarshal([]byte(trimmed), &sc); err != nil || !sc.CoreAgentV1 {
		return nil, nil
	}
	return slices.Clone(sc.Topics), maps.Clone(sc.Metadata)
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
