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

// Example: minimal multi-turn agent using the core-agent library
// directly (no CLI). Uses Gemini by default; export GOOGLE_API_KEY
// before running:
//
//	GOOGLE_API_KEY=... go run ./examples/basic
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/go-steer/core-agent/agent"
	"github.com/go-steer/core-agent/config"
	"github.com/go-steer/core-agent/models"
	_ "github.com/go-steer/core-agent/models/gemini"
)

func main() {
	cfg := config.DefaultConfig()
	cfg.Model.Provider = config.ProviderGemini

	provider, err := models.Resolve(cfg)
	if err != nil {
		log.Fatal(err)
	}
	ctx := context.Background()
	m, err := provider.Model(ctx, cfg.Model.Name)
	if err != nil {
		log.Fatal(err)
	}

	a, err := agent.New(m, agent.WithInstruction("You are a concise helper. Answer in one sentence."))
	if err != nil {
		log.Fatal(err)
	}

	for event, err := range a.Run(ctx, "What is the capital of France?") {
		if err != nil {
			log.Fatal(err)
		}
		if event.Content == nil {
			continue
		}
		for _, p := range event.Content.Parts {
			if p.Text != "" && event.Partial {
				fmt.Fprint(os.Stdout, p.Text)
			}
		}
	}
	fmt.Println()
}
