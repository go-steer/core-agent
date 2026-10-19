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

package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

func TestWithCostPreflight_ComposesParentCheck(t *testing.T) {
	t.Parallel()

	var calls []string
	ctx := withCostPreflight(context.Background(), func(preflightCall) (preflightRelease, error) {
		calls = append(calls, "parent")
		return func(bool) { calls = append(calls, "release parent") }, nil
	})
	ctx = withCostPreflight(ctx, func(preflightCall) (preflightRelease, error) {
		calls = append(calls, "child")
		return func(bool) { calls = append(calls, "release child") }, nil
	})
	check := costPreflightFromContext(ctx)
	if check == nil {
		t.Fatal("composed context has no cost preflight")
	}
	release, err := check(preflightCall{req: &adkmodel.LLMRequest{}})
	if err != nil {
		t.Fatalf("composed check: %v", err)
	}
	if got, want := strings.Join(calls, ","), "parent,child"; got != want {
		t.Fatalf("checks called = %q, want %q", got, want)
	}
	release(true)
	if got, want := strings.Join(calls, ","), "parent,child,release child,release parent"; got != want {
		t.Errorf("check/release order = %q, want %q", got, want)
	}
}

func TestWithCostPreflight_ChildFailureReleasesParentReservation(t *testing.T) {
	t.Parallel()

	parentReleased, parentProduced := false, true
	childErr := errors.New("child budget exhausted")
	ctx := withCostPreflight(context.Background(), func(preflightCall) (preflightRelease, error) {
		return func(produced bool) { parentReleased, parentProduced = true, produced }, nil
	})
	ctx = withCostPreflight(ctx, func(preflightCall) (preflightRelease, error) {
		return nil, childErr
	})

	release, err := costPreflightFromContext(ctx)(preflightCall{req: &adkmodel.LLMRequest{}})
	if !errors.Is(err, childErr) {
		t.Fatalf("composed check error = %v, want %v", err, childErr)
	}
	if release != nil {
		t.Fatal("failed composed check returned a release function")
	}
	if !parentReleased {
		t.Error("parent reservation was not released after child check failed")
	}
	// The request is never sent, so no usage will settle the parent's
	// estimate: the release must say so, or the parent keeps it.
	if parentProduced {
		t.Error("parent was released as if the refused request had produced usage")
	}
}

func TestEstimateSerializedRequestTokens_ExcludesRuntimeTools(t *testing.T) {
	t.Parallel()

	req := &adkmodel.LLMRequest{
		Contents: []*genai.Content{genai.NewContentFromText("hello", genai.RoleUser)},
		Tools:    map[string]any{"runtime-only": make(chan int)},
	}
	if _, ok := estimateSerializedRequestTokens(req); !ok {
		t.Fatal("a runtime-only tool value made the wire request unestimable")
	}
}
