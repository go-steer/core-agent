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
	"strings"
	"testing"

	"github.com/go-steer/core-agent/pkg/attach"
	"github.com/go-steer/core-agent/pkg/models/mock"
)

func TestAgent_AttachReload_NotRegistered_ReturnsSentinel(t *testing.T) {
	t.Parallel()

	provider := mock.NewEcho()
	m, err := provider.Model(context.Background(), "echo")
	if err != nil {
		t.Fatalf("model: %v", err)
	}
	a, err := New(m)
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}

	resp := a.AttachReload(context.Background())
	if resp.Memory || resp.Skills || resp.MCP {
		t.Errorf("AttachReload without reloader: surface flags = %+v, want all false", resp)
	}
	if len(resp.Errors) == 0 || !strings.Contains(resp.Errors[0], attach.ErrCapabilityNotRegistered.Error()) {
		t.Errorf("AttachReload without reloader: errors = %v, want one containing %q",
			resp.Errors, attach.ErrCapabilityNotRegistered.Error())
	}
}

func TestAgent_AttachReload_Wired_DelegatesToClosure(t *testing.T) {
	t.Parallel()

	provider := mock.NewEcho()
	m, err := provider.Model(context.Background(), "echo")
	if err != nil {
		t.Fatalf("model: %v", err)
	}
	called := false
	a, err := New(m, WithAttachReloader(func(_ context.Context) attach.ReloadResponse {
		called = true
		return attach.ReloadResponse{Memory: true, Skills: true, MCP: false, Errors: []string{"mcp: not yet"}}
	}))
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}

	resp := a.AttachReload(context.Background())
	if !called {
		t.Fatal("AttachReload: closure was not invoked")
	}
	if !resp.Memory || !resp.Skills || resp.MCP {
		t.Errorf("AttachReload: got %+v, want Memory=true Skills=true MCP=false", resp)
	}
	if len(resp.Errors) != 1 || resp.Errors[0] != "mcp: not yet" {
		t.Errorf("AttachReload: errors = %v, want [\"mcp: not yet\"]", resp.Errors)
	}
}
