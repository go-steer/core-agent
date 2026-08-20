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

// Package mock ships two credential-free LLM providers and a recording
// wrapper that pair for offline testing of agent flows:
//
//   - "echo" returns the user's last message as the model response.
//     Zero config; useful for "does the binary boot?" smoke tests.
//
//   - "scripted" replays a JSONL transcript turn-by-turn. Pair it with
//     a recording captured against a real provider to exercise the
//     agent loop without burning API quota.
//
//   - NewRecorder wraps any model.LLM and appends each turn (request +
//     response stream) to an io.Writer as JSONL. Recorded transcripts
//     are consumed by the scripted provider via the same shared
//     RecordedTurn type defined in format.go.
//
// Tool execution at replay time uses the live environment, so the
// scripted provider faithfully replays the LLM side but not the wider
// tool surface — fine for testing prompt construction and loop shape,
// not for bit-exact session reproduction.
package mock

import (
	"bytes"
	"context"
	"fmt"
	"os"

	adkmodel "google.golang.org/adk/model"

	"github.com/go-steer/core-agent/v2/pkg/config"
	"github.com/go-steer/core-agent/v2/pkg/models"
)

// Provider names registered by this package.
const (
	ProviderEcho     = "echo"
	ProviderScripted = "scripted"
)

func init() {
	models.Register(ProviderEcho, newEcho)
	models.Register(ProviderScripted, newScripted)
}

// Provider is the mock implementation of models.Provider. It hands
// back either one shared model.LLM (echo, NewScripted) or a freshly
// built one per call (NewScriptedPerCall) — the modelID argument is
// ignored either way, because mocks aren't model-specific.
type Provider struct {
	name string
	llm  adkmodel.LLM
	// fresh, when non-nil, builds a new LLM for every Model call and
	// takes precedence over llm. Only NewScriptedPerCall sets it; see
	// there for why a shared replay is the wrong default for fan-out.
	fresh func() (adkmodel.LLM, error)
}

// Name reports the provider identity ("echo" or "scripted").
func (p *Provider) Name() string { return p.name }

// Model returns an LLM to run against. The modelID is accepted for
// interface compatibility but ignored; mock providers don't
// differentiate by model name.
//
// Whether repeated calls share one LLM depends on the constructor:
// NewEcho and NewScripted return the same instance every time,
// NewScriptedPerCall a new one per call.
func (p *Provider) Model(_ context.Context, _ string) (adkmodel.LLM, error) {
	if p.fresh != nil {
		return p.fresh()
	}
	return p.llm, nil
}

// NewEcho returns an echo Provider directly. Useful for library
// callers that want the mock without going through models.Resolve.
func NewEcho() *Provider {
	return &Provider{name: ProviderEcho, llm: echoLLM{}}
}

// NewScripted returns a scripted Provider that plays back the JSONL
// transcript at path. strict toggles request-shape validation per
// turn (see scriptedLLM for details).
//
// Every Model call gets the SAME replay, so the transcript is the
// whole conversation for one agent. For a fan-out where several
// agents each replay the transcript from its own first turn, use
// NewScriptedPerCall.
func NewScripted(path string, strict bool) (*Provider, error) {
	turns, err := loadScript(path)
	if err != nil {
		return nil, fmt.Errorf("scripted: %w", err)
	}
	return &Provider{
		name: ProviderScripted,
		llm:  &scriptedLLM{turns: turns, strict: strict},
	}, nil
}

// NewScriptedPerCall returns a scripted Provider that builds a NEW
// replay of the transcript at path on every Model call, so each caller
// starts at turn 0 with a cursor of its own.
//
// This is the constructor for hermetic fan-out. A background.Manager
// asks its provider for one LLM per spawn, so with NewScripted three
// concurrent children share a single cursor and interleave through one
// script: child A gets recorded turn 0, child B turn 1, child C turn 2,
// and which is which depends on goroutine scheduling. The cursor is
// mutex-guarded, so this is not a data race — which makes it worse, not
// better: -race stays silent while the replay is nondeterministic. With
// NewScriptedPerCall, every child replays the same script from the top,
// which is almost always what a fan-out fixture means.
//
// The transcript is read and parsed once, here, so a malformed one
// fails at construction rather than at the first spawn, and an edit to
// the file mid-run cannot change what a later spawn replays. Each Model
// call then re-parses those same bytes rather than sharing the decoded
// turns: recorded responses are pointers handed straight into the agent
// loop, and concurrent children must not be handed the same ones.
func NewScriptedPerCall(path string, strict bool) (*Provider, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("scripted: open: %w", err)
	}
	if _, err := decodeScript(bytes.NewReader(raw), path); err != nil {
		return nil, fmt.Errorf("scripted: %w", err)
	}
	return &Provider{
		name: ProviderScripted,
		fresh: func() (adkmodel.LLM, error) {
			turns, err := decodeScript(bytes.NewReader(raw), path)
			if err != nil {
				return nil, fmt.Errorf("scripted: %w", err)
			}
			return &scriptedLLM{turns: turns, strict: strict}, nil
		},
	}, nil
}

func newEcho(_ *config.Config) (models.Provider, error) {
	return NewEcho(), nil
}

func newScripted(cfg *config.Config) (models.Provider, error) {
	if cfg.Mock.Script == "" {
		return nil, fmt.Errorf("scripted: mock.script is required (set in config or pass --script PATH)")
	}
	return NewScripted(cfg.Mock.Script, cfg.Mock.Strict)
}
