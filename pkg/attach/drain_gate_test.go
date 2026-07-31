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
	"net/http"
	"testing"
)

// TestDrainGate_InjectAndWakeReject503AfterDaemonCtxCancel covers
// #564: in the window between SIGTERM's ctx-cancel (wake loops dead)
// and Server.Close (listener gone), /inject and /wake must refuse
// intake with 503 + Retry-After instead of acknowledging messages
// that die with the in-memory inbox.
func TestDrainGate_InjectAndWakeReject503AfterDaemonCtxCancel(t *testing.T) {
	t.Parallel()
	reg := NewSessionRegistry()
	ag := &stubRegistrant{app: "core-agent", user: "u", sid: "default"}
	if _, err := reg.Register(ag); err != nil {
		t.Fatalf("Register: %v", err)
	}
	daemonCtx, cancel := context.WithCancel(context.Background())
	base, done := startTestServerOpts(t, reg, Options{DaemonCtx: daemonCtx})
	defer done()

	// Live daemon: intake works.
	resp := doReq(t, http.MethodPost, base+"/sessions/default/inject",
		"application/json", "", `{"message":"before shutdown"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("inject before cancel = %d, want 200", resp.StatusCode)
	}

	// SIGTERM window: ctx cancelled, listener still up.
	cancel()
	resp = doReq(t, http.MethodPost, base+"/sessions/default/inject",
		"application/json", "", `{"message":"lost forever"}`)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("inject after cancel = %d, want 503", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Error("503 response missing Retry-After header")
	}
	resp = doReq(t, http.MethodPost, base+"/sessions/default/wake",
		"application/json", "", `{"prompt":"also lost"}`)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("wake after cancel = %d, want 503", resp.StatusCode)
	}
}

// TestDrainGate_NilDaemonCtxNeverGates pins the embedder/test default:
// servers constructed without Options.DaemonCtx keep pre-#564
// behavior.
func TestDrainGate_NilDaemonCtxNeverGates(t *testing.T) {
	t.Parallel()
	reg := NewSessionRegistry()
	ag := &stubRegistrant{app: "core-agent", user: "u", sid: "default"}
	if _, err := reg.Register(ag); err != nil {
		t.Fatalf("Register: %v", err)
	}
	base, done := startTestServer(t, reg)
	defer done()
	resp := doReq(t, http.MethodPost, base+"/sessions/default/inject",
		"application/json", "", `{"message":"m"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("inject with nil DaemonCtx = %d, want 200", resp.StatusCode)
	}
}
