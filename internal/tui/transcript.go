// Copyright 2026 The Cogo Authors.
// SPDX-License-Identifier: Apache-2.0

// Transcript persistence for the in-process TUI. Lifted from cogo's
// internal/session/transcript.go and inlined into the tui package so
// there's no separate single-consumer package. When a project has a
// .agents/ directory configured, every TUI exit writes a JSON
// transcript to .agents/sessions/<RFC3339-timestamp>.json containing
// the chat history and the final usage totals. Atomic write
// (temp + rename). The schema is versioned for safe forward evolution.

package tui

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

// TranscriptSchemaVersion is the on-disk schema version for transcripts.
const TranscriptSchemaVersion = 1

// Transcript captures one TUI session for archival.
type Transcript struct {
	Version   int             `json:"version"`
	StartedAt time.Time       `json:"started_at"`
	EndedAt   time.Time       `json:"ended_at"`
	Model     string          `json:"model"`
	Messages  []TranscriptMsg `json:"messages"`
	Usage     TranscriptUsage `json:"usage"`
}

// TranscriptMsg is one entry in the chat. Role is "user" | "assistant" |
// "system" | "error" — matches the TUI's RoleUser/Assistant/System/Error
// values rendered as their lowercase names so external tools can read
// the file without depending on the package's enum.
type TranscriptMsg struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

// TranscriptUsage mirrors usage.Totals, written by value so the
// transcript doesn't need to import usage.
type TranscriptUsage struct {
	Turns        int     `json:"turns"`
	InputTokens  int     `json:"input_tokens"`
	OutputTokens int     `json:"output_tokens"`
	CostUSD      float64 `json:"cost_usd"`
}

// transcriptSessionsDir is the subdirectory under .agents/ where
// transcripts land. Created on demand.
const transcriptSessionsDir = "sessions"

// saveTranscriptFile writes t to <agentsDir>/sessions/<timestamp>.json
// atomically. Empty agentsDir is a no-op (no project root → no place
// to write). Returns the absolute path of the new file (or "" when
// skipped).
func saveTranscriptFile(agentsDir string, t Transcript) (string, error) {
	if agentsDir == "" {
		return "", nil
	}
	if t.Version == 0 {
		t.Version = TranscriptSchemaVersion
	}
	if t.EndedAt.IsZero() {
		t.EndedAt = time.Now()
	}
	dir := filepath.Join(agentsDir, transcriptSessionsDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("transcript: mkdir %s: %w", dir, err)
	}

	name := transcriptFileName(t.StartedAt)
	path := filepath.Join(dir, name)
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return "", fmt.Errorf("transcript: marshal: %w", err)
	}
	data = append(data, '\n')
	if err := atomicWriteTranscript(path, data, 0o644); err != nil {
		return "", err
	}
	abs, _ := filepath.Abs(path)
	return abs, nil
}

// transcriptFileName returns a filesystem-safe RFC3339-style timestamp
// suitable for sorting chronologically.
func transcriptFileName(started time.Time) string {
	if started.IsZero() {
		started = time.Now()
	}
	// Replace ':' with '-' so the filename is portable across
	// filesystems (Windows in particular rejects ':').
	return strings.ReplaceAll(started.UTC().Format(time.RFC3339), ":", "-") + ".json"
}

// atomicWriteTranscript writes data to path via a temp-file + rename
// so a partial write doesn't leave a corrupt transcript on disk.
func atomicWriteTranscript(path string, data []byte, mode fs.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".core-agent-transcript-*")
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
		return fmt.Errorf("transcript: rename: %w", err)
	}
	return nil
}

// ErrNoProject is returned (via wrap) when caller asks for a save
// but no project directory is configured.
var ErrNoProject = errors.New("transcript: no project directory configured")
