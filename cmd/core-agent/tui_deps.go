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
	adkmodel "google.golang.org/adk/model"

	"github.com/go-steer/core-agent/pkg/agent"
	"github.com/go-steer/core-agent/pkg/config"
	"github.com/go-steer/core-agent/pkg/instruction"
	"github.com/go-steer/core-agent/pkg/mcp"
	"github.com/go-steer/core-agent/pkg/models"
	"github.com/go-steer/core-agent/pkg/permissions"
	"github.com/go-steer/core-agent/pkg/skills"
	"github.com/go-steer/core-agent/pkg/usage"
)

// tuiDeps bundles everything launchTUI needs from main.go's run().
// Lives in this file (no build tag) so both tui_enabled.go and
// tui_disabled.go can refer to the same shape.
//
// Fields mirror what main.go has assembled by the time it reaches
// the TUI-launch branch; the struct exists purely to keep
// launchTUI's signature short.
type tuiDeps struct {
	Cfg          *config.Config
	Model        adkmodel.LLM
	AgentOpts    []agent.Option
	Provider     models.Provider
	Gate         *permissions.Gate
	Tracker      *usage.Tracker
	Memory       instruction.Loaded
	MCPServers   []*mcp.Server
	LoadedSkills skills.Skills
	AgentsDir    string
	CoreHome     string
}
