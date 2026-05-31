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
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/core-agent/pkg/permissions"
)

// promptRegistrant satisfies PromptBrokerProvider on top of
// stubRegistrant so the prompt-stream + respond handlers can resolve
// to a real broker.
type promptRegistrant struct {
	stubRegistrant
	broker *PromptBroker
}

func (p *promptRegistrant) AttachPromptBroker() *PromptBroker { return p.broker }

func TestIntegration_PromptStreamAndRespond_RoundTrip(t *testing.T) {
	t.Parallel()

	broker := NewPromptBroker()
	defer broker.Close()
	reg := NewSessionRegistry()
	ag := &promptRegistrant{
		stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s1"},
		broker:         broker,
	}
	if _, err := reg.Register(ag); err != nil {
		t.Fatal(err)
	}
	base, cleanup := startTestServer(t, reg)
	defer cleanup()

	// Subscribe to /perms/stream first so we don't race the AskApproval.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		base+"/sessions/core-agent/s1/perms/stream", nil)
	streamResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer streamResp.Body.Close()
	if streamResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(streamResp.Body)
		t.Fatalf("subscribe status %d: %s", streamResp.StatusCode, body)
	}

	// Fire AskApproval in a goroutine; collect the decision when it unblocks.
	decided := make(chan struct {
		d   permissions.Decision
		err error
	}, 1)
	go func() {
		d, err := broker.AskApproval(context.Background(), permissions.PromptRequest{
			Kind:     permissions.PromptKindBash,
			ToolName: "bash",
			Detail:   "echo hi",
			Verb:     "echo",
		})
		decided <- struct {
			d   permissions.Decision
			err error
		}{d, err}
	}()

	// Read the SSE frame.
	scanner := bufio.NewScanner(streamResp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var frame PromptFrame
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		raw := strings.TrimPrefix(line, "data: ")
		if err := json.Unmarshal([]byte(raw), &frame); err != nil {
			t.Fatalf("frame unmarshal: %v: %s", err, raw)
		}
		break
	}
	if frame.ID == "" {
		t.Fatalf("no frame received within deadline")
	}
	if frame.Kind != "bash" || frame.Verb != "echo" {
		t.Errorf("frame = %+v, want kind=bash verb=echo", frame)
	}

	// POST the decision and verify the goroutine unblocks.
	body, _ := json.Marshal(PromptResponse{ID: frame.ID, Decision: "allow-session"})
	resp, err := http.Post(base+"/sessions/core-agent/s1/perms/respond", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST respond: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST respond status %d", resp.StatusCode)
	}

	select {
	case got := <-decided:
		if got.err != nil {
			t.Fatalf("AskApproval err = %v", got.err)
		}
		if got.d != permissions.DecisionAllowSession {
			t.Errorf("decision = %v, want DecisionAllowSession", got.d)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("AskApproval did not unblock after POST")
	}
}

func TestIntegration_PromptEndpoints_501WhenNoBroker(t *testing.T) {
	t.Parallel()
	reg := NewSessionRegistry()
	// stubRegistrant doesn't implement PromptBrokerProvider.
	ag := &stubRegistrant{app: "core-agent", user: "u", sid: "s1"}
	if _, err := reg.Register(ag); err != nil {
		t.Fatal(err)
	}
	base, cleanup := startTestServer(t, reg)
	defer cleanup()

	resp, err := http.Get(base + "/sessions/core-agent/s1/perms/stream")
	if err != nil {
		t.Fatalf("GET stream: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("GET stream status = %d, want 501", resp.StatusCode)
	}

	body, _ := json.Marshal(PromptResponse{ID: "x", Decision: "deny"})
	resp, err = http.Post(base+"/sessions/core-agent/s1/perms/respond", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST respond: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("POST respond status = %d, want 501", resp.StatusCode)
	}
}

func TestIntegration_PromptRespond_404OnUnknownID(t *testing.T) {
	t.Parallel()
	broker := NewPromptBroker()
	defer broker.Close()
	reg := NewSessionRegistry()
	ag := &promptRegistrant{
		stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s1"},
		broker:         broker,
	}
	if _, err := reg.Register(ag); err != nil {
		t.Fatal(err)
	}
	base, cleanup := startTestServer(t, reg)
	defer cleanup()

	body, _ := json.Marshal(PromptResponse{ID: "missing", Decision: "allow-once"})
	resp, err := http.Post(base+"/sessions/core-agent/s1/perms/respond", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST respond: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("POST respond unknown id: status = %d, want 404", resp.StatusCode)
	}
}

func TestIntegration_PromptRespond_400OnBadDecision(t *testing.T) {
	t.Parallel()
	broker := NewPromptBroker()
	defer broker.Close()
	reg := NewSessionRegistry()
	ag := &promptRegistrant{
		stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s1"},
		broker:         broker,
	}
	if _, err := reg.Register(ag); err != nil {
		t.Fatal(err)
	}
	base, cleanup := startTestServer(t, reg)
	defer cleanup()

	body, _ := json.Marshal(PromptResponse{ID: "x", Decision: "bogus"})
	resp, err := http.Post(base+"/sessions/core-agent/s1/perms/respond", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST respond: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("POST respond bogus decision: status = %d, want 400", resp.StatusCode)
	}
}
