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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The oracle. Every case below is both scanned and RUN, with the binary
// replaced by a stub that prints a sentinel, and the two counts must
// agree. A hand-rolled shell lexer whose soundness is argued in a doc
// comment is not sound; the previous one in this repo (#1105) was
// falsified within the hour by `cat <<""`. So the argument is a test.
//
// The invariant is command-position count, not violation count: an
// `attach` site is exempt from needing a pin but still executes, so it
// counts on both sides.
type oracleCase struct {
	name string
	// script runs with CORE_AGENT/BIN/AGENT_BIN pointing at the stub and
	// STUB holding the same path.
	script string
	// divergence, when non-empty, records a known and accepted
	// disagreement with bash and explains why it is tolerable. The test
	// then asserts the divergence still exists, so that fixing the lexer
	// makes this list shrink loudly rather than rot.
	divergence string
	// wantScan is only consulted for divergent cases.
	wantScan int
}

var oracleCases = []oracleCase{
	// --- executes, and the scanner must see it ---
	{name: "plain", script: `"${CORE_AGENT}" -p hi`},
	{name: "timeout wrapper", script: `timeout 5 "${CORE_AGENT}" -p hi`},
	{name: "timeout with flags", script: `timeout --signal=INT 5s "${CORE_AGENT}" -p hi`},
	{name: "after cd &&", script: `cd /tmp && "${CORE_AGENT}" -p hi`},
	{name: "env assignment prefix", script: `FOO=1 BAR=2 "${CORE_AGENT}" -p hi`},
	{name: "exec in subshell", script: `( exec "${CORE_AGENT}" -p hi )`},
	{name: "backgrounded", script: `"${CORE_AGENT}" -p hi &` + "\nwait"},
	{name: "backslash continuation", script: `"${CORE_AGENT}" \` + "\n" + `  -p hi`},
	{name: "absolute literal path", script: `"${STUB}" -p hi`},
	{name: "pipeline tail", script: `echo x | "${CORE_AGENT}" -p hi`},
	{name: "inside if", script: `if "${CORE_AGENT}" -p hi; then :; fi`},
	// stdout stays attached on purpose: redirect it and the sentinel
	// vanishes and the case passes as 0 == 0, testing nothing.
	{name: "redirected", script: `"${CORE_AGENT}" -p hi < /dev/null 2>/dev/null`},

	// The tmux shape from dev/uat/attach/run.sh: a whole command line
	// built as a double-quoted string and dispatched later. This is the
	// case a lexer that blanks quoted spans — shellCode() in
	// examples/gke-platform-agent/recipe_test.go — cannot see at all.
	{name: "dispatched command string", script: `cmd="cd /tmp && FOO=1 '${BIN}' -p hi"` + "\n" + `bash -c "$cmd"`},
	{name: "heredoc fed to bash", script: "bash <<EOF\n\"${CORE_AGENT}\" -p hi\nEOF"},
	{name: "bash -c single quoted", script: `bash -c '"$CORE_AGENT" -p hi'`},
	// The same dispatched shape written the other idiomatic way. The
	// binary reference arrives as `\"${BIN}\"`, so unquote() has to strip
	// the backslash as well as the quote or the site is invisible —
	// a silent miss in one of the two places this check exists for.
	{name: "escaped quotes in a dispatched string", script: `cmd="cd /tmp && \"${BIN}\" -p hi"` + "\n" + `bash -c "$cmd"`},

	// Brace groups. Written across lines the newline restores command
	// position on its own; the one-line forms do not, and both are
	// idiomatic.
	{name: "one-line brace group", script: `{ "${CORE_AGENT}" -p hi; }`},
	{name: "one-line function body", script: `run_agent() { "${CORE_AGENT}" "$@"; }` + "\n" + `run_agent -p hi`},

	// --- does not execute, and the scanner must stay quiet ---
	{name: "test -x is not a call", script: `[[ -x "${CORE_AGENT}" ]] && true`},
	{name: "newer-than test", script: `[[ /etc/hostname -nt "${CORE_AGENT}" ]] || true`},
	{name: "mentioned in echo", script: `echo "building ${CORE_AGENT}" > /dev/null`},
	{name: "passed as a flag value", script: `ls -l "${CORE_AGENT}" > /dev/null`},
	{name: "passed as --binary", script: `echo --binary "${CORE_AGENT}" > /dev/null`},
	{name: "default-value assignment", script: `CORE_AGENT="${CORE_AGENT:-/bin/true}"` + "\n" + `:`},
	{name: "build output flag", script: `echo build -o "${CORE_AGENT}" ./cmd/core-agent > /dev/null`},
	{name: "prose naming the binary", script: `echo "core-agent attach http://x" > /dev/null`},
	{name: "prose with a slashed name", script: `echo "deploy/core-agent in ns has no replica" > /dev/null`},
	{name: "commented out", script: `# "${CORE_AGENT}" -p hi` + "\n" + `:`},
	{name: "apostrophe in a comment", script: `# it isn't a call: "${CORE_AGENT}" -p hi` + "\n" + `:`},

	// Bash constructs that broke a plausible-looking scanner before.
	{name: "json heredoc data", script: "cat > /dev/null <<EOF\n{\"a\": \"it's fine\"}\nEOF"},
	{name: "empty heredoc delimiter", script: "cat > /dev/null <<\"\"\n{\"a\": 1}\n\n:"},
	{name: "herestring", script: `cat > /dev/null <<< "core-agent"`},
	{name: "arithmetic left shift", script: `echo $((1 << 2)) > /dev/null`},
	{name: "backslash heredoc delimiter", script: "cat > /dev/null <<\\EOF\nnot a call\nEOF"},
	{name: "indented heredoc", script: "\tcat > /dev/null <<-EOF\n\tnot a call\n\tEOF"},

	// --- known and accepted divergences ---
	//
	// A divergence towards "the scanner sees more than bash ran" costs a
	// false positive, which is loud and takes a minute to fix. One the
	// other way is a silent miss, and the gate then passes forever. Only
	// the first kind is acceptable here; the second is listed only where
	// no harness script can reach it.
	{
		name:       "unexpanded heredoc delimiter",
		script:     "cat > /dev/null <<'EOF'\n${CORE_AGENT} -p hi\nEOF",
		divergence: "SAFE DIRECTION. <<'EOF' suppresses expansion, so `cat` writes the text and runs nothing — but `bash <<'EOF'` with the same body expands and runs it at execution time, and the scanner cannot tell the two apart without knowing the command the heredoc feeds. It flags both.",
		wantScan:   1,
	},
	{
		name:       "third-level nesting",
		script:     `outer="bash -c '\"${CORE_AGENT}\" -p hi'"` + "\n" + `bash -c "$outer"`,
		divergence: "nested content is lexed without quote tracking, so a command string inside a command string is not recursed a third time. No harness script in this repo nests that deep; adding one would need this fixed.",
		wantScan:   0,
	},
}

// stubScript prints a sentinel line per invocation so the oracle can
// count executions.
const stubScript = "#!/usr/bin/env bash\necho ORACLE_SENTINEL \"$@\"\n"

func TestLexerAgreesWithBash(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skipf("bash not available: %v", err)
	}
	dir := t.TempDir()
	stub := filepath.Join(dir, "core-agent")
	if err := os.WriteFile(stub, []byte(stubScript), 0o755); err != nil {
		t.Fatal(err)
	}

	for _, tc := range oracleCases {
		t.Run(tc.name, func(t *testing.T) {
			script := strings.NewReplacer("${STUB}", stub).Replace(tc.script)

			got := len(Scan("case.sh", script))

			cmd := exec.Command(bash, "-c", script)
			cmd.Env = append(os.Environ(),
				"CORE_AGENT="+stub, "BIN="+stub, "AGENT_BIN="+stub, "STUB="+stub)
			cmd.Dir = dir
			out, err := cmd.CombinedOutput()
			if err != nil {
				// A non-zero exit is fine (a [[ ]] that fails, say). A
				// syntax error is not: it would mean the case never
				// tested anything.
				if strings.Contains(string(out), "syntax error") {
					t.Fatalf("case is not valid bash:\n%s\n--- output ---\n%s", script, out)
				}
			}
			ran := strings.Count(string(out), "ORACLE_SENTINEL")

			if tc.divergence != "" {
				if got != tc.wantScan {
					t.Fatalf("divergent case changed: scanner found %d, want %d\n%s", got, tc.wantScan, tc.divergence)
				}
				if got == ran {
					t.Fatalf("divergence is gone (scanner %d == bash %d) — delete the divergence note:\n%s", got, ran, tc.divergence)
				}
				return
			}
			if got != ran {
				t.Errorf("scanner found %d invocation(s), bash ran %d\nscript:\n%s\noutput:\n%s",
					got, ran, script, out)
			}
		})
	}
}

// TestOracleStubActuallyRuns guards the oracle itself. If the stub were
// never reachable — wrong env var, wrong mode bits — every positive case
// would report 0 == 0 and the whole suite would pass while testing
// nothing.
func TestOracleStubActuallyRuns(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skipf("bash not available: %v", err)
	}
	dir := t.TempDir()
	stub := filepath.Join(dir, "core-agent")
	if err := os.WriteFile(stub, []byte(stubScript), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bash, "-c", `"${CORE_AGENT}" -p hi`)
	cmd.Env = append(os.Environ(), "CORE_AGENT="+stub)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("stub did not run: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "ORACLE_SENTINEL -p hi") {
		t.Fatalf("stub ran but printed no sentinel: %q", out)
	}
}

// TestPositiveCasesActuallyExecute is the other half of the same guard,
// per-case: it asserts that every case not marked as a non-executing one
// really did reach the stub. Without it, a case that silently stopped
// running (a typo in the script, a changed shell builtin) would keep
// agreeing with a scanner that also found nothing.
func TestPositiveCasesActuallyExecute(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skipf("bash not available: %v", err)
	}
	dir := t.TempDir()
	stub := filepath.Join(dir, "core-agent")
	if err := os.WriteFile(stub, []byte(stubScript), 0o755); err != nil {
		t.Fatal(err)
	}
	executed := 0
	for _, tc := range oracleCases {
		script := strings.NewReplacer("${STUB}", stub).Replace(tc.script)
		cmd := exec.Command(bash, "-c", script)
		cmd.Env = append(os.Environ(),
			"CORE_AGENT="+stub, "BIN="+stub, "AGENT_BIN="+stub, "STUB="+stub)
		cmd.Dir = dir
		out, _ := cmd.CombinedOutput()
		if strings.Contains(string(out), "ORACLE_SENTINEL") {
			executed++
		}
	}
	// 18 positives plus the divergent third-level case, which executes
	// under bash and is exactly why it is divergent.
	if want := 19; executed != want {
		t.Errorf("%d oracle cases reached the stub, want %d — a case stopped executing and is now agreeing with the scanner for the wrong reason", executed, want)
	}
}
