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
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"

	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
)

// Trimmed mirror of extras/scion-agent/sciontool.go — just the
// sticky-status tool. The orchestrator binary doesn't push transient
// activity updates (those make sense for a long-running interactive
// agent, less so for a demo that's expected to complete in minutes);
// when sciontool isn't on PATH the call is a no-op.

var validStickyTypes = []string{"ask_user", "blocked", "task_completed", "limits_exceeded"}

type scionStatusArgs struct {
	StatusType string `json:"status_type" jsonschema:"one of: ask_user, blocked, task_completed, limits_exceeded"`
	Message    string `json:"message" jsonschema:"a brief human-readable description of the event"`
}

type scionStatusResult struct {
	Status  string `json:"status"`
	Message string `json:"message"`
}

func newScionStatusTool() (tool.Tool, error) {
	return functiontool.New(functiontool.Config{
		Name: "sciontool_status",
		Description: "Signal a lifecycle event to scion. Call ask_user before " +
			"asking the user a question, blocked when waiting on an external " +
			"dependency, task_completed when the task is finished, and " +
			"limits_exceeded when a budget is exhausted.",
	}, func(_ tool.Context, in scionStatusArgs) (scionStatusResult, error) {
		if !validStickyType(in.StatusType) {
			return scionStatusResult{}, fmt.Errorf(
				"sciontool_status: invalid status_type %q; must be one of %v",
				in.StatusType, validStickyTypes,
			)
		}
		runStatus(in.StatusType, in.Message)
		return scionStatusResult{
			Status:  "success",
			Message: fmt.Sprintf("Reported %s: %s", in.StatusType, trimSpace(in.Message)),
		}, nil
	})
}

func validStickyType(t string) bool {
	for _, v := range validStickyTypes {
		if v == t {
			return true
		}
	}
	return false
}

// runStatus shells out to scion's container-side `sciontool` binary
// when available. Outside a Scion container this is a no-op so the
// same binary runs cleanly during local development.
func runStatus(statusType, message string) {
	bin, err := exec.LookPath("sciontool")
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// #nosec G204 — bin comes from exec.LookPath; statusType is
	// validated against validStickyTypes before reaching here.
	cmd := exec.CommandContext(ctx, bin, "status", statusType, message)
	if out, err := cmd.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "research-orchestrator: sciontool status %s exited: %v\n%s\n", statusType, err, out)
	}
}
