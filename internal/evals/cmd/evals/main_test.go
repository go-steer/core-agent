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
	"testing"

	"github.com/go-steer/core-agent/v3/internal/evals/evalcompose"
)

// TestPromptCacheFlag: --prompt-cache stands in for a bundle's
// prompt_cache block; unset is the product default, on at 5 minutes.
func TestPromptCacheFlag(t *testing.T) {
	for in, want := range map[string]struct {
		on  bool
		ttl string
	}{
		"":    {true, evalcompose.PromptCacheTTL5m},
		"off": {false, evalcompose.PromptCacheTTL5m},
		"5m":  {true, evalcompose.PromptCacheTTL5m},
		"1h":  {true, evalcompose.PromptCacheTTL1h},
	} {
		pc, err := promptCache(in)
		if err != nil {
			t.Fatalf("promptCache(%q): %v", in, err)
		}
		if pc.On() != want.on || pc.EffectiveTTL() != want.ttl {
			t.Errorf("promptCache(%q) = on %v ttl %q, want on %v ttl %q", in, pc.On(), pc.EffectiveTTL(), want.on, want.ttl)
		}
	}
	if _, err := promptCache("60m"); err == nil {
		t.Error("promptCache(60m) = nil error, want a refusal")
	}
}
