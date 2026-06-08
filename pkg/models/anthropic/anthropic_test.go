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

package anthropic

import (
	"testing"

	"github.com/go-steer/core-agent/pkg/models"
)

// TestProviderImplementsSmallModelDefaulter is a compile-time sanity
// check that *Provider satisfies models.SmallModelDefaulter, so
// ResolveSmallModel routes Anthropic subtasks to the cheap-tier
// default without requiring --agentic-small-model on the CLI.
func TestProviderImplementsSmallModelDefaulter(t *testing.T) {
	var _ models.SmallModelDefaulter = (*Provider)(nil)
}

func TestDefaultSmallModel(t *testing.T) {
	// Use a zero-value Provider — DefaultSmallModel doesn't depend on
	// any provider state (client, builtins, etc.), so this is safe and
	// avoids the API-key requirement of the New() constructor.
	p := &Provider{}
	if got, want := p.DefaultSmallModel(), DefaultSmallModelID; got != want {
		t.Errorf("DefaultSmallModel() = %q, want %q", got, want)
	}
	if DefaultSmallModelID != "claude-haiku-4-5" {
		t.Errorf("DefaultSmallModelID = %q; expected the haiku-4-5 alias used elsewhere in the codebase", DefaultSmallModelID)
	}
}
