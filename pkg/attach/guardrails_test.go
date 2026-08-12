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
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"google.golang.org/adk/session"

	"github.com/go-steer/core-agent/v2/pkg/eventlog"
)

// guardrailRegistrant is a stubRegistrant that also answers the
// guardrail read + reset capabilities.
type guardrailRegistrant struct {
	stubRegistrant
	mu sync.Mutex

	info GuardrailInfo

	lastReq *GuardrailResetRequest
	resp    GuardrailResetResponse
	err     error
}

func (g *guardrailRegistrant) AttachGuardrails() GuardrailInfo {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.info
}

func (g *guardrailRegistrant) AttachResetGuardrail(req GuardrailResetRequest) (GuardrailResetResponse, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	cp := req
	g.lastReq = &cp
	return g.resp, g.err
}

func (g *guardrailRegistrant) request() *GuardrailResetRequest {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.lastReq
}

// guardrailReadOnlyRegistrant implements the read capability but NOT
// the resetter — the "no reset wired" 501 path.
type guardrailReadOnlyRegistrant struct {
	stubRegistrant
	info GuardrailInfo
}

func (g *guardrailReadOnlyRegistrant) AttachGuardrails() GuardrailInfo { return g.info }

func serveGuardrails(t *testing.T, ag Registrant) string {
	t.Helper()
	reg := NewSessionRegistry()
	if _, err := reg.Register(ag); err != nil {
		t.Fatal(err)
	}
	base, cleanup := startTestServer(t, reg)
	t.Cleanup(cleanup)
	return base
}

func postGuardrailReset(t *testing.T, base, body string) (*http.Response, GuardrailResetResponse) {
	t.Helper()
	resp, err := http.Post(base+"/sessions/core-agent/s1/guardrails/reset",
		"application/json", bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatalf("POST /guardrails/reset: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	var out GuardrailResetResponse
	// Error bodies are text/plain; decoding failure is fine there.
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp, out
}

func TestGuardrails_Read(t *testing.T) {
	t.Parallel()
	ag := &guardrailRegistrant{
		stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s1"},
		info: GuardrailInfo{
			Watchdog: WatchdogInfo{Mode: "enforce", Tripped: true, Reason: "runaway"},
			CostCeiling: CostCeilingInfo{
				MaxSessionUSD: 10, SessionCostUSD: 11, Tripped: true, WouldRetrip: true,
			},
			Halted: true,
		},
	}
	base := serveGuardrails(t, ag)

	resp, err := http.Get(base + "/sessions/core-agent/s1/guardrails")
	if err != nil {
		t.Fatalf("GET /guardrails: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	var got GuardrailInfo
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.Halted || !got.Watchdog.Tripped || !got.CostCeiling.WouldRetrip {
		t.Errorf("got = %+v, want the halted state round-tripped", got)
	}
	if got.CostCeiling.SessionCostUSD != 11 {
		t.Errorf("SessionCostUSD = %v, want 11", got.CostCeiling.SessionCostUSD)
	}
}

// A registrant with no guardrail capability gets 200 + zero state,
// matching the read-endpoint convention (/tools, /perms, …) rather
// than 501.
func TestGuardrails_ReadWithoutCapability(t *testing.T) {
	t.Parallel()
	base := serveGuardrails(t, &stubRegistrant{app: "core-agent", user: "u", sid: "s1"})
	resp, err := http.Get(base + "/sessions/core-agent/s1/guardrails")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	var got GuardrailInfo
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Halted {
		t.Errorf("no capability should report not-halted, got %+v", got)
	}
}

func TestGuardrailsReset_ClearsAndEchoesState(t *testing.T) {
	t.Parallel()
	ag := &guardrailRegistrant{
		stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s1"},
		resp: GuardrailResetResponse{
			Reset:      []string{GuardrailCostCeiling},
			Guardrails: GuardrailInfo{CostCeiling: CostCeilingInfo{MaxSessionUSD: 15}},
			Message:    "cleared cost_ceiling",
		},
	}
	base := serveGuardrails(t, ag)

	resp, out := postGuardrailReset(t, base, `{"guardrail":"cost_ceiling","additional_budget_usd":5}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	req := ag.request()
	if req == nil || req.Guardrail != GuardrailCostCeiling || req.AdditionalBudgetUSD != 5 {
		t.Fatalf("capability saw %+v, want the decoded request", req)
	}
	if len(out.Reset) != 1 || out.Reset[0] != GuardrailCostCeiling {
		t.Errorf("Reset = %v", out.Reset)
	}
	if out.Guardrails.CostCeiling.MaxSessionUSD != 15 {
		t.Errorf("post-reset state not echoed: %+v", out.Guardrails)
	}
}

// An empty body means "clear whatever tripped" — the common operator
// request. decodePOST (used by every other operator POST) rejects an
// empty body, so this asserts guardrails/reset uses the optional
// decoder.
func TestGuardrailsReset_EmptyBodyMeansAll(t *testing.T) {
	t.Parallel()
	ag := &guardrailRegistrant{
		stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s1"},
		resp:           GuardrailResetResponse{Reset: []string{}},
	}
	base := serveGuardrails(t, ag)

	resp, _ := postGuardrailReset(t, base, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200 for an empty body", resp.StatusCode)
	}
	if req := ag.request(); req == nil || req.Guardrail != "" {
		t.Errorf("capability saw %+v, want the zero request", req)
	}
}

// A reset that provably re-trips is 409 with the numbers, not a 200
// that does nothing and not a 500.
func TestGuardrailsReset_RetripIs409(t *testing.T) {
	t.Parallel()
	ag := &guardrailRegistrant{
		stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s1"},
		resp: GuardrailResetResponse{
			Guardrails: GuardrailInfo{CostCeiling: CostCeilingInfo{
				MaxSessionUSD: 10, SessionCostUSD: 12, Tripped: true, WouldRetrip: true,
			}},
		},
		err: fmt.Errorf("%w: session has spent $12.0000 against a $10.0000 ceiling", ErrGuardrailRetrip),
	}
	base := serveGuardrails(t, ag)

	resp, out := postGuardrailReset(t, base, `{}`)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status %d, want 409", resp.StatusCode)
	}
	if len(out.Reset) != 0 {
		t.Errorf("Reset = %v, want empty on refusal", out.Reset)
	}
	if !strings.Contains(out.Message, "$10.0000") {
		t.Errorf("Message = %q, want the ceiling in the refusal", out.Message)
	}
	if out.Guardrails.CostCeiling.SessionCostUSD != 12 {
		t.Errorf("409 should carry the state so the client needn't re-GET: %+v", out.Guardrails)
	}
}

func TestGuardrailsReset_RejectsUnknownGuardrailAndNegativeBudget(t *testing.T) {
	t.Parallel()
	ag := &guardrailRegistrant{stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s1"}}
	base := serveGuardrails(t, ag)

	for _, body := range []string{
		`{"guardrail":"everything"}`,
		`{"additional_budget_usd":-1}`,
		`{not json`,
	} {
		resp, _ := postGuardrailReset(t, base, body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("body %s: status %d, want 400", body, resp.StatusCode)
		}
	}
	if ag.request() != nil {
		t.Error("a rejected request must not reach the capability")
	}
}

// The reset is a mutation the operator must know took effect, so an
// unwired capability is 501 — not the read side's 200-with-zeros.
func TestGuardrailsReset_WithoutCapabilityIs501(t *testing.T) {
	t.Parallel()
	base := serveGuardrails(t, &guardrailReadOnlyRegistrant{
		stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s1"},
	})
	resp, _ := postGuardrailReset(t, base, `{}`)
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("status %d, want 501", resp.StatusCode)
	}
}

// #331 asked for an audit trail. "Who un-halted a session that had
// blown its budget, and how much runway did they hand it?" is
// otherwise invisible in the transcript.
func TestNewGuardrailResetAuditEvent(t *testing.T) {
	t.Parallel()
	ev := NewGuardrailResetAuditEvent("alice", []string{GuardrailCostCeiling}, 5)
	if ev.Author != "attach/guardrail-reset" {
		t.Errorf("Author = %q", ev.Author)
	}
	if ev.CustomMetadata["caller"] != "alice" {
		t.Errorf("caller = %v, want alice", ev.CustomMetadata["caller"])
	}
	if ev.CustomMetadata["budget_added_usd"] != 5.0 {
		t.Errorf("budget_added_usd = %v, want 5", ev.CustomMetadata["budget_added_usd"])
	}
	got, _ := ev.CustomMetadata["reset"].([]string)
	if len(got) != 1 || got[0] != GuardrailCostCeiling {
		t.Errorf("reset = %v", ev.CustomMetadata["reset"])
	}
	// An anonymous caller (no auth middleware) omits the key rather
	// than recording an empty attribution.
	anon := NewGuardrailResetAuditEvent("", nil, 0)
	if _, ok := anon.CustomMetadata["caller"]; ok {
		t.Error("empty identity should be omitted, not recorded as \"\"")
	}
	if _, ok := anon.CustomMetadata["budget_added_usd"]; ok {
		t.Error("zero budget should be omitted")
	}
}

// guardrailEventfulRegistrant is a guardrailRegistrant with a real
// eventlog behind it, so the audit write has somewhere to land.
type guardrailEventfulRegistrant struct {
	guardrailRegistrant
	handle *eventlog.Handle
}

func (g *guardrailEventfulRegistrant) EventLog() *eventlog.Handle { return g.handle }

// The reset takes effect in the agent BEFORE the audit row is written,
// so a client that hangs up in between must not be able to erase the
// record of it — otherwise the one caller with a motive to hide a
// budget bump is the one who can. Fails before the WithoutCancel fix:
// the canceled request context propagates into the eventlog Get and
// the row is silently skipped.
func TestGuardrailsReset_AuditSurvivesClientDisconnect(t *testing.T) {
	t.Parallel()
	handle, cleanup := openTestEventLog(t)
	t.Cleanup(cleanup)
	appendTestEvent(t, handle, "core-agent", "u1", "s1", "hello")

	ag := &guardrailEventfulRegistrant{handle: handle}
	ag.app, ag.user, ag.sid = "core-agent", "u1", "s1"
	ag.resp = GuardrailResetResponse{
		Reset:          []string{GuardrailCostCeiling},
		BudgetAddedUSD: 5,
	}
	reg := NewSessionRegistry()
	if _, err := reg.Register(ag); err != nil {
		t.Fatal(err)
	}
	entry, err := reg.Lookup(context.Background(), "core-agent", "s1")
	if err != nil {
		t.Fatal(err)
	}

	// A request whose context is already done — what the server hands
	// the handler once the peer has gone away.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodPost, "/sessions/core-agent/u1/s1/guardrails/reset",
		strings.NewReader(`{"additional_budget_usd":5}`)).WithContext(ctx)
	rec := httptest.NewRecorder()
	(&handlers{reg: reg, pool: newBroadcasterPool()}).doGuardrailsReset(rec, req, entry)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	getResp, err := handle.Service.Get(context.Background(), &session.GetRequest{
		AppName: "core-agent", UserID: "u1", SessionID: "s1",
	})
	if err != nil {
		t.Fatalf("session Get: %v", err)
	}
	var found bool
	for ev := range getResp.Session.Events().All() {
		if ev.Author == "attach/guardrail-reset" {
			found = true
			if ev.CustomMetadata["budget_added_usd"] != 5.0 {
				t.Errorf("budget_added_usd = %v, want 5", ev.CustomMetadata["budget_added_usd"])
			}
		}
	}
	if !found {
		t.Error("no attach/guardrail-reset audit row after a disconnected reset")
	}
}

func TestBuildFeatures_GuardrailProviderFlipsCostCeiling(t *testing.T) {
	t.Parallel()
	// cost_ceiling was hardcoded false before #666. It must now
	// reflect whether a bound is actually armed.
	armed := &guardrailRegistrant{
		stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s1"},
		info:           GuardrailInfo{CostCeiling: CostCeilingInfo{MaxSessionUSD: 10}},
	}
	got := buildFeatures(&Entry{AppName: "core-agent", SessionID: "s1", Agent: armed}, nil)
	if !got[featureGuardrails] {
		t.Error("guardrails feature = false for a GuardrailProvider")
	}
	if !got[featureCostCeiling] {
		t.Error("cost_ceiling = false with a $10 session ceiling armed")
	}

	unarmed := &guardrailRegistrant{
		stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s1"},
	}
	got = buildFeatures(&Entry{AppName: "core-agent", SessionID: "s1", Agent: unarmed}, nil)
	if got[featureCostCeiling] {
		t.Error("cost_ceiling = true with no ceiling configured")
	}
	if !got[featureGuardrails] {
		t.Error("guardrails feature = false for a GuardrailProvider")
	}
}

func TestRenderGuardrails_TrippedNamesTheRecovery(t *testing.T) {
	t.Parallel()
	out := RenderGuardrails(GuardrailInfo{
		Watchdog: WatchdogInfo{Mode: "enforce"},
		CostCeiling: CostCeilingInfo{
			MaxSessionUSD: 10, SessionCostUSD: 12, Tripped: true, WouldRetrip: true,
		},
		Halted: true,
	})
	if !strings.Contains(out, "HALTED") || !strings.Contains(out, GuardrailCostCeiling) {
		t.Errorf("render missing the halt banner:\n%s", out)
	}
	if !strings.Contains(out, "/guardrail reset +") {
		t.Errorf("a would-retrip render must ask for budget:\n%s", out)
	}

	healthy := RenderGuardrails(GuardrailInfo{Watchdog: WatchdogInfo{Mode: "warn"}})
	if strings.Contains(healthy, "Reset with") {
		t.Errorf("healthy render shouldn't advertise a reset:\n%s", healthy)
	}
}
