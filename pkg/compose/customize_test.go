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

package compose

import (
	"context"
	"errors"
	"iter"
	"strings"
	"testing"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"

	"github.com/go-steer/core-agent/v2/pkg/auth"
	"github.com/go-steer/core-agent/v2/pkg/permissions"
)

// namedLLM is stubLLM with a controllable Name — the observable for
// asserting the Customize hook's model swap took effect.
type namedLLM struct{ name string }

func (m namedLLM) Name() string { return m.name }
func (namedLLM) GenerateContent(context.Context, *adkmodel.LLMRequest, bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		yield(nil, errors.New("namedLLM should not be invoked in this test"))
	}
}

func stubTool(t *testing.T, name string) tool.Tool {
	t.Helper()
	type empty struct{}
	tl, err := functiontool.New(
		functiontool.Config{Name: name, Description: "stub"},
		func(_ tool.Context, _ empty) (empty, error) { return empty{}, nil },
	)
	if err != nil {
		t.Fatalf("functiontool.New(%q): %v", name, err)
	}
	return tl
}

// TestReproduceAgent_CustomizeHook pins the #505 contract: the hook
// sees the caller and the daemon-wide defaults, its model/tool
// changes shape the constructed agent, and its slice appends never
// write through into the shared deps.
func TestReproduceAgent_CustomizeHook(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	shared := stubTool(t, "shared_tool")
	sharedTools := make([]tool.Tool, 1, 4) // spare capacity: the append-aliasing trap
	sharedTools[0] = shared

	var hookCaller auth.Caller
	var sawDefaults bool
	deps := SessionFactoryDeps{
		DaemonCtx:    ctx,
		Model:        namedLLM{name: "daemon-default"},
		Template:     permissions.New(permissions.Options{}),
		BuiltinTools: sharedTools,
		Customize: func(_ context.Context, caller auth.Caller, c *SessionCustomization) error {
			hookCaller = caller
			sawDefaults = c.Model.Name() == "daemon-default" && len(c.Tools) == 1
			c.Model = namedLLM{name: "tenant-tier"}
			c.Tools = append(c.Tools, stubTool(t, "tenant_tool"))
			return nil
		},
	}

	ad, cancelSess, err := ReproduceAgent(deps, auth.Caller{Identity: "alice@example.com"}, "sid-cust", "created")
	if err != nil {
		t.Fatalf("ReproduceAgent: %v", err)
	}
	t.Cleanup(cancelSess)

	if hookCaller.Identity != "alice@example.com" {
		t.Errorf("hook caller = %q, want alice@example.com", hookCaller.Identity)
	}
	if !sawDefaults {
		t.Error("hook did not receive the daemon-wide defaults pre-filled")
	}
	if got := ad.Agent().ModelName(); got != "tenant-tier" {
		t.Errorf("constructed agent model = %q, want the hook's tenant-tier", got)
	}
	toolNames := map[string]bool{}
	for _, ti := range ad.AttachTools() {
		toolNames[ti.Name] = true
	}
	if !toolNames["shared_tool"] || !toolNames["tenant_tool"] {
		t.Errorf("agent tools = %v, want shared_tool AND tenant_tool", toolNames)
	}
	// The hook's append must not write through into the shared deps
	// slice (the spare-capacity aliasing trap) or leak the tenant
	// tool into the NEXT session built from the same deps.
	if len(sharedTools) != 1 {
		t.Fatalf("deps.BuiltinTools grew to %d — hook append wrote through the shared backing array", len(sharedTools))
	}
	deps.Customize = nil
	ad2, cancel2, err := ReproduceAgent(deps, auth.Anonymous, "sid-plain", "created")
	if err != nil {
		t.Fatalf("ReproduceAgent (plain): %v", err)
	}
	t.Cleanup(cancel2)
	for _, ti := range ad2.AttachTools() {
		if ti.Name == "tenant_tool" {
			t.Fatal("tenant_tool leaked into a session built without the hook")
		}
	}
}

// TestReproduceAgent_CustomizeErrorAborts pins the fail-fast
// contract: a hook error aborts construction with the caller
// identity in the message and no session resources created.
func TestReproduceAgent_CustomizeErrorAborts(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	deps := SessionFactoryDeps{
		DaemonCtx: ctx,
		Model:     stubLLM{},
		Template:  permissions.New(permissions.Options{}),
		Customize: func(context.Context, auth.Caller, *SessionCustomization) error {
			return errors.New("tenant suspended")
		},
	}
	_, _, err := ReproduceAgent(deps, auth.Caller{Identity: "bob@example.com"}, "sid-x", "created")
	if err == nil || !strings.Contains(err.Error(), "tenant suspended") || !strings.Contains(err.Error(), "bob@example.com") {
		t.Fatalf("err = %v, want the hook's error wrapped with the caller identity", err)
	}
}

// TestReproduceAgent_CustomizeNilModelRestoresDefault pins the
// defensive guard: a hook that nils the model gets the daemon
// default back instead of a downstream construction panic.
func TestReproduceAgent_CustomizeNilModelRestoresDefault(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	deps := SessionFactoryDeps{
		DaemonCtx: ctx,
		Model:     namedLLM{name: "daemon-default"},
		Template:  permissions.New(permissions.Options{}),
		Customize: func(_ context.Context, _ auth.Caller, c *SessionCustomization) error {
			c.Model = nil
			return nil
		},
	}
	ad, cancelSess, err := ReproduceAgent(deps, auth.Anonymous, "sid-nil", "created")
	if err != nil {
		t.Fatalf("ReproduceAgent: %v", err)
	}
	t.Cleanup(cancelSess)
	if got := ad.Agent().ModelName(); got != "daemon-default" {
		t.Errorf("model = %q, want the restored daemon default", got)
	}
}
