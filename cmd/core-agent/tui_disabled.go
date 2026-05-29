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

//go:build no_tui

// Slim build (`go build -tags no_tui`) — the in-process bubble-tea
// TUI is excluded entirely so binary size drops by ~5 MB (bubble-tea
// + glamour + lipgloss + the lifted internal/tui tree). Use for
// K8s pods + CI runs + any deployment where the TUI is dead weight
// and the agent only runs headless (-p) or in attach-mode listen
// for remote clients.
//
// Runtime behavior: launchTUI always returns didRun=false so the
// caller falls through to the REPL fallback. Operators with a TTY
// who actually wanted a TUI get the line-mode REPL (no harm — the
// REPL still works for everything except the rich UI). A one-line
// stderr note tells them how to get the rich TUI back.

package main

import (
	"context"
	"fmt"
	"os"

	"github.com/go-steer/core-agent/pkg/mcp"
)

// makeMCPElicitor returns nil in the slim build — there's no TUI
// to attach to, so MCP elicitation requests fail with the standard
// "no elicitor" decline (the SDK surfaces this as a server-side
// cancel). Matches the pre-PR-1 behavior for headless deployments.
func makeMCPElicitor() mcp.ElicitorFn { return nil }

// launchTUI is a no-op in the slim build: prints one stderr line
// explaining why no TUI launched, returns didRun=false so main.go
// falls through to the REPL fallback exactly as if --no-tui had
// been passed at runtime.
//
// The didRun=false → REPL fallback path is the intentional design:
// a slim binary should be usable interactively for emergency
// debugging even though it lacks the rich UI.
func launchTUI(_ context.Context, _ tuiDeps) (didRun bool, exitCode int, err error) {
	fmt.Fprintln(os.Stderr,
		"core-agent: built with -tags no_tui; the bubble-tea TUI is excluded from this binary. Using the line-mode REPL. Install the default-tag binary for the full TUI.")
	return false, 0, nil
}

// launchTUIv2 mirrors launchTUI's no-op behavior in the slim build.
// Same stderr note, same fall-through to REPL — the env-var picker
// in main.go references both symbols at compile time so they both
// need a slim-build counterpart.
func launchTUIv2(ctx context.Context, deps tuiDeps) (didRun bool, exitCode int, err error) {
	return launchTUI(ctx, deps)
}
