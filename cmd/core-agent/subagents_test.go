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

package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"

	"github.com/go-steer/core-agent/v2/pkg/agent"
	"github.com/go-steer/core-agent/v2/pkg/config"
	"github.com/go-steer/core-agent/v2/pkg/eventlog"
	"github.com/go-steer/core-agent/v2/pkg/models/mock"
)

// identityInterp is the no-op interpolator: subagent tests don't exercise
// ${env:...} substitution (covered by pkg/instruction), only the plumbing.
func identityInterp(s string) string { return s }

// discard swallows the human-facing progress lines buildDeclaredSubagents
// emits — tests assert on returned agents, not on narration.
func discard(string) {}

// writeScript drops a one-turn JSONL transcript so the scripted provider
// resolves without a real recording. resolveSubagentModel only needs the
// provider to build; it never plays the turn back.
func writeScript(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "script.jsonl")
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write script: %v", err)
	}
	return path
}

// TestBuildDeclaredSubagents_OwnModelAndIdentity is the γ.2 acceptance
// check: a declared subagent is built with its own name / description /
// instructions and, when it declares a Model, runs on that model — distinct
// from the parent's. Parent is echo; the "cluster" subagent is scripted, so
// the two are trivially distinguishable by LLM identity.
func TestBuildDeclaredSubagents_OwnModelAndIdentity(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{
		Model: config.ModelConfig{Provider: mock.ProviderEcho, Name: "echo"},
		// Inherited by the scripted subagent's cfg copy in
		// resolveSubagentModel (newScripted reads Mock.Script).
		Mock: config.MockConfig{Script: writeScript(t)},
		Subagents: []config.SubagentSpec{{
			Name:         "cluster",
			Description:  "read-only cluster investigator",
			Instructions: "You are a read-only cluster investigator.\n",
			Model:        &config.ModelConfig{Provider: mock.ProviderScripted, Name: "scripted-x"},
		}},
	}

	subs, err := buildDeclaredSubagents(
		context.Background(), cfg, mock.NewEcho(), t.TempDir(),
		nil, nil, identityInterp, discard,
	)
	if err != nil {
		t.Fatalf("buildDeclaredSubagents: %v", err)
	}
	if len(subs) != 1 {
		t.Fatalf("expected exactly 1 subagent, got %d", len(subs))
	}
	sub := subs[0]
	if got := sub.Inner().Name(); got != "cluster" {
		t.Errorf("subagent name = %q, want %q", got, "cluster")
	}
	if got := sub.Description(); got != "read-only cluster investigator" {
		t.Errorf("subagent description = %q, want %q", got, "read-only cluster investigator")
	}
	// Own model: scripted, NOT the parent's echo.
	if got := sub.Model().Name(); got != "scripted" {
		t.Errorf("subagent LLM = %q, want %q (its own model, not the parent's)", got, "scripted")
	}
}

// TestBuildDeclaredSubagents_InheritsParentModel: when a spec omits Model,
// resolveSubagentModel reuses the parent provider — the subagent runs on the
// parent's model (echo here), the OQ2 "inherit when unset" default.
func TestBuildDeclaredSubagents_InheritsParentModel(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{
		Model: config.ModelConfig{Provider: mock.ProviderEcho, Name: "echo"},
		Subagents: []config.SubagentSpec{{
			Name:        "helper",
			Description: "inherits the parent model",
		}},
	}
	subs, err := buildDeclaredSubagents(
		context.Background(), cfg, mock.NewEcho(), t.TempDir(),
		nil, nil, identityInterp, discard,
	)
	if err != nil {
		t.Fatalf("buildDeclaredSubagents: %v", err)
	}
	if len(subs) != 1 {
		t.Fatalf("expected 1 subagent, got %d", len(subs))
	}
	if got := subs[0].Model().Name(); got != "echo" {
		t.Errorf("inherited LLM = %q, want %q (parent's model)", got, "echo")
	}
}

// TestBuildDeclaredSubagents_NoneDeclared: an empty Subagents block returns
// (nil, nil) so the caller skips agent.WithSubagents entirely.
func TestBuildDeclaredSubagents_NoneDeclared(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Model: config.ModelConfig{Provider: mock.ProviderEcho, Name: "echo"}}
	subs, err := buildDeclaredSubagents(
		context.Background(), cfg, mock.NewEcho(), t.TempDir(),
		nil, nil, identityInterp, discard,
	)
	if err != nil {
		t.Fatalf("buildDeclaredSubagents: %v", err)
	}
	if subs != nil {
		t.Errorf("expected nil slice when no subagents declared, got %v", subs)
	}
}

// TestBuildDeclaredSubagents_RegisteredAsParentTool wires the built
// subagents into a parent via agent.WithSubagents and asserts the named
// tool shows up on the parent — the "invoked by name" half of γ.2. The
// parent needs a session-backed event log for WithSubagents to resolve the
// subsession service, matching the real cmd path.
func TestBuildDeclaredSubagents_RegisteredAsParentTool(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{
		Model: config.ModelConfig{Provider: mock.ProviderEcho, Name: "echo"},
		Subagents: []config.SubagentSpec{{
			Name:        "cluster",
			Description: "read-only cluster investigator",
		}},
	}
	subs, err := buildDeclaredSubagents(
		context.Background(), cfg, mock.NewEcho(), t.TempDir(),
		nil, nil, identityInterp, discard,
	)
	if err != nil {
		t.Fatalf("buildDeclaredSubagents: %v", err)
	}

	dsn := filepath.Join(t.TempDir(), "session.db")
	h, err := eventlog.Open(context.Background(), sqlite.Open(dsn))
	if err != nil {
		t.Fatalf("eventlog.Open: %v", err)
	}
	defer func() { _ = h.Close() }()

	parentLLM, err := mock.NewEcho().Model(context.Background(), "echo")
	if err != nil {
		t.Fatalf("parent model: %v", err)
	}
	parent, err := agent.New(parentLLM,
		agent.WithName("platform"),
		agent.WithEventLog(h),
		agent.WithSession("u", "p"),
		agent.WithSubagents(subs),
	)
	if err != nil {
		t.Fatalf("New parent: %v", err)
	}
	var found bool
	for _, tl := range parent.Tools() {
		if tl.Name() == "cluster" {
			found = true
		}
	}
	if !found {
		t.Error("declared subagent should register as a parent tool named \"cluster\"")
	}
}
