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

package compose

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"

	"github.com/go-steer/core-agent/v3/internal/monitor"
	"github.com/go-steer/core-agent/v3/pkg/attach"
	"github.com/go-steer/core-agent/v3/pkg/auth"
	"github.com/go-steer/core-agent/v3/pkg/config"
	"github.com/go-steer/core-agent/v3/pkg/eventlog"
)

// MonitorAckerConfig configures a MonitorAcker (#1406).
type MonitorAckerConfig struct {
	WorkloadName string
	AppName      string
	UserID       string
	Monitor      *config.MonitorConfig
	AckArgs      map[string]any
	Toolsets     []tool.Toolset
	RunTool      func(ctx adkagent.Context, name string, args map[string]any) (map[string]any, error)
	EventLog     *eventlog.Handle
	Logger       *slog.Logger
	Now          func() time.Time
}

// MonitorAcker records operator acknowledgements durably on "mast-acks:mast-ops"
// and optionally forwards them to the configured monitor.ack MCP tool (#1406).
type MonitorAcker struct {
	workloadName string
	appName      string
	userID       string
	ackTool      string
	ackArgs      map[string]any
	toolsets     []tool.Toolset
	runTool      func(ctx adkagent.Context, name string, args map[string]any) (map[string]any, error)
	eventLog     *eventlog.Handle
	logger       *slog.Logger
	now          func() time.Time
}

// NewMonitorAcker constructs a MonitorAcker from cfg (#1406).
func NewMonitorAcker(cfg MonitorAckerConfig) *MonitorAcker {
	appName := strings.TrimSpace(cfg.AppName)
	if appName == "" {
		appName = "core-agent"
	}
	userID := strings.TrimSpace(cfg.UserID)
	if userID == "" {
		userID = "daemon"
	}
	workloadName := strings.TrimSpace(cfg.WorkloadName)
	if workloadName == "" {
		workloadName = appName
	}
	var ackTool string
	if cfg.Monitor != nil && cfg.Monitor.Ack != nil {
		ackTool = strings.TrimSpace(cfg.Monitor.Ack.Tool)
	}
	nowFn := cfg.Now
	if nowFn == nil {
		nowFn = func() time.Time { return time.Now().UTC() }
	}
	runFn := cfg.RunTool
	if runFn == nil {
		toolsets := cfg.Toolsets
		runFn = func(cctx adkagent.Context, name string, args map[string]any) (map[string]any, error) {
			return runOwnBehalf(cctx, toolsets, name, args)
		}
	}
	return &MonitorAcker{
		workloadName: workloadName,
		appName:      appName,
		userID:       userID,
		ackTool:      ackTool,
		ackArgs:      cfg.AckArgs,
		toolsets:     cfg.Toolsets,
		runTool:      runFn,
		eventLog:     cfg.EventLog,
		logger:       cfg.Logger,
		now:          nowFn,
	}
}

// Ack records the operator's acknowledgement durably and forwards it to the
// configured monitor.ack tool when present (#1406).
func (a *MonitorAcker) Ack(ctx context.Context, req attach.MonitorAckRequest) (attach.MonitorAckResult, error) {
	if a == nil {
		return attach.MonitorAckResult{}, errors.New("compose: MonitorAcker is nil")
	}
	if req.Workload != "" && a.workloadName != "" && req.Workload != a.workloadName {
		return attach.MonitorAckResult{}, fmt.Errorf("%w: this daemon serves workload %q, not %q", attach.ErrMonitorAckBadRequest, a.workloadName, req.Workload)
	}
	subject := strings.TrimSpace(req.Subject)
	if subject == "" {
		return attach.MonitorAckResult{}, fmt.Errorf("%w: subject is required", attach.ErrMonitorAckBadRequest)
	}

	by, proxyBy, err := resolveAckIdentity(ctx, req)
	if err != nil {
		return attach.MonitorAckResult{}, err
	}

	prevBy, prevAt := a.lookupPreviousAck(ctx, subject)

	rec := eventlog.AckRecord{
		Workload:  a.workloadName,
		Subject:   subject,
		By:        by,
		ProxyBy:   proxyBy,
		Reason:    strings.TrimSpace(req.Reason),
		At:        a.now().UTC(),
		Forwarded: false,
	}
	if a.eventLog != nil {
		if err := a.eventLog.RecordAck(ctx, a.appName, rec); err != nil {
			return attach.MonitorAckResult{}, fmt.Errorf("recording the ack durably: %w", err)
		}
	}

	if a.ackTool != "" {
		if err := a.forwardAck(ctx, subject, by, rec.Reason); err != nil {
			return attach.MonitorAckResult{}, err
		}
		rec.Forwarded = true
		if a.eventLog != nil {
			if err := a.eventLog.RecordAck(ctx, a.appName, rec); err != nil && a.logger != nil {
				a.logger.Warn("ack forwarded but durable record update failed",
					"workload", a.workloadName, "subject", subject, "error", err.Error())
			}
		}
	}

	return attach.MonitorAckResult{
		Workload:          a.workloadName,
		Subject:           subject,
		By:                by,
		ProxyBy:           proxyBy,
		At:                rec.At,
		Forwarded:         rec.Forwarded,
		PreviouslyAckedBy: prevBy,
		PreviouslyAckedAt: prevAt,
	}, nil
}

func resolveAckIdentity(ctx context.Context, req attach.MonitorAckRequest) (string, string, error) {
	by := strings.TrimSpace(req.By)
	proxyBy := strings.TrimSpace(req.ProxyBy)
	if by == "" {
		if caller, ok := auth.CallerFromContext(ctx); ok {
			by = strings.TrimSpace(caller.Identity)
		}
		if p, ok := auth.ProxyByFromContext(ctx); ok {
			proxyBy = strings.TrimSpace(p)
		}
	}
	if by == "" || by == "anonymous" || by == auth.Anonymous.Identity {
		return "", "", fmt.Errorf("%w: no authenticated caller on this request, so the ack cannot be attributed to anyone", attach.ErrMonitorAckAnonymous)
	}
	return by, proxyBy, nil
}

func (a *MonitorAcker) lookupPreviousAck(ctx context.Context, subject string) (string, string) {
	if a.eventLog == nil {
		return "", ""
	}
	prev, err := a.eventLog.Ack(ctx, a.appName, a.workloadName, subject)
	if err != nil || prev == nil {
		return "", ""
	}
	return prev.By, prev.At.Format(time.RFC3339)
}

func (a *MonitorAcker) forwardAck(ctx context.Context, subject, by, reason string) error {
	actx := newCollectContext(ctx, a.appName, a.userID, "")
	args := make(map[string]any, len(a.ackArgs)+2)
	for k, v := range a.ackArgs {
		args[k] = v
	}
	// Overwrite any bundle-supplied subject_key or ack_by so caller attribution
	// is always derived from the authenticated credential.
	args[monitor.AckSubjectArg] = subject
	args[monitor.AckByArg] = by
	if reason != "" {
		if _, taken := args["reason"]; !taken {
			args["reason"] = reason
		}
	}
	if _, err := a.runTool(actx, a.ackTool, args); err != nil {
		if a.logger != nil {
			a.logger.Error("operator ack recorded but not forwarded; the suppression did not take",
				"workload", a.workloadName, "subject", subject, "ack_by", by,
				"tool", a.ackTool, "error", err.Error())
		}
		return fmt.Errorf("forwarding the ack to %s: %w", a.ackTool, err)
	}
	return nil
}
