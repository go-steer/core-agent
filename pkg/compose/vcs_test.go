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
	"path/filepath"
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	"google.golang.org/adk/v2/session"

	"github.com/go-steer/core-agent/v3/pkg/config"
	"github.com/go-steer/core-agent/v3/pkg/eventlog"
	"github.com/go-steer/core-agent/v3/pkg/permissions"
	"github.com/go-steer/core-agent/v3/pkg/tools/vcs"
)

func TestBuildVCSTools_UpdatesRegisteredToolsAndHonorsExtraDisables(t *testing.T) {
	t.Parallel()
	cfg := config.DefaultConfig()
	cfg.VCS = config.VCSConfig{
		Forges: []config.VCSForgeConfig{{
			Name:     "gh",
			Provider: config.VCSProviderGitHub,
			TokenEnv: "GH_TOKEN",
		}},
		Repositories: []config.VCSRepositoryConfig{{
			Forge:      "gh",
			Repository: "acme/infra",
			Role:       config.VCSRoleGitOps,
			BaseBranch: "main",
		}},
	}
	gate := permissions.New(permissions.Options{Mode: permissions.ModeYolo})
	gate.SetRegisteredTools(map[string]bool{"read_file": true})

	built, err := BuildVCSTools(cfg, gate, t.TempDir(), []string{"vcs_publish"})
	if err != nil {
		t.Fatalf("BuildVCSTools: %v", err)
	}
	if len(built) != len(vcs.ToolNames())-1 {
		t.Fatalf("len(built) = %d, want %d", len(built), len(vcs.ToolNames())-1)
	}
	reg := gate.RegisteredTools()
	if !reg["read_file"] || !reg["vcs_clone"] || !reg["vcs_propose"] {
		t.Errorf("gate.RegisteredTools() = %v, missing expected tools", reg)
	}
	if reg["vcs_publish"] {
		t.Errorf("disabled vcs_publish should not be registered on gate: %v", reg)
	}
}

func TestFormatVCSStartupLine(t *testing.T) {
	t.Parallel()
	if got := FormatVCSStartupLine(config.DefaultConfig()); got != "" {
		t.Errorf("FormatVCSStartupLine(default) = %q, want empty", got)
	}
	cfg := config.DefaultConfig()
	cfg.VCS = config.VCSConfig{
		BranchPrefix: "gitops/",
		Forges: []config.VCSForgeConfig{{
			Name:     "ghes",
			Provider: config.VCSProviderGitHub,
			Host:     "git.internal",
			TokenEnv: "GH_TOKEN",
		}},
		Repositories: []config.VCSRepositoryConfig{{
			Forge:      "ghes",
			Repository: "acme/infra",
			Role:       config.VCSRoleGitOps,
			BaseBranch: "main",
		}},
	}
	got := FormatVCSStartupLine(cfg)
	for _, want := range []string{"vcs: 1 forge(s)", "ghes=github/git.internal", "acme/infra@ghes[role=gitops,base=main]", `branch_prefix="gitops/"`} {
		if !strings.Contains(got, want) {
			t.Errorf("FormatVCSStartupLine = %q, want substring %q", got, want)
		}
	}
}

func TestVCSEventSink_AppendsDurableAuditEvents(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "eventlog.db")
	handle, err := eventlog.Open(ctx, sqlite.Open(dsn))
	if err != nil {
		t.Fatalf("eventlog.Open: %v", err)
	}
	defer func() { _ = handle.Close() }()

	_, err = handle.Service.Create(ctx, &session.CreateRequest{
		AppName:   "core-agent",
		UserID:    "local",
		SessionID: "sess-vcs-1",
	})
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}

	sink := VCSEventSink(func() *eventlog.Handle { return handle })
	sink(ctx, vcs.Event{
		Kind:       vcs.EventKindPublished,
		Forge:      "gh",
		Repository: "acme/infra",
		Branch:     "agent/scale-up",
		Revision:   "abc1234",
		AppName:    "core-agent",
		UserID:     "local",
		SessionID:  "sess-vcs-1",
	})
	sink(ctx, vcs.Event{
		Kind:       vcs.EventKindProposed,
		Forge:      "gh",
		Repository: "acme/infra",
		Number:     42,
		URL:        "https://github.com/acme/infra/pull/42",
		AppName:    "core-agent",
		UserID:     "local",
		SessionID:  "sess-vcs-1",
	})

	getResp, err := handle.Service.Get(ctx, &session.GetRequest{
		AppName:   "core-agent",
		UserID:    "local",
		SessionID: "sess-vcs-1",
	})
	if err != nil {
		t.Fatalf("Get session: %v", err)
	}
	events := getResp.Session.Events()
	if events.Len() != 2 {
		t.Fatalf("session events len = %d, want 2", events.Len())
	}
	first := events.At(0)
	if first.CustomMetadata["core_agent_event_kind"] != vcs.EventKindPublished {
		t.Errorf("first event kind = %v, want %q", first.CustomMetadata["core_agent_event_kind"], vcs.EventKindPublished)
	}
	second := events.At(1)
	if second.CustomMetadata["core_agent_event_kind"] != vcs.EventKindProposed {
		t.Errorf("second event kind = %v, want %q", second.CustomMetadata["core_agent_event_kind"], vcs.EventKindProposed)
	}
}
