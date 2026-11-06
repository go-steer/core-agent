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

	"github.com/go-steer/core-agent/v3/pkg/runner"
)

func TestResolveOutputFormat(t *testing.T) {
	t.Parallel()

	t.Run("default text allowed without -p", func(t *testing.T) {
		t.Parallel()
		f, err := resolveOutputFormat("", "")
		if err != nil {
			t.Fatalf("resolveOutputFormat: %v", err)
		}
		if f != runner.OutputFormatText {
			t.Errorf("got %q, want %q", f, runner.OutputFormatText)
		}
	})

	t.Run("json requires -p", func(t *testing.T) {
		t.Parallel()
		if _, err := resolveOutputFormat("json", ""); err == nil {
			t.Fatal("expected error for --output-format=json without -p, got nil")
		}
	})

	t.Run("stream-json requires -p", func(t *testing.T) {
		t.Parallel()
		if _, err := resolveOutputFormat("stream-json", ""); err == nil {
			t.Fatal("expected error for --output-format=stream-json without -p, got nil")
		}
	})

	t.Run("json with -p succeeds", func(t *testing.T) {
		t.Parallel()
		f, err := resolveOutputFormat("json", "hello")
		if err != nil {
			t.Fatalf("resolveOutputFormat: %v", err)
		}
		if f != runner.OutputFormatJSON {
			t.Errorf("got %q, want %q", f, runner.OutputFormatJSON)
		}
	})

	t.Run("unknown format rejected", func(t *testing.T) {
		t.Parallel()
		if _, err := resolveOutputFormat("xml", "hello"); err == nil {
			t.Fatal("expected error for unknown format 'xml', got nil")
		}
	})
}

func TestQuietFlagAlias(t *testing.T) {
	t.Parallel()

	build := func() (*flag.FlagSet, *bool) {
		fs := flag.NewFlagSet("core-agent-test", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		var q bool
		fs.BoolVar(&q, "q", false, "short quiet")
		fs.BoolVar(&q, "quiet", false, "long quiet")
		return fs, &q
	}

	t.Run("short -q", func(t *testing.T) {
		t.Parallel()
		fs, q := build()
		if err := fs.Parse([]string{"-q"}); err != nil {
			t.Fatalf("parse: %v", err)
		}
		if !*q {
			t.Error("-q did not set quiet=true")
		}
	})

	t.Run("long --quiet", func(t *testing.T) {
		t.Parallel()
		fs, q := build()
		if err := fs.Parse([]string{"--quiet"}); err != nil {
			t.Fatalf("parse: %v", err)
		}
		if !*q {
			t.Error("--quiet did not set quiet=true")
		}
	})
}
