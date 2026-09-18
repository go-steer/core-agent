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

// The recorded-session corpus (#655's second acceptance criterion).
//
// Every other test in this package replays a shape reconstructed from
// an issue. That is the right way to test a detector against the
// failure it was built for, and it is structurally incapable of testing
// the other half of a detector's contract: that it does NOT fire on
// ordinary work. A reconstruction can only contain what its author
// thought ordinary work looked like, which is the same belief the
// threshold was chosen from.
//
// testdata/corpus/gke-drill.jsonl is sixty-four real sessions — every
// archived run of the GKE drill against a live cluster, all of them
// judged good at the time — distilled to exactly what the default
// signals read. dev/tools/extract-watchdog-corpus produced it and its
// header documents the distillation and the redaction.
//
// So this is a false-positive corpus, and the asymmetry is deliberate:
// positive cases we can write from an issue; "and it does not fire on
// real work" is only knowable from real work. What the corpus does NOT
// contain is a recorded runaway, because the drill archives runs and
// none of the archived runs looped. The mechanism for that is in place
// rather than promised — a trace carries expect_silent, and a future
// extraction that catches a real loop flips the flag and lists the
// signals that should fire.

package watchdog_test

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/watchdog"
)

const corpusPath = "testdata/corpus/gke-drill.jsonl"

// corpusObservation is one step of a recorded session. Kind is one of
// call, result, text, turn; the other fields are populated per kind.
// See the extractor for what each one is distilled from.
type corpusObservation struct {
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	Args    string `json:"args"`
	Failed  bool   `json:"failed"`
	NoOp    bool   `json:"no_op"`
	Payload string `json:"payload"`
	Chars   int    `json:"chars"`
}

type corpusTrace struct {
	ID           string              `json:"id"`
	ExpectSilent bool                `json:"expect_silent"`
	Signals      []string            `json:"expect_signals"`
	Observations []corpusObservation `json:"observations"`
}

func loadCorpus(t *testing.T) []corpusTrace {
	t.Helper()
	f, err := os.Open(corpusPath)
	if err != nil {
		t.Fatalf("open corpus: %v", err)
	}
	defer f.Close()
	var out []corpusTrace
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for line := 1; sc.Scan(); line++ {
		raw := strings.TrimSpace(sc.Text())
		if raw == "" {
			continue
		}
		var tr corpusTrace
		if err := json.Unmarshal([]byte(raw), &tr); err != nil {
			t.Fatalf("%s:%d: %v", corpusPath, line, err)
		}
		if tr.ID == "" {
			t.Fatalf("%s:%d: trace has no id", corpusPath, line)
		}
		out = append(out, tr)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("read corpus: %v", err)
	}
	if len(out) == 0 {
		t.Fatalf("%s is empty", corpusPath)
	}
	return out
}

// replay drives one recorded trace through a watchdog and returns
// everything it alerted, in order.
//
// The feed mirrors pkg/agent's tap: calls and results go to the two
// observer entry points, text goes to the third, and a turn marker
// announces a boundary for the signals that scope their memory to one.
// Assistant prose was recorded as a length only (see the extractor), so
// a string of that length stands in — every signal reads text for
// presence, not content.
func replay(w *watchdog.DefaultWatchdog, tr corpusTrace) []watchdog.Alert {
	var got []watchdog.Alert
	drain := func() { got = append(got, w.Check()...) }
	for _, o := range tr.Observations {
		switch o.Kind {
		case "turn":
			w.ObserveTurnStart()
		case "call":
			w.ObserveToolCall(watchdog.ToolCall{Name: o.Name, Args: o.Args})
		case "result":
			r := watchdog.ToolResult{Name: o.Name, NoOp: o.NoOp, Digest: o.Payload}
			if o.Failed {
				r.Error = "recorded tool error"
			}
			w.ObserveToolResult(r)
		case "text":
			w.ObserveAssistantText(strings.Repeat("x", max(o.Chars, 1)))
		}
		drain()
	}
	return got
}

// TestRecordedSessionsDoNotTripTheDefaultSet is the false-positive
// gate. A default signal alerting on a session we shipped as good is a
// threshold regression, and it is the failure mode none of the
// hand-written tests in this package can see.
func TestRecordedSessionsDoNotTripTheDefaultSet(t *testing.T) {
	t.Parallel()

	for _, tr := range loadCorpus(t) {
		t.Run(tr.ID, func(t *testing.T) {
			t.Parallel()
			got := replay(watchdog.NewDefaultWatchdog(), tr)

			if tr.ExpectSilent {
				if len(got) != 0 {
					t.Fatalf("recorded good session alerted: %+v\n"+
						"Either a threshold got tighter than real work, or this run really "+
						"did loop — in which case set expect_silent:false and expect_signals "+
						"on its line in %s.", got, corpusPath)
				}
				return
			}
			// A recorded runaway: every named signal must fire.
			for _, want := range tr.Signals {
				found := false
				for _, a := range got {
					if a.Signal == want {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("%s did not fire on a recorded runaway; got %+v", want, got)
				}
			}
			if len(tr.Signals) == 0 {
				t.Fatalf("trace is expect_silent:false but names no signals in expect_signals")
			}
		})
	}
}

// TestDefaultToolsWithoutTextClearsTheRecordedCorpus is the threshold's
// justification, kept as an assertion rather than a sentence.
//
// The design doc guessed 15 for this signal in a table headed "initial
// guess". The corpus says what real sessions do, and the constant was
// set from it — so the derivation has to be re-checkable, or the next
// person to touch either one has only a comment to go on.
//
// The margin is asserted in both directions. Too low and the signal
// fires on work we shipped; far too high and it is decoration, since a
// threshold no recorded session comes within a factor of is one nothing
// will ever reach.
func TestDefaultToolsWithoutTextClearsTheRecordedCorpus(t *testing.T) {
	t.Parallel()

	longest, where := 0, ""
	for _, tr := range loadCorpus(t) {
		if !tr.ExpectSilent {
			continue // a recorded runaway is supposed to exceed it
		}
		run := 0
		for _, o := range tr.Observations {
			switch o.Kind {
			case "call":
				run++
				if run > longest {
					longest, where = run, tr.ID
				}
			case "text":
				run = 0
			}
		}
	}

	if longest == 0 {
		t.Fatal("no tool calls in the corpus; the measurement below is vacuous")
	}
	if watchdog.DefaultToolsWithoutText <= longest {
		t.Errorf("DefaultToolsWithoutText = %d, but recorded good session %s ran %d tool calls "+
			"without assistant text — the shipped default would alert on work we judged fine",
			watchdog.DefaultToolsWithoutText, where, longest)
	}
	if watchdog.DefaultToolsWithoutText > 3*longest {
		t.Errorf("DefaultToolsWithoutText = %d against a recorded maximum of %d (%s): a threshold "+
			"no real session comes near is decoration, not a signal",
			watchdog.DefaultToolsWithoutText, longest, where)
	}
	t.Logf("longest textless tool-call run across the recorded corpus: %d (%s); threshold %d",
		longest, where, watchdog.DefaultToolsWithoutText)
}

// TestCorpusIsWhatItClaimsToBe guards the fixture itself. A corpus that
// silently lost its calls — a changed archive layout, a broken
// extraction — would make every assertion above pass by being empty,
// which is the one way a false-positive gate can fail open.
func TestCorpusIsWhatItClaimsToBe(t *testing.T) {
	t.Parallel()

	traces := loadCorpus(t)
	if len(traces) < 20 {
		t.Errorf("corpus has %d traces; too few to say anything about false positives", len(traces))
	}
	kinds := map[string]int{}
	seen := map[string]bool{}
	for _, tr := range traces {
		if seen[tr.ID] {
			t.Errorf("duplicate trace id %q", tr.ID)
		}
		seen[tr.ID] = true
		calls := 0
		for _, o := range tr.Observations {
			kinds[o.Kind]++
			switch o.Kind {
			case "call":
				calls++
				if o.Name == "" {
					t.Errorf("%s: a call with no tool name", tr.ID)
				}
			case "result":
				if o.Name == "" {
					t.Errorf("%s: a result with no tool name", tr.ID)
				}
			}
		}
		if calls == 0 {
			t.Errorf("%s: no tool calls, so it exercises no signal", tr.ID)
		}
	}
	for _, kind := range []string{"call", "result", "text", "turn"} {
		if kinds[kind] == 0 {
			t.Errorf("no %q observations in the corpus; the extraction dropped a kind", kind)
		}
	}
	// The redaction. Hashed argument blobs and payload keys are what
	// keeps the drill's project, cluster and namespace out of the tree;
	// a future extractor that stopped hashing would put them back.
	for _, tr := range traces {
		for _, o := range tr.Observations {
			for _, v := range []string{o.Args, o.Payload} {
				if v == "" {
					continue
				}
				if !isOpaqueKey(v) {
					t.Errorf("%s: %q is not an opaque key — the extraction stopped redacting", tr.ID, v)
				}
			}
		}
	}
}

func isOpaqueKey(s string) bool {
	if len(s) != 16 {
		return false
	}
	for _, r := range s {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return false
		}
	}
	return true
}
