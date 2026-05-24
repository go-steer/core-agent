// Copyright 2026 The Cogo Authors.
// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"context"
	"errors"
	"sync"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/go-steer/core-agent/mcp"
)

// tuiElicitor is the host-side bridge that turns an MCP server's
// elicitation request into a Bubble Tea modal, blocks on the user's
// answer, and hands the ElicitResult back to the SDK.
//
// Mirrors tuiPrompter: built before tea.NewProgram so MCP servers can
// reference it during connect, and rewired with the program sender
// after construction.
type tuiElicitor struct {
	mu   sync.Mutex
	send programSender
}

// newTUIElicitor returns an elicitor that initially has no program
// attached. attach() must be called once the program exists; until
// then the elicitor returns an error (the SDK will translate that
// into a server-side decline).
func newTUIElicitor() *tuiElicitor { return &tuiElicitor{} }

func (e *tuiElicitor) attach(p programSender) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.send = p
}

// Elicit implements mcp.ElicitorFn. It Send-s an elicitReqMsg into the
// running tea.Program (with a buffered reply chan) and blocks on the
// chan. ctx cancellation returns ctx.Err — the SDK will surface that
// as a protocol-level cancel.
func (e *tuiElicitor) Elicit(ctx context.Context, serverName string, req *mcpsdk.ElicitRequest) (*mcpsdk.ElicitResult, error) {
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
var _ mcp.ElicitorFn = (*tuiElicitor)(nil).Elicit
