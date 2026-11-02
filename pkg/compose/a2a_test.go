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

package compose

import (
	"context"
	"errors"
	"iter"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	adkagent "google.golang.org/adk/v2/agent"
	adkmodel "google.golang.org/adk/v2/model"
	adktool "google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/genai"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"github.com/go-steer/core-agent/v3/pkg/attach"
	"github.com/go-steer/core-agent/v3/pkg/auth"
	"github.com/go-steer/core-agent/v3/pkg/config"
	"github.com/go-steer/core-agent/v3/pkg/eventlog"
	"github.com/go-steer/core-agent/v3/pkg/permissions"
)

type scriptedA2AModel struct {
	name string
	fn   func(ctx context.Context, req *adkmodel.LLMRequest, stream bool) iter.Seq2[*adkmodel.LLMResponse, error]
}

func (m *scriptedA2AModel) Name() string {
	if m.name != "" {
		return m.name
	}
	return "scripted-a2a"
}

func (m *scriptedA2AModel) GenerateContent(ctx context.Context, req *adkmodel.LLMRequest, stream bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return m.fn(ctx, req, stream)
}

func TestBuildA2AOptions(t *testing.T) {
	t.Parallel()

	if opts := BuildA2AOptions(nil, nil); opts.Expose {
		t.Fatalf("BuildA2AOptions(nil) Expose = true, want false")
	}

	cfg := &config.Config{
		Agent: config.AgentConfig{Description: "Fallback agent description"},
		A2A: config.A2AConfig{
			Expose:      true,
			SkillName:   "triage",
			InputSchema: map[string]any{"type": "object"},
			Auth: config.A2AAuthConfig{
				Required: true,
				Scopes:   []string{"a2a:invoke"},
			},
		},
	}
	backend := NewA2ABackend(A2ABackendOptions{})
	got := BuildA2AOptions(cfg, backend)
	if !got.Expose {
		t.Fatalf("Expose = false, want true")
	}
	if got.SkillName != "triage" {
		t.Errorf("SkillName = %q, want triage", got.SkillName)
	}
	if got.SkillDescription != "Fallback agent description" {
		t.Errorf("SkillDescription = %q, want fallback description", got.SkillDescription)
	}
	if !got.Auth.Required || len(got.Auth.Scopes) != 1 || got.Auth.Scopes[0] != "a2a:invoke" {
		t.Errorf("Auth = %+v, want Required=true Scopes=[a2a:invoke]", got.Auth)
	}
	if got.Backend != backend {
		t.Errorf("Backend pointer mismatch")
	}
}

func TestA2ABackend_ContextReuseAndMultiTurn(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	var mu sync.Mutex
	var callCount int
	var seenUserPrompts []int
	model := &scriptedA2AModel{
		fn: func(_ context.Context, req *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
			return func(yield func(*adkmodel.LLMResponse, error) bool) {
				mu.Lock()
				callCount++
				idx := callCount
				userCount := 0
				for _, c := range req.Contents {
					if c != nil && c.Role == genai.RoleUser {
						userCount++
					}
				}
				seenUserPrompts = append(seenUserPrompts, userCount)
				mu.Unlock()

				resp := &adkmodel.LLMResponse{
					Content:      genai.NewContentFromText("reply-"+string(rune('0'+idx)), genai.RoleModel),
					TurnComplete: true,
					FinishReason: genai.FinishReasonStop,
				}
				yield(resp, nil)
			}
		},
	}

	reg := attach.NewSessionRegistry()
	deps := SessionFactoryDeps{
		DaemonCtx:      ctx,
		Model:          model,
		Template:       permissions.New(permissions.Options{Mode: permissions.ModeAllow}),
		Registry:       reg,
		NoCompact:      true,
		CheckpointMode: config.CheckpointModeOff,
	}
	backend := NewA2ABackend(A2ABackendOptions{
		DaemonCtx:   ctx,
		Registry:    reg,
		FactoryDeps: &deps,
	})

	out1, err := backend.SendMessage(ctx, attach.A2ATaskRequest{
		TaskID:    "task-1",
		ContextID: "ctx-shared",
		Text:      "first message",
		Caller:    auth.Caller{Identity: "alice"},
	})
	if err != nil {
		t.Fatalf("SendMessage 1: %v", err)
	}
	if out1.State != attach.TaskStateCompleted || out1.Output != "reply-1" {
		t.Fatalf("out1 = %+v, want completed reply-1", out1)
	}

	out2, err := backend.SendMessage(ctx, attach.A2ATaskRequest{
		TaskID:    "task-2",
		ContextID: "ctx-shared",
		Text:      "second message in same context",
		Caller:    auth.Caller{Identity: "alice"},
	})
	if err != nil {
		t.Fatalf("SendMessage 2: %v", err)
	}
	if out2.State != attach.TaskStateCompleted || out2.Output != "reply-2" {
		t.Fatalf("out2 = %+v, want completed reply-2", out2)
	}

	// Only 1 session should have been registered for ctx-shared.
	if got := len(reg.List()); got != 1 {
		t.Fatalf("registered sessions = %d, want 1 for shared contextId", got)
	}
	mu.Lock()
	if len(seenUserPrompts) != 2 || seenUserPrompts[1] <= seenUserPrompts[0] {
		t.Errorf("seenUserPrompts = %v, want turn 2 to include turn 1 history", seenUserPrompts)
	}
	mu.Unlock()

	// A different contextId creates a second session.
	if _, err := backend.SendMessage(ctx, attach.A2ATaskRequest{
		TaskID:    "task-3",
		ContextID: "ctx-other",
		Text:      "isolated context",
		Caller:    auth.Caller{Identity: "alice"},
	}); err != nil {
		t.Fatalf("SendMessage 3: %v", err)
	}
	if got := len(reg.List()); got != 2 {
		t.Fatalf("registered sessions = %d, want 2 after second contextId", got)
	}
}

type scaleDeployArgs struct {
	Deployment string `json:"deployment"`
	Replicas   int    `json:"replicas"`
}

func TestA2ABackend_DurableParkResumeAndCrossRestartRecovery(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	dbPath := filepath.Join(t.TempDir(), "a2a-parks.db")
	handle, err := eventlog.Open(
		ctx,
		sqlite.Open(dbPath),
		eventlog.WithGORMConfig(&gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)}),
	)
	if err != nil {
		t.Fatalf("eventlog.Open: %v", err)
	}
	t.Cleanup(func() { _ = handle.Close() })

	aclStore, err := attach.NewSessionACLStore(ctx, handle.DB)
	if err != nil {
		t.Fatalf("NewSessionACLStore: %v", err)
	}

	var mu sync.Mutex
	var executions []scaleDeployArgs
	scaleTool, err := functiontool.New(functiontool.Config{
		Name:        "scale_deployment",
		Description: "Scale a deployment replica count",
	}, func(tctx adkagent.Context, args scaleDeployArgs) (map[string]any, error) {
		c := tctx.ToolConfirmation()
		if c == nil {
			if err := tctx.RequestConfirmation("Approve scaling "+args.Deployment+"?", map[string]any{
				"tool": "scale_deployment",
				"args": map[string]any{"deployment": args.Deployment, "replicas": args.Replicas},
			}); err != nil {
				return nil, err
			}
			return map[string]any{"status": "awaiting operator approval"}, nil
		}
		if !c.Confirmed {
			return map[string]any{"status": "rejected"}, nil
		}
		mu.Lock()
		executions = append(executions, args)
		mu.Unlock()
		return map[string]any{"scaled": args.Replicas}, nil
	})
	if err != nil {
		t.Fatalf("functiontool.New: %v", err)
	}

	model := &scriptedA2AModel{
		fn: func(_ context.Context, req *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
			return func(yield func(*adkmodel.LLMResponse, error) bool) {
				hasScaledResult := false
				for _, c := range req.Contents {
					if c == nil {
						continue
					}
					for _, p := range c.Parts {
						if p != nil && p.FunctionResponse != nil && p.FunctionResponse.Name == "scale_deployment" {
							if _, ok := p.FunctionResponse.Response["scaled"]; ok {
								hasScaledResult = true
							}
						}
					}
				}
				var resp *adkmodel.LLMResponse
				if !hasScaledResult {
					resp = &adkmodel.LLMResponse{
						Content: &genai.Content{
							Role: genai.RoleModel,
							Parts: []*genai.Part{genai.NewPartFromFunctionCall("scale_deployment", map[string]any{
								"deployment": "checkout",
								"replicas":   4,
							})},
						},
						TurnComplete: true,
						FinishReason: genai.FinishReasonStop,
					}
				} else {
					resp = &adkmodel.LLMResponse{
						Content:      genai.NewContentFromText("scaled checkout to 4 replicas", genai.RoleModel),
						TurnComplete: true,
						FinishReason: genai.FinishReasonStop,
					}
				}
				yield(resp, nil)
			}
		},
	}

	buildBackend := func() *A2ABackend {
		reg := attach.NewSessionRegistryWithStore(aclStore)
		deps := SessionFactoryDeps{
			DaemonCtx:      ctx,
			Model:          model,
			Template:       permissions.New(permissions.Options{Mode: permissions.ModeAllow}),
			BuiltinTools:   []adktool.Tool{scaleTool},
			EventlogHandle: handle,
			Registry:       reg,
			ACLStore:       aclStore,
			NoCompact:      true,
			CheckpointMode: config.CheckpointModeOff,
		}
		return NewA2ABackend(A2ABackendOptions{
			DaemonCtx:      ctx,
			Registry:       reg,
			FactoryDeps:    &deps,
			EventlogHandle: handle,
			ACLStore:       aclStore,
			EnforceACL:     true,
		})
	}

	// Daemon 1: Turn 1 parks at the write gate -> input-required.
	backend1 := buildBackend()
	aliceCtx := auth.WithCaller(ctx, auth.Caller{Identity: "alice"})
	out1, err := backend1.SendMessage(aliceCtx, attach.A2ATaskRequest{
		TaskID:    "task-park-1",
		ContextID: "ctx-park-1",
		Text:      "please scale checkout to 4",
		Caller:    auth.Caller{Identity: "alice"},
	})
	if err != nil {
		t.Fatalf("Turn 1 SendMessage: %v", err)
	}
	if out1.State != attach.TaskStateInputRequired {
		t.Fatalf("Turn 1 State = %q, want %q", out1.State, attach.TaskStateInputRequired)
	}
	if !strings.Contains(out1.StatusMessage, "Approve scaling checkout?") {
		t.Errorf("Turn 1 StatusMessage = %q, want 'Approve scaling checkout?'", out1.StatusMessage)
	}
	mu.Lock()
	if len(executions) != 0 {
		t.Fatalf("executions after park = %d, want 0", len(executions))
	}
	mu.Unlock()

	// Simulate daemon restart: construct backend2 with an empty in-memory taskRegistry.
	backend2 := buildBackend()
	recoveredPark, found, err := backend2.GetTask(aliceCtx, "task-park-1")
	if err != nil {
		t.Fatalf("post-restart GetTask(parked): %v", err)
	}
	if !found {
		t.Fatalf("post-restart GetTask(parked) found = false, want true")
	}
	if recoveredPark.State != attach.TaskStateInputRequired {
		t.Fatalf("post-restart GetTask State = %q, want %q", recoveredPark.State, attach.TaskStateInputRequired)
	}
	if recoveredPark.ContextID != "ctx-park-1" {
		t.Errorf("post-restart GetTask ContextID = %q, want ctx-park-1", recoveredPark.ContextID)
	}

	// Resume parked task on backend2 with an approval verdict.
	out2, err := backend2.SendMessage(aliceCtx, attach.A2ATaskRequest{
		TaskID: "task-park-1",
		Text:   "approve",
		Caller: auth.Caller{Identity: "alice"},
	})
	if err != nil {
		t.Fatalf("Turn 2 (resume) SendMessage: %v", err)
	}
	if out2.State != attach.TaskStateCompleted {
		t.Fatalf("Turn 2 State = %q, want %q (status=%q)", out2.State, attach.TaskStateCompleted, out2.StatusMessage)
	}
	if out2.Output != "scaled checkout to 4 replicas" {
		t.Errorf("Turn 2 Output = %q, want 'scaled checkout to 4 replicas'", out2.Output)
	}
	mu.Lock()
	if len(executions) != 1 || executions[0].Deployment != "checkout" || executions[0].Replicas != 4 {
		t.Errorf("executions after approval = %+v, want [{checkout 4}]", executions)
	}
	mu.Unlock()

	// Simulate a second daemon restart: GetTask on backend3 recovers completed state + output.
	backend3 := buildBackend()
	recoveredDone, found, err := backend3.GetTask(aliceCtx, "task-park-1")
	if err != nil {
		t.Fatalf("second post-restart GetTask(completed): %v", err)
	}
	if !found {
		t.Fatalf("second post-restart GetTask(completed) found = false, want true")
	}
	if recoveredDone.State != attach.TaskStateCompleted {
		t.Fatalf("second post-restart State = %q, want %q", recoveredDone.State, attach.TaskStateCompleted)
	}
	if recoveredDone.Output != "scaled checkout to 4 replicas" {
		t.Errorf("second post-restart Output = %q, want 'scaled checkout to 4 replicas'", recoveredDone.Output)
	}
}

func TestA2ABackend_BoundedTaskRegistry_LRUAndTTLAndPinning(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	var clockMu sync.Mutex
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	nowFn := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return now
	}
	advanceClock := func(d time.Duration) {
		clockMu.Lock()
		now = now.Add(d)
		clockMu.Unlock()
	}

	blockCh := make(chan struct{})
	startedInFlight := make(chan struct{})
	model := &scriptedA2AModel{
		fn: func(_ context.Context, req *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
			return func(yield func(*adkmodel.LLMResponse, error) bool) {
				for _, c := range req.Contents {
					if c != nil && extractContentText(c) == "hold-inflight" {
						close(startedInFlight)
						<-blockCh
					}
				}
				yield(&adkmodel.LLMResponse{
					Content:      genai.NewContentFromText("ok", genai.RoleModel),
					TurnComplete: true,
					FinishReason: genai.FinishReasonStop,
				}, nil)
			}
		},
	}

	reg := attach.NewSessionRegistry()
	deps := SessionFactoryDeps{
		DaemonCtx:      ctx,
		Model:          model,
		Template:       permissions.New(permissions.Options{Mode: permissions.ModeAllow}),
		Registry:       reg,
		NoCompact:      true,
		CheckpointMode: config.CheckpointModeOff,
	}
	backend := NewA2ABackend(A2ABackendOptions{
		DaemonCtx:   ctx,
		Registry:    reg,
		FactoryDeps: &deps,
		MaxTasks:    2,
		TaskTTL:     10 * time.Minute,
		Now:         nowFn,
	})

	if _, err := backend.SendMessage(ctx, attach.A2ATaskRequest{TaskID: "t1", ContextID: "c1", Text: "1"}); err != nil {
		t.Fatalf("t1: %v", err)
	}
	advanceClock(1 * time.Minute)
	if _, err := backend.SendMessage(ctx, attach.A2ATaskRequest{TaskID: "t2", ContextID: "c2", Text: "2"}); err != nil {
		t.Fatalf("t2: %v", err)
	}
	advanceClock(1 * time.Minute)
	// Touch t1 via GetTask so t2 is the least recently accessed.
	if _, found, err := backend.GetTask(ctx, "t1"); err != nil || !found {
		t.Fatalf("GetTask(t1): found=%v err=%v", found, err)
	}
	advanceClock(1 * time.Minute)
	if _, err := backend.SendMessage(ctx, attach.A2ATaskRequest{TaskID: "t3", ContextID: "c3", Text: "3"}); err != nil {
		t.Fatalf("t3: %v", err)
	}

	backend.mu.Lock()
	_, hasT1 := backend.tasks["t1"]
	_, hasT2 := backend.tasks["t2"]
	_, hasT3 := backend.tasks["t3"]
	backend.mu.Unlock()
	if !hasT1 || hasT2 || !hasT3 {
		t.Fatalf("after LRU cap=2: hasT1=%v hasT2=%v hasT3=%v, want true/false/true", hasT1, hasT2, hasT3)
	}

	// Start an in-flight task and advance clock past TaskTTL (15m).
	doneInFlight := make(chan error, 1)
	go func() {
		_, err := backend.SendMessage(ctx, attach.A2ATaskRequest{
			TaskID:    "t-inflight",
			ContextID: "c-inflight",
			Text:      "hold-inflight",
		})
		doneInFlight <- err
	}()
	<-startedInFlight

	advanceClock(15 * time.Minute)
	// Trigger eviction via GetTask on the in-flight task.
	inflightOut, found, err := backend.GetTask(ctx, "t-inflight")
	if err != nil || !found || inflightOut.State != attach.TaskStateWorking {
		t.Fatalf("GetTask(t-inflight): out=%+v found=%v err=%v, want working", inflightOut, found, err)
	}
	backend.mu.Lock()
	_, stillHasT1 := backend.tasks["t1"]
	_, stillHasT3 := backend.tasks["t3"]
	_, stillHasInFlight := backend.tasks["t-inflight"]
	backend.mu.Unlock()
	if stillHasT1 || stillHasT3 || !stillHasInFlight {
		t.Fatalf("after TTL expiry: t1=%v t3=%v inflight=%v, want false/false/true", stillHasT1, stillHasT3, stillHasInFlight)
	}

	close(blockCh)
	if err := <-doneInFlight; err != nil {
		t.Fatalf("t-inflight SendMessage: %v", err)
	}
}

func TestA2ABackend_CancelWinsInvariant(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	turnStarted := make(chan struct{})
	allowFinish := make(chan struct{})
	model := &scriptedA2AModel{
		fn: func(_ context.Context, _ *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
			return func(yield func(*adkmodel.LLMResponse, error) bool) {
				close(turnStarted)
				<-allowFinish
				// Even if the model ignores ctx.Done() and yields a final response,
				// cancel-wins must keep the task in TaskStateCanceled and suppress output.
				yield(&adkmodel.LLMResponse{
					Content:      genai.NewContentFromText("late output after cancel", genai.RoleModel),
					TurnComplete: true,
					FinishReason: genai.FinishReasonStop,
				}, nil)
			}
		},
	}

	reg := attach.NewSessionRegistry()
	deps := SessionFactoryDeps{
		DaemonCtx:      ctx,
		Model:          model,
		Template:       permissions.New(permissions.Options{Mode: permissions.ModeAllow}),
		Registry:       reg,
		NoCompact:      true,
		CheckpointMode: config.CheckpointModeOff,
	}
	backend := NewA2ABackend(A2ABackendOptions{
		DaemonCtx:   ctx,
		Registry:    reg,
		FactoryDeps: &deps,
	})

	type sendRes struct {
		out attach.A2ATaskOutcome
		err error
	}
	resCh := make(chan sendRes, 1)
	go func() {
		out, err := backend.SendMessage(ctx, attach.A2ATaskRequest{
			TaskID:    "task-cancel-race",
			ContextID: "ctx-cancel-race",
			Text:      "work",
		})
		resCh <- sendRes{out: out, err: err}
	}()

	<-turnStarted
	canceledOut, found, err := backend.CancelTask(ctx, "task-cancel-race")
	if err != nil || !found {
		t.Fatalf("CancelTask: found=%v err=%v", found, err)
	}
	if canceledOut.State != attach.TaskStateCanceled {
		t.Fatalf("CancelTask State = %q, want %q", canceledOut.State, attach.TaskStateCanceled)
	}

	close(allowFinish)
	res := <-resCh
	if res.err != nil {
		t.Fatalf("SendMessage after cancel returned err = %v, want nil with canceled outcome", res.err)
	}
	if res.out.State != attach.TaskStateCanceled || res.out.Output != "" {
		t.Fatalf("SendMessage outcome after cancel = %+v, want State=canceled Output=\"\"", res.out)
	}

	got, found, err := backend.GetTask(ctx, "task-cancel-race")
	if err != nil || !found || got.State != attach.TaskStateCanceled || got.Output != "" {
		t.Fatalf("GetTask after cancel race = %+v (found=%v err=%v), want State=canceled Output=\"\"", got, found, err)
	}
}

func TestA2ABackend_PartialSkipAndDetachedSendContext(t *testing.T) {
	t.Parallel()

	daemonCtx, cancelDaemon := context.WithCancel(context.Background())
	t.Cleanup(cancelDaemon)

	reqCanceledObserved := make(chan struct{})
	model := &scriptedA2AModel{
		fn: func(mctx context.Context, req *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
			return func(yield func(*adkmodel.LLMResponse, error) bool) {
				prompt := ""
				for _, c := range req.Contents {
					if c != nil && c.Role == genai.RoleUser {
						prompt = extractContentText(c)
					}
				}
				if prompt == "detach-check" {
					<-reqCanceledObserved
					if err := mctx.Err(); err != nil {
						yield(nil, err)
						return
					}
				}
				// Emit two partial deltas and one final complete event (mast #408).
				if !yield(&adkmodel.LLMResponse{
					Content: genai.NewContentFromText("hel", genai.RoleModel),
					Partial: true,
				}, nil) {
					return
				}
				if !yield(&adkmodel.LLMResponse{
					Content: genai.NewContentFromText("lo", genai.RoleModel),
					Partial: true,
				}, nil) {
					return
				}
				yield(&adkmodel.LLMResponse{
					Content:      genai.NewContentFromText("hello world", genai.RoleModel),
					Partial:      false,
					TurnComplete: true,
					FinishReason: genai.FinishReasonStop,
				}, nil)
			}
		},
	}

	reg := attach.NewSessionRegistry()
	deps := SessionFactoryDeps{
		DaemonCtx:      daemonCtx,
		Model:          model,
		Template:       permissions.New(permissions.Options{Mode: permissions.ModeAllow}),
		Registry:       reg,
		NoCompact:      true,
		CheckpointMode: config.CheckpointModeOff,
	}
	backend := NewA2ABackend(A2ABackendOptions{
		DaemonCtx:   daemonCtx,
		Registry:    reg,
		FactoryDeps: &deps,
	})

	// 1. Streaming progress callback skips Partial=true events (mast #408).
	var progressTexts []string
	outStream, err := backend.SendMessage(daemonCtx, attach.A2ATaskRequest{
		TaskID:    "task-partial",
		ContextID: "ctx-partial",
		Text:      "stream-check",
		OnProgress: func(_ attach.A2ATaskState, statusText string) {
			progressTexts = append(progressTexts, statusText)
		},
	})
	if err != nil {
		t.Fatalf("SendMessage(stream-check): %v", err)
	}
	if len(progressTexts) != 1 || progressTexts[0] != "hello world" {
		t.Errorf("progressTexts = %v, want [\"hello world\"] (partial chunks must be skipped)", progressTexts)
	}
	if outStream.Output != "hello world" {
		t.Errorf("outStream.Output = %q, want 'hello world'", outStream.Output)
	}

	// 2. Non-streaming message/send detaches from HTTP request context (mast #562).
	httpCtx, cancelHTTP := context.WithCancel(daemonCtx)
	cancelHTTP()
	close(reqCanceledObserved)

	outDetached, err := backend.SendMessage(httpCtx, attach.A2ATaskRequest{
		TaskID:    "task-detached",
		ContextID: "ctx-detached",
		Text:      "detach-check",
	})
	if err != nil {
		t.Fatalf("SendMessage with canceled HTTP ctx failed: %v (want detached turn to succeed)", err)
	}
	if outDetached.State != attach.TaskStateCompleted || outDetached.Output != "hello world" {
		t.Errorf("outDetached = %+v, want completed 'hello world'", outDetached)
	}
}

func TestA2ABackend_ACLIsolation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	model := &scriptedA2AModel{
		fn: func(_ context.Context, _ *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
			return func(yield func(*adkmodel.LLMResponse, error) bool) {
				yield(&adkmodel.LLMResponse{
					Content:      genai.NewContentFromText("alice secret", genai.RoleModel),
					TurnComplete: true,
					FinishReason: genai.FinishReasonStop,
				}, nil)
			}
		},
	}

	reg := attach.NewSessionRegistry()
	deps := SessionFactoryDeps{
		DaemonCtx:      ctx,
		Model:          model,
		Template:       permissions.New(permissions.Options{Mode: permissions.ModeAllow}),
		Registry:       reg,
		NoCompact:      true,
		CheckpointMode: config.CheckpointModeOff,
	}
	backend := NewA2ABackend(A2ABackendOptions{
		DaemonCtx:   ctx,
		Registry:    reg,
		FactoryDeps: &deps,
		EnforceACL:  true,
	})

	alice := auth.Caller{Identity: "alice"}
	bob := auth.Caller{Identity: "bob"}
	aliceCtx := auth.WithCaller(ctx, alice)
	bobCtx := auth.WithCaller(ctx, bob)

	if _, err := backend.SendMessage(aliceCtx, attach.A2ATaskRequest{
		TaskID:    "task-alice",
		ContextID: "ctx-alice",
		Text:      "hello",
		Caller:    alice,
	}); err != nil {
		t.Fatalf("alice SendMessage: %v", err)
	}

	if _, _, err := backend.GetTask(bobCtx, "task-alice"); !errors.Is(err, attach.ErrA2AForbidden) {
		t.Errorf("bob GetTask(task-alice) err = %v, want ErrA2AForbidden", err)
	}
	if _, _, err := backend.CancelTask(bobCtx, "task-alice"); !errors.Is(err, attach.ErrA2AForbidden) {
		t.Errorf("bob CancelTask(task-alice) err = %v, want ErrA2AForbidden", err)
	}
	if err := backend.PreflightTask(bobCtx, attach.A2ATaskRequest{
		ContextID: "ctx-alice",
		Caller:    bob,
	}); !errors.Is(err, attach.ErrA2AForbidden) {
		t.Errorf("bob PreflightTask(ctx-alice) err = %v, want ErrA2AForbidden", err)
	}
	if _, err := backend.SendMessage(bobCtx, attach.A2ATaskRequest{
		TaskID:    "task-bob-hijack",
		ContextID: "ctx-alice",
		Text:      "hijack",
		Caller:    bob,
	}); !errors.Is(err, attach.ErrA2AForbidden) {
		t.Errorf("bob SendMessage(ctx-alice) err = %v, want ErrA2AForbidden", err)
	}
}

func TestA2ABackend_ApproverAttributionAndTerminalTaskRejection(t *testing.T) {
	t.Parallel()

	// 1. Anonymous caller produces empty Approver (never "anon") and spoofed approver is rejected.
	vAnon, err := parseA2AApprovalVerdict(attach.A2ATaskRequest{Text: "approve"}, auth.Anonymous)
	if err != nil {
		t.Fatalf("parseA2AApprovalVerdict(anon): %v", err)
	}
	if vAnon.Approver != "" {
		t.Errorf("anon Approver = %q, want empty string", vAnon.Approver)
	}

	_, err = parseA2AApprovalVerdict(attach.A2ATaskRequest{
		Data: map[string]any{"verdict": "approve", "approver": "alice"},
	}, auth.Caller{Identity: "mallory"})
	if err == nil || !strings.Contains(err.Error(), "does not match authenticated caller") {
		t.Errorf("spoofed approver err = %v, want mismatch error", err)
	}

	// 2. Re-sending on an already-completed task ID is rejected.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	model := &scriptedA2AModel{
		fn: func(_ context.Context, _ *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
			return func(yield func(*adkmodel.LLMResponse, error) bool) {
				yield(&adkmodel.LLMResponse{
					Content:      genai.NewContentFromText("done", genai.RoleModel),
					TurnComplete: true,
					FinishReason: genai.FinishReasonStop,
				}, nil)
			}
		},
	}
	reg := attach.NewSessionRegistry()
	deps := SessionFactoryDeps{
		DaemonCtx:      ctx,
		Model:          model,
		Template:       permissions.New(permissions.Options{Mode: permissions.ModeAllow}),
		Registry:       reg,
		NoCompact:      true,
		CheckpointMode: config.CheckpointModeOff,
	}
	backend := NewA2ABackend(A2ABackendOptions{
		DaemonCtx:   ctx,
		Registry:    reg,
		FactoryDeps: &deps,
	})
	if _, err := backend.SendMessage(ctx, attach.A2ATaskRequest{
		TaskID:    "task-terminal-1",
		ContextID: "ctx-terminal-1",
		Text:      "run",
	}); err != nil {
		t.Fatalf("first SendMessage: %v", err)
	}
	if _, err := backend.SendMessage(ctx, attach.A2ATaskRequest{
		TaskID: "task-terminal-1",
		Text:   "re-open completed task",
	}); err == nil || !strings.Contains(err.Error(), "terminal state") {
		t.Fatalf("second SendMessage on completed task = %v, want terminal state error", err)
	}
}
