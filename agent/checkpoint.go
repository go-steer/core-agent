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
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/adk/session"
)

// checkpointAuthorSuffix is what every checkpoint event's Author
// string ends with. Resume queries use eventlog.WithAuthorSuffix on
// this so a run started from one binary can be resumed from another
// (e.g. `core-agent/autonomous` → `scion-agent/autonomous`).
const checkpointAuthorSuffix = "/autonomous"

// checkpointPayload is the structured content stored in
// session.Event.CustomMetadata for every checkpoint emitted by the
// autonomous driver. Per-turn checkpoints have empty StopReason; the
// final checkpoint emitted on loop exit sets it to one of the
// StopReason values.
type checkpointPayload struct {
	Turn               int     `json:"turn"`
	InputTokens        int     `json:"input_tokens"`
	OutputTokens       int     `json:"output_tokens"`
	CostUSD            float64 `json:"cost_usd"`
	Goal               string  `json:"goal"`
	ContinuationPrompt string  `json:"continuation_prompt"`
	StopReason         string  `json:"stop_reason,omitempty"`
	DoneDetail         string  `json:"done_detail,omitempty"`
	FinalText          string  `json:"final_text,omitempty"`
}

// toMap projects the typed payload into the untyped map[string]any
// shape ADK's session.Event.CustomMetadata uses on the wire.
func (p checkpointPayload) toMap() map[string]any {
	return map[string]any{
		"turn":                p.Turn,
		"input_tokens":        p.InputTokens,
		"output_tokens":       p.OutputTokens,
		"cost_usd":            p.CostUSD,
		"goal":                p.Goal,
		"continuation_prompt": p.ContinuationPrompt,
		"stop_reason":         p.StopReason,
		"done_detail":         p.DoneDetail,
		"final_text":          p.FinalText,
	}
}

// checkpointFromMap is the inverse — used by ResumeAutonomous to
// rehydrate the payload from a loaded session.Event. Defensively
// handles type drift (e.g. JSON round-trip turning ints into
// float64s).
func checkpointFromMap(m map[string]any) checkpointPayload {
	getInt := func(k string) int {
		switch v := m[k].(type) {
		case int:
			return v
		case int64:
			return int(v)
		case float64:
			return int(v)
		}
		return 0
	}
	getFloat := func(k string) float64 {
		switch v := m[k].(type) {
		case float64:
			return v
		case int:
			return float64(v)
		case int64:
			return float64(v)
		}
		return 0
	}
	getStr := func(k string) string {
		s, _ := m[k].(string)
		return s
	}
	return checkpointPayload{
		Turn:               getInt("turn"),
		InputTokens:        getInt("input_tokens"),
		OutputTokens:       getInt("output_tokens"),
		CostUSD:            getFloat("cost_usd"),
		Goal:               getStr("goal"),
		ContinuationPrompt: getStr("continuation_prompt"),
		StopReason:         getStr("stop_reason"),
		DoneDetail:         getStr("done_detail"),
		FinalText:          getStr("final_text"),
	}
}

// emitCheckpoint writes a checkpoint event to the agent's
// session.Service. No-op when the agent has no event log wired
// (in-memory sessions can't survive a process restart, so the
// checkpoint would be lost anyway).
//
// Returns nil on the no-op path; surfaces the underlying
// AppendEvent error otherwise. Callers typically ignore the error
// because checkpoint failure should not abort an otherwise healthy
// run.
func emitCheckpoint(ctx context.Context, a *Agent, payload checkpointPayload) error {
	if a == nil || a.eventLog == nil {
		return nil
	}
	svc := a.SessionService()
	if svc == nil {
		return nil
	}
	resp, err := svc.Get(ctx, &session.GetRequest{
		AppName:   a.AppName(),
		UserID:    a.UserID(),
		SessionID: a.SessionID(),
	})
	if err != nil {
		return fmt.Errorf("checkpoint: load session: %w", err)
	}
	if resp == nil || resp.Session == nil {
		// Session doesn't exist yet — race during first turn. Try
		// to create it so the first checkpoint can land.
		_, cerr := svc.Create(ctx, &session.CreateRequest{
			AppName:   a.AppName(),
			UserID:    a.UserID(),
			SessionID: a.SessionID(),
		})
		if cerr != nil {
			return fmt.Errorf("checkpoint: create session: %w", cerr)
		}
		resp, err = svc.Get(ctx, &session.GetRequest{
			AppName:   a.AppName(),
			UserID:    a.UserID(),
			SessionID: a.SessionID(),
		})
		if err != nil || resp == nil || resp.Session == nil {
			return errors.New("checkpoint: session unavailable after create")
		}
	}
	ev := &session.Event{
		ID:        newEventID(),
		Author:    binaryAuthor(),
		Timestamp: time.Now(),
		LLMResponse: adkmodel.LLMResponse{
			CustomMetadata: payload.toMap(),
		},
	}
	if err := svc.AppendEvent(ctx, resp.Session, ev); err != nil {
		return fmt.Errorf("checkpoint: append: %w", err)
	}
	return nil
}

// binaryAuthor returns the Author string used on every checkpoint
// event: "<binary>/autonomous". The binary name comes from
// os.Executable() so forks land with their own identity in the audit
// log; resume queries match by suffix so cross-binary handoffs work.
func binaryAuthor() string {
	name := "core-agent"
	if exe, err := os.Executable(); err == nil {
		base := filepath.Base(exe)
		base = strings.TrimSuffix(base, ".exe")
		if base != "" {
			name = base
		}
	}
	return name + checkpointAuthorSuffix
}

// newEventID returns a hex-encoded random ID for a manually
// constructed event. ADK's runner-emitted events get IDs from the
// agent.Context implementation; manually constructed checkpoints
// need their own.
func newEventID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Extremely unlikely; fall back to a timestamp-based ID
		// so downstream constraints (unique index on event_id)
		// keep working under exotic failure modes.
		return fmt.Sprintf("checkpoint-%d", time.Now().UnixNano())
	}
	return "checkpoint-" + hex.EncodeToString(b[:])
}
