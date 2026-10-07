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
	"strings"
	"unicode/utf8"
)

// tok is one shell token. sep marks a command separator (newline, ;, &,
// |, &&, ||, a subshell paren): the token after one of those stands in
// command position.
type tok struct {
	text  string
	line  int
	sep   bool
	spans []span // quoted / substituted interiors, to be scanned as code
	// quoted is set by lexPlain when the word began inside a quote of
	// the text being scanned. Only the *pin* side reads it — see
	// classify. lexShell never sets it, because at the top level a
	// quoted argument is one whole token and a `-c` inside it cannot
	// appear as a token of its own.
	quoted bool
	// pend holds heredocs opened inside one of this word's
	// substitutions whose `)` closed before the line ended — bash reads
	// their bodies after the next newline all the same, so lexShell must.
	pend []pending
}

// pending is a heredoc whose operator has been seen and whose body
// begins after the next newline.
type pending struct {
	delim string
	strip bool // <<- form: leading tabs allowed before the terminator
	// quoted: any part of the delimiter word was quoted, so the body is
	// taken literally; otherwise bash expands it and drops one level of
	// backslashes first (see unescapeBody).
	quoted bool
}

// newPending builds the pending heredoc for an operator word that
// heredocDelim accepted.
func newPending(word, delim string, strip bool) pending {
	op := word[strings.Index(word, "<<")+2:]
	return pending{delim: delim, strip: strip, quoted: strings.ContainsAny(op, `"'\`)}
}

// span is a stretch of a script that the shell may later execute even
// though it is quoted here — a tmux command string, a bash -c argument,
// a heredoc fed to ssh. line is the 1-based line the interior starts on.
type span struct {
	text string
	line int
}

const sepRunes = " \t\r\n;|&()"

// lexShell tokenises a whole script. It tracks quoting so that a `#`
// inside a string is not mistaken for a comment and an apostrophe in a
// comment cannot desync the scan; it returns heredoc bodies separately
// so their contents are scanned as their own document rather than
// leaking quote state into the enclosing script.
//
// It is not a bash parser and does not try to be. It resolves every
// ambiguity towards "this is code" (see the Scan doc comment for why
// that is the safe direction here), and lex_test.go holds it to that by
// running each case through real bash.
func lexShell(src string, startLine int) (toks []tok, heredocs []span) {
	line := startLine
	i, n := 0, len(src)
	// Delimiters seen on the current line, whose bodies begin after the
	// next newline.
	var pend []pending

	for i < n {
		c := src[i]
		switch {
		case c == '\n':
			toks = append(toks, tok{text: "\n", line: line, sep: true})
			line++
			i++
			for _, p := range pend {
				body, consumed, lines := readHeredoc(src[i:], p.delim, p.strip)
				if !p.quoted {
					body = unescapeBody(body)
				}
				heredocs = append(heredocs, span{text: body, line: line})
				i += consumed
				line += lines
			}
			pend = nil
		case c == ' ' || c == '\t' || c == '\r':
			i++
		case c == '\\' && i+1 < n && src[i+1] == '\n':
			line++
			i += 2
		case c == '#':
			// Word-initial only: we reach this arm from whitespace or a
			// separator, never mid-word, so this really is a comment.
			for i < n && src[i] != '\n' {
				i++
			}
		case strings.HasPrefix(src[i:], "&&"), strings.HasPrefix(src[i:], "||"):
			toks = append(toks, tok{text: src[i : i+2], line: line, sep: true})
			i += 2
		case c == ';' || c == '|' || c == '&' || c == '(' || c == ')':
			toks = append(toks, tok{text: string(c), line: line, sep: true})
			i++
		default:
			var t tok
			t, i, line = lexWord(src, i, line)
			pend = append(pend, t.pend...)
			// A herestring (<<<) is not a heredoc. Check the longer
			// operator first or the scan lands on the second '<' and
			// consumes the rest of the file as a heredoc body.
			if w := heredocOperatorWord(t.text); w != "" {
				if d, strip, ok := heredocDelim(w); ok {
					pend = append(pend, newPending(w, d, strip))
				}
			}
			toks = append(toks, t)
		}
	}
	return toks, heredocs
}

// lexWord consumes one word, recording the quoted and substituted
// interiors inside it.
func lexWord(src string, i, line int) (tok, int, int) {
	start := i
	n := len(src)
	t := tok{line: line}
	for i < n {
		c := src[i]
		if strings.IndexByte(sepRunes, c) >= 0 {
			break
		}
		switch c {
		case '\\':
			if i+1 < n {
				if src[i+1] == '\n' {
					line++
				}
				i += 2
			} else {
				i++
			}
		case '\'':
			j, l := i+1, line
			for j < n && src[j] != '\'' {
				if src[j] == '\n' {
					line++
				}
				j++
			}
			t.spans = append(t.spans, span{text: src[i+1 : min(j, n)], line: l})
			i = min(j+1, n)
		case '"':
			j, l := i+1, line
			var hd []pending
			j, line, hd = skipDoubleQuoted(src, j, line)
			t.pend = append(t.pend, hd...)
			t.spans = append(t.spans, span{text: unescapeDQ(src[i+1 : min(j, n)]), line: l})
			i = min(j+1, n)
		case '$':
			switch {
			case i+1 < n && src[i+1] == '(':
				l := line
				var j int
				var hd []pending
				j, line, hd = skipSubstitution(src, i+2, line)
				t.pend = append(t.pend, hd...)
				t.spans = append(t.spans, span{text: src[i+2 : min(j, n)], line: l})
				i = min(j+1, n)
			case i+1 < n && src[i+1] == '\'':
				// ANSI-C quoting (#1241): `\'` does not close it. The
				// interior is recorded like a single-quoted span so a
				// `bash -c $'…'` command string is still scanned, and
				// recorded DECODED, because that is the text bash -c
				// receives: `\n` separates two commands and `\x27` is
				// a quote. Raw, `$'cd /tmp\n"$BIN" -p hi'` hid the
				// invocation behind `/tmp\n"$BIN"`, one word. The cost
				// is that a finding after a decoded `\n` reports a line
				// past the real one — loud and still in the right span.
				l := line
				var j int
				j, line = skipANSIC(src, i+2, line)
				t.spans = append(t.spans, span{text: decodeANSIC(src[i+2 : min(j, n)]), line: l})
				i = min(j+1, n)
			case i+1 < n && src[i+1] == '$':
				i += 2 // $$ is the PID; a `'` after it is an ordinary quote
			default:
				i++
			}
		case '`':
			j, l := i+1, line
			for j < n && src[j] != '`' {
				if src[j] == '\n' {
					line++
				}
				j++
			}
			t.spans = append(t.spans, span{text: unescapeBackticks(src[i+1 : min(j, n)]), line: l})
			i = min(j+1, n)
		default:
			i++
		}
	}
	t.text = src[start:i]
	return t, i, line
}

// heredocOperatorWord returns word when its `<<` is a heredoc operator at THIS
// level of the script, and "" when the `<<` sits inside a quote or a
// substitution.
//
// Without it, a word like `X="$(python3 - <<'PY' || echo e …PY\n)"`
// matched heredocDelim on the `<<` inside the substitution, registered a
// top-level heredoc with a delimiter assembled from the rest of the word,
// and — that delimiter never matching any line — swallowed the remainder
// of the file as a heredoc body at the next newline (#1209). Everything
// after it was then scanned under the wrong rules, which is why a comment
// apostrophe 30 lines later on dev/uat/self-dev/run.sh flipped two pins.
// The heredoc inside the substitution is still handled: skipSubstitution
// reads it, and the substitution's span is scanned as its own document.
//
// So the word is walked with its quoting: the first `<<` outside every
// quote and substitution is the operator, and the word is returned from
// there, because a quoted delimiter (`<<'EOF'`, `<<"EOF"`) is exactly the
// quote that follows the operator and heredocDelim needs it. A quote
// BEFORE the operator does not demote it — `cat >"$f"<<EOF` is a heredoc,
// and an earlier "before the first quote" rule dropped its body into the
// code scan.
func heredocOperatorWord(word string) string {
	n := len(word)
	for i := 0; i < n; {
		switch c := word[i]; {
		case c == '\\':
			i += 2
		case c == '\'':
			k := strings.IndexByte(word[i+1:], '\'')
			if k < 0 {
				return ""
			}
			i += k + 2
		case c == '`':
			k := strings.IndexByte(word[i+1:], '`')
			if k < 0 {
				return ""
			}
			i += k + 2
		case c == '"':
			j, _, _ := skipDoubleQuoted(word, i+1, 0)
			i = j + 1
		case c == '$' && i+1 < n && word[i+1] == '(':
			j, _, _ := skipSubstitution(word, i+2, 0)
			i = j + 1
		case c == '$' && i+1 < n && word[i+1] == '\'':
			j, _ := skipANSIC(word, i+2, 0)
			i = j + 1
		case c == '$' && i+1 < n && word[i+1] == '$':
			i += 2
		case c == '<' && strings.HasPrefix(word[i:], "<<<"):
			i += 3
		case c == '<' && strings.HasPrefix(word[i:], "<<"):
			return word[i:]
		default:
			i++
		}
	}
	return ""
}

// skipDoubleQuoted scans from just inside an opening `"` to its closing
// `"`, returning that index (or len(src)) and the updated line.
//
// A `$(` inside double quotes opens a FRESH quoting context in bash: in
// `"$(cmd "arg")"` the inner quotes belong to the substitution, and the
// outer string ends at the last `"`. Scanning straight to the next `"`
// — what this did before #1209 — closes the outer string inside the
// substitution and leaves the rest of the file with inverted quote
// parity. On dev/uat/self-dev/run.sh that let an apostrophe in a later
// comment decide whether two pinned invocations were credited, and the
// nested-substitution oracle case shows the same skew dropping an
// invocation from the scan entirely, which is the fatal direction for a
// violation scanner.
//
// It also returns the heredocs its substitutions opened and left unread;
// see skipSubstitution.
func skipDoubleQuoted(src string, j, line int) (int, int, []pending) {
	n := len(src)
	var left []pending
	for j < n && src[j] != '"' {
		switch {
		case src[j] == '\\' && j+1 < n:
			if src[j+1] == '\n' {
				line++
			}
			j += 2
			continue
		case src[j] == '$' && j+1 < n && src[j+1] == '$':
			j += 2 // the PID, so a `(` after it opens nothing
			continue
		case src[j] == '$' && j+1 < n && src[j+1] == '(':
			var hd []pending
			j, line, hd = skipSubstitution(src, j+2, line)
			left = append(left, hd...)
			j++ // past the closing ')'
			continue
		case src[j] == '\n':
			line++
		}
		j++
	}
	return j, line, left
}

// unescapeDQ removes one level of double-quote escaping from the
// interior of a "…" string: `\"`, `\\`, `\$` and "\`", the only bytes a
// backslash escapes there. What remains is the text a later
// `bash -c "$cmd"` receives. quoteMask treats a backslash as an escape,
// so the raw interior of `cmd="'$B' -p \"explain -c x.json\""` would hide
// the quotes around the prompt and credit its -c as a pin (#1241).
//
// A `$(…)` inside the string is copied as written: it opens a fresh
// quoting context, and its backslashes are its own. A backslash-newline
// is left for lexPlain to join, so line numbers do not move.
func unescapeDQ(s string) string { return unescapeOnce(s, "$`\"\\", true) }

// unescapeBody does the same for a heredoc body whose delimiter is
// unquoted, where bash removes a backslash only before `\`, `$` and "`"
// (a `"` there is ordinary text, so `\"` keeps its backslash).
func unescapeBody(s string) string { return unescapeOnce(s, "$`\\", true) }

// unescapeBackticks does it for a `…` substitution, whose text loses the
// same three backslashes before it is parsed — there is no fresh context
// at a `$(` inside it, since the whole text is unescaped first.
//
// Without these two, quoteMask's backslash handling read a raw `\\\"` as
// an escaped backslash and an escaped quote, while bash, a level later,
// sees `\\"` and a quote that opens: a -c inside the argument was
// credited as a pin (#1241).
func unescapeBackticks(s string) string { return unescapeOnce(s, "$`\\", false) }

// unescapeOnce drops the backslash before each byte in escapable, copying
// a `$( … )` verbatim when keepSubst is set.
func unescapeOnce(s, escapable string, keepSubst bool) string {
	var b strings.Builder
	n := len(s)
	for i := 0; i < n; i++ {
		switch {
		case s[i] == '\\' && i+1 < n && strings.IndexByte(escapable, s[i+1]) >= 0:
			i++
			b.WriteByte(s[i])
		case s[i] == '$' && i+1 < n && s[i+1] == '$':
			b.WriteString("$$")
			i++
		case keepSubst && s[i] == '$' && i+1 < n && s[i+1] == '(':
			j, _, _ := skipSubstitution(s, i+2, 0)
			e := min(j+1, n)
			b.WriteString(s[i:e])
			i = e - 1
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// skipANSIC scans from just inside `$'` to the `'` that closes it,
// returning that index (or len(src)) and the updated line.
//
// `$'…'` is ANSI-C quoting, where a backslash escapes the next byte: in
// `$'it\'s'` the string runs to the last `'`, and in `$'a\\'` the `\\` is
// one escaped backslash, so the `'` after it closes the string. Ending
// it at the first `'` — what lexWord did before #1241 — inverts quote
// parity for the rest of the line, and a `#` after it then hides the
// next invocation from the scan.
//
// Callers reach this only from an unquoted `$` that is not itself the
// second half of `$$` (the PID, after which `'` is an ordinary quote).
// Inside double quotes `$'` is literal text, so skipDoubleQuoted has no
// such arm.
func skipANSIC(src string, j, line int) (int, int) {
	n := len(src)
	for j < n && src[j] != '\'' {
		if src[j] == '\\' && j+1 < n {
			j++
		}
		if src[j] == '\n' {
			line++
		}
		j++
	}
	return j, line
}

// skipSubstitution scans from just inside `$(` to its matching `)`,
// returning that index (or len(src)) and the updated line. A `)` only
// closes it outside quotes, outside a nested substitution and outside a
// heredoc body, so `$(cat <<'EOF'\n)\nEOF\n)` and `$(echo ")")` both end
// at their last `)`. A heredoc's body starts after the newline that ends
// the line its `<<WORD` is on and runs to a line equal to the delimiter.
//
// A word-initial `#` starts a comment that runs to the newline, and a
// comment's apostrophe or `"` opens nothing: reading it as a quote ran the
// scan to the next one, closed on a `)` inside that string, and dropped an
// invocation from the line after. `$'…'` honours its backslash escapes
// (skipANSIC) unless the `$` ends a `$$`, and `<<<` is a herestring, not a
// heredoc whose delimiter never arrives.
//
// A heredoc can outlive its substitution: in `x=$(cat <<EOF)\nbody\nEOF`
// the `)` closes first and bash reads the body after the newline anyway.
// Those still-unread delimiters are returned for the caller to read at its
// next newline; dropping them left the body to be lexed as code, where its
// apostrophe hid a later invocation from the scan.
func skipSubstitution(src string, j, line int) (int, int, []pending) {
	n := len(src)
	depth := 1
	var pend []pending // heredocs waiting for the end of this line
	for j < n {
		c := src[j]
		switch {
		case c == '\\' && j+1 < n:
			if src[j+1] == '\n' {
				line++
			}
			j += 2
			continue
		case c == '#' && j > 0 && strings.IndexByte(" \t\n;|&(", src[j-1]) >= 0:
			for j < n && src[j] != '\n' {
				j++
			}
			continue // the newline arm below still reads pending heredocs
		case c == '$' && j+1 < n && src[j+1] == '$':
			j += 2
			continue
		case c == '$' && j+1 < n && src[j+1] == '\'':
			j, line = skipANSIC(src, j+2, line)
			j++
			continue
		case c == '<' && strings.HasPrefix(src[j:], "<<<"):
			j += 3
			continue
		case c == '\'':
			k := j + 1
			for k < n && src[k] != '\'' {
				if src[k] == '\n' {
					line++
				}
				k++
			}
			j = k + 1
			continue
		case c == '"':
			var hd []pending
			j, line, hd = skipDoubleQuoted(src, j+1, line)
			pend = append(pend, hd...)
			j++
			continue
		case c == '$' && j+1 < n && src[j+1] == '(':
			var hd []pending
			j, line, hd = skipSubstitution(src, j+2, line)
			pend = append(pend, hd...)
			j++
			continue
		case c == '<' && strings.HasPrefix(src[j:], "<<"):
			k := j + 2
			for k < n && strings.IndexByte(" \t\n;|&()", src[k]) < 0 {
				k++
			}
			if d, strip, ok := heredocDelim(src[j:k]); ok {
				pend = append(pend, newPending(src[j:k], d, strip))
			}
			j = k
			continue
		case c == '\n':
			line++
			j++
			for _, p := range pend {
				_, consumed, lines := readHeredoc(src[j:], p.delim, p.strip)
				j += consumed
				line += lines
			}
			pend = nil
			continue
		case c == '(':
			depth++
		case c == ')':
			depth--
			if depth == 0 {
				return j, line, pend
			}
		}
		j++
	}
	return j, line, pend
}

// heredocDelim recognises the <<WORD / <<-WORD forms and returns the
// delimiter with its quoting removed. It rejects <<< (herestring) and
// << with nothing after it (a left-shift). The empty delimiter of
// `cat <<""` is legal bash and terminates at the first empty line.
func heredocDelim(word string) (delim string, strip bool, ok bool) {
	k := strings.Index(word, "<<")
	if k < 0 {
		return "", false, false
	}
	rest := word[k+2:]
	if strings.HasPrefix(rest, "<") {
		return "", false, false // herestring
	}
	if strings.HasPrefix(rest, "-") {
		strip = true
		rest = rest[1:]
	}
	// `$((1 << 2))` arrives here as part of an arithmetic word; a shift
	// is always followed by a space or a digit, never by a word
	// character that could be a delimiter.
	if rest != "" && rest[0] >= '0' && rest[0] <= '9' {
		return "", false, false
	}
	d := unquoteDelim(rest)
	if d == "" && !strings.ContainsAny(rest, `"'\`) {
		return "", false, false // bare `<<` with no word: not a heredoc
	}
	return d, strip, true
}

// unquoteDelim performs bash's quote removal on a heredoc delimiter
// word, which is what the terminator line has to equal: `<<$'EOF'` ends
// at a line reading EOF, and `<<"E'F"` at one reading E'F. Deleting
// every quote character, as this did before #1241, turned the first into
// `$EOF` — a delimiter no line matches, so the heredoc swallowed the rest
// of the file and the invocations after it were lexed as heredoc text.
//
// Getting this wrong is NOT safe. A delimiter that never matches runs
// the body to the end of the file, and a body is lexed by lexPlain,
// which does not recurse into quoted spans: a later
// `bash -c '"$BIN" -p hi'` is then missed outright, the fatal direction.
// So every escape bash defines for `$'…'` is decoded (see decodeANSIC);
// an escape it does not define stays verbatim in bash too.
func unquoteDelim(w string) string {
	var b strings.Builder
	n := len(w)
	for i := 0; i < n; i++ {
		switch c := w[i]; {
		case c == '\\' && i+1 < n:
			i++
			b.WriteByte(w[i])
		case c == '$' && i+1 < n && w[i+1] == '$':
			b.WriteString("$$") // the PID's name; a quote after it is ordinary
			i++
		case c == '$' && i+1 < n && w[i+1] == '\'':
			j, _ := skipANSIC(w, i+2, 0)
			b.WriteString(decodeANSIC(w[i+2 : min(j, n)]))
			i = j
		case c == '$' && i+1 < n && w[i+1] == '"':
			// $"…" is a locale-translated "…": drop the `$`, and the
			// next iteration reads the double-quoted string.
		case c == '\'':
			j := i + 1
			for j < n && w[j] != '\'' {
				j++
			}
			b.WriteString(w[i+1 : j])
			i = j
		case c == '"':
			j := i + 1
			for ; j < n && w[j] != '"'; j++ {
				if w[j] == '\\' && j+1 < n && strings.IndexByte("$`\"\\\n", w[j+1]) >= 0 {
					j++
				}
				b.WriteByte(w[j])
			}
			i = j
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// decodeANSIC decodes a `$'…'` interior the way bash does: the letter
// escapes, \nnn (one to three octal digits), \xHH (one or two hex
// digits), \uHHHH and \UHHHHHHHH (one to four / eight hex digits, as
// UTF-8), and \cX (control-X, with `\c\\` reading one escaped
// backslash). An escape bash does not define stays as written, in bash
// and here. Each of these is a way to spell a newline, a quote or a
// separator, so leaving one raw hid `bash -c $'cd /tmp\012"$BIN" -p hi'`
// from the scan.
//
// The one place this differs from bash: a decoded NUL ends the string in
// bash, and here it does not. That can only make the scanner see more
// than bash ran, which is the loud direction.
func decodeANSIC(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] != '\\' || i+1 >= len(s) {
			b.WriteByte(s[i])
			i++
			continue
		}
		out, used := decodeANSICEscape(s[i+1:])
		if used == 0 {
			b.WriteByte('\\')
			i++
			continue
		}
		b.WriteString(out)
		i += 1 + used
	}
	return b.String()
}

// decodeANSICEscape decodes the escape whose text (after the backslash)
// starts e, returning its value and how many bytes of e it used; 0 means
// bash does not define it and the backslash is literal.
func decodeANSICEscape(e string) (string, int) {
	if r, ok := ansiCSimple[e[0]]; ok {
		return string([]byte{r}), 1
	}
	switch c := e[0]; {
	case c >= '0' && c <= '7':
		v, m := digits(e, 0, 3, 8)
		return string([]byte{lowByte(v)}), m
	case c == 'x' || c == 'u' || c == 'U':
		limit := map[byte]int{'x': 2, 'u': 4, 'U': 8}[c]
		v, m := digits(e, 1, limit, 16)
		if m == 0 {
			return "", 0
		}
		if c == 'x' {
			return string([]byte{lowByte(v)}), 1 + m
		}
		if v < 0 || v > utf8.MaxRune {
			v = utf8.RuneError
		}
		return string(rune(v)), 1 + m
	case c == 'c' && len(e) > 1:
		x, used := e[1], 2
		if x == '\\' && len(e) > 2 && e[2] == '\\' {
			used = 3
		}
		if x == '?' {
			return "\x7f", used
		}
		if x >= 'a' && x <= 'z' {
			x -= 'a' - 'A'
		}
		return string([]byte{x & 0x1f}), used
	}
	return "", 0
}

// lowByte is the byte bash keeps of an escape's value: \777 is 0xff.
func lowByte(v int) byte {
	v &= 0xff
	if v < 0 || v > 0xff {
		return 0
	}
	return byte(v)
}

// digits reads up to limit digits of the given base from e[from:],
// returning the value and how many it read.
func digits(e string, from, limit, base int) (v, m int) {
	for m < limit && from+m < len(e) {
		d := digitVal(e[from+m])
		if d < 0 || d >= base {
			break
		}
		v = v*base + d
		m++
	}
	return v, m
}

func digitVal(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	}
	return -1
}

// ansiCSimple maps the letter after a backslash in `$'…'` to its byte.
var ansiCSimple = map[byte]byte{
	'a': '\a', 'b': '\b', 'e': 0x1b, 'E': 0x1b, 'f': '\f', 'n': '\n',
	'r': '\r', 't': '\t', 'v': '\v', '\\': '\\', '\'': '\'', '"': '"', '?': '?',
}

// readHeredoc returns the body up to the terminator line, how many
// bytes to skip, and how many lines that spans.
func readHeredoc(src, delim string, strip bool) (body string, consumed, lines int) {
	i := 0
	for i <= len(src) {
		j := strings.IndexByte(src[i:], '\n')
		var lineText string
		if j < 0 {
			lineText = src[i:]
		} else {
			lineText = src[i : i+j]
		}
		cand := lineText
		if strip {
			cand = strings.TrimLeft(cand, "\t")
		}
		if strings.TrimRight(cand, "\r") == delim {
			if j < 0 {
				return src[:i], len(src), lines + 1
			}
			return src[:i], i + j + 1, lines + 1
		}
		if j < 0 {
			// Unterminated: the rest of the file is the body. Returning
			// it (rather than dropping it) keeps the contents scanned.
			return src, len(src), lines + 1
		}
		i += j + 1
		lines++
	}
	return src, len(src), lines
}

// lexPlain tokenises nested content — a quoted command string, a
// heredoc body, a bash -c argument. Tokenisation itself ignores quoting:
// quote characters are ordinary bytes that unquote() strips when a token
// is tested, and a word is whatever sits between separators whether or
// not a quote is open.
//
// That is deliberate, and it is the *code* bias. Nested content is where
// prose lives ("the drill didn't converge"), and an apostrophe there
// would desync a quote-tracking lexer and blank a window of real lines —
// a silent miss, the one error this scanner must not make. Splitting on
// whitespace cannot desync, and the cost is only that a nested
// `bash -c '…'` is not recursed into a third level.
//
// It does, separately, record for each word whether a best-effort shell
// quote state machine thinks it began inside a quote. Nothing about
// *finding* an invocation consults that flag; only classify() does, to
// refuse a `-c` that is really a word of an argument
// (`-p 'explain the -c flag'` is not a pin). That question needs the
// opposite bias, and the flag has it: an unbalanced apostrophe leaves
// the machine stuck inside a quote, so the words after it look quoted,
// so pins stop being credited — loud, not silent.
func lexPlain(s string, startLine int) []tok {
	var out []tok
	quotedAt := quoteMask(s)
	line := startLine
	i, n := 0, len(s)
	for i < n {
		c := s[i]
		switch {
		case c == '\n':
			out = append(out, tok{text: "\n", line: line, sep: true})
			line++
			i++
		case c == ' ' || c == '\t' || c == '\r':
			i++
		case c == '\\' && i+1 < n && s[i+1] == '\n':
			line++
			i += 2
		case strings.HasPrefix(s[i:], "&&"), strings.HasPrefix(s[i:], "||"):
			out = append(out, tok{text: s[i : i+2], line: line, sep: true})
			i += 2
		case c == ';' || c == '|' || c == '&' || c == '(' || c == ')':
			out = append(out, tok{text: string(c), line: line, sep: true})
			i++
		default:
			j := i
			for j < n && strings.IndexByte(sepRunes, s[j]) < 0 {
				j++
			}
			// A word right after an escaped blank is glued to the word
			// before it (`"'"\ -c` is one argument), so it is as much
			// inside an argument as a quoted one.
			glued := i > 0 && quotedAt[i-1] && (s[i-1] == ' ' || s[i-1] == '\t')
			out = append(out, tok{text: s[i:j], line: line, quoted: quotedAt[i] || glued})
			i = j
		}
	}
	return out
}

// quoteMask marks, byte by byte, whether that byte of s sits inside a
// shell quote. It is the ordinary two-state machine — inside '…' a "
// is literal and vice versa — and it is best-effort by design: an
// unclosed quote simply marks everything after it. See lexPlain for why
// that direction is the safe one for the only question it answers.
//
// Outside single quotes a backslash escapes the next byte, so `\"` and
// `\'` open and close nothing (#1241). Ignoring that paired the quotes of
// `-p "say \" -c x.json \" ok"` wrong and credited the -c inside the
// argument as a pin — a silent credit, not the loud kind. Text that is
// itself the interior of a double-quoted string must be unescaped one
// level before it gets here (see lexWord), or a `\"` that IS a quote by
// the time the command runs would be skipped.
func quoteMask(s string) []bool {
	mask := make([]bool, len(s))
	var q byte // 0, '\'' or '"'
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\' && q != '\'' && i+1 < len(s):
			// The escaped byte is quoted, by the backslash.
			mask[i], mask[i+1] = q != 0, true
			i++
		case q == 0 && (c == '\'' || c == '"'):
			q = c
			mask[i] = true
		case q != 0 && c == q:
			q = 0
			mask[i] = true
		default:
			mask[i] = q != 0
		}
	}
	return mask
}
