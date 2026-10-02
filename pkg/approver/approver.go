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

// Package approver is the model-backed permissions.Approver behind
// permission mode "auto" (#1175, docs/auto-mode-design.md).
//
// It makes one non-streaming, tool-less model call per call the gate
// puts to it, and answers allow, deny or escalate. It is not what keeps
// auto safe: the gate decides in code, before Judge runs, which calls
// may reach it at all (the eligible list, the never-list, path scope,
// control-plane files, subagent calls, a missing task). Every way this
// package can fail — a timeout, a model error, an answer it cannot
// parse — is an error, and the gate escalates an error to a person.
//
// EXPERIMENTAL, like permissions.Approver, until the auto-mode
// evaluation (#1175 phase 5) has run against a real model.
package approver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v2/pkg/config"
	"github.com/go-steer/core-agent/v2/pkg/models"
	"github.com/go-steer/core-agent/v2/pkg/permissions"
)

// DefaultMaxArgsBytes is the largest Args a judgement is attempted on
// when Options.MaxArgsBytes is unset. A bigger call escalates instead
// of being truncated: a truncated call is one the approver did not
// see, and the part it did not see is where an injection would sit.
const DefaultMaxArgsBytes = 64 << 10

// maxOutputTokens bounds the answer. It is generous for a two-field
// JSON object because a thinking model spends part of it before
// answering, and a cut-off answer escalates.
const maxOutputTokens = 4096

// Options configures an Approver.
type Options struct {
	// Model makes the judgement. Required.
	Model adkmodel.LLM

	// ModelID names Model in verdicts, audit rows and billing. Empty
	// uses Model.Name().
	ModelID string

	// Timeout bounds one judgement. Zero is
	// config.DefaultAutoApproverTimeout.
	Timeout time.Duration

	// Instructions is the recipe's addition to the built-in policy
	// (permissions.auto.instructions_file), or "".
	Instructions string

	// MaxArgsBytes is the largest Args judged. Zero is
	// DefaultMaxArgsBytes.
	MaxArgsBytes int
}

// Approver judges calls with a model. It is safe for concurrent use.
type Approver struct {
	llm          adkmodel.LLM
	modelID      string
	timeout      time.Duration
	system       string
	maxArgsBytes int
}

var _ permissions.Approver = (*Approver)(nil)

// New builds an Approver from opts.
func New(opts Options) (*Approver, error) {
	if opts.Model == nil {
		return nil, errors.New("approver: a model is required")
	}
	a := &Approver{
		llm:          opts.Model,
		modelID:      opts.ModelID,
		timeout:      opts.Timeout,
		system:       systemInstruction(opts.Instructions),
		maxArgsBytes: opts.MaxArgsBytes,
	}
	if a.modelID == "" {
		a.modelID = opts.Model.Name()
	}
	if a.timeout <= 0 {
		a.timeout = config.DefaultAutoApproverTimeout
	}
	if a.maxArgsBytes <= 0 {
		a.maxArgsBytes = DefaultMaxArgsBytes
	}
	return a, nil
}

// FromConfig builds the Approver permissions.auto configures. The model
// is resolved through provider (permissions.auto.model, else
// model.name), and the instructions file is read now, once: a missing
// file is an error at startup rather than an approver that quietly
// runs without the recipe's policy. cfg.Permissions.Auto must be set.
func FromConfig(ctx context.Context, provider models.Provider, cfg *config.Config, agentsDir string) (*Approver, error) {
	auto := cfg.Permissions.Auto
	if auto == nil {
		return nil, errors.New("approver: permissions.auto is not configured")
	}
	timeout, err := auto.ResolvedTimeout()
	if err != nil {
		return nil, fmt.Errorf("approver: %w", err)
	}
	modelID := auto.Model
	if modelID == "" {
		modelID = cfg.Model.Name
	}
	llm, err := provider.Model(ctx, modelID)
	if err != nil {
		return nil, fmt.Errorf("approver: permissions.auto.model %q: %w", modelID, err)
	}
	var instructions string
	if path := auto.InstructionsPath(agentsDir); path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("approver: permissions.auto.instructions_file: %w", err)
		}
		instructions = string(b)
	}
	return New(Options{Model: llm, ModelID: modelID, Timeout: timeout, Instructions: instructions})
}

// Judge asks the model about one call. Usage is billed to the turn
// through the request context's permissions.ApproverContext, whatever
// the outcome — as far as the provider reported it: a call cut off by
// the timeout, or one that failed before a response carried usage,
// bills nothing, and neither does an attempt a provider retried
// internally.
func (a *Approver) Judge(ctx context.Context, req permissions.ApproverRequest) (permissions.Verdict, error) {
	if len(req.Args) > a.maxArgsBytes {
		return permissions.Verdict{}, fmt.Errorf("approver: the call's arguments are %d bytes, over the %d-byte limit", len(req.Args), a.maxArgsBytes)
	}
	user, err := userMessage(req)
	if err != nil {
		return permissions.Verdict{}, err
	}
	callCtx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()
	text, err := a.generate(callCtx, user)
	if err != nil {
		// Only our own deadline is a timeout. A cancelled turn also ends
		// the call, and saying "no answer within 30s" about it would
		// send an operator to tune the wrong knob.
		if ctx.Err() == nil && errors.Is(callCtx.Err(), context.DeadlineExceeded) {
			return permissions.Verdict{}, fmt.Errorf("approver: no answer within %s: %w", a.timeout, err)
		}
		return permissions.Verdict{}, fmt.Errorf("approver: %w", err)
	}
	v, err := parseVerdict(text)
	if err != nil {
		return permissions.Verdict{}, err
	}
	v.Model = a.modelID
	return v, nil
}

// generate makes the call and returns the answer's text.
func (a *Approver) generate(ctx context.Context, user string) (string, error) {
	req := &adkmodel.LLMRequest{
		Contents: []*genai.Content{genai.NewContentFromText(user, genai.RoleUser)},
		// A non-nil Config with no tools: a nil one lets a provider
		// wrapper add its built-ins (see AskSideQuestion).
		Config: &genai.GenerateContentConfig{
			SystemInstruction: genai.NewContentFromText(a.system, genai.RoleUser),
			MaxOutputTokens:   maxOutputTokens,
			Temperature:       genai.Ptr[float32](0),
		},
	}
	// Each prompt differs in the call it carries, so a cache write is a
	// surcharge for a read that does not come.
	ctx = models.WithoutPromptCache(ctx)
	ctx = models.WithoutBuiltins(ctx)

	var usage *genai.GenerateContentResponseUsageMetadata
	defer func() {
		if ac, ok := permissions.ApproverContextFromContext(ctx); ok && usage != nil {
			ac.Bill(a.modelID, usage)
		}
	}()
	var b strings.Builder
	for resp, err := range a.llm.GenerateContent(ctx, req, false) {
		if err != nil {
			return "", err
		}
		if resp == nil {
			continue
		}
		if resp.UsageMetadata != nil {
			usage = resp.UsageMetadata
		}
		if resp.Content == nil || resp.Partial {
			continue
		}
		for _, p := range resp.Content.Parts {
			if p != nil && p.Text != "" && !p.Thought {
				b.WriteString(p.Text)
			}
		}
	}
	return b.String(), nil
}

// pendingCall and recentCall are the JSON shapes the model reads.
type pendingCall struct {
	Tool      string          `json:"tool"`
	Detail    string          `json:"detail"`
	Arguments json.RawMessage `json:"arguments"`
}

type recentCall struct {
	Tool   string `json:"tool"`
	Detail string `json:"detail"`
}

// userMessage renders the request as one JSON document. Encoding is
// what keeps untrusted text in its field: a delimiter in a prompt
// template can be closed by the text inside it, and a JSON string
// cannot.
func userMessage(req permissions.ApproverRequest) (string, error) {
	recent := make([]recentCall, 0, len(req.RecentCalls))
	for _, c := range req.RecentCalls {
		recent = append(recent, recentCall{Tool: c.ToolName, Detail: c.Detail})
	}
	doc := struct {
		Task        string       `json:"task"`
		RecentCalls []recentCall `json:"recent_calls"`
		PendingCall pendingCall  `json:"pending_call"`
	}{
		Task:        req.Task,
		RecentCalls: recent,
		PendingCall: pendingCall{Tool: req.ToolName, Detail: req.Detail, Arguments: req.Args},
	}
	// No HTML escaping: the model should read `a && b` as written, not
	// as `a \u0026\u0026 b`.
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return "", fmt.Errorf("approver: encoding the request: %w", err)
	}
	return strings.TrimSuffix(b.String(), "\n"), nil
}

// parseVerdict reads the model's answer: one JSON object with a verdict
// and a reason, optionally inside a code fence or surrounded by prose.
// Anything else is an error, which escalates.
func parseVerdict(text string) (permissions.Verdict, error) {
	start, end := strings.Index(text, "{"), strings.LastIndex(text, "}")
	if start < 0 || end < start {
		return permissions.Verdict{}, fmt.Errorf("approver: the answer holds no JSON object: %q", clip(text))
	}
	var ans struct {
		Verdict string `json:"verdict"`
		Reason  string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(text[start:end+1]), &ans); err != nil {
		return permissions.Verdict{}, fmt.Errorf("approver: the answer is not the verdict object: %w: %q", err, clip(text))
	}
	v := permissions.Verdict{Reason: strings.TrimSpace(ans.Reason)}
	switch strings.ToLower(strings.TrimSpace(ans.Verdict)) {
	case "allow":
		v.Outcome = permissions.VerdictAllow
	case "deny":
		v.Outcome = permissions.VerdictDeny
	case "escalate":
		v.Outcome = permissions.VerdictEscalate
	default:
		return permissions.Verdict{}, fmt.Errorf("approver: unknown verdict %q", ans.Verdict)
	}
	return v, nil
}

// clip bounds model output quoted in an error.
func clip(s string) string {
	const max = 200
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
