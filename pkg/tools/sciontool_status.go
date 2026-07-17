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

// sciontool_status is a thin ADK-tool wrapper around Scion's container-
// side `sciontool` CLI. It lets the model signal sticky lifecycle
// events — `ask_user` / `blocked` / `task_completed` / `limits_exceeded`
// — that Scion's hub reads to know whether the agent is waiting on
// input, has finished, etc.
//
// Registration is conditional: builtins.Build only wires this tool
// when `sciontool` is on PATH, so agents outside a Scion container
// don't see a broken tool in their schema. See pkg/tools/builtins.go
// for the pattern.
//
// The transient-activity side of the Scion contract (thinking /
// executing / working) is handled by pkg/hooks + a `sciontool hook`
// command in the operator's config; no in-process code here needs to
// tick agent-info.json.

package tools

import (
	"context"
	"fmt"
	"os/exec"
	"time"

	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
)

// validStickyTypes are the four state names accepted by
// `sciontool status`. Order matters only for the error message.
var validStickyTypes = []string{
	"ask_user",
	"blocked",
	"task_completed",
	"limits_exceeded",
}

// sciontoolStatusToolArgs is the schema the model sees. Field tags
// double as JSON-schema descriptions so write them like prompts.
type sciontoolStatusToolArgs struct {
	StatusType string `json:"status_type" jsonschema:"one of: ask_user, blocked, task_completed, limits_exceeded"`
	Message    string `json:"message" jsonschema:"a brief human-readable description of the event (a question, a reason, or a task summary)"`
}

type sciontoolStatusToolResult struct {
	Status  string `json:"status"`
	Message string `json:"message"`
}

// NewSciontoolStatusTool returns an ADK tool the agent can call to
// signal sticky lifecycle transitions to Scion. Wired into the
// built-in registry by tools.Build when `sciontool` is on PATH; can
// also be constructed directly by out-of-registry consumers.
func NewSciontoolStatusTool() (tool.Tool, error) {
	return functiontool.New(functiontool.Config{
		Name: "sciontool_status",
		Description: "Signal a lifecycle event to Scion. Call ask_user before " +
			"asking the user a question, blocked when waiting on an external " +
			"dependency, task_completed when the task is finished, and " +
			"limits_exceeded when a budget is exhausted.",
	}, func(_ tool.Context, in sciontoolStatusToolArgs) (sciontoolStatusToolResult, error) {
		if !isValidStickyType(in.StatusType) {
			return sciontoolStatusToolResult{}, fmt.Errorf(
				"sciontool_status: invalid status_type %q; must be one of %v",
				in.StatusType, validStickyTypes,
			)
		}
		runSciontoolStatus(in.StatusType, in.Message)
		return sciontoolStatusToolResult{
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

// runSciontoolStatus invokes `sciontool status <statusType> <message>`
// for sticky state transitions. No-op when sciontool isn't on PATH
// (development / testing outside a Scion container).
//
// Bounded by a 10s timeout so a hung sciontool can't stall the agent.
func runSciontoolStatus(statusType, message string) {
	bin, err := exec.LookPath("sciontool")
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// #nosec G204 — bin comes from exec.LookPath; statusType is checked
	// against validStickyTypes by the tool handler before reaching here.
	cmd := exec.CommandContext(ctx, bin, "status", statusType, message)
	_ = cmd.Run()
}
