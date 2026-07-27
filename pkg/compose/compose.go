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

// Package compose lifts reusable substrate wiring out of
// cmd/core-agent so library consumers can build the same agent
// stack the bundled binary does, per
// docs/compose-extraction-design.md (#386): substrate builders
// (compactor, agentic tools, MCP digest LLM fallback, context
// cache), operator-visible formatters, pricing operations, grant
// persistence, and multi-session construction (session factory,
// resumer, and authn wiring). Flag parsing and run() orchestration
// stay in the binary.
package compose
