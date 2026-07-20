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

package auth_test

import (
	"context"
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/auth"
)

func TestCallerContext_RoundTrip(t *testing.T) {
	t.Parallel()
	want := auth.Caller{
		Identity: "alice@example.com",
		Labels:   map[string]string{"team": "platform"},
	}
	ctx := auth.WithCaller(context.Background(), want)
	got, ok := auth.CallerFromContext(ctx)
	if !ok {
		t.Fatal("CallerFromContext returned ok=false; expected the Caller we just stored")
	}
	if got.Identity != want.Identity {
		t.Errorf("Identity: got %q, want %q", got.Identity, want.Identity)
	}
	if got.Labels["team"] != "platform" {
		t.Errorf("Labels[team]: got %q, want %q", got.Labels["team"], "platform")
	}
}

func TestCallerContext_MissingReturnsNotOK(t *testing.T) {
	t.Parallel()
	_, ok := auth.CallerFromContext(context.Background())
	if ok {
		t.Error("CallerFromContext on a fresh context returned ok=true; expected false")
	}
}

func TestAnonymousCallerSentinel(t *testing.T) {
	t.Parallel()
	if auth.Anonymous.Identity != "anon" {
		t.Errorf("Anonymous.Identity: got %q, want %q (the documented default; changing this breaks operator-facing docs)", auth.Anonymous.Identity, "anon")
	}
	if auth.Anonymous.Admin {
		t.Error("Anonymous.Admin: must never be true")
	}
}
