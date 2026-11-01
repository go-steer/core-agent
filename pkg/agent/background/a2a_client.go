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
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

// Compile-time check that *A2AClient implements RemoteAgentSpawner.
var _ RemoteAgentSpawner = (*A2AClient)(nil)

// A2AClient is an A2A v0.3 client and RemoteAgentSpawner for one
// configured remote agent. Safe for concurrent use.
type A2AClient struct {
	cfg          A2AConfig
	http         *http.Client
	pollInterval time.Duration
	nextID       atomic.Int64

	cardMu   sync.Mutex
	card     *A2ACard
	endpoint string
}

// A2AClientOption customizes an A2AClient.
type A2AClientOption func(*A2AClient)

// WithA2AHTTPClient substitutes the HTTP client used for card and
// JSON-RPC requests.
func WithA2AHTTPClient(h *http.Client) A2AClientOption {
	return func(c *A2AClient) { c.http = h }
}

// WithA2APollInterval overrides the tasks/get polling cadence.
func WithA2APollInterval(d time.Duration) A2AClientOption {
	return func(c *A2AClient) { c.pollInterval = d }
}

// NewA2AClient validates cfg and returns an A2AClient. Card discovery
// is lazy and occurs on first use.
func NewA2AClient(cfg A2AConfig, opts ...A2AClientOption) (*A2AClient, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	c := &A2AClient{
		cfg:          cfg,
		http:         &http.Client{},
		pollInterval: 500 * time.Millisecond,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c, nil
}

// Name returns the configured remote agent name.
func (c *A2AClient) Name() string { return c.cfg.Name }

// Card returns the remote agent card, fetching and caching it on first
// use. Endpoint-only configs (no AgentCardURL) return (nil, nil).
func (c *A2AClient) Card(ctx context.Context) (*A2ACard, error) {
	c.cardMu.Lock()
	defer c.cardMu.Unlock()
	return c.cardLocked(ctx)
}

func (c *A2AClient) cardLocked(ctx context.Context) (*A2ACard, error) {
	if c.card != nil {
		return c.card, nil
	}
	if strings.TrimSpace(c.cfg.AgentCardURL) == "" {
		return nil, nil
	}
	cardURL, err := resolveA2ACardURL(c.cfg.AgentCardURL)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cardURL, nil)
	if err != nil {
		return nil, fmt.Errorf("a2a: %s: build card request: %w", c.cfg.Name, err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set(A2AVersionHeader, A2AProtocolVersion)
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(req.Header))
	if err := c.setAuth(req); err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("%w: %s: fetch agent card %s timed out: %v", ErrRemoteTimeout, c.cfg.Name, cardURL, err)
		}
		if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
			return nil, fmt.Errorf("a2a: %s: fetch agent card %s canceled: %w", c.cfg.Name, cardURL, err)
		}
		return nil, fmt.Errorf("%w: %s: fetch agent card %s: %v", ErrRemoteUnreachable, c.cfg.Name, cardURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("%w: %s: agent card fetch returned HTTP %d", ErrRemoteAuthFailed, c.cfg.Name, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("a2a: %s: agent card fetch %s returned HTTP %d", c.cfg.Name, cardURL, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("a2a: %s: read agent card: %w", c.cfg.Name, err)
	}
	card := &A2ACard{}
	if err := json.Unmarshal(body, card); err != nil {
		return nil, fmt.Errorf("a2a: %s: parse agent card: %w", c.cfg.Name, err)
	}
	c.card = card
	return card, nil
}

func resolveA2ACardURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("a2a: agent_card_url %q: %w", raw, err)
	}
	if u.Path == "" || u.Path == "/" {
		u.Path = A2AWellKnownCardPath
	}
	return u.String(), nil
}

func (c *A2AClient) endpointURL(ctx context.Context) (string, error) {
	c.cardMu.Lock()
	defer c.cardMu.Unlock()
	if c.endpoint != "" {
		return c.endpoint, nil
	}
	if ep := strings.TrimSpace(c.cfg.Endpoint); ep != "" {
		c.endpoint = ep
		return c.endpoint, nil
	}
	card, err := c.cardLocked(ctx)
	if err != nil {
		return "", err
	}
	if card.PreferredTransport == "" || card.PreferredTransport == A2ATransportJSONRPC {
		if card.URL == "" {
			return "", fmt.Errorf("a2a: %s: agent card has no url", c.cfg.Name)
		}
		c.endpoint = card.URL
		return c.endpoint, nil
	}
	for _, iface := range card.AdditionalInterfaces {
		if iface.Transport == A2ATransportJSONRPC && iface.URL != "" {
			c.endpoint = iface.URL
			return c.endpoint, nil
		}
	}
	return "", fmt.Errorf("%w: %s: agent card offers no JSONRPC interface (preferredTransport=%q)",
		ErrRemoteProtocolMismatch, c.cfg.Name, card.PreferredTransport)
}

func (c *A2AClient) setAuth(req *http.Request) error {
	if c.cfg.Auth == nil {
		return nil
	}
	token := os.Getenv(c.cfg.Auth.TokenEnv)
	if token == "" {
		return fmt.Errorf("%w: %s: auth.token_env %s is not set in the environment", ErrRemoteAuthFailed, c.cfg.Name, c.cfg.Auth.TokenEnv)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return nil
}

func (c *A2AClient) checkSkill(ctx context.Context, skill string) error {
	if skill == "" {
		return nil
	}
	if len(c.cfg.Skills) > 0 {
		allowed := false
		for _, s := range c.cfg.Skills {
			if s == skill {
				allowed = true
				break
			}
		}
		if !allowed {
			return fmt.Errorf("a2a: %s: skill %q is not in the configured allowlist %v", c.cfg.Name, skill, c.cfg.Skills)
		}
	}
	card, err := c.Card(ctx)
	if err != nil || card == nil {
		return err
	}
	for _, s := range card.Skills {
		if s.ID == skill {
			return nil
		}
	}
	ids := make([]string, 0, len(card.Skills))
	for _, s := range card.Skills {
		ids = append(ids, s.ID)
	}
	return fmt.Errorf("a2a: %s: skill %q not present in agent card (card skills: %s)", c.cfg.Name, skill, strings.Join(ids, ", "))
}

// Send invokes skill on the remote agent with inputs and blocks until
// a terminal state or the bounded timeout. If ctx is canceled or the
// timeout expires mid-task, Send issues tasks/cancel best-effort.
func (c *A2AClient) Send(ctx context.Context, skill string, inputs map[string]any, timeout time.Duration) (*A2AResult, error) {
	if timeout <= 0 {
		timeout = c.cfg.Timeout()
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if err := c.checkSkill(ctx, skill); err != nil {
		return nil, err
	}
	endpoint, err := c.endpointURL(ctx)
	if err != nil {
		return nil, err
	}
	msg := &A2AMessage{
		Kind:      "message",
		MessageID: newA2AID(),
		Role:      "user",
		Parts:     []A2APart{{Kind: "data", Data: inputs}},
	}
	if skill != "" {
		msg.Metadata = map[string]any{"skillId": skill}
	}
	blocking := true
	raw, err := c.call(ctx, endpoint, methodMessageSend, messageSendParams{
		Message:       msg,
		Configuration: &messageSendConfig{Blocking: &blocking},
	})
	if err != nil {
		return nil, err
	}
	reply, err := decodeSendReply(raw)
	if err != nil {
		return nil, err
	}
	if reply.Message != nil {
		res := resultFromA2AParts(reply.Message.Parts, nil)
		res.State = string(A2ATaskStateCompleted)
		res.Raw = reply.Raw
		return res, nil
	}
	return c.pollTaskToTerminal(ctx, endpoint, reply.Task, reply.Raw, timeout)
}

func (c *A2AClient) pollTaskToTerminal(ctx context.Context, endpoint string, task *A2ATask, lastRaw json.RawMessage, timeout time.Duration) (*A2AResult, error) {
	for !task.Status.State.Terminal() {
		if task.Status.State == A2ATaskStateInputRequired || task.Status.State == A2ATaskStateAuthRequired {
			c.cancelTask(ctx, endpoint, task.ID)
			return nil, fmt.Errorf("%w: %s: task %s entered %q, which synchronous A2A calls cannot service; task canceled",
				ErrRemoteFailed, c.cfg.Name, task.ID, task.Status.State)
		}
		select {
		case <-ctx.Done():
			c.cancelTask(ctx, endpoint, task.ID)
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return nil, fmt.Errorf("%w: %s: task %s still %q after %s; tasks/cancel sent",
					ErrRemoteTimeout, c.cfg.Name, task.ID, task.Status.State, timeout)
			}
			return nil, fmt.Errorf("%s: task %s canceled by caller; tasks/cancel sent: %w", c.cfg.Name, task.ID, context.Cause(ctx))
		case <-time.After(c.pollInterval):
		}
		raw, err := c.call(ctx, endpoint, methodTasksGet, taskQueryParams{ID: task.ID})
		if err != nil {
			if ctx.Err() != nil {
				c.cancelTask(ctx, endpoint, task.ID)
			}
			return nil, err
		}
		polled := &A2ATask{}
		if err := json.Unmarshal(raw, polled); err != nil {
			return nil, fmt.Errorf("a2a: %s: malformed tasks/get result: %w", c.cfg.Name, err)
		}
		polled.Kind = "task"
		task = polled
		lastRaw = raw
	}
	if task.Status.State == A2ATaskStateCompleted {
		var statusParts []A2APart
		if task.Status.Message != nil {
			statusParts = task.Status.Message.Parts
		}
		res := resultFromA2AParts(statusParts, task.Artifacts)
		res.State = string(task.Status.State)
		res.RemoteID = task.ID
		res.Raw = lastRaw
		return res, nil
	}
	detail := ""
	if task.Status.Message != nil {
		if txt := textOfA2AParts(task.Status.Message.Parts); txt != "" {
			detail = ": " + txt
		}
	}
	return nil, fmt.Errorf("%w: %s: task %s ended %q%s", ErrRemoteFailed, c.cfg.Name, task.ID, task.Status.State, detail)
}

// Spawn implements RemoteAgentSpawner. It validates auth, skill, and
// endpoint discovery up front, then launches a background goroutine
// that runs message/stream (when the remote card advertises streaming)
// or message/send + tasks/get polling, feeding events into the returned
// RemoteAgentHandle.
func (c *A2AClient) Spawn(ctx context.Context, spec RemoteAgentSpec) (RemoteAgentHandle, error) {
	if c.cfg.Auth != nil && os.Getenv(c.cfg.Auth.TokenEnv) == "" {
		return nil, fmt.Errorf("%w: %s: auth.token_env %s is not set in the environment", ErrRemoteAuthFailed, c.cfg.Name, c.cfg.Auth.TokenEnv)
	}
	timeout := c.cfg.Timeout()
	if spec.Budgets.MaxWallclock > 0 && spec.Budgets.MaxWallclock < timeout {
		timeout = spec.Budgets.MaxWallclock
	}
	preCtx, preCancel := context.WithTimeout(ctx, timeout)
	defer preCancel()

	skill := strings.TrimSpace(c.cfg.Skill)
	if err := c.checkSkill(preCtx, skill); err != nil {
		return nil, err
	}
	card, err := c.Card(preCtx)
	if err != nil {
		return nil, err
	}
	endpoint, err := c.endpointURL(preCtx)
	if err != nil {
		return nil, err
	}

	var parts []A2APart
	if sp := strings.TrimSpace(spec.SystemPrompt); sp != "" {
		parts = append(parts, A2APart{Kind: "text", Text: sp})
	}
	parts = append(parts, A2APart{Kind: "text", Text: spec.Goal})
	msg := &A2AMessage{
		Kind:      "message",
		MessageID: newA2AID(),
		Role:      "user",
		Parts:     parts,
	}
	if skill != "" {
		msg.Metadata = map[string]any{"skillId": skill}
	}

	handleID := spec.ID
	if handleID == "" {
		handleID = spec.Name
	}
	if handleID == "" {
		handleID = c.cfg.Name
	}

	runCtx, cancel := context.WithTimeout(contextWithoutCancel(ctx), timeout)
	h := &a2aHandle{
		client:   c,
		endpoint: endpoint,
		events:   make(chan RemoteAgentEvent, 32),
		id:       handleID,
		status:   RemoteStatusRunning,
		cancel:   cancel,
	}
	streaming := card != nil && card.Capabilities.Streaming
	go h.run(runCtx, cancel, msg, streaming, timeout)
	return h, nil
}

type a2aHandle struct {
	client   *A2AClient
	endpoint string
	events   chan RemoteAgentEvent

	mu         sync.Mutex
	id         string
	taskID     string
	status     RemoteAgentStatus
	stopped    bool
	cancelSent bool
	cancel     context.CancelFunc
}

func (h *a2aHandle) ID() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.id
}

func (h *a2aHandle) Status(_ context.Context) (RemoteAgentStatus, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.status, nil
}

func (h *a2aHandle) Events() <-chan RemoteAgentEvent {
	return h.events
}

func (h *a2aHandle) Stop(ctx context.Context) error {
	h.mu.Lock()
	h.stopped = true
	if h.status == RemoteStatusPending || h.status == RemoteStatusRunning {
		h.status = RemoteStatusStopped
	}
	cancel := h.cancel
	h.mu.Unlock()
	h.sendCancelOnce(ctx)
	if cancel != nil {
		cancel()
	}
	return nil
}

func (h *a2aHandle) sendCancelOnce(ctx context.Context) {
	h.mu.Lock()
	if h.cancelSent || h.taskID == "" {
		h.mu.Unlock()
		return
	}
	h.cancelSent = true
	taskID := h.taskID
	h.mu.Unlock()
	h.client.cancelTask(ctx, h.endpoint, taskID)
}

func (h *a2aHandle) setTaskID(id string) {
	if id == "" {
		return
	}
	h.mu.Lock()
	h.taskID = id
	stopped := h.stopped
	h.mu.Unlock()
	if stopped {
		h.sendCancelOnce(context.Background())
	}
}

func (h *a2aHandle) getTaskID() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.taskID
}

func (h *a2aHandle) isStopped() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.stopped
}

func (h *a2aHandle) emitAlert(text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	h.events <- RemoteAgentEvent{
		Kind:      "alert",
		Text:      text,
		Timestamp: time.Now(),
	}
}

func (h *a2aHandle) finishCompleted(text string) {
	h.mu.Lock()
	if h.stopped {
		h.status = RemoteStatusStopped
		h.mu.Unlock()
		h.events <- RemoteAgentEvent{
			Kind:      "stopped",
			Text:      "stopped by parent",
			Timestamp: time.Now(),
		}
		return
	}
	if h.status == RemoteStatusPending || h.status == RemoteStatusRunning {
		h.status = RemoteStatusCompleted
	}
	h.mu.Unlock()
	h.events <- RemoteAgentEvent{
		Kind:      "completed",
		Text:      text,
		Timestamp: time.Now(),
	}
}

func (h *a2aHandle) finishFailed(text string) {
	h.mu.Lock()
	if h.stopped {
		h.status = RemoteStatusStopped
		h.mu.Unlock()
		h.events <- RemoteAgentEvent{
			Kind:      "stopped",
			Text:      "stopped by parent",
			Timestamp: time.Now(),
		}
		return
	}
	if h.status == RemoteStatusPending || h.status == RemoteStatusRunning {
		h.status = RemoteStatusFailed
	}
	h.mu.Unlock()
	h.events <- RemoteAgentEvent{
		Kind:      "failed",
		Text:      text,
		Timestamp: time.Now(),
	}
}

func (h *a2aHandle) finishStopped() {
	h.mu.Lock()
	h.status = RemoteStatusStopped
	h.mu.Unlock()
	h.events <- RemoteAgentEvent{
		Kind:      "stopped",
		Text:      "stopped by parent",
		Timestamp: time.Now(),
	}
}

func (h *a2aHandle) finishTask(task *A2ATask, artifacts []A2AArtifact) {
	switch task.Status.State {
	case A2ATaskStateCompleted:
		var statusParts []A2APart
		if task.Status.Message != nil {
			statusParts = task.Status.Message.Parts
		}
		res := resultFromA2AParts(statusParts, artifacts)
		h.finishCompleted(textOrJSON(res))
	case A2ATaskStateCanceled:
		if h.isStopped() {
			h.finishStopped()
			return
		}
		detail := taskStatusDetail(task.Status)
		h.finishFailed(fmt.Sprintf("%v: %s: task %s ended %q%s", ErrRemoteFailed, h.client.cfg.Name, task.ID, task.Status.State, detail))
	default:
		detail := taskStatusDetail(task.Status)
		h.finishFailed(fmt.Sprintf("%v: %s: task %s ended %q%s", ErrRemoteFailed, h.client.cfg.Name, task.ID, task.Status.State, detail))
	}
}

func taskStatusDetail(st A2ATaskStatus) string {
	if st.Message == nil {
		return ""
	}
	if txt := textOfA2AParts(st.Message.Parts); txt != "" {
		return ": " + txt
	}
	return ""
}

func (h *a2aHandle) run(runCtx context.Context, cancel context.CancelFunc, msg *A2AMessage, streaming bool, timeout time.Duration) {
	defer close(h.events)
	defer cancel()
	if streaming {
		h.runStream(runCtx, msg, timeout)
		return
	}
	h.runPoll(runCtx, msg, timeout)
}

func (h *a2aHandle) runStream(ctx context.Context, msg *A2AMessage, timeout time.Duration) {
	body, err := json.Marshal(rpcRequest{
		JSONRPC: "2.0",
		ID:      h.client.nextID.Add(1),
		Method:  methodMessageStream,
		Params:  messageSendParams{Message: msg},
	})
	if err != nil {
		h.finishFailed(fmt.Sprintf("a2a: %s: marshal %s: %v", h.client.cfg.Name, methodMessageStream, err))
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.endpoint, bytes.NewReader(body))
	if err != nil {
		h.finishFailed(fmt.Sprintf("a2a: %s: build %s request: %v", h.client.cfg.Name, methodMessageStream, err))
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set(A2AVersionHeader, A2AProtocolVersion)
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(req.Header))
	if err := h.client.setAuth(req); err != nil {
		h.finishFailed(err.Error())
		return
	}
	resp, err := h.client.http.Do(req)
	if err != nil {
		h.finishStreamTransportErr(ctx, timeout, err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		h.finishFailed(fmt.Sprintf("%v: %s: %s returned HTTP %d", ErrRemoteAuthFailed, h.client.cfg.Name, methodMessageStream, resp.StatusCode))
		return
	}
	if resp.StatusCode != http.StatusOK {
		h.finishFailed(fmt.Sprintf("a2a: %s: %s returned HTTP %d", h.client.cfg.Name, methodMessageStream, resp.StatusCode))
		return
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 8<<20)
	var artifacts []A2AArtifact
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" {
			continue
		}
		done := h.handleStreamFrame(ctx, []byte(payload), &artifacts)
		if done {
			return
		}
	}
	h.finishStreamEnd(ctx, timeout, scanner.Err())
}

func (h *a2aHandle) finishStreamTransportErr(ctx context.Context, timeout time.Duration, err error) {
	if h.isStopped() {
		h.finishStopped()
		return
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		h.finishFailed(fmt.Sprintf("%v: %s: %s timed out after %s", ErrRemoteTimeout, h.client.cfg.Name, methodMessageStream, timeout))
		return
	}
	h.finishFailed(fmt.Errorf("%w: %s: %s: %v", ErrRemoteUnreachable, h.client.cfg.Name, methodMessageStream, err).Error())
}

func (h *a2aHandle) handleStreamFrame(ctx context.Context, payload []byte, artifacts *[]A2AArtifact) bool {
	var rpcResp rpcResponse
	if err := json.Unmarshal(payload, &rpcResp); err != nil {
		h.finishFailed(fmt.Sprintf("a2a: %s: parse stream frame: %v", h.client.cfg.Name, err))
		return true
	}
	if rpcResp.Error != nil {
		h.finishFailed(fmt.Sprintf("a2a: %s: %s: %v", h.client.cfg.Name, methodMessageStream, rpcResp.Error))
		return true
	}
	var probe struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(rpcResp.Result, &probe); err != nil {
		h.finishFailed(fmt.Sprintf("a2a: %s: malformed stream result: %v", h.client.cfg.Name, err))
		return true
	}
	switch probe.Kind {
	case "message":
		var m A2AMessage
		if err := json.Unmarshal(rpcResp.Result, &m); err != nil {
			h.finishFailed(fmt.Sprintf("a2a: %s: malformed stream message: %v", h.client.cfg.Name, err))
			return true
		}
		h.finishCompleted(textOrJSON(resultFromA2AParts(m.Parts, nil)))
		return true
	case "task":
		return h.handleStreamTask(ctx, rpcResp.Result, artifacts)
	case "artifact-update":
		var au A2ATaskArtifactUpdateEvent
		if err := json.Unmarshal(rpcResp.Result, &au); err != nil {
			h.finishFailed(fmt.Sprintf("a2a: %s: malformed artifact-update: %v", h.client.cfg.Name, err))
			return true
		}
		h.setTaskID(au.TaskID)
		*artifacts = append(*artifacts, au.Artifact)
		return false
	case "status-update":
		return h.handleStreamStatusUpdate(ctx, rpcResp.Result, *artifacts)
	default:
		h.finishFailed(fmt.Sprintf("a2a: %s: unknown stream event kind %q", h.client.cfg.Name, probe.Kind))
		return true
	}
}

func (h *a2aHandle) handleStreamTask(ctx context.Context, raw json.RawMessage, artifacts *[]A2AArtifact) bool {
	var task A2ATask
	if err := json.Unmarshal(raw, &task); err != nil {
		h.finishFailed(fmt.Sprintf("a2a: %s: malformed stream task: %v", h.client.cfg.Name, err))
		return true
	}
	h.setTaskID(task.ID)
	if len(task.Artifacts) > 0 {
		*artifacts = append(*artifacts, task.Artifacts...)
	}
	if task.Status.State == A2ATaskStateInputRequired || task.Status.State == A2ATaskStateAuthRequired {
		h.sendCancelOnce(ctx)
		h.finishFailed(fmt.Sprintf("%v: %s: task %s entered %q; task canceled", ErrRemoteFailed, h.client.cfg.Name, task.ID, task.Status.State))
		return true
	}
	if task.Status.State.Terminal() {
		h.finishTask(&task, *artifacts)
		return true
	}
	if task.Status.Message != nil {
		h.emitAlert(textOfA2AParts(task.Status.Message.Parts))
	}
	return false
}

func (h *a2aHandle) handleStreamStatusUpdate(ctx context.Context, raw json.RawMessage, artifacts []A2AArtifact) bool {
	var su A2ATaskStatusUpdateEvent
	if err := json.Unmarshal(raw, &su); err != nil {
		h.finishFailed(fmt.Sprintf("a2a: %s: malformed status-update: %v", h.client.cfg.Name, err))
		return true
	}
	h.setTaskID(su.TaskID)
	if su.Status.State == A2ATaskStateInputRequired || su.Status.State == A2ATaskStateAuthRequired {
		h.sendCancelOnce(ctx)
		h.finishFailed(fmt.Sprintf("%v: %s: task %s entered %q; task canceled", ErrRemoteFailed, h.client.cfg.Name, su.TaskID, su.Status.State))
		return true
	}
	if su.Final || su.Status.State.Terminal() {
		h.finishTask(&A2ATask{ID: su.TaskID, Status: su.Status}, artifacts)
		return true
	}
	if su.Status.Message != nil {
		h.emitAlert(textOfA2AParts(su.Status.Message.Parts))
	}
	return false
}

func (h *a2aHandle) finishStreamEnd(ctx context.Context, timeout time.Duration, scanErr error) {
	taskID := h.getTaskID()
	if h.isStopped() {
		h.sendCancelOnce(ctx)
		h.finishStopped()
		return
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		h.sendCancelOnce(ctx)
		h.finishFailed(fmt.Sprintf("%v: %s: task %s timed out after %s; tasks/cancel sent", ErrRemoteTimeout, h.client.cfg.Name, taskID, timeout))
		return
	}
	if ctx.Err() != nil {
		h.sendCancelOnce(ctx)
		h.finishFailed(fmt.Sprintf("a2a: %s: stream canceled: %v", h.client.cfg.Name, ctx.Err()))
		return
	}
	if scanErr != nil {
		h.finishFailed(fmt.Sprintf("a2a: %s: read stream: %v", h.client.cfg.Name, scanErr))
		return
	}
	h.sendCancelOnce(ctx)
	h.finishFailed(fmt.Sprintf("%v: %s: stream closed before task %s reached a terminal state", ErrRemoteFailed, h.client.cfg.Name, taskID))
}

func (h *a2aHandle) runPoll(ctx context.Context, msg *A2AMessage, timeout time.Duration) {
	blocking := true
	raw, err := h.client.call(ctx, h.endpoint, methodMessageSend, messageSendParams{
		Message:       msg,
		Configuration: &messageSendConfig{Blocking: &blocking},
	})
	if err != nil {
		if h.isStopped() {
			h.finishStopped()
			return
		}
		h.finishFailed(err.Error())
		return
	}
	reply, err := decodeSendReply(raw)
	if err != nil {
		h.finishFailed(err.Error())
		return
	}
	if reply.Message != nil {
		res := resultFromA2AParts(reply.Message.Parts, nil)
		h.finishCompleted(textOrJSON(res))
		return
	}
	h.setTaskID(reply.Task.ID)
	h.pollHandleLoop(ctx, reply.Task, timeout)
}

func (h *a2aHandle) pollHandleLoop(ctx context.Context, task *A2ATask, timeout time.Duration) {
	lastAlert := ""
	for !task.Status.State.Terminal() {
		if task.Status.State == A2ATaskStateInputRequired || task.Status.State == A2ATaskStateAuthRequired {
			h.sendCancelOnce(ctx)
			h.finishFailed(fmt.Sprintf("%v: %s: task %s entered %q; task canceled", ErrRemoteFailed, h.client.cfg.Name, task.ID, task.Status.State))
			return
		}
		if task.Status.Message != nil {
			if txt := textOfA2AParts(task.Status.Message.Parts); txt != "" && txt != lastAlert {
				lastAlert = txt
				h.emitAlert(txt)
			}
		}
		select {
		case <-ctx.Done():
			h.sendCancelOnce(ctx)
			if h.isStopped() {
				h.finishStopped()
				return
			}
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				h.finishFailed(fmt.Sprintf("%v: %s: task %s still %q after %s; tasks/cancel sent",
					ErrRemoteTimeout, h.client.cfg.Name, task.ID, task.Status.State, timeout))
				return
			}
			h.finishFailed(fmt.Sprintf("a2a: %s: task %s canceled; tasks/cancel sent: %v", h.client.cfg.Name, task.ID, ctx.Err()))
			return
		case <-time.After(h.client.pollInterval):
		}
		raw, err := h.client.call(ctx, h.endpoint, methodTasksGet, taskQueryParams{ID: task.ID})
		if err != nil {
			if ctx.Err() != nil {
				h.sendCancelOnce(ctx)
			}
			if h.isStopped() {
				h.finishStopped()
				return
			}
			h.finishFailed(err.Error())
			return
		}
		polled := &A2ATask{}
		if err := json.Unmarshal(raw, polled); err != nil {
			h.finishFailed(fmt.Sprintf("a2a: %s: malformed tasks/get result: %v", h.client.cfg.Name, err))
			return
		}
		polled.Kind = "task"
		task = polled
	}
	h.finishTask(task, task.Artifacts)
}

func (c *A2AClient) call(ctx context.Context, endpoint, method string, params any) (json.RawMessage, error) {
	body, err := json.Marshal(rpcRequest{
		JSONRPC: "2.0",
		ID:      c.nextID.Add(1),
		Method:  method,
		Params:  params,
	})
	if err != nil {
		return nil, fmt.Errorf("a2a: %s: marshal %s: %w", c.cfg.Name, method, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("a2a: %s: build %s request: %w", c.cfg.Name, method, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set(A2AVersionHeader, A2AProtocolVersion)
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(req.Header))
	if err := c.setAuth(req); err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return nil, fmt.Errorf("%w: %s: %s: %v", ErrRemoteTimeout, c.cfg.Name, method, err)
		case errors.Is(err, context.Canceled):
			return nil, fmt.Errorf("a2a: %s: %s: %w", c.cfg.Name, method, err)
		default:
			return nil, fmt.Errorf("%w: %s: %s: %v", ErrRemoteUnreachable, c.cfg.Name, method, err)
		}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("%w: %s: %s returned HTTP %d", ErrRemoteAuthFailed, c.cfg.Name, method, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("a2a: %s: %s returned HTTP %d", c.cfg.Name, method, resp.StatusCode)
	}
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("a2a: %s: read %s response: %w", c.cfg.Name, method, err)
	}
	var rpcResp rpcResponse
	if err := json.Unmarshal(respBody, &rpcResp); err != nil {
		return nil, fmt.Errorf("a2a: %s: parse %s response: %w", c.cfg.Name, method, err)
	}
	if rpcResp.Error != nil {
		return nil, fmt.Errorf("a2a: %s: %s: %w", c.cfg.Name, method, rpcResp.Error)
	}
	if len(rpcResp.Result) == 0 {
		return nil, fmt.Errorf("a2a: %s: %s response has neither result nor error", c.cfg.Name, method)
	}
	return rpcResp.Result, nil
}

func (c *A2AClient) cancelTask(ctx context.Context, endpoint, taskID string) {
	if taskID == "" {
		return
	}
	dctx, cancel := context.WithTimeout(contextWithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_, _ = c.call(dctx, endpoint, methodTasksCancel, taskIDParams{ID: taskID})
}

func resultFromA2AParts(statusParts []A2APart, artifacts []A2AArtifact) *A2AResult {
	res := &A2AResult{}
	var texts []string
	merge := func(parts []A2APart) {
		for _, p := range parts {
			switch p.Kind {
			case "text":
				if p.Text != "" {
					texts = append(texts, p.Text)
				}
			case "data":
				if len(p.Data) > 0 {
					if res.Output == nil {
						res.Output = map[string]any{}
					}
					for k, v := range p.Data {
						res.Output[k] = v
					}
				}
			}
		}
	}
	for _, a := range artifacts {
		merge(a.Parts)
	}
	merge(statusParts)
	res.Text = strings.Join(texts, "\n")
	return res
}

func textOfA2AParts(parts []A2APart) string {
	var texts []string
	for _, p := range parts {
		if p.Kind == "text" && p.Text != "" {
			texts = append(texts, p.Text)
		}
	}
	return strings.Join(texts, "\n")
}

func textOrJSON(res *A2AResult) string {
	if res == nil {
		return ""
	}
	if res.Text != "" {
		return res.Text
	}
	if len(res.Output) > 0 {
		if b, err := json.Marshal(res.Output); err == nil {
			return string(b)
		}
	}
	return ""
}

func newA2AID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("a2a: crypto/rand unavailable: %v", err))
	}
	return hex.EncodeToString(b[:])
}
