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

// Package memory provides the storage-neutral multi-scope long-term memory
// contract, concurrent fan-out Router, URI opener registry, in-memory store,
// and ADK v2 memory.Service bridge for core-agent.
//
// Backends that carry heavy third-party or cloud dependencies (such as Vertex
// AI / Gemini Enterprise Agent Platform Memory Bank or SQLite FTS5) live in
// separate subpackages so importing pkg/memory keeps slim library embeds free
// of cloud gapic clients and database drivers.
package memory

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"google.golang.org/adk/v2/session"
)

// AccessMode controls whether a mounted memory scope permits mutations.
type AccessMode string

const (
	// AccessReadWrite permits Recall, Remember, Forget, and consolidation.
	AccessReadWrite AccessMode = "rw"
	// AccessReadOnly permits Recall only; mutating calls return ErrReadOnlyMount.
	AccessReadOnly AccessMode = "ro"
)

// Valid reports whether m is a supported AccessMode ("rw" or "ro").
func (m AccessMode) Valid() bool {
	return m == AccessReadWrite || m == AccessReadOnly
}

// WriteMode selects how explicit Remember calls land in backends that offer
// server-side LLM consolidation.
type WriteMode string

const (
	// WriteConsolidate merges candidate facts with existing facts in the same
	// scope (GenerateMemories with DirectMemoriesSource on Memory Bank).
	WriteConsolidate WriteMode = "consolidate"
	// WriteVerbatim writes each candidate fact directly without LLM merging
	// (CreateMemory on Memory Bank).
	WriteVerbatim WriteMode = "verbatim"
)

// Valid reports whether m is a supported WriteMode ("consolidate" or "verbatim").
func (m WriteMode) Valid() bool {
	return m == WriteConsolidate || m == WriteVerbatim
}

const (
	// MaxScopeKeys is the hard key-count ceiling enforced by Vertex AI Memory
	// Bank on scope dictionaries; enforced across all backends so recipes stay
	// portable.
	MaxScopeKeys = 5

	// MaxIdentifierLen bounds scope keys/values, mount names, and namespaces.
	MaxIdentifierLen = 256

	// DefaultTopK is the default maximum number of memories returned by Recall.
	DefaultTopK = 10

	// MaxTopK is the upper bound on Recall's per-call TopK parameter.
	MaxTopK = 50

	// MountAgent is the canonical mount name for an agent's private scope.
	MountAgent = "agent"

	// MountSharedPrefix prefixes named shared pool mounts ("shared:<pool>").
	MountSharedPrefix = "shared:"

	// MountAll targets every mounted scope on Recall.
	MountAll = "all"

	// Canonical ScopeMap keys constructed by AgentMount and SharedMount.
	ScopeKeyNamespace = "namespace"
	ScopeKeyKind      = "scope_kind"
	ScopeKeyAgentID   = "agent_id"
	ScopeKeyPool      = "pool"
	ScopeKeyUserID    = "user_id"

	// Canonical ScopeKeyKind values.
	ScopeKindAgent  = "agent"
	ScopeKindShared = "shared"

	// ConsolidationDelta action labels.
	ActionCreated = "CREATED"
	ActionUpdated = "UPDATED"
	ActionDeleted = "DELETED"
	ActionNoop    = "NOOP"

	// Session transcript audit event authors.
	AuditAuthorRemember    = "memory/remember"
	AuditAuthorForget      = "memory/forget"
	AuditAuthorConsolidate = "memory/consolidate"
)

// Sentinel errors returned by Router, ScopeMap, ParseRef, and Backend implementations.
var (
	ErrInvalidScope           = errors.New("memory: invalid scope map")
	ErrInvalidMount           = errors.New("memory: invalid mount template")
	ErrUnknownMount           = errors.New("memory: unknown mount")
	ErrReadOnlyMount          = errors.New("memory: mount is read-only")
	ErrNoWritableMount        = errors.New("memory: router has no writable mount")
	ErrAmbiguousWritableMount = errors.New("memory: mount must be specified when private agent mount is absent or read-only")
	ErrMissingUserIdentity    = errors.New("memory: mount requires user_id but no user identity is bound on context")
	ErrEmptyFact              = errors.New("memory: candidate fact text must not be empty")
	ErrEmptyID                = errors.New("memory: memory item id must not be empty")
	ErrNotFound               = errors.New("memory: memory item not found")
	ErrScopeMismatch          = errors.New("memory: memory item belongs to a different scope")
	ErrInvalidRef             = errors.New("memory: invalid memory reference URI")
	ErrUnknownScheme          = errors.New("memory: unsupported memory URI scheme")
	// ErrAuditFailed wraps an AuditRecorder error. The backend write it
	// records already succeeded, so callers should not retry the write.
	ErrAuditFailed = errors.New("memory: write succeeded but its audit record failed")
)

// ScopeMap is the exact-match partition key-value dictionary sent to a Backend.
// Backends match scopes by exact dictionary equality with no wildcards.
type ScopeMap map[string]string

// Validate checks that s has between 1 and MaxScopeKeys entries and that every
// key and value is valid non-empty UTF-8 without control characters,
// surrounding whitespace, or '*' wildcards.
func (s ScopeMap) Validate() error {
	if len(s) == 0 {
		return fmt.Errorf("%w: scope must contain at least 1 key", ErrInvalidScope)
	}
	if len(s) > MaxScopeKeys {
		return fmt.Errorf("%w: scope has %d keys (max %d)", ErrInvalidScope, len(s), MaxScopeKeys)
	}
	for k, v := range s {
		if err := validateScopeAtom("key", k); err != nil {
			return err
		}
		if err := validateScopeAtom(fmt.Sprintf("value for %q", k), v); err != nil {
			return err
		}
	}
	return nil
}

func validateScopeAtom(label, val string) error {
	if val == "" {
		return fmt.Errorf("%w: %s is empty", ErrInvalidScope, label)
	}
	if len(val) > MaxIdentifierLen {
		return fmt.Errorf("%w: %s exceeds %d bytes", ErrInvalidScope, label, MaxIdentifierLen)
	}
	if !utf8.ValidString(val) {
		return fmt.Errorf("%w: %s is not valid UTF-8", ErrInvalidScope, label)
	}
	if strings.TrimSpace(val) != val {
		return fmt.Errorf("%w: %s %q has surrounding whitespace", ErrInvalidScope, label, val)
	}
	if strings.ContainsRune(val, '*') {
		return fmt.Errorf("%w: %s %q must not contain '*' wildcards", ErrInvalidScope, label, val)
	}
	for _, r := range val {
		if unicode.IsControl(r) {
			return fmt.Errorf("%w: %s contains control character", ErrInvalidScope, label)
		}
	}
	return nil
}

// Clone returns a shallow copy of s, or nil if s is nil.
func (s ScopeMap) Clone() ScopeMap {
	if s == nil {
		return nil
	}
	return maps.Clone(s)
}

// Equal reports whether s and other contain the exact same key-value pairs.
func (s ScopeMap) Equal(other ScopeMap) bool {
	return maps.Equal(s, other)
}

// CanonicalKey returns a collision-free deterministic string representation of
// s using length-prefixed sorted keys and values. Two ScopeMaps produce the
// same CanonicalKey if and only if s.Equal(other) is true.
func (s ScopeMap) CanonicalKey() string {
	if len(s) == 0 {
		return ""
	}
	keys := make([]string, 0, len(s))
	for k := range s {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		v := s[k]
		fmt.Fprintf(&b, "%d:%s=%d:%s;", len(k), k, len(v), v)
	}
	return b.String()
}

// MountTemplate defines a logical memory partition mounted on a Router.
type MountTemplate struct {
	Name            string        `json:"name"`
	BaseScope       ScopeMap      `json:"base_scope"`
	PartitionByUser bool          `json:"partition_by_user,omitempty"`
	Mode            AccessMode    `json:"mode"`
	WriteMode       WriteMode     `json:"write_mode,omitempty"`
	DefaultTTL      time.Duration `json:"default_ttl,omitempty"`
}

// SharedMountName returns the canonical mount name ("shared:<pool>") for a
// shared pool name.
func SharedMountName(pool string) string {
	return MountSharedPrefix + pool
}

// ParseSharedMountName extracts <pool> from "shared:<pool>".
func ParseSharedMountName(mount string) (pool string, ok bool) {
	if !strings.HasPrefix(mount, MountSharedPrefix) {
		return "", false
	}
	pool = strings.TrimPrefix(mount, MountSharedPrefix)
	if pool == "" {
		return "", false
	}
	return pool, true
}

// AgentMount constructs a MountTemplate for an agent's private memory scope.
// If namespace is empty, "default" is used so the ScopeMap remains non-empty
// and self-describing across backends.
func AgentMount(namespace, agentID string, mode AccessMode) MountTemplate {
	if namespace == "" {
		namespace = "default"
	}
	return MountTemplate{
		Name: MountAgent,
		BaseScope: ScopeMap{
			ScopeKeyNamespace: namespace,
			ScopeKeyKind:      ScopeKindAgent,
			ScopeKeyAgentID:   agentID,
		},
		Mode:      mode,
		WriteMode: WriteConsolidate,
	}
}

// SharedMount constructs a MountTemplate for a named shared memory pool.
// If namespace is empty, "default" is used.
func SharedMount(namespace, pool string, mode AccessMode) MountTemplate {
	if namespace == "" {
		namespace = "default"
	}
	return MountTemplate{
		Name: SharedMountName(pool),
		BaseScope: ScopeMap{
			ScopeKeyNamespace: namespace,
			ScopeKeyKind:      ScopeKindShared,
			ScopeKeyPool:      pool,
		},
		Mode:      mode,
		WriteMode: WriteConsolidate,
	}
}

// Validate checks structural invariants on a MountTemplate.
func (m MountTemplate) Validate() error {
	if m.Name != MountAgent {
		pool, ok := ParseSharedMountName(m.Name)
		if !ok {
			return fmt.Errorf("%w: mount name %q must be %q or %q+<pool>", ErrInvalidMount, m.Name, MountAgent, MountSharedPrefix)
		}
		if err := validateScopeAtom("pool name", pool); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidMount, err)
		}
	}
	if !m.Mode.Valid() {
		return fmt.Errorf("%w: mount %q mode %q must be %q or %q", ErrInvalidMount, m.Name, m.Mode, AccessReadWrite, AccessReadOnly)
	}
	if m.WriteMode != "" && !m.WriteMode.Valid() {
		return fmt.Errorf("%w: mount %q write_mode %q must be %q or %q", ErrInvalidMount, m.Name, m.WriteMode, WriteConsolidate, WriteVerbatim)
	}
	if m.DefaultTTL < 0 {
		return fmt.Errorf("%w: mount %q default_ttl %v must be >= 0", ErrInvalidMount, m.Name, m.DefaultTTL)
	}
	if m.PartitionByUser {
		if _, exists := m.BaseScope[ScopeKeyUserID]; exists {
			return fmt.Errorf("%w: mount %q sets partition_by_user=true and also hardcodes %q in base_scope", ErrInvalidMount, m.Name, ScopeKeyUserID)
		}
		if len(m.BaseScope)+1 > MaxScopeKeys {
			return fmt.Errorf("%w: mount %q base_scope has %d keys, leaving no room for %q (max %d)", ErrInvalidMount, m.Name, len(m.BaseScope), ScopeKeyUserID, MaxScopeKeys)
		}
		for k, v := range m.BaseScope {
			if err := validateScopeAtom("key", k); err != nil {
				return fmt.Errorf("%w: mount %q: %v", ErrInvalidMount, m.Name, err)
			}
			if err := validateScopeAtom(fmt.Sprintf("value for %q", k), v); err != nil {
				return fmt.Errorf("%w: mount %q: %v", ErrInvalidMount, m.Name, err)
			}
		}
		return nil
	}
	if err := m.BaseScope.Validate(); err != nil {
		return fmt.Errorf("%w: mount %q: %v", ErrInvalidMount, m.Name, err)
	}
	return nil
}

// Clone returns a deep copy of m.
func (m MountTemplate) Clone() MountTemplate {
	m.BaseScope = m.BaseScope.Clone()
	return m
}

// Item is a single stored or retrieved memory entry.
type Item struct {
	ID        string            `json:"id"`
	Mount     string            `json:"mount,omitempty"`
	ReadOnly  bool              `json:"read_only,omitempty"`
	Fact      string            `json:"fact"`
	Topics    []string          `json:"topics,omitempty"`
	Distance  *float64          `json:"distance,omitempty"`
	Scope     ScopeMap          `json:"scope,omitempty"`
	CreatedAt time.Time         `json:"created_at,omitempty"`
	UpdatedAt time.Time         `json:"updated_at,omitempty"`
	ExpireAt  time.Time         `json:"expire_at,omitempty"`
	Metadata  map[string]string `json:"metadata,omitempty"`
}

// Clone returns a deep copy of it.
func (it Item) Clone() Item {
	it.Topics = slices.Clone(it.Topics)
	if it.Distance != nil {
		d := *it.Distance
		it.Distance = &d
	}
	it.Scope = it.Scope.Clone()
	it.Metadata = maps.Clone(it.Metadata)
	return it
}

// MountWarning reports a non-fatal error on an individual mount during a
// multi-mount fan-out Recall when strict_recall is false and at least one
// other targeted mount succeeded.
type MountWarning struct {
	Mount   string `json:"mount"`
	Message string `json:"message"`
}

// RetrieveRequest queries a single resolved ScopeMap on a Backend.
type RetrieveRequest struct {
	Query  string
	Topics []string
	TopK   int
}

// CandidateFact is one fact submitted to Backend.Put.
type CandidateFact struct {
	Fact     string
	Topics   []string
	TTL      time.Duration
	Metadata map[string]string
}

// PutRequest writes or consolidates one or more facts into a ScopeMap.
type PutRequest struct {
	Facts     []CandidateFact
	WriteMode WriteMode
}

// ConsolidationDelta reports one mutation produced by Put or
// ConsolidateConversation.
type ConsolidationDelta struct {
	ID     string `json:"id"`
	Action string `json:"action"`
	Fact   string `json:"fact,omitempty"`
}

// ConversationTurn is a sanitized dialogue turn extracted from a session for
// backend-agnostic conversation consolidation.
type ConversationTurn struct {
	Role string
	Text string
}

// ConversationRequest asks a Backend to extract and consolidate durable
// memories from session turns.
type ConversationRequest struct {
	SessionID string
	Session   session.Session
	Turns     []ConversationTurn
	StartTime *time.Time
}

// Backend is implemented by each storage provider (Memory Bank, SQLite FTS5,
// InMemory, Redis AMS, Postgres, MCP). Each call targets a single concrete
// ScopeMap; multi-scope fan-out and access control live once in Router.
// Implementations must be safe for concurrent use across goroutines.
type Backend interface {
	Retrieve(ctx context.Context, scope ScopeMap, req RetrieveRequest) ([]Item, error)
	Put(ctx context.Context, scope ScopeMap, req PutRequest) ([]ConsolidationDelta, error)
	Delete(ctx context.Context, scope ScopeMap, id string) error
	ConsolidateConversation(ctx context.Context, scope ScopeMap, req ConversationRequest) ([]ConsolidationDelta, error)
	Close() error
}

// VertexSessionOrigin is an optional interface that an Agent Engine-backed
// session.Session may implement. When VertexSessionResource returns ok=true
// with a reasoningEngineParent matching the Memory Bank Backend's engine,
// ConsolidateConversation can use zero-copy VertexSessionSource instead of
// re-uploading DirectContentsSource.
type VertexSessionOrigin interface {
	VertexSessionResource() (reasoningEngineParent string, sessionResource string, ok bool)
}

// AuditRecorder appends memory/remember, memory/forget, and memory/consolidate
// audit records to the active session transcript.
type AuditRecorder func(ctx context.Context, author string, payload map[string]any) error

// BindingSpec is the JSON/env-serializable representation of a Router's
// backend URI and scope mounts, passed to out-of-process subagent workers.
type BindingSpec struct {
	Ref       string          `json:"ref"`
	Namespace string          `json:"namespace,omitempty"`
	Mounts    []MountTemplate `json:"mounts"`
}

type userIDContextKey struct{}

// WithUserID binds a caller/session user identity onto ctx for resolving
// mounts that declare PartitionByUser: true.
func WithUserID(ctx context.Context, userID string) context.Context {
	return context.WithValue(ctx, userIDContextKey{}, strings.TrimSpace(userID))
}

// UserIDFromContext returns the user identity previously bound via WithUserID,
// or "" if none is present.
func UserIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v, _ := ctx.Value(userIDContextKey{}).(string)
	return v
}
