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

// Originally derived from go-steer/mast.

package eventlog

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"google.golang.org/adk/v2/session"
)

const (
	// ackOpsSessionID is the reserved ops session ID used to persist durable
	// operator acknowledgements in ADK session state without touching
	// agent_approval_decisions.
	ackOpsSessionID = "mast-acks:mast-ops"
	ackOpsUserID    = "mast-ops"
	ackStatePrefix  = "mast_ack:"
	defaultAckApp   = "core-agent"
)

// AckRecord is the durable operator acknowledgement of an active monitoring
// subject (`Transition.SubjectKey`), stored in ADK session state on the
// reserved `"mast-acks:mast-ops"` session.
//
// An ack is an operational signal suppressing monitor.notify.remind_after
// reminders for an open subject; it is NOT an approval of a gated tool call
// and never writes a DecisionRecord.
type AckRecord struct {
	Workload  string    `json:"workload"`
	Subject   string    `json:"subject"`
	By        string    `json:"by"`
	ProxyBy   string    `json:"proxy_by,omitempty"`
	Reason    string    `json:"reason,omitempty"`
	At        time.Time `json:"at"`
	Forwarded bool      `json:"forwarded"`
}

func ackKey(workload, subject string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d:%s%d:%s", len(workload), workload, len(subject), subject)))
	return ackStatePrefix + hex.EncodeToString(sum[:])
}

func resolveAckApp(appName string) string {
	if strings.TrimSpace(appName) == "" {
		return defaultAckApp
	}
	return appName
}

// RecordAck persists rec into the reserved `"mast-acks:mast-ops"` session's
// durable state via AppendEvent StateDelta. Repeated acks for the same
// (Workload, Subject) overwrite the state entry in place.
func (h *Handle) RecordAck(ctx context.Context, appName string, rec AckRecord) error {
	if h == nil || h.Service == nil {
		return errors.New("eventlog: RecordAck: nil handle or session service")
	}
	if strings.TrimSpace(rec.Workload) == "" {
		return errors.New("eventlog: AckRecord.Workload is required")
	}
	if strings.TrimSpace(rec.Subject) == "" {
		return errors.New("eventlog: AckRecord.Subject is required")
	}
	if strings.TrimSpace(rec.By) == "" {
		return errors.New("eventlog: AckRecord.By is required")
	}
	if rec.At.IsZero() {
		rec.At = time.Now().UTC()
	} else {
		rec.At = rec.At.UTC()
	}

	app := resolveAckApp(appName)
	sess, err := h.ensureAckOpsSession(ctx, app)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("eventlog: marshal ack record: %w", err)
	}
	ev := session.NewEvent(ctx, "ack-"+ackKey(rec.Workload, rec.Subject))
	ev.Author = "system"
	ev.Actions.StateDelta = map[string]any{
		ackKey(rec.Workload, rec.Subject): string(raw),
	}
	if err := h.Service.AppendEvent(ctx, sess, ev); err != nil {
		return fmt.Errorf("eventlog: append ack state delta: %w", err)
	}
	return nil
}

// Ack looks up the durable AckRecord for (workload, subject). It returns
// (nil, nil) when no acknowledgement has been recorded.
func (h *Handle) Ack(ctx context.Context, appName, workload, subject string) (*AckRecord, error) {
	if h == nil || h.Service == nil {
		return nil, nil
	}
	app := resolveAckApp(appName)
	getResp, err := h.Service.Get(ctx, &session.GetRequest{
		AppName:   app,
		UserID:    ackOpsUserID,
		SessionID: ackOpsSessionID,
	})
	if err != nil || getResp == nil || getResp.Session == nil {
		return nil, nil
	}
	val, err := getResp.Session.State().Get(ackKey(workload, subject))
	if err != nil || val == nil {
		return nil, nil
	}
	raw, ok := val.(string)
	if !ok || raw == "" {
		return nil, nil
	}
	var rec AckRecord
	if err := json.Unmarshal([]byte(raw), &rec); err != nil {
		return nil, fmt.Errorf("eventlog: decode ack record: %w", err)
	}
	return &rec, nil
}

// Acks returns all recorded acknowledgements for workload (or across all
// workloads when workload is empty), ordered by At ascending.
func (h *Handle) Acks(ctx context.Context, appName, workload string) ([]AckRecord, error) {
	if h == nil || h.Service == nil {
		return nil, nil
	}
	app := resolveAckApp(appName)
	getResp, err := h.Service.Get(ctx, &session.GetRequest{
		AppName:   app,
		UserID:    ackOpsUserID,
		SessionID: ackOpsSessionID,
	})
	if err != nil || getResp == nil || getResp.Session == nil {
		return nil, nil
	}
	var out []AckRecord
	for k, v := range getResp.Session.State().All() {
		if !strings.HasPrefix(k, ackStatePrefix) {
			continue
		}
		raw, ok := v.(string)
		if !ok || raw == "" {
			continue
		}
		var rec AckRecord
		if err := json.Unmarshal([]byte(raw), &rec); err != nil {
			continue
		}
		if workload != "" && rec.Workload != workload {
			continue
		}
		out = append(out, rec)
	}
	slices.SortFunc(out, func(a, b AckRecord) int {
		if c := a.At.Compare(b.At); c != 0 {
			return c
		}
		return cmp.Compare(a.Subject, b.Subject)
	})
	return out, nil
}

func (h *Handle) ensureAckOpsSession(ctx context.Context, appName string) (session.Session, error) {
	getResp, err := h.Service.Get(ctx, &session.GetRequest{
		AppName:   appName,
		UserID:    ackOpsUserID,
		SessionID: ackOpsSessionID,
	})
	if err == nil && getResp != nil && getResp.Session != nil {
		return getResp.Session, nil
	}
	createResp, createErr := h.Service.Create(ctx, &session.CreateRequest{
		AppName:   appName,
		UserID:    ackOpsUserID,
		SessionID: ackOpsSessionID,
	})
	if createErr == nil && createResp != nil && createResp.Session != nil {
		return createResp.Session, nil
	}
	getResp, err = h.Service.Get(ctx, &session.GetRequest{
		AppName:   appName,
		UserID:    ackOpsUserID,
		SessionID: ackOpsSessionID,
	})
	if err != nil {
		return nil, fmt.Errorf("eventlog: create/get ack ops session: %w (create err: %v)", err, createErr)
	}
	return getResp.Session, nil
}
