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

package memory_test

import (
	"context"
	"errors"
	"iter"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	adkmemory "google.golang.org/adk/v2/memory"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v3/pkg/memory"
	"github.com/go-steer/core-agent/v3/pkg/memory/memtest"
)

func TestInMemoryConformance(t *testing.T) {
	t.Parallel()
	memtest.RunConformance(t, func(_ *testing.T) memory.Backend {
		return memory.NewInMemory()
	})
}

func TestScopeMapCanonicalKeyCollisionFreedom(t *testing.T) {
	t.Parallel()
	s1 := memory.ScopeMap{"a": "b=1;2:c=3", "x": "y"}
	s2 := memory.ScopeMap{"a=b": "1;2:c=3", "x": "y"}
	if s1.CanonicalKey() == s2.CanonicalKey() {
		t.Fatalf("distinct scopes collided on CanonicalKey: %q", s1.CanonicalKey())
	}
	reordered := memory.ScopeMap{"x": "y", "a": "b=1;2:c=3"}
	if s1.CanonicalKey() != reordered.CanonicalKey() {
		t.Fatalf("equal scopes produced different CanonicalKey: %q vs %q", s1.CanonicalKey(), reordered.CanonicalKey())
	}
}

func TestMountTemplateValidation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		mt      memory.MountTemplate
		wantErr error
	}{
		{
			name: "valid_agent",
			mt:   memory.AgentMount("prod", "triage", memory.AccessReadWrite),
		},
		{
			name: "valid_shared_ro",
			mt:   memory.SharedMount("prod", "runbooks", memory.AccessReadOnly),
		},
		{
			name: "bad_mount_name",
			mt: memory.MountTemplate{
				Name:      "team-pool",
				BaseScope: memory.ScopeMap{"pool": "team"},
				Mode:      memory.AccessReadOnly,
			},
			wantErr: memory.ErrInvalidMount,
		},
		{
			name: "partition_by_user_conflicts_with_hardcoded_user_id",
			mt: memory.MountTemplate{
				Name:            memory.MountAgent,
				BaseScope:       memory.ScopeMap{"user_id": "fixed"},
				PartitionByUser: true,
				Mode:            memory.AccessReadWrite,
			},
			wantErr: memory.ErrInvalidMount,
		},
		{
			name: "partition_by_user_exceeds_5_keys",
			mt: memory.MountTemplate{
				Name: memory.MountAgent,
				BaseScope: memory.ScopeMap{
					"k1": "v1", "k2": "v2", "k3": "v3", "k4": "v4", "k5": "v5",
				},
				PartitionByUser: true,
				Mode:            memory.AccessReadWrite,
			},
			wantErr: memory.ErrInvalidMount,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.mt.Validate()
			if tc.wantErr == nil && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("want error %v, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestRouterMultiScopeFanOutAccessModesAndAudit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	var clockMu sync.Mutex
	backend := memory.NewInMemory(memory.WithClock(func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return now
	}))

	// Pre-seed read-only runbooks scope directly on the backend (as an operator
	// pipeline or admin job would).
	runbookMount := memory.SharedMount("prod", "runbooks", memory.AccessReadOnly)
	if _, err := backend.Put(ctx, runbookMount.BaseScope, memory.PutRequest{
		Facts:     []memory.CandidateFact{{Fact: "Drain Envoy weight to 0 before restarting checkout pods", Topics: []string{"envoy"}}},
		WriteMode: memory.WriteVerbatim,
	}); err != nil {
		t.Fatalf("seed runbooks: %v", err)
	}

	var audits []string
	var auditMu sync.Mutex
	rec := func(_ context.Context, author string, _ map[string]any) error {
		auditMu.Lock()
		defer auditMu.Unlock()
		audits = append(audits, author)
		return nil
	}

	agentMount := memory.AgentMount("prod", "gke-triage", memory.AccessReadWrite)
	agentMount.DefaultTTL = 10 * time.Minute
	fleetMount := memory.SharedMount("prod", "fleet", memory.AccessReadWrite)

	router, err := memory.NewRouter(
		backend,
		[]memory.MountTemplate{agentMount, fleetMount, runbookMount},
		memory.WithAuditRecorder(rec),
		memory.WithBackendName("inmemory"),
	)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}

	// Default Remember targets "agent".
	dPriv, err := router.Remember(ctx, "", []memory.CandidateFact{{Fact: "Checkout pod OOMs correlate with batch cron at :05"}})
	if err != nil {
		t.Fatalf("Remember(default->agent): %v", err)
	}
	if _, err := router.Remember(ctx, "shared:fleet", []memory.CandidateFact{{Fact: "Checkout namespace migrated to regional cluster"}}); err != nil {
		t.Fatalf("Remember(shared:fleet): %v", err)
	}

	// Writing to read-only mount fails with ErrReadOnlyMount and emits no audit event.
	if _, err := router.Remember(ctx, "shared:runbooks", []memory.CandidateFact{{Fact: "Unauthorized edit"}}); !errors.Is(err, memory.ErrReadOnlyMount) {
		t.Fatalf("Remember(ro): want ErrReadOnlyMount, got %v", err)
	}

	// Fan-out Recall across all 3 mounts returns items with Mount + ReadOnly provenance.
	items, warnings, err := router.Recall(ctx, "checkout pods", nil, nil, 10)
	if err != nil {
		t.Fatalf("Recall(all): %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %+v", warnings)
	}
	if len(items) != 3 {
		t.Fatalf("Recall(all) returned %d items, want 3: %+v", len(items), items)
	}
	byMount := map[string]memory.Item{}
	for _, it := range items {
		byMount[it.Mount] = it
	}
	if !byMount["shared:runbooks"].ReadOnly {
		t.Errorf("shared:runbooks item must have ReadOnly=true")
	}
	if byMount["agent"].ReadOnly || byMount["shared:fleet"].ReadOnly {
		t.Errorf("writable mounts must have ReadOnly=false")
	}

	// Forget refuses empty mount, refuses ro mount, refuses cross-scope delete, and deletes own item.
	if err := router.Forget(ctx, "", dPriv[0].ID); !errors.Is(err, memory.ErrUnknownMount) {
		t.Fatalf("Forget(empty mount): want ErrUnknownMount, got %v", err)
	}
	if err := router.Forget(ctx, "shared:runbooks", byMount["shared:runbooks"].ID); !errors.Is(err, memory.ErrReadOnlyMount) {
		t.Fatalf("Forget(ro mount): want ErrReadOnlyMount, got %v", err)
	}
	if err := router.Forget(ctx, "shared:fleet", dPriv[0].ID); !errors.Is(err, memory.ErrNotFound) {
		t.Fatalf("Forget(wrong mount): want ErrNotFound, got %v", err)
	}
	if err := router.Forget(ctx, "agent", dPriv[0].ID); err != nil {
		t.Fatalf("Forget(agent): %v", err)
	}

	// Re-add private item with DefaultTTL (10m), advance clock 15m, confirm expiry.
	if _, err := router.Remember(ctx, "agent", []memory.CandidateFact{{Fact: "Ephemeral checkout incident note"}}); err != nil {
		t.Fatalf("Remember(ttl): %v", err)
	}
	clockMu.Lock()
	now = now.Add(15 * time.Minute)
	clockMu.Unlock()

	privAfterExpiry, _, err := router.Recall(ctx, "ephemeral", []string{"agent"}, nil, 10)
	if err != nil {
		t.Fatalf("Recall after TTL: %v", err)
	}
	if len(privAfterExpiry) != 0 {
		t.Fatalf("expected TTL-expired item to be pruned, got %+v", privAfterExpiry)
	}

	auditMu.Lock()
	gotAudits := slices.Clone(audits)
	auditMu.Unlock()
	wantAudits := []string{
		memory.AuditAuthorRemember,
		memory.AuditAuthorRemember,
		memory.AuditAuthorForget,
		memory.AuditAuthorRemember,
	}
	if !slices.Equal(gotAudits, wantAudits) {
		t.Fatalf("audit log = %v, want %v", gotAudits, wantAudits)
	}
}

func TestRouterPartitionByUserAndAmbiguousDefaultMount(t *testing.T) {
	t.Parallel()
	backend := memory.NewInMemory()
	userPrefs := memory.SharedMount("prod", "user-prefs", memory.AccessReadWrite)
	userPrefs.PartitionByUser = true

	router, err := memory.NewRouter(backend, []memory.MountTemplate{userPrefs})
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}

	// Default mount is ambiguous/absent when "agent" is not mounted rw.
	if _, err := router.Remember(memory.WithUserID(context.Background(), "alice"), "", []memory.CandidateFact{{Fact: "Prefers concise tables"}}); !errors.Is(err, memory.ErrAmbiguousWritableMount) {
		t.Fatalf("Remember(empty mount without agent): want ErrAmbiguousWritableMount, got %v", err)
	}

	// Missing user_id on context fails closed with ErrMissingUserIdentity.
	if _, err := router.Remember(context.Background(), "shared:user-prefs", []memory.CandidateFact{{Fact: "Prefers concise tables"}}); !errors.Is(err, memory.ErrMissingUserIdentity) {
		t.Fatalf("Remember(no user): want ErrMissingUserIdentity, got %v", err)
	}

	ctxAlice := memory.WithUserID(context.Background(), "alice")
	ctxBob := memory.WithUserID(context.Background(), "bob")
	if _, err := router.Remember(ctxAlice, "shared:user-prefs", []memory.CandidateFact{{Fact: "Prefers concise tables"}}); err != nil {
		t.Fatalf("Remember(alice): %v", err)
	}

	bobItems, _, err := router.Recall(ctxBob, "tables", nil, nil, 10)
	if err != nil {
		t.Fatalf("Recall(bob): %v", err)
	}
	if len(bobItems) != 0 {
		t.Fatalf("alice's user-partitioned memory leaked to bob: %+v", bobItems)
	}

	aliceItems, _, err := router.Recall(ctxAlice, "tables", nil, nil, 10)
	if err != nil || len(aliceItems) != 1 {
		t.Fatalf("Recall(alice) = %+v, err=%v; want 1 item", aliceItems, err)
	}
}

type flakyBackend struct {
	memory.Backend
	failPool string
}

func (f *flakyBackend) Retrieve(ctx context.Context, scope memory.ScopeMap, req memory.RetrieveRequest) ([]memory.Item, error) {
	if scope[memory.ScopeKeyPool] == f.failPool {
		return nil, errors.New("simulated IAM PermissionDenied on pool")
	}
	return f.Backend.Retrieve(ctx, scope, req)
}

func TestRouterPartialRecallWarningsVsStrict(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := memory.NewInMemory()
	flaky := &flakyBackend{Backend: base, failPool: "restricted"}

	mAgent := memory.AgentMount("prod", "triage", memory.AccessReadWrite)
	mRestricted := memory.SharedMount("prod", "restricted", memory.AccessReadOnly)

	nonStrict, err := memory.NewRouter(flaky, []memory.MountTemplate{mAgent, mRestricted})
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	if _, err := nonStrict.Remember(ctx, "agent", []memory.CandidateFact{{Fact: "Healthy private fact"}}); err != nil {
		t.Fatalf("Remember: %v", err)
	}

	items, warnings, err := nonStrict.Recall(ctx, "fact", nil, nil, 10)
	if err != nil {
		t.Fatalf("non-strict Recall should succeed with warning, got err=%v", err)
	}
	if len(items) != 1 || len(warnings) != 1 || warnings[0].Mount != "shared:restricted" {
		t.Fatalf("got items=%+v warnings=%+v, want 1 item + 1 warning on shared:restricted", items, warnings)
	}

	strict, err := memory.NewRouter(flaky, []memory.MountTemplate{mAgent, mRestricted}, memory.WithStrictRecall(true))
	if err != nil {
		t.Fatalf("NewRouter(strict): %v", err)
	}
	if _, _, err := strict.Recall(ctx, "fact", nil, nil, 10); err == nil {
		t.Fatalf("strict Recall must fail when any mount errors")
	}
}

func TestDeriveForSubagentAllSixPaths(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	backend := memory.NewInMemory()
	parent, err := memory.NewRouter(backend, []memory.MountTemplate{
		memory.AgentMount("prod", "coordinator", memory.AccessReadWrite),
		memory.SharedMount("prod", "fleet", memory.AccessReadWrite),
		memory.SharedMount("prod", "runbooks", memory.AccessReadOnly),
	})
	if err != nil {
		t.Fatalf("parent NewRouter: %v", err)
	}

	// Seed parent's private memory and fleet shared memory.
	if _, err := parent.Remember(ctx, "agent", []memory.CandidateFact{{Fact: "Coordinator private secret"}}); err != nil {
		t.Fatalf("seed parent private: %v", err)
	}
	if _, err := parent.Remember(ctx, "shared:fleet", []memory.CandidateFact{{Fact: "Shared fleet topology note"}}); err != nil {
		t.Fatalf("seed shared:fleet: %v", err)
	}

	// Path 1: Declarative chat/task specialist -> own rw agent scope, ro inherited shared.
	dbSub, err := parent.DeriveForSubagent(memory.SubagentDeriveConfig{
		SubagentName: "db-investigator",
		Mode:         "task",
	})
	if err != nil {
		t.Fatalf("Path 1 DeriveForSubagent: %v", err)
	}
	// Cannot see coordinator's private memory!
	subPriv, _, err := dbSub.Recall(ctx, "secret", []string{"agent"}, nil, 10)
	if err != nil || len(subPriv) != 0 {
		t.Fatalf("Path 1 subagent leaked parent private scope: %+v (err=%v)", subPriv, err)
	}
	// Can write its own private memory, but inherited shared:fleet is ro by default!
	if _, err := dbSub.Remember(ctx, "agent", []memory.CandidateFact{{Fact: "DB lock heuristic"}}); err != nil {
		t.Fatalf("Path 1 subagent private write failed: %v", err)
	}
	if _, err := dbSub.Remember(ctx, "shared:fleet", []memory.CandidateFact{{Fact: "Should be blocked"}}); !errors.Is(err, memory.ErrReadOnlyMount) {
		t.Fatalf("Path 1 inherited shared:fleet must default to ro, got %v", err)
	}

	// Path 2: Declarative capability="read_only" -> agent defaults to ro.
	roSub, err := parent.DeriveForSubagent(memory.SubagentDeriveConfig{
		SubagentName: "log-reader",
		Mode:         "task",
		Capability:   "read_only",
	})
	if err != nil {
		t.Fatalf("Path 2 DeriveForSubagent: %v", err)
	}
	if roSub.HasWritableMount() {
		t.Fatalf("Path 2 read_only subagent must have no writable mounts by default")
	}

	// Path 3: Declarative mode="single_turn" -> agent off, shared ro; explicit agent="rw" rejected.
	if _, err := parent.DeriveForSubagent(memory.SubagentDeriveConfig{
		SubagentName: "classifier",
		Mode:         "single_turn",
		AgentMode:    "rw",
	}); !errors.Is(err, memory.ErrInvalidMount) {
		t.Fatalf("Path 3 single_turn with agent=rw: want ErrInvalidMount, got %v", err)
	}
	stSub, err := parent.DeriveForSubagent(memory.SubagentDeriveConfig{
		SubagentName: "classifier",
		Mode:         "single_turn",
	})
	if err != nil {
		t.Fatalf("Path 3 DeriveForSubagent: %v", err)
	}
	for _, m := range stSub.Mounts() {
		if m.Name == memory.MountAgent || m.Mode != memory.AccessReadOnly {
			t.Fatalf("Path 3 unexpected mount on single_turn subagent: %+v", m)
		}
	}

	// Path 4 & 5a: Predefined subagent with InheritShared="rw" still clamps parent's ro "shared:runbooks" to ro,
	// and runtime NarrowAgentMode/NarrowSharedMode only narrows, never widens.
	predef, err := parent.DeriveForSubagent(memory.SubagentDeriveConfig{
		SubagentName:     "remediator",
		Mode:             "chat",
		InheritShared:    "rw",
		NarrowAgentMode:  "ro",
		NarrowSharedMode: "ro",
	})
	if err != nil {
		t.Fatalf("Path 5a DeriveForSubagent: %v", err)
	}
	if predef.HasWritableMount() {
		t.Fatalf("Path 5a runtime narrowing to ro left a writable mount: %+v", predef.Mounts())
	}

	// Attempt to widen ro -> rw via NarrowAgentMode must stay ro.
	notWidened, err := parent.DeriveForSubagent(memory.SubagentDeriveConfig{
		SubagentName:    "reader",
		Mode:            "chat",
		Capability:      "read_only",
		NarrowAgentMode: "rw",
	})
	if err != nil {
		t.Fatalf("Path 5a widen check: %v", err)
	}
	if notWidened.HasWritableMount() {
		t.Fatalf("NarrowAgentMode='rw' widened a read_only subagent")
	}

	// Path 5b: Ad-hoc inline spawn_agent -> forces agent="off" and shared="ro" even if caller passes rw/override!
	adhoc, err := parent.DeriveForSubagent(memory.SubagentDeriveConfig{
		AdHoc:           true,
		SubagentName:    "db-investigator", // spoof attempt!
		AgentMode:       "rw",
		AgentIDOverride: "db-investigator",
		InheritShared:   "rw",
		ExtraShared:     []memory.MountTemplate{memory.SharedMount("prod", "extra", memory.AccessReadWrite)},
	})
	if err != nil {
		t.Fatalf("Path 5b DeriveForSubagent: %v", err)
	}
	if adhoc.HasWritableMount() {
		t.Fatalf("Path 5b ad-hoc subagent must never have writable mounts: %+v", adhoc.Mounts())
	}
	for _, m := range adhoc.Mounts() {
		if m.Name == memory.MountAgent || m.Name == "shared:extra" {
			t.Fatalf("Path 5b ad-hoc subagent mounted forbidden scope: %+v", m)
		}
	}

	// Path 6: Internal RunSubtask -> returns nil router.
	subtaskRouter, err := parent.DeriveForSubagent(memory.SubagentDeriveConfig{
		IsSubtask:    true,
		SubagentName: "digester",
	})
	if err != nil || subtaskRouter != nil {
		t.Fatalf("Path 6 IsSubtask: want (nil, nil), got (%v, %v)", subtaskRouter, err)
	}
}

func TestParseRefAndOpenRouter(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	mbFull, err := memory.ParseRef("memorybank://projects/my-proj/locations/us-central1/reasoningEngines/98765")
	if err != nil {
		t.Fatalf("ParseRef(full): %v", err)
	}
	if mbFull.Project != "my-proj" || mbFull.Location != "us-central1" || mbFull.EngineID != "98765" {
		t.Fatalf("unexpected mbFull fields: %+v", mbFull)
	}

	mbShort, err := memory.ParseRef("memorybank://my-proj/us-central1/98765")
	if err != nil {
		t.Fatalf("ParseRef(compact): %v", err)
	}
	if mbShort.Resource != mbFull.Resource {
		t.Fatalf("compact Resource %q != full Resource %q", mbShort.Resource, mbFull.Resource)
	}

	sq, err := memory.ParseRef("sqlite:///tmp/core-agent-mem.db")
	if err != nil || sq.Resource != "/tmp/core-agent-mem.db" {
		t.Fatalf("ParseRef(sqlite) = %+v, err=%v", sq, err)
	}

	for _, bad := range []string{
		"",
		"  memorybank://a/b/c  ",
		"memorybank://projects/p/locations/../reasoningEngines/1",
		"memorybank://projects/p/only-three",
		"memorybank://only-two/segments",
		"sqlite:///",
		"inmemory://nested/path",
	} {
		if _, err := memory.ParseRef(bad); !errors.Is(err, memory.ErrInvalidRef) {
			t.Errorf("ParseRef(%q): want ErrInvalidRef, got %v", bad, err)
		}
	}

	// Named inmemory:// URI shares state across two OpenRouter calls in the same process.
	spec1 := memory.BindingSpec{
		Ref:       "inmemory://shared-smoke-bus",
		Namespace: "smoke",
		Mounts:    []memory.MountTemplate{memory.SharedMount("smoke", "team", memory.AccessReadWrite)},
	}
	r1, b1, err := memory.OpenRouter(ctx, spec1)
	if err != nil {
		t.Fatalf("OpenRouter 1: %v", err)
	}
	defer func() { _ = b1.Close() }()

	if _, err := r1.Remember(ctx, "shared:team", []memory.CandidateFact{{Fact: "Stateless worker 1 finding"}}); err != nil {
		t.Fatalf("r1.Remember: %v", err)
	}
	_ = b1.Close() // Closing handle 1 must not wipe the shared named bus for handle 2.

	r2, b2, err := memory.OpenRouter(ctx, spec1)
	if err != nil {
		t.Fatalf("OpenRouter 2: %v", err)
	}
	defer func() { _ = b2.Close() }()

	got, _, err := r2.Recall(ctx, "worker 1", nil, nil, 5)
	if err != nil || len(got) != 1 {
		t.Fatalf("r2.Recall across shared inmemory URI = %+v, err=%v; want 1 item", got, err)
	}
}

type stubEvents []*session.Event

func (e stubEvents) All() iter.Seq[*session.Event] {
	return func(yield func(*session.Event) bool) {
		for _, ev := range e {
			if !yield(ev) {
				return
			}
		}
	}
}
func (e stubEvents) Len() int                { return len(e) }
func (e stubEvents) At(i int) *session.Event { return e[i] }

type stubSession struct {
	id     string
	userID string
	events stubEvents
}

func (s *stubSession) ID() string                { return s.id }
func (s *stubSession) AppName() string           { return "core-agent" }
func (s *stubSession) UserID() string            { return s.userID }
func (s *stubSession) State() session.State      { return nil }
func (s *stubSession) Events() session.Events    { return s.events }
func (s *stubSession) LastUpdateTime() time.Time { return time.Time{} }

func TestConsolidateSessionAndADKServiceBridge(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	backend := memory.NewInMemory()

	priv := memory.AgentMount("prod", "assistant", memory.AccessReadWrite)
	priv.PartitionByUser = true
	router, err := memory.NewRouter(backend, []memory.MountTemplate{priv})
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}

	t0 := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	sess := &stubSession{
		id:     "sess-42",
		userID: "carol",
		events: stubEvents{
			{
				Timestamp: t0,
				Author:    "user",
				LLMResponse: model.LLMResponse{
					Partial: true, // Partial streaming chunks must be skipped.
					Content: genai.NewContentFromText("partial chunk", genai.RoleUser),
				},
			},
			{
				Timestamp: t0.Add(time.Second),
				Author:    "memory/remember", // Internal audit events must be skipped.
				LLMResponse: model.LLMResponse{
					Content: genai.NewContentFromText("audit payload", genai.RoleUser),
				},
			},
			{
				Timestamp: t0.Add(2 * time.Second),
				Author:    "user",
				LLMResponse: model.LLMResponse{
					CustomMetadata: map[string]any{"core_agent_synthetic_repair": true}, // Synthetic repair skipped.
					Content:        genai.NewContentFromText("repaired", genai.RoleUser),
				},
			},
			{
				Timestamp: t0.Add(3 * time.Second),
				Author:    "user",
				LLMResponse: model.LLMResponse{
					Content: genai.NewContentFromText("On-call handoff channel is #sre-handoff.", genai.RoleUser),
				},
			},
			{
				Timestamp: t0.Add(4 * time.Second),
				Author:    "assistant",
				LLMResponse: model.LLMResponse{
					Content: &genai.Content{
						Role: genai.RoleModel,
						Parts: []*genai.Part{
							{Text: "internal chain of thought", Thought: true}, // Thought parts skipped!
							{Text: "Noted #sre-handoff as the handoff channel."},
						},
					},
				},
			},
		},
	}

	adkSvc := memory.AsADKService(router, "")
	if err := adkSvc.AddSessionToMemory(ctx, sess); err != nil {
		t.Fatalf("AddSessionToMemory: %v", err)
	}

	resp, err := adkSvc.SearchMemory(ctx, &adkmemory.SearchRequest{
		Query:   "handoff channel",
		UserID:  "carol",
		AppName: "core-agent",
	})
	if err != nil {
		t.Fatalf("SearchMemory: %v", err)
	}
	if len(resp.Memories) != 2 {
		t.Fatalf("SearchMemory returned %d entries, want 2: %+v", len(resp.Memories), resp.Memories)
	}
	for _, e := range resp.Memories {
		for _, p := range e.Content.Parts {
			if p.Text == "internal chain of thought" || p.Text == "partial chunk" {
				t.Fatalf("filtered event leaked into consolidated memory: %q", p.Text)
			}
		}
	}

	// Disabled consolidation ("off") on bridge refuses AddSessionToMemory.
	offSvc := memory.AsADKService(router, "off")
	if err := offSvc.AddSessionToMemory(ctx, sess); !errors.Is(err, memory.ErrReadOnlyMount) {
		t.Fatalf("AddSessionToMemory(off): want ErrReadOnlyMount, got %v", err)
	}
}

func TestDeriveForSubagentExtraSharedAndMultiHopCeilings(t *testing.T) {
	t.Parallel()
	ctxAlice := memory.WithUserID(context.Background(), "alice")
	backend := memory.NewInMemory()

	userPool := memory.SharedMount("prod", "team", memory.AccessReadOnly)
	userPool.PartitionByUser = true

	root, err := memory.NewRouter(backend, []memory.MountTemplate{
		memory.AgentMount("prod", "coordinator", memory.AccessReadWrite),
		userPool,
	})
	if err != nil {
		t.Fatalf("root NewRouter: %v", err)
	}

	// 1. Passing AgentMount in ExtraShared MUST fail closed (cannot spoof parent agent_id).
	if _, err := root.DeriveForSubagent(memory.SubagentDeriveConfig{
		SubagentName: "helper",
		Mode:         "task",
		AgentMode:    "off",
		ExtraShared:  []memory.MountTemplate{memory.AgentMount("prod", "coordinator", memory.AccessReadWrite)},
	}); !errors.Is(err, memory.ErrInvalidMount) {
		t.Fatalf("ExtraShared with AgentMount: want ErrInvalidMount, got %v", err)
	}

	// 2. Multi-hop ceiling: hop1 drops shared mounts (InheritShared="off"), then hop2
	// tries to re-add shared:team as rw with PartitionByUser=false. Both the ancestor's
	// ro ceiling AND PartitionByUser=true must stick across hops.
	hop1, err := root.DeriveForSubagent(memory.SubagentDeriveConfig{
		SubagentName:  "hop1",
		Mode:          "task",
		InheritShared: "off",
	})
	if err != nil {
		t.Fatalf("hop1: %v", err)
	}
	hop2, err := hop1.DeriveForSubagent(memory.SubagentDeriveConfig{
		SubagentName: "hop2",
		Mode:         "task",
		ExtraShared:  []memory.MountTemplate{memory.SharedMount("prod", "team", memory.AccessReadWrite)},
	})
	if err != nil {
		t.Fatalf("hop2: %v", err)
	}
	if _, err := hop2.Remember(ctxAlice, "shared:team", []memory.CandidateFact{{Fact: "escalated"}}); !errors.Is(err, memory.ErrReadOnlyMount) {
		t.Fatalf("hop2 escalated ancestor ro pool to rw: err=%v", err)
	}
	// Missing user_id on context must still fail closed (PartitionByUser preserved).
	if _, _, err := hop2.Recall(context.Background(), "anything", []string{"shared:team"}, nil, 5); err == nil {
		t.Fatalf("hop2 dropped PartitionByUser=true from ancestor shared:team")
	}

	// 3. Runtime NarrowSharedMode="off" on hopA must prevent hopB from reopening any shared pool.
	hopA, err := root.DeriveForSubagent(memory.SubagentDeriveConfig{
		SubagentName:     "hopA",
		Mode:             "task",
		NarrowSharedMode: "off",
	})
	if err != nil {
		t.Fatalf("hopA: %v", err)
	}
	hopB, err := hopA.DeriveForSubagent(memory.SubagentDeriveConfig{
		SubagentName: "hopB",
		Mode:         "task",
		AgentMode:    "off",
		ExtraShared:  []memory.MountTemplate{memory.SharedMount("prod", "brand-new", memory.AccessReadWrite)},
	})
	if err != nil {
		t.Fatalf("hopB: %v", err)
	}
	if hopB != nil {
		t.Fatalf("hopB reopened shared mount despite ancestor NarrowSharedMode=off: %+v", hopB.Mounts())
	}
}

type unscoredOrderedBackend struct {
	memory.Backend
	items []memory.Item
}

func (u *unscoredOrderedBackend) Retrieve(_ context.Context, _ memory.ScopeMap, _ memory.RetrieveRequest) ([]memory.Item, error) {
	out := make([]memory.Item, len(u.items))
	for i, it := range u.items {
		out[i] = it.Clone()
	}
	return out, nil
}

func TestRouterEdgeCasesFromAdversarialReview(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// 1. OpenRouter preserves BindingSpec.Namespace for DeriveForSubagent even when
	// parent mounts use custom ScopeMap keys without "namespace".
	customMount := memory.MountTemplate{
		Name:      "shared:incidents",
		Mode:      memory.AccessReadOnly,
		BaseScope: memory.ScopeMap{"app_name": "sre-portal"},
	}
	parentFromSpec, b, err := memory.OpenRouter(ctx, memory.BindingSpec{
		Ref:       "inmemory://ns-preserve-test",
		Namespace: "sre-prod",
		Mounts:    []memory.MountTemplate{customMount},
	})
	if err != nil {
		t.Fatalf("OpenRouter: %v", err)
	}
	defer func() { _ = b.Close() }()

	derived, err := parentFromSpec.DeriveForSubagent(memory.SubagentDeriveConfig{
		SubagentName: "triage",
		Mode:         "task",
	})
	if err != nil {
		t.Fatalf("DeriveForSubagent: %v", err)
	}
	var gotAgentNS string
	for _, m := range derived.Mounts() {
		if m.Name == memory.MountAgent {
			gotAgentNS = m.BaseScope[memory.ScopeKeyNamespace]
		}
	}
	if gotAgentNS != "sre-prod" {
		t.Fatalf("derived agent mount namespace = %q, want %q", gotAgentNS, "sre-prod")
	}

	// 2. WithUserResolver fallback to session.UserID() / SearchRequest.UserID,
	// blank-only topics filter, non-LLM empty-Role audit event skip, and
	// AsADKService metadata provenance protection.
	store := memory.NewInMemory()
	partMount := memory.AgentMount("prod", "triage", memory.AccessReadWrite)
	partMount.PartitionByUser = true
	customResolverRouter, err := memory.NewRouter(
		store,
		[]memory.MountTemplate{partMount},
		memory.WithUserResolver(func(context.Context) string { return "" }), // Auth middleware absent on background/ADK ctx.
	)
	if err != nil {
		t.Fatalf("NewRouter(custom resolver): %v", err)
	}

	sess := &stubSession{
		id:     "sess-audit",
		userID: "carol",
		events: stubEvents{
			{
				Author: "core-agent/autonomous",
				LLMResponse: model.LLMResponse{
					// Empty Role marks a non-LLM harness audit event in ADK v2.
					Content: &genai.Content{Parts: []*genai.Part{{Text: "autonomous budget checkpoint"}}},
				},
			},
			{
				Author: "user",
				LLMResponse: model.LLMResponse{
					Content: genai.NewContentFromText("Carol prefers UTC timestamps.", genai.RoleUser),
				},
			},
		},
	}
	if _, err := customResolverRouter.ConsolidateSession(ctx, "agent", sess, nil); err != nil {
		t.Fatalf("ConsolidateSession with custom resolver + sess.UserID(): %v", err)
	}

	// Inject a metadata-spoofing attempt and verify blank-only topic filter + ADK provenance.
	if _, err := customResolverRouter.Remember(memory.WithUserID(ctx, "carol"), "agent", []memory.CandidateFact{{
		Fact:     "Spoof attempt fact",
		Metadata: map[string]string{"mount": "shared:spoofed", "read_only": "true"},
	}}); err != nil {
		t.Fatalf("Remember(spoof): %v", err)
	}

	recalled, _, err := customResolverRouter.Recall(memory.WithUserID(ctx, "carol"), "UTC", nil, []string{"", "   "}, 10)
	if err != nil || len(recalled) == 0 {
		t.Fatalf("Recall with blank-only topics dropped items: items=%+v err=%v", recalled, err)
	}
	for _, it := range recalled {
		if strings.Contains(it.Fact, "autonomous budget checkpoint") {
			t.Fatalf("non-LLM audit marker with empty Role leaked into memory: %+v", it)
		}
	}

	adkSvc := memory.AsADKService(customResolverRouter, "")
	sResp, err := adkSvc.SearchMemory(ctx, &adkmemory.SearchRequest{UserID: "carol", Query: "Spoof"})
	if err != nil || len(sResp.Memories) == 0 {
		t.Fatalf("AsADKService.SearchMemory with custom resolver + req.UserID: resp=%+v err=%v", sResp, err)
	}
	gotMeta := sResp.Memories[0].CustomMetadata
	if gotMeta["mount"] != "agent" || gotMeta["read_only"] != false {
		t.Fatalf("Item.Metadata overwrote Router provenance in CustomMetadata: %+v", gotMeta)
	}

	// 3. Unscored backends (Distance == nil) keep their relevance order instead of
	// scrambling by UpdatedAt.
	tOld := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tNew := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	unscored := &unscoredOrderedBackend{
		Backend: store,
		items: []memory.Item{
			{ID: "top-bm25-hit", Fact: "Exact match", UpdatedAt: tOld},
			{ID: "weak-bm25-hit", Fact: "Weak match", UpdatedAt: tNew},
		},
	}
	unscoredRouter, err := memory.NewRouter(unscored, []memory.MountTemplate{
		memory.SharedMount("prod", "kb", memory.AccessReadOnly),
	})
	if err != nil {
		t.Fatalf("NewRouter(unscored): %v", err)
	}
	top1, _, err := unscoredRouter.Recall(ctx, "exact", nil, nil, 1)
	if err != nil || len(top1) != 1 || top1[0].ID != "top-bm25-hit" {
		t.Fatalf("Recall(topK=1) scrambled unscored backend relevance: got %+v (err=%v)", top1, err)
	}
}

func TestRouterWithChainsAuditAndLeavesOriginalUnchanged(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	var mu sync.Mutex
	var calls []string
	rec := func(tag string) memory.AuditRecorder {
		return func(_ context.Context, author string, _ map[string]any) error {
			mu.Lock()
			defer mu.Unlock()
			calls = append(calls, tag+":"+author)
			return nil
		}
	}
	base, err := memory.NewRouter(memory.NewInMemory(),
		[]memory.MountTemplate{memory.AgentMount("prod", "a", memory.AccessReadWrite)},
		memory.WithAuditRecorder(rec("host")))
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	derived := base.With(memory.AppendAuditRecorder(rec("agent")), memory.AppendAuditRecorder(nil))
	if _, err := derived.Remember(ctx, "agent", []memory.CandidateFact{{Fact: "x"}}); err != nil {
		t.Fatalf("derived.Remember: %v", err)
	}
	if _, err := base.Remember(ctx, "agent", []memory.CandidateFact{{Fact: "y"}}); err != nil {
		t.Fatalf("base.Remember: %v", err)
	}
	want := []string{"host:memory/remember", "agent:memory/remember", "host:memory/remember"}
	if !slices.Equal(calls, want) {
		t.Fatalf("audit calls = %v, want %v (host recorder kept, agent's chained only on the copy)", calls, want)
	}
	if (*memory.Router)(nil).With() != nil {
		t.Fatal("nil Router With returned non-nil")
	}
}

func TestStripContextBlocks(t *testing.T) {
	t.Parallel()
	block := memory.FormatContext([]memory.Item{{ID: "m1", Mount: "agent", Fact: "recalled"}})
	for in, want := range map[string]string{
		block + "\n\nhow do I restart?":                   "\n\nhow do I restart?",
		"before " + block + " middle " + block + " after": "before  middle  after",
		"no block here": "no block here",
		"keep this " + memory.ContextOpenTag + " unterminated recalled text": "keep this ",
	} {
		if got := memory.StripContextBlocks(in); got != want {
			t.Errorf("StripContextBlocks(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAuditRecorderErrorWrapsErrAuditFailed(t *testing.T) {
	t.Parallel()
	boom := errors.New("audit store down")
	r, err := memory.NewRouter(memory.NewInMemory(),
		[]memory.MountTemplate{memory.AgentMount("prod", "a", memory.AccessReadWrite)},
		memory.WithAuditRecorder(func(context.Context, string, map[string]any) error { return boom }))
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	deltas, err := r.Remember(context.Background(), "agent", []memory.CandidateFact{{Fact: "x"}})
	if !errors.Is(err, memory.ErrAuditFailed) || !errors.Is(err, boom) || len(deltas) != 1 {
		t.Fatalf("Remember with failing audit: deltas=%v err=%v, want the write's deltas and an ErrAuditFailed wrapping the cause", deltas, err)
	}
}
