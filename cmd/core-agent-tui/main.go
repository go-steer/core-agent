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
// member). For local-spawn use, run `core-agent` directly — its
// in-process TUI is the default when stdin is a terminal.
//
// Architecture: a thin shell around go-steer/core-tui. URL parsing +
// optional session-picker run pre-TUI; once a session is resolved
// the shell constructs an internal/coretuiremote adapter and hands
// it to coretui.Run. The adapter wraps internal/attachclient so all
// remote-attach round-trips go through the same protocol the
// in-process core-agent surfaces.
//
// Usage:
//
//	core-agent-tui                       # bare: prompt for URL
//	core-agent-tui <url> [--token=ENV]   # attach immediately
//
// URL forms (same as `core-agent attach`):
//
//	http(s)://host:port                              # hub: enumerates sessions
//	http(s)://host:port/sessions/<sid>               # direct-jump
//	http(s)://host:port/sessions/<app>/<sid>         # qualified direct-jump
//	unix:///path/to/socket                           # Unix socket hub
//	unix:///path/to/socket/sessions/<sid>            # Unix socket direct-jump
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	tea "github.com/charmbracelet/bubbletea"
	coretui "github.com/go-steer/core-tui/tui"

	"github.com/go-steer/core-agent/internal/attachclient"
	"github.com/go-steer/core-agent/internal/coretuiremote"
	"github.com/go-steer/core-agent/internal/version"
)

func main() {
	// --version short-circuits before flag.Parse so the operator
	// can read it without satisfying any other flag requirements.
	for _, a := range os.Args[1:] {
		if a == "--version" || a == "-version" {
			fmt.Println(version.String("core-agent-tui"))
			return
		}
	}

	fs := flag.NewFlagSet("core-agent-tui", flag.ContinueOnError)
	tokenEnv := fs.String("token", "", "env var holding the bearer token (e.g. ATTACH_TOKEN)")
	theme := fs.String("theme", "", "force a theme: 'dark', 'light', or empty for auto (queries the terminal via OSC 11)")
	alias := fs.String("alias", "", "agent identity label shown in the status banner; default uses the session ID")
	if err := fs.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}
	args := fs.Args()

	token := ""
	if *tokenEnv != "" {
		token = os.Getenv(*tokenEnv)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	err := run(ctx, args, token, *theme, *alias)
	cancel()
	if err != nil {
		fmt.Fprintf(os.Stderr, "core-agent-tui: %v\n", err)
		os.Exit(1)
	}
}

// run resolves a session (parse URL → optional picker), constructs
// the coretuiremote adapter, and hands off to coretui.Run.
func run(ctx context.Context, args []string, token, theme, alias string) error {
	rawURL, err := chooseURL(args)
	if err != nil {
		return err
	}
	parsed, err := attachclient.ParseURL(rawURL)
	if err != nil {
		return fmt.Errorf("parse URL: %w", err)
	}
	client := attachclient.New(parsed, token, 0)

	sessionPath, err := resolveSessionPath(ctx, parsed, client)
	if err != nil {
		return err
	}

	a := coretuiremote.New(client, sessionPath)

	// Pre-fetch the static feeds (memory / skills / mcp). These
	// don't change during a session unless the operator triggers
	// /reload, which the adapter handles by re-fetching server-side
	// and surfacing the result in a system row. The TUI's display
	// of these slices stays static for the session — acceptable v1
	// tradeoff vs. wiring a refresh hook through coretui.
	memory := a.FetchMemory(ctx)
	skills := a.FetchSkills(ctx)
	mcpServers := a.FetchMCPServers(ctx)

	wordmark := "core-agent-tui"
	identity := alias
	if identity == "" {
		identity = displayIdentity(sessionPath)
	}

	opts := coretui.Options{
		Agent:        a,
		UsageTracker: a,
		ForceTheme:   theme,
		Memory:       memory,
		Skills:       skills,
		MCPServers:   mcpServers,
		Branding: coretui.Branding{
			Wordmark:      wordmark,
			AgentIdentity: identity,
		},
	}
	return coretui.Run(ctx, opts)
}

// chooseURL returns the attach URL — either from args[0] or via a
// stdin prompt for bare invocation. The bare-invocation path
// replaced the welcome.go bubble-tea screen (which v1 dropped in
// the remote-TUI-on-core-tui flip); operators who want pretty
// landing UX point bookmarks / shell aliases at the URL form.
func chooseURL(args []string) (string, error) {
	if len(args) > 0 {
		return strings.TrimSpace(args[0]), nil
	}
	fmt.Print("attach URL (e.g. http://localhost:7777 or http://host:7777/sessions/<sid>): ")
	scanner := bufio.NewScanner(os.Stdin)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return "", err
		}
		return "", errors.New("no URL entered")
	}
	url := strings.TrimSpace(scanner.Text())
	if url == "" {
		return "", errors.New("no URL entered")
	}
	return url, nil
}

// resolveSessionPath turns a parsed URL into a fully-qualified
// session path. Direct-jump URLs (those carrying /sessions/<sid>
// already) pass through; hub URLs trigger the picker so the
// operator can select among the registered sessions.
func resolveSessionPath(ctx context.Context, parsed *attachclient.ParsedURL, client *attachclient.Client) (string, error) {
	if !parsed.IsHubURL() {
		return parsed.Session, nil
	}
	return pickSession(ctx, client)
}

// pickSession runs the picker as a standalone tea.Program (pre-
// coretui handoff). Returns the picked session path or an error
// if the operator hit Ctrl+C without selecting.
func pickSession(ctx context.Context, client *attachclient.Client) (string, error) {
	pm := newPickerModel(client)
	prog := tea.NewProgram(&standalonePicker{pm: pm}, tea.WithContext(ctx))
	final, err := prog.Run()
	if err != nil {
		return "", fmt.Errorf("picker: %w", err)
	}
	sp, ok := final.(*standalonePicker)
	if !ok || sp.pm.selected == nil {
		return "", errors.New("no session selected")
	}
	return sp.pm.selected.sessionPath(), nil
}

// standalonePicker wraps the existing pickerModel so it can run as
// a self-contained tea.Program (calls tea.Quit on selection
// instead of relying on a parent rootModel to detect the
// `selected != nil` flag).
type standalonePicker struct {
	pm pickerModel
}

func (s *standalonePicker) Init() tea.Cmd { return s.pm.Init() }

func (s *standalonePicker) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if k, ok := msg.(tea.KeyMsg); ok && k.Type == tea.KeyCtrlC {
		return s, tea.Quit
	}
	if sz, ok := msg.(tea.WindowSizeMsg); ok {
		s.pm.SetSize(sz.Width, sz.Height)
	}
	var cmd tea.Cmd
	s.pm, cmd = s.pm.UpdateInner(msg)
	if s.pm.selected != nil {
		return s, tea.Quit
	}
	return s, cmd
}

func (s *standalonePicker) View() string { return s.pm.View() }

// displayIdentity derives a short label from a session path for
// the status-line banner when --alias wasn't passed. Returns the
// trailing SID (the operator-meaningful bit) rather than the
// app/sid form.
func displayIdentity(sessionPath string) string {
	trimmed := strings.TrimPrefix(sessionPath, "/sessions/")
	if i := strings.LastIndex(trimmed, "/"); i >= 0 {
		return trimmed[i+1:]
	}
	return trimmed
}
