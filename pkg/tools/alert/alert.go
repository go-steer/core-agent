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

// Package alert implements the native `alert` built-in tool: a
// config-driven, distroless-safe way for a headless core-agent daemon to
// fire escalations to pre-registered webhook targets (Slack, Discord,
// PagerDuty, generic JSON) without shelling out or running a separate MCP
// server. The design lives in docs/alert-tool-design.md.
//
// SSRF is impossible by construction: the tool exposes no arbitrary-URL
// parameter — the model picks a target by NAME from the operator's
// registry, and an unknown name is rejected rather than dialed.
package alert

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"

	"github.com/go-steer/core-agent/v2/pkg/config"
	"github.com/go-steer/core-agent/v2/pkg/permissions"
)

const (
	toolName        = "alert"
	httpTimeout     = 10 * time.Second // tight bound; a slow webhook must not hang a session
	maxErrBodyBytes = 1024             // cap of the failed-response snippet surfaced to the agent
)

// Args is the tool's input. Auth material is deliberately absent — it is
// resolved from env at call time and never appears in the schema or the
// audited args (design §Security).
type Args struct {
	Target  string         `json:"target" jsonschema:"required; the name of a pre-registered alert target (see the tool description for the available names and what each is for)"`
	Level   string         `json:"level" jsonschema:"required; severity of the alert: one of info, warning, critical, resolved"`
	Summary string         `json:"summary" jsonschema:"required; a one-line human-readable summary of what happened"`
	Details map[string]any `json:"details,omitempty" jsonschema:"optional; a structured object of extra context (cluster, incident id, session url, ...) merged into the target's payload"`
}

// Result is the tool's output. Response BODY is intentionally excluded —
// it may carry PII from the destination and could be a smuggle channel
// back to the model; only the status code and duration are returned.
type Result struct {
	Target     string `json:"target"`
	StatusCode int    `json:"status_code"`
	DurationMs int64  `json:"duration_ms"`
}

// validLevels is the closed set of severities the tool accepts.
var validLevels = map[string]struct{}{
	"info":     {},
	"warning":  {},
	"critical": {},
	"resolved": {},
}

// New builds the alert tool from cfg's target registry. The caller
// (pkg/tools.Build) only invokes this when cfg.Alerts.Targets is
// non-empty, so the model never sees an `alert` with no destinations.
func New(gate *permissions.Gate, cfg *config.Config) (tool.Tool, error) {
	return newTool(gate, cfg, os.Getenv, time.Now, nil)
}

// newTool is New with the env lookup, clock, and HTTP client injectable
// for tests. getenv nil → os.Getenv, now nil → time.Now, client nil →
// a 10s-timeout default client.
func newTool(gate *permissions.Gate, cfg *config.Config, getenv func(string) string, now func() time.Time, client *http.Client) (tool.Tool, error) {
	h, err := newHandler(gate, cfg, getenv, now, client)
	if err != nil {
		return nil, err
	}
	return functiontool.New(
		functiontool.Config{Name: toolName, Description: buildDescription(h.order, h.targets)},
		h.run,
	)
}

// newHandler builds the tool's handler state. Extracted from newTool so
// tests can drive h.run directly without going through ADK's functiontool
// arg-conversion wrapper (the fetchURLFunc pattern).
func newHandler(gate *permissions.Gate, cfg *config.Config, getenv func(string) string, now func() time.Time, client *http.Client) (*handler, error) {
	if gate == nil {
		return nil, errors.New("alert: gate is required")
	}
	if cfg == nil {
		return nil, errors.New("alert: cfg is required")
	}
	if len(cfg.Alerts.Targets) == 0 {
		return nil, errors.New("alert: no targets configured")
	}
	if getenv == nil {
		getenv = os.Getenv
	}
	targets := make(map[string]config.AlertTarget, len(cfg.Alerts.Targets))
	order := make([]string, 0, len(cfg.Alerts.Targets))
	for _, t := range cfg.Alerts.Targets {
		targets[t.Name] = t
		order = append(order, t.Name)
	}
	limiter, err := newRateLimiter(cfg.Alerts.RateLimitPerTarget, now)
	if err != nil {
		return nil, err
	}
	if client == nil {
		client = &http.Client{Timeout: httpTimeout}
	}
	return &handler{
		gate:    gate,
		targets: targets,
		order:   order,
		limiter: limiter,
		client:  client,
		getenv:  getenv,
	}, nil
}

// handler holds the per-tool state closed over by the ADK function.
type handler struct {
	gate    *permissions.Gate
	targets map[string]config.AlertTarget
	order   []string // registry order, for deterministic errors + schema text
	limiter *rateLimiter
	client  *http.Client
	getenv  func(string) string
}

func (h *handler) run(ctx tool.Context, in Args) (Result, error) {
	if in.Target == "" {
		return Result{}, errors.New("alert: target is required")
	}
	tgt, ok := h.targets[in.Target]
	if !ok {
		return Result{}, fmt.Errorf("alert: unknown target %q (available: %s)", in.Target, strings.Join(h.order, ", "))
	}
	if in.Summary == "" {
		return Result{}, errors.New("alert: summary is required")
	}
	if _, ok := validLevels[in.Level]; !ok {
		return Result{}, fmt.Errorf("alert: level %q is invalid (want one of: info, warning, critical, resolved)", in.Level)
	}

	// Gate first — operators scope per target via
	// permissions.allow: ["alert:slack-oncall"] (or "alert:*" for all).
	if err := h.gate.CheckGeneric(ctx, toolName, in.Target); err != nil {
		return Result{}, err
	}

	if !h.limiter.allow(in.Target) {
		return Result{}, fmt.Errorf("alert: rate-limited on target %q (wait for the window to refill or fire a different target)", in.Target)
	}

	body, contentType, err := renderTemplate(tgt.Template, in)
	if err != nil {
		return Result{}, fmt.Errorf("alert: render template %q: %w", tgt.Template, err)
	}
	dest, err := resolveURL(tgt, h.getenv)
	if err != nil {
		return Result{}, err
	}

	// Parent the request on the inbound tool ctx (not context.Background)
	// so a turn-level cancel — /interrupt, daemon shutdown — aborts an
	// in-flight alert. tool.Context is an interface; some tests pass nil.
	parent := context.Context(ctx)
	if parent == nil {
		parent = context.Background()
	}
	req, err := http.NewRequestWithContext(parent, http.MethodPost, dest, bytes.NewReader(body))
	if err != nil {
		return Result{}, fmt.Errorf("alert: build request: %w", err)
	}
	req.Header.Set("Content-Type", contentType)
	if err := applyAuth(req, tgt.Auth, h.getenv); err != nil {
		return Result{}, err
	}

	start := time.Now()
	resp, err := h.client.Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("alert: post to %q: %w", in.Target, err)
	}
	defer func() { _ = resp.Body.Close() }()
	dur := time.Since(start)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrBodyBytes))
		msg := strings.TrimSpace(string(snippet))
		if msg == "" {
			return Result{}, fmt.Errorf("alert: target %q returned HTTP %d", in.Target, resp.StatusCode)
		}
		return Result{}, fmt.Errorf("alert: target %q returned HTTP %d: %s", in.Target, resp.StatusCode, msg)
	}

	return Result{Target: in.Target, StatusCode: resp.StatusCode, DurationMs: dur.Milliseconds()}, nil
}

// resolveURL returns the target's destination, reading url_env from the
// environment when the literal url is not set. (validateAlerts guarantees
// exactly one of the two is non-empty.)
func resolveURL(t config.AlertTarget, getenv func(string) string) (string, error) {
	if t.URL != "" {
		return t.URL, nil
	}
	v := strings.TrimSpace(getenv(t.URLEnv))
	if v == "" {
		return "", fmt.Errorf("alert: target %q: url_env %q is unset or empty", t.Name, t.URLEnv)
	}
	return v, nil
}

// applyAuth adds the per-target auth header, resolving the secret from
// env at call time so a rotated Secret is picked up without a restart.
// Nil auth → no header (the Slack Incoming Webhook case).
func applyAuth(req *http.Request, a *config.AlertAuth, getenv func(string) string) error {
	if a == nil {
		return nil
	}
	switch {
	case a.BearerEnv != "":
		tok := strings.TrimSpace(getenv(a.BearerEnv))
		if tok == "" {
			return fmt.Errorf("alert: auth.bearer_env %q is unset or empty", a.BearerEnv)
		}
		req.Header.Set("Authorization", "Bearer "+tok)
	case a.BasicEnvUser != "":
		user := getenv(a.BasicEnvUser)
		pass := getenv(a.BasicEnvPass)
		if user == "" || pass == "" {
			return errors.New("alert: auth.basic_env_user/basic_env_pass resolve to empty")
		}
		req.SetBasicAuth(user, pass)
	}
	return nil
}

// buildDescription renders the tool's LLM-facing description, enumerating
// the available targets (design OQ2: listing + describing targets beats
// an opaque free-string arg for the model's matching).
func buildDescription(order []string, targets map[string]config.AlertTarget) string {
	var b strings.Builder
	b.WriteString("Fire a pre-configured alert/notification webhook. Use for escalation, incident summaries, or notifying humans of agent decisions. ")
	b.WriteString("You can only fire targets the operator registered — there is no arbitrary-URL parameter. ")
	b.WriteString("Fire-and-forget: on a delivery failure the tool returns an error and you decide whether to retry, try another target, or give up.\n\n")
	b.WriteString("Available targets:")
	for _, name := range order {
		if desc := targets[name].Description; desc != "" {
			fmt.Fprintf(&b, "\n  - %s: %s", name, desc)
		} else {
			fmt.Fprintf(&b, "\n  - %s", name)
		}
	}
	return b.String()
}
