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
// docs/attach-tui-design.md + docs/embedded-tui-design.md for the
// full design.
//
// Usage:
//
//	core-agent-tui                       # welcome screen → pick local or remote
//	core-agent-tui --local               # spawn a local agent, attach
//	core-agent-tui --local -- <args>     # forward args after `--` to the spawned agent
//	core-agent-tui <url> [--token=ENV]   # remote attach (v1.8.0 behavior)
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
	"context"
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
	local := fs.Bool("local", false, "spawn a core-agent process locally on a unix socket and attach to it (alternative to passing a URL)")
	noCleanup := fs.Bool("no-cleanup", false, "with --local: leave the spawned agent + socket in place on TUI exit (default: SIGTERM the agent + remove socket)")
	if err := fs.Parse(os.Args[1:]); err != nil {
		return
	}
	args := fs.Args()
	rawURL := ""
	extraArgs := []string(nil)
	if *local {
		// With --local, any trailing positional args after a `--`
		// separator forward to the spawned agent. The flag
		// package already strips `--` from the head of args.
		extraArgs = args
	} else if len(args) > 0 {
		rawURL = args[0]
	}

	token := ""
	if *tokenEnv != "" {
		token = os.Getenv(*tokenEnv)
	}

	var root rootModel
	switch {
	case *local:
		// Spawn synchronously so we can fail-fast before opening
		// the TUI. The spawn handle stays on the rootModel for
		// cleanup on exit.
		spawn, err := spawnLocalAgent(context.Background(), extraArgs)
		if err != nil {
			fmt.Fprintf(os.Stderr, "core-agent-tui --local: %v\n", err)
			os.Exit(2)
		}
		spawn.keep = *noCleanup
		parsed, perr := attachclient.ParseURL(spawn.url())
		if perr != nil {
			spawn.shutdown()
			fmt.Fprintf(os.Stderr, "core-agent-tui --local: %v\n", perr)
			os.Exit(2)
		}
		client := attachclient.New(parsed, spawn.token, 0)
		root = newRootModel(client, *theme, *alias)
		root.spawn = spawn
		root.noCleanup = *noCleanup
		// No defer here — the post-Run cleanup below handles spawn shutdown.
		// (defer + later os.Exit on the rawURL parse path would trip
		//  gocritic's exitAfterDefer warning.)
	case rawURL != "":
		parsed, err := attachclient.ParseURL(rawURL)
		if err != nil {
			fmt.Fprintf(os.Stderr, "core-agent-tui: %v\n", err)
			os.Exit(2)
		}
		client := attachclient.New(parsed, token, 0)
		root = newRootModel(client, *theme, *alias)
	default:
		// Bare invocation: welcome screen.
		root = newRootModel(nil, *theme, *alias)
		root.noCleanup = *noCleanup
	}

	prog := tea.NewProgram(root,
		tea.WithAltScreen(),
		tea.WithMouseCellMotion(),
	)
	final, err := prog.Run()
	// Final model may carry a spawn handle from welcome-driven or
	// /spawn-driven spawns; clean those up too.
	if rm, ok := final.(rootModel); ok && rm.spawn != nil {
		rm.spawn.shutdown()
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "core-agent-tui: %v\n", err)
		os.Exit(1)
	}
}
