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

package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/go-steer/core-agent/v3/pkg/config"
	"github.com/go-steer/core-agent/v3/pkg/mcp"
	"github.com/go-steer/core-agent/v3/pkg/models"
	"github.com/go-steer/core-agent/v3/pkg/models/anthropic"
	"github.com/go-steer/core-agent/v3/pkg/models/profiles"
	"github.com/go-steer/core-agent/v3/pkg/modeltier"
	"github.com/go-steer/core-agent/v3/pkg/taskclass"
)

// applyWorkspaceTrust gates repo-supplied privileged capabilities in
// agentsDir (#1353): stdio MCP servers, command hooks, permission modes
// above "ask", rooted subagents, and external content_roots.
//
// When the operator explicitly passed -c <path> on the CLI
// (agentsDirFrom == agentsDirFromCfg), that config file was named by
// the operator directly on argv and is treated as trusted (and saved
// to the trust store if --trust was also passed).
func applyWorkspaceTrust(ctx context.Context, cfg *config.Config, agentsDir, discoveredAgentsDir string, agentsDirFrom agentsDirSource, trustFlag, unattended bool, send func(string)) (context.Context, error) {
	if agentsDir == "" && discoveredAgentsDir == "" {
		return ctx, nil
	}
	if agentsDirFrom == agentsDirFromCfg && !trustFlag {
		return mcp.WithTrustedAgentsDir(ctx, agentsDir), nil
	}
	opts := config.WorkspaceTrustOptions{
		TrustFlag:   trustFlag,
		Interactive: !unattended,
	}
	if discoveredAgentsDir != "" && !sameDir(discoveredAgentsDir, agentsDir) {
		discTrusted, discClamped, err := config.ResolveWorkspaceTrust(cfg, discoveredAgentsDir, opts)
		if err != nil {
			return ctx, fmt.Errorf("workspace trust: %w", err)
		}
		if !discTrusted && len(discClamped) > 0 && send != nil {
			send(fmt.Sprintf("workspace trust: %s is untrusted (%s); pass --trust to allow", discoveredAgentsDir, strings.Join(discClamped, "; ")))
		}
	}
	if agentsDir == "" {
		return ctx, nil
	}
	trusted, clamped, err := config.ResolveWorkspaceTrust(cfg, agentsDir, opts)
	if err != nil {
		return ctx, fmt.Errorf("workspace trust: %w", err)
	}
	if trusted {
		return mcp.WithTrustedAgentsDir(ctx, agentsDir), nil
	}
	if len(clamped) > 0 && send != nil {
		send(fmt.Sprintf("workspace trust: %s is untrusted (%s); pass --trust to allow", agentsDir, strings.Join(clamped, "; ")))
	}
	return mcp.WithUntrustedAgentsDir(ctx, agentsDir), nil
}

// checkSmallTierParent applies the small-tier-parent guard (#121) and
// returns a non-nil error when safety.small_tier_parent is "refuse".
func checkSmallTierParent(cfg *config.Config, provider models.Provider, prompt string, yolo bool, smallTierParent string, send func(string)) error {
	if smallTierParent != "" {
		cfg.Safety.SmallTierParent = smallTierParent
	}
	stpMode := cfg.Safety.SmallTierParent
	if stpMode == "" {
		stpMode = config.SmallTierParentWarn
	}
	if prompt != "" || yolo || !modeltier.IsSmall(cfg.Model.Name) {
		return nil
	}
	suggested := taskclass.ModelForTier(provider.Name(), taskclass.TierFrontier)
	if suggested == "" && cfg.IsProfileProvider(cfg.Model.Provider) {
		for _, tier := range []string{taskclass.TierFrontier, taskclass.TierMid} {
			if id, err := profiles.DefaultModel(cfg, cfg.Model.Provider, tier); err == nil && id != cfg.Model.Name {
				suggested = id
				break
			}
		}
	}
	if suggested == "" {
		suggested = anthropic.DefaultModel
	}
	notice := fmt.Sprintf(
		"%s is a small-tier model. Small-tier models work well as subtask workers (--agentic-small-model) but loop and stall as the parent for long interactive sessions. Consider a frontier or mid-tier model for the parent — e.g. --model %s --agentic-small-model %s.",
		cfg.Model.Name, suggested, cfg.Model.Name,
	)
	switch stpMode {
	case config.SmallTierParentRefuse:
		fmt.Fprintf(os.Stderr, "core-agent: refuse-on-small-tier-parent: %s Pass --small-tier-parent=warn or --small-tier-parent=allow to proceed anyway.\n", notice)
		return fmt.Errorf("refuse-on-small-tier-parent")
	case config.SmallTierParentWarn:
		send("small-tier parent: " + notice + " Pass --small-tier-parent=allow to suppress this notice.")
	case config.SmallTierParentAllow:
		// no-op
	}
	return nil
}
