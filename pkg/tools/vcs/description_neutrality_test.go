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

package vcs

import (
	"testing"

	"github.com/go-steer/core-agent/v3/internal/testutil"
	"github.com/go-steer/core-agent/v3/pkg/config"
	"github.com/go-steer/core-agent/v3/pkg/permissions"
)

func TestVCSToolsTextIsDeploymentNeutral(t *testing.T) {
	t.Parallel()
	cfg := config.DefaultConfig()
	cfg.VCS = config.VCSConfig{
		Forges: []config.VCSForgeConfig{
			{
				Name:     "gh",
				Provider: config.VCSProviderGitHub,
				TokenEnv: "GITHUB_TOKEN",
			},
		},
		Repositories: []config.VCSRepositoryConfig{
			{
				Forge:      "gh",
				Repository: "acme/infra",
				Role:       config.VCSRoleGitOps,
				BaseBranch: "main",
			},
		},
	}
	gate := permissions.New(permissions.Options{Mode: permissions.ModeYolo})
	built, err := Build(cfg, gate, t.TempDir())
	if err != nil {
		t.Fatalf("vcs.Build: %v", err)
	}
	wantNames := ToolNames()
	if len(built) != len(wantNames) {
		t.Fatalf("vcs.Build returned %d tools, want %d (%v)", len(built), len(wantNames), wantNames)
	}
	have := make(map[string]bool, len(built))
	for _, tl := range built {
		have[tl.Name()] = true
		if tl.Description() == "" {
			t.Errorf("tool %q has an empty description", tl.Name())
		}
		texts, scanned := testutil.ModelFacingText(tl)
		if !scanned {
			t.Errorf("tool %q exposes no arg schema to scan", tl.Name())
		}
		for _, text := range texts {
			for _, bad := range testutil.ModelFacingBanViolations(text) {
				t.Errorf("tool %q: %s\n  %s", tl.Name(), bad, text)
			}
		}
		refs, checked := testutil.UndeclaredArgRefs(tl)
		if !checked {
			t.Errorf("tool %q exposes no arg schema to cross-check its description against", tl.Name())
		}
		for _, ref := range refs {
			t.Errorf("tool %q description tells the model to set %q, which it does not declare:\n  %s", tl.Name(), ref, tl.Description())
		}
	}
	for _, name := range wantNames {
		if !have[name] {
			t.Errorf("canonical VCS tool %q was not built by sweep", name)
		}
	}
}
