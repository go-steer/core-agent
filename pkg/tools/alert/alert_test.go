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

package alert

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/adk/tool"

	"github.com/go-steer/core-agent/v2/pkg/config"
	"github.com/go-steer/core-agent/v2/pkg/permissions"
)

// captured records what a mock webhook server received.
type captured struct {
	method      string
	contentType string
	authHeader  string
	body        []byte
}

// mockServer returns an httptest server that records the last request and
// replies with status. It writes into *got.
func mockServer(t *testing.T, status int, reply string, got *captured) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		*got = captured{
			method:      r.Method,
			contentType: r.Header.Get("Content-Type"),
			authHeader:  r.Header.Get("Authorization"),
			body:        body,
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func yoloGate(t *testing.T) *permissions.Gate {
	t.Helper()
	return permissions.New(permissions.Options{Mode: permissions.ModeYolo})
}

// cfgWith builds a Config carrying exactly the given alert targets.
func cfgWith(targets ...config.AlertTarget) *config.Config {
	c := config.DefaultConfig()
	c.Alerts = config.AlertsConfig{Targets: targets}
	return c
}

func TestRun_GenericHappyPath(t *testing.T) {
	t.Parallel()
	var got captured
	srv := mockServer(t, 200, "ok", &got)
	cfg := cfgWith(config.AlertTarget{Name: "audit", URL: srv.URL, Template: config.AlertTemplateGeneric})

	h, err := newHandler(yoloGate(t), cfg, func(string) string { return "" }, nil, srv.Client())
	if err != nil {
		t.Fatalf("newHandler: %v", err)
	}
	res, err := h.run(tool.Context(nil), Args{
		Target:  "audit",
		Level:   "warning",
		Summary: "checkout-svc unresolved",
		Details: map[string]any{"cluster": "prod", "incident": "abc-123"},
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Target != "audit" || res.StatusCode != 200 {
		t.Errorf("result = %+v, want target=audit status=200", res)
	}
	if res.DurationMs < 0 {
		t.Errorf("duration_ms = %d, want >= 0", res.DurationMs)
	}
	if got.method != http.MethodPost {
		t.Errorf("method = %q, want POST", got.method)
	}
	if got.contentType != "application/json" {
		t.Errorf("content-type = %q, want application/json", got.contentType)
	}
	// The generic template is a flat JSON pass-through of level/summary/details.
	var payload map[string]any
	if err := json.Unmarshal(got.body, &payload); err != nil {
		t.Fatalf("payload not JSON: %v (%s)", err, got.body)
	}
	if payload["level"] != "warning" || payload["summary"] != "checkout-svc unresolved" {
		t.Errorf("payload = %v, want level=warning summary set", payload)
	}
	details, ok := payload["details"].(map[string]any)
	if !ok || details["cluster"] != "prod" {
		t.Errorf("payload details = %v, want cluster=prod", payload["details"])
	}
	if _, hasTS := payload["timestamp"]; hasTS {
		t.Errorf("generic payload should omit timestamp in this release, got %v", payload)
	}
}

func TestRun_OmitsDetailsWhenEmpty(t *testing.T) {
	t.Parallel()
	var got captured
	srv := mockServer(t, 204, "", &got)
	cfg := cfgWith(config.AlertTarget{Name: "audit", URL: srv.URL, Template: config.AlertTemplateGeneric})
	h, _ := newHandler(yoloGate(t), cfg, nil, nil, srv.Client())
	if _, err := h.run(tool.Context(nil), Args{Target: "audit", Level: "info", Summary: "hi"}); err != nil {
		t.Fatalf("run: %v", err)
	}
	var payload map[string]any
	_ = json.Unmarshal(got.body, &payload)
	if _, has := payload["details"]; has {
		t.Errorf("details should be omitted when empty, got %v", payload)
	}
}

func TestRun_URLEnvResolved(t *testing.T) {
	t.Parallel()
	var got captured
	srv := mockServer(t, 200, "ok", &got)
	cfg := cfgWith(config.AlertTarget{Name: "slack", URLEnv: "TEST_HOOK_URL", Template: config.AlertTemplateGeneric})
	env := func(k string) string {
		if k == "TEST_HOOK_URL" {
			return srv.URL
		}
		return ""
	}
	h, _ := newHandler(yoloGate(t), cfg, env, nil, srv.Client())
	if _, err := h.run(tool.Context(nil), Args{Target: "slack", Level: "info", Summary: "hi"}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got.method != http.MethodPost {
		t.Errorf("server not hit via url_env; method=%q", got.method)
	}
}

func TestRun_URLEnvUnset(t *testing.T) {
	t.Parallel()
	cfg := cfgWith(config.AlertTarget{Name: "slack", URLEnv: "TEST_HOOK_URL", Template: config.AlertTemplateGeneric})
	h, _ := newHandler(yoloGate(t), cfg, func(string) string { return "" }, nil, http.DefaultClient)
	_, err := h.run(tool.Context(nil), Args{Target: "slack", Level: "info", Summary: "hi"})
	if err == nil || !strings.Contains(err.Error(), "url_env") {
		t.Errorf("err = %v, want url_env unset error", err)
	}
}

func TestRun_BearerAuth(t *testing.T) {
	t.Parallel()
	var got captured
	srv := mockServer(t, 200, "ok", &got)
	cfg := cfgWith(config.AlertTarget{Name: "pd", URL: srv.URL, Template: config.AlertTemplateGeneric, Auth: &config.AlertAuth{BearerEnv: "PD_TOKEN"}})
	env := func(k string) string {
		if k == "PD_TOKEN" {
			return "s3cr3t"
		}
		return ""
	}
	h, _ := newHandler(yoloGate(t), cfg, env, nil, srv.Client())
	if _, err := h.run(tool.Context(nil), Args{Target: "pd", Level: "critical", Summary: "page"}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got.authHeader != "Bearer s3cr3t" {
		t.Errorf("auth header = %q, want Bearer s3cr3t", got.authHeader)
	}
}

func TestRun_BasicAuth(t *testing.T) {
	t.Parallel()
	var got captured
	srv := mockServer(t, 200, "ok", &got)
	cfg := cfgWith(config.AlertTarget{Name: "legacy", URL: srv.URL, Template: config.AlertTemplateGeneric, Auth: &config.AlertAuth{BasicEnvUser: "U_ENV", BasicEnvPass: "P_ENV"}})
	env := func(k string) string {
		switch k {
		case "U_ENV":
			return "user"
		case "P_ENV":
			return "pass"
		}
		return ""
	}
	h, _ := newHandler(yoloGate(t), cfg, env, nil, srv.Client())
	if _, err := h.run(tool.Context(nil), Args{Target: "legacy", Level: "info", Summary: "hi"}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.HasPrefix(got.authHeader, "Basic ") {
		t.Errorf("auth header = %q, want Basic ...", got.authHeader)
	}
}

func TestRun_BearerEnvUnset(t *testing.T) {
	t.Parallel()
	var got captured
	srv := mockServer(t, 200, "ok", &got)
	cfg := cfgWith(config.AlertTarget{Name: "pd", URL: srv.URL, Template: config.AlertTemplateGeneric, Auth: &config.AlertAuth{BearerEnv: "PD_TOKEN"}})
	h, _ := newHandler(yoloGate(t), cfg, func(string) string { return "" }, nil, srv.Client())
	_, err := h.run(tool.Context(nil), Args{Target: "pd", Level: "info", Summary: "hi"})
	if err == nil || !strings.Contains(err.Error(), "bearer_env") {
		t.Errorf("err = %v, want bearer_env unset error", err)
	}
	if got.method != "" {
		t.Errorf("request should not be sent when auth resolution fails; got method %q", got.method)
	}
}

func TestRun_UnknownTarget(t *testing.T) {
	t.Parallel()
	cfg := cfgWith(config.AlertTarget{Name: "audit", URL: "https://example.com", Template: config.AlertTemplateGeneric})
	h, _ := newHandler(yoloGate(t), cfg, nil, nil, http.DefaultClient)
	_, err := h.run(tool.Context(nil), Args{Target: "nope", Level: "info", Summary: "hi"})
	if err == nil || !strings.Contains(err.Error(), "unknown target") {
		t.Errorf("err = %v, want unknown target error", err)
	}
	if err != nil && !strings.Contains(err.Error(), "audit") {
		t.Errorf("err %q should list the available target 'audit'", err)
	}
}

func TestRun_InvalidLevel(t *testing.T) {
	t.Parallel()
	cfg := cfgWith(config.AlertTarget{Name: "audit", URL: "https://example.com", Template: config.AlertTemplateGeneric})
	h, _ := newHandler(yoloGate(t), cfg, nil, nil, http.DefaultClient)
	_, err := h.run(tool.Context(nil), Args{Target: "audit", Level: "loud", Summary: "hi"})
	if err == nil || !strings.Contains(err.Error(), "level") {
		t.Errorf("err = %v, want invalid level error", err)
	}
}

func TestRun_MissingRequiredArgs(t *testing.T) {
	t.Parallel()
	cfg := cfgWith(config.AlertTarget{Name: "audit", URL: "https://example.com", Template: config.AlertTemplateGeneric})
	h, _ := newHandler(yoloGate(t), cfg, nil, nil, http.DefaultClient)
	if _, err := h.run(tool.Context(nil), Args{Level: "info", Summary: "hi"}); err == nil {
		t.Error("missing target should error")
	}
	if _, err := h.run(tool.Context(nil), Args{Target: "audit", Level: "info"}); err == nil {
		t.Error("missing summary should error")
	}
}

func TestRun_Non2xxReturnsError(t *testing.T) {
	t.Parallel()
	var got captured
	srv := mockServer(t, 500, "boom-details", &got)
	cfg := cfgWith(config.AlertTarget{Name: "audit", URL: srv.URL, Template: config.AlertTemplateGeneric})
	h, _ := newHandler(yoloGate(t), cfg, nil, nil, srv.Client())
	_, err := h.run(tool.Context(nil), Args{Target: "audit", Level: "info", Summary: "hi"})
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Errorf("err = %v, want HTTP 500 error", err)
	}
	if err != nil && !strings.Contains(err.Error(), "boom-details") {
		t.Errorf("err %q should include the response snippet", err)
	}
}

func TestRun_GateDenied(t *testing.T) {
	t.Parallel()
	var got captured
	srv := mockServer(t, 200, "ok", &got)
	cfg := cfgWith(config.AlertTarget{Name: "audit", URL: srv.URL, Template: config.AlertTemplateGeneric})
	// A deny pattern short-circuits the gate before HTTP (deny wins in
	// every mode, including yolo).
	pol, err := permissions.NewPolicy(nil, []string{"alert:*"})
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	gate := permissions.New(permissions.Options{Mode: permissions.ModeYolo, Policy: pol})
	h, _ := newHandler(gate, cfg, nil, nil, srv.Client())
	if _, runErr := h.run(tool.Context(nil), Args{Target: "audit", Level: "info", Summary: "hi"}); runErr == nil {
		t.Fatal("gate should deny when a deny pattern matches the target")
	}
	if got.method != "" {
		t.Errorf("no request should be sent when the gate denies; got method %q", got.method)
	}
}

func TestRun_PerTargetGateScoping(t *testing.T) {
	t.Parallel()
	var got captured
	srv := mockServer(t, 200, "ok", &got)
	cfg := cfgWith(
		config.AlertTarget{Name: "allowed", URL: srv.URL, Template: config.AlertTemplateGeneric},
		config.AlertTarget{Name: "blocked", URL: srv.URL, Template: config.AlertTemplateGeneric},
	)
	// A per-target deny pattern proves the gate is keyed by "alert:<target>":
	// "blocked" is denied while "allowed" passes (yolo allows the rest).
	pol, err := permissions.NewPolicy(nil, []string{"alert:blocked"})
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	gate := permissions.New(permissions.Options{Mode: permissions.ModeYolo, Policy: pol})
	h, _ := newHandler(gate, cfg, nil, nil, srv.Client())

	if _, err := h.run(tool.Context(nil), Args{Target: "allowed", Level: "info", Summary: "hi"}); err != nil {
		t.Errorf("allowed target should pass the gate, got %v", err)
	}
	if _, err := h.run(tool.Context(nil), Args{Target: "blocked", Level: "info", Summary: "hi"}); err == nil {
		t.Error("blocked target should be denied by the gate")
	}
}

func TestNew_BuildsToolWithTargetsInDescription(t *testing.T) {
	t.Parallel()
	cfg := cfgWith(
		config.AlertTarget{Name: "slack-oncall", URL: "https://example.com", Template: config.AlertTemplateGeneric, Description: "post to #sre-oncall"},
		config.AlertTarget{Name: "audit", URL: "https://example.com", Template: config.AlertTemplateGeneric},
	)
	tl, err := New(yoloGate(t), cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if tl.Name() != "alert" {
		t.Errorf("Name = %q, want alert", tl.Name())
	}
	desc := tl.Description()
	for _, want := range []string{"slack-oncall", "post to #sre-oncall", "audit"} {
		if !strings.Contains(desc, want) {
			t.Errorf("description missing %q; got:\n%s", want, desc)
		}
	}
}

func TestNew_Rejects(t *testing.T) {
	t.Parallel()
	if _, err := New(nil, cfgWith(config.AlertTarget{Name: "a", URL: "https://x", Template: config.AlertTemplateGeneric})); err == nil {
		t.Error("nil gate should error")
	}
	if _, err := New(yoloGate(t), nil); err == nil {
		t.Error("nil cfg should error")
	}
	if _, err := New(yoloGate(t), config.DefaultConfig()); err == nil {
		t.Error("no targets should error")
	}
}
