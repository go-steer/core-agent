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
	"fmt"

	"github.com/go-steer/core-agent/v2/pkg/attach"
	"github.com/go-steer/core-agent/v2/pkg/permissions"
)

var _ attach.PermModeController = (*Adapter)(nil)

// settableModes is attach.RemotePermModes as this session can take
// them: "auto" only when the gate can enter it. AttachSetPermMode
// refuses exactly what this leaves out, so the chip a client builds
// from it never offers a mode the switch would refuse.
func settableModes(g *permissions.Gate) []string {
	out := make([]string, 0, len(attach.RemotePermModes))
	for _, m := range attach.RemotePermModes {
		if m == permissions.ModeAuto && g.AutoSelectable() != nil {
			continue
		}
		out = append(out, string(m))
	}
	return out
}

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
	// SwapMode drops a mode the gate refuses without a word, so check
	// first: otherwise the answer would be 200 {"mode":"auto"} and an
	// audit row recording a change that never happened, with the
	// session still in its old mode (#1175 decision 12).
	if to == permissions.ModeAuto {
		if err := g.AutoSelectable(); err != nil {
			return attach.PermModeResponse{}, fmt.Errorf("perms/mode: %w", err)
		}
	}
	from := g.SwapMode(to)
	a.RecordPermModeChange(from, to, req.Caller)
	return attach.PermModeResponse{Previous: string(from), Mode: string(to)}, nil
}
