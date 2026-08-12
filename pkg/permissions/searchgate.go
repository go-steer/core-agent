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
	"fmt"
	"path"
	"sort"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// This file implements the bash search gate (#158): a refusal aimed at
// one specific model reflex, `bash grep` when a native `grep` tool is
// already registered.
//
// # Why a gate and not a better tool description
//
// The descriptions already say it. The native grep's reads "PREFERRED
// over `bash grep`/`bash rg`/`bash find` for code search"; bash's own
// description opens by pointing at the structured tools. Measured
// result, from docs/gemini-tier1-followup-plan.md: a Gemini variant
// picked bash for search 15 times out of 27 anyway. One observed
// session opened with three bash-as-grep calls in its first four tool
// calls, then ran 164 turns and $5.41 without producing a diagnosis.
//
// A description is advisory and read once, at registration. It gives
// the model no signal at the moment it makes the wrong choice — a
// `grep -rn` that returns nothing looks like "no matches", not like "you
// used the wrong tool", so the model tries another bash variant. A
// refusal is in-context, immediate, and can name the replacement.
//
// # This is steering, not security
//
// The gate refuses a shape a model reaches for by reflex; it is not
// trying to stop anyone who wants to run grep. It fails OPEN on
// anything it cannot parse or cannot read literally (`$TOOL -rn foo`,
// `eval`, a wrapper script), because a false refusal on a legitimate
// build command costs more than a missed nudge. The security boundary
// for bash is the permission mode and the allowlist, not this.
//
// # What counts as "search-shaped"
//
// A search binary in a command position whose stdin is NOT a pipe.
// The distinction is the whole design:
//
//	grep -rn "foo" .            → refused; this is a tree search
//	go test ./... | grep -v ok  → allowed; grep is filtering a stream
//
// The native grep tool can do the first and cannot do the second, so
// refusing the second would be refusing something with no replacement.
// Piped-into commands are therefore left alone at any depth.
//
// `find` gets one more carve-out: with an action predicate (-delete,
// -exec, …) it is not a search, it is a file operation with no native
// equivalent, so it passes. That set is verbAutoAllowDenyTokens["find"]
// — the same list safecmd.go already maintains for exactly the reason
// that those flags make find something other than a lookup.

// searchBinaries maps a search-shaped binary to the native tool that
// replaces it. Keys are compared against the command verb's basename,
// so /usr/bin/grep and grep are the same entry.
//
// egrep/fgrep/rgrep are grep aliases; including them costs nothing and
// leaving them out would make the gate trivially sidesteppable by a
// model that isn't even trying to sidestep it.
var searchBinaries = map[string]string{
	"grep":  "grep",
	"egrep": "grep",
	"fgrep": "grep",
	"rgrep": "grep",
	"rg":    "grep",
	"ag":    "grep",
	"ack":   "grep",
	"fd":    "glob",
	"find":  "glob",
}

// SearchShapedCommand reports the first search-shaped binary in
// command, along with the native tool that replaces it. ok is false
// when nothing matched — including every case the parser could not
// resolve, because this check fails open (see the file doc).
func SearchShapedCommand(command string) (binary, native string, ok bool) {
	parser := syntax.NewParser(syntax.Variant(syntax.LangBash))
	file, err := parser.Parse(strings.NewReader(command), "")
	if err != nil {
		return "", "", false
	}
	for _, stmt := range file.Stmts {
		if b, n, hit := searchInStmt(stmt, false); hit {
			return b, n, true
		}
	}
	return "", "", false
}

// searchInStmt walks one statement. pipedInput is true when this
// statement's stdin comes from a pipe, which makes a search binary a
// stream filter rather than a tree search — the one shape the native
// tool cannot replace.
func searchInStmt(stmt *syntax.Stmt, pipedInput bool) (binary, native string, ok bool) {
	if stmt == nil {
		return "", "", false
	}
	switch cmd := stmt.Cmd.(type) {
	case *syntax.CallExpr:
		if pipedInput {
			return "", "", false
		}
		return searchInCall(cmd)
	case *syntax.BinaryCmd:
		// `|` and `|&` mark the right side as reading a stream. `&&`
		// and `||` are plain sequencing: both sides keep the caller's
		// stdin, so `make && grep -rn foo .` still refuses on the grep.
		rightPiped := pipedInput
		if cmd.Op == syntax.Pipe || cmd.Op == syntax.PipeAll {
			rightPiped = true
		}
		if b, n, hit := searchInStmt(cmd.X, pipedInput); hit {
			return b, n, true
		}
		return searchInStmt(cmd.Y, rightPiped)
	case *syntax.Subshell:
		return searchInStmts(cmd.Stmts, pipedInput)
	case *syntax.Block:
		return searchInStmts(cmd.Stmts, pipedInput)
	}
	// IfClause, ForClause, WhileClause, CaseClause, FuncDecl, ... — a
	// model writing a shell loop to grep is not the reflex this gate
	// exists to interrupt, and recursing into every construct buys
	// coverage of shapes nobody has observed at the cost of surface.
	return "", "", false
}

func searchInStmts(stmts []*syntax.Stmt, pipedInput bool) (binary, native string, ok bool) {
	for _, s := range stmts {
		if b, n, hit := searchInStmt(s, pipedInput); hit {
			return b, n, true
		}
	}
	return "", "", false
}

func searchInCall(call *syntax.CallExpr) (binary, native string, ok bool) {
	if len(call.Args) == 0 {
		return "", "", false
	}
	verb, lit := literalWord(call.Args[0])
	if !lit {
		// `$TOOL -rn foo` — unresolvable without running it. Fail open.
		return "", "", false
	}
	base := path.Base(verb)
	nativeTool, isSearch := searchBinaries[base]
	if !isSearch {
		return "", "", false
	}
	if base == "find" && findHasAction(call.Args[1:]) {
		// `find . -name '*.tmp' -delete` is a file operation, not a
		// lookup, and glob cannot delete anything.
		return "", "", false
	}
	return base, nativeTool, true
}

// findHasAction reports whether a find invocation carries a predicate
// that makes it act rather than report.
func findHasAction(args []*syntax.Word) bool {
	actions := verbAutoAllowDenyTokens["find"]
	for _, w := range args {
		tok, lit := literalWord(w)
		if !lit {
			continue
		}
		if _, hit := actions[tok]; hit {
			return true
		}
	}
	return false
}

// SearchGateMessage is the refusal (and, in warn mode, the notice) the
// model sees. It names the shape refused, the tool to use instead, why
// that tool is better, and how an operator turns the gate off — a
// refusal the model can't act on is just a dead end.
func SearchGateMessage(binary, native string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "`%s` is a search-shaped command; use the native `%s` tool instead. ", binary, native)
	switch native {
	case "grep":
		b.WriteString("It returns structured {path, line, text} matches, honors the permission gate " +
			"and the path scope, and applies per-tool output caps. ")
	case "glob":
		b.WriteString("`glob` matches path patterns (and `list_dir` walks a directory), " +
			"honoring the permission gate and the path scope. ")
	}
	b.WriteString("Piping into a search binary is unaffected — `go test ./... | grep -v ok` filters a " +
		"stream, which the native tool does not do. ")
	b.WriteString("Operator override: safety.bash_search_gate = \"warn\" or \"allow\" (CLI: --bash-search-gate).")
	return b.String()
}

// SearchGatedBinaries lists the gated binaries, sorted. For docs and
// operator-facing output.
func SearchGatedBinaries() []string {
	out := make([]string, 0, len(searchBinaries))
	for name := range searchBinaries {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
