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
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// This file implements the "safe command" analysis that backs
// auto-allow decisions for bash commands (prefix-matched allowlist
// rules and verb-scoped session grants).
//
// A command is "safe" for auto-allow purposes when it is exactly ONE
// simple command with a fully literal argv:
//
//   - No chaining or composition: `;`, `&&`, `||`, pipes, background
//     `&`, subshells, command groups, or negation.
//   - No redirections of any kind (`>`, `>>`, `<`, `2>&1`, heredocs,
//     process substitution used as a redirect target, ...).
//   - Every word is literal: no parameter expansion (`$VAR`), command
//     substitution (`$(...)` / backticks), arithmetic expansion,
//     process substitution, brace/extended expansion, or `$'...'`
//     quoting. Plain single/double quotes around literal text are
//     fine (`find . -name '*.go'` is safe).
//   - Leading KEY=VAL environment assignments are skipped when
//     building the argv (matching extractBashVerb semantics), but
//     their values must be literal too — `FOO=$(evil) cat f` runs the
//     substitution and is therefore not safe.
//
// Anything the parser rejects, and anything outside the shape above,
// is NOT safe — the analysis fails closed. "Not safe" never means
// "deny"; it only means "no auto-allow", so the command falls through
// to the normal mode/prompt flow.

// parseSafeArgv parses command as bash and, when it is exactly one
// simple literal command, returns its argv (leading environment
// assignments skipped) with ok=true. Any parse error, compound or
// chained structure, redirection, or non-literal word yields ok=false
// (fail closed).
func parseSafeArgv(command string) (argv []string, ok bool) {
	parser := syntax.NewParser(syntax.Variant(syntax.LangBash))
	file, err := parser.Parse(strings.NewReader(command), "")
	if err != nil {
		return nil, false
	}
	if len(file.Stmts) != 1 {
		return nil, false
	}
	stmt := file.Stmts[0]
	// Background `cmd &`, coprocesses, `! cmd` negation, and any
	// redirection disqualify the command outright.
	if stmt.Background || stmt.Coprocess || stmt.Negated || len(stmt.Redirs) > 0 {
		return nil, false
	}
	call, isCall := stmt.Cmd.(*syntax.CallExpr)
	if !isCall {
		// BinaryCmd (&&, ||, |), Subshell, Block, IfClause, etc. —
		// all compound forms, none eligible for auto-allow.
		return nil, false
	}
	// Environment assignments are skipped from the argv but must be
	// literal: an expansion in the value executes at assignment time.
	for _, assign := range call.Assigns {
		if assign.Value == nil {
			continue // bare `FOO=` — literal empty
		}
		if _, lit := literalWord(assign.Value); !lit {
			return nil, false
		}
	}
	if len(call.Args) == 0 {
		// Assignment-only statement (`FOO=bar`) — no command to allow.
		return nil, false
	}
	argv = make([]string, 0, len(call.Args))
	for _, word := range call.Args {
		s, lit := literalWord(word)
		if !lit {
			return nil, false
		}
		argv = append(argv, s)
	}
	return argv, true
}

// literalWord flattens w into its literal string value. ok=false when
// any part of the word is an expansion of any kind ($VAR, $(...),
// backticks, arithmetic, process substitution, brace expansion,
// $'...' quoting). Plain single quotes and double quotes wrapping
// literal text are accepted.
func literalWord(w *syntax.Word) (string, bool) {
	var sb strings.Builder
	for _, part := range w.Parts {
		switch p := part.(type) {
		case *syntax.Lit:
			sb.WriteString(p.Value)
		case *syntax.SglQuoted:
			if p.Dollar {
				// $'...' processes escape sequences; treat as
				// non-literal rather than reimplement the decoder.
				return "", false
			}
			sb.WriteString(p.Value)
		case *syntax.DblQuoted:
			for _, inner := range p.Parts {
				lit, isLit := inner.(*syntax.Lit)
				if !isLit {
					return "", false
				}
				sb.WriteString(lit.Value)
			}
		default:
			// ParamExp, CmdSubst, ArithmExp, ProcSubst, ExtGlob,
			// BraceExp, ... — all dynamic.
			return "", false
		}
	}
	return sb.String(), true
}

// verbAutoAllowDenyTokens maps a command verb to the set of argv
// tokens that disable auto-allow for that verb ("verb profiles").
// The canonical example is find: it is read-only by convention, but a
// handful of predicate flags turn it into an exec/delete engine. A
// profile hit does NOT deny the command — it only removes the
// auto-allow, so the command falls through to normal prompting.
//
// Structured as a map so future verbs can add their own profiles
// (e.g. tar's --to-command, rsync's --rsh) without touching the
// matching logic.
var verbAutoAllowDenyTokens = map[string]map[string]struct{}{
	"find": setOf(
		"-exec", "-execdir", "-ok", "-okdir",
		"-delete", "-fls", "-fprint", "-fprint0", "-fprintf",
	),
}

func setOf(items ...string) map[string]struct{} {
	out := make(map[string]struct{}, len(items))
	for _, s := range items {
		out[s] = struct{}{}
	}
	return out
}

// bashSafeForAutoAllow reports whether command may be auto-allowed by
// a prefix-matched allowlist rule: it must be a single simple literal
// command (parseSafeArgv) AND clear its verb's profile, if one exists.
func bashSafeForAutoAllow(command string) bool {
	argv, ok := parseSafeArgv(command)
	if !ok {
		return false
	}
	denied, hasProfile := verbAutoAllowDenyTokens[argv[0]]
	if !hasProfile {
		return true
	}
	for _, tok := range argv[1:] {
		if _, hit := denied[tok]; hit {
			return false
		}
	}
	return true
}
