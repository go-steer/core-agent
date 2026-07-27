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

package attach

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"google.golang.org/adk/session"

	"github.com/go-steer/core-agent/v2/pkg/eventlog"
)

// seedTestEvents creates the session once and appends n small events,
// returning after all have been persisted. Unlike appendTestEvent it
// doesn't re-Create the session per append, so it can seed dense
// histories cheaply.
func seedTestEvents(t *testing.T, h *eventlog.Handle, appName, userID, sessionID string, n int) {
	t.Helper()
	ctx := context.Background()
	if _, err := h.Service.Create(ctx, &session.CreateRequest{
		AppName: appName, UserID: userID, SessionID: sessionID,
	}); err != nil {
		t.Fatalf("session Create: %v", err)
	}
	getResp, err := h.Service.Get(ctx, &session.GetRequest{
		AppName: appName, UserID: userID, SessionID: sessionID,
	})
	if err != nil {
		t.Fatalf("session Get: %v", err)
	}
	for i := 0; i < n; i++ {
		ev := session.NewEvent(fmt.Sprintf("evt-%03d", i))
		ev.Author = "test"
		ev.CustomMetadata = map[string]any{"n": i}
		if err := h.Service.AppendEvent(ctx, getResp.Session, ev); err != nil {
			t.Fatalf("AppendEvent %d: %v", i, err)
		}
	}
}

// TestEvents_SinceZeroReplayIsCapped pins the #385 replay-cap fix:
// a ?since=0 subscribe against a history longer than maxReplayEvents
// must replay only the NEWEST maxReplayEvents frames (the tail), not
// the full table — and neither the replay goroutine nor the pump may
// re-introduce the dropped head.
//
// Deliberately NOT t.Parallel(): it lowers the package-level
// maxReplayEvents test seam, which parallel subscribers would read
// concurrently.
func TestEvents_SinceZeroReplayIsCapped(t *testing.T) {
	oldCap := maxReplayEvents
	maxReplayEvents = 10
	defer func() { maxReplayEvents = oldCap }()

	h, cleanupLog := openTestEventLog(t)
	defer cleanupLog()

	reg := NewSessionRegistry()
	ag := &eventfulRegistrant{
		stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "capped"},
		handle:         h,
	}
	if _, err := reg.Register(ag); err != nil {
		t.Fatal(err)
	}

	const total = 30 // seqs 1..30 in a fresh per-test database
	seedTestEvents(t, h, "core-agent", "u", "capped", total)

	base, cleanupSrv := startTestServer(t, reg)
	defer cleanupSrv()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		base+"/sessions/core-agent/capped/events?since=0", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("subscribe status %d", resp.StatusCode)
	}

	frames := readSSEFrames(t, resp.Body)

	// Collect every legacy `agent` frame's seq until the newest event
	// (seq == total) arrives; typed boot frames are skipped.
	var seqs []int64
	deadline := time.After(4 * time.Second)
collect:
	for {
		select {
		case f, ok := <-frames:
			if !ok {
				break collect
			}
			if f.Event != EventAgent {
				continue
			}
			var fr struct {
				Seq int64 `json:"seq"`
			}
			if err := json.Unmarshal([]byte(f.Data), &fr); err != nil {
				t.Fatalf("frame JSON: %v (data=%s)", err, f.Data)
			}
			seqs = append(seqs, fr.Seq)
			if fr.Seq == total {
				break collect
			}
		case <-deadline:
			break collect
		}
	}

	if len(seqs) == 0 {
		t.Fatal("no agent frames arrived")
	}
	if got := int64(len(seqs)); got != maxReplayEvents {
		t.Errorf("replayed %d frames, want exactly cap=%d (seqs=%v)", got, maxReplayEvents, seqs)
	}
	// The cap must keep the TAIL: newest cap-many, in order, ending
	// at the head. First delivered seq is total-cap+1; anything lower
	// means the head of the table leaked through.
	wantFirst := int64(total) - maxReplayEvents + 1
	for i, s := range seqs {
		if want := wantFirst + int64(i); s != want {
			t.Fatalf("seqs[%d] = %d, want %d (full tail, oldest-events dropped); seqs=%v", i, s, want, seqs)
		}
	}
}

// TestEvents_IncrementalResumeBelowCapUnaffected locks in the
// preserved semantics for since>0 resumes whose gap fits under the
// cap: every event after the cursor arrives, nothing is truncated.
func TestEvents_IncrementalResumeBelowCapUnaffected(t *testing.T) {
	oldCap := maxReplayEvents
	maxReplayEvents = 10
	defer func() { maxReplayEvents = oldCap }()

	h, cleanupLog := openTestEventLog(t)
	defer cleanupLog()

	reg := NewSessionRegistry()
	ag := &eventfulRegistrant{
		stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "resume"},
		handle:         h,
	}
	if _, err := reg.Register(ag); err != nil {
		t.Fatal(err)
	}

	const total = 12
	seedTestEvents(t, h, "core-agent", "u", "resume", total)

	base, cleanupSrv := startTestServer(t, reg)
	defer cleanupSrv()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Cursor 5: gap of 7 events < cap of 10 → uncapped incremental resume.
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		base+"/sessions/core-agent/resume/events?since=5", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer resp.Body.Close()

	frames := readSSEFrames(t, resp.Body)
	var seqs []int64
	deadline := time.After(4 * time.Second)
collect:
	for {
		select {
		case f, ok := <-frames:
			if !ok {
				break collect
			}
			if f.Event != EventAgent {
				continue
			}
			var fr struct {
				Seq int64 `json:"seq"`
			}
			if err := json.Unmarshal([]byte(f.Data), &fr); err != nil {
				t.Fatalf("frame JSON: %v (data=%s)", err, f.Data)
			}
			seqs = append(seqs, fr.Seq)
			if fr.Seq == total {
				break collect
			}
		case <-deadline:
			break collect
		}
	}

	want := []int64{6, 7, 8, 9, 10, 11, 12}
	if len(seqs) != len(want) {
		t.Fatalf("got %d frames %v, want %v", len(seqs), seqs, want)
	}
	for i := range want {
		if seqs[i] != want[i] {
			t.Fatalf("seqs[%d] = %d, want %d (incremental resume must be untouched); seqs=%v", i, seqs[i], want[i], seqs)
		}
	}
}
