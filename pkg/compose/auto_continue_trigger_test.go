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

package compose

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// captureStderr swaps os.Stderr for a pipe while fn runs and returns
// everything written to it. Deliberately NOT parallel-safe, and every
// test below therefore omits t.Parallel: the swap is process-wide, and
// a paused parallel test cannot run during a sequential one, so the
// capture only ever holds this test's output.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	orig := os.Stderr
	os.Stderr = w
	read := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		read <- string(b)
	}()
	fn()
	os.Stderr = orig
	_ = w.Close()
	out := <-read
	_ = r.Close()
	return out
}

// TestAutoContinueScan_LogsTheTriggerThatFiredIt is #1066. The 8-hour
// A1 soak of 2026-09-14 recorded a stranded continuation self-healing
// at 19:23 on a pod with 0 restarts and 6h38m of uptime — and the line
// announcing it read "auto-continue boot scan:". The operator that
// line is written for is reconstructing a night they did not watch;
// "boot scan" sends them hunting a crash that did not happen. The scan
// is shared by two drivers, so it has to say which one fired.
func TestAutoContinueScan_LogsTheTriggerThatFiredIt(t *testing.T) {
	t.Run("the boot driver still says boot scan", func(t *testing.T) {
		h := openAC(t)
		seedACSession(t, h, "sid-hung", acUserEvent("hello?", time.Now().Add(-5*time.Minute)))
		deps := scanDeps(t, h)
		putACLRow(t, deps.ACLStore, "sid-hung")

		out := captureStderr(t, func() { AutoContinueBootScan(deps, 10) })

		if !strings.Contains(out, "auto-continue boot scan: queued continuations for 1 session(s)") {
			t.Errorf("stderr = %q, want the boot-scan-labelled queued line", out)
		}
	})
	t.Run("the in-lifetime retry driver says retry", func(t *testing.T) {
		h := openAC(t)
		seedACSession(t, h, "sid-hung", acUserEvent("hello?", time.Now().Add(-5*time.Minute)))
		deps := scanDeps(t, h)
		putACLRow(t, deps.ACLStore, "sid-hung")

		out := captureStderr(t, func() { AutoContinueRetryScan(deps, 10) })

		if !strings.Contains(out, "auto-continue retry: queued continuations for 1 session(s)") {
			t.Errorf("stderr = %q, want the retry-labelled queued line", out)
		}
		if strings.Contains(out, "boot scan") {
			t.Errorf("stderr = %q, must not claim a boot scan — nothing booted", out)
		}
	})
}

// TestAutoContinueStartup_LogsTheTriggerThatFiredIt is the #1066 pair
// for the headless single-session daemon (#558), whose pass the same
// retry loop re-fires. The breaker line is the one this trigger writes
// on a path a test can reach, and it is also the one that says what is
// being stood down: on the retry driver that is a pass, not a boot.
func TestAutoContinueStartup_LogsTheTriggerThatFiredIt(t *testing.T) {
	ctx := context.Background()
	t.Run("the boot driver stands down this boot", func(t *testing.T) {
		h := seedAC(t, acUserEvent("hello?", time.Now()))
		for i := 0; i < breakerBootThreshold; i++ {
			if _, err := h.RecordBoot(ctx, time.Now().Add(-time.Duration(i+1)*time.Minute), []string{"other-sid"}); err != nil {
				t.Fatalf("RecordBoot: %v", err)
			}
		}
		ag := acAgent(t, h)

		out := captureStderr(t, func() { AutoContinueStartupSession(ctx, h, ag, time.Hour) })

		if !strings.Contains(out, "standing down this boot") {
			t.Errorf("stderr = %q, want the breaker standing down this boot", out)
		}
	})
	t.Run("the in-lifetime retry driver stands down this pass", func(t *testing.T) {
		h := seedAC(t, acUserEvent("hello?", time.Now()))
		for i := 0; i < breakerBootThreshold; i++ {
			if _, err := h.RecordBoot(ctx, time.Now().Add(-time.Duration(i+1)*time.Minute), []string{"other-sid"}); err != nil {
				t.Fatalf("RecordBoot: %v", err)
			}
		}
		ag := acAgent(t, h)

		out := captureStderr(t, func() { AutoContinueStartupRetry(ctx, h, ag, time.Hour) })

		if !strings.Contains(out, "standing down this pass") {
			t.Errorf("stderr = %q, want the breaker standing down this pass — the daemon is not rebooting", out)
		}
		if strings.Contains(out, "this boot") {
			t.Errorf("stderr = %q, must not call an in-lifetime pass a boot", out)
		}
	})
}
