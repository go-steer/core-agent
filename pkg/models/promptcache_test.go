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

package models

import (
	"context"
	"testing"
)

func TestPromptCacheSuppressed(t *testing.T) {
	t.Parallel()
	if PromptCacheSuppressed(context.Background()) {
		t.Error("a plain context reported suppression")
	}
	//nolint:staticcheck // SA1012: the nil case is the contract being tested.
	if PromptCacheSuppressed(nil) {
		t.Error("a nil context reported suppression")
	}
	if !PromptCacheSuppressed(WithoutPromptCache(context.Background())) {
		t.Error("WithoutPromptCache did not take")
	}
	// Inherited by children, since the side calls pass their ctx down
	// through timeouts and cancellation wrappers before it reaches the
	// provider.
	child, cancel := context.WithCancel(WithoutPromptCache(context.Background()))
	defer cancel()
	if !PromptCacheSuppressed(child) {
		t.Error("suppression did not survive a derived context")
	}
}
