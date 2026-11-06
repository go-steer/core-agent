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
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// VCS provider and repository role constants (#1490).
const (
	VCSProviderGitHub = "github"
	VCSProviderGitLab = "gitlab"
	VCSProviderGit    = "git"

	VCSRoleGitOps  = "gitops"
	VCSRoleManaged = "managed"
	VCSRoleContext = "context"
)

// VCSConfig configures the native VCS tool suite (`vcs_*`) backed by
// `github.com/go-steer/core-vcs` (#1490, `docs/gitops-vcs-design.md`).
//
// An empty VCSConfig (no forges and no repositories) leaves the `vcs_*` tools
// unregistered so non-VCS deployments see an unchanged tool catalog.
type VCSConfig struct {
	// Forges declares named forge endpoints and their credentials.
	Forges []VCSForgeConfig `json:"forges,omitempty" yaml:"forges,omitempty"`

	// Repositories lists the repositories the agent may clone or query, along
	// with each repository's role ("gitops" | "managed" | "context") and
	// optional base branch.
	Repositories []VCSRepositoryConfig `json:"repositories,omitempty" yaml:"repositories,omitempty"`

	// WorkspaceDir is the directory under which `vcs_clone` places working
	// copies (`<workspace_dir>/<host>/<owner>/<repo>`). Defaults to `<cwd>/.vcs`.
	WorkspaceDir string `json:"workspace_dir,omitempty" yaml:"workspace_dir,omitempty"`

	// MaxCloneBytes caps the size of a single cloned working copy (default 256 MiB).
	MaxCloneBytes int64 `json:"max_clone_bytes,omitempty" yaml:"max_clone_bytes,omitempty"`

	// BranchPrefix is the required prefix for branches the agent creates and
	// publishes (default "agent/").
	BranchPrefix string `json:"branch_prefix,omitempty" yaml:"branch_prefix,omitempty"`
}

// Enabled reports whether the VCS block declares any forges or repositories.
func (v VCSConfig) Enabled() bool {
	return len(v.Forges) > 0 || len(v.Repositories) > 0
}

// EffectiveWorkspaceDir resolves the directory under which vcs_clone places
// working copies, defaulting to `<cwd>/.vcs` when WorkspaceDir is unset.
func (v VCSConfig) EffectiveWorkspaceDir(cwd string) string {
	wsDir := strings.TrimSpace(v.WorkspaceDir)
	if wsDir == "" {
		wsDir = ".vcs"
	}
	if !filepath.IsAbs(wsDir) && cwd != "" {
		wsDir = filepath.Join(cwd, wsDir)
	}
	if abs, err := filepath.Abs(wsDir); err == nil {
		return abs
	}
	return filepath.Clean(wsDir)
}

// VCSForgeConfig declares a single named forge in `vcs.forges[]`.
type VCSForgeConfig struct {
	// Name is the unique identifier referenced by `vcs.repositories[].forge`.
	Name string `json:"name" yaml:"name"`

	// Provider selects the forge adapter: "github", "gitlab", or "git".
	Provider string `json:"provider" yaml:"provider"`

	// Host is the forge hostname (e.g. "gitlab.corp.example.com"). Defaults to
	// "github.com" for provider "github" and "gitlab.com" for provider "gitlab".
	// Required for provider "git".
	Host string `json:"host,omitempty" yaml:"host,omitempty"`

	// Scheme selects "https" (default) or "http".
	Scheme string `json:"scheme,omitempty" yaml:"scheme,omitempty"`

	// Port is an optional non-default TCP port in [1, 65535].
	Port int `json:"port,omitempty" yaml:"port,omitempty"`

	// CAFile is an optional PEM bundle for self-signed / private CAs. Refused on
	// provider "github" when Host is empty (public github.com).
	CAFile string `json:"ca_file,omitempty" yaml:"ca_file,omitempty"`

	// Username is the HTTP Basic auth username for git smart-HTTP push/fetch when
	// the remote requires a specific username (defaults per provider).
	Username string `json:"username,omitempty" yaml:"username,omitempty"`

	// ProposalURLTemplate is an optional URL template for provider "git" (which
	// has no native proposal API) returned in `vcs_propose.proposal.url` (valid
	// placeholders: {host}, {path}, {source}, {target}).
	ProposalURLTemplate string `json:"proposal_url_template,omitempty" yaml:"proposal_url_template,omitempty"`

	// TokenFile is an absolute path to a file holding the forge bearer/PAT token,
	// re-read on every operation so Kubernetes ProjectedVolume / ExternalSecret
	// rotations take effect without restarting the daemon. Mutually exclusive
	// with TokenEnv. Added to the permission gate's credential-file refusal list
	// and withheld from `@include` expansion.
	TokenFile string `json:"token_file,omitempty" yaml:"token_file,omitempty"`

	// TokenEnv names an environment variable holding the forge token. Mutually
	// exclusive with TokenFile. Automatically discovered by Config.EnvRefs() and
	// withheld from child processes by withholdDaemonCredentials.
	TokenEnv string `json:"token_env,omitempty" yaml:"token_env,omitempty"`

	// CommitIdentity overrides the default git commit author ("core-agent
	// <core-agent@invalid>") for commits on this forge.
	CommitIdentity *VCSCommitIdentityConfig `json:"commit_identity,omitempty" yaml:"commit_identity,omitempty"`
}

// VCSCommitIdentityConfig configures the git author Name and Email for commits.
type VCSCommitIdentityConfig struct {
	Name  string `json:"name" yaml:"name"`
	Email string `json:"email" yaml:"email"`
}

// VCSRepositoryConfig declares a single repository in `vcs.repositories[]`.
type VCSRepositoryConfig struct {
	// Forge references a declared `vcs.forges[].name`.
	Forge string `json:"forge" yaml:"forge"`

	// Repository is the forge-relative path (e.g. "org/cluster-gitops" or
	// "group/sub/repo").
	Repository string `json:"repository" yaml:"repository"`

	// Role governs which VCS verbs are permitted on the repository:
	//   - "gitops":  read + branch/commit/publish + proposals (at most one per config; default when `repository` arg is omitted)
	//   - "managed": read + branch/commit/publish + proposals
	//   - "context": read-only (clone, status, diff, log, proposal_view, proposal_list)
	Role string `json:"role" yaml:"role"`

	// BaseBranch is the branch cloned and targeted by proposals when the caller
	// does not specify one (default "main"). Refused on role "context".
	BaseBranch string `json:"base_branch,omitempty" yaml:"base_branch,omitempty"`
}

// VCSCredentialFiles returns the sorted, deduplicated list of forge token_file
// paths (both raw and environment-expanded when they differ) declared under
// `vcs.forges[]`.
//
// Kept separate from CredentialFiles() because CredentialFiles() is scoped by
// its doc comment and tests to caller-to-daemon authentication files, while
// both lists are registered on the permission gate and withheld from
// `@include` expansion (`docs/gitops-vcs-design.md` decision 7).
func (c *Config) VCSCredentialFiles() []string {
	if c == nil || len(c.VCS.Forges) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(c.VCS.Forges)*2)
	add := func(raw string) {
		for _, p := range []string{strings.TrimSpace(raw), strings.TrimSpace(expandKeepUnset(raw))} {
			if p == "" {
				continue
			}
			seen[p] = struct{}{}
		}
	}
	for _, f := range c.VCS.Forges {
		add(f.TokenFile)
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

func (c *Config) validateVCS() error {
	v := c.VCS
	if v.MaxCloneBytes < 0 {
		return fmt.Errorf("config: vcs.max_clone_bytes=%d must be >= 0", v.MaxCloneBytes)
	}
	if v.BranchPrefix != "" {
		if err := validateVCSBranchPrefix(v.BranchPrefix); err != nil {
			return fmt.Errorf("config: vcs.branch_prefix: %w", err)
		}
	}
	if v.WorkspaceDir != "" && strings.TrimSpace(v.WorkspaceDir) == "" {
		return fmt.Errorf("config: vcs.workspace_dir is whitespace-only")
	}

	if !v.Enabled() {
		return nil
	}
	if len(v.Repositories) == 0 {
		return fmt.Errorf("config: vcs.repositories must not be empty when vcs.forges is configured")
	}

	forges := make(map[string]VCSForgeConfig, len(v.Forges))
	for i, f := range v.Forges {
		if err := validateVCSForge(i, f, forges); err != nil {
			return err
		}
		forges[f.Name] = f
	}

	gitopsCount := 0
	seenRepo := make(map[string]struct{}, len(v.Repositories))
	for i, r := range v.Repositories {
		if err := validateVCSRepository(i, r, forges, seenRepo, &gitopsCount); err != nil {
			return err
		}
	}
	return nil
}

func validateVCSForge(i int, f VCSForgeConfig, seen map[string]VCSForgeConfig) error {
	name := strings.TrimSpace(f.Name)
	if name == "" {
		return fmt.Errorf("config: vcs.forges[%d].name is required", i)
	}
	if _, dup := seen[name]; dup {
		return fmt.Errorf("config: vcs.forges[%d]: duplicate forge name %q", i, name)
	}
	switch f.Provider {
	case VCSProviderGitHub, VCSProviderGitLab, VCSProviderGit:
	default:
		return fmt.Errorf("config: vcs.forges[%d] (%s): unknown provider %q (want %q, %q, or %q)",
			i, name, f.Provider, VCSProviderGitHub, VCSProviderGitLab, VCSProviderGit)
	}
	if f.Provider == VCSProviderGit && strings.TrimSpace(f.Host) == "" {
		return fmt.Errorf("config: vcs.forges[%d] (%s): host is required for provider %q", i, name, VCSProviderGit)
	}
	if strings.Contains(f.Host, "/") || strings.Contains(f.Host, ":") {
		return fmt.Errorf("config: vcs.forges[%d] (%s): host %q must be a bare hostname without scheme, port, or path", i, name, f.Host)
	}
	switch f.Scheme {
	case "", "https", "http":
	default:
		return fmt.Errorf("config: vcs.forges[%d] (%s): scheme %q must be \"https\" or \"http\"", i, name, f.Scheme)
	}
	if f.Port < 0 || f.Port > 65535 {
		return fmt.Errorf("config: vcs.forges[%d] (%s): port %d out of range [0, 65535]", i, name, f.Port)
	}
	if f.CAFile != "" && f.Provider == VCSProviderGitHub && strings.TrimSpace(f.Host) == "" {
		return fmt.Errorf("config: vcs.forges[%d] (%s): ca_file is not allowed for public github.com (set host for GitHub Enterprise Server)", i, name)
	}
	return validateVCSForgeAuthAndIdentity(i, name, f)
}

func validateVCSForgeAuthAndIdentity(i int, name string, f VCSForgeConfig) error {
	tf := strings.TrimSpace(f.TokenFile)
	te := strings.TrimSpace(f.TokenEnv)
	if tf != "" && te != "" {
		return fmt.Errorf("config: vcs.forges[%d] (%s): token_file and token_env are mutually exclusive", i, name)
	}
	if tf == "" && te == "" && (f.Provider == VCSProviderGitHub || f.Provider == VCSProviderGitLab) {
		return fmt.Errorf("config: vcs.forges[%d] (%s): token_file or token_env is required for provider %q", i, name, f.Provider)
	}
	if tf != "" {
		expanded := expandKeepUnset(tf)
		if !filepath.IsAbs(expanded) && !strings.HasPrefix(tf, "${") {
			return fmt.Errorf("config: vcs.forges[%d] (%s): token_file %q must be an absolute path", i, name, f.TokenFile)
		}
	}
	if te != "" && !validEnvVarName(te) {
		return fmt.Errorf("config: vcs.forges[%d] (%s): token_env %q must be a valid environment variable name", i, name, f.TokenEnv)
	}
	if f.CommitIdentity != nil {
		if strings.TrimSpace(f.CommitIdentity.Name) == "" || strings.TrimSpace(f.CommitIdentity.Email) == "" {
			return fmt.Errorf("config: vcs.forges[%d] (%s): commit_identity requires both name and email", i, name)
		}
	}
	return nil
}

func validateVCSRepository(
	i int,
	r VCSRepositoryConfig,
	forges map[string]VCSForgeConfig,
	seenRepo map[string]struct{},
	gitopsCount *int,
) error {
	forgeName := strings.TrimSpace(r.Forge)
	if forgeName == "" {
		return fmt.Errorf("config: vcs.repositories[%d].forge is required", i)
	}
	if _, ok := forges[forgeName]; !ok {
		return fmt.Errorf("config: vcs.repositories[%d].forge %q does not match any declared entry in vcs.forges", i, r.Forge)
	}
	repoPath := strings.Trim(strings.TrimSpace(r.Repository), "/")
	if repoPath == "" || !strings.Contains(repoPath, "/") || strings.Contains(repoPath, "..") {
		return fmt.Errorf("config: vcs.repositories[%d].repository %q must be a non-empty \"owner/repo\" path", i, r.Repository)
	}
	key := forgeName + ":" + repoPath
	if _, dup := seenRepo[key]; dup {
		return fmt.Errorf("config: vcs.repositories[%d]: duplicate repository %q on forge %q", i, repoPath, forgeName)
	}
	seenRepo[key] = struct{}{}

	switch r.Role {
	case VCSRoleGitOps:
		*gitopsCount++
		if *gitopsCount > 1 {
			return fmt.Errorf("config: vcs.repositories[%d]: at most one repository may have role %q", i, VCSRoleGitOps)
		}
	case VCSRoleManaged:
	case VCSRoleContext:
		if strings.TrimSpace(r.BaseBranch) != "" {
			return fmt.Errorf("config: vcs.repositories[%d] (%s): base_branch is not allowed on role %q", i, repoPath, VCSRoleContext)
		}
	default:
		return fmt.Errorf("config: vcs.repositories[%d] (%s): unknown role %q (want %q, %q, or %q)",
			i, repoPath, r.Role, VCSRoleGitOps, VCSRoleManaged, VCSRoleContext)
	}
	return nil
}

func validateVCSBranchPrefix(prefix string) error {
	if strings.TrimSpace(prefix) != prefix || prefix == "" {
		return fmt.Errorf("branch_prefix %q must not have surrounding whitespace", prefix)
	}
	if strings.HasPrefix(prefix, "/") || strings.HasPrefix(prefix, "-") || strings.Contains(prefix, "..") || strings.ContainsAny(prefix, " \t\r\n~^:?*[\\") {
		return fmt.Errorf("branch_prefix %q is not a valid git ref prefix", prefix)
	}
	return nil
}

func validEnvVarName(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'A' && r <= 'Z':
		case r >= 'a' && r <= 'z':
		case r == '_':
		case i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}

// MergeFrom overlays non-zero fields of override onto c (#1490).
func (c *VCSConfig) MergeFrom(override VCSConfig) {
	if c == nil {
		return
	}
	if len(override.Forges) > 0 {
		c.Forges = append([]VCSForgeConfig(nil), override.Forges...)
	}
	if len(override.Repositories) > 0 {
		c.Repositories = append([]VCSRepositoryConfig(nil), override.Repositories...)
	}
	if override.WorkspaceDir != "" {
		c.WorkspaceDir = override.WorkspaceDir
	}
	if override.MaxCloneBytes > 0 {
		c.MaxCloneBytes = override.MaxCloneBytes
	}
	if override.BranchPrefix != "" {
		c.BranchPrefix = override.BranchPrefix
	}
}
