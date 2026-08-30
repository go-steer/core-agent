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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/attach"
)

// The attribution the daemon records has to survive the projection into
// core-tui's row shape. It did not before v0.24.0 — the gate attributed
// approvals (#830), ApprovalInfo.By carried the name over the wire, and
// this loop dropped it at the literal because coretui.ApprovalLog had
// nowhere to put it (core-tui #277). That is the whole reason the field
// exists: several operators attached to one daemon, where "allowed
// once" without a name answers half the question /permissions is asked.
func TestAdapter_SessionApprovals_CarriesApprover(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /sessions/{sid}/perms", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(attach.PermsInfo{
			Mode: "ask",
			Approvals: []attach.ApprovalInfo{
				{Tool: "bash", Key: "kubectl rollout restart deploy/api", Decision: "allow-once", By: "ops@example.com"},
				// The daemon omits By when it verified nobody. An
				// empty By must stay empty all the way through:
				// core-tui renders it as no suffix, and a placeholder
				// in an audit line reads like a name somebody checked.
				{Tool: "read_file", Key: "/etc/hosts", Decision: "allow-session"},
			},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	rows := newPauseAdapter(t, srv).SessionApprovals()
	if len(rows) != 2 {
		t.Fatalf("got %d approval rows, want 2", len(rows))
	}
	if rows[0].By != "ops@example.com" {
		t.Errorf("rows[0].By = %q, want %q — the approver was dropped in projection", rows[0].By, "ops@example.com")
	}
	if rows[1].By != "" {
		t.Errorf("rows[1].By = %q, want empty for an unattributed approval", rows[1].By)
	}
}
