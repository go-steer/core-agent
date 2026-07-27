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
	"bytes"
	"io"
)

// NewFilteredLogWriter wraps w in a writer that drops noisy log
// lines from genai/ADK that embedding surfaces don't want exposed.
// Today the only filtered lines are the `Error context canceled` /
// `Error context deadline exceeded` messages from genai's SSE
// scanner, which fire every time a turn is interrupted mid-stream
// (genai/api_client.go:484 log.Printf's it unconditionally).
//
// Anything that isn't filtered passes through to w unchanged, so
// consumer-supplied log lines still appear. Typical use:
//
//	log.SetOutput(compose.NewFilteredLogWriter(os.Stderr))
func NewFilteredLogWriter(w io.Writer) io.Writer {
	return &filteredLogWriter{w: w}
}

// filteredLogWriter drops noisy log lines from genai/ADK that the
// bundled CLI doesn't want to expose.
type filteredLogWriter struct{ w io.Writer }

// droppedLogPatterns is the set of substrings that mark a line for
// filtering. Kept small + literal so we don't accidentally suppress
// something users need to see.
var droppedLogPatterns = [][]byte{
	[]byte("Error context canceled"),
	[]byte("Error context deadline exceeded"),
}

func (f *filteredLogWriter) Write(p []byte) (int, error) {
	for _, pat := range droppedLogPatterns {
		if bytes.Contains(p, pat) {
			// Return the full length so log.Output() doesn't see a
			// short write and retry. The semantic is "consumed".
			return len(p), nil
		}
	}
	return f.w.Write(p)
}
