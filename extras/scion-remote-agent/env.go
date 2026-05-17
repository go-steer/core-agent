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

// Package scionremote implements core-agent's RemoteAgentSpawner
// against Scion's Hub HTTP API. When wired via
// agent.NewSpawnRemoteAgentTool, a core-agent running inside a
// Scion container can use spawn_remote_agent to spawn sibling
// agents in adjacent containers managed by the same Scion Hub.
//
// Typical wiring from inside a Scion-managed core-agent:
//
//	spawner, err := scionremote.New() // reads token/endpoint/project from env
//	if err != nil {
//	    spawner = agent.RefuseRemoteAgentSpawner(err.Error())
//	}
//	tool, _ := agent.NewSpawnRemoteAgentTool(spawner, bgMgr)
//	agent.New(m, agent.WithTools([]tool.Tool{tool, ...}))
//
// Environment variables consulted by New() when no explicit
// options are supplied:
//
//	SCION_AGENT_TOKEN    Bearer token (X-Scion-Agent-Token).
//	                     Injected by Scion at container startup.
//	SCION_HUB_ENDPOINT   Base URL of the Scion Hub (no trailing slash).
//	SCION_PROJECT_ID     Project ID to scope spawned agents to.
//	SCION_DEFAULT_TEMPLATE  Optional default template name; falls
//	                     back to "default" when unset.
package scionremote

import "os"

// envVars carries the four environment variables this package
// consults. Held in a struct so tests can pass a stub instead of
// touching the real os.Getenv.
type envVars struct {
	Token    string
	Endpoint string
	Project  string
	Template string
}

func loadEnv() envVars {
	return envVars{
		Token:    os.Getenv("SCION_AGENT_TOKEN"),
		Endpoint: os.Getenv("SCION_HUB_ENDPOINT"),
		Project:  os.Getenv("SCION_PROJECT_ID"),
		Template: os.Getenv("SCION_DEFAULT_TEMPLATE"),
	}
}
