// Copyright 2026 The Cogo Authors.
// SPDX-License-Identifier: Apache-2.0

package testutil

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"

	"google.golang.org/adk/tool"
	"google.golang.org/genai"
)

// Tool descriptions and arg schemas are system-prompt-weight text that
// nothing reviews: AGENTS.md gets careful scrutiny, a `Description:`
// string in a Go file gets read as code. They outrank the persona at
// the point where the model decides what to call, and a recipe author
// can neither see nor override them (#909, #910).
//
// core-agent began as an interactive coding agent and now also runs as
// a headless, long-lived daemon consuming machine signals, where "after
// shipping a feature" and "the codebase" describe nothing that happens.
// mark_task_done is the confirmed instance where that frame broke a live
// deployment: its "use this generously at natural task boundaries" beat
// a persona that explicitly forbade the behaviour, and its
// "one-paragraph completion summary" ARG SCHEMA — not the description —
// shaped what the model actually wrote (#905).
//
// Two rules, in the order they matter:
//
//  1. A string-typed argument on a model-facing tool is a WRITING
//     PROMPT. It must name what the text has to CONTAIN (findings,
//     evidence, the proposed change), never what GENRE of document it
//     is. A genre name carries a rhetorical mode with it — "summary"
//     implies retrospection, "report" implies an audience, "status"
//     implies a state machine — and the model honours that mode over
//     the persona, because satisfying the schema is a precondition for
//     the call succeeding at all.
//
//  2. Branch when the branch changes what is TRUE about this build
//     (whenTool, gate.HasTool, sciontoolOnPath); delete when it only
//     changes what is TYPICAL. There is no interactive/headless bit to
//     switch on — the session that motivated #909 was a headless daemon
//     with an operator attached over the TUI, i.e. both at once — so a
//     mode-varying description would be computed from a fact that can
//     change after it is baked. One neutral string per tool.
//
// This list is the only thing that stops re-drift, because the review
// gate reads a changed description as a changed line of code. Precedent:
// markTaskDoneRepeatStatus is already a constant asserted by a test
// because the content IS the contract.
//
// It lives here rather than beside any one catalog because the five
// packages that register model-facing tools — pkg/tools,
// pkg/tools/agentic, pkg/agent/background, pkg/agent/autonomous and
// pkg/agent — must not drift from each other. Two return tools
// disagreeing about whether a status line is acceptable is exactly the
// defect the audit found.
//
// #909 shipped saying four, and pkg/agent was the missing one (#919) —
// which meant mark_task_done, the tool whose description started this
// whole thread, was the single tool with no re-drift guard. If you add
// a package that registers a tool a model can call, it gets a
// description_neutrality_test.go and this sentence gets a new name in
// it. The count is load-bearing: it is the only record of what "every
// registered tool" is being claimed over.
//
// Matching is on lowercased text, so entries here must be lowercase.
var ModelFacingBans = []struct {
	Phrase string
	Why    string
}{
	{"generously", "a frequency instruction the persona cannot see or countermand (#905)"},
	{"the codebase", "assumes a repository; many deployments have none"},
	{"shipping a feature", "assumes a code workload"},
	{"code review", "assumes a code workload"},
	{"code search", "assumes a code workload; say what the tool does — search file contents"},
	{"code investigation", "assumes a code workload; say what the tool does — read files, search them, list directories"},
	{"debugging session", "assumes a code workload"},
	{"source file", "assumes a code workload; a file is a file"},
	{"completion summary", "names a genre, not a content obligation"},
	{"status update", "names a genre, not a content obligation"},
	{"one-paragraph", "prescribes a document shape the caller's AGENTS.md should pick"},
	{"one-sentence detail", "names a genre and a length, not a content obligation"},
}

// ModelFacingBanViolations returns one message per banned phrase found
// in text. Empty means clean.
func ModelFacingBanViolations(text string) []string {
	lower := strings.ToLower(text)
	var out []string
	for _, ban := range ModelFacingBans {
		if strings.Contains(lower, ban.Phrase) {
			out = append(out, "contains "+strconv.Quote(ban.Phrase)+" — "+ban.Why)
		}
	}
	return out
}

// declarer is the shape every ADK functiontool satisfies. Asserted for
// rather than requiring a hand-maintained list of arg structs, so a new
// tool is covered the day it is registered — which is the whole point.
type declarer interface {
	Declaration() *genai.FunctionDeclaration
}

// ModelFacingText returns every string a tool puts in front of a model:
// its description plus its whole arg schema. The second return is false
// when the tool does not expose a declaration, i.e. its arg schema went
// unscanned and the caller should say so rather than pass quietly.
//
// The schema goes in as marshalled JSON rather than a typed walk.
// functiontool populates ParametersJsonSchema (an `any` holding a
// jsonschema document), not the typed genai.Schema, so there is no
// struct to walk — and scanning the serialized form is what keeps this
// robust to a nested object or an array-of-objects arg growing a
// description later, which a hand-written walk would silently miss.
func ModelFacingText(tl tool.Tool) (texts []string, scannedSchema bool) {
	out := []string{tl.Description()}
	d, ok := tl.(declarer)
	if !ok {
		return out, false
	}
	decl := d.Declaration()
	if decl == nil {
		return out, false
	}
	if decl.Description != "" {
		out = append(out, decl.Description)
	}
	if decl.ParametersJsonSchema == nil {
		// A tool that genuinely takes no arguments. Nothing to scan and
		// nothing wrong with that.
		return out, true
	}
	raw, err := json.Marshal(decl.ParametersJsonSchema)
	if err != nil {
		return out, false
	}
	return append(out, string(raw)), true
}

// argRefPattern matches the two ways a description tells a model to
// populate a named argument: an assignment (`state="done"`) and a
// reference (`the result argument`, "`detail` argument").
//
// Deliberately narrow. A description can also refer to an argument in
// prose no pattern will catch ("put your findings in detail"), so a
// clean result is not proof the description is consistent with the
// schema — it only rules out the two forms specific enough to be worth
// obeying literally, which are the forms a model does obey literally.
var argRefPattern = regexp.MustCompile("`?([a-z_][a-z0-9_]*)`?(?:\\s*=\\s*\"|\\s+argument\\b)")

// UndeclaredArgRefs returns the argument names a tool's own description
// tells the model to populate that the tool does not actually declare.
//
// This exists because #909's first draft rewrote the autonomous loop's
// default report_done description to say "put your actual findings in
// the result argument" — and that branch builds a
// coretools.NewLifecycleTool, whose arguments are {state, detail}. ADK
// validates function args with additionalProperties:false, so a model
// that obeyed would emit a hard validation error and the run would
// never receive its done signal. The two return tools in this repo take
// different argument names, and a sweep that only reads prose cannot
// tell that apart from a wording change.
//
// The second return is false when the tool exposes no declaration or no
// arg schema, i.e. nothing was checked.
func UndeclaredArgRefs(tl tool.Tool) (refs []string, checked bool) {
	declared, ok := declaredArgs(tl)
	if !ok {
		return nil, false
	}
	seen := map[string]bool{}
	for _, m := range argRefPattern.FindAllStringSubmatch(tl.Description(), -1) {
		name := m[1]
		if declared[name] || seen[name] {
			continue
		}
		seen[name] = true
		refs = append(refs, name)
	}
	return refs, true
}

// declaredArgs returns the top-level property names of a tool's arg
// schema.
func declaredArgs(tl tool.Tool) (map[string]bool, bool) {
	d, ok := tl.(declarer)
	if !ok {
		return nil, false
	}
	decl := d.Declaration()
	if decl == nil || decl.ParametersJsonSchema == nil {
		return nil, false
	}
	raw, err := json.Marshal(decl.ParametersJsonSchema)
	if err != nil {
		return nil, false
	}
	var doc struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil || doc.Properties == nil {
		return nil, false
	}
	out := make(map[string]bool, len(doc.Properties))
	for name := range doc.Properties {
		out[name] = true
	}
	return out, true
}
