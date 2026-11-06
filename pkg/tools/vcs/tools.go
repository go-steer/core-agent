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
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	corevcs "github.com/go-steer/core-vcs/vcs"
	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
)

// CapabilitiesArgs is the input to vcs_capabilities.
type CapabilitiesArgs struct {
	Repository string `json:"repository,omitempty" jsonschema:"configured repository identifier (e.g. owner/repo or host/owner/repo); omit when a single gitops or configured repository exists"`
}

// CapabilitiesResult is the output of vcs_capabilities.
type CapabilitiesResult struct {
	Forge        string               `json:"forge"`
	Repository   string               `json:"repository"`
	Role         string               `json:"role"`
	BaseBranch   string               `json:"base_branch"`
	BranchPrefix string               `json:"branch_prefix"`
	Capabilities corevcs.Capabilities `json:"capabilities"`
}

func (s *Suite) newCapabilitiesTool() (tool.Tool, error) {
	return newTool(
		"vcs_capabilities",
		"Report the forge provider, base branch, branch prefix, proposal noun, and supported operations for a configured repository.",
		func(ctx adkagent.Context, in CapabilitiesArgs) (CapabilitiesResult, error) {
			if err := s.gate.CheckCredentialMention("vcs_capabilities", in.Repository); err != nil {
				return CapabilitiesResult{}, err
			}
			repo, err := s.resolveRepo(in.Repository)
			if err != nil {
				return CapabilitiesResult{}, err
			}
			if err := s.gate.CheckPolicyDeny(ctx, "vcs_capabilities", repo.Ref.Path); err != nil {
				return CapabilitiesResult{}, err
			}
			caps, err := repo.Capabilities(ctx)
			if err != nil {
				return CapabilitiesResult{}, err
			}
			base := caps.BaseBranch
			if base == "" {
				base = repo.BaseBranch
			}
			return CapabilitiesResult{
				Forge:        repo.Forge.Name,
				Repository:   repo.Ref.Path,
				Role:         string(repo.Role),
				BaseBranch:   base,
				BranchPrefix: s.effectiveBranchPrefix(),
				Capabilities: caps,
			}, nil
		},
	)
}

// CloneArgs is the input to vcs_clone.
type CloneArgs struct {
	Repository string `json:"repository,omitempty" jsonschema:"configured repository identifier (e.g. owner/repo); omit when a single gitops or configured repository exists"`
	Branch     string `json:"branch,omitempty" jsonschema:"branch to clone; omit to clone the repository's configured base branch"`
}

// CloneResult is the output of vcs_clone.
type CloneResult struct {
	Forge      string `json:"forge"`
	Repository string `json:"repository"`
	Role       string `json:"role"`
	Path       string `json:"path"`
	Branch     string `json:"branch"`
	Revision   string `json:"revision"`
}

func (s *Suite) newCloneTool() (tool.Tool, error) {
	return newTool(
		"vcs_clone",
		"Clone a configured repository into the local workspace directory at its base branch (or a specified branch) and return the checkout path, branch, and HEAD revision.",
		func(ctx adkagent.Context, in CloneArgs) (CloneResult, error) {
			if err := s.gate.CheckCredentialMention("vcs_clone", in.Repository, in.Branch); err != nil {
				return CloneResult{}, err
			}
			repo, err := s.resolveRepo(in.Repository)
			if err != nil {
				return CloneResult{}, err
			}
			dest := s.repoDestDir(repo)
			if err := s.gate.CheckFileRead(ctx, "vcs_clone", dest); err != nil {
				return CloneResult{}, err
			}
			s.cloneMu.Lock()
			defer s.cloneMu.Unlock()
			if err := os.RemoveAll(dest); err != nil {
				return CloneResult{}, fmt.Errorf("vcs_clone: clean workspace %s: %w", dest, err)
			}
			ws, err := corevcs.Clone(ctx, repo, strings.TrimSpace(in.Branch), dest, s.policy)
			if err != nil {
				return CloneResult{}, err
			}
			s.storeWorkspace(repo, ws)
			return CloneResult{
				Forge:      repo.Forge.Name,
				Repository: repo.Ref.Path,
				Role:       string(repo.Role),
				Path:       ws.Dir(),
				Branch:     ws.ClonedBranch(),
				Revision:   ws.ClonedRevision(),
			}, nil
		},
	)
}

// StatusArgs is the input to vcs_status.
type StatusArgs struct {
	Repository string `json:"repository,omitempty" jsonschema:"configured repository identifier; omit when a single gitops or configured repository exists"`
}

// StatusResult is the output of vcs_status.
type StatusResult struct {
	Repository string         `json:"repository"`
	Path       string         `json:"path"`
	Branch     string         `json:"branch"`
	Revision   string         `json:"revision"`
	Status     corevcs.Status `json:"status"`
}

func (s *Suite) newStatusTool() (tool.Tool, error) {
	return newTool(
		"vcs_status",
		"Return the working tree status (clean state, modified, added, deleted, and untracked files), current branch, and HEAD revision for a cloned repository.",
		func(ctx adkagent.Context, in StatusArgs) (StatusResult, error) {
			if err := s.gate.CheckCredentialMention("vcs_status", in.Repository); err != nil {
				return StatusResult{}, err
			}
			repo, ws, err := s.lookupWorkspace(in.Repository)
			if err != nil {
				return StatusResult{}, err
			}
			if err := s.gate.CheckFileRead(ctx, "vcs_status", ws.Dir()); err != nil {
				return StatusResult{}, err
			}
			st, err := ws.Status()
			if err != nil {
				return StatusResult{}, err
			}
			return StatusResult{
				Repository: repo.Ref.Path,
				Path:       ws.Dir(),
				Branch:     st.Branch,
				Revision:   st.Revision,
				Status:     st,
			}, nil
		},
	)
}

// DiffArgs is the input to vcs_diff.
type DiffArgs struct {
	Repository  string   `json:"repository,omitempty" jsonschema:"configured repository identifier; omit when a single gitops or configured repository exists"`
	Paths       []string `json:"paths,omitempty" jsonschema:"optional workspace-relative file paths to restrict the diff to"`
	AgainstBase bool     `json:"against_base,omitempty" jsonschema:"diff against the cloned base revision instead of HEAD"`
}

// DiffResult is the output of vcs_diff.
type DiffResult struct {
	Repository string `json:"repository"`
	Diff       string `json:"diff"`
}

func (s *Suite) newDiffTool() (tool.Tool, error) {
	return newTool(
		"vcs_diff",
		"Return the unified diff between HEAD (or the cloned base revision) and the working tree for a cloned repository.",
		func(ctx adkagent.Context, in DiffArgs) (DiffResult, error) {
			candidates := append([]string{in.Repository}, in.Paths...)
			if err := s.gate.CheckCredentialMention("vcs_diff", candidates...); err != nil {
				return DiffResult{}, err
			}
			repo, ws, err := s.lookupWorkspace(in.Repository)
			if err != nil {
				return DiffResult{}, err
			}
			if err := s.gate.CheckFileRead(ctx, "vcs_diff", ws.Dir()); err != nil {
				return DiffResult{}, err
			}
			diff, err := ws.Diff(corevcs.DiffOptions{
				Paths:       in.Paths,
				AgainstBase: in.AgainstBase,
			})
			if err != nil {
				return DiffResult{}, err
			}
			return DiffResult{
				Repository: repo.Ref.Path,
				Diff:       diff,
			}, nil
		},
	)
}

// LogArgs is the input to vcs_log.
type LogArgs struct {
	Repository string `json:"repository,omitempty" jsonschema:"configured repository identifier; omit when a single gitops or configured repository exists"`
	Limit      int    `json:"limit,omitempty" jsonschema:"maximum number of commits to return from HEAD in reverse chronological order (default 20)"`
}

// LogResult is the output of vcs_log.
type LogResult struct {
	Repository string           `json:"repository"`
	Commits    []corevcs.Commit `json:"commits"`
}

func (s *Suite) newLogTool() (tool.Tool, error) {
	return newTool(
		"vcs_log",
		"Return the commit history starting from HEAD in reverse chronological order for a cloned repository.",
		func(ctx adkagent.Context, in LogArgs) (LogResult, error) {
			if err := s.gate.CheckCredentialMention("vcs_log", in.Repository); err != nil {
				return LogResult{}, err
			}
			repo, ws, err := s.lookupWorkspace(in.Repository)
			if err != nil {
				return LogResult{}, err
			}
			if err := s.gate.CheckFileRead(ctx, "vcs_log", ws.Dir()); err != nil {
				return LogResult{}, err
			}
			commits, err := ws.Log(in.Limit)
			if err != nil {
				return LogResult{}, err
			}
			return LogResult{
				Repository: repo.Ref.Path,
				Commits:    commits,
			}, nil
		},
	)
}

// CommitArgs is the input to vcs_commit.
type CommitArgs struct {
	Repository string `json:"repository,omitempty" jsonschema:"configured repository identifier; omit when a single gitops or configured repository exists"`
	Branch     string `json:"branch,omitempty" jsonschema:"optional topic branch name under the configured branch_prefix (e.g. agent/fix-foo) to create or switch to before committing"`
	Message    string `json:"message" jsonschema:"required; commit message (at least 10 characters) stating what changed and why"`
}

// CommitResult is the output of vcs_commit.
type CommitResult struct {
	Repository string         `json:"repository"`
	Branch     string         `json:"branch"`
	Commit     corevcs.Commit `json:"commit"`
}

func (s *Suite) newCommitTool() (tool.Tool, error) {
	return newTool(
		"vcs_commit",
		"Stage working tree modifications and create a local commit in the cloned repository using the forge's configured commit identity.",
		func(ctx adkagent.Context, in CommitArgs) (CommitResult, error) {
			if err := s.gate.CheckCredentialMention("vcs_commit", in.Repository, in.Branch, in.Message); err != nil {
				return CommitResult{}, err
			}
			repo, ws, err := s.lookupWorkspace(in.Repository)
			if err != nil {
				return CommitResult{}, err
			}
			detail := repo.Ref.Path
			if strings.TrimSpace(in.Branch) != "" {
				detail += ":" + strings.TrimSpace(in.Branch)
			}
			gateArgs := struct {
				CommitArgs
				Diff string `json:"diff,omitempty"`
			}{CommitArgs: in}
			if d, derr := ws.Diff(corevcs.DiffOptions{}); derr == nil && d != "" {
				gateArgs.Diff = truncateDiffForGate(d)
			}
			if err := s.gate.CheckGenericWithArgs(ctx, "vcs_commit", detail, gateArgs); err != nil {
				return CommitResult{}, err
			}
			if strings.TrimSpace(in.Branch) != "" {
				if err := ws.Branch(strings.TrimSpace(in.Branch)); err != nil {
					return CommitResult{}, err
				}
			}
			commit, err := ws.Commit(in.Message, s.commitIdentity(repo.Forge.Name))
			if err != nil {
				return CommitResult{}, err
			}
			var branch string
			if st, serr := ws.Status(); serr == nil {
				branch = st.Branch
			}
			return CommitResult{
				Repository: repo.Ref.Path,
				Branch:     branch,
				Commit:     commit,
			}, nil
		},
	)
}

// PublishArgs is the input to vcs_publish.
type PublishArgs struct {
	Repository string `json:"repository,omitempty" jsonschema:"configured repository identifier; omit when a single gitops or configured repository exists"`
	Branch     string `json:"branch" jsonschema:"required; topic branch name under the configured branch_prefix (e.g. agent/fix-foo) to push to the remote"`
}

// PublishResult is the output of vcs_publish.
type PublishResult struct {
	Forge      string                `json:"forge"`
	Repository string                `json:"repository"`
	Result     corevcs.PublishResult `json:"result"`
}

func (s *Suite) newPublishTool() (tool.Tool, error) {
	return newTool(
		"vcs_publish",
		"Push a local topic branch created in this session to the remote forge after verifying branch ownership, prefix policy, and remote base-branch ancestry.",
		func(ctx adkagent.Context, in PublishArgs) (PublishResult, error) {
			if err := s.gate.CheckCredentialMention("vcs_publish", in.Repository, in.Branch); err != nil {
				return PublishResult{}, err
			}
			repo, ws, err := s.lookupWorkspace(in.Repository)
			if err != nil {
				return PublishResult{}, err
			}
			branch := strings.TrimSpace(in.Branch)
			detail := repo.Ref.Path + ":" + branch
			gateArgs := struct {
				PublishArgs
				Diff string `json:"diff,omitempty"`
			}{PublishArgs: in}
			if d, derr := ws.Diff(corevcs.DiffOptions{AgainstBase: true}); derr == nil && d != "" {
				gateArgs.Diff = truncateDiffForGate(d)
			}
			if err := s.gate.CheckGenericWithArgs(ctx, "vcs_publish", detail, gateArgs); err != nil {
				return PublishResult{}, err
			}
			// When the caller committed on the cloned branch before naming the
			// topic branch at publish time, switch/create the topic branch now
			// so commits made since clone are carried onto it. Skip when branch
			// equals the cloned/base branch so ws.Publish returns TARGET_IS_BRANCH.
			if st, berr := ws.Status(); berr == nil && st.Branch == ws.ClonedBranch() && branch != "" && branch != ws.ClonedBranch() && branch != repo.BaseBranch {
				if err := ws.Branch(branch); err != nil {
					return PublishResult{}, err
				}
			}
			res, err := ws.Publish(ctx, branch)
			if err != nil {
				return PublishResult{}, err
			}
			s.emitEvent(ctx, Event{
				Kind:       EventKindPublished,
				Forge:      repo.Forge.Name,
				Repository: repo.Ref.Path,
				Branch:     res.Branch,
				Revision:   res.Revision,
			})
			return PublishResult{
				Forge:      repo.Forge.Name,
				Repository: repo.Ref.Path,
				Result:     res,
			}, nil
		},
	)
}

// ProposeArgs is the input to vcs_propose.
type ProposeArgs struct {
	Repository   string   `json:"repository,omitempty" jsonschema:"configured repository identifier; omit when a single gitops or configured repository exists"`
	Title        string   `json:"title" jsonschema:"required; proposal title stating the change"`
	Body         string   `json:"body" jsonschema:"required; proposal body containing the root-cause findings, supporting evidence, proposed change, and rollback steps"`
	SourceBranch string   `json:"source_branch" jsonschema:"required; published topic branch name carrying the commits"`
	TargetBranch string   `json:"target_branch,omitempty" jsonschema:"target base branch to merge into; omit to use the repository's configured base branch"`
	Draft        bool     `json:"draft,omitempty" jsonschema:"open the proposal as a draft when supported by the forge"`
	Labels       []string `json:"labels,omitempty" jsonschema:"optional label names to attach when supported by the forge"`
}

// ProposeResult is the output of vcs_propose.
type ProposeResult struct {
	Forge      string           `json:"forge"`
	Repository string           `json:"repository"`
	Proposal   corevcs.Proposal `json:"proposal"`
}

func (s *Suite) newProposeTool() (tool.Tool, error) {
	return newTool(
		"vcs_propose",
		"Open a change proposal (pull request or merge request) on the remote forge from a published topic branch into the target base branch.",
		func(ctx adkagent.Context, in ProposeArgs) (ProposeResult, error) {
			candidates := append([]string{in.Repository, in.Title, in.Body, in.SourceBranch, in.TargetBranch}, in.Labels...)
			if err := s.gate.CheckCredentialMention("vcs_propose", candidates...); err != nil {
				return ProposeResult{}, err
			}
			repo, err := s.resolveRepo(in.Repository)
			if err != nil {
				return ProposeResult{}, err
			}
			detail := repo.Ref.Path + ":" + strings.TrimSpace(in.SourceBranch)
			gateArgs := struct {
				ProposeArgs
				Diff string `json:"diff,omitempty"`
			}{ProposeArgs: in}
			if _, ws, werr := s.lookupWorkspace(in.Repository); werr == nil {
				if d, derr := ws.Diff(corevcs.DiffOptions{AgainstBase: true}); derr == nil && d != "" {
					gateArgs.Diff = truncateDiffForGate(d)
				}
			}
			if err := s.gate.CheckGenericWithArgs(ctx, "vcs_propose", detail, gateArgs); err != nil {
				return ProposeResult{}, err
			}
			prop, err := repo.CreateProposal(ctx, corevcs.ProposalRequest{
				Title:  in.Title,
				Body:   in.Body,
				Source: strings.TrimSpace(in.SourceBranch),
				Target: strings.TrimSpace(in.TargetBranch),
				Draft:  in.Draft,
				Labels: in.Labels,
			})
			if err != nil {
				return ProposeResult{}, err
			}
			s.emitEvent(ctx, Event{
				Kind:       EventKindProposed,
				Forge:      repo.Forge.Name,
				Repository: repo.Ref.Path,
				Branch:     prop.Source,
				Revision:   prop.Revision,
				Number:     prop.Number,
				URL:        prop.URL,
			})
			return ProposeResult{
				Forge:      repo.Forge.Name,
				Repository: repo.Ref.Path,
				Proposal:   prop,
			}, nil
		},
	)
}

// ProposalViewArgs is the input to vcs_proposal_view.
type ProposalViewArgs struct {
	Repository string `json:"repository,omitempty" jsonschema:"configured repository identifier; omit when a single gitops or configured repository exists"`
	Number     int    `json:"number" jsonschema:"required; proposal number to inspect"`
}

// ProposalViewResult is the output of vcs_proposal_view.
type ProposalViewResult struct {
	Forge      string            `json:"forge"`
	Repository string            `json:"repository"`
	Proposal   corevcs.Proposal  `json:"proposal"`
	Comments   []corevcs.Comment `json:"comments,omitempty"`
}

func (s *Suite) newProposalViewTool() (tool.Tool, error) {
	return newTool(
		"vcs_proposal_view",
		"Fetch a change proposal and its review comments by number from the remote forge.",
		func(ctx adkagent.Context, in ProposalViewArgs) (ProposalViewResult, error) {
			if err := s.gate.CheckCredentialMention("vcs_proposal_view", in.Repository); err != nil {
				return ProposalViewResult{}, err
			}
			repo, err := s.resolveRepo(in.Repository)
			if err != nil {
				return ProposalViewResult{}, err
			}
			if err := s.gate.CheckPolicyDeny(ctx, "vcs_proposal_view", repo.Ref.Path+"#"+strconv.Itoa(in.Number)); err != nil {
				return ProposalViewResult{}, err
			}
			prop, err := repo.GetProposal(ctx, in.Number)
			if err != nil {
				return ProposalViewResult{}, err
			}
			comments, err := repo.ProposalComments(ctx, in.Number, corevcs.Page{Limit: 100})
			if err != nil {
				var vcsErr *corevcs.Error
				if !errors.As(err, &vcsErr) || vcsErr.Code != corevcs.CodeForgeUnsupported {
					return ProposalViewResult{}, err
				}
			}
			return ProposalViewResult{
				Forge:      repo.Forge.Name,
				Repository: repo.Ref.Path,
				Proposal:   prop,
				Comments:   comments.Comments,
			}, nil
		},
	)
}

// ProposalListArgs is the input to vcs_proposal_list.
type ProposalListArgs struct {
	Repository   string   `json:"repository,omitempty" jsonschema:"configured repository identifier; omit when a single gitops or configured repository exists"`
	State        string   `json:"state,omitempty" jsonschema:"proposal state filter: open (default), closed, merged, or all"`
	SourceBranch string   `json:"source_branch,omitempty" jsonschema:"optional source branch filter"`
	TargetBranch string   `json:"target_branch,omitempty" jsonschema:"optional target branch filter"`
	Labels       []string `json:"labels,omitempty" jsonschema:"optional label filter"`
	Limit        int      `json:"limit,omitempty" jsonschema:"maximum number of proposals to return (default 20)"`
	Page         int      `json:"page,omitempty" jsonschema:"1-indexed page number (default 1)"`
}

// ProposalListResult is the output of vcs_proposal_list.
type ProposalListResult struct {
	Forge      string             `json:"forge"`
	Repository string             `json:"repository"`
	Proposals  []corevcs.Proposal `json:"proposals"`
	Truncated  bool               `json:"truncated,omitempty"`
}

func (s *Suite) newProposalListTool() (tool.Tool, error) {
	return newTool(
		"vcs_proposal_list",
		"List change proposals on the remote forge matching the supplied state, branch, or label filters.",
		func(ctx adkagent.Context, in ProposalListArgs) (ProposalListResult, error) {
			candidates := append([]string{in.Repository, in.State, in.SourceBranch, in.TargetBranch}, in.Labels...)
			if err := s.gate.CheckCredentialMention("vcs_proposal_list", candidates...); err != nil {
				return ProposalListResult{}, err
			}
			repo, err := s.resolveRepo(in.Repository)
			if err != nil {
				return ProposalListResult{}, err
			}
			if err := s.gate.CheckPolicyDeny(ctx, "vcs_proposal_list", repo.Ref.Path); err != nil {
				return ProposalListResult{}, err
			}
			list, err := repo.ListProposals(ctx, corevcs.ProposalFilter{
				State:  corevcs.ProposalState(strings.TrimSpace(in.State)),
				Source: strings.TrimSpace(in.SourceBranch),
				Target: strings.TrimSpace(in.TargetBranch),
				Labels: in.Labels,
				Page:   corevcs.Page{Limit: in.Limit, Page: in.Page},
			})
			if err != nil {
				return ProposalListResult{}, err
			}
			return ProposalListResult{
				Forge:      repo.Forge.Name,
				Repository: repo.Ref.Path,
				Proposals:  list.Proposals,
				Truncated:  list.Truncated,
			}, nil
		},
	)
}

// ProposalCommentArgs is the input to vcs_proposal_comment.
type ProposalCommentArgs struct {
	Repository string `json:"repository,omitempty" jsonschema:"configured repository identifier; omit when a single gitops or configured repository exists"`
	Number     int    `json:"number" jsonschema:"required; proposal number to comment on"`
	Body       string `json:"body" jsonschema:"required; comment text containing the response to review feedback or the verification details"`
}

// ProposalCommentResult is the output of vcs_proposal_comment.
type ProposalCommentResult struct {
	Forge      string          `json:"forge"`
	Repository string          `json:"repository"`
	Comment    corevcs.Comment `json:"comment"`
}

func (s *Suite) newProposalCommentTool() (tool.Tool, error) {
	return newTool(
		"vcs_proposal_comment",
		"Post a comment on an existing change proposal on the remote forge.",
		func(ctx adkagent.Context, in ProposalCommentArgs) (ProposalCommentResult, error) {
			if err := s.gate.CheckCredentialMention("vcs_proposal_comment", in.Repository, in.Body); err != nil {
				return ProposalCommentResult{}, err
			}
			repo, err := s.resolveRepo(in.Repository)
			if err != nil {
				return ProposalCommentResult{}, err
			}
			detail := fmt.Sprintf("%s#%d", repo.Ref.Path, in.Number)
			if err := s.gate.CheckGenericWithArgs(ctx, "vcs_proposal_comment", detail, in); err != nil {
				return ProposalCommentResult{}, err
			}
			comment, err := repo.CommentProposal(ctx, in.Number, in.Body)
			if err != nil {
				return ProposalCommentResult{}, err
			}
			return ProposalCommentResult{
				Forge:      repo.Forge.Name,
				Repository: repo.Ref.Path,
				Comment:    comment,
			}, nil
		},
	)
}
