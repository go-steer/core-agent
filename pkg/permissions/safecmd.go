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
	// Environment assignments are skipped from the argv, but their
	// values must be literal: an expansion in the value executes at
	// assignment time.
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

// safeEnvAssignmentKeys is the short list of harmless build/locale
// environment variable names permitted as leading KEY=VAL assignments
// on a command that rides a verb grant or safecmd check (#1356).
var safeEnvAssignmentKeys = setOf(
	"CGO_ENABLED", "GOOS", "GOARCH", "GOAMD64", "GOARM", "GOEXPERIMENT",
	"GO111MODULE", "GOWORK", "TZ", "LANG", "LC_ALL", "LC_CTYPE", "NO_COLOR", "TERM",
)

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

// verbAutoAllowProfiles holds per-verb argument validators for verbs
// whose dangerous forms include attached flag values (--output=FILE,
// -oFILE), top-level flags before a subcommand (git -c), or positional
// output operands (uniq IN OUT) (#1352, #1356).
var verbAutoAllowProfiles = map[string]func(args []string) bool{
	"sort":  sortSafeProfile,
	"tree":  treeSafeProfile,
	"uniq":  uniqSafeProfile,
	"rg":    rgSafeProfile,
	"git":   gitSafeProfile,
	"go":    goSafeProfile,
	"gofmt": gofmtSafeProfile,
}

func setOf(items ...string) map[string]struct{} {
	out := make(map[string]struct{}, len(items))
	for _, s := range items {
		out[s] = struct{}{}
	}
	return out
}

func hasShortFlagRune(arg string, target rune) bool {
	if !strings.HasPrefix(arg, "-") || strings.HasPrefix(arg, "--") || arg == "-" {
		return false
	}
	return strings.ContainsRune(arg[1:], target)
}

func sortSafeProfile(args []string) bool {
	for _, arg := range args {
		if arg == "--" {
			break
		}
		// GNU sort uses getopt_long unambiguous prefix matching:
		// --output is the only --o* long option, and --compress-program
		// is the only --comp* long option.
		if arg == "-o" || strings.HasPrefix(arg, "--o") ||
			strings.HasPrefix(arg, "--comp") ||
			hasShortFlagRune(arg, 'o') {
			return false
		}
	}
	return true
}

func treeSafeProfile(args []string) bool {
	for _, arg := range args {
		if arg == "--" {
			break
		}
		if arg == "-o" || arg == "--output" || strings.HasPrefix(arg, "--output=") ||
			hasShortFlagRune(arg, 'o') {
			return false
		}
	}
	return true
}

func uniqSafeProfile(args []string) bool {
	afterDash := false
	positional := 0
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !afterDash {
			if arg == "--" {
				afterDash = true
				continue
			}
			if arg == "-o" || arg == "--output" || strings.HasPrefix(arg, "--output=") ||
				hasShortFlagRune(arg, 'o') {
				return false
			}
			if arg == "-f" || arg == "-s" || arg == "-w" ||
				arg == "--skip-fields" || arg == "--skip-chars" || arg == "--check-chars" {
				i++ // skip flag value
				continue
			}
			if strings.HasPrefix(arg, "-") && arg != "-" {
				continue
			}
		}
		positional++
		if positional >= 2 {
			return false
		}
	}
	return true
}

func rgSafeProfile(args []string) bool {
	for _, arg := range args {
		if arg == "--" {
			break
		}
		if arg == "--pre" || strings.HasPrefix(arg, "--pre=") ||
			arg == "--pre-glob" || strings.HasPrefix(arg, "--pre-glob=") {
			return false
		}
	}
	return true
}

var gitBranchMutatingFlags = setOf(
	"-d", "-D", "--delete", "--delete-merged",
	"-m", "-M", "--move", "-c", "-C", "--copy",
	"-u", "--set-upstream-to", "--unset-upstream",
	"--edit-description", "--no-list", "-f", "--force",
)

var gitReflogMutatingSubcmds = setOf(
	"delete", "expire", "drop", "write", "exists",
)

func gitSafeProfile(args []string) bool {
	for _, arg := range args {
		if arg == "--output" || strings.HasPrefix(arg, "--output=") ||
			arg == "--ext-diff" ||
			arg == "--config-env" || strings.HasPrefix(arg, "--config-env=") ||
			arg == "--exec-path" || strings.HasPrefix(arg, "--exec-path=") {
			return false
		}
	}
	subIdx := -1
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") {
			subIdx = i
			break
		}
		if arg == "-c" || strings.HasPrefix(arg, "-c") {
			return false
		}
		if arg == "-C" || arg == "--git-dir" || arg == "--work-tree" ||
			arg == "--namespace" || arg == "--super-prefix" || arg == "--attr-source" {
			i++ // consumes directory/path/tree argument before the subcommand
		}
	}
	if subIdx < 0 {
		return true
	}
	switch args[subIdx] {
	case "branch":
		return gitBranchArgsSafe(args[subIdx+1:])
	case "reflog":
		return gitReflogArgsSafe(args[subIdx+1:])
	}
	return true
}

func gitBranchArgsSafe(args []string) bool {
	for _, arg := range args {
		if _, bad := gitBranchMutatingFlags[arg]; bad {
			return false
		}
		if strings.HasPrefix(arg, "--set-upstream-to=") {
			return false
		}
	}
	return true
}

func gitReflogArgsSafe(args []string) bool {
	for _, arg := range args {
		if _, bad := gitReflogMutatingSubcmds[arg]; bad {
			return false
		}
	}
	return true
}

func goSafeProfile(args []string) bool {
	for _, arg := range args {
		if arg == "-vettool" || arg == "--vettool" ||
			strings.HasPrefix(arg, "-vettool=") || strings.HasPrefix(arg, "--vettool=") ||
			arg == "-toolexec" || arg == "--toolexec" ||
			strings.HasPrefix(arg, "-toolexec=") || strings.HasPrefix(arg, "--toolexec=") {
			return false
		}
	}
	sub, subArgs := goSubcommandArgs(args)
	switch sub {
	case "env":
		for _, arg := range subArgs {
			if arg == "-w" || arg == "--w" || strings.HasPrefix(arg, "-w=") || strings.HasPrefix(arg, "--w=") ||
				arg == "-u" || arg == "--u" || strings.HasPrefix(arg, "-u=") || strings.HasPrefix(arg, "--u=") {
				return false
			}
		}
	case "vet":
		for _, arg := range subArgs {
			if arg == "-fix" || arg == "--fix" || strings.HasPrefix(arg, "-fix=") || strings.HasPrefix(arg, "--fix=") {
				return false
			}
		}
	}
	return true
}

func goSubcommandArgs(args []string) (string, []string) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "-C" || arg == "--C" {
			i++
			continue
		}
		if strings.HasPrefix(arg, "-C=") || strings.HasPrefix(arg, "--C=") {
			continue
		}
		if !strings.HasPrefix(arg, "-") {
			return arg, args[i+1:]
		}
	}
	return "", nil
}

// gofmtSafeProfile rejects combining list/diff flags (-l/-d, which are
// in BundleDevTools) with in-place write (-w), while leaving plain
// `gofmt -w ...` eligible for AutoBundleCoding (#1356).
func gofmtSafeProfile(args []string) bool {
	hasListOrDiff := false
	hasWrite := false
	for _, arg := range args {
		if arg == "--" {
			break
		}
		if hasShortFlagRune(arg, 'l') || hasShortFlagRune(arg, 'd') {
			hasListOrDiff = true
		}
		if hasShortFlagRune(arg, 'w') || arg == "--w" {
			hasWrite = true
		}
	}
	return !hasListOrDiff || !hasWrite
}

func hasOnlySafeEnvAssigns(command string) bool {
	parser := syntax.NewParser(syntax.Variant(syntax.LangBash))
	file, err := parser.Parse(strings.NewReader(command), "")
	if err != nil || len(file.Stmts) != 1 {
		return false
	}
	call, ok := file.Stmts[0].Cmd.(*syntax.CallExpr)
	if !ok {
		return false
	}
	for _, assign := range call.Assigns {
		if assign.Name == nil || assign.Index != nil || assign.Append {
			return false
		}
		if _, allowed := safeEnvAssignmentKeys[assign.Name.Value]; !allowed {
			return false
		}
	}
	return true
}

// bashSafeForAutoAllow reports whether command may be auto-allowed by
// a prefix-matched allowlist rule or verb-scoped session grant: it
// must be a single simple literal command (parseSafeArgv), carry only
// harmless leading env assignments (hasOnlySafeEnvAssigns; #1356), AND
// clear its verb's profile, if one exists.
func bashSafeForAutoAllow(command string) bool {
	argv, ok := parseSafeArgv(command)
	if !ok || !hasOnlySafeEnvAssigns(command) {
		return false
	}
	if denied, hasProfile := verbAutoAllowDenyTokens[argv[0]]; hasProfile {
		for _, tok := range argv[1:] {
			if _, hit := denied[tok]; hit {
				return false
			}
		}
	}
	if profile, hasProfile := verbAutoAllowProfiles[argv[0]]; hasProfile {
		if !profile(argv[1:]) {
			return false
		}
	}
	return true
}
