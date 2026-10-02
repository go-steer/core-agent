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
	"strconv"
	"strings"
	"unicode"
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
	if req.ApproverModel != "" {
		return p.askEscalated(ctx, req)
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

// askEscalated is the prompt for a call ModeAuto's approver passed on
// (#1175 decision 11): the approver's reason, quoted as its words on
// one line it cannot break out of, and only once or deny on offer —
// the gate applies any allow here as once anyway, so offering more
// would promise a grant that is never made.
func (p *stdinPrompter) askEscalated(ctx context.Context, req PromptRequest) (Decision, error) {
	if reason := oneTerminalLine(req.ApproverReason); reason != "" {
		_, _ = fmt.Fprintf(p.out, "  passed to you by %s, which said: %q\n", approverName(req.ApproverModel), reason)
	} else {
		_, _ = fmt.Fprintf(p.out, "  passed to you by %s, with no reason given\n", approverName(req.ApproverModel))
	}
	for {
		if err := ctx.Err(); err != nil {
			return DecisionDeny, err
		}
		_, _ = fmt.Fprint(p.out, "[y]es once · [N]o (default): ")
		line, err := p.br.ReadString('\n')
		if err != nil {
			return DecisionDeny, fmt.Errorf("stdin prompter: %w", err)
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "y":
			return DecisionAllowOnce, nil
		case "n", "":
			return DecisionDeny, nil
		default:
			_, _ = fmt.Fprintf(p.out, "unrecognized choice %q; expected y/n\n", strings.TrimSpace(line))
		}
	}
}

// approverName is the approver's model name for the stdin prompt. It
// comes from config, but it is printed as untrusted text all the same:
// %q-quoted when it holds anything %q would escape, such as a bidi
// control, so nothing in it can reorder the line around it.
func approverName(s string) string {
	s = oneTerminalLine(s)
	if q := strconv.Quote(s); q[1:len(q)-1] != s {
		return q
	}
	return s
}

// oneTerminalLine is untrusted text made safe to print as part of one
// terminal line: control characters, which include the escape that
// starts a terminal sequence, are dropped, and every whitespace run
// (newlines included) becomes one space.
func oneTerminalLine(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || r == '\r' {
			return ' '
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	return strings.Join(strings.Fields(s), " ")
}

// promptHeading describes what the gate is asking about in a single
// sentence. Tool name is included even for bash so the prompt is
// self-contained when readers see only one line in their backscroll.
// When req.Source is non-empty (background-subagent originated), the
// heading is prefixed with "[<source>] " so the human knows which
// agent is asking.
func promptHeading(req PromptRequest) string {
	tool := req.ToolName
	if tool == "" {
		tool = "tool"
	}
	var verb string
	switch req.Kind {
	case PromptKindBash:
		verb = "wants to run:"
	case PromptKindFileWrite:
		verb = "wants to write to:"
	case PromptKindPathScope:
		verb = "wants to access an out-of-scope path:"
	case PromptKindControlPlaneWrite:
		verb = "wants to modify a privilege-bearing control-plane file (elevated):"
	default:
		verb = "needs approval:"
	}
	if req.Source != "" {
		return "[" + req.Source + "] " + tool + " " + verb
	}
	return tool + " " + verb
}
