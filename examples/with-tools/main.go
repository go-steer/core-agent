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

// Example: an agent with one custom tool, MCP servers loaded from
// .agents/mcp.json (if present), and SKILL.md skills from
// .agents/skills/ (if any).
//
//	ANTHROPIC_API_KEY=... go run ./examples/with-tools
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"

	adktool "google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"

	"github.com/go-steer/core-agent/v2/pkg/agent"
	"github.com/go-steer/core-agent/v2/pkg/config"
	"github.com/go-steer/core-agent/v2/pkg/mcp"
	"github.com/go-steer/core-agent/v2/pkg/models"
	_ "github.com/go-steer/core-agent/v2/pkg/models/anthropic"
	"github.com/go-steer/core-agent/v2/pkg/permissions"
	"github.com/go-steer/core-agent/v2/pkg/skills"
)

type addArgs struct {
	A int `json:"a" jsonschema_description:"first number"`
	B int `json:"b" jsonschema_description:"second number"`
}

type addResult struct {
	Sum int `json:"sum"`
}

// addTool is a trivial custom tool the agent can invoke.
func addTool() adktool.Tool {
	t, err := functiontool.New(
		functiontool.Config{
			Name:        "add",
			Description: "Add two integers and return the sum.",
		},
		func(_ adktool.Context, in addArgs) (addResult, error) {
			return addResult{Sum: in.A + in.B}, nil
		},
	)
	if err != nil {
		panic(err)
	}
	return t
}

func main() {
	cfg := config.DefaultConfig()
	cfg.Model.Provider = config.ProviderAnthropic
	cfg.Model.Name = "claude-opus-4-7"
	cfg.Permissions.Mode = config.PermissionModeYolo // skip prompts in the example

	provider, err := models.Resolve(cfg)
	if err != nil {
		log.Fatal(err)
	}
	ctx := context.Background()
	m, err := provider.Model(ctx, cfg.Model.Name)
	if err != nil {
		log.Fatal(err)
	}

	cwd, _ := os.Getwd()
	gate, _ := permissions.FromConfig(cfg, cwd, "", nil)

	send := func(s string) { fmt.Fprintln(os.Stderr, "core-agent: "+s) }
	// nil DigestOptions: this example doesn't wire pkg/digest. Real
	// callers pass a *mcp.DigestOptions to enable the structural wrap
	// per docs/digest-design.md.
	_, mcpToolsets, err := mcp.Build(ctx, filepath.Join(cwd, ".agents"), "", send, gate, nil, nil)
	if err != nil {
		log.Printf("mcp: %v", err)
	}
	loadedSkills, err := skills.Load(ctx, filepath.Join(cwd, ".agents"), gate)
	if err != nil {
		log.Printf("skills: %v", err)
	}
	allToolsets := append([]adktool.Toolset{}, mcpToolsets...)
	if !loadedSkills.Empty() {
		allToolsets = append(allToolsets, loadedSkills.Toolset)
	}

	a, err := agent.New(m,
		agent.WithInstruction("You are a calculator. Use the add tool when the user asks for arithmetic."),
		agent.WithTools([]adktool.Tool{addTool()}),
		agent.WithToolsets(allToolsets),
	)
	if err != nil {
		log.Fatal(err)
	}

	for event, err := range a.Run(ctx, "What is 17 + 25?") {
		if err != nil {
			log.Fatal(err)
		}
		if event.Content == nil {
			continue
		}
		for _, p := range event.Content.Parts {
			switch {
			case p.FunctionCall != nil:
				fmt.Fprintf(os.Stderr, "→ %s\n", p.FunctionCall.Name)
			case p.Text != "" && event.Partial:
				fmt.Fprint(os.Stdout, p.Text)
			}
		}
	}
	fmt.Println()
}
