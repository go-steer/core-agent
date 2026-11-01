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
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

const (
	// maxA2ASSEFrameBytes bounds the accumulated payload of a single
	// Server-Sent Event frame across one or more data: lines.
	maxA2ASSEFrameBytes = 8 << 20
)

var (
	// a2aMaxStreamArtifactBytes bounds the cumulative text and data bytes
	// held in memory across streaming artifact-update events for a single
	// task. Package-level var so tests can exercise the cap with small
	// payloads.
	a2aMaxStreamArtifactBytes = 8 << 20

	// a2aMaxStreamArtifacts bounds the number of distinct artifacts
	// accumulated during a single streaming task.
	a2aMaxStreamArtifacts = 256
)

func (h *a2aHandle) runStream(ctx context.Context, msg *A2AMessage, timeout time.Duration) {
	reqID := h.client.nextID.Add(1)
	body, err := json.Marshal(rpcRequest{
		JSONRPC: "2.0",
		ID:      reqID,
		Method:  methodMessageStream,
		Params:  messageSendParams{Message: msg},
	})
	if err != nil {
		h.finishFailed(ctx, fmt.Sprintf("a2a: %s: marshal %s: %v", h.client.cfg.Name, methodMessageStream, err))
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.endpoint, bytes.NewReader(body))
	if err != nil {
		h.finishFailed(ctx, fmt.Sprintf("a2a: %s: build %s request: %v", h.client.cfg.Name, methodMessageStream, err))
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set(A2AVersionHeader, A2AProtocolVersion)
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(req.Header))
	if err := h.client.setAuth(req); err != nil {
		h.finishFailed(ctx, err.Error())
		return
	}
	resp, err := h.client.http.Do(req)
	if err != nil {
		h.finishStreamTransportErr(ctx, timeout, err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		h.finishFailed(ctx, fmt.Sprintf("%v: %s: %s returned HTTP %d", ErrRemoteAuthFailed, h.client.cfg.Name, methodMessageStream, resp.StatusCode))
		return
	}
	if resp.StatusCode != http.StatusOK {
		h.finishFailed(ctx, fmt.Sprintf("a2a: %s: %s returned HTTP %d", h.client.cfg.Name, methodMessageStream, resp.StatusCode))
		return
	}

	var (
		artifacts     []A2AArtifact
		artifactBytes int
	)
	done, scanErr := scanSSEFrames(resp.Body, func(payload []byte) bool {
		return h.handleStreamFrame(ctx, reqID, payload, &artifacts, &artifactBytes)
	})
	if done {
		return
	}
	h.finishStreamEnd(ctx, timeout, scanErr)
}

// scanSSEFrames reads a WHATWG Server-Sent Events stream from r, joining
// consecutive `data:` lines within an event with "\n" and invoking onFrame only
// when a blank line dispatches the event. Comment lines (`:`) and non-data
// fields (`event:`, `id:`, `retry:`) are ignored; an unterminated event at EOF
// without a trailing blank line is not dispatched.
func scanSSEFrames(r io.Reader, onFrame func(payload []byte) bool) (bool, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), maxA2ASSEFrameBytes)

	var (
		dataBuf bytes.Buffer
		hasData bool
	)
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if line == "" {
			if !hasData {
				continue
			}
			payload := bytes.TrimSpace(dataBuf.Bytes())
			dataBuf.Reset()
			hasData = false
			if len(payload) == 0 {
				continue
			}
			frame := append([]byte(nil), payload...)
			if onFrame(frame) {
				return true, nil
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, val, hasColon := strings.Cut(line, ":")
		if !hasColon {
			field = line
			val = ""
		} else {
			val = strings.TrimPrefix(val, " ")
		}
		if field != "data" {
			continue
		}
		if hasData {
			dataBuf.WriteByte('\n')
		}
		if dataBuf.Len()+len(val) > maxA2ASSEFrameBytes {
			return false, fmt.Errorf("SSE frame exceeds %d bytes", maxA2ASSEFrameBytes)
		}
		dataBuf.WriteString(val)
		hasData = true
	}
	return false, scanner.Err()
}

func (h *a2aHandle) finishStreamTransportErr(ctx context.Context, timeout time.Duration, err error) {
	if h.isStopped() {
		h.sendCancelOnce(ctx)
		h.finishStopped(ctx)
		return
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		h.abandonFailed(ctx, fmt.Sprintf("%v: %s: %s timed out after %s", ErrRemoteTimeout, h.client.cfg.Name, methodMessageStream, timeout))
		return
	}
	h.abandonFailed(ctx, fmt.Errorf("%w: %s: %s: %v", ErrRemoteUnreachable, h.client.cfg.Name, methodMessageStream, err).Error())
}

func (h *a2aHandle) handleStreamFrame(
	ctx context.Context,
	reqID int64,
	payload []byte,
	artifacts *[]A2AArtifact,
	artifactBytes *int,
) bool {
	var rpcResp rpcResponse
	if err := json.Unmarshal(payload, &rpcResp); err != nil {
		h.abandonFailed(ctx, fmt.Sprintf("a2a: %s: parse stream frame: %v", h.client.cfg.Name, err))
		return true
	}
	if err := checkRPCResponseID(reqID, rpcResp.ID, rpcResp.Error != nil); err != nil {
		h.abandonFailed(ctx, fmt.Sprintf("a2a: %s: %s: %v", h.client.cfg.Name, methodMessageStream, err))
		return true
	}
	if rpcResp.Error != nil {
		h.abandonFailed(ctx, fmt.Sprintf("a2a: %s: %s: %v", h.client.cfg.Name, methodMessageStream, rpcResp.Error))
		return true
	}
	var probe struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(rpcResp.Result, &probe); err != nil {
		h.abandonFailed(ctx, fmt.Sprintf("a2a: %s: malformed stream result: %v", h.client.cfg.Name, err))
		return true
	}
	switch probe.Kind {
	case "message":
		var m A2AMessage
		if err := json.Unmarshal(rpcResp.Result, &m); err != nil {
			h.abandonFailed(ctx, fmt.Sprintf("a2a: %s: malformed stream message: %v", h.client.cfg.Name, err))
			return true
		}
		h.markRemoteTerminal()
		h.finishCompleted(ctx, textOrJSON(resultFromA2AParts(m.Parts, nil)))
		return true
	case "task":
		return h.handleStreamTask(ctx, rpcResp.Result, artifacts, artifactBytes)
	case "artifact-update":
		return h.handleStreamArtifactUpdate(ctx, rpcResp.Result, artifacts, artifactBytes)
	case "status-update":
		return h.handleStreamStatusUpdate(ctx, rpcResp.Result, *artifacts)
	default:
		h.abandonFailed(ctx, fmt.Sprintf("a2a: %s: unknown stream event kind %q", h.client.cfg.Name, probe.Kind))
		return true
	}
}

func (h *a2aHandle) handleStreamTask(
	ctx context.Context,
	raw json.RawMessage,
	artifacts *[]A2AArtifact,
	artifactBytes *int,
) bool {
	var task A2ATask
	if err := json.Unmarshal(raw, &task); err != nil {
		h.abandonFailed(ctx, fmt.Sprintf("a2a: %s: malformed stream task: %v", h.client.cfg.Name, err))
		return true
	}
	if task.Status.State.Terminal() {
		h.markRemoteTerminal()
	}
	h.setTaskID(task.ID)
	for _, art := range task.Artifacts {
		if err := mergeArtifactChunk(artifacts, art, false, artifactBytes); err != nil {
			h.abandonFailed(ctx, fmt.Sprintf("a2a: %s: %v", h.client.cfg.Name, err))
			return true
		}
	}
	if task.Status.State == A2ATaskStateInputRequired || task.Status.State == A2ATaskStateAuthRequired {
		h.abandonFailed(ctx, fmt.Sprintf("%v: %s: task %s entered %q; task canceled", ErrRemoteFailed, h.client.cfg.Name, task.ID, task.Status.State))
		return true
	}
	if task.Status.State.Terminal() {
		h.finishTask(ctx, &task, *artifacts)
		return true
	}
	if task.Status.Message != nil {
		h.emitAlert(ctx, textOfA2AParts(task.Status.Message.Parts))
	}
	return false
}

func (h *a2aHandle) handleStreamArtifactUpdate(
	ctx context.Context,
	raw json.RawMessage,
	artifacts *[]A2AArtifact,
	artifactBytes *int,
) bool {
	var au A2ATaskArtifactUpdateEvent
	if err := json.Unmarshal(raw, &au); err != nil {
		h.abandonFailed(ctx, fmt.Sprintf("a2a: %s: malformed artifact-update: %v", h.client.cfg.Name, err))
		return true
	}
	h.setTaskID(au.TaskID)
	if err := mergeArtifactChunk(artifacts, au.Artifact, au.Append, artifactBytes); err != nil {
		h.abandonFailed(ctx, fmt.Sprintf("a2a: %s: %v", h.client.cfg.Name, err))
		return true
	}
	return false
}

func (h *a2aHandle) handleStreamStatusUpdate(ctx context.Context, raw json.RawMessage, artifacts []A2AArtifact) bool {
	var su A2ATaskStatusUpdateEvent
	if err := json.Unmarshal(raw, &su); err != nil {
		h.abandonFailed(ctx, fmt.Sprintf("a2a: %s: malformed status-update: %v", h.client.cfg.Name, err))
		return true
	}
	if su.Status.State.Terminal() {
		h.markRemoteTerminal()
	}
	h.setTaskID(su.TaskID)
	if su.Status.State == A2ATaskStateInputRequired || su.Status.State == A2ATaskStateAuthRequired {
		h.abandonFailed(ctx, fmt.Sprintf("%v: %s: task %s entered %q; task canceled", ErrRemoteFailed, h.client.cfg.Name, su.TaskID, su.Status.State))
		return true
	}
	if su.Status.State.Terminal() {
		h.finishTask(ctx, &A2ATask{ID: su.TaskID, Status: su.Status}, artifacts)
		return true
	}
	if su.Final {
		h.abandonFailed(ctx, fmt.Sprintf("%v: %s: task %s final status-update in non-terminal state %q; task canceled",
			ErrRemoteFailed, h.client.cfg.Name, su.TaskID, su.Status.State))
		return true
	}
	if su.Status.Message != nil {
		h.emitAlert(ctx, textOfA2AParts(su.Status.Message.Parts))
	}
	return false
}

func (h *a2aHandle) finishStreamEnd(ctx context.Context, timeout time.Duration, scanErr error) {
	taskID := h.getTaskID()
	if h.isStopped() {
		h.sendCancelOnce(ctx)
		h.finishStopped(ctx)
		return
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		h.abandonFailed(ctx, fmt.Sprintf("%v: %s: task %s timed out after %s; tasks/cancel sent", ErrRemoteTimeout, h.client.cfg.Name, taskID, timeout))
		return
	}
	if ctx.Err() != nil {
		h.abandonFailed(ctx, fmt.Sprintf("a2a: %s: stream canceled: %v", h.client.cfg.Name, ctx.Err()))
		return
	}
	if scanErr != nil {
		h.abandonFailed(ctx, fmt.Sprintf("a2a: %s: read stream: %v", h.client.cfg.Name, scanErr))
		return
	}
	h.abandonFailed(ctx, fmt.Sprintf("%v: %s: stream closed before task %s reached a terminal state", ErrRemoteFailed, h.client.cfg.Name, taskID))
}

func mergeArtifactChunk(dst *[]A2AArtifact, incoming A2AArtifact, appendMode bool, totalBytes *int) error {
	chunkBytes := artifactPayloadBytes(incoming)
	if appendMode && incoming.ArtifactID != "" {
		for i := range *dst {
			if (*dst)[i].ArtifactID != incoming.ArtifactID {
				continue
			}
			if *totalBytes+chunkBytes > a2aMaxStreamArtifactBytes {
				return fmt.Errorf("streaming artifacts exceed %d bytes", a2aMaxStreamArtifactBytes)
			}
			*totalBytes += chunkBytes
			mergeArtifactParts(&(*dst)[i], incoming)
			return nil
		}
	}
	if !appendMode && incoming.ArtifactID != "" {
		for i := range *dst {
			if (*dst)[i].ArtifactID != incoming.ArtifactID {
				continue
			}
			prevBytes := artifactPayloadBytes((*dst)[i])
			if *totalBytes-prevBytes+chunkBytes > a2aMaxStreamArtifactBytes {
				return fmt.Errorf("streaming artifacts exceed %d bytes", a2aMaxStreamArtifactBytes)
			}
			*totalBytes = *totalBytes - prevBytes + chunkBytes
			(*dst)[i] = cloneArtifact(incoming)
			return nil
		}
	}
	if len(*dst) >= a2aMaxStreamArtifacts {
		return fmt.Errorf("streaming artifact count exceeds %d", a2aMaxStreamArtifacts)
	}
	if *totalBytes+chunkBytes > a2aMaxStreamArtifactBytes {
		return fmt.Errorf("streaming artifacts exceed %d bytes", a2aMaxStreamArtifactBytes)
	}
	*totalBytes += chunkBytes
	*dst = append(*dst, cloneArtifact(incoming))
	return nil
}

func mergeArtifactParts(dst *A2AArtifact, incoming A2AArtifact) {
	if dst.Name == "" {
		dst.Name = incoming.Name
	}
	if dst.Description == "" {
		dst.Description = incoming.Description
	}
	for _, p := range incoming.Parts {
		if p.Kind == "text" {
			if ti := lastTextPartIndex(dst.Parts); ti >= 0 {
				dst.Parts[ti].Text += p.Text
				continue
			}
		}
		dst.Parts = append(dst.Parts, clonePart(p))
	}
}

func lastTextPartIndex(parts []A2APart) int {
	for i := len(parts) - 1; i >= 0; i-- {
		if parts[i].Kind == "text" {
			return i
		}
	}
	return -1
}

func cloneArtifact(in A2AArtifact) A2AArtifact {
	out := in
	if len(in.Parts) > 0 {
		out.Parts = make([]A2APart, len(in.Parts))
		for i, p := range in.Parts {
			out.Parts[i] = clonePart(p)
		}
	}
	return out
}

func clonePart(in A2APart) A2APart {
	out := in
	if len(in.Data) > 0 {
		out.Data = make(map[string]any, len(in.Data))
		for k, v := range in.Data {
			out.Data[k] = v
		}
	}
	return out
}

func artifactPayloadBytes(a A2AArtifact) int {
	n := 0
	for _, p := range a.Parts {
		n += len(p.Text)
		if len(p.Data) > 0 {
			if b, err := json.Marshal(p.Data); err == nil {
				n += len(b)
			}
		}
	}
	return n
}
