// Copyright 2026 The Cogo Authors.
// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"context"
	"errors"
	"sync"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/go-steer/core-agent/pkg/mcp"
)

// Elicitor is the host-side bridge that turns an MCP server's
// elicitation request into a Bubble Tea modal, blocks on the user's
// answer, and hands the ElicitResult back to the SDK.
//
// Mirrors the prompter pattern: built before tea.NewProgram so MCP
// servers (constructed pre-TUI in cmd/core-agent/main.go) can hold
// the Elicit method pointer at connect time, and rewired with the
// program sender after tui.Run constructs the program. Exposed so
// callers in cmd/ can plumb the elicitor through tui.Options
// instead of the TUI having to own MCP construction.
type Elicitor struct {
	mu   sync.Mutex
	send programSender
}

// NewElicitor returns an Elicitor that initially has no program
// attached. tui.Run attaches the running program internally; until
// then Elicit returns an error (the SDK translates that into a
// server-side decline).
func NewElicitor() *Elicitor { return &Elicitor{} }

func (e *Elicitor) attach(p programSender) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.send = p
}

// Elicit implements mcp.ElicitorFn. It Send-s an elicitReqMsg into the
// running tea.Program (with a buffered reply chan) and blocks on the
// chan. ctx cancellation returns ctx.Err — the SDK will surface that
// as a protocol-level cancel.
func (e *Elicitor) Elicit(ctx context.Context, serverName string, req *mcpsdk.ElicitRequest) (*mcpsdk.ElicitResult, error) {
	e.mu.Lock()
	send := e.send
	e.mu.Unlock()
	if send == nil {
		return nil, errors.New("tui elicitor: no program attached")
	}
	out := make(chan *mcpsdk.ElicitResult, 1)
	send.Send(elicitReqMsg{ServerName: serverName, Req: req, Out: out})
	select {
	case res := <-out:
		if res == nil {
			return &mcpsdk.ElicitResult{Action: "decline"}, nil
		}
		return res, nil
	case <-ctx.Done():
		// Drain any late reply so the modal handler doesn't block
		// forever on a buffered send (the chan is cap 1, so the drain
		// only matters if it had a reader; safe to leak the goroutine
		// since the chan is GC'd once both sides drop refs).
		go func() { <-out }()
		return nil, ctx.Err()
	}
}

// Compile-time check that the bridge satisfies the mcp.ElicitorFn shape.
var _ mcp.ElicitorFn = (*Elicitor)(nil).Elicit
