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

// Package session writes session transcripts to disk on exit.
//
// Every run that has a project .agents/ directory persists a JSON
// transcript to .agents/sessions/<RFC3339-timestamp>.json containing
// the chat history and the final usage totals. The schema is
// versioned so future readers can evolve safely.
package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// SchemaVersion is the on-disk schema version for transcripts.
const SchemaVersion = 1

// Transcript captures one session for archival.
type Transcript struct {
	Version   int       `json:"version"`
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at"`
	Model     string    `json:"model"`
	Messages  []Message `json:"messages"`
	Usage     Usage     `json:"usage"`
}

// Message is one entry in the chat. Role is "user" | "assistant" |
// "system" | "error".
type Message struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

// Usage mirrors usage.Totals, written by value so the transcript
// doesn't need to import the usage package.
type Usage struct {
	Turns        int     `json:"turns"`
	InputTokens  int     `json:"input_tokens"`
	OutputTokens int     `json:"output_tokens"`
	CostUSD      float64 `json:"cost_usd"`
}

// SessionsDirName is the subdirectory under .agents/ where transcripts
// land. Created on demand.
const SessionsDirName = "sessions"

// Save writes t to <agentsDir>/sessions/<timestamp>.json atomically.
// Empty agentsDir is a no-op (no project root → no place to write).
//
// Returns the absolute path of the new file (or "" when skipped).
func Save(agentsDir string, t Transcript) (string, error) {
	if agentsDir == "" {
		return "", nil
	}
	if t.Version == 0 {
		t.Version = SchemaVersion
	}
	if t.EndedAt.IsZero() {
		t.EndedAt = time.Now()
	}
	dir := filepath.Join(agentsDir, SessionsDirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("session: mkdir %s: %w", dir, err)
	}

	name := transcriptFileName(t.StartedAt)
	path := filepath.Join(dir, name)
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return "", fmt.Errorf("session: marshal: %w", err)
	}
	data = append(data, '\n')
	if err := atomicWrite(path, data, 0o644); err != nil {
		return "", err
	}
	abs, _ := filepath.Abs(path)
	return abs, nil
}

func transcriptFileName(started time.Time) string {
	if started.IsZero() {
		started = time.Now()
	}
	return strings.ReplaceAll(started.UTC().Format(time.RFC3339), ":", "-") + ".json"
}

func atomicWrite(path string, data []byte, mode fs.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".core-agent-session-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("session: rename: %w", err)
	}
	return nil
}

// ErrNoProject is a sentinel callers can check via errors.Is when they
// want to distinguish "no project" from a real failure.
var ErrNoProject = errors.New("session: no project directory configured")
