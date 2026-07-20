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
	"context"
	"fmt"

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

// Provider is the mock implementation of models.Provider. It holds a
// single model.LLM (echo or scripted) and returns it from Model — the
// modelID argument is ignored because mocks aren't model-specific.
type Provider struct {
	name string
	llm  adkmodel.LLM
}

// Name reports the provider identity ("echo" or "scripted").
func (p *Provider) Name() string { return p.name }

// Model returns the wrapped LLM. The modelID is accepted for interface
// compatibility but ignored; mock providers don't differentiate by
// model name.
func (p *Provider) Model(_ context.Context, _ string) (adkmodel.LLM, error) {
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

func newEcho(_ *config.Config) (models.Provider, error) {
	return NewEcho(), nil
}

func newScripted(cfg *config.Config) (models.Provider, error) {
	if cfg.Mock.Script == "" {
		return nil, fmt.Errorf("scripted: mock.script is required (set in config or pass --script PATH)")
	}
	return NewScripted(cfg.Mock.Script, cfg.Mock.Strict)
}
