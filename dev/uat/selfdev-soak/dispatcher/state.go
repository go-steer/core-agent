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
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// state is what the dispatcher must remember across its own restarts:
// the issue it is working on, and the PRs whose working copies wait for a
// merge or close before they are removed. Small, rewritten whole, and
// written atomically.
type state struct {
	Active  *activeIssue `json:"active,omitempty"`
	OpenPRs []openPR     `json:"open_prs,omitempty"`
	// PendingComments are stop comments not yet posted. The stop itself
	// (labels, cleanup) is already done; only the explanation is owed.
	PendingComments []pendingComment `json:"pending_comments,omitempty"`
}

// activeIssue is the claimed issue. SessionPath is empty between the
// claim and the session's creation; a restart in that window starts the
// issue over rather than guess.
type activeIssue struct {
	Number      int    `json:"number"`
	Title       string `json:"title"`
	Upstream    string `json:"upstream"`
	Dir         string `json:"dir"`
	BaseSHA     string `json:"base_sha"`
	SessionPath string `json:"session_path,omitempty"`
	SessionID   string `json:"session_id,omitempty"`
	Injected    bool   `json:"injected,omitempty"`
	// StopReason is set once the dispatcher has decided to stop the
	// issue, before it tells GitHub, so a failed comment or label is
	// retried on the next poll rather than the stop being forgotten.
	StopReason string    `json:"stop_reason,omitempty"`
	StartedAt  time.Time `json:"started_at"`
}

// openPR is a PR the dispatcher opened and still tracks. The session and
// base are kept because the later loops (steps 4-6: CI, review and rebase
// re-wakes) address the same session and push follow-ups onto the same
// branch; A7 itself only uses Issue and PR, to remove the working copy.
type openPR struct {
	Issue       int    `json:"issue"`
	PR          int    `json:"pr"`
	SessionPath string `json:"session_path,omitempty"`
	BaseSHA     string `json:"base_sha,omitempty"`
}

func loadState(path string) (*state, error) {
	raw, err := os.ReadFile(path) // #nosec G304 -- the operator names the state file.
	if errors.Is(err, os.ErrNotExist) {
		return &state{}, nil
	}
	if err != nil {
		return nil, err
	}
	var st state
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, fmt.Errorf("state file %s: %w", path, err)
	}
	return &st, nil
}

func saveState(path string, st *state) error {
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil { // #nosec G703 -- the operator's --state-file.
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".state-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // #nosec G703 -- a temp file this function created.
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		_ = tmp.Close()
		return err
	}
	// fsync the file before the rename and the directory after it: a
	// node crash must leave either the old state or the new one, never an
	// empty file or a rename that did not reach the disk.
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil { // #nosec G703 -- the operator's --state-file.
		return err
	}
	return syncDir(filepath.Dir(path))
}

func syncDir(dir string) error {
	f, err := os.Open(dir) // #nosec G304 G703 -- the --state-file's directory.
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return f.Sync()
}

// pendingComment is a stop comment still owed to an issue.
type pendingComment struct {
	Issue    int    `json:"issue"`
	Body     string `json:"body"`
	Attempts int    `json:"attempts,omitempty"`
}
