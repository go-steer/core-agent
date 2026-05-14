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
	"strings"
	"testing"
)

func TestTruncate_NoLimit(t *testing.T) {
	t.Parallel()
	in := "abc\n"
	if got := Truncate(in, 0, 0); got != in {
		t.Errorf("no-cap should be identity")
	}
}

func TestTruncate_BytesCap(t *testing.T) {
	t.Parallel()
	in := strings.Repeat("x", 100)
	got := Truncate(in, 10, 0)
	if !strings.HasPrefix(got, "xxxxxxxxxx\n... [truncated") {
		t.Errorf("unexpected output: %q", got)
	}
}

func TestTruncate_LinesCap(t *testing.T) {
	t.Parallel()
	in := "a\nb\nc\nd\ne\n"
	got := Truncate(in, 0, 3)
	if !strings.HasPrefix(got, "a\nb\nc\n") {
		t.Errorf("expected first 3 lines preserved, got: %q", got)
	}
	if !strings.Contains(got, "[truncated by core-agent") {
		t.Errorf("missing truncation marker: %q", got)
	}
}

func TestTruncate_BothCaps(t *testing.T) {
	t.Parallel()
	in := strings.Repeat("line\n", 100)
	got := Truncate(in, 200, 5)
	// Lines cap should fire (5 lines = ~25 bytes well under 200).
	lineCount := strings.Count(got, "\n")
	// 5 lines + 1 marker line; marker is appended without trailing \n.
	if lineCount < 5 || lineCount > 6 {
		t.Errorf("expected ~5 lines, got %d", lineCount)
	}
}
