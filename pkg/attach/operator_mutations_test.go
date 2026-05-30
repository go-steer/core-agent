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
	"errors"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"
)

// operatorMutationsRegistrant satisfies every PR A2 mutation
// capability (PermsProvider, PermsController, PricingController,
// Reloader) on top of stubRegistrant. Mutations record into the
// struct's fields so tests can assert against them.
type operatorMutationsRegistrant struct {
	stubRegistrant
	mu sync.Mutex

	perms PermsInfo

	// PermsController call recorders.
	addedAllow [][]string
	addedDeny  [][]string

	// PricingController behaviors.
	refreshResp PricingRefreshResponse
	refreshErr  error
	setReq      *PricingSetRequest
	setErr      error

	// Reloader behavior.
	reloadResp ReloadResponse
}

func (m *operatorMutationsRegistrant) AttachPerms() PermsInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.perms
}

func (m *operatorMutationsRegistrant) AttachAddAllow(patterns []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.addedAllow = append(m.addedAllow, append([]string(nil), patterns...))
	return nil
}

func (m *operatorMutationsRegistrant) AttachAddDeny(patterns []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.addedDeny = append(m.addedDeny, append([]string(nil), patterns...))
	return nil
}

func (m *operatorMutationsRegistrant) AttachRefreshPricing(ctx context.Context) (PricingRefreshResponse, error) {
	return m.refreshResp, m.refreshErr
}

func (m *operatorMutationsRegistrant) AttachSetManualPricing(req PricingSetRequest) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := req
	m.setReq = &cp
	return m.setErr
}

func (m *operatorMutationsRegistrant) AttachReload(ctx context.Context) ReloadResponse {
	return m.reloadResp
}

func TestIntegration_PermsEndpoint_Read(t *testing.T) {
	t.Parallel()
	reg := NewSessionRegistry()
	ag := &operatorMutationsRegistrant{
		stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s1"},
		perms: PermsInfo{
			Mode:  "ask",
			Allow: []string{"tool.read_file", "tool.grep"},
			Deny:  []string{"tool.bash:rm *"},
		},
	}
	if _, err := reg.Register(ag); err != nil {
		t.Fatal(err)
	}
	base, cleanup := startTestServer(t, reg)
	defer cleanup()

	resp, err := http.Get(base + "/sessions/core-agent/s1/perms")
	if err != nil {
		t.Fatalf("GET /perms: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var got PermsInfo
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Mode != "ask" || len(got.Allow) != 2 || len(got.Deny) != 1 {
		t.Errorf("got = %+v", got)
	}
}

func TestIntegration_PermsAllowEndpoint(t *testing.T) {
	t.Parallel()
	reg := NewSessionRegistry()
	ag := &operatorMutationsRegistrant{
		stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s1"},
	}
	if _, err := reg.Register(ag); err != nil {
		t.Fatal(err)
	}
	base, cleanup := startTestServer(t, reg)
	defer cleanup()

	body, _ := json.Marshal(PatternsRequest{Patterns: []string{"tool.read_file", "tool.glob"}})
	resp, err := http.Post(base+"/sessions/core-agent/s1/perms/allow", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /perms/allow: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status %d, want 204", resp.StatusCode)
	}
	if len(ag.addedAllow) != 1 || len(ag.addedAllow[0]) != 2 || ag.addedAllow[0][0] != "tool.read_file" {
		t.Errorf("addedAllow = %v", ag.addedAllow)
	}
}

func TestIntegration_PermsDenyEndpoint(t *testing.T) {
	t.Parallel()
	reg := NewSessionRegistry()
	ag := &operatorMutationsRegistrant{
		stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s1"},
	}
	if _, err := reg.Register(ag); err != nil {
		t.Fatal(err)
	}
	base, cleanup := startTestServer(t, reg)
	defer cleanup()

	body, _ := json.Marshal(PatternsRequest{Patterns: []string{"tool.bash:rm *"}})
	resp, err := http.Post(base+"/sessions/core-agent/s1/perms/deny", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /perms/deny: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if len(ag.addedDeny) != 1 || ag.addedDeny[0][0] != "tool.bash:rm *" {
		t.Errorf("addedDeny = %v", ag.addedDeny)
	}
}

func TestIntegration_PermsAllow_EmptyPatterns_400(t *testing.T) {
	t.Parallel()
	reg := NewSessionRegistry()
	ag := &operatorMutationsRegistrant{
		stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s1"},
	}
	if _, err := reg.Register(ag); err != nil {
		t.Fatal(err)
	}
	base, cleanup := startTestServer(t, reg)
	defer cleanup()

	body, _ := json.Marshal(PatternsRequest{Patterns: nil})
	resp, err := http.Post(base+"/sessions/core-agent/s1/perms/allow", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status %d, want 400", resp.StatusCode)
	}
}

func TestIntegration_PermsAllow_NoController_501(t *testing.T) {
	t.Parallel()
	// stubRegistrant does NOT implement PermsController — POST 501.
	reg := NewSessionRegistry()
	ag := &stubRegistrant{app: "core-agent", user: "u", sid: "s1"}
	if _, err := reg.Register(ag); err != nil {
		t.Fatal(err)
	}
	base, cleanup := startTestServer(t, reg)
	defer cleanup()

	body, _ := json.Marshal(PatternsRequest{Patterns: []string{"x"}})
	resp, err := http.Post(base+"/sessions/core-agent/s1/perms/allow", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("status %d, want 501", resp.StatusCode)
	}
}

func TestIntegration_PricingRefreshEndpoint(t *testing.T) {
	t.Parallel()
	reg := NewSessionRegistry()
	now := time.Date(2026, 5, 29, 21, 0, 0, 0, time.UTC)
	ag := &operatorMutationsRegistrant{
		stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s1"},
		refreshResp: PricingRefreshResponse{
			Updated:     true,
			KnownModels: 847,
			LastRefresh: now,
		},
	}
	if _, err := reg.Register(ag); err != nil {
		t.Fatal(err)
	}
	base, cleanup := startTestServer(t, reg)
	defer cleanup()

	resp, err := http.Post(base+"/sessions/core-agent/s1/pricing/refresh", "application/json", bytes.NewReader([]byte("{}")))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	var got PricingRefreshResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.Updated || got.KnownModels != 847 {
		t.Errorf("got = %+v", got)
	}
}

func TestIntegration_PricingSetEndpoint(t *testing.T) {
	t.Parallel()
	reg := NewSessionRegistry()
	ag := &operatorMutationsRegistrant{
		stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s1"},
	}
	if _, err := reg.Register(ag); err != nil {
		t.Fatal(err)
	}
	base, cleanup := startTestServer(t, reg)
	defer cleanup()

	body, _ := json.Marshal(PricingSetRequest{Model: "claude-opus-4-7", InputUSDPerMTok: 15, OutputUSDPerMTok: 75})
	resp, err := http.Post(base+"/sessions/core-agent/s1/pricing/set", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("status %d, want 204", resp.StatusCode)
	}
	if ag.setReq == nil || ag.setReq.Model != "claude-opus-4-7" || ag.setReq.InputUSDPerMTok != 15 {
		t.Errorf("setReq = %+v", ag.setReq)
	}
}

func TestIntegration_PricingSet_NegativeRate_400(t *testing.T) {
	t.Parallel()
	reg := NewSessionRegistry()
	ag := &operatorMutationsRegistrant{
		stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s1"},
	}
	if _, err := reg.Register(ag); err != nil {
		t.Fatal(err)
	}
	base, cleanup := startTestServer(t, reg)
	defer cleanup()

	body, _ := json.Marshal(PricingSetRequest{Model: "x", InputUSDPerMTok: -1, OutputUSDPerMTok: 1})
	resp, err := http.Post(base+"/sessions/core-agent/s1/pricing/set", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status %d, want 400", resp.StatusCode)
	}
}

func TestIntegration_ReloadEndpoint(t *testing.T) {
	t.Parallel()
	reg := NewSessionRegistry()
	ag := &operatorMutationsRegistrant{
		stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s1"},
		reloadResp:     ReloadResponse{Memory: true, Skills: true, MCP: false, Errors: []string{"mcp: server 'kube' failed to restart"}},
	}
	if _, err := reg.Register(ag); err != nil {
		t.Fatal(err)
	}
	base, cleanup := startTestServer(t, reg)
	defer cleanup()

	resp, err := http.Post(base+"/sessions/core-agent/s1/reload", "application/json", bytes.NewReader([]byte("{}")))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var got ReloadResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.Memory || !got.Skills || got.MCP || len(got.Errors) != 1 {
		t.Errorf("got = %+v", got)
	}
}

func TestIntegration_Reload_NoCapability_501(t *testing.T) {
	t.Parallel()
	// stubRegistrant doesn't implement Reloader — POST 501.
	reg := NewSessionRegistry()
	ag := &stubRegistrant{app: "core-agent", user: "u", sid: "s1"}
	if _, err := reg.Register(ag); err != nil {
		t.Fatal(err)
	}
	base, cleanup := startTestServer(t, reg)
	defer cleanup()

	resp, err := http.Post(base+"/sessions/core-agent/s1/reload", "application/json", bytes.NewReader([]byte("{}")))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("status %d, want 501", resp.StatusCode)
	}
}

func TestOperatorView_NilMutationFuncs_501(t *testing.T) {
	t.Parallel()
	// OperatorView with nil RefreshPricing / SetPricing / Reload
	// satisfies the controller interfaces (via methods) but each
	// returns ErrCapabilityNotRegistered, which the handler maps
	// to 501.
	reg := NewSessionRegistry()
	view := &OperatorView{
		Registrant: &stubRegistrant{app: "core-agent", user: "u", sid: "s1"},
	}
	if _, err := reg.Register(view); err != nil {
		t.Fatal(err)
	}
	base, cleanup := startTestServer(t, reg)
	defer cleanup()

	cases := []struct {
		path string
		body string
	}{
		{"/pricing/refresh", "{}"},
		{"/pricing/set", `{"model":"x","input_usd_per_mtok":1,"output_usd_per_mtok":1}`},
		{"/reload", "{}"},
	}
	for _, c := range cases {
		resp, err := http.Post(base+"/sessions/core-agent/s1"+c.path, "application/json", bytes.NewReader([]byte(c.body)))
		if err != nil {
			t.Fatalf("POST %s: %v", c.path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotImplemented {
			t.Errorf("%s status %d, want 501", c.path, resp.StatusCode)
		}
	}
}

func TestOperatorView_PopulatedMutationFuncs(t *testing.T) {
	t.Parallel()
	reg := NewSessionRegistry()
	var refreshCalled, reloadCalled bool
	var setReq PricingSetRequest
	view := &OperatorView{
		Registrant: &stubRegistrant{app: "core-agent", user: "u", sid: "s1"},
		RefreshPricing: func(ctx context.Context) (PricingRefreshResponse, error) {
			refreshCalled = true
			return PricingRefreshResponse{Updated: true, KnownModels: 100}, nil
		},
		SetPricing: func(req PricingSetRequest) error {
			setReq = req
			return nil
		},
		Reload: func(ctx context.Context) ReloadResponse {
			reloadCalled = true
			return ReloadResponse{Memory: true, Skills: true, MCP: true}
		},
	}
	if _, err := reg.Register(view); err != nil {
		t.Fatal(err)
	}
	base, cleanup := startTestServer(t, reg)
	defer cleanup()

	if r, err := http.Post(base+"/sessions/core-agent/s1/pricing/refresh", "application/json", bytes.NewReader([]byte("{}"))); err != nil {
		t.Fatal(err)
	} else {
		_ = r.Body.Close()
	}
	if !refreshCalled {
		t.Error("refresh not called")
	}

	body, _ := json.Marshal(PricingSetRequest{Model: "x", InputUSDPerMTok: 1, OutputUSDPerMTok: 2})
	if r, err := http.Post(base+"/sessions/core-agent/s1/pricing/set", "application/json", bytes.NewReader(body)); err != nil {
		t.Fatal(err)
	} else {
		_ = r.Body.Close()
	}
	if setReq.Model != "x" {
		t.Errorf("setReq.Model = %q", setReq.Model)
	}

	if r, err := http.Post(base+"/sessions/core-agent/s1/reload", "application/json", bytes.NewReader([]byte("{}"))); err != nil {
		t.Fatal(err)
	} else {
		_ = r.Body.Close()
	}
	if !reloadCalled {
		t.Error("reload not called")
	}
}

func TestOperatorView_ErrCapabilityNotRegistered_IsSentinel(t *testing.T) {
	t.Parallel()
	view := &OperatorView{
		Registrant: &stubRegistrant{app: "core-agent", user: "u", sid: "s1"},
	}
	if _, err := view.AttachRefreshPricing(context.Background()); !errors.Is(err, ErrCapabilityNotRegistered) {
		t.Errorf("AttachRefreshPricing: err = %v, want ErrCapabilityNotRegistered", err)
	}
	if err := view.AttachSetManualPricing(PricingSetRequest{}); !errors.Is(err, ErrCapabilityNotRegistered) {
		t.Errorf("AttachSetManualPricing: err = %v, want ErrCapabilityNotRegistered", err)
	}
	resp := view.AttachReload(context.Background())
	if len(resp.Errors) != 1 || resp.Errors[0] != ErrCapabilityNotRegistered.Error() {
		t.Errorf("AttachReload: resp = %+v, want sentinel in Errors", resp)
	}
}
