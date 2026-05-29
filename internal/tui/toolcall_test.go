// Copyright 2026 The Cogo Authors.
// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/go-steer/core-agent/pkg/config"
)

// TestFormatToolCall covers the per-tool arg-hint logic. The hint is
// best-effort — unknown tools fall through to bare-name rendering —
// but the recognized cogo built-ins (bash, read_file, write_file,
// grep) MUST surface their primary argument so the chat line
// actually tells the user what's happening.
func TestFormatToolCall(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		toolnm string
		args   map[string]any
		want   string
	}{
		{"bare unknown tool", "do_something", nil, "do_something"},
		{"bare tool empty args", "do_something", map[string]any{}, "do_something"},
		{"bash with command", "bash", map[string]any{"command": "ls -la"}, "bash · $ ls -la"},
		{"bash with cmd alias", "bash", map[string]any{"cmd": "go test ./..."}, "bash · $ go test ./..."},
		{"bash collapses newlines", "bash", map[string]any{"command": "echo a\necho b"}, "bash · $ echo a echo b"},
		{"read_file path", "read_file", map[string]any{"path": "internal/tui/model.go"}, "read_file · internal/tui/model.go"},
		{"read_file file alias", "read_file", map[string]any{"file": "README.md"}, "read_file · README.md"},
		{"write_file path", "write_file", map[string]any{"path": "out.txt"}, "write_file · out.txt"},
		{"grep pattern + path", "grep", map[string]any{"pattern": "TODO", "path": "internal/"}, "grep · \"TODO\" in internal/"},
		{"grep pattern only", "grep", map[string]any{"pattern": "FIXME"}, "grep · \"FIXME\""},
		{"unknown tool with args still bare", "weird_tool", map[string]any{"x": 42}, "weird_tool"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := formatToolCall(tc.toolnm, tc.args); got != tc.want {
				t.Errorf("formatToolCall(%q, %v) = %q; want %q", tc.toolnm, tc.args, got, tc.want)
			}
		})
	}
}

// TestFormatToolCall_LongHintTruncated guards the absolute-upper-bound
// cap on the inline hint. Without this, a giant inlined file or a 4 KB
// bash heredoc would push the chat line into many wrap rows.
func TestFormatToolCall_LongHintTruncated(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("a", 500)
	out := formatToolCall("bash", map[string]any{"command": long})
	if !strings.HasSuffix(out, "…") {
		t.Errorf("oversized hint should end in '…'; got: %q", out[len(out)-20:])
	}
	if len(out) > 250 {
		t.Errorf("formatToolCall did not cap a 500-char hint; got %d chars", len(out))
	}
}

// TestRenderMessage_ToolCallShowsIconAndName pins the chat-display
// contract for tool calls: the rendered line contains the ⚙ icon and
// the tool name (and any arg hint), wrapped + styled through the same
// HeaderAccent path that the model name in the header uses (proven
// stable on every host we test).
//
// DO NOT silence this test if it breaks. The whole point of the
// feature is that the user can see, mid-stream, which tools the agent
// is invoking; losing the icon, the name, or the visibility on a
// stable styling path defeats the affordance.
func TestRenderMessage_ToolCallShowsIconAndName(t *testing.T) {
	t.Parallel()
	cfg := config.DefaultConfig()
	m := NewModel(cfg, nil, "dark")
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 24})
	m.history.Append(Message{Role: RoleUser, Text: "list files"})
	m.history.Append(Message{Role: RoleTool, Text: formatToolCall("bash", map[string]any{"command": "ls -la"})})
	m.refreshViewport()
	body := stripANSI(m.viewport.View())
	if !strings.Contains(body, "⚙") {
		t.Errorf("tool line missing ⚙ icon; got:\n%s", body)
	}
	if !strings.Contains(body, "bash") {
		t.Errorf("tool line missing tool name 'bash'; got:\n%s", body)
	}
	if !strings.Contains(body, "ls -la") {
		t.Errorf("tool line missing arg hint 'ls -la'; got:\n%s", body)
	}
}

// TestUpdate_ToolCallSplitsAssistantSegments pins the layout fix from
// the dogfood report: tool calls and the assistant chunks that arrive
// AFTER them must show up *below* the tool line in the chat, not
// pinned beneath the assistant text. Concretely:
//
//   - Assistant text "before tool" lands in segment A.
//   - Tool call appended -> segment A is closed (Glamour-rendered, idx
//     reset).
//   - Assistant text "after tool" lands in a NEW segment B.
//   - History order ends up: user, assistant("before tool"),
//     tool, assistant("after tool").
//
// Without this, the user sees the model's response permanently above
// every tool call and permission prompt of the turn — the bug the
// user reported as "tool calls and permissions are pinned to the
// bottom of the viewport".
//
// DO NOT silence this test. Re-introducing the pre-created assistant
// placeholder, or letting handleStreamChunk dump everything into one
// message regardless of intervening tool calls, brings the bug back.
func TestUpdate_ToolCallSplitsAssistantSegments(t *testing.T) {
	t.Parallel()
	cfg := config.DefaultConfig()
	m := NewModel(cfg, nil, "dark")
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 24})

	// Simulate a turn: state=streaming, no assistant placeholder.
	m.state = StateStreaming
	m.currentAssistantIdx = -1
	m.history.Append(Message{Role: RoleUser, Text: "list files"})

	// First chunk lands BEFORE any tool call.
	m.Update(streamChunkMsg{Text: "I'll list them for you."})
	// Tool call closes that segment.
	m.Update(toolCallMsg{Name: "bash", Args: map[string]any{"command": "ls"}})
	// Next chunk MUST start a new assistant message below the tool.
	m.Update(streamChunkMsg{Text: "Here are the files."})

	got := m.history.Snapshot()
	if len(got) < 4 {
		t.Fatalf("expected at least 4 history entries (user, assistant1, tool, assistant2); got %d:\n%+v", len(got), got)
	}
	wantRoles := []Role{RoleUser, RoleAssistant, RoleTool, RoleAssistant}
	for i, want := range wantRoles {
		if got[i].Role != want {
			t.Errorf("history[%d].Role = %v; want %v\nfull history: %+v", i, got[i].Role, want, got)
		}
	}
	// First assistant segment carries the "before tool" text.
	if !strings.Contains(got[1].Text, "I'll list them for you") {
		t.Errorf("history[1] should hold pre-tool assistant text; got %q", got[1].Text)
	}
	// Second assistant segment carries the "after tool" text.
	if !strings.Contains(got[3].Text, "Here are the files") {
		t.Errorf("history[3] should hold post-tool assistant text; got %q", got[3].Text)
	}
	// The pre-tool segment must NOT have absorbed the post-tool text.
	if strings.Contains(got[1].Text, "Here are the files") {
		t.Errorf("history[1] absorbed post-tool text; should have been closed when the tool call landed. got: %q", got[1].Text)
	}
}

// TestUpdate_ToolCallMsgAppendsHistory pins the wiring from agent
// goroutine to chat history: a toolCallMsg that arrives in Update
// MUST cause a RoleTool entry to land in m.history with the formatted
// summary text. Without this, the agentcmd.go FunctionCall hook is a
// no-op for the user.
func TestUpdate_ToolCallMsgAppendsHistory(t *testing.T) {
	t.Parallel()
	cfg := config.DefaultConfig()
	m := NewModel(cfg, nil, "dark")
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 24})
	before := m.history.Len()
	m.Update(toolCallMsg{Name: "read_file", Args: map[string]any{"path": "go.mod"}})
	if m.history.Len() != before+1 {
		t.Fatalf("toolCallMsg should append exactly one history entry; was %d, now %d", before, m.history.Len())
	}
	last := m.history.Snapshot()[m.history.Len()-1]
	if last.Role != RoleTool {
		t.Errorf("appended message has Role=%v; want RoleTool", last.Role)
	}
	if !strings.Contains(last.Text, "read_file") || !strings.Contains(last.Text, "go.mod") {
		t.Errorf("appended message text should contain tool name + arg hint; got %q", last.Text)
	}
}
