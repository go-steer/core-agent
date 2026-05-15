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
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteActivity_WritesAtomically(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	WriteActivity("thinking")

	body, err := os.ReadFile(filepath.Join(home, "agent-info.json"))
	if err != nil {
		t.Fatalf("read agent-info.json: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["activity"] != "thinking" {
		t.Errorf("activity = %v, want thinking", got["activity"])
	}
}

func TestWriteActivity_PreservesOtherFields(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// Pre-populate with a field other than activity — WriteActivity
	// should preserve it and only mutate "activity".
	pre := []byte(`{"agent_name":"scion-agent-test","activity":"working"}`)
	if err := os.WriteFile(filepath.Join(home, "agent-info.json"), pre, 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	WriteActivity("executing")

	body, _ := os.ReadFile(filepath.Join(home, "agent-info.json"))
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["activity"] != "executing" {
		t.Errorf("activity = %v, want executing", got["activity"])
	}
	if got["agent_name"] != "scion-agent-test" {
		t.Errorf("agent_name lost: %v", got["agent_name"])
	}
}

func TestWriteActivity_RespectsStickyState(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// Seed with a sticky state — WriteActivity must not overwrite it.
	for _, sticky := range []string{"waiting_for_input", "blocked", "completed", "limits_exceeded"} {
		t.Run(sticky, func(t *testing.T) {
			pre := []byte(`{"activity":"` + sticky + `"}`)
			if err := os.WriteFile(filepath.Join(home, "agent-info.json"), pre, 0o644); err != nil {
				t.Fatalf("seed: %v", err)
			}

			WriteActivity("thinking")

			body, _ := os.ReadFile(filepath.Join(home, "agent-info.json"))
			var got map[string]any
			_ = json.Unmarshal(body, &got)
			if got["activity"] != sticky {
				t.Errorf("activity = %v, want %s (sticky must not be overwritten)", got["activity"], sticky)
			}
		})
	}
}

func TestWriteActivity_DropsLegacyFields(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	pre := []byte(`{"activity":"working","status":"old","sessionStatus":"old"}`)
	if err := os.WriteFile(filepath.Join(home, "agent-info.json"), pre, 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	WriteActivity("thinking")

	body, _ := os.ReadFile(filepath.Join(home, "agent-info.json"))
	var got map[string]any
	_ = json.Unmarshal(body, &got)
	if _, present := got["status"]; present {
		t.Errorf("legacy 'status' field not dropped: %v", got)
	}
	if _, present := got["sessionStatus"]; present {
		t.Errorf("legacy 'sessionStatus' field not dropped: %v", got)
	}
}

func TestRunStatus_GracefulWithoutSciontool(t *testing.T) {
	// Force a $PATH where sciontool cannot be found.
	t.Setenv("PATH", t.TempDir())

	// Should not panic, exit, or block — just a quiet no-op.
	RunStatus("task_completed", "smoke test")
}

func TestRunStatus_CallsSciontoolBinary(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "sciontool")
	logPath := filepath.Join(dir, "args.log")
	// Stub records its argv (joined by spaces) to a log file we can inspect.
	script := `#!/bin/sh
echo "$@" > "` + logPath + `"
`
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	t.Setenv("PATH", dir)

	RunStatus("ask_user", "what next?")

	body, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("stub did not run: %v", err)
	}
	got := strings.TrimSpace(string(body))
	want := "status ask_user what next?"
	if got != want {
		t.Errorf("sciontool argv = %q, want %q", got, want)
	}
}

func TestIsValidStickyType(t *testing.T) {
	for _, valid := range validStickyTypes {
		if !isValidStickyType(valid) {
			t.Errorf("isValidStickyType(%q) = false, want true", valid)
		}
	}
	for _, bad := range []string{"", "bogus", "ASK_USER", "ask user", "completed"} {
		if isValidStickyType(bad) {
			t.Errorf("isValidStickyType(%q) = true, want false", bad)
		}
	}
}

func TestStatusTool_Constructs(t *testing.T) {
	tl, err := StatusTool()
	if err != nil {
		t.Fatalf("StatusTool: %v", err)
	}
	if tl == nil {
		t.Fatal("StatusTool returned nil tool")
	}
	if got := tl.Name(); got != "sciontool_status" {
		t.Errorf("tool name = %q, want sciontool_status", got)
	}
}
