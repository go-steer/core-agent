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

package mock

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"os"
	"sync"

	adkmodel "google.golang.org/adk/model"

	"github.com/go-steer/core-agent/recording"
)

// scriptedLLM replays a sequence of recording.RecordedTurns. Each
// call to GenerateContent advances a cursor and yields the responses
// that were captured for that turn. The script is exhausted when
// more calls arrive than there are recorded turns, and the next
// yield surfaces a clear error rather than a silent empty stream.
//
// In strict mode, each incoming request's Contents must JSON-equal
// the recorded request's Contents — that catches regressions in how
// the agent assembles its prompt without depending on tool decls or
// other Config drift.
type scriptedLLM struct {
	mu     sync.Mutex
	turns  []recording.RecordedTurn
	cursor int
	strict bool
}

func (l *scriptedLLM) Name() string { return "scripted" }

func (l *scriptedLLM) GenerateContent(_ context.Context, req *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		l.mu.Lock()
		if l.cursor >= len(l.turns) {
			n := l.cursor
			l.mu.Unlock()
			yield(nil, fmt.Errorf("scripted: script exhausted at turn %d (no more recorded responses)", n))
			return
		}
		turn := l.turns[l.cursor]
		l.cursor++
		idx := l.cursor - 1
		l.mu.Unlock()

		if l.strict {
			if err := compareContents(turn.Request, req); err != nil {
				yield(nil, fmt.Errorf("scripted: strict mismatch on turn %d: %w", idx, err))
				return
			}
		}
		for _, resp := range turn.Responses {
			if !yield(resp, nil) {
				return
			}
		}
	}
}

// loadScript parses a JSONL file where each non-blank line is a
// single recording.RecordedTurn. Comment lines starting with "#" are
// tolerated so consumers can hand-edit fixtures.
func loadScript(path string) ([]recording.RecordedTurn, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	defer f.Close()
	return decodeScript(f, path)
}

func decodeScript(r io.Reader, source string) ([]recording.RecordedTurn, error) {
	var out []recording.RecordedTurn
	sc := bufio.NewScanner(r)
	// Allow long lines — recorded turns can be large.
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for line := 1; sc.Scan(); line++ {
		raw := bytes.TrimSpace(sc.Bytes())
		if len(raw) == 0 || raw[0] == '#' {
			continue
		}
		var t recording.RecordedTurn
		if err := json.Unmarshal(raw, &t); err != nil {
			return nil, fmt.Errorf("%s: line %d: %w", source, line, err)
		}
		out = append(out, t)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("%s: scan: %w", source, err)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s: no turns found", source)
	}
	return out, nil
}

// compareContents reports whether the recorded and incoming request
// have the same Contents (the message history). Config is ignored on
// purpose — tool declarations legitimately drift as the agent's tool
// registry evolves.
func compareContents(recorded, incoming *adkmodel.LLMRequest) error {
	var rec, inc []byte
	var err error
	if recorded != nil {
		rec, err = json.Marshal(recorded.Contents)
		if err != nil {
			return fmt.Errorf("marshal recorded: %w", err)
		}
	}
	if incoming != nil {
		inc, err = json.Marshal(incoming.Contents)
		if err != nil {
			return fmt.Errorf("marshal incoming: %w", err)
		}
	}
	if !bytes.Equal(rec, inc) {
		return fmt.Errorf("contents differ:\n  recorded: %s\n  incoming: %s", rec, inc)
	}
	return nil
}
