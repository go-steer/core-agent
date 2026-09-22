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

package main

import (
	"path"
	"strings"
)

// binaryVars are the shell variable names that hold a path to the
// core-agent binary in this repo's harness scripts. A reference to one
// of them in command position is an invocation of the agent.
//
// Deliberately a fixed list rather than "any variable whose value looks
// like a path to core-agent": the scanner reads source, it does not
// evaluate it, so there is no value to look at. A harness that invents a
// new name for the binary has to add it here, and the Invocations() dump
// (--print) is how you notice that a script's sites went missing.
var binaryVars = map[string]bool{
	"CORE_AGENT": true,
	"BIN":        true,
	"AGENT_BIN":  true,
}

// commandPrefixes are words that precede the real command word without
// being it. After one of these the next word is still in command
// position.
var commandPrefixes = map[string]bool{
	"exec": true, "env": true, "sudo": true, "nohup": true,
	"time": true, "command": true, "builtin": true, "!": true,
	// Shell keywords that introduce a command rather than being one.
	"if": true, "then": true, "else": true, "elif": true,
	"do": true, "while": true, "until": true, "fi": true,
	"done": true, "esac": true,
	// Brace groups and one-line function bodies. On its own line the
	// newline restores command position anyway, but the one-line forms
	// — `{ "${CORE_AGENT}" -p hi; }` and
	// `run() { "${CORE_AGENT}" "$@"; }` — are idiomatic and would
	// otherwise put the binary in argument position.
	"{": true, "}": true,
}

// wrapperCommands run the command named in their own arguments. After
// one of these, the flags, the bare numbers and the variable references
// belonging to the wrapper are skipped and the next ordinary word is
// back in command position.
//
// Variable references have to be skipped because a wrapper's own
// argument is routinely parameterised — `timeout "${TIMEOUT_SECS}"
// "${BIN}"` is the idiomatic form, and isDuration cannot see a duration
// through the variable. Skipping stops at a known binary reference, so
// the word the wrapper is going to run is never swallowed. The cost of
// being wrong here is asymmetric in the safe direction: skipping one
// word too many can only turn a site the scanner already misses into a
// site it still misses, whereas *not* skipping loses command position
// altogether and makes the invocation invisible to --print's census as
// well as to the check.
var wrapperCommands = map[string]bool{
	"timeout": true, "nice": true, "ionice": true, "stdbuf": true,
	"setsid": true, "chrt": true,
}

// Finding is one invocation of the agent binary that pins no config.
type Finding struct {
	File string
	Line int
	Text string // the command word as written
	Args string // the rest of the invocation, for the report
}

// Invocation is any call site the scanner recognised, pinned or not.
// --print dumps these so a reviewer can check the scanner still sees
// every site it used to; a site that silently stops being recognised is
// the failure mode a violation scanner cannot report on its own.
type Invocation struct {
	File   string
	Line   int
	Text   string
	Args   string
	Pinned bool
	Exempt string // non-empty when the site needs no pin, saying why
}

// Scan reports every agent invocation in one shell script.
//
// SOUNDNESS DIRECTION. This is a violation scanner: it fails when it
// *finds* something. The dangerous error is therefore a miss, not a
// false positive — a miss means the gate passes silently forever, while
// a false positive is loud and gets fixed in a minute. Every judgement
// call below is resolved towards "treat it as code":
//
//   - Quoted spans are scanned as code, not blanked. The harness
//     dispatches whole command lines as strings through tmux
//     (dev/uat/attach/run.sh), so a double-quoted span is exactly where
//     a real invocation hides. This is why the scanner cannot reuse
//     shellCode() from examples/gke-platform-agent/recipe_test.go: that
//     one blanks quoted spans, which is right for its check and fatal
//     for this one.
//   - Heredoc bodies are scanned too, as their own document, rather
//     than skipped.
//   - A quote that never closes swallows the rest of the file into one
//     span, which is then still scanned.
//
// The two deliberate exceptions are `#` comments, which the shell never
// executes, and quoted spans with no whitespace in them, which cannot
// hold a command *and its arguments* and are overwhelmingly a bare
// `"${VAR}"` that the outer pass has already classified.
func Scan(file, src string) []Invocation {
	var out []Invocation
	toks, heredocs := lexShell(src, 1)
	analyze(file, toks, &out)
	// Every quoted span and command substitution, as its own document.
	for _, t := range toks {
		for _, s := range t.spans {
			scanNested(file, s, &out)
		}
	}
	for _, h := range heredocs {
		scanNested(file, h, &out)
	}
	return out
}

func scanNested(file string, s span, out *[]Invocation) {
	// A span with no whitespace holds at most a bare word. It cannot be
	// a command with arguments, and the common case — "${CORE_AGENT}",
	// or "${CORE_AGENT:-/tmp/core-agent}" on the right of an assignment
	// — is already classified correctly by the enclosing pass. Scanning
	// it again would report the *argument* "${CORE_AGENT}" in
	// `go build -o "${CORE_AGENT}"` as a command.
	if strings.IndexFunc(s.text, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n'
	}) < 0 {
		return
	}
	analyze(file, lexPlain(s.text, s.line), out)
}

// analyze walks a token stream and reports the binary references that
// stand in command position.
func analyze(file string, toks []tok, out *[]Invocation) {
	atStart := true
	inWrapperArgs := false
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		if t.sep {
			atStart = true
			inWrapperArgs = false
			continue
		}
		if atStart {
			bare := unquote(t.text)
			if isAssignment(bare) {
				continue // NAME=value prefix; still command position
			}
			if commandPrefixes[path.Base(bare)] {
				continue
			}
			if wrapperCommands[path.Base(bare)] {
				inWrapperArgs = true
				continue
			}
			if inWrapperArgs && !isBinaryRef(bare) &&
				(strings.HasPrefix(bare, "-") || isDuration(bare) || isVarRef(bare)) {
				continue // the wrapper's own flags, duration and variables
			}
			// `go run ./cmd/core-agent …` builds and runs the same
			// binary, and inherits config discovery from the same cwd.
			if path.Base(bare) == "go" && isGoRunOfAgent(toks, i) {
				*out = append(*out, classify(file, toks, i+2))
				atStart = false
				inWrapperArgs = false
				continue
			}
			if isBinaryRef(bare) {
				*out = append(*out, classify(file, toks, i))
				atStart = false
				inWrapperArgs = false
				continue
			}
		}
		atStart = false
		inWrapperArgs = false
	}
}

// isGoRunOfAgent reports whether toks[i] ("go") begins a
// `go run <pkg>` whose package is cmd/core-agent.
func isGoRunOfAgent(toks []tok, i int) bool {
	if i+2 >= len(toks) || toks[i+1].sep || toks[i+2].sep {
		return false
	}
	if unquote(toks[i+1].text) != "run" {
		return false
	}
	pkg := strings.TrimSuffix(unquote(toks[i+2].text), "/")
	return path.Base(pkg) == "core-agent"
}

// classify walks the rest of the simple command looking for the pin.
//
// SOUNDNESS DIRECTION, INVERTED. Finding an invocation resolves every
// ambiguity towards "this is code", because a missed invocation is a
// silent pass. Deciding that an invocation is *pinned* is the same
// question asked from the other end: a `-c` credited in error is also a
// silent pass. So this half resolves every ambiguity towards "not a
// pin", and the two biases meet in the middle.
//
// Two rules carry it, and both matter only for nested content, where
// lexPlain splits on whitespace and an argument's interior words become
// tokens of their own:
//
//   - A `-c` that a best-effort quote scan places inside a quote is not
//     a pin. Otherwise `-p 'explain the -c flag'` inside a tmux command
//     string marks the site pinned forever — in exactly the two sites
//     this check was built for.
//   - The value has to look like a config file: a path ending in .json,
//     or something with a variable in it. This is what stops a `-c` that
//     is really the *value* of the flag before it (`--log-file -c`), or
//     a bare `-c` at the end of a line, from counting.
func classify(file string, toks []tok, i int) Invocation {
	inv := Invocation{File: file, Line: toks[i].line, Text: toks[i].text}
	var args []string
	for j := i + 1; j < len(toks) && !toks[j].sep; j++ {
		a := unquote(toks[j].text)
		args = append(args, toks[j].text)
		// `attach` and `ls` are peeled off in main() before flag.Parse
		// and never read a config; they reject -c outright. --version
		// short-circuits for the same reason. See cmd/core-agent/main.go.
		if j == i+1 && (a == "attach" || a == "ls") {
			inv.Exempt = a + " subcommand: dispatched before flag.Parse, reads no config"
			break
		}
		if a == "--version" || a == "-version" {
			inv.Exempt = "--version: short-circuits before flag.Parse"
			break
		}
		if toks[j].quoted {
			continue // a word of an argument, not a flag of this command
		}
		if v, ok := strings.CutPrefix(a, "-c="); ok {
			if isConfigValue(v) {
				inv.Pinned = true
				break
			}
			continue
		}
		if v, ok := strings.CutPrefix(a, "--c="); ok {
			if isConfigValue(v) {
				inv.Pinned = true
				break
			}
			continue
		}
		if a == "-c" || a == "--c" {
			if j+1 < len(toks) && !toks[j+1].sep && isConfigValue(unquote(toks[j+1].text)) {
				inv.Pinned = true
				break
			}
			continue
		}
	}
	inv.Args = strings.Join(args, " ")
	if len(inv.Args) > 90 {
		inv.Args = inv.Args[:90] + "…"
	}
	return inv
}

// isBinaryRef reports whether a bare (unquoted) word names the agent
// binary.
//
// Two shapes count: a reference to one of binaryVars, and an explicit
// path whose base is core-agent. The path form is deliberately narrow —
// it must start with /, ./ or ../ — because prose says things like
// "deploy/core-agent in ${NS} has no ready replica", and a scanner that
// called that an invocation would cry wolf on every drill script.
func isBinaryRef(w string) bool {
	if strings.HasPrefix(w, "$") {
		name := strings.TrimPrefix(w, "$")
		name = strings.TrimPrefix(name, "{")
		name = strings.TrimSuffix(name, "}")
		// ${VAR:-default}, ${VAR##x} and friends: take the name only.
		if k := strings.IndexAny(name, ":-#%/^,@"); k >= 0 {
			name = name[:k]
		}
		return binaryVars[name]
	}
	if strings.HasPrefix(w, "/") || strings.HasPrefix(w, "./") || strings.HasPrefix(w, "../") {
		return path.Base(w) == "core-agent"
	}
	return false
}

// isConfigValue reports whether a word can be the config path handed to
// -c. Deliberately narrow, because this is the pin side: anything it
// rejects becomes a loud finding on a site that may well be fine, and
// anything it accepts in error is a silent pass. A real pin names a
// .json file or builds the path out of a variable; a flag, an empty
// word, or ordinary prose does neither.
func isConfigValue(w string) bool {
	if w == "" || strings.HasPrefix(w, "-") {
		return false
	}
	return strings.HasSuffix(w, ".json") || strings.Contains(w, "$")
}

func isAssignment(w string) bool {
	k := strings.IndexByte(w, '=')
	if k <= 0 {
		return false
	}
	for i := 0; i < k; i++ {
		c := w[i]
		ok := c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(i > 0 && c >= '0' && c <= '9')
		if !ok {
			return false
		}
	}
	return true
}

// isVarRef reports whether a word is nothing but a parameter expansion.
// A word that merely contains one (`--timeout=${N}`, `${DIR}/bin/x`) is
// not one: only a whole-word reference can be a wrapper's own argument
// standing in for a literal.
func isVarRef(w string) bool {
	if strings.HasPrefix(w, "${") && strings.HasSuffix(w, "}") {
		return !strings.ContainsAny(w[2:len(w)-1], "${}")
	}
	// The unbraced spelling is at least as idiomatic, and isBinaryRef
	// already handles it on the other side. Missing it here left
	// `timeout $SECS "${BIN}"` invisible to the check AND to --print's
	// census, which is the exact failure the braced case was fixed for.
	if !strings.HasPrefix(w, "$") || len(w) < 2 {
		return false
	}
	for _, r := range w[1:] {
		alnum := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
		if r != '_' && !alnum {
			return false
		}
	}
	return true
}

func isDuration(w string) bool {
	if w == "" {
		return false
	}
	body := strings.TrimRight(w, "smhd")
	if body == "" {
		return false
	}
	for _, r := range body {
		if (r < '0' || r > '9') && r != '.' {
			return false
		}
	}
	return true
}

// unquote strips the quoting a word carries as written. Backslashes go
// too: a command string built for tmux or `bash -c` escapes its inner
// quotes, so the binary reference arrives here as `\"${BIN}\"` and a trim
// of quotes alone leaves the backslash and misses the invocation
// entirely. That is the silent-miss direction, so the trim is wide.
func unquote(w string) string {
	return strings.Trim(w, `"'\`)
}
