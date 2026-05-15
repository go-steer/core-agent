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

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	adkmodel "google.golang.org/adk/model"
	adktool "google.golang.org/adk/tool"
	"google.golang.org/genai"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/go-steer/core-agent/agent"
	axproto "github.com/go-steer/core-agent/extras/ax-agent/internal/axproto"
	"github.com/go-steer/core-agent/models/mock"
	"github.com/go-steer/core-agent/tools"
)

// startTestServer spins up an in-process gRPC server backed by the
// echo provider. Returns a connected client and a cleanup func.
func startTestServer(t *testing.T) (axproto.AgentServiceClient, func()) {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()

	echoProvider := mock.NewEcho()
	echoLLM, err := echoProvider.Model(context.Background(), "")
	if err != nil {
		t.Fatalf("echo provider: %v", err)
	}
	axproto.RegisterAgentServiceServer(srv, &axServer{
		agentFactory: func() (*agent.Agent, error) {
			return agent.New(echoLLM)
		},
	})

	go func() {
		if err := srv.Serve(lis); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			t.Logf("server: %v", err)
		}
	}()

	dialer := func(context.Context, string) (net.Conn, error) { return lis.Dial() }
	conn, err := grpc.NewClient("passthrough://bufnet",
		grpc.WithContextDialer(dialer),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return axproto.NewAgentServiceClient(conn), func() {
		_ = conn.Close()
		srv.GracefulStop()
	}
}

func TestConnect_EchoEndToEnd(t *testing.T) {
	t.Parallel()
	client, cleanup := startTestServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := client.Connect(ctx)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}

	if err := stream.Send(&axproto.AgentMessage{
		ConversationId: "conv-1",
		ExecId:         "exec-1",
		Type: &axproto.AgentMessage_Start{Start: &axproto.AgentStart{
			AgentId: "echo",
			Messages: []*axproto.Message{
				{Role: "user", Content: textContent("ping")},
			},
		}},
	}); err != nil {
		t.Fatalf("send start: %v", err)
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("close send: %v", err)
	}

	var (
		gotText string
		gotEnd  bool
	)
	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("recv: %v", err)
		}
		switch payload := msg.GetType().(type) {
		case *axproto.AgentMessage_Outputs:
			for _, m := range payload.Outputs.Messages {
				if tc, ok := m.GetContent().GetType().(*axproto.Content_Text); ok {
					gotText += tc.Text.GetText()
				}
			}
		case *axproto.AgentMessage_End:
			gotEnd = true
		default:
			t.Fatalf("unexpected message variant %T", payload)
		}
		if msg.ConversationId != "conv-1" || msg.ExecId != "exec-1" {
			t.Errorf("conv/exec id leaked: conv=%q exec=%q", msg.ConversationId, msg.ExecId)
		}
	}
	if !gotEnd {
		t.Errorf("server never sent AgentEnd")
	}
	if !strings.Contains(gotText, "ping") {
		t.Errorf("expected echoed 'ping' in response, got %q", gotText)
	}
}

func TestConnect_RejectsNonStartFirst(t *testing.T) {
	t.Parallel()
	client, cleanup := startTestServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := client.Connect(ctx)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}

	if err := stream.Send(&axproto.AgentMessage{
		ConversationId: "conv-1",
		ExecId:         "exec-1",
		Type:           &axproto.AgentMessage_Outputs{Outputs: &axproto.AgentOutputs{}},
	}); err != nil {
		t.Fatalf("send outputs: %v", err)
	}
	_ = stream.CloseSend()

	// We expect the server to reject and the stream to error out.
	for {
		_, err := stream.Recv()
		if err == nil {
			continue
		}
		if errors.Is(err, io.EOF) {
			t.Fatal("expected a server error, got EOF")
		}
		if !strings.Contains(err.Error(), "first message must be AgentStart") {
			t.Errorf("error message should mention AgentStart, got: %v", err)
		}
		return
	}
}

func TestHealthCheck(t *testing.T) {
	t.Parallel()
	client, cleanup := startTestServer(t)
	defer cleanup()

	resp, err := client.HealthCheck(context.Background(), &axproto.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("HealthCheck: %v", err)
	}
	if !resp.Healthy {
		t.Errorf("expected Healthy=true")
	}
}

// TestConnect_LifecycleToolEmitsInternalOnly proves that a model
// calling the bundled set_status tool produces wire messages flagged
// InternalOnly:true on both the call and response sides — the spirit
// of the autonomous-plan's step 3 ("AX UI sees the state but the
// conversation history stays clean").
func TestConnect_LifecycleToolEmitsInternalOnly(t *testing.T) {
	t.Parallel()
	llm := &lifecycleStubLLM{}
	lifecycleTool, err := tools.NewLifecycleTool(tools.LifecycleOptions{
		Handler: func(_ context.Context, _ tools.LifecycleEvent) error { return nil },
	})
	if err != nil {
		t.Fatalf("lifecycle tool: %v", err)
	}

	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	axproto.RegisterAgentServiceServer(srv, &axServer{
		agentFactory: func() (*agent.Agent, error) {
			return agent.New(llm,
				agent.WithSession("u-life", "s-life"),
				agent.WithTools([]adktool.Tool{lifecycleTool}),
			)
		},
	})
	go func() {
		if err := srv.Serve(lis); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			t.Logf("server: %v", err)
		}
	}()
	defer srv.GracefulStop()

	conn, err := grpc.NewClient("passthrough://bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := axproto.NewAgentServiceClient(conn).Connect(ctx)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := stream.Send(&axproto.AgentMessage{
		ConversationId: "c", ExecId: "e",
		Type: &axproto.AgentMessage_Start{Start: &axproto.AgentStart{
			AgentId:  "lifecycle",
			Messages: []*axproto.Message{{Role: "user", Content: textContent("go")}},
		}},
	}); err != nil {
		t.Fatalf("send start: %v", err)
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("close send: %v", err)
	}

	var (
		sawCallInternal   bool
		sawResultInternal bool
		gotEnd            bool
		callDetail        string
	)
	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("recv: %v", err)
		}
		switch payload := msg.GetType().(type) {
		case *axproto.AgentMessage_Outputs:
			for _, m := range payload.Outputs.GetMessages() {
				switch ct := m.GetContent().GetType().(type) {
				case *axproto.Content_ToolCall:
					if ct.ToolCall.GetFunctionCall().GetName() == "set_status" {
						if !m.GetInternalOnly() {
							t.Errorf("set_status ToolCall message must be InternalOnly:true; got false")
						}
						sawCallInternal = m.GetInternalOnly()
						if args := ct.ToolCall.GetFunctionCall().GetArguments(); args != nil {
							if d, ok := args.AsMap()["detail"].(string); ok {
								callDetail = d
							}
						}
					}
				case *axproto.Content_ToolResult:
					if ct.ToolResult.GetFunctionResult().GetName() == "set_status" {
						if !m.GetInternalOnly() {
							t.Errorf("set_status ToolResult message must be InternalOnly:true; got false")
						}
						sawResultInternal = m.GetInternalOnly()
					}
				}
			}
		case *axproto.AgentMessage_End:
			gotEnd = true
		}
	}
	if !sawCallInternal {
		t.Errorf("never saw an internal-only ToolCall for set_status")
	}
	if !sawResultInternal {
		t.Errorf("never saw an internal-only ToolResult for set_status")
	}
	if callDetail != "looking around" {
		t.Errorf("call detail = %q, want %q", callDetail, "looking around")
	}
	if !gotEnd {
		t.Errorf("missing AgentEnd")
	}
}

// lifecycleStubLLM scripts two LLM round-trips: the model first calls
// set_status, then (after the tool ack lands) emits a final text and
// completes the turn. Just enough surface to exercise the conversion
// path without dragging in the full scripted-provider machinery.
type lifecycleStubLLM struct {
	mu     sync.Mutex
	cursor int
}

func (l *lifecycleStubLLM) Name() string { return "lifecycle-stub" }

func (l *lifecycleStubLLM) GenerateContent(_ context.Context, _ *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		l.mu.Lock()
		n := l.cursor
		l.cursor++
		l.mu.Unlock()
		switch n {
		case 0:
			fc := &genai.FunctionCall{
				Name: "set_status",
				Args: map[string]any{"state": "thinking", "detail": "looking around"},
			}
			yield(&adkmodel.LLMResponse{
				Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: fc}}},
				TurnComplete: true,
				FinishReason: genai.FinishReasonStop,
			}, nil)
		case 1:
			yield(&adkmodel.LLMResponse{
				Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "all set"}}},
				TurnComplete: true,
				FinishReason: genai.FinishReasonStop,
			}, nil)
		default:
			yield(nil, fmt.Errorf("lifecycleStubLLM: unexpected call %d", n))
		}
	}
}
