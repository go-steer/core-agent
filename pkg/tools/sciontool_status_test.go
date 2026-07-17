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

package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-steer/core-agent/pkg/config"
	"github.com/go-steer/core-agent/pkg/permissions"
)

func TestSciontoolStatusTool_Constructs(t *testing.T) {
	tl, err := NewSciontoolStatusTool()
	if err != nil {
		t.Fatalf("NewSciontoolStatusTool: %v", err)
	}
	if tl == nil {
		t.Fatal("NewSciontoolStatusTool returned nil tool")
	}
	if got := tl.Name(); got != "sciontool_status" {
		t.Errorf("tool name = %q, want sciontool_status", got)
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

func TestRunSciontoolStatus_GracefulWithoutSciontool(t *testing.T) {
	// Force a $PATH where sciontool cannot be found.
	t.Setenv("PATH", t.TempDir())
	// Should not panic, exit, or block — just a quiet no-op.
	runSciontoolStatus("task_completed", "smoke test")
}

func TestBuild_GatesSciontoolStatusOnPath(t *testing.T) {
	// This test can't call t.Parallel because it mutates $PATH.
	cfg := config.DefaultConfig()
	gate := permissions.New(permissions.Options{Mode: permissions.ModeYolo})

	// PATH without sciontool → the tool must NOT be registered.
	t.Setenv("PATH", t.TempDir())
	reg, err := Build(cfg, gate, "", Default())
	if err != nil {
		t.Fatalf("Build (no sciontool): %v", err)
	}
	for _, tl := range reg.Tools {
		if tl.Name() == "sciontool_status" {
			t.Errorf("sciontool_status registered even though sciontool not on PATH")
		}
	}

	// Now drop a stub sciontool onto PATH → the tool must appear.
	dir := t.TempDir()
	stub := filepath.Join(dir, "sciontool")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	t.Setenv("PATH", dir)
	reg, err = Build(cfg, gate, "", Default())
	if err != nil {
		t.Fatalf("Build (stub sciontool): %v", err)
	}
	found := false
	for _, tl := range reg.Tools {
		if tl.Name() == "sciontool_status" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("sciontool_status not registered even though sciontool IS on PATH")
	}
}

func TestRunSciontoolStatus_CallsSciontoolBinary(t *testing.T) {
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

	runSciontoolStatus("ask_user", "what next?")

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
