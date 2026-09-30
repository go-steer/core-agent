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
	"sync"
	"testing"

	coretui "github.com/go-steer/core-tui/tui"

	"github.com/go-steer/core-agent/v2/pkg/attach"
	"github.com/go-steer/core-agent/v2/pkg/permissions"
)

// The attached chip starts at the daemon's live mode, and each press
// posts the mode it then shows (#1168).
func TestFetchPermissionMode_StartsAtTheDaemonsModeAndPosts(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var posted []string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /sessions/{sid}/perms", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(attach.PermsInfo{Mode: "plan"})
	})
	mux.HandleFunc("POST /sessions/{sid}/perms/mode", func(w http.ResponseWriter, r *http.Request) {
		var req attach.PermModeRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		posted = append(posted, req.Mode)
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(attach.PermModeResponse{Previous: "plan", Mode: req.Mode})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	w := newPauseAdapter(t, srv).FetchPermissionMode(context.Background())
	if w.Set == nil {
		t.Fatal("Set is nil: the chip would be hidden")
	}
	if w.Initial != coretui.PermissionModePlan {
		t.Errorf("Initial = %v, want plan", w.Initial)
	}
	for _, m := range []coretui.PermissionMode{coretui.PermissionModeBypass, coretui.PermissionModeDefault, coretui.PermissionModeAcceptEdits} {
		if err := w.Set(m); err != nil {
			t.Fatalf("Set(%v): %v", m, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{"yolo", "ask", "acceptEdits"}
	if len(posted) != len(want) {
		t.Fatalf("posted = %v, want %v", posted, want)
	}
	for i := range want {
		if posted[i] != want[i] {
			t.Errorf("posted = %v, want %v", posted, want)
			break
		}
	}
}

// A refusal comes back as Set's error, which core-tui uses to roll the
// chip back and show why.
func TestFetchPermissionMode_RefusalIsSetsError(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /sessions/{sid}/perms", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(attach.PermsInfo{Mode: "ask"})
	})
	mux.HandleFunc("POST /sessions/{sid}/perms/mode", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "session not found", http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	w := newPauseAdapter(t, srv).FetchPermissionMode(context.Background())
	if err := w.Set(coretui.PermissionModeBypass); err == nil {
		t.Error("Set against a refusing daemon returned nil; the chip would show a mode the daemon doesn't have")
	}
}

// Without the daemon's mode there is no honest starting value, so the
// chip stays hidden.
func TestFetchPermissionMode_UnreadableModeHidesTheChip(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /sessions/{sid}/perms", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	if w := newPauseAdapter(t, srv).FetchPermissionMode(context.Background()); w.Set != nil {
		t.Errorf("wiring = %+v, want the zero value (hidden chip)", w)
	}
}

// The attached chip must map modes exactly as the local one does
// (cmd/core-agent translateMode / translateModeBack), including
// "allow" showing as default.
func TestPermModeChipMapping(t *testing.T) {
	t.Parallel()
	cases := []struct {
		mode permissions.Mode
		chip coretui.PermissionMode
	}{
		{permissions.ModeAsk, coretui.PermissionModeDefault},
		{permissions.ModeAcceptEdits, coretui.PermissionModeAcceptEdits},
		{permissions.ModePlan, coretui.PermissionModePlan},
		{permissions.ModeYolo, coretui.PermissionModeBypass},
	}
	for _, c := range cases {
		if got := permModeToChip(c.mode); got != c.chip {
			t.Errorf("permModeToChip(%q) = %v, want %v", c.mode, got, c.chip)
		}
		if got := chipToPermMode(c.chip); got != c.mode {
			t.Errorf("chipToPermMode(%v) = %q, want %q", c.chip, got, c.mode)
		}
	}
	if got := permModeToChip(permissions.ModeAllow); got != coretui.PermissionModeDefault {
		t.Errorf("permModeToChip(allow) = %v, want default", got)
	}
}

// The chip is wired once and core-tui never rebinds it, so after a
// /switch it must not post to the session it started on: the operator
// is looking at a different session, and pressing the chip would
// change the one they left (#1168 review). It refuses, posts nothing,
// and works again after a switch back.
func TestFetchPermissionMode_RefusesAfterSwitch(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var posted []string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /sessions", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"sessions":[{"sessionID":"s1"},{"sessionID":"s2"}]}`))
	})
	mux.HandleFunc("GET /sessions/{sid}/perms", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(attach.PermsInfo{Mode: "ask"})
	})
	mux.HandleFunc("POST /sessions/{sid}/perms/mode", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		posted = append(posted, r.PathValue("sid"))
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(attach.PermModeResponse{Previous: "ask", Mode: "plan"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	a := newPauseAdapter(t, srv)
	w := a.FetchPermissionMode(context.Background())
	if w.Set == nil {
		t.Fatal("Set is nil: the chip would be hidden")
	}

	tgt, err := a.SwitchToSession("s2")
	if err != nil {
		t.Fatalf("SwitchToSession(s2): %v", err)
	}
	err = w.Set(coretui.PermissionModeBypass)
	if err == nil || !strings.Contains(err.Error(), "s1") || !strings.Contains(err.Error(), "re-attach to s2") {
		t.Errorf("Set after /switch = %v, want a refusal naming s1 and s2", err)
	}
	mu.Lock()
	if len(posted) != 0 {
		t.Errorf("Set after /switch posted to %v; the chip changed a session the operator had left", posted)
	}
	mu.Unlock()

	back, err := tgt.Agent.(*Adapter).SwitchToSession("s1")
	if err != nil {
		t.Fatalf("SwitchToSession(s1): %v", err)
	}
	_ = back
	if err := w.Set(coretui.PermissionModePlan); err != nil {
		t.Fatalf("Set after switching back: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(posted) != 1 || posted[0] != "s1" {
		t.Errorf("posted = %v, want [s1]", posted)
	}
}
