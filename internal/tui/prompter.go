// Copyright 2026 The Cogo Authors.
// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"context"
	"errors"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/go-steer/core-agent/pkg/permissions"
)

// confirmReqMsg is sent into the Bubble Tea program from a tool
// goroutine when the permission gate needs user input. It carries a
// reply channel; the model writes the user's decision back through it.
//
// Both the goroutine and the model must agree to use a buffered
// channel of capacity 1 so neither side blocks if the other has
// already moved on (e.g. the program is quitting).
type confirmReqMsg struct {
	Req permissions.PromptRequest
	Out chan permissions.Decision
}

// tuiPrompter implements permissions.Prompter by Send-ing a
// confirmReqMsg into the running tea.Program and blocking on the
// reply channel.
type tuiPrompter struct {
	send programSender
}

// NewPrompter returns a Prompter wired to send into prog.
func NewPrompter(prog programSender) permissions.Prompter {
	return &tuiPrompter{send: prog}
}

func (t *tuiPrompter) AskApproval(ctx context.Context, req permissions.PromptRequest) (permissions.Decision, error) {
	if t.send == nil {
		return permissions.DecisionDeny, errors.New("tui prompter: no program attached")
	}
	out := make(chan permissions.Decision, 1)
	t.send.Send(confirmReqMsg{Req: req, Out: out})
	select {
	case d := <-out:
		return d, nil
	case <-ctx.Done():
		// Drain the channel asynchronously if the model later replies
		// after we've given up; the buffered chan ensures the send
		// won't block.
		go func() { <-out }()
		return permissions.DecisionDeny, ctx.Err()
	}
}

// Compile-time check.
var _ tea.Msg = confirmReqMsg{}
