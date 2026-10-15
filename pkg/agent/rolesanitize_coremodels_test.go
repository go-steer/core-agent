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
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	adkmodel "google.golang.org/adk/v2/model"

	"github.com/go-steer/core-agent/v3/pkg/models/anthropic"
	"github.com/go-steer/core-agent/v3/pkg/models/gemini"
)

// recordingServer answers every request with body and keeps the last
// request body it saw.
func recordingServer(t *testing.T, contentType, body string) (*httptest.Server, func() map[string]any) {
	t.Helper()
	var mu sync.Mutex
	var last map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		_ = json.Unmarshal(raw, &last)
		mu.Unlock()
		w.Header().Set("Content-Type", contentType)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, func() map[string]any {
		mu.Lock()
		defer mu.Unlock()
		return last
	}
}

func drainModel(t *testing.T, m adkmodel.LLM) {
	t.Helper()
	for _, err := range m.GenerateContent(context.Background(), &adkmodel.LLMRequest{}, false) {
		if err != nil {
			t.Fatalf("GenerateContent: %v", err)
		}
	}
}

// firstText digs the first text out of a request's first message, for
// either wire format (Gemini's contents[].parts[].text, Anthropic's
// messages[].content as a string or blocks).
func firstText(body map[string]any, list string) (role, text string) {
	items, _ := body[list].([]any)
	if len(items) == 0 {
		return "", ""
	}
	m, _ := items[0].(map[string]any)
	role, _ = m["role"].(string)
	switch c := m["content"].(type) {
	case string:
		return role, c
	case []any:
		if b, ok := c[0].(map[string]any); ok {
			text, _ = b["text"].(string)
		}
		return role, text
	}
	parts, _ := m["parts"].([]any)
	if len(parts) > 0 {
		p, _ := parts[0].(map[string]any)
		text, _ = p["text"].(string)
	}
	return role, text
}

// TestEmptyRequestThroughTheCoreModelsFacades: a wake on a fresh session
// with nothing queued sends a request with no contents. Both providers
// refuse an empty conversation, and core-models' adapters do not pad
// one, so the role sanitizer agent.New wraps around every model must
// still send the placeholder turn through the library-backed providers.
func TestEmptyRequestThroughTheCoreModelsFacades(t *testing.T) {
	t.Run("gemini", func(t *testing.T) {
		srv, last := recordingServer(t, "application/json",
			`{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`)
		t.Setenv("GOOGLE_GEMINI_BASE_URL", srv.URL)
		p, err := gemini.NewAPIKey("test-key", gemini.WithBuiltinTools(gemini.BuiltinTools{}))
		if err != nil {
			t.Fatalf("NewAPIKey: %v", err)
		}
		m, err := p.Model(context.Background(), "gemini-3.5-flash")
		if err != nil {
			t.Fatalf("Model: %v", err)
		}
		drainModel(t, newRoleSanitizingLLM(m, ""))
		if role, text := firstText(last(), "contents"); role != "user" || text != emptyContentsPlaceholder {
			t.Errorf("first content = %q %q, want the user placeholder", role, text)
		}
	})
	t.Run("anthropic", func(t *testing.T) {
		srv, last := recordingServer(t, "text/event-stream", `event: message_start
data: {"type":"message_start","message":{"id":"m","type":"message","role":"assistant","model":"claude-test","content":[],"stop_reason":null,"usage":{"input_tokens":5,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}

event: message_stop
data: {"type":"message_stop"}

`)
		t.Setenv(anthropic.EnvBaseURL, srv.URL)
		p, err := anthropic.New("test-key")
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		m, err := p.Model(context.Background(), "claude-test")
		if err != nil {
			t.Fatalf("Model: %v", err)
		}
		drainModel(t, newRoleSanitizingLLM(m, ""))
		if role, text := firstText(last(), "messages"); role != "user" || text != emptyContentsPlaceholder {
			t.Errorf("first message = %q %q, want the user placeholder", role, text)
		}
	})
}
