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
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	coretui "github.com/go-steer/core-tui/tui"

	"github.com/go-steer/core-agent/v2/pkg/attach"
)

// capturingPrompter records the request it was asked about.
type capturingPrompter struct{ req coretui.PermissionRequest }

func (p *capturingPrompter) AskApproval(_ context.Context, req coretui.PermissionRequest) (coretui.PermissionDecision, error) {
	p.req = req
	return coretui.DecisionDeny, nil
}

func (p *capturingPrompter) AskApprovalDetailed(_ context.Context, req coretui.PermissionRequest) (coretui.PermissionOutcome, error) {
	p.req = req
	return coretui.PermissionOutcome{Decision: coretui.DecisionDeny}, nil
}

// A frame ModeAuto's approver passed on (#1175, protocol 1.18.0)
// reaches core-tui as an escalated request, so the attached prompt
// offers only once and deny and quotes the reason. A frame without the
// field stays an ordinary prompt.
func TestRemotePrompt_EscalatedFrameBecomesAnEscalation(t *testing.T) {
	t.Parallel()
	rec := &respondRecorder{}
	client := rec.serve(t)
	host := &fakeBridgeHost{version: "1.18.0"}

	frame := testPromptFrame
	frame.ApproverModel, frame.ApproverReason = "judge-1", "the task did not ask for this"
	p := &capturingPrompter{}
	handleRemotePromptFrame(context.Background(), client, "/sessions/s1", p, frame, io.Discard, host)
	if e := p.req.Escalation; e == nil || e.Approver != "judge-1" || e.Reason != "the task did not ask for this" {
		t.Errorf("escalation = %+v, want the approver and its reason", e)
	}

	plain := &capturingPrompter{}
	handleRemotePromptFrame(context.Background(), client, "/sessions/s1", plain, testPromptFrame, io.Discard, host)
	if plain.req.Escalation != nil {
		t.Errorf("an ordinary frame became an escalation: %+v", plain.req.Escalation)
	}
}

// The approver model on an approval row survives the projection into
// core-tui's row (#1175 decision 10).
func TestAdapter_SessionApprovals_CarriesApproverModel(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /sessions/{sid}/perms", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(attach.PermsInfo{
			Mode:      "auto",
			Approvals: []attach.ApprovalInfo{{Tool: "bash", Key: "go test ./...", Decision: "allow-once", ApproverModel: "judge-1"}},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	rows := newPauseAdapter(t, srv).SessionApprovals()
	if len(rows) != 1 || rows[0].Approver != "judge-1" || rows[0].By != "" {
		t.Errorf("rows = %+v, want the approver model and no human", rows)
	}
}
