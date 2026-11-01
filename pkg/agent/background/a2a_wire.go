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

package background

import (
	"encoding/json"
	"fmt"
)

// A2AWellKnownCardPath is the spec-defined agent-card discovery path.
const A2AWellKnownCardPath = "/.well-known/agent-card.json"

// A2AProtocolVersion is the A2A spec line this client implements and
// advertises in the A2A-Version request header.
const A2AProtocolVersion = "0.3"

// A2AVersionHeader is the HTTP request header carrying A2AProtocolVersion.
const A2AVersionHeader = "A2A-Version"

// A2ATransportJSONRPC is the card transport identifier for JSON-RPC 2.0
// over HTTP. An absent preferredTransport defaults to JSON-RPC per spec.
const A2ATransportJSONRPC = "JSONRPC"

// A2ACard is the discovery-time contract served at
// /.well-known/agent-card.json.
type A2ACard struct {
	Name                 string                `json:"name"`
	Description          string                `json:"description,omitempty"`
	URL                  string                `json:"url"`
	Version              string                `json:"version,omitempty"`
	ProtocolVersion      string                `json:"protocolVersion,omitempty"`
	PreferredTransport   string                `json:"preferredTransport,omitempty"`
	AdditionalInterfaces []A2AInterface        `json:"additionalInterfaces,omitempty"`
	Capabilities         A2ACapabilities       `json:"capabilities,omitempty"`
	DefaultInputModes    []string              `json:"defaultInputModes,omitempty"`
	DefaultOutputModes   []string              `json:"defaultOutputModes,omitempty"`
	Skills               []A2ASkill            `json:"skills,omitempty"`
	SecuritySchemes      *A2ASecuritySchemes   `json:"securitySchemes,omitempty"`
	Security             []map[string][]string `json:"security,omitempty"`
}

// A2ASecuritySchemes is the card's declared security-scheme set.
type A2ASecuritySchemes struct {
	Bearer *A2ASecurityScheme `json:"bearer,omitempty"`
}

// A2ASecurityScheme is one A2A security scheme (HTTP Bearer).
type A2ASecurityScheme struct {
	Type   string `json:"type"`
	Scheme string `json:"scheme"`
}

// A2AInterface is one (transport, url) alternative from the card.
type A2AInterface struct {
	Transport string `json:"transport"`
	URL       string `json:"url"`
}

// A2ACapabilities is the card's capability declaration.
type A2ACapabilities struct {
	Streaming         bool `json:"streaming,omitempty"`
	PushNotifications bool `json:"pushNotifications,omitempty"`
}

// A2ASkill is one named capability advertised in an A2ACard.
type A2ASkill struct {
	ID          string   `json:"id"`
	Name        string   `json:"name,omitempty"`
	Description string   `json:"description,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	InputModes  []string `json:"inputModes,omitempty"`
	OutputModes []string `json:"outputModes,omitempty"`
}

// A2AMessage is an A2A message (kind: "message").
type A2AMessage struct {
	Kind      string         `json:"kind"`
	MessageID string         `json:"messageId"`
	Role      string         `json:"role"`
	Parts     []A2APart      `json:"parts"`
	TaskID    string         `json:"taskId,omitempty"`
	ContextID string         `json:"contextId,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

// A2APart is a message or artifact content part ("text" or "data").
type A2APart struct {
	Kind     string         `json:"kind"`
	Text     string         `json:"text,omitempty"`
	Data     map[string]any `json:"data,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// A2ATask is an A2A task (kind: "task").
type A2ATask struct {
	Kind      string        `json:"kind"`
	ID        string        `json:"id"`
	ContextID string        `json:"contextId,omitempty"`
	Status    A2ATaskStatus `json:"status"`
	Artifacts []A2AArtifact `json:"artifacts,omitempty"`
}

// A2ATaskStatus is the task's current lifecycle position.
type A2ATaskStatus struct {
	State   A2ATaskState `json:"state"`
	Message *A2AMessage  `json:"message,omitempty"`
}

// A2AArtifact is a task output artifact.
type A2AArtifact struct {
	ArtifactID  string    `json:"artifactId,omitempty"`
	Name        string    `json:"name,omitempty"`
	Description string    `json:"description,omitempty"`
	Parts       []A2APart `json:"parts,omitempty"`
}

// A2ATaskStatusUpdateEvent is one streaming status update (kind:
// "status-update") in a message/stream SSE response. Final marks the
// terminal update that ends the stream.
type A2ATaskStatusUpdateEvent struct {
	Kind      string         `json:"kind"`
	TaskID    string         `json:"taskId"`
	ContextID string         `json:"contextId,omitempty"`
	Status    A2ATaskStatus  `json:"status"`
	Final     bool           `json:"final"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

// A2ATaskArtifactUpdateEvent is one streaming artifact update (kind:
// "artifact-update") in a message/stream SSE response.
type A2ATaskArtifactUpdateEvent struct {
	Kind      string         `json:"kind"`
	TaskID    string         `json:"taskId"`
	ContextID string         `json:"contextId,omitempty"`
	Artifact  A2AArtifact    `json:"artifact"`
	Append    bool           `json:"append,omitempty"`
	LastChunk bool           `json:"lastChunk,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

// A2ATaskState is the A2A v0.3 task lifecycle vocabulary.
type A2ATaskState string

const (
	A2ATaskStateSubmitted     A2ATaskState = "submitted"
	A2ATaskStateWorking       A2ATaskState = "working"
	A2ATaskStateInputRequired A2ATaskState = "input-required"
	A2ATaskStateAuthRequired  A2ATaskState = "auth-required"
	A2ATaskStateCompleted     A2ATaskState = "completed"
	A2ATaskStateFailed        A2ATaskState = "failed"
	A2ATaskStateCanceled      A2ATaskState = "canceled"
	A2ATaskStateRejected      A2ATaskState = "rejected"
)

// Terminal reports whether the state ends the task lifecycle.
func (s A2ATaskState) Terminal() bool {
	switch s {
	case A2ATaskStateCompleted, A2ATaskStateFailed, A2ATaskStateCanceled, A2ATaskStateRejected:
		return true
	}
	return false
}

// A2AResult is the normalized outcome of a synchronous A2AClient.Send call.
type A2AResult struct {
	State    string          `json:"state"`
	RemoteID string          `json:"remote_id,omitempty"`
	Text     string          `json:"text,omitempty"`
	Output   map[string]any  `json:"output,omitempty"`
	Raw      json.RawMessage `json:"raw,omitempty"`
}

// JSON-RPC 2.0 methods on the single A2A endpoint.
const (
	methodMessageSend   = "message/send"
	methodMessageStream = "message/stream"
	methodTasksGet      = "tasks/get"
	methodTasksCancel   = "tasks/cancel"
)

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *A2ARPCError    `json:"error,omitempty"`
}

// A2ARPCError is a JSON-RPC 2.0 error object returned by an A2A server.
type A2ARPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *A2ARPCError) Error() string {
	return fmt.Sprintf("a2a: JSON-RPC error %d: %s", e.Code, e.Message)
}

type messageSendParams struct {
	Message       *A2AMessage        `json:"message"`
	Configuration *messageSendConfig `json:"configuration,omitempty"`
}

type messageSendConfig struct {
	Blocking            *bool    `json:"blocking,omitempty"`
	AcceptedOutputModes []string `json:"acceptedOutputModes,omitempty"`
	HistoryLength       *int     `json:"historyLength,omitempty"`
}

type taskQueryParams struct {
	ID            string `json:"id"`
	HistoryLength *int   `json:"historyLength,omitempty"`
}

type taskIDParams struct {
	ID string `json:"id"`
}

type sendReply struct {
	Message *A2AMessage
	Task    *A2ATask
	Raw     json.RawMessage
}

func decodeSendReply(raw json.RawMessage) (*sendReply, error) {
	var probe struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("a2a: malformed message/send result: %w", err)
	}
	reply := &sendReply{Raw: raw}
	switch probe.Kind {
	case "message":
		reply.Message = &A2AMessage{}
		if err := json.Unmarshal(raw, reply.Message); err != nil {
			return nil, fmt.Errorf("a2a: malformed message reply: %w", err)
		}
	case "task":
		reply.Task = &A2ATask{}
		if err := json.Unmarshal(raw, reply.Task); err != nil {
			return nil, fmt.Errorf("a2a: malformed task reply: %w", err)
		}
	default:
		return nil, fmt.Errorf("a2a: message/send result has unknown kind %q (want \"message\" or \"task\")", probe.Kind)
	}
	return reply, nil
}
