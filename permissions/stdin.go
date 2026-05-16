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

package permissions

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"
)

// StdinPrompter returns a Prompter that renders permission requests
// to out and reads a single-character decision key from in. Suitable
// for interactive CLI use; callers should gate construction on
// runner.IsTerminal(in) so headless invocations fall back to
// ErrNoPrompter instead of blocking on stdin.
//
// Decision keys (case-insensitive, one character followed by newline):
//
//	y         allow once
//	s         allow this exact request for the rest of the session
//	t         allow every call to this tool for the rest of the session
//	a         allow always (persist to the project's config allowlist)
//	n / empty deny
//
// Invalid input reprompts. EOF or context cancellation returns the
// underlying error so the gate can surface a denial with context.
func StdinPrompter(in io.Reader, out io.Writer) Prompter {
	return &stdinPrompter{br: bufio.NewReader(in), out: out}
}

type stdinPrompter struct {
	br  *bufio.Reader
	out io.Writer
}

func (p *stdinPrompter) AskApproval(ctx context.Context, req PromptRequest) (Decision, error) {
	if err := ctx.Err(); err != nil {
		return DecisionDeny, err
	}
	heading := promptHeading(req)
	_, _ = fmt.Fprintln(p.out)
	_, _ = fmt.Fprintf(p.out, "core-agent (permissions): %s\n", heading)
	if req.Detail != "" {
		_, _ = fmt.Fprintf(p.out, "  %s\n", req.Detail)
	}
	for {
		if err := ctx.Err(); err != nil {
			return DecisionDeny, err
		}
		_, _ = fmt.Fprint(p.out, "[y]es once · [s]ession · session-[t]ool · [a]lways · [N]o (default): ")
		line, err := p.br.ReadString('\n')
		if err != nil {
			return DecisionDeny, fmt.Errorf("stdin prompter: %w", err)
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "y":
			return DecisionAllowOnce, nil
		case "s":
			return DecisionAllowSession, nil
		case "t":
			return DecisionAllowSessionTool, nil
		case "a":
			return DecisionAllowAlways, nil
		case "n", "":
			return DecisionDeny, nil
		default:
			_, _ = fmt.Fprintf(p.out, "unrecognized choice %q; expected y/s/t/a/n\n", strings.TrimSpace(line))
		}
	}
}

// promptHeading describes what the gate is asking about in a single
// sentence. Tool name is included even for bash so the prompt is
// self-contained when readers see only one line in their backscroll.
func promptHeading(req PromptRequest) string {
	tool := req.ToolName
	if tool == "" {
		tool = "tool"
	}
	switch req.Kind {
	case PromptKindBash:
		return tool + " wants to run:"
	case PromptKindFileWrite:
		return tool + " wants to write to:"
	case PromptKindPathScope:
		return tool + " wants to access an out-of-scope path:"
	default:
		return tool + " needs approval:"
	}
}
