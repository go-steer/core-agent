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

package coretuiremote

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-steer/core-agent/internal/attachclient"
)

// TestAdapter_Interrupt_ForwardsToRemoteEndpoint pins the happy-path
// wire hop: coretui's /interrupt slash → RemoteInterrupter.Interrupt
// on our Adapter → POST /sessions/<sid>/interrupt on the daemon.
// Without this, /interrupt in observer mode short-circuits with
// "no turn in flight" (see the 2026-07-17 demo drive where a
// runaway list_skills loop stayed live because the TUI slash
// couldn't reach the daemon endpoint).
func TestAdapter_Interrupt_ForwardsToRemoteEndpoint(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	var lastSessionPath atomic.Value // string

	mux := http.NewServeMux()
	mux.HandleFunc("POST /sessions/{sid}/interrupt", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		lastSessionPath.Store(r.URL.Path)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"interrupted": true,
			"session":     "s1",
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	parsed, err := attachclient.ParseURL(srv.URL + "/sessions/s1")
	if err != nil {
		t.Fatal(err)
	}
	client := attachclient.New(parsed, "", 0)
	a := New(client, "/sessions/s1")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := a.Interrupt(ctx); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("expected 1 interrupt call, got %d", got)
	}
	if got, _ := lastSessionPath.Load().(string); got != "/sessions/s1/interrupt" {
		t.Errorf("session path routed to %q, want /sessions/s1/interrupt", got)
	}
}

// TestAdapter_Interrupt_SurfacesEndpointFailure pins that HTTP
// failures propagate — the /interrupt slash renders them as inline
// RoleError rows so the operator sees the failure instead of a
// false "cancelled" confirmation.
func TestAdapter_Interrupt_SurfacesEndpointFailure(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /sessions/{sid}/interrupt", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "session not found", http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	parsed, err := attachclient.ParseURL(srv.URL + "/sessions/s1")
	if err != nil {
		t.Fatal(err)
	}
	client := attachclient.New(parsed, "", 0)
	a := New(client, "/sessions/s1")

	err = a.Interrupt(context.Background())
	if err == nil {
		t.Fatal("expected error propagation on 404, got nil")
	}
	// Underlying error should include the status so the operator can
	// distinguish "network" from "endpoint said no."
	if !strings.Contains(err.Error(), "404") && !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected 404-shaped error text, got %q", err.Error())
	}
}

// (ctx-propagation is covered at attachclient.Client's HTTP layer,
// which uses http.NewRequestWithContext under the hood — no need to
// re-test the interaction here.)
