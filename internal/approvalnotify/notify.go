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

// Package approvalnotify wires the permission gate's unwatched-prompt
// signal to the operator's alert registry (#647).
//
// The gate can now bound how long a prompt waits
// (permissions.approval_timeout). That stops an unattended daemon from
// hanging forever on an approval nobody will give. It does not tell
// anybody the approval was wanted — so on its own the bound converts a
// hung agent into an agent that quietly gives up, which is better but
// still not operable.
//
// This package closes the other half: when a prompt opens and the
// fan-out reaches nobody, one notification goes out to a pre-registered
// alert target carrying what was asked, which session asked it, the
// request id, and when it expires. That is the difference between a
// doorbell and a door.
package approvalnotify

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/go-steer/core-agent/v2/pkg/attach"
	"github.com/go-steer/core-agent/v2/pkg/config"
	"github.com/go-steer/core-agent/v2/pkg/tools/alert"
)

// sender is the subset of *alert.Sender this package needs, so tests
// can observe a notification without standing up a webhook.
type sender interface {
	Send(ctx context.Context, level, summary string, details map[string]any, session string) error
	Target() string
}

// Notifier turns unwatched prompts into alerts on one target.
type Notifier struct {
	snd sender
	log *slog.Logger
}

// New builds a Notifier for cfg.Permissions.ApprovalNotify, or (nil,
// nil) when the operator did not ask for one.
//
// An error here is a startup error and callers should treat it as
// fatal rather than degrading to no notification. The whole point of
// the field is that an operator running gated and unattended has stated
// they cannot watch the console; starting anyway, with a warning on that
// same console, hands them exactly the silence they configured against.
func New(cfg *config.Config, log *slog.Logger) (*Notifier, error) {
	if cfg == nil || cfg.Permissions.ApprovalNotify == "" {
		return nil, nil
	}
	snd, err := alert.NewSender(cfg, cfg.Permissions.ApprovalNotify)
	if err != nil {
		return nil, fmt.Errorf("permissions.approval_notify: %w", err)
	}
	if log == nil {
		log = slog.Default()
	}
	return &Notifier{snd: snd, log: log}, nil
}

// Target reports where notifications will be sent, for a startup log
// line that lets an operator confirm the wiring before they rely on it.
func (n *Notifier) Target() string {
	if n == nil {
		return ""
	}
	return n.snd.Target()
}

// Attach installs n on broker for the session the getter names. A nil
// Notifier is a no-op, so callers can wire unconditionally.
//
// The session id is supplied rather than read off the frame because the
// broker is per-session and the frame does not carry the id — and the id
// is the one thing a responder cannot do without, since the respond
// route is /sessions/<sid>/perms/respond.
//
// It is a GETTER rather than a string because of when the wiring
// happens. On the daemon path the broker is constructed and handed to
// the gate while the primary agent is still just a list of options —
// agent.New does not run until the runner starts — so there is no
// session id to read yet, and on a multi-session daemon there is no
// primary agent at all and never will be. Taking a string here invites
// the caller to resolve it eagerly, which is a nil dereference at
// startup rather than a missing field in one notification.
func (n *Notifier) Attach(broker *attach.PromptBroker, session func() string) {
	if n == nil || broker == nil {
		return
	}
	broker.SetUnwatchedNotifier(func(ctx context.Context, p attach.UnwatchedPrompt) {
		sid := ""
		if session != nil {
			sid = session()
		}
		n.notify(ctx, sid, p)
	})
}

// AttachSession is Attach for a caller that already holds the id — the
// per-session factory, where the session exists before its broker does.
func (n *Notifier) AttachSession(broker *attach.PromptBroker, session string) {
	n.Attach(broker, func() string { return session })
}

func (n *Notifier) notify(ctx context.Context, session string, p attach.UnwatchedPrompt) {
	// A route with an empty segment reads as a typo in our code rather
	// than as something the recipient has to fill in, and they would be
	// right either way — so say which part is missing.
	route := session
	if route == "" {
		route = "{session_id}"
	}
	details := map[string]any{
		"request_id": p.Frame.ID,
		"tool":       p.Frame.ToolName,
		"kind":       p.Frame.Kind,
		"detail":     p.Frame.Detail,
		"asked_at":   p.Frame.At.Format(time.RFC3339),
		// The instruction, not just the facts. Somebody woken by this at
		// 3am should not have to go find the API reference to answer it.
		"respond": fmt.Sprintf(
			"POST /sessions/%s/perms/respond {\"id\":%q,\"decision\":\"allow-once\"} (or \"deny\")",
			route, p.Frame.ID),
	}
	if session != "" {
		details["session"] = session
	}
	if p.Frame.Source != "" {
		// Which agent asked. A subagent's request and the parent's read
		// identically otherwise, and they are not equally surprising.
		details["source"] = p.Frame.Source
	}

	var summary string
	if p.Deadline.IsZero() {
		details["expires"] = "never — the agent is blocked until somebody answers"
		summary = fmt.Sprintf("core-agent: %s is waiting for approval and nobody is attached", p.Frame.ToolName)
	} else {
		details["expires_at"] = p.Deadline.UTC().Format(time.RFC3339)
		details["expires_in"] = time.Until(p.Deadline).Round(time.Second).String()
		summary = fmt.Sprintf("core-agent: %s needs approval within %s and nobody is attached",
			p.Frame.ToolName, time.Until(p.Deadline).Round(time.Second))
	}

	// warning, not critical. An unattended gated run asking for one
	// approval is the system working as designed; paging for it would
	// train the recipient to route the channel to a folder, and the one
	// that mattered would go there with the rest.
	if err := n.snd.Send(ctx, "warning", summary, details, session); err != nil {
		// Logged rather than returned: the broker deliberately does not
		// wait on this, and a prompt must stay answerable while its
		// notification is failing. But it must not fail SILENTLY — "we
		// tried to tell you and could not" is the single most useful
		// line in the log of a run that stalled.
		n.log.Error("approval notification failed; the operator was not told this prompt is waiting",
			"err", err, "target", n.snd.Target(), "session", session, "request_id", p.Frame.ID)
		return
	}
	n.log.Info("approval notification sent",
		"target", n.snd.Target(), "session", session, "request_id", p.Frame.ID, "tool", p.Frame.ToolName)
}
