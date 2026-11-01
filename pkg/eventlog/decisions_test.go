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

package eventlog

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool/toolconfirmation"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v3/internal/effects"
	"github.com/go-steer/core-agent/v3/pkg/approval"
)

var testDecidedAt = time.Date(2026, 8, 16, 9, 0, 0, 0, time.UTC)

func sampleApprovedDecision(sid, callID, toolName, approver string, at time.Time) approval.Decision {
	return approval.Decision{
		DecidedAt:      at,
		Session:        sid,
		Invocation:     "inv-1",
		FunctionCallID: callID,
		Workload:       "gke-triage",
		Specialist:     "remediator",
		Tool:           toolName,
		Outcome:        approval.OutcomeApprove,
		Scope:          approval.ScopeOnce,
		Authority:      approval.AuthorityVerdict,
		Disposition:    approval.DispositionAuthorized,
		ProposedKey:    toolName + "(deployment=api)",
		ProposedArgs:   map[string]any{"deployment": "api"},
		Approver:       approver,
	}
}

func makeDecisionEvent(callID string, d approval.Decision) *session.Event {
	raw, err := approval.EncodeDecision(d)
	if err != nil {
		panic(err)
	}
	ev := session.NewEvent(context.Background(), "inv-1")
	ev.Author = "sre"
	ev.Content = genai.NewContentFromText("decided", genai.RoleModel)
	ev.Actions.StateDelta = map[string]any{approval.DecisionStateKey(callID): raw}
	return ev
}

func parseExportedJSONL(t *testing.T, buf *bytes.Buffer) (ExportMeta, []approval.Decision) {
	t.Helper()
	sc := bufio.NewScanner(bytes.NewReader(buf.Bytes()))
	if !sc.Scan() {
		t.Fatal("export is empty; want provenance header")
	}
	var header metaLine
	if err := json.Unmarshal(sc.Bytes(), &header); err != nil {
		t.Fatalf("unmarshal header: %v", err)
	}
	var rows []approval.Decision
	for sc.Scan() {
		line := sc.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			t.Error("unexpected blank line in JSONL export")
			continue
		}
		var d approval.Decision
		if err := json.Unmarshal(line, &d); err != nil {
			t.Fatalf("unmarshal row: %v", err)
		}
		rows = append(rows, d)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scanner error: %v", err)
	}
	return header.Meta, rows
}

func TestDecisionStore_RecordListAndDelete(t *testing.T) {
	h, _ := openTestHandle(t)
	ctx := context.Background()

	_, err := h.Service.Create(ctx, &session.CreateRequest{
		AppName: "app", UserID: "u1", SessionID: "s-1",
	})
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}

	d1 := sampleApprovedDecision("s-1", "fc-1", "scale_deployment", "user:alice", testDecidedAt)
	d2 := sampleApprovedDecision("s-1", "fc-2", "rollout_restart", "user:alice", testDecidedAt.Add(time.Minute))
	d2.Outcome = approval.OutcomeReject
	d2.Disposition = approval.DispositionRefusedByOperator

	if err := h.RecordDecision(ctx, d1); err != nil {
		t.Fatalf("RecordDecision d1: %v", err)
	}
	if err := h.RecordDecision(ctx, d2); err != nil {
		t.Fatalf("RecordDecision d2: %v", err)
	}
	// Upsert idempotency on same (session_id, function_call_id).
	d1Updated := d1
	d1Updated.Note = "updated note"
	if err := h.RecordDecision(ctx, d1Updated); err != nil {
		t.Fatalf("RecordDecision d1Updated: %v", err)
	}

	got, err := h.Decisions.ListDecisions(ctx, "s-1")
	if err != nil {
		t.Fatalf("ListDecisions: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListDecisions len = %d, want 2", len(got))
	}
	if got[0].Note != "updated note" {
		t.Errorf("got[0].Note = %q, want updated note", got[0].Note)
	}
	if got[1].Outcome != approval.OutcomeReject {
		t.Errorf("got[1].Outcome = %q, want %q", got[1].Outcome, approval.OutcomeReject)
	}
	if has, err := h.HasDecision(ctx, "s-1", "fc-1"); err != nil || !has {
		t.Errorf("HasDecision(s-1, fc-1) = (%v, %v), want (true, nil)", has, err)
	}
	if has, err := h.HasDecision(ctx, "s-1", "fc-missing"); err != nil || has {
		t.Errorf("HasDecision(s-1, fc-missing) = (%v, %v), want (false, nil)", has, err)
	}

	// If another user owns a session with the same ID, deleting u2's session
	// must not wipe u1's decision rows.
	if _, err := h.Service.Create(ctx, &session.CreateRequest{
		AppName: "app", UserID: "u2", SessionID: "s-1",
	}); err != nil {
		t.Fatalf("Create u2 session: %v", err)
	}
	if err := h.Service.Delete(ctx, &session.DeleteRequest{
		AppName: "app", UserID: "u2", SessionID: "s-1",
	}); err != nil {
		t.Fatalf("Delete u2 session: %v", err)
	}
	stillThere, err := h.Decisions.ListDecisions(ctx, "s-1")
	if err != nil || len(stillThere) != 2 {
		t.Fatalf("ListDecisions after deleting u2's session = (%d, %v), want 2", len(stillThere), err)
	}

	// Deleting the last session with ID "s-1" deletes its decision rows.
	if err := h.Service.Delete(ctx, &session.DeleteRequest{
		AppName: "app", UserID: "u1", SessionID: "s-1",
	}); err != nil {
		t.Fatalf("Delete session: %v", err)
	}
	afterDelete, err := h.Decisions.ListDecisions(ctx, "s-1")
	if err != nil {
		t.Fatalf("ListDecisions after Delete: %v", err)
	}
	if len(afterDelete) != 0 {
		t.Errorf("ListDecisions after Delete = %+v, want empty", afterDelete)
	}
}

func TestSessionDecisions_ChronologicalMergeOfSQLAndEventDecisions(t *testing.T) {
	h, _ := openTestHandle(t)
	ctx := context.Background()

	s1, err := h.Service.Create(ctx, &session.CreateRequest{
		AppName: "app", UserID: "u1", SessionID: "s-merge",
	})
	if err != nil {
		t.Fatalf("Create s-merge: %v", err)
	}

	// 1. An earlier crashed call (t1) persisted only in SQL via DecisionSink.
	t1 := testDecidedAt
	dSQLOnly := sampleApprovedDecision("s-merge", "fc-sql-early", "scale_deployment", "user:alice", t1)
	if err := h.RecordDecision(ctx, dSQLOnly); err != nil {
		t.Fatalf("RecordDecision dSQLOnly: %v", err)
	}

	// 2. A later completed call (t2 > t1) persisted in session event StateDelta.
	t2 := testDecidedAt.Add(5 * time.Minute)
	dEventLater := sampleApprovedDecision("s-merge", "fc-event-later", "rollout_restart", "user:bob", t2)
	if err := h.Service.AppendEvent(ctx, s1.Session, makeDecisionEvent("fc-event-later", dEventLater)); err != nil {
		t.Fatalf("AppendEvent dEventLater: %v", err)
	}

	merged, err := h.SessionDecisions(ctx, "app", "u1", "s-merge")
	if err != nil {
		t.Fatalf("SessionDecisions: %v", err)
	}
	if len(merged) != 2 {
		t.Fatalf("merged len = %d, want 2", len(merged))
	}
	if merged[0].FunctionCallID != "fc-sql-early" || merged[1].FunctionCallID != "fc-event-later" {
		t.Errorf("merged order = [%s, %s], want chronological [fc-sql-early, fc-event-later]",
			merged[0].FunctionCallID, merged[1].FunctionCallID)
	}
}

func TestExportDecisions_RedactionAndFilters(t *testing.T) {
	h, _ := openTestHandle(t)
	ctx := context.Background()

	s1, err := h.Service.Create(ctx, &session.CreateRequest{
		AppName: "app", UserID: "u1", SessionID: "s-1",
	})
	if err != nil {
		t.Fatalf("Create s-1: %v", err)
	}
	s2, err := h.Service.Create(ctx, &session.CreateRequest{
		AppName: "app", UserID: "u1", SessionID: "s-2",
	})
	if err != nil {
		t.Fatalf("Create s-2: %v", err)
	}
	_, err = h.Service.Create(ctx, &session.CreateRequest{
		AppName: "app", UserID: "u1", SessionID: "s-1:mast-ops",
	})
	if err != nil {
		t.Fatalf("Create ops row: %v", err)
	}

	d1 := sampleApprovedDecision("s-1", "fc-1", "scale_deployment", "user:sre-oncall", testDecidedAt)
	d2 := sampleApprovedDecision("s-1", "fc-2", "rollout_restart", "core-agent:internal", testDecidedAt.Add(time.Minute))
	d3 := sampleApprovedDecision("s-2", "fc-3", "scale_deployment", "user:other", testDecidedAt.Add(2*time.Hour))
	d3.Workload = "other-fleet"

	if err := h.Service.AppendEvent(ctx, s1.Session, makeDecisionEvent("fc-1", d1)); err != nil {
		t.Fatalf("AppendEvent fc-1: %v", err)
	}
	// Record fc-2 only in SQL DecisionStore (simulating #561 pre-tool persistence)
	// and verify SessionDecisions / ExportDecisions includes it without duplication.
	if err := h.RecordDecision(ctx, d1); err != nil {
		t.Fatalf("RecordDecision fc-1: %v", err)
	}
	if err := h.RecordDecision(ctx, d2); err != nil {
		t.Fatalf("RecordDecision fc-2: %v", err)
	}
	if err := h.Service.AppendEvent(ctx, s2.Session, makeDecisionEvent("fc-3", d3)); err != nil {
		t.Fatalf("AppendEvent fc-3: %v", err)
	}

	var buf bytes.Buffer
	n, err := h.ExportDecisions(ctx, &buf, ExportOptions{
		AppName: "app",
		Source:  "/tmp/sessions.db",
	})
	if err != nil {
		t.Fatalf("ExportDecisions: %v", err)
	}
	meta, rows := parseExportedJSONL(t, &buf)
	if n != 3 || len(rows) != 3 || meta.Records != 3 {
		t.Fatalf("ExportDecisions n=%d rows=%d meta.Records=%d, want 3", n, len(rows), meta.Records)
	}
	if meta.Redaction != RedactionApproverDigest {
		t.Errorf("meta.Redaction = %q, want %q", meta.Redaction, RedactionApproverDigest)
	}
	if rows[0].Approver != approval.RedactApprover("user:sre-oncall") {
		t.Errorf("rows[0].Approver = %q, want redacted digest", rows[0].Approver)
	}
	if rows[1].Approver != "core-agent:internal" {
		t.Errorf("rows[1].Approver = %q, want core-agent:internal in the clear", rows[1].Approver)
	}

	// Filter by Workload and IncludeApprover.
	buf.Reset()
	_, err = h.ExportDecisions(ctx, &buf, ExportOptions{
		AppName:         "app",
		Workload:        "other-fleet",
		IncludeApprover: true,
	})
	if err != nil {
		t.Fatalf("ExportDecisions filtered: %v", err)
	}
	meta, rows = parseExportedJSONL(t, &buf)
	if meta.Redaction != RedactionNone || len(rows) != 1 || rows[0].Approver != "user:other" {
		t.Errorf("filtered export meta=%+v rows=%+v, want 1 unredacted other-fleet row", meta, rows)
	}
}

func TestAckEffects_WatermarkClearsDanglingEffects(t *testing.T) {
	h, _ := openTestHandle(t)
	ctx := context.Background()
	pred := effects.NewPredicate(nil)

	created, err := h.Service.Create(ctx, &session.CreateRequest{
		AppName: "app", UserID: "u1", SessionID: "s-effects",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, ok := h.EffectsAckedAt(ctx, "app", "u1", "s-effects"); ok {
		t.Fatal("EffectsAckedAt on fresh session = true, want false")
	}

	// Append a dangling mutating tool call.
	ev := session.NewEvent(ctx, "inv-1")
	ev.Author = "agent"
	ev.Content = &genai.Content{
		Role: genai.RoleModel,
		Parts: []*genai.Part{{
			FunctionCall: &genai.FunctionCall{
				ID:   "fc-dangling-1",
				Name: "write_file",
				Args: map[string]any{"path": "foo.txt"},
			},
		}},
	}
	if err := h.Service.AppendEvent(ctx, created.Session, ev); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}

	reloaded, err := h.Service.Get(ctx, &session.GetRequest{
		AppName: "app", UserID: "u1", SessionID: "s-effects",
	})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	dangling := effects.ScanDangling(reloaded.Session.Events(), pred, nil).Unacked(time.Time{})
	if len(dangling) != 1 {
		t.Fatalf("ScanDangling before ack = %+v, want 1 dangling call", dangling)
	}

	ackAt, err := h.AckEffects(ctx, "app", "", "s-effects", "user:alice", "verified manually")
	if err != nil {
		t.Fatalf("AckEffects: %v", err)
	}
	gotAck, ok := h.EffectsAckedAt(ctx, "app", "", "s-effects")
	if !ok || !gotAck.Equal(ackAt) {
		t.Fatalf("EffectsAckedAt = (%v, %v), want (%v, true)", gotAck, ok, ackAt)
	}

	reloadedAfter, err := h.Service.Get(ctx, &session.GetRequest{
		AppName: "app", UserID: "u1", SessionID: "s-effects",
	})
	if err != nil {
		t.Fatalf("Get after ack: %v", err)
	}
	danglingAfter := effects.ScanDangling(reloadedAfter.Session.Events(), pred, nil).Unacked(gotAck)
	if len(danglingAfter) != 0 {
		t.Errorf("ScanDangling after ack = %+v, want 0", danglingAfter)
	}
}

func TestListParks_ProjectsApprovalAndInputParks(t *testing.T) {
	h, _ := openTestHandle(t)
	ctx := context.Background()

	s1, err := h.Service.Create(ctx, &session.CreateRequest{
		AppName: "app", UserID: "u1", SessionID: "s-parked",
	})
	if err != nil {
		t.Fatalf("Create s-parked: %v", err)
	}

	parkEv := session.NewEvent(ctx, "inv-1")
	parkEv.Author = "agent"
	parkEv.LongRunningToolIDs = []string{"adk-conf-1"}
	parkEv.Content = &genai.Content{
		Role: genai.RoleModel,
		Parts: []*genai.Part{{
			FunctionCall: &genai.FunctionCall{
				ID:   "adk-conf-1",
				Name: toolconfirmation.FunctionCallName,
				Args: map[string]any{
					"originalFunctionCall": map[string]any{
						"id":   "call-orig-1",
						"name": "scale_deployment",
						"args": map[string]any{"deployment": "checkout", "replicas": float64(4)},
					},
					"toolConfirmation": map[string]any{
						"hint": "approve scale_deployment",
						"payload": map[string]any{
							"tool":    "scale_deployment",
							"args":    map[string]any{"deployment": "checkout", "replicas": float64(4)},
							"subject": "scale_deployment(deployment=checkout, replicas=4)",
						},
					},
				},
			},
		}},
	}
	if err := h.Service.AppendEvent(ctx, s1.Session, parkEv); err != nil {
		t.Fatalf("AppendEvent parkEv: %v", err)
	}

	parks, err := h.ListParks(ctx, "app", "", "")
	if err != nil {
		t.Fatalf("ListParks: %v", err)
	}
	if len(parks) != 1 || parks[0].SessionID != "s-parked" {
		t.Fatalf("ListParks = %+v, want 1 session s-parked", parks)
	}
	if parks[0].Awaiting != AwaitingApproval {
		t.Errorf("Awaiting = %q, want %q", parks[0].Awaiting, AwaitingApproval)
	}
	if len(parks[0].Pending) != 1 || parks[0].Pending[0].ProposedTool != "scale_deployment" {
		t.Errorf("Pending = %+v, want scale_deployment park", parks[0].Pending)
	}

	// Resolve the park and verify ListParks becomes empty.
	resolveEv := session.NewEvent(ctx, "inv-2")
	resolveEv.Author = "user"
	resolveEv.Content = &genai.Content{
		Role: genai.RoleUser,
		Parts: []*genai.Part{{
			FunctionResponse: &genai.FunctionResponse{
				ID:       "adk-conf-1",
				Name:     toolconfirmation.FunctionCallName,
				Response: map[string]any{"confirmed": true},
			},
		}},
	}
	if err := h.Service.AppendEvent(ctx, s1.Session, resolveEv); err != nil {
		t.Fatalf("AppendEvent resolveEv: %v", err)
	}

	parksAfter, err := h.ListParks(ctx, "app", "", "s-parked")
	if err != nil {
		t.Fatalf("ListParks after resolve: %v", err)
	}
	if len(parksAfter) != 0 {
		t.Errorf("ListParks after resolve = %+v, want empty", parksAfter)
	}
}
