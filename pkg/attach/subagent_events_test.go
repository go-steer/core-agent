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
	"io"
	"net/http"
	"testing"

	"google.golang.org/adk/session"

	"github.com/go-steer/core-agent/v2/pkg/eventlog"
)

// appendBranchedEvent writes one event into sessionID tagged with
// branch, mirroring what the subagent runners do: they wrap the
// PARENT's session.Service so the child's events land in the same
// database under a derived session row plus a Branch label.
func appendBranchedEvent(t *testing.T, h *eventlog.Handle, appName, userID, sessionID, branch, id string) {
	t.Helper()
	ctx := context.Background()
	got, err := h.Service.Get(ctx, &session.GetRequest{
		AppName: appName, UserID: userID, SessionID: sessionID,
	})
	if err != nil || got == nil || got.Session == nil {
		if _, cerr := h.Service.Create(ctx, &session.CreateRequest{
			AppName: appName, UserID: userID, SessionID: sessionID,
		}); cerr != nil {
			t.Fatalf("session Create(%s): %v", sessionID, cerr)
		}
		got, err = h.Service.Get(ctx, &session.GetRequest{
			AppName: appName, UserID: userID, SessionID: sessionID,
		})
		if err != nil {
			t.Fatalf("session Get(%s): %v", sessionID, err)
		}
	}
	ev := session.NewEvent(id)
	ev.Author = "test"
	ev.Branch = branch
	ev.CustomMetadata = map[string]any{"id": id}
	if err := h.Service.AppendEvent(ctx, got.Session, ev); err != nil {
		t.Fatalf("AppendEvent(%s): %v", id, err)
	}
}

// getSubagentEvents performs the GET and decodes the response,
// failing the test on any status other than want.
func getSubagentEvents(t *testing.T, url string, want int) SubagentEventsResponse {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		t.Fatalf("GET %s status %d (want %d): %s", url, resp.StatusCode, want, body)
	}
	var out SubagentEventsResponse
	if want == http.StatusOK {
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatalf("decode %s: %v (body %s)", url, err, body)
		}
	}
	return out
}

// eventIDs pulls the synthetic ids back out of the returned frames so
// assertions read as "which events came back", not "how many".
func eventIDs(t *testing.T, r SubagentEventsResponse) []string {
	t.Helper()
	out := make([]string, 0, len(r.Events))
	for _, f := range r.Events {
		if f.Event == nil {
			t.Fatalf("frame seq=%d has nil event", f.Seq)
		}
		id, _ := f.Event.CustomMetadata["id"].(string)
		out = append(out, id)
	}
	return out
}

func sameIDs(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TestSubagentEvents_ReturnsBranchScopedTurns is the core #638 case:
// a subagent's inner turns are persisted, and the operator can now
// read them back scoped to that subagent — across every branch
// spelling the launch paths use, including nested descendants, and
// without leaking the parent's own turns or a sibling subagent's.
//
// Fails on pre-fix code: the route does not exist (404).
func TestSubagentEvents_ReturnsBranchScopedTurns(t *testing.T) {
	t.Parallel()
	h, cleanupLog := openTestEventLog(t)
	defer cleanupLog()

	const (
		app = "core-agent"
		usr = "u"
		sid = "s1"
	)
	// Parent turn — same session row, no branch.
	appendBranchedEvent(t, h, app, usr, sid, "", "parent-turn")
	// Sync subagent tool: bare name as the branch, per-invocation row.
	appendBranchedEvent(t, h, app, usr, sid+":sub:cluster:inv1", "cluster", "sync-turn")
	// Background / spawn_agent: "bg." prefix, deterministic row.
	appendBranchedEvent(t, h, app, usr, sid+":sub:bg.cluster", "bg.cluster", "bg-turn")
	// A subagent nested inside the background one.
	appendBranchedEvent(t, h, app, usr, sid+":sub:bg.cluster:sub:bg.child", "bg.cluster.bg.child", "nested-turn")
	// A different subagent — must not appear.
	appendBranchedEvent(t, h, app, usr, sid+":sub:bg.other", "bg.other", "other-turn")
	// A subagent whose name merely starts with "cluster" — must not
	// appear either (prefix match is separator-aware, not substring).
	appendBranchedEvent(t, h, app, usr, sid+":sub:bg.clusterly", "bg.clusterly", "clusterly-turn")

	reg := NewSessionRegistry()
	if _, err := reg.Register(&eventfulRegistrant{
		stubRegistrant: stubRegistrant{app: app, user: usr, sid: sid},
		handle:         h,
	}); err != nil {
		t.Fatal(err)
	}
	base, cleanupSrv := startTestServer(t, reg)
	defer cleanupSrv()

	for _, url := range []string{
		base + "/sessions/core-agent/s1/agents/cluster/events", // qualified
		base + "/sessions/s1/agents/cluster/events",            // shortcut
	} {
		got := getSubagentEvents(t, url, http.StatusOK)
		want := []string{"sync-turn", "bg-turn", "nested-turn"}
		if ids := eventIDs(t, got); !sameIDs(ids, want) {
			t.Errorf("%s events = %v, want %v", url, ids, want)
		}
		if got.Agent != "cluster" || got.ParentSessionID != sid {
			t.Errorf("%s echo = %q/%q", url, got.Agent, got.ParentSessionID)
		}
		if len(got.Branches) != 4 || got.Branches[0] != "cluster" || got.Branches[1] != "bg.cluster" {
			t.Errorf("%s branches = %v", url, got.Branches)
		}
		if got.Truncated {
			t.Errorf("%s reported truncated for a 3-event result", url)
		}
	}

	// The documented corollary of anchored prefix matching: the nested
	// subagent above is reachable under its ancestor's name (asserted
	// as nested-turn), and NOT under its own. Pinned so that widening
	// to a leading-wildcard LIKE later is a deliberate change with a
	// failing test attached, not a silent one.
	got := getSubagentEvents(t, base+"/sessions/core-agent/s1/agents/child/events", http.StatusOK)
	if ids := eventIDs(t, got); len(ids) != 0 {
		t.Errorf("nested subagent by its own name = %v, want [] (prefix match is anchored)", ids)
	}
}

// TestSubagentEvents_UnderscoreNameIsNotAWildcard guards the LIKE
// escaping: '_' is SQL's single-character wildcard, so an unescaped
// prefix for a subagent named "gke_cluster" would also match a
// sibling named "gkeXcluster".
//
// Fails on pre-fix code twice over: no route, and — once routed — the
// unescaped LIKE in applyQueryFilters returns the decoy.
func TestSubagentEvents_UnderscoreNameIsNotAWildcard(t *testing.T) {
	t.Parallel()
	h, cleanupLog := openTestEventLog(t)
	defer cleanupLog()

	const (
		app = "core-agent"
		usr = "u"
		sid = "s1"
	)
	appendBranchedEvent(t, h, app, usr, sid+":sub:bg.gke_cluster:sub:bg.c", "bg.gke_cluster.bg.c", "real-nested")
	appendBranchedEvent(t, h, app, usr, sid+":sub:bg.gkeXcluster:sub:bg.c", "bg.gkeXcluster.bg.c", "decoy-nested")

	reg := NewSessionRegistry()
	if _, err := reg.Register(&eventfulRegistrant{
		stubRegistrant: stubRegistrant{app: app, user: usr, sid: sid},
		handle:         h,
	}); err != nil {
		t.Fatal(err)
	}
	base, cleanupSrv := startTestServer(t, reg)
	defer cleanupSrv()

	got := getSubagentEvents(t, base+"/sessions/core-agent/s1/agents/gke_cluster/events", http.StatusOK)
	if ids := eventIDs(t, got); !sameIDs(ids, []string{"real-nested"}) {
		t.Errorf("events = %v, want [real-nested] — '_' leaked as a LIKE wildcard", ids)
	}
}

// TestSubagentEvents_Pagination checks the ?limit= / ?since= /
// next_since / truncated contract, including that the last page
// reports truncated=false rather than leaving a client looping.
func TestSubagentEvents_Pagination(t *testing.T) {
	t.Parallel()
	h, cleanupLog := openTestEventLog(t)
	defer cleanupLog()

	const (
		app = "core-agent"
		usr = "u"
		sid = "s1"
	)
	for i := range 5 {
		appendBranchedEvent(t, h, app, usr, sid+":sub:bg.w", "bg.w", fmt.Sprintf("turn-%d", i))
	}

	reg := NewSessionRegistry()
	if _, err := reg.Register(&eventfulRegistrant{
		stubRegistrant: stubRegistrant{app: app, user: usr, sid: sid},
		handle:         h,
	}); err != nil {
		t.Fatal(err)
	}
	base, cleanupSrv := startTestServer(t, reg)
	defer cleanupSrv()

	var seen []string
	since := int64(0)
	for page := 0; page < 5; page++ {
		url := fmt.Sprintf("%s/sessions/core-agent/s1/agents/w/events?limit=2&since=%d", base, since)
		got := getSubagentEvents(t, url, http.StatusOK)
		seen = append(seen, eventIDs(t, got)...)
		if !got.Truncated {
			break
		}
		if got.NextSince <= since {
			t.Fatalf("page %d: next_since %d did not advance past %d", page, got.NextSince, since)
		}
		since = got.NextSince
	}
	want := []string{"turn-0", "turn-1", "turn-2", "turn-3", "turn-4"}
	if !sameIDs(seen, want) {
		t.Errorf("paged events = %v, want %v", seen, want)
	}
}

// TestSubagentEvents_LimitClamped verifies an over-large ?limit= is
// clamped instead of honored — the endpoint must not be a lever for
// dumping the whole log in one request.
func TestSubagentEvents_LimitClamped(t *testing.T) {
	t.Parallel()
	if got := parseSubagentEventsLimit("999999"); got != subagentEventsMaxLimit {
		t.Errorf("parseSubagentEventsLimit(999999) = %d, want %d", got, subagentEventsMaxLimit)
	}
	for _, in := range []string{"", "0", "-3", "abc"} {
		if got := parseSubagentEventsLimit(in); got != subagentEventsDefaultLimit {
			t.Errorf("parseSubagentEventsLimit(%q) = %d, want default %d", in, got, subagentEventsDefaultLimit)
		}
	}
	if got := parseSubagentEventsLimit(" 7 "); got != 7 {
		t.Errorf("parseSubagentEventsLimit(\" 7 \") = %d, want 7", got)
	}
}

// TestSubagentEvents_UnknownNameIsEmptyNot404 documents the
// deliberate choice: the endpoint reads the log, not the live
// manager, so "never heard of it" and "ran and finished three
// restarts ago" are indistinguishable — and answering 404 for the
// first would be a lie for the second.
func TestSubagentEvents_UnknownNameIsEmptyNot404(t *testing.T) {
	t.Parallel()
	h, cleanupLog := openTestEventLog(t)
	defer cleanupLog()

	reg := NewSessionRegistry()
	if _, err := reg.Register(&eventfulRegistrant{
		stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s1"},
		handle:         h,
	}); err != nil {
		t.Fatal(err)
	}
	base, cleanupSrv := startTestServer(t, reg)
	defer cleanupSrv()

	got := getSubagentEvents(t, base+"/sessions/core-agent/s1/agents/nobody/events", http.StatusOK)
	if len(got.Events) != 0 {
		t.Errorf("events = %v, want none", eventIDs(t, got))
	}
	if len(got.Branches) != 4 {
		t.Errorf("branches = %v, want the four searched spellings", got.Branches)
	}
}

// TestSubagentEvents_BadNameIs400 rejects names that could never have
// produced a branch label, so a query key can't smuggle separators.
func TestSubagentEvents_BadNameIs400(t *testing.T) {
	t.Parallel()
	h, cleanupLog := openTestEventLog(t)
	defer cleanupLog()

	reg := NewSessionRegistry()
	if _, err := reg.Register(&eventfulRegistrant{
		stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s1"},
		handle:         h,
	}); err != nil {
		t.Fatal(err)
	}
	base, cleanupSrv := startTestServer(t, reg)
	defer cleanupSrv()

	for _, name := range []string{"bg.cluster", "a%20b", "%20lead"} {
		getSubagentEvents(t, base+"/sessions/core-agent/s1/agents/"+name+"/events", http.StatusBadRequest)
	}
}

// TestSubagentEvents_NoEventLogIs412 matches /events' contract: no
// --session-db means no history to read, and that's a precondition
// failure rather than an empty success.
func TestSubagentEvents_NoEventLogIs412(t *testing.T) {
	t.Parallel()
	reg := NewSessionRegistry()
	if _, err := reg.Register(&stubRegistrant{app: "core-agent", user: "u", sid: "s1"}); err != nil {
		t.Fatal(err)
	}
	base, cleanupSrv := startTestServer(t, reg)
	defer cleanupSrv()

	getSubagentEvents(t, base+"/sessions/core-agent/s1/agents/w/events", http.StatusPreconditionFailed)
}
