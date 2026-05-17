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

package scionremote

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"

	"github.com/go-steer/core-agent/agent"
)

// handle is the per-spawn record. Implements agent.RemoteAgentHandle.
// One handle per spawned sibling agent; the streamLogs goroutine
// drains the Hub's SSE cloud-logs stream and converts each entry
// into an agent.RemoteAgentEvent on the events channel.
type handle struct {
	svc        hubclient.AgentService
	agentID    string
	classifier Classifier

	events chan agent.RemoteAgentEvent

	mu        sync.Mutex
	stopped   bool
	closeOnce sync.Once
}

func newHandle(svc hubclient.AgentService, agentID string, classifier Classifier) *handle {
	return &handle{
		svc:        svc,
		agentID:    agentID,
		classifier: classifier,
		events:     make(chan agent.RemoteAgentEvent, 64),
	}
}

// ID returns the Scion-assigned agent ID. Implements
// agent.RemoteAgentHandle.
func (h *handle) ID() string { return h.agentID }

// Status returns the current Scion phase mapped onto our
// RemoteAgentStatus enum. Implements agent.RemoteAgentHandle.
func (h *handle) Status(ctx context.Context) (agent.RemoteAgentStatus, error) {
	a, err := h.svc.Get(ctx, h.agentID)
	if err != nil {
		return agent.RemoteStatusFailed, fmt.Errorf("scion-remote-agent: status %q: %w", h.agentID, err)
	}
	return mapPhase(a.Phase), nil
}

// Stop signals the Scion Hub to stop the spawned agent. Idempotent;
// repeated calls return nil even after the first succeeded.
// Implements agent.RemoteAgentHandle.
func (h *handle) Stop(ctx context.Context) error {
	h.mu.Lock()
	already := h.stopped
	h.stopped = true
	h.mu.Unlock()
	if already {
		return nil
	}
	if err := h.svc.Stop(ctx, h.agentID); err != nil {
		return fmt.Errorf("scion-remote-agent: stop %q: %w", h.agentID, err)
	}
	return nil
}

// Events returns the channel the streamLogs goroutine populates.
// Closed when the SSE stream ends and the goroutine exits.
// Implements agent.RemoteAgentHandle.
func (h *handle) Events() <-chan agent.RemoteAgentEvent { return h.events }

// streamLogs is the background goroutine launched by Spawner.Spawn.
// Drains Hub's SSE cloud-logs stream until ctx is cancelled or the
// connection ends; classifies each entry and pushes onto h.events.
// Closes h.events on exit.
func (h *handle) streamLogs(ctx context.Context) {
	defer h.closeEvents()
	opts := &hubclient.GetCloudLogsOptions{}
	err := h.svc.StreamCloudLogs(ctx, h.agentID, opts, func(entry hubclient.CloudLogEntry) {
		ev, ok := h.classifier(entry)
		if !ok {
			return
		}
		// Push with drop-oldest backpressure so a stuck downstream
		// consumer can't deadlock the SSE goroutine.
		select {
		case h.events <- ev:
		default:
			// Drop oldest to make room.
			select {
			case <-h.events:
			default:
			}
			select {
			case h.events <- ev:
			default:
			}
		}
	})
	if err != nil && ctx.Err() == nil {
		// Stream ended with an error unrelated to caller-driven
		// cancellation. Surface as a failed event so the manager's
		// fan-in goroutine records a terminal state.
		h.tryPush(agent.RemoteAgentEvent{
			Kind:      "failed",
			Text:      "stream-cloud-logs: " + err.Error(),
			Timestamp: time.Now(),
		})
	}
}

// closeEvents closes the events channel exactly once.
func (h *handle) closeEvents() {
	h.closeOnce.Do(func() { close(h.events) })
}

// tryPush sends ev to the events channel without blocking; drops
// it if the channel is full. Used for the final terminal event
// where downstream may already have closed.
func (h *handle) tryPush(ev agent.RemoteAgentEvent) {
	defer func() { _ = recover() }() // events may be closed
	select {
	case h.events <- ev:
	default:
	}
}

// mapPhase translates a Scion agent phase string onto our
// RemoteAgentStatus enum. Scion's phases per /pkg/hubclient/types.go:
//
//	created / provisioning / running / stopped / error / suspended
//
// Anything unrecognised maps to Running (conservative — better to
// keep watching than declare it terminal).
func mapPhase(phase string) agent.RemoteAgentStatus {
	switch phase {
	case "running":
		return agent.RemoteStatusRunning
	case "created", "provisioning":
		return agent.RemoteStatusPending
	case "stopped":
		return agent.RemoteStatusStopped
	case "error", "failed":
		return agent.RemoteStatusFailed
	case "suspended":
		// Suspended is a Scion-specific paused state that has no
		// 1:1 with our enum. Treat as Running so the watcher
		// keeps the handle alive; consumers who care can poll
		// Get() directly via the underlying client.
		return agent.RemoteStatusRunning
	default:
		return agent.RemoteStatusRunning
	}
}
