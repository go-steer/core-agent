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

//go:build !no_tui

// File tui_enabled.go in the default build wires the core-tui-backed
// elicitor for MCP. The launchTUIv2 entrypoint lives in
// coretui_enabled.go. The lifted internal/tui/ tree and its
// launchTUI counterpart were retired in v2.1; CORE_AGENT_TUI=internal
// is no longer recognized.

package main

import (
	coretui "github.com/go-steer/core-tui/tui"

	"github.com/go-steer/core-agent/pkg/mcp"
)

// makeMCPElicitor constructs the core-tui elicitor and returns its
// elicit binding for mcp.Build. The constructed handle is stashed
// in pkgCoreElicitor so launchTUIv2 (coretui_enabled.go) can attach
// the same handle to the bubble-tea program.
//
// In the no_tui build (tui_disabled.go) this returns nil; MCP
// elicit requests then decline with the SDK's standard "no
// elicitor" cancel.
func makeMCPElicitor() mcp.ElicitorFn {
	pkgCoreElicitor = coretui.NewElicitor()
	w := &coreMCPElicitor{inner: pkgCoreElicitor}
	return w.elicit
}
