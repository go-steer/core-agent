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
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	coretui "github.com/go-steer/core-tui/tui"

	"github.com/go-steer/core-agent/v2/internal/attachclient"
	"github.com/go-steer/core-agent/v2/pkg/attach"
)

// StartRemotePrompter wires the remote agent's permission prompts
// into a coretui.Prompter so the local TUI's modal can render them.
// Returns the prompter (to pass into coretui.Options.Prompter) and a
// stop func the caller invokes when the program ends.
//
// The bridge goroutine ranges over /perms/stream; each frame is
// handed to prompter.AskApproval (which blocks until the operator
// picks a decision in the modal), then the decision is POSTed back
// via /perms/respond. If the remote daemon wasn't constructed with
// WithAttachPromptBroker the initial GET returns 501; the bridge
// logs once and returns (the returned prompter sits idle and the
// daemon's gate then surfaces its usual "no prompter configured"
// error, which is the correct headless-mode behavior).
//
// errOut receives one-line diagnostics about the bridge's network
// trouble (transient stream errors, 404 on response). Pass nil to
// drop them.
func StartRemotePrompter(ctx context.Context, client *attachclient.Client, sessionPath string, errOut io.Writer) (coretui.PermissionPrompter, func()) {
	prompter := coretui.NewPrompter()
	bridgeCtx, cancel := context.WithCancel(ctx)
	go runRemotePromptBridge(bridgeCtx, client, sessionPath, prompter, errOut)
	return prompter, cancel
}

func runRemotePromptBridge(ctx context.Context, client *attachclient.Client, sessionPath string, prompter *coretui.Prompter, errOut io.Writer) {
	const (
		initialBackoff = 5 * time.Second
		maxBackoff     = 30 * time.Second
	)
	backoff := initialBackoff
	for ctx.Err() == nil {
		debugf("prompt bridge: connecting to %s/perms/stream", sessionPath)
		frames, err := client.PromptStream(ctx, sessionPath)
		if err != nil {
			// 501 = daemon has no broker → don't reconnect-loop.
			if strings.Contains(err.Error(), "501") || strings.Contains(err.Error(), "not registered") {
				debugf("prompt bridge: capability not registered on daemon (%v); exiting", err)
				logBridge(errOut, "remote prompt bridge: capability not registered on daemon (%v)", err)
				return
			}
			debugf("prompt bridge: connect failed: %v; sleeping %s", err, backoff)
			logBridge(errOut, "remote prompt stream: %v; retrying in %v", err, backoff)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff < maxBackoff {
				backoff *= 2
			}
			continue
		}
		debugf("prompt bridge: connected; reading frames")
		backoff = initialBackoff
		for frame := range frames {
			if ctx.Err() != nil {
				return
			}
			debugf("prompt bridge: frame id=%s kind=%s tool=%s", frame.ID, frame.Kind, frame.ToolName)
			handleRemotePromptFrame(ctx, client, sessionPath, prompter, frame, errOut)
		}
		debugf("prompt bridge: stream closed; will reconnect after %s", backoff)
	}
}

func handleRemotePromptFrame(ctx context.Context, client *attachclient.Client, sessionPath string, prompter *coretui.Prompter, frame attach.PromptFrame, errOut io.Writer) {
	req := coretui.PermissionRequest{
		Kind:        permissionKindFromWire(frame.Kind),
		ToolName:    frame.ToolName,
		Detail:      frame.Detail,
		DetailKind:  detailKindFor(frame),
		Verb:        frame.Verb,
		Source:      frame.Source,
		PersistTool: frame.PersistTool,
		PersistKey:  frame.PersistKey,
	}
	decision, err := prompter.AskApproval(ctx, req)
	if err != nil {
		// ctx cancelled or prompter torn down mid-decision; nothing to send.
		return
	}
	wire := decisionToWire(decision)
	if rerr := client.RespondToPrompt(ctx, sessionPath, frame.ID, wire); rerr != nil {
		logBridge(errOut, "remote prompt respond (id=%s decision=%s): %v", frame.ID, wire, rerr)
	}
}

func permissionKindFromWire(s string) coretui.PermissionKind {
	switch s {
	case "bash":
		return coretui.PermissionKindBash
	case "file_write":
		return coretui.PermissionKindEdit
	default:
		// path_scope + generic both render as "other" — coretui's
		// modal handles the detail rendering, the kind only steers
		// the title/icon.
		return coretui.PermissionKindOther
	}
}

// detailKindFor picks the modal's payload renderer based on the
// frame's tool. Bash gets the shell renderer; file_write gets the
// diff renderer if the detail looks like one; everything else falls
// to the generic args renderer. The remote frame doesn't carry the
// payload type explicitly today, so we infer from heuristics.
func detailKindFor(frame attach.PromptFrame) coretui.DetailKind {
	switch frame.Kind {
	case "bash":
		return coretui.DetailShell
	case "file_write":
		if strings.Contains(frame.Detail, "\n@@") || strings.HasPrefix(frame.Detail, "---") {
			return coretui.DetailDiff
		}
		return coretui.DetailArgs
	}
	return coretui.DetailArgs
}

func decisionToWire(d coretui.PermissionDecision) string {
	switch d {
	case coretui.DecisionAllowOnce:
		return "allow-once"
	case coretui.DecisionAllowSession:
		return "allow-session"
	case coretui.DecisionAllowSessionVerb:
		return "allow-session-verb"
	case coretui.DecisionAllowSessionTool:
		return "allow-session-tool"
	case coretui.DecisionAllowAlways:
		return "allow-always"
	}
	return "deny"
}

func logBridge(w io.Writer, format string, args ...any) {
	if w == nil {
		w = os.Stderr
	}
	fmt.Fprintf(w, "core-agent-tui: "+format+"\n", args...)
}
