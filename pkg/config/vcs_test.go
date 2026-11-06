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

package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestValidateVCS(t *testing.T) {
	t.Parallel()

	validBase := func() *Config {
		cfg := DefaultConfig()
		cfg.VCS = VCSConfig{
			Forges: []VCSForgeConfig{
				{
					Name:      "gh",
					Provider:  VCSProviderGitHub,
					TokenFile: "/var/run/secrets/vcs/github-token",
					CommitIdentity: &VCSCommitIdentityConfig{
						Name:  "Platform Agent",
						Email: "agent@example.com",
					},
				},
			},
			Repositories: []VCSRepositoryConfig{
				{
					Forge:      "gh",
					Repository: "org/cluster-gitops",
					Role:       VCSRoleGitOps,
					BaseBranch: "main",
				},
				{
					Forge:      "gh",
					Repository: "org/platform-docs",
					Role:       VCSRoleContext,
				},
			},
			WorkspaceDir:  ".vcs",
			MaxCloneBytes: 64 << 20,
			BranchPrefix:  "agent/",
		}
		return cfg
	}

	if err := validBase().Validate(); err != nil {
		t.Fatalf("valid VCS config rejected: %v", err)
	}

	cases := []struct {
		name    string
		mutate  func(c *Config)
		wantErr string
	}{
		{
			name: "negative max_clone_bytes",
			mutate: func(c *Config) {
				c.VCS.MaxCloneBytes = -1
			},
			wantErr: "vcs.max_clone_bytes",
		},
		{
			name: "invalid branch_prefix",
			mutate: func(c *Config) {
				c.VCS.BranchPrefix = "agent/..bad"
			},
			wantErr: "vcs.branch_prefix",
		},
		{
			name: "forges without repositories",
			mutate: func(c *Config) {
				c.VCS.Repositories = nil
			},
			wantErr: "vcs.repositories must not be empty",
		},
		{
			name: "duplicate forge name",
			mutate: func(c *Config) {
				c.VCS.Forges = append(c.VCS.Forges, c.VCS.Forges[0])
			},
			wantErr: "duplicate forge name",
		},
		{
			name: "unknown provider",
			mutate: func(c *Config) {
				c.VCS.Forges[0].Provider = "bitbucket"
			},
			wantErr: "unknown provider",
		},
		{
			name: "git provider missing host",
			mutate: func(c *Config) {
				c.VCS.Forges[0].Provider = VCSProviderGit
				c.VCS.Forges[0].Host = ""
			},
			wantErr: "host is required for provider \"git\"",
		},
		{
			name: "both token_file and token_env",
			mutate: func(c *Config) {
				c.VCS.Forges[0].TokenEnv = "GH_TOKEN"
			},
			wantErr: "token_file and token_env are mutually exclusive",
		},
		{
			name: "github missing token",
			mutate: func(c *Config) {
				c.VCS.Forges[0].TokenFile = ""
			},
			wantErr: "token_file or token_env is required",
		},
		{
			name: "relative token_file",
			mutate: func(c *Config) {
				c.VCS.Forges[0].TokenFile = "secrets/token"
			},
			wantErr: "must be an absolute path",
		},
		{
			name: "ca_file on public github.com",
			mutate: func(c *Config) {
				c.VCS.Forges[0].CAFile = "/etc/ssl/certs/corp-ca.pem"
			},
			wantErr: "ca_file is not allowed for public github.com",
		},
		{
			name: "unknown forge reference",
			mutate: func(c *Config) {
				c.VCS.Repositories[0].Forge = "missing"
			},
			wantErr: "does not match any declared entry in vcs.forges",
		},
		{
			name: "two gitops repos",
			mutate: func(c *Config) {
				c.VCS.Repositories[1].Role = VCSRoleGitOps
			},
			wantErr: "at most one repository may have role \"gitops\"",
		},
		{
			name: "base_branch on context repo",
			mutate: func(c *Config) {
				c.VCS.Repositories[1].BaseBranch = "main"
			},
			wantErr: "base_branch is not allowed on role \"context\"",
		},
		{
			name: "unknown repository role",
			mutate: func(c *Config) {
				c.VCS.Repositories[0].Role = "admin"
			},
			wantErr: "unknown role",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := validBase()
			tc.mutate(cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate() error = %q, want substring %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestVCSCredentialFilesAndEnvRefs(t *testing.T) {
	t.Setenv("TEST_VCS_SECRET_DIR", "/var/run/vcs")

	cfg := DefaultConfig()
	cfg.VCS = VCSConfig{
		Forges: []VCSForgeConfig{
			{
				Name:      "gh",
				Provider:  VCSProviderGitHub,
				TokenFile: "${TEST_VCS_SECRET_DIR}/token",
			},
			{
				Name:     "gl",
				Provider: VCSProviderGitLab,
				Host:     "gitlab.example.com",
				TokenEnv: "GITLAB_VCS_TOKEN",
			},
		},
		Repositories: []VCSRepositoryConfig{
			{Forge: "gh", Repository: "org/repo", Role: VCSRoleGitOps},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	gotFiles := cfg.VCSCredentialFiles()
	wantFiles := []string{"${TEST_VCS_SECRET_DIR}/token", "/var/run/vcs/token"}
	if !reflect.DeepEqual(gotFiles, wantFiles) {
		t.Errorf("VCSCredentialFiles() = %v, want %v", gotFiles, wantFiles)
	}

	gotEnv := cfg.EnvRefs()
	if !reflect.DeepEqual(gotEnv, []string{"GITLAB_VCS_TOKEN"}) {
		t.Errorf("EnvRefs() = %v, want [GITLAB_VCS_TOKEN]", gotEnv)
	}
}

func TestVCSUnknownKeyIsFatal(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	agentsDir := filepath.Join(dir, AgentsDirName)
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	raw := `{
		"version": 1,
		"model": {"provider": "echo", "name": "echo"},
		"vcs": {
			"forges": [{"name": "gh", "provider": "github", "token_fil": "/tmp/tok"}],
			"repositories": [{"forge": "gh", "repository": "org/repo", "role": "gitops"}]
		}
	}`
	if err := os.WriteFile(filepath.Join(agentsDir, ConfigFileName), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(agentsDir)
	if err == nil || !strings.Contains(err.Error(), "vcs.forges[0].token_fil") {
		t.Fatalf("expected fatal unknown key error for vcs.forges[0].token_fil, got %v", err)
	}
}

func TestVCSConfig_MergeFrom(t *testing.T) {
	t.Parallel()
	base := VCSConfig{
		Forges:        []VCSForgeConfig{{Name: "gh", Provider: "github", TokenEnv: "GH_TOKEN"}},
		Repositories:  []VCSRepositoryConfig{{Forge: "gh", Repository: "org/repo", Role: VCSRoleGitOps}},
		WorkspaceDir:  ".vcs",
		MaxCloneBytes: 268435456,
		BranchPrefix:  "agent/",
	}
	base.MergeFrom(VCSConfig{
		WorkspaceDir:  "/tmp/override-vcs",
		MaxCloneBytes: 1024,
		BranchPrefix:  "bot/",
	})
	if len(base.Forges) != 1 || len(base.Repositories) != 1 {
		t.Fatalf("MergeFrom clobbered base Forges/Repositories: %+v", base)
	}
	if base.WorkspaceDir != "/tmp/override-vcs" || base.MaxCloneBytes != 1024 || base.BranchPrefix != "bot/" {
		t.Errorf("MergeFrom did not apply scalar overrides: %+v", base)
	}
}
