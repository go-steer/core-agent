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
	type pending struct {
		delim string
		strip bool // <<- form: leading tabs allowed before the terminator
	}
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
			// A herestring (<<<) is not a heredoc. Check the longer
			// operator first or the scan lands on the second '<' and
			// consumes the rest of the file as a heredoc body.
			if d, strip, ok := heredocDelim(t.text); ok {
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
			for j < n && src[j] != '"' {
				if src[j] == '\\' && j+1 < n {
					if src[j+1] == '\n' {
						line++
					}
					j += 2
					continue
				}
				if src[j] == '\n' {
					line++
				}
				j++
			}
			t.spans = append(t.spans, span{text: src[i+1 : min(j, n)], line: l})
			i = min(j+1, n)
		case '$':
			if i+1 < n && src[i+1] == '(' {
				j, l, depth := i+2, line, 1
				for j < n {
					switch src[j] {
					case '(':
						depth++
					case ')':
						depth--
					case '\n':
						line++
					}
					if depth == 0 {
						break
					}
					j++
				}
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
