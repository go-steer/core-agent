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

// Package agentic implements the agentic tool wrappers from
// docs/context-management-design.md Mechanism B. Each wrapper is
// a tool.Tool whose handler internally calls Agent.RunSubtask to
// run the underlying operation (read_file, fetch_url, grep,
// research) in an isolated subagent on a (typically smaller)
// model — only the digest reaches the parent's context.
//
// Why wrap rather than expose the raw tools directly: raw tool
// output is the single largest source of context bloat in long
// sessions. A 5,000-line file read, a 200KB URL fetch, a grep
// hitting hundreds of matches — each one dumps that volume into
// the parent's context window. Wrapping the call in a subtask
// keeps the bloat off the parent: the subtask sees the raw
// output, digests it, returns just the digest.
//
// The package lives at tools/agentic (a sub-package of tools)
// to avoid the import cycle that would result from agentic
// living inside `tools` proper — `agent` imports `tools`, and
// the wrappers need to import `agent` to call RunSubtask.
//
// Inner tools are supplied by the caller via
// AgenticToolOpts.InnerTools. The caller already has the built-
// in tool instances (e.g. read_file, grep) constructed by
// tools.Build(); this design lets the caller pass exactly the
// instances they want — same gate, same caps — without the
// wrapper layer needing to re-build them.

package agentic

import (
	"errors"
	"fmt"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"

	"github.com/go-steer/core-agent/pkg/agent"
	"github.com/go-steer/core-agent/pkg/models"
)

// AgenticToolOpts is shared configuration for every agentic
// wrapper. AgentGetter is the late-binding hook the wrappers
// need to call Agent.RunSubtask — same pattern as
// agent.NewMarkTaskDoneTool, because tool registration happens
// before agent.New returns.
type AgenticToolOpts struct {
	// AgentGetter returns the *Agent the wrapper should call
	// RunSubtask on. Required. Pass a closure that captures the
	// agent pointer the consumer will populate after agent.New
	// returns. A nil return is treated as "registration race
	// not yet complete" and the tool reports a clear error to
	// the model (defensive; shouldn't happen in practice).
	AgentGetter func() *agent.Agent

	// Provider + SmallModelID resolve the subtask's model on
	// first call (and cache it). When SmallModelID is "" (or
	// Provider is nil), the wrapper omits SubtaskSpec.Model and
	// the subtask inherits the parent's model. Set
	// SmallModelID to a flash/haiku-tier model to realize the
	// cost-efficiency win the design doc calls out.
	Provider     models.Provider
	SmallModelID string

	// Budgets override SubtaskBudgetDefaults for this tool's
	// subtasks. Each preset constructor sets a sensible
	// Budgets.MaxTurns default when the caller leaves it zero;
	// override here to raise (or lower) the cap.
	Budgets agent.SubtaskBudgets

	// InnerTools is the set of tools the subtask is allowed to
	// call. Required. Each preset's docstring lists the
	// specific tools the subtask needs to do its job — pass
	// those instances (typically the ones you already
	// registered on the parent agent, so they share the same
	// permission gate and output caps).
	InnerTools []tool.Tool
}

// agenticTool is the shared implementation. Each public preset
// constructor (AgenticReadFile, AgenticFetchURL, AgenticGrep,
// AgenticResearch) calls this with its own description, system
// prompt, and uses opts.InnerTools as the subtask's tool set.
func agenticTool(opts AgenticToolOpts, name, description, systemPrompt string) tool.Tool {
	if opts.AgentGetter == nil {
		panic("agentic: AgenticToolOpts.AgentGetter is required")
	}
	if len(opts.InnerTools) == 0 {
		panic(fmt.Sprintf("agentic.%s: AgenticToolOpts.InnerTools is empty (the subtask needs at least one tool to do its job)", name))
	}
	type agenticArgs struct {
		Request string `json:"request" jsonschema:"the question or instruction the subtask should answer. Be specific — the subtask has no context other than this request and its system instruction."`
	}
	type agenticResult struct {
		Digest    string  `json:"digest"`
		Truncated bool    `json:"truncated,omitempty"`
		TurnsUsed int     `json:"turns_used,omitempty"`
		CostUSD   float64 `json:"cost_usd,omitempty"`
	}

	// Lazily resolve the small model on first call; cache on
	// success. Retry on failure so transient provider outages
	// don't permanently disable the wrapper.
	var resolvedModel adkmodel.LLM
	var resolveErr error
	var resolveDone bool

	handler := func(toolCtx tool.Context, args agenticArgs) (agenticResult, error) {
		a := opts.AgentGetter()
		if a == nil {
			return agenticResult{}, fmt.Errorf("%s: agent not yet bound (registration race?)", name)
		}
		if !resolveDone || resolveErr != nil {
			if opts.Provider != nil && opts.SmallModelID != "" {
				resolvedModel, resolveErr = opts.Provider.Model(toolCtx, opts.SmallModelID)
				resolveDone = true
				if resolveErr != nil {
					return agenticResult{}, fmt.Errorf("%s: resolve small model %q: %w", name, opts.SmallModelID, resolveErr)
				}
			} else {
				// Inherit parent's model — leave resolvedModel
				// nil; RunSubtask falls through to a.model.
				resolveDone = true
			}
		}

		res, err := a.RunSubtask(toolCtx, agent.SubtaskSpec{
			Name:         name,
			SystemPrompt: systemPrompt,
			UserMessage:  args.Request,
			Tools:        opts.InnerTools,
			Model:        resolvedModel,
			Budgets:      opts.Budgets,
		})
		if err != nil {
			if errors.Is(err, agent.ErrSubtaskSpecInvalid) {
				return agenticResult{}, fmt.Errorf("%s: invalid request: %w", name, err)
			}
			return agenticResult{}, fmt.Errorf("%s: %w", name, err)
		}
		return agenticResult{
			Digest:    res.Digest,
			Truncated: res.Truncated,
			TurnsUsed: res.TurnsUsed,
			CostUSD:   res.CostUSD,
		}, nil
	}

	t, err := functiontool.New(functiontool.Config{
		Name:        name,
		Description: description,
	}, handler)
	if err != nil {
		panic(fmt.Sprintf("agentic.%s: %v", name, err))
	}
	return t
}

// --- public presets ---

// AgenticReadFile wraps read_file in a subtask that reads the
// target file and returns a focused excerpt or summary. Use
// instead of the bare read_file when the file might be large and
// the model only needs a specific section — common for config
// files, large source files, or long logs.
//
// Subtask tool requirements (InnerTools): the canonical
// "read_file" tool. Defaults to 2 turns when Budgets.MaxTurns
// is zero (read + summarize, no spiraling).
func AgenticReadFile(opts AgenticToolOpts) tool.Tool {
	if opts.Budgets.MaxTurns == 0 {
		opts.Budgets.MaxTurns = 2
	}
	return agenticTool(opts,
		"agentic_read_file",
		"Read a file and return a focused excerpt or summary. Use INSTEAD OF read_file when the file might be large and you only need a specific section (config files, long source files, build logs). Pass the file path AND what you're looking for as one combined request — e.g. 'read /etc/nginx.conf and tell me what listen ports are configured' or 'read internal/auth/handlers.go and summarize the login flow'. The subtask reads the raw file (only the digest comes back to you), so the file's full content never enters your context window. Don't re-read the same path with bare read_file to spot-check the digest — that re-introduces the raw content you were trying to avoid. If the digest is missing something specific, call agentic_read_file again with a narrower question instead.",
		"You are a focused subtask. Your job: read a file the operator points you at, find what they asked about, and return a concise digest. Use the read_file tool. Quote specific lines when relevant. Stay under 500 words unless the request explicitly asks for more detail. If the request is unclear, return what you found and ask the operator to refine.",
	)
}

// AgenticFetchURL wraps fetch_url in a subtask that fetches the
// URL and returns the relevant section. Use instead of the bare
// fetch_url when the page is likely long (documentation,
// articles, search results) and you only need a specific
// answer. The full fetched HTML/text never enters the parent's
// context.
//
// Subtask tool requirements (InnerTools): the canonical
// "fetch_url" tool. Defaults to 2 turns.
func AgenticFetchURL(opts AgenticToolOpts) tool.Tool {
	if opts.Budgets.MaxTurns == 0 {
		opts.Budgets.MaxTurns = 2
	}
	return agenticTool(opts,
		"agentic_fetch_url",
		"Fetch a URL and return only the relevant section. Use INSTEAD OF fetch_url when the page is likely long (documentation, articles, blog posts, search results) and you only need to answer a specific question from it. Pass the URL AND what you're looking for as one combined request — e.g. 'fetch https://example.com/api/auth and tell me what the bearer-token header format is'. The subtask reads the raw page (only the digest comes back to you), so the full HTML never enters your context window. Don't re-fetch the same URL with bare fetch_url to spot-check the digest — that re-introduces the raw HTML you were trying to avoid. If the digest is missing something specific, call agentic_fetch_url again with a narrower question instead.",
		"You are a focused subtask. Your job: fetch a URL, find what the operator asked about, return a concise digest. Use the fetch_url tool. Quote URLs and key phrases verbatim when relevant. Stay under 500 words. If the page is empty / errors out / doesn't have the answer, say so explicitly rather than padding.",
	)
}

// AgenticGrep wraps grep in a subtask that ranks and summarizes
// matches. Use instead of the bare grep when the pattern is
// likely to hit many matches and you only care about the most
// relevant few. The raw match list never enters the parent's
// context.
//
// Subtask tool requirements (InnerTools): the canonical "grep"
// tool. "read_file" is also supplied so a subtask can pull
// surrounding context when the operator's question genuinely
// demands it, but the default subtask prompt steers AWAY from
// that — see below. Defaults to 2 turns since #60 — the earlier
// 3-turn default invited Flash to do too many tool round trips
// on cross-corpus searches and confabulate file:line content.
// Two turns forces the subtask into grep → digest. Operators
// with rich-context needs can raise via opts.Budgets — at the
// cost of larger hallucination surface on weaker subtask models.
func AgenticGrep(opts AgenticToolOpts) tool.Tool {
	if opts.Budgets.MaxTurns == 0 {
		opts.Budgets.MaxTurns = 2
	}
	return agenticTool(opts,
		"agentic_grep",
		"Search the codebase for a pattern and return the ranked, most-relevant matches with context. Use INSTEAD OF grep when the pattern is likely to hit dozens of matches and you only need the top few (function definitions, config keys, error messages). Pass the pattern AND what you're looking for as one combined request — e.g. 'grep for TODO in internal/ and tell me which ones are about auth' or 'find where http.Server is constructed and which port it binds to'. The subtask runs grep + reads surrounding context (only the digest comes back to you), so the raw match list never enters your context window. Don't re-run bare grep on the same pattern/files to spot-check the digest — that re-introduces the raw match list you were trying to avoid. If the digest is missing something specific, call agentic_grep again with a narrower question instead.",
		"You are a focused subtask. Your job: search the codebase with grep, rank the matches by relevance to what the operator asked, and return a concise digest. Turn budget is tight (2 turns) — plan on a single grep call in turn 1 followed by the digest in turn 2; do NOT chase the matches with read_file unless the operator's question demands it (and even then prefer refusing and recommending a narrower follow-up call). Cite file:line verbatim from grep output for every match — do not paraphrase or invent paths. If you didn't read the file's surrounding lines, do NOT describe what's around the match; report only what grep actually returned. Stay under 500 words. If the pattern doesn't hit anything, say so directly.",
	)
}

// AgenticResearch wraps an open-ended exploration in a subtask
// that can use a broader read-only toolset to investigate a
// question. Use for "understand how X works" / "trace the
// data flow from A to B" / "what's the convention for Y in
// this codebase" — questions that need multiple file reads
// and greps but whose intermediate exploration is noise to
// the parent.
//
// Subtask tool requirements (InnerTools): read_file + grep +
// list_dir + glob (the standard read-only investigation kit).
// Defaults to 5 turns (the design doc's "broad research
// dispatch" budget); raise via opts.Budgets for genuinely
// open-ended questions.
func AgenticResearch(opts AgenticToolOpts) tool.Tool {
	if opts.Budgets.MaxTurns == 0 {
		opts.Budgets.MaxTurns = 5
	}
	return agenticTool(opts,
		"agentic_research",
		"Investigate an open-ended question about the codebase or environment. Use for 'understand how X works', 'trace data flow from A to B', 'what's the convention for Y in this codebase' — questions that need multiple file reads + greps but whose intermediate exploration would clutter your context. Pass the question as the request. The subtask runs many read-only tools (read_file, grep, list_dir, glob) and returns a structured walkthrough with file:line citations (only the walkthrough comes back to you), so the raw tool output never enters your context window. Don't re-read or re-grep the cited locations with bare tools to spot-check the walkthrough — that re-introduces the raw content you were trying to avoid. If the walkthrough is missing something specific, call agentic_research again with a narrower question instead.",
		"You are a focused research subtask. Your job: investigate the operator's question using the read-only tools provided (read_file, grep, list_dir, glob). Be thorough — for an architecture question, find the entry points, follow the data flow, name the key files and functions. Return a structured walkthrough: lead with a one-paragraph headline answer, then file:line citations for anything specific, then optional notes on gotchas / alternative interpretations. Quote sparingly. Stay under 800 words.",
	)
}
