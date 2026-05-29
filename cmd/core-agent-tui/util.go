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
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/go-steer/core-agent/pkg/attach"
)

// streamRegistry is a process-global map of session-path → live SSE
// channel. The chat model's tea.Cmd loop relies on this to "resume"
// the in-flight stream between Update cycles: subscribeCmd opens the
// stream and stores the channel here; nextFrameCmd pops the next
// frame from it. One TUI process attaches to one session at a time
// in v1, so this map has at most one entry, but keeping it map-keyed
// makes the multi-session-tabs feature additive later.
var streamRegistry = newStreamReg()

type streamReg struct {
	mu sync.Mutex
	m  map[string]<-chan attach.Frame
}

func newStreamReg() *streamReg {
	return &streamReg{m: map[string]<-chan attach.Frame{}}
}

func (r *streamReg) set(path string, ch <-chan attach.Frame) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.m[path] = ch
}

func (r *streamReg) get(path string) <-chan attach.Frame {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.m[path]
}

func (r *streamReg) clear(path string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.m, path)
}

// writeTranscript dumps the chat scrollback to a markdown file. Tool
// calls render as fenced sections; model text as plain markdown; user
// lines as quoted blocks. Best-effort formatting — the file should be
// readable in any markdown viewer.
func writeTranscript(path string, events []chatEvent) error {
	var sb strings.Builder
	for _, e := range events {
		switch e.kind {
		case "user":
			sb.WriteString("> **user:** " + e.body + "\n\n")
		case "asst":
			sb.WriteString("**asst:**\n\n")
			sb.WriteString(e.body + "\n\n")
		case "tool":
			sb.WriteString("`tool: " + e.meta + "`\n\n")
		case "system":
			sb.WriteString("_" + e.body + "_\n\n")
		case "error":
			sb.WriteString("> **error:** " + e.body + "\n\n")
		}
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
		return fmt.Errorf("write transcript: %w", err)
	}
	return nil
}
