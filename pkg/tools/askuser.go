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

package tools

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"

	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
)

// Prompter delivers a question from the agent to a human (or any
// external authority) and returns their answer. Implementations
// decide the channel — stdin, websocket, queue, file watch, etc.
//
// Returning a non-nil error signals "no user available" or transport
// failure; the agent sees the error string as the tool's response and
// can adapt (typically by proceeding with its best assumption or
// reporting the problem) instead of blocking forever.
//
// Prompt should respect ctx cancellation so SIGTERM, deadlines, and
// agent-driven aborts unblock the tool cleanly.
type Prompter interface {
	Prompt(ctx context.Context, question string) (string, error)
}

// PrompterFunc adapts a function to the Prompter interface for
// one-off uses.
type PrompterFunc func(ctx context.Context, question string) (string, error)

// Prompt implements Prompter.
func (f PrompterFunc) Prompt(ctx context.Context, q string) (string, error) {
	return f(ctx, q)
}

// AskUserOptions configures NewAskUserTool.
type AskUserOptions struct {
	// Prompter is the consumer's delivery mechanism. Required.
	Prompter Prompter

	// Name overrides the tool's function name. Defaults to "ask_user"
	// when empty.
	Name string

	// Description overrides the tool's description (the prose the
	// model sees in its function-decl list). Empty falls back to a
	// sensible default.
	Description string
}

const (
	defaultAskUserName        = "ask_user"
	defaultAskUserDescription = "Ask the user a clarifying question and wait for their answer. Use sparingly — only when you need information you don't have to make progress, and not when you can reasonably proceed with a default. The user's answer is returned as the tool's response."
)

type askUserArgs struct {
	Question string `json:"question" jsonschema:"the question to ask the user — keep it short and specific"`
}

type askUserResult struct {
	Answer string `json:"answer"`
}

// NewAskUserTool wraps a Prompter as an ADK tool the agent can call
// during a turn. The tool's handler blocks until the prompter
// responds, then returns the answer as the tool result — so the
// agent's reasoning continues in the same turn (no end-of-turn break).
//
// Use the in-turn pattern when the wait is short (interactive CLI,
// quick approval). For long waits where the model should release the
// turn (Scion-style: emit a status, end the turn, driver loop reads
// the next stdin message and starts a fresh agent.Run), use a
// status-emitting tool instead and let your driver loop handle the
// hand-off.
//
// Returns an error only if opts.Prompter is nil.
func NewAskUserTool(opts AskUserOptions) (tool.Tool, error) {
	if opts.Prompter == nil {
		return nil, fmt.Errorf("tools: NewAskUserTool: Prompter is required")
	}
	name := opts.Name
	if name == "" {
		name = defaultAskUserName
	}
	desc := opts.Description
	if desc == "" {
		desc = defaultAskUserDescription
	}
	return functiontool.New(
		functiontool.Config{Name: name, Description: desc},
		func(ctx tool.Context, in askUserArgs) (askUserResult, error) {
			ans, err := opts.Prompter.Prompt(ctx, in.Question)
			if err != nil {
				// Surface the error as the tool result so the model
				// sees it in conversation context and can adapt,
				// rather than aborting the turn.
				return askUserResult{Answer: fmt.Sprintf("(no user available: %v)", err)}, nil
			}
			return askUserResult{Answer: ans}, nil
		},
	)
}

// StdinPrompter reads the user's answer from in (typically os.Stdin).
// The question is written to out (typically os.Stderr) so it doesn't
// pollute stdout. Each call reads one newline-terminated line and
// returns it with surrounding whitespace trimmed.
//
// Suitable for interactive CLI use and for stdin-driven adapters
// (e.g. Scion's tmux-fed message loop) where one line of input maps
// to one user answer.
//
// Cancellation note: the underlying os.Stdin read isn't ctx-aware on
// every platform, so a hung read may not unblock until a newline
// arrives. For long-blocking deployments, wrap with a goroutine +
// channel pattern in your own Prompter.
func StdinPrompter(in io.Reader, out io.Writer) Prompter {
	br := bufio.NewReader(in)
	return PrompterFunc(func(ctx context.Context, q string) (string, error) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		_, _ = fmt.Fprintf(out, "agent asks: %s\n> ", q)
		line, err := br.ReadString('\n')
		if err != nil {
			return "", fmt.Errorf("stdin prompter: %w", err)
		}
		return strings.TrimSpace(line), nil
	})
}

// RefusePrompter returns a Prompter that always errors with reason.
// Use in non-interactive runs (batch jobs, CI, daemon deployments
// without a wired-up channel) so the model gets a clear "no user
// available" signal as the ask_user tool result and adapts, rather
// than blocking forever on a missing reader.
//
// Recommended reason text guides the model toward useful behavior:
//
//	tools.RefusePrompter("running unattended; proceed with reasonable defaults and explain in your final response")
func RefusePrompter(reason string) Prompter {
	if reason == "" {
		reason = "no interactive user is connected to this agent run"
	}
	return PrompterFunc(func(_ context.Context, _ string) (string, error) {
		return "", fmt.Errorf("%s", reason)
	})
}

// StaticPrompter returns a Prompter that always returns answer with
// no delay. Test fixture; production code should not use this.
func StaticPrompter(answer string) Prompter {
	return PrompterFunc(func(_ context.Context, _ string) (string, error) {
		return answer, nil
	})
}
