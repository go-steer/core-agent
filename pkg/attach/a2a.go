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

// Originally derived from go-steer/mast@d2f45c7 internal/a2a/server.go (#1407).

package attach

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/google/uuid"

	"github.com/go-steer/core-agent/v3/pkg/auth"
)

const (
	// a2aMaxRequestBytes caps inbound JSON-RPC request bodies at 1 MiB.
	a2aMaxRequestBytes = 1 << 20

	// a2aSSEWriteTimeout bounds each SSE frame write on message/stream so a
	// wedged TCP peer cannot pin a streaming goroutine indefinitely.
	a2aSSEWriteTimeout = 30 * time.Second
)

// ErrA2AForbidden is returned by A2ABackend methods when a caller is not
// authorized to access the session or task (#1407).
var ErrA2AForbidden = errors.New("attach: a2a forbidden")

// A2AAuthOptions configures authentication and required token scopes for an
// inbound A2A skill (#1407).
type A2AAuthOptions struct {
	Required bool
	Scopes   []string
}

// A2ASkill describes one A2A skill exposed on the attach server (#1407).
type A2ASkill struct {
	ID           string
	Name         string
	Description  string
	Tags         []string
	Examples     []string
	InputSchema  map[string]any
	OutputSchema map[string]any
	Auth         A2AAuthOptions
}

// A2AOptions configures inbound A2A v0.3 JSON-RPC 2.0 server exposure on the
// attach mux (POST /a2a and /.well-known/agent-card.json) (#1407).
type A2AOptions struct {
	// Expose mounts POST /a2a (when Backend != nil) and switches the Agent
	// Card to advertise preferredTransport="JSONRPC" and the /a2a URL.
	Expose bool

	// Backend executes and tracks A2A tasks.
	Backend A2ABackend

	// SkillName is the default A2A skill id and name when Skills is empty.
	SkillName string

	// SkillDescription is the human-readable description for the default A2A
	// skill when Skills is empty.
	SkillDescription string

	// InputSchema and OutputSchema are optional JSON Schema maps for the
	// default skill.
	InputSchema  map[string]any
	OutputSchema map[string]any

	// Auth configures authentication and required scopes for the default skill.
	Auth A2AAuthOptions

	// Skills optionally declares multiple named A2A skills. When empty, one
	// skill is synthesized from SkillName, SkillDescription, InputSchema,
	// OutputSchema, and Auth.
	Skills []A2ASkill
}

// A2ATaskRequest is the normalized request passed to A2ABackend.SendMessage.
type A2ATaskRequest struct {
	TaskID     string
	ContextID  string
	SkillID    string
	Message    A2AMessage
	Text       string
	Data       map[string]any
	Metadata   map[string]any
	Caller     auth.Caller
	OnProgress func(state A2ATaskState, statusText string)
}

// A2ATaskOutcome is the result returned by A2ABackend methods.
type A2ATaskOutcome struct {
	TaskID        string
	ContextID     string
	State         A2ATaskState
	Output        string
	Data          map[string]any
	StatusMessage string
	Metadata      map[string]any
}

// A2ABackend executes and tracks A2A v0.3 tasks on behalf of the attach
// listener (#1407).
type A2ABackend interface {
	SendMessage(ctx context.Context, req A2ATaskRequest) (A2ATaskOutcome, error)
	GetTask(ctx context.Context, id string) (A2ATaskOutcome, bool, error)
	CancelTask(ctx context.Context, id string) (A2ATaskOutcome, bool, error)
}

// A2APreflighter is an optional interface A2ABackend implementations may
// satisfy to reject unknown or unauthorized task continuations before
// message/stream commits HTTP 200 text/event-stream headers (#1407).
type A2APreflighter interface {
	PreflightTask(ctx context.Context, req A2ATaskRequest) error
}

type compiledA2ASkill struct {
	A2ASkill
	compiledInput  *jsonschema.Resolved
	compiledOutput *jsonschema.Resolved
}

type a2aHandler struct {
	backend         A2ABackend
	skills          []compiledA2ASkill
	skillByID       map[string]*compiledA2ASkill
	authRequired    bool
	defaultScopes   []string
	defaultCallerID string
	costLimit       *costRateLimiter
	closing         <-chan struct{}
}

func mountA2A(opts *Options, mux *http.ServeMux, h *handlers) error {
	a2a, err := prepareA2A(opts, h.costLimit, h.closing)
	if err != nil || a2a == nil {
		return err
	}
	a2a.register(mux)
	return nil
}

func prepareA2A(opts *Options, costLimit *costRateLimiter, closing <-chan struct{}) (*a2aHandler, error) {
	if !opts.A2A.Expose {
		return nil, nil
	}
	rawSkills := resolveA2ASkills(*opts)
	compiled := make([]compiledA2ASkill, 0, len(rawSkills))
	byID := make(map[string]*compiledA2ASkill, len(rawSkills))
	anyAuthReq := opts.A2A.Auth.Required || len(opts.A2A.Auth.Scopes) > 0 || (opts.MultiSessionEnabled && !opts.AllowAnonymous)

	for _, sk := range rawSkills {
		if strings.TrimSpace(sk.ID) == "" {
			return nil, errors.New("attach: A2A skill ID must not be empty")
		}
		if _, dup := byID[sk.ID]; dup {
			return nil, fmt.Errorf("attach: duplicate A2A skill ID %q", sk.ID)
		}
		inRes, err := compileA2ASchema(sk.InputSchema)
		if err != nil {
			return nil, fmt.Errorf("attach: A2A skill %q input_schema: %w", sk.ID, err)
		}
		outRes, err := compileA2ASchema(sk.OutputSchema)
		if err != nil {
			return nil, fmt.Errorf("attach: A2A skill %q output_schema: %w", sk.ID, err)
		}
		if len(sk.Auth.Scopes) > 0 {
			sk.Auth.Required = true
		}
		if sk.Auth.Required {
			anyAuthReq = true
		}
		if sk.Name == "" {
			sk.Name = sk.ID
		}
		if len(sk.Tags) == 0 {
			sk.Tags = []string{"a2a"}
		}
		compiled = append(compiled, compiledA2ASkill{
			A2ASkill:       sk,
			compiledInput:  inRes,
			compiledOutput: outRes,
		})
	}
	for i := range compiled {
		byID[compiled[i].ID] = &compiled[i]
	}

	applyA2AToAgentCard(&opts.AgentCard, opts.Registry, compiled, anyAuthReq)
	if opts.A2A.Backend == nil {
		return nil, nil
	}
	return &a2aHandler{
		backend:         opts.A2A.Backend,
		skills:          compiled,
		skillByID:       byID,
		authRequired:    opts.A2A.Auth.Required || len(opts.A2A.Auth.Scopes) > 0,
		defaultScopes:   append([]string(nil), opts.A2A.Auth.Scopes...),
		defaultCallerID: opts.DefaultCaller.Identity,
		costLimit:       costLimit,
		closing:         closing,
	}, nil
}

func resolveA2ASkills(opts Options) []A2ASkill {
	if len(opts.A2A.Skills) > 0 {
		out := make([]A2ASkill, len(opts.A2A.Skills))
		copy(out, opts.A2A.Skills)
		for i := range out {
			if out[i].ID == "" {
				out[i].ID = out[i].Name
			}
			if !out[i].Auth.Required && opts.A2A.Auth.Required {
				out[i].Auth.Required = true
			}
			if len(out[i].Auth.Scopes) == 0 && len(opts.A2A.Auth.Scopes) > 0 {
				out[i].Auth.Scopes = append([]string(nil), opts.A2A.Auth.Scopes...)
			}
		}
		return out
	}
	skillName := strings.TrimSpace(opts.A2A.SkillName)
	if skillName == "" {
		skillName = resolveName(opts.AgentCard.Name, opts.Registry)
	}
	skillDesc := strings.TrimSpace(opts.A2A.SkillDescription)
	if skillDesc == "" {
		skillDesc = resolveDescription(opts.AgentCard.Description, opts.Registry)
	}
	if skillDesc == "" {
		skillDesc = skillName
	}
	return []A2ASkill{{
		ID:           skillName,
		Name:         skillName,
		Description:  skillDesc,
		Tags:         []string{"a2a"},
		InputSchema:  opts.A2A.InputSchema,
		OutputSchema: opts.A2A.OutputSchema,
		Auth:         opts.A2A.Auth,
	}}
}

func applyA2AToAgentCard(card *AgentCardConfig, reg *SessionRegistry, skills []compiledA2ASkill, bearerAuth bool) {
	card.A2AExposed = true
	if bearerAuth {
		card.A2ABearerAuth = true
	}
	if card.Description == "" {
		if len(skills) > 0 && skills[0].Description != "" {
			card.Description = skills[0].Description
		} else if d := resolveDescription("", reg); d != "" {
			card.Description = d
		} else {
			card.Description = resolveName(card.Name, reg)
		}
	}
	existing := make(map[string]bool, len(card.ExtraSkills))
	for _, s := range card.ExtraSkills {
		existing[s.ID] = true
	}
	for _, sk := range skills {
		if existing[sk.ID] {
			continue
		}
		card.ExtraSkills = append(card.ExtraSkills, AgentCardSkill{
			ID:           sk.ID,
			Name:         sk.Name,
			Description:  sk.Description,
			Tags:         sk.Tags,
			Examples:     sk.Examples,
			InputSchema:  sk.InputSchema,
			OutputSchema: sk.OutputSchema,
		})
	}
}

var validA2AJSONSchemaTypes = map[string]bool{
	"null":    true,
	"boolean": true,
	"object":  true,
	"array":   true,
	"number":  true,
	"string":  true,
	"integer": true,
}

func compileA2ASchema(m map[string]any) (*jsonschema.Resolved, error) {
	if len(m) == 0 {
		return nil, nil
	}
	if err := validateA2AJSONSchemaNode("", m); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}
	var s jsonschema.Schema
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}
	return s.Resolve(nil)
}

func validateA2AJSONSchemaNode(path string, node map[string]any) error {
	if t, hasType := node["type"]; hasType {
		switch tv := t.(type) {
		case string:
			if !validA2AJSONSchemaTypes[tv] {
				return fmt.Errorf("%s: unknown JSON Schema type %q", a2aSchemaPathLabel(path, "type"), tv)
			}
		case []any:
			if len(tv) == 0 {
				return fmt.Errorf("%s: JSON Schema type array must not be empty", a2aSchemaPathLabel(path, "type"))
			}
			for _, item := range tv {
				s, ok := item.(string)
				if !ok || !validA2AJSONSchemaTypes[s] {
					return fmt.Errorf("%s: unknown JSON Schema type %v", a2aSchemaPathLabel(path, "type"), item)
				}
			}
		default:
			return fmt.Errorf("%s: JSON Schema type must be a string or array of strings", a2aSchemaPathLabel(path, "type"))
		}
	}
	if props, hasProps := node["properties"]; hasProps {
		pm, ok := props.(map[string]any)
		if !ok {
			return fmt.Errorf("%s: properties must be an object", a2aSchemaPathLabel(path, "properties"))
		}
		for k, v := range pm {
			sub, ok := v.(map[string]any)
			if !ok {
				return fmt.Errorf("%s: property schema must be an object", a2aSchemaPathLabel(path, "properties."+k))
			}
			if err := validateA2AJSONSchemaNode(a2aSchemaPathLabel(path, "properties."+k), sub); err != nil {
				return err
			}
		}
	}
	if items, hasItems := node["items"]; hasItems {
		sub, ok := items.(map[string]any)
		if !ok {
			return fmt.Errorf("%s: items schema must be an object", a2aSchemaPathLabel(path, "items"))
		}
		if err := validateA2AJSONSchemaNode(a2aSchemaPathLabel(path, "items"), sub); err != nil {
			return err
		}
	}
	return nil
}

func a2aSchemaPathLabel(prefix, field string) string {
	if prefix == "" {
		return field
	}
	return prefix + "." + field
}

func (a *a2aHandler) register(mux *http.ServeMux) {
	mux.HandleFunc("POST "+A2APath, a.serveHTTP)
	mux.HandleFunc(A2APath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			a.serveHTTP(w, r)
			return
		}
		w.Header().Set("Allow", http.MethodPost)
		w.Header().Set(A2AVersionHeader, A2AHeaderValue)
		writeA2AErrorStatus(w, http.StatusMethodNotAllowed, nil, JSONRPCInvalidRequest, "method not allowed; use POST", nil)
	})
}

func (a *a2aHandler) serveHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set(A2AVersionHeader, A2AHeaderValue)
	if err := checkA2AVersionHeader(r.Header.Get(A2AVersionHeader)); err != nil {
		writeA2AErrorStatus(w, http.StatusBadRequest, nil, JSONRPCInvalidRequest, err.Error(), nil)
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, a2aMaxRequestBytes))
	if err != nil {
		writeA2AError(w, nil, JSONRPCParseError, fmt.Sprintf("read request body: %v", err))
		return
	}
	var req jsonrpcRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeA2AError(w, nil, JSONRPCParseError, fmt.Sprintf("parse error: %v", err))
		return
	}
	if req.JSONRPC != "2.0" || strings.TrimSpace(req.Method) == "" {
		writeA2AError(w, req.ID, JSONRPCInvalidRequest, "invalid JSON-RPC 2.0 request (jsonrpc must be \"2.0\" and method non-empty)")
		return
	}
	if a.authRequired && !a.isAuthenticated(r) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="a2a"`)
		writeA2AErrorStatus(w, http.StatusUnauthorized, req.ID, JSONRPCServerError, "unauthenticated", nil)
		return
	}

	switch req.Method {
	case A2AMethodMessageSend:
		a.handleMessageSend(w, r, req)
	case A2AMethodMessageStream:
		a.handleMessageStream(w, r, req)
	case A2AMethodTasksGet:
		a.handleTasksGet(w, r, req)
	case A2AMethodTasksCancel:
		a.handleTasksCancel(w, r, req)
	default:
		writeA2AError(w, req.ID, JSONRPCMethodNotFound, fmt.Sprintf("method %q not found", req.Method))
	}
}

func (a *a2aHandler) isAuthenticated(r *http.Request) bool {
	if src, ok := authSourceFromContext(r.Context()); ok && src != "" && src != whoAmISourceAnonymous {
		return true
	}
	c, _ := auth.CallerFromContext(r.Context())
	return c.Identity != "" && c.Identity != auth.Anonymous.Identity && c.Identity != a.defaultCallerID
}

func (a *a2aHandler) checkRateLimit(w http.ResponseWriter, r *http.Request, reqID any) bool {
	if a.costLimit == nil {
		return true
	}
	caller, _ := auth.CallerFromContext(r.Context())
	ok, retryAfter := a.costLimit.Allow(caller.Identity)
	if ok {
		return true
	}
	secs := int(math.Ceil(retryAfter.Seconds()))
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	writeA2AErrorStatus(w, http.StatusTooManyRequests, reqID, JSONRPCServerError, "rate limit exceeded", map[string]any{
		"retry_after_seconds": secs,
	})
	return false
}

func (a *a2aHandler) prepareTurnRequest(w http.ResponseWriter, r *http.Request, req jsonrpcRequest) (A2ATaskRequest, A2AMessage, bool) {
	if !a.checkRateLimit(w, r, req.ID) {
		return A2ATaskRequest{}, A2AMessage{}, false
	}
	var params A2ASendMessageParams
	if len(req.Params) == 0 {
		writeA2AError(w, req.ID, JSONRPCInvalidParams, "missing params")
		return A2ATaskRequest{}, A2AMessage{}, false
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		writeA2AError(w, req.ID, JSONRPCInvalidParams, fmt.Sprintf("invalid params: %v", err))
		return A2ATaskRequest{}, A2AMessage{}, false
	}
	if len(params.Message.Parts) == 0 {
		writeA2AError(w, req.ID, JSONRPCInvalidParams, "invalid params: message.parts must not be empty")
		return A2ATaskRequest{}, A2AMessage{}, false
	}

	skill, err := a.selectSkill(params)
	if err != nil {
		writeA2AError(w, req.ID, JSONRPCInvalidParams, err.Error())
		return A2ATaskRequest{}, A2AMessage{}, false
	}
	if !a.authorizeSkill(w, r, req.ID, skill) {
		return A2ATaskRequest{}, A2AMessage{}, false
	}

	text, data, err := extractA2AParts(params.Message.Parts)
	if err != nil {
		writeA2AError(w, req.ID, JSONRPCInvalidParams, err.Error())
		return A2ATaskRequest{}, A2AMessage{}, false
	}
	if err := validateA2AInput(skill, text, data); err != nil {
		writeA2AError(w, req.ID, JSONRPCInvalidParams, err.Error())
		return A2ATaskRequest{}, A2AMessage{}, false
	}

	msg := normalizeA2AMessage(params.Message)
	caller, _ := auth.CallerFromContext(r.Context())
	return A2ATaskRequest{
		TaskID:    msg.TaskID,
		ContextID: msg.ContextID,
		SkillID:   skill.ID,
		Message:   msg,
		Text:      text,
		Data:      data,
		Metadata:  params.Metadata,
		Caller:    caller,
	}, msg, true
}

func (a *a2aHandler) authorizeSkill(w http.ResponseWriter, r *http.Request, reqID any, skill *compiledA2ASkill) bool {
	if skill.Auth.Required && !a.isAuthenticated(r) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="a2a"`)
		writeA2AErrorStatus(w, http.StatusUnauthorized, reqID, JSONRPCServerError, "unauthenticated", nil)
		return false
	}
	if len(skill.Auth.Scopes) == 0 {
		return true
	}
	caller, _ := auth.CallerFromContext(r.Context())
	if caller.Admin {
		return true
	}
	have := extractCallerScopes(caller)
	for _, need := range skill.Auth.Scopes {
		if !have[need] {
			writeA2AErrorStatus(w, http.StatusForbidden, reqID, JSONRPCServerError,
				fmt.Sprintf("insufficient_scope: skill %q requires scope %q", skill.ID, need), nil)
			return false
		}
	}
	return true
}

func extractCallerScopes(c auth.Caller) map[string]bool {
	out := make(map[string]bool)
	for _, key := range []string{"scopes", "scope"} {
		raw := c.Labels[key]
		if raw == "" {
			continue
		}
		for _, tok := range strings.FieldsFunc(raw, func(r rune) bool {
			return r == ',' || unicode.IsSpace(r)
		}) {
			if tok != "" {
				out[tok] = true
			}
		}
	}
	return out
}

func (a *a2aHandler) handleMessageSend(w http.ResponseWriter, r *http.Request, req jsonrpcRequest) {
	taskReq, msg, ok := a.prepareTurnRequest(w, r, req)
	if !ok {
		return
	}
	outcome, err := a.backend.SendMessage(r.Context(), taskReq)
	if err != nil {
		if errors.Is(err, ErrA2AForbidden) {
			writeA2AErrorStatus(w, http.StatusForbidden, req.ID, JSONRPCServerError, err.Error(), nil)
			return
		}
		writeA2AError(w, req.ID, JSONRPCServerError, err.Error())
		return
	}
	writeA2AResult(w, req.ID, outcomeToA2ATask(outcome, &msg))
}

func (a *a2aHandler) handleMessageStream(w http.ResponseWriter, r *http.Request, req jsonrpcRequest) {
	taskReq, _, ok := a.prepareTurnRequest(w, r, req)
	if !ok {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeA2AError(w, req.ID, JSONRPCInternalError, "streaming unsupported by response writer")
		return
	}
	if pre, ok := a.backend.(A2APreflighter); ok {
		if err := pre.PreflightTask(r.Context(), taskReq); err != nil {
			if errors.Is(err, ErrA2AForbidden) {
				writeA2AErrorStatus(w, http.StatusForbidden, req.ID, JSONRPCServerError, err.Error(), nil)
				return
			}
			writeA2AError(w, req.ID, JSONRPCServerError, err.Error())
			return
		}
	}
	if taskReq.TaskID == "" {
		taskReq.TaskID = "a2a-" + uuid.NewString()
	} else if taskReq.ContextID == "" {
		if existing, found, err := a.backend.GetTask(r.Context(), taskReq.TaskID); err == nil && found && existing.ContextID != "" {
			taskReq.ContextID = existing.ContextID
		}
	}
	if taskReq.ContextID == "" {
		taskReq.ContextID = "ctx-" + uuid.NewString()
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	a.streamTaskTurn(w, r, flusher, req.ID, taskReq)
}

func (a *a2aHandler) streamTaskTurn(w http.ResponseWriter, r *http.Request, flusher http.Flusher, reqID any, taskReq A2ATaskRequest) {
	rc := http.NewResponseController(w)
	var writeMu sync.Mutex
	sendFrame := func(result any) bool {
		writeMu.Lock()
		defer writeMu.Unlock()
		return writeA2ASSEFrame(w, rc, flusher, jsonrpcResponse{
			JSONRPC: "2.0",
			ID:      reqID,
			Result:  result,
		}) == nil
	}

	if !sendFrame(newStatusUpdateEvent(taskReq.TaskID, taskReq.ContextID, TaskStateWorking, "", false, nil)) {
		return
	}
	taskReq.OnProgress = func(state A2ATaskState, statusText string) {
		sendFrame(newStatusUpdateEvent(taskReq.TaskID, taskReq.ContextID, state, statusText, false, nil))
	}

	outcome, err := a.backend.SendMessage(r.Context(), taskReq)
	if err != nil {
		sendFrame(newStatusUpdateEvent(taskReq.TaskID, taskReq.ContextID, TaskStateFailed, err.Error(), true, nil))
		return
	}
	taskID := firstNonEmptyA2A(outcome.TaskID, taskReq.TaskID)
	ctxID := firstNonEmptyA2A(outcome.ContextID, taskReq.ContextID)
	state := outcome.State
	if state == "" {
		state = TaskStateCompleted
	}
	if parts := buildOutcomeParts(outcome); len(parts) > 0 {
		sendFrame(A2ATaskArtifactUpdateEvent{
			Kind:      "artifact-update",
			TaskID:    taskID,
			ContextID: ctxID,
			Artifact: A2AArtifact{
				ArtifactID: "art-" + taskID,
				Name:       "response",
				Parts:      parts,
			},
			LastChunk: true,
		})
	}
	sendFrame(newStatusUpdateEvent(taskID, ctxID, state, outcome.StatusMessage, true, outcome.Metadata))
}

func newStatusUpdateEvent(taskID, ctxID string, state A2ATaskState, msgText string, final bool, meta map[string]any) A2ATaskStatusUpdateEvent {
	status := A2ATaskStatus{
		State:     state,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}
	if msgText != "" {
		status.Message = &A2AMessage{
			Kind:      "message",
			MessageID: "msg-" + uuid.NewString(),
			Role:      "agent",
			Parts:     []A2APart{NewA2ATextPart(msgText)},
			TaskID:    taskID,
			ContextID: ctxID,
		}
	}
	return A2ATaskStatusUpdateEvent{
		Kind:      "status-update",
		TaskID:    taskID,
		ContextID: ctxID,
		Status:    status,
		Final:     final,
		Metadata:  meta,
	}
}

func (a *a2aHandler) authorizeDefaultScopes(w http.ResponseWriter, r *http.Request, reqID any) bool {
	if len(a.defaultScopes) == 0 {
		return true
	}
	if !a.isAuthenticated(r) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="a2a"`)
		writeA2AErrorStatus(w, http.StatusUnauthorized, reqID, JSONRPCServerError, "unauthenticated", nil)
		return false
	}
	caller, _ := auth.CallerFromContext(r.Context())
	if caller.Admin {
		return true
	}
	have := extractCallerScopes(caller)
	for _, need := range a.defaultScopes {
		if !have[need] {
			writeA2AErrorStatus(w, http.StatusForbidden, reqID, JSONRPCServerError,
				fmt.Sprintf("insufficient_scope: requires scope %q", need), nil)
			return false
		}
	}
	return true
}

func (a *a2aHandler) handleTasksGet(w http.ResponseWriter, r *http.Request, req jsonrpcRequest) {
	if !a.authorizeDefaultScopes(w, r, req.ID) {
		return
	}
	var params A2ATaskQueryParams
	if len(req.Params) == 0 {
		writeA2AError(w, req.ID, JSONRPCInvalidParams, "missing params")
		return
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		writeA2AError(w, req.ID, JSONRPCInvalidParams, fmt.Sprintf("invalid params: %v", err))
		return
	}
	if strings.TrimSpace(params.ID) == "" {
		writeA2AError(w, req.ID, JSONRPCInvalidParams, "invalid params: id is required")
		return
	}
	outcome, found, err := a.backend.GetTask(r.Context(), params.ID)
	if err != nil {
		if errors.Is(err, ErrA2AForbidden) {
			writeA2AErrorStatus(w, http.StatusForbidden, req.ID, JSONRPCServerError, err.Error(), nil)
			return
		}
		writeA2AError(w, req.ID, JSONRPCServerError, err.Error())
		return
	}
	if !found {
		writeA2AError(w, req.ID, JSONRPCServerError, fmt.Sprintf("task %q not found", params.ID))
		return
	}
	writeA2AResult(w, req.ID, outcomeToA2ATask(outcome, nil))
}

func (a *a2aHandler) handleTasksCancel(w http.ResponseWriter, r *http.Request, req jsonrpcRequest) {
	if !a.authorizeDefaultScopes(w, r, req.ID) {
		return
	}
	var params A2ATaskIDParams
	if len(req.Params) == 0 {
		writeA2AError(w, req.ID, JSONRPCInvalidParams, "missing params")
		return
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		writeA2AError(w, req.ID, JSONRPCInvalidParams, fmt.Sprintf("invalid params: %v", err))
		return
	}
	if strings.TrimSpace(params.ID) == "" {
		writeA2AError(w, req.ID, JSONRPCInvalidParams, "invalid params: id is required")
		return
	}
	outcome, found, err := a.backend.CancelTask(r.Context(), params.ID)
	if err != nil {
		if errors.Is(err, ErrA2AForbidden) {
			writeA2AErrorStatus(w, http.StatusForbidden, req.ID, JSONRPCServerError, err.Error(), nil)
			return
		}
		writeA2AError(w, req.ID, JSONRPCServerError, err.Error())
		return
	}
	if !found {
		writeA2AError(w, req.ID, JSONRPCServerError, fmt.Sprintf("task %q not found", params.ID))
		return
	}
	writeA2AResult(w, req.ID, outcomeToA2ATask(outcome, nil))
}

func (a *a2aHandler) selectSkill(params A2ASendMessageParams) (*compiledA2ASkill, error) {
	requested := extractRequestedSkillID(params)
	if requested == "" {
		if len(a.skills) == 1 {
			return &a.skills[0], nil
		}
		ids := make([]string, 0, len(a.skills))
		for _, sk := range a.skills {
			ids = append(ids, sk.ID)
		}
		sort.Strings(ids)
		return nil, fmt.Errorf("skill must be specified in metadata.skill when multiple skills are exposed (available: %s)", strings.Join(ids, ", "))
	}
	sk, ok := a.skillByID[requested]
	if !ok {
		return nil, fmt.Errorf("unknown skill %q", requested)
	}
	return sk, nil
}

func extractRequestedSkillID(params A2ASendMessageParams) string {
	for _, m := range []map[string]any{params.Metadata, params.Message.Metadata} {
		if m == nil {
			continue
		}
		for _, key := range []string{"skill", "skillId", "skill_id"} {
			if v, ok := m[key].(string); ok && strings.TrimSpace(v) != "" {
				return strings.TrimSpace(v)
			}
		}
	}
	return ""
}

func extractA2AParts(parts []A2APart) (string, map[string]any, error) {
	var texts []string
	var data map[string]any
	for i, p := range parts {
		switch p.Kind {
		case "", "text":
			texts = append(texts, p.Text)
		case "data":
			if p.Data != nil {
				if data == nil {
					data = make(map[string]any, len(p.Data))
				}
				for k, v := range p.Data {
					data[k] = v
				}
			}
		default:
			return "", nil, fmt.Errorf("unsupported part[%d].kind %q (want \"text\" or \"data\")", i, p.Kind)
		}
	}
	return strings.Join(texts, "\n"), data, nil
}

func validateA2AInput(skill *compiledA2ASkill, text string, data map[string]any) error {
	if skill.compiledInput == nil {
		return nil
	}
	var candidate any
	switch {
	case data != nil:
		candidate = data
	case strings.TrimSpace(text) != "":
		if err := json.Unmarshal([]byte(text), &candidate); err != nil {
			return fmt.Errorf("skill %q requires JSON input matching input_schema: %v", skill.ID, err)
		}
	default:
		return fmt.Errorf("skill %q requires input matching input_schema, got empty payload", skill.ID)
	}
	if err := skill.compiledInput.Validate(candidate); err != nil {
		return fmt.Errorf("input_schema validation failed for skill %q: %v", skill.ID, err)
	}
	return nil
}

func normalizeA2AMessage(msg A2AMessage) A2AMessage {
	if msg.Kind == "" {
		msg.Kind = "message"
	}
	if msg.Role == "" {
		msg.Role = "user"
	}
	if msg.MessageID == "" {
		msg.MessageID = "msg-" + uuid.NewString()
	}
	return msg
}

func outcomeToA2ATask(o A2ATaskOutcome, inbound *A2AMessage) A2ATask {
	taskID := o.TaskID
	if taskID == "" {
		taskID = "a2a-" + uuid.NewString()
	}
	ctxID := o.ContextID
	if ctxID == "" {
		ctxID = "ctx-" + uuid.NewString()
	}
	state := o.State
	if state == "" {
		state = TaskStateCompleted
	}
	status := A2ATaskStatus{
		State:     state,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}
	if o.StatusMessage != "" {
		status.Message = &A2AMessage{
			Kind:      "message",
			MessageID: "msg-" + uuid.NewString(),
			Role:      "agent",
			Parts:     []A2APart{NewA2ATextPart(o.StatusMessage)},
			TaskID:    taskID,
			ContextID: ctxID,
		}
	}
	var artifacts []A2AArtifact
	if parts := buildOutcomeParts(o); len(parts) > 0 {
		artifacts = []A2AArtifact{{
			ArtifactID: "art-" + taskID,
			Name:       "response",
			Parts:      parts,
		}}
	}
	var history []A2AMessage
	if inbound != nil {
		history = []A2AMessage{*inbound}
	}
	return A2ATask{
		Kind:      "task",
		ID:        taskID,
		ContextID: ctxID,
		Status:    status,
		Artifacts: artifacts,
		History:   history,
		Metadata:  o.Metadata,
	}
}

func buildOutcomeParts(o A2ATaskOutcome) []A2APart {
	var parts []A2APart
	if o.Output != "" {
		parts = append(parts, NewA2ATextPart(o.Output))
	}
	if len(o.Data) > 0 {
		parts = append(parts, NewA2ADataPart(o.Data))
	}
	return parts
}

func checkA2AVersionHeader(raw string) error {
	v := strings.TrimSpace(raw)
	if v == "" || v == "0.3" || strings.HasPrefix(v, "0.3.") {
		return nil
	}
	return fmt.Errorf("unsupported %s %q (server speaks A2A 0.3)", A2AVersionHeader, raw)
}

func writeA2ASSEFrame(w http.ResponseWriter, rc *http.ResponseController, flusher http.Flusher, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_ = rc.SetWriteDeadline(time.Now().Add(a2aSSEWriteTimeout))
	defer func() {
		_ = rc.SetWriteDeadline(time.Time{})
	}()
	if _, err := fmt.Fprintf(w, "data: %s\n\n", raw); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

func writeA2AResult(w http.ResponseWriter, id any, result any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(jsonrpcResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result:  result,
	})
}

func writeA2AError(w http.ResponseWriter, id any, code int, msg string) {
	writeA2AErrorStatus(w, http.StatusOK, id, code, msg, nil)
}

func writeA2AErrorStatus(w http.ResponseWriter, httpStatus int, id any, code int, msg string, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(httpStatus)
	_ = json.NewEncoder(w).Encode(jsonrpcResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error: &jsonrpcError{
			Code:    code,
			Message: msg,
			Data:    data,
		},
	})
}

func firstNonEmptyA2A(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
