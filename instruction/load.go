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

// Package instruction loads project + user "agent memory" files (typically
// AGENTS.md) into the system prompt.
//
// The project root is searched in this order (first match wins):
// AGENTS.md → CLAUDE.md → GEMINI.md. The fallback chain lets the agent
// be dropped into a repo that already has memory authored for an
// Anthropic-flavored or Gemini-flavored agent tool.
//
// The user-global root reads only AGENTS.md; no fallback chain. Both files
// are concatenated, user first, project second, so a per-repo memory file
// can layer on top of a user's personal preferences.
package instruction

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// maxFileBytes caps how much of any single memory file is loaded. 32 KiB
// keeps a sprawling memory file from eating most of the context window
// before the conversation starts.
const maxFileBytes = 32 * 1024

// Project memory filename fallback chain.
var projectMemoryNames = []string{"AGENTS.md", "CLAUDE.md", "GEMINI.md"}

// userMemoryName is the only name read from the user-global root.
const userMemoryName = "AGENTS.md"

// Source records where one piece of loaded memory came from. Used by
// callers (e.g. a /memory slash command) to show provenance.
type Source struct {
	Scope     string // "user" | "project"
	Path      string // absolute path
	Bytes     int    // bytes after truncation
	Truncated bool   // true if the on-disk file exceeded maxFileBytes
}

// Loaded is the result of a Load call. Instruction is the assembled
// text suitable for prepending to the agent's system prompt; Sources
// describes what got included.
type Loaded struct {
	Instruction string
	Sources     []Source
}

// Empty reports whether nothing was loaded.
func (l Loaded) Empty() bool { return l.Instruction == "" }

// Load resolves the project + user memory files and returns the
// concatenated instruction text. Missing files are not errors —
// memory is optional. Other I/O errors (permission denied, etc.) are
// returned so the caller can surface them.
//
// projectRoot may be empty; in that case only user memory is loaded.
// userRoot may be empty in tests.
func Load(projectRoot, userRoot string) (Loaded, error) {
	var loaded Loaded
	var b strings.Builder

	// User memory first, so project memory can override.
	if userRoot != "" {
		path := filepath.Join(userRoot, userMemoryName)
		if src, body, err := readMemory(path, "user"); err != nil {
			return loaded, err
		} else if src != nil {
			loaded.Sources = append(loaded.Sources, *src)
			b.WriteString("# User memory (")
			b.WriteString(path)
			b.WriteString(")\n")
			b.WriteString(body)
			if !strings.HasSuffix(body, "\n") {
				b.WriteByte('\n')
			}
		}
	}

	// Project memory: walk the fallback chain.
	if projectRoot != "" {
		for _, name := range projectMemoryNames {
			path := filepath.Join(projectRoot, name)
			src, body, err := readMemory(path, "project")
			if err != nil {
				return loaded, err
			}
			if src == nil {
				continue
			}
			loaded.Sources = append(loaded.Sources, *src)
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString("# Project memory (")
			b.WriteString(path)
			b.WriteString(")\n")
			b.WriteString(body)
			if !strings.HasSuffix(body, "\n") {
				b.WriteByte('\n')
			}
			break // first match wins
		}
	}

	loaded.Instruction = b.String()
	return loaded, nil
}

// readMemory loads a single memory file. Returns (nil, "", nil) if
// the file does not exist (a normal "no memory at this slot"
// outcome). On other errors returns (nil, "", err).
func readMemory(path, scope string) (*Source, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, "", nil
		}
		return nil, "", fmt.Errorf("instruction: read %s: %w", path, err)
	}
	truncated := false
	if len(data) > maxFileBytes {
		data = data[:maxFileBytes]
		data = append(data, []byte("\n[... truncated by core-agent: file exceeds 32 KiB cap ...]\n")...)
		truncated = true
	}
	abs, _ := filepath.Abs(path)
	return &Source{
		Scope:     scope,
		Path:      abs,
		Bytes:     len(data),
		Truncated: truncated,
	}, string(data), nil
}
