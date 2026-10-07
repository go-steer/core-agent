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

package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"google.golang.org/adk/session"

	"github.com/go-steer/core-agent/v2/pkg/attach"
	"github.com/go-steer/core-agent/v2/pkg/usage"
)

// TestTurnFailure_ReplayCarriesTheDurableRecords is #1258's acceptance
// criterion run in-process: a session whose turn was cut by the
// per-turn ceiling is replayed over the real attach server with
// GET …/events?since=0 — after the fact, by a client that was never
// attached — and the replay carries the cut and the turn error as
// `agent` frames.
//
// This is the measurement the 2026-10-06 fault batch made by hand on
// std-simian-test, where all three sessions replayed 0 cuts and 0 turn
// errors against 7 and 7 in the daemon log.
//
// Fails on pre-#1258 code: the replay carries neither row.
func TestTurnFailure_ReplayCarriesTheDurableRecords(t *testing.T) {
	t.Parallel()
	h, cleanup := openTestEventLog(t)
	defer cleanup()
	tr := usage.NewTracker()
	llm := &burnLoopLLM{perCallIn: 1000}
	a := newCutAgent(t, h, "s-1258-replay", llm, tr)
	burnToCut(t, a, tr, llm)

	reg := attach.NewSessionRegistry()
	if _, err := reg.Register(a); err != nil {
		t.Fatalf("Register: %v", err)
	}
	srv, err := attach.NewServer(attach.Options{Registry: reg, Addr: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	go func() { _ = srv.ListenAndServe() }()
	defer func() { _ = srv.Close() }()
	base := ""
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline) && base == ""; time.Sleep(5 * time.Millisecond) {
		if addr := srv.Addr(); addr != "" {
			base = "http://" + addr
		}
	}
	if base == "" {
		t.Fatal("attach server never bound")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		base+"/sessions/"+a.AppName()+"/s-1258-replay/events?since=0", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("subscribe status %d", resp.StatusCode)
	}

	var trips []attach.GuardrailTrip
	var errs []attach.TurnError
	var cutBy string
	typed := map[string]int{}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	name := ""
	for sc.Scan() && (len(trips) == 0 || len(errs) == 0) {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: ") && name == attach.EventAgent:
			var fr struct {
				Event *session.Event `json:"event"`
			}
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &fr); err != nil {
				t.Fatalf("agent frame JSON: %v", err)
			}
			if gt, ok := attach.GuardrailTurnTrip(fr.Event); ok {
				trips = append(trips, gt)
			}
			if te, cb, ok := attach.TurnErrorRow(fr.Event); ok {
				errs, cutBy = append(errs, te), cb
			}
		case strings.HasPrefix(line, "data: "):
			typed[name]++
		}
	}
	if len(trips) != 1 || len(errs) != 1 {
		t.Fatalf("replay carried %d cut rows and %d turn-error rows; want 1 and 1 (typed frames seen: %v)",
			len(trips), len(errs), typed)
	}
	if trips[0].Guardrail != attach.GuardrailCostCeiling || !trips[0].HaltedTurn || trips[0].EventID == "" {
		t.Errorf("replayed cut = %+v", trips[0])
	}
	if errs[0].Kind != attach.TurnErrorCanceled || cutBy != attach.TurnErrorCostCeiling {
		t.Errorf("replayed turn error = %+v cut_by=%q", errs[0], cutBy)
	}
	// Typed frames are live-only and are not what this test found: the
	// replay must not have re-synthesized them.
	if typed[attach.EventGuardrailTrip] != 0 || typed[attach.EventTurnError] != 0 {
		t.Errorf("replay re-emitted typed failure frames: %v", typed)
	}
}
