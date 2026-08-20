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
	"fmt"
	"net/http"
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/auth"
)

// POST /inject echoes the prompt_id it assigned (#840). Without it a
// client that puts a chat thread in front of a session cannot key
// anything by turn: the id exists at the moment of the inject and was
// being thrown away.

// identifyingRegistrant implements the full IdentifyingInjector +
// IdentifyingDeferredInjector pair, minting a distinguishable id per
// call so a test can tell WHICH path produced the id in the response
// rather than just that some string came back.
type identifyingRegistrant struct {
	deferringRegistrant
	n int
}

func (i *identifyingRegistrant) nextID(kind string) string {
	i.n++
	return fmt.Sprintf("%s-%d", kind, i.n)
}

func (i *identifyingRegistrant) InjectAsContextWithID(ctx context.Context, message string, caller auth.Caller) (string, error) {
	return i.nextID("woke"), i.InjectAsContext(ctx, message, caller)
}

func (i *identifyingRegistrant) QueueAsContextWithID(ctx context.Context, message string, caller auth.Caller) (string, error) {
	return i.nextID("queued"), i.QueueAsContext(ctx, message, caller)
}

func newIdentifyingRegistrant(t *testing.T) *identifyingRegistrant {
	t.Helper()
	return &identifyingRegistrant{deferringRegistrant: *newDeferringRegistrant(t)}
}

// postRaw POSTs body and returns the status plus the 200 body decoded
// as a generic object, so a test can assert on a key's ABSENCE — which
// a typed decode into InjectResponse cannot distinguish from "".
func postRaw(t *testing.T, url string, body any) (int, map[string]any) {
	t.Helper()
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	resp, err := http.Post(url, "application/json", bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("decode 200 body: %v", err)
		}
	}
	return resp.StatusCode, out
}

// sessionFixture stands up a server over ag and returns its
// /sessions/core-agent/s1 base, for the handlers injectFixture's
// /inject-only URL doesn't reach.
func sessionFixture(t *testing.T, ag Registrant) string {
	t.Helper()
	reg := NewSessionRegistry()
	if _, err := reg.Register(ag); err != nil {
		t.Fatal(err)
	}
	base, cleanup := startTestServer(t, reg)
	t.Cleanup(cleanup)
	return base + "/sessions/core-agent/s1"
}

// Both deliveries report an id. A deferred message still drains into a
// turn, so a client keying state by turn needs the handle either way —
// and a response shape that varies with a request flag is one clients
// read wrong.
func TestInject_ReportsThePromptID(t *testing.T) {
	for _, tc := range []struct {
		name   string
		body   map[string]any
		wantID string
	}{
		{"waking", map[string]any{"message": "what is the cluster doing"}, "woke-1"},
		{"deferred", map[string]any{"message": "fyi", "wake": false}, "queued-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ag := newIdentifyingRegistrant(t)
			url := injectFixture(t, ag)

			code, body := postInject(t, url, tc.body)
			if code != http.StatusOK {
				t.Fatalf("status = %d, want 200", code)
			}
			if body.PromptID != tc.wantID {
				t.Errorf("prompt_id = %q, want %q (the id the registrant assigned on this path)",
					body.PromptID, tc.wantID)
			}
		})
	}
}

// The id must come from the call that queued the message, not from a
// second one: an id minted by an extra inject would name a message
// nobody sent, and the client would wait forever for a turn that
// answers it.
func TestInject_PromptIDComesFromTheOneDelivery(t *testing.T) {
	ag := newIdentifyingRegistrant(t)
	url := injectFixture(t, ag)

	if code, _ := postInject(t, url, map[string]any{"message": "one"}); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if ag.injectCt != 1 {
		t.Errorf("waking inject path ran %d times, want exactly 1", ag.injectCt)
	}
	if len(ag.queued) != 0 {
		t.Errorf("queued = %v, want nothing on the waking path", ag.queued)
	}
	if ag.n != 1 {
		t.Errorf("ids minted = %d, want 1", ag.n)
	}
}

// The capability is optional, so a registrant that predates it must
// still get its message — and the response must OMIT the key rather
// than carry an empty one. There is no informative empty id: absent is
// the honest encoding of "this daemon cannot name one", and it is what
// a client already has to tolerate from an older build.
func TestInject_OmitsPromptIDWithoutTheCapability(t *testing.T) {
	for _, tc := range []struct {
		name string
		body map[string]any
	}{
		{"waking", map[string]any{"message": "hello"}},
		{"deferred", map[string]any{"message": "hello", "wake": false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// deferringRegistrant has ContextInjector and
			// DeferredInjector but neither *WithID method.
			ag := newDeferringRegistrant(t)
			url := injectFixture(t, ag)

			code, body := postRaw(t, url, tc.body)
			if code != http.StatusOK {
				t.Fatalf("status = %d, want 200", code)
			}
			if _, ok := body["prompt_id"]; ok {
				t.Errorf("prompt_id present (%v) for a registrant that cannot name one; want the key omitted", body["prompt_id"])
			}
			// Degrading must cost the id and nothing else.
			if delivered := ag.injectCt + len(ag.queued); delivered != 1 {
				t.Errorf("message deliveries = %d, want 1", delivered)
			}
		})
	}
}

// A wake carrying a prompt IS an inject, so it reports the id on the
// same terms. A bare wake queues nothing and must name nothing.
func TestWake_ReportsThePromptIDOnlyWhenItCarriesOne(t *testing.T) {
	for _, tc := range []struct {
		name   string
		body   map[string]any
		wantID string
	}{
		{"with prompt", map[string]any{"prompt": "rescan now"}, "woke-1"},
		{"bare", map[string]any{}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ag := newIdentifyingRegistrant(t)
			url := sessionFixture(t, ag) + "/wake"

			code, body := postRaw(t, url, tc.body)
			if code != http.StatusOK {
				t.Fatalf("status = %d, want 200", code)
			}
			got, _ := body["prompt_id"].(string)
			if got != tc.wantID {
				t.Errorf("prompt_id = %q, want %q", got, tc.wantID)
			}
			if tc.wantID == "" {
				if _, ok := body["prompt_id"]; ok {
					t.Error("bare wake reported a prompt_id key; want it omitted")
				}
			}
			// The pre-existing shape is untouched either way.
			if body["woken"] != "s1" {
				t.Errorf("woken = %v, want \"s1\"", body["woken"])
			}
		})
	}
}
