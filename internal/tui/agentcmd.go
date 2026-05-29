// Copyright 2026 The Cogo Authors.
// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"context"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/go-steer/core-agent/pkg/agent"
)

// programSender is the subset of *tea.Program the goroutine uses. We
// take this as an interface so tests can inject a fake without spinning
// up a real program.
type programSender interface {
	Send(tea.Msg)
}

// startAgentTurn launches an agent turn in a goroutine. It returns
// immediately; subsequent events (streamChunkMsg, usageMsg, turnDoneMsg,
// turnErrMsg, turnCancelledMsg) flow through send.Send.
//
// ctx controls the turn — cancelling it interrupts the model and tools
// mid-stream and the goroutine exits with turnCancelledMsg.
func startAgentTurn(ctx context.Context, send programSender, a *agent.Agent, prompt string) {
	go func() {
		var lastIn, lastOut int
		// ADK can emit the same FunctionCall on more than one event
		// (e.g., a partial streaming event followed by the committed
		// non-partial event). Deduping by FunctionCall.ID keeps the
		// chat from showing every tool invocation twice.
		seenToolCallIDs := make(map[string]bool)
		for event, err := range a.Run(ctx, prompt) {
			if err != nil {
				if lastIn > 0 || lastOut > 0 {
					send.Send(usageMsg{InputTokens: lastIn, OutputTokens: lastOut})
				}
				if ctx.Err() != nil {
					send.Send(turnCancelledMsg{})
				} else {
					send.Send(turnErrMsg{Err: err})
				}
				return
			}
			if event.UsageMetadata != nil {
				lastIn = int(event.UsageMetadata.PromptTokenCount)
				lastOut = int(event.UsageMetadata.CandidatesTokenCount)
			}
			if event.Content == nil {
				continue
			}
			// Surface each unique tool invocation to the chat. Dedupe
			// on FunctionCall.ID so we don't emit the same call twice
			// when ADK echoes it across partial + committed events.
			// Calls without an ID are emitted unconditionally — a
			// missing ID is rare but a tool line is more useful than
			// a silent drop.
			for _, p := range event.Content.Parts {
				if p.FunctionCall == nil || p.FunctionCall.Name == "" {
					continue
				}
				id := p.FunctionCall.ID
				if id != "" && seenToolCallIDs[id] {
					continue
				}
				if id != "" {
					seenToolCallIDs[id] = true
				}
				send.Send(toolCallMsg{
					Name: p.FunctionCall.Name,
					Args: p.FunctionCall.Args,
				})
			}
			if !event.Partial {
				continue
			}
			for _, p := range event.Content.Parts {
				if p.Text != "" {
					send.Send(streamChunkMsg{Text: p.Text})
				}
			}
		}
		// Iterator drained.
		if lastIn > 0 || lastOut > 0 {
			send.Send(usageMsg{InputTokens: lastIn, OutputTokens: lastOut})
		}
		if ctx.Err() != nil {
			send.Send(turnCancelledMsg{})
			return
		}
		send.Send(turnDoneMsg{})
	}()
}
