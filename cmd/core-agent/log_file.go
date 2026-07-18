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
	"fmt"
	"io"
	"os"
)

// installLogFileTee mirrors every write to os.Stderr into path in
// addition to the terminal. The path is opened in append mode so
// long-running daemons accumulate history across restarts.
//
// Empty path and "-" are no-ops (preserving today's stderr-only
// behavior). Any other value is fatal on failure — the operator asked
// for a log destination and returning silently would leave them worse
// off than the no-flag baseline.
//
// Motivation (#179): the bubble-tea TUI takes over the screen and
// swallows every startup-time diagnostic written to os.Stderr (MCP
// init failures, model resolution, watchdog notices). Operators reach
// for `2> /tmp/…` only after a problem hits and they've lost the
// stderr lines that would have explained it. --log-file lets them
// capture the same content up-front, without shell redirection
// wrapping the whole daemon invocation.
func installLogFileTee(path string) error {
	if path == "" || path == "-" {
		return nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	return teeStderrTo(f)
}

// teeStderrTo swaps os.Stderr for a pipe whose reader teeing goroutine
// writes to both the original stderr and w. The original os.Stderr is
// preserved so the terminal still sees writes (the TUI's alternate-
// screen mode "swallows" them visually, but they land on fd 2 all the
// same — and if the TUI never starts, the operator sees them normally).
//
// Split from installLogFileTee so tests can drive it with an
// arbitrary io.Writer sink.
func teeStderrTo(w io.Writer) error {
	r, pw, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("os.Pipe: %w", err)
	}
	origStderr := os.Stderr
	os.Stderr = pw
	mw := io.MultiWriter(origStderr, w)
	go func() {
		// io.Copy exits only when the pipe write end closes, which
		// happens implicitly at process exit. No explicit teardown —
		// os.Exit doesn't run defers, and buffered content is best-
		// effort during shutdown regardless.
		_, _ = io.Copy(mw, r)
	}()
	return nil
}
