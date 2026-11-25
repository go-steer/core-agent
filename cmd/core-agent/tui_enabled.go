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

	"github.com/go-steer/core-agent/v3/pkg/mcp"
)

// tuiElicitorHandle carries the core-tui elicitor instance between
// makeMCPElicitor (which wires its mcp.ElicitorFn binding into
// mcp.Build) and launchTUIv2 (which passes the same handle into
// coretui.Options). Wrapping it in a package-local struct keeps
// tui_deps.go and run_steps.go free of core-tui imports so -tags no_tui
// builds stay slim while avoiding package-global mutable state across
// runs and parallel tests.
type tuiElicitorHandle struct {
	inner coretui.Elicitor
}

// makeMCPElicitor constructs the core-tui elicitor and returns both its
// mcp.ElicitorFn binding for mcp.Build and the matching handle for
// launchTUIv2. When interactive is false (headless -p, --no-repl daemon,
// --no-tui, or non-TTY stdin), it returns (nil, nil) so MCP clients omit
// the elicitation capability and never park on an undrained TUI channel
// (#1606).
//
// In the no_tui build (tui_disabled.go) this always returns (nil, nil).
func makeMCPElicitor(interactive bool) (mcp.ElicitorFn, *tuiElicitorHandle) {
	return newMCPElicitor(interactive, coretui.NewElicitor)
}

func newMCPElicitor(interactive bool, factory func() coretui.Elicitor) (mcp.ElicitorFn, *tuiElicitorHandle) {
	if !interactive {
		return nil, nil
	}
	el := factory()
	w := &coreMCPElicitor{inner: el}
	return w.elicit, &tuiElicitorHandle{inner: el}
}
