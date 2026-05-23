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
// attach-mode. Ships as a separate binary so the default core-agent
// stays bubble-tea-free (and distroless-clean). See
// docs/attach-tui-design.md for the full design.
//
// Usage:
//
//	core-agent-tui <url> [--token=ENVVAR] [--theme=auto|dark|light] [--alias=NAME]
//
// URL forms (same as core-agent attach):
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
	theme := fs.String("theme", "auto", "glamour theme: auto | dark | light | notty")
	alias := fs.String("alias", "", "display label override for the agent identity (default: agent name → sessionID)")
	if err := fs.Parse(os.Args[1:]); err != nil {
		return
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "core-agent-tui: URL is required")
		fmt.Fprintln(os.Stderr, "usage: core-agent-tui <url> [--token=ENVVAR] [--theme=auto|dark|light] [--alias=NAME]")
		os.Exit(2)
	}
	rawURL := fs.Arg(0)
	token := ""
	if *tokenEnv != "" {
		token = os.Getenv(*tokenEnv)
	}

	parsed, err := attachclient.ParseURL(rawURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "core-agent-tui: %v\n", err)
		os.Exit(2)
	}
	client := attachclient.New(parsed, token, 0)

	root := newRootModel(client, *theme, *alias)
	prog := tea.NewProgram(root,
		tea.WithAltScreen(),
		tea.WithMouseCellMotion(), // limited mouse for viewport scrolling
	)
	if _, err := prog.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "core-agent-tui: %v\n", err)
		os.Exit(1)
	}
}
