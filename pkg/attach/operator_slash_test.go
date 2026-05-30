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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"
)

// operatorSlashRegistrant satisfies the four PR A3 slash capability
// interfaces on top of stubRegistrant. Each call records its inputs
// and returns the canned response.
type operatorSlashRegistrant struct {
	stubRegistrant
	mu sync.Mutex

	compactCalls    []string // focus args, in order
	compactResp     CompactResponse
	compactErr      error
	checkpointCalls []string // note args
	checkpointResp  CheckpointResponse
	checkpointErr   error
	btwCalls        []string // questions
	btwAnswer       string
	btwErr          error
	subagentCalls   []SubagentSpec
	subagentResp    SubagentSpawnResponse
	subagentErr     error
}

func (s *operatorSlashRegistrant) AttachCompact(ctx context.Context, focus string) (CompactResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.compactCalls = append(s.compactCalls, focus)
	return s.compactResp, s.compactErr
}

func (s *operatorSlashRegistrant) AttachCheckpoint(ctx context.Context, note string) (CheckpointResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checkpointCalls = append(s.checkpointCalls, note)
	return s.checkpointResp, s.checkpointErr
}

func (s *operatorSlashRegistrant) AttachAskSideQuestion(ctx context.Context, question string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.btwCalls = append(s.btwCalls, question)
	return s.btwAnswer, s.btwErr
}

func (s *operatorSlashRegistrant) AttachSpawnSubagent(ctx context.Context, spec SubagentSpec) (SubagentSpawnResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.subagentCalls = append(s.subagentCalls, spec)
	return s.subagentResp, s.subagentErr
}

func TestIntegration_SlashCompact(t *testing.T) {
	t.Parallel()
	reg := NewSessionRegistry()
	ag := &operatorSlashRegistrant{
		stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s1"},
		compactResp:    CompactResponse{SummaryEventID: "evt-42", SummaryText: "summary text", DurationMS: 9123},
	}
	if _, err := reg.Register(ag); err != nil {
		t.Fatal(err)
	}
	base, cleanup := startTestServer(t, reg)
	defer cleanup()

	body, _ := json.Marshal(CompactRequest{Focus: "preserve test failures"})
	resp, err := http.Post(base+"/sessions/core-agent/s1/slash/compact", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
	var got CompactResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.SummaryEventID != "evt-42" || got.DurationMS != 9123 {
		t.Errorf("got = %+v", got)
	}
	if len(ag.compactCalls) != 1 || ag.compactCalls[0] != "preserve test failures" {
		t.Errorf("calls = %v", ag.compactCalls)
	}
}

func TestIntegration_SlashCompact_EmptyBody_AllowedAsDefaultFocus(t *testing.T) {
	t.Parallel()
	reg := NewSessionRegistry()
	ag := &operatorSlashRegistrant{
		stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s1"},
	}
	if _, err := reg.Register(ag); err != nil {
		t.Fatal(err)
	}
	base, cleanup := startTestServer(t, reg)
	defer cleanup()

	resp, err := http.Post(base+"/sessions/core-agent/s1/slash/compact", "application/json", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status %d, want 200", resp.StatusCode)
	}
	if len(ag.compactCalls) != 1 || ag.compactCalls[0] != "" {
		t.Errorf("calls = %v, want one empty-focus call", ag.compactCalls)
	}
}

func TestIntegration_SlashCompact_NoProvider_501(t *testing.T) {
	t.Parallel()
	reg := NewSessionRegistry()
	if _, err := reg.Register(&stubRegistrant{app: "core-agent", user: "u", sid: "s1"}); err != nil {
		t.Fatal(err)
	}
	base, cleanup := startTestServer(t, reg)
	defer cleanup()

	resp, err := http.Post(base+"/sessions/core-agent/s1/slash/compact", "application/json", bytes.NewReader([]byte("{}")))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("status %d, want 501", resp.StatusCode)
	}
}

func TestIntegration_SlashDone(t *testing.T) {
	t.Parallel()
	reg := NewSessionRegistry()
	ag := &operatorSlashRegistrant{
		stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s1"},
		checkpointResp: CheckpointResponse{CheckpointEventID: "cp-1", TaskNote: "shipped reorg", DurationMS: 5000},
	}
	if _, err := reg.Register(ag); err != nil {
		t.Fatal(err)
	}
	base, cleanup := startTestServer(t, reg)
	defer cleanup()

	body, _ := json.Marshal(CheckpointRequest{Note: "shipped reorg"})
	resp, err := http.Post(base+"/sessions/core-agent/s1/slash/done", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var got CheckpointResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.TaskNote != "shipped reorg" || got.CheckpointEventID != "cp-1" {
		t.Errorf("got = %+v", got)
	}
}

func TestIntegration_SlashBtw(t *testing.T) {
	t.Parallel()
	reg := NewSessionRegistry()
	ag := &operatorSlashRegistrant{
		stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s1"},
		btwAnswer:      "We use Gemini 3.1 Pro for the parent and Flash for subtasks.",
	}
	if _, err := reg.Register(ag); err != nil {
		t.Fatal(err)
	}
	base, cleanup := startTestServer(t, reg)
	defer cleanup()

	body, _ := json.Marshal(SideQueryRequest{Question: "what models are we using?"})
	resp, err := http.Post(base+"/sessions/core-agent/s1/slash/btw", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var got SideQueryResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Answer != ag.btwAnswer {
		t.Errorf("answer = %q", got.Answer)
	}
}

func TestIntegration_SlashBtw_EmptyQuestion_400(t *testing.T) {
	t.Parallel()
	reg := NewSessionRegistry()
	ag := &operatorSlashRegistrant{
		stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s1"},
	}
	if _, err := reg.Register(ag); err != nil {
		t.Fatal(err)
	}
	base, cleanup := startTestServer(t, reg)
	defer cleanup()

	body, _ := json.Marshal(SideQueryRequest{Question: ""})
	resp, err := http.Post(base+"/sessions/core-agent/s1/slash/btw", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status %d, want 400", resp.StatusCode)
	}
}

func TestIntegration_SlashSubagent(t *testing.T) {
	t.Parallel()
	reg := NewSessionRegistry()
	now := time.Date(2026, 5, 29, 21, 0, 0, 0, time.UTC)
	ag := &operatorSlashRegistrant{
		stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s1"},
		subagentResp:   SubagentSpawnResponse{Name: "watcher", StartedAt: now},
	}
	if _, err := reg.Register(ag); err != nil {
		t.Fatal(err)
	}
	base, cleanup := startTestServer(t, reg)
	defer cleanup()

	spec := SubagentSpec{
		Name:    "watcher",
		Goal:    "watch deployment myapp every 10 minutes",
		Tools:   []string{"bash", "read_file"},
		Budgets: SubagentBudget{MaxTurns: 50, MaxCostUSD: 1.0, MaxWallClockS: 3600},
	}
	body, _ := json.Marshal(spec)
	resp, err := http.Post(base+"/sessions/core-agent/s1/slash/subagent", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
	var got SubagentSpawnResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Name != "watcher" || !got.StartedAt.Equal(now) {
		t.Errorf("got = %+v", got)
	}
	if len(ag.subagentCalls) != 1 || ag.subagentCalls[0].Goal != spec.Goal {
		t.Errorf("calls = %+v", ag.subagentCalls)
	}
}

func TestIntegration_SlashSubagent_MissingName_400(t *testing.T) {
	t.Parallel()
	reg := NewSessionRegistry()
	if _, err := reg.Register(&operatorSlashRegistrant{
		stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s1"},
	}); err != nil {
		t.Fatal(err)
	}
	base, cleanup := startTestServer(t, reg)
	defer cleanup()

	body, _ := json.Marshal(SubagentSpec{Goal: "do something"}) // missing Name
	resp, err := http.Post(base+"/sessions/core-agent/s1/slash/subagent", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status %d, want 400", resp.StatusCode)
	}
}

func TestIntegration_SlashSubagent_SpawnerUnavailable_501(t *testing.T) {
	t.Parallel()
	// Provider exists but returns the agent-side
	// ErrSubagentSpawnerUnavailable sentinel — handler maps to 501
	// so the operator sees "spawner not registered" instead of 500.
	reg := NewSessionRegistry()
	ag := &operatorSlashRegistrant{
		stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s1"},
		subagentErr:    errors.New("agent: subagent spawner unavailable (no BackgroundAgentManager wired)"),
	}
	if _, err := reg.Register(ag); err != nil {
		t.Fatal(err)
	}
	base, cleanup := startTestServer(t, reg)
	defer cleanup()

	body, _ := json.Marshal(SubagentSpec{Name: "x", Goal: "y"})
	resp, err := http.Post(base+"/sessions/core-agent/s1/slash/subagent", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("status %d, want 501", resp.StatusCode)
	}
}

func TestIntegration_SlashCompact_AgentError_500(t *testing.T) {
	t.Parallel()
	reg := NewSessionRegistry()
	ag := &operatorSlashRegistrant{
		stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s1"},
		compactErr:     errors.New("summarizer LLM call failed"),
	}
	if _, err := reg.Register(ag); err != nil {
		t.Fatal(err)
	}
	base, cleanup := startTestServer(t, reg)
	defer cleanup()

	resp, err := http.Post(base+"/sessions/core-agent/s1/slash/compact", "application/json", bytes.NewReader([]byte("{}")))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status %d, want 500", resp.StatusCode)
	}
}
