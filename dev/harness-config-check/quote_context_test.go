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

import "testing"

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
