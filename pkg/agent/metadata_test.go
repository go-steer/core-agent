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

package agent_test

import (
	"context"
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/agent"
	"github.com/go-steer/core-agent/v2/pkg/auth"
	"github.com/go-steer/core-agent/v2/pkg/eventlog"
)

func TestEventlogMetadataExtractor_NoCallerReturnsNil(t *testing.T) {
	t.Parallel()
	fn := agent.EventlogMetadataExtractor()
	got := fn(context.Background())
	if got != nil {
		t.Errorf("no caller on context should produce nil metadata; got %v", got)
	}
}

func TestEventlogMetadataExtractor_EmptyIdentityReturnsNil(t *testing.T) {
	t.Parallel()
	// A Caller-on-context with no Identity is treated as "no auth
	// info" — same as the bare context.
	fn := agent.EventlogMetadataExtractor()
	ctx := auth.WithCaller(context.Background(), auth.Caller{})
	got := fn(ctx)
	if got != nil {
		t.Errorf("empty-identity Caller should produce nil metadata; got %v", got)
	}
}

func TestEventlogMetadataExtractor_CallerOnly(t *testing.T) {
	t.Parallel()
	fn := agent.EventlogMetadataExtractor()
	ctx := auth.WithCaller(context.Background(), auth.Caller{Identity: "alice@example.com"})
	got := fn(ctx)
	if got == nil {
		t.Fatal("non-empty Caller must produce non-nil metadata")
	}
	if got[eventlog.MetadataKeyCaller] != "alice@example.com" {
		t.Errorf("MetadataKeyCaller: got %q, want %q", got[eventlog.MetadataKeyCaller], "alice@example.com")
	}
	if _, ok := got[eventlog.MetadataKeyProxyBy]; ok {
		t.Error("MetadataKeyProxyBy must NOT be set when no ProxyBy is on context")
	}
}

func TestEventlogMetadataExtractor_CallerWithProxy(t *testing.T) {
	t.Parallel()
	fn := agent.EventlogMetadataExtractor()
	ctx := auth.WithCaller(context.Background(), auth.Caller{Identity: "alice@example.com"})
	ctx = auth.WithProxyBy(ctx, "sa:slack-bot")
	got := fn(ctx)
	if got[eventlog.MetadataKeyCaller] != "alice@example.com" {
		t.Errorf("MetadataKeyCaller: got %q, want %q", got[eventlog.MetadataKeyCaller], "alice@example.com")
	}
	if got[eventlog.MetadataKeyProxyBy] != "sa:slack-bot" {
		t.Errorf("MetadataKeyProxyBy: got %q, want %q", got[eventlog.MetadataKeyProxyBy], "sa:slack-bot")
	}
}
