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

// Originally derived from go-steer/mast.

package notify

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestClient_PostAndChunking(t *testing.T) {
	type call struct {
		method       string
		path         string
		auth         string
		session      string
		conversation string
		text         string
	}
	var calls []call
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload map[string]string
		_ = json.Unmarshal(body, &payload)
		calls = append(calls, call{
			method:       r.Method,
			path:         r.URL.Path,
			auth:         r.Header.Get("Authorization"),
			session:      r.Header.Get("X-Agent-Session"),
			conversation: payload["conversation"],
			text:         payload["text"],
		})
		w.Header().Set("Content-Type", "application/json")
		if len(calls) == 1 {
			_, _ = w.Write([]byte(`{"channel":"C123","id":"1700000.001"}`))
			return
		}
		_, _ = w.Write([]byte(`{"channel":"C123","id":"1700000.002"}`))
	}))
	defer srv.Close()

	c, err := New(Config{
		Endpoint:      srv.URL,
		Channel:       "C123",
		Token:         "secret-tok",
		MaxChunkRunes: 12,
	})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	ref, err := c.Post(context.Background(), "line-one\nline-two\nline-three", "sess-42")
	if err != nil {
		t.Fatalf("Post() error: %v", err)
	}
	if ref.Channel != "C123" || ref.ID != "1700000.001" {
		t.Errorf("Post() ref = %+v, want C123/1700000.001", ref)
	}
	if len(calls) != 3 {
		t.Fatalf("len(calls) = %d, want 3: %+v", len(calls), calls)
	}
	if calls[0].conversation != "C123" || calls[0].session != "sess-42" || calls[0].auth != "Bearer secret-tok" {
		t.Errorf("calls[0] = %+v", calls[0])
	}
	if calls[1].conversation != "C123:1700000.001" || calls[1].session != "" {
		t.Errorf("calls[1] = %+v, want thread reply on C123:1700000.001 without session header", calls[1])
	}
	if calls[2].conversation != "C123:1700000.001" || calls[2].text != "line-three" {
		t.Errorf("calls[2] = %+v", calls[2])
	}
}

func TestClient_ReplaceAndSentinels(t *testing.T) {
	var statusToReturn atomic.Int32
	statusToReturn.Store(http.StatusOK)

	var gotMethod, gotPath, gotText string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		var p map[string]string
		_ = json.Unmarshal(b, &p)
		gotText = p["text"]
		code := int(statusToReturn.Load())
		w.WriteHeader(code)
		if code == http.StatusOK {
			_, _ = w.Write([]byte(`{"channel":"C123","id":"m-1"}`))
		} else {
			_, _ = w.Write([]byte(`{"error":"boom"}`))
		}
	}))
	defer srv.Close()

	c, err := New(Config{
		Endpoint:      srv.URL,
		Channel:       "C123",
		Token:         "tok",
		MaxChunkRunes: 20,
		Sleep:         func(context.Context, time.Duration) error { return nil },
	})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	// Happy path PUT.
	ref, err := c.Replace(context.Background(), Ref{Channel: "C123", ID: "m-1"}, "updated body")
	if err != nil {
		t.Fatalf("Replace() error: %v", err)
	}
	if gotMethod != http.MethodPut || gotPath != "/v1/messages/C123/m-1" || gotText != "updated body" {
		t.Errorf("PUT request = %s %s text=%q", gotMethod, gotPath, gotText)
	}
	if ref.ID != "m-1" {
		t.Errorf("ref = %+v", ref)
	}

	// Oversized text -> ErrSendFullText without calling server.
	gotPath = ""
	_, err = c.Replace(context.Background(), Ref{Channel: "C123", ID: "m-1"}, strings.Repeat("x", 25))
	if !errors.Is(err, ErrSendFullText) {
		t.Errorf("Replace(oversized) err = %v, want ErrSendFullText", err)
	}
	if gotPath != "" {
		t.Errorf("expected no HTTP call on ErrSendFullText, got path %q", gotPath)
	}

	// 501 -> ErrEditUnsupported (no retry).
	statusToReturn.Store(http.StatusNotImplemented)
	_, err = c.Replace(context.Background(), Ref{Channel: "C123", ID: "m-1"}, "short")
	if !errors.Is(err, ErrEditUnsupported) {
		t.Errorf("Replace(501) err = %v, want ErrEditUnsupported", err)
	}

	// 404 -> ErrNoSuchMessage.
	statusToReturn.Store(http.StatusNotFound)
	_, err = c.Replace(context.Background(), Ref{Channel: "C123", ID: "m-1"}, "short")
	if !errors.Is(err, ErrNoSuchMessage) {
		t.Errorf("Replace(404) err = %v, want ErrNoSuchMessage", err)
	}

	// 403 -> ErrDenied.
	statusToReturn.Store(http.StatusForbidden)
	_, err = c.Replace(context.Background(), Ref{Channel: "C123", ID: "m-1"}, "short")
	if !errors.Is(err, ErrDenied) {
		t.Errorf("Replace(403) err = %v, want ErrDenied", err)
	}
}

func TestClient_RetriesOn429And503(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if n == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte("rate limited"))
			return
		}
		if n == 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("try again"))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"channel":"C123","id":"ok-3"}`))
	}))
	defer srv.Close()

	var sleeps int
	c, err := New(Config{
		Endpoint:   srv.URL,
		Channel:    "C123",
		MaxRetries: 3,
		Sleep: func(_ context.Context, _ time.Duration) error {
			sleeps++
			return nil
		},
	})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	ref, err := c.Post(context.Background(), "hello", "")
	if err != nil {
		t.Fatalf("Post() after retries error: %v", err)
	}
	if ref.ID != "ok-3" || attempts.Load() != 3 || sleeps != 2 {
		t.Errorf("ref=%+v attempts=%d sleeps=%d", ref, attempts.Load(), sleeps)
	}
}
