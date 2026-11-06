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

// Package vcs implements the built-in vcs_* tool suite for shell-free GitOps
// workflows over go-steer/core-vcs (#1490).
package vcs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/go-steer/core-vcs/providers"
	corevcs "github.com/go-steer/core-vcs/vcs"
	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"github.com/go-steer/core-agent/v3/pkg/config"
	"github.com/go-steer/core-agent/v3/pkg/permissions"
)

// EventKind identifies a typed VCS audit event emitted on outward-facing
// repository actions (#1490).
const (
	EventKindPublished = "vcs.published"
	EventKindProposed  = "vcs.proposed"
)

// Event is a structured audit event emitted by vcs_publish and vcs_propose.
type Event struct {
	Kind       string `json:"kind"`
	Forge      string `json:"forge"`
	Repository string `json:"repository"`
	Branch     string `json:"branch,omitempty"`
	Revision   string `json:"revision,omitempty"`
	Number     int    `json:"number,omitempty"`
	URL        string `json:"url,omitempty"`
	AppName    string `json:"app_name,omitempty"`
	UserID     string `json:"user_id,omitempty"`
	SessionID  string `json:"session_id,omitempty"`
}

// EventSink receives typed VCS events emitted by vcs_publish and vcs_propose.
type EventSink func(ctx context.Context, ev Event)

var allToolNames = []string{
	"vcs_capabilities",
	"vcs_clone",
	"vcs_status",
	"vcs_diff",
	"vcs_log",
	"vcs_commit",
	"vcs_publish",
	"vcs_propose",
	"vcs_proposal_view",
	"vcs_proposal_list",
	"vcs_proposal_comment",
}

// ToolNames returns a fresh copy of the canonical vcs_* tool names.
func ToolNames() []string {
	return slices.Clone(allToolNames)
}

// IsToolName reports whether name is one of the canonical vcs_* tool names.
func IsToolName(name string) bool {
	return slices.Contains(allToolNames, name)
}

// Option configures Build.
type Option func(*buildOpts)

type buildOpts struct {
	registry  *corevcs.Registry
	getenv    func(string) string
	eventSink EventSink
}

// WithRegistry overrides the provider registry used to resolve forges.
func WithRegistry(reg *corevcs.Registry) Option {
	return func(o *buildOpts) { o.registry = reg }
}

// WithGetenv overrides the environment lookup used for token_env credentials.
func WithGetenv(fn func(string) string) Option {
	return func(o *buildOpts) { o.getenv = fn }
}

// WithEventSink registers a callback invoked after a successful vcs_publish or
// vcs_propose call.
func WithEventSink(sink EventSink) Option {
	return func(o *buildOpts) { o.eventSink = sink }
}

// Suite holds the catalog and in-memory workspace state for a configured VCS
// tool suite.
type Suite struct {
	catalog      *corevcs.Catalog
	gate         *permissions.Gate
	workspaceDir string
	policy       corevcs.Policy
	identities   map[string]corevcs.Identity
	eventSink    EventSink
	mu           sync.Mutex
	cloneMu      sync.Mutex
	workspaces   map[string]*corevcs.Workspace
}

// Build constructs the vcs_* tools declared by cfg.VCS. When cfg is nil or
// cfg.VCS.Repositories is empty, Build returns (nil, nil) so deployments with
// no VCS configuration register no vcs_* tools.
func Build(cfg *config.Config, gate *permissions.Gate, cwd string, opts ...Option) ([]tool.Tool, error) {
	suite, tools, err := BuildSuite(cfg, gate, cwd, opts...)
	_ = suite
	return tools, err
}

// BuildSuite constructs the Suite and the enabled vcs_* tools.
func BuildSuite(cfg *config.Config, gate *permissions.Gate, cwd string, opts ...Option) (*Suite, []tool.Tool, error) {
	if cfg == nil || !cfg.VCS.Enabled() {
		return nil, nil, nil
	}
	if gate == nil {
		return nil, nil, errors.New("vcs: gate is required")
	}
	if err := cfg.Validate(); err != nil {
		return nil, nil, err
	}

	bo := buildOpts{
		getenv: os.Getenv,
	}
	for _, o := range opts {
		if o != nil {
			o(&bo)
		}
	}
	if bo.registry == nil {
		bo.registry = providers.NewRegistry()
	}
	if bo.getenv == nil {
		bo.getenv = os.Getenv
	}

	forges, identities := buildForges(cfg.VCS.Forges, bo.getenv)
	repos := buildRepoConfigs(cfg.VCS.Repositories)
	cat, err := corevcs.NewCatalog(bo.registry, forges, repos)
	if err != nil {
		return nil, nil, fmt.Errorf("vcs: catalog: %w", err)
	}

	suite := &Suite{
		catalog:      cat,
		gate:         gate,
		workspaceDir: cfg.VCS.EffectiveWorkspaceDir(cwd),
		policy: corevcs.Policy{
			MaxCloneBytes: cfg.VCS.MaxCloneBytes,
			BranchPrefix:  cfg.VCS.BranchPrefix,
		},
		identities: identities,
		eventSink:  bo.eventSink,
		workspaces: make(map[string]*corevcs.Workspace),
	}

	built, err := suite.buildTools(cfg.Tools.Disable)
	if err != nil {
		return nil, nil, err
	}
	return suite, built, nil
}

func buildForges(specs []config.VCSForgeConfig, getenv func(string) string) ([]corevcs.Forge, map[string]corevcs.Identity) {
	out := make([]corevcs.Forge, 0, len(specs))
	identities := make(map[string]corevcs.Identity, len(specs))
	for _, f := range specs {
		var cred corevcs.Credential
		switch {
		case strings.TrimSpace(f.TokenFile) != "":
			cred = corevcs.TokenFile(strings.TrimSpace(f.TokenFile))
		case strings.TrimSpace(f.TokenEnv) != "":
			envName := strings.TrimSpace(f.TokenEnv)
			cred = corevcs.CredentialFunc(func(context.Context) (string, error) {
				return strings.TrimSpace(getenv(envName)), nil
			})
		}
		var id corevcs.Identity
		if f.CommitIdentity != nil {
			id = corevcs.Identity{
				Name:  strings.TrimSpace(f.CommitIdentity.Name),
				Email: strings.TrimSpace(f.CommitIdentity.Email),
			}
			identities[strings.TrimSpace(f.Name)] = id
		}
		out = append(out, corevcs.Forge{
			Name:                strings.TrimSpace(f.Name),
			Provider:            strings.TrimSpace(f.Provider),
			Host:                strings.TrimSpace(f.Host),
			Scheme:              strings.TrimSpace(f.Scheme),
			Port:                f.Port,
			Cred:                cred,
			Username:            strings.TrimSpace(f.Username),
			CAFile:              strings.TrimSpace(f.CAFile),
			CommitIdentity:      id,
			ProposalURLTemplate: strings.TrimSpace(f.ProposalURLTemplate),
		})
	}
	return out, identities
}

func buildRepoConfigs(specs []config.VCSRepositoryConfig) []corevcs.RepoConfig {
	out := make([]corevcs.RepoConfig, 0, len(specs))
	for _, r := range specs {
		out = append(out, corevcs.RepoConfig{
			Forge:      strings.TrimSpace(r.Forge),
			Repository: strings.TrimSpace(r.Repository),
			Role:       corevcs.Role(strings.TrimSpace(r.Role)),
			BaseBranch: strings.TrimSpace(r.BaseBranch),
		})
	}
	return out
}

func (s *Suite) resolveRepo(value string) (corevcs.Repo, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		if repo, ok := s.catalog.GitOps(); ok {
			return repo, nil
		}
		if repos := s.catalog.Repos(); len(repos) == 1 {
			return repos[0], nil
		}
	}
	return s.catalog.Resolve(trimmed)
}

func (s *Suite) repoDestDir(repo corevcs.Repo) string {
	host := repo.Ref.Host
	if host == "" {
		host = repo.Forge.Name
	}
	segments := []string{s.workspaceDir, sanitizeSegment(host)}
	for _, part := range strings.Split(repo.Ref.Path, "/") {
		if part != "" {
			segments = append(segments, sanitizeSegment(part))
		}
	}
	return filepath.Join(segments...)
}

func sanitizeSegment(seg string) string {
	var b strings.Builder
	for _, r := range seg {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	res := strings.Trim(b.String(), ".")
	if res == "" {
		return "_"
	}
	return res
}

func (s *Suite) storeWorkspace(repo corevcs.Repo, ws *corevcs.Workspace) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := repo.Ref.String()
	s.workspaces[key] = ws
}

func (s *Suite) lookupWorkspace(repoValue string) (corevcs.Repo, *corevcs.Workspace, error) {
	repo, err := s.resolveRepo(repoValue)
	if err != nil {
		return corevcs.Repo{}, nil, err
	}
	s.mu.Lock()
	ws := s.workspaces[repo.Ref.String()]
	s.mu.Unlock()
	if ws == nil {
		return corevcs.Repo{}, nil, fmt.Errorf("vcs: repository %q has not been cloned in this session; call vcs_clone first", repo.Ref.Path)
	}
	return repo, ws, nil
}

func (s *Suite) commitIdentity(forgeName string) corevcs.Identity {
	if id, ok := s.identities[forgeName]; ok && (id.Name != "" || id.Email != "") {
		if id.Name == "" {
			id.Name = "core-agent[bot]"
		}
		if id.Email == "" {
			id.Email = "core-agent@localhost"
		}
		return id
	}
	return corevcs.Identity{
		Name:  "core-agent[bot]",
		Email: "core-agent@localhost",
	}
}

func (s *Suite) effectiveBranchPrefix() string {
	if p := strings.TrimSpace(s.policy.BranchPrefix); p != "" {
		return p
	}
	return corevcs.DefaultBranchPrefix
}

func (s *Suite) emitEvent(ctx adkagent.Context, ev Event) {
	if s.eventSink == nil {
		return
	}
	if ctx != nil {
		if ev.AppName == "" {
			ev.AppName = ctx.AppName()
		}
		if ev.UserID == "" {
			ev.UserID = ctx.UserID()
		}
		if ev.SessionID == "" {
			ev.SessionID = ctx.SessionID()
		}
	}
	s.eventSink(ctx, ev)
}

func (s *Suite) buildTools(disabled []string) ([]tool.Tool, error) {
	disableSet := make(map[string]bool, len(disabled))
	for _, name := range disabled {
		disableSet[strings.TrimSpace(name)] = true
	}

	type spec struct {
		name string
		ctor func() (tool.Tool, error)
	}
	specs := []spec{
		{"vcs_capabilities", s.newCapabilitiesTool},
		{"vcs_clone", s.newCloneTool},
		{"vcs_status", s.newStatusTool},
		{"vcs_diff", s.newDiffTool},
		{"vcs_log", s.newLogTool},
		{"vcs_commit", s.newCommitTool},
		{"vcs_publish", s.newPublishTool},
		{"vcs_propose", s.newProposeTool},
		{"vcs_proposal_view", s.newProposalViewTool},
		{"vcs_proposal_list", s.newProposalListTool},
		{"vcs_proposal_comment", s.newProposalCommentTool},
	}

	out := make([]tool.Tool, 0, len(specs))
	registered := make([]string, 0, len(specs))
	for _, sp := range specs {
		if disableSet[sp.name] {
			continue
		}
		tl, err := sp.ctor()
		if err != nil {
			return nil, fmt.Errorf("vcs: build %s: %w", sp.name, err)
		}
		out = append(out, tl)
		registered = append(registered, sp.name)
	}
	if len(registered) > 0 {
		s.gate.RegisterPlanGatedTools(registered...)
	}
	return out, nil
}

// newTool is a tiny helper around functiontool.New.
func newTool[In, Out any](name, desc string, fn functiontool.Func[In, Out]) (tool.Tool, error) {
	return functiontool.New(functiontool.Config{
		Name:        name,
		Description: desc,
	}, fn)
}

const maxGateDiffBytes = 4096

func truncateDiffForGate(diff string) string {
	if len(diff) <= maxGateDiffBytes {
		return diff
	}
	return diff[:maxGateDiffBytes] + "\n... [diff truncated]"
}
