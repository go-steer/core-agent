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

package mcp

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/go-steer/core-agent/v3/pkg/config"
)

type trustedAgentsDirKey struct{}
type untrustedAgentsDirKey struct{}

// WithTrustedAgentsDir marks agentsDir as trusted on ctx for Build
// (#1353), allowing stdio MCP servers declared in <agentsDir>/mcp.json
// to be spawned.
func WithTrustedAgentsDir(ctx context.Context, agentsDir string) context.Context {
	c := canonicalizeDir(agentsDir)
	if c == "" {
		return ctx
	}
	return context.WithValue(ctx, trustedAgentsDirKey{}, c)
}

// WithUntrustedAgentsDir marks agentsDir as untrusted on ctx for Build
// (#1353), causing Build to skip any stdio MCP servers declared in
// <agentsDir>/mcp.json before spawning child processes.
func WithUntrustedAgentsDir(ctx context.Context, agentsDir string) context.Context {
	c := canonicalizeDir(agentsDir)
	if c == "" {
		return ctx
	}
	return context.WithValue(ctx, untrustedAgentsDirKey{}, c)
}

func isAgentsDirTrusted(ctx context.Context, agentsDir string) bool {
	c := canonicalizeDir(agentsDir)
	if c == "" {
		return true
	}
	if ctx != nil {
		if untrusted, ok := ctx.Value(untrustedAgentsDirKey{}).(string); ok && untrusted == c {
			return false
		}
		if trusted, ok := ctx.Value(trustedAgentsDirKey{}).(string); ok && trusted == c {
			return true
		}
	}
	return config.IsWorkspaceTrusted("", agentsDir)
}

func canonicalizeDir(dir string) string {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		return filepath.Clean(resolved)
	}
	if abs, err := filepath.Abs(dir); err == nil {
		return filepath.Clean(abs)
	}
	return filepath.Clean(dir)
}

// loadAllWithTrust loads mcp.json from agentsDir and homeAgentsDir,
// stripping stdio servers from agentsDir when agentsDir is untrusted
// (#1353).
func loadAllWithTrust(ctx context.Context, agentsDir, homeAgentsDir string, send func(string)) (Servers, error) {
	if isAgentsDirTrusted(ctx, agentsDir) {
		return LoadAll(agentsDir, homeAgentsDir)
	}
	projectCfg, err := loadOne(agentsDir)
	if err != nil {
		return Servers{}, err
	}
	projectCfg.AgenticWrapLLM = nil
	projectCfg.AgenticWrapModel = ""
	if len(projectCfg.Servers) > 0 {
		var skipped []string
		for name, spec := range projectCfg.Servers {
			if spec.Transport == "stdio" {
				skipped = append(skipped, name)
				delete(projectCfg.Servers, name)
			}
		}
		sort.Strings(skipped)
		if send != nil {
			for _, name := range skipped {
				send(fmt.Sprintf("workspace trust: skipped untrusted stdio MCP server %q in %s (pass --trust to allow)", name, agentsDir))
			}
		}
	}
	homeCfg, err := loadOne(homeAgentsDir)
	if err != nil {
		return Servers{}, err
	}
	return mergeLoadedServers(projectCfg, homeCfg), nil
}

func mergeLoadedServers(list ...Servers) Servers {
	var merged Servers
	for _, s := range list {
		for name, spec := range s.Servers {
			if merged.Servers == nil {
				merged.Servers = make(map[string]ServerSpec)
			}
			if _, exists := merged.Servers[name]; !exists {
				merged.Servers[name] = spec
			}
		}
		if merged.Version == 0 {
			merged.Version = s.Version
		}
		if merged.AgenticWrap == nil && s.AgenticWrap != nil {
			merged.AgenticWrap = s.AgenticWrap
		}
		if merged.AgenticWrapThreshold == 0 && s.AgenticWrapThreshold > 0 {
			merged.AgenticWrapThreshold = s.AgenticWrapThreshold
		}
		if merged.AgenticWrapLLM == nil && s.AgenticWrapLLM != nil {
			merged.AgenticWrapLLM = s.AgenticWrapLLM
		}
		if merged.AgenticWrapModel == "" && s.AgenticWrapModel != "" {
			merged.AgenticWrapModel = s.AgenticWrapModel
		}
	}
	return merged
}
