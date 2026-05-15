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
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/go-steer/core-agent/agent"
	axproto "github.com/go-steer/core-agent/extras/ax-agent/internal/axproto"
	"github.com/go-steer/core-agent/models/mock"
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
