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

import "github.com/go-steer/core-agent/v3/pkg/childenv"

// osEnviron is the parent environment a stdio server starts from: the
// daemon's, minus its own credentials (#1157). An MCP server is
// third-party code, and the attach token answers the agent's permission
// prompts. A server that genuinely needs one of those values still gets
// it, explicitly, through mcp.json's `env`, which is applied on top.
func osEnviron() []string { return childenv.Environ() }
