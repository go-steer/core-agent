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
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/core-agent/v2/pkg/auth"
	"github.com/go-steer/core-agent/v2/pkg/permissions"
)

func respondBody(t *testing.T, r PromptResponse) string {
	t.Helper()
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestPermsRespond_DenyCarriesTheReason is #1165 at the wire: the
// reason arrives on the Approval the gate reads, on one line.
func TestPermsRespond_DenyCarriesTheReason(t *testing.T) {
	t.Parallel()
	mux, broker := promptRouteFixture(t)
	done, id := askInBackground(t, broker)

	r, rr := respondRequest(t, respondBody(t, PromptResponse{
		ID: id, Decision: "deny", Reason: "  use the staging\n\tcluster  ",
	}), auth.Caller{}, "")
	mux.ServeHTTP(rr, r)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", rr.Code, rr.Body.String())
	}
	got := <-done
	if got.approval.Decision != permissions.DecisionDeny {
		t.Fatalf("decision = %v, want deny", got.approval.Decision)
	}
	if got.approval.Reason != "use the staging cluster" {
		t.Errorf("reason = %q, want %q: whitespace runs collapse so the model reads one line", got.approval.Reason, "use the staging cluster")
	}
}

// A refused reason must leave the prompt answerable; otherwise one
// malformed request costs the operator the prompt.
func TestPermsRespond_RefusedReasonLeavesThePromptPending(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		decision string
		reason   string
		wantBody string
	}{
		{"reason on an approval", "allow-once", "but only in staging", `only with decision "deny"`},
		{"reason over the limit", "deny", strings.Repeat("x", MaxDenyReasonBytes+1), "over the 500-byte limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mux, broker := promptRouteFixture(t)
			done, id := askInBackground(t, broker)

			r, rr := respondRequest(t, respondBody(t, PromptResponse{ID: id, Decision: tc.decision, Reason: tc.reason}), auth.Caller{}, "")
			mux.ServeHTTP(rr, r)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%q", rr.Code, rr.Body.String())
			}
			if !strings.Contains(rr.Body.String(), tc.wantBody) {
				t.Errorf("body = %q, want it to contain %q", rr.Body.String(), tc.wantBody)
			}
			if len(broker.Pending()) != 1 {
				t.Fatal("a refused reason consumed the prompt")
			}

			r, rr = respondRequest(t, respondBody(t, PromptResponse{ID: id, Decision: "deny"}), auth.Caller{}, "")
			mux.ServeHTTP(rr, r)
			if rr.Code != http.StatusOK {
				t.Fatalf("retry: status = %d, want 200; body=%q", rr.Code, rr.Body.String())
			}
			<-done
		})
	}
}

// The bound is on the normalized text and is inclusive, and whitespace
// alone is no reason at all — so it is fine on an approval.
func TestPermsRespond_ReasonBoundaries(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		decision   string
		reason     string
		wantReason string
	}{
		{"exactly the limit", "deny", strings.Repeat("x", MaxDenyReasonBytes), strings.Repeat("x", MaxDenyReasonBytes)},
		{"over the limit only before collapsing", "deny", "a" + strings.Repeat(" ", MaxDenyReasonBytes) + "b", "a b"},
		{"whitespace on an approval", "allow-once", " \n\t ", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mux, broker := promptRouteFixture(t)
			done, id := askInBackground(t, broker)
			r, rr := respondRequest(t, respondBody(t, PromptResponse{ID: id, Decision: tc.decision, Reason: tc.reason}), auth.Caller{}, "")
			mux.ServeHTTP(rr, r)
			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%q", rr.Code, rr.Body.String())
			}
			if got := <-done; got.approval.Reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", got.approval.Reason, tc.wantReason)
			}
		})
	}
}

// End to end through a real gate: the operator's reason is in the
// error the tool call returns, which is the text the model reads.
func TestPermsRespond_ReasonIsInTheRefusalTheModelReads(t *testing.T) {
	t.Parallel()
	mux, broker := promptRouteFixture(t)
	g := permissions.New(permissions.Options{Mode: permissions.ModeAsk, Prompter: broker})

	errc := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		errc <- g.CheckGeneric(ctx, "deploy", "restart deploy/api")
	}()
	var id string
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline) && id == ""; time.Sleep(time.Millisecond) {
		if p := broker.Pending(); len(p) == 1 {
			id = p[0].ID
		}
	}
	if id == "" {
		t.Fatal("no prompt became pending")
	}

	r, rr := respondRequest(t, respondBody(t, PromptResponse{ID: id, Decision: "deny", Reason: "restart the canary first"}), auth.Caller{}, "")
	mux.ServeHTTP(rr, r)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%q", rr.Code, rr.Body.String())
	}
	err := <-errc
	want := `The operator's reason: "restart the canary first".`
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("refusal = %v\nwant it to contain %q", err, want)
	}
}
