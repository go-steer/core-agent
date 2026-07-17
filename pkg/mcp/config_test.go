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

package mcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoad_MissingIsEmpty(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	got, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Servers) != 0 {
		t.Errorf("expected no servers, got %+v", got)
	}
}

func TestLoad_StdioParse(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	body := `{"version":1,"servers":{"fs":{"transport":"stdio","command":"mcp-fs","args":["--root","/tmp"],"env":{"X":"y"}}}}`
	if err := os.WriteFile(filepath.Join(dir, MCPFileName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	spec, ok := got.Servers["fs"]
	if !ok {
		t.Fatalf("missing fs server: %+v", got)
	}
	if spec.Transport != "stdio" || spec.Command != "mcp-fs" {
		t.Errorf("wrong fields: %+v", spec)
	}
	if len(spec.Args) != 2 || spec.Env["X"] != "y" {
		t.Errorf("args/env not parsed: %+v", spec)
	}
}

func TestLoad_RejectsBadTransport(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	body := `{"version":1,"servers":{"x":{"transport":"smoke-signals"}}}`
	if err := os.WriteFile(filepath.Join(dir, MCPFileName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(dir)
	if err == nil || !strings.Contains(err.Error(), "unknown transport") {
		t.Fatalf("expected unknown-transport error, got %v", err)
	}
}

func TestLoad_RejectsStdioWithURL(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	body := `{"version":1,"servers":{"x":{"transport":"stdio","command":"a","url":"https://b"}}}`
	if err := os.WriteFile(filepath.Join(dir, MCPFileName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(dir)
	if err == nil || !strings.Contains(err.Error(), "must not set url") {
		t.Fatalf("expected validation error, got %v", err)
	}
}

func TestInterpolateEnv(t *testing.T) {
	t.Setenv("FOO", "bar")
	if got := InterpolateEnv("Bearer ${env:FOO}"); got != "Bearer bar" {
		t.Errorf("got %q", got)
	}
	if got := InterpolateEnv("${env:NOT_SET}"); got != "" {
		t.Errorf("unset env should be empty: %q", got)
	}
	if got := InterpolateEnv("plain text"); got != "plain text" {
		t.Errorf("non-template should pass through: %q", got)
	}
}

func TestLoad_HTTPWithGoogleOAuth_Parse(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	body := `{"version":1,"servers":{"gke":{"transport":"http","url":"https://container.googleapis.com/mcp","auth":{"google_oauth":{"scopes":["https://www.googleapis.com/auth/container.read-only"]}}}}}`
	if err := os.WriteFile(filepath.Join(dir, MCPFileName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	spec, ok := got.Servers["gke"]
	if !ok {
		t.Fatalf("missing gke server: %+v", got)
	}
	if spec.Auth == nil || spec.Auth.GoogleOAuth == nil {
		t.Fatalf("auth.google_oauth not parsed: %+v", spec)
	}
	if got, want := spec.Auth.GoogleOAuth.Scopes, []string{"https://www.googleapis.com/auth/container.read-only"}; len(got) != 1 || got[0] != want[0] {
		t.Errorf("scopes wrong: got %v want %v", got, want)
	}
}

func TestLoad_RejectsAuthOnStdio(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	body := `{"version":1,"servers":{"x":{"transport":"stdio","command":"a","auth":{"google_oauth":{"scopes":["s"]}}}}}`
	if err := os.WriteFile(filepath.Join(dir, MCPFileName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(dir)
	if err == nil || !strings.Contains(err.Error(), "auth is only valid for http transport") {
		t.Fatalf("expected stdio-with-auth error, got %v", err)
	}
}

func TestLoad_RejectsGoogleOAuthEmptyScopes(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	body := `{"version":1,"servers":{"gke":{"transport":"http","url":"https://x","auth":{"google_oauth":{"scopes":[]}}}}}`
	if err := os.WriteFile(filepath.Join(dir, MCPFileName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(dir)
	if err == nil || !strings.Contains(err.Error(), "must list at least one scope") {
		t.Fatalf("expected empty-scopes error, got %v", err)
	}
}

func TestLoad_RejectsGoogleOAuthEmptyScopeString(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	body := `{"version":1,"servers":{"gke":{"transport":"http","url":"https://x","auth":{"google_oauth":{"scopes":["valid",""]}}}}}`
	if err := os.WriteFile(filepath.Join(dir, MCPFileName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(dir)
	if err == nil || !strings.Contains(err.Error(), "scopes[1] is empty") {
		t.Fatalf("expected empty-scope-string error, got %v", err)
	}
}

func TestLoad_RejectsAuthWithoutStrategy(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	body := `{"version":1,"servers":{"x":{"transport":"http","url":"https://x","auth":{}}}}`
	if err := os.WriteFile(filepath.Join(dir, MCPFileName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(dir)
	if err == nil || !strings.Contains(err.Error(), "no strategy is configured") {
		t.Fatalf("expected empty-AuthSpec error, got %v", err)
	}
}

// TestAgenticWrapLLMEnabled pins the opt-in default for the LLM
// second-chance path (#223): absence == off (the opposite of
// AgenticWrap). The subagent trades wall-clock + cost for its
// compression win, so we make operators opt in rather than
// discovering the trade-off in production.
func TestAgenticWrapLLMEnabled(t *testing.T) {
	t.Parallel()
	yes, no := true, false
	tests := []struct {
		name string
		s    *Servers
		want bool
	}{
		{"nil receiver", nil, false},
		{"absent field defaults off", &Servers{}, false},
		{"explicit false", &Servers{AgenticWrapLLM: &no}, false},
		{"explicit true", &Servers{AgenticWrapLLM: &yes}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.s.AgenticWrapLLMEnabled(); got != tt.want {
				t.Errorf("AgenticWrapLLMEnabled() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestLoad_AgenticWrapLLMFields pins that mcp.json's agentic_wrap_llm
// + agentic_wrap_model round-trip through Load. Regression signal:
// if this fails, operators lose the config-file path for enabling
// the LLM subagent — CLI flag becomes the only knob.
func TestLoad_AgenticWrapLLMFields(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	body := `{"version":1,"servers":{"x":{"transport":"stdio","command":"a"}},"agentic_wrap_llm":true,"agentic_wrap_model":"gemini-2.5-flash"}`
	if err := os.WriteFile(filepath.Join(dir, MCPFileName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !got.AgenticWrapLLMEnabled() {
		t.Errorf("AgenticWrapLLMEnabled() = false, want true (agentic_wrap_llm: true in mcp.json)")
	}
	if got.AgenticWrapModel != "gemini-2.5-flash" {
		t.Errorf("AgenticWrapModel = %q, want gemini-2.5-flash", got.AgenticWrapModel)
	}
}

func TestInterpolateMap(t *testing.T) {
	t.Setenv("TOKEN", "secret")
	got := InterpolateMap(map[string]string{
		"Authorization": "Bearer ${env:TOKEN}",
		"X-Static":      "value",
	})
	if got["Authorization"] != "Bearer secret" || got["X-Static"] != "value" {
		t.Errorf("map interpolation wrong: %+v", got)
	}
}
