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

// Package tools provides ADK-side helpers and built-in tool
// implementations for the agent loop:
//   - GateToolset: bridges permissions.Gate to ADK Toolsets (used by mcp + skills)
//   - Built-in tool suite: file I/O, shell, todo tracking, output truncation
//
// All built-in tools share a common shape via
// google.golang.org/adk/tool/functiontool and the helpers in this
// package: output truncation (Truncate) and gate consultation (via
// permissions.Gate). Tool authors define a typed Args + Result pair
// and a handler closing over any dependencies; builtins.go assembles
// them into the slice consumed by agent.WithTools.
package tools

import (
	"fmt"
	"strings"
)

// Truncate caps s at the lower of maxBytes / maxLines. When truncation
// occurs, a marker line is appended so the model knows it received an
// abridged output and can ask for more (e.g. via offset/limit on a
// re-read or a narrower grep).
//
// maxBytes <= 0 means "no byte limit"; same for maxLines.
func Truncate(s string, maxBytes, maxLines int) string {
	if s == "" {
		return s
	}
	truncated := false
	originalBytes := len(s)
	originalLines := -1

	if maxBytes > 0 && len(s) > maxBytes {
		s = s[:maxBytes]
		truncated = true
	}
	if maxLines > 0 {
		lines := strings.SplitAfter(s, "\n")
		if len(lines) > maxLines {
			originalLines = countLines(s) // lower bound; we already byte-truncated
			s = strings.Join(lines[:maxLines], "")
			truncated = true
		}
	}
	if !truncated {
		return s
	}
	marker := fmt.Sprintf("\n... [truncated by core-agent: original size %d bytes", originalBytes)
	if originalLines > 0 {
		marker += fmt.Sprintf(", %d lines", originalLines)
	}
	marker += "; ask with a narrower scope or read in chunks]"
	return s + marker
}

func countLines(s string) int {
	if s == "" {
		return 0
	}
	n := strings.Count(s, "\n")
	if !strings.HasSuffix(s, "\n") {
		n++
	}
	return n
}
