// Copyright 2026 The Cogo Authors.
// SPDX-License-Identifier: Apache-2.0

package tui

import (
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Typed tea.Msg events for the TUI.
//
// All custom message types live here so the Update function has one
// switch-target to refer to. Bubble Tea's tea.Msg is interface{}; we
// use small struct types for type safety.

// elicitReqMsg is sent by mcp/elicitation bridge when an MCP server
// asks the user for input. Carries the parsed request plus a buffered
// reply channel that the TUI writes the user's response to.
type elicitReqMsg struct {
	ServerName string
	Req        *mcpsdk.ElicitRequest
	Out        chan *mcpsdk.ElicitResult
}

// streamChunkMsg is emitted by the agent goroutine for each Partial
// event. Text is the raw token chunk from the model.
type streamChunkMsg struct {
	Text string
}

// toolCallMsg is emitted by the agent goroutine when the model decides
// to invoke a tool. The TUI renders a one-line summary in the chat so
// the user can see the agent's actions interleaved with its prose.
// Args is the raw key/value map from the model; the renderer pulls a
// brief summary out of it (the bash command, the file path, etc.).
type toolCallMsg struct {
	Name string
	Args map[string]any
}

// turnDoneMsg signals the agent has finished the current turn cleanly.
// The TUI uses this to flip state back to idle and to apply the Glamour
// re-render to the in-progress assistant message.
type turnDoneMsg struct{}

// turnErrMsg carries an unrecoverable error from the agent goroutine.
// Treated as a turn end (cleared spinner, re-enable input) plus a
// system-error message in the chat.
type turnErrMsg struct {
	Err error
}

// turnCancelledMsg is sent when the user interrupts a turn via Ctrl+C.
// Distinct from turnErrMsg so we can show a tidy "(interrupted)" notice
// instead of an error banner.
type turnCancelledMsg struct{}

// usageMsg carries the most recent usage metadata seen on an event,
// emitted right before turnDoneMsg / turnErrMsg / turnCancelledMsg so
// the model has a single place to update the tracker per turn.
type usageMsg struct {
	InputTokens  int
	OutputTokens int
}
