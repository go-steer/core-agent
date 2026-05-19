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

// Package config defines the on-disk schema for `.agents/config.json` and
// the rules for discovering, parsing, and merging it with built-in defaults.
//
// A minimal config.json only needs to set what the consumer wants to override;
// all other fields fall back to DefaultConfig().
package config

import (
	"fmt"
)

// SchemaVersion is the current major version of the on-disk config format.
// Bump when making a breaking change; older versions are rejected at load
// time with a clear error suggesting the upgrade path.
const SchemaVersion = 1

// Config is the in-memory representation of `.agents/config.json`.
//
// All sub-sections except Model have sensible zero-valued defaults, so a
// minimal `config.json` only needs to set what the user wants to override.
type Config struct {
	Version     int               `json:"version"`
	Model       ModelConfig       `json:"model"`
	Permissions PermissionsConfig `json:"permissions,omitempty"`
	PathScope   PathScopeConfig   `json:"path_scope,omitempty"`
	Agent       AgentConfig       `json:"agent,omitempty"`
	ToolOutput  ToolOutputConfig  `json:"tool_output,omitempty"`
	Tools       ToolsConfig       `json:"tools,omitempty"`
	Mock        MockConfig        `json:"mock,omitempty"`
	OTEL        OTELConfig        `json:"otel,omitempty"`
}

// PathScopeConfig holds extra paths that file tools may read/write
// outside the default project + user-home scope. Patterns may be exact
// paths or directory globs (terminating "/...") and are typically
// appended via the "Always allow this path/tree" prompt path.
type PathScopeConfig struct {
	Allow []string `json:"allow,omitempty"`
}

// ModelConfig selects the LLM provider and model.
//
// Provider: one of "gemini", "vertex", "anthropic". When empty, the resolver
// auto-detects from the environment (see models.Resolve).
// Name: a model ID, e.g. "gemini-3.1-pro-preview-customtools" or "claude-opus-4-7".
// APIKey: optional inline key for Provider="gemini"; usually unset and
// read from GOOGLE_API_KEY at runtime.
// Vertex: required when Provider="vertex"; project + location.
// Anthropic: optional credentials for Provider="anthropic"; usually unset and
// read from ANTHROPIC_API_KEY at runtime.
type ModelConfig struct {
	Provider  string           `json:"provider,omitempty"`
	Name      string           `json:"name"`
	APIKey    string           `json:"api_key,omitempty"`
	Vertex    *VertexConfig    `json:"vertex,omitempty"`
	Anthropic *AnthropicConfig `json:"anthropic,omitempty"`
	Pricing   *PricingConfig   `json:"pricing,omitempty"`
}

// VertexConfig holds GCP-specific settings for the vertex provider.
type VertexConfig struct {
	Project  string `json:"project"`
	Location string `json:"location"`
}

// AnthropicConfig holds Claude-specific settings for the anthropic
// provider family. APIKey is used by the first-party "anthropic"
// provider (api.anthropic.com); Vertex is used by "anthropic-vertex"
// (Claude served via Google Vertex AI).
type AnthropicConfig struct {
	APIKey string        `json:"api_key,omitempty"`
	Vertex *VertexConfig `json:"vertex,omitempty"`
}

// PricingConfig overrides the built-in price table for cost estimation.
type PricingConfig struct {
	InputPerMTok  float64 `json:"input_per_mtok,omitempty"`
	OutputPerMTok float64 `json:"output_per_mtok,omitempty"`
}

// PermissionsConfig configures the permission gate.
type PermissionsConfig struct {
	Mode  string   `json:"mode,omitempty"`  // "ask" | "allow" | "yolo"
	Allow []string `json:"allow,omitempty"` // pattern allowlist
	Deny  []string `json:"deny,omitempty"`  // pattern denylist
}

// AgentConfig tunes runtime agent behavior.
type AgentConfig struct {
	MaxSteps int `json:"max_steps,omitempty"`
}

// ToolOutputConfig caps tool result size before it enters model context.
type ToolOutputConfig struct {
	MaxBytes int                              `json:"max_bytes,omitempty"`
	MaxLines int                              `json:"max_lines,omitempty"`
	PerTool  map[string]ToolOutputPerToolCaps `json:"per_tool,omitempty"`
}

// ToolOutputPerToolCaps overrides global tool-output limits for one tool.
type ToolOutputPerToolCaps struct {
	MaxBytes int `json:"max_bytes,omitempty"`
	MaxLines int `json:"max_lines,omitempty"`
}

// ToolsConfig configures the bundled CLI's built-in tool suite.
//
// Disable lists tools to turn off. Names must match the canonical
// built-in names (see tools.BuiltinToolNames). Unknown names cause
// a startup error from tools.BuiltinTools.Disable, so typos fail
// loudly rather than silently leaving a tool on.
//
// The CLI's --disable-tools flag composes with this list by union;
// --no-builtin-tools disables the entire suite and makes Disable moot.
type ToolsConfig struct {
	Disable []string `json:"disable,omitempty"`
}

// MockConfig configures the mock providers (echo, scripted) and the
// orthogonal recording wrapper.
//
// Script is the path to a JSONL transcript consumed by the scripted
// provider; it's required when model.provider is "scripted".
//
// Strict makes the scripted provider assert that each incoming
// request's Contents JSON-equal the recorded request. Off by default
// — the typical use is replaying without caring about prompt drift.
//
// Record is a path to write a JSONL recording of every LLM turn.
// Works with any provider, not just the mocks; lives in MockConfig
// because it shares the file format the scripted provider consumes.
type MockConfig struct {
	Script string `json:"script,omitempty"`
	Strict bool   `json:"strict,omitempty"`
	Record string `json:"record,omitempty"`
}

// OTELConfig configures the OpenTelemetry exporter.
type OTELConfig struct {
	Exporter string `json:"exporter,omitempty"` // "none" | "console" | "otlp"
	Endpoint string `json:"endpoint,omitempty"`
}

// Permission modes.
const (
	PermissionModeAsk   = "ask"
	PermissionModeAllow = "allow"
	PermissionModeYolo  = "yolo"
)

// Provider names recognized by the resolver.
const (
	ProviderGemini          = "gemini"
	ProviderVertex          = "vertex"
	ProviderAnthropic       = "anthropic"
	ProviderAnthropicVertex = "anthropic-vertex"
	ProviderEcho            = "echo"
	ProviderScripted        = "scripted"
)

// DefaultConfig returns a Config with all fields populated by sensible
// defaults. Override-then-merge happens at Load time.
func DefaultConfig() *Config {
	return &Config{
		Version: SchemaVersion,
		Model: ModelConfig{
			// Provider intentionally empty — resolver auto-detects from env.
			// `-customtools` is a Vertex behavioral variant of
			// gemini-3.1-pro-preview fine-tuned to prefer developer-defined
			// tools over raw shell — without it the model bypasses our
			// structured grep / read_file / edit_file and shells out via
			// bash, breaking the permission gate's coverage and never
			// batching tool calls. Same pricing, same context window, same
			// reasoning behavior. Override via Model.Name when a consumer
			// wants the un-tuned variant for behavior-baseline comparisons.
			Name: "gemini-3.1-pro-preview-customtools",
		},
		Permissions: PermissionsConfig{
			Mode: PermissionModeAsk,
		},
		Agent: AgentConfig{
			MaxSteps: 50,
		},
		ToolOutput: ToolOutputConfig{
			MaxBytes: 32 * 1024,
			MaxLines: 500,
			PerTool: map[string]ToolOutputPerToolCaps{
				"bash":            {MaxBytes: 64 * 1024, MaxLines: 2000},
				"read_file":       {MaxBytes: 256 * 1024, MaxLines: 5000},
				"read_many_files": {MaxBytes: 256 * 1024, MaxLines: 5000},
				"glob":            {MaxBytes: 32 * 1024, MaxLines: 500},
				"grep":            {MaxBytes: 256 * 1024, MaxLines: 5000},
			},
		},
		OTEL: OTELConfig{
			Exporter: "none",
		},
	}
}

// Validate returns an error if the config is internally inconsistent.
// Validation here is structural; environmental concerns (is GOOGLE_API_KEY
// set? does the GCP project exist?) are checked at provider-construction
// time so test fixtures don't need real creds.
func (c *Config) Validate() error {
	if c.Version != 0 && c.Version != SchemaVersion {
		return fmt.Errorf("config: unsupported schema version %d (expected %d); upgrade your .agents/config.json", c.Version, SchemaVersion)
	}
	if c.Model.Name == "" {
		return fmt.Errorf("config: model.name is required")
	}
	switch c.Model.Provider {
	case "", ProviderGemini, ProviderVertex, ProviderAnthropic, ProviderAnthropicVertex, ProviderEcho, ProviderScripted:
		// ok; "" means auto-detect at resolve time.
	default:
		return fmt.Errorf("config: unknown model.provider %q (want one of %q, %q, %q, %q, %q, %q)", c.Model.Provider, ProviderGemini, ProviderVertex, ProviderAnthropic, ProviderAnthropicVertex, ProviderEcho, ProviderScripted)
	}
	if c.Model.Provider == ProviderScripted && c.Mock.Script == "" {
		return fmt.Errorf("config: mock.script is required when provider is %q (or pass --script PATH)", ProviderScripted)
	}
	if c.Model.Provider == ProviderVertex && c.Model.Vertex != nil {
		if c.Model.Vertex.Project == "" || c.Model.Vertex.Location == "" {
			return fmt.Errorf("config: model.vertex.project and model.vertex.location are required when provider is %q (or set GOOGLE_CLOUD_PROJECT / GOOGLE_CLOUD_LOCATION)", ProviderVertex)
		}
	}
	if c.Model.Provider == ProviderAnthropicVertex && c.Model.Anthropic != nil && c.Model.Anthropic.Vertex != nil {
		if c.Model.Anthropic.Vertex.Project == "" || c.Model.Anthropic.Vertex.Location == "" {
			return fmt.Errorf("config: model.anthropic.vertex.project and model.anthropic.vertex.location are required when provider is %q (or set ANTHROPIC_VERTEX_PROJECT_ID / CLOUD_ML_REGION)", ProviderAnthropicVertex)
		}
	}
	switch c.Permissions.Mode {
	case "", PermissionModeAsk, PermissionModeAllow, PermissionModeYolo:
		// ok
	default:
		return fmt.Errorf("config: unknown permissions.mode %q", c.Permissions.Mode)
	}
	return nil
}
