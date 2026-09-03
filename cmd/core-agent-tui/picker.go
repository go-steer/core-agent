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

package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"

	"github.com/go-steer/core-agent/v2/internal/attachclient"
)

// pickerModel is the session-picker screen. Lists hub-local sessions
// + (when the listener is a peer-registration hub) sessions
// discovered on each peer, fetched in parallel with a per-peer
// timeout so a slow peer doesn't block the picker.
type pickerModel struct {
	client *attachclient.Client

	width, height int
	loading       bool
	error         string
	entries       []pickerEntry
	cursor        int

	// selected is set by Update when the operator hits Enter on a row;
	// the root model picks it up to switch into the chat view, then
	// nils it.
	selected *pickerEntry
}

func newPickerModel(client *attachclient.Client) pickerModel {
	return pickerModel{client: client, loading: true}
}

func (m *pickerModel) SetSize(w, h int) {
	m.width = w
	m.height = h
}

// Init kicks off the initial session enumeration.
func (m pickerModel) Init() tea.Cmd {
	return m.refreshCmd()
}

// refreshCmd fires off the GET /sessions (+ /peers + per-peer
// /sessions) gather and emits a single pickerSessionsLoadedMsg.
func (m pickerModel) refreshCmd() tea.Cmd {
	client := m.client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		// Prepend the "+ New session" sentinel — always shown, so the
		// picker has at least one actionable row even when the
		// authenticated caller has no sessions visible to them
		// (typical for multi-session deployments where each user
		// starts with zero sessions).
		out := []pickerEntry{{Kind: kindCreate, Origin: "local", Endpoint: client.URL.BaseURL}}
		// Local sessions on the supplied URL.
		local, err := client.ListSessions(ctx)
		if err != nil {
			return pickerSessionsLoadedMsg{err: err}
		}
		for _, s := range local {
			out = append(out, pickerEntry{
				App:           s.App,
				User:          s.User,
				SessionID:     s.SessionID,
				HasEventLog:   s.HasEventLog,
				Endpoint:      client.URL.BaseURL,
				Origin:        "local",
				LastTouchedAt: s.LastTouchedAt,
				Title:         s.Title,
			})
		}
		// Peers: best-effort. 404 (no peer-registration) is fine.
		peers, perr := client.ListPeers(ctx)
		if perr == nil && len(peers) > 0 {
			peerEntries := fetchPeerSessions(ctx, peers, client.Token)
			out = append(out, peerEntries...)
		}
		return pickerSessionsLoadedMsg{sessions: out}
	}
}

// fetchPeerSessions parallel-fans over peers, gathering each one's
// /sessions response. Per-peer timeout is bounded so the picker
// doesn't block on a single slow peer.
func fetchPeerSessions(parent context.Context, peers []attachclient.PeerDescriptor, token string) []pickerEntry {
	type result struct {
		entries []pickerEntry
	}
	results := make(chan result, len(peers))
	for _, p := range peers {
		go func(p attachclient.PeerDescriptor) {
			out := []pickerEntry{}
			parsed, err := attachclient.ParseURL(p.Endpoint)
			if err != nil {
				results <- result{nil}
				return
			}
			c := attachclient.New(parsed, token, 5*time.Second)
			ctx, cancel := context.WithTimeout(parent, 5*time.Second)
			defer cancel()
			sessions, err := c.ListSessions(ctx)
			if err != nil {
				results <- result{nil}
				return
			}
			for _, s := range sessions {
				out = append(out, pickerEntry{
					App:           s.App,
					User:          s.User,
					SessionID:     s.SessionID,
					HasEventLog:   s.HasEventLog,
					Endpoint:      p.Endpoint,
					Origin:        p.Name,
					LastTouchedAt: s.LastTouchedAt,
					Title:         s.Title,
				})
			}
			results <- result{out}
		}(p)
	}
	var all []pickerEntry
	for i := 0; i < len(peers); i++ {
		select {
		case r := <-results:
			all = append(all, r.entries...)
		case <-parent.Done():
			return all
		}
	}
	return all
}

// orderEntries stamps each row's creation time (decoded from a
// UUIDv7 session ID) and sorts the list newest-first, with the
// "+ New session" sentinel pinned to the top.
//
// Before this the order was whatever the fetch produced: the
// listener's in-memory half sorted by (app, user, sessionID), its
// persisted-ACL half appended in store order, then each peer's rows
// appended in goroutine-completion order — i.e. grouped by where a
// session came from, and not stable between refreshes once peers are
// in play. Operators reach for the session they just left, so recency
// is the useful axis; grouping by origin is what the ORIGIN column is
// for.
//
// Rows whose creation time can't be recovered fall back to the
// listener's last-activity stamp, and rows with neither sort to the
// bottom in (app, session ID) order so they at least stay put.
func orderEntries(entries []pickerEntry) []pickerEntry {
	out := make([]pickerEntry, len(entries))
	copy(out, entries)
	for i := range out {
		if out[i].CreatedAt.IsZero() {
			if t, ok := uuidV7Time(out[i].SessionID); ok {
				out[i].CreatedAt = t
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if (a.Kind == kindCreate) != (b.Kind == kindCreate) {
			return a.Kind == kindCreate
		}
		ta, tb := a.sortTime(), b.sortTime()
		if !ta.Equal(tb) {
			return ta.After(tb) // newest first; zero time sinks
		}
		if a.App != b.App {
			return a.App < b.App
		}
		return a.SessionID < b.SessionID
	})
	return out
}

// UpdateInner is the picker's Update. Returns the (possibly mutated)
// picker plus a tea.Cmd. The root model checks .selected after each
// Update to detect a session pick.
func (m pickerModel) UpdateInner(msg tea.Msg) (pickerModel, tea.Cmd) {
	switch msg := msg.(type) {
	case pickerSessionsLoadedMsg:
		m.loading = false
		if msg.err != nil {
			m.error = msg.err.Error()
		} else {
			m.error = ""
			m.entries = orderEntries(msg.sessions)
			if m.cursor >= len(m.entries) {
				m.cursor = 0
			}
		}
		return m, nil
	case tea.KeyPressMsg:
		if m.loading {
			return m, nil
		}
		switch msg.String() {
		case "ctrl+r", "r":
			m.loading = true
			return m, m.refreshCmd()
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.cursor < len(m.entries)-1 {
				m.cursor++
			}
		case "enter":
			if len(m.entries) > 0 {
				e := m.entries[m.cursor]
				if e.Kind == kindCreate {
					m.loading = true
					m.error = ""
					return m, m.createSessionCmd()
				}
				m.selected = &e
			}
		case "q", "esc":
			return m, tea.Quit
		}
	case pickerSessionCreatedMsg:
		m.loading = false
		if msg.err != nil {
			m.error = msg.err.Error()
			return m, nil
		}
		// Attach directly to the freshly-created session — operator
		// already committed to "+ New session" so the extra "select
		// the row that just appeared" step would be friction.
		e := msg.entry
		m.selected = &e
		return m, nil
	}
	return m, nil
}

// createSessionCmd POSTs /sessions, then dispatches the returned
// descriptor as a pickerSessionCreatedMsg. The picker treats the
// create as a no-op on error (the error is surfaced inline; the
// operator can press 'r' to refresh and try again).
func (m pickerModel) createSessionCmd() tea.Cmd {
	client := m.client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		resp, err := client.NewSession(ctx)
		if err != nil {
			return pickerSessionCreatedMsg{err: err}
		}
		return pickerSessionCreatedMsg{entry: pickerEntry{
			App:         resp.AppName,
			User:        resp.UserID,
			SessionID:   resp.SessionID,
			HasEventLog: true, // POST /sessions always returns an event-log-backed session
			Endpoint:    client.URL.BaseURL,
			Origin:      "local",
		}}
	}
}

// View renders the picker. Two-section layout: header + row list +
// hint footer.
func (m pickerModel) View() string {
	header := styleStatusBar.Width(m.width).Render(
		fmt.Sprintf("core-agent-tui  ●  %s  ·  session picker", m.client.URL.BaseURL),
	)
	var body strings.Builder
	switch {
	case m.loading:
		body.WriteString("\n  loading sessions…\n")
	case m.error != "":
		body.WriteString("\n  " + styleBubbleErr.Render("error: "+m.error) + "\n")
		body.WriteString("\n  press 'r' to retry, 'q' to quit.\n")
	case len(m.entries) == 0:
		body.WriteString("\n  no sessions registered.\n")
		body.WriteString("\n  press 'r' to refresh, 'q' to quit.\n")
	default:
		// Entry count excludes the synthetic "+ New session" sentinel
		// so the hint matches the operator's mental model of "real
		// sessions visible to me."
		realCount := 0
		for _, e := range m.entries {
			if e.Kind != kindCreate {
				realCount++
			}
		}
		// Every row is indented by the gutter ("  ") plus the cursor
		// column ("▸ "), so the table gets what's left.
		const gutter = 4
		avail := m.width - gutter
		if m.width <= 0 {
			avail = 80 - gutter // pre-WindowSizeMsg render
		}
		if avail < 20 {
			avail = 20
		}

		// Keys live in the footer; the hint just says what's on
		// screen and in what order. Elided rather than wrapped so a
		// narrow terminal doesn't reflow the table down the screen.
		body.WriteString("\n  ")
		body.WriteString(styleHint.Render(elide(fmt.Sprintf("%d session(s) · newest first", realCount), avail, false)))
		body.WriteString("\n\n")
		now := time.Now()
		cols, widths := fitColumns(pickerColumns(), m.entries, now, avail)

		body.WriteString("    " + styleTableHeader.Render(renderRow(cols, widths, headerCells(cols))) + "\n")
		fmt.Fprintf(&body, "    %s\n", strings.Repeat("─", min(tableWidth(widths), avail)))
		for i, e := range m.entries {
			cursor := "  "
			var line string
			if e.Kind == kindCreate {
				// Distinct styling so the sentinel doesn't look like
				// a real session. Operators reading top-to-bottom see
				// the affordance first.
				label := "+ New session"
				if i == m.cursor {
					cursor = "▸ "
					line = lipgloss.NewStyle().Foreground(colorAccent).Bold(true).Render(label)
				} else {
					line = lipgloss.NewStyle().Foreground(colorAccent).Render(label)
				}
			} else {
				line = renderRow(cols, widths, rowCells(cols, e, now))
				if i == m.cursor {
					cursor = "▸ "
					line = lipgloss.NewStyle().Foreground(colorAccent).Bold(true).Render(line)
				}
			}
			body.WriteString("  " + cursor + line + "\n")
		}
	}
	footer := styleFooter.Width(m.width).Render(
		"  ↑/↓ navigate · Enter attach · r refresh · q quit",
	)

	// Pad with blank lines so the footer pins to the bottom.
	bodyStr := body.String()
	bodyLines := strings.Count(bodyStr, "\n")
	pad := m.height - bodyLines - 2 // header + footer
	if pad > 0 {
		bodyStr += strings.Repeat("\n", pad)
	}
	return header + "\n" + bodyStr + footer
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
