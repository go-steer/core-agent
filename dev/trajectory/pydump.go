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

package trajectory

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// pyDumps renders raw JSON the way Python's json.dumps would, up to
// `limit` characters, and stops there.
//
// This exists for one reason: score.py decides "suspect" by scanning
// `json.dumps(payload).lower()[:200]`, so a 200-character window makes
// the ENCODING part of the rule. Encode the same payload differently and
// the cut lands somewhere else, and the two tools start reporting
// different tool-failure counts for one transcript — the #694 failure
// with an audit trail attached.
//
// Go's encoding/json differs from Python's three ways, all of which move
// the cut:
//
//   - Separators. Python writes ", " and ": "; Go writes "," and ":".
//     Two characters per field, so Go's window reaches DEEPER into the
//     payload. This is not theoretical and it is not a tie-breaker: it
//     is the whole disagreement on this corpus. Run
//     20260909T232522Z-c's step 12 is a pod listing whose STATUS column
//     reads "Error" about 180 characters in — inside Go's window, past
//     the end of Python's. With compact separators this package calls
//     that step suspect and score.py calls it ok.
//   - Key order. Go sorts a map's keys; Python keeps the order they were
//     parsed in. No run in the archive is decided by this today (the gke
//     payloads happen to arrive alphabetically, so sorting is a no-op),
//     but nothing keeps a tool from adding a key that lands out of
//     order, and the failure would be silent.
//   - Non-ASCII. Python escapes it as \uXXXX by default; Go emits UTF-8.
//     One accented character is 1 byte of window in Go and 6 in Python.
//
// Verified against CPython 3.12's json.dumps on the cases in
// TestPyDumpsMatchesPythonJSONDumps, and end to end against score.py's
// own numbers on all fifteen archived drill runs.
func pyDumps(raw json.RawMessage, limit int) string {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()

	// One frame per open container: whether it is an object, and how
	// many items it has emitted (an object item is key AND value).
	type level struct {
		object bool
		items  int
	}
	var (
		b        strings.Builder
		stack    []level
		afterKey bool
	)

	// sep writes the ", " that precedes every item after the first. The
	// token following a key is not an item — ": " already separated it.
	sep := func() {
		if afterKey {
			afterKey = false
			return
		}
		if len(stack) == 0 {
			return
		}
		top := &stack[len(stack)-1]
		if top.items > 0 {
			b.WriteString(", ")
		}
		top.items++
	}

	for b.Len() < limit {
		tok, err := dec.Token()
		if err != nil {
			break
		}

		if d, ok := tok.(json.Delim); ok {
			if d == '{' || d == '[' {
				sep()
				b.WriteRune(rune(d))
				stack = append(stack, level{object: d == '{'})
			} else {
				b.WriteRune(rune(d))
				if len(stack) > 0 {
					stack = stack[:len(stack)-1]
				}
			}
			continue
		}

		if len(stack) > 0 && stack[len(stack)-1].object && !afterKey {
			sep()
			writePyScalar(&b, tok)
			b.WriteString(": ")
			afterKey = true
			continue
		}
		sep()
		writePyScalar(&b, tok)
	}

	s := b.String()
	if len(s) > limit {
		s = s[:limit]
	}
	return s
}

func writePyScalar(b *strings.Builder, tok any) {
	switch v := tok.(type) {
	case string:
		writePyString(b, v)
	case json.Number:
		b.WriteString(pyNumber(v))
	case bool:
		if v {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case nil:
		b.WriteString("null")
	default:
		fmt.Fprint(b, v)
	}
}

// pyNumber renders a JSON number as Python would after a round trip
// through json.loads. An integer literal survives verbatim; anything
// with a fraction or an exponent becomes a float and is re-rendered from
// its VALUE, so 1e3 comes back as 1000.0 rather than as it was written.
func pyNumber(n json.Number) string {
	s := n.String()
	if !strings.ContainsAny(s, ".eE") {
		return s
	}
	f, err := n.Float64()
	if err != nil {
		return s
	}
	// Python's repr switches to exponent notation outside this band;
	// inside it, an integral float still carries its ".0".
	if abs := math.Abs(f); f != 0 && (abs < 1e-4 || abs >= 1e16) {
		return strconv.FormatFloat(f, 'e', -1, 64)
	}
	out := strconv.FormatFloat(f, 'f', -1, 64)
	if !strings.Contains(out, ".") {
		out += ".0"
	}
	return out
}

// writePyString escapes as Python's json.dumps does with its default
// ensure_ascii=True: ASCII printables verbatim, the short escapes for
// the six characters that have them, and \uXXXX for everything else
// including all non-ASCII.
func writePyString(b *strings.Builder, s string) {
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			switch {
			case r >= 0x20 && r < 0x7f:
				b.WriteRune(r)
			case r > 0xffff:
				// Python emits a surrogate pair.
				r -= 0x10000
				fmt.Fprintf(b, `\u%04x\u%04x`, 0xd800+(r>>10), 0xdc00+(r&0x3ff))
			default:
				fmt.Fprintf(b, `\u%04x`, r)
			}
		}
	}
	b.WriteByte('"')
}
