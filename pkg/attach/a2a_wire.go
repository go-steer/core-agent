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

// Originally derived from go-steer/mast@d2f45c7 internal/a2a/wire.go (#1407).

package attach

import "encoding/json"

// A2A v0.3 protocol and path constants (#1407).
const (
	// A2APath is the HTTP path mounted on the attach mux when A2A server
	// exposure is enabled.
	A2APath = "/a2a"

	// A2AProtocolVersion is the A2A specification version spoken by this
	// handler.
	A2AProtocolVersion = "0.3.0"

	// A2AVersionHeader is the HTTP header carrying the A2A wire version.
	A2AVersionHeader = "A2A-Version"

	// A2AHeaderValue is the version prefix placed on outbound A2A responses.
	A2AHeaderValue = "0.3"
)

// A2A v0.3 JSON-RPC 2.0 method names.
const (
	A2AMethodMessageSend   = "message/send"
	A2AMethodMessageStream = "message/stream"
	A2AMethodTasksGet      = "tasks/get"
	A2AMethodTasksCancel   = "tasks/cancel"
)

// Standard JSON-RPC 2.0 error codes.
const (
	JSONRPCParseError     = -32700
	JSONRPCInvalidRequest = -32600
	JSONRPCMethodNotFound = -32601
	JSONRPCInvalidParams  = -32602
	JSONRPCInternalError  = -32603
	JSONRPCServerError    = -32000
)

// A2ATaskState is the A2A v0.3 lifecycle state of a Task.
type A2ATaskState string

// TaskState is an alias for A2ATaskState so callers can reference
// attach.TaskStateCompleted or attach.A2ATaskState interchangeably.
type TaskState = A2ATaskState

// Canonical A2A v0.3 task states.
const (
	TaskStateSubmitted     A2ATaskState = "submitted"
	TaskStateWorking       A2ATaskState = "working"
	TaskStateInputRequired A2ATaskState = "input-required"
	TaskStateCompleted     A2ATaskState = "completed"
	TaskStateFailed        A2ATaskState = "failed"
	TaskStateCanceled      A2ATaskState = "canceled"
	TaskStateRejected      A2ATaskState = "rejected"
	TaskStateAuthRequired  A2ATaskState = "auth-required"
)

// IsTerminal reports whether the task state is terminal (no further state
// transitions may occur). Note that TaskStateInputRequired is non-terminal:
// a parked task resumes when the caller sends a follow-up message.
func (s A2ATaskState) IsTerminal() bool {
	switch s {
	case TaskStateCompleted, TaskStateFailed, TaskStateCanceled, TaskStateRejected:
		return true
	default:
		return false
	}
}

// A2APart is one content part of an A2A Message or Artifact ("text" or "data").
type A2APart struct {
	Kind     string         `json:"kind"`
	Text     string         `json:"text,omitempty"`
	Data     map[string]any `json:"data,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// NewA2ATextPart constructs a text A2APart.
func NewA2ATextPart(text string) A2APart {
	return A2APart{Kind: "text", Text: text}
}

// NewA2ADataPart constructs a structured JSON data A2APart.
func NewA2ADataPart(data map[string]any) A2APart {
	return A2APart{Kind: "data", Data: data}
}

// A2AMessage is an A2A v0.3 conversational turn ("user" or "agent").
type A2AMessage struct {
	Kind      string         `json:"kind,omitempty"`
	MessageID string         `json:"messageId"`
	Role      string         `json:"role"`
	Parts     []A2APart      `json:"parts"`
	TaskID    string         `json:"taskId,omitempty"`
	ContextID string         `json:"contextId,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

// A2ATaskStatus is the status block of an A2A Task or status-update event.
type A2ATaskStatus struct {
	State     A2ATaskState `json:"state"`
	Message   *A2AMessage  `json:"message,omitempty"`
	Timestamp string       `json:"timestamp,omitempty"`
}

// A2AArtifact is a structured output artifact produced by an A2A Task.
type A2AArtifact struct {
	ArtifactID  string         `json:"artifactId"`
	Name        string         `json:"name,omitempty"`
	Description string         `json:"description,omitempty"`
	Parts       []A2APart      `json:"parts"`
	Metadata    map[string]any `json:"metadata,omitempty"`
}

// A2ATask is the A2A v0.3 Task wire object returned by message/send,
// tasks/get, and tasks/cancel.
type A2ATask struct {
	Kind      string         `json:"kind"`
	ID        string         `json:"id"`
	ContextID string         `json:"contextId,omitempty"`
	Status    A2ATaskStatus  `json:"status"`
	Artifacts []A2AArtifact  `json:"artifacts,omitempty"`
	History   []A2AMessage   `json:"history,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

// A2ASendMessageParams is the params payload for message/send and
// message/stream.
type A2ASendMessageParams struct {
	Message       A2AMessage                   `json:"message"`
	Configuration *A2ASendMessageConfiguration `json:"configuration,omitempty"`
	Metadata      map[string]any               `json:"metadata,omitempty"`
}

// A2ASendMessageConfiguration carries optional per-call execution settings.
type A2ASendMessageConfiguration struct {
	AcceptedOutputModes []string `json:"acceptedOutputModes,omitempty"`
	Blocking            *bool    `json:"blocking,omitempty"`
	HistoryLength       *int     `json:"historyLength,omitempty"`
}

// A2ATaskQueryParams is the params payload for tasks/get.
type A2ATaskQueryParams struct {
	ID            string         `json:"id"`
	HistoryLength *int           `json:"historyLength,omitempty"`
	Metadata      map[string]any `json:"metadata,omitempty"`
}

// A2ATaskIDParams is the params payload for tasks/cancel.
type A2ATaskIDParams struct {
	ID       string         `json:"id"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// A2ATaskStatusUpdateEvent is an SSE result frame of kind "status-update"
// emitted by message/stream.
type A2ATaskStatusUpdateEvent struct {
	Kind      string         `json:"kind"`
	TaskID    string         `json:"taskId"`
	ContextID string         `json:"contextId,omitempty"`
	Status    A2ATaskStatus  `json:"status"`
	Final     bool           `json:"final"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

// A2ATaskArtifactUpdateEvent is an SSE result frame of kind "artifact-update"
// emitted by message/stream.
type A2ATaskArtifactUpdateEvent struct {
	Kind      string         `json:"kind"`
	TaskID    string         `json:"taskId"`
	ContextID string         `json:"contextId,omitempty"`
	Artifact  A2AArtifact    `json:"artifact"`
	Append    bool           `json:"append,omitempty"`
	LastChunk bool           `json:"lastChunk,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

// jsonrpcRequest is the inbound JSON-RPC 2.0 envelope.
type jsonrpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// jsonrpcResponse is the outbound JSON-RPC 2.0 envelope.
type jsonrpcResponse struct {
	JSONRPC string        `json:"jsonrpc"`
	ID      any           `json:"id"`
	Result  any           `json:"result,omitempty"`
	Error   *jsonrpcError `json:"error,omitempty"`
}

// jsonrpcError is the JSON-RPC 2.0 error object.
type jsonrpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}
