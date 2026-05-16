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

// Command inspect-grounding dumps every agent_eventlog row whose
// author starts with "gemini/" from a SQLite eventlog database. Used
// by dev/smoke/03-vertex-grounding.sh to assert the GoogleSearch
// grounding projection landed rows in the eventlog, without
// depending on the sqlite3 CLI being on PATH.
//
//	go run ./dev/smoke/cmd/inspect-grounding /tmp/smoke.db
//
// Output is one line per matching row, suitable for grep / wc -l
// from a shell script.
package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/glebarez/sqlite"

	"github.com/go-steer/core-agent/eventlog"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: inspect-grounding <eventlog.db>")
		os.Exit(2)
	}
	ctx := context.Background()
	h, err := eventlog.Open(ctx, sqlite.Open(os.Args[1]))
	if err != nil {
		fmt.Fprintf(os.Stderr, "open %s: %v\n", os.Args[1], err)
		os.Exit(2)
	}
	// No defer h.Close() — we os.Exit on error paths, which skips
	// defers. Close explicitly when we reach a clean exit instead.
	count := 0
	for entry, err := range h.Stream.Since(ctx, 0) {
		if err != nil {
			_ = h.Close()
			fmt.Fprintf(os.Stderr, "stream: %v\n", err)
			os.Exit(2)
		}
		if !strings.HasPrefix(entry.Event.Author, "gemini/") {
			continue
		}
		text := ""
		if entry.Event.Content != nil && len(entry.Event.Content.Parts) > 0 && entry.Event.Content.Parts[0] != nil {
			text = entry.Event.Content.Parts[0].Text
		}
		fmt.Printf("seq=%d author=%s text=%q\n", entry.Seq, entry.Event.Author, text)
		count++
	}
	fmt.Fprintf(os.Stderr, "matched %d gemini/* rows\n", count)
	_ = h.Close()
}
