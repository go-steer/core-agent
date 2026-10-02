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

package attachadapter

import (
	"context"
	"testing"
	"time"

	"github.com/go-steer/core-agent/v2/internal/testutil"
	"github.com/go-steer/core-agent/v2/pkg/agent"
	"github.com/go-steer/core-agent/v2/pkg/permissions"
)

type allowingApprover struct{}

func (allowingApprover) Judge(context.Context, permissions.ApproverRequest) (permissions.Verdict, error) {
	return permissions.Verdict{Outcome: permissions.VerdictAllow, Reason: "routine", Model: "judge-1"}, nil
}

// A call ModeAuto's approver allowed shows on GET /perms with the model
// that allowed it, under approver_model, and with no human in By
// (#1175 decision 10, protocol 1.18.0).
func TestAttachPerms_ApprovalRowNamesTheApproverModel(t *testing.T) {
	t.Parallel()
	pol, err := permissions.NewPolicy([]string{"bash:go *"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	g := permissions.New(permissions.Options{
		Mode: permissions.ModeAuto, Approver: allowingApprover{}, AutoEligible: pol, ApprovalTimeout: time.Minute,
	})
	if err := g.CheckBashWithArgs(testutil.ApproverContext(context.Background()), "go test ./...", map[string]string{"command": "go test ./..."}); err != nil {
		t.Fatalf("the approver's allow did not run the call: %v", err)
	}
	info := New(newEchoAgent(t, agent.WithGate(g))).AttachPerms()
	if len(info.Approvals) != 1 || info.Approvals[0].ApproverModel != "judge-1" || info.Approvals[0].By != "" {
		t.Errorf("approvals = %+v, want one row naming judge-1 and no human", info.Approvals)
	}
}
