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

// Package main's sciontool.go is a thin Go wrapper around scion's
// container-side `sciontool` CLI binary (scion/cmd/sciontool/). It does
// not reimplement sciontool — it just speaks the same status-emission
// contract:
//
//  1. Transient activity (thinking/executing/working) is written
//     directly to $HOME/agent-info.json via atomic rename. Cheap and
//     high-frequency.
//  2. Sticky transitions (waiting_for_input/blocked/completed/
//     limits_exceeded) shell out to `sciontool status <type> <message>`
//     so scion's hub gets notified in addition to the local file.
//
// Both paths degrade gracefully when running outside a scion container
// (sciontool not on PATH, no $HOME, etc.) so the same binary is
// runnable for local development without a Scion runtime.
//
// Mirrors scion/examples/adk_scion_agent/sciontool.py one-for-one.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
)

// Sticky activities — once the agent is in one of these states, a
// transient update (thinking/executing/working) must not overwrite it.
// Match the lowercase names emitted by scion's StatusHandler
// (scion/pkg/agent/state).
var stickyActivities = map[string]struct{}{
	"waiting_for_input": {},
	"blocked":           {},
	"completed":         {},
	"limits_exceeded":   {},
}

// Valid sticky status types accepted by `sciontool status …` and by
// the StatusTool ADK tool. Order matters only for error messages.
var validStickyTypes = []string{"ask_user", "blocked", "task_completed", "limits_exceeded"}

// resolveSciontool looks up the sciontool binary on PATH each call.
// We don't cache the result: status emissions are infrequent (per turn,
// not per token) and a fresh lookup keeps the logic trivially testable
// across PATH changes.
func resolveSciontool() string {
	bin, err := exec.LookPath("sciontool")
	if err != nil {
		return ""
	}
	return bin
}

// AgentInfoPath returns the absolute path to agent-info.json for the
// current $HOME. Falls back to /home/scion (scion's default UID) when
// $HOME is unset, matching the Python wrapper's behavior.
func AgentInfoPath() string {
	home := os.Getenv("HOME")
	if home == "" {
		home = "/home/scion"
	}
	return filepath.Join(home, "agent-info.json")
}

// WriteActivity writes a transient activity to agent-info.json via
// temp-file + rename. No-op when the current activity is sticky.
//
// Errors are logged to stderr but never returned — the agent must keep
// running even if the host filesystem rejects writes (e.g. when running
// outside a container where $HOME isn't writable).
func WriteActivity(activity string) {
	path := AgentInfoPath()

	current := readCurrentActivity(path)
	if _, sticky := stickyActivities[current]; sticky {
		return
	}

	existing := map[string]any{}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &existing)
	}
	existing["activity"] = activity
	// Drop legacy keys the Python wrapper also clears.
	delete(existing, "status")
	delete(existing, "sessionStatus")

	body, err := json.Marshal(existing)
	if err != nil {
		fmt.Fprintf(os.Stderr, "scion-agent: marshal agent-info: %v\n", err)
		return
	}
	if err := atomicWrite(path, body, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "scion-agent: write agent-info: %v\n", err)
	}
}

// RunStatus invokes `sciontool status <statusType> <message>` for sticky
// state transitions. No-op when sciontool isn't on PATH (development /
// testing outside a container).
//
// Bounded by a 10s timeout so a hung sciontool can't stall the agent.
func RunStatus(statusType, message string) {
	bin := resolveSciontool()
	if bin == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// #nosec G204 — bin comes from exec.LookPath; statusType is checked
	// against validStickyTypes by StatusTool before reaching here.
	cmd := exec.CommandContext(ctx, bin, "status", statusType, message)
	out, err := cmd.CombinedOutput()
	if err != nil {
		fmt.Fprintf(os.Stderr, "scion-agent: sciontool status %s exited: %v\n%s\n", statusType, err, out)
	}
}

// statusToolArgs is the schema the model sees for the sciontool_status
// ADK tool. Field tags double as JSON-schema descriptions so write them
// like prompts.
type statusToolArgs struct {
	StatusType string `json:"status_type" jsonschema:"one of: ask_user, blocked, task_completed, limits_exceeded"`
	Message    string `json:"message" jsonschema:"a brief human-readable description of the event (a question, a reason, or a task summary)"`
}

type statusToolResult struct {
	Status  string `json:"status"`
	Message string `json:"message"`
}

// StatusTool returns an ADK tool the agent can call to signal sticky
// lifecycle transitions to scion. Pair with WriteActivity-driven
// transient updates from the run loop.
func StatusTool() (tool.Tool, error) {
	return functiontool.New(functiontool.Config{
		Name: "sciontool_status",
		Description: "Signal a lifecycle event to scion. Call ask_user before " +
			"asking the user a question, blocked when waiting on an external " +
			"dependency, task_completed when the task is finished, and " +
			"limits_exceeded when a budget is exhausted.",
	}, func(_ tool.Context, in statusToolArgs) (statusToolResult, error) {
		if !isValidStickyType(in.StatusType) {
			return statusToolResult{}, fmt.Errorf(
				"sciontool_status: invalid status_type %q; must be one of %v",
				in.StatusType, validStickyTypes,
			)
		}
		RunStatus(in.StatusType, in.Message)
		return statusToolResult{
			Status:  "success",
			Message: fmt.Sprintf("Reported %s: %s", in.StatusType, in.Message),
		}, nil
	})
}

// isValidStickyType returns true when t matches one of the four sticky
// state names accepted by `sciontool status`.
func isValidStickyType(t string) bool {
	for _, v := range validStickyTypes {
		if v == t {
			return true
		}
	}
	return false
}

// readCurrentActivity returns the "activity" field from agent-info.json
// or "" when the file is missing / unreadable / lacks the field.
func readCurrentActivity(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var obj map[string]any
	if err := json.Unmarshal(data, &obj); err != nil {
		return ""
	}
	if v, ok := obj["activity"].(string); ok {
		return v
	}
	return ""
}

// atomicWrite writes data to path via temp + rename so a concurrent
// reader can never see a half-written file.
func atomicWrite(path string, data []byte, mode fs.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "agent-info-*.tmp")
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
	return os.Rename(tmpName, path)
}
