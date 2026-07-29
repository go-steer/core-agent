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

package models_test

import (
	"strings"
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/models"

	_ "github.com/go-steer/core-agent/v2/pkg/models/anthropic"
)

// TestNew_RoutesPerProviderOptions pins the #492 programmatic path:
// models.New must reach the same registry constructor Resolve would,
// carrying the option struct's fields — no on-disk config.Config
// fabrication required by the caller. Anthropic with an explicit key
// is the credential-free probe: construction succeeds without any
// env vars set.
func TestNew_RoutesPerProviderOptions(t *testing.T) {
	p, err := models.New(models.AnthropicAPI{APIKey: "test-key-not-real"})
	if err != nil {
		t.Fatalf("New(AnthropicAPI{APIKey}): %v", err)
	}
	if p == nil {
		t.Fatal("New returned a nil provider without error")
	}

	// Nil options: explicit fast-fail pointing at Resolve.
	if _, err := models.New(nil); err == nil || !strings.Contains(err.Error(), "Resolve") {
		t.Fatalf("New(nil) error = %v, want an error pointing at Resolve", err)
	}

	// Unregistered backend (gemini is not blank-imported anywhere in
	// this test binary): the registry error must tell the caller to
	// import the provider package, same as Resolve does.
	if _, err := models.New(models.GeminiAPI{APIKey: "k"}); err == nil || !strings.Contains(err.Error(), "import") {
		t.Fatalf("New(GeminiAPI) without the backend imported = %v, want the forget-to-import error", err)
	}
}
