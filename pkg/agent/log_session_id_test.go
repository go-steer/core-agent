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

package agent

import (
	"bytes"
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v2/pkg/usage"
)

// Every operator-facing line this package logs names its session
// (#1137).
//
// #1136 named the four that report a turn CUT, which was a complete and
// checkable unit — every production a.Interrupt() site — and not the
// complete set an operator reads off a daemon. Nine more lines said
// something about one session and identified none of them, including
// the wake-fence notice, whose sentence ends by pointing at a reset
// that takes an id.
//
// Two tests, and they prove different things on purpose:
//
//   - the table below drives nine sites for real and reads the id back
//     out of the log, which is the only way to know the format string
//     and its arguments line up;
//   - the scanner underneath it reads the package's source, which is
//     the only way to cover a site nobody has written yet. #1136 fixed
//     four lines by hand and missed nine, so "we will remember next
//     time" is the approach with a measured failure rate.

// logSites drives one session-scoped log line each, on an agent whose
// session id is unique to the case. Unique ids are load-bearing: a
// single hardcoded suffix, or a suffix accidentally read off some other
// agent, fails eight of the nine.
func logSites() []struct {
	name    string
	session string
	needle  string // identifies this site's line within the captured log
	drive   func(t *testing.T, session string)
} {
	return []struct {
		name    string
		session string
		needle  string
		drive   func(t *testing.T, session string)
	}{{
		name:    "unknown context window",
		session: "s-1137-window",
		needle:  "is not in the context-window table",
		drive: func(t *testing.T, session string) {
			sessionAgent(t, session, &captureLLM{response: "unused"}).
				noteAssumedContextWindow("some-future-llm-7b")
		},
	}, {
		name:    "mechanical compaction announced",
		session: "s-1137-mech",
		needle:  "summarizer unavailable",
		drive: func(t *testing.T, session string) {
			sessionAgent(t, session, &captureLLM{response: "unused"}).noteMechanicalCompaction(
				"summarizer unavailable (down) — context was bounded by mechanical truncation instead.")
		},
	}, {
		name:    "mechanical fallback also failed",
		session: "s-1137-mechfail",
		needle:  "mechanical compaction fallback also failed",
		drive: func(t *testing.T, session string) {
			// No session service, so mechanicalCompact fails with
			// something other than errNothingToTruncate — the one
			// branch where both context-reduction strategies are down.
			a := &Agent{sessionID: session}
			a.fallBackToMechanicalCompaction(context.Background(), errors.New("summarizer down"))
		},
	}, {
		name:    "auto-compaction failed",
		session: "s-1137-autocompact",
		needle:  "auto-compaction failed",
		drive: func(t *testing.T, session string) {
			a := sessionAgent(t, session, &captureLLM{
				response: "unused",
				err:      errors.New("Error 429, Status: RESOURCE_EXHAUSTED"),
			})
			plantEvent(t, a, genai.RoleUser, "history over the threshold")
			a.mu.Lock()
			a.compactionPending = true
			a.mu.Unlock()
			a.runPendingCompaction(context.Background())
		},
	}, {
		name:    "pending checkpoint failed",
		session: "s-1137-checkpoint",
		needle:  "pending checkpoint failed",
		drive: func(t *testing.T, session string) {
			a := sessionAgent(t, session,
				&captureLLM{response: "unused", err: errors.New("summarizer down")},
				WithCheckpointer(NewDefaultCheckpointer()))
			plantEvent(t, a, genai.RoleUser, "something worth a checkpoint")
			a.mu.Lock()
			a.checkpointPending = true
			a.mu.Unlock()
			a.runPendingCheckpoint(context.Background())
		},
	}, {
		name:    "wake fenced",
		session: "s-1137-fence",
		needle:  "wake fenced while the guardrail is tripped",
		drive: func(t *testing.T, session string) {
			a := &Agent{
				sessionID:       session,
				inbox:           newInbox(),
				wake:            newWakeSignal(),
				watchdogTripped: true,
				watchdogReason:  "watchdog halted the agent (repeated-tool-call): looping.",
			}
			a.fireWakeFenced()
		},
	}, {
		name:    "inbox cap exceeded",
		session: "s-1137-inbox",
		needle:  "inbox cap exceeded",
		drive: func(t *testing.T, session string) {
			a := sessionAgent(t, session, &captureLLM{response: "unused"})
			for i := range defaultInboxCap + 1 {
				if err := a.Inject("queued message"); err != nil {
					t.Fatalf("Inject %d: %v", i, err)
				}
			}
		},
	}, {
		name:    "empty summary retried",
		session: "s-1137-empty",
		needle:  "model returned no summary text",
		drive: func(t *testing.T, session string) {
			// Unexplained rather than terminal: a provider that said
			// why is not retried, and the retry notice is the line.
			a := sessionAgent(t, session, &emptySummaryLLM{scripted: []*adkmodel.LLMResponse{
				textlessUnexplained(), textlessUnexplained(),
			}})
			plantEvent(t, a, genai.RoleUser, "some history worth summarizing")
			a.Compact(context.Background(), "") //nolint:errcheck // the failure is the point
		},
	}, {
		name:    "empty summary recovered",
		session: "s-1137-recovered",
		needle:  "empty summary recovered on retry",
		drive: func(t *testing.T, session string) {
			a := sessionAgent(t, session, &emptySummaryLLM{scripted: []*adkmodel.LLMResponse{
				textlessUnexplained(), summaryResponse("# Current state\nrecovered"),
			}})
			plantEvent(t, a, genai.RoleUser, "some history worth summarizing")
			if _, err := a.Compact(context.Background(), ""); err != nil {
				t.Fatalf("Compact: %v", err)
			}
		},
	}}
}

func TestEveryOperatorLogLineNamesItsSession(t *testing.T) {
	// Deliberately not parallel, here and in the subtests:
	// captureAgentLog replaces the global logger's writer.
	for _, tc := range logSites() {
		t.Run(tc.name, func(t *testing.T) {
			sink := captureAgentLog(t)
			tc.drive(t, tc.session)

			var line string
			for _, l := range strings.Split(sink.String(), "\n") {
				if strings.Contains(l, tc.needle) {
					line = l
					break
				}
			}
			if line == "" {
				t.Fatalf("this site logged nothing matching %q; the test drives the "+
					"wrong path.\nCaptured:\n%s", tc.needle, sink.String())
			}
			// The id AND its slot. Asserting the whole `agent: [session
			// x] ` opening rather than a bare Contains of the id is
			// what makes this a test of the convention: a line that
			// carried the id somewhere in its middle would satisfy
			// #1137's letter and lose the property the fixed slot buys,
			// which is that every message body stays greppable as one
			// contiguous string.
			if want := "agent: [session " + tc.session + "] "; !strings.Contains(line, want) {
				t.Errorf("line does not open with %q:\n%s\n\n"+
					"A daemon interleaves sessions into one log, so a notice that "+
					"names no session is one an operator cannot act on", want, line)
			}
		})
	}
}

// ---------------------------------------------------------------
// The class guard
// ---------------------------------------------------------------

// sessionNamingExprs are the two ways a call in this package is allowed
// to put the session on an operator line.
//
// logSessionSuffix is the one to reach for. q.logSuffix is the inbox's
// copy of it, which exists because push has no *Agent to ask and a
// back-pointer from a child to its locking parent is not worth one log
// line.
var sessionNamingExprs = map[string]bool{
	"a.logSessionSuffix()": true,
	"q.logSuffix":          true,
}

// rawSessionIDFile is the one file allowed to name its session with the
// raw field instead. The tail-repair notice writes to os.Stderr under
// the CLI's `core-agent:` prefix rather than through log — a second
// idiom, deliberately left alone because it already names its session
// inline and is not part of the `agent:` family.
//
// Scoped to the file rather than added to the map above, because
// `a.sessionID` in the map is a package-wide hole: it satisfies rule 1
// anywhere, and rule 2 only engages on an `agent:`-prefixed literal, so
// `log.Printf("compaction stalled for %s", a.sessionID)` would pass
// both — which is the per-site judgement call #1137 exists to abolish.
const rawSessionIDFile = "tail_repair.go"

// TestNoOperatorLineCanShipWithoutASessionID is the half the table
// cannot be: it covers the line that has not been written yet.
//
// Source-level, and the weakness of that is worth naming — it proves a
// call is WRITTEN correctly, not that the bytes come out right. The
// table above is the half that proves the bytes. Together they cover
// both failure modes; either alone misses one.
func TestNoOperatorLineCanShipWithoutASessionID(t *testing.T) {
	t.Parallel()

	// File by file rather than parser.ParseDir, which is deprecated for
	// ignoring build tags when it groups files into packages. Reading
	// the directory keeps every .go file in scope whatever it is tagged
	// for, which is what a rule about "every line in this package"
	// wants.
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	fset := token.NewFileSet()

	var checked int
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			formatIdx, ok := operatorOutputCall(call)
			if !ok {
				return true
			}
			checked++
			where := name + ":" + strconv.Itoa(fset.Position(call.Pos()).Line)

			// Rule 1, everywhere: the line names its session.
			if !namesASession(fset, call, name) {
				t.Errorf("%s: operator output that names no session.\n"+
					"Add a.logSessionSuffix() — several sessions interleave into "+
					"one daemon log, and a notice nobody can attribute is one "+
					"nobody can act on (#1136, #1137). If this line is genuinely "+
					"about the process and not a session, say so here.", where)
				return true
			}

			// Rule 2, for the `agent:`-prefixed log.Printf family:
			// the id sits in the fixed slot right after the prefix.
			//
			// Needs a literal to read, so a format held in a const or a
			// variable is checked for presence (rule 1) and not for
			// position. Nothing in the package does that today and the
			// census below is what would notice if it started; the
			// honest statement of the guarantee is "every operator line
			// names its session, and every one written as a literal
			// names it in the fixed slot".
			format, ok := leadingStringLit(call.Args[formatIdx])
			if !ok || !strings.HasPrefix(format, "agent:") {
				return true
			}
			if !strings.HasPrefix(format, "agent:%s ") {
				t.Errorf("%s: format starts %q; the session slot is fixed at "+
					"`agent:%%s ` so that every message body stays greppable as "+
					"one contiguous string (#1137). See logSessionSuffix.",
					where, truncFormat(format))
			}
			if got := exprText(fset, call.Args[formatIdx+1]); got != "a.logSessionSuffix()" && got != "q.logSuffix" {
				t.Errorf("%s: first format argument is %s, want the session "+
					"suffix — `agent:%%s ` binds it to that slot.", where, got)
			}
			return true
		})
	}

	// A scanner that matches nothing passes everything, and a floor
	// ("at least ten") tolerates the matcher quietly losing three. An
	// exact count is the only version that notices a site it has
	// stopped recognising, which is the failure a violation scanner is
	// structurally blind to — the same reason
	// verify-harness-config-pinned carries a census.
	//
	// Adding or removing an operator line means updating this number,
	// deliberately: it is one line of churn in exchange for the count
	// being evidence rather than a floor.
	const operatorOutputCalls = 13
	if checked != operatorOutputCalls {
		t.Errorf("scanned %d operator-output calls in pkg/agent, want %d.\n"+
			"If you added or removed a line, update the constant. If you did "+
			"not, operatorOutputCall has stopped recognising a shape the "+
			"package uses and this test is checking less than it claims.",
			checked, operatorOutputCalls)
	}
}

// operatorOutputCall reports whether the call writes an operator-facing
// line, and at which argument the format string sits.
//
// Both shapes the package uses: log.Printf (twelve sites) and
// fmt.Fprintf to os.Stderr (one). Covering the second matters because
// it is the obvious way to add a line while sidestepping a rule written
// only for the first. Fatal/Panic are covered for the same reason and
// are the cheapest accidental evasion of the three — they are the same
// package, one keystroke away, and print the same line.
//
// What this does NOT see, recorded so the guarantee is not read wider
// than it is: a *log.Logger value (`a.logger.Printf`), a wrapper method
// (`a.logf`), `println`, and `os.Stderr.WriteString`. None exist in
// pkg/agent — the package logs through the `log` package and one
// `fmt.Fprintf` — so the rule holds today by a fact about the tree
// rather than by construction. A matcher keyed on a *type* rather than
// on a name would close that, and needs go/types; the census bound at
// the bottom of the test is the cheap guard that notices if the shapes
// here stop covering the package.
func operatorOutputCall(call *ast.CallExpr) (formatIdx int, ok bool) {
	sel, isSel := call.Fun.(*ast.SelectorExpr)
	if !isSel {
		return 0, false
	}
	pkgIdent, isIdent := sel.X.(*ast.Ident)
	if !isIdent {
		return 0, false
	}
	switch {
	case pkgIdent.Name == "log" && isPrintLike(sel.Sel.Name):
		return 0, len(call.Args) > 0
	case pkgIdent.Name == "fmt" && strings.HasPrefix(sel.Sel.Name, "Fprint") && len(call.Args) > 1:
		// Only when the sink is stderr; fmt.Fprintf into a buffer is
		// not an operator line.
		if w, isSel := call.Args[0].(*ast.SelectorExpr); isSel {
			if x, isIdent := w.X.(*ast.Ident); isIdent && x.Name == "os" && w.Sel.Name == "Stderr" {
				return 1, true
			}
		}
	}
	return 0, false
}

// isPrintLike reports whether a name in the `log` package writes a
// line: Print/Printf/Println, and the Fatal and Panic families, which
// write the same line before they exit or unwind.
func isPrintLike(name string) bool {
	return strings.HasPrefix(name, "Print") ||
		strings.HasPrefix(name, "Fatal") ||
		strings.HasPrefix(name, "Panic")
}

// namesASession reports whether any argument of the call is one of the
// approved session-naming expressions, plus the raw field in the one
// file that is allowed it.
func namesASession(fset *token.FileSet, call *ast.CallExpr, file string) bool {
	for _, arg := range call.Args {
		text := exprText(fset, arg)
		if sessionNamingExprs[text] {
			return true
		}
		if text == "a.sessionID" && file == rawSessionIDFile {
			return true
		}
	}
	return false
}

// leadingStringLit returns the value of the leftmost string literal in
// expr, which for a `"a" + "b"` format is the part carrying the prefix.
func leadingStringLit(expr ast.Expr) (string, bool) {
	for {
		switch e := expr.(type) {
		case *ast.BasicLit:
			if e.Kind != token.STRING {
				return "", false
			}
			s, err := strconv.Unquote(e.Value)
			return s, err == nil
		case *ast.BinaryExpr:
			expr = e.X
		case *ast.ParenExpr:
			expr = e.X
		default:
			return "", false
		}
	}
}

// exprText renders an expression back to source so it can be compared
// as a string. Comparing rendered source rather than matching the AST
// by hand keeps the approved list above readable as Go.
func exprText(fset *token.FileSet, expr ast.Expr) string {
	var buf bytes.Buffer
	if err := printer.Fprint(&buf, fset, expr); err != nil {
		return ""
	}
	return buf.String()
}

// By rune, not by byte: every message body in this package contains an
// em dash, so a byte slice would hand the reader mojibake in the middle
// of the failure that is supposed to tell them what is wrong.
func truncFormat(s string) string {
	r := []rune(s)
	if len(r) > 48 {
		return string(r[:48]) + "…"
	}
	return s
}

// ---------------------------------------------------------------
// Rigs
// ---------------------------------------------------------------

// sessionAgent builds an agent on a named session with an event log
// wired, so the paths that write a durable degraded or failure row
// alongside their log line run end to end rather than short-circuiting
// somewhere before the log.
func sessionAgent(t *testing.T, session string, llm adkmodel.LLM, extra ...Option) *Agent {
	t.Helper()
	h, cleanup := openTestEventLog(t)
	t.Cleanup(cleanup)
	createTestSession(t, h, "core-agent", "u", session)

	opts := append([]Option{
		WithEventLog(h),
		WithSession("u", session),
		WithUsageTracker(usage.NewTracker()),
		WithCompactor(NewDefaultCompactor()),
	}, extra...)
	a, err := New(llm, opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}
