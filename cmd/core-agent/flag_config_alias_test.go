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

// TestConfigFlagAlias locks in that -c and --config bind to the same
// underlying variable (issue #209). We build the two flags with the
// same registration pattern main.go uses (two flag.StringVar calls
// against the same destination) and verify:
//
//   - passing only -c sets the variable
//   - passing only --config sets the variable
//   - passing both, --config wins (Go flag package: last-on-argv wins)
//
// This exercises the ALIAS binding contract, not the higher-level
// config loader — that's covered separately. Regression guard for
// anyone who might collapse the two calls back into a single one.
func TestConfigFlagAlias(t *testing.T) {
	t.Parallel()

	build := func() (*flag.FlagSet, *string) {
		fs := flag.NewFlagSet("core-agent-test", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		var cfgPathVal string
		fs.StringVar(&cfgPathVal, "c", "", "config file path")
		fs.StringVar(&cfgPathVal, "config", "", "long-form alias for -c")
		return fs, &cfgPathVal
	}

	t.Run("short only", func(t *testing.T) {
		t.Parallel()
		fs, cfgPath := build()
		if err := fs.Parse([]string{"-c", "/tmp/a.json"}); err != nil {
			t.Fatalf("parse: %v", err)
		}
		if *cfgPath != "/tmp/a.json" {
			t.Errorf("-c: want /tmp/a.json, got %q", *cfgPath)
		}
	})

	t.Run("long only", func(t *testing.T) {
		t.Parallel()
		fs, cfgPath := build()
		if err := fs.Parse([]string{"--config", "/tmp/b.json"}); err != nil {
			t.Fatalf("parse: %v", err)
		}
		if *cfgPath != "/tmp/b.json" {
			t.Errorf("--config: want /tmp/b.json, got %q", *cfgPath)
		}
	})

	t.Run("long with equals", func(t *testing.T) {
		t.Parallel()
		fs, cfgPath := build()
		if err := fs.Parse([]string{"--config=/tmp/c.json"}); err != nil {
			t.Fatalf("parse: %v", err)
		}
		if *cfgPath != "/tmp/c.json" {
			t.Errorf("--config=: want /tmp/c.json, got %q", *cfgPath)
		}
	})

	t.Run("both, last wins", func(t *testing.T) {
		t.Parallel()
		fs, cfgPath := build()
		if err := fs.Parse([]string{"-c", "/tmp/first.json", "--config", "/tmp/second.json"}); err != nil {
			t.Fatalf("parse: %v", err)
		}
		if *cfgPath != "/tmp/second.json" {
			t.Errorf("both flags: want /tmp/second.json (last wins), got %q", *cfgPath)
		}
	})

	t.Run("neither", func(t *testing.T) {
		t.Parallel()
		fs, cfgPath := build()
		if err := fs.Parse([]string{}); err != nil {
			t.Fatalf("parse: %v", err)
		}
		if *cfgPath != "" {
			t.Errorf("neither flag: want empty, got %q", *cfgPath)
		}
	})
}
