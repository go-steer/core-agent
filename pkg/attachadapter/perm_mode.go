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
	"github.com/go-steer/core-agent/v2/pkg/attach"
)

var _ attach.PermModeController = (*Adapter)(nil)

// AttachSetPermMode implements attach.PermModeController (#1168). It is
// the one place a running session's mode changes: the HTTP endpoint and
// the in-process TUI's Shift+Tab both land here, so both leave the same
// audit trail.
//
// The gate is the session's own — DeriveForSession copies the mode into
// each session's gate — so on a multi-session daemon this changes one
// session, not its neighbours.
func (ad *Adapter) AttachSetPermMode(req attach.PermModeRequest) (attach.PermModeResponse, error) {
	to, err := attach.ParseRemotePermMode(req.Mode)
	if err != nil {
		return attach.PermModeResponse{}, err
	}
	a := ad.Agent()
	if a == nil || a.Gate() == nil {
		return attach.PermModeResponse{}, attach.ErrCapabilityNotRegistered
	}
	g := a.Gate()
	from := g.SwapMode(to)
	a.RecordPermModeChange(from, to, req.Caller)
	return attach.PermModeResponse{Previous: string(from), Mode: string(to)}, nil
}
