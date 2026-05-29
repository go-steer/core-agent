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

package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
)

// RemoteAgentSpawner is implemented by consumers who want the parent
// agent's model to be able to spawn out-of-process subagents — gRPC
// to a remote agent server, K8s Jobs, Cloud Run, NATS-dispatched
// workers, anything that runs an agent somewhere other than this
// process. core-agent stays agnostic about transport: the consumer's
// Spawn implementation is responsible for whatever IPC and lifecycle
// the substrate requires.
//
// Mirrors the consumer-pluggability shape of tools.NewAskUserTool +
// tools.Prompter: a small interface the host implements, wired into
// a tool the model can call uniformly.
type RemoteAgentSpawner interface {
	// Spawn launches a remote subagent under spec and returns a
	// handle the manager uses for status checks, stop, and event
	// fan-in. Errors at this point propagate as Go errors (caller
	// is wiring the spawner directly); errors that surface later
	// flow through RemoteAgentHandle.Events() as Kind="failed".
	Spawn(ctx context.Context, spec RemoteAgentSpec) (RemoteAgentHandle, error)
}

// RemoteAgentSpec is what the manager hands to the consumer's
// spawner — the same shape as the in-process BackgroundSpec, plus a
// stable opaque ID the consumer can use as a primary key in their
// substrate (e.g. the K8s Job name). Spec.Name is the human-facing
// identifier the parent's model chose; Spec.ID is the manager's
// invariant under the manager's registry. They're usually the same
// string but kept distinct so consumers don't have to guess.
type RemoteAgentSpec struct {
	ID           string
	Name         string
	SystemPrompt string
	Goal         string
	Tools        []string
	Extras       []string
	Budgets      BackgroundBudgets
}

// RemoteAgentHandle is the contract for a running remote subagent.
// Implementations live in the consumer's package (the substrate
// adapter); the manager treats it opaquely apart from draining
// Events() for the alert pipeline.
type RemoteAgentHandle interface {
	// ID returns the spawner-assigned identifier for diagnostics.
	// Stable across the handle's lifetime.
	ID() string

	// Status returns the current lifecycle state of the remote
	// subagent. Implementations are expected to be cheap (cached
	// status from the last received event is fine).
	Status(ctx context.Context) (RemoteAgentStatus, error)

	// Stop signals the remote subagent to terminate. The
	// implementation decides whether this is best-effort (e.g.
	// async cancel of a K8s Job) or synchronous.
	Stop(ctx context.Context) error

	// Events returns a channel of events streamed from the remote
	// subagent. The consumer's implementation closes the channel
	// once the remote has terminated; the manager's fan-in
	// goroutine exits on close.
	Events() <-chan RemoteAgentEvent
}

// RemoteAgentStatus mirrors BackgroundStatus but lives in the remote
// space. Implementations map their native states (K8s Job phase,
// HTTP response, etc.) onto these.
type RemoteAgentStatus int

const (
	RemoteStatusPending RemoteAgentStatus = iota
	RemoteStatusRunning
	RemoteStatusCompleted
	RemoteStatusFailed
	RemoteStatusStopped
)

func (s RemoteAgentStatus) String() string {
	switch s {
	case RemoteStatusPending:
		return "pending"
	case RemoteStatusRunning:
		return "running"
	case RemoteStatusCompleted:
		return "completed"
	case RemoteStatusFailed:
		return "failed"
	case RemoteStatusStopped:
		return "stopped"
	default:
		return "?"
	}
}

// RemoteAgentEvent is what the consumer's transport delivers back
// from the remote subagent. The Kind field is the manager's hook for
// classifying — "alert" gets fanned into the parent's alert channel
// as a normal Alert; terminal kinds ("completed" / "failed" /
// "stopped") trigger the terminal Alert + handle close.
type RemoteAgentEvent struct {
	Kind      string // "alert" | "log" | "completed" | "failed" | "stopped"
	Text      string
	Timestamp time.Time
}

// RefuseRemoteAgentSpawner returns a spawner whose Spawn always
// errors with reason. Use it as the default spawner when running
// headless / unattended so the model sees a clean tool result it
// can adapt to, rather than the bundled CLI crashing on a nil
// dereference. Analog of tools.RefusePrompter.
func RefuseRemoteAgentSpawner(reason string) RemoteAgentSpawner {
	if reason == "" {
		reason = "no remote agent spawner is configured for this agent run"
	}
	return refuseRemoteSpawner{reason: reason}
}

type refuseRemoteSpawner struct{ reason string }

func (r refuseRemoteSpawner) Spawn(_ context.Context, _ RemoteAgentSpec) (RemoteAgentHandle, error) {
	return nil, errors.New(r.reason)
}

// ErrNoSpawner is returned by NewSpawnRemoteAgentTool when nil is
// passed for the spawner. Use RefuseRemoteAgentSpawner instead of
// nil when you want the tool registered but no-op.
var ErrNoSpawner = errors.New("agent: NewSpawnRemoteAgentTool: spawner is required (use RefuseRemoteAgentSpawner for the headless / unattended case)")

// spawnRemoteAgentArgs mirrors spawnAgentArgs — same fields so the
// model can use whichever spawn tool with the same mental model.
type spawnRemoteAgentArgs struct {
	Name                string   `json:"name" jsonschema:"unique short identifier for this remote subagent (no spaces, dots or slashes)"`
	SystemPrompt        string   `json:"system_prompt" jsonschema:"the remote subagent's system instruction"`
	Goal                string   `json:"goal" jsonschema:"the task the remote subagent should accomplish"`
	Tools               []string `json:"tools,omitempty" jsonschema:"tool names the remote subagent may use (interpretation depends on the consumer's substrate)"`
	Extras              []string `json:"extras,omitempty" jsonschema:"additional tool names beyond the built-ins"`
	MaxTurns            int      `json:"max_turns,omitempty"`
	MaxCostUSD          float64  `json:"max_cost_usd,omitempty"`
	MaxWallclockSeconds int      `json:"max_wallclock_seconds,omitempty"`
}

type spawnRemoteAgentResult struct {
	Name   string `json:"name"`
	ID     string `json:"id"`
	Status string `json:"status"`
}

// NewSpawnRemoteAgentTool returns a tool the parent's model can call
// to launch an out-of-process subagent via the consumer-supplied
// spawner. The handle's Events() channel is drained by a goroutine
// the manager starts inside Spawn; events of Kind="alert" land on
// the manager's alert channel under the subagent's name, and the
// terminal handle status is recorded for list/check/stop uniformity
// alongside in-process subagents.
//
// Pass nil for mgr to skip the alert + registry fan-in (alerts will
// be dropped); typically you want both wired, especially for the
// bundled CLI.
func NewSpawnRemoteAgentTool(spawner RemoteAgentSpawner, mgr *BackgroundAgentManager) (tool.Tool, error) {
	if spawner == nil {
		return nil, ErrNoSpawner
	}
	handler := func(toolCtx tool.Context, args spawnRemoteAgentArgs) (spawnRemoteAgentResult, error) {
		spec := RemoteAgentSpec{
			ID:           args.Name,
			Name:         args.Name,
			SystemPrompt: args.SystemPrompt,
			Goal:         args.Goal,
			Tools:        args.Tools,
			Extras:       args.Extras,
			Budgets: BackgroundBudgets{
				MaxTurns:     args.MaxTurns,
				MaxCost:      args.MaxCostUSD,
				MaxWallclock: time.Duration(args.MaxWallclockSeconds) * time.Second,
			},
		}
		h, err := spawner.Spawn(toolCtx, spec)
		if err != nil {
			return spawnRemoteAgentResult{
				Name:   args.Name,
				Status: "error: " + err.Error(),
			}, nil
		}
		// Register with the manager so list/check/stop see remote
		// subagents alongside in-process ones, and drain events
		// into the alert pipeline.
		if mgr != nil {
			mgr.registerRemote(h, spec)
		}
		status := "running"
		if st, err := h.Status(toolCtx); err == nil {
			status = st.String()
		}
		return spawnRemoteAgentResult{
			Name:   spec.Name,
			ID:     h.ID(),
			Status: status,
		}, nil
	}
	t, err := functiontool.New(functiontool.Config{
		Name:        "spawn_remote_agent",
		Description: "Spawn an out-of-process subagent via the consumer-configured RemoteAgentSpawner (e.g. a K8s Job, gRPC remote agent, Cloud Run job). Same model-facing shape as spawn_agent; the actual subagent runs elsewhere. Use when you need substrate-specific isolation, scale-out beyond this process, or hardware/permission boundaries the in-process spawner can't provide.",
	}, handler)
	if err != nil {
		return nil, err
	}
	return t, nil
}

// registerRemote inserts a remote handle into the manager's registry
// (so list/check/stop work uniformly) and starts a fan-in goroutine
// that maps events from the remote into the alert channel. Called by
// NewSpawnRemoteAgentTool's handler.
func (m *BackgroundAgentManager) registerRemote(rh RemoteAgentHandle, spec RemoteAgentSpec) {
	bh := &BackgroundHandle{
		Name:      spec.Name,
		Branch:    "remote." + spec.Name,
		StartedAt: time.Now(),
		status:    StatusRunning,
		done:      make(chan struct{}),
	}
	// The remote's Stop replaces our cancel-func semantics — we
	// don't have a Go ctx to cancel, the consumer's Stop is what
	// signals termination.
	bh.cancel = func() {
		_ = rh.Stop(context.Background())
	}

	m.mu.Lock()
	m.agents[spec.Name] = bh
	m.mu.Unlock()

	go m.fanInRemote(rh, bh, spec.Name)
}

// fanInRemote drains the remote's event channel, maps each event
// onto the manager's alert pipeline, and updates the BackgroundHandle
// status when terminal events arrive. Exits when the remote closes
// its channel.
func (m *BackgroundAgentManager) fanInRemote(rh RemoteAgentHandle, bh *BackgroundHandle, name string) {
	defer close(bh.done)
	var once sync.Once
	finalize := func(s BackgroundStatus) {
		once.Do(func() {
			bh.mu.Lock()
			if bh.status == StatusRunning {
				bh.status = s
			}
			bh.mu.Unlock()
		})
	}
	for ev := range rh.Events() {
		switch ev.Kind {
		case "alert", "":
			m.pushAlert(Alert{
				From:      name,
				Text:      ev.Text,
				Kind:      "alert",
				Timestamp: nowOr(ev.Timestamp),
			})
		case "completed":
			finalize(StatusCompleted)
			m.pushAlert(Alert{
				From: name, Text: ev.Text,
				Kind: "completed", Timestamp: nowOr(ev.Timestamp),
			})
		case "failed":
			finalize(StatusFailed)
			m.pushAlert(Alert{
				From: name, Text: ev.Text,
				Kind: "failed", Timestamp: nowOr(ev.Timestamp),
			})
		case "stopped":
			finalize(StatusStopped)
			m.pushAlert(Alert{
				From: name, Text: ev.Text,
				Kind: "stopped", Timestamp: nowOr(ev.Timestamp),
			})
		case "log":
			// Logs aren't surfaced as alerts (would drown the
			// parent's context). Consumers who want them should
			// poll Status() / their own substrate. Could project
			// to the eventlog in a future release.
		default:
			// Unknown kinds — surface as alerts so the parent at
			// least sees the text. Better than silently dropping.
			m.pushAlert(Alert{
				From: name, Text: fmt.Sprintf("[%s] %s", ev.Kind, ev.Text),
				Kind: "alert", Timestamp: nowOr(ev.Timestamp),
			})
		}
	}
	// Channel closed without a terminal kind — treat as completed
	// so list/check/stop converge.
	finalize(StatusCompleted)
}

func nowOr(t time.Time) time.Time {
	if t.IsZero() {
		return time.Now()
	}
	return t
}
