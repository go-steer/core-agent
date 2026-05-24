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

// Command core-agent-tui is the operator-facing TUI consumer of
// attach-mode — the remote client for an agent running elsewhere
// (on a workstation, in a K8s pod, or as a peer-registered fleet
// member). For the local-spawn use case use `core-agent` directly:
// since v2 (docs/embedded-tui-design-v2.md), bare `core-agent`
// launches the in-process TUI when stdin is a terminal, so there's
// no longer a reason to fork a second process just to get a UI.
//
// Usage:
//
//	core-agent-tui                       # welcome screen → enter URL
//	core-agent-tui <url> [--token=ENV]   # attach immediately
//
// URL forms (same as `core-agent attach`):
//
//	http(s)://host:port                              # hub: enumerates sessions
//	http(s)://host:port/sessions/<sid>               # direct-jump to session
//	http(s)://host:port/sessions/<app>/<sid>         # qualified direct-jump
//	unix:///path/to/socket                           # Unix socket hub
//	unix:///path/to/socket/sessions/<sid>            # Unix socket direct-jump
package main

import (
	"flag"
	"fmt"
	"os"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/go-steer/core-agent/internal/attachclient"
)

func main() {
	fs := flag.NewFlagSet("core-agent-tui", flag.ContinueOnError)
	tokenEnv := fs.String("token", "", "env var holding the bearer token (e.g. ATTACH_TOKEN)")
	theme := fs.String("theme", "dark", "glamour theme: auto | dark | light | notty. 'auto' queries the terminal for its background color via OSC 11 — under bubble tea's raw mode this response leaks into the input box ('\\033]11;rgb:...'), so 'dark' is the safe default.")
	alias := fs.String("alias", "", "display label override for the agent identity (default: agent name → sessionID)")
	if err := fs.Parse(os.Args[1:]); err != nil {
		return
	}
	args := fs.Args()

	token := ""
	if *tokenEnv != "" {
		token = os.Getenv(*tokenEnv)
	}

	var root rootModel
	switch {
	case len(args) > 0:
		parsed, err := attachclient.ParseURL(args[0])
		if err != nil {
			fmt.Fprintf(os.Stderr, "core-agent-tui: %v\n", err)
			os.Exit(2)
		}
		client := attachclient.New(parsed, token, 0)
		root = newRootModel(client, *theme, *alias)
	default:
		// Bare invocation: welcome screen prompts for a URL.
		root = newRootModel(nil, *theme, *alias)
	}

	prog := tea.NewProgram(root,
		tea.WithAltScreen(),
		tea.WithMouseCellMotion(),
	)
	if _, err := prog.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "core-agent-tui: %v\n", err)
		os.Exit(1)
	}
}
