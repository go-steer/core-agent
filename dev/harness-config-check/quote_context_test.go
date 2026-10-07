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

// #1241 independent review. The bash oracle compares invocation COUNTS;
// this one compares the PIN. Each script runs with the binary replaced by
// a stub that prints one argument per line, and the scanner's Pinned
// must equal "bash passed `-c` followed by a .json path as two separate
// arguments". A pin credited where bash passed no -c is the silent
// direction: the gate then never asks for the pin that is missing.
//
// Every script has exactly one invocation, so the row compares one
// verdict, and each row is checked for really having run.
func TestPinCreditAgreesWithBash(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skipf("bash not available: %v", err)
	}
	dir := t.TempDir()
	stub := filepath.Join(dir, "core-agent")
	if err := os.WriteFile(stub, []byte("#!/usr/bin/env bash\necho PIN_ORACLE_RAN\nprintf 'ARG<%s>\\n' \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cases := []struct{ name, script string }{
		// Decoded `$'…'` puts real backslashes and quotes into the command
		// string; a quote scan that ignores the backslash pairs them wrong.
		{"escaped quotes from hex escapes", `bash -c $'"$CORE_AGENT" -p "a \x5c\x22 -c /x.json \x5c\x22 b"'`},
		{"hex quote, escaped apostrophe, hex backslash", `bash -c $'"$CORE_AGENT" -p \x22\'"\x5c -c /x.json #'`},
		{"escaped apostrophe around a hex quote", `bash -c $'"$CORE_AGENT" -p \'\x22\'\\ -c /x.json'`},
		// The same flaw without ANSI-C: an escaped `"` inside a double-
		// quoted argument of a single-quoted command string.
		{"escaped double quote in a quoted argument", `bash -c '"$CORE_AGENT" -p "say \" -c /x.json \" ok"'`},
		{"escaped backslash before a closing quote", `bash -c '"$CORE_AGENT" -p "a\\" -c /x.json'`},
		// A double-quoted command string is unescaped once before bash -c
		// sees it: `\"` there IS a quote by the time the command runs.
		{"escaped quotes in a dispatched double-quoted string", `cmd="'${BIN}' -p \"explain -c /x.json\""` + "\n" + `bash -c "$cmd"`},
		// An unquoted heredoc body and a backtick substitution lose one
		// level of `\\`, `\$` and "\`" before bash parses them, so a raw
		// `\\\"` is `\\"` to bash: an escaped backslash, then a quote that
		// opens. Read raw, the -c inside that quote was credited.
		{"escaped backslash and quote in an unquoted heredoc body", "bash <<EOF\n\"\\$CORE_AGENT\" -p \\\\\\\" -c /x.json \"\nEOF"},
		{"escaped backslash and apostrophe in an unquoted heredoc body", "bash <<EOF\n\"\\$CORE_AGENT\" -p \\\\\\' -c /x.json '\nEOF"},
		{"escaped backslash and quote in backticks", "x=`\"$CORE_AGENT\" -p \\\\\\\" -c /x.json \"`; echo \"$x\""},
		// …and a quoted delimiter takes the body literally: `\\\"` is an
		// escaped backslash and an escaped quote, and the pin is real.
		{"escaped backslash and quote in a quoted heredoc body", "bash <<'EOF'\n\"$CORE_AGENT\" -p \\\\\\\" -c /x.json\nEOF"},
		// Inside single quotes a backslash is literal: 'a\' closes there.
		{"backslash before a closing single quote", `cmd="\"${BIN}\" -p 'a\\' -c /x.json"` + "\n" + `bash -c "$cmd"`},
		// A `$(…)` inside double quotes is a fresh context; its `\"` is
		// an escaped quote of the substitution, not one to unescape.
		{"escaped quotes in a substitution in double quotes", `x="$("$CORE_AGENT" -p "a \" -c /x.json \" b")"; echo "$x"`},
		{"real pin in a dispatched double-quoted string", `cmd="\"${BIN}\" -c /x.json -p \"hi there\""` + "\n" + `bash -c "$cmd"`},
		{"real pin after a hex-quoted prompt", `bash -c $'"$CORE_AGENT" -p \x22it\x27s\x22 -c /x.json'`},
		{"real pin in a single-quoted command string", `bash -c '"$CORE_AGENT" -p "a \" b" -c /x.json'`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			invs := Scan("t.sh", tc.script)
			if len(invs) != 1 {
				t.Fatalf("scanner found %d invocation(s), want 1: %+v", len(invs), invs)
			}
			cmd := exec.Command(bash, "-c", tc.script)
			cmd.Env = append(os.Environ(), "CORE_AGENT="+stub, "BIN="+stub)
			cmd.Dir = dir
			out, _ := cmd.CombinedOutput()
			if !strings.Contains(string(out), "PIN_ORACLE_RAN") {
				t.Fatalf("the stub never ran, so the row tests nothing:\n%s", out)
			}
			args := strings.Split(strings.TrimSpace(string(out)), "\n")
			bashPinned := false
			for i := 0; i+1 < len(args); i++ {
				if args[i] == "ARG<-c>" && strings.HasSuffix(args[i+1], ".json>") {
					bashPinned = true
				}
			}
			if invs[0].Pinned != bashPinned {
				t.Errorf("scanner Pinned=%v, bash pinned=%v (args %s)\nbash saw:\n%s", invs[0].Pinned, bashPinned, invs[0].Args, out)
			}
		})
	}
}

// decodeANSIC is checked byte for byte against bash's own `$'…'`.
func TestDecodeANSICMatchesBash(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skipf("bash not available: %v", err)
	}
	for _, s := range []string{
		`a\nb`, `\t\v\f\r\a\b\e\E`, `\\\'\"\?`, `\q`, `\x41\x4`, `\xZ`,
		`\012`, `\11x`, `\1012`, `\777`, `\u000a`, `\u41`, `\U0000000a`, `\U41`,
		`\cJ`, `\cj`, `\c?`, `\c\\x`, // not `\c@` or `\0`: bash ends the string at a NUL; see decodeANSIC
	} {
		out, err := exec.Command(bash, "-c", "printf '%s' $'"+s+"'").Output()
		if err != nil {
			t.Fatalf("%q: %v", s, err)
		}
		if got := decodeANSIC(s); got != string(out) {
			t.Errorf("decodeANSIC(%q) = %q, bash gives %q", s, got, out)
		}
	}
}

// #1241. A heredoc delimiter is compared after bash's quote removal, and
// a delimiter computed wrongly never matches: the body then swallows the
// rest of the file. Each row is checked against bash as well as against
// heredocDelim — bash must end the body at want, and so must we.
func TestHeredocDelimiterMatchesBash(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skipf("bash not available: %v", err)
	}
	cases := []struct{ word, want string }{
		{"<<EOF", "EOF"},
		{"<<'EOF'", "EOF"},
		{`<<"EOF"`, "EOF"},
		{`<<\EOF`, "EOF"},
		{`<<E"O"F`, "EOF"},
		{`<<"E'F"`, "E'F"},
		{`<<"E\F"`, `E\F`},
		{`<<'E\F'`, `E\F`},
		{"<<$'EOF'", "EOF"},
		{`<<$'E\'F'`, "E'F"},
		{`<<$'E\\F'`, `E\F`},
		{`<<$'E\x41F'`, "EAF"},
		{`<<$'E\117F'`, "EOF"},
		{`<<$'E\u004fF'`, "EOF"},
		{`<<$"EOF"`, "EOF"},
		{"<<-$'EOF'", "EOF"},
		// `$$` is a name, not a quote opener: the `'` after it is an
		// ordinary quote and the `$$` stays in the delimiter.
		{"<<$$'EOF'", "$$EOF"},
		{`<<$$"EOF"`, "$$EOF"},
	}
	for _, tc := range cases {
		t.Run(tc.word, func(t *testing.T) {
			got, _, ok := heredocDelim(tc.word)
			if !ok || got != tc.want {
				t.Errorf("heredocDelim(%q) = %q, %v; want %q", tc.word, got, ok, tc.want)
			}
			script := "cat " + tc.word + "\nBODY\n" + tc.want + "\necho AFTER\n"
			out, _ := exec.Command(bash, "-c", script).CombinedOutput()
			if string(out) != "BODY\nAFTER\n" {
				t.Errorf("bash does not end %q at %q: the row is wrong\n%s", tc.word, tc.want, out)
			}
		})
	}
}

// #1209. The bash oracle compares invocation COUNTS, and the scanner
// deliberately descends into quoted spans, so a lexer that loses its
// quote context still counts the invocation that follows — the damage is
// to the PIN: a `-c` the quote scan places inside a quote is not credited.
// That is exactly how run.sh failed: two pinned invocations reported as
// unpinned. So each construct here is followed by a pinned invocation that
// must be found once and credited.
func TestAPinAfterAQuotingConstructIsStillCredited(t *testing.T) {
	t.Parallel()
	const pinned = "\n" + `"${CORE_AGENT}" -c "${CFG}" -p hi` + "\n"
	cases := []struct{ name, prefix string }{
		{"heredoc in a substitution in double quotes",
			"body=\"$(python3 - \"$1\" <<'PY'\nprint({\"prompt\": 1})\nPY\n)\"\n# it's one apostrophe"},
		{"heredoc with a trailing || in a substitution",
			"MODE=\"$(python3 - \"$A\" <<'PY' || echo error\nprint('x')\nPY\n)\"\n# the tier's premise"},
		{"spaced inner quotes in a substitution in double quotes",
			`x="a $(echo "b c") d"` + "\n# it's"},
		{"apostrophe inside a quoted-delimiter heredoc body",
			"cat <<'EOF' >/dev/null\nit's\nEOF"},
		// Odd quote counts are what break a mis-paired lexer: with even
		// counts it recovers by accident, which is why the two cases below
		// exist. An apostrophe inside double quotes inside a substitution
		// inside double quotes is literal; closing the outer string at the
		// inner `"` exposes it as a real single quote.
		{"apostrophe in double quotes in a substitution in double quotes",
			`x="$(echo "it's")"`},
		// An unbalanced double quote in a quoted heredoc's body is literal
		// text; lexed as code, it opens a quote that reaches the -c below.
		{"unbalanced double quote inside a quoted-delimiter heredoc body",
			"cat <<'EOF' >/dev/null\nsay \"hi\nEOF"},
		// A comment in a multi-line substitution, an ANSI-C string and a
		// herestring are each something a substitution scanner can take
		// for the start of a quote or a heredoc that never ends.
		{"apostrophe comment inside a multi-line substitution",
			"x=$(\n  # it's\n  echo hi\n)"},
		{"apostrophe comment inside a multi-line substitution in double quotes",
			"x=\"$(\n  # it's\n  echo hi\n)\""},
		{"ANSI-C string with an escaped apostrophe in a substitution",
			"x=$(echo $'it\\'s')"},
		// #1241: the same ANSI-C shapes at the top level, where lexWord
		// rather than skipSubstitution reads them, and as a heredoc
		// delimiter, where bash removes the ANSI-C quoting.
		// The `"it's"` after it is what makes the inverted parity reach
		// the pin: alone, the stray quote swallows the rest into one span
		// whose quotes balance, and the pin is credited by accident.
		{"top-level ANSI-C string with an escaped apostrophe",
			"x=$'it\\'s'; echo \"it's\""},
		{"top-level ANSI-C string ending in an escaped backslash",
			"x=$'it\\\\' y='s'"},
		{"PID parameter before a single quote",
			"echo $$'\\'"},
		// The two below recover their COUNT in the oracle — the swallowed
		// text is still scanned as a span or a heredoc body — so only the
		// pin shows the lexer went wrong.
		{"heredoc operator inside an ANSI-C string",
			"echo $'\\'<<EOF' >/dev/null\n# it's"},
		{"PID parameter before a paren in double quotes",
			"x=\"$$(\"; echo \"it's\""},
		{"ANSI-C heredoc delimiter",
			"cat <<$'EOF' >/dev/null\nit's\nEOF"},
		{"ANSI-C heredoc delimiter with an escaped apostrophe",
			"cat <<$'E\\'F' >/dev/null\nit's\nE'F"},
		{"herestring in a substitution in double quotes",
			"x=\"$(\n  tr a b <<<\"$v\"\n  echo z\n)\""},
		// A heredoc body inside a substitution is not code: its apostrophe
		// and `)` are text. Unquoted, the `<<` must also not be taken for a
		// top-level operator whose delimiter swallows the rest of the file.
		{"heredoc with an apostrophe and a paren in a substitution in double quotes",
			"x=\"$(cat <<'EOF'\nit's )\nEOF\n)\""},
		{"heredoc in an unquoted substitution",
			"x=$(cat <<'EOF'\nit's\nEOF\n)\n# it's"},
		{"top-level heredoc with no space after a quoted redirect target",
			"tee \"$f\"<<EOF >/dev/null\nit's prose\nEOF"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			invs := Scan("t.sh", tc.prefix+pinned)
			var agent []Invocation
			for _, in := range invs {
				if in.Text == `"${CORE_AGENT}"` {
					agent = append(agent, in)
				}
			}
			if len(agent) != 1 {
				t.Fatalf("found %d agent invocation(s), want 1: %+v", len(agent), invs)
			}
			if !agent[0].Pinned {
				t.Fatalf("the invocation after %q was found but its -c was not credited: the lexer lost quote context", tc.name)
			}
		})
	}
}

// The double-quote half of #1209's fix is not always visible in findings
// — the scanner's descent into spans recovers on many inputs — so it is
// pinned at the token level: bash opens a fresh quoting context at `$(`
// inside double quotes, so this is two words, not one word that runs to
// the end of the input on the apostrophe.
func TestDoubleQuotesOpenAFreshContextAtSubstitution(t *testing.T) {
	t.Parallel()
	toks, _ := lexShell(`x="$(echo "it's")" y`+"\n", 1)
	var words []string
	for _, tk := range toks {
		if !tk.sep {
			words = append(words, tk.text)
		}
	}
	if len(words) != 2 || words[0] != `x="$(echo "it's")"` || words[1] != "y" {
		t.Fatalf("lexed as %q; want [x=\"$(echo \"it's\")\" y]", words)
	}
}
