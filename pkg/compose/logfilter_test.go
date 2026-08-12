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

package compose

import (
	"errors"
	"log"
	"strings"
	"testing"
)

func TestFilteredLogWriter(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		line string
		want bool // true == the line reaches the underlying writer
	}{
		{"genai cancellation noise is dropped", "2026/08/12 Error context canceled\n", false},
		{"genai deadline noise is dropped", "2026/08/12 Error context deadline exceeded\n", false},
		{"the pattern matches mid-line, not just at the start", "prefix junk Error context canceled suffix\n", false},
		{"ordinary lines pass through", "2026/08/12 core-agent: attach listener on /tmp/x.sock\n", true},
		// The filter is a substring match on two literals, which is
		// deliberately narrow. A line that merely mentions
		// cancellation must survive — over-filtering silently hides
		// the operator's own diagnostics.
		{"a nearby but different message survives", "core-agent: turn canceled by operator\n", true},
		{"an error about the context package survives", "core-agent: Error contextual retrieval failed\n", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var sink strings.Builder
			w := NewFilteredLogWriter(&sink)

			n, err := w.Write([]byte(tc.line))
			if err != nil {
				t.Fatalf("Write: %v", err)
			}
			// Dropped or not, the writer must claim the full length:
			// log.Output treats a short write as an error and the
			// standard logger would surface it to stderr, turning a
			// suppressed line into a noisier one.
			if n != len(tc.line) {
				t.Errorf("Write returned %d, want %d — a short write makes log.Output report an error", n, len(tc.line))
			}
			if got := sink.String() != ""; got != tc.want {
				t.Errorf("reached underlying writer = %v, want %v (sink: %q)", got, tc.want, sink.String())
			}
			if tc.want && sink.String() != tc.line {
				t.Errorf("passthrough mangled the line: got %q, want %q", sink.String(), tc.line)
			}
		})
	}
}

func TestFilteredLogWriter_EmptyWrite(t *testing.T) {
	t.Parallel()
	var sink strings.Builder
	n, err := NewFilteredLogWriter(&sink).Write(nil)
	if n != 0 || err != nil {
		t.Errorf("Write(nil) = (%d, %v), want (0, nil)", n, err)
	}
}

func TestFilteredLogWriter_PropagatesWriteErrors(t *testing.T) {
	t.Parallel()
	// A passthrough line hitting a broken sink must report the
	// failure; swallowing it would make the filter look like it
	// dropped a line it was supposed to forward.
	sentinel := errors.New("pipe closed")
	w := NewFilteredLogWriter(errWriter{err: sentinel})
	if _, err := w.Write([]byte("core-agent: hello\n")); !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want %v", err, sentinel)
	}
	// A dropped line never touches the sink, so it can't fail.
	if _, err := w.Write([]byte("Error context canceled\n")); err != nil {
		t.Errorf("dropped line returned %v, want nil", err)
	}
}

func TestFilteredLogWriter_ThroughTheStandardLogger(t *testing.T) {
	t.Parallel()
	// The documented use is log.SetOutput(NewFilteredLogWriter(...)),
	// where the logger prepends a timestamp prefix before the writer
	// sees the bytes. Exercise that composition rather than only the
	// bare Write, since a filter anchored at the start of the buffer
	// would pass the unit test above and fail here.
	var sink strings.Builder
	lg := log.New(NewFilteredLogWriter(&sink), "", log.LstdFlags)
	lg.Print("Error context canceled")
	lg.Print("core-agent: still alive")

	out := sink.String()
	if strings.Contains(out, "Error context canceled") {
		t.Errorf("noise survived the standard logger's prefix: %q", out)
	}
	if !strings.Contains(out, "core-agent: still alive") {
		t.Errorf("real log line was dropped: %q", out)
	}
}

type errWriter struct{ err error }

func (e errWriter) Write([]byte) (int, error) { return 0, e.err }
