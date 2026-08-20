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

	"github.com/go-steer/core-agent/v2/internal/attachclient"
	"github.com/go-steer/core-agent/v2/pkg/attach"
)

// titleServer records what the /title endpoint was actually sent, so
// the tests can tell a request that says "clear it" apart from one
// that says nothing — the distinction the endpoint is built around.
type titleServer struct {
	*httptest.Server

	mu       sync.Mutex
	requests []attach.SessionTitleRequest
	resp     attach.SessionTitleResponse
}

func startTitleServer(t *testing.T) *titleServer {
	t.Helper()
	ts := &titleServer{resp: attach.SessionTitleResponse{Session: "s1", Title: "renamed", Persisted: true}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /sessions/{sid}/title", func(w http.ResponseWriter, r *http.Request) {
		var req attach.SessionTitleRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		ts.mu.Lock()
		ts.requests = append(ts.requests, req)
		resp := ts.resp
		ts.mu.Unlock()
		_ = json.NewEncoder(w).Encode(resp)
	})
	ts.Server = httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func newTitleAdapter(t *testing.T, ts *titleServer) *Adapter {
	t.Helper()
	parsed, err := attachclient.ParseURL(ts.URL + "/sessions/s1")
	if err != nil {
		t.Fatal(err)
	}
	return New(attachclient.New(parsed, "", 0), "/sessions/s1")
}

func (ts *titleServer) sent(t *testing.T) []attach.SessionTitleRequest {
	t.Helper()
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return append([]attach.SessionTitleRequest(nil), ts.requests...)
}

func TestSlashTitle_SetsTheName(t *testing.T) {
	t.Parallel()
	ts := startTitleServer(t)
	a := newTitleAdapter(t, ts)

	res, err := a.InvokeSlash(context.Background(), "title", "  renamed  ")
	if err != nil {
		t.Fatalf("InvokeSlash: %v", err)
	}
	sent := ts.sent(t)
	if len(sent) != 1 || sent[0].Title == nil || *sent[0].Title != "renamed" {
		t.Fatalf("server saw %+v, want one request with title %q", sent, "renamed")
	}
	// The message reports the daemon's stored value, not the typed one.
	if !strings.Contains(res.SystemMessage, `"renamed"`) {
		t.Errorf("systemMessage = %q, want it to name the stored title", res.SystemMessage)
	}
}

// A bare /title must not clear. The endpoint keeps "omitted" and
// "empty" apart so a request that says nothing can't wipe a name;
// making the bare slash the destructive form would reintroduce that
// hazard one layer up, where operators type a bare command precisely
// to find out what it does.
func TestSlashTitle_BareFormPrintsUsageAndSendsNothing(t *testing.T) {
	t.Parallel()
	ts := startTitleServer(t)
	a := newTitleAdapter(t, ts)

	res, err := a.InvokeSlash(context.Background(), "title", "   ")
	if err != nil {
		t.Fatalf("InvokeSlash: %v", err)
	}
	if got := ts.sent(t); len(got) != 0 {
		t.Errorf("bare /title sent %+v to the daemon, want no request at all", got)
	}
	if !strings.Contains(res.SystemMessage, "--clear") {
		t.Errorf("systemMessage = %q, want the usage line naming --clear", res.SystemMessage)
	}
}

func TestSlashTitle_ClearSendsEmptyString(t *testing.T) {
	t.Parallel()
	ts := startTitleServer(t)
	ts.resp = attach.SessionTitleResponse{Session: "s1", Persisted: true}
	a := newTitleAdapter(t, ts)

	res, err := a.InvokeSlash(context.Background(), "title", "--clear")
	if err != nil {
		t.Fatalf("InvokeSlash: %v", err)
	}
	sent := ts.sent(t)
	// Sent, not omitted: the daemon 400s an omitted key, so a client
	// that dropped the field would turn a clear into an error.
	if len(sent) != 1 || sent[0].Title == nil || *sent[0].Title != "" {
		t.Fatalf(`server saw %+v, want one request carrying "title":""`, sent)
	}
	if !strings.Contains(res.SystemMessage, "cleared") {
		t.Errorf("systemMessage = %q, want it to say the title was cleared", res.SystemMessage)
	}
}

// persisted=false with a detail is the case an operator has to see:
// the rename is live but reverts at the next restart.
func TestSlashTitle_SurfacesAFailedPersist(t *testing.T) {
	t.Parallel()
	ts := startTitleServer(t)
	ts.resp = attach.SessionTitleResponse{
		Session: "s1",
		Title:   "renamed",
		Detail:  "title set for this process only — persisting it failed: disk on fire",
	}
	a := newTitleAdapter(t, ts)

	res, err := a.InvokeSlash(context.Background(), "title", "renamed")
	if err != nil {
		t.Fatalf("InvokeSlash: %v", err)
	}
	if !strings.Contains(res.SystemMessage, "persisting it failed") {
		t.Errorf("systemMessage = %q, want the daemon's detail carried through", res.SystemMessage)
	}
}

// The slash has to be advertised, or it exists and nobody can find it.
func TestSlashTitle_IsAdvertised(t *testing.T) {
	t.Parallel()
	ts := startTitleServer(t)
	for _, spec := range newTitleAdapter(t, ts).SlashCommands() {
		if spec.Name == "title" {
			return
		}
	}
	t.Error("SlashCommands() does not list /title")
}
