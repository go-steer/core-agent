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

package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v3/pkg/agent/internal/subsession"
	"github.com/go-steer/core-agent/v3/pkg/auth"
	"github.com/go-steer/core-agent/v3/pkg/permissions"
)

func newOutOfBandAuditTool(t *testing.T, name string, svc func() session.Service, kinds ...string) tool.Tool {
	t.Helper()
	return newOutOfBandAuditToolWithIDs(t, name, svc, nil, kinds...)
}

func newOutOfBandAuditToolWithIDs(t *testing.T, name string, svc func() session.Service, ids []string, kinds ...string) tool.Tool {
	t.Helper()
	type emptyArgs struct{}
	tl, err := functiontool.New(
		functiontool.Config{Name: name, Description: "Emits out-of-band session audit events."},
		func(ctx adkagent.Context, _ emptyArgs) (map[string]any, error) {
			for i, k := range kinds {
				ev := session.NewEvent(ctx, "vcs")
				if i < len(ids) && ids[i] != "" {
					ev.ID = ids[i]
				}
				ev.Author = "system"
				ev.CustomMetadata = map[string]any{"core_agent_event_kind": k}
				if err := EnqueueOrAppendSessionEvent(ctx, svc(), ctx.AppName(), ctx.UserID(), ctx.SessionID(), ev); err != nil {
					return nil, err
				}
			}
			return map[string]any{"ok": true}, nil
		},
	)
	if err != nil {
		t.Fatalf("functiontool.New(%s): %v", name, err)
	}
	return tl
}

func TestEnqueueOrAppendSessionEvent_MainAgentInFlightSQLiteTurn(t *testing.T) {
	t.Parallel()
	h, cleanup := openTestEventLog(t)
	defer cleanup()

	var ref atomic.Pointer[Agent]
	var midTurnWrites atomic.Int32
	origSvc := h.Service
	h.Service = &appendHookService{
		Service: origSvc,
		onAppend: func(_ context.Context, ev *session.Event) {
			if ev != nil && ev.Author == "system" && ref.Load() != nil && ref.Load().turnInFlight() {
				midTurnWrites.Add(1)
			}
		},
	}

	// Pin descending IDs ("ffffffff..." then "00000000...") so ADK's
	// "ORDER BY timestamp DESC, id DESC" deterministically reverses FIFO
	// order whenever two deferred events share an identical timestamp.
	auditTool := newOutOfBandAuditToolWithIDs(
		t,
		"emit_vcs_audit",
		func() session.Service { return h.Service },
		[]string{"ffffffff-ffff-4fff-8fff-ffffffffffff", "00000000-0000-4000-8000-000000000001"},
		"vcs.published",
		"vcs.proposed",
	)
	llm := &callWithArgsLLM{tool: "emit_vcs_audit", args: map[string]any{}}
	a, err := New(llm,
		WithEventLog(h),
		WithSession("u-main", "s-main-1571"),
		WithGate(permissions.New(permissions.Options{Mode: permissions.ModeYolo})),
		WithTools([]tool.Tool{auditTool}),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ref.Store(a)

	for _, runErr := range a.Run(context.Background(), "propose fix") {
		if runErr != nil {
			t.Fatalf("Run failed (stale session regression): %v", runErr)
		}
	}
	if n := midTurnWrites.Load(); n != 0 {
		t.Fatalf("mid-turn out-of-band AppendEvent count = %d, want 0", n)
	}

	resp, err := h.Service.Get(context.Background(), &session.GetRequest{
		AppName:   DefaultAppName,
		UserID:    "u-main",
		SessionID: "s-main-1571",
	})
	if err != nil {
		t.Fatalf("Service.Get: %v", err)
	}
	var gotKinds []string
	for ev := range resp.Session.Events().All() {
		if k, ok := ev.CustomMetadata["core_agent_event_kind"].(string); ok {
			gotKinds = append(gotKinds, k)
		}
	}
	if strings.Join(gotKinds, ",") != "vcs.published,vcs.proposed" {
		t.Fatalf("persisted kinds = %v, want [vcs.published vcs.proposed]", gotKinds)
	}
}

func TestEnqueueOrAppendSessionEvent_SyncSubagentInFlightSQLiteTurn(t *testing.T) {
	t.Parallel()
	h, cleanup := openTestEventLog(t)
	defer cleanup()

	auditTool := newOutOfBandAuditTool(t, "sub_emit_vcs", func() session.Service { return h.Service }, "vcs.proposed")
	childLLM := &callWithArgsLLM{tool: "sub_emit_vcs", args: map[string]any{}}
	child, err := New(childLLM,
		WithName("proposer"),
		WithDescription("Proposes repository changes."),
		WithEventLog(h),
		WithSession("u-sub", "s-child-unused"),
		WithGate(permissions.New(permissions.Options{Mode: permissions.ModeYolo})),
		WithTools([]tool.Tool{auditTool}),
	)
	if err != nil {
		t.Fatalf("New child: %v", err)
	}

	parentLLM := &callWithArgsLLM{
		tool: "proposer",
		args: map[string]any{"request": "open a pull request"},
	}
	parent, err := New(parentLLM,
		WithName("coordinator"),
		WithEventLog(h),
		WithSession("u-sub", "s-parent-1571"),
		WithGate(permissions.New(permissions.Options{Mode: permissions.ModeYolo})),
		WithSubagents([]*Agent{child}),
	)
	if err != nil {
		t.Fatalf("New parent: %v", err)
	}

	for _, runErr := range parent.Run(context.Background(), "fix deployment") {
		if runErr != nil {
			t.Fatalf("parent.Run failed (subagent stale session regression): %v", runErr)
		}
	}

	listResp, err := h.Service.List(context.Background(), &session.ListRequest{
		AppName: DefaultAppName,
		UserID:  "u-sub",
	})
	if err != nil {
		t.Fatalf("Service.List: %v", err)
	}
	var foundSubAudit bool
	for _, s := range listResp.Sessions {
		getResp, gerr := h.Service.Get(context.Background(), &session.GetRequest{
			AppName:   DefaultAppName,
			UserID:    "u-sub",
			SessionID: s.ID(),
		})
		if gerr != nil {
			t.Fatalf("Service.Get(%s): %v", s.ID(), gerr)
		}
		if s.ID() == "s-parent-1571" {
			for ev := range getResp.Session.Events().All() {
				if ev.Content == nil {
					continue
				}
				for _, p := range ev.Content.Parts {
					if p.FunctionResponse != nil {
						if errVal, hasErr := p.FunctionResponse.Response["error"]; hasErr && errVal != nil && errVal != "" {
							t.Fatalf("subagent tool returned error (stale session regression): %v", errVal)
						}
					}
				}
			}
			continue
		}
		for ev := range getResp.Session.Events().All() {
			if ev.CustomMetadata["core_agent_event_kind"] == "vcs.proposed" {
				foundSubAudit = true
				if ev.Branch == "" {
					t.Errorf("subagent audit event missing Branch metadata")
				}
			}
		}
	}
	if !foundSubAudit {
		t.Fatalf("expected vcs.proposed audit event in subagent child session")
	}
}

func TestEnqueueOrAppendSessionEvent_ServiceWithoutEventLogNestedUnderParentAgent(t *testing.T) {
	t.Parallel()
	h, cleanup := openTestEventLog(t)
	defer cleanup()

	var capturedCaller, capturedProxy string
	hookSvc := &appendHookService{
		Service: h.Service,
		onAppend: func(ctx context.Context, ev *session.Event) {
			if ev != nil && ev.CustomMetadata["core_agent_event_kind"] == "vcs.published" {
				if c, ok := auth.CallerFromContext(ctx); ok {
					capturedCaller = c.Identity
				}
				if p, ok := auth.ProxyByFromContext(ctx); ok {
					capturedProxy = p
				}
			}
		},
	}

	outerParent, err := New(&callWithArgsLLM{},
		WithEventLog(h),
		WithSession("u-svconly", "s-outer-parent"),
	)
	if err != nil {
		t.Fatalf("New outerParent: %v", err)
	}

	wrapped := &subsession.BranchInjectingService{
		Inner:  hookSvc,
		Branch: "bg.proposer",
	}
	auditTool := newOutOfBandAuditTool(t, "emit_svc_only", func() session.Service { return hookSvc }, "vcs.published")
	llm := &callWithArgsLLM{tool: "emit_svc_only", args: map[string]any{}}
	a, err := New(llm,
		WithSessionService(wrapped),
		WithSession("u-svconly", "s-svconly-1571"),
		WithGate(permissions.New(permissions.Options{Mode: permissions.ModeYolo})),
		WithTools([]tool.Tool{auditTool}),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Simulate background.Manager.launch stamping outerParent via WithRunningAgent
	// onto the context handed to the child Agent.Run.
	nestedCtx := WithRunningAgent(context.Background(), outerParent)
	nestedCtx = auth.WithProxyBy(auth.WithCaller(nestedCtx, auth.Caller{Identity: "carol@example.com"}), "sa:bg-bridge")
	for _, runErr := range a.Run(nestedCtx, "publish branch") {
		if runErr != nil {
			t.Fatalf("Run failed (nested background stale session regression): %v", runErr)
		}
	}

	if capturedCaller != "carol@example.com" || capturedProxy != "sa:bg-bridge" {
		t.Fatalf("captured caller=%q proxy=%q, want carol@example.com / sa:bg-bridge", capturedCaller, capturedProxy)
	}

	resp, err := h.Service.Get(context.Background(), &session.GetRequest{
		AppName:   DefaultAppName,
		UserID:    "u-svconly",
		SessionID: "s-svconly-1571",
	})
	if err != nil {
		t.Fatalf("Service.Get: %v", err)
	}
	var found bool
	for ev := range resp.Session.Events().All() {
		if ev.CustomMetadata["core_agent_event_kind"] == "vcs.published" {
			found = true
			if ev.Branch != "bg.proposer" {
				t.Errorf("audit event Branch = %q, want %q", ev.Branch, "bg.proposer")
			}
		}
	}
	if !found {
		t.Fatalf("expected vcs.published row persisted via drainServiceEvents")
	}
}

func TestEnqueueOrAppendSessionEvent_RunSubtaskInFlightSQLiteTurn(t *testing.T) {
	t.Parallel()
	h, cleanup := openTestEventLog(t)
	defer cleanup()

	auditTool := newOutOfBandAuditTool(t, "subtask_emit_vcs", func() session.Service { return h.Service }, "vcs.published")
	subLLM := &callWithArgsLLM{tool: "subtask_emit_vcs", args: map[string]any{}}
	parent, err := New(subLLM,
		WithEventLog(h),
		WithSession("u-subtask", "s-subtask-parent"),
		WithGate(permissions.New(permissions.Options{Mode: permissions.ModeYolo})),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res, err := parent.RunSubtask(WithRunningAgent(context.Background(), parent), SubtaskSpec{
		Name:         "gitops",
		SystemPrompt: "Emit audit event.",
		UserMessage:  "run",
		Model:        subLLM,
		Tools:        []tool.Tool{auditTool},
	})
	if err != nil {
		t.Fatalf("RunSubtask failed (stale session regression): %v", err)
	}
	_ = res

	listResp, err := h.Service.List(context.Background(), &session.ListRequest{
		AppName: DefaultAppName,
		UserID:  "u-subtask",
	})
	if err != nil {
		t.Fatalf("Service.List: %v", err)
	}
	var found bool
	for _, s := range listResp.Sessions {
		getResp, gerr := h.Service.Get(context.Background(), &session.GetRequest{
			AppName:   DefaultAppName,
			UserID:    "u-subtask",
			SessionID: s.ID(),
		})
		if gerr != nil {
			t.Fatalf("Service.Get(%s): %v", s.ID(), gerr)
		}
		for ev := range getResp.Session.Events().All() {
			if ev.CustomMetadata["core_agent_event_kind"] == "vcs.published" {
				found = true
				if ev.Branch == "" {
					t.Errorf("subtask audit event missing Branch metadata")
				}
			}
		}
	}
	if !found {
		t.Fatalf("expected vcs.published row in subtask session")
	}
}

func TestEnqueueOrAppendSessionEvent_RunWithContentsInFlightSQLiteTurn(t *testing.T) {
	t.Parallel()
	h, cleanup := openTestEventLog(t)
	defer cleanup()

	var flushedKinds []string
	var capturedCaller string
	var mu sync.Mutex
	origSvc := h.Service
	h.Service = &appendHookService{
		Service: origSvc,
		onAppend: func(ctx context.Context, ev *session.Event) {
			if ev != nil && ev.CustomMetadata["core_agent_event_kind"] != nil {
				mu.Lock()
				flushedKinds = append(flushedKinds, fmt.Sprint(ev.CustomMetadata["core_agent_event_kind"]))
				if c, ok := auth.CallerFromContext(ctx); ok {
					capturedCaller = c.Identity
				}
				mu.Unlock()
			}
		},
	}

	auditTool := newOutOfBandAuditTool(t, "rwc_emit_vcs", func() session.Service { return h.Service }, "vcs.published", "vcs.proposed")
	llm := &callWithArgsLLM{tool: "rwc_emit_vcs", args: map[string]any{}}
	a, err := New(llm,
		WithEventLog(h),
		WithSession("u-rwc", "s-rwc-base"),
		WithGate(permissions.New(permissions.Options{Mode: permissions.ModeYolo})),
		WithTools([]tool.Tool{auditTool}),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	authedCtx := auth.WithCaller(context.Background(), auth.Caller{Identity: "ax-runner@example.com"})
	contents := []*genai.Content{genai.NewContentFromText("propose via rwc", genai.RoleUser)}
	for _, runErr := range a.RunWithContents(authedCtx, contents) {
		if runErr != nil {
			t.Fatalf("RunWithContents failed (stale session regression): %v", runErr)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if strings.Join(flushedKinds, ",") != "vcs.published,vcs.proposed" {
		t.Fatalf("flushed kinds = %v, want [vcs.published vcs.proposed]", flushedKinds)
	}
	if capturedCaller != "ax-runner@example.com" {
		t.Fatalf("captured caller = %q, want ax-runner@example.com", capturedCaller)
	}
}

func TestWithSubagentSessionEvents_ConcurrentFlushAndLateCallers(t *testing.T) {
	t.Parallel()
	h, cleanup := openTestEventLog(t)
	defer cleanup()

	ctx := context.Background()
	if _, err := h.Service.Create(ctx, &session.CreateRequest{
		AppName:   DefaultAppName,
		UserID:    "u-race",
		SessionID: "s-race",
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	subCtx, flush := withSubagentSessionEvents(ctx, "proposer", DefaultAppName, "u-race", "s-race")
	deferrer := subagentSessionEventsFrom(subCtx)
	for i := 0; i < 4; i++ {
		ev := session.NewEvent(ctx, "vcs")
		ev.Author = "system"
		ev.CustomMetadata = map[string]any{"idx": i}
		if !deferrer(ctx, h.Service, DefaultAppName, "u-race", "s-race", ev) {
			t.Fatalf("deferrer returned false for matching session")
		}
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		flush()
	}()
	for i := 4; i < 12; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			ev := session.NewEvent(ctx, "vcs")
			ev.Author = "system"
			ev.CustomMetadata = map[string]any{"idx": i}
			deferrer(ctx, h.Service, DefaultAppName, "u-race", "s-race", ev)
		}()
	}
	wg.Wait()

	getResp, err := h.Service.Get(ctx, &session.GetRequest{
		AppName:   DefaultAppName,
		UserID:    "u-race",
		SessionID: "s-race",
	})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got := getResp.Session.Events().Len(); got != 12 {
		t.Fatalf("persisted events = %d, want 12 (no dropped rows under concurrent flush + late callers)", got)
	}
}

func TestAdvanceEventTimestampAfter(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 11, 17, 12, 0, 0, 500_200, time.UTC) // 500us + 200ns

	evEarlier := &session.Event{Timestamp: base.Add(-10 * time.Millisecond)}
	advanceEventTimestampAfter(evEarlier, base)
	if !evEarlier.Timestamp.Equal(base.Add(time.Microsecond)) {
		t.Fatalf("earlier timestamp = %v, want %v", evEarlier.Timestamp, base.Add(time.Microsecond))
	}

	// Sub-microsecond nanosecond difference with identical UnixMicro() must still advance by 1us.
	evSameMicro := &session.Event{Timestamp: base.Add(100 * time.Nanosecond)}
	advanceEventTimestampAfter(evSameMicro, base)
	if evSameMicro.Timestamp.UnixMicro() <= base.UnixMicro() {
		t.Fatalf("same-micro timestamp UnixMicro = %d, want > %d", evSameMicro.Timestamp.UnixMicro(), base.UnixMicro())
	}

	evLater := &session.Event{Timestamp: base.Add(5 * time.Microsecond)}
	advanceEventTimestampAfter(evLater, base)
	if !evLater.Timestamp.Equal(base.Add(5 * time.Microsecond)) {
		t.Fatalf("later timestamp mutated to %v, want %v", evLater.Timestamp, base.Add(5*time.Microsecond))
	}

	evZeroLast := &session.Event{Timestamp: base}
	advanceEventTimestampAfter(evZeroLast, time.Time{})
	if !evZeroLast.Timestamp.Equal(base) {
		t.Fatalf("zero-last timestamp mutated to %v, want %v", evZeroLast.Timestamp, base)
	}
}

type appendHookService struct {
	session.Service
	onAppend func(context.Context, *session.Event)
}

func (s *appendHookService) AppendEvent(ctx context.Context, sess session.Session, ev *session.Event) error {
	if s.onAppend != nil {
		s.onAppend(ctx, ev)
	}
	return s.Service.AppendEvent(ctx, sess, ev)
}
