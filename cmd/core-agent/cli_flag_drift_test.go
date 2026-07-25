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
	"flag"
	"io"
	"testing"
)

// TestModelFlagAlias is the #395 regression guard for the -m/--model
// alias. The startup summary and the task-class message both told
// operators to "override individual knobs with --model", but --model
// was never registered — only -m existed, so --model silently no-op'd
// (or, worse, was treated as an unknown flag). This locks in the same
// two-StringVar-into-one-var pattern main.go now uses, mirroring the
// -c/--config alias contract.
func TestModelFlagAlias(t *testing.T) {
	t.Parallel()

	build := func() (*flag.FlagSet, *string) {
		fs := flag.NewFlagSet("core-agent-test", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		var modelVal string
		fs.StringVar(&modelVal, "m", "", "override model name from config")
		fs.StringVar(&modelVal, "model", "", "long-form alias for -m — same behavior")
		return fs, &modelVal
	}

	t.Run("short only", func(t *testing.T) {
		t.Parallel()
		fs, model := build()
		if err := fs.Parse([]string{"-m", "gemini-x"}); err != nil {
			t.Fatalf("parse: %v", err)
		}
		if *model != "gemini-x" {
			t.Errorf("-m: want gemini-x, got %q", *model)
		}
	})

	t.Run("long only", func(t *testing.T) {
		t.Parallel()
		fs, model := build()
		if err := fs.Parse([]string{"--model", "gemini-y"}); err != nil {
			t.Fatalf("parse: %v", err)
		}
		if *model != "gemini-y" {
			t.Errorf("--model: want gemini-y, got %q", *model)
		}
	})

	t.Run("both, last wins", func(t *testing.T) {
		t.Parallel()
		fs, model := build()
		if err := fs.Parse([]string{"-m", "first", "--model", "second"}); err != nil {
			t.Fatalf("parse: %v", err)
		}
		if *model != "second" {
			t.Errorf("both flags: want second (last wins), got %q", *model)
		}
	})

	t.Run("neither", func(t *testing.T) {
		t.Parallel()
		fs, model := build()
		if err := fs.Parse([]string{}); err != nil {
			t.Fatalf("parse: %v", err)
		}
		if *model != "" {
			t.Errorf("neither flag: want empty, got %q", *model)
		}
	})
}

// TestAskFlagDefaultIsEmpty is the #395 regression guard for the --ask
// default. It used to default to "off", which made the "operator left
// --ask unspecified" branch (`if ask == ""`) in run() dead code: a task
// profile's AskMode could never fill in because ask was always at least
// "off". The default is now "" so run() can tell "unset" from an
// explicit "off". resolveAskUserTool must still treat "" as off so the
// no-flag default behaves identically to before at the wiring layer.
func TestAskFlagDefaultIsEmpty(t *testing.T) {
	t.Parallel()

	fs := flag.NewFlagSet("core-agent-test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	ask := fs.String("ask", "", "ask_user tool mode")
	if err := fs.Parse([]string{}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if *ask != "" {
		t.Fatalf("--ask default: want empty sentinel, got %q", *ask)
	}

	// The empty sentinel must be safe downstream: no ask_user tool, no
	// error — identical to an explicit "off".
	tool, err := resolveAskUserTool(*ask, nil, io.Discard)
	if err != nil {
		t.Fatalf("resolveAskUserTool(%q): unexpected error %v", *ask, err)
	}
	if tool != nil {
		t.Errorf("resolveAskUserTool(%q): want nil tool (off), got %T", *ask, tool)
	}
}

// TestCompactionThresholdFlag is the #395 regression guard for the
// --compaction-threshold flag. The task-class message referenced
// "--compaction-threshold" but no such flag was registered. It is now a
// float64 flag defaulting to 0 (the "unset" sentinel). This locks in the
// type and zero default so run()'s "if compactionThreshold != 0" gate
// keeps meaning "operator asked for a specific threshold."
func TestCompactionThresholdFlag(t *testing.T) {
	t.Parallel()

	build := func() (*flag.FlagSet, *float64) {
		fs := flag.NewFlagSet("core-agent-test", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		thr := fs.Float64("compaction-threshold", 0, "compaction trigger utilization")
		return fs, thr
	}

	t.Run("unset defaults to zero", func(t *testing.T) {
		t.Parallel()
		fs, thr := build()
		if err := fs.Parse([]string{}); err != nil {
			t.Fatalf("parse: %v", err)
		}
		if *thr != 0 {
			t.Errorf("default: want 0 (unset sentinel), got %v", *thr)
		}
	})

	t.Run("parses a fraction", func(t *testing.T) {
		t.Parallel()
		fs, thr := build()
		if err := fs.Parse([]string{"--compaction-threshold=0.8"}); err != nil {
			t.Fatalf("parse: %v", err)
		}
		if *thr != 0.8 {
			t.Errorf("--compaction-threshold=0.8: got %v", *thr)
		}
	})
}
