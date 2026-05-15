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
	"fmt"

	"google.golang.org/grpc"

	"github.com/go-steer/core-agent/agent"
	axproto "github.com/go-steer/core-agent/extras/ax-agent/internal/axproto"
)

// axServer implements axproto.AgentServiceServer. One server handles
// every conversation; each Connect call gets a fresh agent (and a
// fresh session) constructed from the same agentOpts. The server is
// stateless across calls.
type axServer struct {
	axproto.UnimplementedAgentServiceServer
	agentFactory func() (*agent.Agent, error)
}

// HealthCheck signals liveness. AX dials this to probe whether the
// remote agent is reachable; we don't track meaningful state, so
// always return healthy.
func (s *axServer) HealthCheck(_ context.Context, _ *axproto.HealthCheckRequest) (*axproto.HealthCheckResponse, error) {
	return &axproto.HealthCheckResponse{Healthy: true, Message: "ok"}, nil
}

// Connect runs one AX execution turn. Per AX's protocol:
//   - Receive exactly one AgentStart on the stream.
//   - Run the agent over the supplied conversation history.
//   - Stream zero or more AgentOutputs as the agent emits events.
//   - Send exactly one AgentEnd to signal turn completion.
//
// The stream is one turn long; AX closes it after AgentEnd.
func (s *axServer) Connect(stream grpc.BidiStreamingServer[axproto.AgentMessage, axproto.AgentMessage]) error {
	msg, err := stream.Recv()
	if err != nil {
		return fmt.Errorf("ax-agent: recv start: %w", err)
	}
	start := msg.GetStart()
	if start == nil {
		return fmt.Errorf("ax-agent: first message must be AgentStart, got %T", msg.GetType())
	}
	convID := msg.GetConversationId()
	execID := msg.GetExecId()

	contents := axMessagesToGenai(start.GetMessages())
	if len(contents) == 0 {
		return fmt.Errorf("ax-agent: AgentStart contains no messages")
	}

	a, err := s.agentFactory()
	if err != nil {
		return fmt.Errorf("ax-agent: build agent: %w", err)
	}

	for ev, err := range a.RunWithContents(stream.Context(), contents) {
		if err != nil {
			return fmt.Errorf("ax-agent: agent run: %w", err)
		}
		outputs := genaiEventToAXOutputs(ev)
		if outputs == nil {
			continue
		}
		if sendErr := stream.Send(&axproto.AgentMessage{
			ConversationId: convID,
			ExecId:         execID,
			Type:           &axproto.AgentMessage_Outputs{Outputs: outputs},
		}); sendErr != nil {
			return fmt.Errorf("ax-agent: send outputs: %w", sendErr)
		}
	}

	return stream.Send(&axproto.AgentMessage{
		ConversationId: convID,
		ExecId:         execID,
		Type:           &axproto.AgentMessage_End{End: &axproto.AgentEnd{}},
	})
}
