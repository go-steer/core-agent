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

package alert

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-steer/core-agent/v2/pkg/config"
)

// Sender delivers to ONE pre-registered alert target on behalf of the
// runtime itself rather than on behalf of the model.
//
// Three differences from the `alert` tool, and each of them is the
// reason this type exists instead of a second caller of the tool:
//
//   - It is not gated. The permission gate decides what the MODEL may
//     do; a notification the gate emits about its own unanswered prompt
//     is not a model action, and routing it through the gate would be
//     circular — the gate asking permission to tell you it needs
//     permission, with nobody attached to answer either one.
//
//   - It carries its own rate-limit budget. The tool's limiter is keyed
//     per target and shared across every call the model makes; if the
//     runtime drew from the same bucket, a chatty agent could exhaust it
//     and silence the channel that governs that same agent. A dropped
//     model alert is a dropped alert. A dropped approval notification is
//     an operator who never learns their gate is waiting.
//
//   - Its target is fixed at construction. The model picks a target by
//     name; the runtime was told which one to use by the operator, and
//     resolving a name per call would be a lookup that can start failing
//     long after startup said it would work.
//
// Construction fails when the named target is unknown or undeliverable,
// which is deliberate: a caller that wants out-of-band notification and
// cannot have it must find out at wiring time, not at the first prompt
// nobody answers.
type Sender struct {
	target  config.AlertTarget
	client  *http.Client
	getenv  func(string) string
	limiter *rateLimiter
}

// NewSender builds a Sender for the target named in cfg's registry.
func NewSender(cfg *config.Config, target string) (*Sender, error) {
	return newSender(cfg, target, os.Getenv, time.Now, nil)
}

// newSender is NewSender with env, clock and HTTP client injectable for
// tests, mirroring newTool.
func newSender(cfg *config.Config, target string, getenv func(string) string, now func() time.Time, client *http.Client) (*Sender, error) {
	if cfg == nil {
		return nil, fmt.Errorf("alert: sender: cfg is required")
	}
	if target == "" {
		return nil, fmt.Errorf("alert: sender: target is required")
	}
	if getenv == nil {
		getenv = os.Getenv
	}

	// Look the name up in the FULL registry first, so "you configured a
	// target whose webhook env is unset" and "you named a target that
	// does not exist" stay two different sentences. Partitioning first
	// would collapse them into "unknown target", and an operator who
	// spelled the name right would go looking for a typo.
	var found *config.AlertTarget
	names := make([]string, 0, len(cfg.Alerts.Targets))
	for i := range cfg.Alerts.Targets {
		names = append(names, cfg.Alerts.Targets[i].Name)
		if cfg.Alerts.Targets[i].Name == target {
			found = &cfg.Alerts.Targets[i]
		}
	}
	if found == nil {
		return nil, fmt.Errorf("alert: sender: unknown target %q (configured: %s)", target, strings.Join(names, ", "))
	}
	if reason := undeliverable(*found, getenv); reason != "" {
		return nil, fmt.Errorf("alert: sender: target %q cannot be delivered to: %s", target, reason)
	}

	limiter, err := newRateLimiter(cfg.Alerts.RateLimitPerTarget, now)
	if err != nil {
		return nil, err
	}
	if client == nil {
		client = &http.Client{Timeout: httpTimeout}
	}
	return &Sender{target: *found, client: client, getenv: getenv, limiter: limiter}, nil
}

// Target reports the name this Sender delivers to, for startup logs that
// want to say where notifications will go.
func (s *Sender) Target() string { return s.target.Name }

// Send delivers one notification. level must be one of the values the
// tool accepts; session is interpolated by templates that reference it
// and may be empty.
//
// A rate-limited send returns ErrRateLimited rather than nil. Reporting
// it as a success would be the same lie the whole out-of-band path
// exists to prevent: the caller believes somebody was told.
func (s *Sender) Send(ctx context.Context, level, summary string, details map[string]any, session string) error {
	if _, ok := validLevels[level]; !ok {
		return fmt.Errorf("alert: sender: level %q is invalid (want one of: info, warning, critical, resolved)", level)
	}
	if summary == "" {
		return fmt.Errorf("alert: sender: summary is required")
	}
	if !s.limiter.allow(s.target.Name) {
		return fmt.Errorf("%w on target %q", ErrRateLimited, s.target.Name)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	_, err := post(ctx, s.client, s.getenv, s.target, Args{
		Target:  s.target.Name,
		Level:   level,
		Summary: summary,
		Details: details,
	}, session)
	return err
}

// ErrRateLimited is returned by Send when the target's budget for this
// window is spent. A sentinel so a caller can log it differently from a
// delivery failure — the destination is fine, we chose not to call it.
var ErrRateLimited = fmt.Errorf("alert: rate-limited")
