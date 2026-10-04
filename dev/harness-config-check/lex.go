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

import "strings"

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
			if d, strip, ok := heredocDelim(heredocOperatorWord(t.text)); ok {
				pend = append(pend, pending{delim: d, strip: strip})
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
			t.spans = append(t.spans, span{text: src[i+1 : min(j, n)], line: l})
			i = min(j+1, n)
		case '$':
			if i+1 < n && src[i+1] == '(' {
				l := line
				var j int
				var hd []pending
				j, line, hd = skipSubstitution(src, i+2, line)
				t.pend = append(t.pend, hd...)
				t.spans = append(t.spans, span{text: src[i+2 : min(j, n)], line: l})
				i = min(j+1, n)
			} else {
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
			t.spans = append(t.spans, span{text: src[i+1 : min(j, n)], line: l})
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
// invocation from the line after. `$'…'` honours its backslash escapes, and
// `<<<` is a herestring, not a heredoc whose delimiter never arrives.
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
		case c == '$' && j+1 < n && src[j+1] == '\'':
			k := j + 2
			for k < n && src[k] != '\'' {
				if src[k] == '\\' {
					k++
				}
				if k < n && src[k] == '\n' {
					line++
				}
				k++
			}
			j = k + 1
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
				pend = append(pend, pending{delim: d, strip: strip})
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
	d := strings.NewReplacer(`"`, "", `'`, "", `\`, "").Replace(rest)
	if d == "" && !strings.ContainsAny(rest, `"'\`) {
		return "", false, false // bare `<<` with no word: not a heredoc
	}
	return d, strip, true
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
			out = append(out, tok{text: s[i:j], line: line, quoted: quotedAt[i]})
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
func quoteMask(s string) []bool {
	mask := make([]bool, len(s))
	var q byte // 0, '\'' or '"'
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
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
