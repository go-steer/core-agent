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

package effects

import (
	"context"
	"fmt"
	"iter"
	"log/slog"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool/toolconfirmation"
	"google.golang.org/genai"
)

const subRunRecordTimeout = 5 * time.Second

// DeriveSubSessionID computes the deterministic child session ID used
// by synchronous subagent dispatches ("<parent>:sub:<subName>:<callID>").
func DeriveSubSessionID(parentSessionID, subName, callID string) string {
	id := "sub:" + subName
	if parentSessionID != "" {
		id = parentSessionID + ":" + id
	}
	if callID != "" {
		id = id + ":" + callID
	}
	return id
}

// SubSessionScannerFor builds a SubSessionScanner over svc that locates
// deterministic child sessions ("<parentSID>:sub:<subName>:<callID>")
// and returns any dangling mutating intents inside them.
func SubSessionScannerFor(svc session.Service, appName, userID string, pred Predicate, subAgents map[string]bool) SubSessionScanner {
	if svc == nil || pred == nil {
		return nil
	}
	return func(ctx context.Context, parentSessionID string, spawn DanglingIntent) ([]DanglingIntent, bool) {
		childID := DeriveSubSessionID(parentSessionID, spawn.ToolName, spawn.CallID)
		resp, err := svc.Get(ctx, &session.GetRequest{
			AppName:   appName,
			UserID:    userID,
			SessionID: childID,
		})
		if err != nil || resp == nil || resp.Session == nil {
			return nil, false
		}
		scan := ScanDangling(resp.Session.Events(), pred, subAgents)
		return scan.Mutating, true
	}
}

// SubRunnerFunc is the execution signature of runner.Runner.Run.
type SubRunnerFunc func(ctx context.Context, userID, sessionID string, msg *genai.Content, cfg agent.RunConfig, opts ...runner.RunOption) iter.Seq2[*session.Event, error]

// WrapSubRunner wraps a sub-runner's Run method so any mutating intent
// and completion emitted by the sub-runner is recorded via rec.
func WrapSubRunner(run SubRunnerFunc, rec *SubRunRecorder) SubRunnerFunc {
	if rec == nil {
		return run
	}
	return func(ctx context.Context, userID, sessionID string, msg *genai.Content, cfg agent.RunConfig, opts ...runner.RunOption) iter.Seq2[*session.Event, error] {
		return func(yield func(*session.Event, error) bool) {
			for ev, err := range run(ctx, userID, sessionID, msg, cfg, opts...) {
				if err != nil {
					yield(ev, err)
					return
				}
				if obsErr := rec.Observe(ev); obsErr != nil {
					yield(nil, obsErr)
					return
				}
				if !yield(ev, nil) {
					return
				}
			}
		}
	}
}

// WrapADKRunner wraps *runner.Runner.Run with rec.
func WrapADKRunner(r *runner.Runner, rec *SubRunRecorder) SubRunnerFunc {
	return WrapSubRunner(r.Run, rec)
}

// IntentStore persists a sub-run's mutating intents somewhere the
// workload's own session can be scanned for them later.
type IntentStore interface {
	RecordSubRunIntents(ctx context.Context, sessionID, specialist string, intents []DanglingIntent) error
	CompleteSubRunIntents(ctx context.Context, sessionID string, callIDs []string) error
}

// SubRunRecorderConfig configures a recorder.
type SubRunRecorderConfig struct {
	Store         IntentStore
	SessionID     string
	Specialist    string
	Predicate     Predicate
	SubAgentNames map[string]bool
	Logger        *slog.Logger
}

// NewSubRunRecorder builds the recording half of the dispatch seam.
func NewSubRunRecorder(cfg SubRunRecorderConfig) (*SubRunRecorder, error) {
	if cfg.Store == nil {
		return nil, fmt.Errorf("effects: SubRunRecorderConfig.Store is required")
	}
	if cfg.Predicate == nil {
		return nil, fmt.Errorf("effects: SubRunRecorderConfig.Predicate is required")
	}
	if cfg.SessionID == "" {
		return nil, fmt.Errorf("effects: SubRunRecorderConfig.SessionID is required (a dispatch's own session is a throwaway and cannot be the filing key)")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &SubRunRecorder{cfg: cfg}, nil
}

// SubRunRecorder writes one dispatch's mutating intents and completions
// to an IntentStore.
type SubRunRecorder struct {
	cfg SubRunRecorderConfig
}

// Observe records the mutating intents an event raises and pairs the
// ones it completes.
func (r *SubRunRecorder) Observe(ev *session.Event) error {
	raised, completed := MutatingCalls(ev, r.cfg.Predicate, r.cfg.SubAgentNames)
	if len(completed) > 0 {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), subRunRecordTimeout)
		err := r.cfg.Store.CompleteSubRunIntents(ctx, r.cfg.SessionID, completed)
		cancel()
		if err != nil {
			r.cfg.Logger.Error("could not pair a dispatched specialist's completed effect; it stays dangling and the next turn will refuse mutating calls until an operator acks",
				"session", r.cfg.SessionID, "specialist", r.cfg.Specialist,
				"calls", completed, "error", err.Error())
		}
	}
	if len(raised) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), subRunRecordTimeout)
	defer cancel()
	if err := r.cfg.Store.RecordSubRunIntents(ctx, r.cfg.SessionID, r.cfg.Specialist, raised); err != nil {
		r.cfg.Logger.Error("stopping a planner dispatch: its mutating call could not be recorded, and an unrecorded mutation is the hole this seam exists to close",
			"session", r.cfg.SessionID, "specialist", r.cfg.Specialist,
			"calls", describe(raised), "error", err.Error())
		return fmt.Errorf("effects: recording %s's intent to call %s could not be made durable, so the call was not made: %w",
			r.cfg.Specialist, describe(raised), err)
	}
	return nil
}

// MutatingCalls splits one event into the mutating- or spawning-class
// FunctionCalls it raises and the call IDs its FunctionResponses
// complete.
func MutatingCalls(ev *session.Event, pred Predicate, subAgents map[string]bool) (raised []DanglingIntent, completed []string) {
	if ev == nil || ev.Content == nil || pred == nil || isSyntheticRepairEvent(ev) {
		return nil, nil
	}
	longRunning := make(map[string]bool, len(ev.LongRunningToolIDs))
	for _, id := range ev.LongRunningToolIDs {
		longRunning[id] = true
	}
	for _, part := range ev.Content.Parts {
		if part == nil {
			continue
		}
		if fc := part.FunctionCall; fc != nil && fc.ID != "" {
			if longRunning[fc.ID] || controlCalls[fc.Name] || subAgents[fc.Name] {
				continue
			}
			if c := pred(fc.Name); c != ClassMutating && c != ClassSpawning {
				continue
			}
			raised = append(raised, DanglingIntent{
				ToolName:     fc.Name,
				CallID:       fc.ID,
				InvocationID: ev.InvocationID,
				Timestamp:    ev.Timestamp,
				EventIndex:   -1,
			})
		}
		if fr := part.FunctionResponse; fr != nil && fr.ID != "" {
			if _, pending := ev.Actions.RequestedToolConfirmations[fr.ID]; pending {
				continue
			}
			if fr.Name == toolconfirmation.FunctionCallName {
				continue
			}
			completed = append(completed, fr.ID)
		}
	}
	return raised, completed
}

// WithExternal folds intents recorded outside this session's event log
// into a scan.
func (s DanglingScan) WithExternal(external []DanglingIntent) DanglingScan {
	if len(external) == 0 {
		return s
	}
	out := s
	out.Mutating = append(append([]DanglingIntent(nil), s.Mutating...), external...)
	sortByTimestamp(out.Mutating)
	return out
}
