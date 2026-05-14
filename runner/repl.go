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

package runner

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"

	adkmodel "google.golang.org/adk/model"

	"github.com/go-steer/core-agent/agent"
	"github.com/go-steer/core-agent/usage"
)

// REPL drives a multi-turn stdin loop against m. Each line is sent
// through the same Agent so the ADK runner accumulates conversation
// history across turns (the in-memory session service appends events
// per session ID, which the Agent reuses).
//
// Prompts are written to stdout, partial assistant text streams back
// inline, and tool-call summaries go to stderr — same shape as
// Headless. Built-in commands: `/exit`, `/quit`, EOF (Ctrl-D).
//
// agentOpts mirrors Headless. The same tracker/pricing pair is used
// across every turn so the final summary is meaningful.
func REPL(ctx context.Context, m adkmodel.LLM, stdin io.Reader, stdout, stderr io.Writer, tracker *usage.Tracker, pricing usage.Pricing, agentOpts ...agent.Option) (int, error) {
	a, err := agent.New(m, agentOpts...)
	if err != nil {
		return ExitAgentError, err
	}

	br := bufio.NewReader(stdin)
	fmt.Fprintln(stderr, "core-agent REPL — /exit or Ctrl-D to quit")

	for {
		if err := ctx.Err(); err != nil {
			return ExitOK, nil
		}
		fmt.Fprint(stdout, "> ")
		line, err := br.ReadString('\n')
		if err == io.EOF {
			fmt.Fprintln(stdout)
			return ExitOK, nil
		}
		if err != nil {
			return ExitAgentError, fmt.Errorf("runner: read stdin: %w", err)
		}
		prompt := strings.TrimSpace(line)
		if prompt == "" {
			continue
		}
		if prompt == "/exit" || prompt == "/quit" {
			return ExitOK, nil
		}
		code, _, err := streamTurn(ctx, a, m, prompt, stdout, stderr, tracker, pricing)
		if err != nil {
			fmt.Fprintf(stderr, "core-agent: %v\n", err)
			// Don't exit on a single turn error — let the user retry.
			continue
		}
		if code != ExitOK {
			return code, nil
		}
	}
}
